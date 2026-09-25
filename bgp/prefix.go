package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// Sentinel errors for malformed NLRI/withdrawn-routes encodings, distinguished
// by which structural rule was violated: not enough bytes for a declared
// path-identifier/prefix-length/prefix-data field, versus a prefix length
// that exceeds the address family's maximum. These are returned by
// ParsePrefixesV4 and propagate (wrapped) out of ParseUpdate as hard errors —
// unlike a malformed path *attribute*, a corrupt NLRI/withdrawn-routes field
// cannot be safely skipped or discarded in place (its own length prefixes are
// what determine where the next entry starts), so RFC 7606's softer
// attribute-discard/treat-as-withdraw ladder does not apply here; the message
// as a whole cannot be reliably parsed further. See ParseUpdate's doc comment
// for why NLRI/withdrawn errors and attribute errors are handled differently.
var (
	ErrPrefixTruncated  = errors.New("bgp: nlri truncated")
	ErrPrefixLenInvalid = errors.New("bgp: prefix length exceeds family maximum")
)

// Prefix is one IPv4 unicast NLRI/withdrawn-routes entry (RFC 4271 §4.3),
// optionally carrying an RFC 7911 ADD-PATH Path Identifier. PathID is 0 when
// add-path is not in use for this entry.
type Prefix struct {
	Prefix netip.Prefix
	PathID uint32
}

// ParsePrefixesV4 decodes a sequence of IPv4 unicast NLRI/withdrawn-routes
// entries (RFC 4271 §4.3: a 1-byte prefix length in bits, followed by
// ceil(length/8) bytes of address, zero-padded on read into the low-order
// bits of the family's address size). When addPath is true, each entry is
// preceded by a 4-byte Path Identifier (RFC 7911 §3).
//
// A prefix length above 32 (the IPv4 maximum) is rejected with
// ErrPrefixLenInvalid rather than used to size a copy — plen is bounds-checked
// before it ever reaches an allocation or slice length. Host bits set beyond
// the declared prefix length are tolerated (seen from real routers) and
// normalized via Masked() rather than rejected.
func ParsePrefixesV4(b []byte, addPath bool) ([]Prefix, error) {
	var out []Prefix
	for len(b) > 0 {
		var p Prefix
		if addPath {
			if len(b) < 5 {
				return nil, fmt.Errorf("%w: add-path entry needs 5 bytes (path-id+length), %d remain", ErrPrefixTruncated, len(b))
			}
			p.PathID = binary.BigEndian.Uint32(b[:4])
			b = b[4:]
		}
		plen := int(b[0])
		b = b[1:]
		if plen > 32 {
			return nil, fmt.Errorf("%w: v4 prefix length %d", ErrPrefixLenInvalid, plen)
		}
		n := (plen + 7) / 8
		if len(b) < n {
			return nil, fmt.Errorf("%w: prefix of length %d needs %d bytes, %d remain", ErrPrefixTruncated, plen, n, len(b))
		}
		var a [4]byte
		copy(a[:], b[:n])
		b = b[n:]
		pfx := netip.PrefixFrom(netip.AddrFrom4(a), plen)
		if pfx != pfx.Masked() {
			// Host bits set beyond the declared prefix length: tolerated
			// (seen in the wild from real implementations), normalized
			// rather than rejected.
			pfx = pfx.Masked()
		}
		p.Prefix = pfx
		out = append(out, p)
	}
	return out, nil
}

// AppendPrefixesV4 encodes ps in the wire format ParsePrefixesV4 decodes
// (builder for tests and bmpgen). Each Prefix must hold an IPv4 (or
// IPv4-mapped IPv6) address with Bits() in [0,32]; netip.Addr.As4 panics
// otherwise, per this package's builders-guard-their-preconditions
// convention (see AppendOpen, bmp.AppendTLV) — callers pass known-good
// values, not attacker-controlled wire input.
func AppendPrefixesV4(dst []byte, ps []Prefix, addPath bool) []byte {
	for _, p := range ps {
		if addPath {
			dst = binary.BigEndian.AppendUint32(dst, p.PathID)
		}
		plen := p.Prefix.Bits()
		dst = append(dst, byte(plen))
		a := p.Prefix.Addr().As4()
		dst = append(dst, a[:(plen+7)/8]...)
	}
	return dst
}

// parsePrefixesV4Auto runs the structural add-path heuristic when caps are
// unknown (known == false): try both interpretations and, when only one is
// even structurally valid, use it. Returns (prefixes, usedAddPath,
// heuristicFired, err); heuristicFired is true whenever the heuristic ran —
// i.e. whenever known is false and this call fell through to the
// two-interpretation attempt below — regardless of which interpretation it
// settled on. (heuristicFired used to be true only when the
// heuristic both ran AND chose add-path, so a run that settled on "plain"
// was indistinguishable from a caps-known certain parse — a downstream
// consumer had no way to tell a guess from a fact. The flag now honestly
// means "this NLRI region was parsed by inference, not from negotiated
// capabilities," which is what ParseFlag_ADDPATH_HEURISTIC is for.)
//
// When caps are known, this is a direct, non-guessing pass-through to
// ParsePrefixesV4(b, addPath).
//
// The genuinely-ambiguous case — both interpretations parse without error —
// is real, not a corner case invented for testing: RFC 7911 leaves the Path
// Identifier's value entirely up to the sender, but every deployed
// implementation this package is aware of (Cisco, Juniper, FRR, BIRD) assigns
// them as small, sequentially-increasing integers. A small integer's
// big-endian encoding starts with one or more zero bytes, and a zero byte is
// also a syntactically valid prefix length (0, i.e. 0.0.0.0/0): read as plain
// NLRI, each add-path entry's leading zero byte(s) parse as their own,
// spurious zero-length "default route" entries, and the parse can run to
// completion with no error at all — it is simply wrong. (Confirmed
// empirically, not just reasoned about: PathID 9 with two /32 entries
// round-trips through a "plain" reparse with zero errors, silently producing
// 12 bogus entries instead of the real 2. A naive "try plain first, use it if
// it doesn't error" heuristic gets this backwards for exactly the realistic
// case that matters.)
//
// A real UPDATE legitimately carries at most one 0.0.0.0/0 entry (sending the
// same default route two or more times in one message is not something any
// real implementation does), so two or more zero-length entries in the plain
// interpretation is a reliable tell that it is the spurious one. This does
// not resolve every conceivable adversarial byte sequence — that is
// information-theoretically impossible from structure alone when capabilities
// are genuinely unknown — but it resolves the realistic case this heuristic
// exists for.
func parsePrefixesV4Auto(b []byte, known, addPath bool) ([]Prefix, bool, bool, error) {
	if known {
		ps, err := ParsePrefixesV4(b, addPath)
		if err == nil || !addPath {
			return ps, addPath, false, err
		}
		// The capability said add-path and the bytes disagree. That is not
		// hypothetical: frr/frr-10.3-pair-eor-withdraw.bmpcap carries
		// `18 0a 0a 01` -- a plain /24 withdrawal, no Path Identifier -- on a
		// session whose Peer Up OPENs both advertise
		// `ADDPATH afi=1 safi=1 sendrecv=3`. A BMP implementation may
		// re-encode UPDATEs from its internal representation rather than
		// mirroring the original bytes, so the negotiated capability is a
		// statement about the BGP session, not a guarantee about what arrived
		// in the BMP stream.
		//
		// Recovering the prefix beats discarding it: the alternative, which
		// this replaces, dropped four real withdrawals per capture and
		// reported nothing at all. The plain reading is only used when it
		// parses cleanly, and it is flagged as a guess -- ADDPATH_HEURISTIC
		// means "parsed by inference rather than from capabilities", which is
		// exactly what this is.
		if plain, errPlain := ParsePrefixesV4(b, false); errPlain == nil {
			return plain, false, true, nil
		}
		// Neither reading works. Report the capability-directed error, which
		// is the more informative of the two.
		return nil, addPath, false, err
	}

	// For an empty region, both interpretations trivially return (nil, nil),
	// so report the heuristic as not fired — there was nothing to decide.
	if len(b) == 0 {
		return nil, addPath, false, nil
	}

	plain, errPlain := ParsePrefixesV4(b, false)
	withID, errAP := ParsePrefixesV4(b, true)

	switch {
	case errPlain == nil && errAP != nil:
		return plain, false, true, nil
	case errPlain != nil && errAP == nil:
		return withID, true, true, nil
	case errPlain != nil && errAP != nil:
		return nil, false, true, errPlain
	default:
		// Both structurally valid: genuinely ambiguous from bounds-checking
		// alone. Disambiguate using the zero-length-entry tell above.
		zeroEntries := 0
		for _, p := range plain {
			if p.Prefix.Bits() == 0 {
				zeroEntries++
			}
		}
		if zeroEntries >= 2 {
			return withID, true, true, nil
		}
		return plain, false, true, nil
	}
}

// ParsePrefixesV6 decodes a sequence of IPv6 unicast NLRI entries (RFC 4760
// §5, same shape as RFC 4271 §4.3: a 1-byte prefix length in bits followed by
// ceil(length/8) bytes of address). When addPath is true, each entry is
// preceded by a 4-byte Path Identifier.
//
// Identical in structure to ParsePrefixesV4 apart from the address width and
// the 128-bit ceiling. Kept as its own function rather than folded into a
// width-parameterized helper because ParsePrefixesV4 is exported and used
// widely; changing its signature to carry a width would ripple further than
// this family is worth.
func ParsePrefixesV6(b []byte, addPath bool) ([]Prefix, error) {
	var out []Prefix
	for len(b) > 0 {
		var p Prefix
		if addPath {
			if len(b) < 5 {
				return nil, fmt.Errorf("%w: add-path entry needs 5 bytes (path-id+length), %d remain", ErrPrefixTruncated, len(b))
			}
			p.PathID = binary.BigEndian.Uint32(b[:4])
			b = b[4:]
		}
		plen := int(b[0])
		b = b[1:]
		if plen > 128 {
			return nil, fmt.Errorf("%w: v6 prefix length %d", ErrPrefixLenInvalid, plen)
		}
		n := (plen + 7) / 8
		if len(b) < n {
			return nil, fmt.Errorf("%w: prefix of length %d needs %d bytes, %d remain", ErrPrefixTruncated, plen, n, len(b))
		}
		var a [16]byte
		copy(a[:], b[:n])
		b = b[n:]
		pfx := netip.PrefixFrom(netip.AddrFrom16(a), plen)
		if pfx != pfx.Masked() {
			// Host bits beyond the declared length: tolerated and normalized,
			// matching ParsePrefixesV4's behavior on real-router output.
			pfx = pfx.Masked()
		}
		p.Prefix = pfx
		out = append(out, p)
	}
	return out, nil
}
