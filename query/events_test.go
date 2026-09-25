package query

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// eventsFixtureAnchor is the calendar date at() and atSeq() build every
// instant against: today, at package init, truncated to a microsecond --
// peer_events.ts_collector is DateTime64(6), and an untruncated time.Now()
// does not survive the round trip intact (see insertHistoryFixture's own
// base for the identical reason).
//
// It is NOT a fixed date. peer_events carries `TTL toDateTime(ts_collector)
// + INTERVAL 90 DAY` (schema.sql), and a background merge can purge a row
// past that TTL at any point after it lands, including mid-test. Anchored
// on a fixed date such as 2026-01-01,
// TestPeerEventsPageWalksWithoutRepeatingOrDropping fails intermittently
// with far fewer than 25 rows, because rows written early in that test's
// 25-iteration insert loop are TTL-eligible from the moment they land (more
// than 90 days old) and a merge purges some of them before the test's own
// query runs. Anchoring on today keeps
// every row this file writes newer than the TTL for as long as any test
// binary actually runs.
var eventsFixtureAnchor = time.Now().UTC().Truncate(time.Microsecond)

// at returns the instant on eventsFixtureAnchor's date at clock time hhmmss
// ("15:04:05"). It exists for tests that need two absolute instants to
// compare against each other -- TestPeerEventsPageOrdersOnTheCollectorClock
// wants a ts_collector and a ts_router that disagree about which of two
// events is newer, which a fixed offset from time.Now() cannot express as
// readably as a clock reading can.
func at(hhmmss string) time.Time {
	tod, err := time.Parse("15:04:05", hhmmss)
	if err != nil {
		panic("events_test: at(" + hhmmss + "): " + err.Error())
	}
	return time.Date(eventsFixtureAnchor.Year(), eventsFixtureAnchor.Month(), eventsFixtureAnchor.Day(),
		tod.Hour(), tod.Minute(), tod.Second(), 0, time.UTC)
}

// atSeq returns a strictly increasing instant for i >= 1, one second apart.
// TestPeerEventsPageWalksWithoutRepeatingOrDropping uses it to give 25 rows
// 25 distinct ts_collector values, so the walk's order is unambiguous
// without leaning on stream_seq to break a tie.
func atSeq(i int) time.Time {
	return eventsFixtureAnchor.Add(time.Duration(i) * time.Second)
}

// peerEventRow is the write-side shape this file's own tests need: just the
// four columns PeerEventsPage's behavior actually turns on. Everything else
// a peer_events row carries (collector, sysname, asn, session, local
// endpoint) is filled in by insertPeerEvents with one fixed, sufficient
// value, in insertPeerEvent's own spirit -- this package never writes
// production data, only enough of a fixture for its own queries to read
// back.
type peerEventRow struct {
	StreamSeq             uint64
	TsCollector, TsRouter time.Time
	Kind                  string
	DownReason            uint32
}

// eventsFixtureScope, eventsFixtureT and eventsFixtureN back eventsScopeFor:
// each top-level test in this file gets its own (router, peer) pair, so
// that TestPeerEventsPageOrdersOnTheCollectorClock's "exactly 2 rows" and
// TestPeerEventsPageWalksWithoutRepeatingOrDropping's "exactly 25" hold
// regardless of what other tests in this binary have written to
// peer_events -- chtest recreates the test database once per binary, not
// once per test function (see chtest.ensureDatabase), so every test in this
// package shares one growing table.
var (
	eventsFixtureMu    sync.Mutex
	eventsFixtureT     *testing.T
	eventsFixtureN     int
	eventsFixtureScope PeerEventFilter
)

// eventsScopeFor returns the (router, peer, rib) scope the CALLING test's
// fixture rows belong under: a freshly allocated one the first time a given
// *testing.T reaches it, and the same one on every later call from that
// same test. Two calls with different *testing.T values are two different
// tests and get two different scopes; two calls with the same *testing.T --
// TestPeerEventsPageWalksWithoutRepeatingOrDropping calls insertPeerEvents
// 25 times over -- get the identical one.
//
// This is what lets testScope(), below, take no arguments at all and still
// return the right answer: by the time a test calls it, that same test has
// already called insertPeerEvents at least once, which is what allocates
// the scope testScope() then reads back. Go runs this package's top-level
// tests sequentially unless one calls t.Parallel(), which none here do, so
// there is no race between one test's allocation and the next test's read
// of it.
func eventsScopeFor(t *testing.T) PeerEventFilter {
	t.Helper()
	eventsFixtureMu.Lock()
	defer eventsFixtureMu.Unlock()
	if eventsFixtureT != t {
		eventsFixtureN++
		eventsFixtureT = t
		eventsFixtureScope = PeerEventFilter{
			Router: netip.MustParseAddr(fmt.Sprintf("10.90.%d.1", eventsFixtureN)),
			Peer:   netip.MustParseAddr(fmt.Sprintf("10.90.%d.2", eventsFixtureN)),
			RIB:    "in_pre",
		}
	}
	return eventsFixtureScope
}

// testScope returns the calling test's own (router, peer, rib) scope,
// allocated by whichever insertPeerEvents call in that test ran first. See
// eventsScopeFor.
func testScope() PeerEventFilter {
	eventsFixtureMu.Lock()
	defer eventsFixtureMu.Unlock()
	return eventsFixtureScope
}

// insertPeerEvents writes one peer_events row per r, under the calling
// test's own scope (see eventsScopeFor). An empty Kind defaults to "up" and
// a zero TsRouter or TsCollector defaults to eventsFixtureAnchor, both
// because ClickHouse would otherwise reject the row outright -- kind is an
// Enum8 with no member named "", and DateTime64(6) does not accept Go's
// zero time.Time (year 1), so a test that only cares about, say, StreamSeq
// and DownReason is not forced to also spell out a Kind and two timestamps
// it does not care about.
func insertPeerEvents(t *testing.T, ctx context.Context, q *Q, rows ...peerEventRow) {
	t.Helper()
	scope := eventsScopeFor(t)
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events insert: %v", err)
	}
	for _, r := range rows {
		kind := r.Kind
		if kind == "" {
			kind = "up"
		}
		tsRouter := r.TsRouter
		if tsRouter.IsZero() {
			tsRouter = eventsFixtureAnchor
		}
		tsCollector := r.TsCollector
		if tsCollector.IsZero() {
			tsCollector = eventsFixtureAnchor
		}
		// Column order matches peer_events' own DDL exactly, the same
		// contract insertPeerEvent (routers_test.go) holds against
		// sink.insertPeer: collector_id, router_ip, router_sysname,
		// peer_ip, rib, peer_asn, peer_bgp_id, session_id, seq, ts_router,
		// ts_collector, parse_flags, stream_seq, kind, local_ip,
		// local_port, remote_port, down_reason, cap_four_byte_as.
		if err := b.Append(
			"events-fixture", scope.Router.String(), "events-fixture-router",
			scope.Peer.String(), scope.RIB, uint32(65000),
			scope.Peer.String(), uint64(1), r.StreamSeq, tsRouter, tsCollector,
			[]string{}, r.StreamSeq,
			kind, "::", uint16(0), uint16(0), r.DownReason, uint8(0),
		); err != nil {
			t.Fatalf("append peer_events row: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send peer_events batch: %v", err)
	}
}

func TestPeerEventsPageRequiresScope(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	_, _, err := q.PeerEventsPage(ctx, PeerEventFilter{})
	if err == nil {
		t.Fatal("expected an error: an unscoped events query is a full scan " +
			"against the sort key, which is why /v1/events requires router and peer")
	}
}

func TestPeerEventsPageOrdersOnTheCollectorClock(t *testing.T) {
	// The two clocks disagree in real data, and the table's ORDER BY stores
	// ts_router -- so ordering here is a deliberate choice against the key,
	// not a restatement of it. Sorting on ts_router would make the newest
	// event the one the router happened to timestamp last.
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPeerEvents(t, ctx, q,
		// older on the collector clock, NEWER on the router clock
		peerEventRow{StreamSeq: 1, TsCollector: at("10:00:00"), TsRouter: at("20:00:00"), Kind: "up"},
		peerEventRow{StreamSeq: 2, TsCollector: at("11:00:00"), TsRouter: at("09:00:00"), Kind: "down"},
	)
	rows, _, err := q.PeerEventsPage(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Kind != "down" {
		t.Fatalf("newest-first on ts_collector: want down first, got %+v", rows)
	}
}

func TestPeerEventsPageViewLostKeepsReasonZero(t *testing.T) {
	// The collector emits view_lost with no reason because the router said
	// nothing. Zero here is absence; a query that coerced it to anything else
	// would manufacture a router statement.
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPeerEvents(t, ctx, q, peerEventRow{StreamSeq: 1, Kind: "view_lost", DownReason: 0})
	rows, _, err := q.PeerEventsPage(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Kind != "view_lost" || rows[0].DownReason != 0 {
		t.Fatalf("view_lost must carry reason 0: got %+v", rows[0])
	}
}

// TestPeerEventsPageUnspecifiedKindPassesThrough pins the one Enum8 member
// the binding constraints call out by name: "unspecified" is a real value a
// caller can be told, not a placeholder for an event this endpoint could not
// classify, and it takes the identical argMax(pe.kind, pe.ts_collector) path
// every other kind does -- there is no branch anywhere that treats it
// differently, so this exists to prove that rather than assume it from the
// "up"/"down"/"view_lost" coverage the other tests already give.
func TestPeerEventsPageUnspecifiedKindPassesThrough(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPeerEvents(t, ctx, q, peerEventRow{StreamSeq: 1, Kind: "unspecified"})
	rows, _, err := q.PeerEventsPage(ctx, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != "unspecified" {
		t.Fatalf("kind=unspecified must pass through as itself, not be dropped, "+
			"defaulted or renamed: got %+v", rows)
	}
}

// TestPeerEventsPageResolvesAnUnmergedDuplicateToTheFreshestRow is the test
// for the decision that matters most here, and the one thing every other
// test in this file leaves unpinned: not "does the query dedup" in the
// abstract, but WHICH of a key's duplicate rows the answer
// comes from. peerEventsSQL's argMax(field, ts_collector) / max(ts_collector)
// over the scoped key says the freshest one, on the collector clock. Nothing
// enforced that -- inverting every argMax to argMin, or adding ts_collector
// to the GROUP BY so the dedup stops happening at all, passed this package
// and the api package alike, because no test anywhere wrote two rows sharing
// one (router_ip, peer_ip, rib, ts_router, stream_seq).
//
// The two rows below are that key twice over: same ts_router, same
// stream_seq, different ts_collector, and -- the part that makes the choice
// legible rather than arithmetic -- different KINDS. Getting this backwards
// reports a session as up when the archive's freshest copy of that same
// event says it went down, which is the operator-visible cost of the
// decision, not a detail of the aggregate.
//
// SYSTEM STOP MERGES and the raw/distinct precondition are load-bearing for
// the reason TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent spells
// out at length: peer_events is a ReplacingMergeTree, a merge collapsing
// these two rows keeps the last one INSERTED -- which is the same row argMax
// picks -- so a merge winning the race would leave this test passing under
// both mutations while proving nothing at all. A "precondition failed" here
// is this test failing to do its job, not this test passing.
func TestPeerEventsPageResolvesAnUnmergedDuplicateToTheFreshestRow(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	if err := q.conn.Exec(ctx, fmt.Sprintf("SYSTEM STOP MERGES %s.peer_events", q.db)); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	// context.WithoutCancel for counts_test.go's own reason: t's context is
	// already done when Cleanup runs, and leaving peer_events' merges off
	// would follow every later test and every later developer silently.
	t.Cleanup(func() {
		_ = q.conn.Exec(context.WithoutCancel(ctx),
			fmt.Sprintf("SYSTEM START MERGES %s.peer_events", q.db))
	})

	// Two calls, not one batch of two rows: ClickHouse's optimize_on_insert
	// collapses same-key duplicates within a single inserted block, so one
	// call would leave nothing to dedup at read time. Two inserts are two
	// parts, which is how a redelivered envelope actually arrives.
	//
	// The stale copy goes in FIRST, so that the freshest ts_collector and
	// the last-inserted row are the same row -- if a merge does slip past
	// SYSTEM STOP MERGES, the precondition below catches it rather than the
	// assertion silently agreeing for the wrong reason.
	const seq = 1
	stale, fresh := at("12:00:00"), at("12:00:05")
	insertPeerEvents(t, ctx, q, peerEventRow{
		StreamSeq: seq, TsRouter: at("12:00:00"), TsCollector: stale, Kind: "up",
	})
	insertPeerEvents(t, ctx, q, peerEventRow{
		StreamSeq: seq, TsRouter: at("12:00:00"), TsCollector: fresh,
		Kind: "down", DownReason: 3,
	})

	scope := testScope()
	var raw, distinct uint64
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(), uniqExact((router_ip, peer_ip, rib, ts_router, stream_seq))
		 FROM %s.peer_events WHERE router_ip = ?`, q.db),
		scope.Router.String()).Scan(&raw, &distinct); err != nil {
		t.Fatal(err)
	}
	if raw == distinct {
		t.Fatalf("precondition failed: count()=%d equals uniqExact()=%d, so the "+
			"duplicate was merged away and this test proves nothing", raw, distinct)
	}

	rows, _, err := q.PeerEventsPage(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	// One row, not two: the duplicate is one event the archive holds twice,
	// and reporting it twice would tell an operator this session flapped.
	if len(rows) != 1 {
		t.Fatalf("two rows at one (router, peer, rib, ts_router, stream_seq) came "+
			"back as %d rows, want 1 -- the dedup is not happening: %+v", len(rows), rows)
	}
	if rows[0].Kind != "down" || rows[0].DownReason != 3 {
		t.Errorf("the duplicate resolved to kind %q reason %d, want \"down\"/3 -- "+
			"the freshest copy on the collector clock is the answer, and the stale "+
			"one says this session was up", rows[0].Kind, rows[0].DownReason)
	}
	if !rows[0].TsCollector.Equal(fresh) {
		t.Errorf("latest_ts_collector = %s, want %s -- the row's own reported "+
			"collector clock must be the freshest copy's, not the stale one's",
			rows[0].TsCollector, fresh)
	}
}

func TestPeerEventsPageWalksWithoutRepeatingOrDropping(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	var want []uint64
	for i := 1; i <= 25; i++ {
		insertPeerEvents(t, ctx, q, peerEventRow{StreamSeq: uint64(i), TsCollector: atSeq(i), Kind: "up"})
		want = append(want, uint64(i))
	}
	var got []uint64
	f := testScope()
	f.Limit = 10
	for {
		rows, next, err := q.PeerEventsPage(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			got = append(got, r.StreamSeq)
		}
		if next == nil {
			break
		}
		f.Cursor = next
	}
	// A length check alone cannot tell "correct" from "repeated one row and
	// dropped a different one" -- the two errors cancel in the count. So
	// this asserts three separate things a repeat-and-drop defect cannot
	// satisfy all at once: no StreamSeq appears twice across the WHOLE
	// walk, every StreamSeq 1..25 is present, and the walk's own order is
	// exactly descending (atSeq gives row i the collector timestamp
	// anchor+i seconds, so newest-first is exactly 25, 24, ..., 1 -- a full
	// total order, not merely a set).
	seen := make(map[uint64]int, len(got))
	for _, sq := range got {
		seen[sq]++
	}
	for i := 1; i <= 25; i++ {
		switch n := seen[uint64(i)]; {
		case n == 0:
			t.Errorf("StreamSeq %d never came back (dropped at a page boundary)", i)
		case n > 1:
			t.Errorf("StreamSeq %d came back %d times (repeated at a page boundary)", i, n)
		}
	}
	wantOrder := make([]uint64, 25)
	for i := range wantOrder {
		wantOrder[i] = uint64(25 - i)
	}
	if !slices.Equal(got, wantOrder) {
		t.Fatalf("walk order = %v, want strictly descending %v", got, wantOrder)
	}
}

// TestPeerEventsPageSinceExcludesWhatCameBefore pins the one thing that
// actually bounds this walk's work: since the cursor does not (see
// PeerEventsPage's own doc comment on why a page here costs O(rows in
// scope), not O(page size)), a Since bound that is silently accepted and
// ignored would leave every one of this endpoint's callers reading the
// peer's ENTIRE recorded history on every page, forever, which is exactly
// the cost since exists to let a caller decline.
func TestPeerEventsPageSinceExcludesWhatCameBefore(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPeerEvents(t, ctx, q,
		peerEventRow{StreamSeq: 1, TsCollector: at("08:00:00"), Kind: "up"},
		peerEventRow{StreamSeq: 2, TsCollector: at("09:00:00"), Kind: "down"},
		peerEventRow{StreamSeq: 3, TsCollector: at("10:00:00"), Kind: "up"},
	)

	f := testScope()
	f.Since = at("09:00:00")
	rows, _, err := q.PeerEventsPage(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 -- since=09:00:00 is inclusive, so the "+
			"08:00:00 event (StreamSeq 1) must be excluded and the 09:00:00 and "+
			"10:00:00 events kept: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.StreamSeq == 1 {
			t.Errorf("StreamSeq 1 (ts_collector 08:00:00) survived a since=09:00:00 bound")
		}
		if r.TsCollector.Before(f.Since) {
			t.Errorf("event at %s is before the since bound %s", r.TsCollector, f.Since)
		}
	}
}

// TestPeerEventsPageRefusesACorruptPreEpochCursorPosition pins the guard on
// the write side of the cursor's ts_collector conversion. PeerEventsPage
// carries Last[0] as a uint64 (api/cursor.go's wire codec has no case for
// int64 -- see PeerEventsPage's own comment), and a bare uint64(micro) on a
// negative micro would wrap silently into a huge, wrong position rather
// than fail. No real peer_events row can predate the Unix epoch -- the
// table's own 90-day TTL rules it out long before that -- but the guard
// exists for exactly the row that should never occur, and this proves it
// fires rather than silently wraps.
func TestPeerEventsPageRefusesACorruptPreEpochCursorPosition(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	preEpoch := time.Date(1969, 1, 1, 0, 0, 0, 0, time.UTC)
	evenEarlier := time.Date(1968, 1, 1, 0, 0, 0, 0, time.UTC)
	// Three rows so the pre-epoch one lands at the END of a two-row page --
	// where its key becomes the cursor -- with a third, older row still
	// beyond it to prove another page exists. A pre-epoch row that never
	// reaches a page boundary would never exercise the guard at all.
	insertPeerEvents(t, ctx, q,
		peerEventRow{StreamSeq: 1, TsCollector: at("12:00:00"), Kind: "up"},
		peerEventRow{StreamSeq: 2, TsCollector: preEpoch, Kind: "up"},
		peerEventRow{StreamSeq: 3, TsCollector: evenEarlier, Kind: "up"},
	)
	f := testScope()
	f.Limit = 2
	if _, _, err := q.PeerEventsPage(ctx, f); err == nil {
		t.Error("PeerEventsPage with a pre-epoch row at a page boundary succeeded; " +
			"want a refusal rather than a cursor built from a wrapped, wrong position")
	}
}

// TestPeerEventsPageRejectsACursorFromADifferentScope pins the hazard
// RIBCursor's own doc comment describes at length for the RIB and
// link-state walks, applied here: a cursor is a POSITION, and a position
// only means something inside the walk that produced it. This walk has no
// session pin of its own to catch a mismatched cursor incidentally the way
// ribCursorScope's callers sometimes do, so it checks Router, Peer and RIB
// against the call it arrives on directly.
func TestPeerEventsPageRejectsACursorFromADifferentScope(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPeerEvents(t, ctx, q, peerEventRow{StreamSeq: 1, TsCollector: at("08:00:00"), Kind: "up"})
	scope := testScope()

	cursor := &RIBCursor{
		Router: scope.Router,
		// A peer this cursor was never issued for.
		Peer: netip.MustParseAddr("10.90.99.99"),
		RIB:  scope.RIB,
		Last: []any{uint64(eventsFixtureAnchor.UnixMicro()), uint64(1)},
	}
	f := scope
	f.Cursor = cursor
	if _, _, err := q.PeerEventsPage(ctx, f); !errors.Is(err, ErrBadFilter) {
		t.Errorf("PeerEventsPage with a cursor issued for a different peer error = "+
			"%v, want one wrapping ErrBadFilter", err)
	}
}

// TestFleetEventsCrossesPeersThatAScopedWalkCannotSee is the whole point of
// the unscoped mode, stated as a test: a scoped walk answers about ONE
// (router, peer), and this must answer about several in one response. Seeded
// with two distinct peers so a query that silently kept a scope predicate --
// or that returned the first group only -- comes back with one identity and
// fails here rather than looking plausible.
func TestFleetEventsCrossesPeersThatAScopedWalkCannotSee(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i, peer := range []struct{ router, peer string }{
		{"10.95.1.1", "10.95.1.2"},
		{"10.95.2.1", "10.95.2.2"},
	} {
		for j := range 3 {
			insertPeerEvent(t, ctx, q, peerEventFixture{
				RouterIP: peer.router, RouterSysname: "fleet-fixture-router",
				PeerIP: peer.peer, RIB: "in_pre",
				PeerASN: 65000, PeerBGPID: peer.peer,
				SessionID: 1, Seq: uint64(j + 1), StreamSeq: uint64(i*10 + j),
				Kind:        "up",
				TsRouter:    now.Add(-time.Duration(j) * time.Minute),
				TsCollector: now.Add(-time.Duration(j) * time.Minute),
			})
		}
	}

	got, err := q.FleetEvents(t.Context(), FleetEventFilter{
		Since: time.Now().Add(-24 * time.Hour),
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("FleetEvents: %v", err)
	}
	peers := map[netip.Addr]bool{}
	for _, e := range got {
		peers[e.PeerIP] = true
	}
	if len(peers) < 2 {
		t.Fatalf("FleetEvents returned %d distinct peers across %d rows, want at "+
			"least 2 -- an unscoped answer that carries one identity is a scoped "+
			"answer wearing a different name", len(peers), len(got))
	}
}

// TestFleetEventsIsNewestFirstOnTheCollectorClock pins the ORDER BY to the
// trustworthy clock. ts_router is deliberately seeded OUT of agreement with
// ts_collector on at least one row, so an implementation that ordered on the
// physical sort key's ts_router instead would produce a different sequence
// and fail here. Ordering on ts_router is not a slower answer, it is a wrong
// one: this project holds ts_router is never authoritative.
func TestFleetEventsIsNewestFirstOnTheCollectorClock(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Two distinct peers, each with ts_router deliberately out of step with
	// ts_collector: peerA is older on the collector clock but newer on the
	// router clock, and vice versa for peerB. An ORDER BY that sorted on
	// ts_router would put peerA's row first; the collector clock puts
	// peerB's row first instead.
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: "10.95.3.1", RouterSysname: "fleet-fixture-router",
		PeerIP: "10.95.3.2", RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: "10.95.3.2",
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Kind:        "up",
		TsRouter:    now,
		TsCollector: now.Add(-10 * time.Minute),
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: "10.95.4.1", RouterSysname: "fleet-fixture-router",
		PeerIP: "10.95.4.2", RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: "10.95.4.2",
		SessionID: 1, Seq: 1, StreamSeq: 2,
		Kind:        "down",
		TsRouter:    now.Add(-20 * time.Minute),
		TsCollector: now.Add(-5 * time.Minute),
	})

	got, err := q.FleetEvents(t.Context(), FleetEventFilter{
		Since: time.Now().Add(-24 * time.Hour),
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("FleetEvents: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].TsCollector.After(got[i-1].TsCollector) {
			t.Fatalf("row %d (ts_collector %s) is newer than row %d (%s): the walk "+
				"is not newest-first on the collector clock",
				i, got[i].TsCollector, i-1, got[i-1].TsCollector)
		}
	}
}

// TestFleetEventsRefusesAnUnboundedWindow is the query layer's half of the
// clamp. api/ refuses a too-wide window with a 400; this refuses NO window at
// all, which is the shape a caller reaches by constructing the filter
// directly (a dashboard, a future CLI) and bypassing api/ entirely. The two
// layers refuse for different reasons and neither may depend on the other's
// diligence -- the argument filters.wideASN already makes about origin_asn=0.
func TestFleetEventsRefusesAnUnboundedWindow(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	_, err := q.FleetEvents(t.Context(), FleetEventFilter{Limit: 100})
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("FleetEvents with no Since: err = %v, want ErrBadFilter -- an "+
			"unscoped events query with no window reads the whole table (2,040,000 "+
			"rows, 237-289ms, gigabytes resident: see "+UnscopedEventsMeasurement+")", err)
	}
	if _, err := q.CountFleetEvents(t.Context(), FleetEventFilter{Limit: 100}); !errors.Is(err, ErrBadFilter) {
		t.Fatalf("CountFleetEvents with no Since: err = %v, want ErrBadFilter -- "+
			"the count must refuse exactly what the list refuses, or a capped "+
			"answer reports a total for a query that was never allowed to run", err)
	}
}

// TestCountFleetEventsIgnoresLimitAndCountsTheWholeMatch is what makes
// meta.total_matched honest. A count that respected Limit would report
// "showing 5 of 5" for an answer that capped 40 rows -- the exact failure
// truncatedWarning's own doc comment describes.
func TestCountFleetEventsIgnoresLimitAndCountsTheWholeMatch(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i := range 12 {
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: "10.95.5.1", RouterSysname: "fleet-fixture-router",
			PeerIP: "10.95.5.2", RIB: "in_pre",
			PeerASN: 65000, PeerBGPID: "10.95.5.2",
			SessionID: 1, Seq: uint64(i + 1), StreamSeq: uint64(i + 1),
			Kind:        "up",
			TsRouter:    now.Add(-time.Duration(i) * time.Minute),
			TsCollector: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	since := time.Now().Add(-24 * time.Hour)
	rows, err := q.FleetEvents(t.Context(), FleetEventFilter{Since: since, Limit: 5})
	if err != nil {
		t.Fatalf("FleetEvents: %v", err)
	}
	total, err := q.CountFleetEvents(t.Context(), FleetEventFilter{Since: since, Limit: 5})
	if err != nil {
		t.Fatalf("CountFleetEvents: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("FleetEvents returned %d rows, want 5 (the Limit)", len(rows))
	}
	if total <= uint64(len(rows)) {
		t.Fatalf("CountFleetEvents = %d with %d rows returned, want strictly more "+
			"-- the count must ignore Limit, or a capped answer claims completeness",
			total, len(rows))
	}
}

// TestFleetEventsSinceExcludesWhatFallsOutsideIt distinguishes "the window
// filtered" from "the window was ignored and everything came back", which a
// test asserting only a row count cannot tell apart.
func TestFleetEventsSinceExcludesWhatFallsOutsideIt(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	now := time.Now().UTC().Truncate(time.Microsecond)

	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: "10.95.6.1", RouterSysname: "fleet-fixture-router",
		PeerIP: "10.95.6.2", RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: "10.95.6.2",
		SessionID: 1, Seq: 1, StreamSeq: 1,
		Kind:        "up",
		TsRouter:    now.Add(-30 * time.Minute),
		TsCollector: now.Add(-30 * time.Minute),
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: "10.95.6.1", RouterSysname: "fleet-fixture-router",
		PeerIP: "10.95.6.2", RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: "10.95.6.2",
		SessionID: 1, Seq: 2, StreamSeq: 2,
		Kind:        "down",
		TsRouter:    now.Add(-3 * time.Hour),
		TsCollector: now.Add(-3 * time.Hour),
	})

	got, err := q.FleetEvents(t.Context(), FleetEventFilter{
		Since: time.Now().Add(-2 * time.Hour),
		Limit: 1000,
	})
	if err != nil {
		t.Fatalf("FleetEvents: %v", err)
	}
	cutoff := time.Now().Add(-2 * time.Hour)
	for _, e := range got {
		if e.TsCollector.Before(cutoff) {
			t.Fatalf("row with ts_collector %s is older than the %s window: since "+
				"did not bound the read", e.TsCollector, cutoff)
		}
	}
	if len(got) == 0 {
		t.Fatal("no rows at all -- this test cannot tell a working filter from a " +
			"broken query; seed recent rows")
	}
}
