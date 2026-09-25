package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// requestSeries gathers the request counter from a private registry and
// returns count by "route code".
func requestSeries(t *testing.T, s *Server) (counts map[string]float64, observed map[string]uint64) {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(s.Metrics()); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts, observed = map[string]float64{}, map[string]uint64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			switch mf.GetName() {
			case "vantage_api_http_requests_total":
				counts[labels["route"]+" "+labels["code"]] = m.GetCounter().GetValue()
			case "vantage_api_http_request_duration_seconds":
				if _, ok := labels["code"]; ok {
					t.Errorf("duration histogram carries a code label; the contract gives it route only")
				}
				observed[labels["route"]] = m.GetHistogram().GetSampleCount()
			default:
				t.Errorf("unexpected metric %s", mf.GetName())
			}
		}
	}
	return counts, observed
}

func do(s *Server, method, path, token string) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// TestRequestMetricsLabelByPattern: route is the mux pattern the request
// resolves to -- including for a request the gate refuses before the mux
// runs -- and code is the status actually written.
func TestRequestMetricsLabelByPattern(t *testing.T) {
	s := newTestServerNoDB(t, testToken)
	do(s, "GET", "/v1/openapi.yaml", "")
	do(s, "GET", "/v1/openapi.yaml", "")
	do(s, "GET", "/v1/routers", "") // 401 from the gate
	do(s, "GET", "/", "")
	do(s, "GET", "/assets/index-deadbeef.js", "")

	counts, observed := requestSeries(t, s)
	for key, want := range map[string]float64{
		"GET /v1/openapi.yaml 200": 2,
		"GET /v1/routers 401":      1,
	} {
		if counts[key] != want {
			t.Errorf("requests{%s} = %v, want %v (all: %v)", key, counts[key], want, counts)
		}
	}
	var ui float64
	for k, v := range counts {
		if strings.HasPrefix(k, "ui ") {
			ui += v
		}
	}
	if ui != 2 {
		t.Errorf("the two UI requests were counted %v times under route=ui (all: %v)", ui, counts)
	}
	if observed["GET /v1/openapi.yaml"] != 2 || observed["GET /v1/routers"] != 1 || observed["ui"] != 2 {
		t.Errorf("duration observations = %v", observed)
	}
}

// TestRequestMetricsRouteIsBounded: the route label must never carry the raw
// path. Every path a scanner might send -- unknown endpoints, traversal,
// doubled slashes, trailing slashes, wrong methods -- lands on a label
// drawn from the routing table or the two fixed fallbacks.
func TestRequestMetricsRouteIsBounded(t *testing.T) {
	s := newTestServerNoDB(t, testToken)
	allowed := map[string]bool{"ui": true, "unmatched": true}
	for _, p := range s.routePatterns() {
		allowed[p] = true
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/no-such-endpoint-1"},
		{"GET", "/v1/no-such-endpoint-2"},
		{"GET", "/v1"},
		{"GET", "/v1/"},
		{"GET", "/v1/routers/"},
		{"GET", "/v1//routers"},
		{"GET", "/v1/../v1/routers"},
		{"GET", "//v1/zzz"},
		{"POST", "/v1/routers"},
		{"DELETE", "/v1/openapi.yaml"},
		{"GET", "/wp-login.php"},
		{"GET", "/.env"},
	} {
		for _, tok := range []string{"", testToken} {
			func() {
				// Authenticated requests to a real route would reach a
				// handler with the nil database; only the label is under
				// test, so a panic past the gate is not this test's concern.
				defer func() { _ = recover() }()
				do(s, c.method, c.path, tok)
			}()
		}
	}
	counts, _ := requestSeries(t, s)
	var unmatched float64
	for key, n := range counts {
		route := key[:strings.LastIndex(key, " ")]
		if !allowed[route] {
			t.Errorf("route label %q is not a pattern or a fixed fallback", route)
		}
		if route == "unmatched" {
			unmatched += n
		}
	}
	if unmatched == 0 {
		t.Errorf("no request was labeled unmatched: %v", counts)
	}
	if n := counts["unmatched 401"]; n == 0 {
		t.Errorf("an unauthenticated unknown /v1 path should count as unmatched 401: %v", counts)
	}
}

// TestRequestMetricsRouteRefusesNonPatterns pins the guard in route: given
// a subtree route with a wildcard, ServeMux.Handler answers a CONNECT for
// the unslashed path with the redirect target -- the client's own path --
// where the pattern should be. No route in the table is a subtree today, so
// this builds its own mux rather than going through NewServer.
func TestRequestMetricsRouteRefusesNonPatterns(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/v1/things/{id}/", http.NotFoundHandler())
	m := newHTTPMetrics(mux, []string{"/v1/things/{id}/"})
	req := httptest.NewRequest("CONNECT", "/v1/things/client-chosen-text", nil)
	if _, p := mux.Handler(req); p != "/v1/things/client-chosen-text/" {
		t.Fatalf("precondition: ServeMux returned %q; this Go no longer puts the redirect "+
			"target in the pattern position, and the guard may be unnecessary", p)
	}
	if got := m.route(req); got != routeUnmatched {
		t.Errorf("route = %q, want %q", got, routeUnmatched)
	}
}
