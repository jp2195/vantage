package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

const (
	msgUpdate = 2 // RFC 4271 §4.1 message type: UPDATE

	attrOrigin     = 1  // RFC 4271 §5.1.1, well-known mandatory
	attrASPath     = 2  // RFC 4271 §5.1.2, well-known mandatory
	attrNextHop    = 3  // RFC 4271 §5.1.3, well-known mandatory
	attrMED        = 4  // RFC 4271 §5.1.4, optional non-transitive
	attrLocalPref  = 5  // RFC 4271 §5.1.5, well-known discretionary
	attrAtomicAgg  = 6  // RFC 4271 §5.1.6, well-known discretionary
	attrAggregator = 7  // RFC 4271 §5.1.7, optional transitive
	attrCommunity  = 8  // RFC 1997, optional transitive
	attrMPReach    = 14 // RFC 4760 §3, optional non-transitive
	attrMPUnreach  = 15 // RFC 4760 §3, optional non-transitive
	attrExtComm    = 16 // RFC 4360, optional transitive
	attrLargeComm  = 32 // RFC 8092, optional transitive
	attrLinkState  = 29 // RFC 9552 §5, the BGP-LS Attribute

	attrFlagExtLen = 0x10 // RFC 4271 §4.3: Extended Length attribute flag bit
)

// Sentinel errors for malformed UPDATE structure, distinguished by which
// length-prefixed region ran out of bytes: the UPDATE body's own
// withdrawn-routes-length/total-path-attribute-length fields, versus an
// individual path attribute's own header/declared length. Both are hard
// errors — ParseUpdate itself returns (nil, err) — as opposed to the RFC
// 7606 outcomes (TreatAsWithdraw / attribute discard) applied to a malformed
// *attribute value*; see ParseUpdate's doc comment for why the two are
// handled differently.
var (
	ErrUpdateTruncated = errors.New("bgp: update message truncated")
	ErrAttrTruncated   = errors.New("bgp: path attribute truncated")
)

// Update is the decoded form of one BGP UPDATE message (RFC 4271 §4.3),
// produced by ParseUpdate.
type Update struct {
	Family        Family
	Attrs         *vantagev1.PathAttributes
	Announced     []Prefix
	Withdrawn     []Prefix
	VpnAnnounced  []VpnPrefix
	VpnWithdrawn  []VpnPrefix
	EvpnAnnounced []EvpnRoute
	EvpnWithdrawn []EvpnRoute
	// LsNodes/LsLinks hold decoded BGP-LS NLRI announced via MP_REACH, and
	// LsAttrs the decoded BGP-LS Attribute (type 29) that describes them.
	// Attribute 29 was previously absent from this parser entirely, so
	// every link-state attribute -- SRGB, adjacency SIDs, IGP and TE
	// metrics -- was discarded before an envelope was built.
	LsNodes    []LsNodeNLRI
	LsLinks    []LsLinkNLRI
	LsPrefixes []LsPrefixNLRI
	LsAttrs    *LsAttrs
	// LsNodesWithdrawn/LsLinksWithdrawn hold BGP-LS NLRI withdrawn via
	// MP_UNREACH, mirroring the VpnAnnounced/VpnWithdrawn and
	// EvpnAnnounced/EvpnWithdrawn split above -- reach and unreach are kept
	// in separate slices for every other typed family, and BGP-LS is no
	// different: collapsing both into LsNodes/LsLinks would make a
	// withdrawn link indistinguishable from an announced one.
	LsNodesWithdrawn    []LsNodeNLRI
	LsLinksWithdrawn    []LsLinkNLRI
	LsPrefixesWithdrawn []LsPrefixNLRI
	// RawReach holds the MP_REACH attribute value bytes: set for a family
	// with no typed decoder, or when a typed decoder ran but left
	// something raw or failed.
	RawReach []byte
	// RawUnreach holds the MP_UNREACH attribute value bytes; same
	// population rule as RawReach.
	RawUnreach      []byte
	EndOfRIB        bool
	TreatAsWithdraw bool
	Flags           []vantagev1.ParseFlag
}

// flag appends f to u.Flags if not already present, so multiple call sites
// can raise the same condition without producing duplicate flags.
func (u *Update) flag(f vantagev1.ParseFlag) {
	if slices.Contains(u.Flags, f) {
		return
	}
	u.Flags = append(u.Flags, f)
}

// ParseUpdate parses a BGP UPDATE message body (RFC 4271 §4.3) — body is
// everything after the 19-byte BGP header — under the session's negotiated
// capabilities caps (Merge's output; a zero-value Caps with nil maps
// means "capabilities unknown", e.g. a route-monitoring message observed
// before this collector saw the session's Peer-Up/OPENs).
//
// Two different error-handling regimes are in play, matching two structurally
// different parts of an UPDATE:
//
//   - The withdrawn-routes and NLRI fields are each a sequence of
//     self-describing, but not independently-length-prefixed-as-a-whole,
//     prefix entries (RFC 4271 §4.3): a corrupt prefix length or a truncated
//     address makes it impossible to know where the *next* entry starts, so
//     there is no safe way to skip past just the bad part and keep reading.
//     A failure here (prefix.go's ErrPrefixTruncated/ErrPrefixLenInvalid) is
//     therefore a hard error: ParseUpdate returns (nil, err) immediately.
//   - Path attributes are individually length-prefixed TLVs: a malformed
//     attribute's own declared length still says exactly where it ends, so
//     parsing can safely continue past it. RFC 7606 formalizes this —
//     discard just the attribute, or treat the route as withdrawn, rather
//     than failing the whole message. See the per-type ladder below.
//
// End-of-RIB (RFC 4724 §2) is recognized in its two wire forms: the classic
// completely-empty UPDATE (withdrawn-routes-length=0, total-path-attribute-
// length=0, no NLRI) for IPv4 unicast, and — for any family, including one
// this package doesn't natively decode — an UPDATE whose only path attribute
// is MP_UNREACH_NLRI with an empty embedded NLRI field.
func ParseUpdate(body []byte, caps Caps) (*Update, error) {
	u := &Update{Family: FamilyIPv4U, Attrs: &vantagev1.PathAttributes{}}
	if len(body) < 4 {
		return nil, fmt.Errorf("%w: body is %d bytes, need at least 4", ErrUpdateTruncated, len(body))
	}
	wl := int(binary.BigEndian.Uint16(body[0:2]))
	if 2+wl+2 > len(body) {
		return nil, fmt.Errorf("%w: withdrawn-routes-length %d exceeds %d bytes available", ErrUpdateTruncated, wl, len(body)-4)
	}
	withdrawnB := body[2 : 2+wl]
	al := int(binary.BigEndian.Uint16(body[2+wl : 4+wl]))
	if 4+wl+al > len(body) {
		return nil, fmt.Errorf("%w: total-path-attribute-length %d exceeds %d bytes available", ErrUpdateTruncated, al, len(body)-4-wl)
	}
	attrsB := body[4+wl : 4+wl+al]
	nlriB := body[4+wl+al:]

	if wl == 0 && al == 0 && len(nlriB) == 0 {
		u.EndOfRIB = true
		return u, nil
	}

	// See Merge's/ParseOpen's doc comments: Merge always returns
	// non-nil maps for a real Peer-Up, so the "!= nil" term below is only
	// ever false for a genuinely zero-value Caps{} (no OPENs on record). In
	// the real-Peer-Up case the gate reduces to len(caps.MP) > 0 — which is
	// false only when capabilities are truly unknown, since even an
	// IPv4-unicast-only session with no MP capability at all now yields
	// MP[FamilyIPv4U]=true (RFC 4760's implicit default).
	known := caps.AddPathRecv != nil && (caps.AddPathRecv[FamilyIPv4U] || len(caps.MP) > 0)
	ap := caps.AddPathRecv[FamilyIPv4U]

	withdrawn, _, heur, err := parsePrefixesV4Auto(withdrawnB, known, ap)
	if err != nil {
		return nil, fmt.Errorf("bgp: withdrawn routes: %w", err)
	}
	u.Withdrawn = withdrawn
	if heur {
		u.flag(vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC)
	}

	// sawMPUnreachOnly / mpUnreachEmptyNLRI feed the MP End-of-RIB check
	// below. They are computed directly from the attribute's own declared
	// length here, not inferred later from u.Withdrawn/u.RawUnreach: u.Withdrawn
	// only ever holds classic IPv4-unicast withdraws -- a non-ipv4u family's
	// withdraws land in VpnWithdrawn/EvpnWithdrawn, or in RawUnreach when a
	// decoder couldn't fully type them, but never in u.Withdrawn -- so an
	// empty u.Withdrawn cannot by itself distinguish a genuinely empty
	// MP_UNREACH NLRI from a full withdraw list for some other family.
	sawMPUnreachOnly := len(attrsB) > 0
	mpUnreachEmptyNLRI := false
	// seenMPReach / seenMPUnreach guard against a second MP_REACH_NLRI or
	// MP_UNREACH_NLRI attribute in the same UPDATE. RFC 7606 §3
	// mandates a NOTIFICATION for a repeated instance of either; this
	// collector is passive and never resets a session, so a duplicate is
	// instead routed through the same treat-as-withdraw outcome the ladder
	// below already applies to a malformed MP_REACH/MP_UNREACH *value* --
	// and, critically, the duplicate is never handed to parseAttr, so the
	// first occurrence's Family/RawReach/RawUnreach survive
	// instead of being silently overwritten by the second, and the "only
	// attribute is an empty-NLRI MP_UNREACH" End-of-RIB condition below
	// cannot be faked by a real-NLRI MP_UNREACH followed by an empty one.
	seenMPReach, seenMPUnreach := false, false
	for len(attrsB) > 0 {
		if len(attrsB) < 3 {
			return nil, fmt.Errorf("%w: attribute header needs 3 bytes, %d remain", ErrAttrTruncated, len(attrsB))
		}
		flags, typ := attrsB[0], attrsB[1]
		var alen, off int
		if flags&attrFlagExtLen != 0 {
			if len(attrsB) < 4 {
				return nil, fmt.Errorf("%w: extended-length attribute header needs 4 bytes, %d remain", ErrAttrTruncated, len(attrsB))
			}
			alen, off = int(binary.BigEndian.Uint16(attrsB[2:4])), 4
		} else {
			alen, off = int(attrsB[2]), 3
		}
		if off+alen > len(attrsB) {
			return nil, fmt.Errorf("%w: attribute type %d declares %d bytes, %d available", ErrAttrTruncated, typ, alen, len(attrsB)-off)
		}
		val := attrsB[off : off+alen]
		attrsB = attrsB[off+alen:]

		if typ == attrMPUnreach {
			mpUnreachEmptyNLRI = len(val) == 3 // AFI(2)+SAFI(1), no NLRI
		} else {
			sawMPUnreachOnly = false
		}

		var perr error
		switch typ {
		case attrMPReach:
			if seenMPReach {
				perr = fmt.Errorf("mp_reach: duplicate MP_REACH_NLRI attribute (RFC 7606 §3)")
			} else {
				seenMPReach = true
				perr = parseAttr(u, typ, flags, val, caps)
			}
		case attrMPUnreach:
			if seenMPUnreach {
				perr = fmt.Errorf("mp_unreach: duplicate MP_UNREACH_NLRI attribute (RFC 7606 §3)")
			} else {
				seenMPUnreach = true
				perr = parseAttr(u, typ, flags, val, caps)
			}
		default:
			perr = parseAttr(u, typ, flags, val, caps)
		}

		if perr != nil {
			switch typ {
			case attrOrigin, attrASPath, attrNextHop, attrMED, attrCommunity, attrExtComm, attrLargeComm, attrMPReach, attrMPUnreach:
				// RFC 7606 §7.1/§7.2/§7.3 (ORIGIN/AS_PATH/NEXT_HOP), §7.4
				// (MULTI_EXIT_DISC), §7.8 (COMMUNITIES), §7.14 (Extended
				// Communities), and RFC 8092 §5 (Large Communities) all
				// mandate treat-as-withdraw for a malformed instance, and
				// MP_REACH/MP_UNREACH's own malformed-value and
				// duplicate-attribute cases above get the same outcome.
				// RFC 7606 §2 permits attribute-discard only for an
				// attribute "that has no effect on route selection or
				// installation" -- MED and all three community flavors
				// plainly do, so they cannot use the default branch below.
				//
				// LOCAL_PREF (§7.5, type 5) is deliberately NOT in this
				// list: RFC 7606 makes it treat-as-withdraw from an
				// internal neighbor but attribute-discard from an external
				// one, and this parser has no iBGP/eBGP knowledge (it
				// does no peer-AS comparison), so it falls through to the
				// conservative attribute-discard default below rather
				// than guessing a direction it cannot know.
				u.TreatAsWithdraw = true
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606)
			default:
				// RFC 7606 §2 "attribute discard": every other malformed
				// attribute is dropped (verbatim, for lossless audit) and
				// the route otherwise survives.
				u.Attrs.Unknown = append(u.Attrs.Unknown,
					&vantagev1.UnknownAttr{Type: uint32(typ), Flags: uint32(flags), Value: append([]byte{}, val...)})
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_ATTR_DISCARDED_7606)
			}
		}
	}

	if len(nlriB) > 0 {
		ann, _, heur2, err := parsePrefixesV4Auto(nlriB, known, ap)
		if err != nil {
			return nil, fmt.Errorf("bgp: nlri: %w", err)
		}
		if heur2 {
			u.flag(vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC)
		}
		u.Announced = append(u.Announced, ann...)
	}

	if sawMPUnreachOnly && mpUnreachEmptyNLRI && len(u.Withdrawn) == 0 &&
		len(u.Announced) == 0 && !u.TreatAsWithdraw {
		u.EndOfRIB = true
	}
	return u, nil
}

// parseAttr decodes one path attribute's value into u.Attrs. A non-nil
// return means the attribute's *value* was malformed for its declared type;
// the caller (ParseUpdate) applies the RFC 7606 ladder. A genuinely
// unrecognized type (the default case) is not an error: it is stored
// verbatim in Unknown and reported as such, with no flag — this package
// simply doesn't know what it means, which is unremarkable and expected
// (real OPENs/UPDATEs routinely carry attribute types a given consumer has
// no use for).
func parseAttr(u *Update, typ, flags uint8, v []byte, caps Caps) error {
	a := u.Attrs
	switch typ {
	case attrOrigin:
		if len(v) != 1 {
			return fmt.Errorf("origin: expected 1 byte, got %d", len(v))
		}
		if v[0] > 2 {
			// RFC 7606 §7.1: ORIGIN is malformed if its length is wrong *or*
			// its value is undefined. Only 0 (IGP), 1 (EGP), and 2
			// (INCOMPLETE) are defined; an undefined value must not be
			// stored silently (every accommodation sets a flag; this one
			// is not an accommodation at all).
			return fmt.Errorf("origin: undefined value %d", v[0])
		}
		a.Origin = uint32(v[0])
	case attrASPath:
		segs, err := parseASPath(v, caps.FourByteAS)
		if err != nil {
			return err
		}
		a.AsPath = segs
	case attrNextHop:
		if len(v) != 4 {
			return fmt.Errorf("next_hop: expected 4 bytes, got %d", len(v))
		}
		a.NextHop = netip.AddrFrom4([4]byte(v)).String()
	case attrMED:
		if len(v) != 4 {
			return fmt.Errorf("med: expected 4 bytes, got %d", len(v))
		}
		m := binary.BigEndian.Uint32(v)
		a.Med = &m
	case attrLocalPref:
		// Attribute-discard here is deliberate, not an oversight: RFC 7606
		// §7.5 makes a malformed LOCAL_PREF treat-as-withdraw from an
		// internal neighbor but attribute-discard from an external one, and
		// this parser has no iBGP/eBGP knowledge (it does no peer-AS
		// comparison) -- see the reclassification ladder in ParseUpdate
		// for the fuller rationale. Falling through to attribute-discard is
		// the conservative choice, not a guess at the missing distinction.
		if len(v) != 4 {
			return fmt.Errorf("local_pref: expected 4 bytes, got %d", len(v))
		}
		lp := binary.BigEndian.Uint32(v)
		a.LocalPref = &lp
	case attrAtomicAgg:
		// RFC 4271 §5.1.6: ATOMIC_AGGREGATE carries no value at all; its
		// mere presence is the signal. A non-zero length is malformed, not
		// silently-ignored extra data.
		if len(v) != 0 {
			return fmt.Errorf("atomic_aggregate: expected 0 bytes, got %d", len(v))
		}
		a.AtomicAggregate = true
	case attrAggregator:
		// The 4-byte-vs-2-byte-ASN encoding is self-describing from the
		// attribute's own length (8 vs 6 bytes), which is a more reliable
		// signal than trusting caps.FourByteAS blindly.
		switch len(v) {
		case 8:
			a.Aggregator = &vantagev1.Aggregator{Asn: binary.BigEndian.Uint32(v[0:4]),
				Ip: netip.AddrFrom4([4]byte(v[4:8])).String()}
		case 6:
			a.Aggregator = &vantagev1.Aggregator{Asn: uint32(binary.BigEndian.Uint16(v[0:2])),
				Ip: netip.AddrFrom4([4]byte(v[2:6])).String()}
		default:
			return fmt.Errorf("aggregator: expected 6 or 8 bytes, got %d", len(v))
		}
	case attrCommunity:
		if len(v) == 0 || len(v)%4 != 0 {
			// RFC 7606 §7.8: malformed unless length is a *non-zero*
			// multiple of 4 -- a zero length is not a valid empty list.
			return fmt.Errorf("communities: length %d not a non-zero multiple of 4", len(v))
		}
		for i := 0; i < len(v); i += 4 {
			a.Communities = append(a.Communities, binary.BigEndian.Uint32(v[i:i+4]))
		}
	case attrExtComm:
		if len(v) == 0 || len(v)%8 != 0 {
			// RFC 7606 §7.14: malformed unless length is a *non-zero*
			// multiple of 8.
			return fmt.Errorf("extended communities: length %d not a non-zero multiple of 8", len(v))
		}
		for i := 0; i < len(v); i += 8 {
			a.ExtendedCommunities = append(a.ExtendedCommunities, decodeExtComm(v[i:i+8]))
		}
	case attrLargeComm:
		if len(v) == 0 || len(v)%12 != 0 {
			// RFC 8092 §5: malformed unless the length is a *non-zero*
			// multiple of 12 -- a zero length is not a valid empty list.
			return fmt.Errorf("large_communities: length %d not a non-zero multiple of 12", len(v))
		}
		for i := 0; i < len(v); i += 12 {
			a.LargeCommunities = append(a.LargeCommunities, &vantagev1.LargeCommunity{
				GlobalAdmin: binary.BigEndian.Uint32(v[i : i+4]),
				LocalData1:  binary.BigEndian.Uint32(v[i+4 : i+8]),
				LocalData2:  binary.BigEndian.Uint32(v[i+8 : i+12]),
			})
		}
	case attrLinkState:
		lsa, err := DecodeLsAttrs(v)
		if err != nil {
			return fmt.Errorf("ls attr: %w", err)
		}
		u.LsAttrs = &lsa
		if len(lsa.Unknown) > 0 {
			// DecodeLsAttrs routing a TLV into Unknown is not an error --
			// the attribute otherwise decoded fine -- so nothing else on
			// this path calls u.flag for it. Without this, an unrecognized
			// BGP-LS attribute TLV was invisible on the parse-anomaly
			// dashboards, when an unrecognized TLV is exactly the case a
			// parse flag exists to raise.
			u.flag(vantagev1.ParseFlag_PARSE_FLAG_LS_TLV_UNKNOWN)
		}
	case attrMPReach:
		return parseMPReach(u, v, caps)
	case attrMPUnreach:
		return parseMPUnreach(u, v, caps)
	default:
		a.Unknown = append(a.Unknown,
			&vantagev1.UnknownAttr{Type: uint32(typ), Flags: uint32(flags), Value: append([]byte{}, v...)})
	}
	return nil
}

// parseASPath decodes AS_PATH (RFC 4271 §4.3/§5.1.2) or AS4_PATH-equivalent
// segment data: a sequence of (segment-type, count, ASNs...) segments,
// 2-byte or 4-byte ASNs per fourByte.
//
// Segment type 1 (AS_SET) and 2 (AS_SEQUENCE) are the RFC 4271 base types;
// 3 (AS_CONFED_SEQUENCE) and 4 (AS_CONFED_SET) are RFC 5065 §5's BGP
// confederation segment types, which a router that is itself a
// confederation member peering with a fellow member can legitimately send.
// Rejecting those as malformed would treat a well-formed confederation
// UPDATE as corrupt and force the whole route to treat-as-withdraw.
func parseASPath(v []byte, fourByte bool) ([]*vantagev1.AsPathSegment, error) {
	asLen := 2
	if fourByte {
		asLen = 4
	}
	var segs []*vantagev1.AsPathSegment
	for len(v) > 0 {
		if len(v) < 2 {
			return nil, fmt.Errorf("as_path: segment header truncated")
		}
		st, n := v[0], int(v[1])
		v = v[2:]
		if st == 0 || st > 4 {
			return nil, fmt.Errorf("as_path: segment type %d", st)
		}
		if len(v) < n*asLen {
			return nil, fmt.Errorf("as_path: segment declares %d ASNs (%d bytes), %d remain", n, n*asLen, len(v))
		}
		seg := &vantagev1.AsPathSegment{Type: uint32(st)}
		for i := range n {
			if fourByte {
				seg.Asns = append(seg.Asns, binary.BigEndian.Uint32(v[i*4:i*4+4]))
			} else {
				seg.Asns = append(seg.Asns, uint32(binary.BigEndian.Uint16(v[i*2:i*2+2])))
			}
		}
		v = v[n*asLen:]
		segs = append(segs, seg)
	}
	return segs, nil
}

// parseMPReach decodes MP_REACH_NLRI (RFC 4760 §3): AFI(2) SAFI(1)
// next-hop-length(1) next-hop(variable) reserved(1) NLRI(variable). For
// FamilyIPv4U, the next hop and NLRI are decoded. For any other family with a
// registered decodeNLRI entry (vpn4, lu4, EVPN), the next hop
// and the NLRI are both decoded into typed results (nextHopForFamily,
// per-family per RFC 4364/RFC 8277/RFC 7432 plus each one's RFC 8950 IPv6
// form) and the raw attribute value is dropped on a clean parse; the raw
// bytes survive whenever no decoder exists for the family,
// the NLRI decoder fails outright, the NLRI decodes some entries but leaves
// others raw, or the next hop's length isn't one nextHopForFamily recognizes
// for that family (PARSE_FLAG_NLRI_UNTYPED covers all three -- a decoder ran
// but something about this attribute could not be fully typed). A family
// with truly no decoder gets PARSE_FLAG_UNKNOWN_FAMILY instead -- that flag
// no longer fires for every non-ipv4u family, only a genuinely unrecognized
// one.
//
// The next-hop-length structural validation runs first, before any of that
// dispatch: u.Family is committed above
// unconditionally, so a malformed next-hop-length must still leave RawReach
// set and a flag raised rather than stranding Family with neither. Which flag
// it raises is decided by familyHasDecoder, not hard-coded to NLRI_UNTYPED:
// there is nothing IPv4-unicast-specific to validate for a family this
// package doesn't decode in the first place, but a family with no registered
// decoder at all is truthfully UNKNOWN_FAMILY even when the failure is this
// early. No decoder should see NLRI bytes whose start offset (4+nhLen+1)
// isn't even known to be in bounds, so the bound check itself cannot be
// deferred past this point regardless of which flag it ends up raising.
func parseMPReach(u *Update, v []byte, caps Caps) error {
	if len(v) < 5 {
		return fmt.Errorf("mp_reach: value is %d bytes, need at least 5", len(v))
	}
	fam := Family{AFI: binary.BigEndian.Uint16(v[0:2]), SAFI: v[2]}
	u.Family = fam
	nhLen := int(v[3])

	if fam != FamilyIPv4U {
		// The next-hop-length bound is family-independent (RFC 4760 §3 lays
		// the field out identically for every AFI/SAFI), so validate it
		// before anything else and before committing typed results.
		if len(v) < 4+nhLen+1 {
			u.RawReach = append([]byte{}, v...)
			if familyHasDecoder(fam) {
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED)
			} else {
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY)
			}
			return fmt.Errorf("mp_reach: next-hop-length %d exceeds value (%d bytes available)", nhLen, len(v)-4-1)
		}
		nh := v[4 : 4+nhLen]
		nlri := v[4+nhLen+1:]
		plain, vpn, evpn, lsNodes, lsLinks, lsPrefixes, untyped, decoded, derr := decodeNLRI(fam, nlri, caps.AddPathRecv[fam])
		switch {
		case !decoded:
			// No decoder for this family: the default fallback, unchanged.
			u.RawReach = append([]byte{}, v...)
			u.flag(vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY)
		case derr != nil:
			// A decoder ran and failed. Keep the bytes so reparse can try
			// again after a fix, flag it, and do not fail the message.
			u.RawReach = append([]byte{}, v...)
			u.flag(vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED)
		default:
			// ipv6 unicast lands in Announced, the same field ipv4 unicast
			// uses: both are plain prefixes, and netip.Prefix already carries
			// which family it is, so nothing downstream needs a second list.
			u.Announced = append(u.Announced, plain...)
			u.VpnAnnounced = append(u.VpnAnnounced, vpn...)
			u.EvpnAnnounced = append(u.EvpnAnnounced, evpn...)
			u.LsNodes = append(u.LsNodes, lsNodes...)
			u.LsLinks = append(u.LsLinks, lsLinks...)
			u.LsPrefixes = append(u.LsPrefixes, lsPrefixes...)
			if untyped {
				// decodeNLRI itself left something raw (an EVPN route type
				// this build doesn't type, or -- for BGP-LS -- an undecoded
				// NLRI type, i.e. prefix NLRI). BGP-LS gets its own, more
				// specific flag; every other family keeps the generic one.
				u.flag(nlriUntypedFlag(fam))
			}
			if LsHasUnknownTLVs(lsNodes, lsLinks, lsPrefixes) {
				// A Node/Link NLRI decodes as a structural success even
				// when one of its own TLVs (or a node descriptor's nested
				// sub-TLV) was unrecognized -- walkTLVs never errors on an
				// unknown type, it just routes the bytes into Unknown.
				// Without this, that silent routing stayed silent all the
				// way to ClickHouse: the bytes reached unknown_tlvs, but
				// nothing raised a flag saying so, and an unrecognized TLV
				// must be both preserved raw and flagged. No-op for every
				// non-BGP-LS family: decodeNLRI leaves lsNodes/lsLinks nil
				// for those.
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_LS_TLV_UNKNOWN)
			}
			// The NLRI decoded; now decode this family's next hop the same
			// way. The non-ipv4u branch previously never touched
			// u.Attrs.NextHop at all: RawReach was dropped on a clean NLRI
			// parse and the next hop bytes went nowhere, for every
			// cleanly-decoded vpn4/lu4/EVPN/BGP-LS route.
			if nhStr, ok := nextHopForFamily(fam, nh); ok {
				u.Attrs.NextHop = nhStr
			} else {
				// An unrecognized next-hop length for this family. Do not
				// guess at it and do not drop it: keep the raw bytes (the
				// only place it survives) and flag the attribute as only
				// partially typed, exactly like an untyped NLRI entry. This
				// is a next-hop problem, not an NLRI-type problem, so it
				// always uses the generic flag even for BGP-LS.
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED)
				untyped = true
			}
			if untyped {
				// Some entries decoded, some were carried raw, and/or the
				// next hop couldn't be recognized. Keep the bytes: the raw
				// ones are only recoverable from here.
				u.RawReach = append([]byte{}, v...)
			}
		}
		return nil
	}

	if len(v) < 4+nhLen+1 {
		return fmt.Errorf("mp_reach: next-hop-length %d exceeds value (%d bytes available)", nhLen, len(v)-4-1)
	}
	nh := v[4 : 4+nhLen]
	nlri := v[4+nhLen+1:] // +1: RFC 4760 §3 reserved byte

	switch nhLen {
	case 4:
		u.Attrs.NextHop = netip.AddrFrom4([4]byte(nh)).String()
	case 16, 32:
		// RFC 8950 (formerly RFC 5549): IPv4 unicast NLRI carried via
		// MP_REACH with an IPv6 next hop (unnumbered IPv4-over-IPv6
		// links are the common real deployment). 32 bytes additionally
		// carries an RFC 2545-style link-local address; only the global
		// address (first 16 bytes) is kept, matching standard RIB usage.
		u.Attrs.NextHop = netip.AddrFrom16([16]byte(nh[:16])).String()
	default:
		return fmt.Errorf("mp_reach: unsupported next-hop-length %d for ipv4-unicast", nhLen)
	}

	ps, err := ParsePrefixesV4(nlri, caps.AddPathRecv[FamilyIPv4U])
	if err != nil {
		return err
	}
	u.Announced = append(u.Announced, ps...)
	return nil
}

// parseMPUnreach decodes MP_UNREACH_NLRI (RFC 4760 §3): AFI(2) SAFI(1)
// NLRI(variable) — no next hop, unlike MP_REACH. For FamilyIPv4U the NLRI is
// decoded. For any other family with a registered decodeNLRI entry (vpn4,
// lu4, EVPN), the NLRI is decoded into typed withdraws and the raw
// attribute value is dropped on a clean parse; the raw bytes survive
// whenever no decoder exists for the family, the decoder fails outright,
// or it decodes some entries but leaves others raw
// (PARSE_FLAG_NLRI_UNTYPED) — regardless of whether the NLRI sub-field
// happens to be empty (an End-of-RIB marker, RFC 4724 §2) or not.
// ParseUpdate's End-of-RIB detection reads the attribute's own length
// directly rather than inferring it from this function's side effects,
// precisely because those side effects (RawUnreach, VpnWithdrawn,
// EvpnWithdrawn) don't vary between the two cases.
func parseMPUnreach(u *Update, v []byte, caps Caps) error {
	if len(v) < 3 {
		return fmt.Errorf("mp_unreach: value is %d bytes, need at least 3", len(v))
	}
	fam := Family{AFI: binary.BigEndian.Uint16(v[0:2]), SAFI: v[2]}
	u.Family = fam
	nlri := v[3:]

	if fam != FamilyIPv4U {
		plain, vpn, evpn, lsNodes, lsLinks, lsPrefixes, untyped, decoded, derr := decodeNLRI(fam, nlri, caps.AddPathRecv[fam])
		switch {
		case !decoded:
			// No decoder for this family: the default fallback, unchanged.
			u.RawUnreach = append([]byte{}, v...)
			u.flag(vantagev1.ParseFlag_PARSE_FLAG_UNKNOWN_FAMILY)
		case derr != nil:
			// A decoder ran and failed. Keep the bytes so reparse can try
			// again after a fix, flag it, and do not fail the message.
			u.RawUnreach = append([]byte{}, v...)
			u.flag(vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED)
		default:
			// ipv6 unicast withdrawals join Withdrawn, matching the way
			// announcements join Announced -- see parseMPReach.
			u.Withdrawn = append(u.Withdrawn, plain...)
			u.VpnWithdrawn = append(u.VpnWithdrawn, vpn...)
			u.EvpnWithdrawn = append(u.EvpnWithdrawn, evpn...)
			// Withdrawn, not LsNodes/LsLinks: those two are reserved for
			// MP_REACH-announced NLRI, mirroring the
			// Vpn/Evpn Announced/Withdrawn split. Collapsing reach and
			// unreach into one pair of slices made a withdrawn link
			// indistinguishable from an announced one.
			u.LsNodesWithdrawn = append(u.LsNodesWithdrawn, lsNodes...)
			u.LsLinksWithdrawn = append(u.LsLinksWithdrawn, lsLinks...)
			u.LsPrefixesWithdrawn = append(u.LsPrefixesWithdrawn, lsPrefixes...)
			if untyped {
				// Some entries decoded, some were carried raw. Keep the
				// bytes: the raw ones are only recoverable from here.
				u.RawUnreach = append([]byte{}, v...)
				u.flag(nlriUntypedFlag(fam))
			}
			if LsHasUnknownTLVs(lsNodes, lsLinks, lsPrefixes) {
				// See parseMPReach's identical check: a withdrawn Node/Link
				// NLRI can carry an unrecognized TLV exactly the same way an
				// announced one can.
				u.flag(vantagev1.ParseFlag_PARSE_FLAG_LS_TLV_UNKNOWN)
			}
		}
		return nil
	}

	ps, err := ParsePrefixesV4(nlri, caps.AddPathRecv[FamilyIPv4U])
	if err != nil {
		return err
	}
	u.Withdrawn = append(u.Withdrawn, ps...)
	return nil
}

// BuildUpdate + AppendUpdate: builder for tests and bmpgen (classic v4
// only — no MP_REACH/MP_UNREACH emission; hand-built raw byte slices cover
// that in tests, mirroring caps_test.go's buildRawOpen/rawOpen for OPEN).
type BuildUpdate struct {
	Announced   []Prefix
	Withdrawn   []Prefix
	AddPath     bool
	Origin      uint8
	ASPath      []uint32
	FourByteAS  bool
	NextHop     netip.Addr
	Communities []uint32

	// MP carries a non-IPv4-unicast family. When set, AppendUpdate emits
	// RFC 4760 MP_REACH_NLRI / MP_UNREACH_NLRI attributes; the Announced and
	// Withdrawn fields above stay IPv4 unicast and are independent of it, so
	// a message can legitimately carry both (real routers do).
	MP *BuildMP
}

// BuildMP is one family's MP_REACH/MP_UNREACH content.
//
// Which of the two prefix lists applies is decided by the family, mirroring
// what decodeNLRI returns for it: the labeled families (vpn4, vpn6, lu4) carry
// VpnPrefix entries, and the plain ones (ipv6u) carry Prefix. Using the same
// split as the decoder is deliberate -- it means a caller who knows how a
// family comes OUT of this package already knows how to put it in.
type BuildMP struct {
	Family Family

	// NextHop is encoded per the family's own rule, which differs by family
	// and is the part most easily got wrong: vpn4/vpn6 prefix the address
	// with an 8-byte zero Route Distinguisher (RFC 4364 §4.3.2, RFC 4659
	// §3.2.1.1), while lu4 and ipv6u carry a bare address. AppendUpdate
	// applies the right one; callers pass the address itself.
	NextHop netip.Addr

	// Labeled families (vpn4, vpn6, lu4).
	Announced []VpnPrefix
	Withdrawn []VpnPrefix

	// Plain families (ipv6u).
	PlainAnnounced []Prefix
	PlainWithdrawn []Prefix

	// EVPN, whose NLRI is typed rather than a prefix -- each route type has
	// its own layout (see AppendEvpnNLRI).
	EvpnAnnounced []EvpnRoute
	EvpnWithdrawn []EvpnRoute

	// BGP-LS Node NLRI (see AppendLsNodeNLRI). Link and Prefix NLRI are not
	// built yet.
	LsNodesAnnounced []LsNodeNLRI
	LsNodesWithdrawn []LsNodeNLRI
}

// attrFlags returns the wire-correct RFC 4271 §4.3 attribute-flags byte for
// typ, per each attribute's own defining RFC. Well-known attributes
// (mandatory or discretionary) are transitive by definition (0x40); optional
// attributes vary: MED (RFC 4271 §5.1.4) and MP_REACH/MP_UNREACH (RFC 4760
// §3/§4, each explicitly "an optional non-transitive attribute") are
// optional non-transitive (0x80), while AGGREGATOR (RFC 4271 §5.1.7),
// COMMUNITIES (RFC 1997), Extended Communities (RFC 4360), and Large
// Communities (RFC 8092) are optional transitive (0xC0). AppendUpdate only
// emits a subset of these today (see its doc comment), but the mapping is
// kept complete so it stays correct if a future BuildUpdate field starts
// emitting one of the others.
func attrFlags(typ uint8) uint8 {
	switch typ {
	case attrMED, attrMPReach, attrMPUnreach:
		return 0x80 // optional non-transitive
	case attrAggregator, attrCommunity, attrExtComm, attrLargeComm:
		return 0xC0 // optional transitive
	default:
		return 0x40 // well-known (ORIGIN, AS_PATH, NEXT_HOP, LOCAL_PREF, ATOMIC_AGGREGATE)
	}
}

// AppendUpdate builds a full BGP UPDATE message (19-byte header + body).
//
// Panics if the withdrawn-routes length, path-attributes length, overall
// message length, or any single attribute's value would overflow the wire
// format's length fields — callers pass known-good test/bmpgen values, not
// attacker-controlled wire input, so per this package's
// builders-guard-their-preconditions convention (AppendOpen, bmp.AppendTLV)
// this fails loudly rather than silently truncating a length field and
// emitting a corrupt message. In particular, an attribute value over 255
// bytes cannot be represented in this builder's non-extended-length 1-byte
// attribute-length field (e.g. 64 communities is 256 bytes: byte(256) == 0
// would silently truncate the length to zero and leave the 256 value bytes
// to be misread as further attribute headers) — this builder does not emit
// the RFC 4271 §4.3 Extended Length form, so such a value panics instead of
// producing a corrupt message. NextHop must be a valid IPv4 (or
// IPv4-mapped IPv6) address whenever Announced is non-empty; netip.Addr.As4
// panics otherwise.
func AppendUpdate(dst []byte, b BuildUpdate) []byte {
	var attrs []byte
	appendAttr := func(typ uint8, val []byte) {
		switch {
		case len(val) > 0xFFFF:
			panic(fmt.Sprintf("bgp: AppendUpdate: attribute type %d value is %d bytes, "+
				"exceeding even the Extended Length form's 2-byte field", typ, len(val)))
		case len(val) > 0xFF:
			// RFC 4271 §4.3 Extended Length: set the flag bit and use a
			// 2-byte length. Required for MP_REACH at any realistic prefix
			// count -- a vpn4 entry is ~16 bytes, so the 1-byte form caps a
			// message near 16 prefixes, and this builder feeds a generator
			// whose whole purpose is high counts.
			attrs = append(attrs, attrFlags(typ)|attrFlagExtLen, typ,
				byte(len(val)>>8), byte(len(val)))
		default:
			attrs = append(attrs, attrFlags(typ), typ, byte(len(val)))
		}
		attrs = append(attrs, val...)
	}
	if len(b.Announced) > 0 {
		if b.Origin > 2 {
			// RFC 7606 §7.1 makes an undefined ORIGIN value malformed, and
			// ParseUpdate treats it as withdraw. Emitting one would build a
			// fixture this package's own parser rejects, so guard it here
			// the way the attribute-length precondition above is guarded.
			panic(fmt.Sprintf("bgp: AppendUpdate: ORIGIN value %d is undefined; valid values are 0 (IGP), 1 (EGP), 2 (INCOMPLETE)", b.Origin))
		}
		appendAttr(attrOrigin, []byte{b.Origin})
		var ap []byte
		if len(b.ASPath) > 0 {
			ap = append(ap, 2, byte(len(b.ASPath)))
			for _, as := range b.ASPath {
				if b.FourByteAS {
					ap = binary.BigEndian.AppendUint32(ap, as)
				} else {
					ap = binary.BigEndian.AppendUint16(ap, uint16(as))
				}
			}
		}
		appendAttr(attrASPath, ap)
		nh := b.NextHop.As4()
		appendAttr(attrNextHop, nh[:])
		if len(b.Communities) > 0 {
			var cv []byte
			for _, c := range b.Communities {
				cv = binary.BigEndian.AppendUint32(cv, c)
			}
			appendAttr(attrCommunity, cv)
		}
	}

	// MP families. The ORIGIN and AS_PATH above are emitted only when there
	// is IPv4-unicast NLRI, but an MP_REACH message needs them too -- RFC 4271
	// §5 makes ORIGIN and AS_PATH well-known mandatory for any UPDATE that
	// carries reachability, whatever family it is in. Without this an MP-only
	// message is missing mandatory attributes and ParseUpdate applies RFC 7606
	// treat-as-withdraw, so the routes arrive as withdrawals.
	if b.MP != nil {
		if len(b.Announced) == 0 && (len(b.MP.Announced) > 0 || len(b.MP.PlainAnnounced) > 0 || len(b.MP.EvpnAnnounced) > 0 || len(b.MP.LsNodesAnnounced) > 0) {
			if b.Origin > 2 {
				panic(fmt.Sprintf("bgp: AppendUpdate: ORIGIN value %d is undefined", b.Origin))
			}
			appendAttr(attrOrigin, []byte{b.Origin})
			var ap []byte
			if len(b.ASPath) > 0 {
				ap = append(ap, 2, byte(len(b.ASPath)))
				for _, as := range b.ASPath {
					if b.FourByteAS {
						ap = binary.BigEndian.AppendUint32(ap, as)
					} else {
						ap = binary.BigEndian.AppendUint16(ap, uint16(as))
					}
				}
			}
			appendAttr(attrASPath, ap)
			if len(b.Communities) > 0 {
				var cv []byte
				for _, c := range b.Communities {
					cv = binary.BigEndian.AppendUint32(cv, c)
				}
				appendAttr(attrCommunity, cv)
			}
		}
		if len(b.MP.Announced) > 0 || len(b.MP.PlainAnnounced) > 0 || len(b.MP.EvpnAnnounced) > 0 || len(b.MP.LsNodesAnnounced) > 0 {
			appendAttr(attrMPReach, appendMPReach(b.MP, b.AddPath))
		}
		if len(b.MP.Withdrawn) > 0 || len(b.MP.PlainWithdrawn) > 0 || len(b.MP.EvpnWithdrawn) > 0 || len(b.MP.LsNodesWithdrawn) > 0 {
			appendAttr(attrMPUnreach, appendMPUnreach(b.MP, b.AddPath))
		}
	}

	withdrawn := AppendPrefixesV4(nil, b.Withdrawn, b.AddPath)
	if len(withdrawn) > 0xFFFF {
		panic(fmt.Sprintf("bgp: AppendUpdate: withdrawn-routes %d bytes exceeds the 2-byte length field's range", len(withdrawn)))
	}
	if len(attrs) > 0xFFFF {
		panic(fmt.Sprintf("bgp: AppendUpdate: path attributes %d bytes exceeds the 2-byte length field's range", len(attrs)))
	}

	body := append([]byte{byte(len(withdrawn) >> 8), byte(len(withdrawn))}, withdrawn...)
	body = append(body, byte(len(attrs)>>8), byte(len(attrs)))
	body = append(body, attrs...)
	body = AppendPrefixesV4(body, b.Announced, b.AddPath)

	if bgpHeaderLen+len(body) > 0xFFFF {
		panic(fmt.Sprintf("bgp: AppendUpdate: total message length %d exceeds the 2-byte length field's range", bgpHeaderLen+len(body)))
	}

	for range 16 {
		dst = append(dst, 0xFF)
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(bgpHeaderLen+len(body)))
	dst = append(dst, msgUpdate)
	return append(dst, body...)
}

// appendLabeledNLRI encodes VpnPrefix entries in the wire format
// parseLabeledCommon decodes: per entry, an optional 4-byte Path Identifier,
// then ONE length byte giving the total length IN BITS of everything that
// follows in the entry -- label stack, plus the 8-byte Route Distinguisher
// when withRD, plus the prefix -- then those fields in that order.
//
// The bit-count length is the trap this mirrors from the decoder: it is not a
// byte count, and it covers the labels and RD as well as the prefix. Getting
// it wrong produces an entry that decodes to a plausible but wrong prefix
// rather than an error, so this is derived the same way the decoder derives
// it, from the same three components.
//
// The bottom-of-stack bit is set on the last label. Without it a decoder walks
// past the stack hunting for a terminator that never comes and consumes the RD
// as if it were more labels.
func appendLabeledNLRI(dst []byte, ps []VpnPrefix, addPath, withRD bool) []byte {
	for _, p := range ps {
		if addPath {
			dst = binary.BigEndian.AppendUint32(dst, p.PathID)
		}
		var body []byte
		for i, l := range p.Labels {
			v := l << 4
			if i == len(p.Labels)-1 {
				v |= 1 // bottom of stack
			}
			body = append(body, byte(v>>16), byte(v>>8), byte(v))
		}
		if withRD {
			rd, err := encodeRD(p.RD)
			if err != nil {
				panic(fmt.Sprintf("bgp: AppendUpdate: route distinguisher %q: %v", p.RD, err))
			}
			body = append(body, rd...)
		}
		plen := p.Prefix.Bits()
		addr := p.Prefix.Addr()
		var ab []byte
		if addr.Is4() {
			a := addr.As4()
			ab = a[:(plen+7)/8]
		} else {
			a := addr.As16()
			ab = a[:(plen+7)/8]
		}
		body = append(body, ab...)

		totalBits := len(p.Labels)*24 + plen
		if withRD {
			totalBits += 64
		}
		if totalBits > 0xFF {
			panic(fmt.Sprintf("bgp: AppendUpdate: NLRI entry is %d bits, exceeds the 1-byte length field", totalBits))
		}
		dst = append(dst, byte(totalBits))
		dst = append(dst, body...)
	}
	return dst
}

// encodeRD renders a Route Distinguisher text form back to its 8 wire bytes,
// the inverse of decodeRD. Only the two ASN forms are emitted: type 0 when the
// administrator fits in 16 bits and type 2 when it does not. The IPv4 form
// (type 1) is accepted on the wire by decodeRD but never produced here --
// nothing in this project originates one, and guessing which form a caller
// meant from an ambiguous string is how a builder starts disagreeing with its
// own parser.
func encodeRD(s string) ([]byte, error) {
	admin, assigned, ok := strings.Cut(s, ":")
	if !ok {
		return nil, fmt.Errorf("want ASN:value")
	}
	a, err := strconv.ParseUint(admin, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("administrator %q is not a 32-bit number (the IPv4 form is not emitted)", admin)
	}
	v, err := strconv.ParseUint(assigned, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("assigned number %q is not a 32-bit number", assigned)
	}
	out := make([]byte, 8)
	if a <= 0xFFFF {
		binary.BigEndian.PutUint16(out[0:2], 0)
		binary.BigEndian.PutUint16(out[2:4], uint16(a))
		binary.BigEndian.PutUint32(out[4:8], uint32(v))
		return out, nil
	}
	if v > 0xFFFF {
		return nil, fmt.Errorf("4-byte administrator %d needs a 2-byte assigned number, got %d", a, v)
	}
	binary.BigEndian.PutUint16(out[0:2], 2)
	binary.BigEndian.PutUint32(out[2:6], uint32(a))
	binary.BigEndian.PutUint16(out[6:8], uint16(v))
	return out, nil
}

// mpNextHop encodes nh per fam's own next-hop rule (see nextHopForFamily,
// which decodes these same shapes).
func mpNextHop(fam Family, nh netip.Addr) []byte {
	var out []byte
	if fam == FamilyVPNv4 || fam == FamilyVPNv6 {
		// RFC 4364 §4.3.2 / RFC 4659 §3.2.1.1: an 8-byte Route Distinguisher
		// field, always zero because a next hop has no RD of its own.
		out = make([]byte, 8)
	}
	if nh.Is4() {
		a := nh.As4()
		return append(out, a[:]...)
	}
	a := nh.As16()
	return append(out, a[:]...)
}

// appendMPReach builds an MP_REACH_NLRI attribute value (RFC 4760 §3):
// AFI(2) SAFI(1) next-hop-length(1) next-hop(var) reserved(1) NLRI(var).
func appendMPReach(mp *BuildMP, addPath bool) []byte {
	v := binary.BigEndian.AppendUint16(nil, mp.Family.AFI)
	v = append(v, mp.Family.SAFI)
	nh := mpNextHop(mp.Family, mp.NextHop)
	v = append(v, byte(len(nh)))
	v = append(v, nh...)
	v = append(v, 0) // reserved
	return appendMPNLRI(v, mp.Family, mp.Announced, mp.PlainAnnounced, mp.EvpnAnnounced, mp.LsNodesAnnounced, addPath)
}

// appendMPUnreach builds an MP_UNREACH_NLRI attribute value (RFC 4760 §4):
// AFI(2) SAFI(1) NLRI(var) -- no next hop, unlike MP_REACH.
func appendMPUnreach(mp *BuildMP, addPath bool) []byte {
	v := binary.BigEndian.AppendUint16(nil, mp.Family.AFI)
	v = append(v, mp.Family.SAFI)
	return appendMPNLRI(v, mp.Family, mp.Withdrawn, mp.PlainWithdrawn, mp.EvpnWithdrawn, mp.LsNodesWithdrawn, addPath)
}

// appendMPNLRI encodes the NLRI region for fam, choosing the labeled or plain
// encoding the way decodeNLRI chooses which to return.
func appendMPNLRI(dst []byte, fam Family, labeled []VpnPrefix, plain []Prefix, evpn []EvpnRoute, lsNodes []LsNodeNLRI, addPath bool) []byte {
	switch fam {
	case FamilyVPNv4, FamilyVPNv6:
		return appendLabeledNLRI(dst, labeled, addPath, true)
	case FamilyLU4:
		return appendLabeledNLRI(dst, labeled, addPath, false)
	case FamilyIPv6U:
		return appendPrefixesV6(dst, plain, addPath)
	case FamilyEVPN:
		return AppendEvpnNLRI(dst, evpn, addPath)
	case FamilyBGPLS:
		return AppendLsNodeNLRI(dst, lsNodes)
	default:
		panic(fmt.Sprintf("bgp: AppendUpdate: no MP encoder for family %v; "+
			"add one here and to decodeNLRI together, or the builder and parser drift apart", fam))
	}
}

// appendPrefixesV6 encodes ps in the wire format ParsePrefixesV6 decodes.
func appendPrefixesV6(dst []byte, ps []Prefix, addPath bool) []byte {
	for _, p := range ps {
		if addPath {
			dst = binary.BigEndian.AppendUint32(dst, p.PathID)
		}
		plen := p.Prefix.Bits()
		dst = append(dst, byte(plen))
		a := p.Prefix.Addr().As16()
		dst = append(dst, a[:(plen+7)/8]...)
	}
	return dst
}
