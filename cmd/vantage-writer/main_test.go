package main

// This file's tests exercise run(ctx, cfgPath) directly, the way
// cmd/vantage-collector/main_test.go does, for one specific behavior that
// has no other test coverage: a malformed nats_url or clickhouse_dsn must
// not leak its password into run's returned error, which is exactly what
// reaches this binary's own slog.Error("fatal", ...) and therefore stdout.
// Neither test needs a real NATS or ClickHouse: redact.CheckURL
// rejects a malformed connection string before run ever tries to dial
// anything, so both tests are fast and infrastructure-free.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeWriterConfig writes a minimal writer.yaml naming natsURL and
// clickHouseDSN, mirroring cmd/vantage-collector/main_test.go's writeConfig.
func writeWriterConfig(t *testing.T, natsURL, clickHouseDSN string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "writer.yaml")
	body := "nats_url: " + quoteYAML(natsURL) + "\nclickhouse_dsn: " + quoteYAML(clickHouseDSN) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// quoteYAML wraps s in double quotes for a YAML scalar, backslash-escaping
// any '"' or '\' it contains -- both of which this file's tests deliberately
// put in a password, so a naive %q-free literal would break the YAML.
func quoteYAML(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// TestRunRedactsMalformedNatsURLPassword mirrors
// cmd/vantage-collector/main_test.go's test of the same name: a nats_url
// whose password contains a '"' must not leak into run's returned error.
func TestRunRedactsMalformedNatsURLPassword(t *testing.T) {
	const password = `sup"secret`
	natsURL := `nats://user:` + password + `@127.0.0.1:1`
	path := writeWriterConfig(t, natsURL, "clickhouse://vantage:vantage@127.0.0.1:1/vantage")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, path)
	if err == nil {
		t.Fatal("run with a malformed nats_url returned a nil error, want a rejection")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("run's error %q contains the raw password %q", err.Error(), password)
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("run's error %q does not mention the expected \"invalid URL\" -- "+
			"did the CheckURL short-circuit not fire?", err.Error())
	}
}

// TestRunRedactsMalformedClickHouseDSNPassword covers the second
// credential-bearing field this binary has that vantage-collector does not:
// a clickhouse_dsn whose password contains a backslash must not leak into
// run's returned error either.
func TestRunRedactsMalformedClickHouseDSNPassword(t *testing.T) {
	const password = `sup\secret`
	dsn := `clickhouse://vantage:` + password + `@127.0.0.1:1/vantage`
	path := writeWriterConfig(t, "nats://127.0.0.1:1", dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, path)
	if err == nil {
		t.Fatal("run with a malformed clickhouse_dsn returned a nil error, want a rejection")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("run's error %q contains the raw password %q", err.Error(), password)
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("run's error %q does not mention the expected \"invalid URL\" -- "+
			"did the CheckURL short-circuit not fire?", err.Error())
	}
}

// TestRunRedactsMalformedNatsURLPasswordInMultiServerList mirrors
// cmd/vantage-collector/main_test.go's test of the same name: NATS's
// documented HA syntax is a comma-separated list of full URLs in one
// nats_url string, and a single-URL check/redaction only ever looked at the
// first one. The bad segment is placed last, the position an
// implementation that only checks the first segment would miss entirely.
func TestRunRedactsMalformedNatsURLPasswordInMultiServerList(t *testing.T) {
	const password = `sup"secret`
	natsURL := "nats://user:goodpass@127.0.0.1:1,nats://user:" + password + "@127.0.0.1:2"
	path := writeWriterConfig(t, natsURL, "clickhouse://vantage:vantage@127.0.0.1:1/vantage")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := run(ctx, path)
	if err == nil {
		t.Fatal("run with a malformed multi-server nats_url returned a nil error, want a rejection")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("run's error %q contains the raw password %q", err.Error(), password)
	}
	if strings.Contains(err.Error(), "goodpass") {
		t.Fatalf("run's error %q contains the first segment's password %q", err.Error(), "goodpass")
	}
	if !strings.Contains(err.Error(), "invalid URL") {
		t.Fatalf("run's error %q does not mention the expected \"invalid URL\" -- "+
			"did the CheckNatsURL short-circuit not fire?", err.Error())
	}
}

// TestRunRedactsAWellFormedButUnreachableNatsURL covers the half of the
// connect path the tests above do not: a nats_url that is perfectly valid,
// passes Validate, is handed to nats.Connect, and fails on the dial. That
// error carries the raw URL through nats.go, and the message run builds
// from it is the one that reaches slog.Error("fatal", ...).
//
// It is also the test that would fail if cfg.NatsURL went back to being a
// string: the format string here ("connect nats %s") is run's own, and it
// is safe now only because the value it formats renders redacted.
func TestRunRedactsAWellFormedButUnreachableNatsURL(t *testing.T) {
	const sentinel = "SECRET"
	for _, natsURL := range []string{
		"nats://user:s3cr3t" + sentinel + "@127.0.0.1:1",
		"nats://token" + sentinel + "@127.0.0.1:1",
		"user:s3cr3t" + sentinel + "@127.0.0.1:1",
		"nats://user:a" + sentinel + "@127.0.0.1:1,nats://user:b" + sentinel + "@127.0.0.1:2",
	} {
		path := writeWriterConfig(t, natsURL, "clickhouse://vantage:vantage@127.0.0.1:1/vantage")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := run(ctx, path)
		cancel()
		if err == nil {
			t.Fatalf("run with an unreachable nats_url %q returned nil, want a dial failure", natsURL)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("run's error %q contains the credential from %q", err.Error(), natsURL)
		}
		if !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("run's error %q does not name the endpoint", err.Error())
		}
	}
}

// TestNewMetricsServerEnforcesTimeouts is the writer's copy of
// cmd/vantage-collector/main_test.go's TestNewHTTPServerEnforcesTimeouts,
// and exists for the same reason: a zero-value http.Server has no timeout
// on anything, so a client that opens a connection and never finishes its
// headers holds a goroutine and a file descriptor for as long as it likes.
// MetricsListen defaults to "0.0.0.0:9472", so that client need not be
// local. The test proves the behavior (the server hangs up on a stalled
// client) rather than asserting a struct field is non-zero.
func TestNewMetricsServerEnforcesTimeouts(t *testing.T) {
	orig := readHeaderTimeout
	readHeaderTimeout = 150 * time.Millisecond
	defer func() { readHeaderTimeout = orig }()

	srv := newMetricsServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for name, got := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	} {
		if got == 0 {
			t.Errorf("newMetricsServer: %s is 0, meaning no bound at all", name)
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /metrics HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	switch _, err := conn.Read(buf); {
	case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET), err == nil:
		// Hung up (or answered 408 and then hung up): either way it acted.
	case errors.Is(err, os.ErrDeadlineExceeded):
		t.Fatal("connection still open 5s after a stalled request with a " +
			"150ms ReadHeaderTimeout: the metrics server is not enforcing it")
	default:
		t.Fatalf("unexpected read error: %v", err)
	}
}

// appendConfig adds lines to a config writeWriterConfig wrote.
func appendConfig(t *testing.T, path, more string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(more); err != nil {
		t.Fatal(err)
	}
}
