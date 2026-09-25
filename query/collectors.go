package query

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// CollectorRouter is one router as ONE collector currently sees it: the
// same per-(collector, router) facts Router itself carries, minus
// SessionID and Collector -- both redundant once this row already sits
// inside a CollectorSummary keyed on the collector, and Collectors has no
// caller that needs a session id the way Routers' own callers do.
//
// SysDescr is the router's Initiation sysDescr TLV, exactly as Peer.SysDescr
// documents it, rolled up from whichever of the router's peers actually
// carried a non-empty one most recently -- see collectorsSQL's own doc
// comment for why that rollup has to filter on non-empty before it orders
// by recency, and TestCollectorsSysDescrSurvivesALaterEventCarryingNone for
// what happens when it does not. Empty means never observed, not backfilled
// or defaulted; the "not observed" rendering is a caller's job.
//
// IP is always in plain form, never IPv4-mapped -- see unmapAll's own doc
// comment for why that is not automatic.
//
// PeersUp + PeersDown + PeersViewLost + PeersStale is a bound on this
// router's peer count, not an identity, for the identical reason Router's
// own doc comment gives: a peer whose state resolved 'unspecified' is in
// none of the four.
//
// LastSeen is Router.LastSeen's own fact -- the newest peer_events row in
// THIS router's current session -- not CollectorSummary.LastRowAt, which is
// the newest row the collector wrote to any table at all, scoped to no
// session and no router. A router whose peers have been quiet for hours
// while its collector keeps writing route or LS updates for OTHER routers
// has an old LastSeen here and a fresh LastRowAt on its own
// CollectorSummary; the two answer different questions and neither is a
// looser version of the other.
type CollectorRouter struct {
	SysName       string
	IP            netip.Addr
	PeersUp       int
	PeersDown     int
	PeersViewLost int
	PeersStale    int
	SysDescr      string
	LastSeen      time.Time
}

// CollectorSummary is one collector's whole current view of the archive:
// every router it currently monitors, its peers rolled up into one total
// per state, and when it last wrote anything to the archive at all.
//
// PeersUp, PeersDown, PeersViewLost and PeersStale are the SUM of the
// identical fields across every CollectorRouter in Routers -- see
// Collectors' own doc comment for why that roll-up happens in Go rather than
// in a second layer of SQL aggregation, and TestCollectorsSumsPeerCountsAcrossAllItsRouters
// for the guard that it is a sum and not, say, the first router's count
// repeated.
//
// A router two collectors both monitor appears once under EACH collector's
// own CollectorSummary -- not merged, and not picked between -- the same
// answer Router.Collector gives at the row grain Routers itself works at;
// see that field's own doc comment for why, and
// TestCollectorsGroupsRoutersUnderTheirOwnCollector for this grain's own
// version of the same guard.
//
// LastRowAt is the newest ts_collector this collector has written to ANY
// of the archive's ten data tables, not merely to peer_events, read from
// the history tables and the current-state tables together so it survives
// history retention -- see
// collectorsSQL's own doc comment for why peer_events alone reports a
// briskly-collecting collector as dead the moment its BGP sessions have
// simply had nothing new to say.
type CollectorSummary struct {
	Collector     string
	Routers       []CollectorRouter
	PeersUp       int
	PeersDown     int
	PeersViewLost int
	PeersStale    int
	LastRowAt     time.Time
	// LastBeatAt is when the archive last received this collector's
	// heartbeat, on the archive's clock -- the instant staleness is measured
	// from. StartedAt is its newest process start, on its own clock. Both
	// are nil when no heartbeat was ever received, which is not the same as
	// an old one.
	LastBeatAt *time.Time
	StartedAt  *time.Time
}

// collectorsSQL rolls routersSQL's own per-(collector, router) shape up one
// level, to the collector itself, and answers two questions Routers never
// had to: what a router's peers denormalize as their sys_descr when its
// peers disagree about carrying one, and when the collector last wrote
// anything at all.
//
// router_state and router_peers are routersSQL's own two aggregations --
// see that statement's doc comment for why the peer counts have to come
// from peer_up rather than from peer_state directly (peer_state's grain is
// one row per RIB VIEW of a peer, not one row per peer, so counting it
// double-counts every peer a router mirrors pre- and post-policy), and for
// why any(sysname) is safe under the same "cur and this GROUP BY agree"
// argument. This statement drops any(sid): a collector card has no single
// session to report the way a router card does.
//
// sys_descr is argMaxIf(sys_descr, last_seen, sys_descr not empty), never a
// plain argMax. peer_state's own sys_descr column (see peerStateCTE) is
// already the per-(peer, rib) resolution of "the newest event that
// actually carried one", but a router with more than one peer can still
// feed router_state two peer_state rows that disagree: one peer's
// Initiation told the collector the router's sys_descr, another peer's own
// most recent event carried none at all. A plain argMax over those two
// rows picks whichever has the LATER last_seen regardless of which one
// actually says something, so a router's own well-known identity reads
// "not observed" the moment its quietest peer happens to be the most
// recent. argMaxIf filters to rows that carry a value FIRST, and only
// orders by recency among those that do -- see
// TestCollectorsSysDescrSurvivesALaterEventCarryingNone, whose fixture
// manufactures exactly two such disagreeing peer_state rows and fails
// (reads "" instead of the router's real descr) the moment this argMaxIf
// is swapped back to argMax.
//
// last_row is why this is a new statement rather than routersSQL with a
// column removed: a collector's liveness cannot be read off peer_events
// alone. Nothing about BMP requires a peer session to flap just because
// route updates or LS objects are still arriving, so a collector can be
// reading either kind of traffic briskly while its newest peer up/down
// event sits hours in the past -- a query scoped to peer_events would call
// that collector dead. This subquery unions all TEN history tables that
// carry both collector_id and ts_collector, confirmed against
// system.columns: peer_events, route_unicast, route_vpn, route_evpn,
// eor_events, ls_events, ls_nodes, ls_links, ls_prefixes and stats_events
// -- the identical set collection.go's own flagAllTables names, for the
// identical reason (a table silently missing from either list is a table
// whose freshest write goes unseen). See
// TestCollectorsLastRowScansEveryDataTable, which guards the SQL text
// directly, and
// TestCollectorsLastRowAtIsTheNewestAcrossEveryTableThisCollectorWrites,
// which proves it behaviorally against ls_nodes -- one of the three tables
// an earlier, seven-table draft of this statement omitted.
//
// It also unions the SEVEN current-state tables that carry ts_collector:
// peer_current, route_unicast_current, route_vpn_current,
// route_evpn_current, ls_nodes_current, ls_links_current and
// ls_prefixes_current. eor_current is the eighth current table and is not
// here because it has no ts_collector column; it cannot add a collector
// either, since every collector router_state produces has a peer_current
// row. The current tables are what keep the join below sound. router_state
// is built from peer_current, which has no TTL, while the ten history
// tables all expire. A collector whose session has been quiet for longer
// than retention has no history row left at all, yet its session, routes
// and LS objects are still current, and an INNER JOIN against a
// history-only last_row dropped it from Collectors entirely. peer_current
// is one of the arms, so every collector_id router_state can produce has a
// last_row row, and the join stays INNER. See
// TestStateOlderThanRetentionIsStillCurrent.
//
// last_row_at stays a real timestamp in that case: the newest ts_collector
// the archive still holds for the collector, which is the newest row it
// ever wrote (a current row is a copy of a history row, made when that row
// was inserted). It is old, which is the truth about a quiet collector, not
// a stand-in for a missing value. While history still holds a collector's
// rows, the current arms add no newer timestamp than the history arms
// already report.
//
// It groups by collector_id alone, not by router: a collector's liveness is
// not scoped to any one router it happens to also be monitoring peers on.
const collectorsSQL = "WITH " + peerStateCTE + peerUpCTE + `,
router_state AS (
    SELECT
        collector_id,
        router_ip,
        any(sysname)                                   AS sysname,
        argMaxIf(sys_descr, last_seen, sys_descr != '') AS sys_descr,
        -- Aliased seen_at, not back to last_seen: peer_state's own
        -- last_seen is exactly what argMaxIf above reads as its ordering
        -- argument, and peerStateCTE's own doc comment already names this
        -- hazard at length -- ClickHouse resolves a SELECT alias inside
        -- that same query's other expressions, so max(last_seen) aliased
        -- back to last_seen, sitting beside argMaxIf(sys_descr, last_seen,
        -- ...), rewrites that second argument to the aggregate output
        -- rather than the peer_state input column, and ClickHouse rejects
        -- the result outright: 'Aggregate function max(last_seen) AS
        -- last_seen is found inside another aggregate function.' Caught by
        -- running this statement, not by inspection.
        max(last_seen)                                  AS seen_at
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
),
last_row AS (
    SELECT collector_id, max(ts_collector) AS last_row_at
    FROM (
        SELECT collector_id, ts_collector FROM %[1]s.peer_events
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_unicast
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_vpn
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_evpn
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.eor_events
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_events
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_nodes
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_links
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_prefixes
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.stats_events
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.peer_current
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_unicast_current
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_vpn_current
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_evpn_current
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_nodes_current
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_links_current
        UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_prefixes_current
    )
    GROUP BY collector_id
)
SELECT
    rs.collector_id,
    rs.sysname,
    rs.router_ip,
    rp.peers_up,
    rp.peers_down,
    rp.peers_view_lost,
    rp.peers_stale,
    rs.sys_descr,
    rs.seen_at,
    lr.last_row_at,
    lv.beat_seen,
    lv.last_beat,
    lv.epoch_at
FROM router_state rs
INNER JOIN router_peers rp
    ON rp.collector_id = rs.collector_id
   AND rp.router_ip    = rs.router_ip
INNER JOIN last_row lr
    ON lr.collector_id = rs.collector_id
INNER JOIN live lv
    ON lv.collector_id = rs.collector_id
ORDER BY rs.collector_id, rs.sysname`

// Collectors reports one CollectorSummary per collector the archive
// currently has any record of: every router that collector currently
// monitors (Routers' own answer, grouped one level higher), its peers
// rolled up into totals, its routers' sys_descr resolved so a peer whose
// own newest event carries none cannot erase what another peer already
// told the collector, and when the collector last wrote anything to the
// archive at all, across every table it might have written to rather than
// peer_events alone. See collectorsSQL for the statement this scans, and
// for why each of those needed a new query rather than a re-grouping of
// routersSQL's own.
//
// collectorsSQL returns one row per (collector, router); this rolls that
// into one CollectorSummary per collector, in Go, rather than asking
// ClickHouse to marshal the router list into one SQL value. The roll-up is
// a map lookup keyed on collector_id, so it is correct at ANY row order --
// contiguity is not what makes it safe. What the statement's own ORDER BY
// collector_id, sysname buys is a DETERMINISTIC answer rather than a
// correct one: collectors come back in the same order on every run, and
// each one's
// routers in the same order within it, so two answers can be compared.
func (q *Q) Collectors(ctx context.Context) ([]CollectorSummary, error) {
	rows, err := q.conn.Query(ctx, fmt.Sprintf(collectorsSQL, q.db))
	if err != nil {
		return nil, fmt.Errorf("query collectors: %w", err)
	}
	defer rows.Close()

	byCollector := map[string]*CollectorSummary{}
	var order []string
	for rows.Next() {
		var collector, sysname, sysDescr string
		var ip netip.Addr
		// countIf returns UInt64, and clickhouse-go's UInt64 column scans
		// only into *uint64 (or **uint64) -- there is no narrowing case for
		// *int in its ScanRow switch. See Routers' identical comment on the
		// same hazard.
		var up, down, viewLost, stale uint64
		var lastSeen, lastRowAt, lastBeat, epochAt time.Time
		var beatSeen uint8
		if err := rows.Scan(
			&collector, &sysname, &ip, &up, &down, &viewLost, &stale, &sysDescr, &lastSeen, &lastRowAt,
			&beatSeen, &lastBeat, &epochAt,
		); err != nil {
			return nil, fmt.Errorf("scan collector router: %w", err)
		}
		// router_ip is IPv6-typed and clickhouse-go's scan never unmaps an
		// IPv4-mapped address -- see unmapAll's own doc comment.
		unmapAll(&ip)

		cs, ok := byCollector[collector]
		if !ok {
			cs = &CollectorSummary{Collector: collector, LastRowAt: lastRowAt}
			if beatSeen == 1 {
				lb, sa := lastBeat.UTC(), epochAt.UTC()
				cs.LastBeatAt, cs.StartedAt = &lb, &sa
			}
			byCollector[collector] = cs
			order = append(order, collector)
		}
		cs.Routers = append(cs.Routers, CollectorRouter{
			SysName:       sysname,
			IP:            ip,
			PeersUp:       int(up),
			PeersDown:     int(down),
			PeersViewLost: int(viewLost),
			PeersStale:    int(stale),
			SysDescr:      sysDescr,
			LastSeen:      lastSeen,
		})
		cs.PeersUp += int(up)
		cs.PeersDown += int(down)
		cs.PeersViewLost += int(viewLost)
		cs.PeersStale += int(stale)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]CollectorSummary, 0, len(order))
	for _, c := range order {
		out = append(out, *byCollector[c])
	}
	return out, nil
}

// CollectorActivity is one collector's ROWS ARCHIVED in one minute of a
// requested window -- literally what count() sees over that collector's
// rows landing in that minute, nothing else.
//
// It is NOT a message count and NOT a rate, and neither word may attach to
// it anywhere downstream. One BMP route-monitoring message carrying 40
// NLRIs is 40 rows, so this series and a true messages-per-second rate are
// two different measurements of two different things, not the same number
// rescaled -- see CollectorActivity's own method doc comment (below) for
// why a rate is not something this archive can honestly produce at all.
// The Collector health screen's design mock originally labeled this
// sparkline "messages/s, last 30 min"; that label has been deliberately
// changed because it names a measurement this method does not make.
type CollectorActivity struct {
	// Minute is the bucket's START, aligned by ClickHouse's own
	// toStartOfMinute -- ChurnBucket.TS's own convention, for the identical
	// reason: a bar belongs at a round instant, not at the timestamp of
	// whichever row happened to land in it first.
	Minute time.Time

	// Rows is how many rows this collector archived in this minute, summed
	// across every table collectorActivitySQL unions. A minute with no
	// rows at all is Rows == 0, not a missing entry -- see
	// collectorActivitySQL's own doc comment for the WITH FILL shape that
	// makes that true, and CollectorActivity's method doc comment for why
	// a missing bucket would be the wrong answer here.
	Rows uint64
}

// collectorActivitySQL unions the SAME ten tables collectorsSQL's own
// last_row CTE does, in the same order, for the same reason: a collector's
// BGP sessions can sit quiet for half an hour while it keeps writing route
// or link-state updates for other peers, or filing periodic stats reports,
// and a series scoped to peer_events alone would draw that collector as
// flatlined while it is actually busy. This card shows LastRowAt right
// beside this series (see CollectorSummary), and the two have to answer
// the same "is this collector doing anything" question with the same
// definition of "anything" or they contradict each other on the same
// card -- LastRowAt saying "wrote something 4 seconds ago" beside a
// sparkline reading zero for 30 straight minutes because it was scoped to
// a table that collector simply is not using right now.
//
// NO FINAL, which is this package's own default (see this package's own
// doc comment in query.go) and this statement has no reason to be a fourth
// exception to it. FlagCounts needs FINAL because it counts ENVELOPES, and
// an un-merged duplicate row from a JetStream redelivery would inflate
// that count past what was actually decoded once. This statement counts
// ROWS -- the archive's own row count, un-merged duplicates included --
// which is exactly what the binding constraint on this series asks it to
// report, and exactly what a plain count() over an un-FINAL'd
// ReplacingMergeTree already gives for free. Forcing a merge on every
// poll of a sparkline that every collector card refreshes on its own
// schedule would be the wrong trade for a number this series does not
// need to be envelope-exact.
//
// toStartOfMinute(ts_collector), never ts_router: ts_collector is the
// collector's own clock (schema.sql's own note, quoted in query.go's
// package doc comment), and a series about what THIS COLLECTOR archived
// and when has to bucket by the clock that actually decided when the row
// landed in the archive.
//
// WITH FILL FROM/TO wrap their bind values in toStartOfMinute(
// fromUnixTimestamp64Micro(?)), not just fromUnixTimestamp64Micro(?) alone:
// omitting that wrap fails outright against ClickHouse 24.8 -- "Incompatible
// types of WITH FILL expression values with column type DateTime" -- caught
// by running this exact statement, not by inspection. toStartOfMinute(
// ts_collector), the ORDER BY column WITH FILL is filling, is a plain
// DateTime; fromUnixTimestamp64Micro(?) alone produces a DateTime64(6), one
// notch finer, and ClickHouse refuses to fill one type's column from
// another's bounds.
//
// ORDER BY collector_id ASC, minute ASC, with WITH FILL on minute alone
// and NOT on collector_id, is the shape that fills PER COLLECTOR rather
// than across all of them at once: ClickHouse fills over the ORDER BY,
// restarting the fill sequence every time a column carrying no WITH FILL
// of its own changes value, so collector_id here is the group break and
// minute is what actually gets filled -- one full FROM..TO run of
// zero-filled minutes for EACH collector, never bleeding a hole across the
// boundary into the next collector's own rows. Confirmed directly against
// this running project's ClickHouse 24.8 with a two-collector fixture
// before this statement was written (five minutes back for both
// collectors, one with a hole in the middle, one with a single row):
// both collectors came back with all five minutes, interior holes
// zero-filled, no bleed. Dropping collector_id from this ORDER BY
// collapses both collectors' rows into one interleaved timeline before
// WITH FILL ever runs -- verified separately, and it does not merely
// under- or over-count, it emits FEWER rows than either collector alone
// needs and attributes some of them to an empty collector_id, because the
// synthesized filler rows no longer belong to either group.
// TestCollectorActivityFillsEveryMinutePerCollector is the behavioral
// guard, and its own doc comment carries this mutation's exact output.
//
// %[1]s is the database; the twenty bind values that follow are this
// call's (start, end) pair, once per branch; the final two are the same
// pair again, for WITH FILL's own FROM/TO.
const collectorActivitySQL = `
SELECT collector_id, toStartOfMinute(ts_collector) AS minute, count() AS rows
FROM (
    SELECT collector_id, ts_collector FROM %[1]s.peer_events
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_unicast
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_vpn
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.route_evpn
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.eor_events
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_events
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_nodes
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_links
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.ls_prefixes
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
    UNION ALL SELECT collector_id, ts_collector FROM %[1]s.stats_events
        WHERE ts_collector >= fromUnixTimestamp64Micro(?) AND ts_collector < fromUnixTimestamp64Micro(?)
)
GROUP BY collector_id, minute
ORDER BY collector_id ASC, minute ASC
WITH FILL FROM toStartOfMinute(fromUnixTimestamp64Micro(?)) TO toStartOfMinute(fromUnixTimestamp64Micro(?)) STEP INTERVAL 1 MINUTE`

// collectorActivityBranches is how many UNION ALL arms collectorActivitySQL
// has -- the ten tables named in its own doc comment -- kept as a named
// constant rather than a bare 10 so the one place CollectorActivity repeats
// its bound window per branch cannot drift from the SQL text silently if a
// future edit adds or removes a branch without updating both.
const collectorActivityBranches = 10

// CollectorActivity reports rows-archived-per-minute, bucketed by minute,
// per collector, for the most recent window -- what a Collector health
// card's own sparkline draws. See CollectorActivity the TYPE (above) for
// the unit this counts and the two words ("messages", a rate) that must
// never attach to it, and collectorActivitySQL for which ten tables and
// why.
//
// A minute nothing landed in is a REAL ZERO, present in the series, not a
// missing bucket: a plain GROUP BY produces no row for a minute with no
// rows, and a sparkline drawn from that silently closes the gap -- a
// collector that stopped for ten minutes would render as one that never
// did. WITH FILL is what keeps that from happening; see
// collectorActivitySQL's own doc comment for the exact shape and the
// mutation that breaks it.
//
// The returned map has one entry per collector that archived AT LEAST ONE
// row somewhere in the window; each entry holds exactly window/1m entries
// (WITH FILL's own FROM..TO run), oldest minute first. A collector with
// NOTHING archived anywhere in the window -- not one row, in any of the ten
// tables, for the whole span asked for -- has no key at all: the same
// shape Collectors' own INNER JOIN gives a collector with no peer_events
// row ever (see collectorsSQL's own doc comment). This method answers
// "what did collectors that were doing something archive, minute by
// minute", not "list every collector this archive has ever heard of" --
// a caller that also wants a flat, all-zero line for a collector this map
// has no key for should get that collector's name from Collectors and
// treat a missing key here as exactly that: an entirely quiet window, not
// an error.
//
// window must be at least a minute -- this series has no finer grain than
// that, so a shorter window cannot be answered honestly, and a zero or
// negative one would ask WITH FILL to fill backwards or not at all. window
// need not be a whole number of minutes; a remainder shorter than a
// minute is silently dropped, matching the series' own one-minute grain
// rather than rounding up to a bucket count the caller did not ask for.
func (q *Q) CollectorActivity(ctx context.Context, window time.Duration) (map[string][]CollectorActivity, error) {
	if window < time.Minute {
		return nil, fmt.Errorf("%w: collector activity window must be at least a minute, got %s",
			ErrBadFilter, window)
	}
	buckets := int64(window / time.Minute)

	// end is the START of the NEXT minute after now, so the current,
	// possibly still-filling minute is itself the newest bucket rather than
	// being excluded for not having finished yet. start is exactly
	// `buckets` minutes before that, so the window holds precisely
	// window/1m buckets -- neither an extra one at the boundary nor one
	// short of it.
	end := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-time.Duration(buckets) * time.Minute)

	bound := []any{start.UnixMicro(), end.UnixMicro()}
	args := make([]any, 0, (collectorActivityBranches+1)*len(bound))
	for range collectorActivityBranches {
		args = append(args, bound...)
	}
	args = append(args, bound...)

	rows, err := q.conn.Query(ctx, fmt.Sprintf(collectorActivitySQL, q.db), args...)
	if err != nil {
		return nil, fmt.Errorf("query collector activity: %w", err)
	}
	defer rows.Close()

	out := map[string][]CollectorActivity{}
	for rows.Next() {
		var collector string
		var a CollectorActivity
		if err := rows.Scan(&collector, &a.Minute, &a.Rows); err != nil {
			return nil, fmt.Errorf("scan collector activity: %w", err)
		}
		out[collector] = append(out[collector], a)
	}
	return out, rows.Err()
}
