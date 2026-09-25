package query

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// findCollector returns the single CollectorSummary named name, failing the
// test if it is missing. collector_id is Collectors' own grouping key, so
// unlike a router's sysname (see routerNamed in routers_test.go) there is
// exactly one summary per name by construction -- this hides the map lookup
// rather than guarding against a duplicate that cannot occur.
func findCollector(t *testing.T, got []CollectorSummary, name string) CollectorSummary {
	t.Helper()
	for _, c := range got {
		if c.Collector == name {
			return c
		}
	}
	t.Fatalf("Collectors: no summary for collector %q (got %d collectors)", name, len(got))
	return CollectorSummary{}
}

// findCollectorRouter returns the one CollectorRouter named sysname inside
// cs, failing the test if it is missing or duplicated. It exists for the
// same reason routers_test.go's own routerNamed does: testDB is shared by
// every test in this package and never truncated, so a collector's own
// Routers slice can carry routers other tests wrote under the same
// collector_id, and indexing by position would make this test's assertion
// depend on what else happened to run first.
func findCollectorRouter(t *testing.T, cs CollectorSummary, sysname string) CollectorRouter {
	t.Helper()
	var matches []CollectorRouter
	for _, r := range cs.Routers {
		if r.SysName == sysname {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("collector %q: got %d routers named %q, want 1: %+v",
			cs.Collector, len(matches), sysname, cs.Routers)
	}
	return matches[0]
}

// insertEnvelopeOnlyRow writes one row into table using only the envelope
// columns every one of the ten data tables shares -- collection_test.go's
// own flagFixtureColumns, reused rather than re-derived, and the same
// mechanism seedFlagCountsFixtures already proves works against all ten
// (see insertParseFlagRow). It differs from that helper only in taking
// collector as a parameter instead of hard-coding "query-test": these
// LastRowAt tests need a collector_id no other fixture in the
// package writes under, so their assertions are not answerable by rows any
// other test happened to leave behind.
func insertEnvelopeOnlyRow(t *testing.T, ctx context.Context, q *Q, table, collector, router, peer string,
	sessionID, seq, streamSeq uint64, ts time.Time,
) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+"."+table+" ("+flagFixtureColumns+")")
	if err != nil {
		t.Fatalf("prepare %s envelope-only insert: %v", table, err)
	}
	if err := b.Append(
		collector, router, "collectors-fixture-router", peer, "in_pre",
		uint32(65000), peer, sessionID, seq, ts, ts,
		[]string{}, streamSeq,
	); err != nil {
		t.Fatalf("append %s envelope-only row: %v", table, err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send %s envelope-only batch: %v", table, err)
	}
}

// TestCollectorsGroupsRoutersUnderTheirOwnCollector reuses
// insertTwoCollectorFixture -- the fixture routersSQL needed to prove
// session identity is (collector, router), never router alone -- at the
// grain above Routers. tcRouter is monitored by both dev-c1 and dev-c2, so
// it must appear under BOTH collectors' own summaries: that is two
// collectors' views of the network, not a duplicate to merge or a winner
// to pick. See Router.Collector's own doc comment for why, and
// TestRoutersDoesNotDropASecondCollectorsView for the identical claim one
// grain down.
func TestCollectorsGroupsRoutersUnderTheirOwnCollector(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertTwoCollectorFixture(t, ctx, q)

	got, err := q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}

	c1 := findCollectorRouter(t, findCollector(t, got, "dev-c1"), tcSysname)
	c2 := findCollectorRouter(t, findCollector(t, got, "dev-c2"), tcSysname)

	if c1.IP.String() != tcRouter || c2.IP.String() != tcRouter {
		t.Errorf("dev-c1 and dev-c2 must both report %s, got %s and %s",
			tcRouter, c1.IP, c2.IP)
	}
	if c1.PeersUp != 1 || c2.PeersUp != 1 {
		t.Errorf("PeersUp = dev-c1:%d dev-c2:%d, want 1 each -- dev-c2's "+
			"superseded session (whose id collides with dev-c1's current one) "+
			"must not contribute a peer to dev-c2's own view of the router",
			c1.PeersUp, c2.PeersUp)
	}
}

// TestCollectorsCountsPeersOncePerPeerNotOncePerRIBView reuses
// insertTwoRibPeerFixture -- the fixture routersSQL needed when the same
// defect (counting peer_state rows instead of peer_up rows) was found
// there. A collector total built by summing peer_state rows double-counts
// every peer on a router mirroring pre- and post-policy adj-RIB-in; see
// routersSQL's own doc comment for the fuller account.
func TestCollectorsCountsPeersOncePerPeerNotOncePerRIBView(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertTwoRibPeerFixture(t, ctx, q)

	got, err := q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}
	r := findCollectorRouter(t, findCollector(t, got, defaultFixtureCollector), twoRibSysname)

	if r.PeersUp != 1 {
		t.Errorf("PeersUp = %d, want 1 -- %s is ONE peer the router mirrors "+
			"under two ribs, not two peers", r.PeersUp, twoRibUpPeer)
	}
	if r.PeersDown != 1 {
		t.Errorf("PeersDown = %d, want 1 -- %s ended its session down (its "+
			"newest event across every rib is the in_post down)",
			r.PeersDown, twoRibSplitPeer)
	}
	if sum := r.PeersUp + r.PeersDown; sum != 2 {
		t.Errorf("PeersUp + PeersDown = %d for a router with 2 peers -- a peer "+
			"counted once per rib view inflates the total", sum)
	}
}

// Addresses for this file's own fixtures, on 10.98.0.0/16 -- confirmed
// unused elsewhere in query/*_test.go. 10.90-10.96, 10.97 and 10.99 are all
// spoken for (10.97.1.1/.2 most recently, by peers_test.go's own
// sessionFactsRouter/sessionFactsPeer); a colliding fixture here would win
// peerStateCTE's cur with a higher session_id and fail an unrelated test
// that passes in isolation -- see peers_test.go:562 for the fuller account
// of that exact failure mode.
const (
	sysDescrAbsentRouterIP = "10.98.2.1"
	sysDescrAbsentSysname  = "collectors-sysdescr-absent-router"
	sysDescrAbsentPeerIP   = "10.98.2.2"
	sysDescrAbsentSession  = 6001
)

// TestCollectorsReportsSysDescrAbsentWhenNeverObserved: sys_descr is a
// column every row does not necessarily carry, so every row written before
// it existed -- and every peer whose Initiation this collector simply never
// saw -- is empty. Empty must survive to the caller as empty and must NOT be
// backfilled, guessed, or rendered as a styled blank; the card says "not
// observed", and that rendering is a caller's job, not this query's.
func TestCollectorsReportsSysDescrAbsentWhenNeverObserved(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: sysDescrAbsentRouterIP, RouterSysname: sysDescrAbsentSysname,
		PeerIP: sysDescrAbsentPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: sysDescrAbsentPeerIP,
		SessionID: sysDescrAbsentSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
		// SysDescr left at its Go zero value: this peer's Initiation, if
		// any, never reached this fixture.
	})

	got, err := q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}
	r := findCollectorRouter(t, findCollector(t, got, defaultFixtureCollector), sysDescrAbsentSysname)
	if r.SysDescr != "" {
		t.Errorf("SysDescr = %q, want \"\" -- this router's Initiation was "+
			"never observed, and an empty string must survive to the caller "+
			"rather than a backfilled or placeholder value", r.SysDescr)
	}
}

const (
	sysDescrSurvivesRouterIP = "10.98.3.1"
	sysDescrSurvivesSysname  = "collectors-sysdescr-survives-router"
	// sysDescrSurvivesPeerA carries the router's real sys_descr, observed
	// EARLIER. sysDescrSurvivesPeerB's own only event carries none, and is
	// observed LATER -- so a router-level aggregate that orders by recency
	// without first filtering to non-empty values picks peer B's blank over
	// peer A's real one.
	sysDescrSurvivesPeerA   = "10.98.3.2"
	sysDescrSurvivesPeerB   = "10.98.3.3"
	sysDescrSurvivesSession = 6002
	sysDescrSurvivesText    = "IOS-XR 7.11"
)

// TestCollectorsSysDescrSurvivesALaterEventCarryingNone covers what
// TestCollectorsReportsSysDescrAbsentWhenNeverObserved does not: that test
// only covers the never-observed case, which passes under plain
// argMax(sys_descr, last_seen) exactly as well as under argMaxIf -- a
// vacuous pass on the
// requirement that actually matters, because sys_descr does not ride on
// every peer_events row (over the live archive's last two days, kind=up
// has 6 rows with 4 carrying a descr, kind=down 1 with 0).
//
// peer_state's own per-(peer, rib) sys_descr is already argMaxIf-derived
// (see peerStateCTE), so a single-peer fixture cannot tell collectorsSQL's
// OWN argMax-versus-argMaxIf choice apart: with one input row the two
// aggregate to the same answer regardless. This fixture puts two
// peer_state rows under ONE router -- peer A's, carrying the router's real
// descr at the earlier timestamp, and peer B's, carrying none at the later
// one -- so the router-level rollup has an actual choice to get wrong.
// Swap collectorsSQL's argMaxIf back to a plain argMax and this test fails
// (SysDescr reads "" instead of "IOS-XR 7.11").
func TestCollectorsSysDescrSurvivesALaterEventCarryingNone(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: sysDescrSurvivesRouterIP, RouterSysname: sysDescrSurvivesSysname,
		PeerIP: sysDescrSurvivesPeerA, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: sysDescrSurvivesPeerA,
		SessionID: sysDescrSurvivesSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
		SysDescr: sysDescrSurvivesText,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: sysDescrSurvivesRouterIP, RouterSysname: sysDescrSurvivesSysname,
		PeerIP: sysDescrSurvivesPeerB, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: sysDescrSurvivesPeerB,
		SessionID: sysDescrSurvivesSession, Seq: 1, StreamSeq: 2,
		Kind:        "up",
		TsRouter:    now.Add(5 * time.Minute),
		TsCollector: now.Add(5 * time.Minute),
		// SysDescr left empty: peer B's own Initiation, if any, carried
		// nothing this collector saw, and peer B's row is the LATER one.
	})

	got, err := q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}
	r := findCollectorRouter(t, findCollector(t, got, defaultFixtureCollector), sysDescrSurvivesSysname)
	if r.SysDescr != sysDescrSurvivesText {
		t.Errorf("SysDescr = %q, want %q -- peer B's later event carries no "+
			"descr at all, and that must not erase peer A's earlier, real one",
			r.SysDescr, sysDescrSurvivesText)
	}
}

const (
	lastRowCollector = "collectors-lastrow-test"
	lastRowRouterIP  = "10.98.4.1"
	lastRowSysname   = "collectors-lastrow-router"
	lastRowPeerIP    = "10.98.4.2"
	lastRowSession   = 6003
)

// TestCollectorsLastRowAtIsTheNewestAcrossEveryTableThisCollectorWrites:
// peer_events alone is the wrong answer. A collector can be reading route
// updates and stats reports briskly while its newest peer event is hours
// old, and a liveness number built on peer_events would call that
// collector dead. This fixture uses a collector_id no other test in this
// package writes under, so the "before" assertion -- LastRowAt resolves to
// exactly the one peer_events row's timestamp -- is not answerable by
// anything left behind by another test.
func TestCollectorsLastRowAtIsTheNewestAcrossEveryTableThisCollectorWrites(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	anchor := time.Now().UTC().Truncate(time.Microsecond)
	tOld := anchor.Add(-2 * time.Hour)

	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: lastRowCollector,
		RouterIP:  lastRowRouterIP, RouterSysname: lastRowSysname,
		PeerIP: lastRowPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: lastRowPeerIP,
		SessionID: lastRowSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: tOld, TsCollector: tOld,
	})

	got, err := q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}
	before := findCollector(t, got, lastRowCollector).LastRowAt
	if !before.Equal(tOld) {
		t.Fatalf("LastRowAt = %v before any other table has a row, want %v -- "+
			"this fixture's premise (peer_events alone reports the old "+
			"timestamp) does not hold, so the rest of this test proves nothing",
			before, tOld)
	}

	// A row in ls_nodes -- one of three tables (ls_links, ls_nodes and
	// ls_prefixes all carry both collector_id and ts_collector, confirmed
	// against system.columns) --
	// with a ts_collector two hours newer than the peer_events row above.
	// LastRowAt must move to it.
	insertEnvelopeOnlyRow(t, ctx, q, "ls_nodes", lastRowCollector,
		lastRowRouterIP, lastRowPeerIP, lastRowSession, 2, 2, anchor)

	got, err = q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}
	after := findCollector(t, got, lastRowCollector).LastRowAt
	if !after.Equal(anchor) {
		t.Errorf("LastRowAt = %v, want %v (the ls_nodes row's ts_collector) -- "+
			"a query scoped to peer_events alone would still report %v, "+
			"calling a collector that is actively writing BGP-LS objects dead",
			after, anchor, tOld)
	}
}

// TestCollectorsLastRowScansEveryDataTable is the SQL-text regression
// guard for the same fact TestCollectorsLastRowAtIsTheNewestAcrossEveryTableThisCollectorWrites
// proves behaviorally for one table. last_row unions seventeen tables: the
// ten history tables that carry both collector_id and ts_collector
// (confirmed against system.columns, and matching collection.go's own
// flagAllTables, built for the identical reason -- a table silently missing
// from either list is a table whose freshest write goes unseen), and the
// seven current-state tables that carry ts_collector, which keep a
// collector listed after every one of its history rows has expired (see
// TestStateOlderThanRetentionIsStillCurrent). Checking the SQL text is this
// package's own established defense for an invariant a result-based test
// could pass while quietly narrowing -- see
// TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree. The behavioral tests
// cannot tell a history arm from its current twin: every row inserted into
// a history table also lands in its current table, so deleting one of the
// pair leaves them green.
//
// The needle is the last_row ARM's own text, not a bare "FROM %[1]s.<table>".
// collectorsSQL embeds peerStateCTE and peerUpCTE, which read FROM
// %[1]s.peer_current for their own unrelated reasons, and peerStateCTE read
// FROM %[1]s.peer_events the same way when it was the peer table: a bare
// needle for either was satisfied by those CTEs alone, and deleting the
// peer_events arm from last_row once left this guard GREEN. The
// peer_current arm is load-bearing twice over -- router_state is built
// from peer_current, so that arm is what guarantees every collector
// router_state can produce has a last_row row, which is the premise of the
// outer INNER JOIN (see collectorsSQL's own doc comment). Mutation-checked
// by deleting an arm and confirming this fails.
func TestCollectorsLastRowScansEveryDataTable(t *testing.T) {
	// In the statement's OWN arm order: only the first arm is written
	// without a UNION ALL prefix, so reordering the arms in collectorsSQL
	// means reordering this list with them.
	want := []string{
		"peer_events", "route_unicast", "route_vpn", "route_evpn",
		"eor_events", "ls_events", "ls_nodes", "ls_links", "ls_prefixes",
		"stats_events",
		"peer_current", "route_unicast_current", "route_vpn_current",
		"route_evpn_current", "ls_nodes_current", "ls_links_current",
		"ls_prefixes_current",
	}
	for i, table := range want {
		needle := "SELECT collector_id, ts_collector FROM %[1]s." + table + "\n"
		if i > 0 {
			needle = "UNION ALL " + needle
		}
		if !strings.Contains(collectorsSQL, needle) {
			t.Errorf("collectorsSQL's last_row union no longer reads %s -- a "+
				"collector whose newest row lives only in this table would "+
				"be reported dead, or dropped (looked for %q)", table, needle)
		}
	}
}

const (
	sumCollector      = "collectors-sum-test"
	sumRouterAIP      = "10.98.5.1"
	sumRouterASys     = "collectors-sum-router-a"
	sumRouterAPeer1   = "10.98.5.2"
	sumRouterAPeer2   = "10.98.5.3"
	sumRouterASession = 6004

	sumRouterBIP      = "10.98.6.1"
	sumRouterBSys     = "collectors-sum-router-b"
	sumRouterBPeer1   = "10.98.6.2"
	sumRouterBPeer2   = "10.98.6.3"
	sumRouterBSession = 6005
)

// TestCollectorsSumsPeerCountsAcrossAllItsRouters guards the roll-up done
// in Go rather than in SQL (see Collectors' own doc comment for
// why): a collector's PeersUp/PeersDown/PeersViewLost are the SUM across
// every router it monitors, not one router's own count repeated or the
// first router's count alone. sumCollector is a dedicated collector_id so
// this sum is exact rather than diluted by whatever else the shared
// "query-test" collector has accumulated from other tests in this package.
func TestCollectorsSumsPeerCountsAcrossAllItsRouters(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC()

	for _, p := range []string{sumRouterAPeer1, sumRouterAPeer2} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			Collector: sumCollector,
			RouterIP:  sumRouterAIP, RouterSysname: sumRouterASys,
			PeerIP: p, RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: p,
			SessionID: sumRouterASession, Seq: 1, StreamSeq: 1,
			Kind: "up", TsRouter: now, TsCollector: now,
		})
	}
	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: sumCollector,
		RouterIP:  sumRouterBIP, RouterSysname: sumRouterBSys,
		PeerIP: sumRouterBPeer1, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: sumRouterBPeer1,
		SessionID: sumRouterBSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: sumCollector,
		RouterIP:  sumRouterBIP, RouterSysname: sumRouterBSys,
		PeerIP: sumRouterBPeer2, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: sumRouterBPeer2,
		SessionID: sumRouterBSession, Seq: 1, StreamSeq: 1,
		Kind: "down", TsRouter: now, TsCollector: now,
	})

	got, err := q.Collectors(ctx)
	if err != nil {
		t.Fatalf("Collectors: %v", err)
	}
	cs := findCollector(t, got, sumCollector)
	if len(cs.Routers) != 2 {
		t.Fatalf("got %d routers, want 2: %+v", len(cs.Routers), cs.Routers)
	}
	if cs.PeersUp != 3 || cs.PeersDown != 1 || cs.PeersViewLost != 0 {
		t.Errorf("PeersUp=%d PeersDown=%d PeersViewLost=%d, want 3/1/0 -- the "+
			"collector total must be the SUM across both of its routers, not "+
			"either router's own count alone", cs.PeersUp, cs.PeersDown, cs.PeersViewLost)
	}
}

// Addresses for CollectorActivity's own fixtures, still on 10.98.0.0/16 --
// 10.98.7.x and 10.98.8.x, the next free pair after this file's own
// 10.98.2.x through 10.98.6.x (see the block comment above
// sysDescrAbsentRouterIP). Confirmed unused elsewhere in query/*_test.go.
const (
	activityFillRouterIP = "10.98.7.1"
	activityFillSysname  = "collectors-activity-fill-router"
	activityFillPeerA    = "10.98.7.2"
	activityFillPeerB    = "10.98.7.3"
	activityFillSession  = 6006

	activityRowsRouterIP = "10.98.8.1"
	activityRowsSysname  = "collectors-activity-rows-router"
	activityRowsPeerIP   = "10.98.8.2"
	activityRowsSession  = 6007
)

// findActivity returns the one CollectorActivity in series whose Minute
// equals want, failing the test if it is missing. It exists because this
// file's own fixtures cannot be pinned to a fixed historical anchor the
// way every other fixture in this package is -- see
// insertActivityFillFixture's own doc comment -- so a test cannot assume
// the bucket it cares about sits at a fixed slice index; it has to find
// the bucket by the instant it actually asked for.
func findActivity(t *testing.T, series []CollectorActivity, want time.Time) CollectorActivity {
	t.Helper()
	for _, a := range series {
		if a.Minute.Equal(want) {
			return a
		}
	}
	t.Fatalf("no bucket for minute %v in series %+v", want, series)
	return CollectorActivity{}
}

// insertActivityFillFixture writes two collectors' rows for
// TestCollectorActivityFillsEveryMinutePerCollector: c1 gets two rows seven
// minutes before curMinute (proving a minute can sum more than one row),
// nothing four minutes before it (the hole this test's name requires
// filled), and one row three minutes before it; c2 gets exactly ONE row, at
// the same seven-minutes-before instant as c1's first pair, and NOTHING
// else anywhere in the window. c2's single data point is deliberate: WITH
// FILL has to extend c2's own fill across the WHOLE window even though
// c2's real data sits at one edge of it, not merely between two rows c2
// actually wrote -- verified directly against this project's own running
// ClickHouse 24.8 before collectorActivitySQL was written (see that
// statement's own doc comment).
//
// Both collector_id values carry now's own UnixNano as a suffix, unlike
// every other fixture in this file. Every earlier query took its
// window as an explicit Since the caller supplied (ChurnFilter.Since,
// CollectionFilter.Since, HistoryFilter.Since); CollectorActivity is the
// first to anchor its window to time.Now() internally, with no override,
// because that is what CollectorActivity's own interface requires. testDB is never
// truncated (requireQuery's own doc comment), so a fixture written under a
// FIXED collector_id and placed minutes-relative-to-now would collide with
// its own previous run's rows on a second `go test` inside the same
// window -- an ordinary occurrence in an edit-test loop, not a hypothetical
// one. The suffix makes each run's fixture invisible to every other run's,
// the same guarantee a fixed historical anchor gives every other fixture
// in this package for free.
func insertActivityFillFixture(t *testing.T, ctx context.Context, q *Q, now time.Time) (c1, c2 string) {
	t.Helper()
	suffix := now.UnixNano()
	c1 = fmt.Sprintf("collectors-activity-fill-c1-%d", suffix)
	c2 = fmt.Sprintf("collectors-activity-fill-c2-%d", suffix)
	curMinute := now.Truncate(time.Minute)

	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: c1, RouterIP: activityFillRouterIP, RouterSysname: activityFillSysname,
		PeerIP: activityFillPeerA, RIB: "in_pre", PeerASN: 65000, PeerBGPID: activityFillPeerA,
		SessionID: activityFillSession, Seq: 1, StreamSeq: 1, Kind: "up",
		TsRouter:    curMinute.Add(-7*time.Minute + 5*time.Second),
		TsCollector: curMinute.Add(-7*time.Minute + 5*time.Second),
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: c1, RouterIP: activityFillRouterIP, RouterSysname: activityFillSysname,
		PeerIP: activityFillPeerB, RIB: "in_pre", PeerASN: 65000, PeerBGPID: activityFillPeerB,
		SessionID: activityFillSession, Seq: 2, StreamSeq: 2, Kind: "up",
		TsRouter:    curMinute.Add(-7*time.Minute + 10*time.Second),
		TsCollector: curMinute.Add(-7*time.Minute + 10*time.Second),
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: c1, RouterIP: activityFillRouterIP, RouterSysname: activityFillSysname,
		PeerIP: activityFillPeerA, RIB: "in_pre", PeerASN: 65000, PeerBGPID: activityFillPeerA,
		SessionID: activityFillSession, Seq: 3, StreamSeq: 3, Kind: "down",
		TsRouter:    curMinute.Add(-3*time.Minute + 5*time.Second),
		TsCollector: curMinute.Add(-3*time.Minute + 5*time.Second),
	})

	// c2's row has a StreamSeq of its own. peer_events' sort key is
	// (router_ip, peer_ip, rib, ts_router, stream_seq), with no
	// collector_id, so at c1's StreamSeq this row would be one key with
	// c1's peer-A up, and a background merge would keep only one of the
	// two. Production never writes that: stream_seq is one JetStream
	// sequence shared by every collector.
	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: c2, RouterIP: activityFillRouterIP, RouterSysname: activityFillSysname,
		PeerIP: activityFillPeerA, RIB: "in_pre", PeerASN: 65000, PeerBGPID: activityFillPeerA,
		SessionID: activityFillSession, Seq: 1, StreamSeq: 101, Kind: "up",
		TsRouter:    curMinute.Add(-7*time.Minute + 5*time.Second),
		TsCollector: curMinute.Add(-7*time.Minute + 5*time.Second),
	})
	return c1, c2
}

// TestCollectorActivityFillsEveryMinutePerCollector pins that: WITH FILL
// must fill PER COLLECTOR, not
// across the whole result, and a fixture carrying only one collector
// cannot tell the correct grouping apart from the broken one -- a single
// collector's own rows pass under both an ORDER BY that carries
// collector_id and one that drops it, because with only one group there is
// no boundary for a dropped group column to bleed across. This fixture
// carries two.
//
// A ten-minute window ending at curMinute puts curMinute-9 through
// curMinute in the series. insertActivityFillFixture's data sits safely
// inside that (seven and three minutes back), leaving margin on both edges
// against a minute rolling over between this test's own "now" and
// CollectorActivity's internal one.
//
// # Mutation evidence
//
// Applied with `sed` directly to query/collectors.go (`ORDER BY
// collector_id ASC, minute ASC` -> `ORDER BY minute ASC`), rebuilt, reran
// this test:
//
//	=== RUN   TestCollectorActivityFillsEveryMinutePerCollector
//	    collectors_test.go:578: collectors-activity-fill-c1-1789742458391564554
//	    bucket count = 2, want 10 -- dropping collector_id from the ORDER BY
//	    merges both collectors into one interleaved timeline before WITH
//	    FILL runs, so a group's own run of filled minutes no longer spans
//	    the whole window
//	    collectors_test.go:584: collectors-activity-fill-c2-1789742458391564554
//	    bucket count = 1, want 10 -- same defect, the other collector's
//	    side of it
//	    collectors_test.go:589: no bucket for minute 2026-09-18
//	    14:36:00 +0000 UTC in series [{Minute:2026-09-18 14:33:00 +0000 UTC
//	    Rows:2} {Minute:2026-09-18 14:37:00 +0000 UTC Rows:1}]
//	--- FAIL: TestCollectorActivityFillsEveryMinutePerCollector (0.07s)
//
// Reverted (diff against the pre-mutation copy showed byte-identical
// restore) and reconfirmed green.
func TestCollectorActivityFillsEveryMinutePerCollector(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC()
	curMinute := now.Truncate(time.Minute)
	c1, c2 := insertActivityFillFixture(t, ctx, q, now)

	got, err := q.CollectorActivity(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("CollectorActivity: %v", err)
	}

	c1Series, ok := got[c1]
	if !ok {
		t.Fatalf("no series for %s at all", c1)
	}
	c2Series, ok := got[c2]
	if !ok {
		t.Fatalf("no series for %s at all", c2)
	}
	if len(c1Series) != 10 {
		t.Errorf("%s bucket count = %d, want 10 -- dropping collector_id from "+
			"the ORDER BY merges both collectors into one interleaved timeline "+
			"before WITH FILL runs, so a group's own run of filled minutes no "+
			"longer spans the whole window", c1, len(c1Series))
	}
	if len(c2Series) != 10 {
		t.Errorf("%s bucket count = %d, want 10 -- same defect, the other "+
			"collector's side of it", c2, len(c2Series))
	}

	holeMinute := curMinute.Add(-4 * time.Minute)
	if hole := findActivity(t, c1Series, holeMinute); hole.Rows != 0 {
		t.Errorf("%s at %v: Rows = %d, want 0 -- this minute has no fixture "+
			"row at all; a plain GROUP BY would have produced no bucket here, "+
			"which WITH FILL exists to prevent", c1, holeMinute, hole.Rows)
	}

	twoRowMinute := curMinute.Add(-7 * time.Minute)
	if two := findActivity(t, c1Series, twoRowMinute); two.Rows != 2 {
		t.Errorf("%s at %v: Rows = %d, want 2", c1, twoRowMinute, two.Rows)
	}
	oneRowMinute := curMinute.Add(-3 * time.Minute)
	if one := findActivity(t, c1Series, oneRowMinute); one.Rows != 1 {
		t.Errorf("%s at %v: Rows = %d, want 1", c1, oneRowMinute, one.Rows)
	}

	// c2 wrote exactly one row, at twoRowMinute, and nothing else anywhere
	// in the window -- every other one of its ten buckets must still be a
	// real, present zero, INCLUDING the ones outside the narrow range
	// between c2's own (nonexistent) other rows: WITH FILL has to reach
	// the full FROM..TO span asked for, not merely the span c2 itself
	// happened to write into.
	var c2Sum uint64
	for _, a := range c2Series {
		c2Sum += a.Rows
	}
	if c2Sum != 1 {
		t.Errorf("%s: sum of all ten buckets = %d, want 1 -- exactly the one "+
			"row this collector actually wrote", c2, c2Sum)
	}
	if got := findActivity(t, c2Series, twoRowMinute).Rows; got != 1 {
		t.Errorf("%s at %v: Rows = %d, want 1", c2, twoRowMinute, got)
	}
	if got := findActivity(t, c2Series, curMinute).Rows; got != 0 {
		t.Errorf("%s at %v (the window's OTHER edge from its one real row): "+
			"Rows = %d, want 0 -- WITH FILL must reach all the way to the "+
			"window's edge for a group with a single row, not merely between "+
			"rows the group does not have", c2, curMinute, got)
	}
}

// TestCollectorActivityCountsArchivedRowsAndSaysSo pins a named
// requirement: this series is rows archived, never messages or a rate.
// Five route_unicast rows sharing ONE stream_seq -- one BMP UPDATE that
// carried five NLRIs, the identical shape
// TestFlagCountsCountsEnvelopesNotRows uses in collection_test.go to prove
// the opposite point for FlagCounts (which counts ENVELOPES and must read
// 1 here). CollectorActivity must read 5: an envelope count is exactly the
// wrong answer this test exists to keep off this series.
func TestCollectorActivityCountsArchivedRowsAndSaysSo(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC()
	collector := fmt.Sprintf("collectors-activity-rows-%d", now.UnixNano())
	minute := now.Truncate(time.Minute).Add(-time.Minute)
	ts := minute.Add(5 * time.Second)

	for i, prefix := range []string{
		"10.98.80.0/32", "10.98.80.1/32", "10.98.80.2/32",
		"10.98.80.3/32", "10.98.80.4/32",
	} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			Collector: collector, RouterIP: activityRowsRouterIP, RouterSysname: activityRowsSysname,
			PeerIP: activityRowsPeerIP, RIB: "in_pre", Family: "ipv4u", Prefix: prefix,
			PeerASN: 65000, PeerBGPID: activityRowsPeerIP,
			SessionID: activityRowsSession, Seq: uint64(i + 1), StreamSeq: 1,
			TsRouter: ts, TsCollector: ts,
		})
	}

	got, err := q.CollectorActivity(ctx, 3*time.Minute)
	if err != nil {
		t.Fatalf("CollectorActivity: %v", err)
	}
	series, ok := got[collector]
	if !ok {
		t.Fatalf("no series for %s at all", collector)
	}
	if rows := findActivity(t, series, minute).Rows; rows != 5 {
		t.Errorf("Rows = %d, want 5 -- one BMP UPDATE carrying five NLRIs is "+
			"five rows archived, not the one message or one envelope it "+
			"physically was", rows)
	}
}

// TestCollectorActivityScansAllTenDataTables is
// TestCollectorsLastRowScansEveryDataTable's own shape, for
// collectorActivitySQL: neither of this file's two behavioral
// CollectorActivity tests writes into more than one of the ten tables
// (TestCollectorActivityFillsEveryMinutePerCollector only peer_events,
// TestCollectorActivityCountsArchivedRowsAndSaysSo only route_unicast), so
// nothing behavioral would notice a future edit that silently dropped one
// of the other eight UNION ALL arms -- every test in the package would
// still pass. Checking the SQL text directly is this package's own
// established defense for exactly that gap; see
// TestCollectorsLastRowScansEveryDataTable's own doc comment, which guards
// collectorsSQL against the identical failure: a union with only seven of
// the ten tables.
func TestCollectorActivityScansAllTenDataTables(t *testing.T) {
	want := []string{
		"peer_events", "route_unicast", "route_vpn", "route_evpn",
		"eor_events", "ls_events", "ls_nodes", "ls_links", "ls_prefixes",
		"stats_events",
	}
	for _, table := range want {
		needle := "FROM %[1]s." + table
		if !strings.Contains(collectorActivitySQL, needle) {
			t.Errorf("collectorActivitySQL no longer reads %s -- a collector "+
				"whose activity lives only in this table would silently drop "+
				"out of its own sparkline", table)
		}
	}
}
