package bgp

import (
	"errors"
	"testing"
)

// TestDecodeRD covers all three RFC 4364 §4.2 type encodings with byte
// fixtures written out from the RFC, not produced by our own encoder -- a
// round-trip cannot catch a misreading both sides share.
func TestDecodeRD(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want string
	}{
		// Type 0: 2-byte administrator (ASN) + 4-byte assigned number.
		{"type 0", []byte{0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64}, "65000:100"},
		// Type 1: 4-byte administrator (IPv4) + 2-byte assigned number.
		{"type 1", []byte{0x00, 0x01, 0x0A, 0x00, 0x00, 0x01, 0x00, 0x01}, "10.0.0.1:1"},
		// Type 2: 4-byte administrator (4-byte ASN) + 2-byte assigned number.
		{"type 2", []byte{0x00, 0x02, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01}, "65536:1"},
		{"all zero", []byte{0, 0, 0, 0, 0, 0, 0, 0}, "0:0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeRD(c.in)
			if err != nil {
				t.Fatalf("decodeRD(%x): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("decodeRD(%x) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDecodeRDRejectsShortAndUnknownType(t *testing.T) {
	if _, err := decodeRD([]byte{0, 0, 0}); !errors.Is(err, ErrNLRITruncated) {
		t.Fatalf("short RD: err=%v, want ErrNLRITruncated", err)
	}
	// Unknown type must not be silently rendered as some other type's shape.
	// Pinned to ErrNLRIBadLength rather than just "an error": the two
	// sentinels mean different things to a caller (ran out of bytes vs.
	// structurally impossible), and nothing else fixes which one this is.
	if _, err := decodeRD([]byte{0x00, 0x09, 1, 2, 3, 4, 5, 6}); !errors.Is(err, ErrNLRIBadLength) {
		t.Fatalf("unknown RD type: err=%v, want ErrNLRIBadLength", err)
	}
}

// TestDecodeLabels pins the 3-byte label encoding: 20-bit label in the high
// bits, then 3 bits TC, then the bottom-of-stack bit.
func TestDecodeLabels(t *testing.T) {
	// 24001 = 0x5DC1. Shifted left 4 with BoS set: 0x05 0xDC 0x11.
	one := []byte{0x05, 0xDC, 0x11}
	got, n, err := decodeLabels(one)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(got) != 1 || got[0] != 24001 {
		t.Fatalf("single label: got=%v n=%d, want [24001] n=3", got, n)
	}

	// Two-label stack: 100 (BoS clear), then 200 (BoS set).
	// 100 = 0x64 -> 0x00 0x06 0x40 ; 200 = 0xC8 -> 0x00 0x0C 0x81
	two := []byte{0x00, 0x06, 0x40, 0x00, 0x0C, 0x81}
	got, n, err = decodeLabels(two)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 || len(got) != 2 || got[0] != 100 || got[1] != 200 {
		t.Fatalf("two-label stack: got=%v n=%d, want [100 200] n=6", got, n)
	}
}

// TestDecodeLabelsWithdrawSentinel pins RFC 3107/8277's withdraw label. It is
// matched on the raw three bytes, not on the shifted value.
func TestDecodeLabelsWithdrawSentinel(t *testing.T) {
	got, n, err := decodeLabels([]byte{0x80, 0x00, 0x00})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(got) != 1 || got[0] != withdrawLabel>>4 {
		t.Fatalf("withdraw sentinel: got=%v n=%d", got, n)
	}
}

func TestDecodeLabelsTruncated(t *testing.T) {
	// Fewer than 3 bytes cannot hold a label.
	if _, _, err := decodeLabels([]byte{0x05, 0xDC}); !errors.Is(err, ErrNLRITruncated) {
		t.Fatalf("err=%v, want ErrNLRITruncated", err)
	}
	// A stack that never sets bottom-of-stack must terminate rather than run
	// off the end -- this is the classic unbounded-loop shape.
	never := []byte{0x00, 0x06, 0x40, 0x00, 0x06, 0x40}
	if _, _, err := decodeLabels(never); !errors.Is(err, ErrNLRITruncated) {
		t.Fatalf("no-BoS stack: err=%v, want ErrNLRITruncated", err)
	}
}

func FuzzDecodeRD(f *testing.F) {
	f.Add([]byte{0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64})
	f.Add([]byte{0x00, 0x01, 0x0A, 0x00, 0x00, 0x01, 0x00, 0x01})
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0xFF, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeRD(b) // must never panic
	})
}

func FuzzDecodeLabels(f *testing.F) {
	f.Add([]byte{0x05, 0xDC, 0x11})
	f.Add([]byte{0x80, 0x00, 0x00})
	f.Add([]byte{0x00, 0x06, 0x40, 0x00, 0x06, 0x40})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		labels, n, err := decodeLabels(b)
		if err == nil && n > len(b) {
			t.Fatalf("consumed %d bytes from a %d-byte input", n, len(b))
		}
		if err == nil && len(labels) == 0 {
			t.Fatal("success with no labels")
		}
	})
}

// TestDecodeLabelsDepthBound reaches the maxLabelStack ceiling, which the
// existing no-bottom-of-stack test does not: that one supplies 6 bytes and so
// trips the byte-availability guard first. A deep stack with bytes to spare is
// structurally wrong rather than truncated, so it reports ErrNLRIBadLength.
func TestDecodeLabelsDepthBound(t *testing.T) {
	// 32 labels, none with the bottom-of-stack bit set.
	var deep []byte
	for range 32 {
		deep = append(deep, 0x00, 0x06, 0x40)
	}
	_, _, err := decodeLabels(deep)
	if !errors.Is(err, ErrNLRIBadLength) {
		t.Fatalf("deep stack with bytes remaining: err=%v, want ErrNLRIBadLength", err)
	}
	if errors.Is(err, ErrNLRITruncated) {
		t.Fatal("a depth-bound rejection must not claim the input was truncated")
	}
}

// TestFamilyHasDecoderMatchesDecodeNLRI pins the invariant that the two
// switches in this file agree: familyHasDecoder answers "is there a decodeNLRI
// case for fam", and parseMPReach/parseMPUnreach trust that answer when
// choosing between PARSE_FLAG_UNKNOWN_FAMILY and PARSE_FLAG_NLRI_UNTYPED. A
// family listed in one switch but not the other is silently wrong in one of
// two directions: named-but-undecodable (every NLRI kept raw under
// UNKNOWN_FAMILY, writing no row) or decodable-but-misflagged.
//
// Both directions have shipped. ipv6u and vpn6 were named by the sink's row
// builder while decodeNLRI had no AFI 2 case at all, so every one of their
// NLRI decoded to nothing for as long as that held (until 2026-08-28);
// BGP-LS had the same shape before its decoder landed (2026-08-13). Neither was caught by a
// test -- both surfaced only when someone read captured rows and noticed the
// flag. This asserts the property directly so the next one fails here first.
func TestFamilyHasDecoderMatchesDecodeNLRI(t *testing.T) {
	// Every family this package intends to decode into typed NLRI. IPv4
	// unicast is absent deliberately: parseMPReach handles it on its own
	// branch and never consults decodeNLRI for it.
	families := map[string]Family{
		"ipv6u": FamilyIPv6U,
		"vpn4":  FamilyVPNv4,
		"vpn6":  FamilyVPNv6,
		"lu4":   FamilyLU4,
		"evpn":  FamilyEVPN,
		"bgpls": FamilyBGPLS,
	}
	for name, fam := range families {
		t.Run(name, func(t *testing.T) {
			// An empty NLRI is the End-of-RIB shape (RFC 4724 §2) and is
			// valid for every family here, so `decoded` reports whether a
			// case exists rather than whether these particular bytes parsed.
			_, _, _, _, _, _, _, decoded, err := decodeNLRI(fam, nil, false)
			if err != nil {
				t.Fatalf("decodeNLRI(%v, empty): unexpected error: %v", fam, err)
			}
			if !decoded {
				t.Errorf("decodeNLRI has no case for %v: its NLRI would be kept raw under PARSE_FLAG_UNKNOWN_FAMILY and write no row", fam)
			}
			if got := familyHasDecoder(fam); got != decoded {
				t.Errorf("familyHasDecoder(%v) = %v, decodeNLRI reports decoded = %v: the two switches disagree", fam, got, decoded)
			}
		})
	}
}
