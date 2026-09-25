package query

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// updateParity rewrites the parity snapshots instead of comparing against
// them. Run it only on query code whose answers are known to be right: a
// snapshot records what the code says, not what it should say.
var updateParity = flag.Bool("update", false, "rewrite parity snapshots")

// The parity fixture's own collectors, routers and peers. Everything sits in
// 10.250.0.0/16 (routers 10.250.1.x, peers 10.250.2.x), under two collector
// names no other fixture writes, so that every query below can be scoped to
// rows this fixture alone owns. chtest shares one database across every test
// in this package, and an unscoped answer would change whenever another
// fixture did.
const (
	parityC1 = "parity-c1"
	parityC2 = "parity-c2"

	// parityR1 is the route router: one collector, a superseded and a
	// current session, every route family.
	parityR1 = "10.250.1.1"
	// parityR2 is dual-homed: both collectors watch it, each with its own
	// superseded and current session.
	parityR2 = "10.250.1.2"
	// parityR3 is the link-state router.
	parityR3 = "10.250.1.3"

	parityP1 = "10.250.2.1" // R1: flaps inside the current session and ends up; carries every family.
	parityP2 = "10.250.2.2" // R1: up, then down, in the current session.
	parityP3 = "10.250.2.3" // R1: in_pre and loc_rib; the Loc-RIB comparison's peer.
	parityP4 = "10.250.2.4" // R2: up on both collectors.
	parityP5 = "10.250.2.5" // R2: up on collector 2's superseded session only.
	parityP6 = "10.250.2.6" // R3: link-state, with an End-of-RIB.
	parityP7 = "10.250.2.7" // R3: link-state, no End-of-RIB in the current session.
	parityP8 = "10.250.2.8" // R1: up, then view_lost, in the current session.

	// Session ids: the superseded session of each (collector, router) is one
	// below its current one. Current-state answers must come from the higher.
	paritySessR1Old   = 25001
	paritySessR1Cur   = 25002
	paritySessR2C1Old = 25100
	paritySessR2C1Cur = 25101
	paritySessR2C2Old = 25200
	paritySessR2C2Cur = 25201
	paritySessR3Old   = 25300
	paritySessR3Cur   = 25301

	// paritySupersededSeq is the seq every superseded-session row carries:
	// higher than any current-session seq, as it often is in real data,
	// since seq restarts with each session. See insertParityFixture.
	paritySupersededSeq = 900

	parityLSASN = uint32(65250)
	// Link-state router_ids. The fleet-wide label tier is keyed by node_key
	// across every router in the database, so these are ids no other fixture
	// writes; a shared id would let another test's node name a parity link.
	parityLSNodeA = "0afa00a1"
	parityLSNodeB = "0afa00b2"
	parityLSNodeC = "0afa00c3" // announced, then withdrawn
	parityLSNodeD = "0afa00d4" // superseded session only
	parityLSNodeE = "0afa00e5" // named by P7 only
	parityLSNodeF = "0afa00f6" // named by nobody
)

// parityBase is the fixture's fixed clock. Every timestamp is parityBase
// plus the row's stream_seq in seconds, so no snapshot holds the time the
// test ran. It is in the future on purpose: the history tables carry a
// 90-day TTL on ts_collector, and a fixed past date would let a background
// merge expire the fixture's history rows some months after the snapshot was
// taken, changing the answers of queries that still read history. The
// same TTL reaches these dates around 2030-04-01: before then, move
// parityBase forward and regenerate the snapshots (-update) on code whose
// answers are otherwise unchanged.
var parityBase = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// parityWriter hands every row the fixture writes its own stream_seq and a
// timestamp derived from it. A unique stream_seq per row is what keeps the
// history tables' ReplacingMergeTree from collapsing two fixture rows that
// happen to share the rest of their sort key (collector_id is not in it).
// seq is set by each call, independently: several rows below are written in
// an order that deliberately disagrees with their seq.
type parityWriter struct {
	t   *testing.T
	ctx context.Context
	q   *Q
	n   uint64
}

func (w *parityWriter) next() (uint64, time.Time) {
	w.n++
	return w.n, parityBase.Add(time.Duration(w.n) * time.Second)
}

func (w *parityWriter) peer(f peerEventFixture) {
	w.t.Helper()
	f.StreamSeq, f.TsRouter = w.next()
	f.TsCollector = f.TsRouter
	if f.PeerBGPID == "" {
		f.PeerBGPID = f.PeerIP
	}
	insertPeerEvent(w.t, w.ctx, w.q, f)
}

func (w *parityWriter) unicast(f routeUnicastFixture) {
	w.t.Helper()
	f.StreamSeq, f.TsRouter = w.next()
	f.TsCollector = f.TsRouter
	if f.PeerBGPID == "" {
		f.PeerBGPID = f.PeerIP
	}
	insertRouteUnicastEvent(w.t, w.ctx, w.q, f)
}

func (w *parityWriter) vpn(f routeVPNFixture) {
	w.t.Helper()
	f.StreamSeq, f.TsRouter = w.next()
	f.TsCollector = f.TsRouter
	if f.PeerBGPID == "" {
		f.PeerBGPID = f.PeerIP
	}
	insertRouteVPNEvent(w.t, w.ctx, w.q, f)
}

func (w *parityWriter) evpn(f routeEVPNFixture) {
	w.t.Helper()
	f.StreamSeq, f.TsRouter = w.next()
	f.TsCollector = f.TsRouter
	if f.PeerBGPID == "" {
		f.PeerBGPID = f.PeerIP
	}
	insertRouteEVPNEvent(w.t, w.ctx, w.q, f)
}

func (w *parityWriter) eor(f eorFixture) {
	w.t.Helper()
	f.StreamSeq, f.TsRouter = w.next()
	f.TsCollector = f.TsRouter
	if f.PeerBGPID == "" {
		f.PeerBGPID = f.PeerIP
	}
	insertEorEvent(w.t, w.ctx, w.q, f)
}

func (w *parityWriter) stats(f statsEventFixture) {
	w.t.Helper()
	f.StreamSeq, f.TsRouter = w.next()
	f.TsCollector = f.TsRouter
	if f.PeerBGPID == "" {
		f.PeerBGPID = f.PeerIP
	}
	insertStatsEvent(w.t, w.ctx, w.q, f)
}

// append writes one row to table in the column order the caller gives,
// which is the table's own: the link-state tables have no fixture struct in
// this package, and these rows mirror insertLSNodeFixture's,
// insertLSLinkFixture's, insertLSPrefixFixture's and
// insertLSDumpStateFixture's positional appends.
func (w *parityWriter) append(table string, row ...any) {
	w.t.Helper()
	b, err := w.q.conn.PrepareBatch(w.ctx, "INSERT INTO "+w.q.db+"."+table)
	if err != nil {
		w.t.Fatalf("prepare %s: %v", table, err)
	}
	if err := b.Append(row...); err != nil {
		w.t.Fatalf("append %s: %v", table, err)
	}
	if err := b.Send(); err != nil {
		w.t.Fatalf("send %s: %v", table, err)
	}
}

// lsEnvelope is the thirteen envelope columns every link-state table
// starts with, for R3's peer on the given session.
func (w *parityWriter) lsEnvelope(peer string, session, seq uint64) []any {
	ss, ts := w.next()
	return []any{
		parityC1, netip.MustParseAddr(parityR3), "parity-r3",
		netip.MustParseAddr(peer), "in_pre", parityLSASN,
		netip.MustParseAddr(parityR3), session, seq,
		ts, ts, []string{}, ss,
	}
}

func (w *parityWriter) lsNode(peer string, session, seq uint64, routerID, r4, name string, withdraw uint8) {
	w.t.Helper()
	row := append(w.lsEnvelope(peer, session, seq),
		uint8(2), uint64(0), parityLSASN, uint32(0), uint32(0), routerID,
		r4, withdraw, name,
		uint32(16000), uint32(8000), uint32(15000), uint32(1000),
		[]uint8{0}, map[uint16]string{},
	)
	w.append("ls_nodes", row...)
}

func (w *parityWriter) lsLink(peer string, session, seq uint64, local, remote, localIf, remoteIf string,
	localID, remoteID uint32, withdraw uint8) {
	w.t.Helper()
	row := append(w.lsEnvelope(peer, session, seq),
		uint8(2), uint64(0),
		parityLSASN, uint32(0), uint32(0), local,
		parityLSASN, uint32(0), uint32(0), remote,
		localIf, remoteIf, localID, remoteID,
		// local_node_key and remote_node_key are MATERIALIZED and are not
		// appended; see insertLSLinkFixture.
		withdraw,
		[]uint32{24001}, []uint8{0x30}, []uint8{0},
		uint32(10), uint32(20), uint32(0), float32(1e9),
		map[uint16]string{},
	)
	w.append("ls_links", row...)
}

func (w *parityWriter) lsPrefix(peer string, session, seq uint64, routerID, addr string, length uint8, withdraw uint8) {
	w.t.Helper()
	row := append(w.lsEnvelope(peer, session, seq),
		uint8(2), uint64(0), parityLSASN, uint32(0), uint32(0), routerID,
		addr, length,
		withdraw, uint32(16001), uint8(0), uint8(1), uint32(10), uint8(0), uint8(0),
		map[uint16]string{},
	)
	w.append("ls_prefixes", row...)
}

func (w *parityWriter) lsEndOfRIB(peer string, session, seq uint64) {
	w.t.Helper()
	row := append(w.lsEnvelope(peer, session, seq), "ls", "", "", uint8(1))
	w.append("ls_events", row...)
}

func u32(v uint32) *uint32 { return &v }

// insertParityFixture writes one database's worth of every shape a
// current-state query has to resolve, under parity-only routers and
// collectors. Each case is there because it is a row a current-state
// answer must EXCLUDE or must resolve by (session, seq) rather than by
// arrival order -- the rows that give the snapshot the power to fail:
//
//   - every (collector, router) has a superseded session whose rows carry
//     different attributes (sysname, next hop, AS path, labels, names) for
//     the current session's own keys, announce keys the current session
//     withdrew, or hold objects the current session lacks -- all at a
//     HIGHER seq than the current session's rows, so that resolving
//     "newest" by (seq, stream_seq) across sessions picks them;
//   - P1 flaps inside R1's current session with its events written out of
//     seq order, and its hold time and capabilities sit on an older row than
//     its newest event;
//   - P2 ends down and P8 ends view_lost, each with a route;
//   - per-family End-of-RIB markers leave P1 complete in ipv4u and evpn,
//     dumping in ipv6u, vpn4 and lu4, and R1's superseded-session marker
//     must not count;
//   - R2 is dual-homed, with a peer (P5) only collector 2's superseded
//     session saw;
//   - unicast, VPN and EVPN routes each include an add-path pair, a route
//     withdrawn in the current session, and a re-announcement with a new
//     path;
//   - R3 carries a link-state topology: live, withdrawn and
//     superseded-session nodes, links and prefixes, labels from both the
//     observer and the fleet tier, and one peer with an End-of-RIB and one
//     without;
//   - P3 carries a Loc-RIB and a stats report whose newest counter 8 is
//     lower than an older one.
//
// The existing fixture helpers (insertFlappedPeerFixture,
// insertPerFamilyDumpFixture, insertTwoCollectorFixture, the LS fixtures)
// are not called: each writes under its own hard-coded router IPs, its own
// collector and time.Now() timestamps, and every one of those would put a
// wall-clock value or another test's rows into the snapshot. Their shapes
// are rebuilt here from the same lower-level insert helpers instead.
func insertParityFixture(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	w := &parityWriter{t: t, ctx: ctx, q: q}

	// --- R1, collector 1, superseded session -----------------------------
	//
	// At seq 900 (paritySupersededSeq): seq restarts with every session, so a
	// superseded session's row for a key can carry a higher seq than the
	// current session's. Anything that resolved "newest" by (seq, stream_seq)
	// across sessions -- a query that lost its session join, or a current
	// table whose key lost session_id -- would pick these rows, since seq is
	// compared first, and every one of them differs from the current
	// session's answer.
	//
	// They are written BEFORE the current session, so stream_seq still
	// tracks session order. TestPublishOrderNeverInvertsSessionOrder asserts
	// that premise over every route_unicast row in the shared database, and a
	// superseded row published after the current one would break it.
	const r1Old = "parity-r1-before-reset" // must not win: sysname comes from the current session
	for _, p := range []struct {
		peer, rib string
		asn       uint32
	}{
		{parityP1, "in_pre", 65101},
		// P2 ends the current session down; up here.
		{parityP2, "in_pre", 65102},
		{parityP3, "in_pre", 65103},
		{parityP3, "loc_rib", 65103},
	} {
		w.peer(peerEventFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1Old,
			PeerIP: p.peer, RIB: p.rib, PeerASN: p.asn,
			SessionID: paritySessR1Old, Seq: paritySupersededSeq, Kind: "up",
			HoldTime: 30, HoldTimeSeen: 1, MPFamilies: []string{"ipv4u"},
			SysDescr: "parity router one before reset",
		})
	}
	for _, r := range []struct {
		peer, rib, prefix string
		asn, pathID       uint32
		path              []uint32
	}{
		// The same route keys the current session advertises, with a
		// different next hop and path.
		{parityP1, "in_pre", "198.18.1.0/24", 65101, 0, []uint32{65101, 65999, 65300}},
		{parityP1, "in_pre", "198.18.4.0/24", 65101, 0, []uint32{65101, 65999, 65204}},
		// Keys the current session WITHDREW, announced here: a merge across
		// sessions resurrects them.
		{parityP1, "in_pre", "198.18.2.0/24", 65101, 1, []uint32{65101, 65999, 65201}},
		{parityP1, "in_pre", "198.18.3.0/24", 65101, 0, []uint32{65101, 65999, 65203}},
		// Only the superseded session ever advertised this one.
		{parityP1, "in_pre", "198.18.9.0/24", 65101, 0, []uint32{65101, 65998}},
		// P2's route, live here while P2 was up.
		{parityP2, "in_pre", "198.18.6.0/24", 65102, 0, []uint32{65102, 65999, 65300}},
		// Loc-RIB rows. The Loc-RIB comparison's archived side reads the
		// current session only, so 198.18.13.0/24, which only this
		// superseded session advertised, does NOT count; 198.18.10.0/24 is
		// re-advertised in the current session and counts once.
		{parityP3, "loc_rib", "198.18.10.0/24", 65103, 0, []uint32{65103, 65995}},
		{parityP3, "loc_rib", "198.18.13.0/24", 65103, 0, []uint32{65103, 65995}},
	} {
		w.unicast(routeUnicastFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1Old,
			PeerIP: r.peer, RIB: r.rib, PeerASN: r.asn, Prefix: r.prefix, PathID: r.pathID,
			NextHop: "10.250.9.9", ASPath: r.path,
			SessionID: paritySessR1Old, Seq: paritySupersededSeq,
		})
	}
	// An ipv6u and a vpn4 marker in the superseded session: both must still
	// read "dumping" in the current one, which has no marker for either.
	for _, fam := range []string{"ipv6u", "vpn4"} {
		w.eor(eorFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1Old,
			PeerIP: parityP1, RIB: "in_pre", Family: fam, PeerASN: 65101,
			SessionID: paritySessR1Old, Seq: paritySupersededSeq,
		})
	}
	for _, r := range []struct {
		prefix, rd string
		labels     []uint32
		path       []uint32
	}{
		// Same key as a live current-session route, different label and path.
		{"192.0.2.0/24", "65101:1", []uint32{901}, []uint32{65101, 65997, 65400}},
		// Same key as a route the current session withdrew.
		{"192.0.2.128/25", "65101:1", []uint32{902}, []uint32{65101, 65997, 65404}},
		// Superseded session only.
		{"192.0.2.64/26", "65101:9", []uint32{900}, []uint32{65101, 65997}},
	} {
		w.vpn(routeVPNFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1Old,
			PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101, Family: "vpn4",
			Prefix: r.prefix, RD: r.rd, NextHop: "10.250.9.9",
			Labels: r.labels, ASPath: r.path,
			SessionID: paritySessR1Old, Seq: paritySupersededSeq,
		})
	}
	for _, r := range []struct {
		routeType     uint8
		prefix, mac   string
		ip            string
		ethernetTagID uint32
		path          []uint32
	}{
		// Same key as a live current-session MAC/IP route.
		{2, "", "00:50:79:25:00:01", "10.250.50.1", 10, []uint32{65101, 65996, 65500}},
		// Same key as the MAC/IP route the current session withdrew.
		{2, "", "00:50:79:25:00:02", "10.250.50.2", 10, []uint32{65101, 65996, 65500}},
		// Same key as the current session's type-5 route.
		{5, "203.0.113.0/24", "", "", 0, []uint32{65101, 65996, 65501}},
		// Superseded session only.
		{3, "", "", "10.250.9.9", 0, []uint32{65101, 65996}},
	} {
		w.evpn(routeEVPNFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1Old,
			PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
			RouteType: r.routeType, RD: "65101:10", Prefix: r.prefix, MAC: r.mac, IP: r.ip,
			ESI: evpnFixtureESIZero, EthernetTag: r.ethernetTagID, NextHop: "10.250.9.9",
			Labels: []uint32{10999}, ASPath: r.path,
			SessionID: paritySessR1Old, Seq: paritySupersededSeq,
		})
	}

	// --- R1, collector 1, current session ---------------------------------
	const r1 = "parity-r1"
	// P1 flaps, written out of seq order: the up that ends the session
	// (seq 3) is written FIRST and so carries the earliest stream_seq and
	// timestamp, and the down (seq 2) is written LAST. Resolving by arrival
	// reads down; resolving by seq reads up. The seq-3 up carries the
	// session facts; the seq-2 down carries none (a Peer Down has no OPEN).
	w.peer(peerEventFixture{
		Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
		PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
		SessionID: paritySessR1Cur, Seq: 3, Kind: "up",
		HoldTime: 180, HoldTimeSeen: 1,
		MPFamilies:      []string{"ipv4u", "ipv6u", "vpn4", "lu4", "evpn"},
		AddPathFamilies: []string{"ipv4u"},
		SysDescr:        "parity router one",
	})
	w.peer(peerEventFixture{
		Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
		PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
		SessionID: paritySessR1Cur, Seq: 1, Kind: "up",
		HoldTime: 90, HoldTimeSeen: 1, MPFamilies: []string{"ipv4u"},
	})
	w.peer(peerEventFixture{
		Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
		PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
		SessionID: paritySessR1Cur, Seq: 2, Kind: "down",
	})
	for _, e := range []struct {
		peer, rib, kind string
		asn             uint32
		seq             uint64
	}{
		{parityP2, "in_pre", "up", 65102, 1},
		{parityP2, "in_pre", "down", 65102, 2},
		{parityP3, "in_pre", "up", 65103, 1},
		{parityP3, "loc_rib", "up", 65103, 1},
		{parityP8, "in_pre", "up", 65108, 1},
		{parityP8, "in_pre", "view_lost", 65108, 2},
	} {
		w.peer(peerEventFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
			PeerIP: e.peer, RIB: e.rib, PeerASN: e.asn,
			SessionID: paritySessR1Cur, Seq: e.seq, Kind: e.kind,
		})
	}

	// P1's unicast routes.
	type uni struct {
		peer, rib, family, prefix, nextHop string
		asn                                uint32
		pathID                             uint32
		seq                                uint64
		withdraw                           uint8
		path                               []uint32
		med, lp                            *uint32
		communities                        []uint32
		large                              []string
	}
	for _, r := range []uni{
		// Live, every attribute set.
		{parityP1, "in_pre", "ipv4u", "198.18.1.0/24", "10.250.9.1", 65101, 0, 1, 0,
			[]uint32{65101, 65200, 65300}, u32(50), u32(100),
			[]uint32{65101<<16 | 100}, []string{"65101:1:1"}},
		// Add-path pair; path 1 is then withdrawn, path 2 stays.
		{parityP1, "in_pre", "ipv4u", "198.18.2.0/24", "10.250.9.1", 65101, 1, 1, 0,
			[]uint32{65101, 65201}, nil, nil, nil, nil},
		{parityP1, "in_pre", "ipv4u", "198.18.2.0/24", "10.250.9.2", 65101, 2, 1, 0,
			[]uint32{65101, 65202}, nil, nil, nil, nil},
		{parityP1, "in_pre", "ipv4u", "198.18.2.0/24", "", 65101, 1, 2, 1,
			nil, nil, nil, nil, nil},
		// Announced, then withdrawn.
		{parityP1, "in_pre", "ipv4u", "198.18.3.0/24", "10.250.9.1", 65101, 0, 1, 0,
			[]uint32{65101, 65203}, nil, nil, nil, nil},
		{parityP1, "in_pre", "ipv4u", "198.18.3.0/24", "", 65101, 0, 2, 1,
			nil, nil, nil, nil, nil},
		// Re-announced with a new path; the newer path wins, first_seen is
		// the first announcement.
		{parityP1, "in_pre", "ipv4u", "198.18.4.0/24", "10.250.9.1", 65101, 0, 1, 0,
			[]uint32{65101, 65204}, nil, nil, nil, nil},
		{parityP1, "in_pre", "ipv4u", "198.18.4.0/24", "10.250.9.1", 65101, 0, 2, 0,
			[]uint32{65101, 65205, 65204}, nil, nil, nil, nil},
		// iBGP: empty path.
		{parityP1, "in_pre", "ipv4u", "198.18.5.0/24", "10.250.9.1", 65101, 0, 1, 0,
			nil, nil, u32(200), nil, nil},
		{parityP1, "in_pre", "ipv6u", "2001:db8:250::/48", "2001:db8:250::1", 65101, 0, 1, 0,
			[]uint32{65101, 65206}, nil, nil, nil, nil},
		// P2's route: live, but its peer ends the session down.
		{parityP2, "in_pre", "ipv4u", "198.18.6.0/24", "10.250.9.2", 65102, 0, 1, 0,
			[]uint32{65102, 65300}, nil, nil, nil, nil},
		// P8's route: live, but its peer ends the session view_lost.
		{parityP8, "in_pre", "ipv4u", "198.18.7.0/24", "10.250.9.8", 65108, 0, 1, 0,
			[]uint32{65108, 65300}, nil, nil, nil, nil},
		// P3's adj-RIB-in and Loc-RIB. The Loc-RIB has 198.18.10.0/24 (also
		// in the superseded session), 198.18.11.0/24, and 198.18.12.0/24
		// withdrawn.
		{parityP3, "in_pre", "ipv4u", "198.18.11.0/24", "10.250.9.3", 65103, 0, 1, 0,
			[]uint32{65103, 65300}, nil, nil, nil, nil},
		{parityP3, "loc_rib", "ipv4u", "198.18.10.0/24", "10.250.9.3", 65103, 0, 1, 0,
			[]uint32{65103, 65995}, nil, nil, nil, nil},
		{parityP3, "loc_rib", "ipv4u", "198.18.11.0/24", "10.250.9.3", 65103, 0, 1, 0,
			[]uint32{65103, 65300}, nil, nil, nil, nil},
		{parityP3, "loc_rib", "ipv4u", "198.18.12.0/24", "10.250.9.3", 65103, 0, 1, 0,
			[]uint32{65103, 65301}, nil, nil, nil, nil},
		{parityP3, "loc_rib", "ipv4u", "198.18.12.0/24", "", 65103, 0, 2, 1,
			nil, nil, nil, nil, nil},
	} {
		w.unicast(routeUnicastFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
			PeerIP: r.peer, RIB: r.rib, Family: r.family, Prefix: r.prefix,
			NextHop: r.nextHop, PeerASN: r.asn, PathID: r.pathID,
			SessionID: paritySessR1Cur, Seq: r.seq, IsWithdraw: r.withdraw,
			ASPath: r.path, MED: r.med, LocalPref: r.lp,
			Communities: r.communities, LargeCommunities: r.large,
		})
	}

	// P1's VPN routes: one prefix under two RDs, an add-path pair, one
	// withdrawn, one labeled-unicast (no RD).
	type vpnRow struct {
		family, prefix, rd string
		pathID             uint32
		seq                uint64
		withdraw           uint8
		labels, path       []uint32
		rts                []string
	}
	for _, r := range []vpnRow{
		{"vpn4", "192.0.2.0/24", "65101:1", 0, 1, 0, []uint32{100}, []uint32{65101, 65400}, []string{"65101:1"}},
		{"vpn4", "192.0.2.0/24", "65101:2", 0, 1, 0, []uint32{200}, []uint32{65101, 65401}, []string{"65101:2"}},
		{"vpn4", "192.0.2.32/27", "65101:1", 1, 1, 0, []uint32{101}, []uint32{65101, 65402}, nil},
		{"vpn4", "192.0.2.32/27", "65101:1", 2, 1, 0, []uint32{102}, []uint32{65101, 65403}, nil},
		{"vpn4", "192.0.2.128/25", "65101:1", 0, 1, 0, []uint32{103}, []uint32{65101, 65404}, nil},
		{"vpn4", "192.0.2.128/25", "65101:1", 0, 2, 1, []uint32{524288}, nil, nil},
		{"lu4", "198.51.100.0/24", "", 0, 1, 0, []uint32{300}, []uint32{65101, 65405}, nil},
	} {
		w.vpn(routeVPNFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
			PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
			Family: r.family, Prefix: r.prefix, RD: r.rd, NextHop: "10.250.9.1",
			PathID: r.pathID, SessionID: paritySessR1Cur, Seq: r.seq, IsWithdraw: r.withdraw,
			Labels: r.labels, ASPath: r.path, RouteTargets: r.rts,
		})
	}

	// P1's EVPN routes: two MAC/IP routes (one then withdrawn), and an IP
	// prefix route re-announced with a new path.
	type evpnRow struct {
		routeType     uint8
		prefix, mac   string
		ip            string
		seq           uint64
		withdraw      uint8
		labels, path  []uint32
		ethernetTagID uint32
	}
	for _, r := range []evpnRow{
		{2, "", "00:50:79:25:00:01", "10.250.50.1", 1, 0, []uint32{10010}, []uint32{65101, 65500}, 10},
		{2, "", "00:50:79:25:00:02", "10.250.50.2", 1, 0, []uint32{10010}, []uint32{65101, 65500}, 10},
		{2, "", "00:50:79:25:00:02", "10.250.50.2", 2, 1, []uint32{}, nil, 10},
		{5, "203.0.113.0/24", "", "", 1, 0, []uint32{10020}, []uint32{65101, 65501}, 0},
		{5, "203.0.113.0/24", "", "", 2, 0, []uint32{10020}, []uint32{65101, 65502, 65501}, 0},
	} {
		w.evpn(routeEVPNFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
			PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
			RouteType: r.routeType, RD: "65101:10", Prefix: r.prefix, MAC: r.mac, IP: r.ip,
			ESI: evpnFixtureESIZero, EthernetTag: r.ethernetTagID, NextHop: "10.250.9.1",
			SessionID: paritySessR1Cur, Seq: r.seq, IsWithdraw: r.withdraw,
			Labels: r.labels, ASPath: r.path,
		})
	}

	// P1 finishes ipv4u and evpn in the current session; ipv6u, vpn4 and
	// lu4 are still dumping. P3 finishes ipv4u on its Loc-RIB only.
	for _, m := range []struct {
		peer, rib, family string
		asn               uint32
	}{
		{parityP1, "in_pre", "ipv4u", 65101},
		{parityP1, "in_pre", "evpn", 65101},
		{parityP3, "loc_rib", "ipv4u", 65103},
	} {
		w.eor(eorFixture{
			Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
			PeerIP: m.peer, RIB: m.rib, Family: m.family, PeerASN: m.asn,
			SessionID: paritySessR1Cur, Seq: 1,
		})
	}

	// Stats: P3's Loc-RIB count, reported twice; the newer report (3) must
	// win over the older (5). P1 reports counters that do not include type
	// 8, and has no Loc-RIB rows, so it has nothing the comparison can
	// report.
	w.stats(statsEventFixture{
		Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
		PeerIP: parityP3, RIB: "loc_rib", PeerASN: 65103,
		SessionID: paritySessR1Cur, Seq: 1, Counters: map[uint32]uint64{7: 1, 8: 5},
	})
	w.stats(statsEventFixture{
		Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
		PeerIP: parityP3, RIB: "loc_rib", PeerASN: 65103,
		SessionID: paritySessR1Cur, Seq: 2, Counters: map[uint32]uint64{7: 1, 8: 3},
	})
	w.stats(statsEventFixture{
		Collector: parityC1, RouterIP: parityR1, RouterSysname: r1,
		PeerIP: parityP1, RIB: "in_pre", PeerASN: 65101,
		SessionID: paritySessR1Cur, Seq: 1, Counters: map[uint32]uint64{7: 10},
	})

	// --- R2: dual-homed ----------------------------------------------------
	//
	// Each collector's superseded session is written first, at seq 900, for
	// the reasons R1's superseded session gives.
	const r2, r2Old = "parity-r2", "parity-r2-before-reset"
	type r2Route struct {
		peer, prefix string
		path         []uint32
	}
	type r2Session struct {
		collector, sysname string
		session, seq       uint64
		peers              []string
		routes             []r2Route
		marker             bool
	}
	for _, s := range []r2Session{
		{parityC1, r2Old, paritySessR2C1Old, paritySupersededSeq, []string{parityP4},
			[]r2Route{
				{parityP4, "198.18.20.0/24", []uint32{65104, 65998, 65600}},
				{parityP4, "198.18.21.0/24", []uint32{65104, 65998, 65600}},
			}, true},
		{parityC1, r2, paritySessR2C1Cur, 1, []string{parityP4},
			[]r2Route{{parityP4, "198.18.20.0/24", []uint32{65104, 65600}}}, true},
		// A marker here must not make collector 2's current session
		// complete, and P5 must not appear at all.
		{parityC2, r2Old, paritySessR2C2Old, paritySupersededSeq, []string{parityP4, parityP5},
			[]r2Route{
				{parityP4, "198.18.20.0/24", []uint32{65104, 65997, 65600}},
				{parityP5, "198.18.22.0/24", []uint32{65104, 65997, 65600}},
			}, true},
		{parityC2, r2, paritySessR2C2Cur, 1, []string{parityP4},
			[]r2Route{{parityP4, "198.18.20.0/24", []uint32{65104, 65600}}}, false},
	} {
		for _, p := range s.peers {
			w.peer(peerEventFixture{
				Collector: s.collector, RouterIP: parityR2, RouterSysname: s.sysname,
				PeerIP: p, RIB: "in_pre", PeerASN: 65104,
				SessionID: s.session, Seq: s.seq, Kind: "up",
			})
		}
		for _, r := range s.routes {
			w.unicast(routeUnicastFixture{
				Collector: s.collector, RouterIP: parityR2, RouterSysname: s.sysname,
				PeerIP: r.peer, RIB: "in_pre", PeerASN: 65104, Prefix: r.prefix,
				NextHop: "10.250.9.4", ASPath: r.path,
				SessionID: s.session, Seq: s.seq,
			})
		}
		if s.marker {
			w.eor(eorFixture{
				Collector: s.collector, RouterIP: parityR2, RouterSysname: s.sysname,
				PeerIP: parityP4, RIB: "in_pre", PeerASN: 65104,
				SessionID: s.session, Seq: s.seq,
			})
		}
	}

	// --- R3: link state -----------------------------------------------------
	//
	// The superseded session is written first, at seq 900 and up, for the
	// reasons R1's superseded session gives.
	for _, p := range []struct {
		peer    string
		session uint64
		seq     uint64
		sysname string
	}{
		{parityP6, paritySessR3Old, paritySupersededSeq, "parity-r3-before-reset"},
		{parityP7, paritySessR3Old, paritySupersededSeq, "parity-r3-before-reset"},
		{parityP6, paritySessR3Cur, 1, "parity-r3"},
		{parityP7, paritySessR3Cur, 1, "parity-r3"},
	} {
		w.peer(peerEventFixture{
			Collector: parityC1, RouterIP: parityR3, RouterSysname: p.sysname,
			PeerIP: p.peer, RIB: "in_pre", PeerASN: parityLSASN,
			SessionID: p.session, Seq: p.seq, Kind: "up",
		})
	}

	// Superseded session.
	const s3 = paritySupersededSeq
	// Same keys as current-session objects: A under another name, and C, the
	// A-C link and 10.250.101.0/24 announced where the current session
	// withdrew them.
	w.lsNode(parityP6, paritySessR3Old, s3, parityLSNodeA, "10.250.201.1", "parity-ls-a-before-reset", 0)
	w.lsNode(parityP6, paritySessR3Old, s3+1, parityLSNodeC, "", "parity-ls-c-before-reset", 0)
	w.lsLink(parityP6, paritySessR3Old, s3+2, parityLSNodeA, parityLSNodeC, "10.250.33.1", "10.250.33.2", 3, 4, 0)
	w.lsPrefix(parityP6, paritySessR3Old, s3+3, parityLSNodeB, "10.250.101.0", 24, 0)
	// Objects only the superseded session has, and an End-of-RIB for P7 that
	// must not make P7's current session complete.
	w.lsNode(parityP6, paritySessR3Old, s3+4, parityLSNodeD, "", "parity-ls-d-before-reset", 0)
	w.lsLink(parityP6, paritySessR3Old, s3+5, parityLSNodeA, parityLSNodeD, "10.250.30.1", "10.250.30.2", 1, 2, 0)
	w.lsPrefix(parityP6, paritySessR3Old, s3+6, parityLSNodeD, "10.250.102.0", 24, 0)
	w.lsEndOfRIB(parityP7, paritySessR3Old, s3+7)

	// Current session, P6: A renamed (the lower-seq name must lose), B, C
	// announced then withdrawn with the withdrawal arriving FIRST.
	w.lsNode(parityP6, paritySessR3Cur, 1, parityLSNodeA, "", "parity-ls-a-old-name", 0)
	w.lsNode(parityP6, paritySessR3Cur, 2, parityLSNodeA, "10.250.200.1", "parity-ls-a", 0)
	w.lsNode(parityP6, paritySessR3Cur, 3, parityLSNodeB, "", "parity-ls-b", 0)
	w.lsNode(parityP6, paritySessR3Cur, 5, parityLSNodeC, "", "parity-ls-c", 1)
	w.lsNode(parityP6, paritySessR3Cur, 4, parityLSNodeC, "", "parity-ls-c", 0)
	// P7 names B differently at a higher seq (the fleet tier's answer, which
	// P6's links must not use), and is the only one to name E.
	w.lsNode(parityP7, paritySessR3Cur, 6, parityLSNodeB, "", "parity-ls-b-per-p7", 0)
	w.lsNode(parityP7, paritySessR3Cur, 7, parityLSNodeE, "", "parity-ls-e", 0)

	// P6's links: A-B live, a parallel A-B on other interfaces, A-C
	// withdrawn (withdrawal first), A-E labeled from the fleet, A-F unnamed.
	w.lsLink(parityP6, paritySessR3Cur, 10, parityLSNodeA, parityLSNodeB, "10.250.31.1", "10.250.31.2", 1, 2, 0)
	w.lsLink(parityP6, paritySessR3Cur, 11, parityLSNodeA, parityLSNodeB, "10.250.32.1", "10.250.32.2", 1, 2, 0)
	w.lsLink(parityP6, paritySessR3Cur, 13, parityLSNodeA, parityLSNodeC, "10.250.33.1", "10.250.33.2", 3, 4, 1)
	w.lsLink(parityP6, paritySessR3Cur, 12, parityLSNodeA, parityLSNodeC, "10.250.33.1", "10.250.33.2", 3, 4, 0)
	w.lsLink(parityP6, paritySessR3Cur, 14, parityLSNodeA, parityLSNodeE, "10.250.34.1", "10.250.34.2", 5, 6, 0)
	w.lsLink(parityP6, paritySessR3Cur, 15, parityLSNodeA, parityLSNodeF, "10.250.35.1", "10.250.35.2", 7, 8, 0)
	// P7's one link.
	w.lsLink(parityP7, paritySessR3Cur, 16, parityLSNodeE, parityLSNodeB, "10.250.36.1", "10.250.36.2", 9, 10, 0)

	// Prefixes: two live from A and B, one withdrawn, one from P7.
	w.lsPrefix(parityP6, paritySessR3Cur, 20, parityLSNodeA, "10.250.100.0", 24, 0)
	w.lsPrefix(parityP6, paritySessR3Cur, 21, parityLSNodeB, "10.250.104.0", 22, 0)
	w.lsPrefix(parityP6, paritySessR3Cur, 22, parityLSNodeB, "10.250.101.0", 24, 0)
	w.lsPrefix(parityP6, paritySessR3Cur, 23, parityLSNodeB, "10.250.101.0", 24, 1)
	w.lsPrefix(parityP7, paritySessR3Cur, 24, parityLSNodeE, "10.250.103.0", 24, 0)

	// P6 finishes its dump; P7 does not (in this session).
	w.lsEndOfRIB(parityP6, paritySessR3Cur, 30)

}

// parityCase is one query with fixed arguments. run returns the value that
// is snapshotted: already scoped to the parity fixture's own rows and in a
// deterministic order.
type parityCase struct {
	name string
	run  func(ctx context.Context) (any, error)
}

// parityPage is one page of a walk and the cursor it handed back. The
// cursor is part of the snapshot: it names the (collector, session) the walk
// is pinned to, and a walk pinned to a superseded session would differ here
// even where the rows happened to agree.
type parityPage[T any] struct {
	Rows []T
	Next *RIBCursor
}

// walkParity walks a paginated query from start to its end.
func walkParity[T any](ctx context.Context, start *RIBCursor,
	page func(ctx context.Context, cur *RIBCursor) ([]T, *RIBCursor, error)) (any, error) {
	var out []parityPage[T]
	cur := start
	for range 100 {
		rows, next, err := page(ctx, cur)
		if err != nil {
			return nil, err
		}
		out = append(out, parityPage[T]{Rows: rows, Next: next})
		if next == nil {
			return out, nil
		}
		cur = next
	}
	return nil, fmt.Errorf("walk did not end within 100 pages")
}

// sortedByJSON sorts rows by their own JSON encoding. It is applied to
// answers whose ORDER BY is not total over the rows a scope returns --
// Routes, VPNRoutes and EVPNRoutes order by columns that leave different
// prefixes of one peer tied, since the endpoint is normally prefix-scoped --
// so the snapshot cannot flip on a tie the API never promised to break.
func sortedByJSON[T any](rows []T) ([]T, error) {
	keys := make(map[int]string, len(rows))
	idx := make([]int, len(rows))
	for i, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		keys[i] = string(b)
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return strings.Compare(keys[a], keys[b]) })
	out := make([]T, len(rows))
	for i, j := range idx {
		out[i] = rows[j]
	}
	return out, nil
}

func sortedRows[T any](rows []T, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return sortedByJSON(rows)
}

// counted wraps a (rows, total, err) return as one snapshot value.
type counted[T any] struct {
	Rows  []T
	Total uint64
}

// The unicast prefixes the fixture writes under each router, in every
// session and every state -- so the per-prefix answers include the ones
// that must be empty (withdrawn, superseded-session only, down peer).
var (
	parityR1Prefixes = []string{
		"198.18.1.0/24", "198.18.2.0/24", "198.18.3.0/24", "198.18.4.0/24",
		"198.18.5.0/24", "198.18.6.0/24", "198.18.7.0/24", "198.18.9.0/24",
		"198.18.10.0/24", "198.18.11.0/24", "198.18.12.0/24", "198.18.13.0/24",
		"2001:db8:250::/48",
	}
	parityR2Prefixes = []string{"198.18.20.0/24", "198.18.21.0/24", "198.18.22.0/24"}
)

// routesByPrefix runs Routes once per prefix under base's other fields.
// The map is encoded with its keys sorted.
func routesByPrefix(ctx context.Context, q *Q, base RouteFilter, prefixes []string) (any, error) {
	out := make(map[string][]Route, len(prefixes))
	for _, p := range prefixes {
		f := base
		f.Prefix = p
		rows, err := q.Routes(ctx, f)
		if err != nil {
			return nil, err
		}
		if out[p], err = sortedByJSON(rows); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func countRoutesByPrefix(ctx context.Context, q *Q, base RouteFilter, prefixes []string) (any, error) {
	out := make(map[string]uint64, len(prefixes))
	for _, p := range prefixes {
		f := base
		f.Prefix = p
		n, err := q.CountRoutes(ctx, f)
		if err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, nil
}

// parityRefusal snapshots a call that must fail: the error's text, and
// whether it is ErrSessionChanged or ErrBadFilter. A call that succeeds
// instead snapshots its rows, so that the difference shows as a changed
// snapshot rather than a harness failure.
func parityRefusal[T any](rows []T, next *RIBCursor, err error) (any, error) {
	if err == nil {
		return parityPage[T]{Rows: rows, Next: next}, nil
	}
	return struct {
		Err            string
		SessionChanged bool
		BadFilter      bool
	}{err.Error(), errors.Is(err, ErrSessionChanged), errors.Is(err, ErrBadFilter)}, nil
}

func parityCases(q *Q) []parityCase {
	r1 := netip.MustParseAddr(parityR1)
	r2 := netip.MustParseAddr(parityR2)
	r3 := netip.MustParseAddr(parityR3)
	p1 := netip.MustParseAddr(parityP1)
	p2 := netip.MustParseAddr(parityP2)
	p3 := netip.MustParseAddr(parityP3)
	p4 := netip.MustParseAddr(parityP4)
	p6 := netip.MustParseAddr(parityP6)
	p7 := netip.MustParseAddr(parityP7)
	area0 := uint32(0)

	one := func(v any, err error) (any, error) { return v, err }
	count := func(n uint64, err error) (any, error) { return n, err }

	cases := []parityCase{
		// Routers and Collectors take no scope, so their answers are
		// filtered to the parity routers and collectors here. Every
		// CollectorSummary kept is wholly the parity fixture's: no other
		// fixture writes under parity-c1 or parity-c2.
		{"routers", func(ctx context.Context) (any, error) {
			all, err := q.Routers(ctx)
			if err != nil {
				return nil, err
			}
			var out []Router
			for _, r := range all {
				if r.IP == r1 || r.IP == r2 || r.IP == r3 {
					out = append(out, r)
				}
			}
			return out, nil
		}},
		{"collectors", func(ctx context.Context) (any, error) {
			all, err := q.Collectors(ctx)
			if err != nil {
				return nil, err
			}
			var out []CollectorSummary
			for _, c := range all {
				if c.Collector == parityC1 || c.Collector == parityC2 {
					out = append(out, c)
				}
			}
			return out, nil
		}},

		{"peers_r1", func(ctx context.Context) (any, error) { return one(q.Peers(ctx, PeerFilter{Router: r1})) }},
		{"peers_r1_loc_rib", func(ctx context.Context) (any, error) {
			return one(q.Peers(ctx, PeerFilter{Router: r1, RIB: "loc_rib"}))
		}},
		{"peers_r2", func(ctx context.Context) (any, error) { return one(q.Peers(ctx, PeerFilter{Router: r2})) }},
		{"peers_r3", func(ctx context.Context) (any, error) { return one(q.Peers(ctx, PeerFilter{Router: r3})) }},

		// Routes answers nothing without a prefix, covers or wide filter
		// (an empty Prefix matches the empty prefix), so the unicast route
		// cases ask per prefix: every prefix the fixture writes for the
		// router, including the ones that must come back empty.
		{"routes_r1_by_prefix", func(ctx context.Context) (any, error) {
			return routesByPrefix(ctx, q, RouteFilter{Router: r1}, parityR1Prefixes)
		}},
		{"routes_r1_loc_rib_by_prefix", func(ctx context.Context) (any, error) {
			return routesByPrefix(ctx, q, RouteFilter{Router: r1, RIB: "loc_rib"}, parityR1Prefixes)
		}},
		{"routes_r2_by_prefix", func(ctx context.Context) (any, error) {
			return routesByPrefix(ctx, q, RouteFilter{Router: r2}, parityR2Prefixes)
		}},
		{"routes_r1_covers", func(ctx context.Context) (any, error) {
			return sortedRows(q.Routes(ctx, RouteFilter{Router: r1, Covers: "198.18.2.7"}))
		}},
		{"routes_r1_origin", func(ctx context.Context) (any, error) {
			return sortedRows(q.Routes(ctx, RouteFilter{Router: r1, OriginASN: 65300}))
		}},
		{"routes_r1_through", func(ctx context.Context) (any, error) {
			return sortedRows(q.Routes(ctx, RouteFilter{Router: r1, ThroughASN: 65101}))
		}},
		{"routes_r2_origin", func(ctx context.Context) (any, error) {
			return sortedRows(q.Routes(ctx, RouteFilter{Router: r2, OriginASN: 65600}))
		}},
		{"routes_r1_community", func(ctx context.Context) (any, error) {
			return sortedRows(q.Routes(ctx, RouteFilter{Router: r1, Community: "65101:100"}))
		}},
		{"count_routes_r1_by_prefix", func(ctx context.Context) (any, error) {
			return countRoutesByPrefix(ctx, q, RouteFilter{Router: r1}, parityR1Prefixes)
		}},
		{"count_routes_r2_by_prefix", func(ctx context.Context) (any, error) {
			return countRoutesByPrefix(ctx, q, RouteFilter{Router: r2}, parityR2Prefixes)
		}},
		{"count_routes_r1_through", func(ctx context.Context) (any, error) {
			return count(q.CountRoutes(ctx, RouteFilter{Router: r1, ThroughASN: 65101}))
		}},
		{"count_routes_r1_origin", func(ctx context.Context) (any, error) {
			return count(q.CountRoutes(ctx, RouteFilter{Router: r1, OriginASN: 65300}))
		}},

		{"vpn_routes_r1", func(ctx context.Context) (any, error) {
			return sortedRows(q.VPNRoutes(ctx, VPNRouteFilter{Router: r1}))
		}},
		{"vpn_routes_r1_rd", func(ctx context.Context) (any, error) {
			return sortedRows(q.VPNRoutes(ctx, VPNRouteFilter{Router: r1, RD: "65101:1"}))
		}},
		{"count_vpn_routes_r1", func(ctx context.Context) (any, error) {
			return count(q.CountVPNRoutes(ctx, VPNRouteFilter{Router: r1}))
		}},

		{"evpn_routes_r1", func(ctx context.Context) (any, error) {
			return sortedRows(q.EVPNRoutes(ctx, EVPNRouteFilter{Router: r1}))
		}},
		{"evpn_routes_r1_type5", func(ctx context.Context) (any, error) {
			return sortedRows(q.EVPNRoutes(ctx, EVPNRouteFilter{Router: r1, RouteType: 5}))
		}},
		{"count_evpn_routes_r1", func(ctx context.Context) (any, error) {
			return count(q.CountEVPNRoutes(ctx, EVPNRouteFilter{Router: r1}))
		}},

		// RIB walks, Limit 2 so every walk crosses at least one page
		// boundary. rib "" walks every rib of the peer.
		{"rib_unicast_r1_p1", func(ctx context.Context) (any, error) {
			return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, r1, p1, "", c, 2)
			})
		}},
		{"rib_unicast_r1_p2", func(ctx context.Context) (any, error) {
			return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, r1, p2, "", c, 2)
			})
		}},
		{"rib_unicast_r1_p3", func(ctx context.Context) (any, error) {
			return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, r1, p3, "", c, 2)
			})
		}},
		// A dual-homed router's walk is started per collector.
		{"rib_unicast_r2_c1_p4", func(ctx context.Context) (any, error) {
			start, err := q.RIBStart(ctx, r2, p4, "", parityC1)
			if err != nil {
				return nil, err
			}
			return walkParity(ctx, start, func(ctx context.Context, c *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, r2, p4, "", c, 2)
			})
		}},
		{"rib_unicast_r2_c2_p4", func(ctx context.Context) (any, error) {
			start, err := q.RIBStart(ctx, r2, p4, "", parityC2)
			if err != nil {
				return nil, err
			}
			return walkParity(ctx, start, func(ctx context.Context, c *RIBCursor) ([]Route, *RIBCursor, error) {
				return q.RIBPageUnicast(ctx, r2, p4, "", c, 2)
			})
		}},
		{"rib_vpn_r1_p1", func(ctx context.Context) (any, error) {
			return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]VPNRoute, *RIBCursor, error) {
				return q.RIBPageVPN(ctx, r1, p1, "", c, 2)
			})
		}},
		{"rib_evpn_r1_p1", func(ctx context.Context) (any, error) {
			return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]EVPNRoute, *RIBCursor, error) {
				return q.RIBPageEVPN(ctx, r1, p1, "", c, 2)
			})
		}},

		// The two refusals a RIB walk makes about sessions, recorded as
		// their error text: a dual-homed router with no collector named has
		// nothing to pin to, and a cursor pinned to a superseded session is
		// refused rather than walked.
		{"rib_unicast_r2_unpinned_refusal", func(ctx context.Context) (any, error) {
			return parityRefusal(q.RIBPageUnicast(ctx, r2, p4, "", nil, 2))
		}},
		{"rib_unicast_r1_superseded_refusal", func(ctx context.Context) (any, error) {
			return parityRefusal(q.RIBPageUnicast(ctx, r1, p1, "", &RIBCursor{
				Collector: parityC1, SessionID: paritySessR1Old, Router: r1, Peer: p1,
			}, 2))
		}},

		{"topology_unicast_r1_p1", func(ctx context.Context) (any, error) {
			return one(q.TopologyUnicast(ctx, RouteFilter{Router: r1, Peer: p1}))
		}},
		{"topology_unicast_r1_p2", func(ctx context.Context) (any, error) {
			return one(q.TopologyUnicast(ctx, RouteFilter{Router: r1, Peer: p2}))
		}},
		{"topology_unicast_r1_origin", func(ctx context.Context) (any, error) {
			return one(q.TopologyUnicast(ctx, RouteFilter{Router: r1, OriginASN: 65300}))
		}},
		{"topology_unicast_r2_p4", func(ctx context.Context) (any, error) {
			return one(q.TopologyUnicast(ctx, RouteFilter{Router: r2, Peer: p4}))
		}},
		{"topology_vpn_r1", func(ctx context.Context) (any, error) {
			return one(q.TopologyVPN(ctx, VPNRouteFilter{Router: r1}))
		}},
		{"topology_evpn_r1", func(ctx context.Context) (any, error) {
			return one(q.TopologyEVPN(ctx, EVPNRouteFilter{Router: r1}))
		}},

		{"ls_nodes_r3", func(ctx context.Context) (any, error) { return one(q.LSNodes(ctx, LSNodeFilter{Router: r3})) }},
		{"ls_nodes_r3_withdrawn", func(ctx context.Context) (any, error) {
			return one(q.LSNodes(ctx, LSNodeFilter{Router: r3, State: LSStateWithdrawn}))
		}},
		{"ls_nodes_r3_any", func(ctx context.Context) (any, error) {
			return one(q.LSNodes(ctx, LSNodeFilter{Router: r3, State: LSStateAny}))
		}},
		{"ls_nodes_r3_area0", func(ctx context.Context) (any, error) {
			return one(q.LSNodes(ctx, LSNodeFilter{Router: r3, Area: &area0}))
		}},
		{"count_ls_nodes_r3", func(ctx context.Context) (any, error) {
			return count(q.CountLSNodes(ctx, LSNodeFilter{Router: r3}))
		}},
		{"ls_links_r3", func(ctx context.Context) (any, error) { return one(q.LSLinks(ctx, LSLinkFilter{Router: r3})) }},
		{"ls_links_r3_withdrawn", func(ctx context.Context) (any, error) {
			return one(q.LSLinks(ctx, LSLinkFilter{Router: r3, State: LSStateWithdrawn}))
		}},
		{"count_ls_links_r3", func(ctx context.Context) (any, error) {
			return count(q.CountLSLinks(ctx, LSLinkFilter{Router: r3}))
		}},
		{"ls_prefixes_r3", func(ctx context.Context) (any, error) {
			return one(q.LSPrefixes(ctx, LSPrefixFilter{Router: r3}))
		}},
		{"ls_prefixes_r3_withdrawn", func(ctx context.Context) (any, error) {
			return one(q.LSPrefixes(ctx, LSPrefixFilter{Router: r3, State: LSStateWithdrawn}))
		}},
		{"ls_prefixes_r3_covers", func(ctx context.Context) (any, error) {
			return one(q.LSPrefixes(ctx, LSPrefixFilter{Router: r3, Covers: "10.250.105.1"}))
		}},
		{"count_ls_prefixes_r3", func(ctx context.Context) (any, error) {
			return count(q.CountLSPrefixes(ctx, LSPrefixFilter{Router: r3}))
		}},
	}

	// Link-state walks, per peer, Limit 1.
	for _, p := range []struct {
		name string
		peer netip.Addr
	}{{"p6", p6}, {"p7", p7}} {
		cases = append(cases,
			parityCase{"ls_nodes_page_r3_" + p.name, func(ctx context.Context) (any, error) {
				return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]LSNode, *RIBCursor, error) {
					return q.LSNodesPage(ctx, LSNodeFilter{Router: r3, Peer: p.peer, Limit: 1, Cursor: c})
				})
			}},
			parityCase{"ls_links_page_r3_" + p.name, func(ctx context.Context) (any, error) {
				return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]LSLink, *RIBCursor, error) {
					return q.LSLinksPage(ctx, LSLinkFilter{Router: r3, Peer: p.peer, Limit: 1, Cursor: c})
				})
			}},
			parityCase{"ls_prefixes_page_r3_" + p.name, func(ctx context.Context) (any, error) {
				return walkParity(ctx, nil, func(ctx context.Context, c *RIBCursor) ([]LSPrefix, *RIBCursor, error) {
					return q.LSPrefixesPage(ctx, LSPrefixFilter{Router: r3, Peer: p.peer, Limit: 1, Cursor: c})
				})
			}},
		)
	}

	// R1 is the only router with a Loc-RIB; the archived side is scoped to
	// the current session, so its superseded-session-only Loc-RIB route does
	// not count.
	cases = append(cases, parityCase{"locrib_r1", func(ctx context.Context) (any, error) {
		rows, total, err := q.LocRIBComparison(ctx, CollectionFilter{Router: r1})
		return counted[PeerLocRIB]{Rows: rows, Total: total}, err
	}})
	return cases
}

// TestCurrentStateParity snapshots the answer of every current-state query
// over one fixture, so that moving a query to a different source table can
// be checked against the answers it gave before the move. A difference is a
// changed answer: either the move is wrong, or the change is deliberate and
// the snapshot is regenerated (-update) with the reason in the commit.
func TestCurrentStateParity(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertParityFixture(t, ctx, q)

	dir := filepath.Join("testdata", "parity")
	if *updateParity {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, c := range parityCases(q) {
		if seen[c.name] {
			t.Fatalf("duplicate parity case %q", c.name)
		}
		seen[c.name] = true

		got, err := c.run(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		b = append(b, '\n')
		path := filepath.Join(dir, c.name+".json")
		if *updateParity {
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: no snapshot (run with -update on the unchanged code first): %v", c.name, err)
		}
		if !bytes.Equal(want, b) {
			t.Errorf("%s changed:\n%s", c.name, parityDiff(want, b))
		}
	}

	// A snapshot file no case writes is one nothing compares any more.
	if !*updateParity {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if name := strings.TrimSuffix(filepath.Base(f), ".json"); !seen[name] {
				t.Errorf("snapshot %s has no parity case", f)
			}
		}
	}
}

// parityDiff renders the first differing line of two snapshots with a few
// lines of context on each side, which is enough to name the field that
// moved.
func parityDiff(want, got []byte) string {
	w := strings.Split(string(want), "\n")
	g := strings.Split(string(got), "\n")
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "first difference at line %d (want %d lines, got %d)\n", i+1, len(w), len(g))
	lo, hi := max(0, i-3), i+4
	for j := lo; j < hi; j++ {
		if j < len(w) {
			fmt.Fprintf(&sb, "  want %4d: %s\n", j+1, w[j])
		}
	}
	for j := lo; j < hi; j++ {
		if j < len(g) {
			fmt.Fprintf(&sb, "  got  %4d: %s\n", j+1, g[j])
		}
	}
	return sb.String()
}
