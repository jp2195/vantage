package status

import (
	"encoding/json"
	"testing"
	"time"
)

// TestReportJSONFieldNames locks the wire contract this package exists to
// hold still. api/ decodes a collector's /status response by field name,
// without importing this package's producer (collector/), so a json tag
// renamed here breaks silently on the reading side unless something
// asserts on the actual wire bytes rather than on Go field names that
// round-trip regardless of what their tags say.
func TestReportJSONFieldNames(t *testing.T) {
	started := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	observed := started.Add(90 * time.Minute)
	rep := Report{
		CollectorID:          "dev-c1",
		StartedAt:            started,
		ObservedAt:           observed,
		SessionsActive:       3,
		BMPMessagesTotal:     12,
		EventsPublishedTotal: 9,
		PublishErrorsTotal:   2,
		PublishRejectsTotal:  1,
	}

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}

	wantKeys := map[string]any{
		"collector_id":           "dev-c1",
		"sessions_active":        float64(3),
		"bmp_messages_total":     float64(12),
		"events_published_total": float64(9),
		"publish_errors_total":   float64(2),
		"publish_rejects_total":  float64(1),
	}
	for key, want := range wantKeys {
		got, ok := wire[key]
		if !ok {
			t.Errorf("wire JSON missing key %q (raw: %s)", key, raw)
			continue
		}
		if got != want {
			t.Errorf("wire[%q] = %v, want %v", key, got, want)
		}
	}
	if _, ok := wire["started_at"]; !ok {
		t.Errorf("wire JSON missing key %q (raw: %s)", "started_at", raw)
	}
	if _, ok := wire["observed_at"]; !ok {
		t.Errorf("wire JSON missing key %q (raw: %s)", "observed_at", raw)
	}

	// Round-trip: a reader on the other side of the wire (api/, without
	// importing this package's producer) must get back exactly what was
	// sent.
	var got Report
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal into Report: %v", err)
	}
	if got.CollectorID != rep.CollectorID {
		t.Errorf("CollectorID = %q, want %q", got.CollectorID, rep.CollectorID)
	}
	if !got.StartedAt.Equal(rep.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, rep.StartedAt)
	}
	if !got.ObservedAt.Equal(rep.ObservedAt) {
		t.Errorf("ObservedAt = %v, want %v", got.ObservedAt, rep.ObservedAt)
	}
	if got.SessionsActive != rep.SessionsActive {
		t.Errorf("SessionsActive = %d, want %d", got.SessionsActive, rep.SessionsActive)
	}
	if got.BMPMessagesTotal != rep.BMPMessagesTotal {
		t.Errorf("BMPMessagesTotal = %d, want %d", got.BMPMessagesTotal, rep.BMPMessagesTotal)
	}
	if got.EventsPublishedTotal != rep.EventsPublishedTotal {
		t.Errorf("EventsPublishedTotal = %d, want %d", got.EventsPublishedTotal, rep.EventsPublishedTotal)
	}
	if got.PublishErrorsTotal != rep.PublishErrorsTotal {
		t.Errorf("PublishErrorsTotal = %d, want %d", got.PublishErrorsTotal, rep.PublishErrorsTotal)
	}
	if got.PublishRejectsTotal != rep.PublishRejectsTotal {
		t.Errorf("PublishRejectsTotal = %d, want %d", got.PublishRejectsTotal, rep.PublishRejectsTotal)
	}
}
