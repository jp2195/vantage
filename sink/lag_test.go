package sink

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/natsutil/natstest"
)

// The lag tracker turns two numbers JetStream reports -- the stream's oldest
// surviving sequence and the consumer's ack floor -- into "how many envelopes
// left the stream without this writer ever archiving them".
//
// That arithmetic is the whole risk in the lag feature. The goroutine around
// it is a ticker; the counter it feeds is the only durable evidence that the
// archive has a hole in it, and a wrong count is worse than no count because
// it looks authoritative. So it lives in a pure type with no clock, no
// network and no metrics, tested exhaustively here, and the sampler does
// nothing but call it.
func TestLagTrackerObserve(t *testing.T) {
	// Each step is one sample: what JetStream reported, and how many newly
	// lost envelopes the tracker must attribute to it.
	type step struct {
		name     string
		firstSeq uint64 // stream's oldest surviving sequence
		ackFloor uint64 // consumer's ack floor (last contiguous acked seq)
		wantLost uint64
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			// A consumer created against a stream that has already aged
			// data out has not lost anything: with DeliverAll it starts at
			// FirstSeq, so sequences below that were never its
			// responsibility. Counting them would fire a permanent-data-loss
			// alarm on every fresh deployment against a live stream, which
			// is the fastest way to teach an operator to ignore it.
			name: "fresh consumer against an already-trimmed stream loses nothing",
			steps: []step{
				{"first sample", 51, 0, 0},
				{"steady", 51, 60, 0},
			},
		},
		{
			name: "keeping up loses nothing as the stream trims behind the ack floor",
			steps: []step{
				{"first sample", 1, 0, 0},
				{"acked ahead of the trim", 101, 150, 0},
				{"trim advances, still behind the ack floor", 201, 260, 0},
			},
		},
		{
			// The writer stalls; retention trims past where it had acked.
			// Sequences 11..50 left the stream unarchived.
			name: "trim passing the ack floor loses the gap",
			steps: []step{
				{"first sample", 1, 10, 0},
				{"trimmed past the ack floor", 51, 10, 40},
			},
		},
		{
			// The case a naive "report the delta since last time" gets
			// wrong. After recovering from a 40-envelope hole, a second
			// stall loses 399 more. Anything that tracks a running
			// "lost so far" and subtracts what it already reported credits
			// the first event's 40 against the second and reports 359.
			name: "a second loss after recovery is counted in full",
			steps: []step{
				{"first sample", 1, 10, 0},
				{"first loss", 51, 10, 40},
				{"recovered, caught up well past the trim", 201, 500, 0},
				{"second loss", 900, 500, 399},
			},
		},
		{
			// Two samples inside one continuous outage. The second must
			// count only what the first did not.
			name: "an ongoing outage is not double counted across samples",
			steps: []step{
				{"first sample", 1, 10, 0},
				{"outage, first sample", 51, 10, 40},
				{"outage, second sample", 91, 10, 40},
				{"outage, no further trim", 91, 10, 0},
			},
		},
		{
			// An empty stream reports FirstSeq 0. Subtracting 1 from it
			// underflows uint64 to a count of 18 quintillion lost
			// envelopes, which is the kind of number that gets a metric
			// permanently disbelieved.
			name: "an empty stream reports no loss rather than underflowing",
			steps: []step{
				{"first sample, empty stream", 0, 0, 0},
				{"still empty", 0, 0, 0},
				{"first message arrives", 1, 0, 0},
			},
		},
		{
			// FirstSeq going backwards should not happen, but a stream
			// deleted and recreated under the same name would do it. Never
			// report negative-turned-enormous loss.
			name: "a stream rewound under us reports no loss",
			steps: []step{
				{"first sample", 500, 600, 0},
				{"rewound", 1, 0, 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lt lagTracker
			for i, s := range tt.steps {
				got := lt.observe(s.firstSeq, s.ackFloor)
				if got != s.wantLost {
					t.Fatalf("step %d (%s): observe(firstSeq=%d, ackFloor=%d) = %d lost, want %d",
						i, s.name, s.firstSeq, s.ackFloor, got, s.wantLost)
				}
			}
		})
	}
}

// TestSampleLagCountsEnvelopesTrimmedBeforeAck is the live half: the pure
// tracker above proves the arithmetic, this proves the sampler reads the
// two numbers JetStream actually reports and feeds them in the right order.
//
// It reproduces the real failure rather than simulating it. A stream with a
// small MaxMsgs stands in for ROUTES' 48h/8GiB retention, a consumer that
// acks nothing stands in for a writer that is down or wedged, and publishing
// past the limit is what a fleet of routers does regardless. The sequences
// that fall off the back are gone from the system: no error is raised
// anywhere, the consumer resumes cleanly from what survived, and without
// this counter nothing would ever say the archive is missing them.
func TestSampleLagCountsEnvelopesTrimmedBeforeAck(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A deliberately tiny stream: five messages, oldest discarded. Created
	// directly rather than via EnsureStreams because the point is the trim,
	// and the production STATS stream is sized never to trim in a test.
	const keep = 5
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "STATS",
		Subjects:  []string{"vantage.v1.stats.>"},
		MaxMsgs:   keep,
		Discard:   jetstream.DiscardOld,
		Retention: jetstream.LimitsPolicy,
		Replicas:  1,
	}); err != nil {
		t.Fatalf("CreateOrUpdateStream: %v", err)
	}

	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-lag",
		BatchRows: 100, BatchWait: 100 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, &fakeInserter{})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	// The consumer exists and acks nothing: a writer that is up but not
	// making progress, which is the shape that loses data silently.
	cons, err := js.CreateOrUpdateConsumer(ctx, "STATS", jetstream.ConsumerConfig{
		Durable: "test-lag", AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("CreateOrUpdateConsumer: %v", err)
	}

	publishStatsEnvelopes(t, ctx, js, keep)

	var lt lagTracker
	lostBefore := testutil.ToFloat64(metricEnvelopesLost.WithLabelValues("STATS"))

	// First sample, nothing trimmed yet: this is what establishes that the
	// sequences now in the stream are this consumer's responsibility.
	if err := c.sampleLag(ctx, cons, &lt); err != nil {
		t.Fatalf("sampleLag (before trim): %v", err)
	}
	if got := testutil.ToFloat64(metricEnvelopesLost.WithLabelValues("STATS")) - lostBefore; got != 0 {
		t.Fatalf("envelopes_lost rose by %v before anything was trimmed", got)
	}
	if got := testutil.ToFloat64(metricConsumerLag.WithLabelValues("STATS")); got != keep {
		t.Fatalf("consumer_lag = %v, want %d: every published message is unacked", got, keep)
	}

	// Push the first `lost` sequences off the back of the stream. Nothing
	// acked them, so they are gone from the archive for good.
	const lost = 10
	publishStatsEnvelopes(t, ctx, js, lost)

	if err := c.sampleLag(ctx, cons, &lt); err != nil {
		t.Fatalf("sampleLag (after trim): %v", err)
	}
	if got := testutil.ToFloat64(metricEnvelopesLost.WithLabelValues("STATS")) - lostBefore; got != lost {
		t.Fatalf("envelopes_lost rose by %v, want %d -- %d messages were "+
			"discarded by retention with an ack floor of 0, so every one of "+
			"them left the stream unarchived", got, lost, lost)
	}
}

// TestRunSamplesLag proves the sampler is actually wired into Run and keeps
// reporting while the insert path is failing. That combination is the point:
// a writer whose ClickHouse is unreachable backs off up to 30s between
// attempts and acks nothing, so it is simultaneously the most likely
// candidate to be losing envelopes to retention and the least able to
// notice. A sampler that only ticked alongside successful batches would go
// quiet exactly when its numbers matter.
func TestRunSamplesLag(t *testing.T) {
	origInterval := lagSampleInterval
	lagSampleInterval = 50 * time.Millisecond
	defer func() { lagSampleInterval = origInterval }()

	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	const n = 7
	publishStatsEnvelopes(t, ctx, js, n)

	// Every insert fails, so nothing is ever acked and the lag must settle
	// at exactly the number published and stay there.
	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "STATS", Durable: "test-run-lag",
		BatchRows: 100, BatchWait: 50 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, &fakeInserter{err: errors.New("clickhouse unreachable")})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	errsBefore := testutil.ToFloat64(metricLagSampleErrors.WithLabelValues("STATS"))
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if got := testutil.ToFloat64(metricConsumerLag.WithLabelValues("STATS")); got == n {
			break
		} else if time.Now().After(deadline) {
			runCancel()
			<-done
			t.Fatalf("consumer_lag never reached %d (last %v): Run is not sampling", n, got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := testutil.ToFloat64(metricLagSampleErrors.WithLabelValues("STATS")) - errsBefore; got != 0 {
		t.Fatalf("lag_sample_errors rose by %v against a healthy NATS", got)
	}

	// The sampler is Run's goroutine, so Run must WAIT for it, not merely
	// signal it. The write below is the assertion: under -race it fails if
	// any sampler is still reading lagSampleInterval after Run returned,
	// which is precisely the lifetime bug where a sampler outlives Run and
	// keeps calling Info() against a connection main is closing. It is
	// deliberately not a defer -- it has to run at this exact point.
	runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	lagSampleInterval = origInterval
}

// seriesValue returns the value of the named metric carrying stream=label,
// and whether that series exists at all. It gathers from the default
// registry rather than calling WithLabelValues, because WithLabelValues
// CREATES the series it looks up -- using it here would manufacture the very
// thing under test.
func seriesValue(t *testing.T, name, label string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() != "stream" || lp.GetValue() != label {
					continue
				}
				if c := m.GetCounter(); c != nil {
					return c.GetValue(), true
				}
				if g := m.GetGauge(); g != nil {
					return g.GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// A Prometheus *Vec exports nothing for a label set until something touches
// it, so a counter that only ever increments on failure does not exist while
// the system is healthy. That is the wrong default for this particular
// counter: `rate(vantage_sink_envelopes_lost_total[1h])` returns no data
// rather than 0, an alert on it never evaluates, and a dashboard panel reads
// "No data" -- which looks the same as a panel reporting no loss. The series
// must exist at zero from startup so that zero is an assertion rather than
// an absence.
//
// LS is used here because no other test in this package touches that label,
// so its absence before Run is genuinely this code's doing.
func TestRunPublishesLagSeriesBeforeAnyLoss(t *testing.T) {
	origInterval := lagSampleInterval
	lagSampleInterval = time.Hour // never fires; startup alone must publish
	defer func() { lagSampleInterval = origInterval }()

	if _, ok := seriesValue(t, "vantage_sink_envelopes_lost_total", "LS"); ok {
		t.Fatal("envelopes_lost already carries stream=LS before Run: " +
			"another test contaminated the label, so this one proves nothing")
	}

	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := natsutil.EnsureStreams(ctx, js, testStreamOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	c, err := NewConsumer(js, ConsumerConfig{
		Stream: "LS", Durable: "test-ls-series",
		BatchRows: 100, BatchWait: 50 * time.Millisecond, FetchBatch: 10,
		AckWait: 30 * time.Second, MaxAckPending: 1000,
	}, &fakeInserter{})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	defer func() { runCancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for _, name := range []string{
		"vantage_sink_envelopes_lost_total",
		"vantage_sink_lag_sample_errors_total",
		"vantage_sink_consumer_lag",
	} {
		for {
			v, ok := seriesValue(t, name, "LS")
			if ok {
				if v != 0 {
					t.Fatalf("%s{stream=LS} = %v at startup, want 0", name, v)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never published a stream=LS series: it will read "+
					"\"no data\" rather than zero until the first failure", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
