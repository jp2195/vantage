package query

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
)

// mustVPNRoutes calls VPNRoutes and fails the test on error, so each
// subtest below reads as a claim about the result rather than about error
// handling. See mustRoutes in routes_test.go for the same convention, and
// for why it takes the whole filter rather than a prefix alone.
func mustVPNRoutes(t *testing.T, ctx context.Context, q *Q, f VPNRouteFilter) []VPNRoute {
	t.Helper()
	got, err := q.VPNRoutes(ctx, f)
	if err != nil {
		t.Fatalf("VPNRoutes(%+v): %v", f, err)
	}
	return got
}

func TestVPNRoutes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertVPNRouteFixture(t, ctx, q)

	// NOTE: this case is fixture-only. Verified 2026-08-24 against the live
	// archive: no prefix appears under more than one RD, so route_vpn cannot
	// falsify this query. Same shape as elsewhere in this package -- the
	// fixture is the only thing testing it, so it has to actually bite.
	t.Run("one prefix in two VRFs stays two routes", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.1.0/24"})
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 -- the same prefix under two RDs is "+
				"two routes, not a duplicate: %+v", len(got), got)
		}
		if got[0].RD == got[1].RD {
			t.Errorf("both rows carry RD %q; the fixture is not adversarial", got[0].RD)
		}
	})

	// A withdrawn VPN route is not a route VPNRoutes should hand back at
	// all -- the same "where is this prefix right now" question routesSQL
	// answers for route_unicast, applied here. Reporting the withdrawn row,
	// on the theory that Label/HasLabel already handles the one artifact a
	// withdrawal leaves behind, would let a withdrawn route through with a
	// plausible-looking NextHop, RD and RouteTargets sitting right next to
	// its (correctly) absent label -- reporting a route that is not there
	// as though it still were.
	t.Run("a withdrawn VPN route is not returned at all", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.9.0/24"})
		if len(got) != 0 {
			t.Fatalf("got %d rows, want 0 -- a withdrawn route is not a "+
				"live one: %+v", len(got), got)
		}
	})

	// The withdraw sentinel's stripping in label() is defense in depth,
	// not the primary guard now that withdrawn rows are excluded outright
	// (see the subtest above): on the live archive, every row carrying
	// 524288 is also a withdrawal, so this exercises the one shape that
	// can still reach HasLabel -- a route that is NOT withdrawn but
	// carries the sentinel value anyway. looking-glass-vpn rendered this
	// value beside real labels until it was stripped on 2026-08-23; HasLabel
	// is how a caller tells "no label" from "label 0".
	t.Run("a live route whose labels carry the withdraw sentinel still reports no label", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.11.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].HasLabel {
			t.Errorf("HasLabel = true for a route carrying the withdraw "+
				"sentinel: %+v", got[0])
		}
		if got[0].Label == 524288 {
			t.Error("Label = 524288: the withdraw sentinel is being reported as an MPLS label")
		}
	})

	t.Run("a route with no route target is reported, not dropped", func(t *testing.T) {
		// 108 of the archive's 164 route_vpn rows carry no route target: all
		// 74 lu4 rows, plus 34 of the 90 vpn4 rows. An inner join to an RT
		// table would silently drop two thirds of the table, not a fifth.
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.7.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1 -- a missing route target must not "+
				"remove the route from the answer", len(got))
		}
		if len(got[0].RouteTargets) != 0 {
			t.Errorf("RouteTargets = %v, want empty", got[0].RouteTargets)
		}
	})

	t.Run("origin AS is absent, not zero, when there is no AS path", func(t *testing.T) {
		// 142 of 164 live VPN rows carry no AS path, and as_path[-1] on an
		// empty array yields the reserved AS 0 -- which l3vpn-rib-browser
		// rendered as a real origin until it was fixed. Do not repeat it.
		// Reuses 192.168.7.0/24, the same row the no-route-target subtest
		// above reads: that row is realistic precisely because it has
		// neither a route target nor an AS path, the two most common gaps
		// in the archive.
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.7.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].OriginASN != 0 || len(got[0].ASPath) != 0 {
			t.Fatalf("got OriginASN=%d ASPath=%v; a route with no AS path "+
				"must report an empty path, and callers must be able to tell "+
				"that from AS 0", got[0].OriginASN, got[0].ASPath)
		}
	})

	t.Run("origin AS is the last hop of the AS path, not the first", func(t *testing.T) {
		// The counterpart to the empty-path subtest above: 192.168.6.0/24
		// carries a three-hop path (65010, 65020, 65099), so this would
		// catch both "OriginASN is always 0" and "OriginASN is the first
		// hop", not just "OriginASN ignores the path entirely".
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.6.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].OriginASN != 65099 {
			t.Errorf("OriginASN = %d, want 65099 (the AS path's last hop, "+
				"not its first)", got[0].OriginASN)
		}
	})

	// This subtest exercises the RD-less family "explicitly" because it is
	// the family the alias-shadowing bug made invisible the first time
	// (see vpnRoutesSQL's doc comment).
	// It now reads 192.168.5.0/24 (live) rather than 192.168.9.0/24
	// (withdrawn, and so no longer returned at all -- see the withdrawal
	// subtest above): the RD-less family still needs a row VPNRoutes
	// actually returns for this assertion to mean anything.
	t.Run("an RD-less route reports the real column, not a placeholder", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.5.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].RD != "" {
			t.Errorf("RD = %q, want \"\" -- lu4 carries no route distinguisher "+
				"at all, and this must not come back as a rendered fallback "+
				"like l3vpn-rib-browser.json's own \"(none: ...)\" string",
				got[0].RD)
		}
	})

	// The archive has zero route_vpn rows with a genuinely empty labels
	// array -- this fixture is the only thing that can test this at all
	// (see insertVPNRouteFixture's own comment on 192.168.12.0/24).
	// argMax(r.labels[1], ...) -- a plain argMax over labels[1] --
	// returns 0 for an empty array, indistinguishable from a route
	// genuinely carrying the implicit-null label. HasLabel exists
	// specifically to remove that ambiguity; this subtest is what proves
	// it actually does.
	t.Run("a route with no labels reports no label, not label 0", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.12.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].HasLabel {
			t.Errorf("HasLabel = true for a route whose labels array was "+
				"empty: %+v", got[0])
		}
		if got[0].Label != 0 {
			t.Errorf("Label = %d, want 0", got[0].Label)
		}
	})

	// 192.168.13.0/24 is advertised twice under the same RD but two
	// different path-ids, neither ever withdrawn. A query that groups
	// without path_id in the route key collapses this to one row via
	// argMax; one that groups correctly but omits rib/path_id from the
	// SELECT list returns two rows
	// that agree on every field it exposes -- indistinguishable from the
	// first failure by inspection of the result alone. This subtest checks
	// both: the row count, and that the two rows are actually
	// distinguishable via PathID.
	t.Run("add-path keeps both path-ids distinguishable", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.13.0/24"})
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 -- add-path siblings are distinct "+
				"routes: %+v", len(got), got)
		}
		if got[0].PathID == got[1].PathID {
			t.Fatalf("both rows carry path-id %d; the fixture is not "+
				"adversarial", got[0].PathID)
		}
		// ORDER BY now ties off on r.path_id (see vpnRoutesSQL's own doc
		// comment), so the fixture's own path-id 1 is expected in got[0].
		if got[0].PathID != 1 || got[1].PathID != 2 {
			t.Errorf("got path-ids %d, %d in that order, want 1, 2", got[0].PathID, got[1].PathID)
		}
		if got[0].NextHop == got[1].NextHop {
			t.Errorf("both rows carry next hop %s; the fixture is not adversarial", got[0].NextHop)
		}
	})

	// The down-peer exclusion, asserted so that it cannot pass on a query
	// that returns nothing at all: 192.168.15.0/24 is advertised by an up
	// peer AND by one that goes down inside the current session, so this is
	// a non-zero count before the exclusion and a specific one after.
	//
	// It is here because it was NOT here: deleting vpnRoutesSQL's own
	// `peer_up.state = 'up'` left the whole package suite green, since every
	// VPN fixture in this file wrote up peers only. TestRoutes has covered
	// this for route_unicast and TestEVPNRoutes covers it for
	// route_evpn; route_vpn had the predicate and no test of it.
	t.Run("a down peer contributes nothing, and the up peer still does", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: vpnRouteFixtureDownPeerPrefix})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want exactly 1 -- the up peer's copy, not "+
				"the down peer's: %+v", len(got), got)
		}
		if got[0].PeerIP.String() != vpnRouteFixtureUpPeerIP {
			t.Errorf("PeerIP = %s, want %s", got[0].PeerIP, vpnRouteFixtureUpPeerIP)
		}
		for _, r := range got {
			if r.PeerIP.String() == vpnRouteFixtureDownPeerIP {
				t.Errorf("a down peer's VPN route was reported as live: %+v", r)
			}
		}
	})

	// DumpState's two per-route readings, on two peers of one router. It is
	// on VPNRoute at all because api/openapi.yaml puts dump_state in
	// RouteCommon.required and VPNRoute is an allOf over it -- and because
	// an empty VPN result and a half-delivered one are otherwise the same
	// answer. vpnRouteFixtureUpPeerIP carries nine route_vpn rows and no
	// marker at all, so its dump is genuinely still in progress.
	t.Run("DumpState reads dumping while the session's dump has no marker", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.6.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].DumpState != "dumping" {
			t.Errorf("DumpState = %q, want %q -- this peer's session carries "+
				"route_vpn rows and no End-of-RIB marker for any family",
				got[0].DumpState, "dumping")
		}
	})

	t.Run("DumpState reads complete once the session's dump has an end-of-rib marker", func(t *testing.T) {
		// The subtest above on its own leaves the field looking like it
		// could be pinned to one constant. vpnRouteFixtureDonePeerIP's
		// session carries a vpn4 marker in eor_events, so its one route
		// must read "complete" instead.
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: "192.168.14.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].DumpState != "complete" {
			t.Errorf("DumpState = %q, want %q -- the session that advertised "+
				"this route also carries an eor_events marker for its family",
				got[0].DumpState, "complete")
		}
	})
}

// TestVPNRoutesDumpStateIsPerFamily is the guard on vpnRoutesSQL's
// `eor.fam = r.family` join predicate, and it needs a fixture the VPN
// fixture cannot supply: a peer whose session carries a marker for one
// family and route rows for another.
//
// insertPerFamilyDumpFixture is exactly that peer. Its evpn family has both
// routes and a marker; its vpn4 family has a route and NO marker. So the
// vpn4 route below must read "dumping", and a join that matched any marker
// for the (router, peer, rib, session) would read "complete" -- one family's
// finished dump declaring another family's, which is precisely the question
// eor_events was split out of route_unicast to make answerable.
//
// route_vpn is the table where this matters most: it holds three families at
// once (vpn4, vpn6, lu4), so the wrong reading is available within one table
// rather than only across tables.
func TestVPNRoutesDumpStateIsPerFamily(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPerFamilyDumpFixture(t, ctx, q)

	got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{
		Router: netip.MustParseAddr(perFamilyDumpFixtureRouterIP),
	})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	if got[0].DumpState != "dumping" {
		t.Errorf("DumpState = %q, want %q -- this peer's session carries an "+
			"evpn End-of-RIB and no vpn4 one, and a route's dump progress is "+
			"its OWN family's", got[0].DumpState, "dumping")
	}
}

// TestVPNRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded is
// TestRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded applied to
// route_vpn, and it became reachable on this table only when VPNRoute grew
// DumpState: vpnRoutesSQL had no peer_state join at all before that, so
// there was no rib-scoped join for a route to fall through.
//
// Adding one was the whole hazard. peer_state is keyed on (collector,
// router, peer, rib), and an INNER join to it -- the obvious way to reach
// the column dumpStateExpr reads -- deletes every route observed under a rib
// peer_events has no accounting for, however plainly up the peer is. See
// insertPostPolicyRouteFixture's own doc comment for the live-archive
// occurrence, and peerStateCTE's for why peer_up is what decides survival.
//
// The route must come back, reading DumpState = "unknown": not a guess
// either way, but the honest answer when there is no rib-scoped
// dump-progress data to consult at all.
func TestVPNRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPostPolicyRouteFixture(t, ctx, q)

	got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Prefix: postPolicyRouteFixtureVPNPrefix})
	if len(got) != 1 {
		t.Fatalf("got %d routes, want 1 -- a VPN route under a rib peer_events "+
			"never recorded for this peer must still be reported, since nothing "+
			"about this fixture says the peer is down: %+v", len(got), got)
	}
	if got[0].RIB != "in_post" {
		t.Errorf("RIB = %q, want %q", got[0].RIB, "in_post")
	}
	if got[0].DumpState != "unknown" {
		t.Errorf("DumpState = %q, want %q -- no rib-scoped peer_events data "+
			"exists for this route's rib at all", got[0].DumpState, "unknown")
	}
}

// TestVPNRoutesFiltersByRouterPeerAndRib is TestRoutesFiltersByRouterPeerAndRib
// applied to route_vpn, against the same fixture topology (see
// insertRouteFilterFixture, which writes one route_vpn row for every
// route_unicast row it writes).
//
// It is a separate test rather than a subtest of the unicast one because the
// two statements are separate SQL rendered by separate builders:
// VPNRouteFilter.predicates emits its own predicates, and vpnRoutesSQL
// splices them into its own WHERE, aliases its own FROM, and groups by a
// route key that includes rd. A filter working for Routes is not evidence it
// works for VPNRoutes -- the two have already diverged twice, on which peer
// CTE gates route survival and on whether an empty Prefix is a value or an
// omission (see vpnRoutesSQL's and VPNRouteFilter's own doc comments).
func TestVPNRoutesFiltersByRouterPeerAndRib(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFilterFixture(t, ctx, q)

	var (
		routerA    = netip.MustParseAddr(routeFilterFixtureRouterA)
		routerB    = netip.MustParseAddr(routeFilterFixtureRouterB)
		peerA1     = netip.MustParseAddr(routeFilterFixturePeerA1)
		sharedPeer = netip.MustParseAddr(routeFilterFixtureSharedPeer)
	)

	for _, tc := range []struct {
		name string
		f    VPNRouteFilter
		want int
	}{
		{"prefix alone", VPNRouteFilter{Prefix: routeFilterFixtureVPNPrefix}, 6},
		{"router", VPNRouteFilter{Prefix: routeFilterFixtureVPNPrefix, Router: routerA}, 4},
		{"the other router", VPNRouteFilter{Prefix: routeFilterFixtureVPNPrefix, Router: routerB}, 2},
		{"peer", VPNRouteFilter{Prefix: routeFilterFixtureVPNPrefix, Peer: peerA1}, 2},
		{"a peer of both routers", VPNRouteFilter{Prefix: routeFilterFixtureVPNPrefix, Peer: sharedPeer}, 2},
		{"router and peer together", VPNRouteFilter{
			Prefix: routeFilterFixtureVPNPrefix, Router: routerA, Peer: sharedPeer,
		}, 1},
		{"rib", VPNRouteFilter{Prefix: routeFilterFixtureVPNPrefix, RIB: "loc_rib"}, 1},
		{"router and rib together", VPNRouteFilter{
			Prefix: routeFilterFixtureVPNPrefix, Router: routerA, RIB: "in_pre",
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustVPNRoutes(t, ctx, q, tc.f)
			if len(got) != tc.want {
				t.Fatalf("got %d routes, want %d: %+v", len(got), tc.want, got)
			}
			for _, r := range got {
				if tc.f.Router.IsValid() && r.RouterIP != tc.f.Router {
					t.Errorf("router %s in the result of a query filtered to %s", r.RouterIP, tc.f.Router)
				}
				if tc.f.Peer.IsValid() && r.PeerIP != tc.f.Peer {
					t.Errorf("peer %s in the result of a query filtered to %s", r.PeerIP, tc.f.Peer)
				}
				if tc.f.RIB != "" && r.RIB != tc.f.RIB {
					t.Errorf("rib %q in the result of a query filtered to %q", r.RIB, tc.f.RIB)
				}
				if r.Prefix != tc.f.Prefix {
					t.Errorf("prefix %q in the result of a query filtered to %q", r.Prefix, tc.f.Prefix)
				}
			}
		})
	}
}

// TestVPNRoutesFiltersByRDAndFamily is the pair of dimensions route_unicast
// has nowhere to put, asked of the real query: ?rd= and the vpn4/vpn6/lu4
// half of ?family=.
//
// Unlike the unicast family filter (see TestRoutesFiltersByFamily for why
// that one can only ever narrow to nothing), ?family= genuinely SELECTS here.
// familyFixtureVPNShared is carried twice under one router, peer and rib --
// once as vpn4 with an RD, once as lu4 without one -- which is a shape the
// protocol really produces and the archive really has the ingredients for
// (74 of its 164 route_vpn rows are lu4). Asking for that prefix returns
// both; adding family= returns exactly one, and which one is not inferable
// from the RD, which is the distinction api/openapi.yaml draws in so many
// words ("rd is \"\" and route_targets empty by nature, not by absence of
// data. family= distinguishes them without inferring from an empty rd").
//
// Several of these cases name no router at all, which is the point rather
// than an oversight: rd= standing alone is one of the three filters the
// contract says may. Their counts are therefore claims about every
// route_vpn row in the shared test database, which is why
// insertFamilyFilterFixture reserves an RD block no other fixture writes --
// see its own doc comment.
func TestVPNRoutesFiltersByRDAndFamily(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFamilyFilterFixture(t, ctx, q)

	router := netip.MustParseAddr(familyFixtureRouterIP)

	for _, tc := range []struct {
		name string
		f    VPNRouteFilter
		want int
	}{
		// The unfiltered-within-this-router baseline first: without it,
		// every count below could be explained by the fixture having
		// written fewer rows than it meant to.
		{"router alone", VPNRouteFilter{Router: router}, 5},

		// One prefix, two families. This is the case that makes family=
		// a selection rather than a narrowing to zero.
		{"a prefix carried by two families", VPNRouteFilter{Prefix: familyFixtureVPNShared}, 2},
		{"that prefix as vpn4", VPNRouteFilter{Prefix: familyFixtureVPNShared, Family: "vpn4"}, 1},
		{"that prefix as lu4", VPNRouteFilter{Prefix: familyFixtureVPNShared, Family: "lu4"}, 1},

		// rd= alone, the shape that needs Prefix to be optional.
		{"one rd", VPNRouteFilter{RD: familyFixtureRDOne}, 3},
		{"the other rd", VPNRouteFilter{RD: familyFixtureRDTwo}, 1},
		{"rd and family together", VPNRouteFilter{RD: familyFixtureRDOne, Family: "vpn6"}, 1},

		// family= as a narrowing of a router-scoped answer, which is the
		// shape a dashboard actually asks. vpn6 is fixture-only -- the
		// live archive has zero vpn6 rows -- so it is asserted positively
		// (one row, and it is the v6 prefix) rather than as an emptiness
		// check the empty archive would satisfy on its own.
		{"router and vpn4", VPNRouteFilter{Router: router, Family: "vpn4"}, 3},
		{"router and vpn6", VPNRouteFilter{Router: router, Family: "vpn6"}, 1},
		{"router and lu4", VPNRouteFilter{Router: router, Family: "lu4"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustVPNRoutes(t, ctx, q, tc.f)
			if len(got) != tc.want {
				t.Fatalf("got %d routes, want %d: %+v", len(got), tc.want, got)
			}
			// The count alone would pass for a filter that selected the
			// right NUMBER of wrong rows. Every row is checked against
			// every dimension the filter actually named -- and RD is
			// checked even when the filter did not name one, against the
			// fixture's own two, so a query that answered "rd 65080:1"
			// with 65080:2's row cannot pass on arithmetic.
			for _, r := range got {
				if tc.f.Prefix != "" && r.Prefix != tc.f.Prefix {
					t.Errorf("prefix %q in the result of a query filtered to %q", r.Prefix, tc.f.Prefix)
				}
				if tc.f.RD != "" && r.RD != tc.f.RD {
					t.Errorf("rd %q in the result of a query filtered to %q", r.RD, tc.f.RD)
				}
				if tc.f.Router.IsValid() && r.RouterIP != tc.f.Router {
					t.Errorf("router %s in the result of a query filtered to %s", r.RouterIP, tc.f.Router)
				}
			}
		})
	}

	// family= vpn6's one row is the v6 prefix and nothing else. Asserting
	// the identity, not just the count, is what stops a broken vpn6
	// predicate passing because some other single row happened to survive.
	t.Run("vpn6 selects the v6 prefix itself", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Router: router, Family: "vpn6"})
		if len(got) != 1 || got[0].Prefix != familyFixtureVPNv6 {
			t.Fatalf("got %+v, want exactly %s", got, familyFixtureVPNv6)
		}
	})

	// lu4's one row carries the empty RD, and that is a real value rather
	// than an absence -- the distinction l3vpn-rib-browser.json got wrong
	// (see vpnRoutesSQL's own doc comment on r.rd AS vrf). It is also why
	// RD cannot be used to ASK for lu4: filtering on RD "" means "not
	// asked", so family= is the only way to reach this row by its family.
	t.Run("lu4 is reachable by family, and its RD is genuinely empty", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{Router: router, Family: "lu4"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].RD != "" {
			t.Errorf("RD = %q, want \"\" -- BGP-LU carries no route distinguisher", got[0].RD)
		}
	})
}

// TestVPNRoutesRequiresPrefixRDOrRouter pins api/openapi.yaml's own rule for
// /v1/routes/vpn: "At least one of prefix=, rd=, or router= is required --
// the same rule as /v1/routes/evpn (an unfiltered dump belongs to
// /v1/rib/vpn)."
//
// The three that do NOT satisfy it are the interesting half. peer=, rib= and
// family= are all real filters that really do narrow the answer, and every
// one of them still leaves it unbounded: `?rib=in_pre` over a fleet is
// essentially the whole table, and this surface has no cursor, no page limit
// and no pinned session to hand that back through. /v1/rib/vpn has all
// three, which is what makes "the unfiltered dump belongs there" a routing
// decision rather than a refusal to answer.
//
// It is enforced in query/ rather than left to the HTTP layer because
// "return everything" is a query this package would otherwise run happily,
// and a validation living only in cmd/vantage-api is one that `vantage
// query` and every test helper walks straight past.
func TestVPNRoutesRequiresPrefixRDOrRouter(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFamilyFilterFixture(t, ctx, q)
	// The covers fixture's VPN rows are what make the "covers alone"
	// accepted case return something; without them it would assert only
	// that the filter was not refused.
	insertCoversFixture(t, ctx, q)

	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    VPNRouteFilter
		}{
			{"nothing at all", VPNRouteFilter{}},
			{"peer alone", VPNRouteFilter{Peer: netip.MustParseAddr(familyFixturePeerIP)}},
			{"rib alone", VPNRouteFilter{RIB: "in_pre"}},
			{"family alone", VPNRouteFilter{Family: "vpn4"}},
			{"peer, rib and family together", VPNRouteFilter{
				Peer: netip.MustParseAddr(familyFixturePeerIP), RIB: "in_pre", Family: "vpn4",
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := q.VPNRoutes(ctx, tc.f)
				if !errors.Is(err, ErrBadFilter) {
					t.Fatalf("VPNRoutes(%+v) error = %v, want one wrapping ErrBadFilter", tc.f, err)
				}
				if got != nil {
					t.Errorf("VPNRoutes returned %d rows alongside its error; a "+
						"refused filter must return no rows at all", len(got))
				}
			})
		}
	})

	// The other half: each of the three DOES satisfy the rule on its own,
	// so the check is a floor and not a second required-parameter rule.
	t.Run("accepted", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    VPNRouteFilter
		}{
			{"prefix alone", VPNRouteFilter{Prefix: familyFixtureVPNShared}},
			{"rd alone", VPNRouteFilter{RD: familyFixtureRDOne}},
			{"router alone", VPNRouteFilter{Router: netip.MustParseAddr(familyFixtureRouterIP)}},
			// Covers is the fourth, and it satisfies the rule on the
			// rule's own terms: a containment query is a prefix-space
			// narrowing that bounds the answer, which is what the other
			// three have in common. GET /v1/routes?covers=X fans out to
			// exactly this call, so a rule that refused it would leave a
			// documented endpoint unanswerable. See VPNRouteFilter.Covers.
			{"covers alone", VPNRouteFilter{Covers: coversFixtureVPNTarget}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := mustVPNRoutes(t, ctx, q, tc.f); len(got) == 0 {
					t.Errorf("VPNRoutes(%+v) returned nothing; the fixture is not "+
						"adversarial and this case proves nothing", tc.f)
				}
			})
		}
	})
}

// TestVPNRoutesRejectsAFamilyRouteVPNCannotHold is
// TestRoutesRejectsAFamilyRouteUnicastCannotHold's counterpart, and it
// matters for the same reason: family is LowCardinality(String), so an
// unknown value matches nothing rather than raising.
//
// "ipv4u" is the interesting case. It is what a caller who pointed
// /v1/routes/vpn at the unicast vocabulary would send, and route_vpn will
// never hold it -- a plain unicast route is in the other table entirely --
// so accepting it would answer "no VPN routes for this prefix" to a question
// that was never asked properly.
func TestVPNRoutesRejectsAFamilyRouteVPNCannotHold(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFamilyFilterFixture(t, ctx, q)

	for _, family := range []string{"ipv4u", "ipv6u", "evpn", "vpnv4"} {
		_, err := q.VPNRoutes(ctx, VPNRouteFilter{Prefix: familyFixtureVPNShared, Family: family})
		if !errors.Is(err, ErrBadFilter) {
			t.Errorf("VPNRoutes(family=%q) error = %v, want one wrapping ErrBadFilter",
				family, err)
		}
	}
}

// TestVPNRoutesReportsPathAttributes is the route_vpn half of the four
// RouteCommon attributes, plus the Family field VPNRoute.required has always
// listed and this type never had.
//
// Family matters more here than it does on Route and this test says so
// directly: route_vpn holds vpn4, vpn6 and lu4 at once, RD does not recover
// which one a row is (an lu4 row's rd = "" is a real column value, not a
// marker), and the column is what every row's DumpState is resolved against
// through eor.fam = r.family. It is also a new SELECT position between rib
// and path_id, so a transposition is the other thing under test here.
func TestVPNRoutesReportsPathAttributes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPathAttrFixture(t, ctx, q)

	got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRD})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	r := got[0]

	// rib, family, rd and prefix are four String-shaped values in one SELECT
	// list and all four are different, so a transposed pair reads as an
	// ordinary row rather than as an error.
	if r.RIB != "in_pre" || r.Family != "vpn4" || r.RD != pathAttrVPNRD || r.Prefix != pathAttrVPNPrefix {
		t.Errorf("got RIB=%q Family=%q RD=%q Prefix=%q, want %q, %q, %q, %q",
			r.RIB, r.Family, r.RD, r.Prefix, "in_pre", "vpn4", pathAttrVPNRD, pathAttrVPNPrefix)
	}
	wantMED, wantLocalPref := uint32(pathAttrMED), uint32(pathAttrLocalPref)
	if !sameNullableU32(r.MED, &wantMED) || !sameNullableU32(r.LocalPref, &wantLocalPref) {
		t.Errorf("got MED=%s LocalPref=%s, want %d and %d -- two adjacent "+
			"Nullable(UInt32) columns, so a swap between them raises nothing",
			nullableU32(r.MED), nullableU32(r.LocalPref), pathAttrMED, pathAttrLocalPref)
	}
	if !slices.Equal(r.Communities, pathAttrCommunityText) {
		t.Errorf("Communities = %v, want %v", r.Communities, pathAttrCommunityText)
	}
	if !slices.Equal(r.LargeCommunities, pathAttrLargeCommunities) {
		t.Errorf("LargeCommunities = %v, want %v", r.LargeCommunities, pathAttrLargeCommunities)
	}
	// The fields that already existed, asserted alongside the new ones
	// because adding four SELECT positions to a statement is exactly when a
	// scan drifts from it: route_targets, communities and large_communities
	// are three array columns in a row, and labels is a fourth.
	if !slices.Equal(r.RouteTargets, []string{"65120:100"}) {
		t.Errorf("RouteTargets = %v, want [65120:100] -- an Array(String) "+
			"sitting beside two other array columns", r.RouteTargets)
	}
	if !r.HasLabel || r.Label != 120001 {
		t.Errorf("got Label=%d HasLabel=%v, want 120001 and true", r.Label, r.HasLabel)
	}
	if !slices.Equal(r.ASPath, pathAttrASPath) || r.OriginASN != pathAttrASPath[len(pathAttrASPath)-1] {
		t.Errorf("got ASPath=%v OriginASN=%d, want %v and %d",
			r.ASPath, r.OriginASN, pathAttrASPath, pathAttrASPath[len(pathAttrASPath)-1])
	}

	// The nullable aggregation, pinned separately on this statement rather
	// than borrowed from the unicast one. argMax(tuple(...), ...).1 is
	// written out once per statement, so a copy reverted to a plain argMax
	// here is a copy no unicast test can see -- verified by exactly that
	// mutation, which left the package green before rd 65120:3 existed.
	t.Run("a cleared MED reads as absent, not as the value it used to have", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRDCleared})
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

// TestVPNRoutesFiltersByWideParameters closes a gap: nothing before it set
// OriginASN, ThroughASN or Community on a VPNRouteFilter and ran a live
// query against real data. The %[3]s HAVING splice VPNRoutes renders them
// into was exercised by exactly one handler test, and that test asserted
// only rec.Code == 200.
//
// It reuses insertPathAttrFixture's own VPN row rather than writing a new
// fixture: RD pathAttrVPNRD already carries an AS path (pathAttrASPath =
// [65010, 65120], so 65120 is the origin and 65010 the one transit hop), a
// route target (65120:100) and a standard community (65000:100, packed the
// same way TestParseCommunityPacksAStandardCommunity pins), which is
// everything a wide filter needs to find. pathAttrVPNRDCleared, the
// neighboring row TestVPNRoutesReportsPathAttributes' own subtest uses, sets
// none of the community columns, which is what makes it useful here too: a
// community query that matched it would be matching on nothing.
func TestVPNRoutesFiltersByWideParameters(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPathAttrFixture(t, ctx, q)

	origin, transit := pathAttrASPath[len(pathAttrASPath)-1], pathAttrASPath[0]

	t.Run("origin_asn matches the last hop", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRD, OriginASN: origin})
		if len(got) != 1 {
			t.Fatalf("OriginASN=%d against rd=%s got %d rows, want 1",
				origin, pathAttrVPNRD, len(got))
		}
	})

	t.Run("origin_asn does not match a transit hop", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRD, OriginASN: transit})
		if len(got) != 0 {
			t.Errorf("OriginASN=%d matched %d rows; %d is a transit AS on this "+
				"path, not the origin", transit, len(got), transit)
		}
	})

	t.Run("through_asn matches the transit hop", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRD, ThroughASN: transit})
		if len(got) != 1 {
			t.Fatalf("ThroughASN=%d against rd=%s got %d rows, want 1",
				transit, pathAttrVPNRD, len(got))
		}
	})

	t.Run("community matches the packed standard community", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRD, Community: "65000:100"})
		if len(got) != 1 {
			t.Fatalf("community=65000:100 against rd=%s got %d rows, want 1",
				pathAttrVPNRD, len(got))
		}
	})

	t.Run("community matches the route target", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{RD: pathAttrVPNRD, Community: "65120:100"})
		if len(got) != 1 {
			t.Fatalf("community=65120:100 against rd=%s got %d rows, want 1",
				pathAttrVPNRD, len(got))
		}
	})

	t.Run("community does not match a row with no communities at all", func(t *testing.T) {
		got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{
			RD: pathAttrVPNRDCleared, Community: "65000:100",
		})
		if len(got) != 0 {
			t.Errorf("community=65000:100 matched %d rows under rd=%s, which "+
				"carries no communities in this fixture", len(got), pathAttrVPNRDCleared)
		}
	})
}

// TestVPNRoutesCovers is ?covers= reaching route_vpn, which it could not do
// for a task even though api/openapi.yaml documented the parameter on
// /v1/routes -- the fan-out whose answer carries all three families.
//
// The failure that made this a must-fix is not the missing capability but
// the wrong answer beside a right one: with no Covers on this filter,
// `?covers=X&router=R` passed VPNRouteFilter.check on Router alone and
// returned EVERY VPN route that router has, unfiltered by the address, in
// the same JSON object as a correct unicast answer. `?covers=X` alone was
// the honest half of the same defect -- ErrBadFilter, because nothing
// bounded it.
//
// Two lengths and a sibling, which is the smallest shape that tells
// containment apart from equality and from a comparison one octet too wide.
// The predicate itself is one shared const exercised in depth by
// TestRoutesCovers, so this asserts that route_vpn is asked the question,
// not that the arithmetic is right a second time.
func TestVPNRoutesCovers(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertCoversFixture(t, ctx, q)
	router := netip.MustParseAddr(coversFixtureRouterIP)

	want := []string{coversFixtureVPNSlash16, coversFixtureVPNSlash24}
	slices.Sort(want)

	got := mustVPNRoutes(t, ctx, q, VPNRouteFilter{
		Covers: coversFixtureVPNTarget, Router: router,
	})
	prefixes := make([]string, 0, len(got))
	for _, r := range got {
		prefixes = append(prefixes, r.Prefix)
	}
	slices.Sort(prefixes)
	if !slices.Equal(prefixes, want) {
		t.Fatalf("VPNRoutes(Covers=%s, Router=%s) = %v, want %v -- every prefix "+
			"that contains the address at every length, and nothing else. The "+
			"sibling %s shares the /16 and does not contain it; a full list of "+
			"this router's VPN routes here is the unfiltered answer the missing "+
			"Covers field used to give.",
			coversFixtureVPNTarget, coversFixtureRouterIP, prefixes, want,
			coversFixtureVPNSibling)
	}

	// And the RD travels with the rows, so this is route_vpn being answered
	// rather than some other table's prefixes arriving under a VPN type.
	for _, r := range got {
		if r.RD != coversFixtureVPNRD {
			t.Errorf("row %s carries rd %q, want %q", r.Prefix, r.RD, coversFixtureVPNRD)
		}
	}

	// Covers and Prefix together are refused rather than ANDed, the same
	// rule RouteFilter carries and now the same code path (checkCovers).
	if _, err := q.VPNRoutes(ctx, VPNRouteFilter{
		Prefix: coversFixtureVPNSlash24, Covers: coversFixtureVPNTarget,
	}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("VPNRoutes(Prefix and Covers) error = %v, want one wrapping "+
			"ErrBadFilter -- ANDing them asks for an exact prefix that also "+
			"contains an address, whose empty answer reads like the prefix "+
			"being nowhere", err)
	}
}

// TestCountVPNRoutesAgreesWithVPNRoutes is CountRoutes' own regression (see
// its own comment in routes_test.go), applied to VPNRoutes: the count has to
// come from the same statement VPNRoutes runs, not a hand-derived copy of
// its predicate that could drift from it.
//
// It reuses TestVPNRoutes' own "one prefix in two VRFs stays two routes"
// case, which is fixture-only for the reason that subtest's own comment
// gives -- and exactly why it is useful here too: two rows under one
// prefix is a case a naive `SELECT count() FROM route_vpn WHERE prefix = ?`
// would also get right, so this is really testing that CountVPNRoutes counts
// live route KEYS the way VPNRoutes' own GROUP BY does, not raw rows.
func TestCountVPNRoutesAgreesWithVPNRoutes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertVPNRouteFixture(t, ctx, q)

	f := VPNRouteFilter{Prefix: "192.168.1.0/24"}
	rows, err := q.VPNRoutes(ctx, f)
	if err != nil {
		t.Fatalf("VPNRoutes: %v", err)
	}
	n, err := q.CountVPNRoutes(ctx, f)
	if err != nil {
		t.Fatalf("CountVPNRoutes: %v", err)
	}
	if n != uint64(len(rows)) {
		t.Errorf("CountVPNRoutes = %d but VPNRoutes returned %d rows", n, len(rows))
	}
	if n == 0 {
		t.Fatal("the fixture matched nothing, so this comparison is vacuous")
	}
}
