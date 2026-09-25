package main

// This file is the daemon's only automated test: run(ctx, cfgPath) is the
// seam, and it is exercised the way the process really starts -- LoadConfig
// reads a real YAML file, a real embedded NATS/JetStream server stands in
// for production NATS, real BMP bytes cross a real TCP connection, and
// shutdown is driven by canceling ctx exactly as SIGINT/SIGTERM would.
//
// It targets three behaviors that live only in main.go and have no other
// test coverage at all:
//
//  1. cfg.Streams.LSReplicas -> natsutil.StreamOpts.LSReplicas: dropped, the
//     LS stream's create request falls back to StreamOpts.withDefaults'
//     LSReplicas=3, which the single-node embedded server used here (and by
//     a laptop dev deployment) rejects outright -- confirmed empirically by
//     natsutil/streams_test.go's TestNonClusteredServerRejectsReplicasAboveOne,
//     not merely assumed. TestRunProvisionsStreamsAndPublishesEnvelopes
//     asserts the LS stream exists (and at LSReplicas=1, what LoadConfig
//     computes for an unset streams: section) rather than merely that
//     EnsureStreams returned nil.
//  2. jetstream.WithPublishAsyncTimeout(publishAsyncTimeout): dropped, a
//     PublishAsync future that the server never acks (natsutil.Publisher's
//     own doc comment: "a future that is neither acked nor resolved by the
//     client's reconnect path never resolves") never resolves on its own,
//     so the only thing that ever unblocks Drain is Drain's own outer
//     timeout -- and every subsequent Drain would pay that same cost again,
//     for the life of the process, since the leaked await() goroutine keeps
//     Publisher.inflight above zero forever.
//  3. errors.Join(serveErr, drainErr) as run's return value: dropped (e.g.
//     back to an earlier version that discarded Drain's return
//     value), a rejected publish is logged but never reaches the caller, so
//     nothing would ever set the daemon's exit code on data loss.
//
// TestRunSurfacesStuckPublishOnDrain proves behaviors 2 and 3 together
// with one assertion: it deletes the STATS stream out from under a
// running daemon (so a Stats-Report publish's subject is captured by no
// stream at all -- the real-world shape of "the server will never ack
// this") and asserts run's returned error satisfies
// errors.Is(err, jetstream.ErrAsyncPublishTimeout). That specific
// sentinel can only appear in run's return value if BOTH the ack timeout
// fired AND its resulting Drain error was actually propagated out of run;
// reverting either one changes the outcome (see that test's comments for
// exactly how).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	"github.com/jp2195/vantage/subjects"
)

// freeAddr reserves a loopback TCP port by opening and immediately closing a
// listener on ":0", so a config file can name a concrete address before
// run's own net.Listen call claims it. There is a small window between the
// Close here and run's Listen in which another process could steal the
// port; dialWithRetry's retry loop (rather than a single dial attempt right
// after starting run) is what makes that window harmless in practice.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// writeConfig writes a minimal collector.yaml naming natsURL and listenAddr.
// It sets ONLY the two stream byte limits under "streams:", leaving every
// other field in that section unset: LoadConfig's own
// defaulting (cfg.Streams.LSReplicas falls back to cfg.Streams.Replicas,
// which falls back to 1) is what a real single-node laptop deployment
// relies on, and this test's job is to exercise exactly that path rather
// than a hand-tuned config that papers over a regression in it. The sizes are
// the one exception because they are not a defaulting question at all --
// LoadConfig leaves them zero and natsutil fills in the production 8 GiB /
// 2 GiB, which nats-server then refuses on any filesystem with less free
// space than that, because it validates MaxBytes against the store
// directory's actual disk rather than against the account limit. That failed
// the whole daemon on CI before its listener ever bound, which surfaced only
// as "connection refused".
// writeConfig returns the config path and the admin listener address it
// assigned. admin_listen is reserved here for the same reason metrics_listen
// is: left unset it defaults to the real 127.0.0.1:9470, so the test binds a
// port a developer may already be running a collector on, and run() only logs
// the bind failure rather than failing the test.
func writeConfig(t *testing.T, natsURL, listenAddr string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "collector.yaml")
	adminAddr := freeAddr(t)
	body := fmt.Sprintf("listen: %q\nnats_url: %q\ncollector_id: test-collector\nmetrics_listen: %q\nadmin_listen: %q\n"+
		"streams:\n  routes_max_bytes: 16777216\n  raw_max_bytes: 16777216\n",
		listenAddr, natsURL, freeAddr(t), adminAddr)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, adminAddr
}

// dialWithRetry dials addr, retrying until run's own net.Listen has claimed
// it (NATS connect + EnsureStreams both happen before that point) or timeout
// elapses.
func dialWithRetry(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", addr, lastErr)
	return nil
}

// testPeer is the single simulated BMP peer both tests drive traffic
// through: a global-type IPv4 peer with IPv4-unicast + 4-byte-ASN
// capabilities negotiated, matching what a real Peer-Up exchange looks like
// (see cmd/vantage/bmpgen.go's genMessages for the same shape).
func testPeer() (bmp.PeerHeader, bgp.Caps) {
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{
		FourByteAS: true, MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{},
	}
	return ph, caps
}

// waitForStreamMsg blocks (via an ordered consumer, DeliverAllPolicy) until
// stream durably holds at least one message, and returns its unmarshaled
// envelope. Fails the test if none arrives before deadline.
func waitForStreamMsg(t *testing.T, js jetstream.JetStream, stream string, deadline time.Duration) *vantagev1.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cons, err := js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{DeliverPolicy: jetstream.DeliverAllPolicy})
	if err != nil {
		t.Fatalf("%s: ordered consumer: %v", stream, err)
	}
	m, err := cons.Next(jetstream.FetchMaxWait(deadline))
	if err != nil {
		t.Fatalf("%s: waiting for a durable message: %v", stream, err)
	}
	env := &vantagev1.Envelope{}
	if err := proto.Unmarshal(m.Data(), env); err != nil {
		t.Fatalf("%s: unmarshal envelope: %v", stream, err)
	}
	return env
}

// waitForStreamPayload is waitForStreamMsg for a stream that carries more
// than one kind of envelope: it returns the first envelope match accepts.
// STATS carries both stats reports and heartbeats, and the startup beat is
// always the first message on it.
func waitForStreamPayload(t *testing.T, js jetstream.JetStream, stream string,
	match func(*vantagev1.Envelope) bool, deadline time.Duration) *vantagev1.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cons, err := js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{DeliverPolicy: jetstream.DeliverAllPolicy})
	if err != nil {
		t.Fatalf("%s: ordered consumer: %v", stream, err)
	}
	end := time.Now().Add(deadline)
	for {
		remaining := time.Until(end)
		if remaining <= 0 {
			t.Fatalf("%s: no matching message within %v", stream, deadline)
		}
		m, err := cons.Next(jetstream.FetchMaxWait(remaining))
		if err != nil {
			t.Fatalf("%s: no matching message within %v: %v", stream, deadline, err)
		}
		env := &vantagev1.Envelope{}
		if err := proto.Unmarshal(m.Data(), env); err != nil {
			t.Fatalf("%s: unmarshal envelope: %v", stream, err)
		}
		if match(env) {
			return env
		}
	}
}

// TestRunProvisionsStreamsAndPublishesEnvelopes is the happy-path proof: run
// started against a real embedded NATS, fed real BMP traffic over a real
// TCP connection, produces durable envelopes on PEER/ROUTES/STATS, and the
// LS stream -- which nothing in this test's traffic ever publishes to --
// still exists at the replica count LoadConfig computed for an unset
// streams: section. Revert the LSReplicas wiring in main.go's run
// (natsutil.StreamOpts{... /* no LSReplicas field */}) and this test fails:
// StreamOpts.withDefaults falls back to LSReplicas=3, EnsureStreams' LS
// create is rejected outright by this single-node server, run returns an
// error before ever opening the BMP listener, and dialWithRetry below times
// out with nothing to connect to.
func TestRunProvisionsStreamsAndPublishesEnvelopes(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	natsURL := nc.ConnectedUrl()

	listenAddr := freeAddr(t)
	cfgPath, adminAddr := writeConfig(t, natsURL, listenAddr)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfgPath) }()

	conn := dialWithRetry(t, listenAddr, 10*time.Second)
	defer conn.Close()

	// Arm mirror mode for this connection's own source address before writing
	// any BMP, so the messages below are mirrored as they arrive. The proof
	// this is wired to the collector's registry is read out of the RAW stream
	// further down, not out of the admin API that accepted the arm -- a
	// handler holding its own NewMirrorRegistry() answers arm-then-list
	// perfectly well while nothing is ever actually mirrored.
	armMirrorViaAdmin(t, adminAddr, "127.0.0.1")

	ph, caps := testPeer()
	for _, raw := range [][]byte{
		bmptest.Init("test-r1", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, ph.AS),
		bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
			Origin:    0, ASPath: []uint32{ph.AS, 64512}, FourByteAS: true,
			NextHop: ph.Addr,
		}),
		bmptest.Stats(ph, map[uint32]uint64{0: 7}),
	} {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}

	// LSReplicas wiring: the LS stream must have been provisioned at
	// all, and at the replica count LoadConfig's defaulting produces (1) for
	// a config with no streams: section -- not natsutil's own bare-StreamOpts
	// default of 3, which this single-node server would have rejected.
	lsCtx, lsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer lsCancel()
	lsStream, err := js.Stream(lsCtx, "LS")
	if err != nil {
		t.Fatalf("LS stream not provisioned (LSReplicas wiring broken?): %v", err)
	}
	if info, err := lsStream.Info(lsCtx); err != nil {
		t.Fatalf("LS stream info: %v", err)
	} else if info.Config.Replicas != 1 {
		t.Fatalf("LS stream Replicas = %d, want 1 (LoadConfig's default for an unset streams.ls_replicas)", info.Config.Replicas)
	}

	if env := waitForStreamMsg(t, js, "PEER", 10*time.Second); env.GetPeerEvent() == nil {
		t.Fatalf("PEER stream: envelope is not a peer event: %+v", env)
	}
	if env := waitForStreamMsg(t, js, "ROUTES", 10*time.Second); env.GetRoute() == nil {
		t.Fatalf("ROUTES stream: envelope is not a route event: %+v", env)
	}
	// Admin API wiring: the mirror armed above must have been armed on the
	// registry handleConn calls Take on, which shows up as a RawEvent marked
	// mirrored. NewAdminHandler(NewMirrorRegistry()) passes every other
	// assertion in this test and fails here.
	if env := waitForStreamMsg(t, js, "RAW", 10*time.Second); env.GetRaw() == nil || !env.GetRaw().Mirrored {
		t.Fatalf("RAW stream: want a mirrored raw event from the armed router, got %+v", env)
	}

	if env := waitForStreamPayload(t, js, "STATS",
		func(e *vantagev1.Envelope) bool { return e.GetStats() != nil }, 10*time.Second); env.GetStats() == nil {
		t.Fatalf("STATS stream: envelope is not a stats event: %+v", env)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error on clean shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

// TestRunBeatsBeforeItServesAndBelowEverySession is the daemon half of the
// epoch rule.
//
// The first beat goes out at startup, before Serve: BeatInterval is 30 s, so
// a beat seen within 5 s of the listener opening is the startup beat, not a
// tick. And every session this process opens carries an id at or above the
// started_at its beats announce. The read side resolves a session below its
// collector's newest started_at as view_lost, so a session id below its own
// process's start would make a live session read lost.
func TestRunBeatsBeforeItServesAndBelowEverySession(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	listenAddr := freeAddr(t)
	cfgPath, _ := writeConfig(t, nc.ConnectedUrl(), listenAddr)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfgPath) }()

	conn := dialWithRetry(t, listenAddr, 10*time.Second)
	defer conn.Close()
	beat := waitForStreamPayload(t, js, "STATS",
		func(e *vantagev1.Envelope) bool { return e.GetBeat() != nil }, 5*time.Second)
	if beat.GetCollectorId() != "test-collector" {
		t.Fatalf("beat collector_id = %q, want test-collector", beat.GetCollectorId())
	}
	started := beat.GetBeat().GetStartedAt().AsTime()

	ph, caps := testPeer()
	for _, raw := range [][]byte{
		bmptest.Init("test-r1", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, ph.AS),
	} {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	pe := waitForStreamPayload(t, js, "PEER",
		func(e *vantagev1.Envelope) bool { return e.GetPeerEvent() != nil }, 10*time.Second)
	if sid := pe.GetSessionId(); sid < uint64(started.UnixNano()) {
		t.Fatalf("session %d is below its own process's started_at %d (%v): the read side "+
			"would resolve this live session as view_lost", sid, started.UnixNano(), started)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error on clean shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

// TestRunSurfacesStuckPublishOnDrain proves behaviors 2 and 3 together. See
// the file-level doc comment for the full argument; in short: with the STATS
// stream deleted out from under a running daemon, a Stats-Report publish's
// PublishAsync future is captured by no stream and JetStream itself will
// never ack it -- the exact "unresolved future" scenario natsutil.Publisher's
// own doc comment warns about.
//
// Deleting the stream alone is not enough to provoke a genuine hang: with no
// subscriber at all left on the subject, nats-server's core "no responders"
// mechanism fires almost immediately -- confirmed empirically: without
// the black-hole subscriber below,
// jetstream.ErrNoStreamResponse ["no response from stream"] resolves the
// future in well under a second, never touching the ack-timeout path at
// all. A no-responders reply is still a prompt, ordinary resolution, not
// the silent-forever case the ack-timeout guards against. So a black-hole
// subscriber (statsSub below) is added on the exact subject the
// Stats-Report publish will use, to manufacture real "interest" and
// suppress no-responders, while the deleted stream ensures JetStream
// itself still never acks it -- leaving the ack-timeout as the only
// thing that can ever resolve the future.
//
// jetstream.ErrAsyncPublishTimeout can only end up wrapped in run's returned
// error if the ack-timeout fired (wired via
// jetstream.WithPublishAsyncTimeout) AND that Drain error was actually
// joined into run's return. natsutil re-sends a timed-out publish
// until its retry deadline, far past this test's drainTimeout, so the
// timeout reaches run's return through Drain's timeout report, which names
// each publish still retrying with the failure it is retrying; Drain's
// own fallback ("publish drain timed out after ...",
// natsutil/publisher.go) carries no such sentinel, so
// reverting the ack-timeout wiring changes the wrapped error away from
// ErrAsyncPublishTimeout, and reverting the error-joining (e.g. logging
// drainErr but returning only serveErr) makes run's returned error nil
// outright -- either way errors.Is below goes false.
func TestRunSurfacesStuckPublishOnDrain(t *testing.T) {
	// Shrink both to keep a deliberately-provoked stuck publish fast to
	// observe: with the ack-timeout wiring present, the ack timeout (not
	// Drain's own outer timeout) is what resolves the future, so shrinking
	// it is what makes this test take milliseconds instead of the production
	// 30s; shrinking drainTimeout too just bounds how long the test takes
	// if that wiring is reverted and Drain's own fallback timeout is the
	// only thing that ever fires.
	origPublishTimeout, origDrainTimeout := publishAsyncTimeout, drainTimeout
	publishAsyncTimeout, drainTimeout = 300*time.Millisecond, 3*time.Second
	defer func() { publishAsyncTimeout, drainTimeout = origPublishTimeout, origDrainTimeout }()

	nc, js := natstest.RunJSConn(t)
	natsURL := nc.ConnectedUrl()

	listenAddr := freeAddr(t)
	cfgPath, _ := writeConfig(t, natsURL, listenAddr)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfgPath) }()

	conn := dialWithRetry(t, listenAddr, 10*time.Second)
	defer conn.Close()

	// Wait for STATS to exist (proves run's own EnsureStreams has completed)
	// before deleting it -- deleting a stream that was never created would
	// prove nothing.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := js.Stream(waitCtx, "STATS"); err != nil {
		waitCancel()
		t.Fatalf("STATS stream not provisioned: %v", err)
	}
	waitCancel()

	delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := js.DeleteStream(delCtx, "STATS"); err != nil {
		delCancel()
		t.Fatalf("delete STATS stream: %v", err)
	}
	delCancel()

	ph, caps := testPeer()

	// Black-hole subscriber: manufactures core-NATS "interest" on the exact
	// subject the Stats-Report publish below will use, so nats-server's
	// no-responders mechanism does not short-circuit the future quickly (see
	// this test's doc comment). The subject is deterministic: the router
	// token is the dialed TCP connection's own source address (loopback),
	// the peer token is testPeer's address.
	statsSubject := subjects.Stats(subjects.EncodeIP(netip.MustParseAddr("127.0.0.1")), subjects.EncodeIP(ph.Addr))
	statsSub, err := nc.Subscribe(statsSubject, func(*nats.Msg) {})
	if err != nil {
		t.Fatalf("subscribe black hole on %s: %v", statsSubject, err)
	}
	defer statsSub.Unsubscribe()
	for _, raw := range [][]byte{
		bmptest.Init("test-r1", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, ph.AS),
		bmptest.Stats(ph, map[uint32]uint64{0: 1}),
	} {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}

	// Give the session goroutine a moment to have actually issued the Stats
	// publish before shutting down; this is a convenience, not a
	// correctness requirement -- Publish's error is retained on the
	// Publisher regardless of whether Drain is already waiting when it
	// resolves (see natsutil.Publisher.await/takeErrsLocked).
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, jetstream.ErrAsyncPublishTimeout) {
			t.Fatalf("run() returned %v; want an error wrapping jetstream.ErrAsyncPublishTimeout "+
				"(the stuck Stats publish should have timed out and that failure should have reached run's return value)", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("run did not return within the shrunk publishAsyncTimeout+drainTimeout bound; " +
			"WithPublishAsyncTimeout may not be wired into jetstream.New")
	}
}

// armMirrorViaAdmin arms a mirror for router through the admin API, retrying
// until the admin listener is up (run opens it concurrently with everything
// else). It asserts only that the request was accepted; whether the arm
// reached the registry that matters is proved from the RAW stream by the
// caller.
func armMirrorViaAdmin(t *testing.T, adminAddr, router string) {
	t.Helper()
	url := "http://" + adminAddr + "/admin/mirror"
	body := `{"router":"` + router + `","window":"1m","max_bytes":1048576}`

	deadline := time.Now().Add(10 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = http.Post(url, "application/json", strings.NewReader(body))
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("admin listener never came up at %s: %v", adminAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /admin/mirror = %d, want 200", resp.StatusCode)
	}
	var got collector.MirrorStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Router != router {
		t.Fatalf("armed %+v, want router %s", got, router)
	}
}

// TestRunRedactsMalformedNatsURLPassword is the connect-path regression test
// for a credential leak: a nats_url whose password contains a '"'
// -- which net/url.Error.Error() re-escapes when it renders the URL it
// failed to parse, defeating a plain literal-string redaction -- must not
// leak that password into run's returned error, which is exactly what
// reaches this binary's own slog.Error("fatal", ...) and therefore stdout.
// This drives the real run(ctx, cfgPath) seam with a malformed config file,
// not a hand-built error, so it proves redact.CheckURL's early
// return is actually wired into this binary's connect path -- not merely
// present somewhere in the package -- and needs no embedded NATS server: a
// malformed nats_url is rejected before run ever tries to dial anything.
func TestRunRedactsMalformedNatsURLPassword(t *testing.T) {
	const password = `sup"secret`
	natsURL := `nats://user:` + password + `@127.0.0.1:1`
	path, _ := writeConfig(t, natsURL, freeAddr(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, path)
	if err == nil {
		t.Fatal("run with a malformed nats_url returned a nil error, want a rejection")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("run's error %q contains the raw password %q", err.Error(), password)
	}
	// Confirm this took the structural CheckURL short-circuit specifically
	// (nats_url: ...: invalid URL), not merely that some unrelated failure
	// happened to omit the password by coincidence.
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("run's error %q does not mention the expected \"invalid URL\" -- "+
			"did the CheckURL short-circuit not fire?", err.Error())
	}
}

// TestRunRedactsMalformedNatsURLPasswordInMultiServerList is the
// connect-path regression test for the same leak in a multi-server list:
// NATS's documented HA syntax is a comma-separated list of full URLs in one
// nats_url string, and a single-URL check/redaction only ever looked at the
// first one -- a malformed later segment's password reached nats.Connect
// and its own error text unredacted. The bad segment is placed last, the
// position an implementation that only checks the first segment would miss
// entirely.
func TestRunRedactsMalformedNatsURLPasswordInMultiServerList(t *testing.T) {
	const password = `sup"secret`
	natsURL := "nats://user:goodpass@127.0.0.1:1,nats://user:" + password + "@127.0.0.1:2"
	path, _ := writeConfig(t, natsURL, freeAddr(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, path)
	if err == nil {
		t.Fatal("run with a malformed multi-server nats_url returned a nil error, want a rejection")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("run's error %q contains the raw password %q", err.Error(), password)
	}
	if strings.Contains(err.Error(), "goodpass") {
		t.Fatalf("run's error %q contains the first segment's password %q", err.Error(), "goodpass")
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("run's error %q does not mention the expected \"invalid URL\" -- "+
			"did the CheckNatsURL short-circuit not fire?", err.Error())
	}
}

// TestRunRedactsAWellFormedButUnreachableNatsURL is the connect-path
// counterpart to the malformed-URL tests above, and the mirror of
// cmd/vantage-writer/main_test.go's test of the same name: a valid nats_url
// that simply cannot be dialed still produces an error built from
// cfg.NatsURL, and that value renders redacted because it is a
// secret.NatsURL rather than a string.
func TestRunRedactsAWellFormedButUnreachableNatsURL(t *testing.T) {
	const sentinel = "SECRET"
	for _, natsURL := range []string{
		"nats://user:s3cr3t" + sentinel + "@127.0.0.1:1",
		"nats://token" + sentinel + "@127.0.0.1:1",
		"user:s3cr3t" + sentinel + "@127.0.0.1:1",
	} {
		path, _ := writeConfig(t, natsURL, freeAddr(t))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := run(ctx, path)
		cancel()
		if err == nil {
			t.Fatalf("run with an unreachable nats_url %q returned nil, want a dial failure", natsURL)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("run's error %q contains the credential from %q", err.Error(), natsURL)
		}
		if !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("run's error %q does not name the endpoint", err.Error())
		}
	}
}

// TestNewHTTPServerEnforcesTimeouts is a behavioral test, not a field
// assertion: it starts a server built exactly the way run builds both of
// this daemon's listeners, opens a raw TCP connection, sends a partial
// request line and then nothing at all, and requires the server to hang up
// on its own. That is the slow-loris shape -- a client that never finishes
// its headers -- and against a zero-value http.Server the connection and
// its goroutine stay alive indefinitely, which is what the timeouts exist
// to stop.
//
// readHeaderTimeout is shrunk to keep the test fast, the same way
// TestRunReportsStuckPublishes above shrinks publishAsyncTimeout. Shrinking
// it is also what makes the test meaningful: it fails by hanging until the
// deadline below rather than by asserting a struct field is non-zero, so
// dropping the field from newHTTPServer fails it for the real reason.
func TestNewHTTPServerEnforcesTimeouts(t *testing.T) {
	orig := readHeaderTimeout
	readHeaderTimeout = 150 * time.Millisecond
	defer func() { readHeaderTimeout = orig }()

	srv := newHTTPServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for name, got := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	} {
		if got == 0 {
			t.Errorf("newHTTPServer: %s is 0, meaning no bound at all", name)
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A request line with no terminating blank line: the server is still
	// waiting for headers that never come.
	if _, err := conn.Write([]byte("GET /metrics HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}

	// The server must close the connection itself. Read blocks until it
	// does; the deadline is what fails the test if it never does.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	switch _, err := conn.Read(buf); {
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
		// Server hung up on the stalled client, which is the point.
	case err == nil:
		// A 408 Request Timeout body before the close is equally fine --
		// the server acted on the timeout either way.
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Fatal("connection still open 5s after a stalled request with a " +
			"150ms ReadHeaderTimeout: the server is not enforcing it, so a " +
			"client that never finishes its headers holds a goroutine forever")
	default:
		t.Fatalf("unexpected read error: %v", err)
	}
}

// TestRunKeepsSessionsAcrossANATSReconnect is the lame-duck NATS restart,
// end to end. The daemon's NATS connection runs through a natstest.Proxy,
// which holds the acks for one batch of route events (stored, ack lost) and
// the publishes of a second (never stored), then drops the connection. The
// NATS client fails all of them with nats.ErrDisconnected at once. Before
// the in-flight retry, that closed the BMP session and bumped
// sessions_aborted_total; now each is re-sent after the reconnect, the
// stream holds every event exactly once, the session stays open and keeps
// publishing, and publish_retries_total counts the re-sends.
func TestRunKeepsSessionsAcrossANATSReconnect(t *testing.T) {
	srv := natstest.RunServer(t)
	proxy := natstest.NewProxy(t, srv.Addr().String())
	dnc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer dnc.Close()
	direct, err := jetstream.New(dnc)
	if err != nil {
		t.Fatal(err)
	}

	listenAddr := freeAddr(t)
	cfgPath, _ := writeConfig(t, proxy.URL(), listenAddr)
	abortedBefore := counterValue(t, "vantage_collector_sessions_aborted_total")
	retriesBefore := counterValue(t, "vantage_collector_publish_retries_total")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfgPath) }()

	conn := dialWithRetry(t, listenAddr, 10*time.Second)
	defer conn.Close()

	ph, caps := testPeer()
	send := func(raw []byte) {
		t.Helper()
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	next := 0
	sendRoutes := func(n int) {
		t.Helper()
		for range n {
			next++
			send(bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				Announced: []bgp.Prefix{{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 18, byte(next >> 8), byte(next)}), 32)}},
				ASPath:    []uint32{ph.AS}, FourByteAS: true, NextHop: ph.Addr,
			}))
		}
	}
	send(bmptest.Init("test-r1", "Cisco IOS XR Software, Version 7.9.2"))
	send(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, ph.AS))
	sendRoutes(10)
	waitStreamCount(t, direct, "ROUTES", 10)

	proxy.FreezeToClient()
	sendRoutes(10)
	waitStreamCount(t, direct, "ROUTES", 20) // stored; acks held in the proxy
	proxy.FreezeToServer()
	sendRoutes(10) // held in the proxy; never stored
	time.Sleep(100 * time.Millisecond)
	if got := streamCount(t, direct, "ROUTES"); got != 20 {
		t.Fatalf("ROUTES holds %d before the cut, want 20", got)
	}
	proxy.Sever()

	waitStreamCount(t, direct, "ROUTES", 30)
	// The session survived: it still reads and publishes...
	sendRoutes(1)
	waitStreamCount(t, direct, "ROUTES", 31)
	// ...and the collector has not closed it. It never writes, so a read
	// that times out is an open connection and anything else is a close.
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read on the BMP connection = %v; want a timeout, i.e. the session still open", err)
	}
	if got := counterValue(t, "vantage_collector_sessions_aborted_total") - abortedBefore; got != 0 {
		t.Fatalf("sessions_aborted_total rose by %v across the reconnect, want 0", got)
	}
	if got := counterValue(t, "vantage_collector_publish_retries_total") - retriesBefore; got < 20 {
		t.Fatalf("publish_retries_total rose by %v, want at least the 20 publishes in flight at the cut", got)
	}
	if ids := streamMsgIDs(t, direct, "ROUTES"); len(ids) != 31 {
		t.Fatalf("ROUTES holds %d distinct msg-ids, want 31 (each event exactly once)", len(ids))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned %v on shutdown; every publish should have resolved", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

// TestRunBeatsPromptlyAfterANATSReconnect: when the NATS connection comes
// back, a beat goes out within seconds, not at the next 30 s tick. Beats are
// dropped rather than retried, so one lost to the outage would otherwise
// stretch the gap between beats to the outage plus up to one more interval.
//
// The beat subject is watched on a direct connection that the cut does not
// touch. Both sides: nothing but the startup beat arrives before the cut,
// and a beat arrives within reconnectBeatBound after it -- far less than
// collector.BeatInterval, so it cannot be the ticker's.
func TestRunBeatsPromptlyAfterANATSReconnect(t *testing.T) {
	const reconnectBeatBound = 10 * time.Second
	if reconnectBeatBound >= collector.BeatInterval {
		t.Fatalf("the bound %v does not separate a reconnect beat from a tick at %v",
			reconnectBeatBound, collector.BeatInterval)
	}
	srv := natstest.RunServer(t)
	proxy := natstest.NewProxy(t, srv.Addr().String())
	dnc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer dnc.Close()
	beats, err := dnc.SubscribeSync(subjects.Beat("test-collector"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dnc.Flush(); err != nil {
		t.Fatal(err)
	}

	listenAddr := freeAddr(t)
	cfgPath, _ := writeConfig(t, proxy.URL(), listenAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfgPath) }()

	conn := dialWithRetry(t, listenAddr, 10*time.Second)
	defer conn.Close()
	if _, err := beats.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("no startup beat: %v", err)
	}
	if m, err := beats.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("a second beat before any reconnect (%d bytes): the test cannot tell "+
			"a reconnect beat from it", len(m.Data))
	}

	cut := time.Now()
	proxy.Sever()
	if _, err := beats.NextMsg(reconnectBeatBound); err != nil {
		t.Fatalf("no beat within %v of the NATS connection being cut and restored "+
			"(the next tick is %v away): %v", reconnectBeatBound, collector.BeatInterval, err)
	}
	t.Logf("reconnect beat %v after the cut", time.Since(cut).Round(time.Millisecond))

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned %v on shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

// counterValue reads a counter from the default Prometheus registry, where
// the collector package's promauto metrics live. It is process-wide, so
// callers compare before and after.
func counterValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			var v float64
			for _, m := range mf.GetMetric() {
				v += m.GetCounter().GetValue()
			}
			return v
		}
	}
	t.Fatalf("metric %s not registered", name)
	return 0
}

// vecCounterValue is counterValue for a counter vector, which the registry
// does not report at all until one of its label sets has been used: absent
// reads as 0.
func vecCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var v float64
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				v += m.GetCounter().GetValue()
			}
		}
	}
	return v
}

func streamCount(t *testing.T, js jetstream.JetStream, stream string) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

func waitStreamCount(t *testing.T, js jetstream.JetStream, stream string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got := streamCount(t, js, stream)
		if got == want {
			return
		}
		if got > want || time.Now().After(deadline) {
			t.Fatalf("%s holds %d messages, want %d", stream, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// streamMsgIDs returns the distinct Nats-Msg-Id values in stream, failing on
// any that appears twice.
func streamMsgIDs(t *testing.T, js jetstream.JetStream, stream string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := streamCount(t, js, stream)
	cons, err := js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for range n {
		m, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		id := m.Headers().Get(jetstream.MsgIDHeader)
		if ids[id] {
			t.Fatalf("msg-id %s stored twice in %s", id, stream)
		}
		ids[id] = true
	}
	return ids
}

// TestRunShutsDownPromptlyWithARetryBudgetFull: with ROUTES deleted, every
// route publish retries no-responders, and a session fed more route
// updates than the 4096-publish budget holds ends up waiting for room --
// 10s, far past this test's bound. Shutdown must end that wait at once
// (run wires ctx to Publisher.StopWaiting); otherwise Serve waits for the
// session, and a pod's grace period can run out before Drain reports.
//
// The signal that the session is waiting is events_published_total: the
// collector counts an event before handing it to the publisher, so the
// counter stops, short of what was sent, when the publisher holds a
// session's goroutine. publish_retries_total counts re-sends, not the
// budget, and keeps climbing throughout.
func TestRunShutsDownPromptlyWithARetryBudgetFull(t *testing.T) {
	origDrain := drainTimeout
	drainTimeout = time.Second
	defer func() { drainTimeout = origDrain }()

	nc, js := natstest.RunJSConn(t)
	listenAddr := freeAddr(t)
	cfgPath, _ := writeConfig(t, nc.ConnectedUrl(), listenAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, cfgPath) }()
	conn := dialWithRetry(t, listenAddr, 10*time.Second)
	defer conn.Close()
	delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer delCancel()
	if _, err := js.Stream(delCtx, "ROUTES"); err != nil {
		t.Fatalf("ROUTES not provisioned: %v", err)
	}
	if err := js.DeleteStream(delCtx, "ROUTES"); err != nil {
		t.Fatal(err)
	}

	const updates = 30000
	eventsBefore := vecCounterValue(t, "vantage_collector_events_published_total")
	ph, caps := testPeer()
	go func() {
		// Writes block once the collector stops reading; closing conn at
		// the end of the test unblocks them.
		conn.Write(bmptest.Init("test-r1", "Cisco IOS XR Software, Version 7.9.2"))
		conn.Write(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, ph.AS))
		for i := range updates {
			conn.Write(bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
				Announced: []bgp.Prefix{{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{198, byte(18 + i>>16), byte(i >> 8), byte(i)}), 32)}},
				ASPath:    []uint32{ph.AS}, FourByteAS: true, NextHop: ph.Addr,
			}))
		}
	}()

	// Waiting: the counter has passed the budget, stopped short of
	// everything sent, and held still.
	deadline := time.Now().Add(20 * time.Second)
	last, stillSince := -1.0, time.Now()
	for {
		n := vecCounterValue(t, "vantage_collector_events_published_total") - eventsBefore
		if n != last {
			last, stillSince = n, time.Now()
		} else if n > 4096 && n < updates && time.Since(stillSince) > 500*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never stopped at a full retry budget: %v events published", n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("session waiting after %v events", last)
	start := time.Now()
	cancel()
	select {
	case <-errCh:
		t.Logf("run returned %v after cancellation", time.Since(start))
		if elapsed := time.Since(start); elapsed > drainTimeout+2*time.Second {
			t.Fatalf("run took %v to shut down; want at most drainTimeout (%v) + 2s", elapsed, drainTimeout)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return within 30s of cancellation")
	}
}
