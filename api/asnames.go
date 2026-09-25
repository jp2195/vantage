// GET /v1/asnames: RIPE's registered AS holder names, looked up in batch
// for whatever ASNs a caller names.
//
// The dataset is OPTIONAL BY DESIGN -- a person runs `make fetch-asnames`
// and applies deploy/clickhouse/asnames.sql and asnames_meta.sql by hand;
// nobody has to, and vantage runs identically without either. This file's
// whole job is making the three states query.(*Q).ASNames and
// ASNamesPublished already distinguish (loaded-and-listed,
// loaded-and-unlisted, no-dataset-at-all) survive onto the wire without
// collapsing back into two: see WireASName's own doc comment for the
// per-row half of that, and Meta.ASNamesLoaded / Meta.ASNamesPublished for
// the envelope half.
package api

import (
	"net/http"
	"strconv"
	"time"
)

// maxASNBatch is the contract's own ceiling on how many ASNs one GET
// /v1/asnames request may name: no screen renders more than a page of
// rows and the widest table caps well below this, so 512 is headroom, not
// a limit anyone meets in practice.
//
// It is enforced HERE, not in query/, because query.(*Q).ASNames
// deliberately does not enforce it -- see that method's own doc comment,
// which names this file as the trust boundary that has to. A request past
// it is refused with a 400 naming the bound, the way limit=0 is refused
// rather than silently becoming a default page: exceeding a documented
// cap is the caller's mistake to fix, not this daemon's to paper over by
// truncating a batch it never agreed to answer past 512 of.
const maxASNBatch = 512

// asns parses every asn= value this request named.
//
// At least one is required: GET /v1/asnames answering a request that
// named none by returning every ASN in the dataset (122,442 of them, on
// the real one) would be a dump, not an answer to the question asked.
// Naming more than max is refused rather than truncated, per
// maxASNBatch's own doc comment.
//
// Duplicates are preserved in the COUNT that is checked against max (a
// caller naming asn= max+1 times, even the same value repeated, still
// trips the cap -- the request costs the same regardless) but collapsed
// in the returned slice, in first-seen order: the response answers each
// distinct ASN once, and a caller that named one twice does not need two
// identical rows to tell that apart from a caller that never asked about
// it.
//
// No individual value is refused for being 0 or otherwise "reserved" the
// way origin_asn= and through_asn= refuse it (see (*params).asn). Those
// two use 0 as their OWN unset sentinel, so an explicit 0 there would be
// silently swallowed as "no filter"; this parameter has no such sentinel
// -- absence is expressed by naming asn= zero times, not by the value 0
// -- so accepting 0 introduces no ambiguity, and refusing it would only
// mean a caller asking "does AS 0 have a registered holder" (it does not,
// and never will) gets a 400 instead of the correct, unremarkable answer.
func (p *params) asns(name string, max int) []uint32 {
	if p.err != nil {
		return nil
	}
	raw := p.v[name]
	if len(raw) == 0 {
		p.fail("%s= is required at least once; a request naming none would answer "+
			"every AS this dataset holds rather than the ones asked about", name)
		return nil
	}
	if len(raw) > max {
		p.fail("the request named %d AS numbers; GET /v1/asnames accepts at most %d "+
			"per request", len(raw), max)
		return nil
	}
	seen := make(map[uint32]bool, len(raw))
	out := make([]uint32, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			p.fail("%s= names a value that is not an AS number (a decimal 0-4294967295)", name)
			return nil
		}
		asn := uint32(n)
		if seen[asn] {
			continue
		}
		seen[asn] = true
		out = append(out, asn)
	}
	return out
}

// handleASNames serves GET /v1/asnames.
//
// The two query/ calls are independent on purpose -- see
// query.(*Q).ASNamesPublished's own doc comment on why the names
// dictionary and its date companion are checked separately rather than
// one gating the other -- so both run on every request regardless of
// what the first answered. That costs one extra cheap query per batch
// when the dataset is not loaded at all; it buys a response that is
// honest about EACH dictionary's own state rather than one that infers
// the second from the first.
func (s *Server) handleASNames(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	asns := p.asns("asn", maxASNBatch)
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	names, loaded, err := s.q.ASNames(r.Context(), asns)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rawPublished, publishedLoaded, err := s.q.ASNamesPublished(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	rows := make([]WireASName, len(asns))
	for i, asn := range asns {
		rows[i] = NewWireASName(asn, names[asn])
	}

	var published *time.Time
	if publishedLoaded {
		// A value this package's own fetch pipeline could not produce is
		// treated as "no date" rather than surfaced as a 500: see
		// query.(*Q).ASNamesPublished's own doc comment on why parsing
		// happens here, at the wire boundary, and why a parse failure can
		// only ever mean this project's own tooling wrote something
		// unexpected -- never a fact a caller supplied.
		if t, err := http.ParseTime(rawPublished); err == nil {
			// .UTC(), matching stamp()'s own rule for every other timestamp
			// this daemon renders: the wire form should not depend on
			// whether http.ParseTime happened to attach a zone object
			// named "GMT" or the package's own UTC location for a
			// zero-offset value.
			t = t.UTC()
			published = &t
		}
	}

	s.writeJSON(w, http.StatusOK, Envelope{
		Data: rows,
		Meta: Meta{
			Warnings:         Warnings{},
			ASNamesLoaded:    &loaded,
			ASNamesPublished: published,
		},
	})
}
