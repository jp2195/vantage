// Request metrics: vantage_api_http_requests_total{route,code} and
// vantage_api_http_request_duration_seconds{route}.
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The two route values that are not mux patterns. Every other value route
// can take is a pattern from routes(), so the label's cardinality is the
// size of the routing table plus two, whatever paths clients send.
const (
	// routeUI is every request outside /v1: the single-page app and its
	// assets. They are one label because the app's asset names are content
	// hashes that change with every build.
	routeUI = "ui"
	// routeUnmatched is a /v1 request that names no route: an unknown
	// path, a wrong method, or a path the mux would redirect.
	routeUnmatched = "unmatched"
)

// httpMetrics is created per Server and registered by the caller, not on
// the default registry at package init. Anything that links this package
// -- the CLI does -- would otherwise export a request counter for a server
// it never runs, the same trap status/ exists to keep the collector's
// metrics out of.
type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	mux      *http.ServeMux
	patterns map[string]bool
}

func newHTTPMetrics(mux *http.ServeMux, patterns []string) *httpMetrics {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vantage_api_http_requests_total",
			Help: "HTTP requests answered, by route (the mux pattern, \"ui\" or \"unmatched\") and status code.",
		}, []string{"route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vantage_api_http_request_duration_seconds",
			Help:    "Time to answer an HTTP request, by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		mux:      mux,
		patterns: make(map[string]bool, len(patterns)),
	}
	for _, p := range patterns {
		m.patterns[p] = true
	}
	return m
}

func (m *httpMetrics) Describe(ch chan<- *prometheus.Desc) {
	m.requests.Describe(ch)
	m.duration.Describe(ch)
}

func (m *httpMetrics) Collect(ch chan<- prometheus.Metric) {
	m.requests.Collect(ch)
	m.duration.Collect(ch)
}

// route names the request for the route label. The pattern is resolved by
// asking the mux rather than by recording what the mux ran, so a request
// the gate refuses before the mux is reached still counts against the
// route it was for -- a burst of 401s on one endpoint is worth seeing.
//
// The mux's answer is checked against the routing table rather than used
// as is. ServeMux.Handler does not always return a pattern in the pattern
// position: for a CONNECT request it would redirect to a subtree, it
// returns the redirect target, which is the client's path plus a slash.
// No route here is a subtree today, so this is the guard for the day one
// is added, not a fix for a label seen in the wild.
func (m *httpMetrics) route(r *http.Request) string {
	if !isV1Path(r.URL.Path) {
		return routeUI
	}
	if _, p := m.mux.Handler(r); m.patterns[p] {
		return p
	}
	return routeUnmatched
}

// wrap instruments next. It is the outermost handler, so every response
// this daemon writes -- a 421 from hostCheck and a 401 from the gate
// included -- is counted.
func (m *httpMetrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := m.route(r)
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		m.requests.WithLabelValues(route, strconv.Itoa(sw.code)).Inc()
		m.duration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

// statusWriter records the status code a handler wrote. A handler that
// writes a body without calling WriteHeader has sent 200, which is the
// initial value.
type statusWriter struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.code = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
