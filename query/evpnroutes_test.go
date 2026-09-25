package query

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
)

// mustEVPNRoutes calls EVPNRoutes and fails the test on error, so each
// subtest below reads as a claim about the result rather than about error
// handling. See mustRoutes in routes_test.go for the same convention.
func mustEVPNRoutes(t *testing.T, ctx context.Context, q *Q, f EVPNRouteFilter) []EVPNRoute {
	t.Helper()
	got, err := q.EVPNRoutes(ctx, f)
	if err != nil {
		t.Fatalf("EVPNRoutes(%+v): %v", f, err)
	}
	return got
}

func TestEVPNRoutes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertEVPNRouteFixture(t, ctx, q)

	t.Run("a type-2 MAC/IP route round-trips its MAC and IP", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDType2})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		if r.RouteType != 2 {
			t.Errorf("RouteType = %d, want 2", r.RouteType)
		}
		if r.MAC != evpnFixtureType2MAC || r.IP != evpnFixtureType2IP {
			t.Errorf("MAC/IP = %q/%q, want %q/%q -- a type-2 route's MAC and IP "+
				"are the route, not decoration on one",
				r.MAC, r.IP, evpnFixtureType2MAC, evpnFixtureType2IP)
		}
		if r.Prefix != "" {
			t.Errorf("Prefix = %q, want \"\" -- a type-2 NLRI carries no IP prefix", r.Prefix)
		}
		// The rest of the row, asserted here rather than in a subtest of its
		// own: every one of these fields is a separate SELECT position, and
		// a transposed pair in the scan is exactly the defect that survives
		// a test which only ever reads one column back.
		if r.EthernetTag != 10 {
			t.Errorf("EthernetTag = %d, want 10", r.EthernetTag)
		}
		if r.ESI != evpnFixtureESIZero {
			t.Errorf("ESI = %q, want %q", r.ESI, evpnFixtureESIZero)
		}
		if len(r.Labels) != 2 || r.Labels[0] != 10010 || r.Labels[1] != 50001 {
			t.Errorf("Labels = %v, want [10010 50001] -- RFC 7432 §7.2 lets a "+
				"type-2 route carry two, and the stack is reported as received", r.Labels)
		}
		if len(r.RouteTargets) != 1 || r.RouteTargets[0] != "65090:100" {
			t.Errorf("RouteTargets = %v, want [65090:100]", r.RouteTargets)
		}
		if r.NextHop.String() != "10.9.90.1" {
			t.Errorf("NextHop = %s, want 10.9.90.1", r.NextHop)
		}
		if r.OriginASN != 65090 {
			t.Errorf("OriginASN = %d, want 65090 (the AS path's last hop, not "+
				"its first)", r.OriginASN)
		}
		if r.RouterIP.String() != evpnFixtureRouterIP || r.PeerIP.String() != evpnFixtureUpPeerIP {
			t.Errorf("router/peer = %s/%s, want %s/%s -- an IPv4 address must "+
				"come back in plain form, never ::ffff:-mapped",
				r.RouterIP, r.PeerIP, evpnFixtureRouterIP, evpnFixtureUpPeerIP)
		}
	})

	// The IMET case, and the one most likely to be silently lost: on a real
	// captured archive every one of the 64 type-3 rows has an empty prefix,
	// MAC, IP and ESI, and an empty label stack. A query that treats any of
	// those as "no data here" drops a third of the table without erroring.
	t.Run("a type-3 IMET route with no MAC, IP or prefix is still a route", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDType3})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1 -- an IMET route carries no prefix, "+
				"MAC or IP, and must not be dropped for it: %+v", len(got), got)
		}
		r := got[0]
		if r.RouteType != 3 {
			t.Errorf("RouteType = %d, want 3", r.RouteType)
		}
		if r.MAC != "" || r.IP != "" || r.Prefix != "" || r.ESI != "" {
			t.Errorf("got MAC=%q IP=%q Prefix=%q ESI=%q; all four are empty on a "+
				"type-3 route and must be reported as empty, not filled in",
				r.MAC, r.IP, r.Prefix, r.ESI)
		}
		if len(r.Labels) != 0 {
			t.Errorf("Labels = %v, want empty", r.Labels)
		}
		if r.EthernetTag != 20 {
			t.Errorf("EthernetTag = %d, want 20 -- the ethernet tag and the RD "+
				"are the only things identifying this route", r.EthernetTag)
		}
	})

	t.Run("a type-5 route reports its prefix", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDType5})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		if r.RouteType != 5 || r.Prefix != "192.168.95.0/24" {
			t.Errorf("got RouteType=%d Prefix=%q, want 5 and 192.168.95.0/24",
				r.RouteType, r.Prefix)
		}
		if r.MAC != "" || r.IP != "" {
			t.Errorf("got MAC=%q IP=%q, want both empty on a type-5 route", r.MAC, r.IP)
		}
		if r.GatewayIP != "0.0.0.0" {
			t.Errorf("GatewayIP = %q, want %q -- it is the raw column, not a "+
				"parsed address, so the all-zeros gateway every type-5 row in "+
				"the archive carries survives as itself", r.GatewayIP, "0.0.0.0")
		}
	})

	// Fixture-only, and it has to be: the archive's 202 rows carry exactly
	// two ESI values -- all-zero on every type-2 and type-5 row, empty on
	// every type-3 -- and never two under one NLRI, so nothing there can
	// falsify a query that leaves esi out of the route key.
	t.Run("two routes differing only in ESI are two routes", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDESI})
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 -- an ESI is part of an EVPN route's "+
				"identity the same way a path-id is part of a unicast route's: %+v",
				len(got), got)
		}
		if got[0].ESI == got[1].ESI {
			t.Fatalf("both rows carry ESI %q; the fixture is not adversarial", got[0].ESI)
		}
		// ORDER BY carries esi, so the all-zero one sorts first. Without it
		// the pair has no tie-break at all and which row lands where is
		// unspecified.
		if got[0].ESI != evpnFixtureESIZero || got[1].ESI != evpnFixtureESIOther {
			t.Errorf("got ESIs %q, %q in that order, want %q, %q",
				got[0].ESI, got[1].ESI, evpnFixtureESIZero, evpnFixtureESIOther)
		}
		if got[0].NextHop == got[1].NextHop {
			t.Errorf("both rows carry next hop %s; the two are indistinguishable "+
				"in the answer even though the query kept them apart", got[0].NextHop)
		}
	})

	// The withdrawal exclusion, asserted so that it cannot pass on a query
	// that returns nothing at all: rd 65090:9 carries a live route beside
	// the withdrawn one, and the live one is what the non-zero half of this
	// claim rests on. Delete HAVING live_is_withdraw = 0 and this reads 2.
	t.Run("a withdrawn route is not returned, and its live sibling still is", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDWithdraw})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want exactly 1 -- the live sibling, not the "+
				"withdrawn route beside it: %+v", len(got), got)
		}
		if got[0].MAC != evpnFixtureLiveMAC {
			t.Errorf("MAC = %q, want %q -- the one route returned here must be "+
				"the live one", got[0].MAC, evpnFixtureLiveMAC)
		}
		for _, r := range got {
			if r.MAC == evpnFixtureWithdrawnMAC {
				t.Errorf("the withdrawn route came back as live: %+v", r)
			}
		}
	})

	// The down-peer exclusion, asserted the same way and for the same
	// reason: rd 65090:11 is advertised by an up peer AND by one that goes
	// down inside the current session. Both rows are still sitting in
	// route_evpn -- nothing deletes a route when its peer drops -- so
	// deleting the peer_up gate reads 2 here rather than 0.
	t.Run("a down peer contributes nothing, and the up peer still does", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDDownPeer})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want exactly 1 -- the up peer's copy, not "+
				"the down peer's: %+v", len(got), got)
		}
		if got[0].PeerIP.String() != evpnFixtureUpPeerIP {
			t.Errorf("PeerIP = %s, want %s", got[0].PeerIP, evpnFixtureUpPeerIP)
		}
		for _, r := range got {
			if r.PeerIP.String() == evpnFixtureDownPeerIP {
				t.Errorf("a down peer's route was reported as live: %+v", r)
			}
		}
	})

	// The session gate, asserted the same way the two exclusions above are:
	// rd 65090:17 carries one route from the session this router has since
	// replaced and one from the current session. Drop route_evpn's own
	// session_id from the cur join and this reads 2.
	t.Run("a session reset discards the prior dump, and the current one survives", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDSession})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want exactly 1 -- the current session's "+
				"route, not the dead session's beside it: %+v", len(got), got)
		}
		if got[0].Prefix != evpnFixtureLivePrefix {
			t.Errorf("Prefix = %q, want %q -- BMP re-dumps the whole table on a "+
				"new session, so an old-session row is a claim about a session "+
				"that no longer exists", got[0].Prefix, evpnFixtureLivePrefix)
		}
	})

	// DumpState's three readings, on three different rows of one fixture.
	// The first is also the guard on evpnRoutesSQL's `eor.fam = ?` join
	// predicate: evpnFixtureUpPeerIP's session carries a vpn4 marker and no
	// evpn one, so a join that ignored the family would report every route
	// below as "complete".
	t.Run("DumpState reads dumping while a marker for another family is on record", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDType2})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].DumpState != "dumping" {
			t.Errorf("DumpState = %q, want %q -- this peer's session carries a "+
				"vpn4 End-of-RIB and no evpn one, and one family's finished "+
				"dump says nothing about another's", got[0].DumpState, "dumping")
		}
	})

	t.Run("DumpState reads complete once the session carries this family's marker", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDDone})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].DumpState != "complete" {
			t.Errorf("DumpState = %q, want %q -- the session that advertised "+
				"this route also carries an evpn marker in eor_events",
				got[0].DumpState, "complete")
		}
	})

	// The route_evpn counterpart of
	// TestRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded: a
	// route under a rib peer_events has no accounting for must survive
	// (peer_up, not peer_state, decides that) and read "unknown", which is
	// the honest answer when there is no rib-scoped dump-progress data at
	// all. Turn the LEFT JOIN peer_state into an INNER one and this row
	// vanishes entirely rather than reading a wrong DumpState.
	// The claim EVPNRoute.Labels' own doc comment makes -- "the stack as
	// received", with nothing here stripping the RFC 3107 withdraw sentinel --
	// and, until rd 65090:19 existed, a claim no test could falsify. Every
	// sentinel-bearing row anywhere in this package and in a real captured
	// archive is also is_withdraw = 1, so HAVING live_is_withdraw = 0 removed
	// all of them before anything read a label: a future reader who added a
	// strip on 8388608, or reached for vpnroutes.go's label(), would have failed
	// nothing and shipped it. Note the two sentinels are not the same number
	// -- 8388608 raw here against the shifted 524288 route_vpn's MPLS labels
	// carry -- so reusing the VPN constant here strips nothing at all, which
	// is a third way to get this wrong and be told nothing.
	t.Run("a live route's label stack is reported as received, sentinel included", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDLiveSentinel})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1 -- this route is live (is_withdraw = 0) "+
				"and a sentinel in its label stack is not a reason to drop it: %+v",
				len(got), got)
		}
		if !slices.Equal(got[0].Labels, []uint32{evpnWithdrawSentinel}) {
			t.Errorf("Labels = %v, want [%d] -- api/openapi.yaml types this as "+
				"the stack as received, and query/ has no encapsulation "+
				"community to justify deciding otherwise",
				got[0].Labels, evpnWithdrawSentinel)
		}
	})

	t.Run("a route under a rib peer_events never recorded is still reported", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFixtureRDPostRIB})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1 -- nothing about this fixture says "+
				"the peer is down: %+v", len(got), got)
		}
		if got[0].RIB != "in_post" {
			t.Errorf("RIB = %q, want %q", got[0].RIB, "in_post")
		}
		if got[0].DumpState != "unknown" {
			t.Errorf("DumpState = %q, want %q -- peer_events has no in_post row "+
				"for this peer, so there is nothing to conclude",
				got[0].DumpState, "unknown")
		}
	})
}

// TestEVPNRoutesTreatsARedeliveredMarkerAsOneMarker is eorCTE's guard on the
// route surfaces: two markers and one marker have to be the same answer.
//
// eor_events is a plain MergeTree with no deduplication, so a JetStream
// redelivery leaves a second, permanent marker row for the same (router,
// peer, rib, session, family) -- insertPerFamilyDumpFixture writes exactly
// that pair, byte-identical, on purpose. eorCTE answers "did a marker
// arrive" with a constant marked = 1, never a tally, because anything able
// to tell two markers from one is reading a collection artifact as a fact
// about the network -- this project's most recurring defect, and one
// reintroduced here would report every redelivered dump as unfinished.
// Verified by mutation: turning that constant into count() makes this test
// read DumpState = "dumping" on both rows.
//
// What this test does NOT catch is worth recording rather than leaving to be
// rediscovered. Deleting eorCTE's own GROUP BY leaves the whole suite green,
// because all three route statements aggregate to their own route key
// afterwards and dumpStateExpr reaches the eor columns through any() -- so
// the duplicate joined row is absorbed before it can reach an answer. That
// GROUP BY is a second, independent guard rather than the load-bearing one
// today, and no result-based test can distinguish it; it is pinned by
// eorCTE's own doc comment and by this note. What IS load-bearing, and what
// this test holds, is that nothing counts.
//
// This peer is the right one for it: its evpn family has the duplicated
// marker AND the route rows to attach it to. Peers' own guard on the same
// fixture (max(is_marker), not count) cannot substitute -- peersSQL
// aggregates markers into a map, a shape where a doubled marker cannot
// change the value, while dumpStateExpr compares one.
func TestEVPNRoutesTreatsARedeliveredMarkerAsOneMarker(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPerFamilyDumpFixture(t, ctx, q)

	got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{
		Router: netip.MustParseAddr(perFamilyDumpFixtureRouterIP),
	})
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 -- this peer advertises exactly two EVPN "+
			"routes: %+v", len(got), got)
	}
	for _, r := range got {
		if r.DumpState != "complete" {
			t.Errorf("prefix %q: DumpState = %q, want %q -- this session carries "+
				"the SAME evpn End-of-RIB marker twice, as a redelivery leaves "+
				"it, and an expression that could tell two markers from one "+
				"reads a collection artifact as a fact about the network",
				r.Prefix, r.DumpState, "complete")
		}
	}
}

// TestEVPNRoutesFilters is the behavior half of EVPNRouteFilter: the six
// dimensions api/openapi.yaml documents on /v1/routes/evpn, asked of the real
// query rather than of the builder's rendered text.
//
// It exists as a counterpart to TestEVPNRouteFilterRendersOnlyWhatWasAsked
// rather than a duplicate of it, for the reason
// TestRoutesFiltersByRouterPeerAndRib gives: that test catches a predicate
// that stopped being emitted, this one catches a predicate emitted against
// the wrong column -- r.mac where r.ip was meant, say -- which renders
// perfectly and answers wrongly. See insertEVPNFilterFixture's own doc
// comment for why each filter here selects a differently-sized subset.
func TestEVPNRoutesFilters(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertEVPNFilterFixture(t, ctx, q)

	var (
		routerA = netip.MustParseAddr(evpnFilterRouterA)
		routerB = netip.MustParseAddr(evpnFilterRouterB)
		peerA1  = netip.MustParseAddr(evpnFilterPeerA1)
		shared  = netip.MustParseAddr(evpnFilterShared)
	)

	for _, tc := range []struct {
		name string
		f    EVPNRouteFilter
		want int
	}{
		// The two baselines first: without them every count below could be
		// explained by the fixture having written fewer rows than it meant to.
		{"the topology rd alone", EVPNRouteFilter{RD: evpnFilterRDTopology}, 6},
		{"router alone", EVPNRouteFilter{Router: routerA}, 6},

		{"the other router", EVPNRouteFilter{Router: routerB}, 2},
		{"peer", EVPNRouteFilter{RD: evpnFilterRDTopology, Peer: peerA1}, 2},
		{"a peer of both routers", EVPNRouteFilter{RD: evpnFilterRDTopology, Peer: shared}, 2},
		{"router and peer together", EVPNRouteFilter{
			RD: evpnFilterRDTopology, Router: routerA, Peer: shared,
		}, 1},
		{"rib", EVPNRouteFilter{RD: evpnFilterRDTopology, RIB: "loc_rib"}, 1},
		{"router and rib together", EVPNRouteFilter{Router: routerA, RIB: "in_pre"}, 5},

		// The second rd, and the two route types only it carries.
		{"the second rd", EVPNRouteFilter{RD: evpnFilterRDTypes}, 2},
		{"type 2", EVPNRouteFilter{Router: routerA, RouteType: 2}, 4},
		{"type 3", EVPNRouteFilter{Router: routerA, RouteType: 3}, 1},
		{"type 5", EVPNRouteFilter{Router: routerA, RouteType: 5}, 1},
		{"rd and type together", EVPNRouteFilter{RD: evpnFilterRDTypes, RouteType: 5}, 1},

		// prefix, exercised against the one route type in this fixture that
		// carries one at all.
		{"prefix", EVPNRouteFilter{Prefix: evpnFilterPrefix}, 1},
		{"prefix and router together", EVPNRouteFilter{Prefix: evpnFilterPrefix, Router: routerA}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustEVPNRoutes(t, ctx, q, tc.f)
			if len(got) != tc.want {
				t.Fatalf("got %d routes, want %d: %+v", len(got), tc.want, got)
			}
			// The count alone would pass for a filter that selected the
			// right NUMBER of wrong rows, so every row is checked against
			// every dimension the filter actually named.
			for _, r := range got {
				if tc.f.Prefix != "" && r.Prefix != tc.f.Prefix {
					t.Errorf("prefix %q in the result of a query filtered to %q", r.Prefix, tc.f.Prefix)
				}
				if tc.f.RD != "" && r.RD != tc.f.RD {
					t.Errorf("rd %q in the result of a query filtered to %q", r.RD, tc.f.RD)
				}
				if tc.f.RouteType != 0 && r.RouteType != tc.f.RouteType {
					t.Errorf("route type %d in the result of a query filtered to %d",
						r.RouteType, tc.f.RouteType)
				}
				if tc.f.Router.IsValid() && r.RouterIP != tc.f.Router {
					t.Errorf("router %s in the result of a query filtered to %s", r.RouterIP, tc.f.Router)
				}
				if tc.f.Peer.IsValid() && r.PeerIP != tc.f.Peer {
					t.Errorf("peer %s in the result of a query filtered to %s", r.PeerIP, tc.f.Peer)
				}
				if tc.f.RIB != "" && r.RIB != tc.f.RIB {
					t.Errorf("rib %q in the result of a query filtered to %q", r.RIB, tc.f.RIB)
				}
			}
		})
	}
}

// TestEVPNRoutesRequiresPrefixRDOrRouter pins api/openapi.yaml's own rule for
// /v1/routes/evpn: "At least one of prefix=, rd=, or router= is required (an
// unfiltered EVPN dump belongs to /v1/rib/evpn)."
//
// It is the same rule TestVPNRoutesRequiresPrefixRDOrRouter pins for the VPN
// surface, and the three that do NOT satisfy it are the interesting half
// here too: peer=, rib= and type= are all real filters that really do
// narrow, and every one of them still leaves a fleet-wide answer unbounded.
// ?type=2 alone is the sharpest example -- it is 72 of the archive's 202
// rows, and would be most of any real fabric's EVPN table.
func TestEVPNRoutesRequiresPrefixRDOrRouter(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertEVPNFilterFixture(t, ctx, q)
	// The covers fixture's EVPN rows are what make the "covers alone"
	// accepted case return something; without them it would assert only
	// that the filter was not refused.
	insertCoversFixture(t, ctx, q)

	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    EVPNRouteFilter
		}{
			{"nothing at all", EVPNRouteFilter{}},
			{"peer alone", EVPNRouteFilter{Peer: netip.MustParseAddr(evpnFilterPeerA1)}},
			{"rib alone", EVPNRouteFilter{RIB: "in_pre"}},
			{"type alone", EVPNRouteFilter{RouteType: 2}},
			{"peer, rib and type together", EVPNRouteFilter{
				Peer: netip.MustParseAddr(evpnFilterPeerA1), RIB: "in_pre", RouteType: 2,
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := q.EVPNRoutes(ctx, tc.f)
				if !errors.Is(err, ErrBadFilter) {
					t.Fatalf("EVPNRoutes(%+v) error = %v, want one wrapping ErrBadFilter", tc.f, err)
				}
				if got != nil {
					t.Errorf("EVPNRoutes returned %d rows alongside its error; a "+
						"refused filter must return no rows at all", len(got))
				}
			})
		}
	})

	// The other half: each of the three DOES satisfy the rule on its own, so
	// the check is a floor and not a second required-parameter rule.
	t.Run("accepted", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    EVPNRouteFilter
		}{
			{"prefix alone", EVPNRouteFilter{Prefix: evpnFilterPrefix}},
			{"rd alone", EVPNRouteFilter{RD: evpnFilterRDTopology}},
			{"router alone", EVPNRouteFilter{Router: netip.MustParseAddr(evpnFilterRouterA)}},
			// Covers is the fourth, for the reason VPNRouteFilter.Covers
			// gives: GET /v1/routes?covers=X fans out to this call, so a
			// rule that refused it would leave a documented endpoint
			// unanswerable.
			{"covers alone", EVPNRouteFilter{Covers: coversFixtureEVPNTarget}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := mustEVPNRoutes(t, ctx, q, tc.f); len(got) == 0 {
					t.Errorf("EVPNRoutes(%+v) returned nothing; the fixture is not "+
						"adversarial and this case proves nothing", tc.f)
				}
			})
		}
	})
}

// TestEVPNRoutesRejectsARouteTypeOutsideTheContract is the route_type
// counterpart of TestRoutesRejectsAFamilyRouteUnicastCannotHold, and it
// matters for the same reason: route_type is a bare UInt8 with no enum
// behind it (contrast rib, whose Enum8 makes ClickHouse itself raise -- see
// TestRoutesRejectsARibThatIsNotAnEnumMember), so an out-of-range value
// matches nothing rather than raising and returns an empty result nobody can
// tell from a correct one.
//
// 12 is the case worth naming, and the bound moved to reach it. IANA
// registers EVPN route types up to 11, and bgp/evpn.go sets RouteType
// unconditionally while carrying an undecoded type's bytes in Raw, so
// route_evpn genuinely can hold 6 through 11 and EVPNRoutes can RETURN one.
// A filter that refused those would be enforcing on input a bound the output
// cannot honor. 12 and above are registered by nobody, so refusing them is a
// bound the whole pipeline can keep. See evpnMaxRouteType.
func TestEVPNRoutesRejectsARouteTypeOutsideTheContract(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	for _, rt := range []uint8{12, 13, 255} {
		_, err := q.EVPNRoutes(ctx, EVPNRouteFilter{RD: evpnFilterRDTopology, RouteType: rt})
		if !errors.Is(err, ErrBadFilter) {
			t.Errorf("EVPNRoutes(RouteType=%d) error = %v, want one wrapping ErrBadFilter", rt, err)
		}
	}
	// And every type the contract does document is accepted, so the check is
	// a bound rather than a second way of refusing the query outright. Only
	// 2, 3 and 5 have a fixture or a live row behind them anywhere in this
	// repo -- the archive carries those three only -- so this loop is the
	// only thing asserting the other eight are legal at all, and it
	// deliberately asserts nothing about what they would return.
	for rt := uint8(1); rt <= evpnMaxRouteType; rt++ {
		if err := (EVPNRouteFilter{RD: evpnFilterRDTopology, RouteType: rt}).check(); err != nil {
			t.Errorf("check(RouteType=%d) = %v, want nil -- api/openapi.yaml "+
				"documents 1 through %d, IANA's own registry",
				rt, err, evpnMaxRouteType)
		}
	}
}

// TestEVPNRoutesStatementBindsEveryPlaceholder holds evpnRoutesStatement to
// this package's one binding invariant -- one `?` emitted, one value
// appended -- which matters more here than on the two statements whose
// placeholders all come from a single filter rendering.
//
// evpnRoutesSQL interleaves two sources: the eor join's own `?`, bound to
// evpnFamily, and whatever EVPNRouteFilter.predicates renders at %[2]s. The
// driver binds strictly left to right and clickhouse-go silently DISCARDS a
// surplus argument (it raises only on too few -- see peersStatement's doc
// comment for the version and the verification), so appending the family
// AFTER the filter's values would bind the filter's first value to the eor
// join and go unreported: every EVPN route in the fleet would read
// DumpState = "dumping" while an rd filter matched a family name.
func TestEVPNRoutesStatementBindsEveryPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    EVPNRouteFilter
		want int
	}{
		// One for the eor join in every case, plus one per set field.
		{"rd alone", EVPNRouteFilter{RD: "65090:2"}, 2},
		{"rd and type", EVPNRouteFilter{RD: "65090:2", RouteType: 5}, 3},
		{"everything", EVPNRouteFilter{
			Prefix: "192.168.95.0/24", RD: "65090:5", RouteType: 5,
			Router: netip.MustParseAddr("10.0.0.90"),
			Peer:   netip.MustParseAddr("10.0.0.91"),
			RIB:    "in_pre",
		}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := evpnRoutesStatement(testDB, tc.f)
			// evpnRoutesSQL carries no string literal containing a question
			// mark, so counting the character is counting placeholders. If
			// one is ever added, this test is where that assumption stops
			// holding.
			if n := strings.Count(sql, "?"); n != len(args) || n != tc.want {
				t.Fatalf("%d placeholders, %d values, want %d of each", n, len(args), tc.want)
			}
			if args[0] != evpnFamily {
				t.Errorf("args[0] = %v, want %q -- the eor join's placeholder "+
					"sits above the outer WHERE in the finished statement, so "+
					"the family binds first", args[0], evpnFamily)
			}
		})
	}
}

// TestEVPNFamilyComesFromTheSubjectsRegistry is the guard
// TestPeersEVPNFamilyLiteralMatchesSubjectsFamilyToken's doc comment
// describes, applied to the one place evpnRoutesSQL has to name a family
// that route_evpn has no column for.
//
// sink writes subjects.FamilyToken(f) into eor_events.family (tied back to
// the same source by sink's own TestFamilyNameMatchesSubjectsFamilyToken),
// and evpnRoutesSQL joins that column against evpnFamily. A token spelled
// here as a string would drift the moment the registry was renamed:
// production would write the new token while this join kept matching the
// old one, and every EVPN route in the fleet would read DumpState =
// "dumping" forever with no error anywhere.
//
// The assertion looks tautological -- evpnFamily is DEFINED as
// subjects.FamilyToken(bgp.FamilyEVPN) -- and it is not quite: it names the
// AFI/SAFI pair independently, so replacing the definition with a literal
// (or pointing it at the wrong family) fails here. The AFI/SAFI pair is what
// RFC 7432 fixes and what no rename can move, which is why it, rather than
// the token, is what this test spells out.
func TestEVPNFamilyComesFromTheSubjectsRegistry(t *testing.T) {
	if want := subjects.FamilyToken(bgp.Family{AFI: 25, SAFI: 70}); evpnFamily != want {
		t.Errorf("evpnFamily = %q, want %q -- sink writes that token into "+
			"eor_events.family, so a value that disagrees with it joins an "+
			"EVPN route to no marker at all. Derive it from the registry, do "+
			"not transcribe it", evpnFamily, want)
	}
	// And the same token peersSQL's own literal spells, since both meet on
	// eor_events.family: one of them drifting from the other puts a peer's
	// EVPN dump progress and its EVPN routes' DumpState under two different
	// answers for the same session.
	if !strings.Contains(peersSQL, "'"+evpnFamily+"' AS fam") {
		t.Errorf("peersSQL does not spell route_evpn's family as %q; peersSQL "+
			"and evpnRoutesSQL read the same eor_events.family and must agree",
			evpnFamily)
	}
}

// TestEVPNRoutesReportsRowsInItsStatementsOwnOrder is evpnRoutesOrder's
// counterpart to TestRoutesReportsRowsInItsStatementsOwnOrder, found the same
// way and added for the same reason -- see that test for the full account.
// Deleting evpnRoutesOrder from EVPNRoutes' statement left the suite green.
//
// vpnRoutesOrder needs no such test: TestVPNRoutes' add-path subtest indexes
// got[0] and got[1] and fails outright when that statement loses its ordering,
// which is exactly what vpnRoutesSQL's own doc comment says the ORDER BY is
// there for.
func TestEVPNRoutesReportsRowsInItsStatementsOwnOrder(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertEVPNFilterFixture(t, ctx, q)

	got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: evpnFilterRDTopology})
	if len(got) < 2 {
		t.Fatalf("got %d rows; this test needs at least two to say anything about "+
			"order", len(got))
	}
	key := func(r EVPNRoute) []string {
		return []string{
			r.RouterSysName, r.PeerIP.String(), fmt.Sprint(r.RouteType), r.RD, r.Prefix,
			r.MAC, r.IP, fmt.Sprint(r.EthernetTag), r.ESI, fmt.Sprint(r.PathID), r.Collector,
		}
	}
	for i := 1; i < len(got); i++ {
		if slices.Compare(key(got[i-1]), key(got[i])) > 0 {
			t.Errorf("rows %d and %d are out of order: %v then %v -- evpnRoutesOrder "+
				"sorts by sysname, peer_ip and then the whole NLRI key",
				i-1, i, key(got[i-1]), key(got[i]))
		}
	}
	if slices.Compare(key(got[0]), key(got[len(got)-1])) == 0 {
		t.Errorf("every row shares the ordering key %v; this fixture cannot tell an "+
			"ordered answer from an unordered one", key(got[0]))
	}
}

// TestEVPNRoutesReportsPathAttributes is the route_evpn half of the four
// RouteCommon attributes, and the one family where a real captured archive
// can say nothing at all: every one of its 202 route_evpn rows leaves med,
// local_pref, communities and large_communities empty. That is a fact about
// the EVPN deployments seen so far, not about the columns -- RFC 7432's own
// route targets ride in on the same attribute set, and this table already
// stores those -- so a fixture row is the only thing that can show
// EVPNRoutes reads the four at all rather than returning four zero values
// that happen to match every row it has ever been run against.
//
// There is no Family assertion here, and its absence is deliberate:
// route_evpn has no family column, api/openapi.yaml leaves family out of
// EVPNRoute.required, and a field this package filled in from evpnFamily
// would be asserting a fact rather than reporting one.
func TestEVPNRoutesReportsPathAttributes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPathAttrFixture(t, ctx, q)

	got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRD})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	r := got[0]

	wantMED, wantLocalPref := uint32(pathAttrMED), uint32(pathAttrLocalPref)
	if !sameNullableU32(r.MED, &wantMED) || !sameNullableU32(r.LocalPref, &wantLocalPref) {
		t.Errorf("got MED=%s LocalPref=%s, want %d and %d -- two adjacent "+
			"Nullable(UInt32) columns in a twenty-four-column positional scan, "+
			"so a swap between them raises nothing",
			nullableU32(r.MED), nullableU32(r.LocalPref), pathAttrMED, pathAttrLocalPref)
	}
	if !slices.Equal(r.Communities, pathAttrCommunityText) {
		t.Errorf("Communities = %v, want %v", r.Communities, pathAttrCommunityText)
	}
	if !slices.Equal(r.LargeCommunities, pathAttrLargeCommunities) {
		t.Errorf("LargeCommunities = %v, want %v", r.LargeCommunities, pathAttrLargeCommunities)
	}
	// labels, route_targets, communities and large_communities are four array
	// columns in a row now. Two of them are Array(UInt32) and two
	// Array(String), so the only transpositions the driver would refuse are
	// the cross-typed ones -- these assertions are what catch the rest.
	if !slices.Equal(r.Labels, []uint32{120002}) {
		t.Errorf("Labels = %v, want [120002]", r.Labels)
	}
	if !slices.Equal(r.RouteTargets, []string{"65120:100"}) {
		t.Errorf("RouteTargets = %v, want [65120:100]", r.RouteTargets)
	}
	if !slices.Equal(r.ASPath, pathAttrASPath) || r.OriginASN != pathAttrASPath[len(pathAttrASPath)-1] {
		t.Errorf("got ASPath=%v OriginASN=%d, want %v and %d",
			r.ASPath, r.OriginASN, pathAttrASPath, pathAttrASPath[len(pathAttrASPath)-1])
	}
	if r.RouteType != 2 || r.MAC != pathAttrEVPNMAC || r.RD != pathAttrEVPNRD {
		t.Errorf("got RouteType=%d MAC=%q RD=%q, want 2, %q, %q",
			r.RouteType, r.MAC, r.RD, pathAttrEVPNMAC, pathAttrEVPNRD)
	}

	// The nullable aggregation, pinned on this statement rather than borrowed
	// from another's: argMax(tuple(...), ...).1 is written out once per
	// statement, and a copy reverted to a plain argMax here is a copy no
	// unicast or VPN test can see.
	t.Run("a cleared MED reads as absent, not as the value it used to have", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRDCleared})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].MED != nil || got[0].LocalPref != nil {
			t.Errorf("got MED=%s LocalPref=%s, want both absent -- ClickHouse's "+
				"aggregates skip NULL, so a plain argMax reports %d here, which "+
				"is what the observation BEFORE the newest one carried",
				nullableU32(got[0].MED), nullableU32(got[0].LocalPref), pathAttrClearedMED)
		}
	})
}

// TestEVPNRoutesFiltersByWideParameters is TestVPNRoutesFiltersByWideParameters
// for route_evpn -- made a test after finding nothing before it set OriginASN,
// ThroughASN or Community on an EVPNRouteFilter and ran a live query. This
// statement is the one with the trickiest bind order in the package --
// evpnFamily is prepended ahead of the filter's own values (see EVPNRoutes'
// own comment on why), so a wide filter whose HAVING args landed before the
// WHERE args, or before evpnFamily, would bind evpnFamily's placeholder to
// the wrong value and every row would read as though its dump were still in
// progress. Only a live query proves the order actually holds.
//
// It reuses insertPathAttrFixture's own EVPN row: RD pathAttrEVPNRD carries
// the same AS path, route target and standard community
// TestVPNRoutesFiltersByWideParameters exercises on the VPN row beside it,
// and pathAttrEVPNRDCleared is its community-less neighbor.
func TestEVPNRoutesFiltersByWideParameters(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPathAttrFixture(t, ctx, q)

	origin, transit := pathAttrASPath[len(pathAttrASPath)-1], pathAttrASPath[0]

	t.Run("origin_asn matches the last hop", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRD, OriginASN: origin})
		if len(got) != 1 {
			t.Fatalf("OriginASN=%d against rd=%s got %d rows, want 1",
				origin, pathAttrEVPNRD, len(got))
		}
	})

	t.Run("origin_asn does not match a transit hop", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRD, OriginASN: transit})
		if len(got) != 0 {
			t.Errorf("OriginASN=%d matched %d rows; %d is a transit AS on this "+
				"path, not the origin", transit, len(got), transit)
		}
	})

	t.Run("through_asn matches the transit hop", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRD, ThroughASN: transit})
		if len(got) != 1 {
			t.Fatalf("ThroughASN=%d against rd=%s got %d rows, want 1",
				transit, pathAttrEVPNRD, len(got))
		}
	})

	t.Run("community matches the packed standard community", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRD, Community: "65000:100"})
		if len(got) != 1 {
			t.Fatalf("community=65000:100 against rd=%s got %d rows, want 1",
				pathAttrEVPNRD, len(got))
		}
	})

	t.Run("community matches the route target", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{RD: pathAttrEVPNRD, Community: "65120:100"})
		if len(got) != 1 {
			t.Fatalf("community=65120:100 against rd=%s got %d rows, want 1",
				pathAttrEVPNRD, len(got))
		}
	})

	t.Run("community does not match a row with no communities at all", func(t *testing.T) {
		got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{
			RD: pathAttrEVPNRDCleared, Community: "65000:100",
		})
		if len(got) != 0 {
			t.Errorf("community=65000:100 matched %d rows under rd=%s, which "+
				"carries no communities in this fixture", len(got), pathAttrEVPNRDCleared)
		}
	})
}

// TestEVPNRoutesCovers is TestVPNRoutesCovers for route_evpn: see it for why
// ?covers= has to reach every family the /v1/routes fan-out answers with,
// and what the unfiltered EVPN arm returned before it did.
//
// The EVPN-specific half is the prefix-less route types. 136 of the
// archive's 202 route_evpn rows carry no prefix at all -- every type 2 and
// type 3 -- and the empty string contains no address, so those rows are
// absent from a containment answer rather than matched by it. That is
// asserted here against a router whose fixture really does hold them.
func TestEVPNRoutesCovers(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertCoversFixture(t, ctx, q)
	insertEVPNRouteFixture(t, ctx, q)
	router := netip.MustParseAddr(coversFixtureRouterIP)

	want := []string{coversFixtureEVPNSlash16, coversFixtureEVPNSlash24}
	slices.Sort(want)

	got := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{
		Covers: coversFixtureEVPNTarget, Router: router,
	})
	prefixes := make([]string, 0, len(got))
	for _, r := range got {
		prefixes = append(prefixes, r.Prefix)
	}
	slices.Sort(prefixes)
	if !slices.Equal(prefixes, want) {
		t.Fatalf("EVPNRoutes(Covers=%s, Router=%s) = %v, want %v -- every prefix "+
			"that contains the address at every length, and nothing else (the "+
			"sibling %s shares the /16 and does not contain it)",
			coversFixtureEVPNTarget, coversFixtureRouterIP, prefixes, want,
			coversFixtureEVPNSibling)
	}

	// The prefix-less types, on the fixture router that carries them. The
	// premise is asserted first: without a type-2 and a type-3 row in the
	// table for this router, "no prefix-less row came back" is a claim about
	// an empty table.
	evpnRouter := netip.MustParseAddr(evpnFixtureRouterIP)
	all := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{Router: evpnRouter})
	var prefixless int
	for _, r := range all {
		if r.Prefix == "" {
			prefixless++
		}
	}
	if prefixless == 0 {
		t.Fatalf("the EVPN fixture router %s carries no prefix-less row; the "+
			"assertion below would be about an empty set", evpnFixtureRouterIP)
	}
	covering := mustEVPNRoutes(t, ctx, q, EVPNRouteFilter{
		Covers: "192.168.95.33", Router: evpnRouter,
	})
	if len(covering) == 0 {
		t.Fatalf("nothing covers 192.168.95.33 on %s, though its type-5 route "+
			"is 192.168.95.0/24 -- an empty answer makes the exclusion below "+
			"unfalsifiable", evpnFixtureRouterIP)
	}
	for _, r := range covering {
		if r.Prefix == "" {
			t.Errorf("a prefix-less type-%d route came back from a containment "+
				"query: the empty prefix contains no address, so it is not a "+
				"covering route for anything", r.RouteType)
		}
	}
}

// TestCountEVPNRoutesAgreesWithEVPNRoutes is CountRoutes' own regression (see
// its own comment in routes_test.go), applied to EVPNRoutes: the count has
// to come from the same statement EVPNRoutes runs -- evpnFamily bound ahead
// of the filter's own values included, since a count that dropped or
// misordered that value would silently count against the wrong family's
// eor_events marker rather than merely against the wrong predicate.
//
// It reuses TestEVPNRoutesFilters' own "the topology rd alone" case, whose 6
// rows already establish the fixture's baseline there.
func TestCountEVPNRoutesAgreesWithEVPNRoutes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertEVPNFilterFixture(t, ctx, q)

	f := EVPNRouteFilter{RD: evpnFilterRDTopology}
	rows, err := q.EVPNRoutes(ctx, f)
	if err != nil {
		t.Fatalf("EVPNRoutes: %v", err)
	}
	n, err := q.CountEVPNRoutes(ctx, f)
	if err != nil {
		t.Fatalf("CountEVPNRoutes: %v", err)
	}
	if n != uint64(len(rows)) {
		t.Errorf("CountEVPNRoutes = %d but EVPNRoutes returned %d rows", n, len(rows))
	}
	if n == 0 {
		t.Fatal("the fixture matched nothing, so this comparison is vacuous")
	}
}
