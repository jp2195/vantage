package main

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/natsutil/natstest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// cliTestStreamOpts clamps every replica count to 1, mirroring
// natsutil/streams_test.go's testOpts and
// collector/e2e_test.go's e2eStreamOpts: the embedded natstest
// server is a single, non-clustered node, and
// natsutil.StreamOpts{}'s own zero-value LSReplicas defaults to 3, which
// such a node rejects outright. Calling EnsureStreams with a bare
// StreamOpts{} fails here for exactly that reason (LS never provisions,
// and EnsureStreams's own doc comment explains why every *other* stream
// still gets created despite that one failing) -- corrected the same way
// the other two packages' tests already were.
// RoutesMaxBytes/RawMaxBytes are bounded because nats-server validates a
// stream's MaxBytes against the FREE SPACE of the JetStream store
// directory's filesystem, not against the account limit -- an unlimited
// account still refuses a stream larger than the disk can hold. The
// production defaults (8 GiB ROUTES, 2 GiB RAW) exceeded what the CI
// runner's temp filesystem had free, so every test that provisioned
// streams failed there with "insufficient storage resources available"
// while passing locally on a bigger disk. Tests publish kilobytes; 16 MiB
// is far above anything they write and fits anywhere.
var cliTestStreamOpts = natsutil.StreamOpts{Replicas: 1, LSReplicas: 1,
	RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20}

// TestGenMessagesParseBack builds bmpgen's message sequence directly (no
// network) and parses every message
// back through bmp.ReadMsg, checking both the total count and the type
// histogram match a router that sends Initiation once, then per peer
// Peer-Up before any Route Monitoring, then Stats.
func TestGenMessagesParseBack(t *testing.T) {
	msgs := genMessages("rr1", "Arista Networks EOS version 4.30.2F", 2, 3)
	// init + per peer: peer-up + 3 updates + 1 stats
	want := 1 + 2*(1+3+1)
	if len(msgs) != want {
		t.Fatalf("got %d msgs want %d", len(msgs), want)
	}
	types := map[uint8]int{}
	for i, raw := range msgs {
		m, err := bmp.ReadMsg(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		types[m.Type]++
	}
	if types[bmp.TypeInitiation] != 1 || types[bmp.TypePeerUp] != 2 ||
		types[bmp.TypeRouteMonitoring] != 6 || types[bmp.TypeStatsReport] != 2 {
		t.Fatalf("types=%v", types)
	}
}

// TestEndToEndBmpgenToStream is a smoke test: it drives
// genMessages over a real TCP connection into a real collector.Server
// backed by a real embedded JetStream server, and waits for the ROUTES
// stream to durably hold every route event. It uses cliTestStreamOpts in
// place of a bare StreamOpts{} (see its doc comment above) and checks
// errors that an earlier version silently discarded (_, _ := ...).
func TestEndToEndBmpgenToStream(t *testing.T) {
	_, js := natstest.RunJSConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := natsutil.EnsureStreams(ctx, js, cliTestStreamOpts); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := collector.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.CollectorID = "test"
	srv := collector.NewServer(cfg, natsutil.NewPublisher(js), time.Now)
	serveCtx := t.Context()
	go srv.Serve(serveCtx, ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, m := range genMessages("rr1", "Arista Networks EOS version 4.30.2F", 2, 5) {
		if _, err := conn.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := js.Stream(ctx, "ROUTES")
		if err != nil {
			t.Fatal(err)
		}
		si, err := s.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if si.State.Msgs == 10 { // 2 peers x 5 updates
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ROUTES msgs=%d want 10", si.State.Msgs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDebugFilterMatchesLiveAndStored is the closed-loop proof that
// `vantage debug`'s two filter paths agree: bmpgen's own genMessages, fed
// through a real collector.Server into a real embedded JetStream server,
// consumed on both paths `vantage debug` actually uses -- the live
// core-NATS subscription on defaultFilter, and the -from-start ROUTES
// replay via routesStreamFilter
// -- calling this package's own filter logic directly, not a
// reimplementation of it. If either filter were wrong, this test fails.
//
// It also pins down *why* routesStreamFilter exists: a realistic,
// family-scoped operator filter ("vantage.v1.route.ipv4u.>") applied
// unadjusted directly to the ROUTES stream's ordered consumer matches
// nothing at all (asserted below), because ROUTES stores messages under a
// partition-inserted, 7-token subject where the collector published 6.
// That is the silent "looks like no traffic" failure mode
// routesStreamFilter's job is to prevent -from-start from ever hitting.
func TestDebugFilterMatchesLiveAndStored(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := natsutil.EnsureStreams(ctx, js, cliTestStreamOpts); err != nil {
		t.Fatal(err)
	}

	// Subscribe on debug's own default filter *before* any traffic is sent,
	// exactly as `vantage debug` does against core NATS -- proving the live
	// path sees collector publishes on their pre-transform subject.
	sub, err := nc.SubscribeSync(defaultFilter)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := collector.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.CollectorID = "test"
	srv := collector.NewServer(cfg, natsutil.NewPublisher(js), time.Now)
	serveCtx := t.Context()
	go srv.Serve(serveCtx, ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const peers, updates = 2, 5
	msgs := genMessages("rr1", "Arista Networks EOS version 4.30.2F", peers, updates)
	for _, m := range msgs {
		if _, err := conn.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	// Initiation produces no event; each peer then produces exactly one
	// Peer-Up event, `updates` Route Monitoring events, and one Stats event.
	wantLive := peers * (1 + updates + 1)

	live := 0
	byPayload := map[string]int{}
	for live < wantLive {
		m, err := sub.NextMsgWithContext(ctx)
		if err != nil {
			t.Fatalf("live subscribe on %q: got %d/%d envelopes: %v", defaultFilter, live, wantLive, err)
		}
		var env vantagev1.Envelope
		if err := proto.Unmarshal(m.Data, &env); err != nil {
			t.Fatal(err)
		}
		live++
		switch {
		case env.GetRoute() != nil:
			byPayload["route"]++
		case env.GetPeerEvent() != nil:
			byPayload["peer"]++
		case env.GetStats() != nil:
			byPayload["stats"]++
		default:
			byPayload["other"]++
		}
	}
	if byPayload["route"] != peers*updates || byPayload["peer"] != peers || byPayload["stats"] != peers {
		t.Fatalf("live envelopes by payload = %+v, want route=%d peer=%d stats=%d",
			byPayload, peers*updates, peers, peers)
	}

	// Wait for the ROUTES stream to durably hold every route event before
	// exercising the -from-start replay path against it.
	wantRoutes := uint64(peers * updates)
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := js.Stream(ctx, "ROUTES")
		if err != nil {
			t.Fatal(err)
		}
		si, err := s.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if si.State.Msgs == wantRoutes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ROUTES msgs=%d want %d", si.State.Msgs, wantRoutes)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A realistic, family-scoped operator filter -- not the catch-all
	// default -- exercised through debug's own routesStreamFilter, exactly
	// as cmdDebug's -from-start branch would.
	opFilter := "vantage.v1.route.ipv4u.>"
	correct, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		FilterSubjects: []string{routesStreamFilter(opFilter)},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := correct.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.NumPending != wantRoutes {
		t.Fatalf("routesStreamFilter(%q) = %q: NumPending=%d, want %d -- debug's -from-start would under- or over-match",
			opFilter, routesStreamFilter(opFilter), info.NumPending, wantRoutes)
	}
	for i := range wantRoutes {
		m, err := correct.Next()
		if err != nil {
			t.Fatalf("replay message %d: %v", i, err)
		}
		toks := strings.Split(m.Subject(), ".")
		if len(toks) != 7 || toks[4] != "ipv4u" {
			t.Fatalf("stored subject %q: want 7 tokens with family ipv4u at index 4", m.Subject())
		}
	}

	// The regression this test exists to catch: applying opFilter directly
	// to the stream, with no partition-token adjustment, must match
	// *nothing* -- proving routesStreamFilter's splice is load-bearing, not
	// cosmetic. If a future change makes routesStreamFilter a no-op, this
	// assertion (NumPending == 0, despite wantRoutes real messages sitting in
	// the stream) fails loudly, rather than -from-start silently degrading
	// to "no traffic" for any filter more specific than the catch-all
	// default.
	naive, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		FilterSubjects: []string{opFilter},
	})
	if err != nil {
		t.Fatal(err)
	}
	naiveInfo, err := naive.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if naiveInfo.NumPending != 0 {
		t.Fatalf("unadjusted filter %q unexpectedly matched %d stored messages; "+
			"the partition-token premise this test rests on no longer holds",
			opFilter, naiveInfo.NumPending)
	}

	// The assertions above detect under-matching only. Collapsing every route
	// filter to "vantage.v1.route.>" -- silently discarding the operator's
	// family/router/peer scoping -- would pass all of them, because every
	// message published so far is one family from one router. Publish a
	// second family and require the family-scoped filter to exclude it, so
	// over-matching fails too.
	other := collector.Event{
		Subject: "vantage.v1.route.ipv6u.0a000001.0a000009",
		MsgID:   "over-match-probe/1",
		Env:     &vantagev1.Envelope{CollectorId: "c1", Seq: 1},
	}
	op := natsutil.NewPublisher(js)
	if err := op.Publish(other); err != nil {
		t.Fatal(err)
	}
	if err := op.Drain(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	scoped, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		FilterSubjects: []string{routesStreamFilter(opFilter)},
	})
	if err != nil {
		t.Fatal(err)
	}
	scopedInfo, err := scoped.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if scopedInfo.NumPending != wantRoutes {
		t.Fatalf("routesStreamFilter(%q) matched %d of %d ipv4u messages after an ipv6u message was added: "+
			"the adjustment is over-matching and has discarded the operator's family scoping",
			opFilter, scopedInfo.NumPending, wantRoutes)
	}
}

// TestBmpgenProfileEmitsMeasuredSysDescr checks that `bmpgen -profile <name>`
// impersonates a real implementation rather than a plausible-looking string.
//
// The default sysDescr this command shipped with was
// "Cisco IOS XR Software, Version 7.9.2" -- a string no IOS-XR router has ever
// sent over BMP (XRd sends a bare "26.1.1"). Generating traffic under an
// invented identity is how a synthetic corpus starts disagreeing with reality,
// and quirk.anchors is the standing example of what that costs.
//
// bmptest.Profiles() is proven against committed captures by
// TestProfilesAreDerivedFromRealCaptures; this test checks bmpgen actually
// puts the profile's bytes on the wire, and refuses names it has no capture
// for.
func TestBmpgenProfileEmitsMeasuredSysDescr(t *testing.T) {
	for _, name := range []string{"iosxr", "nxos", "frr"} {
		p, err := profileByName(name)
		if err != nil {
			t.Fatalf("profileByName(%q): %v", name, err)
		}
		msgs := genMessages(p.SysName, p.SysDescr, 1, 1)
		m, err := bmp.ReadMsg(bytes.NewReader(msgs[0]))
		if err != nil {
			t.Fatalf("%s: reading Initiation: %v", name, err)
		}
		if m.Type != bmp.TypeInitiation {
			t.Fatalf("%s: first message is type %d, want Initiation", name, m.Type)
		}
		if !bytes.Contains(m.Payload, []byte(p.SysDescr)) {
			t.Errorf("%s: Initiation does not carry the profile's sysDescr %q", name, p.SysDescr)
		}
	}
	if _, err := profileByName("arista"); err == nil {
		t.Error("profileByName(\"arista\") succeeded; there is no committed Arista capture, " +
			"so a profile for it could only be invented")
	}
}

// TestGenChurnProducesWithdrawals covers what the generator is actually
// for: sustained churn at a chosen prefix count, which is the
// one thing a corpus of captures cannot provide. A committed fixture is a
// handful of messages frozen at one instant; CI needs a feed that keeps
// announcing and withdrawing.
//
// The assertion is that churn produces BOTH directions. Announce-only
// coverage exercises the insert path and never the delete path -- and
// route churn is exactly where the sink's dedupe and the dashboards' "did
// this prefix go away" logic live.
func TestGenChurnProducesWithdrawals(t *testing.T) {
	ph := bmp.PeerHeader{
		Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9",
	}
	msgs := genChurn(ph, 4, 3)
	if len(msgs) == 0 {
		t.Fatal("genChurn produced no messages")
	}

	var announced, withdrawn int
	for i, raw := range msgs {
		m, err := bmp.ReadMsg(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if m.Type != bmp.TypeRouteMonitoring {
			t.Fatalf("msg %d: type %d, want Route Monitoring", i, m.Type)
		}
		_, rest, err := bmp.ParsePeerHeader(m.Payload)
		if err != nil {
			t.Fatalf("msg %d: peer header: %v", i, err)
		}
		// RouteMonitoring embeds a complete BGP message; ParseUpdate wants the
		// UPDATE body, so skip the 19-byte BGP header (16-byte marker, length,
		// type).
		if len(rest) < 19 {
			t.Fatalf("msg %d: embedded BGP message too short", i)
		}
		u, err := bgp.ParseUpdate(rest[19:], bgp.Caps{FourByteAS: true,
			MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}})
		if err != nil {
			t.Fatalf("msg %d: parse update: %v", i, err)
		}
		announced += len(u.Announced)
		withdrawn += len(u.Withdrawn)
	}
	if announced == 0 {
		t.Error("churn announced nothing")
	}
	if withdrawn == 0 {
		t.Error("churn withdrew nothing; an announce-only feed never exercises the delete path")
	}
}

// TestBmpgenDefaultSysNameIsSynthetic guards the split between the two
// identity fields. sysDescr must be a measured vendor banner (see
// TestBmpgenProfileEmitsMeasuredSysDescr); sysName must NOT default to the
// real device the profile sample came from, or synthetic traffic lands in the
// same router_sysname as that device's genuine data and the two cannot be told
// apart afterwards.
func TestBmpgenDefaultSysNameIsSynthetic(t *testing.T) {
	for _, p := range bmptest.Profiles() {
		if got := defaultSysName(p); got == p.SysName {
			t.Errorf("profile %q defaults sysName to %q, the real device the sample came from; "+
				"synthetic traffic would be indistinguishable from that router's own", p.Name, got)
		} else if !strings.Contains(got, "bmpgen") {
			t.Errorf("profile %q default sysName %q does not mark itself synthetic", p.Name, got)
		}
	}
}

// TestGenChurnFamilies covers the generator emitting a family other than IPv4
// unicast, which it could not do at all until MP_REACH landed in the builder.
//
// This is what makes the generator useful beyond ipv4u: the L3VPN dashboards
// and the vpn/evpn query paths are built on tables that, before this, could
// only be filled by replaying a handful of frozen captures. Now they can be
// loaded at any volume.
func TestGenChurnFamilies(t *testing.T) {
	ph := bmp.PeerHeader{
		Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9",
	}
	for _, fam := range []bgp.Family{bgp.FamilyVPNv4, bgp.FamilyVPNv6, bgp.FamilyIPv6U, bgp.FamilyEVPN, bgp.FamilyBGPLS} {
		msgs := genChurnFamily(ph, fam, 3, 2)
		if len(msgs) == 0 {
			t.Fatalf("%v: no messages", fam)
		}
		caps := bgp.Caps{FourByteAS: true, MP: map[bgp.Family]bool{fam: true},
			AddPathRecv: map[bgp.Family]bool{}}
		var announced, withdrawn int
		for i, raw := range msgs {
			m, err := bmp.ReadMsg(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("%v msg %d: %v", fam, i, err)
			}
			_, rest, err := bmp.ParsePeerHeader(m.Payload)
			if err != nil {
				t.Fatalf("%v msg %d: peer header: %v", fam, i, err)
			}
			u, err := bgp.ParseUpdate(rest[19:], caps)
			if err != nil {
				t.Fatalf("%v msg %d: parse update: %v", fam, i, err)
			}
			if u.Family != fam {
				t.Errorf("%v msg %d: family = %v", fam, i, u.Family)
			}
			announced += len(u.VpnAnnounced) + len(u.Announced) + len(u.EvpnAnnounced) + len(u.LsNodes)
			withdrawn += len(u.VpnWithdrawn) + len(u.Withdrawn) + len(u.EvpnWithdrawn) + len(u.LsNodesWithdrawn)
		}
		if announced == 0 {
			t.Errorf("%v: announced nothing", fam)
		}
		if withdrawn == 0 {
			t.Errorf("%v: withdrew nothing", fam)
		}
	}
}

// TestGenChurnEmitsValidIGPRouterIDWidths pins that generated BGP-LS nodes
// carry an IGP Router-ID of a width the protocol they claim can actually
// produce.
//
// RFC 9552 section 5.2.1.4 makes the width protocol-specific: IS-IS carries a
// 6-byte system ID, or 7 with the pseudonode octet; OSPF carries a 4-byte
// Router-ID, or 8 for a LAN pseudonode. The generator was emitting protocol 2
// (IS-IS Level 2) with a 4-byte Router-ID -- an OSPF shape under an IS-IS
// protocol-ID, which no real router sends.
//
// This matters beyond tidiness because the load generator writes into the same
// archive real captures do. 248 such rows are already there, and anyone
// classifying nodes by router-ID width -- which is the only way to tell a LAN
// pseudonode from a router, since neither IGP flags it -- meets them looking
// like a decoder defect. A generator that emits shapes the decoder should
// never see manufactures false bug reports.
func TestGenChurnEmitsValidIGPRouterIDWidths(t *testing.T) {
	ph := bmp.PeerHeader{
		Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9",
	}
	msgs := genChurnFamily(ph, bgp.FamilyBGPLS, 4, 3)
	caps := bgp.Caps{FourByteAS: true,
		MP:          map[bgp.Family]bool{bgp.FamilyBGPLS: true},
		AddPathRecv: map[bgp.Family]bool{}}

	var checked int
	for i, raw := range msgs {
		m, err := bmp.ReadMsg(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		_, rest, err := bmp.ParsePeerHeader(m.Payload)
		if err != nil {
			t.Fatalf("msg %d: peer header: %v", i, err)
		}
		u, err := bgp.ParseUpdate(rest[19:], caps)
		if err != nil {
			t.Fatalf("msg %d: parse update: %v", i, err)
		}
		for _, n := range append(append([]bgp.LsNodeNLRI{}, u.LsNodes...), u.LsNodesWithdrawn...) {
			checked++
			w := len(n.Local.RouterID)
			switch n.Protocol {
			case 1, 2: // IS-IS: system ID, plus the pseudonode octet
				if w != 6 && w != 7 {
					t.Errorf("protocol %d (IS-IS) node has a %d-byte IGP Router-ID, want 6 or 7: %x",
						n.Protocol, w, n.Local.RouterID)
				}
			case 3, 6: // OSPF: Router-ID, plus the DR interface for a pseudonode
				if w != 4 && w != 8 {
					t.Errorf("protocol %d (OSPF) node has a %d-byte IGP Router-ID, want 4 or 8: %x",
						n.Protocol, w, n.Local.RouterID)
				}
			default:
				t.Errorf("node claims protocol %d, which the generator should not emit", n.Protocol)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no BGP-LS nodes decoded: this test asserted nothing")
	}
}

// TestSendMessagesPacesOnlyTheChurn pins -churn-interval: the initial dump
// goes out back to back, and the wait falls between churn messages only. A
// test that fed paced traffic to a NATS rolling restart needs the churn
// spread over minutes; pacing the dump too would only delay the session's
// Peer Ups.
func TestSendMessagesPacesOnlyTheChurn(t *testing.T) {
	msgs := [][]byte{{1}, {2}, {3}, {4}, {5}}
	var buf bytes.Buffer
	var waits []int // len(buf) at each wait: what had been written by then
	wait := func(d time.Duration) {
		if d != time.Second {
			t.Errorf("wait(%s), want the configured interval 1s", d)
		}
		waits = append(waits, buf.Len())
	}
	if err := sendMessages(&buf, msgs, 2, time.Second, wait); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes(); !bytes.Equal(got, []byte{1, 2, 3, 4, 5}) {
		t.Fatalf("wrote %v, want every message in order", got)
	}
	// Dump (2 messages) unpaced, then one wait before each of the 3 churn
	// messages.
	if want := []int{2, 3, 4}; !equalInts(waits, want) {
		t.Fatalf("waited after %v bytes, want %v", waits, want)
	}

	buf.Reset()
	waits = nil
	if err := sendMessages(&buf, msgs, 2, 0, wait); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("interval 0 waited %d times, want none", len(waits))
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
