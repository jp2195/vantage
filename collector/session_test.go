package collector

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
	"github.com/jp2195/vantage/quirk"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

var t0 = time.Unix(1753600000, 0).UTC()

func newTestSession() *Session {
	return NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 111, func() time.Time { return t0 }, Overrides{})
}

func peerHdr() bmp.PeerHeader {
	return bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: t0}
}

func fullCaps() bgp.Caps {
	c := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
	c.FourByteAS = true
	return c
}

func mustOne(t *testing.T, evs []Event) Event {
	t.Helper()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(evs), evs)
	}
	return evs[0]
}

func mustMsg(t *testing.T, raw []byte) bmp.Msg {
	t.Helper()
	m, err := bmp.ReadMsg(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func hasFlag(env *vantagev1.Envelope, f vantagev1.ParseFlag) bool {
	return slices.Contains(env.ParseFlags, f)
}

// Router token for 10.0.0.1 is hex(0a,00,00,01); peer token for 10.0.0.9 is
// hex(0a,00,00,09) -- subjects.EncodeIP encodes address bytes as hex, not
// dashed decimal text.
const (
	routerTok = "0a000001"
	peerTok   = "0a000009"
)

func TestSessionLifecycle(t *testing.T) {
	s := newTestSession()

	if evs := s.Handle(mustMsg(t, bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2"))); len(evs) != 0 {
		t.Fatalf("init must emit nothing, got %v", evs)
	}

	up := mustOne(t, s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001))))
	if up.Subject != "vantage.v1.peer."+routerTok+"."+peerTok || up.Env.Seq != 1 ||
		up.Env.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_UP {
		t.Fatalf("up=%+v", up)
	}
	if up.MsgID != routerTok+"/"+peerTok+"/111/1" {
		t.Fatalf("msgid=%s", up.MsgID)
	}
	if up.Env.Router.SysName != "rr1" || up.Env.RouterInfo.Os != "iosxr" {
		t.Fatalf("router identity not applied: %+v", up.Env)
	}
	if up.Env.GetPeerEvent().GetCaps().GetFourByteAs() != true {
		t.Fatalf("negotiated caps not attached: %+v", up.Env.GetPeerEvent())
	}

	rm := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		ASPath:    []uint32{65001}, FourByteAS: true,
		NextHop: netip.MustParseAddr("10.0.0.9"),
	}))))
	if rm.Subject != "vantage.v1.route.ipv4u."+routerTok+"."+peerTok || rm.Env.Seq != 2 {
		t.Fatalf("rm=%+v", rm)
	}
	if rm.MsgID != routerTok+"/"+peerTok+"/111/2" {
		t.Fatal(rm.MsgID)
	}
	if len(rm.Env.GetRoute().Announced) != 1 || rm.Env.GetRoute().Announced[0].Prefix != "192.0.2.0/24" {
		t.Fatalf("route=%v", rm.Env.GetRoute())
	}
	if hasFlag(rm.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Fatalf("caps were on record, must not flag CAPS_MISSING: %v", rm.Env.ParseFlags)
	}

	st := mustOne(t, s.Handle(mustMsg(t, bmptest.Stats(peerHdr(), map[uint32]uint64{7: 42}))))
	if st.Env.GetStats().Counters[7] != 42 || st.Env.Seq != 3 {
		t.Fatalf("stats=%+v", st.Env)
	}

	dn := mustOne(t, s.Handle(mustMsg(t, bmptest.PeerDown(peerHdr(), 2, nil))))
	if dn.Env.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_DOWN || dn.Env.Seq != 4 {
		t.Fatalf("down=%+v", dn.Env)
	}
}

func TestSessionZeroTimestampFallback(t *testing.T) {
	s := newTestSession()
	ph := peerHdr()
	ph.Timestamp = time.Time{}
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.Stats(ph, map[uint32]uint64{1: 1}))))
	if !ev.Env.TsRouter.AsTime().Equal(t0) {
		t.Fatalf("ts_router=%v", ev.Env.TsRouter.AsTime())
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

func TestSessionRMWithoutPeerUp(t *testing.T) {
	s := newTestSession()
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}))))
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionPeerUpWithUnparseableOpensStillFlagsCapsMissing pins the split
// between "peer is up" and "we know its capabilities". A Peer-Up whose
// embedded OPENs cannot be parsed establishes the first but not the second:
// ps.caps stays empty, so the UPDATE below is parsed capability-blind. Before
// capsKnown existed, QK_CAPS_MISSING was keyed off ps.up alone, so this peer's
// Route Monitoring went out unflagged while being exactly as
// capability-blind as a peer with no Peer-Up at all.
func TestSessionPeerUpWithUnparseableOpensStillFlagsCapsMissing(t *testing.T) {
	s := newTestSession()

	// A well-formed Peer-Up envelope (per-peer header + local addr + ports)
	// whose OPEN region is garbage rather than two BGP OPEN messages.
	ph := peerHdr()
	payload := ph.Append(nil)
	payload = append(payload, make([]byte, 16)...)    // local address
	payload = append(payload, 0, 179, 0xCC, 0x79)     // local port, remote port
	payload = append(payload, 0xDE, 0xAD, 0xBE, 0xEF) // not a BGP OPEN
	up := s.Handle(bmp.Msg{Type: bmp.TypePeerUp, Payload: payload})
	if len(up) != 1 || up[0].Env.GetPeerEvent() == nil {
		t.Fatalf("Peer-Up should still produce a PeerEvent: %+v", up)
	}
	if up[0].Env.GetPeerEvent().Caps != nil {
		t.Fatal("capabilities must not be reported when the OPENs did not parse")
	}

	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}))))
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Fatalf("route monitoring after a caps-less Peer-Up must carry CAPS_MISSING, flags=%v", ev.Env.ParseFlags)
	}
}

func TestSessionGarbageBecomesRaw(t *testing.T) {
	s := newTestSession()
	ev := mustOne(t, s.Handle(bmp.Msg{Type: bmp.TypeRouteMonitoring, Payload: []byte{1, 2, 3}}))
	if ev.Subject != "vantage.v1.raw."+routerTok || ev.Env.GetRaw() == nil || ev.Env.GetRaw().ParseError == "" {
		t.Fatalf("ev=%+v", ev)
	}
}

func TestSessionTerminationBecomesRaw(t *testing.T) {
	s := newTestSession()
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.Termination(1))))
	if ev.Subject != "vantage.v1.raw."+routerTok || ev.Env.GetRaw() == nil || ev.Env.GetRaw().ParseError != "" {
		t.Fatalf("termination should be a clean (no-parse-error) RawEvent, got %+v", ev)
	}
}

// TestSessionPeerFlapSeqNeverRepeats pins the fix documented on peerState:
// Peer-Down must not reset the per-peer seq counter, and a Route Monitoring
// message arriving between a Peer-Down and the next Peer-Up for the same
// peer must see capabilities as missing again (a flap is functionally a new
// negotiation, even though the (router, peer) identity is unchanged).
func TestSessionPeerFlapSeqNeverRepeats(t *testing.T) {
	s := newTestSession()

	up1 := mustOne(t, s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001))))
	if up1.Env.Seq != 1 {
		t.Fatalf("up1 seq=%d", up1.Env.Seq)
	}

	down := mustOne(t, s.Handle(mustMsg(t, bmptest.PeerDown(peerHdr(), 2, nil))))
	if down.Env.Seq != 2 {
		t.Fatalf("down seq=%d", down.Env.Seq)
	}

	// RM after Down, before the next Peer-Up: caps must read as missing again.
	rmDown := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}))))
	if rmDown.Env.Seq != 3 || !hasFlag(rmDown.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Fatalf("rmDown=%+v flags=%v", rmDown.Env, rmDown.Env.ParseFlags)
	}

	up2 := mustOne(t, s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001))))
	if up2.Env.Seq != 4 {
		t.Fatalf("up2 seq=%d", up2.Env.Seq)
	}
	if up2.MsgID == up1.MsgID {
		t.Fatalf("post-flap Peer-Up must not reuse the pre-flap msg-id: both are %s", up1.MsgID)
	}

	rmUp := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}))))
	if rmUp.Env.Seq != 5 || hasFlag(rmUp.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Fatalf("rmUp=%+v flags=%v (caps should be back on record post-flap)", rmUp.Env, rmUp.Env.ParseFlags)
	}
}

// TestSessionVersionUnparsedFlag pins that a router whose sysDescr
// names a known vendor/OS but not a version this package can parse must
// carry PARSE_FLAG_VERSION_UNPARSED on every subsequent envelope, not just
// silently affect internal quirk matching with no observable signal.
func TestSessionVersionUnparsedFlag(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.Init("rr1", "Cisco IOS XR Software, no recognizable version token")))

	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.Stats(peerHdr(), map[uint32]uint64{1: 1}))))
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_VERSION_UNPARSED) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
	if ev.Env.RouterInfo.Vendor != "cisco" || ev.Env.RouterInfo.Os != "iosxr" {
		t.Fatalf("router info=%+v", ev.Env.RouterInfo)
	}
}

// --- Route Monitoring fixtures bgp.BuildUpdate cannot express: raw
// MP_REACH_NLRI attributes for non-ipv4u families. Mirrors the hand-built
// wire helpers bgp/update_test.go uses for the same reason (buildBody,
// buildAttr, mpReachVal), redefined here because those are unexported.

const (
	bgpAttrMPReach   = 14 // RFC 4760 §3
	bgpAttrMPUnreach = 15 // RFC 4760 §3
	bgpMsgUpdate     = 2  // RFC 4271 §4.1
)

func buildAttr(flags, typ uint8, val []byte) []byte {
	return append([]byte{flags, typ, byte(len(val))}, val...)
}

func mpReachVal(afi uint16, safi uint8, nh, nlri []byte) []byte {
	v := binary.BigEndian.AppendUint16(nil, afi)
	v = append(v, safi, byte(len(nh)))
	v = append(v, nh...)
	v = append(v, 0) // reserved
	return append(v, nlri...)
}

func updateBody(withdrawn, attrs, nlri []byte) []byte {
	body := []byte{byte(len(withdrawn) >> 8), byte(len(withdrawn))}
	body = append(body, withdrawn...)
	body = append(body, byte(len(attrs)>>8), byte(len(attrs)))
	body = append(body, attrs...)
	return append(body, nlri...)
}

// rmMsg builds a bmp.Msg carrying a Route Monitoring message (per-peer header
// + a BGP UPDATE with the given body) without going through the wire
// round-trip -- Handle takes a bmp.Msg directly, per TestSessionGarbageBecomesRaw.
func rmMsg(ph bmp.PeerHeader, body []byte) bmp.Msg {
	p := ph.Append(nil)
	for range 16 {
		p = append(p, 0xFF) // BGP marker (RFC 4271 §4.1)
	}
	p = binary.BigEndian.AppendUint16(p, uint16(19+len(body)))
	p = append(p, bgpMsgUpdate)
	p = append(p, body...)
	return bmp.Msg{Type: bmp.TypeRouteMonitoring, Payload: p}
}

// TestSessionRouteMonitoringOtherFamilyRawBytes pins that raw MP_REACH
// bytes must be preserved on the RouteEvent (route.{family} subject) for
// every non-ipv4u family, not only BGP-LS -- VPNv4 here.
//
// VPNv4 (AFI 1 / SAFI 128) has a registered vpn4 decoder in the bgp
// package, and the 8 opaque bytes below are not a valid vpn4 entry, so the
// decoder runs and fails. That degrades to the same raw-bytes-preserved
// outcome as before, but the flag is now PARSE_FLAG_NLRI_UNTYPED ("a
// decoder ran and failed"), not PARSE_FLAG_UNKNOWN_FAMILY, which means "no
// decoder exists for this family at all." The subject/RawReach assertions
// -- the actual point of this test -- are unaffected.
func TestSessionRouteMonitoringOtherFamilyRawBytes(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	nh := []byte{10, 0, 0, 9}
	nlri := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07} // not a valid vpn4 entry: decodes with an error
	val := mpReachVal(1, 128, nh, nlri)                            // AFI 1 / SAFI 128 = VPNv4 (RFC 4364)
	body := updateBody(nil, buildAttr(0x80, bgpAttrMPReach, val), nil)

	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	if ev.Subject != "vantage.v1.route.vpn4."+routerTok+"."+peerTok {
		t.Fatalf("subject=%s", ev.Subject)
	}
	re := ev.Env.GetRoute()
	if re == nil {
		t.Fatalf("want RouteEvent, got %+v", ev.Env.Payload)
	}
	if string(re.RawReach) != string(val) {
		t.Fatalf("RawReach=%x, want %x", re.RawReach, val)
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionRouteMonitoringLSFamilyRoutesToLsSubject exercises the ls
// subject / LsEvent branch, which also carries the raw MP_REACH/MP_UNREACH
// bytes every non-ipv4u family gets (BGP-LS is
// AFI 16388, still not FamilyIPv4U as far as bgp.ParseUpdate is concerned).
func TestSessionRouteMonitoringLSFamilyRoutesToLsSubject(t *testing.T) {
	s := newTestSession()
	nh := []byte{10, 0, 0, 9}
	nlri := []byte{0xaa, 0xbb}
	val := mpReachVal(16388, 71, nh, nlri) // BGP-LS (RFC 7752)
	body := updateBody(nil, buildAttr(0x80, bgpAttrMPReach, val), nil)

	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	if ev.Subject != "vantage.v1.ls."+routerTok+"."+peerTok {
		t.Fatalf("subject=%s", ev.Subject)
	}
	ls := ev.Env.GetLs()
	if ls == nil {
		t.Fatalf("want LsEvent, got %+v", ev.Env.Payload)
	}
	if string(ls.RawReach) != string(val) {
		t.Fatalf("RawReach=%x, want %x", ls.RawReach, val)
	}
}

// TestSessionTreatAsWithdrawFoldsAnnouncedIntoWithdrawn pins the Handle-level
// contract for an RFC 7606 treat-as-withdraw outcome: Announced must be
// folded into Withdrawn and cleared, and the flag must reach the envelope.
func TestSessionTreatAsWithdrawFoldsAnnouncedIntoWithdrawn(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	// NEXT_HOP (type 3) with a malformed 3-byte value instead of 4 (RFC 7606
	// §7.3: malformed NEXT_HOP is treat-as-withdraw).
	attrs := buildAttr(0x40, 3, []byte{10, 0, 0})
	nlri := bgp.AppendPrefixesV4(nil, []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}}, false)
	body := updateBody(nil, attrs, nlri)

	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	re := ev.Env.GetRoute()
	if re == nil {
		t.Fatalf("want RouteEvent, got %+v", ev.Env.Payload)
	}
	if len(re.Announced) != 0 {
		t.Fatalf("want Announced cleared under treat-as-withdraw, got %v", re.Announced)
	}
	if len(re.Withdrawn) != 1 || re.Withdrawn[0].Prefix != "192.0.2.0/24" {
		t.Fatalf("want folded withdrawn, got %v", re.Withdrawn)
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionTreatAsWithdrawFoldsVpnAnnouncedIntoWithdrawn pins that an
// RFC 7606 treat-as-withdraw outcome must fold VpnAnnounced into
// VpnWithdrawn too, not only the classic ipv4u Announced/Withdrawn pair
// TestSessionTreatAsWithdrawFoldsAnnouncedIntoWithdrawn already pins.
// Without the fold, a malformed ORIGIN alongside a clean vpn4 MP_REACH
// publishes the route as an announcement -- the only NLRI representation
// on the wire -- even though RFC 7606 requires the whole route be treated
// as withdrawn.
func TestSessionTreatAsWithdrawFoldsVpnAnnouncedIntoWithdrawn(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	// Malformed ORIGIN (type 1, RFC 7606 §7.1 requires exactly 1 byte):
	// triggers treat-as-withdraw for the whole UPDATE.
	attrs := buildAttr(0x40, 1, []byte{0, 0})

	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v := []byte{0x00, 0x01, 0x80, 0x0C} // AFI 1, SAFI 128, next-hop-length 12
	v = append(v, make([]byte, 8)...)   // the RD, always zero
	v = append(v, 0x0A, 0x00, 0x00, 0x09)
	v = append(v, 0x00) // reserved
	v = append(v, nlri...)
	attrs = append(attrs, buildAttr(0x80, bgpAttrMPReach, v)...)

	body := updateBody(nil, attrs, nil)
	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	re := ev.Env.GetRoute()
	if re == nil {
		t.Fatalf("want RouteEvent, got %+v", ev.Env.Payload)
	}
	if len(re.VpnAnnounced) != 0 {
		t.Fatalf("want vpn_announced cleared under treat-as-withdraw, got %+v", re.VpnAnnounced)
	}
	if len(re.VpnWithdrawn) != 1 {
		t.Fatalf("want the announced vpn prefix folded into vpn_withdrawn, got %+v", re.VpnWithdrawn)
	}
	got := re.VpnWithdrawn[0]
	if got.Prefix != "10.0.0.0/24" || got.Rd != "65000:100" {
		t.Fatalf("vpn withdrawn = %+v", got)
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionTreatAsWithdrawFoldsEvpnAnnouncedIntoWithdrawn is the EVPN
// counterpart of TestSessionTreatAsWithdrawFoldsVpnAnnouncedIntoWithdrawn.
func TestSessionTreatAsWithdrawFoldsEvpnAnnouncedIntoWithdrawn(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	attrs := buildAttr(0x40, 1, []byte{0, 0}) // malformed ORIGIN

	val := []byte{0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01} // RD 65000:1
	val = append(val, 0x00, 0x00, 0x00, 0x64)                     // Ethernet Tag 100
	val = append(val, 32)                                         // IP length in bits
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)                     // Originating IP 10.0.0.11
	nlri := append([]byte{3, byte(len(val))}, val...)             // route type 3

	v := []byte{0x00, 0x19, 0x46, 0x04}   // AFI 25, SAFI 70 (EVPN), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // reserved
	v = append(v, nlri...)
	attrs = append(attrs, buildAttr(0x80, bgpAttrMPReach, v)...)

	body := updateBody(nil, attrs, nil)
	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	re := ev.Env.GetRoute()
	if re == nil {
		t.Fatalf("want RouteEvent, got %+v", ev.Env.Payload)
	}
	if len(re.EvpnAnnounced) != 0 {
		t.Fatalf("want evpn_announced cleared under treat-as-withdraw, got %+v", re.EvpnAnnounced)
	}
	if len(re.EvpnWithdrawn) != 1 {
		t.Fatalf("want the announced evpn route folded into evpn_withdrawn, got %+v", re.EvpnWithdrawn)
	}
	got := re.EvpnWithdrawn[0]
	if got.RouteType != 3 || got.Rd != "65000:1" || got.EthernetTag != 100 || got.OriginatingIp != "10.0.0.11" {
		t.Fatalf("evpn withdrawn = %+v", got)
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionHardParseErrorBecomesRaw: a genuinely truncated UPDATE (as
// opposed to an RFC 7606 outcome) must degrade to a RawEvent, per
// ParseUpdate's two-tier error model (see bgp/update.go's doc comment).
func TestSessionHardParseErrorBecomesRaw(t *testing.T) {
	s := newTestSession()
	// withdrawn-routes-length declares more than the body actually has: a
	// hard ErrUpdateTruncated from bgp.ParseUpdate, not a 7606 outcome.
	body := []byte{0, 200, 1, 2, 0, 0}
	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	if ev.Subject != "vantage.v1.raw."+routerTok || ev.Env.GetRaw() == nil || ev.Env.GetRaw().ParseError == "" {
		t.Fatalf("ev=%+v", ev)
	}
}

// TestConcurrentSessionsRaceClean is what -race actually needs to exercise
// for this package: the collector runs one Session per BMP TCP connection,
// many connections concurrently. A single Session is never touched by more
// than one goroutine (same single-goroutine-owned contract as quirk.Set),
// but many independent Sessions do run at once, all reading the shared,
// read-only quirk.Registry and calling into the stateless bgp/bmp/subjects
// packages simultaneously. This drives many Sessions through a full
// Init/Peer-Up/RM/Stats/Peer-Down/flap lifecycle in parallel so the race
// detector has something real to check beyond one goroutine's sequential
// calls.
func TestConcurrentSessionsRaceClean(t *testing.T) {
	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := netip.MustParseAddr(fmt.Sprintf("10.0.%d.1", i))
			s := NewSession(addr, "c1", uint64(1000+i), func() time.Time { return t0 }, Overrides{})
			ph := peerHdr()

			s.Handle(mustMsg(t, bmptest.Init(fmt.Sprintf("rr%d", i), "Cisco IOS XR Software, Version 7.9.2")))
			s.Handle(mustMsg(t, bmptest.PeerUp(ph, addr, 179, 33001, fullCaps(), fullCaps(), 65000, 65001)))
			for range 5 {
				s.Handle(mustMsg(t, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
					Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
					NextHop:   netip.MustParseAddr("10.0.0.9"),
				})))
			}
			s.Handle(mustMsg(t, bmptest.Stats(ph, map[uint32]uint64{7: 42})))
			s.Handle(mustMsg(t, bmptest.PeerDown(ph, 2, nil)))
			s.Handle(mustMsg(t, bmptest.PeerUp(ph, addr, 179, 33001, fullCaps(), fullCaps(), 65000, 65001)))
			ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("198.51.100.0/24")}},
				NextHop:   netip.MustParseAddr("10.0.0.9"),
			}))))
			if ev.Env.Seq != 10 {
				t.Errorf("session %d: final rm seq=%d, want 10", i, ev.Env.Seq)
			}
		}(i)
	}
	wg.Wait()
}

// TestSessionVpn4TypedReachesEnvelope pins that typed VPN prefixes survive the
// session layer onto the envelope, and that a clean parse carries no raw
// bytes.
//
// An earlier fixture built the MP_REACH next hop as a bare 4-byte IPv4
// address (next-hop-length 4), which no real vpn4 speaker sends -- RFC 4364
// §4.3.2 fixes a vpn4 next hop at an 8-byte all-zero Route Distinguisher
// followed by the 4-byte address, 12 bytes total. A 4-byte next hop is a
// length nextHopForFamily does not recognize for vpn4 (bgp/nlri.go), so the
// parser keeps the raw MP_REACH bytes and raises PARSE_FLAG_NLRI_UNTYPED --
// which would have failed this test's own "clean typed parse must carry no
// raw bytes" assertion. Built correctly here (12-byte next hop), matching
// bgp/update_test.go's TestParseUpdateVpn4Typed.
func TestSessionVpn4TypedReachesEnvelope(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v := []byte{0x00, 0x01, 0x80, 0x0C}   // AFI 1, SAFI 128, next-hop-length 12
	v = append(v, make([]byte, 8)...)     // the RD, always zero (RFC 4364 §4.3.2)
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // RFC 4760 §3 reserved byte
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, 14, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.VpnAnnounced) != 1 {
		t.Fatalf("vpn_announced = %+v", r.VpnAnnounced)
	}
	got := r.VpnAnnounced[0]
	if got.Prefix != "10.0.0.0/24" || got.Rd != "65000:100" ||
		len(got.Labels) != 1 || got.Labels[0] != 24001 {
		t.Fatalf("vpn prefix = %+v", got)
	}
	// The next hop is what an earlier, broken fixture silently lost: with a
	// 4-byte next hop the parser cannot recognize it for vpn4 at all, so
	// Attrs.NextHop stays "" even though this assertion block would still
	// pass. Asserting it lands is the whole point of fixing the fixture.
	if r.Attrs.GetNextHop() != "10.0.0.9" {
		t.Fatalf("next hop = %q, want 10.0.0.9", r.Attrs.GetNextHop())
	}
	if len(r.RawReach) != 0 {
		t.Fatalf("clean typed parse must carry no raw bytes, got %d", len(r.RawReach))
	}
	if ev.Subject != "vantage.v1.route.vpn4."+routerTok+"."+peerTok {
		t.Fatalf("subject = %q", ev.Subject)
	}
}

// bgpUpdateWithAttr builds a complete BGP UPDATE PDU carrying exactly one path
// attribute, for feeding to Session.Handle as Route Monitoring.
func bgpUpdateWithAttr(t *testing.T, flags, typ uint8, val []byte) []byte {
	t.Helper()
	// byte(len(val)) truncates silently above 255, and this helper never
	// sets the extended-length attribute flag (0x10), so a fixture over 255
	// bytes would produce a structurally wrong PDU that fails somewhere
	// unrelated to whatever the test actually meant to exercise. Both
	// current callers are well under 255 bytes; this guards against a
	// future large fixture doing that silently.
	if len(val) > 255 {
		t.Fatalf("bgpUpdateWithAttr: val is %d bytes, exceeds the 255-byte range of this helper's "+
			"non-extended-length attribute-length field", len(val))
	}
	attr := append([]byte{flags, typ, byte(len(val))}, val...)
	body := []byte{0, 0} // withdrawn-routes-length
	body = append(body, byte(len(attr)>>8), byte(len(attr)))
	body = append(body, attr...)
	msg := make([]byte, 16)
	for i := range msg {
		msg[i] = 0xFF
	}
	l := 19 + len(body)
	msg = append(msg, byte(l>>8), byte(l), 2)
	return append(msg, body...)
}

// TestSessionEvpnTypedReachesEnvelope extends vpn4 coverage to EVPN: the
// gap being closed is "a fully-decoded vpn4 or EVPN route reaches NATS ...
// with no prefixes at all," and only vpn4 was pinned by
// TestSessionVpn4TypedReachesEnvelope. A type-3 Inclusive Multicast
// Ethernet Tag route (RFC 7432 §7.3) with a 4-byte IPv4 next hop, built to
// match the fixture in bgp/update_test.go's
// TestParseUpdateEvpnNextHop4And16Bytes.
func TestSessionEvpnTypedReachesEnvelope(t *testing.T) {
	val := []byte{0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01} // RD 65000:1
	val = append(val, 0x00, 0x00, 0x00, 0x64)                     // Ethernet Tag 100
	val = append(val, 32)                                         // IP length in bits
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)                     // Originating IP 10.0.0.11
	nlri := append([]byte{3, byte(len(val))}, val...)             // route type 3, evpn entry header

	v := []byte{0x00, 0x19, 0x46, 0x04}   // AFI 25, SAFI 70 (EVPN), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // RFC 4760 §3 reserved byte
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, 14, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.EvpnAnnounced) != 1 {
		t.Fatalf("evpn_announced = %+v", r.EvpnAnnounced)
	}
	got := r.EvpnAnnounced[0]
	if got.RouteType != 3 || got.Rd != "65000:1" || got.EthernetTag != 100 || got.OriginatingIp != "10.0.0.11" {
		t.Fatalf("evpn route = %+v", got)
	}
	// Prefix is only meaningful for route type 5; a type-3 route's
	// zero-value netip.Prefix must not be stamped onto the wire (an
	// unconditional r.Prefix.String() renders as the literal string
	// "invalid Prefix").
	if got.Prefix != "" {
		t.Fatalf("type-3 evpn route must not carry a prefix, got %q", got.Prefix)
	}
	if r.Attrs.GetNextHop() != "10.0.0.9" {
		t.Fatalf("next hop = %q, want 10.0.0.9", r.Attrs.GetNextHop())
	}
	if len(r.RawReach) != 0 {
		t.Fatalf("clean typed parse must carry no raw bytes, got %d", len(r.RawReach))
	}
	if ev.Subject != "vantage.v1.route.evpn."+routerTok+"."+peerTok {
		t.Fatalf("subject = %q", ev.Subject)
	}
}

// TestSessionVpn4WithdrawReachesEnvelope pins that a vpn4 MP_UNREACH must
// decode into VpnWithdrawn and reach the envelope with no raw bytes on a
// clean parse. Mutation-proved: replacing
// `re.VpnWithdrawn = vpnProto(u.VpnWithdrawn)` with a no-op left the full
// suite passing.
func TestSessionVpn4WithdrawReachesEnvelope(t *testing.T) {
	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	v := []byte{0x00, 0x01, 0x80} // AFI 1, SAFI 128 -- MP_UNREACH has no next hop/reserved byte
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPUnreach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.VpnWithdrawn) != 1 {
		t.Fatalf("vpn_withdrawn = %+v", r.VpnWithdrawn)
	}
	got := r.VpnWithdrawn[0]
	if got.Prefix != "10.0.0.0/24" || got.Rd != "65000:100" ||
		len(got.Labels) != 1 || got.Labels[0] != 24001 {
		t.Fatalf("vpn withdrawn = %+v", got)
	}
	if len(r.RawUnreach) != 0 {
		t.Fatalf("clean typed parse must carry no raw bytes, got %d", len(r.RawUnreach))
	}
	if ev.Subject != "vantage.v1.route.vpn4."+routerTok+"."+peerTok {
		t.Fatalf("subject = %q", ev.Subject)
	}
}

// TestSessionEvpnWithdrawReachesEnvelope is
// TestSessionVpn4WithdrawReachesEnvelope's EVPN counterpart.
// Mutation-proved: replacing `re.EvpnWithdrawn = evpnProto(u.EvpnWithdrawn)`
// with a no-op left the full suite passing before this test existed.
func TestSessionEvpnWithdrawReachesEnvelope(t *testing.T) {
	val := []byte{0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01} // RD 65000:1
	val = append(val, 0x00, 0x00, 0x00, 0x64)                     // Ethernet Tag 100
	val = append(val, 32)                                         // IP length in bits
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)                     // Originating IP 10.0.0.11
	nlri := append([]byte{3, byte(len(val))}, val...)             // route type 3

	v := []byte{0x00, 0x19, 0x46} // AFI 25, SAFI 70 (EVPN) -- no next hop for MP_UNREACH
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPUnreach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.EvpnWithdrawn) != 1 {
		t.Fatalf("evpn_withdrawn = %+v", r.EvpnWithdrawn)
	}
	got := r.EvpnWithdrawn[0]
	if got.RouteType != 3 || got.Rd != "65000:1" || got.EthernetTag != 100 || got.OriginatingIp != "10.0.0.11" {
		t.Fatalf("evpn withdrawn = %+v", got)
	}
	if len(r.RawUnreach) != 0 {
		t.Fatalf("clean typed parse must carry no raw bytes, got %d", len(r.RawUnreach))
	}
	if ev.Subject != "vantage.v1.route.evpn."+routerTok+"."+peerTok {
		t.Fatalf("subject = %q", ev.Subject)
	}
}

// TestSessionEvpnType2ReachesEnvelope pins that a MAC/IP Advertisement
// (route type 2) exercises Mac, Ip, Esi and Labels -- four of the eight
// evpnProto field assignments TestSessionEvpnTypedReachesEnvelope's
// type-3 fixture cannot touch (type 3 has no MAC, IP, ESI, or label fields at
// all). Fixture from bgp/evpn_test.go's TestParseEvpnType2.
func TestSessionEvpnType2ReachesEnvelope(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01) // RD 65000:1
	val = append(val, make([]byte, 10)...)                            // ESI all-zero
	val = append(val, 0x00, 0x00, 0x00, 0x00)                         // Ethernet Tag 0
	val = append(val, 48)                                             // MAC length in bits
	val = append(val, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)             // MAC
	val = append(val, 32)                                             // IP length in bits
	val = append(val, 0x0A, 0x01, 0x01, 0x01)                         // 10.1.1.1
	val = append(val, 0x00, 0x27, 0x74)                               // VNI 10100 (raw 24-bit)
	nlri := append([]byte{2, byte(len(val))}, val...)                 // route type 2

	v := []byte{0x00, 0x19, 0x46, 0x04}   // AFI 25, SAFI 70 (EVPN), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // reserved
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPReach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.EvpnAnnounced) != 1 {
		t.Fatalf("evpn_announced = %+v", r.EvpnAnnounced)
	}
	got := r.EvpnAnnounced[0]
	if got.Mac != "aa:bb:cc:dd:ee:ff" || got.Ip != "10.1.1.1" ||
		got.Esi != "00000000000000000000" || len(got.Labels) != 1 || got.Labels[0] != 10100 {
		t.Fatalf("evpn type 2 route = %+v", got)
	}
}

// TestSessionEvpnType5ReachesEnvelope pins the remaining fields
// TestSessionEvpnType2ReachesEnvelope's route type doesn't reach: an IP
// Prefix route (route type 5) exercises GatewayIp and Prefix, the two
// fields no other typed session-layer EVPN test reaches. It also kills the
// "delete the r.Prefix.IsValid() block entirely" mutant, together with
// TestSessionEvpnTypedReachesEnvelope's Prefix == "" assertion on a
// type-3 route: deleting the block would leave Prefix unset here too,
// where it must be "192.0.2.0/24". Fixture from bgp/evpn_test.go's
// TestParseEvpnType5.
func TestSessionEvpnType5ReachesEnvelope(t *testing.T) {
	val := []byte{}
	val = append(val, 0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x02) // RD 65000:2
	val = append(val, make([]byte, 10)...)                            // ESI
	val = append(val, 0x00, 0x00, 0x00, 0x00)                         // Ethernet Tag
	val = append(val, 24)                                             // prefix length bits
	val = append(val, 0xC0, 0x00, 0x02, 0x00)                         // 192.0.2.0 (always 4 bytes for v4)
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)                         // gateway 10.0.0.11
	val = append(val, 0x00, 0x27, 0x74)                               // VNI
	nlri := append([]byte{5, byte(len(val))}, val...)                 // route type 5

	v := []byte{0x00, 0x19, 0x46, 0x04}   // AFI 25, SAFI 70 (EVPN), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // reserved
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPReach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.EvpnAnnounced) != 1 {
		t.Fatalf("evpn_announced = %+v", r.EvpnAnnounced)
	}
	got := r.EvpnAnnounced[0]
	if got.Prefix != "192.0.2.0/24" || got.GatewayIp != "10.0.0.11" {
		t.Fatalf("evpn type 5 route = %+v", got)
	}
}

// TestSessionEvpnUndecodedRouteTypeRawReachesEnvelope pins the last field
// TestSessionEvpnType2ReachesEnvelope and TestSessionEvpnType5ReachesEnvelope
// don't reach, Raw: a route type not decoded (type 4) must still
// reach the envelope, tagged, with its raw bytes intact. Fixture from
// bgp/evpn_test.go's TestParseEvpnUntypedCarriedRaw.
func TestSessionEvpnUndecodedRouteTypeRawReachesEnvelope(t *testing.T) {
	val := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	nlri := append([]byte{4, byte(len(val))}, val...) // route type 4: this decoder does not type it

	v := []byte{0x00, 0x19, 0x46, 0x04}   // AFI 25, SAFI 70 (EVPN), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // reserved
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPReach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.EvpnAnnounced) != 1 {
		t.Fatalf("evpn_announced = %+v", r.EvpnAnnounced)
	}
	got := r.EvpnAnnounced[0]
	if got.RouteType != 4 || string(got.Raw) != string(val) {
		t.Fatalf("evpn undecoded route = %+v", got)
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_NLRI_UNTYPED) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionVpn4AddPathReachesEnvelope pins that vpnProto's PathId
// is unfalsifiable without an add-path vpn4 fixture, since no other
// session-layer test negotiates ADD-PATH receive for AFI 1/SAFI 128. The
// router's own OPEN must advertise the Receive bit and the peer's OPEN the
// Send bit for that family (bgp.Merge's doc comment); AppendOpen emits both
// bits for every family in Caps.AddPathRecv, so passing the same Caps as
// both routerCaps and peerCaps negotiates it in both directions at once.
func TestSessionVpn4AddPathReachesEnvelope(t *testing.T) {
	s := newTestSession()
	vpnAddPathCaps := bgp.Caps{
		MP:          map[bgp.Family]bool{bgp.FamilyVPNv4: true},
		AddPathRecv: map[bgp.Family]bool{bgp.FamilyVPNv4: true},
	}
	vpnAddPathCaps.FourByteAS = true
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, vpnAddPathCaps, vpnAddPathCaps, 65000, 65001)))

	nlri := []byte{
		0x70,
		0x05, 0xDC, 0x11,
		0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x64,
		0x0A, 0x00, 0x00,
	}
	apNLRI := append([]byte{0x00, 0x00, 0x00, 0x07}, nlri...) // add-path path ID 7

	v := []byte{0x00, 0x01, 0x80, 0x0C} // AFI 1, SAFI 128, next-hop-length 12
	v = append(v, make([]byte, 8)...)   // the RD, always zero
	v = append(v, 0x0A, 0x00, 0x00, 0x09)
	v = append(v, 0x00) // reserved
	v = append(v, apNLRI...)

	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPReach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.VpnAnnounced) != 1 {
		t.Fatalf("vpn_announced = %+v", r.VpnAnnounced)
	}
	got := r.VpnAnnounced[0]
	if got.PathId != 7 || got.Prefix != "10.0.0.0/24" {
		t.Fatalf("vpn announced = %+v", got)
	}
}

// TestSessionEvpnAddPathReachesEnvelope pins the remaining field:
// evpnProto's own PathId (distinct from vpnProto's) is not exercised by the
// type-2/type-3/type-5 fixtures above, since none of them
// negotiate ADD-PATH for AFI 25/SAFI 70. Mirrors
// TestSessionVpn4AddPathReachesEnvelope's caps setup for the EVPN family.
func TestSessionEvpnAddPathReachesEnvelope(t *testing.T) {
	s := newTestSession()
	evpnAddPathCaps := bgp.Caps{
		MP:          map[bgp.Family]bool{bgp.FamilyEVPN: true},
		AddPathRecv: map[bgp.Family]bool{bgp.FamilyEVPN: true},
	}
	evpnAddPathCaps.FourByteAS = true
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, evpnAddPathCaps, evpnAddPathCaps, 65000, 65001)))

	val := []byte{0x00, 0x00, 0xFD, 0xE8, 0x00, 0x00, 0x00, 0x01} // RD 65000:1
	val = append(val, 0x00, 0x00, 0x00, 0x64)                     // Ethernet Tag 100
	val = append(val, 32)                                         // IP length in bits
	val = append(val, 0x0A, 0x00, 0x00, 0x0B)                     // Originating IP 10.0.0.11
	entry := append([]byte{3, byte(len(val))}, val...)            // route type 3 entry
	apNLRI := append([]byte{0x00, 0x00, 0x00, 0x09}, entry...)    // add-path path ID 9

	v := []byte{0x00, 0x19, 0x46, 0x04}   // AFI 25, SAFI 70 (EVPN), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09) // 10.0.0.9
	v = append(v, 0x00)                   // reserved
	v = append(v, apNLRI...)

	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPReach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.EvpnAnnounced) != 1 {
		t.Fatalf("evpn_announced = %+v", r.EvpnAnnounced)
	}
	got := r.EvpnAnnounced[0]
	if got.PathId != 9 || got.OriginatingIp != "10.0.0.11" {
		t.Fatalf("evpn announced = %+v", got)
	}
}

// TestSessionLU4TypedReachesEnvelope pins that lu4 (AFI 1/SAFI 4) shares
// vpnProto with vpn4, but has no session-layer coverage of its own, and its
// one differentiator from vpn4 -- RD == "" -- is otherwise unexercised at
// this layer. Fixture from bgp/update_test.go's
// TestParseUpdateLU4Typed.
func TestSessionLU4TypedReachesEnvelope(t *testing.T) {
	nlri := []byte{0x30, 0x00, 0x06, 0x41, 0xC0, 0x00, 0x02} // RFC 8277: label 100, prefix 192.0.2.0/24
	v := []byte{0x00, 0x01, 0x04, 0x04}                      // AFI 1, SAFI 4 (lu4), next-hop-length 4
	v = append(v, 0x0A, 0x00, 0x00, 0x09)                    // 10.0.0.9
	v = append(v, 0x00)                                      // reserved
	v = append(v, nlri...)

	s := newTestSession()
	body := bgpUpdateWithAttr(t, 0x80, bgpAttrMPReach, v)
	ev := mustOne(t, s.Handle(bmp.Msg{
		Type:    bmp.TypeRouteMonitoring,
		Payload: append(peerHdr().Append(nil), body...),
	}))

	r := ev.Env.GetRoute()
	if r == nil {
		t.Fatalf("no route payload: %+v", ev.Env)
	}
	if len(r.VpnAnnounced) != 1 {
		t.Fatalf("vpn_announced = %+v", r.VpnAnnounced)
	}
	got := r.VpnAnnounced[0]
	if got.Prefix != "192.0.2.0/24" || got.Rd != "" || len(got.Labels) != 1 || got.Labels[0] != 100 {
		t.Fatalf("lu4 announced = %+v", got)
	}
	if ev.Subject != "vantage.v1.route.lu4."+routerTok+"."+peerTok {
		t.Fatalf("subject = %q", ev.Subject)
	}
}

// TestQuirkOverridesTakeEffect pins the ops escape hatch. The
// override path was fully plumbed -- config schema, validation, quirkIDs,
// Resolve -- but the accommodation sites never consulted Set.Active, so
// disable_quirks was accepted, validated, documented in the README, and did
// nothing. A knob that reports success and has no effect is worse than one
// that errors.
func TestQuirkOverridesTakeEffect(t *testing.T) {
	ph := peerHdr()
	ph.Timestamp = time.Time{} // triggers QK_TS_ZERO

	t.Run("QK_TS_ZERO honored by default", func(t *testing.T) {
		s := newTestSession()
		ev := mustOne(t, s.Handle(mustMsg(t, bmptest.Stats(ph, map[uint32]uint64{1: 1}))))
		if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK) {
			t.Fatalf("want the accommodation by default, flags=%v", ev.Env.ParseFlags)
		}
		if !ev.Env.TsRouter.AsTime().Equal(t0) {
			t.Fatalf("ts_router=%v, want collector time", ev.Env.TsRouter.AsTime())
		}
	})

	t.Run("QK_TS_ZERO disabled per-router", func(t *testing.T) {
		s := NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 111,
			func() time.Time { return t0 }, Overrides{Disable: []quirk.ID{quirk.QkTSZero}})
		ev := mustOne(t, s.Handle(mustMsg(t, bmptest.Stats(ph, map[uint32]uint64{1: 1}))))
		if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK) {
			t.Fatalf("disable_quirks must suppress the flag, flags=%v", ev.Env.ParseFlags)
		}
		if !ev.Env.TsRouter.AsTime().IsZero() {
			t.Fatalf("disabled means trust the router's zero, got ts_router=%v", ev.Env.TsRouter.AsTime())
		}
	})

	t.Run("QK_CAPS_MISSING disabled per-router", func(t *testing.T) {
		s := NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 111,
			func() time.Time { return t0 }, Overrides{Disable: []quirk.ID{quirk.QkCapsMissing}})
		ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), bgp.BuildUpdate{
			Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
			NextHop:   netip.MustParseAddr("10.0.0.9"),
		}))))
		if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
			t.Fatalf("disable_quirks must suppress the flag, flags=%v", ev.Env.ParseFlags)
		}
	})
}

// TestSessionAdjRIBOutIsASeparatePeer pins RFC 8671 end to end. A router
// mirroring a neighbor's adj-RIB-out sends the same peer address with the
// O flag set; those are routes it *sent*, not routes it received, so they
// must not land on the adj-RIB-in subject, must not share that peer's
// sequence counter, and must be identifiable on the envelope itself.
//
// Capabilities are the one thing the split does NOT separate: they are a
// property of the OPEN exchange, not of the RIB view, so the adj-RIB-out
// stream takes them from the Peer Up already on record for this BGP session
// (transposed for its own direction of travel -- see
// TestSessionAdjRIBOutBorrowsTheOutDirectionMerge) and CAPS_MISSING does not
// fire. Flagging it would be announcing an unknown that is not unknown, and
// parsing with an empty Caps would turn every 4-byte-encoded AS_PATH on the
// feed into a treat-as-withdraw.
func TestSessionAdjRIBOutIsASeparatePeer(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2")))
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	upd := bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		ASPath:    []uint32{65001}, FourByteAS: true,
		NextHop: netip.MustParseAddr("10.0.0.9"),
	}
	in := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), upd))))

	out := peerHdr()
	out.Flags |= 0x10 // RFC 8671 O flag: adj-RIB-out
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(out, upd))))

	if ev.Subject == in.Subject {
		t.Fatalf("adj-RIB-out shares the adj-RIB-in subject %q", ev.Subject)
	}
	if want := "vantage.v1.route.ipv4u." + routerTok + "." + peerTok + "-dout"; ev.Subject != want {
		t.Errorf("subject = %q, want %q", ev.Subject, want)
	}
	if !ev.Env.Peer.AdjRibOut {
		t.Errorf("envelope peer does not carry adj_rib_out")
	}
	if in.Env.Peer.AdjRibOut {
		t.Errorf("adj-RIB-in envelope carries adj_rib_out")
	}
	if ev.Env.Peer.Ip != "10.0.0.9" {
		t.Errorf("peer ip = %q, want the neighbour address unchanged", ev.Env.Peer.Ip)
	}
	if ev.Env.Seq != 1 {
		t.Errorf("seq = %d, want 1: the adj-RIB-out stream has its own counter", ev.Env.Seq)
	}
	if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Errorf("adj-RIB-out must borrow its BGP session's capabilities, not flag them missing: %v", ev.Env.ParseFlags)
	}
	if len(ev.Env.GetRoute().GetAnnounced()) != 1 {
		t.Errorf("announced = %v; an empty Caps would have made the 4-byte AS_PATH a treat-as-withdraw", ev.Env.GetRoute())
	}
	if hasFlag(in.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Errorf("adj-RIB-in had a Peer Up on record: %v", in.Env.ParseFlags)
	}
}

// asymmetricOpen builds a complete BGP OPEN whose only capability is RFC
// 7911 ADD-PATH for ipv4 unicast with the given Send/Receive value.
// bgp.AppendOpen cannot express this -- it always emits value 3 (both bits)
// -- and the single-bit form is exactly what a route reflector and its
// client negotiate, so these tests hand-build the message the way
// caps_test.go's rawOpen does. With no MP capability present at all,
// bgp.ParseOpen applies RFC 4760's implicit ipv4-unicast default, so the
// merged MP set still contains ipv4 unicast; the 4-octet AS capability is
// deliberately absent, so AS_PATHs on these sessions are 2-byte encoded.
func asymmetricOpen(asn uint16, bgpID [4]byte, sendRecv byte) []byte {
	caps := []byte{69, 4, 0x00, 0x01, 0x01, sendRecv} // code 69, len 4, AFI 1, SAFI 1
	body := []byte{4, byte(asn >> 8), byte(asn), 0x00, 0xb4}
	body = append(body, bgpID[:]...)
	body = append(body, byte(len(caps)+2), 2, byte(len(caps)))
	body = append(body, caps...)
	msg := bytes.Repeat([]byte{0xff}, 16)
	msg = binary.BigEndian.AppendUint16(msg, uint16(19+len(body)))
	return append(append(msg, 1), body...)
}

// asymmetricPeerUp is bmptest.PeerUp with hand-built OPENs: the monitored
// router advertises ADD-PATH send-only for ipv4 unicast, the peer
// receive-only. Nothing the peer sends the router carries a Path
// Identifier; everything the router sends the peer does.
func asymmetricPeerUp(ph bmp.PeerHeader) bmp.Msg {
	p := ph.Append(nil)
	p = append(p, make([]byte, 16)...) // local address
	p = binary.BigEndian.AppendUint16(p, 179)
	p = binary.BigEndian.AppendUint16(p, 33001)
	p = append(p, asymmetricOpen(65000, [4]byte{1, 1, 1, 1}, 0x2)...) // router: Send
	p = append(p, asymmetricOpen(65001, [4]byte{2, 2, 2, 2}, 0x1)...) // peer: Receive
	return bmp.Msg{Type: bmp.TypePeerUp, Payload: p}
}

// addPathUpdate is a classic-v4 UPDATE announcing 192.0.2.0/24 with RFC 7911
// path ID 7 and a 2-byte-encoded AS_PATH (these sessions negotiate no
// 4-octet AS capability).
func addPathUpdate() bgp.BuildUpdate {
	return bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24"), PathID: 7}},
		AddPath:   true,
		ASPath:    []uint32{65001},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}
}

// TestSessionPeerUpNegotiatesPerRIBDirection is the session-layer half of the
// RFC 8671 direction rule: the same OPEN pair means different things for the
// two RIBs it describes. bgp.Merge answers "do the UPDATEs traveling from
// the sender to the receiver carry a Path Identifier?", so an O-flagged Peer
// Up -- whose route monitoring mirrors what the router *sent* -- must pair
// the peer's receive bit with the router's send bit, the transposition of
// what an adj-RIB-in Peer Up computes.
//
// With the asymmetric config here the two answers differ, so the merge is
// visible on the Peer Up event's own Capabilities: absent for adj-RIB-in,
// present for adj-RIB-out. A collector that called bgp.Merge(sent, recv)
// unconditionally would report both as absent.
func TestSessionPeerUpNegotiatesPerRIBDirection(t *testing.T) {
	s := newTestSession()
	in := mustOne(t, s.Handle(asymmetricPeerUp(peerHdr())))

	outHdr := peerHdr()
	outHdr.Flags |= 0x10 // RFC 8671 O flag
	out := mustOne(t, s.Handle(asymmetricPeerUp(outHdr)))

	if fams := in.Env.GetPeerEvent().GetCaps().GetAddpathFamilies(); len(fams) != 0 {
		t.Errorf("adj-RIB-in caps report addpath families %v; the peer never advertised the send bit", fams)
	}
	fams := out.Env.GetPeerEvent().GetCaps().GetAddpathFamilies()
	if len(fams) != 1 || fams[0].GetAfi() != 1 || fams[0].GetSafi() != 1 {
		t.Fatalf("adj-RIB-out caps addpath families = %v, want ipv4 unicast: the router advertised Send and the peer Receive", fams)
	}
	// The symmetric terms must survive the transposition.
	if len(out.Env.GetPeerEvent().GetCaps().GetMpFamilies()) != 1 {
		t.Errorf("adj-RIB-out mp families = %v, want ipv4 unicast (RFC 4760 implicit default on both OPENs)",
			out.Env.GetPeerEvent().GetCaps().GetMpFamilies())
	}
}

// TestSessionAdjRIBOutBorrowsTheOutDirectionMerge is the trap this fix exists
// to avoid, stated as a test. The adj-RIB-out stream has no Peer Up of its
// own, so it borrows its BGP session's OPEN exchange -- but it must borrow
// the merge computed for *its* direction of travel, not the adj-RIB-in one
// sitting right there on the base peer.
//
// The UPDATE carries a path ID. Three outcomes distinguish the three
// possible implementations: with the out-direction merge it decodes as one
// prefix with path ID 7 and no heuristic flag; with the in-direction merge
// borrowed by mistake, capabilities are "known" and add-path off, so the
// four path-ID bytes are read as prefixes and the announcement is garbage;
// with no borrowing at all, the structural heuristic recovers the path ID
// but says so with PARSE_FLAG_ADDPATH_HEURISTIC and CAPS_MISSING.
func TestSessionAdjRIBOutBorrowsTheOutDirectionMerge(t *testing.T) {
	s := newTestSession()
	s.Handle(asymmetricPeerUp(peerHdr()))

	out := peerHdr()
	out.Flags |= 0x10 // RFC 8671 O flag: adj-RIB-out, no Peer Up of its own
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(out, addPathUpdate()))))

	r := ev.Env.GetRoute()
	if len(r.GetAnnounced()) != 1 {
		t.Fatalf("announced = %+v, want exactly one prefix: the in-direction merge would read the path ID as NLRI", r.GetAnnounced())
	}
	got := r.GetAnnounced()[0]
	if got.GetPrefix() != "192.0.2.0/24" || got.GetPathId() != 7 {
		t.Errorf("announced = %+v, want 192.0.2.0/24 path id 7", got)
	}
	if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC) {
		t.Errorf("path ID was guessed structurally, not negotiated: %v", ev.Env.ParseFlags)
	}
	if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Errorf("the BGP session's capabilities are on record: %v", ev.Env.ParseFlags)
	}

	// The converse, on the same session and the same bytes: the adj-RIB-in
	// stream keeps the in-direction merge, under which those four leading
	// bytes are NLRI rather than a path ID -- and read that way they are
	// malformed, so the message lands on the raw stream as a parse failure.
	// The two directions demonstrably disagree, and the stream above got the
	// one that belongs to it.
	inEv := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(peerHdr(), addPathUpdate()))))
	if inEv.Env.GetRoute() != nil {
		t.Errorf("adj-RIB-in decoded add-path NLRI, so it inherited the out-direction merge: %+v", inEv.Env.GetRoute())
	}
	if inEv.Env.GetRaw().GetParseError() == "" {
		t.Errorf("adj-RIB-in event = %+v, want a parse failure: those bytes are not NLRI in that direction", inEv.Env)
	}
}

// TestSessionCapsMissingWhenNeitherViewHasAPeerUp is the floor under the
// borrowing above: QK_CAPS_MISSING still fires when no RIB view of this BGP
// session has a Peer Up on record, which is the condition the flag actually
// names.
func TestSessionCapsMissingWhenNeitherViewHasAPeerUp(t *testing.T) {
	s := newTestSession()
	out := peerHdr()
	out.Flags |= 0x10
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(out, bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}))))
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Errorf("no Peer Up on any RIB view of this peer must flag CAPS_MISSING: %v", ev.Env.ParseFlags)
	}
}

// TestSessionPeerDownEndsBorrowing pins that the shared OPEN exchange dies
// with the session it describes: after a Peer Down, an adj-RIB-out stream
// has nothing left to borrow and says so, rather than parsing with the
// capabilities of a session that has ended.
func TestSessionPeerDownEndsBorrowing(t *testing.T) {
	s := newTestSession()
	s.Handle(asymmetricPeerUp(peerHdr()))
	s.Handle(mustMsg(t, bmptest.PeerDown(peerHdr(), 2, []byte{0, 0})))

	out := peerHdr()
	out.Flags |= 0x10
	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(out, bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	}))))
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Errorf("a Peer Down must end borrowing from that session's OPEN exchange: %v", ev.Env.ParseFlags)
	}
}

// TestSessionAdjRIBOutPeerUpFeedsItsOwnStream completes the RFC 8671
// lifecycle: once the O-flagged Peer Up arrives, its capabilities apply to
// the adj-RIB-out stream and CAPS_MISSING stops.
func TestSessionAdjRIBOutPeerUpFeedsItsOwnStream(t *testing.T) {
	s := newTestSession()
	out := peerHdr()
	out.Flags |= 0x10
	s.Handle(mustMsg(t, bmptest.PeerUp(out, netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	ev := mustOne(t, s.Handle(mustMsg(t, bmptest.RouteMonitoring(out, bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		ASPath:    []uint32{65001}, FourByteAS: true,
		NextHop: netip.MustParseAddr("10.0.0.9"),
	}))))
	if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING) {
		t.Fatalf("O-flagged Peer Up must supply the adj-RIB-out stream's caps: %v", ev.Env.ParseFlags)
	}
	if !ev.Env.Peer.AdjRibOut {
		t.Fatal("envelope peer does not carry adj_rib_out")
	}
}

// TestFoldTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn is the covering
// test for foldTreatAsWithdraw's BGP-LS half: LsNodes/LsLinks
// (MP_REACH-announced) must fold into LsNodesWithdrawn/LsLinksWithdrawn
// under RFC 7606 treat-as-withdraw, the same as the already-covered
// Vpn/Evpn Announced/Withdrawn pairs (see
// TestSessionTreatAsWithdrawFoldsVpnAnnouncedIntoWithdrawn and its EVPN
// counterpart).
//
// This exercises foldTreatAsWithdraw directly against a *bgp.Update, the
// same way every sibling fold test in this file does, rather than through
// Session.Handle -- that isolates the fold logic itself from everything
// else Handle does on the way to an envelope. LsEvent carries
// LsNodes/LsLinks, so the BGP-LS half of this fold is observable
// black-box too: TestSessionTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn
// below is that black-box test.
func TestFoldTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn(t *testing.T) {
	node := bgp.LsNodeNLRI{Protocol: 3, Identifier: 100,
		Local: bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 1}}}
	link := bgp.LsLinkNLRI{Protocol: 3, Identifier: 100,
		Local:  bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 2}},
		Remote: bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 5}}}
	u := &bgp.Update{
		LsNodes: []bgp.LsNodeNLRI{node},
		LsLinks: []bgp.LsLinkNLRI{link},
	}

	foldTreatAsWithdraw(u)

	if len(u.LsNodes) != 0 || len(u.LsLinks) != 0 {
		t.Fatalf("want LsNodes/LsLinks cleared under treat-as-withdraw, got %+v / %+v", u.LsNodes, u.LsLinks)
	}
	if len(u.LsNodesWithdrawn) != 1 || u.LsNodesWithdrawn[0].Local.ASN != 65000 {
		t.Fatalf("want the announced node folded into LsNodesWithdrawn, got %+v", u.LsNodesWithdrawn)
	}
	if len(u.LsLinksWithdrawn) != 1 || u.LsLinksWithdrawn[0].Remote.ASN != 65000 {
		t.Fatalf("want the announced link folded into LsLinksWithdrawn, got %+v", u.LsLinksWithdrawn)
	}
}

// lsTLVBytes appends one Type(2) Length(2) Value TLV, mirroring BGP-LS's
// wire shape (RFC 9552 §5.2). A small local builder rather than hex string
// literals, so the test below can construct a minimal but structurally real
// Node/Link NLRI without depending on bgp package internals (its own
// equivalent, bgp.lsTLVBytes, is unexported).
func lsTLVBytes(b []byte, typ uint16, val []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, typ)
	b = binary.BigEndian.AppendUint16(b, uint16(len(val)))
	return append(b, val...)
}

// lsNodeDescriptorBytes builds a minimal Node Descriptor (256/257 TLV
// value): AS (512) + IGP Router-ID (515), the two sub-TLVs decodeLsNode/
// decodeLsLink actually require for a node to have an identity.
func lsNodeDescriptorBytes(asn uint32, routerID []byte) []byte {
	var b []byte
	b = lsTLVBytes(b, 512, binary.BigEndian.AppendUint32(nil, asn))
	b = lsTLVBytes(b, 515, routerID)
	return b
}

// lsNodeNLRIBytes builds one Node NLRI (TLV type 1): Protocol(1)
// Identifier(8) then the Local Node Descriptor (256) TLV.
func lsNodeNLRIBytes(protocol uint8, identifier uint64, localDesc []byte) []byte {
	v := append([]byte{protocol}, binary.BigEndian.AppendUint64(nil, identifier)...)
	v = lsTLVBytes(v, 256, localDesc)
	return lsTLVBytes(nil, 1, v)
}

// lsLinkNLRIBytes builds one Link NLRI (TLV type 2): Protocol(1)
// Identifier(8) then the Local (256) and Remote (257) Node Descriptor TLVs.
func lsLinkNLRIBytes(protocol uint8, identifier uint64, localDesc, remoteDesc []byte) []byte {
	v := append([]byte{protocol}, binary.BigEndian.AppendUint64(nil, identifier)...)
	v = lsTLVBytes(v, 256, localDesc)
	v = lsTLVBytes(v, 257, remoteDesc)
	return lsTLVBytes(nil, 2, v)
}

// TestSessionTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn is the
// black-box end-to-end test: the same RFC 7606 treat-as-withdraw scenario
// TestSessionTreatAsWithdrawFoldsVpnAnnouncedIntoWithdrawn and its EVPN
// counterpart already prove through Session.Handle, but for BGP-LS -- a
// malformed non-LS attribute (ORIGIN) alongside a clean BGP-LS MP_REACH,
// asserted against the envelope Session.Handle actually returns rather than
// against foldTreatAsWithdraw called directly. LsEvent previously carried
// no nodes/links, so this test could not have been written; the comment
// above TestFoldTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn had gone stale
// claiming it still couldn't, which is the gap this test closes.
func TestSessionTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	attrs := buildAttr(0x40, 1, []byte{0, 0}) // malformed ORIGIN: RFC 7606 §7.1 wants 1 byte

	localDesc := lsNodeDescriptorBytes(65000, []byte{10, 255, 0, 1})
	remoteDesc := lsNodeDescriptorBytes(65000, []byte{10, 255, 0, 2})
	nlri := append(lsNodeNLRIBytes(3, 100, localDesc), lsLinkNLRIBytes(3, 100, localDesc, remoteDesc)...)
	v := mpReachVal(16388, 71, []byte{10, 0, 0, 9}, nlri) // AFI 16388 / SAFI 71 (BGP-LS, RFC 9552 §4)
	attrs = append(attrs, buildAttr(0x80, bgpAttrMPReach, v)...)

	body := updateBody(nil, attrs, nil)
	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body)))
	ls := ev.Env.GetLs()
	if ls == nil {
		t.Fatalf("want LsEvent, got %+v", ev.Env.Payload)
	}
	if len(ls.GetNodes()) != 1 || !ls.GetNodes()[0].GetIsWithdraw() {
		t.Fatalf("nodes = %+v, want exactly 1 with is_withdraw=true", ls.GetNodes())
	}
	if len(ls.GetLinks()) != 1 || !ls.GetLinks()[0].GetIsWithdraw() {
		t.Fatalf("links = %+v, want exactly 1 with is_withdraw=true", ls.GetLinks())
	}
	if !hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TREAT_AS_WITHDRAW_7606) {
		t.Fatalf("flags=%v", ev.Env.ParseFlags)
	}
}

// TestSessionPublishesTypedLinkState replays a real BGP-LS capture and
// checks that the LsEvent envelope carries typed nodes/links, not just the
// raw MP_REACH/MP_UNREACH bytes RawReach/RawUnreach already covered.
// Corpus-driven (like TestCorpusReplay) rather than hand-built,
// since a hand-built NLRI would only prove the construction round-trips
// against itself.
//
// Beyond the node/link counts, this also checks two real decoded VALUES
// against bytes actually captured off the wire: xr-rr1's node SRGB
// (16000+8000, see bgp.nodeAttrHex's doc comment in bgp/linkstate_test.go)
// and one link's adjacency SID (24001, bgp.linkAttrHex). A count only proves
// the construction loop ran; a specific value proves lsNodeMessage/
// lsLinkMessage actually copied the right bgp.LsAttrs fields into the wire
// message rather than, say, swapping SRGB and SRLB or dropping AdjSids.
func TestSessionPublishesTypedLinkState(t *testing.T) {
	evs := replayCorpus(t, "../bgp/testdata/corpus/iosxr/xrd-26.1.1-p-linkstate.bmpcap")
	var nodes []*vantagev1.LsNode
	var links []*vantagev1.LsLink
	for _, ev := range evs {
		ls := ev.Env.GetLs()
		if ls == nil {
			continue
		}
		nodes = append(nodes, ls.GetNodes()...)
		links = append(links, ls.GetLinks()...)
	}
	if len(nodes) == 0 || len(links) == 0 {
		t.Fatalf("typed LS envelopes: %d nodes, %d links; want both non-zero", len(nodes), len(links))
	}

	var gotSRGB bool
	for _, n := range nodes {
		if n.GetSrgbBase() == 16000 && n.GetSrgbSize() == 8000 {
			gotSRGB = true
		}
	}
	if !gotSRGB {
		t.Errorf("no node with SRGB 16000+8000 (xr-rr1's real captured value) among %d nodes", len(nodes))
	}

	var gotAdjSID bool
	for _, l := range links {
		for _, sid := range l.GetAdjSids() {
			if sid.GetSid() == 24001 {
				gotAdjSID = true
			}
		}
	}
	if !gotAdjSID {
		t.Errorf("no link with adjacency SID 24001 (the real captured value) among %d links", len(links))
	}

	// XRd 26.1.1 sends TLV 258 (Link Local/Remote
	// Identifiers) inside the BGP-LS Attribute, not the Link NLRI -- so this
	// value can only reach the envelope if lsLinkMessage's fallback to
	// bgp.LsAttrs.HasLinkID actually runs, end to end, through the real
	// collector session (not just DecodeLsAttrs in isolation, which
	// bgp.TestDecodeLsAttrsLink already covers). Confirmed against this
	// fixture directly: the link between 10.255.0.2 and 10.255.0.5 carries
	// local=4 remote=3.
	var gotLinkID bool
	for _, l := range links {
		if l.GetLinkLocalId() == 4 && l.GetLinkRemoteId() == 3 {
			gotLinkID = true
		}
	}
	if !gotLinkID {
		t.Errorf("no link with link_local_id=4 link_remote_id=3 (the real captured attribute-side value) among %d links", len(links))
	}
}

// TestBuildLsEventMarksWithdrawnDistinctFromAnnounced covers the exact
// mechanism: a node/link reaching MP_UNREACH must land in the envelope
// with is_withdraw=true, and one reaching MP_REACH in the same UPDATE
// must land with is_withdraw=false. The
// committed corpus fixture (TestSessionPublishesTypedLinkState) has no
// withdrawals, so nothing before this test could catch is_withdraw being
// inverted or dropped for the collector's typed output. Exercises
// buildLsEvent directly, the same way
// TestFoldTreatAsWithdrawFoldsLsAnnouncedIntoWithdrawn exercises
// foldTreatAsWithdraw directly, since there is no committed capture with a
// real BGP-LS withdrawal to replay.
func TestBuildLsEventMarksWithdrawnDistinctFromAnnounced(t *testing.T) {
	announcedNode := bgp.LsNodeNLRI{Protocol: 3, Identifier: 100,
		Local: bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 1}}}
	withdrawnNode := bgp.LsNodeNLRI{Protocol: 3, Identifier: 100,
		Local: bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 9}}}
	announcedLink := bgp.LsLinkNLRI{Protocol: 3, Identifier: 100,
		Local:  bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 2}},
		Remote: bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 5}}}
	withdrawnLink := bgp.LsLinkNLRI{Protocol: 3, Identifier: 100,
		Local:  bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 6}},
		Remote: bgp.LsNodeDescriptor{ASN: 65000, RouterID: []byte{10, 255, 0, 7}}}

	u := &bgp.Update{
		LsNodes:          []bgp.LsNodeNLRI{announcedNode},
		LsNodesWithdrawn: []bgp.LsNodeNLRI{withdrawnNode},
		LsLinks:          []bgp.LsLinkNLRI{announcedLink},
		LsLinksWithdrawn: []bgp.LsLinkNLRI{withdrawnLink},
	}

	ls := buildLsEvent(&vantagev1.Family{}, u)

	if len(ls.GetNodes()) != 2 || len(ls.GetLinks()) != 2 {
		t.Fatalf("got %d nodes, %d links; want 2 and 2", len(ls.GetNodes()), len(ls.GetLinks()))
	}

	// Found by their distinguishing router-id byte rather than by loop
	// position: withdrawn-before-announced is an implementation detail of
	// buildLsEvent, and asserting on ordering would couple this test to
	// that detail instead of to is_withdraw itself.
	var announcedNodeMsg, withdrawnNodeMsg *vantagev1.LsNode
	for _, n := range ls.GetNodes() {
		switch n.GetLocal().GetRouterId()[3] {
		case 1:
			announcedNodeMsg = n
		case 9:
			withdrawnNodeMsg = n
		}
	}
	if announcedNodeMsg == nil || withdrawnNodeMsg == nil {
		t.Fatalf("could not find both nodes by router-id in %+v", ls.GetNodes())
	}
	if announcedNodeMsg.GetIsWithdraw() {
		t.Error("announced node has is_withdraw=true, want false")
	}
	if !withdrawnNodeMsg.GetIsWithdraw() {
		t.Error("withdrawn node has is_withdraw=false, want true")
	}

	var announcedLinkMsg, withdrawnLinkMsg *vantagev1.LsLink
	for _, l := range ls.GetLinks() {
		switch l.GetLocal().GetRouterId()[3] {
		case 2:
			announcedLinkMsg = l
		case 6:
			withdrawnLinkMsg = l
		}
	}
	if announcedLinkMsg == nil || withdrawnLinkMsg == nil {
		t.Fatalf("could not find both links by router-id in %+v", ls.GetLinks())
	}
	if announcedLinkMsg.GetIsWithdraw() {
		t.Error("announced link has is_withdraw=true, want false")
	}
	if !withdrawnLinkMsg.GetIsWithdraw() {
		t.Error("withdrawn link has is_withdraw=false, want true")
	}
}

// initMsg builds a BMP Initiation carrying sysDescr and sysName, so the
// tests below exercise the real ParseInit path rather than reaching into
// Session's fields.
func initMsg(t *testing.T, sysDescr, sysName string) bmp.Msg {
	t.Helper()
	var payload []byte
	add := func(typ uint16, v string) {
		payload = append(payload, byte(typ>>8), byte(typ))
		payload = append(payload, byte(len(v)>>8), byte(len(v)))
		payload = append(payload, v...)
	}
	add(1, sysDescr) // RFC 7854 §4.3: sysDescr
	add(2, sysName)  // sysName
	return bmp.Msg{Type: bmp.TypeInitiation, Payload: payload}
}

// Operator config identifies a router whose banner cannot. XRd sends "26.1.1"
// as its entire sysDescr -- a bare version, no vendor token -- so before
// config supplied an identity every XR envelope carried an empty vendor and
// OS, and quirk.Entry.AppliesTo could never fire for it. Config supplies the
// identity, the banner still supplies the version.
func TestSessionAppliesConfiguredIdentityToAnUnidentifiableBanner(t *testing.T) {
	s := NewSession(netip.MustParseAddr("10.0.103.62"), "c1", 1,
		func() time.Time { return t0 },
		Overrides{Vendor: "cisco", OS: "iosxr"})
	s.Handle(initMsg(t, "26.1.1", "xr-pe1"))

	ri := s.profile.RouterInfo()
	if ri.GetVendor() != "cisco" || ri.GetOs() != "iosxr" {
		t.Errorf("RouterInfo vendor/os = %q/%q, want cisco/iosxr", ri.GetVendor(), ri.GetOs())
	}
	if ri.GetVersion() != "26.1.1" {
		t.Errorf("RouterInfo version = %q, want 26.1.1 from the banner", ri.GetVersion())
	}
	if ri.GetSysDescr() != "26.1.1" {
		t.Errorf("RouterInfo sys_descr = %q, want the raw banner preserved", ri.GetSysDescr())
	}
	// The whole point: with a vendor known and a version read, this router is
	// no longer flagged as unidentifiable on every single envelope.
	if s.quirks.Active(quirk.QkVersionUnparsed) {
		t.Error("QK_VERSION_UNPARSED still active for a fully identified router")
	}
}

// Config outranks the banner. An anchor is a regex guessing from text; an
// operator can see the device.
func TestSessionConfiguredIdentityOverridesTheBannerAnchor(t *testing.T) {
	s := NewSession(netip.MustParseAddr("10.0.103.74"), "c1", 1,
		func() time.Time { return t0 },
		Overrides{Vendor: "acme", OS: "acmeos"})
	s.Handle(initMsg(t, "Nexus9000 C9300v Chassis, Software Version 10.6(2)I9(1)", "nx-p3"))
	if got := s.profile.RouterInfo().GetVendor(); got != "acme" {
		t.Errorf("vendor = %q, want the configured acme to win over the Nexus anchor", got)
	}
}

// With no config for a router, the banner anchors still apply -- a zero-config
// deployment must not be worse off than before config existed.
func TestSessionWithoutConfiguredIdentityFallsBackToTheAnchor(t *testing.T) {
	s := NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 1,
		func() time.Time { return t0 }, Overrides{})
	s.Handle(initMsg(t, "FRRouting 10.3_git", "frr1"))
	ri := s.profile.RouterInfo()
	if ri.GetVendor() != "frr" || ri.GetVersion() != "10.3" {
		t.Errorf("RouterInfo vendor/version = %q/%q, want frr/10.3 from the anchor",
			ri.GetVendor(), ri.GetVersion())
	}
}

// A configured identity that contradicts a banner the collector could read is
// worth surfacing: one of the two is wrong, and silently believing either
// leaves an operator with no way to notice. Config still wins -- this only
// records that they disagreed.
func TestSessionRecordsAnIdentityConflict(t *testing.T) {
	s := NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 1,
		func() time.Time { return t0 },
		Overrides{Vendor: "cisco", OS: "iosxr"})
	s.Handle(initMsg(t, "FRRouting 10.3_git", "frr1"))
	if !s.IdentityConflict() {
		t.Error("a banner that says frr under a config that says cisco/iosxr must " +
			"be recorded as a conflict")
	}

	agree := NewSession(netip.MustParseAddr("10.0.0.2"), "c1", 2,
		func() time.Time { return t0 },
		Overrides{Vendor: "frr", OS: "frr"})
	agree.Handle(initMsg(t, "FRRouting 10.3_git", "frr1"))
	if agree.IdentityConflict() {
		t.Error("config and banner agreeing must not be a conflict")
	}

	// A banner with no identity in it does not contradict anything. This is
	// the common case for IOS-XR and must stay quiet, or the warning is noise
	// on exactly the fleet it was added for.
	silent := NewSession(netip.MustParseAddr("10.0.0.3"), "c1", 3,
		func() time.Time { return t0 },
		Overrides{Vendor: "cisco", OS: "iosxr"})
	silent.Handle(initMsg(t, "26.1.1", "xr-pe1"))
	if silent.IdentityConflict() {
		t.Error("a banner carrying no vendor cannot contradict the config")
	}
}

// The four tests below cover Session.Close: the synthetic PeerEvent the
// collector emits for peers it can still see when the BMP transport itself
// ends. Before it existed, KIND_DOWN was emitted only on a PeerDown message,
// so a collector whose TCP session dropped left its peers recorded up under
// what stays max(session_id) for that (collector, router) forever.

func TestSessionCloseEmitsViewLostForUpPeers(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	ev := mustOne(t, s.Close())
	pe := ev.Env.GetPeerEvent()
	if pe.GetKind() != vantagev1.PeerEvent_KIND_VIEW_LOST {
		t.Fatalf("kind = %v, want KIND_VIEW_LOST", pe.GetKind())
	}
	if ev.Subject != "vantage.v1.peer."+routerTok+"."+peerTok {
		t.Fatalf("subject = %s", ev.Subject)
	}
	// seq continues the peer's own counter (the Peer-Up above took 1), so
	// this event's msg-id collides with nothing the session already sent.
	if ev.Env.Seq != 2 || ev.MsgID != routerTok+"/"+peerTok+"/111/2" {
		t.Fatalf("seq=%d msgid=%s", ev.Env.Seq, ev.MsgID)
	}
	if ev.Env.GetPeer().GetIp() != "10.0.0.9" || ev.Env.GetPeer().GetAsn() != 65001 {
		t.Fatalf("peer identity not carried: %+v", ev.Env.GetPeer())
	}
	// The router said nothing, so there is no RFC 7854 sec 4.9 reason code
	// to report and none is invented.
	if pe.GetDownReason() != 0 || len(pe.GetDownData()) != 0 {
		t.Fatalf("down_reason=%d down_data=%v; a synthetic close has neither",
			pe.GetDownReason(), pe.GetDownData())
	}
}

func TestSessionCloseSaysNothingAboutAPeerAlreadyDown(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))
	s.Handle(mustMsg(t, bmptest.PeerDown(peerHdr(), 2, nil)))

	// The router already stated this peer's session was down, with a reason.
	// That is the peer's final state and a view-lost event would overwrite a
	// better answer with a worse one.
	if evs := s.Close(); len(evs) != 0 {
		t.Fatalf("want no events, got %+v", evs)
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	if evs := s.Close(); len(evs) != 1 {
		t.Fatalf("first Close: want 1 event, got %d", len(evs))
	}
	// handleConn closes the session from a defer; a second call must not
	// publish a second view-lost event under a fresh seq, which would look
	// like the peer was lost twice.
	if evs := s.Close(); len(evs) != 0 {
		t.Fatalf("second Close: want 0 events, got %+v", evs)
	}
}

func TestSessionCloseStampsCollectorTimeAndFlagsNoRouterClock(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	ev := mustOne(t, s.Close())
	// There is no router message behind this event, so ts_router can only be
	// the collector's clock. It is NOT left zero: ts_collector drives
	// PARTITION BY and TTL, and a zero stamp would land the row in partition
	// 197001, already past the 90-day TTL (see schema.sql).
	if !ev.Env.TsRouter.AsTime().Equal(t0) || !ev.Env.TsCollector.AsTime().Equal(t0) {
		t.Fatalf("ts_router=%v ts_collector=%v, want both %v",
			ev.Env.TsRouter.AsTime(), ev.Env.TsCollector.AsTime(), t0)
	}
	// TS_COLLECTOR_FALLBACK means "the router's clock read zero and ours was
	// substituted". This router never sent a zero clock -- it sent nothing at
	// all -- and flagging it here would attribute a quirk to a router that
	// does not have one, in the flag set the Parse anomalies dashboard counts
	// per router.
	if hasFlag(ev.Env, vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK) {
		t.Fatalf("view-lost must not claim the router's clock was zero: %v", ev.Env.ParseFlags)
	}
}

// rmMsgDeclaring builds a Route Monitoring message whose BGP header claims
// declared bytes while the payload actually carries 19+len(body): the two
// disagree whenever a sender lies about, or corrupts, the length field.
func rmMsgDeclaring(ph bmp.PeerHeader, declared uint16, body []byte) bmp.Msg {
	m := rmMsg(ph, body)
	hdr := len(ph.Append(nil))
	binary.BigEndian.PutUint16(m.Payload[hdr+16:], declared)
	return m
}

// An IPv4 NLRI trailer runs to the end of the BGP message, and every 0x00
// byte in it is one /0 prefix. The BGP header's length field is what bounds
// that trailer, so a message whose header says 26 bytes can hold at most
// three 1-byte prefixes, whatever follows it in the BMP message. Before the
// collector read that field it parsed to the end of the BMP message instead:
// 16 MiB of zeros became 16 million prefixes and 2.4 GB of heap.
func TestSessionRMTrailerPastBGPLengthIsNotParsed(t *testing.T) {
	s := newTestSession()
	body := updateBody(nil, nil, make([]byte, 3)) // three /0 prefixes
	trailer := make([]byte, 1<<20)                // zeros: a million more /0s
	m := rmMsgDeclaring(peerHdr(), uint16(19+len(body)), append(body, trailer...))

	ev := mustOne(t, s.Handle(m))
	if r := ev.Env.GetRoute(); r != nil && len(r.GetAnnounced()) > 3 {
		t.Fatalf("parsed %d prefixes; a 26-byte BGP message holds at most 3", len(r.GetAnnounced()))
	}
	raw := ev.Env.GetRaw()
	if raw == nil || !strings.Contains(raw.GetParseError(), "length") {
		t.Fatalf("want a raw event naming the length mismatch, got %+v", ev.Env.Payload)
	}
	// Lossless: the whole message, trailer included, is what the raw event keeps.
	if len(raw.GetBmpMsg()) != 6+len(m.Payload) {
		t.Fatalf("raw bmp_msg is %d bytes, want %d", len(raw.GetBmpMsg()), 6+len(m.Payload))
	}
}

func TestSessionRMBGPLengthMismatchBecomesRaw(t *testing.T) {
	body := updateBody(nil, nil, []byte{24, 10, 0, 0})
	for _, tc := range []struct {
		name     string
		declared uint16
	}{
		{"zero", 0},
		{"below the 19-byte header", 18},
		{"past the bytes present", uint16(19 + len(body) + 1)},
		{"short of the bytes present", uint16(19 + len(body) - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSession()
			ev := mustOne(t, s.Handle(rmMsgDeclaring(peerHdr(), tc.declared, body)))
			if ev.Env.GetRaw() == nil || ev.Env.GetRaw().GetParseError() == "" {
				t.Fatalf("want a raw event with a parse error, got %+v", ev.Env.Payload)
			}
		})
	}
	// The control: the same body with an honest length parses.
	ev := mustOne(t, newTestSession().Handle(rmMsgDeclaring(peerHdr(), uint16(19+len(body)), body)))
	if r := ev.Env.GetRoute(); r == nil || len(r.GetAnnounced()) != 1 || r.GetAnnounced()[0].GetPrefix() != "10.0.0.0/24" {
		t.Fatalf("honest length: want one 10.0.0.0/24, got %+v", ev.Env.Payload)
	}
}

func peerUpFor(t *testing.T, addr string) bmp.Msg {
	t.Helper()
	ph := peerHdr()
	ph.Addr = netip.MustParseAddr(addr)
	return mustMsg(t, bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001))
}

// A router controls how many peers it announces, and each one costs this
// session a map entry it keeps until the connection ends, plus a VIEW_LOST
// event at Close. Past the cap a new peer is recorded raw, not tracked.
func TestSessionPeerCapRecordsExcessPeersRaw(t *testing.T) {
	s := newTestSession()
	s.maxPeers = 2
	for _, a := range []string{"10.0.0.9", "10.0.0.10"} {
		if ev := mustOne(t, s.Handle(peerUpFor(t, a))); ev.Env.GetPeerEvent() == nil {
			t.Fatalf("peer %s under the cap: want a PeerEvent, got %+v", a, ev.Env.Payload)
		}
	}
	ev := mustOne(t, s.Handle(peerUpFor(t, "10.0.0.11")))
	if ev.Env.GetRaw() == nil || !strings.Contains(ev.Env.GetRaw().GetParseError(), "peer") {
		t.Fatalf("third peer past a cap of 2: want a raw event with a parse error, got %+v", ev.Env.Payload)
	}

	// Every other message type that would create peer state is capped too:
	// Route Monitoring, Stats and Peer Down for an unseen peer each used to
	// allocate one.
	ph := peerHdr()
	ph.Addr = netip.MustParseAddr("10.0.0.12")
	body := updateBody(nil, nil, []byte{24, 10, 0, 0})
	for name, m := range map[string]bmp.Msg{
		"route monitoring": rmMsg(ph, body),
		"stats":            {Type: bmp.TypeStatsReport, Payload: append(ph.Append(nil), 0, 0, 0, 0)},
		"peer down":        mustMsg(t, bmptest.PeerDown(ph, 2, nil)),
	} {
		if ev := mustOne(t, s.Handle(m)); ev.Env.GetRaw() == nil || ev.Env.GetRaw().GetParseError() == "" {
			t.Fatalf("%s for a peer past the cap: want raw with a parse error, got %+v", name, ev.Env.Payload)
		}
	}
	if len(s.peers) != 2 {
		t.Fatalf("tracked %d peers, want the cap of 2", len(s.peers))
	}

	// A peer already tracked keeps working at the cap.
	if ev := mustOne(t, s.Handle(rmMsg(peerHdr(), body))); ev.Env.GetRoute() == nil {
		t.Fatalf("tracked peer at the cap: want a RouteEvent, got %+v", ev.Env.Payload)
	}
	if evs := s.Close(); len(evs) != 2 {
		t.Fatalf("Close: want 2 VIEW_LOST events, got %d", len(evs))
	}
}

func TestSessionPeerCapDefault(t *testing.T) {
	if s := newTestSession(); s.maxPeers != maxPeersPerSession || maxPeersPerSession != 10000 {
		t.Fatalf("maxPeers=%d maxPeersPerSession=%d, want 10000", s.maxPeers, maxPeersPerSession)
	}
}

// A BGP-LS Node Name is raw wire bytes. proto3 string fields must be valid
// UTF-8, and proto.Marshal rejects the whole envelope otherwise, so an
// invalid name used to lose the entire LsEvent at publish time.
func TestSessionLsNodeNameInvalidUTF8StillMarshals(t *testing.T) {
	s := newTestSession()
	s.Handle(mustMsg(t, bmptest.PeerUp(peerHdr(), netip.MustParseAddr("10.0.0.1"),
		179, 33001, fullCaps(), fullCaps(), 65000, 65001)))

	nlri := lsNodeNLRIBytes(3, 100, lsNodeDescriptorBytes(65000, []byte{10, 255, 0, 1}))
	attrs := buildAttr(0x80, bgpAttrMPReach, mpReachVal(16388, 71, []byte{10, 0, 0, 9}, nlri))
	attrs = append(attrs, buildAttr(0x80, 29, lsTLVBytes(nil, 1026, []byte{'r', 0xff, 0xfe, '1'}))...)
	ev := mustOne(t, s.Handle(rmMsg(peerHdr(), updateBody(nil, attrs, nil))))

	nodes := ev.Env.GetLs().GetNodes()
	if len(nodes) != 1 {
		t.Fatalf("want 1 node, got %+v", ev.Env.Payload)
	}
	if name := nodes[0].GetName(); !utf8.ValidString(name) || name != "r�1" {
		t.Fatalf("name = %q, want %q", name, "r�1")
	}
	if _, err := proto.Marshal(ev.Env); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
