package bgp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/jp2195/vantage/bmp"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// linkNLRI is the NLRI portion of a real MP_REACH attribute value captured
// from XRd 26.1.1 (bgp/testdata/corpus/iosxr/xrd-26.1.1-p-linkstate.bmpcap),
// with the AFI(2)/SAFI(1)/next-hop-length(1)/next-hop(4)/reserved(1) prefix
// already stripped -- DecodeLsNLRI takes only the NLRI list.
//
// Decoded by hand: type 2 (Link), length 0x61, protocol 3 (OSPFv2),
// identifier 100, local node 10.255.0.2 in AS 65000 area 0, remote node
// 10.255.0.5, interface 10.1.0.2 -> neighbor 10.1.0.3.
const linkNLRIHex = "000200610300000000000000640100002002000004" +
	"0000FDE802010004000000000202000400000000020300040AFF0002" +
	"01010020020000040000FDE8020100040000000002020004000000000203" +
	"00040AFF0005010300040A010002010400040A010003"

// nodeNLRI is a real Node NLRI from the same capture: type 1, length 0x2D,
// protocol 3 (OSPFv2), identifier 100, IGP router-ID 10.255.0.1.
const nodeNLRIHex = "0001002D030000000000000064010000200200000400" +
	"00FDE802010004000000000202000400000000020300040AFF0001"

// routerID converts a raw router-ID descriptor to an IPv4 address, failing
// the test rather than panicking when the descriptor is missing or not 4
// bytes -- a malformed fixture should fail an assertion, not crash the test
// binary.
func routerID(t *testing.T, name string, raw []byte) netip.Addr {
	t.Helper()
	a, ok := netip.AddrFromSlice(raw)
	if !ok {
		t.Fatalf("%s router-id: %d bytes, not a valid address", name, len(raw))
	}
	return a
}

func TestDecodeLsNLRILink(t *testing.T) {
	b, err := hex.DecodeString(linkNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	nodes, links, _, undecoded, err := DecodeLsNLRI(b)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(nodes) != 0 || undecoded != 0 {
		t.Fatalf("got %d nodes, %d undecoded; want 0, 0", len(nodes), undecoded)
	}
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	l := links[0]
	if l.Protocol != 3 {
		t.Errorf("Protocol = %d, want 3 (OSPFv2)", l.Protocol)
	}
	if l.Identifier != 100 {
		t.Errorf("Identifier = %d, want 100", l.Identifier)
	}
	if got := routerID(t, "local", l.Local.RouterID); got.String() != "10.255.0.2" {
		t.Errorf("local router-id = %s, want 10.255.0.2", got)
	}
	if got := routerID(t, "remote", l.Remote.RouterID); got.String() != "10.255.0.5" {
		t.Errorf("remote router-id = %s, want 10.255.0.5", got)
	}
	if l.Local.ASN != 65000 || l.Remote.ASN != 65000 {
		t.Errorf("ASNs = %d/%d, want 65000/65000", l.Local.ASN, l.Remote.ASN)
	}
	if l.LocalIfAddr.String() != "10.1.0.2" {
		t.Errorf("LocalIfAddr = %s, want 10.1.0.2", l.LocalIfAddr)
	}
	if l.RemoteIfAddr.String() != "10.1.0.3" {
		t.Errorf("RemoteIfAddr = %s, want 10.1.0.3", l.RemoteIfAddr)
	}
}

// TestDecodeLsNLRILinkRejectsWrongWidthIfAddr reproduces a crash in
// collector.lsLinkMessage: netip.AddrFromSlice accepts a 16-byte value as a
// valid address, but TLV 259 (IPv4 Interworking Interface Address, RFC 9552
// §5.2.2) is defined as IPv4-only, and Addr.As4() panics on any
// valid-but-non-IPv4 Addr. Unchecked, a 16-byte value in TLV 259 -- e.g.
// from a malformed or adversarial UPDATE -- decodes to a valid, non-IPv4
// LocalIfAddr that satisfies every downstream IsValid() gate and panics the
// first time something calls As4() on it, crashing the whole collector
// process on one bad message from one router.
//
// A KNOWN link-descriptor TLV of the wrong width is therefore a hard decode
// error (matching decodeNodeDescriptor's precedent for node descriptor
// sub-TLVs), not a silent "keep the zero value" -- because a
// silently-dropped 259/260 is indistinguishable from that TLV never having
// appeared, i.e. an unnumbered link, which is exactly the
// plausible-looking-zero failure mode this guard exists to close. The test
// asserts that DecodeLsNLRI's error names the TLV and the bad length, which
// keeps the crash closed by the strongest means available: the malformed
// value never survives into an LsLinkNLRI at all, so
// collector.lsLinkMessage's own Is4()-gated As4() call (kept as
// defense-in-depth, see its comment) cannot be reached with it in the first
// place.
//
// Built by splicing the real captured linkNLRIHex fixture's TLV 259 value
// from 4 bytes (10.1.0.2) to 16, rather than a hand-built fixture unrelated
// to real wire bytes, so this exercises the actual decoder against
// (patched) real capture bytes.
func TestDecodeLsNLRILinkRejectsWrongWidthIfAddr(t *testing.T) {
	b, err := hex.DecodeString(linkNLRIHex)
	if err != nil {
		t.Fatal(err)
	}

	// TLV 259, length 4: type=0x0103, len=0x0004. Confirmed to occur exactly
	// once in this fixture (in the real 10.1.0.2 interface-address TLV, not
	// inside any node descriptor's sub-TLV bytes).
	needle := []byte{0x01, 0x03, 0x00, 0x04}
	i := bytes.Index(b, needle)
	if i < 0 {
		t.Fatal("fixture no longer contains a TLV 259 (IPv4 If Addr) header -- update this test")
	}
	badVal := []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01} // 2001:db8::1
	patched := append([]byte{}, b[:i]...)
	patched = append(patched, 0x01, 0x03, 0x00, byte(len(badVal)))
	patched = append(patched, badVal...)
	patched = append(patched, b[i+4+4:]...) // skip old header(4) + old 4-byte value

	// The outer Link NLRI's own length field (bytes [2:4]) must grow by the
	// value's size delta, or DecodeLsNLRI's length bookkeeping reports a
	// truncated NLRI instead of exercising the path this test exists to
	// cover.
	oldLen := binary.BigEndian.Uint16(patched[2:4])
	binary.BigEndian.PutUint16(patched[2:4], oldLen+uint16(len(badVal)-4))

	_, _, _, _, err = DecodeLsNLRI(patched)
	if err == nil {
		t.Fatal("want an error for a 16-byte TLV 259 (want 4), got nil")
	}
	if !strings.Contains(err.Error(), "259") || !strings.Contains(err.Error(), "16") {
		t.Errorf("error = %q, want it to name TLV 259 and the observed length 16", err)
	}
}

func TestDecodeLsNLRINode(t *testing.T) {
	b, err := hex.DecodeString(nodeNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	nodes, links, _, undecoded, err := DecodeLsNLRI(b)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(links) != 0 || undecoded != 0 {
		t.Fatalf("got %d links, %d undecoded; want 0, 0", len(links), undecoded)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	n := nodes[0]
	if n.Protocol != 3 || n.Identifier != 100 {
		t.Errorf("Protocol/Identifier = %d/%d, want 3/100", n.Protocol, n.Identifier)
	}
	if got := routerID(t, "node local", n.Local.RouterID); got.String() != "10.255.0.1" {
		t.Errorf("router-id = %s, want 10.255.0.1", got)
	}
}

// An NLRI type this build does not decode must be COUNTED rather than
// silently skipped -- the caller leaves the bytes raw and raises a flag
// instead of reporting an empty success. That is what this test pins.
//
// The type it uses has moved twice, and how it moved is the point. It was
// type 3 until 2026-08-17, when a real capture arrived and type 3 became
// decodable. It was then type 4 until 2026-09-01, when the same thing
// happened again -- 74 captured NLRI, two routers, see lsNLRIPrefixV6.
//
// So this test now uses an UNASSIGNED type rather than the next unimplemented
// one. Both previous choices were types that merely lacked a capture, and
// both went stale the moment one arrived; an unassigned type is genuinely
// unhandled and stays that way. RFC 9552 assigns 1-4 and later RFCs have
// taken low values above that, so this reaches well up into the unassigned
// range rather than picking the next number free today.
func TestDecodeLsNLRICountsUndecodedTypes(t *testing.T) {
	// Type 32767 (0x7FFF, unassigned), length 4, four bytes of body.
	b, err := hex.DecodeString("7FFF00040A000001")
	if err != nil {
		t.Fatal(err)
	}
	nodes, links, _, undecoded, err := DecodeLsNLRI(b)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(nodes) != 0 || len(links) != 0 {
		t.Fatalf("decoded something from an unsupported NLRI type")
	}
	if undecoded != 1 {
		t.Fatalf("undecoded = %d, want 1", undecoded)
	}
}

// lsTLVBytes appends one Type(2) Length(2) Value TLV, mirroring walkTLVs'
// wire shape -- a small local builder rather than more hex string literals,
// so the tests below can splice in TLV types that have no real captured
// example (261/263, and a nested descriptor sub-TLV).
func lsTLVBytes(b []byte, typ uint16, val []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, typ)
	b = binary.BigEndian.AppendUint16(b, uint16(len(val)))
	return append(b, val...)
}

// TestDecodeLsNLRIKeepsUnknownLinkTLVs covers the Link-NLRI-level half: a
// TLV this build does not recognize (261, IPv6 Interface Address -- a real
// TLV RFC 9552 §5.2.2 defines but this build does not decode, no
// IPv6-IGP capture exists yet) at the Link NLRI's own top level must be
// preserved in Unknown, not silently dropped -- which mattered most for a
// link on an IPv6-only IGP, which would otherwise decode with NO interface
// identity at all and nothing indicating why.
func TestDecodeLsNLRIKeepsUnknownLinkTLVs(t *testing.T) {
	b, err := hex.DecodeString(linkNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	extra := lsTLVBytes(nil, 261, []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	patched := append([]byte{}, b...)
	patched = append(patched, extra...)
	// Grow the outer Link NLRI's own length field (bytes [2:4]) to match, or
	// DecodeLsNLRI's length bookkeeping reports a truncated NLRI instead of
	// exercising the path this test exists to cover.
	oldLen := binary.BigEndian.Uint16(patched[2:4])
	binary.BigEndian.PutUint16(patched[2:4], oldLen+uint16(len(extra)))

	_, links, _, _, err := DecodeLsNLRI(patched)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1", len(links))
	}
	got, ok := links[0].Unknown[261]
	if !ok {
		t.Fatalf("unknown TLV 261 was dropped; Unknown = %v", links[0].Unknown)
	}
	if len(got) != 16 {
		t.Errorf("Unknown[261] = %x, want the 16-byte value preserved verbatim", got)
	}
}

// TestDecodeLsNLRIKeepsUnknownNodeDescriptorSubTLVs covers the nested
// half: a sub-TLV inside a Local Node Descriptor (256) that this build
// does not recognize must also reach Unknown, via decodeNodeDescriptor
// writing into the caller's map -- not silently ignored as pure "forward
// compatibility."
func TestDecodeLsNLRIKeepsUnknownNodeDescriptorSubTLVs(t *testing.T) {
	b, err := hex.DecodeString(nodeNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	// nodeNLRIHex's layout: outer header b[0:4], protocol b[4], identifier
	// b[5:13], Local Node Descriptor (256) TLV header b[13:17] (type
	// b[13:15], length b[15:17]), its sub-TLVs from b[17:]. Appending a
	// sub-TLV to the descriptor means growing both the descriptor's own
	// length (b[15:17]) and the outer NLRI's length (b[2:4]).
	extra := lsTLVBytes(nil, 65000, []byte{0xAB, 0xCD})
	patched := append([]byte{}, b...)
	patched = append(patched, extra...)
	oldOuterLen := binary.BigEndian.Uint16(patched[2:4])
	binary.BigEndian.PutUint16(patched[2:4], oldOuterLen+uint16(len(extra)))
	oldDescLen := binary.BigEndian.Uint16(patched[15:17])
	binary.BigEndian.PutUint16(patched[15:17], oldDescLen+uint16(len(extra)))

	nodes, _, _, _, err := DecodeLsNLRI(patched)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}
	got, ok := nodes[0].Unknown[65000]
	if !ok {
		t.Fatalf("unknown descriptor sub-TLV 65000 was dropped; Unknown = %v", nodes[0].Unknown)
	}
	if hex.EncodeToString(got) != "abcd" {
		t.Errorf("Unknown[65000] = %x, want abcd", got)
	}
}

func TestDecodeLsNLRIRejectsTruncated(t *testing.T) {
	// Declares length 0x61 but carries 4 bytes.
	b, err := hex.DecodeString("0002006103000000")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := DecodeLsNLRI(b); err == nil {
		t.Fatal("want error for a truncated NLRI, got nil")
	}
}

// TestDecodeLsNLRIRejectsShortKnownSubTLV covers a Node NLRI whose Local Node
// Descriptor carries an AS sub-TLV (512) of length 2 instead of the required
// 4. Silently accepting it would decode ASN as 0 with a nil error --
// indistinguishable from a legitimate AS-0 record, and a later phase's node
// join key (cityHash64 over protocol/identifier/asn/bgpls_id/area/router_id)
// would collapse a genuinely different node onto that same wrong key.
func TestDecodeLsNLRIRejectsShortKnownSubTLV(t *testing.T) {
	// Node NLRI: protocol 3, identifier 100, Local Node Descriptor holding a
	// 2-byte AS sub-TLV (512) followed by a valid 4-byte IGP router-id (515).
	b, err := hex.DecodeString("0001001B0300000000000000640100000E020000020001020300040AFF0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := DecodeLsNLRI(b); err == nil {
		t.Fatal("want error for an AS sub-TLV of the wrong width, got nil")
	}
}

// TestDecodeLsNLRIRejectsMissingRouterID covers a Node NLRI whose Local Node
// Descriptor has AS/BGP-LS-ID/Area but no IGP Router-ID (515) sub-TLV at all.
// Such a descriptor has no identity: the same join key concern as above
// applies, and every node without a captured router-id would otherwise
// collapse onto the same all-zero RouterID.
func TestDecodeLsNLRIRejectsMissingRouterID(t *testing.T) {
	// Node NLRI: protocol 3, identifier 100, Local Node Descriptor holding
	// AS 65000, BGP-LS ID 0, Area 0 -- and no router-id sub-TLV.
	b, err := hex.DecodeString("0001002503000000000000006401000018020000040000FDE802010004000000000202000400000000")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := DecodeLsNLRI(b); err == nil {
		t.Fatal("want error for a node descriptor with no IGP router-id, got nil")
	}
}

// nodeAttrHex is a real BGP path attribute 29 value from
// xrd-26.1.1-p-linkstate.bmpcap, for the Node NLRI of xr-rr1. Decoded by
// hand: node name "xr-rr1", IPv4 router-ID 10.255.0.1, SR Capabilities with
// range 8000 from label 16000 (SRGB 16000-23999), SR algorithms 0 and 1, and
// SR Local Block range 1000 from 15000 (SRLB 15000-15999).
const nodeAttrHex = "010A0002010A0402000678722D72723104040004" +
	"0AFF0001040A000C0000001F4004890003003E80040B0002000104" +
	"0C000C00000003E804890003003A98"

// linkAttrHex is the attribute for one of the Link NLRI in the same capture:
// max bandwidth 1 Gbps, TE default metric 1, adjacency SID label 24001.
const linkAttrHex = "010200080000000400000003010B0002010A04040004" +
	"0AFF0002040600040AFF0005044100044CEE6B2804440004000000" +
	"01044700030000010" + "44B000760000000005DC1"

// TestDecodeLsAttrsNodeNameInvalidUTF8 covers the one string this package
// builds from raw wire bytes. The Node Name reaches a proto3 string field,
// and proto.Marshal rejects a whole message holding invalid UTF-8, so an
// unsanitized name silently lost every node and link in the UPDATE.
func TestDecodeLsAttrsNodeNameInvalidUTF8(t *testing.T) {
	b := binary.BigEndian.AppendUint16(nil, lsAttrNodeName)
	b = binary.BigEndian.AppendUint16(b, 4)
	b = append(b, 'r', 0xff, 0xfe, '1')
	a, err := DecodeLsAttrs(b)
	if err != nil {
		t.Fatalf("DecodeLsAttrs: %v", err)
	}
	if a.NodeName != "r\uFFFD1" {
		t.Fatalf("NodeName = %q, want %q", a.NodeName, "r\uFFFD1")
	}
}

func TestDecodeLsAttrsNode(t *testing.T) {
	b, err := hex.DecodeString(nodeAttrHex)
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecodeLsAttrs(b)
	if err != nil {
		t.Fatalf("DecodeLsAttrs: %v", err)
	}
	if a.NodeName != "xr-rr1" {
		t.Errorf("NodeName = %q, want %q", a.NodeName, "xr-rr1")
	}
	if a.LocalRouterIDv4.String() != "10.255.0.1" {
		t.Errorf("LocalRouterIDv4 = %s, want 10.255.0.1", a.LocalRouterIDv4)
	}
	// The SRGB is the single most load-bearing value here: a wrong base or
	// size makes every derived label wrong.
	if a.SRGBBase != 16000 || a.SRGBSize != 8000 {
		t.Errorf("SRGB = %d+%d, want 16000+8000", a.SRGBBase, a.SRGBSize)
	}
	if a.SRLBBase != 15000 || a.SRLBSize != 1000 {
		t.Errorf("SRLB = %d+%d, want 15000+1000", a.SRLBBase, a.SRLBSize)
	}
	if len(a.SRAlgorithms) != 2 || a.SRAlgorithms[0] != 0 || a.SRAlgorithms[1] != 1 {
		t.Errorf("SRAlgorithms = %v, want [0 1]", a.SRAlgorithms)
	}
}

func TestDecodeLsAttrsLink(t *testing.T) {
	b, err := hex.DecodeString(linkAttrHex)
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecodeLsAttrs(b)
	if err != nil {
		t.Fatalf("DecodeLsAttrs: %v", err)
	}
	if len(a.AdjSIDs) != 1 || a.AdjSIDs[0].SID != 24001 {
		t.Errorf("AdjSIDs = %+v, want exactly one entry with SID 24001", a.AdjSIDs)
	}
	if a.TEMetric != 1 {
		t.Errorf("TEMetric = %d, want 1", a.TEMetric)
	}
	if a.LocalRouterIDv4.String() != "10.255.0.2" {
		t.Errorf("LocalRouterIDv4 = %s, want 10.255.0.2", a.LocalRouterIDv4)
	}
	if a.RemoteRouterIDv4.String() != "10.255.0.5" {
		t.Errorf("RemoteRouterIDv4 = %s, want 10.255.0.5", a.RemoteRouterIDv4)
	}
	// 0x4CEE6B28 as an IEEE-754 float32 is 1.25e8 bytes/s, i.e. 1 Gbps.
	if a.MaxBandwidth < 1.24e8 || a.MaxBandwidth > 1.26e8 {
		t.Errorf("MaxBandwidth = %v, want ~1.25e8 bytes/s", a.MaxBandwidth)
	}
	// linkAttrHex's first 12 bytes (0102 0008 0000000400000003) are TLV
	// 258 (Link Local/Remote Identifiers) as XRd 26.1.1 actually sends it
	// -- inside the BGP-LS Attribute, not the Link NLRI. DecodeLsAttrs
	// previously had no case for 258 at all, so this fell into Unknown and
	// ls_links.link_local_id/link_remote_id were always 0
	// for every XRd-sourced link -- silently colliding parallel adjacencies
	// onto one ls_links sort tuple (both columns are in that table's ORDER
	// BY identity tail).
	if !a.HasLinkID || a.LinkLocalID != 4 || a.LinkRemoteID != 3 {
		t.Errorf("HasLinkID/LinkLocalID/LinkRemoteID = %v/%d/%d, want true/4/3", a.HasLinkID, a.LinkLocalID, a.LinkRemoteID)
	}
}

// TestDecodeLsAttrsRejectsWrongWidthLinkLocalRemID covers TLV 258 (Link
// Local/Remote Identifiers) at the wrong width inside the BGP-LS Attribute --
// the attribute-side counterpart of decodeLsLink's own strictness for the
// same TLV number. A wrong width silently producing
// 0/0 would be indistinguishable from the TLV never having appeared, and
// both fields feed ls_links' sort-key identity tail.
func TestDecodeLsAttrsRejectsWrongWidthLinkLocalRemID(t *testing.T) {
	b, err := hex.DecodeString("0102000700000004000003") // 7 bytes, want 8
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLsAttrs(b); err == nil {
		t.Fatal("want an error for a 7-byte TLV 258 (want 8), got nil")
	}
}

// TestDecodeLsAttrsMultipleAdjacencySIDs covers a link carrying more than
// one Adjacency SID TLV. RFC 9085 §2.2.1 permits several 1099 TLVs on one
// link, and a protected/unprotected SID pair is the ordinary case for
// SR-MPLS, not a malformed message -- so the second TLV must add an entry,
// not overwrite the first. linkAttrHex is the real captured attribute
// (single Adjacency SID, SID 24001); the second TLV appended here is
// synthetic, built to a distinct SID/flags/weight so the two are easy to
// tell apart in the assertions.
func TestDecodeLsAttrsMultipleAdjacencySIDs(t *testing.T) {
	// Type 1099 (0x044B), length 7: Flags 0x20, Weight 0x05, Reserved
	// 0x0000, SID (3-byte label) 24002 (0x005DC2).
	const secondAdjSIDHex = "044B000720050000005DC2"
	b, err := hex.DecodeString(linkAttrHex + secondAdjSIDHex)
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecodeLsAttrs(b)
	if err != nil {
		t.Fatalf("DecodeLsAttrs: %v", err)
	}
	if len(a.AdjSIDs) != 2 {
		t.Fatalf("AdjSIDs = %+v, want 2 entries", a.AdjSIDs)
	}
	if a.AdjSIDs[0].SID != 24001 {
		t.Errorf("AdjSIDs[0].SID = %d, want 24001 (from the captured TLV)", a.AdjSIDs[0].SID)
	}
	if a.AdjSIDs[1].SID != 24002 || a.AdjSIDs[1].Flags != 0x20 || a.AdjSIDs[1].Weight != 0x05 {
		t.Errorf("AdjSIDs[1] = %+v, want {SID:24002 Flags:0x20 Weight:0x05} (the synthetic TLV)", a.AdjSIDs[1])
	}
}

// A TLV nobody has seen must be preserved, not dropped: that is how a
// vendor difference gets discovered at all.
func TestDecodeLsAttrsKeepsUnknownTLVs(t *testing.T) {
	// Type 0xFFFF, length 2, value 0xBEEF -- not a real TLV.
	b, err := hex.DecodeString("FFFF0002BEEF")
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecodeLsAttrs(b)
	if err != nil {
		t.Fatalf("DecodeLsAttrs: %v", err)
	}
	got, ok := a.Unknown[0xFFFF]
	if !ok {
		t.Fatalf("unknown TLV 0xFFFF was dropped; Unknown = %v", a.Unknown)
	}
	if hex.EncodeToString(got) != "beef" {
		t.Errorf("Unknown[0xFFFF] = %x, want beef", got)
	}
}

// TestDecodeSRRangeRejectsSecondRange pins that decodeSRRange refuses an SR
// Capabilities/SR Local Block TLV carrying a second {Range Size, SID/Label}
// entry. Keeping only the FIRST would report a real but understated SRGB
// with err==nil whenever a router sent two. Built from the real
// captured SR Capabilities value (nodeAttrHex's TLV 1034, range 8000 from
// label 16000) with a second, synthetic range (100 from label 4096)
// appended and the outer TLV's length grown to match.
func TestDecodeSRRangeRejectsSecondRange(t *testing.T) {
	const twoRangeSRCapHex = "040A0016" + // TLV 1034, length 0x16 = 22
		"0000001f4004890003003e80" + // real range: size 8000, base 16000
		"00006404890003001000" // synthetic second range: size 100, base 4096
	b, err := hex.DecodeString(twoRangeSRCapHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLsAttrs(b); err == nil {
		t.Fatal("want an error for a second SR Capabilities range, got nil (silently understated SRGB)")
	}
}

// TestDecodeSRRangeRejectsUnknownNestedSubTLV pins that decodeSRRange fails
// on an SR range whose nested sub-TLV is not the SID/Label sub-TLV (1161).
// Returning base=0, err=nil there would be a plausible-looking fabricated
// "SRGB starting at label 0" rather than a parse failure. Unlike
// a top-level or descriptor unknown TLV (genuinely forward-compatible
// elsewhere in this package), this nested sub-TLV is the only place a base
// value can come from, so there is no value to report and this must fail
// loud instead.
func TestDecodeSRRangeRejectsUnknownNestedSubTLV(t *testing.T) {
	const unknownNestedSubTLVHex = "040A000A" + // TLV 1034, length 0x0A = 10
		"0000006412340002AAAA" // flags+reserved+size=100, then an unrecognized nested sub-TLV (0x1234)
	b, err := hex.DecodeString(unknownNestedSubTLVHex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLsAttrs(b); err == nil {
		t.Fatal("want an error for an SR range with an unrecognized nested sub-TLV, got nil (fabricated base=0)")
	}
}

// Attribute 29 was previously not in the parser's attribute table at all,
// so every link-state attribute -- the SRGB, the SIDs, the metrics -- was
// discarded before an envelope was ever built.
func TestParseUpdateDecodesLinkState(t *testing.T) {
	msgs := corpusUpdates(t, "testdata/corpus/iosxr/xrd-26.1.1-p-linkstate.bmpcap")
	var sawNode, sawLink bool
	for _, u := range msgs {
		for range u.LsNodes {
			sawNode = true
			if u.LsAttrs != nil && u.LsAttrs.NodeName == "xr-rr1" {
				if u.LsAttrs.SRGBBase != 16000 || u.LsAttrs.SRGBSize != 8000 {
					t.Errorf("xr-rr1 SRGB = %d+%d, want 16000+8000",
						u.LsAttrs.SRGBBase, u.LsAttrs.SRGBSize)
				}
			}
		}
		if len(u.LsLinks) > 0 {
			sawLink = true
			if u.LsAttrs == nil {
				t.Error("link UPDATE decoded no BGP-LS attribute")
			}
		}
		// A successful typed decode must clear the raw fallback, exactly as
		// every other typed family clears it.
		if (len(u.LsNodes) > 0 || len(u.LsLinks) > 0) && len(u.RawReach) != 0 {
			t.Errorf("typed LS decode left %d bytes in RawReach", len(u.RawReach))
		}
	}
	if !sawNode || !sawLink {
		t.Fatalf("corpus produced node=%v link=%v; want both", sawNode, sawLink)
	}
}

func corpusUpdates(t *testing.T, path string) []*Update {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var out []*Update
	r := bytes.NewReader(raw)
	for {
		m, err := bmp.ReadMsg(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("bmp.ReadMsg: %v", err)
		}
		if m.Type != bmp.TypeRouteMonitoring {
			continue
		}
		_, rest, err := bmp.ParsePeerHeader(m.Payload)
		if err != nil {
			t.Fatalf("ParsePeerHeader: %v", err)
		}
		// rest is the full BGP message (19-byte header + body); ParseUpdate
		// wants only the body, same as collector/session.go's rest[19:].
		u, err := ParseUpdate(rest[19:], Caps{})
		if err != nil {
			t.Fatalf("ParseUpdate: %v", err)
		}
		out = append(out, u)
	}
	return out
}

// TestParseUpdateFlagsUnknownLsAttrTLV covers the attribute side:
// DecodeLsAttrs routing a TLV into Unknown is not itself an error (the
// attribute otherwise decodes fine), so unless the ParseUpdate path calls
// u.flag for it, an unrecognized BGP-LS attribute TLV is invisible on the
// parse-anomaly dashboards.
func TestParseUpdateFlagsUnknownLsAttrTLV(t *testing.T) {
	nlri, err := hex.DecodeString(nodeNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	unknownAttrTLV, err := hex.DecodeString("FFFF0002BEEF")
	if err != nil {
		t.Fatal(err)
	}
	attrs := buildAttr(0x80, attrLinkState, unknownAttrTLV)
	attrs = append(attrs, buildAttr(0x80, attrMPReach, mpReachVal(FamilyBGPLS, []byte{10, 0, 0, 9}, nlri))...)
	body := buildBody(nil, attrs, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, f := range u.Flags {
		got = got || f == vantagev1.ParseFlag_PARSE_FLAG_LS_TLV_UNKNOWN
	}
	if !got {
		t.Fatalf("flags = %v, want PARSE_FLAG_LS_TLV_UNKNOWN", u.Flags)
	}
}

// TestParseUpdateFlagsUnknownLsNLRITLV covers the NLRI side: a Node/Link
// NLRI TLV this build does not recognize decodes as a structural success
// (walkTLVs never errors on an unknown type); that silent routing into
// Unknown used to stay silent all the way to ClickHouse -- the bytes
// reached unknown_tlvs, but nothing said so.
func TestParseUpdateFlagsUnknownLsNLRITLV(t *testing.T) {
	nlri, err := hex.DecodeString(linkNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	// Append an unrecognized top-level Link NLRI TLV (261) and grow the
	// outer Link NLRI's length field to match -- same construction as
	// TestDecodeLsNLRIKeepsUnknownLinkTLVs.
	extra := lsTLVBytes(nil, 261, []byte{0x20, 0x01, 0x0d, 0xb8})
	patched := append([]byte{}, nlri...)
	patched = append(patched, extra...)
	oldLen := binary.BigEndian.Uint16(patched[2:4])
	binary.BigEndian.PutUint16(patched[2:4], oldLen+uint16(len(extra)))

	attrs := buildAttr(0x80, attrMPReach, mpReachVal(FamilyBGPLS, []byte{10, 0, 0, 9}, patched))
	body := buildBody(nil, attrs, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	for _, f := range u.Flags {
		got = got || f == vantagev1.ParseFlag_PARSE_FLAG_LS_TLV_UNKNOWN
	}
	if !got {
		t.Fatalf("flags = %v, want PARSE_FLAG_LS_TLV_UNKNOWN", u.Flags)
	}
}

// TestParseUpdateMPUnreachBGPLSWithdrawn pins that a BGP-LS NLRI carried in
// MP_UNREACH lands in LsLinksWithdrawn, not LsLinks: with one pair of slices
// for both paths, a withdrawn link would be byte-for-byte identical to an
// announced one. The NLRI bytes (linkNLRIHex) are the same real Link NLRI
// captured from XRd 26.1.1 that TestDecodeLsNLRILink decodes; only the
// MP_UNREACH_NLRI attribute framing around them (AFI/SAFI, no next hop, no
// reserved byte) is synthetic -- BGP-LS withdrawals do not appear anywhere in
// the committed corpus.
func TestParseUpdateMPUnreachBGPLSWithdrawn(t *testing.T) {
	nlri, err := hex.DecodeString(linkNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	val := mpUnreachVal(FamilyBGPLS, nlri)
	attr := buildAttr(0x80, attrMPUnreach, val)
	body := buildBody(nil, attr, nil)

	u, err := ParseUpdate(body, capsV4(false))
	if err != nil {
		t.Fatal(err)
	}
	if u.Family != FamilyBGPLS {
		t.Fatalf("family = %+v, want BGP-LS", u.Family)
	}
	if len(u.LsLinksWithdrawn) != 1 {
		t.Fatalf("LsLinksWithdrawn = %+v, want 1 entry", u.LsLinksWithdrawn)
	}
	if len(u.LsLinks) != 0 {
		t.Fatalf("LsLinks (announced) = %+v, want empty -- this NLRI was withdrawn", u.LsLinks)
	}
	if len(u.LsNodes) != 0 || len(u.LsNodesWithdrawn) != 0 {
		t.Fatalf("LsNodes/LsNodesWithdrawn = %+v/%+v, want both empty -- this NLRI carried no node entry",
			u.LsNodes, u.LsNodesWithdrawn)
	}
	if len(u.RawUnreach) != 0 {
		t.Errorf("clean typed LS withdraw decode left %d bytes in RawUnreach", len(u.RawUnreach))
	}
}

// FuzzDecodeLsNLRI and FuzzDecodeLsAttrs mirror FuzzParseUpdate's shape
// (the closest existing model in this repo, of 14 fuzz targets total) for
// the two link-state decoders. Seeded with the real captured wire bytes
// already used above (linkNLRIHex/nodeNLRIHex for the NLRI decoder,
// nodeAttrHex/linkAttrHex for the attribute decoder, all from
// bgp/testdata/corpus/iosxr/xrd-26.1.1-p-linkstate.bmpcap) plus the
// hand-built adversarial fixtures the tests above already exercise as
// named cases, so the seeds are chosen deliberately to reach every
// strict-decode path rather than leaving the fuzzer to discover them from
// scratch. Neither decoder may panic on any input -- that is the entire
// contract; a decode failure is an ordinary error return, not a bug.
func FuzzDecodeLsNLRI(f *testing.F) {
	for _, h := range []string{
		linkNLRIHex, nodeNLRIHex,
		"000300040A000001", // undecoded NLRI type (prefix)
		"0002006103000000", // truncated
		"0001001B0300000000000000640100000E020000020001020300040AFF0001",                     // short known sub-TLV
		"0001002503000000000000006401000018020000040000FDE802010004000000000202000400000000", // missing router-id
	} {
		b, err := hex.DecodeString(h)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		DecodeLsNLRI(b) // must not panic
	})
}

func FuzzDecodeLsAttrs(f *testing.F) {
	for _, h := range []string{
		nodeAttrHex, linkAttrHex,
		"FFFF0002BEEF",           // unknown top-level TLV
		"0102000700000004000003", // TLV 258, wrong width
		"040A0016" + "0000001f4004890003003e80" + "00006404890003001000", // SR Capabilities, two ranges
		"040A000A" + "0000006412340002AAAA",                              // SR Capabilities, unrecognized nested sub-TLV
	} {
		b, err := hex.DecodeString(h)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		DecodeLsAttrs(b) // must not panic
	})
}

// prefixNLRIHex is a real IPv4 Topology Prefix NLRI (type 3) captured from
// XRd 26.1.1 on 2026-08-17, read out of the archive's
// ls_events.raw_reach with the MP_REACH AFI/SAFI/next-hop/reserved prefix
// stripped, exactly as linkNLRIHex and nodeNLRIHex were.
//
// Decoded by hand before the decoder existed: type 3, length 0x3B (59),
// protocol 3 (OSPFv2), identifier 100, local node descriptor AS 65000 /
// BGP-LS ID 0 / area 0 / router-ID 10.255.0.5, then TLV 264 (OSPF Route
// Type) = 1 (intra-area) and TLV 265 (IP Reachability) = prefix length 0x20
// (32) with the four packed bytes 0A FF 00 05 -- 10.255.0.5/32, which is
// xr-p2's Loopback0.
const prefixNLRIHex = "0003003B03" + "0000000000000064" +
	"01000020" + "020000040000FDE8" + "0201000400000000" +
	"0202000400000000" + "020300040AFF0005" +
	"0108000101" +
	"01090005200AFF0005"

func TestDecodeLsNLRIPrefix(t *testing.T) {
	b, err := hex.DecodeString(prefixNLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	nodes, links, prefixes, undecoded, err := DecodeLsNLRI(b)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(nodes) != 0 || len(links) != 0 || undecoded != 0 {
		t.Fatalf("got %d nodes, %d links, %d undecoded; want 0, 0, 0", len(nodes), len(links), undecoded)
	}
	if len(prefixes) != 1 {
		t.Fatalf("got %d prefixes, want 1", len(prefixes))
	}
	p := prefixes[0]
	if p.Protocol != 3 {
		t.Errorf("Protocol = %d, want 3 (OSPFv2)", p.Protocol)
	}
	if p.Identifier != 100 {
		t.Errorf("Identifier = %d, want 100", p.Identifier)
	}
	if got := routerID(t, "local", p.Local.RouterID); got.String() != "10.255.0.5" {
		t.Errorf("local router-id = %s, want 10.255.0.5", got)
	}
	if p.Local.ASN != 65000 {
		t.Errorf("local ASN = %d, want 65000", p.Local.ASN)
	}
	if p.OSPFRouteType != 1 {
		t.Errorf("OSPFRouteType = %d, want 1 (intra-area)", p.OSPFRouteType)
	}
	if got := p.Prefix.String(); got != "10.255.0.5/32" {
		t.Errorf("Prefix = %s, want 10.255.0.5/32", got)
	}
}

// prefixAttrHex is a BGP-LS Attribute (path attribute 29) value carrying the
// three prefix TLVs XRd 26.1.1 was observed sending alongside a Prefix NLRI
// on 2026-08-17. Each TLV's value here is a real observed
// one, read out of the archive's unknown_tlvs map (where this build was
// dropping them) rather than composed from a reading of RFC 9085:
//
//	1170 Prefix Attribute Flags = 0x40
//	1155 Prefix Metric          = 0x00000028 (40)
//	1158 Prefix-SID             = flags 0, algorithm 0, reserved 0, SID 5
//
// SID 5 is the check that matters: xr-p2's Loopback0 is configured
// "prefix-sid index 5", so a decoder that reports 5 is agreeing with the
// router's own configuration and not merely with itself.
const prefixAttrHex = "04920001" + "40" +
	"04830004" + "00000028" +
	"04860008" + "0000000000000005"

func TestDecodeLsAttrsPrefixTLVs(t *testing.T) {
	b, err := hex.DecodeString(prefixAttrHex)
	if err != nil {
		t.Fatal(err)
	}
	a, err := DecodeLsAttrs(b)
	if err != nil {
		t.Fatalf("DecodeLsAttrs: %v", err)
	}
	if a.PrefixMetric != 40 {
		t.Errorf("PrefixMetric = %d, want 40", a.PrefixMetric)
	}
	if a.PrefixSID != 5 {
		t.Errorf("PrefixSID = %d, want 5 (xr-p2's configured prefix-sid index)", a.PrefixSID)
	}
	if a.PrefixSIDFlags != 0 {
		t.Errorf("PrefixSIDFlags = %#x, want 0", a.PrefixSIDFlags)
	}
	if !a.HasPrefixSID {
		t.Error("HasPrefixSID = false, want true -- index 0 is a legal SID and must be distinguishable from an absent TLV")
	}
	if a.PrefixAttrFlags != 0x40 {
		t.Errorf("PrefixAttrFlags = %#x, want 0x40", a.PrefixAttrFlags)
	}
	if len(a.Unknown) != 0 {
		t.Errorf("Unknown = %v, want empty -- all three TLVs should now decode", a.Unknown)
	}
}

// TestAppendLsNLRIRoundTrip covers building BGP-LS Node NLRI, the last family
// the synthetic generator could not produce.
//
// BGP-LS is the family where a builder is most likely to be subtly wrong and
// still "work": the NLRI is nested TLVs (an NLRI header, then a Local Node
// Descriptor TLV, then its own sub-TLVs), and every level carries its own
// type/length pair. A length computed at the wrong nesting level yields bytes
// that still walk cleanly but describe a different node.
//
// So the assertion is a round trip through DecodeLsNLRI, which is proven
// against real IOS-XR and NX-OS link-state captures in the corpus.
func TestAppendLsNLRIRoundTrip(t *testing.T) {
	want := []LsNodeNLRI{
		{
			Protocol: 2, Identifier: 0,
			Local: LsNodeDescriptor{
				ASN: 65000, BGPLSID: 0, Area: 0,
				RouterID: []byte{10, 255, 0, 5}, // OSPF: a 4-byte router ID
			},
		},
		{
			Protocol: 2, Identifier: 7,
			Local: LsNodeDescriptor{
				ASN: 65100, BGPLSID: 1, Area: 1,
				RouterID: []byte{10, 255, 1, 1},
			},
		},
	}

	raw := AppendLsNodeNLRI(nil, want)
	nodes, links, prefixes, undecoded, err := DecodeLsNLRI(raw)
	if err != nil {
		t.Fatalf("DecodeLsNLRI on builder output: %v", err)
	}
	if undecoded != 0 {
		t.Errorf("decoder reported %d undecoded NLRI; the builder emitted something it cannot type", undecoded)
	}
	if len(links) != 0 || len(prefixes) != 0 {
		t.Errorf("got %d links and %d prefixes, want none", len(links), len(prefixes))
	}
	if len(nodes) != len(want) {
		t.Fatalf("got %d nodes, want %d", len(nodes), len(want))
	}
	for i, w := range want {
		g := nodes[i]
		if g.Protocol != w.Protocol || g.Identifier != w.Identifier {
			t.Errorf("[%d] protocol/identifier = %d/%d, want %d/%d",
				i, g.Protocol, g.Identifier, w.Protocol, w.Identifier)
		}
		if g.Local.ASN != w.Local.ASN || g.Local.BGPLSID != w.Local.BGPLSID || g.Local.Area != w.Local.Area {
			t.Errorf("[%d] descriptor = asn %d bgplsid %d area %d, want %d/%d/%d",
				i, g.Local.ASN, g.Local.BGPLSID, g.Local.Area,
				w.Local.ASN, w.Local.BGPLSID, w.Local.Area)
		}
		if string(g.Local.RouterID) != string(w.Local.RouterID) {
			t.Errorf("[%d] router id = %v, want %v", i, g.Local.RouterID, w.Local.RouterID)
		}
		if len(g.Unknown) != 0 {
			t.Errorf("[%d] decoder saw unknown TLVs %v; the builder emitted a TLV type it should not have",
				i, g.Unknown)
		}
	}
}

// prefixV6NLRIHex is a real IPv6 Topology Prefix NLRI (type 4), lifted
// verbatim out of ls_events.raw_reach where this build has been leaving them
// since 2026-08-29 19:00, when the archive began carrying IPv6 IS-IS.
// It is one of 74 in the archive, from router 10.0.103.73 via peer
// 10.1.0.17.
//
// It is captured rather than composed, and that is the whole point: the
// reason type 4 went undecoded for two weeks was that writing it from RFC
// 9552 alone would mean testing a reading of the spec against fixtures built
// from that same reading. These bytes came off a router.
//
// Decoded by hand before the decoder existed: type 4, length 0x3A (58),
// protocol 2 (IS-IS Level 2), identifier 100, local node descriptor AS 65000
// / BGP-LS ID 0 / IGP router-ID 01:02:55:00:00:05 -- six bytes, an IS-IS
// system ID rather than an address, which is why this test asserts its hex
// and not a netip.Addr. Then TLV 263 (Multi-Topology ID) = 2, and TLV 265
// (IP Reachability) = prefix length 0x40 (64) with the eight packed bytes
// 20 01 0D B8 55 49 00 04 -- 2001:db8:5549:4::/64.
//
// The MT-ID is worth noting: lsTLVMultiTopoID's own comment says the lab is
// single-topology and no real example exists to decode against. This NLRI
// carries one, so that claim has expired too -- but decoding it is not this
// change, and 263 stays in Unknown here.
const prefixV6NLRIHex = "0004003A02" + "0000000000000064" +
	"0100001A" + "020000040000FDE8" + "0201000400000000" +
	"02030006" + "010255000005" +
	"010700020002" +
	"01090009" + "40" + "20010DB855490004"

func TestDecodeLsNLRIPrefixV6(t *testing.T) {
	b, err := hex.DecodeString(prefixV6NLRIHex)
	if err != nil {
		t.Fatal(err)
	}
	nodes, links, prefixes, undecoded, err := DecodeLsNLRI(b)
	if err != nil {
		t.Fatalf("DecodeLsNLRI: %v", err)
	}
	if len(nodes) != 0 || len(links) != 0 || undecoded != 0 {
		t.Fatalf("got %d nodes, %d links, %d undecoded; want 0, 0, 0 -- a type-4 "+
			"NLRI counted as undecoded means the case was never added",
			len(nodes), len(links), undecoded)
	}
	if len(prefixes) != 1 {
		t.Fatalf("got %d prefixes, want 1", len(prefixes))
	}
	p := prefixes[0]
	if p.Protocol != 2 {
		t.Errorf("Protocol = %d, want 2 (IS-IS Level 2)", p.Protocol)
	}
	if p.Identifier != 100 {
		t.Errorf("Identifier = %d, want 100", p.Identifier)
	}
	if p.Local.ASN != 65000 {
		t.Errorf("local ASN = %d, want 65000", p.Local.ASN)
	}
	if got := hex.EncodeToString(p.Local.RouterID); got != "010255000005" {
		t.Errorf("local router-id = %s, want 010255000005 -- six bytes, an "+
			"IS-IS system ID rather than an address", got)
	}
	// The assertion this test exists for. A decoder that capped at 32 bits
	// would have errored out long before here; one that built a 4-byte
	// address from eight packed bytes would report a v4 prefix.
	if got := p.Prefix.String(); got != "2001:db8:5549:4::/64" {
		t.Errorf("Prefix = %s, want 2001:db8:5549:4::/64", got)
	}
	if !p.Prefix.Addr().Is6() {
		t.Errorf("Prefix.Addr() is not IPv6 (%s); a type-4 NLRI decoded into a "+
			"v4 address means the family never reached decodeIPReach", p.Prefix.Addr())
	}
	// 263 is carried, not decoded -- see the const's comment.
	if _, ok := p.Unknown[263]; !ok {
		t.Errorf("TLV 263 (Multi-Topology ID) is not in Unknown; this NLRI " +
			"carries one and unrecognized TLVs must be kept, not dropped")
	}
}

// TestDecodeLsNLRIPrefixV6RejectsOversizedLength is decodeIPReach's new upper
// bound. 129 exceeds an IPv6 prefix just as 33 exceeded an IPv4 one, and the
// v4 path must keep its own 32-bit cap rather than inheriting 128 -- a type-3
// NLRI claiming /64 is malformed, and accepting it would build a v4 address
// out of eight bytes.
func TestDecodeLsNLRIPrefixV6RejectsOversizedLength(t *testing.T) {
	for _, tc := range []struct {
		name, nlri string
	}{
		{"v6 over 128", "0004001902" + "0000000000000064" +
			"0100000C" + "020000040000FDE8" +
			"010900028100"},
		{"v3 over 32", "0003001903" + "0000000000000064" +
			"0100000C" + "020000040000FDE8" +
			"010900024000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := hex.DecodeString(tc.nlri)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := DecodeLsNLRI(b); err == nil {
				t.Error("an out-of-range prefix length was accepted")
			}
		})
	}
}
