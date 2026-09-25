// Command vantage-api is the HTTP read API over the ClickHouse archive:
// fifteen GET paths, thirteen of them answered by way of query/ and the
// other two -- the contract itself and the auth-mode probe -- answered
// from the binary without asking the database anything. Of the thirteen,
// six are answered by exactly one query/ call apiece (routers, peers,
// history, and the three /v1/rib pages); the rest fan out to two or more
// query/ calls each, most visibly /v1/routes, which asks after all three
// route families at once. All fifteen conform to api/openapi.yaml, which
// this daemon also serves, from the copy compiled into it.
//
// Most of the fifteen are authenticated unless an operator has written
// otherwise. Two of them are public by design rather than by that
// exception -- /v1/auth/config and /v1/openapi.yaml, neither gated because
// a caller has to be able to learn how this daemon expects to be
// authenticated, or read the contract, before it can be expected to hold a
// credential; api/server.go's routing table marks them so, and
// api/openapi.yaml gives both an empty security requirement, not this
// binary. Separately, and unconditionally, auth.mode: none lets an operator
// open all fifteen paths, the two public ones included -- the section below
// is what that means and why the exception exists.
//
// It writes nothing. Not to ClickHouse, not to NATS, not to disk: every
// statement it issues is a SELECT, and the only state it holds between
// requests is the connection pool. That is worth saying at the top because
// it is what makes the deployment story simple -- this daemon can be
// restarted, run in several copies, or killed mid-request without anything
// needing to be reconciled afterward.
//
// # Authenticated unless an operator says otherwise, in writing
//
// api.LoadConfig refuses a config with no tokens UNLESS auth.mode is set
// to none, and run below returns any such refusal before it dials anything
// or binds anything. Requiring tokens remains the default for every config
// that does not say otherwise -- including every config that predates
// auth.mode existing at all -- because the stack already carries one
// unauthenticated path to arbitrary ClickHouse SQL (Grafana's anonymous
// access), and a second would be worse than the first, since this one
// answers in JSON that a script can consume.
//
// auth.mode: none exists anyway, for a deployment that already gates
// access with a fronting proxy or a VPN and has no use for a second
// credential on top of it. It is not a flag that quietly widens what a
// default config accepts -- an operator has to type it, and api.NewServer
// logs a warning every time this daemon starts under it -- so the one
// thing this section used to promise, that authentication cannot be turned
// off by convenience or by omission, still holds. What changed is that it
// can now be turned off on purpose, with the cost written down instead of
// hidden.
//
// # Why the dial goes through sink
//
// sink.NewClickHouse rather than a clickhouse.Open here, for two things
// neither of which is the connection. It owns the DSN redaction -- its doc
// comment states the current guarantee -- and it owns the
// ExpectedSchemaVersion guard, which refuses a database
// whose shape this build was not written against. A second dial path in
// this file would re-derive both, and the failure mode of getting the
// second one wrong is an API that cheerfully serves wrong answers from a
// migrated database.
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

	"golang.org/x/sync/errgroup"

	"github.com/jp2195/vantage/api"
	"github.com/jp2195/vantage/buildinfo"
	"github.com/jp2195/vantage/health"
	"github.com/jp2195/vantage/logging"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/sink"
)

// The timeouts both listeners carry. A zero-value http.Server has none --
// not on reading headers, not on reading a body, not on writing a response,
// not on an idle keep-alive connection -- so a single client that opens a
// connection and sends a byte a minute holds a goroutine and a file
// descriptor indefinitely.
//
// Vars, not consts, so main_test.go can assert on them. Production code
// never assigns to them.
var (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	idleTimeout       = 120 * time.Second

	// metricsWriteTimeout covers a whole Prometheus exposition over a slow
	// link, the same value and reasoning as vantage-writer's.
	metricsWriteTimeout = 30 * time.Second

	// apiWriteTimeout is larger, and the difference is the point of having
	// two. WriteTimeout covers HANDLER time, not merely the write: it runs
	// from the end of the request headers to the end of the response. The
	// most expensive thing this API does is /v1/routes, which fans out to
	// three queries, one of which (covers=) is a full scan by construction
	// -- api/openapi.yaml records it at 31-52ms over 3.2M rows, and that
	// archive will grow. A /v1/rib page at the maximum limit of 10000 rows
	// is the other end of the same question.
	//
	// 30s would probably do today. 60s is chosen because the cost of being
	// wrong is asymmetric and badly diagnosable: a write timeout does not
	// produce an error body, it cuts the connection, so a caller sees a
	// truncated read or a reset rather than anything naming a timeout. An
	// over-generous ceiling costs a held goroutine on a query that was
	// going to be slow anyway; a tight one costs an unexplainable failure
	// on the endpoint a person is most likely to be waiting on.
	apiWriteTimeout = 60 * time.Second

	// shutdownGrace bounds the drain. It exceeds apiWriteTimeout so that a
	// request already in a handler when SIGTERM arrives gets the time it
	// was promised rather than being cut short by the shutdown instead of
	// by its own timeout.
	shutdownGrace = 70 * time.Second
)

// newAPIServer builds the read API's listener.
func newAPIServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      apiWriteTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// newMetricsServer builds the listener for /metrics, /healthz and /readyz.
func newMetricsServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func main() {
	cfgPath := flag.String("config", "", "path to YAML config")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Line("vantage-api"))
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *cfgPath, nil); err != nil {
		// slog.Default, not a logger captured before run: run installs the
		// configured one as the default once the config has loaded.
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// run is main's testable seam. It takes ctx rather than constructing its own
// via signal.NotifyContext, the shape the other two daemons use for the same
// reason -- a test can drive the lifecycle without sending real OS signals
// to the test binary's whole process -- and it takes a logger so a test can
// discard what a daemon would print. A nil logger means the one the config's
// log_level and log_format describe, which run also installs as slog's
// default; main passes nil.
//
// The order of the first three steps is load-bearing and is pinned by
// TestMainRefusesToStartWithoutTokens: config, then validate, then dial.
// Loading first means a daemon with no tokens fails on the tokens, not on
// whichever database happened to be unreachable at the time.
func run(ctx context.Context, cfgPath string, logger *slog.Logger) error {
	cfg, err := api.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if logger == nil {
		logger = logging.New(cfg.LogLevel, cfg.LogFormat, os.Stderr)
		slog.SetDefault(logger)
	}
	logger.Info("vantage-api starting", "version", buildinfo.Version,
		"revision", buildinfo.Rev(), "goversion", runtime.Version())
	// Parsed here, before the value reaches sink.NewClickHouse, for a clear
	// failure on an obvious typo rather than a dial timeout on whatever the
	// library salvaged. It is not what keeps the password out of the log --
	// ClickHouseDSN is a secret type whose every rendering is redacted, and
	// NewClickHouse's own errors have already been through redact.Err.
	if err := cfg.ClickHouseDSN.Validate(); err != nil {
		return fmt.Errorf("clickhouse_dsn: %w", err)
	}

	ch, err := sink.NewClickHouse(ctx, cfg.ClickHouseDSN)
	if err != nil {
		return fmt.Errorf("connect clickhouse %s: %w", cfg.ClickHouseDSN, err)
	}
	defer ch.Close()

	// The database name comes from the DSN by way of sink, not from a
	// constant here. That is what lets this daemon be pointed at a test
	// database the way sink's and query's own tests are, and it means there
	// is exactly one place -- the DSN -- that decides which database the
	// whole process reads.
	q, err := query.New(ch.Conn(), ch.DB())
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	if q, err = q.WithStaleAfter(cfg.StaleAfter); err != nil {
		return fmt.Errorf("stale_after: %w", err)
	}
	srv, err := api.NewServer(q, cfg, logger)
	if err != nil {
		return err
	}

	// /readyz reports whether ClickHouse answers: every route but the two
	// public ones is a query. It is dependency health for monitoring, and
	// the Helm chart deliberately does not use it for readiness: pulling
	// the API out of its Service while ClickHouse is down would replace the
	// API's own error, which says what is wrong, with a connection failure
	// in the UI, which says nothing.
	metricsMux, err := health.MetricsMux("api", health.Cached(health.Ping(ch.Conn())), srv.Metrics())
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	apiSrv := newAPIServer(cfg.Listen, srv.Handler())
	metricsSrv := newMetricsServer(cfg.MetricsListen, metricsMux)

	g, gctx := errgroup.WithContext(ctx)

	// The API listener is supervised: if it cannot bind, this daemon has
	// nothing to do and must exit rather than sit there serving metrics
	// about a service that is not running.
	g.Go(func() error {
		logger.Info("api listening", "addr", cfg.Listen)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("api server: %w", err)
		}
		return nil
	})

	// The metrics listener is best-effort, as in the other two daemons: a
	// failure to bind the metrics port must not stop this one answering
	// queries. It is the observability of the service, not the service.
	g.Go(func() error {
		logger.Info("metrics listening", "addr", cfg.MetricsListen)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server", "err", err)
		}
		return nil
	})

	// The shutdown goroutine, and the reason it is in the group rather than
	// a defer: Shutdown is what makes ListenAndServe return, so the two
	// goroutines above cannot finish until something calls it. gctx is
	// canceled both by a signal (ctx) and by the API listener failing, so
	// a bind failure drains the metrics listener too instead of leaving it
	// holding the process open.
	g.Go(func() error {
		<-gctx.Done()
		// context.Background, not gctx: gctx is already canceled, and a
		// canceled context makes Shutdown return immediately without
		// draining -- which is the opposite of what it is being called for.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		logger.Info("shutting down", "grace", shutdownGrace)
		err := apiSrv.Shutdown(shutdownCtx)
		// Both are shut down even if the first errors: a metrics listener
		// left running would hold the process after the API had drained.
		if err2 := metricsSrv.Shutdown(shutdownCtx); err == nil {
			err = err2
		}
		return err
	})

	// A clean shutdown reaches here with ctx.Err() set and nothing else
	// wrong, which is not a failure of this process. Anything else is.
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
