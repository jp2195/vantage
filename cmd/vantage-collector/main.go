// Command vantage-collector is the BMP collector daemon: it accepts BMP TCP
// connections from routers, drives a collector.Session per connection, and
// publishes the resulting envelopes to JetStream.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/buildinfo"
	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/health"
	"github.com/jp2195/vantage/logging"
	"github.com/jp2195/vantage/natsutil"
)

// publishAsyncTimeout bounds how long a single PublishAsync future is allowed
// to sit unresolved before JetStream's client gives up on it and resolves it
// with an error. jetstream.New defaults this to zero (no timeout at all): a
// future that is never acked and never times out keeps natsutil.Publisher's
// inflight counter above zero forever, which makes every subsequent Drain --
// including the one this daemon's shutdown path depends on to report
// publish failures -- time out for the rest of the process's life instead of
// ever reporting the true error. This value must be passed to jetstream.New
// (see run below); it is not a default the jetstream package applies on its
// own.
//
// It is also how long a publish a stream leader dropped takes to be noticed:
// a leader stepping down (as it does when its server enters lame-duck mode)
// discards proposals in flight without replying, and the timeout is the only
// thing that resolves them, after which natsutil re-sends them with the
// same msg-id. natsutil gives up on a publish once 60s have passed since it
// was first sent, so the timeout decides how many tries fit: at 30s, one
// dropped publish got a single re-send, and a second drop failed it. 10s
// allows five. It is still far above a healthy ack: measured against an
// embedded three-server cluster with an R3 stream, publishing about 33k
// messages/s with up to 4096 in flight through leader changes, acks took p50
// 0.3ms and at most 258ms, the tail being nats.go's own 250ms no-responders
// retry during a leader change (p99 4-6ms without one). A timeout this short that fired on a slow-but-alive ack would only
// cause a re-send the duplicate window turns into a no-op.
//
// A var, not a const: cmd/vantage-collector's own test (main_test.go) needs
// to shrink this (and drainTimeout below) to keep a deliberately-provoked
// stuck-publish scenario fast, rather than waiting out the real 10s/10s
// production values. Production code never assigns to either.
var publishAsyncTimeout = 10 * time.Second

// drainTimeout bounds how long shutdown waits for outstanding publishes to
// resolve after the accept loop and every in-flight session have stopped.
var drainTimeout = 10 * time.Second

// newHTTPServer builds one of this daemon's two HTTP listeners with every
// timeout field set. An http.Server left at its zero value has no timeouts
// at all -- not on reading headers, not on reading a body, not on writing a
// response, not on an idle keep-alive connection -- so one client that opens
// a connection and then sends a byte a minute holds a goroutine and a file
// descriptor for as long as it likes. Repeat that and the daemon runs out of
// both while looking perfectly healthy.
//
// This matters more here than the "it's only metrics" framing suggests.
// MetricsListen defaults to ":9469", every interface, so the metrics
// listener takes connections from anything that can route to the host. The
// admin listener defaults to loopback (see config.go), but its handler is
// the one with a body to read: admin.go already bounds how many BYTES a
// request body may carry (maxArmBodyBytes) and explains that without it a
// client "would block the handler goroutine forever". A byte cap does not
// bound TIME -- a client dribbling 4 KiB out over an hour never exceeds it.
// ReadTimeout is the half of that defense the byte cap cannot provide.
//
// The four durations are deliberately generous rather than sized to fit:
// the point is that each is finite, not that each is tight. writeTimeout is
// the one worth stating a reason for -- it has to cover generating and
// writing a whole Prometheus exposition over whatever link the scraper is
// on, and a scrape that trips it looks like a scrape failure. idleTimeout
// has to exceed the scrape interval, or Prometheus re-dials on every scrape
// instead of reusing its keep-alive connection.
//
// Vars, not consts, for the same reason publishAsyncTimeout above is one:
// main_test.go asserts on these. Production code never assigns to them.
var (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

func newHTTPServer(addr string, h http.Handler) *http.Server {
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
		fmt.Println(buildinfo.Line("vantage-collector"))
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
// via signal.NotifyContext, so main_test.go can drive its lifecycle (start,
// feed traffic, cancel, assert) deterministically instead of sending real OS
// signals to the test binary's whole process.
func run(ctx context.Context, cfgPath string) error {
	// Captured before anything below that can block (config load is fast
	// and local, but the NATS connect just after it is not), so uptime
	// reported on /status is process uptime rather than time-since-NATS or
	// time-since-first-peer.
	started := time.Now().UTC()

	cfg, err := collector.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	slog.SetDefault(logging.New(cfg.LogLevel, cfg.LogFormat, os.Stderr))
	slog.Info("vantage-collector starting", "version", buildinfo.Version,
		"revision", buildinfo.Rev(), "goversion", runtime.Version())

	// Validate parses cfg.NatsURL ourselves before it is ever handed to
	// nats.Connect below, for a fast, clear failure on an obvious typo. It
	// is not what keeps a credential out of the log if nats.Connect rejects
	// the value anyway: cfg.NatsURL is a secret type whose every rendering
	// is redacted, and natsutil.Connect's error has already been through
	// redact.Err (see secret's package comment).
	if err := cfg.NatsURL.Validate(); err != nil {
		return fmt.Errorf("nats_url: %w", err)
	}

	nc, err := natsutil.Connect(cfg.NatsURL, cfg.NatsTLS, nats.MaxReconnects(-1), nats.Name("vantage-collector/"+cfg.CollectorID))
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", cfg.NatsURL, err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc,
		jetstream.WithPublishAsyncMaxPending(4096),
		jetstream.WithPublishAsyncTimeout(publishAsyncTimeout),
	)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	if err := natsutil.EnsureStreams(ctx, js, natsutil.StreamOpts{
		Partitions:     cfg.Streams.Partitions,
		Replicas:       cfg.Streams.Replicas,
		LSReplicas:     cfg.Streams.LSReplicas,
		RoutesMaxBytes: cfg.Streams.RoutesMaxBytes,
		RawMaxBytes:    cfg.Streams.RawMaxBytes,
	}); err != nil {
		return fmt.Errorf("ensure streams: %w", err)
	}

	// Ready means publishes can reach NATS. A collector with NATS down
	// still accepts BMP and holds sessions, but everything it parses is
	// failing to publish, which is what a readiness probe should say.
	mux, err := health.MetricsMux("collector", health.NATS(nc))
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	// /status is the same data for the same audience -- see
	// collector.StatusHandler for why it is here and not on admin_listen.
	// started is captured before any session is accepted, so uptime is
	// process uptime rather than time-since-first-peer.
	mux.Handle("/status", collector.StatusHandler(cfg.CollectorID, started))
	metricsSrv := newHTTPServer(cfg.MetricsListen, mux)
	go func() {
		slog.Info("metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// The metrics endpoint is best-effort: a failure to bind it
			// (e.g. the port is already in use) must not prevent this
			// daemon from doing its actual job of accepting BMP sessions
			// and publishing their events.
			slog.Error("metrics server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	pub := natsutil.NewPublisher(js)
	// Surface server-side rejections when they resolve, not just at the
	// shutdown Drain: a stream deleted underneath a running collector would
	// otherwise produce no signal at all until the process restarts.
	pub.OnError = collector.CountPublishReject
	// A publish that fails in flight (NATS restarting, a stream electing a
	// leader) is re-sent rather than failed; count and log each one.
	pub.OnRetry = collector.CountPublishRetry
	defer collector.FlushPublishRetryLog()
	// On shutdown, a publish waiting for room in a full retry budget must not
	// hold its session -- and so Serve, and so the Drain report -- for the
	// rest of the wait. Retries already under way keep going; Drain reports
	// them.
	go func() {
		<-ctx.Done()
		pub.StopWaiting()
	}()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}
	// proxy_protocol is on this line because during a hard cutover "is the
	// flag actually on in this pod?" is the first question asked, and the
	// answer was otherwise only in the ConfigMap -- one indirection away from
	// the logs someone is already reading.
	slog.Info("bmp listening", "addr", cfg.Listen, "collector_id", cfg.CollectorID,
		"proxy_protocol", cfg.ProxyProtocol)
	if cfg.CollectorIDDerived {
		// Not an error, and not always a problem: on a VM, a laptop or a
		// StatefulSet pod the hostname is a perfectly stable identity and
		// this is the right default. It is logged because the case where it
		// is wrong is silent and permanent.
		//
		// collector_id is half of session identity -- current state is
		// max(session_id) per (collector_id, router_ip) -- so if a
		// replacement process comes up under a different name, its sessions
		// land under a pairing the old ones never used and can never
		// displace them. The dead view stays in the archive for as long as
		// retention holds it, and after a hard kill (where no view-lost
		// event is emitted) its peers stay recorded up and its routes keep
		// being served as current.
		slog.Warn("collector_id was taken from the hostname; set it explicitly "+
			"if this process can be replaced under a different name",
			"collector_id", cfg.CollectorID,
			"why", "collector_id is half of session identity, so an id that changes "+
				"across a restart strands the previous session's view in the archive")
	}

	srv := collector.NewServer(cfg, pub, time.Now)

	// The admin API runs on its own listener, not the metrics one: arming a
	// mirror is a real capability (it copies another router's BMP traffic
	// into the raw stream), and metrics ports are routinely scraped from far
	// more broadly than an admin control plane should be reachable from --
	// see admin.go's package doc. It shares srv.Mirrors with the accept loop
	// started by srv.Serve below, so arming here takes effect on the very
	// same registry every connection goroutine's handleConn checks.
	adminSrv := newHTTPServer(cfg.AdminListen, collector.NewAdminHandler(srv.Mirrors, cfg.AdminListen))
	go func() {
		slog.Info("admin listening", "addr", cfg.AdminListen)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Best-effort, like the metrics endpoint above: a failure to
			// bind the admin listener must not prevent this daemon from
			// accepting BMP sessions and publishing their events.
			slog.Error("admin server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = adminSrv.Shutdown(shutdownCtx)
	}()

	// The first beat goes out before Serve accepts a session, so a restarted
	// collector's epoch is visible as soon as the new process is. started was
	// captured at the top of run, before anything that mints a session id, so
	// every session this process opens has an id at or above it -- which is
	// what the read side's epoch rule relies on. A first beat that fails is
	// dropped like any other; the next is BeatInterval away.
	hb := collector.NewHeartbeat(cfg.CollectorID, started, pub, time.Now)
	// A beat attempted while the client is still reconnecting is dropped, so
	// the connection's return asks for one at once rather than at the next
	// tick. It is set before the first beat so a reconnect from here on is
	// never missed; Run takes a request made before it starts. Nothing else
	// sets this connection's reconnect handler.
	nc.SetReconnectHandler(func(*nats.Conn) { hb.Reconnected() })
	if err := hb.Beat(ctx); err != nil {
		slog.Warn("first heartbeat not published; the next one replaces it", "err", err)
	}
	hbCtx, hbStop := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		hb.Run(hbCtx, slog.Default())
	}()

	serveErr := srv.Serve(ctx, ln)
	// Serve can return on a dead listener without ctx being canceled, so the
	// heartbeat gets its own stop rather than waiting on ctx.
	hbStop()
	<-hbDone
	// Serve only returns once every in-flight connection goroutine has
	// itself returned (see Server.Serve's doc comment), so every Publish
	// call any session ever issued has already happened by this point, and
	// Drain here is guaranteed to wait for, and report the fate of, all of
	// them -- not a snapshot that misses events a still-running session
	// goroutine was about to publish.
	drainErr := pub.Drain(drainTimeout)
	if drainErr != nil {
		slog.Error("publisher drain reported rejected publishes", "err", drainErr)
	}
	// A drain error means data this daemon told the caller/log it had
	// handled was in fact never durably stored -- that must reach the
	// process exit code, not just a log line, or it is silently lost the
	// same way discarding this return value would lose it.
	return errors.Join(serveErr, drainErr)
}
