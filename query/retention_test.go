package query

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// retentionTestDB is this test's own database, not testDB. The test expires
// history the way retention does, with OPTIMIZE ... FINAL on every history
// table, and it merges every current table the same way. On testDB that
// would also collapse the duplicates other fixtures write on purpose, and a
// current table has no partition to confine a merge to. A database this
// test alone writes lets it run the retention path whole.
const retentionTestDB = "vantage_query_retention_test"

// The retention fixture's one collector, router, peer and session.
const (
	retentionCollector = "retention-test-collector"
	retentionRouter    = "10.89.0.1"
	retentionPeer      = "10.89.0.2"
	retentionSysname   = "retention-router"
	retentionSession   = 8900
	retentionPeerASN   = 65890
	retentionRIB       = "in_pre"
)

// retentionExpiredAt anchors every fixture row. It is years past the 90-day
// history TTL, and a fixed instant so the expected timestamps below are
// exact.
var retentionExpiredAt = time.Date(2019, 6, 15, 12, 0, 0, 0, time.UTC)

// retentionAt is the timestamp of the row written at seq: one second per
// seq, so every row has its own instant and the newest is the last written.
func retentionAt(seq uint64) time.Time {
	return retentionExpiredAt.Add(time.Duration(seq) * time.Second)
}

// retentionUnicast is the fixture's ten unicast prefixes, seq 2 to 11. The
// last one is announced on a path of its own and then withdrawn at seq 12,
// so nine are live.
var retentionUnicast = []string{
	"10.89.1.0/24", "10.89.2.0/24", "10.89.3.0/24", "10.89.4.0/24", "10.89.5.0/24",
	"10.89.6.0/24", "10.89.7.0/24", "10.89.8.0/24", "10.89.9.0/24", "10.89.10.0/24",
}

const (
	retentionWithdrawn = "10.89.10.0/24"
	retentionVPNPrefix = "10.89.20.0/24"
	retentionVPNRD     = "65890:1"
	retentionEVPNMAC   = "00:89:00:00:00:01"
	retentionEVPNRD    = "65890:2"
	retentionLSRouter  = "0a590001"

	// The Loc-RIB route is announced by a peer of its own, so it adds
	// nothing to the in_pre peer's route count or topology.
	retentionLocRIBPeer   = "10.89.0.3"
	retentionLocRIBPrefix = "10.89.40.0/24"

	// The seq of the fixture's newest row, the LS node. Every other row is
	// older.
	retentionNewestSeq = 20
)

// The AS each family's live routes go through after the peer's own AS, so
// each family draws one edge of its own, and the AS the withdrawn route's
// path went through.
const (
	retentionUnicastAS   = 65891
	retentionVPNAS       = 65892
	retentionEVPNAS      = 65893
	retentionWithdrawnAS = 65899
)

// retentionHistoryTables are the ten tables with a TTL, all of which
// carry collector_id.
var retentionHistoryTables = []string{
	"peer_events", "route_unicast", "route_vpn", "route_evpn", "eor_events",
	"ls_events", "ls_nodes", "ls_links", "ls_prefixes", "stats_events",
}

// retentionCurrentTables are the eight tables without one.
var retentionCurrentTables = []string{
	"peer_current", "eor_current", "route_unicast_current", "route_vpn_current",
	"route_evpn_current", "ls_nodes_current", "ls_links_current", "ls_prefixes_current",
}

// seedRetentionFixture writes one router's whole state, every row stamped
// years ago, then runs retention: OPTIMIZE ... FINAL on each history table
// applies its TTL and deletes every row, and the same on each current table
// merges it (it has no TTL, so nothing leaves). Written rows, by seq:
//
//	1       peer up
//	2-10    unicast 10.89.1.0/24 .. 10.89.9.0/24     65890 65891
//	11      unicast 10.89.10.0/24                    65890 65899
//	12      10.89.10.0/24 withdrawn
//	13      vpn4 65890:1 10.89.20.0/24               65890 65892
//	14      evpn type 2, 65890:2, 00:89:00:00:00:01  65890 65893
//	15      End-of-RIB, ipv4u
//	16      End-of-RIB, vpn4
//	17      End-of-RIB, evpn
//	18      loc_rib unicast 10.89.40.0/24 from 10.89.0.3  65890
//	19      LS End-of-RIB (ls_events, end_of_rib = 1)
//	20      LS node 0a590001
//
// It checks the premise before any assertion relies on it: history holds
// none of these rows, and every current table the fixture feeds still does.
func seedRetentionFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	router, peer := netip.MustParseAddr(retentionRouter), netip.MustParseAddr(retentionPeer)

	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
		PeerIP: retentionPeer, RIB: retentionRIB, PeerASN: retentionPeerASN, PeerBGPID: retentionPeer,
		SessionID: retentionSession, Seq: 1, StreamSeq: 1, Kind: "up",
		TsRouter: retentionAt(1), TsCollector: retentionAt(1),
	})

	unicast := func(seq uint64, prefix string, path []uint32, withdraw uint8) {
		insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
			Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
			PeerIP: retentionPeer, RIB: retentionRIB, Prefix: prefix, NextHop: retentionPeer,
			PeerASN: retentionPeerASN, PeerBGPID: retentionPeer,
			SessionID: retentionSession, Seq: seq, StreamSeq: seq,
			IsWithdraw: withdraw, ASPath: path,
			TsRouter: retentionAt(seq), TsCollector: retentionAt(seq),
		})
	}
	for i, prefix := range retentionUnicast {
		path := []uint32{retentionPeerASN, retentionUnicastAS}
		if prefix == retentionWithdrawn {
			path = []uint32{retentionPeerASN, retentionWithdrawnAS}
		}
		unicast(uint64(i+2), prefix, path, 0)
	}
	unicast(12, retentionWithdrawn, nil, 1)

	insertRouteVPNEvent(t, ctx, q, routeVPNFixture{
		Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
		PeerIP: retentionPeer, RIB: retentionRIB, Family: "vpn4", Prefix: retentionVPNPrefix,
		RD: retentionVPNRD, NextHop: retentionPeer, PeerASN: retentionPeerASN, PeerBGPID: retentionPeer,
		SessionID: retentionSession, Seq: 13, StreamSeq: 13, Labels: []uint32{100},
		ASPath:   []uint32{retentionPeerASN, retentionVPNAS},
		TsRouter: retentionAt(13), TsCollector: retentionAt(13),
	})
	insertRouteEVPNEvent(t, ctx, q, routeEVPNFixture{
		Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
		PeerIP: retentionPeer, RIB: retentionRIB, RouteType: 2, RD: retentionEVPNRD,
		MAC: retentionEVPNMAC, IP: "10.89.30.1", NextHop: retentionPeer,
		PeerASN: retentionPeerASN, PeerBGPID: retentionPeer,
		SessionID: retentionSession, Seq: 14, StreamSeq: 14, Labels: []uint32{200},
		ASPath:   []uint32{retentionPeerASN, retentionEVPNAS},
		TsRouter: retentionAt(14), TsCollector: retentionAt(14),
	})
	// One End-of-RIB per family, so every family's routes read a finished
	// dump, not only the one peers.go's own dump map reads.
	for i, family := range []string{"ipv4u", "vpn4", evpnFamily} {
		seq := uint64(15 + i)
		insertEorEvent(t, ctx, q, eorFixture{
			Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
			PeerIP: retentionPeer, RIB: retentionRIB, Family: family,
			PeerASN: retentionPeerASN, PeerBGPID: retentionPeer,
			SessionID: retentionSession, Seq: seq, StreamSeq: seq,
			TsRouter: retentionAt(seq), TsCollector: retentionAt(seq),
		})
	}

	insertRouteUnicastEvent(t, ctx, q, routeUnicastFixture{
		Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
		PeerIP: retentionLocRIBPeer, RIB: "loc_rib", Prefix: retentionLocRIBPrefix,
		NextHop: retentionLocRIBPeer, PeerASN: retentionPeerASN, PeerBGPID: retentionLocRIBPeer,
		SessionID: retentionSession, Seq: 18, StreamSeq: 18,
		ASPath:   []uint32{retentionPeerASN},
		TsRouter: retentionAt(18), TsCollector: retentionAt(18),
	})

	// The link-state End-of-RIB reaches eor_current through ls_events, not
	// eor_events: eor_current_ls_mv copies it as family 'ls'.
	events, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_events")
	if err != nil {
		t.Fatalf("prepare ls_events: %v", err)
	}
	if err := events.Append(
		retentionCollector, router, retentionSysname, peer, retentionRIB, uint32(retentionPeerASN),
		peer, uint64(retentionSession), uint64(19),
		retentionAt(19), retentionAt(19), []string{}, uint64(19),
		"ls", "", "", uint8(1),
	); err != nil {
		t.Fatalf("append ls_events: %v", err)
	}
	if err := events.Send(); err != nil {
		t.Fatalf("send ls_events: %v", err)
	}

	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	if err := nodes.Append(
		retentionCollector, router, retentionSysname, peer, retentionRIB, uint32(retentionPeerASN),
		peer, uint64(retentionSession), uint64(retentionNewestSeq),
		retentionAt(retentionNewestSeq), retentionAt(retentionNewestSeq), []string{}, uint64(retentionNewestSeq),
		uint8(2), uint64(0), uint32(retentionPeerASN), uint32(0), uint32(0), retentionLSRouter,
		"", uint8(0), "retention-ls-node",
		uint32(16000), uint32(8000), uint32(15000), uint32(1000),
		[]uint8{0}, map[uint16]string{},
	); err != nil {
		t.Fatalf("append ls_nodes: %v", err)
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}

	for _, tbl := range append(slices.Clone(retentionHistoryTables), retentionCurrentTables...) {
		if err := q.conn.Exec(ctx, "OPTIMIZE TABLE "+q.db+"."+tbl+" FINAL"); err != nil {
			t.Fatalf("optimize %s: %v", tbl, err)
		}
	}

	count := func(query string) uint64 {
		t.Helper()
		var n uint64
		if err := q.conn.QueryRow(ctx, query, retentionCollector).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	for _, tbl := range retentionHistoryTables {
		if n := count("SELECT count() FROM " + q.db + "." + tbl + " WHERE collector_id = ?"); n != 0 {
			t.Fatalf("after OPTIMIZE ... FINAL %s still holds %d of the fixture's rows, "+
				"want 0: the fixture does not describe expired history, so nothing "+
				"below says anything about retention", tbl, n)
		}
	}
	for _, c := range []struct {
		tbl, expr string
		want      uint64
	}{
		{"peer_current", "count()", 1},
		{"eor_current", "count()", 4},
		{"route_unicast_current", "uniqExact(prefix)", uint64(len(retentionUnicast)) + 1},
		{"route_vpn_current", "count()", 1},
		{"route_evpn_current", "count()", 1},
		{"ls_nodes_current", "count()", 1},
	} {
		if n := count("SELECT " + c.expr + " FROM " + q.db + "." + c.tbl + " WHERE collector_id = ?"); n != c.want {
			t.Fatalf("after OPTIMIZE ... FINAL %s holds %d of the fixture's rows, want %d",
				c.tbl, n, c.want)
		}
	}
}

// TestStateOlderThanRetentionIsStillCurrent is the bug the current tables
// fix. BMP sends only changes, so a session, a route or a dump marker that
// has not changed in longer than the history TTL is still live, and every
// row that says so has aged out of history. Every current-state read must
// still answer from the current tables: the router is listed, its peer is
// up with a complete dump and counts its live routes, its routes in all
// three families are returned by both the lookups and the RIB walks, read a
// finished dump and are drawn on the topology, its LS node is listed with a
// finished dump, its collector is still listed with its real newest row,
// and the Loc-RIB comparison still counts the archived Loc-RIB route.
//
// A withdrawn route stays withdrawn: its current row is the withdrawal
// alone, and the path it was withdrawn from left with history, so it is
// neither returned nor drawn.
func TestStateOlderThanRetentionIsStillCurrent(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, retentionTestDB)
	q, err := New(conn, retentionTestDB)
	if err != nil {
		t.Fatalf("New(%s): %v", retentionTestDB, err)
	}
	seedRetentionFixture(t, ctx, q)
	router, peer := netip.MustParseAddr(retentionRouter), netip.MustParseAddr(retentionPeer)
	live := retentionUnicast[:len(retentionUnicast)-1]

	t.Run("Routers", func(t *testing.T) {
		got, err := q.Routers(ctx)
		if err != nil {
			t.Fatalf("Routers: %v", err)
		}
		for _, r := range got {
			if r.IP == router && r.Collector == retentionCollector {
				if r.PeersUp != 1 || r.SessionID != retentionSession {
					t.Errorf("router %s = (%d peers up, session %d), want (1, %d)",
						router, r.PeersUp, r.SessionID, retentionSession)
				}
				return
			}
		}
		t.Fatalf("Routers does not list %s: %+v", router, got)
	})

	t.Run("Peers", func(t *testing.T) {
		got, err := q.Peers(ctx, PeerFilter{Router: router})
		if err != nil {
			t.Fatalf("Peers: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("Peers = %d rows, want 1: %+v", len(got), got)
		}
		p := got[0]
		if p.PeerIP != peer || p.State != "up" || p.DumpStates["ipv4u"] != "complete" {
			t.Errorf("peer = (%s, %q, ipv4u %q), want (%s, \"up\", \"complete\")",
				p.PeerIP, p.State, p.DumpStates["ipv4u"], peer)
		}
		// A stable peer's route count is its live routes, all of which are
		// older than retention: route_counts must count them from
		// route_unicast_current.
		if p.Routes != len(live) {
			t.Errorf("peer Routes = %d, want %d, its live unicast routes", p.Routes, len(live))
		}
	})

	t.Run("Routes", func(t *testing.T) {
		for _, prefix := range retentionUnicast {
			got, err := q.Routes(ctx, RouteFilter{Prefix: prefix, Router: router})
			if err != nil {
				t.Fatalf("Routes(%s): %v", prefix, err)
			}
			want := 1
			if prefix == retentionWithdrawn {
				want = 0
			}
			if len(got) != want {
				t.Errorf("Routes(%s) = %d rows, want %d: %+v", prefix, len(got), want, got)
				continue
			}
			// The dump finished years ago, and its End-of-RIB left
			// history with everything else: it must still read
			// finished, not "dumping".
			if want == 1 && got[0].DumpState != "complete" {
				t.Errorf("Routes(%s) DumpState = %q, want \"complete\"", prefix, got[0].DumpState)
			}
		}
	})

	t.Run("VPNRoutes", func(t *testing.T) {
		got, err := q.VPNRoutes(ctx, VPNRouteFilter{Router: router})
		if err != nil {
			t.Fatalf("VPNRoutes: %v", err)
		}
		if len(got) != 1 || got[0].Prefix != retentionVPNPrefix || got[0].RD != retentionVPNRD {
			t.Fatalf("VPNRoutes = %+v, want the one route %s %s", got, retentionVPNRD, retentionVPNPrefix)
		}
		if got[0].DumpState != "complete" {
			t.Errorf("VPNRoutes DumpState = %q, want \"complete\"", got[0].DumpState)
		}
	})

	t.Run("EVPNRoutes", func(t *testing.T) {
		got, err := q.EVPNRoutes(ctx, EVPNRouteFilter{Router: router})
		if err != nil {
			t.Fatalf("EVPNRoutes: %v", err)
		}
		if len(got) != 1 || got[0].MAC != retentionEVPNMAC || got[0].RD != retentionEVPNRD {
			t.Fatalf("EVPNRoutes = %+v, want the one route %s %s", got, retentionEVPNRD, retentionEVPNMAC)
		}
		if got[0].DumpState != "complete" {
			t.Errorf("EVPNRoutes DumpState = %q, want \"complete\"", got[0].DumpState)
		}
	})

	// Limit 4 over nine routes walks three pages, so the session pin is
	// carried across a cursor, not only resolved once.
	t.Run("RIBPageUnicast", func(t *testing.T) {
		pages, _ := walkRIB(t, func(cur *RIBCursor) ([]Route, *RIBCursor, error) {
			return q.RIBPageUnicast(ctx, router, peer, retentionRIB, cur, 4)
		})
		var got []string
		for _, page := range pages {
			for _, r := range page {
				got = append(got, r.Prefix)
			}
		}
		slices.Sort(got)
		want := slices.Clone(live)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("RIBPageUnicast walked %v, want %v", got, want)
		}
	})

	t.Run("RIBPageVPN", func(t *testing.T) {
		rows, next, err := q.RIBPageVPN(ctx, router, peer, retentionRIB, nil, 100)
		if err != nil {
			t.Fatalf("RIBPageVPN: %v", err)
		}
		if next != nil || len(rows) != 1 || rows[0].Prefix != retentionVPNPrefix {
			t.Errorf("RIBPageVPN = %+v (next %v), want the one route %s", rows, next, retentionVPNPrefix)
		}
	})

	t.Run("RIBPageEVPN", func(t *testing.T) {
		rows, next, err := q.RIBPageEVPN(ctx, router, peer, retentionRIB, nil, 100)
		if err != nil {
			t.Fatalf("RIBPageEVPN: %v", err)
		}
		if next != nil || len(rows) != 1 || rows[0].MAC != retentionEVPNMAC {
			t.Errorf("RIBPageEVPN = %+v (next %v), want the one route %s", rows, next, retentionEVPNMAC)
		}
	})

	t.Run("TopologyUnicast", func(t *testing.T) {
		g, err := q.TopologyUnicast(ctx, RouteFilter{Router: router, Peer: peer})
		if err != nil {
			t.Fatalf("TopologyUnicast: %v", err)
		}
		e := edgeIn(t, g, retentionPeerASN, retentionUnicastAS)
		if e.Routes != uint64(len(live)) || e.LiveRoutes != uint64(len(live)) {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (%d, %d)",
				retentionPeerASN, retentionUnicastAS, e.Routes, e.LiveRoutes, len(live), len(live))
		}
		// The withdrawn route's path left with its history.
		edgeAbsent(t, g, retentionPeerASN, retentionWithdrawnAS)
	})

	t.Run("TopologyVPN", func(t *testing.T) {
		g, err := q.TopologyVPN(ctx, VPNRouteFilter{Router: router, Peer: peer})
		if err != nil {
			t.Fatalf("TopologyVPN: %v", err)
		}
		if e := edgeIn(t, g, retentionPeerASN, retentionVPNAS); e.Routes != 1 || e.LiveRoutes != 1 {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (1, 1)",
				retentionPeerASN, retentionVPNAS, e.Routes, e.LiveRoutes)
		}
	})

	t.Run("TopologyEVPN", func(t *testing.T) {
		g, err := q.TopologyEVPN(ctx, EVPNRouteFilter{Router: router, Peer: peer})
		if err != nil {
			t.Fatalf("TopologyEVPN: %v", err)
		}
		if e := edgeIn(t, g, retentionPeerASN, retentionEVPNAS); e.Routes != 1 || e.LiveRoutes != 1 {
			t.Errorf("edge %d->%d = (%d routes, %d live), want (1, 1)",
				retentionPeerASN, retentionEVPNAS, e.Routes, e.LiveRoutes)
		}
	})

	t.Run("LSNodes", func(t *testing.T) {
		got, err := q.LSNodes(ctx, LSNodeFilter{Router: router})
		if err != nil {
			t.Fatalf("LSNodes: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("LSNodes = %d rows, want 1: %+v", len(got), got)
		}
		// The link-state End-of-RIB is as old as the rest: the node's dump
		// still reads finished.
		if got[0].DumpState != "complete" {
			t.Errorf("LSNodes DumpState = %q, want \"complete\"", got[0].DumpState)
		}
	})

	// LastRowAt is the newest row the collector ever wrote, which is old:
	// history no longer holds it, and the current tables do.
	t.Run("Collectors", func(t *testing.T) {
		got, err := q.Collectors(ctx)
		if err != nil {
			t.Fatalf("Collectors: %v", err)
		}
		c := findCollector(t, got, retentionCollector)
		findCollectorRouter(t, c, retentionSysname)
		if want := retentionAt(retentionNewestSeq); !c.LastRowAt.Equal(want) {
			t.Errorf("LastRowAt = %s, want %s, the LS node's ts_collector",
				c.LastRowAt.UTC(), want)
		}
		if c.PeersUp != 1 {
			t.Errorf("PeersUp = %d, want 1", c.PeersUp)
		}
	})

	// A router's Stats Reports are periodic, so its newest Loc-RIB count is
	// recent even when the Loc-RIB itself has not changed in longer than
	// retention. The archived side must still count the old route, or the
	// comparison reads the whole Loc-RIB as missing.
	//
	// This subtest writes the fixture's one fresh row, so it runs last:
	// Collectors above asserts the collector's newest row is an old one.
	t.Run("LocRIBComparison", func(t *testing.T) {
		now := time.Now().UTC()
		insertStatsEvent(t, ctx, q, statsEventFixture{
			Collector: retentionCollector, RouterIP: retentionRouter, RouterSysname: retentionSysname,
			PeerIP: retentionLocRIBPeer, RIB: "loc_rib", PeerASN: retentionPeerASN,
			PeerBGPID: retentionLocRIBPeer, SessionID: retentionSession,
			Seq: 21, StreamSeq: 21, Counters: map[uint32]uint64{8: 1},
			TsRouter: now, TsCollector: now,
		})
		got, _, err := q.LocRIBComparison(ctx, CollectionFilter{Router: router})
		if err != nil {
			t.Fatalf("LocRIBComparison: %v", err)
		}
		locPeer := netip.MustParseAddr(retentionLocRIBPeer)
		if len(got) != 1 || got[0].PeerIP != locPeer {
			t.Fatalf("LocRIBComparison = %+v, want one row for peer %s", got, locPeer)
		}
		if r := got[0]; r.Reported != 1 || r.Archived != 1 || !r.HasStat {
			t.Errorf("LocRIBComparison = (reported %d, archived %d, has stat %t), want (1, 1, true)",
				r.Reported, r.Archived, r.HasStat)
		}
	})
}

// retentionDaysTestDB is TestRetentionDaysReadsTheTableTTL's own database:
// the test rewrites route_unicast's TTL, which no other test's fixture may
// see.
const retentionDaysTestDB = "vantage_query_retention_days_test"

// TestRetentionDaysReadsTheTableTTL reads the history retention from the
// table itself, not from configuration: the shipped schema keeps 90 days,
// and a TTL changed in place is reported as changed. A TTL that is not a
// whole number of days is reported as ErrRetentionNotInDays rather than
// guessed at.
func TestRetentionDaysReadsTheTableTTL(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, retentionDaysTestDB)
	q, err := New(conn, retentionDaysTestDB)
	if err != nil {
		t.Fatalf("New(%s): %v", retentionDaysTestDB, err)
	}
	modify := func(interval string) {
		t.Helper()
		stmt := "ALTER TABLE " + retentionDaysTestDB + ".route_unicast MODIFY TTL " +
			"toDateTime(ts_collector) + " + interval +
			" SETTINGS materialize_ttl_after_modify = 0"
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if got, err := q.RetentionDays(ctx); err != nil || got != 90 {
		t.Fatalf("RetentionDays on the shipped schema = (%d, %v), want (90, nil)", got, err)
	}
	modify("INTERVAL 30 DAY")
	if got, err := q.RetentionDays(ctx); err != nil || got != 30 {
		t.Fatalf("RetentionDays after MODIFY TTL ... 30 DAY = (%d, %v), want (30, nil)", got, err)
	}
	modify("INTERVAL 3 MONTH")
	if got, err := q.RetentionDays(ctx); !errors.Is(err, ErrRetentionNotInDays) {
		t.Fatalf("RetentionDays after MODIFY TTL ... 3 MONTH = (%d, %v), want ErrRetentionNotInDays", got, err)
	}
}
