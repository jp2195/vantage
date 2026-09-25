package subjects

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strings"
	"testing"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
)

// --- Token vectors ---
//
// The two EncodeIP vectors are the one place these move away from
// dotted/colon text with both separators mapped to '-'
// ("10.1.2.3" -> "10-1-2-3", "2001:db8::9" -> "2001-db8--9"). That scheme
// is not injective -- it collapsed the distinct addresses ::ffff:1.2.3.4
// and ::ffff:1:2:3:4 onto one token -- and since collector msg-ids embed
// these tokens, a collision makes JetStream drop one peer's message as a
// duplicate of another's. Encoding the address bytes as hex is injective
// by construction.

func TestTokens(t *testing.T) {
	if got := EncodeIP(netip.MustParseAddr("10.1.2.3")); got != "0a010203" {
		t.Fatal(got)
	}
	if got := EncodeIP(netip.MustParseAddr("2001:db8::9")); got != "20010db8000000000000000000000009" {
		t.Fatal(got)
	}
	if FamilyToken(bgp.FamilyIPv4U) != "ipv4u" || FamilyToken(bgp.Family{AFI: 25, SAFI: 70}) != "evpn" {
		t.Fatal("family registry")
	}
	if got := FamilyToken(bgp.Family{AFI: 12, SAFI: 34}); got != "x12-34" {
		t.Fatal(got)
	}
	if !IsLS(bgp.Family{AFI: 16388, SAFI: 71}) || IsLS(bgp.FamilyIPv4U) {
		t.Fatal("IsLS")
	}
}

func TestPeerToken(t *testing.T) {
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9")}
	if got := PeerToken(ph); got != "0a000009" {
		t.Fatal(got)
	}
	ph.Type = bmp.PeerTypeRD
	ph.Distinguisher = 42
	got := PeerToken(ph)
	if len(got) != len("0a000009-r")+8 || got[:10] != "0a000009-r" {
		t.Fatal(got)
	}
	if PeerToken(bmp.PeerHeader{Type: bmp.PeerTypeLocRIB}) != "locrib" {
		t.Fatal("locrib")
	}
}

func TestSubjects(t *testing.T) {
	if got := Route("ipv4u", "10-0-0-1", "10-0-0-9"); got != "vantage.v1.route.ipv4u.10-0-0-1.10-0-0-9" {
		t.Fatal(got)
	}
	if got := Raw("10-0-0-1"); got != "vantage.v1.raw.10-0-0-1" {
		t.Fatal(got)
	}
}

// --- Registry completeness ---

// TestFamilyTokenAllRegistryEntries checks every entry in familyNames,
// not just the two TestTokens happens to touch, and confirms the
// x{afi}-{safi} fallback for a handful of AFI/SAFI pairs outside it.
func TestFamilyTokenAllRegistryEntries(t *testing.T) {
	cases := []struct {
		f    bgp.Family
		want string
	}{
		{bgp.Family{AFI: 1, SAFI: 1}, "ipv4u"},
		{bgp.Family{AFI: 2, SAFI: 1}, "ipv6u"},
		{bgp.Family{AFI: 1, SAFI: 4}, "lu4"},
		{bgp.Family{AFI: 1, SAFI: 128}, "vpn4"},
		{bgp.Family{AFI: 2, SAFI: 128}, "vpn6"},
		{bgp.Family{AFI: 25, SAFI: 70}, "evpn"},
		{bgp.Family{AFI: 16388, SAFI: 71}, "ls"},
		{bgp.Family{AFI: 16388, SAFI: 72}, "x16388-72"}, // BGP-LS VPN SAFI not in the registry above
		{bgp.Family{AFI: 0, SAFI: 0}, "x0-0"},
		{bgp.Family{AFI: 65535, SAFI: 255}, "x65535-255"},
	}
	for _, c := range cases {
		if got := FamilyToken(c.f); got != c.want {
			t.Errorf("FamilyToken(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

// --- EncodeIP / DecodeIP edge cases ---

// TestEncodeIPInvalidAddr pins that the invalid zero Addr gets its own
// token rather than a run of zeros, which would collide with "::" (whose
// 16 address bytes are all zero).
func TestEncodeIPInvalidAddr(t *testing.T) {
	var zero netip.Addr
	if zero.IsValid() {
		t.Fatal("test assumption violated: zero netip.Addr is valid")
	}
	got := EncodeIP(zero)
	if got != "invalid" {
		t.Fatalf("EncodeIP(invalid addr) = %q, want %q", got, "invalid")
	}
	if !validToken(got) {
		t.Fatalf("EncodeIP(invalid addr) = %q is not a valid subject token", got)
	}
	for _, other := range []netip.Addr{
		netip.MustParseAddr("::"),
		netip.MustParseAddr("0.0.0.0"),
	} {
		if EncodeIP(other) == got {
			t.Fatalf("invalid addr collided with %v on %q", other, got)
		}
	}
	if _, err := DecodeIP(got); err == nil {
		t.Fatal("DecodeIP(\"invalid\") must return an error")
	}
}

// TestEncodeIPZoneIsSafeAndDistinguishing covers IPv6 zones. net/netip does
// not validate or restrict zone content at all -- confirmed empirically,
// ParseAddr accepts a zone containing '.', '*', '>', NUL, DEL and arbitrary
// non-ASCII bytes. Hex encoding never renders the zone, so no byte of it
// can reach a subject; the hashed suffix exists only so two addresses that
// differ solely by zone stay distinct.
func TestEncodeIPZoneIsSafeAndDistinguishing(t *testing.T) {
	base := netip.MustParseAddr("fe80::1")
	hostile := base.WithZone("eth0*evil>.\t\n\x00\xff")
	if !hostile.IsValid() || hostile.Zone() == "" {
		t.Fatal("test assumption violated: WithZone produced no zone")
	}
	got := EncodeIP(hostile)
	if !validToken(got) {
		t.Fatalf("EncodeIP(hostile zone) = %q is not a valid subject token", got)
	}
	for _, bad := range []string{".", "*", ">", "\t", "\n", "\x00", "\xff", " "} {
		if strings.Contains(got, bad) {
			t.Fatalf("EncodeIP(hostile zone) = %q leaked %q", got, bad)
		}
	}
	a, b := EncodeIP(base.WithZone("eth0")), EncodeIP(base.WithZone("eth1"))
	if a == b {
		t.Fatalf("addresses differing only by zone collided on %q", a)
	}
	if a == EncodeIP(base) || b == EncodeIP(base) {
		t.Fatal("a zoned address must not collide with its unzoned form")
	}
}

// TestEncodeIPMappedEqualsUnmapped pins the one deliberate merge: an
// IPv4-mapped address and its IPv4 form denote the same address, so they
// share a token. bmp.ParsePeerHeader keeps them distinct in PeerHeader.Addr
// and the envelope carries that exact value; only the routing key is
// normalized.
func TestEncodeIPMappedEqualsUnmapped(t *testing.T) {
	v4 := netip.MustParseAddr("10.0.0.1")
	mapped := netip.MustParseAddr("::ffff:10.0.0.1")
	if mapped == v4 {
		t.Fatal("test assumption violated: netip treats these as equal")
	}
	if EncodeIP(mapped) != EncodeIP(v4) {
		t.Fatalf("mapped %q != unmapped %q", EncodeIP(mapped), EncodeIP(v4))
	}
	if got := EncodeIP(v4); got != "0a000001" {
		t.Fatalf("EncodeIP(10.0.0.1) = %q, want %q", got, "0a000001")
	}
}

// TestDecodeIPRoundTrip pins the round-trip hex was chosen for: operator
// tooling can turn a subject token back into a real address. The earlier
// text-and-escape scheme was lossy and could not.
func TestDecodeIPRoundTrip(t *testing.T) {
	for _, s := range []string{
		"0.0.0.0", "10.0.0.1", "192.0.2.1", "255.255.255.255",
		"::", "::1", "2001:db8::9", "fe80::1",
		"2001:db8:85a3::8a2e:370:7334",
	} {
		a := netip.MustParseAddr(s)
		got, err := DecodeIP(EncodeIP(a))
		if err != nil {
			t.Fatalf("DecodeIP(EncodeIP(%s)): %v", s, err)
		}
		if got != a {
			t.Fatalf("round-trip %s: got %v", s, got)
		}
	}
	// A PeerToken suffix is ignored, so a peer token decodes directly.
	tok := PeerToken(bmp.PeerHeader{Type: bmp.PeerTypeRD, Addr: netip.MustParseAddr("10.0.0.9"), Distinguisher: 42})
	got, err := DecodeIP(tok)
	if err != nil {
		t.Fatalf("DecodeIP(%q): %v", tok, err)
	}
	if got != netip.MustParseAddr("10.0.0.9") {
		t.Fatalf("DecodeIP(%q) = %v", tok, got)
	}
	for _, bad := range []string{"", "locrib", "invalid", "zzzzzzzz", "0a00000", "0a0000011"} {
		if _, err := DecodeIP(bad); err == nil {
			t.Fatalf("DecodeIP(%q) must return an error", bad)
		}
	}
}

// TestEncodeIPNoCollisionAcrossRealisticAddresses is a concrete, table-
// driven check (not just an argument in prose) that a representative set
// of real router/peer address forms -- IPv4, IPv6, IPv4-mapped IPv6,
// link-local, all-zeros, all-ones, and the invalid zero value -- never
// collide with each other.
func TestEncodeIPNoCollisionAcrossRealisticAddresses(t *testing.T) {
	addrs := []netip.Addr{
		netip.Addr{},
		netip.MustParseAddr("0.0.0.0"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.0.0.9"),
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("255.255.255.255"),
		netip.MustParseAddr("::"),
		netip.MustParseAddr("::1"),
		netip.MustParseAddr("2001:db8::9"),
		netip.MustParseAddr("2001:db8:85a3::8a2e:370:7334"),
		// NOT ::ffff:10.0.0.1 / ::ffff:10.0.0.9 -- EncodeIP unmaps Is4In6, so
		// those deliberately share a token with 10.0.0.1 / 10.0.0.9 above.
		// See TestEncodeIPInjectiveOverMappedAndHexForms.
		netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("fe80::1").WithZone("eth0"),
		netip.MustParseAddr("fe80::1").WithZone("eth1"), // different link, same address
	}
	seen := map[string]netip.Addr{}
	for _, a := range addrs {
		tok := EncodeIP(a)
		if prev, ok := seen[tok]; ok {
			t.Fatalf("collision: EncodeIP(%v) and EncodeIP(%v) both = %q", prev, a, tok)
		}
		seen[tok] = a
		if !validToken(tok) {
			t.Fatalf("EncodeIP(%v) = %q is not a valid subject token", a, tok)
		}
	}
}

// --- PeerToken ---

// TestPeerTokenExactHash pins the FNV-1a computation to a value computed
// independently in the test, so a future refactor of the byte-order logic
// can't silently drift while still satisfying a looser
// length/prefix-only check.
func TestPeerTokenExactHash(t *testing.T) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 42)
	h := fnv.New32a()
	h.Write(b[:])
	want := "0a000009-r" + hex32(h.Sum32())

	ph := bmp.PeerHeader{Type: bmp.PeerTypeRD, Distinguisher: 42, Addr: netip.MustParseAddr("10.0.0.9")}
	if got := PeerToken(ph); got != want {
		t.Fatalf("PeerToken = %q, want %q", got, want)
	}
}

func hex32(v uint32) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = hexdigits[v&0xf]
		v >>= 4
	}
	return string(b)
}

// TestPeerTokenLocRibNeverCollidesWithIPBranch proves "locrib" (a fixed
// literal, all lowercase letters) can never be produced by the IP-encoding
// branch: valid, unscoped netip.Addr text is only digits, '.', ':', and
// lowercase hex digits 0-9a-f, none of which include 'l', 'o', 'c', or 'r'
// together in that combination, and 'l'/'o'/'c'/'r' are the very first
// letters of "locrib" -- so no real router/peer address, encoded, can ever
// equal the LocRIB sentinel token.
func TestPeerTokenLocRibNeverCollidesWithIPBranch(t *testing.T) {
	addrs := []netip.Addr{
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("2001:db8::9"),
		netip.MustParseAddr("::ffff:10.0.0.1"),
		netip.MustParseAddr("fe80::1"),
		{},
	}
	for _, a := range addrs {
		for _, d := range []uint64{0, 1, 42, 0xFFFFFFFFFFFFFFFF} {
			ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: a, Distinguisher: d}
			if got := PeerToken(ph); got == "locrib" {
				t.Fatalf("PeerToken(%+v) collided with the LocRIB sentinel", ph)
			}
		}
	}
}

// TestPeerTokenLocRibDistinguishesVRFs pins RFC 9069 §4.2: the Peer
// Distinguisher on the Loc-RIB peer type says *which* L3VPN/VRF a Loc-RIB
// dump belongs to. An earlier version returned a bare "locrib" for every
// Loc-RIB peer, so a collector monitoring per-VRF Loc-RIB instances merged
// every VRF's routes onto one subject.
//
// This does not break the existing verbatim PeerToken vector, which asserts
// PeerToken(PeerHeader{Type: PeerTypeLocRIB}) == "locrib" with a *zero*
// Distinguisher -- still true. No exported name or signature changes, so
// nothing downstream is affected.
func TestPeerTokenLocRibDistinguishesVRFs(t *testing.T) {
	base := PeerToken(bmp.PeerHeader{Type: bmp.PeerTypeLocRIB, Distinguisher: 0})
	if base != "locrib" {
		t.Fatalf("zero Distinguisher: got %q, want %q", base, "locrib")
	}
	vrfA := PeerToken(bmp.PeerHeader{Type: bmp.PeerTypeLocRIB, Distinguisher: 99})
	vrfB := PeerToken(bmp.PeerHeader{Type: bmp.PeerTypeLocRIB, Distinguisher: 100})
	if vrfA == base || vrfB == base {
		t.Fatalf("a VRF-scoped Loc-RIB must not collide with the unscoped one: %q, %q, %q", base, vrfA, vrfB)
	}
	if vrfA == vrfB {
		t.Fatalf("distinct Loc-RIB VRFs collided on %q", vrfA)
	}
	for _, tok := range []string{base, vrfA, vrfB} {
		if !validToken(tok) {
			t.Fatalf("PeerToken = %q is not a valid subject token", tok)
		}
	}
}

// --- Route/Ls/Peer/Stats/Raw: token-count and precondition guards ---

func TestRouteIsSixTokens(t *testing.T) {
	got := Route("ipv4u", "10-0-0-1", "10-0-0-9")
	if n := strings.Count(got, "."); n != 5 {
		t.Fatalf("Route(...) = %q has %d dots, want 5 (6 tokens)", got, n)
	}
}

func TestSubjectTokenCounts(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want int // dot count
	}{
		{"Route", Route("ipv4u", "r", "p"), 5},
		{"Ls", Ls("r", "p"), 4},
		{"Peer", Peer("r", "p"), 4},
		{"Stats", Stats("r", "p"), 4},
		{"Raw", Raw("r"), 3},
	}
	for _, c := range cases {
		if n := strings.Count(c.got, "."); n != c.want {
			t.Errorf("%s = %q has %d dots, want %d", c.name, c.got, n, c.want)
		}
	}
}

// TestRealisticRouterAndIPv6PeerSubject builds a subject from an actual
// hostnamed-router IP and a real IPv6 peer address, and confirms it is a
// well-formed, exactly-6-token, filterable subject.
func TestRealisticRouterAndIPv6PeerSubject(t *testing.T) {
	router := EncodeIP(netip.MustParseAddr("203.0.113.7"))
	peer := EncodeIP(netip.MustParseAddr("2001:db8:85a3::8a2e:370:7334"))
	got := Route(FamilyToken(bgp.Family{AFI: 2, SAFI: 1}), router, peer)
	want := "vantage.v1.route.ipv6u.cb007107.20010db885a3000000008a2e03707334"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if n := strings.Count(got, "."); n != 5 {
		t.Fatalf("%q has %d dots, want 5", got, n)
	}
	// The whole point of hex over escaped text: a subject round-trips back
	// to the real addresses, so operator tooling never has to guess.
	parts := strings.Split(got, ".")
	if r, err := DecodeIP(parts[4]); err != nil || r != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("router token %q decoded to %v (err %v)", parts[4], r, err)
	}
	if p, err := DecodeIP(parts[5]); err != nil || p != netip.MustParseAddr("2001:db8:85a3::8a2e:370:7334") {
		t.Fatalf("peer token %q decoded to %v (err %v)", parts[5], p, err)
	}
}

func expectPanic(t *testing.T, wantSubstr string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, got none")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, wantSubstr) {
			t.Fatalf("want panic containing %q, got: %v", wantSubstr, r)
		}
	}()
	fn()
}

func TestRoutePanicsOnEmbeddedDot(t *testing.T) {
	expectPanic(t, "not a valid NATS subject token", func() {
		Route("ipv4u", "router.evil.example", "10-0-0-9")
	})
}

func TestRoutePanicsOnEmptyToken(t *testing.T) {
	expectPanic(t, "not a valid NATS subject token", func() {
		Route("ipv4u", "", "10-0-0-9")
	})
}

func TestRoutePanicsOnWildcardTokens(t *testing.T) {
	expectPanic(t, "not a valid NATS subject token", func() {
		Route("ipv4u", "*", "10-0-0-9")
	})
	expectPanic(t, "not a valid NATS subject token", func() {
		Route("ipv4u", "10-0-0-1", ">")
	})
}

func TestRoutePanicsOnWhitespaceToken(t *testing.T) {
	expectPanic(t, "not a valid NATS subject token", func() {
		Route("ipv4u", "10 0 0 1", "10-0-0-9")
	})
}

func TestRoutePanicsOnControlByteToken(t *testing.T) {
	expectPanic(t, "not a valid NATS subject token", func() {
		Route("ipv4u", "router\x00evil", "10-0-0-9")
	})
}

func TestLsPeerStatsRawPanicOnBadTokens(t *testing.T) {
	expectPanic(t, "not a valid NATS subject token", func() { Ls("router.evil", "p") })
	expectPanic(t, "not a valid NATS subject token", func() { Ls("r", "") })
	expectPanic(t, "not a valid NATS subject token", func() { Peer("r", "*") })
	expectPanic(t, "not a valid NATS subject token", func() { Stats(">", "p") })
	expectPanic(t, "not a valid NATS subject token", func() { Raw("") })
	expectPanic(t, "not a valid NATS subject token", func() { Raw("a.b") })
}

// --- Fuzz targets ---

// FuzzEncodeIP asserts the invariant that matters for a function deriving a
// subject token from untrusted wire-shaped data: for absolutely any 4 or 16
// address bytes and any IPv6 zone string (net/netip places no restriction
// on zone content at all -- see TestEncodeIPZoneInjectionChars), EncodeIP
// never panics and always returns a non-empty, safe subject token.
func FuzzEncodeIP(f *testing.F) {
	f.Add(false, byte(10), byte(1), byte(2), byte(3), uint64(0), uint64(0), "")
	f.Add(true, byte(0), byte(0), byte(0), byte(0), uint64(0x20010db800000000), uint64(0), "")
	f.Add(true, byte(0), byte(0), byte(0), byte(0), uint64(0xfe80000000000000), uint64(1), "eth0")
	f.Add(true, byte(0), byte(0), byte(0), byte(0), uint64(0xfe80000000000000), uint64(1), "eth0*evil>.\t\n\x00\xff")
	// The invalid zero Addr renders as "invalid IP" -- a leaked space if
	// unescaped. AddrFrom4/AddrFrom16 can never produce it, so without this
	// seed the path is unreachable from the fuzzer.
	f.Add(false, byte(0), byte(0), byte(0), byte(0), uint64(1), uint64(0), "zero")
	f.Fuzz(func(t *testing.T, isV6 bool, a, b, c, d byte, hi, lo uint64, zone string) {
		var addr netip.Addr
		switch {
		case zone == "zero" && !isV6:
			addr = netip.Addr{} // invalid zero Addr
		case isV6:
			var raw [16]byte
			binary.BigEndian.PutUint64(raw[:8], hi)
			binary.BigEndian.PutUint64(raw[8:], lo)
			addr = netip.AddrFrom16(raw)
			if zone != "" {
				addr = addr.WithZone(zone)
			}
		default:
			addr = netip.AddrFrom4([4]byte{a, b, c, d})
		}
		got := EncodeIP(addr) // must never panic
		if !validToken(got) {
			t.Fatalf("EncodeIP(%v) = %q is not a valid subject token", addr, got)
		}
	})
}

// TestEncodeIPInjectiveOverMappedAndHexForms guards the collision class that
// the hand-written address table could not: because '.' and ':' both render
// as '-', Go's dotted text for an IPv4-mapped address (::ffff:1.2.3.4) and
// its colon text for the *distinct* address ::ffff:1:2:3:4 both sanitized to
// "--ffff-1-2-3-4". A sweep found 3600 such pairs, all reachable from the
// wire, since bmp.ParsePeerHeader builds Is4In6 addresses from raw v6 bytes.
// EncodeIP unmaps Is4In6 to break the tie; this asserts the property rather
// than a fixed list, so a future change to the escape scheme cannot silently
// reintroduce it.
func TestEncodeIPInjectiveOverMappedAndHexForms(t *testing.T) {
	seen := map[string]netip.Addr{}
	for _, o1 := range []byte{0, 1, 10, 99, 192, 255} {
		for _, o2 := range []byte{0, 2, 20, 255} {
			for _, o3 := range []byte{0, 3, 30} {
				for _, o4 := range []byte{1, 4, 40, 255} {
					mapped := netip.AddrFrom4([4]byte{o1, o2, o3, o4})
					hex := netip.MustParseAddr(fmt.Sprintf("::ffff:%x:%x:%x:%x",
						uint16(o1), uint16(o2), uint16(o3), uint16(o4)))
					for _, a := range []netip.Addr{mapped, netip.AddrFrom16(mapped.As16()), hex} {
						tok := EncodeIP(a)
						if prev, ok := seen[tok]; ok && prev != a && !sameAddress(prev, a) {
							t.Fatalf("collision: EncodeIP(%v) and EncodeIP(%v) both = %q", prev, a, tok)
						}
						seen[tok] = a
					}
				}
			}
		}
	}
}

// sameAddress reports whether two Addrs denote the same address ignoring the
// IPv4-mapped-vs-IPv4 distinction, which EncodeIP deliberately collapses.
func sameAddress(x, y netip.Addr) bool { return x.Unmap() == y.Unmap() }

// FuzzPeerToken mirrors FuzzEncodeIP for the peer-header-derived token,
// covering every PeerHeader.Type value (not just the four named
// constants -- a malformed/future BMP message could carry any byte), every
// flags byte, and any Distinguisher. The flags byte is fuzzed because
// PeerToken now reads it: ribDirection turns RFC 7854's L flag and RFC
// 8671's O flag into a token suffix, so "never panics and never returns an
// unsafe token, for any PeerHeader whatsoever" is only a claim this fuzz
// backs if the flags byte varies too.
func FuzzPeerToken(f *testing.F) {
	f.Add(byte(bmp.PeerTypeGlobal), byte(0), uint64(0), false, byte(10), byte(0), byte(0), byte(9), uint64(0), uint64(0), "")
	f.Add(byte(bmp.PeerTypeRD), byte(0x40), uint64(42), false, byte(10), byte(0), byte(0), byte(9), uint64(0), uint64(0), "")
	f.Add(byte(bmp.PeerTypeGlobal), byte(0x50), uint64(0), false, byte(10), byte(0), byte(0), byte(9), uint64(0), uint64(0), "")
	f.Add(byte(bmp.PeerTypeLocRIB), byte(0x90), uint64(0), false, byte(0), byte(0), byte(0), byte(0), uint64(0), uint64(0), "")
	f.Add(byte(255), byte(255), uint64(0xFFFFFFFFFFFFFFFF), true, byte(0), byte(0), byte(0), byte(0), uint64(0xfe80000000000000), uint64(1), "z*.>\t\x00")
	f.Fuzz(func(t *testing.T, typ, flags byte, distinguisher uint64, isV6 bool, a, b, c, d byte, hi, lo uint64, zone string) {
		var addr netip.Addr
		if isV6 {
			var raw [16]byte
			binary.BigEndian.PutUint64(raw[:8], hi)
			binary.BigEndian.PutUint64(raw[8:], lo)
			addr = netip.AddrFrom16(raw)
			if zone != "" {
				addr = addr.WithZone(zone)
			}
		} else {
			addr = netip.AddrFrom4([4]byte{a, b, c, d})
		}
		ph := bmp.PeerHeader{Type: typ, Flags: flags, Distinguisher: distinguisher, Addr: addr}
		got := PeerToken(ph) // must never panic
		if !validToken(got) {
			t.Fatalf("PeerToken(%+v) = %q is not a valid subject token", ph, got)
		}
		// Every token is its base plus a direction suffix, and the base is
		// itself a valid token -- the property collector state that borrows
		// across RIB directions keys on.
		base := PeerBaseToken(ph)
		if !validToken(base) {
			t.Fatalf("PeerBaseToken(%+v) = %q is not a valid subject token", ph, base)
		}
		if !strings.HasPrefix(got, base) {
			t.Fatalf("PeerToken(%+v) = %q does not start with its base token %q", ph, got, base)
		}
	})
}

// FuzzFamilyToken covers every possible AFI/SAFI pair, including ones this
// package has no registry entry for.
func FuzzFamilyToken(f *testing.F) {
	f.Add(uint16(1), uint8(1))
	f.Add(uint16(16388), uint8(71))
	f.Add(uint16(0), uint8(0))
	f.Add(uint16(65535), uint8(255))
	f.Fuzz(func(t *testing.T, afi uint16, safi uint8) {
		got := FamilyToken(bgp.Family{AFI: afi, SAFI: safi}) // must never panic
		if !validToken(got) {
			t.Fatalf("FamilyToken({%d,%d}) = %q is not a valid subject token", afi, safi, got)
		}
	})
}

// FuzzRoute exercises the builder-guards-its-preconditions side of the
// package with arbitrary strings standing in for family/router/peer. Route
// is allowed (expected) to panic on an unsafe token -- that's the whole
// point of mustToken -- but if it returns at all, the result must have
// exactly 6 tokens and the family/router/peer must reappear verbatim in
// their positions (i.e. nothing was corrupted or merged).
// FuzzRoute asserts the *biconditional*: Route returns exactly when all three
// caller tokens are already valid, and panics otherwise. An earlier version
// swallowed the panic with a bare recover() and then only compared dot-split
// token counts, so it would have accepted wildcard, empty, whitespace, NUL and
// non-ASCII tokens if validToken were ever weakened -- it caught embedded dots
// and nothing else. Asserting "panicked == !allValid" is what makes every
// injection class load-bearing here rather than only in the unit tests.
func FuzzRoute(f *testing.F) {
	f.Add("ipv4u", "10-0-0-1", "10-0-0-9")
	f.Add("evpn", "router.evil", "peer") // embedded dot: must panic, not merge tokens
	f.Add("", "", "")
	f.Add("x", "*", ">")
	f.Add("x", "a b", "p\x00q")
	f.Add("x", "\xff\xfe", "p")
	f.Fuzz(func(t *testing.T, family, router, peer string) {
		wantPanic := !validToken(family) || !validToken(router) || !validToken(peer)

		var got string
		panicked := func() (p bool) {
			defer func() {
				if r := recover(); r != nil {
					p = true
				}
			}()
			got = Route(family, router, peer)
			return false
		}()

		if panicked != wantPanic {
			t.Fatalf("Route(%q,%q,%q): panicked=%v, want %v", family, router, peer, panicked, wantPanic)
		}
		if panicked {
			return
		}
		parts := strings.Split(got, ".")
		if len(parts) != 6 {
			t.Fatalf("Route(%q,%q,%q) = %q has %d tokens, want 6", family, router, peer, got, len(parts))
		}
		if parts[0] != "vantage" || parts[1] != "v1" || parts[2] != "route" {
			t.Fatalf("Route(%q,%q,%q) = %q: wrong fixed prefix", family, router, peer, got)
		}
		if parts[3] != family || parts[4] != router || parts[5] != peer {
			t.Fatalf("Route(%q,%q,%q) = %q: token mismatch", family, router, peer, got)
		}
		// Every emitted token must independently be injection-free, not just
		// free of the '.' that Split already accounts for.
		for i, p := range parts {
			if !validToken(p) {
				t.Fatalf("Route(%q,%q,%q) = %q: token %d (%q) is not a valid subject token", family, router, peer, got, i, p)
			}
		}
	})
}

// TestPeerTokenLocRIBIgnoresRIBDirectionFlags pins RFC 9069 §4.2 against RFC
// 8671: for the Loc-RIB peer type the per-peer flags byte is redefined (F at
// 0x80, everything else reserved), so bits 0x40 and 0x10 are not the L and O
// flags there and must not split one Loc-RIB peer across four subjects, four
// collector peer states and four sequence counters. A sender that leaves
// junk in the reserved bits -- which the RFC permits a reader to expect,
// since it only requires them to be sent as zero -- must still land on one
// token.
func TestPeerTokenLocRIBIgnoresRIBDirectionFlags(t *testing.T) {
	for _, flags := range []byte{0x00, 0x10, 0x40, 0x50, 0x80, 0xF0} {
		ph := bmp.PeerHeader{Type: bmp.PeerTypeLocRIB, Flags: flags, Addr: netip.MustParseAddr("10.0.0.9")}
		if got := PeerToken(ph); got != "locrib" {
			t.Errorf("PeerToken(locrib, flags=%#02x) = %q, want %q", flags, got, "locrib")
		}
	}
	// The Distinguisher suffix still applies (RFC 9069 §4.2 uses it to name
	// the VRF), and is still unaffected by the flags byte.
	withRD := bmp.PeerHeader{Type: bmp.PeerTypeLocRIB, Flags: 0x50, Distinguisher: 42}
	plain := bmp.PeerHeader{Type: bmp.PeerTypeLocRIB, Distinguisher: 42}
	if got, want := PeerToken(withRD), PeerToken(plain); got != want {
		t.Errorf("PeerToken(locrib+rd, flags=0x50) = %q, want %q", got, want)
	}
	if PeerToken(withRD) == "locrib" {
		t.Errorf("PeerToken(locrib+rd) = %q, want a distinguisher suffix", PeerToken(withRD))
	}
}

// TestPeerTokenIsBasePlusDirection pins the split PeerBaseToken exists for:
// the four RIB views a router may mirror for one neighbor produce four
// distinct tokens that all share one base, so a caller can key per-view
// state on PeerToken and per-session state on PeerBaseToken.
func TestPeerTokenIsBasePlusDirection(t *testing.T) {
	addr := netip.MustParseAddr("10.0.0.9")
	base := PeerBaseToken(bmp.PeerHeader{Addr: addr})
	seen := map[string]byte{}
	for _, flags := range []byte{0x00, 0x40, 0x10, 0x50} {
		ph := bmp.PeerHeader{Flags: flags, Addr: addr}
		tok := PeerToken(ph)
		if PeerBaseToken(ph) != base {
			t.Errorf("PeerBaseToken(flags=%#02x) = %q, want %q for every RIB view of one session", flags, PeerBaseToken(ph), base)
		}
		if prev, ok := seen[tok]; ok {
			t.Errorf("flags %#02x and %#02x share the token %q", prev, flags, tok)
		}
		seen[tok] = flags
	}
	if len(seen) != 4 {
		t.Errorf("four RIB views produced %d tokens: %v", len(seen), seen)
	}
}

// TestPeerTokenDirectionSuffixVectors pins the exact suffix spelling, and
// its position after the "-r" Distinguisher suffix, so the encoding can't
// drift while still satisfying the distinctness check above.
func TestPeerTokenDirectionSuffixVectors(t *testing.T) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 42)
	h := fnv.New32a()
	h.Write(b[:])
	rd := "-r" + hex32(h.Sum32())

	for _, tc := range []struct {
		flags uint8
		dist  uint64
		want  string
	}{
		{0x40, 0, "0a000009-dpost"},
		{0x10, 0, "0a000009-dout"},
		{0x50, 0, "0a000009-doutpost"},
		{0x10, 42, "0a000009" + rd + "-dout"},
	} {
		ph := bmp.PeerHeader{Type: bmp.PeerTypeRD, Flags: tc.flags, Distinguisher: tc.dist,
			Addr: netip.MustParseAddr("10.0.0.9")}
		if got := PeerToken(ph); got != tc.want {
			t.Errorf("PeerToken(flags %#02x, dist %d) = %q, want %q", tc.flags, tc.dist, got, tc.want)
		}
	}
}

// TestBeatSubject: a collector's heartbeat goes on a STATS subject keyed by
// its collector_id, and the key has to survive every id an operator can
// write. collector_id is free text -- a hostname by default, dots included --
// so a scheme that escaped or replaced characters would put two collectors
// on one subject, and a subject shared by two collectors is a heartbeat that
// vouches for the wrong one.
func TestBeatSubject(t *testing.T) {
	if got, want := Beat("dev-c1"), "vantage.v1.stats.beat.id6465762d6331"; got != want {
		t.Fatalf("Beat(%q) = %q, want %q", "dev-c1", got, want)
	}
	seen := map[string]string{}
	for _, id := range []string{
		"a.b", "a-b", "a_b", "ab", "dev1", "vantage-collector-0",
		"collector.example.com", "with space", "ünïcode",
	} {
		s := Beat(id)
		if !strings.HasPrefix(s, Prefix+".stats.beat.") {
			t.Errorf("Beat(%q) = %q, not under %s.stats.beat. -- the STATS stream "+
				"captures vantage.v1.stats.> and nothing else", id, s, Prefix)
		}
		if prev, ok := seen[s]; ok {
			t.Errorf("Beat(%q) and Beat(%q) are both %q", prev, id, s)
		}
		seen[s] = id
		toks := strings.Split(s, ".")
		if len(toks) != 5 {
			t.Errorf("Beat(%q) = %q has %d tokens, want 5", id, s, len(toks))
			continue
		}
		for _, tok := range toks {
			if !validToken(tok) {
				t.Errorf("Beat(%q) = %q carries unsafe token %q", id, s, tok)
			}
		}
		// "dev1" is the case this pins: its hex, 64657631, is exactly eight
		// hex digits, which DecodeIP -- and so `vantage debug` -- would print
		// as 100.101.118.49 if the token were bare hex.
		if a, err := DecodeIP(toks[4]); err == nil {
			t.Errorf("DecodeIP accepted Beat(%q)'s key %q as the address %s", id, toks[4], a)
		}
	}
}

func TestBeatPanicsOnAnEmptyCollectorID(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Beat(\"\") returned; an empty collector_id names no collector")
		}
	}()
	Beat("")
}
