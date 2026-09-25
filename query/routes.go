package query

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// routesSQL answers "where is this prefix right now, fleet-wide": one row
// per (collector, router, peer, rib, path_id) that is currently
// advertising prefix -- the collector is part of the key because session
// identity is (see peerStateCTE), not because a route is per-collector
// data --
// with route_unicast's competing rows for that route key collapsed to the
// newest by argMax over (seq, stream_seq) -- never ts_router (a router
// with a zero clock reports 1970) and never ts_collector (batch-granular);
// see peerStateCTE's own doc comment for the same reasoning applied to
// peer_events.
//
// It reads route_unicast_current, the table the schema keeps versioned by
// seq alone (see deploy/clickhouse/schema.sql), not route_unicast itself.
// One UPDATE that both withdraws and announces the same prefix is a known,
// unresolved tie: the writer files both rows from one envelope
// (sink/rows.go), so they share seq AND stream_seq, and this argMax picks
// either. That was already true over the history table. A merge of the
// current table keeps one of the two as well, so the answer can change when
// it runs.
//
// Four joins narrow route_unicast down to what is worth reporting:
//
//   - cur scopes r to the CURRENT BMP session of the (collector, router)
//     that observed it -- every one of these four joins carries
//     collector_id for the reason peerStateCTE's own doc comment gives, so
//     a route is never gated on, or decorated with, another collector's
//     view of the same router. A route row from a
//     session the router has since replaced is not stale data to merge
//     with a fresher row for the same key -- BMP re-dumps the whole table
//     from scratch on a new session, so an old-session row with no
//     fresher counterpart this session is not "this prefix, a bit out of
//     date." It is a claim about a session that no longer exists. See
//     TestRoutes's "a session reset discards the prior dump" subtest.
//   - peer_up, filtered by servedGate, drops every route whose
//     advertising peer is down as of the current session. The row can
//     still be sitting in route_unicast -- nothing deletes it when a peer
//     goes down -- but a down peer is not a source for "where is this
//     prefix now." See TestRoutes's "a down peer contributes nothing"
//     subtest. This is peer_up (peerUpCTE), not peer_state: peer_state is
//     scoped to (router, peer, rib), and joining a route to it ON rib
//     drops every route observed under a rib peer_events has no
//     accounting for at all -- see peerStateCTE's own doc comment for the
//     live-archive occurrence this was proven against, and TestRoutes's
//     "a route observed under a rib peer_events never recorded is still
//     reported" subtest for the regression coverage.
//   - peer_state, LEFT-joined (not INNER -- route survival no longer
//     depends on it, only DumpState does), is what lets this query report
//     DumpState per row via dumpStateExpr, reused verbatim rather than
//     re-derived (see that const's own doc comment for why, including why
//     dumpStateExpr coalesces peer_state.state now that this join can
//     leave it unmatched). A route under a rib that peer_state has no row
//     for at all still survives peer_up's gate and is still reported here;
//     it just reads DumpState = "unknown", the honest answer when there is
//     no rib-scoped dump-progress data to consult at all.
//   - eor (eorCTE, shared with vpnRoutesSQL and evpnRoutesSQL -- see its own
//     doc comment for why it groups rather than counts, and why it carries
//     no WHERE), left-joined directly against r and cur rather than through
//     peer_state (peer_state may not have a matching row, per the point
//     above, and eor must not inherit that absence), is what lets this
//     query report DumpState per row via dumpStateExpr. It reads
//     eor_events, not route_unicast: end-of-RIB markers left the route
//     tables for a table of their own, which is what makes this join
//     keyable on the marker's family. The join carries eor.fam =
//     r.family for exactly that reason -- a route's DumpState is its OWN
//     family's dump progress, so an EVPN marker must not tell an ipv4u
//     route that its dump is finished. any(...) wraps the expression here
//     because GROUP BY below is at route-key granularity, finer than
//     peer_state/eor's own (collector, router, peer, rib, family); the
//     expression is still constant within each group -- it depends only on
//     (collector_id, router_ip, peer_ip, rib, family), every one of which
//     this query's own GROUP BY carries outright, family included -- so
//     any() cannot land on a wrong value the way it could if the group
//     mixed several tuples.
//
// live_is_withdraw, live_next_hop and live_as_path -- not is_withdraw,
// next_hop or as_path: ClickHouse resolves a SELECT alias inside that same
// query's WHERE/HAVING/ORDER BY, so aliasing a computed expression back to
// the name of the real route_unicast column it was computed from leaves
// every clause after it one rename away from binding to the alias instead
// of the column -- the same trap that produced the rd/vrf defect in
// l3vpn-rib-browser.json (see peerStateCTE's doc comment for the fuller
// account). Only live_is_withdraw is read back today (by HAVING), but
// live_next_hop and live_as_path are named the same way on principle: a
// filter added on either column later should not have to remember this
// rule to stay safe.
//
// There is no end_of_rib predicate on r any more, and none is needed: a
// marker is BMP's own "the initial table dump for this (router, peer, rib,
// session, family) is done" sentinel, not a route, and it now lands in
// eor_events rather than in route_unicast carrying an empty prefix and an
// empty next hop. This query used to need `r.end_of_rib = 0` to keep
// Routes(ctx, RouteFilter{Prefix: ""}) from returning every one of those
// markers as a route to the empty prefix; splitting the artifact out of the
// data is what makes forgetting the predicate impossible rather than merely
// tested for. See TestRoutes's "an end-of-rib marker is not a route"
// subtest, which still asserts the outcome against the new shape.
//
// The outer WHERE carries servedGate and nothing else of its own:
// the prefix and the four optional narrowings (family, router, peer, rib)
// all arrive together at %[2]s, rendered by RouteFilter.predicates (see
// filters.go). Splicing them at one point rather than several is what keeps
// the bind order trivially equal to the order the fields are added in --
// contrast peersSQL, whose one predicate lands at seven positions and has
// to order its own args by hand. r.prefix is inside that rendering rather
// than left in the statement as a fixed `r.prefix = ?` for the same reason:
// a placeholder in the statement text and a placeholder in the builder
// would be two things to keep in agreement, which is the arrangement this
// whole builder exists to end.
//
// argMax(tuple(r.med), ...).1, never argMax(r.med, ...), and the same for
// local_pref. Both columns are Nullable(UInt32), and ClickHouse's aggregate
// functions SKIP a NULL argument: argMax over a nullable column resolves to
// the newest NON-NULL observation, not to the newest observation's value.
// A route re-advertised without a MED -- an ordinary thing for a router to
// do, and the whole point of the attribute being optional -- would keep
// reporting the MED it used to carry, forever, with no tell anywhere.
// Wrapping the column in a one-element tuple makes the aggregated argument
// itself non-nullable, so no row is skipped, and .1 unwraps the newest row's
// value with its NULL intact. Measured on 24.8.14.39 against this package's
// own test database: over the two rows (seq 1, med 5) and (seq 2, med NULL),
// argMax(med, seq) is 5 and argMax(tuple(med), seq).1 is NULL. See
// TestRoutesReportsPathAttributes' "a cleared MED reads as absent, not as
// the value it used to have", which is the fixture built to make that
// difference visible.
//
// communities is selected as the Array(UInt32) it is stored as and rendered
// into api/openapi.yaml's "65000:100" text in Go, by communityStrings -- see
// that function's own doc comment for why the conversion is deliberately not
// an arrayMap in this statement. large_communities needs no conversion at
// all: sink writes RFC 8092's own canonical text into an Array(String)
// before it ever reaches the table. ext_communities and route_targets, added
// beside it, are the same shape: already-rendered Array(String), selected
// and scanned straight through with no Go-side conversion of their own (see
// Route's own doc comment for why the two overlap by design).
//
// GROUP BY ends in path_id, not just prefix: add-path lets one peer
// advertise the same prefix under several path-ids at once, each a
// distinct, independently-withdrawable route. Grouping on prefix alone --
// the obvious shape -- silently keeps one sibling and drops the rest. See
// TestRoutes's "add-path keeps both path-ids" subtest.
//
// r.family is in that GROUP BY too, beside the route key rather than as part
// of it. Route.Family has to be selected plainly (any(r.family) would be
// this statement asserting the value is constant in the group rather than
// making it so), and the eor join above already resolves each row's
// DumpState against eor.fam = r.family, which any(dumpStateExpr) can only
// read correctly if the group holds one family. Today it cannot hold two --
// an IPv4 prefix's text is never an IPv6 prefix's -- but that is a fact
// about how prefixes are spelled, not a constraint route_unicast enforces,
// and grouping on the column costs nothing to make it structural. Deleting
// r.family from this GROUP BY is not a silent mutation: ClickHouse refuses a
// bare column that is neither grouped nor aggregated, so the statement stops
// running at all -- verified by exactly that mutation, which fails every test
// that calls Routes with code 215.
//
// It does NOT belong in unicastRIBKey (or vpnRIBKey) beside it, and that
// asymmetry is deliberate rather than an oversight in rib.go. Those keys are
// derived from the same GROUP BY, so a reader could reasonably add family to
// them too; what stops that being right is that a page key exists to break
// TIES, and family breaks none -- within one peer and one rib it is
// determined by the rest of the key. Adding it would change every cursor's
// arity and the order pages come back in, and break no tie in exchange. If
// that determination ever stopped holding, the group really would split into
// two rows sharing a page key, and the keys would have to grow family with
// it; nothing else about the walk would.
//
// It ends at HAVING and carries no ORDER BY of its own, because it has two
// callers that need two different ones: Routes appends routesOrder, and
// RIBPageUnicast appends the keyset tail ribOrder builds (see rib.go).
// Everything above that
// line -- the session scoping, the peer_up gate, the argMax dedup, the route
// key -- is identical for both and is shared rather than copied, for the
// reason peerStateCTE's own doc comment gives about its own sharing: the two
// ways to get session scoping wrong are both invisible in a passing test, and
// a second copy is a second chance to get them wrong with only one copy's
// tests watching.
const routesSQL = "WITH " + peerStateCTE + peerUpCTE + eorCTE + `
SELECT
    argMax(r.router_sysname, (r.seq, r.stream_seq)) AS sysname,
    r.router_ip, r.peer_ip, r.collector_id, r.rib, r.family, r.prefix, r.path_id,
    argMax(r.next_hop,    (r.seq, r.stream_seq))    AS live_next_hop,
    argMax(r.as_path,     (r.seq, r.stream_seq))    AS live_as_path,
    argMax(tuple(r.med),        (r.seq, r.stream_seq)).1 AS live_med,
    argMax(tuple(r.local_pref), (r.seq, r.stream_seq)).1 AS live_local_pref,
    argMax(r.communities,       (r.seq, r.stream_seq))   AS live_communities,
    argMax(r.large_communities, (r.seq, r.stream_seq))   AS live_large_communities,
    argMax(r.ext_communities,   (r.seq, r.stream_seq))   AS live_ext_communities,
    argMax(r.route_targets,     (r.seq, r.stream_seq))   AS live_route_targets,
    argMax(r.is_withdraw, (r.seq, r.stream_seq))    AS live_is_withdraw,
    any(` + dumpStateExpr + `)                      AS dump_state
FROM %[1]s.route_unicast_current r
INNER JOIN cur
    ON r.collector_id = cur.collector_id
   AND r.router_ip    = cur.router_ip
   AND r.session_id   = cur.sid
INNER JOIN peer_up
    ON r.collector_id = peer_up.collector_id
   AND r.router_ip    = peer_up.router_ip
   AND r.peer_ip      = peer_up.peer_ip
   AND cur.sid        = peer_up.sid
LEFT JOIN peer_state
    ON r.collector_id = peer_state.collector_id
   AND r.router_ip    = peer_state.router_ip
   AND r.peer_ip      = peer_state.peer_ip
   AND r.rib          = peer_state.rib
   AND cur.sid        = peer_state.sid
LEFT JOIN eor
    ON eor.collector_id = r.collector_id
   AND eor.router_ip    = r.router_ip
   AND eor.peer_ip      = r.peer_ip
   AND eor.rib          = r.rib
   AND eor.session_id   = cur.sid
   AND eor.fam          = r.family
WHERE ` + servedGate + `%[2]s
GROUP BY r.collector_id, r.router_ip, r.peer_ip, r.rib, r.family, r.prefix, r.path_id
HAVING live_is_withdraw = 0%[3]s`

// routesOrder is the ordering Routes reports its answer in, appended to
// routesSQL at the one call site that wants it. It is deliberately NOT part of
// routesSQL: RIBPageUnicast appends the keyset tail ribOrder builds to the
// same body instead, and the two orderings answer different questions. This
// one is presentational -- sysname first, so a fleet-wide answer reads router
// by router -- while the walk's is its correctness condition (see ribOrder).
// Merging them would be wrong for one of the two.
const routesOrder = `
ORDER BY sysname, r.peer_ip, r.path_id, r.collector_id`

// originASN returns the last AS in path, or 0 when there is none.
// as_path[-1] in SQL would also return 0 for an empty array --
// indistinguishable from the reserved AS 0 -- which is exactly the defect
// fixed in the L3VPN RIB browser on 2026-08-23. Deriving it here instead does
// not, on its own, make 0 mean anything different: it still means both
// "no path" and "the real origin is AS 0". What actually restores the
// distinction is that Route always carries ASPath alongside OriginASN --
// see Route's own doc comment -- so a caller checks len(ASPath) == 0
// rather than trusting OriginASN in isolation. originASN has no error
// return of its own for the same reason: there is nothing for it to
// report that ASPath does not already say.
func originASN(path []uint32) uint32 {
	if len(path) == 0 {
		return 0
	}
	return path[len(path)-1]
}

// communityStrings renders RFC 1997 communities into the "<high>:<low>"
// notation api/openapi.yaml's communities schema asks for ("Standard
// notation, \"65000:100\"") from the Array(UInt32) all three route tables
// actually store. It is shared by all three scan loops rather than copied
// into each, for the reason originASN is.
//
// The conversion lives in Go, not in the statement, and that is the same
// decision -- taken against the same shipped defect -- that `r.rd AS vrf`
// records. ClickHouse resolves a SELECT alias inside that same query's
// WHERE, so an arrayMap rendering these into text in the SELECT list would
// put a rendered value one rename away from a predicate that meant to filter
// the real column. That is precisely what l3vpn-rib-browser.json shipped:
// a rendered RD placeholder aliased back to rd, and a filter that then
// matched the rendering instead of the data, making the RD-less family
// invisible to its own filter. No /v1 path filters on communities today,
// which is exactly when the cheap version of this decision is available --
// the column stays numeric all the way to Go, so a filter added later has
// nothing but the numeric column to name, and `has(communities, ?)` over an
// Array(UInt32) can use the column as stored where matching text could not.
//
// The third reason is the one originASN takes for as_path[-1]: a pure
// function is testable without a database, so the mapping itself can be
// pinned by a unit test rather than only observed through a live query.
//
// The two halves are the community's own unsigned 16-bit halves, with no
// name mapping. RFC 1997's well-known values come back as numbers --
// NO_EXPORT (0xFFFFFF01) reads "65535:65281" -- for the reason
// EVPNRoute.RouteType gives for not naming EVPN route types: the contract
// asks for notation, not nomenclature, and a name table this package
// invented would ship with nothing behind it.
//
// It returns an EMPTY, non-nil slice for a route carrying no communities,
// never nil. api/openapi.yaml has communities in RouteCommon.required and
// types it as an array, and a nil slice marshals to JSON null rather than
// []. That is also exactly what clickhouse-go's own scan hands back for an
// empty Array(String) -- verified against 24.8.14.39 -- which is what
// LargeCommunities beside it gets for free, so the two community fields
// agree on the shape of "none" instead of differing by which of them Go
// built.
func communityStrings(cs []uint32) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, strconv.FormatUint(uint64(c>>16), 10)+":"+
			strconv.FormatUint(uint64(c&0xffff), 10))
	}
	return out
}

// Routes reports where f.Prefix is advertised right now, fleet-wide: one row
// per (collector, router, peer, rib, path_id) whose current-session, up
// peer is still advertising it (see Route.Collector for why the collector
// is part of that key). See routesSQL's own doc comment for what "current" and
// "still advertising" mean here, and Route.DumpState's doc comment for why
// an empty result is not, by itself, proof the prefix is nowhere in the
// network.
//
// f names what is being asked about in one of two ways, never both:
// f.Prefix matches a prefix exactly, and f.Covers reports every prefix that
// CONTAINS an address, at every length -- api/openapi.yaml's ?prefix= and
// ?covers=, which it documents as mutually exclusive. Covers is a full scan
// by construction and says so in the contract; see filters.covers for why no
// index can serve containment and why the predicate is written to be total.
// An empty f.Covers means "not asked", leaving f.Prefix to render, and an
// empty f.Prefix is the empty prefix -- a real question with a real (empty)
// answer, not a request for every route in the table.
//
// f's Family, Router, Peer and RIB narrow that answer further and are each
// optional -- an unset one is omitted from the statement rather than
// compared against anything (see RouteFilter, and eqAddr for why the
// distinction is not merely tidiness).
//
// A Family that is not one of route_unicast's own is rejected before the
// statement is built, wrapping ErrBadFilter, as are a Covers that is not an
// address and a Covers set alongside a Prefix. Family's check has to live in
// Go:
// family is LowCardinality(String), so an unknown one would match nothing
// and return the empty result a caller cannot tell from a correct one --
// unlike RIB, whose Enum8 makes ClickHouse itself raise (see
// unicastFamilies, and TestRoutesRejectsARibThatIsNotAnEnumMember for the
// same argument made about rib).
//
// f.Limit caps the number of rows RETURNED, not the number matched -- 0
// means no cap, so every caller written before this field existed asks
// exactly the same question it always did. The cap is applied as a bare
// `LIMIT %d` after routesOrder, never as a bound `?`: ClickHouse does not
// accept a placeholder in LIMIT, and there is nothing unsafe about the
// literal either, because the value never comes from a caller's text --
// none of the four /v1/routes* paths documents a ?limit= at all (that
// parameter is api/openapi.yaml's components.parameters.limit, $ref'd by
// the three /v1/rib/* walks alone). Every route handler sets f.Limit from the
// operator's own s.cfg.MaxPage instead, an int the daemon's config already
// validated at startup. See CountRoutes for how a caller learns whether a
// capped answer left anything out.
func (q *Q) Routes(ctx context.Context, f RouteFilter) ([]Route, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	w := f.predicates()
	semi, args := routesSemiJoin(q.db, "route_unicast_current", unicastRoutesKey, w)
	stmt := fmt.Sprintf(routesSQL+routesOrder, q.db, w.where()+semi, w.having())
	if f.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query routes: %w", err)
	}
	defer rows.Close()
	return scanRoutes(rows)
}

// CountRoutes reports how many routes match f, ignoring f.Limit entirely --
// it is the answer to "how much did you not show me" for a Routes call
// capped by Limit, and api/openapi.yaml's meta.total_matched is this number
// on the wire. Without it, a caller handed a capped page has no way to tell
// a complete answer from a truncated one: an agent told "here are the 100
// routes AS 64512 originates" would report that as the true count when the
// AS in fact originates 3,250 of them across 501 prefixes -- the exact
// silent-truncation failure this whole package is written against, now one
// layer up from a missing predicate.
//
// It wraps routesSQL -- the identical statement Routes builds, minus
// routesOrder -- rather than re-deriving f's predicate as a second SQL
// fragment. A hand-written second version is exactly how a count could drift
// from the query it describes, and a count that disagrees with what Routes
// actually returns is worse than no count at all: a confident wrong total,
// not an honest missing one. Wrapping in `SELECT count() FROM (...)` counts
// the statement's own GROUP BY/HAVING output -- one row per live route key --
// which is the number CountRoutes has to report; a bare `SELECT count() FROM
// route_unicast WHERE ...` would count raw observations instead, inflated by
// every superseded row argMax discards and by the ReplacingMergeTree
// duplicates counts_test.go's own fixture manufactures.
//
// routesOrder is deliberately NOT part of what gets wrapped: ordering the
// inner query's rows is work whose result is discarded the moment count()
// runs over it, since a COUNT has no notion of the order its input arrived
// in.
func (q *Q) CountRoutes(ctx context.Context, f RouteFilter) (uint64, error) {
	if err := f.check(); err != nil {
		return 0, err
	}
	w := f.predicates()
	// The count carries the semi-join for the same reason it wraps routesSQL
	// rather than re-deriving the predicate: it is the identical statement
	// under a count(), and a count that ran a different query from the page it
	// describes is a confident wrong total. It is also the half that most
	// needs the rewrite -- api/handlers.go runs the two concurrently, so the
	// pair costs the max of them, and before this both halves paid the full
	// whole-scope aggregate.
	semi, args := routesSemiJoin(q.db, "route_unicast_current", unicastRoutesKey, w)
	inner := fmt.Sprintf(routesSQL, q.db, w.where()+semi, w.having())
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count routes: %w", err)
	}
	return n, nil
}

// scanRoutes drains rows into Routes' own answer shape. It is split out of
// Routes because RIBPageUnicast reads the identical SELECT list -- both
// statements are routesSQL with a different ORDER BY appended -- and a second
// copy of this loop would be a second place for the column order, the
// discarded live_is_withdraw, the next_hop parse and the unmap to drift from
// the statement that feeds them. Scan is positional: a column added to
// routesSQL and forgotten here fails loudly, but a column REORDERED there and
// not here binds a prefix to a rib without a word.
func scanRoutes(rows driver.Rows) ([]Route, error) {
	var out []Route
	for rows.Next() {
		var r Route
		var nextHop string
		// live_communities is scanned as the Array(UInt32) route_unicast
		// stores and rendered by communityStrings below, rather than scanned
		// straight into r.Communities: the wire form is text, the column is
		// numeric, and the conversion is deliberately on this side of the
		// line (see communityStrings' own doc comment).
		var communities []uint32
		// live_is_withdraw is scanned and discarded: it exists so HAVING
		// has an alias to filter on that cannot collide with
		// route_unicast's own is_withdraw column (see routesSQL's doc
		// comment), not because a caller of Routes needs it -- HAVING
		// already guarantees every row reaching here has
		// live_is_withdraw = 0.
		var liveIsWithdraw uint8
		if err := rows.Scan(
			&r.RouterSysName, &r.RouterIP, &r.PeerIP, &r.Collector,
			&r.RIB, &r.Family, &r.Prefix, &r.PathID,
			&nextHop, &r.ASPath, &r.MED, &r.LocalPref, &communities, &r.LargeCommunities,
			&r.ExtCommunities, &r.RouteTargets,
			&liveIsWithdraw, &r.DumpState,
		); err != nil {
			return nil, fmt.Errorf("scan route: %w", err)
		}
		r.Communities = communityStrings(communities)
		// route_unicast.next_hop is a plain String column -- it has to
		// carry both IPv4 and IPv6 next hops, so it is not typed IPv6 the
		// way router_ip and peer_ip are -- so parsing here, rather than
		// scanning straight into a netip.Addr, is this function's own job
		// rather than the driver's.
		//
		// A parse failure is deliberately not a query-wide error: one
		// route with an unparseable next hop (a vendor quirk, say) must
		// not cost the caller every other row this prefix legitimately
		// has, for a "where is this right now, fleet-wide" surface that
		// dashboards and an LLM both read a partial answer from. On
		// failure, ParseAddr already hands back the zero netip.Addr, whose
		// String() renders "invalid IP" -- self-describing at the field
		// level, rather than a plausible-looking wrong address -- so the
		// error is simply not propagated.
		r.NextHop, _ = netip.ParseAddr(nextHop)
		r.OriginASN = originASN(r.ASPath)
		// Every address this loop hands back goes through unmapAll, next
		// hop included -- see its own doc comment for why one row carrying
		// two textual forms of one address is a defect and not a cosmetic
		// difference. NextHop is not exempt: netip.ParseAddr("::ffff:10.0.0.1")
		// parses to the mapped form, and next_hop is a String column holding
		// whatever the decoder wrote (bgp/update.go's netip.AddrFrom16 path
		// produces exactly that text). Unmap on the zero Addr a failed parse
		// leaves behind is the zero Addr, so the invalid case passes through
		// untouched.
		unmapAll(&r.RouterIP, &r.PeerIP, &r.NextHop)
		out = append(out, r)
	}
	return out, rows.Err()
}
