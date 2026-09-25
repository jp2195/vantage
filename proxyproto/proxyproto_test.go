package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// v2Header builds a version-2 header: verCmd and famProto are the two nibble
// bytes, body is everything the length field covers (address block plus any
// TLVs).
func v2Header(verCmd, famProto byte, body []byte) []byte {
	h := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}
	h = append(h, verCmd, famProto)
	h = binary.BigEndian.AppendUint16(h, uint16(len(body)))
	return append(h, body...)
}

// v2INET builds the 12-byte AF_INET address block: src, dst, srcport, dstport.
func v2INET(src, dst netip.Addr, srcPort, dstPort uint16) []byte {
	b := src.As4()
	d := dst.As4()
	out := append(append([]byte{}, b[:]...), d[:]...)
	out = binary.BigEndian.AppendUint16(out, srcPort)
	return binary.BigEndian.AppendUint16(out, dstPort)
}

// v2INET6 builds the 36-byte AF_INET6 address block.
func v2INET6(src, dst netip.Addr, srcPort, dstPort uint16) []byte {
	b := src.As16()
	d := dst.As16()
	out := append(append([]byte{}, b[:]...), d[:]...)
	out = binary.BigEndian.AppendUint16(out, srcPort)
	return binary.BigEndian.AppendUint16(out, dstPort)
}

func TestReadAccepts(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want netip.Addr
	}{
		{
			name: "v1 TCP4",
			in:   []byte("PROXY TCP4 10.0.0.80 10.0.0.1 54321 11019\r\n"),
			want: netip.MustParseAddr("10.0.0.80"),
		},
		{
			name: "v1 TCP6",
			in:   []byte("PROXY TCP6 2001:db8::80 2001:db8::1 54321 11019\r\n"),
			want: netip.MustParseAddr("2001:db8::80"),
		},
		{
			name: "v1 TCP6 carrying a v4-mapped source is unmapped",
			in:   []byte("PROXY TCP6 ::ffff:10.0.0.80 ::ffff:10.0.0.1 54321 11019\r\n"),
			want: netip.MustParseAddr("10.0.0.80"),
		},
		{
			name: "v2 AF_INET",
			in: v2Header(0x21, 0x11, v2INET(
				netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019)),
			want: netip.MustParseAddr("10.0.0.80"),
		},
		{
			name: "v2 AF_INET6",
			in: v2Header(0x21, 0x21, v2INET6(
				netip.MustParseAddr("2001:db8::80"), netip.MustParseAddr("2001:db8::1"), 54321, 11019)),
			want: netip.MustParseAddr("2001:db8::80"),
		},
		{
			// The trap: a proxy may present an IPv4 client in an AF_INET6 block.
			// Left mapped, this router files under a second identity and matches
			// no `routers:` override.
			name: "v2 AF_INET6 carrying a v4-mapped source is unmapped",
			in: v2Header(0x21, 0x21, v2INET6(
				netip.MustParseAddr("::ffff:10.0.0.80"), netip.MustParseAddr("::ffff:10.0.0.1"), 54321, 11019)),
			want: netip.MustParseAddr("10.0.0.80"),
		},
		{
			// TLVs follow the address block inside the declared length. We do
			// not parse them, but we must consume exactly all of them.
			name: "v2 AF_INET with trailing TLVs",
			in: v2Header(0x21, 0x11, append(
				v2INET(netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019),
				0x03, 0x00, 0x04, 0xDE, 0xAD, 0xBE, 0xEF)),
			want: netip.MustParseAddr("10.0.0.80"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Read(bytes.NewReader(tc.in))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if h.Kind != KindProxy {
				t.Fatalf("Kind = %v, want KindProxy", h.Kind)
			}
			if h.Source != tc.want {
				t.Fatalf("Source = %v, want %v", h.Source, tc.want)
			}
		})
	}
}

// TestReadDoesNotOverReadTheStream is the contract that keeps a header parser
// from eating the first bytes of the BMP stream behind it. A parser that
// buffers greedily passes every test above and fails this one, and in
// production the damage surfaces far away as a malformed BMP message.
func TestReadDoesNotOverReadTheStream(t *testing.T) {
	const sentinel = "\x03\x00\x00\x00\x06\x04 the BMP stream begins here"
	headers := map[string][]byte{
		"v1": []byte("PROXY TCP4 10.0.0.80 10.0.0.1 54321 11019\r\n"),
		"v2": v2Header(0x21, 0x11, v2INET(
			netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019)),
		"v2-with-tlv": v2Header(0x21, 0x11, append(
			v2INET(netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019),
			0x03, 0x00, 0x04, 0xDE, 0xAD, 0xBE, 0xEF)),
		// The two headers that declare a body and then never look at it: a
		// LOCAL command and an AF_UNSPEC family both return before the
		// address block is read. They are the paths where consuming the full
		// declared length is doing all the work, and the only ones where an
		// early return would leave body bytes on the reader for bmp.ReadMsg
		// to choke on. A real sender produces both -- a gateway's health
		// probe carries the address block it filled in anyway.
		"v2-local-with-a-body": v2Header(0x20, 0x11, v2INET(
			netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019)),
		"v2-unspec-with-a-body": v2Header(0x21, 0x00, make([]byte, 12)),
	}
	for name, hdr := range headers {
		t.Run(name, func(t *testing.T) {
			r := bytes.NewReader(append(append([]byte{}, hdr...), sentinel...))
			if _, err := Read(r); err != nil {
				t.Fatalf("Read: %v", err)
			}
			rest, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(rest) != sentinel {
				t.Fatalf("bytes left on the reader = %q, want %q", rest, sentinel)
			}
		})
	}
}

func TestReadAssertsNoIdentity(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want Kind
	}{
		{
			// A gateway health-checking its backend. Envoy and HAProxy send
			// LOCAL for connections they originate; rejecting these marks a
			// healthy collector down.
			name: "v2 LOCAL",
			in:   v2Header(0x20, 0x11, v2INET(netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019)),
			want: KindLocal,
		},
		{name: "v2 LOCAL with an empty body", in: v2Header(0x20, 0x00, nil), want: KindLocal},
		{name: "v2 AF_UNSPEC", in: v2Header(0x21, 0x00, nil), want: KindUnspec},
		{name: "v1 UNKNOWN", in: []byte("PROXY UNKNOWN\r\n"), want: KindUnspec},
		{
			name: "v1 UNKNOWN with trailing junk the protocol says to ignore",
			in:   []byte("PROXY UNKNOWN ffff:f...f:ffff ffff:f...f:ffff 65535 65535\r\n"),
			want: KindUnspec,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Read(bytes.NewReader(tc.in))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if h.Kind != tc.want {
				t.Fatalf("Kind = %v, want %v", h.Kind, tc.want)
			}
			if h.Source.IsValid() {
				t.Fatalf("Source = %v, want the zero Addr", h.Source)
			}
		})
	}
}

func TestReadRejects(t *testing.T) {
	good := v2INET(netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019)
	tests := []struct {
		name string
		in   []byte
	}{
		{name: "v2 version nibble is 1", in: v2Header(0x11, 0x11, good)},
		{name: "v2 version nibble is 3", in: v2Header(0x31, 0x11, good)},
		{name: "v2 unknown command", in: v2Header(0x2F, 0x11, good)},
		{name: "v2 AF_INET body one byte short", in: v2Header(0x21, 0x11, good[:11])},
		{name: "v2 AF_INET6 body one byte short", in: v2Header(0x21, 0x21, make([]byte, 35))},
		{name: "v2 AF_UNIX", in: v2Header(0x21, 0x31, make([]byte, 216))},
		{name: "v2 DGRAM transport", in: v2Header(0x21, 0x12, good)},
		{name: "v2 body shorter than declared", in: append(v2Header(0x21, 0x11, good)[:16], good[:6]...)},
		{name: "v1 no CRLF within 107 bytes", in: append([]byte("PROXY TCP4 "), bytes.Repeat([]byte("9"), 200)...)},
		{name: "v1 TCP4 with too few fields", in: []byte("PROXY TCP4 10.0.0.80 10.0.0.1 54321\r\n")},
		{name: "v1 TCP4 with an unparseable source", in: []byte("PROXY TCP4 not-an-ip 10.0.0.1 54321 11019\r\n")},
		{name: "v1 TCP4 carrying an IPv6 source", in: []byte("PROXY TCP4 2001:db8::80 10.0.0.1 54321 11019\r\n")},
		{name: "v1 TCP6 carrying an IPv4 source", in: []byte("PROXY TCP6 10.0.0.80 2001:db8::1 54321 11019\r\n")},
		// netip.ParseAddr accepts a zone and Is4() is false for one, so a
		// zoned source passes both the parse and the TCP6 family check. It
		// must not: subjects.EncodeIP hashes the zone into a different NATS
		// token while the archive drops it, so one router would arrive under
		// two identities on one side and one on the other.
		{name: "v1 TCP6 with a zoned source", in: []byte("PROXY TCP6 fe80::1%eth0 fe80::2 54321 11019\r\n")},
		{name: "v1 TCP6 with a zoned v4-mapped source", in: []byte("PROXY TCP6 ::ffff:10.0.0.80%eth0 ::ffff:10.0.0.1 54321 11019\r\n")},
		// Rejected today by the family check, and pinned here because it sits
		// directly beside the v4-mapped handling this package cares most
		// about: TCP4 announces a 4-byte address, and a mapped one is not
		// that, whatever it denotes.
		{name: "v1 TCP4 carrying a v4-mapped source", in: []byte("PROXY TCP4 ::ffff:10.0.0.80 10.0.0.1 54321 11019\r\n")},
		{name: "v1 unsupported protocol token", in: []byte("PROXY SCTP4 10.0.0.80 10.0.0.1 54321 11019\r\n")},
		{name: "v1 line ends before it could be valid", in: []byte("PROXY \r\n padding to twelve bytes")},
		{name: "double space between v1 fields", in: []byte("PROXY  TCP4 10.0.0.80 10.0.0.1 54321 11019\r\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Read(bytes.NewReader(tc.in)); err == nil {
				t.Fatal("Read succeeded, want an error")
			}
		})
	}
}

// TestReadReportsAnAbsentHeaderDistinctly separates "the proxy is not sending
// headers" from "we disagree about the format". The first is the rollout
// failure an operator will actually hit, and the log and the metric have to
// tell them apart.
func TestReadReportsAnAbsentHeaderDistinctly(t *testing.T) {
	// A real BMP Initiation message: version 0x03, then a length.
	raw := []byte("\x03\x00\x00\x00\x2e\x04\x00\x01\x00\x03rr1")
	_, err := Read(bytes.NewReader(raw))
	if !errors.Is(err, ErrNoHeader) {
		t.Fatalf("err = %v, want ErrNoHeader", err)
	}
}

// TestReadReportsNoDataOnlyWhenNothingArrived pins ErrNoData to the single
// case it exists for: zero bytes read. A peer that commits to a version by
// sending part of the 12-byte prefix and then stops must not match it -- that
// is a format disagreement, not silence, and the caller uses this sentinel to
// tell the two apart.
func TestReadReportsNoDataOnlyWhenNothingArrived(t *testing.T) {
	_, err := Read(bytes.NewReader(nil))
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData for an empty reader", err)
	}
	_, err = Read(bytes.NewReader([]byte("PROXY TCP4 1")))
	if errors.Is(err, ErrNoData) {
		t.Fatalf("err = %v, matched ErrNoData for a partial prefix that arrived", err)
	}
}

// TestReadRejectsEveryTruncation walks a valid header of each version and
// cuts it at every offset. Each prefix must produce an error and never a
// half-built Header -- the case a sender can force at will by writing a few
// bytes and hanging up.
func TestReadRejectsEveryTruncation(t *testing.T) {
	valid := map[string][]byte{
		"v1": []byte("PROXY TCP4 10.0.0.80 10.0.0.1 54321 11019\r\n"),
		"v2": v2Header(0x21, 0x11, v2INET(
			netip.MustParseAddr("10.0.0.80"), netip.MustParseAddr("10.0.0.1"), 54321, 11019)),
	}
	for name, full := range valid {
		for cut := range full {
			t.Run(fmt.Sprintf("%s/cut-at-%d", name, cut), func(t *testing.T) {
				h, err := Read(bytes.NewReader(full[:cut]))
				if err == nil {
					t.Fatalf("Read of a %d-byte prefix succeeded, giving %+v", cut, h)
				}
			})
		}
	}
}

// endlessReader yields the same byte forever and never returns io.EOF, which
// is what a peer dripping a v1 header without a CRLF looks like. A
// bytes.Reader cannot stand in for it: that terminates on its own, so the
// v1 length ceiling is never the thing that ends the read.
type endlessReader struct{ b byte }

func (e endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.b
	}
	return len(p), nil
}

// TestReadBoundsAV1HeaderThatNeverEnds pins maxV1Line. Without the ceiling in
// readV1 this does not fail, it hangs -- so the bound is the only thing
// standing between the collector and a peer that never sends a CRLF.
func TestReadBoundsAV1HeaderThatNeverEnds(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := Read(io.MultiReader(strings.NewReader("PROXY TCP4 "), endlessReader{'9'}))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Read succeeded on a v1 header with no CRLF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not return: the v1 length ceiling is not bounding the scan")
	}
}
