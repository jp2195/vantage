package query

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// DumpCostMeasurement is the one place the citation for the measurement
// behind DumpCounts is written down, cited the way UnscopedEventsMeasurement
// is for the unscoped events walk. It is EXPORTED for the same reason
// UnscopedEventsMeasurement is: api/'s 400 body for an over-wide
// /v1/collection/* window quotes it, so an operator reading the refusal can
// find the measurement instead of assuming the limit is arbitrary. The value
// is a repo-relative path plus the section's anchor.
//
// WHAT IT MEASURED, which a citation must not overstate: the DUMP
// classification -- the three route tables read together. It did NOT measure
// SessionCounts or FlagCounts; those are SessionsFlagsCostMeasurement's.
// Any caller quoting this citation has to carry that qualification with it.
//
// It measured two separate things this file depends on:
// that reading all three route tables together is affordable, and that each
// table's OWN route identity is load-bearing rather than defensive -- dropping
// rd from the VPN identity, or ip from the EVPN one, miscounts exactly 100
// real dumps as changes on a purpose-built 20,000-row population, per table.
//
// TestDumpCountsCitesAMeasurementThatExists guards that the file and the
// section heading are still there.
const DumpCostMeasurement = "docs/measurements.md#collection-dumps-cost"

// SessionsFlagsCostMeasurement is DumpCostMeasurement's counterpart for the
// two signals that had no measurement until 2026-09-20 -- SessionCounts' FINAL
// scan of peer_events and FlagCounts' ten-table union -- and it is guarded by
// TestCollectionCitesACostMeasurementThatExists for the same reason.
const SessionsFlagsCostMeasurement = "docs/measurements.md#collection-sessions-and-flags-cost"

// CollectionFilter is the narrowing every /v1/collection/* aggregate accepts:
// a window, an optional router, and a cap.
type CollectionFilter struct {
	// Since bounds the read inclusively against ts_collector. The zero time
	// means no lower bound at all, which reads back to each table's own
	// 90-day TTL.
	//
	// That is deliberately not refused here, unlike FleetEventFilter.Since,
	// and the difference is a measurement rather than a preference: the
	// dumps cost measurement (DumpCostMeasurement) ran all three tables at the
	// full 90-day ceiling and got 7,571,382 rows / 339 ms / ~1.46 GiB -- real
	// money on a host with 92 GB of RAM, but nowhere near the unbounded
	// fleet-events read that Since is required for. api/ clamps this to max_unscoped_since (24h
	// by default) before it ever arrives, which is where the policy belongs;
	// this layer does not pretend the query is unaffordable when it was
	// measured and is not.
	//
	// WHAT IT MEANS FOR THE ANSWER, which is not only a matter of cost:
	// the window is applied BEFORE the classification, inside each table's
	// own subquery, exactly as the shipped dashboards apply theirs. So a
	// "session dump" is the first observation of a route, in its session,
	// THAT THIS WINDOW CONTAINS -- not the first one that ever happened. A
	// route whose real dump landed an hour before `since` has its earliest
	// in-window re-advertisement reported as a dump. Widening the window can
	// therefore move a row from changes to dumps. That is the semantics
	// route-churn.json and evpn-churn.json already ship and that
	// TestDumpCountsAgreesWithTheDashboards pins; reporting a different one
	// here would mean Grafana and the API disagreeing about what a dump is.
	Since time.Time

	// Router narrows to one router; the zero Addr crosses the whole fleet.
	//
	// It is safe to apply before the classification for a reason the window
	// bound is not: router_ip is one of the PARTITION BY columns of all three
	// identities, so removing another router's rows cannot change which row
	// is the minimum of any surviving partition. Narrowing by router reads
	// less and answers identically; narrowing by time reads less and can
	// answer differently (see Since).
	Router netip.Addr

	// Limit caps the routers returned, through clampRIBLimit -- the same
	// clamp every walk in this package uses. It is a CAP and not a page size,
	// as FleetEventFilter.Limit is: there is no cursor here, and the rows are
	// ordered busiest-first so that a caller that hits the cap loses the
	// quietest routers rather than an arbitrary slice.
	Limit int
}

// RouterDumpCount is one router's share of what collection archived inside
// the window, split into the two things that share can be made of.
//
// ARCHIVED == DUMPS + CHANGES, always. Every row in the window is exactly one
// of the two, and TestDumpCountsPartsSumToArchived holds it. A response whose
// parts do not sum to the whole means a row escaped classification.
//
// Dumps is a router re-sending a route it had already sent, because its BMP
// session restarted and the table came down again. It is ORDINARY BMP
// behavior and not a fault -- see the Monitor design's "what Monitor must not
// say". A high dump ratio is a statement about what storage is composed of,
// nothing more.
//
// Changes is everything else: a genuine re-advertisement inside a live
// session, and every withdrawal. A withdrawal is NEVER a dump, structurally
// -- nothing synthesizes a withdrawal, so it cannot be the initial dump of
// anything. See dumpClassifiedSQL.
//
// Archived counts ROWS STORED, which is the question the signal asks ("what
// is our storage made of"), and this package reads no FINAL (see the package
// doc comment). So an at-least-once redelivery that a background merge has
// not yet collapsed is counted -- once for each stored copy. Its copies are
// byte-identical, hence share (seq, stream_seq) and are classified
// identically, so they land on the same side and the invariant above holds
// regardless. The dashboards do use FINAL and would therefore report one
// where this reports two, until the merge runs.
type RouterDumpCount struct {
	RouterIP      netip.Addr
	RouterSysname string
	Archived      uint64
	Dumps         uint64
	Changes       uint64
}

// THREE TABLES, THREE IDENTITIES.
//
// Each route table has its own route key and the classification below must
// partition on that table's own, never on a tuple shared between them. The
// three are read from the statements that OWN each key -- not from the
// tables' ORDER BY, which is a ReplacingMergeTree DEDUP key that deliberately
// carries ts_router and stream_seq and is not the route's identity:
//
//	route_unicast  unicastRIBKeysSQL's GROUP BY (query/rib.go)
//	route_vpn      vpnRoutesSQL's GROUP BY (query/vpnroutes.go) -- unicast's, plus rd
//	route_evpn     evpnRoutesSQL's GROUP BY (query/evpnroutes.go) -- twelve columns
//
// A UNION ALL of the three under one shared tuple would merge VPN routes
// differing only in rd and collapse EVPN type-2 rows differing only in ip.
// The 2026-09-11 dumps cost measurement (see DumpCostMeasurement) measured that: exactly
// 100 real dumps per table reported as changes, on a population built so that
// two routes share every identity column but the one under test. So the
// classification runs once per table, with that table's own identity, and
// ONLY the resulting counts are summed.
//
// TestDumpCountsIdentitiesMatchTheRIBStatements compares these three lists,
// column for column, against those statements' own GROUP BY text, so a column
// added to a route key and forgotten here fails rather than drifts.
const (
	unicastRouteIdentity = "collector_id, router_ip, peer_ip, rib, family, prefix, path_id"
	vpnRouteIdentity     = "collector_id, router_ip, peer_ip, rib, family, rd, prefix, path_id"
	evpnRouteIdentity    = "collector_id, router_ip, peer_ip, rib, route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id"
)

// locRIBRouteKeyWithinPeer is unicastRouteIdentity with the three columns that
// are already fixed inside one (router, peer) group removed -- collector_id,
// router_ip and peer_ip -- leaving what actually distinguishes one Loc-RIB
// route from another there.
//
// Dropping collector_id is the point rather than a tidy-up. LocRIBComparison
// weighs what collection holds against the router's own claim, and that claim
// is one number whatever the fleet's collector count is, so the archived side
// has to count a route once however many collectors hold a copy. It cannot get
// there by dropping collector_id from the inner GROUP BY instead: that group
// resolves each route's latest state with argMax over (seq, stream_seq), and
// seq is a per-stream counter each collector mints for itself, so a group
// spanning two of them ranks independent counters against each other. The
// state stays per collector; only the counting is shared.
//
// TestLocRIBRouteKeyWithinPeerMatchesTheUnicastIdentity pins the two together,
// so a column added to the unicast route key and forgotten here fails.
const locRIBRouteKeyWithinPeer = "rib, family, prefix, path_id"

// dumpClassifiedSQL classifies every row of ONE route table, and is rendered
// three times -- once per table, once per identity.
//
// The expression is the one that shipped to deploy/grafana/dashboards/
// route-churn.json and evpn-churn.json, unchanged:
//
//	is_withdraw = 0 AND (seq, stream_seq) = min((seq, stream_seq)) OVER (
//	  PARTITION BY <this table's identity columns>, session_id
//	) AS is_session_dump
//
// ORDERED ON (seq, stream_seq), NEVER ON A TIMESTAMP. seq and stream_seq are
// the collector's own monotonic counters; ts_router is whatever the router
// claimed and ts_collector is a wall clock that can step backwards. Deciding
// which observation of a route came first in its session is exactly the
// question a clock must not be asked.
//
// session_id IN THE PARTITION is what makes the answer mean anything. Without
// it, a route re-dumped once per session across five sessions and a route
// advertised five times inside one session are the same five rows with the
// same one minimum -- which is the indistinguishability the whole signal
// exists to remove.
//
// is_withdraw = 0 IS NOT SYMMETRY. A withdrawal can be the first thing seen
// for a route in a session (a measured archive held 246 of them in
// route_evpn, none of them first-in-session -- which is precisely why a rule
// without this clause LOOKS correct on that table), but nothing synthesizes a
// withdrawal: a router re-dumping its table sends what it has, not what it
// does not. A withdrawal is always a change.
//
// ts_collector AND is_withdraw are selected for a SECOND consumer,
// ChurnBuckets, which groups the same classified rows into time buckets and
// keeps withdrawals as their own series. DumpCounts ignores both columns.
// They live here rather than in a churn-specific copy of this template
// because the classification expression is the thing that must not drift --
// it is the one that shipped to route-churn.json and evpn-churn.json, and a
// second copy is how the Go answer and the dashboards would come apart while
// both still looked right.
//
// %[5]s IS THAT RULE GENERALIZED, for the two grouped consumers added after
// those three. ChurnByPeer needs peer_ip and peer_asn; ChurnByPrefix needs
// prefix, session_id and the rest of the unicast identity. Neither set can
// be hard-coded here -- route_evpn has no `family` column at all, so one
// fixed list cannot render against all three tables -- and giving each a
// copy of the classification is the drift this comment exists to prevent.
// So the columns vary and the expression does not. Callers pass "" when they
// need nothing extra, and a leading comma when they do.
//
// %[1]s is the database name, %[2]s the table, %[3]s that table's identity
// columns, %[4]s the WHERE clause.
const dumpClassifiedSQL = `
    SELECT
        router_ip,
        router_sysname,
        seq,
        stream_seq,
        ts_collector,
        is_withdraw%[5]s,
        is_withdraw = 0 AND (seq, stream_seq) = min((seq, stream_seq)) OVER (
            PARTITION BY %[3]s, session_id
        ) AS is_session_dump
    FROM %[1]s.%[2]s
    %[4]s`

// dumpCountsSQL folds the three classified tables into one row per router.
//
// It groups on router_ip alone and resolves the NAME with argMax, rather than
// grouping on (router_ip, router_sysname). router_sysname is a per-row column
// carrying whatever the router called itself at the time, so a device renamed
// mid-window would otherwise come back as two rows for one router -- twice in
// a fleet list, each with part of its archive. argMax over (seq, stream_seq)
// is how peerStateCTE resolves the same column for the same reason.
//
// The alias is `sysname`, not `router_sysname`. ClickHouse folds a SELECT
// alias back into sibling expressions in the same SELECT list, which is what
// turned `max(ts_collector) AS ts_collector` into an aggregate inside an
// aggregate in peerEventsSQL (Code: 184, ILLEGAL_AGGREGATION); naming the
// result after the column it aggregates is the shape that has already gone
// wrong once here. peerStateCTE aliases the identical expression `sysname`.
//
// ORDER BY archived DESC is what makes Limit a sensible cap rather than an
// arbitrary truncation: a caller that asks for fewer routers than the fleet
// has loses the quietest ones. router_ip breaks ties so the answer is stable
// across calls.
//
// total_matched is count() OVER () -- a window function over the WHOLE
// grouped result, computed BEFORE ORDER BY and LIMIT truncate it (both are
// logically applied last, over the already-materialized SELECT list, in
// ClickHouse as in the SQL standard), and rendered as one extra column
// carrying the SAME value on every row LIMIT lets through. That is what
// lets DumpCounts report "N routers matched, K were returned" for the cost
// of one more pass over a result set the GROUP BY already built, rather
// than a second COUNT(DISTINCT router_ip) statement.
//
// It adds no detectable cost over the plain (non-windowed) form, measured
// rather than assumed. This statement, unscoped against the full live
// archive, best of three on 2026-09-11: with the column, 16,422 read_rows /
// 1,540,939 read_bytes / 7-8 ms; without it, 16,422 / 1,540,939 / 6-7 ms --
// identical to the byte, and the duration and memory differences are inside
// run-to-run noise. flagCountsSQL's own doc comment records the same
// measurement on api/'s most expensive signal, the ten-table union, and
// reaches the same result. A window function over a result set the GROUP BY
// already built adds one more pass over rows already in memory, not a new
// scan.
//
// Read as 0 when the result is empty, and the mechanism is the absence of a
// mechanism: a window function produces no rows to carry a value on when
// there is nothing to group, so DumpCounts' res.Next() loop never runs and
// its named `total` return keeps its zero value. There is no fallback branch
// -- nothing tests for the empty case, because nothing has to.
//
// %[1]s, %[2]s and %[3]s are the three rendered classifications; %[4]d is the
// clamped limit.
const dumpCountsSQL = `
SELECT
    router_ip,
    argMax(router_sysname, (seq, stream_seq)) AS sysname,
    count()                                   AS archived,
    countIf(is_session_dump)                  AS dumps,
    countIf(NOT is_session_dump)              AS changes,
    count() OVER ()                           AS total_matched
FROM (
%[1]s
    UNION ALL
%[2]s
    UNION ALL
%[3]s
)
GROUP BY router_ip
ORDER BY archived DESC, router_ip
LIMIT %[4]d`

// DumpCounts reports, per router, how much of what collection archived inside
// f's window was the router re-dumping a route after a session reset, and how
// much was the network actually changing.
//
// It reads route_unicast, route_vpn AND route_evpn. Reading only
// route_unicast would understate a claim about storage, which is the error
// fleet-health's parse-flag panel already made once -- it "reported 2
// occurrences of a condition that had fired 43 times fleet-wide". The
// 2026-09-11 dumps cost measurement (DumpCostMeasurement) measured all three together at
// 802,411 rows / 28 ms / ~50 MiB at the endpoint's 24h operating point, and
// concluded no narrowing was needed.
//
// THE CLASSIFICATION RUNS PER TABLE AND ONLY THE COUNTS ARE SUMMED. See the
// three-identity comment above unicastRouteIdentity: a single UNION ALL under
// a shared tuple would merge routes that are not the same route. Several
// callers read this function's numbers, so a wrong identity here is not
// contained.
//
// A DUMP IS NOT A FAILURE, and nothing in this answer is scored. See
// RouterDumpCount.
//
// The uint64 return is total_matched: how many routers the window and
// router= filter matched BEFORE f.Limit truncated the answer, 0 when it is
// empty. See dumpCountsSQL's own doc comment for how it is computed
// (count() OVER ()) and why that costs nothing extra to read.
func (q *Q) DumpCounts(ctx context.Context, f CollectionFilter) (rows []RouterDumpCount, total uint64, err error) {
	var w filters
	w.eqAddr("router_ip", f.Router)
	w.since("ts_collector", f.Since)
	where := w.clause()

	stmt := fmt.Sprintf(dumpCountsSQL,
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_unicast", unicastRouteIdentity, where, ""),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_vpn", vpnRouteIdentity, where, ""),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_evpn", evpnRouteIdentity, where, ""),
		clampRIBLimit(f.Limit),
	)

	// The same WHERE is rendered three times, so its values are bound three
	// times, in the order the three branches appear. The driver binds strictly
	// left to right and has no idea the three predicates came from one filter
	// -- see filters.values' own note on a caller that splices one rendering
	// into several positions.
	args := make([]any, 0, 3*len(w.values()))
	for range 3 {
		args = append(args, w.values()...)
	}

	res, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query dump counts: %w", err)
	}
	defer res.Close()

	var out []RouterDumpCount
	for res.Next() {
		var r RouterDumpCount
		if err := res.Scan(&r.RouterIP, &r.RouterSysname,
			&r.Archived, &r.Dumps, &r.Changes, &total); err != nil {
			return nil, 0, fmt.Errorf("scan dump count: %w", err)
		}
		unmapAll(&r.RouterIP)
		out = append(out, r)
	}
	return out, total, res.Err()
}

// RouterSessionCount is one router's BMP session churn inside f's window:
// how many distinct sessions collection saw, and how each session's peer
// events broke down.
//
// SESSIONS ARE NOT EVENTS. One BMP session can carry many peer_events rows --
// every peer under it announces its own up, and can flap down and back up
// without the session itself resetting -- so Sessions is uniqExact(session_id),
// never count(). A query that counted rows would report how chatty the
// session was, not how often the router's transport actually reset, which is
// the question this signal exists to answer.
// TestSessionCountsCountsSessionsNotEvents pins this against a fixture built
// so the two numbers disagree: one session_id, five peer_events rows across
// four peers.
//
// Up, Down and ViewLost are peer_events.kind counts -- events, not sessions,
// so a peer that flapped twice inside one session contributes twice to
// whichever of Up/Down it flapped through. ViewLost is NEVER folded into
// Down, and the two describe different subjects: Down is the router telling
// BMP a BGP session ended, on the wire, with a reason code (RFC 7854 sec
// 4.9); ViewLost is the collector losing its own BMP transport to the
// router while the router said nothing at all (down_reason is 0 on every
// such row -- see schema.sql's own comment on peer_events.kind). Reporting
// the second as the first misreports the collector's own blindness as a
// fact about the network, which is exactly the distinction this project's
// EventKindMark vocabulary (extracted 2026-09-09) already holds across three
// other screens. TestSessionCountsKeepsViewLostApartFromDown seeds both
// kinds, in different counts, and asserts they land in separate columns; a
// production archive cannot supply this fixture -- it carries 'up' and
// 'down' only, zero 'view_lost' rows -- so a test that trusted the
// archive for this would pass vacuously.
//
// RouterSysname is whatever the router's sysName TLV carried, verbatim --
// including the empty string, which two live routers report. This package
// does not substitute a placeholder for it, the same choice RouterDumpCount
// makes for its own RouterSysname field: rendering something like
// parse-anomalies.json's "(no sysName TLV)" is a display decision for a
// caller closer to the screen, not a fact this layer manufactures. A
// caller consuming this field should decide its own rendering rather than
// assume this one already picked.
type RouterSessionCount struct {
	RouterIP      netip.Addr
	RouterSysname string
	Sessions      uint64
	Up            uint64
	Down          uint64
	ViewLost      uint64
}

// sessionCountsSQL is SessionCounts' whole statement, over peer_events
// resolved per collector and then merged per router.
//
// # Three columns are the router's and one is the collectors'
//
// This statement grouped by router_ip alone until 2026-09-20, so every
// column scaled with how many collectors happened to be watching,
// confirmed against a measured archive: router 172.22.0.8 reported 7
// sessions where dev-c1 saw 5 and dev-c2 saw 2, and 14 ups against their
// 10 and 4. Router 172.22.0.10, watched simultaneously by both, reported
// 2 sessions for one session lifecycle seen twice.
//
// Sessions, Up and Down come from the BEST SINGLE VANTAGE POINT -- the
// collector that heard the most FROM THE ROUTER, reported whole, exactly as
// churnByPeerSQL chooses and for the same reasons (there is no
// collector-independent event identity to deduplicate on, and ts_router is
// not a key this package will discriminate with). RouterSessionCount's own
// doc settles that these are router facts: Sessions answers "how often the
// router's transport actually reset", and a Down is the router telling BMP
// a BGP session ended, on the wire, with a reason code.
//
// The ranking is (up + down, collector_id) rather than every event, which
// matters here in a way it does not elsewhere: view_lost rows are the
// collector's own, so counting them toward "who saw the most" would let a
// collector win the vantage-point choice by going blind.
//
// VIEW_LOST IS SUMMED, DELIBERATELY, unlike its neighboring columns here.
// It is a COLLECTOR-side statement -- Session.Close recording that it
// stopped being able to see the peer, with down_reason 0 because the router
// said nothing -- so it describes COLLECTION, and a collection quantity
// legitimately scales with the number of collectors, as `observations` does
// on the most-changed-prefixes panel.
//
// Best-vantage here would not be merely imprecise, it would be
// signal-suppressing and structurally so: a collector that has gone blind
// records fewer router statements BY DEFINITION, so it always loses the
// choice, and the one column that exists to surface its blindness would be
// read off the collector that stayed healthy. On a collection-health signal
// that is the wrong failure direction. TestSessionCountsKeepsEveryCollectors
// LostView pins the adversarial case, where every view_lost row belongs to
// the losing collector and a best-vantage answer reads 0.
//
// So one row mixes two subjects. That is a real cost and it is taken
// knowingly: what must be avoided is an ARBITRARY mix -- a per-column argMax
// reporting one collector's ups beside another's downs -- and these two
// subjects are nameable, named by the columns, and written down here.
//
// A known limitation, stated rather than discovered later: session counts
// cannot tell a ROUTER reset from a COLLECTOR reset. A collector restarting
// opens a new session_id without the router doing anything, and since the
// choice takes the maximum, a collector that churns on its own inflates
// Sessions. Nothing in peer_events distinguishes the two without
// correlating session starts across collectors on wall clocks this project
// does not trust. Best vantage point at least stops the count scaling with
// the NUMBER of observers, which was the defect.
//
// FINAL, unlike every route-table statement in this package (see the package
// doc comment's "No FINAL anywhere" and DumpCounts' own account of the cost
// that rule avoids). The difference is what each query is asking. DumpCounts
// and every route surface resolve a route key's NEWEST observation with
// argMax, which stays correct whether or not a background merge has
// collapsed a redelivered duplicate yet -- an unmerged pair loses to its own
// newer sibling regardless of which copy a query happens to see. This
// statement instead COUNTS raw events (uniqExact(session_id), countIf(kind =
// ...)), and a count has no such defense: an at-least-once redelivery of the
// identical peer_events row is, until the next merge, a second row carrying
// the same kind and the same session_id, and without FINAL it is counted
// twice. peer_events is ReplacingMergeTree and can take FINAL (unlike
// eor_events, which cannot -- confirmed 2026-09-10, "nine tables take
// FINAL; the tenth does not"), and this project's own
// dashboards already read it this way: route-churn.json's panel documents
// "FINAL is load-bearing rather than decoration" for the identical
// redelivery reason, and link-state-topology.json's own `countDistinct(session_id)
// ... FROM vantage.peer_events FINAL` panel is this query's session-count
// half, verbatim. The Monitor design's own cost table calls this endpoint
// "cheap" beside dumps' measured three-table read, which is the trade this
// table can afford where route_unicast, at 2,511,496 rows at the 90-day
// ceiling (the dumps cost measurement's own per-table figure, not its
// 7,571,382 three-table sum -- see DumpCostMeasurement), could not.
//
// MEASURED 2026-09-20 (SessionsFlagsCostMeasurement). This used to record
// itself as UNMEASURED, resting on a judgment: that peer_events "cannot
// approach the route tables' row counts at any comparable window". The rig
// put it at 1.02M rows against route_unicast's 1.5M -- a fleet flapping far
// harder than the assumption allows -- and this statement still returned in
// 23 ms at the 90-day ceiling and 3 ms at the 24h clamp it actually runs
// under. The judgment holds, but now for a measured reason rather than a
// structural one.
//
// The surprise was the direction. This is ONE table and the Monitor design
// calls it "cheap"; FlagCounts is TEN and the design calls it the expensive
// one. At the 90-day ceiling this reads 94 MiB and FlagCounts reads 81 --
// because FINAL reads every column to merge, while each of that union's ten
// branches reads stream_seq and parse_flags and nothing else. Table count was
// the wrong proxy for cost, in both directions.
//
// argMax(router_sysname, ts_collector), not (seq, stream_seq): a router's
// displayed name is cosmetic here, unlike a route's own newest observation,
// and ts_collector is adequate for picking one recent name among what should
// be a constant column per router in any case.
//
// The alias is `sysname`, not `router_sysname` -- the same reason
// dumpCountsSQL gives at length: naming a SELECT alias after the column it
// aggregates is the shape that has already produced an aggregate-inside-
// aggregate collision once in this package (Code: 184, ILLEGAL_AGGREGATION).
// Nothing else in this SELECT list references router_sysname again, so
// today's statement would not actually collide -- but matching the file's
// one safe spelling costs nothing and is what "match it rather than
// inventing a second style" means in practice.
//
// total_matched is count() OVER () -- see dumpCountsSQL's own doc comment
// for what it computes, when it truncates versus f.Limit, and why it costs
// nothing extra to carry.
//
// %[1]s is the database name, %[2]s the WHERE clause -- rendered through
// filters exactly as DumpCounts renders its own (see CollectionFilter.Since
// and .Router for what each predicate means and when it is omitted) rather
// than a fixed, always-bound WHERE; the difference is that
// CollectionFilter.Since's zero value is a real, documented "no lower bound"
// rather than a value this statement can assume is always set. %[3]d is the
// clamped limit, rendered the same way dumpCountsSQL's own %[4]d is.
const sessionCountsSQL = `
SELECT
    router_ip,
    sysname,
    sessions,
    up,
    down,
    view_lost,
    count() OVER () AS total_matched
FROM (
    SELECT
        router_ip,
        argMax(sysname, (router_events, collector_id))  AS sysname,
        argMax(sessions, (router_events, collector_id)) AS sessions,
        argMax(up, (router_events, collector_id))       AS up,
        argMax(down, (router_events, collector_id))     AS down,
        sum(view_lost)                                  AS view_lost
    FROM (
        SELECT
            router_ip,
            collector_id,
            argMax(router_sysname, ts_collector) AS sysname,
            uniqExact(session_id)                AS sessions,
            countIf(kind = 'up')                 AS up,
            countIf(kind = 'down')               AS down,
            countIf(kind = 'view_lost')          AS view_lost,
            up + down                            AS router_events
        FROM %[1]s.peer_events FINAL
        %[2]s
        GROUP BY router_ip, collector_id
    )
    GROUP BY router_ip
)
ORDER BY sessions DESC, router_ip
LIMIT %[3]d`

// SessionCounts reports, per router, how many distinct BMP sessions
// collection observed inside f's window, and how each session's peer events
// resolved: the router reporting a peer up, the router reporting a peer
// down, or the collector losing its own transport to the router while the
// router reported nothing (view_lost).
//
// See RouterSessionCount for what each column means and, more importantly,
// what it must never be folded into: ViewLost is not a synonym for Down, and
// Sessions counts sessions, never the events inside them.
//
// # Cost, measured rather than asserted
//
// This endpoint was labeled "cheap" with no number behind it, unlike dumps.
// Measured 2026-09-17 through THIS function (not the table: query_id-tagged
// runs read back out of system.query_log, best of three) on one measured
// archive's 813 peer_events rows: unbounded, 813 read_rows / 53,420
// read_bytes / 1 ms / 121-253 KiB peak memory, answering 11 router rows. At
// the three windows the Monitor screen offers -- 1h, 6h, 24h -- 0 read_rows
// and 1-2 ms, because every part's max ts_collector predates all three.
//
// That zero is the shape worth carrying forward, and it is not "the window
// made it free". ts_collector is in the PARTITION key (toYYYYMM) and in no
// table's sorting key -- peer_events sorts on (router_ip, peer_ip, rib,
// ts_router, stream_seq) -- so `since` prunes whole PARTS by their min/max
// and nothing finer. A part wholly older than the window is skipped
// entirely; a part that straddles the boundary is read in full. Cost
// therefore tracks the peer_events rows in the parts a window INTERSECTS,
// not the window's own width, and a narrow window over a currently-writing
// month buys nothing.
//
// peer_events at fleet scale is still unmeasured, and deliberately: no rig
// in this project holds more than a few hundred of them (ribload's copy has
// one row), the table takes one row per session lifecycle event rather than
// per route, and api/ clamps the window to max_unscoped_since before this
// layer is reached. What to re-measure if that assumption is ever in doubt
// is this function at a peer_events row count that fills a month's part.
//
// The uint64 return is total_matched -- see DumpCounts' own doc comment for
// what it means and 0 when the result is empty.
func (q *Q) SessionCounts(ctx context.Context, f CollectionFilter) (rows []RouterSessionCount, total uint64, err error) {
	var w filters
	w.eqAddr("router_ip", f.Router)
	w.since("ts_collector", f.Since)
	where := w.clause()

	stmt := fmt.Sprintf(sessionCountsSQL, q.db, where, clampRIBLimit(f.Limit))

	res, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, 0, fmt.Errorf("query session counts: %w", err)
	}
	defer res.Close()

	var out []RouterSessionCount
	for res.Next() {
		var r RouterSessionCount
		if err := res.Scan(&r.RouterIP, &r.RouterSysname,
			&r.Sessions, &r.Up, &r.Down, &r.ViewLost, &total); err != nil {
			return nil, 0, fmt.Errorf("scan session count: %w", err)
		}
		unmapAll(&r.RouterIP)
		out = append(out, r)
	}
	return out, total, res.Err()
}

// PeerLocRIB is one (router, peer)'s Loc-RIB comparison: what the router's
// newest BMP Stats Report claims its Loc-RIB holds, against how many
// route_unicast rows collection has archived FOR THAT SAME RIB VIEW --
// rib = 'loc_rib', never the peer's adj-RIB-in rows -- for that peer.
//
// THE COMPARISON IS UNICAST-ONLY, and that is an honest limit stated by the
// Monitor design, not an oversight. Counter 8 (RFC 7854 §4.8, "Number of
// routes in Loc-RIB") is one count with no family breakdown, and only
// route_unicast holds a route identity this package already trusts for it
// (see unicastRouteIdentity and the three-identity comment above it).
// Summing route_vpn and route_evpn into Archived would compare one router
// number against a total the router never reported.
//
// ARCHIVED IS SCOPED TO rib = 'loc_rib', and that is not a detail. It is
// tempting to omit it, on the reading that route_unicast's rows are simply
// "what collection archived for the peer".
// route_unicast holds five RIB views (in_pre, in_post, out_pre, out_post,
// loc_rib; see envelope.RIB's own comment), and on one measured archive
// in_pre carries 8,553 rows across 24 peers while loc_rib carries 27 across
// 2 -- 99.7% of an unfiltered count would have been adj-RIB-in, compared
// against a Loc-RIB number the router never claimed to describe adj-RIB-in
// with. That is not a coarser comparison, it is a meaningless one: a peer
// reporting a Loc-RIB of 11 would be compared against hundreds of in_pre
// rows and render a fabricated surplus on an operational screen.
// fleet-health's own "Loc-RIB routes by peer" panel is NOT prior art for the
// unfiltered form -- it reads only stats_events and never joins route_unicast
// at all, leaving the comparison to the operator's eye; this function does
// the comparison in code, so it has to compare like with like.
//
// THE CONSEQUENCE, WORTH STATING PLAINLY rather than leaving a caller to
// discover it: RFC 9069 Loc-RIB monitoring is a separate capability a router
// must be configured to send, distinct from ordinary adj-RIB-in BMP route
// monitoring, and most routers in a private test deployment are not
// configured for it. So Archived = 0 against a non-zero Reported, for MOST
// peers, is the expected and correct reading of "this peer's Loc-RIB
// monitoring is not enabled" -- NOT evidence collection dropped anything.
// This is a "must not say" rule in the data: A LOC-RIB GAP IS NOT PROOF OF
// LOSS. A caller rendering this comparison on the Monitor screen must carry
// that reading forward rather than rediscovering it from a wall of zeroes.
//
// HasStat is what keeps a Reported of 0 honest, and it exists around one
// specific, measured hazard: a ClickHouse Map returns 0 for a missing key,
// so counters[8] on a peer whose Loc-RIB really is empty and counters[8] on
// a peer that has NEVER sent stat type 8 are the identical value, 0. Only
// has(counters, 8) tells them apart. Without HasStat, a silent peer -- the
// one measured archive held three, of 26 (router, peer) pairs --
// reports "router says 0, we archived N" for every N > 0: a collection
// artifact rendered as proof of loss, the exact defect class this project's
// Monitor screen exists to catch rather than add a new instance of.
// TestLocRIBComparisonDistinguishesNoStatFromZero is the finding this
// field exists around, held as a test.
//
// Reported is the router's own newest claim, resolved the same way
// fleet-health's "Loc-RIB routes by peer" panel resolves it (deploy/grafana/
// dashboards/fleet-health.json): argMax(counters[8], (ts_collector,
// stream_seq)), restricted to the rows that carry stat type 8 at all. THE
// NEWEST, not the first -- a Loc-RIB's size moves as routes come and go, and
// an argMin here would compare against a number the router reported once,
// possibly long superseded. TestLocRIBComparisonTakesTheLatestStatNotTheFirst
// holds that direction.
//
// Archived counts route_unicast's currently live Loc-RIB routes for the
// peer -- rib = 'loc_rib' ONLY, see above -- in each (collector, router)'s
// CURRENT BMP session (the newer of peer_current's and the loc_rib rows'
// own newest; see locRIBComparisonSQL), grouped on unicastRouteIdentity (the same identity
// DumpCounts classifies against) whose newest observation in that session,
// by (seq, stream_seq), was not a withdrawal. A route this peer advertised
// and later withdrew is not something the router still has, and counting it
// would overstate our own archive against the router's live claim.
//
// ONE SESSION, NOT EVERY SESSION collection still holds -- and cleanup keeps
// two per (collector, router) on purpose, so there are routinely two. seq is
// the per-session message counter and restarts at every reconnect, so
// ranking a route's rows by (seq, stream_seq) across sessions compares two
// unrelated counters: a route announced late in the old session and
// withdrawn early in the new one read as live, and one withdrawn late in the
// old session and re-announced early in the new one read as dead. Ordering
// on session_id first would fix those two and still count a route the old
// session announced and the new one never mentioned. It is not live: a new
// session re-dumps the router's whole table, and Reported counts what the
// router holds now. TestLocRIBComparisonResolvesEachRouteInTheCurrentSession
// holds all three, plus the collector half of the session key.
//
// CollectionFilter.Since DOES NOT SCOPE Archived, and that is a contract
// statement rather than an implementation detail: the window bounds Reported
// and nothing else. The two sides are structurally different quantities.
// Reported is a LATEST SNAPSHOT -- one argMax over stats_events, the newest
// number the router has claimed. Archived is what collection currently
// HOLDS. Windowing the archived side would turn it into an accumulation
// over the window instead, and comparing an accumulation against a snapshot
// reads as a gap that is an artifact of the window: a peer whose Loc-RIB is
// stable wrote its rows once, at session start, and has written nothing
// since, so a narrowing window decays its Archived toward 0 while Reported
// holds steady. Measured on one archive on 2026-09-11, holding
// the stats side fixed and varying only the archived side: 2 pairs with
// Archived > 0, summing to 4, unwindowed and at 2160h alike -- and 0 pairs,
// summing to 0, at 313h, 24h, 6h and 1h alike. The newest rib = 'loc_rib'
// row on that archive is 2026-08-27 while the newest stats_events row is
// 2026-08-30, and the Monitor screen offers 1h, 6h and 24h and nothing
// wider, so EVERY window the screen can ask for would have rendered
// Archived = 0 against a non-zero Reported for every peer -- a fabricated
// Loc-RIB gap, on the screen whose third "must not say" rule is that A
// LOC-RIB GAP IS NOT PROOF OF LOSS.
//
// The cost of reading the archived side unwindowed is small and measured
// rather than assumed: rib = 'loc_rib' is a tiny minority of route_unicast
// (27 rows of 8,611 on this archive), so dropping the bound adds rows the
// partition pruner was skipping but almost no bytes. This whole statement,
// best of three against a production archive on 2026-09-11: unwindowed, 30,072
// read_rows / 2,630,754 read_bytes / 9-11 ms; with a 24h bound on the
// archived side, 12,852 read_rows / 2,565,420 read_bytes / 10 ms. +2.5% of
// bytes and no detectable time, for an answer that is correct instead of
// fabricated.
type PeerLocRIB struct {
	RouterIP, PeerIP netip.Addr
	Reported         uint64
	Archived         uint64
	HasStat          bool
}

// locRIBComparisonSQL is LocRIBComparison's whole statement.
//
// reported and archived are each grouped on (router_ip, peer_ip) alone, and
// peers is their identity UNION. Driving the final SELECT from peers, rather
// than a FULL JOIN of reported against archived directly, is what lets every
// measure column resolve its own "peer absent from this side" case through
// this server's ordinary LEFT JOIN default rather than a NULL check: this
// cluster runs join_use_nulls = 0 (verified in query.go's own doc comment,
// "Verified on 24.8.14.39"), so an unmatched numeric column already comes
// back as that type's zero value with no coalesce required -- and 0 is
// exactly Reported, Archived and HasStat's own "this side has nothing to
// say" meaning. coalesce is applied anyway, matching peersSQL's own
// `coalesce(route_counts.n, 0)`: the guard costs nothing today and keeps the
// answer correct if join_use_nulls is ever turned on, rather than this
// package depending on a setting it does not control.
//
// reported carries NO FINAL, like everything else in this package (see
// query.go's own package doc comment). argMax over (ts_collector, stream_seq)
// is immune to a redelivered, byte-identical duplicate row on its own: the
// duplicate shares its sibling's tie-break value exactly, so argMax picks the
// same counters[8] from either copy. archived is immune for a separate
// reason -- it counts route KEYS through a GROUP BY, so a duplicate row falls
// inside the group it already belongs to rather than creating one.
//
// That GROUP BY leads with collector_id, though, so its groups are per
// collector and counting them counted a route once per collector holding it.
// The outer aggregate is therefore uniqExact over locRIBRouteKeyWithinPeer
// rather than count(): state resolves per collector, and the route is counted
// once across them. See that constant for why the collector cannot simply be
// dropped from the inner group, and
// TestLocRIBComparisonCountsARouteOnceAcrossTwoCollectors for the fixture that
// separates this answer from both wrong ones.
//
// IT DID CARRY FINAL until 2026-09-19, and the reason the comment gave was
// "matching fleet-health's own panel SQL verbatim ... for fidelity to the
// panel this statement is required to agree with, not because omitting it
// would change this answer". A 2026-09-19 change had already dropped FINAL
// from that panel -- "Loc-RIB routes by peer" and "Prefixes rejected by
// inbound policy", both untested at the time and neither named in that
// change's description -- so the stated reason had been arguing for removal
// before anyone noticed it had changed sides.
//
// TestLocRIBComparisonIsCorrectWithUnmergedDuplicatesPresent is what settled
// it, rather than the argument above: merges disabled, both sides written
// duplicated, the duplicates asserted to really still be two rows, and
// Reported and Archived asserted not to move. Mutation-checked with FINAL
// absent -- collapsing archived's inner GROUP BY to a row count makes it read
// 6 for 3 routes. Measured on a scale test on 2026-08-13, FINAL cost 48x
// the rows read and 1,270x the memory.
//
// has_stat is a literal `1 AS has_stat` inside reported, never a
// max(has(counters, 8)) computed over every row of the group: reported's own
// FROM is already restricted to has(counters, 8) rows (folded into %[2]s --
// see LocRIBComparison), so every row reported's GROUP BY produces already
// carries the stat, and a peer that has never sent it has no row in reported
// at all. That absence is exactly what the LEFT JOIN miss above turns into
// HasStat = false; nothing here computes "did any row have it" directly.
//
// archived's inner subquery is unicastRIBKeysSQL's own withdrawal test
// (`HAVING argMax(is_withdraw, (seq, stream_seq)) = 0`) under the same
// session scoping: its WHERE (folded into %[3]s) keeps only rows whose
// (collector_id, router_ip, session_id) is the session locrib_cur resolves.
// That argMax is only meaningful inside one session -- see
// PeerLocRIB.Archived for what resolving across sessions got wrong. The
// collector is part of the key for peerStateCTE's own reason: session_ids
// are minted per collector process and nothing makes them distinct across
// collectors, so (router_ip, session_id) alone can match one collector's
// superseded session against another's current one.
//
// locrib_cur is, per (collector, router), the NEWER of two sessions: cur's
// (peerStateCTE's one definition of the current session, max(session_id)
// over peer_current) and the newest session the router's own loc_rib rows
// carry (folded into %[7]s). cur alone is not enough, because cur is built
// from Peer Ups and route rows do not wait for them:
//
//   - A router with loc_rib rows and no peer_events row at all -- a Peer Up
//     lost, or malformed (collector/session.go's handlePeerUp writes no
//     row for one it cannot decode) -- has no cur row, and scoping to cur
//     alone read its Archived as 0 against a real Reported: a fabricated
//     Loc-RIB gap, from a side that did not depend on peer_events at all
//     before it was session-scoped.
//   - A router that reconnected, whose new session's loc_rib rows landed
//     before that session's Peer Up (different streams; sink/cleanup.go's
//     doc names this as ordinary), still has cur on the OLD session. The
//     newest session is the right answer there too: session_ids order a
//     (collector, router)'s sessions -- the assumption cur and cleanup
//     already rest on -- so a loc_rib row in a newer session proves the old
//     one is superseded.
//
// Where cur is the newer, cur wins, even though the router has sent no
// loc_rib row in it: that session's Loc-RIB, as far as collection knows, is
// empty, and the older session's routes are ghosts.
// TestLocRIBComparisonFallsBackToTheNewestLocRIBSession holds the first two;
// the second collector in TestLocRIBComparisonResolvesEachRouteInTheCurrentSession
// holds the third.
//
// What neither source can see is a newer session that has written neither
// a Peer Up nor a loc_rib row yet; locrib_cur then reads the session before
// it, as cur would. Nothing in collection distinguishes that from a router
// that has not reconnected.
//
// It does NOT take unicastRIBKeysSQL's peer_up gate. That gate asks whether
// the peer's BGP session is up; this signal compares what collection holds
// against the router's own count, and Loc-RIB peer accounting is thin
// (peerStateCTE's doc records 1 loc_rib peer_events row on one measured
// archive), so gating on it could drop routes for want of a Peer Up rather
// than because the router let them go. That risk is unmeasured, and keeping
// the gate out keeps this change to the defect.
//
// cur is the only one of peerStateCTE's CTEs this statement names.
// peer_state rides along in the text unused, as it does in
// unicastRIBKeysSQL -- see that statement's own doc comment for why sharing
// the one definition is worth carrying it.
//
// archived's WHERE (folded into %[3]s -- see LocRIBComparison) carries
// `rib = 'loc_rib'`, and that predicate is load-bearing, not decoration: see
// PeerLocRIB's own doc comment for why comparing Reported (a Loc-RIB count)
// against an unscoped route_unicast count -- 99.7% adj-RIB-in on one
// measured archive -- is not a looser answer but a wrong one.
// TestLocRIBComparisonCountsLocRIBRowsOnlyNotAdjRIBIn pins this against a
// fixture built so the two counts differ, which is what makes the filter's
// presence observable at all.
//
// That WHERE carries NO ts_collector bound, and its absence is the half of
// this statement most easily "tidied" back in. %[2]s is windowed and %[3]s
// is not, because reported is a snapshot (one argMax over the window's
// newest stat) while archived is what collection currently holds. Bounding
// %[3]s too would make archived an ACCUMULATION over the window and compare
// it against a snapshot, which reads as a Loc-RIB gap that is an artifact of
// the window rather than anything about the data -- see PeerLocRIB.Archived
// for the measurement, and
// TestLocRIBComparisonArchivedSideIgnoresTheWindow for the test that fails
// the moment a since is put back on this clause.
//
// total_matched is count() OVER () -- see dumpCountsSQL's own doc comment
// for what it computes and why it costs nothing extra to carry. It sits on
// the OUTER SELECT, over `peers` after both LEFT JOINs, so it counts
// (router, peer) PAIRS -- the same population LIMIT truncates -- not either
// CTE's own row count on its own.
//
// ORDER BY is written as `coalesce(archived.archived, 0)` rather than the
// bare alias `archived`, and the qualification is the point: `archived` is
// BOTH a CTE name and an output column alias in this statement, so a bare
// `ORDER BY archived DESC` is ambiguous on its face even though ClickHouse
// resolves it to the column today. Naming the CTE-qualified column inside
// the same coalesce the SELECT list uses says which one is meant and keeps
// the ordering correct if join_use_nulls is ever turned on -- see the note
// on coalesce above for why every measure column carries one.
//
// %[1]s is the database, %[2]s the stats_events WHERE (CollectionFilter's
// Router and Since plus `has(counters, 8)`), %[3]s the route_unicast WHERE
// (CollectionFilter's Router plus `rib = 'loc_rib'` and the current-session
// IN -- and NOT its Since; see the paragraph above and LocRIBComparison),
// %[4]s unicastRouteIdentity, %[5]d the clamped limit, %[6]s
// locRIBRouteKeyWithinPeer, %[7]s locrib_cur's route_unicast_current WHERE
// (CollectionFilter's Router plus `rib = 'loc_rib'`).
const locRIBComparisonSQL = "WITH " + peerStateCTE + `,
reported AS (
    SELECT
        router_ip,
        peer_ip,
        argMax(counters[8], (ts_collector, stream_seq)) AS reported,
        toUInt8(1) AS has_stat
    FROM %[1]s.stats_events
    %[2]s
    GROUP BY router_ip, peer_ip
),
locrib_cur AS (
    SELECT collector_id, router_ip, max(sid) AS sid
    FROM (
        SELECT collector_id, router_ip, sid FROM cur
        UNION ALL
        SELECT collector_id, router_ip, max(session_id) AS sid
        FROM %[1]s.route_unicast_current
        %[7]s
        GROUP BY collector_id, router_ip
    )
    GROUP BY collector_id, router_ip
),
archived AS (
    SELECT router_ip, peer_ip, uniqExact((%[6]s)) AS archived
    FROM (
        SELECT router_ip, peer_ip, %[6]s
        FROM %[1]s.route_unicast_current
        %[3]s
        GROUP BY %[4]s
        HAVING argMax(is_withdraw, (seq, stream_seq)) = 0
    )
    GROUP BY router_ip, peer_ip
),
peers AS (
    SELECT router_ip, peer_ip FROM reported
    UNION DISTINCT
    SELECT router_ip, peer_ip FROM archived
)
SELECT
    peers.router_ip                AS router_ip,
    peers.peer_ip                  AS peer_ip,
    coalesce(reported.reported, 0) AS reported,
    coalesce(archived.archived, 0) AS archived,
    coalesce(reported.has_stat, 0) AS has_stat,
    count() OVER ()                AS total_matched
FROM peers
LEFT JOIN reported
    ON reported.router_ip = peers.router_ip AND reported.peer_ip = peers.peer_ip
LEFT JOIN archived
    ON archived.router_ip = peers.router_ip AND archived.peer_ip = peers.peer_ip
ORDER BY coalesce(archived.archived, 0) DESC, peers.router_ip, peers.peer_ip
LIMIT %[5]d`

// LocRIBComparison reports, per (router, peer), what the router's newest BMP
// Stats Report claims its Loc-RIB holds against how many route_unicast
// Loc-RIB rows (rib = 'loc_rib') collection currently has archived for that
// peer.
//
// See PeerLocRIB for what Reported, Archived and HasStat each mean, and
// above all what HasStat guards against: without it, a peer that has never
// sent stat type 8 reports Reported = 0 indistinguishably from a peer whose
// Loc-RIB really is empty, turning a collection gap into a fabricated one.
//
// A LOC-RIB GAP IS NOT PROOF OF LOSS. Archived = 0 against a non-zero
// Reported most often means RFC 9069 Loc-RIB monitoring is simply not
// enabled for that peer -- a router capability distinct from ordinary
// adj-RIB-in route monitoring -- not that collection failed to store
// something the router sent. Measured by running this exact comparison
// against one archive on 2026-09-11: 21 of the 25 (router, peer)
// pairs it returns read Archived = 0 against Reported > 0. It is 25, not
// the 26 (router, peer) pairs stats_events holds overall, because one pair
// sends stats_events rows but never one carrying counter 8 AND has no
// route_unicast rows under rib = 'loc_rib' either -- it has nothing this
// comparison can say anything about, on either side, so it is absent from
// the output entirely rather than appearing as a 26th row. See PeerLocRIB's
// own doc comment for why Archived is scoped to rib = 'loc_rib' at all,
// rather than counting every route_unicast row collection holds for the
// peer regardless of RIB view.
//
// THE COMPARISON IS UNICAST-ONLY. See PeerLocRIB's own doc comment for why
// route_vpn and route_evpn are not summed in.
//
// f.Since SCOPES THE REPORTED SIDE ONLY. The window bounds the stats_events
// read and never the route_unicast one, so Archived is what collection
// holds for the peer in its router's current session, whole, at every
// window this endpoint offers. That is a contract statement and not an
// implementation detail: Reported is a snapshot and Archived is a stock, and
// windowing the stock would compare an accumulation against a snapshot and
// render a Loc-RIB gap that is an artifact of the window. See
// PeerLocRIB.Archived for the measurement that says how badly -- on one
// measured archive every window the Monitor screen offers would have read
// Archived = 0 for every peer --
// and for the read cost of leaving the archived side unbounded.
//
// The stats_events and route_unicast WHERE clauses are built from two
// SEPARATE filters values, not one reused the way DumpCounts reuses a single
// where across three identically-shaped tables: the two clauses here are not
// identical text -- stats_events' carries an extra `AND has(counters, 8)`
// and route_unicast's carries an extra `AND rib = 'loc_rib'`, and only
// stats_events' carries f.Since at all -- so each is bound against its own
// values() in its own rendered order, rather than one set of values spliced
// into two different places (see filters.values' own note on why that
// splicing is the caller's responsibility to get right).
//
// The uint64 return is total_matched -- see DumpCounts' own doc comment for
// what it means and 0 when the result is empty.
func (q *Q) LocRIBComparison(ctx context.Context, f CollectionFilter) (rows []PeerLocRIB, total uint64, err error) {
	var statsW filters
	statsW.eqAddr("router_ip", f.Router)
	statsW.since("ts_collector", f.Since)
	statsW.conds = append(statsW.conds, "has(counters, 8)")
	statsWhere := statsW.clause()

	// NO since ON THE ARCHIVED SIDE. f.Since scopes the stats half only --
	// see this function's own doc comment for why, and PeerLocRIB.Archived
	// for what the resulting column therefore means.
	//
	// ONE SESSION PER (collector, router) ON THE ARCHIVED SIDE. The IN below
	// is what keeps the withdrawal argMax inside one session -- see
	// locRIBComparisonSQL's own doc comment for why it has to be.
	var archW filters
	archW.eqAddr("router_ip", f.Router)
	archW.conds = append(archW.conds, "rib = 'loc_rib'",
		"(collector_id, router_ip, session_id) IN (SELECT collector_id, router_ip, sid FROM locrib_cur)")
	archWhere := archW.clause()

	// locrib_cur's own read of the loc_rib rows' newest session, scoped to
	// the same router so it prunes on route_unicast_current's primary key
	// rather than reading every router's rib column.
	var curW filters
	curW.eqAddr("router_ip", f.Router)
	curW.conds = append(curW.conds, "rib = 'loc_rib'")
	curWhere := curW.clause()

	stmt := fmt.Sprintf(locRIBComparisonSQL,
		q.db, statsWhere, archWhere, unicastRouteIdentity, clampRIBLimit(f.Limit),
		locRIBRouteKeyWithinPeer, curWhere)

	// Bound in the order the placeholders appear in the text: reported,
	// then locrib_cur, then archived. The driver binds strictly left to
	// right.
	args := make([]any, 0, len(statsW.values())+len(curW.values())+len(archW.values()))
	args = append(args, statsW.values()...)
	args = append(args, curW.values()...)
	args = append(args, archW.values()...)

	res, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query loc-rib comparison: %w", err)
	}
	defer res.Close()

	var out []PeerLocRIB
	for res.Next() {
		var r PeerLocRIB
		var hasStat uint8
		if err := res.Scan(&r.RouterIP, &r.PeerIP,
			&r.Reported, &r.Archived, &hasStat, &total); err != nil {
			return nil, 0, fmt.Errorf("scan loc-rib comparison: %w", err)
		}
		r.HasStat = hasStat == 1
		unmapAll(&r.RouterIP, &r.PeerIP)
		out = append(out, r)
	}
	return out, total, res.Err()
}

// FlagCount is one parse flag's share of what collection decoded inside f's
// window: the flag's own name, and how many ENVELOPES raised it. See
// FlagCounts for what an envelope is and why it, not a row, is the unit
// counted.
type FlagCount struct {
	Flag      string
	Envelopes uint64
}

// flagStreamTables pairs each of the ten tables that carry a parse_flags
// column with the JetStream stream its own envelopes travel on -- see
// cmd/vantage-writer/main.go's consumer list and sink/consumer.go's
// allowedStreams for the four streams themselves (ROUTES, LS, PEER, STATS).
// stream_seq is that STREAM's own sequence number, assigned once per
// message by JetStream, so the same stream_seq value means nothing at all
// compared across two tables on different streams -- it is only (stream,
// stream_seq) TOGETHER that names one envelope, which is exactly the pair
// flagEnvelopeSQL tags every exploded row with and flagCountsSQL's
// uniqExact counts. This is fleet-health's own "Parse flags" panel's
// grouping (deploy/grafana/dashboards/fleet-health.json), reproduced here
// rather than invented: eor_events travels on ROUTES beside the three BGP
// route tables (an End-of-RIB marker closes out the same UPDATE stream the
// routes it marks arrived on), and the four BGP-LS object tables all share
// LS.
//
// replacing reports whether that table is a ReplacingMergeTree. Every entry
// is except eor_events -- see flagEnvelopeDistinctSQL.
//
// TestFlagCountsReadsEveryFlaggedTable flags a distinguishable row in each
// of these ten tables and asserts every one comes back through FlagCounts,
// which is what actually proves this list is complete -- a table silently
// dropped from it is a table this behavioral check catches, not only a
// panel-text comparison this package does not also keep.
var flagStreamTables = []struct {
	stream, table string
	// replacing is true for the ReplacingMergeTree tables, which this
	// statement reads plainly. It is false only for eor_events, a plain
	// MergeTree whose redeliveries nothing collapses, read through a
	// DISTINCT wrapper instead. The field was called `final` until
	// 2026-09-19, when FINAL left this statement and the name stopped
	// describing what it selects.
	replacing bool
}{
	{"ROUTES", "route_unicast", true},
	{"ROUTES", "route_vpn", true},
	{"ROUTES", "route_evpn", true},
	{"ROUTES", "eor_events", false},
	{"LS", "ls_events", true},
	{"LS", "ls_nodes", true},
	{"LS", "ls_links", true},
	{"LS", "ls_prefixes", true},
	{"STATS", "stats_events", true},
	{"PEER", "peer_events", true},
}

// flagEnvelopeSQL renders one ReplacingMergeTree table's contribution to the
// parse-flags union: every parse flag any of its rows carries inside the
// window, exploded one flag per row by arrayJoin (a row's parse_flags is an
// Array(LowCardinality(String)), and a row raising two flags must count
// toward both, not toward one manufactured "flag pair"), tagged with the
// stream its own envelopes travel on and the stream_seq that names which
// envelope raised it -- see flagStreamTables.
//
// NO FINAL, in keeping with this package's own "No FINAL anywhere" rule (see
// query.go's package doc comment). flagCountsSQL's outer
// uniqExact((stream, stream_seq)) is what collapses a redelivered envelope:
// the duplicate carries the SAME stream_seq, so it produces the identical
// tuple and uniqExact counts it once, merged or not.
//
// THESE NINE TABLES READ FINAL until 2026-09-19, on the stated grounds of
// "fidelity to the panel this statement is required to reproduce exactly,
// which chose FINAL for all nine of these tables". A 2026-09-19 change dropped
// FINAL from all TEN arms of fleet-health's "Parse flags" panel, having
// measured that the panel's own uniqExact was doing the dedup and FINAL was
// merely masking it -- the same outer aggregate this statement feeds. That
// change swept the dashboards for prose it had falsified and corrected a
// sentence; it did not sweep this package, whose comments say it reproduces
// those dashboards, and nine tables went out of sync in the same keystroke.
//
// TestFlagCountsIsCorrectWithUnmergedDuplicatesPresent is the evidence rather
// than the reasoning above: merges disabled, one envelope written twice
// byte-identical, the two raw rows asserted still present, and Envelopes
// asserted to read 1. Mutation-checked with FINAL absent -- uniqExact
// weakened to count() makes it read 2.
//
// eor_events remains the exception, and not a stylistic one: it is a plain
// MergeTree ClickHouse refuses FINAL on outright, and is read through a
// DISTINCT wrapper instead -- see flagEnvelopeDistinctSQL and
// TestEorEventsRefusesFinal.
//
// %[1]s is the stream label, %[2]s the database, %[3]s the table, %[4]s the
// WHERE clause -- filters-rendered, exactly as DumpCounts, SessionCounts and
// LocRIBComparison render their own, rather than the dashboard's
// $__timeFilter/$rib macros: CollectionFilter carries no rib dimension, and
// its own Since and Router replace $__timeFilter and the router scope
// renderDashboardSQL's own test harness substitutes for it (see
// collection_test.go).
const flagEnvelopeSQL = `
    SELECT '%[1]s' AS stream, stream_seq, arrayJoin(parse_flags) AS flag
    FROM %[2]s.%[3]s
    %[4]s`

// flagEnvelopeDistinctSQL is flagEnvelopeSQL's eor_events shape.
//
// eor_events is a plain MergeTree, not ReplacingMergeTree (see schema.sql's
// own comment on why: a redelivered marker's count() is wrong PERMANENTLY
// under MergeTree rather than intermittently-then-correct under
// ReplacingMergeTree, which is the choice this project wants for a table
// nothing may count rows of). ClickHouse refuses FINAL on it outright --
// TestEorEventsRefusesFinal runs the refused statement itself and records
// ClickHouse's own error text, rather than asserting this from the engine's
// documentation.
//
// So this reads through `(SELECT DISTINCT * FROM ...)` instead, which is
// fleet-health's own panel's shape for this one branch, reproduced for
// fidelity with the panel this whole statement is extracted from, and NOT
// because this particular count needs it. Read carefully, because the
// obvious claim here is the wrong one: DISTINCT is not what keeps a
// JetStream redelivery of eor_events from being double-counted.
// flagCountsSQL's own uniqExact((stream, stream_seq)) already does that --
// two byte-identical redelivered rows produce the identical
// (stream, stream_seq, flag) tuple once arrayJoin runs, and uniqExact
// counts DISTINCT tuples regardless of how many times each one physically
// occurs, exactly as it would if this branch read the raw table with no
// wrapper at all. So this SELECT DISTINCT is defensive, not load-bearing,
// for the count FlagCounts actually computes: it protects a future
// SELECT * or per-row read of this branch that a reader might add later,
// and it keeps this branch's shape identical to fleet-health's own panel,
// but no test in this file exercises it being NECESSARY, because for this
// specific metric it is not. Do not read
// TestFlagCountsReadsEorEventsWithoutFinal's PASS as proof this wrapper
// matters; it is proof the REDELIVERY is handled, by uniqExact, not proof
// of which clause handles it.
//
// %[1]-[4] are flagEnvelopeSQL's own.
const flagEnvelopeDistinctSQL = `
    SELECT '%[1]s' AS stream, stream_seq, arrayJoin(parse_flags) AS flag
    FROM (SELECT DISTINCT * FROM %[2]s.%[3]s)
    %[4]s`

// flagCountsSQL folds all ten tables' branches into one row per flag.
//
// uniqExact((stream, stream_seq)), NOT count(). One UPDATE carrying fifty
// prefixes that all trip the same parse flag is ONE envelope, not fifty
// rows -- see FlagCounts' own doc comment for the measured stakes of
// getting this wrong. TestFlagCountsCountsEnvelopesNotRows pins the
// direction with a fixture built so the two numbers disagree by
// construction rather than by chance.
//
// ORDER BY envelopes DESC, flag -- this package's usual busiest-first
// ordering with a deterministic tie-break, exactly as dumpCountsSQL and
// sessionCountsSQL order by their own metric then router_ip. flag is
// already the GROUP BY key, so no two rows can tie on it; the tie-break
// exists for the SAME reason it does on every other statement in this
// file: readable, reproducible output ordering when two flags happen to
// carry the same envelope count, not correctness.
//
// LIMIT %[11]d, through clampRIBLimit, exactly as DumpCounts, SessionCounts
// and LocRIBComparison cap their own answers -- CollectionFilter.Limit is
// reused unchanged rather than this function inventing a fourth cap. In
// practice it is close to dormant here: bgp/'s own PARSE_FLAG_* vocabulary is
// on the order of ten literals, and one production archive raised six of them
// (measured 2026-09-11) -- nowhere near clampRIBLimit's own default of 1,000
// rows, unlike DumpCounts' and SessionCounts' router lists or
// LocRIBComparison's peer list, which a real fleet can approach. Kept
// anyway: it costs nothing, and holding the same contract as the other
// three signals is worth more than the few bytes a signal-specific
// carve-out would save.
//
// total_matched is count() OVER () -- see dumpCountsSQL's own doc comment for
// what it computes and why it costs nothing extra to carry. Measured here
// specifically, because this is api/'s own most expensive signal (the
// ten-table union): run against a production archive, unscoped,
// best-of-three, this column changed read_rows and read_bytes NOT AT ALL
// (45,008 rows / 2,124,774 bytes, identical to the byte, with or without it)
// and left query_duration_ms and memory_usage within ordinary run-to-run
// noise (6-7ms either way; memory actually read LOWER with the column present
// across all three runs, 441-534 KiB vs 665-754 KiB, which is noise rather
// than a real saving). A window function over a result set the GROUP BY
// already built adds one more pass over rows already in memory, not a new
// scan -- which is exactly what this measured. dumpCountsSQL's own doc
// comment carries the same measurement for the three-table dump
// classification.
//
// %[1]-[10] are the ten rendered branches, in flagStreamTables' order.
const flagCountsSQL = `
SELECT flag, uniqExact((stream, stream_seq)) AS envelopes, count() OVER () AS total_matched
FROM (
%[1]s
    UNION ALL
%[2]s
    UNION ALL
%[3]s
    UNION ALL
%[4]s
    UNION ALL
%[5]s
    UNION ALL
%[6]s
    UNION ALL
%[7]s
    UNION ALL
%[8]s
    UNION ALL
%[9]s
    UNION ALL
%[10]s
)
GROUP BY flag
ORDER BY envelopes DESC, flag
LIMIT %[11]d`

// FlagCounts reports, fleet-wide, how often the decoder raised each parse
// flag inside f's window -- across every one of the ten tables that carry a
// parse_flags column (see flagStreamTables), read the way fleet-health's own
// "Parse flags" panel reads them (deploy/grafana/dashboards/
// fleet-health.json), not retyped from scratch.
//
// COUNTS ENVELOPES, NOT ROWS. A row is one route, node, link, prefix or
// event that a single BMP UPDATE or notification exploded into; an envelope
// is that ONE message. A truncated-attribute UPDATE carrying fifty prefixes
// that all trip PARSE_FLAG_VERSION_UNPARSED is one parse event, not fifty,
// and measured against one archive on 2026-09-11 the gap between
// the two readings is not academic:
//
//	flag                          envelopes   rows   ratio
//	PARSE_FLAG_LS_NLRI_UNDECODED        117   1213   10.4x
//	PARSE_FLAG_LS_TLV_UNKNOWN          2626   4291    1.6x
//	PARSE_FLAG_VERSION_UNPARSED        1530   1699    1.1x
//
// Counting rows would overstate PARSE_FLAG_LS_NLRI_UNDECODED TENFOLD. See
// flagCountsSQL's own uniqExact((stream, stream_seq)) and
// TestFlagCountsCountsEnvelopesNotRows.
//
// READS EVERY TABLE THAT CARRIES parse_flags -- TEN of them, not the three
// route tables this package's other three collection-health signals read.
// fleet-health's own panel is the cautionary tale for reading fewer: an
// earlier cut of it "reported 2 occurrences of a condition that had fired
// 43 times fleet-wide", and two of the ten (ls_prefixes, eor_events) were
// missing from that panel until a test caught them. See
// TestFlagCountsReadsEveryFlaggedTable, which flags a distinguishable row
// in each of the ten and asserts every one of the ten comes back, so that
// dropping ANY single table from the union -- not only the one this
// project already got wrong twice -- fails a test rather than silently
// under-reporting.
//
// PARSE FLAGS ARE HISTORICAL, and that is a rule the UI this signal feeds
// must not violate, not merely a fact about the archive. A parse_flags
// value records the decoder as it stood AT THE MOMENT THE ROW WAS WRITTEN.
// Nothing in this schema records which build of the collector wrote a row
// (see schema.sql -- no version or build column exists on any of the ten
// tables), so a flag counted inside f's window may describe a decoder bug
// that was fixed before the window even opened. This project has already made
// the opposite mistake once: 36 PARSE_FLAG_UNKNOWN_FAMILY rows were read as a
// live defect that had in fact been repaired weeks earlier. "Is this flag
// still firing" is answerable only by widening the window to see whether it
// recurs, never by this count alone, and never by treating an old count as
// proof of a current one. A caller rendering FlagCounts on the Monitor
// screen must carry that reading forward rather than presenting a count as
// a diagnosis.
//
// ROUTER'S ROLE HERE, stated because it is not the same role it plays in
// DumpCounts, SessionCounts or LocRIBComparison, and left implicit would be
// a decision nobody could see was made. f.Router DOES narrow this query --
// every one of the ten tables carries its own router_ip column, and it is
// rendered into every branch's WHERE exactly as DumpCounts and
// SessionCounts render theirs, so ?router= answers "how often did THIS
// router's decoder raise each flag", which is a real and useful narrowing
// (api/'s own contract documents ?router= narrowing all four
// collection-health endpoints alike). What Router does NOT do is become a
// dimension of the OUTPUT: FlagCount carries no RouterIP, unlike
// RouterDumpCount and RouterSessionCount, which return one row PER ROUTER.
// That is deliberate rather than an oversight, for a structural reason a
// per-router GROUP BY would paper over: stream_seq is the JetStream
// STREAM's own sequence number, assigned once per message across the WHOLE
// fleet publishing to that stream (see flagStreamTables) -- it identifies
// an envelope, never a router's own position in anything, so "envelopes
// per (router, flag)" is not a finer-grained reading of the same number
// this function already returns, it is a different question this package
// does not answer today. Grouping by flag alone, fleet-wide, also matches
// fleet-health's own panel exactly, which carries no per-router dimension
// at all. A caller that needs one router's own share of a fleet-wide flag
// spike gets it by calling FlagCounts once scoped (f.Router set) and once
// unscoped and comparing the two totals, not by reading a column this type
// does not have.
//
// # Cost, and why "the expensive one" is the wrong label
//
// This endpoint was assumed to be the expensive one on the strength of the
// ten-table union, and flagCountsSQL's own comment carries a measurement of
// a different question (whether count() OVER () adds anything: it does
// not). Neither measured what the endpoint costs. Measured 2026-09-17
// through THIS function, query_id-tagged and read back out of
// system.query_log, best of three:
//
//   - One measured archive, all ten legs populated, 6 distinct flags over
//     1,853 flagged rows: unbounded, 19,601 read_rows / 477,194 read_bytes /
//     3-4 ms / 465-749 KiB. At 1h, 6h and 24h alike, 17,152 read_rows /
//     411,648 read_bytes / 3-4 ms -- 88% of the unbounded read, for a window
//     whose answer is empty.
//   - ribload, 1,500,644 route_unicast rows on this union's heaviest leg:
//     unbounded, 1,500,646 read_rows / 24,010,469 read_bytes / 6-7 ms /
//     239-639 KiB.
//
// So the union is cheap in the two resources that bound a daemon -- 1.5M
// rows in 6-7 ms under 1 MiB -- and dumps, at 21 ms / 101 MiB for
// route_unicast alone, is the expensive signal of the three that
// have been measured. Not of the four: locrib is still classified as "one
// join," with no number behind it. The
// flag vocabulary is what keeps this small: bgp/'s PARSE_FLAG_* literals
// number about ten, so arrayJoin explodes into a handful of groups no
// matter how much it reads.
//
// Two caveats the numbers do not carry on their own. ribload's rows hold no
// parse_flags at all, so its 1.5M-row figure bounds the READ and the FINAL
// merge, not the aggregation a flag-heavy fleet would add on top. And the
// window does not bound the read: ts_collector is in the partition key
// (toYYYYMM) and in no sorting key, so `since` prunes whole parts by
// min/max and nothing finer -- which is exactly why the 1h read above is
// 88% of the unbounded one (one stats_events part straddles every window
// the screen offers and is read in full) while ribload's single 12-day-old
// part prunes to 0 read_rows at all three windows. See
// SessionCounts' own cost note: this is a property of the schema, not of
// these two statements.
//
// The uint64 return is total_matched -- see DumpCounts' own doc comment for
// what it means and 0 when the result is empty.
func (q *Q) FlagCounts(ctx context.Context, f CollectionFilter) (rows []FlagCount, total uint64, err error) {
	var w filters
	w.eqAddr("router_ip", f.Router)
	w.since("ts_collector", f.Since)
	where := w.clause()

	branches := make([]any, 0, len(flagStreamTables)+1)
	for _, t := range flagStreamTables {
		tmpl := flagEnvelopeSQL
		if !t.replacing {
			tmpl = flagEnvelopeDistinctSQL
		}
		branches = append(branches, fmt.Sprintf(tmpl, t.stream, q.db, t.table, where))
	}
	branches = append(branches, clampRIBLimit(f.Limit))

	stmt := fmt.Sprintf(flagCountsSQL, branches...)

	// The same WHERE is rendered once per table -- ten times -- so its
	// values are bound ten times, in the order the ten branches appear.
	// See DumpCounts' own comment on this exact pattern at a third of the
	// width.
	args := make([]any, 0, len(flagStreamTables)*len(w.values()))
	for range flagStreamTables {
		args = append(args, w.values()...)
	}

	res, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query flag counts: %w", err)
	}
	defer res.Close()

	var out []FlagCount
	for res.Next() {
		var r FlagCount
		if err := res.Scan(&r.Flag, &r.Envelopes, &total); err != nil {
			return nil, 0, fmt.Errorf("scan flag count: %w", err)
		}
		out = append(out, r)
	}
	return out, total, res.Err()
}
