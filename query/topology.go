package query

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ASNode is one autonomous system in a merged AS-path graph.
//
// Routes counts ROUTE IDENTITIES that traverse this AS, never path
// positions: a route whose path prepends the same ASN three times traverses
// it once. See topologyNodesSQL for the arrayDistinct that makes that true.
//
// Origin and Transit are not exclusive and are not an enum. One AS is
// routinely the last hop of one route and the middle of another, and the
// three /v1/topology renders as `roles` are a SET for that reason -- a node
// that originates some prefixes and carries others is the ordinary shape of
// a mid-size network, not an edge case.
//
// Peer means "this is the AS of a BMP peer we hold a session with", which is
// a fact about the fleet's vantage point rather than about the internet. It
// is derived from route_unicast's own peer_asn column, so it is the AS the
// router told us its neighbor was.
//
// There is no holder name here. GET /v1/asnames resolves RIPE's registered
// holder names in a batch, when that optional dataset is loaded, and a client
// renders the name beside the number; the graph itself carries "AS3356".
type ASNode struct {
	ASN       uint32
	Routes    uint64
	Origin    bool
	Transit   bool
	Peer      bool
	FirstSeen time.Time
}

// ASEdge is one AS adjacency: a pair of ASNs that appear consecutively in at
// least one route's live AS path.
//
// Routes and LiveRoutes are carried separately, and that pairing is the whole
// reason this type is not a string. LiveRoutes == 0 means every route
// carrying this adjacency has been withdrawn -- the edge the collector knows
// about and the network no longer uses -- and the client renders it dashed.
// A server-computed state string would have to bake the screen's time window
// into the response, and the window is a thing the operator moves.
//
// FirstSeen is the EARLIEST observation among the routes that currently
// traverse this edge, which is not the same as the first time the edge itself
// was seen: a route first advertised with one path and later re-advertised
// with another contributes its original timestamp to whatever edge it carries
// now. The distinction is invisible in the archive and worth stating rather
// than discovering. It is also bounded by the history table's 90-day TTL, so
// an edge marked "appeared" appeared in what we hold: once a route's history
// has expired, its first_seen is its current row's own timestamp.
type ASEdge struct {
	Src, Dst   uint32
	Routes     uint64
	LiveRoutes uint64
	FirstSeen  time.Time
}

// Graph is one family's merged AS-path graph for one scope.
//
// Routes is the population the graph was built from -- route identities, not
// rows, and not a sum over Nodes or Edges, both of which count each route once
// per AS or adjacency it touches. /v1/topology reports it as
// meta.total_matched, which is what lets a caller tell "six ASNs is the whole
// answer" from "six ASNs is what fitted".
//
// Nodes and Edges are non-nil even when empty. The contract types both as
// arrays and a nil slice marshals to JSON null, which is a different claim
// from an empty graph -- and an empty graph is an ordinary answer here, since
// a one-hop path is a node with no edge at all and EVPN's real graph is two
// edges wide.
type Graph struct {
	Nodes  []ASNode
	Edges  []ASEdge
	Routes uint64
}

// topologySQL is the statement all three families' graphs are built from, up
// to the SELECT that reads it.
//
// It is ONE text rendered three times rather than three near-copies, because
// everything below this line -- the argMaxIf, the missing withdrawal gate, the
// two inherited INNER JOINs, the empty-path exclusion -- is a claim about what
// a graph MEANS, and three copies of it would be three chances for `routes` to
// mean one thing in the unicast member of a /v1/topology response and
// something else in the other two. That inconsistency is invisible at the type
// level: all three return the same Graph.
//
// It reads each route's rows from BOTH of its family's route tables, as one
// source, topologyRowsCTE: the current table (one row per route, newest by
// seq, no TTL) and the history table (every observation, 90-day TTL). The
// current table alone cannot answer two of the questions below: a withdrawn
// route's current row is the withdrawal, which carries no path, and the
// current table keeps no earlier row to take a min(ts_collector) over. The
// history table alone loses a route that is still live but whose last
// announcement is older than the TTL -- a stable route, which is the common
// case. Over both, the aggregates below read the right row for each:
//
//   - live_is_withdraw is the newest row's, and the newest row is in the
//     current table whether or not history still holds a copy of it.
//   - live_as_path is the newest ANNOUNCEMENT's path: the current row's when
//     the route is live, and the last advertised path from history when it
//     is withdrawn. A withdrawn route whose history has expired has no path
//     left anywhere and drops out at `length(live_as_path) > 0` -- withdrawn
//     edges are bounded by retention, as /v1/topology documents. Until the
//     current table's background merge runs, its superseded rows can still
//     supply that path; that only ever draws an edge a little longer.
//   - first_seen is the earliest row in either table: the route's first
//     observation while history holds it, and the current row's own
//     timestamp after.
//
// The current row is a copy of the newest history row, so when both exist
// the two tie in argMax and agree in value, and min is unaffected.
//
// What differs per family is exactly two things, and both are interpolated:
// the route TABLE, and that family's own route IDENTITY (see unicastRoutesKey,
// vpnRoutesKey and evpnRoutesKey). The identity is what keeps these counts
// right -- one prefix under two route distinguishers is two VPN routes,
// and an EVPN route is identified by its
// MAC, IP, ethernet tag and ESI as well as its prefix -- and grouping any
// family on another's key does not raise, it silently reports a smaller
// number.
//
// It is NOT routesSQL wrapped, and that keeps withdrawn edges as data, in
// the SQL rather than in a comment: routesSQL ends with
// `HAVING live_is_withdraw = 0`, so wrapping it -- the way CountRoutes
// rightly does -- would drop every withdrawn route before the graph was
// built and delete the one edge state this screen exists to show. What it
// DOES reuse is everything that decides WHICH routes are in scope: the same
// predicate helpers, the same semi-join, the same current-session and
// peer_up joins, so the graph describes exactly the population /v1/routes
// shows for the same query string. The semi-join reads topologyRowsCTE too,
// not the current table /v1/routes' semi-join reads: over the current table
// alone a withdrawn route has only its path-less withdrawal, no candidate
// matches it, and origin_asn= would drop exactly the withdrawn routes this
// statement keeps.
//
// Three deliberate differences from routesSQL, and only three:
//
//  1. live_as_path is argMaxIf(..., is_withdraw = 0) -- the last ADVERTISED
//     path. A withdrawal carries no path attributes, so a plain argMax reports
//     the empty path for exactly the routes whose edges this screen must still
//     draw.
//  2. There is no `HAVING live_is_withdraw = 0`. Withdrawal is DATA here
//     (live_routes per edge), not a filter.
//  3. live_communities, live_large_communities, live_ext_communities and
//     live_route_targets are ALSO argMaxIf(..., is_withdraw = 0), for
//     live_as_path's own reason and not routesSQL's plain argMax: routesSQL
//     reads them from route_unicast_current alone and then filters
//     `HAVING live_is_withdraw = 0`, so its newest row is always the live
//     one and a plain argMax over it is correct. This statement keeps
//     withdrawn routes, so its newest row can be the withdrawal, which
//     carries no communities or route targets for the same reason it
//     carries no path -- a plain argMax here would report a withdrawn
//     route's community as empty and community= would stop matching it the
//     moment it withdrew, contradicting origin_asn='s own promise to keep
//     matching a route "this AS has STOPPED originating" (TopologyUnicast's
//     own doc comment). These four were the columns missing entirely until
//     now: the shared predicate in query/filters.go's community
//     method names them in its HAVING, routesSQL / vpnRoutesSQL /
//     evpnRoutesSQL each define them for their own SELECT, and this
//     statement had never defined them for its own -- a HAVING naming an
//     identifier `latest` never SELECTed, which ClickHouse refuses at parse
//     time rather than answering, the way it refuses length(live_as_path)
//     would if that column were missing too.
//
// The alias live_as_path keeps routesSQL's name because wideASN renders
// origin_asn= and through_asn= as HAVING predicates against that exact
// identifier (query/filters.go:573), and the four community aliases keep
// routesSQL's names for the identical reason: query/community.go's
// colCommunities, colLargeCommunities, colExtCommunities and colRouteTargets
// are those literal strings, shared verbatim with routesSQL, vpnRoutesSQL
// and evpnRoutesSQL. Rename any of the five and a scope mode or the
// community filter breaks silently.
//
// eorCTE is deliberately absent, unlike every other route statement here:
// this one has no LEFT JOIN eor and no dumpStateExpr, so composing it would
// read eor_events for a column nothing selects. Run without it against a
// live archive, the statement returns the correct graph, which is what
// makes it dead weight rather than a subtle dependency. Dropping it also
// removes evpnRoutesSQL's one extra placeholder -- the EVPN family token its
// eor join binds -- so all three families here bind exactly their filter's own
// values and nothing else.
//
// A route whose live path is EMPTY is excluded by `length(live_as_path) > 0`.
// That is not the same as excluding withdrawn routes: a withdrawn route that
// was ever advertised has a path here. It excludes a route this collector has
// only ever seen withdrawn, which contributes neither a node nor an edge and
// whose origin the archive genuinely does not know -- 1,839 of the archive's
// 8,611 unicast rows have an empty as_path, and query.originASN already
// refuses to report them as originating in AS 0.
//
// %[1]s is the database, %[2]s the history route table (its current table is
// the same name with _current appended), %[3]s that family's route
// identity, %[4]s the WHERE (predicates plus any semi-join), %[5]s the wide
// HAVING. topologyCTE is the only thing that renders it.
const topologySQL = "WITH " + peerStateCTE + peerUpCTE + `,
` + topologyRowsCTE + ` AS (
    SELECT * FROM %[1]s.%[2]s_current
    UNION ALL
    SELECT * FROM %[1]s.%[2]s
),
latest AS (
    SELECT
        argMaxIf(r.as_path,           (r.seq, r.stream_seq), r.is_withdraw = 0) AS live_as_path,
        argMaxIf(r.communities,       (r.seq, r.stream_seq), r.is_withdraw = 0) AS live_communities,
        argMaxIf(r.large_communities, (r.seq, r.stream_seq), r.is_withdraw = 0) AS live_large_communities,
        argMaxIf(r.ext_communities,   (r.seq, r.stream_seq), r.is_withdraw = 0) AS live_ext_communities,
        argMaxIf(r.route_targets,     (r.seq, r.stream_seq), r.is_withdraw = 0) AS live_route_targets,
        argMax(r.is_withdraw, (r.seq, r.stream_seq))                  AS live_is_withdraw,
        min(r.ts_collector)                                           AS first_seen,
        any(r.peer_asn)                                               AS peer_asn,
        %[6]s                                                         AS route_key
    FROM ` + topologyRowsCTE + ` r
    INNER JOIN cur
        ON r.collector_id = cur.collector_id
       AND r.router_ip    = cur.router_ip
       AND r.session_id   = cur.sid
    INNER JOIN peer_up
        ON r.collector_id = peer_up.collector_id
       AND r.router_ip    = peer_up.router_ip
       AND r.peer_ip      = peer_up.peer_ip
       AND cur.sid        = peer_up.sid
    WHERE ` + servedGate + `%[4]s
    GROUP BY %[3]s
    HAVING length(live_as_path) > 0%[5]s
)`

// vpnRoutesKey and evpnRoutesKey are route_vpn's and route_evpn's own route
// identities, in the same shape unicastRoutesKey gives route_unicast's: the
// columns their family's /v1/routes statement groups by, in that order.
//
// They live here rather than beside unicastRoutesKey in semijoin.go because
// topology is their only consumer -- neither vpnRoutesSQL nor evpnRoutesSQL
// carries a candidate-key semi-join, so neither has ever needed its GROUP BY
// as data. Calling them semiJoinKey would say the opposite.
// TestTopologyKeysMatchTheirFamilysStatement holds all three against the
// statement each has to agree with.
//
// vpn is unicast's key plus rd, and the position of rd is vpnRoutesSQL's own
// (between family and prefix) rather than an append: a GROUP BY is a set, so
// the order changes nothing about the grouping, and matching the statement
// character for character is what lets one test compare them.
var (
	vpnRoutesKey = []string{
		"r.collector_id", "r.router_ip", "r.peer_ip", "r.rib", "r.family",
		"r.rd", "r.prefix", "r.path_id",
	}
	evpnRoutesKey = []string{
		"r.collector_id", "r.router_ip", "r.peer_ip", "r.rib",
		"r.route_type", "r.rd", "r.prefix", "r.mac", "r.ip", "r.ethernet_tag",
		"r.esi", "r.path_id",
	}
)

// topologyCTE renders topologySQL for one family: its table, its route
// identity, and the WHERE and HAVING its filter produced.
//
// It takes the rendered clauses rather than a *filters because the three
// families' filters are three different types, and because unicast splices a
// semi-join fragment into its WHERE that the other two do not have.
// routeKeyTuple renders a family's route identity with the COLLECTOR
// REMOVED, as a tuple `latest` projects for the counts above it to
// deduplicate on.
//
// The split is the point. collector_id must STAY in `latest`'s GROUP BY:
// that grouping resolves each route's live state with argMax over (seq,
// stream_seq), and seq is a counter each collector mints for itself, so a
// group spanning collectors would rank independent counters and pick a
// winner on nothing. But a route two collectors both carry is ONE route,
// and `routes` is defined as route identities -- so the counts read this
// instead of counting `latest`'s rows.
//
// It is the same division the Loc-RIB comparison uses: inner grouping
// keyed by collector, outer count deduped across them.
func routeKeyTuple(key []string) string {
	out := make([]string, 0, len(key))
	for _, c := range key {
		if c == "r.collector_id" {
			continue
		}
		out = append(out, c)
	}
	return "(" + strings.Join(out, ", ") + ")"
}

func topologyCTE(db, table string, key []string, where, having string) string {
	return fmt.Sprintf(topologySQL, db, table, strings.Join(key, ", "), where, having,
		routeKeyTuple(key))
}

// topologyEdgesSQL turns each route's live path into the adjacencies it
// asserts and aggregates them across the scope.
//
// range(1, length(path)) is [1 .. length-1], so the pairs are (p[1],p[2]) up
// to (p[n-1],p[n]) and a one-hop path yields none -- which is correct, and is
// why nodes are not derived from this list (see topologyNodesSQL).
//
// arrayFilter drops the pairs whose two halves are equal, and arrayDistinct
// then counts each adjacency once per route. Both exist for AS PATH
// PREPENDING, which is the most common thing a network does to its own
// announcements and which the live archive cannot show: measured 2026-09-11,
// ZERO of its 8,611 route_unicast, 6,907 route_vpn and 904 route_evpn rows
// carry a repeated ASN anywhere in as_path, so this whole class is invisible
// to any measurement taken against it, in every family. A path of
// `65050 65051 65051 65052` yields the consecutive pair (65051, 65051), and
// an AS does not peer with itself -- drawing that pair puts a self-loop on
// the canvas describing the prepend rather than the network. arrayDistinct is
// the second half of the same correction: without it that route would be
// counted twice on any adjacency its path repeats, and `routes` is defined as
// route identities.
//
// This SELECT is shared by all three families, so the rule is too, and that
// is the point rather than a convenience: a `routes` that meant route
// identities for unicast and path positions for VPN would be one number with
// two meanings inside a single /v1/topology response, and nothing in the
// Graph type would say so. TestTopologyUnicastIgnoresPathPrepending,
// TestTopologyVPNIgnoresPathPrepending and
// TestTopologyEVPNIgnoresPathPrepending are the three fixtures; not one of
// them could be drawn from real data, which is exactly why each had to be
// manufactured.
//
// countIf(live_is_withdraw = 0) is the live half of the pair, counted rather
// than filtered -- see ASEdge.
const topologyEdgesSQL = `
SELECT e.1 AS src, e.2 AS dst,
       uniqExact(route_key)                               AS routes,
       uniqExactIf(route_key, live_is_withdraw = 0)       AS live_routes,
       min(first_seen)                                    AS first_seen
FROM (SELECT live_is_withdraw, first_seen, route_key,
             arrayJoin(arrayDistinct(arrayFilter(p -> p.1 != p.2,
                 arrayMap(i -> (live_as_path[i], live_as_path[i + 1]),
                          range(1, length(live_as_path)))))) AS e
      FROM latest)
GROUP BY src, dst
ORDER BY routes DESC, src, dst`

// topologyNodesSQL reads the paths directly rather than deriving nodes from
// topologyEdgesSQL's output, and that is not a missed simplification: a
// one-hop path contributes a node and NO edge, so a graph assembled from the
// edge list would report an origin-only answer as nothing at all.
//
// arrayDistinct is here for topologyEdgesSQL's reason -- a prepended ASN is
// one route traversing one AS, not two -- and, like that one, it is shared by
// all three families rather than repeated per family.
//
// is_origin and is_transit are both computed against live_as_path[-1] rather
// than one being the negation of the other after aggregation, because an AS
// can be both across different routes and the flags are a set, not an enum.
// The subscript is safe here where it is not in originASN: `HAVING
// length(live_as_path) > 0` has already excluded the empty path, so [-1]
// cannot be the 0 that ClickHouse returns for an out-of-range subscript.
const topologyNodesSQL = `
SELECT asn,
       uniqExact(route_key)     AS routes,
       maxIf(1, is_origin)  = 1 AS origin,
       maxIf(1, is_transit) = 1 AS transit,
       maxIf(1, is_peer)    = 1 AS peer,
       min(first_seen)          AS first_seen
FROM (SELECT first_seen, peer_asn, live_as_path, route_key,
             arrayJoin(arrayDistinct(live_as_path)) AS asn,
             asn =  live_as_path[-1] AS is_origin,
             asn != live_as_path[-1] AS is_transit,
             asn =  peer_asn         AS is_peer
      FROM latest)
GROUP BY asn
ORDER BY routes DESC, asn`

// topologyRoutesSQL counts the route identities the scope matched, which is
// the population the graph describes. It counts `latest`'s own rows -- one
// per live route key -- for CountRoutes' reason: a bare count over the route
// table would count raw observations instead, inflated by every superseded
// row argMax discards and by the ReplacingMergeTree duplicates this package
// never reads FINAL to collapse.
//
// The leading newline is load-bearing: the CTE text ends with `)`, and
// `)SELECT` is not a statement.
const topologyRoutesSQL = `
SELECT uniqExact(route_key) FROM latest`

// topologyPredicates renders rf the way RouteFilter.predicates does, with
// exactly ONE rule changed: the exact-prefix predicate is rendered only when
// Prefix is actually set.
//
// This is a deliberate divergence from a shared method, so it is worth being
// precise about what forced it. predicates() renders `r.prefix = ?` bound to
// "" whenever neither Covers nor a wide filter is present, and that is
// CORRECT for /v1/routes: it is what makes an unfiltered dump unreachable
// through the type, and "the empty prefix" is a real question there with a
// real (empty) answer. /v1/topology's sufficient scopes include router= +
// peer= with nothing else, which /v1/routes refuses at the API layer -- so
// no caller could previously reach the combination at all. Through this
// endpoint that rendering becomes a query for a prefix route_unicast has
// never held: zero rows, a 200, and an empty graph indistinguishable from a
// peer with no paths. It was measured against 20,000 real routes before any
// of this shipped.
//
// Relaxing predicates() itself was rejected: it is shared with /v1/routes,
// /v1/routes/unicast and CountRoutes, where that predicate is the guard, and
// loosening it there to serve a new endpoint trades a real safety property
// for convenience. What replaces the guard HERE is checkTopology, which
// refuses a filter carrying no scope at all rather than answering it.
//
// Everything else is identical, including the order the predicates are added
// in -- which is the order their placeholders bind, so it is a contract with
// the statement and not a style choice. TestTopologyPredicatesMatchRoutesWhereverBothCanAnswer
// asserts the two render character-for-character the same WHERE, HAVING,
// bound values and semi-join for every filter where they must agree, and
// TestTopologyPredicatesOmitTheEmptyPrefixForAPeerScope pins the one case
// where they must not.
func (rf RouteFilter) topologyPredicates() *filters {
	var f filters
	if !f.coversIfAsked("r.prefix", rf.Covers) && rf.Prefix != "" {
		f.eq("r.prefix", rf.Prefix)
	}
	f.eqNonEmpty("r.family", rf.Family)
	f.eqAddr("r.router_ip", rf.Router)
	f.eqAddr("r.peer_ip", rf.Peer)
	f.eqNonEmpty("r.rib", rf.RIB)
	f.wideASN(rf.OriginASN, rf.ThroughASN)
	if rf.Community != "" {
		// The error is dropped here because check() has already refused an
		// unparseable value, exactly as coversIfAsked drops covers' parse
		// error for the same reason.
		m, _ := ParseCommunity(rf.Community)
		f.community(m)
	}
	return &f
}

// checkTopology validates rf for /v1/topology: everything RouteFilter.check
// validates, plus the scope rule that /v1/routes does not need.
//
// The scope rule exists because topologyPredicates drops the unconditional
// prefix predicate. On /v1/routes that predicate is what makes "every route
// in the table" unreachable through the type; without it, RouteFilter{} would
// render no predicate at all and aggregate the whole archive for a caller who
// named nothing. VPNRouteFilter.check is the model -- it has always had this
// shape, for the same reason and with the same remedy in the message.
//
// What counts as a scope is a fixed list, and router= + peer=
// TOGETHER is on it where neither alone is. That is the endpoint's declared
// departure from /v1/routes, and it is affordable for a stated reason: one
// peer's RIB is hundreds of thousands of ROUTES and /v1/routes refuses to
// return it, but the same peer's AS-path GRAPH is tens of edges, and
// router_ip/peer_ip are a prefix of every route table's sort key, so that
// mode is a seek where origin_asn= is a scan.
//
// Family, RIB and Limit are narrowings, not scopes. A filter carrying only
// those still asks about every route in the archive, just fewer columns of
// it.
func (rf RouteFilter) checkTopology() error {
	if err := rf.check(); err != nil {
		return err
	}
	if rf.Prefix != "" || rf.Covers != "" || rf.wideAsked() {
		return nil
	}
	if rf.Router.IsValid() && rf.Peer.IsValid() {
		return nil
	}
	return fmt.Errorf("%w: /v1/topology needs something to scope the graph to "+
		"-- prefix, covers, origin_asn, through_asn or community names the "+
		"routes, and router together with peer names the vantage point. "+
		"family, rib and limit narrow an answer but do not make one: without "+
		"one of the above this would aggregate every route in the archive",
		ErrBadFilter)
}

// TopologyUnicast reports the merged AS-path graph the unicast routes in
// scope form: one node per AS that appears in a live path, one edge per pair
// of ASNs that appear consecutively in one.
//
// f names the scope exactly as Routes' does -- prefix or covers, narrowed by
// family, router, peer and rib, or the wide origin_asn/through_asn/community
// filters -- with two differences from that endpoint, both deliberate:
//
//   - router= AND peer= together is a sufficient scope here, where
//     /v1/routes requires a content filter. See checkTopology.
//   - a WITHDRAWN route is in the answer, described by its last advertised
//     path, for as long as the history table still holds that path. So
//     origin_asn= matches routes this AS has STOPPED originating,
//     which is exactly the question the screen exists to answer and a
//     deliberate divergence from /v1/routes' result for the identical query
//     string.
//
// The graph is the current session's, because /v1/routes is: the statement
// inherits routesSQL's INNER JOIN on cur and its servedGate predicate,
// so a peer whose BMP session has since reset contributes nothing here for
// the same reason it contributes nothing there. That is the property that
// makes the looking glass's Topology tab honest beside its Paths tab, and it
// costs edges -- an unscoped baseline of 15 unicast edges was
// measured without those joins and the shipped statement returns 13 on the
// same archive. Both numbers are correct about different populations.
//
// f.Limit is ignored. It caps ROWS RETURNED on /v1/routes, and this endpoint
// returns no rows; capping a graph would mean choosing which adjacencies to
// hide, which is a decision no caller asked for and none could detect.
func (q *Q) TopologyUnicast(ctx context.Context, f RouteFilter) (Graph, error) {
	if err := f.checkTopology(); err != nil {
		return Graph{}, err
	}
	w := f.topologyPredicates()
	semi, args := routesSemiJoinFrom(topologyRowsCTE, unicastRoutesKey, w)
	cte := topologyCTE(q.db, "route_unicast", unicastRoutesKey, w.where()+semi, w.having())
	return q.graph(ctx, cte, args)
}

// TopologyVPN is TopologyUnicast for route_vpn: the merged AS-path graph the
// VPN and labeled-unicast routes in scope form, over the SAME statement text
// and therefore with the same meaning for every field of Graph.
//
// It groups on route_vpn's own route identity, which is route_unicast's plus
// rd. That is not a detail of the SQL: one prefix advertised under two route
// distinguishers is TWO routes in two VRFs, and grouping them on the unicast
// key collapses them into one -- which does not raise and does not empty the
// graph, it just reports half the routes on every edge they share.
// TestTopologyVPNGroupsOnTheVPNIdentity is the fixture, and the mutation that
// swaps vpnRoutesKey for unicastRoutesKey is what it was written against.
//
// It takes VPNRouteFilter for VPNRoutes' reasons -- rd, and a family
// vocabulary of vpn4/vpn6/lu4 that route_unicast's column cannot hold -- and
// it validates with that type's own check() rather than a topology-specific
// one. TopologyUnicast needs checkTopology because RouteFilter.predicates
// renders an unconditional `r.prefix = ?`, which turns a router+peer scope
// into a query for the empty prefix; VPNRouteFilter has never had that
// rendering (its "" means "every prefix"), and its check() already refuses a
// filter naming none of prefix, covers, rd, router, origin_asn, through_asn
// or community. So the divergence topologyPredicates exists for does not
// arise here, and inventing a second copy of it would be two identical
// renderings kept in sync by hand.
// TestTopologyVPNNeedsNoPredicateDivergence pins that, so this stops being
// true loudly rather than quietly.
//
// Like TopologyUnicast it ignores f.Limit, answers about the current session's
// up peers only, and reports withdrawn routes as data rather than filtering
// them out. See TopologyUnicast for each of those at length.
//
// It renders no candidate-key semi-join, and that is deliberate rather than
// an omission: vpnRoutesSQL has none either, so this reads the same population
// /v1/routes/vpn does through the same shape of statement. The semi-join is an
// optimization measured on a 1.5M-row unicast peer (see semijoin.go); nothing
// has measured it here, and adding an unmeasured rewrite to a new path is how
// this repo has acquired defects before.
func (q *Q) TopologyVPN(ctx context.Context, f VPNRouteFilter) (Graph, error) {
	if err := f.check(); err != nil {
		return Graph{}, err
	}
	w := f.predicates()
	cte := topologyCTE(q.db, "route_vpn", vpnRoutesKey, w.where(), w.having())
	return q.graph(ctx, cte, w.values())
}

// TopologyEVPN is TopologyUnicast for route_evpn, over the same statement text
// and with the same meaning for every field of Graph.
//
// It groups on route_evpn's own route identity, which shares only its leading
// four columns with the other two: route_type, rd, prefix, mac, ip,
// ethernet_tag, esi and path_id are what tell one EVPN route from another,
// and 544 of the archive's 904 rows have no AS path at all while 136 carry no
// prefix. Grouping on a shorter key -- the obvious one being "the columns EVPN
// shares with unicast" -- collapses every MAC under one RD into a single
// route. TestTopologyEVPNGroupsOnTheEVPNIdentity is the fixture: nine routes
// differing from each other in exactly one identity column apiece.
//
// It validates with EVPNRouteFilter.check() for the reason TopologyVPN uses
// VPNRouteFilter's -- see there -- and it renders no semi-join, for the reason
// given there too.
//
// EVPN's real graph is SMALL. The archive's unscoped EVPN answer is three
// ASNs and two edges, and that is the truth about the network rather than a
// symptom: an EVPN fabric's routes are mostly iBGP inside one AS, and an empty
// or near-empty graph is the ordinary answer here, not a failed query. Graph's
// own doc comment says why Nodes and Edges are non-nil for exactly that case.
func (q *Q) TopologyEVPN(ctx context.Context, f EVPNRouteFilter) (Graph, error) {
	if err := f.check(); err != nil {
		return Graph{}, err
	}
	w := f.predicates()
	cte := topologyCTE(q.db, "route_evpn", evpnRoutesKey, w.where(), w.having())
	return q.graph(ctx, cte, w.values())
}

// graph runs one family's CTE under the three SELECTs and assembles the
// result. It takes the rendered CTE rather than a filter because the three
// families' filters are three different types with three different
// identities -- what they share is the shape of the answer, which is this.
//
// Three statements over one CTE text, rather than three hand-kept-in-sync
// copies of the scope: the edges, the nodes and the population are by
// construction describing the same set of routes. They are three round trips
// and not one snapshot, which is a real limit -- a route withdrawn between
// the edge read and the node read would leave a node with no edge -- and it
// is the same limit /v1/routes and CountRoutes already run concurrently under
// at the handler. A UNION would not fix it either, since the three SELECTs
// have three different column types.
func (q *Q) graph(ctx context.Context, cte string, args []any) (Graph, error) {
	// Non-nil even when the scope matches nothing: see Graph.
	g := Graph{Nodes: []ASNode{}, Edges: []ASEdge{}}

	edges, err := q.conn.Query(ctx, cte+topologyEdgesSQL, args...)
	if err != nil {
		return Graph{}, fmt.Errorf("query topology edges: %w", err)
	}
	defer edges.Close()
	for edges.Next() {
		var e ASEdge
		if err := edges.Scan(&e.Src, &e.Dst, &e.Routes, &e.LiveRoutes, &e.FirstSeen); err != nil {
			return Graph{}, fmt.Errorf("scan topology edge: %w", err)
		}
		g.Edges = append(g.Edges, e)
	}
	if err := edges.Err(); err != nil {
		return Graph{}, fmt.Errorf("read topology edges: %w", err)
	}

	nodes, err := q.conn.Query(ctx, cte+topologyNodesSQL, args...)
	if err != nil {
		return Graph{}, fmt.Errorf("query topology nodes: %w", err)
	}
	defer nodes.Close()
	for nodes.Next() {
		var n ASNode
		if err := nodes.Scan(&n.ASN, &n.Routes, &n.Origin, &n.Transit, &n.Peer,
			&n.FirstSeen); err != nil {
			return Graph{}, fmt.Errorf("scan topology node: %w", err)
		}
		g.Nodes = append(g.Nodes, n)
	}
	if err := nodes.Err(); err != nil {
		return Graph{}, fmt.Errorf("read topology nodes: %w", err)
	}

	if err := q.conn.QueryRow(ctx, cte+topologyRoutesSQL, args...).Scan(&g.Routes); err != nil {
		return Graph{}, fmt.Errorf("count topology routes: %w", err)
	}
	return g, nil
}

// topologyRowsCTE names the row source topologySQL aggregates: a family's
// current table and its history table, UNION ALL. SELECT * is safe across
// the two because each current table is created AS its history table, with
// the same columns in the same order. The WHERE on the statement that reads
// it reaches both tables' primary keys; ClickHouse pushes it into each branch.
const topologyRowsCTE = "topo_rows"
