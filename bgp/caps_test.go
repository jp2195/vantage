package bgp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// TestParseOpenAndMerge, TestParseOpenNoAddPath, and TestParseOpenTruncated
// cover ParseOpen and Merge: a full round trip, a message with no
// add-path capability, and a truncated message.

func TestParseOpenAndMerge(t *testing.T) {
	// Router's sent OPEN: MP ipv4u, 4-byte AS, add-path recv (1).
	sentCaps := Caps{
		FourByteAS:  true,
		MP:          map[Family]bool{FamilyIPv4U: true},
		AddPathRecv: map[Family]bool{FamilyIPv4U: true},
	}
	sent := AppendOpen(nil, 65001, 180, "10.0.0.1", sentCaps)
	recv := AppendOpen(nil, 65002, 180, "10.0.0.9", sentCaps) // builder emits send+recv(3)

	sp, err := ParseOpen(sent)
	if err != nil || !sp.FourByteAS || !sp.MP[FamilyIPv4U] || !sp.AddPathRecv[FamilyIPv4U] {
		t.Fatalf("sp=%+v err=%v", sp, err)
	}
	rp, err := ParseOpen(recv)
	if err != nil {
		t.Fatal(err)
	}
	m := Merge(sp, rp)
	if !m.FourByteAS || !m.AddPathRecv[FamilyIPv4U] || !m.MP[FamilyIPv4U] {
		t.Fatalf("m=%+v", m)
	}
}

func TestParseOpenNoAddPath(t *testing.T) {
	c := Caps{MP: map[Family]bool{FamilyIPv4U: true}}
	p, err := ParseOpen(AppendOpen(nil, 65001, 180, "10.0.0.1", c))
	if err != nil || p.FourByteAS || p.AddPathRecv[FamilyIPv4U] {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestParseOpenTruncated(t *testing.T) {
	c := Caps{MP: map[Family]bool{FamilyIPv4U: true}}
	b := AppendOpen(nil, 65001, 180, "10.0.0.1", c)
	if _, err := ParseOpen(b[:20]); err == nil {
		t.Fatal("want error")
	}
}

// --- ADD-PATH direction, and edge cases ---
//
// AppendOpen always emits ADD-PATH capabilities with both the send and
// receive bits set (value 3, see AppendOpen's doc comment), so none of
// the tests above can distinguish a parser that correctly reads
// bit 0x1 (receive) and bit 0x2 (send) separately from one that only reads
// bit 0x1 and reuses it for both directions. Real routers commonly
// advertise only one of the two bits (e.g. a stub edge that advertises
// receive-only, or a route reflector that advertises send-only toward a
// client). TestMergeAddPathAsymmetric below hand-builds such a session and
// would fail if Merge's negotiation collapsed to a single bit.

// buildRawOpen builds a full OPEN message by hand, bypassing AppendOpen, so
// callers can construct wire forms AppendOpen cannot produce (asymmetric
// ADD-PATH bits, unknown capabilities, multiple optional parameters,
// deliberately-wrong lengths). params must fit a 1-byte opt-param-len
// (<=255); callers that need that checked against a *testing.T should use
// rawOpen instead.
func buildRawOpen(params []byte) []byte {
	body := []byte{4} // version
	body = binary.BigEndian.AppendUint16(body, 65001)
	body = binary.BigEndian.AppendUint16(body, 180)
	body = append(body, 10, 0, 0, 1) // bgp id 10.0.0.1
	body = append(body, byte(len(params)))
	body = append(body, params...)
	msg := make([]byte, 0, bgpHeaderLen+len(body))
	for range 16 {
		msg = append(msg, 0xFF)
	}
	msg = binary.BigEndian.AppendUint16(msg, uint16(bgpHeaderLen+len(body)))
	msg = append(msg, msgOpen)
	msg = append(msg, body...)
	return msg
}

// rawOpen is buildRawOpen with a test-friendly precondition check.
func rawOpen(t *testing.T, params []byte) []byte {
	t.Helper()
	if len(params) > 255 {
		t.Fatalf("rawOpen: params too long for a 1-byte opt-param-len: %d", len(params))
	}
	return buildRawOpen(params)
}

// capBytes builds one capability TLV (code, len, value...).
func capBytes(code byte, value ...byte) []byte {
	return append([]byte{code, byte(len(value))}, value...)
}

// paramBytes wraps capability bytes in a type-2 (Capabilities) optional
// parameter.
func paramBytes(caps ...byte) []byte {
	return append([]byte{2, byte(len(caps))}, caps...)
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

func mpCapBytes(f Family) []byte {
	b := []byte{}
	b = binary.BigEndian.AppendUint16(b, f.AFI)
	b = append(b, 0, f.SAFI)
	return capBytes(capMP, b...)
}

func addPathCapBytes(f Family, sendRecv byte) []byte {
	b := []byte{}
	b = binary.BigEndian.AppendUint16(b, f.AFI)
	b = append(b, f.SAFI, sendRecv)
	return capBytes(capAddPath, b...)
}

func as4CapBytes(asn uint32) []byte {
	b := []byte{}
	b = binary.BigEndian.AppendUint32(b, asn)
	return capBytes(cap4ByteAS, b...)
}

// TestMergeAddPathAsymmetric proves Merge implements the RFC 7911 §3
// directional rule exactly, not an approximation that happens to agree
// with it whenever both OPENs set the same bits. The router's sent OPEN
// advertises ADD-PATH receive-only (value 1) for ipv4 unicast; the peer's
// OPEN advertises send-only (value 2). Per RFC 7911 that is a complete,
// valid negotiation for the peer-to-router direction (router can receive,
// peer will send) even though neither side ever set both bits.
func TestMergeAddPathAsymmetric(t *testing.T) {
	sentMsg := rawOpen(t, paramBytes(addPathCapBytes(FamilyIPv4U, addPathRecvBit)...))
	recvMsg := rawOpen(t, paramBytes(addPathCapBytes(FamilyIPv4U, addPathSendBit)...))

	sp, err := ParseOpen(sentMsg)
	if err != nil {
		t.Fatalf("parse sent: %v", err)
	}
	rp, err := ParseOpen(recvMsg)
	if err != nil {
		t.Fatalf("parse recv: %v", err)
	}
	m := Merge(sp, rp)
	if !m.AddPathRecv[FamilyIPv4U] {
		t.Fatalf("want AddPathRecv[FamilyIPv4U]=true for receive+send split across the two OPENs, got %+v", m)
	}
}

// TestMergeDirectionsDisagree pins the property that makes Merge's argument
// order load-bearing rather than cosmetic, and that RFC 8671 adj-RIB-out
// monitoring depends on: with an asymmetric ADD-PATH negotiation, the two
// directions of travel have different answers, so the same pair of OPENs
// must be merged transposed depending on which RIB is being monitored.
//
// The config is the common route-reflector one: the router advertises
// send-only (value 2) — it will send multiple paths to its client — and the
// client advertises receive-only (value 1). Nothing the client sends the
// router carries a Path Identifier; everything the router sends the client
// does. A caller that merged (router, peer) for an adj-RIB-out feed would
// read every NLRI in it four bytes short.
func TestMergeDirectionsDisagree(t *testing.T) {
	routerMsg := rawOpen(t, paramBytes(addPathCapBytes(FamilyIPv4U, addPathSendBit)...))
	peerMsg := rawOpen(t, paramBytes(addPathCapBytes(FamilyIPv4U, addPathRecvBit)...))

	router, err := ParseOpen(routerMsg)
	if err != nil {
		t.Fatalf("parse router open: %v", err)
	}
	peer, err := ParseOpen(peerMsg)
	if err != nil {
		t.Fatalf("parse peer open: %v", err)
	}

	// adj-RIB-in: the peer sends, the router receives.
	if in := Merge(router, peer); in.AddPathRecv[FamilyIPv4U] {
		t.Errorf("peer-to-router direction: AddPathRecv[ipv4u]=true, but the peer never advertised the send bit: %+v", in)
	}
	// RFC 8671 adj-RIB-out: the router sends, the peer receives.
	out := Merge(peer, router)
	if !out.AddPathRecv[FamilyIPv4U] {
		t.Errorf("router-to-peer direction: AddPathRecv[ipv4u]=false, but the peer advertised receive and the router send: %+v", out)
	}
	// The symmetric terms must not move when the arguments transpose, or the
	// swap would be trading one bug for another.
	if !out.MP[FamilyIPv4U] || !Merge(router, peer).MP[FamilyIPv4U] {
		t.Errorf("MP is a symmetric intersection and must survive the transposition: in=%+v out=%+v", Merge(router, peer).MP, out.MP)
	}
}

// TestMergeAddPathBothReceiveOnly is the mirror case: both OPENs advertise
// only the receive bit. Neither end ever said it would send multiple
// paths, so the peer-to-router direction must NOT be treated as add-path.
// A parser that conflated the two bits (reading only bit 0x1 on both
// sides) would incorrectly report this as negotiated.
func TestMergeAddPathBothReceiveOnly(t *testing.T) {
	sentMsg := rawOpen(t, paramBytes(addPathCapBytes(FamilyIPv4U, addPathRecvBit)...))
	recvMsg := rawOpen(t, paramBytes(addPathCapBytes(FamilyIPv4U, addPathRecvBit)...))

	sp, err := ParseOpen(sentMsg)
	if err != nil {
		t.Fatalf("parse sent: %v", err)
	}
	rp, err := ParseOpen(recvMsg)
	if err != nil {
		t.Fatalf("parse recv: %v", err)
	}
	m := Merge(sp, rp)
	if m.AddPathRecv[FamilyIPv4U] {
		t.Fatalf("want AddPathRecv[FamilyIPv4U]=false when neither OPEN advertised a send bit, got %+v", m)
	}
}

// TestParseOpenUnknownCapabilitySkipped mirrors a real router's OPEN: a
// Route Refresh capability (code 2, zero-length — extremely common, sent
// by nearly every real BGP implementation) sits between two recognized
// capabilities inside one Capabilities optional parameter. It must be
// skipped without error and without disrupting the surrounding
// capabilities, and its zero length must not stall the loop.
func TestParseOpenUnknownCapabilitySkipped(t *testing.T) {
	caps := append([]byte{}, mpCapBytes(FamilyIPv4U)...)
	caps = append(caps, capBytes(2)...) // Route Refresh, RFC 2918, zero-length
	caps = append(caps, as4CapBytes(65001)...)
	msg := rawOpen(t, paramBytes(caps...))

	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !c.MP[FamilyIPv4U] || !c.FourByteAS {
		t.Fatalf("c=%+v", c)
	}
}

// TestParseOpenMultipleOptionalParameters mirrors implementations (Juniper,
// among others) that place each capability in its own type-2 optional
// parameter rather than packing them into one.
func TestParseOpenMultipleOptionalParameters(t *testing.T) {
	params := append([]byte{}, paramBytes(mpCapBytes(FamilyIPv4U)...)...)
	params = append(params, paramBytes(as4CapBytes(65001)...)...)
	params = append(params, paramBytes(addPathCapBytes(FamilyIPv4U, addPathRecvBit|addPathSendBit)...)...)
	msg := rawOpen(t, params)

	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !c.MP[FamilyIPv4U] || !c.FourByteAS || !c.AddPathRecv[FamilyIPv4U] {
		t.Fatalf("c=%+v", c)
	}
}

// TestParseOpenNoOptionalParameters covers an OPEN with opt-param-len=0 —
// a legal, if old-fashioned, message with no capabilities at all.
//
// Per RFC 4760, no MP capability at all means implicit IPv4 unicast, so
// c.MP must contain FamilyIPv4U, not be empty. FourByteAS/AddPathRecv are
// unaffected and still must be empty.
func TestParseOpenNoOptionalParameters(t *testing.T) {
	msg := rawOpen(t, nil)
	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if c.FourByteAS || len(c.MP) != 1 || !c.MP[FamilyIPv4U] || len(c.AddPathRecv) != 0 {
		t.Fatalf("c=%+v", c)
	}
}

// TestParseOpenDuplicateCapabilityUnioned documents the chosen behavior for
// a capability advertised more than once for the same family: bits are
// unioned across occurrences rather than the last one winning or an error
// being raised.
func TestParseOpenDuplicateCapabilityUnioned(t *testing.T) {
	caps := append([]byte{}, addPathCapBytes(FamilyIPv4U, addPathRecvBit)...)
	caps = append(caps, addPathCapBytes(FamilyIPv4U, addPathSendBit)...)
	msg := rawOpen(t, paramBytes(caps...))

	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !c.AddPathRecv[FamilyIPv4U] || !c.addPathSend[FamilyIPv4U] {
		t.Fatalf("c=%+v", c)
	}
}

// TestParseOpenNotOpen asserts sentinel-error identity for a well-formed
// BGP message that simply isn't an OPEN (type byte 2 = UPDATE).
func TestParseOpenNotOpen(t *testing.T) {
	msg := rawOpen(t, nil)
	msg[18] = 2 // UPDATE
	if _, err := ParseOpen(msg); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("want ErrNotOpen, got %v", err)
	}
}

// TestParseOpenTooShortSentinel asserts sentinel-error identity for the
// overall-message-too-short case (distinct from TestParseOpenTruncated,
// which only checks err != nil).
func TestParseOpenTooShortSentinel(t *testing.T) {
	msg := rawOpen(t, nil)
	if _, err := ParseOpen(msg[:28]); !errors.Is(err, ErrOpenTruncated) {
		t.Fatalf("want ErrOpenTruncated, got %v", err)
	}
}

// TestParseOpenOptParamLenExceedsBuffer covers the OPEN's own
// opt-param-len byte declaring more bytes than the message actually
// contains. The byte at bgpHeaderLen+9 is body[9], the opt-param-len
// field (body[0:9] is version+my-AS+hold-time+bgp-id).
func TestParseOpenOptParamLenExceedsBuffer(t *testing.T) {
	msg := rawOpen(t, paramBytes(mpCapBytes(FamilyIPv4U)...))
	msg[bgpHeaderLen+9] = 100 // claims 100 bytes of params; far fewer are present
	if _, err := ParseOpen(msg); !errors.Is(err, ErrOpenTruncated) {
		t.Fatalf("want ErrOpenTruncated, got %v", err)
	}
}

// TestParseOpenParamLenExceedsBuffer covers a single optional parameter's
// own declared length running past the (correctly-bounded) params region —
// distinct from the OPEN-level opt-param-len check above.
func TestParseOpenParamLenExceedsBuffer(t *testing.T) {
	params := []byte{2, 5, 0xAA, 0xBB} // declares 5 bytes of value, only 2 present
	msg := rawOpen(t, params)
	if _, err := ParseOpen(msg); !errors.Is(err, ErrOptParamTruncated) {
		t.Fatalf("want ErrOptParamTruncated, got %v", err)
	}
}

// TestParseOpenCapLenExceedsBuffer covers a capability's own declared
// length running past the (correctly-bounded) parameter value.
func TestParseOpenCapLenExceedsBuffer(t *testing.T) {
	// code=1 (MP), declared len=10, but only 2 bytes of value follow.
	capVal := []byte{1, 10, 0xAA, 0xBB}
	msg := rawOpen(t, paramBytes(capVal...))
	if _, err := ParseOpen(msg); !errors.Is(err, ErrCapTruncated) {
		t.Fatalf("want ErrCapTruncated, got %v", err)
	}
}

// TestParseOpenAddPathTrailingPartialEntry covers a single ADD-PATH
// capability whose declared length isn't a multiple of 4 (one whole
// 4-byte AFI/SAFI/Send-Receive entry plus a 2-byte dangling remainder,
// all within the capability's own declared length). The whole entry must
// still be read and the partial remainder tolerated, not treated as an
// error — mirrors ParseTLVs-style tolerant handling of
// malformed-but-non-hostile input.
func TestParseOpenAddPathTrailingPartialEntry(t *testing.T) {
	entry := []byte{}
	entry = binary.BigEndian.AppendUint16(entry, FamilyIPv4U.AFI)
	entry = append(entry, FamilyIPv4U.SAFI, addPathRecvBit|addPathSendBit) // one complete 4-byte entry
	value := append(append([]byte{}, entry...), 0xAA, 0xBB)                // +2 dangling bytes, same capability
	capTLV := capBytes(capAddPath, value...)                               // len byte correctly says 6
	msg := rawOpen(t, paramBytes(capTLV...))

	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !c.AddPathRecv[FamilyIPv4U] {
		t.Fatalf("c=%+v", c)
	}
}

// TestParseOpenTrailingGarbage confirms bytes appended after a complete,
// well-formed OPEN message are ignored rather than rejected: ParseOpen
// derives the message's true extent from opt-param-len and nested
// lengths, not from the caller having sliced msg to exactly one message.
func TestParseOpenTrailingGarbage(t *testing.T) {
	c := Caps{MP: map[Family]bool{FamilyIPv4U: true}, FourByteAS: true}
	b := AppendOpen(nil, 65001, 180, "10.0.0.1", c)
	withGarbage := append(append([]byte{}, b...), 0xDE, 0xAD, 0xBE, 0xEF)

	got, err := ParseOpen(withGarbage)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !got.MP[FamilyIPv4U] || !got.FourByteAS {
		t.Fatalf("got=%+v", got)
	}
}

// TestAppendOpenPanicsOnOversizedCaps proves AppendOpen guards its
// precondition on the capabilities payload fitting the wire format's
// 1-byte length fields, per the package convention established by
// bmp.AppendTLV (panic rather than silently truncate/corrupt the length).
func TestAppendOpenPanicsOnOversizedCaps(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, but AppendOpen returned normally")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "exceeds") {
			t.Fatalf("want panic with 'exceeds', got: %v", r)
		}
	}()
	mp := map[Family]bool{}
	for i := range 100 { // 100 * 6 bytes/entry = 600 > maxCapsLen (252)
		mp[Family{AFI: uint16(i + 2), SAFI: 1}] = true
	}
	AppendOpen(nil, 65001, 180, "10.0.0.1", Caps{MP: mp})
}

// TestAppendOpenPanicsOnUnrepresentableASN proves AppendOpen refuses to
// silently substitute AS_TRANS for an ASN it has no capability to recover.
func TestAppendOpenPanicsOnUnrepresentableASN(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, but AppendOpen returned normally")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "AS_TRANS") {
			t.Fatalf("want panic with 'AS_TRANS', got: %v", r)
		}
	}()
	AppendOpen(nil, 70000, 180, "10.0.0.1", Caps{MP: map[Family]bool{FamilyIPv4U: true}})
}

// --- implicit IPv4-unicast, RFC 9072 guard, and related ---

// TestParseOpenImplicitIPv4UnicastWhenMPAbsent covers the primary case: an
// OPEN with no Multiprotocol Extensions capability at all (and no
// Capabilities optional parameter at all — the actual legacy wire form)
// must be read as implicitly supporting IPv4 unicast per RFC 4760 §5, not
// as supporting nothing.
func TestParseOpenImplicitIPv4UnicastWhenMPAbsent(t *testing.T) {
	msg := AppendOpen(nil, 65001, 180, "10.0.0.1", Caps{})
	if got := msg[bgpHeaderLen+9]; got != 0 {
		t.Fatalf("test setup: want opt-param-len=0 (no MP capability at all), got %d", got)
	}
	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(c.MP) != 1 || !c.MP[FamilyIPv4U] {
		t.Fatalf("want implicit IPv4 unicast when the MP capability is entirely absent, got c.MP=%+v", c.MP)
	}
}

// TestParseOpenNoImplicitIPv4UnicastWhenOtherFamilyAdvertised covers the
// negative case: a speaker that sends MP capabilities for other families
// but not IPv4 unicast is deliberately disabling it, and the implicit
// default must not override that.
func TestParseOpenNoImplicitIPv4UnicastWhenOtherFamilyAdvertised(t *testing.T) {
	c := Caps{MP: map[Family]bool{{AFI: 2, SAFI: 1}: true}} // IPv6 unicast only
	msg := AppendOpen(nil, 65001, 180, "10.0.0.1", c)

	got, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.MP[FamilyIPv4U] {
		t.Fatalf("want no implicit IPv4 unicast when MP is present for other families, got c.MP=%+v", got.MP)
	}
	if !got.MP[Family{AFI: 2, SAFI: 1}] {
		t.Fatalf("want the explicitly-advertised family preserved, got c.MP=%+v", got.MP)
	}
}

// TestParseOpenMPIncludesIPv4Unicast covers the third case: when the MP
// capability is present and explicitly includes IPv4 unicast, it must be
// recorded exactly once (map semantics), not duplicated by the default.
func TestParseOpenMPIncludesIPv4Unicast(t *testing.T) {
	c := Caps{MP: map[Family]bool{FamilyIPv4U: true, {AFI: 2, SAFI: 1}: true}}
	msg := AppendOpen(nil, 65001, 180, "10.0.0.1", c)

	got, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got.MP) != 2 || !got.MP[FamilyIPv4U] {
		t.Fatalf("want IPv4 unicast present exactly once alongside the other family, got c.MP=%+v", got.MP)
	}
}

// TestMergeLegacyOPENsWithImplicitIPv4Unicast covers a property: two
// legacy OPENs (no optional parameters at all, built via AppendOpen with
// an empty Caps) must merge to produce a Caps whose MP contains exactly
// FamilyIPv4U. Downstream logic gates on len(Merge(sent, recv).MP) > 0, so
// the implicit-IPv4-unicast default (added by ParseOpen) must persist
// through Merge, not evaporate.
func TestMergeLegacyOPENsWithImplicitIPv4Unicast(t *testing.T) {
	sentMsg := AppendOpen(nil, 65001, 180, "10.0.0.1", Caps{})
	recvMsg := AppendOpen(nil, 65002, 180, "10.0.0.9", Caps{})

	sent, err := ParseOpen(sentMsg)
	if err != nil {
		t.Fatalf("parse sent: %v", err)
	}
	recv, err := ParseOpen(recvMsg)
	if err != nil {
		t.Fatalf("parse recv: %v", err)
	}

	m := Merge(sent, recv)
	if len(m.MP) != 1 || !m.MP[FamilyIPv4U] {
		t.Fatalf("want merged Caps with exactly FamilyIPv4U, got MP=%+v", m.MP)
	}
}

// TestParseOpenEmptyType2CapabilityParameterInjectsIPv4Unicast covers a
// second case: a type-2 Capabilities parameter present but empty (plen == 0)
// must still inject FamilyIPv4U, since no capability code 1 (MP) appeared. The
// message is hand-built because AppendOpen cannot emit an empty Capabilities
// parameter (it omits the parameter entirely when Caps is empty).
func TestParseOpenEmptyType2CapabilityParameterInjectsIPv4Unicast(t *testing.T) {
	// Build an OPEN with a type-2 Capabilities parameter that declares length 0.
	params := []byte{2, 0} // type=2 (Capabilities), length=0
	msg := rawOpen(t, params)

	c, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(c.MP) != 1 || !c.MP[FamilyIPv4U] {
		t.Fatalf("want implicit IPv4 unicast even with an empty type-2 Capabilities parameter, got c.MP=%+v", c.MP)
	}
}

// TestParseOpenExtendedOptParamsMarkerRejected covers the RFC 9072 guard:
// an opt-param-len byte of 255 is the reserved marker for extended-length
// encoding, which this package doesn't yet support. ParseOpen must reject
// it with ErrExtendedOptParams and an empty Caps rather than misreading
// the extended-length bytes as a legacy parameter.
func TestParseOpenExtendedOptParamsMarkerRejected(t *testing.T) {
	msg := buildRawOpen(nil)
	msg[bgpHeaderLen+9] = 255 // RFC 9072 marker

	c, err := ParseOpen(msg)
	if !errors.Is(err, ErrExtendedOptParams) {
		t.Fatalf("want ErrExtendedOptParams, got %v", err)
	}
	if c.FourByteAS || len(c.MP) != 0 || len(c.AddPathRecv) != 0 {
		t.Fatalf("want empty Caps alongside ErrExtendedOptParams, got %+v", c)
	}
}

// TestParseOpenLargeNonMarkerOptParamLenStillParses proves the guard above
// is scoped exactly to the reserved value 255, not to "large" opt-param-len
// values in general: a legacy OPEN can legitimately carry a big, but
// non-255, opt-param-len (e.g. many capabilities/families) and must still
// parse normally.
func TestParseOpenLargeNonMarkerOptParamLenStillParses(t *testing.T) {
	// 126 unknown zero-length capabilities (code 99) = 252 bytes of
	// capability data; +2 bytes of type-2 parameter header = 254 bytes of
	// optional parameters total -- legitimately large, but distinct from
	// the 255 marker.
	var capsBuf []byte
	for range 126 {
		capsBuf = append(capsBuf, capBytes(99)...)
	}
	params := paramBytes(capsBuf...)
	if len(params) != 254 {
		t.Fatalf("test setup: params len = %d, want 254", len(params))
	}
	msg := rawOpen(t, params)
	if got := msg[bgpHeaderLen+9]; got != 254 {
		t.Fatalf("test setup: opt-param-len = %d, want 254", got)
	}

	if _, err := ParseOpen(msg); err != nil {
		t.Fatalf("want nil err for a legitimate large (non-255) opt-param-len, got %v", err)
	}
}

// TestAppendOpenMaxSizeCapsRoundTrips covers the maxCapsLen boundary from
// both directions: AppendOpen must not panic at exactly the maximum size it
// permits, the emitted opt-param-len must not collide with the RFC 9072
// marker (255), and the result must still round-trip through ParseOpen.
func TestAppendOpenMaxSizeCapsRoundTrips(t *testing.T) {
	const n = maxCapsLen / 6 // 6 bytes per MP capability entry
	if n*6 != maxCapsLen {
		t.Fatalf("test setup: maxCapsLen %d is not a multiple of 6", maxCapsLen)
	}
	mp := make(map[Family]bool, n)
	for i := range n {
		mp[Family{AFI: uint16(i + 1), SAFI: 1}] = true
	}

	msg := AppendOpen(nil, 65001, 180, "10.0.0.1", Caps{MP: mp}) // must not panic
	if got := msg[bgpHeaderLen+9]; got == 255 {
		t.Fatalf("opt-param-len hit the RFC 9072 reserved marker value 255 (got %d)", got)
	}
	parsed, err := ParseOpen(msg)
	if err != nil {
		t.Fatalf("max-size AppendOpen output failed to round-trip: %v", err)
	}
	if len(parsed.MP) != n {
		t.Fatalf("want %d families round-tripped, got %d: %+v", n, len(parsed.MP), parsed.MP)
	}
}

// TestParseOpenErrorReturnsEmptyCaps covers a guarantee: every error path
// must return an empty Caps, even when earlier parameters in the same
// message parsed successfully and would otherwise have left entries behind.
func TestParseOpenErrorReturnsEmptyCaps(t *testing.T) {
	caps := append([]byte{}, mpCapBytes(FamilyIPv4U)...) // valid: would populate c.MP
	caps = append(caps, as4CapBytes(65001)...)           // valid: would populate c.FourByteAS
	caps = append(caps, 1, 10, 0xAA, 0xBB)               // code=1 (MP), declares len=10, only 2 bytes follow
	msg := rawOpen(t, paramBytes(caps...))

	c, err := ParseOpen(msg)
	if err == nil {
		t.Fatal("want error")
	}
	if c.FourByteAS || len(c.MP) != 0 || len(c.AddPathRecv) != 0 {
		t.Fatalf("want empty Caps on error path even though earlier capabilities parsed cleanly, got %+v", c)
	}
}

// TestAppendOpenDeterministic covers a property: AppendOpen must not
// depend on Go's randomized map iteration order. Run repeatedly because
// that randomization varies per iteration, not just per process.
func TestAppendOpenDeterministic(t *testing.T) {
	c := Caps{
		FourByteAS: true,
		MP: map[Family]bool{
			FamilyIPv4U:         true,
			{AFI: 2, SAFI: 1}:   true,
			{AFI: 1, SAFI: 128}: true,
			{AFI: 25, SAFI: 70}: true,
		},
		AddPathRecv: map[Family]bool{
			FamilyIPv4U:         true,
			{AFI: 2, SAFI: 1}:   true,
			{AFI: 1, SAFI: 128}: true,
		},
	}
	want := AppendOpen(nil, 65001, 180, "10.0.0.1", c)
	for i := range 20 {
		got := AppendOpen(nil, 65001, 180, "10.0.0.1", c)
		if !bytes.Equal(got, want) {
			t.Fatalf("AppendOpen run %d differs from run 0:\n want=%x\n got =%x", i, want, got)
		}
	}
}

// TestAppendOpenBGPIDIs4In6Unmapped pins that an IPv4-mapped IPv6 BGP ID
// such as "::ffff:10.0.0.1" is Is4In6, not Is4, so it must be unmapped
// before the IPv4 check rather than silently emitted as 0.0.0.0.
func TestAppendOpenBGPIDIs4In6Unmapped(t *testing.T) {
	msg := AppendOpen(nil, 65001, 180, "::ffff:10.0.0.1", Caps{})
	// body layout: version(1) my-AS(2) hold-time(2) bgp-id(4) ...
	off := bgpHeaderLen + 1 + 2 + 2
	want := []byte{10, 0, 0, 1}
	got := msg[off : off+4]
	if !bytes.Equal(got, want) {
		t.Fatalf("want bgp id bytes %v, got %v", want, got)
	}
}

// TestAppendOpenPanicsOnUnparseableBGPID covers the other half: a
// genuinely invalid BGP ID must panic (builder-guards-its-preconditions),
// not silently emit 0.0.0.0.
func TestAppendOpenPanicsOnUnparseableBGPID(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, but AppendOpen returned normally")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "not a valid IP address") {
			t.Fatalf("want panic with 'not a valid IP address', got: %v", r)
		}
	}()
	AppendOpen(nil, 65001, 180, "not-an-ip", Caps{})
}

// TestAppendOpenPanicsOnIPv6BGPID covers the constraint that AppendOpen
// refuses to emit a genuine IPv6 BGP ID (not IPv4-mapped, but a real IPv6
// address), which is not representable in the 4-byte BGP ID field.
func TestAppendOpenPanicsOnIPv6BGPID(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("want panic, but AppendOpen returned normally")
		}
		msg, ok := r.(string)
		if !ok || !contains(msg, "not representable as an IPv4") {
			t.Fatalf("want panic with 'not representable as an IPv4', got: %v", r)
		}
	}()
	AppendOpen(nil, 65001, 180, "2001:db8::1", Caps{})
}

// TestAppendOpenSkipsEmptyCapabilitiesParameter pins that a Caps with
// nothing to advertise must produce opt-param-len=0 and no Capabilities
// optional parameter at all, not a degenerate zero-length one.
func TestAppendOpenSkipsEmptyCapabilitiesParameter(t *testing.T) {
	msg := AppendOpen(nil, 65001, 180, "10.0.0.1", Caps{})
	if got := msg[bgpHeaderLen+9]; got != 0 {
		t.Fatalf("want opt-param-len=0 for empty Caps, got %d", got)
	}
	if len(msg) != bgpHeaderLen+10 {
		t.Fatalf("want message to end right after the opt-param-len byte (no capabilities parameter emitted), got len=%d", len(msg))
	}
}

// FuzzParseOpen mirrors FuzzReadMsg, FuzzParsePeerHeader, and
// FuzzParseTLVs: ParseOpen walks attacker-controlled bytes through two
// nested length-prefixed loops (optional parameters containing
// capabilities) and must never panic, regardless of what error it returns.
func FuzzParseOpen(f *testing.F) {
	full := Caps{
		FourByteAS:  true,
		MP:          map[Family]bool{FamilyIPv4U: true, {AFI: 2, SAFI: 1}: true},
		AddPathRecv: map[Family]bool{FamilyIPv4U: true},
	}
	f.Add(AppendOpen(nil, 65001, 180, "10.0.0.1", full))
	f.Add(AppendOpen(nil, 4200000000, 180, "10.0.0.1", Caps{FourByteAS: true}))
	f.Add(AppendOpen(nil, 65001, 180, "10.0.0.1", Caps{}))
	f.Add(buildRawOpen(paramBytes(append(mpCapBytes(FamilyIPv4U), capBytes(2)...)...)))
	f.Add([]byte{})
	f.Add(make([]byte, 29))
	f.Add([]byte{2, 5, 0xAA, 0xBB})

	// Seeds a multi-optional-parameter message (each capability in its own
	// type-2 parameter, mirroring TestParseOpenMultipleOptionalParameters).
	multiParam := append([]byte{}, paramBytes(mpCapBytes(FamilyIPv4U)...)...)
	multiParam = append(multiParam, paramBytes(as4CapBytes(65001)...)...)
	multiParam = append(multiParam, paramBytes(addPathCapBytes(FamilyIPv4U, addPathRecvBit|addPathSendBit)...)...)
	f.Add(buildRawOpen(multiParam))

	// Asymmetric ADD-PATH direction bits within one OPEN: one family
	// receive-only, another send-only (mirrors TestMergeAddPathAsymmetric's
	// bit split, but as a single-message fuzz seed).
	asymAddPath := append([]byte{}, addPathCapBytes(FamilyIPv4U, addPathRecvBit)...)
	asymAddPath = append(asymAddPath, addPathCapBytes(Family{AFI: 2, SAFI: 1}, addPathSendBit)...)
	f.Add(buildRawOpen(paramBytes(asymAddPath...)))

	// The RFC 9072 extended-optional-parameters marker.
	marker := buildRawOpen(nil)
	marker[bgpHeaderLen+9] = 255
	f.Add(marker)

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseOpen(data) // must not panic
	})
}

// The hold time the OPEN actually carried, which ParseOpen walked past for
// as long as this package existed.
//
// It is a real protocol field -- RFC 4271 §4.2's body is version, my-AS,
// HOLD-TIME, BGP-ID, opt-param-len -- and it is the only timer BGP announces.
// The KEEPALIVE interval is a local timer a speaker never sends, so there is
// deliberately no field for it here and none downstream: hold-time/3 is a
// convention, and rendering a convention beside an observation is how an
// invented number gets an authoritative-looking home.
func TestParseOpenCarriesTheHoldTime(t *testing.T) {
	c := Caps{MP: map[Family]bool{FamilyIPv4U: true}}
	for _, want := range []uint16{180, 90, 240} {
		p, err := ParseOpen(AppendOpen(nil, 65001, want, "10.0.0.1", c))
		if err != nil {
			t.Fatalf("hold time %d: %v", want, err)
		}
		if p.HoldTime != want {
			t.Errorf("hold time: got %d, want %d", p.HoldTime, want)
		}
	}
}

// Zero is a legal, meaningful hold time -- RFC 4271 §4.2: "A value of 0
// indicates that the Hold Timer is never going to expire", i.e. keepalives
// are disabled for this session. It must survive as 0 rather than read as
// "absent", which is the distinction a nullable column downstream has to
// preserve too.
func TestParseOpenKeepsAZeroHoldTime(t *testing.T) {
	c := Caps{MP: map[Family]bool{FamilyIPv4U: true}}
	p, err := ParseOpen(AppendOpen(nil, 65001, 0, "10.0.0.1", c))
	if err != nil {
		t.Fatal(err)
	}
	if p.HoldTime != 0 {
		t.Errorf("hold time: got %d, want 0", p.HoldTime)
	}
}

// Merge takes the NEGOTIATED hold time, which RFC 4271 §4.2 defines as the
// smaller of the two a session's speakers sent. Capabilities merge by union
// -- both sides must support a family -- but a timer merges by minimum, and
// getting that backwards would report a session as more patient than it is.
func TestMergeNegotiatesTheSmallerHoldTime(t *testing.T) {
	a := Caps{HoldTime: 180, MP: map[Family]bool{}, AddPathRecv: map[Family]bool{}}
	b := Caps{HoldTime: 90, MP: map[Family]bool{}, AddPathRecv: map[Family]bool{}}
	if got := Merge(a, b).HoldTime; got != 90 {
		t.Errorf("Merge(180, 90) hold time = %d, want 90", got)
	}
	if got := Merge(b, a).HoldTime; got != 90 {
		t.Errorf("Merge(90, 180) hold time = %d, want 90 -- the minimum, whichever side it came from", got)
	}
}
