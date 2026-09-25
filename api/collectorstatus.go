// collectorstatus.go is the fan-out that turns Config.Collectors into a
// picture of every collector's own process facts.
//
// It decodes status.Report by field name and never imports collector/ --
// see status's own doc comment, and api/deps_test.go, for why that is a
// requirement rather than a preference: promauto registers on the default
// registry at package init, so linking collector/ into this binary would
// make vantage-api's own /metrics claim it holds BMP sessions.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/jp2195/vantage/status"
)

// maxStatusBodyBytes bounds how much of a collector's /status response this
// daemon will ever decode. A real status.Report is a few hundred bytes of
// JSON; this is generous headroom above that, not a sized-to-fit limit --
// the same reasoning collector/admin.go's maxArmBodyBytes gives for the same
// shape of problem on the other side of a connection. Without a cap, a
// misbehaving or hostile endpoint could send an unbounded body and make the
// decoder buffer an unbounded amount of memory before decoding ever gets a
// chance to fail.
const maxStatusBodyBytes = 4 << 10 // 4 KiB

// statusHTTPClient is the one http.Client every collectorStatuses call
// reuses, rather than constructing one per collector or per call. It carries
// no Timeout of its own -- the deadline that bounds every request it makes
// comes from the context collectorStatuses derives from cfg.CollectorsTimeout,
// over the WHOLE fan-out, not layered again per request on top of it.
var statusHTTPClient = &http.Client{}

// CollectorStatus is one configured collector's answer to /status, or the
// reason it has none.
//
// Report is nil, not a zero status.Report, when the daemon did not answer.
// Config.Collectors's doc comment is why that distinction has to survive
// this far: a collector with no configured endpoint, or one that is down,
// has NO uptime and NO message count, and a zero value here would render
// indistinguishable from a collector that truly counted zero of everything.
//
// Err carries why there is no Report: unreachable, timed out, a non-200
// response, a body that did not decode, or a body that decoded but named no
// collector_id at all -- see pollCollector for why that last one is refused
// here rather than reported as a collector that counted zero of everything.
//
// ReportedID and IDMismatch are the two halves of one disagreement, written
// by two different places on purpose:
//
//   - ReportedID is the id the daemon named as ITS OWN, and pollCollector
//     sets it only when the daemon answered AND that id disagrees with the
//     id this entry was configured under. "" both when the daemon stayed
//     silent (silence states nothing about identity) and when the two
//     agreed -- never because the daemon named an EMPTY id, which
//     pollCollector refuses before this field is ever reached, so callers
//     may read "" as "no disagreement" and nothing else. The disagreement
//     is never resolved to one side here.
//   - IDMismatch is the CONFIGURED id, and handleCollectors -- never
//     pollCollector -- sets it, on the extra row it synthesizes under a
//     reported id no config entry names, so that row can say whose endpoint
//     answered under it. See that function for why the two ids get one row
//     each rather than one row between them.
type CollectorStatus struct {
	Endpoint   string
	Report     *status.Report
	Err        string
	ReportedID string
	IDMismatch string
}

// collectorStatuses polls every configured collector's /status concurrently
// and returns one CollectorStatus per CONFIGURED entry, keyed on that
// entry's own CollectorEndpoint.ID.
//
// The configured id is the key, rather than whichever id turns out to be
// authoritative for that entry, because it is the only one this daemon can
// guarantee is unique: validate() (api/config.go) rejects a duplicate
// configured id, and nothing anywhere constrains what two daemons report
// about themselves. Keying on the REPORTED id -- which this did until two
// polls were found able to land on the same key -- collapses two configured
// entries into one whenever their daemons agree on a name, silently, with
// whichever poll finished last winning: the loser then has no entry under
// any name and serializes byte-identically to a collector with no endpoint
// configured at all, while an endpoint IS configured for it. Both ways that
// happens are ordinary deployment mistakes rather than exotic ones -- two
// entries pointed at the same pod, or the bare headless Service name pasted
// twice (api/config.go's Collectors doc comment names that one by name) --
// and the result flips between polls. Which id is authoritative for a ROW
// is a question about the response, and handleCollectors answers it there,
// where both ids are still in hand.
//
// The whole fan-out shares a single context.WithTimeout deadline
// (s.cfg.CollectorsTimeout) rather than each request carrying its own, so
// one wedged collector can never cost the caller more than that ceiling no
// matter how many collectors are configured, and every request is issued
// concurrently over one reused http.Client so a slow collector cannot delay
// the others' answers.
func (s *Server) collectorStatuses(ctx context.Context) map[string]CollectorStatus {
	// A zero CollectorsTimeout means "not configured," never "already
	// expired" -- LoadConfig defaults it before a real Server ever sees one,
	// but a *Server built by a test literal (as this package's own tests
	// do) can reach here with the zero value, and context.WithTimeout(ctx,
	// 0) would produce a deadline in the past, failing every request
	// instantly regardless of how reachable the endpoint actually is.
	timeout := s.cfg.CollectorsTimeout
	if timeout <= 0 {
		timeout = defaultCollectorsTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := make(map[string]CollectorStatus, len(s.cfg.Collectors))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, ce := range s.cfg.Collectors {
		wg.Add(1)
		go func(ce CollectorEndpoint) {
			defer wg.Done()
			st := pollCollector(ctx, ce)
			mu.Lock()
			result[ce.ID] = st
			mu.Unlock()
		}(ce)
	}
	wg.Wait()
	return result
}

// pollCollector fetches one configured collector's /status.
//
// It never decides which id the answer belongs to: the caller keys the
// result on ce.ID, and a decoded Report that names a DIFFERENT id records
// that id in CollectorStatus.ReportedID rather than resolving the
// disagreement here. See that field's own doc comment.
func pollCollector(ctx context.Context, ce CollectorEndpoint) CollectorStatus {
	st := CollectorStatus{Endpoint: ce.URL}

	// TrimSuffix rather than a bare concatenation: config.go's validate()
	// accepts a url with a trailing slash (it only checks scheme), and
	// without this a configured "http://collector:9469/" would poll
	// ".../status" and get a 404 that looks like the collector, not the
	// config, is broken.
	statusURL := strings.TrimSuffix(ce.URL, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	resp, err := statusHTTPClient.Do(req)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		st.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return st
	}

	var rep status.Report
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBodyBytes))
	if err := dec.Decode(&rep); err != nil {
		st.Err = fmt.Sprintf("decode: %v", err)
		return st
	}

	// An answer carrying no collector_id is not an answer this endpoint
	// can use, and it is refused HERE rather than folded into either of
	// the two outcomes below. encoding/json leaves an absent field at its
	// zero value, so ANY 200 whose body is a JSON object decodes into a
	// status.Report -- a url pointed at another service's health endpoint,
	// or at a different daemon entirely, which is the same class of
	// deployment mistake the id check below exists to catch. Read as a
	// report, that zero value renders as a HEALTHY card with a full set of
	// zero process tiles and no reason given: the rule that zero is not
	// absence, inverted, on the one card an operator opened to find exactly
	// this.
	//
	// Refusing it also keeps "" out of ReportedID, whose whole meaning is
	// "there is a disagreement, and here is the other side of it".
	// handleCollectors branches on that field being empty, so an empty
	// reported id reaching it would be indistinguishable from agreement.
	// CollectorID is the join key this entire endpoint is built on (see
	// status.Report's own doc comment): an endpoint that does not state one
	// has not identified itself, and no process fact it sent can be
	// attributed to any id at all.
	if rep.CollectorID == "" {
		st.Err = "answered 200 with no collector_id; " +
			"that is not a vantage-collector /status response"
		return st
	}

	st.Report = &rep
	if rep.CollectorID != ce.ID {
		st.ReportedID = rep.CollectorID
	}
	return st
}
