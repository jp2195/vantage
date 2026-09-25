package query

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// actionAnnounce and actionWithdraw are api/openapi.yaml's own
// HistoryEvent.action enum, and they are derived in Go from route_unicast's
// is_withdraw column rather than rendered in SQL.
//
// Doing it in Go is not a style preference. The SQL alternative -- an
// `if(is_withdraw, 'withdraw', 'announce') AS action` in the SELECT -- would
// put a computed expression next to the real column it was computed from,
// and ClickHouse resolves a SELECT alias inside that same query's WHERE and
// ORDER BY; this package has already paid for that once (see peerStateCTE's
// doc comment on the l3vpn-rib-browser rd/vrf defect). A future filter on
// is_withdraw -- "show me only the withdrawals for this prefix" is an
// obvious next parameter -- would then be one rename away from binding to a
// String alias instead of the UInt8 column. Mapping in Go leaves the SELECT
// list made of nothing but real columns, so there is no alias for anything
// to shadow.
//
// The two spellings are the contract's, exactly. Nothing downstream is
// expected to translate them -- cmd/vantage-api, when it exists, will
// serialize Action as it finds it -- so a typo here is a contract violation
// that no test of the HTTP layer would catch either. TestRouteHistory
// asserts the literal "withdraw" rather than this const for that reason: the
// const and the contract are two things that have to agree, and a test
// written against the const alone cannot tell them apart.
const (
	actionAnnounce = "announce"
	actionWithdraw = "withdraw"
)

// HistoryEvent is one row of RouteHistory: a single route_unicast
// observation, reported as the event it was rather than folded into a
// current-state answer. It matches api/openapi.yaml's HistoryEvent schema
// field for field.
//
// It lives here rather than in types.go, alongside Router, Peer, Route,
// VPNRoute and EVPNRoute, and the placement is deliberate: every type in
// that file describes something that is TRUE NOW, and this one describes
// something that HAPPENED. A reader who finds them in one list will
// reasonably assume they share the session scoping, the down-peer exclusion
// and the withdrawal exclusion that the other five do. They do not; see
// RouteHistory.
//
// TsCollector and TsRouter are two different claims and callers need both to
// tell them apart. TsCollector is when the collector received the message and
// is the trustworthy one: it is a clock this fleet runs, and can fix.
// TsRouter is what the ROUTER said, and a
// router with a dead clock reports 1970; the contract exposes it because it
// is data the operator may want to see (a fleet-wide clock skew is a real
// finding), not because anything should order or filter on it. RouteHistory
// orders by TsCollector for exactly that reason.
//
// Seq is the collector-assigned per-(peer, session) sequence and is the
// authoritative order WITHIN one session; it restarts at each new session, so
// it is a tiebreaker under TsCollector rather than a fleet-wide ordering of
// its own. api/openapi.yaml types it as a string (a u64 does not survive
// JSON's number type intact); it is a uint64 here, and rendering it is the
// HTTP layer's job.
//
// There is no StreamSeq field, though RouteHistory's ORDER BY reaches for
// stream_seq as its last tiebreaker: it is the JetStream stream sequence of
// the message that carried the event (see sink.RowsFor), a fact about the
// transport rather than about the network, and api/openapi.yaml does not put
// it in HistoryEvent. Adding it would be this repo's most recurring defect in
// its mildest form -- a collection artifact surfaced as if it described BGP.
//
// There is no DumpState field either, unlike Route and VPNRoute. DumpState
// answers "is this peer's initial table dump still arriving, so is an empty
// answer really an empty answer" -- a question about a CURRENT view. An event
// that happened is not made more or less true by a dump that is still in
// progress, so there is nothing for this type to report and no eor join for
// RouteHistory to carry.
//
// NextHop is the zero netip.Addr for a withdrawal, and that is the honest
// value rather than a gap: a BGP withdrawal names an NLRI and carries no path
// attributes at all, so there is no next hop to report. api/openapi.yaml types
// next_hop as `[string, "null"]` for the same reason; the HTTP layer renders
// an invalid Addr as null. An announcement whose next hop failed to parse also
// lands here as the zero Addr -- see RouteHistory's scan loop for why that is
// not a query-wide error.
//
// RouterIP, PeerIP and NextHop are always in plain form -- "10.0.103.61",
// never "::ffff:10.0.103.61" -- the same guarantee every other type in this
// package makes; see unmapAll.
type HistoryEvent struct {
	TsCollector time.Time
	TsRouter    time.Time
	Seq         uint64
	SessionID   uint64
	RouterIP    netip.Addr
	PeerIP      netip.Addr
	Collector   string
	RIB         string
	Family      string
	Prefix      string
	PathID      uint32
	Action      string
	NextHop     netip.Addr
	ASPath      []uint32
}

// HistoryFilter is the narrowing RouteHistory accepts: api/openapi.yaml's own
// parameters on /v1/routes/history (?prefix=, ?since=, ?router=, ?peer=,
// ?rib=).
//
// It is a fourth filter struct rather than a reuse of RouteFilter because the
// dimensions genuinely differ in both directions, which is the same test
// RouteFilter, VPNRouteFilter and EVPNRouteFilter were separated under (see
// RouteFilter's doc comment). Since exists on no other endpoint -- every other
// surface answers "now", and a time bound on "now" is meaningless. Covers and
// Family exist here on neither: /v1/routes/history documents neither
// parameter, and a filter field this package accepted and ignored is exactly
// what those three structs were kept apart to prevent.
type HistoryFilter struct {
	// Prefix is an exact match on route_unicast's own prefix column, and it
	// is REQUIRED: an empty Prefix is refused by check rather than read as
	// "every prefix" (VPNRouteFilter's reading) or as "the empty prefix"
	// (RouteFilter's). An unbounded event dump is not a question this
	// endpoint answers -- there is no cursor, no page limit and no session
	// pin here, and route_unicast holds 90 days of every announcement and
	// withdrawal the fleet produced.
	//
	// Its predicate is rendered as a bare equality against the column, never
	// wrapped in a function, because idx_prefix -- a bloom filter on prefix
	// (see schema.sql) -- is what makes this query cheap at all. The table's
	// PRIMARY KEY leads with router_ip/peer_ip/rib, so a point lookup for one
	// prefix across the fleet gets no help from it; the skip index is the
	// whole story, and a `lower(prefix) = ?` or a `prefix LIKE ?` would
	// silently give it up and scan every granule.
	Prefix string

	// Since bounds the timeline below, INCLUSIVELY, against ts_collector --
	// never ts_router, for the reason HistoryEvent.TsRouter gives.
	//
	// The zero time means NO BOUND AT ALL, and that is a deliberate reading
	// rather than an accident of Go's zero value. api/openapi.yaml defaults
	// ?since= to "1h" and accepts either an RFC 3339 timestamp or a relative
	// duration ("1h", "30m"), so an HTTP caller always arrives here with a
	// real instant: parsing that string is cmd/vantage-api's job, and this
	// package takes a time.Time precisely so it never has to decide what
	// "now" is. The zero value is therefore reachable only from Go -- a
	// `vantage query` invocation or a test -- where "the whole timeline this
	// prefix has" is a question worth being able to ask directly, and where
	// substituting a silent one-hour default would answer a DIFFERENT
	// question than the caller wrote.
	//
	// What bounds the answer in that case is Prefix, which is required, and
	// route_unicast's own 90-day TTL. Neither is a page limit; a caller that
	// wants one bounds it with Since.
	Since time.Time

	// Router, Peer and RIB mean exactly what RouteFilter's do, and are each
	// omitted from the statement entirely when unset (see eqAddr and
	// eqNonEmpty). "Unset" is not a value they compare against.
	//
	// Router and Peer are NOT the session scoping the rest of this package
	// applies -- they narrow which router's or peer's view of the prefix is
	// reported, and narrow nothing about which SESSION. See RouteHistory.
	Router netip.Addr
	Peer   netip.Addr
	RIB    string
}

// predicates renders hf against historySQL's own `r` alias, the same alias
// RouteFilter.predicates, VPNRouteFilter.predicates and
// EVPNRouteFilter.predicates hard-code against their own statements and for
// the same reason (see RouteFilter.predicates): the alias is a property of
// the statement, not of the caller, so a query that aliased its FROM
// differently would be silently unfiltered.
//
// The order the predicates are added in is the order their placeholders bind.
// historySQL splices this whole rendering in at a single point and has no
// placeholder of its own, so that is trivially true here.
//
// Prefix is added with eq rather than eqNonEmpty, so it renders
// unconditionally -- but unlike RouteFilter's, an empty one never reaches
// this method: check refuses it first. The unconditional form is still the
// right one, because a future eqNonEmpty here would turn a forgotten check
// into a fleet-wide dump rather than an error.
func (hf HistoryFilter) predicates() *filters {
	var f filters
	f.eq("r.prefix", hf.Prefix)
	f.since("r.ts_collector", hf.Since)
	f.eqAddr("r.router_ip", hf.Router)
	f.eqAddr("r.peer_ip", hf.Peer)
	f.eqNonEmpty("r.rib", hf.RIB)
	return &f
}

// check enforces the one rule the statement cannot: a Prefix is required.
//
// It wraps ErrBadFilter, the package's existing sentinel, rather than
// introducing a second one. That is what lets cmd/vantage-api answer 400
// rather than 500 for this the same way it already does for an unknown family
// or an unfiltered VPN dump, without matching on message text -- and a second
// sentinel would mean every caller had to learn which surface used which.
//
// Nothing else is checked here, and each omission has a reason. rib is an
// Enum8: an unknown member makes ClickHouse itself raise, which is the loud
// failure this package wants (see eqNonEmpty, and
// TestRoutesRejectsARibThatIsNotAnEnumMember). Router and Peer are already
// netip.Addr, so there is no text to reject. Since needs no check at all: the
// zero time is a meaningful value here (see HistoryFilter.Since), and a
// future Since is simply a timeline with nothing in it yet -- an empty answer
// that is also the correct one.
func (hf HistoryFilter) check() error {
	if hf.Prefix == "" {
		return fmt.Errorf("%w: RouteHistory needs a Prefix -- /v1/routes/history "+
			"documents prefix= as required, and an unbounded event dump is not a "+
			"question this surface answers: it has no cursor, no page limit and no "+
			"session pin, and route_unicast holds every announcement and withdrawal "+
			"the fleet produced over the history retention (90 days by default)", ErrBadFilter)
	}
	return nil
}

// historySQL is a plain SELECT over route_unicast, and the absences in it are
// the design. There is no peerStateCTE, no peer_up join, no eor join, no
// argMax, no GROUP BY at route-key grain and no HAVING -- see RouteHistory's
// own doc comment before adding any of them.
//
// Every column in the SELECT list is a real route_unicast column, and nothing
// is aliased. is_withdraw is scanned raw and mapped to the contract's action
// enum in Go (see actionAnnounce), which is what leaves no alias for a later
// WHERE or ORDER BY clause to bind to by mistake.
//
// ORDER BY is (ts_collector, seq, stream_seq) DESC, and each of the three is
// load-bearing:
//
//   - ts_collector leads because it is the only trustworthy clock in the row.
//     ts_router is what the router claimed, and a router with a dead clock
//     reports 1970 -- ordering a timeline by it would put today's withdrawal
//     at the bottom and read as "this prefix has been up since the Nixon
//     administration". The same reasoning is why peerStateCTE's argMax orders
//     on (seq, stream_seq) and never on ts_router.
//   - seq breaks a ts_collector tie because ts_collector is batch-granular:
//     every event the collector drained in one batch can carry the identical
//     microsecond, and seq is the collector's own per-(peer, session) counter,
//     which is the authoritative order within a session.
//   - stream_seq breaks a seq tie because seq RESTARTS at each new session.
//     Two events from two different sessions can share both a ts_collector
//     and a seq, and stream_seq -- the JetStream stream sequence, monotonic
//     across the whole stream -- is what puts them in a stable order. Without
//     it the two rows come back in whatever order the merge happened to
//     produce, which is not wrong on any single run and is not reproducible
//     across runs either.
//
// DISTINCT is not current-state deduplication and must not be mistaken for
// it. route_unicast is a ReplacingMergeTree whose sort key ends in
// (..., stream_seq, prefix, path_id, is_withdraw), and stream_seq is the
// stream sequence of the message that carried the event -- so an at-least-once
// JetStream redelivery of one envelope leaves TWO byte-identical rows in the
// table until the next merge collapses them (schema.sql says exactly this at
// the column, and insertDuplicateRouteFixture manufactures it on purpose for
// Peers). Without DISTINCT, a redelivered announcement appears in this
// timeline as two announcements at the identical microsecond: a collection
// artifact reported as a fact about the network, which is this project's most
// recurring defect. With it, the answer is the same before and after a merge,
// which is precisely what argMax buys the other surfaces and what this one
// cannot use argMax to get.
//
// The distinction from argMax is exact rather than rhetorical, and it is what
// keeps this from being the harmonization RouteHistory's doc comment warns
// against. DISTINCT collapses rows that are equal in EVERY column this query
// reads, stream_seq included -- so its key is a strict SUPERSET of the
// engine's own sort key, and it can therefore only ever collapse a pair
// ReplacingMergeTree will itself collapse. argMax collapses rows that are
// merely equal on the ROUTE KEY, which is how an announcement and the
// withdrawal that followed it become one row. The first removes a duplicate
// of one event; the second removes the event.
//
// stream_seq is in the SELECT list only because DISTINCT and ORDER BY have to
// agree on it: ClickHouse applies DISTINCT before ORDER BY, so a tiebreaker
// the projection had already dropped would be ordering on a column that no
// longer exists at that point. It is scanned and discarded (see
// RouteHistory's scan loop) rather than surfaced on HistoryEvent -- see that
// type's own doc comment for why.
//
// %[1]s is the database name and %[2]s the whole filter rendering, spliced at
// one point. It is clause() rather than where(), because this statement has
// no predicate of its own to hang an AND on -- and it is never empty in
// practice, since HistoryFilter.Prefix is required and renders
// unconditionally.
const historySQL = `
SELECT DISTINCT
    r.ts_collector, r.ts_router, r.seq, r.session_id,
    r.router_ip, r.peer_ip, r.collector_id,
    r.rib, r.family, r.prefix, r.path_id, r.is_withdraw,
    r.next_hop, r.as_path, r.stream_seq
FROM %[1]s.route_unicast r
%[2]s
ORDER BY r.ts_collector DESC, r.seq DESC, r.stream_seq DESC`

// RouteHistory reports the event timeline for one exact prefix, newest first:
// every announcement and every withdrawal route_unicast holds for it, in the
// order the collector received them.
//
// IT IS THE ONE SURFACE IN THIS PACKAGE THAT IS DELIBERATELY NOT
// CURRENT-STATE, AND THE INVERSION IS THE ENTIRE POINT. Every other function
// here -- Routers, Peers, Routes, VPNRoutes, EVPNRoutes -- excludes
// withdrawals, scopes to the router's current BMP session, and drops a down
// peer's routes, because each of them answers "what is true right now". A
// prefix's HISTORY is made of exactly the rows those three rules throw away:
// a timeline with the withdrawals filtered out is not a shorter timeline, it
// is a false one, and a timeline that stops at the current session ends
// wherever the router last reconnected rather than wherever the operator
// asked it to.
//
// So, concretely, and stated as prohibitions because a future reader will
// arrive here expecting consistency with the file next door and will be
// tempted to supply it:
//
//   - NO peerStateCTE and NO session scoping. An event from a session the
//     router has since replaced is INCLUDED. Routes is right to discard such
//     a row -- BMP re-dumps the whole table on a new session, so an
//     old-session row is a claim about a session that no longer exists -- but
//     "that session no longer exists" is a statement about now, and this
//     surface is not about now. See TestRouteHistoryIncludesASupersededSession,
//     which asks Routes and RouteHistory the same question and asserts they
//     disagree.
//   - NO peer_up join and NO down-peer exclusion. A peer that has since gone
//     down still announced what it announced. See
//     TestRouteHistoryIncludesADownPeersEvents, likewise asserted against
//     Routes' opposite answer.
//   - NO argMax over (seq, stream_seq) and no GROUP BY at route-key grain.
//     Collapsing a route key to its newest observation is how a prefix
//     announced, withdrawn and re-announced becomes ONE row; here it must be
//     three. TestRouteHistory's "announce, withdraw, re-announce is three
//     events" subtest fails outright if an argMax dedup is added -- that
//     mutation was run.
//   - NO `HAVING is_withdraw = 0` and nothing else that hides a withdrawal.
//     The withdrawal is the event most callers came for.
//
// historySQL's own DISTINCT is not an exception to any of this, and the
// difference is exact: it collapses rows equal in every column including
// stream_seq -- a JetStream redelivery of one message -- and never two rows
// that describe two events. See historySQL.
//
// Order is (ts_collector, seq, stream_seq) descending. ts_collector leads
// because it is the only clock in the row this fleet controls; ts_router is
// router-reported and untrusted (a dead clock reports 1970) and is REPORTED
// but never ordered or filtered on. seq and stream_seq are the tiebreakers,
// in that order, because ts_collector is batch-granular and seq restarts per
// session. See historySQL for the full argument.
//
// f.Prefix is required: an empty one is refused with an error wrapping
// ErrBadFilter rather than read as "every prefix", because an unbounded event
// dump is not a question this endpoint answers (see HistoryFilter.Prefix).
// f.Since bounds the timeline inclusively against ts_collector, and the ZERO
// time means no bound at all -- deliberately; api/openapi.yaml's "1h" default
// is parsed by the HTTP layer, which is why this signature takes a time.Time
// and never has to decide what "now" is (see HistoryFilter.Since). f.Router,
// f.Peer and f.RIB are optional narrowings, each omitted from the statement
// entirely when unset.
//
// An unknown f.RIB fails loudly rather than quietly: rib is an Enum8, so
// ClickHouse raises on a member the contract does not define. There is no
// family filter here at all -- api/openapi.yaml documents none on
// /v1/routes/history -- so this function needs none of checkFamily's
// machinery.
func (q *Q) RouteHistory(ctx context.Context, f HistoryFilter) ([]HistoryEvent, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	w := f.predicates()
	rows, err := q.conn.Query(ctx, fmt.Sprintf(historySQL, q.db, w.clause()), w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query route history: %w", err)
	}
	defer rows.Close()

	var out []HistoryEvent
	for rows.Next() {
		var e HistoryEvent
		var nextHop string
		var isWithdraw uint8
		// stream_seq is scanned and discarded: it is in the SELECT list
		// because DISTINCT and ORDER BY have to agree on the tiebreaker
		// (see historySQL), not because a caller of RouteHistory needs the
		// JetStream sequence of the message that carried the event. See
		// HistoryEvent's doc comment for why it is not a field.
		var streamSeq uint64
		if err := rows.Scan(
			&e.TsCollector, &e.TsRouter, &e.Seq, &e.SessionID,
			&e.RouterIP, &e.PeerIP, &e.Collector,
			&e.RIB, &e.Family, &e.Prefix, &e.PathID, &isWithdraw,
			&nextHop, &e.ASPath, &streamSeq,
		); err != nil {
			return nil, fmt.Errorf("scan history event: %w", err)
		}
		e.Action = actionAnnounce
		if isWithdraw != 0 {
			e.Action = actionWithdraw
		}
		// route_unicast.next_hop is a plain String column -- it carries both
		// IPv4 and IPv6 next hops, so it is not typed IPv6 the way router_ip
		// and peer_ip are -- so parsing is this function's job rather than
		// the driver's, exactly as in Routes.
		//
		// A parse failure is deliberately not a query-wide error, for the
		// reason Routes gives and one more of this surface's own: a
		// withdrawal legitimately carries no next hop at all, so the empty
		// string is the ORDINARY case here rather than the exceptional one,
		// and ParseAddr fails on it every time. Failing the query would mean
		// no prefix that was ever withdrawn had a history. The zero Addr
		// ParseAddr already returns is what HistoryEvent.NextHop documents
		// for both cases, and what the HTTP layer renders as the contract's
		// null.
		e.NextHop, _ = netip.ParseAddr(nextHop)
		// Every address this loop hands back goes through unmapAll, next
		// hop included -- see unmapAll, and Routes' own scan loop for why
		// netip.ParseAddr is not the exemption it looks like.
		unmapAll(&e.RouterIP, &e.PeerIP, &e.NextHop)
		out = append(out, e)
	}
	return out, rows.Err()
}
