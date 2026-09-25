package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jp2195/vantage/query"
)

// churnDefaultBucket is the width an omitted bucket= answers at.
//
// Five minutes over the default one-hour window is twelve bars -- a shape
// that reads as a chart rather than a stripe -- and over the 24h clamp
// ceiling it is 288, still well inside what one answer carries. A default
// that had to be widened for wide windows would mean two callers asking the
// same question getting bars of different meaning without either saying so.
const churnDefaultBucket = 5 * time.Minute

// bucket parses ?bucket= into the width of one bar.
//
// A duration only -- "5m", "1h" -- never an RFC 3339 instant, which since=
// also accepts: a width is not a point in time, and accepting one would make
// `bucket=2026-09-17T00:00:00Z` mean something.
func (p *params) bucket() time.Duration {
	if p.err != nil {
		return 0
	}
	raw := p.v.Get("bucket")
	if raw == "" {
		return churnDefaultBucket
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.fail(`bucket= is not a duration ("30s", "5m", "1h")`)
		return 0
	}
	if d < time.Second {
		// Zero and negative both land here, and both are the same mistake:
		// a bar has a width. The message names the floor rather than only
		// refusing, because "0" is what a caller sends when they mean "do
		// not bucket", and that is not an option this endpoint has.
		p.fail("bucket= must be at least 1s; it is the width of one bar")
		return 0
	}
	return d
}

// handleCollectionChurn serves GET /v1/collection/churn.
//
// The fifth collection signal, and the first with a time axis: what the
// archive received over the window, bucketed, split into re-advertisements,
// withdrawals and session dumps. The classification is route-churn.json's
// own -- see query.ChurnBuckets and dumpClassifiedSQL, which the dashboards,
// /v1/collection/dumps and this path all read from one expression.
//
// It takes the same window clamp as the other four -- checkUnscopedWindow,
// the shared function -- for the same measured reason: unscoped, this reads
// three route tables across the window, and router= narrows the WHERE without
// turning the read into a seek. The HINT is its own (unscopedChurnHint),
// because this is the one signal in the family that takes peer=, and the
// shared hint says in so many words that none of them does.
//
// peer= IS a parameter here, unlike on the other four. The route tables carry
// peer_ip, and a peer's own churn is the chart Peer detail draws; the hint
// the clamp prints still says router=, because neither parameter buys an
// exemption from the window -- only a narrower read.
func (s *Server) handleCollectionChurn(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	now := time.Now()
	f := query.ChurnFilter{
		Router:    p.addr("router"),
		Peer:      p.addr("peer"),
		Since:     p.since(now),
		Collector: p.str("collector"),
		Bucket:    p.bucket(),
	}
	if p.err == nil {
		if err := s.checkUnscopedWindow(f.Since, now, unscopedChurnHint); err != nil {
			p.err = err
		}
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	rows, err := s.q.ChurnBuckets(r.Context(), f)
	if err != nil {
		// ErrBadFilter here is the bars-per-answer guard, which is a
		// property of the REQUEST (window over width) rather than a failure:
		// it belongs to the caller as a 400 naming what to change, not to
		// the operator as a 500.
		if errors.Is(err, query.ErrBadFilter) {
			s.badParam(w, err)
			return
		}
		s.fail(w, r, err)
		return
	}

	bucket := f.Bucket.String()
	// The window this answer covers, from the SAME `now` the unscoped-window
	// check above used, so the bounds a client draws with are the bounds the
	// request was validated against rather than a second reading of the
	// clock that could differ by a poll.
	from := f.Since.UTC()
	to := now.UTC()
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireChurnBucket),
		// The width every bar in this answer was computed at, and the window
		// they sit in. A chart labeled "5 minutes" drawn from 1-minute bars
		// is the same defect as an unlabeled window: the picture is right
		// and the claim beside it is not. The bounds are the other half of
		// that -- without them a client places collector timestamps against
		// its own clock. See Meta.ChurnFrom.
		Meta: Meta{Warnings: Warnings{}, ChurnBucket: &bucket, ChurnFrom: &from, ChurnTo: &to},
	})
}

// activityBucketCount is how many bars one peer's sparkline carries at most.
//
// A COUNT rather than a width, because a sparkline's width is fixed in
// pixels and its bar count is what has to be bounded -- unlike
// /v1/collection/churn, where the caller is drawing a real chart and names
// the width it wants. Twenty-four is small enough that a fleet of peers
// costs a bounded number of rows and large enough that a burst is visibly a
// burst rather than one tall bar.
const activityBucketCount = 24

// activityBucket cuts a window into activityBucketCount bars, never finer
// than a second.
//
// A second is the floor the query layer itself enforces, so rounding up to
// it here keeps a very short window answerable rather than turning it into
// an ErrBadFilter the caller cannot act on -- their window is legal, it is
// simply shorter than this path's own resolution.
func activityBucket(span time.Duration) time.Duration {
	if span <= 0 {
		return time.Second
	}
	b := span / activityBucketCount
	if b < time.Second {
		return time.Second
	}
	return b.Round(time.Second)
}

// handleCollectionChurnPeers serves GET /v1/collection/churn/peers.
//
// The churn signal ranked by peer instead of drawn over time: one row per
// (router, peer), busiest first, which is what Monitor's "peers by update
// volume" table reads. Same classification, same window clamp, no bucket --
// this answer has no time axis, so there are no bars to bound.
//
// It is NOT route-churn.json's "Changes by peer" panel, which groups by
// (bucket, peer_ip) and draws a line each. They share the classification and
// not the shape; they were once wrongly built as if they were one query.
func (s *Server) handleCollectionChurnPeers(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	f, ok := s.churnGroupFilter(w, r)
	if !ok {
		return
	}
	// The window this answer actually covers, which is where changes_per_
	// second's denominator has to come from: since= is a duration or an
	// instant, and only this side has resolved which.
	window := now.Sub(f.Since).Seconds()

	// The same window, cut into a fixed NUMBER of bars rather than a width
	// the caller names. A sparkline has a fixed pixel width, so what it
	// needs is a constant bar count; /v1/collection/churn is the path where
	// the width is the caller's decision because there the chart's own
	// width is. The width this resolved to is reported in meta for the
	// reason that path reports its own: a chart labeled with a width it was
	// not drawn at is the defect that field exists to prevent.
	af := f
	af.Bucket = activityBucket(now.Sub(f.Since))
	// BOTH reads bounded at the same instant, so the ranking and the series
	// describe one window. Without it each is open at the top and a row
	// archived between the two queries lands in whichever ran later -- the
	// sum identity the query tests pin (every peer's bars add up to its own
	// readvertise + withdraw) would then hold on a seeded fixture and not in
	// production. `now` is the same instant churn_to reports, so the answer
	// says which window it means.
	f.Until = now
	af.Until = now
	rows, err := s.q.ChurnByPeer(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	activity, err := s.q.ChurnPeerActivity(r.Context(), af)
	if err != nil {
		// Same branch handleCollectionChurn takes on the same sentinel: the
		// bars-per-answer guard is a property of the REQUEST, so it belongs
		// to the caller as a 400 naming what to change rather than to the
		// operator as a 500. Unreachable while activityBucket floors at a
		// second -- and that is the kind of "unreachable" that stops being
		// true the moment the width is derived differently.
		if errors.Is(err, query.ErrBadFilter) {
			s.badParam(w, err)
			return
		}
		s.fail(w, r, err)
		return
	}
	// Grouped by the same (router, peer) identity the rows carry, and
	// unmapped on both sides already, so the key cannot miss the way an
	// IPv4-mapped address makes joins miss elsewhere in this package.
	byPeer := map[[2]string][]query.ChurnPeerActivity{}
	for _, a := range activity {
		k := [2]string{a.RouterIP.String(), a.PeerIP.String()}
		byPeer[k] = append(byPeer[k], a)
	}

	bucket := af.Bucket.String()
	// The same bounds /v1/collection/churn sends, and needed here for the
	// same reason: each row's activity omits its quiet buckets, so a client
	// can only place the bars it DID get against a window it knows. Without
	// them it would have to space them evenly -- which draws a peer that
	// churned twice an hour apart identically to one that churned twice in
	// a minute -- or bound the axis with its own clock, which is the defect
	// these fields were added to end.
	from := f.Since.UTC()
	to := now.UTC()
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, func(p query.PeerChurn) WirePeerChurn {
			k := [2]string{p.RouterIP.String(), p.PeerIP.String()}
			return NewWirePeerChurn(p, window, byPeer[k])
		}),
		Meta: Meta{
			Warnings:    Warnings{},
			ChurnBucket: &bucket,
			ChurnFrom:   &from,
			ChurnTo:     &to,
		},
	})
}

// handleCollectionChurnPrefixes serves GET /v1/collection/churn/prefixes.
//
// route-churn.json's "Most-changed prefixes", ranked, capped at the same 50
// that panel uses. The cap is not a `limit` parameter: this is a ranking, and
// a page past the first answers no question the first did not, so there is
// nothing for a caller to page through and no total_matched to report.
//
// route_unicast alone -- see WirePrefixChurn for why a prefix cannot be
// summed across the three tables, and why undecoded rows are excluded.
func (s *Server) handleCollectionChurnPrefixes(w http.ResponseWriter, r *http.Request) {
	f, ok := s.churnGroupFilter(w, r)
	if !ok {
		return
	}
	rows, err := s.q.ChurnByPrefix(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWirePrefixChurn),
		Meta: Meta{Warnings: Warnings{}},
	})
}

// churnGroupFilter parses the scope and window the two grouped churn answers
// share, and writes the 400 itself if either is bad.
//
// Neither takes bucket=: they have no time axis. Both take the SAME window
// clamp as /v1/collection/churn, for the same measured reason -- they read
// the same route tables across the same window, and router= or peer= narrows
// the WHERE without turning the read into a seek.
func (s *Server) churnGroupFilter(w http.ResponseWriter, r *http.Request) (query.ChurnFilter, bool) {
	p := newParams(r)
	now := time.Now()
	f := query.ChurnFilter{
		Router:    p.addr("router"),
		Peer:      p.addr("peer"),
		Since:     p.since(now),
		Collector: p.str("collector"),
	}
	if p.err == nil {
		if err := s.checkUnscopedWindow(f.Since, now, unscopedChurnHint); err != nil {
			p.err = err
		}
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return query.ChurnFilter{}, false
	}
	return f, true
}
