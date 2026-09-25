package query

import (
	"fmt"
	"strings"
)

// The candidate-key semi-join: answer a wide filter by first deciding WHICH
// route keys can possibly match, then aggregating only those.
//
// The problem it solves is the one measured on 2026-08-30 and re-measured
// on 2026-09-05. `origin_asn`, `through_asn` and `community` are HAVING
// predicates by necessity (§4.1: a WHERE on a
// mutable attribute changes which row argMax picks and reports stale
// attributes as live), and a HAVING runs after the GROUP BY. So the cost of a
// wide filter is not proportional to how many routes match it -- it is eleven
// argMax state machines over every route the scope holds, whether one matches
// or a million do. Measured on a 1.5M-row peer: 683ms and 4.43 GiB of peak
// memory to return 2,000 routes, and the same cost for a filter matching a
// third of the table as for one matching 0.2% of it.
//
// The rewrite keeps the HAVING exactly where it is and adds a restriction to
// the keys a much cheaper statement says are worth considering:
//
//	683ms / 4.43 GiB   as it stands
//	227ms / 1.28 GiB   restricted to candidate keys
//	 53ms / 71 MiB     ...with the raw-row candidates pruning the aggregate
//
// Twelve times faster and sixty times smaller, for the same answer.
//
// IT IS NOT FREE, AND THE WORST CASE IS A REGRESSION. The subquery is pure
// overhead when it prunes nothing, and a filter that matches every route
// prunes nothing. Measured on the same peer with `through_asn` set to an AS
// present in every path:
//
//	622ms / 4.43 GiB   as it stands
//	973ms / 4.59 GiB   with the semi-join -- 56% slower, no memory saved
//
// The penalty is bounded by what the candidate statement costs: the same scan
// with two argMax instead of eleven, roughly a third of the full statement.
// So the trade is a large win when a filter is selective against a moderate
// loss when it is not, and the crossover selectivity is NOT measured -- the
// rig has only 0.2% and 100% shapes, with nothing in between. That is the open
// question if this ever needs tuning; the shape that loses is a filter
// matching most of the table, which is a question whose answer is "all of
// them" and which no caller has a reason to ask twice.
//
// THE SAME ANSWER is the whole claim, and three things hold it up:
//
//  1. The outer statement is untouched. Its GROUP BY still sees every
//     observation of every key it aggregates, so its argMax still resolves
//     true live state and its HAVING still decides membership. The semi-join
//     can only remove keys from consideration; it can never change what is
//     reported about a key that survives.
//  2. The candidate predicates cannot remove a matching key. See
//     filters.candidates for the argument in full: a key matches only if its
//     LIVE row satisfies the having, that row is the newest observation, the
//     candidate is the same predicate over that row's own raw columns, and the
//     newest row overall stays newest inside any subset containing it.
//  3. The predicate TEXT is shared, not reimplemented. The subquery declares
//     the same live_ aliases the outer statement does and runs the identical
//     rendered having against them. There is no second definition of what
//     `origin_asn=X` means, which is the failure this package has paid for
//     before (see peerStateCTE, and the RIB read path's phase two).
//
// What a mutation pass over this can and cannot see, measured 2026-09-05:
// making a candidate NARROWER than its having (reading the wrong AS path
// position, or keeping one branch of a community OR) loses routes and fails
// the parity test and three existing filter tests. Making the subquery WIDER
// cannot be seen in an answer at all -- dropping the candidates, the wide
// having, or the withdrawal gate from it each leaves the outer statement to
// reject what the subquery let through, so the answer is identical and only
// the cost moves. That asymmetry is the design working as intended: the
// subquery is allowed to be wrong in one direction only, and it is the
// direction that costs milliseconds rather than routes.
//
// It is one statement, not two. That matters more than it looks: the RIB
// read path's two-phase reader shipped a walk-ending defect in the seam
// between its statements, where one query decided a page's membership and
// another its contents. A semi-join has no seam -- one query, one snapshot,
// one answer -- and /v1/routes is capped rather than paginated, so there is no
// cursor to carry a disagreement into either.

// semiJoinKey is the route key one family aggregates by: the columns of its
// statement's own GROUP BY, in that order. The tuple the outer statement tests
// and the tuple the subquery returns are both built from this one list, so
// they cannot disagree about width or order.
type semiJoinKey []string

// unicastRoutesKey is routesSQL's GROUP BY, column for column.
// TestSemiJoinKeysMatchTheirStatements holds them together.
var unicastRoutesKey = semiJoinKey{
	"r.collector_id", "r.router_ip", "r.peer_ip", "r.rib", "r.family",
	"r.prefix", "r.path_id",
}

// bare strips the `r.` qualifier, giving the names the subquery projects.
func (k semiJoinKey) bare() []string {
	out := make([]string, len(k))
	for i, c := range k {
		out[i] = strings.TrimPrefix(c, "r.")
	}
	return out
}

// routesSemiJoin renders the candidate-key restriction for one route
// statement, and the complete ordered argument list for the finished
// statement.
//
// It returns both because the two cannot be produced independently. The
// fragment is spliced into the outer WHERE, so its placeholders sit between
// the outer statement's own WHERE placeholders and its HAVING ones, and the
// subquery repeats both the WHERE and the HAVING predicates -- the driver
// binds strictly left to right and silently discards a surplus argument (see
// peersStatement), so an argument list assembled anywhere but here would be a
// standing invitation to bind a page's values one position out.
//
// The order, which is the order the placeholders appear in the finished text:
//
//	outer WHERE | sub WHERE | sub candidates | sub HAVING | outer HAVING
//
// An empty fragment means this statement gets no semi-join, and then the args
// are exactly what the statement always bound. Every caller can splice the
// fragment unconditionally.
func routesSemiJoin(db, table string, key semiJoinKey, f *filters) (string, []any) {
	return routesSemiJoinFrom(db+"."+table, key, f)
}

// routesSemiJoinFrom is routesSemiJoin over any row source the outer
// statement can name, not only a table: source is rendered verbatim after
// FROM and aliased r. Topology passes a CTE, because its outer statement
// reads the current and history tables together and the candidate keys must
// come from the same rows. A semi-join over either table alone drops routes
// the outer statement would keep.
func routesSemiJoinFrom(source string, key semiJoinKey, f *filters) (string, []any) {
	if !f.semiJoinable() {
		return "", f.values()
	}

	// The aliases this filter's having actually names, and nothing else. A
	// statement filtering on origin_asn declares one argMax here; the outer
	// statement declares eleven.
	defs := make([]string, 0, len(f.liveAliases))
	for _, alias := range f.liveAliases {
		agg, ok := liveAggregate[alias]
		if !ok {
			// An alias with no declared aggregate cannot be rendered, and
			// rendering the subquery without it would leave the having naming
			// an identifier the subquery does not define. Fall back to the
			// statement that needs no subquery at all.
			return "", f.values()
		}
		defs = append(defs, ",\n    "+agg+" AS "+alias)
	}

	bare := key.bare()
	sel := make([]string, len(key))
	for i, c := range key {
		sel[i] = c + " AS " + bare[i]
	}

	fragment := fmt.Sprintf(`
  AND (%[2]s) IN (
SELECT %[3]s FROM (
SELECT
    %[4]s,
    argMax(r.is_withdraw, (r.seq, r.stream_seq)) AS live_is_withdraw%[5]s
FROM %[1]s r
INNER JOIN cur
    ON r.collector_id = cur.collector_id
   AND r.router_ip    = cur.router_ip
   AND r.session_id   = cur.sid
INNER JOIN peer_up
    ON r.collector_id = peer_up.collector_id
   AND r.router_ip    = peer_up.router_ip
   AND r.peer_ip      = peer_up.peer_ip
   AND cur.sid        = peer_up.sid
WHERE `+servedGate+`%[6]s%[7]s
GROUP BY %[2]s
HAVING live_is_withdraw = 0%[8]s
))`,
		source,
		strings.Join(key, ", "),
		strings.Join(bare, ", "),
		strings.Join(sel, ",\n    "),
		strings.Join(defs, ""),
		f.where(),
		" AND "+strings.Join(f.candidates, " AND "),
		f.having(),
	)

	args := make([]any, 0, len(f.args)*2+len(f.candidateArgs)+len(f.havingArgs)*2)
	args = append(args, f.args...)          // outer WHERE
	args = append(args, f.args...)          // sub WHERE
	args = append(args, f.candidateArgs...) // sub candidates
	args = append(args, f.havingArgs...)    // sub HAVING
	args = append(args, f.havingArgs...)    // outer HAVING
	return fragment, args
}
