package bgp

// BGP-LS (RFC 9552) NLRI decoding.
//
// A BGP-LS NLRI is Type(2) Length(2) Value, and the Value's shape depends on
// the type: a Node NLRI carries protocol, identifier and one node descriptor;
// a Link NLRI carries protocol, identifier, two node descriptors and a set of
// link descriptors. Node descriptors are themselves sub-TLVs, which is why a
// router-ID alone does not identify a node -- the AS, BGP-LS identifier and
// area are part of its identity.
//
// IPv4 Prefix NLRI (type 3) and IPv6 Prefix NLRI (type 4) both decode, into
// the same LsPrefixNLRI shape -- see lsNLRIPrefixV6 below for why one type
// serves both, and for the history of that decoder's own comment being
// wrong about this. Undecoded NLRI is counted and reported so the caller
// can keep the raw bytes and flag them rather than report an empty
// success.

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"strings"
)

// BGP-LS NLRI types (RFC 9552 §5.2).
const (
	lsNLRINode     = 1
	lsNLRILink     = 2
	lsNLRIPrefix   = 3 // IPv4 Topology Prefix.
	lsNLRIPrefixV6 = 4 // IPv6 Topology Prefix.

	// IPv6 Topology Prefix (type 4) decodes as of 2026-09-01, through the
	// same decodeLsPrefix as type 3 below: RFC 9552 §5.2.3's descriptors are
	// identical for the two types, and Prefix is a netip.Prefix, which
	// already carries its own family, so nothing downstream needs a new
	// column to tell them apart.
	//
	// It stayed undecoded past the point it should have on two claims that
	// were each true once and wrong when repeated. First, that no IPv6 IGP
	// was running, so decoding would mean testing a reading of RFC 9552
	// against fixtures built from that same reading -- true when written on
	// 2026-08-17, false from 2026-08-29 19:00, when the archive began
	// carrying IPv6 IS-IS and held 74 type-4 NLRI sitting undecoded in
	// ls_events.raw_reach. Second, "no captured example," repeated the
	// next day without re-checking, when the archive already held one.
	// Both were corrected on 2026-09-01 before this NLRI was implemented,
	// the same trigger type 3 fired on 2026-08-17.
	//
	// The lesson that outlived the fix: this comment is as much a claim
	// about the code as the code is, and it goes stale the same way any
	// unread note does -- re-read it before repeating what it says.
)

// Node-descriptor sub-TLVs (RFC 9552 §5.2.1.4) and link-descriptor TLVs
// (§5.2.2).
const (
	lsTLVLocalNodeDesc  = 256
	lsTLVRemoteNodeDesc = 257
	lsTLVLinkLocalRemID = 258
	lsTLVIPv4IfAddr     = 259
	lsTLVIPv4NeighAddr  = 260

	// Prefix-descriptor TLVs (RFC 9552 SS5.2.3). 263 (Multi-Topology ID) is
	// accepted and kept raw, and real examples exist. Measured through the
	// decoder on 2026-09-01: 166 of the 1,272 prefixes in a lab archive's
	// raw NLRI carry it, and 346 ls_nodes rows already hold it in
	// unknown_tlvs.
	//
	// It stays raw for a better reason. Every value on the prefixes is the
	// same (MT-ID 2) and the node rows carry two, so a typed column would be
	// a near-constant -- and it would BE a new column, in a schema whose own
	// header says it creates and never migrates, with no in-place upgrade
	// path. That trade is a database recreation for a field the
	// unknown_tlvs map already preserves, keyed by TLV number and queryable
	// today. Nothing is being lost by leaving it there.
	//
	// The trigger to revisit is not another capture -- captures exist. It is
	// a caller who needs to FILTER on MT-ID, which the map cannot serve
	// efficiently, or a lab that runs enough topologies for the value to
	// vary meaningfully.
	lsTLVMultiTopoID    = 263
	lsTLVOSPFRouteType  = 264
	lsTLVIPReachability = 265

	lsTLVAutonomousSystem = 512
	lsTLVBGPLSID          = 513
	lsTLVOSPFArea         = 514
	lsTLVIGPRouterID      = 515
)

// LsNodeDescriptor identifies one node. Every field is part of the identity:
// a router-ID is unique only within an AS and area, so joining a link's far
// end to a node on router-ID alone can match the wrong node and grow a wrong
// edge in a topology rather than failing visibly.
type LsNodeDescriptor struct {
	ASN      uint32
	BGPLSID  uint32
	Area     uint32
	RouterID []byte
}

type LsNodeNLRI struct {
	Protocol   uint8
	Identifier uint64
	Local      LsNodeDescriptor
	// Unknown holds NLRI-level TLVs this build does not recognize: this
	// Node NLRI's own top-level TLVs plus its node descriptor's
	// nested sub-TLVs (decodeNodeDescriptor writes into the same map -- see
	// its doc comment for why the two key spaces are merged rather than kept
	// apart). Always non-nil, matching LsAttrs.Unknown's convention.
	Unknown map[uint16][]byte
}

type LsLinkNLRI struct {
	Protocol     uint8
	Identifier   uint64
	Local        LsNodeDescriptor
	Remote       LsNodeDescriptor
	LocalIfAddr  netip.Addr
	RemoteIfAddr netip.Addr
	LinkLocalID  uint32
	LinkRemoteID uint32
	// HasLinkID reports whether TLV 258 (Link Local/Remote Identifiers)
	// appeared in THIS Link NLRI's own descriptor list, RFC 9552 §5.2.2's
	// literal placement for it. See LsAttrs.HasLinkID for the other place a
	// real router was observed putting the same TLV number, and
	// collector.lsLinkMessage for how the two are reconciled.
	HasLinkID bool
	// Unknown: see LsNodeNLRI.Unknown -- this Link NLRI's own top-level TLVs
	// (261/262 IPv6 interface/neighbor address, 263 Multi-Topology ID, and
	// any genuinely unrecognized type) plus both node descriptors' nested
	// sub-TLVs, all merged into one map for the same reason.
	Unknown map[uint16][]byte
}

// LsPrefixNLRI is an IPv4 Topology Prefix NLRI (RFC 9552 SS5.2.3): a prefix,
// plus the node that advertises reachability to it. The node half is the same
// Local Node Descriptor a Node NLRI carries, which is what lets a prefix be
// joined back to its node on the same derived key ls_links uses for its
// endpoints -- see sink/rows.go.
type LsPrefixNLRI struct {
	Protocol   uint8
	Identifier uint64
	Local      LsNodeDescriptor
	// OSPFRouteType is TLV 264 (RFC 9552 SS5.2.3.1): 1 intra-area, 2 inter-area,
	// 3 external-1, 4 external-2, 5 NSSA-1, 6 NSSA-2. Zero means the TLV was
	// absent, which is legal for protocols other than OSPF.
	OSPFRouteType uint8
	// Prefix is TLV 265 (IP Reachability Information), whose value is a
	// one-byte prefix length followed by only the significant bytes -- a /31
	// and a /32 both carry 4, a /24 carries 3. Decoded here into a whole
	// address so consumers never have to re-derive the padding.
	Prefix netip.Prefix
	// Unknown: see LsNodeNLRI.Unknown. Holds this Prefix NLRI's own
	// unrecognized top-level TLVs (263 Multi-Topology ID among them) and the
	// node descriptor's nested sub-TLVs.
	Unknown map[uint16][]byte
}

// decodeIPReach decodes TLV 265's one-byte prefix length plus packed
// significant bytes (RFC 9552 SS5.2.3.2) into a netip.Prefix.
//
// The length check is against ceil(bits/8) rather than a fixed 4: a shorter
// prefix legitimately carries fewer bytes, and requiring 4 would reject every
// prefix shorter than /25 that a real network advertises. Trailing bytes
// beyond what the prefix length calls for are a malformed TLV, not padding to
// ignore -- accepting them would let two different encodings of one prefix
// both decode, and the prefix is part of this row's identity.
// The family comes from the NLRI TYPE, not from the TLV: RFC 9552 uses the
// same TLV 265 for both, and its value carries no family discriminator of its
// own. Type 3 and type 4 differ only in how many bits are legal and how wide
// an address the packed bytes fill, so v6 is a parameter rather than a second
// function -- one body, two bounds, and no chance of the two drifting.
//
// Each family keeps its OWN cap. A type-3 NLRI claiming /64 is malformed, and
// widening the v4 bound to 128 to save a branch would build a 4-byte address
// out of eight packed bytes and call it a prefix.
func decodeIPReach(val []byte, v6 bool) (netip.Prefix, error) {
	if len(val) < 1 {
		return netip.Prefix{}, fmt.Errorf("ls prefix nlri: ip reachability tlv is empty")
	}
	max, family := 32, "ipv4"
	if v6 {
		max, family = 128, "ipv6"
	}
	bits := int(val[0])
	if bits > max {
		return netip.Prefix{}, fmt.Errorf("ls prefix nlri: prefix length %d exceeds %d for an %s prefix NLRI", bits, max, family)
	}
	want := (bits + 7) / 8
	if len(val)-1 != want {
		return netip.Prefix{}, fmt.Errorf("ls prefix nlri: prefix length %d needs %d packed bytes, tlv carries %d", bits, want, len(val)-1)
	}
	if v6 {
		var addr [16]byte
		copy(addr[:], val[1:])
		return netip.PrefixFrom(netip.AddrFrom16(addr), bits), nil
	}
	var addr [4]byte
	copy(addr[:], val[1:])
	return netip.PrefixFrom(netip.AddrFrom4(addr), bits), nil
}

func decodeLsPrefix(v []byte, v6 bool) (LsPrefixNLRI, error) {
	proto, id, rest, err := lsNLRIHeader(v)
	if err != nil {
		return LsPrefixNLRI{}, err
	}
	p := LsPrefixNLRI{Protocol: proto, Identifier: id, Unknown: map[uint16][]byte{}}
	seenReach := false
	err = walkTLVs(rest, func(typ uint16, val []byte) error {
		switch typ {
		case lsTLVLocalNodeDesc:
			d, err := decodeNodeDescriptor(val, p.Unknown)
			if err != nil {
				return err
			}
			p.Local = d
		case lsTLVOSPFRouteType:
			// Wrong-width known TLVs are rejected rather than truncated, the
			// same rule decodeLsLink applies to 258/259/260.
			if len(val) != 1 {
				return fmt.Errorf("ls prefix nlri: ospf route type tlv is %d bytes, want 1", len(val))
			}
			p.OSPFRouteType = val[0]
		case lsTLVIPReachability:
			pfx, err := decodeIPReach(val, v6)
			if err != nil {
				return err
			}
			p.Prefix = pfx
			seenReach = true
		default:
			p.Unknown[typ] = append([]byte(nil), val...)
		}
		return nil
	})
	if err != nil {
		return LsPrefixNLRI{}, err
	}
	// Same reasoning as decodeLsNode's router-id check: a prefix NLRI whose
	// descriptor has no router-id cannot be joined to a node, and one with no
	// IP Reachability TLV has no prefix -- it is not a row worth writing, and
	// a zero-valued 0.0.0.0/0 would look like a default route rather than a
	// decode failure.
	if len(p.Local.RouterID) == 0 {
		return LsPrefixNLRI{}, fmt.Errorf("ls prefix nlri: local node descriptor has no IGP router-id")
	}
	if !seenReach {
		return LsPrefixNLRI{}, fmt.Errorf("ls prefix nlri: no ip reachability tlv (265)")
	}
	return p, nil
}

// DecodeLsNLRI decodes the NLRI list from an MP_REACH or MP_UNREACH value
// whose AFI/SAFI (and, for MP_REACH, next-hop and reserved byte) have already
// been stripped. undecoded counts NLRI of a type this does not handle.
func DecodeLsNLRI(b []byte) (nodes []LsNodeNLRI, links []LsLinkNLRI, prefixes []LsPrefixNLRI, undecoded int, err error) {
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, nil, nil, 0, fmt.Errorf("ls nlri: %d trailing bytes, need at least a 4-byte header", len(b))
		}
		typ := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+l {
			return nil, nil, nil, 0, fmt.Errorf("ls nlri: type %d declares %d bytes, %d remain", typ, l, len(b)-4)
		}
		v := b[4 : 4+l]
		b = b[4+l:]

		switch typ {
		case lsNLRINode:
			n, err := decodeLsNode(v)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			nodes = append(nodes, n)
		case lsNLRILink:
			l, err := decodeLsLink(v)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			links = append(links, l)
		case lsNLRIPrefix, lsNLRIPrefixV6:
			// Both land in the same LsPrefixNLRI and the same ls_prefixes
			// row: the descriptors RFC 9552 5.2.3 defines are identical for
			// the two types, and Prefix is a netip.Prefix, which carries its
			// own family. Nothing downstream needs a new column to tell them
			// apart -- the stored prefix text does, exactly as query/'s
			// covers predicate already discriminates v4 from v6.
			p, err := decodeLsPrefix(v, typ == lsNLRIPrefixV6)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			prefixes = append(prefixes, p)
		default:
			undecoded++
		}
	}
	return nodes, links, prefixes, undecoded, nil
}

// header splits the Protocol-ID and Identifier every Node/Link/Prefix NLRI
// begins with (RFC 9552 §5.2).
func lsNLRIHeader(v []byte) (proto uint8, id uint64, rest []byte, err error) {
	if len(v) < 9 {
		return 0, 0, nil, fmt.Errorf("ls nlri: %d bytes, need 9 for protocol+identifier", len(v))
	}
	return v[0], binary.BigEndian.Uint64(v[1:9]), v[9:], nil
}

func decodeLsNode(v []byte) (LsNodeNLRI, error) {
	proto, id, rest, err := lsNLRIHeader(v)
	if err != nil {
		return LsNodeNLRI{}, err
	}
	n := LsNodeNLRI{Protocol: proto, Identifier: id, Unknown: map[uint16][]byte{}}
	err = walkTLVs(rest, func(typ uint16, val []byte) error {
		switch typ {
		case lsTLVLocalNodeDesc:
			d, err := decodeNodeDescriptor(val, n.Unknown)
			if err != nil {
				return err
			}
			n.Local = d
		default:
			// RFC 9552 §5.2.1 defines only TLV 256 at the Node NLRI's top
			// level, so there is no known example of this firing, but an
			// unrecognized TLV here must be preserved (and flagged -- see
			// DecodeLsNLRI's caller in bgp/update.go) the same way
			// DecodeLsAttrs already preserves an unrecognized attribute
			// TLV, not silently dropped.
			n.Unknown[typ] = append([]byte(nil), val...)
		}
		return nil
	})
	if err != nil {
		return LsNodeNLRI{}, err
	}
	// A later phase joins a link's far end to a node on cityHash64(protocol,
	// identifier, asn, bgpls_id, area, router_id): a node with no IGP
	// router-id has no identity that key can distinguish, so it must fail
	// here rather than decode as a plausible-looking zero. Checking
	// n.Local.RouterID after the walk (rather than inside
	// decodeNodeDescriptor) is deliberate: it also catches a Local Node
	// Descriptor TLV (256) that never appeared at all, which leaves n.Local
	// zero-valued without decodeNodeDescriptor ever running.
	if len(n.Local.RouterID) == 0 {
		return LsNodeNLRI{}, fmt.Errorf("ls node nlri: local node descriptor has no IGP router-id")
	}
	return n, nil
}

func decodeLsLink(v []byte) (LsLinkNLRI, error) {
	proto, id, rest, err := lsNLRIHeader(v)
	if err != nil {
		return LsLinkNLRI{}, err
	}
	l := LsLinkNLRI{Protocol: proto, Identifier: id, Unknown: map[uint16][]byte{}}
	err = walkTLVs(rest, func(typ uint16, val []byte) error {
		switch typ {
		case lsTLVLocalNodeDesc:
			d, err := decodeNodeDescriptor(val, l.Unknown)
			if err != nil {
				return err
			}
			l.Local = d
		case lsTLVRemoteNodeDesc:
			d, err := decodeNodeDescriptor(val, l.Unknown)
			if err != nil {
				return err
			}
			l.Remote = d
		case lsTLVIPv4IfAddr:
			// RFC 9552 §5.2.2 defines this TLV as a 4-byte IPv4 address,
			// full stop. This used to accept any width netip.AddrFromSlice
			// could parse (4 or 16 bytes) and silently drop anything else
			// -- including a 16-byte value, which AddrFromSlice happily
			// turns into a *valid*, non-IPv4 netip.Addr that satisfies
			// every downstream IsValid() gate and panics the first time
			// something calls Addr.As4() on it ("As4 called on IPv6
			// address"). A silently-dropped wrong width is also
			// indistinguishable from TLV 259 never having appeared at all
			// -- an unnumbered link -- which is exactly the
			// plausible-looking-zero failure mode this guard exists to
			// close, matching decodeNodeDescriptor's precedent for
			// KNOWN TLVs of the wrong width. Erroring here is a STRONGER
			// guard against the As4() panic than the old silent-drop was:
			// the malformed value now never survives into an LsLinkNLRI at
			// all, so collector.lsLinkMessage's own Is4() check (kept as
			// defense-in-depth) can no longer be reached with it.
			if len(val) != 4 {
				return fmt.Errorf("ls link descriptor: IPv4 interface address TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			l.LocalIfAddr = netip.AddrFrom4([4]byte(val))
		case lsTLVIPv4NeighAddr:
			if len(val) != 4 {
				return fmt.Errorf("ls link descriptor: IPv4 neighbor address TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			l.RemoteIfAddr = netip.AddrFrom4([4]byte(val))
		case lsTLVLinkLocalRemID:
			// RFC 9552 §5.2.2: Local Identifier(4) Remote Identifier(4), so
			// always 8 bytes. 0/0 on a wrong width used to be silently
			// indistinguishable from "no 258 TLV at all" -- and both
			// link_local_id and link_remote_id are part of
			// ls_links' identity tail (its ORDER BY), so that collision can
			// make two distinct parallel adjacencies collapse onto one sort
			// tuple and lose one at merge time. See LsAttrs.HasLinkID for
			// the other container this same TLV number was observed in.
			if len(val) != 8 {
				return fmt.Errorf("ls link descriptor: link local/remote identifiers TLV (%d) is %d bytes, want 8", typ, len(val))
			}
			l.LinkLocalID = binary.BigEndian.Uint32(val)
			l.LinkRemoteID = binary.BigEndian.Uint32(val[4:])
			l.HasLinkID = true
		default:
			// 261 (IPv6 Interface Address), 262 (IPv6 Neighbor Address)
			// and 263 (Multi-Topology ID) are real TLVs this build does
			// not decode -- no IPv6-IGP capture exists yet to test a
			// decoder against -- plus any genuinely unknown type. Both
			// used to vanish with no record; a link on an IPv6-only IGP
			// decoded with NO interface identity at all and nothing
			// indicating why. Preserved raw and merged with the node
			// descriptors' unknown sub-TLVs (see LsLinkNLRI.Unknown).
			l.Unknown[typ] = append([]byte(nil), val...)
		}
		return nil
	})
	if err != nil {
		return LsLinkNLRI{}, err
	}
	// See decodeLsNode: a missing IGP router-id on either end -- whether the
	// Local/Remote Node Descriptor TLV (256/257) never appeared or its
	// router-id sub-TLV did not -- leaves that end with no identity a join
	// key can use. Reporting success here would grow a wrong edge in a
	// topology graph instead of failing visibly, so it is treated the same
	// as a truncated NLRI.
	if len(l.Local.RouterID) == 0 {
		return LsLinkNLRI{}, fmt.Errorf("ls link nlri: local node descriptor has no IGP router-id")
	}
	if len(l.Remote.RouterID) == 0 {
		return LsLinkNLRI{}, fmt.Errorf("ls link nlri: remote node descriptor has no IGP router-id")
	}
	return l, nil
}

// decodeNodeDescriptor decodes a Local/Remote Node Descriptor's sub-TLVs.
// A KNOWN sub-TLV (512/513/514/515) at the wrong width is an error, not a
// no-op: silently leaving ASN/BGPLSID/Area at their zero value on a bad
// length would make a malformed sub-TLV indistinguishable from a legitimate
// AS-0/area-0 record, and a later phase's node join key
// (cityHash64(protocol, identifier, asn, bgpls_id, area, router_id)) would
// collapse distinct nodes onto that same wrong key. An UNKNOWN sub-TLV type
// is forward-compatible -- RFC 9552 leaves room for future descriptor
// sub-TLVs -- and "forward-compatible" here means "preserved raw", not
// "silently dropped": unknown collects it into the
// caller's own NLRI-level unknown map (decodeLsNode/decodeLsLink's
// n.Unknown/l.Unknown), keyed by the same uint16 TLV-type space those use
// for their own top-level unknowns. Sub-TLV numbers (512+) and Node/Link
// NLRI top-level TLV numbers (256-263) do not currently overlap, so merging
// the two spaces loses only "which container did this come from", not any
// data -- see LsNodeNLRI.Unknown's doc comment. unknown may be nil (no
// caller currently passes nil, but nothing here requires one).
func decodeNodeDescriptor(v []byte, unknown map[uint16][]byte) (LsNodeDescriptor, error) {
	var d LsNodeDescriptor
	err := walkTLVs(v, func(typ uint16, val []byte) error {
		switch typ {
		case lsTLVAutonomousSystem:
			if len(val) != 4 {
				return fmt.Errorf("ls node descriptor: AS sub-TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			d.ASN = binary.BigEndian.Uint32(val)
		case lsTLVBGPLSID:
			if len(val) != 4 {
				return fmt.Errorf("ls node descriptor: BGP-LS ID sub-TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			d.BGPLSID = binary.BigEndian.Uint32(val)
		case lsTLVOSPFArea:
			if len(val) != 4 {
				return fmt.Errorf("ls node descriptor: area sub-TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			d.Area = binary.BigEndian.Uint32(val)
		case lsTLVIGPRouterID:
			// Variable width by IGP: 4 for OSPF, 6/7 for IS-IS, 8 for an
			// OSPF pseudonode -- there is no single expected width to
			// enforce here the way there is above. Emptiness (and a wholly
			// absent 256/257 TLV) is caught by decodeLsNode/decodeLsLink
			// instead, since both leave RouterID unset.
			d.RouterID = append([]byte(nil), val...)
		default:
			if unknown != nil {
				unknown[typ] = append([]byte(nil), val...)
			}
		}
		return nil
	})
	return d, err
}

// walkTLVs iterates Type(2) Length(2) Value triplets, calling fn for each.
func walkTLVs(b []byte, fn func(typ uint16, val []byte) error) error {
	for len(b) > 0 {
		if len(b) < 4 {
			return fmt.Errorf("ls tlv: %d trailing bytes, need at least a 4-byte header", len(b))
		}
		typ := binary.BigEndian.Uint16(b)
		l := int(binary.BigEndian.Uint16(b[2:]))
		if len(b) < 4+l {
			return fmt.Errorf("ls tlv: type %d declares %d bytes, %d remain", typ, l, len(b)-4)
		}
		if err := fn(typ, b[4:4+l]); err != nil {
			return err
		}
		b = b[4+l:]
	}
	return nil
}

// BGP-LS Attribute TLVs: RFC 9552 §5.3 for the base set, RFC 9085 §2 for the
// segment-routing ones. Only the TLVs the committed corpus actually contains
// are decoded; everything else is preserved verbatim in Unknown so a vendor
// difference shows up on the parse-anomaly dashboard instead of vanishing.
const (
	lsAttrNodeName        = 1026
	lsAttrLocalRouterIDv4 = 1028
	lsAttrRemoteRouterID4 = 1030
	lsAttrSRCapabilities  = 1034 // RFC 9085 §2.1.2
	lsAttrSRAlgorithm     = 1035 // RFC 9085 §2.1.3
	lsAttrSRLocalBlock    = 1036 // RFC 9085 §2.1.4
	lsAttrAdminGroup      = 1088
	lsAttrMaxBandwidth    = 1089
	lsAttrTEDefaultMetric = 1092
	lsAttrIGPMetric       = 1095
	lsAttrPrefixMetric    = 1155 // RFC 9085 §2.3.2
	lsAttrPrefixSID       = 1158 // RFC 9085 §2.3.1
	lsAttrPrefixAttrFlags = 1170 // RFC 9085 §2.3.5
	lsAttrAdjacencySID    = 1099 // RFC 9085 §2.2.1
	lsAttrSIDLabel        = 1161 // RFC 9085 §2.1.1, nested inside 1034/1036
	// lsAttrLinkLocalRemID is TLV 258 (Link Local/Remote Identifiers) again --
	// the same TLV number as lsTLVLinkLocalRemID above, in a different
	// container. RFC 9552 §5.2.2 places it among the Link NLRI's own
	// descriptor TLVs (lsTLVLinkLocalRemID, decodeLsLink); XRd 26.1.1 was
	// observed instead sending it inside the BGP-LS Attribute (path
	// attribute 29) -- see the captured linkAttrHex fixture in
	// linkstate_test.go (bytes 0102 0008 0000000400000003, local=4 remote=3)
	// -- with no TLV 258 anywhere in that same UPDATE's Link NLRI.
	// DecodeLsAttrs previously had no case for 258 at all, so XRd's
	// placement fell into Unknown and ls_links.link_local_id/link_remote_id
	// were always 0 -- silently colliding two parallel adjacencies onto one
	// ls_links sort tuple, since both columns are in that table's identity
	// tail. See LsAttrs.HasLinkID and collector.lsLinkMessage for how the two
	// containers are reconciled.
	lsAttrLinkLocalRemID = 258
)

// LsAdjacencySID is one Adjacency SID (1099) TLV. RFC 9085 §2.2.1 allows
// several on a single link -- a protected/unprotected pair is the ordinary
// case for SR-MPLS, not a malformed message -- so DecodeLsAttrs keeps one
// entry per TLV encountered rather than the last one overwriting the rest.
type LsAdjacencySID struct {
	SID    uint32
	Flags  uint8
	Weight uint8
}

type LsAttrs struct {
	NodeName         string
	LocalRouterIDv4  netip.Addr
	RemoteRouterIDv4 netip.Addr

	SRGBBase, SRGBSize uint32
	SRLBBase, SRLBSize uint32
	SRAlgorithms       []uint8

	AdjSIDs []LsAdjacencySID

	TEMetric     uint32
	IGPMetric    uint32
	AdminGroup   uint32
	MaxBandwidth float32

	// LinkLocalID/LinkRemoteID/HasLinkID: see lsAttrLinkLocalRemID's doc
	// comment -- this is TLV 258 as XRd 26.1.1 was observed sending it, in
	// the BGP-LS Attribute rather than the Link NLRI. HasLinkID distinguishes
	// "0/0 because the TLV was absent from the attribute" from "0/0 because
	// the TLV legitimately carried zeros", the same way LsLinkNLRI.HasLinkID
	// does for the NLRI-side placement.
	LinkLocalID, LinkRemoteID uint32
	HasLinkID                 bool

	// Prefix attributes (RFC 9085 §2.3), carried alongside a Prefix NLRI.
	//
	// PrefixSID is the reason prefix NLRI is decoded at all: node SIDs ride
	// here and nowhere else, so without it a topology has adjacency SIDs and
	// no node SIDs. HasPrefixSID distinguishes an absent TLV from a
	// legitimately-zero SID index, the same problem HasLinkID solves above.
	//
	// PrefixSIDFlags is kept rather than dropped because it changes what the
	// SID MEANS: with the V and L flags set the 4-byte field is an absolute
	// label rather than an index into the SRGB, so a consumer that ignores
	// the flags can read a label as an index and be wrong by the SRGB base.
	// Every SID observed in the lab so far has flags 0 (index).
	PrefixMetric    uint32
	PrefixSID       uint32
	PrefixSIDFlags  uint8
	HasPrefixSID    bool
	PrefixAttrFlags uint8

	Unknown map[uint16][]byte
}

func DecodeLsAttrs(b []byte) (LsAttrs, error) {
	a := LsAttrs{Unknown: map[uint16][]byte{}}
	err := walkTLVs(b, func(typ uint16, val []byte) error {
		switch typ {
		case lsAttrNodeName:
			// The wire does not promise UTF-8, and this lands in a proto3
			// string field: proto.Marshal rejects the whole LsEvent over one
			// invalid byte, so every node and link in the UPDATE would be
			// lost at publish time. Replaced the way bmp.ParseInit replaces
			// a banner's invalid bytes.
			a.NodeName = strings.ToValidUTF8(string(val), "\uFFFD")
		case lsAttrLocalRouterIDv4:
			if x, ok := netip.AddrFromSlice(val); ok && x.Is4() {
				a.LocalRouterIDv4 = x
			}
		case lsAttrRemoteRouterID4:
			if x, ok := netip.AddrFromSlice(val); ok && x.Is4() {
				a.RemoteRouterIDv4 = x
			}
		case lsAttrSRCapabilities:
			base, size, err := decodeSRRange(typ, val)
			if err != nil {
				return err
			}
			a.SRGBBase, a.SRGBSize = base, size
		case lsAttrSRLocalBlock:
			base, size, err := decodeSRRange(typ, val)
			if err != nil {
				return err
			}
			a.SRLBBase, a.SRLBSize = base, size
		case lsAttrSRAlgorithm:
			a.SRAlgorithms = append([]uint8(nil), val...)
		case lsAttrAdminGroup:
			// Fixed 4 bytes (RFC 9552 §5.3.1). A wrong width left unchecked
			// would report AdminGroup=0, indistinguishable from a real
			// admin-group-0 link -- same defect as the node descriptor's
			// KNOWN sub-TLVs (decodeNodeDescriptor), same fix.
			if len(val) != 4 {
				return fmt.Errorf("ls attr: admin group TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			a.AdminGroup = binary.BigEndian.Uint32(val)
		case lsAttrMaxBandwidth:
			// Fixed 4 bytes (IEEE-754 float32). A wrong width left
			// unchecked would report MaxBandwidth=0.0, which a consumer
			// reads as "unusable link" rather than "could not parse".
			if len(val) != 4 {
				return fmt.Errorf("ls attr: max bandwidth TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			a.MaxBandwidth = math.Float32frombits(binary.BigEndian.Uint32(val))
		case lsAttrTEDefaultMetric:
			// Fixed 4 bytes. TEMetric=0 is a plausible real metric, so a
			// silently-zeroed wrong width would be indistinguishable from
			// a legitimate value rather than a parse failure.
			if len(val) != 4 {
				return fmt.Errorf("ls attr: TE default metric TLV (%d) is %d bytes, want 4", typ, len(val))
			}
			a.TEMetric = binary.BigEndian.Uint32(val)
		case lsAttrIGPMetric:
			// Width varies by IGP: 1 byte (IS-IS small metric), 2 (OSPF
			// metric), 3 (IS-IS wide metric) -- RFC 9552 §5.3.1 leaves the
			// encoding IGP-specific. Those three widths are the only valid
			// ones; anything else is a malformed KNOWN TLV, not a metric.
			if len(val) < 1 || len(val) > 3 {
				return fmt.Errorf("ls attr: IGP metric TLV (%d) is %d bytes, want 1, 2 or 3", typ, len(val))
			}
			a.IGPMetric = beUintN(val)
		case lsAttrPrefixMetric:
			if len(val) != 4 {
				return fmt.Errorf("ls attr: prefix metric tlv is %d bytes, want 4", len(val))
			}
			a.PrefixMetric = binary.BigEndian.Uint32(val)
		case lsAttrPrefixSID:
			// RFC 9085 §2.3.1: Flags(1) Algorithm(1) Reserved(2) SID(4 or 3).
			// Only the 4-byte (index) form has been observed; the 3-byte form
			// is a label and is rejected rather than guessed at, so it shows
			// up as a decode failure instead of a silently wrong SID.
			if len(val) != 8 {
				return fmt.Errorf("ls attr: prefix-sid tlv is %d bytes, want 8 (flags, algorithm, reserved, 4-byte SID)", len(val))
			}
			a.PrefixSIDFlags = val[0]
			a.PrefixSID = binary.BigEndian.Uint32(val[4:])
			a.HasPrefixSID = true
		case lsAttrPrefixAttrFlags:
			if len(val) != 1 {
				return fmt.Errorf("ls attr: prefix attribute flags tlv is %d bytes, want 1", len(val))
			}
			a.PrefixAttrFlags = val[0]
		case lsAttrAdjacencySID:
			// Flags(1) Weight(1) Reserved(2) SID(3 label or 4 index), so
			// the only valid total widths are 7 and 8. AdjSID=0 collides
			// with the MPLS implicit-null label, so a wrong width silently
			// producing 0 would read as a real (if unusual) SID rather
			// than a parse failure.
			if len(val) != 7 && len(val) != 8 {
				return fmt.Errorf("ls attr: adjacency SID TLV (%d) is %d bytes, want 7 (label) or 8 (index)", typ, len(val))
			}
			a.AdjSIDs = append(a.AdjSIDs, LsAdjacencySID{
				Flags:  val[0],
				Weight: val[1],
				SID:    beUintN(val[4:]),
			})
		case lsAttrLinkLocalRemID:
			// See lsAttrLinkLocalRemID's doc comment: XRd 26.1.1's placement
			// of TLV 258. Same wire shape and same strictness argument as
			// lsTLVLinkLocalRemID in decodeLsLink -- a wrong width silently
			// producing 0/0 would be indistinguishable from "TLV absent",
			// and both fields are part of ls_links' sort-key identity tail.
			if len(val) != 8 {
				return fmt.Errorf("ls attr: link local/remote identifiers TLV (%d) is %d bytes, want 8", typ, len(val))
			}
			a.LinkLocalID = binary.BigEndian.Uint32(val)
			a.LinkRemoteID = binary.BigEndian.Uint32(val[4:])
			a.HasLinkID = true
		default:
			a.Unknown[typ] = append([]byte(nil), val...)
		}
		return nil
	})
	return a, err
}

// LsHasUnknownTLVs reports whether any decoded node or link carries an NLRI
// TLV this build did not recognize. A Node/Link NLRI decodes as a
// structural success even when walkTLVs silently routed one of its TLVs
// to Unknown -- there is no error to check -- so this is the
// signal bgp/update.go uses to raise PARSE_FLAG_LS_TLV_UNKNOWN for the NLRI
// side, mirroring the len(LsAttrs.Unknown) > 0 check it uses for the
// attribute side.
func LsHasUnknownTLVs(nodes []LsNodeNLRI, links []LsLinkNLRI, prefixes []LsPrefixNLRI) bool {
	for _, n := range nodes {
		if len(n.Unknown) > 0 {
			return true
		}
	}
	for _, l := range links {
		if len(l.Unknown) > 0 {
			return true
		}
	}
	for _, p := range prefixes {
		if len(p.Unknown) > 0 {
			return true
		}
	}
	return false
}

// decodeSRRange decodes an SR Capabilities (1034) or SR Local Block (1036)
// value: Flags(1) Reserved(1) then one or more {Range Size(3), SID/Label
// sub-TLV} entries (RFC 9085 §2.1.2/§2.1.4). typ is the caller's TLV type
// (1034 or 1036), used only to name the field in an error.
//
// Two failure modes here were changed from "decode as a plausible-looking
// value, err==nil" to "report the error":
//
//   - Multiple ranges: this function's contract is one (base, size) pair,
//     but the wire format allows several entries (e.g. a router whose SRGB
//     was carved from two label blocks). The previous version silently kept
//     only the first and discarded the rest -- a real, understated SRGB
//     reported with no error. There is no list-returning replacement here
//     (every caller in DecodeLsAttrs wants exactly one SRGB/SRLB), so a
//     genuine second range is now a hard error rather than a silent
//     truncation: DecodeLsAttrs propagates it, and the caller above that
//     (bgp/update.go's RFC 7606 ladder) already has a lossless fallback for
//     any malformed BGP-LS attribute TLV -- attribute-discard, verbatim
//     bytes kept, PARSE_FLAG_ATTR_DISCARDED_7606 raised.
//   - An unrecognized nested sub-TLV: the previous version returned
//     base=0, err=nil -- an SRGB reported as starting at label 0, which is a
//     plausible-looking fabricated value (0 is also byte-for-byte what "add
//     the implicit-null label" would look like). Unlike a top-level or
//     descriptor unknown TLV (which this package treats as genuinely
//     forward-compatible, e.g. decodeNodeDescriptor/DecodeLsAttrs's default
//     case), this nested sub-TLV is the ONLY place a base value can come
//     from, so skipping it silently is skipping the whole range's meaning.
//     It is now an error instead of a fabricated base.
//
// Every length check below guards a KNOWN structure -- the outer range
// header, the nested sub-TLV header, and the SID/Label sub-TLV's value --
// so a malformed one is reported rather than silently decoding as SRGB/SRLB
// 0+0, which looks like "no SR capability" instead of "could not parse".
func decodeSRRange(typ uint16, v []byte) (base, size uint32, err error) {
	if len(v) < 5 {
		return 0, 0, fmt.Errorf("ls attr: SR range TLV (%d) is %d bytes, want at least 5 for flags+reserved+range-size", typ, len(v))
	}
	size = beUintN(v[2:5])
	sub := v[5:]
	if len(sub) < 4 {
		return 0, 0, fmt.Errorf("ls attr: SR range TLV (%d) has %d trailing bytes after the range size, want a 4-byte SID/Label sub-TLV header", typ, len(sub))
	}
	subTyp := binary.BigEndian.Uint16(sub)
	l := int(binary.BigEndian.Uint16(sub[2:]))
	if len(sub) < 4+l {
		return 0, 0, fmt.Errorf("ls attr: SR range TLV (%d) nested sub-TLV (%d) declares %d bytes, %d remain", typ, subTyp, l, len(sub)-4)
	}
	if subTyp != lsAttrSIDLabel {
		return 0, size, fmt.Errorf("ls attr: SR range TLV (%d) nested sub-TLV (%d) is not the SID/Label sub-TLV (%d) this decoder recognizes -- no base to report", typ, subTyp, lsAttrSIDLabel)
	}
	// Valid widths for the SID/Label sub-TLV (RFC 9085 §2.1.1): 3 bytes for
	// a label, 4 for an index. A different width is a malformed KNOWN
	// sub-TLV, not a base value to zero out.
	if l != 3 && l != 4 {
		return 0, size, fmt.Errorf("ls attr: SID/Label sub-TLV (%d) is %d bytes, want 3 (label) or 4 (index)", lsAttrSIDLabel, l)
	}
	base = beUintN(sub[4 : 4+l])
	if rest := sub[4+l:]; len(rest) > 0 {
		return base, size, fmt.Errorf("ls attr: SR range TLV (%d) carries %d trailing bytes after its first range -- multiple ranges are not decoded (see decodeSRRange)", typ, len(rest))
	}
	return base, size, nil
}

// beUintN reads up to 4 big-endian bytes as a uint32. BGP-LS encodes labels
// in 3 bytes and indexes in 4, and IGP metrics in 1, 2 or 3, so the width is
// data rather than a constant.
func beUintN(b []byte) uint32 {
	if len(b) > 4 {
		b = b[:4]
	}
	var v uint32
	for _, c := range b {
		v = v<<8 | uint32(c)
	}
	return v
}

// AppendLsNodeNLRI encodes Node NLRI in the wire format DecodeLsNLRI reads
// (RFC 9552 §5.2): per entry a 2-byte NLRI type and 2-byte length, then the
// NLRI body -- a 1-byte protocol ID, an 8-byte identifier, and a Local Node
// Descriptor TLV whose value is itself a set of sub-TLVs.
//
// Three nesting levels each carry their own length, which is what makes this
// easy to get subtly wrong: a length computed at the wrong level still walks
// cleanly and describes a different node. The lengths here are derived from
// the encoded bytes rather than predicted, so they cannot disagree with the
// content.
//
// Only Node NLRI is emitted. Link (type 2) and Prefix (type 3) NLRI carry
// descriptor sets of their own and are not built yet; a generator that emitted
// a half-formed one would show up as decoder breakage rather than as a missing
// feature.
func AppendLsNodeNLRI(dst []byte, ns []LsNodeNLRI) []byte {
	for _, n := range ns {
		body := []byte{n.Protocol}
		body = binary.BigEndian.AppendUint64(body, n.Identifier)
		body = appendLsTLV(body, lsTLVLocalNodeDesc, appendLsNodeDescriptor(nil, n.Local))
		dst = appendLsTLV(dst, lsNLRINode, body)
	}
	return dst
}

// appendLsTLV writes one type/length/value triple in the 2+2+n form every
// level of BGP-LS uses -- NLRI headers, descriptor TLVs and their sub-TLVs
// alike, which is why this is shared rather than written out three times.
func appendLsTLV(dst []byte, typ uint16, val []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, typ)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(val)))
	return append(dst, val...)
}

// appendLsNodeDescriptor encodes the sub-TLVs decodeNodeDescriptor reads.
//
// A zero ASN, BGP-LS ID or Area is emitted rather than omitted: RFC 9552 makes
// these descriptors identity, and a router that reports area 0 is saying
// something different from one that reports no area at all. The decoder cannot
// tell an absent TLV from a zero-valued one after the fact, so the builder
// does not create that ambiguity by dropping zeros.
//
// RouterID is variable width by IGP -- 4 bytes for OSPF, 6 or 7 for IS-IS, 8
// for an OSPF pseudonode -- so it is written at whatever width the caller
// supplied. An empty RouterID is omitted entirely, which is the one shape
// decodeLsNode treats as an error, so callers get a decode failure rather than
// a silently identity-less node.
func appendLsNodeDescriptor(dst []byte, d LsNodeDescriptor) []byte {
	dst = appendLsTLV(dst, lsTLVAutonomousSystem, binary.BigEndian.AppendUint32(nil, d.ASN))
	dst = appendLsTLV(dst, lsTLVBGPLSID, binary.BigEndian.AppendUint32(nil, d.BGPLSID))
	dst = appendLsTLV(dst, lsTLVOSPFArea, binary.BigEndian.AppendUint32(nil, d.Area))
	if len(d.RouterID) > 0 {
		dst = appendLsTLV(dst, lsTLVIGPRouterID, d.RouterID)
	}
	return dst
}
