package bgp

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// Extended community type octets (RFC 4360 §3). The high-order octet
// chooses the administrator field's layout; the subtype says what the
// community *means* but never how it is laid out.
//
// Masking the transitive bit (0x40) maps the non-transitive types onto
// these same values -- 0x40 onto 0x00, 0x41 onto 0x01, 0x42 onto 0x02 --
// so one switch covers both halves of the registry with no extra cases,
// and the bit stays visible in the stored Type for a consumer that needs
// to tell them apart.
const (
	extTypeAS2    = 0x00 // 2-byte AS : 4-byte assigned
	extTypeIPv4   = 0x01 // 4-byte IPv4 : 2-byte assigned
	extTypeAS4    = 0x02 // 4-byte AS : 2-byte assigned
	extTypeOpaque = 0x03 // 6 opaque bytes
)

// decodeExtComm decodes one extended community from exactly 8 bytes.
//
// It cannot fail, and that is a property of the caller as much as of this
// function: parseAttr rejects an Extended Communities attribute whose length
// is not a non-zero multiple of 8 (RFC 7606 §7.14) before any of this runs,
// so the length is fixed and every type octet has a rendering -- opaque hex
// being the fallback for one this package does not model. No error path
// means no new ParseFlag and no change to treat-as-withdraw behavior.
func decodeExtComm(b []byte) *vantagev1.ExtCommunity {
	val, ok := extCommSpecial(b)
	if !ok {
		val = extCommValue(b)
	}
	return &vantagev1.ExtCommunity{
		Type:    uint32(b[0]),
		SubType: uint32(b[1]),
		Value:   val,
		// Copied, not aliased: b points into the caller's attribute buffer,
		// which ParseUpdate does not own past the call.
		Raw: append([]byte{}, b...),
	}
}

// extCommValue renders the six bytes after the type and subtype octets.
//
// The experimental types (0x80-0x82, e.g. flow-spec traffic-rate) mask to
// the 2-byte-AS shape, which is structurally what they carry -- a 2-byte AS
// followed by four bytes whose meaning is subtype-specific. Rendering them
// as an AS pair is therefore right about the layout and says nothing about
// the meaning, which is the correct division of labor here.
func extCommValue(b []byte) string {
	switch b[0] & 0x3f {
	case extTypeAS2:
		return fmt.Sprintf("%d:%d", binary.BigEndian.Uint16(b[2:4]), binary.BigEndian.Uint32(b[4:8]))
	case extTypeIPv4:
		return fmt.Sprintf("%s:%d", netip.AddrFrom4([4]byte(b[2:6])), binary.BigEndian.Uint16(b[6:8]))
	case extTypeAS4:
		return fmt.Sprintf("%d:%d", binary.BigEndian.Uint32(b[2:6]), binary.BigEndian.Uint16(b[6:8]))
	default:
		return fmt.Sprintf("0x%x", b[2:8])
	}
}

// The remaining types this package names, and the subtypes. Spelled as
// (type, subtype) pairs rather than subtype alone because the registry
// assigns subtypes per type: a route target exists for the three transitive
// types and not for their non-transitive counterparts, and link bandwidth
// exists only as non-transitive.
const (
	extTypeEVPN     = 0x06 // RFC 7432
	extTypeAS2NonTr = 0x40 // non-transitive 2-byte AS specific
	extTypeExperim  = 0x80 // generic transitive experimental (RFC 8955 flow spec)

	extSubRT  = 0x02 // route target, on the three AS/IPv4 types
	extSubSoO = 0x03 // route origin / site of origin
	// OSPF Domain Identifier, RFC 4577 4.2.1. Assigned on all three
	// AS/IPv4 types, like route target and route origin.
	extSubOSPFDomainID = 0x05

	// OSPF Route Type and Router ID as IOS-XR 26.1.1 actually emits them:
	// the pre-standard Cisco codepoints under the generic transitive
	// experimental type, NOT RFC 4577's 0x0306 and 0x0107. Verified against
	// iosxr/xrd-26.1.1-p2-ospf-extcomm.bmpcap, where the router's own
	// rendering of the same route agrees. The standard codepoints are left
	// unnamed on purpose -- no capture here carries them, and this file
	// separates what traffic verified from what was transcribed.
	experimSubOSPFRouteType = 0x00
	experimSubOSPFRouterID  = 0x01

	opaqueSubColor = 0x0b // RFC 9012
	opaqueSubEncap = 0x0c // RFC 9012 tunnel encapsulation

	evpnSubMACMobility = 0x00 // RFC 7432 §7.7
	evpnSubESILabel    = 0x01 // RFC 7432 §7.5
	evpnSubESImportRT  = 0x02 // RFC 7432 §7.6
	evpnSubRouterMAC   = 0x03 // RFC 9135
	evpnSubDefaultGW   = 0x0d // RFC 7432 §5

	as2NonTrSubLinkBW = 0x04 // draft-ietf-idr-link-bandwidth

	fsSubTrafficRate    = 0x06 // RFC 8955 §7
	fsSubTrafficAction  = 0x07
	fsSubRedirect       = 0x08
	fsSubTrafficMarking = 0x09
)

// ExtCommName returns the well-known name for an extended community's
// (type, subtype) pair, or "" for a pair this package does not model.
//
// The name is not stored on the wire message because (type, sub_type)
// already determines it; this is the single definition, exported so the
// ClickHouse sink's rendering and operator tooling share it rather than
// each carrying a table that can drift.
//
// Only the four route-target/EVPN/encapsulation groups and the flow-spec
// four are named. Of those, exactly four have ever been seen in this
// repo's captures -- rt, encap, mac-mobility and router-mac. The rest are
// transcribed from the IANA registry and are unverified by traffic; see
// docs/openbmp-parity.md, which tracks that distinction deliberately.
func ExtCommName(typ, sub uint8) string {
	switch typ {
	case extTypeAS2, extTypeIPv4, extTypeAS4:
		switch sub {
		case extSubRT:
			return "rt"
		case extSubSoO:
			return "soo"
		case extSubOSPFDomainID:
			return "ospf-domain-id"
		}
	case extTypeOpaque:
		switch sub {
		case opaqueSubColor:
			return "color"
		case opaqueSubEncap:
			return "encap"
		}
	case extTypeEVPN:
		switch sub {
		case evpnSubMACMobility:
			return "mac-mobility"
		case evpnSubESILabel:
			return "esi-label"
		case evpnSubESImportRT:
			return "es-import-rt"
		case evpnSubRouterMAC:
			return "router-mac"
		case evpnSubDefaultGW:
			return "default-gw"
		}
	case extTypeAS2NonTr:
		if sub == as2NonTrSubLinkBW {
			return "link-bw"
		}
	case extTypeExperim:
		switch sub {
		// These two share the experimental type with flow spec but not its
		// subtype range, so they coexist without shadowing anything.
		case experimSubOSPFRouteType:
			return "ospf-route-type"
		case experimSubOSPFRouterID:
			return "ospf-router-id"
		case fsSubTrafficRate:
			return "flowspec-traffic-rate"
		case fsSubTrafficAction:
			return "flowspec-traffic-action"
		case fsSubRedirect:
			return "flowspec-redirect"
		case fsSubTrafficMarking:
			return "flowspec-traffic-marking"
		}
	}
	return ""
}

// tunnelTypeVXLAN is the one RFC 9012 tunnel type this repo has seen on a
// wire (every NX-OS EVPN route in the corpus carries it). Others render as
// their number rather than a name transcribed from a registry and never
// checked against a router.
const tunnelTypeVXLAN = 8

// extCommSpecial renders the subtypes whose layout is known *and* whose
// meaning is the point of the community, returning false for everything
// else so extCommValue's structural rendering applies.
//
// These three and no others, because the corpus proves these three: NX-OS
// EVPN routes carry a router MAC and a sticky MAC-mobility community on
// their type-2 routes and a VXLAN encapsulation community throughout.
// Rendering a router MAC as "0x5231d4031b08" is not wrong so much as
// useless, and rendering, say, a link-bandwidth community that no capture
// contains would be a guess dressed as a decode.
func extCommSpecial(b []byte) (string, bool) {
	switch b[0] {
	case extTypeEVPN:
		switch b[1] {
		case evpnSubRouterMAC:
			// Six bytes of MAC, rendered by the same function that renders
			// an EVPN type-2 route's MAC (decodeEvpnType2). That shared
			// renderer is the point, not a convenience: a router-mac
			// community and route_evpn.mac are meant to join on equality in
			// the sink, so two independent spellings of a MAC address would
			// be a join that silently returns nothing. net.HardwareAddr's
			// String is lowercase colon-separated, which is also how every
			// vendor CLI prints one.
			return net.HardwareAddr(b[2:8]).String(), true
		case evpnSubMACMobility:
			// RFC 7432 §7.7: flags(1) reserved(1) sequence(4). Bit 0 of
			// flags is Sticky/static -- a MAC that must not be considered
			// to have moved, which is why it is worth surfacing by name
			// rather than as a bit in a hex blob.
			seq := binary.BigEndian.Uint32(b[4:8])
			if b[2]&0x01 != 0 {
				return fmt.Sprintf("sticky,seq=%d", seq), true
			}
			return fmt.Sprintf("seq=%d", seq), true
		}
	case extTypeOpaque:
		if b[1] == opaqueSubEncap {
			// RFC 9012 §6: four reserved bytes then a 2-byte tunnel type.
			tt := binary.BigEndian.Uint16(b[6:8])
			if tt == tunnelTypeVXLAN {
				return "vxlan", true
			}
			return fmt.Sprintf("%d", tt), true
		}
	}
	return "", false
}
