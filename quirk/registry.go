package quirk

import vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"

// ID names one entry in the quirk Registry. IDs are stable strings (not an
// iota-based enum) because they are logged and put in per-router config
// overrides, where they are matched by name.
type ID string

const (
	QkTSZero           ID = "QK_TS_ZERO"
	QkAddPathHeuristic ID = "QK_ADDPATH_HEURISTIC"
	QkVersionUnparsed  ID = "QK_VERSION_UNPARSED"
	QkCapsMissing      ID = "QK_CAPS_MISSING"
)

// Entry describes one known vendor/BMP quirk: which envelope ParseFlag it
// maps to, and (for quirks with a known static signature) which profiles it
// statically applies to. AppliesTo is nil for quirks that are exclusively
// detected at runtime from the byte stream (Latch), with no known
// vendor/version signature to match against ahead of time.
type Entry struct {
	ID          ID
	Layer       string // which taxonomy layer this quirk belongs to
	Flag        vantagev1.ParseFlag
	AppliesTo   func(Profile) bool // static detection; nil = dynamic-only
	Description string
}

// Registry is the central quirk table. A new quirk is one Entry
// (+ hook use at its layer + corpus sample + docs/quirks.md row).
//
// Concurrency: Registry is read-only after package init. Resolve reads it
// once per BMP session, and the collector runs sessions concurrently across
// goroutines; nothing in this package appends to or mutates Registry at
// runtime, which is what makes those concurrent reads safe. Any future code
// that mutates Registry outside of init (e.g. a hot-reloadable quirk table)
// would race with those concurrent Resolve calls and must add its own
// synchronization — mirroring the single-goroutine-ownership contract
// documented on Set below, but for the table Resolve reads from instead of
// the Set it produces.
var Registry = []Entry{
	{QkTSZero, "per-peer-header", vantagev1.ParseFlag_PARSE_FLAG_TS_COLLECTOR_FALLBACK, nil,
		"router sends zero timestamp in per-peer header; collector time substituted"},
	{QkAddPathHeuristic, "peer-up", vantagev1.ParseFlag_PARSE_FLAG_ADDPATH_HEURISTIC, nil,
		"add-path wire format decided structurally because capabilities were absent or contradicted the data"},
	{QkVersionUnparsed, "init-tlv", vantagev1.ParseFlag_PARSE_FLAG_VERSION_UNPARSED, nil,
		"sysDescr version string not parseable; vendor-only quirk matching in effect"},
	{QkCapsMissing, "peer-up", vantagev1.ParseFlag_PARSE_FLAG_CAPS_MISSING, nil,
		"route monitoring for a peer with no Peer-Up OPENs on record"},
}

// FlagFor returns the envelope ParseFlag for a registered quirk ID, or
// PARSE_FLAG_UNSPECIFIED if id isn't in Registry.
func FlagFor(id ID) vantagev1.ParseFlag {
	for _, e := range Registry {
		if e.ID == id {
			return e.Flag
		}
	}
	return vantagev1.ParseFlag_PARSE_FLAG_UNSPECIFIED
}

// Set is a session's resolved quirk state. Not safe for concurrent use; a
// Set belongs to exactly one BMP session goroutine.
type Set struct {
	active   map[ID]bool
	disabled map[ID]bool
}

// Resolve computes the static QuirkSet for a profile, honoring per-router
// config overrides. Precedence, highest to lowest:
//
//  1. disable always wins: a disabled quirk cannot be made Active by force,
//     static profile match, or a later Latch.
//  2. force and dynamic Latch calls both directly set a quirk active; each
//     can activate a quirk regardless of what the profile's version claims
//     (dynamic detection outranks version claims).
//  3. static profile match (Entry.AppliesTo) is the weakest signal: it only
//     sets the initial state and never overrides a later disable.
//
// Dynamic quirks (QK_VERSION_UNPARSED here; others via Latch) join after
// the static pass.
func Resolve(p Profile, force, disable []ID) *Set {
	s := &Set{active: map[ID]bool{}, disabled: map[ID]bool{}}
	for _, e := range Registry {
		if e.AppliesTo != nil && e.AppliesTo(p) {
			s.active[e.ID] = true
		}
	}
	// The vendor is known but its version string didn't match any scheme
	// this package reads: vendor-only quirk matching is in effect for the
	// rest of the session.
	//
	// The Vendor check is what makes the flag mean anything. It used to be
	// absent, so the quirk fired on any non-empty sysDescr with an
	// unreadable version -- which, once three real senders were compared
	// against the anchor table, turned out to be EVERY envelope from EVERY
	// router. A flag that is always set distinguishes nothing. With no
	// vendor there is also no "vendor-only matching" to fall back TO, so
	// raising it there claimed more than the collector knew.
	//
	// An unidentified sender is now silent here rather than mislabeled.
	// That is a deliberate gap: reporting "we could not identify this
	// router at all" is a different statement from "unparseable version",
	// and it wants its own flag rather than a borrowed one.
	//
	// An empty sysDescr (no Initiation TLV at all) is a distinct case and
	// does not imply this quirk. This must run before the force/disable
	// loops below (not after) so that a disable of QK_VERSION_UNPARSED is
	// honored in s.active itself, not merely papered over by Active's
	// re-check of s.disabled — s.active must be a truthful record of state
	// for any future code that iterates it directly (logging, a flags
	// helper) rather than only ever calling Active.
	if p.Vendor != "" && !p.Version.OK {
		s.active[QkVersionUnparsed] = true
	}
	for _, id := range force {
		s.active[id] = true
	}
	for _, id := range disable {
		s.disabled[id] = true
		delete(s.active, id)
	}
	return s
}

// Active reports whether id is in effect for this session: it must have
// been activated (statically, by force, or by Latch) and must not be
// disabled. disable is checked here rather than only at write time so that
// a Latch occurring after Resolve can never resurrect a disabled quirk.
func (s *Set) Active(id ID) bool { return s.active[id] && !s.disabled[id] }

// Latch activates a quirk from runtime evidence observed in the byte
// stream — this is how dynamic detection outranks a version claim that
// said the quirk shouldn't apply. Returns true only the first time a given
// id is latched, so callers can log/metric once per session. A disabled
// quirk can never be latched active.
//
// Latch is safe to call on a zero-value Set (Resolve is the intended
// constructor, but Set and Latch are both exported, so a zero-value Set{}
// must not panic): reads from a nil map yield the zero value in Go, but an
// assignment to a nil map does panic, so the active map is lazily
// initialized here if a zero-value Set reaches this point.
func (s *Set) Latch(id ID) bool {
	if s.disabled[id] || s.active[id] {
		return false
	}
	if s.active == nil {
		s.active = map[ID]bool{}
	}
	s.active[id] = true
	return true
}
