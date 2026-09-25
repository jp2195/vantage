package query

import (
	"context"
	"net/netip"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jp2195/vantage/chtest"
)

// stubConn is a driver.Conn that is not nil but otherwise does nothing.
// TestNewRejectsADatabaseNameItWouldHaveToInterpolate needs a conn that is
// merely non-nil -- New's nil check would otherwise mask the database-name
// error these cases are actually about, and New's nil rejection has its own
// dedicated test below. Embedding driver.Conn rather than hand-implementing
// its dozen methods is what the interface's own doc comment recommends: see
// driver.Conn's "Compatibility" note on why a hand-rolled implementation of
// every method is a compile break waiting to happen the next time the
// driver adds one.
type stubConn struct {
	driver.Conn
}

func TestNewRejectsADatabaseNameItWouldHaveToInterpolate(t *testing.T) {
	for _, db := range []string{"", "va ntage", "vantage;DROP", "0vantage", "vantage-1"} {
		if _, err := New(stubConn{}, db); err == nil {
			t.Errorf("New accepted %q; it is interpolated into SQL and must be rejected", db)
		}
	}
	if _, err := New(stubConn{}, "vantage_query_test"); err != nil {
		t.Errorf("New rejected a valid identifier: %v", err)
	}
}

// TestNewRejectsANilConn is the regression test for New(nil, db): a nil
// conn used to let New construct a *Q that panicked on its first real
// query instead of reporting the mistake at construction time, the same
// way New's own database-name check already does.
func TestNewRejectsANilConn(t *testing.T) {
	if _, err := New(nil, "vantage_query_test"); err == nil {
		t.Error("New(nil, ...) succeeded; a nil conn must be rejected before it panics on first use")
	}
}

// testDB is this package's own ClickHouse database, distinct from sink's
// vantage_test (see sink/clickhouse_test.go) and from vantage itself. Two
// packages sharing one test database would each see the other's fixture
// rows; chtest.DSN and chtest.Require key everything on this name, so
// picking a name this package alone uses is what keeps its tests isolated
// from sink's without either package knowing the other exists.
const testDB = "vantage_query_test"

// requireQuery ensures testDB exists with the shipped schema applied (via
// chtest.Require) and wraps the resulting connection in a *Q, the same way
// a real caller would. It builds on chtest.Require rather than dialing
// testDB directly so that an unreachable ClickHouse skips (or, under
// VANTAGE_REQUIRE_CLICKHOUSE, fails) through chtest.Skip's single gate
// instead of every test in this package re-implementing it.
func requireQuery(t *testing.T, ctx context.Context) *Q {
	t.Helper()
	conn := chtest.Require(t, ctx, testDB)
	q, err := New(conn, testDB)
	if err != nil {
		// Require already proved testDB is a valid identifier by using it
		// to build a DSN; New rejecting it here would mean New and
		// chtest.DSN disagree about what a plain identifier looks like,
		// which is a bug in this package, not a skippable environment
		// problem.
		t.Fatalf("New(testDB): %v", err)
	}
	return q
}

// TestEveryNextHopComesBackUnmapped covers all four scan loops that read a
// next_hop column, because all four carried the same false comment: that
// netip.ParseAddr "never produces the IPv4-mapped form to begin with". It
// does, whenever the stored text is mapped, and next_hop is a String column
// holding whatever sink wrote. See unmapAll.
//
// The failure is silent and it is the one unmapAll exists to prevent: a
// mapped NextHop prints "::ffff:10.130.9.1" beside a plain "10.130.9.1"
// router_ip in the same response, and netip.Addr equality is
// representation-sensitive, so NextHop == PeerIP is false for a pair that
// is in fact the same address.
//
// Both directions are asserted per surface: that the stored text really is
// mapped -- otherwise the fixture could drift to a plain next hop and leave
// every assertion below passing against nothing -- and that what comes back
// is the plain form. The stored-side check reads the column directly rather
// than through any query in this package, so it cannot be satisfied by the
// very unmapping it is there to make load-bearing.
func TestEveryNextHopComesBackUnmapped(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertMappedNextHopFixture(t, ctx, q)

	want := netip.MustParseAddr(mappedNextHopPlain)
	if want.Is4In6() {
		t.Fatalf("mappedNextHopPlain %q is itself mapped; it is what the "+
			"unmapped answer must equal", mappedNextHopPlain)
	}

	for _, tc := range []struct{ table, column, where string }{
		{"route_unicast", "next_hop", "prefix = '" + mappedNextHopUnicastPrefix + "'"},
		{"route_vpn", "next_hop", "prefix = '" + mappedNextHopVPNPrefix + "'"},
		{"route_evpn", "next_hop", "prefix = '" + mappedNextHopEVPNPrefix + "'"},
	} {
		var stored string
		if err := q.conn.QueryRow(ctx, "SELECT any("+tc.column+") FROM "+q.db+"."+
			tc.table+" WHERE "+tc.where).Scan(&stored); err != nil {
			t.Fatalf("read stored %s.%s: %v", tc.table, tc.column, err)
		}
		if stored != mappedNextHopStored {
			t.Fatalf("%s stores next_hop %q, want the IPv4-mapped %q -- with a "+
				"plain next hop stored, Unmap is a no-op and this test asserts "+
				"nothing", tc.table, stored, mappedNextHopStored)
		}
		if a, err := netip.ParseAddr(stored); err != nil || !a.Is4In6() {
			t.Fatalf("netip.ParseAddr(%q) = %v, %v -- the premise of this test "+
				"is that ParseAddr yields the mapped form for mapped text",
				stored, a, err)
		}
	}

	t.Run("Routes", func(t *testing.T) {
		got, err := q.Routes(ctx, RouteFilter{Prefix: mappedNextHopUnicastPrefix})
		if err != nil {
			t.Fatalf("Routes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		assertUnmapped(t, got[0].NextHop, want)
	})

	t.Run("VPNRoutes", func(t *testing.T) {
		got, err := q.VPNRoutes(ctx, VPNRouteFilter{Prefix: mappedNextHopVPNPrefix})
		if err != nil {
			t.Fatalf("VPNRoutes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		assertUnmapped(t, got[0].NextHop, want)
	})

	t.Run("EVPNRoutes", func(t *testing.T) {
		got, err := q.EVPNRoutes(ctx, EVPNRouteFilter{Prefix: mappedNextHopEVPNPrefix})
		if err != nil {
			t.Fatalf("EVPNRoutes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(got), got)
		}
		assertUnmapped(t, got[0].NextHop, want)
	})

	t.Run("RouteHistory", func(t *testing.T) {
		got, err := q.RouteHistory(ctx, HistoryFilter{Prefix: mappedNextHopUnicastPrefix})
		if err != nil {
			t.Fatalf("RouteHistory: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d events, want 1: %+v", len(got), got)
		}
		assertUnmapped(t, got[0].NextHop, want)
	})
}

// assertUnmapped fails unless got is want in plain form. Both halves are
// checked rather than equality alone: Is4In6 names the actual defect, and
// the comparison is what a caller would make.
func assertUnmapped(t *testing.T, got, want netip.Addr) {
	t.Helper()
	if got.Is4In6() {
		t.Errorf("NextHop = %q is IPv4-mapped; every netip.Addr this package "+
			"returns is plain form (see unmapAll), so a mapped one compares "+
			"unequal to the same address reached through any other field", got)
	}
	if got != want {
		t.Errorf("NextHop = %q, want %q", got, want)
	}
}
