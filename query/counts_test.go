package query

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
)

// TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent pins a hazard:
// under it, count() has been observed returning 37,000 against a true
// 32,000, no longer reproducible against a real deployment's archive,
// because ReplacingMergeTree merged the injected duplicates away in the
// background sometime after the measurement was taken. The hazard itself (a
// redelivered envelope leaves two rows until the next merge) is still real;
// only the archive's evidence of it decayed. This test manufactures the
// hazard directly instead of relying on the archive to still exhibit it, and
// defeats the merge that would otherwise erase it before the assertion runs.
//
// The raw == distinct guard below is the point of the test, not ceremony
// around it: without it, a merge winning the race between insertion and
// query silently turns this into count(dup) == count(dup) == 100, which
// passes whether or not Peers is correct. That is exactly how the
// headline measurement above stopped meaning anything -- see this test's own
// name and the package doc comment on why the query package exists at all.
// A "precondition failed" result here is this test failing to do its job,
// not this test passing.
func TestRouteCountsAreCorrectWithUnmergedDuplicatesPresent(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	// Insert the same 100 route rows twice, byte-identical, as a
	// redelivered envelope would arrive. ReplacingMergeTree will
	// eventually collapse them -- so assert the precondition rather than
	// assuming it, and disable the merge that would otherwise make this
	// test pass for the wrong reason.
	if err := q.conn.Exec(ctx, fmt.Sprintf("SYSTEM STOP MERGES %s.route_unicast", q.db)); err != nil {
		t.Fatalf("stop merges: %v", err)
	}
	// context.WithoutCancel: t's own context is already done by the time
	// Cleanup runs, and START MERGES must still reach the server even
	// then. A cleanup that silently no-ops because ctx was canceled would
	// leave route_unicast's merges off for every test and every developer
	// that runs after this one, with nothing in the output to say so.
	t.Cleanup(func() {
		_ = q.conn.Exec(context.WithoutCancel(ctx),
			fmt.Sprintf("SYSTEM START MERGES %s.route_unicast", q.db))
	})

	insertDuplicateRouteFixture(t, ctx, q) // 100 distinct routes, inserted twice

	// Scoped to duplicateRouteFixtureRouterIP, not the whole table: this
	// precondition is load-bearing (see the doc comment above), and an
	// unscoped count() != uniqExact() only happens to hold today because
	// counts_test.go sorts first alphabetically and runs before any other
	// test's fixture writes its own unmerged duplicates into route_unicast.
	// Reorder the tests -- a new a*_test.go, a -run filter, -shuffle -- and
	// an unscoped guard would start passing on someone else's duplicates
	// while proving nothing about this fixture's own raw/distinct counts.
	var raw, distinct uint64
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(), uniqExact((router_ip, peer_ip, rib, prefix, path_id))
		 FROM %s.route_unicast WHERE router_ip = ?`, q.db),
		duplicateRouteFixtureRouterIP).Scan(&raw, &distinct); err != nil {
		t.Fatal(err)
	}
	if raw == distinct {
		t.Fatalf("precondition failed: count()=%d equals uniqExact()=%d, so the "+
			"duplicates were merged away and this test proves nothing", raw, distinct)
	}

	peers, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(duplicateRouteFixtureRouterIP)})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(peers))
	}
	if peers[0].Routes != duplicateRouteFixtureCount {
		t.Errorf("Routes = %d, want %d (raw rows in the whole table: %d)",
			peers[0].Routes, duplicateRouteFixtureCount, raw)
	}
}

// TestPeersRouteCountExcludesWhatIsNotACurrentRoute covers the three ways
// a route_unicast row can sit in the table without being a route this
// peer is currently advertising: it is a protocol sentinel rather than a
// route at all, its own route key's newest observation withdrew it, or the
// peer that sent it is down as of the current session. insertRouteCountFixture
// places one peer in each shape -- see its own doc comment for why each
// peer is kept separate from the others rather than combined into one
// fixture whose Routes total could hide which specific predicate broke.
func TestPeersRouteCountExcludesWhatIsNotACurrentRoute(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRouteCountFixture(t, ctx, q)

	got, err := q.Peers(ctx, PeerFilter{Router: netip.MustParseAddr(routeCountFixtureRouterIP)})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	// Three peers went into the fixture; each subtest below picks its own
	// row back out and asserts its own Routes value, so this only checks
	// that Peers returned exactly the three rows the fixture wrote and
	// nothing unexpected -- the value assertions themselves live in the
	// subtests, where a failure names which specific behavior broke.
	if len(got) != 3 {
		t.Fatalf("got %d peers, want 3: %+v", len(got), got)
	}
	for _, p := range got {
		switch peerIP := p.PeerIP.String(); peerIP {
		case routeCountFixtureMarkerPeer, routeCountFixtureWithdrawPeer, routeCountFixtureDownPeer:
		default:
			t.Errorf("unexpected peer %s in result: %+v", peerIP, p)
		}
	}

	t.Run("a peer with routes and a marker reports only the routes", func(t *testing.T) {
		for _, p := range got {
			if p.PeerIP.String() != routeCountFixtureMarkerPeer {
				continue
			}
			if p.Routes != 2 {
				t.Errorf("Routes = %d, want 2 -- the end-of-rib marker is not "+
					"a third route", p.Routes)
			}
			return
		}
		t.Fatal("no row for routeCountFixtureMarkerPeer")
	})

	t.Run("a withdrawn route is not counted", func(t *testing.T) {
		for _, p := range got {
			if p.PeerIP.String() != routeCountFixtureWithdrawPeer {
				continue
			}
			if p.Routes != 1 {
				t.Errorf("Routes = %d, want 1 -- the withdrawn route's earlier "+
					"advertise row is still in route_unicast and must not be "+
					"counted", p.Routes)
			}
			return
		}
		t.Fatal("no row for routeCountFixtureWithdrawPeer")
	})

	t.Run("a down peer counts zero routes", func(t *testing.T) {
		for _, p := range got {
			if p.PeerIP.String() != routeCountFixtureDownPeer {
				continue
			}
			if p.State != "down" {
				t.Fatalf("fixture is not adversarial: State = %q, want %q", p.State, "down")
			}
			if p.Routes != 0 {
				t.Errorf("Routes = %d, want 0 -- a down peer contributes "+
					"nothing, even though its route_unicast row from before "+
					"it went down is still on record", p.Routes)
			}
			return
		}
		t.Fatal("no row for routeCountFixtureDownPeer")
	})
}
