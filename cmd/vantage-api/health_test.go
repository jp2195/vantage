package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

func httpGet(url string) (int, string) {
	resp, err := http.Get(url)
	if err != nil {
		return -1, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

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

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestRunServesHealthAndMetrics reads the metrics listener of a real run:
// the probes, the build-info gauge, and the request counter -- which lives
// on the Server, not the default registry, so this is the test that notices
// run forgetting to register it.
func TestRunServesHealthAndMetrics(t *testing.T) {
	requireDevStack(t)
	ch := newCutProxy(t, chtest.Addr())
	apiAddr, metricsAddr := freeAddr(t), freeAddr(t)
	path := writeTempYAML(t, "listen: "+apiAddr+"\nmetrics_listen: "+metricsAddr+"\n"+
		"clickhouse_dsn: \""+strings.Replace(devDSN(), chtest.Addr(), ch.addr(), 1)+"\"\n"+
		"tokens:\n  - name: test\n    token: not-a-real-secret\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(slog.DiscardHandler)) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("run did not return after cancel")
		}
	}()

	base := "http://" + metricsAddr
	if body := waitStatus(t, base+"/readyz", 200, 15*time.Second); body != "ok\n" {
		t.Errorf("/readyz body = %q", body)
	}
	if code, _ := httpGet(base + "/healthz"); code != 200 {
		t.Errorf("/healthz = %d", code)
	}
	if code, _ := httpGet("http://" + apiAddr + "/v1/openapi.yaml"); code != 200 {
		t.Fatalf("/v1/openapi.yaml = %d", code)
	}
	_, m := httpGet(base + "/metrics")
	for _, want := range []string{
		`vantage_build_info{component="api",`,
		`vantage_api_http_requests_total{code="200",route="GET /v1/openapi.yaml"} 1`,
		`vantage_api_http_request_duration_seconds_count{route="GET /v1/openapi.yaml"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}

	// ClickHouse goes away. The cached ping answers for up to five seconds,
	// so the 503 may take that long to appear.
	ch.cut()
	reason := waitStatus(t, base+"/readyz", 503, 15*time.Second)
	if !strings.HasPrefix(reason, "not ready: clickhouse:") {
		t.Errorf("/readyz reason = %q, want it to name clickhouse", reason)
	}
	if code, _ := httpGet(base + "/healthz"); code != 200 {
		t.Errorf("/healthz = %d with ClickHouse down; liveness must not follow a dependency", code)
	}
}

// TestRunInstallsTheConfiguredLogger: with no logger passed (main's case),
// run builds one from log_level and log_format and makes it slog's default.
// The dial fails -- nothing listens on port 1 -- after the logger is in.
func TestRunInstallsTheConfiguredLogger(t *testing.T) {
	defer func(l *slog.Logger) { slog.SetDefault(l) }(slog.Default())
	path := writeTempYAML(t, "listen: 127.0.0.1:0\nmetrics_listen: 127.0.0.1:0\n"+
		"clickhouse_dsn: \"clickhouse://u@127.0.0.1:1/vantage?dial_timeout=100ms\"\n"+
		"log_level: warn\nlog_format: json\n"+
		"tokens:\n  - name: test\n    token: not-a-real-secret\n")
	if err := run(context.Background(), path, nil); err == nil {
		t.Fatal("run succeeded against port 1")
	}
	h := slog.Default().Handler()
	if _, ok := h.(*slog.JSONHandler); !ok {
		t.Errorf("default handler is %T, want the JSON handler log_format asked for", h)
	}
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("default handler does not honor log_level: warn")
	}
}

// cutProxy forwards TCP connections to a target until cut is called, then
// closes every one of them and refuses new ones: a ClickHouse outage a test
// can switch on without stopping the shared ClickHouse.
type cutProxy struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln}
	t.Cleanup(p.cut)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, u)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(u, c); u.Close() }()
			go func() { _, _ = io.Copy(c, u); c.Close() }()
		}
	}()
	return p
}

func (p *cutProxy) addr() string { return p.ln.Addr().String() }

func (p *cutProxy) cut() {
	p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}
