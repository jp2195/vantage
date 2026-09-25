package collector

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestReportReadsTheRegistryThatServesMetrics is the whole point of building
// Report on a Gatherer rather than on private counters of its own: /status and
// /metrics are two renderings of ONE gather, so they cannot report different
// numbers, and a metric renamed in metrics.go breaks this test instead of
// silently zeroing a card.
//
// It touches the REAL package metrics on the REAL default registry. A version
// of this test that built its own registry with its own metrics would pass
// with every name in Report misspelled.
func TestReportReadsTheRegistryThatServesMetrics(t *testing.T) {
	start := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	now := start.Add(90 * time.Minute)

	// metricPublishErrors and metricBMPMessages are package-global counters
	// on the default registry: another test in this package may already
	// have added to them, and a later one will. A baseline Report taken
	// before this test's own mutations, with the delta asserted below, is
	// what keeps this test correct under that sharing instead of passing
	// today and breaking the day some other collector/ test starts touching
	// the same counters. metricSessions is a gauge, not a counter -- Set
	// below overwrites it outright, so its absolute value needs no
	// baseline.
	before, err := Report(prometheus.DefaultGatherer, "dev-c1", start, now)
	if err != nil {
		t.Fatalf("Report (baseline): %v", err)
	}

	metricSessions.Set(3)
	metricPublishErrors.Add(2)
	metricBMPMessages.WithLabelValues("route_monitoring").Add(7)
	metricBMPMessages.WithLabelValues("stats_report").Add(5)

	got, err := Report(prometheus.DefaultGatherer, "dev-c1", start, now)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got.CollectorID != "dev-c1" {
		t.Errorf("CollectorID = %q, want dev-c1", got.CollectorID)
	}
	if got.SessionsActive != 3 {
		t.Errorf("SessionsActive = %d, want 3 (the gauge this test set)", got.SessionsActive)
	}
	if delta := got.PublishErrorsTotal - before.PublishErrorsTotal; delta != 2 {
		t.Errorf("PublishErrorsTotal delta = %d, want 2", delta)
	}
	// 7 + 5: the Vec is summed across its label dimension, not sampled from
	// whichever series the gather happened to order first.
	if delta := got.BMPMessagesTotal - before.BMPMessagesTotal; delta != 12 {
		t.Errorf("BMPMessagesTotal delta = %d, want 12 (summed across both types)", delta)
	}
	if !got.ObservedAt.Equal(now) || !got.StartedAt.Equal(start) {
		t.Errorf("clocks = %v/%v, want %v/%v", got.StartedAt, got.ObservedAt, start, now)
	}
}

// TestReportFailsOnAMissingGaugeRatherThanReportingZero: an absent family and a
// zero family are the same number and opposite facts. The gauges and plain
// counters are registered unconditionally at package init, so their absence can
// only mean a rename or a wrong registry -- and reporting that as a calm zero is
// how a renamed metric ships a card that reads HEALTHY forever.
//
// The *Vec families are exempt and must stay exempt: a CounterVec exports
// nothing until a label set is touched, so a collector that has read no BMP
// messages yet legitimately has no vantage_collector_bmp_messages_total at all.
func TestReportFailsOnAMissingGaugeRatherThanReportingZero(t *testing.T) {
	empty := prometheus.NewRegistry()
	if _, err := Report(empty, "dev-c1", time.Now(), time.Now()); err == nil {
		t.Fatal("Report on a registry with none of our metrics returned no error; " +
			"a renamed metric would then read as a healthy zero")
	}
}
