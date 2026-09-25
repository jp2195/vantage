package query

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// churnFilterFor is collectionFilterFor's shape for this signal: the fixture
// anchor as the window, so every seeded row is inside it, and a bucket wide
// enough to hold them all unless a test narrows it.
func churnFilterFor(router string, bucket time.Duration) ChurnFilter {
	f := ChurnFilter{Since: collectionFixtureAnchor.Add(-time.Hour), Bucket: bucket}
	if router != "" {
		f.Router = netip.MustParseAddr(router)
	}
	return f
}

func sumChurn(rows []ChurnBucket) (readvertise, withdraw, dump uint64) {
	for _, r := range rows {
		readvertise += r.Readvertise
		withdraw += r.Withdraw
		dump += r.Dump
	}
	return
}

// The port's whole claim: bucketing changes WHEN a row is counted, never
// WHAT it is counted as. The oracle is the dashboard panel this SQL was
// lifted from -- the same one TestDumpCountsAgreesWithTheDashboards uses --
// so a classification that drifts from the shipped panel fails here rather
// than in a chart nobody can check.
func TestChurnBucketsAgreeWithTheDashboardClassification(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	sql := renderDashboardSQL(t,
		dashboardPanelSQL(t, "route-churn", "Re-advertisements, session dumps and withdrawals", "A"),
		q.db, dumpRouterParityUnicast)
	wantArchived, wantDumps, wantChanges := dashboardChurn(t, ctx, q, sql)
	if wantArchived == 0 || wantDumps == 0 || wantChanges == 0 {
		t.Fatalf("the panel returned %d archived / %d dumps / %d changes for %s; "+
			"the comparison below needs both sides of the classification exercised",
			wantArchived, wantDumps, wantChanges, dumpRouterParityUnicast)
	}

	// One bucket wide enough to hold the whole fixture, so this compares the
	// classification and nothing about time.
	rows, err := q.ChurnBuckets(ctx, churnFilterFor(dumpRouterParityUnicast, time.Hour))
	if err != nil {
		t.Fatalf("ChurnBuckets: %v", err)
	}
	readvertise, withdraw, dump := sumChurn(rows)

	if dump != wantDumps {
		t.Errorf("dumps: ChurnBuckets says %d, route-churn.json says %d", dump, wantDumps)
	}
	// The dashboard's "changes" is everything that is not a session dump,
	// which INCLUDES withdrawals; this answer keeps the two apart, so the
	// comparison has to add them back. Getting this backwards is exactly the
	// "counting one thing under a label claiming another" defect -- a
	// Withdraw folded into Readvertise would still sum correctly here.
	if readvertise+withdraw != wantChanges {
		t.Errorf("changes: ChurnBuckets says %d readvertise + %d withdraw = %d, "+
			"route-churn.json says %d", readvertise, withdraw, readvertise+withdraw, wantChanges)
	}
	if readvertise+withdraw+dump != wantArchived {
		t.Errorf("the three series sum to %d, the panel archived %d -- every row must "+
			"land in exactly one series", readvertise+withdraw+dump, wantArchived)
	}
}

// Bucketing has to actually group. The parity fixture writes its rows one
// second apart, so a one-second bucket separates them and an hour-wide
// bucket holds them all -- with the totals identical either way.
func TestChurnBucketsGroupByTheWidthAsked(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	wideFilter := churnFilterFor(dumpRouterParityUnicast, time.Hour)
	wideFilter.Since = collectionFixtureAnchor.Add(-time.Minute)
	wide, err := q.ChurnBuckets(ctx, wideFilter)
	if err != nil {
		t.Fatalf("ChurnBuckets(1h): %v", err)
	}
	// A one-second bucket needs a window a chart could hold: the fixture is
	// anchored at now and spans seconds, so a minute covers it while staying
	// far inside maxChurnBuckets. Asking for an hour of one-second bars is
	// refused by design, which the guard's own test covers.
	narrowFilter := churnFilterFor(dumpRouterParityUnicast, time.Second)
	narrowFilter.Since = collectionFixtureAnchor.Add(-time.Minute)
	narrow, err := q.ChurnBuckets(ctx, narrowFilter)
	if err != nil {
		t.Fatalf("ChurnBuckets(1s): %v", err)
	}
	if len(wide) != 1 {
		t.Errorf("an hour-wide bucket over a fixture spanning seconds returned %d buckets, want 1", len(wide))
	}
	if len(narrow) <= len(wide) {
		t.Errorf("a one-second bucket returned %d buckets and an hour-wide one %d; "+
			"the width is not reaching the query", len(narrow), len(wide))
	}
	wr, ww, wd := sumChurn(wide)
	nr, nw, nd := sumChurn(narrow)
	if wr != nr || ww != nw || wd != nd {
		t.Errorf("the same rows counted differently at two widths:\n  1h: %d/%d/%d\n  1s: %d/%d/%d",
			wr, ww, wd, nr, nw, nd)
	}
}

// Every bucket timestamp must be a multiple of the width, anchored the way
// ClickHouse anchors toStartOfInterval -- a chart draws bars at these
// instants, and a bucket labeled with the timestamp of its first ROW would
// put a 14:59:58 bar where a 14:55 bar belongs.
func TestChurnBucketsAreAlignedToTheirWidth(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	const width = 10 * time.Second
	rows, err := q.ChurnBuckets(ctx, churnFilterFor(dumpRouterParityUnicast, width))
	if err != nil {
		t.Fatalf("ChurnBuckets: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no buckets; the alignment check below would hold vacuously")
	}
	for _, r := range rows {
		if r.TS.UTC().Unix()%int64(width.Seconds()) != 0 {
			t.Errorf("bucket at %s is not aligned to a %s boundary", r.TS.UTC(), width)
		}
	}
}

// The window bounds the answer, and an empty window is an answer rather than
// an error: a chart with no bars says "nothing churned then", which is a
// fact, and this is the one place it could instead have said "we failed".
func TestChurnBucketsAnswersNothingForAWindowWithNoRows(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	f := churnFilterFor(dumpRouterParityUnicast, time.Minute)
	f.Since = collectionFixtureAnchor.Add(24 * time.Hour)
	rows, err := q.ChurnBuckets(ctx, f)
	if err != nil {
		t.Fatalf("ChurnBuckets: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a window after every seeded row returned %d buckets, want none", len(rows))
	}
}

// A router= narrows to that router's own churn. Without this, one noisy
// router's re-advertisements would appear on every other router's chart.
func TestChurnBucketsNarrowToOneRouter(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	scoped, err := q.ChurnBuckets(ctx, churnFilterFor(dumpRouterParityUnicast, time.Hour))
	if err != nil {
		t.Fatalf("ChurnBuckets(scoped): %v", err)
	}
	fleet, err := q.ChurnBuckets(ctx, churnFilterFor("", time.Hour))
	if err != nil {
		t.Fatalf("ChurnBuckets(fleet): %v", err)
	}
	sr, sw, sd := sumChurn(scoped)
	fr, fw, fd := sumChurn(fleet)
	if sr+sw+sd == 0 {
		t.Fatal("the scoped answer is empty; the comparison below would hold vacuously")
	}
	if fr+fw+fd <= sr+sw+sd {
		t.Errorf("the fleet answer (%d rows classified) is not larger than one router's (%d); "+
			"router= is not narrowing anything", fr+fw+fd, sr+sw+sd)
	}
}

// The guard that keeps a chart request from asking for millions of rows: a
// window divided by a bucket width has to produce a drawable number of
// buckets. 90 days at one second is 7.8 million, which is not a chart.
func TestChurnBucketsRefusesAWindowItCannotDraw(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	f := ChurnFilter{Since: time.Now().Add(-90 * 24 * time.Hour), Bucket: time.Second}
	if _, err := q.ChurnBuckets(ctx, f); !errors.Is(err, ErrBadFilter) {
		t.Fatalf("ChurnBuckets(90d at 1s) error = %v, want ErrBadFilter", err)
	}
}

func TestChurnBucketsRefusesABucketOfZero(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	f := ChurnFilter{Since: time.Now().Add(-time.Hour)}
	if _, err := q.ChurnBuckets(ctx, f); !errors.Is(err, ErrBadFilter) {
		t.Fatalf("ChurnBuckets(bucket=0) error = %v, want ErrBadFilter", err)
	}
}

// churnRetentionTestDB is TestChurnGuardUsesTheLiveRetention's own database:
// the test rewrites route_unicast's TTL, which no other test's fixture may
// see (the same reasoning as retentionDaysTestDB in retention_test.go).
const churnRetentionTestDB = "vantage_query_churn_retention_test"

// TestChurnGuardUsesTheLiveRetention is the regression test for the
// bars-per-answer guard: an unbounded request (Since zero) used to assume
// the window was 90 days, hardcoded, when it is actually the archive's live
// history retention -- configurable per deployment (retention.days), and no
// longer always 90.
//
// route_unicast's TTL is rewritten to 365 days. The bucket (2h) is chosen so
// 90 days of it is 1,080 buckets -- under maxChurnBuckets (1,500), so a
// guard still reading a hardcoded 90 would let the request through -- while
// 365 days of it is 4,380 buckets, over the cap, so a guard reading the
// live retention must refuse it. No fixture rows are seeded: the guard
// returns before either statement ever runs.
func TestChurnGuardUsesTheLiveRetention(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, churnRetentionTestDB)
	q, err := New(conn, churnRetentionTestDB)
	if err != nil {
		t.Fatalf("New(%s): %v", churnRetentionTestDB, err)
	}
	stmt := "ALTER TABLE " + churnRetentionTestDB + ".route_unicast MODIFY TTL " +
		"toDateTime(ts_collector) + INTERVAL 365 DAY SETTINGS materialize_ttl_after_modify = 0"
	if err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	if got, err := q.RetentionDays(ctx); err != nil || got != 365 {
		t.Fatalf("RetentionDays after MODIFY TTL ... 365 DAY = (%d, %v), want (365, nil)", got, err)
	}

	f := ChurnFilter{Bucket: 2 * time.Hour} // Since left zero: unbounded
	if _, err := q.ChurnBuckets(ctx, f); !errors.Is(err, ErrBadFilter) {
		t.Fatalf("ChurnBuckets(unbounded, retention=365d, bucket=2h) error = %v, want ErrBadFilter "+
			"(365d/2h = 4,380 buckets, over the %d cap; a guard still reading a hardcoded 90 days "+
			"would compute 1,080 and let this through)", err, maxChurnBuckets)
	}
	if _, err := q.ChurnPeerActivity(ctx, f); !errors.Is(err, ErrBadFilter) {
		t.Fatalf("ChurnPeerActivity(unbounded, retention=365d, bucket=2h) error = %v, want ErrBadFilter", err)
	}
}

// churnFilterForPeers is churnFilterFor without a bucket: the two grouped
// answers below have no time axis, so Bucket is not part of their question.
func churnGroupingFilter() ChurnFilter {
	return ChurnFilter{
		Since:  collectionFixtureAnchor.Add(-time.Hour),
		Router: netip.MustParseAddr(churnRouterGrouping),
	}
}

// ChurnByPeer separates peers, and separates each peer's three series.
//
// The oracle is arithmetic rather than a dashboard panel, because
// route-churn.json's "Changes by peer" is a TIME SERIES (GROUP BY t,
// peer_ip) and this is a ranked total -- they share the classification, not
// the shape. The classification itself is already pinned against that
// panel by TestChurnBucketsAgreeWithTheDashboardClassification; what is
// new here is the grouping, so that is what this checks.
func TestChurnByPeerSeparatesPeersAndSeries(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	rows, err := q.ChurnByPeer(ctx, churnGroupingFilter())
	if err != nil {
		t.Fatalf("ChurnByPeer: %v", err)
	}

	// Keyed on the address AS RETURNED, not on r.PeerIP.Unmap().String():
	// normalizing the key here would look up "10.93.12.2" successfully even
	// when the answer carried "::ffff:10.93.12.2", which is the precise way
	// an IPv4-mapped address hides from a test that is otherwise about
	// something else. TestChurnByPeerReturnsUnmappedAddresses below is the
	// one that states it outright.
	got := make(map[string]PeerChurn, len(rows))
	for _, r := range rows {
		got[r.PeerIP.String()] = r
	}
	if len(got) != 2 {
		t.Fatalf("want one row per peer (2), got %d: %+v", len(got), rows)
	}

	// The busy peer, counted across ALL THREE route tables, which is what a
	// per-peer answer means: peer_ip names the same thing on every one of
	// them, so adding a peer's unicast, VPN and EVPN churn is addition. (The
	// prefix answer below is the opposite case and reads one table -- a
	// prefix does NOT name the same thing across the three.)
	//
	//   readvertise 3  10.93.1201.0/24 sent 4 times in one session: the
	//                  first is its dump, the other three are changes
	//   withdraw    1  10.93.1202.0/24's single row
	//   dump        4  first-in-session for each of four distinct routes --
	//                  unicast 10.93.1201.0/24, the undecoded unicast row,
	//                  the route_vpn row and the route_evpn row
	//
	// The 4 is the load-bearing number here: it is 1 if this reads one table
	// and 4 if it reads three, so a fold quietly dropped to route_unicast
	// fails here rather than under-reporting every peer in production.
	busy := got[churnPeerBusy]
	if busy.Readvertise != 3 || busy.Withdraw != 1 || busy.Dump != 4 {
		t.Errorf("busy peer: got %d readvertise / %d withdraw / %d dump, want 3/1/4",
			busy.Readvertise, busy.Withdraw, busy.Dump)
	}
	quiet := got[churnPeerQuiet]
	if quiet.Readvertise != 0 || quiet.Withdraw != 0 || quiet.Dump != 1 {
		t.Errorf("quiet peer: got %d readvertise / %d withdraw / %d dump, want 0/0/1",
			quiet.Readvertise, quiet.Withdraw, quiet.Dump)
	}
}

// Busiest first, so a screen that renders the answer in order renders a
// ranking. Ordering in SQL rather than in the client is what keeps a
// truncated answer meaningful: the top N of an unordered answer is N
// arbitrary peers.
func TestChurnByPeerRanksBusiestFirst(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	rows, err := q.ChurnByPeer(ctx, churnGroupingFilter())
	if err != nil {
		t.Fatalf("ChurnByPeer: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("want at least 2 peers to rank, got %d", len(rows))
	}
	if first := rows[0].PeerIP.Unmap().String(); first != churnPeerBusy {
		t.Errorf("first row is %s, want the busiest peer %s", first, churnPeerBusy)
	}
}

// ChurnByPrefix reads route_unicast ALONE.
//
// churnRouterGrouping writes 10.93.1201.0/24 into all three route tables on
// purpose. A three-table union would report 6 observations for it (4 unicast
// + 1 vpn + 1 evpn) under one label, merging a global-table route with a VRF
// route and an EVPN route that merely share a string -- and a route_vpn
// prefix without its rd is not even an identity. Four is the unicast answer.
func TestChurnByPrefixReadsUnicastAlone(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	rows, err := q.ChurnByPrefix(ctx, churnGroupingFilter())
	if err != nil {
		t.Fatalf("ChurnByPrefix: %v", err)
	}
	got := make(map[string]PrefixChurn, len(rows))
	for _, r := range rows {
		got[r.Prefix] = r
	}
	shared, ok := got["10.93.1201.0/24"]
	if !ok {
		t.Fatalf("10.93.1201.0/24 missing from the answer: %+v", rows)
	}
	if shared.Observations != 4 {
		t.Errorf("10.93.1201.0/24: %d observations, want the 4 unicast rows alone "+
			"(6 means the vpn and evpn rows sharing this prefix string were folded in)",
			shared.Observations)
	}
	if shared.Readvertise != 3 || shared.Dump != 1 {
		t.Errorf("10.93.1201.0/24: %d readvertise / %d dump, want 3/1",
			shared.Readvertise, shared.Dump)
	}
}

// An undecoded row has no prefix to be most-changed ABOUT.
//
// One measured archive held 56 route_unicast rows with prefix = "" carrying
// PARSE_FLAG_VERSION_UNPARSED. Ranked by change count they sort to the TOP,
// under a blank label -- a collection artifact presented as the network's
// busiest prefix. Excluded in SQL rather than filtered in the client,
// because a LIMITed answer filtered afterwards is short by however many
// artifacts it contained.
func TestChurnByPrefixExcludesUndecodedRows(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	rows, err := q.ChurnByPrefix(ctx, churnGroupingFilter())
	if err != nil {
		t.Fatalf("ChurnByPrefix: %v", err)
	}
	for _, r := range rows {
		if r.Prefix == "" {
			t.Fatalf("an empty prefix reached the answer (%+v); undecoded NLRI is a "+
				"collection artifact, not the network's busiest prefix", r)
		}
	}
	// And the guard is not passing because the answer is empty.
	if len(rows) != 3 {
		t.Errorf("want the 3 real prefixes this router advertises, got %d: %+v", len(rows), rows)
	}
}

// Addresses come back in one textual form, never the IPv4-mapped one.
//
// netip.Addr equality is representation-sensitive, so "::ffff:10.93.12.2"
// and "10.93.12.2" are different values naming one host. This answer is
// joined against /v1/peers by peer_ip in the client that reads it -- the
// Monitor table pairs a peer's churn with its ASN, state and route count --
// so a mapped address here is not cosmetic: every IPv4 peer would silently
// fail to match and the table would render churn with no peer beside it.
//
// unmapAll's own doc comment describes this defect and four call sites that
// once got it wrong. This is the fifth; it shipped mapped and was caught
// against a production archive, not by the tests above.
func TestChurnByPeerReturnsUnmappedAddresses(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	rows, err := q.ChurnByPeer(ctx, churnGroupingFilter())
	if err != nil {
		t.Fatalf("ChurnByPeer: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows, so nothing was checked")
	}
	for _, r := range rows {
		if r.RouterIP.Is4In6() {
			t.Errorf("router_ip came back IPv4-mapped (%s); see unmapAll", r.RouterIP)
		}
		if r.PeerIP.Is4In6() {
			t.Errorf("peer_ip came back IPv4-mapped (%s); see unmapAll", r.PeerIP)
		}
	}
}

// The dual-homed churn fixture: one router, two collectors, two peers whose
// collectors agree about one and disagree about the other. 10.93.14.x is
// claimed by nothing else in this package.
const (
	churnDualRouter    = "10.93.14.1"
	churnDualPeerAgree = "10.93.14.2"
	churnDualPeerSplit = "10.93.14.3"
	churnDualCollector = "query-test-c2"
)

// churnDualHomedOnce guards the fixture below, matching seedCollectionFixtures.
// It is not defensive tidiness: three tests read this fixture, churn COUNTS
// ROWS, and a second insertion of identical rows is visible until a merge
// collapses it -- so seeding per test inflated readvertise by one here and
// made the numbers drift between runs. The Loc-RIB fixture next door needs no
// such guard because its answer counts route KEYS through a GROUP BY, where a
// duplicate falls inside the group it already belongs to.
var churnDualHomedOnce sync.Once

// insertChurnDualHomedFixture writes one router seen by two collectors.
//
// churnDualPeerAgree is the normal case: both collectors saw the same three
// announcements, so every per-collector count is equal and only a SUM can
// inflate them. churnDualPeerSplit is the case that says WHICH collector an
// answer came from: the second collector saw four announcements where the
// first saw one, so "best vantage point", "first collector" and "sum" are
// three different numbers.
func insertChurnDualHomedFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	churnDualHomedOnce.Do(func() { seedChurnDualHomed(t, ctx, q) })
}

func seedChurnDualHomed(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "churn-dual-r1"
	base := collectionFixtureAnchor.Add(200 * time.Second)
	// seq is per-collector, so both collectors start theirs at 1. stream_seq
	// is the shared JetStream sequence and is unique across the fixture.
	streamSeq := uint64(9400)
	announce := func(collector, peer, prefix string, session, seq uint64) {
		streamSeq++
		ts := base.Add(time.Duration(seq) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: churnDualRouter, RouterSysname: sysname, PeerIP: peer,
			RIB: "in_pre", Collector: collector, PeerASN: 65000,
			PeerBGPID: peer, Family: "ipv4u", Prefix: prefix,
			SessionID: session, Seq: seq, StreamSeq: streamSeq,
			TsRouter: ts, TsCollector: ts,
		})
	}
	// Both collectors saw the same three announcements of one prefix: the
	// first is that route's session dump, the other two are changes.
	for _, c := range []struct {
		id      string
		session uint64
	}{{defaultFixtureCollector, 1}, {churnDualCollector, 2}} {
		for seq := uint64(1); seq <= 3; seq++ {
			announce(c.id, churnDualPeerAgree, "10.93.14.16/28", c.session, seq)
		}
	}
	// The split peer: one announcement on the first collector, four on the
	// second. Sum says 3 changes over 2 dumps, first-collector says 0 over 1,
	// and the best vantage point says 3 over 1.
	announce(defaultFixtureCollector, churnDualPeerSplit, "10.93.14.32/28", 1, 1)
	for seq := uint64(1); seq <= 4; seq++ {
		announce(churnDualCollector, churnDualPeerSplit, "10.93.14.32/28", 2, seq)
	}
}

// TestChurnByPeerReportsOneCollectorsViewNotTheSumOfBoth holds the fix for
// churnByPeerSQL double-counting a router two collectors both watch.
//
// churnByPeerSQL grouped by (router_ip, peer_ip) and counted rows from every
// collector, so a router watched by two collectors reported roughly twice the
// churn its peers actually produced. The inflation is not uniform either --
// measured 2.0x on one router in a test archive and 1.4x on another, because
// the collectors came up at different times -- so "busiest first" ranked by
// how many collectors were watching rather than by how much a peer churned.
//
// Churn is a fact about the PEER. Counting one event twice because two
// observers saw it is this project's most recurring defect: a collection
// artifact recorded as a fact about the network.
//
// There is no collector-independent event identity to deduplicate on. Two
// collectors observing one UPDATE agree on ts_router alone, and ts_router is
// the clock this project explicitly does not discriminate with -- a dead
// router clock reports 1970, and QK_TS_ZERO then substitutes each COLLECTOR's
// own now(), so the two would disagree exactly where the router's clock is
// broken. So the answer is one collector's coherent view: the one that saw
// the most, chosen by argMax over the row total so all three counts come from
// the same collector rather than a per-column maximum mixing them.
func TestChurnByPeerReportsOneCollectorsViewNotTheSumOfBoth(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertChurnDualHomedFixture(t, ctx, q)

	rows, err := q.ChurnByPeer(ctx, ChurnFilter{
		Router: netip.MustParseAddr(churnDualRouter),
		Since:  collectionFixtureAnchor.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("ChurnByPeer: %v", err)
	}
	got := make(map[string]PeerChurn, len(rows))
	for _, r := range rows {
		got[r.PeerIP.Unmap().String()] = r
	}
	if len(got) != 2 {
		t.Fatalf("want one row per peer (2), got %d: %+v", len(got), rows)
	}

	agree := got[churnDualPeerAgree]
	if agree.Readvertise != 2 || agree.Withdraw != 0 || agree.Dump != 1 {
		t.Errorf("the agreeing peer: got %d readvertise / %d withdraw / %d dump, "+
			"want 2/0/1. 4/0/2 is the defect -- both collectors saw the same "+
			"three announcements and the counts were summed, so one peer's "+
			"churn doubled because a second collector was watching",
			agree.Readvertise, agree.Withdraw, agree.Dump)
	}
	split := got[churnDualPeerSplit]
	if split.Readvertise != 3 || split.Withdraw != 0 || split.Dump != 1 {
		t.Errorf("the split peer: got %d readvertise / %d withdraw / %d dump, "+
			"want 3/0/1 -- the view of %s, which saw four announcements where "+
			"%s saw one. 3/0/2 is the sum, and 0/0/1 is taking whichever "+
			"collector sorted first rather than the one that saw the most",
			split.Readvertise, split.Withdraw, split.Dump,
			churnDualCollector, defaultFixtureCollector)
	}
}

// TestChurnBucketsReportOneCollectorsViewNotTheSumOfBoth is the time-series
// half of the same defect. Same fixture, same reasoning as ChurnByPeer: one
// wide bucket holding every seeded row, so the bucketing is not what is under
// test here -- the merge across collectors is.
func TestChurnBucketsReportOneCollectorsViewNotTheSumOfBoth(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertChurnDualHomedFixture(t, ctx, q)

	rows, err := q.ChurnBuckets(ctx, ChurnFilter{
		Router: netip.MustParseAddr(churnDualRouter),
		Since:  collectionFixtureAnchor.Add(-time.Hour),
		Bucket: time.Hour,
	})
	if err != nil {
		t.Fatalf("ChurnBuckets: %v", err)
	}
	var readvertise, withdraw, dump uint64
	for _, r := range rows {
		readvertise += r.Readvertise
		withdraw += r.Withdraw
		dump += r.Dump
	}
	// Collector query-test-c2 saw the most across both peers: 2 changes on the
	// agreeing peer and 3 on the split one, over one session dump each.
	if readvertise != 5 || withdraw != 0 || dump != 2 {
		t.Errorf("got %d readvertise / %d withdraw / %d dump, want 5/0/2 -- "+
			"%s's whole view. 7/0/4 is both collectors summed",
			readvertise, withdraw, dump, churnDualCollector)
	}
}

// TestChurnByPrefixCountsRoutesAndSessionsWithinOneCollector holds a
// defect this answer had twice over, in opposite directions, in one
// SELECT.
//
// Its counts summed both collectors, and its `routes` column was a
// uniqExact over a tuple LEADING with collector_id -- so one route two
// collectors both held counted as two ROUTES, in a column named routes.
// `sessions` counted per-collector session ids the same way.
//
// `observations` is the one column here that could honestly have doubled: it
// names what collection recorded, not what the network did. It is reported
// from the same single collector as the rest so the row stays one coherent
// view rather than a mix of two vantage points.
func TestChurnByPrefixCountsRoutesAndSessionsWithinOneCollector(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertChurnDualHomedFixture(t, ctx, q)

	rows, err := q.ChurnByPrefix(ctx, ChurnFilter{
		Router: netip.MustParseAddr(churnDualRouter),
		Since:  collectionFixtureAnchor.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("ChurnByPrefix: %v", err)
	}
	got := make(map[string]PrefixChurn, len(rows))
	for _, r := range rows {
		got[r.Prefix] = r
	}

	// Both collectors saw this prefix identically, so only a sum can move it.
	agree := got["10.93.14.16/28"]
	if agree.Observations != 3 || agree.Readvertise != 2 || agree.Dump != 1 {
		t.Errorf("10.93.14.16/28: got %d observations / %d readvertise / %d dump, "+
			"want 3/2/1; 6/4/2 is both collectors summed",
			agree.Observations, agree.Readvertise, agree.Dump)
	}
	if agree.Routes != 1 || agree.Sessions != 1 {
		t.Errorf("10.93.14.16/28: got %d routes / %d sessions, want 1/1. It is "+
			"ONE route in ONE session, seen by two collectors. 2/2 is the "+
			"collector_id still leading the routes tuple and the two "+
			"per-collector session ids being counted as two sessions",
			agree.Routes, agree.Sessions)
	}
	// And the prefix the collectors disagree about: the better-informed one.
	split := got["10.93.14.32/28"]
	if split.Observations != 4 || split.Readvertise != 3 || split.Dump != 1 {
		t.Errorf("10.93.14.32/28: got %d observations / %d readvertise / %d dump, "+
			"want 4/3/1 -- %s's view, which saw four announcements where %s saw one",
			split.Observations, split.Readvertise, split.Dump,
			churnDualCollector, defaultFixtureCollector)
	}
}

// TestChurnByPeerCollectorFilterNarrowsToOneView pins the drill-down half of
// the contract: the unscoped answer is the best vantage point, and naming a
// collector gets that collector even when it is the one that saw less. Without
// this, "best vantage point" and "the filter works" are indistinguishable on
// the collector that happens to win.
func TestChurnByPeerCollectorFilterNarrowsToOneView(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertChurnDualHomedFixture(t, ctx, q)

	for _, tc := range []struct {
		collector                     string
		readvertise, withdraw, dumped uint64
	}{
		{defaultFixtureCollector, 0, 0, 1},
		{churnDualCollector, 3, 0, 1},
	} {
		rows, err := q.ChurnByPeer(ctx, ChurnFilter{
			Router:    netip.MustParseAddr(churnDualRouter),
			Peer:      netip.MustParseAddr(churnDualPeerSplit),
			Since:     collectionFixtureAnchor.Add(-time.Hour),
			Collector: tc.collector,
		})
		if err != nil {
			t.Fatalf("ChurnByPeer(%s): %v", tc.collector, err)
		}
		if len(rows) != 1 {
			t.Fatalf("ChurnByPeer(%s) returned %d rows, want 1: %+v", tc.collector, len(rows), rows)
		}
		if r := rows[0]; r.Readvertise != tc.readvertise || r.Withdraw != tc.withdraw || r.Dump != tc.dumped {
			t.Errorf("collector %s: got %d readvertise / %d withdraw / %d dump, want %d/%d/%d",
				tc.collector, r.Readvertise, r.Withdraw, r.Dump,
				tc.readvertise, tc.withdraw, tc.dumped)
		}
	}
}

// The staggered churn fixture: one router, two collectors whose observation
// eras DO NOT OVERLAP. 10.93.15.x is claimed by nothing else in this package.
//
// insertChurnDualHomedFixture cannot stand in for it. There both collectors
// write at the same instants, so every row they duplicate shares a bucket
// and choosing the collector per bucket and choosing it per window give the
// same answer -- which is why TestChurnBucketsReportOneCollectorsViewNotThe
// SumOfBoth deliberately asks for one wide bucket.
//
// The shape is measured rather than invented: lab-archive router 172.22.0.8
// on 2026-09-20 held 20 rows under dev-c1 (09-17 to 09-18) and 8 under
// dev-c2 (09-20, three minutes), the SAME two ts_router values on both
// sides -- one captured session, replayed to a second collector two days
// later. The router advertised those routes once.
const (
	churnStagRouter    = "10.93.15.1"
	churnStagPeer      = "10.93.15.2"
	churnStagCollector = "query-test-c3"
	// churnStagLateGap puts the second collector's era ten buckets past the
	// first at the minute width the test asks for, so the two cannot share
	// one. It stays well inside the fixture window.
	churnStagLateGap = 600 * time.Second
)

var churnStaggeredOnce sync.Once

func insertChurnStaggeredFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	churnStaggeredOnce.Do(func() { seedChurnStaggered(t, ctx, q) })
}

func seedChurnStaggered(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "churn-stag-r1"
	const prefix = "10.93.15.64/28"
	// base is minute-aligned so the early collector's three rows, one
	// second apart, share one minute bucket whatever second the anchor
	// fell on. Unaligned, an anchor at :57 or :58 splits them across two
	// buckets and the test reads a boundary it is not about.
	base := collectionFixtureAnchor.Truncate(time.Minute).Add(300 * time.Second)
	streamSeq := uint64(9500)
	// One router-side instant for every row, both collectors alike: the
	// router advertised this route once and each collector wrote its own
	// copies down. ts_router is the reader's evidence for that and is used
	// for nothing else -- churnByPeerSQL says at length why no query here
	// may key on it.
	tsRouter := base
	announce := func(collector string, session, seq uint64, at time.Time) {
		streamSeq++
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: churnStagRouter, RouterSysname: sysname, PeerIP: churnStagPeer,
			RIB: "in_pre", Collector: collector, PeerASN: 65000,
			PeerBGPID: churnStagPeer, Family: "ipv4u", Prefix: prefix,
			SessionID: session, Seq: seq, StreamSeq: streamSeq,
			TsRouter: tsRouter, TsCollector: at,
		})
	}
	// The early collector's era: three observations, so one session dump and
	// two re-advertisements.
	for seq := uint64(1); seq <= 3; seq++ {
		announce(defaultFixtureCollector, 1, seq, base.Add(time.Duration(seq)*time.Second))
	}
	// The late collector's era: two observations of the SAME route, ten
	// minutes later. One dump and one re-advertisement in its own view, and
	// nothing new about the router. Fewer than the early collector's, so
	// "best vantage point" and "latest collector" are different answers.
	late := base.Add(churnStagLateGap)
	for seq := uint64(1); seq <= 2; seq++ {
		announce(churnStagCollector, 2, seq, late.Add(time.Duration(seq)*time.Second))
	}
}

// TestChurnBucketsPickTheirCollectorOnceOverTheWindow is the fix for
// ChurnBuckets picking its vantage point per bucket, applied to the Go
// answer rather than to the panel.
//
// ChurnBuckets picked the best vantage point PER BUCKET. That defends
// against two collectors' copies of one event only when both copies land in
// the same bucket -- and ts_collector, the column that separates them into
// different buckets, is precisely the column that differs between
// collectors. So the per-bucket defense fails exactly where duplication is
// most likely: a collector that joins, leaves, restarts, or is replayed to
// at a different time from its sibling.
//
// It also draws a line whose consecutive points come from different
// observers, which is the defect this fix corrects: a collector restarting
// mid-window puts a step in the chart that no router caused.
//
// This must move with route-churn.json and evpn-churn.json rather than after
// them: leaving the Go layer and the dashboards on different rules is the
// same class of defect fixed once already in the ASN-view endpoint, where
// one question answers differently depending on whether you read the panel
// or the API.
func TestChurnBucketsPickTheirCollectorOnceOverTheWindow(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertChurnStaggeredFixture(t, ctx, q)

	rows, err := q.ChurnBuckets(ctx, ChurnFilter{
		Router: netip.MustParseAddr(churnStagRouter),
		Since:  collectionFixtureAnchor.Add(-time.Hour),
		Bucket: time.Minute,
	})
	if err != nil {
		t.Fatalf("ChurnBuckets: %v", err)
	}
	var readvertise, withdraw, dump uint64
	for _, r := range rows {
		readvertise += r.Readvertise
		withdraw += r.Withdraw
		dump += r.Dump
	}
	if len(rows) != 1 || readvertise != 2 || withdraw != 0 || dump != 1 {
		t.Errorf("got %d buckets totalling %d readvertise / %d withdraw / "+
			"%d dump, want 1 bucket of 2/0/1 -- %s saw this route three "+
			"times and %s holds two copies of it from ten minutes later. "+
			"2 buckets of 3/0/2 is the per-bucket choice: the two eras never "+
			"share a bucket, so the best-vantage comparison never sees them "+
			"together and adds the second collector's copies instead",
			len(rows), readvertise, withdraw, dump,
			defaultFixtureCollector, churnStagCollector)
	}
}

// ChurnPeerActivity draws each peer's changes over time, and the shape it
// has to hold is the one the summed ranking beside it cannot: a peer that
// churned steadily and one that churned all at once have the same total and
// different series.
//
// The oracle is the SUM: every peer's activity buckets must add up to that
// peer's own readvertise + withdraw from ChurnByPeer, computed over the same
// window by the same classification. Two statements agreeing on a number
// they derive differently is what makes this more than a restatement -- a
// series that dropped a bucket, double-counted a collector, or classified
// dumps as changes would break the identity.
func TestChurnPeerActivitySumsToTheRankedTotals(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	f := churnGroupingFilter()
	f.Bucket = time.Minute

	ranked, err := q.ChurnByPeer(ctx, f)
	if err != nil {
		t.Fatalf("ChurnByPeer: %v", err)
	}
	series, err := q.ChurnPeerActivity(ctx, f)
	if err != nil {
		t.Fatalf("ChurnPeerActivity: %v", err)
	}
	if len(series) == 0 {
		t.Fatal("no activity rows at all -- the series is vacuous and the sums below would pass on nothing")
	}

	summed := map[string]uint64{}
	for _, a := range series {
		summed[a.PeerIP.String()] += a.Changes
	}
	// NOT parity with the ranking's row count, and the fixture is what
	// taught that: its second peer archived dumps and no changes, so it
	// ranks (with zero changes) and draws no bars at all. An empty series
	// for a peer that changed nothing is the honest answer -- zero-filling
	// it would make the row count a function of the window rather than of
	// the data. What must hold is the SUM, peer by peer, including the
	// zero.
	if len(summed) == 0 {
		t.Fatal("no peer has any activity -- the per-peer sums below would pass on nothing")
	}
	// A bar of zero is not a bar. Buckets that held no change are absent
	// rather than emitted, so a caller drawing these never has to tell a
	// quiet bucket from a drawn nothing -- and the row count stays a
	// function of the data rather than of the window's width.
	for _, a := range series {
		if a.Changes == 0 {
			t.Errorf("peer %s has a zero-change bucket at %v: those are absent, not emitted",
				a.PeerIP, a.Bucket)
		}
	}
	for _, r := range ranked {
		want := uint64(r.Readvertise + r.Withdraw)
		if got := summed[r.PeerIP.String()]; got != want {
			t.Errorf("peer %s: activity sums to %d, ranked changes are %d",
				r.PeerIP, got, want)
		}
	}
}

// Dumps are not changes, here as everywhere else. The ranking excludes them
// from its ordering and this series excludes them from its bars, so a peer
// whose session restarted does not draw a spike it did not cause.
func TestChurnPeerActivityCountsChangesNotDumps(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	f := churnGroupingFilter()
	f.Bucket = time.Minute

	ranked, err := q.ChurnByPeer(ctx, f)
	if err != nil {
		t.Fatalf("ChurnByPeer: %v", err)
	}
	series, err := q.ChurnPeerActivity(ctx, f)
	if err != nil {
		t.Fatalf("ChurnPeerActivity: %v", err)
	}
	// The fixture has to contain a dump for this to mean anything, or the
	// assertion below passes on a set where changes and rows are the same.
	var dumps uint64
	for _, r := range ranked {
		dumps += uint64(r.Dump)
	}
	if dumps == 0 {
		t.Fatal("fixture carries no session dumps -- this test cannot tell changes from rows")
	}
	var total uint64
	for _, a := range series {
		total += a.Changes
	}
	var changes, all uint64
	for _, r := range ranked {
		changes += uint64(r.Readvertise + r.Withdraw)
		all += uint64(r.Readvertise + r.Withdraw + r.Dump)
	}
	if total != changes {
		t.Errorf("activity totals %d, want %d (changes only)", total, changes)
	}
	if total == all {
		t.Errorf("activity totals %d, which is every row including dumps", total)
	}
}

// The vantage point is chosen ONCE over the window, not per bucket -- the
// same property ChurnBuckets holds, on the per-peer series.
//
// The fixture is the staggered one for the reason that test gives: the two
// collectors' eras never share a bucket, so a per-bucket comparison never
// sees them together and adds the second collector's copies instead of
// choosing between them. A single-collector fixture cannot tell any of the
// three readings apart, which is how two mutations survived this statement's
// first run -- summing collectors, and choosing per bucket, both passed.
func TestChurnPeerActivityPicksItsCollectorOnceOverTheWindow(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertChurnStaggeredFixture(t, ctx, q)

	rows, err := q.ChurnPeerActivity(ctx, ChurnFilter{
		Router: netip.MustParseAddr(churnStagRouter),
		Since:  collectionFixtureAnchor.Add(-time.Hour),
		Bucket: time.Minute,
	})
	if err != nil {
		t.Fatalf("ChurnPeerActivity: %v", err)
	}
	var changes uint64
	for _, r := range rows {
		changes += r.Changes
	}
	// One bucket, two changes: the best-placed collector saw this route
	// three times in one minute, of which one was its session dump and two
	// were re-advertisements. The other collector's two copies land ten
	// minutes later and belong to a view that saw less.
	if len(rows) != 1 || changes != 2 {
		t.Errorf("got %d buckets totalling %d changes, want 1 bucket of 2 -- "+
			"2 buckets is the per-bucket choice or the sum of both collectors, "+
			"neither of which is one vantage point over the window (%s vs %s)",
			len(rows), changes, defaultFixtureCollector, churnStagCollector)
	}
}
