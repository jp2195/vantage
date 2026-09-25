// Package bmptest builds syntactically valid BMP messages for tests and the
// bmpgen synthetic router. Builders are wire-writers only — no parsing.
package bmptest

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"slices"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
)

// finish wraps payload in a BMP common header (RFC 7854 §4.1) of the given
// type, ready to write to a TCP conn.
func finish(typ uint8, payload []byte) []byte {
	return append(bmp.AppendHeader(nil, typ, len(payload)), payload...)
}

// Init builds a BMP Initiation message (RFC 7854 §4.3): the common header
// followed by a sysDescr Information TLV (type 1) and a sysName Information
// TLV (type 2).
func Init(sysName, sysDescr string) []byte {
	p := bmp.AppendTLV(nil, 1, []byte(sysDescr))
	p = bmp.AppendTLV(p, 2, []byte(sysName))
	return finish(bmp.TypeInitiation, p)
}

// PeerUp builds a BMP Peer Up Notification (RFC 7854 §4.10): the per-peer
// header, a 16-byte local address (an IPv4 address occupies only the last 4
// bytes, with the leading 12 zeroed — RFC 7854's IPv4-mapped-IPv6 form), a
// 2-byte local port, a 2-byte remote port, the OPEN message the monitored
// router sent to its peer, and the OPEN message it received back. The two
// OPEN messages need no BMP-level delimiter between them: each is a complete,
// self-describing RFC 4271 §4.1 BGP message with its own 19-byte header
// (16-byte marker + 2-byte length + 1-byte type), so a reader determines
// where the first ends and the second begins from that embedded length
// field alone.
//
// The sent OPEN always advertises BGP ID "1.1.1.1" and the received OPEN
// "2.2.2.2", both with a 180-second hold time — part of this function's
// contract.
func PeerUp(ph bmp.PeerHeader, localIP netip.Addr, localPort, remotePort uint16,
	routerCaps, peerCaps bgp.Caps, routerASN, peerASN uint32) []byte {
	p := ph.Append(nil)
	var a16 [16]byte
	if localIP.Is4() {
		a4 := localIP.As4()
		copy(a16[12:], a4[:])
	} else {
		a16 = localIP.As16()
	}
	p = append(p, a16[:]...)
	p = binary.BigEndian.AppendUint16(p, localPort)
	p = binary.BigEndian.AppendUint16(p, remotePort)
	p = bgp.AppendOpen(p, routerASN, 180, "1.1.1.1", routerCaps)
	p = bgp.AppendOpen(p, peerASN, 180, "2.2.2.2", peerCaps)
	return finish(bmp.TypePeerUp, p)
}

// PeerDown builds a BMP Peer Down Notification (RFC 7854 §4.9): the per-peer
// header, a 1-byte reason code, and an opaque Data field whose shape (a BGP
// PDU for reasons 1/3, a 2-byte FSM event code for reason 2, or nothing for
// reasons 4/5) is determined entirely by reason. Callers supply data already
// shaped for the reason they pass; this builder does not interpret it.
func PeerDown(ph bmp.PeerHeader, reason uint8, data []byte) []byte {
	p := append(ph.Append(nil), reason)
	p = append(p, data...)
	return finish(bmp.TypePeerDown, p)
}

// RouteMonitoring builds a BMP Route Monitoring message (RFC 7854 §4.6): the
// per-peer header immediately followed by one complete BGP UPDATE PDU
// (including that PDU's own 19-byte BGP header).
func RouteMonitoring(ph bmp.PeerHeader, u bgp.BuildUpdate) []byte {
	p := bgp.AppendUpdate(ph.Append(nil), u)
	return finish(bmp.TypeRouteMonitoring, p)
}

// Stats builds a BMP Stats Report message (RFC 7854 §4.8): the per-peer
// header, a 4-byte count of the stat TLVs that follow, and one TLV per
// counter (2-byte stat type, 2-byte length, then that many bytes of stat
// data).
//
// Stat Data width follows the RFC's own type registry rather than a fixed
// 8 bytes: §4.8 defines types 0-6 and 11-13 as 32-bit Counters and types
// 7-10 as 64-bit Gauges, and Stat Len is the length of the Stat Data that
// follows. Emitting 8 bytes for a counter type would make this builder
// produce a message no real router sends, which matters because these
// bytes are the fixture corpus and bmpgen's synthetic traffic -- a fake
// router that is wrong in the same way as our parser proves nothing. A
// counter value that does not fit 32 bits panics rather than silently
// wrapping, per this package's builders-guard-their-preconditions rule.
// Unknown types above 13 default to the 64-bit form, since a future
// registry addition is more likely to be a gauge and truncation is the
// worse failure.
//
// counters is a Go map, so range order is not inherently deterministic;
// entries are emitted sorted by stat type ascending so identical input always
// produces byte-identical output, as required of anything that becomes a
// test fixture or bmpgen wire traffic. Panics if a counter type exceeds the
// wire format's 2-byte stat-type field (math.MaxUint16) — silently truncating
// the type would misidentify which counter the value belongs to, so this
// guards its precondition the same way bmp.AppendTLV and bgp.AppendOpen/
// AppendUpdate guard theirs.
func Stats(ph bmp.PeerHeader, counters map[uint32]uint64) []byte {
	p := ph.Append(nil)
	p = binary.BigEndian.AppendUint32(p, uint32(len(counters)))

	types := make([]uint32, 0, len(counters))
	for typ := range counters {
		if typ > math.MaxUint16 {
			panic(fmt.Sprintf("bmptest: Stats: counter type %d exceeds the 2-byte stat-type field's range", typ))
		}
		types = append(types, typ)
	}
	slices.Sort(types)

	for _, typ := range types {
		v := counters[typ]
		p = binary.BigEndian.AppendUint16(p, uint16(typ))
		if statIs32Bit(typ) {
			if v > math.MaxUint32 {
				panic(fmt.Sprintf("bmptest: Stats: counter type %d is a 32-bit Counter (RFC 7854 §4.8) but value %d exceeds 32 bits", typ, v))
			}
			p = binary.BigEndian.AppendUint16(p, 4)
			p = binary.BigEndian.AppendUint32(p, uint32(v))
			continue
		}
		p = binary.BigEndian.AppendUint16(p, 8)
		p = binary.BigEndian.AppendUint64(p, v)
	}
	return finish(bmp.TypeStatsReport, p)
}

// statIs32Bit reports whether stat type typ is one of RFC 7854 §4.8's
// 32-bit Counters (types 0-6 and 11-13). Types 7-10 are 64-bit Gauges, and
// unregistered types above 13 are treated as 64-bit so a future addition is
// never silently truncated.
func statIs32Bit(typ uint32) bool {
	return typ <= 6 || (typ >= 11 && typ <= 13)
}

// Termination builds a BMP Termination message (RFC 7854 §4.5): a single
// Information TLV of type 1 (Reason) carrying a 2-byte reason code.
func Termination(reason uint16) []byte {
	v := binary.BigEndian.AppendUint16(nil, reason)
	return finish(bmp.TypeTermination, bmp.AppendTLV(nil, 1, v))
}
