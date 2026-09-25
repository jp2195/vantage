// Package bgp implements BGP wire parsing (RFC 4271, 4760, 6793, 7911, 7606
// subset needed by BMP route monitoring). Pure functions, stdlib-only; they
// never panic on wire input.
package bgp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
)

// Family identifies a BGP address family by AFI/SAFI (RFC 4760 §5).
type Family struct {
	AFI  uint16
	SAFI uint8
}

// FamilyIPv4U is the IPv4 unicast address family (AFI 1, SAFI 1).
var FamilyIPv4U = Family{AFI: 1, SAFI: 1}

const (
	bgpHeaderLen = 19 // RFC 4271 §4.1: 16-byte marker + 2-byte length + 1-byte type
	msgOpen      = 1  // RFC 4271 §4.1 message type: OPEN

	optParamCaps = 2  // RFC 5492 §4: Capabilities Optional Parameter type
	capMP        = 1  // RFC 4760 §8: Multiprotocol Extensions capability code
	cap4ByteAS   = 65 // RFC 6793 §3: 4-octet AS number capability code
	capAddPath   = 69 // RFC 7911 §3: ADD-PATH capability code

	addPathRecvBit = 0x1 // RFC 7911 §3 Send/Receive value 1 (Receive)
	addPathSendBit = 0x2 // RFC 7911 §3 Send/Receive value 2 (Send)

	// maxCapsLen is the largest capabilities payload AppendOpen can wrap in
	// a single Capabilities optional parameter. The parameter's own 1-byte
	// length field must hold len(caps), and the OPEN's 1-byte opt-param-len
	// field must hold len(caps)+2 (the parameter's own type+length bytes).
	// That alone would allow 253 (255-2). But RFC 9072 reserves the value
	// 255 in the opt-param-len byte as a marker for extended-length
	// encoding (see ErrExtendedOptParams), so the emitted opt-param-len
	// must never reach 255 either — capping it at 254 tightens the bound to
	// 254-2 = 252, which is the binding constraint here.
	maxCapsLen = 252

	// extendedOptParamsMarker is the RFC 9072 §2 reserved value: an
	// opt-param-len byte of 255 signals that a 2-octet Non-Ext OP Len field
	// follows and each optional parameter carries a 2-octet length instead
	// of 1-octet. It is not supported; ParseOpen detects and rejects it
	// rather than misreading the extended-length bytes as a legacy
	// parameter.
	extendedOptParamsMarker = 255
)

// Sentinel errors for malformed OPEN messages, distinguished by which
// length-prefixed structure ran out of bytes: the OPEN's own fixed
// fields/opt-param-len, an individual optional parameter, or an individual
// capability nested inside one.
var (
	ErrNotOpen           = errors.New("bgp: not an OPEN message")
	ErrOpenTruncated     = errors.New("bgp: open message truncated")
	ErrOptParamTruncated = errors.New("bgp: optional parameter truncated")
	ErrCapTruncated      = errors.New("bgp: capability truncated")

	// ErrExtendedOptParams is returned when an OPEN's opt-param-len byte is
	// the RFC 9072 reserved marker value (255), signaling the 2-octet
	// extended optional-parameters encoding. That encoding is recognized
	// but not yet supported by this package; ParseOpen fails loudly rather
	// than misinterpreting the extended-length bytes as a legacy
	// parameter, which could otherwise silently fabricate capabilities.
	ErrExtendedOptParams = errors.New("bgp: RFC 9072 extended optional parameters are recognized but not yet supported")
)

// Caps is the set of capabilities advertised in one BGP OPEN message (RFC
// 5492).
//
// AddPathRecv records, for this single OPEN, which families had the
// ADD-PATH Receive bit (RFC 7911 §3, Send/Receive value 1) set. addPathSend
// mirrors it for the Send bit (value 2). Both bits must be tracked
// separately per OPEN because Merge's negotiation rule needs one side's
// receive bit and the other side's send bit — see Merge's doc comment. The
// send-bit table is unexported because nothing outside this file needs it
// standalone; only the merged Caps.AddPathRecv (the output of Merge) is
// part of this package's public contract.
type Caps struct {
	FourByteAS  bool
	MP          map[Family]bool
	AddPathRecv map[Family]bool
	addPathSend map[Family]bool

	// HoldTime is the OPEN's own Hold Time field (RFC 4271 §4.2), in
	// seconds. Not a capability -- it is a fixed body field, which is why
	// ParseOpen could read every capability in the message while walking
	// straight past it -- but it travels with them because it is negotiated
	// from the same pair of OPENs and consumed by the same callers.
	//
	// ZERO IS A VALUE, NOT AN ABSENCE. RFC 4271 §4.2: "A value of 0 indicates
	// that the Hold Timer is never going to expire" -- keepalives disabled
	// for the session. Anything storing this has to keep that distinct from
	// "we did not observe an OPEN".
	//
	// THERE IS NO KEEPALIVE FIELD HERE BECAUSE THERE IS NONE ON THE WIRE.
	// The keepalive interval is a local timer, conventionally HoldTime/3,
	// that a speaker never announces. Deriving it here would put a
	// convention in the same struct as observations and let it inherit their
	// authority downstream.
	HoldTime uint16
}

func newCaps() Caps {
	return Caps{
		MP:          map[Family]bool{},
		AddPathRecv: map[Family]bool{},
		addPathSend: map[Family]bool{},
	}
}

// ParseOpen parses a full BGP OPEN message (RFC 4271 §4.1/§4.2), including
// its 19-byte header, extracting capabilities from its RFC 5492
// Capabilities optional parameter(s): RFC 4760 Multiprotocol Extensions,
// RFC 6793 4-octet AS numbers, and RFC 7911 ADD-PATH.
//
// Real OPEN messages routinely spread capabilities across more than one
// type-2 optional parameter (one capability per parameter) and just as
// routinely pack more than one capability into a single parameter; both
// forms are handled and accumulate into the same Caps. Capability codes
// this package has no use for (Route Refresh, Graceful Restart, and dozens
// of others real routers send) are skipped, not treated as errors. If the
// same <AFI,SAFI> is advertised more than once — malformed, but tolerated
// rather than rejected — the bits observed across all occurrences are
// unioned.
//
// msg's declared BGP header length (bytes 16-17) is not consulted. The
// message's true extent is derived entirely from the OPEN body's own
// opt-param-len and each parameter/capability's own length, so trailing
// bytes in msg beyond the parsed structure (e.g. a second message
// concatenated after it) are ignored rather than rejected.
//
// Implicit IPv4 unicast (RFC 4760 §5, "Old" BGP-4 speakers): the
// Multiprotocol Extensions capability exists to negotiate address families
// *beyond* what base BGP-4 (RFC 4271) already carries natively, which is
// IPv4 unicast. A speaker that sends no MP capability at all — still a
// legal, deployed configuration — is therefore relying on that native
// default, not declining every family. If no capability with code 1 (MP)
// appears anywhere in the message, ParseOpen sets c.MP[FamilyIPv4U] = true
// before returning. This only fires when the MP capability is entirely
// absent: a speaker that sends MP capabilities for other families but
// deliberately omits IPv4 unicast (a legitimate way to disable it) is left
// alone. The default is never applied on an error return.
//
// RFC 9072 extended optional parameters: an opt-param-len byte of 255 is a
// reserved marker (see ErrExtendedOptParams) that this package does not yet
// support; ParseOpen detects it before walking any parameters and fails
// loudly instead of misreading the extended-length encoding as a legacy
// parameter.
func ParseOpen(msg []byte) (Caps, error) {
	// RFC 4271 §4.2: version(1) + my-AS(2) + hold-time(2) + bgp-id(4) +
	// opt-param-len(1) = 10 fixed body bytes before any optional
	// parameters, so the minimum well-formed OPEN is 19+10 = 29 bytes.
	if len(msg) < bgpHeaderLen+10 {
		return newCaps(), fmt.Errorf("%w: %d bytes, need at least %d", ErrOpenTruncated, len(msg), bgpHeaderLen+10)
	}
	if msg[18] != msgOpen {
		return newCaps(), fmt.Errorf("%w: type %d", ErrNotOpen, msg[18])
	}
	body := msg[bgpHeaderLen:]
	optLen := int(body[9])
	if optLen == extendedOptParamsMarker {
		return newCaps(), ErrExtendedOptParams
	}
	if len(body) < 10+optLen {
		return newCaps(), fmt.Errorf("%w: opt-param-len %d exceeds %d bytes available", ErrOpenTruncated, optLen, len(body)-10)
	}
	c := newCaps()
	// body[3:5] -- after version(1) and my-AS(2). Read before the parameter
	// walk because it is not a parameter: a message carrying no optional
	// parameters at all still carries a hold time.
	c.HoldTime = binary.BigEndian.Uint16(body[3:5])
	mpSeen := false
	params := body[10 : 10+optLen]
	for len(params) > 0 {
		if len(params) < 2 {
			return newCaps(), fmt.Errorf("%w: parameter header", ErrOptParamTruncated)
		}
		ptype, plen := params[0], int(params[1])
		if len(params) < 2+plen {
			return newCaps(), fmt.Errorf("%w: parameter declares %d bytes, %d available", ErrOptParamTruncated, plen, len(params)-2)
		}
		val := params[2 : 2+plen]
		params = params[2+plen:]
		if ptype != optParamCaps {
			continue
		}
		for len(val) > 0 {
			if len(val) < 2 {
				return newCaps(), fmt.Errorf("%w: capability header", ErrCapTruncated)
			}
			code, clen := val[0], int(val[1])
			if len(val) < 2+clen {
				return newCaps(), fmt.Errorf("%w: capability declares %d bytes, %d available", ErrCapTruncated, clen, len(val)-2)
			}
			cv := val[2 : 2+clen]
			val = val[2+clen:]
			switch code {
			case capMP:
				// Any occurrence of this code — regardless of whether it
				// parses cleanly — means the speaker sent the capability,
				// so the RFC 4760 implicit-IPv4-unicast default below must
				// not apply.
				mpSeen = true
				// RFC 4760 §4: AFI(2) Reserved(1) SAFI(1).
				if clen == 4 {
					c.MP[Family{AFI: binary.BigEndian.Uint16(cv[0:2]), SAFI: cv[3]}] = true
				}
			case cap4ByteAS:
				// RFC 6793 §3: the 4-octet AS number itself is not needed
				// here (my-AS/AS_TRANS already tells the caller when to
				// look for it); only presence matters. Accumulate with OR
				// so a later malformed duplicate can't erase an earlier
				// valid one.
				if clen == 4 {
					c.FourByteAS = true
				}
			case capAddPath:
				// RFC 7911 §3: repeating AFI(2) SAFI(1) Send/Receive(1)
				// tuples, no reserved byte (unlike the MP capability).
				for len(cv) >= 4 {
					f := Family{AFI: binary.BigEndian.Uint16(cv[0:2]), SAFI: cv[2]}
					if cv[3]&addPathRecvBit != 0 {
						c.AddPathRecv[f] = true
					}
					if cv[3]&addPathSendBit != 0 {
						c.addPathSend[f] = true
					}
					cv = cv[4:]
				}
			}
		}
	}
	if !mpSeen {
		c.MP[FamilyIPv4U] = true
	}
	return c, nil
}

// Merge computes the negotiated capability view for one direction of travel
// from the two OPEN messages a BMP Peer Up Notification carries (RFC 7854
// §4.9).
//
// The parameters are named for the roles they play in the direction being
// asked about, not for which end of the session sent them: receiver is the
// OPEN of the side that *receives* the monitored UPDATEs, sender is the OPEN
// of the side that *sends* them. For adj-RIB-in — the RFC 7854 default,
// where route monitoring mirrors what the peer sent the router — that is
// Merge(routerOpen, peerOpen). For RFC 8671 adj-RIB-out the mirrored UPDATEs
// travel the other way, router to peer, so the same two OPENs go in
// transposed: Merge(peerOpen, routerOpen). Callers that always pass
// (routerOpen, peerOpen) answer the wrong question about an adj-RIB-out
// feed.
//
// FourByteAS requires both ends to support RFC 6793, so it is a plain AND,
// and MP is the RFC 4760 intersection: a family is usable only if both ends
// advertised it. Neither depends on which way the UPDATEs travel, so both
// are unchanged by transposing the arguments.
//
// AddPathRecv is the term that is not symmetric. It answers a narrower,
// directional question: for a given family, do the UPDATEs traveling from
// sender to receiver — the ones the BMP route-monitoring messages in
// question mirror — carry a Path Identifier? Per RFC 7911 §3 that is decided
// by that one direction of travel only: the receiving side must have
// advertised it can receive multiple paths (bit 0x1 in its own OPEN), and
// the sending side must have advertised it will send multiple paths (bit
// 0x2 in its OPEN). Two OPENs that each independently advertise only
// "receive" does NOT mean either direction carries path IDs — only
// receiver's receive bit paired with sender's send bit does, which is why
// ParseOpen tracks the two bits in separate tables instead of collapsing
// them into one.
func Merge(receiver, sender Caps) Caps {
	m := newCaps()
	// The negotiated hold time is the SMALLER of the two offered (RFC 4271
	// §4.2), not a union the way every capability below merges. min() rather
	// than a conjunction because this is a timer: getting it backwards would
	// report a session as more patient than it agreed to be.
	m.HoldTime = min(receiver.HoldTime, sender.HoldTime)
	m.FourByteAS = receiver.FourByteAS && sender.FourByteAS
	for f := range receiver.MP {
		if sender.MP[f] {
			m.MP[f] = true
		}
	}
	for f := range receiver.AddPathRecv {
		if sender.addPathSend[f] {
			m.AddPathRecv[f] = true
		}
	}
	return m
}

// sortedFamilies returns the keys of m sorted by AFI then SAFI, so callers
// that must emit deterministic wire output (AppendOpen) never depend on Go's
// randomized map iteration order.
func sortedFamilies(m map[Family]bool) []Family {
	fams := make([]Family, 0, len(m))
	for f := range m {
		fams = append(fams, f)
	}
	sort.Slice(fams, func(i, j int) bool {
		if fams[i].AFI != fams[j].AFI {
			return fams[i].AFI < fams[j].AFI
		}
		return fams[i].SAFI < fams[j].SAFI
	})
	return fams
}

// AppendOpen builds a full OPEN message (builder for tests and bmpgen). It
// emits MP capabilities for every family in c.MP, the 4-octet AS capability
// if c.FourByteAS, and an ADD-PATH capability with both the send and
// receive bits set (value 3) for every family in c.AddPathRecv — this
// builder cannot produce the asymmetric single-bit captures real routers
// sometimes send, since Caps only records what a single OPEN needs to mean
// downstream, not raw wire bits for both directions; hand-built byte slices
// cover that case in tests. Families within each capability type are
// emitted sorted by AFI then SAFI, so two calls with the same Caps always
// produce byte-identical output (needed for bmpgen golden-file fixtures). If
// there are no capabilities to emit at all, the Capabilities optional
// parameter is omitted entirely and the OPEN carries opt-param-len = 0, the
// form a legacy router without RFC 5492 support actually sends — not a
// zero-length Capabilities parameter.
//
// Panics if the capability set doesn't fit the wire format's 1-byte length
// fields, if asn needs the 4-octet AS capability to be representable but
// c.FourByteAS is false, or if bgpID isn't a representable IPv4 address —
// all are caller preconditions, not conditions that arise from untrusted
// wire input, so per package convention (bmp.AppendTLV, bmp.PeerHeader.
// Append) they panic rather than silently emit truncated, zeroed, or wrong
// output. bgpID is unmapped via netip.Addr.Unmap before the IPv4 check, so
// an IPv4-mapped IPv6 form such as "::ffff:10.0.0.1" (Is4In6, not Is4) is
// accepted rather than silently zeroed.
func AppendOpen(dst []byte, asn uint32, holdTime uint16, bgpID string, c Caps) []byte {
	if asn > 0xFFFF && !c.FourByteAS {
		panic(fmt.Sprintf("bgp: AppendOpen: asn %d exceeds 16 bits but c.FourByteAS is false; "+
			"the true AS number would be silently replaced by AS_TRANS (23456) with no capability to recover it", asn))
	}

	var caps []byte
	for _, f := range sortedFamilies(c.MP) {
		caps = append(caps, capMP, 4)
		caps = binary.BigEndian.AppendUint16(caps, f.AFI)
		caps = append(caps, 0, f.SAFI)
	}
	if c.FourByteAS {
		caps = append(caps, cap4ByteAS, 4)
		caps = binary.BigEndian.AppendUint32(caps, asn)
	}
	for _, f := range sortedFamilies(c.AddPathRecv) {
		caps = append(caps, capAddPath, 4)
		caps = binary.BigEndian.AppendUint16(caps, f.AFI)
		caps = append(caps, f.SAFI, addPathRecvBit|addPathSendBit)
	}
	if len(caps) > maxCapsLen {
		panic(fmt.Sprintf("bgp: AppendOpen: capability payload %d bytes exceeds %d, the most the 1-byte "+
			"opt-param-len/param-length fields can encode", len(caps), maxCapsLen))
	}

	addr, err := netip.ParseAddr(bgpID)
	if err != nil {
		panic(fmt.Sprintf("bgp: AppendOpen: bgpID %q is not a valid IP address: %v", bgpID, err))
	}
	addr = addr.Unmap() // ::ffff:10.0.0.1 (Is4In6) -> 10.0.0.1 (Is4), not silently zeroed
	if !addr.Is4() {
		panic(fmt.Sprintf("bgp: AppendOpen: bgpID %q is not representable as an IPv4 BGP identifier", bgpID))
	}
	id := addr.As4()

	body := []byte{4} // BGP version 4
	as2 := uint16(asn)
	if asn > 0xFFFF {
		as2 = 23456 // AS_TRANS (RFC 6793 §4)
	}
	body = binary.BigEndian.AppendUint16(body, as2)
	body = binary.BigEndian.AppendUint16(body, holdTime)
	body = append(body, id[:]...)
	if len(caps) == 0 {
		body = append(body, 0) // opt-param-len = 0: no optional parameters at all
	} else {
		body = append(body, byte(len(caps)+2))
		body = append(body, optParamCaps, byte(len(caps)))
		body = append(body, caps...)
	}

	for range 16 {
		dst = append(dst, 0xFF)
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(bgpHeaderLen+len(body)))
	dst = append(dst, msgOpen)
	return append(dst, body...)
}
