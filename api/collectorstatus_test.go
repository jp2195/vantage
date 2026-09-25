package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jp2195/vantage/status"
)

// TestCollectorStatusesAttributesAnUnreachableEndpoint is why the config
// carries an id at all. A collector that does not answer cannot state its own
// id, so without the configured one an outage would render as a card that
// simply vanished -- the single most misleading thing this screen could do.
func TestCollectorStatusesAttributesAnUnreachableEndpoint(t *testing.T) {
	// A closed listener: connection refused, immediately, no timeout wait.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	s := &Server{cfg: Config{Collectors: []CollectorEndpoint{{ID: "dev-c1", URL: dead}}}}
	got := s.collectorStatuses(context.Background())

	st, ok := got["dev-c1"]
	if !ok {
		t.Fatalf("an unreachable endpoint produced no entry at all; got %v", got)
	}
	if st.Report != nil {
		t.Errorf("Report = %+v, want nil for an endpoint that did not answer", st.Report)
	}
	if st.Err == "" {
		t.Error("Err is empty; the card cannot say why it has no process facts")
	}
}

// TestCollectorStatusesCarriesADisagreementRatherThanResolvingIt: a daemon
// answering under a different id than its config entry names is a
// misconfiguration pointing one card at another collector's process. Both
// ids survive this layer -- the configured one as the key, the daemon's own
// in ReportedID -- so that handleCollectors, which is the only place that
// can see the WHOLE config at once, decides which row each belongs on.
//
// The key is the CONFIGURED id, and that is load-bearing rather than
// incidental: it is the only id validate() guarantees unique, so it is the
// only one two polls cannot collide on. See collectorStatuses' own doc
// comment, and TestCollectorsKeepsEveryConfiguredIDWhenTwoDaemonsReport
// TheSameID for what the collision did to the response.
func TestCollectorStatusesCarriesADisagreementRatherThanResolvingIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status.Report{CollectorID: "actually-c2"})
	}))
	defer srv.Close()

	s := &Server{cfg: Config{Collectors: []CollectorEndpoint{{ID: "dev-c1", URL: srv.URL}}}}
	got := s.collectorStatuses(context.Background())

	if _, wrong := got["actually-c2"]; wrong {
		t.Error("keyed on the REPORTED id; two daemons reporting one name would then " +
			"collapse two configured entries into a single entry")
	}
	st, ok := got["dev-c1"]
	if !ok {
		t.Fatalf("no entry under the configured id; got %v", got)
	}
	if st.ReportedID != "actually-c2" {
		t.Errorf("ReportedID = %q, want the daemon's own id %q so the screen can name both sides",
			st.ReportedID, "actually-c2")
	}
	if st.Report == nil {
		t.Error("Report = nil; the endpoint DID answer, just under a different id")
	}
	if st.IDMismatch != "" {
		t.Errorf("IDMismatch = %q, want \"\": this layer never decides which row an "+
			"answer belongs on -- handleCollectors sets that", st.IDMismatch)
	}
}

// TestCollectorStatusesKeepsBothEntriesWhenTwoDaemonsReportTheSameID is the
// collision the configured-id key exists to make impossible. Two config
// entries, two live endpoints, and both daemons answer with the SAME
// collector_id -- an ordinary deployment mistake (the bare headless Service
// name pasted twice resolves round-robin to one pod, and both entries then
// poll it). Keyed on the reported id, one of these two entries would simply
// not be in the map.
func TestCollectorStatusesKeepsBothEntriesWhenTwoDaemonsReportTheSameID(t *testing.T) {
	sameID := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(status.Report{CollectorID: "one-pod", SessionsActive: 7})
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	s := &Server{cfg: Config{Collectors: []CollectorEndpoint{
		{ID: "dev-c1", URL: sameID()},
		{ID: "dev-c2", URL: sameID()},
	}}}
	got := s.collectorStatuses(context.Background())

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 -- one entry per CONFIGURED collector, whatever the "+
			"daemons behind them call themselves; got %v", len(got), got)
	}
	for _, id := range []string{"dev-c1", "dev-c2"} {
		st, ok := got[id]
		if !ok {
			t.Fatalf("no entry for configured id %q; got %v", id, got)
		}
		if st.Report == nil {
			t.Errorf("%s: Report = nil; its own endpoint answered", id)
		}
		if st.ReportedID != "one-pod" {
			t.Errorf("%s: ReportedID = %q, want %q", id, st.ReportedID, "one-pod")
		}
	}
}

// TestCollectorStatusesDoesNotLetOneSlowEndpointHoldTheRest: N endpoints, one
// request. A collector wedged mid-response must not turn the whole screen into
// a spinner, which is the failure mode of a sequential loop with a per-request
// timeout.
func TestCollectorStatusesDoesNotLetOneSlowEndpointHoldTheRest(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status.Report{CollectorID: "fast", SessionsActive: 4})
	}))
	defer fast.Close()
	// Deferred LAST so it runs FIRST during unwind, before either Close():
	// httptest.Server.Close documents that it "blocks until all outstanding
	// requests on this server have completed" and its source (server.go)
	// deliberately refuses to force-close a StateActive connection ("we wait
	// for outstanding requests, so we don't close things in StateActive").
	// slow's handler cannot return until block closes, so if close(block) ran
	// after slow.Close() in the defer stack -- as a top-to-bottom reading
	// would suggest -- slow.Close() would wait forever for a handler that is
	// itself waiting for this defer to run. Confirmed by running it that way
	// first: it hung past a 20s test timeout.
	defer close(block)

	s := &Server{cfg: Config{
		Collectors:        []CollectorEndpoint{{ID: "slow", URL: slow.URL}, {ID: "fast", URL: fast.URL}},
		CollectorsTimeout: 150 * time.Millisecond,
	}}
	start := time.Now()
	got := s.collectorStatuses(context.Background())
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("took %v; the two endpoints were queried in series, not concurrently", elapsed)
	}
	if got["fast"].Report == nil || got["fast"].Report.SessionsActive != 4 {
		t.Errorf("the healthy endpoint's answer was lost: %+v", got["fast"])
	}
	if got["slow"].Err == "" {
		t.Error("the wedged endpoint produced no error; its card would show blanks with no reason")
	}
}
