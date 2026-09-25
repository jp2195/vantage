package bgp

import (
	"encoding/hex"
	"testing"
)

// mustHex decodes a wire-form extended community written as 16 hex digits.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 8 {
		t.Fatalf("bad test vector %q: %v (%d bytes)", s, err, len(b))
	}
	return b
}

// TestDecodeExtCommShapes pins RFC 4360 §3: the administrator field's
// layout is chosen by the high-order type octet, so masking the transitive
// bit gives the same three shapes for transitive and non-transitive
// communities alike. The rendered text must match decodeRD (nlri.go) --
// a route target and a route distinguisher that read "65000:100" have to
// be the same string, or joining routes to VRFs becomes string surgery.
func TestDecodeExtCommShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
		typ  uint32
		sub  uint32
		val  string
	}{
		{"2-byte AS route target", "0002fde800000064", 0x00, 0x02, "65000:100"},
		{"IPv4 route target", "01020a0000010005", 0x01, 0x02, "10.0.0.1:5"},
		{"4-byte AS route target", "0202fa56ea010005", 0x02, 0x02, "4200000001:5"},
		{"non-transitive 2-byte AS", "4004fde800000064", 0x40, 0x04, "65000:100"},
		{"non-transitive IPv4", "41020a0000010005", 0x41, 0x02, "10.0.0.1:5"},
		{"opaque, unmodelled subtype", "0311000000000008", 0x03, 0x11, "0x000000000008"},
		{"unmodelled type falls back to hex", "7f11deadbeefcafe", 0x7f, 0x11, "0xdeadbeefcafe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := mustHex(t, tc.wire)
			ec := decodeExtComm(b)
			if ec.GetType() != tc.typ || ec.GetSubType() != tc.sub {
				t.Errorf("type/sub = %#x/%#x, want %#x/%#x", ec.GetType(), ec.GetSubType(), tc.typ, tc.sub)
			}
			if ec.GetValue() != tc.val {
				t.Errorf("value = %q, want %q", ec.GetValue(), tc.val)
			}
		})
	}
}

// TestDecodeExtCommKeepsRaw pins the lossless-fallback rule: the wire bytes
// survive decoding, so a rendering found wrong later is fixable from stored
// data instead of needing a re-capture.
func TestDecodeExtCommKeepsRaw(t *testing.T) {
	b := mustHex(t, "0002fde800000064")
	ec := decodeExtComm(b)
	if hex.EncodeToString(ec.GetRaw()) != "0002fde800000064" {
		t.Fatalf("raw = %x, want the 8 wire bytes", ec.GetRaw())
	}
	b[7] = 0xff // decodeExtComm must have copied, not aliased the caller's slice
	if hex.EncodeToString(ec.GetRaw()) != "0002fde800000064" {
		t.Fatalf("raw aliases the input buffer: %x", ec.GetRaw())
	}
}

// TestDecodeExtCommRTMatchesDecodeRD is the constraint stated as a test:
// the same administrator fields must render identically whether they are
// read as a route target or as a route distinguisher.
//
// The two wire forms are NOT the same bytes, and assuming they were is the
// trap here. An RD's type is a two-byte field (RFC 4364 §4.2), so type 0
// is "0000"; an extended community spends those same two bytes on a
// one-byte type and a one-byte subtype, so the matching route target is
// "0002". Read the RT's bytes as an RD and you get 4259840000:100, not
// 65000:100 -- which is why this test pairs the two encodings of one value
// rather than round-tripping one buffer through both decoders.
func TestDecodeExtCommRTMatchesDecodeRD(t *testing.T) {
	for _, tc := range []struct {
		shape          uint8
		admin          string // the six administrator/assigned bytes, hex
		rdWire, rtWire string
	}{
		{0x00, "fde800000064", "0000fde800000064", "0002fde800000064"},
		{0x01, "0a0000010005", "00010a0000010005", "01020a0000010005"},
		{0x02, "fa56ea010005", "0002fa56ea010005", "0202fa56ea010005"},
	} {
		rd, err := decodeRD(mustHex(t, tc.rdWire))
		if err != nil {
			t.Fatalf("decodeRD(%s): %v", tc.rdWire, err)
		}
		if got := decodeExtComm(mustHex(t, tc.rtWire)).GetValue(); got != rd {
			t.Errorf("shape %#x: route target %q != route distinguisher %q", tc.shape, got, rd)
		}
	}
}

// TestExtCommName pins the (type, sub_type) lookup. The name is derivable,
// so the envelope does not store it; this is the one definition, shared by
// the sink's rendering and any operator tooling.
//
// The lookup switches on the full type octet rather than masking the
// transitive bit, because the registry does: a route target is defined for
// the transitive types only, and link bandwidth exists only as the
// non-transitive 0x40. Masking would confidently name communities that do
// not exist.
func TestExtCommName(t *testing.T) {
	for _, tc := range []struct {
		typ, sub uint8
		want     string
	}{
		{0x00, 0x02, "rt"},
		{0x01, 0x02, "rt"},
		{0x02, 0x02, "rt"},
		{0x00, 0x03, "soo"},
		{0x03, 0x0b, "color"},
		{0x03, 0x0c, "encap"},
		{0x06, 0x00, "mac-mobility"},
		{0x06, 0x01, "esi-label"},
		{0x06, 0x02, "es-import-rt"},
		{0x06, 0x03, "router-mac"},
		{0x06, 0x0d, "default-gw"},
		{0x40, 0x04, "link-bw"},
		{0x80, 0x06, "flowspec-traffic-rate"},
		{0x80, 0x09, "flowspec-traffic-marking"},
		{0x42, 0x02, ""}, // non-transitive 4-byte AS: no route target is defined
		{0x7f, 0x11, ""}, // unmodeled
	} {
		if got := ExtCommName(tc.typ, tc.sub); got != tc.want {
			t.Errorf("ExtCommName(%#x, %#x) = %q, want %q", tc.typ, tc.sub, got, tc.want)
		}
	}
}

// TestDecodeExtCommSpecialRenderings covers the three subtypes whose layout
// is known and whose meaning is the point of the community. Every vector
// here is bytes a real router sent: the router MAC and sticky MAC mobility
// come from the NX-OS EVPN captures, the VXLAN encapsulation from both.
func TestDecodeExtCommSpecialRenderings(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
		val  string
	}{
		{"router MAC, from n9kv leaf", "06035231d4031b08", "52:31:d4:03:1b:08"},
		{"router MAC, from n9kv spine", "0603529128f71b08", "52:91:28:f7:1b:08"},
		{"MAC mobility, sticky, from n9kv", "0600010000000000", "sticky,seq=0"},
		{"MAC mobility, moved twice", "0600000000000002", "seq=2"},
		{"encapsulation VXLAN, from n9kv", "030c000000000008", "vxlan"},
		{"encapsulation, unmodelled tunnel type", "030c000000000063", "99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeExtComm(mustHex(t, tc.wire)).GetValue(); got != tc.val {
				t.Errorf("value = %q, want %q", got, tc.val)
			}
		})
	}
}

// TestDecodeExtCommSpecialKeepsRaw guards the rule that a purpose-built
// rendering is still not allowed to discard evidence.
func TestDecodeExtCommSpecialKeepsRaw(t *testing.T) {
	ec := decodeExtComm(mustHex(t, "06035231d4031b08"))
	if hex.EncodeToString(ec.GetRaw()) != "06035231d4031b08" {
		t.Fatalf("raw = %x", ec.GetRaw())
	}
}

// TestExtCommNameOSPFSubtypes pins the three OSPF extended communities that
// a PE running OSPF inside a VRF and redistributing it into BGP attaches to
// every route it originates. They are in
// iosxr/xrd-26.1.1-p2-ospf-extcomm.bmpcap, and until this they decoded to
// nothing:
//
//	0005 0000 00006500   OSPF Domain Identifier      (RFC 4577 4.2.1)
//	8000 00000000 01 00  OSPF Route Type: area 0, type 1, options 0x0
//	8001 c0a87101 0000   OSPF Router ID:  192.168.113.1
//
// The last two use 0x80, the pre-standard Cisco codepoints, and not RFC
// 4577's 0x0306 and 0x0107. That is what IOS-XR 26.1.1 actually emits --
// confirmed by decoding the capture and by the router's own rendering of
// the same route ("OSPF domain-id:0x5:0x000000006500 OSPF route-type:0:1:0x0
// OSPF router-id:192.168.113.1"). The standard codepoints are deliberately
// NOT added here: this package's convention, stated in ExtCommName's doc
// comment, is to separate what traffic has verified from what is transcribed
// from the registry, and no capture in this repo carries them.
func TestExtCommNameOSPFSubtypes(t *testing.T) {
	for _, c := range []struct {
		typ, sub uint8
		want     string
	}{
		// Domain Identifier is assigned on all three AS/IPv4 types.
		{0x00, 0x05, "ospf-domain-id"},
		{0x01, 0x05, "ospf-domain-id"},
		{0x02, 0x05, "ospf-domain-id"},
		{0x80, 0x00, "ospf-route-type"},
		{0x80, 0x01, "ospf-router-id"},
	} {
		if got := ExtCommName(c.typ, c.sub); got != c.want {
			t.Errorf("ExtCommName(0x%02x, 0x%02x) = %q, want %q", c.typ, c.sub, got, c.want)
		}
	}
}

// TestExtCommNameOSPFDoesNotShadowFlowSpec guards the choice above. Type
// 0x80 is the generic transitive experimental type that RFC 8955 flow spec
// uses, so naming subtypes 0x00 and 0x01 under it must not disturb the
// flow-spec subtypes that already live there.
func TestExtCommNameOSPFDoesNotShadowFlowSpec(t *testing.T) {
	for _, c := range []struct {
		sub  uint8
		want string
	}{
		{0x06, "flowspec-traffic-rate"},
		{0x07, "flowspec-traffic-action"},
		{0x08, "flowspec-redirect"},
		{0x09, "flowspec-traffic-marking"},
	} {
		if got := ExtCommName(0x80, c.sub); got != c.want {
			t.Errorf("ExtCommName(0x80, 0x%02x) = %q, want %q", c.sub, got, c.want)
		}
	}
}
