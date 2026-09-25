package query

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
)

// evpnFamily is the family token route_evpn's rows belong to. It is DERIVED
// from the family-token registry via subjects.FamilyToken rather than
// written here as "evpn", and that indirection is the whole point of the
// shape.
//
// route_evpn has no family column -- an EVPN table holds exactly one family
// -- so evpnRoutesSQL has to supply the token itself when it joins eor_events,
// whose own family column sink fills with subjects.FamilyToken(f). Spell it
// as a string here and the two ends of that join are an untethered copy of
// one registry: rename familyNames[{AFI: 25, SAFI: 70}] in subjects and
// production starts writing the new token into eor_events.family while this
// statement keeps joining on the old one, with every EVPN route in the fleet
// reading DumpState = "dumping" forever and no error anywhere. Naming the
// bgp.Family instead -- an AFI/SAFI pair, which is what RFC 7432 fixes and
// what no rename can move -- makes the token looked up rather than
// remembered.
//
// This is the same coupling TestPeersEVPNFamilyLiteralMatchesSubjectsFamilyToken
// pins for peersSQL's own 'evpn' literal, and the same one sink's
// TestFamilyNameMatchesSubjectsFamilyToken pins for the write side -- see
// either for the full account of the failure, which this repo has already
// been bitten by once. Unlike peersSQL's, this one needs no SQL-text
// assertion to hold it: the token is a bound value here, not a literal in
// the statement, so the compiler carries the coupling.
var evpnFamily = subjects.FamilyToken(bgp.FamilyEVPN)

// evpnRoutesSQL answers "which EVPN NLRIs are live right now, fleet-wide"
// the way routesSQL and vpnRoutesSQL answer it for their own tables, at the
// granularity route_evpn actually carries: one row per (collector, router,
// peer, rib, route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id).
//
// That GROUP BY is the whole NLRI tuple, and every component of it is load
// bearing rather than defensive. An EVPN route is not identified by a
// prefix -- a type-2 MAC/IP route has no prefix at all, and a type-3 IMET
// route has neither prefix nor MAC nor IP -- so there is no shorter key to
// fall back on. Two rows differing only in esi are two routes for the
// identical reason two rows differing only in path_id are (see Route's own
// doc comment on PathID), and the live archive cannot prove that: its 202
// rows carry exactly two distinct ESI values, the all-zero one on every
// type-2 and type-5 row and the empty one on every type-3 row, never two
// under the same NLRI. TestEVPNRoutes' "two routes differing only in ESI
// are two routes" subtest is fixture-only for that reason. What the archive
// DOES prove is that the key cannot be shortened: its 202 rows collapse to
// 16 distinct tuples, and the type-2 rows under one rd and mac differ only
// in ip -- so a key missing ip merges an "MAC only" advertisement with the
// "MAC plus IP" one and reports one of them, plausibly, as the answer.
//
// The columns of that key are selected plainly and unaliased, the way
// routesSQL selects r.prefix: they are real columns, so no SELECT alias
// exists for a later WHERE to bind to by mistake. Only the argMax
// expressions are aliased, and every one of them is prefixed live_ rather
// than named back to its source column, for the alias-shadowing reason
// peerStateCTE's doc comment gives at length. r.rd needs no `AS vrf`
// treatment here (contrast vpnRoutesSQL, which does alias it): nothing in
// this statement computes an expression over rd, so there is no second
// meaning for the name to pick up.
//
// Route survival is gated on peer_up and nothing else, and peer_state is
// LEFT-joined only so dumpStateExpr has the rib-scoped state it reads.
// See vpnRoutesSQL's own doc comment for the full argument, which applies
// here unchanged: joining a route to peer_state ON rib and dropping the
// unmatched rows deletes every route observed under a rib peer_events has no
// accounting for at all, however plainly up the peer's session is.
//
// eor (eorCTE) is joined on the route's own collector, router, peer, rib and
// session, and on `eor.fam = ?` -- a BOUND value, evpnFamily, not a literal.
// This is the one join predicate that differs from routesSQL's and
// vpnRoutesSQL's `eor.fam = r.family`, and it differs because route_evpn has
// no family column to compare against. Dropping the predicate would not
// merely widen the join: a peer's finished vpn4 or ipv4u dump would start
// reporting this peer's still-arriving EVPN routes as "complete", which is
// exactly the per-family question eor_events was split out of route_unicast
// to make answerable. TestEVPNRoutes' "a marker for another family does not
// complete this one" subtest is what fails when it goes.
//
// The bound family is also why this is the one statement in the package
// whose placeholders do not all come from its filter: `?` here binds ahead
// of everything RouteFilter-style rendering splices in at %[2]s, and the
// driver binds strictly left to right, so EVPNRoutes prepends the value
// rather than passing the filter's own values alone.
//
// HAVING live_is_withdraw = 0 excludes a route whose newest observation was
// a withdrawal, exactly as it does for the other two families -- 46 of the
// archive's 202 rows are withdrawals, so this is not a corner case here. It
// is a HAVING over argMax rather than a WHERE on r.is_withdraw for the
// reason peersSQL's rs subquery spells out: an advertisement that was later
// withdrawn still has its is_withdraw = 0 row sitting in the table, so
// filtering rows by their own column value keeps the stale advertisement and
// answers with a route that is not there.
//
// No FINAL, and argMax over (r.seq, r.stream_seq) -- never ts_router (a
// router with a dead clock reports 1970), never ts_collector
// (batch-granular). See this package's own doc comment.
//
// med and local_pref are aggregated as argMax(tuple(...), ...).1 rather than
// argMax(...) directly, the same way routesSQL and vpnRoutesSQL aggregate
// them and for the reason routesSQL's own doc comment measures out:
// ClickHouse's aggregates skip a NULL argument, so a plain argMax over a
// Nullable column reports the newest NON-NULL observation and an attribute
// cleared on re-advertisement keeps its old value forever. communities is
// selected as the Array(UInt32) route_evpn stores and rendered into the
// contract's "65000:100" text in Go by communityStrings; large_communities
// is already text and is passed through, and so, now, is ext_communities
// beside it -- route_targets needed no addition here, since this statement
// already selects it as live_route_targets. Every
// archive row leaves all five empty today, which is a fact about EVPN
// deployments rather than about the columns -- an EVPN route can carry any
// of them, and RouteCommon.required asks for all five whatever this table
// currently holds.
//
// ORDER BY carries the entire route key, not just the leading columns.
// Without esi and ip in it, two rows differing only there have no tie-break
// at all, so which one lands in got[0] versus got[1] is unspecified and a
// test indexing either would flake on nothing more than execution order --
// the same lesson vpnRoutesSQL's ORDER BY records for rib and path_id.
const evpnRoutesSQL = "WITH " + peerStateCTE + peerUpCTE + eorCTE + `
SELECT
    argMax(r.router_sysname, (r.seq, r.stream_seq)) AS sysname,
    r.router_ip, r.peer_ip, r.collector_id, r.rib, r.path_id,
    r.route_type, r.rd, r.prefix, r.mac, r.ip, r.ethernet_tag, r.esi,
    argMax(r.gateway_ip,    (r.seq, r.stream_seq)) AS live_gateway_ip,
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
FROM %[1]s.route_evpn_current r
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
   AND eor.fam          = ?
WHERE ` + servedGate + `%[2]s
GROUP BY r.collector_id, r.router_ip, r.peer_ip, r.rib,
         r.route_type, r.rd, r.prefix, r.mac, r.ip, r.ethernet_tag, r.esi, r.path_id
HAVING live_is_withdraw = 0%[3]s`

// evpnRoutesOrder is the ordering EVPNRoutes reports its answer in, appended
// to evpnRoutesSQL by evpnRoutesStatement, exactly as routesOrder is to
// routesSQL and for the same reason: RIBPageEVPN appends the tail ribOrder
// builds to this same body instead. See evpnRoutesSQL's own doc comment for why this one
// carries the entire route key rather than its leading columns.
const evpnRoutesOrder = `
ORDER BY sysname, r.peer_ip, r.route_type, r.rd, r.prefix, r.mac, r.ip,
         r.ethernet_tag, r.esi, r.path_id, r.collector_id`

// evpnRoutesStatement renders evpnRoutesSQL for one EVPNRouteFilter and
// returns it with the values its placeholders bind, in the order the driver
// binds them.
//
// It is split out of EVPNRoutes for the reason peersStatement is split out
// of Peers: so a test can hold it to this builder's one invariant -- one
// `?` emitted, one value appended -- without a live database. That guard
// matters more here than on the two statements whose placeholders all come
// from one rendering, because this one interleaves two sources. The eor
// join's `?` is emitted by the statement text and its value supplied here;
// the filter's are emitted and supplied together. clickhouse-go raises on
// too few arguments but silently DISCARDS a surplus one (see
// peersStatement's own doc comment for the version and the verification),
// so a family value appended AFTER the filter's values would bind the
// filter's first value to the eor join and go unreported.
//
// evpnFamily leads because the eor join sits above the outer WHERE in the
// finished statement, not because it was built first.
//
// ef.Limit, when set, is appended as a bare `LIMIT %d` after evpnRoutesOrder
// -- the same way Routes and VPNRoutes cap their own statements, and for the
// same reason (see RouteFilter.Limit): ClickHouse refuses a bound `?` in
// LIMIT, and the value never comes from a caller's raw text in any case. It
// adds no placeholder of its own, so it changes nothing
// TestEVPNRoutesStatementBindsEveryPlaceholder checks.
func evpnRoutesStatement(db string, ef EVPNRouteFilter) (string, []any) {
	w := ef.predicates()
	stmt := fmt.Sprintf(evpnRoutesSQL+evpnRoutesOrder, db, w.where(), w.having())
	if ef.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", ef.Limit)
	}
	return stmt, append([]any{evpnFamily}, w.values()...)
}

// EVPNRoutes reports the EVPN NLRIs that are live right now, fleet-wide:
// one row per (collector, router, peer, rib) and NLRI tuple whose
// current-session, up peer is still advertising it. It is the third route
// family, and EVPNRoutes is to route_evpn what Routes is to route_unicast
// and VPNRoutes is to route_vpn -- see evpnRoutesSQL's own doc comment for
// what "current" and "still advertising" mean, and EVPNRoute's for why the
// answer is keyed on a whole NLRI tuple rather than on a prefix.
//
// Every one of f's six fields is optional, prefix included, so an empty
// filter would render to no predicate at all and dump route_evpn whole.
// EVPNRouteFilter.check is what stops that, which is why this validates
// before it renders rather than after. Two ways of asking are refused
// outright, both wrapping ErrBadFilter so the HTTP layer answers 400 rather
// than 500:
//
//   - A filter naming none of Prefix, RD or Router. api/openapi.yaml
//     requires at least one of the three on /v1/routes/evpn, and the
//     alternative is not a smaller answer but the whole table -- an
//     unpaginated dump, which is what /v1/rib/evpn's cursor walk exists to
//     serve instead.
//   - A RouteType above 11, IANA's own bound. route_type is a bare UInt8
//     with no enum behind it, so an out-of-range value matches nothing
//     rather than raising and hands back an empty result indistinguishable
//     from a correct one. See evpnMaxRouteType, including why the bound is
//     the registry's rather than the three types the archive holds.
//
// f.Limit caps the rows RETURNED, not the rows matched -- see
// RouteFilter.Limit for the full argument, applied here unchanged. See
// CountEVPNRoutes for how a caller learns whether a capped answer left
// anything out.
func (q *Q) EVPNRoutes(ctx context.Context, f EVPNRouteFilter) ([]EVPNRoute, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	sql, args := evpnRoutesStatement(q.db, f)
	rows, err := q.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query evpn routes: %w", err)
	}
	defer rows.Close()
	return scanEVPNRoutes(rows)
}

// CountEVPNRoutes reports how many EVPN routes match f, ignoring f.Limit --
// EVPNRoutes' counterpart to CountRoutes and CountVPNRoutes, for the
// identical reason (see CountRoutes' own doc comment): a capped page needs a
// true total from the same statement EVPNRoutes runs, not a hand-derived
// copy of its predicate that could drift from it.
//
// It builds from evpnRoutesSQL directly rather than through
// evpnRoutesStatement, because evpnRoutesStatement bakes in evpnRoutesOrder
// and this wants the statement WITHOUT it -- ordering rows that are only
// ever counted is work with no effect on the answer, the same omission
// CountRoutes and CountVPNRoutes make against their own ORDER BY. It still
// has to prepend evpnFamily to the filter's own values, exactly as
// evpnRoutesStatement does and for the same reason: the eor join's `?`
// binds ahead of everything f.predicates() renders, and the driver binds
// strictly left to right.
func (q *Q) CountEVPNRoutes(ctx context.Context, f EVPNRouteFilter) (uint64, error) {
	if err := f.check(); err != nil {
		return 0, err
	}
	w := f.predicates()
	inner := fmt.Sprintf(evpnRoutesSQL, q.db, w.where(), w.having())
	args := append([]any{evpnFamily}, w.values()...)
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count evpn routes: %w", err)
	}
	return n, nil
}

// scanEVPNRoutes drains rows into EVPNRoutes' own answer shape, split out of
// EVPNRoutes for the reason scanRoutes is split out of Routes: RIBPageEVPN
// reads the identical SELECT list -- both statements are evpnRoutesSQL with a
// different ORDER BY appended -- and a second copy of this twenty-four-column
// positional Scan would be a second place for it to drift from the statement
// that feeds it. Eight of those columns are the NLRI key and five of them are
// consecutive strings, and live_med and live_local_pref are two more adjacent
// columns of one type -- which is exactly the shape a transposed pair hides
// in. See TestEVPNRoutesReportsPathAttributes for the fixture that gives
// every one of them a distinct value so a transposition cannot pass.
func scanEVPNRoutes(rows driver.Rows) ([]EVPNRoute, error) {
	var out []EVPNRoute
	for rows.Next() {
		var r EVPNRoute
		var nextHop string
		// live_communities is scanned as the Array(UInt32) route_evpn stores
		// and rendered by communityStrings below, for the reason that
		// function's own doc comment gives -- the same split scanRoutes makes.
		var communities []uint32
		// live_is_withdraw is scanned and discarded: it exists so HAVING
		// has an alias to filter on that cannot collide with route_evpn's
		// own is_withdraw column (see evpnRoutesSQL's doc comment), not
		// because a caller of EVPNRoutes needs it -- HAVING already
		// guarantees every row reaching here has live_is_withdraw = 0.
		var liveIsWithdraw uint8
		if err := rows.Scan(
			&r.RouterSysName, &r.RouterIP, &r.PeerIP, &r.Collector, &r.RIB, &r.PathID,
			&r.RouteType, &r.RD, &r.Prefix, &r.MAC, &r.IP, &r.EthernetTag, &r.ESI,
			&r.GatewayIP, &nextHop, &r.ASPath, &r.Labels, &r.RouteTargets,
			&r.MED, &r.LocalPref, &communities, &r.LargeCommunities,
			&r.ExtCommunities,
			&liveIsWithdraw, &r.DumpState,
		); err != nil {
			return nil, fmt.Errorf("scan evpn route: %w", err)
		}
		r.Communities = communityStrings(communities)
		// See routes.go's identical next_hop parse for why a failure here
		// degrades the field instead of failing the whole call: one EVPN
		// route with an unparseable next hop must not cost the caller every
		// other NLRI this query legitimately has. gateway_ip is NOT parsed
		// -- it stays the raw string the contract types it as; see
		// EVPNRoute's own doc comment for why turning "" into "invalid IP"
		// would be a worse answer than leaving it alone.
		r.NextHop, _ = netip.ParseAddr(nextHop)
		// originASN, not a second copy of it: see Route's own doc comment
		// and originASN's, in routes.go, for why this has to be derived in
		// Go rather than as as_path[-1] in SQL. Every one of the archive's
		// 202 route_evpn rows carries an empty AS path, so the shortcut
		// would report the reserved AS 0 as a real origin for all of them --
		// the identical defect route_vpn proved and the L3VPN RIB browser
		// fixed on 2026-08-23.
		r.OriginASN = originASN(r.ASPath)
		// Every address this loop hands back goes through unmapAll, next
		// hop included -- see its own doc comment, and Routes' own scan loop
		// for why netip.ParseAddr is not the exemption it looks like.
		unmapAll(&r.RouterIP, &r.PeerIP, &r.NextHop)
		out = append(out, r)
	}
	return out, rows.Err()
}
