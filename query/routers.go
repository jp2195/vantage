package query

import (
	"context"
	"fmt"
)

// routersSQL joins two per-(collector, router) aggregations: router_state,
// which carries the identity peer_state holds (sysname, the current session,
// the newest event), and router_peers, which counts peers off peer_up.
//
// The counts come from peer_up (see peerUpCTE) rather than from peer_state,
// and that is the whole reason this statement has a join in it at all.
// peer_state's grain is (collector, router, peer, rib, session): one row per
// RIB VIEW of a peer, not one row per peer. Counting it directly -- which
// this statement used to do -- answers a question nobody asked. A router
// mirroring both pre- and post-policy
// adj-RIB-in contributes two peer_state rows for one peer, so an up peer is
// counted twice; and a peer whose newest event under one rib is up while
// another rib's newest is down lands in BOTH counts, so peers_up +
// peers_down can exceed the number of peers the router has. Neither failure
// raises, and both report a plausible number.
//
// peer_up is the peer-level fact introduced for exactly this
// distinction -- one row per (collector, router, peer, session), resolving
// the newest event across every rib -- and every other surface in this
// package was moved onto it. Aggregating it here means each peer is counted
// at most once, whatever the router mirrors, rather than not-quite-once by
// luck of a fixture whose peers all sit under one rib. See
// TestRoutersCountsPeersNotRibViews and insertTwoRibPeerFixture, which is
// the first fixture in this package to write a peer under two ribs at all.
//
// At MOST once, and that bound is the honest one: peers_up + peers_down
// equals the router's peer count whenever every peer's newest event
// resolved up or down, which is not guaranteed. peer_events.kind is
// Enum8('unspecified' = 0, 'up' = 1, 'down' = 2) and sink/rows.go's peerRow
// writes 'unspecified' for any PeerEvent that is neither KIND_UP nor
// KIND_DOWN, so a peer whose newest event is one of those satisfies neither
// countIf and appears in neither total. The sum is then BELOW the peer
// count. Two countIfs cannot report that peer at all, which is a real
// limitation of this shape rather than an oversight -- Router has no third
// field for it -- and the archive carries 788 up, 17 down and zero
// unspecified, so it is latent exactly the way the rib-grained count it
// replaced was. Do not restore the stronger claim without adding somewhere
// for an unresolved peer to go.
//
// INNER JOIN, not LEFT: router_state and router_peers are aggregations of
// peer_state and peer_up, which are themselves two groupings of the same
// peer_current rows against the same cur, so the two carry the identical set
// of (collector_id, router_ip) pairs. The join can neither drop a router nor
// duplicate one.
//
// any(sid) inside router_state is safe only because peer_state is already
// scoped to one session per (collector, router) -- cur groups by
// (collector_id, router_ip) and router_state's GROUP BY carries the
// identical pair, so every peer_state row in a group carries the same sid
// and any() cannot land on a different one. The premise moved when session
// identity gained collector_id; the conclusion did not, and it holds only
// while the two GROUP BYs agree. Drop either column from router_state's --
// collapsing two collectors' views into a single row is the tempting one --
// and any(sid) stops being a constant-per-group pick and becomes an
// arbitrary one: a defect no result-based test would reliably catch, since
// it would be right on roughly half of any fixture with two sessions in
// play. See TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree, which
// asserts the invariant against the SQL text rather than a result, for the
// same reason sink/dashboard_sql_test.go asserts argMax versus any the same
// way.
//
// One row per (collector, router) is not a merge and not a deduplication:
// a router two collectors both monitor appears twice, once per collector's
// own view of its current session. See Router.Collector's doc comment for
// why that is the answer rather than picking a winner between them, and
// TestRoutersDoesNotDropASecondCollectorsView for what picking a winner
// silently costs.
const routersSQL = "WITH " + peerStateCTE + peerUpCTE + `,
router_state AS (
    SELECT
        collector_id,
        router_ip,
        any(sysname)   AS sysname,
        any(sid)       AS sid,
        max(last_seen) AS last_seen
    FROM peer_state
    GROUP BY collector_id, router_ip
),
router_peers AS (
    SELECT
        collector_id,
        router_ip,
        countIf(state = 'up')        AS peers_up,
        countIf(state = 'down')      AS peers_down,
        countIf(state = 'view_lost') AS peers_view_lost,
        countIf(state = 'stale')     AS peers_stale
    FROM peer_up
    GROUP BY collector_id, router_ip
)
SELECT
    rs.sysname,
    rs.router_ip,
    rs.collector_id,
    rs.sid,
    rp.peers_up,
    rp.peers_down,
    rp.peers_view_lost,
    rp.peers_stale,
    rs.last_seen
FROM router_state rs
INNER JOIN router_peers rp
    ON rp.collector_id = rs.collector_id
   AND rp.router_ip    = rs.router_ip
ORDER BY rs.sysname, rs.collector_id`

// Routers reports one row per (collector, router), as of that pairing's
// current BMP session: how many of the router's peers are up versus down
// in the view that collector has of it, and when the most recent event in
// that session was collected. One collector per router is the supported
// deployment, so one row per router is the ordinary result; see
// Router.Collector for what a second collector produces and why it is
// extra rows rather than a merged or a truncated answer.
//
// "Down" here means the peer's session-scoped state resolved to down at the
// end of the current session, not that it went down at some point during it
// -- peerUpCTE is what makes that distinction hold; see its doc comment,
// and peerStateCTE's for why "the current session" is keyed on (collector,
// router).
//
// A peer is counted at most once, however many RIB views the router mirrors
// for it, and its one state is the newest event across all of them. PeersUp
// + PeersDown + PeersViewLost + PeersStale is therefore the router's peer
// count whenever every peer's state resolved to one of those four -- and
// less than it when one resolved 'unspecified', which counts in none. See routersSQL
// for both halves of that: why the counts needed moving off peer_state, and
// why the sum is a bound rather than an identity.
func (q *Q) Routers(ctx context.Context) ([]Router, error) {
	rows, err := q.conn.Query(ctx, fmt.Sprintf(routersSQL, q.db))
	if err != nil {
		return nil, fmt.Errorf("query routers: %w", err)
	}
	defer rows.Close()

	var out []Router
	for rows.Next() {
		var r Router
		// countIf returns UInt64, and clickhouse-go's UInt64 column scans
		// only into *uint64 (or **uint64) -- there is no narrowing case for
		// *int in its ScanRow switch. Router's fields stay plain int, the
		// natural Go type for a peer count, by scanning through these four
		// and narrowing here instead of at every caller of Routers.
		var up, down, viewLost, stale uint64
		if err := rows.Scan(
			&r.SysName, &r.IP, &r.Collector, &r.SessionID, &up, &down, &viewLost, &stale, &r.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("scan router: %w", err)
		}
		r.PeersUp, r.PeersDown, r.PeersViewLost, r.PeersStale = int(up), int(down), int(viewLost), int(stale)
		// router_ip is IPv6-typed and clickhouse-go's scan never unmaps an
		// IPv4-mapped address -- see unmapAll's own doc comment.
		unmapAll(&r.IP)
		out = append(out, r)
	}
	return out, rows.Err()
}
