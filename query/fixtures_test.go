package query

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
)

// fixtureRouterIP is the router insertFlappedPeerFixture writes its three
// peers under, as a query.Peers argument rather than the plain string
// routers_test.go's fixtures use for the same purpose (Peers takes a
// netip.Addr, Routers takes none). It is 10.0.0.7 to avoid every router IP
// already occupied by routers_test.go's own fixtures -- 10.0.0.1
// (two-session-router), 10.0.0.3 (recovered-peer-router) and 10.0.0.5
// (seq-outlives-session-router).
var fixtureRouterIP = netip.MustParseAddr("10.0.0.7")

// flappedPeerSessionID is the one session insertFlappedPeerFixture writes
// all three of its peers into. Its value carries no meaning beyond not
// colliding with a session_id routers_test.go's fixtures already use (1, 2,
// 9, 10, 11).
const flappedPeerSessionID = 100

// insertFlappedPeerFixture writes fixtureRouterIP's peers through one
// session -- manufacturing a currently-down peer the live archive lacks: a
// real captured archive's 20 current (router, peer, rib) tuples are all up,
// so "a down peer's routes are excluded" has nothing in the archive to make
// it false. Without a fixture that manufactures a currently-down peer, that
// claim is untested no matter how the query is written.
//
// Three peers, one session:
//
//	10.0.0.1  one event: up.                      Session ends up.
//	10.0.0.2  up, then down at a higher seq.      Session ends down.
//	10.0.0.3  up, down, then up again at the      Session ends up, even
//	          highest seq -- the shape behind 12  though a query that merely
//	          of a real captured archive's 17     asks WHERE kind = 'down'
//	          down events.                        calls it down.
//
// 10.0.0.2 alone would leave a bug free to pass: a query that treats "ever
// went down" as "is down" gets 10.0.0.2 right by accident, since 10.0.0.2
// never came back. 10.0.0.3 is what actually exercises the session-ending
// state versus the ever-seen state.
//
// Every row's stream_seq and ts_router track the order this function calls
// insertPeerEvent, not seq: 10.0.0.3's session-ending up row (seq 3) is
// written first, and its down row (seq 2) last. So a query that resolves
// state by "highest stream_seq" or "latest ts_router" instead of by seq
// lands on down for a peer whose session in fact ended up.
// TestFlappedPeerFixtureDefeatsBothNaivePeerStateQueries asserts that
// property directly against this fixture's own rows, before any test
// trusts Peers to interpret them correctly.
func insertFlappedPeerFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "flapped-peer-router"
	base := time.Now().UTC()

	type event struct {
		peerIP         string
		kind           string
		seq, streamSeq uint64
	}
	// Physical insertion order -- deliberately not seq order for 10.0.0.3
	// or 10.0.0.2; see the doc comment above.
	events := []event{
		{"10.0.0.3", "up", 3, 1},
		{"10.0.0.2", "down", 2, 2},
		{"10.0.0.1", "up", 1, 3},
		{"10.0.0.3", "up", 1, 4},
		{"10.0.0.2", "up", 1, 5},
		{"10.0.0.3", "down", 2, 6},
	}
	for _, e := range events {
		ts := base.Add(time.Duration(e.streamSeq) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: fixtureRouterIP.String(), RouterSysname: sysname,
			PeerIP: e.peerIP, RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: e.peerIP,
			SessionID: flappedPeerSessionID, Seq: e.seq, StreamSeq: e.streamSeq,
			Kind:        e.kind,
			TsRouter:    ts,
			TsCollector: ts,
		})
	}
}

// dumpStateFixtureRouterIP is the router insertDumpStateFixture writes its
// three peers under -- distinct from fixtureRouterIP because that
// fixture's three peers carry no route or marker rows at all, and mixing a
// dump-progress concern into a fixture built to isolate a state-resolution
// concern would make a future failure ambiguous about which behavior
// broke.
var dumpStateFixtureRouterIP = netip.MustParseAddr("10.0.0.9")

const dumpStateFixtureSessionID = 200

// routeUnicastFixture is the write-side shape this package's own tests need
// to build one route_unicast row -- just enough to exercise Peers' and
// Routes' own queries, not sink's whole UnicastRow shape (see
// sink.UnicastRow and sink.insertUnicast, which this mirrors the column
// order of).
//
// PathID, IsWithdraw, ASPath and NextHop are read by Routes. A fixture that
// leaves them unnamed gets their Go zero values (0, 0, nil, ""), and nil and
// "" round-trip through the driver identically to []uint32{} and "".
//
// There is no EndOfRIB field: end-of-RIB markers live in eor_events, so a
// marker is written with eorFixture and insertEorEvent below, not by
// setting a flag on a route row. Family is a field because Route.DumpState
// joins eor_events ON family -- a route whose family does not match a
// marker's is a route whose dump is still in progress, and a fixture that
// could not vary the family could not exercise that. It defaults to "ipv4u"
// (see insertRouteUnicastEvent), the family name the sink actually writes.
type routeUnicastFixture struct {
	RouterIP, RouterSysname, PeerIP, RIB, Family, Prefix, NextHop string
	// Collector is the collector_id this row is attributed to, defaulting to
	// defaultFixtureCollector exactly as peerEventFixture.Collector does and
	// for the same reason: only a fixture whose whole point is that two
	// collectors saw the same router has to name it. insertRIBTwoCollectorFixture
	// is the one that does -- a RIB walk is pinned to one (collector, session),
	// and nothing can show that the pin's collector half is load-bearing unless
	// a second collector's rows are sitting in the same table under the same
	// route keys.
	Collector                 string
	PeerASN                   uint32
	PeerBGPID                 string
	SessionID, Seq, StreamSeq uint64
	PathID                    uint32
	IsWithdraw                uint8
	ASPath                    []uint32
	// MED and LocalPref are POINTERS here for the reason Route's own fields
	// are: route_unicast types both columns Nullable(UInt32), and a fixture
	// that could only write 0 could not build the one row that matters --
	// an attribute present and zero beside one that is absent. nil writes
	// NULL, which is what every fixture predating them wrote implicitly.
	MED, LocalPref *uint32
	// Communities is the RAW Array(UInt32) the column stores, never the
	// "65000:100" text the contract renders it as: this is the write side,
	// and a fixture that wrote text would be testing the rendering against
	// itself. LargeCommunities IS text, because sink writes RFC 8092's own
	// canonical form into an Array(String) before the row is ever inserted.
	Communities      []uint32
	LargeCommunities []string
	// ExtCommunities and RouteTargets are the two columns route_unicast has
	// always had and no fixture could write: insertRouteUnicastEvent passed
	// []string{} for both. They are here because the community= filter
	// searches them, and a filter cannot be tested against a column no
	// fixture can populate.
	ExtCommunities, RouteTargets []string
	TsRouter, TsCollector        time.Time
	// ParseFlags is the raw parse_flags column, "" -- nil, in practice --
	// meaning no flag, the same reading every fixture written before this
	// column existed gets implicitly: insertRouteUnicastEvent used to
	// hard-code []string{} in this column's place, and a nil slice
	// here appends identically. collection_test.go's own
	// TestFlagCountsCountsEnvelopesNotRows is the first caller to set it, to
	// build several rows sharing one stream_seq (one envelope) that all
	// carry the same flag -- query.FlagCounts' whole point is that this is
	// ONE parse event, not one per row.
	ParseFlags []string
}

// insertRouteUnicastEvent writes one route_unicast row via q's own
// connection and database, in the same column order sink.insertUnicast uses
// against the same table -- see the comment above sink's insertUnicast for
// why the argument count and order there matter. The one column no query in
// this package reads (origin) is still pinned to its zero value rather than
// threaded through routeUnicastFixture; med, local_pref, communities and
// large_communities stopped being among them when Route grew the four
// fields the contract's RouteCommon requires, and ext_communities and
// route_targets stopped being among them here too, once the community=
// filter needed a fixture that could populate both (see
// TestRoutesReturnsExtCommunitiesAndRouteTargets). route_unicast has 26
// columns since end_of_rib moved to eor_events; sink's own count comment is
// the other half of this pair.
func insertRouteUnicastEvent(t *testing.T, ctx context.Context, q *Q, f routeUnicastFixture) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast insert: %v", err)
	}
	if err := b.Append(
		cmp.Or(f.Collector, defaultFixtureCollector),
		f.RouterIP, f.RouterSysname, f.PeerIP, f.RIB, f.PeerASN,
		f.PeerBGPID, f.SessionID, f.Seq, f.TsRouter, f.TsCollector,
		f.ParseFlags, f.StreamSeq,
		cmp.Or(f.Family, "ipv4u"), f.Prefix, f.PathID, f.IsWithdraw,
		uint8(0), f.ASPath, f.NextHop, f.MED, f.LocalPref,
		f.Communities, f.ExtCommunities, f.RouteTargets, f.LargeCommunities,
	); err != nil {
		t.Fatalf("append route_unicast row: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send route_unicast batch: %v", err)
	}
}

// eorFixture is the write-side shape this package's own tests need to build
// one eor_events row -- the marker counterpart to routeUnicastFixture, and
// the only way to write a marker now that end_of_rib is gone from the route
// tables.
type eorFixture struct {
	RouterIP, RouterSysname, PeerIP, RIB, Family string
	// Collector defaults to defaultFixtureCollector, as
	// routeUnicastFixture.Collector does and for the same reason.
	Collector                 string
	PeerASN                   uint32
	PeerBGPID                 string
	SessionID, Seq, StreamSeq uint64
	TsRouter, TsCollector     time.Time
}

// insertEorEvent writes one eor_events row via q's own connection and
// database, in the same column order sink's insertEor uses against the same
// table -- eor_events' 14 columns being the 13 envelope columns plus
// family. Family defaults to "ipv4u" for the same reason
// insertRouteUnicastEvent's does: the fixtures that predate per-family dump
// state are all unicast, and spelling it at every one of their call sites
// would say nothing those fixtures are about.
func insertEorEvent(t *testing.T, ctx context.Context, q *Q, f eorFixture) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".eor_events")
	if err != nil {
		t.Fatalf("prepare eor_events insert: %v", err)
	}
	if err := b.Append(
		cmp.Or(f.Collector, defaultFixtureCollector), f.RouterIP, f.RouterSysname, f.PeerIP, f.RIB, f.PeerASN,
		f.PeerBGPID, f.SessionID, f.Seq, f.TsRouter, f.TsCollector,
		[]string{}, f.StreamSeq,
		cmp.Or(f.Family, "ipv4u"),
	); err != nil {
		t.Fatalf("append eor_events row: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send eor_events batch: %v", err)
	}
}

// insertDumpStateFixture writes dumpStateFixtureRouterIP's peers through
// one session that reaches all three readings of ipv4u dump progress,
// including the one found by experiment rather than by inspection:
//
//	10.0.0.10  up, eor_events carries an ipv4u    DumpStates =
//	           marker.                             {"ipv4u": "complete"}
//	10.0.0.11  up, route_unicast carries an        DumpStates =
//	           ipv4u row but no marker.            {"ipv4u": "dumping"}
//	10.0.0.12  down, eor_events carries an ipv4u   DumpStates = {}
//	           marker -- the peer finished its
//	           dump earlier in the session, then
//	           went down.
//
// 10.0.0.12 is the load-bearing case: without it, a query that forgets to
// check peer_state.state at all would still pass every other fixture in
// this package, because insertFlappedPeerFixture's own down peer
// (10.0.0.2) writes no route or marker rows at all and therefore already
// reads an empty map via the separate "nothing on record" branch, whether
// or not the down check exists. Only a down peer that ALSO has a complete
// dump on record can tell those two branches apart --
// TestPeersReportsDumpStateForADownPeerWithACompleteDump asserts exactly
// this peer, and fails against a query with no down check at all.
//
// A real archive shows the complete/dumping split happens (end-of-RIB
// markers were 56 of one archive's 476 route rows), but every one of
// routers_test.go's and insertFlappedPeerFixture's peers writes zero route
// rows, which would leave "dumping" resting only on the left join's default
// fill and never on a peer that actually has routes but hasn't finished
// sending them.
func insertDumpStateFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "dump-state-router"
	now := time.Now().UTC()

	for peerIP, kind := range map[string]string{
		"10.0.0.10": "up", "10.0.0.11": "up", "10.0.0.12": "down",
	} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: dumpStateFixtureRouterIP.String(), RouterSysname: sysname,
			PeerIP: peerIP, RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: peerIP,
			SessionID: dumpStateFixtureSessionID, Seq: 1, StreamSeq: 1,
			Kind:        kind,
			TsRouter:    now,
			TsCollector: now,
		})
	}

	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: dumpStateFixtureRouterIP.String(), RouterSysname: sysname,
		PeerIP: "10.0.0.10", RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: "10.0.0.10",
		SessionID: dumpStateFixtureSessionID, Seq: 1, StreamSeq: 1,
		TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: dumpStateFixtureRouterIP.String(), RouterSysname: sysname,
		PeerIP: "10.0.0.11", RIB: "in_pre", Prefix: "203.0.113.0/24",
		PeerASN: 65000, PeerBGPID: "10.0.0.11",
		SessionID: dumpStateFixtureSessionID, Seq: 1, StreamSeq: 1,
		TsRouter: now, TsCollector: now,
	})
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: dumpStateFixtureRouterIP.String(), RouterSysname: sysname,
		PeerIP: "10.0.0.12", RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: "10.0.0.12",
		SessionID: dumpStateFixtureSessionID, Seq: 1, StreamSeq: 1,
		TsRouter: now, TsCollector: now,
	})
}

// routeFixtureRouterIP and its three peers are a new router, distinct
// from every router IP already claimed elsewhere in this package
// (10.0.0.1, .3, .5, .7 and .9 as routers; .2, .4, .6, .10, .11 and .12 as
// peers -- see fixtureRouterIP's and dumpStateFixtureRouterIP's own
// comments). 10.0.0.20/.21/.22/.23 sit outside all of those.
const (
	routeFixtureRouterIP     = "10.0.0.20"
	routeFixtureSysname      = "route-fixture-router"
	routeFixtureUpPeerIP     = "10.0.0.21" // stays up through both sessions below; its dump is still in progress.
	routeFixtureDownPeer     = "10.0.0.22" // up, then down, in the current session.
	routeFixtureCompletePeer = "10.0.0.23" // up in the current session, with a completed dump.

	// routeFixtureOldSession is the session routeFixtureUpPeerIP first came
	// up in; routeFixtureCurSession is the one it -- and every other peer
	// below -- are in now. cur is scoped to the whole router
	// (peerStateCTE's cur CTE groups by router_ip alone), so it is
	// routeFixtureUpPeerIP's own second up event, not anything a later
	// peer does, that advances routeFixtureRouterIP's current session from
	// old to cur.
	routeFixtureOldSession = 700
	routeFixtureCurSession = 701
)

// insertRouteFixture writes one router, three peers and ten route_unicast
// rows -- enough for every subtest TestRoutes runs against Routes, each
// row placed to make one specific wrong answer visible rather than merely
// plausible:
//
//   - 10.1.0.0/24, path-ids 1 and 2, both advertised by routeFixtureUpPeerIP
//     in the current session; path-id 1 is then withdrawn. A query that
//     groups by prefix alone, or that lets a withdrawn row's HAVING check
//     leak past its own alias, reports 0 or 2 routes here instead of the 1
//     (path-id 2) that is actually still live.
//   - 10.2.0.0/24, advertised by routeFixtureDownPeer, which then goes
//     down -- in the SAME (current) session, so this is not a stale-session
//     case. The row never leaves route_unicast; only DumpState-style
//     down-peer filtering keeps it out of Routes' answer.
//   - 10.3.0.0/24, advertised by routeFixtureUpPeerIP in the OLD session,
//     with no counterpart in the current one. routeFixtureUpPeerIP is up
//     right now, so a query that filters on peer state alone (and forgets
//     route_unicast's own session_id) reports this route as live when the
//     dump that produced it belongs to a session the router has since
//     replaced.
//   - 10.5.0.0/24, advertised twice by routeFixtureUpPeerIP in the current
//     session under path-ids 7 and 8, with different next hops and never
//     withdrawn. The obvious GROUP BY (prefix alone) collapses these to
//     one row and silently drops the other.
//   - 10.4.0.0/24, advertised once by routeFixtureUpPeerIP in the current
//     session with an empty AS path -- ordinary for an iBGP route. Origin
//     derived as as_path[-1] in SQL returns 0 for this row, indistinguishable
//     from a route that genuinely originated in AS 0.
//   - 10.6.0.0/24, advertised once by routeFixtureCompletePeer, whose
//     current session ALSO carries an ipv4u marker in eor_events.
//     routeFixtureCompletePeer is the only peer here with a marker for the
//     family its routes are in, so routeFixtureUpPeerIP's rows above only
//     ever prove DumpState reads "dumping"; this route is what proves it
//     can also read "complete", and the marker row that produces that
//     reading is what TestRoutes's "an end-of-rib marker is not a route"
//     subtest asserts never comes back from Routes(ctx, "") as a route in
//     its own right.
//
// Every advertisement's seq is 1 within its own route key; only the
// withdrawal of 10.1.0.0/24 path-id 1 needs a second seq (2) to land after
// its own advertisement. stream_seq increases across the whole function in
// the order the rows are written below, which is not the same order as
// seq resets per key -- the fixture does not depend on stream_seq tracking
// wall-clock time across different route keys, only on argMax ordering
// correctly within each one.
func insertRouteFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureOldSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 2,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureDownPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeFixtureDownPeer,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 3,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureDownPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeFixtureDownPeer,
		SessionID: routeFixtureCurSession, Seq: 2, StreamSeq: 4,
		Kind: "down", TsRouter: now, TsCollector: now,
	})

	// 10.1.0.0/24 -- a withdraw must remove only its own path-id.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.1.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 10,
		PathID: 1, NextHop: "10.9.9.11", ASPath: []uint32{65010},
		TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.1.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 11,
		PathID: 2, NextHop: "10.9.9.12", ASPath: []uint32{65010},
		TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.1.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 2, StreamSeq: 12,
		PathID: 1, IsWithdraw: 1,
		TsRouter: now, TsCollector: now,
	})

	// 10.2.0.0/24 -- advertised by a peer that then goes down, in the
	// current session. The row stays in route_unicast; Routes must not
	// report it.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureDownPeer, RIB: "in_pre", Prefix: "10.2.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureDownPeer,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 13,
		PathID: 1, NextHop: "10.9.9.20", ASPath: []uint32{65020},
		TsRouter: now, TsCollector: now,
	})

	// 10.3.0.0/24 -- advertised only in the OLD session, by a peer that is
	// up right now. The dump that produced this row belongs to a session
	// routeFixtureRouterIP has since replaced.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.3.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureOldSession, Seq: 1, StreamSeq: 5,
		PathID: 1, NextHop: "10.9.9.30", ASPath: []uint32{65030},
		TsRouter: now, TsCollector: now,
	})

	// 10.5.0.0/24 -- add-path: the same peer advertises the same prefix
	// twice, under different path-ids and different next hops, neither
	// ever withdrawn.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.5.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 14,
		PathID: 7, NextHop: "10.9.9.71",
		TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.5.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 15,
		PathID: 8, NextHop: "10.9.9.72",
		TsRouter: now, TsCollector: now,
	})

	// 10.4.0.0/24 -- an ordinary iBGP route with no AS path at all.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Prefix: "10.4.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 16,
		PathID: 1, NextHop: "10.9.9.40",
		TsRouter: now, TsCollector: now,
	})

	// A vpn4 marker for routeFixtureUpPeerIP -- a family this peer has no
	// route rows for at all, which is an ordinary shape: a peer that
	// negotiated a family with an empty table sends End-of-RIB for it and
	// nothing else. It is here to make routesSQL's `eor.fam = r.family`
	// join predicate load-bearing rather than merely present. Every route
	// row this fixture writes is ipv4u, so without the family predicate
	// this marker would match them all and the "add-path keeps both
	// path-ids" subtest's DumpState = "dumping" assertion would read
	// "complete" instead -- one family's finished dump silently declaring
	// another family's finished.
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureUpPeerIP, RIB: "in_pre", Family: "vpn4",
		PeerASN: 65000, PeerBGPID: routeFixtureUpPeerIP,
		SessionID: routeFixtureCurSession, Seq: 2, StreamSeq: 19,
		TsRouter: now, TsCollector: now,
	})

	// routeFixtureCompletePeer -- up in the current session, its dump
	// finished. Kept separate from routeFixtureUpPeerIP (whose ipv4u rows
	// above have no ipv4u marker, so on their own they can only ever prove
	// DumpState reads "dumping") so that adding a marker here cannot also
	// flip routeFixtureUpPeerIP's own DumpState.
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureCompletePeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeFixtureCompletePeer,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 6,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	// The end-of-RIB marker itself, now an eor_events row rather than a
	// route_unicast row with end_of_rib = 1 and an empty prefix. It is
	// written for the ipv4u family, matching the family every route row in
	// this fixture carries, because routesSQL joins eor ON family: a marker
	// for some other family would leave 10.6.0.0/24 reading "dumping".
	// TestRoutes's "an end-of-rib marker is not a route" subtest still
	// queries Routes(ctx, "") to prove this row never comes back as a route
	// to the empty prefix -- it cannot now, because it is not in a route
	// table at all, which is the whole point of the split.
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureCompletePeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeFixtureCompletePeer,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 17,
		TsRouter: now, TsCollector: now,
	})

	// 10.6.0.0/24 -- an ordinary route from the same peer, session and
	// family as the marker above, so eor matches for (routeFixtureRouterIP,
	// routeFixtureCompletePeer, in_pre, routeFixtureCurSession, ipv4u) and
	// this route's own DumpState reads "complete".
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeFixtureRouterIP, RouterSysname: routeFixtureSysname,
		PeerIP: routeFixtureCompletePeer, RIB: "in_pre", Prefix: "10.6.0.0/24",
		PeerASN: 65000, PeerBGPID: routeFixtureCompletePeer,
		SessionID: routeFixtureCurSession, Seq: 1, StreamSeq: 18,
		PathID: 1, NextHop: "10.9.9.60", ASPath: []uint32{65060},
		TsRouter: now, TsCollector: now,
	})
}

// vpnRouteFixtureRouterIP and vpnRouteFixtureUpPeerIP are a new
// router and peer, distinct from every router/peer IP already claimed
// elsewhere in this package (10.0.0.1/.3/.5/.7/.9/.20 as routers;
// 10.0.0.2/.4/.6/.10/.11/.12/.21/.22/.23 as peers -- see
// routeFixtureRouterIP's own comment for the addresses already claimed).
// 10.0.0.30/.31 sit outside all of them, and 900 sits outside every
// session_id already claimed (1, 2, 9, 10, 11, 100, 200, 700, 701).
//
// vpnRouteFixtureDonePeerIP (10.0.0.32) arrived with VPNRoute.DumpState and
// is deliberately a second peer rather than a marker on the first: every
// route_vpn row this fixture writes under vpnRouteFixtureUpPeerIP is vpn4 or
// lu4 with no marker of its own, so those rows can only ever prove DumpState
// reads "dumping". A marker added to that peer would flip all of them at
// once and leave "dumping" untested.
//
// vpnRouteFixtureDownPeerIP (10.0.0.33) arrived with the same change, and
// for a sharper reason: until it existed, DELETING vpnRoutesSQL's own
// `peer_up.state = 'up'` left the entire package suite green. Every VPN
// fixture in this file wrote up peers only, so "a down peer contributes
// nothing" was true of route_vpn by accident of the fixtures rather than by
// anything a test asserted -- verified by running exactly that mutation.
const (
	vpnRouteFixtureRouterIP   = "10.0.0.30"
	vpnRouteFixtureSysname    = "vpn-route-fixture-router"
	vpnRouteFixtureUpPeerIP   = "10.0.0.31"
	vpnRouteFixtureDonePeerIP = "10.0.0.32"
	vpnRouteFixtureDownPeerIP = "10.0.0.33"
	vpnRouteFixtureSessionID  = 900

	// vpnRouteFixtureDownPeerPrefix is advertised TWICE, once by the up peer
	// and once by the peer that goes down inside this session, so the
	// down-peer subtest asserts a non-zero count before the exclusion and
	// zero after. A prefix only the down peer advertised would leave that
	// subtest passing on a query that returned nothing at all.
	vpnRouteFixtureDownPeerPrefix = "192.168.15.0/24"
)

// routeVPNFixture is the write-side shape this package's own tests need to
// build one route_vpn row -- the VPN counterpart to routeUnicastFixture.
// route_vpn's 28 columns are route_unicast's 26 plus RD and Labels, which a
// plain unicast route has nowhere to put.
type routeVPNFixture struct {
	RouterIP, RouterSysname, PeerIP, RIB, Family, Prefix, RD, NextHop string
	// Collector defaults to defaultFixtureCollector, as
	// routeUnicastFixture.Collector does and for the same reason.
	Collector                 string
	PeerASN                   uint32
	PeerBGPID                 string
	SessionID, Seq, StreamSeq uint64
	PathID                    uint32
	IsWithdraw                uint8
	Labels                    []uint32
	ASPath                    []uint32
	RouteTargets              []string
	// MED, LocalPref, Communities and LargeCommunities carry exactly what
	// routeUnicastFixture's own four do, in the same representations and for
	// the same reasons -- see that struct rather than have two accounts of
	// one decision.
	MED, LocalPref        *uint32
	Communities           []uint32
	LargeCommunities      []string
	TsRouter, TsCollector time.Time
}

// insertRouteVPNEvent writes one route_vpn row via q's own connection and
// database, in the same column order sink's insertVpn uses against the
// same table -- see the comment above insertRouteUnicastEvent for why the
// argument count and order there matter. The columns no query in this
// package reads (origin, ext_communities) are pinned to their zero value
// the same way insertRouteUnicastEvent pins route_unicast's equivalents.
func insertRouteVPNEvent(t *testing.T, ctx context.Context, q *Q, f routeVPNFixture) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".route_vpn")
	if err != nil {
		t.Fatalf("prepare route_vpn insert: %v", err)
	}
	if err := b.Append(
		cmp.Or(f.Collector, defaultFixtureCollector), f.RouterIP, f.RouterSysname, f.PeerIP, f.RIB, f.PeerASN,
		f.PeerBGPID, f.SessionID, f.Seq, f.TsRouter, f.TsCollector,
		[]string{}, f.StreamSeq,
		f.Family, f.Prefix, f.PathID, f.IsWithdraw, f.RD, f.Labels,
		uint8(0), f.ASPath, f.NextHop, f.MED, f.LocalPref,
		f.Communities, []string{}, f.RouteTargets, f.LargeCommunities,
	); err != nil {
		t.Fatalf("append route_vpn row: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send route_vpn batch: %v", err)
	}
}

// insertVPNRouteFixture writes vpnRouteFixtureRouterIP's one up peer and
// nine route_vpn rows -- enough for TestVPNRoutes' subtests, each row
// placed to make one specific wrong answer visible rather than merely
// plausible:
//
//   - 192.168.1.0/24 twice, under two different route distinguishers
//     (65001:1 and 65001:2) but otherwise identical (same router, peer,
//     rib). Grouping without rd in the route key -- the obvious mistake,
//     since route_unicast's own key has nowhere to put one -- silently
//     collapses these to one row via argMax, keeping whichever RD happens
//     to win and losing the other VRF's route entirely.
//   - 192.168.9.0/24, RD-less (family lu4's own shape: rd = "") and
//     withdrawn, its labels carrying 524288 -- the RFC 3107 sentinel a
//     withdrawn labeled route's label field carries, not a real label.
//     This is deliberately the same combination a real captured archive's
//     own two sentinel rows carry (both lu4, both rd = ""), not a
//     coincidence: it is the row that proves a withdrawn route is not
//     returned at all -- vpnRoutesSQL's HAVING live_is_withdraw = 0 is
//     what drops it, and without that clause it comes back looking live.
//   - 192.168.7.0/24, an ordinary vpn4 route (rd = "65001:3") with no
//     route target at all -- the shape 34 of a real captured archive's 90
//     vpn4 rows have, predating the extended-communities decoder
//     (2026-08-10) -- and no AS path either, the shape 142 of a real
//     captured archive's 164 route_vpn rows have. A query that reaches route
//     targets via an inner join drops this row from the answer entirely;
//     one that derives origin as as_path[-1] in SQL reports it as
//     originating in the reserved AS 0. VPNRoutes must do neither.
//   - 192.168.5.0/24, RD-less (family lu4's own shape: rd = "") but
//     live, not withdrawn -- distinct from 192.168.9.0/24 above
//     specifically so the RD-less family still has a row VPNRoutes
//     returns, for the subtest that inspects RD on the result rather than
//     just its length.
//   - 192.168.11.0/24, an ordinary vpn4 route that is NOT withdrawn
//     (is_withdraw = 0) but whose labels carry 524288 anyway -- a
//     combination a real captured archive never shows (there, the sentinel
//     and is_withdraw = 1 are the same 2 rows), manufactured to prove
//     label()'s stripping is a real, independent guard and not dead code
//     now that HAVING live_is_withdraw = 0 handles the archive's own case.
//   - 192.168.6.0/24, an ordinary vpn4 route with a three-hop AS path
//     (65010, 65020, 65099) -- the counterpart to 192.168.7.0/24's empty
//     one, proving OriginASN resolves to the path's last hop (65099), not
//     its first (65010) and not an unconditional 0.
//   - 192.168.12.0/24, an ordinary vpn4 route whose labels array is
//     genuinely empty -- []uint32{}, not a missing field defaulting to it.
//     A real captured archive has zero route_vpn rows shaped this way, so
//     this fixture is the only thing that can exercise it at all: without it,
//     labels[1]'s out-of-range zero value and a real implicit-null label
//     (0) are the same bit pattern with nothing to tell them apart, the
//     exact ambiguity Label/HasLabel exists to remove (see label()'s own
//     doc comment).
//   - 192.168.13.0/24 twice, under the same RD (65001:11) but two
//     different path-ids (1 and 2) -- VPNRoutes' own add-path case, the
//     counterpart to TestRoutes' "add-path keeps both path-ids" subtest.
//     Grouping without path_id in the route key -- possible here in a way
//     it is not for 192.168.1.0/24's two-RD case above, since these two
//     rows share every other column -- would collapse the pair to one row
//     via argMax, and even a query that groups correctly but does not
//     select rib/path_id back out would return two rows identical in
//     every field VPNRoute exposes (see VPNRoute's own doc comment).
func insertVPNRouteFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	// 192.168.1.0/24 -- one prefix, two VRFs.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.1.0/24", RD: "65001:1",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 10,
		NextHop: "10.9.30.1", Labels: []uint32{24001},
		RouteTargets: []string{"65001:100"},
		TsRouter:     now, TsCollector: now,
	})
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.1.0/24", RD: "65001:2",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 11,
		NextHop: "10.9.30.2", Labels: []uint32{24002},
		RouteTargets: []string{"65001:200"},
		TsRouter:     now, TsCollector: now,
	})

	// 192.168.9.0/24 -- RD-less and withdrawn: the same combination a real
	// captured archive's own two sentinel rows carry. VPNRoutes must return
	// nothing for this prefix.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "lu4", Prefix: "192.168.9.0/24", RD: "",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 12,
		IsWithdraw: 1, Labels: []uint32{withdrawSentinel},
		TsRouter: now, TsCollector: now,
	})

	// 192.168.7.0/24 -- an ordinary vpn4 route with no route target and no
	// AS path.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.7.0/24", RD: "65001:3",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 13,
		NextHop: "10.9.30.7", Labels: []uint32{24007},
		TsRouter: now, TsCollector: now,
	})

	// 192.168.5.0/24 -- RD-less but live: the RD-less family's own
	// counterpart to 192.168.9.0/24, present in VPNRoutes' answer rather
	// than filtered out by it.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "lu4", Prefix: "192.168.5.0/24", RD: "",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 14,
		NextHop: "10.9.30.5", Labels: []uint32{24005},
		TsRouter: now, TsCollector: now,
	})

	// 192.168.11.0/24 -- carries the withdraw sentinel in its labels but
	// is NOT withdrawn: the one shape that still reaches label()'s
	// stripping once HAVING live_is_withdraw = 0 handles every row a real
	// captured archive itself carries the sentinel on.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.11.0/24", RD: "65001:4",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 15,
		NextHop: "10.9.30.11", Labels: []uint32{withdrawSentinel},
		TsRouter: now, TsCollector: now,
	})

	// 192.168.6.0/24 -- a three-hop AS path, so origin resolves to its
	// last hop (65099) rather than its first (65010) or an unconditional 0.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.6.0/24", RD: "65001:6",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 16,
		NextHop: "10.9.30.6", Labels: []uint32{24006},
		ASPath:   []uint32{65010, 65020, 65099},
		TsRouter: now, TsCollector: now,
	})

	// 192.168.12.0/24 -- a genuinely empty labels array, not merely an
	// unset field defaulting to one: the archive has no such row, so this
	// fixture alone can prove Label/HasLabel tells "no label" apart from
	// the implicit-null label 0.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.12.0/24", RD: "65001:9",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 17,
		NextHop: "10.9.30.12", Labels: []uint32{},
		TsRouter: now, TsCollector: now,
	})

	// 192.168.13.0/24 -- add-path: the same peer, RD and prefix advertised
	// twice under two different path-ids, neither ever withdrawn.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.13.0/24", RD: "65001:11",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 18,
		PathID: 1, NextHop: "10.9.30.131", Labels: []uint32{24013},
		TsRouter: now, TsCollector: now,
	})
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.13.0/24", RD: "65001:11",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureUpPeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 19,
		PathID: 2, NextHop: "10.9.30.132", Labels: []uint32{24013},
		TsRouter: now, TsCollector: now,
	})

	// vpnRouteFixtureDonePeerIP -- up in the current session with a vpn4
	// marker on record, so 192.168.14.0/24 below reads DumpState =
	// "complete" where every row above reads "dumping". See this fixture's
	// own constants for why the marker is on a second peer rather than on
	// the one that carries the other nine rows.
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureDonePeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureDonePeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 2,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureDonePeerIP, RIB: "in_pre", Family: "vpn4",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureDonePeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 2, StreamSeq: 20,
		TsRouter: now, TsCollector: now,
	})
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
		PeerIP: vpnRouteFixtureDonePeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "192.168.14.0/24", RD: "65001:14",
		PeerASN: 65000, PeerBGPID: vpnRouteFixtureDonePeerIP,
		SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: 21,
		NextHop: "10.9.30.14", Labels: []uint32{24014},
		TsRouter: now, TsCollector: now,
	})

	// vpnRouteFixtureDownPeerIP -- up, then down at a higher seq, inside the
	// SAME (current) session, so its route below is not a stale-session case.
	// The row never leaves route_vpn; only the peer_up gate keeps it out of
	// VPNRoutes' answer. See this fixture's own constants for what deleting
	// that gate used to cost: nothing at all.
	for _, e := range []struct {
		kind    string
		seq, ss uint64
	}{{"up", 1, 3}, {"down", 2, 4}} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
			PeerIP: vpnRouteFixtureDownPeerIP, RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: vpnRouteFixtureDownPeerIP,
			SessionID: vpnRouteFixtureSessionID, Seq: e.seq, StreamSeq: e.ss,
			Kind: e.kind, TsRouter: now, TsCollector: now,
		})
	}
	for i, p := range []string{vpnRouteFixtureUpPeerIP, vpnRouteFixtureDownPeerIP} {
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: vpnRouteFixtureRouterIP, RouterSysname: vpnRouteFixtureSysname,
			PeerIP: p, RIB: "in_pre",
			Family: "vpn4", Prefix: vpnRouteFixtureDownPeerPrefix, RD: "65001:15",
			PeerASN: 65000, PeerBGPID: p,
			SessionID: vpnRouteFixtureSessionID, Seq: 1, StreamSeq: uint64(22 + i),
			NextHop: "10.9.30.15", Labels: []uint32{24015},
			TsRouter: now, TsCollector: now,
		})
	}
}

// duplicateRouteFixtureRouterIP and duplicateRouteFixtureUpPeerIP are a
// new router and peer -- distinct from every router/peer IP already
// claimed elsewhere in this package (10.0.0.1/.3/.5/.7/.9/.20/.30 as
// routers; 10.0.0.2/.4/.6/.10/.11/.12/.21/.22/.23/.31 as peers -- see
// vpnRouteFixtureRouterIP's own comment for the addresses already
// claimed). Deliberately its own router rather than a reuse of
// fixtureRouterIP (a call to Peers(ctx, fixtureRouterIP) after calling
// this fixture would be wrong to copy literally: that
// router IP already belongs to insertFlappedPeerFixture's session
// flappedPeerSessionID, and chtest shares one database across every test in
// this package (see this file's own package doc), so writing a second,
// higher session_id under the same router_ip here would silently steal
// "current" away from insertFlappedPeerFixture's peers by way of
// peerStateCTE's max(session_id) -- exactly the cross-fixture corruption
// a route count is most exposed to.
const (
	duplicateRouteFixtureRouterIP  = "10.0.0.40"
	duplicateRouteFixtureSysname   = "duplicate-route-router"
	duplicateRouteFixtureUpPeerIP  = "10.0.0.41"
	duplicateRouteFixtureSessionID = 1000
	// duplicateRouteFixtureCount is the number of distinct route keys
	// insertDuplicateRouteFixture writes, and the number
	// TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent asserts
	// Peer.Routes reports despite each one landing in route_unicast twice.
	duplicateRouteFixtureCount = 100
)

// insertDuplicateRouteFixture writes one up peer and
// duplicateRouteFixtureCount distinct route_unicast route keys, each
// inserted as two byte-identical rows -- same seq, same stream_seq, same
// timestamps, same everything -- exactly the shape a JetStream redelivery
// of the same envelope produces (see this package's own doc comment and
// schema.sql's ReplacingMergeTree note). ReplacingMergeTree will eventually
// collapse each pair back to one row at merge time; the caller is
// responsible for disabling merges on route_unicast before calling this
// (see TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent) and for
// asserting count() != uniqExact() actually holds afterward, rather than
// trusting that stopping merges was enough on its own.
func insertDuplicateRouteFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: duplicateRouteFixtureRouterIP, RouterSysname: duplicateRouteFixtureSysname,
		PeerIP: duplicateRouteFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: duplicateRouteFixtureUpPeerIP,
		SessionID: duplicateRouteFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	for i := range duplicateRouteFixtureCount {
		prefix := fmt.Sprintf("10.200.%d.0/24", i)
		row := routeUnicastFixture{
			RouterIP: duplicateRouteFixtureRouterIP, RouterSysname: duplicateRouteFixtureSysname,
			PeerIP: duplicateRouteFixtureUpPeerIP, RIB: "in_pre", Prefix: prefix,
			PeerASN: 65000, PeerBGPID: duplicateRouteFixtureUpPeerIP,
			SessionID: duplicateRouteFixtureSessionID, Seq: 1, StreamSeq: uint64(10 + i),
			PathID: 0, NextHop: "10.9.40.1", ASPath: []uint32{65040},
			TsRouter: now, TsCollector: now,
		}
		// Two calls, identical fixture value: this is the "byte-identical
		// redelivery" this fixture exists to manufacture, not two different
		// routes that happen to share a prefix.
		insertRouteUnicastEvent(t, ctx, q, row)
		insertRouteUnicastEvent(t, ctx, q, row)
	}
}

// routeCountFixtureRouterIP and its three peers are a second, separate
// fixture, isolated from insertDuplicateRouteFixture's own router (that
// one exists solely to manufacture unmerged duplicates under SYSTEM STOP
// MERGES, and mixing a marker/withdraw/down-peer concern into it would
// leave a future failure ambiguous about which behavior broke -- the same
// reasoning dumpStateFixtureRouterIP's own comment gives for staying
// separate from fixtureRouterIP). 10.0.0.42/.43/.44/.45 sit outside every
// router/peer IP already claimed in this package (see
// duplicateRouteFixtureRouterIP's own comment for the addresses already
// claimed), and 1001 sits outside every session_id already claimed
// (1, 2, 9, 10, 11, 100, 200, 700, 701, 900, 1000).
const (
	routeCountFixtureRouterIP = "10.0.0.42"
	routeCountFixtureSysname  = "route-count-edge-router"
	// routeCountFixtureMarkerPeer carries two live routes and one
	// end-of-rib marker. Peer.Routes must report 2, not 3: a marker is a
	// protocol sentinel, not a route. The marker now sits in eor_events
	// rather than in route_unicast (56 of a real captured archive's 476
	// route_unicast rows were markers before the split), so this peer also
	// stands for the family universe -- its DumpStates reads
	// {"ipv4u": "complete"}.
	routeCountFixtureMarkerPeer = "10.0.0.43"
	// routeCountFixtureWithdrawPeer carries one live route and one route
	// that was advertised and then withdrawn. Peer.Routes must report 1,
	// not 2: withdrawing a route does not delete its earlier row from
	// route_unicast (see routesSQL's own doc comment on live_is_withdraw),
	// so a count that does not resolve each route key's newest observation
	// reports a route that is no longer there.
	routeCountFixtureWithdrawPeer = "10.0.0.44"
	// routeCountFixtureDownPeer carries one route_unicast row from before
	// it went down, in its current session. Peer.Routes must report 0: a
	// down peer contributes nothing, the same rule TestRoutes's "a down
	// peer contributes nothing" subtest already enforces for Routes(ctx,
	// prefix).
	routeCountFixtureDownPeer  = "10.0.0.45"
	routeCountFixtureSessionID = 1001
)

// insertRouteCountFixture writes routeCountFixtureRouterIP's three peers
// and their route_unicast rows -- see each peer constant's own comment
// above for what its rows are shaped to prove.
func insertRouteCountFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureMarkerPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeCountFixtureMarkerPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureWithdrawPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeCountFixtureWithdrawPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 2,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureDownPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeCountFixtureDownPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 3,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureDownPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeCountFixtureDownPeer,
		SessionID: routeCountFixtureSessionID, Seq: 2, StreamSeq: 4,
		Kind: "down", TsRouter: now, TsCollector: now,
	})

	// routeCountFixtureMarkerPeer: two live routes...
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureMarkerPeer, RIB: "in_pre", Prefix: "10.201.0.0/24",
		PeerASN: 65000, PeerBGPID: routeCountFixtureMarkerPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 10,
		PathID: 1, NextHop: "10.9.42.1", ASPath: []uint32{65042},
		TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureMarkerPeer, RIB: "in_pre", Prefix: "10.201.1.0/24",
		PeerASN: 65000, PeerBGPID: routeCountFixtureMarkerPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 11,
		PathID: 1, NextHop: "10.9.42.2", ASPath: []uint32{65042},
		TsRouter: now, TsCollector: now,
	})
	// ...and one end-of-rib marker, in eor_events rather than in
	// route_unicast: Peer.Routes must still report 2, and now cannot report
	// 3 by forgetting a predicate, because the marker is not in the table
	// being counted at all.
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureMarkerPeer, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: routeCountFixtureMarkerPeer,
		SessionID: routeCountFixtureSessionID, Seq: 2, StreamSeq: 12,
		TsRouter: now, TsCollector: now,
	})

	// routeCountFixtureWithdrawPeer: one live route...
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureWithdrawPeer, RIB: "in_pre", Prefix: "10.201.2.0/24",
		PeerASN: 65000, PeerBGPID: routeCountFixtureWithdrawPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 13,
		PathID: 1, NextHop: "10.9.42.3", ASPath: []uint32{65042},
		TsRouter: now, TsCollector: now,
	})
	// ...one route advertised...
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureWithdrawPeer, RIB: "in_pre", Prefix: "10.201.3.0/24",
		PeerASN: 65000, PeerBGPID: routeCountFixtureWithdrawPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 14,
		PathID: 1, NextHop: "10.9.42.4", ASPath: []uint32{65042},
		TsRouter: now, TsCollector: now,
	})
	// ...then withdrawn at a later seq. The advertise row above stays in
	// route_unicast -- nothing deletes it -- so a count that does not
	// resolve to the newest observation per route key still sees it.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureWithdrawPeer, RIB: "in_pre", Prefix: "10.201.3.0/24",
		PeerASN: 65000, PeerBGPID: routeCountFixtureWithdrawPeer,
		SessionID: routeCountFixtureSessionID, Seq: 2, StreamSeq: 15,
		PathID: 1, IsWithdraw: 1,
		TsRouter: now, TsCollector: now,
	})

	// routeCountFixtureDownPeer: one route advertised before the peer went
	// down, in the same (current) session -- the row never leaves
	// route_unicast; only the down check keeps it out of the count.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routeCountFixtureRouterIP, RouterSysname: routeCountFixtureSysname,
		PeerIP: routeCountFixtureDownPeer, RIB: "in_pre", Prefix: "10.201.4.0/24",
		PeerASN: 65000, PeerBGPID: routeCountFixtureDownPeer,
		SessionID: routeCountFixtureSessionID, Seq: 1, StreamSeq: 16,
		PathID: 1, NextHop: "10.9.42.5", ASPath: []uint32{65042},
		TsRouter: now, TsCollector: now,
	})
}

// postPolicyRouteFixtureRouterIP and postPolicyRouteFixtureUpPeerIP are
// the regression fixture for the rib-scoped join defect
// insertPostPolicyRouteFixture's own doc comment describes -- distinct
// from every router/peer IP already claimed elsewhere in this package
// (10.0.0.1/.3/.5/.7/.9/.20/.30/.40/.42 as routers;
// 10.0.0.2/.4/.6/.10/.11/.12/.21/.22/.23/.31/.41/.43/.44/.45 as peers -- see
// routeCountFixtureRouterIP's own comment for the addresses already
// claimed). 10.0.0.50/.51 sit outside all of them, and 1100 sits outside
// every session_id already claimed (1, 2, 9, 10, 11, 100, 200, 700, 701, 900,
// 1000, 1001).
const (
	postPolicyRouteFixtureRouterIP  = "10.0.0.50"
	postPolicyRouteFixtureSysname   = "post-policy-router"
	postPolicyRouteFixtureUpPeerIP  = "10.0.0.51"
	postPolicyRouteFixturePrefix    = "10.250.0.0/24"
	postPolicyRouteFixtureVPNPrefix = "192.168.250.0/24"
	postPolicyRouteFixtureSessionID = 1100
)

// insertPostPolicyRouteFixture writes one router, one peer and one
// route_unicast row, reproducing exactly a shape observed in a real
// deployment (verified 2026-08-24): the peer's own peer_events row is up
// under rib = 'in_pre' -- the only rib peer_events has any accounting for,
// for this peer -- and its route_unicast row is under rib = 'in_post', in
// the SAME current session. In that observed case the pattern sat in a
// session that had already been superseded, where a rib-scoped peer_state
// join asks the wrong question only about a session nothing queries any
// more, so the defect cannot show. This fixture places the identical shape
// in the router's CURRENT session, the one thing a real captured archive
// cannot do, so that Routes actually has to answer for it.
//
// The peer is unambiguously up: its only peer_events row says so, and there
// is no down event anywhere in this fixture to make the fixture's own
// claim about the peer's state ambiguous. A query that joins the route to
// peer_state ON rib finds no peer_state row for (this router, this peer,
// in_post) at all -- peer_state has no in_post row for a peer whose
// peer_events only ever recorded in_pre -- and drops the route as an
// unmatched INNER JOIN, indistinguishable from "the peer is down" even
// though nothing about this fixture says that.
func insertPostPolicyRouteFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: postPolicyRouteFixtureRouterIP, RouterSysname: postPolicyRouteFixtureSysname,
		PeerIP: postPolicyRouteFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: postPolicyRouteFixtureUpPeerIP,
		SessionID: postPolicyRouteFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: postPolicyRouteFixtureRouterIP, RouterSysname: postPolicyRouteFixtureSysname,
		PeerIP: postPolicyRouteFixtureUpPeerIP, RIB: "in_post", Prefix: postPolicyRouteFixturePrefix,
		PeerASN: 65000, PeerBGPID: postPolicyRouteFixtureUpPeerIP,
		SessionID: postPolicyRouteFixtureSessionID, Seq: 1, StreamSeq: 2,
		PathID: 1, NextHop: "10.9.50.1", ASPath: []uint32{65050},
		TsRouter: now, TsCollector: now,
	})

	// The same shape in route_vpn, added when VPNRoute grew DumpState and
	// vpnRoutesSQL grew the peer_state join that made this hazard reachable
	// on a second table. The unicast row above is what proves Routes
	// survives it; this one is what proves VPNRoutes does, and neither can
	// stand in for the other -- the two statements are separate SQL with
	// separate joins, and they have already diverged twice (see
	// TestVPNRoutesFiltersByRouterPeerAndRib's own doc comment). It does not
	// disturb the Peers assertions this fixture also carries: peer_state has
	// no in_post row for this peer at all, so nothing this row adds to
	// dump_families can reach Peers' output, and Peer.Routes counts unicast
	// route keys only.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: postPolicyRouteFixtureRouterIP, RouterSysname: postPolicyRouteFixtureSysname,
		PeerIP: postPolicyRouteFixtureUpPeerIP, RIB: "in_post",
		Family: "vpn4", Prefix: postPolicyRouteFixtureVPNPrefix, RD: "65050:1",
		PeerASN: 65000, PeerBGPID: postPolicyRouteFixtureUpPeerIP,
		SessionID: postPolicyRouteFixtureSessionID, Seq: 1, StreamSeq: 3,
		PathID: 1, NextHop: "10.9.50.1", Labels: []uint32{24050},
		ASPath: []uint32{65050}, TsRouter: now, TsCollector: now,
	})
}

// perFamilyDumpFixtureRouterIP and perFamilyDumpFixtureUpPeerIP are a new
// router and peer -- distinct from every router/peer IP already
// claimed elsewhere in this package (10.0.0.1/.3/.5/.7/.9/.20/.30/.40/.42/.50
// as routers; 10.0.0.2/.4/.6/.10/.11/.12/.21/.22/.23/.31/.41/.43/.44/.45/.51
// as peers -- see postPolicyRouteFixtureRouterIP's own comment for the
// addresses already claimed). 10.0.0.60/.61 sit outside all of them, and
// 1200 sits outside every session_id already claimed (1, 2, 9, 10, 11, 100, 200,
// 700, 701, 900, 1000, 1001, 1100).
const (
	perFamilyDumpFixtureRouterIP  = "10.0.0.60"
	perFamilyDumpFixtureSysname   = "per-family-dump-router"
	perFamilyDumpFixtureUpPeerIP  = "10.0.0.61"
	perFamilyDumpFixtureSessionID = 1200
)

// routeEVPNFixture is the write-side shape this package's own tests need to
// build one route_evpn row -- the EVPN counterpart to routeUnicastFixture
// and routeVPNFixture. route_evpn's 33 columns are the widest of the three
// route tables: it carries no family (an EVPN table holds exactly one) but
// eight NLRI columns where route_unicast has one prefix.
//
// It replaced a fifteen-argument positional helper that hard-coded route
// type 5 and left mac, ip, gateway_ip, ethernet_tag, esi and labels at
// their zero values. That was the right shape while the only caller was
// insertPerFamilyDumpFixture, which needed a route_evpn row to exist and
// did not care what was in it. TestEVPNRoutes cares about every one of
// those columns -- they are the route key -- and a positional call with
// eight empty strings in a row is exactly the shape a transposed pair hides
// in.
type routeEVPNFixture struct {
	RouterIP, RouterSysname, PeerIP, RIB string
	// Collector defaults to defaultFixtureCollector, as
	// routeUnicastFixture.Collector does and for the same reason.
	Collector                           string
	RouteType                           uint8
	RD, Prefix, MAC, IP, GatewayIP, ESI string
	EthernetTag                         uint32
	NextHop                             string
	PeerASN                             uint32
	PeerBGPID                           string
	SessionID, Seq, StreamSeq           uint64
	PathID                              uint32
	IsWithdraw                          uint8
	Labels                              []uint32
	ASPath                              []uint32
	RouteTargets                        []string
	// The same four routeUnicastFixture carries, in the same representations
	// and for the same reasons -- see that struct.
	MED, LocalPref        *uint32
	Communities           []uint32
	LargeCommunities      []string
	TsRouter, TsCollector time.Time
}

// insertRouteEVPNEvent writes one route_evpn row via q's own connection and
// database, in the same column order sink's insertEvpn uses against the
// same table -- see the comment above insertRouteUnicastEvent for why the
// argument count and order there matter. The columns no query in this
// package reads (origin, ext_communities) are pinned to their zero value
// the same way the other two helpers pin their tables' equivalents.
func insertRouteEVPNEvent(t *testing.T, ctx context.Context, q *Q, f routeEVPNFixture) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".route_evpn")
	if err != nil {
		t.Fatalf("prepare route_evpn insert: %v", err)
	}
	if err := b.Append(
		cmp.Or(f.Collector, defaultFixtureCollector), f.RouterIP, f.RouterSysname, f.PeerIP, f.RIB, f.PeerASN,
		f.PeerBGPID, f.SessionID, f.Seq, f.TsRouter, f.TsCollector,
		[]string{}, f.StreamSeq,
		f.RouteType, f.RD, f.Prefix, f.MAC, f.IP, f.GatewayIP, f.EthernetTag,
		f.ESI, f.Labels, f.PathID, f.IsWithdraw,
		uint8(0), f.ASPath, f.NextHop, f.MED, f.LocalPref,
		f.Communities, []string{}, f.RouteTargets, f.LargeCommunities,
	); err != nil {
		t.Fatalf("append route_evpn row: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send route_evpn batch: %v", err)
	}
}

// insertPerFamilyDumpFixture writes one router and one up peer whose
// current session reaches all three shapes Peer.DumpStates has to tell
// apart, in three different families at once:
//
//	evpn   route_evpn rows AND an evpn marker    "complete"
//	       in eor_events.
//	vpn4   route_vpn rows, no marker.            "dumping"
//	ipv4u  nothing at all -- no route row, no    ABSENT from the map
//	       marker.
//
// This is the payoff of the eor_events split, and the fixture is shaped to
// make it visible rather than merely available. Before the split, end_of_rib
// existed only on route_unicast, so this peer -- which has no unicast rows
// whatsoever -- would have read "unknown" no matter how complete its EVPN
// dump was. There was no way to ask the question, so the answer was always
// an honest shrug.
//
// All three families are on ONE peer rather than three, and that is
// load-bearing: a per-peer fixture would pass against a query that resolves
// dump progress per (router, peer, rib) and merely labels the result with
// whichever family it happened to find. Only a peer carrying two families
// in different states at the same instant can tell "per family" apart from
// "per peer, labeled with a family".
//
// ipv4u's absence is the third assertion and the one with no other way to
// be made: a map entry reading "unknown" would be a claim about a dump this
// session never started, manufactured out of the family simply existing in
// the protocol. The families this peer never carried are not part-way
// through anything.
func insertPerFamilyDumpFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: perFamilyDumpFixtureRouterIP, RouterSysname: perFamilyDumpFixtureSysname,
		PeerIP: perFamilyDumpFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: perFamilyDumpFixtureUpPeerIP,
		SessionID: perFamilyDumpFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	// evpn: two route_evpn rows and the marker that closes their dump.
	for i, prefix := range []string{"192.0.2.0/24", "192.0.2.128/25"} {
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: perFamilyDumpFixtureRouterIP, RouterSysname: perFamilyDumpFixtureSysname,
			PeerIP: perFamilyDumpFixtureUpPeerIP, RIB: "in_pre",
			RouteType: 5, RD: "65001:100", Prefix: prefix,
			PeerASN: 65000, PeerBGPID: perFamilyDumpFixtureUpPeerIP,
			SessionID: perFamilyDumpFixtureSessionID, Seq: 1, StreamSeq: uint64(2 + i),
			TsRouter: now, TsCollector: now,
		})
	}
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: perFamilyDumpFixtureRouterIP, RouterSysname: perFamilyDumpFixtureSysname,
		PeerIP: perFamilyDumpFixtureUpPeerIP, RIB: "in_pre", Family: "evpn",
		PeerASN: 65000, PeerBGPID: perFamilyDumpFixtureUpPeerIP,
		SessionID: perFamilyDumpFixtureSessionID, Seq: 2, StreamSeq: 4,
		TsRouter: now, TsCollector: now,
	})

	// evpn's marker is written TWICE, byte-identical, as a JetStream
	// redelivery would leave it. eor_events is a plain MergeTree with no
	// deduplication (see deploy/clickhouse/schema.sql), so this second row
	// is permanent -- nothing will ever collapse it. A dump-progress query
	// that counts markers instead of asking whether one exists reads 2 here
	// and has to decide what 2 means; this fixture exists so that a query
	// which starts caring has somewhere to fail. See peersSQL's own
	// max(is_marker): a count of markers is a count of deliveries, a
	// collection artifact, not a fact about the peer.
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: perFamilyDumpFixtureRouterIP, RouterSysname: perFamilyDumpFixtureSysname,
		PeerIP: perFamilyDumpFixtureUpPeerIP, RIB: "in_pre", Family: "evpn",
		PeerASN: 65000, PeerBGPID: perFamilyDumpFixtureUpPeerIP,
		SessionID: perFamilyDumpFixtureSessionID, Seq: 2, StreamSeq: 4,
		TsRouter: now, TsCollector: now,
	})

	// vpn4: a route, and deliberately no marker.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: perFamilyDumpFixtureRouterIP, RouterSysname: perFamilyDumpFixtureSysname,
		PeerIP: perFamilyDumpFixtureUpPeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: "198.51.100.0/24", RD: "65001:200",
		PeerASN: 65000, PeerBGPID: perFamilyDumpFixtureUpPeerIP,
		SessionID: perFamilyDumpFixtureSessionID, Seq: 1, StreamSeq: 5,
		NextHop: "10.9.60.1", Labels: []uint32{24060},
		TsRouter: now, TsCollector: now,
	})

	// ipv4u: nothing. Not a withdrawn route, not an empty-prefix row --
	// nothing at all, which is the only shape that makes "absent" different
	// from "dumping".
}

// evpnOnlyDumpFixture* is the EVPN-only peer insertPerFamilyDumpFixture
// cannot be: its own evpn family carries a marker, so its evpn entry
// reaches the map through eor_events whether or not peersSQL ever looks at
// route_evpn at all. This peer has route_evpn rows and NO marker, which is
// the one shape that requires the route_evpn branch of dump_families' union
// to exist -- and, because route_evpn has no family column, the one shape
// that requires the literal 'evpn' that branch supplies to be spelled the
// same way sink spells it in eor_events. Get either wrong and this peer
// reports an empty map: an EVPN-only peer stuck at "no idea", which is
// exactly the state eor_events was split out to end.
//
// Distinct from every router/peer IP already claimed elsewhere in this
// package (see perFamilyDumpFixtureRouterIP's own comment for the
// addresses already claimed): 10.0.0.62/.63 sit outside all of them, and
// 1300 sits outside every session_id already claimed (1, 2, 9, 10, 11,
// 100, 200, 700, 701, 900, 1000, 1001, 1100, 1200).
const (
	evpnOnlyDumpFixtureRouterIP  = "10.0.0.62"
	evpnOnlyDumpFixtureSysname   = "evpn-only-dump-router"
	evpnOnlyDumpFixtureUpPeerIP  = "10.0.0.63"
	evpnOnlyDumpFixtureSessionID = 1300
)

// insertEvpnOnlyDumpFixture writes one router and one up peer whose current
// session carries a single route_evpn row and nothing else -- no marker, no
// unicast, no VPN. See the constants above for what that shape is for.
func insertEvpnOnlyDumpFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: evpnOnlyDumpFixtureRouterIP, RouterSysname: evpnOnlyDumpFixtureSysname,
		PeerIP: evpnOnlyDumpFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: evpnOnlyDumpFixtureUpPeerIP,
		SessionID: evpnOnlyDumpFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
		RouterIP: evpnOnlyDumpFixtureRouterIP, RouterSysname: evpnOnlyDumpFixtureSysname,
		PeerIP: evpnOnlyDumpFixtureUpPeerIP, RIB: "in_pre",
		RouteType: 5, RD: "65001:300", Prefix: "203.0.113.0/24",
		PeerASN: 65000, PeerBGPID: evpnOnlyDumpFixtureUpPeerIP,
		SessionID: evpnOnlyDumpFixtureSessionID, Seq: 1, StreamSeq: 2,
		TsRouter: now, TsCollector: now,
	})
}

// The two-collector fixture's identifiers. Distinct from every router,
// peer and session_id already claimed elsewhere in this package (see
// evpnOnlyDumpFixture's own comment for the addresses already claimed):
// 10.0.197.1 and 10.255.197.x sit outside every 10.0.0.x this package
// already uses, and 9701/9702 sit outside every session_id already
// claimed (1, 2, 9, 10, 11, 100, 200, 700, 701, 900, 1000, 1001, 1100, 1200, 1300).
const (
	tcRouter    = "10.0.197.1"
	tcSysname   = "tc-rtr"
	tcPeerC1    = "10.255.197.1"
	tcPeerC2    = "10.255.197.2"
	tcPeerStale = "10.255.197.3"
	tcSessionC1 = 9701
	// Larger than tcSessionC1 on purpose: see
	// TestRoutersDoesNotDropASecondCollectorsView.
	tcSessionC2 = 9702
	// tcSessionStaleC2 is deliberately EQUAL to tcSessionC1, and that
	// equality is the whole point of tcPeerStale's row -- see
	// insertTwoCollectorFixture's doc comment.
	tcSessionStaleC2 = tcSessionC1
)

// insertTwoCollectorFixture writes one router monitored by two collectors,
// each with its own session and its own up peer. It is the only way to
// exercise per-collector session scoping: a real captured archive has
// exactly one collector (11 routers, 373 sessions), so this case asserts
// nothing against real data and this fixture is the only thing standing
// between the defect and production.
//
//	dev-c1  session 9701  10.255.197.1 up   c1's current session.
//	dev-c2  session 9702  10.255.197.2 up   c2's current session.
//	dev-c2  session 9701  10.255.197.3 up   c2's PREVIOUS session, whose
//	                                        id collides with c1's current
//	                                        one.
//
// The first two rows are what make the primary defect visible: session_id
// is now().UnixNano() assigned independently by each collector (see
// collector.Server.nextSessionID), so a cur CTE that takes
// max(session_id) per router alone resolves this router to 9702 and drops
// every peer_events row dev-c1 ever wrote for it -- no error, no empty
// result, just a fleet one collector's view smaller. 9702 > 9701 is
// deliberate so that the wrong answer is c2's row surviving and c1's
// vanishing, rather than both surviving by luck of ordering.
//
// The third row is what makes the join predicate itself load-bearing
// rather than incidentally satisfied, and it needs its own justification
// because the collision looks contrived. Nothing anywhere makes session_id
// unique ACROSS collectors: nextSessionID is monotonic within one process,
// against that process's own clock, and two collectors are two processes
// with two clocks and no shared counter -- and this repo has already
// written down that the collision it guards against is ordinary:
// Server.lastSessionID exists because two UnixNano() calls landing on the
// same value "happen routinely under a coarse or fake clock and is not
// vanishingly rare even under a real one on fast hardware or a busy
// reconnect storm" (collector/server.go), and that guard is an in-process
// atomic, which a second collector process is not behind. So "the same
// session_id under two collectors for the same router" is not a fantasy,
// it is an undefended case rather than an impossible one -- and a
// peer_state join scoped to (router, session) alone is correct only while
// that assumption holds. With dev-c2's superseded session carrying the
// same 9701 as dev-c1's current one, dropping collector_id from
// peer_state's join to cur lets that dead session's peer join dev-c1's cur
// row and reappear in dev-c2's current-state answer:
// PeersUp = 2 for a collector that has one peer up. Its peer is up, not
// down, because that is how a BMP session usually ends -- the TCP
// connection drops with every peer still up and no peer-down event for any
// of them, which is exactly the stale-session reading peerStateCTE exists
// to prevent.
func insertTwoCollectorFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	type view struct {
		collector string
		peerIP    string
		sessionID uint64
		streamSeq uint64
	}
	// Each row gets its own peer_ip and its own stream_seq. peer_events is
	// a ReplacingMergeTree ordered by (router_ip, peer_ip, rib, ts_router,
	// stream_seq) -- collector_id is NOT in that key -- so two collectors'
	// rows that agreed on all five would be free to collapse into one,
	// which would make this fixture assert against whichever row survived
	// a merge rather than against the query.
	for _, v := range []view{
		{"dev-c1", tcPeerC1, tcSessionC1, 1},
		{"dev-c2", tcPeerC2, tcSessionC2, 2},
		{"dev-c2", tcPeerStale, tcSessionStaleC2, 3},
	} {
		ts := now.Add(time.Duration(v.streamSeq) * time.Second)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: tcRouter, RouterSysname: tcSysname,
			PeerIP: v.peerIP, RIB: "in_pre",
			Collector: v.collector,
			PeerASN:   65000, PeerBGPID: v.peerIP,
			SessionID: v.sessionID, Seq: 1, StreamSeq: v.streamSeq,
			Kind: "up", TsRouter: ts, TsCollector: ts,
		})
	}
}

// The route-filter fixture's own routers, peers and sessions, chosen to sit
// outside every value already claimed in this package -- 10.0.0.1/.3/.5/.7/
// .9/.20/.30/.40/.42/.50/.60/.62 and 10.0.197.1 as routers; 10.0.0.2/.4/.6/
// .10/.11/.12/.21/.22/.23/.31/.41/.43/.44/.45/.51/.61/.63 and 10.255.197.x
// as peers; 1, 2, 9, 10, 11, 100, 200, 700, 701, 900, 1000, 1001, 1100,
// 1200 and 1300 as session_ids. chtest shares one database across every
// test in this package, so a collision would not fail loudly -- it would
// quietly add rows to somebody else's assertion.
//
// routeFilterFixtureSharedPeer is deliberately a peer of BOTH routers. A
// peer IP unique to one router would let a router filter and a peer filter
// be confused for each other without any test noticing: filtering on the
// peer would narrow to that peer's one router as a side effect, and the two
// predicates would be indistinguishable by result.
const (
	routeFilterFixtureRouterA    = "10.0.0.70"
	routeFilterFixtureRouterB    = "10.0.0.71"
	routeFilterFixtureSysnameA   = "route-filter-router-a"
	routeFilterFixtureSysnameB   = "route-filter-router-b"
	routeFilterFixturePeerA1     = "10.0.0.72" // router A only, under two ribs
	routeFilterFixturePeerA2     = "10.0.0.73" // router A only, one rib
	routeFilterFixturePeerB1     = "10.0.0.74" // router B only, one rib
	routeFilterFixtureSharedPeer = "10.0.0.75" // BOTH routers, one rib each

	routeFilterFixtureSessionA = 1400
	routeFilterFixtureSessionB = 1401

	// One prefix per table, advertised by every (router, peer, rib) below,
	// so the row count a filtered query returns is a statement about the
	// filter and nothing else. A fixture that varied the prefix as well
	// would let a broken router predicate pass on a query that had already
	// narrowed to one row by prefix alone.
	routeFilterFixturePrefix    = "10.70.0.0/24"
	routeFilterFixtureVPNPrefix = "192.168.70.0/24"
	routeFilterFixtureVPNRD     = "65070:1"
)

// routeFilterFixtureRows is the (router, peer, rib) topology
// insertRouteFilterFixture writes, once into route_unicast under
// routeFilterFixturePrefix and once into route_vpn under
// routeFilterFixtureVPNPrefix. Six rows per table, arranged so that each of
// the three (router, peer, rib) filters RouteFilter and VPNRouteFilter both
// carry selects a DIFFERENT, proper subset of them:
//
//	router = A          -> 4 (everything but B's two rows)
//	peer   = A1         -> 2 (its in_pre and loc_rib rows)
//	rib    = loc_rib    -> 1 (A/A1 only)
//	peer   = sharedPeer -> 2, one under each router
//	router = A, peer = sharedPeer -> 1
//
// Every subset has a different size, and none of them is the full six, so a
// predicate deleted from the builder changes at least one of these counts.
// ?family= and ?rd= are NOT exercised here -- every row this fixture writes
// is ipv4u or vpn4 under one RD, so neither filter could narrow anything.
// insertFamilyFilterFixture is the fixture for those two, kept separate for
// exactly the reason this paragraph gives about the three above.
// That is the property a mutation check depends on: a
// fixture where two filters happened to select the same rows would let
// either of them be dropped with every assertion still passing.
var routeFilterFixtureRows = []struct {
	router, sysname, peer, rib string
	sessionID                  uint64
}{
	{routeFilterFixtureRouterA, routeFilterFixtureSysnameA, routeFilterFixturePeerA1, "in_pre", routeFilterFixtureSessionA},
	{routeFilterFixtureRouterA, routeFilterFixtureSysnameA, routeFilterFixturePeerA1, "loc_rib", routeFilterFixtureSessionA},
	{routeFilterFixtureRouterA, routeFilterFixtureSysnameA, routeFilterFixturePeerA2, "in_pre", routeFilterFixtureSessionA},
	{routeFilterFixtureRouterA, routeFilterFixtureSysnameA, routeFilterFixtureSharedPeer, "in_pre", routeFilterFixtureSessionA},
	{routeFilterFixtureRouterB, routeFilterFixtureSysnameB, routeFilterFixturePeerB1, "in_pre", routeFilterFixtureSessionB},
	{routeFilterFixtureRouterB, routeFilterFixtureSysnameB, routeFilterFixtureSharedPeer, "in_pre", routeFilterFixtureSessionB},
}

// insertRouteFilterFixture writes routeFilterFixtureRows as a peer_events
// "up" row, a route_unicast row and a route_vpn row apiece: two routers,
// four distinct peers, two ribs, and one prefix per table shared by all six
// (router, peer, rib) tuples.
//
// Every peer is up and every route is live. That is deliberate: this
// fixture's whole job is to make the FILTER the only thing that can change
// a row count, so nothing here should be excludable for any other reason.
// The down-peer, stale-session, withdrawn-route and add-path cases all have
// fixtures of their own (see insertRouteFixture and insertVPNRouteFixture),
// and mixing those hazards in here would mean a failing count could be
// explained by any of half a dozen unrelated defects.
//
// The peer_events row for A/A1 is written under both of that peer's ribs
// rather than only in_pre, so the loc_rib route has a peer_state row of its
// own. peer_up alone would keep the route alive without it (see
// peerUpCTE), but a route whose rib peer_events never recorded is a
// different case with a fixture of its own already
// (insertPostPolicyRouteFixture); this one is not about that.
func insertRouteFilterFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	for i, r := range routeFilterFixtureRows {
		ss := uint64(i + 1)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: r.router, RouterSysname: r.sysname,
			PeerIP: r.peer, RIB: r.rib,
			PeerASN: 65000, PeerBGPID: r.peer,
			SessionID: r.sessionID, Seq: ss, StreamSeq: ss,
			Kind: "up", TsRouter: now, TsCollector: now,
		})
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: r.router, RouterSysname: r.sysname,
			PeerIP: r.peer, RIB: r.rib, Prefix: routeFilterFixturePrefix,
			PeerASN: 65000, PeerBGPID: r.peer,
			SessionID: r.sessionID, Seq: 1, StreamSeq: 10 + ss,
			PathID: 1, NextHop: "10.9.70.1", ASPath: []uint32{65070},
			TsRouter: now, TsCollector: now,
		})
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: r.router, RouterSysname: r.sysname,
			PeerIP: r.peer, RIB: r.rib,
			Family: "vpn4", Prefix: routeFilterFixtureVPNPrefix, RD: routeFilterFixtureVPNRD,
			PeerASN: 65000, PeerBGPID: r.peer,
			SessionID: r.sessionID, Seq: 1, StreamSeq: 20 + ss,
			PathID: 1, NextHop: "10.9.70.1", Labels: []uint32{24070},
			ASPath: []uint32{65070}, RouteTargets: []string{"65070:1"},
			TsRouter: now, TsCollector: now,
		})
	}
}

// The family/RD fixture's own router, peer, session and route
// distinguishers, chosen to sit outside every value already claimed in this
// package (see the route-filter fixture's own reservation comment above for
// the full list, and for why a collision here would be silent rather than
// loud). 10.0.0.80 and 10.0.0.81 are the first router/peer pair past
// insertRouteFilterFixture's 10.0.0.70-.75 block; 1500 is the first session
// past its 1400/1401; 65080:1 and 65080:2 are the only RDs in this package
// outside the 65001:x and 65070:x families the two VPN fixtures above use.
//
// That last point is load-bearing rather than tidy. TestVPNRoutesFiltersByRDAndFamily
// asks VPNRoutes for an RD with NO router scope at all -- which is the whole
// point of the query, since api/openapi.yaml lists rd= as one of the three
// filters that may stand alone -- so its row counts are statements about
// every route_vpn row in the shared database, not just this fixture's. An RD
// another fixture also wrote would make those counts a claim about insertion
// order.
const (
	familyFixtureRouterIP  = "10.0.0.80"
	familyFixtureSysname   = "family-filter-router"
	familyFixturePeerIP    = "10.0.0.81"
	familyFixtureSessionID = 1500

	familyFixtureRDOne = "65080:1"
	familyFixtureRDTwo = "65080:2"

	// The unicast pair: one prefix per family, because no prefix string is
	// legal NLRI in both. See TestRoutesFiltersByFamily for what that costs
	// the mutation check, and why it cannot be fixed with a better fixture.
	familyFixtureV4Prefix = "10.80.0.0/24"
	familyFixtureV6Prefix = "2001:db8:80::/48"

	// The VPN set. familyFixtureVPNShared is deliberately carried by BOTH
	// vpn4 (under familyFixtureRDOne) and lu4 (RD-less, as BGP-LU always
	// is): a labeled-unicast route and a VPN route really can cover the
	// same IPv4 prefix, and it is the one shape in which ?family= SELECTS a
	// row rather than merely narrowing to none.
	familyFixtureVPNShared = "192.168.80.0/24"
	familyFixtureVPNSecond = "192.168.81.0/24"   // vpn4, RD one
	familyFixtureVPNOther  = "192.168.82.0/24"   // vpn4, RD two
	familyFixtureVPNv6     = "2001:db8:180::/48" // vpn6, RD one
)

// insertFamilyFilterFixture writes one up peer carrying two unicast routes
// and five VPN routes, arranged so that ?family= and ?rd= each select a
// DIFFERENT, proper subset -- the same property insertRouteFilterFixture's
// own doc comment claims for ?router=/?peer=/?rib=, and for the same reason:
// a fixture where two filters happened to select the same rows would let
// either be deleted with every assertion still passing.
//
//	route_unicast   family = ipv4u          -> 1 (10.80.0.0/24)
//	                family = ipv6u          -> 1 (2001:db8:80::/48)
//
//	route_vpn       rd     = 65080:1        -> 3
//	                rd     = 65080:2        -> 1
//	                family = vpn4           -> 3
//	                family = lu4            -> 1
//	                family = vpn6           -> 1
//	                prefix = 192.168.80.0/24 -> 2, one vpn4 and one lu4
//	                router alone            -> 5
//
// The IPv6 rows are fixture-only by necessity and are not decoration: a
// real captured archive holds zero ipv6u and zero vpn6 rows (verified
// 2026-08-24), so a test that exercised those two family values against
// the archive would pass whether or not the predicate existed. Writing them
// here is what makes "family = ipv6u returns the v6 row and not the v4
// one" an assertion about the filter rather than about the archive being
// empty.
func insertFamilyFilterFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: familyFixtureRouterIP, RouterSysname: familyFixtureSysname,
		PeerIP: familyFixturePeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: familyFixturePeerIP,
		SessionID: familyFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	for i, r := range []struct{ family, prefix, nextHop string }{
		{"ipv4u", familyFixtureV4Prefix, "10.9.80.1"},
		{"ipv6u", familyFixtureV6Prefix, "2001:db8:9::1"},
	} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: familyFixtureRouterIP, RouterSysname: familyFixtureSysname,
			PeerIP: familyFixturePeerIP, RIB: "in_pre",
			Family: r.family, Prefix: r.prefix,
			PeerASN: 65000, PeerBGPID: familyFixturePeerIP,
			SessionID: familyFixtureSessionID, Seq: 1, StreamSeq: uint64(10 + i),
			PathID: 1, NextHop: r.nextHop, ASPath: []uint32{65080},
			TsRouter: now, TsCollector: now,
		})
	}

	for i, r := range []struct{ family, prefix, rd, nextHop string }{
		{"vpn4", familyFixtureVPNShared, familyFixtureRDOne, "10.9.80.1"},
		{"lu4", familyFixtureVPNShared, "", "10.9.80.2"},
		{"vpn4", familyFixtureVPNSecond, familyFixtureRDOne, "10.9.80.1"},
		{"vpn4", familyFixtureVPNOther, familyFixtureRDTwo, "10.9.80.1"},
		{"vpn6", familyFixtureVPNv6, familyFixtureRDOne, "2001:db8:9::1"},
	} {
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: familyFixtureRouterIP, RouterSysname: familyFixtureSysname,
			PeerIP: familyFixturePeerIP, RIB: "in_pre",
			Family: r.family, Prefix: r.prefix, RD: r.rd,
			PeerASN: 65000, PeerBGPID: familyFixturePeerIP,
			SessionID: familyFixtureSessionID, Seq: 1, StreamSeq: uint64(20 + i),
			PathID: 1, NextHop: r.nextHop, Labels: []uint32{uint32(24080 + i)},
			ASPath: []uint32{65080}, TsRouter: now, TsCollector: now,
		})
	}
}

// The EVPN route fixture's own router, peers and session. Distinct from
// every value already claimed in this package (see familyFixtureRouterIP's
// own comment for the addresses already claimed): 10.0.0.90/.91/.92/.93
// sit past insertFamilyFilterFixture's 10.0.0.80/.81, and 1600 sits past
// its 1500.
//
// The RD block 65090:x is reserved to this fixture for the reason
// familyFixtureRDOne's is reserved to its own: TestEVPNRoutes asks
// EVPNRoutes for an RD with no router scope at all -- rd= is one of the
// three filters api/openapi.yaml lets stand alone -- so its row counts are
// statements about every route_evpn row in the shared database, not just
// this fixture's. An RD another fixture also wrote would make those counts a
// claim about insertion order.
const (
	evpnFixtureRouterIP   = "10.0.0.90"
	evpnFixtureSysname    = "evpn-route-fixture-router"
	evpnFixtureUpPeerIP   = "10.0.0.91" // up, and its EVPN dump is still in progress.
	evpnFixtureDownPeerIP = "10.0.0.92" // up, then down, in the current session.
	evpnFixtureDonePeerIP = "10.0.0.93" // up, with an evpn marker on record.
	evpnFixtureSessionID  = 1600

	// The route distinguishers, one per subtest, so each can query by rd
	// alone and get a set this fixture entirely controls.
	evpnFixtureRDType2    = "65090:2"  // one type-2 MAC/IP route.
	evpnFixtureRDType3    = "65090:3"  // one type-3 IMET route.
	evpnFixtureRDType5    = "65090:5"  // one type-5 IP-prefix route.
	evpnFixtureRDESI      = "65090:7"  // two routes differing ONLY in esi.
	evpnFixtureRDWithdraw = "65090:9"  // one live route beside one withdrawn.
	evpnFixtureRDDownPeer = "65090:11" // one route from an up peer, one from a down one.
	evpnFixtureRDDone     = "65090:13" // one route whose family's dump is complete.
	evpnFixtureRDPostRIB  = "65090:15" // one route under a rib peer_events never recorded.
	evpnFixtureRDSession  = "65090:17" // one route from the current session, one from a dead one.

	// evpnFixtureRDLiveSentinel is the one RD here whose route is LIVE and
	// whose label stack carries evpnWithdrawSentinel anyway. Every other
	// sentinel-bearing row in this file, and every one in a real captured
	// archive, is also is_withdraw = 1 and is therefore excluded by
	// evpnRoutesSQL's HAVING before its labels are ever looked at -- which
	// left "EVPN labels are reported as received" resting on no assertion at
	// all. A future reader who reached for vpnroutes.go's label() here, or
	// wrote a strip on 8388608 of their own, would have failed nothing.
	evpnFixtureRDLiveSentinel = "65090:19"

	// The two prefixes under evpnFixtureRDSession. The stale one belongs to
	// a session this router has since replaced; the live one is what stops
	// that subtest passing on a query that returns nothing at all.
	evpnFixtureStalePrefix = "192.168.99.0/24"
	evpnFixtureLivePrefix  = "192.168.100.0/24"

	// evpnFixtureOldSession is the session evpnFixtureUpPeerIP first came up
	// in; evpnFixtureSessionID is the one it is in now. cur is scoped to
	// (collector, router) and takes max(session_id), so the higher of the
	// two is current for every peer on this router.
	evpnFixtureOldSession = 1599

	// The ESIs the two-ESI subtest tells apart. An Ethernet Segment
	// Identifier is 10 bytes, rendered by bgp/evpn.go as 20 hex characters;
	// the all-zero one is what every type-2 and type-5 row in the live
	// archive carries (a single-homed segment), which is exactly why a
	// second, non-zero value has to be manufactured here.
	evpnFixtureESIZero  = "00000000000000000000"
	evpnFixtureESIOther = "0102030405060708090a"

	// evpnFixtureType2MAC and the two below are the MAC addresses the
	// type-2 subtests read back. The withdrawn one differs from the live one
	// only in its last octet, so a query that lost the withdrawal exclusion
	// returns a row that looks entirely ordinary.
	evpnFixtureType2MAC     = "00:50:79:66:68:01"
	evpnFixtureLiveMAC      = "00:50:79:66:68:91"
	evpnFixtureWithdrawnMAC = "00:50:79:66:68:99"

	// evpnFixtureType2IP is the type-2 route's IP half. 12 of the archive's
	// 16 distinct type-2 NLRIs carry no IP at all and 4 carry one, under the
	// same rd and mac -- which is why ip is part of the route key and why
	// this fixture sets it on one type-2 row and leaves it empty on others.
	evpnFixtureType2IP = "192.168.90.11"
)

// evpnWithdrawSentinel is the value a real captured archive's withdrawn EVPN
// rows carry in their label stack -- 0x800000, RFC 3107's reserved "this
// NLRI is being withdrawn" value, in the RAW 24-bit form bgp/evpn.go stores
// (see bgp.EvpnRoute.Labels: EVPN labels are not shifted, because on a VXLAN
// fabric the field is a VNI). It is 16x vpnroutes.go's own withdrawSentinel
// = 524288, which is the same reserved value SHIFTED for route_vpn's MPLS
// labels, and a reader who reached for that constant here would strip
// nothing.
//
// All 46 withdrawn rows in the archive's route_evpn carry it and no
// advertised row does. Nothing in query/ strips it -- api/openapi.yaml types
// EVPNRoute.labels as "the label stack as received", and HAVING
// live_is_withdraw = 0 already removes every row carrying it -- so this
// const exists to make the fixture's withdrawn row look like a real one
// rather than to be filtered on.
const evpnWithdrawSentinel = 8388608

// insertEVPNRouteFixture writes one router, three peers and fifteen
// route_evpn rows -- enough for every subtest TestEVPNRoutes runs, each row
// placed to make one specific wrong answer visible rather than merely
// plausible:
//
//   - A type-2 MAC/IP route (rd 65090:2) carrying both a MAC and an IP,
//     two labels, a route target and an AS path. The archive's own type-2
//     rows carry a MAC, no prefix, no route target and no AS path; this one
//     sets the last two so the fields are proven to round-trip rather than
//     proven to be empty.
//   - A type-3 IMET route (rd 65090:3) with an empty prefix, MAC, IP and
//     ESI, which is what every one of the archive's 64 type-3 rows looks
//     like. A query that treats an empty MAC or prefix as a row to skip
//     loses this route and, on the archive, a third of the table.
//   - A type-5 IP-prefix route (rd 65090:5) with a prefix and a gateway IP
//     and no MAC or IP, the archive's own type-5 shape.
//   - Two type-2 routes under rd 65090:7 that differ ONLY in esi, with
//     different next hops so the pair is distinguishable in the answer as
//     well as countable. The archive cannot falsify a query that drops esi
//     from the route key: it carries exactly two ESI values, the all-zero
//     one on every type-2 and type-5 row and the empty one on every type-3
//     row, never two under one NLRI. This is fixture-only for that reason.
//   - Two type-2 routes under rd 65090:9, one live and one advertised and
//     then withdrawn at a higher seq, its label stack carrying
//     evpnWithdrawSentinel the way the archive's own withdrawn rows do. The
//     live sibling is what stops the withdrawal subtest passing on a query
//     that returns nothing at all.
//   - Two type-5 routes under rd 65090:11 with the same prefix, one from
//     the up peer and one from the peer that goes down inside the current
//     session. The down peer's row never leaves route_evpn; only the
//     peer_up gate keeps it out of the answer, and the up peer's row is
//     what stops that subtest passing vacuously.
//   - A type-5 route (rd 65090:13) from evpnFixtureDonePeerIP, whose
//     current session also carries an evpn marker in eor_events, so its
//     DumpState reads "complete" where every other row here reads
//     "dumping".
//   - A type-5 route (rd 65090:15) under rib in_post, a rib this peer's
//     peer_events have no accounting for at all. It must still be
//     returned -- nothing here says the peer is down -- reading DumpState =
//     "unknown"; see insertPostPolicyRouteFixture's own doc comment for the
//     live-archive occurrence of that shape.
//   - Two type-5 routes under rd 65090:17, one advertised in the session
//     this router has since replaced and one in the current session. The
//     peer is up right now, so only route_evpn's own session_id keeps the
//     stale one out, and the current-session sibling is what stops that
//     subtest passing on a query that returns nothing at all.
//   - A LIVE type-2 route (rd 65090:19) whose one label is
//     evpnWithdrawSentinel, is_withdraw = 0. The archive has no row this
//     shape -- all 46 of its sentinel-bearing rows are withdrawals, and so
//     is every other one in this file -- which is exactly the gap: with
//     HAVING live_is_withdraw = 0 removing every sentinel row before
//     anything reads its labels, "the stack is reported as received" was a
//     claim no test could falsify. This row is the one that can. It is a
//     deliberate counterpart to route_vpn's 192.168.11.0/24, which is
//     live-with-a-sentinel for the opposite purpose: there, label() MUST
//     strip; here, nothing may. The two sentinels are not even the same
//     number -- 8388608 raw here against vpnroutes.go's shifted 524288 --
//     which is why a reader who reused the VPN constant would strip
//     nothing and notice nothing.
//
// evpnFixtureUpPeerIP's session carries a vpn4 marker and no evpn one. That
// is what makes evpnRoutesSQL's `eor.fam = ?` join predicate load-bearing
// rather than merely present: without it, this peer's finished vpn4 dump
// would report every EVPN route above as "complete", which is one family's
// dump progress declaring another family's -- exactly the question
// eor_events was split out of route_unicast to make answerable.
func insertEVPNRouteFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	// The three peers. evpnFixtureDownPeerIP comes up and then goes down at
	// a higher seq, inside the SAME (current) session, so its route below is
	// not a stale-session case.
	for _, p := range []struct {
		ip      string
		kind    string
		seq, ss uint64
	}{
		{evpnFixtureUpPeerIP, "up", 1, 1},
		{evpnFixtureDownPeerIP, "up", 1, 2},
		{evpnFixtureDownPeerIP, "down", 2, 3},
		{evpnFixtureDonePeerIP, "up", 1, 4},
	} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: evpnFixtureRouterIP, RouterSysname: evpnFixtureSysname,
			PeerIP: p.ip, RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: p.ip,
			SessionID: evpnFixtureSessionID, Seq: p.seq, StreamSeq: p.ss,
			Kind: p.kind, TsRouter: now, TsCollector: now,
		})
	}

	// up is the shape every row below starts from: this fixture's router,
	// its up peer, its current session, rib in_pre. Only the NLRI and the
	// per-row sequencing differ, and writing them out at each call site
	// would bury that in eight identical lines apiece.
	up := func(f routeEVPNFixture) routeEVPNFixture {
		f.RouterIP, f.RouterSysname = evpnFixtureRouterIP, evpnFixtureSysname
		if f.PeerIP == "" {
			f.PeerIP = evpnFixtureUpPeerIP
		}
		if f.RIB == "" {
			f.RIB = "in_pre"
		}
		f.PeerASN, f.PeerBGPID = 65000, f.PeerIP
		if f.SessionID == 0 {
			f.SessionID = evpnFixtureSessionID
		}
		if f.Seq == 0 {
			f.Seq = 1
		}
		// Only defaulted, never overwritten: the withdrawal below sets its
		// own timestamps EARLIER than the advertisement it supersedes, which
		// is what makes argMax's ordering by (seq, stream_seq) rather than by
		// either clock a testable claim rather than an untested convention.
		if f.TsRouter.IsZero() {
			f.TsRouter = now
		}
		if f.TsCollector.IsZero() {
			f.TsCollector = now
		}
		return f
	}

	// rd 65090:2 -- a type-2 MAC/IP route with everything a type 2 can
	// carry: two labels (RFC 7432 §7.2 permits a second), a route target
	// and an AS path.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 2, RD: evpnFixtureRDType2,
		MAC: evpnFixtureType2MAC, IP: evpnFixtureType2IP,
		ESI: evpnFixtureESIZero, EthernetTag: 10,
		Labels: []uint32{10010, 50001}, NextHop: "10.9.90.1",
		ASPath: []uint32{65010, 65090}, RouteTargets: []string{"65090:100"},
		StreamSeq: 10,
	}))

	// rd 65090:3 -- a type-3 IMET route: no prefix, no MAC, no IP, no ESI
	// and no labels, which is what all 64 of the archive's type-3 rows look
	// like. Its ethernet tag and RD are the only things identifying it.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 3, RD: evpnFixtureRDType3,
		EthernetTag: 20, Labels: []uint32{}, NextHop: "10.9.90.1",
		StreamSeq: 11,
	}))

	// rd 65090:5 -- a type-5 IP-prefix route: a prefix and a gateway IP,
	// no MAC and no IP.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 5, RD: evpnFixtureRDType5, Prefix: "192.168.95.0/24",
		GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero,
		Labels: []uint32{50001}, NextHop: "10.9.90.5",
		StreamSeq: 12,
	}))

	// rd 65090:7 -- two routes differing ONLY in esi, with different next
	// hops so a caller can tell them apart in the answer.
	for i, e := range []struct{ esi, nextHop string }{
		{evpnFixtureESIZero, "10.9.90.71"},
		{evpnFixtureESIOther, "10.9.90.72"},
	} {
		insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
			RouteType: 2, RD: evpnFixtureRDESI,
			MAC: evpnFixtureType2MAC, ESI: e.esi, EthernetTag: 10,
			Labels: []uint32{10070}, NextHop: e.nextHop,
			StreamSeq: uint64(13 + i),
		}))
	}

	// rd 65090:9 -- one live route beside one that was advertised and then
	// withdrawn. Both are ordinary type-2 rows differing only in their last
	// MAC octet.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 2, RD: evpnFixtureRDWithdraw,
		MAC: evpnFixtureLiveMAC, ESI: evpnFixtureESIZero, EthernetTag: 10,
		Labels: []uint32{10090}, NextHop: "10.9.90.91",
		StreamSeq: 15,
	}))
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 2, RD: evpnFixtureRDWithdraw,
		MAC: evpnFixtureWithdrawnMAC, ESI: evpnFixtureESIZero, EthernetTag: 10,
		Labels: []uint32{10099}, NextHop: "10.9.90.99",
		StreamSeq: 16,
	}))
	// The withdrawal, at a higher seq and stream_seq than the advertisement
	// it supersedes but an HOUR EARLIER by both clocks. A query that
	// resolves a route key's newest observation by ts_router (a router with
	// a dead clock reports 1970) or by ts_collector (batch-granular) lands
	// on the advertisement instead and reports this route as live -- the
	// same property insertFlappedPeerFixture builds into peer_events, here
	// for route rows.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 2, RD: evpnFixtureRDWithdraw,
		MAC: evpnFixtureWithdrawnMAC, ESI: evpnFixtureESIZero, EthernetTag: 10,
		Labels: []uint32{evpnWithdrawSentinel},
		Seq:    2, StreamSeq: 17, IsWithdraw: 1,
		TsRouter: now.Add(-time.Hour), TsCollector: now.Add(-time.Hour),
	}))

	// rd 65090:11 -- the same prefix from an up peer and from the peer that
	// goes down inside this session.
	for i, p := range []string{evpnFixtureUpPeerIP, evpnFixtureDownPeerIP} {
		insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
			PeerIP:    p,
			RouteType: 5, RD: evpnFixtureRDDownPeer, Prefix: "192.168.96.0/24",
			GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero,
			Labels: []uint32{50011}, NextHop: "10.9.90.11",
			StreamSeq: uint64(18 + i),
		}))
	}

	// evpnFixtureDonePeerIP -- up, with an evpn marker on record, so rd
	// 65090:13's route reads DumpState = "complete". Kept on its own peer so
	// that adding the marker cannot also flip evpnFixtureUpPeerIP's rows.
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: evpnFixtureRouterIP, RouterSysname: evpnFixtureSysname,
		PeerIP: evpnFixtureDonePeerIP, RIB: "in_pre", Family: "evpn",
		PeerASN: 65000, PeerBGPID: evpnFixtureDonePeerIP,
		SessionID: evpnFixtureSessionID, Seq: 2, StreamSeq: 20,
		TsRouter: now, TsCollector: now,
	})
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		PeerIP:    evpnFixtureDonePeerIP,
		RouteType: 5, RD: evpnFixtureRDDone, Prefix: "192.168.97.0/24",
		GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero,
		Labels: []uint32{50013}, NextHop: "10.9.90.13",
		StreamSeq: 21,
	}))

	// A vpn4 marker for evpnFixtureUpPeerIP -- a family this peer has no
	// route rows for at all, which is an ordinary shape: a peer that
	// negotiated a family with an empty table sends End-of-RIB for it and
	// nothing else. Without evpnRoutesSQL's `eor.fam = ?` predicate this
	// marker matches every EVPN row above and reports them all as
	// "complete".
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: evpnFixtureRouterIP, RouterSysname: evpnFixtureSysname,
		PeerIP: evpnFixtureUpPeerIP, RIB: "in_pre", Family: "vpn4",
		PeerASN: 65000, PeerBGPID: evpnFixtureUpPeerIP,
		SessionID: evpnFixtureSessionID, Seq: 2, StreamSeq: 22,
		TsRouter: now, TsCollector: now,
	})

	// rd 65090:15 -- a route under in_post, a rib this peer's peer_events
	// have no accounting for at all.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RIB:       "in_post",
		RouteType: 5, RD: evpnFixtureRDPostRIB, Prefix: "192.168.98.0/24",
		GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero,
		Labels: []uint32{50015}, NextHop: "10.9.90.15",
		StreamSeq: 23,
	}))

	// rd 65090:17 -- one route in the session this router has since
	// replaced, one in the current one. The peer is up right now, so a query
	// that gates on peer state alone (and forgets route_evpn's own
	// session_id) reports the stale route as live: BMP re-dumps the whole
	// table on a new session, so an old-session row with no fresher
	// counterpart is a claim about a session that no longer exists, not a
	// slightly out-of-date route.
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: evpnFixtureRouterIP, RouterSysname: evpnFixtureSysname,
		PeerIP: evpnFixtureUpPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: evpnFixtureUpPeerIP,
		SessionID: evpnFixtureOldSession, Seq: 1, StreamSeq: 5,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		SessionID: evpnFixtureOldSession,
		RouteType: 5, RD: evpnFixtureRDSession, Prefix: evpnFixtureStalePrefix,
		GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero,
		Labels: []uint32{50017}, NextHop: "10.9.90.17",
		StreamSeq: 6,
	}))
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 5, RD: evpnFixtureRDSession, Prefix: evpnFixtureLivePrefix,
		GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero,
		Labels: []uint32{50018}, NextHop: "10.9.90.18",
		StreamSeq: 24,
	}))

	// rd 65090:19 -- live, and carrying the withdraw sentinel in its label
	// stack anyway. Nothing in query/ strips it: api/openapi.yaml types
	// EVPNRoute.labels as the stack "as received", and this package has no
	// encapsulation community to justify deciding otherwise. See
	// evpnFixtureRDLiveSentinel's own comment for why the row has to exist
	// for that to be a testable claim rather than a convention.
	insertRouteEVPNEvent(t, ctx, q, up(routeEVPNFixture{
		RouteType: 2, RD: evpnFixtureRDLiveSentinel,
		MAC: evpnFixtureType2MAC, ESI: evpnFixtureESIZero, EthernetTag: 10,
		Labels: []uint32{evpnWithdrawSentinel}, NextHop: "10.9.90.19",
		StreamSeq: 25,
	}))
}

// The EVPN filter fixture's own topology. Distinct from every value already
// claimed in this package (see evpnFixtureRouterIP's own comment for the
// addresses already claimed): 10.0.0.94-.99 sit past this file's last
// claimed 10.0.0.93, 1700/1701 past its last session 1600, and the
// 65094:x RD block past the 65090:x block insertEVPNRouteFixture reserves.
//
// The RD reservation is load-bearing for the same reason
// insertFamilyFilterFixture's is: TestEVPNRoutesFilters asks for an rd with
// no router scope, so that count is a claim about every route_evpn row in
// the shared test database.
const (
	evpnFilterRouterA  = "10.0.0.94"
	evpnFilterRouterB  = "10.0.0.95"
	evpnFilterSysnameA = "evpn-filter-router-a"
	evpnFilterSysnameB = "evpn-filter-router-b"
	evpnFilterPeerA1   = "10.0.0.96" // router A only, under two ribs
	evpnFilterPeerA2   = "10.0.0.97" // router A only, one rib
	evpnFilterPeerB1   = "10.0.0.98" // router B only, one rib
	evpnFilterShared   = "10.0.0.99" // BOTH routers, one rib each
	evpnFilterSessionA = 1700
	evpnFilterSessionB = 1701

	// One RD carried by the whole six-row topology, and a second carried
	// only by the two rows that exist to make ?type= select rather than
	// merely narrow.
	evpnFilterRDTopology = "65094:1"
	evpnFilterRDTypes    = "65094:2"

	// The one prefix in this fixture, on its one type-5 row. ?prefix= has to
	// be exercised against a route type that carries one at all -- 136 of
	// the archive's 202 rows have an empty prefix -- so this is the row it
	// selects.
	evpnFilterPrefix = "10.94.0.0/24"

	evpnFilterMAC = "00:50:79:66:68:94"
)

// evpnFilterRows is the (router, peer, rib) topology insertEVPNFilterFixture
// writes under evpnFilterRDTopology, arranged the way
// insertRouteFilterFixture's own rows are and for the same reason: each of
// ?router=, ?peer= and ?rib= selects a DIFFERENT, proper subset, so a
// predicate deleted from the builder changes at least one asserted count.
var evpnFilterRows = []struct {
	router, sysname, peer, rib string
	sessionID                  uint64
}{
	{evpnFilterRouterA, evpnFilterSysnameA, evpnFilterPeerA1, "in_pre", evpnFilterSessionA},
	{evpnFilterRouterA, evpnFilterSysnameA, evpnFilterPeerA1, "loc_rib", evpnFilterSessionA},
	{evpnFilterRouterA, evpnFilterSysnameA, evpnFilterPeerA2, "in_pre", evpnFilterSessionA},
	{evpnFilterRouterA, evpnFilterSysnameA, evpnFilterShared, "in_pre", evpnFilterSessionA},
	{evpnFilterRouterB, evpnFilterSysnameB, evpnFilterPeerB1, "in_pre", evpnFilterSessionB},
	{evpnFilterRouterB, evpnFilterSysnameB, evpnFilterShared, "in_pre", evpnFilterSessionB},
}

// insertEVPNFilterFixture writes evpnFilterRows as a peer_events "up" row
// and a type-2 route_evpn row apiece, plus two more rows on router A's first
// peer -- one type 3 and one type 5, under a second RD -- so that ?type=,
// ?rd= and ?prefix= each select a subset of their own:
//
//	rd     = 65094:1        -> 6 (the whole topology)
//	rd     = 65094:2        -> 2 (the type-3 and type-5 rows)
//	router = A              -> 6 (four topology rows plus both type rows)
//	router = B              -> 2
//	peer   = A1  (with rd)  -> 2 (its in_pre and loc_rib topology rows)
//	peer   = shared         -> 2, one under each router
//	rib    = loc_rib        -> 1
//	type   = 3 (with router) -> 1
//	type   = 5 (with router) -> 1
//	prefix = 10.94.0.0/24    -> 1
//
// Every peer is up, every route is live and none is withdrawn. That is
// deliberate, the same way it is in insertRouteFilterFixture: this fixture's
// whole job is to make the FILTER the only thing that can change a row
// count, so a failing count cannot be explained by a down peer, a stale
// session or a withdrawal. Those cases have insertEVPNRouteFixture.
func insertEVPNFilterFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	for i, r := range evpnFilterRows {
		ss := uint64(i + 1)
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: r.router, RouterSysname: r.sysname,
			PeerIP: r.peer, RIB: r.rib,
			PeerASN: 65000, PeerBGPID: r.peer,
			SessionID: r.sessionID, Seq: ss, StreamSeq: ss,
			Kind: "up", TsRouter: now, TsCollector: now,
		})
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: r.router, RouterSysname: r.sysname,
			PeerIP: r.peer, RIB: r.rib,
			RouteType: 2, RD: evpnFilterRDTopology,
			MAC: evpnFilterMAC, ESI: evpnFixtureESIZero, EthernetTag: 94,
			PeerASN: 65000, PeerBGPID: r.peer,
			SessionID: r.sessionID, Seq: 1, StreamSeq: 10 + ss,
			Labels: []uint32{10094}, NextHop: "10.9.94.1",
			TsRouter: now, TsCollector: now,
		})
	}

	// The two rows ?type= and ?prefix= select, on router A's first peer and
	// under the second RD so that neither filter's count is also the
	// topology's.
	for i, r := range []struct {
		routeType uint8
		prefix    string
		tag       uint32
	}{
		{3, "", 940},
		{5, evpnFilterPrefix, 0},
	} {
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: evpnFilterRouterA, RouterSysname: evpnFilterSysnameA,
			PeerIP: evpnFilterPeerA1, RIB: "in_pre",
			RouteType: r.routeType, RD: evpnFilterRDTypes, Prefix: r.prefix,
			EthernetTag: r.tag,
			PeerASN:     65000, PeerBGPID: evpnFilterPeerA1,
			SessionID: evpnFilterSessionA, Seq: 1, StreamSeq: uint64(20 + i),
			Labels: []uint32{}, NextHop: "10.9.94.2",
			TsRouter: now, TsCollector: now,
		})
	}
}

// The covers fixture's own router, peer and session, chosen past every value
// already claimed in this package -- 10.0.0.90-.99 are the two EVPN
// fixtures' (see evpnFixtureRouterIP and evpnFilterRouterA), so this one
// starts at 10.0.0.100, and 1800 sits past insertEVPNFilterFixture's
// 1700/1701.
//
// The prefixes matter more than the addresses here, and in a way no other
// fixture in this file has to think about. Every other route filter is an
// EQUALITY: a fixture's rows can only ever answer for themselves. Covers is
// CONTAINMENT, so a covers query asked without a router scope is a statement
// about every route_unicast row the shared test database holds -- a
// 0.0.0.0/0 or a second 10.0.0.0/8 written by any other fixture would join
// the answer, and this fixture's counts would silently become a claim about
// insertion order. 10.77.x is unclaimed by every prefix this package writes
// (10.1-10.6, 10.70, 10.80, 10.94, 10.200, 10.201, 10.250 and the
// 192.168.x/198.51.100/203.0.113 blocks, none of which contains
// coversFixtureTarget and none of which is a default route), which is what
// makes the unscoped subtest legitimate; every other subtest scopes to this
// router as well.
const (
	coversFixtureRouterIP  = "10.0.0.100"
	coversFixtureSysname   = "covers-fixture-router"
	coversFixturePeerIP    = "10.0.0.101"
	coversFixtureSessionID = 1800

	// coversFixtureTarget is the IPv4 address TestRoutesCovers asks about,
	// and coversFixtureTargetV6 its IPv6 counterpart. The v6 target is not
	// symmetry for its own sake: an IPv4 prefix length has to be converted
	// to its IPv4-mapped IPv6 equivalent (+96) and an IPv6 one must not be,
	// so a fixture with only v4 targets would let that conversion be
	// deleted, doubled or applied to both families with every assertion
	// still green.
	coversFixtureTarget   = "10.77.0.33"
	coversFixtureTargetV6 = "2001:db8:77::1"

	// The three prefixes that contain coversFixtureTarget, at three
	// different lengths. A single covering prefix would be answered
	// correctly by a predicate that compared only network addresses and
	// ignored the length entirely.
	coversFixtureSlash24 = "10.77.0.0/24"
	coversFixtureSlash16 = "10.77.0.0/16"
	coversFixtureSlash8  = "10.0.0.0/8"

	// coversFixtureSibling shares coversFixtureSlash16's own /16 and does
	// NOT contain the target. It is what fails when the predicate widens by
	// an octet, which no assertion about the three covering prefixes above
	// can catch on its own.
	coversFixtureSibling = "10.77.1.0/24"

	// coversFixtureV6 contains coversFixtureTargetV6 and contains no IPv4
	// address at all. It is the row the v6 target selects, and the row a v4
	// target must not.
	coversFixtureV6 = "2001:db8:77::/48"

	// coversFixtureV6Default is an IPv6 default route -- a real route a
	// default-originate session carries -- and it is the row the family test
	// exists for. IPv6CIDRToRange is arithmetic on 128 bits with no notion
	// of a family, so ::/0 contains ::ffff:10.77.0.33 exactly the way it
	// contains everything else; without the family test it is reported as
	// covering every IPv4 address in the fleet, which is a wrong answer with
	// no tell at all. It must cover coversFixtureTargetV6 (where it really
	// is the covering route) and must not appear in any IPv4 answer, so this
	// one row fails in BOTH directions rather than only proving an
	// exclusion.
	coversFixtureV6Default = "::/0"

	// The deliberately MALFORMED stored prefixes, and the reason this
	// fixture exists in the shape it does. route_unicast.prefix is a plain
	// String column with no constraint of any kind (see schema.sql), so
	// nothing in the database stops a row from holding one of these; the
	// end-of-RIB markers that used to carry an empty prefix were the only
	// rows in the looking-glass fixture that exercised the panel SQL's
	// row-side guards at all, and they moved to eor_events, leaving those
	// guards exercised by nothing.
	//
	// Each is caught by exactly ONE conjunct of the covers predicate, which
	// is what makes each independently mutation-testable (see
	// TestRoutesCovers's own malformed subtests), and none of the four is a
	// wrong answer a reader would question -- every one of them reads as an
	// ordinary covering route for the address asked about:
	//
	//	coversFixtureBadAddr  toIPv6OrNull(net) IS NOT NULL
	//	    Reached by an IPv6 query only: its address half carries colons,
	//	    so the family test hands it to the v6 side, where the '::'
	//	    coalesce substitutes and a length of 0 makes the range every
	//	    address there is. Its length is 0 rather than 48 for exactly that
	//	    reason -- with the family test in place, a colon-free malformed
	//	    address can never produce a false positive at all (the fallback
	//	    range starts at :: and +96 pushes it past every IPv4-mapped
	//	    address), so a v4-reachable row would pin nothing.
	//
	//	coversFixtureBadLen   the length range test, via NULL
	//	    toUInt8OrNull("999") is NULL, so the range test is NULL and the
	//	    row drops. Without it, coalesce substitutes 0, +96 makes a /96,
	//	    and a /96 of ::ffff:10.0.0.0 is the whole IPv4 space.
	//
	//	coversFixtureWrapLen  the length range test, via the bound
	//	    "200" parses perfectly well as a UInt8, so a null check would
	//	    pass it through; 200 + 96 is 296, which toUInt8 WRAPS to 40, and
	//	    a /40 contains every address this query can be asked about. Every
	//	    stored v4 length from 160 to 255 wraps this way.
	//
	//	coversFixtureOverLen  the length range test's per-family BOUND
	//	    "40" is out of range for IPv4 and inside it for IPv6, so it is
	//	    the row that tells `<= if(colon, 128, 32)` apart from a flat
	//	    `<= 128`: under the flat bound 40 + 96 = 136 clamps to /128 and
	//	    the row is reported as covering exactly its own base address,
	//	    which is why coversFixtureOverLenBase is queried directly.
	coversFixtureBadAddr = "2001:db8::bogus/0"
	coversFixtureBadLen  = "10.0.0.0/999"
	coversFixtureWrapLen = "10.0.0.0/200"
	coversFixtureOverLen = "10.88.0.0/40"

	// coversFixtureOverLenBase is coversFixtureOverLen's own base address,
	// the one address a flat 128-bit bound would report that row as
	// covering. It sits inside coversFixtureSlash8, so the answer to it is
	// one row rather than none -- a subtest asserting emptiness would pass
	// on a query that had stopped returning anything at all.
	coversFixtureOverLenBase = "10.88.0.0"

	// The VPN and EVPN halves, which exist because api/openapi.yaml
	// documents ?covers= on /v1/routes -- the fan-out, whose answer carries
	// all three families -- and only RouteFilter could render it.
	// See VPNRouteFilter.Covers.
	//
	// Each family gets its own /8-free address space (10.78 for VPN, 10.79
	// for EVPN) rather than reusing coversFixtureTarget, so that a query
	// against the wrong table cannot answer the right-looking way: if
	// VPNRoutes were somehow reading route_unicast, 10.78.0.33 has no row
	// there at all.
	//
	// Two lengths and a sibling per family, which is the smallest shape that
	// distinguishes containment from equality (two lengths) and from a
	// widened comparison (the sibling). The unicast half above carries the
	// malformed-prefix and family-arithmetic cases; those live in the
	// predicate, which is one shared const, so repeating them per table
	// would be three copies of one claim.
	coversFixtureVPNTarget  = "10.78.5.33"
	coversFixtureVPNSlash24 = "10.78.5.0/24"
	coversFixtureVPNSlash16 = "10.78.0.0/16"
	coversFixtureVPNSibling = "10.78.6.0/24"
	coversFixtureVPNRD      = "65078:1"

	coversFixtureEVPNTarget  = "10.79.5.33"
	coversFixtureEVPNSlash24 = "10.79.5.0/24"
	coversFixtureEVPNSlash16 = "10.79.0.0/16"
	coversFixtureEVPNSibling = "10.79.6.0/24"
	coversFixtureEVPNRD      = "65079:1"
)

// coversFixtureRows is every route this fixture writes, one per prefix under
// its own path-id so that the route keys are distinct and routesSQL's
// ORDER BY is deterministic across them.
//
// family tracks the prefix rather than being uniformly ipv4u: every row is
// written under the family its own shape claims, so that a covers query
// naming no family (the shape every containment query takes -- an address
// does not pick a family the way a prefix string does) sees all of them and
// every row-side conjunct is actually asked to do its job. The family COLUMN
// is not what the predicate tests, though: covers compares the stored
// prefix's own text, for the reason its doc comment gives, so these values
// make the fixture honest rather than making the test pass.
var coversFixtureRows = []struct {
	prefix, family, nextHop string
	pathID                  uint32
}{
	{coversFixtureSlash24, "ipv4u", "10.9.77.1", 1},
	{coversFixtureSlash16, "ipv4u", "10.9.77.2", 2},
	{coversFixtureSlash8, "ipv4u", "10.9.77.3", 3},
	{coversFixtureSibling, "ipv4u", "10.9.77.4", 4},
	{coversFixtureV6, "ipv6u", "2001:db8:9::1", 5},
	{coversFixtureV6Default, "ipv6u", "2001:db8:9::2", 6},
	{coversFixtureBadAddr, "ipv6u", "2001:db8:9::3", 7},
	{coversFixtureBadLen, "ipv4u", "10.9.77.6", 8},
	{coversFixtureWrapLen, "ipv4u", "10.9.77.7", 9},
	{coversFixtureOverLen, "ipv4u", "10.9.77.8", 10},
}

// insertCoversFixture writes one up peer carrying coversFixtureRows: three
// nested covering prefixes, a non-covering sibling, two IPv6 routes (one of
// them a default route), and four malformed stored prefixes.
//
// Every peer is up, every route is live and none is withdrawn, for the
// reason insertRouteFilterFixture's own doc comment gives: this fixture's
// job is to make CONTAINMENT the only thing that can change a row count, so
// nothing here should be excludable for any other reason. The down-peer,
// stale-session, withdrawn-route and add-path hazards all have fixtures of
// their own.
func insertCoversFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: coversFixtureRouterIP, RouterSysname: coversFixtureSysname,
		PeerIP: coversFixturePeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: coversFixturePeerIP,
		SessionID: coversFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	for i, r := range coversFixtureRows {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: coversFixtureRouterIP, RouterSysname: coversFixtureSysname,
			PeerIP: coversFixturePeerIP, RIB: "in_pre",
			Family: r.family, Prefix: r.prefix,
			PeerASN: 65000, PeerBGPID: coversFixturePeerIP,
			SessionID: coversFixtureSessionID, Seq: 1, StreamSeq: uint64(10 + i),
			PathID: r.pathID, NextHop: r.nextHop, ASPath: []uint32{65077},
			TsRouter: now, TsCollector: now,
		})
	}

	// The VPN half. vpn4 rather than lu4 so the row carries a real RD, which
	// is what lets the "covers alone needs no rd" subtest be a claim about
	// the check rather than about a table with nothing else in it.
	for i, prefix := range []string{
		coversFixtureVPNSlash24, coversFixtureVPNSlash16, coversFixtureVPNSibling,
	} {
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: coversFixtureRouterIP, RouterSysname: coversFixtureSysname,
			PeerIP: coversFixturePeerIP, RIB: "in_pre",
			Family: "vpn4", Prefix: prefix, RD: coversFixtureVPNRD,
			PeerASN: 65000, PeerBGPID: coversFixturePeerIP,
			SessionID: coversFixtureSessionID, Seq: 1, StreamSeq: uint64(30 + i),
			PathID: uint32(i + 1), NextHop: "10.9.78.1", Labels: []uint32{78001},
			TsRouter: now, TsCollector: now,
		})
	}

	// The EVPN half, all type 5 -- the IP-prefix route type, and the only
	// one whose prefix column is non-empty. A type-2 or type-3 row carries
	// "" there, which the predicate's row-side guard excludes; that is the
	// right answer (the empty prefix contains nothing) and it is asserted in
	// TestEVPNRoutesCovers rather than assumed, against the EVPN fixture
	// that already holds such rows.
	for i, prefix := range []string{
		coversFixtureEVPNSlash24, coversFixtureEVPNSlash16, coversFixtureEVPNSibling,
	} {
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: coversFixtureRouterIP, RouterSysname: coversFixtureSysname,
			PeerIP: coversFixturePeerIP, RIB: "in_pre",
			RouteType: 5, RD: coversFixtureEVPNRD, Prefix: prefix,
			GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero, EthernetTag: 10,
			PeerASN: 65000, PeerBGPID: coversFixturePeerIP,
			SessionID: coversFixtureSessionID, Seq: 1, StreamSeq: uint64(40 + i),
			PathID: uint32(i + 1), NextHop: "10.9.79.1", Labels: []uint32{79001},
			TsRouter: now, TsCollector: now,
		})
	}
}

// historyFixture* are new routers, peers, sessions and prefixes,
// distinct from every one already claimed in this package (routers 10.0.0.1,
// .3, .5, .7, .9, .20, .30, .40, .42, .50, .60, .70, .80, .90, .100; peers up
// to 10.0.0.101 -- see coversFixtureRouterIP's own comment for the
// addresses already claimed). 10.0.0.110 through .116 sit outside all of
// them, and 1900-1903 sit outside every session_id already claimed (1, 2, 9, 10, 11, 100, 200,
// 700, 701, 900, 1000, 1001, 1100, 1200, 1300, 1400, 1401, 1500, 1599, 1600,
// 1700, 1701, 1800).
//
// The prefixes are 10.110.0.0/24 through 10.115.0.0/24, one per behavior, and
// they are separate prefixes on purpose rather than one prefix carrying every
// case: RouteHistory is filtered by prefix and by nothing else that is
// required, so a shared prefix would make every subtest's count depend on
// every other subtest's rows.
const (
	historyFixtureRouterIP = "10.0.0.110"
	historyFixtureSysname  = "history-fixture-router"
	// historyFixtureUpPeer is up in both sessions below; historyFixtureDownPeer
	// comes up and then goes down inside the CURRENT one, which is the shape
	// that makes "a down peer's events are still history" a real claim rather
	// than one no fixture can make false (see insertFlappedPeerFixture's own
	// comment for the same argument about Peers).
	historyFixtureUpPeer   = "10.0.0.111"
	historyFixtureDownPeer = "10.0.0.112"

	// historyFixtureRouterB carries the filter prefix's third event and has NO
	// peer_events rows at all. That is deliberate: RouteHistory joins nothing,
	// so a router BMP never recorded a session for still has a history, and a
	// fixture whose every router had peer_events could not tell a query that
	// dropped the joins from one that never had them.
	historyFixtureRouterB  = "10.0.0.113"
	historyFixtureSysnameB = "history-fixture-router-b"
	historyFixturePeerB    = "10.0.0.114"

	historyFixtureOldSession = 1900
	historyFixtureCurSession = 1901
	historyFixtureSessionB   = 1902

	// historyFixtureFlapPrefix is announced, withdrawn and re-announced by
	// one peer in one session -- the timeline every current-state query in
	// this package collapses to a single row.
	historyFixtureFlapPrefix = "10.110.0.0/24"
	// historyFixtureOldPrefix is announced ONLY in the superseded session, by
	// a peer that is up right now. Routes discards it (the dump belongs to a
	// session the router has replaced); RouteHistory must not.
	historyFixtureOldPrefix = "10.111.0.0/24"
	// historyFixtureDownPeerPrefix is announced in the current session by the
	// peer that then went down. Routes discards it; RouteHistory must not.
	historyFixtureDownPeerPrefix = "10.112.0.0/24"
	// historyFixtureFilterPrefix carries one event per optional narrowing, so
	// that ?router=, ?peer= and ?rib= each select a differently-sized subset.
	historyFixtureFilterPrefix = "10.113.0.0/24"
	// historyFixtureTiePrefix's four events share one ts_collector to the
	// microsecond, so the ORDER BY's tiebreakers are the only thing that can
	// order them. See insertHistoryFixture for how seq and stream_seq are
	// deliberately made to disagree.
	historyFixtureTiePrefix = "10.114.0.0/24"
)

// historyFixtureFamily is the family every history fixture row carries, and
// it is looked up rather than spelled: sink writes subjects.FamilyToken(f)
// into route_unicast.family, so a hard-coded "ipv4u" here would be a copy of
// the registry that a rename could silently drift from -- the same argument
// unicastFamilies makes at length in filters.go.
var historyFixtureFamily = subjects.FamilyToken(bgp.FamilyIPv4U)

// historyFixtureOnce and historyFixtureBase memoize insertHistoryFixture's
// one write, and the memoization is REQUIRED here rather than an
// optimization.
//
// Every other fixture in this file can be, and several are, inserted by more
// than one test function: their readers all aggregate to a route key with
// argMax, so a second copy of the same logical row collapses into the first
// and the counts hold. RouteHistory has no such aggregation by design -- a
// second insert of the flap prefix is three MORE events in the timeline, and
// "announce, withdraw, re-announce is three events" would read six. The rows
// also cannot simply be made byte-identical across calls, because their
// timestamps are offsets from time.Now() and each call would produce its own.
//
// One write per test binary is the right grain because that is exactly the
// database's own lifetime: chtest.Require drops and recreates the database
// once per binary (see setupDatabase), so a memo scoped to the process and a
// database scoped to the process are created and destroyed together.
var (
	historyFixtureOnce sync.Once
	historyFixtureBase time.Time
)

// insertHistoryFixture writes historyFixture*'s routers, peers and
// route_unicast events and returns the base instant every timestamp below
// is offset from, so a test can name a `since` boundary in the same terms
// the fixture used.
//
// The base is truncated to a microsecond because route_unicast.ts_collector
// is DateTime64(6): an untruncated time.Now() does not survive the round trip
// intact, and a `since` test that asserts an INCLUSIVE boundary would then be
// comparing a nanosecond-precision Go value against a microsecond-precision
// stored one and passing or failing on the rounding.
//
// Two properties of the rows below are load-bearing and neither is visible
// from a count:
//
//   - The flap prefix's ts_router values run in the OPPOSITE order to its
//     ts_collector values -- the newest event carries the Unix epoch, the way
//     a router with a dead clock really does report it, and the oldest
//     carries a timestamp ten hours in the future. A query that ordered this
//     timeline by ts_router returns exactly the same three events in exactly
//     the reverse order, which is why TestRouteHistory asserts the sequence
//     of seq values rather than only the set of actions.
//   - The tie prefix's four events share one ts_collector, and its seq and
//     stream_seq orders CONTRADICT each other (seq 3 carries the lowest
//     stream_seq). Without that, ordering by stream_seq alone would produce
//     the same answer as ordering by (seq, stream_seq) and the seq tiebreaker
//     would be untested.
func insertHistoryFixture(t *testing.T, ctx context.Context, q *Q) time.Time {
	t.Helper()
	historyFixtureOnce.Do(func() { historyFixtureBase = writeHistoryFixture(t, ctx, q) })
	return historyFixtureBase
}

// writeHistoryFixture is insertHistoryFixture's body, split out so that the
// memoization above reads as the one line it is. Call insertHistoryFixture,
// never this.
func writeHistoryFixture(t *testing.T, ctx context.Context, q *Q) time.Time {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Microsecond)

	// Router A's sessions. The up peer's second up event is what advances
	// this router's current session from old to cur (cur is scoped to the
	// whole router -- see peerStateCTE), which is what makes the old-session
	// route below genuinely superseded.
	for _, e := range []struct {
		peerIP, kind            string
		session, seq, streamSeq uint64
	}{
		{historyFixtureUpPeer, "up", historyFixtureOldSession, 1, 1},
		{historyFixtureUpPeer, "up", historyFixtureCurSession, 1, 2},
		{historyFixtureDownPeer, "up", historyFixtureCurSession, 1, 3},
		{historyFixtureDownPeer, "down", historyFixtureCurSession, 2, 4},
	} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: historyFixtureRouterIP, RouterSysname: historyFixtureSysname,
			PeerIP: e.peerIP, RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: e.peerIP,
			SessionID: e.session, Seq: e.seq, StreamSeq: e.streamSeq,
			Kind: e.kind, TsRouter: base, TsCollector: base,
		})
	}

	// The flap: announce, withdraw, re-announce, one peer, one session.
	// ts_router runs backwards against ts_collector on purpose (see above).
	for _, e := range []struct {
		tsCollector, tsRouter time.Time
		seq, streamSeq        uint64
		isWithdraw            uint8
		nextHop               string
		asPath                []uint32
	}{
		{base.Add(-3 * time.Hour), base.Add(10 * time.Hour), 1, 101, 0, "10.9.110.1", []uint32{65110}},
		{base.Add(-2 * time.Hour), base.Add(5 * time.Hour), 2, 102, 1, "", nil},
		{base.Add(-1 * time.Hour), time.Unix(0, 0).UTC(), 3, 103, 0, "10.9.110.3", []uint32{65110}},
	} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: historyFixtureRouterIP, RouterSysname: historyFixtureSysname,
			PeerIP: historyFixtureUpPeer, RIB: "in_pre",
			Family: historyFixtureFamily, Prefix: historyFixtureFlapPrefix,
			PeerASN: 65000, PeerBGPID: historyFixtureUpPeer,
			SessionID: historyFixtureCurSession, Seq: e.seq, StreamSeq: e.streamSeq,
			PathID: 1, IsWithdraw: e.isWithdraw, NextHop: e.nextHop, ASPath: e.asPath,
			TsRouter: e.tsRouter, TsCollector: e.tsCollector,
		})
	}

	// Announced only in the superseded session, by a peer that is up now.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: historyFixtureRouterIP, RouterSysname: historyFixtureSysname,
		PeerIP: historyFixtureUpPeer, RIB: "in_pre",
		Family: historyFixtureFamily, Prefix: historyFixtureOldPrefix,
		PeerASN: 65000, PeerBGPID: historyFixtureUpPeer,
		SessionID: historyFixtureOldSession, Seq: 1, StreamSeq: 104,
		PathID: 1, NextHop: "10.9.111.1", ASPath: []uint32{65111},
		TsRouter: base.Add(-4 * time.Hour), TsCollector: base.Add(-4 * time.Hour),
	})

	// Announced in the current session by the peer that then went down.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: historyFixtureRouterIP, RouterSysname: historyFixtureSysname,
		PeerIP: historyFixtureDownPeer, RIB: "in_pre",
		Family: historyFixtureFamily, Prefix: historyFixtureDownPeerPrefix,
		PeerASN: 65000, PeerBGPID: historyFixtureDownPeer,
		SessionID: historyFixtureCurSession, Seq: 1, StreamSeq: 105,
		PathID: 1, NextHop: "10.9.112.1", ASPath: []uint32{65112},
		TsRouter: base.Add(-90 * time.Minute), TsCollector: base.Add(-90 * time.Minute),
	})

	// One prefix, three events, differing in exactly one dimension each so
	// that router=, peer= and rib= select 2, 2 and 2 of the 3 respectively --
	// three different subsets, none of them the whole set.
	for _, e := range []struct {
		routerIP, sysname, peerIP, rib string
		session, seq, streamSeq        uint64
		offset                         time.Duration
	}{
		{historyFixtureRouterIP, historyFixtureSysname, historyFixtureUpPeer, "in_pre",
			historyFixtureCurSession, 1, 106, -50 * time.Minute},
		{historyFixtureRouterIP, historyFixtureSysname, historyFixtureUpPeer, "loc_rib",
			historyFixtureCurSession, 2, 107, -40 * time.Minute},
		{historyFixtureRouterB, historyFixtureSysnameB, historyFixturePeerB, "in_pre",
			historyFixtureSessionB, 1, 108, -30 * time.Minute},
	} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: e.routerIP, RouterSysname: e.sysname,
			PeerIP: e.peerIP, RIB: e.rib,
			Family: historyFixtureFamily, Prefix: historyFixtureFilterPrefix,
			PeerASN: 65000, PeerBGPID: e.peerIP,
			SessionID: e.session, Seq: e.seq, StreamSeq: e.streamSeq,
			PathID: 1, NextHop: "10.9.113.1", ASPath: []uint32{65113},
			TsRouter: base.Add(e.offset), TsCollector: base.Add(e.offset),
		})
	}

	// Four events at one ts_collector. path_id is the only field that tells
	// them apart in the result, so it is what the ordering subtest asserts.
	// seq descends as stream_seq ASCENDS across the first three, so an
	// ORDER BY that dropped seq reverses them.
	tie := base.Add(-20 * time.Minute)
	for _, e := range []struct {
		pathID         uint32
		seq, streamSeq uint64
	}{
		{1, 3, 110},
		{2, 2, 120},
		{3, 1, 130},
		{4, 1, 140},
	} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: historyFixtureRouterIP, RouterSysname: historyFixtureSysname,
			PeerIP: historyFixtureUpPeer, RIB: "in_pre",
			Family: historyFixtureFamily, Prefix: historyFixtureTiePrefix,
			PeerASN: 65000, PeerBGPID: historyFixtureUpPeer,
			SessionID: historyFixtureCurSession, Seq: e.seq, StreamSeq: e.streamSeq,
			PathID: e.pathID, NextHop: "10.9.114.1", ASPath: []uint32{65114},
			TsRouter: tie, TsCollector: tie,
		})
	}

	return base
}

// historyDuplicateFixture* is a router of its own, used only by
// TestRouteHistoryReportsARedeliveredEventOnce. It is kept apart from
// insertHistoryFixture's router for the reason insertDuplicateRouteFixture is
// kept apart from insertRouteFixture's: manufacturing unmerged duplicates
// requires SYSTEM STOP MERGES around the insert, and mixing that into a
// fixture every other subtest reads would make a future failure ambiguous
// about which behavior broke.
const (
	historyDuplicateFixtureRouterIP  = "10.0.0.115"
	historyDuplicateFixtureSysname   = "history-duplicate-router"
	historyDuplicateFixturePeerIP    = "10.0.0.116"
	historyDuplicateFixtureSessionID = 1903
	historyDuplicateFixturePrefix    = "10.115.0.0/24"
)

// insertHistoryDuplicateFixture writes ONE announcement as two byte-identical
// route_unicast rows -- same seq, same stream_seq, same timestamps, same
// everything -- which is exactly what an at-least-once JetStream redelivery
// of one envelope leaves behind until the next merge (see sink.RowsFor's own
// note on stream_seq, and schema.sql's ReplacingMergeTree comment).
//
// The caller is responsible for disabling merges on route_unicast before
// calling this and for asserting that count() != uniqExact() actually holds
// afterward, exactly as insertDuplicateRouteFixture's caller is: a merge
// winning the race turns the assertion into a tautology that passes whether
// or not RouteHistory is correct.
func insertHistoryDuplicateFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)

	row := routeUnicastFixture{
		RouterIP: historyDuplicateFixtureRouterIP, RouterSysname: historyDuplicateFixtureSysname,
		PeerIP: historyDuplicateFixturePeerIP, RIB: "in_pre",
		Family: historyFixtureFamily, Prefix: historyDuplicateFixturePrefix,
		PeerASN: 65000, PeerBGPID: historyDuplicateFixturePeerIP,
		SessionID: historyDuplicateFixtureSessionID, Seq: 1, StreamSeq: 1,
		PathID: 1, NextHop: "10.9.115.1", ASPath: []uint32{65115},
		TsRouter: now, TsCollector: now,
	}
	insertRouteUnicastEvent(t, ctx, q, row)
	insertRouteUnicastEvent(t, ctx, q, row)
}

// The RIB-walk fixtures below occupy 10.0.1.x, a block no other fixture in
// this package touches -- everything else lives in 10.0.0.x (see the IP
// comments on routeFixtureRouterIP and its successors). Sessions 2000-2049 are
// likewise unused elsewhere. Both matter more here than usual: every test in
// this package shares one database, and a walk that resolved its pin from
// another fixture's peer_events rows would be reading a session it knows
// nothing about.
const (
	ribFixtureRouterIP  = "10.0.1.1"
	ribFixtureSysname   = "rib-walk-router"
	ribFixturePeerIP    = "10.0.1.2" // the peer every unicast walk test pages.
	ribFixtureOtherPeer = "10.0.1.3" // up, same router and rib, its own route.
	ribFixtureDownPeer  = "10.0.1.4" // up then down, with a route on record.
	// ribFixtureOtherRouterIP is a SECOND router carrying the same peer IP,
	// the same rib and -- deliberately -- the same session_id, with one route
	// key that collides with the walked peer's. Sharing a session_id across
	// routers is not a contrivance: session_id is now().UnixNano() assigned by
	// each collector process (see peerStateCTE), and nothing anywhere makes
	// two routers' values differ. Making them equal here is what leaves the
	// walk's own `r.router_ip = ?` predicate as the only thing keeping this
	// router's routes out of the answer, so deleting that predicate is visible
	// instead of masked by the session pin.
	ribFixtureOtherRouterIP = "10.0.1.5"

	ribFixtureRIB      = "in_pre"
	ribFixtureOtherRIB = "in_post"

	ribFixtureCurSession = 2000
	ribFixtureOldSession = 1999
)

// The five route keys a unicast walk over (ribFixtureRouterIP,
// ribFixturePeerIP, ribFixtureRIB) must return, and nothing else.
//
// The prefixes are chosen so that their BYTE order -- which is what both
// ClickHouse's String comparison and Go's own use -- differs from their
// numeric order: "100.64.10.0/24" sorts before "100.64.2.0/24" bytewise and
// after it numerically. A walk whose two halves disagreed about ordering (a cursor
// compared one way, an ORDER BY the other) would still return plausible pages
// over prefixes that sort the same either way, and would skip and repeat rows
// over these.
//
// ribFixtureAddPathPrefix carries two path-ids, so path_id really is part of
// the key rather than a column that happens to be selected: a walk keyed on
// prefix alone has a tie here, and a tie is a dropped route the moment it
// straddles a page boundary.
const (
	ribFixtureAddPathPrefix = "100.64.10.0/24"
	ribFixturePrefixTwo     = "100.64.2.0/24"
	ribFixturePrefixThree   = "100.64.3.0/24"
	ribFixturePrefixLast    = "100.64.200.0/24"
)

// ribFixtureUnicastKeys is that answer, written out as (rib, prefix, path_id)
// triples in no particular order -- the walk tests compare SETS, never
// sequences, so that a test cannot pass merely because it happened to index the
// same rows the query happened to return.
var ribFixtureUnicastKeys = [][]any{
	{ribFixtureRIB, ribFixtureAddPathPrefix, uint32(0)},
	{ribFixtureRIB, ribFixtureAddPathPrefix, uint32(3)},
	{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)},
	{ribFixtureRIB, ribFixturePrefixThree, uint32(0)},
	{ribFixtureRIB, ribFixturePrefixLast, uint32(0)},
}

// ribFixtureUnicastKeysEveryRIB is what the SAME walk must return with rib
// unset: this peer's whole current table, across every rib it reported one
// under.
//
// The two rows for ribFixturePrefixTwo are the point of it. They share a prefix
// and a path_id and differ only in rib, which is the shape a real captured
// archive carries (10.99.1.0/24 and 10.99.2.0/24 sit under in_pre, in_post and
// loc_rib at once), and it is the shape a key without rib in it cannot tell
// apart: two rows, one key, a tie in the walk's own ordering, and a route
// dropped the moment the tie straddles a page boundary. Only a rib-less walk
// over this fixture can fail that way, which is why the walk tests run one.
var ribFixtureUnicastKeysEveryRIB = append(
	append([][]any{}, ribFixtureUnicastKeys...),
	[]any{ribFixtureOtherRIB, ribFixturePrefixTwo, uint32(0)},
	[]any{ribFixtureOtherRIB, ribFixtureOtherRIBPrefix, uint32(0)},
)

// The prefixes that are in route_unicast under this router but must NOT reach
// a walk of (ribFixturePeerIP, ribFixtureRIB). Each one is there to make one
// specific missing predicate visible in the answer rather than merely
// plausible; see insertRIBWalkFixture for which is which.
const (
	ribFixtureWithdrawnPrefix   = "100.64.4.0/24"
	ribFixtureOldSessionPrefix  = "100.64.66.0/24"
	ribFixtureOtherRIBPrefix    = "100.64.99.0/24"
	ribFixtureOtherPeerPrefix   = "100.64.55.0/24"
	ribFixtureDownPeerPrefix    = "100.64.77.0/24"
	ribFixtureOtherRouterPrefix = "100.64.88.0/24"
)

// insertRIBWalkFixture writes the router, four peers and thirteen
// route_unicast rows every unicast RIB-walk test reads.
//
// Five rows are the answer (ribFixtureUnicastKeys). The other eight are each
// placed so that ONE predicate of the walk, deleted, changes the returned key
// SET -- either by adding a key that does not belong or by duplicating one
// that does. A duplicate matters as much as an extra: the walk's keyset
// ordering has no tie-break past the route key, so two rows sharing a key are
// two rows the pager can split across a page boundary and lose one of.
//
//   - 100.64.4.0/24, announced and then withdrawn at a higher seq, under the
//     walked peer and rib. Only HAVING live_is_withdraw = 0 keeps it out.
//   - 100.64.66.0/24, under the walked peer and rib but in the SUPERSEDED
//     session 1999. The cur join keeps it out; so, independently, does the
//     walk's own r.session_id pin.
//   - in_post carries 100.64.2.0/24 path-id 0 -- a key the answer already has
//     -- and 100.64.99.0/24, which it does not. Deleting the rib predicate
//     therefore both duplicates a key and adds one, which is why rib is
//     required for a walk at all (see ribPage).
//   - 100.64.55.0/24 under a DIFFERENT, up peer on the same router and rib. Only
//     the peer predicate keeps it out.
//   - 100.64.77.0/24 under a peer that came up and then went down in the current
//     session. Only the peer_up gate keeps it out; the row itself never leaves
//     route_unicast when a peer drops.
//   - The second router carries 100.64.88.0/24 (a new key) and 100.64.10.0/24
//     path-id 0 (a duplicate of one in the answer), under the same peer IP,
//     the same rib and the same session_id. Only the router predicate keeps
//     them out -- see ribFixtureOtherRouterIP for why the shared session_id is
//     deliberate.
func insertRIBWalkFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	peerUp := func(router, peer string, session, seq, stream uint64, kind string) {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: router, RouterSysname: ribFixtureSysname,
			PeerIP: peer, RIB: ribFixtureRIB,
			PeerASN: 65000, PeerBGPID: peer,
			SessionID: session, Seq: seq, StreamSeq: stream,
			Kind: kind, TsRouter: now, TsCollector: now,
		})
	}
	// The walked peer came up in the superseded session first, then again in
	// the current one: cur is per (collector, router), so it is this second
	// event that makes 2000 the router's current session at all.
	peerUp(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureOldSession, 1, 1, "up")
	peerUp(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureCurSession, 1, 2, "up")
	peerUp(ribFixtureRouterIP, ribFixtureOtherPeer, ribFixtureCurSession, 1, 3, "up")
	peerUp(ribFixtureRouterIP, ribFixtureDownPeer, ribFixtureCurSession, 1, 4, "up")
	peerUp(ribFixtureRouterIP, ribFixtureDownPeer, ribFixtureCurSession, 2, 5, "down")
	peerUp(ribFixtureOtherRouterIP, ribFixturePeerIP, ribFixtureCurSession, 1, 6, "up")

	stream := uint64(100)
	route := func(router, peer, rib, prefix string, pathID uint32, session, seq uint64, withdraw uint8) {
		stream++
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: router, RouterSysname: ribFixtureSysname,
			PeerIP: peer, RIB: rib, Prefix: prefix, PathID: pathID,
			PeerASN: 65000, PeerBGPID: peer,
			SessionID: session, Seq: seq, StreamSeq: stream,
			IsWithdraw: withdraw, NextHop: "10.9.9.9", ASPath: []uint32{65010, 65099},
			TsRouter: now, TsCollector: now,
		})
	}

	// The answer.
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureAddPathPrefix, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureAddPathPrefix, 3, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixturePrefixTwo, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixturePrefixThree, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixturePrefixLast, 0, ribFixtureCurSession, 1, 0)

	// Everything that must not reach it.
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureWithdrawnPrefix, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureWithdrawnPrefix, 0, ribFixtureCurSession, 2, 1)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureOldSessionPrefix, 0, ribFixtureOldSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureOtherRIB, ribFixturePrefixTwo, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixturePeerIP, ribFixtureOtherRIB, ribFixtureOtherRIBPrefix, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixtureOtherPeer, ribFixtureRIB, ribFixtureOtherPeerPrefix, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureRouterIP, ribFixtureDownPeer, ribFixtureRIB, ribFixtureDownPeerPrefix, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureOtherRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureOtherRouterPrefix, 0, ribFixtureCurSession, 1, 0)
	route(ribFixtureOtherRouterIP, ribFixturePeerIP, ribFixtureRIB, ribFixtureAddPathPrefix, 0, ribFixtureCurSession, 1, 0)
}

// ribResetFixture is a router that reconnected: two sessions, routes in the
// older one only.
const (
	ribResetRouterIP   = "10.0.1.20"
	ribResetSysname    = "rib-reset-router"
	ribResetPeerIP     = "10.0.1.21"
	ribResetOldSession = 2010
	ribResetNewSession = 2011
	ribResetPrefixA    = "100.65.30.0/24"
	ribResetPrefixB    = "100.65.31.0/24"
)

// insertRIBResetFixture writes a peer whose current session carries NO routes
// at all, and a superseded one that carries two.
//
// The shape is exactly what a mid-walk reconnect leaves behind at the instant
// the caller comes back for page two: the router has re-dumped from scratch
// and the new dump has not delivered anything yet. A walk pinned to the old
// session has to fail here rather than answer, and a fresh walk has to report
// the new session's (empty) view rather than the old session's two routes --
// those two assertions together are what make ErrSessionChanged mean
// something. Testing only the first would pass against a query that answered
// from the superseded dump forever.
func insertRIBResetFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	for _, e := range []struct {
		session, seq, stream uint64
	}{{ribResetOldSession, 1, 1}, {ribResetNewSession, 1, 2}} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: ribResetRouterIP, RouterSysname: ribResetSysname,
			PeerIP: ribResetPeerIP, RIB: ribFixtureRIB,
			PeerASN: 65000, PeerBGPID: ribResetPeerIP,
			SessionID: e.session, Seq: e.seq, StreamSeq: e.stream,
			Kind: "up", TsRouter: now, TsCollector: now,
		})
	}
	for i, prefix := range []string{ribResetPrefixA, ribResetPrefixB} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: ribResetRouterIP, RouterSysname: ribResetSysname,
			PeerIP: ribResetPeerIP, RIB: ribFixtureRIB, Prefix: prefix,
			PeerASN: 65000, PeerBGPID: ribResetPeerIP,
			SessionID: ribResetOldSession, Seq: 1, StreamSeq: uint64(10 + i),
			NextHop: "10.9.9.9", TsRouter: now, TsCollector: now,
		})
	}
}

// The two-collector fixture: one router, one peer, one session_id, and two
// collectors that both saw every route.
const (
	ribTwoCollectorRouterIP = "10.0.1.10"
	ribTwoCollectorSysname  = "rib-two-collector-router"
	ribTwoCollectorPeerIP   = "10.0.1.11"
	ribTwoCollectorSession  = 2020
	ribCollectorA           = "rib-collector-a"
	ribCollectorB           = "rib-collector-b"
	ribTwoCollectorPrefixA  = "100.65.20.0/24"
	ribTwoCollectorPrefixB  = "100.65.21.0/24"
)

// insertRIBTwoCollectorFixture writes the deployment peerStateCTE's own doc
// comment describes as unsupported-but-not-silent: two collectors monitoring
// one router, each with its own complete view.
//
// Both collectors carry the SAME session_id, which is the whole point. Two
// collector processes assign session_id from their own clocks with nothing
// shared between them (see peerStateCTE), so equal values are possible and
// nothing prevents them -- and while they are equal, the walk's r.session_id
// pin cannot tell one collector's rows from the other's. Only r.collector_id
// can, which makes this the fixture that proves that predicate is carrying
// weight: with it, a walk pinned to collector A returns two routes; without
// it, four rows under two keys, each key duplicated.
func insertRIBTwoCollectorFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	for i, collector := range []string{ribCollectorA, ribCollectorB} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: ribTwoCollectorRouterIP, RouterSysname: ribTwoCollectorSysname,
			PeerIP: ribTwoCollectorPeerIP, RIB: ribFixtureRIB, Collector: collector,
			PeerASN: 65000, PeerBGPID: ribTwoCollectorPeerIP,
			SessionID: ribTwoCollectorSession, Seq: 1, StreamSeq: uint64(1 + i),
			Kind: "up", TsRouter: now, TsCollector: now,
		})
		for j, prefix := range []string{ribTwoCollectorPrefixA, ribTwoCollectorPrefixB} {
			insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
				RouterIP: ribTwoCollectorRouterIP, RouterSysname: ribTwoCollectorSysname,
				PeerIP: ribTwoCollectorPeerIP, RIB: ribFixtureRIB, Prefix: prefix,
				Collector: collector,
				PeerASN:   65000, PeerBGPID: ribTwoCollectorPeerIP,
				SessionID: ribTwoCollectorSession, Seq: 1, StreamSeq: uint64(20 + 2*i + j),
				NextHop: "10.9.9.9", TsRouter: now, TsCollector: now,
			})
		}
	}
}

// The VPN walk fixture: five route keys under one (router, peer, rib), plus
// the two exclusions a VPN walk shares with the unicast one.
const (
	ribVPNRouterIP = "10.0.1.30"
	ribVPNSysname  = "rib-vpn-router"
	ribVPNPeerIP   = "10.0.1.31"
	ribVPNSession  = 2030

	ribVPNRDNone = "" // lu4's own shape: a labeled-unicast route carries no RD.
	ribVPNRDOne  = "65100:1"
	ribVPNRDTwo  = "65100:2"
)

// ribFixtureVPNKeys is the answer a VPN walk over (ribVPNRouterIP,
// ribVPNPeerIP, ribFixtureRIB) must return, as (rd, prefix, path_id) triples.
//
// The set is built so every component of the key does work. The RD-less lu4
// route shares nothing with the rest and sorts first; one prefix appears under
// two different RDs, which is two routes and not one (see vpnRoutesSQL); and
// one (rd, prefix) pair appears under two path-ids. A walk keyed on prefix
// alone ties three of these five together.
var ribFixtureVPNKeys = [][]any{
	{ribFixtureRIB, ribVPNRDNone, "100.66.5.0/24", uint32(0)},
	{ribFixtureRIB, ribVPNRDOne, "100.66.1.0/24", uint32(0)},
	{ribFixtureRIB, ribVPNRDOne, "100.66.1.0/24", uint32(7)},
	{ribFixtureRIB, ribVPNRDTwo, "100.66.9.0/24", uint32(0)},
	{ribFixtureRIB, ribVPNRDTwo, "100.66.1.0/24", uint32(0)},
}

// ribFixtureVPNKeysEveryRIB is the same walk with rib unset. Its in_post copy
// of (ribVPNRDOne, 100.66.1.0/24, 0) is route_vpn's counterpart to the unicast
// fixture's cross-rib pair -- see ribFixtureUnicastKeysEveryRIB.
var ribFixtureVPNKeysEveryRIB = append(
	append([][]any{}, ribFixtureVPNKeys...),
	[]any{ribFixtureOtherRIB, ribVPNRDOne, "100.66.1.0/24", uint32(0)},
	[]any{ribFixtureOtherRIB, ribVPNRDOne, ribVPNOtherRIBPrefix, uint32(0)},
)

const (
	ribVPNWithdrawnPrefix = "100.66.19.0/24"
	ribVPNOtherRIBPrefix  = "100.66.99.0/24"
)

func insertRIBVPNFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: ribVPNRouterIP, RouterSysname: ribVPNSysname,
		PeerIP: ribVPNPeerIP, RIB: ribFixtureRIB,
		PeerASN: 65000, PeerBGPID: ribVPNPeerIP,
		SessionID: ribVPNSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	stream := uint64(100)
	route := func(rib, family, rd, prefix string, pathID uint32, withdraw uint8, seq uint64) {
		stream++
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: ribVPNRouterIP, RouterSysname: ribVPNSysname,
			PeerIP: ribVPNPeerIP, RIB: rib,
			Family: family, Prefix: prefix, RD: rd, PathID: pathID,
			PeerASN: 65000, PeerBGPID: ribVPNPeerIP,
			SessionID: ribVPNSession, Seq: seq, StreamSeq: stream,
			IsWithdraw: withdraw, NextHop: "10.9.30.1", Labels: []uint32{24001},
			RouteTargets: []string{"65001:100"},
			TsRouter:     now, TsCollector: now,
		})
	}
	for _, k := range ribFixtureVPNKeys {
		// k[0] is the rib the key leads with; the fixture writes them all
		// under ribFixtureRIB and adds the cross-rib rows below by hand.
		rd, prefix, pathID := k[1].(string), k[2].(string), k[3].(uint32)
		family := "vpn4"
		if rd == ribVPNRDNone {
			family = "lu4"
		}
		route(k[0].(string), family, rd, prefix, pathID, 0, 1)
	}
	// Announced then withdrawn, under the walked peer and rib.
	route(ribFixtureRIB, "vpn4", ribVPNRDOne, ribVPNWithdrawnPrefix, 0, 0, 1)
	route(ribFixtureRIB, "vpn4", ribVPNRDOne, ribVPNWithdrawnPrefix, 0, 1, 2)
	// Under a rib the walk did not ask for.
	route(ribFixtureOtherRIB, "vpn4", ribVPNRDOne, ribVPNOtherRIBPrefix, 0, 0, 1)
	// A key the answer already has, under that same other rib: deleting the
	// walk's rib predicate duplicates it rather than merely adding a row.
	route(ribFixtureOtherRIB, "vpn4", ribVPNRDOne, "100.66.1.0/24", 0, 0, 1)
}

// The EVPN walk fixture: five NLRI tuples under one (router, peer, rib), shaped
// so that every component of the eight-column key separates some pair of them.
const (
	ribEVPNRouterIP = "10.0.1.40"
	ribEVPNSysname  = "rib-evpn-router"
	ribEVPNPeerIP   = "10.0.1.41"
	ribEVPNSession  = 2040

	ribEVPNRDOne = "65101:1"
	ribEVPNRDTwo = "65101:2"
	ribEVPNMac   = "00:50:79:66:70:01"
	ribEVPNIP    = "100.66.0.1"
	ribEVPNESI   = "00000000000000000001"
)

// ribFixtureEVPNKeys is the answer an EVPN walk must return, as the full NLRI
// tuple (route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id).
//
// Consecutive entries differ in exactly one component wherever that is
// possible: rows 1 and 2 only in ip, rows 2 and 3 only in esi, rows 4 and 5 in
// route_type and everything a type-5 route carries that a type-3 does not.
// Shorten the key by any one column and two of these collapse into a tie.
var ribFixtureEVPNKeys = [][]any{
	{ribFixtureRIB, uint8(2), ribEVPNRDOne, "", ribEVPNMac, "", uint32(0), "", uint32(0)},
	{ribFixtureRIB, uint8(2), ribEVPNRDOne, "", ribEVPNMac, ribEVPNIP, uint32(0), "", uint32(0)},
	{ribFixtureRIB, uint8(2), ribEVPNRDOne, "", ribEVPNMac, ribEVPNIP, uint32(0), ribEVPNESI, uint32(0)},
	{ribFixtureRIB, uint8(3), ribEVPNRDOne, "", "", "", uint32(100), "", uint32(0)},
	{ribFixtureRIB, uint8(5), ribEVPNRDTwo, "100.66.95.0/24", "", "", uint32(0), "", uint32(2)},
}

// ribFixtureEVPNKeysEveryRIB is the same walk with rib unset. Its in_post row
// is a byte-for-byte copy of the first NLRI under a different rib -- route_evpn's
// counterpart to the unicast fixture's cross-rib pair, and the only way an
// eight-column key can still be one column short.
var ribFixtureEVPNKeysEveryRIB = append(
	append([][]any{}, ribFixtureEVPNKeys...),
	append([]any{ribFixtureOtherRIB}, ribFixtureEVPNKeys[0][1:]...),
)

const ribEVPNWithdrawnMac = "00:50:79:66:70:99"

func insertRIBEVPNFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: ribEVPNRouterIP, RouterSysname: ribEVPNSysname,
		PeerIP: ribEVPNPeerIP, RIB: ribFixtureRIB,
		PeerASN: 65000, PeerBGPID: ribEVPNPeerIP,
		SessionID: ribEVPNSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	stream := uint64(100)
	// key is a full page key, rib first; rib comes from the caller so the
	// cross-rib row below can reuse one NLRI under a second rib.
	route := func(rib string, key []any, withdraw uint8, seq uint64) {
		stream++
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: ribEVPNRouterIP, RouterSysname: ribEVPNSysname,
			PeerIP: ribEVPNPeerIP, RIB: rib,
			RouteType: key[1].(uint8), RD: key[2].(string), Prefix: key[3].(string),
			MAC: key[4].(string), IP: key[5].(string),
			EthernetTag: key[6].(uint32), ESI: key[7].(string), PathID: key[8].(uint32),
			NextHop: "10.9.40.1",
			PeerASN: 65000, PeerBGPID: ribEVPNPeerIP,
			SessionID: ribEVPNSession, Seq: seq, StreamSeq: stream,
			IsWithdraw: withdraw, Labels: []uint32{100},
			TsRouter: now, TsCollector: now,
		})
	}
	for _, k := range ribFixtureEVPNKeys {
		route(ribFixtureRIB, k, 0, 1)
	}
	// Announced then withdrawn, under the walked peer and rib.
	withdrawn := []any{ribFixtureRIB, uint8(2), ribEVPNRDOne, "", ribEVPNWithdrawnMac, "", uint32(0), "", uint32(0)}
	route(ribFixtureRIB, withdrawn, 0, 1)
	route(ribFixtureRIB, withdrawn, 1, 2)
	// A key the answer already has, under a rib the walk did not ask for.
	route(ribFixtureOtherRIB, ribFixtureEVPNKeys[0], 0, 1)
}

// The path-attribute fixture's own topology, distinct from every value this
// package already claims: routers run out at 10.0.1.5 in the RIB block and at
// 10.0.0.116 in the history block, so 10.0.0.120/.121 sit outside both, and
// session 2100 sits past the 2000-2049 the RIB walks reserve. Its prefixes are
// in 10.120/16 and 192.168.120/24, neither of which contains
// coversFixtureTarget or any other address TestRoutesCovers asks about -- a
// covers query is the one question in this package that is answered
// fleet-wide, so a new prefix that happened to cover 10.77.0.33 would change
// another test's answer without touching its code.
//
// The RD block is 65120:x for the same reason insertEVPNRouteFixture reserves
// 65090:x: TestVPNRoutesFilters and TestEVPNRoutesFilters both ask for an rd
// with no router scope, so an RD reused across fixtures is a count that
// depends on which fixtures ran.
const (
	pathAttrFixtureRouterIP  = "10.0.0.120"
	pathAttrFixtureSysname   = "path-attr-fixture-router"
	pathAttrFixturePeerIP    = "10.0.0.121"
	pathAttrFixtureSessionID = 2100

	// pathAttrPrefixAll carries every attribute at once, with values chosen
	// to be pairwise distinct -- see pathAttrMED and pathAttrLocalPref.
	pathAttrPrefixAll = "10.120.0.0/24"
	// pathAttrPrefixZeroMED carries MED 0 and no local-pref at all. It is the
	// row that makes "absent" and "present and zero" two different answers
	// rather than one, and it makes them different in BOTH directions on a
	// single row: a Route type that defaulted its way past NULL reads MED 0
	// and LocalPref 0 here, which is right for one field and wrong for the
	// other, and no assertion about a row carrying only one of the two could
	// tell those apart.
	pathAttrPrefixZeroMED = "10.120.1.0/24"
	// pathAttrPrefixCleared is advertised WITH a MED, a local-pref and
	// communities, then re-advertised at a higher seq carrying none of them
	// -- an ordinary thing for a router to do, and the shape that catches
	// ClickHouse's NULL-skipping argMax. A plain argMax(med, ...) resolves to
	// the newest NON-NULL observation, so this route keeps reporting
	// pathAttrClearedMED forever; argMax(tuple(med), ...).1 reports the
	// newest observation, NULL included. See routesSQL's own doc comment.
	pathAttrPrefixCleared = "10.120.2.0/24"
	pathAttrClearedMED    = uint32(4242)

	pathAttrVPNPrefix = "192.168.120.0/24"
	pathAttrVPNRD     = "65120:1"
	pathAttrEVPNRD    = "65120:2"
	pathAttrEVPNMAC   = "00:50:79:66:12:01"

	// The VPN and EVPN counterparts of pathAttrPrefixCleared: the same route
	// re-advertised with its MED and local-pref gone. They exist because the
	// argMax(tuple(...), ...).1 form is written out THREE times, once per
	// statement, and a copy is a chance to get it wrong on its own. Without
	// these two rows the unicast statement is the only one whose nullable
	// aggregation any test can falsify -- verified by mutation: reverting
	// vpnRoutesSQL alone to a plain argMax left the whole package green.
	pathAttrVPNRDCleared     = "65120:3"
	pathAttrVPNPrefixCleared = "192.168.121.0/24"
	pathAttrEVPNRDCleared    = "65120:4"
	pathAttrEVPNMACCleared   = "00:50:79:66:12:02"

	// pathAttrMED and pathAttrLocalPref are DIFFERENT numbers, and that is
	// the whole of what makes a transposed pair in the SELECT list or the
	// Scan visible: med and local_pref are adjacent columns of one type
	// (Nullable(UInt32)) in all three statements, so a swap between them
	// raises nothing, changes no row count, and is invisible to any fixture
	// that gives them the same value. They are also both non-zero, so
	// neither can be confused with the defaulted zero this pair exists to
	// keep distinguishable.
	pathAttrMED       = uint32(100)
	pathAttrLocalPref = uint32(200)
)

// pathAttrCommunities is the raw Array(UInt32) the fixture writes, and
// pathAttrCommunityText is what communityStrings must render it as. The four
// values are chosen so that no plausibly-wrong rendering produces all four:
//
//	65000<<16|100  the contract's own example, "65000:100".
//	0xFFFFFF01     NO_EXPORT. Its high half is 65535 and its low half 65281,
//	               so a rendering that treated the halves as signed, or that
//	               shifted arithmetically, reads negative here and nowhere
//	               else. It is also the value a name table would be tempted
//	               to render as "NO_EXPORT" -- the contract asks for
//	               notation, not nomenclature (see communityStrings).
//	300            High half zero: "0:300". A rendering that dropped the high
//	               half entirely, or printed only the raw value, still looks
//	               plausible on the first two and not on this one.
//	65002<<16      Low half zero: "65002:0". The mirror of the above, and the
//	               one a mask of the wrong width turns into "65002:65536" or
//	               into a second copy of the high half.
//
// The order is preserved rather than sorted, which the values also make
// visible: sorted numerically they would be 300, 4259840100, 4259971072,
// 4294967041, which is not the order below.
var (
	pathAttrCommunities   = []uint32{65000<<16 | 100, 0xFFFFFF01, 300, 65002 << 16}
	pathAttrCommunityText = []string{"65000:100", "65535:65281", "0:300", "65002:0"}

	// pathAttrLargeCommunities is already text in the column: sink renders
	// RFC 8092's "global:local1:local2" before the row is written (see
	// sink's formatLargeCommunity), so query/ passes it through untouched
	// and this is both the written and the expected value. The second
	// element's global administrator is past 2^32-1's own 16-bit halves on
	// purpose: a large community is three 32-bit fields, and anything here
	// that reused communityStrings' 16-bit split would mangle it.
	pathAttrLargeCommunities = []string{"65000:1:2", "4294967295:0:1"}
)

// pathAttrASPath is deliberately unlike pathAttrCommunities in both length
// and magnitude. as_path and communities are both Array(UInt32) on all three
// tables, and two same-typed array columns are the other pair a transposition
// can hide between; a fixture that gave them similar-looking contents would
// let one be reported as the other.
var pathAttrASPath = []uint32{65010, 65120}

// u32p is the one-line "address of a literal" this file needs for the
// Nullable(UInt32) fixture fields. It exists because &uint32(0) is not
// something Go will write, and because a package-level var per value would
// let two rows share a pointer -- harmless here, but the kind of aliasing a
// later edit turns into one row's edit changing another's.
//

// insertPathAttrFixture writes one router, one up peer and ten route rows --
// four unicast, three VPN, three EVPN -- covering the four RouteCommon
// attributes every route type carries and, on all three, the two ways a
// nullable one can be got wrong.
//
// One fixture serves all three families rather than three serving one each,
// because the claim under test is identical on all three: the columns are the
// same columns, the statements aggregate them the same way, and the scan
// loops render them with the same function. Three fixtures would be three
// places for that to stop being true silently. What differs per family is
// only which OTHER fields sit beside them in the SELECT list -- which is
// exactly what the per-family tests assert.
//
// Every peer here is up, every route live, no session is superseded and
// nothing is withdrawn: this fixture's job is to make the ATTRIBUTES the only
// thing that can change an answer. The down-peer, stale-session,
// withdrawn-route and add-path hazards all have fixtures of their own, and
// mixing one in here would make a failure ambiguous between two causes.
func insertPathAttrFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	// 10.120.0.0/24 -- every attribute set, all of them distinguishable.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre", Prefix: pathAttrPrefixAll,
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 1, StreamSeq: 10,
		PathID: 1, NextHop: "10.9.120.1", ASPath: pathAttrASPath,
		MED: new(pathAttrMED), LocalPref: new(pathAttrLocalPref),
		Communities: pathAttrCommunities, LargeCommunities: pathAttrLargeCommunities,
		TsRouter: now, TsCollector: now,
	})

	// 10.120.1.0/24 -- MED present and zero, local-pref absent, on one row.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre", Prefix: pathAttrPrefixZeroMED,
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 1, StreamSeq: 11,
		PathID: 1, NextHop: "10.9.120.2",
		MED: new(uint32(0)), LocalPref: nil,
		TsRouter: now, TsCollector: now,
	})

	// 10.120.2.0/24 -- advertised with attributes, then re-advertised at a
	// higher seq with none. Both rows stay in route_unicast; only the newest
	// observation is this route's current state.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre", Prefix: pathAttrPrefixCleared,
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 1, StreamSeq: 12,
		PathID: 1, NextHop: "10.9.120.3", ASPath: pathAttrASPath,
		MED: new(pathAttrClearedMED), LocalPref: new(pathAttrClearedMED),
		Communities: pathAttrCommunities, LargeCommunities: pathAttrLargeCommunities,
		TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre", Prefix: pathAttrPrefixCleared,
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 2, StreamSeq: 13,
		PathID: 1, NextHop: "10.9.120.3", ASPath: pathAttrASPath,
		MED: nil, LocalPref: nil,
		TsRouter: now, TsCollector: now,
	})

	// The VPN row. vpn4 rather than lu4 so that Family carries a token RD
	// alone could not have implied -- an RD-less lu4 row is told apart from a
	// VPN one by the family column and by nothing else.
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre",
		Family: "vpn4", Prefix: pathAttrVPNPrefix, RD: pathAttrVPNRD,
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 1, StreamSeq: 14,
		PathID: 1, NextHop: "10.9.120.4", ASPath: pathAttrASPath,
		Labels: []uint32{120001}, RouteTargets: []string{"65120:100"},
		MED: new(pathAttrMED), LocalPref: new(pathAttrLocalPref),
		Communities: pathAttrCommunities, LargeCommunities: pathAttrLargeCommunities,
		TsRouter: now, TsCollector: now,
	})

	// The VPN route that loses its MED and local-pref on re-advertisement.
	// Both rows stay in route_vpn; only the newest is this route's state.
	for _, r := range []struct {
		seq, stream uint64
		med, lp     *uint32
	}{
		{1, 16, new(pathAttrClearedMED), new(pathAttrClearedMED)},
		{2, 17, nil, nil},
	} {
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
			PeerIP: pathAttrFixturePeerIP, RIB: "in_pre",
			Family: "vpn4", Prefix: pathAttrVPNPrefixCleared, RD: pathAttrVPNRDCleared,
			PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
			SessionID: pathAttrFixtureSessionID, Seq: r.seq, StreamSeq: r.stream,
			PathID: 1, NextHop: "10.9.120.6", ASPath: pathAttrASPath,
			Labels: []uint32{120003},
			MED:    r.med, LocalPref: r.lp,
			TsRouter: now, TsCollector: now,
		})
	}

	// The EVPN row. Every route_evpn row in a real captured archive leaves
	// all four of these columns empty, which is a fact about the EVPN
	// deployments seen so far and not about the columns -- so this row is
	// the only thing that can show EVPNRoutes reads them at all.
	insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
		RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
		PeerIP: pathAttrFixturePeerIP, RIB: "in_pre",
		RouteType: 2, RD: pathAttrEVPNRD, MAC: pathAttrEVPNMAC,
		ESI: evpnFixtureESIZero, EthernetTag: 10,
		PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
		SessionID: pathAttrFixtureSessionID, Seq: 1, StreamSeq: 15,
		PathID: 1, NextHop: "10.9.120.5", ASPath: pathAttrASPath,
		Labels: []uint32{120002}, RouteTargets: []string{"65120:100"},
		MED: new(pathAttrMED), LocalPref: new(pathAttrLocalPref),
		Communities: pathAttrCommunities, LargeCommunities: pathAttrLargeCommunities,
		TsRouter: now, TsCollector: now,
	})

	// The EVPN route that loses its MED and local-pref on re-advertisement,
	// the third copy of the shape pathAttrPrefixCleared pins for unicast and
	// pathAttrVPNRDCleared for VPN -- one per statement, because the
	// aggregation is written out once per statement.
	for _, r := range []struct {
		seq, stream uint64
		med, lp     *uint32
	}{
		{1, 18, new(pathAttrClearedMED), new(pathAttrClearedMED)},
		{2, 19, nil, nil},
	} {
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			RouterIP: pathAttrFixtureRouterIP, RouterSysname: pathAttrFixtureSysname,
			PeerIP: pathAttrFixturePeerIP, RIB: "in_pre",
			RouteType: 2, RD: pathAttrEVPNRDCleared, MAC: pathAttrEVPNMACCleared,
			ESI: evpnFixtureESIZero, EthernetTag: 10,
			PeerASN: 65000, PeerBGPID: pathAttrFixturePeerIP,
			SessionID: pathAttrFixtureSessionID, Seq: r.seq, StreamSeq: r.stream,
			PathID: 1, NextHop: "10.9.120.7", ASPath: pathAttrASPath,
			Labels: []uint32{120004},
			MED:    r.med, LocalPref: r.lp,
			TsRouter: now, TsCollector: now,
		})
	}
}

// twoRibRouter and its two peers are the only fixture in this package that
// writes a peer under more than one rib: every other insertPeerEvent call
// hard-codes RIB: "in_pre", so without this one no result in the package
// could tell a per-peer count from a per-(peer, rib) one -- the defect
// TestRoutersCountsPeersNotRibViews pins.
//
// Both shapes of the defect are here, because they are different failures
// and only one of them is a double count:
//
//	10.0.0.141  up under in_pre AND up under in_post.
//	            Counting peer_state rows reports this ONE peer as two peers
//	            up. Nothing about the router changed; it simply mirrors both
//	            adj-RIB-in views, which is what pre-/post-policy monitoring
//	            IS.
//
//	10.0.0.142  up under in_pre, then down under in_post at a higher seq.
//	            Counting peer_state rows puts this ONE peer in BOTH counts,
//	            so peers_up + peers_down comes to more than the router has
//	            peers. peer_up resolves it to the newest event across every
//	            rib -- down -- which is the peer-level fact a "how many of
//	            this router's peers are up" answer is asking for.
//
// So the correct answer for this router is 1 up and 1 down against 2 peers;
// counting peer_state rows gives 3 and 1 against the same 2 peers. Both
// numbers are plausible and neither raises, which is why this needed a
// fixture rather than a reading.
//
// 10.0.0.142's down event carries the higher seq AND the higher stream_seq,
// so peer_up's argMax lands on it under either tiebreaker. That is
// deliberate: this fixture is about the rib dimension, and
// insertFlappedPeerFixture already pins the seq-versus-stream_seq ordering
// on its own. Two fixtures asserting one thing each fail one at a time.
const (
	twoRibRouter    = "10.0.0.140"
	twoRibSysname   = "two-rib-router"
	twoRibUpPeer    = "10.0.0.141"
	twoRibSplitPeer = "10.0.0.142"
	twoRibSessionID = 2210
)

func insertTwoRibPeerFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	for _, e := range []struct {
		peerIP         string
		rib            string
		kind           string
		seq, streamSeq uint64
	}{
		{twoRibUpPeer, "in_pre", "up", 1, 1},
		{twoRibUpPeer, "in_post", "up", 1, 2},
		{twoRibSplitPeer, "in_pre", "up", 1, 3},
		{twoRibSplitPeer, "in_post", "down", 2, 4},
	} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: twoRibRouter, RouterSysname: twoRibSysname,
			PeerIP: e.peerIP, RIB: e.rib,
			PeerASN: 65000, PeerBGPID: e.peerIP,
			SessionID: twoRibSessionID, Seq: e.seq, StreamSeq: e.streamSeq,
			Kind: e.kind, TsRouter: now, TsCollector: now,
		})
	}
}

// mappedNextHop and its three route rows exist because next_hop is a String
// column and netip.ParseAddr honors whatever text is in it. A row whose
// next_hop reads "::ffff:10.130.9.1" parses to the IPv4-MAPPED form, which
// prints mapped and compares unequal to the plain address -- so a scan loop
// that does not unmap it hands a caller one address in two spellings inside
// a single JSON object. See unmapAll.
//
// The text is not hypothetical. bgp/update.go and bgp/nlri.go build next
// hops through netip.AddrFrom16 for the 16-byte forms on the wire, which is
// exactly the value that renders mapped, and sink writes the rendering.
//
// One row per route table, and the unicast row doubles as RouteHistory's:
// all four scan loops carried the same false comment ("netip.ParseAddr
// above never produces the IPv4-mapped form to begin with"), so all four
// need a row to prove otherwise. mappedNextHopPlain is what each of them
// must return.
const (
	mappedNextHopRouterIP  = "10.0.0.130"
	mappedNextHopSysname   = "mapped-next-hop-router"
	mappedNextHopPeerIP    = "10.0.0.131"
	mappedNextHopSessionID = 2200

	// The stored text and the address it denotes. Storing the mapped
	// spelling of an address that IS IPv4 is the whole point: Unmap is a
	// no-op on anything else, so a v6-only next hop would prove nothing.
	mappedNextHopStored = "::ffff:10.130.9.1"
	mappedNextHopPlain  = "10.130.9.1"

	mappedNextHopUnicastPrefix = "10.130.1.0/24"
	mappedNextHopVPNPrefix     = "10.130.2.0/24"
	mappedNextHopVPNRD         = "65130:1"
	mappedNextHopEVPNPrefix    = "10.130.3.0/24"
	mappedNextHopEVPNRD        = "65130:2"
)

func insertMappedNextHopFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: mappedNextHopRouterIP, RouterSysname: mappedNextHopSysname,
		PeerIP: mappedNextHopPeerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: mappedNextHopPeerIP,
		SessionID: mappedNextHopSessionID, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: mappedNextHopRouterIP, RouterSysname: mappedNextHopSysname,
		PeerIP: mappedNextHopPeerIP, RIB: "in_pre", Family: "ipv4u",
		Prefix:  mappedNextHopUnicastPrefix,
		PeerASN: 65000, PeerBGPID: mappedNextHopPeerIP,
		SessionID: mappedNextHopSessionID, Seq: 1, StreamSeq: 2,
		PathID: 1, NextHop: mappedNextHopStored, ASPath: []uint32{65130},
		TsRouter: now, TsCollector: now,
	})
	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		RouterIP: mappedNextHopRouterIP, RouterSysname: mappedNextHopSysname,
		PeerIP: mappedNextHopPeerIP, RIB: "in_pre", Family: "vpn4",
		Prefix: mappedNextHopVPNPrefix, RD: mappedNextHopVPNRD,
		PeerASN: 65000, PeerBGPID: mappedNextHopPeerIP,
		SessionID: mappedNextHopSessionID, Seq: 1, StreamSeq: 3,
		PathID: 1, NextHop: mappedNextHopStored, Labels: []uint32{130001},
		TsRouter: now, TsCollector: now,
	})
	insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
		RouterIP: mappedNextHopRouterIP, RouterSysname: mappedNextHopSysname,
		PeerIP: mappedNextHopPeerIP, RIB: "in_pre",
		RouteType: 5, RD: mappedNextHopEVPNRD, Prefix: mappedNextHopEVPNPrefix,
		GatewayIP: "0.0.0.0", ESI: evpnFixtureESIZero, EthernetTag: 10,
		PeerASN: 65000, PeerBGPID: mappedNextHopPeerIP,
		SessionID: mappedNextHopSessionID, Seq: 1, StreamSeq: 4,
		PathID: 1, NextHop: mappedNextHopStored, Labels: []uint32{130002},
		TsRouter: now, TsCollector: now,
	})
}

// extCommFixturePrefix is this fixture's own prefix, deliberately NOT
// 10.200.0.0/24: that address is insertDuplicateRouteFixture's own i=0
// ("10.200.%d.0/24" for i in [0,100)), and TestRoutesReturnsExtCommunitiesAndRouteTargets
// filters on prefix alone, with no router/peer/rib narrowing -- the shape
// every /v1/routes/unicast?prefix= call takes. Sharing a prefix with a
// route another fixture leaves permanently "up" would hand this test two
// routes instead of one, whichever suite order it runs in. 10.202.0.0/24
// sits outside that range and outside insertRouteCountFixture's own
// 10.201.0-4.0/24.
const extCommFixturePrefix = "10.202.0.0/24"

// insertExtCommunityFixture writes one unicast route carrying both an
// extended community set and a route target, under a peer that is up.
// The two ext communities are deliberately different TYPES -- one rt, one
// soo -- so a test asserting on the array cannot pass by returning only the
// route targets under a different name.
//
// Its router and peer (10.0.0.160/.161) and session (2300) are its own,
// distinct from every one already claimed elsewhere in this package (see
// perFamilyDumpFixtureRouterIP's own comment for the addresses already
// claimed, and mappedNextHopFixture's constants for the highest entry
// claimed after it): 10.0.0.61 and 10.0.0.62 are already perFamilyDumpFixture's peer and
// evpnOnlyDumpFixture's router respectively, and 700 is
// routeFixtureOldSession's.
func insertExtCommunityFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const (
		sysname  = "extcomm-router"
		routerIP = "10.0.0.160"
		peerIP   = "10.0.0.161"
		sid      = uint64(2300)
	)
	ts := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname,
		PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65001, PeerBGPID: routerIP,
		SessionID: sid, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: ts, TsCollector: ts,
	})

	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routerIP, RouterSysname: sysname,
		PeerIP: peerIP, RIB: "in_pre", Prefix: extCommFixturePrefix,
		NextHop: peerIP,
		PeerASN: 65001, PeerBGPID: routerIP,
		SessionID: sid, Seq: 1, StreamSeq: 1,
		TsRouter: ts, TsCollector: ts,
		ASPath:         []uint32{65001},
		ExtCommunities: []string{"rt:65101:1", "soo:65000:777"},
		RouteTargets:   []string{"65101:1"},
	})
}

// originFixtureMatch, originFixtureOther, originFixtureNoAS and repathPrefix
// are new prefixes, continuing the 10.20x.0.0/24 convention
// insertRouteCountFixture and insertExtCommunityFixture already established
// rather than picking a fresh range: 10.201.0-4.0/24 are
// insertRouteCountFixture's (routeCountFixtureMarkerPeer and
// routeCountFixtureWithdrawPeer's own prefixes) and 10.202.0.0/24 is
// extCommFixturePrefix's, both already claimed, so these fixtures pick
// up at 10.203 and 10.204, the next two ranges free of either.
const (
	originFixtureMatch = "10.203.0.0/24" // as_path [64500 65100], origin 65100
	originFixtureOther = "10.203.1.0/24" // as_path [64500 65200], origin 65200
	originFixtureNoAS  = "10.203.2.0/24" // as_path [],            origin unknown

	repathPrefix    = "10.204.0.0/24"
	repathOldOrigin = uint32(65310) // seq 1: as_path [64500 65310]
	repathNewOrigin = uint32(65320) // seq 2: as_path [64500 65320]  <- live
)

// insertOriginFixture writes three routes under one up peer: one originated
// by 65100, one by 65200, and one with no AS path at all. The third is the
// one that matters -- see TestRoutesOriginASNIgnoresAnEmptyASPath.
//
// Its router, peer and session (10.0.0.170/10.0.0.171/710) are its own,
// chosen past every router/peer IP already claimed in this package (the
// highest is 10.0.0.161, insertExtCommunityFixture's peer -- see its own
// comment for the addresses already claimed). 10.0.0.71-75, the obvious
// next block, already belong to routeFilterFixtureRouterB and its three
// peers (see that fixture's own comment), so this one sits well clear of
// them instead of one collision away.
func insertOriginFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	insertPathFixture(t, ctx, q, "origin-fixture", "10.0.0.170", "10.0.0.171", 710,
		[]pathRow{
			{originFixtureMatch, []uint32{64500, 65100}},
			{originFixtureOther, []uint32{64500, 65200}},
			{originFixtureNoAS, []uint32{}},
		})
}

// insertRepathedFixture writes ONE route key observed twice, the second
// observation carrying a different AS path. Its live origin is
// repathNewOrigin; repathOldOrigin appears only in the superseded row, which
// is what makes a WHERE-based filter visibly wrong.
//
// Its router, peer and session (10.0.0.172/10.0.0.173/720) are
// insertOriginFixture's own neighbors in the same range, for the same reason.
func insertRepathedFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	insertPathFixture(t, ctx, q, "repath-fixture", "10.0.0.172", "10.0.0.173", 720,
		[]pathRow{
			{repathPrefix, []uint32{64500, repathOldOrigin}}, // seq 1
			{repathPrefix, []uint32{64500, repathNewOrigin}}, // seq 2, live
		})
}

type pathRow struct {
	prefix string
	path   []uint32
}

// repathCommunityPrefix, repathCommunityOld and repathCommunityLive are
// TestRoutesCommunityFilterReadsTheLIVEValue's own -- the community counterpart
// of repathPrefix/repathOldOrigin/repathNewOrigin above, and deliberately not
// reusing that fixture's coordinates: mixing an AS-path mutation into a
// fixture built to isolate a community mutation would leave a future failure
// ambiguous about which channel broke.
//
// 10.205.0.0/24 sits past every prefix range this file already claims --
// 10.200-10.204 (see extCommFixturePrefix's and originFixtureMatch's own
// comments for the ranges already claimed). Its router, peer and session
// (10.0.0.174, 10.0.0.175, 730) continue insertOriginFixture's and insertRepathedFixture's
// own sequence (710, 720) past the highest router/peer IP claimed anywhere in
// this package, 10.0.0.173 (repathPrefix's own peer) -- checked directly
// against this file rather than assumed.
const (
	repathCommunityPrefix = "10.205.0.0/24"
	repathCommunityOld    = "65000:100" // seq 1, superseded
	repathCommunityLive   = "65000:200" // seq 2, live

	// The packed Array(UInt32) forms of the two strings above -- see
	// ParseCommunity's own doc comment for why communities is stored packed
	// rather than as text, and TestParseCommunityPacksAStandardCommunity for
	// the identical shift pinned as a unit test.
	repathCommunityOldPacked  = uint32(65000)<<16 | 100
	repathCommunityLivePacked = uint32(65000)<<16 | 200
)

// insertRepathedCommunityFixture writes ONE route key observed twice, the
// second observation carrying a DIFFERENT community set rather than an
// additional one -- a router re-advertising a route under a changed policy
// replaces its communities, it does not accumulate them, and a fixture that
// only ever added one would leave a stale-value match indistinguishable from
// a live one that happens to also match. repathCommunityLive is what makes
// the live row visible; repathCommunityOld appears only in the superseded
// row, which is what makes a WHERE-based filter visibly wrong -- see
// TestRoutesCommunityFilterReadsTheLIVEValue.
func insertRepathedCommunityFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const (
		sysname  = "repath-community-fixture"
		routerIP = "10.0.0.174"
		peerIP   = "10.0.0.175"
		sid      = uint64(730)
	)
	ts := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname,
		PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65001, PeerBGPID: routerIP,
		SessionID: sid, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: ts, TsCollector: ts,
	})

	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routerIP, RouterSysname: sysname,
		PeerIP: peerIP, RIB: "in_pre", Prefix: repathCommunityPrefix,
		NextHop: "10.0.0.99",
		PeerASN: 65001, PeerBGPID: routerIP,
		SessionID: sid, Seq: 1, StreamSeq: 1,
		ASPath:      []uint32{65001},
		Communities: []uint32{repathCommunityOldPacked},
		TsRouter:    ts,
		TsCollector: ts,
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: routerIP, RouterSysname: sysname,
		PeerIP: peerIP, RIB: "in_pre", Prefix: repathCommunityPrefix,
		NextHop: "10.0.0.99",
		PeerASN: 65001, PeerBGPID: routerIP,
		SessionID: sid, Seq: 2, StreamSeq: 2,
		ASPath:      []uint32{65001},
		Communities: []uint32{repathCommunityLivePacked},
		TsRouter:    ts,
		TsCollector: ts,
	})
}

// insertPathFixture writes one up peer and one unicast row per entry, with
// seq and stream_seq ascending in slice order, so the LAST entry for a given
// route key is the live one.
//
// ts is stamped on every row's TsRouter AND TsCollector, the peer event and
// each route row alike -- not just the peer event -- because route_unicast's
// TTL is keyed on ts_collector (see schema.sql): a route row left at the Go
// zero time is not merely wrong, it is a row ClickHouse's own TTL enforcement
// deletes, silently, on whatever schedule the merge runs. A fixture built
// that way passes the moment it is written and fails the first time this
// package's full suite runs after that merge fires.
func insertPathFixture(t *testing.T, ctx context.Context, q *Q,
	sysname, routerIP, peerIP string, sid uint64, rows []pathRow) {
	t.Helper()
	ts := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname,
		PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65001, PeerBGPID: routerIP,
		SessionID: sid, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: ts, TsCollector: ts,
	})

	for i, r := range rows {
		n := uint64(i + 1)
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: routerIP, RouterSysname: sysname,
			PeerIP: peerIP, RIB: "in_pre", Prefix: r.prefix,
			NextHop: "10.0.0.99",
			PeerASN: 65001, PeerBGPID: routerIP,
			SessionID: sid, Seq: n, StreamSeq: n,
			ASPath:      r.path,
			TsRouter:    ts,
			TsCollector: ts,
		})
	}
}

// The link-state fixture's coordinates. Two observers (two peers of one
// router) report the SAME node -- router_id "0a0000f1", with every other
// node_key input identical too, so the two rows hash to one node_key --
// which is what makes the one-row-per-observer rule assertable rather than
// assumed: a query that grouped by node_key alone returns one row here, and
// the correct one returns two.
const (
	lsFixCollector = "ls-test-collector"
	lsFixRouterIP  = "10.0.0.51"
	lsFixPeerA     = "10.0.0.52"
	lsFixPeerB     = "10.0.0.53"
	lsFixSession   = uint64(1788023088001199999)
	lsFixSysName   = "ls-fixture-router"
	// Area 0 on purpose. It is the archive's most common area (1,224 of
	// 1,441 rows) and it is the value a zero-means-unset filter would make
	// unaskable, so the fixture puts the hazard in the default path.
	lsFixArea     = uint32(0)
	lsFixASN      = uint32(65100)
	lsFixProtocol = uint8(2)
)

// insertLSNodeFixture writes one peer_up event and four ls_nodes rows:
//
//   - "shared": reported by peer A and peer B, same node_key, so the answer
//     must carry two rows.
//   - "gone": reported by peer A twice, the newer observation a withdrawal,
//     so state=live excludes it and state=withdrawn returns exactly it.
//   - "ospf": protocol 3 and area 1, so protocol= and area= each have a
//     value that selects a strict subset.
func insertLSNodeFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	for i, peer := range []string{lsFixPeerA, lsFixPeerB} {
		if err := peers.Append(
			lsFixCollector, netip.MustParseAddr(lsFixRouterIP), lsFixSysName,
			netip.MustParseAddr(peer), "in_pre", lsFixASN,
			netip.MustParseAddr(lsFixRouterIP), lsFixSession, uint64(i+1),
			ts, ts, []string{}, uint64(i+1),
			"up", netip.MustParseAddr(lsFixRouterIP), uint16(179), uint16(50001),
			uint32(0), uint8(1),
		); err != nil {
			t.Fatalf("append peer_events: %v", err)
		}
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	// seq is the ordering argMax reads, so "gone" gets a higher seq on its
	// withdrawal than on its announcement.
	//
	// That pair's ts_collector is INVERTED against its seq, deliberately: the
	// withdrawal (seq 13) carries the base timestamp and the announcement
	// (seq 12) carries the base plus one second, so the withdrawal is the
	// row this collector received FIRST even though the sender numbered it
	// last. That models a real hazard -- a message reordered in flight
	// relative to the sender's own sequence -- and it is what makes the
	// ordering this package chose testable instead of merely asserted.
	// argMax over (seq, stream_seq) picks seq 13 and reports the node
	// withdrawn, which is correct; argMax over (ts_collector, stream_seq) --
	// link-state-nodes.json's ordering, and not this package's (see
	// lsNodesSQL's own doc comment) -- compares ts_collector first, picks the
	// later-received ANNOUNCEMENT, and reports a withdrawn node as live, so
	// TestLSNodesExcludesWithdrawnByDefault fails outright under it.
	//
	// The inversion is a full second rather than an equal pair of timestamps
	// on purpose. Identical timestamps would leave the wrong ordering to
	// break its tie on stream_seq -- which agrees with seq here, so the
	// mutation would still pass -- and an ordering by ts_collector alone
	// would resolve a genuine tie arbitrarily, making the guard flaky rather
	// than failing. A strictly earlier withdrawal is deterministic in both.
	type row struct {
		peer        string
		seq         uint64
		protocol    uint8
		area        uint32
		routerID    string
		name        string
		isWithdraw  uint8
		tsCollector time.Time
	}
	for _, r := range []row{
		{lsFixPeerA, 10, lsFixProtocol, lsFixArea, "0a0000f1", "ls-shared", 0, ts},
		{lsFixPeerB, 11, lsFixProtocol, lsFixArea, "0a0000f1", "ls-shared", 0, ts},
		{lsFixPeerA, 12, lsFixProtocol, lsFixArea, "0a0000f2", "ls-gone", 0, ts.Add(time.Second)},
		{lsFixPeerA, 13, lsFixProtocol, lsFixArea, "0a0000f2", "ls-gone", 1, ts},
		{lsFixPeerA, 14, 3, 1, "0a0000f3", "ls-ospf", 0, ts},
	} {
		if err := nodes.Append(
			lsFixCollector, netip.MustParseAddr(lsFixRouterIP), lsFixSysName,
			netip.MustParseAddr(r.peer), "in_pre", lsFixASN,
			netip.MustParseAddr(lsFixRouterIP), lsFixSession, r.seq,
			ts, r.tsCollector, []string{}, r.seq,
			r.protocol, uint64(0), lsFixASN, uint32(0), r.area, r.routerID,
			"", // router_id_v4: empty, so the label falls back
			r.isWithdraw, r.name,
			uint32(16000), uint32(8000), uint32(15000), uint32(1000),
			[]uint8{0}, map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_nodes: %v", err)
		}
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}
}

// lsPageFixtureRouterIP, lsPageFixturePeerIP and lsPageFixtureSession are
// insertLSPageFixture's own island, kept apart from insertLSNodeFixture's
// lsFixRouterIP/lsFixPeerA/lsFixSession so a walk scoped to this fixture's
// (router, peer) never picks up insertLSNodeFixture's rows through
// peerUpCTE's cur.sid.
const (
	lsPageFixtureRouterIP = "10.0.0.210"
	lsPageFixturePeerIP   = "10.0.0.211"
	lsPageFixtureSession  = lsFixSession + 1000
)

// insertLSPageFixture writes one peer_up event and five ls_nodes rows, all
// under one (collector, router, peer, rib, session) scope -- the single
// scope TestLSNodesPageWalksEveryRowExactlyOnce walks with Limit: 2, which
// needs more rows than fit on one page to exercise more than one.
//
// router_id is NOT unique across the fixture: rows 1 and 2 both carry
// "0a0000f1" and differ only in protocol (2 vs 3), so node_key still tells
// them apart while router_id alone would not. That tie is deliberate, not
// incidental, and so is where it sits: sorted by router_id the five rows
// read f0, f1, f1, f2, f3, which puts the tied pair spanning the boundary
// between page one (rows 0-1) and page two (rows 2-4) of a Limit: 2 walk --
// not both on the same page, where the tie would never be asked to break.
// This is what a mutation dropping n.node_key from lsNodesKey needs in
// order to fail: with the tied pair confined to
// one page, or with every router_id distinct, a key of (rib, router_id)
// alone would still walk correctly and the mutation would pass by accident,
// proving nothing about node_key's role. The other three rows carry
// distinct router_ids, so the fixture has exactly five distinct node_keys,
// matching the five rows the unpaginated LSNodes call must return.
func insertLSPageFixture(t *testing.T, ctx context.Context, q *Q) (router, peer netip.Addr) {
	t.Helper()
	router = netip.MustParseAddr(lsPageFixtureRouterIP)
	peer = netip.MustParseAddr(lsPageFixturePeerIP)
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: lsPageFixtureRouterIP, RouterSysname: "ls-page-router",
		PeerIP: lsPageFixturePeerIP, RIB: "in_pre",
		Collector: lsFixCollector,
		PeerASN:   lsFixASN, PeerBGPID: lsPageFixturePeerIP,
		SessionID: lsPageFixtureSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	type row struct {
		protocol uint8
		routerID string
	}
	rows := []row{
		{lsFixProtocol, "0a0000f0"},
		{lsFixProtocol, "0a0000f1"},
		{3, "0a0000f1"}, // same router_id as the row above, different protocol: see doc comment above
		{lsFixProtocol, "0a0000f2"},
		{lsFixProtocol, "0a0000f3"},
	}

	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	for i, r := range rows {
		if err := nodes.Append(
			lsFixCollector, router, "ls-page-router",
			peer, "in_pre", lsFixASN,
			router, lsPageFixtureSession, uint64(i+1),
			now, now, []string{}, uint64(i+1),
			r.protocol, uint64(0), lsFixASN, uint32(0), lsFixArea, r.routerID,
			"", // router_id_v4: empty, so the label falls back
			uint8(0), fmt.Sprintf("ls-page-%d", i),
			uint32(16000), uint32(8000), uint32(15000), uint32(1000),
			[]uint8{0}, map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_nodes: %v", err)
		}
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}
	return router, peer
}

// lsPrefixPageFixtureRouterIP, lsPrefixPageFixturePeerIP and
// lsPrefixPageFixtureSession are insertLSPrefixPageFixture's own island, for
// insertLSPageFixture's own reason: a walk scoped to this fixture's (router,
// peer) must never pick up another fixture's rows through peerUpCTE's
// cur.sid.
const (
	lsPrefixPageFixtureRouterIP = "10.0.0.220"
	lsPrefixPageFixturePeerIP   = "10.0.0.221"
	lsPrefixPageFixtureSession  = lsFixSession + 1500
)

// insertLSPrefixPageFixture writes one peer_up event and five ls_prefixes
// rows under two nodes, all in one (collector, router, peer, rib, session)
// scope -- the single scope TestLSPrefixesPageWalksEveryRowExactlyOnce walks
// with Limit: 2, which needs more rows than fit on one page to exercise more
// than one.
//
// Node "0a0000c1" originates three of the five prefixes and node "0a0000c2"
// originates the other two. That skew is what makes the walk test's
// uniqueness check need to be keyed on (NodeKey, Prefix) rather than NodeKey
// alone: NodeKey alone repeats three times across "0a0000c1"'s own rows, and
// that repetition is ordinary -- one node originating several prefixes --
// not a defect the walk introduced.
//
// The two nodes' prefix sets also share ONE cidr -- 10.90.21.0/24, once
// under each node -- and that repetition is deliberate rather than
// incidental, the way insertLSPageFixture's tied router_id is. The same
// prefix genuinely can be originated by two different nodes (an anycast
// address is the ordinary case for it), lsPrefixesSQL's GROUP BY carries
// node_key alongside (prefix, prefix_len) so the two are two rows rather
// than one, and lsPrefixesKey's node_key column exists to keep the tie
// between their otherwise-identical cidr sorted rather than left to
// ClickHouse's discretion. Sorted by cidr the five rows read
// 20(c1), 21(?), 21(?), 22(c2), 23(c1) -- which puts the tied pair spanning
// the boundary between page one (rows 0-1) and page two (rows 2-3) of a
// Limit: 2 walk, not both on the same page, where the tie would never be
// asked to break. This is what a mutation dropping p.node_key from
// lsPrefixesKey needs in order to fail: with the tied pair confined to one
// page, or with no tie in the fixture at all, a key of (rib, cidr) alone
// would still walk this fixture correctly and the mutation would pass by
// accident, proving nothing about node_key's role.
func insertLSPrefixPageFixture(t *testing.T, ctx context.Context, q *Q) (router, peer netip.Addr) {
	t.Helper()
	router = netip.MustParseAddr(lsPrefixPageFixtureRouterIP)
	peer = netip.MustParseAddr(lsPrefixPageFixturePeerIP)
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: lsPrefixPageFixtureRouterIP, RouterSysname: "ls-prefix-page-router",
		PeerIP: lsPrefixPageFixturePeerIP, RIB: "in_pre",
		Collector: lsFixCollector,
		PeerASN:   lsFixASN, PeerBGPID: lsPrefixPageFixturePeerIP,
		SessionID: lsPrefixPageFixtureSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	type row struct {
		routerID string
		addr     string
		len_     uint8
	}
	rows := []row{
		{"0a0000c1", "10.90.20.0", 24},
		{"0a0000c1", "10.90.21.0", 24}, // tied cidr with the row below, different node -- see doc comment above
		{"0a0000c2", "10.90.21.0", 24},
		{"0a0000c2", "10.90.22.0", 24},
		{"0a0000c1", "10.90.23.0", 24},
	}

	pfx, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_prefixes")
	if err != nil {
		t.Fatalf("prepare ls_prefixes: %v", err)
	}
	for i, r := range rows {
		if err := pfx.Append(
			lsFixCollector, router, "ls-prefix-page-router",
			peer, "in_pre", lsFixASN,
			router, lsPrefixPageFixtureSession, uint64(i+1),
			now, now, []string{}, uint64(i+1),
			lsFixProtocol, uint64(0), lsFixASN, uint32(0), lsFixArea, r.routerID,
			r.addr, r.len_,
			uint8(0), uint32(16001), uint8(0), uint8(1), uint32(10), uint8(0), uint8(0),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_prefixes: %v", err)
		}
	}
	if err := pfx.Send(); err != nil {
		t.Fatalf("send ls_prefixes: %v", err)
	}
	return router, peer
}

// lsDumpStateFixtureRouterIP, lsDumpStateFixturePeerIP and
// lsDumpStateFixtureSession are their own island, deliberately not
// insertLSNodeFixture's shared lsFixRouterIP/lsFixPeerA/lsFixSession: this
// fixture is the only one in the package that writes an ls_events row at
// all, and it writes two, so it needs a (router, peer, session) nothing
// else touches to keep its ls_events rows from being joined against any
// other test's ls_nodes rows through peerUpCTE's cur.sid.
const (
	lsDumpStateFixtureRouterIP = "10.0.0.201"
	lsDumpStateFixturePeerIP   = "10.0.0.202"
	lsDumpStateFixtureSession  = lsFixSession + 500
	lsDumpStateFixtureNodeID   = "0a0000d1"
)

// insertLSDumpStateFixture writes one up peer, one live ls_nodes row, and
// TWO ls_events end-of-rib markers for that same (collector, router, peer,
// rib, session) -- the shape lsEorCTE's own doc comment names but that no
// fixture in this package had ever actually written: every other insertLS*
// fixture writes ZERO ls_events rows, so a mutation collapsing lsEorCTE's
// `marked` from the constant 1 to count() has nothing here to disagree
// with it on -- LEFT JOIN eor produces the same NULL either way when eor
// has no matching row at all. Two markers under one session, rather than
// one, is what makes toUInt8(1) and count() answer differently: the
// archive itself has a session with 13 (lsEorCTE's own comment), and this
// is the smallest fixture that reproduces the same disagreement -- 2 is
// enough, 13 is not the point.
//
// See TestLSNodesDumpStateIgnoresDuplicateEndOfRibMarkers, the only test
// that reads this fixture.
func insertLSDumpStateFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := peers.Append(
		lsFixCollector, netip.MustParseAddr(lsDumpStateFixtureRouterIP), lsFixSysName,
		netip.MustParseAddr(lsDumpStateFixturePeerIP), "in_pre", lsFixASN,
		netip.MustParseAddr(lsDumpStateFixtureRouterIP), uint64(lsDumpStateFixtureSession), uint64(1),
		ts, ts, []string{}, uint64(1),
		"up", netip.MustParseAddr(lsDumpStateFixtureRouterIP), uint16(179), uint16(50001),
		uint32(0), uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	if err := nodes.Append(
		lsFixCollector, netip.MustParseAddr(lsDumpStateFixtureRouterIP), lsFixSysName,
		netip.MustParseAddr(lsDumpStateFixturePeerIP), "in_pre", lsFixASN,
		netip.MustParseAddr(lsDumpStateFixtureRouterIP), uint64(lsDumpStateFixtureSession), uint64(1),
		ts, ts, []string{}, uint64(1),
		lsFixProtocol, uint64(0), lsFixASN, uint32(0), lsFixArea, lsDumpStateFixtureNodeID,
		"", // router_id_v4
		uint8(0), "ls-dumpstate-marked",
		uint32(16000), uint32(8000), uint32(15000), uint32(1000),
		[]uint8{0}, map[uint16]string{},
	); err != nil {
		t.Fatalf("append ls_nodes: %v", err)
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}

	events, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_events")
	if err != nil {
		t.Fatalf("prepare ls_events: %v", err)
	}
	// Two markers, same (collector, router, peer, rib, session), different
	// seq. lsEorCTE's GROUP BY collapses them to one row either way; only
	// what `marked` computes over that one row tells the two
	// implementations apart -- toUInt8(1) reports "the RIB finished
	// converging, asked once or twice" and reads dump_state "complete"
	// regardless, while count() reports 2 here, and dumpStateExpr's
	// `coalesce(eor.marked, 0) = 1` then reads false, so a caller would be
	// told the object is still "dumping" for a RIB that finished twice
	// over.
	for _, seq := range []uint64{2, 3} {
		if err := events.Append(
			lsFixCollector, netip.MustParseAddr(lsDumpStateFixtureRouterIP), lsFixSysName,
			netip.MustParseAddr(lsDumpStateFixturePeerIP), "in_pre", lsFixASN,
			netip.MustParseAddr(lsDumpStateFixtureRouterIP), uint64(lsDumpStateFixtureSession), seq,
			ts, ts, []string{}, seq,
			"ls", "", "", uint8(1),
		); err != nil {
			t.Fatalf("append ls_events: %v", err)
		}
	}
	if err := events.Send(); err != nil {
		t.Fatalf("send ls_events: %v", err)
	}
}

const (
	lsFixPrefixCIDR  = "10.90.7.0/24"
	lsFixCoveredAddr = "10.90.7.33"

	// lsFixPrefixOtherCIDR, lsFixPrefixOtherArea and lsFixPrefixOtherASN
	// are the third fixture prefix: a prefix originated by a node in a
	// DIFFERENT area AND a different AS from every other row here.
	//
	// Both at once is the point, and one alone would be worthless. While
	// every ls_prefixes row carried the same area and the same ASN, area=
	// and asn= selected the identical row set as each other and as no
	// filter at all -- so transposing the two predicates in
	// LSPrefixFilter.predicates, or the two fields in handleLSPrefixes'
	// filter literal, changed no answer any test in this repo could see.
	// That was measured, not feared: the swap left the whole test suite
	// green. The same transposition on nodes fails two tests and on links
	// one, because those fixtures already vary both columns.
	//
	// The values are chosen so the transposed statement matches NOTHING
	// rather than something plausible: no fixture prefix carries asn 0 or
	// asn 7, and none carries area 65100 or area 65107, so area= and asn=
	// swapped return an empty answer and the match assertions in
	// TestLSPrefixesFilterByAreaAndASN fail outright.
	lsFixPrefixOtherCIDR = "10.92.9.0/24"
	lsFixPrefixOtherArea = uint32(7)
	lsFixPrefixOtherASN  = uint32(65107)
)

// insertLSPrefixFixture writes three prefixes: two for the node
// insertLSNodeFixture already writes -- the one the tests name, and a
// second that must NOT match it, so every filter assertion has a row it has
// to exclude as well as one it has to find -- plus a third originated by a
// node in another area and another AS, which is what makes area= and asn=
// distinguishable from each other at all. See lsFixPrefixOtherCIDR's own
// comment for what that third row costs to leave out.
//
// The third row writes into ls_prefixes ONLY. It deliberately adds no
// ls_nodes row: lsPrefixesSQL joins nothing to ls_nodes, so a node row would
// buy this fixture nothing, and the whole package shares one ClickHouse
// database where TestLSNodesFiltersOnAreaZero and
// TestLSNodesExcludesWithdrawnByDefault assert exact ls_nodes counts.
func insertLSPrefixFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	insertLSNodeFixture(t, ctx, q)
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	pfx, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_prefixes")
	if err != nil {
		t.Fatalf("prepare ls_prefixes: %v", err)
	}
	for i, p := range []struct {
		addr     string
		len_     uint8
		asn      uint32
		area     uint32
		routerID string
	}{
		{"10.90.7.0", 24, lsFixASN, lsFixArea, "0a0000f1"},
		{"10.91.8.0", 24, lsFixASN, lsFixArea, "0a0000f1"},
		{"10.92.9.0", 24, lsFixPrefixOtherASN, lsFixPrefixOtherArea, "0a0000f4"},
	} {
		if err := pfx.Append(
			lsFixCollector, netip.MustParseAddr(lsFixRouterIP), lsFixSysName,
			netip.MustParseAddr(lsFixPeerA), "in_pre", lsFixASN,
			netip.MustParseAddr(lsFixRouterIP), lsFixSession, uint64(30+i),
			ts, ts, []string{}, uint64(30+i),
			lsFixProtocol, uint64(0), p.asn, uint32(0), p.area, p.routerID,
			p.addr, p.len_,
			uint8(0), uint32(16001), uint8(0), uint8(1), uint32(10), uint8(0), uint8(0),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_prefixes: %v", err)
		}
	}
	if err := pfx.Send(); err != nil {
		t.Fatalf("send ls_prefixes: %v", err)
	}
}

// The link fixture's endpoints, one per label tier. Each constant is a
// remote router_id a fixture link carries, and the tests index the answer by
// exactly that string -- so the name a link comes back with is checked
// against the tier that was arranged to be the ONLY one able to supply it.
//
// That "only one" is the whole point of the arrangement, and it is what a
// looser fixture would lose: if every endpoint were reachable from every
// tier, a chain that always fell through to the fleet tier -- or one that
// never left it -- would still label every link non-empty and pass a test
// that asked no more than that.
const (
	// lsFixLinkLocal is the local end of every fixture link. The reporting
	// peer named it itself, so both ends of a link have a label and
	// TestLSLinkLabelIsNeverEmpty is not satisfied by the identifier
	// fallback alone.
	lsFixLinkLocal = "0a0000e0"
	// lsFixLinkNear is named by the REPORTING peer and by the other peer,
	// under two different names: the observer tier must answer with the
	// reporting peer's, and a chain that consulted the fleet first would
	// answer with the other's, which is a different string as well as a
	// different tier.
	lsFixLinkNear = "0a0000e1"
	// lsFixLinkFar is named by the other peer ONLY. The reporting peer has
	// never reported this node, so nothing the observer tier can see names
	// it and the fleet tier is the only one left that can.
	lsFixLinkFar = "0a0000e2"
	// lsFixLinkAnon has an ls_nodes row from the reporting peer whose name
	// AND router_id_v4 are both empty. A row exists, so a chain that tested
	// for a matched join row rather than for a non-empty name would call
	// this the observer tier and hand back "".
	lsFixLinkAnon = "0a0000e3"
	// lsFixLinkCross is the far end of the area-crossing link, and it has no
	// ls_nodes row at all. It is deliberately in area 1 and deliberately
	// unnamed: an area-1 node row here would be a second area-1 node, and
	// TestLSNodesFiltersOnAreaZero asserts there is exactly one. Every test
	// in this package shares one database, so a fixture's extra rows are
	// every other fixture's input.
	lsFixLinkCross = "0a0000e4"
	// lsFixLinkStale is named by the reporting peer in a PREVIOUS session
	// and by nobody in the current one. The observer tier is scoped to the
	// current session, so it must not answer; the fleet tier is keyed by
	// node_key alone, across sessions, so it does. Dropping the label join's
	// session predicate flips this one row's tier from "fleet" to
	// "observer", which is the only fixture arrangement in which that
	// predicate's absence is visible at all -- see
	// TestLSLinkLabelsPreferTheReportingObserver's own comment.
	lsFixLinkStale = "0a0000e5"
	// lsFixLinkGone is the far end of the withdrawn link.
	lsFixLinkGone = "0a0000e6"
	// lsFixLinkStaleLocal is lsFixLinkStale's mirror on the LOCAL end: a
	// node the reporting peer named in a previous session only, sitting at
	// the near end of its own link. The two observer-scoped label joins are
	// separate clauses carrying the same session predicate, so one fixture
	// case guards one clause and a mutation dropping the other's would
	// otherwise pass unnoticed.
	lsFixLinkStaleLocal = "0a0000e7"
	// lsFixLinkDotted is named by NOTHING but its router_id_v4: its rows
	// carry an empty name and a dotted quad. It exists because every other
	// endpoint here leaves router_id_v4 empty, which left branches 2 and 4
	// of lsLabelExpr -- and both `r4 != ''` halves of lsLabelSourceExpr --
	// unreachable in every test. That is not a hypothetical gap: a real
	// captured archive carries 66 ls_nodes rows with an empty name and a
	// non-empty router_id_v4, so for 66 real nodes r4 IS the deciding
	// tier-1 signal.
	//
	// The other observer reports a DIFFERENT router_id_v4 for the same node,
	// later in seq, so the observer branch answering and the fleet branch
	// answering are two different strings here too -- without that, deleting
	// the observer r4 branch from lsLabelExpr alone would fall through to
	// the fleet's identical quad and report the fleet's answer under the
	// observer's tier name, which is the exact lockstep failure
	// lsLabelSourceExpr's doc comment calls worse than no report at all.
	lsFixLinkDotted = "0a0000e8"
	// lsFixLinkFleetDotted is lsFixLinkDotted one tier down: its ONLY name
	// anywhere is another observer's router_id_v4. The reporting peer has
	// never reported it, so the observer tier cannot answer; no observer
	// gives it a name, so the fleet tier's nm branch cannot either; the
	// fleet's r4 branch is the only thing left that can.
	//
	// It exists because lsFixLinkDotted closes the OBSERVER tier's r4 branch
	// only -- that endpoint's observer tier answers first, so branch 4 of
	// lsLabelExpr and the `%[2]s.r4 != ''` half of lsLabelSourceExpr stayed
	// unreachable, and deleting both left the package green. The same
	// lockstep failure, one tier down.
	lsFixLinkFleetDotted = "0a0000e9"
)

// The names the link fixture's ls_nodes rows carry, spelled once so a test
// asserting a label and the fixture writing it cannot drift apart.
const (
	lsFixLocalLabel      = "ls-link-local"
	lsFixNearLabel       = "ls-near"
	lsFixNearElse        = "ls-near-elsewhere"
	lsFixFarLabel        = "ls-far"
	lsFixStaleLabel      = "ls-stale"
	lsFixStaleLocalLabel = "ls-stale-local"
	// The superseded names: same node, same observer, LOWER seq. They exist
	// to give the label CTEs' argMax something to win against -- with one
	// candidate per key, argMax and any() cannot be told apart, and the
	// CTEs' own doc comment says at length why any() is not admissible.
	lsFixNearOld = "ls-near-superseded"
	lsFixFarOld  = "ls-far-superseded"
	// lsFixLinkDotted's two router_id_v4 values: the reporting observer's,
	// which must win, and the rest of the fleet's, which must not.
	lsFixDottedV4     = "10.0.0.88"
	lsFixDottedElseV4 = "10.0.0.99"
	// lsFixLinkFleetDotted's only name anywhere, held by the other observer.
	lsFixFleetDottedV4 = "10.0.0.77"
)

// insertLSLinkFixture writes the link-state links every LSLinks test reads,
// plus the ls_nodes rows that make each endpoint label resolvable through
// exactly one of the three label tiers (observer, fleet, identifier).
//
// It builds on insertLSNodeFixture rather than repeating it: that fixture
// already writes the peer_up events both peers need -- without which no link
// survives lsLinksSQL's peer_up gate -- and its own nodes are left alone
// here.
//
// Every link is reported by peer A, which makes peer A "the reporting
// observer" throughout and peer B the rest of the fleet. The endpoints are
// arranged so that:
//
//   - lsFixLinkNear is named by peer A ("ls-near") and, later in seq, by
//     peer B ("ls-near-elsewhere"). The fleet tier's argMax picks peer B's,
//     so the observer tier answering correctly and the fleet tier answering
//     instead are two different STRINGS, not just two different tier names.
//   - lsFixLinkFar is named by peer B alone, so only the fleet tier can
//     answer.
//   - both of those endpoints ALSO carry a superseded name from the same
//     observer at a lower seq, so each CTE's argMax has a loser to beat:
//     ls_node_labels resolves lsFixLinkNear against peer A's own older row,
//     and ls_fleet_labels resolves lsFixLinkFar against peer B's. Without
//     them every key has exactly one candidate and argMax is
//     indistinguishable from any(), which is the tie-break this package
//     refuses to leave arbitrary.
//   - lsFixLinkDotted is named by no name at all, only by a router_id_v4 --
//     the OBSERVER tier's second branch, which no other endpoint reaches.
//   - lsFixLinkFleetDotted is the same one tier down: its only name anywhere
//     is the OTHER observer's router_id_v4, so the fleet tier's own second
//     branch is the only thing that can answer it. Both dotted endpoints are
//     needed, because each tier's r4 branch is unreachable while a tier
//     above it can answer first.
//   - lsFixLinkAnon is named by nobody -- its one row has an empty name and
//     an empty router_id_v4 -- so only the raw identifier is left.
//   - lsFixLinkStale is named by peer A in the PREVIOUS session, so the
//     current-session observer tier must miss it and the fleet tier, which
//     spans sessions, must catch it. lsFixLinkStaleLocal is the same
//     arrangement at the near end of its own link, because the two
//     observer-scoped label joins are separate clauses and one case guards
//     one clause.
//
// It writes TWELVE ls_nodes rows -- ten under lsFixSession and two under the
// previous one -- and that number is quoted by
// TestLSNodesReturnsOneRowPerObserver and
// TestLSNodesExcludesWithdrawnByDefault, which both count what this fixture
// adds to the shared database. Count the literal below, not this sentence,
// if the two ever disagree.
//
// The node rows added here are all protocol 2, area 0 and live, and none is
// in area 1: the whole package shares one ClickHouse database, so these rows
// are also input to every LSNodes test, and those assert an exact count for
// area 1 and an exact set for state=withdrawn.
func insertLSLinkFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	insertLSNodeFixture(t, ctx, q)
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	for _, n := range []struct {
		peer     string
		session  uint64
		seq      uint64
		routerID string
		name     string
		r4       string
	}{
		{lsFixPeerA, lsFixSession, 40, lsFixLinkLocal, lsFixLocalLabel, ""},
		// Peer A's own superseded name for the near node, at a LOWER seq
		// than its live one below. ls_node_labels must argMax past it; an
		// any() there returns this row, which is the first the table's sort
		// order presents.
		{lsFixPeerA, lsFixSession, 38, lsFixLinkNear, lsFixNearOld, ""},
		{lsFixPeerA, lsFixSession, 41, lsFixLinkNear, lsFixNearLabel, ""},
		// Higher seq than peer A's row above, so the fleet tier's argMax
		// over (seq, stream_seq) resolves this node to peer B's name and the
		// two tiers disagree on the string as well as on the tier.
		{lsFixPeerB, lsFixSession, 42, lsFixLinkNear, lsFixNearElse, ""},
		// The same arrangement for the far node, whose label is the one that
		// actually surfaces through the fleet tier: a superseded peer-B row
		// at a lower seq, so ls_fleet_labels' argMax has a loser to beat.
		{lsFixPeerB, lsFixSession, 39, lsFixLinkFar, lsFixFarOld, ""},
		{lsFixPeerB, lsFixSession, 43, lsFixLinkFar, lsFixFarLabel, ""},
		{lsFixPeerA, lsFixSession, 44, lsFixLinkAnon, "", ""},
		// Named only by a router_id_v4, and by a different one at each
		// observer. The reporting observer's must win.
		{lsFixPeerA, lsFixSession, 47, lsFixLinkDotted, "", lsFixDottedV4},
		{lsFixPeerB, lsFixSession, 48, lsFixLinkDotted, "", lsFixDottedElseV4},
		// Peer B only, no name, a quad: nothing above the fleet tier's r4
		// branch can answer for this node.
		{lsFixPeerB, lsFixSession, 49, lsFixLinkFleetDotted, "", lsFixFleetDottedV4},
		// A previous session of the same (collector, router, peer, rib).
		// peer_events carries only lsFixSession, so cur still resolves to
		// lsFixSession and this row is invisible to LSNodes -- it exists
		// solely to be the wrong answer the label join's session predicate
		// refuses.
		{lsFixPeerA, lsFixSession - 1, 45, lsFixLinkStale, lsFixStaleLabel, ""},
		{lsFixPeerA, lsFixSession - 1, 46, lsFixLinkStaleLocal, lsFixStaleLocalLabel, ""},
	} {
		if err := nodes.Append(
			lsFixCollector, netip.MustParseAddr(lsFixRouterIP), lsFixSysName,
			netip.MustParseAddr(n.peer), "in_pre", lsFixASN,
			netip.MustParseAddr(lsFixRouterIP), n.session, n.seq,
			ts, ts, []string{}, n.seq,
			lsFixProtocol, uint64(0), lsFixASN, uint32(0), lsFixArea, n.routerID,
			n.r4, // empty on every endpoint but lsFixLinkDotted, which is the
			// one that reaches each tier's second branch.
			uint8(0), n.name,
			uint32(16000), uint32(8000), uint32(15000), uint32(1000),
			[]uint8{0}, map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_nodes: %v", err)
		}
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}

	links, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_links")
	if err != nil {
		t.Fatalf("prepare ls_links: %v", err)
	}
	// The withdrawn pair inverts ts_collector against seq for the reason
	// insertLSNodeFixture's own comment gives at length: the withdrawal
	// (seq 56) is received FIRST, so an argMax ordered by ts_collector picks
	// the announcement and reports a withdrawn link as live.
	for _, l := range []struct {
		seq         uint64
		localID     string
		remoteID    string
		remoteArea  uint32
		localIfAddr string
		remoteIf    string
		localLinkID uint32
		remoteLinkI uint32
		isWithdraw  uint8
		tsCollector time.Time
	}{
		{50, lsFixLinkLocal, lsFixLinkNear, lsFixArea, "10.1.1.1", "10.1.1.2", 1, 2, 0, ts},
		{51, lsFixLinkLocal, lsFixLinkFar, lsFixArea, "10.1.2.1", "10.1.2.2", 3, 4, 0, ts},
		{52, lsFixLinkLocal, lsFixLinkAnon, lsFixArea, "10.1.3.1", "10.1.3.2", 5, 6, 0, ts},
		{53, lsFixLinkLocal, lsFixLinkCross, 1, "10.1.4.1", "10.1.4.2", 7, 8, 0, ts},
		{54, lsFixLinkLocal, lsFixLinkStale, lsFixArea, "10.1.5.1", "10.1.5.2", 9, 10, 0, ts},
		{55, lsFixLinkLocal, lsFixLinkGone, lsFixArea, "10.1.6.1", "10.1.6.2", 11, 12, 0, ts.Add(time.Second)},
		{56, lsFixLinkLocal, lsFixLinkGone, lsFixArea, "10.1.6.1", "10.1.6.2", 11, 12, 1, ts},
		// The local end named only in a previous session, and the only link
		// whose two ends resolve through different tiers in that direction:
		// its local end from the fleet, its remote end -- the node every
		// other link starts from -- from the reporting observer.
		{57, lsFixLinkStaleLocal, lsFixLinkLocal, lsFixArea, "10.1.7.1", "10.1.7.2", 13, 14, 0, ts},
		// The endpoint whose only name is a dotted quad the reporting
		// observer holds, and the one whose only name is a quad only the
		// other observer holds.
		{58, lsFixLinkLocal, lsFixLinkDotted, lsFixArea, "10.1.8.1", "10.1.8.2", 15, 16, 0, ts},
		{59, lsFixLinkLocal, lsFixLinkFleetDotted, lsFixArea, "10.1.9.1", "10.1.9.2", 17, 18, 0, ts},
	} {
		if err := links.Append(
			lsFixCollector, netip.MustParseAddr(lsFixRouterIP), lsFixSysName,
			netip.MustParseAddr(lsFixPeerA), "in_pre", lsFixASN,
			netip.MustParseAddr(lsFixRouterIP), lsFixSession, l.seq,
			ts, l.tsCollector, []string{}, l.seq,
			lsFixProtocol, uint64(0),
			lsFixASN, uint32(0), lsFixArea, l.localID,
			lsFixASN, uint32(0), l.remoteArea, l.remoteID,
			l.localIfAddr, l.remoteIf, l.localLinkID, l.remoteLinkI,
			// local_node_key and remote_node_key are MATERIALIZED and must
			// not be appended: PrepareBatch is positional, and one extra
			// value here shifts every column after it by one.
			l.isWithdraw,
			[]uint32{24001}, []uint8{0x30}, []uint8{0},
			uint32(10), uint32(20), uint32(0), float32(1e9),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_links: %v", err)
		}
	}
	if err := links.Send(); err != nil {
		t.Fatalf("send ls_links: %v", err)
	}
}

// The fleet-label retention fixture's own coordinates, kept apart from
// insertLSLinkFixture's for insertLSPageFixture's own reason: a walk scoped
// to either fixture's (router, peer) must never pick up the other's rows
// through peerUpCTE's cur.sid. They are ALSO kept apart from each other:
// lsFleetRetentionRouterIP/PeerIP is the observer that reports the live
// link and names nothing, lsFleetRetentionNamerRouterIP/PeerIP is the one
// that names node N and then ages out of history. Nothing ties the two
// together except node N's own identity -- which is the property under
// test, since the fleet tier is keyed by node_key alone.
const (
	lsFleetRetentionCollector     = "ls-fleet-retention-collector"
	lsFleetRetentionRouterIP      = "10.0.0.180"
	lsFleetRetentionPeerIP        = "10.0.0.181"
	lsFleetRetentionSession       = lsFixSession + 3000
	lsFleetRetentionSysName       = "ls-fleet-retention-router"
	lsFleetRetentionNamerRouterIP = "10.0.0.182"
	lsFleetRetentionNamerPeerIP   = "10.0.0.183"
	lsFleetRetentionNamerSession  = lsFixSession + 3001
	lsFleetRetentionNamerSysName  = "ls-fleet-retention-namer"
	// Node N's identity, shared by the namer's ls_nodes row and the link's
	// remote end so the two resolve to the same node_key.
	lsFleetRetentionNodeRouterID = "0a0000fd"
	lsFleetRetentionName         = "ls-fleet-retained-name"
)

// lsFleetRetentionExpiredAt stamps the namer's one ls_nodes row, for
// seedTopoRetentionRouter's own reason (topology_test.go): any instant more
// than 90 days old is past ls_nodes' TTL, and a fixed month keeps the
// fixture isolated from time.Now(). ls_nodes is partitioned by
// toYYYYMM(ts_collector), and optimizeLSFleetRetentionPartition forces the
// TTL with OPTIMIZE on that one partition, so the month must hold no other
// fixture's ls_nodes rows -- confirmed by grep, nothing else in query/
// writes an ls_nodes row dated 2020.
var lsFleetRetentionExpiredAt = time.Date(2020, 1, 15, 12, 0, 0, 0, time.UTC)

const lsFleetRetentionPartition = "202001"

// insertLSFleetLabelRetentionFixture writes the scenario ls_fleet_labels'
// move to ls_nodes_current exists for: one observer names node N, and a
// second, unrelated observer reports a live link to N and never names it
// itself -- the same shape as TestLSLinkLabelsPreferTheReportingObserver's
// ANON case, except here the fixture's only candidate name for N is the one
// row a caller is expected to age out of retention afterward, with
// optimizeLSFleetRetentionPartition.
//
// It writes three things:
//   - one peer_up event for (lsFleetRetentionRouterIP,
//     lsFleetRetentionPeerIP), the link's own current session -- without it
//     lsLinksSQL's peer_up gate drops the link entirely;
//   - one ls_links row from that observer, live, whose remote end is node N
//     and carries an empty name and an empty router_id_v4 of its own, so the
//     observer tier has nothing to answer with;
//   - one ls_nodes row for node N, named, written under a wholly different
//     (router, peer, session) with no peer_up event of its own -- the fleet
//     tier (ls_fleet_labels) does not join peer_up at all, so this row needs
//     none -- and stamped lsFleetRetentionExpiredAt.
func insertLSFleetLabelRetentionFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := peers.Append(
		lsFleetRetentionCollector, netip.MustParseAddr(lsFleetRetentionRouterIP), lsFleetRetentionSysName,
		netip.MustParseAddr(lsFleetRetentionPeerIP), "in_pre", lsFixASN,
		netip.MustParseAddr(lsFleetRetentionRouterIP), lsFleetRetentionSession, uint64(1),
		ts, ts, []string{}, uint64(1),
		"up", netip.MustParseAddr(lsFleetRetentionRouterIP), uint16(179), uint16(50001),
		uint32(0), uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	links, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_links")
	if err != nil {
		t.Fatalf("prepare ls_links: %v", err)
	}
	if err := links.Append(
		lsFleetRetentionCollector, netip.MustParseAddr(lsFleetRetentionRouterIP), lsFleetRetentionSysName,
		netip.MustParseAddr(lsFleetRetentionPeerIP), "in_pre", lsFixASN,
		netip.MustParseAddr(lsFleetRetentionRouterIP), lsFleetRetentionSession, uint64(60),
		ts, ts, []string{}, uint64(60),
		lsFixProtocol, uint64(0),
		lsFixASN, uint32(0), lsFixArea, "0a0000fc",
		lsFixASN, uint32(0), lsFixArea, lsFleetRetentionNodeRouterID,
		"10.1.20.1", "10.1.20.2", uint32(30), uint32(31),
		// local_node_key and remote_node_key are MATERIALIZED and must not
		// be appended: PrepareBatch is positional, and one extra value here
		// shifts every column after it by one.
		uint8(0),
		[]uint32{24001}, []uint8{0x30}, []uint8{0},
		uint32(10), uint32(20), uint32(0), float32(1e9),
		map[uint16]string{},
	); err != nil {
		t.Fatalf("append ls_links: %v", err)
	}
	if err := links.Send(); err != nil {
		t.Fatalf("send ls_links: %v", err)
	}

	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	if err := nodes.Append(
		lsFleetRetentionCollector, netip.MustParseAddr(lsFleetRetentionNamerRouterIP), lsFleetRetentionNamerSysName,
		netip.MustParseAddr(lsFleetRetentionNamerPeerIP), "in_pre", lsFixASN,
		netip.MustParseAddr(lsFleetRetentionNamerRouterIP), lsFleetRetentionNamerSession, uint64(1),
		lsFleetRetentionExpiredAt, lsFleetRetentionExpiredAt, []string{}, uint64(1),
		lsFixProtocol, uint64(0), lsFixASN, uint32(0), lsFixArea, lsFleetRetentionNodeRouterID,
		"", // router_id_v4: empty, so only the name can resolve this test's label
		uint8(0), lsFleetRetentionName,
		uint32(16000), uint32(8000), uint32(15000), uint32(1000),
		[]uint8{0}, map[uint16]string{},
	); err != nil {
		t.Fatalf("append ls_nodes: %v", err)
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}
}

// optimizeLSFleetRetentionPartition forces ls_nodes' TTL on the namer's
// expired partition, the way real retention eventually does, for
// seedTopoRetentionRouter's own reason. It checks both halves before any
// test relies on them: history holds no row for the namer router, and the
// current table still holds its one named row -- the whole premise
// TestLSLinkFleetLabelSurvivesNodeHistoryRetention stands on.
func optimizeLSFleetRetentionPartition(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	if err := q.conn.Exec(ctx, "OPTIMIZE TABLE "+q.db+".ls_nodes PARTITION ID '"+
		lsFleetRetentionPartition+"' FINAL"); err != nil {
		t.Fatalf("optimize the expired ls_nodes partition: %v", err)
	}

	var history, current uint64
	if err := q.conn.QueryRow(ctx, "SELECT count() FROM "+q.db+
		".ls_nodes WHERE router_ip = toIPv6(?)",
		lsFleetRetentionNamerRouterIP).Scan(&history); err != nil {
		t.Fatalf("count the namer's history rows: %v", err)
	}
	if err := q.conn.QueryRow(ctx, "SELECT count() FROM "+q.db+
		".ls_nodes_current WHERE router_ip = toIPv6(?) AND name = ?",
		lsFleetRetentionNamerRouterIP, lsFleetRetentionName).Scan(&current); err != nil {
		t.Fatalf("count the namer's current rows: %v", err)
	}
	if history != 0 || current != 1 {
		t.Fatalf("after OPTIMIZE ... FINAL the namer has %d history rows and "+
			"%d current named rows, want 0 and 1: the fixture does not describe "+
			"expired history, so no test built on it can say anything about "+
			"retention", history, current)
	}
}

// lsLinkPageFixtureRouterIP, lsLinkPageFixturePeerIP and
// lsLinkPageFixtureSession are insertLSLinkPageFixture's own island, for
// insertLSPageFixture's own reason: a walk scoped to this fixture's (router,
// peer) must never pick up another fixture's rows through peerUpCTE's
// cur.sid.
const (
	lsLinkPageFixtureRouterIP = "10.0.0.230"
	lsLinkPageFixturePeerIP   = "10.0.0.231"
	lsLinkPageFixtureSession  = lsFixSession + 2000
)

// insertLSLinkPageFixture writes one peer_up event and five ls_links rows,
// all under one (collector, router, peer, rib, session) scope -- the single
// scope TestLSLinksPageWalksParallelLinksExactlyOnce walks with Limit: 1,
// which needs more rows than fit on one page to exercise more than one and
// to prove the walk's union still equals the unpaginated answer, not merely
// that the parallel pair itself survives.
//
// Rows 0 and 1 are the measured case lsLinksKey exists to keep apart: the
// SAME (local_node_key, remote_node_key, link_local_id, link_remote_id) --
// local_router_id "0a0000b0", remote_router_id "0a0000c0", link IDs (1, 2)
// -- reported with two DIFFERENT (local_ifaddr, remote_ifaddr) pairs. That
// is not a hypothetical: up to 2 distinct ifaddr pairs sharing one link
// tuple is what a real captured archive carries (see lsLinksKey's own doc
// comment). Every other field the two rows share is what makes them a tie
// on any key shorter than lsLinksKey -- same protocol, identifier, local
// and remote ASN/BGP-LS ID/area, both router_ids, both link IDs -- so a
// mutated key that drops the ifaddr columns has nothing left to tell them
// apart, and (rib, ...) > (last) then excludes BOTH rows past whichever one
// page one happened to return, dropping the other outright. See this
// fixture's own file header doc for how this is exercised: Limit: 1 makes
// every row its own page boundary, so the boundary between rows 0 and 1 is
// guaranteed regardless of where the pair sorts among the other three,
// unlike insertLSPageFixture and insertLSPrefixPageFixture, which had to
// place their own tied pair at a specific index for a Limit: 2 boundary to
// land inside it.
//
// The other three rows carry distinct (local_router_id, remote_router_id,
// link_local_id, link_remote_id) tuples, so the fixture has exactly five
// distinct link identities, matching the five rows the unpaginated LSLinks
// call must return.
func insertLSLinkPageFixture(t *testing.T, ctx context.Context, q *Q) (router, peer netip.Addr) {
	t.Helper()
	router = netip.MustParseAddr(lsLinkPageFixtureRouterIP)
	peer = netip.MustParseAddr(lsLinkPageFixturePeerIP)
	now := time.Now().UTC()

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: lsLinkPageFixtureRouterIP, RouterSysname: "ls-link-page-router",
		PeerIP: lsLinkPageFixturePeerIP, RIB: "in_pre",
		Collector: lsFixCollector,
		PeerASN:   lsFixASN, PeerBGPID: lsLinkPageFixturePeerIP,
		SessionID: lsLinkPageFixtureSession, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: now, TsCollector: now,
	})

	type row struct {
		localID, remoteID       string
		localIfAddr, remoteIf   string
		localLinkID, remoteLink uint32
	}
	rows := []row{
		{"0a0000b0", "0a0000c0", "10.2.1.1", "10.2.1.2", 1, 2},
		// Same (local_node_key, remote_node_key, link_local_id,
		// link_remote_id) as the row above -- same router_ids, same link
		// IDs -- different ifaddrs. See the doc comment above.
		{"0a0000b0", "0a0000c0", "10.2.1.5", "10.2.1.6", 1, 2},
		{"0a0000b1", "0a0000c1", "10.2.2.1", "10.2.2.2", 3, 4},
		{"0a0000b2", "0a0000c2", "10.2.3.1", "10.2.3.2", 5, 6},
		{"0a0000b3", "0a0000c3", "10.2.4.1", "10.2.4.2", 7, 8},
	}

	links, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_links")
	if err != nil {
		t.Fatalf("prepare ls_links: %v", err)
	}
	for i, r := range rows {
		if err := links.Append(
			lsFixCollector, router, "ls-link-page-router",
			peer, "in_pre", lsFixASN,
			router, lsLinkPageFixtureSession, uint64(i+1),
			now, now, []string{}, uint64(i+1),
			lsFixProtocol, uint64(0),
			lsFixASN, uint32(0), lsFixArea, r.localID,
			lsFixASN, uint32(0), lsFixArea, r.remoteID,
			r.localIfAddr, r.remoteIf, r.localLinkID, r.remoteLink,
			// local_node_key and remote_node_key are MATERIALIZED and must
			// not be appended: PrepareBatch is positional, and one extra
			// value here shifts every column after it by one.
			uint8(0),
			[]uint32{24001}, []uint8{0x30}, []uint8{0},
			uint32(10), uint32(20), uint32(0), float32(1e9),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_links: %v", err)
		}
	}
	if err := links.Send(); err != nil {
		t.Fatalf("send ls_links: %v", err)
	}
	return router, peer
}

// The view-lost fixture: one router, one collector, one session, three
// peers, each ending that session a different way, and one route apiece.
//
// It exists for the defect Session.Close was written to end. A collector
// that dies leaves its session as max(session_id) for its
// (collector_id, router_ip) forever, and without a 'view_lost' kind every
// peer of that session would stay recorded 'up' -- so the route rows below
// would be served as current state indefinitely, with nothing anywhere
// reporting an error. viewLostPeer is that peer. The other two are
// the controls that keep the assertions from passing for the wrong reason:
// upPeer must still be returned (a view-lost peer must not take its
// neighbors with it) and downPeer pins that 'view_lost' is a THIRD state,
// not a rename of 'down'.
const (
	// 10.0.0.150, not .140: insertTwoRibPeerFixture already owns 10.0.0.140
	// and peers .141/.142. The package shares one database for the life of
	// the test binary, so two fixtures on one address silently become one
	// router with whichever sysname any() lands on, and
	// TestRoutersCountsPeersNotRibViews then cannot find "two-rib-router" at
	// all.
	viewLostRouterIP  = "10.0.0.150"
	viewLostSysname   = "view-lost-router"
	viewLostSessionID = 2400

	viewLostPeer = "10.0.0.151"
	viewLostUp   = "10.0.0.152"
	viewLostDown = "10.0.0.153"

	viewLostPrefix = "10.150.1.0/24"
	viewLostUpPfx  = "10.150.2.0/24"
	viewLostDnPfx  = "10.150.3.0/24"
)

func insertViewLostFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()

	// Every peer comes up first and then ends the session its own way, so
	// the peer_events rows differ ONLY in the newest event's kind. A fixture
	// whose view-lost peer had no Peer-Up at all would pass these
	// assertions whether or not the kind was read.
	for _, e := range []struct {
		peerIP         string
		kind           string
		seq, streamSeq uint64
	}{
		{viewLostPeer, "up", 1, 1},
		{viewLostUp, "up", 1, 2},
		{viewLostDown, "up", 1, 3},
		{viewLostPeer, "view_lost", 2, 4},
		{viewLostDown, "down", 2, 5},
	} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: viewLostRouterIP, RouterSysname: viewLostSysname,
			PeerIP: e.peerIP, RIB: "in_pre",
			PeerASN: 65150, PeerBGPID: e.peerIP,
			SessionID: viewLostSessionID, Seq: e.seq, StreamSeq: e.streamSeq,
			Kind: e.kind, TsRouter: now, TsCollector: now,
		})
	}

	for _, r := range []struct {
		peerIP, prefix string
		streamSeq      uint64
	}{
		{viewLostPeer, viewLostPrefix, 10},
		{viewLostUp, viewLostUpPfx, 11},
		{viewLostDown, viewLostDnPfx, 12},
	} {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: viewLostRouterIP, RouterSysname: viewLostSysname,
			PeerIP: r.peerIP, RIB: "in_pre",
			PeerASN: 65150, PeerBGPID: r.peerIP,
			SessionID: viewLostSessionID, Seq: 1, StreamSeq: r.streamSeq,
			Prefix: r.prefix, NextHop: r.peerIP,
			TsRouter: now, TsCollector: now,
		})
	}
}

// ribParityFixture is the walk the two-phase reader is held to. Every other
// RIB fixture in this file is adversarial about which ROUTES reach the answer
// -- a superseded session, a down peer, a withdrawal -- and none of them is
// adversarial about the ATTRIBUTE VALUES those routes carry, because every
// route they hold is observed exactly once. That is fine for the walk's own
// tests and useless for a parity test: with one observation per key, argMax
// and argMin agree, session scoping changes nothing, and a second reader that
// resolved current state completely wrong would still match the first on every
// row. That exact vacuity is what this fixture exists to close, which is why
// it exists separately rather than as three more rows on
// insertRIBWalkFixture.
//
// So every route here is observed at least twice, with values that differ
// between observations, and the newest observation is never the one a naive
// reader would pick:
//
//   - ribParityPrefixOne is re-advertised with a different next hop and MED.
//   - ribParityPrefixTwo has its MED CLEARED on re-advertisement, the
//     Nullable-skipping hazard routesSQL's argMax(tuple(...)) exists for.
//   - ribParityPrefixStraggler carries an observation from the SUPERSEDED
//     session at a HIGHER stream_seq than its current-session row. That is
//     the shape that made an earlier two-phase reader implementation lose
//     200,460 of 1,000,000 routes: a BMP transport that drops without
//     a Peer Down leaves the old session publishing, and JetStream stamps
//     those late messages with the larger sequence. A reader that resolves
//     current state by stream_seq alone, without the session scope, reports
//     this route's dead observation; one that drops the session scope
//     entirely loses the route. Both readers must report the live one.
//   - ribParityAddPathPrefix is advertised under two path-ids, each
//     independently re-advertised, so a reader that groups on prefix alone
//     keeps one and silently drops the other.
const (
	ribParityRouterIP = "10.0.2.10"
	ribParitySysname  = "rib-parity-router"
	ribParityPeerIP   = "10.0.2.11"
	ribParityDownPeer = "10.0.2.12"
	ribParityCurSess  = uint64(4000)
	ribParityOldSess  = uint64(3000)
	ribParityRIB      = "in_pre"
	ribParityOtherRIB = "in_post"

	ribParityAddPathPrefix    = "10.50.1.0/24"
	ribParityPrefixTwo        = "10.50.2.0/24"
	ribParityPrefixThree      = "10.50.3.0/24"
	ribParityPrefixStraggler  = "10.50.4.0/24"
	ribParityWithdrawnPrefix  = "10.50.9.0/24"
	ribParityOldSessionPrefix = "10.50.8.0/24"
	ribParityDownPeerPrefix   = "10.50.7.0/24"
	ribParityOtherRIBPrefix   = "10.50.6.0/24"
	ribParityStrangerPrefix   = "10.50.5.0/24"
)

// ribParityStrangerPeer advertises a route and has NO peer_events row of its
// own -- not an "up", not a "down", nothing. It is the other half of the
// peer_up gate: the gate is an INNER JOIN as well as a state test, and a peer
// whose session was never recorded finds no row to join against. Without this
// peer, a gate rewritten as a LEFT JOIN keeping unmatched rows would pass
// every assertion in this package, because ribParityDownPeer does have a row
// and the state test alone would still exclude it.
//
// It is a peer of the same router, so cur (which is router-grained -- see
// peerStateCTE) resolves for it exactly as it does for every other peer here.
// What it lacks is peer_up.
const ribParityStrangerPeer = "10.0.2.13"

// ribParityUnicastKeys is what a walk of ribParityRouterIP/ribParityPeerIP
// must return, in keyset order, across every rib.
//
// in_pre comes FIRST and in_post last, which is not the alphabetical order.
// rib is Enum8('in_pre' = 0, 'in_post' = 1, ...) and ORDER BY on an Enum8
// sorts by the underlying number, so the walk's first page is the in_pre
// rows. TestRIBPageUnicastTwoPhaseMatchesOneStatement compares against the
// contents, not only len(), so a wrong order here fails it.
var ribParityUnicastKeys = [][]any{
	{ribParityRIB, ribParityAddPathPrefix, uint32(0)},
	{ribParityRIB, ribParityAddPathPrefix, uint32(5)},
	{ribParityRIB, ribParityPrefixTwo, uint32(0)},
	{ribParityRIB, ribParityPrefixThree, uint32(0)},
	{ribParityRIB, ribParityPrefixStraggler, uint32(0)},
	{ribParityOtherRIB, ribParityOtherRIBPrefix, uint32(0)},
}

func insertRIBParityFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	now := time.Now().UTC()
	u32 := func(v uint32) *uint32 { return &v }

	peerEvent := func(peer string, session, seq, stream uint64, kind string) {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: ribParityRouterIP, RouterSysname: ribParitySysname,
			PeerIP: peer, RIB: ribParityRIB,
			PeerASN: 65000, PeerBGPID: peer,
			SessionID: session, Seq: seq, StreamSeq: stream,
			Kind: kind, TsRouter: now, TsCollector: now,
		})
	}
	// The old session first, so it is the SECOND event that makes
	// ribParityCurSess the router's current session at all -- cur is
	// max(session_id) per (collector, router), not per peer.
	peerEvent(ribParityPeerIP, ribParityOldSess, 1, 1, "up")
	peerEvent(ribParityPeerIP, ribParityCurSess, 1, 2, "up")
	peerEvent(ribParityDownPeer, ribParityCurSess, 1, 3, "up")
	peerEvent(ribParityDownPeer, ribParityCurSess, 2, 4, "down")

	// The dump is complete for ipv4u, so DumpState is a value both readers
	// have to derive rather than a constant "unknown" they both fall back to.
	insertEorEvent(t, ctx, q, eorFixture{
		RouterIP: ribParityRouterIP, RouterSysname: ribParitySysname,
		PeerIP: ribParityPeerIP, RIB: ribParityRIB, Family: "ipv4u",
		PeerASN: 65000, PeerBGPID: ribParityPeerIP,
		SessionID: ribParityCurSess, Seq: 1, StreamSeq: 5,
		TsRouter: now, TsCollector: now,
	})

	route := func(peer, rib, prefix string, pathID uint32, session, seq, stream uint64,
		withdraw uint8, nextHop string, med *uint32) {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: ribParityRouterIP, RouterSysname: ribParitySysname,
			PeerIP: peer, RIB: rib, Prefix: prefix, PathID: pathID,
			PeerASN: 65000, PeerBGPID: peer,
			SessionID: session, Seq: seq, StreamSeq: stream,
			IsWithdraw: withdraw, NextHop: nextHop, ASPath: []uint32{65010, 65099},
			MED: med, LocalPref: u32(100), Communities: []uint32{4259840001},
			TsRouter: now, TsCollector: now,
		})
	}

	// Re-advertised: the seq-2 observation is the answer for both.
	route(ribParityPeerIP, ribParityRIB, ribParityAddPathPrefix, 0, ribParityCurSess, 1, 100, 0, "10.9.9.1", u32(10))
	route(ribParityPeerIP, ribParityRIB, ribParityAddPathPrefix, 0, ribParityCurSess, 2, 101, 0, "10.9.9.2", u32(20))
	route(ribParityPeerIP, ribParityRIB, ribParityAddPathPrefix, 5, ribParityCurSess, 1, 102, 0, "10.9.9.3", u32(30))
	route(ribParityPeerIP, ribParityRIB, ribParityAddPathPrefix, 5, ribParityCurSess, 2, 103, 0, "10.9.9.4", u32(40))

	// MED cleared on re-advertisement: the answer is absent, not 50.
	route(ribParityPeerIP, ribParityRIB, ribParityPrefixTwo, 0, ribParityCurSess, 1, 104, 0, "10.9.9.5", u32(50))
	route(ribParityPeerIP, ribParityRIB, ribParityPrefixTwo, 0, ribParityCurSess, 2, 105, 0, "10.9.9.6", nil)

	route(ribParityPeerIP, ribParityRIB, ribParityPrefixThree, 0, ribParityCurSess, 1, 106, 0, "10.9.9.7", u32(60))
	route(ribParityPeerIP, ribParityRIB, ribParityPrefixThree, 0, ribParityCurSess, 2, 107, 0, "10.9.9.8", u32(70))

	// The straggler. The current session's row is stream_seq 108; the
	// superseded session's row for the SAME route key is 999.
	route(ribParityPeerIP, ribParityRIB, ribParityPrefixStraggler, 0, ribParityCurSess, 1, 108, 0, "10.9.9.9", u32(80))
	route(ribParityPeerIP, ribParityRIB, ribParityPrefixStraggler, 0, ribParityOldSess, 9, 999, 0, "10.6.6.6", u32(255))

	// A second rib, so rib is load-bearing in the page key.
	route(ribParityPeerIP, ribParityOtherRIB, ribParityOtherRIBPrefix, 0, ribParityCurSess, 1, 110, 0, "10.9.9.10", u32(90))
	route(ribParityPeerIP, ribParityOtherRIB, ribParityOtherRIBPrefix, 0, ribParityCurSess, 2, 111, 0, "10.9.9.11", u32(95))

	// Everything that must not reach the answer.
	route(ribParityPeerIP, ribParityRIB, ribParityWithdrawnPrefix, 0, ribParityCurSess, 1, 120, 0, "10.9.9.12", nil)
	route(ribParityPeerIP, ribParityRIB, ribParityWithdrawnPrefix, 0, ribParityCurSess, 2, 121, 1, "10.9.9.12", nil)
	route(ribParityPeerIP, ribParityRIB, ribParityOldSessionPrefix, 0, ribParityOldSess, 1, 122, 0, "10.6.6.7", nil)
	route(ribParityDownPeer, ribParityRIB, ribParityDownPeerPrefix, 0, ribParityCurSess, 1, 123, 0, "10.9.9.13", nil)
	route(ribParityStrangerPeer, ribParityRIB, ribParityStrangerPrefix, 0, ribParityCurSess, 1, 124, 0, "10.9.9.14", nil)
}

// statsEventFixture is the write-side shape this package's own tests need to
// build one stats_events row -- routeUnicastFixture's own counterpart for
// the one table this package writes with a Map column. Counters is the RAW
// map the column stores, never anything pre-shaped for has(counters, 8):
// a fixture that wants "the router never sent stat type 8" writes a
// Counters map with no key 8 in it (nil, or any map missing that key), and a
// fixture that wants "the router reported an explicit zero" writes
// map[uint32]uint64{8: 0} -- the same 0 counters[8] returns for the first
// case, which is the entire point LocRIBComparison's HasStat exists to
// separate.
type statsEventFixture struct {
	RouterIP, RouterSysname, PeerIP, RIB string
	// Collector defaults to defaultFixtureCollector, as
	// routeUnicastFixture.Collector does and for the same reason.
	Collector                 string
	PeerASN                   uint32
	PeerBGPID                 string
	SessionID, Seq, StreamSeq uint64
	Counters                  map[uint32]uint64
	TsRouter, TsCollector     time.Time
}

// insertStatsEvent writes one stats_events row via q's own connection and
// database, in the same column order sink.insertStats uses against the same
// table -- see the comment above insertPeerEvent for why the argument count
// and order there matter, and sink/clickhouse.go's own "stats_events has 14
// columns" comment for this table's count. Collector_id defaults to
// defaultFixtureCollector, matching insertPeerEvent and
// insertRouteUnicastEvent's own default; the parity fixture is the one
// caller that names its own collector, so that its collectors' summaries
// hold nothing another fixture wrote.
func insertStatsEvent(t *testing.T, ctx context.Context, q *Q, f statsEventFixture) {
	t.Helper()
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".stats_events")
	if err != nil {
		t.Fatalf("prepare stats_events insert: %v", err)
	}
	if err := b.Append(
		cmp.Or(f.Collector, defaultFixtureCollector), f.RouterIP, f.RouterSysname, f.PeerIP, f.RIB, f.PeerASN,
		f.PeerBGPID, f.SessionID, f.Seq, f.TsRouter, f.TsCollector,
		[]string{}, f.StreamSeq,
		f.Counters,
	); err != nil {
		t.Fatalf("append stats_events row: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send stats_events batch: %v", err)
	}
}
