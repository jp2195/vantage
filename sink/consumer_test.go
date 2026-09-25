package sink

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/natsutil/natstest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

type fakeInserter struct {
	mu    sync.Mutex
	calls []Rows
	err   error
}

func (f *fakeInserter) Insert(_ context.Context, r Rows) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	return f.err
}

func (f *fakeInserter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// testStreamOpts mirrors collector's e2e test: the embedded natstest server
// is a single, non-clustered node, which rejects any stream asking for more
// than one replica.
// RoutesMaxBytes/RawMaxBytes are bounded because nats-server validates a
// stream's MaxBytes against the FREE SPACE of the JetStream store
// directory's filesystem, not against the account limit -- an unlimited
// account still refuses a stream larger than the disk can hold. The
// production defaults (8 GiB ROUTES, 2 GiB RAW) exceeded what the CI
// runner's temp filesystem had free, so every test that provisioned
// streams failed there with "insufficient storage resources available"
// while passing locally on a bigger disk. Tests publish kilobytes; 16 MiB
// is far above anything they write and fits anywhere.
var testStreamOpts = natsutil.StreamOpts{Replicas: 1, LSReplicas: 1,
	RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20}

// publishStatsEnvelopes publishes n StatsEvent envelopes to the STATS
// stream, each of which RowsFor turns into exactly one StatsRow -- a
// deterministic, single-table row count that keeps the batching arithmetic
// in these tests simple.
func publishStatsEnvelopes(t *testing.T, ctx context.Context, js jetstream.JetStream, n int) {
	t.Helper()
	for i := range n {
		env := &vantagev1.Envelope{
			CollectorId: "c1",
			Payload: &vantagev1.Envelope_Stats{
				Stats: &vantagev1.StatsEvent{Counters: map[uint32]uint64{0: uint64(i)}},
			},
		}
		data, err := proto.Marshal(env)
		if err != nil {
			t.Fatalf("marshal envelope %d: %v", i, err)
		}
		if _, err := js.Publish(ctx, "vantage.v1.stats.test", data); err != nil {
			t.Fatalf("publish envelope %d: %v", i, err)
		}
	}
}

// A failed insert must leave the messages unacked so JetStream redelivers.
// Acking on receipt would lose the batch outright.
func TestConsumerDoesNotAckOnInsertFailure(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	const n = 3
	publishStatsEnvelopes(t, ctx, js, n)

	f := &fakeInserter{err: errors.New("clickhouse unreachable")}
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-fail",
		BatchRows: 100, BatchWait: 100 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()

	// Wait until the consumer has attempted at least one insert -- proof it
	// pulled the messages and tried the batch, not just that it started.
	deadline := time.Now().Add(10 * time.Second)
	for f.count() == 0 {
		if time.Now().After(deadline) {
			runCancel()
			<-done
			t.Fatal("inserter was never called")
		}
		time.Sleep(10 * time.Millisecond)
	}

	runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	// Ask JetStream itself, not the consumer, whether anything was acked.
	// AckFloor.Stream only ever advances past a sequence once it (and every
	// sequence before it) has been acked, so it staying at 0 is direct proof
	// that not one of the n messages was acked despite the consumer having
	// fetched and attempted to insert all of them. NumAckPending should
	// account for every message still outstanding: nothing should have been
	// lost or silently dropped either.
	//
	// Msg.Ack() (the only ack site in Run, gated on a nil Insert return) is a
	// fire-and-forget publish -- see TestConsumerFlushesOnTimeBound's settle
	// poll below for why that matters for a *positive* assertion. Here the
	// assertion is negative ("never becomes nonzero"), so a single read is
	// not vulnerable to that same race in the way a single read for a
	// positive assertion would be -- but sampling repeatedly over a short
	// window is still strictly stronger evidence than one read, and it's
	// what "stays at 0" should mean literally: never observed to move, not
	// just not-yet-moved at one instant.
	cons, err := js.Consumer(ctx, "STATS", "test-fail")
	if err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	settleDeadline := time.Now().Add(500 * time.Millisecond)
	for {
		info, err := cons.Info(ctx)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.AckFloor.Stream != 0 {
			t.Fatalf("AckFloor.Stream = %d, want 0 -- a failed insert must never be acked", info.AckFloor.Stream)
		}
		if outstanding := info.NumAckPending + int(info.NumPending); outstanding != n {
			t.Fatalf("NumAckPending(%d)+NumPending(%d) = %d, want %d -- every message must remain outstanding",
				info.NumAckPending, info.NumPending, outstanding, n)
		}
		if time.Now().After(settleDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A partial batch must still flush once the time bound elapses, or a quiet
// network would hold rows indefinitely.
func TestConsumerFlushesOnTimeBound(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	// Publish fewer envelopes than BatchRows so the only way the consumer
	// can flush is the time bound, not the size bound.
	const n = 2
	publishStatsEnvelopes(t, ctx, js, n)

	f := &fakeInserter{}
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-timebound",
		BatchRows: 1000, BatchWait: 200 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()

	deadline := time.Now().Add(10 * time.Second)
	for f.count() == 0 {
		if time.Now().After(deadline) {
			runCancel()
			<-done
			t.Fatal("inserter was never called -- batch did not flush on the time bound")
		}
		time.Sleep(10 * time.Millisecond)
	}

	runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	f.mu.Lock()
	got := f.calls[0].Len()
	f.mu.Unlock()
	if got != n {
		t.Fatalf("first flushed batch had %d rows, want %d", got, n)
	}

	// A successful insert must ack: confirm the messages are gone from the
	// consumer's outstanding set, which is the other half of the ack-after-
	// durable contract this package exists to guarantee. Msg.Ack() publishes
	// the ack asynchronously (fire-and-forget, not a round trip), so the
	// server may not have processed every ack the instant Run returns --
	// this polls rather than checking once, to avoid a timing flake.
	cons, err := js.Consumer(ctx, "STATS", "test-timebound")
	if err != nil {
		t.Fatalf("Consumer: %v", err)
	}
	ackDeadline := time.Now().Add(5 * time.Second)
	var info *jetstream.ConsumerInfo
	for {
		info, err = cons.Info(ctx)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.AckFloor.Stream == n || time.Now().After(ackDeadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if info.AckFloor.Stream != n {
		t.Fatalf("AckFloor.Stream = %d, want %d -- a successful insert must ack", info.AckFloor.Stream, n)
	}
}

// validConsumerConfig is a complete, legal ConsumerConfig. The
// stream/positivity rejection tests each vary exactly one field off this
// baseline, so a failure in one field's check can't be masked by another
// field also being at its (invalid) zero value.
func validConsumerConfig() ConsumerConfig {
	return ConsumerConfig{
		Stream: "STATS", Durable: "d",
		BatchRows: 10, BatchWait: time.Second, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}
}

// A stream this package cannot produce rows for -- RAW chief among them --
// must be rejected at construction, or a misrouted consumer decodes every
// envelope, translates it to zero rows in every table (RowsFor has no
// Envelope_Raw case), no-ops the insert on the resulting all-empty Rows, and
// acks: a silent drain of RAW, the system's lossless replay stream, with no
// error anywhere in that chain to catch it.
func TestNewConsumerRejectsStreamNotInAllowlist(t *testing.T) {
	js := natstest.RunJS(t)

	for _, stream := range []string{"RAW", "NOT-A-REAL-STREAM", ""} {
		cfg := validConsumerConfig()
		cfg.Stream = stream
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
			t.Errorf("NewConsumer(Stream: %q) returned a nil error, want a rejection", stream)
		}
	}
	for _, stream := range []string{"ROUTES", "LS", "PEER", "STATS"} {
		cfg := validConsumerConfig()
		cfg.Stream = stream
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err != nil {
			t.Errorf("NewConsumer(Stream: %q) = %v, want nil error", stream, err)
		}
	}
}

// BatchWait <= 0 leaves the batch-accumulation deadline already in the past
// on entry, so the inner loop never calls Fetch even once and the outer
// loop spins at full rate with no sleep anywhere in that path -- closed at
// the source (a guard in NewConsumer) rather than left to a caller to
// avoid, since this is a value the config can never legally take, not a
// choice of value.
func TestNewConsumerRejectsNonPositiveBatchWait(t *testing.T) {
	js := natstest.RunJS(t)

	for _, bw := range []time.Duration{0, -time.Second} {
		cfg := validConsumerConfig()
		cfg.BatchWait = bw
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
			t.Errorf("NewConsumer(BatchWait: %v) returned a nil error, want a rejection", bw)
		}
	}
	if _, err := NewConsumer(js, validConsumerConfig(), &fakeInserter{}); err != nil {
		t.Errorf("NewConsumer with a valid config = %v, want nil error", err)
	}
}

// BatchRows <= 0 makes the inner loop's "rows.Len() < BatchRows" false
// immediately (rows.Len() starts at 0 every outer iteration), spinning the
// outer loop with no backoff exactly as a non-positive BatchWait does.
func TestNewConsumerRejectsNonPositiveBatchRows(t *testing.T) {
	js := natstest.RunJS(t)

	for _, br := range []int{0, -1} {
		cfg := validConsumerConfig()
		cfg.BatchRows = br
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
			t.Errorf("NewConsumer(BatchRows: %d) returned a nil error, want a rejection", br)
		}
	}
	if _, err := NewConsumer(js, validConsumerConfig(), &fakeInserter{}); err != nil {
		t.Errorf("NewConsumer with a valid config = %v, want nil error", err)
	}
}

// FetchBatch <= 0 does not spin -- JetStream's own pull-consumer Fetch
// rejects a batch size under 1 synchronously, which lands in Run's
// logged-and-backed-off synchronous-fetch-error branch -- but it makes the
// consumer permanently non-functional (every Fetch call fails forever), a
// no-op that looks alive while never doing anything, which construction
// should catch rather than leave to be noticed later in a log stream.
func TestNewConsumerRejectsNonPositiveFetchBatch(t *testing.T) {
	js := natstest.RunJS(t)

	for _, fb := range []int{0, -1} {
		cfg := validConsumerConfig()
		cfg.FetchBatch = fb
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
			t.Errorf("NewConsumer(FetchBatch: %d) returned a nil error, want a rejection", fb)
		}
	}
	if _, err := NewConsumer(js, validConsumerConfig(), &fakeInserter{}); err != nil {
		t.Errorf("NewConsumer with a valid config = %v, want nil error", err)
	}
}

// An empty Durable makes JetStream generate an ephemeral consumer, which the
// server can reap after inactivity -- and on reap, the consumer's position
// (this writer's only durable state) is lost with it. That must be rejected
// at construction, not discovered later as unexplained data loss.
func TestNewConsumerRejectsEmptyDurable(t *testing.T) {
	js := natstest.RunJS(t)

	cfg := validConsumerConfig()
	cfg.Durable = ""
	if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
		t.Errorf("NewConsumer(Durable: \"\") returned a nil error, want a rejection")
	}
	if _, err := NewConsumer(js, validConsumerConfig(), &fakeInserter{}); err != nil {
		t.Errorf("NewConsumer with a valid config = %v, want nil error", err)
	}
}

// A non-positive AckWait or MaxAckPending falls through to JetStream's own
// defaults (30s and 1000) rather than expressing "no opinion" -- and both
// defaults are wrong against this consumer's hold-the-batch-unacked-until-
// Insert-returns shape. Both must be rejected at construction, the same seam
// the stream allowlist and the batching-field guards above already use.
func TestNewConsumerRejectsNonPositiveAckWait(t *testing.T) {
	js := natstest.RunJS(t)

	for _, aw := range []time.Duration{0, -time.Second} {
		cfg := validConsumerConfig()
		cfg.AckWait = aw
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
			t.Errorf("NewConsumer(AckWait: %v) returned a nil error, want a rejection", aw)
		}
	}
	if _, err := NewConsumer(js, validConsumerConfig(), &fakeInserter{}); err != nil {
		t.Errorf("NewConsumer with a valid config = %v, want nil error", err)
	}
}

func TestNewConsumerRejectsNonPositiveMaxAckPending(t *testing.T) {
	js := natstest.RunJS(t)

	for _, m := range []int{0, -1} {
		cfg := validConsumerConfig()
		cfg.MaxAckPending = m
		if _, err := NewConsumer(js, cfg, &fakeInserter{}); err == nil {
			t.Errorf("NewConsumer(MaxAckPending: %d) returned a nil error, want a rejection", m)
		}
	}
	if _, err := NewConsumer(js, validConsumerConfig(), &fakeInserter{}); err != nil {
		t.Errorf("NewConsumer with a valid config = %v, want nil error", err)
	}
}

// An unrecoverable fetch failure -- here, the durable consumer deleted out
// from under Run, which is what a 409 "Consumer Deleted" from the server
// looks like from Run's side, and which CreateOrUpdateConsumer (called once,
// at the very top of Run) never notices or recreates -- must not spin. It
// must be logged, counted (metricFetchErrors), and backed off exactly as an
// insert failure is; otherwise the sink hammers the JetStream API subject at
// full rate, forever, with no signal anywhere that anything is wrong.
func TestConsumerBacksOffOnFetchFailure(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	f := &fakeInserter{}
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-fetcherr",
		BatchRows: 100, BatchWait: 100 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	before := testutil.ToFloat64(metricFetchErrors)

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()

	// Let Run create its durable consumer (its very first act) before
	// deleting it out from under Run -- the same effect a server-side
	// deletion or a stream failover would have. From here on every Fetch
	// fails with "no responders", not a timeout.
	waitDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := js.Consumer(ctx, "STATS", "test-fetcherr"); err == nil {
			break
		}
		if time.Now().After(waitDeadline) {
			runCancel()
			<-done
			t.Fatal("Run never created its durable consumer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := js.DeleteConsumer(ctx, "STATS", "test-fetcherr"); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}

	// Give the loop time to hit the failure repeatedly, then measure how
	// many times it actually did.
	time.Sleep(2 * time.Second)
	runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	got := testutil.ToFloat64(metricFetchErrors) - before
	if got == 0 {
		t.Fatal("metricFetchErrors did not increment -- the fetch failure was never observed")
	}
	// Backoff starts at minInsertBackoff (250ms) and doubles, so the
	// schedule predicts roughly 4 failures in 2s (250+500+1000+... sums to
	// ~1.75s of sleep by the 4th). 10 is generous headroom above that,
	// while still being far below the many hundreds or thousands a genuine
	// busy loop would produce in the same window -- comfortably separating
	// "backed off" from "spinning" without the test being timing-brittle.
	if got > 10 {
		t.Fatalf("metricFetchErrors incremented %v times in 2s -- backoff does not appear to be applied (spinning)", got)
	}
	if f.count() != 0 {
		t.Fatalf("Insert was called %d times, want 0 -- a fetch failure produces nothing to insert", f.count())
	}
}

// TestConsumerAcksEveryMessageItIsDelivered pins that a message JetStream
// delivers to Run is inserted and acked in the same pass, however its batch
// window ends. A delivered message the consumer drops is not lost -- the
// server redelivers it -- but only after AckWait, which is at least 60 s in
// cmd/vantage-writer. Until then it holds the consumer's ack floor back and
// the rows it carries are missing from ClickHouse.
//
// Run used to bound each Fetch by a context that expired with the batch
// window. When that context expired while the server was still delivering a
// pull request -- the window's last Fetch has only milliseconds left, and a
// busy server answers late -- nats.go ended the Fetch and discarded every
// message still on its way, a whole FetchBatch at a time.
//
// A very short BatchWait over a deep backlog puts a window edge in the
// middle of a delivery over and over. AckWait is far longer than the wait
// below, so the ack floor reaching the end of the stream in time means no
// message waited for a redelivery.
func TestConsumerAcksEveryMessageItIsDelivered(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	const n = 3000
	publishStatsEnvelopes(t, ctx, js, n)

	f := &fakeInserter{}
	const ackWait = 2 * time.Minute
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-window-edge",
		BatchRows: 100000, BatchWait: 5 * time.Millisecond, FetchBatch: 100,
		AckWait: ackWait, MaxAckPending: 100000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	defer func() {
		runCancel()
		<-done
	}()

	cons, err := js.Consumer(ctx, "STATS", "test-window-edge")
	for err != nil && ctx.Err() == nil {
		// Run creates the consumer; it may not exist yet.
		time.Sleep(10 * time.Millisecond)
		cons, err = js.Consumer(ctx, "STATS", "test-window-edge")
	}
	if err != nil {
		t.Fatalf("Consumer: %v", err)
	}

	// Half of AckWait: a run that needed a redelivery cannot finish inside
	// it, and a healthy one finishes in a few seconds even under -race.
	deadline := time.Now().Add(ackWait / 2)
	var info *jetstream.ConsumerInfo
	for {
		info, err = cons.Info(ctx)
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.AckFloor.Stream == n || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if info.AckFloor.Stream != n {
		t.Fatalf("ack floor stuck at %d of %d with %d delivered and unacked, %d redelivered so far: "+
			"the consumer dropped messages it was delivered",
			info.AckFloor.Stream, n, info.NumAckPending, info.NumRedelivered)
	}
	if info.Delivered.Consumer != n {
		t.Errorf("JetStream made %d deliveries of %d messages; every message should be delivered once",
			info.Delivered.Consumer, n)
	}
	rows := 0
	f.mu.Lock()
	for _, r := range f.calls {
		rows += r.Len()
	}
	f.mu.Unlock()
	if rows != n {
		t.Errorf("inserted %d rows, want %d", rows, n)
	}
}

// TestConsumerIdleStreamRecordsNoFetchErrors pins that a consumer sitting on
// a stream nobody is publishing to, the ordinary state of a low-rate stream
// (LS and PEER can be idle for hours), produces no fetch errors at all.
//
// Every batch window here closes empty, so the loop exercises the two benign
// exits -- the server ending a Fetch at the window's deadline, and the window
// closing before the next Fetch is sent -- over and over. Either one counted
// as a failure makes vantage_sink_fetch_errors_total meaningless as the
// "durable consumer deleted server-side" signal it exists to be, and drives
// the exponential backoff (which resets only on a successful Insert) toward
// its 30s ceiling on a healthy writer.
func TestConsumerIdleStreamRecordsNoFetchErrors(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	f := &fakeInserter{}
	// A deliberately short BatchWait: it is how many batch windows this test
	// gets through in its run, and every one is a chance to hit the race.
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-idle",
		BatchRows: 100, BatchWait: 5 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	before := testutil.ToFloat64(metricFetchErrors)
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	time.Sleep(2 * time.Second)
	runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if got := testutil.ToFloat64(metricFetchErrors) - before; got != 0 {
		t.Errorf("metricFetchErrors incremented %v times against an idle stream, "+
			"want 0 -- an empty batch window is not a fetch failure", got)
	}
	if f.count() != 0 {
		t.Errorf("Insert was called %d times against an idle stream, want 0", f.count())
	}
}

// TestConsumerCarriesLinkStateRowsToTheInserter covers the seam between
// RowsFor and Insert. Every other LS test in this package calls RowsFor and
// Insert directly, so nothing exercised the consumer's own per-message
// accumulation -- and that loop appended six of the eight row slices,
// silently dropping every typed BGP-LS node and link between a correct
// decode and a correct insert. The failure was invisible from both ends:
// RowsFor returned the rows, Insert accepted the (empty) batch, returned
// nil, and the consumer acked the envelope as durably stored while the
// topology it carried was gone for good.
func TestConsumerCarriesLinkStateRowsToTheInserter(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	// One envelope carrying one node and one link -- the shape a real XRd
	// BGP-LS UPDATE arrives in.
	env := &vantagev1.Envelope{
		CollectorId: "c1",
		Payload: &vantagev1.Envelope_Ls{
			Ls: &vantagev1.LsEvent{
				Family: &vantagev1.Family{Afi: 16388, Safi: 71},
				Nodes: []*vantagev1.LsNode{{
					Protocol: 3, Identifier: 100,
					Local: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}},
				}},
				Links: []*vantagev1.LsLink{{
					Protocol: 3, Identifier: 100,
					Local:  &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 2}},
					Remote: &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 4}},
				}},
			},
		},
	}
	data, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Published to ROUTES, not LS, deliberately: TestRunPublishesLagSeries-
	// BeforeAnyLoss is a canary that requires the envelopes_lost metric to
	// carry no stream="LS" label before it runs, and running a consumer on
	// LS here would create that label and silently defuse it. What is under
	// test is the consumer's per-message accumulation, which never inspects
	// the stream -- so any stream exercises it, and this one costs nothing.
	if _, err := js.Publish(ctx, "vantage.v1.route.test", data); err != nil {
		t.Fatalf("publish: %v", err)
	}

	f := &fakeInserter{}
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "ROUTES", Durable: "test-ls-rows",
		BatchRows: 1000, BatchWait: 200 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()

	deadline := time.Now().Add(10 * time.Second)
	for f.count() == 0 {
		if time.Now().After(deadline) {
			runCancel()
			<-done
			t.Fatal("inserter was never called")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	f.mu.Lock()
	got := f.calls[0]
	f.mu.Unlock()
	if len(got.LsNodes) != 1 {
		t.Errorf("inserter received %d LsNodes, want 1 -- the node decoded by RowsFor never reached Insert", len(got.LsNodes))
	}
	if len(got.LsLinks) != 1 {
		t.Errorf("inserter received %d LsLinks, want 1 -- the link decoded by RowsFor never reached Insert", len(got.LsLinks))
	}
}

// TestConsumerAcksAnEnvelopeClickHouseCannotStore pins the writer
// wedge, end to end through a real JetStream. An envelope with Router.Ip =
// "x" decodes fine; before RowsFor checked addresses it became a row whose
// insert failed, the batch was left unacked, and every redelivery failed
// the same way -- taking the good envelope published beside it down too.
//
// The inserter here refuses exactly what ClickHouse's driver refuses (a
// router_ip that does not parse), so the test fails the way production
// did if the bad envelope ever reaches Insert. The good envelope is the
// other side: it must still be inserted and acked.
func TestConsumerAcksAnEnvelopeClickHouseCannotStore(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	bad := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: "x"},
		Payload:     &vantagev1.Envelope_Stats{Stats: &vantagev1.StatsEvent{Counters: map[uint32]uint64{0: 1}}},
	}
	data, err := proto.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := js.Publish(ctx, "vantage.v1.stats.test", data); err != nil {
		t.Fatalf("publish: %v", err)
	}
	publishStatsEnvelopes(t, ctx, js, 1)

	before := testutil.ToFloat64(metricInvalidEnvelopes)
	f := &strictInserter{}
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-invalid-addr",
		BatchRows: 100, BatchWait: 100 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, f)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	defer func() {
		runCancel()
		<-done
	}()

	// Run creates the durable consumer itself, so it may not exist yet on
	// the first look; not-found is retried like a low ack floor.
	deadline := time.Now().Add(10 * time.Second)
	var floor uint64
	for {
		if cons, err := js.Consumer(ctx, "STATS", "test-invalid-addr"); err == nil {
			info, err := cons.Info(ctx)
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			if floor = info.AckFloor.Stream; floor == 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("AckFloor.Stream = %d after 10s, want 2 -- the writer is wedged "+
				"(inserts attempted: %d, refused: %d)", floor, f.attempts(), f.refused())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := testutil.ToFloat64(metricInvalidEnvelopes) - before; got != 1 {
		t.Errorf("vantage_sink_invalid_envelopes_total rose by %v, want 1", got)
	}
	if n := f.refused(); n != 0 {
		t.Errorf("the inserter refused %d batches; the bad envelope reached Insert", n)
	}
	if n := f.statsRows(); n != 1 {
		t.Errorf("inserted %d stats rows, want 1 -- the good envelope was lost", n)
	}
}

// strictInserter refuses a batch the way ClickHouse's driver does when any
// row's router_ip does not parse, and records what it accepted.
type strictInserter struct {
	mu            sync.Mutex
	tries, refuse int
	stats         int
}

func (s *strictInserter) Insert(_ context.Context, r Rows) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries++
	for _, row := range r.Stats {
		if _, err := netip.ParseAddr(row.RouterIP); err != nil {
			s.refuse++
			return fmt.Errorf("clickhouse [Append]: converting %s to IPv6 is unsupported", row.RouterIP)
		}
	}
	s.stats += len(r.Stats)
	return nil
}

func (s *strictInserter) attempts() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.tries }
func (s *strictInserter) refused() int   { s.mu.Lock(); defer s.mu.Unlock(); return s.refuse }
func (s *strictInserter) statsRows() int { s.mu.Lock(); defer s.mu.Unlock(); return s.stats }
