package query

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// servingCase is one collector of insertServingFixture: its one router, its
// one up peer, and what that peer resolves to.
type servingCase struct {
	collector    string
	router, peer netip.Addr
	sid          uint64
	want         string // "up", "stale" or "view_lost"
	restarted    bool   // its process started after this session
	beatAge      time.Duration
	prefix       string
}

// insertServingFixture writes three collectors, each with one router, one
// peer whose stored state is up, and one live object in every route-bearing
// family: a unicast route with an End-of-RIB marker behind it, a VPN route,
// an EVPN route, and a link-state node, link and prefix. The collectors
// differ only in liveness:
//
//	srv-fresh        heard from a minute ago             -> up
//	srv-stale        last heard two hours ago            -> stale
//	srv-lost         heard from a minute ago, but its    -> view_lost
//	                 process started after this session
//	srv-lost-silent  both: restarted, and silent since   -> view_lost
//
// srv-lost-silent is the row that takes the other side of StalePairs'
// epoch exclusion: it is silent, so only that exclusion keeps it out of the
// stale set.
//
// srv-fresh also carries a second peer whose stored state is down, with a
// unicast route and a link-state node, link and prefix of its own: widening
// the gate to admit stale must not admit down.
func insertServingFixture(t *testing.T, ctx context.Context, q *Q) []servingCase {
	t.Helper()
	const prefix = chtest.LivenessPrefix + "srv-"
	cases := []servingCase{
		{collector: prefix + "fresh", want: "up", beatAge: beatFresh},
		{collector: prefix + "stale", want: "stale", beatAge: beatOld},
		{collector: prefix + "lost", want: "view_lost", restarted: true, beatAge: beatFresh},
		{collector: prefix + "lost-silent", want: "view_lost", restarted: true, beatAge: beatOld},
	}
	for i := range cases {
		c := &cases[i]
		c.router = netip.MustParseAddr(fmt.Sprintf("10.252.%d.1", i+1))
		c.peer = netip.MustParseAddr(fmt.Sprintf("10.252.%d.2", i+1))
		c.sid = livenessSID(700 + i)
		c.prefix = fmt.Sprintf("10.252.%d.0/24", i+1)
		router, peer := c.router.String(), c.peer.String()

		livenessPeer(t, ctx, q, c.collector, router, peer, c.sid, "up")
		started := time.Unix(0, int64(c.sid)).Add(-time.Second)
		if c.restarted {
			started = started.Add(2 * time.Second)
		}
		insertBeat(t, ctx, q, c.collector, started, time.Now(), c.beatAge)

		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			Collector: c.collector, RouterIP: router, RouterSysname: "srv", PeerIP: peer, RIB: "in_pre",
			Family: "ipv4u", Prefix: c.prefix, NextHop: peer, PeerASN: 65252, PeerBGPID: peer,
			SessionID: c.sid, Seq: 2, StreamSeq: 2, ASPath: []uint32{65252},
			TsRouter: livenessBase, TsCollector: livenessBase,
		})
		insertEorEvent(t, ctx, q, eorFixture{
			Collector: c.collector, RouterIP: router, RouterSysname: "srv", PeerIP: peer, RIB: "in_pre",
			Family: "ipv4u", PeerASN: 65252, PeerBGPID: peer, SessionID: c.sid, Seq: 3, StreamSeq: 3,
			TsRouter: livenessBase, TsCollector: livenessBase,
		})
		insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
			Collector: c.collector, RouterIP: router, RouterSysname: "srv", PeerIP: peer, RIB: "in_pre",
			Family: "vpn4", Prefix: c.prefix, RD: "65252:1", NextHop: peer, PeerASN: 65252, PeerBGPID: peer,
			SessionID: c.sid, Seq: 4, StreamSeq: 4, ASPath: []uint32{65252}, Labels: []uint32{100},
			TsRouter: livenessBase, TsCollector: livenessBase,
		})
		insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
			Collector: c.collector, RouterIP: router, RouterSysname: "srv", PeerIP: peer, RIB: "in_pre",
			RouteType: 5, RD: "65252:2", Prefix: c.prefix, NextHop: peer, PeerASN: 65252, PeerBGPID: peer,
			SessionID: c.sid, Seq: 5, StreamSeq: 5, ASPath: []uint32{65252}, Labels: []uint32{1000},
			TsRouter: livenessBase, TsCollector: livenessBase,
		})
		insertServingLinkState(t, ctx, q, c.collector, router, peer, c.sid)
	}

	fresh := cases[0]
	down := "10.252.1.3"
	livenessPeer(t, ctx, q, fresh.collector, fresh.router.String(), down, fresh.sid, "down")
	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		Collector: fresh.collector, RouterIP: fresh.router.String(), RouterSysname: "srv", PeerIP: down,
		RIB: "in_pre", Family: "ipv4u", Prefix: "10.252.1.128/25", NextHop: down, PeerASN: 65252,
		PeerBGPID: down, SessionID: fresh.sid, Seq: 2, StreamSeq: 2, ASPath: []uint32{65252},
		TsRouter: livenessBase, TsCollector: livenessBase,
	})
	insertServingLinkState(t, ctx, q, fresh.collector, fresh.router.String(), down, fresh.sid)
	return cases
}

// insertServingLinkState writes one link-state node, link and prefix for
// collector's session sid with (router, peer).
func insertServingLinkState(t *testing.T, ctx context.Context, q *Q, collector, router, peer string, sid uint64) {
	t.Helper()
	for _, stmt := range []string{
		"INSERT INTO " + q.db + ".ls_nodes (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
			"ts_router, ts_collector, stream_seq, protocol, router_id, name) " +
			"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 6, ?, ?, 6, 2, 'fc000001', 'srv-node')",
		"INSERT INTO " + q.db + ".ls_links (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
			"ts_router, ts_collector, stream_seq, protocol, local_router_id, remote_router_id) " +
			"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 7, ?, ?, 7, 2, 'fc000001', 'fc000002')",
		"INSERT INTO " + q.db + ".ls_prefixes (collector_id, router_ip, peer_ip, rib, session_id, seq, " +
			"ts_router, ts_collector, stream_seq, protocol, router_id, prefix, prefix_len) " +
			"VALUES (?, toIPv6(?), toIPv6(?), 'in_pre', ?, 8, ?, ?, 8, 2, 'fc000001', '10.252.99.1', 32)",
	} {
		if err := q.conn.Exec(ctx, stmt, collector, router, peer, sid, livenessBase, livenessBase); err != nil {
			t.Fatalf("insert link state for %s: %v", collector, err)
		}
	}
}

// TestStaleRoutesAreServedOnEveryFamily, once per gated statement family: a
// stale peer's routes are served, an epoch-lost peer's are not, and a down
// peer's stay excluded. Each family has its own copy of the peer_up gate,
// and each subtest here is the one that fails when its copy is left at 'up'.
//
// The stale unicast route's DumpState is part of it: a stale peer's dump
// progress is what it was at the collector's last beat, which the End-of-RIB
// marker says was complete. It is not "unknown", which is the reading for a
// peer that is disconnected.
func TestStaleRoutesAreServedOnEveryFamily(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	cases := insertServingFixture(t, ctx, q)
	served := func(c servingCase) int {
		if c.want == "view_lost" {
			return 0
		}
		return 1
	}
	for _, c := range cases {
		want := served(c)
		t.Run(c.collector, func(t *testing.T) {
			check := func(what string, got int, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if got != want {
					t.Errorf("%s for a %s peer: %d rows, want %d", what, c.want, got, want)
				}
			}
			routes, err := q.Routes(ctx, RouteFilter{Prefix: c.prefix, Router: c.router})
			check("Routes", len(routes), err)
			for _, r := range routes {
				if r.DumpState != "complete" {
					t.Errorf("Routes: %s DumpState %q, want complete", r.Prefix, r.DumpState)
				}
			}
			wide, err := q.Routes(ctx, RouteFilter{Router: c.router, OriginASN: 65252})
			check("Routes by origin_asn (the semi-join)", len(wide), err)
			vpn, err := q.VPNRoutes(ctx, VPNRouteFilter{Router: c.router})
			check("VPNRoutes", len(vpn), err)
			evpn, err := q.EVPNRoutes(ctx, EVPNRouteFilter{Router: c.router})
			check("EVPNRoutes", len(evpn), err)
			rib, _, err := q.RIBPageUnicast(ctx, c.router, c.peer, "", nil, 100)
			check("RIBPageUnicast", len(rib), err)
			g, err := q.TopologyUnicast(ctx, RouteFilter{Prefix: c.prefix, Router: c.router})
			check("TopologyUnicast routes", int(g.Routes), err)
			nodes, err := q.LSNodes(ctx, LSNodeFilter{Router: c.router})
			check("LSNodes", len(nodes), err)
			links, err := q.LSLinks(ctx, LSLinkFilter{Router: c.router})
			check("LSLinks", len(links), err)
			pfx, err := q.LSPrefixes(ctx, LSPrefixFilter{Router: c.router})
			check("LSPrefixes", len(pfx), err)

			peers, err := q.Peers(ctx, PeerFilter{Router: c.router})
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, p := range peers {
				if p.PeerIP != c.peer {
					continue
				}
				found = true
				if p.State != c.want {
					t.Errorf("Peer.State = %q, want %q", p.State, c.want)
				}
				if p.Routes != want {
					t.Errorf("Peer.Routes = %d, want %d", p.Routes, want)
				}
				// ipv4u has its End-of-RIB; vpn4 and evpn have routes and no
				// marker, so they are still dumping.
				wantDump := map[string]string{"evpn": "dumping", "ipv4u": "complete", "vpn4": "dumping"}
				if want == 0 {
					wantDump = map[string]string{}
				}
				if fmt.Sprint(p.DumpStates) != fmt.Sprint(wantDump) {
					t.Errorf("Peer.DumpStates = %v, want %v", p.DumpStates, wantDump)
				}
			}
			if !found {
				t.Errorf("Peers returned no row for %s", c.peer)
			}
		})
	}
	// The down peer beside srv-fresh's up one: its /25 must not be served.
	routes, err := q.Routes(ctx, RouteFilter{Prefix: "10.252.1.128/25", Router: cases[0].router})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Errorf("a down peer's route was served: %+v", routes)
	}
	// And its link-state objects, one per gated link-state statement.
	down := netip.MustParseAddr("10.252.1.3")
	nodes, err := q.LSNodes(ctx, LSNodeFilter{Router: cases[0].router, Peer: down})
	if err != nil {
		t.Fatal(err)
	}
	links, err := q.LSLinks(ctx, LSLinkFilter{Router: cases[0].router, Peer: down})
	if err != nil {
		t.Fatal(err)
	}
	pfx, err := q.LSPrefixes(ctx, LSPrefixFilter{Router: cases[0].router, Peer: down})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes)+len(links)+len(pfx) != 0 {
		t.Errorf("a down peer's link state was served: %d nodes, %d links, %d prefixes",
			len(nodes), len(links), len(pfx))
	}
}

// TestStalePairsIsExactlyTheStaleSessions: the set the API warns from holds
// the stale collector's session and nothing else. The fresh one is not in it,
// and neither are the two epoch-lost ones -- their routes are not served at
// all, so no answer can carry them -- including srv-lost-silent, which is
// silent too and is kept out by the epoch exclusion alone.
func TestStalePairsIsExactlyTheStaleSessions(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	cases := insertServingFixture(t, ctx, q)
	set, err := q.StalePairs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got, want := set.Has(c.collector, c.router), c.want == "stale"; got != want {
			t.Errorf("StalePairs has (%s, %s) = %v, want %v", c.collector, c.router, got, want)
		}
		if got, want := set.AnyRouter(c.router), c.want == "stale"; got != want {
			t.Errorf("StalePairs.AnyRouter(%s) = %v, want %v", c.router, got, want)
		}
		mapped := netip.AddrFrom16(c.router.As16())
		if set.Has(c.collector, mapped) != set.Has(c.collector, c.router) {
			t.Errorf("Has disagrees with itself over the IPv4-mapped form of %s", c.router)
		}
		if set.AnyRouter(mapped) != set.AnyRouter(c.router) {
			t.Errorf("AnyRouter disagrees with itself over the IPv4-mapped form of %s", c.router)
		}
	}
	if !set.AnyRouter(netip.Addr{}) {
		t.Error("AnyRouter with no router = false, but the set holds a stale session")
	}
	var empty StaleSet
	if empty.AnyRouter(netip.Addr{}) || empty.Has(cases[1].collector, cases[1].router) {
		t.Error("an empty StaleSet reports a stale session")
	}
}

// TestStalePairsHoldsOnlySessionsThatServeRows: four collectors, all silent
// for two hours, each with its own router. Only the two whose session has a
// peer stored up -- and so a peer that reads stale and rows that are served
// -- are in the set. One whose only peer is down, or view_lost, serves
// nothing, and in the set it would flag every answer about its router until
// someone purged it. The mixed row, down beside up, is the one that fails if
// the condition asks for every peer to be up rather than any.
func TestStalePairsHoldsOnlySessionsThatServeRows(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	const prefix = chtest.LivenessPrefix + "sp-"
	cases := []struct {
		name  string
		kinds []string
		want  bool
	}{
		{"down-only", []string{"down"}, false},
		{"view-lost-only", []string{"view_lost"}, false},
		{"up", []string{"up"}, true},
		{"mixed", []string{"down", "up"}, true},
	}
	for i, c := range cases {
		collector := prefix + c.name
		router := fmt.Sprintf("10.253.%d.1", i+1)
		sid := livenessSID(900 + i)
		for j, kind := range c.kinds {
			livenessPeer(t, ctx, q, collector, router, fmt.Sprintf("10.253.%d.%d", i+1, j+2), sid, kind)
		}
		insertBeat(t, ctx, q, collector, time.Unix(0, int64(sid)).Add(-time.Second), time.Now(), beatOld)
	}
	set, err := q.StalePairs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		router := netip.MustParseAddr(fmt.Sprintf("10.253.%d.1", i+1))
		if got := set.Has(prefix+c.name, router); got != c.want {
			t.Errorf("StalePairs has %s's session (peers %v) = %v, want %v", c.name, c.kinds, got, c.want)
		}
		if got := set.AnyRouter(router); got != c.want {
			t.Errorf("StalePairs.AnyRouter(%s) for %s = %v, want %v", router, c.name, got, c.want)
		}
	}
}
