// Package status is the one shape vantage-collector reports about itself and
// vantage-api reads back. It is a leaf on purpose: it imports nothing but
// time, so that api/ can decode a collector's answer without importing
// collector/ and thereby registering that package's promauto metrics into
// the API binary. That leak is real and already visible -- writer:9472 serves
// vantage_collector_sessions_active 0 and api:9474 serves
// vantage_sink_rows_inserted_total 0, because promauto registers on the
// default registry at package init -- and this package exists so a future
// collector-health dashboard reading these metrics does not make the leak
// worse.
package status

import "time"

// Report is what a collector knows about itself that nothing else can know.
//
// Everything here is a PROCESS fact. Peer counts, router inventories and
// route totals are archive facts and are deliberately absent: the API already
// has them, they survive a collector restart, and asking the process for them
// would produce a second answer that disagrees with the first during any
// window where the two disagree.
//
// The counters are cumulative since StartedAt, never rates. A rate needs two
// samples and this is one; whoever wants messages-per-second subtracts two
// Reports and divides by the gap between their ObservedAt. That is why
// ObservedAt is on the wire at all -- the reader's own clock is a different
// clock, which is the same distinction ts_router and ts_collector exist to keep.
type Report struct {
	// CollectorID is the id this process runs under -- the same string the
	// archive stores in collector_id, stated by the process itself. It is the
	// join key between "which collectors exist" and "how healthy is each",
	// and before this field there was none: the metrics carry no collector_id
	// label (collector/metrics.go bounds label cardinality by construction)
	// and Prometheus labels targets job=/instance=.
	CollectorID string `json:"collector_id"`

	// StartedAt is process start, so uptime is the reader's arithmetic rather
	// than a number that was already stale when it was serialized.
	StartedAt time.Time `json:"started_at"`

	// ObservedAt is when the counters below were gathered, on THIS process's
	// clock. Two Reports and their two ObservedAt values are what a rate is
	// made of.
	ObservedAt time.Time `json:"observed_at"`

	// SessionsActive is open BMP sessions right now: a gauge, not a total.
	SessionsActive int64 `json:"sessions_active"`

	// BMPMessagesTotal and EventsPublishedTotal are summed across their label
	// dimensions (BMP message type, envelope payload kind). The per-label
	// breakdown stays on /metrics, which is where a breakdown belongs.
	BMPMessagesTotal     uint64 `json:"bmp_messages_total"`
	EventsPublishedTotal uint64 `json:"events_published_total"`

	// PublishErrorsTotal is local backpressure (the client shedding when NATS
	// is unreachable); PublishRejectsTotal is the JetStream server refusing a
	// publish we believed we had sent. They stay separate here for the reason
	// collector/metrics.go keeps them separate: they are different failures
	// with different fixes, and a card that adds them reports neither.
	PublishErrorsTotal  uint64 `json:"publish_errors_total"`
	PublishRejectsTotal uint64 `json:"publish_rejects_total"`
}
