// GET /v1/collectors is where the archive's own view of each collector and
// that collector's own view of itself meet, on one row per id -- the two
// sides have no join key except the id a
// collector states about itself at /status: the archive stores
// collector_id, but /metrics carries no such label by design, and
// Prometheus labels scrape targets job=/instance=, neither of which is a
// collector's identity.
//
// The rows are the UNION of ids in query.Collectors' result and ids
// Config.Collectors names. Either side alone is a different wrong answer:
// config alone cannot see a collector that was decommissioned but whose
// data is still being read, and the archive alone cannot see a collector
// that is configured and completely down. See WireCollector's own doc
// comment for the four combinations of archive/status presence a row can
// land in.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jp2195/vantage/query"
)

// collectorActivityWindow is how far back GET /v1/collectors' own
// sparkline looks -- the 30 minutes chosen for this signal (see
// query.CollectorActivity's own doc comment for more). Named once, here,
// so the value passed to CollectorActivity and the value echoed in
// meta.activity_window cannot drift apart from each other.
const collectorActivityWindow = 30 * time.Minute

// retentionForMeta is meta.retention_days for RetentionDays' answer: the
// days, or nil when there is no number to report. Retention is context on
// the Collectors screen, not its answer, so a failed read omits the field
// rather than failing the request. A TTL that is not a whole number of days
// is an expected state and is omitted silently; any other error is logged.
func (s *Server) retentionForMeta(days int, err error) *int {
	switch {
	case errors.Is(err, query.ErrRetentionNotInDays):
		return nil
	case err != nil:
		s.logger.Warn("collectors: read history retention; omitting meta.retention_days", "err", err)
		return nil
	}
	return &days
}

// handleCollectors serves GET /v1/collectors.
//
// The three sources run concurrently, over one errgroup, the same idiom
// handleRoutes uses: query.Collectors and query.CollectorActivity are both
// full scans by their own doc comments' account, and collectorStatuses'
// whole budget is s.cfg.CollectorsTimeout -- a network timeout, not a scan.
// Running the three one after another would make this screen wait for the
// SUM of a scan and a network round trip instead of the MAX of the two, and
// this is a screen someone is actively waiting on.
func (s *Server) handleCollectors(w http.ResponseWriter, r *http.Request) {
	var (
		summaries []query.CollectorSummary
		activity  map[string][]query.CollectorActivity
		statuses  map[string]CollectorStatus
		// nil when the history TTL cannot be read as a whole number of
		// days; see retentionForMeta.
		retentionDays *int
	)
	// now is captured once, here, rather than read again inside
	// wireCollectorActivitySeries per collector -- so that every quiet
	// collector in this one response is zero-filled against the identical
	// minute grid, and so that grid sits as close as this handler can put
	// it to the instant query.CollectorActivity computes its own (see that
	// method's doc comment for the formula this mirrors for the missing-key
	// case).
	now := time.Now()
	g, ctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) {
		summaries, err = s.q.Collectors(ctx)
		return err
	})
	g.Go(func() (err error) {
		activity, err = s.q.CollectorActivity(ctx, collectorActivityWindow)
		return err
	})
	g.Go(func() error {
		statuses = s.collectorStatuses(ctx)
		return nil
	})
	g.Go(func() error {
		days, err := s.q.RetentionDays(ctx)
		retentionDays = s.retentionForMeta(days, err)
		return nil
	})
	if err := g.Wait(); err != nil {
		s.fail(w, r, err)
		return
	}

	archiveByID := make(map[string]query.CollectorSummary, len(summaries))
	for _, cs := range summaries {
		archiveByID[cs.Collector] = cs
	}

	// effectiveStatuses is the map from a ROW's id to the process facts that
	// row gets, which is not the same map collectorStatuses returned: that
	// one is keyed on the CONFIGURED id, one entry per config entry (see its
	// own doc comment for why that key and no other), and a daemon answering
	// under a name nobody configured still earns a row of its own.
	//
	// Pass one: every configured id, unconditionally, so no combination of
	// daemon answers can drop one. Dropping a configured id from the
	// response is exactly the failure Config.Collectors exists to prevent
	// (see this file's own doc comment), and it would not read as a
	// dropped row -- it would fall through newWireCollector's "no endpoint
	// configured" branch and serialize byte-identically to a collector
	// nobody ever set up, stating a config fact as an absence.
	//
	// An entry whose daemon answered as SOMEONE ELSE does NOT get that
	// daemon's process facts on its own row: those facts belong to the id
	// the daemon claimed, not to the id an operator typed beside the url.
	// Its row carries the configured endpoint and a reason instead, so it
	// reads as configured but not itself -- reachable false, with an
	// explanation -- which is a third thing, distinct from both "answering"
	// and "nothing configured".
	effectiveStatuses := make(map[string]CollectorStatus, 2*len(s.cfg.Collectors))
	for _, ce := range s.cfg.Collectors {
		st := statuses[ce.ID]
		if st.ReportedID == "" {
			effectiveStatuses[ce.ID] = st
			continue
		}
		effectiveStatuses[ce.ID] = CollectorStatus{
			Endpoint: st.Endpoint,
			Err: fmt.Sprintf(
				"configured as %q; the collector answering at this endpoint reports itself as %q",
				ce.ID, st.ReportedID),
		}
	}
	// Pass two: the daemon's own answer, under the id it actually claimed,
	// for every reported id that does not already have a row. Already
	// having one covers two different situations and the same rule settles
	// both -- the first row written under an id keeps it:
	//
	//   - The reported id is itself configured. Its own entry's own poll is
	//     the authority on its own row; a second endpoint claiming that
	//     name is that second entry's problem, and pass one already says so
	//     on that entry's row.
	//   - An earlier config entry already reported it. Two entries can name
	//     the same unconfigured id, and then the row has to attribute
	//     itself to one of them.
	//
	// Iterated over s.cfg.Collectors rather than over statuses so that
	// "first" means first in the config file, on every request, rather than
	// whatever Go's map order happened to be on this one.
	for _, ce := range s.cfg.Collectors {
		st := statuses[ce.ID]
		if st.ReportedID == "" {
			continue
		}
		if _, taken := effectiveStatuses[st.ReportedID]; taken {
			continue
		}
		reported := st
		reported.ReportedID = ""
		reported.IDMismatch = ce.ID
		effectiveStatuses[st.ReportedID] = reported
	}

	// The union has THREE sources, not two: archive ids, ids
	// Config.Collectors names directly, and effectiveStatuses' own keys --
	// which, beyond every configured id, also carries whatever id a
	// mismatched daemon actually reported (see
	// TestCollectorsSurfacesAnIDMismatchRatherThanResolvingItSilently's
	// "actually-someone-else"), itself never a configured id.
	// Config.Collectors is read here directly even though pass one above
	// already put every configured id in effectiveStatuses: that redundancy
	// is the point rather than an oversight, because a configured id
	// missing from this response is the one failure this endpoint cannot
	// signal -- it renders as a different, plausible state instead of as
	// nothing.
	ids := make([]string, 0, len(summaries)+len(s.cfg.Collectors)+len(effectiveStatuses))
	seen := make(map[string]bool, cap(ids))
	addID := func(id string) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, cs := range summaries {
		addID(cs.Collector)
	}
	for _, ce := range s.cfg.Collectors {
		addID(ce.ID)
	}
	for id := range effectiveStatuses {
		addID(id)
	}
	slices.Sort(ids)

	out := make([]WireCollector, 0, len(ids))
	for _, id := range ids {
		summary, inArchive := archiveByID[id]
		st, hasStatus := effectiveStatuses[id]
		out = append(out, newWireCollector(id, summary, inArchive, st, hasStatus,
			wireCollectorActivitySeries(activity[id], collectorActivityWindow, now)))
	}

	window := collectorActivityWindow.String()
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: out,
		Meta: Meta{
			ActivityWindow: &window, RetentionDays: retentionDays,
			Warnings: StaleWarning(anyCollectorStale(summaries), s.q.StaleAfter()),
		},
	})
}

// newWireCollector builds one row of GET /v1/collectors' data array.
//
// inArchive and hasStatus are separate booleans rather than nil checks on
// summary and st because summary's own zero value (query.CollectorSummary{})
// is indistinguishable from a real archive entry once its fields are all
// zero -- and because query.Collectors never actually returns one of those
// (every entry it returns has at least one router), a zero-valued summary
// reaching here would be silently wrong rather than caught. The two flags
// are what actually answer "is this collector known to the archive" and
// "did this daemon answer", which is the question WireCollector's own doc
// comment says this response has to keep distinguishable from "reads zero".
func newWireCollector(
	id string,
	summary query.CollectorSummary, inArchive bool,
	st CollectorStatus, hasStatus bool,
	activity []WireCollectorActivity,
) WireCollector {
	wc := WireCollector{Collector: id, Activity: activity}
	if inArchive {
		wc.Archive = &WireCollectorArchive{
			Routers:       mapRows(summary.Routers, NewWireCollectorRouter),
			PeersUp:       summary.PeersUp,
			PeersDown:     summary.PeersDown,
			PeersViewLost: summary.PeersViewLost,
			PeersStale:    summary.PeersStale,
			LastRowAt:     stamp(summary.LastRowAt),
			LastBeatAt:    stampOrNull(summary.LastBeatAt),
			StartedAt:     stampOrNull(summary.StartedAt),
		}
	}
	if !hasStatus {
		// No CollectorStatus entry at all means no Config.Collectors entry
		// named this id and no daemon reported it either -- handleCollectors
		// puts every configured id in effectiveStatuses unconditionally, and
		// every reported id beside them. Reachable stays nil: there is
		// nothing configured to be reachable or not.
		return wc
	}
	wc.Endpoint = st.Endpoint
	wc.Error = st.Err
	wc.IDMismatch = st.IDMismatch
	reachable := st.Report != nil
	wc.Reachable = &reachable
	if st.Report != nil {
		wireStatus := NewWireCollectorStatus(*st.Report)
		wc.Status = &wireStatus
	}
	return wc
}

// wireCollectorActivitySeries turns one collector's window slice from
// (*query.Q).CollectorActivity into its wire form -- or, when series is
// empty because that collector's own key is simply absent from the map
// CollectorActivity returned, a same-length series covering the identical
// window with every Rows == 0.
//
// series being empty here is never ambiguous with "the query returned a
// bucket with zero rows in it": CollectorActivity's own doc comment is
// explicit that a collector with nothing archived anywhere in the window
// has NO key in the map at all, so the caller (handleCollectors) passes
// activity[id], which is nil -- not a real, shorter slice -- for exactly
// that collector. window and now reproduce CollectorActivity's own bucket
// formula (see that method's doc comment) so a synthesized zero series
// lines up on the same minute grid a real one would have, including the
// newest bucket being the CURRENT, still-accumulating minute rather than
// the last fully-elapsed one.
func wireCollectorActivitySeries(
	series []query.CollectorActivity, window time.Duration, now time.Time,
) []WireCollectorActivity {
	if len(series) > 0 {
		return mapRows(series, NewWireCollectorActivity)
	}
	buckets := int64(window / time.Minute)
	end := now.UTC().Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-time.Duration(buckets) * time.Minute)
	out := make([]WireCollectorActivity, 0, buckets)
	for t := start; t.Before(end); t = t.Add(time.Minute) {
		out = append(out, WireCollectorActivity{Minute: t})
	}
	return out
}
