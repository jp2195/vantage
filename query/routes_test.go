package query

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
)

// mustRoutes calls Routes and fails the test on error, so each subtest
// below reads as a claim about the result rather than about error
// handling. It takes the whole RouteFilter rather than a prefix alone:
// most subtests here ask only about a prefix, but the filter's optional
// fields are exactly what TestRoutesFiltersByRouterPeerAndRib exercises,
// and two helpers differing only in how much of the filter they let a
// caller set would be one more place for a filter to go missing unnoticed.
func mustRoutes(t *testing.T, ctx context.Context, q *Q, f RouteFilter) []Route {
	t.Helper()
	got, err := q.Routes(ctx, f)
	if err != nil {
		t.Fatalf("Routes(%+v): %v", f, err)
	}
	return got
}

func TestRoutes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFixture(t, ctx, q)

	t.Run("a withdraw removes only its own path-id", func(t *testing.T) {
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: "10.1.0.0/24"})
		// The fixture advertises path-ids 1 and 2, then withdraws only 1.
		if len(got) != 1 || got[0].PathID != 2 {
			t.Fatalf("got %+v, want exactly path-id 2 -- a withdraw must not "+
				"take the sibling path with it", got)
		}
	})

	t.Run("a down peer contributes nothing", func(t *testing.T) {
		// The fixture advertises 10.2.0.0/24 from a peer that then goes
		// down. The route row is still in the table; it must not be
		// reported, and it must not be reported as stale either.
		if got := mustRoutes(t, ctx, q, RouteFilter{Prefix: "10.2.0.0/24"}); len(got) != 0 {
			t.Fatalf("got %+v, want none -- the advertising peer is down", got)
		}
	})

	t.Run("a session reset discards the prior dump", func(t *testing.T) {
		// Advertised in the old session only.
		if got := mustRoutes(t, ctx, q, RouteFilter{Prefix: "10.3.0.0/24"}); len(got) != 0 {
			t.Fatalf("got %+v, want none -- that dump belongs to a dead session", got)
		}
	})

	t.Run("add-path keeps both path-ids", func(t *testing.T) {
		// 10.5.0.0/24 is advertised twice from one peer under path-ids 7
		// and 8 with different next hops. Collapsing on prefix alone --
		// the obvious GROUP BY -- reports one of them and silently drops
		// the other, which is the whole reason path_id is in the route key.
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: "10.5.0.0/24"})
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2 -- add-path siblings are distinct routes: %+v", len(got), got)
		}
		if got[0].NextHop == got[1].NextHop {
			t.Errorf("both rows carry next hop %s; the fixture is not adversarial", got[0].NextHop)
		}
		// routeFixtureUpPeerIP's current session carries route_unicast rows
		// (these two, and 10.1.0.0/24's and 10.4.0.0/24's) but no ipv4u
		// marker, so its ipv4u dump is genuinely still in progress. The
		// session does carry a vpn4 marker for this same peer, which is
		// what makes routesSQL's eor.fam = r.family join predicate
		// load-bearing here: without it, another family's finished dump
		// would report these ipv4u routes as "complete".
		for _, r := range got {
			if r.DumpState != "dumping" {
				t.Errorf("path-id %d: DumpState = %q, want %q", r.PathID, r.DumpState, "dumping")
			}
		}
	})

	t.Run("origin AS is absent, not zero, when there is no AS path", func(t *testing.T) {
		// 142 of 164 live VPN rows carry no AS path, and as_path[-1] on an
		// empty array yields the reserved AS 0 -- which l3vpn-rib-browser
		// rendered as a real origin until it was fixed. Do not repeat it.
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: "10.4.0.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		if got[0].OriginASN != 0 || len(got[0].ASPath) != 0 {
			t.Fatalf("got OriginASN=%d ASPath=%v; an iBGP route with no AS path "+
				"must report an empty path, and callers must be able to tell "+
				"that from AS 0", got[0].OriginASN, got[0].ASPath)
		}
	})

	t.Run("DumpState reads complete once the session's dump has an end-of-rib marker", func(t *testing.T) {
		// 10.5.0.0/24's own subtest above already proves DumpState reads
		// "dumping"; on its own that leaves the field looking like it could
		// be pinned to that one constant. routeFixtureCompletePeer's
		// session carries an ipv4u marker in eor_events (see
		// insertRouteFixture), so 10.6.0.0/24 -- the one live route that
		// peer also advertises -- must read "complete" instead.
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: "10.6.0.0/24"})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		if got[0].DumpState != "complete" {
			t.Errorf("DumpState = %q, want %q -- the session that advertised "+
				"this route also carries an eor_events marker for its family",
				got[0].DumpState, "complete")
		}
	})

	t.Run("an end-of-rib marker is not a route", func(t *testing.T) {
		// insertRouteFixture writes an end-of-RIB marker for
		// routeFixtureCompletePeer's current session. It used to be a
		// route_unicast row with end_of_rib = 1 and an empty prefix, and a
		// query that forgot the end_of_rib predicate returned it from an
		// empty-prefix query as if it were a route to the empty prefix --
		// exactly the "56 of 476 route_unicast rows are end-of-RIB markers"
		// defect the live archive check found. It now lands in eor_events,
		// so no predicate has to be remembered for this to hold.
		//
		// That is also what made the loop below, on its own, assert
		// NOTHING: route_unicast holds no prefix-less row for this router
		// any more, so `got` is empty and the body never runs. The subtest
		// was meaningful before the split and became vacuous with it. It
		// now takes the shape sink/dashboard_sql_test.go's looking-glass fixture check uses for
		// the identical claim, asserting BOTH halves of the split: the
		// marker still exists where it now belongs, and no prefix-less row
		// was left behind where it used to. Asserting only the second half
		// passes just as well against a fixture that stopped writing a
		// marker at all.
		//
		// Everything is scoped to routeFixtureRouterIP rather than asserted
		// fleet-wide: chtest shares one database across every test in this
		// package, and this subtest's claim is about this fixture's marker,
		// not about every empty-prefix row any fixture might ever write.
		var markers uint64
		if err := q.conn.QueryRow(ctx,
			"SELECT count() FROM "+q.db+".eor_events WHERE router_ip = toIPv6(?) AND peer_ip = toIPv6(?)",
			routeFixtureRouterIP, routeFixtureCompletePeer).Scan(&markers); err != nil {
			t.Fatalf("count markers: %v", err)
		}
		if markers == 0 {
			t.Fatalf("the fixture carries no eor_events marker for %s/%s; the "+
				"assertions below are about a marker that was never written, "+
				"and 10.6.0.0/24's DumpState = \"complete\" above would be the "+
				"only thing left exercising the path",
				routeFixtureRouterIP, routeFixtureCompletePeer)
		}

		var prefixless uint64
		if err := q.conn.QueryRow(ctx,
			"SELECT count() FROM "+q.db+".route_unicast WHERE router_ip = toIPv6(?) AND prefix = ''",
			routeFixtureRouterIP).Scan(&prefixless); err != nil {
			t.Fatalf("count prefix-less rows: %v", err)
		}
		if prefixless != 0 {
			t.Errorf("route_unicast holds %d prefix-less row(s) for %s -- the "+
				"marker is filed in eor_events now, and a route table that "+
				"collects artifacts again puts the end_of_rib = 0 predicate "+
				"back into every query over it", prefixless, routeFixtureRouterIP)
		}

		// And the query surface agrees with the table: asking for the empty
		// prefix returns nothing from this router. This is the assertion
		// that would fail if markers were ever filed back into a route
		// table without the storage check above noticing -- they are two
		// different claims, one about the schema and one about the answer.
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: ""})
		fixtureRouter := netip.MustParseAddr(routeFixtureRouterIP)
		for _, r := range got {
			if r.RouterIP == fixtureRouter {
				t.Errorf("Routes(RouteFilter{Prefix: \"\"}) returned %+v -- an end-of-rib marker "+
					"is a protocol sentinel, not a route", r)
			}
		}
	})
}

// TestRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded pins a
// real gap: peerStateCTE's own rib-scoped join (INNER JOIN peer_state ON
// r.rib = peer_state.rib) dropped any route observed under a rib
// peer_events has no accounting for at all, even when the peer that
// advertised it is unambiguously up -- see insertPostPolicyRouteFixture's
// own doc comment for the live-archive occurrence this reproduces (a dead
// session on router 172.22.0.7, invisible to every query that scopes to
// the CURRENT session, which is exactly why this defect needed a fixture
// rather than a live-archive assertion).
//
// Routes must return the route, reading its peer as up (peer_up, not
// peer_state, is what decides that now -- see routesSQL's own doc comment),
// and DumpState = "unknown" -- not a guess either way, but the honest
// answer when there is no rib-scoped dump-progress data to consult at all.
func TestRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPostPolicyRouteFixture(t, ctx, q)

	t.Run("Routes reports the route", func(t *testing.T) {
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: postPolicyRouteFixturePrefix})
		if len(got) != 1 {
			t.Fatalf("got %d routes, want 1 -- a route under a rib peer_events "+
				"never recorded for this peer must still be reported, since "+
				"nothing about this fixture says the peer is down: %+v", len(got), got)
		}
		if got[0].PeerIP.String() != postPolicyRouteFixtureUpPeerIP {
			t.Errorf("PeerIP = %s, want %s", got[0].PeerIP, postPolicyRouteFixtureUpPeerIP)
		}
		if got[0].RIB != "in_post" {
			t.Errorf("RIB = %q, want %q", got[0].RIB, "in_post")
		}
		if got[0].DumpState != "unknown" {
			t.Errorf("DumpState = %q, want %q -- no rib-scoped peer_events "+
				"data exists for this route's rib at all", got[0].DumpState, "unknown")
		}
	})

	// The whole reason the narrow fix was chosen over re-keying peer_state:
	// Peers must keep reporting exactly the one (router, peer, rib) row this
	// fixture's own peer_events data supports (rib = in_pre), never a second
	// row for in_post that peer_state was never asked to produce. The
	// in_post route above is real and reported by Routes, but Peer.Routes on
	// the in_pre row must not silently absorb it either -- route_counts is
	// itself scoped per rib, and this fixture's one route lives under a rib
	// peer_state has no row for, so it has nowhere to attach and correctly
	// reports 0, not 1.
	t.Run("Peers' own per-(router, peer, rib) shape is unchanged", func(t *testing.T) {
		got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(postPolicyRouteFixtureRouterIP)})
		if err != nil {
			t.Fatalf("Peers: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d peer rows, want 1: %+v", len(got), got)
		}
		p := got[0]
		if p.PeerIP.String() != postPolicyRouteFixtureUpPeerIP {
			t.Errorf("PeerIP = %s, want %s", p.PeerIP, postPolicyRouteFixtureUpPeerIP)
		}
		if p.RIB != "in_pre" {
			t.Errorf("RIB = %q, want %q -- peer_events only ever recorded "+
				"this peer under in_pre, and Peers must not invent an in_post "+
				"row peer_state has no data to produce", p.RIB, "in_pre")
		}
		if p.State != "up" {
			t.Errorf("State = %q, want %q", p.State, "up")
		}
		if p.Routes != 0 {
			t.Errorf("Routes = %d, want 0 -- this peer's one route lives "+
				"under in_post, a rib its in_pre peer_state row has no route "+
				"count attached to", p.Routes)
		}
	})
}

// TestRoutesFiltersByRouterPeerAndRib is the behavior half: the
// three optional dimensions api/openapi.yaml documents on /v1/routes/unicast
// (?router=, ?peer=, ?rib=), asked of the real query rather than of the
// builder's rendered text.
//
// It exists as a counterpart to TestRouteFilterRendersOnlyWhatWasAsked
// rather than a duplicate of it. That test would catch a predicate that
// stopped being emitted; this one catches a predicate that is emitted
// against the wrong column -- r.router_ip where r.peer_ip was meant, say --
// which renders perfectly and answers wrongly. insertRouteFilterFixture's
// own doc comment explains why each filter here selects a differently-sized
// subset, and why one of its peers belongs to both routers.
func TestRoutesFiltersByRouterPeerAndRib(t *testing.T) {
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
		f    RouteFilter
		want int
	}{
		// The unfiltered baseline first: without it, every count below
		// could be explained by the fixture having written fewer rows than
		// it meant to rather than by the filter having selected them.
		{"prefix alone", RouteFilter{Prefix: routeFilterFixturePrefix}, 6},
		{"router", RouteFilter{Prefix: routeFilterFixturePrefix, Router: routerA}, 4},
		{"the other router", RouteFilter{Prefix: routeFilterFixturePrefix, Router: routerB}, 2},
		{"peer", RouteFilter{Prefix: routeFilterFixturePrefix, Peer: peerA1}, 2},
		// A peer both routers have: filtering on it alone must not narrow
		// to one router, which is exactly what would happen if this
		// predicate had been built against r.router_ip.
		{"a peer of both routers", RouteFilter{Prefix: routeFilterFixturePrefix, Peer: sharedPeer}, 2},
		{"router and peer together", RouteFilter{
			Prefix: routeFilterFixturePrefix, Router: routerA, Peer: sharedPeer,
		}, 1},
		{"rib", RouteFilter{Prefix: routeFilterFixturePrefix, RIB: "loc_rib"}, 1},
		{"router and rib together", RouteFilter{
			Prefix: routeFilterFixturePrefix, Router: routerA, RIB: "in_pre",
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRoutes(t, ctx, q, tc.f)
			if len(got) != tc.want {
				t.Fatalf("got %d routes, want %d: %+v", len(got), tc.want, got)
			}
			// The count alone would pass for a filter that selected the
			// right NUMBER of wrong rows -- two rows of router B where two
			// rows of router A were asked for. Every row is checked
			// against every dimension the filter actually named.
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

// TestRoutesRejectsARibThatIsNotAnEnumMember pins the failure mode
// eqNonEmpty's doc comment promises for a rib the contract does not define:
// ClickHouse raises on an unknown Enum8 element, so the caller gets an error
// rather than an empty result that reads like "this router advertises
// nothing". An empty result would be the worse answer by far -- it is
// indistinguishable from a correct one, and the HTTP layer that was supposed
// to validate the value against api/openapi.yaml's own five-member enum
// would have no way to learn it had let a typo through.
func TestRoutesRejectsARibThatIsNotAnEnumMember(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFilterFixture(t, ctx, q)

	if _, err := q.Routes(ctx, RouteFilter{
		Prefix: routeFilterFixturePrefix, RIB: "in_pre_but_misspelled",
	}); err == nil {
		t.Error("Routes accepted a rib that is not one of the enum's five members; " +
			"an unknown rib must fail loudly rather than match nothing")
	}
}

// TestRoutesFiltersByFamily is ?family= on /v1/routes/unicast, asked of the
// real query. It is a narrower claim than TestRoutesFiltersByRouterPeerAndRib
// makes about its three dimensions, and the reason is worth stating rather
// than leaving to be rediscovered by whoever tries to improve this test.
//
// f.Prefix is REQUIRED on Routes (see RouteFilter.Prefix), and no prefix
// string is legal NLRI in both ipv4u and ipv6u -- "10.80.0.0/24" is not an
// IPv6 prefix and "2001:db8:80::/48" is not an IPv4 one -- so a prefix has
// already picked the family out before ?family= is applied. There is
// therefore no fixture in which this filter SELECTS between two rows that
// would otherwise both be returned, and manufacturing one would mean writing
// a route_unicast row the sink cannot produce: an IPv4 prefix stamped ipv6u.
// The honest assertion is the cross pair -- each prefix asked for under the
// OTHER family must return nothing -- and that is what fails when the
// predicate is deleted, because the prefix alone still matches.
//
// The filter earns a wider job the moment ?covers= arrives and prefix stops
// being required: a containment query spans both families at once, and
// "ipv4u only" is a real narrowing there. It is implemented now because
// api/openapi.yaml documents it now.
//
// The ipv6u rows this reads are fixture-only of necessity -- the live
// archive holds zero IPv6 routes -- so the v6 half is deliberately written
// as a POSITIVE assertion (the v6 prefix under ipv6u returns its one row)
// rather than only as an emptiness check, which the empty archive would
// satisfy on its own.
func TestRoutesFiltersByFamily(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFamilyFilterFixture(t, ctx, q)

	for _, tc := range []struct {
		name string
		f    RouteFilter
		want int
	}{
		{"the v4 prefix, no family named", RouteFilter{Prefix: familyFixtureV4Prefix}, 1},
		{"the v4 prefix as ipv4u", RouteFilter{Prefix: familyFixtureV4Prefix, Family: "ipv4u"}, 1},
		{"the v4 prefix as ipv6u", RouteFilter{Prefix: familyFixtureV4Prefix, Family: "ipv6u"}, 0},
		{"the v6 prefix, no family named", RouteFilter{Prefix: familyFixtureV6Prefix}, 1},
		{"the v6 prefix as ipv6u", RouteFilter{Prefix: familyFixtureV6Prefix, Family: "ipv6u"}, 1},
		{"the v6 prefix as ipv4u", RouteFilter{Prefix: familyFixtureV6Prefix, Family: "ipv4u"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRoutes(t, ctx, q, tc.f)
			if len(got) != tc.want {
				t.Fatalf("got %d routes, want %d: %+v", len(got), tc.want, got)
			}
			for _, r := range got {
				if r.Prefix != tc.f.Prefix {
					t.Errorf("prefix %q in the result of a query filtered to %q", r.Prefix, tc.f.Prefix)
				}
			}
		})
	}
}

// TestRoutesRejectsAFamilyRouteUnicastCannotHold is the family counterpart to
// TestRoutesRejectsARibThatIsNotAnEnumMember, and it exists because the two
// columns fail differently. rib is an Enum8, so ClickHouse itself raises on
// an unknown member; family is LowCardinality(String), so an unknown one is
// a perfectly valid comparison that matches no row -- an empty result the
// caller cannot tell from "this prefix is not carried in that family
// anywhere". Nothing downstream will ever complain, so the check has to be
// in Go, and this is what says so.
//
// "vpn4" rather than a typo, deliberately: it is a real family token that a
// real route table really does carry, just not this one. A caller pointing
// /v1/routes/unicast at route_vpn's vocabulary is a far likelier mistake
// than a misspelling, and it is the one a single merged family set would
// have accepted (see unicastFamilies).
func TestRoutesRejectsAFamilyRouteUnicastCannotHold(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFamilyFilterFixture(t, ctx, q)

	for _, family := range []string{"vpn4", "evpn", "ipv4-unicast"} {
		_, err := q.Routes(ctx, RouteFilter{Prefix: familyFixtureV4Prefix, Family: family})
		if !errors.Is(err, ErrBadFilter) {
			t.Errorf("Routes(family=%q) error = %v, want one wrapping ErrBadFilter -- "+
				"an unknown family matches nothing rather than raising, so an "+
				"empty result would be indistinguishable from a correct one",
				family, err)
		}
	}
}

// coversPrefixes is the sorted prefix list of a covers result, which is what
// every assertion in TestRoutesCovers is actually about. Sorted rather than
// in routesSQL's own order because that order (sysname, peer, path_id,
// collector) says nothing about containment: every row this fixture writes
// shares a sysname and a peer, so asserting the query's order here would
// pin path_id assignment, which is a property of the fixture and not of the
// predicate under test.
func coversPrefixes(got []Route) []string {
	out := make([]string, 0, len(got))
	for _, r := range got {
		out = append(out, r.Prefix)
	}
	slices.Sort(out)
	return out
}

// TestRoutesCovers is ?covers= on /v1/routes/unicast: "routes whose prefix
// contains this address", the looking-glass question query/ could not ask
// before -- every other route filter in this package is an equality, and an
// exact-prefix surface can only answer "is this exact prefix here", never
// "what covers this address".
//
// api/openapi.yaml documents the cost at the parameter itself ("Full scan by
// construction (no index can serve containment); measured 31-52ms at 3.2M
// rows"), and that is a property of the question rather than of this
// implementation: a bloom filter answers exact membership, and containment
// is a range.
func TestRoutesCovers(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertCoversFixture(t, ctx, q)
	router := netip.MustParseAddr(coversFixtureRouterIP)

	// The three covering prefixes, sorted the way coversPrefixes sorts.
	// Written out rather than derived from coversFixtureRows so that a
	// fixture row silently changing family, prefix or length fails here.
	wantCovering := []string{coversFixtureSlash8, coversFixtureSlash16, coversFixtureSlash24}
	slices.Sort(wantCovering)

	t.Run("every covering prefix, at every length", func(t *testing.T) {
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTarget, Router: router})
		if diff := coversPrefixes(got); !slices.Equal(diff, wantCovering) {
			t.Fatalf("got %v, want %v -- a covers query must return EVERY prefix "+
				"that contains the address, not just the longest match: the "+
				"caller is the one that decides which of them the router would "+
				"actually use", diff, wantCovering)
		}
	})

	t.Run("a non-covering sibling is excluded", func(t *testing.T) {
		// Named as its own subtest rather than left to the set comparison
		// above, because it is the assertion that fails when the predicate
		// widens rather than when it disappears: coversFixtureSibling shares
		// the /16 and differs only in the third octet.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTarget, Router: router})
		for _, r := range got {
			if r.Prefix == coversFixtureSibling {
				t.Errorf("%s was reported as covering %s; it shares the /16 and "+
					"contains no address in %s's own /24",
					coversFixtureSibling, coversFixtureTarget, coversFixtureTarget)
			}
		}
	})

	t.Run("a malformed stored prefix covers nothing", func(t *testing.T) {
		// The row-side half of the predicate's totality, and the half a
		// caller cannot provoke: route_unicast.prefix is an unconstrained
		// String column, so these two rows are as legal to store as any
		// other. Each is caught by exactly one guard -- see
		// coversFixtureBadAddr's own comment for the arithmetic -- so
		// deleting either guard turns exactly one of them into a
		// plausible-looking covering route and fails this subtest.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTarget, Router: router})
		for _, r := range got {
			switch r.Prefix {
			case coversFixtureBadLen:
				t.Errorf("%q was reported as covering %s -- its length half does "+
					"not parse, and the 0 coalesce substitutes becomes a /96 "+
					"spanning the whole IPv4 space. The length range test is "+
					"what excludes it, by being NULL", r.Prefix, coversFixtureTarget)
			case coversFixtureWrapLen:
				t.Errorf("%q was reported as covering %s -- 200 is a perfectly "+
					"good UInt8, so a null check passes it through, and 200 + 96 "+
					"wraps to 40. `<= if(colon, 128, 32)` is what excludes it",
					r.Prefix, coversFixtureTarget)
			case coversFixtureOverLen:
				t.Errorf("%q was reported as covering %s", r.Prefix, coversFixtureTarget)
			}
		}
	})

	t.Run("an IPv6 route never covers an IPv4 address", func(t *testing.T) {
		// The family test, in the direction that produces a WRONG ANSWER
		// rather than a missing one. ::/0 contains ::ffff:10.77.0.33 as
		// 128-bit arithmetic -- verified directly against ClickHouse -- so
		// without the family conjunct an IPv6 default route is reported as
		// covering every IPv4 address in the fleet, and nothing about that
		// row looks wrong to whoever reads it.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTarget, Router: router})
		for _, r := range got {
			if r.Prefix == coversFixtureV6Default || r.Prefix == coversFixtureBadAddr {
				t.Errorf("%q was reported as covering the IPv4 address %s -- an "+
					"IPv6 prefix covers no IPv4 address in any operational sense, "+
					"whatever IPv6CIDRToRange makes of the mapped form",
					r.Prefix, coversFixtureTarget)
			}
		}
	})

	t.Run("a prefix length outside its own family's range covers nothing", func(t *testing.T) {
		// The per-family BOUND rather than the range test as a whole:
		// coversFixtureOverLen's /40 is out of range for IPv4 and inside it
		// for IPv6, so a flat `<= 128` would let it through, where 40 + 96 =
		// 136 clamps to a /128 -- and the row would be reported as covering
		// exactly its own base address. That is the one query in which the
		// two bounds differ, so it is the one this subtest asks.
		//
		// The answer is coversFixtureSlash8, not nothing: the base address
		// sits inside it, which is what keeps this from passing on a query
		// that had stopped returning rows at all.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureOverLenBase, Router: router})
		if want := []string{coversFixtureSlash8}; !slices.Equal(coversPrefixes(got), want) {
			t.Fatalf("got %v, want %v -- %q has a length no IPv4 prefix can carry",
				coversPrefixes(got), want, coversFixtureOverLen)
		}
	})

	t.Run("an IPv6 target selects the IPv6 route and nothing else", func(t *testing.T) {
		// The +96 conversion, from both sides. An IPv4 prefix length has to
		// be lifted into its IPv4-mapped IPv6 equivalent and an IPv6 one
		// must not be, so this subtest and the one above fail in OPPOSITE
		// directions: dropping the conversion entirely makes every IPv4
		// prefix here a /8-to-/24 of the v6 space (which contains the
		// IPv4-mapped target, so the sibling and both malformed rows join
		// the answer), and applying it unconditionally pushes this /48 to a
		// /144 that ClickHouse clamps to a single address (so this row
		// leaves it).
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTargetV6, Router: router})
		// The default route belongs in this answer as much as the /48 does,
		// and it is the same row the IPv4 subtest above requires to be
		// absent -- one fixture row failing in both directions, so neither
		// assertion can be satisfied by a predicate that simply excludes
		// more. coversFixtureBadAddr is NOT here: it reaches the v6 side of
		// the family test and is stopped by the address guard instead.
		want := []string{coversFixtureV6Default, coversFixtureV6}
		slices.Sort(want)
		if !slices.Equal(coversPrefixes(got), want) {
			t.Fatalf("got %v, want %v", coversPrefixes(got), want)
		}
	})

	t.Run("a malformed IPv6 prefix covers nothing either", func(t *testing.T) {
		// The address guard, which only an IPv6 query can now reach: with
		// the family test in place a colon-free malformed address can never
		// produce a false positive (its fallback range starts at :: and the
		// +96 pushes it past every IPv4-mapped address), so this row carries
		// colons and a length of 0. Without the guard, coalesce substitutes
		// '::' and a /0 of '::' contains every address there is.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTargetV6, Router: router})
		for _, r := range got {
			if r.Prefix == coversFixtureBadAddr {
				t.Errorf("%q was reported as covering %s -- its address half does "+
					"not parse, and the '::' coalesce substitutes with a length "+
					"of 0 is every address there is. "+
					"toIPv6OrNull(splitByChar('/', prefix)[1]) IS NOT NULL is "+
					"what excludes it", r.Prefix, coversFixtureTargetV6)
			}
		}
	})

	t.Run("an address nothing covers returns nothing", func(t *testing.T) {
		// 192.0.2.0/24 is TEST-NET-1 and this fixture writes no prefix in
		// it. Without this, every assertion above would still pass on a
		// predicate that was merely true for a great deal more than it
		// should be.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: "192.0.2.7", Router: router})
		if len(got) != 0 {
			t.Fatalf("got %v, want none", coversPrefixes(got))
		}
	})

	t.Run("family narrows a covers query", func(t *testing.T) {
		// The narrowing RouteFilter.Family could not do while Prefix was
		// required: a prefix string already picks its family out, so
		// ?family= could only ever narrow an exact-prefix query to zero
		// rows. An ADDRESS does not, which is why the contract documents
		// both parameters on the same endpoint.
		for _, tc := range []struct {
			family string
			want   []string
		}{
			{"ipv4u", wantCovering},
			{"ipv6u", nil},
		} {
			got := mustRoutes(t, ctx, q, RouteFilter{
				Covers: coversFixtureTarget, Router: router, Family: tc.family,
			})
			if !slices.Equal(coversPrefixes(got), tc.want) {
				t.Errorf("family %q: got %v, want %v", tc.family, coversPrefixes(got), tc.want)
			}
		}
	})

	t.Run("covers alone is a bounded question", func(t *testing.T) {
		// No router scope at all -- which api/openapi.yaml allows ("exactly
		// one of prefix= or covers= is required", and neither router= nor
		// peer= is on that list) and check() must therefore not refuse.
		// The assertion is an equality rather than a containment check for
		// the reason insertCoversFixture's own doc comment argues at
		// length: 10.77.x is reserved to this fixture and no other prefix
		// this package writes contains coversFixtureTarget, so a fleet-wide
		// covers query for it is answerable exactly.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: coversFixtureTarget})
		if !slices.Equal(coversPrefixes(got), wantCovering) {
			t.Fatalf("got %v, want %v", coversPrefixes(got), wantCovering)
		}
	})

	t.Run("an IPv4-mapped Covers asks the IPv4 question", func(t *testing.T) {
		// The other half of the family decision, and the one place the two
		// halves deliberately disagree: a MAPPED PREFIX in the table is an
		// IPv6 prefix (see covers), but a mapped ADDRESS in ?covers= is
		// something a person typed for an IPv4 address, so it is unmapped
		// and answered by the IPv4 rows. Reading it the other way would send
		// this query to the v6 side of the family test and return the
		// default route -- an answer that is wrong twice over.
		got := mustRoutes(t, ctx, q, RouteFilter{
			Covers: "::ffff:" + coversFixtureTarget, Router: router,
		})
		if !slices.Equal(coversPrefixes(got), wantCovering) {
			t.Fatalf("got %v, want %v", coversPrefixes(got), wantCovering)
		}
	})

	t.Run("an empty Covers is not a covers query", func(t *testing.T) {
		// "" means "not asked for" -- the only reading available to it,
		// since an empty string is not an address -- so the filter falls
		// back to RouteFilter.Prefix, which keeps its own literal reading
		// of "" (the empty prefix). What must NOT happen is the third
		// possibility: neither predicate rendered, and every route in
		// route_unicast returned.
		got := mustRoutes(t, ctx, q, RouteFilter{Covers: "", Router: router})
		if len(got) != 0 {
			t.Fatalf("got %v, want none -- an empty Covers with an empty Prefix "+
				"asks for the empty prefix, not for every route this router has",
				coversPrefixes(got))
		}
	})
}

// TestRoutesRejectsACoversThatIsNotAnAddress is the argument-side half of the
// predicate's totality, at the boundary a caller can actually reach.
//
// Every one of these strings is something an operator really does type into
// ?covers=, and not one of them is an address: two are prefixes (the
// parameter next to it takes those), one is a hostname-shaped typo, one
// carries an IPv6 zone that parses in Go and is not an address ClickHouse's
// own toIPv6 accepts. An error is the right answer to all of them, and it
// has to be produced HERE, in Go, for the reason checkFamily's does: the
// statement itself is total (see TestRoutesCoversPredicateRaisesOnNothing),
// so a malformed argument reaching ClickHouse comes back as the empty result
// nobody can tell from "nothing covers this address".
func TestRoutesRejectsACoversThatIsNotAnAddress(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	for _, covers := range []string{
		"not-an-address",
		"10.0.0.0/99",
		"10.77.0.0/24",
		"fe80::1%eth0",
	} {
		_, err := q.Routes(ctx, RouteFilter{Covers: covers})
		if !errors.Is(err, ErrBadFilter) {
			t.Errorf("Routes(Covers: %q) error = %v, want one wrapping ErrBadFilter", covers, err)
		}
	}
}

// TestRoutesRefusesPrefixAndCoversTogether pins api/openapi.yaml's own rule
// ("Mutually exclusive with covers", "Exactly one of prefix= or covers= is
// required"), and it is a real ambiguity rather than a tidiness rule: the two
// predicates would AND together into "the exact prefix 10.77.0.0/24, and
// also containing 10.99.0.1", which is a question no operator meant to ask
// and whose empty answer looks exactly like "that prefix is nowhere".
//
// It is enforced in check() rather than resolved by silently preferring one,
// which is the shape that would let the HTTP layer send both and never learn
// it was ignoring half of what it sent.
func TestRoutesRefusesPrefixAndCoversTogether(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	_, err := q.Routes(ctx, RouteFilter{Prefix: coversFixtureSlash24, Covers: coversFixtureTarget})
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("Routes(Prefix and Covers together) error = %v, want one wrapping ErrBadFilter", err)
	}
}

// TestRoutesCoversPredicateRaisesOnNothing is the totality claim itself,
// made against the STATEMENT rather than against Routes: whatever string
// reaches the three placeholders covers emits, ClickHouse must answer with
// rows or with none, never with an exception.
//
// Routes' own check() rejects all of these before they can be bound, so this
// test reaches past it deliberately -- it renders the predicate the way
// RouteFilter.predicates does and binds its own values. That distinction is
// the point. check() is a Go-side courtesy that a future caller (a `vantage
// query` flag, an HTTP handler, a test helper) can bypass; the statement's
// own totality is what has to hold when one does. toIPv6OrNull, never
// toIPv6, is what makes the difference: toIPv6('not-an-address') raises, and
// a raised query is a 500 on a looking-glass surface where the honest answer
// is an empty result.
//
// The last case is deliberately a VALID address, and it is what stops this
// test passing vacuously: without it, a statement that returned zero rows
// for every argument (a predicate accidentally always false, say) would look
// exactly like a total one.
func TestRoutesCoversPredicateRaisesOnNothing(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertCoversFixture(t, ctx, q)

	var f filters
	f.covers("r.prefix", netip.MustParseAddr(coversFixtureTarget))
	stmt := fmt.Sprintf(routesSQL, q.db, f.where(), f.having())

	for _, tc := range []struct {
		arg      string
		wantRows bool
	}{
		{"not-an-address", false},
		{"10.0.0.0/99", false},
		{"", false},
		// The zero netip.Addr's own String(), which is what covers binds
		// when it is handed one -- see eqAddr's doc comment for why that
		// text is a hazard, and covers' for why it binds the predicate
		// anyway rather than omitting it.
		{"invalid IP", false},
		{"'; DROP TABLE route_unicast --", false},
		{coversFixtureTarget, true},
	} {
		rows, err := q.conn.Query(ctx, stmt, tc.arg, tc.arg, tc.arg)
		if err != nil {
			t.Errorf("covers %q raised: %v", tc.arg, err)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Errorf("covers %q raised while reading rows: %v", tc.arg, err)
			continue
		}
		if got := n > 0; got != tc.wantRows {
			t.Errorf("covers %q returned %d rows, want rows = %v", tc.arg, n, tc.wantRows)
		}
	}
}

// TestRoutesReportsRowsInItsStatementsOwnOrder is the guard routesOrder did
// not have.
//
// It was found by mutation: deleting routesOrder from Routes' statement
// altogether -- leaving an unordered SELECT -- left this whole package's suite
// green. That is a pre-existing gap rather than one the ORDER BY's move into a
// const of its own created (deleting the same three clauses in place would
// have been just as invisible), but the const now carries a claim in its doc
// comment -- sysname first, so a fleet-wide answer reads router by router --
// and a claim nothing checks is the shape this package keeps finding defects
// in.
//
// It asserts MONOTONICITY under the statement's own key rather than a specific
// sequence of rows, deliberately. A written-out expected order would pin
// properties of the fixture (which peer got which path_id) alongside the one
// property under test, which is the reason coversPrefixes sorts instead of
// asserting order at all; monotonicity pins only that the answer is ordered,
// which is the whole of what routesOrder promises.
func TestRoutesReportsRowsInItsStatementsOwnOrder(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFilterFixture(t, ctx, q)

	got := mustRoutes(t, ctx, q, RouteFilter{Prefix: routeFilterFixturePrefix})
	if len(got) < 2 {
		t.Fatalf("got %d rows; this test needs at least two to say anything about "+
			"order", len(got))
	}
	key := func(r Route) []string {
		return []string{r.RouterSysName, r.PeerIP.String(), fmt.Sprint(r.PathID), r.Collector}
	}
	for i := 1; i < len(got); i++ {
		if slices.Compare(key(got[i-1]), key(got[i])) > 0 {
			t.Errorf("rows %d and %d are out of order: %v then %v -- routesOrder "+
				"sorts by (sysname, peer_ip, path_id, collector_id)",
				i-1, i, key(got[i-1]), key(got[i]))
		}
	}
	// And the fixture really does vary the leading columns, so the assertion
	// above is not passing over rows that were all equal anyway.
	if slices.Compare(key(got[0]), key(got[len(got)-1])) == 0 {
		t.Errorf("every row shares the ordering key %v; this fixture cannot tell an "+
			"ordered answer from an unordered one", key(got[0]))
	}
}

// nullableU32 renders a *uint32 for a failure message so that "absent" and
// "0" read differently in the output, which is the whole distinction MED and
// LocalPref exist to carry. A %v of a nil pointer prints "<nil>" and a %v of
// a non-nil one prints an address, so neither is usable here.
func nullableU32(p *uint32) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprintf("%d", *p)
}

// sameNullableU32 compares two *uint32 by their VALUES, treating nil as its
// own value rather than dereferencing it. want is a *uint32 rather than a
// (uint32, bool) pair so that a test asserting "absent" cannot accidentally
// spell it as 0.
func sameNullableU32(got, want *uint32) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

// TestCommunityStrings holds communityStrings without a database, which is
// half the reason the conversion is in Go at all (see its own doc comment).
//
// The cases are the boundaries a 32-bit-to-two-16-bit-halves split can be got
// wrong at, not a sample: a low half at its maximum, a high half at its
// maximum, each half at zero while the other is not, and the all-bits value.
// A rendering that shifted arithmetically, masked to the wrong width, or
// swapped the halves fails at least one of them, and the RFC 1997 well-known
// values are in here as numbers precisely because this function must not
// start naming them.
func TestCommunityStrings(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []uint32
		want []string
	}{
		{"the contract's own example", []uint32{65000<<16 | 100}, []string{"65000:100"}},
		{"NO_EXPORT, as numbers", []uint32{0xFFFFFF01}, []string{"65535:65281"}},
		{"a zero high half", []uint32{300}, []string{"0:300"}},
		{"a zero low half", []uint32{65002 << 16}, []string{"65002:0"}},
		{"both halves at their maximum", []uint32{0xFFFFFFFF}, []string{"65535:65535"}},
		{"zero", []uint32{0}, []string{"0:0"}},
		{
			"several, in the order received",
			pathAttrCommunities, pathAttrCommunityText,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := communityStrings(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("communityStrings(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	// The empty case is asserted on the SHAPE of the result, not on its
	// length: api/openapi.yaml has communities in RouteCommon.required and
	// types it as an array, and a nil slice marshals to JSON null rather than
	// []. len() == 0 is true of both, which is exactly why it is not the
	// assertion here.
	got := communityStrings(nil)
	if got == nil {
		t.Errorf("communityStrings(nil) returned a nil slice; the contract types " +
			"communities as an array, and a nil slice encodes as null, not []")
	}
	if len(got) != 0 {
		t.Errorf("communityStrings(nil) = %v, want empty", got)
	}
}

// TestRoutesReportsPathAttributes is the unicast half of the four attributes
// api/openapi.yaml's RouteCommon requires of every route type -- med,
// local_pref, communities and large_communities -- none of which any route
// type carried until this change, though every route table has had the
// columns since the schema was written.
//
// The three subtests are three different failures, not three samples. The
// first is a transposition guard: a positional Scan against a fifteen-column
// SELECT is exactly where two adjacent same-typed columns swap without a word
// (see scanRoutes' own doc comment), and med/local_pref are such a pair, as
// are as_path/communities. The second and third are the two halves of the
// nullable question -- a present zero, and an absent value that was once
// present -- and the third is the one that fails against the obvious
// implementation rather than against a careless one.
func TestRoutesReportsPathAttributes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPathAttrFixture(t, ctx, q)

	t.Run("every attribute round-trips, and no two of them can be swapped", func(t *testing.T) {
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: pathAttrPrefixAll})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		// Family first: it is a new SELECT position sitting between two other
		// String columns (rib and prefix), so a transposition here is silent
		// in a way a numeric one is not. All three values below are
		// different, which is what makes that assertion mean anything.
		if r.RIB != "in_pre" || r.Family != "ipv4u" || r.Prefix != pathAttrPrefixAll {
			t.Errorf("got RIB=%q Family=%q Prefix=%q, want %q, %q, %q -- three "+
				"adjacent String columns, so a transposed pair reads as an "+
				"ordinary row", r.RIB, r.Family, r.Prefix, "in_pre", "ipv4u", pathAttrPrefixAll)
		}
		wantMED, wantLocalPref := uint32(pathAttrMED), uint32(pathAttrLocalPref)
		if !sameNullableU32(r.MED, &wantMED) || !sameNullableU32(r.LocalPref, &wantLocalPref) {
			t.Errorf("got MED=%s LocalPref=%s, want %d and %d -- the two are "+
				"adjacent Nullable(UInt32) columns and a swap between them "+
				"raises nothing", nullableU32(r.MED), nullableU32(r.LocalPref),
				pathAttrMED, pathAttrLocalPref)
		}
		if !slices.Equal(r.Communities, pathAttrCommunityText) {
			t.Errorf("Communities = %v, want %v -- rendered from the stored "+
				"Array(UInt32) in Go, in the order received",
				r.Communities, pathAttrCommunityText)
		}
		if !slices.Equal(r.LargeCommunities, pathAttrLargeCommunities) {
			t.Errorf("LargeCommunities = %v, want %v -- already text in the "+
				"column, so query/ passes it through untouched",
				r.LargeCommunities, pathAttrLargeCommunities)
		}
		// as_path and communities are the other same-typed pair in this
		// SELECT list (both Array(UInt32)), so the AS path is asserted here
		// too rather than left to the subtests that read it elsewhere.
		if !slices.Equal(r.ASPath, pathAttrASPath) || r.OriginASN != pathAttrASPath[len(pathAttrASPath)-1] {
			t.Errorf("got ASPath=%v OriginASN=%d, want %v and %d",
				r.ASPath, r.OriginASN, pathAttrASPath, pathAttrASPath[len(pathAttrASPath)-1])
		}
		if r.NextHop.String() != "10.9.120.1" || r.RouterSysName != pathAttrFixtureSysname {
			t.Errorf("got NextHop=%s RouterSysName=%q, want 10.9.120.1 and %q",
				r.NextHop, r.RouterSysName, pathAttrFixtureSysname)
		}
	})

	t.Run("a MED of zero is present and an absent local-pref is absent", func(t *testing.T) {
		// The row carries MED 0 and no local-pref at all, so this one
		// assertion fails in both directions: a Route that defaulted its way
		// past NULL reads 0 for both, which is right for one field and wrong
		// for the other, and a Route that treated 0 as absent reads absent
		// for both, which is wrong for the other one.
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: pathAttrPrefixZeroMED})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		zero := uint32(0)
		if !sameNullableU32(got[0].MED, &zero) {
			t.Errorf("MED = %s, want 0 -- MED 0 is a real value a router "+
				"advertises and it wins tie-breaks against MED 10; it is not "+
				"a way of saying the attribute is missing", nullableU32(got[0].MED))
		}
		if got[0].LocalPref != nil {
			t.Errorf("LocalPref = %s, want absent -- this row's local_pref "+
				"column is NULL, and a route with no local-pref is not a "+
				"route with local-pref 0", nullableU32(got[0].LocalPref))
		}
	})

	t.Run("a cleared MED reads as absent, not as the value it used to have", func(t *testing.T) {
		// The fixture advertises this prefix with a MED, a local-pref and
		// communities, then re-advertises it at a higher seq with none of
		// them. ClickHouse's aggregates SKIP a NULL argument, so
		// argMax(med, (seq, stream_seq)) resolves to the newest NON-NULL
		// observation and reports pathAttrClearedMED here -- a value this
		// route stopped carrying, reported as its current state, with nothing
		// anywhere to say so. argMax(tuple(med), ...).1 is what makes the
		// newest observation the answer. Measured, not assumed: see
		// routesSQL's own doc comment.
		got := mustRoutes(t, ctx, q, RouteFilter{Prefix: pathAttrPrefixCleared})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		if r.MED != nil || r.LocalPref != nil {
			t.Errorf("got MED=%s LocalPref=%s, want both absent -- the newest "+
				"observation of this route carries neither, and %d is what the "+
				"one before it carried",
				nullableU32(r.MED), nullableU32(r.LocalPref), pathAttrClearedMED)
		}
		// The array columns are not nullable and so were never at risk from
		// the same mechanism, which is exactly why they are asserted here:
		// they pin that the newest observation is the one being read at all,
		// independently of how the nullable pair is aggregated.
		if len(r.Communities) != 0 || len(r.LargeCommunities) != 0 {
			t.Errorf("got Communities=%v LargeCommunities=%v, want both empty -- "+
				"the newest observation of this route carries no communities",
				r.Communities, r.LargeCommunities)
		}
	})
}

// TestRoutesReturnsExtCommunitiesAndRouteTargets: route_unicast has carried
// both columns since the schema was written, and 447 of the archive's 8,611
// rows populate ext_communities -- but Route declared neither field and the
// statement selected neither, so the API hid data the archive holds. The
// community= filter cannot search a column the response omits, so this
// is a prerequisite rather than a nicety.
func TestRoutesReturnsExtCommunitiesAndRouteTargets(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertExtCommunityFixture(t, ctx, q)

	got, err := q.Routes(ctx, RouteFilter{Prefix: extCommFixturePrefix})
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d routes, want 1", len(got))
	}
	if !slices.Equal(got[0].ExtCommunities, []string{"rt:65101:1", "soo:65000:777"}) {
		t.Errorf("ExtCommunities = %v, want [rt:65101:1 soo:65000:777]",
			got[0].ExtCommunities)
	}
	if !slices.Equal(got[0].RouteTargets, []string{"65101:1"}) {
		t.Errorf("RouteTargets = %v, want [65101:1]", got[0].RouteTargets)
	}
}

// TestRoutesFiltersByOriginASN, and the reason the fixture has three rows:
// one route originated by the AS asked for, one by another AS, and one with
// an EMPTY AS path. The third is the important one -- 1,839 of the archive's
// 8,611 unicast rows have no AS path, query.originASN returns 0 for them and
// the wire renders origin_asn: null, so a filter that matched them would
// contradict the field it filters on.
//
// Every call below sets Prefix alongside OriginASN: RouteFilter.Prefix's
// own doc comment (and TestRoutes' "an end-of-rib marker is not a route"
// subtest) establish that
// its zero value renders as an exact match against the literal empty
// string, not "every prefix" -- that reading belongs to VPNRouteFilter and
// EVPNRouteFilter alone. A RouteFilter setting only OriginASN could ask
// ClickHouse for the route AT THE EMPTY PREFIX whose origin is 65100, which
// route_unicast has never had one of, in this fixture or any other --
// confirmed directly against the table (a raw count of route_unicast rows
// whose prefix column is the empty string comes back 0), not inferred.
//
// predicates() no longer renders that empty-prefix predicate when Prefix is
// unset and a wide filter is set (see wideAsked and
// TestRouteFilterWideAloneMatchesWithoutPrefix, the regression test for the
// fix), so setting Prefix here is no longer required for this query to
// match anything. It stays, deliberately, because pairing OriginASN with an
// exact prefix is a different and equally real question -- "this prefix,
// narrowed to that AS's live announcement of it" -- and this test's own
// premise (originFixtureMatch and originFixtureOther share nothing but
// their origin AS) needs Prefix to tell the two rows apart in the first
// place.
func TestRoutesFiltersByOriginASN(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)

	if got := mustRoutes(t, ctx, q, RouteFilter{Prefix: originFixtureMatch, OriginASN: 65100}); len(got) != 1 {
		t.Errorf("origin_asn=65100 against %s (whose real origin is 65100) "+
			"returned %d routes, want 1", originFixtureMatch, len(got))
	}
	if got := mustRoutes(t, ctx, q, RouteFilter{Prefix: originFixtureOther, OriginASN: 65100}); len(got) != 0 {
		t.Errorf("origin_asn=65100 against %s (whose real origin is 65200) "+
			"returned %d routes, want 0", originFixtureOther, len(got))
	}
}

// TestRoutesOriginASNIgnoresAnEmptyASPath states the same rule from the
// other side: no origin filter value may ever return the fixture's
// no-AS-path route, whatever AS is asked for -- 65100 and 65200 are real
// origins elsewhere in this fixture, and 1 is neither, so none of the three
// is special-cased by the guard.
func TestRoutesOriginASNIgnoresAnEmptyASPath(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)

	for _, asn := range []uint32{65100, 65200, 1} {
		got, err := q.Routes(ctx, RouteFilter{Prefix: originFixtureNoAS, OriginASN: asn})
		if err != nil {
			t.Fatalf("Routes(origin=%d): %v", asn, err)
		}
		if len(got) != 0 {
			t.Errorf("origin_asn=%d matched %s, which has no AS path; its "+
				"origin is unknown and the wire renders it null", asn, originFixtureNoAS)
		}
	}
}

// TestRoutesFiltersByThroughASN: an AS in the middle of the path matches,
// which origin_asn must not.
func TestRoutesFiltersByThroughASN(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)

	through := mustRoutes(t, ctx, q, RouteFilter{Prefix: originFixtureMatch, ThroughASN: 64500})
	if len(through) != 1 {
		t.Fatalf("through_asn=64500 against %s returned %d routes, want 1 -- "+
			"64500 is the transit AS in the fixture's path", originFixtureMatch, len(through))
	}
	origin := mustRoutes(t, ctx, q, RouteFilter{Prefix: originFixtureMatch, OriginASN: 64500})
	if len(origin) != 0 {
		t.Errorf("origin_asn=64500 against %s matched %d routes; 64500 is a "+
			"transit AS, not an origin", originFixtureMatch, len(origin))
	}
}

// TestRouteFilterWideAloneMatchesWithoutPrefix is the regression test for
// a critical defect: RouteFilter.predicates() used to render `r.prefix = ?`
// bound to the empty string unconditionally whenever Covers was absent, so
// a wide filter (OriginASN, ThroughASN or Community) with no Prefix and no Covers beside
// it was silently unanswerable -- measured against the live archive,
// RouteFilter{OriginASN: 64512} returned zero rows where AS 64512 genuinely
// originates 3,250 of them across 501 prefixes. That made this feature's
// headline capability, a wide filter asked alone, non-functional: an empty
// array indistinguishable from a correct one.
//
// It is the test every earlier test in this file could not be, by
// construction: TestRoutesFiltersByOriginASN and
// TestRoutesFiltersByThroughASN, immediately above, both set Prefix
// alongside their wide filter (see the former's own comment for why that was
// itself the right call, against a different hazard), and two other
// tests -- TestCountRoutesAgreesWithRoutes and
// TestRoutesLimitCapsWithoutChangingTheCount -- use RouteFilter{ThroughASN:
// 64500} with no Prefix, which is exactly the shape that exposes this
// defect. Fixing Prefix's vacuity hazard hid this one behind it for a
// while; this is the test that sets neither Prefix nor
// Covers, on purpose.
//
// Confirmed to fail before the fix: reverting RouteFilter.predicates() to
// its unconditional `f.eq("r.prefix", rf.Prefix)` makes the first subtest
// below fail with "returned 0 routes, want 1".
func TestRouteFilterWideAloneMatchesWithoutPrefix(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)

	t.Run("a wide filter alone still matches", func(t *testing.T) {
		got, err := q.Routes(ctx, RouteFilter{OriginASN: 65100})
		if err != nil {
			t.Fatalf("Routes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("RouteFilter{OriginASN: 65100}, with no Prefix and no Covers, "+
				"returned %d routes, want 1 (%s) -- before the fix this rendered "+
				"r.prefix = '' unconditionally, and route_unicast has never held a "+
				"row with an empty prefix", len(got), originFixtureMatch)
		}
		if got[0].Prefix != originFixtureMatch {
			t.Errorf("matched prefix %s, want %s", got[0].Prefix, originFixtureMatch)
		}
	})

	t.Run("an explicit empty Prefix is preserved", func(t *testing.T) {
		// RouteFilter{Prefix: ""} must still mean the empty prefix -- a real
		// question with a real (empty) answer -- and must not be
		// reinterpreted as "not asked" merely because the fix above exists.
		// None of insertOriginFixture's rows carry an empty prefix (its own
		// comment gives the coordinates), so the answer is empty either way;
		// what this subtest pins is that predicates() still renders the
		// equality against "" for this shape rather than omitting it, the
		// way TestRoutes' "an end-of-rib marker is not a route" subtest
		// already pins it against a different fixture. This is that same
		// claim, checked again on the rows this test itself just wrote, so
		// the regression above and the preserved behavior below are both
		// asserted against one fixture rather than two.
		got, err := q.Routes(ctx, RouteFilter{Prefix: ""})
		if err != nil {
			t.Fatalf("Routes: %v", err)
		}
		fixtureRouter := netip.MustParseAddr("10.0.0.170") // insertOriginFixture's own router
		for _, r := range got {
			if r.RouterIP == fixtureRouter {
				t.Errorf("Routes(RouteFilter{Prefix: \"\"}) returned %+v from the "+
					"origin fixture's router, which wrote no empty-prefix row", r)
			}
		}
	})
}

// TestRoutesOriginASNReadsTheLIVEPath is the reason these filters are
// HAVING predicates and not WHERE ones, made a test: a route
// whose newest observation changed its AS path must be judged by the new path
// alone. A WHERE would remove the newest row from the group, and argMax would
// then report the older observation as live.
func TestRoutesOriginASNReadsTheLIVEPath(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRepathedFixture(t, ctx, q)

	stale, err := q.Routes(ctx, RouteFilter{Prefix: repathPrefix, OriginASN: repathOldOrigin})
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("origin_asn=%d returned %d routes. That AS originated this "+
			"prefix in an EARLIER observation only; the live path ends in %d. "+
			"A WHERE-based filter passes this by accident and reports the "+
			"stale attributes as current",
			repathOldOrigin, len(stale), repathNewOrigin)
	}
	live, err := q.Routes(ctx, RouteFilter{Prefix: repathPrefix, OriginASN: repathNewOrigin})
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("origin_asn=%d returned %d routes, want 1", repathNewOrigin, len(live))
	}
}

// TestRoutesFiltersByCommunity runs the dispatch against real rows rather
// than against ParseCommunity's return value: the packing, the column
// aliases and the HAVING placement all have to agree, and only a live query
// checks all three at once.
func TestRoutesFiltersByCommunity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertExtCommunityFixture(t, ctx, q)

	for _, tc := range []struct {
		name, value string
		want        int
	}{
		{"by route target, named", "rt:65101:1", 1},
		{"by route target, stripped", "65101:1", 1},
		{"by a non-rt extended community", "soo:65000:777", 1},
		{"a community the fixture does not carry", "65000:1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.Routes(ctx, RouteFilter{
				Prefix: extCommFixturePrefix, Community: tc.value,
			})
			if err != nil {
				t.Fatalf("Routes: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("community=%s returned %d routes, want %d",
					tc.value, len(got), tc.want)
			}
		})
	}
}

// TestRoutesCommunityFilterReadsTheLIVEValue is the community counterpart of
// TestRoutesOriginASNReadsTheLIVEPath, run against the community channel
// instead of the AS path: repathCommunityFixture writes ONE route key
// observed twice, and its live communities set replaces (not adds to) the
// superseded one, exactly as a router re-advertising a route with a changed
// community policy actually does.
//
// This hazard is not a hypothetical: route
// 10.91.1.0/24 in the live archive carries a stale community pair
// ([4259971172, 4259971272]) against a live pair
// ([4259971372, 4259971472]), and a community filter built on a WHERE
// predicate reports the stale pair as current -- see
// filters.havings' own doc comment. A WHERE would
// remove the newest row from route_unicast's argMax group before it runs, so
// the aggregate would then report the SUPERSEDED communities as if they were
// live.
func TestRoutesCommunityFilterReadsTheLIVEValue(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRepathedCommunityFixture(t, ctx, q)

	stale, err := q.Routes(ctx, RouteFilter{
		Prefix: repathCommunityPrefix, Community: repathCommunityOld,
	})
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("community=%s returned %d routes. That community was carried in "+
			"an EARLIER observation only; the live community set is %s. A "+
			"WHERE-based filter passes this by accident and reports the stale "+
			"community as current", repathCommunityOld, len(stale), repathCommunityLive)
	}

	live, err := q.Routes(ctx, RouteFilter{
		Prefix: repathCommunityPrefix, Community: repathCommunityLive,
	})
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("community=%s returned %d routes, want 1", repathCommunityLive, len(live))
	}
}

// TestCountRoutesAgreesWithRoutes: the count is what stops a capped answer
// being read as a complete one, so it has to be the same predicate. A count
// that drifted from the query it describes would be worse than no count --
// it would be a confident wrong total.
//
// It filters on ThroughASN alone, no Prefix and no Covers: exactly the shape
// TestRouteFilterWideAloneMatchesWithoutPrefix exists to prove RouteFilter
// can answer at all. If this filter matched nothing, the "the fixture
// matched nothing" guard below would be the only thing this test ever
// actually exercised -- a vacuous pass wearing a real assertion's clothes.
// As it is, ThroughASN=64500 matches both of
// insertOriginFixture's non-empty-path rows (originFixtureMatch and
// originFixtureOther both carry 64500 as their transit AS).
func TestCountRoutesAgreesWithRoutes(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)

	f := RouteFilter{ThroughASN: 64500}
	rows, err := q.Routes(ctx, f)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	n, err := q.CountRoutes(ctx, f)
	if err != nil {
		t.Fatalf("CountRoutes: %v", err)
	}
	if n != uint64(len(rows)) {
		t.Errorf("CountRoutes = %d but Routes returned %d rows", n, len(rows))
	}
	if n == 0 {
		t.Fatal("the fixture matched nothing, so this comparison is vacuous")
	}
}

// TestRoutesLimitCapsWithoutChangingTheCount: Limit truncates the rows and
// must not touch what CountRoutes reports, or the caller cannot tell how
// partial the answer is. Same fixture and filter as
// TestCountRoutesAgreesWithRoutes, for the same "wide filter alone" reason;
// the len(all) < 2 guard below is what turns a regression back to the
// pre-fix zero-rows behavior into a loud failure rather than a silently
// vacuous "Limit 1 returned 0 rows, want 1" pass.
func TestRoutesLimitCapsWithoutChangingTheCount(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)

	f := RouteFilter{ThroughASN: 64500}
	all, err := q.Routes(ctx, f)
	if err != nil {
		t.Fatalf("Routes: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("fixture has %d matching routes; need at least 2 to cap", len(all))
	}

	f.Limit = 1
	capped, err := q.Routes(ctx, f)
	if err != nil {
		t.Fatalf("Routes(limit 1): %v", err)
	}
	if len(capped) != 1 {
		t.Errorf("Limit 1 returned %d rows", len(capped))
	}
	n, err := q.CountRoutes(ctx, f)
	if err != nil {
		t.Fatalf("CountRoutes: %v", err)
	}
	if n != uint64(len(all)) {
		t.Errorf("CountRoutes with Limit set = %d, want the unlimited total %d. "+
			"The count answers 'how much did you not show me'", n, len(all))
	}
}

// TestRoutesExcludeAPeerWhoseViewWasLost is the query half of the stale-view
// fix. The collector emits a view-lost event when a BMP transport drops
// (collector's Session.Close); this is what makes that event mean something
// to a reader. Without it the routes below stay live in every current-state
// answer for as long as the archive keeps them, because no newer session
// exists to displace the dead one.
func TestRoutesExcludeAPeerWhoseViewWasLost(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertViewLostFixture(t, ctx, q)

	if got := mustRoutes(t, ctx, q, RouteFilter{Prefix: viewLostPrefix}); len(got) != 0 {
		t.Errorf("got %+v, want none -- the collector lost its view of the "+
			"advertising peer, so this route is not something anyone can "+
			"still observe", got)
	}
	// The control. A view-lost peer must not take the session's other peers
	// with it: the transport carried them all, but only peers that were up
	// when it dropped are affected, and this one's own state is unchanged.
	got := mustRoutes(t, ctx, q, RouteFilter{Prefix: viewLostUpPfx})
	if len(got) != 1 {
		t.Fatalf("got %d rows for the still-up peer, want 1: %+v", len(got), got)
	}
}
