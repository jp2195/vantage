package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/api"
	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
)

// cliLiveness is the fixture's three collectors: one fresh, one silent for
// two hours, and one whose process restarted after its session opened. Each
// has one router, one up peer and one unicast route, and withLS adds one
// link-state node, link and prefix.
var cliLiveness = []struct {
	collector, router, peer, prefix string
	restarted                       bool
	beatAge                         time.Duration
	want                            string
}{
	{chtest.LivenessPrefix + "cli-fresh", "10.254.1.1", "10.254.1.2", "10.254.1.0/24", false, time.Minute, "up"},
	{chtest.LivenessPrefix + "cli-stale", "10.254.2.1", "10.254.2.2", "10.254.2.0/24", false, 2 * time.Hour, "stale"},
	{chtest.LivenessPrefix + "cli-lost", "10.254.3.1", "10.254.3.2", "10.254.3.0/24", true, time.Minute, "view_lost"},
}

func insertCLILivenessFixture(t *testing.T, ctx context.Context, conn driver.Conn, db string, withLS bool) {
	t.Helper()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	for i, c := range cliLiveness {
		sid := uint64(base.UnixNano()) + uint64(i)
		stmts := []string{
			"INSERT INTO " + db + ".peer_events (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, kind) VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 1, ?, ?, 1, 'up')",
			"INSERT INTO " + db + ".route_unicast (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, family, prefix, as_path, next_hop) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 2, ?, ?, 2, 'ipv4u', '" + c.prefix + "', [65254], '" + c.peer + "')",
		}
		if withLS {
			stmts = append(stmts,
				"INSERT INTO "+db+".ls_nodes (collector_id, router_ip, peer_ip, rib, session_id, seq, "+
					"ts_router, ts_collector, stream_seq, protocol, asn, router_id) "+
					"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 3, ?, ?, 3, 2, 65254, '"+c.router+"')",
				"INSERT INTO "+db+".ls_links (collector_id, router_ip, peer_ip, rib, session_id, seq, "+
					"ts_router, ts_collector, stream_seq, protocol, local_asn, local_router_id, remote_asn, remote_router_id) "+
					"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 4, ?, ?, 4, 2, 65254, '"+c.router+"', 65254, '"+c.peer+"')",
				"INSERT INTO "+db+".ls_prefixes (collector_id, router_ip, peer_ip, rib, session_id, seq, "+
					"ts_router, ts_collector, stream_seq, protocol, asn, router_id, prefix, prefix_len) "+
					"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 5, ?, ?, 5, 2, 65254, '"+c.router+"', '"+c.prefix+"', 24)")
		}
		for _, stmt := range stmts {
			if err := conn.Exec(ctx, stmt, c.collector, c.router, c.peer, sid, base, base); err != nil {
				t.Fatalf("fixture for %s: %v", c.collector, err)
			}
		}
		started := int64(sid) - int64(time.Second)
		if c.restarted {
			started = int64(sid) + int64(time.Second)
		}
		if err := conn.Exec(ctx, "INSERT INTO "+db+".collector_beats (collector_id, started_at, beat_at, inserted_at) "+
			"SELECT ?, fromUnixTimestamp64Nano(toInt64(?), 'UTC'), now64(3), now64(3) - toIntervalMillisecond(?)",
			c.collector, started, c.beatAge.Milliseconds()); err != nil {
			t.Fatalf("beat for %s: %v", c.collector, err)
		}
	}
}

// TestBothSourcesAgreeOnLiveness is the parity test for the three states the
// liveness rule adds, and it is not vacuous the way the existing parity tests
// would be here: every other fixture's collector is immortal (chtest), so
// PeersStale is 0 on both sides whether or not the wire carries it. These
// rows are stale and view_lost, and must survive the API's JSON round trip
// exactly as the direct path reads them.
func TestBothSourcesAgreeOnLiveness(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertCLILivenessFixture(t, ctx, conn, testDB, false)
	q, err := query.New(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	if q, err = q.WithStaleAfter(time.Hour); err != nil {
		t.Fatal(err)
	}
	apiSrv, err := api.NewServer(q, testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)
	direct, viaAPI := &directSource{q: q}, &apiSource{base: srv.URL, token: testToken}

	ours := func(rs []query.Router) []query.Router {
		return slices.DeleteFunc(rs, func(r query.Router) bool {
			return !strings.HasPrefix(r.Collector, chtest.LivenessPrefix+"cli-")
		})
	}
	a, err := direct.Routers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := viaAPI.Routers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, b = ours(a), ours(b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("routers disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
	}
	stale := 0
	for _, r := range a {
		stale += r.PeersStale
	}
	if stale != 1 {
		t.Fatalf("the fixture's routers carry %d stale peers, want 1: this comparison would be vacuous", stale)
	}

	for _, c := range cliLiveness {
		router := netip.MustParseAddr(c.router)
		pa, err := direct.Peers(ctx, router)
		if err != nil {
			t.Fatal(err)
		}
		pb, err := viaAPI.Peers(ctx, router)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(pa, pb) {
			t.Errorf("%s: peers disagree:\n\tdirect: %+v\n\tapi:    %+v", c.collector, pa, pb)
		}
		if len(pa) != 1 || pa[0].State != c.want {
			t.Errorf("%s: peers %+v, want one reading %s", c.collector, pa, c.want)
		}
		ra, err := direct.Routes(ctx, RouteQuery{Prefix: c.prefix})
		if err != nil {
			t.Fatal(err)
		}
		rb, err := viaAPI.Routes(ctx, RouteQuery{Prefix: c.prefix})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ra, rb) {
			t.Errorf("%s: routes disagree:\n\tdirect: %+v\n\tapi:    %+v", c.collector, ra, rb)
		}
		if want := map[bool]int{true: 0, false: 1}[c.want == "view_lost"]; len(ra.Unicast) != want {
			t.Errorf("%s: %d unicast routes, want %d", c.collector, len(ra.Unicast), want)
		}
	}
}

// liveWarnDB is TestDirectPathWarnsOfAStaleCollector's database.
const liveWarnDB = "vantage_cli_live_warn_test"

// TestDirectPathWarnsOfAStaleCollector: the -dsn path prints the same
// collector_stale warning the API puts in meta.warnings, for every answer
// that carries routes or link-state rows, and only when a row came from a
// stale collector.
func TestDirectPathWarnsOfAStaleCollector(t *testing.T) {
	ctx := t.Context()
	// A database of its own: the link-state rows would otherwise be counted
	// by the unscoped link-state parity tests that share testDB.
	conn := chtest.Require(t, ctx, liveWarnDB)
	insertCLILivenessFixture(t, ctx, conn, liveWarnDB, true)
	q, err := query.New(conn, liveWarnDB)
	if err != nil {
		t.Fatal(err)
	}
	if q, err = q.WithStaleAfter(time.Hour); err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	printWarnings(&want, api.StaleWarning(true, time.Hour))
	if want.Len() == 0 {
		t.Fatal("the expected warning is empty: this test would pass with no warning at all")
	}

	answers := []struct {
		name string
		ask  func(source, netip.Addr, netip.Addr, string) (int, error)
	}{
		{"routes", func(s source, _, _ netip.Addr, prefix string) (int, error) {
			f, err := s.Routes(ctx, RouteQuery{Prefix: prefix})
			return len(f.Unicast), err
		}},
		{"rib", func(s source, router, peer netip.Addr, _ string) (int, error) {
			f, err := s.RIB(ctx, RIBQuery{Router: router, Peer: peer, RIB: "in_pre", Family: "unicast", Limit: 10})
			return len(f.Unicast), err
		}},
		{"ls nodes", func(s source, router, _ netip.Addr, _ string) (int, error) {
			rows, err := s.LSNodes(ctx, LSQuery{Router: router})
			return len(rows), err
		}},
		{"ls links", func(s source, router, _ netip.Addr, _ string) (int, error) {
			rows, err := s.LSLinks(ctx, LSQuery{Router: router})
			return len(rows), err
		}},
		{"ls prefixes", func(s source, router, _ netip.Addr, _ string) (int, error) {
			rows, err := s.LSPrefixes(ctx, LSQuery{Router: router})
			return len(rows), err
		}},
	}
	for _, c := range cliLiveness {
		router, peer := netip.MustParseAddr(c.router), netip.MustParseAddr(c.peer)
		for _, a := range answers {
			var got strings.Builder
			n, err := a.ask(&directSource{q: q, warn: &got}, router, peer, c.prefix)
			if err != nil {
				t.Fatalf("%s %s: %v", c.collector, a.name, err)
			}
			switch c.want {
			case "stale":
				if n != 1 {
					t.Fatalf("%s %s: %d rows, want 1: the warning check would be vacuous", c.collector, a.name, n)
				}
				if got.String() != want.String() {
					t.Errorf("%s %s: warned %q, want %q", c.collector, a.name, got.String(), want.String())
				}
			case "up":
				if n != 1 {
					t.Fatalf("%s %s: %d rows, want 1: the no-warning check would be vacuous", c.collector, a.name, n)
				}
				if got.Len() != 0 {
					t.Errorf("%s %s: a fresh collector's rows warned %q", c.collector, a.name, got.String())
				}
			default:
				if n != 0 || got.Len() != 0 {
					t.Errorf("%s %s: %d rows and warning %q, want neither", c.collector, a.name, n, got.String())
				}
			}
		}
	}
}

// TestStaleAfterFlagReachesTheDirectPath: -stale-after is what the -dsn path
// reads with, and a value under two heartbeats is refused before any query
// runs.
func TestStaleAfterFlagReachesTheDirectPath(t *testing.T) {
	ctx := t.Context()
	chtest.Require(t, ctx, testDB)
	open := func(args ...string) (source, func(), error) {
		fs := flag.NewFlagSet("query routers", flag.ContinueOnError)
		f := registerQueryFlags(fs)
		if err := fs.Parse(append([]string{"-dsn", chtest.DSN(testDB)}, args...)); err != nil {
			t.Fatal(err)
		}
		return f.source(ctx)
	}
	src, closeFn, err := open("-stale-after", "2h")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if got := src.(*directSource).q.StaleAfter(); got != 2*time.Hour {
		t.Errorf("direct path reads with %v, want 2h", got)
	}
	src, closeFn, err = open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if got := src.(*directSource).q.StaleAfter(); got != query.DefaultStaleAfter {
		t.Errorf("no flag: direct path reads with %v, want %v", got, query.DefaultStaleAfter)
	}
	if _, _, err := open("-stale-after", "10s"); err == nil || !strings.Contains(err.Error(), "-stale-after") {
		t.Errorf("-stale-after 10s = %v, want an error naming the flag", err)
	}
}

// TestStaleAfterFlagWarnsOnTheAPIPath: through the API, the daemon's own
// stale_after applies, so a -stale-after given there changes nothing and
// says so on stderr. Without the flag, nothing is said.
func TestStaleAfterFlagWarnsOnTheAPIPath(t *testing.T) {
	for _, tc := range []struct {
		args []string
		warn bool
	}{
		{[]string{"-token", "t", "-stale-after", "2h"}, true},
		{[]string{"-token", "t"}, false},
	} {
		fs := flag.NewFlagSet("query routers", flag.ContinueOnError)
		f := registerQueryFlags(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		f.warn = &stderr
		src, closeFn, err := f.source(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		closeFn()
		if _, ok := src.(*apiSource); !ok {
			t.Fatalf("%v: source is %T, want the API", tc.args, src)
		}
		said := strings.Contains(stderr.String(), "-stale-after applies only with -dsn")
		if said != tc.warn {
			t.Errorf("%v: stderr %q, want the -stale-after warning: %v", tc.args, stderr.String(), tc.warn)
		}
	}
}
