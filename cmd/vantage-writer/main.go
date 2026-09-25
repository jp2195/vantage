// Command vantage-writer is the ClickHouse sink daemon: it consumes typed
// envelopes for the ROUTES, LS, PEER and STATS streams from JetStream and
// inserts them into ClickHouse with ack-after-durable semantics (see
// sink/consumer.go's Run). RAW is deliberately not consumed -- it
// is the system's lossless replay/safety-net window, not the archive, and
// sink's own consumer construction refuses to be pointed at it.
//
// This binary holds no local state: nothing it writes to disk, no
// checkpoint file, no buffer that outlives a single batch. The only durable
// record of how far it has gotten is each stream's JetStream consumer
// position, which is exactly why every ConsumerConfig below sets a stable
// Durable name -- see consumerConfigs' comment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/jp2195/vantage/buildinfo"
	"github.com/jp2195/vantage/health"
	"github.com/jp2195/vantage/logging"
	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/sink"
)

// newMetricsServer builds this daemon's metrics listener with every timeout
// field set. A zero-value http.Server has none -- not on reading headers,
// not on reading a body, not on writing a response, not on an idle
// keep-alive connection -- so a single client that opens a connection and
// then sends a byte a minute holds a goroutine and a file descriptor
// indefinitely. MetricsListen defaults to "0.0.0.0:9472", so that client
// does not have to be local.
//
// Same durations and same reasoning as vantage-collector's newHTTPServer,
// which carries the full rationale: generous rather than tight, with
// writeTimeout covering a whole Prometheus exposition over a slow link and
// idleTimeout exceeding the scrape interval so a scraper reuses its
// connection instead of re-dialing.
//
// Vars, not consts, so main_test.go can assert on them. Production code
// never assigns to them.
var (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

func newMetricsServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func main() {
	cfgPath := flag.String("config", "", "path to YAML config")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Line("vantage-writer"))
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *cfgPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run is main's testable seam: it takes ctx rather than constructing its own
// via signal.NotifyContext, the same shape cmd/vantage-collector/main.go
// uses for the same reason -- a future test can drive the lifecycle
// (start, cancel, assert clean shutdown) without sending real OS signals to
// the test binary's whole process.
func run(ctx context.Context, cfgPath string) error {
	cfg, err := sink.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	slog.SetDefault(logging.New(cfg.LogLevel, cfg.LogFormat, os.Stderr))
	slog.Info("vantage-writer starting", "version", buildinfo.Version,
		"revision", buildinfo.Rev(), "goversion", runtime.Version())

	// Validate parses both connection strings ourselves before either is
	// ever handed to nats.Connect or sink.NewClickHouse below, for a fast,
	// clear failure on an obvious typo rather than a confusing dial/timeout
	// error once the library gets hold of whatever it salvaged from the
	// input. It is not what keeps a credential out of the log if the
	// library rejects the value anyway: cfg.NatsURL and cfg.ClickHouseDSN
	// are secret types whose every rendering is redacted, and the errors
	// natsutil.Connect and sink.NewClickHouse return have already been
	// through redact.Err (see secret's package comment).
	if err := cfg.NatsURL.Validate(); err != nil {
		return fmt.Errorf("nats_url: %w", err)
	}
	if err := cfg.ClickHouseDSN.Validate(); err != nil {
		return fmt.Errorf("clickhouse_dsn: %w", err)
	}

	nc, err := natsutil.Connect(cfg.NatsURL, cfg.NatsTLS, nats.MaxReconnects(-1), nats.Name("vantage-writer"))
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", cfg.NatsURL, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	// NewClickHouse fails fast on a schema-version mismatch (see
	// sink/clickhouse.go): a silent column mismatch is the failure
	// most likely to corrupt data without anyone noticing, so this daemon
	// refuses to start rather than insert into an unexpected shape.
	ch, err := sink.NewClickHouse(ctx, cfg.ClickHouseDSN)
	if err != nil {
		return fmt.Errorf("connect clickhouse %s: %w", cfg.ClickHouseDSN, err)
	}
	defer ch.Close()

	// Ready means both halves of this daemon's job are reachable: NATS to
	// fetch from and ClickHouse to insert into. The ClickHouse half is a
	// round trip, so it is cached; the NATS half reads client state.
	mux, err := health.MetricsMux("writer", health.All(
		health.NATS(nc),
		health.Cached(health.Ping(ch.Conn())),
	))
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	metricsSrv := newMetricsServer(cfg.MetricsListen, mux)
	go func() {
		slog.Info("metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Best-effort, as in vantage-collector: a failure to bind the
			// metrics port must not prevent this daemon from doing its
			// actual job of consuming and inserting.
			slog.Error("metrics server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	// Every consumer is constructed (and so every ConsumerConfig validated
	// by sink.NewConsumer's guards) before any of them is started, so a
	// construction failure on, say, the fourth stream cannot leave the
	// first three already running as unsupervised goroutines with no
	// errgroup wired up to wait on or cancel them.
	configs := consumerConfigs(cfg)
	consumers := make([]*sink.Consumer, len(configs))
	for i, cc := range configs {
		c, err := sink.NewConsumer(js, cc, ch)
		if err != nil {
			return fmt.Errorf("new consumer %s/%s: %w", cc.Stream, cc.Durable, err)
		}
		consumers[i] = c
	}

	g, gctx := errgroup.WithContext(ctx)
	if cfg.CurrentCleanupInterval > 0 {
		// The hostname names this writer in the cleanup lease: a pod name
		// under Kubernetes, so replicas differ.
		holder, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("hostname for the cleanup lease: %w", err)
		}
		slog.Info("current cleanup starting", "interval", cfg.CurrentCleanupInterval, "holder", holder)
		// In the group, so ClickHouse is not closed under a running cycle.
		// It returns only when gctx is done, and never fails the writer: a
		// failed cycle is logged, counted and retried the next interval.
		g.Go(func() error {
			sink.RunCleanup(gctx, ch, cfg.CurrentCleanupInterval, holder, slog.Default())
			return nil
		})
	}
	for i, cc := range configs {
		c := consumers[i]
		g.Go(func() error {
			slog.Info("consumer starting", "stream", cc.Stream, "durable", cc.Durable)
			// Run pulls, batches, inserts and acks until gctx is canceled,
			// and acks only after Insert returns nil (ack-after-durable).
			// errgroup.WithContext means one consumer returning a non-nil
			// error cancels gctx for the other three as well, so a fatal
			// error in one stream's consumer does not leave the others
			// running unsupervised; a clean shutdown (ctx canceled by a
			// signal) makes every consumer return the same ctx.Err(), which
			// errgroup collapses to a single reported error, not four.
			err := c.Run(gctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("consumer %s/%s: %w", cc.Stream, cc.Durable, err)
			}
			return nil
		})
	}
	return g.Wait()
}

// consumerConfigs builds the four ConsumerConfig values this daemon runs --
// ROUTES, LS, PEER and STATS. RAW is deliberately absent: it is the
// system's replay window, not the archive, and sink.NewConsumer's stream
// allowlist would refuse it anyway if it were added here by mistake.
//
// Durable is set to a stable, per-stream name rather than left empty.
// sink.NewConsumer rejects an empty Durable for exactly this reason: an
// empty value makes JetStream generate an ephemeral consumer, which the
// server can reap after a period of inactivity, and on reap the consumer's
// position -- this writer's only durable state, since it keeps nothing on
// disk -- is lost with it. A restart of this binary must resume each
// stream from where it left off, which requires the same durable name every
// time, not a fresh one per process lifetime.
//
// AckWait and MaxAckPending are derived from cfg.BatchRows/cfg.BatchWait
// rather than independently configurable, because they exist to make this
// consumer's batching shape (see sink/consumer.go's Run: every
// message in a batch stays unacked from Fetch until the whole batch's
// Insert returns) safe against JetStream's own defaults, not to be tuned on
// their own:
//
//   - AckWait must comfortably exceed BatchWait (the time spent
//     accumulating a batch) plus however long the subsequent ClickHouse
//     Insert takes, or JetStream redelivers mid-insert -- safe, because
//     ReplacingMergeTree collapses the duplicate on stream_seq, but
//     wasteful, and a livelock risk if Insert is consistently slower than
//     AckWait. 10x BatchWait covers the accumulation phase with room to
//     spare for the insert itself; the 60s floor keeps a short BatchWait
//     (a low-latency deployment might set it well under a second) from
//     producing an AckWait so small that an ordinary ClickHouse hiccup
//     triggers redelivery.
//   - MaxAckPending must exceed how many messages can be pending in one
//     batch window, or JetStream's default of 1000 silently caps the
//     configured BatchRows at 1000 messages regardless of what the
//     operator asked for. In the common case each message yields roughly
//     one row (peer/LS/stats events always do; a route update can yield
//     more for a multi-prefix announce or fewer for an end-of-RIB marker),
//     so pending message count tracks BatchRows, but the inner fetch loop
//     in Run can overshoot by up to one FetchBatch before it next checks
//     the row threshold. 4x(BatchRows+FetchBatch) leaves generous headroom
//     for that overshoot and for runs of low-row-yield messages, while
//     staying a concrete, finite number rather than reaching for
//     JetStream's -1 (unlimited), which would remove its flow control
//     entirely.
func consumerConfigs(cfg sink.Config) []sink.ConsumerConfig {
	ackWait := max(10*cfg.BatchWait, 60*time.Second)
	maxAckPending := 4 * (cfg.BatchRows + cfg.FetchBatch)

	base := sink.ConsumerConfig{
		BatchRows:     cfg.BatchRows,
		BatchWait:     cfg.BatchWait,
		FetchBatch:    cfg.FetchBatch,
		AckWait:       ackWait,
		MaxAckPending: maxAckPending,
	}
	streams := []struct{ stream, durable string }{
		{"ROUTES", "vantage-writer-routes"},
		{"LS", "vantage-writer-ls"},
		{"PEER", "vantage-writer-peer"},
		{"STATS", "vantage-writer-stats"},
	}
	cfgs := make([]sink.ConsumerConfig, len(streams))
	for i, s := range streams {
		cc := base
		cc.Stream = s.stream
		cc.Durable = s.durable
		cfgs[i] = cc
	}
	return cfgs
}
