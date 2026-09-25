package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
)

func serve(t *testing.T, ready Check, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, ready)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestHealthzIsUnconditional: liveness must not depend on a dependency, or
// a ClickHouse outage makes Kubernetes restart every healthy pod in turn.
func TestHealthzIsUnconditional(t *testing.T) {
	failing := func(context.Context) error { return errors.New("down") }
	rec := serve(t, failing, "GET", "/healthz")
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("/healthz with a failing dependency = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestReadyzReportsTheCheck(t *testing.T) {
	rec := serve(t, func(context.Context) error { return nil }, "GET", "/readyz")
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("/readyz ready = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}

	rec = serve(t, func(context.Context) error { return errors.New("clickhouse: ping:\nrefused") }, "GET", "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz not ready = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if strings.Count(body, "\n") != 1 || !strings.HasSuffix(body, "\n") {
		t.Errorf("/readyz reason must be exactly one line; got %q", body)
	}
	if !strings.Contains(body, "clickhouse: ping: refused") {
		t.Errorf("/readyz reason %q does not carry the check's error", body)
	}
}

func TestReadyzOnlyAnswersGET(t *testing.T) {
	rec := serve(t, func(context.Context) error { return nil }, "POST", "/readyz")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /readyz = %d, want 405", rec.Code)
	}
}

// TestCachedCallsAtMostOncePerTTL: a readiness probe every few seconds from
// several kubelets must not turn into a ClickHouse query per probe.
func TestCachedCallsAtMostOncePerTTL(t *testing.T) {
	var calls atomic.Int32
	fail := true
	now := time.Unix(0, 0)
	c := cached(5*time.Second, func() time.Time { return now }, func(context.Context) error {
		calls.Add(1)
		if fail {
			return errors.New("down")
		}
		return nil
	})
	for range 10 {
		if err := c(context.Background()); err == nil {
			t.Fatal("cached result lost the failure")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("10 probes inside the TTL made %d calls, want 1", n)
	}
	fail = false
	now = now.Add(4 * time.Second)
	if err := c(context.Background()); err == nil {
		t.Error("result refreshed before the TTL expired")
	}
	now = now.Add(time.Second)
	if err := c(context.Background()); err != nil {
		t.Errorf("result not refreshed at the TTL: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

// TestCachedIgnoresTheCallersCancellation: the cached answer is shared by
// every prober, so one prober hanging up must not be cached as everyone's
// failure.
func TestCachedIgnoresTheCallersCancellation(t *testing.T) {
	c := Cached(func(ctx context.Context) error { return ctx.Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c(ctx); err != nil {
		t.Errorf("a canceled caller context reached the check: %v", err)
	}
}

// TestCachedBoundsTheCheck: a ping against a black-holed address must not
// hold the probe past the kubelet's own timeout.
func TestCachedBoundsTheCheck(t *testing.T) {
	c := Cached(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("no deadline")
		}
		return nil
	})
	if err := c(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestAllReturnsTheFirstFailure(t *testing.T) {
	ok := func(context.Context) error { return nil }
	bad := func(context.Context) error { return errors.New("second") }
	if err := All(ok, bad, func(context.Context) error { return errors.New("third") })(context.Background()); err == nil || err.Error() != "second" {
		t.Errorf("All = %v, want the first failure", err)
	}
	if err := All(ok, ok)(context.Background()); err != nil {
		t.Errorf("All of passing checks = %v", err)
	}
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func TestPingNamesClickHouse(t *testing.T) {
	if err := Ping(pinger{})(context.Background()); err != nil {
		t.Errorf("Ping ok = %v", err)
	}
	err := Ping(pinger{errors.New("connection refused")})(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "clickhouse: ") {
		t.Errorf("Ping failure = %v, want it prefixed with clickhouse", err)
	}
}

// TestNATSFollowsTheConnectionState drives a real server so the check sees
// the client library's own state machine, not a stub of it.
func TestNATSFollowsTheConnectionState(t *testing.T) {
	s, err := server.NewServer(&server.Options{Port: -1})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	defer s.Shutdown()

	nc, err := nats.Connect(s.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	check := NATS(nc)
	if err := check(context.Background()); err != nil {
		t.Fatalf("connected: %v", err)
	}

	s.Shutdown()
	deadline := time.Now().Add(5 * time.Second)
	for nc.Status() == nats.CONNECTED && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	err = check(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "nats: ") {
		t.Errorf("server gone: check = %v, want a nats error", err)
	}

	nc.Close()
	if err := check(context.Background()); err == nil || !strings.Contains(err.Error(), "CLOSED") {
		t.Errorf("closed: check = %v, want it to name the state", err)
	}
}

// TestMetricsMuxServesAllThree: the one mux every daemon's metrics listener
// is built from carries /metrics (with the build-info gauge and any extra
// collectors) alongside the two probes.
func TestMetricsMuxServesAllThree(t *testing.T) {
	extra := prometheus.NewCounter(prometheus.CounterOpts{Name: "vantage_test_extra_total", Help: "x"})
	extra.Add(3)
	mux, err := MetricsMux("writer", func(context.Context) error { return errors.New("nats: connection is RECONNECTING") }, extra)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	m := get("/metrics")
	if m.Code != 200 {
		t.Fatalf("/metrics = %d", m.Code)
	}
	for _, want := range []string{
		`vantage_build_info{component="writer",`,
		"vantage_test_extra_total 3",
		// The default registry is still served: the daemons' package-level
		// metrics and the Go runtime's live there.
		"go_goroutines",
	} {
		if !strings.Contains(m.Body.String(), want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if rec := get("/healthz"); rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := get("/readyz"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "RECONNECTING") {
		t.Errorf("/readyz = %d %q", rec.Code, rec.Body.String())
	}
	// A second mux in the same process -- every daemon test that calls run
	// twice builds one -- must not collide with the first.
	if _, err := MetricsMux("writer", func(context.Context) error { return nil }); err != nil {
		t.Errorf("second MetricsMux: %v", err)
	}
}
