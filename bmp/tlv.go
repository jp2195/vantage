package bmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// TLV type codes carried in a BMP Initiation message body (RFC 7854 §4.3).
// Termination messages (§4.4) reuse the same type-length-value wire format
// but a different type-code namespace (0 = String, 1 = Reason); this package
// exposes only the generic TLV/ParseTLVs/AppendTLV primitives for those, since
// nothing here needs termination-specific decoding.
const (
	tlvSysDescr = 1
	tlvSysName  = 2
)

// maxSysName and maxSysDescr cap what ParseInit keeps of each banner string.
// Both are copied into every envelope the session publishes, and the TLV
// length field allows 64 KiB of each, so an uncapped banner multiplies every
// event a router sends. sysName is a hostname: 255 bytes holds the longest
// DNS name (253). sysDescr holds a multi-line "show version" style blob on
// some platforms; the longest in the committed captures is 55 bytes (NX-OS
// 10.6), and a full multi-line NX-OS or IOS banner runs a few hundred, so
// 1024 leaves generous room while still bounding the copy.
const (
	maxSysName  = 255
	maxSysDescr = 1024
)

// ErrTLVTruncated indicates a TLV header or its declared value ran past the
// end of the buffer.
var ErrTLVTruncated = errors.New("bmp: tlv truncated")

// TLV is one type-length-value attribute as carried in the body of a BMP
// Initiation or Termination message.
type TLV struct {
	Type  uint16
	Value []byte
}

// ParseTLVs parses a sequence of TLVs occupying the whole of b (RFC 7854
// §4.3/§4.4: 2-byte type, 2-byte length, then length bytes of value,
// repeated to the end of the message). Returned Value slices alias b;
// callers that retain them past b's lifetime must copy.
func ParseTLVs(b []byte) ([]TLV, error) {
	var out []TLV
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, fmt.Errorf("%w: header needs 4 bytes, have %d", ErrTLVTruncated, len(b))
		}
		typ := binary.BigEndian.Uint16(b[0:2])
		l := int(binary.BigEndian.Uint16(b[2:4]))
		if len(b)-4 < l {
			return nil, fmt.Errorf("%w: type %d declares %d bytes, have %d", ErrTLVTruncated, typ, l, len(b)-4)
		}
		out = append(out, TLV{Type: typ, Value: b[4 : 4+l]})
		b = b[4+l:]
	}
	return out, nil
}

// AppendTLV appends one TLV (type, length, value) to dst. Used by tests and
// bmpgen to build wire fixtures. Panics if len(v) > math.MaxUint16 (65535),
// as the TLV wire format uses a uint16 length field and cannot encode longer
// values; this is a programmer error in the caller, not attacker input.
func AppendTLV(dst []byte, typ uint16, v []byte) []byte {
	if len(v) > math.MaxUint16 {
		panic(fmt.Sprintf("AppendTLV: value length %d exceeds max uint16 length %d", len(v), math.MaxUint16))
	}
	dst = binary.BigEndian.AppendUint16(dst, typ)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(v)))
	return append(dst, v...)
}

// InitInfo holds the router-reported identity carried in a BMP Initiation
// message (RFC 7854 §4.3). The vendor-quirk framework keys off SysDescr
// to identify vendor/version and select workarounds for known BMP bugs.
type InitInfo struct {
	SysName  string
	SysDescr string
}

// ParseInit parses a BMP Initiation message payload into InitInfo. TLV type
// 1 (sysDescr) and type 2 (sysName) are recognized; any other TLV type
// (e.g. type 0, free-form string) is present in the message but ignored
// here. Value bytes are not guaranteed to be valid UTF-8 by the wire format,
// so invalid sequences are replaced per byte-run with "�"
// (strings.ToValidUTF8) rather than rejected — a known quirk layer, since
// some vendors emit non-UTF-8 sysDescr/sysName strings. Each is then cut to
// maxSysDescr/maxSysName bytes on a rune boundary.
func ParseInit(payload []byte) (InitInfo, error) {
	tlvs, err := ParseTLVs(payload)
	if err != nil {
		return InitInfo{}, err
	}
	var info InitInfo
	for _, t := range tlvs {
		switch t.Type {
		case tlvSysDescr:
			info.SysDescr = truncateUTF8(strings.ToValidUTF8(string(t.Value), "�"), maxSysDescr)
		case tlvSysName:
			info.SysName = truncateUTF8(strings.ToValidUTF8(string(t.Value), "�"), maxSysName)
		}
	}
	return info, nil
}

// truncateUTF8 cuts valid UTF-8 s to at most n bytes, backing off to the
// start of any rune the cut would split so the result stays valid.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
