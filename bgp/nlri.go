package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// Sentinel errors for malformed NLRI. The split is by *why* the input is
// unusable, because a caller may reasonably treat the two differently:
//
//   - ErrNLRITruncated: the input ran out of bytes. More data might have
//     completed it, so this is what a short read looks like.
//   - ErrNLRIBadLength: the bytes present are structurally impossible --
//     a length that cannot be right, a type tag that is not defined, or a
//     label stack that never terminates. More data would not have helped.
//
// The name is historical and slightly narrow: ErrNLRIBadLength covers
// malformed shape generally, not only lengths. It is not renamed, for API
// stability.
var (
	ErrNLRITruncated = errors.New("bgp: nlri truncated")
	ErrNLRIBadLength = errors.New("bgp: nlri malformed")
)

// withdrawLabel is the RFC 3107 / RFC 8277 sentinel label stack sent in place
// of a real label when withdrawing. It is matched on the raw three bytes.
const withdrawLabel = 0x800000

// maxLabelStack bounds how many labels decodeLabels will read before giving
// up. A stack that never sets the bottom-of-stack bit would otherwise walk the
// entire remaining NLRI; real stacks are one or two deep.
const maxLabelStack = 16

// decodeRD renders an 8-byte Route Distinguisher as text (RFC 4364 §4.2).
// The three type encodings lay their fields out differently, so a misread
// produces a plausible-looking RD rather than an error -- hence the explicit
// type switch and the refusal to guess at an unknown type.
func decodeRD(b []byte) (string, error) {
	if len(b) < 8 {
		return "", fmt.Errorf("%w: rd needs 8 bytes, got %d", ErrNLRITruncated, len(b))
	}
	switch typ := binary.BigEndian.Uint16(b[0:2]); typ {
	case 0: // 2-byte ASN : 4-byte assigned
		return fmt.Sprintf("%d:%d", binary.BigEndian.Uint16(b[2:4]), binary.BigEndian.Uint32(b[4:8])), nil
	case 1: // 4-byte IPv4 : 2-byte assigned
		ip := netip.AddrFrom4([4]byte(b[2:6]))
		return fmt.Sprintf("%s:%d", ip.String(), binary.BigEndian.Uint16(b[6:8])), nil
	case 2: // 4-byte ASN : 2-byte assigned
		return fmt.Sprintf("%d:%d", binary.BigEndian.Uint32(b[2:6]), binary.BigEndian.Uint16(b[6:8])), nil
	default:
		return "", fmt.Errorf("%w: unknown rd type %d", ErrNLRIBadLength, typ)
	}
}

// decodeLabels reads an MPLS label stack from the front of b, returning the
// 20-bit label values, the number of bytes consumed, and an error.
//
// Each label is 3 bytes: 20 bits of label, 3 bits of traffic class, then the
// bottom-of-stack bit. Reading stops after the label whose BoS bit is set.
// The withdraw sentinel (0x800000) has its BoS bit clear on the wire, so it is
// special-cased -- without that, a withdraw NLRI would send this loop hunting
// for a bottom-of-stack label that is not there.
func decodeLabels(b []byte) ([]uint32, int, error) {
	var out []uint32
	n := 0
	for range maxLabelStack {
		if len(b)-n < 3 {
			return nil, 0, fmt.Errorf("%w: label needs 3 bytes, %d remain", ErrNLRITruncated, len(b)-n)
		}
		raw := uint32(b[n])<<16 | uint32(b[n+1])<<8 | uint32(b[n+2])
		out = append(out, raw>>4)
		n += 3
		if raw == withdrawLabel || b[n-1]&0x01 == 1 {
			return out, n, nil
		}
	}
	// Not ErrNLRITruncated: there may be plenty of bytes left. A stack this
	// deep with no bottom-of-stack bit is structurally wrong, and more input
	// would not fix it.
	return nil, 0, fmt.Errorf("%w: label stack exceeds %d entries with no bottom-of-stack", ErrNLRIBadLength, maxLabelStack)
}

// Families decoded into typed NLRI. Anything not listed here keeps the
// default behavior: raw bytes plus PARSE_FLAG_UNKNOWN_FAMILY.
var (
	FamilyIPv6U = Family{AFI: 2, SAFI: 1} // RFC 4760 §5
	FamilyVPNv4 = Family{AFI: 1, SAFI: 128}
	FamilyVPNv6 = Family{AFI: 2, SAFI: 128} // RFC 4659 §3.2.1
	FamilyLU4   = Family{AFI: 1, SAFI: 4}
	FamilyEVPN  = Family{AFI: 25, SAFI: 70}
	FamilyBGPLS = Family{AFI: 16388, SAFI: 71} // RFC 9552 §4
)

// decodeNLRI dispatches to the decoder for fam. decoded reports whether a
// decoder existed at all -- false means "no decoder for this family", which is
// what keeps UNKNOWN_FAMILY meaningful. untyped reports that a decoder ran but
// carried at least one entry raw -- for BGP-LS this means DecodeLsNLRI's
// undecoded count was greater than zero; the caller raises
// PARSE_FLAG_LS_NLRI_UNDECODED instead of the generic NLRI_UNTYPED for that
// family (see nlriUntypedFlag in update.go).
//
// Keeping this one table rather than a switch buried in update.go means the
// typed/untyped boundary is legible in one place, and adding BGP-LS later is a
// new file plus an entry here rather than surgery on the UPDATE parser.
func decodeNLRI(fam Family, b []byte, addPath bool) (plain []Prefix, vpn []VpnPrefix, evpn []EvpnRoute, lsNodes []LsNodeNLRI, lsLinks []LsLinkNLRI, lsPrefixes []LsPrefixNLRI, untyped, decoded bool, err error) {
	switch fam {
	case FamilyIPv6U:
		// ipv6 unicast carries ordinary prefixes, not labeled or typed
		// entries, so it is the one family here that returns `plain` -- the
		// same []Prefix shape ipv4 unicast produces, which is what lets the
		// caller append it to Announced/Withdrawn alongside ipv4.
		plain, err = ParsePrefixesV6(b, addPath)
		return plain, nil, nil, nil, nil, nil, false, true, err
	case FamilyVPNv4:
		vpn, err = parseVpnNLRI(b, addPath)
		return nil, vpn, nil, nil, nil, nil, false, true, err
	case FamilyVPNv6:
		vpn, err = parseVpn6NLRI(b, addPath)
		return nil, vpn, nil, nil, nil, nil, false, true, err
	case FamilyLU4:
		vpn, err = parseLabeledNLRI(b, addPath)
		return nil, vpn, nil, nil, nil, nil, false, true, err
	case FamilyEVPN:
		evpn, untyped, err = parseEvpnNLRI(b, addPath)
		return nil, nil, evpn, nil, nil, nil, untyped, true, err
	case FamilyBGPLS:
		var undecoded int
		lsNodes, lsLinks, lsPrefixes, undecoded, err = DecodeLsNLRI(b)
		return nil, nil, nil, lsNodes, lsLinks, lsPrefixes, undecoded > 0, true, err
	default:
		return nil, nil, nil, nil, nil, nil, false, false, nil
	}
}

// familyHasDecoder reports whether fam has a decodeNLRI entry, independent of
// whether decoding any particular NLRI would succeed. parseMPReach needs this
// before it can safely call decodeNLRI at all: the next-hop-length bound
// check must run first (nothing downstream knows where the NLRI even starts
// until that check passes), but the flag it raises on failure should still
// be UNKNOWN_FAMILY for a family with literally no decoder, and NLRI_UNTYPED
// for one that has a decoder but never got to run it. Keeping this as its
// own tiny lookup over the same switch, rather than teaching decodeNLRI to
// answer the question with a zero-length input, keeps "does a decoder exist"
// separate from "did decoding this input work."
func familyHasDecoder(fam Family) bool {
	switch fam {
	case FamilyIPv6U, FamilyVPNv4, FamilyVPNv6, FamilyLU4, FamilyEVPN, FamilyBGPLS:
		return true
	default:
		return false
	}
}

// nlriUntypedFlag picks which ParseFlag to raise when decodeNLRI reports
// untyped == true for fam. BGP-LS gets its own, more specific flag
// (PARSE_FLAG_LS_NLRI_UNDECODED) because for that family "untyped" always
// means the same concrete thing -- an NLRI type this build does not decode --
// rather than the mix of reasons (an EVPN route type this build doesn't type,
// an unrecognized next-hop length, ...) the generic PARSE_FLAG_NLRI_UNTYPED
// covers for every other family.
func nlriUntypedFlag(fam Family) vantagev1.ParseFlag {
	if fam == FamilyBGPLS {
		return vantagev1.ParseFlag_PARSE_FLAG_LS_NLRI_UNDECODED
	}
	return vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED
}

// nextHopForFamily decodes the MP_REACH next hop nh for a typed family (vpn4,
// lu4, EVPN, BGP-LS) per each family's own RFC-defined next-hop encoding. ok reports
// whether nh's length was recognized for fam; a typed family's NLRI decoder
// can succeed even when this returns ok == false; the caller is responsible
// for treating that as a partial result (raw bytes kept, PARSE_FLAG_NLRI_
// UNTYPED raised) rather than silently discarding the next hop.
//
//   - vpn4 (RFC 4364 §4.3.2 / RFC 8950 §3 for the IPv6 forms): 12 bytes = an
//     8-byte Route Distinguisher followed by a 4-byte IPv4 address; 24 bytes
//     = 8 zero bytes followed by a 16-byte IPv6 address; 48 bytes = the same
//     again with an RFC 2545 link-local address appended, of which only the
//     global address is kept (matching what update.go does for ipv4-unicast).
//   - lu4 (RFC 8277 / RFC 8950 §3): a bare 4-byte IPv4 address, a 16-byte
//     IPv6 address, or 32 bytes = global + link-local IPv6, of which only the
//     global address is kept -- no Route Distinguisher prefix, unlike vpn4.
//   - EVPN (RFC 7432 / RFC 8950): the same shapes as lu4.
//   - BGP-LS (RFC 9552 §4): the same shapes as lu4 -- a plain per-AFI next
//     hop, no Route Distinguisher.
//
// The link-local forms are not academic: they are the encoding for a session
// over an unnumbered IPv6 link, which is the deployment RFC 8950 exists to
// serve. Omitting them degraded every such route to raw with a false
// "untyped" signal.
//
// vpn4's leading Route Distinguisher must be zero (RFC 4364 §4.3.2). A
// non-zero one is rejected rather than discarded unread: this function's
// contract is that the caller keeps the raw bytes whenever ok is false, so
// rejecting preserves the anomaly for reparse instead of destroying the only
// record of it. It does not cost a route -- the NLRI still decodes.
//
// Any other length is not guessed at: ok is false and the caller keeps the
// raw attribute bytes rather than reporting an address decoded from the
// wrong offset.
func nextHopForFamily(fam Family, nh []byte) (s string, ok bool) {
	switch fam {
	case FamilyVPNv4, FamilyVPNv6:
		// RFC 4659 §3.2.1.1 gives vpn6 the same next-hop shape as vpn4: an
		// 8-byte Route Distinguisher field, always zero here because a next
		// hop has no RD of its own, followed by the address. A vpn6 next hop
		// is therefore a 24-byte (global) or 48-byte (global + RFC 2545
		// link-local) form, both already handled below.
		if len(nh) < 8 || !allZero(nh[0:8]) {
			return "", false
		}
		switch len(nh) {
		case 12:
			return netip.AddrFrom4([4]byte(nh[8:12])).String(), true
		case 24, 48:
			return netip.AddrFrom16([16]byte(nh[8:24])).String(), true
		}
	case FamilyIPv6U, FamilyLU4, FamilyEVPN, FamilyBGPLS:
		// BGP-LS (RFC 9552 §4) carries a plain per-AFI next hop -- no Route
		// Distinguisher prefix, same as lu4/EVPN -- so the same 4/16/32-byte
		// shapes apply.
		switch len(nh) {
		case 4:
			return netip.AddrFrom4([4]byte(nh)).String(), true
		case 16, 32:
			return netip.AddrFrom16([16]byte(nh[:16])).String(), true
		}
	}
	return "", false
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
