package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// httpGet returns status and body, or -1 when nothing answers.
func httpGet(url string) (int, string) {
	resp, err := http.Get(url)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitStatus polls url until it answers code or timeout elapses.
func waitStatus(t *testing.T, url string, code int, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got int
	var body string
	for time.Now().Before(deadline) {
		if got, body = httpGet(url); got == code {
			return body
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s: last answer %d %q, want %d", url, got, body, code)
	return ""
}

// TestRunServesHealthAndBuildInfo drives the real run against a NATS
// server this test can stop, and reads the metrics listener the way a
// kubelet and a Prometheus would.
func TestRunServesHealthAndBuildInfo(t *testing.T) {
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	defer ns.Shutdown()

	metricsAddr := freeAddr(t)
	path := filepath.Join(t.TempDir(), "collector.yaml")
	body := fmt.Sprintf("listen: %q\nnats_url: %q\ncollector_id: test-collector\nmetrics_listen: %q\nadmin_listen: %q\n"+
		"log_level: warn\nstreams:\n  routes_max_bytes: 16777216\n  raw_max_bytes: 16777216\n",
		freeAddr(t), ns.ClientURL(), metricsAddr, freeAddr(t))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("run did not return after cancel")
		}
	}()

	base := "http://" + metricsAddr
	waitStatus(t, base+"/readyz", 200, 10*time.Second)
	if code, b := httpGet(base + "/healthz"); code != 200 || b != "ok\n" {
		t.Errorf("/healthz = %d %q", code, b)
	}
	if _, m := httpGet(base + "/metrics"); !strings.Contains(m, `vantage_build_info{component="collector",`) {
		t.Errorf("/metrics has no collector build-info series")
	}
	if code, _ := httpGet(base + "/status"); code != 200 {
		t.Errorf("/status = %d, still expected on the metrics listener", code)
	}

	ns.Shutdown()
	reason := waitStatus(t, base+"/readyz", 503, 10*time.Second)
	if !strings.HasPrefix(reason, "not ready: nats:") {
		t.Errorf("/readyz reason = %q, want it to name nats", reason)
	}
	if code, _ := httpGet(base + "/healthz"); code != 200 {
		t.Errorf("/healthz = %d with NATS down; liveness must not follow a dependency", code)
	}
}

// TestRunInstallsTheConfiguredLogger: run makes the log_level/log_format
// logger slog's default before anything else can log. The NATS dial fails
// (nothing listens on port 1) after that has happened.
func TestRunInstallsTheConfiguredLogger(t *testing.T) {
	defer func(l *slog.Logger) { slog.SetDefault(l) }(slog.Default())
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte("nats_url: \"nats://127.0.0.1:1\"\ncollector_id: t\n"+
		"log_level: debug\nlog_format: json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), path); err == nil {
		t.Fatal("run succeeded against port 1")
	}
	h := slog.Default().Handler()
	if _, ok := h.(*slog.JSONHandler); !ok {
		t.Errorf("default handler is %T, want the JSON handler log_format asked for", h)
	}
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("default handler does not honor log_level: debug")
	}
}
