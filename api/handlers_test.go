// Handler tests run against a live ClickHouse through a real *query.Q.
// There is no mocked driver.Conn and there deliberately is not one: a
// handler is a thin adapter, so almost everything that can be wrong about
// one is wrong at the seam with query -- a filter field left unset, an
// error sentinel not recognized, a cursor that does not survive the round
// trip. A fake conn would answer whatever this file told it to and every
// one of those would pass.
//
// The fixtures are this package's own rather than query's. query's live in
// unexported test helpers and cannot be reached from here, and copying them
// would put two packages' fixtures in one database anyway -- apiTestDB is a
// separate database for the reason query's testDB is one.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// apiTestDB is this package's own test database. See chtest.SchemaStatements
// on why each package gets one.
const apiTestDB = "vantage_api_test"

// The fixture's coordinates. One collector, one router, two peers, so that
// a filter which ignores its peer argument returns rows from both and is
// caught.
const (
	fixCollector = "api-test-collector"
	fixSysName   = "api-fixture-router"
	fixRouterIP  = "10.0.0.11"
	fixPeerA     = "10.0.0.21"
	fixPeerB     = "10.0.0.22"
	fixSession   = 4242
)

// The link-state fixture's coordinates. insertLSFixture writes under the
// SAME (collector, router, peers, session, rib) insertHandlerFixture already
// establishes peer_up for -- see insertLSFixture's own doc comment for why a
// second set of peer_events rows here would be wrong rather than merely
// redundant.
const (
	// lsFixArea and lsFixOtherArea are the two areas the fixture's rows
	// split across. Area 0 on purpose: it is the OSPF backbone and the
	// value a zero-means-unset filter would
	// make unaskable, so the hazard this fixture exists to catch sits on
	// the default path rather than off to the side of it.
	lsFixArea      = uint32(0)
	lsFixOtherArea = uint32(1)

	// lsFixASN and lsFixOtherASN are the two AS numbers the fixture uses, so
	// that asn= has a real value to narrow against rather than a bogus one
	// that merely empties the answer.
	lsFixASN      = uint32(65100)
	lsFixOtherASN = uint32(65200)

	// lsFixProtocol and lsFixOtherProtocol are IS-IS Level 2 and OSPFv2 --
	// both real entries in query.ProtocolName's registry -- so protocol=
	// narrows to a genuine subset rather than to nothing.
	// lsFixProtocolName is lsFixProtocol's registry spelling, for the one
	// test that asks in that notation instead of the decimal one.
	lsFixProtocol      = uint8(2)
	lsFixProtocolName  = "isis-l2"
	lsFixOtherProtocol = uint8(3)

	// lsFixPrefix is the one prefix prefix= and covers= are asked to find.
	// lsFixCoveredAddr is an address inside it and outside every other
	// prefix the fixture writes, so covers= has exactly one right answer.
	lsFixPrefix      = "10.90.20.0/24"
	lsFixCoveredAddr = "10.90.20.5"

	// lsFixBogusRouter and lsFixBogusRIB are values no fixture row carries,
	// so filtering on one MUST turn a non-empty answer into an empty one --
	// the cheapest proof a parameter reached the query rather than being
	// silently ignored, for the two parameters the fixture's single router
	// and single rib otherwise give no real subset to narrow to. peer=
	// narrows to a real subset instead (fixPeerB), so it needs no bogus
	// counterpart here.
	lsFixBogusRouter = "10.255.255.1"
	lsFixBogusRIB    = "loc_rib"
)

// requireAPIBuilt is the body every requireAPIWith* helper in this package
// delegates to: build a Server over a real query.Q against apiTestDB, with
// the fixture loaded, differing only in the three Config fields a caller
// actually varies. It skips when ClickHouse is unreachable, the way every
// other live test in this repo does.
//
// maxUnscopedSince has no default here the way it does under LoadConfig
// (api/config.go's defaultMaxUnscopedSince): this Config is built directly,
// so a caller that passed zero would make checkUnscopedWindow refuse EVERY
// unscoped /v1/events request the resulting Server serves, including one
// naming no since= at all. Every helper below passes 24 * time.Hour unless
// it exists specifically to vary that field.
func requireAPIBuilt(t *testing.T, defaultPage, maxPage int, maxUnscopedSince time.Duration) *Server {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	insertHandlerFixture(t, ctx, conn)
	q, err := query.New(conn, apiTestDB)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	s, err := NewServer(q, Config{
		DefaultPage:      defaultPage,
		MaxPage:          maxPage,
		MaxUnscopedSince: maxUnscopedSince,
		Tokens:           []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// requireAPI is requireAPIBuilt with defaults big enough that no test built
// on it trips a page cap or the unscoped-window clamp by accident. See
// requireAPIWithMaxUnscopedSince for a test that needs a different window.
func requireAPI(t *testing.T) *Server {
	t.Helper()
	return requireAPIBuilt(t, 1000, 10000, 24*time.Hour)
}

// requireAPIWithMaxPage is requireAPI with a deliberately tiny cap, so
// truncation can be exercised against a fixture of a handful of rows rather
// than by inserting ten thousand.
func requireAPIWithMaxPage(t *testing.T, maxPage int) *Server {
	t.Helper()
	return requireAPIBuilt(t, maxPage, maxPage, 24*time.Hour)
}

// requireAPIWithPageSizes is requireAPIWithMaxPage with DefaultPage and
// MaxPage set independently, for the one case that has to tell them apart:
// requireAPIWithMaxPage's Config sets both to the same value, which cannot
// distinguish a scoped link-state walk's absent limit= resolving to
// cfg.MaxPage from it silently resolving to cfg.DefaultPage instead. A
// Config where DefaultPage is smaller than the fixture's own row count for
// some scope, and MaxPage is not, makes the two observably different.
func requireAPIWithPageSizes(t *testing.T, defaultPage, maxPage int) *Server {
	t.Helper()
	return requireAPIBuilt(t, defaultPage, maxPage, 24*time.Hour)
}

// requireAPIWithLSFixture is requireAPI under the name the link-state
// handler tests use at their own call sites. insertHandlerFixture already
// writes the ls_nodes, ls_links and ls_prefixes rows those tests read -- see
// its own doc comment -- so there is nothing left for this to do
// differently; it exists so a reader of TestLSNodesEndpointReturnsRealRows
// and its siblings sees a name that says what the fixture is being asked
// for, rather than having to go check whether requireAPI happens to load
// link-state data before trusting that it does.
func requireAPIWithLSFixture(t *testing.T) *Server {
	t.Helper()
	return requireAPI(t)
}

// insertHandlerFixture writes the smallest set of rows every test in this
// file can share. It is deliberately not per-test: these tables are
// ReplacingMergeTree keyed on the coordinates below, so re-inserting the
// same rows is idempotent and the fixture can be loaded by every test
// without them interfering.
//
// The shape, and why each part is here:
//
//	peer_events   two peers, both up, in in_pre. Routers and Peers are
//	              derived entirely from this table, so it is what makes a
//	              router exist at all.
//	route_unicast four routes under peer A, one under peer B. Two of peer
//	              A's share a prefix and differ only in path_id, which is
//	              what makes a RIB walk cross a page boundary inside one
//	              prefix -- the case a key without path_id would drop.
//	              10.77.2.0/24 alone carries a route target, 65000:100 --
//	              the one unicast row in this fixture a community= filter
//	              can actually find. Without it, every wide-filter-alone
//	              community test on /v1/routes/unicast could only assert
//	              200, never a real match: route_unicast's own four
//	              community-shaped columns were otherwise empty on every
//	              row here.
//	route_vpn     one route, so the fan-out has a non-empty vpn key to
//	              return and an empty evpn one.
//	eor_events    nothing. A peer that is up with no end-of-RIB marker
//	              reads DumpState "dumping", which is what the
//	              session_dumping warning is asserted against.
//	ls_nodes,
//	ls_links,
//	ls_prefixes   insertLSFixture's own rows, under the same peer_events
//	              this function already wrote above. Called from here,
//	              rather than only from requireAPIWithLSFixture, so that
//	              requireAPIWithMaxPage carries link-state data too --
//	              TestLSNodesTruncatesAndSaysSo drives the cap through that
//	              helper, the same one every other truncation test in this
//	              file uses, and it would have nothing to truncate
//	              otherwise.
func insertHandlerFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	var seq uint64
	for _, p := range []string{fixPeerA, fixPeerB} {
		seq++
		if err := peers.Append(
			fixCollector, netip.MustParseAddr(fixRouterIP), fixSysName,
			netip.MustParseAddr(p), "in_pre", uint32(65001),
			netip.MustParseAddr("10.0.0.11"), uint64(fixSession), seq,
			ts, ts, []string{}, seq,
			"up", netip.MustParseAddr(fixRouterIP), uint16(179), uint16(50000),
			uint32(0), uint8(1),
		); err != nil {
			t.Fatalf("append peer_events: %v", err)
		}
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	uni, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast: %v", err)
	}
	type uroute struct {
		peer   string
		prefix string
		pathID uint32
		// routeTargets is [] for every row except 10.77.2.0/24 -- see this
		// function's own doc comment for why that one carries 65000:100.
		routeTargets []string
	}
	// 10.77.0.0/24 twice under peer A: same prefix, two path_ids. See the
	// doc comment.
	for i, r := range []uroute{
		{fixPeerA, "10.77.0.0/24", 1, []string{}},
		{fixPeerA, "10.77.0.0/24", 2, []string{}},
		{fixPeerA, "10.77.1.0/24", 0, []string{}},
		{fixPeerA, "10.77.2.0/24", 0, []string{"65000:100"}},
		{fixPeerB, "10.88.0.0/24", 0, []string{}},
	} {
		if err := uni.Append(
			fixCollector, netip.MustParseAddr(fixRouterIP), fixSysName,
			netip.MustParseAddr(r.peer), "in_pre", uint32(65001),
			netip.MustParseAddr("10.0.0.11"), uint64(fixSession), uint64(i+1),
			ts, ts, []string{}, uint64(i+1),
			"ipv4u", r.prefix, r.pathID, uint8(0), uint8(0),
			[]uint32{65001, 65002}, "10.0.0.21", nil, nil,
			[]uint32{}, []string{}, r.routeTargets, []string{},
		); err != nil {
			t.Fatalf("append route_unicast: %v", err)
		}
	}
	if err := uni.Send(); err != nil {
		t.Fatalf("send route_unicast: %v", err)
	}

	vpn, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".route_vpn")
	if err != nil {
		t.Fatalf("prepare route_vpn: %v", err)
	}
	if err := vpn.Append(
		fixCollector, netip.MustParseAddr(fixRouterIP), fixSysName,
		netip.MustParseAddr(fixPeerA), "in_pre", uint32(65001),
		netip.MustParseAddr("10.0.0.11"), uint64(fixSession), uint64(90),
		ts, ts, []string{}, uint64(90),
		// Column order is schema.sql's: family, prefix, path_id, is_withdraw,
		// rd, labels, origin, as_path, next_hop, med, local_pref, then the
		// four community arrays. route_vpn puts rd and labels between
		// is_withdraw and origin, which route_unicast has nothing to put.
		"vpn4", "10.77.0.0/24", uint32(0), uint8(0),
		"65000:1", []uint32{24}, uint8(0),
		[]uint32{65001}, "10.0.0.21", nil, nil,
		[]uint32{}, []string{}, []string{"65000:1"}, []string{},
	); err != nil {
		t.Fatalf("append route_vpn: %v", err)
	}
	if err := vpn.Send(); err != nil {
		t.Fatalf("send route_vpn: %v", err)
	}

	insertLSFixture(t, ctx, conn)
	insertDualCollectorFixture(t, ctx, conn)
}

// The dual-collector fixture's coordinates: ONE router that TWO collectors
// monitor, on its own address so no existing count moves.
//
// insertHandlerFixture writes one collector and says so, which means every
// RIB assertion built on it passes whether or not the walk can name a
// collector -- the one-branch fixture problem. A router two collectors
// monitor is the only shape that can tell a pinned walk from an
// under-specified one.
//
// The two collectors hold a DIFFERENT number of routes, so "pinned to c1",
// "pinned to c2" and "both merged" are three distinguishable answers.
const (
	dualCollA     = "api-test-collector-a"
	dualCollB     = "api-test-collector-b"
	dualRouterIP  = "10.0.0.31"
	dualSysName   = "api-dual-router"
	dualPeer      = "10.0.0.41"
	dualSessionA  = 5151
	dualSessionB  = 5252
	dualRoutesOnA = 3
	dualRoutesOnB = 2
)

func insertDualCollectorFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	var seq uint64
	for _, c := range []struct {
		id      string
		session uint64
	}{{dualCollA, dualSessionA}, {dualCollB, dualSessionB}} {
		seq++
		if err := peers.Append(
			c.id, netip.MustParseAddr(dualRouterIP), dualSysName,
			netip.MustParseAddr(dualPeer), "in_pre", uint32(65001),
			netip.MustParseAddr(dualRouterIP), c.session, seq,
			ts, ts, []string{}, 500+seq,
			"up", netip.MustParseAddr(dualRouterIP), uint16(179), uint16(50000),
			uint32(0), uint8(1),
		); err != nil {
			t.Fatalf("append peer_events: %v", err)
		}
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	uni, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast: %v", err)
	}
	var n uint64
	add := func(collector string, session uint64, prefix string) {
		n++
		if err := uni.Append(
			collector, netip.MustParseAddr(dualRouterIP), dualSysName,
			netip.MustParseAddr(dualPeer), "in_pre", uint32(65001),
			netip.MustParseAddr(dualRouterIP), session, n,
			ts, ts, []string{}, 600+n,
			"ipv4u", prefix, uint32(0), uint8(0), uint8(0),
			[]uint32{65001, 65002}, "10.0.0.41", nil, nil,
			[]uint32{}, []string{}, []string{}, []string{},
		); err != nil {
			t.Fatalf("append route_unicast: %v", err)
		}
	}
	for _, pfx := range []string{"10.99.0.0/24", "10.99.1.0/24", "10.99.2.0/24"} {
		add(dualCollA, dualSessionA, pfx)
	}
	for _, pfx := range []string{"10.99.0.0/24", "10.99.1.0/24"} {
		add(dualCollB, dualSessionB, pfx)
	}
	if err := uni.Send(); err != nil {
		t.Fatalf("send route_unicast: %v", err)
	}
}

// insertLSFixture writes the link-state rows the /v1/ls/* handler tests
// read. It shares insertHandlerFixture's own peer_events rows -- fixCollector,
// fixRouterIP, fixPeerA and fixPeerB under rib "in_pre", session fixSession
// -- rather than writing a second set: lsNodesSQL, lsLinksSQL and
// lsPrefixesSQL all join their table to peer_events by exactly that tuple
// (peerStateCTE and peerUpCTE, shared with the route statements), and a
// second peer_events row under the same key would either collide
// (ReplacingMergeTree) or, under a different session, orphan every row this
// function writes from `cur` -- the current-session CTE resolves ONE
// session per (collector, router), so a second one here would not add a
// second current session, it would just move which rows `cur` calls current.
//
// Four identities, in each of the three tables, follow one shape -- named
// -a through -d below for this comment's purposes only, since nothing reads
// those labels back:
//
//	-a  reported by peer A, protocol 2 (isis-l2), area 0, asn 65100. The
//	    "matches everything" row: every narrower filter has to exclude some
//	    OTHER row to prove itself, and -a is the one every such filter must
//	    still include.
//	-b  reported by peer A, protocol 3 (ospfv2), area 1, asn 65200. Differs
//	    from -a in protocol, area AND asn at once, so a single -b row is
//	    enough for protocol=, area= and asn= to each narrow the answer on
//	    their own, independently.
//	-c  reported by peer A, otherwise -a's shape, WITHDRAWN: announced at a
//	    lower seq and withdrawn at a higher one (same identity both times,
//	    so argMax's (seq, stream_seq) ordering resolves the pair to exactly
//	    one row). state=live excludes it, state=withdrawn returns exactly
//	    it, state=any returns it alongside -a and -b but not doubled.
//	-d  reported by peer B, otherwise -a's shape. The one row peer= has to
//	    exclude for peer= to be more than a second router= -- the fixture
//	    puts everything under one router, so router= is tested against a
//	    value no row carries instead (see lsFixBogusRouter).
//
// Links add a fifth row within the same shape, reported by peer A on -a's
// local end and withdrawn the same way -c is, so state= has something to
// narrow on /v1/ls/links too and not only on /v1/ls/nodes.
//
// TestLSContractParametersAllNarrow is what actually reads this shape
// exhaustively, one documented parameter at a time; see its own doc comment
// for the mechanism. The other TestLS* functions each read one or two
// properties of it directly.
func insertLSFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	nodes, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	type lsNodeRow struct {
		peer       string
		seq        uint64
		protocol   uint8
		area       uint32
		asn        uint32
		routerID   string
		name       string
		isWithdraw uint8
	}
	for _, r := range []lsNodeRow{
		{fixPeerA, 500, lsFixProtocol, lsFixArea, lsFixASN, "0a0000c1", "ls-http-node-a", 0},
		{fixPeerA, 501, lsFixOtherProtocol, lsFixOtherArea, lsFixOtherASN, "0a0000c2", "ls-http-node-b", 0},
		{fixPeerA, 502, lsFixProtocol, lsFixArea, lsFixASN, "0a0000c3", "ls-http-node-c", 0}, // announce
		{fixPeerA, 503, lsFixProtocol, lsFixArea, lsFixASN, "0a0000c3", "ls-http-node-c", 1}, // withdrawal, wins on seq
		{fixPeerB, 504, lsFixProtocol, lsFixArea, lsFixASN, "0a0000c4", "ls-http-node-d", 0},
	} {
		if err := nodes.Append(
			fixCollector, netip.MustParseAddr(fixRouterIP), fixSysName,
			netip.MustParseAddr(r.peer), "in_pre", uint32(65001),
			netip.MustParseAddr(fixRouterIP), uint64(fixSession), r.seq,
			ts, ts, []string{}, r.seq,
			r.protocol, uint64(0), r.asn, uint32(0), r.area, r.routerID,
			"", // router_id_v4: empty, so the label falls back to the identifier
			r.isWithdraw, r.name,
			uint32(16000), uint32(8000), uint32(15000), uint32(1000),
			[]uint8{0}, map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_nodes: %v", err)
		}
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}

	links, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".ls_links")
	if err != nil {
		t.Fatalf("prepare ls_links: %v", err)
	}
	type lsLinkRow struct {
		peer                          string
		seq                           uint64
		protocol                      uint8
		localASN, remoteASN           uint32
		localArea, remoteArea         uint32
		localRouterID, remoteRouterID string
		localIfAddr, remoteIfAddr     string
		localLinkID, remoteLinkID     uint32
		isWithdraw                    uint8
	}
	for _, r := range []lsLinkRow{
		// -a: both ends in area 0, both asn 65100 -- the row every
		// either-end filter below must still include.
		{fixPeerA, 520, lsFixProtocol, lsFixASN, lsFixASN, lsFixArea, lsFixArea,
			"0a0000d0", "0a0000d1", "10.2.1.1", "10.2.1.2", 1, 2, 0},
		// -b: same local end as -a, remote end in area 1 / asn 65200 --
		// area= and asn= each narrow away from this row's remote end while
		// still matching -a's local end (either-end semantics).
		{fixPeerA, 521, lsFixProtocol, lsFixASN, lsFixOtherASN, lsFixArea, lsFixOtherArea,
			"0a0000d0", "0a0000d2", "10.2.2.1", "10.2.2.2", 3, 4, 0},
		// -c: reported by peer B, protocol 3, BOTH ends in area 1 and asn
		// 65200 -- the row area=0 and asn=65100 must each exclude by
		// NEITHER end matching, not just one.
		{fixPeerB, 522, lsFixOtherProtocol, lsFixOtherASN, lsFixOtherASN, lsFixOtherArea, lsFixOtherArea,
			"0a0000d3", "0a0000d4", "10.2.3.1", "10.2.3.2", 5, 6, 0},
		// -d: -a's local end again, a third remote end, withdrawn --
		// announce then withdrawal, same identity both times.
		{fixPeerA, 523, lsFixProtocol, lsFixASN, lsFixASN, lsFixArea, lsFixArea,
			"0a0000d0", "0a0000d5", "10.2.4.1", "10.2.4.2", 7, 8, 0},
		{fixPeerA, 524, lsFixProtocol, lsFixASN, lsFixASN, lsFixArea, lsFixArea,
			"0a0000d0", "0a0000d5", "10.2.4.1", "10.2.4.2", 7, 8, 1},
	} {
		if err := links.Append(
			fixCollector, netip.MustParseAddr(fixRouterIP), fixSysName,
			netip.MustParseAddr(r.peer), "in_pre", uint32(65001),
			netip.MustParseAddr(fixRouterIP), uint64(fixSession), r.seq,
			ts, ts, []string{}, r.seq,
			r.protocol, uint64(0),
			r.localASN, uint32(0), r.localArea, r.localRouterID,
			r.remoteASN, uint32(0), r.remoteArea, r.remoteRouterID,
			r.localIfAddr, r.remoteIfAddr, r.localLinkID, r.remoteLinkID,
			// local_node_key and remote_node_key are MATERIALIZED and must
			// not be appended: PrepareBatch is positional, and one extra
			// value here shifts every column after it by one.
			r.isWithdraw,
			[]uint32{24001}, []uint8{0x30}, []uint8{0},
			uint32(10), uint32(20), uint32(0), float32(1e9),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_links: %v", err)
		}
	}
	if err := links.Send(); err != nil {
		t.Fatalf("send ls_links: %v", err)
	}

	pfx, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".ls_prefixes")
	if err != nil {
		t.Fatalf("prepare ls_prefixes: %v", err)
	}
	type lsPrefixRow struct {
		peer       string
		seq        uint64
		protocol   uint8
		area       uint32
		asn        uint32
		routerID   string
		addr       string
		prefixLen  uint8
		isWithdraw uint8
	}
	for _, r := range []lsPrefixRow{
		{fixPeerA, 540, lsFixProtocol, lsFixArea, lsFixASN, "0a0000e1", "10.90.20.0", 24, 0},
		{fixPeerA, 541, lsFixOtherProtocol, lsFixOtherArea, lsFixOtherASN, "0a0000e2", "10.90.21.0", 24, 0},
		{fixPeerA, 542, lsFixProtocol, lsFixArea, lsFixASN, "0a0000e3", "10.90.22.0", 24, 0}, // announce
		{fixPeerA, 543, lsFixProtocol, lsFixArea, lsFixASN, "0a0000e3", "10.90.22.0", 24, 1}, // withdrawal
		{fixPeerB, 544, lsFixProtocol, lsFixArea, lsFixASN, "0a0000e4", "10.90.23.0", 24, 0},
	} {
		if err := pfx.Append(
			fixCollector, netip.MustParseAddr(fixRouterIP), fixSysName,
			netip.MustParseAddr(r.peer), "in_pre", uint32(65001),
			netip.MustParseAddr(fixRouterIP), uint64(fixSession), r.seq,
			ts, ts, []string{}, r.seq,
			r.protocol, uint64(0), r.asn, uint32(0), r.area, r.routerID,
			r.addr, r.prefixLen,
			r.isWithdraw, uint32(16001), uint8(0), uint8(1), uint32(10), uint8(0), uint8(0),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_prefixes: %v", err)
		}
	}
	if err := pfx.Send(); err != nil {
		t.Fatalf("send ls_prefixes: %v", err)
	}
}

// get issues an authenticated GET and returns the recorder.
func get(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getOK issues an authenticated GET, requires 200, and decodes the envelope
// into data (a pointer) plus the meta.
func getOK(t *testing.T, s *Server, target string, data any) Meta {
	t.Helper()
	rec := get(t, s, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body %s", target, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s Content-Type = %q, want application/json", target, ct)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
		Meta Meta            `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("GET %s: decode envelope: %v; body %s", target, err, rec.Body.String())
	}
	if data != nil {
		if err := json.Unmarshal(env.Data, data); err != nil {
			t.Fatalf("GET %s: decode data: %v; data %s", target, err, env.Data)
		}
	}
	return env.Meta
}

// requireError requires a non-2xx with the contract's error shape and the
// given status and code, and returns the message.
func requireError(t *testing.T, s *Server, target string, status int, code string) string {
	t.Helper()
	rec := get(t, s, target)
	if rec.Code != status {
		t.Fatalf("GET %s = %d, want %d; body %s", target, rec.Code, status, rec.Body.String())
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: decode error body: %v; body %s", target, err, rec.Body.String())
	}
	if body.Error.Code != code {
		t.Errorf("GET %s error code = %q, want %q (message %q)",
			target, body.Error.Code, code, body.Error.Message)
	}
	if body.Error.Message == "" {
		t.Errorf("GET %s returned code %q with an empty message; the contract's "+
			"ErrorBody requires both, and a bare code tells an operator nothing",
			target, code)
	}
	return body.Error.Message
}

// getMeta issues an authenticated GET, requires 200, and returns only the
// meta -- getOK already decodes both, but TestLSEndpointsCarryTotalMatched
// checks meta.total_matched across three endpoints whose row shapes differ
// (WireLSNode, WireLSLink, WireLSPrefix), and this call does not need to
// name any of them to do it.
func getMeta(t *testing.T, s *Server, target string) Meta {
	t.Helper()
	return getOK(t, s, target, nil)
}

func TestHandlerRouters(t *testing.T) {
	s := requireAPI(t)
	var got []WireRouter
	getOK(t, s, "/v1/routers", &got)

	var found *WireRouter
	for i, r := range got {
		if r.IP == fixRouterIP {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("/v1/routers did not report %s; got %+v", fixRouterIP, got)
	}
	if found.SysName != fixSysName {
		t.Errorf("sysname = %q, want %q", found.SysName, fixSysName)
	}
	if found.PeersUp != 2 {
		t.Errorf("peers_up = %d, want 2", found.PeersUp)
	}
	// The u64-as-string rule, checked on the wire rather than on the type:
	// a Go field of type string cannot catch a marshaler that was changed
	// to emit a number.
	if found.SessionID != fmt.Sprint(fixSession) {
		t.Errorf("session_id = %q, want %q", found.SessionID, fmt.Sprint(fixSession))
	}
}

func TestHandlerPeers(t *testing.T) {
	s := requireAPI(t)

	t.Run("unfiltered reports both peers", func(t *testing.T) {
		var got []WirePeer
		getOK(t, s, "/v1/peers", &got)
		if len(got) < 2 {
			t.Fatalf("/v1/peers returned %d peers, want at least 2; %+v", len(got), got)
		}
	})

	t.Run("router= narrows", func(t *testing.T) {
		var got []WirePeer
		getOK(t, s, "/v1/peers?router="+fixRouterIP, &got)
		for _, p := range got {
			if p.RouterIP != fixRouterIP {
				t.Errorf("router=%s returned a peer under %s -- the filter is not "+
					"reaching query.PeerFilter", fixRouterIP, p.RouterIP)
			}
		}
		if len(got) != 2 {
			t.Errorf("router=%s returned %d peers, want 2", fixRouterIP, len(got))
		}
	})

	t.Run("a malformed router is 400, not 500", func(t *testing.T) {
		requireError(t, s, "/v1/peers?router=not-an-address",
			http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("a rib outside the contract's enum is 400, not 500", func(t *testing.T) {
		// query's own ribColumn doc comment makes this the HTTP layer's
		// job: an unknown Enum8 member does not compare false in
		// ClickHouse, it raises, which would be a 500 for a failure the
		// contract types as a 400.
		requireError(t, s, "/v1/peers?rib=not_a_rib",
			http.StatusBadRequest, ErrInvalidParam)
	})
}

// TestRoutesFanoutReturnsAllThreeKeysEvenWhenEmpty: the contract makes
// unicast, vpn and evpn all required, so a client can iterate them
// unconditionally. A handler that omits empty families forces every caller
// to write nil checks, and an LLM reading a response with no "evpn" key
// will report that EVPN was not searched rather than that it was empty.
func TestRoutesFanoutReturnsAllThreeKeysEvenWhenEmpty(t *testing.T) {
	s := requireAPI(t)
	rec := get(t, s, "/v1/routes?prefix=10.77.0.0/24")
	if rec.Code != http.StatusOK {
		t.Fatalf("fan-out = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"unicast", "vpn", "evpn"} {
		raw, ok := got.Data[k]
		if !ok {
			t.Errorf("fan-out response is missing the %q key; all three are "+
				"required so callers can iterate unconditionally", k)
			continue
		}
		// Present-but-null is the same defect wearing the required key: a
		// caller iterating it still has to nil-check.
		if string(raw) == "null" {
			t.Errorf("fan-out key %q is null, not []; an empty family is an empty "+
				"array, which is what says it was searched", k)
		}
	}
}

func TestHandlerRoutesFanout(t *testing.T) {
	s := requireAPI(t)

	t.Run("finds the prefix in both unicast and vpn", func(t *testing.T) {
		var got WireRouteFanout
		getOK(t, s, "/v1/routes?prefix=10.77.0.0/24", &got)
		if len(got.Unicast) != 2 {
			t.Errorf("unicast = %d routes, want 2 (two path_ids)", len(got.Unicast))
		}
		if len(got.VPN) != 1 {
			t.Errorf("vpn = %d routes, want 1", len(got.VPN))
		}
		if len(got.EVPN) != 0 {
			t.Errorf("evpn = %d routes, want 0", len(got.EVPN))
		}
	})

	t.Run("covers= finds the containing route", func(t *testing.T) {
		var got WireRouteFanout
		getOK(t, s, "/v1/routes?covers=10.77.0.33", &got)
		if len(got.Unicast) == 0 {
			t.Error("covers=10.77.0.33 found no unicast route, but 10.77.0.0/24 " +
				"contains it")
		}
	})

	t.Run("exactly one of prefix or covers is required", func(t *testing.T) {
		for _, tc := range []struct{ name, target string }{
			{"neither", "/v1/routes"},
			{"both", "/v1/routes?prefix=10.77.0.0/24&covers=10.77.0.33"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				requireError(t, s, tc.target, http.StatusBadRequest, ErrInvalidParam)
			})
		}
	})
}

// TestWideFilterAloneIsEnoughNarrowing: prefix= and covers= stay mutually
// exclusive with each other, but each wide filter is now a sufficient
// narrowing on its own -- at the requireNarrowing gate, all the way through
// to a 200 carrying real rows, on every route-bearing endpoint including
// the /v1/routes fan-out.
//
// "Real rows" is the point, and the reason each case below asserts non-empty
// data rather than only the status code: a bare 200 would also have passed
// against the empty-prefix predicate defect -- the one that bound the empty
// string to r.prefix on a query that had named no prefix at all, described
// at length in RouteFilter.Prefix's own doc comment. That defect answered
// every wide-filter-alone query with an empty array, which is a 200 exactly
// as much as a correct answer is. Every value chosen below is one the
// fixture genuinely carries: as_path is [65001, 65002] on every
// route_unicast row (65001 the first hop, 65002 the origin), and
// 10.77.2.0/24 alone carries the route target 65000:100 (see
// insertHandlerFixture's own comment).
//
// The fan-out case is the one worth spelling out beyond that. Before
// OriginASN, ThroughASN and Community were threaded into the VPN and EVPN
// arms handleRoutes builds, a lone ?origin_asn= reached those two arms as an
// entirely empty filter -- VPNRouteFilter.check and EVPNRouteFilter.check
// both gate on an unfiltered struct -- and was correctly refused: a fail-
// closed 400 rather than the fan-out silently answering from one family
// while treating the other two as unasked. Now that all three arms carry
// the wide filters, that same query is no longer an unfiltered dump of two
// of the three families; it is the slice's headline question -- "who
// carries this AS" -- asked of all three at once, and the honest answer is
// 200 with the unicast arm populated.
func TestWideFilterAloneIsEnoughNarrowing(t *testing.T) {
	s := requireAPI(t)

	t.Run("origin_asn alone, unicast", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, "/v1/routes/unicast?origin_asn=65002", &got)
		if len(got) == 0 {
			t.Fatal("origin_asn=65002 matched nothing; every unicast row in " +
				"the fixture originates in 65002")
		}
	})

	t.Run("through_asn alone, unicast", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, "/v1/routes/unicast?through_asn=65001", &got)
		if len(got) == 0 {
			t.Fatal("through_asn=65001 matched nothing; 65001 is the first hop " +
				"on every unicast row in the fixture")
		}
	})

	t.Run("community alone, unicast", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, "/v1/routes/unicast?community=65000:100", &got)
		if len(got) == 0 {
			t.Fatal("community=65000:100 matched nothing; 10.77.2.0/24 carries " +
				"it as a route target")
		}
	})

	t.Run("origin_asn alone, fan-out", func(t *testing.T) {
		var got WireRouteFanout
		getOK(t, s, "/v1/routes?origin_asn=65002", &got)
		if len(got.Unicast) == 0 {
			t.Fatal("origin_asn=65002 matched nothing in the fan-out's unicast " +
				"arm, on the same fixture the direct endpoint above just matched")
		}
	})

	requireError(t, s, "/v1/routes", http.StatusBadRequest, ErrInvalidParam)
	requireError(t, s, "/v1/routes?prefix=10.77.0.0/24&covers=10.77.0.33",
		http.StatusBadRequest, ErrInvalidParam)
}

func TestHandlerUnicastRoutes(t *testing.T) {
	s := requireAPI(t)

	t.Run("happy path", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, "/v1/routes/unicast?prefix=10.77.1.0/24", &got)
		if len(got) != 1 {
			t.Fatalf("got %d routes, want 1: %+v", len(got), got)
		}
		if got[0].Prefix != "10.77.1.0/24" {
			t.Errorf("prefix = %q, want 10.77.1.0/24", got[0].Prefix)
		}
	})

	t.Run("peer= narrows", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, "/v1/routes/unicast?prefix=10.88.0.0/24&peer="+fixPeerA, &got)
		if len(got) != 0 {
			t.Errorf("peer=%s returned peer B's route -- the peer filter is not "+
				"reaching query.RouteFilter: %+v", fixPeerA, got)
		}
	})

	t.Run("a family outside the enum is 400", func(t *testing.T) {
		requireError(t, s, "/v1/routes/unicast?prefix=10.77.1.0/24&family=vpn4",
			http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("exactly one of prefix or covers is required", func(t *testing.T) {
		requireError(t, s, "/v1/routes/unicast", http.StatusBadRequest, ErrInvalidParam)
		requireError(t, s, "/v1/routes/unicast?prefix=10.77.1.0/24&covers=10.77.1.1",
			http.StatusBadRequest, ErrInvalidParam)
	})
}

// TestRDOnUnicastIsRefusedNotSilentlyEmpty pins a defect confirmed
// directly with curl: GET /v1/routes/unicast?rd=65000:1 returned HTTP
// 200 with data: [] and total_matched: 0 -- an answer indistinguishable
// from "no route carries that RD," though rd= is not a parameter this
// endpoint has ever documented or read. requireNarrowing used to gate every
// route endpoint on one shared parameter list that happened to include
// rd= (a real parameter on /v1/routes/vpn and /v1/routes/evpn), so rd=
// alone satisfied the gate here too. RouteFilter.check has no narrowing
// rule of its own to catch what slipped through -- a wide filter alone is
// deliberately enough on RouteFilter, see its own doc comment -- so the
// request reached query as an entirely unfiltered RouteFilter and matched
// the literal empty prefix route_unicast has never held. Confirmed by
// direct curl against a running daemon without the per-endpoint check: 200,
// not 400.
func TestRDOnUnicastIsRefusedNotSilentlyEmpty(t *testing.T) {
	s := requireAPI(t)

	requireError(t, s, "/v1/routes/unicast?rd=65000:1",
		http.StatusBadRequest, ErrInvalidParam)

	// router= IS a real, documented parameter on /v1/routes/unicast, unlike
	// rd= above -- but the contract's narrowing rule for this endpoint was
	// never "prefix, covers OR router"; only the three wide filters ever
	// joined prefix and covers on that list. router= alone
	// satisfied the same overbroad shared gate and landed in exactly the
	// same r.prefix = '' hole -- also confirmed against a running daemon
	// without the per-endpoint check: 200 with an empty array, not 400.
	requireError(t, s, "/v1/routes/unicast?router="+fixRouterIP,
		http.StatusBadRequest, ErrInvalidParam)

	// family= is real on /v1/routes/evpn, but EVPNRouteFilter has no Family
	// field at all -- route_evpn holds one family by construction (see
	// EVPNRouteFilter's own doc comment). Unlike rd= above, family= was
	// never actually reachable through the OLD shared gate either (it was
	// never a member of the checked list), so this case was refused even
	// without the per-endpoint check. It is here as a regression guard on
	// the per-endpoint check, not as evidence of a second hole.
	requireError(t, s, "/v1/routes/evpn?family=vpn4",
		http.StatusBadRequest, ErrInvalidParam)
}

func TestHandlerVPNRoutes(t *testing.T) {
	s := requireAPI(t)

	t.Run("happy path", func(t *testing.T) {
		var got []WireVPNRoute
		getOK(t, s, "/v1/routes/vpn?prefix=10.77.0.0/24", &got)
		if len(got) != 1 {
			t.Fatalf("got %d routes, want 1: %+v", len(got), got)
		}
		if got[0].RD != "65000:1" {
			t.Errorf("rd = %q, want 65000:1", got[0].RD)
		}
	})

	t.Run("rd= alone is enough to narrow", func(t *testing.T) {
		var got []WireVPNRoute
		getOK(t, s, "/v1/routes/vpn?rd=65000:1", &got)
		if len(got) != 1 {
			t.Errorf("rd=65000:1 returned %d routes, want 1", len(got))
		}
	})

	t.Run("no narrowing parameter at all is 400", func(t *testing.T) {
		// query.VPNRouteFilter.check refuses this with ErrBadFilter; the
		// point of the test is that the handler maps that sentinel to 400
		// rather than letting it fall through to 500.
		requireError(t, s, "/v1/routes/vpn", http.StatusBadRequest, ErrInvalidParam)
	})
}

func TestHandlerEVPNRoutes(t *testing.T) {
	s := requireAPI(t)

	t.Run("empty but well formed", func(t *testing.T) {
		var got []WireEVPNRoute
		getOK(t, s, "/v1/routes/evpn?rd=65000:1", &got)
		if len(got) != 0 {
			t.Errorf("got %d routes, want 0 -- the fixture writes no EVPN", len(got))
		}
	})

	t.Run("no narrowing parameter at all is 400", func(t *testing.T) {
		requireError(t, s, "/v1/routes/evpn", http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("a type outside IANA's range is 400", func(t *testing.T) {
		requireError(t, s, "/v1/routes/evpn?rd=65000:1&type=12",
			http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("a non-numeric type is 400", func(t *testing.T) {
		requireError(t, s, "/v1/routes/evpn?rd=65000:1&type=two",
			http.StatusBadRequest, ErrInvalidParam)
	})
}

func TestHandlerHistory(t *testing.T) {
	s := requireAPI(t)

	t.Run("prefix is required", func(t *testing.T) {
		requireError(t, s, "/v1/routes/history", http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("since accepts a duration", func(t *testing.T) {
		var got []WireHistoryEvent
		getOK(t, s, "/v1/routes/history?prefix=10.77.1.0/24&since=99999h", &got)
		if len(got) == 0 {
			t.Error("since=99999h reaches back past the fixture's timestamp and " +
				"returned nothing; the duration is not being applied")
		}
	})

	t.Run("since accepts an RFC 3339 timestamp", func(t *testing.T) {
		var got []WireHistoryEvent
		getOK(t, s, "/v1/routes/history?prefix=10.77.1.0/24&since=2020-01-01T00:00:00Z", &got)
		if len(got) == 0 {
			t.Error("since=2020-01-01T00:00:00Z is before the fixture and returned " +
				"nothing; the timestamp is not being applied")
		}
	})

	t.Run("since defaults to 1h, which excludes the fixture", func(t *testing.T) {
		var got []WireHistoryEvent
		getOK(t, s, "/v1/routes/history?prefix=10.77.1.0/24", &got)
		if len(got) != 0 {
			t.Errorf("the default since= returned %d events; the fixture is dated "+
				"2026-08-01 and the contract's default is 1h, so the default is "+
				"not being applied", len(got))
		}
	})

	t.Run("a malformed since is 400", func(t *testing.T) {
		requireError(t, s, "/v1/routes/history?prefix=10.77.1.0/24&since=yesterday",
			http.StatusBadRequest, ErrInvalidParam)
	})
}

func TestHandlerRIBUnicastWalk(t *testing.T) {
	s := requireAPI(t)
	base := "/v1/rib/unicast?router=" + fixRouterIP + "&peer=" + fixPeerA + "&rib=in_pre"

	t.Run("router and peer are required", func(t *testing.T) {
		requireError(t, s, "/v1/rib/unicast", http.StatusBadRequest, ErrInvalidParam)
		requireError(t, s, "/v1/rib/unicast?router="+fixRouterIP,
			http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("every page carries the paginated_smear warning", func(t *testing.T) {
		var got []WireUnicastRoute
		meta := getOK(t, s, base, &got)
		if !hasWarning(meta.Warnings, WarnPaginatedSmear) {
			t.Errorf("a /v1/rib page carried warnings %+v, with no %s. The page set "+
				"is a smear over a live table and the contract says so on every page",
				meta.Warnings, WarnPaginatedSmear)
		}
	})

	t.Run("a full walk at limit 1 returns every route exactly once", func(t *testing.T) {
		seen := map[string]bool{}
		target := base + "&limit=1"
		for page := 1; ; page++ {
			if page > 10 {
				t.Fatal("the walk did not terminate in 10 pages; the fixture has 4 routes")
			}
			var got []WireUnicastRoute
			meta := getOK(t, s, target, &got)
			if len(got) != 1 {
				t.Fatalf("page %d returned %d routes at limit=1", page, len(got))
			}
			key := got[0].Prefix + "|" + fmt.Sprint(got[0].PathID)
			if seen[key] {
				t.Fatalf("page %d repeated %s -- the cursor did not advance", page, key)
			}
			seen[key] = true
			if meta.NextCursor == nil {
				break
			}
			target = base + "&limit=1&cursor=" + *meta.NextCursor
		}
		if len(seen) != 4 {
			t.Errorf("the walk saw %d distinct routes, want 4: %v", len(seen), seen)
		}
	})

	t.Run("the last page's next_cursor is present and null", func(t *testing.T) {
		var got []WireUnicastRoute
		meta := getOK(t, s, base, &got)
		if meta.NextCursor != nil {
			t.Errorf("a single page holding every route handed out next_cursor %q, "+
				"want null", *meta.NextCursor)
		}
		if !strings.Contains(get(t, s, base).Body.String(), `"next_cursor":null`) {
			t.Error("the last page omitted next_cursor rather than emitting null; " +
				"a client reading the key on every other page gets undefined here")
		}
	})

	t.Run("a tampered cursor is 400, not 500", func(t *testing.T) {
		requireError(t, s, base+"&cursor=not-a-cursor",
			http.StatusBadRequest, ErrInvalidParam)
	})

	// This subtest pins the half of the per-endpoint cursor codec
	// split that is easy to get backwards: a cursor with no collector and
	// no session_id is exactly the shape
	// query.PeerEventsPage issues for /v1/events, and params.unpinnedCursor
	// exists specifically to accept it there. /v1/rib/* must keep refusing
	// it -- see api/cursor.go's decodeCursor and this handler's own
	// p.cursor -- so this drives a real request through the real router
	// rather than testing decodeCursor in isolation (cursor_test.go already
	// does that exhaustively): what this
	// actually guards against is a handler wired to the wrong params
	// method, which no codec-level test can see.
	t.Run("a session-less cursor is still refused", func(t *testing.T) {
		cur := encodeCursor(query.RIBCursor{
			// No Collector, no SessionID: the zero values -- the shape
			// query.PeerEventsPage issues and /v1/events accepts.
			Router: netip.MustParseAddr(fixRouterIP),
			Peer:   netip.MustParseAddr(fixPeerA),
			RIB:    "in_pre",
			Last:   []any{"in_pre", "10.77.0.0/24", uint32(1)},
		})
		msg := requireError(t, s, base+"&cursor="+cur, http.StatusBadRequest, ErrInvalidParam)
		// Status and code alone do not distinguish the two paths, which is
		// the whole point of this subtest: wire p.cursor to
		// decodeCursorUnpinned and the request is still a 400
		// invalid_param, because query.ribPage refuses the unpinned cursor
		// a moment later. The REFUSAL MOVING from the api codec to the
		// query layer is exactly the miswiring described above, and
		// errBadCursor's text is the one observable that tells them apart
		// -- see api/cursor.go, which keeps this a separate sentinel from
		// query.ErrBadFilter precisely because the two carry different
		// remedies.
		if !strings.Contains(msg, errBadCursor.Error()) {
			t.Errorf("GET %s&cursor=<session-less> refused with %q, want a "+
				"message carrying %q; a 400 from anywhere else means the api "+
				"codec accepted this cursor and something downstream caught it",
				base, msg, errBadCursor.Error())
		}
	})

	t.Run("a limit above max_page is clamped, not refused", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, base+"&limit=999999999", &got)
	})

	t.Run("a limit below the contract's minimum is 400", func(t *testing.T) {
		requireError(t, s, base+"&limit=0", http.StatusBadRequest, ErrInvalidParam)
		requireError(t, s, base+"&limit=-1", http.StatusBadRequest, ErrInvalidParam)
		requireError(t, s, base+"&limit=lots", http.StatusBadRequest, ErrInvalidParam)
	})
}

func TestHandlerRIBVPNAndEVPN(t *testing.T) {
	s := requireAPI(t)

	t.Run("vpn", func(t *testing.T) {
		var got []WireVPNRoute
		meta := getOK(t, s, "/v1/rib/vpn?router="+fixRouterIP+"&peer="+fixPeerA, &got)
		if len(got) != 1 {
			t.Errorf("got %d VPN routes, want 1", len(got))
		}
		if !hasWarning(meta.Warnings, WarnPaginatedSmear) {
			t.Error("no paginated_smear on /v1/rib/vpn")
		}
	})

	t.Run("evpn is empty but well formed", func(t *testing.T) {
		var got []WireEVPNRoute
		meta := getOK(t, s, "/v1/rib/evpn?router="+fixRouterIP+"&peer="+fixPeerA, &got)
		if len(got) != 0 {
			t.Errorf("got %d EVPN routes, want 0", len(got))
		}
		if !hasWarning(meta.Warnings, WarnPaginatedSmear) {
			t.Error("no paginated_smear on /v1/rib/evpn")
		}
	})
}

// TestSessionDumpingWarningOnBothRowShapes pins a defect, made a
// test. The warning rule reads "any returned row reports a dump in
// progress", and the rows do not agree on how they report it: a route
// carries DumpState, one string, and a peer carries DumpStates, a map keyed
// by family. One implementation cannot serve both, so one test cannot
// cover both -- which is exactly how a handler ships with the warning
// working on /v1/routes and silently missing on /v1/peers.
//
// The three "ls ..." cases below are the same lesson learned a second time:
// three more DumpState-string row shapes were added later and none had a case
// here, so handleLSNodes shipped with no test standing between a deleted
// Warnings: dumpingWarning(...) line and a green package. They share the
// "routes" case's shape rather than a shape of
// their own, which is exactly why a missing case for them was so easy not
// to notice: the code to cover them already existed, three times over,
// with no test calling it on any of the three.
func TestSessionDumpingWarningOnBothRowShapes(t *testing.T) {
	s := requireAPI(t)

	t.Run("routes, where dump_state is a string", func(t *testing.T) {
		var got []WireUnicastRoute
		meta := getOK(t, s, "/v1/routes/unicast?prefix=10.77.1.0/24", &got)
		if len(got) == 0 {
			t.Fatal("no rows, so the warning has nothing to fire on")
		}
		if got[0].DumpState != "dumping" {
			t.Fatalf("fixture route dump_state = %q, want dumping -- the fixture "+
				"writes no eor_events marker, so this test's premise is broken",
				got[0].DumpState)
		}
		if !hasWarning(meta.Warnings, WarnSessionDumping) {
			t.Errorf("a row reported dump_state=dumping and meta.warnings is %+v. "+
				"This is the field that stops a partial answer being read as fact",
				meta.Warnings)
		}
	})

	// The three link-state shapes below repeat that lesson:
	// three new row shapes arrived later sharing the route shape's single
	// DumpState string, and none had a case here -- deleting
	// handleLSNodes' Warnings: dumpingWarning(...) block entirely left the
	// whole package green. insertLSFixture writes no ls_events
	// end-of-rib marker, so every row it writes reads dump_state "dumping",
	// the same premise the "routes" case above depends on.
	t.Run("ls nodes, where dump_state is a string", func(t *testing.T) {
		var got []WireLSNode
		meta := getOK(t, s, "/v1/ls/nodes", &got)
		if len(got) == 0 {
			t.Fatal("no rows, so the warning has nothing to fire on")
		}
		if got[0].DumpState != "dumping" {
			t.Fatalf("fixture ls node dump_state = %q, want dumping -- the fixture "+
				"writes no ls_events end-of-rib marker, so this test's premise is "+
				"broken", got[0].DumpState)
		}
		if !hasWarning(meta.Warnings, WarnSessionDumping) {
			t.Errorf("a row reported dump_state=dumping and meta.warnings is %+v. "+
				"This is the field that stops a partial answer being read as fact",
				meta.Warnings)
		}
	})

	t.Run("ls links, where dump_state is a string", func(t *testing.T) {
		var got []WireLSLink
		meta := getOK(t, s, "/v1/ls/links", &got)
		if len(got) == 0 {
			t.Fatal("no rows, so the warning has nothing to fire on")
		}
		if got[0].DumpState != "dumping" {
			t.Fatalf("fixture ls link dump_state = %q, want dumping -- the fixture "+
				"writes no ls_events end-of-rib marker, so this test's premise is "+
				"broken", got[0].DumpState)
		}
		if !hasWarning(meta.Warnings, WarnSessionDumping) {
			t.Errorf("a row reported dump_state=dumping and meta.warnings is %+v. "+
				"This is the field that stops a partial answer being read as fact",
				meta.Warnings)
		}
	})

	t.Run("ls prefixes, where dump_state is a string", func(t *testing.T) {
		var got []WireLSPrefix
		meta := getOK(t, s, "/v1/ls/prefixes", &got)
		if len(got) == 0 {
			t.Fatal("no rows, so the warning has nothing to fire on")
		}
		if got[0].DumpState != "dumping" {
			t.Fatalf("fixture ls prefix dump_state = %q, want dumping -- the fixture "+
				"writes no ls_events end-of-rib marker, so this test's premise is "+
				"broken", got[0].DumpState)
		}
		if !hasWarning(meta.Warnings, WarnSessionDumping) {
			t.Errorf("a row reported dump_state=dumping and meta.warnings is %+v. "+
				"This is the field that stops a partial answer being read as fact",
				meta.Warnings)
		}
	})

	t.Run("peers, where dump_states is a map", func(t *testing.T) {
		var got []WirePeer
		meta := getOK(t, s, "/v1/peers?router="+fixRouterIP, &got)
		if len(got) == 0 {
			t.Fatal("no peers, so the warning has nothing to fire on")
		}
		var dumping bool
		for _, p := range got {
			for _, st := range p.DumpStates {
				if st == "dumping" {
					dumping = true
				}
			}
		}
		if !dumping {
			t.Fatalf("no fixture peer reports a dumping family; premise broken: %+v", got)
		}
		if !hasWarning(meta.Warnings, WarnSessionDumping) {
			t.Errorf("a peer reported a dumping family and meta.warnings is %+v. "+
				"Peer.DumpStates is a map, not a string -- a check written for "+
				"the route shape does not see this", meta.Warnings)
		}
	})

	t.Run("history carries neither, and that is not an omission", func(t *testing.T) {
		var got []WireHistoryEvent
		meta := getOK(t, s, "/v1/routes/history?prefix=10.77.1.0/24&since=99999h", &got)
		if hasWarning(meta.Warnings, WarnSessionDumping) {
			t.Error("/v1/routes/history emitted session_dumping. HistoryEvent has no " +
				"dump state by design -- history is raw events, not current state, " +
				"so there is nothing for the warning to be derived from")
		}
	})
}

func hasWarning(ws Warnings, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestErrorBodiesDoNotEchoTheRequest guards the reflected-content hazard
// api/cursor.go's own comments raise: every 400 here is caused by
// caller-supplied text, and the easy way to write a helpful message is to
// quote it back.
func TestErrorBodiesDoNotEchoTheRequest(t *testing.T) {
	s := requireAPI(t)
	const marker = "zz-reflected-marker-zz"
	// A cursor is caller-supplied text too. It is opaque, not signed (see
	// api/cursor.go), so a caller can hand back a well-formed one carrying
	// any string it likes in the fields the codec does not constrain: the
	// collector, the rib scope, a key element's type tag and a String key
	// value. Each of these reaches a different 400 or 409 message.
	router, peer := netip.MustParseAddr(fixRouterIP), netip.MustParseAddr(fixPeerA)
	forged := func(c query.RIBCursor) string {
		c.Router, c.Peer = router, peer
		return url.QueryEscape(encodeCursor(c))
	}
	ribWalk := "/v1/rib/unicast?router=" + fixRouterIP + "&peer=" + fixPeerA
	// encodeCursor cannot write an unknown type tag, so that one cursor is
	// built from the wire document directly.
	badTag, err := json.Marshal(wireCursor{V: cursorVersion, C: fixCollector, S: "1",
		Rtr: fixRouterIP, Peer: fixPeerA, K: []string{marker + ":7"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/v1/peers?router=" + marker,
		"/v1/peers?rib=" + marker,
		"/v1/rib/unicast?router=" + fixRouterIP + "&peer=" + fixPeerA + "&cursor=" + marker,
		"/v1/routes/evpn?rd=65000:1&type=" + marker,
		"/v1/routes/history?prefix=10.77.1.0/24&since=" + marker,
		"/v1/routes/unicast?community=" + marker,
		// The three link-state paths. state= exists on all three and
		// once reached the caller verbatim through lsState's own p.fail
		// call, which is why it has entries of its own here. protocol=
		// exists on all three too, and
		// is checked once here (on nodes) rather than three times: p.protocol
		// is the one method every /v1/ls/* handler calls, so a fix there is
		// a fix on all three, and a regression on any of the three surfaces
		// here regardless of which path this entry names.
		"/v1/ls/nodes?state=" + marker,
		"/v1/ls/nodes?protocol=" + marker,
		"/v1/ls/links?state=" + marker,
		"/v1/ls/prefixes?state=" + marker,
		// /v1/topology's two 400 branches are requireTopologyScope's own, so
		// neither is covered by any entry above: the first names an
		// unscopeable request (rib= is present but never counts as a scope),
		// the second the prefix=/covers= pairing.
		"/v1/topology?rib=" + marker,
		"/v1/topology?prefix=" + marker + "&covers=" + marker,
		// covers= alone, on every path that reads it. Each is a scan
		// narrowing on its own, so no other parameter is needed to reach
		// the address check.
		"/v1/routes?covers=" + marker,
		"/v1/routes/unicast?covers=" + marker,
		"/v1/topology?covers=" + marker,
		"/v1/ls/prefixes?covers=" + marker,
		// /v1/ls/prefixes has no requireNarrowing, so its prefix=/covers=
		// pairing was refused only by query's own checkCovers, which quoted
		// both values.
		"/v1/ls/prefixes?prefix=" + marker + "&covers=10.0.0.1",
		// family= is passed through to query's own family check.
		"/v1/routes/unicast?origin_asn=65002&family=" + marker,
		"/v1/routes/vpn?origin_asn=65002&family=" + marker,
		// collector= on a RIB walk: a name with no session, and a name that
		// contradicts the cursor's own.
		ribWalk + "&collector=" + marker,
		ribWalk + "&collector=" + marker + "&cursor=" +
			forged(query.RIBCursor{Collector: fixCollector, SessionID: 1}),
		ribWalk + "&collector=other&cursor=" +
			forged(query.RIBCursor{Collector: marker, SessionID: 1}),
		// Forged cursors, one per field the codec passes through as text.
		ribWalk + "&cursor=" + forged(query.RIBCursor{Collector: fixCollector,
			SessionID: 1, RIB: marker}),
		ribWalk + "&cursor=" + forged(query.RIBCursor{Collector: fixCollector,
			SessionID: 1, Last: []any{marker, "10.0.0.0/24", uint32(7)}}),
		ribWalk + "&cursor=" + forged(query.RIBCursor{Collector: marker, SessionID: 1}),
		ribWalk + "&cursor=" + url.QueryEscape(base64.RawURLEncoding.EncodeToString(badTag)),
		"/v1/events?router=" + fixRouterIP + "&peer=" + fixPeerA + "&cursor=" +
			forged(query.RIBCursor{RIB: marker, Last: []any{uint64(1), uint64(1)}}),
	} {
		rec := get(t, s, target)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200; this case is supposed to fail", target)
			continue
		}
		if strings.Contains(rec.Body.String(), marker) {
			t.Errorf("GET %s echoed the caller's own text back in the body: %s",
				target, rec.Body.String())
		}
	}
}

// TestCoversIsRefusedByParameter: a bad covers= is refused by the api
// layer, in the request's own vocabulary. query's checkCovers refuses the
// same values, but in a Go caller's vocabulary -- the filter's field names --
// so a 400 carrying that message means the handler skipped its own check.
func TestCoversIsRefusedByParameter(t *testing.T) {
	s := requireAPI(t)
	for _, path := range []string{"/v1/routes", "/v1/routes/unicast", "/v1/topology", "/v1/ls/prefixes"} {
		for _, tc := range []struct{ covers, want string }{
			{"10.0.0.0/24", "covers= is not an IP address"},
			{url.QueryEscape("fe80::1%eth0"), "covers= carries an IPv6 zone"},
		} {
			msg := requireError(t, s, path+"?covers="+tc.covers, http.StatusBadRequest, ErrInvalidParam)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("GET %s?covers=%s: message %q, want it to contain %q",
					path, tc.covers, msg, tc.want)
			}
		}
	}
	msg := requireError(t, s, "/v1/ls/prefixes?prefix=10.0.0.0/24&covers=10.0.0.1",
		http.StatusBadRequest, ErrInvalidParam)
	if !strings.Contains(msg, "takes exactly one of prefix= or covers=") {
		t.Errorf("/v1/ls/prefixes with prefix= and covers=: message %q names no parameter", msg)
	}
}

// TestWideFilterParamsReachQuery: each parameter narrows, and a value that
// matches nothing returns an empty array rather than everything.
func TestWideFilterParamsReachQuery(t *testing.T) {
	s := requireAPI(t)
	var got []WireUnicastRoute
	getOK(t, s, "/v1/routes/unicast?origin_asn=65002", &got)
	if len(got) == 0 {
		t.Fatal("origin_asn=65002 matched nothing; the fixture's as_path ends " +
			"in 65002 on every unicast row, so the loop below would pass " +
			"vacuously on an ignored filter")
	}
	for _, r := range got {
		if len(r.ASPath) == 0 || r.ASPath[len(r.ASPath)-1] != 65002 {
			t.Errorf("origin_asn=65002 returned %s with as_path %v",
				r.Prefix, r.ASPath)
		}
	}

	var none []WireUnicastRoute
	getOK(t, s, "/v1/routes/unicast?origin_asn=64999", &none)
	if len(none) != 0 {
		t.Errorf("origin_asn=64999 matches no fixture row but returned %d routes; "+
			"an ignored filter reads as an unfiltered dump", len(none))
	}
}

// TestASNZeroIsRejected: AS 0 is reserved and originates nothing, so an
// empty result would be indistinguishable from a real absence.
func TestASNZeroIsRejected(t *testing.T) {
	s := requireAPI(t)
	for _, target := range []string{
		"/v1/routes?origin_asn=0",
		"/v1/routes?through_asn=0",
		"/v1/routes/unicast?origin_asn=0",
	} {
		requireError(t, s, target, http.StatusBadRequest, ErrInvalidParam)
	}
	requireError(t, s, "/v1/routes?origin_asn=notanumber",
		http.StatusBadRequest, ErrInvalidParam)
	requireError(t, s, "/v1/routes?origin_asn=4294967296",
		http.StatusBadRequest, ErrInvalidParam)
}

// TestCommunityColumnsAreDisclosed: the union search can leave an empty
// answer ambiguous, and meta is where that ambiguity is resolved.
func TestCommunityColumnsAreDisclosed(t *testing.T) {
	s := requireAPI(t)
	var got []WireUnicastRoute
	meta := getOK(t, s, "/v1/routes/unicast?community=65000:100", &got)
	want := []string{"live_communities", "live_route_targets"}
	if !slices.Equal(meta.CommunityColumns, want) {
		t.Errorf("meta.community_columns = %v, want %v -- without it a caller "+
			"cannot tell an absent community from one looked for in the wrong "+
			"column", meta.CommunityColumns, want)
	}
	if len(got) == 0 {
		t.Fatal("community=65000:100 matched nothing; 10.77.2.0/24 carries it " +
			"as a route target, and a disclosure test that never finds a row " +
			"cannot tell a real search from one that always comes back empty")
	}

	var noComm []WireUnicastRoute
	plain := getOK(t, s, "/v1/routes/unicast?prefix=10.77.1.0/24", &noComm)
	if len(plain.CommunityColumns) != 0 {
		t.Errorf("meta.community_columns = %v on a request with no community=",
			plain.CommunityColumns)
	}
}

// TestTruncatedAnswerCarriesTheHonestTotal is the one that matters most: an
// agent handed a capped answer with no total will report the cap.
//
// The URL below carries no ?limit= -- /v1/routes/unicast does not document
// one (api/openapi.yaml's own components.parameters.limit is $ref'd by the
// three /v1/rib/* paths only) and handleUnicastRoutes never reads one. The cap that makes this a truncation is entirely
// requireAPIWithMaxPage's Config.MaxPage: 1, which the handler applies as
// RouteFilter.Limit regardless of anything in the query string. There is
// no "&limit=1" in the request: it would do nothing here, and would look
// load-bearing only because MaxPage is also 1.
func TestTruncatedAnswerCarriesTheHonestTotal(t *testing.T) {
	s := requireAPIWithMaxPage(t, 1)
	var got []WireUnicastRoute
	meta := getOK(t, s, "/v1/routes/unicast?origin_asn=65002", &got)

	if len(got) != 1 {
		t.Fatalf("max_page=1 returned %d rows", len(got))
	}
	if meta.TotalMatched == nil {
		t.Fatal("meta.total_matched is null on a filtered answer")
	}
	if *meta.TotalMatched <= 1 {
		t.Fatalf("total_matched = %d, want more than the 1 row returned; the "+
			"fixture must match at least 2 routes for this to be a truncation",
			*meta.TotalMatched)
	}
	if !hasWarning(meta.Warnings, WarnTruncated) {
		t.Errorf("a capped answer carried warnings %+v with no %s",
			meta.Warnings, WarnTruncated)
	}
}

// TestFanoutTruncatesAndCarriesTheHonestTotal pins a defect: the fan-out's
// three arms used to carry no Limit at all, unlike the three single-family
// handlers, which all set Limit: s.cfg.MaxPage. So /v1/routes -- the
// headline route question, asked across all three families at once -- was
// the one route endpoint an operator could not safely point at a large
// answer: truncated could never fire, and total_matched always equaled the
// rows actually returned. Measured against a 3.2M-row table without the
// Limit: ~1.0s/6.9GiB and 1,050,000 rows matched, returned in full, three
// times concurrently.
func TestFanoutTruncatesAndCarriesTheHonestTotal(t *testing.T) {
	s := requireAPIWithMaxPage(t, 1)
	var got WireRouteFanout
	meta := getOK(t, s, "/v1/routes?origin_asn=65002", &got)

	if len(got.Unicast) != 1 {
		t.Fatalf("fan-out unicast arm returned %d rows at max_page=1, want 1 -- "+
			"the fixture's as_path ends in 65002 on every unicast row, so an "+
			"uncapped arm would return all of them here", len(got.Unicast))
	}
	returned := len(got.Unicast) + len(got.VPN) + len(got.EVPN)
	if meta.TotalMatched == nil {
		t.Fatal("meta.total_matched is null on a filtered fan-out answer")
	}
	if *meta.TotalMatched <= uint64(returned) {
		t.Fatalf("total_matched = %d, want more than the %d rows returned across "+
			"all three families -- the fixture must match more unicast rows "+
			"than max_page=1 lets through for this to be a genuine truncation",
			*meta.TotalMatched, returned)
	}
	if !hasWarning(meta.Warnings, WarnTruncated) {
		t.Errorf("a capped fan-out answer carried warnings %+v with no %s",
			meta.Warnings, WarnTruncated)
	}
}

// TestUntruncatedAnswerHasNoTruncatedWarning: the warning is a claim, and a
// warning that is always present carries no information.
func TestUntruncatedAnswerHasNoTruncatedWarning(t *testing.T) {
	s := requireAPI(t)
	var got []WireUnicastRoute
	meta := getOK(t, s, "/v1/routes/unicast?prefix=10.77.1.0/24", &got)
	if hasWarning(meta.Warnings, WarnTruncated) {
		t.Error("an answer that fit under the cap was reported as truncated")
	}
}

// TestLSNodesEndpointReturnsRealRows. A bare 200 would also pass against a
// handler whose filter never reached the statement, so every case in this
// file asserts on data.
func TestLSNodesEndpointReturnsRealRows(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	var nodes []WireLSNode
	getOK(t, s, "/v1/ls/nodes", &nodes)
	if len(nodes) == 0 {
		t.Fatal("/v1/ls/nodes returned no rows against a fixture that has them")
	}
}

// TestLSNodesEndpointDistinguishesAreaZeroFromAbsent pins the area=0
// hazard at the HTTP boundary, where it is easiest to
// reintroduce: the handler must tell "?area=0" from no area= at all, and a
// params helper returning a plain uint32 cannot.
func TestLSNodesEndpointDistinguishesAreaZeroFromAbsent(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	var all, zero []WireLSNode
	getOK(t, s, "/v1/ls/nodes", &all)
	getOK(t, s, "/v1/ls/nodes?area=0", &zero)
	if len(zero) == 0 {
		t.Fatal("area=0 returned nothing; the fixture puts nodes in area 0")
	}
	if len(zero) == len(all) {
		t.Errorf("area=0 returned all %d rows -- identical to no filter at all, "+
			"so the parameter was read as absent rather than as zero", len(all))
	}
	for _, n := range zero {
		if n.Area != 0 {
			t.Errorf("area=0 returned a node in area %d", n.Area)
		}
	}
}

// TestLSStateIsThreeDifferentAnswers: live, withdrawn and any must partition
// the fixture's nodes rather than any two of them agreeing by coincidence.
func TestLSStateIsThreeDifferentAnswers(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	var live, withdrawn, any []WireLSNode
	getOK(t, s, "/v1/ls/nodes", &live)
	getOK(t, s, "/v1/ls/nodes?state=withdrawn", &withdrawn)
	getOK(t, s, "/v1/ls/nodes?state=any", &any)
	if len(live) == 0 || len(withdrawn) == 0 {
		t.Fatalf("live=%d withdrawn=%d; both must be non-empty or the "+
			"partition assertion below is vacuous", len(live), len(withdrawn))
	}
	if len(any) != len(live)+len(withdrawn) {
		t.Errorf("any=%d, live=%d, withdrawn=%d -- the three states must "+
			"partition the answer", len(any), len(live), len(withdrawn))
	}
}

// TestLSEndpointsCarryTotalMatched: total_matched is the only thing that
// makes a capped answer honest, and a client cannot detect its absence.
func TestLSEndpointsCarryTotalMatched(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	for _, path := range []string{"/v1/ls/nodes", "/v1/ls/links", "/v1/ls/prefixes"} {
		meta := getMeta(t, s, path)
		if meta.TotalMatched == nil {
			t.Errorf("%s returned meta.total_matched = null", path)
		}
	}
}

// TestLSEndpointsTruncateAndSaySo, driven by a deliberately small max_page so
// the cap binds against a fixture that is otherwise far below it, on all
// three link-state endpoints rather than nodes alone.
//
// The three meta blocks that build TotalMatched, the dumping warning and the
// truncated warning are near-identical code across handleLSNodes,
// handleLSLinks and handleLSPrefixes -- and near-identical code is exactly
// what a shared test of only ONE of the three gives a false sense of
// covering. This is treated as an important defect for that reason:
// total_matched was already checked on all three (by
// TestLSEndpointsCarryTotalMatched), but the truncated warning was
// mutation-covered on handleLSNodes only, with handleLSLinks' and
// handleLSPrefixes' own truncatedWarning appends free to be deleted with
// nothing here to notice.
func TestLSEndpointsTruncateAndSaySo(t *testing.T) {
	s := requireAPIWithMaxPage(t, 1)

	t.Run("nodes", func(t *testing.T) {
		var nodes []WireLSNode
		meta := getOK(t, s, "/v1/ls/nodes", &nodes)
		if len(nodes) != 1 {
			t.Fatalf("max_page 1 returned %d rows", len(nodes))
		}
		if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
			t.Fatalf("total_matched is %v; it must exceed the returned row "+
				"count for this to be a truncation", meta.TotalMatched)
		}
		if !hasWarning(meta.Warnings, WarnTruncated) {
			t.Errorf("a capped answer carried no %q warning, so a caller is "+
				"told 1 row is the whole answer", WarnTruncated)
		}
	})

	t.Run("links", func(t *testing.T) {
		var links []WireLSLink
		meta := getOK(t, s, "/v1/ls/links", &links)
		if len(links) != 1 {
			t.Fatalf("max_page 1 returned %d rows", len(links))
		}
		if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
			t.Fatalf("total_matched is %v; it must exceed the returned row "+
				"count for this to be a truncation", meta.TotalMatched)
		}
		if !hasWarning(meta.Warnings, WarnTruncated) {
			t.Errorf("a capped answer carried no %q warning, so a caller is "+
				"told 1 row is the whole answer", WarnTruncated)
		}
	})

	t.Run("prefixes", func(t *testing.T) {
		var prefixes []WireLSPrefix
		meta := getOK(t, s, "/v1/ls/prefixes", &prefixes)
		if len(prefixes) != 1 {
			t.Fatalf("max_page 1 returned %d rows", len(prefixes))
		}
		if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
			t.Fatalf("total_matched is %v; it must exceed the returned row "+
				"count for this to be a truncation", meta.TotalMatched)
		}
		if !hasWarning(meta.Warnings, WarnTruncated) {
			t.Errorf("a capped answer carried no %q warning, so a caller is "+
				"told 1 row is the whole answer", WarnTruncated)
		}
	})
}

// TestLSEndpointsHonorLimitOnAnUnpaginatedRequest pins a defect:
// limit= was parsed and range-validated on every LS handler's
// non-paginating branch, then discarded -- f.Limit was hard-set to
// s.cfg.MaxPage regardless, so GET /v1/ls/nodes?limit=1 (and the same on
// links and prefixes) answered with every live row anyway. p.lsLimit
// already means "absent -> cfg.MaxPage, present -> validated and clamped";
// the fix wires its result into f.Limit on the non-paginating branch too,
// not only the paginating one.
//
// Unlike TestLSEndpointsTruncateAndSaySo, which forces the cap through a
// tiny cfg.MaxPage, this drives it entirely through the query parameter
// against requireAPIWithLSFixture's ordinary Config (MaxPage 10000): the
// fixture's unscoped, live answer is 3 rows on each of the three
// endpoints, so limit=1 must return exactly 1 row and still carry the
// truthful total_matched and truncated warning a real cap would.
func TestLSEndpointsHonorLimitOnAnUnpaginatedRequest(t *testing.T) {
	s := requireAPIWithLSFixture(t)

	t.Run("nodes", func(t *testing.T) {
		var nodes []WireLSNode
		meta := getOK(t, s, "/v1/ls/nodes?limit=1", &nodes)
		if len(nodes) != 1 {
			t.Fatalf("limit=1 returned %d rows, want 1", len(nodes))
		}
		if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
			t.Fatalf("total_matched is %v; it must exceed the returned row "+
				"count for this to be a truncation", meta.TotalMatched)
		}
		if !hasWarning(meta.Warnings, WarnTruncated) {
			t.Errorf("limit=1 carried no %q warning, so a caller is told 1 "+
				"row is the whole answer", WarnTruncated)
		}
	})

	t.Run("links", func(t *testing.T) {
		var links []WireLSLink
		meta := getOK(t, s, "/v1/ls/links?limit=1", &links)
		if len(links) != 1 {
			t.Fatalf("limit=1 returned %d rows, want 1", len(links))
		}
		if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
			t.Fatalf("total_matched is %v; it must exceed the returned row "+
				"count for this to be a truncation", meta.TotalMatched)
		}
		if !hasWarning(meta.Warnings, WarnTruncated) {
			t.Errorf("limit=1 carried no %q warning, so a caller is told 1 "+
				"row is the whole answer", WarnTruncated)
		}
	})

	t.Run("prefixes", func(t *testing.T) {
		var prefixes []WireLSPrefix
		meta := getOK(t, s, "/v1/ls/prefixes?limit=1", &prefixes)
		if len(prefixes) != 1 {
			t.Fatalf("limit=1 returned %d rows, want 1", len(prefixes))
		}
		if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
			t.Fatalf("total_matched is %v; it must exceed the returned row "+
				"count for this to be a truncation", meta.TotalMatched)
		}
		if !hasWarning(meta.Warnings, WarnTruncated) {
			t.Errorf("limit=1 carried no %q warning, so a caller is told 1 "+
				"row is the whole answer", WarnTruncated)
		}
	})
}

// TestLSEndpointsRejectWhatTheContractForbids. Each is a documented 400 and
// each must arrive as one, with a message naming the parameter -- checked
// case-insensitively, since query.ParseProtocol and checkCovers spell their
// parameter names with the Go field's own capitalization ("Covers") rather
// than the query string's.
func TestLSEndpointsRejectWhatTheContractForbids(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	for _, tc := range []struct {
		target string
		want   []string
	}{
		{"/v1/ls/nodes?state=livee", []string{"state"}},
		// protocol=0 and protocol=notaprotocol each check for MORE than the
		// parameter's own name: an earlier rewording (away from echoing the
		// caller's raw value) happened to also drop the accepted-name
		// enumeration entirely, and a bare substring check for "protocol"
		// stayed green through that regression -- the message could have
		// said almost anything containing that one word. isis-l2 and
		// ospfv2 are two of query.KnownProtocolNames' seven entries, so
		// this also fails if a future edit stops calling that function
		// and goes back to a generic "see the registry" message with no
		// names in it at all.
		{"/v1/ls/nodes?protocol=0", []string{"protocol", "isis-l2", "ospfv2"}},
		{"/v1/ls/nodes?protocol=notaprotocol", []string{"protocol", "isis-l2", "ospfv2"}},
		{"/v1/ls/nodes?area=4294967296", []string{"area"}},
		{"/v1/ls/nodes?node=notanumber", []string{"node"}},
		{"/v1/ls/prefixes?prefix=10.0.0.0/8&covers=10.0.0.1", []string{"covers"}},
	} {
		msg := requireError(t, s, tc.target, http.StatusBadRequest, ErrInvalidParam)
		lower := strings.ToLower(msg)
		for _, want := range tc.want {
			if !strings.Contains(lower, strings.ToLower(want)) {
				t.Errorf("GET %s error message %q does not contain %q", tc.target, msg, want)
			}
		}
	}
}

// TestLSContractParametersAllNarrow closes a gap: nothing in this
// repo ties a path's documented parameter list to the parameters its
// handler actually reads, and deleting remote_node from /v1/ls/links in
// api/openapi.yaml, as an experiment, proved the gap -- the entire api
// package stayed green, because nothing checked the contract's parameter
// list against what the handler does with a request.
//
// This closes that for the three link-state paths by walking the CONTRACT's
// own parsed parameter list -- loadContractDoc, the same parser
// TestEveryRouteIsDocumentedAndViceVersa uses, never a hand-copied list --
// and, for every parameter each path documents, issuing a request that sets
// ONLY that parameter to a value chosen to narrow the fixture's unfiltered
// answer. A parameter the handler never wires into its filter struct
// returns the same row count as the unfiltered baseline, which is what
// fails here: deleting one line from a handler's filter literal now costs
// exactly what deleting the matching line from the contract used to cost
// nothing at all.
//
// Because the parameter list is read from the document rather than
// restated, removing a parameter from the contract also removes it from
// this test -- which is the right direction for THAT change (a parameter
// nothing documents makes no promise for this test to hold the handler to)
// and is why this is a defense against the handler drifting from the
// contract, not a second copy of the contract to keep in sync by hand.
//
// Row counts, not status codes, per this file's own rule: a parameter that
// parses but is silently ignored is still a 200, and a bare status assertion
// would not catch it.
func TestLSContractParametersAllNarrow(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	doc := loadContractDoc(t)

	// common holds one narrowing value per parameter name, shared by every
	// path that documents it. Each excludes at least insertLSFixture's "-b"
	// row (protocol 3, area 1, asn 65200) from every path's answer; router=
	// and rib= have no real subset to narrow to in a one-router, one-rib
	// fixture, so they use a value no row carries instead -- see
	// lsFixBogusRouter and lsFixBogusRIB's own comments.
	common := map[string]string{
		"router":   lsFixBogusRouter,
		"peer":     fixPeerB,
		"rib":      lsFixBogusRIB,
		"protocol": lsFixProtocolName,
		"area":     "0",
		"asn":      fmt.Sprint(lsFixASN),
		"state":    "withdrawn",
		"prefix":   lsFixPrefix,
		"covers":   lsFixCoveredAddr,
	}

	baseline := map[string]int{}
	values := map[string]map[string]string{}

	var nodes []WireLSNode
	getOK(t, s, "/v1/ls/nodes", &nodes)
	if len(nodes) == 0 {
		t.Fatal("no fixture nodes; every parameter check below would be vacuous")
	}
	baseline["/v1/ls/nodes"] = len(nodes)
	// node= is read back from the unfiltered answer itself: node_key is a
	// cityHash64 this test has no way to predict ahead of a query.
	values["/v1/ls/nodes"] = withParam(common, "node", nodes[0].NodeKey)

	var links []WireLSLink
	getOK(t, s, "/v1/ls/links", &links)
	if len(links) == 0 {
		t.Fatal("no fixture links; every parameter check below would be vacuous")
	}
	baseline["/v1/ls/links"] = len(links)
	linkValues := withParam(common, "local_node", links[0].Local.NodeKey)
	linkValues["remote_node"] = links[0].Remote.NodeKey
	values["/v1/ls/links"] = linkValues

	var prefixes []WireLSPrefix
	getOK(t, s, "/v1/ls/prefixes", &prefixes)
	if len(prefixes) == 0 {
		t.Fatal("no fixture prefixes; every parameter check below would be vacuous")
	}
	baseline["/v1/ls/prefixes"] = len(prefixes)
	values["/v1/ls/prefixes"] = withParam(common, "node", prefixes[0].NodeKey)

	checked := 0
	for path, item := range doc.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/ls/") {
			continue
		}
		base, ok := baseline[path]
		if !ok {
			t.Fatalf("this test has no baseline row count for %s -- add one above", path)
		}
		for _, ref := range item.Get.Parameters {
			name := ref.Value.Name
			// cursor= and limit= are pagination controls, not narrowing
			// filters -- neither removes a row from the answer the way
			// every other parameter here does, so neither belongs in this
			// test's premise. cursor= specifically MUST NOT narrow the
			// fixture's unfiltered answer while unaccompanied by router=
			// and peer=: the query layer refuses that combination outright
			// (TestLSNodesRejectsACursorOnAnUnscopedRequest holds it for
			// /v1/ls/nodes, and handleLSLinks/handleLSPrefixes share the
			// same check), so a getOK call here would fail on the 400
			// rather than ever reach a row count to compare.
			if name == "cursor" || name == "limit" {
				continue
			}
			value, ok := values[path][name]
			if !ok {
				t.Fatalf("api/openapi.yaml documents %s= on %s but this test has no "+
					"narrowing value for it -- add one so the check is real, not "+
					"skipped", name, path)
			}
			checked++
			t.Run(path+" "+name, func(t *testing.T) {
				var got []json.RawMessage
				getOK(t, s, path+"?"+name+"="+value, &got)
				if len(got) == base {
					t.Errorf("%s?%s=%s returned %d rows, same as the %d-row "+
						"unfiltered answer -- %s= reached no predicate the handler "+
						"built", path, name, value, len(got), base, name)
				}
			})
		}
	}
	// A guard against the loop above silently checking LESS than it should:
	// 8 + 9 + 10 = 27 documented parameters across nodes, links and
	// prefixes respectively, per api/openapi.yaml.
	//
	// The exact count, not `checked == 0`, and the difference is the whole
	// point of the guard. Zero only catches the total collapse -- a
	// loadContractDoc that stopped parsing, or a /v1/ls/ prefix filter that
	// stopped matching anything. It says nothing about a path quietly
	// dropping one parameter, or about a fourth /v1/ls/ path arriving with
	// none of its parameters reaching the values map, both of which leave
	// the loop running and the count wrong. A number that has to be updated
	// when the contract changes is the point: this test is the thing that
	// notices the contract changed.
	if want := 27; checked != want {
		t.Errorf("checked %d parameters across /v1/ls/, want %d -- either "+
			"api/openapi.yaml gained or lost a link-state parameter (update "+
			"this number and the values map above), or the contract walk "+
			"stopped matching what it used to", checked, want)
	}
}

// withParam copies base and sets one additional key, so each path's value
// map can add its own node-identity parameter without the three paths'
// maps aliasing one underlying map and stepping on each other's node=,
// local_node= or remote_node= entry.
func withParam(base map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(base)+1)
	maps.Copy(out, base)
	out[key] = value
	return out
}

// TestLSNodesRejectsACursorOnAnUnscopedRequest pins the 400. Pagination needs
// a scope because an unscoped walk spans many sessions; accepting the cursor
// and ignoring the scope would page one router's nodes from another's
// position and skip everything that sorts before it.
func TestLSNodesRejectsACursorOnAnUnscopedRequest(t *testing.T) {
	s := requireAPI(t)
	// The cursor only has to decode -- it never reaches query, because the
	// handler's own scope check runs first. Its scope names the fixture's
	// real router and peer anyway, so a bug that let it through would walk
	// against real data rather than fail for an unrelated reason.
	cur := encodeCursor(query.RIBCursor{
		Collector: fixCollector,
		SessionID: fixSession,
		Router:    netip.MustParseAddr(fixRouterIP),
		Peer:      netip.MustParseAddr(fixPeerA),
	})
	msg := requireError(t, s, "/v1/ls/nodes?cursor="+cur, http.StatusBadRequest, ErrInvalidParam)
	lower := strings.ToLower(msg)
	for _, want := range []string{"router", "peer"} {
		if !strings.Contains(lower, want) {
			t.Errorf("GET /v1/ls/nodes?cursor=... (no router=, no peer=) error "+
				"message %q does not mention %q", msg, want)
		}
	}
}

// TestLSNodesUnscopedStillWarnsTruncated proves the old path is untouched:
// an unscoped request stays capped at max_page with the truncated warning,
// and meta.next_cursor stays null rather than becoming an invitation to
// page a walk this handler cannot pin to one session.
func TestLSNodesUnscopedStillWarnsTruncated(t *testing.T) {
	s := requireAPIWithMaxPage(t, 1)
	var nodes []WireLSNode
	meta := getOK(t, s, "/v1/ls/nodes", &nodes)
	if len(nodes) != 1 {
		t.Fatalf("max_page=1 returned %d rows, want 1", len(nodes))
	}
	if meta.TotalMatched == nil || *meta.TotalMatched <= 1 {
		t.Fatalf("total_matched is %v; it must exceed the returned row count "+
			"for this to be a truncation", meta.TotalMatched)
	}
	if !hasWarning(meta.Warnings, WarnTruncated) {
		t.Errorf("a capped unscoped answer carried no %s warning", WarnTruncated)
	}
	if meta.NextCursor != nil {
		t.Errorf("an unscoped answer carried next_cursor %q, want null -- an "+
			"unscoped walk has no session to pin a cursor to", *meta.NextCursor)
	}
}

// TestLSNodesScopedEmitsNextCursorAndWalks covers the happy path end to end
// through HTTP, not just the query layer: a request naming both router= and
// peer= pages via next_cursor, and walking every page at limit=1 returns
// exactly the rows the same scope's unpaginated (default-limit) answer does,
// no more and no fewer.
func TestLSNodesScopedEmitsNextCursorAndWalks(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	base := "/v1/ls/nodes?router=" + fixRouterIP + "&peer=" + fixPeerA

	var unpaginated []WireLSNode
	unpagMeta := getOK(t, s, base, &unpaginated)
	if len(unpaginated) == 0 {
		t.Fatal("the scoped, unpaginated answer is empty; the fixture has live " +
			"nodes for peer A")
	}
	if unpagMeta.NextCursor != nil {
		t.Fatalf("the scoped answer at the default limit carried a next_cursor; "+
			"the fixture's %d rows must fit in one page for the union check below "+
			"to compare against a complete baseline", len(unpaginated))
	}

	seen := map[string]bool{}
	target := base + "&limit=1"
	for page := 1; ; page++ {
		if page > len(unpaginated)+5 {
			t.Fatalf("the walk did not terminate in %d pages", page-1)
		}
		var got []WireLSNode
		meta := getOK(t, s, target, &got)
		if len(got) != 1 {
			t.Fatalf("page %d returned %d nodes at limit=1", page, len(got))
		}
		if seen[got[0].NodeKey] {
			t.Fatalf("page %d repeated node_key %s -- the cursor did not advance",
				page, got[0].NodeKey)
		}
		seen[got[0].NodeKey] = true
		if meta.NextCursor == nil {
			break
		}
		target = base + "&limit=1&cursor=" + *meta.NextCursor
	}
	if len(seen) != len(unpaginated) {
		t.Fatalf("the walk saw %d distinct nodes, want %d (the unpaginated scoped "+
			"answer)", len(seen), len(unpaginated))
	}
	for _, n := range unpaginated {
		if !seen[n.NodeKey] {
			t.Errorf("the walk never returned node_key %s, which the unpaginated "+
				"scoped answer carries", n.NodeKey)
		}
	}
}

// TestLSNodesUnscopedRejectsAMalformedLimit: limit= is a documented,
// range-checked parameter on /v1/ls/nodes -- the same shared component the
// /v1/rib paths reference -- and a malformed value must be rejected on an
// unscoped request exactly as on a scoped one. A caller who mistypes it
// must see a 400, not have it silently ignored the way it was before this
// parameter existed on this path at all.
func TestLSNodesUnscopedRejectsAMalformedLimit(t *testing.T) {
	s := requireAPI(t)
	for _, target := range []string{
		"/v1/ls/nodes?limit=notanumber",
		"/v1/ls/nodes?limit=0",
		"/v1/ls/nodes?limit=-1",
	} {
		requireError(t, s, target, http.StatusBadRequest, ErrInvalidParam)
	}
}

// TestLSNodesScopedNoLimitTakesMaxPageNotDefaultPage pins behavior 3 in a
// way requireAPIWithMaxPage cannot: that helper's Config sets DefaultPage
// and MaxPage to the same value, so a scoped walk capped at either one
// returns the same answer and the test cannot tell which default actually
// fired. Here DefaultPage (1) is smaller than the fixture's own 2 live
// nodes for (fixRouterIP, fixPeerA) and MaxPage (10) is not: if lsLimit
// ever regressed to cfg.DefaultPage for an absent limit=, this answer
// would be truncated to 1 row with a non-null next_cursor instead of the
// full 2 with none.
func TestLSNodesScopedNoLimitTakesMaxPageNotDefaultPage(t *testing.T) {
	s := requireAPIWithPageSizes(t, 1, 10)
	var nodes []WireLSNode
	meta := getOK(t, s, "/v1/ls/nodes?router="+fixRouterIP+"&peer="+fixPeerA, &nodes)
	if len(nodes) != 2 {
		t.Fatalf("scoped answer with no limit= returned %d nodes, want 2 -- either "+
			"the fixture changed or lsLimit is resolving an absent limit= to "+
			"cfg.DefaultPage (1) rather than cfg.MaxPage (10)", len(nodes))
	}
	if meta.NextCursor != nil {
		t.Errorf("scoped answer with no limit= carried next_cursor %q, want null -- "+
			"both of the fixture's live nodes for this scope must fit in one page "+
			"at cfg.MaxPage", *meta.NextCursor)
	}
}

// TestLSNodesScopedNarrowedRequestIsCappedNotPaginated pins the routing
// rule: a scoped request that also narrows
// by node identity (here, state=any) is answered like an unscoped one --
// capped, with a null next_cursor -- rather than paginated, because a
// cursor such a request emitted could never legally be resent alongside
// the same narrowing (query.LSNodeFilter.Narrowing and LSNodesPage's own
// cursor check refuse that combination outright). Without the cap, a
// request like this one reached LSNodesPage on page 1 (no cursor yet, so
// its narrowing check never fired) and could hand back a next_cursor that
// was guaranteed to 400 the moment the client resent it with the same
// state=any -- the one thing a next_cursor exists to support.
func TestLSNodesScopedNarrowedRequestIsCappedNotPaginated(t *testing.T) {
	s := requireAPIWithLSFixture(t)
	var nodes []WireLSNode
	meta := getOK(t, s,
		"/v1/ls/nodes?router="+fixRouterIP+"&peer="+fixPeerA+"&state=any", &nodes)
	if len(nodes) == 0 {
		t.Fatal("scoped, state=any returned no rows; the fixture carries a " +
			"withdrawn node for peer A that only state=any includes")
	}
	if meta.NextCursor != nil {
		t.Errorf("a scoped, narrowed request carried next_cursor %q, want null -- "+
			"pagination is unavailable once a request narrows by node identity, "+
			"and this request never carried a cursor= of its own to justify one",
			*meta.NextCursor)
	}
	if meta.TotalMatched == nil {
		t.Error("a scoped, narrowed (capped) answer carried no total_matched -- " +
			"it should be answered the same way an unscoped answer is")
	}
}

// TestAuthConfigIsPublic. If this route were gated, the app could never
// discover how to authenticate -- it would need a token to learn that it
// needs a token.
//
// newTestServerNoDB, because handleAuthConfig reads only s.cfg and this
// claim has to hold on every machine rather than only where a dev
// ClickHouse happens to be up. See that helper's comment.
func TestAuthConfigIsPublic(t *testing.T) {
	srv := newTestServerNoDB(t, testToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != string(AuthModeToken) {
		t.Errorf("mode = %q, want %q", got.Mode, AuthModeToken)
	}
}

// TestAuthConfigLeaksNoToken. The response is public, so anything that
// reached it would be readable by anyone -- and this is the one assertion
// of absence in the set, so it must not be a test that skips.
func TestAuthConfigLeaksNoToken(t *testing.T) {
	srv := newTestServerNoDB(t, "sup3rs3cr3ttokenvalue0123456789ab")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/config", nil))
	if strings.Contains(rec.Body.String(), "sup3rs3cr3t") {
		t.Fatalf("auth config response contains a configured token: %s", rec.Body.String())
	}
}

// TestAuthConfigReportsNoneMode is the value the frontend actually
// branches on -- whether to prompt for a token at all -- and neither test
// above exercises it: both build their server through newTestServer /
// newTestServerWithToken, whose Config leaves Auth.Mode at its zero value,
// so both only ever reach the mode == "" fallback in handleAuthConfig.
// Hard-coding mode := AuthModeToken there would leave every test in this
// file green.
//
// It builds the Server directly, with a nil *query.Q, rather than through
// this file's helpers -- which is the point, not a shortcut. Those helpers
// require a live ClickHouse via chtest.Require, which SKIPS (not fails)
// when one is unreachable, so a CI without a database silently drops this
// endpoint's real behavior from the suite. Nothing about /v1/auth/config
// needs a database: handleAuthConfig reads only s.cfg.Auth.Mode (see its
// body) and never s.q, and mode: none skips newAuth's token-list
// requirement in NewServer too, so this test has no database dependency at
// all and must keep running with none configured.
func TestAuthConfigReportsNoneMode(t *testing.T) {
	srv, err := NewServer(nil, Config{Auth: AuthConfig{Mode: AuthModeNone}}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	// A loopback Host, because none mode answers only the hosts it is
	// configured for and httptest's default Host is example.com.
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1/auth/config", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), `{"mode":"none"}`+"\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestHandlerRIBWalkNamesItsCollector pins the fix for a defect found in
// the web UI: the Routes screen was unusable on every dual-homed router.
//
// A RIB walk is pinned to one (collector, session) by construction, so a
// router two collectors monitor is genuinely ambiguous and query refuses it
// -- correctly: two collectors' RIBs are two answers and picking one
// silently is a collection artifact rendered as a fact about the network,
// the same mistake this project keeps re-finding in different layers.
// query has always had the remedy (RIBCursor's doc: "a nil or
// empty Last with a Collector and SessionID set means start at the
// beginning of THIS session"), but no HTTP caller could express it, so the
// web UI's Routes screen was unusable on every dual-homed router while
// telling the operator it had fallen back to one collector's view.
//
// collector= is that expression. It is not a loosening of the refusal: with
// no collector named, a dual-homed router is still a 400.
func TestHandlerRIBWalkNamesItsCollector(t *testing.T) {
	s := requireAPI(t)
	base := "/v1/rib/unicast?router=" + dualRouterIP + "&peer=" + dualPeer + "&rib=in_pre"

	t.Run("without a collector a dual-homed router is still refused", func(t *testing.T) {
		requireError(t, s, base, http.StatusBadRequest, ErrInvalidParam)
	})

	t.Run("collector= pins the walk to that collector's view", func(t *testing.T) {
		for _, tc := range []struct {
			collector string
			want      int
		}{{dualCollA, dualRoutesOnA}, {dualCollB, dualRoutesOnB}} {
			var got []WireUnicastRoute
			getOK(t, s, base+"&collector="+tc.collector, &got)
			if len(got) != tc.want {
				t.Errorf("collector=%s returned %d routes, want %d -- the walk must "+
					"report that collector's view whole, and neither the other's nor "+
					"the two merged", tc.collector, len(got), tc.want)
			}
		}
	})

	// The message matters, not just the status. Without the sid == 0 check
	// this is still a 400 -- the zero session fails the cursor's own "must
	// carry both a collector and a session" guard -- but it reaches the
	// operator as a complaint about a cursor they never sent, for a
	// collector name they did. Asserting only the code lets that through:
	// found by mutation.
	//
	// The message blames the collector without quoting the name: the name
	// is the caller's own text, and TestErrorBodiesDoNotEchoTheRequest
	// holds that no 400 body repeats it.
	t.Run("a collector that does not monitor this router is refused, and blamed", func(t *testing.T) {
		msg := requireError(t, s, base+"&collector="+fixCollector,
			http.StatusBadRequest, ErrInvalidParam)
		if !strings.Contains(msg, "collector has no session") || strings.Contains(msg, "cursor") {
			t.Errorf("the refusal was %q -- the caller's mistake was the collector "+
				"it chose, and the message has to say so rather than blame a cursor "+
				"the caller never sent", msg)
		}
	})

	// A cursor already carries its collector, so the two can contradict.
	// Resolving that silently either way would page one collector's walk
	// from another collector's position.
	t.Run("collector= contradicting the cursor is refused", func(t *testing.T) {
		var first []WireUnicastRoute
		meta := getOK(t, s, base+"&collector="+dualCollA+"&limit=1", &first)
		if meta.NextCursor == nil {
			t.Fatalf("the walk ended in one page of %d, so there is no cursor to "+
				"contradict; %s holds %d routes", len(first), dualCollA, dualRoutesOnA)
		}
		msg := requireError(t, s, base+"&collector="+dualCollB+
			"&cursor="+url.QueryEscape(*meta.NextCursor),
			http.StatusBadRequest, ErrInvalidParam)
		// It names both halves by PARAMETER, not by value: both collector
		// names are caller-supplied text (a cursor is opaque but not
		// signed), and an operator needs to know which half to drop, which
		// the parameter names say.
		if !strings.Contains(msg, "drop collector=") || !strings.Contains(msg, "drop the cursor") {
			t.Errorf("the refusal was %q; it has to name BOTH halves -- the "+
				"cursor and collector= -- or an operator cannot tell which to drop", msg)
		}
		// And the same cursor with a matching collector= is redundant, not
		// contradictory, so it must still page.
		var second []WireUnicastRoute
		getOK(t, s, base+"&collector="+dualCollA+
			"&cursor="+url.QueryEscape(*meta.NextCursor), &second)
		if len(second) == 0 {
			t.Error("a cursor passed alongside the collector it was issued for " +
				"returned nothing; agreement must page, not refuse")
		}
	})

	t.Run("collector= is accepted on a single-collector router too", func(t *testing.T) {
		var got []WireUnicastRoute
		getOK(t, s, "/v1/rib/unicast?router="+fixRouterIP+"&peer="+fixPeerA+
			"&rib=in_pre&collector="+fixCollector, &got)
		if len(got) == 0 {
			t.Error("naming the only collector a router has returned nothing; " +
				"collector= must narrow, never contradict an unambiguous pin")
		}
	})
}
