package query

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ribKeyText renders one route key for set comparison. %#v rather than %v,
// because %v renders []any{"a b", uint32(0)} and []any{"a", "b", uint32(0)}
// identically -- a set comparison written on an ambiguous rendering can report
// two different keys as one and pass.
func ribKeyText(key []any) string { return fmt.Sprintf("%#v", key) }

// walkRIB drives an entire walk from page one and returns every page's rows
// alongside the cursor that page returned, so a test can assert on the shape
// of the walk (page sizes, where the nil cursor lands) as well as on its
// contents.
//
// It fails rather than looping forever if the walk does not terminate: a
// cursor that never advances -- the `>=` mutation, or a keyset predicate
// dropped altogether -- would otherwise hang the whole package's test binary
// instead of reporting a defect.
func walkRIB[T any](t *testing.T, page func(cur *RIBCursor) ([]T, *RIBCursor, error)) ([][]T, []*RIBCursor) {
	t.Helper()
	var pages [][]T
	var cursors []*RIBCursor
	var cur *RIBCursor
	for {
		rows, next, err := page(cur)
		if err != nil {
			t.Fatalf("page %d of the walk: %v", len(pages)+1, err)
		}
		pages = append(pages, rows)
		cursors = append(cursors, next)
		if next == nil {
			return pages, cursors
		}
		// A cursor that does not move is the non-termination this helper has
		// to catch, and catching it HERE rather than at a page cap is both the
		// better diagnostic and the difference between a mutation run that
		// takes seconds and one that runs a hundred whole-RIB aggregations per
		// walk before giving up.
		if cur != nil && ribKeyText(cur.Last) == ribKeyText(next.Last) {
			t.Fatalf("page %d returned the same cursor position it was given (%v); "+
				"the walk repeats this page forever", len(pages), next.Last)
		}
		if len(pages) > 100 {
			t.Fatalf("the walk did not terminate after %d pages", len(pages))
		}
		cur = next
	}
}

// ribKeyCounts flattens a walk's pages into a count per route key. Counting,
// not collecting into a set: a walk that returns a row twice and a walk that
// returns it once are different answers, and a set alone cannot tell them
// apart.
func ribKeyCounts[T any](pages [][]T, keyOf func(T) []any) map[string]int {
	counts := map[string]int{}
	for _, page := range pages {
		for _, row := range page {
			counts[ribKeyText(keyOf(row))]++
		}
	}
	return counts
}

// assertWalkedExactly is the assertion the whole of this file exists to make:
// the union of a walk's pages is EXACTLY the fixture's key set, each key once,
// and the number of ROWS it took to say so is the number of rows the fixture
// wrote.
//
// It compares sets rather than page lengths, and that distinction is the
// point. Three pages of 2, 2 and 1 is the right shape for a five-route walk at
// limit 2, and it is also the shape a walk produces when it skips one route
// and repeats another -- the lengths agree, the pages look ordered, and the
// answer is wrong by two rows. Only naming every key that came back, and
// comparing that against every key the fixture wrote, can tell the two apart.
func assertWalkedExactly[T any](t *testing.T, pages [][]T, keyOf func(T) []any, want [][]any) {
	t.Helper()
	got := ribKeyCounts(pages, keyOf)

	// The ROW count, before anything about keys. This assertion was added
	// after a mutation ran straight past the key comparison below: with rib
	// removed from the page key, a rib-less walk over the cross-rib fixture
	// returned 6 of its 7 rows at limits 1 and 3 and the key comparison saw
	// nothing wrong, because the two rows the key could no longer tell apart
	// collapsed into one entry that was still present. A key-based assertion
	// is blind in exactly the case where the KEY is the thing that broke, and
	// this count is not -- it is the one check here that does not go through
	// keyOf at all.
	total := 0
	for _, page := range pages {
		total += len(page)
	}
	if total != len(want) {
		t.Errorf("the walk returned %d rows, want %d -- rows can go missing without "+
			"the key comparison below noticing, whenever two rows the key cannot "+
			"tell apart collapse into one entry that is still there",
			total, len(want))
	}

	var dupes, missing, extra []string
	for _, k := range want {
		text := ribKeyText(k)
		switch n := got[text]; {
		case n == 0:
			missing = append(missing, text)
		case n > 1:
			dupes = append(dupes, fmt.Sprintf("%s x%d", text, n))
		}
	}
	wanted := map[string]bool{}
	for _, k := range want {
		wanted[ribKeyText(k)] = true
	}
	for text := range got {
		if !wanted[text] {
			extra = append(extra, text)
		}
	}
	slices.Sort(dupes)
	slices.Sort(missing)
	slices.Sort(extra)

	if len(dupes) > 0 {
		t.Errorf("the walk returned %d key(s) more than once: %s -- keyset "+
			"pagination's whole promise is that a row appears on exactly one page, "+
			"and a `>=` where the predicate wants `>` breaks exactly this",
			len(dupes), strings.Join(dupes, ", "))
	}
	if len(missing) > 0 {
		t.Errorf("the walk never returned %d key(s) the fixture wrote: %s -- a gap "+
			"is what an ordering that disagrees with the cursor produces, and every "+
			"page of it still looks well formed",
			len(missing), strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("the walk returned %d key(s) the fixture never wrote for this "+
			"(router, peer, rib, session): %s -- something the walk is supposed to "+
			"scope out reached the answer",
			len(extra), strings.Join(extra, ", "))
	}
}

// pageSizes renders a walk's shape for a failure message: a test that reports
// only "3 pages, want 1" leaves a reader guessing whether the extra pages were
// full, short or empty, which is exactly the distinction the probe row exists
// to make.
func pageSizes[T any](pages [][]T) []int {
	out := make([]int, len(pages))
	for i, p := range pages {
		out[i] = len(p)
	}
	return out
}

// assertPageSizes holds a walk to its own limit. It is a separate assertion
// from assertWalkedExactly because the two catch different things and neither
// catches the other's: a statement with no LIMIT at all returns every row on
// page one, and the union of that single page is still exactly the fixture's
// key set, so the set assertion passes with the pagination entirely absent.
func assertPageSizes[T any](t *testing.T, pages [][]T, limit int) {
	t.Helper()
	for i, page := range pages {
		if len(page) > limit {
			t.Errorf("page %d returned %d rows, more than the limit of %d -- the "+
				"statement's LIMIT is not doing anything", i+1, len(page), limit)
		}
	}
}

func TestRIBPageUnicast(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBWalkFixture(t, ctx, q)

	router := netip.MustParseAddr(ribFixtureRouterIP)
	peer := netip.MustParseAddr(ribFixturePeerIP)
	page := func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
		return q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, cur, 2)
	}

	t.Run("a full walk at limit 2 returns every route exactly once", func(t *testing.T) {
		pages, _ := walkRIB(t, page)
		assertWalkedExactly(t, pages, unicastRIBSpec.keyOf, ribFixtureUnicastKeys)
		assertPageSizes(t, pages, 2)
	})

	t.Run("five routes at limit 2 is three pages, and only the last has no cursor", func(t *testing.T) {
		pages, cursors := walkRIB(t, page)
		if len(pages) != 3 {
			t.Fatalf("the walk took %d pages, want 3 -- five routes at limit 2", len(pages))
		}
		for i, c := range cursors[:len(cursors)-1] {
			if c == nil {
				t.Errorf("page %d returned a nil cursor with more rows still to come", i+1)
			}
		}
		if cursors[len(cursors)-1] != nil {
			t.Errorf("the final page returned a cursor (%+v), want nil -- "+
				"api/openapi.yaml says next_cursor is non-null only when another page "+
				"exists, which is why the statement asks for limit+1 rows and uses the "+
				"extra one as a probe rather than guessing from a full page",
				cursors[len(cursors)-1])
		}
		if n := len(pages[len(pages)-1]); n != 1 {
			t.Errorf("the final page returned %d rows, want 1", n)
		}
	})

	t.Run("a walk whose length is a multiple of the limit does not end on an empty page", func(t *testing.T) {
		// api/openapi.yaml says next_cursor is non-null "only when another
		// page exists", and this is the case that makes that claim mean
		// something: five routes at limit 5 is one page, not one page and an
		// empty one behind a cursor that was handed out on a hunch. It is the
		// only shape that can tell the probe row apart from the guess it
		// replaces -- at limit 2 the final page holds 1 row and both readings
		// agree.
		pages, cursors := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, cur, len(ribFixtureUnicastKeys))
		})
		if len(pages) != 1 {
			t.Errorf("the walk took %d pages, want 1 -- a full page is not evidence "+
				"that another one exists, which is why the statement asks for limit+1 "+
				"rows; page sizes were %v", len(pages), pageSizes(pages))
		}
		if cursors[0] != nil {
			t.Errorf("a page holding every route returned a cursor (%+v), want nil", cursors[0])
		}
		assertWalkedExactly(t, pages, unicastRIBSpec.keyOf, ribFixtureUnicastKeys)
	})

	t.Run("a walk at limit 1 is one page per route, none of them empty", func(t *testing.T) {
		pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, cur, 1)
		})
		assertWalkedExactly(t, pages, unicastRIBSpec.keyOf, ribFixtureUnicastKeys)
		assertPageSizes(t, pages, 1)
		if len(pages) != len(ribFixtureUnicastKeys) {
			t.Errorf("the walk took %d pages, want %d -- page sizes were %v",
				len(pages), len(ribFixtureUnicastKeys), pageSizes(pages))
		}
	})

	t.Run("every page carries the walk's own pin forward unchanged", func(t *testing.T) {
		_, cursors := walkRIB(t, page)
		for i, c := range cursors {
			if c == nil {
				continue
			}
			if c.Collector != defaultFixtureCollector || c.SessionID != ribFixtureCurSession {
				t.Errorf("page %d's cursor pins (%q, %d), want (%q, %d) -- the pin is "+
					"taken once, on page one, and every later page is scoped to it",
					i+1, c.Collector, c.SessionID, defaultFixtureCollector, ribFixtureCurSession)
			}
		}
	})

	t.Run("the paged answer is the same set as an unpaged one", func(t *testing.T) {
		rows, cur, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, nil, 1000)
		if err != nil {
			t.Fatalf("single-page walk: %v", err)
		}
		if cur != nil {
			t.Errorf("a page holding every route returned a cursor (%+v), want nil", cur)
		}
		assertWalkedExactly(t, [][]Route{rows}, unicastRIBSpec.keyOf, ribFixtureUnicastKeys)
	})

	t.Run("nothing outside the walked router, peer, rib and session reaches the answer", func(t *testing.T) {
		// assertWalkedExactly already fails on any of these, since each is a
		// key the fixture's answer does not contain (or, for two of them, a
		// duplicate of one it does). This subtest names them individually so
		// that a failure says WHICH predicate stopped working rather than
		// leaving a reader to work it out from a diff of key sets.
		pages, _ := walkRIB(t, page)
		var got []string
		for _, p := range pages {
			for _, r := range p {
				got = append(got, r.Prefix)
			}
		}
		for _, tc := range []struct{ prefix, why string }{
			{ribFixtureWithdrawnPrefix, "its newest observation is a withdrawal (HAVING live_is_withdraw = 0)"},
			{ribFixtureOldSessionPrefix, "it belongs to a superseded session (the cur join and the r.session_id pin)"},
			{ribFixtureOtherRIBPrefix, "it was observed under another rib (r.rib)"},
			{ribFixtureOtherPeerPrefix, "another peer on this router advertised it (r.peer_ip)"},
			{ribFixtureDownPeerPrefix, "its peer is down as of this session (the peer_up gate)"},
			{ribFixtureOtherRouterPrefix, "another router advertised it (r.router_ip)"},
		} {
			if slices.Contains(got, tc.prefix) {
				t.Errorf("%s reached the walk; it must not, because %s", tc.prefix, tc.why)
			}
		}
	})

	t.Run("every returned row really is this router, peer and rib", func(t *testing.T) {
		pages, _ := walkRIB(t, page)
		for _, p := range pages {
			for _, r := range p {
				if r.RouterIP != router || r.PeerIP != peer || r.RIB != ribFixtureRIB {
					t.Errorf("a row escaped the walk's scope: router %s, peer %s, rib %q; "+
						"want %s, %s, %q", r.RouterIP, r.PeerIP, r.RIB, router, peer, ribFixtureRIB)
				}
				if r.Collector != defaultFixtureCollector {
					t.Errorf("a row came from collector %q, want %q", r.Collector, defaultFixtureCollector)
				}
			}
		}
	})

	t.Run("a page carries the same decoration Routes gives the same row", func(t *testing.T) {
		// RIBPageUnicast and Routes run the same statement under different
		// orderings, so this is really a claim that the shared body and the
		// shared scan loop are in fact shared -- a copy of either that drifted
		// would show up here first.
		rows, _, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, nil, 1000)
		if err != nil {
			t.Fatalf("single-page walk: %v", err)
		}
		i := slices.IndexFunc(rows, func(r Route) bool { return r.Prefix == ribFixturePrefixTwo })
		if i < 0 {
			t.Fatalf("%s is missing from the walk", ribFixturePrefixTwo)
		}
		got := rows[i]
		if got.OriginASN != 65099 {
			t.Errorf("OriginASN = %d, want 65099 -- derived in Go from the AS path, "+
				"the same way Routes derives it", got.OriginASN)
		}
		if got.NextHop.String() != "10.9.9.9" {
			t.Errorf("NextHop = %s, want 10.9.9.9 (in plain form, not IPv4-mapped)", got.NextHop)
		}
		if got.DumpState != "dumping" {
			t.Errorf("DumpState = %q, want %q -- this peer's session carries ipv4u "+
				"routes and no ipv4u marker", got.DumpState, "dumping")
		}
	})
}

// TestRIBPageUnicastAcrossEveryRIB is the test for putting rib in the page
// key: a walk with rib UNSET returns this peer's whole current table,
// across every rib it reported one under, with no duplicates and no gaps.
//
// The load-bearing pair is ribFixturePrefixTwo under in_pre and under in_post.
// Those two rows share a prefix and a path_id and differ only in rib -- the
// shape the live archive carries for real, where 10.99.1.0/24 and 10.99.2.0/24
// sit under in_pre, in_post and loc_rib at once. Both must come back. A key
// without rib in it cannot tell them apart: they collapse to one key, which is
// a tie in the walk's own ordering, and a tie is a route silently dropped the
// moment it straddles a page boundary.
//
// It walks at EVERY limit from 1 to the answer's size rather than at one, and
// that is not thoroughness for its own sake. Where a tie falls relative to a
// page boundary is a function of the limit: at limit 2 over this fixture the
// pair happens to land inside one page and a rib-less key loses nothing, so a
// test written at a single limit can pass against the very defect this exists
// to catch. Sweeping the limits guarantees the boundary lands between them.
func TestRIBPageUnicastAcrossEveryRIB(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBWalkFixture(t, ctx, q)

	router := netip.MustParseAddr(ribFixtureRouterIP)
	peer := netip.MustParseAddr(ribFixturePeerIP)
	want := ribFixtureUnicastKeysEveryRIB

	for limit := 1; limit <= len(want); limit++ {
		t.Run(fmt.Sprintf("at limit %d", limit), func(t *testing.T) {
			pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, router, peer, "", cur, limit)
			})
			assertWalkedExactly(t, pages, unicastRIBSpec.keyOf, want)
			assertPageSizes(t, pages, limit)
		})
	}

	t.Run("the cross-rib pair really is one prefix and one path-id", func(t *testing.T) {
		// If the fixture ever stopped writing that prefix under two ribs, every
		// assertion above would still pass and none of them would be about
		// anything. This is the check that the pair exists at all.
		rows, _, err := q.RIBPageUnicast(ctx, router, peer, "", nil, 1000)
		if err != nil {
			t.Fatalf("single-page walk: %v", err)
		}
		var ribs []string
		for _, r := range rows {
			if r.Prefix == ribFixturePrefixTwo && r.PathID == 0 {
				ribs = append(ribs, r.RIB)
			}
		}
		slices.Sort(ribs)
		if want := []string{ribFixtureOtherRIB, ribFixtureRIB}; !slices.Equal(ribs, want) {
			t.Errorf("%s path-id 0 came back under ribs %v, want %v -- without two "+
				"rows sharing a (prefix, path_id) this test cannot tell a key with rib "+
				"in it from one without", ribFixturePrefixTwo, ribs, want)
		}
	})

	t.Run("a rib-scoped walk is still narrowed", func(t *testing.T) {
		// The rib predicate did not simply become dead when rib joined the key:
		// naming one still returns that rib alone, which is what keeps the
		// contract's optional ?rib= meaningful in both settings.
		pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, ribFixtureOtherRIB, cur, 2)
		})
		assertWalkedExactly(t, pages, unicastRIBSpec.keyOf, [][]any{
			{ribFixtureOtherRIB, ribFixturePrefixTwo, uint32(0)},
			{ribFixtureOtherRIB, ribFixtureOtherRIBPrefix, uint32(0)},
		})
	})
}

// TestRIBPageUnicastRefusesASupersededSession is the pin's whole reason for
// existing, asserted from both directions.
//
// The first half is the loud failure: a cursor pinned to a session the router
// has replaced comes back as ErrSessionChanged with no rows and no cursor. The
// second half is what makes that mean something rather than merely be true --
// a FRESH walk of the same peer reports the new session's view, which is
// empty, instead of the superseded dump's two routes. Without it, a query that
// answered from whichever session had rows would satisfy the first assertion
// and still be reporting a dump that no longer exists.
func TestRIBPageUnicastRefusesASupersededSession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBResetFixture(t, ctx, q)

	router := netip.MustParseAddr(ribResetRouterIP)
	peer := netip.MustParseAddr(ribResetPeerIP)

	t.Run("a cursor pinned to the superseded session is refused", func(t *testing.T) {
		stale := &RIBCursor{
			Collector: defaultFixtureCollector,
			SessionID: ribResetOldSession,
			Router:    router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, ribResetPrefixA, uint32(0)},
		}
		rows, cur, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, stale, 10)
		if !errors.Is(err, ErrSessionChanged) {
			t.Fatalf("err = %v, want ErrSessionChanged -- the router reconnected "+
				"between pages, so the pages already fetched describe a dump that no "+
				"longer exists", err)
		}
		if errors.Is(err, ErrBadFilter) {
			t.Error("the error also wraps ErrBadFilter; the two are different failures " +
				"with different remedies (a 409 that says restart, versus a 400 that " +
				"says the question was malformed) and cmd/vantage-api tells them apart " +
				"with errors.Is")
		}
		if len(rows) != 0 || cur != nil {
			t.Errorf("got %d rows and cursor %+v alongside the error; a superseded "+
				"walk returns nothing, never a plausible page from another dump",
				len(rows), cur)
		}
	})

	t.Run("a fresh walk pins the new session, not the dump that has rows", func(t *testing.T) {
		rows, cur, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, nil, 10)
		if err != nil {
			t.Fatalf("fresh walk: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("got %d rows, want 0 -- the current session has re-dumped from "+
				"scratch and delivered nothing yet; the two rows still sitting in "+
				"route_unicast belong to session %d, which no longer exists: %+v",
				len(rows), ribResetOldSession, rows)
		}
		if cur != nil {
			t.Errorf("an empty page returned a cursor (%+v), want nil", cur)
		}
	})

	t.Run("the pinned session is compared, not merely present", func(t *testing.T) {
		// A cursor naming the CURRENT session walks fine, so the subtest above
		// is not passing because every cursor is refused.
		fresh := &RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribResetNewSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
		}
		if _, _, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, fresh, 10); err != nil {
			t.Fatalf("a cursor pinned to the CURRENT session was refused: %v", err)
		}
	})
}

// TestRIBPageTwoCollectors covers the deployment peerStateCTE calls
// unsupported-but-not-silent: one router, two collectors, and a walk that has
// to be pinned to one (collector, session) pair.
func TestRIBPageTwoCollectors(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBTwoCollectorFixture(t, ctx, q)

	router := netip.MustParseAddr(ribTwoCollectorRouterIP)
	peer := netip.MustParseAddr(ribTwoCollectorPeerIP)

	t.Run("a cursor-less walk is refused rather than resolved to one of them", func(t *testing.T) {
		_, _, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, nil, 10)
		if err == nil {
			t.Fatal("a router two collectors both monitor resolved to one silently; " +
				"both ways of picking discard the other collector's entire view without " +
				"a word (see ribPin)")
		}
		for _, want := range []string{ribCollectorA, ribCollectorB} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name collector %q: %v -- naming them is "+
					"what makes the remedy (start the walk with a cursor) actionable",
					want, err)
			}
		}
		// It wraps ErrBadFilter, not ErrSessionChanged and not nothing: the
		// caller CAN supply something better -- a cursor naming the collector,
		// which the subtest below walks -- and "you can supply something
		// better" is exactly what a 400 means. Unwrapped, cmd/vantage-api
		// would have nothing to match on but message text and no choice but a
		// 500, which api/openapi.yaml has no case for on these paths and which
		// blames the wrong side.
		if !errors.Is(err, ErrBadFilter) {
			t.Errorf("err does not wrap ErrBadFilter: %v -- an unwrapped error leaves "+
				"the HTTP layer matching on message text, or answering 500 for a "+
				"request the caller can fix", err)
		}
		if errors.Is(err, ErrSessionChanged) {
			t.Errorf("err wraps ErrSessionChanged: %v -- nothing reconnected; "+
				"restarting the walk changes nothing without a collector named", err)
		}
	})

	t.Run("a cursor naming one collector walks only that collector's rows", func(t *testing.T) {
		pin := &RIBCursor{
			Collector: ribCollectorA, SessionID: ribTwoCollectorSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
		}
		rows, cur, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, pin, 10)
		if err != nil {
			t.Fatalf("walk pinned to %s: %v", ribCollectorA, err)
		}
		if cur != nil {
			t.Errorf("got a cursor (%+v) for a page holding every route, want nil", cur)
		}
		// Both collectors wrote the same two prefixes under the same session
		// id, so without the walk's own r.collector_id predicate this is four
		// rows under two keys -- each key duplicated, and a duplicated key is
		// a route the pager can drop at a page boundary.
		assertWalkedExactly(t, [][]Route{rows}, unicastRIBSpec.keyOf, [][]any{
			{ribFixtureRIB, ribTwoCollectorPrefixA, uint32(0)},
			{ribFixtureRIB, ribTwoCollectorPrefixB, uint32(0)},
		})
		for _, r := range rows {
			if r.Collector != ribCollectorA {
				t.Errorf("a row came from collector %q, want %q", r.Collector, ribCollectorA)
			}
		}
	})
}

// TestRIBPagePinMatchesTheCurrentSession ties ribPinSQL to peerStateCTE's cur
// without either naming the other's SQL text.
//
// They have to agree: cur is what the page statement's own INNER JOIN scopes
// route rows by, so a pin resolved from a different reading of "the current
// session" would pin every walk to a session the page query does not consider
// current, and every page would come back empty with no error anywhere. Routers
// reports cur's own answer, which is what makes it the right thing to compare
// against here.
func TestRIBPagePinMatchesTheCurrentSession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBWalkFixture(t, ctx, q)

	collector, sid, err := q.ribPin(ctx, netip.MustParseAddr(ribFixtureRouterIP))
	if err != nil {
		t.Fatalf("ribPin: %v", err)
	}
	routers, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	i := slices.IndexFunc(routers, func(r Router) bool {
		return r.IP.String() == ribFixtureRouterIP && r.Collector == collector
	})
	if i < 0 {
		t.Fatalf("ribPin resolved (%q, %d) for %s, but Routers has no row for that "+
			"collector at all -- the two disagree about which session is current",
			collector, sid, ribFixtureRouterIP)
	}
	if routers[i].SessionID != sid {
		t.Errorf("ribPin resolved session %d, Routers reports %d -- a walk pinned to "+
			"a session the page statement's own cur join does not consider current "+
			"returns an empty page and no error", sid, routers[i].SessionID)
	}
	if sid != ribFixtureCurSession {
		t.Errorf("both resolved session %d, but the fixture's current session is %d",
			sid, ribFixtureCurSession)
	}
}

// TestRIBPageRejectsAWalkItCannotKey holds the two narrowings a walk cannot do
// without. Both are optional on other surfaces in this package and required
// here, for one reason: a walk is one peer's table, and an omitted router or
// peer is not a wider answer but an unpaginated fleet-wide dump under a key
// that does not identify its rows.
//
// rib is NOT among them. It is part of the page key rather than merely a
// filter (see ribColumn), so an unset rib is a well-defined walk across
// every rib the peer has -- which is what TestRIBPageUnicastAcrossEveryRIB
// asserts, and what makes the key's leading column load-bearing rather
// than decorative.
func TestRIBPageRejectsAWalkItCannotKey(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	router := netip.MustParseAddr(ribFixtureRouterIP)
	peer := netip.MustParseAddr(ribFixturePeerIP)
	for _, tc := range []struct {
		name          string
		router, peer  netip.Addr
		rib           string
		wantInMessage string
	}{
		{"no router", netip.Addr{}, peer, ribFixtureRIB, "router"},
		{"no peer", router, netip.Addr{}, ribFixtureRIB, "peer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, cur, err := q.RIBPageUnicast(ctx, tc.router, tc.peer, tc.rib, nil, 10)
			if !errors.Is(err, ErrBadFilter) {
				t.Fatalf("err = %v, want ErrBadFilter -- an omitted narrowing here is "+
					"not a wider answer, it is a broken walk", err)
			}
			if !strings.Contains(err.Error(), tc.wantInMessage) {
				t.Errorf("the error does not say which narrowing is missing: %v", err)
			}
			if len(rows) != 0 || cur != nil {
				t.Errorf("got %d rows and cursor %+v alongside the error", len(rows), cur)
			}
		})
	}
	// The same call with all three present is accepted, so the cases above are
	// not passing against a function that refuses everything.
	if _, _, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, nil, 10); err != nil {
		t.Fatalf("a fully specified walk was refused: %v", err)
	}
}

// TestRIBPageRejectsATamperedCursor covers the one input to this package that
// makes a round trip through an encoding nobody here controls: the cursor is
// handed to a client as an opaque string and handed back. api/openapi.yaml
// says a tampered one is a 400, and ErrBadFilter is what makes that true --
// the alternative is a tuple comparison of the wrong arity or the wrong types,
// which is a 500 at best and a quietly different comparison at worst.
func TestRIBPageRejectsATamperedCursor(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBWalkFixture(t, ctx, q)

	router := netip.MustParseAddr(ribFixtureRouterIP)
	peer := netip.MustParseAddr(ribFixturePeerIP)
	for _, tc := range []struct {
		name string
		cur  RIBCursor
	}{
		{"no collector", RIBCursor{
			SessionID: ribFixtureCurSession,
			Router:    router, Peer: peer, RIB: ribFixtureRIB,
		}},
		{"no session", RIBCursor{
			Collector: defaultFixtureCollector,
			Router:    router, Peer: peer, RIB: ribFixtureRIB,
		}},
		{"too few key values", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, ribFixturePrefixTwo},
		}},
		{"too many key values", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0), "extra"},
		}},
		{"a path_id that survived a JSON round trip as a float", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, ribFixturePrefixTwo, float64(0)},
		}},
		{"a prefix and a path_id transposed", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, uint32(0), ribFixturePrefixTwo},
		}},
		// rib is the one key column with a constrained domain, and a type
		// check is not a domain check for it: this exact cursor used to reach
		// ClickHouse and come back as "Code: 691 ... UNKNOWN_ELEMENT_OF_ENUM",
		// which is a 500 for the failure api/openapi.yaml types as a 400.
		{"a rib that is not one of the enum's members", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{"not_a_rib", ribFixturePrefixTwo, uint32(0)},
		}},
		{"an empty rib in the key, which is no enum member either", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, Peer: peer, RIB: ribFixtureRIB,
			Last: []any{"", ribFixturePrefixTwo, uint32(0)},
		}},
		{"no router", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Peer: peer, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)},
		}},
		{"no peer", RIBCursor{
			Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
			Router: router, RIB: ribFixtureRIB,
			Last: []any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := tc.cur
			rows, next, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, &cur, 10)
			if !errors.Is(err, ErrBadFilter) {
				t.Fatalf("err = %v, want ErrBadFilter", err)
			}
			if len(rows) != 0 || next != nil {
				t.Errorf("got %d rows and cursor %+v alongside the error", len(rows), next)
			}
		})
	}
	// A well-formed cursor of the same shape is accepted, so none of the above
	// passes merely because every cursor is refused.
	good := RIBCursor{
		Collector: defaultFixtureCollector, SessionID: ribFixtureCurSession,
		Router: router, Peer: peer, RIB: ribFixtureRIB,
		Last: []any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)},
	}
	if _, _, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, &good, 10); err != nil {
		t.Fatalf("a well-formed cursor was refused: %v", err)
	}
}

// TestRIBPageOnARouterWithNoSessionIsAnEmptyWalk pins the one case that is an
// answer rather than an error: a router this collector has never recorded a
// peer event for has no current session, so its current RIB is empty. Routes
// says the same thing about a prefix nobody advertises, and a walk that
// errored here would make "I have never heard of this router" indistinguishable
// from a database that was down.
func TestRIBPageOnARouterWithNoSessionIsAnEmptyWalk(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	rows, cur, err := q.RIBPageUnicast(ctx,
		netip.MustParseAddr("10.0.1.250"), netip.MustParseAddr("10.0.1.251"),
		ribFixtureRIB, nil, 10)
	if err != nil {
		t.Fatalf("walk of an unknown router: %v", err)
	}
	if len(rows) != 0 || cur != nil {
		t.Errorf("got %d rows and cursor %+v, want an empty walk", len(rows), cur)
	}
}

func TestClampRIBLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultRIBPage},
		{-1, DefaultRIBPage},
		{1, 1},
		{999, 999},
		{MaxRIBPage, MaxRIBPage},
		{MaxRIBPage + 1, MaxRIBPage},
		{1 << 30, MaxRIBPage},
	} {
		if got := clampRIBLimit(tc.in); got != tc.want {
			t.Errorf("clampRIBLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestRIBStatementBindsEveryPlaceholder holds ribStatement to this package's
// binding invariant -- one `?` emitted, one value appended -- and to the two
// things about the bound values that no result-based test over a small fixture
// can see.
//
// The clamp is the first. A limit of 50000 against a five-route fixture returns
// the same five rows whether it was clamped to 10000 or honored outright, so
// only the bound value can say which happened.
//
// The r.session_id pin is the second, and this test is where it is pinned at
// all. It is REDUNDANT against the statement's own INNER JOIN cur, which
// already restricts route rows to the router's current session, and every page
// that reaches ClickHouse has already had its pin checked against that same
// current session -- so no fixture can produce a state where deleting
// `r.session_id = ?` changes a row. It is kept because the walk's pin should be
// legible in the statement doing the walking rather than implied by a CTE
// defined in another file, and because it is what makes a page racing a
// reconnect come back empty rather than full of the new dump's rows. Asserting
// on the rendered statement is the only way to hold a predicate like that, and
// saying so here is better than leaving a future reader to find it unguarded.
func TestRIBStatementBindsEveryPlaceholder(t *testing.T) {
	router := netip.MustParseAddr(ribFixtureRouterIP)
	peer := netip.MustParseAddr(ribFixturePeerIP)

	t.Run("unicast, first page", func(t *testing.T) {
		sql, args, err := ribStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 7)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		// router, peer, rib, collector, session, limit; no keyset on page one.
		assertPlaceholders(t, sql, args, 6)
		if got := args[len(args)-1]; got != 8 {
			t.Errorf("the bound limit is %v, want 8 -- the statement asks for one row "+
				"more than the page size and uses it as a probe for whether another "+
				"page exists", got)
		}
	})

	t.Run("unicast, a later page", func(t *testing.T) {
		sql, args, err := ribStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession,
			[]any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)}, 7)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		// router, peer, rib, collector, session, three key columns, limit.
		assertPlaceholders(t, sql, args, 9)
		if !strings.Contains(sql, "(r.rib, r.prefix, r.path_id) > (?, ?, ?)") {
			t.Errorf("the keyset predicate is missing from the statement:\n%s", sql)
		}
	})

	t.Run("an unset rib omits its predicate but keeps its key column", func(t *testing.T) {
		sql, args, err := ribStatement(testDB, unicastRIBSpec, router, peer,
			"", defaultFixtureCollector, ribFixtureCurSession,
			[]any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)}, 7)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		// router, peer, collector, session, three key columns, limit -- one
		// fewer than the rib-scoped case, and the missing one is the filter,
		// not the key.
		assertPlaceholders(t, sql, args, 8)
		if strings.Contains(sql, "r.rib = ?") {
			t.Errorf("an unset rib still rendered a rib predicate:\n%s", sql)
		}
		if !strings.Contains(sql, "(r.rib, r.prefix, r.path_id) > (?, ?, ?)") {
			t.Errorf("the key lost its rib column when the FILTER was omitted; the "+
				"two are independent, and a key without rib ties every row a peer "+
				"reports under more than one:\n%s", sql)
		}
		if !strings.Contains(sql, "ORDER BY r.rib, r.prefix, r.path_id") {
			t.Errorf("the ORDER BY lost its rib column:\n%s", sql)
		}
	})

	t.Run("evpn binds the family ahead of the filter and the limit after it", func(t *testing.T) {
		sql, args, err := ribStatement(testDB, evpnRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 7)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		// The eor join's family, then the five narrowings, then the limit.
		assertPlaceholders(t, sql, args, 7)
		if args[0] != evpnFamily {
			t.Errorf("args[0] = %v, want %q -- the eor join's placeholder sits above "+
				"the outer WHERE, and the driver binds strictly left to right",
				args[0], evpnFamily)
		}
		if got := args[len(args)-1]; got != 8 {
			t.Errorf("the bound limit is %v, want 8", got)
		}
	})

	t.Run("a limit above the maximum is clamped, not honored", func(t *testing.T) {
		_, args, err := ribStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 50000)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		if got, want := args[len(args)-1], MaxRIBPage+1; got != want {
			t.Errorf("the bound limit is %v, want %d -- api/openapi.yaml caps limit at "+
				"%d, and a caller asking for more is clamped rather than refused",
				got, want, MaxRIBPage)
		}
	})

	t.Run("a limit at or below zero takes the default", func(t *testing.T) {
		_, args, err := ribStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 0)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		if got, want := args[len(args)-1], DefaultRIBPage+1; got != want {
			t.Errorf("the bound limit is %v, want %d", got, want)
		}
	})

	t.Run("the pin is in the statement, not only in the pin check", func(t *testing.T) {
		sql, args, err := ribStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 7)
		if err != nil {
			t.Fatalf("ribStatement: %v", err)
		}
		for _, want := range []string{
			"r.router_ip = toIPv6(?)", "r.peer_ip = toIPv6(?)", "r.rib = ?",
			"r.collector_id = ?", "r.session_id = ?",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("the statement is missing %q:\n%s", want, sql)
			}
		}
		if !slices.Contains(args, any(uint64(ribFixtureCurSession))) {
			t.Errorf("the pinned session is not among the bound values %v -- a "+
				"session_id is a UInt64 and must be bound as one, never through a "+
				"float that cannot hold a now().UnixNano() value", args)
		}
	})

	t.Run("unicast keys, a later page", func(t *testing.T) {
		sql, args, err := ribKeysStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession,
			[]any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)}, 7)
		if err != nil {
			t.Fatalf("ribKeysStatement: %v", err)
		}
		// The five narrowings, three key columns, the limit. No lead: phase
		// one binds none, which TestRIBSpecsWithKeysBodyBindNoLead states as
		// an invariant rather than an observation.
		assertPlaceholders(t, sql, args, 9)
		if got := args[len(args)-1]; got != 8 {
			t.Errorf("the bound limit is %v, want 8 -- phase one carries the probe row, "+
				"because it is phase one that decides how long the page is", got)
		}
	})

	t.Run("unicast attributes bound the page at both ends and carry no limit", func(t *testing.T) {
		// This is the assertion that holds ribAttrsStatement's upper bound in
		// place, and it has to be a structural one. Deleting the bound changes
		// no ANSWER: phase two would return the rest of the peer's RIB from
		// the cursor onwards, in key order, and ribPage would truncate it to
		// the page size and produce exactly the rows it produces now. What it
		// changes is the cost -- the bound is what makes phase two 10ms
		// instead of a second whole-RIB read, which is the entire reason the
		// two-phase reader is faster than the one it replaced. A mutation
		// pass over the reader found nothing that failed when the bound was
		// removed, which is what this subtest is answering.
		last := []any{ribFixtureRIB, ribFixturePrefixTwo, uint32(0)}
		upto := []any{ribFixtureRIB, ribFixturePrefixThree, uint32(0)}
		sql, args, err := ribAttrsStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, last, upto)
		if err != nil {
			t.Fatalf("ribAttrsStatement: %v", err)
		}
		// Five narrowings, three key columns opening the range, three closing
		// it. No limit: the range is the bound, and a LIMIT here would cap a
		// phase two that disagreed with phase one instead of letting the
		// comparison see it.
		assertPlaceholders(t, sql, args, 11)
		for _, want := range []string{
			"(r.rib, r.prefix, r.path_id) > (?, ?, ?)",
			"(r.rib, r.prefix, r.path_id) <= (?, ?, ?)",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("the attributes statement is missing %q, so it no longer reads "+
					"one page's span:\n%s", want, sql)
			}
		}
		if strings.Contains(sql, "LIMIT") {
			t.Errorf("the attributes statement carries a LIMIT:\n%s\nphase two is bounded "+
				"by the key range phase one measured; a LIMIT would silently cap a "+
				"disagreement between the phases instead of surfacing it", sql)
		}
	})

	t.Run("unicast attributes on the first page open at the bottom", func(t *testing.T) {
		// No cursor, so no lower bound -- but the upper bound is still there,
		// which is what keeps page one from reading the whole peer.
		upto := []any{ribFixtureRIB, ribFixturePrefixThree, uint32(0)}
		sql, args, err := ribAttrsStatement(testDB, unicastRIBSpec, router, peer,
			ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, upto)
		if err != nil {
			t.Fatalf("ribAttrsStatement: %v", err)
		}
		assertPlaceholders(t, sql, args, 8)
		if !strings.Contains(sql, "(r.rib, r.prefix, r.path_id) <= (?, ?, ?)") {
			t.Errorf("page one's attributes statement has no upper bound:\n%s", sql)
		}
	})
}

// assertPlaceholders holds one rendered statement to the binding invariant
// filters.go's own header states: every `?` emitted gets its own value
// appended. None of the three statement bodies carries a string literal
// containing a question mark, so counting the character is counting
// placeholders; if one is ever added, this is where that assumption stops
// holding.
//
// clickhouse-go raises on too few arguments but silently DISCARDS a surplus
// one (see peersStatement's own doc comment for the version and the
// verification), so a miscount in that direction binds every later value one
// position early and reports nothing.
func assertPlaceholders(t *testing.T, sql string, args []any, want int) {
	t.Helper()
	if n := strings.Count(sql, "?"); n != len(args) || n != want {
		t.Fatalf("%d placeholders, %d values, want %d of each", n, len(args), want)
	}
}

// TestRIBOrderAndKeysetNameTheSameColumns is the guard on the one way a keyset
// walk fails while every page still looks perfectly well formed.
//
// The predicate says "give me rows past here" and the ORDER BY says "here is
// what past means". If the two name different columns, or the same columns in
// a different order, the walk skips rows and repeats others and no single page
// is visibly wrong. Both are derived from one []ribKeyCol so they cannot
// disagree today; this asserts the derivation rather than the intent, so that
// a future ORDER BY written out by hand for one family fails here instead of
// in production.
//
// ls_nodes, ls_prefixes and ls_links are covered here too, alongside the
// three RIB families, because all three share the same keyOrder/keyset
// derivation (see lsNodesKey and lsNodesPageOrder, lsPrefixesKey and
// lsPrefixesPageOrder, and lsLinksKey and lsLinksPageOrder, all in
// lspage.go) rather than a family of its own: one guard for every scoped
// walk in this package, not one per resource.
func TestRIBOrderAndKeysetNameTheSameColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []ribKeyCol
		last []any
	}{
		{"unicast", unicastRIBKey, []any{"in_pre", "10.0.0.0/24", uint32(0)}},
		{"vpn", vpnRIBKey, []any{"in_pre", "65001:1", "10.0.0.0/24", uint32(0)}},
		{"evpn", evpnRIBKey, []any{
			"in_pre", uint8(2), "65001:1", "", "aa:bb:cc:00:00:01", "", uint32(0), "", uint32(0),
		}},
		{"ls_nodes", lsNodesKey, []any{"in_pre", "0a0000f1", uint64(12345)}},
		{"ls_prefixes", lsPrefixesKey, []any{"in_pre", "10.90.7.0/24", uint64(12345)}},
		{"ls_links", lsLinksKey, []any{
			"in_pre", "0a0000b0", uint64(12345), uint64(67890),
			uint32(1), uint32(2), "10.2.1.1", "10.2.1.2",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f filters
			if err := f.keyset(tc.key, tc.last); err != nil {
				t.Fatalf("keyset: %v", err)
			}
			pred := f.where()
			lhs, _, ok := strings.Cut(strings.TrimPrefix(pred, " AND ("), ") > (")
			if !ok {
				t.Fatalf("the keyset predicate is not the tuple comparison this test "+
					"knows how to read: %q", pred)
			}
			order := ribOrder(tc.key)
			orderCols, _, ok := strings.Cut(strings.TrimPrefix(order, "\nORDER BY "), "\nLIMIT")
			if !ok {
				t.Fatalf("ribOrder did not render an ORDER BY and a LIMIT: %q", order)
			}
			if lhs != orderCols {
				t.Errorf("the keyset compares (%s) but the walk orders by (%s); a walk "+
					"whose two halves disagree skips rows and repeats others, and every "+
					"page of it still looks well formed", lhs, orderCols)
			}
		})
	}
}

// ribKeyExemption is one column a route statement's GROUP BY carries that the
// family's page key deliberately does NOT, together with the argument that
// omitting it breaks no tie.
//
// It exists so that the exception is declared rather than tolerated.
// TestRIBPageKeysAreTheirStatementsUnpinnedGroupBy would otherwise have to
// choose between comparing the two lists exactly -- which fails today, because
// two of the three families really do omit a grouped column on purpose -- and
// comparing them loosely enough that the omission nobody meant slips through
// beside the two that were argued for. Naming each exemption keeps the
// comparison exact everywhere else: an exemption must match a column the
// statement still groups by, so the test fails when one is added, when one is
// removed, and when one stops being needed.
//
// why is carried, and required to be non-empty, because an exemption without
// an argument is precisely the silent pass this test exists to prevent. It is
// printed in the failure message so a reader who has just broken the guard
// sees what the omission was resting on.
type ribKeyExemption struct {
	col string
	why string
}

// ribWalkPinned is the part of every route statement's GROUP BY that a RIB
// walk fixes for the whole walk rather than paginating through: one collector,
// one router, one peer, named in ribStatement as equality predicates and
// therefore constant across every page.
//
// A pinned column cannot tie two rows apart inside a page, so it is the one
// part of the GROUP BY a page key is right to leave out -- which is exactly
// why the list is spelled here and then CHECKED against the rendered
// statement rather than assumed. If ribStatement ever stopped pinning one of
// these, its column would start varying within a walk, and the key would need
// it: two collectors' views of the same route are two rows under one key, a
// tie the keyset walk straddles and loses.
//
// r.session_id is not here. ribStatement pins it too, but no route statement
// groups by it, so it has nothing to subtract; see
// TestRIBStatementBindsEveryPlaceholder for where that predicate is held.
var ribWalkPinned = []string{"r.collector_id", "r.router_ip", "r.peer_ip"}

// TestRIBPageKeysAreTheirStatementsUnpinnedGroupBy makes structural a
// correspondence rib.go so far only argues in prose: each family's page key is
// exactly its own statement's GROUP BY minus the columns the walk pins.
//
// The failure it guards is the one this package has already shipped once. A
// GROUP BY names the granularity the answer comes back at; a page key names
// what distinguishes one row of that answer from another. When the key is
// SHORTER than the unpinned remainder, two rows differing only in the dropped
// column share a key -- a tie, which a keyset walk can straddle at a page
// boundary and lose outright, with every individual page still perfectly well
// formed. rib was that defect in the flesh: the live archive carries
// 10.99.1.0/24 under in_pre, in_post and loc_rib at once, and a key without
// rib dropped two of the three. When the key is LONGER, every cursor carries a
// column the statement cannot order by, and the walk raises instead.
//
// Prose caught rib eventually. It caught it after the fact, which is the
// argument for a test: adding a column to any of the three GROUP BYs without
// adding it to the key, or removing one from a key without removing it from
// the GROUP BY, now fails here rather than in a walk over data nobody has
// loaded yet.
//
// Matching the GROUP BY out of the statement TEXT needs care, and the trap is
// live in this package. All three statements are "WITH " + peerStateCTE +
// peerUpCTE + eorCTE + the outer query, and those CTEs contribute four GROUP
// BYs of their own before the outer one -- so a strings.Contains over the
// whole const, or a cut at the first GROUP BY, reads cur's or eor's and passes
// no matter what the outer statement does.
// TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree was written with exactly
// that hole and passed against a deliberately broken routersSQL. outerGroupBy
// anchors on the outer query's own FROM instead -- the route table the walk
// paginates, which no CTE reads -- and then refuses to guess if what follows
// does not hold exactly one GROUP BY.
func TestRIBPageKeysAreTheirStatementsUnpinnedGroupBy(t *testing.T) {
	// The two exemptions below are the same column for the same underlying
	// reason, spelled per family because the arguments are not identical: the
	// unicast one rests on prefix text alone, the VPN one needs rd as well.
	const unicastFamilyExempt = "route_unicast holds only ipv4u and ipv6u " +
		"(unicastFamilies, and api/openapi.yaml enumerates the same two), and sink " +
		"writes every prefix through netip.Prefix.String() -- an IPv4 NLRI renders " +
		"dotted-quad and never in mapped form, an IPv6 one with colons -- so one " +
		"prefix TEXT cannot belong to both families, and family breaks no tie " +
		"(rib, prefix, path_id) has not already broken. That is an argument from " +
		"the contract's family bound and the writer's spelling, NOT a constraint " +
		"route_unicast enforces, and it is weaker than the rib case by exactly " +
		"that much: for rib the tie is real and was found in the archive. What " +
		"makes it a non-fix is that family breaks no tie, so the key would grow " +
		"a column and change no answer; the contract naming (rib, prefix, " +
		"path_id) for /v1/rib/unicast means both move together, not that either " +
		"is fixed -- this contract is hand-written here and has been amended " +
		"twice: rib entered all three page keys, and covers= entered the VPN " +
		"and EVPN filters."
	const vpnFamilyExempt = "route_vpn holds vpn4, vpn6 and lu4 (vpnFamilies). " +
		"vpn4 versus vpn6 is the unicast argument unchanged: disjoint prefix text. " +
		"lu4 versus either is broken by rd, which vpnRIBKey already carries -- " +
		"bgp.decodeRD never returns \"\" without failing the whole NLRI, so lu4's " +
		"empty rd cannot equal a VPN route's. Family is therefore determined by " +
		"(rd, prefix) within one peer and one rib, and breaks no tie of its own."

	for _, tc := range []struct {
		name string
		// from is the outer query's own FROM, the anchor everything after
		// which belongs to the outer statement rather than to a CTE.
		from   string
		body   string
		key    []ribKeyCol
		stmt   string
		exempt []ribKeyExemption
	}{
		{
			name: "unicast",
			from: "\nFROM %[1]s.route_unicast_current r\n",
			body: routesSQL,
			key:  unicastRIBKey,
			stmt: renderRIBWalk(t, unicastRIBSpec),
			exempt: []ribKeyExemption{
				{col: "r.family", why: unicastFamilyExempt},
			},
		},
		{
			// Phase one of the two-phase unicast walk. It is a SECOND
			// statement whose GROUP BY defines the same page key, and a key
			// that drifts here is worse than one that drifts in routesSQL:
			// this statement produces the cursor position the next page is
			// walked from, so a granularity mismatch mis-slices pages while
			// every returned row stays perfectly well formed. Nothing else
			// checks it -- the parity test compares CONTENT, and two readers
			// grouping differently can still agree on a small fixture's
			// content.
			name: "unicast keys",
			from: "\nFROM %[1]s.route_unicast_current r\n",
			body: unicastRIBKeysSQL,
			key:  unicastRIBKey,
			stmt: renderRIBKeysWalk(t, unicastRIBSpec),
			exempt: []ribKeyExemption{
				// The same argument, for the same column, in a statement that
				// does not even SELECT it: family is grouped so that a group
				// holds one family or nothing downstream may assume it does.
				{col: "r.family", why: unicastFamilyExempt},
			},
		},
		{
			name: "vpn",
			from: "\nFROM %[1]s.route_vpn_current r\n",
			body: vpnRoutesSQL,
			key:  vpnRIBKey,
			stmt: renderRIBWalk(t, vpnRIBSpec),
			exempt: []ribKeyExemption{
				{col: "r.family", why: vpnFamilyExempt},
			},
		},
		{
			// EVPN has no exemption at all: route_evpn has no family column
			// to omit, and its GROUP BY remainder is evpnRIBKey column for
			// column. An empty exempt list is the shape the other two should
			// be growing towards, not a gap.
			name: "evpn",
			from: "\nFROM %[1]s.route_evpn_current r\n",
			body: evpnRoutesSQL,
			key:  evpnRIBKey,
			stmt: renderRIBWalk(t, evpnRIBSpec),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remainder := outerGroupBy(t, tc.body, tc.from)

			// Subtract the pin, checking as we go that the statement really
			// does pin it. Both halves matter: a column in this list that the
			// GROUP BY has dropped means the list is stale, and one the
			// rendered WHERE no longer fixes means the column now varies
			// within a walk and belongs in the key.
			where := ribWhereClause(t, tc.stmt)
			for _, col := range ribWalkPinned {
				if !strings.Contains(where, col+" = ") {
					t.Fatalf("the rendered walk no longer pins %s:\n%s\n"+
						"a walk that does not fix this column lets it vary from row "+
						"to row, which makes it a tie-breaker the page key has to "+
						"carry -- two collectors' views of one route are two rows "+
						"under one key, and a keyset walk straddling that tie loses "+
						"one of them", col, where)
				}
				var ok bool
				if remainder, ok = removeCol(remainder, col); !ok {
					t.Fatalf("%s groups by (%s), which no longer includes the "+
						"pinned column %s; ribWalkPinned is stale, or this "+
						"statement's granularity just changed",
						tc.name, strings.Join(remainder, ", "), col)
				}
			}

			// Subtract the declared exemptions, refusing any that no longer
			// names a grouped column. That is what keeps this comparison
			// exact: an exemption cannot survive the column it excuses.
			for _, ex := range tc.exempt {
				if ex.why == "" {
					t.Fatalf("the exemption for %s carries no argument; an "+
						"exemption without one is the silent pass this test "+
						"exists to prevent", ex.col)
				}
				var ok bool
				if remainder, ok = removeCol(remainder, ex.col); !ok {
					t.Fatalf("%s no longer groups by %s, but an exemption for it is "+
						"still declared here. If the column is gone for good, delete "+
						"the exemption; if it moved into the key, delete it too. The "+
						"argument it carried was:\n%s", tc.name, ex.col, ex.why)
				}
			}

			got := make([]string, len(tc.key))
			for i, k := range tc.key {
				got[i] = k.col
			}
			if slices.Equal(got, remainder) {
				return
			}

			// Two different failures, reported differently, because they need
			// different fixes. A set difference is a real tie or a real
			// over-long cursor; a pure reorder is neither, but the two lists
			// are meant to read column for column and a silent drift between
			// them is how the next set difference gets missed.
			if slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(remainder))) {
				t.Errorf("%s's page key is (%s) but its GROUP BY's unpinned remainder "+
					"is (%s) -- the same columns in a different order. Order matters "+
					"to the key (it is what ribOrder walks by), so mirror the GROUP "+
					"BY's order into the key or the key's into the GROUP BY rather "+
					"than leaving the two to be read side by side and trusted",
					tc.name, strings.Join(got, ", "), strings.Join(remainder, ", "))
				return
			}
			t.Errorf("%s's page key is (%s) but its GROUP BY's unpinned remainder is "+
				"(%s).\nA key SHORTER than the remainder ties every pair of rows "+
				"differing only in the missing column, and a keyset walk straddling "+
				"that tie drops one of them with no page looking wrong -- the defect "+
				"rib was. A key LONGER than it orders by a column the statement does "+
				"not group by. Either add the column to the key, or, if it genuinely "+
				"breaks no tie, declare a ribKeyExemption carrying the argument for "+
				"why", tc.name, strings.Join(got, ", "), strings.Join(remainder, ", "))
		})
	}
}

// renderRIBWalk renders one family's page-one statement, so a test can read
// what ribStatement actually pins rather than what rib.go says it does. The
// arguments are the shared RIB fixture's, but nothing here depends on the
// fixture existing in a database: this is text.
func renderRIBWalk[T any](t *testing.T, spec ribSpec[T]) string {
	t.Helper()
	sql, _, err := ribStatement(testDB, spec,
		netip.MustParseAddr(ribFixtureRouterIP), netip.MustParseAddr(ribFixturePeerIP),
		ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 7)
	if err != nil {
		t.Fatalf("ribStatement(%s): %v", spec.what, err)
	}
	return sql
}

// renderRIBKeysWalk is renderRIBWalk for phase one of a two-phase walk. It
// fails rather than skipping if the spec has no keysBody: a caller asking for
// a keys statement from a single-statement family is asking about a statement
// that does not exist, and the rendering would silently be the ORDER BY tail
// alone.
func renderRIBKeysWalk[T any](t *testing.T, spec ribSpec[T]) string {
	t.Helper()
	if spec.keysBody == "" {
		t.Fatalf("the %s spec has no keysBody, so it has no phase-one statement "+
			"to render", spec.what)
	}
	sql, _, err := ribKeysStatement(testDB, spec,
		netip.MustParseAddr(ribFixtureRouterIP), netip.MustParseAddr(ribFixturePeerIP),
		ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 7)
	if err != nil {
		t.Fatalf("ribKeysStatement(%s): %v", spec.what, err)
	}
	return sql
}

// ribWhereClause returns a rendered walk statement from its outer WHERE
// onwards, which is the only part of it where a pin can be looked for. The
// whole statement will not do: every route statement joins `ON r.collector_id
// = cur.collector_id`, so searching it for "r.collector_id = " finds the join
// and reports a pin that ribStatement may well have stopped rendering.
func ribWhereClause(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "\nWHERE " + servedGate
	_, where, ok := strings.Cut(sql, anchor)
	if !ok {
		t.Fatalf("the rendered walk has no %q, so this test cannot tell the "+
			"outer WHERE's predicates from the join conditions above it:\n%s",
			strings.TrimPrefix(anchor, "\n"), sql)
	}
	return where
}

// outerGroupBy returns the columns of the GROUP BY belonging to the query that
// reads the route table, never one belonging to a CTE above it.
//
// from is the anchor and it has to be the outer query's own FROM: all three
// statements prepend peerStateCTE, peerUpCTE and eorCTE, whose four GROUP BYs
// sit before it and would satisfy any looser match. Everything after the outer
// FROM is the outer query, so this cuts there and then insists on finding
// exactly one GROUP BY and a HAVING to close it -- a subquery introduced below
// the FROM would bring a second, and this fails rather than picking one.
func outerGroupBy(t *testing.T, body, from string) []string {
	t.Helper()
	if n := strings.Count(body, from); n != 1 {
		t.Fatalf("the anchor %q appears %d times, want exactly 1; this test "+
			"cannot locate the outer query's own FROM and would otherwise read a "+
			"CTE's GROUP BY:\n%s", from, n, body)
	}
	_, outer, _ := strings.Cut(body, from)
	if n := strings.Count(outer, "\nGROUP BY "); n != 1 {
		t.Fatalf("%d GROUP BY clauses follow the outer FROM, want exactly 1; "+
			"with more than one there is no telling which is the route key's:\n%s",
			n, outer)
	}
	_, tail, _ := strings.Cut(outer, "\nGROUP BY ")
	cols, _, ok := strings.Cut(tail, "\nHAVING ")
	if !ok {
		t.Fatalf("the outer GROUP BY is not closed by a HAVING, so this test "+
			"cannot tell where the column list ends:\n%s", tail)
	}
	var out []string
	for c := range strings.SplitSeq(cols, ",") {
		// TrimSpace, because evpnRoutesSQL's GROUP BY wraps onto a second,
		// indented line.
		if c = strings.TrimSpace(c); c == "" {
			t.Fatalf("the outer GROUP BY has an empty column in it: %q", cols)
		}
		out = append(out, c)
	}
	return out
}

// removeCol deletes the first occurrence of col, reporting whether there was
// one. The report is the point: every caller treats a missing column as a
// failure rather than as a subtraction that happened to do nothing.
func removeCol(cols []string, col string) ([]string, bool) {
	i := slices.Index(cols, col)
	if i < 0 {
		return cols, false
	}
	return slices.Delete(slices.Clone(cols), i, i+1), true
}

// TestRIBSpecKeysMatchTheirColumns holds each family's keyOf against its own
// declared key: the same length, the same order, the same Go types.
//
// The two are separate declarations that have to agree, and nothing else makes
// them. keyOf builds the cursor a later page is validated and bound against,
// so a keyOf that returned (path_id, prefix) where the key declares (prefix,
// path_id) would produce a cursor this package rejects as tampered -- on its
// own second page, for every walk, with the caller told their cursor is
// malformed.
func TestRIBSpecKeysMatchTheirColumns(t *testing.T) {
	t.Run("unicast", func(t *testing.T) { assertSpecKey(t, unicastRIBSpec) })
	t.Run("vpn", func(t *testing.T) { assertSpecKey(t, vpnRIBSpec) })
	t.Run("evpn", func(t *testing.T) { assertSpecKey(t, evpnRIBSpec) })
}

func assertSpecKey[T any](t *testing.T, spec ribSpec[T]) {
	t.Helper()
	var zero T
	got := spec.keyOf(zero)
	if len(got) != len(spec.key) {
		t.Fatalf("keyOf produces %d values, the key declares %d columns",
			len(got), len(spec.key))
	}
	for i, k := range spec.key {
		if have := reflect.TypeOf(got[i]); have != k.want {
			t.Errorf("keyOf's value %d is %v, but %s is declared %v",
				i, have, k.col, k.want)
		}
	}
}

// TestRIBFixtureKeysMatchTheirColumns checks the TEST DATA rather than the
// code: a fixture key literal written with an int where the column is a uint32
// would fail every walk assertion in this file with a message about the query,
// which is the wrong thing to go looking at.
func TestRIBFixtureKeysMatchTheirColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []ribKeyCol
		want [][]any
	}{
		{"unicast", unicastRIBKey, ribFixtureUnicastKeys},
		{"unicast, every rib", unicastRIBKey, ribFixtureUnicastKeysEveryRIB},
		{"vpn", vpnRIBKey, ribFixtureVPNKeys},
		{"vpn, every rib", vpnRIBKey, ribFixtureVPNKeysEveryRIB},
		{"evpn", evpnRIBKey, ribFixtureEVPNKeys},
		{"evpn, every rib", evpnRIBKey, ribFixtureEVPNKeysEveryRIB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, row := range tc.want {
				if len(row) != len(tc.key) {
					t.Fatalf("fixture key %d has %d values, want %d", i, len(row), len(tc.key))
				}
				for j, k := range tc.key {
					if got := reflect.TypeOf(row[j]); got != k.want {
						t.Errorf("fixture key %d value %d is %v, but %s is %v",
							i, j, got, k.col, k.want)
					}
				}
			}
		})
	}
}

func TestRIBPageVPN(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBVPNFixture(t, ctx, q)

	router := netip.MustParseAddr(ribVPNRouterIP)
	peer := netip.MustParseAddr(ribVPNPeerIP)
	page := func(cur *RIBCursor) ([]VPNRoute, *RIBCursor, error) {
		return q.RIBPageVPN(ctx, router, peer, ribFixtureRIB, cur, 2)
	}

	t.Run("a full walk at limit 2 returns every route exactly once", func(t *testing.T) {
		pages, cursors := walkRIB(t, page)
		assertWalkedExactly(t, pages, vpnRIBSpec.keyOf, ribFixtureVPNKeys)
		assertPageSizes(t, pages, 2)
		if len(pages) != 3 {
			t.Errorf("the walk took %d pages, want 3 -- five routes at limit 2", len(pages))
		}
		if cursors[len(cursors)-1] != nil {
			t.Errorf("the final page returned a cursor, want nil")
		}
	})

	t.Run("a walk with rib unset spans every rib, at every limit", func(t *testing.T) {
		// route_vpn's counterpart to TestRIBPageUnicastAcrossEveryRIB -- see
		// that test for why the limits are swept rather than picked.
		want := ribFixtureVPNKeysEveryRIB
		for limit := 1; limit <= len(want); limit++ {
			pages, _ := walkRIB(t, func(cur *RIBCursor) ([]VPNRoute, *RIBCursor, error) {
				return q.RIBPageVPN(ctx, router, peer, "", cur, limit)
			})
			assertWalkedExactly(t, pages, vpnRIBSpec.keyOf, want)
			assertPageSizes(t, pages, limit)
		}
	})

	t.Run("the RD-less route is walked, not skipped", func(t *testing.T) {
		// rd = "" is a REAL value lu4 rows carry (see VPNRoute.RD), and it
		// sorts first under the walk's (rd, prefix, path_id) key -- so a walk
		// that treated the empty RD as "no cursor value yet" would either drop
		// this route or restart at it forever.
		rows, _, err := q.RIBPageVPN(ctx, router, peer, ribFixtureRIB, nil, 1000)
		if err != nil {
			t.Fatalf("single-page walk: %v", err)
		}
		if !slices.ContainsFunc(rows, func(r VPNRoute) bool { return r.RD == ribVPNRDNone }) {
			t.Errorf("the RD-less route is missing from the answer: %+v", rows)
		}
	})

	t.Run("a withdrawn route and another rib's rows stay out", func(t *testing.T) {
		pages, _ := walkRIB(t, page)
		for _, p := range pages {
			for _, r := range p {
				if r.Prefix == ribVPNWithdrawnPrefix {
					t.Errorf("%s reached the walk; its newest observation is a withdrawal",
						ribVPNWithdrawnPrefix)
				}
				if r.Prefix == ribVPNOtherRIBPrefix || r.RIB != ribFixtureRIB {
					t.Errorf("a row from rib %q reached a walk of %q: %+v",
						r.RIB, ribFixtureRIB, r)
				}
			}
		}
	})
}

func TestRIBPageEVPN(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBEVPNFixture(t, ctx, q)

	router := netip.MustParseAddr(ribEVPNRouterIP)
	peer := netip.MustParseAddr(ribEVPNPeerIP)
	page := func(cur *RIBCursor) ([]EVPNRoute, *RIBCursor, error) {
		return q.RIBPageEVPN(ctx, router, peer, ribFixtureRIB, cur, 2)
	}

	t.Run("a full walk at limit 2 returns every NLRI exactly once", func(t *testing.T) {
		pages, cursors := walkRIB(t, page)
		assertWalkedExactly(t, pages, evpnRIBSpec.keyOf, ribFixtureEVPNKeys)
		assertPageSizes(t, pages, 2)
		if len(pages) != 3 {
			t.Errorf("the walk took %d pages, want 3 -- five NLRIs at limit 2", len(pages))
		}
		if cursors[len(cursors)-1] != nil {
			t.Errorf("the final page returned a cursor, want nil")
		}
	})

	t.Run("a walk with rib unset spans every rib, at every limit", func(t *testing.T) {
		// route_evpn's counterpart to TestRIBPageUnicastAcrossEveryRIB. The
		// in_post row here is a byte-for-byte copy of the first NLRI, so even
		// an eight-column key is one column short of telling them apart.
		want := ribFixtureEVPNKeysEveryRIB
		for limit := 1; limit <= len(want); limit++ {
			pages, _ := walkRIB(t, func(cur *RIBCursor) ([]EVPNRoute, *RIBCursor, error) {
				return q.RIBPageEVPN(ctx, router, peer, "", cur, limit)
			})
			assertWalkedExactly(t, pages, evpnRIBSpec.keyOf, want)
			assertPageSizes(t, pages, limit)
		}
	})

	t.Run("routes that differ only in one NLRI component stay apart", func(t *testing.T) {
		// The fixture's first three keys differ only in ip and only in esi. A
		// key missing either column ties two of them together, and a tie in a
		// keyset walk is a route dropped the moment it straddles a page
		// boundary -- which, at limit 2 over five rows, it does.
		rows, _, err := q.RIBPageEVPN(ctx, router, peer, ribFixtureRIB, nil, 1000)
		if err != nil {
			t.Fatalf("single-page walk: %v", err)
		}
		n := 0
		for _, r := range rows {
			if r.RouteType == 2 && r.MAC == ribEVPNMac {
				n++
			}
		}
		if n != 3 {
			t.Errorf("got %d type-2 routes under one MAC, want 3 -- they differ only "+
				"in ip and in esi, and both are part of the NLRI key: %+v", n, rows)
		}
	})

	t.Run("a withdrawn NLRI and another rib's rows stay out", func(t *testing.T) {
		pages, _ := walkRIB(t, page)
		for _, p := range pages {
			for _, r := range p {
				if r.MAC == ribEVPNWithdrawnMac {
					t.Errorf("the withdrawn NLRI reached the walk: %+v", r)
				}
				if r.RIB != ribFixtureRIB {
					t.Errorf("a row from rib %q reached a walk of %q: %+v",
						r.RIB, ribFixtureRIB, r)
				}
			}
		}
	})
}

// TestRIBPageRefusesACursorFromADifferentWalk covers the exposure a cursor has
// that no HTTP layer can close for it: api/openapi.yaml puts router, peer, rib
// and cursor on one path with no stated constraint between them, so a
// perfectly conforming client can change any of the three on page 2. A cursor
// is a POSITION, and a position only means something inside the walk that
// produced it -- reinterpreted against a different scope it does not error, it
// answers, and the answer is wrong in a way the caller cannot see.
//
// Each case below is asserted against what it would silently do if the cursor
// were accepted, which is why the subtest names say the outcome rather than
// the input.
func TestRIBPageRefusesACursorFromADifferentWalk(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBWalkFixture(t, ctx, q)

	router := netip.MustParseAddr(ribFixtureRouterIP)
	peer := netip.MustParseAddr(ribFixturePeerIP)

	// Page one of a real, rib-scoped walk: every case below reuses this
	// cursor, so none of them can pass because the cursor was malformed.
	_, cur, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, nil, 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if cur == nil {
		t.Fatal("page 1 returned no cursor; this test needs a walk with a page 2")
	}
	if cur.Router != router || cur.Peer != peer || cur.RIB != ribFixtureRIB {
		t.Fatalf("page 1's cursor does not carry the walk's scope: %+v -- without it "+
			"nothing below can be detected at all", cur)
	}

	for _, tc := range []struct {
		name         string
		router, peer netip.Addr
		rib          string
	}{
		// Changing the rib leaves the position a rib boundary away from where
		// the new scope starts: an empty page and a nil cursor, which reads as
		// a walk that finished.
		{"a rib changed mid-walk", router, peer, ribFixtureOtherRIB},
		// Dropping it silently widens the walk to every rib from this position
		// onward, skipping everything that sorts before it in the ribs that
		// were never being walked.
		{"a rib dropped mid-walk", router, peer, ""},
		// Changing the peer pages another peer's table from this peer's last
		// key. peer is pinned by the statement but appears nowhere in the key,
		// so nothing else catches it.
		{"a peer changed mid-walk", router, netip.MustParseAddr(ribFixtureOtherPeer), ribFixtureRIB},
		// Changing the router is only ACCIDENTALLY caught by the session pin,
		// and not here: ribFixtureOtherRouterIP shares the walked router's
		// session_id on purpose (see its own doc comment), so the pin agrees
		// and the walk would proceed against the wrong table.
		{"a router changed mid-walk", netip.MustParseAddr(ribFixtureOtherRouterIP), peer, ribFixtureRIB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, next, err := q.RIBPageUnicast(ctx, tc.router, tc.peer, tc.rib, cur, 2)
			if !errors.Is(err, ErrBadFilter) {
				t.Fatalf("err = %v, want ErrBadFilter -- a cursor reinterpreted against "+
					"a different scope answers rather than failing, and the answer is "+
					"wrong in a way the caller cannot see", err)
			}
			if len(rows) != 0 || next != nil {
				t.Errorf("got %d rows and cursor %+v alongside the error", len(rows), next)
			}
		})
	}

	// The same cursor, unchanged, still walks -- so none of the refusals above
	// is passing because page 2 was broken to begin with.
	if _, _, err := q.RIBPageUnicast(ctx, router, peer, ribFixtureRIB, cur, 2); err != nil {
		t.Fatalf("the unmodified cursor was refused: %v", err)
	}
}

// TestRIBPageRouterChangeIsNotCaughtByTheSessionPinAlone is the evidence for
// the claim above that the router case needed a check of its own.
//
// The session pin catches a changed router only when the two routers resolve
// different sessions, which is not something anything guarantees: session_id
// is now().UnixNano() assigned by each collector from its own clock (see
// peerStateCTE), two routers under one collector can hold the same value, and
// insertRIBWalkFixture writes exactly that pair. If this ever starts failing
// because the two sessions differ, the fixture has drifted and the router
// check has quietly become untested rather than unnecessary.
func TestRIBPageRouterChangeIsNotCaughtByTheSessionPinAlone(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBWalkFixture(t, ctx, q)

	walked, _, err := q.ribPin(ctx, netip.MustParseAddr(ribFixtureRouterIP))
	if err != nil {
		t.Fatalf("ribPin(walked router): %v", err)
	}
	_, sidA, err := q.ribPin(ctx, netip.MustParseAddr(ribFixtureRouterIP))
	if err != nil {
		t.Fatalf("ribPin(walked router): %v", err)
	}
	other, sidB, err := q.ribPin(ctx, netip.MustParseAddr(ribFixtureOtherRouterIP))
	if err != nil {
		t.Fatalf("ribPin(other router): %v", err)
	}
	if walked != other || sidA != sidB {
		t.Fatalf("the two routers resolve different pins ((%q, %d) and (%q, %d)); "+
			"the session check would catch a swapped router by accident and "+
			"TestRIBPageRefusesACursorFromADifferentWalk's router case is no longer "+
			"proving that the explicit check is what catches it",
			walked, sidA, other, sidB)
	}
}

// TestRIBMembersMatchTheSchemaEnum pins ribMembers to the DDL the tests
// actually apply, by reading the enum's own definition back out of ClickHouse.
//
// ribMembers is a fourth copy of a list that also lives in
// deploy/clickhouse/schema.sql, api/openapi.yaml and sink's ribName, and
// nothing in Go can be derived from (see its own doc comment). A copy that
// drifts is not a loud failure here: an extra member would make this package
// reject a cursor the database would have accepted, and a MISSING one would
// let a cursor through to raise UNKNOWN_ELEMENT_OF_ENUM -- the exact 500 the
// domain check exists to prevent. Reading the type back from the server ties
// the copy to the column that will really accept or reject the value.
//
// All six route and link-state tables are checked, not one: they carry six
// separate Enum8 declarations in schema.sql (identical to the same enum on
// the other four rib-bearing tables this package does not query by rib= --
// see schema.sql itself), and a migration that widened one and forgot
// another is precisely the drift this catches. ls_nodes, ls_links and
// ls_prefixes lean on ribMembers exactly as the three route tables do: their
// own filters validate rib= through lsRibColumn, ribColumn's link-state
// counterpart, against the same ribMembers domain.
func TestRIBMembersMatchTheSchemaEnum(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	member := regexp.MustCompile(`'([^']*)' = -?\d+`)
	for _, table := range []string{
		"route_unicast", "route_vpn", "route_evpn",
		"ls_nodes", "ls_links", "ls_prefixes",
	} {
		t.Run(table, func(t *testing.T) {
			var typ string
			if err := q.conn.QueryRow(ctx,
				"SELECT type FROM system.columns WHERE database = ? AND table = ? AND name = 'rib'",
				q.db, table).Scan(&typ); err != nil {
				t.Fatalf("read %s.rib's type: %v", table, err)
			}
			if !strings.HasPrefix(typ, "Enum8(") {
				t.Fatalf("%s.rib is %q, not an Enum8 -- ribColumn's domain check, and "+
					"the whole reason it exists, assume otherwise", table, typ)
			}
			got := map[string]bool{}
			for _, m := range member.FindAllStringSubmatch(typ, -1) {
				got[m[1]] = true
			}
			if !maps.Equal(got, ribMembers) {
				t.Errorf("%s.rib declares %v, ribMembers says %v",
					table, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(ribMembers)))
			}
		})
	}
}

// TestSeqAndStreamSeqAgreeWithinASession pins the property that lets
// rib_unicast_current use stream_seq as its ReplacingMergeTree version while
// every reader argMaxes on (seq, stream_seq).
//
// The two are different clocks. seq is the collector's per-(peer, rib-view)
// counter and restarts at zero when a session starts; stream_seq is
// JetStream's own sequence, monotonic for the life of the stream. The
// projection collapses on stream_seq -- latest publish wins -- and the reader
// resolves on (seq, stream_seq) inside a pinned session. If those two ever
// picked different rows for one route key, the projection would keep an
// observation the reader would not have chosen and the answers would diverge
// silently, in the direction of serving a stale attribute set for a live
// route.
//
// They cannot disagree within a session because one peer's publishes are
// ordered, so seq and stream_seq advance together. Measured on the live
// archive: 0 disagreements over 3,895 (key, session) groups. ACROSS sessions
// they disagree constantly -- 704 of 1,196 keys -- which is not a defect but
// the restart, and is why every comparison here is session-scoped.
func TestSeqAndStreamSeqAgreeWithinASession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFixture(t, ctx, q)
	insertRIBWalkFixture(t, ctx, q)

	var disagreeing, groups uint64
	err := q.conn.QueryRow(ctx, `
SELECT countIf(by_seq != by_stream), count() FROM (
  SELECT argMax(stream_seq, (seq, stream_seq)) AS by_seq, max(stream_seq) AS by_stream
  FROM `+q.db+`.route_unicast
  GROUP BY collector_id, router_ip, peer_ip, rib, session_id, prefix, path_id
)`).Scan(&disagreeing, &groups)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if groups == 0 {
		t.Fatal("no (route key, session) groups, so this assertion is vacuous")
	}
	if disagreeing != 0 {
		t.Errorf("%d of %d (route key, session) groups resolve differently under "+
			"(seq, stream_seq) than under stream_seq alone. rib_unicast_current "+
			"versions on stream_seq and every reader resolves on (seq, "+
			"stream_seq); if those disagree the projection keeps a row the "+
			"reader would not have picked", disagreeing, groups)
	}
}

// TestPublishOrderNeverInvertsSessionOrder pins the assumption
// rib_unicast_current's KEY rests on: that a route's latest observation by
// publish order belongs to its newest session.
//
// The projection is keyed without session_id, so each route key holds whichever
// session last advertised it and the reader's session_id = cur.sid filter
// excludes the rest. That is 7.2x smaller than keying on session_id and gives
// the identical answer -- but only while publish order tracks session order. If
// a superseded session's row for a route could land after the current
// session's, the projection would hold the dead observation, the filter would
// reject it, and the route would vanish from a dump it is really in. A route
// missing from a RIB walk is the worst shape of wrong this table can produce:
// silent, and indistinguishable from a withdrawal.
//
// Probed across the live archive before the design was committed to: zero
// disagreements over 1,196 route keys, 25 peer views, and every link-state and
// peer_events table. This is the permanent form. If it fails, session_id goes
// back into the ORDER BY and the projection stops being cheap.
func TestPublishOrderNeverInvertsSessionOrder(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteFixture(t, ctx, q)
	insertViewLostFixture(t, ctx, q)
	insertRIBWalkFixture(t, ctx, q)

	// route_unicast only, deliberately. peer_events is NOT checked here and
	// must not be: this package's fixtures construct inverted peer_events on
	// purpose -- insertPeerWhoseOldSessionOutlivesItInSeq exists to prove the
	// session join is load-bearing, and four of the package's peer views
	// invert publish order against session order because a test asked them
	// to. Asserting over them would be asserting that the fixtures are not
	// adversarial.
	//
	// It is also the wrong table for this question. The projection is built
	// from route_unicast, and `cur` resolves the session with max(session_id),
	// which does not read publish order at all. What has to hold is that a
	// ROUTE's latest-published observation belongs to its newest session.
	var disagreeing, views uint64
	err := q.conn.QueryRow(ctx, `
SELECT countIf(by_publish != newest), count() FROM (
  SELECT argMax(session_id, stream_seq) AS by_publish, max(session_id) AS newest
  FROM `+q.db+`.route_unicast
  GROUP BY collector_id, router_ip, peer_ip
)`).Scan(&disagreeing, &views)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if views == 0 {
		t.Fatal("no peer views in route_unicast, so this assertion is vacuous")
	}
	if disagreeing != 0 {
		t.Errorf("%d of %d peer views have a latest-published route row from a "+
			"session that is not the newest. rib_unicast_current is keyed "+
			"without session_id on the premise that this cannot happen; if it "+
			"can, the projection drops routes that are really in the current "+
			"dump", disagreeing, views)
	}
}

// TestRIBPageUnicastTwoPhaseMatchesOneStatement is the parity harness the
// two-phase reader is not allowed to ship without, and it is written before
// the reader because of what a 2026-09-05 measurement found: the two ways
// to get a second reader wrong -- resolving current state differently,
// and scoping it differently -- are both invisible in a page that merely
// looks well-formed.
//
// It compares the two readers ROW FOR ROW, over a fixture where every route is
// observed more than once (see insertRIBParityFixture), at several page sizes
// so the phase boundary lands in different places, and across every rib so the
// page key's rib component is exercised.
//
// The absolute assertions below it are not redundant with the parity check:
// parity alone would pass if both readers were wrong in the same way, and
// phase 2 is deliberately routesSQL itself, so a shared bug is the failure
// mode most available to this design.
func TestRIBPageUnicastTwoPhaseMatchesOneStatement(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBParityFixture(t, ctx, q)

	router := netip.MustParseAddr(ribParityRouterIP)
	peer := netip.MustParseAddr(ribParityPeerIP)

	// Every page size from 1 to one past the answer's length: the phase
	// boundary moves with it, and a two-phase reader that mis-slices its own
	// page has nowhere to hide across the whole range.
	for _, limit := range []int{1, 2, 3, 4, 5, 6, 7} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			twoPhase, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, router, peer, "", cur, limit)
			})
			// The reference reader is the shipped one, reached by clearing the
			// field that selects the two-phase fetch -- not a copy of it, and
			// not a test-only entry point, so the thing being compared against
			// cannot drift from the thing VPN and EVPN still run.
			oneStatementSpec := unicastRIBSpec
			oneStatementSpec.keysBody = ""
			oneStatement, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
				return ribPage(ctx, q, oneStatementSpec, router, peer, "", cur, limit)
			})

			flat := func(pages [][]Route) []Route {
				var out []Route
				for _, p := range pages {
					out = append(out, p...)
				}
				return out
			}
			got, want := flat(twoPhase), flat(oneStatement)
			if len(got) != len(want) {
				t.Fatalf("the two-phase walk returned %d routes, the one-statement walk %d",
					len(got), len(want))
			}
			// Against the fixture's own declared answer, in order -- not just
			// its length. A length check is what let ribParityUnicastKeys sit
			// in the wrong order (in_post first, when rib is an Enum8 whose
			// ORDER BY puts in_pre first) without any test noticing.
			gotKeys := make([]string, len(want))
			for i, r := range want {
				gotKeys[i] = ribKeyText(unicastRIBSpec.keyOf(r))
			}
			wantKeys := make([]string, len(ribParityUnicastKeys))
			for i, k := range ribParityUnicastKeys {
				wantKeys[i] = ribKeyText(k)
			}
			if !slices.Equal(gotKeys, wantKeys) {
				t.Fatalf("the one-statement walk returned\n %v\nwant\n %v\n-- the "+
					"fixture, not the reader, is what this test just failed on",
					gotKeys, wantKeys)
			}
			for i := range got {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Errorf("route %d differs between the readers:\n two-phase: %+v\n one-stmt:  %+v",
						i, got[i], want[i])
				}
			}
		})
	}

	t.Run("the straggler reports its live session, not the superseded one", func(t *testing.T) {
		// The defect that killed the projection, asserted in absolute terms:
		// the superseded session's row for this key carries the HIGHER
		// stream_seq, so a reader that ranks on stream_seq without the session
		// scope reports 10.6.6.6, and one that drops the route entirely
		// returns five routes instead of six.
		pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, "", cur, 2)
		})
		var found *Route
		for _, p := range pages {
			for i, r := range p {
				if r.Prefix == ribParityPrefixStraggler {
					found = &p[i]
				}
			}
		}
		if found == nil {
			t.Fatalf("%s is missing from the walk entirely -- a superseded session's "+
				"late row won the route", ribParityPrefixStraggler)
		}
		if got := found.NextHop.String(); got != "10.9.9.9" {
			t.Errorf("%s reports next hop %s, want 10.9.9.9 -- %s is the superseded "+
				"session's observation, which carries the higher stream_seq",
				ribParityPrefixStraggler, got, got)
		}
	})

	t.Run("a re-advertised route reports its newest attributes", func(t *testing.T) {
		pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, ribParityRIB, cur, 3)
		})
		for _, p := range pages {
			for _, r := range p {
				switch {
				case r.Prefix == ribParityAddPathPrefix && r.PathID == 0:
					if got := r.NextHop.String(); got != "10.9.9.2" {
						t.Errorf("%s path 0 reports next hop %s, want 10.9.9.2", r.Prefix, got)
					}
					if r.MED == nil || *r.MED != 20 {
						t.Errorf("%s path 0 reports MED %v, want 20", r.Prefix, r.MED)
					}
				case r.Prefix == ribParityPrefixTwo:
					// The cleared MED: absent, not the 50 it used to carry.
					if r.MED != nil {
						t.Errorf("%s reports MED %d, want absent -- it was cleared on "+
							"re-advertisement, and argMax over a Nullable column skips "+
							"NULL unless the argument is wrapped in a tuple",
							r.Prefix, *r.MED)
					}
				}
			}
		}
	})
}

// TestRIBPageUnicastOnADownPeerIsAnEmptyWalk isolates the peer_up gate, which
// nothing in this file did before it.
//
// TestRIBPageUnicast names the gate in its list of predicates -- "its peer is
// down as of this session" -- but the row it looks for belongs to ANOTHER
// peer, and r.peer_ip excludes that row on its own. Delete the gate from
// routesSQL outright and that assertion still passes, which makes it a test of
// the peer pin wearing the gate's name.
//
// Here the walked peer IS the down one, and its route is live by every other
// measure: the current session, a rib the walk spans, never withdrawn,
// advertised once so no newer observation can be hiding it. peer_up.state =
// 'up' is the only predicate between that row and the page.
//
// Both readers are held to the same answer, because a down peer is where they
// are likeliest to disagree -- the gate is the one narrowing phase one carries
// for a reason other than the page key.
func TestRIBPageUnicastOnADownPeerIsAnEmptyWalk(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBParityFixture(t, ctx, q)

	router := netip.MustParseAddr(ribParityRouterIP)
	peer := netip.MustParseAddr(ribParityDownPeer)

	oneStatementSpec := unicastRIBSpec
	oneStatementSpec.keysBody = ""

	for _, tc := range []struct {
		name string
		page func(cur *RIBCursor) ([]Route, *RIBCursor, error)
	}{
		{"two-phase", func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, "", cur, 10)
		}},
		{"one statement", func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return ribPage(ctx, q, oneStatementSpec, router, peer, "", cur, 10)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages, cursors := walkRIB(t, tc.page)
			for _, p := range pages {
				for _, r := range p {
					t.Errorf("%s reached a walk of a peer that is DOWN as of the current "+
						"session; the peer_up gate is the only thing that excludes it, "+
						"since the row is live by every other measure", r.Prefix)
				}
			}
			if len(pages) != 1 || cursors[0] != nil {
				t.Errorf("the walk took %d pages and page one returned cursor %+v; a peer "+
					"with no current routes is one empty page and a nil cursor",
					len(pages), cursors[0])
			}
		})
	}
}

// TestRIBUnicastKeysPhaseMatchesTheAttributesPhase compares the two-phase
// reader's two statements to EACH OTHER, page by page: the keys phase one
// resolves must be exactly the keys phase two accepts, in the same order.
//
// TestRIBPageUnicastTwoPhaseMatchesOneStatement cannot see this, and the
// reason is ribPage's probe row. A page shorter than limit+1 is how ribPage
// knows a walk has ended, so a phase one that admits one key phase two rejects
// does not produce a wrong row -- it produces a page one row short, and ribPage
// believes it. Every route after that point is silently gone, and every row
// that did come back is correct, which is exactly the failure a test that only
// inspects returned rows cannot have an opinion about.
//
// That divergence is not constructible through the public walk today, and the
// reason is a property of the current scoping rather than of the design: the
// only narrowing phase one carries that phase two does not also apply is the
// peer_up gate; the gate resolves per (collector, router, peer, session); and
// a walk is pinned to one peer. So a page cannot mix an up peer's routes with
// a down peer's -- against an up peer the gate never fires, and against a down
// one phase two rejects every key phase one offers, leaving an empty page
// either way. Change either half (a walk spanning peers, or a gate that
// resolved per rib) and the mixture becomes buildable. Comparing the
// statements directly holds the invariant at the level it actually lives at,
// rather than at the level where it currently happens to be unobservable.
func TestRIBUnicastKeysPhaseMatchesTheAttributesPhase(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBParityFixture(t, ctx, q)

	router := netip.MustParseAddr(ribParityRouterIP)

	for _, tc := range []struct {
		name, peer, rib string
	}{
		{"every rib of the walked peer", ribParityPeerIP, ""},
		{"one rib of the walked peer", ribParityPeerIP, ribParityRIB},
		// The down peer, where the gate is the whole difference between the
		// phases: phase two accepts none of its routes, so phase one must
		// resolve no keys at all. Dropping `WHERE peer_up.state = 'up'` from
		// unicastRIBKeysSQL fails here and nowhere else.
		{"a down peer", ribParityDownPeer, ""},
		// A peer with no peer_events row of its own in the current session.
		// The gate is an INNER JOIN as well as a state test, and this is the
		// half of it a state test alone would not cover.
		{"a peer with no session of its own", ribParityStrangerPeer, ""},
	} {
		for _, limit := range []int{1, 2, 7} {
			t.Run(fmt.Sprintf("%s, limit %d", tc.name, limit), func(t *testing.T) {
				peer := netip.MustParseAddr(tc.peer)
				var last []any
				for page := 1; ; page++ {
					// Phase two's own statement, run as the single-statement
					// reader runs it: limit+1 rows, probe row included, so the
					// comparison covers the row ribPage reads as "there is
					// another page" rather than stopping one short of it.
					stmt, args, err := ribStatement(q.db, unicastRIBSpec, router, peer,
						tc.rib, defaultFixtureCollector, ribParityCurSess, last, limit)
					if err != nil {
						t.Fatalf("page %d, attributes statement: %v", page, err)
					}
					rows, err := ribQuery(ctx, q, unicastRIBSpec, stmt, args)
					if err != nil {
						t.Fatalf("page %d, attributes query: %v", page, err)
					}
					want := make([][]any, len(rows))
					for i, r := range rows {
						want[i] = unicastRIBSpec.keyOf(r)
					}

					keysStmt, keysArgs, err := ribKeysStatement(q.db, unicastRIBSpec, router,
						peer, tc.rib, defaultFixtureCollector, ribParityCurSess, last, limit)
					if err != nil {
						t.Fatalf("page %d, keys statement: %v", page, err)
					}
					got, err := ribScanKeys(ctx, q, unicastRIBSpec, keysStmt, keysArgs)
					if err != nil {
						t.Fatalf("page %d, keys query: %v", page, err)
					}

					if len(got) != len(want) {
						t.Fatalf("page %d: phase one resolved %d keys, phase two accepted "+
							"%d rows (%v against %v). A phase one that is LOOSER truncates "+
							"the walk -- ribPage reads the short page phase two returns as "+
							"the end of it; a phase one that is TIGHTER loses those routes "+
							"outright",
							page, len(got), len(want), ribKeyTexts(got), ribKeyTexts(want))
					}
					for i := range got {
						if !reflect.DeepEqual(got[i], want[i]) {
							t.Errorf("page %d, row %d: phase one resolved %v, phase two %v -- "+
								"the phases agree on which routes exist but not on their "+
								"order, so the cursor phase one hands phase two bounds the "+
								"wrong span", page, i, got[i], want[i])
						}
					}

					if len(rows) <= clampRIBLimit(limit) {
						return
					}
					last = want[clampRIBLimit(limit)-1]
					if page > 100 {
						t.Fatalf("the comparison did not reach the end of the walk in %d pages", page)
					}
				}
			})
		}
	}
}

// ribKeyTexts renders a list of page keys for a failure message.
func ribKeyTexts(keys [][]any) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = ribKeyText(k)
	}
	return out
}

// TestRIBSpecsWithKeysBodyBindNoLead holds the invariant ribKeysStatement
// depends on and cannot check for itself: a family that fetches its pages in
// two phases must have no lead values, because ribKeysStatement does not bind
// them.
//
// The pairing is not hypothetical, which is why this is a test rather than a
// comment. evpnRIBSpec already carries a lead -- the eor join's family token,
// which sits above that statement's outer WHERE -- and EVPN adopts
// two-phase paging later by filling in its own keysBody. If that keysBody
// carried a placeholder of its own, the filter's
// first value would bind to it, because the driver binds strictly left to
// right: a page that is WRONG rather than one that errors.
//
// What is checked is the ARITY, not the presence of spec.lead. A spec may
// legitimately carry a lead for its body -- ribAttrsStatement binds it, and
// phase two IS the body -- so banning the field outright would reject the
// exactly-correct EVPN adoption this comment describes, which
// costs one field. The rendered keys statement having exactly as many
// placeholders as ribKeysStatement has values is the invariant that actually
// makes not binding the lead safe, and it catches any placeholder a keysBody
// grows, lead-shaped or not.
func TestRIBSpecsWithKeysBodyBindNoLead(t *testing.T) {
	t.Run("unicast", func(t *testing.T) { assertKeysBodyBindsNoLead(t, unicastRIBSpec) })
	t.Run("vpn", func(t *testing.T) { assertKeysBodyBindsNoLead(t, vpnRIBSpec) })
	t.Run("evpn", func(t *testing.T) { assertKeysBodyBindsNoLead(t, evpnRIBSpec) })
}

func assertKeysBodyBindsNoLead[T any](t *testing.T, spec ribSpec[T]) {
	t.Helper()
	if spec.keysBody == "" {
		// A single-statement family. ribStatement binds its lead, and there is
		// no second statement to get it wrong -- see ribFetch.
		return
	}
	// The HAVING hazard, which is a different way for the two phases to bind
	// different things. ribPageFilters is shared by every phase and its
	// rendering is spliced into keysBody's own %[3]s -- but a HAVING predicate
	// names the live_* aliases routesSQL's SELECT list defines, and a keysBody
	// selects no attributes and defines none of them. No RIB filter emits one
	// today; the day one does, phase two resolves the alias and phase one
	// raises UNKNOWN_IDENTIFIER, on the fast path only.
	f, err := ribPageFilters(spec, netip.MustParseAddr(ribFixtureRouterIP),
		netip.MustParseAddr(ribFixturePeerIP), ribFixtureRIB,
		defaultFixtureCollector, ribFixtureCurSession, nil)
	if err != nil {
		t.Fatalf("ribPageFilters(%s): %v", spec.what, err)
	}
	if got := f.having(); got != "" {
		t.Fatalf("a %s page's filters now render a HAVING (%q), which ribKeysStatement "+
			"splices into a statement that defines none of the live_* aliases such a "+
			"predicate names. Either give keysBody the aggregates the predicate reads, "+
			"or keep RIB-walk filters on the WHERE side where both phases can serve "+
			"them", spec.what, got)
	}

	sql, args, err := ribKeysStatement(testDB, spec,
		netip.MustParseAddr(ribFixtureRouterIP), netip.MustParseAddr(ribFixturePeerIP),
		ribFixtureRIB, defaultFixtureCollector, ribFixtureCurSession, nil, 7)
	if err != nil {
		t.Fatalf("ribKeysStatement(%s): %v", spec.what, err)
	}
	// router, peer, rib, collector, session, limit. No keyset on page one, and
	// no lead, which is the point.
	assertPlaceholders(t, sql, args, 6)
}

// phaseHookConn fires a side effect the first time a statement matching want
// is about to run, which is what makes a race between the two-phase reader's
// two statements deterministic instead of a thing that happens sometimes.
//
// It wraps the driver rather than adding a seam to ribFetchTwoPhase: the
// production path must be the one under test here, because what is being
// tested is precisely how that path reacts to the world changing underneath
// it.
type phaseHookConn struct {
	driver.Conn
	want  string
	fired bool
	do    func()
}

func (c *phaseHookConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	if !c.fired && strings.Contains(query, c.want) {
		c.fired = true
		c.do()
	}
	return c.Conn.Query(ctx, query, args...)
}

// TestRIBPageUnicastSurvivesAWithdrawalBetweenThePhases is the regression test
// for the failure the two-phase reader shipped with and the single-statement
// reader never had: a walk that ends early and reports every route it did
// return as real.
//
// The probe row is what tells ribPage another page exists. Phase one asks for
// limit+1 keys; if phase two is then asked for the attributes of all limit+1 of
// them, a single route that stops qualifying in between -- a withdrawal, a
// peer going down, a re-advertisement as a withdraw -- comes back as limit rows,
// and ribPage reads that short page as the end of the walk. The rows it hands
// back are all correct, and every route after them is silently gone.
//
// The single-statement reader cannot do this: its LIMIT is applied by the
// database to the live answer, so a withdrawal simply lets the next live route
// take the vacated slot and the page still proves another exists.
//
// The fix is that the PROBE is phase one's business alone. Phase one resolves
// limit+1 keys and decides has-more from that count; phase two is asked only
// about the page's own keys, and the cursor is taken from the key phase one
// resolved rather than from the last row phase two returned. A raced
// withdrawal then costs exactly the row it withdrew -- the smear
// api/openapi.yaml's paginated_smear warning already documents -- instead of
// the rest of the walk.
func TestRIBPageUnicastSurvivesAWithdrawalBetweenThePhases(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRIBParityFixture(t, ctx, q)

	router := netip.MustParseAddr(ribParityRouterIP)
	peer := netip.MustParseAddr(ribParityPeerIP)

	// The first key of the every-rib walk, withdrawn once, just before phase
	// two of the first page runs. `argMax(r.med` appears only in routesSQL --
	// phase two -- and never in unicastRIBKeysSQL, which selects no attributes.
	withdrawn := ribParityUnicastKeys[0]
	hooked := &phaseHookConn{Conn: q.conn, want: "argMax(tuple(r.med)", do: func() {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			RouterIP: ribParityRouterIP, RouterSysname: ribParitySysname,
			PeerIP: ribParityPeerIP, RIB: withdrawn[0].(string),
			Prefix: withdrawn[1].(string), PathID: withdrawn[2].(uint32),
			PeerASN: 65000, PeerBGPID: ribParityPeerIP,
			SessionID: ribParityCurSess, Seq: 900, StreamSeq: 900,
			IsWithdraw: 1, NextHop: "10.9.9.1",
			TsRouter: time.Now().UTC(), TsCollector: time.Now().UTC(),
		})
	}}
	raced, err := New(hooked, q.db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
		return raced.RIBPageUnicast(ctx, router, peer, "", cur, 2)
	})
	if !hooked.fired {
		t.Fatalf("the hook never fired, so no withdrawal raced the page and this "+
			"test proved nothing -- phase two's statement no longer contains %q",
			hooked.want)
	}

	var got []string
	for _, p := range pages {
		for _, r := range p {
			got = append(got, ribKeyText(unicastRIBSpec.keyOf(r)))
		}
	}
	// Every route the fixture holds except the one withdrawn mid-page. The
	// withdrawn route may or may not appear depending on which side of the
	// race phase two landed on -- that is the documented smear -- but the
	// FIVE that were never touched are not optional.
	var want []string
	for _, k := range ribParityUnicastKeys[1:] {
		want = append(want, ribKeyText(k))
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s is missing from a walk that raced a withdrawal on another "+
				"route. A page shortened by the race is being read as the end of the "+
				"walk, which loses every route after it while every row returned is "+
				"real.\nwalked %d routes over %d pages: %v",
				w, len(got), len(pages), got)
		}
	}
}
