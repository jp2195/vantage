package bgp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// VpnPrefix is one labeled prefix from a vpn4 (RFC 4364 §4.3.4) or lu4
// (RFC 8277) NLRI/withdrawn-routes entry. RD is empty for lu4, which carries
// no Route Distinguisher.
type VpnPrefix struct {
	Prefix netip.Prefix
	PathID uint32
	RD     string
	Labels []uint32
}

// parseVpnNLRI decodes a vpn4 MP_REACH/MP_UNREACH NLRI (RFC 4364 §4.3.4):
// each entry is [length-in-bits][label stack][8-byte RD][prefix].
func parseVpnNLRI(b []byte, addPath bool) ([]VpnPrefix, error) {
	return parseLabeledCommon(b, addPath, true, 32)
}

// parseVpn6NLRI decodes a vpn6 MP_REACH/MP_UNREACH NLRI (RFC 4659 §3.2.1).
// Structurally identical to vpn4 -- [length-in-bits][label stack][8-byte
// RD][prefix] -- differing only in that the prefix is an IPv6 address, so the
// derived prefix length is bounded at 128 rather than 32.
func parseVpn6NLRI(b []byte, addPath bool) ([]VpnPrefix, error) {
	return parseLabeledCommon(b, addPath, true, 128)
}

// parseLabeledNLRI decodes a labeled-unicast NLRI (RFC 8277): the same shape
// as vpn4 without the Route Distinguisher -- [length-in-bits][label
// stack][prefix]. The returned VpnPrefix.RD is always "".
func parseLabeledNLRI(b []byte, addPath bool) ([]VpnPrefix, error) {
	return parseLabeledCommon(b, addPath, false, 32)
}

// parseLabeledCommon is the shared walk for both formats. Each entry is an
// optional 4-byte ADD-PATH Path Identifier, then one length byte giving the
// total length *in bits* of everything else in the entry -- the label stack,
// plus the 8-byte RD when withRD, plus the prefix -- then those fields in
// that order.
//
// The length byte is the trap: it is a bit count, not a byte count, and it
// covers the label stack and RD as well as the prefix. The prefix's own
// length in bits is therefore derived, never read directly off the wire:
//
//	prefixBits = totalBits - 24*labelCount - (64 if withRD else 0)
//
// prefixBits is range-checked against the IPv4 maximum (32) *before* it is
// used to size any read or copy, so a hostile length byte cannot turn into
// an oversized copy -- a /33 is rejected on the range check, not truncated
// down or used to over-read.
//
// Every read is bounds-checked before it happens: the declared bit length is
// converted to a byte count and checked against what remains in b *before*
// any of it is sliced off into body, so a truncated entry is rejected there
// rather than at an arbitrary offset a few lines later. decodeLabels and
// decodeRD then only ever see body, which is capped at exactly the entry's
// declared length, so neither can wander into the next entry when this
// entry's internal accounting is wrong.
//
// Each iteration consumes at least the length byte (and, when addPath, the
// leading 4-byte path ID) before any error can be returned, so len(b)
// strictly decreases every time the loop continues -- forward progress is
// unconditional, not merely true along the error-free path.
// maxPrefixBits is the address family's prefix-length ceiling -- 32 for the
// IPv4-based families (vpn4, lu4), 128 for the IPv6-based ones (vpn6). It
// bounds the DERIVED prefix length before that length sizes any read or copy,
// so a hostile length byte is rejected on the range check rather than
// truncated down or used to over-read; it also selects which address width
// the trailing prefix bytes are read into.
func parseLabeledCommon(b []byte, addPath, withRD bool, maxPrefixBits int) ([]VpnPrefix, error) {
	var out []VpnPrefix
	for len(b) > 0 {
		var e VpnPrefix
		if addPath {
			if len(b) < 5 {
				return nil, fmt.Errorf("%w: add-path entry needs 5 bytes (path-id+length), %d remain", ErrNLRITruncated, len(b))
			}
			e.PathID = binary.BigEndian.Uint32(b[0:4])
			b = b[4:]
		}
		// len(b) >= 1 here unconditionally: the loop guard gives it directly
		// when addPath is false, and the addPath branch above only proceeds
		// past its own length check with at least 1 byte left after
		// consuming the path ID.
		totalBits := int(b[0])
		b = b[1:]
		totalBytes := (totalBits + 7) / 8
		if totalBytes > len(b) {
			return nil, fmt.Errorf("%w: entry declares %d bits (%d bytes), %d remain", ErrNLRITruncated, totalBits, totalBytes, len(b))
		}
		body := b[:totalBytes]
		b = b[totalBytes:]

		// body is capped at the entry's DECLARED length above, so a
		// "truncated" verdict from here does not mean a short read -- it
		// means the declared length is too small to hold the mandatory
		// fields, which more wire bytes could never fix. Reclassify, so a
		// caller branching on the sentinel is not told to wait for data
		// that would not help.
		labels, n, err := decodeLabels(body)
		if err != nil {
			return nil, fmt.Errorf("%w: entry declares %d bits, too few for its label stack: %v",
				ErrNLRIBadLength, totalBits, err)
		}
		e.Labels = labels
		body = body[n:]
		usedBits := n * 8

		if withRD {
			rd, err := decodeRD(body)
			if err != nil {
				// Same reasoning as the label stack above: bounded by the
				// declared length, so this is malformed, not truncated.
				return nil, fmt.Errorf("%w: entry declares %d bits, too few for a route distinguisher: %v",
					ErrNLRIBadLength, totalBits, err)
			}
			e.RD = rd
			body = body[8:]
			usedBits += 64
		}

		prefixBits := totalBits - usedBits
		if prefixBits < 0 || prefixBits > maxPrefixBits {
			return nil, fmt.Errorf("%w: prefix length %d bits out of range for a %d-bit family",
				ErrNLRIBadLength, prefixBits, maxPrefixBits)
		}
		pfxBytes := (prefixBits + 7) / 8
		if pfxBytes > len(body) {
			return nil, fmt.Errorf("%w: prefix needs %d bytes, %d remain", ErrNLRITruncated, pfxBytes, len(body))
		}
		// The wire carries only the significant bytes of the prefix; the rest
		// of the address is zero. Copying into a zeroed array of the family's
		// full width is what supplies that padding, so the same walk serves
		// both widths with no per-family special case beyond the array size.
		var addr netip.Addr
		if maxPrefixBits <= 32 {
			var a4 [4]byte
			copy(a4[:], body[:pfxBytes])
			addr = netip.AddrFrom4(a4)
		} else {
			var a16 [16]byte
			copy(a16[:], body[:pfxBytes])
			addr = netip.AddrFrom16(a16)
		}
		p, err := addr.Prefix(prefixBits)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNLRIBadLength, err)
		}
		e.Prefix = p
		out = append(out, e)
	}
	return out, nil
}
