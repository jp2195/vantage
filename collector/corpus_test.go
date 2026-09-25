package collector

import (
	"bytes"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// TestCorpusReplay replays every capture taken from a private lab (a
// 2026-08-09 SR/MPLS lab) through the session layer. These are real
// vendor bytes, not fixtures we wrote, so a failure here means a decoder
// disagrees with a shipping implementation.
//
// The corpus is captured from private lab runs and committed to
// bgp/testdata/corpus separately from this test. Until those files
// land, every case below skips (naming the missing file) rather than
// failing, so the suite stays green in the meantime and starts asserting
// the moment the fixtures are committed.
func TestCorpusReplay(t *testing.T) {
	cases := []struct {
		file      string
		minRoutes int
		// wantExtComms are (name, value) pairs that must appear somewhere in
		// this capture's route events. Every entry is bytes a real router
		// sent, read off the committed fixture -- this is what makes the
		// extended-community decoder traffic-verified rather than another
		// entry in the "verified by reading" backlog.
		wantExtComms [][2]string
	}{
		{"iosxr/xrd-26.1.1-pe-vpn4.bmpcap", 1, [][2]string{{"rt", "65000:100"}}},
		{"iosxr/xrd-26.1.1-rr-vpn4-lu4-ls.bmpcap", 1, [][2]string{{"rt", "65000:100"}}},
		{"iosxr/xrd-26.1.1-pe2-vpn4.bmpcap", 1, [][2]string{{"rt", "65000:100"}}},
		{"iosxr/xrd-26.1.1-p-linkstate.bmpcap", 1, nil},
		// These four were captured 2026-08-26 and committed, but no test
		// referenced them, so they were carried in the repo without ever
		// being replayed. The vpn6 pair in particular decoded to nothing at
		// all until AFI 2 was added to decodeNLRI -- exactly the kind of
		// silent gap an unreferenced fixture hides.
		{"iosxr/xrd-26.1.1-pe1-lu4.bmpcap", 1, nil},
		{"iosxr/xrd-26.1.1-pe1-prefix-churn.bmpcap", 1, nil},
		{"iosxr/xrd-26.1.1-pe1-vpn6.bmpcap", 1, [][2]string{{"rt", "65000:100"}}},
		{"iosxr/xrd-26.1.1-pe2-vpn6-stats.bmpcap", 1, nil},
		// IOS-XE finally exists. This case named cat8000v-...-rr-vpn4-lu4
		// from the day it was written and silently skipped on every run for
		// months, because the file was never captured: BMP on xe-rr2 was
		// configured, accepted, and never connected. The missing piece was
		// `update-source` under `bmp server` -- without it the server reports
		// CfgSvr# 1 with ActSvr# empty and TCB 0x0, and never opens the TCP
		// connection.
		//
		// Named for what it holds, not what was hoped for: the capture
		// carries vpn4 and no labeled-unicast, so the old "-lu4" half of the
		// name is gone rather than left to mislead the next reader the way
		// xrd-26.1.1-pe1-lu4.bmpcap (which contains no lu4) did.
		{"iosxe/cat8000v-17.18.02-rr-vpn4.bmpcap", 1, nil},
		// The first BMP bytes this project ever got from IOS-XE. Kept
		// alongside the vpn4 capture because it is the only IOS-XE fixture
		// with a Peer Down in it.
		{"iosxe/cat8000v-17.18.02-rr-first-bmp.bmpcap", 0, nil},
		// IOS-XE receiving from the 4-byte-ASN FRR speaker. The lab used for
		// these captures is numbered 65000-65102, so this is the only way an
		// IOS-XE capture can carry an ASN above 65535 -- in the AS_PATH and
		// in the AGGREGATOR, which has a 2-byte legacy form the 4-byte one
		// must not be confused with. Also the only IOS-XE source of
		// communities and RFC 8092 large communities.
		{"iosxe/cat8000v-17.18.02-rr-ebgp-rich.bmpcap", 1, nil},
		// The FRR pair -- two FRR 10.3 speakers in one iBGP session, one
		// originating and the other streaming BMP: FRR as the BMP subject
		// rather than as a feeder for someone else's capture.
		{"frr/frr-10.3-pair-rich.bmpcap", 0, nil},
		// KNOWN GAP, kept deliberately. This capture carries four genuine
		// IPv4 withdrawals -- `wlen=4` with no path attributes, which is one
		// byte of prefix length plus a three-byte /24 -- and the pipeline
		// produces no withdrawn prefixes from them at all, so route/withdraw
		// is still OPEN for FRR despite the bytes being right here.
		//
		// The suspicion is ADD-PATH: this session negotiates it (the fixture
		// is why rib/peer-addpath is covered for FRR), and a four-byte
		// withdrawal has no room for a Path ID, so a decoder that expects one
		// consumes the whole thing as an ID and finds no prefix. Whether the
		// fault is FRR omitting the ID or the decoder applying the capability
		// to the wrong direction is NOT established, and guessing at the end
		// of a long session is how the wrong fix gets committed. The fixture
		// exists so the next person starts from bytes rather than from a
		// description.
		//
		// It also holds End-of-RIB markers for vpn6, evpn and ipv6u, which is
		// the RFC 4724 MP form -- MP_UNREACH with no NLRI.
		{"frr/frr-10.3-pair-eor-withdraw.bmpcap", 0, nil},
		{"nxos/n9kv-10.6.2F-leaf-evpn.bmpcap", 1, [][2]string{
			{"rt", "65100:10010"}, // L2VNI 10010 -- the VNI added later, verified through the label path
			{"rt", "65100:50001"}, // L3VNI 50001
			{"encap", "vxlan"},
			{"mac-mobility", "sticky,seq=0"},
			{"router-mac", "52:31:d4:03:1b:08"},
		}},
		{"nxos/n9kv-10.6.2F-spine-evpn.bmpcap", 1, [][2]string{
			{"rt", "65100:10010"},
			{"encap", "vxlan"},
			{"router-mac", "52:91:28:f7:1b:08"},
		}},
		{"nxos/n9kv-10.6.2F-termination.bmpcap", 1, nil},
		{"nxos/n9kv-10.6.2-p3-ls-prefix.bmpcap", 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join("..", "bgp", "testdata", "corpus", tc.file)
			evs := replayCorpus(t, path)

			gotExtComms := map[[2]string]int{}
			for _, ev := range evs {
				for _, ec := range ev.Env.GetRoute().GetAttrs().GetExtendedCommunities() {
					gotExtComms[[2]string{bgp.ExtCommName(uint8(ec.GetType()), uint8(ec.GetSubType())), ec.GetValue()}]++
				}
			}
			for _, want := range tc.wantExtComms {
				if gotExtComms[want] == 0 {
					t.Errorf("corpus %s: no %s=%s extended community decoded; got %v",
						tc.file, want[0], want[1], gotExtComms)
				}
			}
			if len(evs) < tc.minRoutes {
				t.Errorf("got %d events, want at least %d", len(evs), tc.minRoutes)
			}
			t.Logf("%s: %d events", tc.file, len(evs))
		})
	}
}

// replayCorpus reads one committed corpus capture and replays it through a
// fresh Session, returning every published event. Factored out of
// TestCorpusReplay so a second test (TestSessionPublishesTypedLinkState)
// can replay the same fixture without duplicating the read/parse loop --
// see that test's doc comment.
func replayCorpus(t *testing.T, path string) []Event {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Covers both the file itself and any missing parent directory
			// (e.g. the corpus tree not existing at all yet)
			// -- os.ReadFile surfaces both as ENOENT.
			t.Skipf("corpus file not committed yet: %s", path)
		}
		t.Fatalf("read corpus: %v", err)
	}

	t0 := time.Unix(0, 0)
	s := NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 1,
		func() time.Time { return t0 }, Overrides{})

	var events []Event
	msgs := 0
	r := bytes.NewReader(raw)
	for {
		m, err := bmp.ReadMsg(r)
		if err != nil {
			if err != io.EOF {
				// Anything other than a clean EOF at a message boundary
				// means the capture is truncated or corrupt -- ReadMsg's own
				// contract says the byte stream cannot be resynchronized
				// past that point, so treating it as "clean end of stream"
				// would hide a broken fixture.
				t.Fatalf("corpus %s: read msg %d: %v", path, msgs+1, err)
			}
			break // clean end of the concatenated stream
		}
		msgs++
		events = append(events, s.Handle(m)...)
	}
	if msgs == 0 {
		// A file that exists but decodes to zero messages must fail, not
		// pass vacuously -- this is exactly the failure mode an empty or
		// wrongly-captured fixture would produce.
		t.Fatalf("no BMP messages recovered from %s", path)
	}
	return events
}

// TestCorpusPrefixNLRI asserts on the one thing the prefix decoder exists
// for. It replays an NX-OS capture (nx-p3 reporting what the route reflector
// sent it) carrying a Prefix NLRI for a loopback that was created on a lab
// router with "prefix-sid index 201" while the capture window was open,
// and removed straight after -- so the SID asserted below is a value
// chosen on the ROUTER and observed coming back out of the pipeline, not a
// number this package agrees with itself about.
//
// This is also the corpus's first NX-OS capture carrying link-state at all:
// every earlier NX-OS fixture is EVPN, and every earlier link-state fixture
// is IOS-XR.
func TestCorpusPrefixNLRI(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos", "n9kv-10.6.2-p3-ls-prefix.bmpcap")
	evs := replayCorpus(t, path)

	var found int
	for _, ev := range evs {
		for _, p := range ev.Env.GetLs().GetPrefixes() {
			if netip.AddrFrom4([4]byte(p.GetPrefix())).String() != "10.201.0.5" {
				continue
			}
			found++
			if p.GetPrefixLen() != 32 {
				t.Errorf("prefix_len = %d, want 32", p.GetPrefixLen())
			}
			if !p.GetHasPrefixSid() || p.GetPrefixSid() != 201 {
				t.Errorf("prefix_sid = %d (has=%v), want 201 -- the index configured on xr-p2's Loopback201",
					p.GetPrefixSid(), p.GetHasPrefixSid())
			}
			if p.GetOspfRouteType() != 1 {
				t.Errorf("ospf_route_type = %d, want 1 (intra-area)", p.GetOspfRouteType())
			}
			// The advertising node must be xr-p2, or the prefix cannot be
			// joined back to the right node in ls_nodes.
			if got := netip.AddrFrom4([4]byte(p.GetLocal().GetRouterId())).String(); got != "10.255.0.5" {
				t.Errorf("advertising node router-id = %s, want 10.255.0.5 (xr-p2)", got)
			}
		}
	}
	if found == 0 {
		t.Fatal("no prefix NLRI for 10.201.0.5 decoded from the capture")
	}
}

// TestCorpusVpn6NLRI asserts vpn6 (AFI 2, SAFI 128) decodes into typed
// prefixes, replaying real IOS-XR bytes.
//
// Every value asserted here was configured on the ROUTER and is read back out
// of the pipeline, not agreed on between this package and itself: the
// advertising router's config puts `rd 65000:101` and `network
// 2001:db8:a1::/64` under `vrf CUST-A`, whose import/export route-target is
// 65000:100.
//
// The capture has been committed since 2026-08-26 and carried nothing: with no
// AFI 2 case in decodeNLRI, the NLRI was kept as RawReach under
// PARSE_FLAG_UNKNOWN_FAMILY and no vpn6 row was ever written, while
// sink/rows.go named "vpn6" in a table no value could reach.
func TestCorpusVpn6NLRI(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "iosxr", "xrd-26.1.1-pe1-vpn6.bmpcap")
	evs := replayCorpus(t, path)

	var found int
	for _, ev := range evs {
		rt := ev.Env.GetRoute()
		if rt.GetFamily().GetAfi() != 2 || rt.GetFamily().GetSafi() != 128 {
			continue
		}
		for _, vp := range rt.GetVpnAnnounced() {
			if vp.GetPrefix() != "2001:db8:a1::/64" {
				continue
			}
			found++
			if vp.GetRd() != "65000:101" {
				t.Errorf("rd = %q, want %q -- the RD configured on xr-pe1's vrf CUST-A",
					vp.GetRd(), "65000:101")
			}
			if len(vp.GetLabels()) == 0 {
				t.Errorf("no MPLS label on the vpn6 prefix; a VPN route must carry one")
			}
		}
	}
	if found == 0 {
		t.Fatal("no typed vpn6 prefix 2001:db8:a1::/64 decoded from the capture")
	}
}

// TestCorpusNxosCommunities pins the two things that were open questions about
// NX-OS until 2026-08-27, both now answered against bytes a real n9kv sent.
//
// Two leaf routers carried a SET-COMM route-map, previously unverified:
// never applied, never commit-tested, no capture. Its own comment named
// the risk -- an outbound route-map on an EVPN neighbor could strip the
// auto-derived route targets, which is what `additive` is there to
// prevent.
//
// Applying it live to both leaves and capturing nx-spine1's adj-RIB-in (the
// leaves' own BMP feeds mirror adj-RIB-in, so they cannot show what their
// OUTBOUND policy did) settles both:
//
//   - NX-OS accepts the config.
//   - `additive` holds: RT 65100:10010 is the RT the platform derives from
//     L2VNI 10010, and it survives alongside the RT the route-map adds.
//
// This is also the corpus's first standard communities from NX-OS at all --
// before this capture, attr/communities appeared only on IOS-XR fixtures, so
// the community decoder had never seen this vendor's bytes.
func TestCorpusNxosCommunities(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos",
		"n9kv-10.6.2-spine-evpn-comm.bmpcap")
	evs := replayCorpus(t, path)

	// 65101:100 and 65101:200 as set by nx-leaf1's route-map, in the 32-bit
	// wire form (ASN<<16 | value).
	const c100, c200 = 65101<<16 | 100, 65101<<16 | 200

	var sawComms, sawDerivedRT, sawAddedRT bool
	for _, ev := range evs {
		a := ev.Env.GetRoute().GetAttrs()
		if a == nil {
			continue
		}
		var got100, got200 bool
		for _, c := range a.GetCommunities() {
			got100 = got100 || c == c100
			got200 = got200 || c == c200
		}
		if got100 && got200 {
			sawComms = true
		}
		for _, ec := range a.GetExtendedCommunities() {
			if bgp.ExtCommName(uint8(ec.GetType()), uint8(ec.GetSubType())) != "rt" {
				continue
			}
			switch ec.GetValue() {
			case "65100:10010":
				sawDerivedRT = true
			case "65101:1":
				sawAddedRT = true
			}
		}
	}
	if !sawComms {
		t.Errorf("no route carried both standard communities 65101:100 and 65101:200 "+
			"(%d and %d) that nx-leaf1's SET-COMM route-map sets", c100, c200)
	}
	if !sawAddedRT {
		t.Error("no route carried rt:65101:1, the extended community SET-COMM adds")
	}
	if !sawDerivedRT {
		t.Error("no route carried rt:65100:10010, the RT NX-OS auto-derives from L2VNI 10010 -- " +
			"its absence would mean the outbound route-map stripped the auto-derived route targets, " +
			"which is exactly what `additive` exists to prevent")
	}
}

// TestCorpusAggregateAttributes covers ATOMIC_AGGREGATE and AGGREGATOR, the
// last two path attributes the corpus had no real-router example of.
//
// Both were produced by configuring `aggregate-address 172.16.0.0
// 255.255.0.0 summary-only` on a lab CE router (AS 65101, router-id
// 10.255.3.1), which owns 172.16.1.0/24 and 172.16.2.0/24, and capturing
// xr-pe1's adj-RIB-in. xr-pe1 reported the received path as
//
//	65101, (aggregated by 65101 10.255.3.1)
//	Origin IGP, ... atomic-aggregate, best, group-best
//
// so the ASN and router-id below are values chosen on the CE router and
// observed coming back out of the pipeline.
//
// Capturing this took three attempts and the reason is worth keeping: xr-pe1's
// BMP feed had gone silent (last row 19:31:39, aggregate created 19:34:17), so
// the first two windows recorded zero messages even though the route was
// demonstrably in the router's table. Toggling `bmp-activate server 1` on the
// CE neighbor forces a fresh dump; waiting does not.
func TestCorpusAggregateAttributes(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "iosxr",
		"xrd-26.1.1-pe1-aggregate.bmpcap")
	evs := replayCorpus(t, path)

	var found bool
	for _, ev := range evs {
		rt := ev.Env.GetRoute()
		a := rt.GetAttrs()
		if a == nil {
			continue
		}
		var isAggregate bool
		for _, p := range rt.GetAnnounced() {
			if p.GetPrefix() == "172.16.0.0/16" {
				isAggregate = true
			}
		}
		for _, vp := range rt.GetVpnAnnounced() {
			if vp.GetPrefix() == "172.16.0.0/16" {
				isAggregate = true
			}
		}
		if !isAggregate {
			continue
		}
		found = true
		if !a.GetAtomicAggregate() {
			t.Error("172.16.0.0/16 decoded without ATOMIC_AGGREGATE, which xr-pe1 reports on the received path")
		}
		agg := a.GetAggregator()
		if agg == nil {
			t.Fatal("172.16.0.0/16 decoded with no AGGREGATOR attribute")
		}
		if agg.GetAsn() != 65101 {
			t.Errorf("aggregator ASN = %d, want 65101 (ce1's AS)", agg.GetAsn())
		}
		if agg.GetIp() != "10.255.3.1" {
			t.Errorf("aggregator IP = %q, want \"10.255.3.1\" (ce1's router-id)", agg.GetIp())
		}
	}
	if !found {
		t.Fatal("aggregate 172.16.0.0/16 not present in the capture")
	}
}

// TestCorpusIPv6UnicastNLRI covers ipv6 unicast (AFI 2, SAFI 1), the last
// address family sink/rows.go names that nothing could reach.
//
// A lab CE router advertises `network 2001:DB8:E1::/64` (its Loopback20)
// over an IPv6 BGP session to the monitored PE, which mirrors that
// neighbor's adj-RIB-in. So the prefix below is a value configured on the
// CE router and read back out of the pipeline.
//
// The same capture is what covers rib/peer-ipv6: the monitored peer's address
// is IPv6, so the per-peer header's V flag is set -- the first capture in the
// corpus for which that is true.
func TestCorpusIPv6UnicastNLRI(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "iosxr",
		"xrd-26.1.1-pe1-ipv6u.bmpcap")
	evs := replayCorpus(t, path)

	var found bool
	for _, ev := range evs {
		rt := ev.Env.GetRoute()
		if rt.GetFamily().GetAfi() != 2 || rt.GetFamily().GetSafi() != 1 {
			continue
		}
		for _, p := range rt.GetAnnounced() {
			if p.GetPrefix() == "2001:db8:e1::/64" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no typed ipv6 unicast prefix 2001:db8:e1::/64 decoded from the capture")
	}
}

// TestCorpusFourByteASN covers 4-byte ASN values, which the corpus had none of
// until 2026-08-27. Every ASN in every earlier capture is below 65536 -- the
// lab used for these captures is numbered 65000-65102 -- so although every
// session negotiated 4-byte AS, the values still fit in 16 bits and the wide
// path was never exercised on real bytes.
//
// The peer here is FRR in AS 4200000002 (RFC 6996 private 4-byte range), so
// the ASN appears both in the AS_PATH of the routes it originates and in the
// AGGREGATOR of the aggregate it forms. Encoding width and value width are
// different things and both need covering: the per-peer header's A flag says
// how the AS_PATH is encoded, this says what it carries.
func TestCorpusFourByteASN(t *testing.T) {
	const wantASN = 4200000002

	path := filepath.Join("..", "bgp", "testdata", "corpus", "frr",
		"frr-10.3-asn4byte.bmpcap")
	evs := replayCorpus(t, path)

	var inPath, inAggregator bool
	for _, ev := range evs {
		a := ev.Env.GetRoute().GetAttrs()
		if a == nil {
			continue
		}
		for _, seg := range a.GetAsPath() {
			for _, asn := range seg.GetAsns() {
				if asn == wantASN {
					inPath = true
				}
			}
		}
		if agg := a.GetAggregator(); agg != nil && agg.GetAsn() == wantASN {
			inAggregator = true
		}
	}
	if !inPath {
		t.Errorf("no AS_PATH carried ASN %d; a 4-byte ASN must survive the AS_PATH decoder "+
			"without being truncated to 16 bits", wantASN)
	}
	if !inAggregator {
		t.Errorf("no AGGREGATOR carried ASN %d; the 4-byte aggregator path is the one most "+
			"likely to silently truncate, since AGGREGATOR has a 2-byte legacy form", wantASN)
	}
}

// TestCorpusFrrRibStreams pins the two RIB streams no Cisco platform here can
// produce, both read off real FRR bytes.
//
// Neither IOS-XR nor NX-OS exposes a policy option on its BMP neighbor
// activation -- XR's `bmp-activate ?` offers only `server`, NX-OS's
// `bmp-activate-server` likewise -- so post-policy adj-RIB-in was recorded as
// unobtainable until FRR was tried. FRR takes `bmp monitor <afi> <safi>
// post-policy` alongside pre-policy and loc-rib, and reports all three on one
// session.
//
// The inbound route-map on the FRR speaker used here is what makes the
// two streams differ: it sets local-preference and a large community on
// ingress, so the same prefix appears pre-policy without them and
// post-policy with them. Without that the two streams would be
// byte-identical and this fixture would prove nothing.
func TestCorpusFrrRibStreams(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "frr",
		"frr-10.3-ribs-prepost.bmpcap")
	evs := replayCorpus(t, path)

	var pre, post, locRib bool
	for _, ev := range evs {
		peer := ev.Env.GetPeer()
		if ev.Env.GetRoute() == nil {
			continue
		}
		switch {
		case peer.GetType() == vantagev1.PeerType_PEER_TYPE_LOC_RIB:
			locRib = true
		case peer.GetPostPolicy():
			post = true
		default:
			pre = true
		}
	}
	if !pre {
		t.Error("no pre-policy adj-RIB-in route in the capture")
	}
	if !post {
		t.Error("no post-policy adj-RIB-in route in the capture -- the L flag was never set, " +
			"which is the whole reason this fixture exists")
	}
	if !locRib {
		t.Error("no Loc-RIB route in the capture (peer type 3)")
	}
}

// TestCorpusFrrRouteMirroring covers BMP Route Mirroring (type 6), which no
// Cisco platform available here emits. FRR sends it under `bmp mirror`.
//
// The session layer has no typed event for Route Mirroring, so it surfaces as
// a RawEvent carrying the original bytes -- which is the behavior worth
// pinning: an unrecognized message type must be preserved losslessly rather
// than dropped.
func TestCorpusFrrRouteMirroring(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "frr",
		"frr-10.3-mirror.bmpcap")
	evs := replayCorpus(t, path)

	var found int
	for _, ev := range evs {
		raw := ev.Env.GetRaw()
		if raw == nil || len(raw.GetBmpMsg()) < 6 {
			continue
		}
		if raw.GetBmpMsg()[5] == bmp.TypeRouteMirroring {
			found++
			if raw.GetParseError() != "" {
				t.Errorf("Route Mirroring message reported a parse error: %q", raw.GetParseError())
			}
		}
	}
	if found == 0 {
		t.Fatal("no BMP Route Mirroring message recovered from the capture")
	}
}

// TestCorpusNxosIPv4Unicast closes the largest per-vendor gap in the corpus.
//
// NX-OS is vantage's production-first platform, and until 2026-08-27 it had
// never been captured doing plain IPv4 unicast -- no family/ipv4u, no
// announce, no withdraw. Every NX-OS fixture was EVPN or link-state, so the
// most ordinary thing a router does was untested against this vendor's bytes.
//
// Both prefixes are loopback1 (the NVE source) on each leaf, advertised into a
// new ipv4 unicast address family on the existing spine-leaf sessions and
// reflected by nx-spine1, whose adj-RIB-in this is. 10.255.2.2/32 is announced
// and then withdrawn, so both directions are covered from real NX-OS.
//
// Getting this required a technique worth remembering: a scripted CLI
// session to the router takes 60-90s to establish, longer than a sensible
// capture window, so arming `vantage capture` and then triggering churn
// always missed its own window. Arming the mirror over the admin API,
// taking as long as needed over the CLI, then replaying with `-since` is
// what worked.
func TestCorpusNxosIPv4Unicast(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos",
		"n9kv-10.6.2-spine-ipv4u.bmpcap")
	evs := replayCorpus(t, path)

	announced := map[string]bool{}
	withdrawn := map[string]bool{}
	for _, ev := range evs {
		rt := ev.Env.GetRoute()
		if rt.GetFamily().GetAfi() != 1 || rt.GetFamily().GetSafi() != 1 {
			continue
		}
		for _, p := range rt.GetAnnounced() {
			announced[p.GetPrefix()] = true
		}
		for _, p := range rt.GetWithdrawn() {
			withdrawn[p.GetPrefix()] = true
		}
	}
	for _, want := range []string{"10.255.2.2/32", "10.255.2.3/32"} {
		if !announced[want] {
			t.Errorf("ipv4 unicast announce for %s not decoded; got %v", want, announced)
		}
	}
	if !withdrawn["10.255.2.2/32"] {
		t.Errorf("no ipv4 unicast withdraw for 10.255.2.2/32; got %v -- an announce-only "+
			"capture would leave route/withdraw uncovered for this vendor", withdrawn)
	}
}

// TestCorpusNxosVPN covers vpn4 and vpn6 from NX-OS, in both directions.
//
// Every VPN fixture in the corpus was IOS-XR until 2026-08-27. Getting NX-OS
// to advertise vpn4 at all needs a chain that is not obvious and is not in any
// of this repo's configs: `install feature-set mpls`, then `feature-set mpls`,
// then `feature mpls l3vpn`. Without it `feature l3vpn` is rejected as an
// invalid command, the `address-family vpnv4 unicast` config is still accepted
// (it exists for EVPN/VXLAN), and the router simply advertises nothing --
// `advertised-routes` lists the RDs with no prefixes under them.
//
// RD 65100:777 is the VRF configured for this test; RD 10.255.1.2:4 is the
// auto-derived RD of the tenant used for it, so the capture carries both an
// operator-chosen and a platform-derived RD.
//
// The withdraw needs its own note: loopback77 is redistributed as a connected
// route, so removing the `network` statement leaves the prefix in place.
// Shutting the interface is what actually produces a withdraw.
func TestCorpusNxosVPN(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos",
		"n9kv-10.6.2-spine-vpn-churn.bmpcap")
	evs := replayCorpus(t, path)

	ann, wdr := map[string]string{}, map[string]string{}
	for _, ev := range evs {
		rt := ev.Env.GetRoute()
		for _, v := range rt.GetVpnAnnounced() {
			ann[v.GetPrefix()] = v.GetRd()
		}
		for _, v := range rt.GetVpnWithdrawn() {
			wdr[v.GetPrefix()] = v.GetRd()
		}
	}
	if ann["10.77.0.1/32"] != "65100:777" {
		t.Errorf("vpn4 announce for 10.77.0.1/32 has rd %q, want 65100:777; got %v",
			ann["10.77.0.1/32"], ann)
	}
	if ann["2001:db8:77::/64"] != "65100:777" {
		t.Errorf("vpn6 announce for 2001:db8:77::/64 has rd %q, want 65100:777",
			ann["2001:db8:77::/64"])
	}
	if ann["192.168.10.0/24"] != "10.255.1.2:4" {
		t.Errorf("vpn4 announce for 192.168.10.0/24 has rd %q, want the platform-derived "+
			"10.255.1.2:4", ann["192.168.10.0/24"])
	}
	if wdr["10.77.0.1/32"] == "" {
		t.Errorf("no vpn4 withdraw for 10.77.0.1/32; got %v -- an announce-only capture "+
			"leaves route/vpn-withdraw uncovered for this vendor", wdr)
	}
}

// TestCorpusNxosIPv6 covers ipv6 unicast and an IPv6-addressed peer from
// NX-OS, neither of which the corpus had for this vendor.
//
// These are two different things and one capture covers both: the ipv6 unicast
// NLRI (family/ipv6u) and the per-peer header's V flag, set because the
// monitored session's peer address is IPv6 (rib/peer-ipv6). The session runs
// over an existing spine-leaf link, so it needed only IPv6 addresses on
// the interface at each end.
func TestCorpusNxosIPv6(t *testing.T) {
	path := filepath.Join("..", "bgp", "testdata", "corpus", "nxos",
		"n9kv-10.6.2-spine-ipv6-vpn.bmpcap")
	evs := replayCorpus(t, path)

	var found bool
	for _, ev := range evs {
		rt := ev.Env.GetRoute()
		if rt.GetFamily().GetAfi() != 2 || rt.GetFamily().GetSafi() != 1 {
			continue
		}
		for _, p := range rt.GetAnnounced() {
			if p.GetPrefix() != "2001:db8:78::/64" {
				continue
			}
			found = true
			if got := ev.Env.GetPeer().GetIp(); got != "2001:db8:33::2" {
				t.Errorf("ipv6 unicast route came from peer %q, want the IPv6 peer "+
					"2001:db8:33::2 -- an IPv4 peer would leave rib/peer-ipv6 uncovered", got)
			}
		}
	}
	if !found {
		t.Fatal("no ipv6 unicast prefix 2001:db8:78::/64 decoded from the NX-OS capture")
	}
}

// TestCorpusFixtureContents pins which address families each committed capture
// actually carries, and fails if a family's only source disappears.
//
// It exists because several fixture names do not describe their contents, and
// one of those misnomers is load-bearing:
//
//	xrd-26.1.1-pe-vpn4.bmpcap        ipv4u, lu4, vpn6   <- NO vpn4; the
//	                                                       project's ONLY lu4
//	xrd-26.1.1-pe1-lu4.bmpcap        ipv4u              <- NO lu4
//	xrd-26.1.1-pe2-vpn4.bmpcap       vpn6               <- NO vpn4
//	xrd-26.1.1-rr-vpn4-lu4-ls.bmpcap vpn4               <- no lu4, no ls
//
// So the file named for lu4 has none, and the sole source of lu4 is named
// vpn4. Anyone pruning a "redundant" vpn4 fixture would silently delete every
// lu4 byte this project owns, and no test would have noticed -- the corpus
// tests assert extended communities and route counts, not families.
//
// The names are deliberately NOT changed: renaming them would erase which
// capture is which without changing any byte on disk. Pinning the contents
// here fixes the hazard without touching history.
func TestCorpusFixtureContents(t *testing.T) {
	// family -> the fixtures that carry it, as measured. Read off the
	// captures, not off their names.
	want := map[string][]string{
		"lu4":   {"iosxr/xrd-26.1.1-pe-vpn4.bmpcap"},
		"ipv6u": {"iosxr/xrd-26.1.1-pe1-ipv6u.bmpcap", "nxos/n9kv-10.6.2-spine-ipv6-vpn.bmpcap", "nxos/n9kv-10.6.2-spine-vpn-churn.bmpcap"},
	}
	for fam, sources := range want {
		var alive int
		for _, src := range sources {
			path := filepath.Join("..", "bgp", "testdata", "corpus", src)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("family %s: source %s is gone", fam, src)
				continue
			}
			if corpusCarriesFamily(t, path, fam) {
				alive++
			} else {
				t.Errorf("family %s: %s no longer carries it", fam, src)
			}
		}
		if alive == 0 {
			t.Errorf("family %s has NO surviving source in the corpus", fam)
		}
		if len(sources) == 1 && alive == 1 {
			t.Logf("family %s has exactly one source (%s) -- deleting it removes the family entirely",
				fam, sources[0])
		}
	}
}

// corpusCarriesFamily reports whether the capture at path decodes at least one
// typed NLRI in fam. Typed, not merely present: a family with no decoder
// reaches the sink with Family set and RawReach populated, which is the
// distinction TestCorpusFeatureCoverage exists to keep straight.
func corpusCarriesFamily(t *testing.T, path, fam string) bool {
	t.Helper()
	for _, ev := range replayCorpus(t, path) {
		rt := ev.Env.GetRoute()
		if rt == nil {
			continue
		}
		typed := len(rt.GetAnnounced()) + len(rt.GetWithdrawn()) +
			len(rt.GetVpnAnnounced()) + len(rt.GetVpnWithdrawn())
		if typed == 0 {
			continue
		}
		afi, safi := rt.GetFamily().GetAfi(), rt.GetFamily().GetSafi()
		switch fam {
		case "lu4":
			if afi == 1 && safi == 4 {
				return true
			}
		case "ipv6u":
			if afi == 2 && safi == 1 {
				return true
			}
		}
	}
	return false
}
