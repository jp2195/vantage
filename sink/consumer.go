package sink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/proto"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// Insert failures back off exponentially between minInsertBackoff and
// maxInsertBackoff, so an unreachable ClickHouse does not become a tight
// fetch-and-fail loop.
const (
	minInsertBackoff = 250 * time.Millisecond
	maxInsertBackoff = 30 * time.Second
)

var (
	metricDecodeErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_decode_errors_total",
		Help: "Envelopes that failed to protobuf-unmarshal and were acked without insertion."})
	metricInvalidEnvelopes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_invalid_envelopes_total",
		Help: "Envelopes that decoded but carried a value ClickHouse cannot store, such as an unparseable address, and were acked without insertion."})
	metricInsertErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_insert_errors_total",
		Help: "ClickHouse insert attempts that failed; the batch was left unacked for redelivery."})
	metricRowsInserted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_rows_inserted_total",
		Help: "Rows durably inserted into ClickHouse across all tables, acked after."})
	// metricFetchErrors counts JetStream pull failures that are not the two
	// expected, benign ways a Fetch ends without messages (this batch
	// window's own deadline elapsing, or ctx being canceled for shutdown) --
	// e.g. the durable consumer having been deleted server-side, a
	// leadership change, or no responders. Without counting these
	// separately from insert errors, an unrecoverable fetch failure (the
	// consumer is gone; CreateOrUpdateConsumer only runs once, at the top
	// of Run) would spin against the API subject with no signal anywhere
	// that anything was wrong.
	metricFetchErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_fetch_errors_total",
		Help: "JetStream Fetch calls that failed for a reason other than the batch window's own deadline or shutdown."})
)

// allowedStreams is the fixed set of streams this consumer may be pointed
// at. RAW is deliberately absent: RowsFor has no case for Envelope_Raw, so
// an envelope from RAW decodes cleanly, translates to zero rows in every
// table, inserts as a no-op (Insert returns nil on all-empty Rows), and gets
// acked -- silently draining RAW, the system's lossless replay/safety-net
// stream, with no error anywhere in that chain to catch it. Misrouting a
// consumer at RAW is a startup-time mistake, not a runtime one, so it is
// rejected in NewConsumer rather than discovered later as missing data.
var allowedStreams = map[string]bool{
	"ROUTES": true,
	"LS":     true,
	"PEER":   true,
	"STATS":  true,
}

// Inserter is the subset of *ClickHouse the consumer needs, so tests can
// substitute a fake without a database.
type Inserter interface {
	Insert(context.Context, Rows) error
}

type ConsumerConfig struct {
	Stream     string        // JetStream stream, e.g. "ROUTES"
	Durable    string        // durable consumer name
	BatchRows  int           // flush when this many rows have accumulated
	BatchWait  time.Duration // flush when this long has elapsed, even if smaller
	FetchBatch int           // messages to pull per fetch

	// AckWait and MaxAckPending are passed straight through to the
	// JetStream consumer (see Run's CreateOrUpdateConsumer call). Both are
	// required -- NewConsumer rejects a non-positive value for either,
	// below -- rather than left at zero to fall through to JetStream's own
	// defaults (30s and 1000 respectively), because "unset" is exactly how
	// this consumer's two known misconfigurations happened: a value that
	// looks like "no opinion" is actually "silently accept whatever the server
	// defaults to," which for both of these fields is wrong against this
	// consumer's batching shape.
	//
	// AckWait bounds how long a message may sit delivered-but-unacked
	// before JetStream redelivers it. This consumer holds every message in
	// a batch unacked from the moment it is fetched until Insert returns
	// (see Run), so the wall-clock span AckWait must cover is BatchWait
	// (the time spent accumulating the batch) plus however long Insert
	// takes to write it -- not just the insert. Redelivery mid-insert is
	// safe (ReplacingMergeTree collapses the duplicate on stream_seq at
	// merge time) but wasteful: it inflates the next batch with rows
	// already in flight and, if Insert is consistently slower than
	// AckWait, can livelock the consumer into perpetual redelivery instead
	// of ever making forward progress.
	AckWait time.Duration
	// MaxAckPending bounds how many delivered-but-unacked messages the
	// server allows outstanding at once. This consumer holds every message
	// in a batch unacked until the whole batch's Insert succeeds (see Run),
	// so it must be set comfortably above BatchRows: JetStream's own
	// default of 1000 would otherwise cap the server's redelivery window at
	// 1000 messages regardless of what BatchRows says, silently truncating
	// every batch configured larger than that to whatever fits under 1000
	// pending messages -- the exact "batch size becomes a lie" failure this
	// field exists to close.
	MaxAckPending int
}

type Consumer struct {
	js  jetstream.JetStream
	cfg ConsumerConfig
	ins Inserter
	log *slog.Logger
}

func NewConsumer(js jetstream.JetStream, cfg ConsumerConfig, ins Inserter) (*Consumer, error) {
	if !allowedStreams[cfg.Stream] {
		return nil, fmt.Errorf("sink: consumer stream %q is not one of ROUTES, LS, PEER, "+
			"STATS (RAW is never consumed by the sink -- it has no RowsFor case, and would "+
			"silently ack-and-drain instead of erroring)", cfg.Stream)
	}
	// An empty Durable makes JetStream generate an ephemeral consumer
	// instead of a durable one. An ephemeral consumer is reaped by the
	// server after a period of inactivity, and when it is reaped its
	// position goes with it -- and that position is this writer's only
	// durable state, since this package holds no local state of its own. A
	// blank Durable is therefore not a reasonable default to fall through
	// on; it is a configuration bug that would only be discovered later,
	// as unexplained data loss after a quiet period, so it is rejected
	// here instead.
	if cfg.Durable == "" {
		return nil, fmt.Errorf("sink: consumer Durable must be set -- an empty value " +
			"makes JetStream create an ephemeral consumer, which the server can reap " +
			"after inactivity, losing the consumer's position (this writer's only " +
			"durable state) with it")
	}
	// BatchWait and BatchRows both gate the same inner accumulation loop
	// ("for rows.Len() < c.cfg.BatchRows && time.Now().Before(deadline)"),
	// and rows.Len() starts at 0 every outer iteration: a non-positive value
	// for either makes that condition false on its very first check, so the
	// loop never calls Fetch even once, fetchErr never becomes true, the
	// backoff guarding the empty-pending path is gated behind fetchErr
	// alone and so never fires, and the outer loop spins at full rate with
	// no sleep anywhere in that path.
	if cfg.BatchWait <= 0 {
		return nil, fmt.Errorf("sink: consumer BatchWait must be positive, got %v -- a "+
			"non-positive value leaves the batch-accumulation deadline already in the "+
			"past on entry, so the inner loop never runs and the outer loop spins with "+
			"no backoff", cfg.BatchWait)
	}
	if cfg.BatchRows <= 0 {
		return nil, fmt.Errorf("sink: consumer BatchRows must be positive, got %d -- "+
			"rows.Len() starts at 0, so a non-positive bound makes the inner loop's "+
			"\"rows.Len() < BatchRows\" false immediately and the outer loop spins with "+
			"no backoff, exactly as a non-positive BatchWait does", cfg.BatchRows)
	}
	// FetchBatch does not spin -- JetStream's own pull-consumer Fetch
	// rejects any batch size under 1 synchronously ("batch size must be at
	// least 1"), which already lands in Run's logged-and-backed-off
	// synchronous-fetch-error branch. But a consumer that can never
	// complete a single successful Fetch is permanently non-functional
	// (a no-op that merely looks alive, cycling errors and backoff
	// forever), which is exactly the class of "runs but never does
	// anything" misconfiguration this function exists to catch at
	// construction rather than leave to be noticed later in a log stream.
	if cfg.FetchBatch <= 0 {
		return nil, fmt.Errorf("sink: consumer FetchBatch must be positive, got %d -- "+
			"JetStream rejects any Fetch batch size under 1 outright, so a non-positive "+
			"value makes every Fetch call fail forever: the consumer backs off "+
			"correctly rather than spinning, but never fetches a single message",
			cfg.FetchBatch)
	}
	// A non-positive AckWait or MaxAckPending is not "no opinion" -- it
	// falls through to JetStream's own defaults (30s and 1000), and both
	// defaults are wrong against this consumer's hold-the-whole-batch-
	// unacked-until-Insert-returns shape (see the two fields' doc comments
	// above). Requiring an explicit positive value here closes both of
	// those misconfigurations at the same construction-time seam the
	// guards above already use, rather than leaving it to whatever the
	// caller happened to set.
	if cfg.AckWait <= 0 {
		return nil, fmt.Errorf("sink: consumer AckWait must be positive, got %v -- left "+
			"at zero it falls through to JetStream's 30s default, which this consumer's "+
			"batching (BatchWait to accumulate, then a synchronous Insert, both before "+
			"any ack) can easily outrun under real ClickHouse latency, causing mid-"+
			"insert redelivery", cfg.AckWait)
	}
	if cfg.MaxAckPending <= 0 {
		return nil, fmt.Errorf("sink: consumer MaxAckPending must be positive, got %d -- "+
			"left at zero it falls through to JetStream's default of 1000, which "+
			"silently caps any BatchRows configured larger than that at the message-"+
			"count level, making the configured batch size a lie", cfg.MaxAckPending)
	}
	return &Consumer{js: js, cfg: cfg, ins: ins, log: slog.Default().With(
		"stream", cfg.Stream, "durable", cfg.Durable)}, nil
}

// Run pulls, batches, inserts and acks until ctx is canceled.
//
// The ordering here is the whole contract: messages are acked only after
// Insert returns nil. On any insert error nothing is acked, the batch is
// dropped from memory, and JetStream redelivers it -- which is safe because
// every row carries stream_seq and ReplacingMergeTree collapses the duplicate
// at merge time.
func (c *Consumer) Run(ctx context.Context) error {
	cons, err := c.js.CreateOrUpdateConsumer(ctx, c.cfg.Stream,
		jetstream.ConsumerConfig{
			Durable:       c.cfg.Durable,
			AckPolicy:     jetstream.AckExplicitPolicy,
			AckWait:       c.cfg.AckWait,
			MaxAckPending: c.cfg.MaxAckPending,
		})
	if err != nil {
		return err
	}

	// The lag sampler runs on its own ticker rather than inside the batch
	// loop below, because the two need different cadences under exactly the
	// conditions that matter. An unreachable ClickHouse pushes this loop to
	// a 30s backoff between attempts and acks nothing at all, which is both
	// the state most likely to be losing envelopes to stream retention and
	// the state in which a piggybacked sample would report least often. It
	// stops when ctx does, and it never touches rows, pending or the ack
	// path, so it cannot affect ack-after-durable.
	publishLagSeries(c.cfg.Stream)
	// Run waits for the sampler before returning. Stopping on ctx is not
	// enough on its own: the caller treats Run's return as "this consumer is
	// finished", and cmd/vantage-writer's errgroup does exactly that before
	// main's deferred nc.Close() runs -- so an unwaited sampler can still be
	// inside an Info() round trip against a connection being torn down. The
	// race detector caught this against a test mutating lagSampleInterval
	// after Run returned, which is the same lifetime bug wearing a smaller
	// hat.
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		var lt lagTracker
		t := time.NewTicker(lagSampleInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.sampleLag(ctx, cons, &lt); err != nil && ctx.Err() == nil {
					// Counted by sampleLag; logged at Warn because a failed
					// sample degrades observability, not the pipeline.
					c.log.Warn("lag sample failed", "err", err)
				}
			}
		}
	})

	backoff := minInsertBackoff
	for ctx.Err() == nil {
		var (
			rows     Rows
			pending  []jetstream.Msg
			deadline = time.Now().Add(c.cfg.BatchWait)
			fetchErr bool
		)
		// Each Fetch is bounded by FetchMaxWait, which the server enforces:
		// it ends the pull request at the window's deadline and says so on
		// the same subscription the messages arrive on, so every message it
		// delivered is read before the Fetch ends. nats.go keeps listening
		// for a second past that before giving up on its own.
		//
		// A Fetch must not be bounded by a context that expires with the
		// window. nats.go ends a Fetch the moment its context expires and
		// discards whatever the server has delivered but Run has not yet
		// read. A server busy enough to answer the window's last Fetch late
		// delivers into that gap, a whole FetchBatch at a time, and the
		// dropped messages sit unacked until AckWait (at least 60 s in
		// cmd/vantage-writer) makes the server redeliver them.
		//
		// Shutdown is still immediate: the read below selects on ctx, and a
		// message dropped then is redelivered like any other unacked one.
		var stopped bool
		for !stopped && rows.Len() < c.cfg.BatchRows {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			batch, err := cons.Fetch(c.cfg.FetchBatch, jetstream.FetchMaxWait(remaining))
			if err != nil {
				c.log.Error("fetch failed", "err", err)
				metricFetchErrors.Inc()
				fetchErr = true
				break
			}
			msgs := batch.Messages()
		read:
			for {
				var msg jetstream.Msg
				select {
				case <-ctx.Done():
					stopped = true
					break read
				case m, ok := <-msgs:
					if !ok {
						break read
					}
					msg = m
				}
				var env vantagev1.Envelope
				if err := proto.Unmarshal(msg.Data(), &env); err != nil {
					// A poison message must not wedge the consumer: count it,
					// ack it, move on. It is preserved in JetStream's own
					// retention if it needs looking at.
					c.log.Error("undecodable envelope, acking", "err", err)
					metricDecodeErrors.Inc()
					_ = msg.Ack()
					continue
				}
				meta, err := msg.Metadata()
				if err != nil {
					_ = msg.Nak()
					continue
				}
				envRows, err := RowsFor(&env, meta.Sequence.Stream)
				if err != nil {
					// Decoded, but carrying a value ClickHouse would refuse.
					// It is treated as the undecodable envelope above is, and
					// for the same reason: left in the batch, its insert
					// error would leave every envelope beside it unacked and
					// redelivered into the same failure indefinitely.
					c.log.Warn("envelope ClickHouse cannot store, acking without insert",
						"stream_seq", meta.Sequence.Stream, "err", err)
					metricInvalidEnvelopes.Inc()
					_ = msg.Ack()
					continue
				}
				rows.Add(envRows)
				pending = append(pending, msg)
			}
			if stopped {
				break
			}
			// batch.Error() reports how this Fetch ended once Messages() has
			// closed. A pull request the server ended at its expiry, the
			// ordinary end of an idle window, reports nil. Anything else
			// (consumer deleted server-side, leadership changed, no
			// responders, missed heartbeats) is a real fetch failure and
			// must not be silently swallowed.
			if berr := batch.Error(); berr != nil {
				c.log.Error("fetch batch ended with error", "err", berr)
				metricFetchErrors.Inc()
				fetchErr = true
				break
			}
		}
		if len(pending) == 0 {
			if fetchErr {
				// A real fetch failure produced nothing to insert or ack:
				// back off exactly as an insert failure does below, or an
				// unrecoverable fetch error (the consumer was deleted
				// server-side, for instance -- CreateOrUpdateConsumer above
				// runs only once, at the top of Run, so it never notices)
				// spins at full rate against the API subject forever.
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
				}
				if backoff < maxInsertBackoff {
					backoff *= 2
				}
			}
			continue
		}
		if err := c.ins.Insert(ctx, rows); err != nil {
			// Deliberately no ack and no Nak: letting the ack wait expire
			// redelivers without hammering ClickHouse in a tight loop.
			c.log.Error("insert failed, not acking", "rows", rows.Len(),
				"err", err, "backoff", backoff)
			metricInsertErrors.Inc()
			// Back off before pulling again, or a persistently unreachable
			// ClickHouse turns into a tight fetch/fail loop that buries the
			// real error in log noise. Reset on the next success.
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			if backoff < maxInsertBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = minInsertBackoff
		for _, m := range pending {
			if err := m.Ack(); err != nil {
				c.log.Warn("ack failed after durable insert", "err", err)
			}
		}
		metricRowsInserted.Add(float64(rows.Len()))
	}
	return errors.Join(ctx.Err())
}
