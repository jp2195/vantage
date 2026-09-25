package query

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// collectionFixtureAnchor is the instant every route row this file writes is
// stamped at, truncated to a microsecond for the reason
// eventsFixtureAnchor is: ts_collector is DateTime64(6) and an untruncated
// time.Now() does not survive the round trip intact.
//
// It is time.Now() rather than a fixed date, for eventsFixtureAnchor's OTHER
// reason as well: all three route tables carry `TTL toDateTime(ts_collector)
// + INTERVAL 90 DAY` (schema.sql), so a fixed anchor would become
// TTL-eligible and a background merge could purge these rows mid-test. It
// also has to be near now() for TestDumpCountsAgreesWithTheDashboards, whose
// substituted $__timeFilter is a window around now() exactly as Grafana's
// own would be.
var collectionFixtureAnchor = time.Now().UTC().Truncate(time.Microsecond)

// Routers this file reserves. testDB is shared by every test in this package
// and never truncated, so each fixture gets a router IP nothing else writes
// under -- 10.93.0.0/16 is unused elsewhere in query/ -- and every assertion
// below is made through CollectionFilter.Router rather than over the whole
// table.
const (
	// dumpRouterRedump advertises each of its routes exactly once per
	// session, across five sessions: an archive made entirely of session
	// re-dumps.
	dumpRouterRedump = "10.93.1.1"
	// dumpRouterChange advertises each of its routes five times inside ONE
	// session: an archive made entirely of genuine re-advertisement. It
	// carries the same NUMBER of rows as dumpRouterRedump, which is the
	// point -- a query that only counts rows cannot tell the two apart.
	dumpRouterChange = "10.93.2.1"
	// dumpRouterWithdraw's every route opens its session with a WITHDRAWAL.
	dumpRouterWithdraw = "10.93.3.1"
	// dumpRouterVPNIdentity holds two route_vpn rows differing only in rd.
	dumpRouterVPNIdentity = "10.93.4.1"
	// dumpRouterEVPNIdentity holds two route_evpn type-2 rows differing only in ip.
	dumpRouterEVPNIdentity = "10.93.5.1"
	// dumpRouterParityUnicast and dumpRouterParityEVPN each carry rows in
	// exactly ONE route table, so DumpCounts' per-router answer for them can
	// be compared against a single dashboard's SQL. See
	// TestDumpCountsAgreesWithTheDashboards.
	dumpRouterParityUnicast = "10.93.6.1"
	dumpRouterParityEVPN    = "10.93.7.1"
	// locRIBRouterNoStat carries three peers built to separate "the router
	// never sent stat type 8" from "the router reported zero" -- see
	// seedLocRIBNoStatFixtures.
	locRIBRouterNoStat = "10.93.8.1"
	// locRIBRouterLatest carries one peer whose Loc-RIB stat is reported
	// twice, at different sizes -- see seedLocRIBLatestFixture.
	locRIBRouterLatest = "10.93.9.1"
	// locRIBRouterMixed carries one peer with BOTH adj-RIB-in and Loc-RIB
	// route_unicast rows, at different counts -- see
	// seedLocRIBMixedRIBFixture.
	locRIBRouterMixed = "10.93.10.1"
	// locRIBRouterWindow carries two peers whose Loc-RIB rows are written
	// 48 HOURS BEFORE the anchor, far outside any window this endpoint
	// offers -- see seedLocRIBWindowFixture.
	locRIBRouterWindow = "10.93.11.1"
	// churnRouterGrouping is the only fixture router in this file with more
	// than one peer, and the only one carrying rows in all three route
	// tables at once. Both are deliberate: ChurnByPeer's whole claim is that
	// it separates peers, which one peer cannot show, and ChurnByPrefix's is
	// that it reads route_unicast ALONE -- see seedChurnGroupingFixtures.
	churnRouterGrouping = "10.93.12.1"
)

// The two peers churnRouterGrouping writes under. dumpPeer is deliberately
// not reused: a grouping test whose peers are one fixture-wide constant and
// one local value would still pass if the GROUP BY named the wrong column.
const (
	churnPeerBusy  = "10.93.12.2"
	churnPeerQuiet = "10.93.12.3"
)

// dumpPeer is the one BGP peer every fixture in this file writes under. The
// classification partitions on peer_ip among much else, but nothing here
// turns on having two peers, and one peer keeps each fixture's expected
// numbers computable by hand.
const dumpPeer = "10.93.0.2"

// collectionFilterFor is the scoped filter this file's assertions are made
// through: one router, no time bound, default limit.
func collectionFilterFor(router string) CollectionFilter {
	return CollectionFilter{Router: netip.MustParseAddr(router)}
}

// collectionFixturesOnce guards every seed function below, because testDB is
// recreated once per test BINARY (chtest.ensureDatabase) rather than once per
// test, and several tests in this file need the same population. Seeding
// twice would not raise: these are ReplacingMergeTree tables, a byte-identical
// second insert lands as a second row in a new part, and this package reads
// no FINAL -- so every hand-computed count below would silently double
// depending on which tests ran and in what order.
var collectionFixturesOnce sync.Once

// seedCollectionFixtures writes this file's entire population, exactly once
// per test binary. Every test calls it; none of them may seed anything of
// their own outside it.
func seedCollectionFixtures(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	collectionFixturesOnce.Do(func() {
		seedRedumpRouter(t, ctx, q)
		seedChangeRouter(t, ctx, q)
		seedWithdrawRouter(t, ctx, q)
		seedIdentityRouters(t, ctx, q)
		seedParityRouters(t, ctx, q)
		seedSessionCountsFixtures(t, ctx, q)
		seedSessionCountsDualHomed(t, ctx, q)
		seedSessionCountsTie(t, ctx, q)
		seedLocRIBNoStatFixtures(t, ctx, q)
		seedLocRIBLatestFixture(t, ctx, q)
		seedLocRIBMixedRIBFixture(t, ctx, q)
		seedLocRIBWindowFixture(t, ctx, q)
		seedLocRIBTwoSessionFixture(t, ctx, q)
		seedLocRIBSessionFallbackFixture(t, ctx, q)
		seedFlagCountsFixtures(t, ctx, q)
		seedChurnGroupingFixtures(t, ctx, q)
	})
}

// locRIBPeerNeverReported never sends a stats_events row at all: the
// simplest reading of "never sent stat type 8". Its Archived count (3) is
// what lets it appear in
// LocRIBComparison's answer despite having nothing in stats_events -- the
// identity comes from the archived side's UNION half, not the reported one.
const locRIBPeerNeverReported = "10.93.8.2"

// locRIBPeerNoCounter8 sends stats_events rows, but never one carrying
// counter 8 (its map holds only key 1). Without this peer, a mutation that
// deletes the `has(counters, 8)` restriction from the reported side's own
// WHERE -- rather than the final output column -- could pass
// TestLocRIBComparisonDistinguishesNoStatFromZero by coincidence:
// locRIBPeerNeverReported has NO row in stats_events under any WHERE, so
// dropping one predicate from that WHERE cannot change its own HasStat at
// all. Only a peer that DOES have a stats_events row, just never one with
// key 8, can catch that specific mutation. Its Archived count (5) differs
// from every other peer's in this fixture.
const locRIBPeerNoCounter8 = "10.93.8.3"

// locRIBPeerZero reports an EXPLICIT Loc-RIB size of zero: counters[8] = 0,
// with key 8 present. This is the value TestLocRIBComparisonDistinguishesNoStatFromZero
// exists to hold apart from locRIBPeerNeverReported's and
// locRIBPeerNoCounter8's -- all three read Reported = 0, and only HasStat
// tells this peer's real, router-reported zero from the other two peers'
// collection artifact. Its Archived count (7) is the third, distinct value.
const locRIBPeerZero = "10.93.8.4"

// seedLocRIBNoStatFixtures writes locRIBRouterNoStat's three peers: one with
// no stats_events row, one whose stats_events rows never carry counter 8,
// and one that reports counter 8 as an explicit zero. All three read
// Reported = 0 from LocRIBComparison; HasStat is the only column that can
// tell them apart, and the point of this fixture is that it must.
//
// Every peer's Archived count is a different number (3, 5, 7) -- not
// because any test here asserts Archived's exact value against a
// specific peer by name (none does; TestLocRIBComparisonDistinguishesNoStatFromZero
// only checks HasStat), but so a future reader extending this fixture, or a
// mutation that swaps which peer's join row lands on which, cannot pass by
// two peers' Archived counts happening to coincide.
func seedLocRIBNoStatFixtures(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "loc-rib-no-stat-router"
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterNoStat, sysname,
		locRIBPeerNeverReported, 1, 1, collectionFixtureAnchor)

	// liveRoutes writes rib = 'loc_rib' rows, not in_pre: LocRIBComparison's
	// Archived counts ONLY loc_rib rows (see collection.go's own doc comment
	// on why an unfiltered count compares a Loc-RIB number against an
	// almost-entirely-adj-RIB-in population). A fixture that wrote in_pre
	// here would read Archived = 0 for every peer below regardless of n,
	// which would both misrepresent what this fixture is claiming to seed
	// and collapse the three peers' otherwise pairwise-distinct Archived
	// values onto 0.
	liveRoutes := func(peer string, n int) {
		for i := range n {
			ts := collectionFixtureAnchor.Add(time.Duration(i+1) * time.Second)
			insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
				RouterIP: locRIBRouterNoStat, RouterSysname: sysname, PeerIP: peer,
				RIB: "loc_rib", PeerBGPID: peer, Family: "ipv4u",
				Prefix:    fmt.Sprintf("10.93.80.%d/32", i),
				SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
				TsRouter: ts, TsCollector: ts,
			})
		}
	}

	// locRIBPeerNeverReported: three live routes, zero stats_events rows.
	liveRoutes(locRIBPeerNeverReported, 3)

	// locRIBPeerNoCounter8: five live routes, plus a stats_events row that
	// carries some OTHER counter but never key 8.
	liveRoutes(locRIBPeerNoCounter8, 5)
	insertStatsEvent(t, ctx, q, statsEventFixture{
		RouterIP: locRIBRouterNoStat, RouterSysname: sysname, PeerIP: locRIBPeerNoCounter8,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerNoCounter8,
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Counters:    map[uint32]uint64{1: 999},
		TsRouter:    collectionFixtureAnchor,
		TsCollector: collectionFixtureAnchor,
	})

	// locRIBPeerZero: seven live routes, plus a stats_events row that
	// carries counter 8 as an EXPLICIT zero.
	liveRoutes(locRIBPeerZero, 7)
	insertStatsEvent(t, ctx, q, statsEventFixture{
		RouterIP: locRIBRouterNoStat, RouterSysname: sysname, PeerIP: locRIBPeerZero,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerZero,
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Counters:    map[uint32]uint64{8: 0},
		TsRouter:    collectionFixtureAnchor,
		TsCollector: collectionFixtureAnchor,
	})
}

// locRIBPeerLatest is the one peer TestLocRIBComparisonTakesTheLatestStatNotTheFirst
// scopes its assertion to.
const locRIBPeerLatest = "10.93.9.2"

// seedLocRIBLatestFixture writes locRIBRouterLatest/locRIBPeerLatest two
// stats_events rows reporting two DIFFERENT Loc-RIB sizes -- 140 and 610 --
// so that argMax and argMin disagree about the answer and a test can tell
// them apart. The newer row (610, by (ts_collector, stream_seq)) is written
// FIRST, physically, and the older row (140) SECOND: a reader that resolved
// "the current stat" by insertion order rather than by the tie-break column
// pair would get this backwards, the same defense insertTwoSessionRouter's
// own doc comment gives for reversing ITS insertion order.
//
// It also carries four live route_unicast rows, a number distinct from
// every count in seedLocRIBNoStatFixtures, so this peer's Archived answer
// cannot be mistaken for one of that fixture's three peers either.
func seedLocRIBLatestFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "loc-rib-latest-router"
	older := collectionFixtureAnchor.Add(1 * time.Second)
	newer := collectionFixtureAnchor.Add(5 * time.Second)
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterLatest, sysname,
		locRIBPeerLatest, 1, 1, collectionFixtureAnchor)

	insertStatsEvent(t, ctx, q, statsEventFixture{
		RouterIP: locRIBRouterLatest, RouterSysname: sysname, PeerIP: locRIBPeerLatest,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerLatest,
		SessionID: 1, Seq: 2, StreamSeq: 5,
		Counters:    map[uint32]uint64{8: 610},
		TsRouter:    newer,
		TsCollector: newer,
	})
	insertStatsEvent(t, ctx, q, statsEventFixture{
		RouterIP: locRIBRouterLatest, RouterSysname: sysname, PeerIP: locRIBPeerLatest,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerLatest,
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Counters:    map[uint32]uint64{8: 140},
		TsRouter:    older,
		TsCollector: older,
	})

	// rib = 'loc_rib', for the same reason seedLocRIBNoStatFixtures' own
	// liveRoutes helper gives: an in_pre row here would not count toward
	// Archived at all once the rib filter is applied.
	for i := range 4 {
		ts := collectionFixtureAnchor.Add(time.Duration(i+1) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: locRIBRouterLatest, RouterSysname: sysname, PeerIP: locRIBPeerLatest,
			RIB: "loc_rib", PeerBGPID: locRIBPeerLatest, Family: "ipv4u",
			Prefix:    fmt.Sprintf("10.93.90.%d/32", i),
			SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
			TsRouter: ts, TsCollector: ts,
		})
	}
}

// locRIBPeerMixed is the one peer TestLocRIBComparisonCountsLocRIBRowsOnlyNotAdjRIBIn
// scopes its assertion to.
const locRIBPeerMixed = "10.93.10.2"

// seedLocRIBMixedRIBFixture writes locRIBRouterMixed/locRIBPeerMixed SIX
// live route_unicast rows under rib = 'in_pre' and TWO live route_unicast
// rows under rib = 'loc_rib', at different prefixes so both sets are real,
// distinct route keys rather than one row counted twice under two RIB
// labels. This is the fixture the fix in this file exists around: a peer
// whose in_pre and loc_rib counts DISAGREE (6 vs 2) is what makes the
// rib = 'loc_rib' filter's presence in LocRIBComparison observable at all --
// a fixture that only ever wrote loc_rib rows (as
// seedLocRIBNoStatFixtures' and seedLocRIBLatestFixture's own do) cannot
// tell a query that filters by rib apart from one that does not, because
// there would be nothing of the OTHER rib for a dropped filter to
// erroneously include.
//
// It also carries a stats_events row reporting counters[8] = 2 -- matching
// the loc_rib count exactly -- so the fixture also demonstrates the
// well-behaved case: when Loc-RIB monitoring IS enabled and archived,
// Reported and Archived agree and the comparison shows no gap at all. That
// is the fixture's second job, alongside pinning the filter: showing what
// "correct" looks like, not only what "wrong" looks like.
func seedLocRIBMixedRIBFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "loc-rib-mixed-router"
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterMixed, sysname,
		locRIBPeerMixed, 1, 1, collectionFixtureAnchor)

	route := func(rib, prefix string, n int) {
		for i := range n {
			ts := collectionFixtureAnchor.Add(time.Duration(i+1) * time.Second)
			insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
				RouterIP: locRIBRouterMixed, RouterSysname: sysname, PeerIP: locRIBPeerMixed,
				RIB: rib, PeerBGPID: locRIBPeerMixed, Family: "ipv4u",
				Prefix:    fmt.Sprintf("%s.%d/32", prefix, i),
				SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
				TsRouter: ts, TsCollector: ts,
			})
		}
	}
	route("in_pre", "10.93.100", 6)
	route("loc_rib", "10.93.101", 2)

	insertStatsEvent(t, ctx, q, statsEventFixture{
		RouterIP: locRIBRouterMixed, RouterSysname: sysname, PeerIP: locRIBPeerMixed,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerMixed,
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Counters:    map[uint32]uint64{8: 2},
		TsRouter:    collectionFixtureAnchor,
		TsCollector: collectionFixtureAnchor,
	})
}

// locRIBRowFor runs LocRIBComparison scoped to one router and returns the
// row for one peer -- dumpCountFor's own contract does not fit here, because
// unlike DumpCounts and SessionCounts, LocRIBComparison returns one row PER
// PEER, not one row per router; a router-scoped filter can legitimately come
// back with several rows. This fails rather than skips when the peer is
// missing, for dumpCountFor's own reason: every caller has just seeded that
// peer, so its absence is the query being wrong, not the fixture.
func locRIBRowFor(t *testing.T, ctx context.Context, q *Q, router, peer string) PeerLocRIB {
	t.Helper()
	rows, _, err := q.LocRIBComparison(ctx, collectionFilterFor(router))
	if err != nil {
		t.Fatalf("LocRIBComparison(%s): %v", router, err)
	}
	want := netip.MustParseAddr(peer)
	for _, r := range rows {
		if r.PeerIP == want {
			return r
		}
	}
	t.Fatalf("LocRIBComparison(%s) returned no row for peer %s, among %d rows: %+v",
		router, peer, len(rows), rows)
	return PeerLocRIB{}
}

// TestLocRIBComparisonDistinguishesNoStatFromZero pins this defect as a
// test. counters[8] on a peer that never sent stat type 8 yields 0, exactly
// like a peer that reported an empty Loc-RIB. HasStat is what tells them
// apart, and without it the screen invents a gap for every silent peer.
//
// All three peers read Reported = 0; only HasStat may differ, and it must:
// false for the peer with no stats_events row at all, false for the peer
// whose stats_events rows never carry counter 8, true for the peer that
// reported an explicit zero.
func TestLocRIBComparisonDistinguishesNoStatFromZero(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	neverReported := locRIBRowFor(t, ctx, q, locRIBRouterNoStat, locRIBPeerNeverReported)
	noCounter8 := locRIBRowFor(t, ctx, q, locRIBRouterNoStat, locRIBPeerNoCounter8)
	zero := locRIBRowFor(t, ctx, q, locRIBRouterNoStat, locRIBPeerZero)

	if neverReported.HasStat {
		t.Errorf("peer with NO stats_events row: HasStat = true, want false -- "+
			"nothing was ever sent, so Reported = %d must not be read as a claim "+
			"about the network", neverReported.Reported)
	}
	if noCounter8.HasStat {
		t.Errorf("peer whose stats_events rows never carry counter 8: HasStat = "+
			"true, want false -- Reported = %d is a Map returning its missing-key "+
			"default, not a router claim", noCounter8.Reported)
	}
	if !zero.HasStat {
		t.Errorf("peer that reported counter 8 as an explicit zero: HasStat = "+
			"false, want true -- this Reported = %d IS a router claim and must not "+
			"be indistinguishable from the two peers above that never sent one",
			zero.Reported)
	}
	if neverReported.Reported != 0 || noCounter8.Reported != 0 || zero.Reported != 0 {
		t.Fatalf("fixture assumption broken: all three peers must read Reported = 0 "+
			"for HasStat to be the only column telling them apart; got %d/%d/%d",
			neverReported.Reported, noCounter8.Reported, zero.Reported)
	}

	// Archived is pairwise distinct across the three peers (3, 5, 7), so a
	// mutation that swapped which peer's row a join landed on would also be
	// visible here even though no assertion above pins Archived to an exact
	// peer by number.
	archived := map[string]uint64{
		locRIBPeerNeverReported: neverReported.Archived,
		locRIBPeerNoCounter8:    noCounter8.Archived,
		locRIBPeerZero:          zero.Archived,
	}
	seen := map[uint64]bool{}
	for peer, n := range archived {
		if seen[n] {
			t.Errorf("Archived counts are not pairwise distinct across this fixture's "+
				"three peers (%v); peer %s collides on %d", archived, peer, n)
		}
		seen[n] = true
	}
}

// TestLocRIBComparisonTakesTheLatestStatNotTheFirst. Stats arrive repeatedly;
// the current Loc-RIB size is the newest, by (ts_collector, stream_seq).
func TestLocRIBComparisonTakesTheLatestStatNotTheFirst(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := locRIBRowFor(t, ctx, q, locRIBRouterLatest, locRIBPeerLatest)
	if got.Reported != 610 {
		t.Errorf("Reported = %d, want 610 (the NEWER of two stats_events rows, "+
			"by (ts_collector, stream_seq)); 140 is the older report and must lose",
			got.Reported)
	}
	if !got.HasStat {
		t.Errorf("HasStat = false, want true -- this peer sent counter 8 twice")
	}
	if got.Archived != 4 {
		t.Errorf("Archived = %d, want 4", got.Archived)
	}
}

// TestLocRIBComparisonCountsLocRIBRowsOnlyNotAdjRIBIn is the fix this file
// carries: Archived must count ONLY route_unicast rows under rib =
// 'loc_rib', never a peer's adj-RIB-in rows too. Counter 8 (RFC 7854 §4.8)
// is a Loc-RIB route count; comparing it against route_unicast rows drawn
// from every RIB view compares a Loc-RIB number against a population that is
// mostly adj-RIB-in on a real captured archive (8,553 in_pre rows across 24
// peers versus 27 loc_rib rows across 2, measured 2026-09-11) -- not a
// looser answer, a wrong one. See collection.go's own doc comment on
// PeerLocRIB and LocRIBComparison for the fuller argument, including why
// fleet-health's "Loc-RIB routes by peer" panel is not prior art for the
// unfiltered form: that panel never joins route_unicast at all.
//
// seedLocRIBMixedRIBFixture seeds SIX in_pre rows and TWO loc_rib rows for
// the same peer, so an unfiltered Archived would read 8 and the correct,
// filtered answer reads 2 -- the two numbers disagree, which is what makes
// the filter's presence observable rather than a no-op either way.
func TestLocRIBComparisonCountsLocRIBRowsOnlyNotAdjRIBIn(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := locRIBRowFor(t, ctx, q, locRIBRouterMixed, locRIBPeerMixed)
	if got.Archived != 2 {
		t.Errorf("Archived = %d, want 2 -- only this peer's TWO loc_rib rows should "+
			"count; its SIX in_pre rows must be excluded (an unfiltered count would "+
			"read 8)", got.Archived)
	}
	if got.Reported != 2 {
		t.Errorf("Reported = %d, want 2", got.Reported)
	}
	if !got.HasStat {
		t.Errorf("HasStat = false, want true -- this peer did send counter 8")
	}
}

// locRIBPeerOldRoutes and locRIBPeerOldStat are seedLocRIBWindowFixture's
// two peers. Both write every route_unicast row 48 hours before the anchor;
// they differ only in WHEN their stats_events row lands.
//
// locRIBPeerOldRoutes' stat is INSIDE any window a caller can ask for (it
// sits at the anchor, which is time.Now()). It is the peer that reads as a
// stable Loc-RIB: the router dumped its Loc-RIB once, at session start, and
// has written no route row since, while it keeps reporting counter 8.
//
// locRIBPeerOldStat's stat is 48 hours old alongside its routes, so a recent
// window excludes it. It is the peer that proves the window still bites on
// the stats side -- see TestLocRIBComparisonArchivedSideIgnoresTheWindow.
const (
	locRIBPeerOldRoutes = "10.93.11.2"
	locRIBPeerOldStat   = "10.93.11.3"
)

// locRIBWindowOffset is how far before collectionFixtureAnchor this fixture
// writes. It has to be wider than any window a caller of this endpoint can
// ask for -- MonitorView offers 1h, 6h and 24h, and api/ clamps since= at
// max_unscoped_since (24h by default) -- so that a test asking for one hour
// and a test asking for a day are asking the same question of this fixture.
// 48h clears all of them with room to spare and stays far inside every route
// table's own 90-day TTL.
const locRIBWindowOffset = -48 * time.Hour

// seedLocRIBWindowFixture is the fixture for a defect:
// LocRIBComparison windowed the ARCHIVED side, so a peer whose
// Loc-RIB is stable -- rows written once at session start and nothing since
// -- decayed to Archived = 0 at every window the Monitor screen offers,
// against a Reported that held. That is a fabricated Loc-RIB gap rendered on
// the Monitor screen, which must never render a Loc-RIB gap as proof of
// loss.
//
// No fixture in this file could catch it, and the reason is worth naming:
// collectionFilterFor sets no Since at all, so every existing assertion here
// runs against a filter whose window is the zero time, which is exactly the
// case the defect does not show up in. This fixture's tests build their own
// CollectionFilter instead.
//
// Both peers get their route_unicast rows at locRIBWindowOffset before the
// anchor -- outside every window -- and the counts are chosen so that no two
// numbers in the assertions can be swapped without the test noticing:
// locRIBPeerOldRoutes archives THREE and reports EIGHT, locRIBPeerOldStat
// archives TWO and reports SEVEN.
//
// Both peers' rows are rib = 'loc_rib' only. This fixture is about the time
// bound and nothing else; seedLocRIBMixedRIBFixture already owns the
// rib-filter question and carries the adj-RIB-in rows for it.
func seedLocRIBWindowFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "loc-rib-window-router"
	old := collectionFixtureAnchor.Add(locRIBWindowOffset)
	// The session came up with the routes, 48h ago, and is still current:
	// the archived side scopes to the current session and never to the
	// window, so how old the session is must not matter either.
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterWindow, sysname,
		locRIBPeerOldRoutes, 1, 1, old)

	routes := func(peer, prefix string, n int) {
		for i := range n {
			ts := old.Add(time.Duration(i+1) * time.Second)
			insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
				RouterIP: locRIBRouterWindow, RouterSysname: sysname, PeerIP: peer,
				RIB: "loc_rib", PeerBGPID: peer, Family: "ipv4u",
				Prefix:    fmt.Sprintf("%s.%d/32", prefix, i),
				SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
				TsRouter: ts, TsCollector: ts,
			})
		}
	}
	stat := func(peer string, reported uint64, ts time.Time) {
		insertStatsEvent(t, ctx, q, statsEventFixture{
			RouterIP: locRIBRouterWindow, RouterSysname: sysname, PeerIP: peer,
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: peer,
			SessionID: 1, Seq: 1, StreamSeq: 1,
			Counters:    map[uint32]uint64{8: reported},
			TsRouter:    ts,
			TsCollector: ts,
		})
	}

	routes(locRIBPeerOldRoutes, "10.93.110", 3)
	stat(locRIBPeerOldRoutes, 8, collectionFixtureAnchor)

	routes(locRIBPeerOldStat, "10.93.111", 2)
	stat(locRIBPeerOldStat, 7, old)
}

// locRIBRowsFor runs LocRIBComparison under a caller-supplied filter and
// returns its rows keyed by peer. locRIBRowFor's own contract does not fit
// the window tests: it hard-codes collectionFilterFor, whose Since is the
// zero time, which is the one filter the windowing defect is invisible
// under.
func locRIBRowsFor(t *testing.T, ctx context.Context, q *Q, f CollectionFilter) map[string]PeerLocRIB {
	t.Helper()
	rows, _, err := q.LocRIBComparison(ctx, f)
	if err != nil {
		t.Fatalf("LocRIBComparison(%+v): %v", f, err)
	}
	out := make(map[string]PeerLocRIB, len(rows))
	for _, r := range rows {
		out[r.PeerIP.String()] = r
	}
	return out
}

// TestLocRIBComparisonArchivedSideIgnoresTheWindow pins that defect, held
// as a test.
//
// Reported and Archived are structurally different quantities. Reported is a
// LATEST SNAPSHOT -- one argMax over stats_events. Archived is what
// collection currently HOLDS. Bounding the archived side by f.Since turns it
// into an accumulation over the window and compares that accumulation
// against a snapshot, so a peer whose Loc-RIB is stable -- which is the
// ordinary case, since a router dumps its Loc-RIB at session start and then
// says nothing -- decays toward Archived = 0 as the window narrows while
// Reported holds. On a real lab capture the newest loc_rib row was 15 days
// old, so every window MonitorView offers (1h, 6h, 24h) rendered
// Archived = 0 for every peer against a non-zero Reported: a fabricated gap,
// on the Monitor screen, which must never render that reading.
//
// The test asserts BOTH directions, because only asserting one would leave
// the other free to break silently:
//
//   - locRIBPeerOldRoutes: routes 48h old, stat at the anchor. Under a
//     one-hour window it must still read Archived = 3, identical to the
//     same fixture read with no Since at all, with Reported = 8 -- the
//     stats side answering from inside the window as it always did.
//   - locRIBPeerOldStat: routes AND stat both 48h old. Under the same
//     one-hour window it must read Archived = 2 (the archived side ignores
//     the window) while Reported = 0 and HasStat = false (the stats side
//     does not). Widen the filter to no Since and its stat comes back, at
//     7. That half is what stops this test from passing a mutation that
//     dropped the window from BOTH sides.
func TestLocRIBComparisonArchivedSideIgnoresTheWindow(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	router := netip.MustParseAddr(locRIBRouterWindow)
	// An hour is narrower than every route row this fixture wrote and wider
	// than the stat locRIBPeerOldRoutes wrote at the anchor.
	windowed := CollectionFilter{Router: router, Since: time.Now().Add(-1 * time.Hour)}
	unbounded := CollectionFilter{Router: router}

	got := locRIBRowsFor(t, ctx, q, windowed)
	wide := locRIBRowsFor(t, ctx, q, unbounded)

	stable, ok := got[locRIBPeerOldRoutes]
	if !ok {
		t.Fatalf("a one-hour window dropped %s from the answer entirely; got %+v",
			locRIBPeerOldRoutes, got)
	}
	if stable.Archived != 3 {
		t.Errorf("Archived = %d under a one-hour window, want 3 -- this peer's THREE "+
			"loc_rib rows were written 48h ago and collection still holds every one "+
			"of them; the window scopes the reported side only", stable.Archived)
	}
	if stable.Archived != wide[locRIBPeerOldRoutes].Archived {
		t.Errorf("Archived = %d under a one-hour window but %d with no Since at all -- "+
			"the archived side must answer identically at every window, because it "+
			"reports a stock and not an accumulation",
			stable.Archived, wide[locRIBPeerOldRoutes].Archived)
	}
	if stable.Reported != 8 || !stable.HasStat {
		t.Errorf("Reported = %d, HasStat = %v; want 8 and true -- this peer's stat is "+
			"inside the window and must still be read", stable.Reported, stable.HasStat)
	}

	quiet, ok := got[locRIBPeerOldStat]
	if !ok {
		t.Fatalf("a one-hour window dropped %s from the answer entirely -- its routes "+
			"are outside the window but the archived side does not window, so the "+
			"peer must still appear; got %+v", locRIBPeerOldStat, got)
	}
	if quiet.Archived != 2 {
		t.Errorf("Archived = %d under a one-hour window, want 2 -- this peer's TWO "+
			"loc_rib rows were written 48h ago and are still archived", quiet.Archived)
	}
	if quiet.Reported != 0 || quiet.HasStat {
		t.Errorf("Reported = %d, HasStat = %v; want 0 and false -- this peer's only "+
			"stat is 48h old and a one-hour window must exclude it. The window is "+
			"dropped from the ARCHIVED side only, never from both",
			quiet.Reported, quiet.HasStat)
	}
	if w := wide[locRIBPeerOldStat]; w.Reported != 7 || !w.HasStat {
		t.Errorf("with no Since at all, Reported = %d and HasStat = %v; want 7 and "+
			"true -- the stat this fixture wrote 48h ago exists and an unbounded "+
			"read must find it, which is what makes the windowed assertion above a "+
			"statement about the window rather than about a missing row",
			w.Reported, w.HasStat)
	}
}

// dumpCountFor runs DumpCounts scoped to one router and returns its single
// row. It fails rather than skips on a missing row: every caller has just
// seeded that router, so an empty answer is the query being wrong, not the
// fixture being absent.
func dumpCountFor(t *testing.T, ctx context.Context, q *Q, router string) RouterDumpCount {
	t.Helper()
	rows, _, err := q.DumpCounts(ctx, collectionFilterFor(router))
	if err != nil {
		t.Fatalf("DumpCounts(%s): %v", router, err)
	}
	if len(rows) != 1 {
		t.Fatalf("DumpCounts(%s) returned %d rows, want exactly 1: %+v", router, len(rows), rows)
	}
	return rows[0]
}

// seedRedumpRouter writes, in all THREE route tables, one route observed
// once per session across five sessions -- 15 rows, every one of them the
// first observation of its route inside its own session.
//
// All three tables, not just route_unicast, because "dumps reads all three"
// is a constraint of this signal rather than an implementation detail: a
// DumpCounts that quietly dropped route_vpn or route_evpn from its union
// would still separate re-dump from change on the table it kept, and only a
// fixture that puts rows in all three can see the omission.
func seedRedumpRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	for s := uint64(1); s <= 5; s++ {
		ts := collectionFixtureAnchor.Add(time.Duration(s) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: dumpRouterRedump, RouterSysname: "redump", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.101.0/24",
			SessionID: s, Seq: s, StreamSeq: s,
			TsRouter: ts, TsCollector: ts,
		})
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: dumpRouterRedump, RouterSysname: "redump", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "vpn4", Prefix: "10.93.102.0/24", RD: "65000:1",
			SessionID: s, Seq: s, StreamSeq: s,
			TsRouter: ts, TsCollector: ts,
		})
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterRedump, RouterSysname: "redump", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:01", IP: "10.93.103.1",
			SessionID: s, Seq: s, StreamSeq: s,
			TsRouter: ts, TsCollector: ts,
		})
	}
}

// seedChangeRouter writes, in all three route tables, one route observed
// five times inside ONE session -- 15 rows, of which exactly three (one per
// table) are a first-in-session observation and twelve are genuine
// re-advertisement.
func seedChangeRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	for i := uint64(1); i <= 5; i++ {
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: dumpRouterChange, RouterSysname: "change", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.201.0/24",
			SessionID: 1, Seq: i, StreamSeq: i,
			TsRouter: ts, TsCollector: ts,
		})
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: dumpRouterChange, RouterSysname: "change", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "vpn4", Prefix: "10.93.202.0/24", RD: "65000:1",
			SessionID: 1, Seq: i, StreamSeq: i,
			TsRouter: ts, TsCollector: ts,
		})
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterChange, RouterSysname: "change", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:02", IP: "10.93.203.1",
			SessionID: 1, Seq: i, StreamSeq: i,
			TsRouter: ts, TsCollector: ts,
		})
	}
}

// TestDumpCountsSeparatesRedumpFromChange is the whole point of the signal.
// A population that is entirely session re-dumps and one that is entirely
// genuine re-advertisement must not report the same numbers -- that
// indistinguishability is the defect three dashboard panels shipped with, and
// it is what the per-row classification exists to prevent.
//
// The two routers carry the IDENTICAL number of archived rows (15 each, five
// per route table) and differ only in how those rows are distributed across
// sessions. Anything that counts rows rather than classifying them -- and
// anything that drops session_id from the partition, which makes five
// sessions look like one -- reports the same split for both.
func TestDumpCountsSeparatesRedumpFromChange(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	redump := dumpCountFor(t, ctx, q, dumpRouterRedump)
	change := dumpCountFor(t, ctx, q, dumpRouterChange)

	if redump.Archived != 15 || redump.Dumps != 15 || redump.Changes != 0 {
		t.Errorf("pure-redump router: archived/dumps/changes = %d/%d/%d, want 15/15/0 "+
			"(five sessions x one first-and-only observation, in each of three tables)",
			redump.Archived, redump.Dumps, redump.Changes)
	}
	if change.Archived != 15 || change.Dumps != 3 || change.Changes != 12 {
		t.Errorf("pure-change router: archived/dumps/changes = %d/%d/%d, want 15/3/12 "+
			"(one session x five observations, in each of three tables)",
			change.Archived, change.Dumps, change.Changes)
	}
	if redump.Archived != change.Archived {
		t.Fatalf("the fixture no longer makes the point: the two routers must archive "+
			"the same number of rows (%d vs %d), so that only the classification can "+
			"tell them apart", redump.Archived, change.Archived)
	}
	if redump.Dumps == change.Dumps && redump.Changes == change.Changes {
		t.Errorf("an archive that is entirely session re-dumps and one that is entirely "+
			"genuine re-advertisement reported the same split (%d dumps / %d changes): "+
			"the signal cannot tell them apart", redump.Dumps, redump.Changes)
	}
}

// TestDumpCountsPartsSumToArchived holds the invariant: every row in the
// window is classified as exactly one of the two, so a row that escaped
// classification shows up here and nowhere else.
//
// It runs UNSCOPED, over every router any test in this binary has written to
// any of the three route tables, rather than over this file's own fixtures --
// the invariant is about rows, and the widest population available is the
// one most likely to hold a row some narrower fixture never thought of.
func TestDumpCountsPartsSumToArchived(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	rows, _, err := q.DumpCounts(ctx, CollectionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("unscoped DumpCounts returned no rows at all; the invariant below " +
			"would then hold vacuously")
	}
	var total uint64
	for _, r := range rows {
		if r.Archived != r.Dumps+r.Changes {
			t.Errorf("router %s: archived %d != dumps %d + changes %d -- a row in the "+
				"window was classified as neither, or as both",
				r.RouterIP, r.Archived, r.Dumps, r.Changes)
		}
		total += r.Archived
	}
	if total == 0 {
		t.Fatal("every returned router archived 0 rows; the invariant holds vacuously")
	}
}

// seedWithdrawRouter writes, in all three route tables, one route whose
// FIRST observation inside its session is a withdrawal, followed by an
// advertisement of the same route in the same session.
//
// Neither row may be scored as a dump. The withdrawal is first in the
// session and so wins the `(seq, stream_seq) = min(...)` half of the
// classification outright -- only `is_withdraw = 0` keeps it out -- and the
// advertisement is not first, so it is a change either way.
func seedWithdrawRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	for i, w := range []uint8{1, 0} {
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		seq := uint64(i + 1)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: dumpRouterWithdraw, RouterSysname: "withdraw", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.301.0/24",
			SessionID: 1, Seq: seq, StreamSeq: seq, IsWithdraw: w,
			TsRouter: ts, TsCollector: ts,
		})
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: dumpRouterWithdraw, RouterSysname: "withdraw", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "vpn4", Prefix: "10.93.302.0/24", RD: "65000:1",
			SessionID: 1, Seq: seq, StreamSeq: seq, IsWithdraw: w,
			TsRouter: ts, TsCollector: ts,
		})
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterWithdraw, RouterSysname: "withdraw", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:03", IP: "10.93.303.1",
			SessionID: 1, Seq: seq, StreamSeq: seq, IsWithdraw: w,
			TsRouter: ts, TsCollector: ts,
		})
	}
}

// TestDumpCountsNeverClassifiesAWithdrawalAsADump. Nothing synthesizes a
// withdrawal, so it can never be an initial dump. route_evpn carries 0
// first-in-session withdrawals of 246 in a real captured archive, which
// means a symmetric rule LOOKS correct there -- this guard is structural,
// and the fixture must contain a first-in-session withdrawal so it has
// something to hold.
func TestDumpCountsNeverClassifiesAWithdrawalAsADump(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := dumpCountFor(t, ctx, q, dumpRouterWithdraw)
	if got.Archived != 6 {
		t.Fatalf("fixture drift: archived %d, want 6 (two rows in each of three tables)",
			got.Archived)
	}
	if got.Dumps != 0 || got.Changes != 6 {
		t.Errorf("a route whose session OPENS with a withdrawal: dumps/changes = %d/%d, "+
			"want 0/6. The withdrawal is the first observation of its route in its "+
			"session and so satisfies the ordering half of the classification; only "+
			"`is_withdraw = 0` keeps it from being reported as a router re-dumping a "+
			"route it never sent", got.Dumps, got.Changes)
	}
}

// seedIdentityRouters writes the two pairs that separate each table's own
// route identity from a shared one.
//
//   - dumpRouterVPNIdentity: two route_vpn rows identical in every identity
//     column except rd. Two routes, so two first-in-session dumps.
//   - dumpRouterEVPNIdentity: two route_evpn type-2 rows identical in every
//     identity column except ip. Likewise two routes, two dumps.
//
// Each pair sits in ONE session with two distinct (seq, stream_seq) values,
// which is what makes the wrong answer visible: merged into one partition,
// only the lower (seq, stream_seq) is the minimum, so one real dump is
// reported as a change.
func seedIdentityRouters(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	for i, rd := range []string{"65000:1", "65000:2"} {
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: dumpRouterVPNIdentity, RouterSysname: "vpn-identity", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "vpn4", Prefix: "10.93.401.0/24", RD: rd,
			SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
			TsRouter: ts, TsCollector: ts,
		})
	}
	for i, ip := range []string{"10.93.501.1", "10.93.501.2"} {
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterEVPNIdentity, RouterSysname: "evpn-identity", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:05", IP: ip,
			SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
			TsRouter: ts, TsCollector: ts,
		})
	}
}

// TestDumpCountsUsesEachTablesOwnIdentity pins this defect, as a test.
// Two route_vpn rows differing ONLY in rd are two routes; two route_evpn
// type-2 rows differing ONLY in ip are two routes. A shared identity tuple
// merges them and undercounts changes.
//
// The dumps cost measurement (DumpCostMeasurement) found this class of
// failure on a purpose-built 20,000-row population per table: dropping rd,
// or dropping ip,
// misreports exactly 100 real dumps as changes. This is the same failure at
// a size that can be checked by hand -- one stolen dump per pair.
func TestDumpCountsUsesEachTablesOwnIdentity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	vpn := dumpCountFor(t, ctx, q, dumpRouterVPNIdentity)
	if vpn.Archived != 2 || vpn.Dumps != 2 || vpn.Changes != 0 {
		t.Errorf("two route_vpn rows differing only in rd: archived/dumps/changes = "+
			"%d/%d/%d, want 2/2/0. They are two routes in two VRFs, each seen once in "+
			"its session; an identity without rd merges them and reports the second as "+
			"a re-advertisement of the first", vpn.Archived, vpn.Dumps, vpn.Changes)
	}

	evpn := dumpCountFor(t, ctx, q, dumpRouterEVPNIdentity)
	if evpn.Archived != 2 || evpn.Dumps != 2 || evpn.Changes != 0 {
		t.Errorf("two route_evpn type-2 rows differing only in ip: archived/dumps/changes "+
			"= %d/%d/%d, want 2/2/0. A MAC advertised with two host addresses is two "+
			"EVPN routes; an identity without ip merges them",
			evpn.Archived, evpn.Dumps, evpn.Changes)
	}
}

// ---------------------------------------------------------------------------
// Parity with the dashboards
// ---------------------------------------------------------------------------

// dashboardPanelSQL returns the rawSql of one target in one committed
// dashboard under deploy/grafana/dashboards, identified by panel title and
// refId -- sink/dashboard_sql_test.go's panelSQL, in this package and for the
// same stated reason: the dashboard is what an operator opens, and a test
// that agrees with a retyped copy of it proves nothing about what they see.
//
// It fails rather than skips when the panel is missing. A renamed panel that
// silently stopped being compared is exactly the drift this file exists to
// catch.
func dashboardPanelSQL(t *testing.T, dashboard, panelTitle, refID string) string {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				RefID  string `json:"refId"`
				RawSQL string `json:"rawSql"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, p := range d.Panels {
		if p.Title != panelTitle {
			continue
		}
		for _, tg := range p.Targets {
			if tg.RefID == refID {
				if strings.TrimSpace(tg.RawSQL) == "" {
					t.Fatalf("%s panel %q target %q has empty rawSql", path, panelTitle, refID)
				}
				return tg.RawSQL
			}
		}
		t.Fatalf("%s panel %q has no target with refId %q", path, panelTitle, refID)
	}
	t.Fatalf("%s has no panel titled %q", path, panelTitle)
	return ""
}

var (
	dashTimeFilterRe    = regexp.MustCompile(`\$__timeFilter\(([^)]*)\)`)
	dashTimeIntervalRe  = regexp.MustCompile(`\$__timeInterval\(([^)]*)\)`)
	dashTemplateVarRe   = regexp.MustCompile(`\$(rib|route_type)\b`)
	dashDatabasePrefixQ = regexp.MustCompile(`\bvantage\.`)
	// dashLeftoverVarRe matches a Grafana reference Grafana would have
	// rendered -- $name, ${name...} or a $__macro -- and deliberately not a
	// lone trailing $, which these panels use as a regex end anchor.
	dashLeftoverVarRe = regexp.MustCompile(`\$[A-Za-z_{]`)
)

// renderDashboardSQL performs the substitutions Grafana would perform before
// the ClickHouse plugin sends the query, and only those:
//
//   - $__timeInterval(col) becomes a real bucket expression. Its width does
//     not matter here; the caller sums every bucket.
//   - $__timeFilter(col) becomes a window around now() wide enough to contain
//     collectionFixtureAnchor, AND the router scope. The router predicate
//     goes here because this is the panel's only insertion point inside the
//     classification subquery, and a query/ test writes into a database every
//     other test in this package is also writing to -- a panel that counted
//     every router in testDB would be compared against a DumpCounts answer
//     scoped to one. Grafana's own $router variable does the identical thing
//     on the dashboards that have one.
//   - $rib and $route_type become `.*`, which is what a Grafana multi-value
//     "All" renders to inside these panels' `match(toString(col), '^($var)$')`.
//   - the hard-coded `vantage.` database prefix becomes this package's test
//     database. That is the one edit Grafana would NOT make, and it is
//     unavoidable: the committed SQL names the production database.
func renderDashboardSQL(t *testing.T, sql, db, router string) string {
	t.Helper()
	sql = dashTimeIntervalRe.ReplaceAllString(sql, "toStartOfInterval($1, INTERVAL 1 MINUTE)")
	sql = dashTimeFilterRe.ReplaceAllString(sql,
		"$1 >= now() - INTERVAL 1 DAY AND $1 <= now() + INTERVAL 1 DAY "+
			"AND router_ip = toIPv6('"+router+"')")
	sql = dashTemplateVarRe.ReplaceAllString(sql, ".*")
	sql = dashDatabasePrefixQ.ReplaceAllString(sql, db+".")
	// Only a $ that STARTS something counts as unrendered: a bare trailing $
	// is a regular-expression end anchor, which is what `'^($rib)$'` leaves
	// behind once $rib itself is substituted.
	if m := dashLeftoverVarRe.FindString(sql); m != "" {
		t.Fatalf("unrendered Grafana variable %q left in the panel SQL; add it to "+
			"renderDashboardSQL rather than working around it in the dashboard:\n%s", m, sql)
	}
	return sql
}

// dashboardChurn runs one rendered churn panel and folds its per-bucket rows
// into the three numbers DumpCounts reports.
//
// The mapping is exact and is the whole content of the parity claim. Both
// panels select `countIf(is_session_dump)`, `countIf(NOT is_session_dump AND
// is_withdraw = 0)` and `countIf(is_withdraw = 1)`, which are mutually
// exclusive and exhaustive (a session dump is `is_withdraw = 0` by
// construction). So the panel's session_dump IS DumpCounts' Dumps, and its
// readvertise plus withdraw IS DumpCounts' Changes.
func dashboardChurn(t *testing.T, ctx context.Context, q *Q, sql string) (archived, dumps, changes uint64) {
	t.Helper()
	rows, err := q.conn.Query(ctx, sql)
	if err != nil {
		t.Fatalf("run dashboard SQL: %v\n%s", err, sql)
	}
	defer rows.Close()
	for rows.Next() {
		var bucket time.Time
		var readvertise, withdraw, sessionDump uint64
		if err := rows.Scan(&bucket, &readvertise, &withdraw, &sessionDump); err != nil {
			t.Fatalf("scan dashboard row: %v", err)
		}
		dumps += sessionDump
		changes += readvertise + withdraw
		archived += readvertise + withdraw + sessionDump
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dashboard rows: %v", err)
	}
	return archived, dumps, changes
}

// seedParityRouters writes one router per parity-checked table, each carrying
// rows in that table ALONE so that DumpCounts' three-table sum can be
// compared against a single dashboard panel's answer.
//
// Each population deliberately mixes all three outcomes the classification
// can produce -- a multi-session re-dump, an in-session flap, and a
// first-in-session withdrawal -- so the comparison is not satisfied by two
// queries that both happen to return zero for one of them.
func seedParityRouters(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	// route_unicast only.
	for s := uint64(1); s <= 3; s++ { // re-dump across three sessions
		ts := collectionFixtureAnchor.Add(time.Duration(s) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: dumpRouterParityUnicast, RouterSysname: "parity-u", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.601.0/24",
			SessionID: s, Seq: s, StreamSeq: s, TsRouter: ts, TsCollector: ts,
		})
	}
	for i := uint64(1); i <= 4; i++ { // flap inside one session
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: dumpRouterParityUnicast, RouterSysname: "parity-u", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.602.0/24",
			SessionID: 9, Seq: i, StreamSeq: i, TsRouter: ts, TsCollector: ts,
		})
	}
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{ // opens with a withdrawal
		RouterIP: dumpRouterParityUnicast, RouterSysname: "parity-u", PeerIP: dumpPeer,
		RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.603.0/24",
		SessionID: 9, Seq: 1, StreamSeq: 1, IsWithdraw: 1,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
	// Two ADD-PATH siblings: one prefix, two path_id values, two routes. No
	// other fixture in this file sets PathID, so this pair is the only thing
	// in the package that can see path_id leave either identity -- which is
	// what makes it the shape a Go-side-only drift shows up in here and
	// nowhere else.
	for _, pathID := range []uint32{1, 2} {
		ts := collectionFixtureAnchor.Add(time.Duration(4+pathID) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: dumpRouterParityUnicast, RouterSysname: "parity-u", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, Family: "ipv4u", Prefix: "10.93.604.0/24",
			PathID: pathID, SessionID: 9, Seq: uint64(4 + pathID), StreamSeq: uint64(4 + pathID),
			TsRouter: ts, TsCollector: ts,
		})
	}

	// route_evpn only.
	for s := uint64(1); s <= 3; s++ {
		ts := collectionFixtureAnchor.Add(time.Duration(s) * time.Second)
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterParityEVPN, RouterSysname: "parity-e", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:07", IP: "10.93.701.1",
			SessionID: s, Seq: s, StreamSeq: s, TsRouter: ts, TsCollector: ts,
		})
	}
	for i := uint64(1); i <= 4; i++ {
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterParityEVPN, RouterSysname: "parity-e", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:07", IP: "10.93.701.2",
			SessionID: 9, Seq: i, StreamSeq: i, TsRouter: ts, TsCollector: ts,
		})
	}
	insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
		RouterIP: dumpRouterParityEVPN, RouterSysname: "parity-e", PeerIP: dumpPeer,
		RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
		MAC: "00:00:00:00:93:07", IP: "10.93.701.3",
		SessionID: 9, Seq: 1, StreamSeq: 1, IsWithdraw: 1,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
	// The EVPN half of the ADD-PATH pair above, for the same reason.
	for _, pathID := range []uint32{1, 2} {
		ts := collectionFixtureAnchor.Add(time.Duration(4+pathID) * time.Second)
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: dumpRouterParityEVPN, RouterSysname: "parity-e", PeerIP: dumpPeer,
			RIB: "in_pre", PeerBGPID: dumpPeer, RouteType: 2, RD: "65000:1",
			MAC: "00:00:00:00:93:07", IP: "10.93.701.4", PathID: pathID,
			SessionID: 9, Seq: uint64(4 + pathID), StreamSeq: uint64(4 + pathID),
			TsRouter: ts, TsCollector: ts,
		})
	}
}

// TestDumpCountsAgreesWithTheDashboards pins the Go classification against the
// SQL route-churn.json ships, on one fixture. query/'s two-phase RIB reader is
// parity-checked the same way and for the same reason.
//
// evpn-churn.json is checked too, because the classification exists there as
// well and route_evpn's identity is the one a shared tuple damages most. Only
// TWO of the three tables can be checked this way: no shipped dashboard
// carries the VPN classification at all (top-l3vpn-prefixes declines to claim
// churn), which the dumps cost measurement (DumpCostMeasurement) records as
// a limit of its own.
// route_vpn's rendering is therefore held by
// TestDumpCountsUsesEachTablesOwnIdentity and the invariant test, not by
// parity.
//
// One difference between the two sides is deliberate and is NOT drift: the
// dashboards read `FROM ... FINAL` and this package reads no FINAL anywhere
// (see the package doc comment). On a table carrying an unmerged duplicate
// the two therefore CAN disagree, by exactly that duplicate. This fixture
// writes no duplicate rows, so the comparison below is of the classification
// and nothing else -- which is what the test is for.
func TestDumpCountsAgreesWithTheDashboards(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	for _, tc := range []struct {
		name      string
		dashboard string
		panel     string
		router    string
	}{
		{"unicast", "route-churn", "Re-advertisements, session dumps and withdrawals", dumpRouterParityUnicast},
		{"evpn", "evpn-churn", "Re-advertisements, session dumps and withdrawals", dumpRouterParityEVPN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := renderDashboardSQL(t, dashboardPanelSQL(t, tc.dashboard, tc.panel, "A"), q.db, tc.router)
			wantArchived, wantDumps, wantChanges := dashboardChurn(t, ctx, q, sql)
			if wantArchived == 0 {
				t.Fatalf("%s's panel returned nothing for %s; the comparison below "+
					"would hold vacuously", tc.dashboard, tc.router)
			}
			if wantDumps == 0 || wantChanges == 0 {
				t.Fatalf("%s's panel returned %d dumps / %d changes for %s; the fixture "+
					"must exercise both sides of the classification, or a query that "+
					"always answers one of them passes", tc.dashboard, wantDumps, wantChanges, tc.router)
			}
			got := dumpCountFor(t, ctx, q, tc.router)
			if got.Archived != wantArchived || got.Dumps != wantDumps || got.Changes != wantChanges {
				t.Errorf("DumpCounts and %s.json disagree about what a dump is for %s:\n"+
					"  Go        archived/dumps/changes = %d/%d/%d\n"+
					"  dashboard archived/dumps/changes = %d/%d/%d\n"+
					"Grafana and the API would both look right while reporting different "+
					"numbers for the same archive.\n%s",
					tc.dashboard, tc.router,
					got.Archived, got.Dumps, got.Changes,
					wantArchived, wantDumps, wantChanges, sql)
			}
		})
	}
}

// TestDumpCountsCitesAMeasurementThatExists guards the citation
// DumpCostMeasurement names, the way api/'s
// TestCitedUnscopedEventsMeasurementExists guards
// query.UnscopedEventsMeasurement: the three-identity constraint's
// whole justification is a measurement, and a citation pointing at a moved
// file or a renamed section silently stops being a citation.
func TestDumpCountsCitesAMeasurementThatExists(t *testing.T) {
	requireCitedSection(t, "DumpCostMeasurement", DumpCostMeasurement)
}

// TestCollectionCitesACostMeasurementThatExists is the same guard for the
// rig that measured the two signals SessionCounts and FlagCounts serve,
// documented in docs/measurements.md. Separate from the dumps one so a
// failure names which citation broke.
func TestCollectionCitesACostMeasurementThatExists(t *testing.T) {
	requireCitedSection(t, "SessionsFlagsCostMeasurement", SessionsFlagsCostMeasurement)
}

// requireCitedSection fails unless citation -- a repo-relative path, a '#',
// and a section anchor -- names a file that exists AND a heading in it whose
// Markdown anchor is that fragment. The file check alone would stay green
// while a renamed heading left every quoted link landing at the top of the
// page. api/events_test.go carries the same check for its own citation.
func requireCitedSection(t *testing.T, name, citation string) {
	t.Helper()
	file, anchor, ok := strings.Cut(citation, "#")
	if !ok || anchor == "" {
		t.Fatalf("%s = %q has no #anchor naming a section", name, citation)
	}
	body, err := os.ReadFile(filepath.Join("..", file))
	if err != nil {
		t.Fatalf("%s names %s, which does not exist: %v", name, file, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		heading, isHeading := strings.CutPrefix(line, "## ")
		if isHeading && markdownAnchor(heading) == anchor {
			return
		}
	}
	t.Fatalf("%s names section #%s, but %s has no \"## \" heading with that anchor",
		name, anchor, file)
}

// markdownAnchor is the fragment GitHub generates for a heading: lowercased,
// punctuation other than '-' and '_' dropped, spaces turned into hyphens.
func markdownAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestDumpCountsIdentitiesMatchTheRIBStatements is the structural half of the
// three-identity constraint, and it is what a fixture cannot give: every
// column of each table's route identity, as this file renders it into the
// PARTITION BY, compared against the GROUP BY of the statement that OWNS that
// table's route key -- routesSQL's unicast key, vpnRoutesSQL's, and
// evpnRoutesSQL's.
//
// A fixture proves a particular pair of routes is told apart. This proves the
// whole tuple, which is the thing every caller inherits. The comparison
// is made against the real statement text rather than a second hand-written
// list, so a column added to a route key and forgotten here fails loudly.
func TestDumpCountsIdentitiesMatchTheRIBStatements(t *testing.T) {
	for _, tc := range []struct {
		what     string
		identity string
		stmt     string
	}{
		{"route_unicast", unicastRouteIdentity, unicastRIBKeysSQL},
		{"route_vpn", vpnRouteIdentity, vpnRoutesSQL},
		{"route_evpn", evpnRouteIdentity, evpnRoutesSQL},
	} {
		want := ribKeyColumns(t, tc.stmt)
		got := strings.Split(strings.ReplaceAll(tc.identity, " ", ""), ",")
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s identity drifted from the statement that owns its route key:\n"+
				"  collection.go GROUP BY partition: %v\n"+
				"  route statement GROUP BY:         %v", tc.what, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// SessionCounts
// ---------------------------------------------------------------------------

// sessionCountsRouterA and sessionCountsRouterB are this file's own two
// routers for SessionCounts, on 10.96.0.0/16 -- unused elsewhere in
// query/, distinct from the DumpCounts fixtures above (10.93.x.x) and from
// every other router IP any other file in this package claims.
const (
	// sessionCountsRouterA carries four 'up' events, three 'down' events and
	// two 'view_lost' events, each under its own peer and session -- nine
	// sessions in all. The four resulting numbers (Sessions=9, Up=4, Down=3,
	// ViewLost=2) are chosen PAIRWISE DISTINCT on purpose: an untested,
	// implicit Up=0 would catch an up/down swap mutation only by coincidence
	// -- the swap changed Down to 0, which differed from its expected 3, but
	// nothing would have noticed had Up and Down's fixture values ever
	// matched. "Structurally identical to something that is tested" is
	// exactly the reasoning that can let an up/down label swap go undetected
	// until every kind has its own assertion. With all four values distinct
	// here, no mutation that moves what one column counts into another can
	// pass: the swapped column's new value is always one of the OTHER three's
	// expecteds, never its own.
	sessionCountsRouterA = "10.96.1.1"
	// sessionCountsRouterB carries ONE session_id that emits five
	// peer_events rows across four peers -- three come up once, one flaps
	// up/down/up. The point here is that a session is counted once no
	// matter how many events it produced.
	sessionCountsRouterB = "10.96.2.1"
	// sessionCountsRouterDual is watched by TWO collectors, and is the only
	// router here that can tell a per-collector answer from a summed one.
	// Routers A and B carry one collector each, so every assertion about
	// them passes whether the statement groups by collector or not -- the
	// one-branch fixture problem, which hid two defects until a second
	// collector existed to see them.
	//
	// The split is deliberately LOPSIDED and deliberately crossed. The
	// healthy collector heard more from the router (6 statements to 2) and
	// so wins the vantage-point choice, while EVERY view_lost row belongs
	// to the other one -- which also holds MORE rows in total, 7 to 6, so
	// ranking the choice on every event rather than on router statements
	// alone picks the wrong winner and is caught. So "best vantage point", "sum" and "first collector"
	// are three different answers in every column, and a view_lost that
	// rode along with the chosen collector would read 0 where the truth is
	// 3 -- see TestSessionCountsKeepsEveryCollectorsLostView.
	sessionCountsRouterDual = "10.96.3.1"
	// sessionCountsBlindCollector is the collector that lost its view. It
	// sorts AFTER defaultFixtureCollector ("query-test"), so a tie-break on
	// collector_id would pick it -- which is what stops the healthy
	// collector from winning this fixture by accident of naming.
	sessionCountsBlindCollector = "query-test-blind"
	// sessionCountsRouterTie is watched by two collectors that heard the
	// SAME NUMBER of router statements and disagree about everything else.
	// It is the only fixture that can judge the (router_events,
	// collector_id) tie-break: with a bare argMax on router_events,
	// ClickHouse is free to resolve each column's tie independently, so one
	// row could carry one collector's sessions beside the other's ups --
	// the mixed vantage point churnByPeerSQL's tuple exists to prevent.
	//
	// It is not hypothetical. A real dual-homed router was exactly this:
	// 3 ups from each of two collectors.
	sessionCountsRouterTie = "10.96.4.1"
	// sessionCountsTieCollector sorts AFTER defaultFixtureCollector, so the
	// documented tie-break picks it and the assertion names a specific
	// winner rather than "either one".
	sessionCountsTieCollector = "query-test-tie"
)

// seedSessionCountsFixtures writes SessionCounts' own population: the
// view-lost/down split on sessionCountsRouterA and the session/event split
// on sessionCountsRouterB.
//
// It writes peer_events directly and touches no route table, unlike every
// other seed function in this file -- SessionCounts reads peer_events alone.
func seedSessionCountsFixtures(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "session-counts-router"

	// sessionCountsRouterA: 4 up, 3 down, 2 view_lost -- 9 sessions total,
	// each its own peer and session_id, so Sessions/Up/Down/ViewLost come
	// out to 9/4/3/2: four different numbers. See the constant's own doc
	// comment for why that is load-bearing rather than tidy. Session
	// numbering carries no meaning beyond staying distinct from every other
	// session_id this router's peers use.
	upPeers := []string{"10.96.1.7", "10.96.1.8", "10.96.1.9", "10.96.1.10"}
	for i, p := range upPeers {
		ts := collectionFixtureAnchor.Add(time.Duration(20+i) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: sessionCountsRouterA, RouterSysname: sysname, PeerIP: p,
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: p,
			SessionID: uint64(20 + i), Seq: 1, StreamSeq: uint64(20 + i),
			Kind: "up", TsRouter: ts, TsCollector: ts,
		})
	}
	downPeers := []string{"10.96.1.2", "10.96.1.3", "10.96.1.4"}
	for i, p := range downPeers {
		ts := collectionFixtureAnchor.Add(time.Duration(i+1) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: sessionCountsRouterA, RouterSysname: sysname, PeerIP: p,
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: p,
			SessionID: uint64(i + 1), Seq: 1, StreamSeq: uint64(i + 1),
			Kind: "down", TsRouter: ts, TsCollector: ts,
		})
	}
	viewLostPeers := []string{"10.96.1.5", "10.96.1.6"}
	for i, p := range viewLostPeers {
		ts := collectionFixtureAnchor.Add(time.Duration(10+i) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: sessionCountsRouterA, RouterSysname: sysname, PeerIP: p,
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: p,
			SessionID: uint64(10 + i), Seq: 1, StreamSeq: uint64(10 + i),
			Kind: "view_lost", TsRouter: ts, TsCollector: ts,
		})
	}

	// sessionCountsRouterB: one session_id (777), five peer_events rows
	// across four peers. uniqExact(session_id) must read 1 here; count()
	// reads 5.
	const oneSession = 777
	events := []struct {
		peerIP string
		kind   string
		seq    uint64
	}{
		{"10.96.2.2", "up", 1},
		{"10.96.2.3", "up", 1},
		{"10.96.2.4", "up", 1},
		{"10.96.2.2", "down", 2},
		{"10.96.2.2", "up", 3},
	}
	for i, e := range events {
		ts := collectionFixtureAnchor.Add(time.Duration(20+i) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: sessionCountsRouterB, RouterSysname: sysname, PeerIP: e.peerIP,
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: e.peerIP,
			SessionID: oneSession, Seq: e.seq, StreamSeq: uint64(20 + i),
			Kind: e.kind, TsRouter: ts, TsCollector: ts,
		})
	}
}

// seedSessionCountsDualHomed writes one router watched by two collectors.
//
// The healthy collector holds 2 sessions, 5 up and 1 down -- six router
// statements. The blind one holds 1 session, 2 up and 3 view_lost: two
// router statements and three admissions that it stopped being able to see
// this router at all.
//
// Those counts make all three candidate answers distinct. Summed:
// 3 sessions / 7 up / 1 down. Best vantage point: 2 / 5 / 1. First
// collector alone would be the same as best vantage here, which is why the
// blind collector is named to WIN a collector_id tie-break -- with the
// choice made on router statements it must still lose, 6 to 2.
func seedSessionCountsDualHomed(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "session-counts-dual"
	streamSeq := uint64(3000)
	ev := func(collector, peer, kind string, session uint64) {
		streamSeq++
		ts := collectionFixtureAnchor.Add(time.Duration(streamSeq-3000) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: sessionCountsRouterDual, RouterSysname: sysname, PeerIP: peer,
			RIB: "in_pre", Collector: collector, PeerASN: 65000, PeerBGPID: peer,
			SessionID: session, Seq: 1, StreamSeq: streamSeq,
			Kind: kind, TsRouter: ts, TsCollector: ts,
		})
	}
	// The healthy collector: two sessions, five ups and one down.
	for i, peer := range []string{"10.96.3.2", "10.96.3.3", "10.96.3.4"} {
		ev(defaultFixtureCollector, peer, "up", 3001)
		if i == 0 {
			ev(defaultFixtureCollector, peer, "down", 3001)
		}
	}
	ev(defaultFixtureCollector, "10.96.3.2", "up", 3002)
	ev(defaultFixtureCollector, "10.96.3.3", "up", 3002)
	// The blind collector: one session, two ups, and FIVE lost views. Its
	// view_lost rows are the ONLY ones under this router.
	//
	// Five and not three, which is what makes the RANKING falsifiable as
	// well as the merge. With three it holds 5 events to the healthy
	// collector's 6, so ranking the vantage-point choice on every event
	// instead of on router statements alone picked the same winner and no
	// test moved -- a surviving mutation, found by running it. With five it
	// holds 7 to 6 and wins that mutation outright, which drags Sessions,
	// Up and Down onto the blind collector's row and fails
	// TestSessionCountsReportOneCollectorsViewNotTheSumOfBoth by name.
	//
	// Two peers lose their view twice. Nothing about view_lost is
	// once-per-peer: it is Session.Close writing down that this collector
	// stopped being able to see the peer, which can happen as often as the
	// transport drops.
	ev(sessionCountsBlindCollector, "10.96.3.2", "up", 3003)
	ev(sessionCountsBlindCollector, "10.96.3.3", "up", 3003)
	for _, peer := range []string{"10.96.3.2", "10.96.3.3", "10.96.3.4", "10.96.3.2", "10.96.3.3"} {
		ev(sessionCountsBlindCollector, peer, "view_lost", 3003)
	}
}

// seedSessionCountsTie writes one router whose two collectors heard the
// same number of router statements and agree on nothing else.
//
// Four statements each: the first collector 3 up + 1 down over 2 sessions,
// the second 4 up + 0 down over 1. Every column differs, so a tie resolved
// per column rather than per row produces a triple belonging to neither.
func seedSessionCountsTie(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "session-counts-tie"
	streamSeq := uint64(4000)
	ev := func(collector, peer, kind string, session uint64) {
		streamSeq++
		ts := collectionFixtureAnchor.Add(time.Duration(streamSeq-4000) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: sessionCountsRouterTie, RouterSysname: sysname, PeerIP: peer,
			RIB: "in_pre", Collector: collector, PeerASN: 65000, PeerBGPID: peer,
			SessionID: session, Seq: 1, StreamSeq: streamSeq,
			Kind: kind, TsRouter: ts, TsCollector: ts,
		})
	}
	ev(defaultFixtureCollector, "10.96.4.2", "up", 4001)
	ev(defaultFixtureCollector, "10.96.4.3", "up", 4001)
	ev(defaultFixtureCollector, "10.96.4.2", "down", 4001)
	ev(defaultFixtureCollector, "10.96.4.2", "up", 4002)
	ev(sessionCountsTieCollector, "10.96.4.2", "up", 4003)
	ev(sessionCountsTieCollector, "10.96.4.3", "up", 4003)
	ev(sessionCountsTieCollector, "10.96.4.4", "up", 4003)
	ev(sessionCountsTieCollector, "10.96.4.5", "up", 4003)
}

// sessionCountFor runs SessionCounts scoped to one router and returns its
// single row -- dumpCountFor's own contract, for the same reason: every
// caller has just seeded that router, so an empty answer is the query being
// wrong, not the fixture being absent.
func sessionCountFor(t *testing.T, ctx context.Context, q *Q, router string) RouterSessionCount {
	t.Helper()
	rows, _, err := q.SessionCounts(ctx, collectionFilterFor(router))
	if err != nil {
		t.Fatalf("SessionCounts(%s): %v", router, err)
	}
	if len(rows) != 1 {
		t.Fatalf("SessionCounts(%s) returned %d rows, want exactly 1: %+v", router, len(rows), rows)
	}
	return rows[0]
}

// TestSessionCountsKeepsViewLostApartFromDown. view_lost is the collector
// losing sight of a peer while the router said nothing; down is the router
// reporting a session ended. Folding them loses the distinction three
// screens already hold, and misreports our blindness as the network's
// failure.
//
// All four fields are asserted, and the fixture's four expected values
// (Sessions=9, Up=4, Down=3, ViewLost=2) are pairwise distinct -- see
// sessionCountsRouterA's own doc comment for why an untested column, or two
// columns that happen to share a fixture value, can let a mutation that
// swaps what two SELECT expressions count pass by coincidence rather than
// by having been checked. Up is asserted here rather than in a test of its
// own because it is this same router's own column, seeded by this same
// fixture, and a second router would only add a second sync.Once population
// to reason about for no gain in coverage.
func TestSessionCountsKeepsViewLostApartFromDown(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := sessionCountFor(t, ctx, q, sessionCountsRouterA)
	if got.Sessions != 9 {
		t.Errorf("Sessions = %d, want 9 (4 up + 3 down + 2 view_lost sessions, "+
			"each its own session_id)", got.Sessions)
	}
	if got.Up != 4 {
		t.Errorf("Up = %d, want 4 -- the router reporting four peers coming up",
			got.Up)
	}
	if got.Down != 3 {
		t.Errorf("Down = %d, want 3 -- the router reporting three BGP sessions "+
			"ended, on the wire, with a reason code", got.Down)
	}
	if got.ViewLost != 2 {
		t.Errorf("ViewLost = %d, want 2 -- the collector losing its own BMP "+
			"transport to the router twice, with the router saying nothing at "+
			"all", got.ViewLost)
	}
}

// TestSessionCountsCountsSessionsNotEvents. A session that emits many peer
// events is one session; uniqExact(session_id) and count() are different
// numbers and only the first answers "how often did this router reset".
func TestSessionCountsCountsSessionsNotEvents(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := sessionCountFor(t, ctx, q, sessionCountsRouterB)
	if got.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1 -- one BMP session emitted five "+
			"peer_events rows across four peers; uniqExact(session_id) must "+
			"read 1, not the event count", got.Sessions)
	}
}

// ribKeyColumns extracts the route-key columns from one route statement's
// GROUP BY: the `r.`-qualified column names between GROUP BY and the HAVING
// that follows it.
func ribKeyColumns(t *testing.T, stmt string) []string {
	t.Helper()
	i := strings.LastIndex(stmt, "GROUP BY ")
	if i < 0 {
		t.Fatalf("statement has no GROUP BY:\n%s", stmt)
	}
	rest := stmt[i+len("GROUP BY "):]
	if j := strings.Index(rest, "HAVING"); j >= 0 {
		rest = rest[:j]
	}
	var out []string
	for _, f := range strings.Split(rest, ",") {
		f = strings.TrimSpace(f)
		f = strings.TrimPrefix(f, "r.")
		if f == "" {
			t.Fatalf("empty column in GROUP BY:\n%s", rest)
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------------------
// FlagCounts
// ---------------------------------------------------------------------------

// flagAllTables is EVERY table this package believes carries a parse_flags
// column, spelled out HERE rather than read from query.go's own
// flagStreamTables -- deliberately, and that independence is the whole
// point. seedFlagCountsFixtures and TestFlagCountsReadsEveryFlaggedTable
// both walk this list, not flagStreamTables, so that a table silently
// dropped from flagStreamTables (production's own list of what
// flagCountsSQL's union actually reads) is still: (a) seeded with a flagged
// row, because the fixture no longer takes its instructions from the very
// list the mutation would have edited, and (b) still expected in the answer,
// for the same reason. Walking flagStreamTables for both halves instead
// passes against a NINE-table union with ls_prefixes removed from both the
// SQL and this list at once -- the fixture quietly stops writing a row for
// the table the test also quietly stops checking, and nothing fails. This
// list is what prevents that: it is the independent, by-hand ground truth
// flagStreamTables is compared against, not a second copy of the same
// variable.
var flagAllTables = []string{
	"route_unicast", "route_vpn", "route_evpn", "eor_events",
	"ls_events", "ls_nodes", "ls_links", "ls_prefixes",
	"stats_events", "peer_events",
}

// Routers this section reserves, on 10.94.0.0/16 -- unused elsewhere in
// query/ (see collectionFilterFor's own siblings above for the ranges
// already claimed: 10.93.x.x, 10.96.x.x, 10.95.x.x, 10.99.x.x).
const (
	// flagRouterEveryTable carries one row in EACH of flagAllTables --
	// see seedFlagCountsFixtures.
	flagRouterEveryTable = "10.94.1.1"
	flagPeerEveryTable   = "10.94.0.2"
	// flagRouterEnvelopes carries five route_unicast rows sharing one
	// envelope -- TestFlagCountsCountsEnvelopesNotRows' own router.
	flagRouterEnvelopes = "10.94.2.1"
	flagPeerEnvelopes   = "10.94.0.3"
	// flagRouterEorDup carries one eor_events row inserted twice,
	// byte-identical -- TestFlagCountsReadsEorEventsWithoutFinal's own
	// router.
	flagRouterEorDup = "10.94.3.1"
	flagPeerEorDup   = "10.94.0.4"
)

// flagEnvelopesTestFlag and flagEorDupTestFlag are literals no real decoder
// in bgp/ ever raises. That is load-bearing, not cosmetic: testDB is shared
// and never truncated (see collectionFixturesOnce), so an assertion that a
// specific flag comes back with a specific Envelopes count must not be
// answerable by a real PARSE_FLAG_* row some other fixture, or a stray
// capture, happens to also carry.
const (
	flagEnvelopesTestFlag = "TEST_FLAG_ENVELOPES_NOT_ROWS"
	flagEorDupTestFlag    = "TEST_FLAG_EOR_DUP"
)

// flagForTable is the per-table literal seedFlagCountsFixtures raises in
// EACH of the ten tables for TestFlagCountsReadsEveryFlaggedTable, unique
// per table for the same reason flagEnvelopesTestFlag is unique to its own
// test: two tables sharing one flag would let a query that merged their
// envelope counts, or dropped one of the two, pass by coincidence.
func flagForTable(table string) string {
	return "TEST_FLAG_" + strings.ToUpper(table)
}

// flagFixtureColumns is the column list insertParseFlagRow writes through:
// the envelope columns every one of the ten tables flagStreamTables names
// shares verbatim (see schema.sql's own header, "every table ... carries
// the envelope columns"). Naming only these lets one helper write a
// throwaway flagged row into any of the ten without this file re-deriving
// each table's own, much longer and much more different, table-specific
// schema -- ls_nodes and peer_events, say, agree on nothing past this list.
// Every column a table has that this list does not name gets that table's
// own type default (0, the empty string, an empty array or an empty map),
// which is safe here because FlagCounts reads none of them.
const flagFixtureColumns = "collector_id, router_ip, router_sysname, peer_ip, rib, " +
	"peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, parse_flags, stream_seq"

// insertParseFlagRow writes one row into table, through flagFixtureColumns,
// carrying exactly one parse flag. rib is fixed at 'in_pre' -- FlagCounts
// renders no rib predicate of its own (see FlagCounts' own doc comment:
// CollectionFilter carries no rib dimension), so no fixture in this section
// needs to vary it.
func insertParseFlagRow(t *testing.T, ctx context.Context, q *Q, table, router, peer string,
	sessionID, seq, streamSeq uint64, flag string, ts time.Time,
) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+"."+table+" ("+flagFixtureColumns+")")
	if err != nil {
		t.Fatalf("prepare %s parse-flag insert: %v", table, err)
	}
	if err := b.Append(
		"query-test", router, "flag-fixture-router", peer, "in_pre",
		uint32(65000), peer, sessionID, seq, ts, ts,
		[]string{flag}, streamSeq,
	); err != nil {
		t.Fatalf("append %s parse-flag row (flag %s): %v", table, flag, err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send %s parse-flag batch: %v", table, err)
	}
}

// seedFlagCountsFixtures writes this section's entire population.
func seedFlagCountsFixtures(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	ts := collectionFixtureAnchor.Add(30 * time.Second)

	// One row per table, each carrying a flag unique to that table (see
	// flagForTable), so that dropping ANY ONE of the ten branches from
	// flagCountsSQL's union makes exactly that table's own flag disappear
	// from FlagCounts' answer -- not only whichever single table a
	// narrower fixture happened to pick. Walks flagAllTables, NOT
	// query.go's own flagStreamTables -- see flagAllTables' own comment for
	// why that independence is load-bearing rather than redundant.
	for i, table := range flagAllTables {
		insertParseFlagRow(t, ctx, q, table, flagRouterEveryTable, flagPeerEveryTable,
			1, uint64(i+1), uint64(i+1), flagForTable(table), ts)
	}

	// Five route_unicast rows, one envelope: the same stream_seq, five
	// DISTINCT prefixes -- the shape a real fifty-prefix UPDATE takes,
	// scaled down to what a test can check by hand. All five carry
	// flagEnvelopesTestFlag; FlagCounts must read Envelopes = 1, never 5.
	const envelopeStreamSeq = 777
	for i := range 5 {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: flagRouterEnvelopes, RouterSysname: "flag-envelopes-router",
			PeerIP: flagPeerEnvelopes, RIB: "in_pre", Family: "ipv4u",
			Prefix:     fmt.Sprintf("10.94.20.%d/32", i),
			PeerBGPID:  flagPeerEnvelopes,
			SessionID:  1,
			Seq:        uint64(i + 1),
			StreamSeq:  envelopeStreamSeq,
			ParseFlags: []string{flagEnvelopesTestFlag},
			TsRouter:   ts, TsCollector: ts,
		})
	}

	// One eor_events row, inserted TWICE with byte-identical arguments --
	// the shape a JetStream at-least-once redelivery takes on the one
	// table with no ReplacingMergeTree dedup of its own (see
	// flagEnvelopeDistinctSQL). Both calls share every argument, including
	// stream_seq, on purpose: a real redelivery does too.
	for range 2 {
		insertParseFlagRow(t, ctx, q, "eor_events", flagRouterEorDup, flagPeerEorDup,
			1, 900, 900, flagEorDupTestFlag, ts)
	}
}

// flagCountFor runs FlagCounts scoped to one router and returns the
// Envelopes count for one flag, or 0 if the flag did not come back at all.
// 0 is a real, distinguishable answer here -- a flag this router never
// raised -- unlike dumpCountFor's and sessionCountFor's own helpers, which
// fail outright on an absent row: FlagCounts' contract is "zero or more
// flags", never "exactly one row for a router that was just seeded".
func flagCountFor(t *testing.T, ctx context.Context, q *Q, router, flag string) uint64 {
	t.Helper()
	rows, _, err := q.FlagCounts(ctx, collectionFilterFor(router))
	if err != nil {
		t.Fatalf("FlagCounts(%s): %v", router, err)
	}
	for _, r := range rows {
		if r.Flag == flag {
			return r.Envelopes
		}
	}
	return 0
}

// TestFlagCountsCountsEnvelopesNotRows is the whole point of the signal, as
// a test. One truncated-attribute UPDATE carrying fifty prefixes is one
// parse event, not fifty -- FlagCounts' own doc comment measures the live
// archive's stakes at up to 10.4x on PARSE_FLAG_LS_NLRI_UNDECODED. Five
// route_unicast rows here share one stream_seq (one envelope) and carry
// five distinct prefixes (five real routes), all raising the same flag:
// Envelopes must read 1, never 5.

// The duplicate-bearing routers for the two tests below. Their own
// addresses, and their own flag literal, because testDB is shared and never
// truncated: an assertion scoped to the whole table would be answerable by
// some other fixture's rows.
// Addresses used by NOTHING else in this package, checked rather than
// assumed. The first draft reused three that existing fixtures already own --
// 10.94.4.1 is topology_test.go's topoRouterScope, 10.93.11.1 is
// locRIBRouterWindow and 10.93.11.2 is locRIBPeerOldRoutes -- so another
// fixture's rows landed inside these tests' own scoped preconditions and
// inflated them. testDB is shared and never truncated, which this file warns
// about in several places; this is what ignoring that looks like.
//
// It also only showed up in a WHOLE-PACKAGE run. Under -run the colliding
// fixtures never seeded, the preconditions read exactly what they expected,
// and both tests passed.
const (
	dupFinalRouter = "10.94.19.1"
	dupFinalPeer   = "10.94.19.2"
	dupFinalFlag   = "TEST_FLAG_UNMERGED_DUPLICATE"

	locRIBRouterDup = "10.93.19.1"
	locRIBPeerDup   = "10.93.19.2"
)

// stopMergesOn disables background merges on one table for the duration of
// the test and turns them back on afterwards.
//
// context.WithoutCancel on the cleanup for the reason counts_test.go's own
// copy of this gives: t's context is already done by the time Cleanup runs,
// and a START MERGES that silently no-ops would leave that table's merges
// off for every test and every developer that runs after this one.
func stopMergesOn(t *testing.T, ctx context.Context, q *Q, table string) {
	t.Helper()
	if err := q.conn.Exec(ctx, fmt.Sprintf("SYSTEM STOP MERGES %s.%s", q.db, table)); err != nil {
		t.Fatalf("stop merges on %s: %v", table, err)
	}
	t.Cleanup(func() {
		_ = q.conn.Exec(context.WithoutCancel(ctx),
			fmt.Sprintf("SYSTEM START MERGES %s.%s", q.db, table))
	})
}

// THIS TEST EXISTS TO SETTLE A KEPT FINAL, and it is worth saying which.
//
// flagEnvelopeSQL reads nine of its ten tables with FINAL, an explicit
// exception to this package's "No FINAL anywhere" rule. The reason its
// comment gave was fidelity to fleet-health's "Parse flags" panel, "which
// chose FINAL for all nine of these tables". That stopped being true on
// 2026-09-19: the panel now reads all ten WITHOUT it, having measured that its
// own uniqExact((stream, stream_seq)) was doing the dedup and FINAL was
// merely masking it. Measured on a scale test on 2026-08-13, that masking
// cost 48x the rows read and 1,270x the memory.
//
// So the question is whether uniqExact alone is enough HERE, and the honest
// way to ask it is with a duplicate that has demonstrably not been merged
// away -- otherwise a green run says only that ClickHouse happened to merge
// in time, which is the intermittent-correctness trap this whole area keeps
// falling into.
//
// Both halves are asserted: the precondition that two raw rows really are
// present, and the answer that FlagCounts reports one envelope anyway.
// Without the precondition this test would pass for the wrong reason the
// moment a merge got there first.
func TestFlagCountsIsCorrectWithUnmergedDuplicatesPresent(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	stopMergesOn(t, ctx, q, "route_unicast")

	ts := collectionFixtureAnchor.Add(90 * time.Second)
	// The same row twice, byte-identical in every column route_unicast's
	// ORDER BY names -- a redelivered envelope, which is what
	// TestRowsForRedeliveryIsByteIdentical establishes the sink produces.
	for range 2 {
		insertParseFlagRow(t, ctx, q, "route_unicast", dupFinalRouter, dupFinalPeer,
			1, 1, 1, dupFinalFlag, ts)
	}

	// The precondition, scoped to this router: two raw rows, one distinct.
	var raw, distinct uint64
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(), uniqExact(stream_seq) FROM %s.route_unicast
		 WHERE router_ip = toIPv6('%s') AND has(parse_flags, '%s')`,
		q.db, dupFinalRouter, dupFinalFlag)).Scan(&raw, &distinct); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	if raw < 2 || distinct != 1 {
		t.Fatalf("route_unicast holds %d raw rows over %d distinct stream_seq "+
			"for this router, want at least 2 over 1 -- the duplicate has "+
			"already been merged away, so everything below would pass without "+
			"being asked the question", raw, distinct)
	}

	if got := flagCountFor(t, ctx, q, dupFinalRouter, dupFinalFlag); got != 1 {
		t.Errorf("Envelopes = %d, want 1 -- ONE envelope was delivered twice "+
			"and both rows are demonstrably still in the table. 2 means the "+
			"count is relying on FINAL rather than on its own "+
			"uniqExact((stream, stream_seq)), and this package's FINAL cannot "+
			"be dropped", got)
	}
}

// The same question for locRIBComparisonSQL's own kept FINAL, on
// stats_events, whose comment gave the same now-false reason: "matching
// fleet-health's own panel SQL verbatim". The panel dropped it on 2026-09-19,
// silently -- those two stats panels were untested at the time and were not
// named in that change's description.
//
// Both sides of the comparison are duplicated, because both sides could in
// principle count a duplicate: `reported` resolves counters[8] with argMax
// over (ts_collector, stream_seq), and `archived` counts route keys through
// a GROUP BY. Neither should move.
func TestLocRIBComparisonIsCorrectWithUnmergedDuplicatesPresent(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	stopMergesOn(t, ctx, q, "stats_events")
	stopMergesOn(t, ctx, q, "route_unicast")

	const sysname = "loc-rib-dup-router"
	ts := collectionFixtureAnchor.Add(120 * time.Second)
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterDup, sysname,
		locRIBPeerDup, 1, 1, collectionFixtureAnchor)
	stat := statsEventFixture{
		RouterIP: locRIBRouterDup, RouterSysname: sysname, PeerIP: locRIBPeerDup,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerDup,
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Counters:    map[uint32]uint64{8: 3},
		TsRouter:    ts,
		TsCollector: ts,
	}
	for range 2 {
		insertStatsEvent(t, ctx, q, stat)
	}
	// Three archived loc_rib routes, each written twice.
	for i := range 3 {
		rt := collectionFixtureAnchor.Add(time.Duration(i+1) * time.Second)
		row := routeUnicastFixture{
			RouterIP: locRIBRouterDup, RouterSysname: sysname, PeerIP: locRIBPeerDup,
			RIB: "loc_rib", PeerBGPID: locRIBPeerDup, Family: "ipv4u",
			Prefix:    fmt.Sprintf("10.93.19.%d/32", i),
			SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
			TsRouter: rt, TsCollector: rt,
		}
		for range 2 {
			insertRouteUnicastEvent(t, ctx, q, row)
		}
	}

	// Preconditions on both tables, scoped to this router.
	for _, tc := range []struct {
		table string
		want  uint64
	}{
		{"stats_events", 1}, {"route_unicast", 3},
	} {
		var raw, distinct uint64
		if err := q.conn.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(), uniqExact(stream_seq) FROM %s.%s
			 WHERE router_ip = toIPv6('%s')`,
			q.db, tc.table, locRIBRouterDup)).Scan(&raw, &distinct); err != nil {
			t.Fatalf("precondition on %s: %v", tc.table, err)
		}
		if raw != tc.want*2 || distinct != tc.want {
			t.Fatalf("%s holds %d raw rows over %d distinct stream_seq, want "+
				"%d over %d. Either the duplicates were merged away already "+
				"and this test would prove nothing, or these addresses are "+
				"not this test's alone -- testDB is shared and never "+
				"truncated, so another fixture writing the same router lands "+
				"inside this scope",
				tc.table, raw, distinct, tc.want*2, tc.want)
		}
	}

	got := locRIBRowFor(t, ctx, q, locRIBRouterDup, locRIBPeerDup)
	if !got.HasStat {
		t.Fatal("HasStat is false for a peer that sent a stats_events row")
	}
	if got.Reported != 3 {
		t.Errorf("Reported = %d, want 3 -- the router reported counters[8] = 3 "+
			"once and the envelope was redelivered; argMax picks the same "+
			"value from either copy, so this must not move", got.Reported)
	}
	if got.Archived != 3 {
		t.Errorf("Archived = %d, want 3 -- three distinct loc_rib routes, each "+
			"written twice. 6 means the archived side is counting rows rather "+
			"than route keys and is relying on FINAL to hide it", got.Archived)
	}
}

func TestFlagCountsCountsEnvelopesNotRows(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := flagCountFor(t, ctx, q, flagRouterEnvelopes, flagEnvelopesTestFlag)
	if got != 1 {
		t.Errorf("Envelopes = %d, want 1 -- five route_unicast rows share ONE "+
			"stream_seq (one envelope that exploded into five prefixes); "+
			"counting rows instead of uniqExact((stream, stream_seq)) reads "+
			"this as 5", got)
	}
}

// TestFlagCountsReadsEveryFlaggedTable flags one distinguishable row in
// EACH of flagAllTables' ten tables, under a flag unique to that table, and
// asserts every one of the ten comes back reading Envelopes = 1. Dropping
// ANY ONE of the ten branches from flagCountsSQL's union makes exactly that
// table's own flag disappear -- not only whichever single table a narrower
// fixture would have picked. This is the behavioral form of the mistake
// fleet-health's own panel already made once: an earlier cut "reported 2
// occurrences of a condition that had fired 43 times fleet-wide", missing
// ls_prefixes and eor_events, until a test caught it.
//
// Walks flagAllTables, NOT query.go's own flagStreamTables, both here and
// in seedFlagCountsFixtures -- see flagAllTables' own comment for why: a
// mutation that drops a table from flagStreamTables must not also be able
// to shrink what THIS test expects, or the mutation and the test would be
// reading from the same edited list and the drop would pass unnoticed.
func TestFlagCountsReadsEveryFlaggedTable(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	for _, table := range flagAllTables {
		flag := flagForTable(table)
		got := flagCountFor(t, ctx, q, flagRouterEveryTable, flag)
		if got != 1 {
			t.Errorf("table %s: Envelopes(%s) = %d, want 1 -- this table's own "+
				"flagged row never came back through FlagCounts, which means "+
				"the table is missing from flagCountsSQL's union",
				table, flag, got)
		}
	}
}

// TestFlagAllTablesMatchesFlagStreamTables is what keeps flagAllTables'
// independence from query.go's own flagStreamTables (see flagAllTables'
// own comment) from silently rotting into drift under ordinary
// development, as opposed to a mutation: if a future table gains a
// parse_flags column and is added to flagStreamTables alone, this fails
// and says exactly which list is now behind, rather than
// TestFlagCountsReadsEveryFlaggedTable quietly never checking the new
// table because its own hand-typed list was never told about it either.
func TestFlagAllTablesMatchesFlagStreamTables(t *testing.T) {
	want := slices.Clone(flagAllTables)
	slices.Sort(want)

	got := make([]string, len(flagStreamTables))
	for i, tt := range flagStreamTables {
		got[i] = tt.table
	}
	slices.Sort(got)

	if !slices.Equal(want, got) {
		t.Errorf("flagAllTables and query.go's own flagStreamTables have drifted apart:\n"+
			"  flagAllTables (this file):    %v\n"+
			"  flagStreamTables (collection.go): %v\n"+
			"Both must name the same ten tables -- update whichever one is now "+
			"behind the other", want, got)
	}
}

// TestFlagCountsReadsEorEventsWithoutFinal. eor_events is a plain MergeTree
// with no dedup of its own -- a byte-identical JetStream redelivery is a
// second physical row this table never collapses by itself, unlike the
// other nine tables' ReplacingMergeTree. seedFlagCountsFixtures inserts the
// identical row TWICE; Envelopes must still read 1.
//
// What actually keeps this test green is NOT the SELECT DISTINCT wrapper
// flagEnvelopeDistinctSQL reads eor_events through -- see that constant's
// own doc comment for the full argument. It is flagCountsSQL's own
// uniqExact((stream, stream_seq)): the two duplicate physical rows produce
// the identical (stream, stream_seq, flag) tuple after arrayJoin, and
// uniqExact counts distinct tuples regardless of how many rows produced
// each one. This test cannot and does not distinguish "DISTINCT collapsed
// the duplicate" from "uniqExact would have collapsed it anyway" -- it
// only pins that the final answer is 1, which both readings agree on. Do
// not read a PASS here as evidence DISTINCT is doing anything.
func TestFlagCountsReadsEorEventsWithoutFinal(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := flagCountFor(t, ctx, q, flagRouterEorDup, flagEorDupTestFlag)
	if got != 1 {
		t.Errorf("Envelopes = %d, want 1 -- one eor_events row was inserted "+
			"TWICE, byte-identical, the shape a JetStream at-least-once "+
			"redelivery takes on a table with no ReplacingMergeTree dedup of "+
			"its own; uniqExact((stream, stream_seq)) must still read this as "+
			"ONE envelope, the same tuple counted once regardless of how many "+
			"physical copies of the row produced it", got)
	}
}

// TestEorEventsRefusesFinal is the structural fact flagEnvelopeDistinctSQL's
// own doc comment rests on: eor_events is declared ENGINE = MergeTree, not
// ReplacingMergeTree (see schema.sql), and ClickHouse refuses FINAL against
// a table that cannot dedupe at all, at the engine level -- not as a slower
// no-op that merely wastes a scan. This runs the refused statement itself
// and records ClickHouse's own error text, rather than trusting a comment
// to have described the engine correctly and never re-checking it.
func TestEorEventsRefusesFinal(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	err := q.conn.Exec(ctx, "SELECT count() FROM "+q.db+".eor_events FINAL")
	if err == nil {
		t.Fatal("SELECT ... FROM eor_events FINAL succeeded; eor_events is " +
			"ENGINE = MergeTree and this project's own account of it -- " +
			"'ClickHouse refuses FINAL on it outright' -- is no longer true, " +
			"which means flagEnvelopeDistinctSQL's SELECT DISTINCT workaround " +
			"needs re-justifying, not merely re-testing")
	}
	t.Logf("ClickHouse's own refusal, verbatim: %v", err)
	if !strings.Contains(err.Error(), "FINAL") {
		t.Errorf("eor_events FINAL was refused, but not for the reason expected -- "+
			"the error does not even mention FINAL: %v", err)
	}
}

// TestCollectionQueriesReportTotalMatchedBeforeLimit pins the property
// total_matched (each statement's own count() OVER ()) exists for: it must
// count what MATCHED before Limit truncated the answer, not what was
// RETURNED. Every one of these four calls runs UNSCOPED at Limit: 1, against
// this whole test binary's seeded population -- deliberately, the same
// choice TestDumpCountsPartsSumToArchived already makes, because the
// property under test ("total is not just len(rows)") needs a population
// wider than any one row to be observable at all, and every seed function
// this file has already written multiple distinct routers, (router, peer)
// pairs or flags by the time this runs (see seedCollectionFixtures' own
// list). It does not assert an exact total: this package's testDB is
// shared and never truncated (collectionFixturesOnce's own doc comment),
// so an exact count would be re-run-fragile; total > len(rows) is the one
// invariant that holds regardless of how many times this binary's fixtures
// have already been seeded.
func TestCollectionQueriesReportTotalMatchedBeforeLimit(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	t.Run("DumpCounts", func(t *testing.T) {
		rows, total, err := q.DumpCounts(ctx, CollectionFilter{Limit: 1})
		if err != nil {
			t.Fatalf("DumpCounts: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1 (Limit: 1)", len(rows))
		}
		if total <= uint64(len(rows)) {
			t.Errorf("total_matched = %d, want > %d -- this binary has seeded more "+
				"than one router across route_unicast/route_vpn/route_evpn", total, len(rows))
		}
	})
	t.Run("SessionCounts", func(t *testing.T) {
		rows, total, err := q.SessionCounts(ctx, CollectionFilter{Limit: 1})
		if err != nil {
			t.Fatalf("SessionCounts: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1 (Limit: 1)", len(rows))
		}
		if total <= uint64(len(rows)) {
			t.Errorf("total_matched = %d, want > %d -- sessionCountsRouterA and "+
				"sessionCountsRouterB are two distinct routers", total, len(rows))
		}
	})
	t.Run("LocRIBComparison", func(t *testing.T) {
		rows, total, err := q.LocRIBComparison(ctx, CollectionFilter{Limit: 1})
		if err != nil {
			t.Fatalf("LocRIBComparison: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1 (Limit: 1)", len(rows))
		}
		if total <= uint64(len(rows)) {
			t.Errorf("total_matched = %d, want > %d -- this binary has seeded more "+
				"than one (router, peer) pair across the loc-rib fixtures", total, len(rows))
		}
	})
	t.Run("FlagCounts", func(t *testing.T) {
		rows, total, err := q.FlagCounts(ctx, CollectionFilter{Limit: 1})
		if err != nil {
			t.Fatalf("FlagCounts: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1 (Limit: 1)", len(rows))
		}
		if total <= uint64(len(rows)) {
			t.Errorf("total_matched = %d, want > %d -- seedFlagCountsFixtures alone "+
				"raises more than ten distinct flag literals", total, len(rows))
		}
	})
}

// TestCollectionQueriesTotalMatchedIsZeroWhenEmpty pins the OTHER half:
// count() OVER () produces no row to carry a value on when the grouped
// result is empty, so each of the four functions must fall back to 0 itself
// -- see DumpCounts' own doc comment -- rather than leaving total at
// whatever a zero-initialized local happens to already hold (which would
// pass by coincidence, not by the fallback actually running) or, worse,
// returning garbage from an unscanned variable.
//
// bogusRouter matches nothing in any of the four tables this binary ever
// writes to -- 10.93.255.255 sits inside the 10.93.0.0/16 range this file
// reserves (collectionFilterFor's own doc comment) but outside every /24
// a seed function actually uses.
func TestCollectionQueriesTotalMatchedIsZeroWhenEmpty(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	const bogusRouter = "10.93.255.255"
	f := collectionFilterFor(bogusRouter)

	if rows, total, err := q.DumpCounts(ctx, f); err != nil {
		t.Fatalf("DumpCounts: %v", err)
	} else if len(rows) != 0 || total != 0 {
		t.Errorf("DumpCounts(%s) = %d rows, total_matched %d, want 0 and 0", bogusRouter, len(rows), total)
	}
	if rows, total, err := q.SessionCounts(ctx, f); err != nil {
		t.Fatalf("SessionCounts: %v", err)
	} else if len(rows) != 0 || total != 0 {
		t.Errorf("SessionCounts(%s) = %d rows, total_matched %d, want 0 and 0", bogusRouter, len(rows), total)
	}
	if rows, total, err := q.LocRIBComparison(ctx, f); err != nil {
		t.Fatalf("LocRIBComparison: %v", err)
	} else if len(rows) != 0 || total != 0 {
		t.Errorf("LocRIBComparison(%s) = %d rows, total_matched %d, want 0 and 0", bogusRouter, len(rows), total)
	}
	if rows, total, err := q.FlagCounts(ctx, f); err != nil {
		t.Fatalf("FlagCounts: %v", err)
	} else if len(rows) != 0 || total != 0 {
		t.Errorf("FlagCounts(%s) = %d rows, total_matched %d, want 0 and 0", bogusRouter, len(rows), total)
	}
}

// seedChurnGroupingFixtures writes the population the two grouped churn
// answers are checked against: two peers of differing volume, four unicast
// prefixes of differing volume, one UNDECODED unicast row, and one row in
// each of the other two route tables.
//
// churnPeerBusy re-advertises 10.93.1201.0/24 four times inside one session
// (1 dump + 3 readvertise) and withdraws 10.93.1202.0/24 once (1 withdraw).
// churnPeerQuiet advertises 10.93.1203.0/24 exactly once (1 dump). So the
// two peers differ in EVERY series, not only in total -- a GROUP BY that
// dropped the peer column would report one row of 5+1+... rather than two,
// and a per-peer answer that summed the wrong series would still match on
// the total.
//
// THE UNDECODED ROW is the one worth stating at length. 10.93.12.1 writes a
// row with prefix = "" and PARSE_FLAG_VERSION_UNPARSED, which is not an
// invention: a real archive holds 56 such rows in route_unicast, and
// a most-changed-prefixes answer that does not exclude them ranks a BLANK
// LABEL first, because undecoded NLRI is exactly the kind of row a broken
// session produces many of. It is a collection artifact being counted as a
// network fact -- the defect this project has now hit seven times.
//
// THE VPN AND EVPN ROWS pin the other half: prefix does not mean the same
// thing across the three tables. A route_vpn prefix without its rd is
// ambiguous (the same string under two RDs is two different routes), and 136
// of a real archive's 202 route_evpn rows carry prefix = "" because a
// type-2 MAC route has no IP prefix at all. Both rows below use the SAME
// prefix string as a unicast row on purpose, so a three-table union shows up
// here as an inflated count rather than as a plausible one.
func seedChurnGroupingFixtures(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	for i := uint64(1); i <= 4; i++ { // busy peer: 1 dump + 3 readvertise
		ts := collectionFixtureAnchor.Add(time.Duration(i) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: churnRouterGrouping, RouterSysname: "churn-g", PeerIP: churnPeerBusy,
			RIB: "in_pre", PeerBGPID: churnPeerBusy, Family: "ipv4u", Prefix: "10.93.1201.0/24",
			SessionID: 1, Seq: i, StreamSeq: i, TsRouter: ts, TsCollector: ts,
		})
	}
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{ // busy peer: 1 withdraw
		RouterIP: churnRouterGrouping, RouterSysname: "churn-g", PeerIP: churnPeerBusy,
		RIB: "in_pre", PeerBGPID: churnPeerBusy, Family: "ipv4u", Prefix: "10.93.1202.0/24",
		SessionID: 1, Seq: 5, StreamSeq: 5, IsWithdraw: 1,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{ // quiet peer: 1 dump
		RouterIP: churnRouterGrouping, RouterSysname: "churn-g", PeerIP: churnPeerQuiet,
		RIB: "in_pre", PeerBGPID: churnPeerQuiet, Family: "ipv4u", Prefix: "10.93.1203.0/24",
		SessionID: 2, Seq: 1, StreamSeq: 1,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{ // undecoded: no prefix at all
		RouterIP: churnRouterGrouping, RouterSysname: "churn-g", PeerIP: churnPeerBusy,
		RIB: "in_pre", PeerBGPID: churnPeerBusy, Family: "ipv4u", Prefix: "",
		ParseFlags: []string{"PARSE_FLAG_VERSION_UNPARSED"},
		SessionID:  1, Seq: 6, StreamSeq: 6,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{ // same prefix STRING, different table
		RouterIP: churnRouterGrouping, RouterSysname: "churn-g", PeerIP: churnPeerBusy,
		RIB: "in_pre", PeerBGPID: churnPeerBusy, Family: "vpn4", RD: "65000:1",
		Prefix: "10.93.1201.0/24", SessionID: 1, Seq: 7, StreamSeq: 7,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
	insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
		RouterIP: churnRouterGrouping, RouterSysname: "churn-g", PeerIP: churnPeerBusy,
		RIB: "in_pre", PeerBGPID: churnPeerBusy, RouteType: 2, RD: "65000:1",
		Prefix: "10.93.1201.0/24", SessionID: 1, Seq: 8, StreamSeq: 8,
		TsRouter: collectionFixtureAnchor, TsCollector: collectionFixtureAnchor,
	})
}

// The multi-collector Loc-RIB fixture. One router, one peer, two collectors,
// and five routes chosen so that the shipped defect, the obvious wrong fix and
// the correct fix all produce DIFFERENT numbers. 10.93.13.x is claimed by
// nothing else in this package; testDB is shared and never truncated.
const (
	locRIBRouterTwoC   = "10.93.13.1"
	locRIBPeerTwoC     = "10.93.13.2"
	locRIBCollectorTwo = "query-test-c2"
)

// TestLocRIBComparisonCountsARouteOnceAcrossTwoCollectors closes a gap once
// filed as theoretical ("single collector today; the comparison skews on a
// second") -- no longer true once a second collector existed.
//
// The two sides of this comparison scale differently. Reported is the ROUTER's
// own claim about its Loc-RIB (Stats Report counter 8), one argMax over
// (router_ip, peer_ip) with no collector in it -- both collectors receive the
// same Stats Reports, so it is one number however many are watching. Archived
// grouped on unicastRouteIdentity, which LEADS with collector_id, and then
// counted the groups: one route two collectors both hold became two. So
// archived scaled with the number of collectors and reported did not, and the
// screen rendered collection holding twice what the router claims -- on a
// screen that exists to say a Loc-RIB gap is not proof of loss. This is the
// inverse: a fabricated surplus.
//
// Measured against a real dual-homed router, running the shipped CTE's own
// shape: 34 reported against a true distinct count of 17.
//
// The fixture separates three answers on purpose:
//
//	8  the shipped defect -- counting groups with the collector in the key
//	4  the obvious wrong fix -- deleting collector_id from that GROUP BY,
//	   which leaves argMax(is_withdraw, (seq, stream_seq)) spanning two
//	   collectors. seq is a per-stream counter each collector mints for
//	   itself, so that ranks two independent counters and picks a winner on
//	   nothing -- the same defect, also found and fixed in the ASN view.
//	   Here it loses 10.93.13.30, which one collector withdrew and the
//	   other did not.
//	5  correct -- resolve per collector, then count each route identity once
//
// Reported is written by one collector because statsEventFixture hardcodes
// collector_id. That is not a gap in the fixture: reported is an argMax, not a
// count, so a second copy would change nothing, which is precisely the
// asymmetry the test is about.
func TestLocRIBComparisonCountsARouteOnceAcrossTwoCollectors(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	const sysname = "loc-rib-two-collector"
	ts := collectionFixtureAnchor.Add(140 * time.Second)
	// Each collector has its own current session for this router, and both
	// happen to be numbered 1 -- nothing makes session_ids unique across
	// collectors (see peerStateCTE), which is why the archived side scopes on
	// (collector_id, router_ip, session_id) and never on the session alone.
	for i, c := range []string{defaultFixtureCollector, locRIBCollectorTwo} {
		locRIBSessionUp(t, ctx, q, c, locRIBRouterTwoC, sysname,
			locRIBPeerTwoC, 1, uint64(8900+i), ts)
	}
	insertStatsEvent(t, ctx, q, statsEventFixture{
		RouterIP: locRIBRouterTwoC, RouterSysname: sysname, PeerIP: locRIBPeerTwoC,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: locRIBPeerTwoC,
		SessionID: 1, Seq: 1, StreamSeq: 9001,
		Counters:    map[uint32]uint64{8: 5},
		TsRouter:    ts,
		TsCollector: ts,
	})

	route := func(collector, prefix string, seq, streamSeq uint64, withdraw uint8) {
		rt := ts.Add(time.Duration(seq) * time.Second)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: locRIBRouterTwoC, RouterSysname: sysname, PeerIP: locRIBPeerTwoC,
			RIB: "loc_rib", Collector: collector, PeerASN: 65000,
			PeerBGPID: locRIBPeerTwoC, Family: "ipv4u", Prefix: prefix,
			SessionID: 1, Seq: seq, StreamSeq: streamSeq, IsWithdraw: withdraw,
			TsRouter: rt, TsCollector: rt,
		})
	}

	// Three routes both collectors hold. Each collector's seq counter starts
	// at 1 independently -- that is what a per-stream counter does -- while
	// stream_seq is the shared JetStream sequence and is distinct throughout.
	// It has to be: route_unicast's ORDER BY carries stream_seq and NOT
	// collector_id, so two collectors' copies of one route are only separate
	// rows because that number differs.
	for i, pfx := range []string{"10.93.13.10/32", "10.93.13.11/32", "10.93.13.12/32"} {
		route(defaultFixtureCollector, pfx, uint64(i+1), uint64(9100+i), 0)
		route(locRIBCollectorTwo, pfx, uint64(i+1), uint64(9200+i), 0)
	}
	// One route only the first collector holds: it must still count once, so a
	// fix cannot simply divide by the number of collectors.
	route(defaultFixtureCollector, "10.93.13.20/32", 4, 9110, 0)
	// One route both announced and only the FIRST withdrew. Its withdrawal
	// carries the highest seq and stream_seq in the fixture, so it wins any
	// argMax that is allowed to span the two collectors -- which is what makes
	// the wrong fix observable rather than merely wrong in principle.
	route(defaultFixtureCollector, "10.93.13.30/32", 5, 9120, 0)
	route(locRIBCollectorTwo, "10.93.13.30/32", 5, 9220, 0)
	route(defaultFixtureCollector, "10.93.13.30/32", 6, 9300, 1)

	got := locRIBRowFor(t, ctx, q, locRIBRouterTwoC, locRIBPeerTwoC)
	if !got.HasStat {
		t.Fatal("HasStat is false for a peer that sent a stats_events row")
	}
	if got.Reported != 5 {
		t.Errorf("Reported = %d, want 5 -- counter 8 is the router's own claim "+
			"and one collector wrote it; this side must not move", got.Reported)
	}
	if got.Archived != 5 {
		t.Errorf("Archived = %d, want 5. 8 is the shipped defect: the inner "+
			"GROUP BY leads with collector_id and the outer count() counts "+
			"groups, so each of the three routes both collectors hold counts "+
			"twice. 4 is the wrong fix: deleting collector_id lets "+
			"argMax(is_withdraw, (seq, stream_seq)) span both collectors, and "+
			"10.93.13.30 -- withdrawn by one collector and still held by the "+
			"other -- loses to a seq counter from a different collector "+
			"entirely", got.Archived)
	}
}

// locRIBSessionUp writes one Peer Up for a Loc-RIB fixture router, so that
// router has a cur row -- peerStateCTE's current session, max(session_id)
// over peer_current -- the way a healthy BMP session does. LocRIBComparison's
// archived side reads the newer of that session and its loc_rib rows' own
// newest, so the fixtures that do not care about the difference write both;
// seedLocRIBSessionFallbackFixture is the one that withholds it on purpose.
func locRIBSessionUp(t *testing.T, ctx context.Context, q *Q,
	collector, router, sysname, peer string, session, streamSeq uint64, ts time.Time) {
	t.Helper()
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: router, RouterSysname: sysname, PeerIP: peer, RIB: "loc_rib",
		Collector: collector, PeerASN: 65000, PeerBGPID: peer,
		SessionID: session, Seq: 1, StreamSeq: streamSeq, Kind: "up",
		TsRouter: ts, TsCollector: ts,
	})
}

// The two-session Loc-RIB fixture. One router, two BMP sessions, three
// peers, and a second collector whose old session collides with the first
// collector's current one. 10.93.16.x is claimed by nothing else in this
// package; testDB is shared and never truncated.
//
// locRIBSessionOld and locRIBSessionNew are the two session_ids, in the
// order a collector mints them (nextSessionID: wall-clock nanoseconds,
// strictly increasing within a process). seq restarts in every session --
// it is the per-session message counter -- so the fixture gives the OLD
// session the HIGH seq values and the new session the low ones. That is the
// ordinary shape after any reconnect, not a contrived one: a long-lived
// session ends at a large seq and its successor starts again from 1.
// stream_seq, the JetStream sequence, keeps rising across both.
const (
	locRIBRouterTwoS     = "10.93.16.1"
	locRIBPeerWithdrawn  = "10.93.16.2"
	locRIBPeerReannounce = "10.93.16.3"
	locRIBPeerGhost      = "10.93.16.4"

	locRIBSessionOld uint64 = 1_000
	locRIBSessionNew uint64 = 2_000
)

// seedLocRIBTwoSessionFixture writes the routes
// TestLocRIBComparisonResolvesEachRouteInTheCurrentSession asserts on. Every
// peer also gets one control route, announced only in the new session, so
// each peer appears in the answer whatever happens to its interesting route
// -- a peer whose Archived fell to 0 would otherwise vanish from the output
// and fail on a missing row rather than on the count.
//
//	peer          old session (1000)       new session (2000)    live?
//	withdrawn     P announced, seq 100     P withdrawn, seq 5    no
//	reannounce    P announced, seq 50      P announced, seq 5    yes
//	              P withdrawn, seq 100
//	ghost         P announced, seq 100     (nothing)             no
//
// The ghost peer is what separates the correct fix from an ordering that
// also gets it wrong: ordering the argMax on (session_id, seq, stream_seq)
// gets the first two peers right and still reads the ghost's P as live,
// because its newest row anywhere is an announcement. It is not live. A new
// BMP session re-dumps the router's whole table, and a route the new session
// never mentions is one the router no longer holds -- which is what
// Reported, the router's own counter, is counting.
func seedLocRIBTwoSessionFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "loc-rib-two-session"
	base := collectionFixtureAnchor.Add(160 * time.Second)

	// The new session's Peer Up is written first: cur must resolve on
	// max(session_id), not on whichever row landed last.
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterTwoS, sysname,
		locRIBPeerWithdrawn, locRIBSessionNew, 16_500, base.Add(10*time.Second))
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterTwoS, sysname,
		locRIBPeerWithdrawn, locRIBSessionOld, 16_000, base)

	route := func(peer, prefix string, session, seq, streamSeq uint64, withdraw uint8) {
		ts := base.Add(time.Duration(streamSeq-16_000) * time.Millisecond)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: locRIBRouterTwoS, RouterSysname: sysname, PeerIP: peer,
			RIB: "loc_rib", PeerASN: 65000, PeerBGPID: peer, Family: "ipv4u",
			Prefix: prefix, SessionID: session, Seq: seq, StreamSeq: streamSeq,
			IsWithdraw: withdraw, TsRouter: ts, TsCollector: ts,
		})
	}

	route(locRIBPeerWithdrawn, "10.93.16.16/28", locRIBSessionOld, 100, 16_100, 0)
	route(locRIBPeerWithdrawn, "10.93.16.16/28", locRIBSessionNew, 5, 16_605, 1)
	route(locRIBPeerWithdrawn, "10.93.16.112/28", locRIBSessionNew, 6, 16_606, 0)

	route(locRIBPeerReannounce, "10.93.16.32/28", locRIBSessionOld, 50, 16_050, 0)
	route(locRIBPeerReannounce, "10.93.16.32/28", locRIBSessionOld, 100, 16_150, 1)
	route(locRIBPeerReannounce, "10.93.16.32/28", locRIBSessionNew, 5, 16_705, 0)
	route(locRIBPeerReannounce, "10.93.16.128/28", locRIBSessionNew, 6, 16_706, 0)

	route(locRIBPeerGhost, "10.93.16.48/28", locRIBSessionOld, 100, 16_200, 0)
	route(locRIBPeerGhost, "10.93.16.144/28", locRIBSessionNew, 6, 16_806, 0)

	// A second collector watching the same router, whose SUPERSEDED session
	// carries the same number as the first collector's current one. Its
	// current session is 3000 and holds nothing for the ghost peer, so the
	// route its old session announced is a ghost too. Scoping on (router,
	// session) without the collector lets that old session through as if it
	// were current, because 2000 is the first collector's current session.
	locRIBSessionUp(t, ctx, q, locRIBCollectorTwo, locRIBRouterTwoS, sysname,
		locRIBPeerGhost, locRIBSessionNew, 16_900, base)
	locRIBSessionUp(t, ctx, q, locRIBCollectorTwo, locRIBRouterTwoS, sysname,
		locRIBPeerGhost, 3_000, 16_950, base.Add(20*time.Second))
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: locRIBRouterTwoS, RouterSysname: sysname, PeerIP: locRIBPeerGhost,
		RIB: "loc_rib", Collector: locRIBCollectorTwo, PeerASN: 65000,
		PeerBGPID: locRIBPeerGhost, Family: "ipv4u", Prefix: "10.93.16.64/28",
		SessionID: locRIBSessionNew, Seq: 7, StreamSeq: 16_907,
		TsRouter: base, TsCollector: base,
	})
}

// TestLocRIBComparisonResolvesEachRouteInTheCurrentSession holds the archived
// side to one session per (collector, router).
//
// It used to resolve each route with argMax(is_withdraw, (seq, stream_seq))
// across every session route_unicast_current still held -- and cleanup keeps
// two per (collector, router) on purpose. seq is a per-session counter, so
// that argMax ranked two sessions' counters against each other: a route
// announced late in the old session and withdrawn early in the new one read
// as live (withdrawn: 2 instead of 1), and a route withdrawn late in the old
// session and re-announced early in the new one read as dead (reannounce: 1
// instead of 2). The ghost peer reads 2 instead of 1 under both that defect
// and an alternative session_id-first ordering that also gets it wrong; see
// seedLocRIBTwoSessionFixture for why only the current session answers it.
func TestLocRIBComparisonResolvesEachRouteInTheCurrentSession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	for _, tc := range []struct {
		peer string
		want uint64
		why  string
	}{
		{locRIBPeerWithdrawn, 1, "10.93.16.16/28 was withdrawn in the current " +
			"session. 2 means its old session's announcement won on seq 100 > 5, " +
			"a counter that restarts every session"},
		{locRIBPeerReannounce, 2, "10.93.16.32/28 was re-announced in the current " +
			"session. 1 means its old session's withdrawal won on seq 100 > 5"},
		{locRIBPeerGhost, 1, "10.93.16.48/28 exists only in the superseded " +
			"session, which the router's re-dump never repeated, and 10.93.16.64/28 " +
			"only in the second collector's superseded session 2000. 2 means one " +
			"of them was counted: resolving across sessions (ordering on " +
			"session_id first included) counts the first; scoping on session " +
			"without collector_id counts the second"},
	} {
		got := locRIBRowFor(t, ctx, q, locRIBRouterTwoS, tc.peer)
		if got.Archived != tc.want {
			t.Errorf("peer %s: Archived = %d, want %d -- %s",
				tc.peer, got.Archived, tc.want, tc.why)
		}
	}
}

// Two routers for the archived side's session choice when peer_current
// cannot make it alone. 10.93.17.x and 10.93.18.x are claimed by nothing
// else in this package; testDB is shared and never truncated.
//
// locRIBRouterNoPeerUp has loc_rib rows and NO peer_events row at all -- a
// Loc-RIB-only router whose Peer Up was lost, or malformed
// (collector/session.go's handlePeerUp writes no peer_events row for a Peer
// Up it cannot decode). cur has nothing for it.
//
// locRIBRouterLatePeerUp reconnected, and its new session's loc_rib rows
// reached ClickHouse before that session's Peer Up reached peer_current --
// they arrive on different streams, which sink/cleanup.go's own doc comment
// names as ordinary. cur still says the old session.
const (
	locRIBRouterNoPeerUp   = "10.93.17.1"
	locRIBPeerNoPeerUp     = "10.93.17.2"
	locRIBRouterLatePeerUp = "10.93.18.1"
	locRIBPeerLatePeerUp   = "10.93.18.2"
)

// seedLocRIBSessionFallbackFixture writes both routers. Each has an older
// session holding one route the newer session never repeats, and a newer
// session holding two, so the right answer (2) differs from reading the old
// session (1), from reading nothing (0) and from reading both (3).
//
// Each also reports counter 8 = 2. That is what makes an Archived of 0 the
// defect it is: a Reported of 2 against an Archived of 0 renders as a
// Loc-RIB gap on the Monitor screen, which must never fabricate one.
func seedLocRIBSessionFallbackFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "loc-rib-session-fallback"
	base := collectionFixtureAnchor.Add(180 * time.Second)

	for _, r := range []struct{ router, peer, net string }{
		{locRIBRouterNoPeerUp, locRIBPeerNoPeerUp, "10.93.17"},
		{locRIBRouterLatePeerUp, locRIBPeerLatePeerUp, "10.93.18"},
	} {
		route := func(prefix string, session, seq, streamSeq uint64) {
			insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
				RouterIP: r.router, RouterSysname: sysname, PeerIP: r.peer,
				RIB: "loc_rib", PeerASN: 65000, PeerBGPID: r.peer, Family: "ipv4u",
				Prefix: prefix, SessionID: session, Seq: seq, StreamSeq: streamSeq,
				TsRouter: base, TsCollector: base,
			})
		}
		route(r.net+".16/28", locRIBSessionOld, 100, 17_100)
		route(r.net+".32/28", locRIBSessionNew, 5, 17_205)
		route(r.net+".48/28", locRIBSessionNew, 6, 17_206)
		insertStatsEvent(t, ctx, q, statsEventFixture{
			RouterIP: r.router, RouterSysname: sysname, PeerIP: r.peer,
			RIB: "loc_rib", PeerASN: 65000, PeerBGPID: r.peer,
			SessionID: locRIBSessionNew, Seq: 7, StreamSeq: 17_207,
			Counters: map[uint32]uint64{8: 2}, TsRouter: base, TsCollector: base,
		})
	}
	// The late router's OLD session did send its Peer Up; the new one's has
	// not landed yet.
	locRIBSessionUp(t, ctx, q, defaultFixtureCollector, locRIBRouterLatePeerUp, sysname,
		locRIBPeerLatePeerUp, locRIBSessionOld, 17_000, base)
}

// TestLocRIBComparisonFallsBackToTheNewestLocRIBSession holds the archived
// side's session choice to the NEWER of cur's session and the newest session
// the router's own loc_rib rows carry, per (collector, router).
//
// Scoping to cur alone read Archived = 0 for a router with no peer_events
// row at all, against its Reported of 2 -- a fabricated gap, and before the
// archived side was session-scoped it did not depend on peer_events in the
// first place. And it read the superseded session for a router whose new
// Peer Up was still in flight: 1, the route the new session never
// repeated. Both read 2 now. TestLocRIBComparisonResolvesEachRouteInTheCurrentSession
// holds the other direction: where cur is the newer session, cur wins, even
// if the router has sent no loc_rib row in it yet.
func TestLocRIBComparisonFallsBackToTheNewestLocRIBSession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	for _, tc := range []struct{ router, peer, why string }{
		{locRIBRouterNoPeerUp, locRIBPeerNoPeerUp, "this router has no peer_events " +
			"row, so cur has no session for it. 0 means the archived side still " +
			"requires one, and renders a Loc-RIB gap collection fabricated; 3 " +
			"means it reads every session"},
		{locRIBRouterLatePeerUp, locRIBPeerLatePeerUp, "this router's newest " +
			"loc_rib rows are in a session whose Peer Up has not reached " +
			"peer_current. 1 means cur's older session was read"},
	} {
		got := locRIBRowFor(t, ctx, q, tc.router, tc.peer)
		if got.Reported != 2 || !got.HasStat {
			t.Errorf("router %s: Reported = %d, HasStat = %v; want 2 and true",
				tc.router, got.Reported, got.HasStat)
		}
		if got.Archived != 2 {
			t.Errorf("router %s: Archived = %d, want 2 -- %s",
				tc.router, got.Archived, tc.why)
		}
	}
}

// TestLocRIBRouteKeyWithinPeerMatchesTheUnicastIdentity pins the derived key
// to the one it is derived from. locRIBRouteKeyWithinPeer is unicastRouteIdentity
// minus the three columns constant inside a (router, peer) group, and nothing
// enforces that but this: a column added to the unicast route key and not here
// would leave the Loc-RIB comparison deduping on a key that no longer
// distinguishes two routes, silently undercounting instead of overcounting.
func TestLocRIBRouteKeyWithinPeerMatchesTheUnicastIdentity(t *testing.T) {
	want := "collector_id, router_ip, peer_ip, " + locRIBRouteKeyWithinPeer
	if unicastRouteIdentity != want {
		t.Errorf("unicastRouteIdentity = %q, but locRIBRouteKeyWithinPeer implies %q.\n"+
			"These must stay in step: locRIBRouteKeyWithinPeer is the unicast route "+
			"key with collector_id, router_ip and peer_ip removed, and it is what "+
			"LocRIBComparison's archived side deduplicates on",
			unicastRouteIdentity, want)
	}
}

// TestSessionCountsReportOneCollectorsViewNotTheSumOfBoth holds the
// per-collector session count.
//
// sessionCountsSQL grouped by router_ip alone, so every column scaled with
// how many collectors watched. CONFIRMED against a real dual-collector
// deployment: one router reported 7 sessions where one collector saw 5 and
// the other saw 2, and 14 ups where the two saw 10 and 4. Router
// 172.22.0.10, watched simultaneously by both, reported 2 sessions for what
// is one session lifecycle seen twice.
//
// Sessions is the column that makes this unambiguous. RouterSessionCount's
// own doc says it answers "how often the router's transport actually
// reset" -- a fact about the ROUTER -- so a second collector watching must
// not increase it. Up and Down are router statements for the same reason:
// a Peer Down is the router telling BMP a BGP session ended, on the wire,
// with a reason code.
//
// ViewLost is NOT covered here and does not take this treatment; see
// TestSessionCountsKeepsEveryCollectorsLostView for why it must not.
func TestSessionCountsReportOneCollectorsViewNotTheSumOfBoth(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := sessionCountFor(t, ctx, q, sessionCountsRouterDual)
	if got.Sessions != 2 || got.Up != 5 || got.Down != 1 {
		t.Errorf("got %d sessions / %d up / %d down, want 2/5/1 -- %s's whole "+
			"view, it being the collector that heard the most FROM THE "+
			"ROUTER. 3/7/1 is both collectors summed, which reports how many "+
			"collectors watch this router as how often it reconnected",
			got.Sessions, got.Up, got.Down, defaultFixtureCollector)
	}
}

// TestSessionCountsKeepsEveryCollectorsLostView is the half of the same
// defect that must NOT be fixed the way the rest of it is.
//
// view_lost is a COLLECTOR-side statement -- Session.Close writing down that
// it stopped being able to see the peer, with down_reason 0 because the
// router said nothing at all. It is a collection artifact, and a quantity
// describing COLLECTION legitimately scales with the number of collectors,
// exactly as `observations` does on the most-changed-prefixes panel.
//
// Best-vantage-point would not merely be imprecise here, it would be
// SIGNAL-SUPPRESSING, and structurally so. A collector that has gone blind
// records FEWER router statements by definition, so it always loses the
// vantage-point choice -- and the one column that exists to surface its
// blindness is then read off the collector that stayed healthy. This
// fixture is that adversarial case exactly: all three view_lost rows belong
// to the collector that loses, so riding along with the winner reports 0
// lost views on a collection-health signal while a collector really was
// blind three times.
//
// So Sessions, Up and Down come from the chosen collector and ViewLost is
// summed across all of them. That is a deliberate mix of subjects in one
// row, which is usually the wrong shape for a result row; it is justified
// here because the two subjects are nameable and the column names them.
// What must be avoided is an ARBITRARY mix -- a per-column argMax reporting
// one collector's ups beside another's downs.
func TestSessionCountsKeepsEveryCollectorsLostView(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := sessionCountFor(t, ctx, q, sessionCountsRouterDual)
	if got.ViewLost != 5 {
		t.Errorf("got %d view_lost, want 5 -- every one of them %s's, the "+
			"collector that lost its view of this router five times and "+
			"therefore heard the LEAST from it. 0 means view_lost was read "+
			"off the winning collector's row, which hides collector "+
			"blindness on the signal whose whole job is to report it",
			got.ViewLost, sessionCountsBlindCollector)
	}
}

// TestSessionCountsResolvesATieToOneCollectorsWholeRow guards the second
// half of the (router_events, collector_id) ordering tuple.
//
// Every column here is an argMax over the SAME expression, which is what
// makes the answer ONE collector's row rather than a per-column maximum.
// That only holds while the expression is a total order. With a bare argMax
// on router_events, two collectors that heard equally many statements tie,
// and ClickHouse may resolve each column's tie independently -- so a row
// could carry one collector's Sessions beside the other's Up and be a
// vantage point that never existed.
//
// The assertion is therefore two things at once: the triple must be exactly
// one collector's, and it must be the one the documented tie-break names.
func TestSessionCountsResolvesATieToOneCollectorsWholeRow(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedCollectionFixtures(t, ctx, q)

	got := sessionCountFor(t, ctx, q, sessionCountsRouterTie)
	type triple struct{ sessions, up, down uint64 }
	first := triple{2, 3, 1}  // defaultFixtureCollector's whole view
	second := triple{1, 4, 0} // sessionCountsTieCollector's whole view
	have := triple{got.Sessions, got.Up, got.Down}
	if have != first && have != second {
		t.Fatalf("got %d sessions / %d up / %d down, which is NEITHER "+
			"collector's view (%+v or %+v). A row belonging to no vantage "+
			"point is what the collector_id tie-break exists to prevent: "+
			"the two collectors heard four router statements each, so a "+
			"bare argMax on the count alone lets every column resolve its "+
			"tie independently", got.Sessions, got.Up, got.Down, first, second)
	}
	if have != second {
		t.Errorf("got %+v, want %+v -- %s's view. Both collectors heard "+
			"four router statements, so the tuple's second element decides, "+
			"and %s sorts after %s", have, second, sessionCountsTieCollector,
			sessionCountsTieCollector, defaultFixtureCollector)
	}
}
