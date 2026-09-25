package query

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// withdrawSentinel is 0x800000 >> 4 -- RFC 3107's own reserved value for
// "this NLRI is being withdrawn, not advertised", which occupies the
// label field of a withdrawn labeled route exactly where a real MPLS
// label goes. On the live archive it appears on exactly the 2 rows with
// is_withdraw = 1 and no others -- has(labels, 524288) and is_withdraw = 1
// pick out the identical 2 of route_vpn's 164 rows -- so vpnRoutesSQL's
// own HAVING live_is_withdraw = 0 (see its doc comment) already removes
// every row that carries it, on today's data. label() below is kept
// anyway, as a second, independent guard rather than the only one: a
// vendor emitting this sentinel on a row it does not also mark
// is_withdraw would be exactly the kind of quirk this repo keeps finding,
// and nothing about the sentinel's value ties it to is_withdraw at the
// protocol level -- only convention does, and this codebase already
// shipped once on trusting a convention it never checked
// (looking-glass-vpn.json, which printed the sentinel as a label until
// 2026-08-23).
const withdrawSentinel = 524288

// label reports the MPLS label a VPN route carries, and false when it
// carries none -- either because the newest observation's labels array was
// empty outright, or because its one element is the withdraw sentinel.
//
// It takes the whole labels array, not labels[1] pre-extracted by SQL: a
// genuinely empty array and the RFC 3107 withdraw sentinel are two
// different reasons a route has no usable label, but ClickHouse's array
// subscript operator collapses the first one into a value indistinguishable
// from a real label. labels[1] on an empty array returns the element
// type's zero value, 0 -- exactly the implicit-null label a router can
// genuinely advertise -- so extracting the element in SQL before this
// function ever runs would silently turn "no labels" into "label 0,
// HasLabel = true", the identical ambiguity this whole Label/HasLabel pair
// exists to remove. Inspecting the array itself is what lets len(labels)
// == 0 be told apart from labels[0] == withdrawSentinel.
//
// Both branches are defense in depth here, not the primary guard: on the
// live archive, every row carrying the sentinel is also a withdrawal, and
// vpnRoutesSQL's HAVING live_is_withdraw = 0 already excludes every one of
// those (see withdrawSentinel's own doc comment), and no live row carries
// an empty labels array at all. TestVPNRoutes' "a live route whose labels
// carry the withdraw sentinel still reports no label" and "a route with no
// labels reports no label, not label 0" subtests exercise both branches
// directly anyway, against fixture rows manufactured for exactly this
// shape -- the sentinel case against a row that is not withdrawn but
// carries the sentinel, the empty-array case against a row the live
// archive has no counterpart for at all. Returning (0, false) rather than
// plain 0 in either case keeps "no label" distinguishable from the
// implicit-null label, which really is 0 and which a router genuinely
// advertises. See looking-glass-vpn.json's own fix (2026-08-23) for
// the panel that rendered the sentinel beside ordinary labels before this
// distinction existed anywhere in this codebase.
func label(labels []uint32) (uint32, bool) {
	if len(labels) == 0 {
		return 0, false
	}
	if labels[0] == withdrawSentinel {
		return 0, false
	}
	return labels[0], true
}

// vpnRoutesSQL answers the question routesSQL does -- "where is this
// prefix right now, fleet-wide" -- one level further down, at the
// granularity route_vpn actually carries: one row per (collector, router,
// peer, rib, rd, prefix, path_id) rather than per (collector, router, peer,
// rib, prefix, path_id), because a route distinguisher is part of a VPN
// route's
// identity in a way a plain unicast route has no counterpart for. Two
// VRFs can carry the very same prefix as two entirely unrelated routes;
// see TestVPNRoutes' "one prefix in two VRFs stays two routes" subtest,
// which is fixture-only -- the live archive's 164 route_vpn rows never
// put one prefix under two RDs, so nothing there can falsify a query that
// gets this wrong.
//
// r.rib and r.path_id are both selected as well as grouped: they are part
// of route_vpn's real route key the same way they are on route_unicast's
// (see Route's own doc comment on PathID). Grouping by both while
// selecting neither would not be a correctness bug in the query itself --
// dropping either from GROUP BY would let argMax blend two genuinely
// different routes (an in_pre row with an out_pre row, or two add-path
// siblings) into one answer, and nothing here does that -- but two such
// sibling rows would come back identical in every field VPNRoute exposes,
// which is indistinguishable from the query having collapsed them after
// all. Selecting both plainly, the same way router_ip, peer_ip and prefix
// already are, is what makes two such siblings distinguishable in the
// answer VPNRoutes actually returns. A lab archive never exercised either
// case (every route_vpn row observed there is in_pre with path_id 0), which
// is exactly why this has to be reasoned about rather than left for the
// archive to prove.
// ORDER BY carries both columns too, for the same reason: without them,
// two siblings differing only in rib or path_id have no tie-break at all,
// so which one lands in got[0] versus got[1] is unspecified and a test
// indexing either would flake on nothing more than execution order.
//
// r.rd AS vrf, not r.rd AS rd: ClickHouse resolves a SELECT alias inside
// that same query's WHERE/HAVING/ORDER BY, so naming this column back to
// the name of the real route_vpn column it is drawn from would leave any
// later clause added on rd one rename away from binding to whichever
// expression carries that alias instead of the column -- exactly the
// defect that shipped in l3vpn-rib-browser.json, where a rendered
// "(none: this family carries no RD)" fallback aliased to rd made
// match(rd, …) filter the fallback string, and the RD-less family matched
// nothing. This query DOES filter on rd now -- VPNRouteFilter.RD renders
// `r.rd = ?` into the outer WHERE -- which is precisely the clause this
// paragraph was written in anticipation of. It is safe because it names
// r.rd, the column, and because the alias is `vrf` rather than `rd`, so
// there is no expression for the predicate to bind to by mistake. See
// peerStateCTE's and routesSQL's own doc comments for the fuller account.
//
// HAVING live_is_withdraw = 0 excludes a route whose newest observation was
// a withdrawal, the same way routesSQL excludes one from Routes: "where is
// this prefix right now" is not answered by a route that was just withdrawn,
// and VPNRoute has nowhere to expose is_withdraw for a caller to filter one
// out for themselves after the fact. It is tempting to omit this clause, on
// the theory that Label/HasLabel already handles the one artifact a
// withdrawal leaves behind (the RFC 3107 sentinel -- see withdrawSentinel's
// own doc comment). That is wrong, not merely incomplete: without this
// clause VPNRoutes reports a withdrawn route as though it were still live,
// with a plausible-looking NextHop, RD and RouteTargets sitting right beside
// its (correctly) absent Label. See TestVPNRoutes' "a withdrawn VPN route is
// not returned at all" subtest, and live_is_withdraw's own naming -- not
// is_withdraw -- for the same alias-shadowing reason r.rd is aliased vrf
// above.
//
// argMax(r.labels, ...), the whole labels array, not labels[1] extracted in
// SQL: every route_vpn row observed on the live archive carries exactly one
// label, but ClickHouse's array subscript operator returns the element
// type's zero value on an out-of-range index rather than raising, so a row
// whose newest observation carried no label at all would degrade to
// labels[1] = 0 here -- indistinguishable from a real implicit-null label
// -- before label() (see its own doc comment) ever got a chance to tell the
// two apart. Selecting the array and letting label() inspect it in Go is
// what makes that distinction possible at all; extracting the element in
// SQL first would have already destroyed the information label() needs.
//
// Both joins carry collector_id, the same way routesSQL's four do and for
// the reason peerStateCTE's own doc comment gives: session identity is
// (collector, router), so a VPN route is scoped to the current session of
// the collector that observed it and gated on the peer state that same
// collector recorded.
//
// The outer WHERE takes the same shape routesSQL's does: servedGate and
// nothing else of its own, with all six of /v1/routes/vpn's
// documented narrowings (prefix, rd, family, router, peer, rib) arriving
// together at %[2]s from VPNRouteFilter.predicates. Both statements alias
// their route table r -- see RouteFilter.predicates' own doc comment for why
// that alias is hard-coded in the builder rather than passed in.
//
// Every one of those six is optional here, r.prefix included, which is the
// one place this statement's filter differs from routesSQL's: an empty
// filter would render to no predicate at all and dump route_vpn whole.
// VPNRouteFilter.check is what stops that, and it is the reason VPNRoutes
// validates before it renders rather than after.
//
// r.rd is filtered on the real column, never on the `vrf` alias two lines
// down in the SELECT list. That is not a stylistic preference: ClickHouse
// resolves a SELECT alias inside the same query's WHERE, so `WHERE rd = ?`
// here would bind to whichever expression happened to carry that alias --
// the precise mechanism behind l3vpn-rib-browser.json's own RD defect,
// described in the paragraph above. Aliasing the column to `vrf` in the
// first place is what makes r.rd unambiguous now that a clause finally does
// filter on it; the hazard this file called "dormant, not absent" is the one
// that alias was chosen against.
//
// Route survival is gated on peer_up, NEVER on peer_state, and the two
// joins below are not interchangeable even though both are derived from
// peer_events. peer_state is keyed on (collector, router, peer, rib), so
// joining a route to it ON rib and dropping the unmatched rows deletes every
// route observed under a rib peer_events has no accounting for at all,
// regardless of whether the peer's actual BGP session is up -- see
// peerStateCTE's own doc comment for the live-archive occurrence (3 in_post
// route rows against zero in_post peer_events rows) and peerUpCTE's for the
// rib-independent answer that replaces it. So peer_up is INNER-joined and
// filtered by servedGate, exactly as routesSQL does, and that is the only
// thing deciding whether a VPN route reaches the caller.
//
// peer_state is LEFT-joined beside it, purely so dumpStateExpr has the
// rib-scoped peer state it reads. This statement carried no peer_state join
// at all until VPNRoute grew DumpState: at that point the choice was between
// re-scoping route survival to peer_state (the defect above, reintroduced on
// this table) and adding an optional join that can leave every peer_state
// column unmatched without costing a route. A VPN route under a rib
// peer_state has no row for still survives peer_up's gate and is still
// reported; it just reads DumpState = "unknown", which is the honest answer
// when there is no rib-scoped dump-progress data to consult at all. See
// TestVPNRoutesReportsARouteObservedUnderARibPeerEventsNeverRecorded, the
// route_vpn counterpart of the unicast test of the same name.
//
// eor (eorCTE) is left-joined against r and cur directly rather than through
// peer_state, which may have no matching row per the paragraph above and
// whose absence eor must not inherit. eor.fam = r.family is what keeps a
// route's DumpState its OWN family's dump progress: route_vpn holds three
// families at once (vpn4, vpn6, lu4), so without that predicate a peer's
// finished lu4 dump would report its still-arriving vpn4 routes as complete.
// any(...) wraps dumpStateExpr because this GROUP BY is at route-key
// granularity, finer than peer_state/eor's own (collector, router, peer,
// rib, family) -- the expression is constant within each group, since every
// column it depends on is functionally determined by columns this GROUP BY
// carries, so any() cannot land on a wrong value the way it could if the
// group mixed several tuples.
//
// r.family is one of those columns, and it is in the GROUP BY rather than
// merely assumed constant. Until VPNRoute grew a Family field this statement
// grouped without it and relied on a property of the DECODER for that
// assumption to hold: three families share this table, and the only thing
// keeping an lu4 row (rd = "", 74 of the live archive's 164) from sharing a
// route key with a vpn4 row is that bgp.decodeRD never returns "" without
// failing the whole NLRI parse. Nothing in route_vpn's schema says so, and a
// group that did mix two families would hand any(dumpStateExpr) two
// different eor.fam matches to choose between -- one family's finished dump
// declaring another's, the exact question eor_events was split out of
// route_unicast to make answerable. Grouping on the column costs nothing and
// makes it structural. It is also not a silent thing to delete: r.family is
// selected plainly for VPNRoute.Family, so ClickHouse refuses the statement
// outright if it is neither grouped nor aggregated -- verified by exactly
// that mutation, which fails with code 215. See routesSQL for the same
// paragraph applied to route_unicast, including why vpnRIBKey deliberately
// does not carry family even though this GROUP BY does.
//
// argMax(tuple(r.med), ...).1 and its local_pref twin are NOT
// argMax(r.med, ...): ClickHouse's aggregates skip a NULL argument, so
// argMax over a Nullable column resolves to the newest NON-NULL observation
// rather than the newest observation's value, and a route re-advertised
// without a MED would keep reporting the one it used to carry. See
// routesSQL's own doc comment for the measurement behind that claim.
// communities is selected as the Array(UInt32) it is stored as and rendered
// in Go by communityStrings; large_communities needs no rendering at all,
// and neither does ext_communities, added beside it -- both are already
// rendered Array(String) (see Route's own doc comment on ExtCommunities and
// RouteTargets). route_targets needed no addition here: this statement
// already selects it as live_route_targets.
const vpnRoutesSQL = "WITH " + peerStateCTE + peerUpCTE + eorCTE + `
SELECT
    argMax(r.router_sysname, (r.seq, r.stream_seq)) AS sysname,
    r.router_ip, r.peer_ip, r.collector_id, r.rib, r.family, r.path_id, r.rd AS vrf, r.prefix,
    argMax(r.next_hop,      (r.seq, r.stream_seq)) AS live_next_hop,
    argMax(r.as_path,       (r.seq, r.stream_seq)) AS live_as_path,
    argMax(r.labels,        (r.seq, r.stream_seq)) AS live_labels,
    argMax(r.route_targets, (r.seq, r.stream_seq)) AS live_route_targets,
    argMax(tuple(r.med),        (r.seq, r.stream_seq)).1 AS live_med,
    argMax(tuple(r.local_pref), (r.seq, r.stream_seq)).1 AS live_local_pref,
    argMax(r.communities,       (r.seq, r.stream_seq))   AS live_communities,
    argMax(r.large_communities, (r.seq, r.stream_seq))   AS live_large_communities,
    argMax(r.ext_communities,   (r.seq, r.stream_seq))   AS live_ext_communities,
    argMax(r.is_withdraw,   (r.seq, r.stream_seq)) AS live_is_withdraw,
    any(` + dumpStateExpr + `)                     AS dump_state
FROM %[1]s.route_vpn_current r
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
GROUP BY r.collector_id, r.router_ip, r.peer_ip, r.rib, r.family, r.rd, r.prefix, r.path_id
HAVING live_is_withdraw = 0%[3]s`

// vpnRoutesOrder is the ordering VPNRoutes reports its answer in, appended to
// vpnRoutesSQL at the one call site that wants it, exactly as routesOrder is
// to routesSQL and for the same reason: RIBPageVPN appends the tail ribOrder
// builds to this same body instead, and a presentational ordering and a keyset
// walk's correctness condition cannot be the same clause. See vpnRoutesSQL's own doc
// comment for why rib and path_id are in this one -- without them, two
// siblings differing only there have no tie-break at all.
//
// It orders on `vrf`, the SELECT alias, where the walk's tail orders on r.rd,
// the column (see vpnRIBKey). Both are correct here because the alias is not the column's own
// name (see vpnRoutesSQL on why r.rd is aliased vrf at all); the walk uses the
// column because its ORDER BY and its keyset predicate have to name the same
// thing, and the predicate is in the WHERE, where no SELECT alias of this
// statement may be relied on.
const vpnRoutesOrder = `
ORDER BY sysname, r.peer_ip, vrf, r.rib, r.path_id, r.collector_id`

// VPNRoutes reports where f.Prefix is advertised right now, fleet-wide,
// across every route distinguisher that carries it -- VPNRoutes is to
// route_vpn what Routes is to route_unicast. See vpnRoutesSQL's own doc
// comment for what makes this different from Routes rather than a copy of
// it with an extra column: the route key carries an RD, and one prefix under
// two RDs is two routes.
//
// Every VPNRoute carries DumpState, the same per-family, per-(router, peer,
// rib) reading Route.DumpState has, and for the same reason: an empty result
// and a partial one are otherwise indistinguishable. A half-delivered VPN
// table reported as a complete one is precisely the confidently-wrong answer
// this package exists to prevent.
//
// It takes VPNRouteFilter rather than the RouteFilter Routes does, for the
// two dimensions route_unicast has nowhere to put: RD, and a Family
// vocabulary of vpn4/vpn6/lu4. Router, Peer and RIB mean exactly what they
// do on RouteFilter. Prefix does NOT -- here "" means "every prefix", not
// "the empty prefix" -- which is the whole reason the two are separate
// structs; see VPNRouteFilter.Prefix.
//
// Two ways of asking are refused outright, both wrapping ErrBadFilter so the
// HTTP layer can answer 400 rather than 500 (see VPNRouteFilter.check):
//
//   - A filter naming none of Prefix, RD or Router. api/openapi.yaml
//     requires at least one of the three on /v1/routes/vpn, and the
//     alternative is not a smaller answer but the whole table -- an
//     unpaginated dump of route_vpn, which is what /v1/rib/vpn's cursor
//     walk exists to serve instead.
//   - A Family that is not one of route_vpn's own. An unknown family
//     matches nothing rather than raising (see vpnFamilies), so silence
//     here would hand back an empty result indistinguishable from a
//     correct one.
//
// f.Limit caps the rows RETURNED, not the rows matched -- see
// RouteFilter.Limit for the full argument, which applies here unchanged: 0
// means no cap, the cap is a bare `LIMIT %d` appended after vpnRoutesOrder
// because ClickHouse refuses a bound placeholder there, and CountVPNRoutes
// is how a caller learns whether a capped answer left anything out.
func (q *Q) VPNRoutes(ctx context.Context, f VPNRouteFilter) ([]VPNRoute, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	w := f.predicates()
	stmt := fmt.Sprintf(vpnRoutesSQL+vpnRoutesOrder, q.db, w.where(), w.having())
	if f.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query vpn routes: %w", err)
	}
	defer rows.Close()
	return scanVPNRoutes(rows)
}

// CountVPNRoutes reports how many VPN routes match f, ignoring f.Limit --
// VPNRoutes' counterpart to CountRoutes, and for the identical reason (see
// its doc comment): a capped page needs a true total or a caller cannot tell
// a complete answer from a truncated one, and that total has to come from
// the same statement VPNRoutes runs rather than a hand-derived copy of its
// predicate that could drift from it.
//
// It wraps vpnRoutesSQL without vpnRoutesOrder, the same shape CountRoutes
// wraps routesSQL without routesOrder: the inner query's GROUP BY/HAVING
// output is what has to be counted -- one row per live (collector, router,
// peer, rib, rd, prefix, path_id) -- and ordering rows that are only ever
// counted is work with no effect on the answer.
func (q *Q) CountVPNRoutes(ctx context.Context, f VPNRouteFilter) (uint64, error) {
	if err := f.check(); err != nil {
		return 0, err
	}
	w := f.predicates()
	inner := fmt.Sprintf(vpnRoutesSQL, q.db, w.where(), w.having())
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", w.values()...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count vpn routes: %w", err)
	}
	return n, nil
}

// scanVPNRoutes drains rows into VPNRoutes' own answer shape, split out of
// VPNRoutes for the reason scanRoutes is split out of Routes: RIBPageVPN
// reads the identical SELECT list -- both statements are vpnRoutesSQL with a
// different ORDER BY appended -- and a second copy of this loop would be a
// second place for the column order, the labels array label() needs whole,
// the next_hop parse and the unmap to drift from the statement that feeds
// them.
func scanVPNRoutes(rows driver.Rows) ([]VPNRoute, error) {
	var out []VPNRoute
	for rows.Next() {
		var r VPNRoute
		var nextHop string
		var rawLabels []uint32
		// live_communities is scanned as the Array(UInt32) route_vpn stores
		// and rendered by communityStrings below, for the reason that
		// function's own doc comment gives -- the same split scanRoutes makes.
		var communities []uint32
		// live_is_withdraw is scanned and discarded: it exists so HAVING
		// has an alias to filter on that cannot collide with route_vpn's
		// own is_withdraw column (see vpnRoutesSQL's doc comment), not
		// because a caller of VPNRoutes needs it -- HAVING already
		// guarantees every row reaching here has live_is_withdraw = 0.
		var liveIsWithdraw uint8
		if err := rows.Scan(
			&r.RouterSysName, &r.RouterIP, &r.PeerIP, &r.Collector,
			&r.RIB, &r.Family, &r.PathID, &r.RD, &r.Prefix,
			&nextHop, &r.ASPath, &rawLabels, &r.RouteTargets,
			&r.MED, &r.LocalPref, &communities, &r.LargeCommunities,
			&r.ExtCommunities,
			&liveIsWithdraw, &r.DumpState,
		); err != nil {
			return nil, fmt.Errorf("scan vpn route: %w", err)
		}
		r.Communities = communityStrings(communities)
		// See routes.go's identical next_hop parse for why a failure here
		// degrades the field instead of failing the whole call: one VPN
		// route with an unparseable next hop must not cost the caller
		// every other route this prefix legitimately has.
		r.NextHop, _ = netip.ParseAddr(nextHop)
		r.Label, r.HasLabel = label(rawLabels)
		// originASN, not a second copy of it: see Route's own doc comment
		// and originASN's, in routes.go, for why this has to be derived in
		// Go rather than as as_path[-1] in SQL -- route_vpn is in fact
		// where that defect was first proven (142 of its 164 rows carry no
		// AS path), before l3vpn-rib-browser.json fixed it on
		// 2026-08-23.
		r.OriginASN = originASN(r.ASPath)
		// Every address this loop hands back goes through unmapAll, next
		// hop included -- see its own doc comment, and Routes' own scan loop
		// for why netip.ParseAddr is not the exemption it looks like.
		unmapAll(&r.RouterIP, &r.PeerIP, &r.NextHop)
		out = append(out, r)
	}
	return out, rows.Err()
}
