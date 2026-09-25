package collector

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// owingPub is notifyPub (every publish accepted synchronously, each one's
// failure callback kept) with a NATS connection state the test controls.
type owingPub struct {
	notifyPub
	up atomic.Bool
}

func (o *owingPub) Connected() bool { return o.up.Load() }

// serveOwing starts a Server over pub whose owed events are retried every
// interval. The interval is set before Serve starts, so it is not raced.
//
// Cleanup waits for Serve to return. Serve's shutdown closes the ledger, which
// counts what it still holds as dropped; unwaited, that lands in whichever
// test runs next and moves the counter it is measuring.
func serveOwing(t *testing.T, pub EventPublisher, interval time.Duration) (*Server, net.Listener) {
	t.Helper()
	srv, ln, _ := startOwing(t, pub, interval)
	return srv, ln
}

// startOwing is serveOwing for a test that shuts the Server down itself:
// stop cancels Serve and returns once Serve has. Cleanup calls it too.
func startOwing(t *testing.T, pub EventPublisher, interval time.Duration) (*Server, net.Listener, func()) {
	t.Helper()
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	srv.owedRetryInterval = interval
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, ln)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("Serve did not return within 10 s of its ctx being canceled")
			}
		})
	}
	t.Cleanup(stop)
	return srv, ln, stop
}

// TestAViewLostWhosePublishFailedIsRepublishedOnReconnect: a view_lost whose
// publish failed is republished once NATS returns, and resolves as the
// original close would have.
//
// A publish fails, so the session closes. Its close-out view_lost fails too,
// because NATS is what failed. The collector holds the event until the
// publisher is connected again, and then sends it as it was built, with the
// same seq, session and msg-id, so it resolves exactly as a close that had
// succeeded.
//
// The other side is asserted in the middle: while NATS is down, nothing is
// re-sent.
func TestAViewLostWhosePublishFailedIsRepublishedOnReconnect(t *testing.T) {
	pub := &owingPub{}
	pub.up.Store(true)
	srv, ln := serveOwing(t, pub, 20*time.Millisecond)

	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	waitEvents(t, &pub.capturePub, 2) // Peer-Up, then the route

	pub.up.Store(false) // NATS goes away
	pub.reject(t, 0)    // the Peer-Up's publish fails, so the session closes
	assertCollectorClosed(t, conn, "the session whose publish failed")

	evs := waitEvents(t, &pub.capturePub, 3)
	lost := evs[2]
	if lost.Env.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_VIEW_LOST {
		t.Fatalf("third publish is %v, want the close-out view_lost", lost.Env.GetPeerEvent().GetKind())
	}
	pub.reject(t, 2) // ...and so does the close-out's view_lost

	if n := srv.owed.len(); n != 1 {
		t.Fatalf("%d events owed after the close-out failed, want 1", n)
	}
	time.Sleep(10 * srv.owedRetryInterval)
	if n := len(pub.get()); n != 3 {
		t.Fatalf("%d publishes while NATS was down, want 3: an owed event waits for the connection", n)
	}

	pub.up.Store(true) // NATS is back
	again := waitEvents(t, &pub.capturePub, 4)[3]
	if again.MsgID != lost.MsgID {
		t.Errorf("republished msg-id %q, want the original %q -- a different id would not dedupe "+
			"against a close-out that did land", again.MsgID, lost.MsgID)
	}
	if again.Env.GetSeq() != lost.Env.GetSeq() || again.Env.GetSessionId() != lost.Env.GetSessionId() {
		t.Errorf("republished (session %d, seq %d), want the original (%d, %d)",
			again.Env.GetSessionId(), again.Env.GetSeq(), lost.Env.GetSessionId(), lost.Env.GetSeq())
	}
	if again.Env.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_VIEW_LOST {
		t.Errorf("republished kind %v, want view_lost", again.Env.GetPeerEvent().GetKind())
	}
	if n := srv.owed.len(); n != 0 {
		t.Errorf("%d events still owed after a successful republish, want 0", n)
	}
}

// synchronousFailPub accepts the first `ok` publishes and refuses every one
// after, synchronously. It is not a NotifyingPublisher, so the close-out's
// failure arrives as Publish's own error: the second of the two paths into
// the ledger.
type synchronousFailPub struct {
	capturePub
	ok    int
	calls atomic.Int32
}

func (s *synchronousFailPub) Publish(ev Event) error {
	if int(s.calls.Add(1)) > s.ok {
		return fmt.Errorf("boom: nats: connection closed")
	}
	return s.capturePub.Publish(ev)
}

// TestASynchronousCloseOutFailureIsOwedToo: a close-out whose publish fails at
// the call is owed exactly as one that fails later. The retry interval is an
// hour so the ledger is read before anything takes from it.
func TestASynchronousCloseOutFailureIsOwedToo(t *testing.T) {
	pub := &synchronousFailPub{ok: 2}
	srv, ln := serveOwing(t, pub, time.Hour)
	conn := dial(t, ln)
	writeSession(t, conn, nil)
	waitEvents(t, &pub.capturePub, 2)
	conn.Close() // the router goes away; the close-out view_lost is the third publish
	deadline := time.Now().Add(5 * time.Second)
	for srv.owed.len() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("owed = %d, want 1: a close-out refused at the call must be owed", srv.owed.len())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestOwedLedgerIsBounded: a long outage cannot grow the ledger without
// limit. Past its cap a new event is dropped and counted; the ones already
// held are kept.
func TestOwedLedgerIsBounded(t *testing.T) {
	l := newOwedLedger(2)
	before := plainCounterValue(t, "vantage_collector_owed_view_lost_dropped_total")
	for i := range 3 {
		l.add(Event{MsgID: fmt.Sprint(i)})
	}
	if n := l.len(); n != 2 {
		t.Fatalf("len = %d, want the cap, 2", n)
	}
	if got := plainCounterValue(t, "vantage_collector_owed_view_lost_dropped_total") - before; got != 1 {
		t.Errorf("dropped moved by %v, want 1", got)
	}
	evs := l.take()
	if len(evs) != 2 || evs[0].MsgID != "0" || evs[1].MsgID != "1" {
		t.Errorf("take = %+v, want the two held first", evs)
	}
	if n := l.len(); n != 0 {
		t.Errorf("len after take = %d, want 0", n)
	}
}

// oweOneViewLost runs one session over pub until its close-out view_lost is
// owed: NATS goes away, the Peer-Up's publish fails, which closes the session,
// and so does the view_lost the close-out then publishes. It returns that
// view_lost, with pub still disconnected.
func oweOneViewLost(t *testing.T, srv *Server, ln net.Listener, pub *owingPub) Event {
	t.Helper()
	conn := dial(t, ln)
	defer conn.Close()
	writeSession(t, conn, nil)
	waitEvents(t, &pub.capturePub, 2)
	pub.up.Store(false)
	pub.reject(t, 0)
	assertCollectorClosed(t, conn, "the session whose publish failed")
	lost := waitEvents(t, &pub.capturePub, 3)[2]
	if lost.Env.GetPeerEvent().GetKind() != vantagev1.PeerEvent_KIND_VIEW_LOST {
		t.Fatalf("third publish is %v, want the close-out view_lost", lost.Env.GetPeerEvent().GetKind())
	}
	pub.reject(t, 2)
	if n := srv.owed.len(); n != 1 {
		t.Fatalf("%d events owed after the close-out failed, want 1", n)
	}
	return lost
}

// TestShutdownTriesTheOwedOnceThenDropsWhatFails: at shutdown, with NATS
// connected, an owed view_lost gets one last attempt before Serve returns, so
// the daemon's Drain waits for it. The retry interval is an hour, so that
// attempt is the only one. Once Serve has returned nothing republishes, so a
// failure that arrives after -- the last attempt failing in flight -- is
// dropped and counted rather than held by a ledger nobody reads.
func TestShutdownTriesTheOwedOnceThenDropsWhatFails(t *testing.T) {
	pub := &owingPub{}
	pub.up.Store(true)
	srv, ln, stop := startOwing(t, pub, time.Hour)
	lost := oweOneViewLost(t, srv, ln, pub)

	pub.up.Store(true) // NATS is back, but the next interval is an hour away
	stop()
	evs := pub.get()
	if len(evs) != 4 {
		t.Fatalf("%d publishes by the time Serve returned, want 4: the owed view_lost is tried once at shutdown", len(evs))
	}
	if evs[3].MsgID != lost.MsgID {
		t.Errorf("shutdown's attempt carried msg-id %q, want the owed event's %q", evs[3].MsgID, lost.MsgID)
	}

	before := plainCounterValue(t, "vantage_collector_owed_view_lost_dropped_total")
	pub.reject(t, 3) // the last attempt fails, after Serve returned
	if n := srv.owed.len(); n != 0 {
		t.Errorf("%d events owed after shutdown, want 0: nothing is left to republish them", n)
	}
	if got := plainCounterValue(t, "vantage_collector_owed_view_lost_dropped_total") - before; got != 1 {
		t.Errorf("dropped moved by %v when the last attempt failed, want 1", got)
	}
}

// TestShutdownDropsAndCountsWhatItCannotSend: at shutdown with NATS still
// down, an owed view_lost is not sent, and it is not silently forgotten
// either: it is counted as dropped.
func TestShutdownDropsAndCountsWhatItCannotSend(t *testing.T) {
	pub := &owingPub{}
	pub.up.Store(true)
	srv, ln, stop := startOwing(t, pub, time.Hour)
	oweOneViewLost(t, srv, ln, pub) // NATS stays down

	before := plainCounterValue(t, "vantage_collector_owed_view_lost_dropped_total")
	stop()
	if n := len(pub.get()); n != 3 {
		t.Errorf("%d publishes by the time Serve returned, want 3: NATS is down, so nothing is re-sent", n)
	}
	if n := srv.owed.len(); n != 0 {
		t.Errorf("%d events owed after shutdown, want 0", n)
	}
	if got := plainCounterValue(t, "vantage_collector_owed_view_lost_dropped_total") - before; got != 1 {
		t.Errorf("dropped moved by %v at shutdown, want 1", got)
	}
}

// TestAnOwedEventThatFailsAgainIsOwedAgain: a republish is an ordinary
// publish, and one that fails in flight goes back into the ledger and is
// tried again, as the same event.
func TestAnOwedEventThatFailsAgainIsOwedAgain(t *testing.T) {
	pub := &owingPub{}
	pub.up.Store(true)
	srv, ln := serveOwing(t, pub, 20*time.Millisecond)
	lost := oweOneViewLost(t, srv, ln, pub)

	pub.up.Store(true)
	waitEvents(t, &pub.capturePub, 4)
	pub.reject(t, 3) // the republish fails in flight
	again := waitEvents(t, &pub.capturePub, 5)[4]
	if again.MsgID != lost.MsgID {
		t.Errorf("second republish carried msg-id %q, want the owed event's %q", again.MsgID, lost.MsgID)
	}
}

// TestAnOwedEventRefusedAtTheCallIsOwedAgain: the same, for a republish the
// publisher refuses synchronously. It is tried every interval and stays owed.
func TestAnOwedEventRefusedAtTheCallIsOwedAgain(t *testing.T) {
	pub := &synchronousFailPub{ok: 2}
	srv, ln := serveOwing(t, pub, 5*time.Millisecond)
	conn := dial(t, ln)
	writeSession(t, conn, nil)
	waitEvents(t, &pub.capturePub, 2)
	conn.Close() // the close-out is the third publish, refused

	// The close-out is call 3; every call past it is a republish.
	deadline := time.Now().Add(5 * time.Second)
	for pub.calls.Load() < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("%d publish calls, want at least 6: the owed event is retried every interval", pub.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := srv.owed.len(); n != 1 {
		t.Errorf("%d events owed after repeated refusals, want 1", n)
	}
}

// gaugeValue reads an unlabeled gauge from the default registry.
func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("gauge %s not registered", name)
	return 0
}

// waitCounterDelta waits until the counter name has moved by want since
// before, failing if it has not within 5 s.
func waitCounterDelta(t *testing.T, name string, before, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := plainCounterValue(t, name) - before
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s moved by %v, want %v", name, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOwedMetricsTrackTheLedger: the gauge holds what is owed and not yet
// handed back, resent counts each republish attempt, and republish_failed
// counts each attempt that fails. With no cap on attempts, a stream that
// keeps refusing an owed event shows only as those two counters climbing
// together while the gauge flickers.
func TestOwedMetricsTrackTheLedger(t *testing.T) {
	const (
		gauge  = "vantage_collector_owed_view_lost"
		resent = "vantage_collector_owed_view_lost_resent_total"
		failed = "vantage_collector_owed_view_lost_republish_failed_total"
	)
	pub := &owingPub{}
	pub.up.Store(true)
	srv, ln := serveOwing(t, pub, 20*time.Millisecond)
	resent0, failed0 := plainCounterValue(t, resent), plainCounterValue(t, failed)
	oweOneViewLost(t, srv, ln, pub)

	if got := gaugeValue(t, gauge); got != 1 {
		t.Errorf("gauge = %v with one event owed, want 1", got)
	}
	if got := plainCounterValue(t, resent) - resent0; got != 0 {
		t.Errorf("resent moved by %v while NATS was down, want 0", got)
	}

	pub.up.Store(true)
	waitEvents(t, &pub.capturePub, 4) // the first republish
	waitCounterDelta(t, resent, resent0, 1)
	if got := gaugeValue(t, gauge); got != 0 {
		t.Errorf("gauge = %v with the owed event handed back, want 0", got)
	}
	if got := plainCounterValue(t, failed) - failed0; got != 0 {
		t.Errorf("republish_failed moved by %v before any republish failed, want 0", got)
	}

	pub.reject(t, 3) // the republish fails in flight
	waitCounterDelta(t, failed, failed0, 1)
	waitEvents(t, &pub.capturePub, 5) // and is tried again
	waitCounterDelta(t, resent, resent0, 2)
}
