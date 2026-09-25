package bgp

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func evpnEntry(routeType byte, value []byte) []byte {
	return append([]byte{routeType, byte(len(value))}, value...)
}

// TestParseEvpnType2 builds a MAC/IP Advertisement by hand from RFC 7432 §7.2.
func TestParseEvpnType2(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01) // RD 65000:1
	val = append(val, make([]byte, 10)...)                            // ESI all-zero
	val = append(val, 0x00, 0x00, 0x00, 0x00)                         // Ethernet Tag 0
	val = append(val, 48)                                             // MAC length in bits
	val = append(val, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)             // MAC
	val = append(val, 32)                                             // IP length in bits
	val = append(val, 0x0A, 0x01, 0x01, 0x01)                         // 10.1.1.1
	val = append(val, 0x00, 0x27, 0x74)                               // VNI 10100 (raw 24-bit)

	got, untyped, err := parseEvpnNLRI(evpnEntry(2, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if untyped {
		t.Fatal("type 2 must be decoded, not carried raw")
	}
	if len(got) != 1 {
		t.Fatalf("got %d routes", len(got))
	}
	r := got[0]
	if r.RouteType != 2 || r.RD != "65000:1" || r.MAC != "aa:bb:cc:dd:ee:ff" || r.IP != "10.1.1.1" {
		t.Fatalf("route = %+v", r)
	}
	// The label field is stored raw: 0x002774 = 10100. Shifting it as an MPLS
	// label would give 631, silently wrong on a VXLAN fabric.
	if len(r.Labels) != 1 || r.Labels[0] != 10100 {
		t.Fatalf("labels = %v, want [10100] (raw 24-bit VNI, unshifted)", r.Labels)
	}
}

// TestParseEvpnType2MACOnly covers an advertisement with no IP (IPLen 0).
func TestParseEvpnType2MACOnly(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, make([]byte, 10)...)
	val = append(val, 0x00, 0x00, 0x00, 0x00)
	val = append(val, 48, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	val = append(val, 0)                // IP length 0 bits
	val = append(val, 0x00, 0x27, 0x74) // VNI

	got, _, err := parseEvpnNLRI(evpnEntry(2, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IP != "" {
		t.Fatalf("MAC-only advertisement should have empty IP, got %+v", got)
	}
}

// TestParseEvpnType2TwoLabels covers a route carrying both an EVI/VNI label
// and a second (e.g. L3VNI) label, exercising the optional Label2 field.
func TestParseEvpnType2TwoLabels(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, make([]byte, 10)...)
	val = append(val, 0x00, 0x00, 0x00, 0x00)
	val = append(val, 48, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	val = append(val, 32)
	val = append(val, 0x0A, 0x01, 0x01, 0x01)
	val = append(val, 0x00, 0x27, 0x74) // Label1 = 10100 (L2 VNI)
	val = append(val, 0x00, 0x30, 0x39) // Label2 = 12345 (L3 VNI)

	got, _, err := parseEvpnNLRI(evpnEntry(2, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d routes", len(got))
	}
	if labels := got[0].Labels; len(labels) != 2 || labels[0] != 10100 || labels[1] != 12345 {
		t.Fatalf("labels = %v, want [10100 12345]", labels)
	}
}

// TestParseEvpnType2IPv6 covers a MAC/IP advertisement carrying a 128-bit IP.
// Type 3 had IPv4 and IPv6 coverage but type 2 only IPv4 and IP-less, leaving
// its 16-byte branch -- the one where a wrong ipBytes would shift the label
// region and yield a plausible wrong VNI -- unexercised.
func TestParseEvpnType2IPv6(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, make([]byte, 10)...)
	val = append(val, 0x00, 0x00, 0x00, 0x00)
	val = append(val, 48, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	val = append(val, 128)                                                           // IP length in bits
	val = append(val, 0x20, 0x01, 0x0D, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x05) // 2001:db8::5
	val = append(val, 0x00, 0x27, 0x74)                                              // VNI 10100

	got, _, err := parseEvpnNLRI(evpnEntry(2, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IP != "2001:db8::5" {
		t.Fatalf("route = %+v, want IP 2001:db8::5", got[0])
	}
	if labels := got[0].Labels; len(labels) != 1 || labels[0] != 10100 {
		t.Fatalf("labels = %v, want [10100] -- a mis-sized ip would shift this", labels)
	}
}

// TestParseEvpnRDErrorNamesTheEntry pins the entry context on RD failures. The
// three route types all decode an RD from the same offset, so a bare
// "unknown rd type" error left no way to tell which entry produced it.
func TestParseEvpnRDErrorNamesTheEntry(t *testing.T) {
	for _, c := range []struct{ routeType, minLen int }{{2, 33}, {3, 17}, {5, 34}} {
		val := make([]byte, c.minLen)
		val[1] = 9 // RD type 9 is not defined by RFC 4364
		_, _, err := parseEvpnNLRI(evpnEntry(byte(c.routeType), val), false)
		if !errors.Is(err, ErrNLRIBadLength) {
			t.Fatalf("type %d: err = %v, want %v", c.routeType, err, ErrNLRIBadLength)
		}
		if want := fmt.Sprintf("evpn type %d", c.routeType); !strings.Contains(err.Error(), want) {
			t.Fatalf("type %d: err = %q, want it to mention %q", c.routeType, err, want)
		}
	}
}

// TestParseEvpnType3 covers Inclusive Multicast Ethernet Tag (RFC 7432 §7.3).
func TestParseEvpnType3(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, 0x00, 0x00, 0x00, 0x64) // Ethernet Tag 100
	val = append(val, 32)                     // IP length bits
	val = append(val, 0x0A, 0x00, 0x00, 0x0B) // 10.0.0.11

	got, _, err := parseEvpnNLRI(evpnEntry(3, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RouteType != 3 ||
		got[0].OriginatingIP != "10.0.0.11" || got[0].EthernetTag != 100 {
		t.Fatalf("route = %+v", got[0])
	}
}

// TestParseEvpnType3IPv6 covers the IPv6 originating-router-address encoding,
// the branch TestParseEvpnType3 does not reach.
func TestParseEvpnType3IPv6(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, 0x00, 0x00, 0x00, 0x64)                                        // Ethernet Tag 100
	val = append(val, 128)                                                           // IP length bits
	val = append(val, 0x20, 0x01, 0x0D, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01) // 2001:db8::1

	got, _, err := parseEvpnNLRI(evpnEntry(3, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OriginatingIP != "2001:db8::1" {
		t.Fatalf("route = %+v", got[0])
	}
}

// TestParseEvpnType5 covers IP Prefix routes (RFC 9136 §3.1).
func TestParseEvpnType5(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x02)
	val = append(val, make([]byte, 10)...)    // ESI
	val = append(val, 0x00, 0x00, 0x00, 0x00) // Ethernet Tag
	val = append(val, 24)                     // prefix length bits
	val = append(val, 0xC0, 0x00, 0x02, 0x00) // 192.0.2.0 (always 4 bytes for v4)
	val = append(val, 0x0A, 0x00, 0x00, 0x0B) // gateway 10.0.0.11
	val = append(val, 0x00, 0x27, 0x74)       // VNI

	got, _, err := parseEvpnNLRI(evpnEntry(5, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d routes", len(got))
	}
	r := got[0]
	if r.RouteType != 5 || r.Prefix != netip.MustParsePrefix("192.0.2.0/24") || r.GatewayIP != "10.0.0.11" {
		t.Fatalf("route = %+v", r)
	}
	if len(r.Labels) != 1 || r.Labels[0] != 10100 {
		t.Fatalf("labels = %v, want [10100] (raw 24-bit VNI, unshifted)", r.Labels)
	}
}

// TestParseEvpnType5IPv6 exercises the IPv6 branch of type 5, where prefix
// and gateway are each 16 bytes rather than 4 -- the size-from-remaining-
// length inference is the part of this decoder most likely to get the v4/v6
// split wrong.
func TestParseEvpnType5IPv6(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x02)
	val = append(val, make([]byte, 10)...)                                           // ESI
	val = append(val, 0x00, 0x00, 0x00, 0x00)                                        // Ethernet Tag
	val = append(val, 32)                                                            // prefix length bits
	val = append(val, 0x20, 0x01, 0x0D, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)    // 2001:db8::/32
	val = append(val, 0x20, 0x01, 0x0D, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x0B) // gateway 2001:db8::b
	val = append(val, 0x00, 0x27, 0x74)                                              // VNI

	got, _, err := parseEvpnNLRI(evpnEntry(5, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d routes", len(got))
	}
	r := got[0]
	if r.Prefix != netip.MustParsePrefix("2001:db8::/32") || r.GatewayIP != "2001:db8::b" {
		t.Fatalf("route = %+v", r)
	}
}

// TestParseEvpnUntypedCarriedRaw pins that a route type this decoder does
// not type still flows losslessly, tagged, and reports anyUntyped.
func TestParseEvpnUntypedCarriedRaw(t *testing.T) {
	val := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	got, untyped, err := parseEvpnNLRI(evpnEntry(4, val), false)
	if err != nil {
		t.Fatal(err)
	}
	if !untyped {
		t.Fatal("an undecoded route type must report anyUntyped")
	}
	if len(got) != 1 || got[0].RouteType != 4 || string(got[0].Raw) != string(val) {
		t.Fatalf("route = %+v", got[0])
	}
}

// TestParseEvpnAddPath covers the optional 4-byte ADD-PATH Path Identifier
// that precedes each entry when the family negotiated add-path.
func TestParseEvpnAddPath(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, 0x00, 0x00, 0x00, 0x64)
	val = append(val, 32)
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)

	b := append([]byte{0x00, 0x00, 0x00, 0x09}, evpnEntry(3, val)...)
	got, _, err := parseEvpnNLRI(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PathID != 9 || got[0].OriginatingIP != "10.0.0.11" {
		t.Fatalf("route = %+v", got)
	}
}

// TestParseEvpnMultiEntry walks several entries -- a type 2 followed by a
// type 3 -- in one buffer, mirroring real MP_REACH NLRI traffic: an
// MP_REACH NLRI field routinely carries more than one entry. This
// coverage was previously missing, so it is not skipped here.
func TestParseEvpnMultiEntry(t *testing.T) {
	val2 := []byte{}
	val2 = append(val2, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01) // RD 65000:1
	val2 = append(val2, make([]byte, 10)...)                            // ESI
	val2 = append(val2, 0x00, 0x00, 0x00, 0x00)                         // Ethernet Tag
	val2 = append(val2, 48, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)         // MAC
	val2 = append(val2, 0)                                              // IP length 0
	val2 = append(val2, 0x00, 0x27, 0x74)                               // Label

	val3 := []byte{}
	val3 = append(val3, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01) // RD
	val3 = append(val3, 0x00, 0x00, 0x00, 0x64)                         // Ethernet Tag 100
	val3 = append(val3, 32)
	val3 = append(val3, 0x0A, 0x00, 0x00, 0x0B) // 10.0.0.11

	var b []byte
	b = append(b, evpnEntry(2, val2)...)
	b = append(b, evpnEntry(3, val3)...)

	got, untyped, err := parseEvpnNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if untyped {
		t.Fatal("both entries are decodable types; anyUntyped must be false")
	}
	if len(got) != 2 {
		t.Fatalf("got %d routes, want 2: %+v", len(got), got)
	}
	if got[0].RouteType != 2 || got[0].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("entry 0 = %+v", got[0])
	}
	if got[1].RouteType != 3 || got[1].OriginatingIP != "10.0.0.11" {
		t.Fatalf("entry 1 = %+v", got[1])
	}
}

// TestParseEvpnMultiEntryAddPath is the add-path analog of
// TestParseEvpnMultiEntry: each of two entries carries its own path
// identifier, and the second entry's path ID must not be corrupted by the
// first entry's parse.
func TestParseEvpnMultiEntryAddPath(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	val = append(val, 0x00, 0x00, 0x00, 0x64)
	val = append(val, 32)
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)

	var b []byte
	b = append(b, 0x00, 0x00, 0x00, 0x01)
	b = append(b, evpnEntry(3, val)...)
	b = append(b, 0x00, 0x00, 0x00, 0x02)
	b = append(b, evpnEntry(3, val)...)

	got, _, err := parseEvpnNLRI(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PathID != 1 || got[1].PathID != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseEvpnRejectsTruncated(t *testing.T) {
	// A valid type-2 value used as the base for the two malformed-field
	// cases below (mac length, ip length): short reads are a distinct
	// failure mode from a structurally-wrong-but-fully-present field,
	// which is gated by ErrNLRIBadLength.
	validType2 := []byte{}
	validType2 = append(validType2, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	validType2 = append(validType2, make([]byte, 10)...)
	validType2 = append(validType2, 0x00, 0x00, 0x00, 0x00)
	validType2 = append(validType2, 48, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)
	validType2 = append(validType2, 0)
	validType2 = append(validType2, 0x00, 0x27, 0x74)

	badMacLen := append([]byte{}, validType2...)
	badMacLen[22] = 40 // MAC length must be 48 bits; 40 is not a legal MAC length

	badIPLen := append([]byte{}, validType2...)
	badIPLen[29] = 16 // IP length must be 0, 32 or 128 bits

	// RFC 7432 §7.2 ends a type 2 with exactly one or two labels. An entry
	// whose declared length runs past that used to parse clean: the "second
	// label if six bytes remain" test fired on padding and reported a VNI of 0
	// that no router ever sent. Both a 4-byte (one label + 1) and a 9-byte
	// (two labels + 3) region are rejected -- the latter is the shape that
	// fabricated a value rather than merely dropping one.
	overLongType2 := func(pad int) []byte {
		return append(append([]byte{}, validType2...), make([]byte, pad)...)
	}

	validType3 := []byte{}
	validType3 = append(validType3, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	validType3 = append(validType3, 0x00, 0x00, 0x00, 0x64) // Ethernet Tag 100
	validType3 = append(validType3, 32)                     // IP length bits
	validType3 = append(validType3, 0x0A, 0x00, 0x00, 0x0B)

	// A type-5 value whose trailing bytes (after RD/ESI/EthTag/PfxLen) total
	// neither 11 (v4 prefix+gateway+label) nor 35 (v6): here 12, one byte
	// too many for v4, one of the adversarial shapes this test guards.
	badType5Trailer := []byte{}
	badType5Trailer = append(badType5Trailer, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01)
	badType5Trailer = append(badType5Trailer, make([]byte, 10)...)
	badType5Trailer = append(badType5Trailer, 0x00, 0x00, 0x00, 0x00)
	badType5Trailer = append(badType5Trailer, 24)
	badType5Trailer = append(badType5Trailer, make([]byte, 12)...)

	for _, c := range []struct {
		name    string
		in      []byte
		addPath bool
		wantErr error
	}{
		{"header only", []byte{2}, false, ErrNLRITruncated},
		{"length exceeds buffer", []byte{2, 40, 1, 2, 3}, false, ErrNLRITruncated},
		// Malformed, not truncated: parseEvpnNLRI caps each value at the entry's
		// own declared length, so a value that cannot hold its route type is an
		// impossible declaration -- more bytes on the wire would not complete
		// it. Same split nlri.go documents and vpn.go applies.
		{"type 2 value shorter than the type allows", evpnEntry(2, []byte{1, 2, 3}), false, ErrNLRIBadLength},
		{"type 3 value shorter than the type allows", evpnEntry(3, []byte{1, 2, 3}), false, ErrNLRIBadLength},
		{"type 2 label region 4 bytes", evpnEntry(2, overLongType2(1)), false, ErrNLRIBadLength},
		{"type 2 label region 9 bytes", evpnEntry(2, overLongType2(6)), false, ErrNLRIBadLength},
		{"type 3 trailing bytes past the ip", evpnEntry(3, append(append([]byte{}, validType3...), 0, 0)), false, ErrNLRIBadLength},
		{"type 2 mac length not 48 bits", evpnEntry(2, badMacLen), false, ErrNLRIBadLength},
		{"type 2 ip length not 0/32/128 bits", evpnEntry(2, badIPLen), false, ErrNLRIBadLength},
		{"type 5 trailing length neither 11 nor 35", evpnEntry(5, badType5Trailer), false, ErrNLRIBadLength},
		{"add-path declared but buffer has only 3 bytes", []byte{1, 2, 3}, true, ErrNLRITruncated},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := parseEvpnNLRI(c.in, c.addPath)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("parseEvpnNLRI(%x) err = %v, want %v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestParseEvpnEmpty(t *testing.T) {
	got, untyped, err := parseEvpnNLRI(nil, false)
	if err != nil || untyped || len(got) != 0 {
		t.Fatalf("empty NLRI: got=%v untyped=%v err=%v, want no routes and no error", got, untyped, err)
	}
}

func FuzzParseEvpnNLRI(f *testing.F) {
	f.Add(evpnEntry(2, make([]byte, 33)), false)
	f.Add(evpnEntry(3, make([]byte, 17)), false)
	f.Add(append([]byte{0, 0, 0, 7}, evpnEntry(5, make([]byte, 34))...), true)
	f.Add(evpnEntry(5, make([]byte, 58)), false)
	f.Add([]byte{}, false)
	// Hostile seeds: a declared length far exceeding the buffer, and an
	// undefined route type mixed with a decodable one across two entries.
	f.Add([]byte{2, 0xFF, 1, 2, 3}, false)
	f.Add(append(evpnEntry(9, []byte{1, 2, 3}), evpnEntry(2, make([]byte, 33))...), false)
	f.Fuzz(func(t *testing.T, b []byte, addPath bool) {
		_, _, _ = parseEvpnNLRI(b, addPath) // must never panic
	})
}

// TestAppendEvpnNLRIRoundTrip covers building EVPN route types 2, 3 and 5 --
// the three this package decodes, and the three the corpus has observed from
// NX-OS.
//
// Same round-trip property as the MP_REACH tests: the assertion is that
// parseEvpnNLRI, which is proven against real n9kv captures, accepts what the
// builder emits. EVPN is the family where that matters most, because its NLRI
// is typed rather than a plain prefix -- every route type has its own layout,
// and a builder that got one field's width wrong would produce something that
// still parses but means something else.
func TestAppendEvpnNLRIRoundTrip(t *testing.T) {
	want := []EvpnRoute{
		{
			// Type 2, MAC/IP Advertisement, with an IP and two labels --
			// the shape NX-OS sends for a host on a VXLAN fabric.
			RouteType: 2, RD: "65100:32777",
			ESI:         "00000000000000000000",
			EthernetTag: 0,
			MAC:         "52:31:d4:03:1b:08",
			IP:          "10.10.10.1",
			Labels:      []uint32{10010, 50001},
		},
		{
			// Type 3, Inclusive Multicast Ethernet Tag: the only EVPN route
			// type originated without a host attached.
			RouteType: 3, RD: "10.255.1.2:32777",
			EthernetTag:   0,
			OriginatingIP: "10.255.2.2",
		},
		{
			// Type 5, IP Prefix.
			RouteType: 5, RD: "65100:50001",
			ESI:         "00000000000000000000",
			EthernetTag: 0,
			Prefix:      netip.MustParsePrefix("192.168.10.0/24"),
			GatewayIP:   "0.0.0.0",
			Labels:      []uint32{50001},
		},
	}

	raw := AppendEvpnNLRI(nil, want, false)
	got, untyped, err := parseEvpnNLRI(raw, false)
	if err != nil {
		t.Fatalf("parseEvpnNLRI on builder output: %v", err)
	}
	if untyped {
		t.Error("builder emitted an entry the decoder could not type")
	}
	if len(got) != len(want) {
		t.Fatalf("got %d routes, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.RouteType != w.RouteType || g.RD != w.RD {
			t.Errorf("[%d] type/rd = %d/%q, want %d/%q", i, g.RouteType, g.RD, w.RouteType, w.RD)
		}
		if g.MAC != w.MAC {
			t.Errorf("[%d] mac = %q, want %q", i, g.MAC, w.MAC)
		}
		if g.IP != w.IP {
			t.Errorf("[%d] ip = %q, want %q", i, g.IP, w.IP)
		}
		if g.OriginatingIP != w.OriginatingIP {
			t.Errorf("[%d] originating ip = %q, want %q", i, g.OriginatingIP, w.OriginatingIP)
		}
		if w.Prefix.IsValid() && g.Prefix != w.Prefix {
			t.Errorf("[%d] prefix = %s, want %s", i, g.Prefix, w.Prefix)
		}
		if len(g.Labels) != len(w.Labels) {
			t.Errorf("[%d] labels = %v, want %v", i, g.Labels, w.Labels)
			continue
		}
		for j := range w.Labels {
			if g.Labels[j] != w.Labels[j] {
				t.Errorf("[%d] label %d = %d, want %d -- EVPN labels are RAW 24-bit "+
					"values (the VNI on a VXLAN fabric), not shifted MPLS labels",
					i, j, g.Labels[j], w.Labels[j])
			}
		}
	}
}
