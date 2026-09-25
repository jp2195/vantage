package bmp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

const (
	PeerTypeGlobal = 0
	PeerTypeRD     = 1
	PeerTypeLocal  = 2
	PeerTypeLocRIB = 3
)

const PeerHeaderLen = 42

const (
	flagV = 0x80 // peer address is IPv6
	flagL = 0x40 // post-policy
	flagA = 0x20 // AS_PATH encoded as 2-byte ASes
	flagO = 0x10 // RFC 8671: adj-RIB-out rather than adj-RIB-in
)

// PeerHeader is the RFC 7854 §4.2 per-peer header carried by every BMP
// message except Initiation and Termination.
type PeerHeader struct {
	Type          uint8
	Flags         uint8
	Distinguisher uint64
	Addr          netip.Addr
	AS            uint32
	BGPID         string
	Timestamp     time.Time // zero if the router sent ts=0 (QK_TS_ZERO)
}

func (p PeerHeader) IPv6() bool          { return p.Flags&flagV != 0 }
func (p PeerHeader) PostPolicy() bool    { return p.Flags&flagL != 0 }
func (p PeerHeader) TwoByteASPath() bool { return p.Flags&flagA != 0 }

// AdjRIBOut reports RFC 8671's O flag: this message describes routes the
// router *sent* to the peer, not routes it received. Combined with
// PostPolicy it gives the four RIB streams a router may mirror
// concurrently for one neighbor -- pre- and post-policy adj-RIB-in, and
// pre- and post-policy adj-RIB-out.
func (p PeerHeader) AdjRIBOut() bool { return p.Flags&flagO != 0 }

// ParsePeerHeader parses the fixed 42-byte per-peer header from the front of
// b, returning the parsed header and the remaining bytes.
func ParsePeerHeader(b []byte) (PeerHeader, []byte, error) {
	if len(b) < PeerHeaderLen {
		return PeerHeader{}, nil, fmt.Errorf("bmp: per-peer header truncated (%d bytes)", len(b))
	}
	ph := PeerHeader{Type: b[0], Flags: b[1]}
	ph.Distinguisher = binary.BigEndian.Uint64(b[2:10])
	if ph.IPv6() {
		var a [16]byte
		copy(a[:], b[10:26])
		ph.Addr = netip.AddrFrom16(a)
	} else {
		var a [4]byte
		copy(a[:], b[22:26])
		ph.Addr = netip.AddrFrom4(a)
	}
	ph.AS = binary.BigEndian.Uint32(b[26:30])
	ph.BGPID = netip.AddrFrom4([4]byte(b[30:34])).String()
	sec := binary.BigEndian.Uint32(b[34:38])
	usec := binary.BigEndian.Uint32(b[38:42])
	if sec != 0 || usec != 0 {
		ph.Timestamp = time.Unix(int64(sec), int64(usec)*1000).UTC()
	}
	return ph, b[PeerHeaderLen:], nil
}

// Append serializes the header (builder for tests and bmpgen).
//
// Precondition: BGPID, if non-empty, must be parseable as an IPv4 address
// (a dotted-quad string like "10.0.0.1" or an IPv4-mapped IPv6 form like
// "::ffff:10.0.0.1"). An empty BGPID is treated as unset and encodes as
// 0.0.0.0. IPv4-mapped forms are unmapped before the IPv4 check, so
// "::ffff:10.0.0.1" is accepted rather than silently replaced with 0.0.0.0.
// A non-empty unparseable or non-IPv4 BGPID will panic with a message naming
// this function and the offending value; this is correct and deliberate
// (Append is called only by this repo's own test and fixture code, never on
// attacker input), whereas silent emission of 0.0.0.0 would corrupt wire fixtures.
func (p PeerHeader) Append(dst []byte) []byte {
	// The V-flag and address encoding are derived from p.Addr alone so they
	// can never disagree with each other or with a stale caller-supplied
	// flagV bit. Is4In6 addresses (e.g. ::ffff:10.0.0.9) are IPv6 as far as
	// netip.Addr equality is concerned, so they are encoded as full 16-byte
	// addresses with flagV set, distinguishing them from genuine IPv4.
	flags := p.Flags &^ flagV
	if p.Addr.Is6() {
		flags |= flagV
	}
	dst = append(dst, p.Type, flags)
	dst = binary.BigEndian.AppendUint64(dst, p.Distinguisher)
	var a16 [16]byte
	if p.Addr.Is6() {
		a16 = p.Addr.As16()
	} else if p.Addr.IsValid() {
		a4 := p.Addr.As4()
		copy(a16[12:], a4[:])
	}
	dst = append(dst, a16[:]...)
	dst = binary.BigEndian.AppendUint32(dst, p.AS)
	var bgpid [4]byte
	if p.BGPID != "" {
		a, err := netip.ParseAddr(p.BGPID)
		if err != nil {
			panic(fmt.Sprintf("bmp: PeerHeader.Append: bgpID %q is not a valid IP address: %v", p.BGPID, err))
		}
		a = a.Unmap() // ::ffff:10.0.0.1 (Is4In6) -> 10.0.0.1 (Is4), not silently zeroed
		if !a.Is4() {
			panic(fmt.Sprintf("bmp: PeerHeader.Append: bgpID %q is not representable as an IPv4 BGP identifier", p.BGPID))
		}
		bgpid = a.As4()
	}
	// else: bgpid is zero-initialized to 0.0.0.0
	dst = append(dst, bgpid[:]...)
	var sec, usec uint32
	if !p.Timestamp.IsZero() {
		sec = uint32(p.Timestamp.Unix())
		usec = uint32(p.Timestamp.Nanosecond() / 1000)
	}
	dst = binary.BigEndian.AppendUint32(dst, sec)
	return binary.BigEndian.AppendUint32(dst, usec)
}
