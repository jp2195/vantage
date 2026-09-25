package query

import (
	"context"
	"fmt"
	"time"
)

// peersSQL selects peer_state (see peerStateCTE) at
// (collector, router, peer, rib) granularity and left-joins dump_map -- the
// per-family dump progress built from eor_events and the three route tables
// -- to decide DumpStates. It also joins peer_up (see peerUpCTE) to decide
// whether route_counts' own (collector, router, peer, rib)-scoped route
// count reaches the output at all -- see route_counts' own doc comment
// below for why that has to be peer_up and not peer_state.state.
//
// dump_families is the family universe: every family this (router, peer,
// rib, session) has any evidence for at all, from a marker in eor_events or
// from a route row in route_unicast, route_vpn or route_evpn. A family with
// no evidence in any of the four is ABSENT from the resulting map rather
// than present as "unknown" -- there is no dump to be part-way through, so
// there is nothing to report. api/openapi.yaml's dump_states schema is the
// contract this shape answers to ("A family appears once its session has
// carried any route or end-of-RIB marker for it").
//
// KNOWN GAP -- BGP-LS. dump_families' first branch filters eor_current down
// to family != 'ls' before the family-universe union runs, so 'ls' still
// never appears in a Peer.DumpStates map -- the same answer this statement
// has always given, and it disagrees with api/openapi.yaml's dump_states
// schema: "A family appears once its session has carried any route or
// end-of-RIB marker for it."
//
// The filter is deliberate, not a leftover. eor_current merges eor_events'
// six families with link-state End-of-RIB markers filed as family 'ls' (see
// eorCTE's and lsEorCTE's own doc comments), so admitting that family here
// costs nothing to wire up -- but dump_families has no OTHER branch that can
// see link-state activity, the way route_unicast, route_vpn and route_evpn
// let the other three families read "dumping" before their own marker
// arrives. Removing the filter would let a peer report 'ls: complete' the
// moment its End-of-RIB lands, yet never once report 'ls: dumping' before
// that -- a family that can only ever be "complete" or absent, not the
// three-state contract dump_states documents for every other family. That
// is a worse answer than the current gap, not a better one.
//
// TRIGGER: dump_families gains a branch that can report link-state rows in
// progress, the way the three route branches already do for their own
// families -- only then does removing this filter make 'ls' answer the same
// question the other four already do, rather than a narrower one wearing
// their shape.
//
// route_evpn contributes the literal 'evpn' because it has no family column
// at all: an EVPN table holds exactly one family, so the column would be a
// constant on every row (see deploy/clickhouse/schema.sql). eor_events'
// own EVPN markers already spell it 'evpn' (see subjects' AFI/SAFI table,
// which sink shares), so the literal and the marker meet on the same key.
// Every branch CASTs family to String so the UNION ALL has one key type and
// the Map this feeds is Map(String, String) rather than a
// LowCardinality-keyed variant whose scan behavior would depend on the
// driver.
//
// is_marker is 1 on the eor_events branch and 0 on the three route
// branches, and dump_families collapses it with max(), not count(): the
// question is "did a marker arrive for this family", never "how many
// arrived". eor_events is a plain MergeTree with NO deduplication (see its
// own schema.sql comment on why that is the right engine for it), so a
// JetStream redelivery leaves a second, permanent marker row for the same
// (router, peer, rib, session, family). max() over a 0/1 flag is a boolean
// OR: it lands on the same answer whether the table holds one marker or
// five. A count() here -- in the very table split out of route_unicast to
// stop queries counting collection artifacts as facts about the network --
// would be this project's most recurring defect reintroduced at the one
// place built to end it.
//
// dump_families' branches each carry the same (router, rib) predicate
// peer_state is filtered by below, rather than aggregating whole tables and
// relying on the outer WHERE to narrow them after the fact. At the
// archive's present size that distinction is free; the route tables carry a
// 90-day TTL and no upper bound otherwise, and this CTE has no reason to
// scan rows for routers or ribs Peers was never asked about.
//
// That predicate is a filters rendering spliced in at %[2]s rather than the
// `(? = toIPv6('::') OR router_ip = ?)` sentinel it used to be. Only ONE of
// this statement's seven insertion points decides what Peers returns -- the
// final WHERE on peer_state. The four dump_families branches, rs and
// route_counts' own outer WHERE are pure narrowing: every one of them feeds
// a join keyed on (collector_id, router_ip, peer_ip, rib, ...), so rows for
// another router or another rib cannot reach the output through them
// whether they are filtered or not. Deleting any of those six changes how
// much this statement reads and nothing about its answer, so no test that
// inspects a RESULT can go red on their removal -- verified by deleting each
// of them and running the suite. TestPeersStatementBindsEveryPlaceholder is
// what holds them in place instead, by asserting the count of insertion
// points rather than an answer that does not depend on them.
//
// What the sentinel's removal buys on the UNFILTERED path is one per-row
// comparison and fourteen hand-counted placeholders, and it is worth being
// exact about that because the tempting larger claim is wrong. Peers(ctx,
// PeerFilter{}) renders every one of the seven to the empty string, so
// route_counts still aggregates the whole of route_unicast and the LEFT
// JOIN route_counts hash side is still built over all of it -- exactly as
// it was under the sentinel. That cost is structural to an unfiltered
// /v1/peers; nothing here changes it, and nothing here should be
// read as claiming otherwise.
//
// dump_map is the second aggregation level: one row per (collector, router,
// peer, rib, session) whose states column is already the Map(String, String)
// Peer's own field scans into. It is built here rather than zipped from two
// parallel arrays in Go because the map IS the answer -- an intermediate
// pair of arrays would be a shape only the scan loop understood, and a
// caller reading Peers' own SELECT list could not see what it produced.
//
// DumpStates is not simply "the families with markers": a peer's current
// session can carry a complete marker from earlier in the session and
// then go down, and a peer that is down is not "still dumping" no matter
// what its route rows say. dumpStatesExpr (see its own doc
// comment for why it is a shared const rather than inlined here) checks
// peer_state.state FIRST, before any family logic, for exactly that reason,
// and reports an empty map for a down peer. See
// TestPeersReportsDumpStateForADownPeerWithACompleteDump, which exists
// because a query that consults family state before peer_state reports
// dump progress for a peer that is not connected.
//
// Both LEFT JOIN reads inside dumpStatesExpr are coalesced --
// peer_state.state's and dump_map.states' -- though only the first of the
// two can actually go null: ClickHouse cannot put a Map inside a Nullable,
// so dump_map.states is Map(String, String) whatever join_use_nulls is set
// to. See dumpStatesExpr's own doc comment for what was measured and why
// the guard is there anyway. An unmatched row yields 'unspecified' and the
// empty map respectively, which are the right answers for a peer whose
// session carried no route and no marker for any family.
//
// Every CTE here carries collector_id through its own GROUP BY and joins
// to peer_state on it, for the reason peerStateCTE's doc comment gives:
// session identity is (collector, router), so a dump_families family, a
// dump_map entry and a route_counts tally each belong to the collector that
// observed them. Threading it through the aggregations rather than only
// through the final join is what keeps that true -- a dump_map keyed on
// (router, peer, rib, session) alone would merge two collectors' family
// evidence into one map before any join could tell them apart.
//
// route_counts is Peer.Routes: how many (prefix, path_id) keys this
// (router, peer, rib)'s current session is currently advertising. It stays
// unicast-only and unchanged by the per-family split -- Peer.Routes is
// documented as a unicast route-key count -- and it is the one place in
// this package where omitting FINAL is not, on its own, enough (see this
// package's own doc comment): a redelivered envelope leaves two
// byte-identical route_unicast rows until the next merge collapses them,
// and ReplacingMergeTree's guarantee that they collapse is a promise about
// eventually, not about the moment a query runs. count() over those two
// rows is 2; the route is 1. uniqExact((r.prefix, r.path_id)) is what makes
// the count correct regardless of merge timing, because it dedupes on the
// row's own identity rather than trusting the storage engine to have
// already done so -- see
// TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent, which defeats
// merges outright (SYSTEM STOP MERGES) to keep this hazard from being
// silently fixed out from under the test the way it was fixed out from
// under an earlier measurement.
//
// route_counts carries no end_of_rib predicate any more, and needs none:
// end-of-RIB markers left route_unicast for eor_events, so route_unicast
// now contains only routes. That predicate used to be the difference
// between Peer.Routes counting a peer's routes and counting its routes plus
// one phantom for every finished dump; deleting the column is what makes
// forgetting it impossible rather than merely tested for.
//
// rs resolves is_withdraw the way routesSQL's own live_is_withdraw does --
// argMax(is_withdraw, (seq, stream_seq)), the route key's newest
// observation, never a raw is_withdraw = 0 filter (a route that was
// advertised and later withdrawn still has its advertise row sitting in
// route_unicast; filtering rows by their own is_withdraw value keeps that
// row instead of resolving what the key's current state actually is). But
// rs cannot be folded into routesSQL's own shape here: routesSQL's HAVING
// live_is_withdraw = 0 runs on a query already GROUPed to one row per route
// key, which is exactly the collapse this CTE cannot afford to do before
// uniqExact sees the raw rows -- grouping by route key first would dedupe
// this pair of redelivered rows down to one before the redelivery hazard
// above ever became visible, and the count() vs uniqExact() mutation a
// test exists to catch would stop failing for a reason that has nothing to
// do with the query being correct. So rs is joined in as a second,
// finer-grained pass instead: it resolves each route key's live withdraw
// state without collapsing route_counts' own FROM, and the duplicate rows
// this CTE is built to survive stay duplicated all the way to its own
// GROUP BY.
//
// rs is an INNER JOIN, not a LEFT JOIN like dump_map: rs is grouped from
// the same table with the same (router, rib) predicate as r and no further filter,
// so every row r's own WHERE can ever select already has a matching group
// in rs by construction -- there is no unmatched case for coalesce to
// guard, unlike dump_map, which genuinely has no row at all for a peer with
// zero route and marker activity.
//
// route_counts is joined into the final SELECT below on the current
// session and on servedGate (peer_state.sid, in the LEFT JOIN's own ON
// clause rather than the outer WHERE, and peer_up.state, not
// peer_state.state): a down peer reports Routes = 0 via coalesce's
// default, the same "a down peer contributes nothing" rule Routes(ctx,
// prefix) already enforces, even though the row it advertised before going
// down is still sitting in route_unicast for this same current session.
// peer_up, not peer_state, is what decides "down" here for the identical
// reason routesSQL and vpnRoutesSQL both gate route survival on peer_up
// (see peerStateCTE's and peerUpCTE's own doc comments): route_counts is
// itself already scoped to (router, peer, rib) via its own GROUP BY, so a
// route counted here is still attributed to the one existing peer_state
// row for that exact rib -- this join changes only whether that count
// reaches the output at all, from a rib-scoped up/down check to a
// peer-scoped one. peer_up is INNER-joined into FROM peer_state (not LEFT)
// because it is guaranteed to match: peer_up and peer_state are both
// derived from the same peer_current rows via the same cur, just grouped at
// different granularities, so any (router, peer, rib) that peer_state
// produces a row for already implies peer_current has at least one row for
// that (router, peer) -- which is exactly what peer_up requires to have a
// row of its own.
//
// dump_families' first branch reads eor_current, not eor_events: eor_current
// has no TTL, so a session's dump-completion markers stay resolvable here
// after eor_events' own retention window would have expired them. It reads
// from a nested `family != 'ls'` scan of eor_current, not eor_current
// directly, so this branch's own KNOWN GAP paragraph (above) stays true
// regardless of what %[2]s renders to.
const peersSQL = "WITH " + peerStateCTE + peerUpCTE + `,
dump_families AS (
    SELECT collector_id, router_ip, peer_ip, rib, session_id, fam,
           max(is_marker) AS marked
    FROM (
        SELECT collector_id, router_ip, peer_ip, rib, session_id,
               CAST(family AS String) AS fam, 1 AS is_marker
        FROM (SELECT * FROM %[1]s.eor_current WHERE family != 'ls') eor_no_ls
        %[2]s
        UNION ALL
        SELECT collector_id, router_ip, peer_ip, rib, session_id,
               CAST(family AS String) AS fam, 0 AS is_marker
        FROM %[1]s.route_unicast_current
        %[2]s
        UNION ALL
        SELECT collector_id, router_ip, peer_ip, rib, session_id,
               CAST(family AS String) AS fam, 0 AS is_marker
        FROM %[1]s.route_vpn_current
        %[2]s
        UNION ALL
        SELECT collector_id, router_ip, peer_ip, rib, session_id,
               'evpn' AS fam, 0 AS is_marker
        FROM %[1]s.route_evpn_current
        %[2]s
    )
    GROUP BY collector_id, router_ip, peer_ip, rib, session_id, fam
),
dump_map AS (
    SELECT collector_id, router_ip, peer_ip, rib, session_id,
           mapFromArrays(
               groupArray(fam),
               groupArray(if(marked = 1, 'complete', 'dumping'))
           ) AS states
    FROM dump_families
    GROUP BY collector_id, router_ip, peer_ip, rib, session_id
),
route_counts AS (
    SELECT r.collector_id, r.router_ip, r.peer_ip, r.rib, r.session_id,
           uniqExact((r.prefix, r.path_id)) AS n
    FROM %[1]s.route_unicast_current r
    INNER JOIN (
        SELECT collector_id, router_ip, peer_ip, rib, session_id, prefix, path_id,
               argMax(is_withdraw, (seq, stream_seq)) AS live_is_withdraw
        FROM %[1]s.route_unicast_current
        %[2]s
        GROUP BY collector_id, router_ip, peer_ip, rib, session_id, prefix, path_id
    ) rs
        ON rs.collector_id = r.collector_id AND rs.router_ip = r.router_ip
       AND rs.peer_ip = r.peer_ip
       AND rs.rib = r.rib AND rs.session_id = r.session_id
       AND rs.prefix = r.prefix AND rs.path_id = r.path_id
    WHERE rs.live_is_withdraw = 0%[3]s
    GROUP BY r.collector_id, r.router_ip, r.peer_ip, r.rib, r.session_id
)
SELECT
    peer_state.router_ip    AS router_ip,
    peer_state.peer_ip      AS peer_ip,
    peer_state.collector_id AS collector_id,
    peer_state.rib          AS rib,
    peer_state.sid          AS session_id,
    peer_state.state        AS state,
    peer_state.asn          AS peer_asn,
    ` + dumpStatesExpr + ` AS dump_states,
    coalesce(route_counts.n, 0) AS routes,
    peer_state.hold_time        AS hold_time,
    peer_state.hold_time_seen   AS hold_time_seen,
    peer_state.mp_families      AS mp_families,
    peer_state.addpath_families AS addpath_families,
    peer_state.sys_descr        AS sys_descr,
    peer_state.up_since         AS up_since,
    peer_state.up_seen          AS up_seen
FROM peer_state
INNER JOIN peer_up
    ON peer_up.collector_id = peer_state.collector_id
   AND peer_up.router_ip = peer_state.router_ip
   AND peer_up.peer_ip   = peer_state.peer_ip
   AND peer_up.sid       = peer_state.sid
LEFT JOIN dump_map
    ON dump_map.collector_id = peer_state.collector_id
   AND dump_map.router_ip = peer_state.router_ip
   AND dump_map.peer_ip = peer_state.peer_ip
   AND dump_map.rib = peer_state.rib
   AND dump_map.session_id = peer_state.sid
LEFT JOIN route_counts
    ON route_counts.collector_id = peer_state.collector_id
   AND route_counts.router_ip = peer_state.router_ip
   AND route_counts.peer_ip = peer_state.peer_ip
   AND route_counts.rib = peer_state.rib
   AND route_counts.session_id = peer_state.sid
   AND ` + servedGate + `
%[4]s
ORDER BY peer_state.router_ip, peer_state.peer_ip, peer_state.rib,
         peer_state.collector_id`

// peersStatement renders peersSQL for one PeerFilter and returns it with the
// values its placeholders bind, in the order the driver binds them.
//
// It is split out of Peers so a test can hold it to the invariant this whole
// builder exists for: one `?`, one value. That test is not the best guard on
// this statement -- it is the ONLY one, because both layers that would
// ordinarily catch a dropped insertion point are disabled by the shape of
// this particular statement:
//
//   - clickhouse-go raises when a statement has MORE placeholders than
//     arguments, but a SURPLUS argument is silently discarded. Its
//     bindPositional (v2.48.0) checks only for too few.
//   - fmt normally reports a surplus argument as %!(EXTRA ...), but it
//     suppresses that check for any format string that uses indexed verbs.
//     peersSQL uses %[1]s through %[4]s throughout, so it is exempt.
//     Verified directly: fmt.Sprintf("x %[1]s y %[3]s", "db", "unused",
//     "outer") returns "x db y outer" with no EXTRA, while the same surplus
//     against a non-indexed "x %s" returns "x db%!(EXTRA string=surplus)".
//
// So deleting one %[2]s from peersSQL while leaving its entry in the list
// below produces a working query -- confirmed by running exactly that
// mutation, which left the whole suite green while Peers took a router
// alone. It was harmless then only because all seven values were the same
// address, so a shifted binding still landed on the right answer. That is no
// longer true: ?rib= is the second optional filter this comment warned
// about, so each insertion point now contributes an address AND a rib, and a
// dropped one shifts every later binding by two -- an IP arriving where an
// Enum8 rib is expected. TestPeersStatementBindsEveryPlaceholder is what
// fails first, and by design, rather than leaving that to whether ClickHouse
// happens to raise on the transposed pair.
//
// Two optional predicates, seven insertion points, three renderings of them:
// the columns are qualified differently at each level of the statement, so
// each rendering is built against the names that level actually uses. An
// unset field renders to nothing at all three, which is what makes "every
// router" (or every rib) an absent predicate rather than a comparison
// against a sentinel -- see eqAddr's own doc comment for what that costs
// when it is not.
//
// Router before rib in every rendering, because that is the order the
// builder emits them in and therefore the order the driver binds them; the
// two are the same three-line function precisely so they cannot disagree.
func peersStatement(db string, pf PeerFilter) (string, []any) {
	render := func(routerCol, ribCol string) *filters {
		var f filters
		f.eqAddr(routerCol, pf.Router)
		f.eqNonEmpty(ribCol, pf.RIB)
		return &f
	}
	// dump_families' four UNION ALL branches and route_counts' rs subquery
	// each select from one table with nothing else to say about it, so
	// their columns are unqualified and the predicate has to bring its own
	// WHERE keyword.
	tbl := render("router_ip", "rib")
	// route_counts' outer WHERE already has rs.live_is_withdraw = 0 to hang
	// an AND on, and its FROM is aliased r.
	cnt := render("r.router_ip", "r.rib")
	// The final WHERE, the only one of the seven that decides what Peers
	// returns rather than how much of the archive it reads.
	outer := render("peer_state.router_ip", "peer_state.rib")

	sql := fmt.Sprintf(peersSQL, db, tbl.clause(), cnt.where(), outer.clause())

	// The driver binds placeholders strictly left to right, so args follows
	// the order the predicates appear in the FINISHED statement, not the
	// order they were built in -- %[2]s alone lands at five separate
	// positions. Naming each insertion point rather than multiplying a
	// count is what makes an eighth one visible as an edit that has to be
	// made in two places; the test named above is what makes forgetting the
	// second of them fail.
	var args []any
	for _, f := range []*filters{
		tbl,   // dump_families: eor_current
		tbl,   // dump_families: route_unicast
		tbl,   // dump_families: route_vpn
		tbl,   // dump_families: route_evpn
		tbl,   // route_counts: the rs subquery
		cnt,   // route_counts: its own outer WHERE
		outer, // the final WHERE
	} {
		args = append(args, f.values()...)
	}
	return sql, args
}

// Peers reports one row per (collector, router, peer, rib), as of each
// (collector, router)'s current BMP session (see peerStateCTE's doc comment
// for what "current" means, and Peer.Collector for why the collector is
// part of the key): the state that session ended in, not whether the peer
// was ever down during it, and how far along each address family's initial
// table dump is (see Peer.DumpStates).
//
// f's two fields are the ?router= and ?rib= api/openapi.yaml documents on
// /v1/peers, and both are optional: a zero Router reports every router, an
// empty RIB every rib. Narrowing by rib does not change the grain of the
// answer -- one row per (collector, router, peer, rib) either way -- it
// drops the rows for the other ribs, which is what a caller asking "what
// does loc_rib look like" wants and what a caller asking "how many peers
// are there" must not do.
func (q *Q) Peers(ctx context.Context, f PeerFilter) ([]Peer, error) {
	sql, args := peersStatement(q.db, f)
	rows, err := q.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query peers: %w", err)
	}
	defer rows.Close()

	var out []Peer
	for rows.Next() {
		var p Peer
		// routes is UInt64 (uniqExact's own return type, the same as
		// count()) -- clickhouse-go's UInt64 column scans only into
		// *uint64, with no narrowing case for *int (see routers.go's own
		// up/down scan for the same rule applied to countIf). Peer.Routes
		// stays a plain int, the natural Go type for a route count, by
		// scanning through this local and narrowing here instead of at
		// every caller of Peers.
		var routes uint64
		// holdSeen scans the UInt8 column and narrows to the bool the type
		// exposes: a caller asking "was an OPEN observed" should not have to
		// know the column is a number, and `!= 0` written at every call site
		// is how that column's meaning would eventually get read as a count.
		var holdSeen uint8
		// upSince and upSeen are scanned together and only the pair is
		// meaningful: ClickHouse returns the zero instant for a session with
		// no up at all, and handing that to a caller as a time would report
		// every never-up session as having come up in 1970. The Peer field
		// stays unset in that case, so `IsZero` answers "no up was observed"
		// rather than "came up at the epoch".
		var upSince time.Time
		var upSeen uint8
		if err := rows.Scan(
			&p.RouterIP, &p.PeerIP, &p.Collector, &p.RIB, &p.SessionID,
			&p.State, &p.ASN, &p.DumpStates, &routes,
			&p.HoldTime, &holdSeen, &p.MPFamilies, &p.AddPathFamilies, &p.SysDescr,
			&upSince, &upSeen,
		); err != nil {
			return nil, fmt.Errorf("scan peer: %w", err)
		}
		p.Routes = int(routes)
		p.HoldTimeSeen = holdSeen != 0
		if upSeen != 0 {
			p.UpSince = upSince
		}
		// Non-nil, always. An empty ClickHouse Array scans into a nil slice,
		// and nil and []string{} are the same thing to %+v and different
		// things to reflect.DeepEqual -- which is how this surfaced: the CLI's
		// direct-vs-API parity test reported "peers disagree" above two
		// printed structs that were character-for-character identical.
		//
		// []string{} is also the honest value: the answer is "this session
		// negotiated no families of that kind", which is an empty list rather
		// than an absent one, and it is what the JSON path already produces.
		if p.MPFamilies == nil {
			p.MPFamilies = []string{}
		}
		if p.AddPathFamilies == nil {
			p.AddPathFamilies = []string{}
		}
		// router_ip and peer_ip are IPv6-typed and clickhouse-go's scan
		// never unmaps an IPv4-mapped address -- see unmapAll's own doc
		// comment.
		unmapAll(&p.RouterIP, &p.PeerIP)
		out = append(out, p)
	}
	return out, rows.Err()
}
