// Package query owns every current-state SQL statement in vantage and the
// session semantics that make them correct. It is deliberately the only
// place those live: before it, the same session-scoped argMax was
// re-derived inside thirteen dashboard JSONs, and the two defects that
// found -- an alias shadowing a column inside WHERE, and a withdraw
// sentinel rendered as a label -- were both invisible to SQL that had no
// single owner.
//
// It speaks no HTTP, reads no flags and parses no config. cmd/vantage-api
// and `vantage query` are both thin shells over this package; a laptop
// with a ClickHouse DSN and no services running can use it directly.
//
// No FINAL anywhere in this package. Every route table is
// ReplacingMergeTree, and ClickHouse's own guarantee that a redelivered
// envelope's byte-identical rows collapse to one is a promise about
// eventually, at the next merge, not about the moment a query runs (see
// schema.sql's own ReplacingMergeTree note). FINAL would force a merge at
// query time and pay for it on every call; this package instead dedupes by
// resolving each route key's newest observation with argMax over
// (seq, stream_seq) -- see routesSQL's and vpnRoutesSQL's own doc comments
// -- so a duplicate row loses to its own newer sibling regardless of
// whether a background merge has collapsed the pair yet.
//
// argMax is not, on its own, sufficient for a count. Resolving a route
// key's newest observation still leaves two byte-identical rows sitting in
// the table until the next merge, and a naive count() over them is 2 when
// the route is 1. Peer.Routes reaches uniqExact for exactly this reason --
// see peersSQL's own doc comment on route_counts, and
// TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent, which defeats
// merges outright to keep this hazard from being silently fixed out from
// under the test that guards it.
package query

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// dbNameRe is the shape a database name must have to be interpolated into
// the statements in this package. ClickHouse has no placeholder for an
// identifier, so the name cannot be bound as a parameter the way every
// other value here is. It is validated once, at construction, rather than
// trusted at each call site -- the same bargain sink.NewClickHouse makes,
// for the same reason.
var dbNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Q struct {
	// conn is raw wrapped in liveConn: every statement this package runs
	// goes through it, which is what fills in the stale threshold (see
	// liveness.go). Fixtures a test writes through q.conn pass through it
	// unchanged.
	conn driver.Conn
	raw  driver.Conn
	db   string
	// staleAfter is the threshold conn fills in.
	staleAfter time.Duration
}

// New wraps an already-dialed connection. It does not dial one itself:
// dialing means a DSN, a DSN means a secret, and keeping this package free
// of both is what lets it be tested against a throwaway database without
// any of sink's credential machinery.
//
// A nil conn is rejected here rather than left to panic on the first query
// a caller makes: every method on *Q calls q.conn.Query or q.conn.QueryRow
// unconditionally, so a nil conn would fail loudly, but only once someone
// actually asks this Q a question -- and with a stack trace that points at
// whichever query happened to run first, not at the construction site that
// actually got it wrong.
func New(conn driver.Conn, db string) (*Q, error) {
	if conn == nil {
		return nil, errors.New("query: conn is nil")
	}
	if !dbNameRe.MatchString(db) {
		return nil, fmt.Errorf("query: database name %q is not a plain identifier", db)
	}
	return newQ(conn, db, DefaultStaleAfter), nil
}

// peerStateCTE -- the single definition of "the current session", shared by
// every statement in this package.
//
// It is a package-level const rather than repeated per file because the two
// ways to get this wrong are both invisible in a passing test: scoping to a
// stale session reports a reconnected router's old dump as live, and scoping
// per-peer instead of per-router splits one BMP connection into several. One
// definition, exercised by every surface, is the cheapest guard against both.
//
// "The current session" is keyed on (collector_id, router_ip), never on
// router_ip alone, and that is a correctness matter rather than
// bookkeeping. session_id is now().UnixNano(), assigned independently by
// each collector process -- collector.Server.nextSessionID is monotonic
// within one process against that process's own clock and shares nothing
// with any other -- so max(session_id) taken across collectors resolves a
// router to whichever collector's clock is momentarily ahead and silently
// discards every peer, route and marker the other one ever recorded for it.
// No error, no empty result: just a smaller fleet, reported with total
// confidence. One collector per router is the supported deployment (see
// Router.Collector), but "unsupported" must not mean "answers wrongly and
// says nothing about it", so a second collector yields a second row here
// rather than a merged answer or a vanished one.
//
// The same key belongs in the join predicate, not only in cur's GROUP BY.
// p.collector_id = cur.collector_id is what stops one collector's
// superseded session joining a DIFFERENT collector's current cur row when
// the two session_ids happen to be equal -- and nothing makes them unequal:
// two collectors are two processes with two clocks and no shared counter,
// so a shared session_id space is an assumption no code enforces. Without
// that predicate, the dead session's peers reappear inside the live
// collector's answer, up and current-looking. See
// insertTwoCollectorFixture, whose third row is exactly that shape, and
// TestRoutersDoesNotDropASecondCollectorsView, which fails on both halves
// of this scoping.
//
// A third way sits one level down, inside peer_state's own GROUP BY rather
// than cur's: peer_state groups by rib as well as router and peer, which is
// right for peer_state's own purpose (each rib has its own dump-progress
// question -- see dumpStateExpr) but wrong for "is this peer's BGP session
// up." rib is a route-monitoring dimension -- which view of the RIB a route
// came from -- not a property of the session itself, and a caller that joins
// route rows to peer_state ON rib to decide whether to report them silently
// drops every route observed under a rib peer_events has no accounting for
// at all: the live archive carries zero peer_events rows for rib = 'in_post'
// (804 for in_pre, 1 for loc_rib) against 3 route_unicast rows that are
// in_post, so those 3 routes find no peer_state row to join against and
// vanish -- not because the peer is down (the same session's in_pre row
// shows it up), but because peer_state was asked the wrong question about
// the wrong dimension. That specific occurrence is invisible today only
// because the session it sits in has since been superseded; the join
// predicate itself is one-directional and can only ever drop a route, never
// save one, so it was only a matter of time before a post-policy route
// landed in a router's current session instead.
//
// peerUpCTE (below) is the peer-level fact routesSQL, vpnRoutesSQL and
// peersSQL's own route_counts join test route liveness against instead of
// peer_state, for exactly this reason. It is a second, narrower CTE beside
// peer_state -- not a re-key of it: peer_state itself, and Peers' own
// one-row-per-(router, peer, rib) output, are unchanged. One consequence of
// keeping the two separate is worth naming rather than discovering later: a
// peer_state row's own State can, in principle, disagree with what peer_up
// resolves for the same peer, if that peer's most recent session event
// landed under a different rib than the one a given peer_state row
// represents (a peer_state row reflects only ITS rib's newest event; peer_up
// reflects the newest event across every rib the peer has any). The live
// archive never exercises this -- every peer's peer_events sit under exactly
// one rib -- but a caller reading both Peer.State and a route surface's
// answer for the same peer should not assume they are always resolving the
// identical question.
//
// %[1]s is the database name, filled by a single Sprintf argument at each
// call site -- the indexed verb is what lets this const be concatenated into
// a longer statement that also references the database.
//
// peer_state's own column list deliberately never aliases a computed
// expression back to the name of the real peer_events column it was
// computed from: state, sysname and last_seen already diverge from their
// source columns (kind, router_sysname, ts_collector) for that reason, and
// sid and asn are named that way rather than session_id/peer_asn for the
// same one. ClickHouse resolves a SELECT alias inside that same query's
// WHERE, so a later WHERE naming the same identifier as a SELECT alias
// silently binds to the alias instead of the real column: `cur.sid AS
// session_id` sitting a few lines above `p.session_id = cur.sid` would be
// one added WHERE away from doing exactly that to peer_events' real
// column. Nothing in this CTE has a WHERE today, which is exactly why a
// collision here can sit unnoticed until a WHERE clause is added later.
//
// It reads peer_current, not peer_events: peer_current is fed by a
// materialized view over peer_events but carries no TTL of its own, so a
// session's state stays resolvable here long after its individual events
// have aged out of peer_events' retention window.
//
// Collector liveness is applied here, once, for every statement that
// composes these CTEs: live (liveCTE, liveness.go) gives each collector its
// epoch and its last heartbeat, cur carries both beside each current session,
// and peer_state's and peer_up's state resolve the stored kind through
// liveStateSQL. A session older than its collector's newest process start
// reads view_lost; a stored up from a collector unheard for the stale
// threshold reads stale. stored_state keeps the Enum8 kind for anything that
// needs the router's own word.
const peerStateCTE = liveCTE + `,
cur AS (
    SELECT c.collector_id                    AS collector_id,
           c.router_ip                       AS router_ip,
           c.sid                             AS sid,
           toUnixTimestamp64Nano(l.epoch_at) AS epoch_ns,
           l.last_beat                       AS last_beat
    FROM (
        SELECT collector_id, router_ip, max(session_id) AS sid
        FROM %[1]s.peer_current
        GROUP BY collector_id, router_ip
    ) c
    INNER JOIN live l ON l.collector_id = c.collector_id
),
peer_state AS (
    SELECT
        p.collector_id                            AS collector_id,
        p.router_ip                               AS router_ip,
        p.peer_ip                                 AS peer_ip,
        p.rib                                      AS rib,
        cur.sid                                    AS sid,
        argMax(p.kind,           (p.seq, p.stream_seq)) AS stored_state,
        ` + liveStateSQL + ` AS state,
        argMax(p.peer_asn,       (p.seq, p.stream_seq)) AS asn,
        argMax(p.router_sysname, (p.seq, p.stream_seq)) AS sysname,
        max(p.ts_collector)                             AS last_seen,
        -- The session facts, from the newest event in this session that
        -- ACTUALLY CARRIED THEM -- argMaxIf, not argMax. A Peer Down carries
        -- no OPEN message, so a peer that came up and later flapped has its
        -- hold time and capability lists on the older row; a plain argMax
        -- would answer with the down row's empty values and report a session
        -- that negotiated nothing, which on screen is indistinguishable from
        -- one that really did.
        --
        -- max(hold_time_seen) rather than argMax for the flag itself: the
        -- question it answers is "did this session ever tell us", which is a
        -- property of the whole session and not of its newest event.
        argMaxIf(p.hold_time, (p.seq, p.stream_seq), p.hold_time_seen = 1)        AS hold_time,
        max(p.hold_time_seen)                                                     AS hold_time_seen,
        argMaxIf(p.mp_families, (p.seq, p.stream_seq), p.hold_time_seen = 1)      AS mp_families,
        argMaxIf(p.addpath_families, (p.seq, p.stream_seq), p.hold_time_seen = 1) AS addpath_families,
        -- sys_descr is gated on being non-empty rather than on hold_time_seen:
        -- it rides the Initiation message, not the OPEN, so a router that
        -- sent one and a peer that sent no OPEN are independent facts.
        argMaxIf(p.sys_descr, (p.seq, p.stream_seq), p.sys_descr != '')           AS sys_descr,
        -- When this peer's session last came UP, and whether it ever did.
        --
        -- Ordered on (seq, stream_seq), the key stored_state is resolved on
        -- at the top of this SELECT, so the two cannot disagree: a max() over
        -- ts_collector would rank on a different key than the state sitting
        -- beside it in the same row, and a collector whose clock stepped
        -- would report a state from one event and an up-time from another.
        --
        -- The LATEST up, not the first. A peer can flap many times inside one
        -- BMP session -- the session is the router's, the up and down are the
        -- BGP peer's -- so the first up of a session is not when the current
        -- one began.
        --
        -- up_seen is what separates a session that came up at the epoch, which
        -- no collector clock reports, from one that never came up at all:
        -- argMaxIf over no matching row returns the zero instant, and 1970 on
        -- screen is an absence wearing a value. It is the same distinction
        -- hold_time_seen draws above, for the same reason.
        argMaxIf(p.ts_collector, (p.seq, p.stream_seq), p.kind = 'up')            AS up_since,
        maxIf(1, p.kind = 'up')                                                   AS up_seen
    FROM %[1]s.peer_current p
    INNER JOIN cur
        ON p.collector_id = cur.collector_id
       AND p.router_ip    = cur.router_ip
       AND p.session_id   = cur.sid
    GROUP BY p.collector_id, p.router_ip, p.peer_ip, p.rib, cur.sid, cur.epoch_ns, cur.last_beat
)`

// peerUpCTE resolves each (router, peer)'s current-session BGP state --
// independent of rib -- the peer-level fact routesSQL, vpnRoutesSQL and
// peersSQL's own route_counts join test route liveness against, rather than
// peer_state's own per-(router, peer, rib) view. See peerStateCTE's own doc
// comment for the fuller account of why joining a route to peer_state ON rib
// is the wrong question to ask: a route observed under a rib peer_events has
// no accounting for at all finds no peer_state row to join against, and a
// rib-scoped join drops it regardless of whether the peer's actual BGP
// session is up.
//
// It shares cur with peer_state rather than defining a second one: every
// caller of peerUpCTE has already concatenated peerStateCTE first (cur is
// defined there), and a second, independent "current session" CTE would be
// two definitions of the same fact with no guarantee they ever agree --
// exactly the hazard peerStateCTE's own doc comment already warns against,
// for the identical "one definition, exercised by every surface" reason.
//
// argMax(p.kind, (p.seq, p.stream_seq)) orders the same way peer_state's own
// argMax(p.kind, ...) does (never ts_router, a router's own untrustworthy
// clock; never ts_collector, batch-granular -- see peerStateCTE's doc
// comment), just without the GROUP BY rib that turns "the peer's session"
// into "this one rib's view of it": peer_up's GROUP BY is
// (collector_id, router_ip, peer_ip, cur.sid) only, so it resolves the
// newest event across every rib the peer has ever recorded one under in its
// current session, not just one. collector_id is there for the reason
// peerStateCTE's own doc comment gives -- session identity is (collector,
// router) -- and peer_up joins cur on it too, so a route gated on peer_up
// is gated on the peer state ITS OWN collector observed.
//
// peer_up carries no sysname, asn or last_seen: nothing that joins to it
// needs them, and peer_state's own row (unchanged by this CTE) already
// carries them for a caller that does.
//
// It reads peer_current, not peer_events, for the same reason peerStateCTE
// does: peer_current has no TTL, so peer liveness stays resolvable past the
// point where peer_events' own retention would have expired the rows it is
// built from.
const peerUpCTE = `,
peer_up AS (
    SELECT
        p.collector_id                         AS collector_id,
        p.router_ip                            AS router_ip,
        p.peer_ip                              AS peer_ip,
        cur.sid                                AS sid,
        argMax(p.kind, (p.seq, p.stream_seq))  AS stored_state,
        ` + liveStateSQL + ` AS state
    FROM %[1]s.peer_current p
    INNER JOIN cur
        ON p.collector_id = cur.collector_id
       AND p.router_ip    = cur.router_ip
       AND p.session_id   = cur.sid
    GROUP BY p.collector_id, p.router_ip, p.peer_ip, cur.sid, cur.epoch_ns, cur.last_beat
)`

// eorCTE is the existence-shaped view of eor_events every route statement
// LEFT-joins to decide DumpState: one row per (collector, router, peer, rib,
// session, family) that has a marker at all, carrying a constant marked = 1.
//
// It is a package-level const shared by routesSQL, vpnRoutesSQL and
// evpnRoutesSQL rather than three copies, for the reason peerStateCTE's own
// doc comment gives about "the current session": the two ways to get this
// wrong are both silent, and one definition exercised by three surfaces is
// the cheapest guard against either drifting.
//
// marked is a constant 1 and the CTE groups rather than counts. eor_events
// is a plain MergeTree with NO deduplication (see its own schema.sql
// comment), so a JetStream redelivery leaves a second, permanent marker row
// for the same key -- insertPerFamilyDumpFixture writes exactly that pair on
// purpose. Because nothing here counts, two markers and one marker are the
// same answer: anything able to tell them apart would be reading a
// collection artifact as a fact about the network, this project's most
// recurring defect, reintroduced in the very table split out to end it.
// TestEVPNRoutesTreatsARedeliveredMarkerAsOneMarker holds that directly --
// turning this constant into count() makes it fail.
//
// The GROUP BY beside it collapses the redelivered pair to one row, so a
// LEFT JOIN to this CTE cannot fan one route out into two. That is a second,
// independent guard rather than the load-bearing one, and it is worth being
// precise about which: all three callers aggregate to their own route key
// afterwards and reach these columns through any(), so a duplicated joined
// row is absorbed before it can change an answer. Deleting this GROUP BY
// leaves the suite green -- verified -- which is exactly why it is argued
// here rather than left for a test to defend. It stops being defense in
// depth the moment a caller reads eor without an aggregation of its own.
//
// It carries no WHERE at all, not even a router predicate, though every
// caller's filter can now name one. Pushing the filter down would make this
// CTE's shape depend on which fields a caller happened to set, and it is
// LEFT-joined on the route row's own (collector, router, peer, rib, session,
// family) -- a marker for a router the outer WHERE has already excluded has
// nothing to join to. Narrowing it would be a read-volume optimization and
// nothing else, and it is not one anybody has measured here; peersSQL's own
// doc comment accepts the same trade for the same reason.
//
// family is aliased fam, not family, for the alias-shadowing reason
// peerStateCTE's doc comment gives at length: ClickHouse resolves a SELECT
// alias inside that same query's WHERE, so a column aliased back to its own
// source name leaves any later predicate one rename away from binding to the
// wrong thing.
//
// The join predicate that pins fam is the caller's, and it differs by table:
// routesSQL and vpnRoutesSQL both write `eor.fam = r.family`, because a
// route's DumpState is its OWN family's dump progress and an EVPN marker
// must not tell an ipv4u route its dump is finished. route_evpn has no
// family column at all -- an EVPN table holds exactly one family -- so
// evpnRoutesSQL binds the token instead; see evpnFamily.
//
// It reads eor_current, not eor_events: eor_current has no TTL, so a
// session's dump-completion markers stay resolvable here after eor_events'
// own retention window would have expired them.
const eorCTE = `,
eor AS (
    SELECT collector_id, router_ip, peer_ip, rib, session_id,
           family        AS fam,
           toUInt8(1)    AS marked
    FROM %[1]s.eor_current
    GROUP BY collector_id, router_ip, peer_ip, rib, session_id, family
)`

// dumpStateExpr is this package's definition of a single route row's
// DumpState: the initial-dump progress of THAT ROW'S OWN FAMILY, for the
// (router, peer, rib) that advertised it, collapsed to the same three
// strings routesSQL surfaces as Route.DumpState. It is a package-level
// const, not copied per file, because the one subtlety in it -- the
// down-peer check running FIRST, so a peer that finished dumping earlier in
// its session and then dropped reads "unknown" rather than "complete" -- is
// easy to get backwards, and getting it backwards passes every test that
// does not specifically manufacture a down peer with a complete dump on
// record (see TestPeersReportsDumpStateForADownPeerWithACompleteDump, which
// asserts the same ordering on the map dumpStatesExpr builds). A second,
// independently-typed copy of this expression is a second chance to get
// that ordering wrong without either copy's tests catching it.
//
// It assumes its caller's query has peer_state (peerStateCTE) and an eor
// CTE in scope, joined LEFT the way routesSQL joins it: eor keyed on
// (collector_id, router_ip, peer_ip, rib, session_id, fam) with
// marked = 1, matched to the route row on the route's own collector, its
// own session and its own family. collector_id leads that key because
// session identity does (see peerStateCTE); a caller that follows this
// contract without it joins a route to another collector's marker. eor's
// marked column is a constant 1 carried by an existence-shaped CTE, not a
// tally: eor_events is a plain MergeTree with no deduplication, so a
// JetStream redelivery leaves a second, permanent marker row for the same
// (router, peer, rib, session, family), and any expression here that could
// tell two markers from one would be reading a collection artifact as a
// fact about the network -- this project's single most recurring defect,
// reintroduced in the very table built to end it. "Did a marker arrive"
// is the only question, and eor answers it with a GROUP BY rather than a
// count.
//
// There is no "the family has no route rows" branch here, unlike
// dumpStatesExpr's map: the row this expression decorates IS a route row
// for that family, so the family demonstrably has rows and "dumping" is the
// honest reading of a missing marker. "unknown" survives only for the
// peer-level cases -- the peer is down, or peer_state has no row for this
// route's rib at all.
//
// coalesce(peer_state.state, 'unspecified'), not a bare peer_state.state:
// routesSQL LEFT JOINs peer_state (see routesSQL's own doc comment on why
// route survival there is gated by peer_up, not peer_state), so a route under
// a rib that peer_state has no data for at all still reaches this
// expression, with peer_state.state unmatched. state is a String (see
// liveStateSQL), so under this server's usual join_use_nulls = 0 the
// unmatched value is the empty string and resolves to 'unknown' below; under
// join_use_nulls = 1 it would be NULL, and the coalesce is what keeps it
// resolving the same way.
//
// 'stale' passes with 'up'. A stale peer's routes are served (see
// servedGate), and their dump progress is what it was at the collector's last
// heartbeat -- not "unknown", which reads as a peer that is disconnected.
const dumpStateExpr = `multiIf(
    coalesce(peer_state.state, 'unspecified') NOT IN ('up', 'stale'), 'unknown',
    coalesce(eor.marked, 0) = 1,                       'complete',
    'dumping'
)`

// dumpStatesExpr is Peer.DumpStates: every family this peer's current
// session has any evidence for, mapped to that family's own dump progress.
// It replaces the single unicast-scoped string dumpStateExpr used to
// produce for peersSQL, which could only ever answer the question for
// route_unicast -- end_of_rib lived on that table alone, so a peer carrying
// nothing but VPN or EVPN routes read "unknown" forever no matter how
// complete its dump was. eor_events carries a family column, so the
// question is now askable per family and this expression asks it.
//
// It assumes its caller has peer_state (peerStateCTE) and a dump_map CTE in
// scope, LEFT-joined on (collector_id, router_ip, peer_ip, rib,
// session_id) -- collector_id leads for the reason peerStateCTE's doc
// comment gives, and a caller that drops it from this join hands one
// collector's peer another collector's dump progress -- whose states
// column is already a Map(String, String) of family -> "complete" |
// "dumping". Building that map is dump_map's job rather than this const's
// because the aggregation it needs (two levels: families first, then the
// map over them) does not fit in a scalar expression; what belongs here,
// and the only reason this is a shared const at all, is the ordering below.
//
// The down-peer check runs FIRST, before any family logic, for the reason
// dumpStateExpr's own doc comment gives and
// TestPeersReportsDumpStateForADownPeerWithACompleteDump exists to hold: a
// peer that finished its dump earlier in the session and later went down is
// not "still dumping", and it is not "complete" either -- it is
// disconnected, and its dump progress is not a meaningful question. The map
// is empty for such a peer, which is what api/openapi.yaml's dump_states
// schema documents ("Empty object for a down peer").
//
// 'stale' passes with 'up'. A stale peer's routes are served (see
// servedGate), and their dump progress is what it was at the collector's last
// heartbeat -- not "unknown", which reads as a peer that is disconnected.
//
// The empty map is written mapFromArrays(emptyArrayString(),
// emptyArrayString()) rather than a bare map() literal so that both arms of
// the if() have the identical Map(String, String) type; an untyped empty
// map would leave the branch types to be unified at parse time, which is a
// worse thing to depend on than two extra function calls.
//
// dump_map.states is coalesced against that same empty map, for symmetry
// with peer_state.state above rather than because a measurement showed it
// was needed: a peer whose current session carried no route and no marker
// for any family has no dump_map row, and the unmatched Map column comes
// back as the empty map either way.
//
// It is worth recording what was actually checked, because the obvious
// hazard here turns out not to exist and the next reader should not have to
// re-derive that. ClickHouse cannot put a Map inside a Nullable at all --
// "Nested type Map(String, String) cannot be inside Nullable type" -- so
// unlike peer_state.state, this column cannot become Nullable no matter how
// join_use_nulls is set. Verified on 24.8.14.39 against this package's own
// test database: with join_use_nulls = 1 and with it 0, toTypeName of this
// column is Map(String, String) both times, an unmatched row reads {} both
// times, and the query runs identically with the coalesce and without it.
//
// So this coalesce buys no behavior today. It is here because the
// alternative -- one of two adjacent, identically-shaped LEFT JOIN reads
// guarded and the other bare -- makes a reader work out which of the two
// can go null and why, every time either line is touched. See peersSQL's
// own doc comment for the same bargain applied to route_counts.n, where the
// guard is not a no-op.
//
// A family is ABSENT from the map, not present as "unknown": a session that
// carried no rows at all for a family has no dump to be part-way through,
// and an entry claiming otherwise would be manufacturing a fact about a
// family the router never mentioned. That is why the map's value type has
// only two states where Route.DumpState has three.
const dumpStatesExpr = `if(
    coalesce(peer_state.state, 'unspecified') NOT IN ('up', 'stale'),
    mapFromArrays(emptyArrayString(), emptyArrayString()),
    coalesce(dump_map.states, mapFromArrays(emptyArrayString(), emptyArrayString()))
)`
