// Package proxyproto parses HAProxy PROXY protocol headers, versions 1 and 2.
//
// It exists so vantage-collector can take a router's real address from a
// header written by a front-end proxy instead of from the socket, which a
// proxy has necessarily replaced with its own. The rule for when to trust
// such a header is the collector's; see collector.ProxyProtocolRequired.
//
// Read consumes exactly the header and not one byte more. That is the single
// property everything else here is arranged around: the bytes immediately
// behind the header are a BMP stream, and a parser that buffers ahead would
// swallow the start of it and surface the damage far away as a malformed BMP
// message. So there is no bufio.Reader anywhere in this package -- every read
// is for an exact, already-known count.
package proxyproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// sigV2 is the 12-byte version-2 signature. It is deliberately not valid
// anything else: a BMP message begins with version byte 0x03 and can never be
// mistaken for it, nor for version 1's "PROXY " prefix.
var sigV2 = [12]byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// maxV1Line is the protocol's own ceiling on a version-1 line, including the
// terminating CRLF. Bounding the scan is what stops a sender from making this
// package read forever.
const maxV1Line = 107

// ErrNoHeader reports that the stream began with neither signature. The caller
// distinguishes it from a malformed header because the two mean different
// things operationally: this one is "the proxy in front is not configured to
// send headers", which is the failure an operator hits at rollout.
var ErrNoHeader = errors.New("proxyproto: connection did not begin with a PROXY protocol header")

// ErrNoData reports that the peer closed or fell silent without sending a
// single byte. It is deliberately distinct from every other error here: a
// connection that says nothing at all is an L4 health check or a port scan,
// which is routine and uninteresting, while a connection that sends part of a
// header and then stops is a peer that committed to a version and failed to
// deliver one -- that is a format disagreement, and an operator needs to see
// it. Only the very first read can produce this; a read that ends in EOF
// deeper in the parse is io.ErrUnexpectedEOF by way of the wrapping below.
var ErrNoData = errors.New("proxyproto: peer sent no data")

// Kind says whether a header asserted an identity, and if not, why not.
type Kind int

const (
	// KindProxy is a header that names a client. Source is set.
	KindProxy Kind = iota
	// KindLocal is a version-2 LOCAL command: a connection the proxy
	// originated itself, which in practice is a health check. There is no
	// client behind it.
	KindLocal
	// KindUnspec is a version-1 UNKNOWN line or a version-2 AF_UNSPEC family:
	// a real connection whose source the proxy declines to state.
	KindUnspec
)

// Header is one parsed PROXY protocol header. Source is meaningful only when
// Kind is KindProxy, and is already unmapped -- a v4-mapped address arriving
// in an IPv6 block comes back as the IPv4 address it denotes, so one router
// cannot acquire two identities depending on which block its proxy used.
type Header struct {
	Kind   Kind
	Source netip.Addr
}

// Read parses a header from r, consuming exactly its bytes.
func Read(r io.Reader) (Header, error) {
	var pre [12]byte
	n, err := io.ReadFull(r, pre[:])
	if err != nil {
		if n == 0 {
			return Header{}, fmt.Errorf("%w: %w", ErrNoData, err)
		}
		return Header{}, fmt.Errorf("proxyproto: reading header prefix: %w", err)
	}
	if pre == sigV2 {
		return readV2(r)
	}
	// Every valid version-1 line is at least 15 bytes ("PROXY UNKNOWN\r\n"),
	// so reading 12 to disambiguate never overshoots the shortest one.
	if string(pre[:6]) == "PROXY " {
		return readV1(r, pre)
	}
	return Header{}, ErrNoHeader
}

func readV1(r io.Reader, pre [12]byte) (Header, error) {
	// A CRLF inside the 12 bytes already read means the line ended before it
	// could be valid. Rejecting it here lets the scan below check only the
	// last two bytes it has appended.
	for i := 0; i+1 < len(pre); i++ {
		if pre[i] == '\r' && pre[i+1] == '\n' {
			return Header{}, errors.New("proxyproto: v1 header ended before it could be valid")
		}
	}
	line := make([]byte, 12, maxV1Line)
	copy(line, pre[:])
	var b [1]byte
	for len(line) < maxV1Line {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Header{}, fmt.Errorf("proxyproto: reading v1 header: %w", err)
		}
		line = append(line, b[0])
		if line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
			return parseV1(line[:len(line)-2])
		}
	}
	return Header{}, fmt.Errorf("proxyproto: v1 header reached %d bytes with no CRLF", maxV1Line)
}

// parseV1 takes the line with its CRLF already stripped.
func parseV1(line []byte) (Header, error) {
	// Split on single spaces rather than strings.Fields: the protocol
	// specifies exactly one space between fields, and accepting runs of them
	// would be inventing a dialect.
	f := strings.Split(string(line), " ")
	if len(f) < 2 {
		return Header{}, errors.New("proxyproto: v1 header has no protocol field")
	}
	switch f[1] {
	case "UNKNOWN":
		// The protocol says a receiver must ignore everything after UNKNOWN,
		// so the field count is deliberately not checked here.
		return Header{Kind: KindUnspec}, nil
	case "TCP4", "TCP6":
	default:
		return Header{}, fmt.Errorf("proxyproto: unsupported v1 protocol %q", f[1])
	}
	if len(f) != 6 {
		return Header{}, fmt.Errorf("proxyproto: v1 %s header has %d fields, want 6", f[1], len(f))
	}
	src, err := netip.ParseAddr(f[2])
	if err != nil {
		return Header{}, fmt.Errorf("proxyproto: v1 source address %q: %w", f[2], err)
	}
	// netip.ParseAddr accepts a scope id ("fe80::1%eth0"); the v1 grammar
	// does not permit one, and it must not be let through, because the
	// handling downstream is inconsistent in the worst possible way.
	// subjects.EncodeIP hashes the zone into a distinct token, so NATS sees a
	// different router, while the ClickHouse driver parses the zone and then
	// discards it, so the archive sees the same one. Two collector-side
	// identities collapsing to one router_ip is precisely the hazard this
	// package exists to avoid. Unmap() below would also carry the zone along
	// on a v4-mapped source. The v2 path cannot produce a zone at all: it
	// builds addresses from raw bytes.
	if src.Zone() != "" {
		return Header{}, fmt.Errorf("proxyproto: v1 source address %q carries a zone", f[2])
	}
	if want4 := f[1] == "TCP4"; src.Is4() != want4 {
		return Header{}, fmt.Errorf("proxyproto: v1 %s header carries source %s", f[1], src)
	}
	return Header{Kind: KindProxy, Source: src.Unmap()}, nil
}

func readV2(r io.Reader) (Header, error) {
	var meta [4]byte
	if _, err := io.ReadFull(r, meta[:]); err != nil {
		return Header{}, fmt.Errorf("proxyproto: reading v2 header: %w", err)
	}
	verCmd, famProto := meta[0], meta[1]
	if v := verCmd >> 4; v != 0x2 {
		return Header{}, fmt.Errorf("proxyproto: v2 version nibble is %d, want 2", v)
	}
	// Read the full declared length before interpreting anything. The address
	// block may be followed by TLVs, and consuming exactly what was declared
	// -- no more, no less -- is what leaves the reader on the first BMP byte.
	body := make([]byte, binary.BigEndian.Uint16(meta[2:4]))
	if _, err := io.ReadFull(r, body); err != nil {
		return Header{}, fmt.Errorf("proxyproto: reading v2 body (%d bytes): %w", len(body), err)
	}
	switch cmd := verCmd & 0x0F; cmd {
	case 0x0:
		return Header{Kind: KindLocal}, nil
	case 0x1:
	default:
		return Header{}, fmt.Errorf("proxyproto: unknown v2 command %d", cmd)
	}
	fam, proto := famProto>>4, famProto&0x0F
	if fam == 0x0 {
		return Header{Kind: KindUnspec}, nil
	}
	if proto != 0x1 {
		return Header{}, fmt.Errorf("proxyproto: v2 transport %d is not STREAM", proto)
	}
	switch fam {
	case 0x1:
		if len(body) < 12 {
			return Header{}, fmt.Errorf("proxyproto: v2 AF_INET body is %d bytes, want at least 12", len(body))
		}
		return Header{Kind: KindProxy, Source: netip.AddrFrom4([4]byte(body[0:4]))}, nil
	case 0x2:
		if len(body) < 36 {
			return Header{}, fmt.Errorf("proxyproto: v2 AF_INET6 body is %d bytes, want at least 36", len(body))
		}
		return Header{Kind: KindProxy, Source: netip.AddrFrom16([16]byte(body[0:16])).Unmap()}, nil
	default:
		return Header{}, fmt.Errorf("proxyproto: unsupported v2 address family %d", fam)
	}
}
