// Handler tests for GET /v1/collection/{dumps,sessions,locrib,flags}. See
// api/handlers_test.go's own header for why these run against a live
// ClickHouse rather than a mocked driver, and api/events_test.go for the
// harness this file borrows (get, getOK, requireAPIWithMaxUnscopedSince).
//
// What this file does NOT re-test: the dump/change classification, the
// session-vs-event distinction, HasStat's no-stat-vs-zero split and the
// ten-table flag union are all query/collection_test.go's job, exhaustively.
// This file tests the HTTP seam on top of it -- parameter parsing, the
// window clamp, router= narrowing, and that a fleet total rides in meta
// rather than as a row in data -- the things that live in api/ and that
// query/'s own tests cannot see at all.
package api

import (
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// collFixRouterA/collFixPeerA and collFixRouterB/collFixPeerB are this
// file's own coordinates -- 10.95.0.0/16, unused elsewhere under apiTestDB
// -- so router= narrowing has two real routers to tell apart and nothing
// else in this package's shared, persistent database collides with either.
const (
	collFixRouterA = "10.95.0.1"
	collFixPeerA   = "10.95.0.2"
	collFixRouterB = "10.95.1.1"
	collFixPeerB   = "10.95.1.2"
)

// collFixFlag is a parse-flag name unique to this test BINARY's run --
// PARSE_FLAG_COLLECTION_TEST_ plus the process' own start time in base36 --
// so /v1/collection/flags' envelope count for it stays exact even though
// this package never truncates apiTestDB and a prior run's rows are still
// sitting in it. A fixed literal would accumulate one more envelope every
// time this file's tests are re-run within the same hour, exactly the
// hazard TestEventsUnscopedSaysNothingWasLeftOutWhenNothingWas's own doc
// comment describes for peer_events -- there solved with a private rib=,
// here with a private flag name because FlagCounts groups on flag alone.
var collFixFlag = "PARSE_FLAG_COLLECTION_TEST_" + strconv.FormatInt(time.Now().UnixNano(), 36)

// collFixFlag2 is a SECOND process-unique flag name, distinct from
// collFixFlag by its own suffix -- see TestCollectionEndpointsReportTotalMatched,
// which needs an unscoped /v1/collection/flags answer to contain more than
// one distinct flag from this file's OWN fixture alone, deterministically,
// regardless of whether any other test file in this package happened to run
// in the same invocation and contribute flags of its own.
var collFixFlag2 = collFixFlag + "_B"

// collFixSeq is a per-process counter so every row this file writes gets a
// distinct (ts, seq) pair, mirroring events_test.go's own eventsFixSeq.
var collFixSeq atomic.Uint64

// collFixTS returns a timestamp comfortably inside the default 1h window
// (strictly increasing across calls within one run, never colliding with a
// sibling row's own ReplacingMergeTree key) and the seq/session identity to
// pair with it.
func collFixTS() (time.Time, uint64) {
	n := collFixSeq.Add(1)
	return time.Now().UTC().Add(time.Duration(n) * time.Millisecond), n
}

// insertCollRoute writes one route_unicast row for (router, peer) under
// rib, optionally carrying flags (nil for none). session_id is set to this
// row's own seq, so every row it writes opens its OWN session -- the
// simplest population DumpCounts can classify: is_withdraw = 0 and a
// session of exactly one row is always that session's first (and only)
// observation, i.e. always a dump. This file does not need a redump/change
// split -- see this file's own header -- so a population that is
// deterministically all-dumps keeps every assertion here about SHAPE and
// NARROWING rather than about the classification query/ already owns.
func insertCollRoute(t *testing.T, router, peer, rib, prefix string, flags []string) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	if flags == nil {
		flags = []string{}
	}
	ts, seq := collFixTS()
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast: %v", err)
	}
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(router), "coll-fixture",
		netip.MustParseAddr(peer), rib, uint32(65001),
		netip.MustParseAddr(router), seq, seq,
		ts, ts, flags, seq,
		"ipv4u", prefix, uint32(0), uint8(0), uint8(0),
		[]uint32{65001}, "", nil, nil,
		[]uint32{}, []string{}, []string{}, []string{},
	); err != nil {
		t.Fatalf("append route_unicast: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send route_unicast: %v", err)
	}
}

// insertCollSession writes one peer_events row for (router, peer), always
// under its own fresh session_id -- so SessionCounts' sessions and this
// file's own row count agree, which is what lets TestCollectionRouterNarrows
// and TestCollectionTotalsRideInMetaNotInData assert against each other
// without hand-computing a session count.
func insertCollSession(t *testing.T, router, peer, kind string) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	ts, seq := collFixTS()
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(router), "coll-fixture",
		netip.MustParseAddr(peer), "in_pre", uint32(65001),
		netip.MustParseAddr(router), seq, seq,
		ts, ts, []string{}, seq,
		kind, netip.MustParseAddr(router), uint16(179), uint16(50000),
		uint32(0), uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
}

// insertCollStat writes one stats_events row for (router, peer) carrying
// BMP Stats Report counter 8 (Loc-RIB route count) at the given value.
func insertCollStat(t *testing.T, router, peer string, counter8 uint64) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	ts, seq := collFixTS()
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".stats_events")
	if err != nil {
		t.Fatalf("prepare stats_events: %v", err)
	}
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(router), "coll-fixture",
		netip.MustParseAddr(peer), "loc_rib", uint32(65001),
		netip.MustParseAddr(router), seq, seq,
		ts, ts, []string{}, seq,
		map[uint32]uint64{8: counter8},
	); err != nil {
		t.Fatalf("append stats_events: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send stats_events: %v", err)
	}
}

// collFixtureOnce guards seedCollectionFixture the same way
// collectionFixturesOnce guards query/collection_test.go's own seed: this
// package's testDB is never truncated and several tests in this file share
// one population, so seeding it twice would not fail -- it would silently
// double every hand-derivable count. Nothing here depends on an exact
// count, all of it depends on scoped-to-one-router shapes and internal
// (meta vs data) self-consistency, but the guard costs nothing and matches
// this repository's own precedent for a fixture several tests read.
var collFixtureOnce sync.Once

// seedCollectionFixture writes this file's whole population: one router
// (collFixRouterA) built so every one of the four endpoints has something
// to report for it, and a second (collFixRouterB) that exists ONLY so
// router= has something real to narrow away from -- if a handler silently
// ignored router=, router B's own rows would leak into a query scoped to
// router A and every len(rows) == 1 assertion below would fail.
//
// locRIB's shape is deliberately asymmetric: router A's peer both reports
// counter 8 AND has a loc_rib route archived; router B's peer reports
// counter 8 but never gets a loc_rib row. Neither peer exercises
// HasStat == false (a peer that never sends the stat at all) -- that
// distinction is TestLocRIBComparisonDistinguishesNoStatFromZero's job in
// query/collection_test.go; this file only needs a real, narrowable row on
// each side of router=.
func seedCollectionFixture(t *testing.T) {
	t.Helper()
	collFixtureOnce.Do(func() {
		insertCollRoute(t, collFixRouterA, collFixPeerA, "in_pre", "10.95.10.0/24", nil)
		insertCollRoute(t, collFixRouterB, collFixPeerB, "in_pre", "10.95.11.0/24", nil)

		insertCollSession(t, collFixRouterA, collFixPeerA, "up")
		insertCollSession(t, collFixRouterB, collFixPeerB, "up")

		insertCollStat(t, collFixRouterA, collFixPeerA, 5)
		insertCollRoute(t, collFixRouterA, collFixPeerA, "loc_rib", "10.95.20.0/24", nil)
		insertCollStat(t, collFixRouterB, collFixPeerB, 7)

		insertCollRoute(t, collFixRouterA, collFixPeerA, "in_pre", "10.95.30.0/24", []string{collFixFlag})
		insertCollRoute(t, collFixRouterB, collFixPeerB, "in_pre", "10.95.31.0/24", []string{collFixFlag})

		// A second, distinct flag -- see collFixFlag2's own doc comment for
		// why TestCollectionEndpointsReportTotalMatched needs this file's
		// own fixture to carry more than one flag by itself. Written under
		// router B, not router A: TestCollectionTotalsRideInMetaNotInData's
		// own flags subtest depends on router A carrying EXACTLY one flag
		// (collFixFlag), and this must not disturb that.
		insertCollRoute(t, collFixRouterB, collFixPeerB, "in_pre", "10.95.32.0/24", []string{collFixFlag2})
	})
}

// collectionPaths is the four paths every table-driven test in this file
// walks, in api/openapi.yaml's own order.
var collectionPaths = []string{
	"/v1/collection/dumps",
	"/v1/collection/sessions",
	"/v1/collection/locrib",
	"/v1/collection/flags",
}

// TestCollectionEndpointsClampTheWindow. Every one of these is an unscoped
// aggregate over a table whose sort key does not lead with ts_collector --
// the same hazard /v1/events' unscoped mode is clamped for, so it reuses the
// same configured ceiling (checkUnscopedWindow) rather than introducing a
// second one.
//
// Table-driven over all four paths, each its own subtest, is what makes
// mutation 1 (drop the checkUnscopedWindow call from ONE handler) fail only
// that path's subtest rather than the whole function -- a single assertion
// walking all four in one t.Run would still fail overall, but would not by
// itself say which of the four lost its clamp.
//
// The body assertions check that the 400 does not offer /v1/events' own
// remediation, which is false for these four paths. checkUnscopedWindow's
// message used to end with /v1/events' own remediation -- "Narrow the
// window, or name router= and peer=" -- on all five paths that call it. On
// these four that advice is false twice over: peer= is not a parameter
// collectionFilter parses at all, and router= grants no exemption from the
// clamp (the subtest below proves that directly, with router= set). An
// operator who followed it would get the identical 400 and conclude the
// daemon was broken. So the 400 must name the limit, and must NOT advise a
// scope.
func TestCollectionEndpointsClampTheWindow(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	for _, path := range collectionPaths {
		t.Run(path, func(t *testing.T) {
			if res := get(t, s, path); res.Code != http.StatusOK {
				t.Fatalf("absent since=: status = %d, want 200 (defaults to 1h); body %s",
					res.Code, res.Body)
			}
			if res := get(t, s, path+"?since=24h"); res.Code != http.StatusOK {
				t.Fatalf("since=24h, exactly the configured maximum: status = %d, want 200 "+
					"-- the boundary is inclusive; body %s", res.Code, res.Body)
			}
			res := get(t, s, path+"?since=720h")
			if res.Code != http.StatusBadRequest {
				t.Fatalf("since=720h: status = %d, want 400", res.Code)
			}
			body := res.Body.String()
			if !strings.Contains(body, "24h") {
				t.Errorf("the 400 does not name the configured limit an operator would "+
					"have to raise: %s", body)
			}
			// The remediation has to be one that works on THIS path.
			// peer= does not exist here, so advising it sends an operator
			// to an identical 400.
			if strings.Contains(body, "peer=") {
				t.Errorf("the 400 advises peer=, which is not a parameter any "+
					"/v1/collection/* path accepts -- an operator who follows it gets "+
					"this same 400 back: %s", body)
			}
			// router= is a real parameter here, but naming it exempts
			// nothing, so the body must not offer it as a way out either.
			// (Naming it as something that does NOT help is fine, and is
			// what unscopedCollectionHint does.)
			if strings.Contains(body, "or name router=") {
				t.Errorf("the 400 offers router= as a way past the clamp, which it is "+
					"not -- the subtest below proves router= is still a 400: %s", body)
			}
			// The way out that does exist, named so an operator has one.
			if !strings.Contains(body, "max_unscoped_since") {
				t.Errorf("the 400 does not name the config key an operator would have "+
					"to raise: %s", body)
			}

			// router= narrows the WHERE on all four statements but pins no
			// session the way /v1/events' scoped mode does -- see
			// collectionFilter's own doc comment -- so the clamp applies
			// whether or not router= is set.
			res = get(t, s, path+"?router="+collFixRouterA+"&since=720h")
			if res.Code != http.StatusBadRequest {
				t.Errorf("router=%s&since=720h: status = %d, want 400 -- router= must not "+
					"exempt this endpoint's window from the clamp", collFixRouterA, res.Code)
			}
		})
	}
}

// TestCollectionRouterNarrows proves router= actually reaches query/'s own
// WHERE rather than being silently ignored: router B's rows, written by the
// same fixture, must never appear in a response scoped to router A.
func TestCollectionRouterNarrows(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedCollectionFixture(t)

	t.Run("dumps", func(t *testing.T) {
		var rows []WireRouterDumpCount
		getOK(t, s, "/v1/collection/dumps?router="+collFixRouterA, &rows)
		if len(rows) == 0 {
			t.Fatal("router= scoped to a router this fixture wrote returned no rows")
		}
		for _, r := range rows {
			if r.RouterIP != collFixRouterA {
				t.Errorf("router=%s returned a row for %s", collFixRouterA, r.RouterIP)
			}
		}
	})
	t.Run("sessions", func(t *testing.T) {
		var rows []WireRouterSessionCount
		getOK(t, s, "/v1/collection/sessions?router="+collFixRouterA, &rows)
		if len(rows) == 0 {
			t.Fatal("router= scoped to a router this fixture wrote returned no rows")
		}
		for _, r := range rows {
			if r.RouterIP != collFixRouterA {
				t.Errorf("router=%s returned a row for %s", collFixRouterA, r.RouterIP)
			}
		}
	})
	t.Run("locrib", func(t *testing.T) {
		var rows []WirePeerLocRIB
		getOK(t, s, "/v1/collection/locrib?router="+collFixRouterA, &rows)
		if len(rows) == 0 {
			t.Fatal("router= scoped to a router this fixture wrote returned no rows")
		}
		for _, r := range rows {
			if r.RouterIP != collFixRouterA {
				t.Errorf("router=%s returned a row for %s", collFixRouterA, r.RouterIP)
			}
		}
	})
	t.Run("flags", func(t *testing.T) {
		// FlagCount carries no router_ip -- see WireFlagCount's own doc
		// comment -- so narrowing is checked on the COUNT for this file's
		// own private flag, not on an excluded identity column the wire
		// type does not have.
		var unscoped, scoped []WireFlagCount
		getOK(t, s, "/v1/collection/flags", &unscoped)
		getOK(t, s, "/v1/collection/flags?router="+collFixRouterA, &scoped)
		scopedEnv := envelopesFor(scoped, collFixFlag)
		unscopedEnv := envelopesFor(unscoped, collFixFlag)
		if scopedEnv == 0 {
			t.Fatal("router= narrowed away the envelope this fixture wrote for it")
		}
		if scopedEnv >= unscopedEnv {
			t.Errorf("router=%s envelopes (%d) must be fewer than unscoped (%d): "+
				"router B's own envelope under the same flag must be excluded when "+
				"router= narrows", collFixRouterA, scopedEnv, unscopedEnv)
		}
	})
}

func envelopesFor(rows []WireFlagCount, flag string) uint64 {
	for _, r := range rows {
		if r.Flag == flag {
			return r.Envelopes
		}
	}
	return 0
}

// TestCollectionTotalsRideInMetaNotInData. A fleet total rendered as a row
// is a row a client has to know to exclude, and one that will eventually be
// sorted, filtered or counted as a member. Every subtest below scopes to
// ONE router: DumpCounts, SessionCounts and LocRIBComparison all group on
// router_ip (LocRIBComparison also on peer_ip), so a router-scoped answer
// is exactly one row -- a fleet total tacked on as a second row would be
// caught by the len(rows) != 1 check alone, and mutation 2 (move the total
// into data) is what that check exists for.
func TestCollectionTotalsRideInMetaNotInData(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedCollectionFixture(t)

	t.Run("dumps", func(t *testing.T) {
		var rows []WireRouterDumpCount
		meta := getOK(t, s, "/v1/collection/dumps?router="+collFixRouterA, &rows)
		if meta.DumpTotals == nil {
			t.Fatal("meta.dump_totals is nil -- the fleet total never reached meta")
		}
		if len(rows) != 1 {
			t.Fatalf("data has %d rows scoped to one router, want exactly 1 -- a fleet "+
				"total must not ride along as a synthesized row", len(rows))
		}
		row := rows[0]
		if meta.DumpTotals.Archived != row.Archived ||
			meta.DumpTotals.Dumps != row.Dumps ||
			meta.DumpTotals.Changes != row.Changes {
			t.Errorf("dump_totals = %+v, want it to equal the one returned row %+v",
				meta.DumpTotals, row)
		}
	})
	t.Run("sessions", func(t *testing.T) {
		var rows []WireRouterSessionCount
		meta := getOK(t, s, "/v1/collection/sessions?router="+collFixRouterA, &rows)
		if meta.SessionTotals == nil {
			t.Fatal("meta.session_totals is nil -- the fleet total never reached meta")
		}
		if len(rows) != 1 {
			t.Fatalf("data has %d rows scoped to one router, want exactly 1 -- a fleet "+
				"total must not ride along as a synthesized row", len(rows))
		}
		row := rows[0]
		if meta.SessionTotals.Sessions != row.Sessions ||
			meta.SessionTotals.Up != row.Up ||
			meta.SessionTotals.Down != row.Down ||
			meta.SessionTotals.ViewLost != row.ViewLost {
			t.Errorf("session_totals = %+v, want it to equal the one returned row %+v",
				meta.SessionTotals, row)
		}
	})
	t.Run("locrib", func(t *testing.T) {
		var rows []WirePeerLocRIB
		meta := getOK(t, s, "/v1/collection/locrib?router="+collFixRouterA, &rows)
		if meta.LocRIBTotals == nil {
			t.Fatal("meta.locrib_totals is nil -- the fleet total never reached meta")
		}
		if len(rows) != 1 {
			t.Fatalf("data has %d rows scoped to one router, want exactly 1 -- a fleet "+
				"total must not ride along as a synthesized row", len(rows))
		}
		row := rows[0]
		if meta.LocRIBTotals.Reported != row.Reported || meta.LocRIBTotals.Archived != row.Archived {
			t.Errorf("locrib_totals = %+v, want it to equal the one returned row %+v",
				meta.LocRIBTotals, row)
		}
	})
	t.Run("flags", func(t *testing.T) {
		var rows []WireFlagCount
		meta := getOK(t, s, "/v1/collection/flags?router="+collFixRouterA, &rows)
		if meta.FlagTotals == nil {
			t.Fatal("meta.flag_totals is nil -- the fleet total never reached meta")
		}
		// Unlike the other three, FlagCount is not grouped by router, so
		// router= narrows the WHERE without collapsing to one row per
		// router -- this file's own fixture happens to write exactly one
		// flag under collFixRouterA, which is what makes len == 1 a real
		// assertion here rather than a coincidence of the query shape.
		if len(rows) != 1 {
			t.Fatalf("data has %d rows for a router this fixture gave exactly one flag, "+
				"want exactly 1 -- a fleet total must not ride along as a synthesized row",
				len(rows))
		}
		row := rows[0]
		if row.Flag != collFixFlag {
			t.Errorf("flag = %q, want %q", row.Flag, collFixFlag)
		}
		if meta.FlagTotals.Envelopes != row.Envelopes {
			t.Errorf("flag_totals.envelopes = %d, want %d (the one returned row's own value)",
				meta.FlagTotals.Envelopes, row.Envelopes)
		}
	})
}

// TestCollectionEndpointsHaveDistinctShapes guards against two handlers
// answering with the same underlying query -- concretely, the routing
// table (api/server.go) wiring two of the four paths to the same handler,
// which compiles cleanly (every handler here is an ordinary
// http.HandlerFunc) and would not be caught by any type mismatch the
// compiler could refuse. Each of the four row shapes carries fields none of
// its three siblings do, so a swapped handler answers with the WRONG set of
// keys rather than merely the wrong numbers.
func TestCollectionEndpointsHaveDistinctShapes(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedCollectionFixture(t)

	cases := []struct {
		path      string
		wantKeys  []string
		otherKeys []string
	}{
		{"/v1/collection/dumps?router=" + collFixRouterA,
			[]string{"router_ip", "router_sysname", "archived", "dumps", "changes"},
			[]string{"sessions", "up", "down", "view_lost", "reported", "has_stat", "flag", "envelopes"}},
		{"/v1/collection/sessions?router=" + collFixRouterA,
			[]string{"router_ip", "router_sysname", "sessions", "up", "down", "view_lost"},
			[]string{"archived", "dumps", "changes", "reported", "has_stat", "flag", "envelopes"}},
		{"/v1/collection/locrib?router=" + collFixRouterA,
			[]string{"router_ip", "peer_ip", "reported", "archived", "has_stat"},
			[]string{"dumps", "changes", "sessions", "up", "down", "view_lost", "flag", "envelopes", "router_sysname"}},
		{"/v1/collection/flags?router=" + collFixRouterA,
			[]string{"flag", "envelopes"},
			[]string{"router_ip", "router_sysname", "peer_ip", "archived", "dumps", "changes",
				"sessions", "up", "down", "view_lost", "reported", "has_stat"}},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			var rows []map[string]any
			getOK(t, s, c.path, &rows)
			if len(rows) == 0 {
				t.Fatal("no rows returned; the fixture should have produced at least one")
			}
			row := rows[0]
			for _, k := range c.wantKeys {
				if _, ok := row[k]; !ok {
					t.Errorf("row is missing %q, this endpoint's own field: %+v", k, row)
				}
			}
			for _, k := range c.otherKeys {
				if _, ok := row[k]; ok {
					t.Errorf("row carries %q, a field that belongs to a DIFFERENT "+
						"/v1/collection/* endpoint -- two handlers may be answering "+
						"from the same query: %+v", k, row)
				}
			}
		})
	}
}

// TestCollectionEndpointsReportTotalMatched pins a defect: all four
// endpoints declared limit= and applied a real LIMIT in
// query/collection.go, yet left meta.total_matched
// permanently null -- exactly the "a partial answer reads as complete"
// failure this project audits hardest for. query/collection_test.go's own
// TestCollectionQueriesReportTotalMatchedBeforeLimit and
// TestCollectionQueriesTotalMatchedIsZeroWhenEmpty pin the underlying
// count() OVER () column at the query layer; this is the HTTP seam on top
// of it, checked two ways one column value alone cannot prove: that
// total_matched actually reaches the JSON response, and that it EQUALS
// len(data) -- not merely non-nil -- on an answer nothing capped, with a
// truncated warning appearing only once something genuinely did.
func TestCollectionEndpointsReportTotalMatched(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedCollectionFixture(t)

	t.Run("dumps not truncated", func(t *testing.T) {
		var rows []WireRouterDumpCount
		meta := getOK(t, s, "/v1/collection/dumps?router="+collFixRouterA, &rows)
		if meta.TotalMatched == nil {
			t.Fatal("meta.total_matched is nil on a capped endpoint -- the fix this test " +
				"exists for is not wired in")
		}
		if *meta.TotalMatched != uint64(len(rows)) {
			t.Errorf("total_matched = %d, want %d (nothing should be truncated at this "+
				"scope)", *meta.TotalMatched, len(rows))
		}
		if hasWarning(meta.Warnings, WarnTruncated) {
			t.Error("a truncated warning fired on an answer nothing capped")
		}
	})

	// The four cases below force a real truncation with limit=1 against an
	// UNSCOPED request: this file's own fixture alone guarantees at least
	// two distinct routers (dumps, sessions), two distinct (router, peer)
	// pairs (locrib) and two distinct flags (flags, via collFixFlag and
	// collFixFlag2 -- see the latter's own doc comment), regardless of
	// whether any other test file in this package also ran and contributed
	// rows of its own; more rows only makes total_matched larger, never
	// smaller, so the ">" assertions below hold either way.
	t.Run("dumps truncated", func(t *testing.T) {
		var rows []WireRouterDumpCount
		meta := getOK(t, s, "/v1/collection/dumps?limit=1", &rows)
		requireTruncated(t, meta, len(rows))
	})
	t.Run("sessions truncated", func(t *testing.T) {
		var rows []WireRouterSessionCount
		meta := getOK(t, s, "/v1/collection/sessions?limit=1", &rows)
		requireTruncated(t, meta, len(rows))
	})
	t.Run("locrib truncated", func(t *testing.T) {
		var rows []WirePeerLocRIB
		meta := getOK(t, s, "/v1/collection/locrib?limit=1", &rows)
		requireTruncated(t, meta, len(rows))
	})
	t.Run("flags truncated", func(t *testing.T) {
		var rows []WireFlagCount
		meta := getOK(t, s, "/v1/collection/flags?limit=1", &rows)
		requireTruncated(t, meta, len(rows))
	})
}

// requireTruncated asserts the shape every genuinely-capped
// /v1/collection/* answer must carry: total_matched present and strictly
// greater than what was returned, and the truncated warning naming both
// numbers.
func requireTruncated(t *testing.T, meta Meta, returned int) {
	t.Helper()
	if meta.TotalMatched == nil {
		t.Fatal("meta.total_matched is nil on a capped, truncated answer")
	}
	if *meta.TotalMatched <= uint64(returned) {
		t.Fatalf("total_matched = %d, returned = %d -- want total_matched strictly "+
			"greater (limit=1 should have truncated a population this file's own "+
			"fixture alone makes larger than 1)", *meta.TotalMatched, returned)
	}
	if !hasWarning(meta.Warnings, WarnTruncated) {
		t.Errorf("total_matched (%d) exceeds what was returned (%d) but meta.warnings "+
			"carries no truncated warning", *meta.TotalMatched, returned)
	}
}
