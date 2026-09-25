package bgp

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// EvpnRoute is one decoded EVPN NLRI (RFC 7432 §7). Route types 2, 3 and 5
// populate typed fields; every other type populates Raw with RouteType tagged,
// so it stays losslessly recoverable by vantage reparse.
type EvpnRoute struct {
	RouteType     uint8
	RD            string
	PathID        uint32
	MAC           string
	IP            string
	OriginatingIP string
	Prefix        netip.Prefix
	GatewayIP     string
	EthernetTag   uint32
	ESI           string
	// Labels holds the RAW 24-bit value of each label field, NOT a shifted
	// MPLS label. On a VXLAN fabric this is the VNI; on MPLS it is a 20-bit
	// label in the high bits. Which one it is depends on the Encapsulation
	// Extended Community (RFC 9012 / RFC 8365), which this package does not
	// parse -- so it does not guess. Shifting unconditionally would silently
	// halve every VNI.
	Labels []uint32
	Raw    []byte
}

// parseEvpnNLRI decodes an EVPN MP_REACH/MP_UNREACH NLRI. anyUntyped reports
// whether at least one entry was carried in Raw rather than decoded, which the
// caller turns into PARSE_FLAG_NLRI_UNTYPED.
//
// Each iteration consumes at least the 2-byte route-type+length header (and,
// when addPath, the leading 4-byte path ID) before the loop can continue, so
// len(b) strictly decreases every time around -- forward progress is
// unconditional, not merely true along the error-free path.
func parseEvpnNLRI(b []byte, addPath bool) ([]EvpnRoute, bool, error) {
	var out []EvpnRoute
	anyUntyped := false
	for len(b) > 0 {
		var e EvpnRoute
		if addPath {
			if len(b) < 4 {
				return nil, false, fmt.Errorf("%w: add-path id needs 4 bytes, %d remain", ErrNLRITruncated, len(b))
			}
			e.PathID = binary.BigEndian.Uint32(b[0:4])
			b = b[4:]
		}
		if len(b) < 2 {
			return nil, false, fmt.Errorf("%w: evpn entry header needs 2 bytes, %d remain", ErrNLRITruncated, len(b))
		}
		e.RouteType = b[0]
		vlen := int(b[1])
		if 2+vlen > len(b) {
			return nil, false, fmt.Errorf("%w: evpn entry declares %d bytes, %d remain", ErrNLRITruncated, vlen, len(b)-2)
		}
		val := b[2 : 2+vlen]
		b = b[2+vlen:]

		var err error
		switch e.RouteType {
		case 2:
			err = decodeEvpnType2(&e, val)
		case 3:
			err = decodeEvpnType3(&e, val)
		case 5:
			err = decodeEvpnType5(&e, val)
		default:
			e.Raw = append([]byte{}, val...)
			anyUntyped = true
		}
		if err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	return out, anyUntyped, nil
}

// decodeEvpnType2 reads a MAC/IP Advertisement (RFC 7432 §7.2):
// RD(8) ESI(10) EthTag(4) MACLen(1) MAC(6) IPLen(1) IP(0/4/16) Label1(3) [Label2(3)]
func decodeEvpnType2(e *EvpnRoute, v []byte) error {
	const minLen = 8 + 10 + 4 + 1 + 6 + 1 + 3
	if len(v) < minLen {
		return fmt.Errorf("%w: evpn type 2 declares %d bytes, needs at least %d", ErrNLRIBadLength, len(v), minLen)
	}
	rd, err := decodeRD(v[0:8])
	if err != nil {
		return fmt.Errorf("evpn type 2: %w", err)
	}
	e.RD = rd
	e.ESI = hex.EncodeToString(v[8:18])
	e.EthernetTag = binary.BigEndian.Uint32(v[18:22])
	if macBits := v[22]; macBits != 48 {
		return fmt.Errorf("%w: evpn type 2 mac length %d bits, want 48", ErrNLRIBadLength, macBits)
	}
	e.MAC = net.HardwareAddr(v[23:29]).String()
	ipBits := int(v[29])
	rest := v[30:]
	switch ipBits {
	case 0, 32, 128:
	default:
		return fmt.Errorf("%w: evpn type 2 ip length %d bits", ErrNLRIBadLength, ipBits)
	}
	ipBytes := ipBits / 8
	if len(rest) < ipBytes+3 {
		return fmt.Errorf("%w: evpn type 2 declares %d bytes, too few for its ip and label", ErrNLRIBadLength, len(v))
	}
	if ipBytes > 0 {
		a, ok := netip.AddrFromSlice(rest[:ipBytes])
		if !ok {
			return fmt.Errorf("%w: evpn type 2 ip is %d bytes", ErrNLRIBadLength, ipBytes)
		}
		e.IP = a.String()
	}
	rest = rest[ipBytes:]
	// RFC 7432 §7.2 makes the label region exactly one or two labels. Read as
	// "one label, plus a second if six or more bytes remain", any longer region
	// both invented a second label from trailing bytes and dropped whatever
	// followed -- and reported it as a clean parse, so no flag was set and the
	// raw bytes that were the only record of the discrepancy were discarded.
	// Type 5 already rejects a trailer of the wrong size; types 2 and 3 now do
	// the same.
	//
	// Note the one case this cannot catch: exactly three trailing zero bytes
	// are byte-identical to a legitimate Label2 of 0, which the RFC permits.
	// No decoder can distinguish them, so that shape still yields two labels.
	switch len(rest) {
	case 3:
		e.Labels = append(e.Labels, rawLabel(rest[0:3]))
	case 6:
		e.Labels = append(e.Labels, rawLabel(rest[0:3]), rawLabel(rest[3:6]))
	default:
		return fmt.Errorf("%w: evpn type 2 label region is %d bytes, want 3 or 6", ErrNLRIBadLength, len(rest))
	}
	return nil
}

// decodeEvpnType3 reads an Inclusive Multicast Ethernet Tag route
// (RFC 7432 §7.3): RD(8) EthTag(4) IPLen(1) OrigIP(4 or 16)
func decodeEvpnType3(e *EvpnRoute, v []byte) error {
	const minLen = 8 + 4 + 1
	if len(v) < minLen {
		return fmt.Errorf("%w: evpn type 3 declares %d bytes, needs at least %d", ErrNLRIBadLength, len(v), minLen)
	}
	rd, err := decodeRD(v[0:8])
	if err != nil {
		return fmt.Errorf("evpn type 3: %w", err)
	}
	e.RD = rd
	e.EthernetTag = binary.BigEndian.Uint32(v[8:12])
	ipBits := int(v[12])
	if ipBits != 32 && ipBits != 128 {
		return fmt.Errorf("%w: evpn type 3 ip length %d bits", ErrNLRIBadLength, ipBits)
	}
	ipBytes := ipBits / 8
	// Exact, not minimum: trailing bytes past the originating IP would
	// otherwise be dropped silently and the entry still reported as a clean
	// parse, discarding the raw bytes that were the only record of them.
	if len(v) != 13+ipBytes {
		return fmt.Errorf("%w: evpn type 3 declares %d bytes, want %d for a %d-bit ip", ErrNLRIBadLength, len(v), 13+ipBytes, ipBits)
	}
	a, ok := netip.AddrFromSlice(v[13 : 13+ipBytes])
	if !ok {
		return fmt.Errorf("%w: evpn type 3 ip is %d bytes", ErrNLRIBadLength, ipBytes)
	}
	e.OriginatingIP = a.String()
	return nil
}

// decodeEvpnType5 reads an IP Prefix route (RFC 9136 §3.1):
// RD(8) ESI(10) EthTag(4) PfxLen(1) Prefix(4 or 16) GW(4 or 16) Label(3)
func decodeEvpnType5(e *EvpnRoute, v []byte) error {
	const hdr = 8 + 10 + 4 + 1
	if len(v) < hdr+4+4+3 {
		return fmt.Errorf("%w: evpn type 5 declares %d bytes, needs at least %d", ErrNLRIBadLength, len(v), hdr+11)
	}
	rd, err := decodeRD(v[0:8])
	if err != nil {
		return fmt.Errorf("evpn type 5: %w", err)
	}
	e.RD = rd
	e.ESI = hex.EncodeToString(v[8:18])
	e.EthernetTag = binary.BigEndian.Uint32(v[18:22])
	pfxBits := int(v[22])
	rest := v[23:]

	// The address size is implied by the remaining length: v4 carries a 4-byte
	// prefix and 4-byte gateway, v6 carries 16 and 16, both followed by a
	// 3-byte label.
	var size int
	switch len(rest) {
	case 4 + 4 + 3:
		size = 4
	case 16 + 16 + 3:
		size = 16
	default:
		return fmt.Errorf("%w: evpn type 5 value has %d trailing bytes", ErrNLRIBadLength, len(rest))
	}
	if pfxBits > size*8 {
		return fmt.Errorf("%w: evpn type 5 prefix length %d bits exceeds %d", ErrNLRIBadLength, pfxBits, size*8)
	}
	a, ok := netip.AddrFromSlice(rest[:size])
	if !ok {
		return fmt.Errorf("%w: evpn type 5 prefix is %d bytes", ErrNLRIBadLength, size)
	}
	p, err := a.Prefix(pfxBits)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNLRIBadLength, err)
	}
	e.Prefix = p
	gw, ok := netip.AddrFromSlice(rest[size : size*2])
	if !ok {
		return fmt.Errorf("%w: evpn type 5 gateway is %d bytes", ErrNLRIBadLength, size)
	}
	e.GatewayIP = gw.String()
	e.Labels = append(e.Labels, rawLabel(rest[size*2:size*2+3]))
	return nil
}

// rawLabel returns the unshifted 24-bit value of a 3-byte EVPN label field.
// See EvpnRoute.Labels for why this is deliberately not shifted.
func rawLabel(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// AppendEvpnNLRI encodes EVPN routes in the wire format parseEvpnNLRI decodes
// (RFC 7432 §7): per entry, an optional 4-byte ADD-PATH Path Identifier, then
// a 1-byte route type, a 1-byte value length, and the type's own value.
//
// Only route types 2, 3 and 5 are emitted -- the three this package decodes
// and the three observed from real hardware. An unsupported type panics rather
// than emitting bytes the decoder would carry as Raw: a generator whose output
// silently trips PARSE_FLAG_NLRI_UNTYPED would be manufacturing the exact
// signal that flag exists to report.
//
// Labels are written as RAW 24-bit values, matching how EvpnRoute carries them
// on the way out. On a VXLAN fabric that is the VNI, and shifting it here
// would halve every VNI in generated traffic.
func AppendEvpnNLRI(dst []byte, rs []EvpnRoute, addPath bool) []byte {
	for _, r := range rs {
		if addPath {
			dst = binary.BigEndian.AppendUint32(dst, r.PathID)
		}
		var v []byte
		switch r.RouteType {
		case 2:
			v = appendEvpnType2(r)
		case 3:
			v = appendEvpnType3(r)
		case 5:
			v = appendEvpnType5(r)
		default:
			panic(fmt.Sprintf("bgp: AppendEvpnNLRI: route type %d has no encoder; "+
				"types 2, 3 and 5 are the ones parseEvpnNLRI decodes", r.RouteType))
		}
		if len(v) > 0xFF {
			panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type %d value is %d bytes, exceeds the 1-byte length field", r.RouteType, len(v)))
		}
		dst = append(dst, r.RouteType, byte(len(v)))
		dst = append(dst, v...)
	}
	return dst
}

// evpnRDBytes renders an EvpnRoute's RD, accepting the IPv4 form as well as
// the two ASN forms. EVPN uses the IPv4 form routinely -- NX-OS derives a
// per-VNI RD as <router-id>:<vlan-ish>, e.g. "10.255.1.2:32777" -- so unlike
// encodeRD in update.go, which never has to emit one, this must.
func evpnRDBytes(rd string) []byte {
	if b, err := encodeRD(rd); err == nil {
		return b
	}
	admin, assigned, ok := strings.Cut(rd, ":")
	if !ok {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: route distinguisher %q: want ADMIN:VALUE", rd))
	}
	ip, err := netip.ParseAddr(admin)
	if err != nil || !ip.Is4() {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: route distinguisher %q: administrator is neither a 32-bit number nor an IPv4 address", rd))
	}
	n, err := strconv.ParseUint(assigned, 10, 16)
	if err != nil {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: route distinguisher %q: the IPv4 form takes a 16-bit assigned number", rd))
	}
	out := make([]byte, 8)
	binary.BigEndian.PutUint16(out[0:2], 1)
	a := ip.As4()
	copy(out[2:6], a[:])
	binary.BigEndian.PutUint16(out[6:8], uint16(n))
	return out
}

// evpnESIBytes renders a 10-byte Ethernet Segment Identifier from its hex
// text. An empty string means the single-homed all-zero ESI, which is what
// every route in the corpus carries.
func evpnESIBytes(esi string) []byte {
	if esi == "" {
		return make([]byte, 10)
	}
	b, err := hex.DecodeString(esi)
	if err != nil || len(b) != 10 {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: esi %q must be 20 hex digits (10 bytes)", esi))
	}
	return b
}

// appendRawLabel writes a 24-bit label field verbatim (see EvpnRoute.Labels).
func appendRawLabel(dst []byte, l uint32) []byte {
	return append(dst, byte(l>>16), byte(l>>8), byte(l))
}

// appendEvpnType2 builds a MAC/IP Advertisement value (RFC 7432 §7.2):
// RD(8) ESI(10) EthTag(4) MACLen(1) MAC(6) IPLen(1) IP(0/4/16) Label1(3) [Label2(3)]
func appendEvpnType2(r EvpnRoute) []byte {
	v := evpnRDBytes(r.RD)
	v = append(v, evpnESIBytes(r.ESI)...)
	v = binary.BigEndian.AppendUint32(v, r.EthernetTag)
	mac, err := net.ParseMAC(r.MAC)
	if err != nil || len(mac) != 6 {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type 2 mac %q must be a 6-byte address", r.MAC))
	}
	v = append(v, 48) // MAC length in BITS, not bytes
	v = append(v, mac...)
	if r.IP == "" {
		v = append(v, 0)
	} else {
		ip, err := netip.ParseAddr(r.IP)
		if err != nil {
			panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type 2 ip %q", r.IP))
		}
		if ip.Is4() {
			a := ip.As4()
			v = append(v, 32)
			v = append(v, a[:]...)
		} else {
			a := ip.As16()
			v = append(v, 128)
			v = append(v, a[:]...)
		}
	}
	if len(r.Labels) == 0 || len(r.Labels) > 2 {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type 2 needs one or two labels, got %d", len(r.Labels)))
	}
	for _, l := range r.Labels {
		v = appendRawLabel(v, l)
	}
	return v
}

// appendEvpnType3 builds an Inclusive Multicast Ethernet Tag value
// (RFC 7432 §7.3): RD(8) EthTag(4) IPLen(1) OriginatingIP(4/16).
func appendEvpnType3(r EvpnRoute) []byte {
	v := evpnRDBytes(r.RD)
	v = binary.BigEndian.AppendUint32(v, r.EthernetTag)
	ip, err := netip.ParseAddr(r.OriginatingIP)
	if err != nil {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type 3 originating ip %q", r.OriginatingIP))
	}
	if ip.Is4() {
		a := ip.As4()
		v = append(v, 32)
		return append(v, a[:]...)
	}
	a := ip.As16()
	v = append(v, 128)
	return append(v, a[:]...)
}

// appendEvpnType5 builds an IP Prefix route value (RFC 9136):
// RD(8) ESI(10) EthTag(4) PrefixLen(1) Prefix(4/16) Gateway(4/16) Label(3).
//
// The address size is implied by the total length rather than declared, which
// is why the prefix and gateway must be the same family -- the decoder reads
// the size back out of the trailing length.
func appendEvpnType5(r EvpnRoute) []byte {
	v := evpnRDBytes(r.RD)
	v = append(v, evpnESIBytes(r.ESI)...)
	v = binary.BigEndian.AppendUint32(v, r.EthernetTag)
	if !r.Prefix.IsValid() {
		panic("bgp: AppendEvpnNLRI: type 5 needs a prefix")
	}
	v = append(v, byte(r.Prefix.Bits()))
	gw := r.GatewayIP
	if gw == "" {
		if r.Prefix.Addr().Is4() {
			gw = "0.0.0.0"
		} else {
			gw = "::"
		}
	}
	g, err := netip.ParseAddr(gw)
	if err != nil {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type 5 gateway %q", gw))
	}
	if r.Prefix.Addr().Is4() != g.Is4() {
		panic("bgp: AppendEvpnNLRI: type 5 prefix and gateway must be the same family; " +
			"the decoder infers the address size from the value's total length")
	}
	if r.Prefix.Addr().Is4() {
		a, ga := r.Prefix.Addr().As4(), g.As4()
		v = append(v, a[:]...)
		v = append(v, ga[:]...)
	} else {
		a, ga := r.Prefix.Addr().As16(), g.As16()
		v = append(v, a[:]...)
		v = append(v, ga[:]...)
	}
	if len(r.Labels) != 1 {
		panic(fmt.Sprintf("bgp: AppendEvpnNLRI: type 5 needs exactly one label, got %d", len(r.Labels)))
	}
	return appendRawLabel(v, r.Labels[0])
}
