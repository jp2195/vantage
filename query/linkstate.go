package query

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// lsEorCTE is eorCTE's link-state counterpart, and it exists because
// eor_events does not carry link-state at all.
//
// A link-state End-of-RIB is an MP_UNREACH with AFI 16388 / SAFI 71 and no
// NLRI whatsoever (RFC 4724 section 2), so there is no typed node, link or
// prefix row for it to be. It is recorded as ls_events.end_of_rib = 1
// instead -- see that column's own comment in deploy/clickhouse/schema.sql.
// The archive bears this out: eor_events holds six families (ipv4u, ipv6u,
// vpn4, vpn6, lu4, evpn) and no ls.
//
// It carries no family dimension, and that is deliberate rather than an
// omission. Link-state is one family, so a fam column here would be a
// constant, and joining on a constant invents a distinction the data does
// not have. The three route statements bind eor.fam precisely because their
// tables hold several families and an EVPN marker must not tell an ipv4u
// route its dump is finished; there is no such confusion to prevent here.
//
// It is aliased `eor` so that dumpStateExpr -- which reads
// coalesce(eor.marked, 0) -- is reusable verbatim rather than forked.
//
// marked is a constant 1 and the CTE groups rather than counts, for eorCTE's
// stated reason, and here that rule is ACTIVELY load-bearing rather than
// defensive: one (router, peer, session) in the archive carries 13 End-of-RIB
// markers. Whether those are genuine re-signals or JetStream redeliveries is
// not something an answer should be able to reveal -- a count() here would
// report a collection artifact as a fact about the network, which is this
// project's most recurring defect.
//
// It reads eor_current WHERE family = 'ls', not ls_events.end_of_rib = 1:
// eor_current carries ls_events' End-of-RIB markers alongside eor_events'
// own, and has no TTL, so link-state dump completion stays resolvable here
// after ls_events' own retention window would have expired the marker row.
// The GROUP BY stays family-less, since link-state is still the one family
// this CTE ever answers for.
const lsEorCTE = `,
eor AS (
    SELECT collector_id, router_ip, peer_ip, rib, session_id,
           toUInt8(1) AS marked
    FROM %[1]s.eor_current
    WHERE family = 'ls'
    GROUP BY collector_id, router_ip, peer_ip, rib, session_id
)`

// LSNode is one row of LSNodes: one link-state node as ONE observer
// currently reports it, keyed (collector, router, peer, rib, node_key).
//
// One row per observer, not per node. node_key hashes only node properties
// -- protocol, identifier, asn, bgpls_id, area, router_id -- with nothing
// about the observer in it, so the same node arrives from several (router,
// peer) pairs at once; the archive has one reported by six. Collapsing them
// would mean picking a winner among routers that disagree, silently, which
// is the merge this package defers.
//
// IsWithdraw is carried on every row regardless of the filter's State, so a
// caller reading a state=any answer never has to infer an object's state
// from the request it made.
type LSNode struct {
	RouterSysName    string
	RouterIP, PeerIP netip.Addr
	Collector, RIB   string

	Protocol   uint8
	Identifier uint64
	ASN        uint32
	BGPLSID    uint32
	Area       uint32
	RouterID   string
	NodeKey    uint64

	RouterIDv4   string
	Name         string
	SRGBBase     uint32
	SRGBSize     uint32
	SRLBBase     uint32
	SRLBSize     uint32
	SRAlgorithms []uint8

	IsWithdraw bool
	DumpState  string
}

// lsNodesSQL is routesSQL's shape over ls_nodes_current: the same
// current-session pin, the same peer_up liveness test, the same
// argMax-by-(seq, stream_seq) resolution of every mutable attribute, and the
// same dumpStateExpr. ls_nodes_current carries every column ls_nodes does,
// one row per (collector, router, session, peer, rib, node identity) rather
// than one row per observation, and no TTL, so a node's live state stays
// resolvable here after ls_nodes' own 90-day retention would have expired
// its rows -- ls_nodes_current is what routes' own move to
// route_unicast_current established for routes, carried over to this family.
//
// The ordering is (seq, stream_seq) and not (ts_collector, stream_seq).
// link-state-nodes.json uses the latter, and the two agree on every group in
// the archive today -- but 24% of adjacent observations tie on ts_collector
// at microsecond resolution (306 of 1,276), so that ordering leans on
// stream_seq as a tiebreaker for a quarter of them, and seq is the sender's
// own sequence rather than our receive clock. One convention across this
// package beats two.
//
// STATE THE PARTITION KEY WITH THE NUMBER: 306 of 1,276 is the tie rate
// scoped by session. 306 of 1,276 counts CONSECUTIVE observations within
// one (collector_id, router_ip, peer_ip, rib, SESSION_ID), ordered by
// (seq, stream_seq). Session belongs in the key: seq is the sender's own
// per-session sequence, so "adjacent" across a session boundary is not a
// pair of consecutive observations at all, it is two unrelated rows the
// sort happened to place together. ls_nodes holds 1,441 rows in 165 such
// groups, leaving 1,276 adjacent pairs. Dropping session_id from the key
// gives 10 groups and 1,431 pairs, the SAME 306 ties over a wider
// denominator. 306 of 1,441 is the raw row count and counts no pairs at
// all. All three describe the same 306 ties, and the tightest honest
// scope is the session-scoped one.
//
// Within an argMax group -- the same tuple plus node_key, which is what this
// statement actually groups by, and the only scope where a tiebreaker can
// change an answer at all -- the tie rate on this archive is 0%: 0 of 1,175
// pairs unscoped by session, 0 of 949 with session_id in the key. So the 24%
// is the rate at which the two orderings are asked to disagree somewhere in
// the stream, not the rate at which they could return different answers
// here, which today is zero at either scope. The case for (seq, stream_seq)
// rests on seq being the sender's own sequence and on one convention beating
// two; it does not rest on a wrong answer measured in the archive, because
// there is not one. insertLSNodeFixture manufactures what the archive lacks
// -- see the receive-order inversion on its "gone" pair, which makes the
// ts_collector ordering fail outright.
//
// No FINAL. ReplacingMergeTree's FINAL would collapse duplicates at read
// time at a cost the argMax below already avoids, and this package has never
// used it.
//
// %[3]s is havingClause(), not having(): state=any renders no
// post-aggregation predicate at all, so there is no fixed HAVING here for an
// AND to attach to.
const lsNodesSQL = "WITH " + peerStateCTE + peerUpCTE + lsEorCTE + `
SELECT
    argMax(n.router_sysname, (n.seq, n.stream_seq)) AS sysname,
    n.router_ip, n.peer_ip, n.collector_id, n.rib,
    n.protocol, n.identifier, n.asn, n.bgpls_id, n.area, n.router_id, n.node_key,
    argMax(n.router_id_v4,  (n.seq, n.stream_seq)) AS live_router_id_v4,
    argMax(n.name,          (n.seq, n.stream_seq)) AS live_name,
    argMax(n.srgb_base,     (n.seq, n.stream_seq)) AS live_srgb_base,
    argMax(n.srgb_size,     (n.seq, n.stream_seq)) AS live_srgb_size,
    argMax(n.srlb_base,     (n.seq, n.stream_seq)) AS live_srlb_base,
    argMax(n.srlb_size,     (n.seq, n.stream_seq)) AS live_srlb_size,
    argMax(n.sr_algorithms, (n.seq, n.stream_seq)) AS live_sr_algorithms,
    argMax(n.is_withdraw,   (n.seq, n.stream_seq)) AS live_is_withdraw,
    any(` + dumpStateExpr + `)                     AS dump_state
FROM %[1]s.ls_nodes_current n
INNER JOIN cur
    ON n.collector_id = cur.collector_id
   AND n.router_ip    = cur.router_ip
   AND n.session_id   = cur.sid
INNER JOIN peer_up
    ON n.collector_id = peer_up.collector_id
   AND n.router_ip    = peer_up.router_ip
   AND n.peer_ip      = peer_up.peer_ip
   AND cur.sid        = peer_up.sid
LEFT JOIN peer_state
    ON n.collector_id = peer_state.collector_id
   AND n.router_ip    = peer_state.router_ip
   AND n.peer_ip      = peer_state.peer_ip
   AND n.rib          = peer_state.rib
   AND cur.sid        = peer_state.sid
LEFT JOIN eor
    ON eor.collector_id = n.collector_id
   AND eor.router_ip    = n.router_ip
   AND eor.peer_ip      = n.peer_ip
   AND eor.rib          = n.rib
   AND eor.session_id   = cur.sid
WHERE ` + servedGate + `%[2]s
GROUP BY n.collector_id, n.router_ip, n.peer_ip, n.rib,
         n.protocol, n.identifier, n.asn, n.bgpls_id, n.area, n.router_id, n.node_key
%[3]s`

// lsNodesOrder is the ordering LSNodes reports its answer in, appended at the
// one call site that wants it. CountLSNodes does NOT wrap it -- ordering rows
// whose only consumer is count() is work thrown away.
//
// It has two halves and only the first reads for a human: sysname,
// router_ip, peer_ip, live_name and n.router_id, so a fleet-wide answer
// reads router by router and a reader sees each observer's nodes together
// under a name rather than a hash. Three of those five -- router_ip, peer_ip
// and router_id -- are GROUP BY columns as well, so "presentational" is
// about where they sit and what they do for a reader, not about their being
// some separate set from the key.
//
// Everything after that is the REST of the GROUP BY key -- the remaining
// eight of its eleven columns -- and it is there to make the order TOTAL
// rather than to make it read well. A partial order is not a cosmetic
// complaint once f.Limit is set, which every handler does from the
// operator's own page cap: rows tied on every ORDER BY column come back in
// whatever order the merge happened to produce, so a page boundary landing
// inside a tie silently drops one row and repeats the other, and nothing in
// the answer says so.
//
// The live archive has exactly that shape, and the figures below name the
// prefix each one was measured on, because the two differ by an order of
// magnitude:
//
//   - On the full five-column lead (sysname, router_ip, peer_ip, live_name,
//     n.router_id): 11 tied groups covering 22 of 230 rows at state=any.
//     Every one is the IS-IS Level-1 and Level-2 report of a single
//     router_id from a single observer -- same sysname, same router, same
//     peer, same name, same router_id, DIFFERENT node_key -- confirmed by
//     re-measurement (all 11 groups carry protocols {1, 2}, one router_id
//     and two node_keys).
//   - On the four columns WITHOUT n.router_id: 18 tied groups covering 224
//     of 230 rows. Two of those groups are 80 rows deep apiece -- one
//     bmpgen-iosxr peer each, 80 distinct router_ids sharing an empty name.
//
// The correction makes the argument for a total order STRONGER, not weaker:
// on the prefix a reader would guess at, 97% of the answer is tied rather
// than 10%. Running the statement twice with different block sizes returned
// the same 230 rows in a different order, and the rows that moved were
// exactly the tied ones.
//
// That is the convention the three route orderings already keep, and this one
// was the outlier: vpnRoutesOrder carries rib and path_id because two
// siblings differing only there have no tie-break at all, evpnRoutesOrder
// carries the entire route key, and all three end in collector_id as this
// now does.
//
// n.node_key is redundant where it sits -- it is cityHash64 over exactly
// protocol, identifier, asn, bgpls_id, area and router_id, every one of which
// precedes it here, so there is no tie left for it to break. It is listed
// anyway for two reasons: it is the identity a caller pages on and hands back
// through LSNodeFilter.NodeKey, and a later edit that trimmed one of those
// six columns for brevity would find the tie still broken here instead of
// quietly restoring the defect this ordering was widened to fix.
const lsNodesOrder = `
ORDER BY sysname, n.router_ip, n.peer_ip, live_name, n.router_id,
         n.rib, n.protocol, n.identifier, n.asn, n.bgpls_id, n.area,
         n.node_key, n.collector_id`

// LSNodes reports the link-state nodes the fleet is advertising right now:
// one row per (collector, router, peer, rib, node_key) whose current-session,
// up peer still reports it. See lsNodesSQL's own doc comment for what
// "current" and "still reports it" mean here, LSNode's for why two observers
// of one node are two rows rather than one merged winner, and
// LSNode.DumpState for why an empty answer is not by itself proof that a
// topology is empty.
//
// Every field of f is optional and an unset one is omitted from the statement
// rather than compared against anything. Router, Peer and RIB narrow by
// observer; Protocol, Area, ASN and NodeKey narrow by the node's own
// identity. Area, ASN and NodeKey are pointers because zero is a real and
// common value in all three columns -- that argument belongs at
// LSNodeFilter's own doc comment, and it is the one thing about this
// signature a reader should not skip.
//
// f.State selects which objects the answer includes: live (the default, and
// the zero value), withdrawn, or any. A state this package does not
// recognize is refused before the statement is built, wrapping ErrBadFilter,
// rather than falling through to the default -- see LSState.predicate, which
// is where refusing and rendering were fused so that no entry point can skip
// the check.
//
// f.Limit caps the number of rows RETURNED, not the number matched -- 0 means
// no cap, so a caller that never sets it asks exactly what it always did. The
// cap is applied as a bare `LIMIT %d` after lsNodesOrder, never as a bound
// `?`: ClickHouse does not accept a placeholder in LIMIT, and there is
// nothing unsafe about the literal either, because the value never comes from
// a caller's text. Every route handler sets RouteFilter.Limit from the
// operator's own s.cfg.MaxPage, an int the daemon's config validated at
// startup, and the link-state handlers this package is being built for do the
// same; no /v1/ls path documents a ?limit= for a caller to supply. See
// CountLSNodes for how a caller learns whether a capped answer left anything
// out, and lsNodesOrder for why the ordering underneath that cap has to be a
// total one.
func (q *Q) LSNodes(ctx context.Context, f LSNodeFilter) ([]LSNode, error) {
	w, err := f.predicates()
	if err != nil {
		return nil, err
	}
	stmt := fmt.Sprintf(lsNodesSQL+lsNodesOrder, q.db, w.where(), w.havingClause())
	if f.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query ls nodes: %w", err)
	}
	defer rows.Close()
	return scanLSNodes(rows)
}

// CountLSNodes reports how many nodes match f, ignoring f.Limit -- the
// number meta.total_matched carries. It wraps lsNodesSQL, the identical
// statement LSNodes builds minus the ORDER BY, rather than re-deriving the
// predicate: a hand-written second version is how a count drifts from the
// query it describes, and a confident wrong total is worse than no total.
//
// Wrapping in SELECT count() FROM (...) counts the GROUP BY's output -- one
// row per live (observer, node) -- where a bare count over ls_nodes would
// count raw observations, inflated by every superseded row argMax discards.
func (q *Q) CountLSNodes(ctx context.Context, f LSNodeFilter) (uint64, error) {
	w, err := f.predicates()
	if err != nil {
		return 0, err
	}
	inner := fmt.Sprintf(lsNodesSQL, q.db, w.where(), w.havingClause())
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", w.values()...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ls nodes: %w", err)
	}
	return n, nil
}

// scanLSNodes drains rows into LSNodes' answer shape. Scan is positional: a
// column added to lsNodesSQL and forgotten here fails loudly, but a column
// REORDERED there and not here binds an area to an ASN without a word.
func scanLSNodes(rows driver.Rows) ([]LSNode, error) {
	var out []LSNode
	for rows.Next() {
		var n LSNode
		var withdraw uint8
		if err := rows.Scan(
			&n.RouterSysName, &n.RouterIP, &n.PeerIP, &n.Collector, &n.RIB,
			&n.Protocol, &n.Identifier, &n.ASN, &n.BGPLSID, &n.Area,
			&n.RouterID, &n.NodeKey,
			&n.RouterIDv4, &n.Name,
			&n.SRGBBase, &n.SRGBSize, &n.SRLBBase, &n.SRLBSize,
			&n.SRAlgorithms, &withdraw, &n.DumpState,
		); err != nil {
			return nil, fmt.Errorf("scan ls node: %w", err)
		}
		n.IsWithdraw = withdraw == 1
		unmapAll(&n.RouterIP, &n.PeerIP)
		out = append(out, n)
	}
	return out, rows.Err()
}

// LSNodesPage reports one page of one (router, peer)'s current link-state
// nodes, keyset-ordered by lsNodesKey -- (rib, router_id, node_key), via
// lsNodesPageOrder -- and pinned to one BMP session: this package's
// link-state counterpart of RIBPageUnicast. It shares its validation with
// ribPage through ribCursorScope and sessionStillCurrent (see both, in
// rib.go) rather than re-typing them, so see ribPage's own doc comment for
// the whole design: what the session pin buys and does not, why the
// supersession check runs after the page query rather than before it -- with
// extra force here, since lsNodesSQL's own INNER JOIN against cur turns a
// superseded session into an EMPTY page rather than an error, which reads as
// "the walk finished" unless this check catches it -- and why a page set is
// a smear rather than a snapshot.
//
// Router and Peer are both required, unlike a plain LSNodes call: an
// unscoped walk spans every session on the fleet, any of which can be
// superseded mid-walk (xr-p2 alone turned over 252 sessions in 467 hours),
// leaving nothing for the walk to be pinned to. An unscoped f is refused
// with ErrBadFilter rather than answering a fleet-wide dump one page at a
// time.
//
// f.RIB stays optional, exactly as it is for LSNodes and for the three RIB
// walks: it is part of lsNodesKey (see lsRibColumn), so a walk that leaves
// it unset spans every rib the peer has without losing correctness.
//
// f.Protocol, f.Area, f.ASN, f.NodeKey and f.State are node-IDENTITY
// narrowings, not scope, and a cursor cannot be combined with any of them:
// RIBCursor has nowhere to record which narrowing a walk started under, so a
// walk begun under one (or under none) and continued under another would
// silently return rows past the cursor from the wrong scope, with nothing
// anywhere to catch it -- see LSNodeFilter.narrowing. When f.Cursor is set and
// a narrowing is too, this returns ErrBadFilter naming it rather than
// guessing which of the two the caller meant. Paging stays available for a
// scoped, unfiltered walk; a filtered question keeps going through the
// existing capped LSNodes path. The upgrade, if filtered paging is ever
// wanted, is a filter fingerprint carried inside the cursor itself -- not
// built here, deliberately, to keep RIBCursor's shape shared across every
// walk in this package.
//
// f.Limit is resolved through clampRIBLimit, so an unset one takes
// DefaultRIBPage and is clamped to MaxRIBPage when it is too large --
// unlike a bare LSNodes call, where an unset f.Limit means no cap at all.
// Choosing MaxRIBPage instead of DefaultRIBPage for a scoped request with no
// limit= is the handler's decision, not this function's; see clampRIBLimit's
// own doc comment for the clamp's own bounds.
//
// Pass a nil f.Cursor for the first page and the *RIBCursor a previous page
// returned for every page after it. A nil returned cursor ends the walk.
func (q *Q) LSNodesPage(ctx context.Context, f LSNodeFilter) ([]LSNode, *RIBCursor, error) {
	if !f.Router.IsValid() {
		return nil, nil, fmt.Errorf("%w: a link-state node walk needs a router -- "+
			"an unscoped walk is a fleet-wide dump, not a wider answer", ErrBadFilter)
	}
	if !f.Peer.IsValid() {
		return nil, nil, fmt.Errorf("%w: a link-state node walk needs a peer", ErrBadFilter)
	}
	if f.Cursor != nil {
		if narrow := f.narrowing(); narrow != "" {
			return nil, nil, fmt.Errorf("%w: a paginated walk cannot be combined with "+
				"%s= -- RIBCursor has nowhere to record a node-identity narrowing, so a "+
				"walk begun under one filter and continued under a different one (or none "+
				"at all) would silently return rows past the cursor from the wrong scope; "+
				"drop it and page a scoped, unfiltered walk instead, or drop the cursor "+
				"and call LSNodes for a filtered, capped answer",
				ErrBadFilter, narrow)
		}
	}

	collector, sid, last, err := ribCursorScope(ctx, q, f.Cursor, f.Router, f.Peer, f.RIB)
	if err != nil {
		return nil, nil, err
	}
	if collector == "" {
		// No peer_events row for this router at all: it has no current
		// session, so it has no current link-state nodes either. That is
		// an answer, not an error -- see ribPage's own handling of the
		// same case -- and a nil cursor ends the walk in one page.
		return nil, nil, nil
	}

	w, err := f.predicates()
	if err != nil {
		return nil, nil, err
	}
	// The pin, exactly as ribStatement adds it: w.collector_id keeps a
	// second collector's view of this router out of the page, and
	// w.session_id pins the dump.
	w.eq("n.collector_id", collector)
	w.eq("n.session_id", sid)
	if err := w.keyset(lsNodesKey, last); err != nil {
		return nil, nil, err
	}

	size := clampRIBLimit(f.Limit)
	stmt := fmt.Sprintf(lsNodesSQL+lsNodesPageOrder, q.db, w.where(), w.havingClause())
	stmt += fmt.Sprintf(" LIMIT %d", size+1) // the probe row; see ribStatement

	out, err := func() ([]LSNode, error) {
		rows, err := q.conn.Query(ctx, stmt, w.values()...)
		if err != nil {
			return nil, fmt.Errorf("query ls nodes page: %w", err)
		}
		defer rows.Close()
		return scanLSNodes(rows)
	}()
	if err != nil {
		return nil, nil, err
	}

	// After the rows, never before -- see this function's own doc comment,
	// and ribPage's, on why the order is the difference between a loud
	// failure and a silent truncation.
	if err := q.sessionStillCurrent(ctx, collector, sid, f.Router); err != nil {
		return nil, nil, err
	}

	// The probe row: the statement asked for one more than the page size, so
	// its presence is proof another page exists rather than a guess that one
	// might. See ribStatement.
	if len(out) <= size {
		return out, nil, nil
	}
	out = out[:size]
	last1 := out[len(out)-1]
	return out, &RIBCursor{
		Collector: collector,
		SessionID: sid,
		Router:    f.Router,
		Peer:      f.Peer,
		RIB:       f.RIB,
		Last:      []any{last1.RIB, last1.RouterID, last1.NodeKey},
	}, nil
}

// lsPrefixCIDR is ls_prefixes' two storage columns rendered as the one CIDR
// string every other prefix in this contract is. It is a const because it is
// used three times -- the SELECT list, the exact-match predicate and the
// containment predicate -- and three spellings of one expression is three
// chances for the length to be dropped from one of them.
//
// Passing it to filters.covers (by way of coversIfAsked) works because
// covers INTERPOLATES its column argument rather than binding it: coversExpr
// splits the interpolated text on '/', which is exactly what this concat
// produces. Nothing here is caller-supplied, so interpolation is safe; the
// caller's address is still bound, three times, as covers' own doc comment
// describes.
const lsPrefixCIDR = `concat(p.prefix, '/', toString(p.prefix_len))`

// LSPrefix is one row of LSPrefixes: one prefix as one observer currently
// reports one node originating it, keyed (collector, router, peer, rib,
// node_key, prefix, prefix_len).
//
// Prefix is CIDR text, assembled by lsPrefixCIDR. PrefixSID means different
// things depending on PrefixSIDFlags -- with RFC 9085's V and L flags set it
// is an absolute label rather than an index into the node's SRGB -- so both
// travel together, and HasPrefixSID separates an absent TLV from index 0,
// which is a legal SID.
type LSPrefix struct {
	RouterSysName    string
	RouterIP, PeerIP netip.Addr
	Collector, RIB   string

	Protocol   uint8
	Identifier uint64
	ASN        uint32
	BGPLSID    uint32
	Area       uint32
	RouterID   string
	NodeKey    uint64
	Prefix     string

	PrefixSID      uint32
	PrefixSIDFlags uint8
	HasPrefixSID   bool
	PrefixMetric   uint32
	OSPFRouteType  uint8

	IsWithdraw bool
	DumpState  string
}

const lsPrefixesSQL = "WITH " + peerStateCTE + peerUpCTE + lsEorCTE + `
SELECT
    argMax(p.router_sysname, (p.seq, p.stream_seq)) AS sysname,
    p.router_ip, p.peer_ip, p.collector_id, p.rib,
    p.protocol, p.identifier, p.asn, p.bgpls_id, p.area, p.router_id, p.node_key,
    ` + lsPrefixCIDR + ` AS cidr,
    argMax(p.prefix_sid,        (p.seq, p.stream_seq)) AS live_prefix_sid,
    argMax(p.prefix_sid_flags,  (p.seq, p.stream_seq)) AS live_prefix_sid_flags,
    argMax(p.has_prefix_sid,    (p.seq, p.stream_seq)) AS live_has_prefix_sid,
    argMax(p.prefix_metric,     (p.seq, p.stream_seq)) AS live_prefix_metric,
    argMax(p.ospf_route_type,   (p.seq, p.stream_seq)) AS live_ospf_route_type,
    argMax(p.is_withdraw,       (p.seq, p.stream_seq)) AS live_is_withdraw,
    any(` + dumpStateExpr + `)                         AS dump_state
FROM %[1]s.ls_prefixes_current p
INNER JOIN cur
    ON p.collector_id = cur.collector_id
   AND p.router_ip    = cur.router_ip
   AND p.session_id   = cur.sid
INNER JOIN peer_up
    ON p.collector_id = peer_up.collector_id
   AND p.router_ip    = peer_up.router_ip
   AND p.peer_ip      = peer_up.peer_ip
   AND cur.sid        = peer_up.sid
LEFT JOIN peer_state
    ON p.collector_id = peer_state.collector_id
   AND p.router_ip    = peer_state.router_ip
   AND p.peer_ip      = peer_state.peer_ip
   AND p.rib          = peer_state.rib
   AND cur.sid        = peer_state.sid
LEFT JOIN eor
    ON eor.collector_id = p.collector_id
   AND eor.router_ip    = p.router_ip
   AND eor.peer_ip      = p.peer_ip
   AND eor.rib          = p.rib
   AND eor.session_id   = cur.sid
WHERE ` + servedGate + `%[2]s
GROUP BY p.collector_id, p.router_ip, p.peer_ip, p.rib,
         p.protocol, p.identifier, p.asn, p.bgpls_id, p.area, p.router_id,
         p.node_key, p.prefix, p.prefix_len
%[3]s`

// lsPrefixesOrder is lsNodesOrder's shape carried over to lsPrefixesSQL: a
// presentational prefix that reads the same way -- sysname, router_ip,
// peer_ip, then one human-readable column, live_name there and cidr here --
// followed by every column of the statement's OWN GROUP BY key, so the whole
// order is TOTAL rather than partial. See lsNodesOrder's own doc comment for
// why a partial order is not a cosmetic complaint once f.Limit is set: rows
// tied on every ORDER BY column come back in whatever order the merge
// happened to produce, and a page boundary landing inside that tie silently
// drops one row and repeats the other.
//
// p.prefix and p.prefix_len are listed even though cidr -- concat(p.prefix,
// '/', toString(p.prefix_len)) -- already breaks any tie between rows that
// differ in either: '/' cannot appear in a stored address, so the
// concatenation is injective and two rows with different (prefix,
// prefix_len) can never render the same cidr string. They are listed anyway
// for the reason lsNodesOrder lists the already-redundant node_key: a later
// edit to lsPrefixCIDR's separator or rendering would find the tie still
// broken here instead of quietly reopening the defect lsNodesOrder was
// widened to fix.
const lsPrefixesOrder = `
ORDER BY sysname, p.router_ip, p.peer_ip, cidr, p.rib,
         p.protocol, p.identifier, p.asn, p.bgpls_id, p.area, p.router_id,
         p.node_key, p.prefix, p.prefix_len, p.collector_id`

// LSPrefixes reports the link-state prefixes the fleet is originating right
// now: one row per (collector, router, peer, rib, node_key, prefix,
// prefix_len) whose current-session, up peer still reports it. See
// lsNodesSQL's own doc comment for what "current" and "still reports it"
// mean here -- lsPrefixesSQL is that same shape over ls_prefixes_current,
// for lsNodesSQL's own reason -- and LSPrefix's for why the storage split
// across prefix and prefix_len never reaches a caller.
//
// Every field of f is optional and an unset one is omitted from the
// statement rather than compared against anything. Router, Peer and RIB
// narrow by observer; Protocol, Area, ASN and NodeKey narrow by the node
// that originates the prefix, and are pointers for the reason LSNodeFilter's
// own doc comment gives. Prefix and Covers narrow by the prefix itself and
// are mutually exclusive -- see LSPrefixFilter.predicates, which refuses the
// pair via the same checkCovers the three route filters share, before a
// single predicate is built.
//
// f.State selects which objects the answer includes: live (the default),
// withdrawn, or any. A state this package does not recognize is refused
// before the statement is built, wrapping ErrBadFilter -- see
// LSState.predicate, which is where refusing and rendering were fused so
// that no entry point can skip the check.
//
// f.Limit caps the number of rows RETURNED, not the number matched, for
// LSNodes' own reason -- see it for why the cap is a bare `LIMIT %d` rather
// than a bound placeholder -- and CountLSPrefixes reports how many rows
// matched regardless of the cap.
func (q *Q) LSPrefixes(ctx context.Context, f LSPrefixFilter) ([]LSPrefix, error) {
	w, err := f.predicates()
	if err != nil {
		return nil, err
	}
	stmt := fmt.Sprintf(lsPrefixesSQL+lsPrefixesOrder, q.db, w.where(), w.havingClause())
	if f.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query ls prefixes: %w", err)
	}
	defer rows.Close()
	return scanLSPrefixes(rows)
}

// CountLSPrefixes is CountLSNodes' counterpart; see it for why the count
// wraps the identical statement rather than re-deriving the predicate.
func (q *Q) CountLSPrefixes(ctx context.Context, f LSPrefixFilter) (uint64, error) {
	w, err := f.predicates()
	if err != nil {
		return 0, err
	}
	inner := fmt.Sprintf(lsPrefixesSQL, q.db, w.where(), w.havingClause())
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", w.values()...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ls prefixes: %w", err)
	}
	return n, nil
}

// scanLSPrefixes drains rows into LSPrefixes' answer shape. Scan is
// positional, for scanLSNodes' own reason: a column added to lsPrefixesSQL
// and forgotten here fails loudly, but a column REORDERED there and not here
// binds a prefix metric to a route type without a word.
func scanLSPrefixes(rows driver.Rows) ([]LSPrefix, error) {
	var out []LSPrefix
	for rows.Next() {
		var p LSPrefix
		var withdraw, hasSID uint8
		if err := rows.Scan(
			&p.RouterSysName, &p.RouterIP, &p.PeerIP, &p.Collector, &p.RIB,
			&p.Protocol, &p.Identifier, &p.ASN, &p.BGPLSID, &p.Area,
			&p.RouterID, &p.NodeKey, &p.Prefix,
			&p.PrefixSID, &p.PrefixSIDFlags, &hasSID,
			&p.PrefixMetric, &p.OSPFRouteType, &withdraw, &p.DumpState,
		); err != nil {
			return nil, fmt.Errorf("scan ls prefix: %w", err)
		}
		p.HasPrefixSID = hasSID == 1
		p.IsWithdraw = withdraw == 1
		unmapAll(&p.RouterIP, &p.PeerIP)
		out = append(out, p)
	}
	return out, rows.Err()
}

// LSPrefixesPage reports one page of one (router, peer)'s current link-state
// prefixes, keyset-ordered by lsPrefixesKey -- (rib, cidr, node_key), via
// lsPrefixesPageOrder -- and pinned to one BMP session: this package's
// prefix counterpart of LSNodesPage. It shares the same validation, through
// ribCursorScope and sessionStillCurrent (see both, in rib.go), rather than
// re-typing it -- see LSNodesPage's own doc comment for the whole design:
// what the session pin buys and does not, why the supersession check runs
// after the page query rather than before it -- with the same extra force
// here that LSNodesPage's own comment describes, since lsPrefixesSQL's own
// INNER JOIN against cur turns a superseded session into an EMPTY page
// rather than an error -- and why a page set is a smear rather than a
// snapshot.
//
// Router and Peer are both required, unlike a plain LSPrefixes call, for
// LSNodesPage's own reason: an unscoped walk spans every session on the
// fleet, any of which can be superseded mid-walk, leaving nothing for the
// walk to be pinned to. An unscoped f is refused with ErrBadFilter rather
// than answering a fleet-wide dump one page at a time.
//
// f.RIB stays optional, exactly as it is for LSPrefixes and for
// LSNodesPage: it is part of lsPrefixesKey (see lsRibColumn), so a walk that
// leaves it unset spans every rib the peer has without losing correctness.
//
// f.Protocol, f.Area, f.ASN, f.NodeKey, f.Prefix, f.Covers and f.State are
// narrowings, not scope, and a cursor cannot be combined with any of them --
// see LSPrefixFilter.narrowing for the reason RIBCursor has nowhere to
// record which narrowing a walk started under. When f.Cursor is set and a
// narrowing is too, this returns ErrBadFilter naming it rather than
// guessing which of the two the caller meant. Paging stays available for a
// scoped, unfiltered walk; a filtered question keeps going through the
// existing capped LSPrefixes path. The upgrade, if filtered paging is ever
// wanted, is a filter fingerprint carried inside the cursor itself -- not
// built here, deliberately, to keep RIBCursor's shape shared across every
// walk in this package.
//
// f.Limit is resolved through clampRIBLimit, so an unset one takes
// DefaultRIBPage and is clamped to MaxRIBPage when it is too large --
// unlike a bare LSPrefixes call, where an unset f.Limit means no cap at
// all. Choosing MaxRIBPage instead of DefaultRIBPage for a scoped request
// with no limit= is the handler's decision, not this function's; see
// clampRIBLimit's own doc comment for the clamp's own bounds.
//
// Pass a nil f.Cursor for the first page and the *RIBCursor a previous page
// returned for every page after it. A nil returned cursor ends the walk.
func (q *Q) LSPrefixesPage(ctx context.Context, f LSPrefixFilter) ([]LSPrefix, *RIBCursor, error) {
	if !f.Router.IsValid() {
		return nil, nil, fmt.Errorf("%w: a link-state prefix walk needs a router -- "+
			"an unscoped walk is a fleet-wide dump, not a wider answer", ErrBadFilter)
	}
	if !f.Peer.IsValid() {
		return nil, nil, fmt.Errorf("%w: a link-state prefix walk needs a peer", ErrBadFilter)
	}
	if f.Cursor != nil {
		if narrow := f.narrowing(); narrow != "" {
			return nil, nil, fmt.Errorf("%w: a paginated walk cannot be combined with "+
				"%s= -- RIBCursor has nowhere to record a narrowing, so a walk begun under "+
				"one filter and continued under a different one (or none at all) would "+
				"silently return rows past the cursor from the wrong scope; drop it and "+
				"page a scoped, unfiltered walk instead, or drop the cursor and call "+
				"LSPrefixes for a filtered, capped answer",
				ErrBadFilter, narrow)
		}
	}

	collector, sid, last, err := ribCursorScope(ctx, q, f.Cursor, f.Router, f.Peer, f.RIB)
	if err != nil {
		return nil, nil, err
	}
	if collector == "" {
		// No peer_events row for this router at all: it has no current
		// session, so it has no current link-state prefixes either. That is
		// an answer, not an error -- see ribPage's own handling of the same
		// case -- and a nil cursor ends the walk in one page.
		return nil, nil, nil
	}

	w, err := f.predicates()
	if err != nil {
		return nil, nil, err
	}
	// The pin, exactly as LSNodesPage adds it: w.collector_id keeps a
	// second collector's view of this router out of the page, and
	// w.session_id pins the dump.
	w.eq("p.collector_id", collector)
	w.eq("p.session_id", sid)
	if err := w.keyset(lsPrefixesKey, last); err != nil {
		return nil, nil, err
	}

	size := clampRIBLimit(f.Limit)
	stmt := fmt.Sprintf(lsPrefixesSQL+lsPrefixesPageOrder, q.db, w.where(), w.havingClause())
	stmt += fmt.Sprintf(" LIMIT %d", size+1) // the probe row; see ribStatement

	out, err := func() ([]LSPrefix, error) {
		rows, err := q.conn.Query(ctx, stmt, w.values()...)
		if err != nil {
			return nil, fmt.Errorf("query ls prefixes page: %w", err)
		}
		defer rows.Close()
		return scanLSPrefixes(rows)
	}()
	if err != nil {
		return nil, nil, err
	}

	// After the rows, never before -- see this function's own doc comment,
	// and ribPage's, on why the order is the difference between a loud
	// failure and a silent truncation.
	if err := q.sessionStillCurrent(ctx, collector, sid, f.Router); err != nil {
		return nil, nil, err
	}

	// The probe row: the statement asked for one more than the page size, so
	// its presence is proof another page exists rather than a guess that one
	// might. See ribStatement.
	if len(out) <= size {
		return out, nil, nil
	}
	out = out[:size]
	last1 := out[len(out)-1]
	return out, &RIBCursor{
		Collector: collector,
		SessionID: sid,
		Router:    f.Router,
		Peer:      f.Peer,
		RIB:       f.RIB,
		Last:      []any{last1.RIB, last1.Prefix, last1.NodeKey},
	}, nil
}

// lsNodeLabelsCTE holds the two lookup tiers a node's label can come from,
// and they are separate CTEs rather than one because they answer
// different questions.
//
// ls_node_labels is keyed by the OBSERVER as well as the node: it answers
// "what does this router call that node". ls_fleet_labels is keyed by
// node_key alone: "what does anyone call it". The link statement tries the
// first, falls back to the second, and falls back finally to the raw
// router_id the link row itself carries -- so the label is never empty and
// its origin is always reportable.
//
// Both CTEs read ls_nodes_current, not ls_nodes, but for two different
// reasons that must not be confused with each other.
//
// ls_node_labels reads it because lsLinksSQL does: the label join's
// candidate keys and the link row it is joined against are drawn from the
// same current-session state, so a link whose session has moved on cannot
// join to a label row this observer reported under an ENDED session. It
// stays SCOPED by session_id -- GROUP BY still carries it, and the join
// still pins it to cur.sid -- so this tier's reach is unchanged by which
// table backs it; only its correctness under a superseded session is.
//
// ls_fleet_labels reads it for the opposite reason: to stay fleet-wide for
// LONGER than ls_nodes' own 90-day TTL allows. It is still keyed by node_key
// alone, still spanning every session and observer on purpose (see below) --
// moving to ls_nodes_current changes neither its GROUP BY nor its scope, and
// it remains unjoined to peer_up or cur, so a session need not be current,
// or even still open, for its name to count. What changes is retention:
// ls_nodes_current carries no TTL, so a name survives there for as long as
// the writer's superseded-session cleanup keeps that (collector, router,
// session, peer, rib, node identity) row around -- which for an ACTIVE
// session is indefinitely, and for an ended one is until cleanup drops it,
// not merely until 90 days have passed. A node named only by an observer
// whose session ended 91 days ago used to fall back to the raw router_id at
// that point; now it keeps the name until the session itself is cleaned up.
// Once cleanup does drop it, ls_fleet_labels has nothing left to argMax over
// for that row and falls through exactly as it always has when no observer
// has ever named a node -- cleanup is a real boundary this tier still
// respects, TTL was never the boundary it was meant to have.
//
// The middle tier reads ACROSS OBSERVERS, which this package otherwise
// refuses for link-state (see LSNode's own doc comment on why two observers
// of one node are two rows rather than one merged winner). It is admissible
// here and nowhere else because a label is PRESENTATION over node_key, and
// node_key hashes protocol, identifier, asn, bgpls_id, area and router_id --
// none of which is a property of who saw the node. Borrowing a name across
// observers renders an identifier the two already agree on; it does not
// arbitrate conflicting state. Every field that IS state stays per-observer,
// resolved by the link row's own argMax.
//
// All three tiers carry real traffic on real data; none is a theoretical
// branch. Two censuses say so at two different scopes, and the scope has to
// travel with the numbers:
//
//   - UNGATED, every raw ls_links row the archive has ever held, local ends
//     only, before argMax collapse and before the peer_up liveness join:
//     2,333 rows, of which the observer tier labels 1,694 (73%), the fleet
//     fallback takes it to 2,082 (89%), and the remaining 251 reach the raw
//     identifier.
//   - GATED, what LSLinks actually returns at state=live: 222 endpoints
//     across 111 current-state links, of which 162 resolve from the
//     observer tier, 4 from the fleet fallback and 56 from the raw
//     identifier, with zero empty labels (measured 2026-08-30).
//
// The two are different populations, not a disagreement -- "how would every
// observation ever ingested label its local end" is a different question
// from "how do today's live links label both ends" -- and the conclusion
// survives either way. The gated fleet count is small (4) for a measured
// reason: those 222 endpoints span only 17 distinct node_keys, and exactly 3
// of them had a reporting observer with neither a name nor a router ID while
// another observer had one -- the only position the fleet tier can fire in.
//
// Both use argMax, never any(). An arbitrary winner would make one request
// answer differently between calls, which is a worse failure than hex: a
// name that changes under a reader who did not change the question is a
// reason to distrust every other column beside it.
//
// Neither CTE filters is_withdraw, deliberately. A node that has since been
// withdrawn is still the best name anyone has for the identifier a live link
// row carries -- and a link whose far end was withdrawn while the link
// itself was not is exactly the case an operator is looking at when they
// need the name most.
//
// ls_fleet_labels spans SESSIONS as well as observers, since node_key
// carries nothing about either. That is the same "presentation, not state"
// bargain, taken one step further: the fleet tier is allowed to remember
// what a router called a node during a session that has ended, because the
// alternative is hex. The observer tier is NOT allowed to -- see
// lsLinksSQL's label joins, which pin session_id to cur.sid, and
// TestLSLinkLabelsPreferTheReportingObserver's fourth case, which is the one
// that fails if that pin is removed. Reading ls_nodes_current stretches
// exactly this allowance across retention too, for the reason given above --
// TestLSLinkFleetLabelSurvivesNodeHistoryRetention is that case's own
// counterpart, and fails the same way if this tier is pointed back at
// ls_nodes.
//
// Both are one row per join key -- each GROUPs BY exactly the columns
// lsLinksSQL binds it on -- which is what keeps the two LEFT JOINs apiece
// there from fanning one link into several rows. That is asserted rather
// than assumed, by TestLSLabelCTEsAreOneRowPerJoinKey, which runs this text
// and looks for a key carrying more than one row.
//
// It is NOT held by comparing CountLSLinks against the answer. Both sides
// of that comparison read the GROUP BY's output, so fan-out moves them
// together and they can never disagree. Duplicating every row of
// ls_node_labels, with the duplicates carrying different labels, left the
// whole package green. The failure a duplicated key produces is not a
// wrong count -- it is an arbitrary LABEL, picked by the any() in the
// SELECT from among rows that should have been one.
const lsNodeLabelsCTE = `,
ls_node_labels AS (
    SELECT collector_id, router_ip, peer_ip, rib, session_id, node_key,
           argMax(name,         (seq, stream_seq)) AS nm,
           argMax(router_id_v4, (seq, stream_seq)) AS r4
    FROM %[1]s.ls_nodes_current
    GROUP BY collector_id, router_ip, peer_ip, rib, session_id, node_key
),
ls_fleet_labels AS (
    SELECT node_key,
           argMax(name,         (seq, stream_seq)) AS nm,
           argMax(router_id_v4, (seq, stream_seq)) AS r4
    FROM %[1]s.ls_nodes_current
    GROUP BY node_key
)`

// lsLabelExpr renders one endpoint's label. %[1]s is the observer-scoped
// alias, %[2]s the fleet-scoped one, and %[3]s the raw identifier column on
// the link row itself.
//
// The order of the branches IS the three-tier rule, and reordering them
// changes the answer rather than the style: the reporting router's own name
// for a node must beat another router's, because "what this observer sees"
// is the question every other column in the row answers.
//
// The chain ends in a column, not in a literal or an empty string, which is
// what makes LSEndpoint.Label total: local_router_id and remote_router_id
// are part of ls_links' sort key and every row has them.
const lsLabelExpr = `multiIf(
    %[1]s.nm != '', %[1]s.nm,
    %[1]s.r4 != '', %[1]s.r4,
    %[2]s.nm != '', %[2]s.nm,
    %[2]s.r4 != '', %[2]s.r4,
    %[3]s)`

// lsLabelSourceExpr names the tier lsLabelExpr's answer came from.
//
// The two are computed by one multiIf apiece rather than by a shared
// intermediate because ClickHouse has nowhere to put an alias inside a
// spliced SELECT expression -- the same constraint coversExpr works around
// by interpolating its column argument.
//
// THE TWO EXPRESSIONS MUST STAY IN STEP. A label from one tier reported as
// coming from another is worse than no report at all: a caller told a name
// came from the reporting observer will believe the two ends of a link were
// named by the router that reported it, and act on a name borrowed from a
// third router. The branch conditions here are the disjunction of the branch
// conditions there, tier by tier, and that correspondence is what
// TestLSLinkLabelsPreferTheReportingObserver checks by asserting the label
// and the tier together rather than either alone.
const lsLabelSourceExpr = `multiIf(
    %[1]s.nm != '' OR %[1]s.r4 != '', 'observer',
    %[2]s.nm != '' OR %[2]s.r4 != '', 'fleet',
    'identifier')`

// LSEndpoint is one end of a link: its raw identity, plus a label resolved
// through three tiers -- the reporting observer's own name, then any other
// observer's (fleet), then the raw identifier -- and the name of the tier
// that answered.
//
// Label is NEVER empty -- the chain ends in RouterID, which every link row
// carries -- so a caller does not have to handle an absent one. LabelSource
// is one of "observer", "fleet" or "identifier", and it exists so that "this
// router told me the name" and "another router did" are distinguishable
// rather than blended: the second is a weaker claim, and a topology drawn
// from it is asserting an adjacency between a node this router named and one
// it never mentioned.
//
// It is a named struct rather than two flattened sets of fields so the two
// ends cannot drift apart -- a field added for the local end and forgotten
// for the remote one is not expressible.
type LSEndpoint struct {
	ASN         uint32
	BGPLSID     uint32
	Area        uint32
	RouterID    string
	NodeKey     uint64
	IfAddr      string
	InterfaceID uint32

	Label       string
	LabelSource string
}

// LSLink is one row of LSLinks: one adjacency as ONE observer currently
// reports it, keyed by the observer plus the full link tuple -- both node
// keys, both interface addresses and both link IDs.
//
// The key is that wide because parallel links between the same pair of nodes
// are ordinary, and collapsing them would report a redundant pair as a
// single adjacency: an operator looking at a bundle would see one link where
// there are two, which is the difference between "this path has no
// redundancy" and "it does".
//
// One row per observer, not per adjacency, for LSNode's own reason. The two
// directions of one adjacency are also two rows, reported by whichever
// observers see each: a link is advertised by the node at its local end, and
// nothing here pairs A->B with B->A.
//
// IsWithdraw is carried on every row regardless of the filter's State, so a
// caller reading a state=any answer never has to infer an object's state
// from the request it made.
type LSLink struct {
	RouterSysName    string
	RouterIP, PeerIP netip.Addr
	Collector, RIB   string

	Protocol   uint8
	Identifier uint64
	Local      LSEndpoint
	Remote     LSEndpoint

	AdjSIDs      []uint32
	TEMetric     uint32
	IGPMetric    uint32
	AdminGroup   uint32
	MaxBandwidth float32

	IsWithdraw bool
	DumpState  string
}

// lsLinksSQL is lsNodesSQL's shape over ls_links_current -- the same
// current-session pin, the same peer_up liveness test, the same
// argMax-by-(seq, stream_seq) resolution of every mutable attribute, the
// same dumpStateExpr and the same refusal to use FINAL -- plus the four
// label joins nothing else in this package needs. See lsNodesSQL's own doc
// comment for the ordering argument; it is this package's one convention and
// links follow it.
//
// It is a var rather than a const, unlike every other statement here, for
// one mechanical reason: fmt.Sprintf is not a constant expression, and the
// four label expressions are rendered from lsLabelExpr and lsLabelSourceExpr
// at package initialization so that the local and remote ends cannot be
// spelled differently. Writing them out by hand four times would make them
// consts again and would be exactly the drift lsLabelSourceExpr's doc
// comment warns about.
//
// The label joins are LEFT, all four, and that is the difference between a
// label and a filter: a node nobody has named must still produce a link row,
// with the identifier tier answering. An INNER JOIN here would silently
// delete 11% of the archive's links -- the ones whose ends reach neither
// named tier -- and the deletion would look like a topology gap rather than
// a join mistake.
//
// The two observer-scoped joins pin session_id to cur.sid. That predicate is
// LOAD-BEARING, not defense in depth: ls_node_labels is keyed by session, so
// without it a link joins to every session's label rows for the same
// (collector, router, peer, rib, node_key) at once, and the any() below then
// picks among them arbitrarily -- the same "answers differently between
// calls" failure lsNodeLabelsCTE refuses argMax's alternative for. It also
// promotes a name from a session that has ENDED to the observer tier, which
// is a claim about what this router says now.
// TestLSLinkLabelsPreferTheReportingObserver's fourth case fails
// deterministically without it.
//
// The label columns are wrapped in any() rather than argMax because they are
// not mutable attributes of the link: each is already resolved to one value
// per group by its CTE, and the joins are one row per key, so every row in
// the group carries the identical label. any() over a single distinct value
// is deterministic; it would not be if a join above it fanned out, which is
// why that property is asserted directly, by
// TestLSLabelCTEsAreOneRowPerJoinKey, rather than assumed. Comparing the
// count against the answer cannot see it -- see CountLSLinks.
//
// %[3]s is havingClause(), not having(): state=any renders no
// post-aggregation predicate at all, so there is no fixed HAVING here for an
// AND to attach to.
var lsLinksSQL = "WITH " + peerStateCTE + peerUpCTE + lsEorCTE + lsNodeLabelsCTE + `
SELECT
    argMax(l.router_sysname, (l.seq, l.stream_seq)) AS sysname,
    l.router_ip, l.peer_ip, l.collector_id, l.rib,
    l.protocol, l.identifier,
    l.local_asn, l.local_bgpls_id, l.local_area, l.local_router_id,
    l.local_node_key, l.local_ifaddr, l.link_local_id,
    any(` + fmt.Sprintf(lsLabelExpr, "lloc", "floc", "l.local_router_id") + `) AS local_label,
    any(` + fmt.Sprintf(lsLabelSourceExpr, "lloc", "floc") + `) AS local_label_source,
    l.remote_asn, l.remote_bgpls_id, l.remote_area, l.remote_router_id,
    l.remote_node_key, l.remote_ifaddr, l.link_remote_id,
    any(` + fmt.Sprintf(lsLabelExpr, "lrem", "frem", "l.remote_router_id") + `) AS remote_label,
    any(` + fmt.Sprintf(lsLabelSourceExpr, "lrem", "frem") + `) AS remote_label_source,
    argMax(l.adj_sids,      (l.seq, l.stream_seq)) AS live_adj_sids,
    argMax(l.te_metric,     (l.seq, l.stream_seq)) AS live_te_metric,
    argMax(l.igp_metric,    (l.seq, l.stream_seq)) AS live_igp_metric,
    argMax(l.admin_group,   (l.seq, l.stream_seq)) AS live_admin_group,
    argMax(l.max_bandwidth, (l.seq, l.stream_seq)) AS live_max_bandwidth,
    argMax(l.is_withdraw,   (l.seq, l.stream_seq)) AS live_is_withdraw,
    any(` + dumpStateExpr + `)                     AS dump_state
FROM %[1]s.ls_links_current l
INNER JOIN cur
    ON l.collector_id = cur.collector_id
   AND l.router_ip    = cur.router_ip
   AND l.session_id   = cur.sid
INNER JOIN peer_up
    ON l.collector_id = peer_up.collector_id
   AND l.router_ip    = peer_up.router_ip
   AND l.peer_ip      = peer_up.peer_ip
   AND cur.sid        = peer_up.sid
LEFT JOIN peer_state
    ON l.collector_id = peer_state.collector_id
   AND l.router_ip    = peer_state.router_ip
   AND l.peer_ip      = peer_state.peer_ip
   AND l.rib          = peer_state.rib
   AND cur.sid        = peer_state.sid
LEFT JOIN eor
    ON eor.collector_id = l.collector_id
   AND eor.router_ip    = l.router_ip
   AND eor.peer_ip      = l.peer_ip
   AND eor.rib          = l.rib
   AND eor.session_id   = cur.sid
LEFT JOIN ls_node_labels AS lloc
    ON lloc.collector_id = l.collector_id
   AND lloc.router_ip    = l.router_ip
   AND lloc.peer_ip      = l.peer_ip
   AND lloc.rib          = l.rib
   AND lloc.session_id   = cur.sid
   AND lloc.node_key     = l.local_node_key
LEFT JOIN ls_node_labels AS lrem
    ON lrem.collector_id = l.collector_id
   AND lrem.router_ip    = l.router_ip
   AND lrem.peer_ip      = l.peer_ip
   AND lrem.rib          = l.rib
   AND lrem.session_id   = cur.sid
   AND lrem.node_key     = l.remote_node_key
LEFT JOIN ls_fleet_labels AS floc ON floc.node_key = l.local_node_key
LEFT JOIN ls_fleet_labels AS frem ON frem.node_key = l.remote_node_key
WHERE ` + servedGate + `%[2]s
GROUP BY l.collector_id, l.router_ip, l.peer_ip, l.rib, l.protocol, l.identifier,
         l.local_asn, l.local_bgpls_id, l.local_area, l.local_router_id,
         l.local_node_key, l.local_ifaddr, l.link_local_id,
         l.remote_asn, l.remote_bgpls_id, l.remote_area, l.remote_router_id,
         l.remote_node_key, l.remote_ifaddr, l.link_remote_id
%[3]s`

// lsLinksOrder is the ordering LSLinks reports its answer in, appended at
// the one call site that wants it. CountLSLinks does NOT wrap it, for
// lsNodesOrder's reason: ordering rows whose only consumer is count() is
// work thrown away.
//
// It has two halves and only the first reads for a human: sysname,
// router_ip, peer_ip and the two labels, so a fleet-wide answer reads router
// by router and a reader sees each observer's links between NAMES rather
// than between hex identifiers -- which is the whole point of resolving a
// label at all. router_ip and peer_ip are GROUP BY columns as well as
// presentational ones, the same overlap lsNodesOrder's own comment names.
//
// Everything after that is the REST of this statement's GROUP BY key --
// exactly 18 of its 20 columns, the two already spent above being router_ip
// and peer_ip, and collector_id is one of the 18 rather than an extra --
// and it is there to make the order TOTAL
// rather than to make it read well. See lsNodesOrder for the argument and
// for what a partial order cost when it shipped: rows tied on every ORDER BY
// column come back in whatever order the merge happened to produce, so once
// Limit is set (and every handler sets it from the operator's page cap) a
// page boundary landing inside a tie silently drops one row and repeats
// another, with nothing in the answer saying so.
//
// This key is the widest in the slice, and the presentational half covers
// less of it than anywhere else: two links between the same pair of NAMED
// nodes -- a bundle, which is ordinary -- agree on sysname, both addresses
// and both labels, and differ only in their interface addresses and link
// IDs. Parallel links are precisely the shape the wide GROUP BY key exists
// to keep apart (see LSLink), so they are also precisely the shape that
// would tie here, and the tail is what breaks it. Measured on the live
// archive, on the five presentational columns alone: 52 tied groups
// covering 158 of 199 rows at state=any -- 79% of the answer, the highest
// of the three resources.
//
// The two node_key columns are redundant where they sit -- each is
// cityHash64 over six columns that all precede it here -- and are listed for
// lsNodesOrder's stated reason: they are the identity a caller pages on and
// hands back through LSLinkFilter.LocalNode and RemoteNode, and a later edit
// that trimmed one of those six columns for brevity would find the tie still
// broken instead of quietly reopening the defect this ordering exists to
// close. collector_id ends it, as it ends every other ordering in this
// package.
const lsLinksOrder = `
ORDER BY sysname, l.router_ip, l.peer_ip, local_label, remote_label,
         l.rib, l.protocol, l.identifier,
         l.local_asn, l.local_bgpls_id, l.local_area, l.local_router_id,
         l.local_node_key, l.local_ifaddr, l.link_local_id,
         l.remote_asn, l.remote_bgpls_id, l.remote_area, l.remote_router_id,
         l.remote_node_key, l.remote_ifaddr, l.link_remote_id,
         l.collector_id`

// LSLinks reports the link-state adjacencies the fleet is advertising right
// now: one row per (collector, router, peer, rib, link tuple) whose
// current-session, up peer still reports it. See lsNodesSQL's own doc
// comment for what "current" and "still reports it" mean here, LSLink's for
// why parallel links between one pair of nodes stay distinct, and
// LSEndpoint's for what the two labels on every row are and are not claiming.
//
// This is the one statement in the slice that spans a join, and the one
// whose output is assembled from more than one observer's data -- but only
// its LABELS are. Every field that is state stays per-observer; see
// lsNodeLabelsCTE for why that line is drawn where it is.
//
// Every field of f is optional and an unset one is omitted from the
// statement rather than compared against anything. Router, Peer and RIB
// narrow by observer; Protocol narrows by the routing protocol the link was
// learned from. Area and ASN match EITHER END, which is the one place this
// package's filters do that -- see LSLinkFilter's own doc comment for why a
// both-ends reading would silently drop every area-crossing link -- and
// LocalNode and RemoteNode narrow by one specific end, deliberately not
// either.
//
// f.State selects which objects the answer includes: live (the default, and
// the zero value), withdrawn, or any. A state this package does not
// recognize is refused before the statement is built, wrapping ErrBadFilter,
// rather than falling through to the default -- see LSState.predicate, which
// is where refusing and rendering were fused so that no entry point can skip
// the check.
//
// f.Limit caps the number of rows RETURNED, not the number matched -- 0
// means no cap -- for LSNodes' own reason; see it for why the cap is a bare
// `LIMIT %d` rather than a bound placeholder, and lsLinksOrder for why the
// ordering underneath that cap has to be a total one. CountLSLinks reports
// how many rows matched regardless of the cap.
func (q *Q) LSLinks(ctx context.Context, f LSLinkFilter) ([]LSLink, error) {
	w, err := f.predicates()
	if err != nil {
		return nil, err
	}
	stmt := fmt.Sprintf(lsLinksSQL+lsLinksOrder, q.db, w.where(), w.havingClause())
	if f.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query ls links: %w", err)
	}
	defer rows.Close()
	return scanLSLinks(rows)
}

// CountLSLinks is CountLSNodes' counterpart; see it for why the count wraps
// the identical statement rather than re-deriving the predicate.
//
// It counts LINKS, not joined rows, and that distinction is real here in a
// way it is not for the other two: this statement carries four LEFT JOINs
// that a mis-keyed label CTE could fan out, and a count over the join's
// output would then report more adjacencies than the fleet has. What makes
// it correct is that count() reads the GROUP BY's OUTPUT rather than the
// join's.
//
// That same fact is why this function is no evidence about the label joins,
// which is worth stating where a reader will look for it:
// TestCountLSLinksMatchesTheAnswerAndIgnoresLimit holds this count against
// LSLinks' own row count, and both sides read the GROUP BY's output, so a
// label CTE emitting two rows per key leaves the pair agreeing exactly as
// before. The one-row-per-key property is asserted where it lives instead,
// on the CTEs themselves -- see TestLSLabelCTEsAreOneRowPerJoinKey.
func (q *Q) CountLSLinks(ctx context.Context, f LSLinkFilter) (uint64, error) {
	w, err := f.predicates()
	if err != nil {
		return 0, err
	}
	inner := fmt.Sprintf(lsLinksSQL, q.db, w.where(), w.havingClause())
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", w.values()...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ls links: %w", err)
	}
	return n, nil
}

// scanLSLinks drains rows into LSLinks' answer shape. Scan is positional,
// for scanLSNodes' own reason, and this statement is where that matters
// most: it returns two endpoints of identical shape back to back, so a
// column REORDERED in lsLinksSQL and not here does not fail -- it binds the
// remote end's ASN to the local end's and reports a link that runs the other
// way, in an answer that still type-checks and still looks like a topology.
func scanLSLinks(rows driver.Rows) ([]LSLink, error) {
	var out []LSLink
	for rows.Next() {
		var l LSLink
		var withdraw uint8
		if err := rows.Scan(
			&l.RouterSysName, &l.RouterIP, &l.PeerIP, &l.Collector, &l.RIB,
			&l.Protocol, &l.Identifier,
			&l.Local.ASN, &l.Local.BGPLSID, &l.Local.Area, &l.Local.RouterID,
			&l.Local.NodeKey, &l.Local.IfAddr, &l.Local.InterfaceID,
			&l.Local.Label, &l.Local.LabelSource,
			&l.Remote.ASN, &l.Remote.BGPLSID, &l.Remote.Area, &l.Remote.RouterID,
			&l.Remote.NodeKey, &l.Remote.IfAddr, &l.Remote.InterfaceID,
			&l.Remote.Label, &l.Remote.LabelSource,
			&l.AdjSIDs, &l.TEMetric, &l.IGPMetric, &l.AdminGroup,
			&l.MaxBandwidth, &withdraw, &l.DumpState,
		); err != nil {
			return nil, fmt.Errorf("scan ls link: %w", err)
		}
		l.IsWithdraw = withdraw == 1
		unmapAll(&l.RouterIP, &l.PeerIP)
		out = append(out, l)
	}
	return out, rows.Err()
}

// LSLinksPage reports one page of one (router, peer)'s current link-state
// links, keyset-ordered by lsLinksKey -- (rib, local_router_id,
// local_node_key, remote_node_key, link_local_id, link_remote_id,
// local_ifaddr, remote_ifaddr), via lsLinksPageOrder -- and pinned to one
// BMP session: this package's link counterpart of LSNodesPage and
// LSPrefixesPage. It shares the same validation, through ribCursorScope and
// sessionStillCurrent (see both, in rib.go), rather than re-typing it -- see
// LSNodesPage's own doc comment for the whole design: what the session pin
// buys and does not, why the supersession check runs after the page query
// rather than before it -- with the same extra force here that LSNodesPage's
// own comment describes, since lsLinksSQL's own INNER JOIN against cur turns
// a superseded session into an EMPTY page rather than an error -- and why a
// page set is a smear rather than a snapshot.
//
// Router and Peer are both required, unlike a plain LSLinks call, for
// LSNodesPage's own reason: an unscoped walk spans every session on the
// fleet, any of which can be superseded mid-walk, leaving nothing for the
// walk to be pinned to. An unscoped f is refused with ErrBadFilter rather
// than answering a fleet-wide dump one page at a time.
//
// f.RIB stays optional, exactly as it is for LSLinks and for LSNodesPage and
// LSPrefixesPage: it is part of lsLinksKey (see lsRibColumn), so a walk that
// leaves it unset spans every rib the peer has without losing correctness.
//
// f.Protocol, f.Area, f.ASN, f.LocalNode, f.RemoteNode and f.State are
// narrowings, not scope, and a cursor cannot be combined with any of them --
// see LSLinkFilter.narrowing for the reason RIBCursor has nowhere to record
// which narrowing a walk started under. When f.Cursor is set and a
// narrowing is too, this returns ErrBadFilter naming it rather than
// guessing which of the two the caller meant. Paging stays available for a
// scoped, unfiltered walk; a filtered question keeps going through the
// existing capped LSLinks path. The upgrade, if filtered paging is ever
// wanted, is a filter fingerprint carried inside the cursor itself -- not
// built here, deliberately, to keep RIBCursor's shape shared across every
// walk in this package.
//
// f.Limit is resolved through clampRIBLimit, so an unset one takes
// DefaultRIBPage and is clamped to MaxRIBPage when it is too large --
// unlike a bare LSLinks call, where an unset f.Limit means no cap at all.
// Choosing MaxRIBPage instead of DefaultRIBPage for a scoped request with no
// limit= is the handler's decision, not this function's; see clampRIBLimit's
// own doc comment for the clamp's own bounds.
//
// Pass a nil f.Cursor for the first page and the *RIBCursor a previous page
// returned for every page after it. A nil returned cursor ends the walk.
func (q *Q) LSLinksPage(ctx context.Context, f LSLinkFilter) ([]LSLink, *RIBCursor, error) {
	if !f.Router.IsValid() {
		return nil, nil, fmt.Errorf("%w: a link-state link walk needs a router -- "+
			"an unscoped walk is a fleet-wide dump, not a wider answer", ErrBadFilter)
	}
	if !f.Peer.IsValid() {
		return nil, nil, fmt.Errorf("%w: a link-state link walk needs a peer", ErrBadFilter)
	}
	if f.Cursor != nil {
		if narrow := f.narrowing(); narrow != "" {
			return nil, nil, fmt.Errorf("%w: a paginated walk cannot be combined with "+
				"%s= -- RIBCursor has nowhere to record a narrowing, so a walk begun under "+
				"one filter and continued under a different one (or none at all) would "+
				"silently return rows past the cursor from the wrong scope; drop it and "+
				"page a scoped, unfiltered walk instead, or drop the cursor and call "+
				"LSLinks for a filtered, capped answer",
				ErrBadFilter, narrow)
		}
	}

	collector, sid, last, err := ribCursorScope(ctx, q, f.Cursor, f.Router, f.Peer, f.RIB)
	if err != nil {
		return nil, nil, err
	}
	if collector == "" {
		// No peer_events row for this router at all: it has no current
		// session, so it has no current link-state links either. That is
		// an answer, not an error -- see ribPage's own handling of the same
		// case -- and a nil cursor ends the walk in one page.
		return nil, nil, nil
	}

	w, err := f.predicates()
	if err != nil {
		return nil, nil, err
	}
	// The pin, exactly as LSNodesPage adds it: w.collector_id keeps a
	// second collector's view of this router out of the page, and
	// w.session_id pins the dump.
	w.eq("l.collector_id", collector)
	w.eq("l.session_id", sid)
	if err := w.keyset(lsLinksKey, last); err != nil {
		return nil, nil, err
	}

	size := clampRIBLimit(f.Limit)
	stmt := fmt.Sprintf(lsLinksSQL+lsLinksPageOrder, q.db, w.where(), w.havingClause())
	stmt += fmt.Sprintf(" LIMIT %d", size+1) // the probe row; see ribStatement

	out, err := func() ([]LSLink, error) {
		rows, err := q.conn.Query(ctx, stmt, w.values()...)
		if err != nil {
			return nil, fmt.Errorf("query ls links page: %w", err)
		}
		defer rows.Close()
		return scanLSLinks(rows)
	}()
	if err != nil {
		return nil, nil, err
	}

	// After the rows, never before -- see this function's own doc comment,
	// and ribPage's, on why the order is the difference between a loud
	// failure and a silent truncation.
	if err := q.sessionStillCurrent(ctx, collector, sid, f.Router); err != nil {
		return nil, nil, err
	}

	// The probe row: the statement asked for one more than the page size, so
	// its presence is proof another page exists rather than a guess that one
	// might. See ribStatement.
	if len(out) <= size {
		return out, nil, nil
	}
	out = out[:size]
	last1 := out[len(out)-1]
	return out, &RIBCursor{
		Collector: collector,
		SessionID: sid,
		Router:    f.Router,
		Peer:      f.Peer,
		RIB:       f.RIB,
		Last: []any{
			last1.RIB, last1.Local.RouterID, last1.Local.NodeKey, last1.Remote.NodeKey,
			last1.Local.InterfaceID, last1.Remote.InterfaceID,
			last1.Local.IfAddr, last1.Remote.IfAddr,
		},
	}, nil
}
