package bmptest

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
)

// TestBuildersFrameCorrectly covers basic message framing.
func TestBuildersFrameCorrectly(t *testing.T) {
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "10.0.0.9"}
	msgs := [][]byte{
		Init("rr1", "Cisco IOS XR Software, Version 7.9.2"),
		PeerDown(ph, 2, nil),
		Stats(ph, map[uint32]uint64{7: 42}),
		Termination(0),
	}
	for i, raw := range msgs {
		m, err := bmp.ReadMsg(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		// Reading raw[len(raw):] here would always give the empty slice, so
		// ReadMsg would always return io.EOF with a nil Payload and the
		// assertion could never fire. Slice at the message's own declared
		// length instead, so it genuinely proves the builder emitted exactly
		// one message with nothing trailing.
		declared := binary.BigEndian.Uint32(raw[1:5])
		if int(declared) != len(raw) {
			t.Fatalf("msg %d: header declares %d bytes, builder emitted %d", i, declared, len(raw))
		}
		if rest, _ := bmp.ReadMsg(bytes.NewReader(raw[declared:])); rest.Payload != nil {
			t.Fatalf("msg %d: trailing bytes after the declared length", i)
		}
		_ = m
	}
	if m, _ := bmp.ReadMsg(bytes.NewReader(Init("a", "b"))); m.Type != bmp.TypeInitiation {
		t.Fatal("init type")
	}
}

// --- hand-built golden byte slices, verified directly against RFC 7854 field
// layouts (not derived by calling this package's own builders, nor
// bmp.PeerHeader.Append) ---
//
// peerHdrBytes is the RFC 7854 §4.2 per-peer header for:
//
//	PeerHeader{Type: PeerTypeGlobal, Flags: 0, Distinguisher: 0,
//	  Addr: 10.0.0.9, AS: 65001, BGPID: "10.0.0.9", Timestamp: zero}
//
// laid out field-by-field, spelled out literally rather than produced via
// PeerHeader.Append, to close the risk that a builder and this repo's own
// parser could share the same misreading of RFC 7854 and agree with each
// other while both being wrong.
var peerHdrBytes = []byte{
	0x00, 0x00, // Peer Type = 0 (Global), Peer Flags = 0 (v4, pre-policy)
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Peer Distinguisher (8 bytes, unused for Global)
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // Peer Address (16 bytes): 12 zero bytes...
	0x00, 0x00, 0x00, 0x00, 0x0A, 0x00, 0x00, 0x09, // ...then the IPv4 address 10.0.0.9
	0x00, 0x00, 0xFD, 0xE9, // Peer AS = 65001
	0x0A, 0x00, 0x00, 0x09, // Peer BGP ID = 10.0.0.9
	0x00, 0x00, 0x00, 0x00, // Timestamp seconds = 0
	0x00, 0x00, 0x00, 0x00, // Timestamp microseconds = 0
}

func testPeerHeader() bmp.PeerHeader {
	return bmp.PeerHeader{
		Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9",
	}
}

func TestPeerHdrBytesFixtureLength(t *testing.T) {
	if len(peerHdrBytes) != bmp.PeerHeaderLen {
		t.Fatalf("test fixture: peerHdrBytes is %d bytes, want %d (bmp.PeerHeaderLen)", len(peerHdrBytes), bmp.PeerHeaderLen)
	}
}

// TestInitGolden pins the RFC 7854 §4.3 Initiation message layout: common
// header (version=3, length, type=4), then a sysDescr TLV (type 1) and a
// sysName TLV (type 2), each type(2)+length(2)+value.
func TestInitGolden(t *testing.T) {
	want := []byte{
		3, 0, 0, 0, 16, 4, // common header: version=3, length=16, type=4 (Initiation)
		0, 1, 0, 1, 'b', // TLV: type=1 (sysDescr), length=1, value="b"
		0, 2, 0, 1, 'a', // TLV: type=2 (sysName), length=1, value="a"
	}
	got := Init("a", "b")
	if !bytes.Equal(got, want) {
		t.Fatalf("Init(\"a\",\"b\") =\n got  %x\n want %x", got, want)
	}
}

// TestPeerDownGolden pins the RFC 7854 §4.9 Peer Down Notification layout:
// common header (type=2), per-peer header, 1-byte reason, then opaque data.
func TestPeerDownGolden(t *testing.T) {
	want := append([]byte{
		3, 0, 0, 0, 52, 2, // common header: version=3, length=52, type=2 (PeerDown)
	}, peerHdrBytes...)
	want = append(want, 1)                // Reason = 1 (local system closed, NOTIFICATION follows)
	want = append(want, 0xAA, 0xBB, 0xCC) // Data: opaque bytes (stand-in for a BGP NOTIFICATION PDU)

	got := PeerDown(testPeerHeader(), 1, []byte{0xAA, 0xBB, 0xCC})
	if !bytes.Equal(got, want) {
		t.Fatalf("PeerDown =\n got  %x\n want %x", got, want)
	}
}

// TestPeerDownGoldenNilData covers reason codes (4, 5) that carry no data
// field at all: the message ends immediately after the 1-byte reason.
func TestPeerDownGoldenNilData(t *testing.T) {
	want := append([]byte{
		3, 0, 0, 0, 49, 2, // length = 6 (header) + 42 (peer hdr) + 1 (reason) = 49
	}, peerHdrBytes...)
	want = append(want, 5) // Reason = 5 (peer de-configured, no data)

	got := PeerDown(testPeerHeader(), 5, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("PeerDown(reason=5, nil) =\n got  %x\n want %x", got, want)
	}
}

// TestRouteMonitoringGolden pins the RFC 7854 §4.6 Route Monitoring layout:
// common header (type=0), per-peer header, then one complete BGP UPDATE PDU
// with its own 19-byte header. The Update here is the classic End-of-RIB
// marker (RFC 4724 §2): withdrawn-routes-length=0, total-path-attribute-
// length=0, no NLRI -- the minimal legal UPDATE body, chosen so the BGP
// portion of the golden is unambiguous to hand-derive.
func TestRouteMonitoringGolden(t *testing.T) {
	want := append([]byte{
		3, 0, 0, 0, 71, 0, // common header: version=3, length=71, type=0 (RouteMonitoring)
	}, peerHdrBytes...)
	want = append(want, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF) // BGP marker (16 bytes, all ones)
	want = append(want, 0, 23) // BGP message length = 19 (header) + 4 (body) = 23
	want = append(want, 2)     // BGP message type = 2 (UPDATE)
	want = append(want, 0, 0)  // withdrawn-routes-length = 0
	want = append(want, 0, 0)  // total-path-attribute-length = 0

	got := RouteMonitoring(testPeerHeader(), bgp.BuildUpdate{})
	if !bytes.Equal(got, want) {
		t.Fatalf("RouteMonitoring(EoR) =\n got  %x\n want %x", got, want)
	}
}

// TestStatsGolden pins the RFC 7854 §4.8 Stats Report layout: common header
// (type=1), per-peer header, a 4-byte stat count, then one TLV per counter
// (2-byte type, 2-byte length, then that many bytes of value). The input map
// intentionally orders its Go literal with the higher stat type first to
// prove the builder sorts by type ascending rather than reproducing map
// iteration order.
func TestStatsGolden(t *testing.T) {
	// Types 1 and 3 are 32-bit Counters per RFC 7854 §4.8, so Stat Len is 4
	// and the value is 4 bytes -- what a real router emits. Type 8 is a
	// 64-bit Gauge, so it takes the 8-byte form in the same message; having
	// both widths here is the point of the golden.
	want := append([]byte{
		// length = 6 (common) + 42 (peer hdr) + 4 (count) + 8 + 8 (two 4-byte
		// Counters) + 12 (one 8-byte Gauge) = 80
		3, 0, 0, 0, 80, 1, // common header: version=3, length=80, type=1 (StatsReport)
	}, peerHdrBytes...)
	want = append(want, 0, 0, 0, 3)                         // Stats Count = 3
	want = append(want, 0, 1, 0, 4, 0, 0, 0, 5)             // type=1 (Counter), len=4, value=5
	want = append(want, 0, 3, 0, 4, 0, 0, 0, 0x64)          // type=3 (Counter), len=4, value=100
	want = append(want, 0, 8, 0, 8, 0, 0, 0, 1, 0, 0, 0, 0) // type=8 (Gauge),  len=8, value=2^32

	got := Stats(testPeerHeader(), map[uint32]uint64{3: 100, 1: 5, 8: 1 << 32})
	if !bytes.Equal(got, want) {
		t.Fatalf("Stats =\n got  %x\n want %x", got, want)
	}
}

// TestStatsCounterWidthMatchesRFCRegistry pins the type-to-width mapping
// itself, so a future edit cannot quietly go back to a fixed width and still
// pass the golden above (which only covers three types). A builder that
// emits 8 bytes for a 32-bit Counter produces a message no real router
// sends, which would make bmpgen's traffic and the fixture corpus agree with
// our own parser while both diverged from reality.
func TestStatsCounterWidthMatchesRFCRegistry(t *testing.T) {
	statLen := func(typ uint32) int {
		raw := Stats(testPeerHeader(), map[uint32]uint64{typ: 1})
		off := 6 + bmp.PeerHeaderLen + 4 // common header + per-peer header + stats count
		return int(binary.BigEndian.Uint16(raw[off+2 : off+4]))
	}
	for typ := uint32(0); typ <= 14; typ++ {
		want := 8
		if typ <= 6 || (typ >= 11 && typ <= 13) {
			want = 4 // RFC 7854 §4.8 32-bit Counter
		}
		if got := statLen(typ); got != want {
			t.Errorf("stat type %d: Stat Len = %d, want %d", typ, got, want)
		}
	}
}

// TestStatsPanicsOnOversizedCounterValue proves a value too large for a
// 32-bit Counter fails loudly rather than wrapping, per this package's
// builders-guard-their-preconditions convention.
func TestStatsPanicsOnOversizedCounterValue(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic, but Stats returned normally")
		}
	}()
	Stats(testPeerHeader(), map[uint32]uint64{1: 1 << 32}) // type 1 is a 32-bit Counter
}

// TestStatsDeterministic proves Stats does not depend on Go's randomized map
// iteration order (mirrors bgp.TestAppendOpenDeterministic). Run repeatedly
// because the randomization varies per iteration, not just per process.
func TestStatsDeterministic(t *testing.T) {
	counters := map[uint32]uint64{7: 42, 1: 1, 65535: 9, 100: 100, 3: 3}
	ph := testPeerHeader()
	want := Stats(ph, counters)
	for i := range 20 {
		got := Stats(ph, counters)
		if !bytes.Equal(got, want) {
			t.Fatalf("Stats run %d differs from run 0:\n want=%x\n got =%x", i, want, got)
		}
	}
}

// TestStatsPanicsOnOversizedType proves Stats guards its precondition that a
// counter type must fit the wire format's 2-byte stat-type field, rather than
// silently truncating it (per this package's builders-guard-their-
// preconditions convention; see bmp.AppendTLV, bgp.AppendOpen).
func TestStatsPanicsOnOversizedType(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("want panic, but Stats returned normally")
		}
	}()
	Stats(testPeerHeader(), map[uint32]uint64{65536: 1})
}

// TestTerminationGolden pins the RFC 7854 §4.5 Termination message layout:
// common header (type=5), then a single Information TLV of type 1 (Reason)
// carrying a 2-byte reason code.
func TestTerminationGolden(t *testing.T) {
	want := []byte{
		3, 0, 0, 0, 12, 5, // common header: version=3, length=12, type=5 (Termination)
		0, 1, 0, 2, 0, 4, // TLV: type=1 (Reason), length=2, value=4
	}
	got := Termination(4)
	if !bytes.Equal(got, want) {
		t.Fatalf("Termination(4) =\n got  %x\n want %x", got, want)
	}
}

// TestPeerUpGolden pins the RFC 7854 §4.10 Peer Up Notification layout: common
// header (type=3), per-peer header, 16-byte local address (IPv4 in the last
// 4 bytes), 2-byte local port, 2-byte remote port, the sent OPEN, then the
// received OPEN -- each OPEN a complete RFC 4271 §4.1 BGP message with its
// own marker/length/type header, self-delimiting the boundary between them.
// Both Caps are empty here so each OPEN carries opt-param-len=0 and no
// Capabilities optional parameter, keeping the golden's BGP portion small
// enough to hand-derive unambiguously (bgp package's own tests cover
// capability encoding in detail).
func TestPeerUpGolden(t *testing.T) {
	want := append([]byte{
		3, 0, 0, 0, 126, 3, // common header: version=3, length=126, type=3 (PeerUp)
	}, peerHdrBytes...)
	want = append(want,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xC0, 0x00, 0x02, 0x01, // local address: 192.0.2.1
	)
	want = append(want, 0, 0xB3)    // local port = 179
	want = append(want, 0xCC, 0x79) // remote port = 52345

	// Sent OPEN: ASN 65001, hold-time 180, BGP ID 1.1.1.1, no capabilities.
	want = append(want, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF) // marker
	want = append(want, 0, 29)      // length = 19 + 10
	want = append(want, 1)          // type = OPEN
	want = append(want, 4)          // version = 4
	want = append(want, 0xFD, 0xE9) // my-AS = 65001
	want = append(want, 0, 0xB4)    // hold-time = 180
	want = append(want, 1, 1, 1, 1) // BGP ID = 1.1.1.1
	want = append(want, 0)          // opt-param-len = 0

	// Received OPEN: ASN 65002, hold-time 180, BGP ID 2.2.2.2, no capabilities.
	want = append(want, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF) // marker
	want = append(want, 0, 29)      // length = 19 + 10
	want = append(want, 1)          // type = OPEN
	want = append(want, 4)          // version = 4
	want = append(want, 0xFD, 0xEA) // my-AS = 65002
	want = append(want, 0, 0xB4)    // hold-time = 180
	want = append(want, 2, 2, 2, 2) // BGP ID = 2.2.2.2
	want = append(want, 0)          // opt-param-len = 0

	got := PeerUp(testPeerHeader(), netip.MustParseAddr("192.0.2.1"), 179, 52345,
		bgp.Caps{}, bgp.Caps{}, 65001, 65002)
	if !bytes.Equal(got, want) {
		t.Fatalf("PeerUp =\n got  %x\n want %x", got, want)
	}
}

// TestPeerUpGoldenIPv6Local covers the IPv6 branch of the local-address
// encoding: a genuine IPv6 local address occupies the full 16 bytes, unlike
// the IPv4 case which zero-pads the leading 12.
func TestPeerUpGoldenIPv6Local(t *testing.T) {
	local := netip.MustParseAddr("2001:db8::1")
	got := PeerUp(testPeerHeader(), local, 179, 52345, bgp.Caps{}, bgp.Caps{}, 65001, 65002)

	// Local address occupies bytes [6:22] of the payload: common header (6) +
	// per-peer header (42) precede it, so offset = 6 + 42 = 48.
	off := 6 + bmp.PeerHeaderLen
	wantAddr := local.As16()
	if !bytes.Equal(got[off:off+16], wantAddr[:]) {
		t.Fatalf("local address bytes = %x, want %x", got[off:off+16], wantAddr[:])
	}
}

// TestMessagesParseWithThisPackagesOwnParser is a supplementary round-trip
// check (not a substitute for the hand-built goldens above): every builder's
// output must at minimum be readable by bmp.ReadMsg with no trailing bytes,
// and PeerDown/RouteMonitoring/Stats must carry a per-peer header this
// package's own ParsePeerHeader recovers unchanged.
func TestMessagesParseWithThisPackagesOwnParser(t *testing.T) {
	ph := testPeerHeader()
	cases := []struct {
		name    string
		raw     []byte
		typ     uint8
		peerHdr bool // payload begins with a per-peer header
	}{
		{"Init", Init("rr1", "descr"), bmp.TypeInitiation, false},
		{"PeerUp", PeerUp(ph, netip.MustParseAddr("192.0.2.1"), 179, 52345, bgp.Caps{}, bgp.Caps{}, 65001, 65002), bmp.TypePeerUp, true},
		{"PeerDown", PeerDown(ph, 2, nil), bmp.TypePeerDown, true},
		{"RouteMonitoring", RouteMonitoring(ph, bgp.BuildUpdate{}), bmp.TypeRouteMonitoring, true},
		{"Stats", Stats(ph, map[uint32]uint64{1: 1}), bmp.TypeStatsReport, true},
		{"Termination", Termination(1), bmp.TypeTermination, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := bmp.ReadMsg(bytes.NewReader(c.raw))
			if err != nil {
				t.Fatalf("ReadMsg: %v", err)
			}
			if m.Type != c.typ {
				t.Fatalf("type = %d, want %d", m.Type, c.typ)
			}
			if !c.peerHdr {
				return
			}
			got, _, err := bmp.ParsePeerHeader(m.Payload)
			if err != nil {
				t.Fatalf("ParsePeerHeader: %v", err)
			}
			// Compare field by field: Timestamp is a time.Time, so == on the
			// struct would compare monotonic/location internals rather than
			// the wire values.
			if got.Type != ph.Type || got.Flags != ph.Flags || got.Distinguisher != ph.Distinguisher ||
				got.Addr != ph.Addr || got.AS != ph.AS || got.BGPID != ph.BGPID ||
				!got.Timestamp.Equal(ph.Timestamp) {
				t.Fatalf("per-peer header round-trip:\n got  %+v\n want %+v", got, ph)
			}
		})
	}
}
