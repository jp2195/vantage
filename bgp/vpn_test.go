package bgp

import (
	"errors"
	"net/netip"
	"testing"
)

// TestParseVpnNLRI uses a hand-built entry derived from RFC 4364 §4.3.4:
// length byte (bits), 3-byte label, 8-byte RD, then the prefix bytes.
//
//	label 24001 with BoS      -> 0x05 0xDC 0x11         (24 bits)
//	RD type 0, 65000:100      -> 00 00 FD E8 00 00 00 64 (64 bits)
//	prefix 10.0.0.0/24        -> 0A 00 00               (24 bits)
//	total = 24 + 64 + 24      = 112 bits = 0x70
func TestParseVpnNLRI(t *testing.T) {
	b := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	got, err := parseVpnNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d prefixes, want 1", len(got))
	}
	p := got[0]
	if p.Prefix != netip.MustParsePrefix("10.0.0.0/24") {
		t.Fatalf("prefix = %v", p.Prefix)
	}
	if p.RD != "65000:100" {
		t.Fatalf("rd = %q", p.RD)
	}
	if len(p.Labels) != 1 || p.Labels[0] != 24001 {
		t.Fatalf("labels = %v", p.Labels)
	}
}

func TestParseVpnNLRIAddPath(t *testing.T) {
	b := []byte{
		0x00, 0x00, 0x00, 0x07, // path id 7
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	got, err := parseVpnNLRI(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PathID != 7 {
		t.Fatalf("got %+v, want one entry with PathID 7", got)
	}
}

// TestParseLabeledNLRI covers RFC 8277: label stack then prefix, no RD.
//
//	label 100 with BoS -> 0x00 0x06 0x41 (24 bits)
//	prefix 192.0.2.0/24 -> C0 00 02      (24 bits)
//	total = 48 bits = 0x30
func TestParseLabeledNLRI(t *testing.T) {
	b := []byte{0x30, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02}
	got, err := parseLabeledNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Prefix != netip.MustParsePrefix("192.0.2.0/24") {
		t.Fatalf("got %+v", got)
	}
	if got[0].RD != "" {
		t.Fatalf("lu4 must not carry an RD, got %q", got[0].RD)
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != 100 {
		t.Fatalf("labels = %v", got[0].Labels)
	}
}

// TestParseVpnNLRIDefaultRoute checks the /0 edge of the prefix-length
// arithmetic: prefixBits = totalBits - 24*labelCount - 64 must come out to
// exactly 0, and a 0-bit prefix must consume zero prefix bytes rather than
// underflow or over-read.
//
//	label 24001 with BoS -> 0x05 0xDC 0x11          (24 bits)
//	RD type 0, 65000:100 -> 00 00 FD E8 00 00 00 64  (64 bits)
//	prefix 0.0.0.0/0     -> (no bytes)                (0 bits)
//	total = 24 + 64 + 0  = 88 bits = 0x58
func TestParseVpnNLRIDefaultRoute(t *testing.T) {
	b := []byte{
		0x58,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
	}
	got, err := parseVpnNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Prefix != netip.MustParsePrefix("0.0.0.0/0") {
		t.Fatalf("got %+v, want 0.0.0.0/0", got)
	}
	if got[0].RD != "65000:100" {
		t.Fatalf("rd = %q", got[0].RD)
	}
}

// TestParseLabeledNLRIHostRoute checks the /32 edge: prefixBits = 32 must
// consume exactly 4 prefix bytes (the full IPv4 address), the family
// maximum rather than one past it.
//
//	label 100 with BoS      -> 0x00 0x06 0x41 (24 bits)
//	prefix 192.0.2.1/32     -> C0 00 02 01    (32 bits)
//	total = 24 + 32         = 56 bits = 0x38
func TestParseLabeledNLRIHostRoute(t *testing.T) {
	b := []byte{0x38, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02, 0x01}
	got, err := parseLabeledNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Prefix != netip.MustParsePrefix("192.0.2.1/32") {
		t.Fatalf("got %+v, want 192.0.2.1/32", got)
	}
}

// TestParseLabeledNLRITwoLabels checks that the bit accounting generalizes
// to a multi-label stack: usedBits comes from decodeLabels' actual bytes
// consumed (n*8), not a hardcoded assumption of one label. The label bytes
// here are the exact fixture nlri_test.go's TestDecodeLabels already proves
// correct for the two-label case (100 then 200), reused rather than
// re-derived so a hand-arithmetic slip here can't quietly agree with a
// matching slip in a freshly-derived fixture.
//
//	label 100 (no BoS)   -> 0x00 0x06 0x40 (24 bits)
//	label 200 (BoS)      -> 0x00 0x0C 0x81 (24 bits)
//	prefix 172.16.0.0/16 -> AC 10          (16 bits)
//	total = 24 + 24 + 16 = 64 bits = 0x40
func TestParseLabeledNLRITwoLabels(t *testing.T) {
	b := []byte{0x40, 0x00, 0x06, 0x40, 0x00, 0x0C, 0x81, 0xAC, 0x10}
	got, err := parseLabeledNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d prefixes, want 1", len(got))
	}
	p := got[0]
	if p.Prefix != netip.MustParsePrefix("172.16.0.0/16") {
		t.Fatalf("prefix = %v", p.Prefix)
	}
	if len(p.Labels) != 2 || p.Labels[0] != 100 || p.Labels[1] != 200 {
		t.Fatalf("labels = %v, want [100 200]", p.Labels)
	}
}

func TestParseVpnNLRIRejectsMalformed(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      []byte
		wantErr error
	}{
		{"truncated mid-entry", []byte{0x70, 0x05, 0xDC}, ErrNLRITruncated},
		{"bits exceed buffer", []byte{0xFF, 0x05, 0xDC, 0x11}, ErrNLRITruncated},
		// 24 bits total cannot hold a 24-bit label plus a 64-bit RD.
		// ErrNLRIBadLength, not Truncated: the entry's body is capped at its
		// own declared length, so this is a declaration that cannot be right
		// rather than a short read. More wire bytes would not complete it.
		{"bits too small for label+rd", []byte{0x18, 0x05, 0xDC, 0x11}, ErrNLRIBadLength},
		// label(24) + RD(64) + a 33-bit prefix = 121 bits = 0x79. All 16
		// declared bytes are present (this is not a truncation case), so the
		// only thing that can reject it is the prefix-length range check: a
		// /33 is impossible for IPv4 and must not be used to size a copy.
		{"prefix longer than family max", []byte{0x79, 0x05, 0xDC, 0x11,
			0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64, 0x0A, 0, 0, 0, 0}, ErrNLRIBadLength},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseVpnNLRI(c.in, false)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("parseVpnNLRI(%x) err = %v, want %v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestParseVpnNLRIEmpty(t *testing.T) {
	got, err := parseVpnNLRI(nil, false)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty NLRI: got=%v err=%v, want no prefixes and no error", got, err)
	}
}

func FuzzParseVpnNLRI(f *testing.F) {
	f.Add([]byte{0x70, 0x05, 0xDC, 0x11, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64, 0x0A, 0x00, 0x00}, false)
	f.Add([]byte{0x30, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02}, true)
	f.Add([]byte{0x58, 0x05, 0xDC, 0x11, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64}, false)
	f.Add([]byte{0x40, 0x00, 0x06, 0x40, 0x00, 0x0C, 0x81, 0xAC, 0x10}, false)
	f.Add([]byte{}, false)
	// Hostile seeds: a length byte claiming far more than is present, and an
	// entry whose bit accounting yields a /33.
	f.Add([]byte{0xFF, 0x05, 0xDC, 0x11}, false)
	f.Add([]byte{0x79, 0x05, 0xDC, 0x11, 0x00, 0x00, 0xFD, 0xE8,
		0x00, 0x00, 0x00, 0x64, 0x0A, 0x00, 0x00, 0x00, 0x00}, false)
	f.Fuzz(func(t *testing.T, b []byte, addPath bool) {
		_, _ = parseVpnNLRI(b, addPath) // must never panic
		_, _ = parseLabeledNLRI(b, addPath)
	})
}

// TestParseVpnNLRIMultiEntry walks several entries in one buffer, mirroring
// real MP_REACH NLRI traffic -- an MP_REACH NLRI field routinely carries
// more than one. Every other functional test here feeds a single entry,
// so entry-boundary drift from a bit/byte confusion would only ever show up
// here. The first entry is deliberately NOT byte-aligned (/20), since that is
// where an off-by-one in the bit accounting would surface.
func TestParseVpnNLRIMultiEntry(t *testing.T) {
	var b []byte
	// /20: label(24) + RD(64) + 20 = 108 bits = 0x6C, body 14 bytes.
	b = append(b, 0x6C, 0x05, 0xDC, 0x11)
	b = append(b, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64)
	b = append(b, 0x0A, 0x01, 0x10)
	// /24: label(24) + RD(64) + 24 = 112 bits = 0x70, body 15 bytes.
	b = append(b, 0x70, 0x05, 0xDC, 0x11)
	b = append(b, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64)
	b = append(b, 0xC0, 0x00, 0x02)

	got, err := parseVpnNLRI(b, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Prefix != netip.MustParsePrefix("10.1.16.0/20") {
		t.Fatalf("entry 0 = %v, want 10.1.16.0/20", got[0].Prefix)
	}
	if got[1].Prefix != netip.MustParsePrefix("192.0.2.0/24") {
		t.Fatalf("entry 1 = %v, want 192.0.2.0/24", got[1].Prefix)
	}
}

// TestParseLabeledNLRIRejectsMalformed covers the lu4 error paths, where the
// prefix-length arithmetic has no 64-bit RD term -- the table above exercises
// only parseVpnNLRI, so these branches were reached solely by fuzzing.
func TestParseLabeledNLRIRejectsMalformed(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      []byte
		wantErr error
	}{
		{"bits exceed buffer", []byte{0xFF, 0x00, 0x06, 0x41}, ErrNLRITruncated},
		// label(24) + a 33-bit prefix = 57 bits = 0x39, body 8 bytes, all present.
		{"prefix longer than family max", []byte{0x39, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02, 0x00, 0x00}, ErrNLRIBadLength},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseLabeledNLRI(c.in, false)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("parseLabeledNLRI(%x) err = %v, want %v", c.in, err, c.wantErr)
			}
		})
	}
}
