// status.go serves what only this process knows: how long it has been up, how
// many BMP sessions it holds, how much it has read and published, and what it
// failed to publish. See status.Report for why those are process facts and the
// peer/router counts on the same card are not.
package collector

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/jp2195/vantage/status"
)

// Report renders g's current gather as a status.Report.
//
// g is the same Gatherer promhttp serves /metrics from, which is the design:
// two renderings of one gather cannot disagree, and no second set of counters
// exists to drift from the first.
func Report(g prometheus.Gatherer, id string, startedAt, now time.Time) (status.Report, error) {
	mfs, err := g.Gather()
	if err != nil {
		return status.Report{}, fmt.Errorf("gather: %w", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		byName[mf.GetName()] = mf
	}

	// required: registered unconditionally at package init, so absent means
	// renamed. See TestReportFailsOnAMissingGaugeRatherThanReportingZero.
	required := func(name string) (float64, error) {
		mf, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("metric %s absent from the registry serving /metrics; "+
				"it is registered unconditionally, so this is a rename, not a zero", name)
		}
		return sumFamily(mf), nil
	}
	// optional: a *Vec exports nothing until a label set is touched, so absent
	// legitimately means "this has not happened yet".
	optional := func(name string) float64 {
		mf, ok := byName[name]
		if !ok {
			return 0
		}
		return sumFamily(mf)
	}

	sessions, err := required("vantage_collector_sessions_active")
	if err != nil {
		return status.Report{}, err
	}
	pubErr, err := required("vantage_collector_publish_errors_total")
	if err != nil {
		return status.Report{}, err
	}
	pubRej, err := required("vantage_collector_publish_rejects_total")
	if err != nil {
		return status.Report{}, err
	}

	return status.Report{
		CollectorID:          id,
		StartedAt:            startedAt,
		ObservedAt:           now,
		SessionsActive:       int64(sessions),
		BMPMessagesTotal:     uint64(optional("vantage_collector_bmp_messages_total")),
		EventsPublishedTotal: uint64(optional("vantage_collector_events_published_total")),
		PublishErrorsTotal:   uint64(pubErr),
		PublishRejectsTotal:  uint64(pubRej),
	}, nil
}

// sumFamily adds every series in a family. For the plain gauges and counters
// there is exactly one; for the *Vecs this is the sum across the label
// dimension, which is what the card wants -- the breakdown stays on /metrics.
func sumFamily(mf *dto.MetricFamily) float64 {
	var total float64
	for _, m := range mf.GetMetric() {
		switch {
		case m.GetCounter() != nil:
			total += m.GetCounter().GetValue()
		case m.GetGauge() != nil:
			total += m.GetGauge().GetValue()
		}
	}
	return total
}

// StatusHandler serves Report as JSON. It is mounted on the METRICS listener,
// not the admin one: admin_listen defaults to loopback because arming a mirror
// is a real capability, and that default must not be relaxed just so the API
// can read a status. This endpoint carries exactly what /metrics carries.
func StatusHandler(id string, startedAt time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rep, err := Report(prometheus.DefaultGatherer, id, startedAt, time.Now().UTC())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rep)
	})
}
