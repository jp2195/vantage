// Handler tests for GET /v1/events. See api/handlers_test.go's own header
// for why these run against a live ClickHouse rather than a mocked driver.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
)

// peerEventsCols names the nineteen peer_events columns these fixtures set,
// so their inserts are addressed by NAME rather than by position.
//
// Adding five columns (hold_time, hold_time_seen, mp_families,
// addpath_families, sys_descr) to peer_events broke every positional
// fixture in this package at once with "expected 24 arguments, got 19".
// That is the good version of the failure -- loud, immediate, and
// impossible to miss -- but
// it is also a failure none of these tests had an opinion about: they are
// about events and routers, not about capabilities. Named columns let the
// five take their DEFAULTs, so the next column added to the schema does not
// break a fixture that never mentioned it.
//
// The SINK's own insert stays positional on purpose. There, the argument
// count IS the check that the writer and the schema agree -- see the note
// above insertUnicast.
const peerEventsCols = "(collector_id, router_ip, router_sysname, peer_ip, rib, " +
	"peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, " +
	"parse_flags, stream_seq, kind, local_ip, local_port, remote_port, " +
	"down_reason, cap_four_byte_as)"

// eventsFixRouter and eventsFixPeer are the scope every test in this file
// asks about, deliberately outside insertHandlerFixture's own (fixRouterIP,
// fixPeerA/B) coordinates so that this file's rows never appear in that
// fixture's tests and vice versa.
const (
	eventsFixRouter = "10.0.0.1"
	eventsFixPeer   = "10.0.0.2"
)

// eventsFixSeq is a per-process counter for insertPeerEvent's own rows.
// Each call gets a stream_seq -- and a ts_router/ts_collector derived from
// it -- strictly after the one before, so the row a call just wrote sorts
// FIRST under PeerEventsPage's own ORDER BY (latest_ts_collector DESC,
// stream_seq DESC), regardless of what an earlier TestEvents* call already
// wrote at this same (router, peer) scope. That is what lets every test in
// this file read got.Data[0] and mean "the row I just inserted" rather
// than depending on run order or on ReplacingMergeTree's own merge timing.
var eventsFixSeq atomic.Uint64

// insertPeerEvent writes one peer_events row for (eventsFixRouter,
// eventsFixPeer) under rib "in_pre" with the given kind and down_reason,
// and returns once it is durably visible to a query (Send blocks for the
// ack). It returns the seq this row was written with, which is unique per
// call and lets a test that inserts several rows identify which one a page
// returned.
func insertPeerEvent(t *testing.T, kind string, downReason uint32) uint64 {
	t.Helper()
	return insertPeerEventRIB(t, kind, downReason, "in_pre")
}

// insertPeerEventRIB is insertPeerEvent with the rib as a parameter, for a
// test that needs its own rows isolated from the ones every other test in
// this file writes under "in_pre" -- rib= narrows a request the same way
// it narrows every other RIB and link-state walk, so a distinct rib is
// this table's own way of giving a test a private scope without a private
// database.
func insertPeerEventRIB(t *testing.T, kind string, downReason uint32, rib string) uint64 {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)

	// ts is anchored to NOW, not a fixed date: the handler defaults ?since=
	// to 1h ago (api/handlers.go's since), and every test in this file
	// queries with no since= at all, matching how a caller with no time
	// filter behaves. A fixed historical timestamp -- insertHandlerFixture's own
	// 2026-08-01, chosen because nothing there reads it against the
	// default window -- would fall outside that default and never appear.
	n := eventsFixSeq.Add(1)
	ts := time.Now().UTC().Add(time.Duration(n) * time.Millisecond)

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	// Column order matches insertHandlerFixture's own peer_events insert:
	// collector_id, router_ip, router_sysname, peer_ip, rib, peer_asn,
	// peer_bgp_id, session_id, seq, ts_router, ts_collector, parse_flags,
	// stream_seq, kind, local_ip, local_port, remote_port, down_reason,
	// cap_four_byte_as.
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(eventsFixRouter), fixSysName,
		netip.MustParseAddr(eventsFixPeer), rib, uint32(65001),
		netip.MustParseAddr(eventsFixRouter), uint64(fixSession), n,
		ts, ts, []string{}, n,
		kind, netip.MustParseAddr(eventsFixRouter), uint16(179), uint16(50000),
		downReason, uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
	return n
}

// insertPeerEventFixed writes one peer_events row at a caller-chosen,
// non-time-varying identity: ts (used for both ts_router and ts_collector)
// and streamSeq (used for both seq and stream_seq) together fix the row's
// (router_ip, peer_ip, rib, ts_router, stream_seq) -- peer_events' own
// ReplacingMergeTree key -- so a second call with the same arguments
// overwrites this exact row rather than adding another one. See
// TestEventsCursorWalksWithoutRepeatingOrDropping's own fixture comment for
// why that property is what a cursor-walk test needs and insertPeerEvent's
// time-varying identity cannot give it.
func insertPeerEventFixed(t *testing.T, kind string, downReason uint32,
	rib string, ts time.Time, streamSeq uint64) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	// Column order matches insertHandlerFixture's own peer_events insert;
	// see insertPeerEventRIB's own comment.
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(eventsFixRouter), fixSysName,
		netip.MustParseAddr(eventsFixPeer), rib, uint32(65001),
		netip.MustParseAddr(eventsFixRouter), uint64(fixSession), streamSeq,
		ts, ts, []string{}, streamSeq,
		kind, netip.MustParseAddr(eventsFixRouter), uint16(179), uint16(50000),
		downReason, uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
}

// TestEventsRequiresRouterAndPeer holds the scope rule that still applies:
// a HALF scope -- router= alone, or peer= alone -- is a 400, because it is
// neither a scoped walk (which needs both) nor a fleet-wide question (which
// needs neither). It used to also assert "" (no scope at all) is a 400; that
// case was deleted here because the unscoped mode added here is
// precisely what replaces it -- "" is now a 200 with a real fleet-wide
// answer, by design, and is covered by TestEventsUnscopedAnswersAcrossTheFleet
// and the rest of this file's "Unscoped /v1/events" section. Keeping the ""
// case here would have asserted the behavior this design deliberately
// reverses; see TestEventsStillRefusesAHalfScope for the same half-scope
// contract asserted against the message text as well as the status code.
func TestEventsRequiresRouterAndPeer(t *testing.T) {
	s := requireAPI(t)
	for _, q := range []string{"?router=10.0.0.1", "?peer=10.0.0.2"} {
		rec := get(t, s, "/v1/events"+q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: got %d, want 400", q, rec.Code)
		}
	}
}

// TestEventsNamesTheDownReasonBesideTheNumber: protocol_name on LsLink sets
// this contract -- the registry name AND the number, so a caller is never
// forced to trust the decoding.
func TestEventsNamesTheDownReasonBesideTheNumber(t *testing.T) {
	s := requireAPI(t)
	insertPeerEvent(t, "down", 3) // 3 = remote system closed, notification follows
	rec := get(t, s, "/v1/events?router=10.0.0.1&peer=10.0.0.2")
	var got Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got.Data.([]any)[0].(map[string]any)
	if e["down_reason"] != float64(3) || e["reason_name"] != "remote system closed, notification follows" {
		t.Fatalf("want reason 3 with its registry name, got %+v", e)
	}
}

// TestEventsLeavesViewLostReasonUnnamed: down_reason is zero on view_lost
// because the router said nothing. Naming it would invent a router
// statement; "" is the positive answer.
func TestEventsLeavesViewLostReasonUnnamed(t *testing.T) {
	s := requireAPI(t)
	insertPeerEvent(t, "view_lost", 0)
	rec := get(t, s, "/v1/events?router=10.0.0.1&peer=10.0.0.2")
	var got Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got.Data.([]any)[0].(map[string]any)
	if e["kind"] != "view_lost" || e["down_reason"] != float64(0) || e["reason_name"] != "" {
		t.Fatalf("view_lost must carry reason 0 with no name, got %+v", e)
	}
}

// TestEventsNeverNamesAReasonOnAKindThatIsNotDown is
// TestEventsLeavesViewLostReasonUnnamed's own guard made observable against
// the mutation it is meant to catch. down_reason 0 is not in
// peerDownReasons either, so a view_lost row with reason 0 passes that test
// whether or not reasonName's "kind != down" check exists at all -- 0
// simply is not a registered code, guard or no guard. This inserts a
// view_lost with a down_reason that IS registered (real BMP data never
// does this; peer_events' own column has no constraint tying the two
// together, so the wire layer is what has to refuse it), which is the one
// input where dropping the guard is observable: without it, reason_name
// would report a router statement view_lost never carries.
func TestEventsNeverNamesAReasonOnAKindThatIsNotDown(t *testing.T) {
	s := requireAPI(t)
	insertPeerEvent(t, "view_lost", 3) // 3 IS registered; kind is not "down"
	rec := get(t, s, "/v1/events?router=10.0.0.1&peer=10.0.0.2")
	var got Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got.Data.([]any)[0].(map[string]any)
	if e["reason_name"] != "" {
		t.Fatalf("view_lost must never carry a reason name, even for a "+
			"down_reason code that IS registered, got %+v", e)
	}
}

// TestEventsUnknownReasonCodeIsUnnamedNotGuessed pins that a down code
// outside the registry gets no name -- never a guess.
func TestEventsUnknownReasonCodeIsUnnamedNotGuessed(t *testing.T) {
	s := requireAPI(t)
	insertPeerEvent(t, "down", 99)
	rec := get(t, s, "/v1/events?router=10.0.0.1&peer=10.0.0.2")
	var got Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	e := got.Data.([]any)[0].(map[string]any)
	if e["reason_name"] != "" {
		t.Fatalf("an unregistered code gets no name, got %q", e["reason_name"])
	}
}

// walkFixBase and walkFixRIB anchor TestEventsCursorWalksWithoutRepeatingOrDropping's
// three rows at a FIXED, non-time-varying identity, unlike insertPeerEvent's
// (see its own doc comment on why that one is anchored to now instead). A
// walk test re-run minutes, days or a hundred `-count` iterations later
// must see exactly the three rows it wrote, not those plus every row an
// earlier run of the same test left behind in a database this package
// never truncates -- so each row here lands at the SAME
// (router_ip, peer_ip, rib, ts_router, stream_seq), peer_events' own
// ReplacingMergeTree key, and a re-run overwrites rather than
// accumulates. The date is fixed and arbitrary, chosen only to sit inside
// peer_events' 90-day TTL for the foreseeable life of this test.
var (
	walkFixBase = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	walkFixRIB  = "out_pre" // isolates from insertPeerEvent's own "in_pre" rows
)

// TestEventsCursorWalksWithoutRepeatingOrDropping exercises the round trip
// none of this file's other tests do: every one of them reads a single page
// at the default limit, so none actually sends a next_cursor back to the
// handler. This is the codec's genuinely hard part in practice -- a cursor
// query.PeerEventsPage issues with no session pin, carried across a real
// HTTP request, decoded by params.unpinnedCursor, and handed back to
// PeerEventsPage on page 2 -- and TestHandlerRIBUnicastWalk's own "full walk
// at limit 1" subtest is the pattern this mirrors for /v1/rib/unicast.
func TestEventsCursorWalksWithoutRepeatingOrDropping(t *testing.T) {
	s := requireAPI(t)

	want := map[string]bool{}
	for i, kr := range []struct {
		kind   string
		reason uint32
	}{{"up", 0}, {"down", 3}, {"view_lost", 0}} {
		seq := uint64(i + 1)
		insertPeerEventFixed(t, kr.kind, kr.reason,
			walkFixRIB, walkFixBase.Add(time.Duration(i)*time.Second), seq)
		want[u64(seq)] = true
	}

	seen := map[string]bool{}
	sinceQ := url.QueryEscape(walkFixBase.Add(-time.Hour).Format(time.RFC3339))
	target := "/v1/events?router=10.0.0.1&peer=10.0.0.2&rib=" + walkFixRIB + "&since=" + sinceQ + "&limit=1"
	for page := 1; ; page++ {
		if page > 10 {
			t.Fatal("the walk did not terminate in 10 pages; only 3 rows were inserted")
		}
		rec := get(t, s, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: got %d, want 200; body %s", page, rec.Code, rec.Body.String())
		}
		var env struct {
			Data []map[string]any `json:"data"`
			Meta Meta             `json:"meta"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("page %d: decode: %v; body %s", page, err, rec.Body.String())
		}
		if len(env.Data) != 1 {
			t.Fatalf("page %d returned %d rows at limit=1", page, len(env.Data))
		}
		seq, _ := env.Data[0]["seq"].(string)
		if seen[seq] {
			t.Fatalf("page %d repeated seq %s -- the cursor did not advance", page, seq)
		}
		seen[seq] = true
		if env.Meta.NextCursor == nil {
			break
		}
		target = "/v1/events?router=10.0.0.1&peer=10.0.0.2&rib=" + walkFixRIB + "&since=" + sinceQ +
			"&limit=1&cursor=" + *env.Meta.NextCursor
	}
	for seq := range want {
		if !seen[seq] {
			t.Errorf("the walk never returned seq %s", seq)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("the walk saw %d rows, want %d: %v", len(seen), len(want), seen)
	}
}

// TestEventsCursorCarriesNoSessionPin asserts the actual claim api/cursor.go's
// header and query.PeerEventsPage's own doc comment make, directly against a
// cursor this endpoint really issued rather than a hand-built one: Collector
// and SessionID are the zero values, not merely "whatever the request
// happened to carry". A regression that started copying the request's own
// collector/session into the cursor would round-trip fine and pass every
// other test in this file; this is the one that would catch it.
func TestEventsCursorCarriesNoSessionPin(t *testing.T) {
	s := requireAPI(t)
	insertPeerEvent(t, "up", 0)
	insertPeerEvent(t, "down", 3)

	var rows []map[string]any
	meta := getOK(t, s, "/v1/events?router=10.0.0.1&peer=10.0.0.2&limit=1", &rows)
	if meta.NextCursor == nil {
		t.Fatal("2 rows at limit=1 must carry a next_cursor for page 2")
	}
	c, err := decodeCursorUnpinned(*meta.NextCursor)
	if err != nil {
		t.Fatalf("decode the cursor /v1/events itself issued: %v", err)
	}
	if c.Collector != "" {
		t.Errorf("Collector = %q, want \"\" -- /v1/events' cursor must carry no session pin", c.Collector)
	}
	if c.SessionID != 0 {
		t.Errorf("SessionID = %d, want 0 -- /v1/events' cursor must carry no session pin", c.SessionID)
	}
}

// --- Unscoped /v1/events: harness and fixtures ---
//
// requireAPI's own Config (handlers_test.go) sets MaxUnscopedSince to
// 24 * time.Hour, mirroring defaultMaxUnscopedSince -- see requireAPI's own
// doc comment for why a Config built directly (as every test file's is,
// rather than through LoadConfig) cannot leave that field at its zero value
// without checkUnscopedWindow refusing every unscoped request this package's
// tests serve. requireAPIWithMaxUnscopedSince exists beside it anyway, for a
// test that wants the window's exact value to be visible at its own call
// site rather than inherited from requireAPI's default -- every test below
// but one calls it with 24*time.Hour, the same value requireAPI now
// carries, so for those it is a documentation choice rather than a
// behavioral one. The exception is
// TestEventsUnscopedDefaultWindowExactlyAtTheConfiguredMaximumSucceeds,
// which calls it with time.Hour specifically because that test needs a
// DIFFERENT window -- the boundary itself, not 24h's -- so its call is
// behavioral, following requireAPIWithMaxPage's own shape.
//
// eventsFleetRouterA/B and eventsFleetPeerA/B give the unscoped tests below
// two (router, peer) identities distinct from eventsFixRouter/eventsFixPeer,
// so a fleet-wide answer can be told apart from a scoped one that merely
// forgot to filter. insertPeerEventIdentity generalizes
// insertPeerEventFixed two ways this file's existing helpers never needed:
// a caller-chosen identity, and a caller-chosen, non-time-varying (ts,
// streamSeq) pair so that re-running the same test inside the same
// eventsFleetBucket() overwrites peer_events' own ReplacingMergeTree key
// (and collapses under peerEventsSQL's GROUP BY) instead of accumulating a
// fresh row every time -- exactly the property walkFixBase's own doc
// comment describes, needed here because several of these tests assert a
// COUNT rather than only "row 0".

const (
	eventsFleetRouterA = "10.0.9.1"
	eventsFleetPeerA   = "10.0.9.2"
	eventsFleetRouterB = "10.0.9.3"
	eventsFleetPeerB   = "10.0.9.4"
)

// requireAPIWithMaxUnscopedSince is requireAPI with cfg.MaxUnscopedSince set
// explicitly rather than defaulted to 24h. See this section's own header
// comment for why an unscoped /v1/events test cannot use plain requireAPI,
// and requireAPIBuilt (handlers_test.go) for the body every requireAPIWith*
// helper in this package shares.
func requireAPIWithMaxUnscopedSince(t *testing.T, d time.Duration) *Server {
	t.Helper()
	return requireAPIBuilt(t, 1000, 10000, d)
}

// eventsFleetBucket is a timestamp inside the CURRENT hour, stable across
// repeated calls within one half of it, so a mutation-testing loop that
// re-runs the same test many times keeps landing on the SAME (ts_router,
// stream_seq) key rather than adding a new row every time.
//
// It is the top of the hour PLUS 30 minutes, not the top of the hour alone:
// Truncate(time.Hour) by itself can be up to 59m59s old, which leaves only
// fractions of a second of margin against the unscoped mode's 1h default
// window (p.since) -- a row inserted at, say, 10:59:59.9 truncates to
// 10:00:00 and is already at the window's edge the instant it lands, so a
// request whose own `now` (computed a moment later, in handleEvents) has
// crossed 11:00:00 sees a since= bound past that row's timestamp and drops
// it. +30 minutes buys a full 30 minutes of margin instead of a few
// milliseconds, which is what makes this fix real rather than cosmetic.
//
// Guarded against the FUTURE: for the first half of every hour, top-of-hour
// plus 30 minutes has not happened yet, so this falls back to the top of
// the hour itself for that half -- still fixed for its own 30-minute
// stretch, and always <= now.
func eventsFleetBucket() time.Time {
	now := time.Now().UTC()
	top := now.Truncate(time.Hour)
	if t := top.Add(30 * time.Minute); !t.After(now) {
		return t
	}
	return top
}

// insertPeerEventIdentity is insertPeerEventFixed generalized to a
// caller-chosen (router, peer). See this section's own header comment.
func insertPeerEventIdentity(t *testing.T, router, peer, rib, kind string,
	downReason uint32, ts time.Time, streamSeq uint64) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(router), fixSysName,
		netip.MustParseAddr(peer), rib, uint32(65001),
		netip.MustParseAddr(router), uint64(fixSession), streamSeq,
		ts, ts, []string{}, streamSeq,
		kind, netip.MustParseAddr(router), uint16(179), uint16(50000),
		downReason, uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
}

// seedFleetPeers writes one recent event at each of two (router, peer)
// identities distinct from eventsFixRouter/eventsFixPeer, timestamped to
// eventsFleetBucket(). It is shared by every test below that needs an
// unscoped answer to be visibly non-empty AND to visibly span more than one
// peer -- TestEventsUnscopedAnswersAcrossTheFleet and
// TestEventsMalformedScopeIsAParseErrorNotAFleetQuery both need exactly
// that, for different reasons documented on each.
func seedFleetPeers(t *testing.T) {
	t.Helper()
	bucket := eventsFleetBucket()
	insertPeerEventIdentity(t, eventsFleetRouterA, eventsFleetPeerA, "loc_rib", "up", 0, bucket, 1)
	insertPeerEventIdentity(t, eventsFleetRouterB, eventsFleetPeerB, "loc_rib", "up", 0, bucket, 1)
}

// someValidCursorToken is a syntactically well-formed cursor -- one
// decodeCursorUnpinned accepts -- for TestEventsUnscopedRefusesACursor. Its
// position does not matter: eventsUnscoped's guard fires on the cursor's
// PRESENCE, before any walk it might describe is ever read.
var someValidCursorToken = encodeCursor(query.RIBCursor{
	Router: netip.MustParseAddr(eventsFixRouter),
	Peer:   netip.MustParseAddr(eventsFixPeer),
	Last:   []any{uint64(0), uint64(0)},
})

// TestEventsUnscopedAnswersAcrossTheFleet is the mode's reason to exist. It
// asserts more than one identity in the response, not merely a 200: an
// unscoped request that came back with one peer's rows would be a scoped
// answer with the scope hidden, and a status-code assertion cannot tell them
// apart.
func TestEventsUnscopedAnswersAcrossTheFleet(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedFleetPeers(t)

	res := get(t, s, "/v1/events") // no router, no peer
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", res.Code, res.Body)
	}
	var body struct {
		Data []WirePeerEvent `json:"data"`
		Meta Meta            `json:"meta"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, res.Body)
	}
	peers := map[string]bool{}
	for _, e := range body.Data {
		peers[e.PeerIP] = true
	}
	if len(peers) < 2 {
		t.Fatalf("unscoped /v1/events returned %d distinct peers, want >= 2", len(peers))
	}
}

// TestEventsUnscopedNeverHandsBackACursor: meta.next_cursor is present and
// null, never a token. A capped answer that carried a cursor would be
// offering a page 2 this mode cannot serve -- the same rule /v1/ls/nodes
// states as "a request never receives a cursor it could not legally resend
// alongside its own filter".
func TestEventsUnscopedNeverHandsBackACursor(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	bucket := eventsFleetBucket()
	for i, seq := range []uint64{501, 502, 503} {
		insertPeerEventIdentity(t, eventsFixRouter, eventsFixPeer, "loc_rib",
			"up", 0, bucket.Add(time.Duration(i)*time.Second), seq)
	}

	res := get(t, s, "/v1/events?limit=2")
	var body struct {
		Meta Meta `json:"meta"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, res.Body)
	}
	if body.Meta.NextCursor != nil {
		t.Fatalf("meta.next_cursor = %q, want null on an unscoped answer", *body.Meta.NextCursor)
	}
	if !bytes.Contains(res.Body.Bytes(), []byte(`"next_cursor":null`)) {
		t.Error("meta.next_cursor must be present and null, not absent")
	}
}

// TestEventsUnscopedReportsTheTotalItCapped is the server half of the
// screen's "showing 500 of 12,431". It asserts the two numbers DIFFER --
// total strictly greater than the rows returned -- which is what
// distinguishes a real cap from an answer that happened to fit.
func TestEventsUnscopedReportsTheTotalItCapped(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	bucket := eventsFleetBucket()
	for i := range 10 {
		insertPeerEventIdentity(t, eventsFixRouter, eventsFixPeer, "loc_rib",
			"up", 0, bucket.Add(time.Duration(i)*time.Second), uint64(601+i))
	}

	res := get(t, s, "/v1/events?limit=2")
	var body struct {
		Data []WirePeerEvent `json:"data"`
		Meta Meta            `json:"meta"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, res.Body)
	}
	if len(body.Data) != 2 {
		t.Fatalf("returned %d rows, want 2", len(body.Data))
	}
	if body.Meta.TotalMatched == nil {
		t.Fatal("meta.total_matched is null on a capped answer")
	}
	if *body.Meta.TotalMatched <= uint64(len(body.Data)) {
		t.Fatalf("total_matched = %d with %d rows returned; a cap that reports its "+
			"own page size tells a caller nothing was left out",
			*body.Meta.TotalMatched, len(body.Data))
	}
	var truncated bool
	for _, w := range body.Meta.Warnings {
		if w.Code == WarnTruncated {
			truncated = true
		}
	}
	if !truncated {
		t.Error("no truncated warning on a capped answer")
	}
}

// TestEventsUnscopedSaysNothingWasLeftOutWhenNothingWas is the other side of
// the same rule, and it is what stops the truncated warning from being
// emitted unconditionally. An empty warnings array is a positive claim that
// the answer is complete; a cap that always warns destroys that claim.
//
// It reads meta through getMeta, not a bare get()+json.Unmarshal, because
// this test's only assertion is a negative loop over body.Meta.Warnings --
// and an ErrorResponse body ({"error":{...}}) unmarshals into that same
// struct shape with zero warnings, same as a genuinely complete 200 answer
// does. Every sibling test in this file is protected from a silent error
// response by something else in its own assertions (an exact row count, the
// "next_cursor":null check, an explicit 200), which is what made this the
// one gap: read the wrong way, this test would go green on a 400, a 401 or
// a 500 exactly as readily as on the 200 it actually means to check.
// getMeta (handlers_test.go) enforces the 200 this test's own claim depends
// on.
//
// The request narrows to rib=out_post, a value no other test in this file
// writes: this endpoint is unscoped by identity, so an exact-count assertion
// like this one is the one kind this file's shared, persistent ClickHouse
// database cannot give for free the way TestEventsCursorCarriesNoSessionPin
// and its siblings do by reading only row 0 -- see this file's own header
// for why every test here can otherwise share state. A private rib is this
// table's own way of giving this one test a private scope without a private
// database, exactly as walkFixRIB already does for the cursor-walk test.
func TestEventsUnscopedSaysNothingWasLeftOutWhenNothingWas(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	bucket := eventsFleetBucket()
	for i, seq := range []uint64{801, 802, 803} {
		insertPeerEventIdentity(t, eventsFixRouter, eventsFixPeer, "out_post",
			"up", 0, bucket.Add(time.Duration(i)*time.Second), seq)
	}

	meta := getMeta(t, s, "/v1/events?limit=100&rib=out_post")
	for _, w := range meta.Warnings {
		if w.Code == WarnTruncated {
			t.Fatal("truncated warning on an answer that returned everything it matched")
		}
	}
}

// TestEventsStillRefusesAHalfScope holds the line /v1/ls/nodes
// does NOT hold: there, router= alone is an unscoped request filtered to one
// router. Here it is a 400, and it must stay one -- an operator who typed
// router= and forgot peer= asked a scoped question and must not silently
// receive a fleet-wide answer instead.
//
// It checks the body for requiredAddr's own "X= is required" wording, not
// only the status code. A status-code-only check is vacuous against the
// switch's own default branch being bypassed: if a half scope fell through
// to eventsScoped instead (say, router.IsValid() && peer.IsValid() loosened
// to ||), the missing address would still reach query.PeerEventFilter.check
// as the zero netip.Addr and still 400 -- via a different message
// (query.ErrBadFilter's "/v1/events needs router/peer") than the one
// handleEvents' own doc comment promises stays byte for byte unchanged.
// This assertion is what tells those two 400s apart; verified against the
// mutation table's own #2.
func TestEventsStillRefusesAHalfScope(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	cases := []struct {
		q        string
		wantName string // the parameter requiredAddr's own message should name
	}{
		{"/v1/events?router=10.0.103.62", "peer"},
		{"/v1/events?peer=10.0.0.80", "router"},
	}
	for _, c := range cases {
		res := get(t, s, c.q)
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 -- a half scope is neither a scoped "+
				"walk nor a fleet-wide question", c.q, res.Code)
			continue
		}
		if !strings.Contains(res.Body.String(), c.wantName+"= is required") {
			t.Errorf("%s: body = %s, want requiredAddr's own %q wording -- a 400 from "+
				"a different code path is not the SAME 400", c.q, res.Body, c.wantName+"= is required")
		}
	}
}

// TestEventsUnscopedRefusesAWindowPastTheConfiguredMaximum. The message must
// name the limit, so an operator reading a 400 learns what to change; and it
// must name the measurement, so a later reader can find it rather
// than assuming the limit is arbitrary. This is deliberate: the clamp
// gets a test naming the measured reason.
func TestEventsUnscopedRefusesAWindowPastTheConfiguredMaximum(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	res := get(t, s, "/v1/events?since=720h")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a 30-day unscoped window", res.Code)
	}
	body := res.Body.String()
	if !strings.Contains(body, "24h") {
		t.Errorf("the 400 does not name the limit an operator would have to raise: %s", body)
	}
	if !strings.Contains(body, query.UnscopedEventsMeasurement) {
		t.Errorf("the 400 does not name the measurement behind the limit: %s", body)
	}
}

// TestCitedUnscopedEventsMeasurementExists guards the citation
// query.UnscopedEventsMeasurement names, which this file's own test above,
// checkUnscopedWindow's 400 body, and FleetEventFilter.check's ErrBadFilter
// (query/events.go) all quote verbatim. Nothing else in this repo verifies
// that the file still exists on disk AND still has the section the anchor
// names, so a rename of either would leave every one of those citations
// dangling while every test that only greps the error body for the
// constant's own value -- this one's neighbor included -- stayed green.
func TestCitedUnscopedEventsMeasurementExists(t *testing.T) {
	file, anchor, ok := strings.Cut(query.UnscopedEventsMeasurement, "#")
	if !ok || anchor == "" {
		t.Fatalf("query.UnscopedEventsMeasurement = %q has no #anchor naming a section",
			query.UnscopedEventsMeasurement)
	}
	body, err := os.ReadFile("../" + file)
	if err != nil {
		t.Fatalf("the document api/handlers.go and query/events.go cite by name in "+
			"error bodies no longer exists at %s: %v", file, err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		heading, isHeading := strings.CutPrefix(line, "## ")
		if isHeading && markdownAnchor(heading) == anchor {
			return
		}
	}
	t.Fatalf("api/handlers.go and query/events.go cite section #%s in error bodies, "+
		"but %s has no \"## \" heading with that anchor", anchor, file)
}

// markdownAnchor is the fragment GitHub generates for a heading: lowercased,
// punctuation other than '-' and '_' dropped, spaces turned into hyphens.
// query/collection_test.go carries the same function for its own citations.
func markdownAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestEventsUnscopedDefaultWindowExactlyAtTheConfiguredMaximumSucceeds pins
// checkUnscopedWindow's boundary as INCLUSIVE (<=, not <), against the one
// configuration where that choice actually bites: p.since resolves an
// absent since= to exactly now - historyDefaultSince (1h), and handleEvents
// passes ONE shared now to both p.since and checkUnscopedWindow -- so a
// request naming no window at all produces now.Sub(since) ==
// cfg.MaxUnscopedSince exactly when an operator configures
// max_unscoped_since: 1h, a value LoadConfig permits (it refuses only
// strictly BELOW historyDefaultSince, never equal to it). Were the
// comparison strict instead, that operator's own daemon would 400 its own
// default answer to a bare GET /v1/events, and nothing in api/ or query/
// would notice -- this is the test that would.
func TestEventsUnscopedDefaultWindowExactlyAtTheConfiguredMaximumSucceeds(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, time.Hour)
	res := get(t, s, "/v1/events")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- a default (no since=) unscoped request "+
			"against max_unscoped_since=1h sits exactly at the boundary, and the "+
			"boundary is inclusive; body %s", res.Code, res.Body)
	}
}

// TestEventsUnscopedMalformedSinceIsAParseErrorNotAClampViolation guards the
// ordering inside eventsUnscoped: p.err is checked BEFORE
// checkUnscopedWindow, so a malformed since= is reported as a parse failure
// on since= rather than as a clamp violation computed against the zero time
// p.since leaves behind on a parse failure. Reversing that order would still
// 400 -- now.Sub(zero time) is thousands of years, which trivially exceeds
// any configured MaxUnscopedSince -- but for the wrong stated reason, and an
// operator who mistyped since= would be told to narrow a window they never
// asked to widen. None of this file's other new tests exercises this
// specific malformed value; this one exists so mutation 7 (moving the p.err
// check after the clamp) has something to fail against.
func TestEventsUnscopedMalformedSinceIsAParseErrorNotAClampViolation(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	res := get(t, s, "/v1/events?since=notaduration")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unparseable since=", res.Code)
	}
	body := res.Body.String()
	if !strings.Contains(body, "since=") {
		t.Errorf("body does not name since= as the failed parameter: %s", body)
	}
	if strings.Contains(body, "max_unscoped_since") {
		t.Errorf("body names the window clamp instead of the parse failure on since=: %s", body)
	}
}

// TestEventsScopedIsNotClamped holds a deliberate decision: scoped, a wide
// since is still a seek, and refusing it would be gratuitous, since
// refusing to answer is not the same as explaining why an answer is
// empty. Same window, same daemon, opposite answer: that pairing is the
// test, and either half alone would pass against a clamp applied
// everywhere.
func TestEventsScopedIsNotClamped(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	res := get(t, s, "/v1/events?router=10.0.103.62&peer=10.0.0.80&since=720h")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a scoped walk is a seek and is not clamped", res.Code)
	}
}

// TestEventsUnscopedWithNoSinceUsesTheDefaultWindow: an absent ?since= must
// land on the cheap end of the measurement, not be treated as unbounded.
//
// The request narrows to rib=in_post for the reason
// TestEventsUnscopedSaysNothingWasLeftOutWhenNothingWas's own comment gives:
// this is an exact-count assertion ("== 1"), and this file's shared database
// otherwise carries fresh, recent rows from every other test that inserts
// one -- in_post is a value no other test in this file writes to.
func TestEventsUnscopedWithNoSinceUsesTheDefaultWindow(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	bucket := eventsFleetBucket()
	insertPeerEventIdentity(t, eventsFixRouter, eventsFixPeer, "in_post",
		"up", 0, bucket, 701) // well inside the 1h default window
	insertPeerEventIdentity(t, eventsFixRouter, eventsFixPeer, "in_post",
		"up", 0, bucket.Add(-4*time.Hour), 702) // well outside it

	res := get(t, s, "/v1/events?rib=in_post")
	var body struct {
		Data []WirePeerEvent `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body %s", err, res.Body)
	}
	if len(body.Data) != 1 {
		t.Fatalf("returned %d rows, want 1 -- an absent since= must bound at the "+
			"1h default, not read everything", len(body.Data))
	}
}

// TestEventsUnscopedRefusesACursor: a cursor is a position inside a walk,
// and this mode is not a walk.
func TestEventsUnscopedRefusesACursor(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	res := get(t, s, "/v1/events?cursor="+someValidCursorToken)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a cursor with no scope", res.Code)
	}
}

// TestEventsMalformedScopeIsAParseErrorNotAFleetQuery guards the seam
// between the mode switch and the parameter accumulator, which is where
// this design's one non-obvious path runs.
//
// p.addr returns the INVALID Addr both when a parameter is absent and when
// it failed to parse -- the failure is recorded on p.err, not in the return
// value. So router=notanip yields an invalid router, an invalid peer, and a
// switch that reads "neither half named" and dispatches to the UNSCOPED
// branch. The caller must still get "router= is not an IP address", never a
// fleet-wide answer to a question they scoped and mistyped, and never the
// window-clamp message about a since= they did not send.
//
// It works only because eventsUnscoped checks p.err before it queries. That
// ordering is load-bearing and is invisible from either function alone,
// which is why this is a test and not a comment.
func TestEventsMalformedScopeIsAParseErrorNotAFleetQuery(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedFleetPeers(t) // so a fleet answer would be visibly non-empty if the guard were missing

	for _, q := range []string{
		"/v1/events?router=notanip",
		"/v1/events?peer=notanip",
		"/v1/events?router=notanip&peer=notanip",
	} {
		res := get(t, s, q)
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 -- a mistyped address must not fall "+
				"through to a fleet-wide answer", q, res.Code)
			continue
		}
		if !strings.Contains(res.Body.String(), "is not an IP address") {
			t.Errorf("%s: body does not name the parse failure: %s", q, res.Body)
		}
	}
}
