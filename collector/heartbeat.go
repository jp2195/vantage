package collector

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/types/known/timestamppb"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
)

// BeatInterval is how often a collector says it is alive. The read side's
// default stale threshold, query.DefaultStaleAfter, is three of these, and
// its floor, query.MinStaleAfter, two; query's tests hold both to this.
const BeatInterval = 30 * time.Second

// beatPublishTimeout bounds one beat's wait for JetStream's ack, well under
// BeatInterval so a slow ack never overlaps the next beat. It is what makes
// PublishOnce's own no-deadline contract (see natsutil.Publisher.PublishOnce)
// safe to call here: without it, a stalled or non-answering NATS would hold
// a beat -- and so the beat loop, and so a collector shutdown -- for as long
// as the caller's own ctx allowed, which during a clean shutdown is
// indefinitely.
const beatPublishTimeout = 5 * time.Second

// reconnectRetries and reconnectRetryDelay bound the one retry a beat ever
// gets: a beat Reconnected asked for that fails is tried again up to
// reconnectRetries more times, reconnectRetryDelay after each failure.
//
// A NATS cluster that has lost quorum, or restarted whole, accepts clients
// before its meta and stream leaders are elected, and a beat published then
// fails at once (no responders, or a 503) because PublishOnce never retries.
// Dropping that beat would put the next one at the next tick: the outage
// plus up to BeatInterval, which after a 60 s outage is the read side's 90 s
// stale threshold. Two more tries 5 s apart cover an election of about 10 s
// and still add at most three beats per reconnect.
const (
	reconnectRetries    = 2
	reconnectRetryDelay = 5 * time.Second
)

// BeatPublisher publishes one message with no retry and reports the outcome
// to the caller alone. natsutil.Publisher implements it.
type BeatPublisher interface {
	PublishOnce(ctx context.Context, ev Event) error
}

var (
	metricBeatsPublished = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_beats_published_total",
		Help: "Heartbeats JetStream acknowledged."})
	metricBeatsDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_beats_dropped_total",
		Help: "Heartbeats that failed to publish and were dropped; the next one replaces them."})
)

// Heartbeat publishes this collector's CollectorBeat: once at startup, from
// the caller, every BeatInterval from Run, and once more from Run whenever
// the NATS connection comes back (see Reconnected).
//
// Each beat carries the process start the caller captured before any BMP
// session opened. That is what lets the read side tell a restarted collector's
// dead sessions from its live ones: every session id this process mints is
// at or above it.
type Heartbeat struct {
	collectorID string
	started     time.Time
	pub         BeatPublisher
	now         func() time.Time
	interval    time.Duration
	// timeout bounds one Beat's call to PublishOnce, which imposes no
	// deadline of its own. Defaults to beatPublishTimeout; a test may lower
	// it to prove Beat is in fact bounded by it rather than by the caller's
	// own ctx.
	timeout time.Duration
	// retryDelay is reconnectRetryDelay; a test may lower it.
	retryDelay time.Duration

	// key and n make each beat's msg-id: the collector, this process, and a
	// count of beats attempted. A re-send would reuse one; nothing re-sends.
	key string
	n   uint64

	// reconnected carries Reconnected's request for a beat to Run. Its buffer
	// of one is what coalesces: a request already pending absorbs every
	// later one until Run takes it.
	reconnected chan struct{}
}

// NewHeartbeat builds the heartbeat for collectorID, whose process started at
// started. now supplies the collector clock for each beat's ts_collector.
func NewHeartbeat(collectorID string, started time.Time, pub BeatPublisher, now func() time.Time) *Heartbeat {
	return &Heartbeat{
		collectorID: collectorID, started: started, pub: pub, now: now,
		interval: BeatInterval, timeout: beatPublishTimeout, retryDelay: reconnectRetryDelay,
		key:         hex.EncodeToString([]byte(collectorID)),
		reconnected: make(chan struct{}, 1),
	}
}

// Beat publishes one beat now and returns the publish error, if any. A failed
// beat is counted and dropped: nothing re-sends it, and the next one
// supersedes it.
//
// It gives PublishOnce an explicit deadline (h.timeout, beatPublishTimeout by
// default) derived from ctx: PublishOnce itself imposes none, so without this
// a stalled or non-answering NATS would hold Beat -- and so Run's loop, and
// so a collector shutdown waiting on it -- for as long as ctx allowed.
//
// Beat and Run are not safe for concurrent use with each other. The daemon
// calls Beat once, then starts Run.
func (h *Heartbeat) Beat(ctx context.Context) error {
	h.n++
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	if err := h.pub.PublishOnce(ctx, h.event(h.n)); err != nil {
		metricBeatsDropped.Inc()
		return err
	}
	metricBeatsPublished.Inc()
	return nil
}

// Reconnected asks Run for one beat now instead of at the next tick. The
// daemon calls it from nats.go's reconnect handler.
//
// A scheduled beat is dropped, not retried, and one attempted while the
// client is still reconnecting fails even when the server is already back.
// Without this, the gap between the last beat before an outage and the
// first one after it is the outage plus up to one more interval, so an
// outage near 60 s could reach the read side's 90 s stale threshold. The
// beat this asks for is the one beat that is retried: see
// reconnectRetries.
//
// It never blocks: it runs on nats.go's callback goroutine, which serves
// every other connection callback too. Requests coalesce, so a burst of
// reconnects -- or one landing while Run is already beating -- adds at most
// one beat. Once Run has returned, a request is never taken and nothing
// beats.
func (h *Heartbeat) Reconnected() {
	select {
	case h.reconnected <- struct{}{}:
	default:
	}
}

// Run beats every interval, and once for each coalesced Reconnected, until
// ctx is done. It does not beat on entry: the caller sends the first beat
// itself, before the BMP listener accepts a session, so a restart is visible
// as soon as the new process is.
//
// A reconnect beat that fails is tried again up to reconnectRetries times,
// retryDelay after each failure, and every try goes through Beat and its
// deadline. A scheduled beat that fails is not retried. The retry timer
// belongs to Run and is stopped when it returns, so nothing outlives a
// shutdown.
func (h *Heartbeat) Run(ctx context.Context, log *slog.Logger) {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	retry := time.NewTimer(h.retryDelay)
	retry.Stop()
	defer retry.Stop()
	left := 0 // retries left for the current reconnect beat
	for {
		prompt := false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.reconnected:
			prompt, left = true, reconnectRetries
		case <-retry.C:
			prompt = true
		}
		err := h.Beat(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
		case prompt && left > 0:
			left--
			retry.Reset(h.retryDelay)
			log.Warn("heartbeat after a NATS reconnect not published; retrying",
				"err", err, "in", h.retryDelay, "retries_left", left)
		default:
			log.Warn("heartbeat not published; the next one replaces it", "err", err)
		}
	}
}

func (h *Heartbeat) event(n uint64) Event {
	return Event{
		Subject: subjects.Beat(h.collectorID),
		MsgID:   fmt.Sprintf("beat/%s/%d/%d", h.key, h.started.UnixNano(), n),
		Env: &vantagev1.Envelope{
			CollectorId: h.collectorID,
			TsCollector: timestamppb.New(h.now()),
			Payload: &vantagev1.Envelope_Beat{Beat: &vantagev1.CollectorBeat{
				StartedAt: timestamppb.New(h.started),
			}},
		},
	}
}
