package natsutil

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil/natstest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// metricValue reads a gauge or counter from the default Prometheus registry,
// where the collector package's promauto metrics live. It is process-wide,
// so callers compare before and after.
func metricValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			var v float64
			for _, m := range mf.GetMetric() {
				v += m.GetGauge().GetValue() + m.GetCounter().GetValue()
			}
			return v
		}
	}
	t.Fatalf("metric %s not registered", name)
	return 0
}

// waitFor polls cond every 10 ms until it holds, failing after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// peerEnvelope is one message read back off the PEER stream.
type peerEnvelope struct {
	subject, msgID string
	env            *vantagev1.Envelope
}

// readPeer returns every message PEER holds, read over a direct connection.
func readPeer(t *testing.T, js jetstream.JetStream) []peerEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, "PEER")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := js.OrderedConsumer(ctx, "PEER", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]peerEnvelope, 0, info.State.Msgs)
	for range info.State.Msgs {
		m, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		env := &vantagev1.Envelope{}
		if err := proto.Unmarshal(m.Data(), env); err != nil {
			t.Fatal(err)
		}
		out = append(out, peerEnvelope{m.Subject(), m.Headers().Get(jetstream.MsgIDHeader), env})
	}
	return out
}

// TestOwedViewLostIsRepublishedThroughTheRealPublisher drives a collector's
// close-out through a real Publisher, a real nats.go connection and a real
// JetStream server while NATS is unreachable:
//
//  1. a BMP session is up, its Peer-Up stored;
//  2. the connection is cut and reconnects are refused, and one more BMP
//     message is sent, so a publish is in flight during the outage;
//  3. that publish fails at the retry deadline, which closes the session, and
//     the session's view_lost fails the same way and is owed;
//  4. NATS is let back in.
//
// Exactly one view_lost must then be stored, carrying the session and seq of
// the session that closed and the msg-id built from them.
//
// It runs twice. With nats.go's reconnect buffer, the original view_lost sits
// in that buffer and is flushed at reconnect, so the owed republish must
// dedupe against it. With the buffer turned off, nothing is flushed, and the
// owed republish is the only way the view_lost reaches the stream.
func TestOwedViewLostIsRepublishedThroughTheRealPublisher(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []nats.Option
	}{
		{"reconnect buffer flushes the original", nil},
		{"no reconnect buffer", []nats.Option{nats.ReconnectBufSize(-1)}},
	} {
		t.Run(tc.name, func(t *testing.T) { testOwedRepublish(t, tc.opts) })
	}
}

func testOwedRepublish(t *testing.T, extra []nats.Option) {
	srv := natstest.RunServer(t)
	proxy := natstest.NewProxy(t, srv.Addr().String())

	dnc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dnc.Close)
	direct, err := jetstream.New(dnc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := EnsureStreams(ctx, direct, testOpts); err != nil {
		t.Fatal(err)
	}

	nc, err := nats.Connect(proxy.URL(), append([]nats.Option{
		nats.MaxReconnects(-1), nats.ReconnectWait(20 * time.Millisecond)}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher(js)
	pub.retry.deadline = 2 * time.Second

	cs := collector.NewServer(collector.Config{CollectorID: "owed-e2e"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- cs.Serve(serveCtx, ln) }()
	t.Cleanup(func() {
		stopServe()
		<-serveDone
	})

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
	for _, raw := range [][]byte{
		bmptest.Init("rr1", "Arista Networks EOS version 4.30.2F"),
		bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, 65001),
	} {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	var up peerEnvelope
	waitFor(t, 10*time.Second, "the Peer-Up in PEER", func() bool {
		got := readPeer(t, direct)
		if len(got) == 1 {
			up = got[0]
		}
		return len(got) == 1
	})
	if up.env.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_UP {
		t.Fatalf("PEER's first message is %v, want a Peer-Up", up.env.GetPeerEvent().GetKind())
	}

	resent := metricValue(t, "vantage_collector_owed_view_lost_resent_total")
	proxy.Refuse()
	proxy.Sever()
	waitFor(t, 5*time.Second, "the publisher to see the disconnect", func() bool { return !pub.Connected() })
	if _, err := conn.Write(bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the close-out's view_lost to be owed", func() bool {
		return metricValue(t, "vantage_collector_owed_view_lost") > 0
	})
	if got := readPeer(t, direct); len(got) != 1 {
		t.Fatalf("PEER holds %d messages while NATS is unreachable, want only the Peer-Up", len(got))
	}

	proxy.Allow()
	waitFor(t, 20*time.Second, "the owed view_lost to be stored", func() bool {
		return len(readPeer(t, direct)) >= 2
	})
	waitFor(t, 20*time.Second, "the owed view_lost to be handed back to the publisher", func() bool {
		return metricValue(t, "vantage_collector_owed_view_lost_resent_total") > resent
	})
	// Drain waits for the republish's ack, so a second copy the server was
	// going to store is stored by the time PEER is read below. Its error is
	// the outage's: the route publish and the original view_lost both gave
	// up at the retry deadline.
	_ = pub.Drain(10 * time.Second)

	got := readPeer(t, direct)
	var lost []peerEnvelope
	for _, m := range got[1:] {
		if m.env.GetPeerEvent().GetKind() == vantagev1.PeerEvent_KIND_VIEW_LOST {
			lost = append(lost, m)
		}
	}
	if len(lost) != 1 || len(got) != 2 {
		t.Fatalf("PEER holds %d messages after the Peer-Up, %d of them view_lost; want exactly one view_lost",
			len(got)-1, len(lost))
	}
	vl := lost[0]
	if vl.env.GetSessionId() != up.env.GetSessionId() {
		t.Errorf("view_lost session %d, want the Peer-Up's %d", vl.env.GetSessionId(), up.env.GetSessionId())
	}
	if vl.env.GetSeq() <= up.env.GetSeq() {
		t.Errorf("view_lost seq %d, want one after the Peer-Up's %d", vl.env.GetSeq(), up.env.GetSeq())
	}
	if vl.subject != up.subject {
		t.Errorf("view_lost subject %q, want the Peer-Up's %q", vl.subject, up.subject)
	}
	// subjects.Peer is <prefix>.peer.<router token>.<peer token>, and the
	// msg-id is <router token>/<peer token>/<session>/<seq>.
	toks := strings.Split(vl.subject, ".")
	if len(toks) < 2 {
		t.Fatalf("view_lost subject %q has no router and peer tokens", vl.subject)
	}
	wantID := fmt.Sprintf("%s/%s/%d/%d", toks[len(toks)-2], toks[len(toks)-1], vl.env.GetSessionId(), vl.env.GetSeq())
	if vl.msgID != wantID {
		t.Errorf("view_lost msg-id %q, want %q", vl.msgID, wantID)
	}
}
