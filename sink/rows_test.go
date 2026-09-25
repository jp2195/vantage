package sink

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jp2195/vantage/bgp"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
)

func testEnv(payload any) *vantagev1.Envelope {
	e := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "10.0.103.62", SysName: "xr-pe1"},
		Peer:        &vantagev1.PeerId{Ip: "10.255.0.1", Asn: 65000, BgpId: "10.255.0.1"},
		SessionId:   7,
		Seq:         42,
		TsRouter:    timestamppb.New(time.Unix(1700000000, 0)),
		TsCollector: timestamppb.New(time.Unix(1700000001, 0)),
	}
	switch p := payload.(type) {
	case *vantagev1.RouteEvent:
		e.Payload = &vantagev1.Envelope_Route{Route: p}
	case *vantagev1.PeerEvent:
		e.Payload = &vantagev1.Envelope_PeerEvent{PeerEvent: p}
	case *vantagev1.StatsEvent:
		e.Payload = &vantagev1.Envelope_Stats{Stats: p}
	case *vantagev1.LsEvent:
		e.Payload = &vantagev1.Envelope_Ls{Ls: p}
	}
	return e
}

// One UPDATE carrying three prefixes must produce three rows. If this ever
// returns one row, the exploded-UPDATE contract is broken and ClickHouse will
// only ever see a third of the routes.
func TestRowsForExplodesPrefixes(t *testing.T) {
	env := testEnv(&vantagev1.RouteEvent{
		Announced: []*vantagev1.Prefix{
			{Prefix: "10.0.0.0/24"}, {Prefix: "10.0.1.0/24"}, {Prefix: "10.0.2.0/24"},
		},
	})
	got := mustRowsFor(t, env, 99)
	if len(got.Unicast) != 3 {
		t.Fatalf("got %d unicast rows, want 3", len(got.Unicast))
	}
	for i, r := range got.Unicast {
		if r.StreamSeq != 99 {
			t.Errorf("row %d: stream_seq = %d, want 99", i, r.StreamSeq)
		}
		if r.IsWithdraw != 0 {
			t.Errorf("row %d: is_withdraw = %d, want 0", i, r.IsWithdraw)
		}
		if r.RouterIP != "10.0.103.62" || r.PeerASN != 65000 {
			t.Errorf("row %d: envelope columns not propagated: %+v", i, r.envelope)
		}
	}
}

// Announced and withdrawn share a table, distinguished only by is_withdraw.
func TestRowsForMarksWithdrawals(t *testing.T) {
	env := testEnv(&vantagev1.RouteEvent{
		Announced: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}},
		Withdrawn: []*vantagev1.Prefix{{Prefix: "10.9.0.0/24"}},
	})
	got := mustRowsFor(t, env, 1)
	if len(got.Unicast) != 2 {
		t.Fatalf("got %d rows, want 2", len(got.Unicast))
	}
	byPrefix := map[string]uint8{}
	for _, r := range got.Unicast {
		byPrefix[r.Prefix] = r.IsWithdraw
	}
	if byPrefix["10.0.0.0/24"] != 0 || byPrefix["10.9.0.0/24"] != 1 {
		t.Errorf("is_withdraw wrong: %+v", byPrefix)
	}
}

// An end-of-RIB marker carries no prefixes but must still be recorded: it is
// how a consumer knows a peer's initial table dump finished. It is recorded
// as an eor_events row and NOT as a route -- both halves asserted here,
// because asserting only the first would pass just as well for a translation
// that wrote the marker to both tables, which leaves the defect the split
// exists to remove fully intact.
func TestRowsForEndOfRib(t *testing.T) {
	got := mustRowsFor(t, testEnv(&vantagev1.RouteEvent{
		Family: &vantagev1.Family{Afi: 25, Safi: 70}, EndOfRib: true,
	}), 1)
	if len(got.Eor) != 1 {
		t.Fatalf("got %d eor rows, want 1", len(got.Eor))
	}
	if len(got.Unicast) != 0 {
		t.Errorf("got %d unicast rows, want 0 -- an end-of-RIB marker is a "+
			"collection artifact and must not be filed as a route: %+v",
			len(got.Unicast), got.Unicast)
	}
	// The marker's own family, not the route table's: a marker terminating
	// an EVPN dump had nowhere but route_unicast to live before the split,
	// which is what made dump progress unanswerable for a peer carrying no
	// unicast at all.
	if got.Eor[0].Family != "evpn" {
		t.Errorf("end-of-rib row wrong: %+v", got.Eor[0])
	}
}

// The VNI must survive translation unshifted. A VLAN-10 type-2 route
// carries VNI 10010; an MPLS-semantics decoder would give 625.
func TestRowsForEvpnKeepsVni(t *testing.T) {
	env := testEnv(&vantagev1.RouteEvent{
		EvpnAnnounced: []*vantagev1.EvpnRoute{{
			RouteType: 2, Rd: "10.255.1.2:32777",
			Mac: "00:50:79:66:68:01", Ip: "192.168.10.11",
			Labels: []uint32{10010},
		}},
	})
	got := mustRowsFor(t, env, 1)
	if len(got.Evpn) != 1 {
		t.Fatalf("got %d evpn rows, want 1", len(got.Evpn))
	}
	r := got.Evpn[0]
	if len(r.Labels) != 1 || r.Labels[0] != 10010 {
		t.Errorf("labels = %v, want [10010]", r.Labels)
	}
	if r.MAC != "00:50:79:66:68:01" || r.IP != "192.168.10.11" {
		t.Errorf("mac/ip not propagated: %+v", r)
	}
}

// Absent MED and local-pref must stay NULL rather than becoming zero, because
// zero is a legitimate value for both and conflating them loses information.
func TestRowsForOptionalAttrsStayNil(t *testing.T) {
	got := mustRowsFor(t, testEnv(&vantagev1.RouteEvent{
		Announced: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}},
		Attrs:     &vantagev1.PathAttributes{NextHop: "10.1.1.1"},
	}), 1)
	if got.Unicast[0].MED != nil || got.Unicast[0].LocalPref != nil {
		t.Errorf("absent optional attrs should be nil, got med=%v lp=%v",
			got.Unicast[0].MED, got.Unicast[0].LocalPref)
	}
}

func TestRowsForPeerAndStats(t *testing.T) {
	p := mustRowsFor(t, testEnv(&vantagev1.PeerEvent{
		Kind: vantagev1.PeerEvent_KIND_DOWN, DownReason: 3, LocalIp: "10.1.1.1",
	}), 5)
	if len(p.Peer) != 1 || p.Peer[0].Kind != "down" || p.Peer[0].DownReason != 3 {
		t.Errorf("peer row wrong: %+v", p.Peer)
	}
	s := mustRowsFor(t, testEnv(&vantagev1.StatsEvent{Counters: map[uint32]uint64{1: 7}}), 6)
	if len(s.Stats) != 1 || s.Stats[0].Counters[1] != 7 {
		t.Errorf("stats row wrong: %+v", s.Stats)
	}
}

// A Peer Down carries no local address -- handlePeerDown in
// collector/session.go never sets PeerEvent.LocalIp, so it decodes
// to the empty string -- and ClickHouse's IPv6 column type rejects an empty
// string outright (this is exactly what wedged the PEER consumer the first
// time a real Peer Down reached it). RowsFor must normalize that to the
// unspecified address ("::") rather than pass the empty string through, so
// this pins the pure half of that fix; TestClickHousePeerDownRoundTrips in
// clickhouse_test.go pins the half that actually proves ClickHouse accepts
// it, since this test alone would not have caught the original bug.
func TestRowsForPeerDownNormalizesEmptyLocalIP(t *testing.T) {
	p := mustRowsFor(t, testEnv(&vantagev1.PeerEvent{
		Kind: vantagev1.PeerEvent_KIND_DOWN, DownReason: 3,
	}), 5)
	if len(p.Peer) != 1 {
		t.Fatalf("want 1 peer row, got %d", len(p.Peer))
	}
	if p.Peer[0].LocalIP != "::" {
		t.Errorf("LocalIP = %q, want the unspecified address \"::\" for an "+
			"empty LocalIp, not the empty string ClickHouse's IPv6 type rejects", p.Peer[0].LocalIP)
	}
}

// router_ip, peer_ip (IPv6) and peer_bgp_id (IPv4) are populated from
// RouterId/PeerId in every table via commonColumns, not just peer_events.
// The collector's current code paths always set these to a real decoded
// address (see bmp/peerheader.go's ParsePeerHeader, which never
// produces an empty string), so this is defensive rather than pinning an
// observed production failure the way the LocalIP test above does -- but
// commonColumns applies the same normalization to all three uniformly, and
// this is what proves that.
func TestRowsForNormalizesEmptyRouterAndPeerAddresses(t *testing.T) {
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{},
		Peer:        &vantagev1.PeerId{},
		Payload:     &vantagev1.Envelope_Stats{Stats: &vantagev1.StatsEvent{}},
	}
	s := mustRowsFor(t, env, 1)
	if len(s.Stats) != 1 {
		t.Fatalf("want 1 stats row, got %d", len(s.Stats))
	}
	if s.Stats[0].RouterIP != "::" {
		t.Errorf("RouterIP = %q, want \"::\"", s.Stats[0].RouterIP)
	}
	if s.Stats[0].PeerIP != "::" {
		t.Errorf("PeerIP = %q, want \"::\"", s.Stats[0].PeerIP)
	}
	if s.Stats[0].PeerBGPID != "0.0.0.0" {
		t.Errorf("PeerBGPID = %q, want \"0.0.0.0\"", s.Stats[0].PeerBGPID)
	}
}

// BGP-LS carries no prefixes of its own -- the NLRI stays opaque in
// raw_reach/raw_unreach -- so this pins that the payload's family and both
// raw attribute buffers survive translation unchanged rather than being
// dropped by a switch arm that only handles RouteEvent.
func TestRowsForLsEventMapsFamilyAndRaw(t *testing.T) {
	env := testEnv(&vantagev1.LsEvent{
		Family:     &vantagev1.Family{Afi: 16388, Safi: 71},
		RawReach:   []byte{0x01, 0x02},
		RawUnreach: []byte{0x03},
	})
	got := mustRowsFor(t, env, 3)
	if len(got.Ls) != 1 {
		t.Fatalf("got %d ls rows, want 1", len(got.Ls))
	}
	r := got.Ls[0]
	if r.Family != "ls" {
		t.Errorf("family = %q, want %q", r.Family, "ls")
	}
	if string(r.RawReach) != "\x01\x02" || string(r.RawUnreach) != "\x03" {
		t.Errorf("raw reach/unreach not propagated: %+v", r)
	}
}

// VpnPrefix carries an RD and a label stack that Prefix does not, and VPN
// announcements/withdrawals share a table the same way unicast ones do.
// This pins that both map onto the vpn row, distinguished by is_withdraw,
// with RD and Labels intact on each side.
func TestRowsForVpnRouteMapsRDLabelsAndWithdraw(t *testing.T) {
	env := testEnv(&vantagev1.RouteEvent{
		VpnAnnounced: []*vantagev1.VpnPrefix{{
			Prefix: "10.0.0.0/24", Rd: "65000:100", Labels: []uint32{100},
		}},
		VpnWithdrawn: []*vantagev1.VpnPrefix{{
			Prefix: "10.9.0.0/24", Rd: "65000:200", Labels: []uint32{200},
		}},
	})
	got := mustRowsFor(t, env, 4)
	if len(got.Vpn) != 2 {
		t.Fatalf("got %d vpn rows, want 2", len(got.Vpn))
	}
	byPrefix := map[string]VpnRow{}
	for _, r := range got.Vpn {
		byPrefix[r.Prefix] = r
	}
	ann := byPrefix["10.0.0.0/24"]
	if ann.RD != "65000:100" || len(ann.Labels) != 1 || ann.Labels[0] != 100 || ann.IsWithdraw != 0 {
		t.Errorf("vpn announced row wrong: %+v", ann)
	}
	wd := byPrefix["10.9.0.0/24"]
	if wd.RD != "65000:200" || len(wd.Labels) != 1 || wd.Labels[0] != 200 || wd.IsWithdraw != 1 {
		t.Errorf("vpn withdrawn row wrong: %+v", wd)
	}
}

// --- sort-key coverage -------------------------------------------------
//
// ReplacingMergeTree keeps one row per sort tuple. So every row one envelope
// produces has to land on a tuple of its own, or a background merge deletes
// the rest -- no error, no metric, a plausible-looking count, and the archive
// of record quietly missing routes. The tests below are the enforcement of
// deploy/clickhouse/schema.sql's header comment, which claims exactly this
// and (before these) was claimed by nothing but prose.
//
// They read the ORDER BY clauses out of schema.sql rather than restating
// them, so widening or narrowing a sort key in the DDL is checked here
// against real envelopes automatically, and dropping a column from one is
// caught by the accessor lookup below rather than by a merge in production.

// sortKeyFromSchema returns table's ORDER BY column list as written in
// deploy/clickhouse/schema.sql.
func sortKeyFromSchema(t *testing.T, table string) []string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	re := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS vantage\.` +
		regexp.QuoteMeta(table) + `\b.*?ORDER BY \(([^)]*)\)`)
	m := re.FindStringSubmatch(string(ddl))
	if m == nil {
		t.Fatalf("no CREATE TABLE ... ORDER BY (...) for vantage.%s in schema.sql", table)
	}
	var cols []string
	for c := range strings.SplitSeq(m[1], ",") {
		cols = append(cols, strings.TrimSpace(c))
	}
	return cols
}

// envelopeColumn resolves the sort-key columns every table shares.
func envelopeColumn(e envelope, col string) (string, bool) {
	switch col {
	case "router_ip":
		return e.RouterIP, true
	case "peer_ip":
		return e.PeerIP, true
	case "rib":
		return e.RIB, true
	case "ts_router":
		return e.TsRouter.String(), true
	case "stream_seq":
		return fmt.Sprint(e.StreamSeq), true
	}
	return "", false
}

func unicastColumn(r UnicastRow, col string) (string, bool) {
	if v, ok := envelopeColumn(r.envelope, col); ok {
		return v, true
	}
	switch col {
	case "prefix":
		return r.Prefix, true
	case "path_id":
		return fmt.Sprint(r.PathID), true
	case "is_withdraw":
		return fmt.Sprint(r.IsWithdraw), true
	}
	return "", false
}

func vpnColumn(r VpnRow, col string) (string, bool) {
	if v, ok := envelopeColumn(r.envelope, col); ok {
		return v, true
	}
	switch col {
	case "prefix":
		return r.Prefix, true
	case "rd":
		return r.RD, true
	case "path_id":
		return fmt.Sprint(r.PathID), true
	case "is_withdraw":
		return fmt.Sprint(r.IsWithdraw), true
	}
	return "", false
}

func evpnColumn(r EvpnRow, col string) (string, bool) {
	if v, ok := envelopeColumn(r.envelope, col); ok {
		return v, true
	}
	switch col {
	case "route_type":
		return fmt.Sprint(r.RouteType), true
	case "rd":
		return r.RD, true
	case "prefix":
		return r.Prefix, true
	case "mac":
		return r.MAC, true
	case "ip":
		return r.IP, true
	case "ethernet_tag":
		return fmt.Sprint(r.EthernetTag), true
	case "esi":
		return r.ESI, true
	case "path_id":
		return fmt.Sprint(r.PathID), true
	case "is_withdraw":
		return fmt.Sprint(r.IsWithdraw), true
	}
	return "", false
}

func lsNodeColumn(r LsNodeRow, col string) (string, bool) {
	if v, ok := envelopeColumn(r.envelope, col); ok {
		return v, true
	}
	switch col {
	case "protocol":
		return fmt.Sprint(r.Protocol), true
	case "identifier":
		return fmt.Sprint(r.Identifier), true
	case "asn":
		return fmt.Sprint(r.ASN), true
	case "bgpls_id":
		return fmt.Sprint(r.BgplsID), true
	case "area":
		return fmt.Sprint(r.Area), true
	case "router_id":
		return r.RouterID, true
	case "is_withdraw":
		return fmt.Sprint(r.IsWithdraw), true
	}
	return "", false
}

func lsLinkColumn(r LsLinkRow, col string) (string, bool) {
	if v, ok := envelopeColumn(r.envelope, col); ok {
		return v, true
	}
	switch col {
	case "protocol":
		return fmt.Sprint(r.Protocol), true
	case "identifier":
		return fmt.Sprint(r.Identifier), true
	case "local_asn":
		return fmt.Sprint(r.LocalASN), true
	case "local_bgpls_id":
		return fmt.Sprint(r.LocalBgplsID), true
	case "local_area":
		return fmt.Sprint(r.LocalArea), true
	case "local_router_id":
		return r.LocalRouterID, true
	case "remote_asn":
		return fmt.Sprint(r.RemoteASN), true
	case "remote_bgpls_id":
		return fmt.Sprint(r.RemoteBgplsID), true
	case "remote_area":
		return fmt.Sprint(r.RemoteArea), true
	case "remote_router_id":
		return r.RemoteRouterID, true
	case "local_ifaddr":
		return r.LocalIfAddr, true
	case "remote_ifaddr":
		return r.RemoteIfAddr, true
	case "link_local_id":
		return fmt.Sprint(r.LinkLocalID), true
	case "link_remote_id":
		return fmt.Sprint(r.LinkRemoteID), true
	case "is_withdraw":
		return fmt.Sprint(r.IsWithdraw), true
	}
	return "", false
}

// assertDistinctSortTuples is the assertion itself: rows must occupy as many
// distinct sort tuples as there are rows. A column named in the DDL with no
// accessor above is a failure too, not a skip -- silently ignoring it would
// make the test pass by looking at less than the sort key really contains.
func assertDistinctSortTuples[T any](t *testing.T, table string, rows []T,
	col func(T, string) (string, bool)) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("%s: no rows to check", table)
	}
	cols := sortKeyFromSchema(t, table)
	seen := map[string][]int{}
	for i, r := range rows {
		var parts []string
		for _, c := range cols {
			v, ok := col(r, c)
			if !ok {
				t.Fatalf("%s: schema.sql's ORDER BY names column %q, which this "+
					"test cannot read off the row struct -- add it to the "+
					"accessor above (an unread sort column makes this whole "+
					"test weaker than the DDL it is checking)", table, c)
			}
			parts = append(parts, c+"="+v)
		}
		tuple := strings.Join(parts, "\x00")
		seen[tuple] = append(seen[tuple], i)
	}
	if len(seen) == len(rows) {
		return
	}
	for tuple, idx := range seen {
		if len(idx) > 1 {
			t.Errorf("%s: rows %v share one sort tuple, so ReplacingMergeTree "+
				"will keep exactly one of them and silently discard the rest: %s",
				table, idx, strings.ReplaceAll(tuple, "\x00", ", "))
		}
	}
	t.Errorf("%s: %d rows landed on %d distinct sort tuples (want %d); "+
		"schema.sql's ORDER BY is (%s)", table, len(rows), len(seen), len(rows),
		strings.Join(cols, ", "))
}

// TestRowsForDistinctSortTuplesPerRow builds envelopes shaped like the ones
// real routers send -- several routes in one UPDATE -- and asserts each row
// gets its own sort tuple.
//
// Every case here collapsed to a single row under the original sort keys
// (router_ip, peer_ip, rib, ts_router, stream_seq, prefix), which is how the
// defect got through review: `prefix` was in the key, which was the stated
// constraint, but `prefix` is not what tells these routes apart. The EVPN
// case is the one that was live -- 144 of 270 rows measured had an empty
// prefix -- and it is the shape a leaf takes when it advertises every MAC
// it has learned in one UPDATE.
func TestRowsForDistinctSortTuplesPerRow(t *testing.T) {
	t.Run("evpn: one UPDATE carrying two type-2 MAC/IPs and two type-3 IMETs", func(t *testing.T) {
		// No prefix on any of these four: type 2 is a MAC/IP advertisement
		// and type 3 an inclusive-multicast tag, neither of which has one.
		// The two type-2 routes are the same VLAN, so they share RD,
		// ethernet tag, ESI and label and differ only in MAC and IP.
		env := testEnv(&vantagev1.RouteEvent{
			EvpnAnnounced: []*vantagev1.EvpnRoute{
				{RouteType: 2, Rd: "10.255.1.2:32777", EthernetTag: 0,
					Mac: "00:50:79:66:68:01", Ip: "192.168.10.11", Labels: []uint32{10010}},
				{RouteType: 2, Rd: "10.255.1.2:32777", EthernetTag: 0,
					Mac: "00:50:79:66:68:02", Ip: "192.168.10.12", Labels: []uint32{10010}},
				{RouteType: 3, Rd: "10.255.1.2:32777", Ip: "10.255.1.2"},
				{RouteType: 3, Rd: "10.255.1.3:32777", Ip: "10.255.1.3"},
			},
		})
		got := mustRowsFor(t, env, 42)
		if len(got.Evpn) != 4 {
			t.Fatalf("got %d evpn rows, want 4", len(got.Evpn))
		}
		assertDistinctSortTuples(t, "route_evpn", got.Evpn, evpnColumn)
	})

	t.Run("evpn: add-path, one MAC advertised twice with different path ids", func(t *testing.T) {
		env := testEnv(&vantagev1.RouteEvent{
			EvpnAnnounced: []*vantagev1.EvpnRoute{
				{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:01",
					Ip: "192.168.10.11", PathId: 1},
				{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:01",
					Ip: "192.168.10.11", PathId: 2},
			},
		})
		assertDistinctSortTuples(t, "route_evpn", mustRowsFor(t, env, 42).Evpn, evpnColumn)
	})

	t.Run("evpn: the same MAC/IP announced and withdrawn in one UPDATE", func(t *testing.T) {
		env := testEnv(&vantagev1.RouteEvent{
			EvpnAnnounced: []*vantagev1.EvpnRoute{
				{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:01", Ip: "192.168.10.11"},
			},
			EvpnWithdrawn: []*vantagev1.EvpnRoute{
				{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:01", Ip: "192.168.10.11"},
			},
		})
		assertDistinctSortTuples(t, "route_evpn", mustRowsFor(t, env, 42).Evpn, evpnColumn)
	})

	t.Run("vpn: two VRFs exporting the same prefix under different RDs", func(t *testing.T) {
		env := testEnv(&vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 128},
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.9.9.0/24", Rd: "65000:1", Labels: []uint32{24001}},
				{Prefix: "10.9.9.0/24", Rd: "65000:2", Labels: []uint32{24002}},
			},
		})
		assertDistinctSortTuples(t, "route_vpn", mustRowsFor(t, env, 42).Vpn, vpnColumn)
	})

	t.Run("vpn: the same VPN route announced and withdrawn in one UPDATE", func(t *testing.T) {
		env := testEnv(&vantagev1.RouteEvent{
			Family:       &vantagev1.Family{Afi: 1, Safi: 128},
			VpnAnnounced: []*vantagev1.VpnPrefix{{Prefix: "10.9.9.0/24", Rd: "65000:1"}},
			VpnWithdrawn: []*vantagev1.VpnPrefix{{Prefix: "10.9.9.0/24", Rd: "65000:1"}},
		})
		assertDistinctSortTuples(t, "route_vpn", mustRowsFor(t, env, 42).Vpn, vpnColumn)
	})

	t.Run("unicast: add-path, one prefix with two path ids", func(t *testing.T) {
		env := testEnv(&vantagev1.RouteEvent{
			Announced: []*vantagev1.Prefix{
				{Prefix: "10.8.8.0/24", PathId: 1},
				{Prefix: "10.8.8.0/24", PathId: 2},
			},
		})
		assertDistinctSortTuples(t, "route_unicast", mustRowsFor(t, env, 42).Unicast, unicastColumn)
	})

	t.Run("ls_nodes: one UPDATE carrying two distinct node descriptors", func(t *testing.T) {
		env := testEnv(&vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Name: "xr-rr1",
					Local: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}}},
				{Protocol: 3, Identifier: 100, Name: "xr-p1",
					Local: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 2}}},
			},
		})
		got := mustRowsFor(t, env, 42)
		if len(got.LsNodes) != 2 {
			t.Fatalf("got %d ls_nodes rows, want 2", len(got.LsNodes))
		}
		assertDistinctSortTuples(t, "ls_nodes", got.LsNodes, lsNodeColumn)
	})

	t.Run("ls_links: one UPDATE carrying two distinct links", func(t *testing.T) {
		env := testEnv(&vantagev1.LsEvent{
			Links: []*vantagev1.LsLink{
				{Protocol: 3, Identifier: 100,
					Local:       &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 2}},
					Remote:      &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 5}},
					LocalIfaddr: []byte{10, 1, 0, 2}, RemoteIfaddr: []byte{10, 1, 0, 3}},
				{Protocol: 3, Identifier: 100,
					Local:       &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 3}},
					Remote:      &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 4}},
					LocalIfaddr: []byte{10, 1, 1, 2}, RemoteIfaddr: []byte{10, 1, 1, 3}},
			},
		})
		got := mustRowsFor(t, env, 42)
		if len(got.LsLinks) != 2 {
			t.Fatalf("got %d ls_links rows, want 2", len(got.LsLinks))
		}
		assertDistinctSortTuples(t, "ls_links", got.LsLinks, lsLinkColumn)
	})

	t.Run("unicast: several prefixes, an end-of-RIB marker and a withdrawal together", func(t *testing.T) {
		env := testEnv(&vantagev1.RouteEvent{
			EndOfRib:  true,
			Announced: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}, {Prefix: "10.0.1.0/24"}},
			Withdrawn: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}},
		})
		got := mustRowsFor(t, env, 42)
		// Three route rows, not four: the marker is filed into eor_events
		// now, so it is not competing for a route_unicast sort tuple at all.
		// A sender that sets end_of_rib on a non-empty UPDATE is malformed,
		// but its prefixes must still be stored rather than dropped -- which
		// is why the marker's presence does not suppress the route rows.
		if len(got.Unicast) != 3 {
			t.Fatalf("got %d unicast rows, want 3 (2 announced + 1 withdrawn; "+
				"the end-of-RIB marker belongs to eor_events)", len(got.Unicast))
		}
		if len(got.Eor) != 1 {
			t.Fatalf("got %d eor rows, want 1", len(got.Eor))
		}
		assertDistinctSortTuples(t, "route_unicast", got.Unicast, unicastColumn)
	})
}

// TestRowsForRedeliveryIsByteIdentical is the other half of the sort-key
// contract, and it is the one the fix above must not break. JetStream is
// at-least-once: the same envelope can arrive twice, and the writer's
// ack-after-durable loop deliberately lets it, because a redelivered
// envelope produces byte-identical rows on identical sort tuples and
// ReplacingMergeTree collapses them back to one. Widening a sort key is only
// safe while that stays true -- a key column computed from anything but the
// envelope's own content (a clock, a counter, an insertion order) would make
// the duplicate a permanent second row instead.
func TestRowsForRedeliveryIsByteIdentical(t *testing.T) {
	env := testEnv(&vantagev1.RouteEvent{
		Announced: []*vantagev1.Prefix{{Prefix: "10.8.8.0/24", PathId: 1}, {Prefix: "10.8.8.0/24", PathId: 2}},
		VpnAnnounced: []*vantagev1.VpnPrefix{
			{Prefix: "10.9.9.0/24", Rd: "65000:1"}, {Prefix: "10.9.9.0/24", Rd: "65000:2"}},
		EvpnAnnounced: []*vantagev1.EvpnRoute{
			{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:01", Ip: "192.168.10.11"},
			{RouteType: 3, Rd: "10.255.1.2:32777", Ip: "10.255.1.2"}},
	})
	// The same stream sequence, because a redelivery is the same message:
	// JetStream's stream sequence is assigned at publish, not at delivery.
	first := mustRowsFor(t, env, 42)
	second := mustRowsFor(t, env, 42)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("redelivering an envelope produced different rows, so the "+
			"duplicate would survive as a second row instead of collapsing:\n"+
			"first:  %+v\nsecond: %+v", first, second)
	}
}

// TestFamilyNameMatchesSubjectsFamilyToken pins the claim rows.go's
// familyNames comment makes and could not otherwise keep: that the family
// registry transcribed there is the same one subjects publishes
// subjects with, so a family string means the same thing in a subject and in
// a `family` column.
//
// Nothing else enforces it. Adding a family to subjects is what
// makes it publishable at all; forgetting rows.go at the same time is
// invisible -- the column silently reads the fallback "x25-70" while the
// subject token reads "evpn", and every dashboard filter and saved query on
// that family stops matching, with no error anywhere.
//
// rows.go deliberately does not import subjects, to stay pure
// (schema + bgp only). A test has no such constraint, which is why
// this can be checked here even though it cannot be checked there.
func TestFamilyNameMatchesSubjectsFamilyToken(t *testing.T) {
	check := func(afi uint16, safi uint8) {
		t.Helper()
		got := familyName(&vantagev1.Family{Afi: uint32(afi), Safi: uint32(safi)})
		want := subjects.FamilyToken(bgp.Family{AFI: afi, SAFI: safi})
		if got != want {
			t.Errorf("afi=%d safi=%d: rows.go's familyName gives %q, "+
				"subjects.FamilyToken gives %q -- the `family` column and the "+
				"NATS subject token have drifted apart; rows.go's familyNames "+
				"map must carry every entry subjects' does",
				afi, safi, got, want)
		}
	}
	// Every family this package knows a name for, so a rename on either side
	// is caught by name and not merely by a sweep happening to cover it.
	for f := range familyNames {
		check(f.AFI, f.SAFI)
	}
	// And a sweep of registered and unregistered pairs: the low AFIs (which
	// cover ipv4/ipv6 unicast, labeled unicast, VPN and EVPN), the BGP-LS
	// range around 16388, and the top of the AFI space, each across every
	// SAFI. This is what catches a family added to subjects and not
	// here -- the failure that has no other signal.
	for _, afis := range [][2]int{{0, 300}, {16380, 16400}, {65500, 65535}} {
		for afi := afis[0]; afi <= afis[1]; afi++ {
			for safi := 0; safi <= 255; safi++ {
				check(uint16(afi), uint8(safi))
			}
		}
	}
}

func TestRowsForLinkState(t *testing.T) {
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "10.0.103.64", SysName: "xr-p1"},
		Peer:        &vantagev1.PeerId{Ip: "10.255.0.1"},
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{{
				Protocol: 3, Identifier: 100, Name: "xr-rr1",
				SrgbBase: 16000, SrgbSize: 8000,
				Local:      &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}},
				RouterIdV4: []byte{10, 255, 0, 1},
			}},
			Links: []*vantagev1.LsLink{{
				Protocol: 3, Identifier: 100,
				AdjSids: []*vantagev1.LsAdjacencySid{{Sid: 24001}},
				Local:   &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 2}},
				Remote:  &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 5}},
			}},
		}},
	}
	rows := mustRowsFor(t, env, 7)
	if len(rows.LsNodes) != 1 {
		t.Fatalf("LsNodes = %d, want 1", len(rows.LsNodes))
	}
	if rows.LsNodes[0].Name != "xr-rr1" || rows.LsNodes[0].SrgbBase != 16000 {
		t.Errorf("node row = %+v", rows.LsNodes[0])
	}
	// router_id_v4 must reach the row, rendered
	// dotted-quad -- not just carried on the wire and dropped at the writer.
	// router_id (hex) must still be populated too: this is a supplement, not
	// a replacement.
	if rows.LsNodes[0].RouterIDv4 != "10.255.0.1" {
		t.Errorf("RouterIDv4 = %q, want 10.255.0.1", rows.LsNodes[0].RouterIDv4)
	}
	if rows.LsNodes[0].RouterID != "0aff0001" {
		t.Errorf("RouterID (hex) = %q, want 0aff0001 (unchanged by RouterIDv4)", rows.LsNodes[0].RouterID)
	}
	if len(rows.LsLinks) != 1 || len(rows.LsLinks[0].AdjSIDs) != 1 || rows.LsLinks[0].AdjSIDs[0] != 24001 {
		t.Fatalf("LsLinks = %+v", rows.LsLinks)
	}
	// An LS envelope carrying typed objects must NOT also produce a raw
	// ls_events row for the same content, or every node is counted twice.
	if len(rows.Ls) != 0 {
		t.Errorf("typed LS envelope also produced %d raw ls_events rows", len(rows.Ls))
	}
}

// TestRowsForLinkStateEndOfRib pins that an LS End-of-RIB has
// no nodes, no links and no raw bytes at all. Gating solely on
// RawReach/RawUnreach being non-empty would otherwise produce literally
// zero rows in any table for it, which would make "the LS RIB finished
// converging" unrepresentable in ClickHouse. Mirrors TestRowsForEndOfRib's
// classic-unicast counterpart.
func TestRowsForLinkStateEndOfRib(t *testing.T) {
	env := testEnv(&vantagev1.LsEvent{
		Family:   &vantagev1.Family{Afi: 16388, Safi: 71},
		EndOfRib: true,
	})
	got := mustRowsFor(t, env, 9)
	if len(got.Ls) != 1 {
		t.Fatalf("got %d ls rows, want 1", len(got.Ls))
	}
	r := got.Ls[0]
	if r.EndOfRIB != 1 {
		t.Errorf("EndOfRIB = %d, want 1", r.EndOfRIB)
	}
	if len(r.RawReach) != 0 || len(r.RawUnreach) != 0 {
		t.Errorf("End-of-RIB row carries raw bytes: RawReach=%x RawUnreach=%x", r.RawReach, r.RawUnreach)
	}
	if len(got.LsNodes) != 0 || len(got.LsLinks) != 0 {
		t.Errorf("End-of-RIB envelope produced nodes/links: %+v / %+v", got.LsNodes, got.LsLinks)
	}
}

// TestRowsForLinkStateMergesNLRIAndAttrUnknownTLVs pins, at the RowsFor
// boundary, that a Node/Link's own NLRI-level unknown TLVs
// (LsNode.unknown_tlvs / LsLink.unknown_tlvs) must reach ls_nodes/ls_links'
// unknown_tlvs column alongside the shared attribute-level ones
// (LsEvent.unknown_tlvs) -- not silently dropped, and not silently
// overwritten by one another when both are present.
func TestRowsForLinkStateMergesNLRIAndAttrUnknownTLVs(t *testing.T) {
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "10.0.103.64", SysName: "xr-p1"},
		Peer:        &vantagev1.PeerId{Ip: "10.255.0.1"},
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			UnknownTlvs: map[uint32][]byte{100: []byte("attr")},
			Nodes: []*vantagev1.LsNode{{
				Protocol: 3, Identifier: 100,
				Local:       &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}},
				UnknownTlvs: map[uint32][]byte{200: []byte("nlri")},
			}},
		}},
	}
	rows := mustRowsFor(t, env, 7)
	if len(rows.LsNodes) != 1 {
		t.Fatalf("LsNodes = %d, want 1", len(rows.LsNodes))
	}
	got := rows.LsNodes[0].UnknownTLVs
	if got[100] != "attr" {
		t.Errorf("UnknownTLVs[100] (attribute-level) = %q, want \"attr\"", got[100])
	}
	if got[200] != "nlri" {
		t.Errorf("UnknownTLVs[200] (NLRI-level) = %q, want \"nlri\"", got[200])
	}
}

// TestRowsForLinkStateMixedDecodeKeepsRawRemainder is the regression
// test for the data-loss defect: bgp/update.go's
// parseMPReach/parseMPUnreach set RawReach/RawUnreach non-empty not only
// when nothing decoded, but also on a MIXED decode -- one MP_REACH carrying
// Node/Link NLRI (typed here) alongside a Prefix NLRI (BGP-LS types 3/4,
// not decoded by this build), where "the raw ones are only recoverable
// from here" per that code's own comment. A condition gated on
// len(LsNodes)==0 && len(LsLinks)==0 would skip the raw row whenever any
// node/link decoded, silently dropping the undecoded remainder into no
// table at all. This asserts both the typed row AND the raw row survive
// together -- one envelope legitimately producing both is correct, not
// double counting, because the raw row carries only the undecoded
// remainder, never a second copy of the decoded node.
func TestRowsForLinkStateMixedDecodeKeepsRawRemainder(t *testing.T) {
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "10.0.103.64", SysName: "xr-p1"},
		Peer:        &vantagev1.PeerId{Ip: "10.255.0.1"},
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{{
				Protocol: 3, Identifier: 100, Name: "xr-rr1",
				Local: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}},
			}},
			// Standing in for the undecoded Prefix-NLRI remainder
			// parseMPReach keeps when a decode is only partial.
			RawReach: []byte{0xde, 0xad, 0xbe, 0xef},
		}},
	}
	rows := mustRowsFor(t, env, 7)
	if len(rows.LsNodes) != 1 {
		t.Fatalf("LsNodes = %d, want 1 (the typed decode must still land)", len(rows.LsNodes))
	}
	if len(rows.Ls) != 1 {
		t.Fatalf("Ls = %d, want 1 (the undecoded raw remainder must not be dropped)", len(rows.Ls))
	}
	if string(rows.Ls[0].RawReach) != "\xde\xad\xbe\xef" {
		t.Errorf("Ls[0].RawReach = %x, want deadbeef", rows.Ls[0].RawReach)
	}
}

// TestRowsAddCoversEveryRowSlice walks Rows by reflection rather than by
// name, so it fails for a row slice that does not exist yet. The bug it
// guards against was not a wrong append but a missing one: Rows grew
// LsNodes and LsLinks, the consumer's hand-written accumulation did not,
// and the result was silent data loss with every layer reporting success.
// A test that listed the fields itself would have drifted in exactly the
// same way, so this one asks the type.
func TestRowsAddCoversEveryRowSlice(t *testing.T) {
	typ := reflect.TypeFor[Rows]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		src := reflect.New(typ).Elem()
		src.Field(i).Set(reflect.MakeSlice(f.Type, 1, 1))

		var dst Rows
		dst.Add(src.Interface().(Rows))

		if got := reflect.ValueOf(dst).Field(i).Len(); got != 1 {
			t.Errorf("Rows.Add dropped field %s: destination holds %d rows, want 1", f.Name, got)
		}
		if got := dst.Len(); got != 1 {
			t.Errorf("Rows.Len does not count field %s: got %d, want 1", f.Name, got)
		}
	}
}

// TestRowsForLinkStatePrefix covers the prefix half of BGP-LS, using
// values a real router actually sent: a Loopback0 /32 with OSPF route type
// 1 (intra-area), prefix metric 1, and Prefix-SID index 5 -- which is the
// index configured on that loopback, so the row agrees with the router's
// own configuration rather than only with the decoder.
func TestRowsForLinkStatePrefix(t *testing.T) {
	e := &vantagev1.Envelope{
		CollectorId: "c1",
		Payload: &vantagev1.Envelope_Ls{
			Ls: &vantagev1.LsEvent{
				Family: &vantagev1.Family{Afi: 16388, Safi: 71},
				Prefixes: []*vantagev1.LsPrefix{{
					Protocol: 3, Identifier: 100,
					Local: &vantagev1.LsNodeDescriptor{
						Asn: 65000, RouterId: []byte{10, 255, 0, 5},
					},
					Prefix: []byte{10, 255, 0, 5}, PrefixLen: 32,
					OspfRouteType: 1,
					PrefixSid:     5, PrefixSidFlags: 0, HasPrefixSid: true,
					PrefixMetric: 1, PrefixAttrFlags: 0x40,
				}},
			},
		},
	}
	rows := mustRowsFor(t, e, 7)
	if len(rows.LsPrefixes) != 1 {
		t.Fatalf("got %d prefix rows, want 1", len(rows.LsPrefixes))
	}
	r := rows.LsPrefixes[0]
	if r.Prefix != "10.255.0.5" {
		t.Errorf("Prefix = %q, want %q", r.Prefix, "10.255.0.5")
	}
	if r.PrefixLen != 32 {
		t.Errorf("PrefixLen = %d, want 32", r.PrefixLen)
	}
	if r.RouterID != "0aff0005" {
		t.Errorf("RouterID = %q, want %q (hex, as ls_nodes stores it, so the join key matches)", r.RouterID, "0aff0005")
	}
	if r.PrefixSID != 5 || r.HasPrefixSID != 1 {
		t.Errorf("PrefixSID = %d, HasPrefixSID = %d; want 5, 1", r.PrefixSID, r.HasPrefixSID)
	}
	if r.OSPFRouteType != 1 {
		t.Errorf("OSPFRouteType = %d, want 1", r.OSPFRouteType)
	}
	if r.PrefixMetric != 1 {
		t.Errorf("PrefixMetric = %d, want 1", r.PrefixMetric)
	}
	if r.PrefixAttrFlags != 0x40 {
		t.Errorf("PrefixAttrFlags = %#x, want 0x40", r.PrefixAttrFlags)
	}
	if r.StreamSeq != 7 {
		t.Errorf("StreamSeq = %d, want 7", r.StreamSeq)
	}
}

// A Loc-RIB route (RFC 9069) is not one of the four adj-RIB directions: it is
// the router's own table after best-path selection, not something a peer
// sent. Without a dedicated rib value for it, a Loc-RIB route would land as
// rib = "in_pre" with peer_ip 0.0.0.0, making it indistinguishable from a
// route received pre-policy from a peer at 0.0.0.0 -- so every count over
// in_pre would silently include the router's own post-decision table
// wherever Loc-RIB monitoring was on. FRR enables it with one line and is
// the sender most likely to be pointed at this collector first, so the
// inflation is not hypothetical.
func TestRibNameCallsLocRibItsOwnRib(t *testing.T) {
	env := testEnv(&vantagev1.RouteEvent{Announced: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}}})
	env.Peer = &vantagev1.PeerId{
		Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
		Ip:   "0.0.0.0", Asn: 65001, BgpId: "10.99.0.1",
	}
	got := mustRowsFor(t, env, 1)
	if len(got.Unicast) != 1 {
		t.Fatalf("got %d unicast rows, want 1", len(got.Unicast))
	}
	if got.Unicast[0].RIB != "loc_rib" {
		t.Errorf("rib = %q, want %q", got.Unicast[0].RIB, "loc_rib")
	}
}

// RFC 9069 §4.2 redefines the per-peer flags byte for the Loc-RIB peer type:
// bit 0x80 is F (filtered) and everything else is reserved, so the L and O
// bits carry no meaning there. The collector still copies both onto PeerId
// verbatim, which is correct -- it records what the wire said. ribName must
// not act on them, or junk a sender left in reserved bits would scatter one
// Loc-RIB across four rib values. This mirrors subjects.ribDirection and
// collector.adjRIBOut, which both exclude the Loc-RIB peer type for the same
// reason; the three must agree.
func TestRibNameIgnoresReservedFlagBitsOnLocRibPeer(t *testing.T) {
	for _, tt := range []struct {
		name      string
		post, out bool
	}{
		{"L set", true, false},
		{"O set", false, true},
		{"both set", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := testEnv(&vantagev1.RouteEvent{Announced: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}}})
			env.Peer = &vantagev1.PeerId{
				Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
				Ip:   "0.0.0.0", PostPolicy: tt.post, AdjRibOut: tt.out,
			}
			got := mustRowsFor(t, env, 1)
			if got.Unicast[0].RIB != "loc_rib" {
				t.Errorf("rib = %q, want %q", got.Unicast[0].RIB, "loc_rib")
			}
		})
	}
}

// The four adj-RIB directions must keep their existing names. Loc-RIB is an
// addition to the enum, not a renumbering of it: peer types 0-2 are ordinary
// BGP sessions whose L and O flags mean exactly what RFC 7854 and RFC 8671
// say, and existing rows were written under these names.
func TestRibNameKeepsTheFourAdjRibDirections(t *testing.T) {
	for _, tt := range []struct {
		want      string
		post, out bool
	}{
		{"in_pre", false, false},
		{"in_post", true, false},
		{"out_pre", false, true},
		{"out_post", true, true},
	} {
		t.Run(tt.want, func(t *testing.T) {
			env := testEnv(&vantagev1.RouteEvent{Announced: []*vantagev1.Prefix{{Prefix: "10.0.0.0/24"}}})
			env.Peer = &vantagev1.PeerId{
				Type: vantagev1.PeerType_PEER_TYPE_GLOBAL,
				Ip:   "10.255.0.1", PostPolicy: tt.post, AdjRibOut: tt.out,
			}
			got := mustRowsFor(t, env, 1)
			if got.Unicast[0].RIB != tt.want {
				t.Errorf("rib = %q, want %q", got.Unicast[0].RIB, tt.want)
			}
		})
	}
}

// A view-lost event must reach ClickHouse as its own kind. Falling through
// peerRow's switch to "unspecified" would file the collector losing its
// view in the same bucket as a PeerEvent whose kind failed to decode, and
// query/routers.go's peers_up/peers_down counters -- which test kind
// explicitly -- would then report a router with a dead collector as having
// no peers at all rather than as having peers nobody can see.
func TestRowsForPeerViewLostGetsItsOwnKind(t *testing.T) {
	p := mustRowsFor(t, testEnv(&vantagev1.PeerEvent{
		Kind: vantagev1.PeerEvent_KIND_VIEW_LOST,
	}), 5)
	if len(p.Peer) != 1 {
		t.Fatalf("want 1 peer row, got %d", len(p.Peer))
	}
	if p.Peer[0].Kind != "view_lost" {
		t.Errorf("Kind = %q, want \"view_lost\"", p.Peer[0].Kind)
	}
	// The router said nothing on the way out, so there is no RFC 7854 sec
	// 4.9 reason code. Zero here is the absence of a reason, and the
	// "Peer Down reason" dashboard decodes reason 0 as no reason given.
	if p.Peer[0].DownReason != 0 {
		t.Errorf("DownReason = %d, want 0: a synthetic close has no wire reason",
			p.Peer[0].DownReason)
	}
}

// The three facts the wire has always carried and this sink dropped.
//
// capsProto has always built mp_families and addpath_families from BOTH of a
// session's OPEN messages, and bmp/tlv.go has always decoded the Initiation
// sysDescr TLV -- peerRow read GetFourByteAs() and let the rest fall on the
// floor. This pins the projection, not the decode: the decode was never the
// missing half.
func TestPeerRowCarriesTheSessionFactsTheWireAlreadyHad(t *testing.T) {
	// sysDescr is passed alongside the envelope rather than on it: `envelope`
	// is the 13 columns EVERY table shares, and sys_descr lands on
	// peer_events alone.
	e := envelope{RouterSysname: "xr-pe1"}
	const sysDescr = "Cisco IOS XR Software, Version 24.1.1"
	p := &vantagev1.PeerEvent{
		Kind: vantagev1.PeerEvent_KIND_UP,
		Caps: &vantagev1.Capabilities{
			FourByteAs: true,
			MpFamilies: []*vantagev1.Family{
				{Afi: 1, Safi: 1}, {Afi: 2, Safi: 1}, {Afi: 1, Safi: 128},
			},
			AddpathFamilies: []*vantagev1.Family{{Afi: 1, Safi: 1}},
			HoldTime:        180,
			HoldTimeSeen:    true,
		},
	}
	r := peerRow(e, sysDescr, p)

	// The SAME vocabulary the route tables' own `family` column uses --
	// familyName, not a second registry. A capability list rendering "ipv4"
	// where a route row says "ipv4u" would make the two impossible to join
	// while both looked right.
	if got := r.MPFamilies; len(got) != 3 || got[0] != "ipv4u" || got[1] != "ipv6u" || got[2] != "vpn4" {
		t.Errorf("mp_families = %v, want [ipv4u ipv6u vpn4]", got)
	}
	if got := r.AddPathFamilies; len(got) != 1 || got[0] != "ipv4u" {
		t.Errorf("addpath_families = %v, want [ipv4u]", got)
	}
	if r.HoldTime != 180 || r.HoldTimeSeen != 1 {
		t.Errorf("hold_time = %d seen = %d, want 180/1", r.HoldTime, r.HoldTimeSeen)
	}
	if r.SysDescr != sysDescr {
		t.Errorf("sys_descr = %q", r.SysDescr)
	}
}

// Zero is a hold time, and "no OPEN" is not zero.
//
// RFC 4271 sec 4.2: a Hold Time of 0 means the timer never expires --
// keepalives off for the session. A Peer Down carries no OPEN at all and so
// has no hold time to report. Both land on the number 0, and only
// hold_time_seen tells them apart; without it every Peer Down row would
// misread as a session that had disabled its keepalives.
func TestPeerRowKeepsAZeroHoldTimeApartFromNoHoldTime(t *testing.T) {
	zero := peerRow(envelope{}, "", &vantagev1.PeerEvent{
		Kind: vantagev1.PeerEvent_KIND_UP,
		Caps: &vantagev1.Capabilities{HoldTime: 0, HoldTimeSeen: true},
	})
	if zero.HoldTime != 0 || zero.HoldTimeSeen != 1 {
		t.Errorf("negotiated zero: hold_time = %d seen = %d, want 0/1",
			zero.HoldTime, zero.HoldTimeSeen)
	}

	// A Peer Down: no Caps at all, which is what the collector sends.
	down := peerRow(envelope{}, "", &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_DOWN})
	if down.HoldTime != 0 || down.HoldTimeSeen != 0 {
		t.Errorf("no OPEN observed: hold_time = %d seen = %d, want 0/0",
			down.HoldTime, down.HoldTimeSeen)
	}
}

// TestRowsForCollectorBeat: a beat becomes exactly one collector_beats row,
// with started_at from the payload and beat_at from the envelope's own
// ts_collector, and nothing in any other table. The two instants differ in
// the fixture so that swapping them fails.
func TestRowsForCollectorBeat(t *testing.T) {
	started := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	built := started.Add(42 * time.Second)
	env := &vantagev1.Envelope{
		CollectorId: "dev-c1",
		TsCollector: timestamppb.New(built),
		Payload: &vantagev1.Envelope_Beat{Beat: &vantagev1.CollectorBeat{
			StartedAt: timestamppb.New(started),
		}},
	}
	rows := mustRowsFor(t, env, 9)
	if rows.Len() != 1 || len(rows.Beats) != 1 {
		t.Fatalf("Len = %d, Beats = %d; want exactly one beat row and nothing else", rows.Len(), len(rows.Beats))
	}
	b := rows.Beats[0]
	if b.CollectorID != "dev-c1" {
		t.Errorf("CollectorID = %q, want dev-c1", b.CollectorID)
	}
	if !b.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", b.StartedAt, started)
	}
	if !b.BeatAt.Equal(built) {
		t.Errorf("BeatAt = %v, want %v (the envelope's ts_collector)", b.BeatAt, built)
	}
}

// mustRowsFor is RowsFor for the fixtures that are valid by construction,
// so an error is a broken test rather than the case under test.
func mustRowsFor(t testing.TB, env *vantagev1.Envelope, streamSeq uint64) Rows {
	t.Helper()
	rows, err := RowsFor(env, streamSeq)
	if err != nil {
		t.Fatalf("RowsFor: %v", err)
	}
	return rows
}

// TestRowsForRefusesAnAddressClickHouseCannotStore pins the writer
// wedge. Router.Ip = "x" decodes as a perfectly good protobuf string, and
// used to become a row whose insert ClickHouse's driver rejects -- and
// since an insert failure leaves the batch unacked for redelivery, that one
// envelope failed every batch it was in, forever. A peer_bgp_id that parses
// but is IPv6 is worse: the driver's IPv4 column calls netip.Addr.As4 on
// it, which panics.
//
// Every string that feeds an IPv4/IPv6 column is probed, each on an
// otherwise-valid envelope, and the valid spellings -- including the empty
// string, which rows.go normalizes to the unspecified address -- are the
// other side.
func TestRowsForRefusesAnAddressClickHouseCannotStore(t *testing.T) {
	peerUp := func() *vantagev1.Envelope {
		return testEnv(&vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP, LocalIp: "10.0.0.2"})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*vantagev1.Envelope)
	}{
		{"router ip", func(e *vantagev1.Envelope) { e.Router.Ip = "x" }},
		{"peer ip", func(e *vantagev1.Envelope) { e.Peer.Ip = "10.0.0.256" }},
		{"peer bgp id", func(e *vantagev1.Envelope) { e.Peer.BgpId = "not-an-id" }},
		{"peer bgp id that is IPv6", func(e *vantagev1.Envelope) { e.Peer.BgpId = "2001:db8::1" }},
		{"local ip", func(e *vantagev1.Envelope) { e.GetPeerEvent().LocalIp = "x" }},
	} {
		env := peerUp()
		tc.mutate(env)
		if rows, err := RowsFor(env, 1); err == nil {
			t.Errorf("%s: RowsFor accepted %v and produced %d rows", tc.name, env, rows.Len())
		} else if rows.Len() != 0 {
			t.Errorf("%s: RowsFor returned an error and %d rows; the rows would still be inserted", tc.name, rows.Len())
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*vantagev1.Envelope)
	}{
		{"as built", func(*vantagev1.Envelope) {}},
		{"IPv6 router and peer", func(e *vantagev1.Envelope) { e.Router.Ip = "2001:db8::1"; e.Peer.Ip = "fe80::1" }},
		{"empty addresses", func(e *vantagev1.Envelope) {
			e.Router.Ip, e.Peer.Ip, e.Peer.BgpId = "", "", ""
			e.GetPeerEvent().LocalIp = ""
		}},
		{"IPv4-mapped bgp id", func(e *vantagev1.Envelope) { e.Peer.BgpId = "::ffff:10.0.0.1" }},
	} {
		env := peerUp()
		tc.mutate(env)
		if rows, err := RowsFor(env, 1); err != nil || rows.Len() != 1 {
			t.Errorf("%s: RowsFor = %d rows, %v; want 1 row and no error", tc.name, rows.Len(), err)
		}
	}
}
