// Package health builds each daemon's metrics listener -- /metrics, /healthz
// and /readyz -- and holds the dependency checks /readyz is built from.
//
// The two answer different questions and must not be merged. /healthz is
// liveness: 200 whenever the process can serve HTTP at all, with no
// dependency consulted, because a liveness probe that fails when ClickHouse
// is down makes Kubernetes restart every healthy replica in turn and fixes
// nothing. /readyz is readiness: 200 only when the dependencies this daemon
// needs to do its job are usable right now, 503 with a one-line reason when
// they are not.
package health

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jp2195/vantage/buildinfo"
	"github.com/jp2195/vantage/redact"
)

// MetricsMux builds the mux a daemon's metrics listener serves: /metrics,
// /healthz and /readyz (ready decides the latter). component labels the
// vantage_build_info gauge.
//
// /metrics serves the default registry -- where every package-level metric
// in this project and the Go runtime's collectors live -- together with a
// registry private to this mux holding the build-info gauge and extra. The
// private registry is what lets a daemon's tests call run more than once in
// one process: nothing built here touches the default registry, so a second
// mux cannot collide with the first.
func MetricsMux(component string, ready Check, extra ...prometheus.Collector) (*http.ServeMux, error) {
	reg := prometheus.NewRegistry()
	if err := buildinfo.Register(reg, component); err != nil {
		return nil, err
	}
	for _, c := range extra {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer,
		promhttp.HandlerFor(prometheus.Gatherers{prometheus.DefaultGatherer, reg}, promhttp.HandlerOpts{})))
	Register(mux, ready)
	return mux, nil
}

// Check reports whether one dependency is usable. A nil error means ready.
type Check func(ctx context.Context) error

// Register adds GET /healthz and GET /readyz to mux. ready is consulted on
// every /readyz request; wrap an expensive check in Cached.
func Register(mux *http.ServeMux, ready Check) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := ready(r.Context()); err != nil {
			// One line, whatever the error's own shape: a probe log or a
			// `kubectl describe` shows the first line and nothing else.
			reason := strings.Join(strings.Fields(err.Error()), " ")
			writeText(w, http.StatusServiceUnavailable, "not ready: "+reason)
			return
		}
		writeText(w, http.StatusOK, "ok")
	})
}

func writeText(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body + "\n"))
}

// All is ready when every check is, and otherwise reports the first failure
// in argument order.
func All(checks ...Check) Check {
	return func(ctx context.Context) error {
		for _, c := range checks {
			if err := c(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}

// NATS is ready while nc's state is CONNECTED. It reads the client's own
// state and sends nothing, so it is cheap enough to run on every probe:
// during a reconnect the state is RECONNECTING, which is exactly the window
// in which a publish or a fetch would stall.
func NATS(nc *nats.Conn) Check {
	return func(context.Context) error {
		if st := nc.Status(); st != nats.CONNECTED {
			return fmt.Errorf("nats: connection is %s", st)
		}
		return nil
	}
}

// Pinger is the one method of a ClickHouse connection Ping needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Ping is ready while a ClickHouse ping succeeds. It makes a round trip, so
// it belongs inside Cached.
func Ping(p Pinger) Check {
	return func(ctx context.Context) error {
		if err := p.Ping(ctx); err != nil {
			// redact.Err because a driver error can quote the DSN it was
			// dialing, and this text goes out over HTTP.
			return fmt.Errorf("clickhouse: %w", redact.Err(err))
		}
		return nil
	}
}

// cacheTTL is how long Cached reuses one result. Kubelets probe every few
// seconds, several of them per pod, and a readiness answer five seconds old
// is still an answer; a ClickHouse query per probe is load for nothing.
const cacheTTL = 5 * time.Second

// checkTimeout bounds one underlying check, so a black-holed dependency
// makes /readyz answer 503 promptly instead of hanging past the probe's own
// timeout.
const checkTimeout = 2 * time.Second

// Cached runs c at most once per cacheTTL and returns the stored result to
// every call in between, success and failure alike.
func Cached(c Check) Check { return cached(cacheTTL, time.Now, c) }

func cached(ttl time.Duration, now func() time.Time, c Check) Check {
	var (
		mu   sync.Mutex
		at   time.Time
		last error
		have bool
	)
	return func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if have && now().Sub(at) < ttl {
			return last
		}
		// Not the caller's context: the result is shared by every prober,
		// so one prober hanging up must not be stored as everyone's
		// failure. The lock is held across the check on purpose -- probes
		// that arrive during it wait for its answer rather than each
		// starting their own.
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		last = c(ctx)
		at = now()
		have = true
		return last
	}
}
