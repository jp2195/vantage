package api

import (
	"context"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// apiLivenessCase is one collector of insertAPILivenessFixture. Each family
// gets its own prefix, so a /v1/routes fan-out asked about one of them
// returns that family's row alone: the fan-out's three legs each carry their
// own copy of the stale check, and a shared prefix would let any one leg
// raise the warning for the other two.
type apiLivenessCase struct {
	collector, router, peer string
	unicast, vpn, evpn      string // prefixes
	sid                     uint64
	beatAge                 time.Duration // 0: never heard from
}

// apiLiveness is the fixture's four collectors. fresh and stale differ only
// in how long ago the archive heard from them; silent was never heard from.
// silent and stale are both stale, which /v1/collectors must still tell
// apart: one has a last_beat_at, the other has none. fresh2 is fresh and
// watches stale's router, through a peer of its own: a router two collectors
// watch, one of them dead, which is what tells a warning keyed on the row's
// collector from one keyed on its router alone.
var apiLiveness = []apiLivenessCase{
	{chtest.LivenessPrefix + "api-fresh", "10.253.1.1", "10.253.1.2",
		"10.253.1.0/26", "10.253.1.64/26", "10.253.1.128/26", 0, time.Minute},
	{chtest.LivenessPrefix + "api-stale", "10.253.2.1", "10.253.2.2",
		"10.253.2.0/26", "10.253.2.64/26", "10.253.2.128/26", 0, 2 * time.Hour},
	{chtest.LivenessPrefix + "api-silent", "10.253.3.1", "10.253.3.2",
		"10.253.3.0/26", "10.253.3.64/26", "10.253.3.128/26", 0, 0},
	{chtest.LivenessPrefix + "api-fresh2", "10.253.2.1", "10.253.2.3",
		"10.253.4.0/26", "10.253.4.64/26", "10.253.4.128/26", 0, time.Minute},
}

// apiShared is the index in apiLiveness of fresh2, the fresh collector on the
// stale collector's router.
const apiShared = 3

// rowTargets is every request whose answer holds one collector's rows alone,
// even on a router another collector also watches: routes by the collector's
// own prefixes, RIB walks pinned to it with collector=, and link-state
// requests scoped to its own peer, both paginated (router= and peer=, with
// rib= and a cursor pinned to the collector, the only pin a link-state page
// takes) and capped (router= and peer= plus protocol=, which a scoped
// request treats as a narrowing and answers capped). Each of these answers
// keys its warning on its rows' (collector, router), so each is asked here.
// The capped link-state names are listed in lsCapped, so the test can check
// that branch really answered.
func rowTargets(t *testing.T, s *Server, c apiLivenessCase) map[string]string {
	t.Helper()
	u, v, e := url.QueryEscape(c.unicast), url.QueryEscape(c.vpn), url.QueryEscape(c.evpn)
	rib := "?router=" + c.router + "&peer=" + c.peer + "&collector=" + c.collector
	ls := "?router=" + c.router + "&peer=" + c.peer
	router, peer := netip.MustParseAddr(c.router), netip.MustParseAddr(c.peer)
	start, err := s.q.RIBStart(t.Context(), router, peer, "in_pre", c.collector)
	if err != nil || start == nil {
		t.Fatalf("pin %s's walk of %s: %v, %v", c.collector, c.router, start, err)
	}
	page := ls + "&rib=in_pre&cursor=" + url.QueryEscape(encodeCursor(*start))
	return map[string]string{
		"routes fan-out, unicast leg": "/v1/routes?prefix=" + u,
		"routes fan-out, vpn leg":     "/v1/routes?prefix=" + v,
		"routes fan-out, evpn leg":    "/v1/routes?prefix=" + e,
		"routes/unicast":              "/v1/routes/unicast?prefix=" + u,
		"routes/vpn":                  "/v1/routes/vpn?prefix=" + v,
		"routes/evpn":                 "/v1/routes/evpn?prefix=" + e,
		"rib/unicast":                 "/v1/rib/unicast" + rib,
		"rib/vpn":                     "/v1/rib/vpn" + rib,
		"rib/evpn":                    "/v1/rib/evpn" + rib,
		"ls/nodes page":               "/v1/ls/nodes" + page,
		"ls/links page":               "/v1/ls/links" + page,
		"ls/prefixes page":            "/v1/ls/prefixes" + page,
		"ls/nodes capped, scoped":     "/v1/ls/nodes" + ls + "&protocol=2",
		"ls/links capped, scoped":     "/v1/ls/links" + ls + "&protocol=2",
		"ls/prefixes capped, scoped":  "/v1/ls/prefixes" + ls + "&protocol=2",
	}
}

// lsCapped is rowTargets' capped link-state names.
var lsCapped = map[string]bool{
	"ls/nodes capped, scoped": true, "ls/links capped, scoped": true, "ls/prefixes capped, scoped": true,
}

// insertAPILivenessFixture writes, for each collector, one up peer and one
// live object in every family the warning is attached on, and the collector's
// heartbeat. Every session opened after its collector's process started, so
// nothing here is epoch-lost; the epoch rule is query's to test.
func insertAPILivenessFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	for i := range apiLiveness {
		c := &apiLiveness[i]
		c.sid = uint64(base.UnixNano()) + uint64(i)
		args := []any{c.collector, c.router, c.peer, c.sid, base, base}
		for _, stmt := range []string{
			"INSERT INTO " + apiTestDB + ".peer_events (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, kind) VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 1, ?, ?, 1, 'up')",
			"INSERT INTO " + apiTestDB + ".route_unicast (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, family, prefix, as_path, next_hop) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 2, ?, ?, 2, 'ipv4u', '" + c.unicast + "', [65253], '" + c.peer + "')",
			"INSERT INTO " + apiTestDB + ".route_vpn (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, family, prefix, rd, as_path, next_hop) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 3, ?, ?, 3, 'vpn4', '" + c.vpn + "', '65253:1', [65253], '" + c.peer + "')",
			"INSERT INTO " + apiTestDB + ".route_evpn (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, route_type, rd, prefix, as_path, next_hop) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 4, ?, ?, 4, 5, '65253:2', '" + c.evpn + "', [65253], '" + c.peer + "')",
			"INSERT INTO " + apiTestDB + ".ls_nodes (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, protocol, router_id, name) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 5, ?, ?, 5, 2, 'fd000001', 'api-node')",
			"INSERT INTO " + apiTestDB + ".ls_links (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, protocol, local_router_id, remote_router_id) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 6, ?, ?, 6, 2, 'fd000001', 'fd000002')",
			"INSERT INTO " + apiTestDB + ".ls_prefixes (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
				"ts_router, ts_collector, stream_seq, protocol, router_id, prefix, prefix_len) " +
				"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 7, ?, ?, 7, 2, 'fd000001', '10.253.99.1', 32)",
		} {
			if err := conn.Exec(ctx, stmt, args...); err != nil {
				t.Fatalf("fixture for %s: %v", c.collector, err)
			}
		}
		if c.beatAge == 0 {
			continue
		}
		if err := conn.Exec(ctx, "INSERT INTO "+apiTestDB+".collector_beats (collector_id, started_at, beat_at, inserted_at) "+
			"SELECT ?, fromUnixTimestamp64Nano(toInt64(?), 'UTC'), now64(3), now64(3) - toIntervalMillisecond(?)",
			c.collector, int64(c.sid)-int64(time.Second), c.beatAge.Milliseconds()); err != nil {
			t.Fatalf("beat for %s: %v", c.collector, err)
		}
	}
}

// requireLivenessAPI is a Server over apiTestDB reading with a one-hour stale
// threshold, so a beat a minute old is fresh and one two hours old is stale
// however long the test takes.
func requireLivenessAPI(t *testing.T) *Server {
	t.Helper()
	return requireLivenessAPIAfter(t, time.Hour)
}

// requireLivenessAPIAfter is requireLivenessAPI reading with threshold after.
func requireLivenessAPIAfter(t *testing.T, after time.Duration) *Server {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	insertAPILivenessFixture(t, ctx, conn)
	q, err := query.New(conn, apiTestDB)
	if err != nil {
		t.Fatal(err)
	}
	if q, err = q.WithStaleAfter(after); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(q, Config{
		DefaultPage: 1000, MaxPage: 10000, MaxUnscopedSince: 24 * time.Hour,
		Tokens: []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStaleRowsCarryTheWarningAndFreshOnesDoNot: every surface that serves a
// stale collector's rows says so, and none says so about a fresh collector's
// rows -- even with a stale collector in the same archive at the same time.
// That second half is what keys the warning on the ROW's collector rather
// than on "is anything stale anywhere", and each endpoint has its own copy of
// the check, so each is asserted here.
func TestStaleRowsCarryTheWarningAndFreshOnesDoNot(t *testing.T) {
	s := requireLivenessAPI(t)
	fresh, stale := apiLiveness[0], apiLiveness[1]
	// rowTargets, plus the answers scoped by router alone: capped link-state
	// by router=, /v1/peers, and /v1/topology. On stale's router these merge
	// fresh2's rows in, so they are asked here and not in the shared-router
	// test below.
	targets := func(c apiLivenessCase) map[string]string {
		m := rowTargets(t, s, c)
		m["ls/nodes"] = "/v1/ls/nodes?router=" + c.router
		m["ls/links"] = "/v1/ls/links?router=" + c.router
		m["ls/prefixes"] = "/v1/ls/prefixes?router=" + c.router
		m["peers"] = "/v1/peers?router=" + c.router
		m["topology"] = "/v1/topology?router=" + c.router + "&prefix=" + url.QueryEscape(c.unicast)
		return m
	}
	for name, target := range targets(stale) {
		if m := getMeta(t, s, target); !hasWarning(m.Warnings, WarnCollectorStale) {
			t.Errorf("%s over a stale collector's rows: warnings %+v, want %s", name, m.Warnings, WarnCollectorStale)
		}
	}
	for name, target := range targets(fresh) {
		if m := getMeta(t, s, target); hasWarning(m.Warnings, WarnCollectorStale) {
			t.Errorf("%s over a fresh collector's rows carries %s: %+v", name, WarnCollectorStale, m.Warnings)
		}
	}
	// The row is served, not only flagged.
	var rows []WireUnicastRoute
	getOK(t, s, "/v1/routes/unicast?prefix="+url.QueryEscape(stale.unicast), &rows)
	if len(rows) != 1 {
		t.Errorf("a stale collector's route: %d rows, want 1", len(rows))
	}
	if got := peerStates(t, s, stale.router)[stale.collector]; got != query.PeerStateStale {
		t.Errorf("a stale collector's peer reads %q, want stale", got)
	}
}

// peerStates is /v1/peers?router= as each collector's one peer's state.
func peerStates(t *testing.T, s *Server, router string) map[string]string {
	t.Helper()
	var peers []WirePeer
	getOK(t, s, "/v1/peers?router="+router, &peers)
	states := map[string]string{}
	for _, p := range peers {
		if _, dup := states[p.Collector]; dup {
			t.Fatalf("/v1/peers?router=%s: two peers for %s: %+v", router, p.Collector, peers)
		}
		states[p.Collector] = p.State
	}
	return states
}

// TestASharedRouterWarnsOnlyOnTheStaleCollectorsRows: on a router two
// collectors watch, one fresh and one stale, the fresh collector's rows come
// back unqualified and the stale collector's rows on the same router still
// carry collector_stale. A warning keyed on the router alone passes every
// other test in this file, because there each router has one collector; this
// is the test that fails it. /v1/topology is not asked: a graph merges
// collectors, and its warning is keyed on the router by design ("may
// include").
func TestASharedRouterWarnsOnlyOnTheStaleCollectorsRows(t *testing.T) {
	s := requireLivenessAPI(t)
	stale, fresh2 := apiLiveness[1], apiLiveness[apiShared]
	if stale.router != fresh2.router {
		t.Fatalf("fixture: fresh2 watches %s, stale %s; they must share a router", fresh2.router, stale.router)
	}
	check := func(c apiLivenessCase, want bool) {
		for name, target := range rowTargets(t, s, c) {
			m := getMeta(t, s, target)
			if lsCapped[name] && m.TotalMatched == nil {
				t.Errorf("%s: no total_matched, so the capped branch did not answer", name)
			}
			if got := hasWarning(m.Warnings, WarnCollectorStale); got != want {
				t.Errorf("%s over %s's rows on shared router %s: collector_stale %v, want %v (%+v)",
					name, c.collector, c.router, got, want, m.Warnings)
			}
		}
	}
	check(fresh2, false)
	check(stale, true)

	// Each is served. The rows are the collector's own, not the other's.
	var rows []WireUnicastRoute
	getOK(t, s, "/v1/routes/unicast?prefix="+url.QueryEscape(fresh2.unicast), &rows)
	if len(rows) != 1 || rows[0].Collector != fresh2.collector {
		t.Errorf("fresh2's route on the shared router: %+v, want its one row", rows)
	}

	// Peer state is per collector, too.
	states := peerStates(t, s, stale.router)
	if states[fresh2.collector] != "up" || states[stale.collector] != query.PeerStateStale {
		t.Errorf("peers on the shared router: %v, want %s up and %s stale", states, fresh2.collector, stale.collector)
	}

	// /v1/routers lists the router once per collector, each counting its own
	// peers: fresh2's row is not the stale one's, and no count adds the two.
	var routers []WireRouter
	getOK(t, s, "/v1/routers", &routers)
	var n int
	for _, r := range routers {
		if r.IP != stale.router {
			continue
		}
		n++
		wantUp, wantStale := 0, 1
		if r.Collector == fresh2.collector {
			wantUp, wantStale = 1, 0
		}
		if r.PeersUp != wantUp || r.PeersStale != wantStale {
			t.Errorf("/v1/routers %s via %s: peers_up %d peers_stale %d, want %d and %d",
				r.IP, r.Collector, r.PeersUp, r.PeersStale, wantUp, wantStale)
		}
	}
	if n != 2 {
		t.Errorf("/v1/routers: %d rows for shared router %s, want 2 (one per collector)", n, stale.router)
	}
}

// TestRoutersAndCollectorsCountStalePeers: the two inventories count stale
// peers in their own column, and /v1/collectors tells a collector heard from
// long ago (a last_beat_at) from one never heard from at all (null).
func TestRoutersAndCollectorsCountStalePeers(t *testing.T) {
	s := requireLivenessAPI(t)
	var routers []WireRouter
	meta := getOK(t, s, "/v1/routers", &routers)
	if !hasWarning(meta.Warnings, WarnCollectorStale) {
		t.Errorf("/v1/routers with stale routers listed: warnings %+v", meta.Warnings)
	}
	byCollector := map[string]WireRouter{}
	for _, r := range routers {
		byCollector[r.Collector] = r
	}
	for _, c := range apiLiveness {
		r, ok := byCollector[c.collector]
		if !ok {
			t.Errorf("/v1/routers has no row for %s", c.collector)
			continue
		}
		wantUp, wantStale := 0, 1
		if c.beatAge == time.Minute {
			wantUp, wantStale = 1, 0
		}
		if r.PeersUp != wantUp || r.PeersStale != wantStale {
			t.Errorf("%s: peers_up %d peers_stale %d, want %d and %d", c.collector, r.PeersUp, r.PeersStale, wantUp, wantStale)
		}
	}

	if m := getMeta(t, s, "/v1/collectors"); !hasWarning(m.Warnings, WarnCollectorStale) {
		t.Errorf("/v1/collectors with stale collectors listed: warnings %+v", m.Warnings)
	}
	rec := get(t, s, "/v1/collectors")
	body := rec.Body.String()
	for _, c := range apiLiveness {
		i := strings.Index(body, `"collector":"`+c.collector+`"`)
		if i < 0 {
			t.Fatalf("/v1/collectors has no %s", c.collector)
		}
		entry := body[i:]
		if j := strings.Index(entry[1:], `"collector":"`); j >= 0 {
			entry = entry[:j+1]
		}
		// Both nullable instants, on the wire: null for the collector never
		// heard from, and set for the two that beat.
		for _, field := range []string{"last_beat_at", "started_at"} {
			heard := !strings.Contains(entry, `"`+field+`":null`)
			if want := c.beatAge != 0; heard != want {
				t.Errorf("%s: %s set = %v, want %v (%s)", c.collector, field, heard, want, entry)
			}
		}
	}

	// The counts, at both grains /v1/collectors reports them: the archive
	// summary and its one router. A string match on the body cannot tell the
	// two apart, so this decodes. started_at is the fixture's own value, one
	// second before the collector's session opened.
	var collectors []WireCollector
	getOK(t, s, "/v1/collectors", &collectors)
	for _, c := range apiLiveness {
		a := findCollector(t, collectors, c.collector).Archive
		if a == nil || len(a.Routers) != 1 {
			t.Errorf("%s: archive %+v, want one router", c.collector, a)
			continue
		}
		wantStale := 1
		if c.beatAge == time.Minute {
			wantStale = 0
		}
		if a.PeersStale != wantStale || a.Routers[0].PeersStale != wantStale {
			t.Errorf("%s: peers_stale %d, router peers_stale %d, want %d for both",
				c.collector, a.PeersStale, a.Routers[0].PeersStale, wantStale)
		}
		if c.beatAge == 0 {
			continue
		}
		want := time.Unix(0, int64(c.sid)-int64(time.Second)).UTC()
		if a.StartedAt == nil || !a.StartedAt.Equal(want) {
			t.Errorf("%s: started_at %v, want %v", c.collector, a.StartedAt, want)
		}
	}
}

// TestTheLongestThresholdStillReadsASilentCollectorStale: at the largest
// threshold the API accepts, a collector never heard from still reads stale,
// and a collector heard from two hours ago -- well inside a day -- reads up.
// The second half is what shows the threshold was really applied; the first
// is what a threshold past about 56 years would break, and why the API
// refuses one (TestStaleAfterAboveADayIsRefused).
func TestTheLongestThresholdStillReadsASilentCollectorStale(t *testing.T) {
	s := requireLivenessAPIAfter(t, query.MaxStaleAfter)
	for _, c := range apiLiveness {
		want := "up"
		if c.beatAge == 0 {
			want = query.PeerStateStale
		}
		if got := peerStates(t, s, c.router)[c.collector]; got != want {
			t.Errorf("%s at stale_after %v: peer reads %q, want %s", c.collector, query.MaxStaleAfter, got, want)
		}
	}
}
