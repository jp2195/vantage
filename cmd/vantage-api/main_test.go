package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// apiCmdTestDB is this command's own test database, for the reason every
// other package has one: a test that dials the archive would be a test that
// can be broken by the archive.
const apiCmdTestDB = "vantage_api_cmd_test"

// requireDevStack guarantees apiCmdTestDB exists with the shipped schema
// applied, and skips the test when ClickHouse is unreachable. It returns
// nothing: the value it produces is the database's existence, since run
// dials by DSN rather than taking a connection.
func requireDevStack(t *testing.T) {
	t.Helper()
	chtest.Require(t, t.Context(), apiCmdTestDB)
}

func devDSN() string { return chtest.DSN(apiCmdTestDB) }

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMainRefusesToStartWithoutTokens enforces the tokens-required default
// at the top level, not just in LoadConfig: the daemon must fail loudly at
// startup rather than bind a port and serve unauthenticated.
//
// The config it is given is otherwise complete and names no ClickHouse, so
// this also pins the ORDER of run's startup steps: the refusal has to
// happen before the dial, or the test's outcome would depend on whether a
// database happened to be reachable.
func TestMainRefusesToStartWithoutTokens(t *testing.T) {
	path := writeTempYAML(t, "listen: 127.0.0.1:0\nmetrics_listen: 127.0.0.1:0\n")
	err := run(context.Background(), path, slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("run() started with no tokens configured")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should name the missing tokens; got %v", err)
	}
}

// TestServersSetEveryTimeout: a zero-value http.Server has no timeouts at
// all, so one client sending a byte a minute holds a goroutine and an fd
// forever. cmd/vantage-writer learned this already.
func TestServersSetEveryTimeout(t *testing.T) {
	for name, srv := range map[string]*http.Server{
		"api":     newAPIServer("127.0.0.1:0", http.NotFoundHandler()),
		"metrics": newMetricsServer("127.0.0.1:0", http.NotFoundHandler()),
	} {
		t.Run(name, func(t *testing.T) {
			if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 ||
				srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
				t.Errorf("%s server has an unset timeout: %+v", name, srv)
			}
		})
	}
}

// TestAPIServerWriteTimeoutCoversTheSlowestQuery ties the API listener's
// write timeout to a documented cost rather than to the metrics listener's.
//
// api/openapi.yaml records covers= as a full scan measured at 31-52ms over
// 3.2M rows, and /v1/routes runs three of those concurrently. A write
// timeout covers handler time as well as the write, so one set too tight
// turns the slowest legitimate query into a truncated response -- which
// reaches the client as a connection reset, not as an error body, and is
// therefore the failure least likely to be diagnosed correctly.
func TestAPIServerWriteTimeoutCoversTheSlowestQuery(t *testing.T) {
	srv := newAPIServer("127.0.0.1:0", http.NotFoundHandler())
	if srv.WriteTimeout < apiWriteTimeout {
		t.Errorf("api write timeout is %v, below the documented %v",
			srv.WriteTimeout, apiWriteTimeout)
	}
	// A read timeout below the write timeout would cap it: the request has
	// to be read before the handler runs.
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Errorf("read-header timeout %v exceeds read timeout %v",
			srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}

// TestRunReturnsWhenTheContextIsCanceled proves the shutdown path exists
// and terminates. Without it, run's only exit is a listener error, and a
// SIGTERM would leave the process to be killed rather than drained.
//
// It needs a real database, because run dials one before it binds anything;
// the cancellation being tested is the one after startup succeeds.
func TestRunReturnsWhenTheContextIsCanceled(t *testing.T) {
	requireDevStack(t)
	path := writeTempYAML(t, "listen: 127.0.0.1:0\nmetrics_listen: 127.0.0.1:0\n"+
		"clickhouse_dsn: \""+devDSN()+"\"\n"+
		"tokens:\n  - name: test\n    token: not-a-real-secret\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(slog.DiscardHandler)) }()

	// Cancel immediately. run either has not finished starting -- in which
	// case it must still return -- or is serving, in which case it must
	// shut down. Both are the property under test, so there is nothing to
	// wait for first and no sleep here.
	cancel()
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("run returned %v on cancellation, want nil or a canceled context", err)
		}
	case <-time.After(30 * time.Second):
		// A deadlock guard, not a wait: the pass path takes microseconds
		// and never reaches this branch. It is here so a run that never
		// returns fails with this sentence instead of go test's own
		// package-wide timeout, which names no cause.
		t.Fatal("run did not return 30s after its context was canceled; " +
			"the shutdown path does not terminate")
	}
}
