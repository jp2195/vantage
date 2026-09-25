package collector

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

type capturePub struct {
	mu  sync.Mutex
	evs []Event
}

func (c *capturePub) Publish(ev Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
	return nil
}

func (c *capturePub) get() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event{}, c.evs...)
}

// failingPub always rejects, so tests can assert publish errors are counted
// and surfaced rather than silently swallowed.
type failingPub struct {
	mu    sync.Mutex
	calls int
}

func (f *failingPub) Publish(ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return errors.New("boom: nats unavailable")
}

func (f *failingPub) get() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestServerEndToEnd(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

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
	} {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		if evs := pub.get(); len(evs) == 2 {
			if evs[0].Env.GetPeerEvent() == nil || evs[1].Env.GetRoute() == nil {
				t.Fatalf("wrong payloads: %+v", evs)
			}
			if evs[1].Env.Router.SysName != "rr1" {
				t.Fatal("init identity not applied")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout; got %d events", len(pub.get()))
		case <-time.After(20 * time.Millisecond):
		}
	}
	conn.Close()
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil || cfg.Listen != ":11019" || cfg.MetricsListen != ":9469" || cfg.CollectorID == "" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	// natsutil.StreamOpts.LSReplicas defaults to 3 when unset,
	// which a single-node NATS server (the common dev/laptop deployment)
	// rejects outright. LoadConfig's default must not leave this at zero.
	if cfg.Streams.LSReplicas != 1 {
		t.Fatalf("cfg.Streams.LSReplicas = %d, want 1 (single-node-safe default)", cfg.Streams.LSReplicas)
	}
}

// dial connects to ln and returns the conn, failing the test on error.
func dial(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// TestRapidReconnectsGetDistinctSessionIDs is the direct test for the
// adversarial question: "a router that reconnects rapidly -- does each
// connection get a distinct sessionID?" A frozen clock is used
// deliberately: with a real time.Now, collisions are rare enough that a test
// using it would mostly prove nothing. Frozen at one instant, every
// connection's naive now().UnixNano() would be identical, so this only
// passes if Server actually guards against that (see nextSessionID).
func TestRapidReconnectsGetDistinctSessionIDs(t *testing.T) {
	frozen := time.Unix(1753600000, 0)
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, func() time.Time { return frozen })

	const n = 200
	var wg sync.WaitGroup
	ids := make([]uint64, n)
	var mu sync.Mutex
	seen := map[uint64]int{}
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := srv.nextSessionID()
			ids[i] = id
			mu.Lock()
			seen[id]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("got %d distinct session IDs across %d calls with a frozen clock, want %d", len(seen), n, n)
	}
	for id, count := range seen {
		if count > 1 {
			t.Fatalf("session id %d issued %d times", id, count)
		}
	}
}

// TestConcurrentConnectionsGetDistinctSessionIDsOverTheWire is the same
// property, but end to end: real TCP connections accepted concurrently by
// Serve, each asserted (via its Peer-Up event's Env.SessionId) to have a
// distinct session ID. Uses a real router IP per connection would require
// distinct source ports, which the OS already guarantees for concurrent
// Dials from the same box, so distinct sessionID here can only come from
// Server's own bookkeeping, not from distinct router identity accidentally
// making msg-ids differ some other way.
func TestConcurrentConnectionsGetDistinctSessionIDsOverTheWire(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			conn := dial(t, ln)
			defer conn.Close()
			if _, err := conn.Write(bmptest.Init("rr", "Arista Networks EOS version 4.30.2F")); err != nil {
				t.Error(err)
				return
			}
			ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
				AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
			caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
			if _, err := conn.Write(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, 65001)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	// Each connection now publishes TWO events under its own session id: the
	// Peer-Up, and the view-lost its close emits (see Session.Close). The
	// property under test is that no two CONNECTIONS share a session id, so
	// it is counted over the Peer-Ups -- exactly one per connection --
	// rather than over every event, which would now report a connection's
	// own second event as a collision.
	peerUps := func() []Event {
		var out []Event
		for _, ev := range pub.get() {
			if ev.Env.GetPeerEvent().GetKind() == vantagev1.PeerEvent_KIND_UP {
				out = append(out, ev)
			}
		}
		return out
	}
	deadline := time.After(5 * time.Second)
	for {
		if len(peerUps()) >= n {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %d peer-up events, got %d", n, len(peerUps()))
		case <-time.After(20 * time.Millisecond):
		}
	}

	seen := map[uint64]bool{}
	for _, ev := range peerUps() {
		id := ev.Env.SessionId
		if seen[id] {
			t.Fatalf("session id %d observed more than once across concurrent connections", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct session ids across %d connections", len(seen), n)
	}
}

// TestIdleConnectionIsClosed proves handleConn does not leak a goroutine
// forever for a router that connects and then sends nothing at all: with
// idleReadTimeout set short (a white-box override; production uses
// defaultIdleReadTimeout), the connection must be closed on the server side
// once the deadline elapses.
func TestIdleConnectionIsClosed(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.idleReadTimeout = 100 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	conn := dial(t, ln)
	defer conn.Close()
	// Send nothing. The server side must close its end within a bounded
	// time once the idle read deadline elapses; a read on our end should
	// then observe EOF rather than blocking forever.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected the server to close an idle connection, got a successful read")
	}
}

// TestSyncPublishFailureClosesTheSession: an event the publisher refused is
// gone -- the router has moved on and will not send it again -- so the session
// is closed rather than left running with a hole in it. The router reconnects
// and re-dumps; a session that stayed up would leave the archive wrong until
// it happened to reset.
func TestSyncPublishFailureClosesTheSession(t *testing.T) {
	pub := &failingPub{}
	_, ln := serveCfg(t, Config{}, pub)
	errsBefore := plainCounterValue(t, "vantage_collector_publish_errors_total")
	abortsBefore := plainCounterValue(t, "vantage_collector_sessions_aborted_total")

	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	assertCollectorClosed(t, conn, "a session whose publish failed")

	if got := plainCounterValue(t, "vantage_collector_publish_errors_total") - errsBefore; got < 1 {
		t.Fatalf("publish errors moved by %v, want >= 1", got)
	}
	if got := plainCounterValue(t, "vantage_collector_sessions_aborted_total") - abortsBefore; got != 1 {
		t.Fatalf("sessions aborted moved by %v, want 1", got)
	}
}

// notifyPub accepts every publish synchronously and keeps each one's failure
// callback, so a test can play the server rejecting a publish after Publish
// returned -- the way natsutil.Publisher reports JetStream rejections and
// async timeouts.
type notifyPub struct {
	capturePub
	cbMu sync.Mutex
	cbs  []func(error)
}

func (n *notifyPub) PublishNotify(ev Event, onFail func(error)) error {
	n.cbMu.Lock()
	n.cbs = append(n.cbs, onFail)
	n.cbMu.Unlock()
	return n.capturePub.Publish(ev)
}

// reject reports the i'th publish as failed.
func (n *notifyPub) reject(t *testing.T, i int) {
	t.Helper()
	n.cbMu.Lock()
	cb := n.cbs[i]
	n.cbMu.Unlock()
	if cb == nil {
		t.Fatalf("publish %d carried no failure callback", i)
	}
	cb(errors.New("boom: nats: timeout"))
}

// TestAsyncPublishFailureClosesOnlyThatSession: a rejection that resolves
// after Publish returned is attributed to the session that produced the event
// and closes that session, and only that one. The second session is the other
// side: a server that closed everything on any failure would fail here.
func TestAsyncPublishFailureClosesOnlyThatSession(t *testing.T) {
	pub := &notifyPub{}
	_, ln := serveCfg(t, Config{}, pub)

	a := dial(t, ln)
	defer a.Close()
	writeSession(t, a, nil)
	waitEvents(t, &pub.capturePub, 2)
	b := dial(t, ln)
	defer b.Close()
	writeSession(t, b, nil)
	waitEvents(t, &pub.capturePub, 4)

	pub.reject(t, 0) // a's Peer-Up
	assertCollectorClosed(t, a, "the session whose publish was rejected")

	if err := b.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the other session was disturbed by a failure that was not its own: read err = %v", err)
	}
}

// TestPublishFailureLogIsRateLimited: during an outage every in-flight
// publish fails, thousands a second. One log line per failure would bury
// everything else, so a session logs its first failure and then at most one
// line per interval carrying a count.
func TestPublishFailureLogIsRateLimited(t *testing.T) {
	pub := &notifyPub{}
	var logs syncBuffer
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.log = slog.New(slog.NewTextHandler(&logs, nil))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)

	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	waitEvents(t, &pub.capturePub, 2)
	for range 200 {
		pub.reject(t, 1)
	}
	assertCollectorClosed(t, conn, "the session whose publishes failed")
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "bmp session closed") {
		if time.Now().After(deadline) {
			t.Fatalf("session never logged its close:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Count(logs.String(), publishFailedMsg); got < 1 || got > 2 {
		t.Fatalf("%d publish-failure lines for 200 failures, want 1 or 2:\n%s", got, logs.String())
	}
}

// TestRateLimitedLogCounts pins the arithmetic the server test above cannot:
// every failure is accounted for in exactly one line, suppressed ones carry
// their count, and the interval re-opens the gate.
func TestRateLimitedLogCounts(t *testing.T) {
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Unix(1753600000, 0)
	r := &rateLimitedLog{msg: "thing failed", level: slog.LevelError, interval: 10 * time.Second,
		now: func() time.Time { return now }}

	for range 100 {
		r.record(log, "err", "e")
	}
	if got := strings.Count(logs.String(), "thing failed"); got != 1 {
		t.Fatalf("%d lines for 100 failures inside one interval, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "count=1 ") {
		t.Fatalf("the first line should account for one failure:\n%s", logs.String())
	}
	now = now.Add(10 * time.Second)
	r.record(log, "err", "e")
	if !strings.Contains(logs.String(), "count=100 ") {
		t.Fatalf("the line after the interval should carry the 99 suppressed plus itself:\n%s", logs.String())
	}
	r.record(log, "err", "e")
	r.record(log, "err", "e")
	r.flush(log)
	if !strings.Contains(logs.String(), "count=2 ") {
		t.Fatalf("flush should report the 2 still pending:\n%s", logs.String())
	}
	before := logs.String()
	r.flush(log)
	if logs.String() != before {
		t.Fatalf("flush with nothing pending wrote a line:\n%s", logs.String())
	}
}

// TestBurstLogReportsTheBurst: a burst of occurrences is reported as one line
// carrying the whole burst's count, not as a line for its first member.
// Later occurrences wait for the interval, and flush writes what is pending.
func TestBurstLogReportsTheBurst(t *testing.T) {
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	b := &burstLog{msg: "thing retried", level: slog.LevelWarn,
		window: 50 * time.Millisecond, interval: 300 * time.Millisecond}

	for range 100 {
		b.record(log, "err", "e")
	}
	if logs.String() != "" {
		t.Fatalf("a line was written before the window closed:\n%s", logs.String())
	}
	waitFor(t, func() bool { return strings.Contains(logs.String(), "thing retried") })
	if !strings.Contains(logs.String(), "count=100 ") {
		t.Fatalf("the first line should account for the whole burst of 100:\n%s", logs.String())
	}

	b.record(log, "err", "e")
	time.Sleep(100 * time.Millisecond) // past the window, inside the interval
	if got := strings.Count(logs.String(), "thing retried"); got != 1 {
		t.Fatalf("%d lines inside the interval, want 1:\n%s", got, logs.String())
	}
	b.record(log, "err", "e")
	b.flush()
	if got := strings.Count(logs.String(), "thing retried"); got != 2 || !strings.Contains(logs.String(), "count=2 ") {
		t.Fatalf("flush should report the 2 pending in a second line:\n%s", logs.String())
	}
	before := logs.String()
	b.flush()
	time.Sleep(400 * time.Millisecond)
	if logs.String() != before {
		t.Fatalf("a line was written with nothing pending:\n%s", logs.String())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLateFailureAfterSessionEndAbortsNothing: a publish can resolve as failed
// after its session already ended on its own. That is still logged, but it is
// not a session the collector closed, and must not be counted as one.
func TestLateFailureAfterSessionEndAbortsNothing(t *testing.T) {
	pub := &notifyPub{}
	var logs syncBuffer
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.log = slog.New(slog.NewTextHandler(&logs, nil))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx, ln)

	conn := dial(t, ln)
	writeSession(t, conn, nil)
	waitEvents(t, &pub.capturePub, 2)
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "bmp session closed") {
		if time.Now().After(deadline) {
			t.Fatalf("session never logged its close:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	before := plainCounterValue(t, "vantage_collector_sessions_aborted_total")
	pub.reject(t, 1)
	if got := plainCounterValue(t, "vantage_collector_sessions_aborted_total") - before; got != 0 {
		t.Fatalf("sessions aborted moved by %v for a session that had already ended, want 0", got)
	}
	if !strings.Contains(logs.String(), publishFailedMsg) {
		t.Fatalf("the late failure was not logged:\n%s", logs.String())
	}
}

// gatedPub reports a NATS connection state the test controls.
type gatedPub struct {
	capturePub
	up atomic.Bool
}

func (g *gatedPub) Connected() bool { return g.up.Load() }

// TestNewSessionRefusedWhileNATSDisconnected: a session opened while NATS is
// down would stream its whole RIB dump into a reconnect buffer that overflows
// in seconds. The collector closes it at once instead, and the router retries
// on its own timer. The second connection, after NATS returns, is the other
// side: a gate that never reopened fails there.
func TestNewSessionRefusedWhileNATSDisconnected(t *testing.T) {
	pub := &gatedPub{}
	_, ln := serveCfg(t, Config{}, pub)

	before := rejectedCount(t, rejectNATSDisconnected)
	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	assertCollectorClosed(t, conn, "a connection while NATS is down")
	if got := rejectedCount(t, rejectNATSDisconnected) - before; got != 1 {
		t.Fatalf("nats_disconnected moved by %v, want 1", got)
	}
	if n := len(pub.get()); n != 0 {
		t.Fatalf("%d events published from a refused connection, want 0", n)
	}

	pub.up.Store(true)
	ok := dial(t, ln)
	defer ok.Close()
	writeSession(t, ok, nil)
	waitEvents(t, &pub.capturePub, 2)
}

// plainCounterValue reads an unlabeled counter from the default registry.
func plainCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestBadConnectionDoesNotAffectOthers is the rule that one router
// misbehaving must not affect others, exercised directly: one connection
// sends a garbage BMP version byte (an unrecoverable framing error per
// bmp.ReadMsg's contract) while a second, well-behaved connection on the
// same server keeps working.
func TestBadConnectionDoesNotAffectOthers(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	bad := dial(t, ln)
	defer bad.Close()
	if _, err := bad.Write([]byte{0xFF, 0, 0, 0, 6, 4}); err != nil {
		t.Fatal(err)
	}

	good := dial(t, ln)
	defer good.Close()
	if _, err := good.Write(bmptest.Init("rr1", "Arista Networks EOS version 4.30.2F")); err != nil {
		t.Fatal(err)
	}
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
	if _, err := good.Write(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, 65001)); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if len(pub.get()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout; the good connection's event never arrived (bad connection may have broken the server)")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// blockingPub blocks inside Publish until release is closed, then delegates
// to capturePub. It exists to put a handleConn goroutine into a known,
// observable "still doing work" state, so a test can assert Serve does not
// return while that goroutine is in it.
type blockingPub struct {
	capturePub
	entered chan struct{}
	release chan struct{}
}

func newBlockingPub() *blockingPub {
	return &blockingPub{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (b *blockingPub) Publish(ev Event) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.capturePub.Publish(ev)
}

// TestShutdownWaitsForInFlightConnections proves Serve does not return until
// every connection goroutine it started has itself returned -- specifically,
// that a connection goroutine still blocked inside a Publish call (standing
// in for a slow/backed-up NATS) delays Serve's return. A naive Serve
// returns as soon as the accept loop stops, racing against
// still-running handleConn goroutines that might Publish something a
// caller's immediately-following Drain would never see; this test fails
// against that version (Serve returns while entered but before release is
// closed) and passes against the WaitGroup-based fix.
func TestShutdownWaitsForInFlightConnections(t *testing.T) {
	pub := newBlockingPub()
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	conn := dial(t, ln)
	defer conn.Close()

	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, ln) }()

	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
	if _, err := conn.Write(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, 65001)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-pub.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConn never reached Publish")
	}

	// The connection's goroutine is now blocked inside Publish. Trigger
	// shutdown and confirm Serve does NOT return while that's still true.
	cancel()
	select {
	case <-serveDone:
		t.Fatal("Serve returned while a connection goroutine was still blocked in Publish")
	case <-time.After(200 * time.Millisecond):
	}

	close(pub.release)
	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the in-flight Publish completed")
	}
}

// TestQuietSessionSurvivesPastTheHandshakeDeadline is the counterpart to
// TestIdleConnectionIsClosed. The deadline must bound only the wait for a
// connection's *first* message; applying it per-read tore down healthy
// sessions, because BMP feeds are hours-long and legitimately silent between
// events (a stable peer set with stats reporting off sends nothing for long
// stretches). When that happened the router's next update was written into a
// closed socket and lost with no error at either end, and every cycle forced
// a new sessionID and a full RIB re-dump. BMP sessions are hours-long and
// must not be idle-killed.
func TestQuietSessionSurvivesPastTheHandshakeDeadline(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.idleReadTimeout = 100 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	conn := dial(t, ln)
	defer conn.Close()

	// First message arrives promptly, which is what the deadline bounds.
	if _, err := conn.Write(bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2")); err != nil {
		t.Fatal(err)
	}

	// Now go quiet for well past the deadline, as a healthy session does.
	time.Sleep(400 * time.Millisecond)

	// The session must still be alive and still delivering. A Peer-Up sent
	// now has to produce an event; under a per-read deadline the server had
	// already closed this connection and this update vanished.
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	if _, err := conn.Write(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"),
		179, 33001, bgp.Caps{}, bgp.Caps{}, 65000, 65001)); err != nil {
		t.Fatalf("write after idle period: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pub.get()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no event after a quiet period: the session was idle-killed and the update was lost")
}

// TestServerPublishesViewLostWhenTheTransportDrops is the server-level half
// of Session.Close: the defer that actually runs it, and the publish that
// gets the event out. Without it the collector holds a correct view-lost
// event in memory and drops it on the floor, which from the archive's side
// is indistinguishable from never having computed one.
func TestServerPublishesViewLostWhenTheTransportDrops(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	conn := dial(t, ln)
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
	if _, err := conn.Write(bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"),
		179, 33001, caps, caps, 65000, 65001)); err != nil {
		t.Fatal(err)
	}
	waitEvents(t, pub, 1)

	// The router says nothing on the way out -- it cannot; BMP is
	// unidirectional. This is the whole failure mode: a drop with no PeerDown
	// in front of it.
	conn.Close()

	evs := waitEvents(t, pub, 2)
	pe := evs[1].Env.GetPeerEvent()
	if pe.GetKind() != vantagev1.PeerEvent_KIND_VIEW_LOST {
		t.Fatalf("second event kind = %v, want KIND_VIEW_LOST", pe.GetKind())
	}
	if evs[1].Env.SessionId != evs[0].Env.SessionId {
		t.Fatalf("view-lost landed on session %d, the Peer-Up on %d; it has to close "+
			"out the session it belongs to or it displaces nothing",
			evs[1].Env.SessionId, evs[0].Env.SessionId)
	}
	if got := evs[1].Env.GetPeer().GetIp(); got != "10.0.0.9" {
		t.Fatalf("view-lost peer = %q, want 10.0.0.9", got)
	}
}

// waitEvents blocks until pub has seen at least n events, returning them.
func waitEvents(t *testing.T, pub *capturePub, n int) []Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if evs := pub.get(); len(evs) >= n {
			return evs
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %d events; got %d", n, len(pub.get()))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// proxyV2 builds an AF_INET PROXY protocol v2 header naming src as the client.
func proxyV2(src netip.Addr) []byte {
	h := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A, 0x21, 0x11}
	body := src.As4()
	dst := netip.MustParseAddr("10.0.0.1").As4()
	b := append(append([]byte{}, body[:]...), dst[:]...)
	b = binary.BigEndian.AppendUint16(b, 54321)
	b = binary.BigEndian.AppendUint16(b, 11019)
	h = binary.BigEndian.AppendUint16(h, uint16(len(b)))
	return append(h, b...)
}

// proxyV2Mapped builds an AF_INET6 header carrying a v4-mapped client, which
// is what a proxy may hand us for an IPv4 router.
func proxyV2Mapped(src netip.Addr) []byte {
	h := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A, 0x21, 0x21}
	s := src.As16()
	d := netip.MustParseAddr("::ffff:10.0.0.1").As16()
	b := append(append([]byte{}, s[:]...), d[:]...)
	b = binary.BigEndian.AppendUint16(b, 54321)
	b = binary.BigEndian.AppendUint16(b, 11019)
	h = binary.BigEndian.AppendUint16(h, uint16(len(b)))
	return append(h, b...)
}

// bmpSession is the three messages every test below writes once it has (or
// has deliberately not) sent a header.
func bmpSession(t *testing.T) [][]byte {
	t.Helper()
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	caps := bgp.Caps{MP: map[bgp.Family]bool{bgp.FamilyIPv4U: true}, AddPathRecv: map[bgp.Family]bool{}}
	return [][]byte{
		bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.11.1"),
		bmptest.PeerUp(ph, netip.MustParseAddr("10.0.0.1"), 179, 33001, caps, caps, 65000, 65001),
		bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
			Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
			NextHop:   netip.MustParseAddr("10.0.0.9"),
		}),
	}
}

// serveProxy starts a server with proxy_protocol required and returns its
// listener.
func serveProxy(t *testing.T, cfg Config, pub EventPublisher) net.Listener {
	t.Helper()
	cfg.ProxyProtocol = ProxyProtocolRequired
	if cfg.CollectorID == "" {
		cfg.CollectorID = "c1"
	}
	srv := NewServer(cfg, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return ln
}

// TestProxyProtocolIdentityComesFromTheHeader is the whole point of the
// feature: the router is 10.0.0.80 because the header said so, not 127.0.0.1
// because that is who opened the socket.
func TestProxyProtocolIdentityComesFromTheHeader(t *testing.T) {
	pub := &capturePub{}
	ln := serveProxy(t, Config{}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	if _, err := conn.Write(proxyV2(netip.MustParseAddr("10.0.0.80"))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range bmpSession(t) {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	evs := waitEvents(t, pub, 2)
	for i, ev := range evs {
		if got := ev.Env.GetRouter().GetIp(); got != "10.0.0.80" {
			t.Fatalf("event %d router ip = %q, want 10.0.0.80", i, got)
		}
	}
}

// TestProxyProtocolRejectsAConnectionWithNoHeader is the no-fallback rule,
// the central claim of this feature. Deleting the required check makes this
// the test that fails: without it the connection would be accepted and filed
// under the socket address.
//
// The close assertion comes first and carries the weight. "No events were
// published" is an absence, and an absence checked after a fixed sleep passes
// on a loaded machine whether or not the rule exists -- the events would
// simply not have arrived yet. Waiting for the collector to hang up is
// waiting for its actual decision, and only then is the absence meaningful.
func TestProxyProtocolRejectsAConnectionWithNoHeader(t *testing.T) {
	pub := &capturePub{}
	ln := serveProxy(t, Config{}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	for _, raw := range bmpSession(t) {
		// Writes may fail once the collector closes; that is the expected
		// outcome, not a test failure.
		_, _ = conn.Write(raw)
	}
	assertCollectorClosed(t, conn, "a connection that sent raw BMP instead of a header")
	for _, ev := range pub.get() {
		t.Fatalf("a connection with no PROXY header published %+v", ev.Env.GetRouter())
	}
}

// TestProxyProtocolDoesNotOverReadTheBMPStream writes the header and the whole
// BMP session as a single payload, so any byte the header parser buffers past
// its own end is a byte bmp.ReadMsg never sees. The Initiation carries the
// sysName, so losing its first bytes shows up as an identity that never
// arrives.
func TestProxyProtocolDoesNotOverReadTheBMPStream(t *testing.T) {
	pub := &capturePub{}
	ln := serveProxy(t, Config{}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	payload := proxyV2(netip.MustParseAddr("10.0.0.80"))
	for _, raw := range bmpSession(t) {
		payload = append(payload, raw...)
	}
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	evs := waitEvents(t, pub, 2)
	if got := evs[1].Env.GetRouter().GetSysName(); got != "rr1" {
		t.Fatalf("sys_name = %q, want rr1 -- the Initiation was truncated", got)
	}
}

// TestProxyProtocolMappedSourceMatchesRouterOverride pins the trap:
// Config.validate canonicalizes routers: keys through
// netip.Addr.String(), so a source left as ::ffff:10.0.0.80 matches no
// override and files the router under a second identity.
func TestProxyProtocolMappedSourceMatchesRouterOverride(t *testing.T) {
	pub := &capturePub{}
	cfg := Config{Routers: map[string]RouterOverride{
		"10.0.0.80": {Vendor: "cisco", OS: "iosxr"},
	}}
	ln := serveProxy(t, cfg, pub)
	conn := dial(t, ln)
	defer conn.Close()
	if _, err := conn.Write(proxyV2Mapped(netip.MustParseAddr("::ffff:10.0.0.80"))); err != nil {
		t.Fatal(err)
	}
	for _, raw := range bmpSession(t) {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	evs := waitEvents(t, pub, 2)
	if got := evs[0].Env.GetRouter().GetIp(); got != "10.0.0.80" {
		t.Fatalf("router ip = %q, want 10.0.0.80 (unmapped)", got)
	}
	if got := evs[0].Env.GetRouterInfo().GetVendor(); got != "cisco" {
		t.Fatalf("vendor = %q, want cisco -- the routers: override did not match", got)
	}
}

// assertCollectorClosed proves the collector actively hung up on conn, rather
// than merely not having published anything yet.
//
// Every "no session was started" assertion needs this. An absence assertion
// gated only on a sleep passes on a loaded machine whether or not the rule
// being tested exists at all -- the read here is what makes the collector's
// decision observable.
//
// io.EOF is the clean FIN. A connection reset is the same close seen through
// a receive buffer that still held bytes the test wrote after its header, and
// is equally proof of a close, so both pass. A read that blocks until the
// deadline does not: that is a connection the collector left open, which is
// the failure this assertion exists to catch.
func assertCollectorClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	switch _, err := conn.Read(buf); {
	case err == nil:
		t.Fatalf("read %q from %s; the collector should have closed it", buf, what)
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Fatalf("read on %s blocked until the deadline: the collector left it open", what)
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
	default:
		t.Fatalf("read err = %v on %s, want io.EOF or a reset", err, what)
	}
}

// TestProxyProtocolLocalIsNotASession covers a gateway health-checking its
// backend. It must close cleanly and leave no session behind.
func TestProxyProtocolLocalIsNotASession(t *testing.T) {
	pub := &capturePub{}
	ln := serveProxy(t, Config{}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	local := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A,
		0x20, 0x11, 0x00, 0x00}
	if _, err := conn.Write(local); err != nil {
		t.Fatal(err)
	}
	assertCollectorClosed(t, conn, "the LOCAL connection")
	if evs := pub.get(); len(evs) != 0 {
		t.Fatalf("a LOCAL health probe published %d events", len(evs))
	}
}

// TestProxyProtocolUnspecIsNotASession pins the requirement for a
// sender that declines to state a source: "v1 UNKNOWN, or v2 PROXY +
// AF_UNSPEC -- close cleanly, no session". There is no fallback identity to
// give such a connection, and a BMP session with no router identity is not
// useful.
//
// It is asserted here and not only in proxyproto's own tests because the
// parser returning KindUnspec is half the requirement: the server has to act
// on it. Mutating routerIP's KindUnspec arm to return an address and true
// leaves every parser test green.
func TestProxyProtocolUnspecIsNotASession(t *testing.T) {
	pub := &capturePub{}
	ln := serveProxy(t, Config{}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	if _, err := conn.Write([]byte("PROXY UNKNOWN\r\n")); err != nil {
		t.Fatal(err)
	}
	// Written after the header, as a real sender would: if the collector
	// wrongly opened a session, these are what it would publish. Writes may
	// fail once it closes, which is the expected outcome.
	for _, raw := range bmpSession(t) {
		_, _ = conn.Write(raw)
	}
	assertCollectorClosed(t, conn, "the UNKNOWN connection")
	if evs := pub.get(); len(evs) != 0 {
		t.Fatalf("an UNKNOWN header published %d events: %+v", len(evs), evs[0].Env.GetRouter())
	}
}

// proxyHeaderCount reads one vantage_collector_proxy_header_total series out of the
// default registry. The counters are process-global (promauto registers at
// package init), so callers compare a before/after delta rather than an
// absolute value.
func proxyHeaderCount(t *testing.T, result string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "vantage_collector_proxy_header_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestProxyProtocolBareConnectIsNotAFormatDisagreement pins "nothing sent
// at all" as distinct from "malformed header".
//
// A bare TCP connect is what an L4 health check and a port scanner do, and
// HAProxy's `server ... send-proxy check` does not send a header on its own
// health checks unless check-send-proxy is also set -- so the most likely
// front-end, correctly configured for traffic, produces one of these every
// check interval forever. Counting them as malformed would make the counter
// whose whole purpose is telling "the proxy is not configured" apart from
// "the proxy is configured and we disagree about the format" read as
// permanent format disagreement on a healthy system, with a warn line beside
// it at the probe's frequency.
func TestProxyProtocolBareConnectIsNotAFormatDisagreement(t *testing.T) {
	tests := []struct {
		name string
		want string
		prep func(t *testing.T, client, server net.Conn)
	}{
		{
			name: "connect and close",
			want: proxyResultClosed,
			prep: func(t *testing.T, client, server net.Conn) { client.Close() },
		},
		{
			name: "connect and say nothing until the deadline",
			want: proxyResultTimeout,
			prep: func(t *testing.T, client, server net.Conn) {
				t.Cleanup(func() { client.Close() })
				// Stands in for the deadline handleConn sets before the
				// header read, shortened so the test does not wait out
				// defaultIdleReadTimeout.
				if err := server.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			tc.prep(t, client, server)
			var logs bytes.Buffer
			srv := &Server{
				cfg: Config{ProxyProtocol: ProxyProtocolRequired},
				log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			}
			beforeWant := proxyHeaderCount(t, tc.want)
			beforeMalformed := proxyHeaderCount(t, proxyResultMalformed)
			if _, ok := srv.routerIP(server); ok {
				t.Fatal("routerIP accepted a connection that sent no header at all")
			}
			if got := proxyHeaderCount(t, tc.want) - beforeWant; got != 1 {
				t.Fatalf("vantage_collector_proxy_header_total{result=%q} moved by %v, want 1", tc.want, got)
			}
			if got := proxyHeaderCount(t, proxyResultMalformed) - beforeMalformed; got != 0 {
				t.Fatalf("vantage_collector_proxy_header_total{result=\"malformed\"} moved by %v: a bare connect is not a format disagreement", got)
			}
			if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("a bare connect logged above debug, which a health check would repeat forever:\n%s", logs.String())
			}
		})
	}
}

// TestProxyProtocolPartialHeaderIsMalformed is the regression this fix exists
// to prevent. proxyproto.ErrNoData -- and so the "closed"/"timeout" buckets
// downstream of it -- must match only a read that produced zero bytes, not
// any read that ends in EOF. A peer that commits to a version by sending part
// of a header and then stops is a format disagreement, not a bare connect: it
// has to land in "malformed" at warn, or a misbehaving or hostile proxy can
// dodge the warn-level counter simply by sending a fragment instead of
// nothing.
func TestProxyProtocolPartialHeaderIsMalformed(t *testing.T) {
	tests := []struct {
		name string
		send []byte
	}{
		{
			// 12 bytes, no CRLF: a v1 line abandoned before it could ever be
			// valid.
			name: "v1 line truncated before its CRLF",
			send: []byte("PROXY TCP4 1"),
		},
		{
			// The full 12-byte v2 signature with none of the mandatory
			// 4-byte meta block behind it.
			name: "v2 signature with no meta block",
			send: []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			go func() {
				_, _ = client.Write(tc.send)
				client.Close()
			}()
			var logs bytes.Buffer
			srv := &Server{
				cfg: Config{ProxyProtocol: ProxyProtocolRequired},
				log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			}
			beforeMalformed := proxyHeaderCount(t, proxyResultMalformed)
			beforeClosed := proxyHeaderCount(t, proxyResultClosed)
			if _, ok := srv.routerIP(server); ok {
				t.Fatal("routerIP accepted a connection whose header never arrived")
			}
			if got := proxyHeaderCount(t, proxyResultMalformed) - beforeMalformed; got != 1 {
				t.Fatalf("vantage_collector_proxy_header_total{result=\"malformed\"} moved by %v, want 1", got)
			}
			if got := proxyHeaderCount(t, proxyResultClosed) - beforeClosed; got != 0 {
				t.Fatalf("vantage_collector_proxy_header_total{result=\"closed\"} moved by %v: a partial header is not a bare connect", got)
			}
			if !strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("a partial header did not log at warn:\n%s", logs.String())
			}
		})
	}
}

// TestProxyProtocolOffIsUnchanged guards the default path: with the flag off,
// identity still comes from the socket and a header is not expected.
func TestProxyProtocolOffIsUnchanged(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)
	conn := dial(t, ln)
	defer conn.Close()
	for _, raw := range bmpSession(t) {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	evs := waitEvents(t, pub, 2)
	if got := evs[0].Env.GetRouter().GetIp(); got != "127.0.0.1" {
		t.Fatalf("router ip = %q, want 127.0.0.1 from the socket", got)
	}
}

// rejectedCount reads one vantage_collector_connections_rejected_total series. Like
// proxyHeaderCount, callers compare a before/after delta.
func rejectedCount(t *testing.T, reason string) float64 {
	t.Helper()
	return counterValue(t, "vantage_collector_connections_rejected_total", "reason", reason)
}

// bmpMessageCount sums vantage_collector_bmp_messages_total across every type label:
// the number of BMP messages this process has parsed off any connection.
func bmpMessageCount(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		if mf.GetName() == "vantage_collector_bmp_messages_total" {
			for _, m := range mf.GetMetric() {
				n += m.GetCounter().GetValue()
			}
		}
	}
	return n
}

func counterValue(t *testing.T, name, label, value string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// syncBuffer is a log sink safe to read while connection goroutines write to
// it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serveCfg starts a server on cfg and returns its listener. Every test
// connection comes from 127.0.0.1, so allowed_sources and trusted_proxies are
// written relative to that.
func serveCfg(t *testing.T, cfg Config, pub EventPublisher) (*Server, net.Listener) {
	t.Helper()
	if cfg.CollectorID == "" {
		cfg.CollectorID = "c1"
	}
	srv := NewServer(cfg, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return srv, ln
}

// writeSession writes a header (if any) and the standard three-message BMP
// session. Writes may fail once the collector closes, which is the expected
// outcome for a rejected connection and is not a test failure.
func writeSession(t *testing.T, conn net.Conn, header []byte) {
	t.Helper()
	if header != nil {
		_, _ = conn.Write(header)
	}
	for _, raw := range bmpSession(t) {
		_, _ = conn.Write(raw)
	}
}

func TestAllowedSourceIsAccepted(t *testing.T) {
	pub := &capturePub{}
	_, ln := serveCfg(t, Config{AllowedSources: []string{"10.0.0.0/8", "127.0.0.0/8"}}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	if got := waitEvents(t, pub, 2)[0].Env.GetRouter().GetIp(); got != "127.0.0.1" {
		t.Fatalf("router ip = %q, want 127.0.0.1", got)
	}
}

// TestDisallowedSourceIsClosedBeforeParsing is the allowlist's reason to
// exist: a sender outside it gets no session and no byte of its BMP is
// parsed. The parsed-message counter is the proof of "before parsing" -- a
// check placed after the first bmp.ReadMsg would still publish nothing for a
// sender that opened with an Initiation, but it would have parsed one.
func TestDisallowedSourceIsClosedBeforeParsing(t *testing.T) {
	pub := &capturePub{}
	_, ln := serveCfg(t, Config{AllowedSources: []string{"10.0.0.0/8"}}, pub)
	before := rejectedCount(t, rejectSourceNotAllowed)
	parsedBefore := bmpMessageCount(t)
	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	assertCollectorClosed(t, conn, "a connection from outside allowed_sources")
	if evs := pub.get(); len(evs) != 0 {
		t.Fatalf("a disallowed source published %d events", len(evs))
	}
	if got := bmpMessageCount(t) - parsedBefore; got != 0 {
		t.Fatalf("%v BMP messages were parsed from a disallowed source", got)
	}
	if got := rejectedCount(t, rejectSourceNotAllowed) - before; got != 1 {
		t.Fatalf("vantage_collector_connections_rejected_total{reason=%q} moved by %v, want 1", rejectSourceNotAllowed, got)
	}
}

// TestProxiedSourceIsCheckedNotTheProxy: behind a proxy every connection's
// TCP peer is the proxy, so checking it would admit the whole internet or
// nobody. The PROXY header's source is the router, and it is what the
// allowlist applies to -- in both directions.
func TestProxiedSourceIsCheckedNotTheProxy(t *testing.T) {
	t.Run("router inside, proxy outside: accepted", func(t *testing.T) {
		pub := &capturePub{}
		ln := serveProxy(t, Config{AllowedSources: []string{"10.0.0.0/8"}}, pub)
		conn := dial(t, ln)
		defer conn.Close()
		writeSession(t, conn, proxyV2(netip.MustParseAddr("10.0.0.80")))
		if got := waitEvents(t, pub, 2)[0].Env.GetRouter().GetIp(); got != "10.0.0.80" {
			t.Fatalf("router ip = %q, want 10.0.0.80", got)
		}
	})
	t.Run("proxy inside, router outside: rejected", func(t *testing.T) {
		pub := &capturePub{}
		ln := serveProxy(t, Config{AllowedSources: []string{"127.0.0.0/8"}}, pub)
		before := rejectedCount(t, rejectSourceNotAllowed)
		parsedBefore := bmpMessageCount(t)
		conn := dial(t, ln)
		defer conn.Close()
		writeSession(t, conn, proxyV2(netip.MustParseAddr("10.0.0.80")))
		assertCollectorClosed(t, conn, "a proxied router outside allowed_sources")
		if evs := pub.get(); len(evs) != 0 {
			t.Fatalf("a disallowed proxied source published %d events", len(evs))
		}
		if got := bmpMessageCount(t) - parsedBefore; got != 0 {
			t.Fatalf("%v BMP messages were parsed from a disallowed proxied source", got)
		}
		if got := rejectedCount(t, rejectSourceNotAllowed) - before; got != 1 {
			t.Fatalf("source_not_allowed moved by %v, want 1", got)
		}
	})
}

// TestTrustedProxies: with trusted_proxies set, only those peers may assert a
// router identity in a PROXY header. Anyone else who can reach the port could
// otherwise write 28 bytes and become any router.
func TestTrustedProxies(t *testing.T) {
	t.Run("untrusted proxy rejected before its header is read", func(t *testing.T) {
		pub := &capturePub{}
		ln := serveProxy(t, Config{TrustedProxies: []string{"192.0.2.0/24"}}, pub)
		before := rejectedCount(t, rejectProxyNotTrusted)
		acceptedBefore := proxyHeaderCount(t, proxyResultAccepted)
		conn := dial(t, ln)
		defer conn.Close()
		writeSession(t, conn, proxyV2(netip.MustParseAddr("10.0.0.80")))
		assertCollectorClosed(t, conn, "a connection from an untrusted proxy")
		if evs := pub.get(); len(evs) != 0 {
			t.Fatalf("an untrusted proxy published %d events", len(evs))
		}
		if got := rejectedCount(t, rejectProxyNotTrusted) - before; got != 1 {
			t.Fatalf("proxy_not_trusted moved by %v, want 1", got)
		}
		if got := proxyHeaderCount(t, proxyResultAccepted) - acceptedBefore; got != 0 {
			t.Fatalf("an untrusted proxy's header was read and accepted (%v)", got)
		}
	})
	t.Run("trusted proxy accepted", func(t *testing.T) {
		pub := &capturePub{}
		ln := serveProxy(t, Config{TrustedProxies: []string{"127.0.0.1/32"}}, pub)
		conn := dial(t, ln)
		defer conn.Close()
		writeSession(t, conn, proxyV2(netip.MustParseAddr("10.0.0.80")))
		if got := waitEvents(t, pub, 2)[0].Env.GetRouter().GetIp(); got != "10.0.0.80" {
			t.Fatalf("router ip = %q, want 10.0.0.80", got)
		}
	})
}

// TestMaxConnectionsIsEnforced: the N+1th concurrent connection is closed at
// once rather than given a goroutine and a buffer, and the slot comes back
// when a held connection ends.
func TestMaxConnectionsIsEnforced(t *testing.T) {
	pub := &capturePub{}
	_, ln := serveCfg(t, Config{MaxConnections: 2}, pub)

	// Two held sessions, each proven accepted by an event it produced.
	var held []net.Conn
	for i := range 2 {
		c := dial(t, ln)
		defer c.Close()
		writeSession(t, c, nil)
		waitEvents(t, pub, 2*(i+1))
		held = append(held, c)
	}

	before := rejectedCount(t, rejectMaxConnections)
	extra := dial(t, ln)
	defer extra.Close()
	start := time.Now()
	assertCollectorClosed(t, extra, "the connection past max_connections")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the excess connection took %v to close; it should be closed on accept", d)
	}
	if got := rejectedCount(t, rejectMaxConnections) - before; got != 1 {
		t.Fatalf("max_connections moved by %v, want 1", got)
	}

	// Freeing a slot lets the next connection in. The accept loop learns of
	// the close asynchronously, so retry until one is accepted.
	held[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dial(t, ln)
		writeSession(t, c, nil)
		n := len(pub.get())
		ok := false
		for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if len(pub.get()) > n {
				ok = true
				break
			}
		}
		c.Close()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no connection accepted after a held one closed: the slot was never released")
		}
	}
}

// TestEmptyAllowlistAcceptsAndWarnsOnce: no allowlist keeps today's behavior
// (accept any source) but says so once at startup -- not per connection,
// where it would be noise, and not at all when an allowlist is set.
func TestEmptyAllowlistAcceptsAndWarnsOnce(t *testing.T) {
	const warning = "allowed_sources is empty: accepting BMP from any address"
	for _, tc := range []struct {
		name    string
		allowed []string
		want    int
	}{
		{"empty", nil, 1},
		{"set", []string{"127.0.0.0/8"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &capturePub{}
			var logs syncBuffer
			srv := NewServer(Config{CollectorID: "c1", AllowedSources: tc.allowed}, pub, time.Now)
			srv.log = slog.New(slog.NewTextHandler(&logs, nil))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go srv.Serve(ctx, ln)
			for i := range 2 {
				c := dial(t, ln)
				defer c.Close()
				writeSession(t, c, nil)
				waitEvents(t, pub, 2*(i+1))
			}
			if got := strings.Count(logs.String(), warning); got != tc.want {
				t.Fatalf("warning logged %d times, want %d:\n%s", got, tc.want, logs.String())
			}
			if tc.want == 1 && !strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("the empty-allowlist notice is not a warning:\n%s", logs.String())
			}
		})
	}
}

// TestStalledMessageBodyIsClosed: once a message's header has arrived, the
// rest of it has a deadline. Without one a peer could open a session, send a
// header claiming a large body, and trickle it -- or nothing -- forever,
// holding a goroutine, an fd and a buffer.
func TestStalledMessageBodyIsClosed(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.messageReadTimeout = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(t.Context(), ln)
	conn := dial(t, ln)
	defer conn.Close()

	// A complete first message clears the handshake deadline, so what closes
	// the connection below can only be the per-message one.
	writeSession(t, conn, nil)
	waitEvents(t, pub, 2)

	// A header claiming 1000 bytes of body, and 10 of them.
	stalled := append(bmp.AppendHeader(nil, bmp.TypeRouteMonitoring, 1000), make([]byte, 10)...)
	if _, err := conn.Write(stalled); err != nil {
		t.Fatal(err)
	}
	assertCollectorClosed(t, conn, "a connection that stalled mid-message")
}

// TestQuietGapBetweenMessagesOutlivesTheMessageDeadline is the other side of
// TestStalledMessageBodyIsClosed: the per-message deadline must not become an
// idle timeout. A router that goes quiet between messages for longer than the
// deadline, then sends a message in two halves, keeps its session.
func TestQuietGapBetweenMessagesOutlivesTheMessageDeadline(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.messageReadTimeout = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(t.Context(), ln)
	conn := dial(t, ln)
	defer conn.Close()

	msgs := bmpSession(t)
	for _, raw := range msgs[:2] {
		if _, err := conn.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	waitEvents(t, pub, 1)
	time.Sleep(600 * time.Millisecond)
	rm := msgs[2]
	if _, err := conn.Write(rm[:len(rm)/2]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := conn.Write(rm[len(rm)/2:]); err != nil {
		t.Fatal(err)
	}
	waitEvents(t, pub, 2)
	// And quiet again, past the deadline, after that message: still open.
	time.Sleep(600 * time.Millisecond)
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read on a quiet session = %v, want our own deadline: the collector closed it", err)
	}
}

// zonedConn is a net.Conn whose peer address carries an IPv6 zone, which a
// real link-local peer's RemoteAddr does.
type zonedConn struct {
	net.Conn
	remote net.Addr
}

func (c zonedConn) RemoteAddr() net.Addr { return c.remote }

// TestRouterIPStripsTheZone: a zone is an interface on this host, not part of
// the router's identity. Keeping it gives one router two identities
// ("fe80::1%eth0" and "fe80::1%eth1") and makes routers: overrides keyed on
// the bare address never match. proxyproto already rejects zones for the
// same reason.
func TestRouterIPStripsTheZone(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := zonedConn{Conn: server, remote: &net.TCPAddr{
		IP: net.ParseIP("fe80::1"), Port: 179, Zone: "eth0"}}
	srv := &Server{cfg: Config{ProxyProtocol: ProxyProtocolOff}}
	addr, ok := srv.routerIP(conn)
	if !ok {
		t.Fatal("routerIP rejected a proxy-off connection")
	}
	if addr != netip.MustParseAddr("fe80::1") || addr.Zone() != "" {
		t.Fatalf("router ip = %q, want fe80::1 with no zone", addr)
	}
}

// TestUnparsableAllowlistFailsClosed: LoadConfig rejects a bad CIDR, but a
// Config built directly skips it. An allowlist whose only entry does not
// parse must then admit nobody -- treating it as empty would admit everybody.
func TestUnparsableAllowlistFailsClosed(t *testing.T) {
	pub := &capturePub{}
	_, ln := serveCfg(t, Config{AllowedSources: []string{"127.0.0.1"}}, pub)
	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	assertCollectorClosed(t, conn, "a connection checked against an unparsable allowlist")
	if evs := pub.get(); len(evs) != 0 {
		t.Fatalf("an unparsable allowlist admitted a source: %d events", len(evs))
	}
}
