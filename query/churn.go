package query

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// ChurnFilter bounds one churn-over-time answer.
//
// Bucket is required and Since is effectively so: the pair decides how many
// rows come back, and ChurnBuckets refuses a combination that cannot be
// drawn (see maxChurnBuckets). That refusal is the whole reason this filter
// is not CollectionFilter -- the other four collection signals answer one
// row per router however wide the window is, and this one answers one row
// per bucket.
type ChurnFilter struct {
	// Since bounds the read inclusively against ts_collector, exactly as
	// CollectionFilter.Since does, and carries the same consequence for what
	// the answer MEANS: the classification runs inside the window, so a
	// route whose real session dump landed before Since has its earliest
	// in-window observation counted as a dump. Widening the window can move
	// a row from Readvertise to Dump. That is the semantics route-churn.json
	// already ships and TestDumpCountsAgreesWithTheDashboards pins.
	Since time.Time

	// Until bounds the read at the top, exclusively. Zero leaves it open.
	//
	// A caller issuing TWO reads about one window has to set it, or the two
	// are not about one window: both are bounded below at Since and open
	// above, so a row archived between them belongs to whichever read ran
	// later. /v1/collection/churn/peers does exactly that -- a ranking and a
	// per-peer series -- and the identity between them (every peer's bars
	// sum to its own readvertise + withdraw) is only true if both see the
	// same rows.
	Until time.Time

	// Collector narrows every churn answer to one collector's view.
	//
	// Empty is not "all collectors summed" -- that was the defect these
	// signals shipped with, and it made a peer's churn scale with how many
	// collectors happened to be watching its router. Empty means THE BEST
	// SINGLE VANTAGE POINT: the collector that saw the most, reported whole.
	// See churnByPeerSQL for why that is the answer rather than a
	// deduplicated union.
	//
	// "The most" is counted over the WHOLE WINDOW and per entity -- per
	// (router, peer) for ChurnByPeer, per prefix for ChurnByPrefix, per
	// router for ChurnBuckets -- never per bucket. churnBucketsSQL carries
	// the argument for the window half, which is the one that is not
	// obvious: a per-bucket choice does not deduplicate at all when the two
	// collectors' copies land in different buckets, which is the usual case
	// when they are not watching at the same times.
	Collector string

	// Bucket is the width of one bar. Required, at least a second, and
	// rendered into the statement as an integer count of seconds rather than
	// as a string, so nothing a caller types reaches the SQL.
	Bucket time.Duration

	// Router and Peer narrow the same way every other signal's do. Peer is
	// what makes this answer a peer's own chart rather than its router's.
	Router netip.Addr
	Peer   netip.Addr
}

// ChurnPeerActivity is one peer's bar: the changes it archived in one bucket.
//
// Changes, not rows -- see churnPeerActivitySQL. A bucket that held none is
// absent rather than present as zero, so a caller drawing these places them
// on an axis it already knows the bounds of.
type ChurnPeerActivity struct {
	RouterIP netip.Addr
	PeerIP   netip.Addr
	Bucket   time.Time
	Changes  uint64
}

// ChurnBucket is one bar: how many archived rows landed in this bucket, split
// into the three kinds route-churn.json's own panel separates.
//
// THE THREE ARE MUTUALLY EXCLUSIVE AND SUM TO EVERY ROW. A session dump is,
// by the classification's own definition, never a withdrawal (is_withdraw = 0
// is part of it), so a row is exactly one of dump, withdrawal, or
// re-advertisement.
//
// They are NOT DumpCounts' two. That function's Changes is everything that is
// not a dump, which includes withdrawals; here a withdrawal is its own
// series, because a chart that folded withdrawals into re-advertisements
// would draw a table being emptied and a table being refreshed as the same
// bar. Anything comparing the two must add Readvertise and Withdraw before
// comparing against Changes.
type ChurnBucket struct {
	// TS is the bucket's START, aligned to a multiple of Bucket by
	// ClickHouse's own toStartOfInterval -- not the timestamp of the first
	// row in it, which would put a bar wherever a router happened to speak.
	TS time.Time

	// Readvertise is a route re-sent inside a session that had already sent
	// it: the network doing something, as opposed to the session restarting.
	Readvertise uint64

	// Withdraw is a withdrawal, whether or not it is the first thing seen
	// for that route in its session. Nothing synthesizes a withdrawal -- a
	// router re-dumping its table sends what it has, not what it does not --
	// so a withdrawal is always a change and never a dump.
	Withdraw uint64

	// Dump is the first non-withdrawal observation of a route inside its
	// session: the session re-sending what it already knew. A DUMP IS NOT A
	// FAILURE; see RouterDumpCount for the same warning at more length.
	Dump uint64
}

// maxChurnBuckets is the most bars one answer may carry.
//
// 1,500 is a screen's worth and then some: at 1,920 CSS pixels a bar plus its
// gap cannot be under a pixel, so anything past this is rows nobody can see.
// The guard exists because window and width are independent inputs -- 90 days
// at one second is 7,776,000 buckets, which is not a slow chart but a
// different kind of request, and refusing it by name beats discovering it as
// a timeout.
const maxChurnBuckets = 1500

// churnUnboundedSpan is the span ChurnBuckets and ChurnPeerActivity use for
// their bars-per-answer guard when Since is zero: the archive's actual
// history retention, read live from route_unicast's TTL (RetentionDays),
// because retention.days is a deploy-time knob now, not the fixed 90 days
// the guard used to assume. A shorter retention must refuse sooner and a
// longer one must refuse later, or the guard either under-refuses (a
// request that would in fact scan more days than it accounts for) or
// over-refuses (rejecting a window the archive cannot even produce that
// much data for).
//
// If RetentionDays errors -- an operator-set TTL that is not a whole number
// of days, or a transient read failure -- this falls back to the shipped
// 90-day default rather than failing the request: the guard exists to
// refuse an unanswerable request, not to become one itself. This only runs
// for an unbounded request (Since zero), so the extra system-table read
// never lands on the common, bounded path.
func (q *Q) churnUnboundedSpan(ctx context.Context) time.Duration {
	days, err := q.RetentionDays(ctx)
	if err != nil {
		days = 90
	}
	return time.Duration(days) * 24 * time.Hour
}

// churnBucketsSQL folds the three classified route tables into one row per
// bucket.
//
// THE COLLECTOR IS CHOSEN ONCE OVER THE WINDOW, PER ROUTER, and both halves
// of that matter.
//
// Once over the window, because a per-bucket choice does not actually
// deduplicate. It defends against two collectors' copies of one event only
// when both copies land in the SAME bucket -- and ts_collector, the column
// the bucketing reads, is precisely the column that differs between
// collectors. So the per-bucket form failed exactly where duplication is
// most likely: a collector that joins, leaves, restarts or is replayed to at
// a different time from its sibling has its copies compared against nothing
// and added. Measured on a lab archive on 2026-09-20, where router
// 172.22.0.8 carried 20 rows under dev-c1 and 8 under dev-c2 with the same
// two ts_router values on both sides -- one captured session replayed to a
// second collector two days later -- a per-bucket choice reported all 28.
// It also drew a line whose consecutive points came from different
// observers, so a collector restarting mid-window put a step in the chart
// that no router caused.
//
// Per router, rather than one collector for the whole answer, because a
// collector watching part of the fleet must not blank the routers it never
// sees. Summing across ROUTERS is addition -- they are disjoint network
// entities; summing across COLLECTORS is double-counting. On one measured
// archive the two forms agreed at 538 rows, because dev-c1 watches everything
// there; they diverge the moment collectors are partitioned, and the
// per-router form is also the grain ChurnByPeer and ChurnByPrefix already
// use.
//
// It reuses dumpClassifiedSQL verbatim -- the same three renderings
// DumpCounts uses, the same expression that shipped to route-churn.json and
// evpn-churn.json. One classification, three consumers: a second copy here
// is exactly how the Go answer and the dashboards would drift apart while
// both looked right.
//
// %[1]-[3]s are the three classified arms, %[4]d the bucket width in seconds.
const churnBucketsSQL = `
SELECT
    bucket,
    sum(readvertise) AS readvertise,
    sum(withdraw)    AS withdraw,
    sum(dump)        AS dump
FROM (
    SELECT *, max((window_total, collector_id)) OVER (PARTITION BY router_ip) AS best
    FROM (
        SELECT *, sum(total) OVER (PARTITION BY router_ip, collector_id) AS window_total
        FROM (
            SELECT
                toStartOfInterval(ts_collector, INTERVAL %[4]d SECOND) AS bucket,
                router_ip,
                collector_id,
                countIf(NOT is_session_dump AND is_withdraw = 0)       AS readvertise,
                countIf(is_withdraw = 1)                               AS withdraw,
                countIf(is_session_dump)                               AS dump,
                readvertise + withdraw + dump                          AS total
            FROM (
%[1]s
                UNION ALL
%[2]s
                UNION ALL
%[3]s
            )
            GROUP BY bucket, router_ip, collector_id
        )
    )
)
WHERE (window_total, collector_id) = best
GROUP BY bucket
ORDER BY bucket`

// ChurnBuckets reports what the archive received over time, split into
// re-advertisements, withdrawals and session dumps -- the three series
// route-churn.json's own panel draws, computed by the same classification.
//
// An empty window is an answer, not an error: a chart with no bars says
// nothing churned then, which is a fact this archive can support.
//
// The window is applied BEFORE the classification, inside each table's own
// subquery, exactly as the dashboards apply theirs. See ChurnFilter.Since for
// what that does to the meaning of a bar near the window's edge.
func (q *Q) ChurnBuckets(ctx context.Context, f ChurnFilter) ([]ChurnBucket, error) {
	if f.Bucket < time.Second {
		return nil, fmt.Errorf("%w: churn bucket must be at least a second, got %s",
			ErrBadFilter, f.Bucket)
	}
	// Until now, because that is what an unbounded window reads to. A zero
	// Since means the archive's actual history retention (churnUnboundedSpan,
	// read live since retention.days is configurable), which is the widest
	// this can be asked for and the case the guard exists for.
	span := time.Since(f.Since)
	if f.Since.IsZero() {
		span = q.churnUnboundedSpan(ctx)
	}
	if buckets := span / f.Bucket; buckets > maxChurnBuckets {
		return nil, fmt.Errorf("%w: %s of %s buckets is %d bars, more than the %d one answer carries; "+
			"widen the bucket or narrow the window", ErrBadFilter,
			span.Round(time.Second), f.Bucket, buckets, maxChurnBuckets)
	}

	var w filters
	w.eqAddr("router_ip", f.Router)
	w.eqAddr("peer_ip", f.Peer)
	w.since("ts_collector", f.Since)
	w.before("ts_collector", f.Until)
	w.eqNonEmpty("collector_id", f.Collector)
	where := w.clause()

	stmt := fmt.Sprintf(churnBucketsSQL,
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_unicast", unicastRouteIdentity, where, ", collector_id"),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_vpn", vpnRouteIdentity, where, ", collector_id"),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_evpn", evpnRouteIdentity, where, ", collector_id"),
		int(f.Bucket.Seconds()),
	)

	// The same WHERE is rendered once per table -- three times -- so its
	// values are bound three times, in the order the arms appear. See
	// DumpCounts' own comment on this pattern.
	args := make([]any, 0, 3*len(w.values()))
	for range 3 {
		args = append(args, w.values()...)
	}

	rows, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query churn buckets: %w", err)
	}
	defer rows.Close()

	var out []ChurnBucket
	for rows.Next() {
		var b ChurnBucket
		if err := rows.Scan(&b.TS, &b.Readvertise, &b.Withdraw, &b.Dump); err != nil {
			return nil, fmt.Errorf("scan churn bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PeerChurn is one peer's totals over the window: the three churn series a
// bar chart splits, summed rather than bucketed.
//
// One row per (router, peer). The three series are the same mutually
// exclusive three ChurnBucket carries and sum to every row the peer sent --
// see that type for the classification, which this shares rather than
// re-derives.
type PeerChurn struct {
	RouterIP      netip.Addr
	RouterSysname string
	PeerIP        netip.Addr
	PeerASN       uint32

	Readvertise uint64
	Withdraw    uint64
	Dump        uint64
}

// PrefixChurn is one prefix's totals over the window: route-churn.json's
// "Most-changed prefixes", as a Go answer.
//
// ROUTE_UNICAST ONLY, which is what that panel already reads and what this
// type can honestly mean. `prefix` is a column on all three route tables and
// names the same kind of thing on only one of them: a route_vpn prefix
// without its rd is ambiguous (one string under two RDs is two routes), and
// a route_evpn type-2 MAC route has no IP prefix at all -- 136 of a lab
// archive's 202 route_evpn rows carry an empty one. Folding the three would
// merge unrelated routes under a shared string and rank a blank label.
type PrefixChurn struct {
	Prefix string

	// Observations is every archived row for this prefix in the window, and
	// equals Readvertise + Withdraw + Dump.
	Observations uint64
	Readvertise  uint64
	Withdraw     uint64
	Dump         uint64

	// Routes and Sessions are the panel's own two context columns: how many
	// distinct routes (the unicast identity, minus the prefix itself) and how
	// many BMP sessions contributed. A high Observations across one route and
	// many sessions is a flapping session; across many routes and one session
	// it is a busy prefix.
	Routes   uint64
	Sessions uint64
}

// churnByPeerSQL folds the three classified route tables into one row per
// peer. All three, unlike churnByPrefixSQL below: peer_ip is the same kind of
// thing on every route table, so summing a peer's unicast, VPN and EVPN churn
// under one label is addition, not conflation.
//
// argMax resolves router_sysname and peer_asn rather than grouping on them,
// for the reason dumpCountsSQL states at length for the first of the two:
// both are per-row columns carrying what was true when the row was written,
// so a device renamed or a peer renumbered mid-window would otherwise split
// into two rows, each holding part of one peer's archive.
//
// ORDER BY is the ranking the screen renders. It is here rather than in the
// client because an answer is allowed to be truncated: the top N of an
// unordered answer is N arbitrary peers. Changes first (re-advertisements
// plus withdrawals, the two series that mean the network did something),
// then dumps, then the address, so the order is total even when two peers
// churn identically.
//
// EVERY COUNT HERE IS ONE COLLECTOR'S, and which one is chosen deliberately.
//
// These counts describe the PEER -- how much it churned -- so they must not
// scale with how many collectors happened to be watching its router. Summing
// across collectors did exactly that, and not uniformly: measured on one
// archive, one dual-homed router inflated 2.0x and another 1.4x, because the
// collectors came up at different times. "Busiest first" then ranked by
// observer count rather than by churn.
//
// Deduplicating across collectors is not available. Two collectors observing
// one UPDATE agree on ts_router and on nothing else -- seq, stream_seq,
// session_id and ts_collector are all per collector -- and ts_router is the
// clock this package explicitly does not discriminate with: a dead router
// clock reports 1970, and QK_TS_ZERO then substitutes each COLLECTOR's own
// now() (collector/session.go), so the two would disagree precisely where the
// router's clock is broken. A key that silently stops deduplicating on the
// routers with the worst clocks is worse than no key.
//
// So the answer is the BEST SINGLE VANTAGE POINT: resolve each collector's
// view, then report whole the one that saw the most. It can never inflate, it
// is exactly right whenever the collectors agree, and on a single-collector
// deployment it is identical to the old sum -- measured against a test
// archive, a single-collector router read 99 both ways while a dual-homed
// one reads 17 where the sum read 34.
//
// The tie-break is a tuple, (total, collector_id), for two reasons. It makes
// the choice deterministic where two collectors saw equal amounts, and
// because every column is an argMax over the SAME ordering expression, all of
// them come from ONE collector's row -- a per-column max would report a
// readvertise count from one collector beside a dump count from another and
// call it a peer.
//
// ChurnFilter.Collector narrows to a named collector instead, which is the
// drill-down this default deliberately hides: a collector that has fallen
// behind looks like nothing at all here, because the other one wins.
//
// %[1]-[3]s are the three classified arms.
const churnByPeerSQL = `
SELECT
    router_ip,
    argMax(sysname, (total, collector_id))      AS sysname,
    peer_ip,
    argMax(asn, (total, collector_id))          AS asn,
    argMax(readvertise, (total, collector_id))  AS readvertise,
    argMax(withdraw, (total, collector_id))     AS withdraw,
    argMax(dump, (total, collector_id))         AS dump
FROM (
    SELECT
        router_ip,
        collector_id,
        peer_ip,
        argMax(router_sysname, (seq, stream_seq))              AS sysname,
        argMax(peer_asn, (seq, stream_seq))                    AS asn,
        countIf(NOT is_session_dump AND is_withdraw = 0)       AS readvertise,
        countIf(is_withdraw = 1)                               AS withdraw,
        countIf(is_session_dump)                               AS dump,
        readvertise + withdraw + dump                          AS total
    FROM (
%[1]s
        UNION ALL
%[2]s
        UNION ALL
%[3]s
    )
    GROUP BY router_ip, peer_ip, collector_id
)
GROUP BY router_ip, peer_ip
ORDER BY readvertise + withdraw DESC, dump DESC, peer_ip`

// ChurnPeerActivity is one peer's changes over time: the same classification
// churnByPeerSQL sums, kept bucketed instead.
//
// CHANGES ONLY -- re-advertisements plus withdrawals, never dumps. It sits
// beside a ranking ordered the same way, and the reason is the one that
// ordering already states: a peer whose session restarted once would
// otherwise draw a spike it did not cause, and a session dump is ordinary
// BMP behavior rather than something the network did.
//
// The best vantage point is chosen over the WHOLE WINDOW per (router, peer),
// exactly as churnByPeerSQL chooses it, and NOT per bucket. ChurnFilter's own
// doc comment carries the argument: a per-bucket choice does not deduplicate
// at all when two collectors' copies land in different buckets, which is the
// usual case when they are not watching at the same times. The window
// function shape here mirrors churnBucketsSQL's for the same reason.
//
// Buckets that held nothing are ABSENT rather than zero-filled. The caller
// places these on a time axis it already knows the bounds of -- the same
// thing ChurnBuckets' own answer requires -- so a gap is drawable without
// being transmitted, and transmitting it would make the row count a function
// of the window rather than of the data.
const churnPeerActivitySQL = `
SELECT router_ip, peer_ip, bucket, changes
FROM (
    SELECT *, max((window_total, collector_id)) OVER (PARTITION BY router_ip, peer_ip) AS best
    FROM (
        SELECT *, sum(total) OVER (PARTITION BY router_ip, peer_ip, collector_id) AS window_total
        FROM (
            SELECT
                toStartOfInterval(ts_collector, INTERVAL %[4]d SECOND) AS bucket,
                router_ip,
                peer_ip,
                collector_id,
                countIf(NOT is_session_dump AND is_withdraw = 0)
                    + countIf(is_withdraw = 1)                         AS changes,
                changes + countIf(is_session_dump)                     AS total
            FROM (
%[1]s
                UNION ALL
%[2]s
                UNION ALL
%[3]s
            )
            GROUP BY bucket, router_ip, peer_ip, collector_id
        )
    )
)
WHERE (window_total, collector_id) = best AND changes > 0
ORDER BY router_ip, peer_ip, bucket`

// ChurnPeerActivity reports each peer's changes per bucket over the window.
//
// One row per (router, peer, bucket) that held a change. See
// churnPeerActivitySQL for why dumps are excluded and why the vantage point
// is resolved over the whole window rather than per bucket.
func (q *Q) ChurnPeerActivity(ctx context.Context, f ChurnFilter) ([]ChurnPeerActivity, error) {
	if f.Bucket < time.Second {
		return nil, fmt.Errorf("%w: churn bucket must be at least a second, got %s",
			ErrBadFilter, f.Bucket)
	}
	// The same bars-per-answer guard ChurnBuckets enforces, and this
	// statement needs it MORE: it returns a row per bucket PER PEER, where
	// that one returns a row per bucket. Leaving it to the handler's own
	// bucket arithmetic would put the bound in a different package from the
	// statement it bounds -- a second caller, a CLI, or a test reaching this
	// directly would get the unguarded form. A zero Since uses the archive's
	// actual history retention (churnUnboundedSpan), for the same reason
	// ChurnBuckets does.
	span := time.Since(f.Since)
	if f.Since.IsZero() {
		span = q.churnUnboundedSpan(ctx)
	}
	if buckets := span / f.Bucket; buckets > maxChurnBuckets {
		return nil, fmt.Errorf("%w: %s of %s buckets is %d bars per peer, more than the %d one answer carries; "+
			"widen the bucket or narrow the window", ErrBadFilter,
			span.Round(time.Second), f.Bucket, buckets, maxChurnBuckets)
	}
	var w filters
	w.eqAddr("router_ip", f.Router)
	w.eqAddr("peer_ip", f.Peer)
	w.since("ts_collector", f.Since)
	w.before("ts_collector", f.Until)
	w.eqNonEmpty("collector_id", f.Collector)
	where := w.clause()

	// No peer_asn: churnPeerActivitySQL never projects it, and reading a
	// column off all three route tables to discard it is work the answer
	// does not use. ChurnByPeer's own `extra` carries it because that
	// statement resolves it with argMax.
	const extra = ", collector_id, peer_ip"
	stmt := fmt.Sprintf(churnPeerActivitySQL,
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_unicast", unicastRouteIdentity, where, extra),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_vpn", vpnRouteIdentity, where, extra),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_evpn", evpnRouteIdentity, where, extra),
		int(f.Bucket.Seconds()),
	)

	// Three arms, so the WHERE's values are bound three times in the order
	// the arms appear -- see ChurnBuckets for the same pattern.
	args := make([]any, 0, 3*len(w.values()))
	for range 3 {
		args = append(args, w.values()...)
	}

	rows, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query churn peer activity: %w", err)
	}
	defer rows.Close()

	var out []ChurnPeerActivity
	for rows.Next() {
		var a ChurnPeerActivity
		if err := rows.Scan(&a.RouterIP, &a.PeerIP, &a.Bucket, &a.Changes); err != nil {
			return nil, fmt.Errorf("scan churn peer activity: %w", err)
		}
		// Same unmapping every other churn answer does: the client joins
		// these by address, and netip.Addr equality is representation
		// sensitive.
		unmapAll(&a.RouterIP, &a.PeerIP)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ChurnByPeer reports each peer's churn totals over the window, busiest
// first: route-churn.json's "Changes by peer" question, answered as a
// ranking rather than as a time series.
//
// It shares the classification with ChurnBuckets and nothing else. That
// panel groups by (bucket, peer_ip) and draws a line per peer; the Monitor
// screen this serves is a ranked table, and the two cannot be the same query
// however much they look alike -- they were once wrongly built as if they
// could be.
//
// Bucket is not read. This answer has no time axis, so the bars-per-answer
// guard ChurnBuckets applies does not arise: one row per peer is bounded by
// the fleet, not by the window over a width.
func (q *Q) ChurnByPeer(ctx context.Context, f ChurnFilter) ([]PeerChurn, error) {
	var w filters
	w.eqAddr("router_ip", f.Router)
	w.eqAddr("peer_ip", f.Peer)
	w.since("ts_collector", f.Since)
	w.before("ts_collector", f.Until)
	w.eqNonEmpty("collector_id", f.Collector)
	where := w.clause()

	const extra = ", collector_id, peer_ip, peer_asn"
	stmt := fmt.Sprintf(churnByPeerSQL,
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_unicast", unicastRouteIdentity, where, extra),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_vpn", vpnRouteIdentity, where, extra),
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_evpn", evpnRouteIdentity, where, extra),
	)

	// Three arms, so the WHERE's values are bound three times in the order
	// the arms appear -- see ChurnBuckets for the same pattern.
	args := make([]any, 0, 3*len(w.values()))
	for range 3 {
		args = append(args, w.values()...)
	}

	rows, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query churn by peer: %w", err)
	}
	defer rows.Close()

	var out []PeerChurn
	for rows.Next() {
		var p PeerChurn
		if err := rows.Scan(&p.RouterIP, &p.RouterSysname, &p.PeerIP, &p.PeerASN,
			&p.Readvertise, &p.Withdraw, &p.Dump); err != nil {
			return nil, fmt.Errorf("scan peer churn: %w", err)
		}
		// ClickHouse hands back an IPv6 for these columns, so an IPv4 peer
		// arrives as ::ffff:a.b.c.d. The client that reads this answer joins
		// it against /v1/peers BY ADDRESS, and netip.Addr equality is
		// representation-sensitive -- see unmapAll.
		unmapAll(&p.RouterIP, &p.PeerIP)
		out = append(out, p)
	}
	return out, rows.Err()
}

// maxChurnPrefixes is the most prefixes one answer carries.
//
// 50, which is route-churn.json's own LIMIT on this panel. The number is a
// screen's worth rather than a cost bound: this is a ranking, and a ranking
// past the first page answers no question the first page did not.
const maxChurnPrefixes = 50

// churnByPrefixSQL folds ONE classified table -- route_unicast -- into one
// row per prefix.
//
// WHY ONE TABLE, at the risk of repeating PrefixChurn's own doc: `prefix` is
// a column on all three route tables and names the same kind of thing on one
// of them. A route_vpn prefix without its rd is ambiguous, and a route_evpn
// type-2 MAC route has no IP prefix at all -- 136 of a measured archive's 202
// route_evpn rows carried an empty one. This is route-churn.json's own scope,
// not a narrowing invented here.
//
// prefix != ” EXCLUDES UNDECODED NLRI. One measured archive held 56
// route_unicast rows whose prefix is empty and whose parse_flags carry
// PARSE_FLAG_VERSION_UNPARSED: bytes the decoder could not read. Ranked by
// change count they sort to the top, so the busiest "prefix" on the screen
// would be a blank label counting the collector's own failures. Excluded in
// SQL rather than in the caller, because a LIMITed answer filtered afterwards
// comes back short by however many artifacts it held.
//
// Routes counts the unicast identity MINUS prefix -- the other columns of
// unicastRouteIdentity -- because "how many routes does this prefix churn
// across" is a question about everything that distinguishes two routes
// carrying the same prefix.
//
// It also omits collector_id, which it used to lead with. That made one route
// two collectors both held count as two ROUTES, in a column named routes.
// Removing it is now cosmetic rather than load-bearing -- the inner GROUP BY
// keys on collector_id, so the column is constant inside every group and
// putting it back in this tuple changes no answer (verified by mutation). It
// stays out because leaving it in describes a question nobody asked.
//
// %[1]s is the classified arm, %[2]d the row limit.
const churnByPrefixSQL = `
SELECT
    prefix,
    argMax(observations, (total, collector_id)) AS observations,
    argMax(readvertise, (total, collector_id))  AS readvertise,
    argMax(withdraw, (total, collector_id))     AS withdraw,
    argMax(dump, (total, collector_id))         AS dump,
    argMax(routes, (total, collector_id))       AS routes,
    argMax(sessions, (total, collector_id))     AS sessions
FROM (
    SELECT
        prefix,
        collector_id,
        count()                                                AS observations,
        countIf(NOT is_session_dump AND is_withdraw = 0)       AS readvertise,
        countIf(is_withdraw = 1)                               AS withdraw,
        countIf(is_session_dump)                               AS dump,
        uniqExact((router_ip, peer_ip, rib, family, path_id))  AS routes,
        uniqExact(session_id)                                  AS sessions,
        readvertise + withdraw + dump                          AS total
    FROM (
%[1]s
    )
    WHERE prefix != ''
    GROUP BY prefix, collector_id
)
GROUP BY prefix
ORDER BY readvertise + withdraw DESC, withdraw DESC, observations DESC, prefix
LIMIT %[2]d`

// ChurnByPrefix reports the most-changed prefixes over the window, ranked:
// route-churn.json's "Most-changed prefixes" panel as a Go answer, at that
// panel's own scope of route_unicast alone.
//
// Bucket is not read, for the reason ChurnByPeer states.
func (q *Q) ChurnByPrefix(ctx context.Context, f ChurnFilter) ([]PrefixChurn, error) {
	var w filters
	w.eqAddr("router_ip", f.Router)
	w.eqAddr("peer_ip", f.Peer)
	w.since("ts_collector", f.Since)
	w.before("ts_collector", f.Until)
	w.eqNonEmpty("collector_id", f.Collector)
	where := w.clause()

	// Everything the aggregate above reads that the classification does not
	// already select. The identity's own columns come along because Routes
	// counts over them.
	const extra = ", prefix, session_id, collector_id, peer_ip, rib, family, path_id"
	stmt := fmt.Sprintf(churnByPrefixSQL,
		fmt.Sprintf(dumpClassifiedSQL, q.db, "route_unicast", unicastRouteIdentity, where, extra),
		maxChurnPrefixes,
	)

	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query churn by prefix: %w", err)
	}
	defer rows.Close()

	var out []PrefixChurn
	for rows.Next() {
		var p PrefixChurn
		if err := rows.Scan(&p.Prefix, &p.Observations, &p.Readvertise, &p.Withdraw,
			&p.Dump, &p.Routes, &p.Sessions); err != nil {
			return nil, fmt.Errorf("scan prefix churn: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
