package bmp

import (
	"net/netip"
	"testing"
	"time"
)

func TestPeerHeaderRoundTripV4(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x40, Distinguisher: 0,
		Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "10.0.0.9",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}
	ph, rest, err := ParsePeerHeader(append(in.Append(nil), 0xEE))
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0] != 0xEE {
		t.Fatalf("rest = %x", rest)
	}
	if ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != in.BGPID ||
		!ph.Timestamp.Equal(in.Timestamp) || !ph.PostPolicy() || ph.IPv6() {
		t.Fatalf("got %+v", ph)
	}
}

func TestPeerHeaderZeroTimestamp(t *testing.T) {
	in := PeerHeader{Type: PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9")}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil || !ph.Timestamp.IsZero() {
		t.Fatalf("ph=%+v err=%v", ph, err)
	}
}

func TestPeerHeaderV6AndRD(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeRD, Flags: 0x80, Distinguisher: 0x0001000000000063,
		Addr: netip.MustParseAddr("2001:db8::9"), AS: 4200000001, BGPID: "192.0.2.1",
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil || ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != in.BGPID ||
		!ph.Timestamp.Equal(in.Timestamp) || !ph.IPv6() {
		t.Fatalf("ph=%+v err=%v", ph, err)
	}
}

func TestPeerHeaderShort(t *testing.T) {
	if _, _, err := ParsePeerHeader(make([]byte, PeerHeaderLen-1)); err == nil {
		t.Fatal("want error")
	}
}

// TestPeerHeaderRoundTripV4In6NoFlags covers an IPv4-mapped IPv6 address
// (netip deliberately distinguishes ::ffff:10.0.0.9 from 10.0.0.9) with no
// flags set on input. Append must still detect the Is4In6 address as IPv6,
// set flagV, and emit the full 16-byte form so it reparses identically.
func TestPeerHeaderRoundTripV4In6NoFlags(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x00, Distinguisher: 0x42,
		Addr: netip.MustParseAddr("::ffff:10.0.0.9"), AS: 65001, BGPID: "10.0.0.9",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != in.BGPID ||
		!ph.Timestamp.Equal(in.Timestamp) || !ph.IPv6() {
		t.Fatalf("got %+v, want Addr=%v", ph, in.Addr)
	}
}

// TestPeerHeaderRoundTripV4In6WithVFlag is the same Is4In6 address as above
// but with flagV already set on input (as captured from a real V=1 wire
// packet). The stale flagV combined with the additive-4-byte encoding
// used to produce a corrupted address (::a00:9) on reparse.
func TestPeerHeaderRoundTripV4In6WithVFlag(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x80, Distinguisher: 0x42,
		Addr: netip.MustParseAddr("::ffff:10.0.0.9"), AS: 65001, BGPID: "10.0.0.9",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != in.BGPID ||
		!ph.Timestamp.Equal(in.Timestamp) || !ph.IPv6() {
		t.Fatalf("got %+v, want Addr=%v", ph, in.Addr)
	}
}

// TestPeerHeaderRoundTripV4ClearsStaleVFlag uses a genuine IPv4 address with
// a stale flagV set on input, proving Append derives flagV from Addr alone
// and clears the caller-supplied bit rather than leaving it set with a
// mismatched 4-byte encoding.
func TestPeerHeaderRoundTripV4ClearsStaleVFlag(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x80, Distinguisher: 0x42,
		Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "10.0.0.9",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != in.BGPID ||
		!ph.Timestamp.Equal(in.Timestamp) || ph.IPv6() {
		t.Fatalf("got %+v, want Addr=%v and IPv6()=false", ph, in.Addr)
	}
}

// TestPeerHeaderAppendBGPIDIs4In6Unmapped covers the first case: an
// IPv4-mapped IPv6 BGP ID such as "::ffff:10.0.0.1" is Is4In6, not Is4, so
// Append must unmap it before the IPv4 check rather than silently emitting
// 0.0.0.0. The round-trip must recover the original (though ParsePeerHeader
// always converts to a dotted-quad string, so the representation changes).
func TestPeerHeaderAppendBGPIDIs4In6Unmapped(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x40, Distinguisher: 0,
		Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "::ffff:10.0.0.9",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != "10.0.0.9" ||
		!ph.Timestamp.Equal(in.Timestamp) || !ph.PostPolicy() || ph.IPv6() {
		t.Fatalf("got %+v", ph)
	}
}

// TestPeerHeaderAppendEmptyBGPID covers the distinction between unset and
// malformed: an empty BGPID is treated as unset and encodes as 0.0.0.0,
// which round-trips to "0.0.0.0" through ParsePeerHeader.
func TestPeerHeaderAppendEmptyBGPID(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x40, Distinguisher: 0,
		Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ph.Type != in.Type || ph.Distinguisher != in.Distinguisher ||
		ph.Addr != in.Addr || ph.AS != in.AS || ph.BGPID != "0.0.0.0" ||
		!ph.Timestamp.Equal(in.Timestamp) || !ph.PostPolicy() || ph.IPv6() {
		t.Fatalf("got %+v", ph)
	}
}

// TestPeerHeaderAppendPanicsOnUnparseableBGPID covers the second case: an
// unparseable BGP ID must panic (builder-guards-its-preconditions), not
// silently emit 0.0.0.0.
func TestPeerHeaderAppendPanicsOnUnparseableBGPID(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, but Append returned normally")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "not a valid IP address") {
			t.Fatalf("want panic with 'not a valid IP address', got: %v", r)
		}
	}()
	in := PeerHeader{
		Type: PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		BGPID: "not-an-ip",
	}
	in.Append(nil)
}

// TestPeerHeaderAppendPanicsOnIPv6BGPID covers the third constraint: a
// genuine IPv6 BGP ID (not IPv4-mapped, but a real IPv6 address) must panic.
func TestPeerHeaderAppendPanicsOnIPv6BGPID(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, but Append returned normally")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "not representable as an IPv4") {
			t.Fatalf("want panic with 'not representable as an IPv4', got: %v", r)
		}
	}()
	in := PeerHeader{
		Type: PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		BGPID: "2001:db8::1",
	}
	in.Append(nil)
}

// contains is a simple string search helper for panic message assertions.
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// FuzzParsePeerHeader mirrors FuzzReadMsg (see msg_test.go): ParsePeerHeader
// reads attacker-controlled wire bytes and must never panic, regardless of
// what error it returns.
func FuzzParsePeerHeader(f *testing.F) {
	v4 := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x40, Distinguisher: 0,
		Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "10.0.0.9",
		Timestamp: time.Unix(1753600000, 250000).UTC(),
	}.Append(nil)
	v6 := PeerHeader{
		Type: PeerTypeRD, Flags: 0x80, Distinguisher: 0x0001000000000063,
		Addr: netip.MustParseAddr("2001:db8::9"), AS: 4200000001, BGPID: "192.0.2.1",
	}.Append(nil)
	f.Add(v4)
	f.Add(v6)
	f.Add(v4[:PeerHeaderLen-1]) // truncated
	f.Add([]byte{})             // empty
	f.Fuzz(func(t *testing.T, data []byte) {
		ParsePeerHeader(data) // must not panic
	})
}

// TestPeerHeaderAdjRIBOutFlag pins RFC 8671 §4: bit 0x10 of the per-peer
// header flags byte is the O flag, and marks the message as belonging to
// the peer's adj-RIB-out rather than its adj-RIB-in. Before it was
// decoded, an adj-RIB-out feed was indistinguishable from adj-RIB-in, so
// routes a router *sent* were recorded as routes it *received*.
func TestPeerHeaderAdjRIBOutFlag(t *testing.T) {
	in := PeerHeader{
		Type: PeerTypeGlobal, Flags: 0x10,
		Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "10.0.0.9",
	}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !ph.AdjRIBOut() {
		t.Errorf("AdjRIBOut() = false, want true for flags 0x10")
	}
	if ph.PostPolicy() || ph.IPv6() || ph.TwoByteASPath() {
		t.Errorf("O flag alone set L, V or A: %+v", ph)
	}
}

// TestPeerHeaderAdjRIBOutPostPolicy covers RFC 8671's post-policy
// adj-RIB-out: O and L are independent bits and both must survive a
// round trip through Append.
func TestPeerHeaderAdjRIBOutPostPolicy(t *testing.T) {
	in := PeerHeader{Type: PeerTypeGlobal, Flags: 0x50, Addr: netip.MustParseAddr("10.0.0.9")}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !ph.AdjRIBOut() || !ph.PostPolicy() {
		t.Fatalf("flags 0x50: AdjRIBOut=%v PostPolicy=%v, want both true", ph.AdjRIBOut(), ph.PostPolicy())
	}
}

// TestPeerHeaderAdjRIBInIsDefault pins the other direction: a peer header
// with no O flag is adj-RIB-in, which is what every fixture in the corpus
// carries today.
func TestPeerHeaderAdjRIBInIsDefault(t *testing.T) {
	in := PeerHeader{Type: PeerTypeGlobal, Flags: 0x40, Addr: netip.MustParseAddr("10.0.0.9")}
	ph, _, err := ParsePeerHeader(in.Append(nil))
	if err != nil {
		t.Fatal(err)
	}
	if ph.AdjRIBOut() {
		t.Fatal("AdjRIBOut() = true for flags 0x40 (post-policy adj-RIB-in)")
	}
}
