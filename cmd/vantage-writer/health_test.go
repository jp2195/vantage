package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/natsutil"
)

// writerCmdTestDB is this command's own database; see chtest.
const writerCmdTestDB = "vantage_writer_cmd_test"

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

// TestRunServesHealthAndBuildInfo drives the real run against a live
// ClickHouse and a NATS server this test can stop.
func TestRunServesHealthAndBuildInfo(t *testing.T) {
	chtest.Require(t, t.Context(), writerCmdTestDB)

	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := natsutil.EnsureStreams(t.Context(), js, natsutil.StreamOpts{
		Replicas: 1, LSReplicas: 1, RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	nc.Close()

	metricsAddr := freeAddr(t)
	ch := newCutProxy(t, chtest.Addr())
	path := writeWriterConfig(t, ns.ClientURL(),
		strings.Replace(chtest.DSN(writerCmdTestDB), chtest.Addr(), ch.addr(), 1))
	appendConfig(t, path, fmt.Sprintf("metrics_listen: %q\nlog_level: warn\n", metricsAddr))

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
	waitStatus(t, base+"/readyz", 200, 15*time.Second)
	if code, b := httpGet(base + "/healthz"); code != 200 || b != "ok\n" {
		t.Errorf("/healthz = %d %q", code, b)
	}
	if _, m := httpGet(base + "/metrics"); !strings.Contains(m, `vantage_build_info{component="writer",`) {
		t.Errorf("/metrics has no writer build-info series")
	}

	// ClickHouse goes away first, with NATS still up. The cached ping
	// answers for up to five seconds, so the 503 may take that long.
	ch.cut()
	reason := waitStatus(t, base+"/readyz", 503, 15*time.Second)
	if !strings.HasPrefix(reason, "not ready: clickhouse:") {
		t.Errorf("/readyz reason = %q, want it to name clickhouse", reason)
	}

	ns.Shutdown()
	// NATS is checked first, so its failure is the reason once both are down.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasPrefix(reason, "not ready: nats:") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		_, reason = httpGet(base + "/readyz")
	}
	if !strings.HasPrefix(reason, "not ready: nats:") {
		t.Errorf("/readyz reason = %q, want it to name nats", reason)
	}
	if code, _ := httpGet(base + "/healthz"); code != 200 {
		t.Errorf("/healthz = %d with NATS down", code)
	}
}

// TestRunInstallsTheConfiguredLogger: run makes the log_level/log_format
// logger slog's default before anything else can log. The NATS dial fails
// (nothing listens on port 1) after that has happened.
func TestRunInstallsTheConfiguredLogger(t *testing.T) {
	defer func(l *slog.Logger) { slog.SetDefault(l) }(slog.Default())
	path := writeWriterConfig(t, "nats://127.0.0.1:1", "clickhouse://u@127.0.0.1:1/vantage")
	appendConfig(t, path, "log_level: error\nlog_format: json\n")
	if err := run(context.Background(), path); err == nil {
		t.Fatal("run succeeded against port 1")
	}
	h := slog.Default().Handler()
	if _, ok := h.(*slog.JSONHandler); !ok {
		t.Errorf("default handler is %T, want the JSON handler log_format asked for", h)
	}
	if h.Enabled(context.Background(), slog.LevelWarn) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("default handler does not honor log_level: error")
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
