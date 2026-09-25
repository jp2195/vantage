// Package collector_test is deliberately an external test package (not
// collector), because it exercises the full stack -- collector.Server
// talking to a real natsutil.Publisher backed by a real embedded NATS/
// JetStream server (natstest) -- and natsutil imports collector (for
// collector.Event), so collector itself cannot import natsutil without a
// cycle. This is the end-to-end test: start it against an embedded NATS,
// connect a TCP client, feed it bmptest-built BMP messages, and assert the
// envelopes land in the streams -- as opposed to
// server_test.go's capturePub-based tests, which prove the server's own
// logic but never touch a real JetStream stream at all.
package collector_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
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

// e2eStreamOpts clamps every replica count to 1: the embedded natstest
// server is a single, non-clustered node, and (per natsutil's own tests)
// rejects any stream asking for more than one replica outright. Production
// LoadConfig defaults LSReplicas to 1 for exactly this class of deployment
// (single-node NATS); this mirrors that here rather than natsutil's
// production LSReplicas-default-3.
// RoutesMaxBytes/RawMaxBytes are bounded because nats-server validates a
// stream's MaxBytes against the FREE SPACE of the JetStream store
// directory's filesystem, not against the account limit -- an unlimited
// account still refuses a stream larger than the disk can hold. The
// production defaults (8 GiB ROUTES, 2 GiB RAW) exceeded what the CI
// runner's temp filesystem had free, so every test that provisioned
// streams failed there with "insufficient storage resources available"
// while passing locally on a bigger disk. Tests publish kilobytes; 16 MiB
// is far above anything they write and fits anywhere.
var e2eStreamOpts = natsutil.StreamOpts{Replicas: 1, LSReplicas: 1,
	RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20}

// TestDaemonEndToEndRealNATS drives real BMP bytes over a real TCP
// connection into a collector.Server backed by a real natsutil.Publisher and
// a real embedded JetStream server, and asserts the resulting envelopes are
// durably readable back off the PEER and ROUTES streams -- not merely that
// some in-process stub recorded them.
func TestDaemonEndToEndRealNATS(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, e2eStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	pub := natsutil.NewPublisher(js)
	srv := collector.NewServer(collector.Config{CollectorID: "e2e-collector"}, pub, time.Now)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(serveCtx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}

	for _, raw := range [][]byte{
		bmptest.Init("rr1", "Arista Networks EOS version 4.30.2F"),
		bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, 65001),
		bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
			NextHop:   netip.MustParseAddr("10.0.0.9"),
		}),
		bmptest.Stats(ph, map[uint32]uint64{0: 42}),
	} {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}

	// Read the PEER stream: the Peer-Up event must land there, durably,
	// published by the real Publisher through the real Server.
	peerCons, err := js.OrderedConsumer(ctx, "PEER", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	peerMsg, err := peerCons.Next()
	if err != nil {
		t.Fatalf("PEER stream: %v", err)
	}
	var peerEnv vantagev1.Envelope
	if err := proto.Unmarshal(peerMsg.Data(), &peerEnv); err != nil {
		t.Fatal(err)
	}
	if peerEnv.GetPeerEvent() == nil || peerEnv.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_UP {
		t.Fatalf("PEER stream envelope is not a Peer-Up: %+v", &peerEnv)
	}
	if peerEnv.CollectorId != "e2e-collector" {
		t.Fatalf("CollectorId = %q, want e2e-collector", peerEnv.CollectorId)
	}
	// The router token comes from the TCP connection's own remote address
	// (127.0.0.1 for a loopback Dial in this test), not from anything inside
	// the BMP payload -- BMP's per-peer header only ever names the *peer*.
	wantPeerSubj := "vantage.v1.peer.7f000001.0a000009"
	if peerMsg.Subject() != wantPeerSubj {
		t.Fatalf("PEER subject = %q, want %q", peerMsg.Subject(), wantPeerSubj)
	}

	// Read the ROUTES stream: the route-monitoring event, on its
	// partition-transformed subject.
	routeCons, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	routeMsg, err := routeCons.Next()
	if err != nil {
		t.Fatalf("ROUTES stream: %v", err)
	}
	var routeEnv vantagev1.Envelope
	if err := proto.Unmarshal(routeMsg.Data(), &routeEnv); err != nil {
		t.Fatal(err)
	}
	if routeEnv.GetRoute() == nil {
		t.Fatalf("ROUTES stream envelope is not a route event: %+v", &routeEnv)
	}
	if routeEnv.Router.GetSysName() != "rr1" {
		t.Fatalf("Router.SysName = %q, want rr1 (Initiation identity must have applied before this event)", routeEnv.Router.GetSysName())
	}
	if len(routeEnv.GetRoute().GetAnnounced()) != 1 || routeEnv.GetRoute().GetAnnounced()[0].Prefix != "192.0.2.0/24" {
		t.Fatalf("unexpected route payload: %+v", routeEnv.GetRoute())
	}

	// Read the STATS stream too, for full message-type coverage.
	statsCons, err := js.OrderedConsumer(ctx, "STATS", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	statsMsg, err := statsCons.Next()
	if err != nil {
		t.Fatalf("STATS stream: %v", err)
	}
	var statsEnv vantagev1.Envelope
	if err := proto.Unmarshal(statsMsg.Data(), &statsEnv); err != nil {
		t.Fatal(err)
	}
	if statsEnv.GetStats() == nil || statsEnv.GetStats().Counters[0] != 42 {
		t.Fatalf("unexpected stats payload: %+v", statsEnv.GetStats())
	}

	// Now exercise real shutdown ordering: cancel the serve context (as
	// SIGINT/SIGTERM would in cmd/vantage-collector), confirm Serve returns
	// (meaning every in-flight session goroutine has finished issuing its
	// Publish calls), and only then Drain -- asserting nil, i.e. every
	// publish this test issued was actually accepted by the server, not just
	// locally queued.
	serveCancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after ctx cancellation")
	}
	conn.Close()

	if err := pub.Drain(10 * time.Second); err != nil {
		t.Fatalf("Drain reported rejected publishes: %v", err)
	}
}

// assertClosedByCollector reads conn until the collector closes it, failing
// if it is still open after 10s.
func assertClosedByCollector(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("read a byte from %s; the collector never writes", what)
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("%s is still open: the collector did not close it", what)
	}
}

// TestDaemonClosesSessionWhenJetStreamRejects is the async half of closing a
// session on a lost event, through the shipped seam: a real natsutil.Publisher
// whose rejection resolves after Publish returned, reaching a real Server by
// type assertion. The PEER stream is replaced under a running collector by one
// that is full under DiscardNew, so the Peer-Up publish is accepted locally and
// rejected by the server later -- the case that used to leave the session up
// with the event gone.
//
// A subject no stream captures is retried for natsutil's backoff window
// before it is reported, because the no-responders answer it gets is also
// what a stream electing a leader gives (see natsutil.retryClass); that
// case is covered in natsutil. A full stream is a rejection the server
// states, and is reported at once.
func TestDaemonClosesSessionWhenJetStreamRejects(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := natsutil.EnsureStreams(ctx, js, e2eStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	if err := js.DeleteStream(ctx, "PEER"); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "PEER", Subjects: []string{"vantage.v1.peer.>"}, MaxMsgs: 1, Discard: jetstream.DiscardNew,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(ctx, "vantage.v1.peer.fill.fill", nil); err != nil {
		t.Fatalf("filling PEER: %v", err)
	}

	srv := collector.NewServer(collector.Config{CollectorID: "e2e-collector"}, natsutil.NewPublisher(js), time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(t.Context(), ln)

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
	assertClosedByCollector(t, conn, "a session whose Peer-Up JetStream rejected")
}

// TestDaemonRefusesSessionsWhileNATSDisconnected: with the publisher's
// connection closed, a new BMP connection is closed before any session opens.
func TestDaemonRefusesSessionsWhileNATSDisconnected(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	srv := collector.NewServer(collector.Config{CollectorID: "e2e-collector"}, natsutil.NewPublisher(js), time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(t.Context(), ln)
	nc.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertClosedByCollector(t, conn, "a connection while NATS is disconnected")
}
