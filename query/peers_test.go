package query

import (
	"fmt"
	"maps"
	"net/netip"
	"strings"
	"testing"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
	"time"
)

// TestFlappedPeerFixtureDefeatsBothNaivePeerStateQueries asserts the
// adversarial property insertFlappedPeerFixture's own doc comment claims,
// against the fixture's raw rows rather than against Peers: if this test
// fails, the fixture itself is not adversarial enough and no amount of
// fixing Peers will make the behavior test below mean anything.
func TestFlappedPeerFixtureDefeatsBothNaivePeerStateQueries(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFlappedPeerFixture(t, ctx, q)

	// Naive #1: "a peer that ever went down is down." Scoped to
	// fixtureRouterIP, not the whole peer_events table: chtest shares one
	// database across every test in this package (see this rule spelled
	// out in query/fixtures_test.go), and a separate down-peer fixture
	// (routeCountFixtureDownPeer) adds a third peer
	// that has ever gone down elsewhere in this same database. Unscoped,
	// this assertion is not "insertFlappedPeerFixture wrote two down
	// events", it is "this package has written exactly two down events in
	// total" -- true only by accident of which fixtures happened to exist
	// yet, and already false once that other fixture runs first.
	var everDown uint64
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT uniqExact(peer_ip) FROM %s.peer_events WHERE kind = 'down' AND router_ip = ?`,
		q.db), fixtureRouterIP.String()).Scan(&everDown); err != nil {
		t.Fatal(err)
	}
	if everDown != 2 {
		t.Fatalf("fixture is not adversarial: %d peers ever went down, want 2 "+
			"(one that stayed down, one that came back)", everDown)
	}

	// Naive #2: "the last row inserted wins." Scoped to fixtureRouterIP too,
	// for the identical reason naive #1 above is: peer_ip alone is not
	// unique to this fixture across the whole package, and any other
	// fixture that ever writes a peer_events row for ::ffff:10.0.0.3 under
	// a different router would make this assertion about that fixture's
	// insertion order rather than this one's.
	var lastInserted string
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT kind FROM %s.peer_events WHERE peer_ip = toIPv6('::ffff:10.0.0.3')
		 AND router_ip = ? ORDER BY stream_seq DESC LIMIT 1`, q.db),
		fixtureRouterIP.String()).Scan(&lastInserted); err != nil {
		t.Fatal(err)
	}
	if lastInserted == "up" {
		t.Fatal("fixture is not adversarial: insertion order matches seq order, " +
			"so a query that sorts by the wrong column still passes")
	}
}

// TestPeersReportsTheLastStateInTheSessionNotTheLastSeen pins a gap: the
// live archive has no currently-down peer to prove "a down peer's routes
// are excluded" against, so this test manufactures one.
// 10.0.0.3 is the case that actually distinguishes "state the session ended
// in" from "state last written" or "was ever down"; see
// insertFlappedPeerFixture's doc comment for why.
func TestPeersReportsTheLastStateInTheSessionNotTheLastSeen(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFlappedPeerFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: fixtureRouterIP})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	want := map[string]string{"10.0.0.1": "up", "10.0.0.2": "down", "10.0.0.3": "up"}
	if len(got) != len(want) {
		t.Fatalf("Peers returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		if w := want[p.PeerIP.String()]; p.State != w {
			t.Errorf("peer %s: State = %q, want %q", p.PeerIP, p.State, w)
		}
		// insertFlappedPeerFixture writes every row with RIB "in_pre";
		// this is the package's first test to scan the rib column at all,
		// so without this check nothing pins the Enum8-to-string mapping
		// to the literal Peers' own callers (and this package's other
		// fixtures) depend on.
		if p.RIB != "in_pre" {
			t.Errorf("peer %s: RIB = %q, want %q", p.PeerIP, p.RIB, "in_pre")
		}
		// 10.0.0.2 is down and was never given any route or marker rows,
		// so its DumpStates must be empty -- not the stale "still dumping"
		// a plain boolean reported for a disconnected peer, the reason
		// Dumping was replaced by the per-family DumpStates map.
		// 10.0.0.1 and 10.0.0.3 are up but also carry no route or marker
		// rows, so they must be empty too, and for a different reason:
		// there is no family for the map to have an opinion about. "Up
		// with nothing on record" and "down" produce the same empty map
		// here; TestPeersReportsDumpStateForADownPeerWithACompleteDump is
		// what tells the two branches apart.
		if len(p.DumpStates) != 0 {
			t.Errorf("peer %s: DumpStates = %v, want an empty map", p.PeerIP, p.DumpStates)
		}
	}
}

// TestPeersFiltersByRouterIP guards PeerFilter.Router itself: a zero-value
// netip.Addr can't be bound to ClickHouse the way a valid one can (see
// eqAddr's own comment on why), so this is the test that would catch a
// regression where the zero case stopped matching "all routers" -- for
// instance a `Router.String()` call on an invalid Addr slipping into the
// bound parameter, which renders as the literal string "invalid IP" rather
// than "::" and would make every row vanish from an unfiltered call, not
// just fail to filter one.
func TestPeersFiltersByRouterIP(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFlappedPeerFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: fixtureRouterIP})
	if err != nil {
		t.Fatalf("Peers(fixtureRouterIP): %v", err)
	}
	for _, p := range got {
		if p.RouterIP != fixtureRouterIP {
			t.Errorf("Peers(fixtureRouterIP) returned a row for router %s", p.RouterIP)
		}
	}
	if len(got) != 3 {
		t.Fatalf("Peers(fixtureRouterIP) returned %d rows, want 3: %+v", len(got), got)
	}

	all, err := q.Peers(ctx, PeerFilter{Router: netip.Addr{}})
	if err != nil {
		t.Fatalf("Peers(zero): %v", err)
	}
	var sawFixtureRouter bool
	for _, p := range all {
		if p.RouterIP == fixtureRouterIP {
			sawFixtureRouter = true
		}
	}
	if !sawFixtureRouter {
		t.Fatal("Peers(zero) did not include fixtureRouterIP's rows -- the zero " +
			"Router should mean \"every router\", not \"no router\"")
	}
}

// TestPeersReportsDumpState covers the two "the peer is up" values a family
// can take: a peer whose session carries an eor_events marker for its
// (router, peer, rib, family) has finished that family's initial dump
// ("complete"); a peer with route rows but no such marker has not
// ("dumping"). Both fixtures' peers carry ipv4u alone, so each map has
// exactly one entry -- maps.Equal, not a lookup, so a query that invents
// extra families for this peer fails here rather than passing on the one
// key the test remembered to check. See insertDumpStateFixture's doc
// comment for 10.0.0.12, the fixture's third peer, which this test does not
// check -- that assertion belongs to
// TestPeersReportsDumpStateForADownPeerWithACompleteDump below, kept
// separate so a failure there cannot be misread as a complete/dumping mixup.
func TestPeersReportsDumpState(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertDumpStateFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: dumpStateFixtureRouterIP})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	want := map[string]map[string]string{
		"10.0.0.10": {"ipv4u": "complete"},
		"10.0.0.11": {"ipv4u": "dumping"},
	}
	for peerIP, w := range want {
		found := false
		for _, p := range got {
			if p.PeerIP.String() != peerIP {
				continue
			}
			found = true
			if !maps.Equal(p.DumpStates, w) {
				t.Errorf("peer %s: DumpStates = %v, want %v", peerIP, p.DumpStates, w)
			}
		}
		if !found {
			t.Errorf("Peers did not return a row for %s: %+v", peerIP, got)
		}
	}
}

// TestPeersReportsDumpStatePerFamily is the payoff of the eor_events split:
// before it, end_of_rib existed only on route_unicast, so a peer carrying
// only VPN or EVPN routes read "unknown" forever no matter how complete its
// dump was. There was no way to ask the question, so the answer was always
// an honest shrug.
//
// insertPerFamilyDumpFixture's peer carries all three shapes at once -- see
// its doc comment for why one peer with three families is the only fixture
// that can tell "per family" apart from "per peer, labeled with a family",
// and why ipv4u's absence is an assertion rather than an omission.
func TestPeersReportsDumpStatePerFamily(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPerFamilyDumpFixture(t, ctx, q)

	peers, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(perFamilyDumpFixtureRouterIP)})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1: %+v", len(peers), peers)
	}
	got := peers[0].DumpStates
	want := map[string]string{"evpn": "complete", "vpn4": "dumping"}
	if !maps.Equal(got, want) {
		t.Errorf("DumpStates = %v, want %v (ipv4u must be ABSENT, not "+
			"\"unknown\": the session carried no ipv4u rows at all, so there "+
			"is no dump to be part-way through)", got, want)
	}
}

// TestPeersReportsDumpStateForAnEVPNOnlyPeerStillDumping is the other half
// of the per-family payoff, and the only test that makes route_evpn's own
// contribution to the family universe load-bearing: this peer's session
// carries EVPN routes and no marker at all, so "evpn": "dumping" can only
// come from route_evpn itself. Deleting that branch of dump_families' union
// leaves this peer reporting an empty map -- an EVPN-only peer with no
// answer about its own dump, which is the exact state before eor_events
// existed. TestPeersReportsDumpStatePerFamily cannot catch that: its evpn
// family has a marker, so it reaches the map through eor_events regardless.
func TestPeersReportsDumpStateForAnEVPNOnlyPeerStillDumping(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertEvpnOnlyDumpFixture(t, ctx, q)

	peers, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(evpnOnlyDumpFixtureRouterIP)})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1: %+v", len(peers), peers)
	}
	got := peers[0].DumpStates
	want := map[string]string{"evpn": "dumping"}
	if !maps.Equal(got, want) {
		t.Errorf("DumpStates = %v, want %v -- route_evpn has no family column, "+
			"so the family universe has to supply the literal 'evpn' for its "+
			"rows, spelled the way eor_events spells it", got, want)
	}
}

// TestPeersReportsDumpStateForADownPeerWithACompleteDump is the
// regression test for a down peer whose current session carries a
// complete-dump marker, carried forward to the per-family map:
// 10.0.0.12 is down, and its current session carries an eor_events marker
// from before it went down. DumpStates must be EMPTY -- a disconnected
// peer's dump progress is not a meaningful question, whatever its markers
// say -- not {"ipv4u": "complete"}, which is what a query that consults
// family state first reports for a peer that is not connected. The
// down-peer check running FIRST, before any family logic, is what keeps
// this passing; see dumpStatesExpr.
func TestPeersReportsDumpStateForADownPeerWithACompleteDump(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertDumpStateFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: dumpStateFixtureRouterIP})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	for _, p := range got {
		if p.PeerIP.String() != "10.0.0.12" {
			continue
		}
		if p.State != "down" {
			t.Fatalf("fixture is not adversarial: 10.0.0.12's State = %q, want %q", p.State, "down")
		}
		if len(p.DumpStates) != 0 {
			t.Errorf("peer 10.0.0.12: DumpStates = %v, want an empty map -- a "+
				"down peer is never complete or dumping in any family, even "+
				"with an end-of-RIB marker on record", p.DumpStates)
		}
		return
	}
	t.Fatalf("Peers did not return a row for 10.0.0.12: %+v", got)
}

// TestPeersEVPNFamilyLiteralMatchesSubjectsFamilyToken guards the one
// coupling peersSQL cannot express in Go: route_evpn has no family column
// -- an EVPN table holds exactly one family -- so dump_families' route_evpn
// branch supplies the family name as a bare SQL literal. That literal has
// to be the same token sink writes into eor_events.family for an EVPN
// End-of-RIB, or a peer's EVPN routes and its EVPN marker land under two
// different map keys: the routes' key reads "dumping" forever and the
// marker's reads "complete" with no routes attached, for every EVPN peer in
// the fleet, with no error anywhere.
//
// This is the third untethered copy of the same registry, and the repo has
// already been bitten by it once at the second: see sink/rows_test.go's
// TestFamilyNameMatchesSubjectsFamilyToken, whose doc comment describes the
// identical silent failure ("the column silently reads the fallback
// 'x25-70' while the subject token reads 'evpn'"). That test pins sink's
// own familyName to subjects.FamilyToken; this one pins peersSQL's literal
// to the same source, so the two ends of the join are transitively tied to
// one registry rather than to each other.
//
// A behavior test cannot do this job, and it is worth saying why rather
// than leaving it to be rediscovered: every EVPN fixture in this package
// writes route_evpn rows directly and spells "evpn" in its own want map, so
// renaming familyNames[{AFI: 25, SAFI: 70}] leaves the fixture, the want
// map and this literal all still agreeing with each other while production
// writes the new token into eor_events.family. The fixtures are not
// downstream of the registry; only this assertion is.
//
// It checks the SQL text, the same defense
// TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree already uses for
// any(sid) versus cur's GROUP BY, and query/ takes its first internal
// import to do it -- subjects reaches only bgp and bmp, so nothing here can
// close a cycle.
func TestPeersEVPNFamilyLiteralMatchesSubjectsFamilyToken(t *testing.T) {
	want := subjects.FamilyToken(bgp.Family{AFI: 25, SAFI: 70})
	if !strings.Contains(peersSQL, "'"+want+"' AS fam") {
		t.Fatalf("peersSQL does not spell route_evpn's family as %q "+
			"(looked for `'%s' AS fam`): subjects.FamilyToken is what sink "+
			"writes into eor_events.family, so a literal that disagrees with "+
			"it puts an EVPN peer's routes and its End-of-RIB marker under "+
			"two different keys in Peer.DumpStates. Update the literal in "+
			"dump_families' route_evpn branch to match the registry, not the "+
			"other way round:\n%s", want, want, peersSQL)
	}
}

// TestPeersReportsEachCollectorsOwnView is the Peers half of
// TestRoutersDoesNotDropASecondCollectorsView: Routers aggregates
// peer_state up to one row per (collector, router), so it can only show
// that a collector's view survived at all, while Peers reports peer_state
// itself and shows WHICH peers each collector's current session actually
// carries. Both surfaces read the same cur and peer_state (see
// peerStateCTE), and peersSQL threads collector_id through three more joins
// of its own (peer_up, dump_map, route_counts), none of which can be right
// while the CTE beneath them is keyed on router alone.
//
// The two tests do not fail together on every mis-scoping, though, and that
// is worth recording rather than assuming. Widening cur's key back to
// router_ip alone fails both. Deleting peer_state's own
// "p.collector_id = cur.collector_id" join predicate fails only the Routers
// test: peersSQL INNER JOINs peer_up, whose own join to cur still carries
// that predicate, so the stale peer_state row the mutation resurrects
// (insertTwoCollectorFixture's third row, dev-c2's superseded session) finds
// no peer_up row to match and never reaches this surface at all. peer_state
// and peer_up shield each other here; Routers, which reads peer_state
// alone, is where that mutation is visible. Verified both ways.
func TestPeersReportsEachCollectorsOwnView(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertTwoCollectorFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(tcRouter)})
	if err != nil {
		t.Fatalf("Peers(%s): %v", tcRouter, err)
	}

	byCollector := map[string][]Peer{}
	for _, p := range got {
		byCollector[p.Collector] = append(byCollector[p.Collector], p)
	}
	if len(byCollector) != 2 {
		t.Fatalf("router %s reported by %d collectors, want 2: %+v",
			tcRouter, len(byCollector), got)
	}
	want := map[string]struct {
		peerIP    string
		sessionID uint64
	}{
		"dev-c1": {tcPeerC1, tcSessionC1},
		"dev-c2": {tcPeerC2, tcSessionC2},
	}
	for collector, w := range want {
		peers := byCollector[collector]
		if len(peers) != 1 {
			t.Errorf("%s reported %d peers, want 1 (%s only): %+v -- a peer "+
				"from a session this collector has already replaced is not "+
				"part of its current state", collector, len(peers), w.peerIP, peers)
			continue
		}
		if got := peers[0].PeerIP.String(); got != w.peerIP {
			t.Errorf("%s reported peer %s, want %s", collector, got, w.peerIP)
		}
		if got := peers[0].SessionID; got != w.sessionID {
			t.Errorf("%s session = %d, want %d", collector, got, w.sessionID)
		}
	}
}

// TestPeersStatementBindsEveryPlaceholder holds peersSQL to the one-`?`-one-
// value invariant TestFiltersBindsEveryPlaceholderExactlyOnce holds the
// builder to. The builder cannot enforce it here on its own: peersSQL splices
// one rendering in at seven separate positions via fmt.Sprintf's indexed
// verbs, so the statement's placeholder count and peersStatement's own
// hand-ordered argument list are two things that have to agree.
//
// Nothing else makes them agree, and that is stronger than it sounds: this
// test is not the best guard on peersSQL, it is the ONLY one. Both layers
// that would ordinarily catch a dropped insertion point are disabled by this
// statement's shape.
//
// clickhouse-go raises when a statement has more placeholders than
// arguments, but a SURPLUS argument is discarded without a word -- its
// bindPositional (v2.48.0) checks only for too few. And fmt, which does
// normally report a surplus as %!(EXTRA ...), suppresses that check for any
// format string using indexed verbs; peersSQL uses %[1]s through %[4]s
// throughout, so it is exempt. Verified directly: fmt.Sprintf("x %[1]s y
// %[3]s", "db", "unused", "outer") returns "x db y outer" with no EXTRA,
// while the same surplus against a non-indexed "x %s" returns
// "x db%!(EXTRA string=surplus)".
//
// So deleting one %[2]s from peersSQL while leaving its entry in
// peersStatement's list left the entire query suite green. That mutation is
// harmless today only because all seven values are the same address; the
// moment Peers grows a second optional filter, a mismatch would shift
// bindings and answer a question nobody asked. This test is what fails
// instead.
//
// All four cases matter, and the count is not one number any more. Each of
// the seven insertion points contributes one placeholder PER SET FIELD, so
// the router-only and rib-only shapes are both 7, both filters together are
// 14, and the empty filter is 0 -- every predicate vanishing rather than
// leaving the fourteen that an earlier "match everything" sentinel bound
// one value to. Asserting the two single-filter shapes separately, not
// just the extremes, is what catches an insertion point that renders one
// filter and forgets the other.
func TestPeersStatementBindsEveryPlaceholder(t *testing.T) {
	router := netip.MustParseAddr("10.0.0.7")
	for _, tc := range []struct {
		name string
		f    PeerFilter
		want int
	}{
		{"one router", PeerFilter{Router: router}, 7},
		{"one rib", PeerFilter{RIB: "in_pre"}, 7},
		{"router and rib", PeerFilter{Router: router, RIB: "in_pre"}, 14},
		{"every router, every rib", PeerFilter{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := peersStatement(testDB, tc.f)
			// peersSQL carries no string literal containing a question
			// mark, so counting the character is counting placeholders.
			// If one is ever added, this test is where that assumption
			// stops holding.
			n := strings.Count(sql, "?")
			if n != len(args) {
				t.Errorf("%d placeholders but %d values", n, len(args))
			}
			if n != tc.want {
				t.Errorf("%d placeholders, want %d -- peersSQL splices its "+
					"filter at seven insertion points, each of which "+
					"contributes exactly one placeholder for every field "+
					"PeerFilter actually sets and none for a field it does "+
					"not", n, tc.want)
			}
			if strings.Contains(sql, "toIPv6('::')") {
				t.Error("peersSQL still carries the \"::\" match-everything " +
					"sentinel; an absent filter must be an absent predicate")
			}
		})
	}
}

// TestPeersFiltersByRIB is ?rib= on /v1/peers, the parameter
// api/openapi.yaml has always documented and peersSQL never accepted.
//
// It reads insertRouteFilterFixture rather than a fixture of its own because
// that one is already exactly the shape this needs: router A carries peer A1
// under TWO ribs and two other peers under one each, so a rib filter selects
// a proper subset (1 of 4 for loc_rib, 3 of 4 for in_pre) rather than either
// everything or nothing. A fixture whose every peer sat under one rib would
// let this predicate be deleted with the counts unchanged.
//
// The last case names no router at all, which is the one that would catch a
// predicate built against peer_state.router_ip where peer_state.rib was
// meant: scoped to a router, such a mutation still returns that router's
// rows and only the count betrays it, while unscoped it returns every rib in
// the database.
//
// Only ONE of peersSQL's seven insertion points can fail this test -- the
// final WHERE. The other six are pure narrowing (see peersSQL's own doc
// comment), so deleting the rib predicate from any of them changes how much
// the statement reads and nothing about its answer;
// TestPeersStatementBindsEveryPlaceholder is what holds those in place.
func TestPeersFiltersByRIB(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFilterFixture(t, ctx, q)

	routerA := netip.MustParseAddr(routeFilterFixtureRouterA)

	for _, tc := range []struct {
		name string
		f    PeerFilter
		want int
	}{
		{"router alone", PeerFilter{Router: routerA}, 4},
		{"router and loc_rib", PeerFilter{Router: routerA, RIB: "loc_rib"}, 1},
		{"router and in_pre", PeerFilter{Router: routerA, RIB: "in_pre"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.Peers(ctx, tc.f)
			if err != nil {
				t.Fatalf("Peers(%+v): %v", tc.f, err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d peer rows, want %d: %+v", len(got), tc.want, got)
			}
			for _, p := range got {
				if p.RouterIP != tc.f.Router {
					t.Errorf("router %s in the result of a query filtered to %s", p.RouterIP, tc.f.Router)
				}
				if tc.f.RIB != "" && p.RIB != tc.f.RIB {
					t.Errorf("rib %q in the result of a query filtered to %q", p.RIB, tc.f.RIB)
				}
			}
		})
	}

	// rib alone, no router: the count is unassertable across a shared test
	// database, so this asserts the two things that are true regardless --
	// every row carries the rib asked for, and the fixture's own loc_rib
	// row is among them. Without the second half a predicate that matched
	// nothing at all would pass.
	t.Run("rib alone, across every router", func(t *testing.T) {
		got, err := q.Peers(ctx, PeerFilter{RIB: "loc_rib"})
		if err != nil {
			t.Fatalf("Peers(rib only): %v", err)
		}
		var sawFixturePeer bool
		for _, p := range got {
			if p.RIB != "loc_rib" {
				t.Errorf("peer %s/%s: RIB = %q in the result of a query filtered to loc_rib",
					p.RouterIP, p.PeerIP, p.RIB)
			}
			if p.RouterIP == routerA && p.PeerIP.String() == routeFilterFixturePeerA1 {
				sawFixturePeer = true
			}
		}
		if !sawFixturePeer {
			t.Errorf("Peers(rib=loc_rib) did not return %s/%s, which peer_events "+
				"records under exactly that rib -- an empty-looking pass is not a pass",
				routeFilterFixtureRouterA, routeFilterFixturePeerA1)
		}
	})
}

// sessionFactsRouter is this file's own fixture router for the session
// facts (hold time, families, sysDescr).
//
// 10.97.x, and the choice is not arbitrary. testDB is shared by every test
// in this package and never truncated, so a fixture that reuses another
// fixture's (router, peer) pair writes into its answer. This constant was
// 10.94.1.1 for one commit, which is topology_test.go's own
// topoRouterWithdraw -- with its peer, 10.94.1.2, matching too. The down
// event below then carried a higher session_id than the topology fixture's,
// so peerStateCTE's `cur` resolved THAT pair's current session to this
// test's, read the peer as down, and topology correctly dropped every route
// the peer carried: "no edge 65010->65011 among 0 edges", in a test that had
// nothing to do with this one and passed in isolation.
//
// 10.90 through 10.96 and 10.99 are all spoken for elsewhere in this
// package; 10.97 and 10.98 were unused when this was written. Check before
// reusing.
const sessionFactsRouter = "10.97.1.1"
const sessionFactsPeer = "10.97.1.2"

// The facts come from the newest event that ACTUALLY CARRIED THEM, not from
// the newest event.
//
// A Peer Down carries no OPEN -- there is no hold time or capability list in
// a PeerDown message -- so a peer that came up and later flapped has its
// facts on the older row. argMax over every event would answer with the
// down row's empty values and report a session that negotiated nothing,
// which is indistinguishable on screen from a peer that really did.
func TestPeersReportsSessionFactsFromTheEventThatCarriedThem(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	const sid = 940001
	base := time.Now().UTC()
	// The up carries the facts; the later down does not, which is the whole
	// shape of the test.
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: sessionFactsRouter, RouterSysname: "facts-r1", PeerIP: sessionFactsPeer,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: sessionFactsPeer,
		SessionID: sid, Seq: 1, StreamSeq: 1, Kind: "up",
		TsRouter: base, TsCollector: base,
		HoldTime: 180, HoldTimeSeen: 1,
		MPFamilies: []string{"ipv4u", "ipv6u"}, AddPathFamilies: []string{"ipv4u"},
		SysDescr: "FRRouting 10.3_git",
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: sessionFactsRouter, RouterSysname: "facts-r1", PeerIP: sessionFactsPeer,
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: sessionFactsPeer,
		SessionID: sid, Seq: 2, StreamSeq: 2, Kind: "down",
		TsRouter: base.Add(time.Second), TsCollector: base.Add(time.Second),
	})

	got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(sessionFactsRouter)})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 peer, got %d: %+v", len(got), got)
	}
	p := got[0]
	// The newest event still decides the STATE -- that half is unchanged.
	if p.State != "down" {
		t.Errorf("State = %q, want down: the newest event decides the state", p.State)
	}
	if !p.HoldTimeSeen || p.HoldTime != 180 {
		t.Errorf("hold time = %d seen = %v, want 180/true from the up event",
			p.HoldTime, p.HoldTimeSeen)
	}
	if len(p.MPFamilies) != 2 || p.MPFamilies[0] != "ipv4u" || p.MPFamilies[1] != "ipv6u" {
		t.Errorf("MPFamilies = %v, want [ipv4u ipv6u]", p.MPFamilies)
	}
	if len(p.AddPathFamilies) != 1 || p.AddPathFamilies[0] != "ipv4u" {
		t.Errorf("AddPathFamilies = %v, want [ipv4u]", p.AddPathFamilies)
	}
	if p.SysDescr != "FRRouting 10.3_git" {
		t.Errorf("SysDescr = %q", p.SysDescr)
	}
}

// A peer whose session predates the columns reports NOT SEEN, not zero.
//
// Every one of the 818 rows in one lab archive has hold_time_seen =
// 0, and so does every Peer Down. Reporting HoldTime 0 with HoldTimeSeen
// true would tell an operator those sessions ran with keepalives disabled --
// a specific, wrong, and entirely plausible-looking claim.
func TestPeersReportsNoHoldTimeRatherThanZeroWhenNoneWasObserved(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertFlappedPeerFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: fixtureRouterIP})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no peers, so nothing was checked")
	}
	for _, p := range got {
		if p.HoldTimeSeen {
			t.Errorf("peer %s: HoldTimeSeen is true on a fixture that carried no OPEN", p.PeerIP)
		}
		if p.HoldTime != 0 {
			t.Errorf("peer %s: HoldTime = %d, want 0", p.PeerIP, p.HoldTime)
		}
	}
}

const upSinceRouter = "10.97.2.1"
const upSincePeer = "10.97.2.2"

// UpSince is the instant of the LATEST up in the current session -- not the
// first, and not whatever the newest event happens to carry.
//
// Both wrong answers are plausible enough to write by accident, so this
// fixture kills both with one session: up, up again ten seconds later, then
// a down. The first up would report `base`, a plain argMax over every event
// would report the down's `base+20s`, and the answer is the second up's
// `base+10s`.
//
// Ordered on (seq, stream_seq), the same key `state` is resolved on. A
// max() over ts_collector would order on a different key than the state
// sitting beside it in the same row, and the two could disagree on a
// collector whose clock stepped.
func TestPeersReportsTheLatestUpOfTheSessionNotTheFirst(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	const sid = 940002
	base := time.Now().UTC().Truncate(time.Second)
	ev := func(seq uint64, kind string, at time.Time) {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: upSinceRouter, RouterSysname: "up-r1", PeerIP: upSincePeer,
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: upSincePeer,
			SessionID: sid, Seq: seq, StreamSeq: seq, Kind: kind,
			TsRouter: at, TsCollector: at,
		})
	}
	ev(1, "up", base)
	ev(2, "up", base.Add(10*time.Second))
	ev(3, "down", base.Add(20*time.Second))

	got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(upSinceRouter)})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 peer, got %d: %+v", len(got), got)
	}
	want := base.Add(10 * time.Second)
	if !got[0].UpSince.Equal(want) {
		t.Errorf("UpSince = %v, want %v (the latest up, not the first and not the down)",
			got[0].UpSince.UTC(), want)
	}
}

// A session that never carried an up reports NO up_since, not the zero
// instant dressed as one. Same distinction HoldTimeSeen draws: absence is
// not a value, and "up since 1970" is the shape that mistake takes here.
func TestPeersReportsNoUpSinceWhenTheSessionNeverCameUp(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	const sid = 940003
	base := time.Now().UTC().Truncate(time.Second)
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: "10.97.3.1", RouterSysname: "up-r2", PeerIP: "10.97.3.2",
		RIB: "in_pre", PeerASN: 65000, PeerBGPID: "10.97.3.2",
		SessionID: sid, Seq: 1, StreamSeq: 1, Kind: "down",
		TsRouter: base, TsCollector: base,
	})

	got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr("10.97.3.1")})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 peer, got %d: %+v", len(got), got)
	}
	if !got[0].UpSince.IsZero() {
		t.Errorf("UpSince = %v, want the zero instant: this session never came up",
			got[0].UpSince.UTC())
	}
}

// The up is ranked on (seq, stream_seq), never on the collector clock.
//
// Those two agree on every well-behaved collector, which is why a fixture
// whose timestamps rise with its sequence numbers cannot tell the readings
// apart -- against exactly that fixture, a max() over ts_collector
// survives. Here the clock STEPS BACKWARD between
// two ups: seq 3 is the later event and carries the earlier instant.
//
// (seq, stream_seq) is the right key because it is the one `state` is
// resolved on in the same row. Ranking the two on different keys is how a
// peer ends up reporting a state from one event and an up-time from another.
func TestPeersRanksTheUpOnSequenceNotOnTheCollectorClock(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	const sid = 940004
	base := time.Now().UTC().Truncate(time.Second)
	ev := func(seq uint64, kind string, at time.Time) {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: "10.97.4.1", RouterSysname: "up-r3", PeerIP: "10.97.4.2",
			RIB: "in_pre", PeerASN: 65000, PeerBGPID: "10.97.4.2",
			SessionID: sid, Seq: seq, StreamSeq: seq, Kind: kind,
			TsRouter: at, TsCollector: at,
		})
	}
	// The step has to fall between the two UPS, not between a down and an up:
	// a max() over ts_collector ranks only the rows it is filtered to, so a
	// down carrying the latest instant is invisible to it. The first attempt
	// at this fixture stepped the clock around the down and the mutation
	// survived it a second time.
	ev(1, "up", base.Add(30*time.Second))
	ev(2, "down", base.Add(40*time.Second))
	// Newest by sequence, and older on the clock than the up at seq 1.
	stepped := base.Add(10 * time.Second)
	ev(3, "up", stepped)

	got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr("10.97.4.1")})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 peer, got %d: %+v", len(got), got)
	}
	// The peer is up, from seq 3 -- so its up_since must be seq 3's own
	// instant. A max() over ts_collector would answer with seq 1's instant,
	// 20 seconds LATER than the up actually being reported.
	if got[0].State != "up" {
		t.Fatalf("State = %q, want up: seq 3 is the newest event", got[0].State)
	}
	if !got[0].UpSince.Equal(stepped) {
		t.Errorf("UpSince = %v, want %v (seq 3's instant, the up the state comes from)",
			got[0].UpSince.UTC(), stepped)
	}
}
