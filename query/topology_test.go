package query

import (
	"cmp"
	"context"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// topoAnchor is the instant every row this file writes is stamped near,
// truncated to a microsecond for collectionFixtureAnchor's reason:
// ts_collector is DateTime64(6) and an untruncated time.Now() does not
// survive the round trip intact, which would make every FirstSeen assertion
// below compare two values that differ in the sub-microsecond digits.
//
// It is time.Now() rather than a fixed date for that anchor's OTHER reason:
// route_unicast carries `TTL toDateTime(ts_collector) + INTERVAL 90 DAY`
// (schema.sql), so a fixed anchor would eventually become TTL-eligible and a
// background merge could purge these rows out from under a passing test.
var topoAnchor = time.Now().UTC().Truncate(time.Microsecond)

// The routers this file reserves. testDB is shared by every test in this
// package and is never truncated, so each fixture gets a router IP nothing
// else writes under -- 10.94.0.0/16 is unused elsewhere in query/, the way
// collection_test.go reserves 10.93.0.0/16 -- and every assertion below is
// made through a RouteFilter scoped to one of them rather than over the whole
// table.
const (
	// topoRouterWithdraw carries two routes: one live on 65010 -> 65012, one
	// WITHDRAWN on 65010 -> 65011. The two edges are disjoint on purpose --
	// see TestTopologyUnicastKeepsWithdrawnEdges.
	topoRouterWithdraw = "10.94.1.1"
	// topoRouterRoles carries five routes built so that no two ASNs in its
	// graph share a role combination and no two edges share a
	// (Routes, LiveRoutes) pair. See seedTopoRolesRouter for the table.
	topoRouterRoles = "10.94.2.1"
	// topoRouterOneHop carries one route whose AS path is a single ASN: a
	// node and no edge at all, which is a real answer rather than an empty
	// one.
	topoRouterOneHop = "10.94.3.1"
	// topoRouterScope carries two peers whose graphs are entirely disjoint,
	// so narrowing by peer= can only pass for its own reason.
	topoRouterScope = "10.94.4.1"
	// topoRouterPrepend carries one route whose AS path PREPENDS: the same
	// ASN twice in a row. See TestTopologyUnicastIgnoresPathPrepending.
	topoRouterPrepend = "10.94.5.1"
	// topoRouterLoop carries one route whose AS path visits the same ASN
	// twice NON-consecutively, so one adjacency appears twice in one path.
	// See TestTopologyUnicastCountsALoopedAdjacencyOnce.
	topoRouterLoop = "10.94.6.1"
	// topoRouterStale carries three peers that separate "in the graph" from
	// "in the table": one current and up, one down, one whose routes belong
	// to a superseded session. See seedTopoStaleRouter.
	topoRouterStale = "10.94.7.1"

	// The four routers below write to route_vpn and route_evpn rather than
	// route_unicast, and their ASNs are chosen to overlap nothing they can
	// be confused with -- except 65020/65021, which topoRouterVPN shares with
	// topoRouterRoles deliberately: those two ASNs are chosen
	// for the VPN identity test, and a route_vpn graph never reads
	// route_unicast, so the reuse is a coincidence of numbering rather than
	// a shared population.
	//
	// Their ROUTE DISTINGUISHERS are all 65094:<n>, which is the same
	// reservation the 10.94.0.0/16 router block is and for a reason this
	// file has now paid for once: TestVPNRoutesFiltersByRDAndFamily asks
	// route_vpn for one rd with no router beside it, so an rd another fixture
	// already uses is not a name collision, it is an extra row in that test's
	// answer. 65094 is nobody else's; <n> is the router's own third octet
	// followed by a serial.

	// topoRouterVPN carries two VPN routes identical in every column of the
	// UNICAST route identity and differing only in rd. See
	// TestTopologyVPNGroupsOnTheVPNIdentity.
	topoRouterVPN = "10.94.10.1"
	// topoRouterVPNPrepend carries one VPN route whose AS path PREPENDS, the
	// route_vpn counterpart of topoRouterPrepend. See
	// TestTopologyVPNIgnoresPathPrepending.
	topoRouterVPNPrepend = "10.94.11.1"
	// topoRouterEVPN carries nine EVPN routes, each differing from the first
	// in exactly one column of the EVPN route identity. See
	// TestTopologyEVPNGroupsOnTheEVPNIdentity.
	topoRouterEVPN = "10.94.12.1"
	// topoRouterEVPNPrepend carries one EVPN route whose AS path PREPENDS,
	// the route_evpn counterpart of topoRouterPrepend. See
	// TestTopologyEVPNIgnoresPathPrepending.
	topoRouterEVPNPrepend = "10.94.13.1"
	// topoRouterVPNWithdraw carries a withdrawn VPN route on an edge no other
	// route reaches, the route_vpn counterpart of topoRouterWithdraw. See
	// TestTopologyVPNKeepsWithdrawnEdges.
	topoRouterVPNWithdraw = "10.94.14.1"
	// topoRouterEVPNWithdraw is the same fixture on route_evpn. See
	// TestTopologyEVPNKeepsWithdrawnEdges.
	topoRouterEVPNWithdraw = "10.94.15.1"

	// topoRouterCommunity, topoRouterVPNCommunity and topoRouterEVPNCommunity
	// carry the fixtures /v1/topology's community= filter is tested against:
	// a route matching a community= value, one that does not, and -- on the
	// unicast router -- a withdrawn one whose last advertisement matched. See
	// seedTopoCommunityRouter.
	topoRouterCommunity     = "10.94.200.1"
	topoRouterVPNCommunity  = "10.94.201.1"
	topoRouterEVPNCommunity = "10.94.202.1"
)

// The peers those routers advertise through. Each router's peers are its own,
// so a fixture can be read without holding the others in mind.
const (
	topoPeerWithdraw = "10.94.1.2"
	topoPeerRoles    = "10.94.2.2"
	topoPeerOneHop   = "10.94.3.2"
	topoPeerScopeA   = "10.94.4.2"
	topoPeerScopeB   = "10.94.4.3"
	topoPeerPrepend  = "10.94.5.2"
	topoPeerLoop     = "10.94.6.2"
	topoPeerStaleUp  = "10.94.7.2"
	topoPeerStaleDwn = "10.94.7.3"
	topoPeerStaleOld = "10.94.7.4"

	topoPeerVPN         = "10.94.10.2"
	topoPeerVPNPrepend  = "10.94.11.2"
	topoPeerEVPN        = "10.94.12.2"
	topoPeerEVPNPrepend = "10.94.13.2"
	topoPeerVPNWithdraw = "10.94.14.2"
	topoPeerEVPNWdraw   = "10.94.15.2"

	topoPeerCommunity     = "10.94.200.2"
	topoPeerVPNCommunity  = "10.94.201.2"
	topoPeerEVPNCommunity = "10.94.202.2"
)

// topoSession is the BMP session most fixtures here write under. A single
// value is safe because each fixture owns its router outright, and `cur`
// (peerStateCTE) resolves the current session per (collector, router): two
// routers sharing a session number never see each other's rows.
//
// seedTopoStaleRouter is the one fixture that needs two, and it names them
// itself.
const topoSession = 1

// topoRow is one route_unicast observation this file writes: the fields the
// topology statement actually reads, and nothing else.
type topoRow struct {
	router, sysname, peer string
	peerASN               uint32
	prefix                string
	// path is the AS path this observation ADVERTISES. It is ignored when
	// withdraw is set -- see insertTopoRoute, where dropping it is
	// load-bearing rather than tidy.
	path     []uint32
	seq      uint64
	withdraw bool
	ts       time.Time
	// session is the BMP session this observation belongs to, defaulting to
	// topoSession. Only seedTopoStaleRouter sets it: a route row carrying a
	// session the router has since replaced is the fixture for routesSQL's
	// `r.session_id = cur.sid` join, which this statement inherits.
	session uint64
}

// insertTopoRoute writes one route_unicast row.
//
// A withdrawal's path is dropped rather than written, and that is the single
// most load-bearing line in this file. A BGP withdrawal carries NLRI and no
// path attributes, so all 1,619 withdraw rows in the live archive have an
// empty as_path -- and a fixture that wrote a path onto a withdrawal would
// let TestTopologyUnicastKeepsWithdrawnEdges pass under a plain argMax, which
// is the exact mutation that test exists to catch. The fixture would then be
// confirming the statement against a world the collector never produces.
func insertTopoRoute(t *testing.T, ctx context.Context, q *Q, r topoRow) {
	t.Helper()
	var isWithdraw uint8
	path := r.path
	if r.withdraw {
		isWithdraw = 1
		path = nil
	}
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: r.router, RouterSysname: r.sysname, PeerIP: r.peer,
		RIB: "in_pre", PeerASN: r.peerASN, PeerBGPID: r.peer, Family: "ipv4u",
		Prefix:    r.prefix,
		SessionID: cmp.Or(r.session, uint64(topoSession)), Seq: r.seq, StreamSeq: r.seq,
		IsWithdraw: isWithdraw, ASPath: path,
		TsRouter: r.ts, TsCollector: r.ts,
	})
}

// insertTopoPeerUp writes the peer_events row every fixture here needs and
// none of them is about.
//
// It is not optional decoration. topologySQL inherits routesSQL's two
// INNER JOINs -- the current session (cur) and peer_up with state = 'up' --
// so a fixture that wrote route rows and no peer event returns an EMPTY GRAPH
// that reads exactly like a broken query. Every seed function below calls
// this first for that reason.
func insertTopoPeerUp(t *testing.T, ctx context.Context, q *Q, router, sysname, peer string, asn uint32) {
	t.Helper()
	insertTopoPeerEvent(t, ctx, q, router, sysname, peer, asn, "up", topoSession, 1)
}

// insertTopoPeerEvent is insertTopoPeerUp with the two fields
// seedTopoStaleRouter has to vary: the session the event belongs to, and
// whether the session ends up or down.
func insertTopoPeerEvent(t *testing.T, ctx context.Context, q *Q,
	router, sysname, peer string, asn uint32, kind string, session, seq uint64) {
	t.Helper()
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: router, RouterSysname: sysname, PeerIP: peer, RIB: "in_pre",
		PeerASN: asn, PeerBGPID: peer,
		SessionID: session, Seq: seq, StreamSeq: seq,
		Kind:     kind,
		TsRouter: topoAnchor, TsCollector: topoAnchor,
	})
}

// topologyFixturesOnce guards every seed function below, because testDB is
// recreated once per test BINARY (chtest.ensureDatabase) rather than once per
// test, and is never truncated. Seeding twice would not raise: route_unicast
// is a ReplacingMergeTree, a byte-identical second insert lands as a second
// row in a new part, and this package reads no FINAL -- so `routes` and
// `live_routes` would silently double depending on which tests ran and in
// what order. This is seedCollectionFixtures' pattern, for its reason.
var topologyFixturesOnce sync.Once

// seedTopologyFixtures writes this file's entire population, exactly once per
// test binary. Every test calls it; none of them may seed anything of its
// own outside it.
func seedTopologyFixtures(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	topologyFixturesOnce.Do(func() {
		seedTopoWithdrawRouter(t, ctx, q)
		seedTopoRolesRouter(t, ctx, q)
		seedTopoOneHopRouter(t, ctx, q)
		seedTopoScopeRouter(t, ctx, q)
		seedTopoPrependRouter(t, ctx, q)
		seedTopoLoopRouter(t, ctx, q)
		seedTopoStaleRouter(t, ctx, q)
		seedTopoVPNRouter(t, ctx, q)
		seedTopoVPNPrependRouter(t, ctx, q)
		seedTopoEVPNRouter(t, ctx, q)
		seedTopoEVPNPrependRouter(t, ctx, q)
		seedTopoVPNWithdrawRouter(t, ctx, q)
		seedTopoEVPNWithdrawRouter(t, ctx, q)
		seedTopoCommunityRouter(t, ctx, q)
		seedTopoVPNCommunityRouter(t, ctx, q)
		seedTopoEVPNCommunityRouter(t, ctx, q)
		seedTopoDualCollectorRouter(t, ctx, q)
		seedTopoFirstSeenRouter(t, ctx, q)
		seedTopoRetentionRouter(t, ctx, q)
		seedTopoRewithdrawRouter(t, ctx, q)
	})
}

// seedTopoWithdrawRouter writes the fixture proving a withdrawn route's
// edge is still drawn.
//
//	10.94.1.0/24    advertised, never withdrawn   65010 -> 65012
//	10.94.1.128/25  advertised, then WITHDRAWN    65010 -> 65011
//
// The two edges share no ASN pair. That is what makes
// TestTopologyUnicastKeepsWithdrawnEdges unable to pass for the wrong reason:
// 65010 -> 65011 is carried by exactly one route in the whole database, and
// that route's newest observation is a withdrawal, so the only way the edge
// can appear is for the statement to have resolved the last ADVERTISED path.
func seedTopoWithdrawRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-withdraw-router"
	insertTopoPeerUp(t, ctx, q, topoRouterWithdraw, sysname, topoPeerWithdraw, 65010)

	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterWithdraw, sysname: sysname, peer: topoPeerWithdraw,
		peerASN: 65010, prefix: "10.94.1.0/24",
		path: []uint32{65010, 65012}, seq: 1, ts: topoAnchor.Add(1 * time.Second),
	})

	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterWithdraw, sysname: sysname, peer: topoPeerWithdraw,
		peerASN: 65010, prefix: "10.94.1.128/25",
		path: []uint32{65010, 65011}, seq: 2, ts: topoAnchor.Add(2 * time.Second),
	})
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterWithdraw, sysname: sysname, peer: topoPeerWithdraw,
		peerASN: 65010, prefix: "10.94.1.128/25",
		withdraw: true, seq: 3, ts: topoAnchor.Add(3 * time.Second),
	})

	// A third route this collector has ONLY ever seen withdrawn -- an
	// ordinary thing to observe, since a BMP session can begin mid-life and
	// a peer can withdraw a prefix we never watched it announce. Its live
	// path is empty, so it contributes no node and no edge, and
	// `HAVING length(live_as_path) > 0` is what keeps it out of the
	// POPULATION too: without that clause Graph.Routes here reads 3, a graph
	// claiming to describe a route it drew nothing for.
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterWithdraw, sysname: sysname, peer: topoPeerWithdraw,
		peerASN: 65010, prefix: "10.94.1.192/26",
		withdraw: true, seq: 4, ts: topoAnchor.Add(4 * time.Second),
	})
	// Merge the current table now, not whenever the background merge
	// runs: until then it still holds 10.94.1.128/25's advertisement, and
	// the withdrawn edge would be visible to a statement that read the
	// current table alone.
	collapseTopoCurrent(t, ctx, q, topoRouterWithdraw)
}

// seedTopoRolesRouter writes five routes under one peer (AS 65020), built so
// that every claim TopologyUnicast makes about a node or an edge has a witness
// that can only pass for its own reason.
//
//	route  prefix          path                     state      first seen
//	R1     10.94.20.0/24   65020 65021 65022        live       anchor+10s
//	R2     10.94.21.0/24   65020 65021 65023        live       anchor+20s
//	R3     10.94.22.0/24   65020 65021 65022        WITHDRAWN  anchor+5s
//	R4     10.94.23.0/24   65020 65021              live       anchor+30s
//	R5     10.94.24.0/24   65020 65025 65022        live       anchor+40s
//
// The node roles it produces are pairwise distinct, so no two of them can be
// satisfied by one wrong expression:
//
//	65020  transit, peer            (never last in a path; it is the peer's AS)
//	65021  origin, transit          (last in R4, middle in R1-R3)
//	65022  origin                   (last in R1, R3, R5)
//	65023  origin                   (last in R2)
//	65025  transit                  (middle in R5, and not the peer's AS)
//
// The edges it produces carry four distinct (Routes, LiveRoutes) pairs:
//
//	65020 -> 65021   4 routes, 3 live    first seen anchor+5s
//	65021 -> 65022   2 routes, 1 live    first seen anchor+5s
//	65021 -> 65023   1 route,  1 live    first seen anchor+20s
//	65020 -> 65025   1 route,  1 live    first seen anchor+40s
//	65025 -> 65022   1 route,  1 live    first seen anchor+40s
//
// R3 is deliberately the EARLIEST observation in the fixture and the only
// withdrawn one. That pairing is what makes the FirstSeen assertions sharp:
// an implementation that took first_seen over live routes only, or took a max
// instead of a min, reports anchor+10s for 65021 -> 65022 where the right
// answer is anchor+5s.
func seedTopoRolesRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-roles-router"
	insertTopoPeerUp(t, ctx, q, topoRouterRoles, sysname, topoPeerRoles, 65020)

	row := func(prefix string, path []uint32, seq uint64, secs int) topoRow {
		return topoRow{
			router: topoRouterRoles, sysname: sysname, peer: topoPeerRoles,
			peerASN: 65020, prefix: prefix, path: path, seq: seq,
			ts: topoAnchor.Add(time.Duration(secs) * time.Second),
		}
	}

	insertTopoRoute(t, ctx, q, row("10.94.20.0/24", []uint32{65020, 65021, 65022}, 10, 10))
	insertTopoRoute(t, ctx, q, row("10.94.21.0/24", []uint32{65020, 65021, 65023}, 11, 20))

	// R3: advertised early, withdrawn late. Its first observation is the
	// earliest in this fixture and its newest is a withdrawal.
	insertTopoRoute(t, ctx, q, row("10.94.22.0/24", []uint32{65020, 65021, 65022}, 12, 5))
	r3w := row("10.94.22.0/24", nil, 13, 25)
	r3w.withdraw = true
	insertTopoRoute(t, ctx, q, r3w)

	insertTopoRoute(t, ctx, q, row("10.94.23.0/24", []uint32{65020, 65021}, 14, 30))
	insertTopoRoute(t, ctx, q, row("10.94.24.0/24", []uint32{65020, 65025, 65022}, 15, 40))
}

// seedTopoOneHopRouter writes one route whose AS path is a single ASN. A
// one-hop path contributes a NODE and no edge, which is why the node
// statement reads live_as_path directly instead of deriving nodes from the
// edge list -- derive them from edges and this router's graph is empty.
func seedTopoOneHopRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-one-hop-router"
	insertTopoPeerUp(t, ctx, q, topoRouterOneHop, sysname, topoPeerOneHop, 65030)
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterOneHop, sysname: sysname, peer: topoPeerOneHop,
		peerASN: 65030, prefix: "10.94.30.0/24",
		path: []uint32{65030}, seq: 1, ts: topoAnchor.Add(1 * time.Second),
	})
}

// seedTopoScopeRouter writes two peers under one router whose graphs share no
// ASN at all:
//
//	10.94.4.2 (AS 65040)   65040 -> 65041
//	10.94.4.3 (AS 65042)   65042 -> 65043
//
// Disjoint is the point. A peer= predicate that rendered but bound the wrong
// value, or did not render at all, returns a graph containing the other
// peer's edge, and this fixture is the only one here where that is visible.
func seedTopoScopeRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-scope-router"
	insertTopoPeerUp(t, ctx, q, topoRouterScope, sysname, topoPeerScopeA, 65040)
	insertTopoPeerUp(t, ctx, q, topoRouterScope, sysname, topoPeerScopeB, 65042)

	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterScope, sysname: sysname, peer: topoPeerScopeA,
		peerASN: 65040, prefix: "10.94.40.0/24",
		path: []uint32{65040, 65041}, seq: 1, ts: topoAnchor.Add(1 * time.Second),
	})
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterScope, sysname: sysname, peer: topoPeerScopeB,
		peerASN: 65042, prefix: "10.94.41.0/24",
		path: []uint32{65042, 65043}, seq: 1, ts: topoAnchor.Add(2 * time.Second),
	})
}

// seedTopoPrependRouter writes one route whose path prepends: 65051 appears
// twice in a row. See TestTopologyUnicastIgnoresPathPrepending for what the
// statement has to do about it and why the live archive cannot show it.
func seedTopoPrependRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-prepend-router"
	insertTopoPeerUp(t, ctx, q, topoRouterPrepend, sysname, topoPeerPrepend, 65050)
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterPrepend, sysname: sysname, peer: topoPeerPrepend,
		peerASN: 65050, prefix: "10.94.50.0/24",
		path: []uint32{65050, 65051, 65051, 65052},
		seq:  1, ts: topoAnchor.Add(1 * time.Second),
	})
}

// seedTopoLoopRouter writes one route whose path visits 65060 and 65061
// twice each, non-consecutively: `65060 65061 65060 65061`. Its consecutive
// pairs are (65060,65061), (65061,65060) and (65060,65061) again -- one
// adjacency asserted twice by ONE route, which is the duplication a
// self-loop filter alone does not remove.
//
// Prepending cannot produce this shape; only a path that revisits an AS can,
// and eBGP loop detection makes that rare rather than impossible -- a
// pre-policy adj-RIB-in is exactly where a route carrying a loop is visible,
// because that is what the peer sent before anything rejected it.
func seedTopoLoopRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-loop-router"
	insertTopoPeerUp(t, ctx, q, topoRouterLoop, sysname, topoPeerLoop, 65060)
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterLoop, sysname: sysname, peer: topoPeerLoop,
		peerASN: 65060, prefix: "10.94.60.0/24",
		path: []uint32{65060, 65061, 65060, 65061},
		seq:  1, ts: topoAnchor.Add(1 * time.Second),
	})
}

// seedTopoStaleRouter writes three peers under one router, so that "in the
// graph" can be told apart from "in route_unicast":
//
//	10.94.7.2  up in the CURRENT session, route in it       65070 -> 65071
//	10.94.7.3  up then DOWN in the current session          65072 -> 65073
//	10.94.7.4  up in the current session, route in the      65074 -> 65075
//	           SUPERSEDED one
//
// Only the first belongs in the answer. The other two are what
// topologySQL's two inherited INNER JOINs exist for, and they fail
// differently: the down peer is rejected by the peer_up gate (servedGate),
// the stale route by `r.session_id = cur.sid`. A statement missing one join
// still excludes the other peer, so one fixture with one wrong peer could not
// tell which join had been dropped -- hence two.
//
// The current session is 2 and the superseded one is 1, and 10.94.7.4's
// peer_events row is written under session 2 while its ROUTE rows are under
// session 1. That separation is the point: if the peer event were stale too,
// the peer_up join would reject the route as well and the cur join would
// never be exercised.
func seedTopoStaleRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-stale-router"
	const oldSession, curSession = 1, 2

	insertTopoPeerEvent(t, ctx, q, topoRouterStale, sysname, topoPeerStaleUp,
		65070, "up", curSession, 1)
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterStale, sysname: sysname, peer: topoPeerStaleUp,
		peerASN: 65070, prefix: "10.94.70.0/24", path: []uint32{65070, 65071},
		seq: 1, ts: topoAnchor.Add(1 * time.Second), session: curSession,
	})

	// Up, then down at a higher seq: the session ENDS down, which is what
	// peer_up resolves. A peer that merely went down once and came back is a
	// different fixture (insertFlappedPeerFixture) and a different claim.
	insertTopoPeerEvent(t, ctx, q, topoRouterStale, sysname, topoPeerStaleDwn,
		65072, "up", curSession, 1)
	insertTopoPeerEvent(t, ctx, q, topoRouterStale, sysname, topoPeerStaleDwn,
		65072, "down", curSession, 2)
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterStale, sysname: sysname, peer: topoPeerStaleDwn,
		peerASN: 65072, prefix: "10.94.72.0/24", path: []uint32{65072, 65073},
		seq: 1, ts: topoAnchor.Add(2 * time.Second), session: curSession,
	})

	insertTopoPeerEvent(t, ctx, q, topoRouterStale, sysname, topoPeerStaleOld,
		65074, "up", curSession, 1)
	insertTopoRoute(t, ctx, q, topoRow{
		router: topoRouterStale, sysname: sysname, peer: topoPeerStaleOld,
		peerASN: 65074, prefix: "10.94.74.0/24", path: []uint32{65074, 65075},
		seq: 1, ts: topoAnchor.Add(3 * time.Second), session: oldSession,
	})
}

// insertTopoVPNRoute writes one route_vpn row, defaulting the fields no
// fixture here varies.
//
// It is a second helper rather than a generalization of insertTopoRoute
// because route_vpn's write shape is genuinely a different one -- rd and
// labels have nowhere to go on a unicast row, and family is vpn4 here where
// it is ipv4u there. What it DOES share is the rule that makes
// insertTopoRoute honest, and for the same reason: a withdrawal's path is
// dropped rather than written, because a BGP withdrawal carries NLRI and no
// path attributes.
func insertTopoVPNRoute(t *testing.T, ctx context.Context, q *Q, f routeVPNFixture) {
	t.Helper()
	if f.IsWithdraw != 0 {
		f.ASPath = nil
	}
	f.RIB = cmp.Or(f.RIB, "in_pre")
	f.Family = cmp.Or(f.Family, "vpn4")
	f.PeerBGPID = cmp.Or(f.PeerBGPID, f.PeerIP)
	f.SessionID = cmp.Or(f.SessionID, uint64(topoSession))
	f.StreamSeq = cmp.Or(f.StreamSeq, f.Seq)
	insertRouteVPNEvent(t, ctx, q, f)
}

// seedTopoVPNRouter writes the fixture the VPN route identity is tested
// against: TWO route_vpn rows identical in every column of the UNICAST route
// identity -- same collector, router, peer, rib, family, prefix and path_id
// -- and differing only in rd.
//
//	rd 65094:100   10.94.100.0/24   65020 65021
//	rd 65094:101   10.94.100.0/24   65020 65021
//
// Both carry the SAME AS path, which is the fixture's whole point. The defect
// under test is a wrong COUNT rather than a missing edge: 65020 -> 65021 is
// drawn whichever identity the statement groups on, so the only thing that
// can move is Routes, and there is nothing else for a passing assertion to be
// reading. Grouped on route_unicast's key these two collapse into one route
// and the edge reports Routes = 1; grouped on route_vpn's they are two.
//
// This is what keeps these counts right -- each family groups on ITS OWN
// route identity -- asserted rather than assumed.
func seedTopoVPNRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-vpn-router"
	insertTopoPeerUp(t, ctx, q, topoRouterVPN, sysname, topoPeerVPN, 65020)

	for i, rd := range []string{"65094:100", "65094:101"} {
		insertTopoVPNRoute(t, ctx, q, routeVPNFixture{
			RouterIP: topoRouterVPN, RouterSysname: sysname, PeerIP: topoPeerVPN,
			PeerASN: 65020, RD: rd, Prefix: "10.94.100.0/24",
			ASPath: []uint32{65020, 65021}, Seq: uint64(i + 1),
			TsRouter:    topoAnchor.Add(time.Duration(i+1) * time.Second),
			TsCollector: topoAnchor.Add(time.Duration(i+1) * time.Second),
		})
	}
}

// seedTopoVPNPrependRouter writes one VPN route whose path prepends:
// `65080 65081 65081 65082`, the route_vpn twin of seedTopoPrependRouter.
//
// The twin is not redundant. The prepending correction lives in
// topologyEdgesSQL and topologyNodesSQL, which all three families share, so a
// reader can argue that testing it once tests it everywhere -- and that
// argument is exactly what a fixture is for, because it is only true while
// the text stays shared. If a future change splits the edge SELECT per
// family to add a column only one of them has, the unicast fixture would go
// on passing while VPN quietly drew a self-loop.
func seedTopoVPNPrependRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-vpn-prepend-router"
	insertTopoPeerUp(t, ctx, q, topoRouterVPNPrepend, sysname, topoPeerVPNPrepend, 65080)
	insertTopoVPNRoute(t, ctx, q, routeVPNFixture{
		RouterIP: topoRouterVPNPrepend, RouterSysname: sysname, PeerIP: topoPeerVPNPrepend,
		PeerASN: 65080, RD: "65094:110", Prefix: "10.94.110.0/24",
		ASPath: []uint32{65080, 65081, 65081, 65082}, Seq: 1,
		TsRouter:    topoAnchor.Add(1 * time.Second),
		TsCollector: topoAnchor.Add(1 * time.Second),
	})
}

// insertTopoEVPNRoute writes one route_evpn row, defaulting the fields no
// fixture here varies and dropping a withdrawal's path for insertTopoRoute's
// reason.
func insertTopoEVPNRoute(t *testing.T, ctx context.Context, q *Q, f routeEVPNFixture) {
	t.Helper()
	if f.IsWithdraw != 0 {
		f.ASPath = nil
	}
	f.RIB = cmp.Or(f.RIB, "in_pre")
	f.PeerBGPID = cmp.Or(f.PeerBGPID, f.PeerIP)
	f.SessionID = cmp.Or(f.SessionID, uint64(topoSession))
	f.StreamSeq = cmp.Or(f.StreamSeq, f.Seq)
	insertRouteEVPNEvent(t, ctx, q, f)
}

// seedTopoEVPNRouter writes NINE route_evpn rows that differ from the first in
// exactly one column of the EVPN route identity apiece:
//
//	#  differs in      value
//	1  (the base)      type 2, rd 65094:120, prefix "", mac ..:01,
//	                   ip 10.94.120.1, tag 0, esi "", path_id 0
//	2  route_type      3
//	3  rd              65094:121
//	4  prefix          10.94.121.0/24
//	5  mac             ..:02
//	6  ip              10.94.120.2
//	7  ethernet_tag    7
//	8  esi             00:01:02:03:04:05:06:07:08:09
//	9  path_id         1
//
// All nine carry the SAME AS path, 65090 -> 65091, for the reason the VPN
// fixture's two do: the edge is drawn whatever the statement groups on, so
// Routes is the only thing that can move and a passing assertion has nothing
// else to be reading.
//
// One row per identity column, rather than one row for the whole key, is what
// makes the failure legible: nine is the answer only if every one of the eight
// columns discriminates, and a key missing any single one of them reports
// eight. A single "everything differs" pair would report 2 against 1 for any
// omission and never say which.
//
// Row 4 is artificial on purpose -- a type-2 route carrying an IP prefix is
// not a thing a router sends, and 136 of the archive's 202 EVPN rows carry
// prefix "" precisely because their type does not have one. It is here because
// prefix IS in the route identity, and a fixture that only ever varied prefix
// alongside route_type could not tell the two columns apart.
func seedTopoEVPNRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-evpn-router"
	insertTopoPeerUp(t, ctx, q, topoRouterEVPN, sysname, topoPeerEVPN, 65090)

	base := routeEVPNFixture{
		RouterIP: topoRouterEVPN, RouterSysname: sysname, PeerIP: topoPeerEVPN,
		PeerASN: 65090, RouteType: 2, RD: "65094:120", Prefix: "",
		MAC: "00:11:22:33:44:01", IP: "10.94.120.1", EthernetTag: 0, ESI: "",
		PathID: 0, ASPath: []uint32{65090, 65091},
	}
	vary := []func(f *routeEVPNFixture){
		func(f *routeEVPNFixture) {},
		func(f *routeEVPNFixture) { f.RouteType = 3 },
		func(f *routeEVPNFixture) { f.RD = "65094:121" },
		func(f *routeEVPNFixture) { f.Prefix = "10.94.121.0/24" },
		func(f *routeEVPNFixture) { f.MAC = "00:11:22:33:44:02" },
		func(f *routeEVPNFixture) { f.IP = "10.94.120.2" },
		func(f *routeEVPNFixture) { f.EthernetTag = 7 },
		func(f *routeEVPNFixture) { f.ESI = "00:01:02:03:04:05:06:07:08:09" },
		func(f *routeEVPNFixture) { f.PathID = 1 },
	}
	for i, v := range vary {
		f := base
		v(&f)
		f.Seq = uint64(i + 1)
		f.TsRouter = topoAnchor.Add(time.Duration(i+1) * time.Second)
		f.TsCollector = f.TsRouter
		insertTopoEVPNRoute(t, ctx, q, f)
	}
}

// seedTopoEVPNPrependRouter writes one EVPN route whose path prepends:
// `65095 65096 65096 65097`. See seedTopoVPNPrependRouter for why the third
// copy of this fixture earns its place.
func seedTopoEVPNPrependRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-evpn-prepend-router"
	insertTopoPeerUp(t, ctx, q, topoRouterEVPNPrepend, sysname, topoPeerEVPNPrepend, 65095)
	insertTopoEVPNRoute(t, ctx, q, routeEVPNFixture{
		RouterIP: topoRouterEVPNPrepend, RouterSysname: sysname, PeerIP: topoPeerEVPNPrepend,
		PeerASN: 65095, RouteType: 5, RD: "65094:130", Prefix: "10.94.130.0/24",
		ASPath: []uint32{65095, 65096, 65096, 65097}, Seq: 1,
		TsRouter:    topoAnchor.Add(1 * time.Second),
		TsCollector: topoAnchor.Add(1 * time.Second),
	})
}

// seedTopoVPNWithdrawRouter writes seedTopoWithdrawRouter's fixture on
// route_vpn:
//
//	10.94.140.0/24    rd 65094:140  advertised, never withdrawn  65100 -> 65102
//	10.94.140.128/25  rd 65094:141  advertised, then WITHDRAWN   65100 -> 65101
//	10.94.140.192/26  rd 65094:142  only ever seen withdrawn     (no path)
//
// The two edges share no ASN pair, which is what makes the test unable to
// pass for the wrong reason: 65100 -> 65101 is carried by exactly one route
// in the whole database, and that route's newest observation is a
// withdrawal, so the only way the edge can appear at all is for the statement
// to have resolved the last ADVERTISED path.
//
// The third route is the route_vpn version of the one the collector has only
// ever seen withdrawn -- an ordinary shape there, since 3,079 of the
// archive's 6,907 route_vpn rows carry an empty as_path. Its live path is
// empty, so `HAVING length(live_as_path) > 0` keeps it out of the POPULATION
// as well as out of the drawing, and Graph.Routes is 2 rather than 3.
//
// This fixture exists for the reason seedTopoVPNPrependRouter does, and the
// case is stronger here. Withdrawal is DATA, not a filter, and that is
// this endpoint's central constraint, and it lives in two lines of
// topologySQL that all three families now share. A reader can argue that
// testing it once tests it everywhere, and that argument is true only while
// the text stays shared. Proven for one family and argued for two is exactly
// the shape this fixture is written against.
func seedTopoVPNWithdrawRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-vpn-withdraw-router"
	insertTopoPeerUp(t, ctx, q, topoRouterVPNWithdraw, sysname, topoPeerVPNWithdraw, 65100)

	row := func(rd, prefix string, path []uint32, seq uint64, withdraw uint8) routeVPNFixture {
		return routeVPNFixture{
			RouterIP: topoRouterVPNWithdraw, RouterSysname: sysname,
			PeerIP: topoPeerVPNWithdraw, PeerASN: 65100,
			RD: rd, Prefix: prefix, ASPath: path, Seq: seq, IsWithdraw: withdraw,
			TsRouter:    topoAnchor.Add(time.Duration(seq) * time.Second),
			TsCollector: topoAnchor.Add(time.Duration(seq) * time.Second),
		}
	}

	insertTopoVPNRoute(t, ctx, q, row("65094:140", "10.94.140.0/24",
		[]uint32{65100, 65102}, 1, 0))

	// Advertised, then withdrawn at a higher seq. The withdrawal repeats every
	// column of the VPN route identity, or it would be a different route
	// rather than this one's newest observation.
	insertTopoVPNRoute(t, ctx, q, row("65094:141", "10.94.140.128/25",
		[]uint32{65100, 65101}, 2, 0))
	insertTopoVPNRoute(t, ctx, q, row("65094:141", "10.94.140.128/25", nil, 3, 1))

	insertTopoVPNRoute(t, ctx, q, row("65094:142", "10.94.140.192/26", nil, 4, 1))
}

// seedTopoEVPNWithdrawRouter is seedTopoVPNWithdrawRouter on route_evpn, with
// route type 5 throughout so the three routes differ in the columns a
// prefix-carrying EVPN route really differs in:
//
//	10.94.150.0/24    rd 65094:150  advertised, never withdrawn  65110 -> 65112
//	10.94.150.128/25  rd 65094:151  advertised, then WITHDRAWN   65110 -> 65111
//	10.94.150.192/26  rd 65094:152  only ever seen withdrawn     (no path)
//
// 544 of the archive's 904 route_evpn rows carry an empty as_path, so the
// third route is the ordinary shape here rather than a contrived one.
func seedTopoEVPNWithdrawRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-evpn-withdraw-router"
	insertTopoPeerUp(t, ctx, q, topoRouterEVPNWithdraw, sysname, topoPeerEVPNWdraw, 65110)

	row := func(rd, prefix string, path []uint32, seq uint64, withdraw uint8) routeEVPNFixture {
		return routeEVPNFixture{
			RouterIP: topoRouterEVPNWithdraw, RouterSysname: sysname,
			PeerIP: topoPeerEVPNWdraw, PeerASN: 65110,
			RouteType: 5, RD: rd, Prefix: prefix,
			ASPath: path, Seq: seq, IsWithdraw: withdraw,
			TsRouter:    topoAnchor.Add(time.Duration(seq) * time.Second),
			TsCollector: topoAnchor.Add(time.Duration(seq) * time.Second),
		}
	}

	insertTopoEVPNRoute(t, ctx, q, row("65094:150", "10.94.150.0/24",
		[]uint32{65110, 65112}, 1, 0))

	insertTopoEVPNRoute(t, ctx, q, row("65094:151", "10.94.150.128/25",
		[]uint32{65110, 65111}, 2, 0))
	insertTopoEVPNRoute(t, ctx, q, row("65094:151", "10.94.150.128/25", nil, 3, 1))

	insertTopoEVPNRoute(t, ctx, q, row("65094:152", "10.94.150.192/26", nil, 4, 1))
}

// seedTopoCommunityRouter writes the fixture this file's community= tests
// read: /v1/topology?community= raised a ClickHouse
// "missing column" 500, because topologySQL's `latest` CTE never defined
// live_communities or live_route_targets -- the columns query/filters.go's
// community predicate names in its HAVING clause -- where origin_asn= and
// through_asn= (community's own siblings on that HAVING channel) both
// returned 200.
//
//	10.94.200.0/24    live, community 65000:100                 65200 -> 65201
//	10.94.200.128/25  live, no community                        65200 -> 65202
//	10.94.200.192/26  advertised with 65000:100, then WITHDRAWN  65200 -> 65203
//
// The third route is the mutation-check's own witness. 65200 -> 65203 is
// carried by exactly one route, and that route's newest ROW is the
// withdrawal, which -- like a withdrawal's as_path -- carries no
// communities. The predicate can only still find it if the aggregate reads
// the last ADVERTISED communities the way live_as_path already reads the
// last advertised path: argMaxIf(..., is_withdraw = 0), not a plain argMax
// over the whole group. A plain argMax reports the withdrawal's own empty
// communities array, and 65200 -> 65203 silently drops out of the answer.
func seedTopoCommunityRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-community-router"
	insertTopoPeerUp(t, ctx, q, topoRouterCommunity, sysname, topoPeerCommunity, 65200)

	// 65000:100, encoded the way routesSQL's own community column is: high
	// 16 bits 65000, low 16 bits 100.
	standardCommunity := []uint32{uint32(65000)<<16 | 100}

	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: topoRouterCommunity, RouterSysname: sysname, PeerIP: topoPeerCommunity,
		RIB: "in_pre", PeerASN: 65200, PeerBGPID: topoPeerCommunity, Family: "ipv4u",
		Prefix: "10.94.200.0/24", SessionID: topoSession, Seq: 1, StreamSeq: 1,
		ASPath: []uint32{65200, 65201}, Communities: standardCommunity,
		TsRouter:    topoAnchor.Add(1 * time.Second),
		TsCollector: topoAnchor.Add(1 * time.Second),
	})

	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: topoRouterCommunity, RouterSysname: sysname, PeerIP: topoPeerCommunity,
		RIB: "in_pre", PeerASN: 65200, PeerBGPID: topoPeerCommunity, Family: "ipv4u",
		Prefix: "10.94.200.128/25", SessionID: topoSession, Seq: 2, StreamSeq: 2,
		ASPath:      []uint32{65200, 65202},
		TsRouter:    topoAnchor.Add(2 * time.Second),
		TsCollector: topoAnchor.Add(2 * time.Second),
	})

	// Advertised carrying the community, then withdrawn at a higher seq. The
	// withdrawal repeats every column of the route identity -- collector,
	// router, peer, rib, family, prefix, path_id -- or it would be a
	// different route rather than this one's newest observation, and its
	// path is dropped for insertTopoRoute's reason: a BGP withdrawal carries
	// NLRI and no path attributes.
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: topoRouterCommunity, RouterSysname: sysname, PeerIP: topoPeerCommunity,
		RIB: "in_pre", PeerASN: 65200, PeerBGPID: topoPeerCommunity, Family: "ipv4u",
		Prefix: "10.94.200.192/26", SessionID: topoSession, Seq: 3, StreamSeq: 3,
		ASPath: []uint32{65200, 65203}, Communities: standardCommunity,
		TsRouter:    topoAnchor.Add(3 * time.Second),
		TsCollector: topoAnchor.Add(3 * time.Second),
	})
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		RouterIP: topoRouterCommunity, RouterSysname: sysname, PeerIP: topoPeerCommunity,
		RIB: "in_pre", PeerASN: 65200, PeerBGPID: topoPeerCommunity, Family: "ipv4u",
		Prefix: "10.94.200.192/26", SessionID: topoSession, Seq: 4, StreamSeq: 4,
		IsWithdraw: 1,
		TsRouter:   topoAnchor.Add(4 * time.Second), TsCollector: topoAnchor.Add(4 * time.Second),
	})
}

// seedTopoVPNCommunityRouter writes route_vpn's counterpart to
// seedTopoCommunityRouter, with a second case the unicast fixture cannot
// express: community= accepts a ROUTE TARGET as well as a standard
// community (see query/filters.go's ParseCommunity), and route_vpn is where
// a route target is a real thing a route carries.
//
//	10.94.201.0/24    rd 65094:200  community 65210:100          65210 -> 65211
//	10.94.201.128/25  rd 65094:201  neither                      65210 -> 65212
//	10.94.201.192/26  rd 65094:202  route target 10.255.0.3:900  65210 -> 65213
func seedTopoVPNCommunityRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-vpn-community-router"
	insertTopoPeerUp(t, ctx, q, topoRouterVPNCommunity, sysname, topoPeerVPNCommunity, 65210)

	insertTopoVPNRoute(t, ctx, q, routeVPNFixture{
		RouterIP: topoRouterVPNCommunity, RouterSysname: sysname, PeerIP: topoPeerVPNCommunity,
		PeerASN: 65210, RD: "65094:200", Prefix: "10.94.201.0/24",
		ASPath: []uint32{65210, 65211}, Communities: []uint32{uint32(65210)<<16 | 100},
		Seq:         1,
		TsRouter:    topoAnchor.Add(1 * time.Second),
		TsCollector: topoAnchor.Add(1 * time.Second),
	})
	insertTopoVPNRoute(t, ctx, q, routeVPNFixture{
		RouterIP: topoRouterVPNCommunity, RouterSysname: sysname, PeerIP: topoPeerVPNCommunity,
		PeerASN: 65210, RD: "65094:201", Prefix: "10.94.201.128/25",
		ASPath:      []uint32{65210, 65212},
		Seq:         2,
		TsRouter:    topoAnchor.Add(2 * time.Second),
		TsCollector: topoAnchor.Add(2 * time.Second),
	})
	insertTopoVPNRoute(t, ctx, q, routeVPNFixture{
		RouterIP: topoRouterVPNCommunity, RouterSysname: sysname, PeerIP: topoPeerVPNCommunity,
		PeerASN: 65210, RD: "65094:202", Prefix: "10.94.201.192/26",
		ASPath: []uint32{65210, 65213}, RouteTargets: []string{"10.255.0.3:900"},
		Seq:         3,
		TsRouter:    topoAnchor.Add(3 * time.Second),
		TsCollector: topoAnchor.Add(3 * time.Second),
	})
}

// seedTopoEVPNCommunityRouter writes route_evpn's counterpart to
// seedTopoCommunityRouter, using a large community (65220:1:100) rather than
// a standard one: ParseCommunity reads a three-part all-numeric value as
// ambiguously a large community or an unnamed extended community and
// searches both live_large_communities and live_ext_communities, the two
// columns neither of the other two family fixtures exercises.
//
//	10.94.202.0/24    rd 65094:210  large community 65220:1:100  65220 -> 65221
//	10.94.202.128/25  rd 65094:211  neither                      65220 -> 65222
func seedTopoEVPNCommunityRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-evpn-community-router"
	insertTopoPeerUp(t, ctx, q, topoRouterEVPNCommunity, sysname, topoPeerEVPNCommunity, 65220)

	insertTopoEVPNRoute(t, ctx, q, routeEVPNFixture{
		RouterIP: topoRouterEVPNCommunity, RouterSysname: sysname, PeerIP: topoPeerEVPNCommunity,
		PeerASN: 65220, RouteType: 5, RD: "65094:210", Prefix: "10.94.202.0/24",
		ASPath: []uint32{65220, 65221}, LargeCommunities: []string{"65220:1:100"},
		Seq:         1,
		TsRouter:    topoAnchor.Add(1 * time.Second),
		TsCollector: topoAnchor.Add(1 * time.Second),
	})
	insertTopoEVPNRoute(t, ctx, q, routeEVPNFixture{
		RouterIP: topoRouterEVPNCommunity, RouterSysname: sysname, PeerIP: topoPeerEVPNCommunity,
		PeerASN: 65220, RouteType: 5, RD: "65094:211", Prefix: "10.94.202.128/25",
		ASPath:      []uint32{65220, 65222},
		Seq:         2,
		TsRouter:    topoAnchor.Add(2 * time.Second),
		TsCollector: topoAnchor.Add(2 * time.Second),
	})
}

// topoFilter is the scope every behavior test here asks through: one router,
// nothing else. It is a function rather than a literal at each call site so
// that a test naming a router cannot accidentally ask an unscoped question
// against a shared testDB.
func topoFilter(router string) RouteFilter {
	return RouteFilter{Router: netip.MustParseAddr(router), Peer: netip.MustParseAddr(topoPeerFor(router))}
}

// topoPeerFor is the one peer each single-peer fixture router advertises
// through. It exists because a topology filter with no content filter needs
// BOTH router= and peer= to be a legal scope (see checkTopology), so every
// per-router assertion in this file names the peer too.
func topoPeerFor(router string) string {
	switch router {
	case topoRouterWithdraw:
		return topoPeerWithdraw
	case topoRouterRoles:
		return topoPeerRoles
	case topoRouterOneHop:
		return topoPeerOneHop
	case topoRouterPrepend:
		return topoPeerPrepend
	case topoRouterLoop:
		return topoPeerLoop
	case topoRouterFirstSeen:
		return topoPeerFirstSeen
	case topoRouterRetention:
		return topoPeerRetention
	case topoRouterRewithdraw:
		return topoPeerRewithdraw
	}
	panic("topoPeerFor: no single peer for router " + router)
}

// edgeIn returns the graph's edge for one ASN pair, failing when it is
// absent. It fails rather than skips for dumpCountFor's own reason: every
// caller has just seeded that edge, so its absence is the query being wrong.
func edgeIn(t *testing.T, g Graph, src, dst uint32) ASEdge {
	t.Helper()
	for _, e := range g.Edges {
		if e.Src == src && e.Dst == dst {
			return e
		}
	}
	t.Fatalf("no edge %d->%d among %d edges: %+v", src, dst, len(g.Edges), g.Edges)
	return ASEdge{}
}

// edgeAbsent is edgeIn's opposite, and it is not the same assertion written
// backwards: several claims here are about an edge that must NOT be drawn --
// a prepending self-loop, another peer's adjacency -- and "we found no edge"
// is only meaningful beside a graph that has edges in it, which is why it
// prints the whole edge list on failure.
func edgeAbsent(t *testing.T, g Graph, src, dst uint32) {
	t.Helper()
	for _, e := range g.Edges {
		if e.Src == src && e.Dst == dst {
			t.Fatalf("edge %d->%d is in the graph and must not be: %+v\nedges: %+v",
				src, dst, e, g.Edges)
		}
	}
}

// nodeIn returns the graph's node for one ASN, failing when it is absent, for
// edgeIn's reason.
func nodeIn(t *testing.T, g Graph, asn uint32) ASNode {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ASN == asn {
			return n
		}
	}
	t.Fatalf("no node %d among %d nodes: %+v", asn, len(g.Nodes), g.Nodes)
	return ASNode{}
}

// TestTopologyUnicastKeepsWithdrawnEdges is the withdrawal-is-data
// constraint as a test. A withdrawal
// carries no path attributes, so a route whose newest row is a withdrawal has
// an EMPTY as_path on that row. Taking one argMax(as_path) over the group
// therefore reports the empty path and the edge vanishes -- silently, with a
// 200 and a smaller graph. Only argMaxIf over the advertisements sees it.
//
// The fixture is built so the withdrawn route's edge appears NOWHERE else:
// 65010 -> 65011 is carried by exactly one route, and that route is withdrawn.
// If the edge is reachable through any other route, this test passes for the
// wrong reason.
func TestTopologyUnicastKeepsWithdrawnEdges(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterWithdraw))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	e := edgeIn(t, g, 65010, 65011)
	if e.Routes != 1 {
		t.Errorf("edge 65010->65011 Routes = %d, want 1", e.Routes)
	}
	if e.LiveRoutes != 0 {
		t.Errorf("edge 65010->65011 LiveRoutes = %d, want 0 -- the only route "+
			"carrying it is withdrawn, which is what makes it renderable as "+
			"withdrawn rather than absent", e.LiveRoutes)
	}

	// The live edge beside it, so a statement that reported every edge as
	// withdrawn would not pass the assertion above.
	live := edgeIn(t, g, 65010, 65012)
	if live.Routes != 1 || live.LiveRoutes != 1 {
		t.Errorf("edge 65010->65012 = (%d routes, %d live), want (1, 1)",
			live.Routes, live.LiveRoutes)
	}

	// Both routes are in the population, withdrawn one included: Routes is
	// what the endpoint reports as meta.total_matched, and a graph built from
	// two routes that claimed one would misdescribe itself.
	if g.Routes != 2 {
		t.Errorf("Graph.Routes = %d, want 2", g.Routes)
	}
}

// TestTopologyUnicastCountsRoutesAndLiveRoutesSeparately pins the pair on an
// edge where they DIFFER and neither is 0. 65021 -> 65022 is carried by two
// routes, one live and one withdrawn, so an implementation that reported
// Routes for both, or LiveRoutes for both, is wrong in a way neither the
// all-live nor the all-withdrawn edge can show.
func TestTopologyUnicastCountsRoutesAndLiveRoutesSeparately(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterRoles))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	for _, want := range []ASEdge{
		{Src: 65020, Dst: 65021, Routes: 4, LiveRoutes: 3},
		{Src: 65021, Dst: 65022, Routes: 2, LiveRoutes: 1},
		{Src: 65021, Dst: 65023, Routes: 1, LiveRoutes: 1},
		{Src: 65020, Dst: 65025, Routes: 1, LiveRoutes: 1},
		{Src: 65025, Dst: 65022, Routes: 1, LiveRoutes: 1},
	} {
		got := edgeIn(t, g, want.Src, want.Dst)
		if got.Routes != want.Routes || got.LiveRoutes != want.LiveRoutes {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (%d, %d)",
				want.Src, want.Dst, got.Routes, got.LiveRoutes,
				want.Routes, want.LiveRoutes)
		}
	}
	if len(g.Edges) != 5 {
		t.Errorf("graph has %d edges, want exactly 5: %+v", len(g.Edges), g.Edges)
	}
	if g.Routes != 5 {
		t.Errorf("Graph.Routes = %d, want 5", g.Routes)
	}
}

// TestTopologyUnicastResolvesNodeRoles pins all three role flags against a
// fixture where the five ASNs have five DIFFERENT role combinations. A
// per-ASN table rather than three separate assertions is deliberate: the
// failure mode worth catching is a role expression that is constant -- every
// node transit, say -- and a test asserting one node's roles cannot see that.
func TestTopologyUnicastResolvesNodeRoles(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterRoles))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	for _, want := range []ASNode{
		// 65020 is the peer's own AS and is never last in a path.
		{ASN: 65020, Routes: 5, Origin: false, Transit: true, Peer: true},
		// 65021 is last in R4 and in the middle of R1-R3: both roles at once,
		// which is what stops the two from being modeled as one enum.
		{ASN: 65021, Routes: 4, Origin: true, Transit: true, Peer: false},
		{ASN: 65022, Routes: 3, Origin: true, Transit: false, Peer: false},
		{ASN: 65023, Routes: 1, Origin: true, Transit: false, Peer: false},
		// 65025 is a transit AS that is NOT the observing peer, which is the
		// case that separates Transit from Peer.
		{ASN: 65025, Routes: 1, Origin: false, Transit: true, Peer: false},
	} {
		got := nodeIn(t, g, want.ASN)
		if got.Routes != want.Routes || got.Origin != want.Origin ||
			got.Transit != want.Transit || got.Peer != want.Peer {
			t.Errorf("node %d = {Routes:%d Origin:%v Transit:%v Peer:%v}, "+
				"want {Routes:%d Origin:%v Transit:%v Peer:%v}",
				want.ASN, got.Routes, got.Origin, got.Transit, got.Peer,
				want.Routes, want.Origin, want.Transit, want.Peer)
		}
	}
	if len(g.Nodes) != 5 {
		t.Errorf("graph has %d nodes, want exactly 5: %+v", len(g.Nodes), g.Nodes)
	}
}

// TestTopologyUnicastFirstSeenIsTheEarliestObservation pins FirstSeen against
// the one case that separates it from three plausible wrong answers.
//
// 65021 -> 65022 is carried by R1 (live, first seen anchor+10s) and R3
// (withdrawn, first seen anchor+5s). The right answer is anchor+5s. A
// statement taking max reports anchor+10s; one restricted to live routes
// reports anchor+10s; one reading the withdrawal's own timestamp rather than
// the route's earliest observation reports anchor+25s.
func TestTopologyUnicastFirstSeenIsTheEarliestObservation(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterRoles))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	for _, tc := range []struct {
		src, dst uint32
		want     time.Time
	}{
		{65021, 65022, topoAnchor.Add(5 * time.Second)},
		{65020, 65021, topoAnchor.Add(5 * time.Second)},
		{65021, 65023, topoAnchor.Add(20 * time.Second)},
		{65025, 65022, topoAnchor.Add(40 * time.Second)},
	} {
		e := edgeIn(t, g, tc.src, tc.dst)
		if !e.FirstSeen.Equal(tc.want) {
			t.Errorf("edge %d->%d FirstSeen = %s, want %s",
				tc.src, tc.dst, e.FirstSeen.UTC(), tc.want)
		}
	}

	// A node's FirstSeen answers the same question about the routes that
	// traverse it. 65022 is on R1, R3 and R5; R3 is the earliest.
	if n := nodeIn(t, g, 65022); !n.FirstSeen.Equal(topoAnchor.Add(5 * time.Second)) {
		t.Errorf("node 65022 FirstSeen = %s, want %s",
			n.FirstSeen.UTC(), topoAnchor.Add(5*time.Second))
	}
}

// TestTopologyUnicastOneHopPathIsANodeWithNoEdge holds the claim that an
// origin-only graph is a real answer. range(1, 1) is empty, so a one-hop path
// contributes no edge at all -- and if nodes were derived from the edge list
// rather than from live_as_path, this router's graph would be empty and the
// screen would report that we hold nothing for it.
func TestTopologyUnicastOneHopPathIsANodeWithNoEdge(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterOneHop))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	if len(g.Edges) != 0 {
		t.Errorf("a one-hop path produced %d edges, want 0: %+v", len(g.Edges), g.Edges)
	}
	n := nodeIn(t, g, 65030)
	if n.Routes != 1 || !n.Origin || n.Transit || !n.Peer {
		t.Errorf("node 65030 = %+v, want Routes 1, Origin true, Transit false, Peer true", n)
	}
	if g.Routes != 1 {
		t.Errorf("Graph.Routes = %d, want 1", g.Routes)
	}
	// Nodes and Edges are non-nil even when empty, so the endpoint marshals
	// [] rather than null for a family whose graph has no edges. EVPN's real
	// graph is two edges; an empty one is an ordinary answer here.
	if g.Edges == nil {
		t.Error("Graph.Edges is nil; it must be an empty slice so the wire carries [] rather than null")
	}
}

// TestTopologyUnicastNarrowsByRouterAndPeer proves the peer= half of the
// documented scope actually narrows. The fixture's two
// peers share no ASN, so a peer predicate that did not render -- or rendered
// against the wrong column -- shows up as the other peer's edge appearing in
// this answer, not as a subtle count.
//
// It is also the case RouteFilter.predicates() cannot serve at all, which is
// why topologyPredicates exists: with no Prefix and no wide filter, that
// method renders an exact match on the EMPTY prefix, which route_unicast has
// never held, and this query returns nothing at all. See
// TestTopologyPredicatesOmitTheEmptyPrefixForAPeerScope.
func TestTopologyUnicastNarrowsByRouterAndPeer(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	router := netip.MustParseAddr(topoRouterScope)

	both, err := q.TopologyUnicast(ctx, RouteFilter{
		Router: router, Peer: netip.MustParseAddr(topoPeerScopeA)})
	if err != nil {
		t.Fatalf("TopologyUnicast(peer A): %v", err)
	}
	if e := edgeIn(t, both, 65040, 65041); e.Routes != 1 {
		t.Errorf("peer A's edge Routes = %d, want 1", e.Routes)
	}
	edgeAbsent(t, both, 65042, 65043)
	if len(both.Edges) != 1 {
		t.Errorf("peer A's graph has %d edges, want 1: %+v", len(both.Edges), both.Edges)
	}

	// The other peer's graph, asked the same way, so the assertion above
	// cannot be passing because peer B's rows are simply unreachable.
	b, err := q.TopologyUnicast(ctx, RouteFilter{
		Router: router, Peer: netip.MustParseAddr(topoPeerScopeB)})
	if err != nil {
		t.Fatalf("TopologyUnicast(peer B): %v", err)
	}
	if e := edgeIn(t, b, 65042, 65043); e.Routes != 1 {
		t.Errorf("peer B's edge Routes = %d, want 1", e.Routes)
	}
	edgeAbsent(t, b, 65040, 65041)
}

// TestTopologyUnicastIgnoresPathPrepending is the one behavior here that the
// live archive cannot exercise: zero of its 8,611 route_unicast rows carry a
// repeated ASN, so the whole class is invisible to any measurement taken
// against it -- and AS path prepending is one of the most common things a
// real network does to its announcements.
//
// Two claims, both about what prepending must NOT do:
//
//  1. No self-loop edge. A path of 65050 65051 65051 65052 has consecutive
//     pairs (65050,65051), (65051,65051), (65051,65052). The middle one is
//     not an AS adjacency -- no BGP speaker peers with itself -- it is the
//     prepend, and drawing it puts a loop on the canvas that describes
//     nothing.
//  2. Routes counts route identities, not path positions. A node's routes
//     are how many route identities traverse it; a plain
//     arrayJoin over the path counts 65051 twice for this single route.
func TestTopologyUnicastIgnoresPathPrepending(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterPrepend))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	edgeAbsent(t, g, 65051, 65051)
	for _, want := range []ASEdge{
		{Src: 65050, Dst: 65051, Routes: 1, LiveRoutes: 1},
		{Src: 65051, Dst: 65052, Routes: 1, LiveRoutes: 1},
	} {
		got := edgeIn(t, g, want.Src, want.Dst)
		if got.Routes != want.Routes || got.LiveRoutes != want.LiveRoutes {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (%d, %d)",
				want.Src, want.Dst, got.Routes, got.LiveRoutes, want.Routes, want.LiveRoutes)
		}
	}
	if len(g.Edges) != 2 {
		t.Errorf("graph has %d edges, want exactly 2: %+v", len(g.Edges), g.Edges)
	}

	if n := nodeIn(t, g, 65051); n.Routes != 1 {
		t.Errorf("node 65051 Routes = %d, want 1 -- one route traverses it, "+
			"twice, and Routes counts route identities", n.Routes)
	}
	if n := nodeIn(t, g, 65051); !n.Transit || n.Origin {
		t.Errorf("node 65051 = %+v, want Transit true, Origin false", n)
	}
}

// TestTopologyUnicastCountsALoopedAdjacencyOnce is the second half of the
// prepending correction, on the one shape the self-loop filter cannot reach.
//
// A path of 65060 65061 65060 65061 asserts (65060,65061) TWICE, and both
// halves of that pair are different ASNs, so no self-loop filter removes it.
// Routes is defined as route identities; one route asserting an adjacency
// twice is still one route.
func TestTopologyUnicastCountsALoopedAdjacencyOnce(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterLoop))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	if e := edgeIn(t, g, 65060, 65061); e.Routes != 1 {
		t.Errorf("edge 65060->65061 Routes = %d, want 1 -- one route asserts it "+
			"twice, and Routes counts route identities", e.Routes)
	}
	// The reverse adjacency is real and must survive: the correction removes
	// duplicates, not direction.
	if e := edgeIn(t, g, 65061, 65060); e.Routes != 1 {
		t.Errorf("edge 65061->65060 Routes = %d, want 1", e.Routes)
	}
	if len(g.Edges) != 2 {
		t.Errorf("graph has %d edges, want exactly 2: %+v", len(g.Edges), g.Edges)
	}
	if n := nodeIn(t, g, 65060); n.Routes != 1 {
		t.Errorf("node 65060 Routes = %d, want 1", n.Routes)
	}
}

// TestTopologyUnicastShowsOnlyTheCurrentSessionsUpPeers holds the property
// that is the reason the Topology tab is honest beside the Paths tab:
// this graph describes the same population /v1/routes shows, because it
// inherits the same two INNER JOINs.
//
// It asks about each of the three peers separately rather than about the
// router, so each answer is unambiguous -- and because a topology filter
// naming a router alone is not a scope at all (see checkTopology). An empty
// graph is asserted BESIDE a populated one from the same router, so "nothing
// came back" cannot be the fixture never having been written.
func TestTopologyUnicastShowsOnlyTheCurrentSessionsUpPeers(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	router := netip.MustParseAddr(topoRouterStale)

	up, err := q.TopologyUnicast(ctx, RouteFilter{
		Router: router, Peer: netip.MustParseAddr(topoPeerStaleUp)})
	if err != nil {
		t.Fatalf("TopologyUnicast(up peer): %v", err)
	}
	if e := edgeIn(t, up, 65070, 65071); e.Routes != 1 || e.LiveRoutes != 1 {
		t.Errorf("the up peer's edge = (%d routes, %d live), want (1, 1)",
			e.Routes, e.LiveRoutes)
	}

	for _, tc := range []struct {
		name, peer string
		why        string
	}{
		{"a down peer", topoPeerStaleDwn,
			"the peer_up gate is what drops it; its route rows are still in " +
				"route_unicast, because nothing deletes them when a peer goes down"},
		{"a superseded session", topoPeerStaleOld,
			"r.session_id = cur.sid is what drops it; BMP re-dumps the whole " +
				"table on a new session, so a route with no counterpart this " +
				"session is a claim about a session that no longer exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := q.TopologyUnicast(ctx, RouteFilter{
				Router: router, Peer: netip.MustParseAddr(tc.peer)})
			if err != nil {
				t.Fatalf("TopologyUnicast: %v", err)
			}
			if len(g.Edges) != 0 || len(g.Nodes) != 0 || g.Routes != 0 {
				t.Errorf("%s contributed %d edges, %d nodes and %d routes, want "+
					"none -- %s\nedges: %+v\nnodes: %+v",
					tc.name, len(g.Edges), len(g.Nodes), g.Routes, tc.why,
					g.Edges, g.Nodes)
			}
		})
	}
}

// TestTopologyUnicastWideFilterStillSeesAWithdrawnRoute is the test for the
// seam between two things that were each written for the other endpoint.
//
// topologySQL resolves live_as_path with argMaxIf over advertisements,
// so origin_asn= matches a route by its last ADVERTISED origin even after it
// is withdrawn -- intended, not an oversight. The candidate-key
// semi-join, which this statement reuses over topologyRowsCTE, restricts the answer to candidate keys
// from a subquery whose HAVING is a hard-coded `live_is_withdraw = 0` and
// whose live_as_path is a PLAIN argMax (liveAggregate). Read on its own that
// subquery would drop every withdrawn route before this statement ever saw
// it, deleting exactly the edges the screen exists to show.
//
// It does not, and the reason is worth pinning rather than trusting: the
// wide filters' raw-row candidates all test an attribute a withdrawal does
// not carry (`length(r.as_path) > 0`, `has(r.as_path, ?)`,
// `has(r.communities, ?)`), so a withdrawal row never survives into the
// subquery's aggregate, and its `live_is_withdraw = 0` is therefore vacuous
// for any key with a qualifying advertisement. That argument holds only as
// long as a withdrawal carries no path attributes. This test is what fails if
// it ever stops holding, or if the candidate for a future wide filter is
// written against a column a withdrawal does populate.
func TestTopologyUnicastWideFilterStillSeesAWithdrawnRoute(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	// 10.94.1.128/25 is withdrawn; its last advertisement originated in
	// 65011 and transited 65010.
	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"origin_asn", RouteFilter{
			Router: netip.MustParseAddr(topoRouterWithdraw), OriginASN: 65011}},
		{"through_asn", RouteFilter{
			Router: netip.MustParseAddr(topoRouterWithdraw), ThroughASN: 65011}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The semi-join must actually be in play, or this test is about
			// nothing: a wide filter with no candidate renders no fragment.
			if frag, _ := routesSemiJoinFrom(topologyRowsCTE, unicastRoutesKey,
				tc.f.topologyPredicates()); frag == "" {
				t.Fatal("no semi-join rendered for a wide filter, so this test " +
					"checks nothing about the seam it was written for")
			}
			g, err := q.TopologyUnicast(ctx, tc.f)
			if err != nil {
				t.Fatalf("TopologyUnicast: %v", err)
			}
			e := edgeIn(t, g, 65010, 65011)
			if e.Routes != 1 || e.LiveRoutes != 0 {
				t.Errorf("edge 65010->65011 = (%d routes, %d live), want (1, 0)",
					e.Routes, e.LiveRoutes)
			}
			// The live route's edge does NOT match this filter: 65012 is its
			// origin and 65011 is nowhere in its path. Its absence is what
			// proves the wide filter narrowed rather than being ignored.
			edgeAbsent(t, g, 65010, 65012)
		})
	}
}

// TestTopologyUnicastFiltersByCommunity pins the defect where
// GET /v1/topology?community= raised HTTP 500 -- ClickHouse refusing a
// HAVING clause that named live_communities and
// live_route_targets, columns topologySQL's `latest` CTE never defined --
// where origin_asn= and through_asn= (community's own siblings on the
// wideASN/community HAVING channel) both returned 200. See
// seedTopoCommunityRouter for the fixture this reads.
func TestTopologyUnicastFiltersByCommunity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, RouteFilter{
		Router: netip.MustParseAddr(topoRouterCommunity), Community: "65000:100"})
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}

	e := edgeIn(t, g, 65200, 65201)
	if e.Routes != 1 || e.LiveRoutes != 1 {
		t.Errorf("edge 65200->65201 = (%d routes, %d live), want (1, 1)",
			e.Routes, e.LiveRoutes)
	}

	// 65200 -> 65203's only route is withdrawn, and the predicate can only
	// still find it by reading the last ADVERTISED communities -- see
	// seedTopoCommunityRouter's own doc comment for what a plain argMax over
	// the group would report instead.
	w := edgeIn(t, g, 65200, 65203)
	if w.Routes != 1 || w.LiveRoutes != 0 {
		t.Errorf("edge 65200->65203 = (%d routes, %d live), want (1, 0)",
			w.Routes, w.LiveRoutes)
	}

	// 65200 -> 65202 carries no community at all; its absence is what proves
	// the filter narrowed rather than being ignored.
	edgeAbsent(t, g, 65200, 65202)

	if g.Routes != 2 {
		t.Errorf("Graph.Routes = %d, want 2", g.Routes)
	}
}

// TestTopologyVPNFiltersByCommunity is TestTopologyUnicastFiltersByCommunity
// on route_vpn, with the second case the unicast fixture cannot express:
// community= accepts a route target as well as a standard community (see
// query/filters.go's ParseCommunity), and route_vpn is where a route target
// is a real thing a route carries. See seedTopoVPNCommunityRouter.
func TestTopologyVPNFiltersByCommunity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)
	router := netip.MustParseAddr(topoRouterVPNCommunity)

	for _, tc := range []struct {
		name      string
		community string
		src, dst  uint32
	}{
		{"standard community", "65210:100", 65210, 65211},
		{"route target", "10.255.0.3:900", 65210, 65213},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := q.TopologyVPN(ctx, VPNRouteFilter{
				Router: router, Community: tc.community})
			if err != nil {
				t.Fatalf("TopologyVPN: %v", err)
			}
			e := edgeIn(t, g, tc.src, tc.dst)
			if e.Routes != 1 || e.LiveRoutes != 1 {
				t.Errorf("edge %d->%d = (%d routes, %d live), want (1, 1)",
					tc.src, tc.dst, e.Routes, e.LiveRoutes)
			}
			// Neither case matches the route carrying neither value.
			edgeAbsent(t, g, 65210, 65212)
			if g.Routes != 1 {
				t.Errorf("Graph.Routes = %d, want 1", g.Routes)
			}
		})
	}
}

// TestTopologyEVPNFiltersByCommunity is TestTopologyUnicastFiltersByCommunity
// on route_evpn, using a large community rather than a standard one so that
// this file's three community tests together exercise all four live_
// aggregates the community predicate can name (see seedTopoEVPNCommunityRouter).
func TestTopologyEVPNFiltersByCommunity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)
	router := netip.MustParseAddr(topoRouterEVPNCommunity)

	g, err := q.TopologyEVPN(ctx, EVPNRouteFilter{
		Router: router, Community: "65220:1:100"})
	if err != nil {
		t.Fatalf("TopologyEVPN: %v", err)
	}
	e := edgeIn(t, g, 65220, 65221)
	if e.Routes != 1 || e.LiveRoutes != 1 {
		t.Errorf("edge 65220->65221 = (%d routes, %d live), want (1, 1)",
			e.Routes, e.LiveRoutes)
	}
	edgeAbsent(t, g, 65220, 65222)
	if g.Routes != 1 {
		t.Errorf("Graph.Routes = %d, want 1", g.Routes)
	}
}

// TestTopologyPredicatesMatchRoutesWhereverBothCanAnswer is the price of
// topologyPredicates existing at all, and it is deliberately the widest test
// in this file rather than the narrowest.
//
// The two assemblies differ by exactly ONE rule -- the exact-prefix predicate
// is rendered only when Prefix is set -- so for every filter that names a
// prefix, an address to cover, or a wide filter, they must render character
// for character the same WHERE, the same HAVING and the same bound values. A
// divergence anywhere else means /v1/topology and /v1/routes would answer the
// same query string about two different populations, which is the property
// the whole endpoint's honesty rests on.
//
// It compares the bound VALUES as well as the text, because the text can
// agree while the argument order does not: filters appends one value per
// placeholder in the order the predicates are added, and a reordered
// assembly binds a router IP to a rib without changing a character of SQL.
func TestTopologyPredicatesMatchRoutesWhereverBothCanAnswer(t *testing.T) {
	router := netip.MustParseAddr("10.94.9.1")
	peer := netip.MustParseAddr("10.94.9.2")
	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"a prefix alone", RouteFilter{Prefix: "10.0.0.0/24"}},
		{"a prefix with every narrowing dimension", RouteFilter{
			Prefix: "10.0.0.0/24", Family: "ipv4u", Router: router,
			Peer: peer, RIB: "in_pre"}},
		{"covers alone", RouteFilter{Covers: "10.0.0.1"}},
		{"covers with a router and a peer", RouteFilter{
			Covers: "2001:db8::1", Router: router, Peer: peer}},
		{"a prefix beside an origin", RouteFilter{
			Prefix: "10.0.0.0/24", OriginASN: 65001}},
		{"a prefix beside a community", RouteFilter{
			Prefix: "10.0.0.0/24", Community: "65000:1"}},
		{"an origin alone", RouteFilter{OriginASN: 65001}},
		{"both ASN filters alone", RouteFilter{OriginASN: 65001, ThroughASN: 65002}},
		{"a community alone", RouteFilter{Community: "rt:65101:1"}},
		{"a wide filter with a router and a peer", RouteFilter{
			ThroughASN: 65002, Router: router, Peer: peer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := tc.f.predicates()
			topo := tc.f.topologyPredicates()
			if routes.where() != topo.where() {
				t.Errorf("WHERE differs:\n /v1/routes: %s\n topology:   %s",
					routes.where(), topo.where())
			}
			if routes.having() != topo.having() {
				t.Errorf("HAVING differs:\n /v1/routes: %s\n topology:   %s",
					routes.having(), topo.having())
			}
			if !reflect.DeepEqual(routes.values(), topo.values()) {
				t.Errorf("bound values differ:\n /v1/routes: %#v\n topology:   %#v",
					routes.values(), topo.values())
			}
			// The semi-join is built from the same filters object, so a
			// divergence in the candidates would not show in where()/having()
			// at all.
			rf, ra := routesSemiJoin(testDB, "route_unicast", unicastRoutesKey, routes)
			tf, ta := routesSemiJoin(testDB, "route_unicast", unicastRoutesKey, topo)
			if rf != tf {
				t.Errorf("semi-join fragment differs:\n /v1/routes: %s\n topology:   %s", rf, tf)
			}
			if !reflect.DeepEqual(ra, ta) {
				t.Errorf("semi-join args differ:\n /v1/routes: %#v\n topology:   %#v", ra, ta)
			}
		})
	}
}

// TestTopologyPredicatesOmitTheEmptyPrefixForAPeerScope pins the ONE place
// the two assemblies are allowed to disagree, from both sides.
//
// RouteFilter.predicates() renders `r.prefix = ?` bound to "" for a filter
// with no prefix, no covers and no wide filter, and that is correct for
// /v1/routes: it is what makes an unfiltered dump unreachable through the
// type, and route_unicast really does answer "the empty prefix" with nothing.
// /v1/topology's sufficient scopes include router= + peer=, which that
// rendering turns into a query for a prefix route_unicast has never held --
// zero rows, a 200, and an empty graph that reads like a network with no
// paths in it.
//
// Asserting the /v1/routes side too is deliberate. If predicates() is ever
// relaxed, this test fails and says so, rather than leaving topology's
// divergence quietly unnecessary.
func TestTopologyPredicatesOmitTheEmptyPrefixForAPeerScope(t *testing.T) {
	f := RouteFilter{
		Router: netip.MustParseAddr(topoRouterScope),
		Peer:   netip.MustParseAddr(topoPeerScopeA),
	}

	routes := f.predicates().where()
	if !strings.Contains(routes, "r.prefix = ?") {
		t.Errorf("RouteFilter.predicates() no longer renders the empty-prefix "+
			"predicate for a peer scope (%s). topologyPredicates exists only to "+
			"avoid it; if predicates() has been relaxed, topology should go back "+
			"to calling it", routes)
	}
	// The prefix is added first, so it is the FIRST value; assert it by
	// position rather than by searching the list, which could match a
	// legitimately empty rib or family.
	if vals := f.predicates().values(); len(vals) == 0 || vals[0] != "" {
		t.Errorf("RouteFilter.predicates() binds %#v; the empty prefix is "+
			"expected first", vals)
	}

	topo := f.topologyPredicates().where()
	if strings.Contains(topo, "r.prefix") {
		t.Errorf("topologyPredicates rendered a prefix predicate for a "+
			"router+peer scope (%s). That is the one rule it exists to change: "+
			"`r.prefix = ''` matches nothing in route_unicast, so the graph "+
			"would be empty for the \"from a peer\" mode", topo)
	}
	if want := " AND r.router_ip = toIPv6(?) AND r.peer_ip = toIPv6(?)"; topo != want {
		t.Errorf("topologyPredicates rendered %q, want %q", topo, want)
	}
}

// TestTopologyRefusesAFilterWithNoScope holds the rule that replaces the
// empty-prefix predicate as topology's guard against an unfiltered dump.
//
// RouteFilter needs no such rule because its prefix predicate is always
// rendered; dropping that predicate is what makes one necessary here. The
// refusal is in Go rather than in SQL for checkFamily's reason: an unscoped
// statement does not fail, it succeeds expensively and returns the whole
// fleet's graph to a caller who asked for nothing.
func TestTopologyRefusesAFilterWithNoScope(t *testing.T) {
	router := netip.MustParseAddr("10.94.9.1")
	peer := netip.MustParseAddr("10.94.9.2")

	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"nothing at all", RouteFilter{}},
		{"a router alone", RouteFilter{Router: router}},
		{"a peer alone", RouteFilter{Peer: peer}},
		{"narrowing dimensions only", RouteFilter{Family: "ipv4u", RIB: "in_pre"}},
		{"a limit is not a scope", RouteFilter{Limit: 100}},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			if err := tc.f.checkTopology(); !errors.Is(err, ErrBadFilter) {
				t.Errorf("checkTopology() = %v, want an ErrBadFilter", err)
			}
		})
	}

	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"a prefix", RouteFilter{Prefix: "10.0.0.0/24"}},
		{"covers", RouteFilter{Covers: "10.0.0.1"}},
		{"an origin", RouteFilter{OriginASN: 65001}},
		{"a transit AS", RouteFilter{ThroughASN: 65002}},
		{"a community", RouteFilter{Community: "65000:1"}},
		{"a router and a peer together", RouteFilter{Router: router, Peer: peer}},
	} {
		t.Run("accepted: "+tc.name, func(t *testing.T) {
			if err := tc.f.checkTopology(); err != nil {
				t.Errorf("checkTopology() = %v, want nil", err)
			}
		})
	}

	// checkTopology runs RouteFilter.check() first, so the value errors that
	// endpoint already reports are reported here too rather than being
	// swallowed by the scope rule.
	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"an unknown family", RouteFilter{Prefix: "10.0.0.0/24", Family: "vpn4"}},
		{"covers that is not an address", RouteFilter{Covers: "10.0.0.0/24"}},
		{"prefix and covers together", RouteFilter{
			Prefix: "10.0.0.0/24", Covers: "10.0.0.1"}},
	} {
		t.Run("still refused: "+tc.name, func(t *testing.T) {
			if err := tc.f.checkTopology(); !errors.Is(err, ErrBadFilter) {
				t.Errorf("checkTopology() = %v, want an ErrBadFilter", err)
			}
		})
	}
}

// TestTopologyUnicastRefusesAnUnscopedFilter is the rule above asserted
// through the exported function, so that a future TopologyUnicast that forgot
// to call checkTopology fails here rather than answering.
func TestTopologyUnicastRefusesAnUnscopedFilter(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	g, err := q.TopologyUnicast(ctx, RouteFilter{})
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("TopologyUnicast(RouteFilter{}) = (%+v, %v), want an ErrBadFilter", g, err)
	}
}

// TestTopologyStatementBindsEveryPlaceholder is the arity guard, and it is
// here for the reason TestRoutesSemiJoinBindsEveryPlaceholder is there:
// clickhouse-go silently DISCARDS a surplus argument rather than raising,
// which binds every later value one position early. Topology runs the same
// argument list against THREE statements, so a miscount here is three wrong
// answers rather than one -- and now against three FAMILIES, so it is nine.
//
// The VPN and EVPN cases are not decoration beside the unicast ones. They are
// the only thing here that would catch TopologyVPN passing w.values() where
// the statement wanted a semi-join's argument list, or TopologyEVPN inheriting
// evpnRoutesSQL's eor-join placeholder without the value that binds it.
func TestTopologyStatementBindsEveryPlaceholder(t *testing.T) {
	check := func(t *testing.T, cte string, args []any) {
		t.Helper()
		for _, tail := range []struct{ name, sql string }{
			{"edges", topologyEdgesSQL},
			{"nodes", topologyNodesSQL},
			{"routes", topologyRoutesSQL},
		} {
			if n := strings.Count(cte+tail.sql, "?"); n != len(args) {
				t.Errorf("%s statement has %d placeholders, %d values bound",
					tail.name, n, len(args))
			}
		}
	}

	router := netip.MustParseAddr("10.94.9.1")
	peer := netip.MustParseAddr("10.94.9.2")

	t.Run("unicast", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    RouteFilter
		}{
			{"a prefix", RouteFilter{Prefix: "10.0.0.0/24"}},
			{"a router and a peer", RouteFilter{Router: router, Peer: peer}},
			{"covers with a family", RouteFilter{Covers: "10.0.0.1", Family: "ipv4u"}},
			{"an origin alone", RouteFilter{OriginASN: 65001}},
			{"both ASN filters with a prefix", RouteFilter{
				Prefix: "10.0.0.0/24", OriginASN: 65001, ThroughASN: 65002}},
			{"a community and an origin", RouteFilter{
				Community: "rt:65101:1", OriginASN: 65001}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := tc.f.topologyPredicates()
				semi, args := routesSemiJoinFrom(topologyRowsCTE, unicastRoutesKey, w)
				check(t, topologyCTE(testDB, "route_unicast", unicastRoutesKey,
					w.where()+semi, w.having()), args)
			})
		}
	})

	t.Run("vpn", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    VPNRouteFilter
		}{
			{"a router alone", VPNRouteFilter{Router: router}},
			{"an rd with a peer and a rib", VPNRouteFilter{
				RD: "65001:1", Peer: peer, RIB: "in_pre"}},
			{"a prefix with every narrowing dimension", VPNRouteFilter{
				Prefix: "10.0.0.0/24", RD: "65001:1", Family: "vpn4",
				Router: router, Peer: peer, RIB: "in_pre"}},
			{"covers alone", VPNRouteFilter{Covers: "10.0.0.1"}},
			{"both ASN filters with a community", VPNRouteFilter{
				OriginASN: 65001, ThroughASN: 65002, Community: "rt:65101:1"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := tc.f.predicates()
				check(t, topologyCTE(testDB, "route_vpn", vpnRoutesKey,
					w.where(), w.having()), w.values())
			})
		}
	})

	t.Run("evpn", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    EVPNRouteFilter
		}{
			{"a router alone", EVPNRouteFilter{Router: router}},
			{"an rd with a route type", EVPNRouteFilter{RD: "65001:1", RouteType: 2}},
			{"a prefix with every narrowing dimension", EVPNRouteFilter{
				Prefix: "10.0.0.0/24", RD: "65001:1", RouteType: 5,
				Router: router, Peer: peer, RIB: "in_pre"}},
			{"covers alone", EVPNRouteFilter{Covers: "10.0.0.1"}},
			{"both ASN filters with a community", EVPNRouteFilter{
				OriginASN: 65001, ThroughASN: 65002, Community: "rt:65101:1"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				w := tc.f.predicates()
				check(t, topologyCTE(testDB, "route_evpn", evpnRoutesKey,
					w.where(), w.having()), w.values())
			})
		}
	})
}

// TestTopologyKeysMatchTheirFamilysStatement holds each family's topology
// identity against the /v1/routes statement it has to agree with.
//
// This is what keeps these counts right, asserted in the one place it can
// be asserted without a database: each family groups on
// ITS OWN route identity, and the definition of that identity already exists
// -- it is the GROUP BY of routesSQL, vpnRoutesSQL and evpnRoutesSQL. A
// topology key shorter than its statement's does not raise and does not
// empty the graph; it silently reports fewer routes on every edge, which is
// the failure mode this whole file is written against.
//
// The unicast row duplicates TestSemiJoinKeysMatchTheirStatements on purpose:
// unicastRoutesKey has two consumers now, and a test naming only one of them
// would leave the other free to drift.
func TestTopologyKeysMatchTheirFamilysStatement(t *testing.T) {
	for _, tc := range []struct {
		name, body, anchor string
		key                []string
	}{
		{"unicast", routesSQL, "\nFROM %[1]s.route_unicast_current r\n", unicastRoutesKey},
		{"vpn", vpnRoutesSQL, "\nFROM %[1]s.route_vpn_current r\n", vpnRoutesKey},
		{"evpn", evpnRoutesSQL, "\nFROM %[1]s.route_evpn_current r\n", evpnRoutesKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outerGroupBy(t, tc.body, tc.anchor); !slices.Equal(tc.key, got) {
				t.Errorf("the topology key is\n %v\nbut the family statement groups by\n %v",
					tc.key, got)
			}
			// ...and the rendered CTE really carries it, so a key that is
			// right and a render that ignores it cannot both pass.
			want := "GROUP BY " + strings.Join(tc.key, ", ")
			cte := topologyCTE(testDB, "route_"+tc.name, tc.key, "", "")
			if !strings.Contains(cte, want) {
				t.Errorf("the rendered %s CTE does not contain %q:\n%s", tc.name, want, cte)
			}
		})
	}
}

// TestTopologyVPNGroupsOnTheVPNIdentity seeds two VPN routes identical in
// every unicast-identity column and differing ONLY in rd, both carrying the
// same AS path. Grouped on the unicast identity they collapse into ONE route
// and the edge reports Routes = 1; grouped on the VPN identity they are two.
//
// The two routes share a path on purpose: the defect this catches is a
// COUNT being wrong, not an edge being missing, and an edge that is present
// either way is what makes the count the only thing under test.
func TestTopologyVPNGroupsOnTheVPNIdentity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyVPN(ctx, VPNRouteFilter{Router: netip.MustParseAddr(topoRouterVPN)})
	if err != nil {
		t.Fatalf("TopologyVPN: %v", err)
	}

	e := edgeIn(t, g, 65020, 65021)
	if e.Routes != 2 {
		t.Errorf("edge 65020->65021 Routes = %d, want 2 -- two routes differing "+
			"only in rd. A 1 here means the statement grouped on the unicast "+
			"identity and collapsed them", e.Routes)
	}
	if g.Routes != 2 {
		t.Errorf("Graph.Routes = %d, want 2", g.Routes)
	}
}

// TestTopologyEVPNGroupsOnTheEVPNIdentity is
// TestTopologyVPNGroupsOnTheVPNIdentity for the family whose identity shares
// only four columns with the other two.
//
// The fixture is nine EVPN routes differing from the first in exactly one
// identity column apiece -- route_type, rd, prefix, mac, ip, ethernet_tag, esi
// and path_id -- all carrying the same AS path. Nine is the answer only if
// every one of those eight columns discriminates; a statement grouping on any
// shorter key reports eight or fewer, and nothing about a smaller EVPN graph
// looks wrong on its own (the real archive's is two edges wide).
//
// It cannot be written as the VPN test's mutation is. Rendering route_evpn
// with the unicast identity does not miscount, it RAISES: route_evpn has no
// family column at all. So the mutation this is written against is a plausible
// key rather than a borrowed one -- the columns EVPN shares with unicast -- and
// that is what the per-column fixture makes visible.
func TestTopologyEVPNGroupsOnTheEVPNIdentity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyEVPN(ctx, EVPNRouteFilter{Router: netip.MustParseAddr(topoRouterEVPN)})
	if err != nil {
		t.Fatalf("TopologyEVPN: %v", err)
	}

	e := edgeIn(t, g, 65090, 65091)
	if e.Routes != 9 || e.LiveRoutes != 9 {
		t.Errorf("edge 65090->65091 = (%d routes, %d live), want (9, 9) -- nine "+
			"routes differing from each other in exactly one column of the EVPN "+
			"route identity. Anything under 9 means the statement grouped on a "+
			"key missing one of route_type, rd, prefix, mac, ip, ethernet_tag, "+
			"esi or path_id", e.Routes, e.LiveRoutes)
	}
	if g.Routes != 9 {
		t.Errorf("Graph.Routes = %d, want 9", g.Routes)
	}
	for _, asn := range []uint32{65090, 65091} {
		if n := nodeIn(t, g, asn); n.Routes != 9 {
			t.Errorf("node %d Routes = %d, want 9", asn, n.Routes)
		}
	}
}

// TestTopologyVPNIgnoresPathPrepending is
// TestTopologyUnicastIgnoresPathPrepending on route_vpn, and it is a separate
// test rather than a subtest of one because it is a separate fixture in a
// separate table.
//
// Both claims are the unicast test's, unchanged, and so is the reason they
// cannot be measured: zero of the archive's 6,907 route_vpn rows carry a
// repeated ASN anywhere in as_path (measured 2026-09-11), so a fixture had to
// construct one. What it adds is the guarantee that `routes` means route
// identities in EVERY member of a /v1/topology response -- one number with two
// meanings inside one JSON object would be invisible at the type level, since
// all three members are the same Graph.
func TestTopologyVPNIgnoresPathPrepending(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyVPN(ctx, VPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterVPNPrepend)})
	if err != nil {
		t.Fatalf("TopologyVPN: %v", err)
	}

	edgeAbsent(t, g, 65081, 65081)
	for _, want := range []ASEdge{
		{Src: 65080, Dst: 65081, Routes: 1, LiveRoutes: 1},
		{Src: 65081, Dst: 65082, Routes: 1, LiveRoutes: 1},
	} {
		got := edgeIn(t, g, want.Src, want.Dst)
		if got.Routes != want.Routes || got.LiveRoutes != want.LiveRoutes {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (%d, %d)",
				want.Src, want.Dst, got.Routes, got.LiveRoutes, want.Routes, want.LiveRoutes)
		}
	}
	if len(g.Edges) != 2 {
		t.Errorf("graph has %d edges, want exactly 2: %+v", len(g.Edges), g.Edges)
	}
	if n := nodeIn(t, g, 65081); n.Routes != 1 {
		t.Errorf("node 65081 Routes = %d, want 1 -- one route traverses it, "+
			"twice, and Routes counts route identities", n.Routes)
	}
	if n := nodeIn(t, g, 65081); !n.Transit || n.Origin {
		t.Errorf("node 65081 = %+v, want Transit true, Origin false", n)
	}
}

// TestTopologyEVPNIgnoresPathPrepending is the third copy, on route_evpn,
// whose 904 rows carry no repeated ASN either. See
// TestTopologyVPNIgnoresPathPrepending for why all three exist.
func TestTopologyEVPNIgnoresPathPrepending(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyEVPN(ctx, EVPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterEVPNPrepend)})
	if err != nil {
		t.Fatalf("TopologyEVPN: %v", err)
	}

	edgeAbsent(t, g, 65096, 65096)
	for _, want := range []ASEdge{
		{Src: 65095, Dst: 65096, Routes: 1, LiveRoutes: 1},
		{Src: 65096, Dst: 65097, Routes: 1, LiveRoutes: 1},
	} {
		got := edgeIn(t, g, want.Src, want.Dst)
		if got.Routes != want.Routes || got.LiveRoutes != want.LiveRoutes {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (%d, %d)",
				want.Src, want.Dst, got.Routes, got.LiveRoutes, want.Routes, want.LiveRoutes)
		}
	}
	if len(g.Edges) != 2 {
		t.Errorf("graph has %d edges, want exactly 2: %+v", len(g.Edges), g.Edges)
	}
	if n := nodeIn(t, g, 65096); n.Routes != 1 {
		t.Errorf("node 65096 Routes = %d, want 1", n.Routes)
	}
	if n := nodeIn(t, g, 65096); !n.Transit || n.Origin {
		t.Errorf("node 65096 = %+v, want Transit true, Origin false", n)
	}
}

// TestTopologyVPNAndEVPNNarrowByTheirOwnScope proves each new entry point
// really renders its filter, rather than answering about the whole table and
// happening to look right because each fixture router owns its ASNs.
//
// It asks each family for a router it has rows for and then for the OTHER
// family's router, which has none in this table: a predicate that did not
// render would return the seeded graph both times.
func TestTopologyVPNAndEVPNNarrowByTheirOwnScope(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	vpnPrepend, err := q.TopologyVPN(ctx, VPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterVPNPrepend)})
	if err != nil {
		t.Fatalf("TopologyVPN: %v", err)
	}
	edgeAbsent(t, vpnPrepend, 65020, 65021)
	if len(vpnPrepend.Edges) == 0 {
		t.Fatal("the VPN prepend router's graph is empty, so its missing edge " +
			"proves nothing about the scope")
	}

	// topoRouterEVPN has no route_vpn rows at all, and topoRouterVPN has no
	// route_evpn rows: each family must answer about its own table only.
	crossVPN, err := q.TopologyVPN(ctx, VPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterEVPN)})
	if err != nil {
		t.Fatalf("TopologyVPN(evpn router): %v", err)
	}
	if len(crossVPN.Edges) != 0 || len(crossVPN.Nodes) != 0 || crossVPN.Routes != 0 {
		t.Errorf("TopologyVPN for a router with no route_vpn rows = %+v, want an "+
			"empty graph", crossVPN)
	}
	crossEVPN, err := q.TopologyEVPN(ctx, EVPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterVPN)})
	if err != nil {
		t.Fatalf("TopologyEVPN(vpn router): %v", err)
	}
	if len(crossEVPN.Edges) != 0 || len(crossEVPN.Nodes) != 0 || crossEVPN.Routes != 0 {
		t.Errorf("TopologyEVPN for a router with no route_evpn rows = %+v, want an "+
			"empty graph", crossEVPN)
	}

	// An empty graph marshals as empty arrays, never null: the contract types
	// both as arrays and EVPN's real answer is routinely small.
	if crossVPN.Nodes == nil || crossVPN.Edges == nil ||
		crossEVPN.Nodes == nil || crossEVPN.Edges == nil {
		t.Error("an empty graph came back with a nil Nodes or Edges; see Graph")
	}
}

// TestTopologyVPNAndEVPNRefuseAnUnscopedFilter holds that each new entry point
// really calls its filter's check(), so the "return everything" query stays
// unreachable through these functions the way it is through VPNRoutes and
// EVPNRoutes.
//
// They reuse check() rather than a checkTopology of their own, which is the
// deliberate half of this: RouteFilter needed one because
// topologyPredicates drops its unconditional prefix predicate, and these two
// types never had that predicate to drop.
func TestTopologyVPNAndEVPNRefuseAnUnscopedFilter(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	if g, err := q.TopologyVPN(ctx, VPNRouteFilter{}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("TopologyVPN(VPNRouteFilter{}) = (%+v, %v), want an ErrBadFilter", g, err)
	}
	if g, err := q.TopologyEVPN(ctx, EVPNRouteFilter{}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("TopologyEVPN(EVPNRouteFilter{}) = (%+v, %v), want an ErrBadFilter", g, err)
	}
	// The value errors those types already report are reported here too,
	// rather than being swallowed on the way to the scope rule.
	if _, err := q.TopologyVPN(ctx, VPNRouteFilter{
		Prefix: "10.0.0.0/24", Covers: "10.0.0.1"}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("TopologyVPN with prefix and covers together = %v, want an ErrBadFilter", err)
	}
	if _, err := q.TopologyEVPN(ctx, EVPNRouteFilter{
		RD: "65001:1", RouteType: 12}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("TopologyEVPN with route type 12 = %v, want an ErrBadFilter", err)
	}
}

// TestTopologyVPNAndEVPNNeedNoPredicateDivergence pins the reason these two
// families reuse predicates() where unicast could not.
//
// topologyPredicates exists for ONE rule: RouteFilter.predicates renders
// `r.prefix = ?` bound to "" for a filter naming no prefix, no covers and no
// wide filter, which turns /v1/topology's router-scoped mode into a query for
// a prefix no table has ever held -- zero rows, a 200, and an empty graph that
// reads like a network with no paths in it.
//
// VPNRouteFilter and EVPNRouteFilter render that predicate with eqNonEmpty
// instead, because "" means "every prefix" on both types, so they already have
// topology's rule and a second copy of topologyPredicates for each would be
// two identical renderings kept in step by hand. This is the test that fails
// if either type is ever changed to render an unconditional prefix -- at which
// point these two functions need their own divergence, exactly as unicast did.
func TestTopologyVPNAndEVPNNeedNoPredicateDivergence(t *testing.T) {
	router := netip.MustParseAddr(topoRouterVPN)

	vpn := VPNRouteFilter{Router: router}.predicates().where()
	if strings.Contains(vpn, "r.prefix") {
		t.Errorf("VPNRouteFilter.predicates() rendered a prefix predicate for a "+
			"router-only scope (%s). TopologyVPN reuses this rendering precisely "+
			"because it does not; if that has changed, TopologyVPN needs its own "+
			"topologyPredicates the way TopologyUnicast does", vpn)
	}
	if want := " AND r.router_ip = toIPv6(?)"; vpn != want {
		t.Errorf("VPNRouteFilter.predicates() rendered %q, want %q", vpn, want)
	}

	evpn := EVPNRouteFilter{Router: router}.predicates().where()
	if strings.Contains(evpn, "r.prefix") {
		t.Errorf("EVPNRouteFilter.predicates() rendered a prefix predicate for a "+
			"router-only scope (%s); see the VPN half of this test", evpn)
	}
	if want := " AND r.router_ip = toIPv6(?)"; evpn != want {
		t.Errorf("EVPNRouteFilter.predicates() rendered %q, want %q", evpn, want)
	}
}

// TestTopologyVPNAndEVPNNarrowByAWideFilter is the seam test for the one
// clause these two families render that the arity guard can only count.
//
// origin_asn= and through_asn= are HAVING predicates over live_as_path (see
// filters.wideASN), and topologySQL splices them after its own
// `HAVING length(live_as_path) > 0` while their values are bound after the
// WHERE's. Nothing about that is visible in a rendered statement that
// type-checks: a having spliced into the wrong verb, or an argument list
// concatenated in the wrong order, gives a statement ClickHouse runs happily
// and an answer that is simply a different set of routes.
//
// Each family is asked twice against one fixture, once with a wide filter
// that matches every route it has and once with one that matches none, so
// neither answer can be the filter being ignored: ignoring it would return
// the full graph both times, and failing to bind it would return the empty
// one both times.
//
// The ASNs are chosen so origin and transit cannot be confused. On both
// fixtures the path is <peer AS> -> <origin AS>, so the peer's own AS is a
// through_asn match and NOT an origin_asn one.
func TestTopologyVPNAndEVPNNarrowByAWideFilter(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	vpnRouter := netip.MustParseAddr(topoRouterVPN)
	evpnRouter := netip.MustParseAddr(topoRouterEVPN)

	for _, tc := range []struct {
		name  string
		graph func() (Graph, error)
		want  uint64
	}{
		{"vpn origin_asn matches the path's last hop", func() (Graph, error) {
			return q.TopologyVPN(ctx, VPNRouteFilter{Router: vpnRouter, OriginASN: 65021})
		}, 2},
		{"vpn origin_asn does not match a transit AS", func() (Graph, error) {
			return q.TopologyVPN(ctx, VPNRouteFilter{Router: vpnRouter, OriginASN: 65020})
		}, 0},
		{"vpn through_asn matches a transit AS", func() (Graph, error) {
			return q.TopologyVPN(ctx, VPNRouteFilter{Router: vpnRouter, ThroughASN: 65020})
		}, 2},
		{"vpn through_asn does not match an absent AS", func() (Graph, error) {
			return q.TopologyVPN(ctx, VPNRouteFilter{Router: vpnRouter, ThroughASN: 65022})
		}, 0},
		{"evpn origin_asn matches the path's last hop", func() (Graph, error) {
			return q.TopologyEVPN(ctx, EVPNRouteFilter{Router: evpnRouter, OriginASN: 65091})
		}, 9},
		{"evpn origin_asn does not match a transit AS", func() (Graph, error) {
			return q.TopologyEVPN(ctx, EVPNRouteFilter{Router: evpnRouter, OriginASN: 65090})
		}, 0},
		{"evpn through_asn matches a transit AS", func() (Graph, error) {
			return q.TopologyEVPN(ctx, EVPNRouteFilter{Router: evpnRouter, ThroughASN: 65090})
		}, 9},
		{"evpn through_asn does not match an absent AS", func() (Graph, error) {
			return q.TopologyEVPN(ctx, EVPNRouteFilter{Router: evpnRouter, ThroughASN: 65092})
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := tc.graph()
			if err != nil {
				t.Fatalf("topology: %v", err)
			}
			if g.Routes != tc.want {
				t.Errorf("Graph.Routes = %d, want %d", g.Routes, tc.want)
			}
			if tc.want == 0 && (len(g.Edges) != 0 || len(g.Nodes) != 0) {
				t.Errorf("a filter matching no route returned %d nodes and %d edges: %+v %+v",
					len(g.Nodes), len(g.Edges), g.Nodes, g.Edges)
			}
			if tc.want > 0 && len(g.Edges) != 1 {
				t.Errorf("want the fixture's one edge, got %d: %+v", len(g.Edges), g.Edges)
			}
		})
	}
}

// TestTopologyVPNKeepsWithdrawnEdges is the withdrawal-is-data constraint
// as a test on route_vpn, the twin of TestTopologyUnicastKeepsWithdrawnEdges.
//
// It is a separate test and a separate fixture rather than an argument from
// the shared statement text, for the reason the three prepending tests are
// three: the sharing is what makes the claim true, and the claim stops being
// true the moment a future change splits the CTE per family to reach a column
// only one of them has. Withdrawal is drawn as data (live_routes per edge),
// not filtered out -- this endpoint's central constraint -- and proving it
// for unicast while arguing it for the other two leaves the one number the
// screen renders as "dashed" unverified in two thirds of the response.
//
// The fixture is built so the withdrawn route's edge appears NOWHERE else:
// 65100 -> 65101 is carried by exactly one route in the whole database, and
// that route's newest observation is a withdrawal, which carries no path
// attributes. A plain argMax(as_path) over the group therefore reports the
// empty path and the edge vanishes silently, with a 200 and a smaller graph.
func TestTopologyVPNKeepsWithdrawnEdges(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyVPN(ctx, VPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterVPNWithdraw)})
	if err != nil {
		t.Fatalf("TopologyVPN: %v", err)
	}

	e := edgeIn(t, g, 65100, 65101)
	if e.Routes != 1 {
		t.Errorf("edge 65100->65101 Routes = %d, want 1", e.Routes)
	}
	if e.LiveRoutes != 0 {
		t.Errorf("edge 65100->65101 LiveRoutes = %d, want 0 -- the only route "+
			"carrying it is withdrawn, which is what makes it renderable as "+
			"withdrawn rather than absent", e.LiveRoutes)
	}

	// The live edge beside it, so a statement that reported every edge as
	// withdrawn would not pass the assertion above.
	live := edgeIn(t, g, 65100, 65102)
	if live.Routes != 1 || live.LiveRoutes != 1 {
		t.Errorf("edge 65100->65102 = (%d routes, %d live), want (1, 1)",
			live.Routes, live.LiveRoutes)
	}

	// Two routes, not three: the one this collector has only ever seen
	// withdrawn has an empty live path and is excluded from the POPULATION by
	// `HAVING length(live_as_path) > 0`, not just from the drawing. A graph
	// built from two routes that claimed three would misdescribe itself, and
	// Routes is what /v1/topology reports as meta.total_matched.
	if g.Routes != 2 {
		t.Errorf("Graph.Routes = %d, want 2", g.Routes)
	}
	if len(g.Edges) != 2 {
		t.Errorf("graph has %d edges, want exactly 2: %+v", len(g.Edges), g.Edges)
	}
}

// TestTopologyEVPNKeepsWithdrawnEdges is the withdrawal-is-data constraint
// as a test on route_evpn. See TestTopologyVPNKeepsWithdrawnEdges for why
// all three families carry this fixture rather than one standing in for
// the others.
func TestTopologyEVPNKeepsWithdrawnEdges(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyEVPN(ctx, EVPNRouteFilter{
		Router: netip.MustParseAddr(topoRouterEVPNWithdraw)})
	if err != nil {
		t.Fatalf("TopologyEVPN: %v", err)
	}

	e := edgeIn(t, g, 65110, 65111)
	if e.Routes != 1 {
		t.Errorf("edge 65110->65111 Routes = %d, want 1", e.Routes)
	}
	if e.LiveRoutes != 0 {
		t.Errorf("edge 65110->65111 LiveRoutes = %d, want 0 -- the only route "+
			"carrying it is withdrawn", e.LiveRoutes)
	}

	live := edgeIn(t, g, 65110, 65112)
	if live.Routes != 1 || live.LiveRoutes != 1 {
		t.Errorf("edge 65110->65112 = (%d routes, %d live), want (1, 1)",
			live.Routes, live.LiveRoutes)
	}

	if g.Routes != 2 {
		t.Errorf("Graph.Routes = %d, want 2", g.Routes)
	}
	if len(g.Edges) != 2 {
		t.Errorf("graph has %d edges, want exactly 2: %+v", len(g.Edges), g.Edges)
	}
}

// The dual-collector topology fixture: ONE router two collectors monitor,
// carrying ONE route.
//
// Every other fixture here writes defaultFixtureCollector alone, so every
// `routes` assertion passes whether the count is route identities or
// (collector, route) pairs -- the one-branch fixture problem, and the
// reason a single-collector fixture set never caught this defect.
//
// 10.94.90.x is claimed by nothing else in this file.
const (
	topoRouterDual = "10.94.90.1"
	topoPeerDual   = "10.94.90.2"
	// topoCollectorDual sorts after defaultFixtureCollector ("query-test"),
	// and each collector runs its own BMP session: session_id is minted per
	// collector, and peerStateCTE resolves the current session per
	// (collector, router).
	topoCollectorDual = "query-test-topo-c2"
	topoSessionDualA  = 7001
	topoSessionDualB  = 7002
)

func seedTopoDualCollectorRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-dual-router"
	const prefix = "10.94.90.0/24"
	path := []uint32{65090, 65091}

	for _, c := range []struct {
		collector string
		session   uint64
		seq       uint64
	}{
		{defaultFixtureCollector, topoSessionDualA, 1},
		{topoCollectorDual, topoSessionDualB, 1},
	} {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: topoRouterDual, RouterSysname: sysname, PeerIP: topoPeerDual,
			RIB: "in_pre", Collector: c.collector, PeerASN: 65090, PeerBGPID: topoPeerDual,
			SessionID: c.session, Seq: c.seq, StreamSeq: 7000 + c.seq,
			Kind: "up", TsRouter: topoAnchor, TsCollector: topoAnchor,
		})
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: topoRouterDual, RouterSysname: sysname, PeerIP: topoPeerDual,
			RIB: "in_pre", Collector: c.collector, PeerASN: 65090, PeerBGPID: topoPeerDual,
			Family: "ipv4u", Prefix: prefix,
			SessionID: c.session, Seq: c.seq, StreamSeq: 7010 + c.seq,
			ASPath: path, TsRouter: topoAnchor, TsCollector: topoAnchor,
		})
	}
}

// TestTopologyCountsRouteIdentitiesNotCollectorCopies is the fix for a
// dual-collector route being counted twice, the one instance of that
// defect class that is server-side.
//
// topologyEdgesSQL's own doc comment defines the column: "`routes` is
// defined as route identities". The identity it counts with,
// unicastRoutesKey, LEADS WITH collector_id -- so one route two collectors
// both carry is two identities, and a graph node on a dual-homed router
// reports twice the routes it holds. The AS paths screen states "counted
// once per route identity" directly above the doubled number.
//
// collector_id must STAY in the inner grouping: `latest` resolves each
// route's live state with argMax over (seq, stream_seq), and seq is a
// per-collector counter, so a group spanning collectors would rank
// independent counters and pick a winner on nothing. That is the general
// pattern for reconciling per-collector state: inner grouping keyed by
// collector, outer count deduped across them.
func TestTopologyCountsRouteIdentitiesNotCollectorCopies(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, RouteFilter{Prefix: "10.94.90.0/24"})
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}
	if len(g.Nodes) != 2 || len(g.Edges) != 1 {
		t.Fatalf("got %d nodes / %d edges, want 2/1: %+v %+v",
			len(g.Nodes), len(g.Edges), g.Nodes, g.Edges)
	}
	for _, n := range g.Nodes {
		if n.Routes != 1 {
			t.Errorf("AS%d carries %d routes, want 1 -- one router advertised "+
				"one route and two collectors both wrote it down. 2 counts "+
				"observers as reachability, in a column its own doc defines "+
				"as route identities", n.ASN, n.Routes)
		}
	}
	e := g.Edges[0]
	if e.Routes != 1 || e.LiveRoutes != 1 {
		t.Errorf("the edge carries %d routes (%d live), want 1/1 -- the same "+
			"doubling, on the adjacency", e.Routes, e.LiveRoutes)
	}
	// The matched population, which the AS paths screen renders as "N
	// matched routes ... counted once per route identity" directly above
	// the graph. It is a third count off the same CTE and doubled the same
	// way; asserting the nodes and edges alone left it uncovered, found by
	// mutation.
	if g.Routes != 1 {
		t.Errorf("the scope matched %d routes, want 1 -- this is the number "+
			"the AS paths screen labels \"counted once per route identity\", "+
			"so 2 makes the screen contradict its own sentence", g.Routes)
	}
}

// The three routers below separate the two tables topology reads: the
// current table, which holds one row per route and no TTL, and the history
// table, which holds every observation for 90 days. 10.94.16.x through
// 10.94.18.x, and the prefixes 10.94.160.0/24 through 10.94.180.0/24, are
// claimed by nothing else in query/.
const (
	// topoRouterFirstSeen carries one route announced with one path and
	// re-announced with another. See TestTopologyFirstSeenIsTheFirstObservation.
	topoRouterFirstSeen = "10.94.16.1"
	topoPeerFirstSeen   = "10.94.16.2"
	// topoRouterRetention carries routes whose history rows are past the
	// history table's TTL and have been deleted. See
	// TestTopologyLiveEdgesOutliveRetention.
	topoRouterRetention = "10.94.17.1"
	topoPeerRetention   = "10.94.17.2"
	// topoRouterRewithdraw carries one route announced with two paths in
	// turn and then withdrawn. See TestTopologyKeepsRecentlyWithdrawnEdges.
	topoRouterRewithdraw = "10.94.18.1"
	topoPeerRewithdraw   = "10.94.18.2"
)

// topoExpiredAt stamps the retention fixture's route rows. Any instant more
// than 90 days old is past route_unicast's TTL; this one is a fixed month
// for isolation. The history table is partitioned by toYYYYMM(ts_collector)
// and seedTopoRetentionRouter forces the TTL with OPTIMIZE on that one
// partition, so the month must hold no other fixture's rows: OPTIMIZE ...
// FINAL also collapses a ReplacingMergeTree's duplicates, and several tests
// in this package write duplicates on purpose. Nothing else in query/ writes
// a route row dated 2020, and a date relative to time.Now() would one day
// land in the same month as a fixed-date fixture.
var topoExpiredAt = time.Date(2020, 1, 15, 12, 0, 0, 0, time.UTC)

const topoExpiredPartition = "202001"

// collapseTopoCurrent deletes one router's superseded rows from
// route_unicast_current, leaving the newest row per route: the state a
// ReplacingMergeTree merge reaches, reached now rather than whenever the
// background merge runs. Until that merge the current table still holds a
// route's older rows, and those carry the paths these fixtures exist to
// hide from it: a semi-join over the current table alone would pass
// TestTopologyKeepsRecentlyWithdrawnEdges before a merge and fail it after.
//
// It deletes only rows under the named router, which each fixture here owns
// outright, and it fails unless the table ends with one row per route.
func collapseTopoCurrent(t *testing.T, ctx context.Context, q *Q, router string) {
	t.Helper()
	tbl := q.db + ".route_unicast_current"
	if err := q.conn.Exec(ctx, "DELETE FROM "+tbl+
		" WHERE router_ip = toIPv6(?) AND (prefix, seq) NOT IN ("+
		"SELECT prefix, max(seq) FROM "+tbl+" WHERE router_ip = toIPv6(?) GROUP BY prefix)",
		router, router); err != nil {
		t.Fatalf("collapse %s's current rows: %v", router, err)
	}
	var rows, routes uint64
	if err := q.conn.QueryRow(ctx, "SELECT count(), uniqExact(prefix) FROM "+tbl+
		" WHERE router_ip = toIPv6(?)", router).Scan(&rows, &routes); err != nil {
		t.Fatalf("count %s's current rows: %v", router, err)
	}
	if rows != routes {
		t.Fatalf("%s has %d current rows for %d routes after the collapse, want one each",
			router, rows, routes)
	}
}

// seedTopoFirstSeenRouter writes one route observed twice in one session:
//
//	seq 1  anchor+1s  65160 65161
//	seq 2  anchor+2s  65160 65162
//
// The current table keeps only seq 2, whose timestamp is anchor+2s. The
// route was first seen at anchor+1s, and only history still knows that.
func seedTopoFirstSeenRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-first-seen-router"
	insertTopoPeerUp(t, ctx, q, topoRouterFirstSeen, sysname, topoPeerFirstSeen, 65160)
	for _, r := range []struct {
		path []uint32
		seq  uint64
	}{
		{[]uint32{65160, 65161}, 1},
		{[]uint32{65160, 65162}, 2},
	} {
		insertTopoRoute(t, ctx, q, topoRow{
			router: topoRouterFirstSeen, sysname: sysname, peer: topoPeerFirstSeen,
			peerASN: 65160, prefix: "10.94.160.0/24", path: r.path, seq: r.seq,
			ts: topoAnchor.Add(time.Duration(r.seq) * time.Second),
		})
	}
	collapseTopoCurrent(t, ctx, q, topoRouterFirstSeen)
}

// seedTopoRetentionRouter writes two routes at topoExpiredAt, then deletes
// their history rows the way retention does: OPTIMIZE ... FINAL applies the
// history table's TTL. The current table has no TTL, so its rows stay; they
// are collapsed to one per route first, so the withdrawn route's current
// row is the path-less withdrawal alone.
//
//	10.94.170.0/24  seq 1  announced   65170 65171
//	10.94.171.0/24  seq 2  announced   65170 65172
//	10.94.171.0/24  seq 3  withdrawn
//
// It checks both halves of that before any test relies on them: history
// holds no row for this router, and the current table still holds both
// routes.
func seedTopoRetentionRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-retention-router"
	insertTopoPeerUp(t, ctx, q, topoRouterRetention, sysname, topoPeerRetention, 65170)
	row := func(prefix string, path []uint32, seq uint64, withdraw bool) topoRow {
		return topoRow{
			router: topoRouterRetention, sysname: sysname, peer: topoPeerRetention,
			peerASN: 65170, prefix: prefix, path: path, seq: seq, withdraw: withdraw,
			ts: topoExpiredAt.Add(time.Duration(seq) * time.Second),
		}
	}
	insertTopoRoute(t, ctx, q, row("10.94.170.0/24", []uint32{65170, 65171}, 1, false))
	insertTopoRoute(t, ctx, q, row("10.94.171.0/24", []uint32{65170, 65172}, 2, false))
	insertTopoRoute(t, ctx, q, row("10.94.171.0/24", nil, 3, true))
	collapseTopoCurrent(t, ctx, q, topoRouterRetention)

	if err := q.conn.Exec(ctx, "OPTIMIZE TABLE "+q.db+".route_unicast PARTITION ID '"+
		topoExpiredPartition+"' FINAL"); err != nil {
		t.Fatalf("optimize the expired route_unicast partition: %v", err)
	}

	var history, current uint64
	if err := q.conn.QueryRow(ctx, "SELECT count() FROM "+q.db+
		".route_unicast WHERE router_ip = toIPv6(?)", topoRouterRetention).Scan(&history); err != nil {
		t.Fatalf("count the retention router's history rows: %v", err)
	}
	if err := q.conn.QueryRow(ctx, "SELECT uniqExact(prefix) FROM "+q.db+
		".route_unicast_current WHERE router_ip = toIPv6(?)", topoRouterRetention).Scan(&current); err != nil {
		t.Fatalf("count the retention router's current routes: %v", err)
	}
	if history != 0 || current != 2 {
		t.Fatalf("after OPTIMIZE ... FINAL the retention router has %d history rows "+
			"and %d current routes, want 0 and 2: the fixture does not describe "+
			"expired history, so no test built on it can say anything about retention",
			history, current)
	}
}

// seedTopoRewithdrawRouter writes one route announced with two paths in turn
// and then withdrawn, all within retention:
//
//	seq 1  anchor+1s  announced  65180 65181
//	seq 2  anchor+2s  announced  65180 65182
//	seq 3  anchor+3s  withdrawn
//
// The current table keeps only the withdrawal, which has no path. The edge
// the route last used is 65180 -> 65182, and only history holds it.
func seedTopoRewithdrawRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const sysname = "topo-rewithdraw-router"
	insertTopoPeerUp(t, ctx, q, topoRouterRewithdraw, sysname, topoPeerRewithdraw, 65180)
	for _, r := range []struct {
		path     []uint32
		seq      uint64
		withdraw bool
	}{
		{[]uint32{65180, 65181}, 1, false},
		{[]uint32{65180, 65182}, 2, false},
		{nil, 3, true},
	} {
		insertTopoRoute(t, ctx, q, topoRow{
			router: topoRouterRewithdraw, sysname: sysname, peer: topoPeerRewithdraw,
			peerASN: 65180, prefix: "10.94.180.0/24", path: r.path, seq: r.seq,
			withdraw: r.withdraw, ts: topoAnchor.Add(time.Duration(r.seq) * time.Second),
		})
	}
	collapseTopoCurrent(t, ctx, q, topoRouterRewithdraw)
}

// TestTopologyFirstSeenIsTheFirstObservation pins first_seen to the route's
// FIRST observation, which the current table does not hold: it keeps the
// seq 2 row, stamped anchor+2s. The edge the route carries now, 65160 ->
// 65162, reports anchor+1s, when the route was first seen with a different
// path. That is ASEdge.FirstSeen's documented meaning.
func TestTopologyFirstSeenIsTheFirstObservation(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	g, err := q.TopologyUnicast(ctx, topoFilter(topoRouterFirstSeen))
	if err != nil {
		t.Fatalf("TopologyUnicast: %v", err)
	}
	first := topoAnchor.Add(1 * time.Second)

	e := edgeIn(t, g, 65160, 65162)
	if !e.FirstSeen.Equal(first) {
		t.Errorf("edge 65160->65162 FirstSeen = %s, want %s: the route was first "+
			"seen at seq 1, and anchor+2s is the current row's own timestamp",
			e.FirstSeen.UTC(), first)
	}
	if e.Routes != 1 || e.LiveRoutes != 1 {
		t.Errorf("edge 65160->65162 = (%d routes, %d live), want (1, 1)", e.Routes, e.LiveRoutes)
	}
	if n := nodeIn(t, g, 65162); !n.FirstSeen.Equal(first) {
		t.Errorf("node 65162 FirstSeen = %s, want %s", n.FirstSeen.UTC(), first)
	}
	// The superseded path is not an edge: the route is live on its newer one.
	edgeAbsent(t, g, 65160, 65161)
}

// TestTopologyLiveEdgesOutliveRetention holds that a route whose history has
// expired is still drawn when it is still live. Its announcement is past
// route_unicast's 90-day TTL, but the route never changed, so the router
// still holds it and so does the current table.
//
// The origin_asn subtest is the candidate-key semi-join's seam: the semi-join
// decides which keys the outer statement aggregates, so a semi-join reading
// history alone would drop this route there even with the outer statement
// right.
//
// The withdrawn route beside it pins the documented retention bound from the
// other side: its last advertised path lived only in history, so once
// history expires it contributes neither an edge nor a route to the graph.
func TestTopologyLiveEdgesOutliveRetention(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"router and peer", topoFilter(topoRouterRetention)},
		{"origin_asn", RouteFilter{
			Router: netip.MustParseAddr(topoRouterRetention), OriginASN: 65171}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := q.TopologyUnicast(ctx, tc.f)
			if err != nil {
				t.Fatalf("TopologyUnicast: %v", err)
			}
			e := edgeIn(t, g, 65170, 65171)
			if e.Routes != 1 || e.LiveRoutes != 1 {
				t.Errorf("edge 65170->65171 = (%d routes, %d live), want (1, 1)",
					e.Routes, e.LiveRoutes)
			}
			// History is gone, so first_seen falls back to the current row's
			// own timestamp.
			if want := topoExpiredAt.Add(1 * time.Second); !e.FirstSeen.Equal(want) {
				t.Errorf("edge 65170->65171 FirstSeen = %s, want %s",
					e.FirstSeen.UTC(), want)
			}
			edgeAbsent(t, g, 65170, 65172)
			if g.Routes != 1 {
				t.Errorf("Graph.Routes = %d, want 1: the withdrawn route's path "+
					"expired with its history, so it draws nothing and is not "+
					"in the population", g.Routes)
			}
		})
	}
}

// TestTopologyKeepsRecentlyWithdrawnEdges holds that a route withdrawn within
// retention is drawn as a withdrawn edge on the path it was LAST advertised
// with. The current table holds only the withdrawal, which carries no path,
// so the path must come from history -- and it must be the newer of the two
// advertised paths, not the first.
//
// The origin_asn subtest is the semi-join's seam again, from the other side:
// a semi-join reading the current table alone sees only the path-less
// withdrawal and would drop this route before the outer statement ran.
func TestTopologyKeepsRecentlyWithdrawnEdges(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	seedTopologyFixtures(t, ctx, q)

	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"router and peer", topoFilter(topoRouterRewithdraw)},
		{"origin_asn", RouteFilter{
			Router: netip.MustParseAddr(topoRouterRewithdraw), OriginASN: 65182}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := q.TopologyUnicast(ctx, tc.f)
			if err != nil {
				t.Fatalf("TopologyUnicast: %v", err)
			}
			e := edgeIn(t, g, 65180, 65182)
			if e.Routes != 1 || e.LiveRoutes != 0 {
				t.Errorf("edge 65180->65182 = (%d routes, %d live), want (1, 0)",
					e.Routes, e.LiveRoutes)
			}
			if want := topoAnchor.Add(1 * time.Second); !e.FirstSeen.Equal(want) {
				t.Errorf("edge 65180->65182 FirstSeen = %s, want %s",
					e.FirstSeen.UTC(), want)
			}
			edgeAbsent(t, g, 65180, 65181)
			if g.Routes != 1 {
				t.Errorf("Graph.Routes = %d, want 1", g.Routes)
			}
		})
	}
}
