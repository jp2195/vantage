package query

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// PeerEvent is one row of PeerEventsPage: peer_events reported as the event
// it was, rather than folded into a current-state answer -- HistoryEvent's
// own reason, applied to the one table that records what HAPPENED to a
// session rather than what state a route or a peer is in now.
//
// TsCollector and TsRouter are two different claims, for HistoryEvent's own
// reason: TsCollector is when the collector received the message and is the
// trustworthy clock; TsRouter is what the router claimed, carried for
// display because an operator may want it (a fleet-wide clock skew is a
// real finding), and never ordered or filtered on. See PeerEventsPage.
//
// Kind is one of peer_events' own four Enum8 members -- "unspecified",
// "up", "down", "view_lost" -- returned exactly as read. "view_lost" is not
// a "down": it is the collector losing its view of the peer while the
// router said nothing, and DownReason is 0 on every such row because there
// is no reason to report, not because one was omitted. See DownReason.
//
// DownReason is a BMP Peer Down reason code (RFC 7854 sec 4.9) and 0 is not
// a member of that registry -- it means the router said nothing, which
// happens on "kind = view_lost" (the collector's own synthetic close) and
// on "kind" values that carry no reason at all. This query never coerces,
// defaults or normalizes it: a stored 0 is scanned as 0, unconditionally.
type PeerEvent struct {
	TsCollector   time.Time
	TsRouter      time.Time
	StreamSeq     uint64
	Seq           uint64
	SessionID     uint64
	RouterIP      netip.Addr
	RouterSysname string
	PeerIP        netip.Addr
	PeerASN       uint32
	Collector     string
	RIB           string
	Kind          string
	DownReason    uint32
	LocalIP       netip.Addr
	LocalPort     uint32
	RemotePort    uint32
}

// PeerEventFilter is the narrowing PeerEventsPage accepts: GET /v1/events'
// own parameters.
type PeerEventFilter struct {
	// Router and Peer are both REQUIRED, unlike every optional filter field
	// elsewhere in this package. peer_events' sort key is (router_ip,
	// peer_ip, rib, ts_router, stream_seq) -- identity first, time fourth --
	// so a question scoped to one peer's history is a seek and an unscoped
	// one is a scan against the grain of that key. check refuses a filter
	// missing either, the way RouteHistory refuses a missing Prefix.
	Router netip.Addr
	Peer   netip.Addr

	// RIB narrows to one rib; "" crosses every rib the peer appears under,
	// exactly as HistoryFilter.RIB and every RIB/LS walk's own RIB field do.
	RIB string

	// Since bounds the walk inclusively against ts_collector, never
	// ts_router, for HistoryFilter.Since's own reason. The zero time means
	// no lower bound at all.
	//
	// It is the one filter that actually limits how much of the peer's
	// history this walk reads: see PeerEventsPage's own doc comment on why
	// the cursor does not.
	Since time.Time

	// Cursor carries a walk's position: nil for the first page, and the
	// *RIBCursor a previous page returned for every page after it. See
	// PeerEventsPage.
	Cursor *RIBCursor

	// Limit caps the rows one page returns and is resolved through
	// clampRIBLimit, the same clamp the RIB and link-state walks use --
	// "one convention across this package beats two" is lsNodesSQL's own
	// phrase for the identical choice.
	Limit int
}

// check refuses a filter with no Router or no Peer, naming the endpoint the
// way RouteHistory's own check does for a missing Prefix.
func (f PeerEventFilter) check() error {
	if !f.Router.IsValid() {
		return fmt.Errorf("%w: /v1/events needs router -- an unscoped events "+
			"query is a full scan against peer_events' sort key (router_ip, "+
			"peer_ip, rib, ts_router, stream_seq), the same reason /v1/rib/* "+
			"requires router and peer rather than answering a fleet-wide dump "+
			"one page at a time", ErrBadFilter)
	}
	if !f.Peer.IsValid() {
		return fmt.Errorf("%w: /v1/events needs peer", ErrBadFilter)
	}
	return nil
}

// peerEventsSQL dedups peer_events by the strategy measured and chosen on
// 2026-09-07 (docs/measurements.md, "Peer event deduplication"):
// argMax(field, ts_collector) / max(ts_collector), GROUP BY the scoped key
// (router_ip, peer_ip, rib, ts_router, stream_seq). That measurement is
// explicit that the choice is for CORRECTNESS, not a measured speed or
// pagination win -- peer_events is a ReplacingMergeTree,
// so a read landing between an insert and the background merge that
// reconciles it can see duplicate rows for one key, and argMax resolves
// each key's freshest row deterministically from whatever the read sees,
// with no dependency on whether that merge has run. No FINAL: this
// package's rule (see the package doc comment) holds here for the same
// reason and the measurement found it holds even though, on ITS OWN numbers,
// argMax reads more rows than FINAL would -- the decision rests on
// correctness, not cost.
//
// max(ts_collector) is aliased latest_ts_collector, never ts_collector.
// The 2026-09-07 measurement hit `max(ts_collector) AS ts_collector` outright:
// ClickHouse folds that alias back into the sibling argMax(field,
// ts_collector) calls in the same SELECT list, producing an aggregate
// inside an aggregate (Code: 184, ILLEGAL_AGGREGATION). Renaming the alias
// is the whole fix; nothing else about the shape changes.
//
// Every other argMax'd column keeps its own raw column name as its alias --
// seq, session_id, collector_id, router_sysname, peer_asn, kind,
// down_reason, local_ip, local_port, remote_port -- which is safe for the
// same reason the rename is exact: nothing else in this statement
// uses any of those names as an aggregate's ORDERING argument, which is
// the specific collision that broke ts_collector.
//
// peer_bgp_id is deliberately NOT selected. It is a real peer_events
// column, but it has no field on PeerEvent, and this statement's column
// list answers to that struct rather than to the measured candidate's
// column list, which was a timing probe, not a projection.
//
// %[1]s is the database name. %[2]s is the WHERE clause -- router, peer,
// rib and since, all rendered pre-aggregation because every one of them is
// either part of the GROUP BY key (router_ip, peer_ip, rib) or, for since,
// a bound on ts_collector that is equivalent whether it is applied before
// or after aggregation (see PeerEventsPage's own doc comment). %[3]s is the
// HAVING clause -- the keyset cursor predicate, and ONLY the cursor
// predicate, because it is the one narrowing in this statement that names
// an AGGREGATE result (latest_ts_collector) rather than a raw column, and
// an aggregate does not exist yet at WHERE's point in execution. This is
// the one place this statement's cursor mechanics diverge from
// LSNodesPage's: every column in LSNodesPage's keyset is a raw,
// pre-aggregation column, so its predicate lives in WHERE; this walk's
// leading key column is not, so its predicate has to live in HAVING
// instead. Both were run directly against a live ClickHouse (24.8.14.39)
// before being wired into Go, including the HAVING form with a bound
// cursor value, to confirm the alias resolves there the way peerStateCTE's
// own doc comment says it does inside WHERE (the same alias-shadowing
// hazard found in the l3vpn-rib-browser dashboard's rd/vrf filter).
//
// ORDER BY is (latest_ts_collector, stream_seq) DESCENDING -- newest first
// on the collector clock. Neither column is a prefix of peer_events' own
// physical ORDER BY (router_ip,
// peer_ip, rib, ts_router, stream_seq): the table is sorted on ts_router,
// which this project holds is never authoritative for ordering, so this
// ORDER BY is a deliberate choice against the physical key, not a
// restatement of it. That has a real cost, stated plainly rather than
// glossed over: ClickHouse cannot stream this statement's rows in
// ts_collector order and stop at LIMIT, so it must materialize every row
// this WHERE and GROUP BY leave in scope, sort them, and only then trim to
// the page. A page therefore costs O(rows in scope), not O(page size) --
// see PeerEventsPage's own doc comment for what actually bounds that cost,
// since it is not the cursor.
const peerEventsSQL = `
SELECT
    pe.router_ip, pe.peer_ip, pe.rib, pe.ts_router, pe.stream_seq,
    max(pe.ts_collector)                     AS latest_ts_collector,
    argMax(pe.seq,            pe.ts_collector) AS seq,
    argMax(pe.session_id,     pe.ts_collector) AS session_id,
    argMax(pe.collector_id,   pe.ts_collector) AS collector_id,
    argMax(pe.router_sysname, pe.ts_collector) AS router_sysname,
    argMax(pe.peer_asn,       pe.ts_collector) AS peer_asn,
    argMax(pe.kind,           pe.ts_collector) AS kind,
    argMax(pe.down_reason,    pe.ts_collector) AS down_reason,
    argMax(pe.local_ip,       pe.ts_collector) AS local_ip,
    argMax(pe.local_port,     pe.ts_collector) AS local_port,
    argMax(pe.remote_port,    pe.ts_collector) AS remote_port
FROM %[1]s.peer_events pe
%[2]s
GROUP BY pe.router_ip, pe.peer_ip, pe.rib, pe.ts_router, pe.stream_seq
%[3]s
ORDER BY latest_ts_collector DESC, pe.stream_seq DESC`

// scanPeerEvents drains rows into PeerEventsPage's answer shape. Scan is
// positional, for scanRoutes' own reason: a column added to peerEventsSQL
// and forgotten here fails loudly, a column reordered there and not here
// binds a port to an ASN without a word.
//
// local_port and remote_port are UInt16 in the schema; PeerEvent widens
// both to uint32, so they are scanned into local uint16 variables first
// and converted after -- the same
// scan-native-then-convert shape scanLSPrefixes already uses for hasSID and
// scanRoutes for liveIsWithdraw.
func scanPeerEvents(rows driver.Rows) ([]PeerEvent, error) {
	var out []PeerEvent
	for rows.Next() {
		var e PeerEvent
		var localPort, remotePort uint16
		if err := rows.Scan(
			&e.RouterIP, &e.PeerIP, &e.RIB, &e.TsRouter, &e.StreamSeq,
			&e.TsCollector,
			&e.Seq, &e.SessionID, &e.Collector, &e.RouterSysname, &e.PeerASN,
			&e.Kind, &e.DownReason, &e.LocalIP, &localPort, &remotePort,
		); err != nil {
			return nil, fmt.Errorf("scan peer event: %w", err)
		}
		e.LocalPort = uint32(localPort)
		e.RemotePort = uint32(remotePort)
		unmapAll(&e.RouterIP, &e.PeerIP, &e.LocalIP)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PeerEventsPage reports one page of one (router, peer)'s peer_events
// history, keyset-ordered by (ts_collector, stream_seq) descending -- newest
// first, on the collector clock. It is this package's history counterpart
// of LSNodesPage: same RIBCursor, same LIMIT-plus-one probe row, same
// nil-cursor-ends-the-walk contract -- but it answers what HAPPENED to a
// session, in HistoryEvent's sense, not what is true of it now, so it
// carries none of LSNodesPage's session pin.
//
// There is no session scoping here, deliberately. peer_events' own current-
// session view is what peerStateCTE derives from this same table for every
// OTHER surface in this package; this walk answers the opposite question --
// the whole recorded timeline for (router, peer[, rib]), across every
// session the router has ever held -- so f carries no Collector and the
// *RIBCursor this returns leaves Collector and SessionID at their zero
// values. A caller cannot use one to pin a walk to a particular collector's
// view, unlike a RIB or link-state cursor; nothing about this endpoint asks
// it to.
//
// f.Router and f.Peer are both required; see PeerEventFilter.check. f.RIB
// stays optional and, left unset, spans every rib the peer has ever been
// recorded under. f.Since bounds the walk inclusively against ts_collector.
//
// WHAT BOUNDS THE COST OF A PAGE IS f.Since AND THE SCOPE, NOT THE CURSOR.
// peerEventsSQL's own doc comment states why: ts_collector is not a prefix
// of peer_events' physical ORDER BY, so this statement cannot stream in
// that order and stop at LIMIT -- it reads and sorts every row the WHERE
// and GROUP BY leave in scope before trimming to a page, on every page of
// the walk. What the cursor buys instead is real and different: a stable
// walk across a table a collector is actively writing to, where a page
// boundary neither repeats an event nor silently drops one the way an
// offset would if a row landed on either side of it mid-walk. It does not
// make a page cheap; nothing in this design claims that.
//
// f.Cursor is validated against f.Router, f.Peer and f.RIB before it is
// used, the same hazard RIBCursor's own doc comment describes at length for
// the RIB and link-state walks: a cursor is a POSITION, and a position only
// means something inside the walk that produced it. A mismatch is refused
// with ErrBadFilter rather than silently reinterpreted against a different
// scope.
func (q *Q) PeerEventsPage(ctx context.Context, f PeerEventFilter) ([]PeerEvent, *RIBCursor, error) {
	if err := f.check(); err != nil {
		return nil, nil, err
	}

	var lastMicro uint64
	var lastStreamSeq uint64
	haveCursor := f.Cursor != nil
	if haveCursor {
		c := f.Cursor
		if c.Router != f.Router || c.Peer != f.Peer || c.RIB != f.RIB {
			// The cursor's own rib is not named: the cursor is opaque but
			// not signed, so that field is whatever text the caller sent.
			return nil, nil, fmt.Errorf("%w: this cursor was issued for a different "+
				"walk and cannot be used on a walk of router %s peer %s %s -- its "+
				"position is the last key of a DIFFERENT walk, "+
				"and paging one scope from another's cursor skips or repeats rows "+
				"silently", ErrBadFilter, f.Router, f.Peer, ribScopeText(f.RIB))
		}
		if len(c.Last) != 2 {
			return nil, nil, fmt.Errorf("%w: cursor carries %d key values, want 2 "+
				"(ts_collector, stream_seq)", ErrBadFilter, len(c.Last))
		}
		// uint64, not int64: api/cursor.go's encodeCursorKey/decodeCursorKey
		// -- the wire codec every RIBCursor.Last element has to survive --
		// type-switches on string, uint32, uint8 and uint64 and nothing
		// else; an int64 falls to its default case and is tagged "!int64:...",
		// which decodeCursorKey has no case for either, so a cursor built
		// with one would fail to decode on page 2. uint64 is faithful
		// rather than a workaround: a peer_events row's ts_collector is
		// never before the Unix epoch (the table's own 90-day TTL purges
		// anything old enough to make that a live concern), so nothing real
		// is lost by carrying it unsigned.
		micro, ok := c.Last[0].(uint64)
		if !ok {
			return nil, nil, fmt.Errorf("%w: cursor value 0 (ts_collector) has type "+
				"%T, want uint64", ErrBadFilter, c.Last[0])
		}
		streamSeq, ok := c.Last[1].(uint64)
		if !ok {
			return nil, nil, fmt.Errorf("%w: cursor value 1 (stream_seq) has type "+
				"%T, want uint64", ErrBadFilter, c.Last[1])
		}
		lastMicro, lastStreamSeq = micro, streamSeq
	}

	var w filters
	w.eqAddr("pe.router_ip", f.Router)
	w.eqAddr("pe.peer_ip", f.Peer)
	w.eqNonEmpty("pe.rib", f.RIB)
	w.since("pe.ts_collector", f.Since)
	if haveCursor {
		// HAVING, not WHERE: latest_ts_collector is max(pe.ts_collector),
		// an aggregate result that does not exist until after GROUP BY. See
		// peerEventsSQL's own doc comment for why this is the one place
		// this walk's cursor mechanics cannot mirror LSNodesPage's
		// WHERE-based keyset predicate.
		w.havingExpr("(latest_ts_collector, pe.stream_seq) < (fromUnixTimestamp64Micro(?), ?)",
			lastMicro, lastStreamSeq)
	}

	size := clampRIBLimit(f.Limit)
	stmt := fmt.Sprintf(peerEventsSQL, q.db, w.clause(), w.havingClause())
	stmt += fmt.Sprintf(" LIMIT %d", size+1) // the probe row; see ribStatement

	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, nil, fmt.Errorf("query peer events page: %w", err)
	}
	defer rows.Close()
	out, err := scanPeerEvents(rows)
	if err != nil {
		return nil, nil, err
	}

	// The probe row: the statement asked for one more than the page size,
	// so its presence is proof another page exists rather than a guess
	// that one might. See ribStatement.
	if len(out) <= size {
		return out, nil, nil
	}
	out = out[:size]
	last1 := out[len(out)-1]

	// UnixMicro, not the time.Time itself: filters.since's own doc comment
	// records why binding a time.Time directly loses precision and can send
	// a zone the server cannot resolve. A microsecond count is
	// peer_events.ts_collector's own DateTime64(6) resolution exactly, and
	// is lifted back into one by the same fromUnixTimestamp64Micro this
	// statement's own since bound already uses.
	//
	// Carried as uint64, not int64: see this function's own cursor-reading
	// half, above, for why the wire codec requires it. Guarded rather than
	// bare -- a bare uint64(micro) on a negative micro would wrap silently
	// into a huge, wrong position rather than fail -- because a row whose
	// ts_collector predates the Unix epoch is not a real peer_events row
	// (the table's own 90-day TTL rules it out) and should say so rather
	// than hand back a cursor that walks somewhere else.
	micro := last1.TsCollector.UnixMicro()
	if micro < 0 {
		return nil, nil, fmt.Errorf("query: peer event has ts_collector %s, before "+
			"the Unix epoch -- refusing to encode a cursor position from a row this "+
			"corrupt", last1.TsCollector)
	}
	return out, &RIBCursor{
		Router: f.Router,
		Peer:   f.Peer,
		RIB:    f.RIB,
		Last:   []any{uint64(micro), last1.StreamSeq},
	}, nil
}

// UnscopedEventsMeasurement is the one place the citation for the
// unscoped-events cost measurement is spelled out in code: a repo-relative
// path plus the section's anchor. checkUnscopedWindow's 400
// (api/handlers.go) and FleetEventFilter.check's ErrBadFilter below both
// quote it in their error text, so an operator or a later reader can find
// the numbers a refusal is citing; TestCitedUnscopedEventsMeasurementExists
// (api/events_test.go) guards that the file and the section heading still
// exist. A re-measurement that renames or moves the section only has to
// change this one line.
const UnscopedEventsMeasurement = "docs/measurements.md#unscoped-events"

// FleetEventFilter is the narrowing FleetEvents and CountFleetEvents accept:
// GET /v1/events' UNSCOPED mode, the fleet console's own question ("what just
// broke anywhere") rather than one session's history.
//
// It has no Router and no Peer field, and that absence is the type doing the
// work. The alternative -- an Unscoped bool on PeerEventFilter -- is a flag a
// caller can forget, and forgetting it turns a seek into a full-table scan
// that still returns plausible rows. A struct that cannot NAME a router
// cannot be handed one by accident, and PeerEventFilter.check stays exactly
// as shipped, tests included.
type FleetEventFilter struct {
	// RIB narrows to one rib; "" crosses every rib, exactly as
	// PeerEventFilter.RIB does. It is honored here rather than dropped
	// because ?rib= is already on this path's contract, and a parameter that
	// silently stops narrowing is worse than one that never existed.
	RIB string

	// Since bounds the read inclusively against ts_collector and is
	// REQUIRED -- the one field of this struct that check refuses to leave
	// unset.
	//
	// UnscopedEventsMeasurement (measured 2026-09-08) is why.
	// ts_collector is not a prefix of peer_events' physical ORDER BY
	// (router_ip, peer_ip, rib, ts_router, stream_seq), so an unscoped
	// newest-first read cannot stop early: measured at read_rows = 2,040,000
	// at LIMIT 100, 1,000 AND 10,000 alike, and 237-289ms with 2.4-3.6 GB
	// resident once the argMax dedup is layered on. Since is the ONLY thing
	// that bounds that. The same measurement found it pruning whole months via
	// PARTITION BY toYYYYMM(ts_collector): a since=1-DAY bound still read the
	// entire 174,262-row current-month partition, same as a 24-hour bound
	// (14-20ms, 24-30MB resident once the argMax dedup is layered on). But
	// partition grain is not the floor: the same measurement found the
	// shipped 1-hour default at 4-5ms and ~100-210KB resident, roughly 4x
	// faster and ~100x lighter than the 24-hour bound above, not merely "no
	// cheaper." Why was not isolated. The likely reason is ClickHouse's
	// per-part min/max range on the partitioning column, which lets a narrow
	// bound skip parts inside the partition, but that was not tested. A bound
	// spanning most of retention degrades back toward the full-table numbers
	// above regardless.
	Since time.Time

	// Limit caps the rows returned, through clampRIBLimit, the same clamp
	// every other walk in this package uses. Unlike PeerEventsPage's, this
	// limit is a CAP and not a page size: there is no cursor here and no
	// probe row, so a caller that hits it is told by meta.total_matched how
	// much it did not see rather than handed a way to fetch the rest. See
	// api/handlers.go's handleEvents.
	Limit int
}

// check refuses a filter with no Since, the way PeerEventFilter.check
// refuses a missing scope, and for a cost reason rather than a semantic one.
//
// api/ clamps the window to a configured maximum before it ever reaches
// here, so in the daemon this is a second gate on the same hazard. It exists
// anyway because the two layers refuse for different reasons: api/ enforces
// a policy an operator can raise, and this enforces that the statement is
// never built unbounded at all. filters.wideASN makes the identical argument
// about origin_asn=0 being refused twice.
func (f FleetEventFilter) check() error {
	if f.Since.IsZero() {
		return fmt.Errorf("%w: an unscoped events query needs a since bound -- "+
			"ts_collector is not a prefix of peer_events' sort key (router_ip, "+
			"peer_ip, rib, ts_router, stream_seq), so a fleet-wide newest-first "+
			"read cannot stop at LIMIT and scans the whole table (2,040,000 rows, "+
			"237-289ms, gigabytes resident: see "+UnscopedEventsMeasurement+"). "+
			"Bound the window, or name router and peer, which is a seek",
			ErrBadFilter)
	}
	return nil
}

// predicates builds the WHERE this filter contributes. It is a method rather
// than two inline copies because FleetEvents and CountFleetEvents must build
// the IDENTICAL predicate: a count derived from a hand-written second version
// is how meta.total_matched drifts from the rows beside it, which is exactly
// the number that is supposed to make a capped answer honest. CountLSNodes
// makes the same argument for the same reason.
func (f FleetEventFilter) predicates() filters {
	var w filters
	w.eqNonEmpty("pe.rib", f.RIB)
	w.since("pe.ts_collector", f.Since)
	return w
}

// FleetEvents reports the newest peer_events across the WHOLE fleet inside
// f.Since, capped at f.Limit -- the unscoped counterpart of PeerEventsPage.
//
// It reuses peerEventsSQL verbatim, with the router and peer predicates
// simply absent and no HAVING clause, because the dedup that statement
// performs is right here for the same reason it is right there:
// peer_events is a ReplacingMergeTree, so a read landing between an insert
// and its merge can see duplicate rows for one key, and argMax resolves each
// key's freshest row from whatever the read sees. The 2026-09-08
// measurement (UnscopedEventsMeasurement) ran that dedup fleet-wide and found
// it affordable only under a since bound -- which f.Since is required to carry.
//
// THERE IS NO CURSOR HERE, deliberately. peerEventsSQL sorts on
// latest_ts_collector, which is not a prefix of the table's physical key, so
// a page costs O(rows in scope) rather than O(page size) no matter what: a
// cursor would buy a stable walk across a live table without making any page
// cheaper, and the console's question is a bounded recent view, not a deep
// walk. The cap plus meta.total_matched bound the answer instead -- the
// same pair /v1/ls/nodes already uses for its own unscoped request.
func (q *Q) FleetEvents(ctx context.Context, f FleetEventFilter) ([]PeerEvent, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	w := f.predicates()
	stmt := fmt.Sprintf(peerEventsSQL, q.db, w.clause(), "")
	stmt += fmt.Sprintf(" LIMIT %d", clampRIBLimit(f.Limit))

	rows, err := q.conn.Query(ctx, stmt, w.values()...)
	if err != nil {
		return nil, fmt.Errorf("query fleet events: %w", err)
	}
	defer rows.Close()
	return scanPeerEvents(rows)
}

// CountFleetEvents reports how many events match f, IGNORING f.Limit -- the
// number meta.total_matched carries.
//
// It wraps peerEventsSQL rather than counting the base table, and the
// difference is not cosmetic: peerEventsSQL groups by (router_ip, peer_ip,
// rib, ts_router, stream_seq), so a bare count() over peer_events would count
// pre-dedup ROWS while FleetEvents returns post-dedup KEYS. On an archive
// mid-merge those are different numbers, and reporting the larger one beside
// the smaller list would overstate what the caller did not see.
//
// The ORDER BY inside is harmless to a count and is left in place rather than
// stripped, because building a second, subtly different statement is the
// drift CountLSNodes' own doc comment warns about; ClickHouse discards it.
func (q *Q) CountFleetEvents(ctx context.Context, f FleetEventFilter) (uint64, error) {
	if err := f.check(); err != nil {
		return 0, err
	}
	w := f.predicates()
	inner := fmt.Sprintf(peerEventsSQL, q.db, w.clause(), "")
	var n uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT count() FROM ("+inner+")", w.values()...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count fleet events: %w", err)
	}
	return n, nil
}
