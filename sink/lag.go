package sink

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// lagTracker converts the two sequence numbers JetStream reports about a
// stream and its consumer into a count of envelopes that left the stream
// without this writer ever archiving them.
//
// The streams are LimitsPolicy with DiscardOld and a finite MaxAge/MaxBytes
// (ROUTES is 48h / 8GiB -- see natsutil/streams.go), so retention deletes
// the oldest messages whether or not anything consumed them. A writer that
// is down, wedged, or merely slower than the routers for long enough
// therefore loses envelopes permanently, and nothing in the pipeline reports
// it: the stream looks healthy, the consumer resumes cleanly from whatever
// survived, and ClickHouse simply never receives the rows. The archive of
// record ends up with a hole in it that no count, dashboard or error can
// distinguish from a quiet period on the network.
//
// This type is where that is detected, and it is deliberately pure -- no
// clock, no network, no metrics -- because the arithmetic is the whole risk.
// The sampler around it is a ticker; the counter it feeds is the only
// durable evidence the hole exists, and a wrong count is worse than none
// because it still looks authoritative.
//
// The zero value is ready to use: the first observe seeds the watermark and
// reports nothing.
type lagTracker struct {
	// started distinguishes the first observation from later ones. It is
	// load-bearing rather than bookkeeping: a consumer created against a
	// stream that has already aged data out has lost nothing, because with
	// DeliverAll it begins at the stream's first surviving sequence and
	// everything below that was never its responsibility. Without this,
	// every fresh deployment against a live stream would report a large
	// permanent-loss event on its first sample -- the fastest possible way
	// to teach an operator that this metric is noise.
	started bool

	// accountedThrough is the highest stream sequence whose fate this
	// tracker has already settled, archived or lost. Carrying a watermark
	// rather than a running "lost so far" total is what makes repeated
	// outages correct: a tracker that recomputed the total each sample and
	// reported the difference from what it had already reported would
	// credit the first outage's loss against the second and undercount it
	// by exactly that much.
	accountedThrough uint64
}

// observe takes the stream's oldest surviving sequence and the consumer's
// ack floor, and returns how many envelopes have been lost since the last
// call. It never returns a spurious count: an empty stream, a stream that
// has not moved, and a stream rewound underneath us all report zero.
func (t *lagTracker) observe(firstSeq, ackFloor uint64) uint64 {
	// Everything at or below gone has left the stream. FirstSeq is 0 for an
	// empty stream, where "one below the first survivor" is not a sequence
	// at all -- subtracting anyway would underflow uint64 into a count of
	// eighteen quintillion lost envelopes.
	var gone uint64
	if firstSeq > 0 {
		gone = firstSeq - 1
	}

	if !t.started {
		t.started = true
		t.accountedThrough = max(gone, ackFloor)
		return 0
	}

	// Nothing new has left the stream. This also absorbs a stream that was
	// deleted and recreated under the same name, which sends FirstSeq
	// backwards: no data of ours went missing in that case, and reporting
	// the difference as loss would be a lie in the alarming direction.
	if gone <= t.accountedThrough {
		return 0
	}

	// Of the sequences that left since the last sample, the ones the
	// consumer had already acked were archived; only what lies above the
	// ack floor was lost. The watermark is the other lower bound, so a
	// second sample inside one continuous outage counts only the sequences
	// the first did not.
	lost := uint64(0)
	if floor := max(t.accountedThrough, ackFloor); gone > floor {
		lost = gone - floor
	}
	t.accountedThrough = gone
	return lost
}

// lagSampleInterval is how often each consumer samples its stream and
// consumer state. A var, not a const, so tests can shrink it -- the same
// convention cmd/vantage-collector uses for its own timings. Production
// never assigns to it.
var lagSampleInterval = 30 * time.Second

var (
	// metricEnvelopesLost is the durable evidence. It is a counter, not a
	// gauge, because the condition that produces it heals: once the writer
	// resumes and consumes from the stream's new first sequence, its ack
	// floor climbs back above the trim point and any gauge of "the gap"
	// returns to zero. The hole in ClickHouse does not. A counter keeps the
	// event visible in rate() and in any total for as long as the process
	// lives, hours after the outage that caused it ended.
	metricEnvelopesLost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_sink_envelopes_lost_total",
		Help: "Envelopes discarded by stream retention before this writer acked them; each one is a row missing from ClickHouse for good."},
		[]string{"stream"})

	// metricConsumerLag is NumPending + NumAckPending, not NumPending
	// alone. NumPending counts only messages JetStream has not yet
	// delivered to this consumer, so it drops to zero the moment a batch is
	// fetched and stays there for the whole accumulate-insert-ack cycle --
	// understating lag by exactly the batch in flight, which is the part
	// this writer is slowest at.
	metricConsumerLag = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vantage_sink_consumer_lag",
		Help: "Envelopes in the stream this writer has not yet archived: undelivered plus delivered-but-unacked."},
		[]string{"stream"})

	// metricLagSampleErrors exists because the failure mode of a gauge is
	// staleness, and a stale gauge is indistinguishable from a healthy one.
	// A sampler that cannot reach JetStream leaves consumer_lag frozen at
	// whatever it last read; without this counter, "lag has been flat at 12
	// for an hour" reads as a calm pipeline rather than as a sampler that
	// stopped looking.
	metricLagSampleErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_sink_lag_sample_errors_total",
		Help: "Failed attempts to read stream or consumer state; while these accrue, consumer_lag is stale rather than low."},
		[]string{"stream"})
)

// publishLagSeries creates this stream's three series at zero.
//
// A Prometheus *Vec exports nothing for a label set until something touches
// it, so a counter that only ever increments on failure does not exist while
// the system is healthy -- and "does not exist" is not how a data-loss
// counter should read. `rate(vantage_sink_envelopes_lost_total[1h])` returns
// no data rather than 0, an alerting rule on it never evaluates, and a
// dashboard panel shows "No data", which looks identical to a panel
// reporting that nothing was lost. Publishing the series at startup makes
// the healthy case an explicit zero instead of an absence.
func publishLagSeries(stream string) {
	metricEnvelopesLost.WithLabelValues(stream)
	metricLagSampleErrors.WithLabelValues(stream)
	metricConsumerLag.WithLabelValues(stream)
}

// sampleLag reads the stream and consumer state once and updates the three
// metrics above. It is called on a fixed interval by Run and touches nothing
// on the batching path, so it cannot affect ack-after-durable.
//
// The order of the two reads is deliberate and is the difference between a
// metric an operator believes and one they learn to ignore. They cannot be
// atomic -- they are two round trips to the server -- so one of them is
// always the older snapshot:
//
//   - Stream first, then consumer (what this does): the ack floor is read
//     at or after the moment the first sequence was read, so anything acked
//     in between counts as archived. The bias is toward under-reporting,
//     and only within that gap.
//   - Consumer first, then stream: the trim point is the newer value, so
//     messages acked during the gap are counted as lost.
//
// The second order is far worse than it sounds. On a healthy writer both
// the trim point and the ack floor advance continuously, so it would emit a
// steady trickle of phantom loss on a pipeline that is losing nothing --
// leaving the counter permanently non-zero and therefore meaningless. On a
// genuinely stalled writer there is no race to lose: the ack floor is not
// moving, so this order reports the loss exactly.
func (c *Consumer) sampleLag(ctx context.Context, cons jetstream.Consumer, lt *lagTracker) error {
	stream, err := c.js.Stream(ctx, c.cfg.Stream)
	if err != nil {
		metricLagSampleErrors.WithLabelValues(c.cfg.Stream).Inc()
		return fmt.Errorf("lag: stream %s: %w", c.cfg.Stream, err)
	}
	si, err := stream.Info(ctx)
	if err != nil {
		metricLagSampleErrors.WithLabelValues(c.cfg.Stream).Inc()
		return fmt.Errorf("lag: stream %s info: %w", c.cfg.Stream, err)
	}
	ci, err := cons.Info(ctx)
	if err != nil {
		metricLagSampleErrors.WithLabelValues(c.cfg.Stream).Inc()
		return fmt.Errorf("lag: consumer %s info: %w", c.cfg.Durable, err)
	}

	metricConsumerLag.WithLabelValues(c.cfg.Stream).Set(
		float64(ci.NumPending) + float64(ci.NumAckPending))

	if lost := lt.observe(si.State.FirstSeq, ci.AckFloor.Stream); lost > 0 {
		metricEnvelopesLost.WithLabelValues(c.cfg.Stream).Add(float64(lost))
		// Logged at Error because this is unrecoverable data loss, not a
		// degraded state that will resolve: those envelopes are gone from
		// the stream and were never written to ClickHouse. Nothing
		// downstream can reconstruct them.
		c.log.Error("envelopes lost to stream retention before insert",
			"lost", lost, "stream_first_seq", si.State.FirstSeq,
			"ack_floor", ci.AckFloor.Stream)
	}
	return nil
}
