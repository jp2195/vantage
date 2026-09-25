package sink

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/collector"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// This file replays bgp/testdata/corpus -- real BMP captured from
// IOS-XR and NX-OS routers in a lab, not fixtures this repo wrote --
// through the same layers a production vantage-writer does: the collector's
// session layer, RowsFor, and a real ClickHouse Insert. Every other test in
// this package either stays pure (rows_test.go) or hand-builds an envelope
// (clickhouse_test.go's testEnv); this is the only one where nobody on this
// project chose the input bytes, which is what makes it able to catch a
// defect in the seam between two well-tested layers rather than in either
// layer alone.
//
// No build tag: sink's other live-ClickHouse tests
// (clickhouse_test.go) already establish the pattern for this -- gated by
// requireClickHouse/VANTAGE_REQUIRE_CLICKHOUSE, not a build tag -- so that
// `go test ./...` stays fast (a skip, not a full run) without ClickHouse,
// and so CI's existing `go test -race ./...` step (which does not pass
// `-tags integration`) actually executes this test rather than silently
// never running it.

// corpusCollectorClock is what replayCorpus injects as the collector's clock,
// and so what every replayed row's ts_collector column holds.
//
// It is sampled once per test binary rather than being a fixed epoch, and
// that is not a stylistic choice: ts_collector is what
// deploy/clickhouse/schema.sql keys PARTITION BY and TTL on. The obvious
// fixed clock -- time.Unix(0, 0) -- writes every row into partition 197001
// with a delete-TTL of 1970-04-01, i.e. already expired, and ClickHouse
// schedules an off-schedule merge that drops them. The rows insert without
// error and are simply gone moments later, which is exactly the production
// failure the ts_router-keyed TTL used to have (a router whose clock reads
// zero) reproduced against the fixtures. Sampling once, rather than per
// call, keeps two replays in one process byte-identical.
var corpusCollectorClock = time.Now().UTC().Truncate(time.Second)

// mustAddr parses a literal IPv4/IPv6 address for test setup. Every caller
// passes a hardcoded valid literal, so a parse failure here means the test
// itself is broken, not the code under test.
func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("mustAddr(%q): %v", s, err)
	}
	return a
}

// replayCorpus replays every *.bmpcap fixture under
// bgp/testdata/corpus through collector.Session (one fresh Session
// per file, as a real collector would run one per BMP TCP connection) and
// RowsFor, inserting every resulting row into ch as it goes -- one Insert
// call per envelope, exactly as cmd/vantage-writer's consumer does per
// batch. A bad row (the Peer Down / empty-IPv6 defect covered below)
// therefore fails loudly here, at the envelope that produced it, not
// silently later.
//
// It is deterministic within a test binary: the injected clock is
// corpusCollectorClock (fixed once per process, not sampled per call),
// filepath.Glob returns fixture paths in sorted order, and the
// stream-sequence counter starts at the same value on every call. Two calls
// -- one from each test below -- therefore produce byte-identical rows,
// which ReplacingMergeTree collapses at merge time rather than accumulating
// as distinct "duplicate" data. That is what makes each test below safe to
// call this independently, rather than relying on go test's file-order
// execution to have already seeded the database.
//
// Its hand-rolled seq counter starting at 1 occupies the same stream_seq
// space a real ROUTES stream does, and it replays router 10.0.103.62, which
// the dev collector also monitors -- so replaying this into the archive
// could put a fixture row and a production row on the same sort tuple,
// where ReplacingMergeTree would keep one and discard the other. It cannot:
// requireClickHouse points every test in this package at its own database
// (clickhouse_test.go's testDB), which is what makes replaying real router
// captures into a live server safe at all.
func replayCorpus(t *testing.T, ctx context.Context, ch *ClickHouse) (total, files int) {
	t.Helper()
	pattern := filepath.Join("..", "bgp", "testdata", "corpus", "*", "*.bmpcap")
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no corpus fixtures found matching %s: %v", pattern, err)
	}
	t0 := corpusCollectorClock
	routerIP := mustAddr(t, "10.0.103.62")
	var seq uint64
	for _, f := range paths {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// A fresh Session per file: each fixture is its own BMP TCP
		// connection in the lab, and reusing one Session across files
		// would let one capture's peer/session state leak into another's,
		// which is not what a real collector does.
		s := collector.NewSession(routerIP, "test", 1,
			func() time.Time { return t0 }, collector.Overrides{})
		r := bytes.NewReader(raw)
		for {
			m, err := bmp.ReadMsg(r)
			if err != nil {
				break // clean end of the concatenated stream, or a truncated
				// tail; collector/corpus_test.go already asserts
				// each fixture parses cleanly to EOF, so this test does not
				// re-litigate that and simply stops here either way.
			}
			for _, ev := range s.Handle(m) {
				seq++
				rows := mustRowsFor(t, ev.Env, seq)
				if err := ch.Insert(ctx, rows); err != nil {
					t.Fatalf("%s: insert: %v", f, err)
				}
				total += rows.Len()
			}
		}
	}
	return total, len(paths)
}

// countRows runs a count() query against the live ClickHouse and returns
// the result, failing the test on any query error. The queries below are
// written against "vantage.<table>", the names deploy/clickhouse/schema.sql
// uses; qualify redirects them to the tests' own database (see
// clickhouse_test.go's testDB).
func countRows(t *testing.T, ctx context.Context, ch *ClickHouse, query string) uint64 {
	t.Helper()
	var n uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, query)).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// TestCorpusToClickHouse replays the whole corpus end to end and asserts
// specific real values landed in ClickHouse, not just that some nonzero
// number of rows arrived -- a count-only assertion would have passed while
// the Peer Down bug was live for every table except peer_events, and the
// bug's own defining symptom (permanent redelivery) would look identical to
// "the writer just hasn't caught up yet" from a count alone.
//
// Real captured values used below, and where they come from (see
// collector/corpus_test.go's cases and bgp/testdata/corpus/*/*.bmpcap):
//   - iosxr/xrd-26.1.1-pe-vpn4.bmpcap: an ipv4u UPDATE announcing
//     172.16.1.0/24 and 172.16.2.0/24, and lu4 (labeled-unicast) prefixes
//     10.255.0.2/32 and 10.255.0.3/32 with MPLS label 3.
//   - iosxr/xrd-26.1.1-rr-vpn4-lu4-ls.bmpcap: a real vpn4 (MP_REACH AFI 1 /
//     SAFI 128) route, 10.2.0.4/30, RD 65000:102, label 24003 -- a route
//     the RR reflects rather than originates, so it is also add-path-free
//     RD-type-0 evidence distinct from the EVPN RDs below.
//   - nxos/n9kv-10.6.2F-leaf-evpn.bmpcap: a stats report with counter type
//     8 = 11, and EVPN type 2/3/5 routes (see
//     TestCorpusEvpnVniRoundTrips for the type-2 VNI assertion). Type 3
//     (Inclusive Multicast) RD 10.255.1.3:32777, and type 5 (IP Prefix)
//     192.168.10.0/24 carrying L3VNI label 50001.
//   - nxos/n9kv-10.6.2F-leaf-evpn.bmpcap and .../spine-evpn.bmpcap: real
//     Peer Up (leaf-evpn, peer 10.255.1.1, local 10.255.1.2:39836) and Peer
//     Down (leaf-evpn, peer 10.255.1.1, down_reason 1) events -- the second
//     is the defect class covered below.
//   - iosxr/xrd-26.1.1-p-linkstate.bmpcap: BGP-LS UPDATEs from peer
//     10.255.0.1. Attribute 29 (the BGP-LS Attribute) and BGP-LS NLRI (AFI
//     16388 / SAFI 71) now have typed decoders, so these UPDATEs no longer
//     reach ls_events at all (they used to, as raw MP_REACH bytes of length
//     110 and 58, before those decoders existed) -- RowsFor now decodes them
//     into ls_nodes/ls_links rows instead, and emits a raw ls_events row
//     only when RawReach/RawUnreach is actually non-empty (gating this on
//     "nothing typed decoded" instead would silently drop a mixed decode's
//     undecoded remainder). This fixture decodes fully cleanly -- no Prefix
//     NLRI (BGP-LS types 3/4) in it -- so RawReach/RawUnreach are empty and
//     it produces zero ls_events rows for this peer, not an ls_events row
//     with an empty raw_reach.
func TestCorpusToClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	total, files := replayCorpus(t, ctx, ch)
	if total == 0 {
		t.Fatal("corpus produced no rows at all")
	}
	t.Logf("inserted %d rows from %d fixtures", total, files)

	// Every table RAW does not feed (RAW has no RowsFor case and is
	// deliberately never consumed) must have received at least one row.
	// Table-by-table, not just a global total, so a family that decodes
	// but silently never reaches its table -- exactly the class of seam
	// defect this test exists for -- fails with a table name attached
	// rather than as an unexplained shortfall in a single combined count.
	//
	// ls_events is deliberately NOT in this list: the only
	// BGP-LS fixture in the corpus (xrd-26.1.1-p-linkstate.bmpcap) now
	// decodes fully cleanly into ls_nodes/ls_links, so RawReach/RawUnreach
	// are empty and the corpus legitimately produces zero raw ls_events
	// rows -- see TestCorpusToClickHouse's doc comment above. A prior
	// version of this loop included ls_events and only passed in a
	// full-package run because clickhouse_test.go (sorted first
	// alphabetically) happens to insert an unrelated LsRow into the same
	// shared, never-truncated test database; run in isolation
	// (-run TestCorpusToClickHouse) it failed with "got 0 rows from the
	// corpus replay". If a future fixture adds a BGP-LS capture with a
	// Prefix NLRI (types 3/4, which this build does not decode), that
	// fixture's mixed decode would leave a real raw ls_events row behind
	// and ls_events should be added back here.
	for _, table := range []string{
		"route_unicast", "route_vpn", "route_evpn",
		"ls_nodes", "ls_links", "peer_events", "stats_events",
	} {
		if n := countRows(t, ctx, ch, "SELECT count() FROM vantage."+table); n == 0 {
			t.Errorf("vantage.%s: got 0 rows from the corpus replay", table)
		}
	}

	cases := []struct {
		name  string
		query string
	}{
		{"route_unicast: 172.16.1.0/24 from xrd-26.1.1-pe-vpn4",
			`SELECT count() FROM vantage.route_unicast
			 WHERE family = 'ipv4u' AND prefix = '172.16.1.0/24' AND is_withdraw = 0`},
		{"route_unicast: 172.16.2.0/24 from xrd-26.1.1-pe-vpn4",
			`SELECT count() FROM vantage.route_unicast
			 WHERE family = 'ipv4u' AND prefix = '172.16.2.0/24' AND is_withdraw = 0`},
		{"route_vpn: lu4 10.255.0.3/32 label 3 from xrd-26.1.1-pe-vpn4",
			`SELECT count() FROM vantage.route_vpn
			 WHERE family = 'lu4' AND prefix = '10.255.0.3/32' AND has(labels, 3)`},
		{"route_vpn: real vpn4 10.2.0.4/30 RD 65000:102 label 24003 from xrd-26.1.1-rr-vpn4-lu4-ls",
			`SELECT count() FROM vantage.route_vpn
			 WHERE family = 'vpn4' AND prefix = '10.2.0.4/30' AND rd = '65000:102'
			   AND has(labels, 24003)`},
		{"route_evpn: type 3 (IMET) RD 10.255.1.3:32777 from n9kv leaf-evpn",
			`SELECT count() FROM vantage.route_evpn
			 WHERE route_type = 3 AND rd = '10.255.1.3:32777'`},
		{"route_evpn: type 5 (IP Prefix) 192.168.10.0/24 L3VNI 50001 from n9kv leaf-evpn",
			`SELECT count() FROM vantage.route_evpn
			 WHERE route_type = 5 AND prefix = '192.168.10.0/24' AND has(labels, 50001)`},
		{"stats_events: counter type 8 = 11 for peer 10.255.1.1 from n9kv leaf-evpn",
			`SELECT count() FROM vantage.stats_events
			 WHERE peer_ip = toIPv6('10.255.1.1') AND counters[8] = 11`},
		// This replaces a prior assertion that a clean typed decode still
		// left an ls_events row behind (with an empty raw_reach). That is
		// no longer true: a typed decode now lands in ls_nodes/ls_links
		// INSTEAD of ls_events, specifically so one UPDATE is not recorded
		// twice. So the seam proof for this fixture is now that the typed
		// tables received this peer's data, not that ls_events did --
		// value-level assertions on what landed there (node name, SRGB,
		// adjacency SID) belong to TestClickHouseLinkStateFromCorpus.
		{"ls_nodes: node decoded for peer 10.255.0.1 from xrd-26.1.1-p-linkstate",
			`SELECT count() FROM vantage.ls_nodes WHERE peer_ip = toIPv6('10.255.0.1')`},
		{"ls_links: link decoded for peer 10.255.0.1 from xrd-26.1.1-p-linkstate",
			`SELECT count() FROM vantage.ls_links WHERE peer_ip = toIPv6('10.255.0.1')`},
		{"peer_events: real Peer Up local_ip 10.255.1.2:39836 for peer 10.255.1.1 from n9kv leaf-evpn",
			`SELECT count() FROM vantage.peer_events
			 WHERE kind = 'up' AND peer_ip = toIPv6('10.255.1.1')
			   AND local_ip = toIPv6('10.255.1.2') AND local_port = 39836`},
		// This is the regression assertion for the Peer Down / empty-IPv6
		// defect: handlePeerDown never sets PeerEvent.LocalIp (a Peer Down
		// genuinely has no local address), and peer_events.local_ip is a
		// bare (non-nullable) IPv6 column that used to reject the empty
		// string outright, failing the insert, which -- because
		// ack-after-durable never acks a failed insert -- wedged the PEER
		// consumer into redelivering the same batch forever from the very
		// first real Peer Down. If clickHouseIPv6's "::" normalization in
		// rows.go ever regresses (or a future change bypasses it), this
		// query returns 0 and TestCorpusToClickHouse has already failed
		// outright above, at replayCorpus's Insert call for this exact
		// envelope -- the direct route bmp: this query is what proves the
		// row it left behind is not just "insert didn't error" but
		// queryable with the values a real Peer Down carries.
		{"peer_events: real Peer Down (down_reason 1, local_ip normalized to ::) for peer 10.255.1.1 from n9kv leaf-evpn",
			`SELECT count() FROM vantage.peer_events
			 WHERE kind = 'down' AND peer_ip = toIPv6('10.255.1.1')
			   AND down_reason = 1 AND local_ip = toIPv6('::')`},
		{"peer_events: second real Peer Down (down_reason 3) for peer 10.255.1.2 from n9kv spine-evpn",
			`SELECT count() FROM vantage.peer_events
			 WHERE kind = 'down' AND peer_ip = toIPv6('10.255.1.2')
			   AND down_reason = 3 AND local_ip = toIPv6('::')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := countRows(t, ctx, ch, c.query); n == 0 {
				t.Errorf("got 0 matching rows, want at least 1\nquery: %s", c.query)
			}
		})
	}
}

// TestCorpusEvpnVniRoundTrips is the one assertion that carries all the way
// from a real Nexus through the collector, the translation and into the
// database: a type-2 (MAC/IP Advertisement) EVPN route from the NX-OS
// fixtures, carrying MAC 52:54:00:33:4f:88 and the configured L2VNI 10010 in
// its label. EVPN labels are forwarded unshifted (see collector/session.go's
// evpnProto doc comment) -- a decoder that instead applied VPN-style
// MPLS-label semantics (value >> 4) would have stored 625, not 10010, so a
// wrong-shift regression here would show up as this query returning 0 while a
// differently-shaped one (has(labels, 625)) returned rows instead.
//
// Self-contained: it replays the corpus itself rather than depending on
// TestCorpusToClickHouse having already run in the same process, so it
// passes or fails on its own under `go test -run TestCorpusEvpnVniRoundTrips`.
// replayCorpus's determinism (see its doc comment) makes a second replay
// against the same database safe.
func TestCorpusEvpnVniRoundTrips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	if total, files := replayCorpus(t, ctx, ch); total == 0 {
		t.Fatalf("corpus produced no rows at all (%d fixtures)", files)
	}

	const q = `SELECT count() FROM vantage.route_evpn WHERE route_type = 2 AND has(labels, 10010)`
	if n := countRows(t, ctx, ch, q); n == 0 {
		t.Errorf("got 0 route_evpn rows with route_type=2 and VNI 10010, want at least 1\nquery: %s", q)
	}

	// The negative half of the same assertion: a decoder that shifted the
	// label as if it were an MPLS VPN label would store 625
	// (10010 >> 4), not 10010. This must be 0 for the positive assertion
	// above to mean what it claims.
	const wrong = `SELECT count() FROM vantage.route_evpn WHERE route_type = 2 AND has(labels, 625)`
	if n := countRows(t, ctx, ch, wrong); n != 0 {
		t.Errorf("got %d route_evpn rows with route_type=2 and label 625 -- this is the "+
			"MPLS-shifted value 10010 would become under VPN-style label semantics; its "+
			"presence means the EVPN label path is shifting VNIs it must not", n)
	}
}

// TestClickHouseLinkStateFromCorpus asserts specific values decoded by hand
// from xrd-26.1.1-p-linkstate.bmpcap's captured bytes before any of this
// code existed, so it checks the decoder against the wire rather than
// against itself.
//
// adj_sids is Array(UInt32) (RFC 9085 2.2.1 permits several adjacency SIDs
// per link, typically a protected/unprotected pair), not a scalar column, so
// existence is length(adj_sids) > 0 and the value is adj_sids[1] --
// ClickHouse arrays are 1-indexed.
//
// Deliberately no topology-join assertion here: this fixture's one Node NLRI
// (10.255.0.1, xr-rr1) and its two Link NLRIs (10.255.0.2<->10.255.0.5,
// 10.255.0.3<->10.255.0.4) are disjoint sets -- the node's router-id never
// appears as either link's endpoint anywhere in the corpus (confirmed by
// decoding the fixture directly and sweeping the rest of
// bgp/testdata/corpus for any other BGP-LS content).
// That is a fact about this capture, not a defect in the derived keys --
// TestClickHouseTopologyJoinMatchesOnDerivedKeys below proves the key
// expressions themselves agree, without depending on the corpus having a
// node and link that happen to share an endpoint.
func TestClickHouseLinkStateFromCorpus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	if total, files := replayCorpus(t, ctx, ch); total == 0 {
		t.Fatalf("corpus produced no rows at all (%d fixtures)", files)
	}

	var name string
	var base, size uint32
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT name, srgb_base, srgb_size FROM vantage.ls_nodes WHERE name = 'xr-rr1' LIMIT 1")).
		Scan(&name, &base, &size); err != nil {
		t.Fatalf("query ls_nodes: %v", err)
	}
	if base != 16000 || size != 8000 {
		t.Errorf("xr-rr1 SRGB = %d+%d, want 16000+8000", base, size)
	}

	var srlbBase, srlbSize uint32
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT srlb_base, srlb_size FROM vantage.ls_nodes WHERE name = 'xr-rr1' LIMIT 1")).
		Scan(&srlbBase, &srlbSize); err != nil {
		t.Fatalf("query ls_nodes (SRLB): %v", err)
	}
	if srlbBase != 15000 || srlbSize != 1000 {
		t.Errorf("xr-rr1 SRLB = %d+%d, want 15000+1000", srlbBase, srlbSize)
	}

	// Scoped to the sender, not merely to "a row with adj_sids". These
	// values were hand-decoded from the IOS-XR capture, and the corpus now
	// also carries an NX-OS topology whose links have adjacency SIDs of their
	// own -- so an unscoped LIMIT 1 asserts against whichever row ClickHouse
	// happens to return first. It was correct only while one fixture supplied
	// every adj_sid, which is not a property a corpus keeps.
	var adjSID uint32
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT adj_sids[1] FROM vantage.ls_links "+
			"WHERE router_sysname = 'xr-p1' AND length(adj_sids) > 0 LIMIT 1")).
		Scan(&adjSID); err != nil {
		t.Fatalf("query ls_links (adj_sids): %v", err)
	}
	if adjSID != 24001 {
		t.Errorf("adjacency SID = %d, want 24001", adjSID)
	}

	var maxBW float32
	var teMetric uint32
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT max_bandwidth, te_metric FROM vantage.ls_links "+
			"WHERE router_sysname = 'xr-p1' AND max_bandwidth > 0 "+
			"ORDER BY max_bandwidth DESC LIMIT 1")).
		Scan(&maxBW, &teMetric); err != nil {
		t.Fatalf("query ls_links (max_bandwidth/te_metric): %v", err)
	}
	const wantBW = 1.25e8 // 1 Gbps in bytes/sec
	if maxBW < wantBW*0.99 || maxBW > wantBW*1.01 {
		t.Errorf("max link bandwidth = %v, want ~%v (1 Gbps)", maxBW, wantBW)
	}
	if teMetric != 1 {
		t.Errorf("TE metric = %d, want 1", teMetric)
	}

	// XRd 26.1.1 sends TLV 258 (Link Local/Remote
	// Identifiers) inside the BGP-LS Attribute, not the Link NLRI --
	// DecodeLsAttrs previously had no case for 258 at all, so
	// link_local_id/link_remote_id were always 0 for every row this corpus
	// produces. Both columns are in ls_links' ORDER BY identity tail, so a
	// 0/0 pair (indistinguishable from an unnumbered link) is exactly the
	// condition that let two parallel adjacencies between one router pair
	// collapse onto one sort tuple and lose one at merge time -- queried
	// here against the real, replayed, merged table, not just the in-memory
	// decode bgp.TestDecodeLsAttrsLink already covers.
	var remoteID uint32
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT link_remote_id FROM vantage.ls_links "+
			"WHERE router_sysname = 'xr-p1' AND link_local_id = 4")).
		Scan(&remoteID); err != nil {
		t.Fatalf("query ls_links (link_local_id=4): %v", err)
	}
	if remoteID != 3 {
		t.Errorf("link_remote_id for link_local_id=4 = %d, want 3 (the real captured attribute-side value)", remoteID)
	}

	// router_id_v4 is decoded and put on the wire, and the writer must
	// read it: without that, ls_nodes.router_id stays hex-only
	// ("0aff0001"), defeating the whole point of a topology visualization.
	// Queried here against the real, replayed table. The whole corpus is
	// replayed, and other captures carry xr-rr1 Node NLRI without the
	// router-ID attribute, so its rows mix 10.255.0.1 with empty values: a
	// single unordered row proves nothing either way. At least one row must
	// carry the captured router ID, and no row may carry any other.
	var withID, otherID uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT countIf(router_id_v4 = '10.255.0.1'), "+
			"countIf(router_id_v4 NOT IN ('', '10.255.0.1')) "+
			"FROM vantage.ls_nodes WHERE name = 'xr-rr1'")).
		Scan(&withID, &otherID); err != nil {
		t.Fatalf("query ls_nodes (router_id_v4): %v", err)
	}
	if withID == 0 {
		t.Errorf("no xr-rr1 row has router_id_v4 10.255.0.1; the writer is not reading the decoded router ID")
	}
	if otherID != 0 {
		t.Errorf("%d xr-rr1 rows have a router_id_v4 other than 10.255.0.1", otherID)
	}
}

// TestClickHouseTopologyJoinMatchesOnDerivedKeys is the whole point of the
// descriptor and key columns: ls_links.local_node_key and ls_nodes.node_key
// are MATERIALIZED by ClickHouse -- independently, in two different tables
// -- from the same cityHash64 expression over the node-descriptor tuple (see
// schema.sql), so a link's local endpoint must join back to the node that
// describes it. A join that silently matches nothing produces an empty
// topology graph rather than an error, which is precisely the failure these
// derived key columns exist to prevent -- so it is asserted rather than
// assumed.
//
// This does NOT replay the corpus. xrd-26.1.1-p-linkstate.bmpcap's one Node
// NLRI (10.255.0.1, xr-rr1) and its two Link NLRIs (10.255.0.2<->10.255.0.5,
// 10.255.0.3<->10.255.0.4) are disjoint sets -- confirmed by decoding the
// fixture directly and sweeping the rest of bgp/testdata/corpus, which has
// no other BGP-LS content at all -- so no corpus replay can exercise this
// join; there is no shared descriptor anywhere in the committed bytes. What
// the join actually depends on is a property of the two cityHash64
// expressions and the columns feeding them, not of any particular
// capture, so this test constructs a node and a link whose LOCAL
// descriptor is deliberately identical (same protocol,
// identifier, asn, bgpls_id, area, router_id) and pushes both through the
// real RowsFor -> Insert path -- not by writing node_key/local_node_key
// values directly, which would prove nothing about whether the two
// MATERIALIZED expressions actually agree.
func TestClickHouseTopologyJoinMatchesOnDerivedKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	// A router-id (10.255.9.9) not used by any other test in this package,
	// so the WHERE clause below cannot accidentally match a row some other
	// test left behind in the shared, never-truncated testDB.
	local := &vantagev1.LsNodeDescriptor{
		Asn: 65000, BgplsId: 0, Area: 0, RouterId: []byte{10, 255, 9, 9},
	}
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "10.0.103.62", SysName: "xr-p1"},
		Peer:        &vantagev1.PeerId{Ip: "10.255.0.1"},
		TsCollector: timestamppb.New(corpusCollectorClock),
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{{
				Protocol: 3, Identifier: 100, Name: "synthetic-join-node",
				Local: local,
			}},
			Links: []*vantagev1.LsLink{{
				Protocol: 3, Identifier: 100,
				Local:  local,
				Remote: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 9, 8}},
			}},
		}},
	}
	if err := ch.Insert(ctx, mustRowsFor(t, env, 1)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	routerID := hex.EncodeToString(local.GetRouterId())
	var edges uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, fmt.Sprintf(
		"SELECT count() FROM vantage.ls_links AS l "+
			"INNER JOIN vantage.ls_nodes AS n ON n.node_key = l.local_node_key "+
			"WHERE l.local_router_id = '%s' AND n.router_id = '%s'",
		routerID, routerID))).
		Scan(&edges); err != nil {
		t.Fatalf("topology join: %v", err)
	}
	if edges == 0 {
		t.Fatal("node_key and local_node_key, independently computed by ClickHouse " +
			"from the same descriptor tuple in two different tables, did not meet")
	}
}

// The FRR Loc-RIB capture must land as rib = 'loc_rib', end to end, from real
// wire bytes through the collector's session layer and RowsFor into
// ClickHouse.
//
// bgp/testdata/corpus/frr/frr-10.3-locrib.bmpcap is FRR 10.3 configured with
// `bmp monitor ipv4 unicast loc-rib` and no BGP neighbors at all, so every
// route-monitoring message in it is Loc-RIB by construction (RFC 9069 peer
// type 3, peer address 0.0.0.0, BGP-ID 10.99.0.1). That makes it the one
// fixture where a single misrouted row is unambiguous: there is no adj-RIB-in
// traffic in the file for a loc_rib row to be confused with, and no Loc-RIB
// traffic in any other file for these assertions to catch by accident.
//
// Without a loc_rib value, every one of these rows would land as rib =
// 'in_pre' with peer_ip = '0.0.0.0', indistinguishable from a pre-policy
// route received from a peer at 0.0.0.0 -- so "how many routes did we
// receive" would silently count the router's own post-best-path table. The
// second assertion below is the regression: not merely that loc_rib rows
// exist, but that no row from this sender escaped into one of the four
// adj-RIB directions.
func TestCorpusFrrLocRibIsNotCountedAsReceived(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	replayCorpus(t, ctx, ch)

	// peer_bgp_id identifies the FRR sender: replayCorpus gives every fixture
	// the same router_ip, so the BGP identifier is what separates this
	// capture's rows from IOS-XR's and NX-OS's.
	const frr = `peer_bgp_id = '10.99.0.1'`
	locRib := countRows(t, ctx, ch,
		`SELECT count() FROM vantage.route_unicast WHERE `+frr+` AND rib = 'loc_rib'`)
	if locRib == 0 {
		t.Error("no loc_rib rows from the FRR capture; either the fixture stopped " +
			"being replayed or peer type 3 stopped mapping to loc_rib")
	}
	if miscounted := countRows(t, ctx, ch,
		`SELECT count() FROM vantage.route_unicast WHERE `+frr+` AND rib != 'loc_rib'`); miscounted != 0 {
		t.Errorf("%d Loc-RIB rows stored under an adj-RIB direction; every route in "+
			"this capture is Loc-RIB, so any such row inflates a receive count", miscounted)
	}
	// RFC 9069 §4.2: the Loc-RIB peer carries no peer address. Storing
	// 0.0.0.0 is correct and is half of why the old behavior was ambiguous
	// -- it is only unambiguous now because rib says loc_rib alongside it.
	if wrong := countRows(t, ctx, ch,
		`SELECT count() FROM vantage.route_unicast WHERE rib = 'loc_rib' AND peer_ip != toIPv6('::ffff:0.0.0.0')`); wrong != 0 {
		t.Errorf("%d loc_rib rows carry a peer address; RFC 9069 Loc-RIB has none", wrong)
	}
	// The Loc-RIB peer's own Peer Up lands in peer_events, which carries the
	// same rib column. A peer_events row still saying in_pre would leave the
	// session that produced these routes filed under a direction its routes
	// are not in -- and peer_events is a table the migration is easy to
	// forget, since it holds no routes.
	if peerWrong := countRows(t, ctx, ch,
		`SELECT count() FROM vantage.peer_events WHERE `+frr+` AND rib != 'loc_rib'`); peerWrong != 0 {
		t.Errorf("%d FRR peer_events rows are not rib = 'loc_rib'", peerWrong)
	}
}

// The corpus could not demonstrate a topology until this fixture existed:
// xrd-26.1.1-p-linkstate.bmpcap holds one Node NLRI and two Link NLRI
// between *other* routers, so the node is never an endpoint of either
// link and a join over corpus data returns zero edges -- correctly. The
// join mechanism was provable only with
// hand-constructed descriptors (TestClickHouseTopologyJoinMatchesOnDerivedKeys),
// which tests ClickHouse's cityHash64 against itself, not against a router.
//
// n9kv-10.6.2-p3-topology.bmpcap is NX-OS 10.6(2) advertising a converged
// 7-node, 20-edge OSPFv2 topology, captured on a cold boot at the moment the
// BMP session came up -- so it is a full dump rather than an increment, which
// is what IOS-XR did not send by default (measured 2026-08-17: XRd 26.1.1
// with no explicit `initial-refresh` sent only incremental changes across
// four BMP session cycles, never the table it held; see docs/quirks.md for
// what `initial-refresh` did and did not change).
//
// What this asserts is the property the Node Graph depends on and nothing
// else in the corpus can show: every link endpoint resolves to a node
// advertised in the same capture. An edge whose endpoint is missing does not
// render in Grafana and raises no error, so without a fixture like this the
// failure mode is a silently smaller graph.
func TestCorpusNxosTopologyJoinsWithoutOrphanEdges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	const fixture = "n9kv-10.6.2-p3-topology.bmpcap"
	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos", fixture)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// A router IP no other test in this package replays, so the assertions
	// below cannot be satisfied by another fixture's rows in the shared,
	// never-truncated testDB.
	routerIP := mustAddr(t, "10.0.199.74")
	t0 := corpusCollectorClock
	s := collector.NewSession(routerIP, "corpus-topo", 1,
		func() time.Time { return t0 }, collector.Overrides{})
	r := bytes.NewReader(raw)
	var seq uint64
	for {
		m, err := bmp.ReadMsg(r)
		if err != nil {
			break
		}
		for _, ev := range s.Handle(m) {
			seq++
			if err := ch.Insert(ctx, mustRowsFor(t, ev.Env, seq)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	const scope = `router_ip = toIPv6('10.0.199.74')`
	nodes := countRows(t, ctx, ch,
		"SELECT countDistinct(node_key) FROM vantage.ls_nodes FINAL WHERE "+scope)
	edges := countRows(t, ctx, ch,
		"SELECT countDistinct((local_node_key, remote_node_key)) FROM vantage.ls_links FINAL WHERE "+scope)
	if nodes != 7 {
		t.Errorf("fixture yielded %d distinct nodes, want 7", nodes)
	}
	if edges != 20 {
		t.Errorf("fixture yielded %d distinct edges, want 20", edges)
	}

	// The assertion that matters: no edge endpoint is missing from ls_nodes.
	// ClickHouse derives node_key and local/remote_node_key independently in
	// two tables from the same descriptor tuple, so a mismatch here means the
	// two MATERIALIZED expressions have drifted apart -- which yields an empty
	// graph rather than an error.
	orphans := countRows(t, ctx, ch, `
SELECT count() FROM (
    SELECT local_node_key AS k FROM vantage.ls_links FINAL WHERE `+scope+`
    UNION DISTINCT
    SELECT remote_node_key AS k FROM vantage.ls_links FINAL WHERE `+scope+`
) WHERE k NOT IN (SELECT node_key FROM vantage.ls_nodes FINAL WHERE `+scope+`)`)
	if orphans != 0 {
		t.Errorf("%d link endpoints have no matching node advertised in the same "+
			"capture; Grafana's Node Graph would silently drop those edges", orphans)
	}
}

// The link-state withdraw path had no fixture at all: raw_unreach is empty
// in every corpus payload, so LS withdrawal was covered only by the
// argument that MP_UNREACH's NLRI format is identical to MP_REACH's and
// therefore the same fixture-tested decoder handles it. That
// is an argument, not evidence -- and an earlier fix for the same class
// of never-exercised path found a real defect in exactly this way.
//
// Captured by shutting GigabitEthernet0/0/0/4 on xr-p2 and restoring it, with
// nx-p3 -- the only sender carrying a complete topology -- mirroring. It holds
// both halves of the flap: 2 link withdrawals and the re-advertisements that
// follow, so it exercises withdrawal and recovery rather than either alone.
//
// This is also what makes an "objects currently down" view answerable at all:
// an explicit withdrawal is the only signal that distinguishes a link that
// went away from one a router simply never re-advertised after a reconnect,
// which on IOS-XR is every link after every reconnect.
func TestCorpusNxosLinkStateWithdrawIsDecodedAndStored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos",
		"n9kv-10.6.2-p3-ls-withdraw.bmpcap")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// A router IP no other test replays, so these counts cannot be met by
	// another fixture's rows in the shared, never-truncated testDB.
	routerIP := mustAddr(t, "10.0.199.75")
	t0 := corpusCollectorClock
	s := collector.NewSession(routerIP, "corpus-lswd", 1,
		func() time.Time { return t0 }, collector.Overrides{})
	r := bytes.NewReader(raw)
	var seq uint64
	for {
		m, err := bmp.ReadMsg(r)
		if err != nil {
			break
		}
		for _, ev := range s.Handle(m) {
			seq++
			if err := ch.Insert(ctx, mustRowsFor(t, ev.Env, seq)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}

	const scope = `router_ip = toIPv6('10.0.199.75')`
	withdrawn := countRows(t, ctx, ch,
		"SELECT count() FROM vantage.ls_links FINAL WHERE "+scope+" AND is_withdraw = 1")
	advertised := countRows(t, ctx, ch,
		"SELECT count() FROM vantage.ls_links FINAL WHERE "+scope+" AND is_withdraw = 0")
	if withdrawn != 2 {
		t.Errorf("stored %d withdrawn links, want 2 -- MP_UNREACH link-state NLRI "+
			"reached ClickHouse as something other than a withdrawal", withdrawn)
	}
	if advertised == 0 {
		t.Errorf("stored no advertised links; the fixture carries the recovery " +
			"half of the flap and a withdraw-only result means the re-advertisements " +
			"were dropped")
	}
}
