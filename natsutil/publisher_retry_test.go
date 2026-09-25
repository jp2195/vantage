package natsutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil/natstest"
)

// proxied is a Publisher whose connection runs through a natstest.Proxy, plus
// a second, direct connection to the same server for checking what the
// stream actually holds without going through the proxy being cut.
type proxied struct {
	pub     *Publisher
	retries *atomic.Int64 // OnRetry calls
	nc      *nats.Conn
	proxy   *natstest.Proxy
	direct  jetstream.JetStream
}

func newProxied(t *testing.T) proxied {
	t.Helper()
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := EnsureStreams(ctx, direct, testOpts); err != nil {
		t.Fatal(err)
	}

	nc, err := nats.Connect(proxy.URL(), nats.MaxReconnects(-1), nats.ReconnectWait(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	pr := proxied{pub: NewPublisher(js), retries: new(atomic.Int64), nc: nc, proxy: proxy, direct: direct}
	pr.pub.OnRetry = func(error) { pr.retries.Add(1) }

	// One publish through the proxy before any test cuts it, so the async
	// reply subscription exists and the connection is known good.
	if err := pr.pub.Publish(testEvent("warmup/1")); err != nil {
		t.Fatal(err)
	}
	if err := pr.pub.Drain(5 * time.Second); err != nil {
		t.Fatalf("warm-up publish failed: %v", err)
	}
	return pr
}

// routesMsgs is how many messages ROUTES holds, read over the direct
// connection.
func (pr proxied) routesMsgs(t *testing.T) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := pr.direct.Stream(ctx, "ROUTES")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

// waitRoutesMsgs waits until ROUTES holds want messages.
func (pr proxied) waitRoutesMsgs(t *testing.T, want uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := pr.routesMsgs(t)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ROUTES holds %d messages, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// routesMsgIDs returns how many times each Nats-Msg-Id appears in ROUTES.
func (pr proxied) routesMsgIDs(t *testing.T) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := pr.routesMsgs(t)
	cons, err := pr.direct.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int{}
	for range want {
		m, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		ids[m.Headers().Get(jetstream.MsgIDHeader)]++
	}
	return ids
}

// publishN publishes n events with msg-ids prefix/1..prefix/n, each reporting
// a late failure to rec.
func publishN(t *testing.T, p *Publisher, prefix string, n int, rec *failRecorder) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := p.PublishNotify(testEvent(fmt.Sprintf("%s/%d", prefix, i)), rec.record); err != nil {
			t.Fatalf("PublishNotify (synchronous): %v", err)
		}
	}
}

// TestPublisherRetriesPublishesInFlightAtADisconnect is the lame-duck restart
// defect. When the connection drops, nats.go fails every async publish still
// awaiting its PubAck with nats.ErrDisconnected, whether or not the server
// stored it. Two kinds are in flight here, and each needs the retry for a
// different reason:
//
//   - stored/*: the server stored them and the proxy held the acks. A retry
//     that did not carry the same Nats-Msg-Id would store them twice.
//   - unsent/*: the proxy held the publishes themselves, so the server never
//     saw them. Without a retry they are lost.
//
// After the reconnect every one must report success and ROUTES must hold each
// msg-id exactly once.
func TestPublisherRetriesPublishesInFlightAtADisconnect(t *testing.T) {
	pr := newProxied(t)
	var fails failRecorder

	pr.proxy.FreezeToClient()
	publishN(t, pr.pub, "stored", 20, &fails)
	pr.waitRoutesMsgs(t, 1+20) // stored, acks held in the proxy

	pr.proxy.FreezeToServer()
	publishN(t, pr.pub, "unsent", 20, &fails)
	time.Sleep(50 * time.Millisecond)
	if got := pr.routesMsgs(t); got != 21 {
		t.Fatalf("ROUTES holds %d messages before the cut, want 21: the unsent publishes reached the server", got)
	}

	pr.proxy.Sever()

	if err := pr.pub.Drain(20 * time.Second); err != nil {
		t.Fatalf("Drain after the reconnect: %v", err)
	}
	// Every one of the 40 was awaiting its ack at the cut, so every one was
	// re-sent: fewer means the cut did not catch them and this test proved
	// nothing about the retry.
	if got := pr.retries.Load(); got != 40 {
		t.Fatalf("OnRetry ran %d times, want 40", got)
	}
	if n := fails.count(); n != 0 {
		t.Fatalf("%d publishes reported failure to their session; the first: %v", n, fails.errs[0])
	}
	pr.assertNoneRetrying(t)
	ids := pr.routesMsgIDs(t)
	if len(ids) != 41 {
		t.Fatalf("ROUTES holds %d distinct msg-ids, want 41", len(ids))
	}
	for id, n := range ids {
		if n != 1 {
			t.Errorf("msg-id %s stored %d times", id, n)
		}
	}
}

// assertNoneRetrying checks that every retry slot taken was given back. A
// leak would not fail any single publish; it would shrink maxRetrying for
// the life of the process until every in-flight failure was fatal again.
func (pr proxied) assertNoneRetrying(t *testing.T) {
	t.Helper()
	pr.pub.mu.Lock()
	defer pr.pub.mu.Unlock()
	if n := len(pr.pub.retrying); n != 0 {
		t.Fatalf("%d retry slots still held after every publish resolved", n)
	}
}

// retryingCount is how many publishes are between a retryable failure and
// their resolution.
func (pr proxied) retryingCount() int {
	pr.pub.mu.Lock()
	defer pr.pub.mu.Unlock()
	return len(pr.pub.retrying)
}

// cutAndRefuse stores n publishes with their acks held, then cuts the
// connection and refuses the reconnect, leaving all n waiting to re-send.
func (pr proxied) cutAndRefuse(t *testing.T, n int, rec *failRecorder) {
	t.Helper()
	pr.proxy.FreezeToClient()
	publishN(t, pr.pub, "held", n, rec)
	pr.waitRoutesMsgs(t, uint64(1+n))
	pr.proxy.Refuse()
	pr.proxy.Sever()
}

// TestPublisherGivesUpWhenNATSDoesNotReturn: a publish that is still waiting
// for the connection at its retry deadline is reported failed -- to Drain,
// to OnError and to its own onFail, which is what closes the BMP session --
// with the disconnect it was waiting out still visible to errors.Is.
func TestPublisherGivesUpWhenNATSDoesNotReturn(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.deadline = 500 * time.Millisecond
	var global, fails failRecorder
	pr.pub.OnError = global.record

	start := time.Now()
	pr.cutAndRefuse(t, 5, &fails)
	err := pr.pub.Drain(10 * time.Second)
	if err == nil {
		t.Fatal("Drain returned nil with NATS unreachable past the retry deadline")
	}
	if !errors.Is(err, nats.ErrDisconnected) || !errors.Is(err, errRetryDeadline) {
		t.Fatalf("Drain error = %v; want it to wrap both nats.ErrDisconnected and errRetryDeadline", err)
	}
	if strings.Contains(err.Error(), "drain timed out") {
		t.Fatalf("Drain timed out instead of the retry giving up: %v", err)
	}
	if got := fails.count(); got != 5 {
		t.Fatalf("onFail ran %d times, want 5 (once per publish)", got)
	}
	if got := global.count(); got != 5 {
		t.Fatalf("OnError ran %d times, want 5", got)
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("gave up after %v, before the 500ms deadline", elapsed)
	}
}

// TestPublisherRetryAttemptsAreBounded uses the one retryable failure a
// single server produces on demand and forever: a subject no stream
// captures, which nats.go reports as ErrNoStreamResponse. Two re-sends are
// allowed, so there must be exactly two, then the failure.
func TestPublisherRetryAttemptsAreBounded(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.maxAttempts = 2
	pr.pub.retry.backoffMin = 10 * time.Millisecond

	var fails failRecorder
	ev := testEvent("nostream/1")
	ev.Subject = "vantage.v1.nostream.nobody"
	if err := pr.pub.PublishNotify(ev, fails.record); err != nil {
		t.Fatal(err)
	}
	err := pr.pub.Drain(20 * time.Second)
	if !errors.Is(err, jetstream.ErrNoStreamResponse) || !errors.Is(err, errRetryAttempts) {
		t.Fatalf("Drain error = %v; want ErrNoStreamResponse and errRetryAttempts", err)
	}
	if got := pr.retries.Load(); got != 2 {
		t.Fatalf("OnRetry ran %d times, want 2", got)
	}
	if got := fails.count(); got != 1 {
		t.Fatalf("onFail ran %d times, want 1", got)
	}
}

// TestPublisherNoStreamIsReportedAfterTheBackoffWindow: the no-responders
// signal cannot tell a leaderless stream from a subject no stream captures,
// so the permanent case is still reported once backoffWindow has passed,
// with attempts to spare.
func TestPublisherNoStreamIsReportedAfterTheBackoffWindow(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.backoffWindow = 300 * time.Millisecond
	pr.pub.retry.backoffMin = 20 * time.Millisecond
	pr.pub.retry.backoffMax = 40 * time.Millisecond

	ev := testEvent("nostream/1")
	ev.Subject = "vantage.v1.nostream.nobody"
	if err := pr.pub.Publish(ev); err != nil {
		t.Fatal(err)
	}
	err := pr.pub.Drain(20 * time.Second)
	if !errors.Is(err, jetstream.ErrNoStreamResponse) || !errors.Is(err, errRetryWindow) {
		t.Fatalf("Drain error = %v; want ErrNoStreamResponse and errRetryWindow", err)
	}
	if got := pr.retries.Load(); got < 1 {
		t.Fatalf("OnRetry ran %d times; the no-responders failure was never retried", got)
	}
}

// TestPublisherDoesNotRetryAJetStreamRejection: a stream full under
// DiscardNew answers every publish past its limit with an API error. The
// server has answered; the failure is reported at once, with no re-send.
func TestPublisherDoesNotRetryAJetStreamRejection(t *testing.T) {
	pr := newProxied(t)
	createFullStream(t, pr.direct)

	var accepted, rejected failRecorder
	first, second := fullEvent("full/1"), fullEvent("full/2")
	if err := pr.pub.PublishNotify(first, accepted.record); err != nil {
		t.Fatal(err)
	}
	if err := pr.pub.Drain(5 * time.Second); err != nil {
		t.Fatalf("first publish into the one-message stream: %v", err)
	}
	start := time.Now()
	if err := pr.pub.PublishNotify(second, rejected.record); err != nil {
		t.Fatal(err)
	}
	err := pr.pub.Drain(5 * time.Second)
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Drain error = %v; want a JetStream API error", err)
	}
	if got := pr.retries.Load(); got != 0 {
		t.Fatalf("OnRetry ran %d times for a JetStream rejection, want 0", got)
	}
	if got := rejected.count(); got != 1 {
		t.Fatalf("onFail ran %d times, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the rejection took %v to report; it should be immediate", elapsed)
	}
}

// createFullStream creates FULL: one message, DiscardNew, so the second
// publish to it is rejected by the server.
func createFullStream(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: "FULL", Subjects: []string{"vantage.v1.full.>"},
		MaxMsgs: 1, Discard: jetstream.DiscardNew,
	}); err != nil {
		t.Fatal(err)
	}
}

func fullEvent(msgID string) collector.Event {
	ev := testEvent(msgID)
	ev.Subject = "vantage.v1.full.x"
	return ev
}

// TestPublisherRetryingIsBounded: while maxRetrying publishes are retrying,
// a new publish waits for room rather than adding to them, and goes through
// once the retries resolve. (Failing after capacityWait is
// TestAFullBudgetIsWaitedOutOncePerOutage.)
func TestPublisherRetryingIsBounded(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.maxRetrying = 2
	pr.pub.retry.capacityWait = 20 * time.Second
	var fails failRecorder

	// All five were in flight at the cut: they all retry, over the bound,
	// because they were sent before it applied.
	pr.cutAndRefuse(t, 5, &fails)
	waitRetrying(t, pr, 5)

	gated := make(chan error, 1)
	go func() { gated <- pr.pub.PublishNotify(testEvent("gated/1"), fails.record) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-gated:
		t.Fatalf("PublishNotify at capacity returned %v without waiting for room", err)
	default:
	}
	pr.proxy.Allow()
	if err := <-gated; err != nil {
		t.Fatalf("PublishNotify waiting for room: %v", err)
	}
	if err := pr.pub.Drain(20 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := fails.count(); got != 0 {
		t.Fatalf("onFail ran %d times, want 0", got)
	}
	pr.assertNoneRetrying(t)
	if got := len(pr.routesMsgIDs(t)); got != 7 {
		t.Fatalf("ROUTES holds %d distinct msg-ids, want 7 (warm-up, 5 retried, 1 gated)", got)
	}
}

// TestDrainAccountsForARetryInProgress: a shutdown that cannot wait for a
// retry reports it as unresolved, and once the connection closes the retry
// fails -- it is never dropped on the floor.
func TestDrainAccountsForARetryInProgress(t *testing.T) {
	pr := newProxied(t)
	var fails failRecorder

	pr.cutAndRefuse(t, 3, &fails)
	deadline := time.Now().Add(10 * time.Second)
	for pr.retryingCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d publishes retrying, want 3 before the shutdown", pr.retryingCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	err := pr.pub.Drain(200 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "3 publishes unresolved, 3 of them retrying") {
		t.Fatalf("Drain during the retry = %v; want a timeout naming 3 publishes retrying", err)
	}
	// Each is named with the failure it is retrying, so a caller that gives
	// up waiting still learns why.
	if !errors.Is(err, nats.ErrDisconnected) || strings.Count(err.Error(), "still retrying after") != 3 {
		t.Fatalf("Drain during the retry = %v; want each of the 3 named with nats.ErrDisconnected", err)
	}
	if got := pr.retries.Load(); got != 0 {
		t.Fatalf("OnRetry ran %d times with the connection refused; it must count re-sends actually sent", got)
	}

	pr.nc.Close()
	err = pr.pub.Drain(5 * time.Second)
	if !errors.Is(err, nats.ErrConnectionClosed) || !errors.Is(err, nats.ErrDisconnected) {
		t.Fatalf("Drain after close = %v; want ErrConnectionClosed and ErrDisconnected", err)
	}
	if got := fails.count(); got != 3 {
		t.Fatalf("onFail ran %d times, want 3", got)
	}
}

// TestRetryBoundsFitTheDuplicateWindow holds the relation the retry's
// correctness rests on: a re-send of a message the server did store is only
// deduplicated inside the stream's duplicate window. The last re-send starts
// by publishRetryDeadline after the original publish, and the other half of
// the window is margin for it to arrive.
func TestRetryBoundsFitTheDuplicateWindow(t *testing.T) {
	for _, c := range buildStreamConfigs(testOpts) {
		if c.Duplicates != streamDuplicateWindow {
			t.Errorf("stream %s: Duplicates = %v, want streamDuplicateWindow (%v)", c.Name, c.Duplicates, streamDuplicateWindow)
		}
	}
	if publishRetryDeadline > streamDuplicateWindow/2 {
		t.Errorf("publishRetryDeadline %v exceeds half the %v duplicate window", publishRetryDeadline, streamDuplicateWindow)
	}
	if backoffRetryWindow > publishRetryDeadline {
		t.Errorf("backoffRetryWindow %v exceeds publishRetryDeadline %v", backoffRetryWindow, publishRetryDeadline)
	}
}

// TestRetryClass pins the classification, so a change to it is a change to
// this table.
func TestRetryClass(t *testing.T) {
	for _, c := range []struct {
		err  error
		want retryKind
	}{
		{nats.ErrDisconnected, retryOnReconnect},
		{fmt.Errorf("wrapped: %w", nats.ErrDisconnected), retryOnReconnect},
		{nats.ErrReconnectBufExceeded, retryOnReconnect},
		{jetstream.ErrNoStreamResponse, retryNoResponders},
		{jetstream.ErrTooManyStalledMsgs, retryAfterBackoff},
		{jetstream.ErrAsyncPublishTimeout, retryAfterBackoff},
		{&jetstream.APIError{Code: 503, Description: "raft: not leader"}, retryAfterBackoff},
		{fmt.Errorf("wrapped: %w", &jetstream.APIError{Code: 503, Description: "raft: not leader"}), retryAfterBackoff},
		{&jetstream.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"}, retryAfterBackoff},
		{&jetstream.APIError{Code: 400, ErrorCode: 10071, Description: "wrong last sequence: 5"}, retryNever},
		// The same 503 carries a storage write error, which is not retried.
		{&jetstream.APIError{Code: 503, Description: "write error: no space left on device"}, retryNever},
		{&jetstream.APIError{Code: 503, ErrorCode: 10023, Description: "insufficient resources"}, retryNever},
		{&jetstream.APIError{Code: 400, ErrorCode: 10077, Description: "maximum messages exceeded"}, retryNever},
		{nats.ErrConnectionClosed, retryNever},
		{nats.ErrNoResponders, retryNever},
		{nats.ErrMaxPayload, retryNever},
		{&jetstream.APIError{Code: 503, ErrorCode: jetstream.JSErrCodeStreamNotFound}, retryNever},
		{errors.New("maximum messages exceeded"), retryNever},
	} {
		if got := retryClass(c.err); got != c.want {
			t.Errorf("retryClass(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestNewPublisherInstallsBoundsInsideTheDuplicateWindow checks the policy a
// Publisher actually runs with, not only the constants: a default that
// drifted from them would pass TestRetryBoundsFitTheDuplicateWindow.
func TestNewPublisherInstallsBoundsInsideTheDuplicateWindow(t *testing.T) {
	p := NewPublisher(natstest.RunJS(t))
	if p.retry.deadline <= 0 || p.retry.deadline > streamDuplicateWindow/2 {
		t.Errorf("installed retry deadline %v; want positive and at most half the %v duplicate window", p.retry.deadline, streamDuplicateWindow)
	}
	if p.retry.backoffWindow <= 0 || p.retry.backoffWindow > p.retry.deadline {
		t.Errorf("installed backoff window %v; want positive and at most the deadline %v", p.retry.backoffWindow, p.retry.deadline)
	}
	if p.retry.maxAttempts <= 0 || p.retry.maxRetrying <= 0 {
		t.Errorf("installed attempts %d, capacity %d; want both positive", p.retry.maxAttempts, p.retry.maxRetrying)
	}
}

// TestRetryDeadlineRunsFromTheOriginalPublish: the duplicate window runs
// from when the server first saw the message, so the retry deadline must too.
// A black-hole subscriber on the subject makes every attempt time out after
// the ack timeout, the one failure that repeats with no bound of its own.
// Measured from each failure instead, the deadline would never pass and the
// publish would re-send until maxAttempts.
func TestRetryDeadlineRunsFromTheOriginalPublish(t *testing.T) {
	srv := natstest.RunServer(t)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	const subject = "vantage.v1.blackhole.x"
	sub, err := nc.Subscribe(subject, func(*nats.Msg) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	p := NewPublisher(js)
	p.retry.deadline = 700 * time.Millisecond
	p.retry.maxAttempts = 50
	p.retry.backoffMin, p.retry.backoffMax = 10*time.Millisecond, 10*time.Millisecond
	var resends atomic.Int64
	p.OnRetry = func(error) { resends.Add(1) }

	ev := testEvent("blackhole/1")
	ev.Subject = subject
	start := time.Now()
	if err := p.Publish(ev); err != nil {
		t.Fatal(err)
	}
	err = p.Drain(10 * time.Second)
	elapsed := time.Since(start)
	if !errors.Is(err, jetstream.ErrAsyncPublishTimeout) || !errors.Is(err, errRetryDeadline) {
		t.Fatalf("Drain = %v; want ErrAsyncPublishTimeout and errRetryDeadline", err)
	}
	if got := resends.Load(); got < 1 || got > 4 {
		t.Fatalf("%d re-sends; with 200ms attempts and a 700ms deadline from the original publish, want 1 to 4", got)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("gave up after %v; the deadline was 700ms from the original publish", elapsed)
	}
}

// TestWaitConnectedDoesNotDependOnTheStatusEvent: nats.go v1.49.0 drops a
// status listener whose previous event is still unread when the next one
// arrives (sendStatusEvent), so after two quick reconnects the listener can
// be gone. waitConnected must still notice the connection is back. The
// listener is replaced here by one that never fires, which is what a dropped
// one looks like.
func TestWaitConnectedDoesNotDependOnTheStatusEvent(t *testing.T) {
	srv := natstest.RunServer(t)
	proxy := natstest.NewProxy(t, srv.Addr().String())
	nc, err := nats.Connect(proxy.URL(), nats.MaxReconnects(-1), nats.ReconnectWait(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	orig := statusListener
	statusListener = func(*nats.Conn) chan nats.Status { return make(chan nats.Status) }
	defer func() { statusListener = orig }()

	proxy.Refuse()
	proxy.Sever()
	for nc.IsConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- waitConnected(nc, time.Now().Add(10*time.Second)) }()
	time.Sleep(50 * time.Millisecond)
	proxy.Allow()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitConnected = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitConnected did not return within 5s of the reconnect")
	}
}

// TestABackoffPastTheWindowIsReportedAsTheWindow: a backoff that would end
// past the backoff window is not waited out, and the reason reported is the
// window, not the overall deadline -- the two say different things to an
// operator (a missing stream versus NATS unreachable).
func TestABackoffPastTheWindowIsReportedAsTheWindow(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.backoffWindow = 300 * time.Millisecond
	pr.pub.retry.backoffMin, pr.pub.retry.backoffMax = 5*time.Second, 5*time.Second

	ev := testEvent("nostream/1")
	ev.Subject = "vantage.v1.nostream.nobody"
	start := time.Now()
	if err := pr.pub.Publish(ev); err != nil {
		t.Fatal(err)
	}
	err := pr.pub.Drain(10 * time.Second)
	if !errors.Is(err, errRetryWindow) || errors.Is(err, errRetryDeadline) {
		t.Fatalf("Drain = %v; want errRetryWindow and not errRetryDeadline", err)
	}
	if got := pr.retries.Load(); got != 0 {
		t.Fatalf("%d re-sends; the only backoff ended past the window, so want 0", got)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("gave up after %v: it waited out a backoff that ended past the window", elapsed)
	}
}

// TestAFirstSendThatFindsTheAsyncWindowFullIsRetried: with the async window
// full -- acks held, as during a leader change or a reconnect burst -- a new
// publish stalls and nats.go fails it synchronously with
// ErrTooManyStalledMsgs. That used to be returned, closing the session. It is
// retried instead, like the same error on a re-send, and succeeds once acks
// flow again.
func TestAFirstSendThatFindsTheAsyncWindowFullIsRetried(t *testing.T) {
	srv := natstest.RunServer(t)
	proxy := natstest.NewProxy(t, srv.Addr().String())
	nc, err := nats.Connect(proxy.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncMaxPending(1), jetstream.WithPublishAsyncTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(js)
	p.retry.backoffMin = 20 * time.Millisecond
	var resends atomic.Int64
	p.OnRetry = func(error) { resends.Add(1) }
	var fails failRecorder

	proxy.FreezeToClient()
	if err := p.PublishNotify(testEvent("window/1"), fails.record); err != nil {
		t.Fatal(err)
	}
	if err := p.PublishNotify(testEvent("window/2"), fails.record); err != nil {
		t.Fatalf("PublishNotify with the async window full = %v; want it retried, not returned", err)
	}
	time.Sleep(100 * time.Millisecond)
	proxy.ThawToClient()
	if err := p.Drain(10 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := fails.count(); got != 0 {
		t.Fatalf("onFail ran %d times, want 0", got)
	}
	if resends.Load() == 0 {
		t.Fatal("no re-send: the second publish never found the window full, so this proved nothing")
	}
}

// TestAFullBudgetIsWaitedOutOncePerOutage: once one publish has waited
// capacityWait and found no room, the next publishes to that stream fail at
// once instead of each waiting in turn. A failing session's remaining events
// and its close-out used to pay the wait one after another, which held
// shutdown for the number of peers times capacityWait. When room appears,
// publishing resumes.
func TestAFullBudgetIsWaitedOutOncePerOutage(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.maxRetrying = 2
	pr.pub.retry.capacityWait = 300 * time.Millisecond
	var fails failRecorder

	pr.cutAndRefuse(t, 5, &fails)
	waitRetrying(t, pr, 5)
	start := time.Now()
	for i := range 5 {
		if err := pr.pub.PublishNotify(testEvent(fmt.Sprintf("full/%d", i)), fails.record); !errors.Is(err, errRetryCapacity) {
			t.Fatalf("publish %d with the budget full = %v; want errRetryCapacity", i, err)
		}
		if i == 0 && time.Since(start) < 250*time.Millisecond {
			t.Fatalf("the first publish against a full budget failed after %v, without waiting for room", time.Since(start))
		}
	}
	if elapsed := time.Since(start); elapsed > 550*time.Millisecond {
		t.Fatalf("5 publishes against a full budget took %v; want one 300ms wait, not five", elapsed)
	}

	pr.proxy.Allow()
	if err := pr.pub.Drain(20 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := pr.pub.PublishNotify(testEvent("after/1"), fails.record); err != nil {
		t.Fatalf("publish after the budget emptied = %v", err)
	}
	if err := pr.pub.Drain(10 * time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The next outage must be waited out again: room cleared the saturated
	// state. The publish above cannot show that, because it found the
	// budget empty and never looked at the flag.
	pr.proxy.FreezeToClient()
	publishN(t, pr.pub, "held-again", 3, &fails)
	pr.waitRoutesMsgs(t, uint64(1+5+1+3))
	pr.proxy.Refuse()
	pr.proxy.Sever()
	waitRetrying(t, pr, 3)
	start = time.Now()
	if err := pr.pub.PublishNotify(testEvent("full-again/1"), fails.record); !errors.Is(err, errRetryCapacity) {
		t.Fatalf("publish in the second outage = %v; want errRetryCapacity", err)
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("the first publish of the second outage failed after %v without waiting: room did not clear the saturated state", waited)
	}
}

// TestTheWaitForRoomIsOneWaitAcrossWakes: a waiter woken when room appears
// that is taken again before it looks, waits only for what is left of its
// capacityWait, not a fresh one. The budget is filled directly and the room
// channel closed with the budget still full, which is that sequence with
// the race removed.
func TestTheWaitForRoomIsOneWaitAcrossWakes(t *testing.T) {
	p := NewPublisher(natstest.RunJS(t))
	const wait = 400 * time.Millisecond
	p.retry.maxRetrying = 2
	p.retry.capacityWait = wait
	p.mu.Lock()
	p.budgetLocked("route").n = 2
	p.mu.Unlock()

	type result struct {
		err     error
		elapsed time.Duration
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			start := time.Now()
			err := p.waitRetryRoom("route")
			results <- result{err, time.Since(start)}
		}()
	}
	time.Sleep(250 * time.Millisecond)
	p.mu.Lock()
	b := p.budgetLocked("route")
	if b.room == nil {
		p.mu.Unlock()
		t.Fatal("no waiter is waiting for room")
	}
	close(b.room) // room appeared, and was taken again at once: n is still 2
	b.room = nil
	p.mu.Unlock()

	for range 2 {
		r := <-results
		if !errors.Is(r.err, errRetryCapacity) {
			t.Fatalf("waitRetryRoom = %v; want errRetryCapacity", r.err)
		}
		if r.elapsed > wait+150*time.Millisecond {
			t.Fatalf("a waiter woken once returned after %v; its whole wait was %v", r.elapsed, wait)
		}
	}
}

// TestStopWaitingEndsTheWaitForRoom: shutdown must not wait capacityWait for
// each session holding a publish at a full budget.
func TestStopWaitingEndsTheWaitForRoom(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.maxRetrying = 2
	pr.pub.retry.capacityWait = 30 * time.Second
	var fails failRecorder

	pr.cutAndRefuse(t, 3, &fails)
	waitRetrying(t, pr, 3)
	done := make(chan error, 1)
	go func() { done <- pr.pub.PublishNotify(testEvent("waiting/1"), fails.record) }()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	pr.pub.StopWaiting()
	select {
	case err := <-done:
		if !errors.Is(err, errRetryCapacity) {
			t.Fatalf("waiting publish after StopWaiting = %v; want errRetryCapacity", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a publish waiting for room was still waiting 2s after StopWaiting")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("StopWaiting took %v to end the wait", elapsed)
	}
	start = time.Now()
	if err := pr.pub.PublishNotify(testEvent("waiting/2"), fails.record); !errors.Is(err, errRetryCapacity) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("publish after StopWaiting = %v after %v; want errRetryCapacity at once", err, time.Since(start))
	}
}

// TestOneStreamsBudgetDoesNotBlockAnother: RAW is a single copy, so while its
// server restarts every RAW publish retries no-responders for the backoff
// window. That must not use up the room ROUTES publishes need. Here RAW is
// deleted outright, which answers the same way, and fills its budget.
func TestOneStreamsBudgetDoesNotBlockAnother(t *testing.T) {
	pr := newProxied(t)
	pr.pub.retry.maxRetrying = 2
	pr.pub.retry.capacityWait = 200 * time.Millisecond
	pr.pub.retry.backoffMin, pr.pub.retry.backoffMax = 5*time.Second, 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pr.direct.DeleteStream(ctx, "RAW"); err != nil {
		t.Fatal(err)
	}
	var fails failRecorder
	raw := func(id string) collector.Event {
		ev := testEvent(id)
		ev.Subject = "vantage.v1.raw.0a000001"
		return ev
	}
	for i := range 2 {
		if err := pr.pub.PublishNotify(raw(fmt.Sprintf("raw/%d", i)), fails.record); err != nil {
			t.Fatal(err)
		}
	}
	waitRetrying(t, pr, 2)
	if err := pr.pub.PublishNotify(raw("raw/over"), fails.record); !errors.Is(err, errRetryCapacity) {
		t.Fatalf("RAW publish with RAW's budget full = %v; want errRetryCapacity", err)
	}
	start := time.Now()
	if err := pr.pub.PublishNotify(testEvent("routes/1"), fails.record); err != nil {
		t.Fatalf("ROUTES publish with RAW's budget full = %v; want it sent", err)
	}
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Fatalf("ROUTES publish waited %v on RAW's budget", waited)
	}
	pr.waitRoutesMsgs(t, 2) // warm-up + routes/1
	if got := fails.count(); got != 0 {
		t.Fatalf("onFail ran %d times; the RAW retries are still within their window", got)
	}
}

// TestTransientFailuresBackOff pins the backoff for the failures that are
// not waiting on a reconnect -- here the ack timeout, the one a leader change
// produces. Re-sent without it, each timeout would be followed at once by
// another send into the same leaderless stream.
func TestTransientFailuresBackOff(t *testing.T) {
	srv := natstest.RunServer(t)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	const ackTimeout, backoff = 100 * time.Millisecond, 300 * time.Millisecond
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(ackTimeout))
	if err != nil {
		t.Fatal(err)
	}
	const subject = "vantage.v1.blackhole.x"
	sub, err := nc.Subscribe(subject, func(*nats.Msg) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	p := NewPublisher(js)
	p.retry.deadline = 1500 * time.Millisecond
	p.retry.backoffMin, p.retry.backoffMax = backoff, backoff
	var mu sync.Mutex
	var at []time.Time
	p.OnRetry = func(error) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
	}
	ev := testEvent("backoff/1")
	ev.Subject = subject
	if err := p.Publish(ev); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(10 * time.Second); !errors.Is(err, jetstream.ErrAsyncPublishTimeout) {
		t.Fatalf("Drain = %v; want ErrAsyncPublishTimeout", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) < 2 {
		t.Fatalf("%d re-sends; want at least 2 to measure the gap", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < ackTimeout+backoff-50*time.Millisecond {
			t.Fatalf("re-sends %d and %d were %v apart; want the %v ack timeout plus the %v backoff", i, i+1, gap, ackTimeout, backoff)
		}
	}
}

func waitRetrying(t *testing.T, pr proxied, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for pr.retryingCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d publishes retrying, want %d", pr.retryingCount(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStreamOf pins which budget each subject counts against: one per
// stream, whatever the rest of the subject.
func TestStreamOf(t *testing.T) {
	for subject, want := range map[string]string{
		"vantage.v1.route.ipv4u.0a000001.0a000009": "route",
		"vantage.v1.route.vpn4.0a000002.0a000003":  "route",
		"vantage.v1.ls.0a000001.0a000009":          "ls",
		"vantage.v1.peer.0a000001.0a000009":        "peer",
		"vantage.v1.stats.0a000001.0a000009":       "stats",
		"vantage.v1.raw.0a000001":                  "raw",
		// Everything else shares one budget, so budgets stay bounded.
		"other.subject":          otherStreams,
		"vantage.v1.nostream.x":  otherStreams,
		"vantage.v1.blackhole.x": otherStreams,
		"vantage.v1route.x":      otherStreams,
		"vantage.v1.routes.x":    otherStreams,
	} {
		if got := streamOf(subject); got != want {
			t.Errorf("streamOf(%q) = %q, want %q", subject, got, want)
		}
	}
}
