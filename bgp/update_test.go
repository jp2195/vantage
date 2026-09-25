package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// TestParseUpdateAnnounce, TestParseUpdateWithdrawAndEoR,
// TestParseUpdateAddPathNegotiated, TestParseUpdateAddPathHeuristic,
// TestParseUpdateUnknownAttrPreserved, TestParseUpdate7606OptionalDiscard,
// TestParseUpdate7606WellKnownTreatAsWithdraw, FuzzParseUpdate's seed#0, and
// spliceAttr cover the baseline UPDATE-parsing cases (FuzzParseUpdate's
// seed corpus and documentation are expanded well beyond a single seed —
// see below).

func capsV4(addPath bool) Caps {
	c := newCaps()
	c.FourByteAS = true
	c.MP[FamilyIPv4U] = true
	if addPath {
		c.AddPathRecv[FamilyIPv4U] = true
	}
	return c
}

func mustPfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func TestParseUpdateAnnounce(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced:   []Prefix{{Prefix: mustPfx("192.0.2.0/24")}, {Prefix: mustPfx("198.51.100.0/25")}},
		Origin:      0,
		ASPath:      []uint32{65001, 4200000001},
		FourByteAS:  true,
		NextHop:     netip.MustParseAddr("10.0.0.9"),
		Communities: []uint32{65001<<16 | 100},
	})
	u, err := ParseUpdate(msg[19:], capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Announced) != 2 || u.Announced[0].Prefix != mustPfx("192.0.2.0/24") {
		t.Fatalf("announced=%v", u.Announced)
	}
	if u.Attrs.NextHop != "10.0.0.9" || len(u.Attrs.AsPath) != 1 ||
		u.Attrs.AsPath[0].Asns[1] != 4200000001 || u.Attrs.Communities[0] != 65001<<16|100 {
		t.Fatalf("attrs=%v", u.Attrs)
	}
	if u.Family != FamilyIPv4U || u.EndOfRIB || u.TreatAsWithdraw {
		t.Fatalf("u=%+v", u)
	}
}

func TestParseUpdateWithdrawAndEoR(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{Withdrawn: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}}})
	u, err := ParseUpdate(msg[19:], capsV4(false))
	if err != nil || len(u.Withdrawn) != 1 || u.EndOfRIB {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	eor, err := ParseUpdate(AppendUpdate(nil, BuildUpdate{})[19:], capsV4(false))
	if err != nil || !eor.EndOfRIB || eor.Family != FamilyIPv4U {
		t.Fatalf("eor=%+v err=%v", eor, err)
	}
}

func TestParseUpdateAddPathNegotiated(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24"), PathID: 7}},
		AddPath:   true,
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	u, err := ParseUpdate(msg[19:], capsV4(true))
	if err != nil || u.Announced[0].PathID != 7 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	if len(u.Flags) != 0 {
		t.Fatalf("no heuristic flag expected, got %v", u.Flags)
	}
}

// TestParseUpdateAddPathHeuristic exercises the structural add-path
// heuristic's central failure mode: PathID 9, encoded big-endian, is 00 00
// 00 09 — three leading zero bytes. Read as *plain* (non-add-path) NLRI,
// each of those zero bytes is itself a syntactically valid zero-length
// ("default route") prefix entry, so a naive "try plain first, use it if
// it parses without error" heuristic does not fail here -- it succeeds,
// silently, on the wrong interpretation (confirmed by hand-tracing the
// exact bytes AppendUpdate produces for this fixture: the plain reparse
// runs to completion producing 12 bogus entries, 8 of them the spurious
// zero-length kind, instead of the real 2). See parsePrefixesV4Auto's doc
// comment in prefix.go for the fix (a real UPDATE never legitimately
// repeats 0.0.0.0/0).
func TestParseUpdateAddPathHeuristic(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.1/32"), PathID: 9}, {Prefix: mustPfx("192.0.2.2/32"), PathID: 9}},
		AddPath:   true,
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	u, err := ParseUpdate(msg[19:], newCaps()) // no caps on record
	if err != nil || u.Announced[0].PathID != 9 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC
	}
	if !found {
		t.Fatalf("want heuristic flag, got %v", u.Flags)
	}
}

// TestParseUpdateHeuristicPlainDataNotMisreadAsAddPath covers the
// complementary case: genuinely plain (non-add-path) NLRI with unknown caps
// must still resolve to the plain interpretation (no zero-length-entry
// smell), not be flipped to add-path just because both interpretations
// happen to parse without error.
//
// PARSE_FLAG_ADDPATH_HEURISTIC is set whenever the heuristic ran,
// regardless of which interpretation it settled on -- a run that settles
// on "plain" is still an inference, not a caps-known certainty, and the
// flag must say so. The decoded prefixes (the actual
// not-misread-as-add-path claim this test exists for) are unaffected and
// still asserted below.
func TestParseUpdateHeuristicPlainDataNotMisreadAsAddPath(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}, {Prefix: mustPfx("198.51.100.0/25")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	u, err := ParseUpdate(msg[19:], newCaps()) // no caps on record
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(u.Announced) != 2 || u.Announced[0].Prefix != mustPfx("192.0.2.0/24") ||
		u.Announced[1].Prefix != mustPfx("198.51.100.0/25") {
		t.Fatalf("announced=%v", u.Announced)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_ADDPATH_HEURISTIC even though the heuristic picked plain (it still ran on unknown caps), got %v", u.Flags)
	}
}

func TestParseUpdateUnknownAttrPreserved(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	// splice an unknown optional-transitive attr (type 99) into the attr section
	body := spliceAttr(t, msg[19:], 0xC0, 99, []byte{0xDE, 0xAD})
	u, err := ParseUpdate(body, capsV4(false))
	if err != nil || len(u.Attrs.Unknown) != 1 || u.Attrs.Unknown[0].Type != 99 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
}

func TestParseUpdate7606OptionalDiscard(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	// malformed optional attr: AGGREGATOR (7) with bad length 3
	body := spliceAttr(t, msg[19:], 0xC0, 7, []byte{1, 2, 3})
	u, err := ParseUpdate(body, capsV4(false))
	if err != nil || u.TreatAsWithdraw {
		t.Fatalf("optional malformation must not TaW: %+v err=%v", u, err)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_ATTR_DISCARDED_7606
	}
	if !hasFlag || len(u.Announced) != 1 {
		t.Fatalf("u=%+v", u)
	}
}

func TestParseUpdate7606WellKnownTreatAsWithdraw(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	// malformed well-known: ORIGIN (1) with length 2
	body := spliceAttr(t, msg[19:], 0x40, 1, []byte{0, 0})
	u, err := ParseUpdate(body, capsV4(false))
	if err != nil || !u.TreatAsWithdraw || len(u.Announced) != 1 {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
}

// --- RFC 7606 reclassification, ORIGIN undefined value,
// zero-length COMMUNITIES/Extended Communities ---

// TestParseUpdate7606MEDMalformedTreatAsWithdraw pins that a malformed
// MULTI_EXIT_DISC is treat-as-withdraw, not attribute-discard: RFC 7606
// §7.4 says a malformed instance "SHALL be handled using the approach of
// 'treat-as-withdraw'".
func TestParseUpdate7606MEDMalformedTreatAsWithdraw(t *testing.T) {
	attr := buildAttr(0x80, attrMED, []byte{0, 0, 0}) // 3 bytes, not 4
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("malformed MED must treat-as-withdraw (RFC 7606 §7.4): %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
}

// TestParseUpdate7606CommunitiesMalformedTreatAsWithdraw covers a concrete
// scenario: 192.0.2.0/24 announced with a COMMUNITIES attribute of
// length 6 (not a multiple of 4). Every conformant router on the path
// withdraws this route (RFC 7606 §7.8); this collector previously emitted
// it as a live announcement with TreatAsWithdraw false.
func TestParseUpdate7606CommunitiesMalformedTreatAsWithdraw(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	body := spliceAttr(t, msg[19:], 0xC0, attrCommunity, make([]byte, 6))

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("malformed COMMUNITIES must treat-as-withdraw (RFC 7606 §7.8): %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
}

func TestParseUpdate7606ExtCommunitiesMalformedTreatAsWithdraw(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	body := spliceAttr(t, msg[19:], 0xC0, attrExtComm, make([]byte, 5)) // not a multiple of 8

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("malformed Extended Communities must treat-as-withdraw (RFC 7606 §7.14): %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
}

func TestParseUpdate7606LargeCommunitiesMalformedTreatAsWithdraw(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	body := spliceAttr(t, msg[19:], 0xC0, attrLargeComm, make([]byte, 7)) // not a multiple of 12

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("malformed Large Communities must treat-as-withdraw (RFC 8092 §5): %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
}

// TestParseUpdateLocalPrefMalformedStillAttributeDiscard pins down a
// deliberate exclusion: LOCAL_PREF stays attribute-discard
// (not treat-as-withdraw) because this parser has no iBGP/eBGP knowledge to
// apply RFC 7606 §7.5's direction-dependent rule.
func TestParseUpdateLocalPrefMalformedStillAttributeDiscard(t *testing.T) {
	attr := buildAttr(0x40, attrLocalPref, []byte{0, 0, 0}) // 3 bytes, not 4
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.TreatAsWithdraw {
		t.Fatalf("malformed LOCAL_PREF must stay attribute-discard, not treat-as-withdraw: %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_ATTR_DISCARDED_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_ATTR_DISCARDED_7606, got %v", u.Flags)
	}
}

// TestParseUpdateOriginUndefinedValueTreatAsWithdraw pins RFC 7606 §7.1:
// ORIGIN is malformed if its length is wrong *or* its value is undefined
// (valid values are 0/1/2). Checking the length alone would store Origin=7
// with no error and no flag.
func TestParseUpdateOriginUndefinedValueTreatAsWithdraw(t *testing.T) {
	attr := buildAttr(0x40, attrOrigin, []byte{7})
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("undefined ORIGIN value must treat-as-withdraw (RFC 7606 §7.1): %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
}

// TestParseUpdateCommunitiesZeroLengthMalformed and
// TestParseUpdateExtCommunitiesZeroLengthMalformed pin RFC 7606 §7.8/§7.14,
// which both require a *non-zero* multiple of 4/8: a zero length passes a
// modulo check alone and would yield a silent empty list.
func TestParseUpdateCommunitiesZeroLengthMalformed(t *testing.T) {
	attr := buildAttr(0xC0, attrCommunity, nil)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("zero-length COMMUNITIES must treat-as-withdraw (RFC 7606 §7.8): %+v", u)
	}
}

func TestParseUpdateExtCommunitiesZeroLengthMalformed(t *testing.T) {
	attr := buildAttr(0xC0, attrExtComm, nil)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("zero-length Extended Communities must treat-as-withdraw (RFC 7606 §7.14): %+v", u)
	}
}

// spliceAttr splices one attribute into an UPDATE body, fixing the
// attr-length field.
func spliceAttr(t *testing.T, body []byte, flags, typ uint8, val []byte) []byte {
	t.Helper()
	wl := int(body[0])<<8 | int(body[1])
	alOff := 2 + wl
	al := int(body[alOff])<<8 | int(body[alOff+1])
	attr := append([]byte{flags, typ, byte(len(val))}, val...)
	out := append([]byte{}, body[:alOff]...)
	out = append(out, byte((al+len(attr))>>8), byte(al+len(attr)))
	out = append(out, body[alOff+2:alOff+2+al]...)
	out = append(out, attr...)
	return append(out, body[alOff+2+al:]...)
}

// --- attribute encoding, error paths and edge cases ---

// spliceExtAttr is spliceAttr's counterpart using the RFC 4271 §4.3 Extended
// Length attribute flag (0x10) and its 2-byte length field, so tests can
// exercise that wire form directly instead of only when a value happens to
// exceed 255 bytes.
func spliceExtAttr(t *testing.T, body []byte, flags, typ uint8, val []byte) []byte {
	t.Helper()
	return spliceExtAttrRaw(body, flags, typ, val)
}

// spliceAttrRaw and spliceExtAttrRaw are the *testing.T-free cores of
// spliceAttr/spliceExtAttr, used directly by FuzzParseUpdate's seed corpus
// (seed construction runs before any test is executing, so it must not
// depend on a *testing.T).
func spliceAttrRaw(body []byte, flags, typ uint8, val []byte) []byte {
	attr := append([]byte{flags, typ, byte(len(val))}, val...)
	return spliceRaw(body, attr)
}

func spliceExtAttrRaw(body []byte, flags, typ uint8, val []byte) []byte {
	attr := []byte{flags | 0x10, typ, byte(len(val) >> 8), byte(len(val))}
	attr = append(attr, val...)
	return spliceRaw(body, attr)
}

func spliceRaw(body, attr []byte) []byte {
	wl := int(body[0])<<8 | int(body[1])
	alOff := 2 + wl
	al := int(body[alOff])<<8 | int(body[alOff+1])
	out := append([]byte{}, body[:alOff]...)
	out = append(out, byte((al+len(attr))>>8), byte(al+len(attr)))
	out = append(out, body[alOff+2:alOff+2+al]...)
	out = append(out, attr...)
	return append(out, body[alOff+2+al:]...)
}

// buildBody hand-assembles a raw UPDATE body (post-19-byte-header) from its
// three top-level regions, computing the withdrawn-routes-length and
// total-path-attribute-length fields itself. This lets tests construct wire
// forms AppendUpdate cannot produce directly: MP_REACH/MP_UNREACH attributes
// and deliberately-invalid lengths.
func buildBody(withdrawn, attrs, nlri []byte) []byte {
	body := []byte{byte(len(withdrawn) >> 8), byte(len(withdrawn))}
	body = append(body, withdrawn...)
	body = append(body, byte(len(attrs)>>8), byte(len(attrs)))
	body = append(body, attrs...)
	body = append(body, nlri...)
	return body
}

// buildAttr builds one non-extended-length path-attribute TLV.
func buildAttr(flags, typ uint8, val []byte) []byte {
	return append([]byte{flags, typ, byte(len(val))}, val...)
}

// mpReachVal builds an MP_REACH_NLRI attribute value (RFC 4760 §3): AFI(2)
// SAFI(1) next-hop-length(1) next-hop(nhLen) reserved(1) NLRI.
func mpReachVal(fam Family, nh, nlri []byte) []byte {
	v := binary.BigEndian.AppendUint16(nil, fam.AFI)
	v = append(v, fam.SAFI, byte(len(nh)))
	v = append(v, nh...)
	v = append(v, 0) // reserved
	v = append(v, nlri...)
	return v
}

// mpUnreachVal builds an MP_UNREACH_NLRI attribute value (RFC 4760 §3):
// AFI(2) SAFI(1) NLRI.
func mpUnreachVal(fam Family, nlri []byte) []byte {
	v := binary.BigEndian.AppendUint16(nil, fam.AFI)
	v = append(v, fam.SAFI)
	v = append(v, nlri...)
	return v
}

var familyVPNv4 = Family{AFI: 1, SAFI: 128} // RFC 4364, MPLS-labeled VPN unicast

// --- MP_REACH / MP_UNREACH: typed FamilyIPv4U path ---

func TestParseUpdateMPReachTypedIPv4Unicast(t *testing.T) {
	nlri := AppendPrefixesV4(nil, []Prefix{{Prefix: mustPfx("203.0.113.0/24")}}, false)
	attr := buildAttr(0x80, attrMPReach, mpReachVal(FamilyIPv4U, []byte{10, 0, 0, 9}, nlri))
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Family != FamilyIPv4U || u.Attrs.NextHop != "10.0.0.9" ||
		len(u.Announced) != 1 || u.Announced[0].Prefix != mustPfx("203.0.113.0/24") {
		t.Fatalf("u=%+v", u)
	}
	if len(u.RawReach) != 0 {
		t.Fatalf("typed family must not populate RawReach, got %x", u.RawReach)
	}
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY {
			t.Fatalf("typed family must not set PARSE_FLAG_UNKNOWN_FAMILY, got %v", u.Flags)
		}
	}
}

// TestParseUpdateMPReachRFC8950ExtendedNextHop covers a real, deployed
// wire form not otherwise exercised: RFC 8950 (formerly RFC 5549)
// IPv4-unicast NLRI carried via MP_REACH with a 16-byte IPv6 next hop
// (unnumbered IPv4-over-IPv6 links). Attrs.NextHop was previously only
// set when next-hop-length == 4, silently dropping this next hop with no
// error and no flag for any other length.
func TestParseUpdateMPReachRFC8950ExtendedNextHop(t *testing.T) {
	v6nh := netip.MustParseAddr("2001:db8::1").As16()
	nlri := AppendPrefixesV4(nil, []Prefix{{Prefix: mustPfx("203.0.113.0/24")}}, false)
	attr := buildAttr(0x80, attrMPReach, mpReachVal(FamilyIPv4U, v6nh[:], nlri))
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Attrs.NextHop != "2001:db8::1" {
		t.Fatalf("want RFC 8950 IPv6 next hop decoded, got %q", u.Attrs.NextHop)
	}
	if len(u.Announced) != 1 || u.Announced[0].Prefix != mustPfx("203.0.113.0/24") {
		t.Fatalf("announced=%v", u.Announced)
	}
}

func TestParseUpdateMPReachBogusNextHopLength(t *testing.T) {
	// next-hop-length declares 200 bytes; value has nowhere near that many.
	v := []byte{0, 1, 1, 200, 1, 2, 3, 4}
	attr := buildAttr(0x80, attrMPReach, v)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("bogus next-hop length must go through the 7606 ladder, not a hard error: %v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("want TreatAsWithdraw for malformed MP_REACH, got %+v", u)
	}
}

func TestParseUpdateMPReachUnsupportedNextHopLengthForIPv4U(t *testing.T) {
	// next-hop-length 8 for ipv4-unicast is neither the classic 4-byte form
	// nor a recognized RFC 8950 IPv6 form (16/32) -- malformed, not silently
	// accepted with no next hop decoded and no signal.
	attr := buildAttr(0x80, attrMPReach, mpReachVal(FamilyIPv4U, make([]byte, 8), nil))
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("want TreatAsWithdraw for unsupported next-hop-length, got %+v", u)
	}
}

// --- MP_REACH / MP_UNREACH: raw-bytes fallback ---

// TestParseUpdateMPReachOtherFamilyRawPreserved proves the raw-bytes
// fallback for a non-ipv4u family (VPNv4): the raw MP_REACH attribute
// value bytes must be preserved verbatim in RawReach and NLRI decoding
// must not produce any typed results.
//
// VPNv4 is a decoded family, and the NLRI bytes here are 8 arbitrary
// bytes, not a valid vpn4 wire encoding -- the decoder runs and fails, so
// the flag raised is PARSE_FLAG_NLRI_UNTYPED ("a decoder ran and failed"),
// not PARSE_FLAG_UNKNOWN_FAMILY, which means "no decoder exists for this
// family at all." That case is covered separately by
// TestParseUpdateUnknownFamilyStillRaw. The raw-bytes-preserved guarantee
// this test exists to pin still holds either way.
func TestParseUpdateMPReachOtherFamilyRawPreserved(t *testing.T) {
	nh := []byte{10, 0, 0, 9}
	nlri := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07} // not a valid vpn4 entry: decodes with an error
	val := mpReachVal(familyVPNv4, nh, nlri)
	attr := buildAttr(0x80, attrMPReach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Family != familyVPNv4 {
		t.Fatalf("family=%+v, want %+v", u.Family, familyVPNv4)
	}
	if string(u.RawReach) != string(val) {
		t.Fatalf("RawReach=%x, want exact attribute value %x", u.RawReach, val)
	}
	if len(u.Announced) != 0 || len(u.VpnAnnounced) != 0 {
		t.Fatalf("a failed decode must produce no typed results, got announced=%+v vpn=%+v", u.Announced, u.VpnAnnounced)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
}

// TestParseUpdateMPReachFamilyAndRawPreservedEvenWhenMalformed is a
// regression test using this exact byte sequence: AFI 1, SAFI 128
// (VPNv4), next-hop-length 200, but the value is only 8 bytes total -- the
// next-hop-length bound check fails (8 < 4+200+1). u.Family = fam used to
// be committed before that check ran, so a failure there left
// Family={1,128} set but RawReach nil and no flag at all: a deferred
// family with neither raw bytes nor a signal that anything went wrong.
// The raw-bytes-plus-flag branch now runs above that bound check, so for
// any non-ipv4u family, the raw bytes and a flag are preserved regardless
// of whether the rest of the attribute is structurally sound.
//
// This failure is caught by the next-hop-length bound check, which runs
// before family dispatch -- decodeNLRI is never even called on this path.
// VPNv4 has a registered decoder, so the flag raised is
// PARSE_FLAG_NLRI_UNTYPED: the bound check still runs first (it must --
// the NLRI's start offset isn't known to be in bounds until it passes),
// but the flag it raises on failure is chosen by whether a decoder is
// registered for the family at all, via familyHasDecoder, not hard-coded
// to NLRI_UNTYPED regardless of family. The same malformed shape for a
// family with NO registered decoder instead gets
// PARSE_FLAG_UNKNOWN_FAMILY -- see
// TestParseUpdateMPReachMalformedNextHopLengthUnknownFamilyFlagsUnknownFamily.
func TestParseUpdateMPReachFamilyAndRawPreservedEvenWhenMalformed(t *testing.T) {
	v := []byte{0x00, 0x01, 0x80, 0xC8, 0x01, 0x02, 0x03, 0x04}
	attr := buildAttr(0x80, attrMPReach, v)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Family != familyVPNv4 {
		t.Fatalf("family=%+v, want %+v", u.Family, familyVPNv4)
	}
	if len(u.RawReach) == 0 {
		t.Fatalf("want RawReach preserved for a malformed non-ipv4u MP_REACH, got empty")
	}
	if string(u.RawReach) != string(v) {
		t.Fatalf("RawReach=%x, want %x", u.RawReach, v)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
	// Preserving the raw bytes must not cost the RFC 7606 outcome. The
	// next-hop-length bound is family-independent (RFC 4760 §3), so this
	// input is malformed whatever the family and must still treat-as-withdraw.
	if !u.TreatAsWithdraw {
		t.Fatalf("a structurally malformed MP_REACH must treat-as-withdraw even for a deferred family: %+v", u)
	}
}

// familyUndecoded is an AFI/SAFI pair with no registered decodeNLRI entry --
// used wherever a test's point is specifically "no decoder exists for this
// family," now that VPNv4 has a registered decoder and no longer fits that
// description.
//
// {AFI: 2, SAFI: 1} (IPv6 unicast) previously served this role, but it is a
// real, recognized family: subjects maps it to the token "ipv6u",
// so it does not fall back to the "x{afi}-{safi}" unrecognized-family form
// that PARSE_FLAG_UNKNOWN_FAMILY is meant to describe. {AFI: 65534, SAFI:
// 119} matches TestParseUpdateUnknownFamilyStillRaw's own fixture and is
// absent from every family table in the package (subjects.familyNames and
// this package's decodeNLRI switch), so it is a genuinely unrecognized
// family end to end, not merely undecoded by this package alone.
var familyUndecoded = Family{AFI: 65534, SAFI: 119}

// TestParseUpdateMPReachWellFormedDeferredFamily is the counterpart to the
// test above: a *well-formed* deferred-family MP_REACH must keep the raw
// bytes and the UNKNOWN_FAMILY flag while producing no 7606 outcome at all.
// Without this, a change that made every deferred family treat-as-withdraw
// would still pass the malformed case.
//
// This test deliberately uses familyUndecoded rather than VPNv4. VPNv4 has
// a registered decoder, and the opaque NLRI bytes below are not a valid
// vpn4 entry, so VPNv4 here would not exercise "deferred" (no decoder)
// behavior -- it would exercise the decoder-ran-and-failed path already
// covered by TestParseUpdateMPReachOtherFamilyRawPreserved and
// TestParseUpdateVpn4MalformedKeepsRaw.
func TestParseUpdateMPReachWellFormedDeferredFamily(t *testing.T) {
	// AFI 65534, SAFI 119 (familyUndecoded), next-hop-length 12, 12
	// next-hop bytes, the RFC 4760 §3 reserved byte, then opaque NLRI.
	v := []byte{0xFF, 0xFE, 0x77, 0x0C}
	v = append(v, make([]byte, 12)...)
	v = append(v, 0x00)
	v = append(v, 0x70, 0x00, 0x01, 0x02)
	attr := buildAttr(0x80, attrMPReach, v)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Family != familyUndecoded {
		t.Fatalf("family=%+v, want %+v", u.Family, familyUndecoded)
	}
	if string(u.RawReach) != string(v) {
		t.Fatalf("RawReach=%x, want %x", u.RawReach, v)
	}
	if u.TreatAsWithdraw {
		t.Fatalf("a well-formed deferred-family MP_REACH must not treat-as-withdraw: %+v", u)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_UNKNOWN_FAMILY, got %v", u.Flags)
	}
}

// TestParseUpdate7606LargeCommunitiesZeroLengthTreatAsWithdraw pins RFC 8092
// §5's "nonzero multiple of 12" -- the same wording RFC 7606 §7.8/§7.14 use
// for COMMUNITIES and EXTENDED COMMUNITIES. A zero length passed the bare
// `%12 != 0` test and yielded a silent empty list with no flag, which is an
// accommodation without a flag.
func TestParseUpdate7606LargeCommunitiesZeroLengthTreatAsWithdraw(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	body := spliceAttr(t, msg[19:], 0xC0, attrLargeComm, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("zero-length LARGE_COMMUNITIES must treat-as-withdraw (RFC 8092 §5): %+v", u)
	}
}

// TestParseUpdateNoHeuristicFlagForEmptyPrefixRegion guards a flag-precision
// regression. The heuristic reports "it ran" rather than "it chose add-path",
// but for an empty region both interpretations trivially succeed, so the
// "both structurally valid" branch fired with nothing to decide. Because
// ParseUpdate's withdrawn-routes call is unconditional, that meant every
// unknown-caps UPDATE past the classic End-of-RIB early return carried the
// flag -- including MP-only UPDATEs with no IPv4-unicast region at all, such
// as a VPNv4 MP_REACH seen before Peer-Up.
func TestParseUpdateNoHeuristicFlagForEmptyPrefixRegion(t *testing.T) {
	v := []byte{0x00, 0x01, 0x80, 0x0C}
	v = append(v, make([]byte, 12)...)
	v = append(v, 0x00)
	v = append(v, 0x70, 0x00, 0x01, 0x02)
	attr := buildAttr(0x80, attrMPReach, v)
	body := buildBody(nil, attr, nil)

	// newCaps() leaves MP empty, so ParseUpdate's `known` gate is false and
	// the heuristic path is the one under test.
	u, err := ParseUpdate(body, newCaps())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC {
			t.Fatalf("heuristic flag set for an UPDATE with no ipv4-unicast prefix region: %v", u.Flags)
		}
	}
}

// TestAppendUpdateUndefinedOriginPanics pins the builder precondition: an
// ORIGIN above 2 is undefined (RFC 7606 §7.1) and ParseUpdate treats it as
// withdraw, so emitting one would build a fixture this package's own parser
// rejects.
func TestAppendUpdateUndefinedOriginPanics(t *testing.T) {
	build := func(origin uint8) (panicked bool, msg string) {
		defer func() {
			if r := recover(); r != nil {
				panicked, msg = true, fmt.Sprint(r)
			}
		}()
		AppendUpdate(nil, BuildUpdate{
			Origin:    origin,
			Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
			NextHop:   netip.MustParseAddr("10.0.0.9"),
		})
		return false, ""
	}

	if panicked, _ := build(2); panicked {
		t.Fatalf("ORIGIN 2 (INCOMPLETE) is defined and must not panic")
	}
	panicked, msg := build(3)
	if !panicked {
		t.Fatalf("ORIGIN 3 is undefined and must panic")
	}
	if !strings.Contains(msg, "ORIGIN value 3") {
		t.Fatalf("panic message should name the offending value, got %q", msg)
	}
}

// TestParseUpdateMPUnreachOtherFamilyRawPreserved is
// TestParseUpdateMPReachOtherFamilyRawPreserved's MP_UNREACH counterpart —
// the other half of the raw-bytes fallback for a non-BGP-LS family.
//
// This follows the same reclassification as its MP_REACH counterpart --
// VPNv4 now has a decoder, these 8 bytes are not a valid vpn4 entry, so
// the decoder runs and fails, producing PARSE_FLAG_NLRI_UNTYPED rather
// than PARSE_FLAG_UNKNOWN_FAMILY.
func TestParseUpdateMPUnreachOtherFamilyRawPreserved(t *testing.T) {
	nlri := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}
	val := mpUnreachVal(familyVPNv4, nlri)
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Family != familyVPNv4 {
		t.Fatalf("family=%+v, want %+v", u.Family, familyVPNv4)
	}
	if string(u.RawUnreach) != string(val) {
		t.Fatalf("RawUnreach=%x, want exact attribute value %x", u.RawUnreach, val)
	}
	if len(u.Withdrawn) != 0 || len(u.VpnWithdrawn) != 0 {
		t.Fatalf("a failed decode must produce no typed results, got withdrawn=%+v vpn=%+v", u.Withdrawn, u.VpnWithdrawn)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
}

// TestParseUpdateMPUnreachEndOfRIBOtherFamily pins End-of-RIB detection
// for a non-ipv4u family. The obvious check -- `len(u.RawUnreach) == 0 &&
// u.Family != FamilyIPv4U` -- can never be true, because parseMPUnreach
// populates RawUnreach whenever the family isn't FamilyIPv4U, so an
// End-of-RIB marker for any other family (e.g. the initial-table-dump
// completion signal for VPNv4, exactly what a BMP collector doing
// route-monitoring needs to detect) would never be recognized. ParseUpdate
// therefore computes NLRI emptiness from the attribute's own declared
// length in its attribute loop, not from RawUnreach or Withdrawn.
//
// VPNv4 has a decoder, and an empty NLRI decodes cleanly to zero VpnPrefix
// entries -- nothing is lost, so RawUnreach is dropped (the same "drop raw
// bytes on a clean parse" policy that applies to a non-empty typed decode).
// Raw bytes still survive an EoR marker for a family with no decoder at
// all: see TestParseUpdateMPUnreachEndOfRIBUnknownFamilyRawPreserved.
func TestParseUpdateMPUnreachEndOfRIBOtherFamily(t *testing.T) {
	val := mpUnreachVal(familyVPNv4, nil) // AFI+SAFI only, empty NLRI
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.EndOfRIB {
		t.Fatalf("want EndOfRIB=true for an empty-NLRI MP_UNREACH, got %+v", u)
	}
	if u.Family != familyVPNv4 {
		t.Fatalf("family=%+v, want %+v", u.Family, familyVPNv4)
	}
	if len(u.RawUnreach) != 0 {
		t.Fatalf("an empty vpn4 NLRI decodes cleanly (zero entries): RawUnreach must be dropped, got %x", u.RawUnreach)
	}
	if len(u.VpnWithdrawn) != 0 {
		t.Fatalf("want zero typed withdraws for an empty NLRI, got %+v", u.VpnWithdrawn)
	}
}

// TestParseUpdateMPUnreachEndOfRIBUnknownFamilyRawPreserved carries forward
// the same raw-bytes guarantee that
// TestParseUpdateMPUnreachEndOfRIBOtherFamily pinned before VPNv4 had a
// decoder: for a family with no decoder at all, an End-of-RIB
// marker's raw bytes must still be preserved, since there is no decode to
// prove the NLRI really was empty.
func TestParseUpdateMPUnreachEndOfRIBUnknownFamilyRawPreserved(t *testing.T) {
	val := mpUnreachVal(familyUndecoded, nil) // AFI+SAFI only, empty NLRI
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.EndOfRIB {
		t.Fatalf("want EndOfRIB=true for an empty-NLRI MP_UNREACH, got %+v", u)
	}
	if string(u.RawUnreach) != string(val) {
		t.Fatalf("RawUnreach=%x, want %x (raw bytes must still survive an EoR marker on an undecoded family)", u.RawUnreach, val)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_UNKNOWN_FAMILY, got %v", u.Flags)
	}
}

// TestParseUpdateMPUnreachNotEndOfRIBWhenNLRINonEmpty proves the fix above
// doesn't over-fire: the same family with a non-empty (if opaque/undecoded)
// NLRI must NOT be reported as End-of-RIB.
func TestParseUpdateMPUnreachNotEndOfRIBWhenNLRINonEmpty(t *testing.T) {
	val := mpUnreachVal(familyVPNv4, []byte{1, 2, 3})
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.EndOfRIB {
		t.Fatalf("non-empty NLRI must not be reported as End-of-RIB: %+v", u)
	}
}

// TestParseUpdateMPUnreachDuplicateNoFakeEndOfRIB is a regression test for
// the exact MP_UNREACH(with NLRI) then MP_UNREACH(empty) sequence. The
// "empty NLRI" tracking used to hold only the last attribute's value
// while "saw only MP_UNREACH" stayed true, so this sequence fabricated
// EndOfRIB=true, and the second attribute's raw bytes silently overwrote the
// first's (losing the raw bytes for the real withdraw list). RFC 7606
// §3 mandates a NOTIFICATION for a repeated MP_UNREACH; this collector is
// passive, so it applies treat-as-withdraw instead and never lets the
// duplicate touch RawUnreach.
func TestParseUpdateMPUnreachDuplicateNoFakeEndOfRIB(t *testing.T) {
	val1 := mpUnreachVal(familyVPNv4, []byte{1, 2, 3}) // real withdraw list, non-empty NLRI
	val2 := mpUnreachVal(familyVPNv4, nil)             // empty NLRI
	attrs := buildAttr(0x80, attrMPUnreach, val1)
	attrs = append(attrs, buildAttr(0x80, attrMPUnreach, val2)...)
	body := buildBody(nil, attrs, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.EndOfRIB {
		t.Fatalf("duplicate MP_UNREACH must not fabricate End-of-RIB: %+v", u)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("duplicate MP_UNREACH must treat-as-withdraw (RFC 7606 §3): %+v", u)
	}
	hasFlag := false
	for _, f := range u.Flags {
		hasFlag = hasFlag || f == vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606
	}
	if !hasFlag {
		t.Fatalf("want PARSE_FLAG_TREAT_AS_WITHDRAW_7606, got %v", u.Flags)
	}
	if string(u.RawUnreach) != string(val1) {
		t.Fatalf("RawUnreach=%x, want first occurrence %x preserved (not overwritten by the duplicate)", u.RawUnreach, val1)
	}
}

// --- AS_PATH: RFC 5065 confederation segments ---

// TestParseUpdateASPathConfedSegmentAccepted covers a real wire form
// previously rejected outright: RFC 5065 §5 AS_CONFED_SEQUENCE (segment
// type 3), which a router that is itself a BGP confederation member
// legitimately sends when peering with a fellow member. parseASPath used
// to accept only types 1 (AS_SET) and 2 (AS_SEQUENCE), so this
// well-formed confederation UPDATE would have been treated as malformed
// AS_PATH -- a well-known-critical attribute -- forcing the whole route to
// TreatAsWithdraw for input that was never actually corrupt.
func TestParseUpdateASPathConfedSegmentAccepted(t *testing.T) {
	asPath := []byte{3, 1, 0, 0, 0xFD, 0xE9} // AS_CONFED_SEQUENCE, 1 ASN (4-byte): 65001
	attrs := buildAttr(0x40, attrOrigin, []byte{0})
	attrs = append(attrs, buildAttr(0x40, attrASPath, asPath)...)
	nh := []byte{10, 0, 0, 9}
	attrs = append(attrs, buildAttr(0x40, attrNextHop, nh)...)
	nlri := AppendPrefixesV4(nil, []Prefix{{Prefix: mustPfx("192.0.2.0/24")}}, false)
	body := buildBody(nil, attrs, nlri)

	c := capsV4(false)
	u, err := ParseUpdate(body, c)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.TreatAsWithdraw {
		t.Fatalf("well-formed AS_CONFED_SEQUENCE must not TaW: %+v", u)
	}
	if len(u.Attrs.AsPath) != 1 || u.Attrs.AsPath[0].Type != 3 || u.Attrs.AsPath[0].Asns[0] != 65001 {
		t.Fatalf("as_path=%+v", u.Attrs.AsPath)
	}
}

func TestParseUpdateASPathInvalidSegmentTypeTreatAsWithdraw(t *testing.T) {
	asPath := []byte{9, 1, 0, 0, 0xFD, 0xE9} // segment type 9: not defined by any RFC this package knows
	attr := buildAttr(0x40, attrASPath, asPath)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("want TreatAsWithdraw for an invalid AS_PATH segment type, got %+v", u)
	}
}

// --- ATOMIC_AGGREGATE / AGGREGATOR ---

// TestParseUpdateAtomicAggregateMalformedLengthDiscarded covers a gap that
// used to exist: AtomicAggregate = true was set unconditionally, without
// checking the attribute actually carried zero bytes (RFC 4271 §5.1.6). A
// malformed, non-zero-length instance was silently accepted with the extra
// bytes discarded and no flag at all -- violating "every accommodation sets
// a flag". Fixed to validate length and route a malformed instance through
// the RFC 7606 discard ladder like any other non-critical attribute.
func TestParseUpdateAtomicAggregateMalformedLengthDiscarded(t *testing.T) {
	attr := buildAttr(0x40, attrAtomicAgg, []byte{0xAA}) // must be 0 bytes
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Attrs.AtomicAggregate {
		t.Fatalf("malformed ATOMIC_AGGREGATE must not be accepted as true: %+v", u.Attrs)
	}
	if u.TreatAsWithdraw {
		t.Fatalf("ATOMIC_AGGREGATE is not one of the treat-as-withdraw types: %+v", u)
	}
	if len(u.Attrs.Unknown) != 1 || u.Attrs.Unknown[0].Type != attrAtomicAgg {
		t.Fatalf("want the malformed attribute preserved verbatim in Unknown, got %+v", u.Attrs.Unknown)
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_ATTR_DISCARDED_7606
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_ATTR_DISCARDED_7606, got %v", u.Flags)
	}
}

func TestParseUpdateAggregatorFourByteAndTwoByteForms(t *testing.T) {
	v4 := append(binary.BigEndian.AppendUint32(nil, 4200000001), 10, 0, 0, 9)
	attr := buildAttr(0xC0, attrAggregator, v4)
	body := buildBody(nil, attr, nil)
	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Attrs.Aggregator == nil || u.Attrs.Aggregator.Asn != 4200000001 || u.Attrs.Aggregator.Ip != "10.0.0.9" {
		t.Fatalf("4-byte aggregator=%+v", u.Attrs.Aggregator)
	}

	v2 := append(binary.BigEndian.AppendUint16(nil, 65001), 10, 0, 0, 9)
	attr2 := buildAttr(0xC0, attrAggregator, v2)
	body2 := buildBody(nil, attr2, nil)
	u2, err := ParseUpdate(body2, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u2.Attrs.Aggregator == nil || u2.Attrs.Aggregator.Asn != 65001 || u2.Attrs.Aggregator.Ip != "10.0.0.9" {
		t.Fatalf("2-byte aggregator=%+v", u2.Attrs.Aggregator)
	}
}

func TestParseUpdateAggregatorMalformedLengthDiscarded(t *testing.T) {
	attr := buildAttr(0xC0, attrAggregator, []byte{1, 2, 3, 4, 5}) // neither 6 nor 8 bytes
	body := buildBody(nil, attr, nil)
	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.TreatAsWithdraw || u.Attrs.Aggregator != nil {
		t.Fatalf("u=%+v", u)
	}
	if len(u.Attrs.Unknown) != 1 {
		t.Fatalf("want malformed AGGREGATOR preserved in Unknown, got %+v", u.Attrs.Unknown)
	}
}

// --- Extended-length attributes (flags bit 0x10) ---

// TestParseUpdateExtendedLengthAttribute covers the RFC 4271 §4.3 Extended
// Length flag with a realistic reason to use it: 100 communities (400
// bytes), which cannot fit a 1-byte length field.
func TestParseUpdateExtendedLengthAttribute(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	var cv []byte
	want := make([]uint32, 100)
	for i := range 100 {
		c := uint32(65001)<<16 | uint32(i)
		want[i] = c
		cv = binary.BigEndian.AppendUint32(cv, c)
	}
	body := spliceExtAttr(t, msg[19:], 0xC0, attrCommunity, cv)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(u.Attrs.Communities) != 100 {
		t.Fatalf("got %d communities, want 100", len(u.Attrs.Communities))
	}
	for i, c := range want {
		if u.Attrs.Communities[i] != c {
			t.Fatalf("community %d = %d, want %d", i, u.Attrs.Communities[i], c)
		}
	}
}

func TestParseUpdateExtendedLengthHeaderTruncated(t *testing.T) {
	// Extended-length flag set, but only 1 byte follows flags+type instead
	// of the 2 needed for the length field: an extended length flag set
	// with a 1-byte remainder.
	attrs := []byte{0x50, attrCommunity, 0xFF}
	body := buildBody(nil, attrs, nil)
	_, err := ParseUpdate(body, capsV4(false))
	if !errors.Is(err, ErrAttrTruncated) {
		t.Fatalf("want ErrAttrTruncated, got %v", err)
	}
}

// --- Sentinel-error identity: structural truncation at every nesting level ---

func TestParseUpdateBodyTooShortSentinel(t *testing.T) {
	_, err := ParseUpdate([]byte{0, 0, 0}, capsV4(false))
	if !errors.Is(err, ErrUpdateTruncated) {
		t.Fatalf("want ErrUpdateTruncated, got %v", err)
	}
}

func TestParseUpdateWithdrawnLengthOverrunSentinel(t *testing.T) {
	body := []byte{0, 10, 0, 0} // claims 10 bytes of withdrawn routes, none present
	_, err := ParseUpdate(body, capsV4(false))
	if !errors.Is(err, ErrUpdateTruncated) {
		t.Fatalf("want ErrUpdateTruncated, got %v", err)
	}
}

func TestParseUpdateAttrLengthOverrunSentinel(t *testing.T) {
	body := []byte{0, 0, 0, 50} // wl=0, al=50, no attribute bytes present
	_, err := ParseUpdate(body, capsV4(false))
	if !errors.Is(err, ErrUpdateTruncated) {
		t.Fatalf("want ErrUpdateTruncated, got %v", err)
	}
}

func TestParseUpdateAttrHeaderTruncatedSentinel(t *testing.T) {
	attrs := []byte{0x40, attrOrigin} // 2 bytes; needs a 3rd (length) at minimum
	body := buildBody(nil, attrs, nil)
	_, err := ParseUpdate(body, capsV4(false))
	if !errors.Is(err, ErrAttrTruncated) {
		t.Fatalf("want ErrAttrTruncated, got %v", err)
	}
}

func TestParseUpdateAttrValueOverrunSentinel(t *testing.T) {
	attrs := []byte{0x40, attrOrigin, 5, 0} // declares 5 bytes of value, only 1 present
	body := buildBody(nil, attrs, nil)
	_, err := ParseUpdate(body, capsV4(false))
	if !errors.Is(err, ErrAttrTruncated) {
		t.Fatalf("want ErrAttrTruncated, got %v", err)
	}
}

func TestParseUpdateWithdrawnPrefixLenInvalidSentinel(t *testing.T) {
	// A withdrawn-routes prefix length of 33 exceeds the IPv4 maximum (32).
	withdrawn := []byte{33, 1, 2, 3, 4}
	body := buildBody(withdrawn, nil, nil)
	_, err := ParseUpdate(body, capsV4(false))
	if !errors.Is(err, ErrPrefixLenInvalid) {
		t.Fatalf("want ErrPrefixLenInvalid, got %v", err)
	}
}

func TestParseUpdateNlriAddPathTruncatedSentinel(t *testing.T) {
	// add-path negotiated; NLRI has 4 bytes of path-id but nothing left for
	// even the 1-byte prefix length.
	nlri := []byte{0, 0, 0, 9}
	attrs := buildAttr(0x40, attrOrigin, []byte{0})
	attrs = append(attrs, buildAttr(0x40, attrNextHop, []byte{10, 0, 0, 9})...)
	body := buildBody(nil, attrs, nlri)
	_, err := ParseUpdate(body, capsV4(true))
	if !errors.Is(err, ErrPrefixTruncated) {
		t.Fatalf("want ErrPrefixTruncated, got %v", err)
	}
}

// --- prefix.go: direct ParsePrefixesV4 sentinel/behavior tests ---

func TestParsePrefixesV4LenInvalidSentinel(t *testing.T) {
	_, err := ParsePrefixesV4([]byte{33, 1, 2, 3, 4}, false)
	if !errors.Is(err, ErrPrefixLenInvalid) {
		t.Fatalf("want ErrPrefixLenInvalid, got %v", err)
	}
}

func TestParsePrefixesV4TruncatedSentinel(t *testing.T) {
	_, err := ParsePrefixesV4([]byte{24, 1, 2}, false) // /24 needs 3 addr bytes, only 2 present
	if !errors.Is(err, ErrPrefixTruncated) {
		t.Fatalf("want ErrPrefixTruncated, got %v", err)
	}
}

func TestParsePrefixesV4AddPathTruncatedSentinel(t *testing.T) {
	_, err := ParsePrefixesV4([]byte{0, 0, 0}, true) // 3 bytes; add-path entry needs >=5
	if !errors.Is(err, ErrPrefixTruncated) {
		t.Fatalf("want ErrPrefixTruncated, got %v", err)
	}
}

func TestParsePrefixesV4ZeroLengthDefaultRoute(t *testing.T) {
	ps, err := ParsePrefixesV4([]byte{0}, false)
	if err != nil || len(ps) != 1 || ps[0].Prefix != mustPfx("0.0.0.0/0") {
		t.Fatalf("ps=%+v err=%v", ps, err)
	}
}

// TestParsePrefixesV4HostBitsNormalized covers a real router behavior this
// package tolerates rather than rejects: a declared prefix length that
// doesn't fall on a byte boundary can leave non-zero "host bits" in the
// partial byte. RFC 4271 doesn't forbid receiving these; ParsePrefixesV4
// normalizes via Masked() instead of erroring.
func TestParsePrefixesV4HostBitsNormalized(t *testing.T) {
	b := []byte{25, 198, 51, 100, 0xFF} // /25: only the top bit of the last byte is significant
	ps, err := ParsePrefixesV4(b, false)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := mustPfx("198.51.100.128/25")
	if len(ps) != 1 || ps[0].Prefix != want {
		t.Fatalf("got %+v, want %v", ps, want)
	}
}

func TestParsePrefixesV4AddPathRoundTrip(t *testing.T) {
	ps := []Prefix{
		{Prefix: mustPfx("10.0.0.0/8"), PathID: 1},
		{Prefix: mustPfx("172.16.0.0/12"), PathID: 2},
	}
	b := AppendPrefixesV4(nil, ps, true)
	got, err := ParsePrefixesV4(b, true)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 2 || got[0] != ps[0] || got[1] != ps[1] {
		t.Fatalf("got=%+v want=%+v", got, ps)
	}
}

// --- End-of-RIB edge cases ---

func TestParseUpdateNotEndOfRIBWhenAttrsPresentButNoNLRI(t *testing.T) {
	// wl=0, al>0 (a real attribute, not MP_UNREACH), no NLRI: must not be
	// mistaken for End-of-RIB.
	attr := buildAttr(0x40, attrOrigin, []byte{0})
	body := buildBody(nil, attr, nil)
	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.EndOfRIB {
		t.Fatalf("a lone ORIGIN attribute must not be treated as End-of-RIB: %+v", u)
	}
}

// --- AppendUpdate builder: attribute-value length panic, wire
// attribute flags ---

// TestAppendUpdateAttrValueOver255RoundTrips guards the boundary where an
// attribute value stops fitting the 1-byte length field. Communities are 4
// bytes each, so 63 (252 bytes) is the largest count below the 255-byte
// boundary and 64 (256 bytes) is the smallest above it; both sides are
// exercised here.
//
// In the non-extended form, byte(256) == 0 would truncate the length to
// zero and leave 256 value bytes to be misread as further attribute
// headers. The builder emits the RFC 4271 §4.3 Extended Length form for
// such a value, so it is encoded correctly.
//
// What this tests is that a value that does not fit the short form is never
// silently truncated. It is checked by round-tripping the value through
// ParseUpdate rather than by expecting a panic -- a stronger check, since it
// proves the bytes are right rather than merely that the builder noticed.
func TestAppendUpdateAttrValueOver255RoundTrips(t *testing.T) {
	build := func(n int) BuildUpdate {
		cv := make([]uint32, n)
		for i := range cv {
			cv[i] = 65001<<16 | uint32(i)
		}
		return BuildUpdate{
			Announced:   []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
			NextHop:     netip.MustParseAddr("10.0.0.9"),
			Communities: cv,
		}
	}
	caps := Caps{FourByteAS: true, AddPathRecv: map[Family]bool{}}

	for _, n := range []int{63, 64, 200} {
		raw := AppendUpdate(nil, build(n))
		u, err := ParseUpdate(raw[bgpHeaderLen:], caps)
		if err != nil {
			t.Fatalf("%d communities (%d bytes): ParseUpdate: %v", n, n*4, err)
		}
		got := u.Attrs.GetCommunities()
		if len(got) != n {
			t.Fatalf("%d communities (%d bytes): round-tripped %d -- a truncated length "+
				"field would land here", n, n*4, len(got))
		}
		if got[n-1] != 65001<<16|uint32(n-1) {
			t.Errorf("%d communities: last value = %d, want %d", n, got[n-1], 65001<<16|uint32(n-1))
		}
	}
}

// TestAttrFlagsPerType pins down the per-type wire-flags mapping
// directly, including types AppendUpdate does not currently emit (MED,
// MP_REACH/MP_UNREACH, Extended/Large Communities) so the mapping is
// verified even though there's no BuildUpdate field to exercise it through
// the builder today. MP_REACH_NLRI/MP_UNREACH_NLRI are RFC 4760 §3/§4
// "optional non-transitive" attributes (0x80) -- verified directly against
// the RFC text, not 0xC0.
func TestAttrFlagsPerType(t *testing.T) {
	cases := []struct {
		typ  uint8
		want uint8
	}{
		{attrOrigin, 0x40},
		{attrASPath, 0x40},
		{attrNextHop, 0x40},
		{attrMED, 0x80},
		{attrLocalPref, 0x40},
		{attrAtomicAgg, 0x40},
		{attrAggregator, 0xC0},
		{attrCommunity, 0xC0},
		{attrMPReach, 0x80},
		{attrMPUnreach, 0x80},
		{attrExtComm, 0xC0},
		{attrLargeComm, 0xC0},
	}
	for _, c := range cases {
		if got := attrFlags(c.typ); got != c.want {
			t.Fatalf("attrFlags(%d) = 0x%02X, want 0x%02X", c.typ, got, c.want)
		}
	}
}

// TestAppendUpdateAttributeFlags asserts the actual wire bytes AppendUpdate
// emits for the attributes it builds today: ORIGIN/AS_PATH/NEXT_HOP
// well-known (0x40) and COMMUNITIES optional-transitive (0xC0) -- a
// hardcoded 0x40 would have mislabeled COMMUNITIES here.
func TestAppendUpdateAttributeFlags(t *testing.T) {
	msg := AppendUpdate(nil, BuildUpdate{
		Announced:   []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		ASPath:      []uint32{65001},
		FourByteAS:  true,
		NextHop:     netip.MustParseAddr("10.0.0.9"),
		Communities: []uint32{65001<<16 | 100},
	})
	body := msg[19:]
	wl := int(body[0])<<8 | int(body[1])
	al := int(body[2+wl])<<8 | int(body[2+wl+1])
	attrs := body[2+wl+2 : 2+wl+2+al]

	want := map[uint8]uint8{
		attrOrigin:    0x40,
		attrASPath:    0x40,
		attrNextHop:   0x40,
		attrCommunity: 0xC0,
	}
	got := map[uint8]uint8{}
	for len(attrs) > 0 {
		flags, typ, l := attrs[0], attrs[1], int(attrs[2])
		got[typ] = flags
		attrs = attrs[3+l:]
	}
	for typ, wantFlags := range want {
		if got[typ] != wantFlags {
			t.Fatalf("attr type %d: flags = 0x%02X, want 0x%02X", typ, got[typ], wantFlags)
		}
	}
}

// fuzzCaps builds the Caps a fuzz execution parses under, from three
// independent bools: capsV4's fixed helper always set
// MP[FamilyIPv4U]=true and FourByteAS=true, which forced `known` to always
// be true in ParseUpdate's gate (caps.AddPathRecv[FamilyIPv4U] ||
// len(caps.MP) > 0) — so parsePrefixesV4Auto's structural add-path heuristic
// path received zero executions across a ~40M-exec fuzz run, and
// FourByteAS being pinned true meant the 2-byte AS_PATH decode in
// parseASPath never ran either. knownCaps selects between an empty
// newCaps() (MP unset -> known can be false) and a populated one (MP set ->
// known is always true, matching real negotiated-capabilities sessions);
// fourByteAS toggles the AS_PATH ASN width directly; ap sets
// AddPathRecv[FamilyIPv4U] regardless (this alone also forces known=true, so
// reaching the heuristic requires the fuzzer to pick knownCaps=false *and*
// ap=false together -- both are within the explored space now).
func fuzzCaps(knownCaps, ap, fourByteAS bool) Caps {
	c := newCaps()
	c.FourByteAS = fourByteAS
	if knownCaps {
		c.MP[FamilyIPv4U] = true
	}
	if ap {
		c.AddPathRecv[FamilyIPv4U] = true
	}
	return c
}

// FuzzParseUpdate seeds with realistic UPDATEs across both address families,
// add-path on and off, extended-length attributes, RFC 7606 discard/TaW
// cases, MP End-of-RIB, and a range of truncated/malformed inputs -- well
// beyond a single seed, since this parser is the most exposed in
// the project and needs the deepest seed corpus in the package.
func FuzzParseUpdate(f *testing.F) {
	// Classic v4 announce.
	f.Add(AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})[19:], true, true, true)

	// Withdraw + classic End-of-RIB.
	f.Add(AppendUpdate(nil, BuildUpdate{Withdrawn: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}}})[19:], false, true, true)
	f.Add(AppendUpdate(nil, BuildUpdate{})[19:], false, true, true)

	// Add-path negotiated (known caps) and add-path heuristic (unknown caps,
	// ambiguous small path IDs -- knownCaps=false, ap=false is what
	// actually drives this seed into parsePrefixesV4Auto's heuristic branch;
	// under the old fixed capsV4 helper this seed never reached it).
	f.Add(AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24"), PathID: 7}},
		AddPath:   true,
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})[19:], true, true, true)
	f.Add(AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.1/32"), PathID: 9}, {Prefix: mustPfx("192.0.2.2/32"), PathID: 9}},
		AddPath:   true,
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})[19:], false, false, true)

	// Unknown attribute, 7606 discard, 7606 treat-as-withdraw.
	base := AppendUpdate(nil, BuildUpdate{
		Announced: []Prefix{{Prefix: mustPfx("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})[19:]
	f.Add(spliceAttrRaw(base, 0xC0, 99, []byte{0xDE, 0xAD}), false, true, true)
	f.Add(spliceAttrRaw(base, 0xC0, attrAggregator, []byte{1, 2, 3}), false, true, true)
	f.Add(spliceAttrRaw(base, 0x40, attrOrigin, []byte{0, 0}), false, true, true)

	// Extended-length attribute.
	var cv []byte
	for i := range 100 {
		cv = binary.BigEndian.AppendUint32(cv, uint32(65001)<<16|uint32(i))
	}
	f.Add(spliceExtAttrRaw(base, 0xC0, attrCommunity, cv), false, true, true)

	// MP_REACH: typed ipv4u, RFC 8950 extended next hop, other-family raw.
	v4nlri := AppendPrefixesV4(nil, []Prefix{{Prefix: mustPfx("203.0.113.0/24")}}, false)
	f.Add(buildBody(nil, buildAttr(0x80, attrMPReach, mpReachVal(FamilyIPv4U, []byte{10, 0, 0, 9}, v4nlri)), nil), false, true, true)
	v6nh := netip.MustParseAddr("2001:db8::1").As16()
	f.Add(buildBody(nil, buildAttr(0x80, attrMPReach, mpReachVal(FamilyIPv4U, v6nh[:], v4nlri)), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0x80, attrMPReach, mpReachVal(familyVPNv4, []byte{10, 0, 0, 9}, []byte{1, 2, 3, 4})), nil), false, true, true)
	// Regression seed: AFI 1 / SAFI 128 (VPNv4), next-hop-length 200,
	// only 8 value bytes total -- structurally malformed, but must still
	// preserve RawReach + PARSE_FLAG_UNKNOWN_FAMILY rather than committing
	// Family with neither.
	f.Add(buildBody(nil, buildAttr(0x80, attrMPReach, []byte{0x00, 0x01, 0x80, 0xC8, 0x01, 0x02, 0x03, 0x04}), nil), false, true, true)

	// MP_UNREACH: other-family End-of-RIB, other-family non-empty NLRI, and
	// the duplicate-MP_UNREACH fake-End-of-RIB regression.
	f.Add(buildBody(nil, buildAttr(0x80, attrMPUnreach, mpUnreachVal(familyVPNv4, nil)), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0x80, attrMPUnreach, mpUnreachVal(familyVPNv4, []byte{1, 2, 3})), nil), false, true, true)
	dupAttrs := buildAttr(0x80, attrMPUnreach, mpUnreachVal(familyVPNv4, []byte{1, 2, 3}))
	dupAttrs = append(dupAttrs, buildAttr(0x80, attrMPUnreach, mpUnreachVal(familyVPNv4, nil))...)
	f.Add(buildBody(nil, dupAttrs, nil), false, true, true)

	// AS_PATH confederation segment, and a 2-byte-ASN AS_PATH:
	// fourByteAS=false exercises the RFC 4271-classic 2-byte decode path in
	// parseASPath, which the fixed capsV4 helper never reached before.
	f.Add(buildBody(nil, buildAttr(0x40, attrASPath, []byte{3, 1, 0, 0, 0xFD, 0xE9}), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0x40, attrASPath, []byte{2, 1, 0xFD, 0xE9}), nil), false, true, false)

	// RFC 7606 reclassification: malformed MED/COMMUNITIES/Extended
	// Communities/Large Communities must all treat-as-withdraw, and a
	// zero-length COMMUNITIES/Extended-Communities and an undefined
	// ORIGIN value are malformed, not silently accepted.
	f.Add(buildBody(nil, buildAttr(0x80, attrMED, []byte{0, 0, 0}), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0xC0, attrCommunity, make([]byte, 6)), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0xC0, attrCommunity, nil), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0xC0, attrExtComm, make([]byte, 5)), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0xC0, attrExtComm, nil), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0xC0, attrLargeComm, make([]byte, 7)), nil), false, true, true)
	f.Add(buildBody(nil, buildAttr(0x40, attrOrigin, []byte{7}), nil), false, true, true)

	// Truncated / malformed inputs at every nesting level.
	f.Add([]byte{}, false, true, true)
	f.Add([]byte{0, 0}, false, true, true)
	f.Add([]byte{0, 10, 0, 0}, false, true, true)                                                                 // withdrawn-length overrun
	f.Add([]byte{0, 0, 0, 50}, false, true, true)                                                                 // attr-length overrun
	f.Add([]byte{0, 0, 0, 2, 0x40, attrOrigin}, false, true, true)                                                // attr header truncated
	f.Add([]byte{0, 0, 0, 3, 0x50, attrCommunity, 0xFF}, false, true, true)                                       // ext-length header truncated
	f.Add(buildBody([]byte{33, 1, 2, 3, 4}, nil, nil), false, true, true)                                         // prefix length > 32
	f.Add(buildBody(nil, nil, []byte{0, 0, 0, 9}), true, true, true)                                              // add-path NLRI truncated
	f.Add(buildBody(nil, buildAttr(0x80, attrMPReach, []byte{0, 1, 1, 200, 1, 2, 3, 4}), nil), false, true, true) // bogus next-hop length

	f.Fuzz(func(t *testing.T, body []byte, ap bool, knownCaps bool, fourByteAS bool) {
		ParseUpdate(body, fuzzCaps(knownCaps, ap, fourByteAS)) // must not panic
	})
}

// TestParseUpdateVpn4Typed pins that a vpn4 MP_REACH is decoded into typed
// prefixes and that its raw bytes are dropped on a clean parse.
//
// The next hop here is a realistic RFC 4364 §4.3.2 12-byte vpn4 next hop
// (8-byte RD, always zero, + 4-byte IPv4) -- not the bare 4-byte form no
// real vpn4 speaker sends -- and the assertions cover Prefix, Labels and
// Attrs.NextHop, not just RD. A 4-byte next hop here used to silently
// vanish: Attrs.NextHop stayed "", RawReach was still dropped, and no flag
// was raised.
func TestParseUpdateVpn4Typed(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v := []byte{0x00, 0x01, 0x80, 0x0C} // AFI 1 / SAFI 128, next-hop-length 12
	v = append(v, make([]byte, 8)...)   // 8-byte RD portion of the next hop, always zero
	v = append(v, 0x0A, 0x00, 0x00, 0x09)
	v = append(v, 0x00) // reserved
	v = append(v, nlri...)
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if u.Family != FamilyVPNv4 {
		t.Fatalf("family = %+v, want vpn4", u.Family)
	}
	if len(u.VpnAnnounced) != 1 {
		t.Fatalf("vpn announced = %+v", u.VpnAnnounced)
	}
	got := u.VpnAnnounced[0]
	if got.RD != "65000:100" || got.Prefix != mustPfx("10.0.0.0/24") ||
		len(got.Labels) != 1 || got.Labels[0] != 24001 {
		t.Fatalf("vpn announced = %+v", got)
	}
	if u.Attrs.NextHop != "10.0.0.9" {
		t.Fatalf("next hop = %q, want 10.0.0.9", u.Attrs.NextHop)
	}
	if len(u.RawReach) != 0 {
		t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawReach))
	}
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED {
			t.Fatalf("a fully clean parse (typed nlri + typed next hop) must set neither flag, got %v", u.Flags)
		}
	}
}

// TestParseUpdateVpn4RFC8950NextHop covers the RFC 8950 vpn4 next-hop form:
// 8 zero bytes followed by a 16-byte IPv6 address (24 bytes total), the
// vpn4 analog of TestParseUpdateMPReachRFC8950ExtendedNextHop.
func TestParseUpdateVpn4RFC8950NextHop(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v6 := netip.MustParseAddr("2001:db8::1").As16()
	v := []byte{0x00, 0x01, 0x80, 0x18} // next-hop-length 24
	v = append(v, make([]byte, 8)...)
	v = append(v, v6[:]...)
	v = append(v, 0x00) // reserved
	v = append(v, nlri...)
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if u.Attrs.NextHop != "2001:db8::1" {
		t.Fatalf("next hop = %q, want 2001:db8::1", u.Attrs.NextHop)
	}
	if len(u.VpnAnnounced) != 1 || u.VpnAnnounced[0].RD != "65000:100" {
		t.Fatalf("vpn announced = %+v", u.VpnAnnounced)
	}
	if len(u.RawReach) != 0 {
		t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawReach))
	}
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED || f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY {
			t.Fatalf("a fully clean parse must set neither flag, got %v", u.Flags)
		}
	}
}

// TestParseUpdateLinkLocalNextHops covers RFC 8950 §3's second next-hop form
// for each typed family: a global IPv6 address followed by an RFC 2545
// link-local one (vpn4: 48 bytes behind the 8-byte RD; lu4 and EVPN: 32).
// This is the encoding a session over an unnumbered IPv6 link uses, so it is
// the deployment RFC 8950 exists for -- not an edge case. Only the global
// address is kept, matching what the ipv4-unicast path does with its 32-byte
// form. Omitting these lengths degraded every such route to raw and raised a
// false NLRI_UNTYPED.
func TestParseUpdateLinkLocalNextHops(t *testing.T) {
	global := netip.MustParseAddr("2001:db8::1").As16()
	linkLocal := netip.MustParseAddr("fe80::1").As16()

	vpn4NLRI := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	// lu4: 48 bits total = 24 label + 24 prefix -> label 100, 192.0.2.0/24.
	lu4NLRI := []byte{0x30, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02}
	// EVPN type 3: RD(8) + EthernetTag(4) + IPLen(1) + IP(4).
	evpnNLRI := []byte{3, 17,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x00, 0x64,
		32, 0x0A, 0x00, 0x00, 0x0B,
	}

	// vpn4's link-local form is two *VPN*-IPv6 addresses, so the 8-byte zero
	// RD appears twice -- 8+16+8+16 = 48, not 8+16+16 = 40. lu4 and EVPN
	// carry bare addresses: 16+16 = 32.
	vpnNH := func() []byte {
		nh := make([]byte, 8)
		nh = append(nh, global[:]...)
		nh = append(nh, make([]byte, 8)...)
		return append(nh, linkLocal[:]...)
	}
	bareNH := func() []byte {
		return append(append([]byte{}, global[:]...), linkLocal[:]...)
	}

	for _, c := range []struct {
		name    string
		afi     uint16
		safi    byte
		nextHop func() []byte
		wantLen int
		nlri    []byte
	}{
		{"vpn4 48-byte next hop", 1, 128, vpnNH, 48, vpn4NLRI},
		{"lu4 32-byte next hop", 1, 4, bareNH, 32, lu4NLRI},
		{"evpn 32-byte next hop", 25, 70, bareNH, 32, evpnNLRI},
	} {
		t.Run(c.name, func(t *testing.T) {
			nh := c.nextHop()
			if len(nh) != c.wantLen {
				t.Fatalf("fixture builds a %d-byte next hop, want %d", len(nh), c.wantLen)
			}

			v := []byte{byte(c.afi >> 8), byte(c.afi), c.safi, byte(len(nh))}
			v = append(v, nh...)
			v = append(v, 0x00) // RFC 4760 §3 reserved
			v = append(v, c.nlri...)
			body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

			u, err := ParseUpdate(body, capsV4(false))
			if err != nil {
				t.Fatal(err)
			}
			if u.Attrs.NextHop != "2001:db8::1" {
				t.Fatalf("next hop = %q, want the global address 2001:db8::1 "+
					"(the link-local half must be dropped, not decoded over)", u.Attrs.NextHop)
			}
			if len(u.RawReach) != 0 {
				t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawReach))
			}
			for _, f := range u.Flags {
				if f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED || f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY {
					t.Fatalf("a fully clean parse must set neither flag, got %v", u.Flags)
				}
			}
		})
	}
}

// TestParseUpdateVpn4NonZeroRDInNextHopKeepsRaw pins the one place a vpn4 next
// hop is refused rather than decoded. RFC 4364 §4.3.2 fixes the next hop's
// leading Route Distinguisher at zero; a non-zero one means the sender and
// this parser disagree about the layout, so decoding an address from the
// assumed offset would report a plausible wrong next hop. Refusing keeps the
// raw bytes for reparse, which is the only record of the anomaly -- the NLRI
// still decodes, so no route is lost.
func TestParseUpdateVpn4NonZeroRDInNextHopKeepsRaw(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v := []byte{0x00, 0x01, 0x80, 0x0C}
	v = append(v, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64) // RD 65000:100, not zero
	v = append(v, 0x0A, 0x00, 0x00, 0x09)
	v = append(v, 0x00)
	v = append(v, nlri...)
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if u.Attrs.NextHop != "" {
		t.Fatalf("next hop = %q, want it left unset rather than read from an offset "+
			"the sender evidently does not agree on", u.Attrs.NextHop)
	}
	if len(u.VpnAnnounced) != 1 || u.VpnAnnounced[0].RD != "65000:100" {
		t.Fatalf("the NLRI still decodes cleanly: %+v", u.VpnAnnounced)
	}
	if len(u.RawReach) == 0 {
		t.Fatal("raw bytes must be kept: they are the only surviving record of the non-zero RD")
	}
	untyped := false
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED {
			untyped = true
		}
	}
	if !untyped {
		t.Fatalf("flags = %v, want NLRI_UNTYPED", u.Flags)
	}
}

// TestParseUpdateVpn4UnsupportedNextHopLengthKeepsRawAndTyped covers a vpn4
// next hop whose length is neither the 12-byte (RFC 4364 §4.3.2) nor the
// 24-byte (RFC 8950) form -- 4 bytes, the length a plain IPv4-unicast next
// hop would use, which vpn4 never sends (its next hop always carries the
// leading 8-byte RD). The NLRI still decodes cleanly, so it must not be
// thrown away just because the next hop couldn't be recovered: RawReach is
// kept (the only place the undecodable next-hop bytes survive), the typed
// NLRI (VpnAnnounced) is still populated, and NLRI_UNTYPED is raised.
func TestParseUpdateVpn4UnsupportedNextHopLengthKeepsRawAndTyped(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v := []byte{0x00, 0x01, 0x80, 0x04, 0x0A, 0x00, 0x00, 0x09, 0x00}
	v = append(v, nlri...)
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("an unsupported next-hop length must not fail the message: %v", err)
	}
	if len(u.VpnAnnounced) != 1 || u.VpnAnnounced[0].RD != "65000:100" {
		t.Fatalf("typed nlri must survive even when the next hop cannot be decoded, got %+v", u.VpnAnnounced)
	}
	if u.Attrs.NextHop != "" {
		t.Fatalf("an unrecognized next-hop length must not be decoded, got %q", u.Attrs.NextHop)
	}
	if string(u.RawReach) != string(v) {
		t.Fatalf("RawReach = %x, want the full attribute value %x", u.RawReach, v)
	}
	found := false
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY {
			t.Fatalf("vpn4 has a registered decoder: UNKNOWN_FAMILY must not be set, got %v", u.Flags)
		}
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
}

// TestParseUpdateLU4Typed pins the FamilyLU4 registry entry. A
// labeled-unicast (RFC 8277) MP_REACH decodes into a typed VpnPrefix with no
// Route Distinguisher, decodes its 4-byte next hop, drops its raw bytes on a
// clean parse, and must not set PARSE_FLAG_UNKNOWN_FAMILY -- lu4 is one of
// the three families natively decoded. Mutation-proved before
// this test existed: deleting the whole `case FamilyLU4:` arm in decodeNLRI
// left the full suite passing.
func TestParseUpdateLU4Typed(t *testing.T) {
	nlri := []byte{0x30, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02} // RFC 8277: label 100, prefix 192.0.2.0/24
	v := []byte{0x00, 0x01, 0x04, 0x04, 0x0A, 0x00, 0x00, 0x09, 0x00}
	v = append(v, nlri...)
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if u.Family != FamilyLU4 {
		t.Fatalf("family = %+v, want lu4", u.Family)
	}
	if len(u.VpnAnnounced) != 1 {
		t.Fatalf("lu4 announced = %+v", u.VpnAnnounced)
	}
	got := u.VpnAnnounced[0]
	if got.Prefix != mustPfx("192.0.2.0/24") || len(got.Labels) != 1 || got.Labels[0] != 100 || got.RD != "" {
		t.Fatalf("lu4 announced = %+v", got)
	}
	if u.Attrs.NextHop != "10.0.0.9" {
		t.Fatalf("next hop = %q, want 10.0.0.9", u.Attrs.NextHop)
	}
	if len(u.RawReach) != 0 {
		t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawReach))
	}
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY {
			t.Fatal("lu4 is a known, decoded family: UNKNOWN_FAMILY must not be set")
		}
	}
}

// TestParseUpdateEvpnNextHop4And16Bytes covers the EVPN next-hop
// cases: a clean type-3 EVPN route decoded together with a 4-byte IPv4 next
// hop and, separately, a 16-byte RFC 8950 IPv6 next hop.
func TestParseUpdateEvpnNextHop4And16Bytes(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01) // RD 65000:1
	val = append(val, 0x00, 0x00, 0x00, 0x64)                         // Ethernet Tag 100
	val = append(val, 32)                                             // IP length bits
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)                         // 10.0.0.11
	nlri := evpnEntry(3, val)

	t.Run("4-byte ipv4 next hop", func(t *testing.T) {
		v := []byte{0x00, 0x19, 0x46, 0x04, 0x0A, 0x00, 0x00, 0x09, 0x00}
		v = append(v, nlri...)
		body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

		u, err := ParseUpdate(body, capsV4(false))
		if err != nil {
			t.Fatal(err)
		}
		if u.Attrs.NextHop != "10.0.0.9" {
			t.Fatalf("next hop = %q, want 10.0.0.9", u.Attrs.NextHop)
		}
		if len(u.EvpnAnnounced) != 1 || u.EvpnAnnounced[0].OriginatingIP != "10.0.0.11" {
			t.Fatalf("evpn announced = %+v", u.EvpnAnnounced)
		}
		if len(u.RawReach) != 0 {
			t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawReach))
		}
	})

	t.Run("16-byte ipv6 next hop (RFC 8950)", func(t *testing.T) {
		v6 := netip.MustParseAddr("2001:db8::1").As16()
		v := []byte{0x00, 0x19, 0x46, 0x10}
		v = append(v, v6[:]...)
		v = append(v, 0x00) // reserved
		v = append(v, nlri...)
		body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

		u, err := ParseUpdate(body, capsV4(false))
		if err != nil {
			t.Fatal(err)
		}
		if u.Attrs.NextHop != "2001:db8::1" {
			t.Fatalf("next hop = %q, want 2001:db8::1", u.Attrs.NextHop)
		}
		if len(u.EvpnAnnounced) != 1 || u.EvpnAnnounced[0].OriginatingIP != "10.0.0.11" {
			t.Fatalf("evpn announced = %+v", u.EvpnAnnounced)
		}
		if len(u.RawReach) != 0 {
			t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawReach))
		}
	})
}

// TestParseUpdateMPReachMalformedNextHopLengthUnknownFamilyFlagsUnknownFamily
// pins a second branch: the identical malformed-next-hop-length
// shape as TestParseUpdateMPReachFamilyAndRawPreservedEvenWhenMalformed, but
// for a family with NO registered decoder at all, must raise
// PARSE_FLAG_UNKNOWN_FAMILY rather than PARSE_FLAG_NLRI_UNTYPED -- that
// flag's literal meaning ("no decoder exists for this family") is already
// true before the next-hop-length bound check ever runs, so the bound check
// failing first must not paper over it with the more generic NLRI_UNTYPED.
// Mutation-proved: without this test, flagging this path NLRI_UNTYPED
// instead of UNKNOWN_FAMILY leaves the suite passing -- nothing else pins
// which flag is truthful here.
func TestParseUpdateMPReachMalformedNextHopLengthUnknownFamilyFlagsUnknownFamily(t *testing.T) {
	// familyUndecoded (AFI 65534 / SAFI 119, no registered decoder),
	// next-hop-length 200, value only 8 bytes total.
	v := []byte{0xFF, 0xFE, 0x77, 0xC8, 0x01, 0x02, 0x03, 0x04}
	attr := buildAttr(0x80, attrMPReach, v)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if u.Family != familyUndecoded {
		t.Fatalf("family = %+v, want %+v", u.Family, familyUndecoded)
	}
	if len(u.RawReach) == 0 {
		t.Fatalf("want RawReach preserved for a malformed MP_REACH, got empty")
	}
	found := false
	for _, f := range u.Flags {
		if f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED {
			t.Fatalf("a family with no registered decoder must not get NLRI_UNTYPED, got %v", u.Flags)
		}
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_UNKNOWN_FAMILY, got %v", u.Flags)
	}
	if !u.TreatAsWithdraw {
		t.Fatalf("a structurally malformed MP_REACH must treat-as-withdraw regardless of family: %+v", u)
	}
}

// TestParseUpdateEvpnUntypedKeepsRaw pins the other half of the policy: an
// EVPN route type this decoder does not type keeps its bytes and flags.
func TestParseUpdateEvpnUntypedKeepsRaw(t *testing.T) {
	nlri := append([]byte{4, 8}, 1, 2, 3, 4, 5, 6, 7, 8)
	v := []byte{0x00, 0x19, 0x46, 0x04, 0x0A, 0x00, 0x00, 0x09, 0x00}
	v = append(v, nlri...)
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.EvpnAnnounced) != 1 || u.EvpnAnnounced[0].RouteType != 4 {
		t.Fatalf("evpn = %+v", u.EvpnAnnounced)
	}
	// The next hop here is a valid 4-byte EVPN next hop and must still be
	// decoded even though one of the NLRI entries was carried raw -- the
	// two are independent.
	if u.Attrs.NextHop != "10.0.0.9" {
		t.Fatalf("next hop = %q, want 10.0.0.9", u.Attrs.NextHop)
	}
	if len(u.RawReach) == 0 {
		t.Fatal("an undecoded route type must keep its raw bytes")
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
}

// TestParseUpdateUnknownFamilyStillRaw pins that a family with no decoder
// falls back to raw bytes plus UNKNOWN_FAMILY.
func TestParseUpdateUnknownFamilyStillRaw(t *testing.T) {
	v := []byte{0xFF, 0xFE, 0x77, 0x04, 0x0A, 0x00, 0x00, 0x09, 0x00, 0xDE, 0xAD}
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.RawReach) == 0 {
		t.Fatal("unknown family must keep raw bytes")
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_UNKNOWN_FAMILY, got %v", u.Flags)
	}
}

// TestParseUpdateVpn4MalformedKeepsRaw pins that a decoder failure degrades to
// raw plus a flag rather than failing the message.
func TestParseUpdateVpn4MalformedKeepsRaw(t *testing.T) {
	v := []byte{0x00, 0x01, 0x80, 0x04, 0x0A, 0x00, 0x00, 0x09, 0x00, 0xFF, 0x05}
	body := buildBody(nil, buildAttr(0x80, attrMPReach, v), nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatalf("a malformed NLRI must not fail the message: %v", err)
	}
	if len(u.RawReach) == 0 {
		t.Fatal("malformed vpn4 must keep raw bytes for reparse")
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
}

// TestParseUpdateMPUnreachVpn4Typed pins that a vpn4 MP_UNREACH must
// decode into VpnWithdrawn and drop its raw bytes on a clean parse.
// Mutation-proved before this test existed: deleting `u.VpnWithdrawn =
// append(u.VpnWithdrawn, vpn...)` (and, separately, `u.EvpnWithdrawn =
// append(u.EvpnWithdrawn, evpn...)`) in parseMPUnreach left the full suite
// passing.
func TestParseUpdateMPUnreachVpn4Typed(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	val := mpUnreachVal(FamilyVPNv4, nlri)
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if u.Family != FamilyVPNv4 {
		t.Fatalf("family = %+v, want vpn4", u.Family)
	}
	if len(u.VpnWithdrawn) != 1 {
		t.Fatalf("vpn withdrawn = %+v", u.VpnWithdrawn)
	}
	got := u.VpnWithdrawn[0]
	if got.RD != "65000:100" || got.Prefix != mustPfx("10.0.0.0/24") ||
		len(got.Labels) != 1 || got.Labels[0] != 24001 {
		t.Fatalf("vpn withdrawn = %+v", got)
	}
	if len(u.RawUnreach) != 0 {
		t.Fatalf("raw bytes must be dropped on a clean typed parse, got %d", len(u.RawUnreach))
	}
}

// TestParseUpdateMPUnreachEvpnUntypedKeepsRaw is
// TestParseUpdateEvpnUntypedKeepsRaw's MP_UNREACH counterpart, pinning
// another mutation: replacing `if untyped {` with `if false &&
// untyped {` in parseMPUnreach left the full suite passing before this test
// existed (the MP_REACH twin of that mutation does fail a test, so only the
// unreach side was unpinned).
func TestParseUpdateMPUnreachEvpnUntypedKeepsRaw(t *testing.T) {
	nlri := evpnEntry(4, []byte{1, 2, 3, 4, 5, 6, 7, 8}) // route type 4: not currently decoded
	val := mpUnreachVal(FamilyEVPN, nlri)
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.EvpnWithdrawn) != 1 || u.EvpnWithdrawn[0].RouteType != 4 {
		t.Fatalf("evpn withdrawn = %+v", u.EvpnWithdrawn)
	}
	if len(u.RawUnreach) == 0 {
		t.Fatal("an undecoded route type must keep its raw bytes")
	}
	found := false
	for _, f := range u.Flags {
		found = found || f == vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
	}
	if !found {
		t.Fatalf("want PARSE_FLAG_NLRI_UNTYPED, got %v", u.Flags)
	}
}

// TestAppendUpdateMPReachVPNRoundTrip tests MP_REACH in the builder. Without
// it the synthetic generator could produce no vpn4, vpn6, lu4, EVPN or
// BGP-LS message, and CI could exercise volume and churn on only one of the
// seven families the product decodes, and none of the tables the L3VPN
// dashboards are built on.
//
// The assertion is a round trip through this package's own parser rather than
// a byte-for-byte comparison against a hand-written expectation. That is the
// stronger property here: ParseUpdate is already proven against real vendor
// captures in the corpus, so "the parser that agrees with IOS-XR and NX-OS
// also accepts what the builder emits" is what makes generated traffic
// trustworthy. A byte-literal test would only prove the builder agrees with
// whoever wrote the literal.
func TestAppendUpdateMPReachVPNRoundTrip(t *testing.T) {
	want := VpnPrefix{
		Prefix: netip.MustParsePrefix("10.77.0.0/24"),
		RD:     "65000:101",
		Labels: []uint32{24001},
	}
	raw := AppendUpdate(nil, BuildUpdate{
		Origin: 0, ASPath: []uint32{65001}, FourByteAS: true,
		MP: &BuildMP{
			Family:    FamilyVPNv4,
			NextHop:   netip.MustParseAddr("10.0.0.9"),
			Announced: []VpnPrefix{want},
		},
	})

	caps := Caps{FourByteAS: true, MP: map[Family]bool{FamilyVPNv4: true},
		AddPathRecv: map[Family]bool{}}
	u, err := ParseUpdate(raw[bgpHeaderLen:], caps)
	if err != nil {
		t.Fatalf("ParseUpdate on builder output: %v", err)
	}
	if u.Family != FamilyVPNv4 {
		t.Fatalf("family = %v, want vpn4", u.Family)
	}
	if len(u.VpnAnnounced) != 1 {
		t.Fatalf("got %d vpn prefixes, want 1 (raw_reach=%d, flags=%v)",
			len(u.VpnAnnounced), len(u.RawReach), u.Flags)
	}
	got := u.VpnAnnounced[0]
	if got.Prefix != want.Prefix {
		t.Errorf("prefix = %s, want %s", got.Prefix, want.Prefix)
	}
	if got.RD != want.RD {
		t.Errorf("rd = %q, want %q", got.RD, want.RD)
	}
	if len(got.Labels) != 1 || got.Labels[0] != want.Labels[0] {
		t.Errorf("labels = %v, want %v", got.Labels, want.Labels)
	}
	if u.Attrs.GetNextHop() != "10.0.0.9" {
		t.Errorf("next hop = %q, want 10.0.0.9", u.Attrs.GetNextHop())
	}
}

// TestAppendUpdateMPFamiliesRoundTrip extends the vpn4 round trip to every
// family the builder now emits, including withdrawals.
//
// Withdrawals matter as much as announcements and are easy to leave out: an
// announce-only generator never exercises the sink's delete path or the
// "did this prefix go away" logic the churn dashboards are built on.
func TestAppendUpdateMPFamiliesRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		fam     Family
		nextHop string
		mp      BuildMP
		wantPfx string
		wantRD  string
	}{
		{
			name: "vpn6", fam: FamilyVPNv6, nextHop: "2001:db8::1",
			mp: BuildMP{Announced: []VpnPrefix{{
				Prefix: netip.MustParsePrefix("2001:db8:77::/64"),
				RD:     "65100:777", Labels: []uint32{24002}}}},
			wantPfx: "2001:db8:77::/64", wantRD: "65100:777",
		},
		{
			// A 4-byte-ASN RD takes the type-2 encoding, a different wire
			// layout from the 2-byte type-0 form above.
			name: "vpn4 with 4-byte-ASN RD", fam: FamilyVPNv4, nextHop: "10.0.0.9",
			mp: BuildMP{Announced: []VpnPrefix{{
				Prefix: netip.MustParsePrefix("10.90.0.0/16"),
				RD:     "4200000065:7", Labels: []uint32{24003}}}},
			wantPfx: "10.90.0.0/16", wantRD: "4200000065:7",
		},
		{
			name: "lu4 (no RD)", fam: FamilyLU4, nextHop: "10.0.0.9",
			mp: BuildMP{Announced: []VpnPrefix{{
				Prefix: netip.MustParsePrefix("10.91.1.0/24"),
				Labels: []uint32{24004}}}},
			wantPfx: "10.91.1.0/24", wantRD: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := tc.mp
			mp.Family = tc.fam
			mp.NextHop = netip.MustParseAddr(tc.nextHop)
			raw := AppendUpdate(nil, BuildUpdate{
				Origin: 0, ASPath: []uint32{65001}, FourByteAS: true, MP: &mp})

			caps := Caps{FourByteAS: true, MP: map[Family]bool{tc.fam: true},
				AddPathRecv: map[Family]bool{}}
			u, err := ParseUpdate(raw[bgpHeaderLen:], caps)
			if err != nil {
				t.Fatalf("ParseUpdate: %v", err)
			}
			if len(u.VpnAnnounced) != 1 {
				t.Fatalf("got %d vpn prefixes, want 1 (flags=%v)", len(u.VpnAnnounced), u.Flags)
			}
			got := u.VpnAnnounced[0]
			if got.Prefix.String() != tc.wantPfx {
				t.Errorf("prefix = %s, want %s", got.Prefix, tc.wantPfx)
			}
			if got.RD != tc.wantRD {
				t.Errorf("rd = %q, want %q", got.RD, tc.wantRD)
			}
			if len(got.Labels) != 1 || got.Labels[0] != mp.Announced[0].Labels[0] {
				t.Errorf("labels = %v, want %v", got.Labels, mp.Announced[0].Labels)
			}
		})
	}

	t.Run("ipv6u announce and withdraw", func(t *testing.T) {
		raw := AppendUpdate(nil, BuildUpdate{
			Origin: 0, ASPath: []uint32{65001}, FourByteAS: true,
			MP: &BuildMP{
				Family: FamilyIPv6U, NextHop: netip.MustParseAddr("2001:db8::1"),
				PlainAnnounced: []Prefix{{Prefix: netip.MustParsePrefix("2001:db8:aa::/48")}},
			}})
		caps := Caps{FourByteAS: true, MP: map[Family]bool{FamilyIPv6U: true},
			AddPathRecv: map[Family]bool{}}
		u, err := ParseUpdate(raw[bgpHeaderLen:], caps)
		if err != nil {
			t.Fatalf("ParseUpdate: %v", err)
		}
		if len(u.Announced) != 1 || u.Announced[0].Prefix.String() != "2001:db8:aa::/48" {
			t.Fatalf("announced = %v, want [2001:db8:aa::/48] (flags=%v)", u.Announced, u.Flags)
		}

		raw = AppendUpdate(nil, BuildUpdate{MP: &BuildMP{
			Family:         FamilyIPv6U,
			PlainWithdrawn: []Prefix{{Prefix: netip.MustParsePrefix("2001:db8:aa::/48")}},
		}})
		u, err = ParseUpdate(raw[bgpHeaderLen:], caps)
		if err != nil {
			t.Fatalf("ParseUpdate (withdraw): %v", err)
		}
		if len(u.Withdrawn) != 1 || u.Withdrawn[0].Prefix.String() != "2001:db8:aa::/48" {
			t.Fatalf("withdrawn = %v, want [2001:db8:aa::/48]", u.Withdrawn)
		}
	})

	t.Run("vpn4 withdraw", func(t *testing.T) {
		raw := AppendUpdate(nil, BuildUpdate{MP: &BuildMP{
			Family: FamilyVPNv4,
			Withdrawn: []VpnPrefix{{
				Prefix: netip.MustParsePrefix("10.77.0.0/24"),
				RD:     "65000:101", Labels: []uint32{0x800000 >> 4}}},
		}})
		caps := Caps{FourByteAS: true, MP: map[Family]bool{FamilyVPNv4: true},
			AddPathRecv: map[Family]bool{}}
		u, err := ParseUpdate(raw[bgpHeaderLen:], caps)
		if err != nil {
			t.Fatalf("ParseUpdate: %v", err)
		}
		if len(u.VpnWithdrawn) != 1 {
			t.Fatalf("got %d vpn withdrawals, want 1 (flags=%v)", len(u.VpnWithdrawn), u.Flags)
		}
		if got := u.VpnWithdrawn[0].Prefix.String(); got != "10.77.0.0/24" {
			t.Errorf("withdrawn prefix = %s, want 10.77.0.0/24", got)
		}
	})
}

// TestAppendUpdateExtendedLength covers attribute values over 255 bytes, which
// this builder used to panic on.
//
// That limit was tolerable while the builder emitted only IPv4 unicast, where
// an attribute rarely gets large. It is not tolerable for MP_REACH: a vpn4
// entry is about 16 bytes, so the 1-byte length field caps a message at
// roughly 16 prefixes -- and the generator exists precisely to produce high
// prefix counts. Found by running it: 25 vpn4 prefixes is a 392-byte MP_REACH
// and the builder panicked.
//
// ParseUpdate has always understood the RFC 4271 §4.3 Extended Length form;
// only the builder could not emit it.
func TestAppendUpdateExtendedLength(t *testing.T) {
	var vpn []VpnPrefix
	for i := range 40 {
		vpn = append(vpn, VpnPrefix{
			Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 90, byte(i), 0}), 24),
			RD:     "65000:101", Labels: []uint32{uint32(24000 + i)},
		})
	}
	raw := AppendUpdate(nil, BuildUpdate{
		Origin: 0, ASPath: []uint32{65001}, FourByteAS: true,
		MP: &BuildMP{Family: FamilyVPNv4,
			NextHop: netip.MustParseAddr("10.0.0.9"), Announced: vpn},
	})

	caps := Caps{FourByteAS: true, MP: map[Family]bool{FamilyVPNv4: true},
		AddPathRecv: map[Family]bool{}}
	u, err := ParseUpdate(raw[bgpHeaderLen:], caps)
	if err != nil {
		t.Fatalf("ParseUpdate: %v", err)
	}
	if len(u.VpnAnnounced) != len(vpn) {
		t.Fatalf("round-tripped %d vpn prefixes, want %d (flags=%v)",
			len(u.VpnAnnounced), len(vpn), u.Flags)
	}
	if got := u.VpnAnnounced[39].Prefix.String(); got != "10.90.39.0/24" {
		t.Errorf("last prefix = %s, want 10.90.39.0/24", got)
	}
}

// TestParsePrefixesV4AutoRecoversPlainNLRIWhenCapsSayAddPath pins the
// behavior a real FRR capture forced: a sender can negotiate ADD-PATH and
// then not use it.
//
// frr/frr-10.3-pair-eor-withdraw.bmpcap carries the withdrawal below --
// `18 0a 0a 01`, four bytes: prefix length 0x18 (24) then 10.10.1.0 -- on a
// session whose Peer Up OPENs both advertise
// `ADDPATH afi=1 safi=1 sendrecv=3`. RFC 7911 3 says NLRI in that direction
// carries a 4-byte Path Identifier and withdrawn routes are NLRI, so by the
// capability this is malformed. It is also, unmistakably, a withdrawal of
// 10.10.1.0/24.
//
// Before this, ParsePrefixesV4(b, true) needed five bytes per entry, got
// four, returned ErrPrefixTruncated, and the caps-known branch propagated it
// -- so the whole UPDATE contributed nothing and four real withdrawals were
// dropped in silence. Recovering the prefix is strictly better than
// discarding it, and the ADDPATH_HEURISTIC flag is what tells a consumer the
// interpretation came from structure rather than from the capability.
func TestParsePrefixesV4AutoRecoversPlainNLRIWhenCapsSayAddPath(t *testing.T) {
	// The exact bytes off the wire.
	b := []byte{0x18, 0x0a, 0x0a, 0x01}

	ps, usedAddPath, heuristic, err := parsePrefixesV4Auto(b, true, true)
	if err != nil {
		t.Fatalf("want the withdrawal recovered, got err=%v", err)
	}
	if len(ps) != 1 || ps[0].Prefix.String() != "10.10.1.0/24" {
		t.Fatalf("want one 10.10.1.0/24, got %+v", ps)
	}
	if usedAddPath {
		t.Fatalf("want the plain interpretation, got usedAddPath=true")
	}
	if !heuristic {
		t.Fatalf("recovering by structure is a guess and must be flagged as one")
	}
}

// TestParsePrefixesV4AutoKeepsAddPathWhenCapsAgreeWithWire is the guard on
// the fallback above: when the capability says add-path AND the bytes really
// are add-path, nothing changes -- the Path ID is still decoded and no
// heuristic flag is raised. Without this, a fallback that silently preferred
// the plain reading would strip every Path ID in the corpus.
func TestParsePrefixesV4AutoKeepsAddPathWhenCapsAgreeWithWire(t *testing.T) {
	b := AppendPrefixesV4(nil, []Prefix{{Prefix: mustPfx("10.10.1.0/24"), PathID: 7}}, true)

	ps, usedAddPath, heuristic, err := parsePrefixesV4Auto(b, true, true)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(ps) != 1 || ps[0].PathID != 7 {
		t.Fatalf("want PathID 7 preserved, got %+v", ps)
	}
	if !usedAddPath {
		t.Fatalf("want usedAddPath=true")
	}
	if heuristic {
		t.Fatalf("caps and wire agree, so this is a fact and not a guess")
	}
}
