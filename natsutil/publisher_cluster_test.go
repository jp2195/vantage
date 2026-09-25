package natsutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/natsutil/natstest"
)

// TestPublisherRetriesAcrossALameDuckStreamLeader is the failure a live
// rolling restart of the three-server NATS cluster still produced after the
// disconnect retry: the ROUTES leader's server enters lame-duck mode, steps
// down, and drops the proposals it had in flight. Measured against this
// embedded cluster, those publishes either get no reply at all -- and fail
// with jetstream.ErrAsyncPublishTimeout once the ack timeout passes -- or a
// 503 "raft: not leader", and their re-sends can meet a 409 "duplicate
// message id is in process" first. The client's connection never drops: it
// is on a server that is not the leader.
//
// Every publish must be reported accepted and ROUTES must hold exactly as
// many messages as were published: none lost, none stored twice.
//
// Whether a leader change catches a publish in flight is timing. Over 20
// runs, 3 dropped nothing, and such a run proves nothing about the retry.
// So the scenario is repeated on a fresh cluster until one drops something,
// and the test is skipped, saying so, only if none of the attempts did.
func TestPublisherRetriesAcrossALameDuckStreamLeader(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a three-server cluster")
	}
	const attempts = 4
	for i := 1; i <= attempts; i++ {
		var drops int64
		if !t.Run(fmt.Sprintf("attempt-%d", i), func(t *testing.T) { drops = lameDuckStreamLeader(t) }) {
			return
		}
		if drops > 0 {
			return
		}
		t.Logf("attempt %d: the leader change dropped no publish in flight; repeating on a new cluster", i)
	}
	t.Skipf("in %d lame-duck leader changes no publish was in flight at the moment of the change, "+
		"so nothing exercised the retry; timing, not a failure", attempts)
}

// lameDuckStreamLeader runs the scenario once on a new cluster, fails t on
// any lost, duplicated or failed publish, and returns how many re-sends
// followed an ack timeout or a not-leader answer: the drops the leader change
// caused.
func lameDuckStreamLeader(t *testing.T) int64 {
	c := natstest.RunCluster(t)
	setupNC, err := nats.Connect(c.Servers[0].ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	setup, err := jetstream.New(setupNC)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opts := testOpts
	opts.Replicas = 3
	// No retry: natstest.RunCluster waits for every route, so the create
	// response always has a path back to this client (see routesUp).
	if err := EnsureStreams(ctx, setup, opts); err != nil {
		t.Fatalf("EnsureStreams on the cluster: %v", err)
	}
	setupNC.Close()

	leader := c.StreamLeader(t, "ROUTES")
	nc, err := nats.Connect(c.Other(leader).ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncMaxPending(4096),
		jetstream.WithPublishAsyncTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(js)
	var retries, leaderDrops atomic.Int64
	var causesMu sync.Mutex
	causes := map[string]int{} // re-sends by the failure they followed, for the log
	p.OnRetry = func(err error) {
		retries.Add(1)
		if errors.Is(err, jetstream.ErrAsyncPublishTimeout) || isNotLeader(err) {
			leaderDrops.Add(1)
		}
		cause := err
		for errors.Unwrap(cause) != nil {
			cause = errors.Unwrap(cause)
		}
		causesMu.Lock()
		causes[cause.Error()]++
		causesMu.Unlock()
	}
	defer func() {
		causesMu.Lock()
		defer causesMu.Unlock()
		t.Logf("re-sends by cause: %v", causes)
	}()
	var fails failRecorder

	// Until the client's server has learned the new leader's interest in
	// ROUTES, publishes get no responders; wait that out so every re-send
	// below is the leader change's.
	if err := p.Publish(testEvent("warmup/1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(30 * time.Second); err != nil {
		t.Fatalf("warm-up publish: %v", err)
	}
	retries.Store(0)
	leaderDrops.Store(0)

	stop := make(chan struct{})
	published := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				published <- n
				return
			default:
			}
			n++
			// About 20k publishes a second, several times a busy
			// collector's rate. Unthrottled, the embedded cluster on a
			// loaded test machine falls far enough behind that thousands of
			// publishes are still retrying 10s later, which tests the load
			// rather than the leader change.
			if n%20 == 0 {
				time.Sleep(time.Millisecond)
			}
			if err := p.PublishNotify(testEvent(fmt.Sprintf("leader/%d", n)), fails.record); err != nil {
				t.Errorf("PublishNotify (synchronous) %d: %v", n, err)
				published <- n - 1
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	leader.LameDuckShutdown() // returns once the server has shut down
	time.Sleep(500 * time.Millisecond)
	close(stop)
	n := <-published

	if err := p.Drain(30 * time.Second); err != nil {
		t.Fatalf("Drain after %d publishes across a lame-duck stream leader: %v", n, err)
	}
	if got := fails.count(); got != 0 {
		t.Fatalf("%d publishes reported failure to their session", got)
	}
	s, err := js.Stream(ctx, "ROUTES")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// LastSeq, not Msgs: the test's 16 MiB byte limit discards the oldest
	// messages long before this many have been published. Every stored
	// message takes the next stream sequence and a deduplicated re-send
	// takes none, so LastSeq is how many distinct messages were stored.
	if info.State.LastSeq != uint64(n)+1 { // +1: the warm-up
		t.Fatalf("ROUTES stored %d messages after %d publishes with distinct msg-ids", info.State.LastSeq, n)
	}
	t.Logf("%d publishes, %d re-sends, %d after a timeout or not-leader answer", n, retries.Load(), leaderDrops.Load())
	return leaderDrops.Load()
}
