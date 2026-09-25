package query

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
)

// mustHistory calls RouteHistory and fails the test on error, so each subtest
// below reads as a claim about the timeline rather than about error handling.
func mustHistory(t *testing.T, ctx context.Context, q *Q, f HistoryFilter) []HistoryEvent {
	t.Helper()
	got, err := q.RouteHistory(ctx, f)
	if err != nil {
		t.Fatalf("RouteHistory(%+v): %v", f, err)
	}
	return got
}

// actions renders a timeline's actions in order, for an assertion that reads
// as the sequence it is checking rather than as four index expressions.
func actions(evs []HistoryEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Action)
	}
	return out
}

// seqs and pathIDs do the same for the two fields the ordering subtests use to
// tell otherwise-identical events apart.
func seqs(evs []HistoryEvent) []uint64 {
	out := make([]uint64, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

func pathIDs(evs []HistoryEvent) []uint32 {
	out := make([]uint32, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.PathID)
	}
	return out
}

func TestRouteHistory(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertHistoryFixture(t, ctx, q)

	t.Run("announce, withdraw, re-announce is three events, not one row", func(t *testing.T) {
		// The headline claim of this whole surface. Every other query in
		// this package resolves these three rows to the one thing that is
		// true now -- Routes' own argMax over (seq, stream_seq) plus
		// HAVING live_is_withdraw = 0 -- and that answer is exactly wrong
		// here: a timeline with the withdrawal removed is not a shorter
		// timeline, it is a false one.
		got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyFixtureFlapPrefix})
		if len(got) != 3 {
			t.Fatalf("got %d events, want 3 -- history keeps the withdrawal and "+
				"the superseded announcement: %+v", len(got), got)
		}
		if want := []string{actionAnnounce, actionWithdraw, actionAnnounce}; !slices.Equal(actions(got), want) {
			t.Errorf("actions = %v, want %v -- newest first, with the withdrawal "+
				"in the middle", actions(got), want)
		}
		// The action spellings are asserted against literals rather than
		// against actionAnnounce/actionWithdraw as well, because the consts
		// and the contract are two things that have to agree and a test
		// written only against the consts cannot tell them apart.
		if got[1].Action != "withdraw" {
			t.Errorf("the middle event's Action = %q, want %q -- "+
				"api/openapi.yaml's HistoryEvent.action enum is [announce, withdraw]",
				got[1].Action, "withdraw")
		}

		// The seq sequence, not just the action sequence: the fixture's
		// ts_router values run in the OPPOSITE order to its ts_collector
		// values (the newest event carries the Unix epoch, as a router with
		// a dead clock really reports it), so a query ordered by ts_router
		// returns these same three events, with the same
		// announce/withdraw/announce shape, in exactly the reverse order.
		// Only the seq sequence tells the two apart.
		if want := []uint64{3, 2, 1}; !slices.Equal(seqs(got), want) {
			t.Errorf("seqs = %v, want %v -- newest first by ts_collector; "+
				"ordering by ts_router would give %v", seqs(got), want, []uint64{1, 2, 3})
		}
		if !got[0].TsRouter.Before(got[2].TsRouter) {
			t.Errorf("the fixture is not adversarial: the newest event's TsRouter "+
				"(%s) must be BEFORE the oldest event's (%s) for this subtest to "+
				"distinguish ts_collector ordering from ts_router ordering",
				got[0].TsRouter, got[2].TsRouter)
		}

		// Routes, asked the same question, answers 1: the surfaces really do
		// disagree, and this subtest is not merely counting rows nothing
		// else would have collapsed.
		live := mustRoutes(t, ctx, q, RouteFilter{Prefix: historyFixtureFlapPrefix})
		if len(live) != 1 {
			t.Fatalf("Routes returned %d rows for the same prefix, want 1 -- the "+
				"contrast with history's 3 is the point of this surface: %+v", len(live), live)
		}
	})

	t.Run("a withdrawal carries no next hop and no AS path", func(t *testing.T) {
		got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyFixtureFlapPrefix})
		if len(got) != 3 {
			t.Fatalf("got %d events, want 3", len(got))
		}
		w := got[1]
		if w.NextHop.IsValid() {
			t.Errorf("the withdrawal's NextHop = %s, want the zero Addr -- a BGP "+
				"withdrawal names an NLRI and carries no path attributes, and "+
				"api/openapi.yaml types next_hop as nullable for that reason", w.NextHop)
		}
		if len(w.ASPath) != 0 {
			t.Errorf("the withdrawal's ASPath = %v, want empty", w.ASPath)
		}
		// And the announcements do carry one, so the assertion above is not
		// passing because the scan drops next hops generally.
		if !got[0].NextHop.IsValid() || !got[2].NextHop.IsValid() {
			t.Errorf("an announcement came back with no next hop (%s, %s); the "+
				"withdrawal's empty one then proves nothing",
				got[0].NextHop, got[2].NextHop)
		}
	})

	t.Run("every field the contract requires is populated", func(t *testing.T) {
		got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyFixtureFlapPrefix})
		if len(got) == 0 {
			t.Fatal("no events")
		}
		e := got[0]
		if e.Collector != defaultFixtureCollector {
			t.Errorf("Collector = %q, want %q", e.Collector, defaultFixtureCollector)
		}
		if e.RouterIP.String() != historyFixtureRouterIP {
			t.Errorf("RouterIP = %s, want %s -- and in plain form, never "+
				"IPv4-mapped (see unmapAll)", e.RouterIP, historyFixtureRouterIP)
		}
		if e.PeerIP.String() != historyFixtureUpPeer {
			t.Errorf("PeerIP = %s, want %s", e.PeerIP, historyFixtureUpPeer)
		}
		if e.SessionID != historyFixtureCurSession {
			t.Errorf("SessionID = %d, want %d", e.SessionID, historyFixtureCurSession)
		}
		if e.RIB != "in_pre" {
			t.Errorf("RIB = %q, want %q", e.RIB, "in_pre")
		}
		// Looked up rather than spelled, for the reason historyFixtureFamily
		// gives: a literal here would be a copy of the registry sink writes
		// from.
		if want := subjects.FamilyToken(bgp.FamilyIPv4U); e.Family != want {
			t.Errorf("Family = %q, want %q", e.Family, want)
		}
		if e.Prefix != historyFixtureFlapPrefix {
			t.Errorf("Prefix = %q, want %q", e.Prefix, historyFixtureFlapPrefix)
		}
		if e.PathID != 1 {
			t.Errorf("PathID = %d, want 1", e.PathID)
		}
		if e.TsCollector.IsZero() || e.TsRouter.IsZero() {
			t.Errorf("TsCollector = %s TsRouter = %s; both are required by the contract",
				e.TsCollector, e.TsRouter)
		}
	})

	t.Run("events from a superseded session are included", func(t *testing.T) {
		// This prefix was advertised only in the session the router has
		// since replaced, by a peer that is up right now -- the exact shape
		// insertRouteFixture's 10.3.0.0/24 uses to prove Routes discards it.
		// History's job is the opposite: the announcement happened, and the
		// session having ended since does not unmake it.
		got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyFixtureOldPrefix})
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1 -- an event from a superseded session "+
				"is history, not noise: %+v", len(got), got)
		}
		if got[0].SessionID != historyFixtureOldSession {
			t.Errorf("SessionID = %d, want %d", got[0].SessionID, historyFixtureOldSession)
		}
		// The other half of the claim, without which the count above could
		// be explained by the session never having been superseded at all.
		if live := mustRoutes(t, ctx, q, RouteFilter{Prefix: historyFixtureOldPrefix}); len(live) != 0 {
			t.Fatalf("Routes returned %d rows for the same prefix, want 0 -- if "+
				"Routes reports it too then the session is not actually "+
				"superseded and this subtest proves nothing: %+v", len(live), live)
		}
	})

	t.Run("a down peer's events are included", func(t *testing.T) {
		got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyFixtureDownPeerPrefix})
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1 -- a peer that has since gone down "+
				"still announced what it announced: %+v", len(got), got)
		}
		if got[0].PeerIP.String() != historyFixtureDownPeer {
			t.Errorf("PeerIP = %s, want %s", got[0].PeerIP, historyFixtureDownPeer)
		}
		if live := mustRoutes(t, ctx, q, RouteFilter{Prefix: historyFixtureDownPeerPrefix}); len(live) != 0 {
			t.Fatalf("Routes returned %d rows for the same prefix, want 0 -- if "+
				"Routes reports it too then the peer is not actually down and "+
				"this subtest proves nothing: %+v", len(live), live)
		}
	})

	t.Run("a router peer_events never recorded still has a history", func(t *testing.T) {
		// historyFixtureRouterB has no peer_events rows at all, so every
		// session-scoping and peer-state join in this package finds nothing
		// to match it against. RouteHistory carries none of them.
		got := mustHistory(t, ctx, q, HistoryFilter{
			Prefix: historyFixtureFilterPrefix,
			Router: netip.MustParseAddr(historyFixtureRouterB),
		})
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1: %+v", len(got), got)
		}
		if live := mustRoutes(t, ctx, q, RouteFilter{
			Prefix: historyFixtureFilterPrefix,
			Router: netip.MustParseAddr(historyFixtureRouterB),
		}); len(live) != 0 {
			t.Fatalf("Routes returned %d rows, want 0 -- a router with no "+
				"peer_events has no current session to scope to: %+v", len(live), live)
		}
	})

	t.Run("ties are broken by seq, then by stream_seq", func(t *testing.T) {
		// Four events at one ts_collector. path_id is the only field that
		// distinguishes them in the result.
		//
		//   path_id 1: seq 3, stream_seq 110
		//   path_id 2: seq 2, stream_seq 120
		//   path_id 3: seq 1, stream_seq 130
		//   path_id 4: seq 1, stream_seq 140
		//
		// seq descends as stream_seq ascends across the first three, so an
		// ORDER BY that dropped seq would report [4, 3, 2, 1].
		got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyFixtureTiePrefix})
		if len(got) != 4 {
			t.Fatalf("got %d events, want 4: %+v", len(got), got)
		}
		if want := []uint32{1, 2, 4, 3}; !slices.Equal(pathIDs(got), want) {
			t.Errorf("path_ids = %v, want %v -- seq descending first, then "+
				"stream_seq for the two events that share a seq",
				pathIDs(got), want)
		}
		for i := 1; i < len(got); i++ {
			if !got[i].TsCollector.Equal(got[0].TsCollector) {
				t.Fatalf("the fixture is not adversarial: event %d's TsCollector "+
					"(%s) differs from event 0's (%s), so ts_collector alone "+
					"could have produced this order",
					i, got[i].TsCollector, got[0].TsCollector)
			}
		}
	})
}

// TestRouteHistorySince is api/openapi.yaml's ?since= on
// /v1/routes/history, asked of the real query.
//
// The boundary cases are the point rather than the middle one: `>=` and `>`
// differ on exactly one row, and it is the row a caller polling for "anything
// since the last event I saw" hands straight back.
func TestRouteHistorySince(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	base := insertHistoryFixture(t, ctx, q)

	// The flap prefix's three events sit at base-3h, base-2h and base-1h.
	for _, tc := range []struct {
		name  string
		since time.Time
		want  int
	}{
		{"the zero time means no bound at all", time.Time{}, 3},
		{"before every event", base.Add(-4 * time.Hour), 3},
		{"between the first and second", base.Add(-150 * time.Minute), 2},
		{"between the second and third", base.Add(-90 * time.Minute), 1},
		{"after every event", base.Add(time.Hour), 0},
		// The inclusive boundary, in both directions: exactly the second
		// event's own timestamp keeps it, one microsecond later drops it.
		{"exactly the second event's timestamp", base.Add(-2 * time.Hour), 2},
		{"one microsecond after it", base.Add(-2*time.Hour + time.Microsecond), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustHistory(t, ctx, q, HistoryFilter{
				Prefix: historyFixtureFlapPrefix,
				Since:  tc.since,
			})
			if len(got) != tc.want {
				t.Fatalf("got %d events, want %d: %+v", len(got), tc.want, got)
			}
			for _, e := range got {
				if !tc.since.IsZero() && e.TsCollector.Before(tc.since) {
					t.Errorf("event at %s is before the since bound %s",
						e.TsCollector, tc.since)
				}
			}
		})
	}
}

// TestRouteHistorySinceIsAnAbsoluteInstant pins what a UTC-only test cannot
// distinguish: that the since bound is bound as a time.Time and compared as an
// absolute instant, rather than formatted into wall-clock text that the server
// would then read in its own zone.
//
// Every fixture in this package writes UTC timestamps, so a version of
// filters.since that dropped the location -- or that formatted the value
// itself -- would return the identical answer for every other test here. The
// same instant expressed in a zone five hours ahead is what tells them apart:
// read as wall-clock time it is five hours later and would drop two of the
// three events.
func TestRouteHistorySinceIsAnAbsoluteInstant(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	base := insertHistoryFixture(t, ctx, q)

	utc := base.Add(-150 * time.Minute)
	shifted := utc.In(time.FixedZone("test+5", 5*60*60))
	if !utc.Equal(shifted) {
		t.Fatalf("the two bounds are not the same instant: %s vs %s", utc, shifted)
	}

	for _, tc := range []struct {
		name  string
		since time.Time
	}{
		{"UTC", utc},
		{"the same instant five hours ahead", shifted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustHistory(t, ctx, q, HistoryFilter{
				Prefix: historyFixtureFlapPrefix,
				Since:  tc.since,
			})
			if len(got) != 2 {
				t.Fatalf("got %d events, want 2 -- the bound is an instant, not "+
					"a wall-clock reading: %+v", len(got), got)
			}
		})
	}
}

// TestRouteHistoryFiltersByRouterPeerAndRib is the behavior half of the three
// optional narrowings api/openapi.yaml documents on /v1/routes/history. It
// catches what a rendered-SQL test cannot: a predicate emitted against the
// wrong column -- r.router_ip where r.peer_ip was meant -- which renders
// perfectly and answers wrongly.
func TestRouteHistoryFiltersByRouterPeerAndRib(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertHistoryFixture(t, ctx, q)

	var (
		routerA = netip.MustParseAddr(historyFixtureRouterIP)
		routerB = netip.MustParseAddr(historyFixtureRouterB)
		peerA   = netip.MustParseAddr(historyFixtureUpPeer)
		peerB   = netip.MustParseAddr(historyFixturePeerB)
	)

	for _, tc := range []struct {
		name string
		f    HistoryFilter
		want int
	}{
		// The unfiltered baseline first: without it, every count below
		// could be explained by the fixture having written fewer rows than
		// it meant to rather than by the filter having selected them.
		{"prefix alone", HistoryFilter{Prefix: historyFixtureFilterPrefix}, 3},
		{"router", HistoryFilter{Prefix: historyFixtureFilterPrefix, Router: routerA}, 2},
		{"the other router", HistoryFilter{Prefix: historyFixtureFilterPrefix, Router: routerB}, 1},
		{"peer", HistoryFilter{Prefix: historyFixtureFilterPrefix, Peer: peerA}, 2},
		{"the other peer", HistoryFilter{Prefix: historyFixtureFilterPrefix, Peer: peerB}, 1},
		{"rib", HistoryFilter{Prefix: historyFixtureFilterPrefix, RIB: "in_pre"}, 2},
		{"the other rib", HistoryFilter{Prefix: historyFixtureFilterPrefix, RIB: "loc_rib"}, 1},
		{"router and rib together", HistoryFilter{
			Prefix: historyFixtureFilterPrefix, Router: routerA, RIB: "loc_rib",
		}, 1},
		// A router and a rib that exist separately but never together.
		{"router and rib that never co-occur", HistoryFilter{
			Prefix: historyFixtureFilterPrefix, Router: routerB, RIB: "loc_rib",
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustHistory(t, ctx, q, tc.f)
			if len(got) != tc.want {
				t.Fatalf("got %d events, want %d: %+v", len(got), tc.want, got)
			}
			// The count alone would pass for a filter that selected the
			// right NUMBER of wrong rows. Every row is checked against
			// every dimension the filter actually named.
			for _, e := range got {
				if tc.f.Router.IsValid() && e.RouterIP != tc.f.Router {
					t.Errorf("router %s in the result of a query filtered to %s", e.RouterIP, tc.f.Router)
				}
				if tc.f.Peer.IsValid() && e.PeerIP != tc.f.Peer {
					t.Errorf("peer %s in the result of a query filtered to %s", e.PeerIP, tc.f.Peer)
				}
				if tc.f.RIB != "" && e.RIB != tc.f.RIB {
					t.Errorf("rib %q in the result of a query filtered to %q", e.RIB, tc.f.RIB)
				}
				if e.Prefix != tc.f.Prefix {
					t.Errorf("prefix %q in the result of a query filtered to %q", e.Prefix, tc.f.Prefix)
				}
			}
		})
	}
}

// TestRouteHistoryRequiresAPrefix pins HistoryFilter.check: an unbounded event
// dump is not a question this surface answers, and the refusal wraps
// ErrBadFilter -- the package's existing sentinel -- so cmd/vantage-api can
// answer 400 rather than 500 without matching on message text.
func TestRouteHistoryRequiresAPrefix(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	got, err := q.RouteHistory(ctx, HistoryFilter{})
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("RouteHistory(HistoryFilter{}) error = %v, want one wrapping "+
			"ErrBadFilter -- an empty Prefix must be refused, not read as "+
			"\"every prefix\"", err)
	}
	if got != nil {
		t.Errorf("RouteHistory returned %d events alongside the error", len(got))
	}
	// Setting only the optional narrowings does not rescue it: none of them
	// bounds the answer to something one unpaginated response can carry.
	if _, err := q.RouteHistory(ctx, HistoryFilter{
		Router: netip.MustParseAddr(historyFixtureRouterIP),
		RIB:    "in_pre",
		Since:  time.Now(),
	}); !errors.Is(err, ErrBadFilter) {
		t.Errorf("RouteHistory accepted a filter with no Prefix but a Router, "+
			"RIB and Since: error = %v, want one wrapping ErrBadFilter", err)
	}
}

// TestRouteHistoryRejectsARibThatIsNotAnEnumMember is the history counterpart
// to TestRoutesRejectsARibThatIsNotAnEnumMember, and it exists for the same
// reason: rib is an Enum8, so ClickHouse raises on a member the contract does
// not define, and an error is a far better answer than the empty result a
// caller cannot tell from "this prefix was never seen under that rib".
func TestRouteHistoryRejectsARibThatIsNotAnEnumMember(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	if _, err := q.RouteHistory(ctx, HistoryFilter{
		Prefix: historyFixtureFlapPrefix, RIB: "in_pre_but_misspelled",
	}); err == nil {
		t.Error("RouteHistory accepted a rib that is not one of the enum's five " +
			"members; an unknown rib must fail loudly rather than match nothing")
	}
}

// TestRouteHistoryReportsARedeliveredEventOnce is the collection-artifact
// guard, and it is the one place this surface deduplicates anything at all.
//
// route_unicast is a ReplacingMergeTree whose sort key carries stream_seq, the
// JetStream sequence of the message that delivered the event, so an
// at-least-once redelivery leaves TWO byte-identical rows in the table until
// the next merge collapses them (schema.sql says so at the column;
// sink.RowsFor says so at streamSeq). Reported raw, that redelivery reads as
// the router having announced the same prefix twice in the same microsecond
// -- a fact about the transport dressed up as a fact about the network, which
// is this project's most recurring defect.
//
// historySQL's DISTINCT is what closes it, and it is NOT the current-state
// deduplication the rest of this package does: its key is every column the
// query reads, stream_seq included, so it collapses only rows the storage
// engine will itself collapse at the next merge, and never an announcement
// into the withdrawal that followed it. See historySQL.
//
// The raw != distinct precondition is the point of the test rather than
// ceremony around it: a merge winning the race between insertion and query
// turns the assertion into 1 == 1, which passes whether or not RouteHistory
// is correct. That is exactly how a headline measurement for this hazard
// stopped meaning anything -- see
// TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent.
func TestRouteHistoryReportsARedeliveredEventOnce(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	if err := q.conn.Exec(ctx, fmt.Sprintf("SYSTEM STOP MERGES %s.route_unicast", q.db)); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	// context.WithoutCancel for the reason
	// TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent gives: t's own
	// context is already done by the time Cleanup runs, and a cleanup that
	// silently no-ops would leave merges off for every test that follows.
	t.Cleanup(func() {
		_ = q.conn.Exec(context.WithoutCancel(ctx),
			fmt.Sprintf("SYSTEM START MERGES %s.route_unicast", q.db))
	})

	insertHistoryDuplicateFixture(t, ctx, q)

	// Scoped to this fixture's own router, not the whole table: an unscoped
	// count() != uniqExact() would start passing on some other fixture's
	// unmerged duplicates and prove nothing about this one.
	var raw, distinct uint64
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(), uniqExact((router_ip, peer_ip, rib, prefix, path_id, stream_seq))
		 FROM %s.route_unicast WHERE router_ip = ?`, q.db),
		historyDuplicateFixtureRouterIP).Scan(&raw, &distinct); err != nil {
		t.Fatal(err)
	}
	if raw == distinct {
		t.Fatalf("precondition failed: count()=%d equals uniqExact()=%d, so the "+
			"redelivered row was merged away and this test proves nothing", raw, distinct)
	}

	got := mustHistory(t, ctx, q, HistoryFilter{Prefix: historyDuplicateFixturePrefix})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 -- a JetStream redelivery of one message "+
			"is one event, not two announcements at the identical microsecond "+
			"(raw rows for this router: %d): %+v", len(got), raw, got)
	}
}

// TestRouteHistoryOrderByIsPinnedInTheStatement asserts historySQL's ORDER BY
// as TEXT, which is a weaker kind of test than the rest of this file and is
// here for one specific reason.
//
// stream_seq is the LAST tiebreaker, and it can only ever change the order of
// rows that are already tied on ts_collector and seq. Deleting it therefore
// does not make the query wrong so much as nondeterministic: ClickHouse is
// free to return the tied pair in either order, so the ordering subtest in
// TestRouteHistory fails on some runs and passes on others depending on how
// the sort was parallelized. A test that fails intermittently is worse than no
// test, so the guarantee is pinned here instead, where deleting the column
// from the ORDER BY fails every time.
//
// ts_collector and seq are NOT pinned by this test alone -- their mutations
// are caught by result, in TestRouteHistory's own subtests. They are asserted
// here only because the whole clause is.
func TestRouteHistoryOrderByIsPinnedInTheStatement(t *testing.T) {
	const want = "ORDER BY r.ts_collector DESC, r.seq DESC, r.stream_seq DESC"
	if !strings.Contains(historySQL, want) {
		t.Errorf("historySQL does not contain %q -- ts_collector leads because it "+
			"is the only trustworthy clock in the row, seq breaks its "+
			"batch-granular ties, and stream_seq breaks seq's (seq restarts each "+
			"session). Dropping stream_seq makes the order of a tied pair "+
			"nondeterministic rather than wrong, which no result-based test can "+
			"pin without failing intermittently.", want)
	}
	// ts_router must not appear in the ordering at all: it is
	// router-reported, and a router with a dead clock reports 1970.
	if i := strings.Index(historySQL, "ORDER BY"); i >= 0 && strings.Contains(historySQL[i:], "ts_router") {
		t.Error("historySQL orders on ts_router, which is router-reported and " +
			"untrusted; it is REPORTED (the contract asks for it) but must never " +
			"order or filter this timeline")
	}
}

// TestHistoryFilterRendersOnlyWhatWasAsked is the rendered-SQL counterpart to
// the behavior tests above, in the shape TestRouteFilterRendersOnlyWhatWasAsked
// already uses for RouteFilter: it catches a predicate that stopped being
// emitted, where those catch one emitted against the wrong column.
//
// The prefix predicate's exact TEXT is what matters most here and is worth
// naming: `r.prefix = ?`, a bare equality against the column. idx_prefix is a
// bloom filter on prefix and is the only index that can serve this query at
// all (route_unicast's PRIMARY KEY leads with router_ip/peer_ip/rib), so any
// rewrite that wrapped the column in a function -- lower(), a LIKE, a
// splitByChar -- would silently give up granule skipping and scan the table.
// No result-based test can see the difference; this one can.
func TestHistoryFilterRendersOnlyWhatWasAsked(t *testing.T) {
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		f      HistoryFilter
		clause string
		args   []any
	}{
		{
			name:   "prefix alone renders one bare equality",
			f:      HistoryFilter{Prefix: "10.110.0.0/24"},
			clause: "WHERE r.prefix = ?",
			args:   []any{"10.110.0.0/24"},
		},
		{
			name:   "an unset Since renders nothing at all",
			f:      HistoryFilter{Prefix: "10.110.0.0/24", Since: time.Time{}},
			clause: "WHERE r.prefix = ?",
			args:   []any{"10.110.0.0/24"},
		},
		{
			name:   "Since renders an inclusive bound on ts_collector",
			f:      HistoryFilter{Prefix: "10.110.0.0/24", Since: at},
			clause: "WHERE r.prefix = ? AND r.ts_collector >= fromUnixTimestamp64Micro(?)",
			args:   []any{"10.110.0.0/24", at.UnixMicro()},
		},
		{
			name: "everything",
			f: HistoryFilter{
				Prefix: "10.110.0.0/24",
				Since:  at,
				Router: netip.MustParseAddr("10.0.0.110"),
				Peer:   netip.MustParseAddr("10.0.0.111"),
				RIB:    "in_pre",
			},
			clause: "WHERE r.prefix = ? AND r.ts_collector >= fromUnixTimestamp64Micro(?)" +
				" AND r.router_ip = toIPv6(?) AND r.peer_ip = toIPv6(?) AND r.rib = ?",
			args: []any{"10.110.0.0/24", at.UnixMicro(), "10.0.0.110", "10.0.0.111", "in_pre"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.predicates()
			if got.clause() != tc.clause {
				t.Errorf("clause() = %q, want %q", got.clause(), tc.clause)
			}
			gotArgs := got.values()
			if len(gotArgs) != len(tc.args) {
				t.Fatalf("values() = %v, want %v", gotArgs, tc.args)
			}
			for i := range tc.args {
				if gotArgs[i] != tc.args[i] {
					t.Errorf("values()[%d] = %v (%T), want %v (%T)",
						i, gotArgs[i], gotArgs[i], tc.args[i], tc.args[i])
				}
			}
			if n := strings.Count(got.clause(), "?"); n != len(gotArgs) {
				t.Errorf("%d placeholders but %d values", n, len(gotArgs))
			}
		})
	}

	// The structural guard, for the reason
	// TestRouteFilterRendersEveryFieldItCarries gives: a dimension added to
	// HistoryFilter and forgotten in predicates() renders nothing and
	// silently widens every answer.
	all := HistoryFilter{
		Prefix: "10.110.0.0/24",
		Since:  at,
		Router: netip.MustParseAddr("10.0.0.110"),
		Peer:   netip.MustParseAddr("10.0.0.111"),
		RIB:    "in_pre",
	}
	if got, want := len(all.predicates().conds), reflect.TypeFor[HistoryFilter]().NumField(); got != want {
		t.Errorf("predicates() rendered %d conditions for a HistoryFilter with "+
			"all %d fields set", got, want)
	}
}
