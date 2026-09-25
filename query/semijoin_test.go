package query

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// routesBaseline runs the statement Routes ran before the semi-join existed:
// the same body, the same predicates, no candidate-key restriction. It is the
// reference every parity assertion below compares against, and it is built
// from the same filters the real path builds so that the two cannot drift in
// anything except the thing under test.
func routesBaseline(t *testing.T, ctx context.Context, q *Q, f RouteFilter) []Route {
	t.Helper()
	if err := f.check(); err != nil {
		t.Fatalf("check: %v", err)
	}
	w := f.predicates()
	stmt := fmt.Sprintf(routesSQL+routesOrder, q.db, w.where(), w.having())
	if f.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		t.Fatalf("baseline query: %v", err)
	}
	defer rows.Close()
	out, err := scanRoutes(rows)
	if err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	return out
}

// TestRoutesSemiJoinMatchesTheBaseline is the blocking parity check the
// wide-filter design demands of any rewrite of these queries: same fixture,
// same question, both paths, assert equal.
//
// Each case asserts FIRST that the semi-join is actually rendered for it.
// Without that, a case whose filter quietly fell back to the baseline
// would compare the baseline against itself and pass while proving
// nothing, which is exactly the failure mode a guard that quietly falls
// back to comparing a baseline against itself produces.
//
// The two repathed fixtures are the ones that matter most. Each holds a route
// key whose OLD observation matches the filter and whose live one does not, so
// the candidate subquery admits the key and the outer HAVING must throw it
// out. That is the exact hazard §4.1 warns a row-level pre-filter creates, and
// the reason filters.candidates is allowed to exist beside that rule.
func TestRoutesSemiJoinMatchesTheBaseline(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertOriginFixture(t, ctx, q)
	insertRepathedFixture(t, ctx, q)
	insertRepathedCommunityFixture(t, ctx, q)
	insertExtCommunityFixture(t, ctx, q)

	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"origin_asn alone", RouteFilter{OriginASN: 65100}},
		{"through_asn alone", RouteFilter{ThroughASN: 64500}},
		{"origin_asn and through_asn together", RouteFilter{
			OriginASN: 65100, ThroughASN: 64500}},
		{"origin_asn narrowed by prefix", RouteFilter{
			Prefix: repathPrefix, OriginASN: repathNewOrigin}},
		{"the SUPERSEDED origin, which must return nothing", RouteFilter{
			Prefix: repathPrefix, OriginASN: repathOldOrigin}},
		{"the superseded community, which must return nothing", RouteFilter{
			Prefix: repathCommunityPrefix, Community: repathCommunityOld}},
		{"a route target", RouteFilter{
			Prefix: extCommFixturePrefix, Community: "rt:65101:1"}},
		{"an extended community", RouteFilter{
			Prefix: extCommFixturePrefix, Community: "soo:65000:777"}},
		{"a community nothing carries", RouteFilter{
			Prefix: extCommFixturePrefix, Community: "65000:1"}},
		{"a wide filter with a limit", RouteFilter{
			OriginASN: 65100, Limit: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.f.predicates()
			frag, _ := routesSemiJoin(testDB, "route_unicast", unicastRoutesKey, w)
			if frag == "" {
				t.Fatalf("this case renders NO semi-join, so comparing it against the " +
					"baseline compares the baseline with itself and proves nothing. " +
					"Either the filter stopped producing candidates or the case is the " +
					"wrong shape for this test")
			}

			got, err := q.Routes(ctx, tc.f)
			if err != nil {
				t.Fatalf("Routes: %v", err)
			}
			want := routesBaseline(t, ctx, q, tc.f)
			if len(got) != len(want) {
				t.Fatalf("the semi-join returned %d routes, the baseline %d",
					len(got), len(want))
			}
			// Compared as a MULTISET, not positionally. routesOrder is
			// (sysname, peer_ip, path_id, collector_id), which is presentational
			// and not a total order -- two routes of one peer under one
			// collector tie on all four, and the two statements are different
			// plans that may break such a tie differently. A positional
			// comparison here failed exactly that way, on two routes that were
			// both genuinely in both answers.
			sortRoutes := func(rs []Route) {
				slices.SortFunc(rs, func(a, b Route) int {
					return strings.Compare(
						fmt.Sprintf("%s|%d|%s|%s|%s|%s", a.Prefix, a.PathID, a.RouterIP,
							a.PeerIP, a.Collector, a.RIB),
						fmt.Sprintf("%s|%d|%s|%s|%s|%s", b.Prefix, b.PathID, b.RouterIP,
							b.PeerIP, b.Collector, b.RIB))
				})
			}
			sortRoutes(got)
			sortRoutes(want)
			for i := range got {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Errorf("route %d differs:\n semi-join: %+v\n baseline:  %+v",
						i, got[i], want[i])
				}
			}

			// The count runs the same statement and has to agree with it.
			n, err := q.CountRoutes(ctx, tc.f)
			if err != nil {
				t.Fatalf("CountRoutes: %v", err)
			}
			if tc.f.Limit == 0 && int(n) != len(want) {
				t.Errorf("CountRoutes reported %d for an answer of %d routes", n, len(want))
			}
		})
	}
}

// TestLiveAggregatesMatchTheirStatements holds the one duplication this design
// could not avoid.
//
// The semi-join's subquery declares the live_ aliases its HAVING names, and it
// has to declare them with the SAME argMax the outer statement uses -- same
// column, same ordering tuple. The outer statements are consts with those
// expressions written inline, so liveAggregate is a second copy of each. A copy
// that drifted would give the subquery a different idea of "live" than the
// statement it is restricting, which is the class of defect peerStateCTE's own
// doc comment exists to warn about.
func TestLiveAggregatesMatchTheirStatements(t *testing.T) {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, stmt := range []struct{ name, sql string }{
		{"routesSQL", routesSQL},
		{"vpnRoutesSQL", vpnRoutesSQL},
		{"evpnRoutesSQL", evpnRoutesSQL},
	} {
		t.Run(stmt.name, func(t *testing.T) {
			body := norm(stmt.sql)
			for alias, agg := range liveAggregate {
				if !strings.Contains(body, norm(agg+" AS "+alias)) {
					t.Errorf("%s does not define %s as %q. liveAggregate is the "+
						"semi-join's copy of that expression; if the statement's "+
						"definition changed, this copy has to change with it or the "+
						"subquery resolves a different value than the statement it "+
						"restricts", stmt.name, alias, agg)
				}
			}
		})
	}
}

// TestSemiJoinKeysMatchTheirStatements holds the semi-join's key against the
// GROUP BY it has to be identical to.
//
// The tuple tested by the outer statement and the tuple returned by the
// subquery are both built from this list, so they agree with each other by
// construction -- but neither agrees with the STATEMENT unless this does. A key
// shorter than the GROUP BY tests a tuple that does not identify a group; a key
// in a different order tests the right columns against the wrong values, and
// ClickHouse would compare collector_id against a router_ip without complaint
// if both are Strings.
func TestSemiJoinKeysMatchTheirStatements(t *testing.T) {
	got := outerGroupBy(t, routesSQL, "\nFROM %[1]s.route_unicast_current r\n")
	if !slices.Equal([]string(unicastRoutesKey), got) {
		t.Errorf("unicastRoutesKey is\n %v\nbut routesSQL groups by\n %v",
			[]string(unicastRoutesKey), got)
	}
}

// TestRoutesSemiJoinBindsEveryPlaceholder is the arity guard. The fragment is
// spliced into the middle of the outer statement and repeats both the WHERE and
// the HAVING predicates, so its argument list is the one place in this package
// where a miscount is most available -- and clickhouse-go silently DISCARDS a
// surplus argument rather than raising, which binds every later value one
// position early.
func TestRoutesSemiJoinBindsEveryPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"origin_asn alone", RouteFilter{OriginASN: 65001}},
		{"origin_asn with a prefix and a family", RouteFilter{
			Prefix: "10.0.0.0/24", Family: "ipv4u", OriginASN: 65001}},
		{"both ASN filters", RouteFilter{OriginASN: 65001, ThroughASN: 65002}},
		{"a community, which is an OR of several columns", RouteFilter{
			Community: "65101:1"}},
		{"a community and an origin together", RouteFilter{
			Community: "rt:65101:1", OriginASN: 65001}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.f.predicates()
			frag, args := routesSemiJoin(testDB, "route_unicast", unicastRoutesKey, w)
			if frag == "" {
				t.Fatalf("no semi-join rendered, so this case checks nothing")
			}
			stmt := fmt.Sprintf(routesSQL+routesOrder, testDB, w.where()+frag, w.having())
			if n := strings.Count(stmt, "?"); n != len(args) {
				t.Fatalf("%d placeholders, %d values", n, len(args))
			}
		})
	}
}

// TestNoSemiJoinWithoutACandidateForEveryPredicate holds the fallback: a
// filter whose HAVING cannot be expressed against raw rows runs the statement
// that was always correct, rather than one with a candidate set narrower than
// its answer.
//
// A narrower candidate set does not fail loudly. It silently returns fewer
// routes than the filter matches, which is this project's signature defect --
// so the safe direction is to lose the optimization, never the routes.
func TestNoSemiJoinWithoutACandidateForEveryPredicate(t *testing.T) {
	t.Run("a having with no candidate disables it", func(t *testing.T) {
		var f filters
		f.wideASN(65001, 0)
		f.havingExpr("live_something = ?", 1) // a predicate wide() never saw
		if f.semiJoinable() {
			t.Error("a statement with more havings than candidates offered a " +
				"semi-join. Its subquery would apply only some of the answer's " +
				"predicates, which is a WIDER candidate set and harmless -- but the " +
				"count check is what stops the opposite case, and it must hold both ways")
		}
	})

	t.Run("an untranslatable column disables it", func(t *testing.T) {
		var f filters
		f.wide("live_mystery = ?", "", nil, 1)
		if f.semiJoinable() {
			t.Error("a predicate with no raw-row twin still offered a semi-join")
		}
		if len(f.havings) != 1 {
			t.Errorf("the predicate was dropped from the HAVING as well (%d havings); "+
				"the optimization is optional, the filter is not", len(f.havings))
		}
	})

	t.Run("no wide filter at all means no semi-join", func(t *testing.T) {
		f := RouteFilter{Prefix: "10.0.0.0/24"}.predicates()
		if frag, _ := routesSemiJoin(testDB, "route_unicast", unicastRoutesKey, f); frag != "" {
			t.Errorf("an exact-prefix lookup rendered a semi-join:\n%s\nthe bloom "+
				"filter on prefix already answers that question in 11ms", frag)
		}
	})
}
