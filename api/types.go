// Package api is vantage's HTTP read API over the query package.
//
// This file holds the wire types: the JSON shapes api/openapi.yaml
// describes, and the converters from query's Go types into them.
//
// They are separate types rather than json tags on query's own structs
// because query returns netip.Addr, uint64 and two deliberately paired
// forms -- OriginASN beside ASPath, Label beside HasLabel -- that JSON has
// to collapse into single nullable fields. Tagging query's structs would
// make the query layer own a wire format it should know nothing about, and
// would leave the collapsing with nowhere to live: there is no json tag
// that means "null when the field next to me is empty".
//
// Three rules run through every type below, all three from
// api/openapi.yaml's own header:
//
//   - u64 identity fields (session_id, seq) are JSON STRINGS. session_id is
//     a UnixNano timestamp, ~1.77e18, past 2^53. Go round-trips a uint64
//     exactly, so no Go test would ever notice the number form; a
//     JavaScript client or an LLM's JSON parser silently rounds it.
//   - A field whose zero value is a real value is a pointer, and it is
//     never omitempty. AS 0 is reserved and label 0 is the implicit-null
//     label, so 0-as-absent is a confident wrong answer in both cases, and
//     omitempty would drop a genuine label 0 on the floor.
//   - required in the contract means the key is ALWAYS PRESENT, not that
//     its value is non-null. No field here carries omitempty, so a caller
//     never has to tell "the server omitted this" from "this route has no
//     MED".
package api

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"time"

	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/status"
)

// Envelope is the {data, meta} object every 200 response in the contract is
// shaped like. Data is any because the three shapes the contract puts there
// -- an array of one wire type, the /v1/routes fanout object, and nothing
// else -- have no useful common type, and because a generic envelope would
// buy nothing: the handler is what knows which one it is returning.
//
// Data is the caller's responsibility to make non-nil. An empty result must
// reach here as an empty slice, not a nil one, for the reason Warnings
// documents; Envelope cannot enforce that through an any field, and a
// handler returning a nil slice is caught by that handler's own test.
type Envelope struct {
	Data any  `json:"data"`
	Meta Meta `json:"meta"`
}

// Meta is the contract's Meta schema.
//
// NextCursor is emitted unconditionally, as null when there is no next
// page. The contract has it optional (only warnings is required) so absent
// would also validate, but "present and null" is what the rest of this file
// commits to for every nullable field, and a client that reads
// meta.next_cursor === null on the last page is reading the same shape it
// read on every other page.
type Meta struct {
	NextCursor *string  `json:"next_cursor"`
	Warnings   Warnings `json:"warnings"`

	// TotalMatched is how many rows matched, which is not how many were
	// returned once a cap applies. It is a pointer so that an endpoint with
	// no cap emits null rather than a 0 indistinguishable from an empty
	// answer.
	TotalMatched *uint64 `json:"total_matched"`

	// CommunityColumns names the columns a community= searched, and is
	// omitted entirely when the request carried none -- unlike every other
	// field here, whose absence would be ambiguous. Here it is not: no
	// community= was asked, so no column was searched, and an empty array
	// would suggest one was and found nothing.
	CommunityColumns []string `json:"community_columns,omitempty"`

	// DumpTotals, SessionTotals, LocRIBTotals and FlagTotals are the totals
	// for the one /v1/collection/* signal that produced this response --
	// omitted on every other endpoint's Meta, including the other three of
	// these four. "Fleet totals" would be the wrong phrase and the contract
	// no longer uses it: each is the sum across every row THIS ANSWER
	// RETURNED, so it inherits data's own busiest-first cap and describes
	// the fleet only when limit= did not bite. Each sums that row type's own
	// numeric fields, and it lives in Meta rather than in data because a
	// total rendered as a synthesized row would be a row every client had
	// to learn to exclude, and one that would eventually be sorted, filtered
	// or counted as a member (see api/openapi.yaml's own header on this
	// endpoint family). Putting it here instead removes that whole class of
	// mistake at the type level -- WireRouterDumpCount and its three
	// siblings carry no "this is the total" row to build by accident.
	//
	// Never nil on the endpoint that populates it, even when data is empty:
	// a fleet with nothing archived this window still has a real total, all
	// zeros, and that is a different, positive claim from the key being
	// absent. See dumpTotals, sessionTotals, locRIBTotals and flagTotals in
	// api/collection.go, the only callers that ever set one of these four.
	DumpTotals    *WireDumpTotals    `json:"dump_totals,omitempty"`
	SessionTotals *WireSessionTotals `json:"session_totals,omitempty"`
	LocRIBTotals  *WireLocRIBTotals  `json:"locrib_totals,omitempty"`
	FlagTotals    *WireFlagTotals    `json:"flag_totals,omitempty"`

	// ChurnBucket is the width every bar in a /v1/collection/churn answer
	// was computed at, as a Go duration string ("5m0s"). Present only on
	// that path, and always present there, including when data is empty: a
	// chart labeled with a width it did not use is the same defect as one
	// labeled with a window it did not use, and the width is a decision the
	// server can make (bucket= is optional) rather than one the caller
	// always states.
	ChurnBucket *string `json:"churn_bucket,omitempty"`

	// The window a /v1/collection/churn answer covers, on the DAEMON's
	// clock: churn_from is what since= resolved to, churn_to is the moment
	// the answer was computed. Both present on that path and absent
	// everywhere else, exactly like churn_bucket.
	//
	// They exist because a bar cannot be PLACED without them. Every ts in
	// the data is a collector timestamp, and a client with only its own
	// clock to bound the axis is drawing collector time against browser
	// time -- which is what MonitorView did until 2026-09-21, deriving the
	// chart's window from `new Date()`. A browser minutes off shifted every
	// bar against its own gridline with nothing on screen able to show it.
	// The bucket width was already sent for the same reason: a picture is
	// only as good as the claim beside it.
	ChurnFrom *time.Time `json:"churn_from,omitempty"`
	ChurnTo   *time.Time `json:"churn_to,omitempty"`

	// ActivityWindow is how far back GET /v1/collectors' own sparkline
	// looks, as a Go duration string ("30m0s") -- present only on that
	// path, and always present there, including when data is empty. Named
	// on the wire rather than left for a client to infer by counting a
	// card's own activity entries or reading its first and last minute: this
	// project has twice shipped a panel that showed a rate or a window
	// nobody named, and this field is what keeps a third one from being a
	// silent assumption instead of a stated fact.
	ActivityWindow *string `json:"activity_window,omitempty"`

	// RetentionDays is the history retention the archive applies, in days,
	// read from the history tables' own TTL (query.(*Q).RetentionDays), not
	// from configuration -- present only on GET /v1/collectors, the screen
	// that already speaks of the archive's reach. History older than this
	// is deleted; current state does not expire. Omitted, on that path
	// too, when the TTL is not a whole number of days (one set by hand in
	// months), because the number would then be a guess, and when the TTL
	// could not be read at all, which is logged rather than failing the
	// request.
	RetentionDays *int `json:"retention_days,omitempty"`

	// ASNamesLoaded says whether GET /v1/asnames' own AS holder-name
	// dataset is loaded AT ALL -- present only on that path, and always
	// present there, true or false, never omitted: see
	// query.(*Q).ASNames' own three states. It answers only the THIRD of
	// those -- "is there a dataset to ask" -- and says nothing about the
	// second (an ASN the dataset simply does not list, which reads back
	// as an equally present but explicitly empty WireASName). A client
	// that reads an empty name without checking this field first cannot
	// tell "we have never been told any AS's holder" from "this AS has
	// none," which is the exact confusion this field exists to prevent.
	//
	// Pointer and omitempty for ChurnBucket and ActivityWindow's own
	// reason: nil, and so omitted, on every OTHER endpoint's Meta, where
	// the question does not apply; never nil on this one.
	ASNamesLoaded *bool `json:"asnames_loaded,omitempty"`

	// ASNamesPublished is WHEN THE DATASET'S SOURCE PUBLISHED IT -- RIPE's
	// own Last-Modified response header, captured once by `make
	// fetch-asnames` and carried into ClickHouse by a companion
	// dictionary (see query.(*Q).ASNamesPublished) -- NEVER the moment
	// that command happened to run. A stale holder name that still looks
	// authoritative is the one way this optional dataset can mislead, and
	// this field is what lets a caller judge that for itself rather than
	// trusting a name with no age attached.
	//
	// ABSENT -- not present-and-null -- whenever ASNamesLoaded is false,
	// and ALSO absent when ASNamesLoaded is true but the date's own
	// companion dictionary is itself unavailable: the two dictionaries
	// are ordinarily fetched and applied together but are independent
	// ClickHouse objects, so one being loaded is never a promise that the
	// other is too. The omitempty is what makes that absence, and it is
	// deliberate: this field joins ChurnBucket and ActivityWindow's
	// pointer-and-omitempty shape, not NextCursor and TotalMatched's
	// unconditional nullable one. Dropping the omitempty to emit null
	// here would put asnames_published: null on EVERY endpoint's Meta,
	// where the age of an unrelated ASN dataset is not a question that
	// applies. A caller tells "no date" from "a date" by the key's
	// presence, and tells no-dataset from dataset-without-a-date by
	// reading ASNamesLoaded, which is always present on that path.
	ASNamesPublished *time.Time `json:"asnames_published,omitempty"`
}

// Warnings is meta.warnings, and it exists as a named type only so that the
// empty case can be guaranteed by the type rather than by whoever built the
// value.
//
// The contract documents an empty array as a positive claim -- "no
// contributing session was mid-dump and no smear applies" -- and a nil Go
// slice marshals to null, which reads as "unknown" to a client checking for
// the key. That is a different answer, and the wrong one.
//
// The guarantee lives here rather than in an envelope constructor because
// handlers build Meta values directly, including zero ones: a constructor
// only fixes the values that go through it, and Meta{} marshaled straight
// to a response is exactly the case that has to be safe.
type Warnings []Warning

// MarshalJSON renders a nil Warnings as [] rather than null. The conversion
// to the unnamed []Warning is what keeps this from recursing: an unnamed
// slice type has no methods.
func (w Warnings) MarshalJSON() ([]byte, error) {
	if w == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]Warning(w))
}

// Warning is one machine-readable caveat about an answer's completeness.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The warning codes the contract's enum allows. A caveat this API cannot
// name is one it must not emit: the enum is part of the contract, so an
// invented code is a contract violation rather than extra information.
const (
	WarnSessionDumping = "session_dumping"
	WarnPaginatedSmear = "paginated_smear"

	// WarnTruncated says the answer was capped. It is always accompanied by
	// meta.total_matched, which is the number that makes it actionable.
	WarnTruncated = "truncated"

	// WarnCollectorStale says at least one row came from a collector that has
	// not been heard from within the stale threshold: the row is that
	// collector's last view, not a current one.
	WarnCollectorStale = "collector_stale"
)

// ErrorResponse is the contract's ErrorResponse: every non-2xx body.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the object inside it. Message is human-readable and always
// redaction-safe -- see the contract; nothing here may echo a token or a
// raw driver error.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The error codes the contract's enum allows.
const (
	ErrInvalidParam   = "invalid_param"
	ErrUnauthorized   = "unauthorized"
	ErrSessionChanged = "session_changed"
	ErrInternal       = "internal"
)

// u64 renders a u64 identity field as the string the contract types it as.
// See this file's header for why the number form is a defect no Go test can
// see.
func u64(v uint64) string { return strconv.FormatUint(v, 10) }

// addrText renders an address the contract types as a plain (non-nullable)
// string.
//
// It returns "" for an invalid Addr rather than calling String() on it,
// because netip.Addr{}.String() returns the literal text "invalid IP" --
// a value that looks like an answer, would be rendered as one in a looking
// glass, and would be indexed as one by anything reading these responses.
// query never produces an invalid address in these fields, so "" should
// never appear; if one ever does, an empty string is unmistakably not an
// address, which "invalid IP" is not.
func addrText(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// addrOrNull renders an address the contract types as [string, "null"] --
// today only next_hop, on all four route-bearing types.
//
// The check is IsValid, not emptiness: the zero Addr's String() is the
// literal "invalid IP", so a next hop that failed to parse (or a withdrawal,
// which carries no path attributes at all and so has no next hop to report)
// would otherwise reach a looking glass as that text. next_hop is the field
// a looking glass reaches for first, and on RouteHistory the no-next-hop
// case is the ordinary one rather than the exceptional one.
func addrOrNull(a netip.Addr) *string {
	if !a.IsValid() {
		return nil
	}
	s := a.String()
	return &s
}

// stamp renders a timestamp. encoding/json gives time.Time the RFC 3339 the
// contract's date-time format asks for; the UTC is so that every timestamp
// in every response ends in Z regardless of the location clickhouse-go's
// scan happened to attach, rather than the wire form depending on a server
// setting. It does not move the instant.
func stamp(t time.Time) time.Time { return t.UTC() }

// stampOrNull is stamp for a nullable instant: nil stays nil.
func stampOrNull(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// originASN collapses query's OriginASN+ASPath pair into the contract's
// single nullable origin_asn.
//
// The pair exists because AS 0 is reserved: a route with no AS path and a
// route that genuinely originated in AS 0 are the same uint32, and query
// deliberately refuses to guess which one a caller is looking at (see
// query.Route's doc comment: as_path[-1] previously reported AS 0 for
// empty-path VPN routes). Empty path is the one and only condition under
// which origin is unknowable, and null is how the contract says so. 36 of
// the archive's 420 live unicast routes and 142 of its 164 VPN rows take
// this branch.
func originASN(asPath []uint32, origin uint32) *uint32 {
	if len(asPath) == 0 {
		return nil
	}
	return &origin
}

// labelOrNull collapses query's Label+HasLabel pair into the contract's
// single nullable label.
//
// HasLabel is what carries the distinction that label 0 cannot: 0 is the
// real implicit-null label, which an MPLS router genuinely advertises, so
// "no label" has to be a different value on the wire and not a zero. This
// is also why label is a pointer rather than a uint32 with omitempty --
// omitempty would drop a true implicit-null label, reporting absence for a
// value that is present.
func labelOrNull(v uint32, has bool) *uint32 {
	if !has {
		return nil
	}
	return &v
}

// arrayOf returns s, or an empty slice when s is nil.
//
// Every array-typed field in the contract is required and typed array, not
// [array, "null"]: an empty AS path is ordinary (iBGP, reflected routes), an
// empty label stack means no labels, and none of them has a null form. A nil
// Go slice marshals to null, so every slice this file emits goes through
// here. query already guarantees non-nil for the fields it scans, which
// makes this belt-and-braces for those -- but a query value built in a test
// or by a future code path that does not scan has nil slices, and that
// must not be able to change the shape of the wire form.
func arrayOf[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// wireBytes renders a []uint8 as the contract's array of small integers.
//
// uint8 IS byte, and encoding/json special-cases []byte: it marshals as a
// base64 string rather than a JSON array, regardless of any json tag on the
// field that holds it. sr_algorithms is documented as `type: array, items:
// integer`, not a string, so a []uint8 passed straight to arrayOf would
// satisfy the "never nil" rule while still marshaling as the wrong JSON
// type entirely -- TestLSArraysAreNeverNull is what caught this the one
// time it was tried. Converting to []int sidesteps the special case, and
// the result still goes through arrayOf so nil and empty both render [].
func wireBytes(s []uint8) []int {
	out := make([]int, len(s))
	for i, v := range s {
		out[i] = int(v)
	}
	return arrayOf(out)
}

// The NewWireX converters are exported, and the reason is cmd/vantage
// rather than anything in this package.
//
// `vantage query -o json` prints the same documents this daemon serves, and
// it has to: it is the same tool an operator uses against the API, so two
// JSON shapes from one binary -- one when it goes over HTTP, another when
// it is pointed straight at ClickHouse with -dsn -- would be a difference
// with no explanation behind it. Worse, a second renderer would be a second
// place to get the u64-as-string rule wrong, and that defect is invisible
// in Go: session_id fits a uint64 perfectly and only loses precision once
// something downstream reads it as a float64.
//
// So the CLI calls these rather than owning copies. What it does own is the
// INVERSE -- see cmd/vantage/query.go, which decodes a response back into
// query/ values -- because nothing here needs it and a decoder in this
// package would be untested by anything this package serves.

// WireRouter is the contract's Router schema.
type WireRouter struct {
	SysName   string `json:"sysname"`
	IP        string `json:"ip"`
	Collector string `json:"collector"`
	SessionID string `json:"session_id"`
	PeersUp   int    `json:"peers_up"`
	PeersDown int    `json:"peers_down"`
	// PeersViewLost is peers the collector stopped being able to see, as
	// opposed to peers the router reported down. See query.Router's own
	// field for why the two are separate counts, and the contract's
	// peers_view_lost description for what a reader does with it.
	PeersViewLost int `json:"peers_view_lost"`
	// PeersStale is peers whose last word was up while their collector has
	// not been heard from within the stale threshold. See query.Router's own
	// field.
	PeersStale int       `json:"peers_stale"`
	LastSeen   time.Time `json:"last_seen"`
}

func NewWireRouter(r query.Router) WireRouter {
	return WireRouter{
		SysName:       r.SysName,
		IP:            addrText(r.IP),
		Collector:     r.Collector,
		SessionID:     u64(r.SessionID),
		PeersUp:       r.PeersUp,
		PeersDown:     r.PeersDown,
		PeersViewLost: r.PeersViewLost,
		PeersStale:    r.PeersStale,
		LastSeen:      stamp(r.LastSeen),
	}
}

// WirePeer is the contract's Peer schema.
//
// DumpStates is a plain map because the contract's dump_states is a plain
// object of family to "complete"|"dumping", and a family with neither is
// absent rather than present as a third value -- see query.Peer's doc
// comment for why absence is the honest answer there.
//
// It is never nil: NewWirePeer substitutes an empty map, because a down
// peer's map is empty and the contract documents {} as the positive claim
// "this peer has no dump in progress", where null would read as "unknown".
// The guarantee sits in the converter rather than in a named map type with
// its own MarshalJSON -- the way Warnings does it -- because nothing builds
// a WirePeer by hand: every one comes from here.
type WirePeer struct {
	RouterIP   string            `json:"router_ip"`
	PeerIP     string            `json:"peer_ip"`
	Collector  string            `json:"collector"`
	RIB        string            `json:"rib"`
	ASN        uint32            `json:"asn"`
	State      string            `json:"state"`
	DumpStates map[string]string `json:"dump_states"`
	SessionID  string            `json:"session_id"`
	Routes     int               `json:"routes"`

	// The session facts, as of the newest event in this session that
	// carried them -- NOT the newest event, because a Peer Down carries no
	// OPEN message at all.
	//
	// hold_time is null, not 0, when no OPEN was observed. Zero is a REAL
	// hold time (RFC 4271 §4.2: the timer never expires, keepalives off), so
	// a client reading 0 must be able to tell the two apart, and a peer whose
	// only event is a Down is the second case. A nullable number is the only
	// encoding where the difference survives JSON.
	//
	// There is no keepalive field. The OPEN carries none -- the interval is
	// a local timer a speaker never announces -- and hold_time/3 is a
	// convention that would inherit this object's authority.
	HoldTime        *uint16  `json:"hold_time"`
	MPFamilies      []string `json:"mp_families"`
	AddPathFamilies []string `json:"addpath_families"`
	SysDescr        string   `json:"sys_descr"`

	// When this peer's current session last came up, on the collector
	// clock. Null -- never the epoch -- when the session never carried an
	// up, for the reason hold_time is null rather than 0: an absence and a
	// real value must stay distinguishable through JSON.
	//
	// AN INSTANT, NOT AN UPTIME, and deliberately so. A measurement on
	// 2026-09-21 compared `now - up_since` against a lab archive and found
	// it equal to how long the router had been SILENT for 25 of 35 peers --
	// a lab torn down rather than shut down sends no Peer Down, so the last
	// state stays `up` forever. Serving a duration would put this API's
	// authority behind archive silence rendered as a live session; serving
	// the instant leaves the client to say what it can support.
	UpSince *time.Time `json:"up_since"`
}

func NewWirePeer(p query.Peer) WirePeer {
	ds := p.DumpStates
	if ds == nil {
		ds = map[string]string{}
	}
	return WirePeer{
		RouterIP:   addrText(p.RouterIP),
		PeerIP:     addrText(p.PeerIP),
		Collector:  p.Collector,
		RIB:        p.RIB,
		ASN:        p.ASN,
		State:      p.State,
		DumpStates: ds,
		SessionID:  u64(p.SessionID),
		Routes:     p.Routes,
		// The pointer IS the "was it observed" flag on the wire. Setting it
		// only when seen is what keeps a negotiated 0 distinguishable from
		// an unobserved one; see the field's own comment.
		HoldTime:        holdTimeOrNull(p),
		MPFamilies:      orEmpty(p.MPFamilies),
		AddPathFamilies: orEmpty(p.AddPathFamilies),
		SysDescr:        p.SysDescr,
		UpSince:         upSinceOrNull(p),
	}
}

// upSinceOrNull keeps "never came up" out of the value space, the same way
// holdTimeOrNull does. query.Peer carries the zero instant for a session
// with no up at all, and marshaling that yields a real-looking
// "0001-01-01T00:00:00Z" on the wire.
func upSinceOrNull(p query.Peer) *time.Time {
	if p.UpSince.IsZero() {
		return nil
	}
	// stamp(), not a bare .UTC(): that helper is the one place the "every
	// wire timestamp ends in Z" rule is written down, and a second spelling
	// here would be a second place to change it.
	t := stamp(p.UpSince)
	return &t
}

func holdTimeOrNull(p query.Peer) *uint16 {
	if !p.HoldTimeSeen {
		return nil
	}
	h := p.HoldTime
	return &h
}

// orEmpty renders a nil slice as [], never null. The two mean different
// things to a JSON reader and only one of them is true here: the answer is
// "this session negotiated no families of that kind", which is a list with
// nothing in it, not an absent list. The same rule every other array on
// these types follows.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// WireRouteCommon is the contract's RouteCommon schema, the allOf every
// route type is built over. It is embedded rather than repeated so that the
// three route types cannot drift apart in field name, json tag or
// nullability -- encoding/json flattens an embedded struct, so the wire
// shape is the same as if these fields were spelled out in each.
//
// The three converters below fill it field by field rather than through one
// shared constructor: the fields are thirteen of them and four are plain
// strings, so a positional helper would be a row of same-typed arguments
// that a swap would silently survive. Named assignment costs three copies
// and cannot be misordered. TestRouteTypesAgreeOnCommonFields is what keeps
// the copies honest.
//
// Communities and LargeCommunities are already rendered strings when they
// arrive: query.communityStrings turns the stored Array(UInt32) into
// "65000:100" notation inside the query layer, for reasons its own doc
// comment gives, and large communities are stored as RFC 8092 text to begin
// with. There is deliberately no second renderer here -- a duplicate would
// be a second place for the shift-and-mask to be wrong. ExtCommunities and
// RouteTargets need no renderer at all: query already hands both back as
// rendered text (see query.Route's own doc comment on the two), so this
// layer's job is the same arrayOf pass-through every array field here gets.
//
// RouteTargets used to be a field on WireVPNRoute and WireEVPNRoute alone,
// each with its own json tag -- that is two places the same key could drift
// in name or nullability, for a column both tables have carried from the
// start. It lives here instead, beside ExtCommunities, so that
// UnicastRoute, VPNRoute and EVPNRoute answer the community= filter's
// question -- did this route's ext_communities or route_targets match? --
// from the identical field regardless of which route type asked it.
type WireRouteCommon struct {
	RouterSysName    string   `json:"router_sysname"`
	RouterIP         string   `json:"router_ip"`
	PeerIP           string   `json:"peer_ip"`
	Collector        string   `json:"collector"`
	RIB              string   `json:"rib"`
	PathID           uint32   `json:"path_id"`
	NextHop          *string  `json:"next_hop"`
	ASPath           []uint32 `json:"as_path"`
	OriginASN        *uint32  `json:"origin_asn"`
	MED              *uint32  `json:"med"`
	LocalPref        *uint32  `json:"local_pref"`
	Communities      []string `json:"communities"`
	LargeCommunities []string `json:"large_communities"`
	ExtCommunities   []string `json:"ext_communities"`
	RouteTargets     []string `json:"route_targets"`
	DumpState        string   `json:"dump_state"`
}

// WireUnicastRoute is the contract's UnicastRoute schema.
type WireUnicastRoute struct {
	WireRouteCommon
	Family string `json:"family"`
	Prefix string `json:"prefix"`
}

func NewWireUnicastRoute(r query.Route) WireUnicastRoute {
	return WireUnicastRoute{
		RouterSysName:    r.RouterSysName,
		RouterIP:         addrText(r.RouterIP),
		PeerIP:           addrText(r.PeerIP),
		Collector:        r.Collector,
		RIB:              r.RIB,
		PathID:           r.PathID,
		NextHop:          addrOrNull(r.NextHop),
		ASPath:           arrayOf(r.ASPath),
		OriginASN:        originASN(r.ASPath, r.OriginASN),
		MED:              r.MED,
		LocalPref:        r.LocalPref,
		Communities:      arrayOf(r.Communities),
		LargeCommunities: arrayOf(r.LargeCommunities),
		ExtCommunities:   arrayOf(r.ExtCommunities),
		RouteTargets:     arrayOf(r.RouteTargets),
		DumpState:        r.DumpState,
		Family:           r.Family,
		Prefix:           r.Prefix,
	}
}

// WireVPNRoute is the contract's VPNRoute schema.
//
// RD is "" for lu4, which carries no route distinguisher -- the real column
// value, never a rendered placeholder, the same rule query.VPNRoute keeps.
type WireVPNRoute struct {
	WireRouteCommon
	Family string  `json:"family"`
	RD     string  `json:"rd"`
	Prefix string  `json:"prefix"`
	Label  *uint32 `json:"label"`
}

func NewWireVPNRoute(r query.VPNRoute) WireVPNRoute {
	return WireVPNRoute{
		RouterSysName:    r.RouterSysName,
		RouterIP:         addrText(r.RouterIP),
		PeerIP:           addrText(r.PeerIP),
		Collector:        r.Collector,
		RIB:              r.RIB,
		PathID:           r.PathID,
		NextHop:          addrOrNull(r.NextHop),
		ASPath:           arrayOf(r.ASPath),
		OriginASN:        originASN(r.ASPath, r.OriginASN),
		MED:              r.MED,
		LocalPref:        r.LocalPref,
		Communities:      arrayOf(r.Communities),
		LargeCommunities: arrayOf(r.LargeCommunities),
		ExtCommunities:   arrayOf(r.ExtCommunities),
		RouteTargets:     arrayOf(r.RouteTargets),
		DumpState:        r.DumpState,
		Family:           r.Family,
		RD:               r.RD,
		Prefix:           r.Prefix,
		Label:            labelOrNull(r.Label, r.HasLabel),
	}
}

// WireEVPNRoute is the contract's EVPNRoute schema.
//
// There is no family field, matching both EVPNRoute.required and
// query.EVPNRoute: route_evpn has no family column because an EVPN table
// holds exactly one family, and a field filled in from a constant would be
// this layer asserting a fact rather than reporting one.
//
// Labels is the stack as received, and it is a plain array with no nullable
// element: an empty stack means no labels, so the null-vs-0 ambiguity that
// makes VPNRoute.Label a pointer has no counterpart here.
type WireEVPNRoute struct {
	WireRouteCommon
	RouteType   uint8    `json:"route_type"`
	RD          string   `json:"rd"`
	Prefix      string   `json:"prefix"`
	MAC         string   `json:"mac"`
	IP          string   `json:"ip"`
	GatewayIP   string   `json:"gateway_ip"`
	EthernetTag uint32   `json:"ethernet_tag"`
	ESI         string   `json:"esi"`
	Labels      []uint32 `json:"labels"`
}

func NewWireEVPNRoute(r query.EVPNRoute) WireEVPNRoute {
	return WireEVPNRoute{
		RouterSysName:    r.RouterSysName,
		RouterIP:         addrText(r.RouterIP),
		PeerIP:           addrText(r.PeerIP),
		Collector:        r.Collector,
		RIB:              r.RIB,
		PathID:           r.PathID,
		NextHop:          addrOrNull(r.NextHop),
		ASPath:           arrayOf(r.ASPath),
		OriginASN:        originASN(r.ASPath, r.OriginASN),
		MED:              r.MED,
		LocalPref:        r.LocalPref,
		Communities:      arrayOf(r.Communities),
		LargeCommunities: arrayOf(r.LargeCommunities),
		ExtCommunities:   arrayOf(r.ExtCommunities),
		RouteTargets:     arrayOf(r.RouteTargets),
		DumpState:        r.DumpState,
		RouteType:        r.RouteType,
		RD:               r.RD,
		Prefix:           r.Prefix,
		MAC:              r.MAC,
		IP:               r.IP,
		GatewayIP:        r.GatewayIP,
		EthernetTag:      r.EthernetTag,
		ESI:              r.ESI,
		Labels:           arrayOf(r.Labels),
	}
}

// WireRouteFanout is the contract's RouteFanout schema: the data of
// /v1/routes, the looking-glass fan-out.
//
// It is a keyed object of three homogeneous arrays rather than one mixed
// array, which is the contract's decision and not a convenience: a unicast
// route, a VPN route and an EVPN route have different required fields, so a
// single array would have to be typed as a union that no client could
// narrow without inspecting a field first. Three keys keep each family's
// schema exact.
//
// All three arrays are required and typed array with no null form, so all
// three go through arrayOf. An empty one is a positive claim -- "this
// prefix is in no VPN table" -- where null would read as "we did not look".
// A fan-out is exactly the answer where "we did not look" is the wrong
// thing to say about two of the three families.
type WireRouteFanout struct {
	Unicast []WireUnicastRoute `json:"unicast"`
	VPN     []WireVPNRoute     `json:"vpn"`
	EVPN    []WireEVPNRoute    `json:"evpn"`
}

func NewWireRouteFanout(u []WireUnicastRoute, v []WireVPNRoute, e []WireEVPNRoute) WireRouteFanout {
	return WireRouteFanout{
		Unicast: arrayOf(u),
		VPN:     arrayOf(v),
		EVPN:    arrayOf(e),
	}
}

// WireTopologyFanout is the contract's TopologyFanout schema: the data of
// /v1/topology.
//
// It keeps WireRouteFanout's three-key discipline and its arrayOf treatment
// for the same reasons, but it is NOT that type and could not have been: a
// route fan-out is three arrays of ROUTES, and this is three GRAPHS. The
// keys are the only thing the two share.
//
// Three separate graphs rather than one merged one is the contract's
// decision and not a convenience. Merging them would assert that an EVPN
// adjacency and a unicast adjacency are the same kind of edge, which is a
// claim no BGP speaker makes and one worth doubting: the three graphs differ
// by an order of magnitude in size, and EVPN's real one is two edges wide.
type WireTopologyFanout struct {
	Unicast WireGraph `json:"unicast"`
	VPN     WireGraph `json:"vpn"`
	EVPN    WireGraph `json:"evpn"`
}

func NewWireTopologyFanout(u, v, e query.Graph) WireTopologyFanout {
	return WireTopologyFanout{
		Unicast: NewWireGraph(u),
		VPN:     NewWireGraph(v),
		EVPN:    NewWireGraph(e),
	}
}

// WireGraph is the contract's Graph schema: one family's merged AS-path
// graph for one scope.
//
// Both members are arrays with no null form, and an empty one is an ordinary
// answer rather than an exceptional one -- a one-hop AS path contributes a
// node and no edge at all, and a scope that reaches nothing in a family is a
// real result. Null would read as "we did not look", which is exactly the
// thing a fan-out under three required keys exists to stop a caller having
// to guess.
//
// query.Graph's own Routes is not carried here. It is the population the
// graph was built from rather than a property of the graph, all three
// families' are summed into meta.total_matched, and a per-family copy beside
// nodes and edges would invite a client to add three numbers that are
// already added.
type WireGraph struct {
	Nodes []WireASNode `json:"nodes"`
	Edges []WireASEdge `json:"edges"`
}

func NewWireGraph(g query.Graph) WireGraph {
	// mapRows already ends in arrayOf, so a nil query.Graph -- one built in
	// a test, or returned beside an error -- still renders two empty arrays
	// rather than two nulls.
	return WireGraph{
		Nodes: mapRows(g.Nodes, NewWireASNode),
		Edges: mapRows(g.Edges, NewWireASEdge),
	}
}

// WireASNode is the contract's ASNode schema: one autonomous system in a
// merged AS-path graph.
//
// Roles collapses query.ASNode's three booleans into the SET they describe,
// which is what they are: one AS is routinely the last hop of one route and
// the middle of another, so origin and transit are not exclusive and are not
// an enum. Three booleans on the wire would say the same thing while
// tempting a client to render them as three columns; an enum would say
// something false. The order is fixed so that two responses describing the
// same node are the same bytes.
//
// There is no holder name here, on purpose. GET /v1/asnames serves RIPE's
// registered holder names (query.(*Q).ASNames), and the answer for this
// graph's own nodes is still the bare ASN: a client that
// wants "AS3356" beside "LEVEL3 - Level 3 Parent, LLC" fetches the batch
// once for every distinct ASN on screen and renders the name in a rail,
// rather than this fanout repeating a name string on every node of every
// graph it returns -- a duplication that would grow with the graph rather
// than with the (much smaller) set of distinct ASes on it, and would go
// stale the moment the two endpoints' answers were cached for different
// lengths of time.
type WireASNode struct {
	ASN       uint32    `json:"asn"`
	Routes    uint64    `json:"routes"`
	Roles     []string  `json:"roles"`
	FirstSeen time.Time `json:"first_seen"`
}

// The role names, and the one place they are spelled. observed_peer rather
// than "peer" because peer= is also a request parameter naming a BMP
// neighbor's address: the role is a fact about this fleet's vantage point --
// "this is the AS of a peer we hold a session with" -- rather than about the
// internet, and the longer name is what keeps the two from reading as the
// same thing.
const (
	roleOrigin       = "origin"
	roleTransit      = "transit"
	roleObservedPeer = "observed_peer"
)

func NewWireASNode(n query.ASNode) WireASNode {
	var roles []string
	if n.Origin {
		roles = append(roles, roleOrigin)
	}
	if n.Transit {
		roles = append(roles, roleTransit)
	}
	if n.Peer {
		roles = append(roles, roleObservedPeer)
	}
	return WireASNode{
		ASN:       n.ASN,
		Routes:    n.Routes,
		Roles:     arrayOf(roles),
		FirstSeen: stamp(n.FirstSeen),
	}
}

// WireASEdge is the contract's ASEdge schema: one AS adjacency.
//
// Routes and LiveRoutes are carried separately rather than collapsed into a
// state string, and that pairing is the whole reason this type exists.
// live_routes == 0 means every route carrying this adjacency has been
// withdrawn; a client derives the screen's three stroke states from these
// two fields plus first_seen, without a second call. A server-computed state
// would have to bake the window the screen is showing into the response, and
// the window is a thing the operator moves.
//
// FirstSeen is query.ASEdge.FirstSeen unchanged, which is the earliest
// observation of the ROUTES that currently traverse this edge rather than of
// the edge itself -- see that type. The distinction is stated in the
// contract rather than quietly rounded off, because the wire form cannot
// carry the caveat and a client reading "first_seen" will otherwise assume
// the stronger claim.
//
// The error runs in BOTH directions, and the contract says so: a re-advertised
// route carries its original timestamp onto a new adjacency (older than the
// edge is), and a route that has stopped carrying an adjacency contributes
// nothing to it (newer than the edge is). The contract briefly claimed only
// the first could happen, and so that a client treating a recent first_seen
// as "newly appeared" would understate rather than overstate. It does not
// and it would not: query.ASEdge makes no directional promise, and neither
// may anything above it.
type WireASEdge struct {
	Src        uint32    `json:"src"`
	Dst        uint32    `json:"dst"`
	Routes     uint64    `json:"routes"`
	LiveRoutes uint64    `json:"live_routes"`
	FirstSeen  time.Time `json:"first_seen"`
}

func NewWireASEdge(e query.ASEdge) WireASEdge {
	return WireASEdge{
		Src:        e.Src,
		Dst:        e.Dst,
		Routes:     e.Routes,
		LiveRoutes: e.LiveRoutes,
		FirstSeen:  stamp(e.FirstSeen),
	}
}

// WireHistoryEvent is the contract's HistoryEvent schema.
//
// Seq is a string for the same reason SessionID is: it is a u64, and the
// contract types every u64 identity as a string. There is no stream_seq
// field, though the contract's header names it among the u64 identities and
// RouteHistory orders by it -- it is a fact about the JetStream transport
// rather than about the network, HistoryEvent does not list it, and query
// does not carry it. Surfacing it would be this repo's most recurring
// defect in its mildest form: a collection artifact reported as if it
// described BGP.
//
// TsRouter is router-reported and untrusted; a router with a dead clock
// reports 1970. It is here because it is data, not because anything should
// order on it -- that is TsCollector's job.
type WireHistoryEvent struct {
	TsCollector time.Time `json:"ts_collector"`
	TsRouter    time.Time `json:"ts_router"`
	Seq         string    `json:"seq"`
	SessionID   string    `json:"session_id"`
	RouterIP    string    `json:"router_ip"`
	PeerIP      string    `json:"peer_ip"`
	Collector   string    `json:"collector"`
	RIB         string    `json:"rib"`
	Family      string    `json:"family"`
	Prefix      string    `json:"prefix"`
	PathID      uint32    `json:"path_id"`
	Action      string    `json:"action"`
	NextHop     *string   `json:"next_hop"`
	ASPath      []uint32  `json:"as_path"`
}

func NewWireHistoryEvent(e query.HistoryEvent) WireHistoryEvent {
	return WireHistoryEvent{
		TsCollector: stamp(e.TsCollector),
		TsRouter:    stamp(e.TsRouter),
		Seq:         u64(e.Seq),
		SessionID:   u64(e.SessionID),
		RouterIP:    addrText(e.RouterIP),
		PeerIP:      addrText(e.PeerIP),
		Collector:   e.Collector,
		RIB:         e.RIB,
		Family:      e.Family,
		Prefix:      e.Prefix,
		PathID:      e.PathID,
		Action:      e.Action,
		NextHop:     addrOrNull(e.NextHop),
		ASPath:      arrayOf(e.ASPath),
	}
}

// peerDownReasons is RFC 7854 sec 4.9's BMP Peer Down reason code registry.
// A code absent from this map gets an empty name rather than a guess -- the
// number beside it is the whole truth, the same contract query.ProtocolName
// (and, on this file's own LsLink.protocol_name) already holds for IGP
// protocol IDs.
var peerDownReasons = map[uint32]string{
	1: "local system closed, notification follows",
	2: "local system closed, FSM event follows",
	3: "remote system closed, notification follows",
	4: "remote system closed, no notification",
	5: "peer de-configured",
	6: "local system closed, TLVs follow",
}

// reasonName renders down_reason as its registry name, or "" when kind is
// not "down" or when down_reason names a code this project has no entry
// for. Both are positive answers, never guesses: a view_lost or up event's
// down_reason is 0 because the router said nothing at all, and naming a
// code it never sent would invent one; an unregistered down code is
// reported as the number alone, exactly as query.ProtocolName reports an
// unregistered protocol ID.
func reasonName(kind string, code uint32) string {
	if kind != "down" {
		return ""
	}
	return peerDownReasons[code]
}

// WirePeerEvent is the contract's PeerEvent schema: peer_events reported as
// the event it was, WireHistoryEvent's own reason applied to the one table
// that records what HAPPENED to a session rather than what state it is in
// now.
//
// StreamSeq, Seq and SessionID are strings for the u64-identity reason this
// file's header states: StreamSeq is peer_events' own physical sort key's
// last column and query.PeerEventsPage's cursor tie-breaker, Seq is the
// collector-assigned per-(peer, session) sequence, and SessionID is a
// UnixNano that exceeds 2^53 exactly as Router.session_id does.
//
// DownReason and ReasonName carry query.PeerEvent's own contract verbatim.
// DownReason is never coerced, defaulted or normalized -- a stored 0 is
// reported as 0, whether the row is a "down" with a code this project does
// not have a name for, or a "view_lost"/"up" whose down_reason is 0 because
// the router said nothing. ReasonName is "" in both of those cases, and it
// is a positive answer either way -- see reasonName.
type WirePeerEvent struct {
	TsCollector   time.Time `json:"ts_collector"`
	TsRouter      time.Time `json:"ts_router"`
	StreamSeq     string    `json:"stream_seq"`
	Seq           string    `json:"seq"`
	SessionID     string    `json:"session_id"`
	RouterIP      string    `json:"router_ip"`
	RouterSysname string    `json:"router_sysname"`
	PeerIP        string    `json:"peer_ip"`
	PeerASN       uint32    `json:"peer_asn"`
	Collector     string    `json:"collector"`
	RIB           string    `json:"rib"`
	Kind          string    `json:"kind"`
	DownReason    uint32    `json:"down_reason"`
	ReasonName    string    `json:"reason_name"`
	LocalIP       string    `json:"local_ip"`
	LocalPort     uint32    `json:"local_port"`
	RemotePort    uint32    `json:"remote_port"`
}

func NewWirePeerEvent(e query.PeerEvent) WirePeerEvent {
	return WirePeerEvent{
		TsCollector:   stamp(e.TsCollector),
		TsRouter:      stamp(e.TsRouter),
		StreamSeq:     u64(e.StreamSeq),
		Seq:           u64(e.Seq),
		SessionID:     u64(e.SessionID),
		RouterIP:      addrText(e.RouterIP),
		RouterSysname: e.RouterSysname,
		PeerIP:        addrText(e.PeerIP),
		PeerASN:       e.PeerASN,
		Collector:     e.Collector,
		RIB:           e.RIB,
		Kind:          e.Kind,
		DownReason:    e.DownReason,
		ReasonName:    reasonName(e.Kind, e.DownReason),
		LocalIP:       addrText(e.LocalIP),
		LocalPort:     e.LocalPort,
		RemotePort:    e.RemotePort,
	}
}

// WireLSCommon is the observer-and-identity block LSNode and LSPrefix share.
// LSLink does NOT embed it: a link has two node identities rather than one,
// so the fields that would be shared here live on WireLSEndpoint twice.
type WireLSCommon struct {
	RouterSysName string `json:"router_sysname"`
	RouterIP      string `json:"router_ip"`
	PeerIP        string `json:"peer_ip"`
	Collector     string `json:"collector"`
	RIB           string `json:"rib"`

	Protocol     uint8  `json:"protocol"`
	ProtocolName string `json:"protocol_name"`
	// Identifier and NodeKey are strings for the reason session_id is: a
	// cityHash64 routinely exceeds 2^53, and a JSON number would be rounded
	// by a JavaScript client or an LLM's parser -- silently collapsing two
	// distinct nodes into one key.
	Identifier string `json:"identifier"`
	ASN        uint32 `json:"asn"`
	BGPLSID    uint32 `json:"bgpls_id"`
	Area       uint32 `json:"area"`
	RouterID   string `json:"router_id"`
	NodeKey    string `json:"node_key"`

	IsWithdraw bool   `json:"is_withdraw"`
	DumpState  string `json:"dump_state"`
}

func newWireLSCommon(
	sysname string, routerIP, peerIP netip.Addr, collector, rib string,
	protocol uint8, identifier uint64, asn, bgplsID, area uint32,
	routerID string, nodeKey uint64, isWithdraw bool, dumpState string,
) WireLSCommon {
	return WireLSCommon{
		RouterSysName: sysname,
		RouterIP:      addrText(routerIP),
		PeerIP:        addrText(peerIP),
		Collector:     collector,
		RIB:           rib,
		Protocol:      protocol,
		ProtocolName:  query.ProtocolName(protocol),
		Identifier:    u64(identifier),
		ASN:           asn,
		BGPLSID:       bgplsID,
		Area:          area,
		RouterID:      routerID,
		NodeKey:       u64(nodeKey),
		IsWithdraw:    isWithdraw,
		DumpState:     dumpState,
	}
}

// WireLSNode is the contract's LSNode schema.
//
// SRAlgorithms is []int, not []uint8 like query.LSNode.SRAlgorithms: see
// wireBytes for why a plain arrayOf(n.SRAlgorithms) would have marshaled as
// a base64 string instead of the array the contract documents.
type WireLSNode struct {
	WireLSCommon
	RouterIDv4   string `json:"router_id_v4"`
	Name         string `json:"name"`
	SRGBBase     uint32 `json:"srgb_base"`
	SRGBSize     uint32 `json:"srgb_size"`
	SRLBBase     uint32 `json:"srlb_base"`
	SRLBSize     uint32 `json:"srlb_size"`
	SRAlgorithms []int  `json:"sr_algorithms"`
}

func NewWireLSNode(n query.LSNode) WireLSNode {
	return WireLSNode{
		WireLSCommon: newWireLSCommon(
			n.RouterSysName, n.RouterIP, n.PeerIP, n.Collector, n.RIB,
			n.Protocol, n.Identifier, n.ASN, n.BGPLSID, n.Area,
			n.RouterID, n.NodeKey, n.IsWithdraw, n.DumpState,
		),
		RouterIDv4:   n.RouterIDv4,
		Name:         n.Name,
		SRGBBase:     n.SRGBBase,
		SRGBSize:     n.SRGBSize,
		SRLBBase:     n.SRLBBase,
		SRLBSize:     n.SRLBSize,
		SRAlgorithms: wireBytes(n.SRAlgorithms),
	}
}

// WireLSEndpoint is the contract's LSEndpoint schema: one end of a link.
//
// There is exactly one converter for it, newWireLSEndpoint, and it is used
// for both the local and the remote end of a WireLSLink. Two hand-written
// converters -- one per end -- is exactly how the remote end ends up
// carrying the local end's data: a field added to one and forgotten in the
// other would type-check and marshal cleanly, and nothing would catch it
// except a test built to look for that specific mistake. One function used
// twice cannot drift from itself.
type WireLSEndpoint struct {
	ASN         uint32 `json:"asn"`
	BGPLSID     uint32 `json:"bgpls_id"`
	Area        uint32 `json:"area"`
	RouterID    string `json:"router_id"`
	NodeKey     string `json:"node_key"`
	IfAddr      string `json:"ifaddr"`
	InterfaceID uint32 `json:"interface_id"`
	Label       string `json:"label"`
	LabelSource string `json:"label_source"`
}

func newWireLSEndpoint(e query.LSEndpoint) WireLSEndpoint {
	return WireLSEndpoint{
		ASN:         e.ASN,
		BGPLSID:     e.BGPLSID,
		Area:        e.Area,
		RouterID:    e.RouterID,
		NodeKey:     u64(e.NodeKey),
		IfAddr:      e.IfAddr,
		InterfaceID: e.InterfaceID,
		Label:       e.Label,
		LabelSource: e.LabelSource,
	}
}

// WireLSLink is the contract's LSLink schema.
//
// It does not embed WireLSCommon: a link has two node identities, local and
// remote, rather than the one LSNode and LSPrefix each carry, so the
// observer-and-identity fields that WireLSCommon holds for those two are
// spelled out here directly instead, alongside the two WireLSEndpoint
// values that hold what a single node identity would.
type WireLSLink struct {
	RouterSysName string `json:"router_sysname"`
	RouterIP      string `json:"router_ip"`
	PeerIP        string `json:"peer_ip"`
	Collector     string `json:"collector"`
	RIB           string `json:"rib"`

	Protocol     uint8  `json:"protocol"`
	ProtocolName string `json:"protocol_name"`
	Identifier   string `json:"identifier"`

	Local  WireLSEndpoint `json:"local"`
	Remote WireLSEndpoint `json:"remote"`

	AdjSIDs      []uint32 `json:"adj_sids"`
	TEMetric     uint32   `json:"te_metric"`
	IGPMetric    uint32   `json:"igp_metric"`
	AdminGroup   uint32   `json:"admin_group"`
	MaxBandwidth float32  `json:"max_bandwidth"`

	IsWithdraw bool   `json:"is_withdraw"`
	DumpState  string `json:"dump_state"`
}

func NewWireLSLink(l query.LSLink) WireLSLink {
	return WireLSLink{
		RouterSysName: l.RouterSysName,
		RouterIP:      addrText(l.RouterIP),
		PeerIP:        addrText(l.PeerIP),
		Collector:     l.Collector,
		RIB:           l.RIB,
		Protocol:      l.Protocol,
		ProtocolName:  query.ProtocolName(l.Protocol),
		Identifier:    u64(l.Identifier),
		Local:         newWireLSEndpoint(l.Local),
		Remote:        newWireLSEndpoint(l.Remote),
		AdjSIDs:       arrayOf(l.AdjSIDs),
		TEMetric:      l.TEMetric,
		IGPMetric:     l.IGPMetric,
		AdminGroup:    l.AdminGroup,
		MaxBandwidth:  l.MaxBandwidth,
		IsWithdraw:    l.IsWithdraw,
		DumpState:     l.DumpState,
	}
}

// WireLSPrefix is the contract's LSPrefix schema.
type WireLSPrefix struct {
	WireLSCommon
	Prefix         string `json:"prefix"`
	PrefixSID      uint32 `json:"prefix_sid"`
	PrefixSIDFlags uint8  `json:"prefix_sid_flags"`
	HasPrefixSID   bool   `json:"has_prefix_sid"`
	PrefixMetric   uint32 `json:"prefix_metric"`
	OSPFRouteType  uint8  `json:"ospf_route_type"`
}

func NewWireLSPrefix(p query.LSPrefix) WireLSPrefix {
	return WireLSPrefix{
		WireLSCommon: newWireLSCommon(
			p.RouterSysName, p.RouterIP, p.PeerIP, p.Collector, p.RIB,
			p.Protocol, p.Identifier, p.ASN, p.BGPLSID, p.Area,
			p.RouterID, p.NodeKey, p.IsWithdraw, p.DumpState,
		),
		Prefix:         p.Prefix,
		PrefixSID:      p.PrefixSID,
		PrefixSIDFlags: p.PrefixSIDFlags,
		HasPrefixSID:   p.HasPrefixSID,
		PrefixMetric:   p.PrefixMetric,
		OSPFRouteType:  p.OSPFRouteType,
	}
}

// --- /v1/collection/* wire types ---
//
// Four signals, four shapes, no shared struct between them beyond what
// query.CollectionFilter already gives their handlers: see api/collection.go
// for why one endpoint per signal rather than one keyed fanout. The
// semantics behind each field -- what a dump is, why a Loc-RIB gap is not
// proof of loss, why parse flags are historical -- live on query's own
// RouterDumpCount, RouterSessionCount, PeerLocRIB and FlagCount, and are
// restated in api/openapi.yaml's own response descriptions, which is where
// a caller who never reads Go actually encounters them; the comments here
// stay short on purpose rather than a third copy of the same text.

// WireRouterDumpCount is the contract's RouterDumpCount schema. Counts are
// plain JSON numbers, not the u64-as-string form this file's header
// reserves for IDENTITY fields (session_id, seq, stream_seq): Archived,
// Dumps and Changes are row counts bounded by table size, the same
// reasoning that already leaves Router.peers_up and Peer.routes as plain
// integers.
type WireRouterDumpCount struct {
	RouterIP      string `json:"router_ip"`
	RouterSysname string `json:"router_sysname"`
	Archived      uint64 `json:"archived"`
	Dumps         uint64 `json:"dumps"`
	Changes       uint64 `json:"changes"`
}

func NewWireRouterDumpCount(r query.RouterDumpCount) WireRouterDumpCount {
	return WireRouterDumpCount{
		RouterIP:      addrText(r.RouterIP),
		RouterSysname: r.RouterSysname,
		Archived:      r.Archived,
		Dumps:         r.Dumps,
		Changes:       r.Changes,
	}
}

// WireChurnBucket is one bar of /v1/collection/churn: what the archive
// received inside one time bucket, split three ways.
//
// The three counts are mutually exclusive and sum to every row the bucket
// held -- a session dump is never a withdrawal, by the classification's own
// definition. They are NOT /v1/collection/dumps' two: that path's `changes`
// is everything that is not a dump, withdrawals included, and anything
// comparing the two answers must add readvertise and withdraw first.
type WireChurnBucket struct {
	// TS is the bucket's start, aligned to a multiple of meta.churn_bucket,
	// not the timestamp of the first row inside it.
	TS          string `json:"ts"`
	Readvertise uint64 `json:"readvertise"`
	Withdraw    uint64 `json:"withdraw"`
	Dump        uint64 `json:"dump"`
}

func NewWireChurnBucket(b query.ChurnBucket) WireChurnBucket {
	return WireChurnBucket{
		TS:          b.TS.UTC().Format(time.RFC3339Nano),
		Readvertise: b.Readvertise,
		Withdraw:    b.Withdraw,
		Dump:        b.Dump,
	}
}

// WirePeerChurn is one row of /v1/collection/churn/peers: one peer's churn
// totals over the window, busiest first.
//
// ACROSS ALL THREE ROUTE TABLES, unlike the prefix ranking below. peer_ip
// names the same thing on route_unicast, route_vpn and route_evpn, so adding
// a peer's unicast, VPN and EVPN churn under one label is addition. A
// prefix does not have that property, which is why the two paths differ.
//
// The three counts are /v1/collection/churn's own three, summed rather than
// bucketed: mutually exclusive, and together every row the peer sent.
type WirePeerChurn struct {
	RouterIP      string `json:"router_ip"`
	RouterSysname string `json:"router_sysname"`
	PeerIP        string `json:"peer_ip"`
	PeerASN       uint32 `json:"peer_asn"`
	Readvertise   uint64 `json:"readvertise"`
	Withdraw      uint64 `json:"withdraw"`
	Dump          uint64 `json:"dump"`

	// ChangesPerSecond is (readvertise + withdraw) over the window's length
	// in seconds -- ARCHIVED ROWS PER SECOND, not the peer's update rate.
	//
	// Computed here rather than by the client because this is where the
	// window is known exactly. A caller sends since= as a duration OR an
	// RFC 3339 instant, and only the daemon has resolved which and against
	// what clock; a client dividing by its own reading of the same string
	// would be dividing by a second, slightly different window.
	//
	// Dumps are excluded for the reason this path ranks without them: a
	// session that restarted once would otherwise outrank a peer that is
	// genuinely busy.
	ChangesPerSecond float64 `json:"changes_per_second"`

	// Activity is this peer's changes over time: the same number
	// ChangesPerSecond averages, kept as the shape an average destroys.
	// Two peers with one rate can have churned steadily and all at once,
	// and only this tells them apart.
	//
	// Oldest first, one entry per bucket that HELD a change -- quiet
	// buckets are absent rather than zero, so the array's length is a
	// function of the data and not of the window. A client draws them on an
	// axis it already knows the bounds of, the same way it draws
	// /v1/collection/churn's own bars.
	//
	// Never null: a peer that changed nothing gets [], which is the honest
	// "no changes in this window" rather than "no answer".
	Activity []WireChurnActivity `json:"activity"`
}

// WireChurnActivity is one bar of a peer's sparkline.
type WireChurnActivity struct {
	// Bucket is the bar's START, aligned to a multiple of
	// meta.churn_bucket, on the collector clock -- the same alignment and
	// the same clock /v1/collection/churn's own ts uses.
	Bucket time.Time `json:"bucket"`

	// Changes is re-advertisements plus withdrawals in this bucket, and
	// never dumps -- the same exclusion the ranking beside it applies, for
	// the same reason: a session that restarted would otherwise draw a
	// spike the network did not cause.
	Changes uint64 `json:"changes"`
}

// NewWirePeerChurn renders one ranked peer. windowSeconds is the length of
// the window the answer covers; a non-positive value yields a zero rate
// rather than a division by zero or an infinity no JSON number can carry.
func NewWirePeerChurn(
	p query.PeerChurn,
	windowSeconds float64,
	activity []query.ChurnPeerActivity,
) WirePeerChurn {
	return WirePeerChurn{
		// Non-nil always: [] is "this peer changed nothing in the window",
		// which is an answer, and null would read as "not asked".
		Activity: func() []WireChurnActivity {
			out := make([]WireChurnActivity, 0, len(activity))
			for _, a := range activity {
				out = append(out, WireChurnActivity{Bucket: stamp(a.Bucket), Changes: a.Changes})
			}
			return out
		}(),
		RouterIP:      addrText(p.RouterIP),
		RouterSysname: p.RouterSysname,
		PeerIP:        addrText(p.PeerIP),
		PeerASN:       p.PeerASN,
		Readvertise:   p.Readvertise,
		Withdraw:      p.Withdraw,
		Dump:          p.Dump,
		ChangesPerSecond: func() float64 {
			if windowSeconds <= 0 {
				return 0
			}
			return float64(p.Readvertise+p.Withdraw) / windowSeconds
		}(),
	}
}

// WirePrefixChurn is one row of /v1/collection/churn/prefixes: the
// most-changed prefixes over the window, ranked.
//
// ROUTE_UNICAST ONLY, and that is a property of what `prefix` can mean
// rather than a narrowing chosen for cost. A route_vpn prefix without its rd
// is ambiguous -- one string under two RDs is two different routes -- and a
// route_evpn type-2 MAC route has no IP prefix at all. Undecoded NLRI is
// excluded for the same reason: a row whose prefix could not be parsed has
// no prefix to be most-changed about, and ranked by change count those rows
// sort to the top under a blank label.
type WirePrefixChurn struct {
	Prefix string `json:"prefix"`
	// Observations is every archived row for this prefix in the window, and
	// equals readvertise + withdraw + dump.
	Observations uint64 `json:"observations"`
	Readvertise  uint64 `json:"readvertise"`
	Withdraw     uint64 `json:"withdraw"`
	Dump         uint64 `json:"dump"`
	// Routes is how many distinct routes carried this prefix, and Sessions
	// how many BMP sessions contributed. High observations over one route and
	// many sessions is a flapping session; over many routes and one session
	// it is a busy prefix.
	Routes   uint64 `json:"routes"`
	Sessions uint64 `json:"sessions"`
}

func NewWirePrefixChurn(p query.PrefixChurn) WirePrefixChurn {
	return WirePrefixChurn{
		Prefix:       p.Prefix,
		Observations: p.Observations,
		Readvertise:  p.Readvertise,
		Withdraw:     p.Withdraw,
		Dump:         p.Dump,
		Routes:       p.Routes,
		Sessions:     p.Sessions,
	}
}

// WireDumpTotals is meta.dump_totals on /v1/collection/dumps: the sum of
// Archived, Dumps and Changes across every router THIS ANSWER RETURNED --
// not across the fleet, which is a claim it can only make when limit= did
// not bite. See Meta's own doc comment for why a total lives here and not
// as a synthesized row in data.
type WireDumpTotals struct {
	Archived uint64 `json:"archived"`
	Dumps    uint64 `json:"dumps"`
	Changes  uint64 `json:"changes"`
}

// WireRouterSessionCount is the contract's RouterSessionCount schema.
type WireRouterSessionCount struct {
	RouterIP      string `json:"router_ip"`
	RouterSysname string `json:"router_sysname"`
	Sessions      uint64 `json:"sessions"`
	Up            uint64 `json:"up"`
	Down          uint64 `json:"down"`
	ViewLost      uint64 `json:"view_lost"`
}

func NewWireRouterSessionCount(r query.RouterSessionCount) WireRouterSessionCount {
	return WireRouterSessionCount{
		RouterIP:      addrText(r.RouterIP),
		RouterSysname: r.RouterSysname,
		Sessions:      r.Sessions,
		Up:            r.Up,
		Down:          r.Down,
		ViewLost:      r.ViewLost,
	}
}

// WireSessionTotals is meta.session_totals on /v1/collection/sessions.
type WireSessionTotals struct {
	Sessions uint64 `json:"sessions"`
	Up       uint64 `json:"up"`
	Down     uint64 `json:"down"`
	ViewLost uint64 `json:"view_lost"`
}

// WirePeerLocRIB is the contract's PeerLocRIB schema.
type WirePeerLocRIB struct {
	RouterIP string `json:"router_ip"`
	PeerIP   string `json:"peer_ip"`
	Reported uint64 `json:"reported"`
	Archived uint64 `json:"archived"`
	HasStat  bool   `json:"has_stat"`
}

func NewWirePeerLocRIB(r query.PeerLocRIB) WirePeerLocRIB {
	return WirePeerLocRIB{
		RouterIP: addrText(r.RouterIP),
		PeerIP:   addrText(r.PeerIP),
		Reported: r.Reported,
		Archived: r.Archived,
		HasStat:  r.HasStat,
	}
}

// WireLocRIBTotals is meta.locrib_totals on /v1/collection/locrib: the sum
// of Reported and Archived across every (router, peer) pair this answer
// returned. It is a sum over the same rows data carries, not a fleet-wide
// recomputation, so it inherits data's own busiest-first cap exactly as
// dump_totals and session_totals do.
type WireLocRIBTotals struct {
	Reported uint64 `json:"reported"`
	Archived uint64 `json:"archived"`
}

// WireFlagCount is the contract's FlagCount schema.
//
// It carries no router_ip, on purpose: see query.FlagCounts' own doc
// comment on why stream_seq identifies an envelope rather than a router's
// position, which is why ?router= narrows this endpoint's WHERE without
// becoming a dimension of its output.
type WireFlagCount struct {
	Flag      string `json:"flag"`
	Envelopes uint64 `json:"envelopes"`
}

func NewWireFlagCount(f query.FlagCount) WireFlagCount {
	return WireFlagCount{Flag: f.Flag, Envelopes: f.Envelopes}
}

// WireFlagTotals is meta.flag_totals on /v1/collection/flags: the sum of
// Envelopes across every flag this answer returned.
//
// It is NOT a count of distinct envelopes that raised a flag: an envelope
// raising two flags is counted once under each flag in data (see
// query.FlagCounts' own arrayJoin), and this total sums those rows exactly
// as they stand, so such an envelope contributes twice here as well. That
// matches what data itself shows rather than silently answering a
// different, deduplicated question data does not.
type WireFlagTotals struct {
	Envelopes uint64 `json:"envelopes"`
}

// WireCollector is the contract's Collector schema, GET /v1/collectors' own
// row: the union of what the archive currently knows about one collector
// id and what that id's own configured process currently says about
// itself.
//
// Archive and Status are two independent nullable facets, not one status
// flag -- see api/collectors.go's own doc comment for why the rows are a
// union rather than either side alone, and TestCollectorsUnionsTheArchiveAndTheConfig
// for the four combinations that actually reach the wire:
//
//   - Archive != nil, Status != nil: known to the archive, and its
//     configured process answered.
//   - Archive != nil, Status == nil, Reachable == nil: known to the
//     archive, no endpoint configured at all -- Endpoint, Error and
//     IDMismatch are all "" and Reachable stays nil, because reachability
//     is not a question with nothing configured to ask it of.
//   - Archive != nil, Status == nil, Reachable != nil && !*Reachable: known
//     to the archive, an endpoint IS configured, and it did not answer --
//     Error names why.
//   - Archive == nil: NOT known to the archive at all -- Config.Collectors
//     names this id but query.Collectors has never returned it. Status,
//     Reachable, Endpoint, Error and IDMismatch behave exactly as in the
//     three cases above; only the archive side is missing.
//
// Archive == nil is never rendered as zero routers and zero peers. A
// collector the archive has a real, empty view of cannot occur --
// query.Collectors never returns an entry with no routers (see its own doc
// comment) -- so a nil Archive here is unambiguous: the archive has no
// record of this id, full stop, not a record that happens to be empty.
type WireCollector struct {
	Collector string `json:"collector"`

	// Archive is nil when query.Collectors' result carries no entry for
	// this id -- never a *WireCollectorArchive with zero fields, which
	// would claim a real, empty view rather than no view at all.
	Archive *WireCollectorArchive `json:"archive"`

	// Status is nil whenever the daemon did not answer, for either reason:
	// no endpoint is configured (Reachable is then also nil), or one is
	// configured and it did not respond (Reachable is then false and Error
	// names why). It is non-nil only when the daemon actually answered.
	Status *WireCollectorStatus `json:"status"`

	// Reachable is nil exactly when Status is nil AND no endpoint is
	// configured -- reachability is not a question to ask of a collector
	// nobody told this daemon how to reach. true when the endpoint
	// answered (Status is then non-nil); false when one is configured and
	// did not answer (Error then names why). A null status paired with a
	// non-nil, false Reachable is "configured and silent"; a null status
	// paired with a nil Reachable is "not configured at all" -- the two
	// must never be collapsed into one rendering.
	Reachable *bool `json:"reachable"`

	// Error is why Status is nil despite an endpoint being configured --
	// unreachable, timed out, a non-200 response, or a body that did not
	// decode. "" whenever Reachable is not false.
	Error string `json:"error"`

	// Endpoint is the URL this daemon polled for Status. "" when no
	// endpoint is configured for this id.
	Endpoint string `json:"endpoint"`

	// IDMismatch is the CONFIGURED id, set only when the daemon answered
	// and reported a DIFFERENT collector_id than the id it is configured
	// under -- see CollectorStatus.IDMismatch. "" otherwise. Never resolved
	// to one side or the other here; the screen states the disagreement.
	IDMismatch string `json:"id_mismatch"`

	// Activity is rows archived per minute, oldest first, over the window
	// meta.activity_window names -- never a message count and never a
	// rate. Length is always window/1m: a collector query.CollectorActivity
	// returned no entry for -- because it archived zero rows anywhere in
	// the window -- still gets one entry per minute, every one Rows == 0,
	// rather than a shorter series or no series at all. The LAST entry is
	// the CURRENT, still-accumulating minute, not the last fully-elapsed
	// one; see (*query.Q).CollectorActivity's own doc comment.
	Activity []WireCollectorActivity `json:"activity"`
}

// WireCollectorArchive is what the archive currently knows about one
// collector: every router it currently monitors, its peers rolled up into
// totals, and when it last wrote anything to the archive at all. See
// query.CollectorSummary, which this is a direct copy of on the wire.
type WireCollectorArchive struct {
	Routers       []WireCollectorRouter `json:"routers"`
	PeersUp       int                   `json:"peers_up"`
	PeersDown     int                   `json:"peers_down"`
	PeersViewLost int                   `json:"peers_view_lost"`
	PeersStale    int                   `json:"peers_stale"`
	LastRowAt     time.Time             `json:"last_row_at"`
	// LastBeatAt and StartedAt are null, not the epoch, when no heartbeat
	// from this collector was ever received: "never heard from" and "heard
	// from in 1970" must stay distinguishable through JSON.
	LastBeatAt *time.Time `json:"last_beat_at"`
	StartedAt  *time.Time `json:"started_at"`
}

// WireCollectorRouter is the contract's CollectorRouter schema: one router
// as this card's collector currently sees it. See query.CollectorRouter,
// which carries the identical fields under the identical names this
// converter copies from.
type WireCollectorRouter struct {
	SysName       string `json:"sysname"`
	IP            string `json:"ip"`
	PeersUp       int    `json:"peers_up"`
	PeersDown     int    `json:"peers_down"`
	PeersViewLost int    `json:"peers_view_lost"`
	PeersStale    int    `json:"peers_stale"`
	// SysDescr is "" when never observed -- see query.CollectorRouter's own
	// doc comment. Never backfilled or defaulted here either.
	SysDescr string    `json:"sys_descr"`
	LastSeen time.Time `json:"last_seen"`
}

func NewWireCollectorRouter(r query.CollectorRouter) WireCollectorRouter {
	return WireCollectorRouter{
		SysName:       r.SysName,
		IP:            addrText(r.IP),
		PeersUp:       r.PeersUp,
		PeersDown:     r.PeersDown,
		PeersViewLost: r.PeersViewLost,
		PeersStale:    r.PeersStale,
		SysDescr:      r.SysDescr,
		LastSeen:      stamp(r.LastSeen),
	}
}

// WireCollectorStatus is the contract's CollectorStatus schema: one
// collector's own process facts, straight from its most recent /status
// answer. Present on the wire (WireCollector.Status non-nil) only when the
// daemon actually answered -- see status.Report, which carries the
// identical fields under the identical names this converter copies from.
//
// There is no lag field here, and there must never be one: the writer's
// vantage_sink_consumer_lag is per STREAM, shared across every collector
// that writer serves, so there is no per-collector lag this daemon could
// honestly attribute to the process these facts describe.
type WireCollectorStatus struct {
	StartedAt            time.Time `json:"started_at"`
	ObservedAt           time.Time `json:"observed_at"`
	SessionsActive       int64     `json:"sessions_active"`
	BMPMessagesTotal     uint64    `json:"bmp_messages_total"`
	EventsPublishedTotal uint64    `json:"events_published_total"`
	PublishErrorsTotal   uint64    `json:"publish_errors_total"`
	PublishRejectsTotal  uint64    `json:"publish_rejects_total"`
}

func NewWireCollectorStatus(rep status.Report) WireCollectorStatus {
	return WireCollectorStatus{
		StartedAt:            stamp(rep.StartedAt),
		ObservedAt:           stamp(rep.ObservedAt),
		SessionsActive:       rep.SessionsActive,
		BMPMessagesTotal:     rep.BMPMessagesTotal,
		EventsPublishedTotal: rep.EventsPublishedTotal,
		PublishErrorsTotal:   rep.PublishErrorsTotal,
		PublishRejectsTotal:  rep.PublishRejectsTotal,
	}
}

// WireCollectorActivity is one minute of a Collector health card's
// sparkline: rows archived, never a message count and never a rate -- see
// query.CollectorActivity's own doc comment for why a rate is not something
// this archive can honestly produce, and WireCollector.Activity for what
// the LAST entry in a series means.
type WireCollectorActivity struct {
	Minute time.Time `json:"minute"`
	Rows   uint64    `json:"rows"`
}

func NewWireCollectorActivity(a query.CollectorActivity) WireCollectorActivity {
	return WireCollectorActivity{Minute: stamp(a.Minute), Rows: a.Rows}
}

// --- /v1/asnames wire type ---

// WireASName is the contract's ASName schema: one requested AS number,
// paired with its registered holder or the explicit absence of one.
//
// Name is the WHOLE line query.ASName carries -- see that type's own doc
// comment for why it is never split into a handle and an organization, a
// rule this converter does not re-decide, only passes through. Name and
// Country are both "", and ONLY both together, when query.(*Q).ASNames
// carries no entry for this ASN: see that method's own three states.
//
// WHICH of those three states an empty pair means is NOT something
// WireASName itself can say -- that is meta.asnames_loaded's job, on the
// envelope this type's rows sit inside. This type's own guarantee is
// narrower and just as load-bearing: ASN is ALWAYS the value the caller
// asked about, present in the array once per distinct value requested,
// whether or not this dataset has ever heard of it -- see
// api/asnames.go's own handler for why an unlisted ASN still gets a row
// here rather than being left out of data entirely.
type WireASName struct {
	ASN     uint32 `json:"asn"`
	Name    string `json:"name"`
	Country string `json:"country"`
}

// NewWireASName pairs asn -- one value the caller actually asked about --
// with whatever query.(*Q).ASNames returned for it. n is the zero ASName
// (both fields "") for an asn that was not a key in that method's map,
// which is exactly the wire shape an unlisted ASN gets: see this
// function's own type for why that shape carries no ambiguity of its own.
func NewWireASName(asn uint32, n query.ASName) WireASName {
	return WireASName{ASN: asn, Name: n.Name, Country: n.Country}
}
