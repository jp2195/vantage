// Collection health: /v1/collection/dumps, /v1/collection/sessions,
// /v1/collection/locrib and /v1/collection/flags.
//
// Four handlers rather than one keyed fanout, and the split is not
// decorative -- see api/handlers.go's own account of /v1/routes' fanout for
// the contrast. Those three families are the SAME question asked three ways
// and answered together on purpose. These four are different questions,
// over different tables, at different costs -- the flags signal alone unions
// ten tables and is the expensive one (see query.FlagCounts) -- joined only
// by being rendered on one screen, which is a UI composition concern and
// not an API one. A single endpoint here would let the slowest of the four
// gate the other three, and one failing query would fail all four instead
// of the one that actually broke.
//
// Each handler still keeps api/handlers.go's own four steps in the same
// order: parse the parameters, call exactly one query/ method, convert with
// a matching newWireX, wrap in an Envelope. What differs from the rest of
// that file is that all four share one parameter set -- router=, since=,
// limit=, nothing else -- so collectionFilter below parses it once rather
// than four near-identical times.
package api

import (
	"net/http"
	"time"

	"github.com/jp2195/vantage/query"
)

// collectionFilter parses the parameters every /v1/collection/* endpoint
// takes and enforces the window clamp all four share.
//
// THE CLAMP APPLIES UNCONDITIONALLY, with no scoped exemption the way
// /v1/events carves one out for a request naming both router AND peer. That
// is not an oversight; it is the shape of the four statements. /v1/events'
// scoped mode is a keyset SEEK, pinned to one session, and a wide window
// there costs the same as a narrow one. None of these four route through a
// cursor or a session pin at all -- they are aggregates over every row the
// window covers, and query.CollectionFilter's own Router field narrows the
// WHERE (router_ip is a PARTITION BY / GROUP BY column on all four
// statements) without turning the read into a seek the way pinning a
// session does. So a wide since= is still a wide read even when router= is
// set, and the contract carries no router-scoped carve-out: absent -> 1h
// (historyDefaultSince), wider than cfg.MaxUnscopedSince -> a 400 through
// checkUnscopedWindow, the SAME function /v1/events' unscoped mode calls,
// rather than a second clamp with its own number to keep in agreement.
//
// What this passes it, and /v1/events does not, is unscopedCollectionHint.
// The clamp is shared; the remediation is not. /v1/events' own hint tells a
// caller to name router= AND peer=, and both halves of that advice are false
// here -- peer= is not a parameter this function parses, and router= grants
// no exemption from the clamp, which is exactly what the paragraph above
// says and what TestCollectionEndpointsClampTheWindow asserts. See
// checkUnscopedWindow and unscopedCollectionHint for the rest.
//
// now is threaded through rather than read from time.Now() here, matching
// every other handler in this package that needs one clock shared between
// resolving since= and checking it against that same instant.
func (s *Server) collectionFilter(p *params, now time.Time) query.CollectionFilter {
	f := query.CollectionFilter{
		Router: p.addr("router"),
		Since:  p.since(now),
		Limit:  p.limit(s.cfg),
	}
	if p.err != nil {
		return f
	}
	if err := s.checkUnscopedWindow(f.Since, now, unscopedCollectionHint); err != nil {
		p.err = err
	}
	return f
}

// handleCollectionDumps serves GET /v1/collection/dumps.
func (s *Server) handleCollectionDumps(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	f := s.collectionFilter(p, time.Now())
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := s.q.DumpCounts(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{TotalMatched: &total, Warnings: Warnings{}, DumpTotals: dumpTotals(rows)}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireRouterDumpCount),
		Meta: meta,
	})
}

// dumpTotals sums Archived, Dumps and Changes across the rows DumpCounts
// returned -- meta.dump_totals, never a synthesized row in data. Never nil:
// an empty rows still has a real, all-zero total, which is a different,
// positive claim from the key being absent (see Meta's own doc comment).
func dumpTotals(rows []query.RouterDumpCount) *WireDumpTotals {
	var t WireDumpTotals
	for _, r := range rows {
		t.Archived += r.Archived
		t.Dumps += r.Dumps
		t.Changes += r.Changes
	}
	return &t
}

// handleCollectionSessions serves GET /v1/collection/sessions.
func (s *Server) handleCollectionSessions(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	f := s.collectionFilter(p, time.Now())
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := s.q.SessionCounts(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{TotalMatched: &total, Warnings: Warnings{}, SessionTotals: sessionTotals(rows)}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireRouterSessionCount),
		Meta: meta,
	})
}

// sessionTotals sums Sessions, Up, Down and ViewLost across the rows
// SessionCounts returned -- meta.session_totals. See dumpTotals' own doc
// comment for why this never returns nil.
func sessionTotals(rows []query.RouterSessionCount) *WireSessionTotals {
	var t WireSessionTotals
	for _, r := range rows {
		t.Sessions += r.Sessions
		t.Up += r.Up
		t.Down += r.Down
		t.ViewLost += r.ViewLost
	}
	return &t
}

// handleCollectionLocRIB serves GET /v1/collection/locrib.
func (s *Server) handleCollectionLocRIB(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	f := s.collectionFilter(p, time.Now())
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := s.q.LocRIBComparison(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{TotalMatched: &total, Warnings: Warnings{}, LocRIBTotals: locRIBTotals(rows)}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWirePeerLocRIB),
		Meta: meta,
	})
}

// locRIBTotals sums Reported and Archived across the rows LocRIBComparison
// returned -- meta.locrib_totals. See dumpTotals' own doc comment for why
// this never returns nil.
//
// It sums every returned row regardless of HasStat. A peer with HasStat
// false always carries Reported == 0 already (see PeerLocRIB's own doc
// comment: absence from stats_events IS what HasStat false means), so
// summing it changes nothing about Reported and correctly still counts
// whatever route_unicast rows that peer's own Archived carries.
func locRIBTotals(rows []query.PeerLocRIB) *WireLocRIBTotals {
	var t WireLocRIBTotals
	for _, r := range rows {
		t.Reported += r.Reported
		t.Archived += r.Archived
	}
	return &t
}

// handleCollectionFlags serves GET /v1/collection/flags.
func (s *Server) handleCollectionFlags(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	f := s.collectionFilter(p, time.Now())
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := s.q.FlagCounts(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{TotalMatched: &total, Warnings: Warnings{}, FlagTotals: flagTotals(rows)}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireFlagCount),
		Meta: meta,
	})
}

// flagTotals sums Envelopes across the rows FlagCounts returned --
// meta.flag_totals. See dumpTotals' own doc comment for why this never
// returns nil.
func flagTotals(rows []query.FlagCount) *WireFlagTotals {
	var t WireFlagTotals
	for _, r := range rows {
		t.Envelopes += r.Envelopes
	}
	return &t
}
