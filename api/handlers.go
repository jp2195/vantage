// Handlers: the thin layer between a query string and one query/ call.
//
// Every handler here has the same four steps in the same order -- parse the
// parameters, call exactly one query/ method, convert the rows with the
// matching newWireX, wrap the result in an Envelope -- and the uniformity
// is the design rather than a coincidence. Nothing in this file decides
// what "current" means, which session a walk is pinned to, or whether a
// route survives a withdrawal; all of that is query/'s, and a handler that
// started making those decisions would be a second, quieter definition of
// the same semantics for a dashboard to disagree with.
//
// # What a handler is allowed to know
//
// Two things, and they are the two query/ deliberately cannot do.
//
// The first is the contract's own vocabulary. ?rib= is validated HERE,
// against query.ValidRIB, because an unknown Enum8 member does not compare
// false in ClickHouse -- it raises, and that is a 500 for a failure
// api/openapi.yaml types as a 400. query.ribColumn's doc comment makes this
// split explicitly: a bad rib in a FILTER is the HTTP layer's to catch
// because the HTTP layer has the enum; a bad rib inside a CURSOR is query's
// to catch, because a cursor is opaque and no layer above can look into
// one. The same argument covers ?limit=, ?type= and ?since=: they are text
// until this file turns them into a number, a uint8 and a time.
//
// The second is page size, and only as a ceiling. clampRIBLimit already
// enforces query's own bound; this file exists to reject what the CONTRACT
// forbids -- limit=0 is not a small page, it is outside the schema's
// minimum, and passing it down would silently become the default page
// instead of an error.
//
// # Errors, and why every 400 is one line of switch
//
// Three sentinels reach a handler and each means something different to a
// caller. query.ErrBadFilter is a malformed QUESTION and its remedy is to
// fix the parameters. errBadCursor is a cursor string this daemon did not
// issue and its remedy is to drop ?cursor= and restart the walk.
// query.ErrSessionChanged is neither -- the question was fine and the
// cursor was ours, but the router reconnected underneath the walk, so the
// remedy is to restart it and the status is 409, not 400. Anything else is
// a 500 with one fixed sentence and a reference, and the error itself goes
// only to the log, under that reference: a ClickHouse error quotes the SQL
// that failed, and no response body may carry it.
//
// Nothing in an error body echoes caller-supplied text. Every 400 here is
// caused by something the caller typed, and quoting it back is how a
// reflected-content bug gets built by accident; the messages name the
// PARAMETER and the rule it broke instead, which is what an operator
// actually needs. TestErrorBodiesDoNotEchoTheRequest holds it.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jp2195/vantage/query"
)

// dumpStateDumping is the one value of query's DumpState vocabulary this
// file reacts to. The other two ("complete", "unknown") need no caveat:
// complete is the good case, and unknown means there is nothing to say --
// emitting a warning for it would train a reader to ignore the field.
const dumpStateDumping = "dumping"

// historyDefaultSince is api/openapi.yaml's documented default for ?since=
// on /v1/routes/history. It is a duration rather than an absolute time
// because that is what the contract writes ("1h"), and it is resolved
// against now() per request.
const historyDefaultSince = time.Hour

// params parses one request's query string, remembering the FIRST failure
// and returning zero values afterward.
//
// It accumulates rather than returning (value, error) per call so a handler
// can build a filter struct in one composite literal and check once. The
// alternative -- an if after every parameter -- puts five error branches in
// front of every handler's single interesting line, and the branch that
// gets forgotten is invisible: a handler that ignored one parse failure
// would silently query for the zero value, which for an address means "no
// filter" and returns the whole fleet where the caller asked for one
// router.
//
// The pattern is bufio.Scanner's: keep going, ask once at the end. Every
// method here must therefore be a no-op when err is already set, or the
// second failure would overwrite the first and the message would name the
// wrong parameter.
type params struct {
	v   url.Values
	err error
}

func newParams(r *http.Request) *params { return &params{v: r.URL.Query()} }

// fail records the first error. The format string and its arguments must
// never include a caller-supplied value; see this file's header.
func (p *params) fail(format string, args ...any) {
	if p.err == nil {
		p.err = fmt.Errorf(format, args...)
	}
}

// str returns a parameter verbatim. It cannot fail, so it does not consult
// p.err -- but it stays a method so that a handler reads the same way for
// every parameter.
func (p *params) str(name string) string { return p.v.Get(name) }

// addr parses an optional address parameter. An absent one is the invalid
// Addr, which every query filter reads as "not asked for".
//
// The address is UNMAPPED, and that is load-bearing rather than tidy:
// query.ribPage compares a cursor's scope against the address this handler
// passes it using netip.Addr !=, which compares an address's FORM as well
// as its value. api/cursor.go canonicalizes what it issues, so a request
// spelling ::ffff:10.0.0.1 would succeed on page 1 (filters unmap at bind
// time) and fail on page 2 against our own canonical cursor. That codec's
// doc comment names this the handler's half of the guarantee; this is it.
func (p *params) addr(name string) netip.Addr {
	if p.err != nil {
		return netip.Addr{}
	}
	raw := p.v.Get(name)
	if raw == "" {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(raw)
	if err != nil {
		p.fail("%s= is not an IP address", name)
		return netip.Addr{}
	}
	return a.Unmap()
}

// covers reads ?covers= and checks that it is an address, as addr checks
// router= and peer=, so that a bad one is refused with a message naming the
// PARAMETER. query's checkCovers makes the same check, but its message is
// written for a Go caller holding a filter struct, and before this existed
// it was the one every covers= 400 carried: the caller's own text quoted
// back, ParseAddr's error quoting it a second time, and the filter's Go
// field names. TestErrorBodiesDoNotEchoTheRequest holds the difference.
//
// It returns the text as given rather than the parsed Addr, because every
// query filter takes Covers as a string and renders it itself.
func (p *params) covers() string {
	if p.err != nil {
		return ""
	}
	raw := p.v.Get("covers")
	if raw == "" {
		return ""
	}
	a, err := netip.ParseAddr(raw)
	if err != nil {
		p.fail("covers= is not an IP address -- it takes the address a route " +
			"would have to contain, never a prefix; a prefix belongs in prefix=, " +
			"which matches it exactly")
		return ""
	}
	if a.Zone() != "" {
		p.fail("covers= carries an IPv6 zone -- a zone belongs to the asking " +
			"host's own interfaces, not to anything a router advertises, so it " +
			"would match no route at all")
		return ""
	}
	return raw
}

// requiredAddr is addr for the parameters api/openapi.yaml marks required
// on the three /v1/rib paths. Every one of them is a walk that needs both
// halves of its (router, peer) scope: without them there is no keyset to
// seek on, only a table.
//
// /v1/events used to be on this list too, back when router and peer were
// required there as well, and its trailing clause -- "an unscoped dump of
// the table is not what this path serves" -- was true of /v1/events for
// exactly that long. It stopped being true the moment the unscoped mode
// shipped: dropping BOTH is now a real, capped answer, not a refusal. Rather
// than reword this shared sentence (still exactly right for the three
// /v1/rib paths) handleEvents' own default branch calls
// requiredAddrWithHint below, keeping this message's "X= is required"
// prefix -- TestEventsStillRefusesAHalfScope pins that much -- while
// supplying a trailing clause that names /v1/events' own two valid shapes
// instead of this one's.
//
// The message says "this walk", not "a RIB walk": it used to name RIB
// specifically, back when requiredAddr had one family of callers, and
// handleEvents inherited that wording verbatim until it was caught here --
// an operator missing peer= on /v1/events would have been told about a RIB
// walk that was never involved.
func (p *params) requiredAddr(name string) netip.Addr {
	return p.requiredAddrWithHint(name,
		"this walk is scoped to one (router, peer), and an unscoped dump "+
			"of the table is not what this path serves")
}

// requiredAddrWithHint is requiredAddr with a caller-chosen trailing clause.
// requiredAddr itself supplies the /v1/rib clause; handleEvents' default
// branch is the other caller, supplying one that is still true of
// /v1/events now that dropping both router and peer answers a real,
// capped, fleet-wide question rather than refusing one.
func (p *params) requiredAddrWithHint(name, hint string) netip.Addr {
	if p.err != nil {
		return netip.Addr{}
	}
	if p.v.Get(name) == "" {
		p.fail("%s= is required -- %s", name, hint)
		return netip.Addr{}
	}
	return p.addr(name)
}

// rib validates ?rib= against the enum the rib column actually holds. See
// this file's header for why it is checked here and not below.
func (p *params) rib() string {
	if p.err != nil {
		return ""
	}
	raw := p.v.Get("rib")
	if raw == "" {
		return ""
	}
	if !query.ValidRIB(raw) {
		p.fail("rib= is not one of the BMP RIB views (%s)",
			strings.Join(query.RIBNames(), ", "))
		return ""
	}
	return raw
}

// limit resolves ?limit= against the contract's schema and the operator's
// configured ceiling.
//
// Absent takes cfg.DefaultPage. Present is REJECTED below the contract's
// minimum of 1 and CLAMPED above the maximum, and the asymmetry is the
// contract's: limit=0 is not a request for a small page, it is a value the
// schema excludes, and passing it down would become the default page rather
// than an error. A limit above the ceiling is an honest request for more
// rows than this surface gives, and the answer is the ceiling plus a cursor
// for the rest -- see query.clampRIBLimit, which makes the same argument
// about its own bound.
func (p *params) limit(cfg Config) int {
	if p.err != nil {
		return 0
	}
	raw := p.v.Get("limit")
	if raw == "" {
		return cfg.DefaultPage
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		p.fail("limit= is not an integer")
		return 0
	}
	if n < 1 {
		p.fail("limit= is below 1, which api/openapi.yaml gives as its minimum; " +
			"a page of zero rows is not a smaller page, it is no answer")
		return 0
	}
	return min(n, cfg.MaxPage)
}

// lsLimit is limit for a SCOPED link-state walk, and differs from it in
// exactly one place: an absent ?limit= here resolves to cfg.MaxPage, not
// cfg.DefaultPage. Format validation, the contract's minimum of 1 and the
// clamp at cfg.MaxPage are all limit's own and are not repeated here.
//
// The divergence is deliberate, not an inconsistency to fix later. Every
// /v1/rib path is already scoped on every request, so cfg.DefaultPage is
// the page size an operator tuned for a walk that was always going to
// page. /v1/ls/nodes, /v1/ls/links and /v1/ls/prefixes answer up to
// cfg.MaxPage today when no limit= is given at all -- see handleLSNodes's
// own doc comment on why an unscoped request is answered rather than
// refused -- and a caller that adds router= and peer= to narrow their
// question, gaining pagination in the bargain, must not be handed FEWER
// rows for doing so. Reusing limit's own cfg.DefaultPage default here
// would silently cut every existing scoped caller's answer from
// cfg.MaxPage to a tenth of it, a page-size change disguised as a
// feature. See query.LSNodesPage's own doc comment for the query layer's
// half of this same argument (clampRIBLimit takes DefaultRIBPage for an
// unset f.Limit, and choosing MaxRIBPage instead is left to the caller).
func (p *params) lsLimit(cfg Config) int {
	if p.err != nil {
		return 0
	}
	if p.v.Get("limit") == "" {
		return cfg.MaxPage
	}
	return p.limit(cfg)
}

// asn parses an AS number parameter. Absent is 0, meaning "not asked".
//
// An explicit 0 is REFUSED rather than treated as absent. AS 0 is reserved
// by RFC 7607 and originates nothing, so the only answers available are "not
// a question" and an empty set indistinguishable from a real absence -- and
// query/'s filter structs use 0 as their unset value, so an explicit 0 that
// reached them would silently become an unfiltered query.
func (p *params) asn(name string) uint32 {
	if p.err != nil {
		return 0
	}
	raw := p.v.Get(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		p.fail("%s= is not an AS number (a decimal 1-4294967295)", name)
		return 0
	}
	if n == 0 {
		p.fail("%s= is 0, which RFC 7607 reserves and no route originates; "+
			"an empty answer for it would be indistinguishable from a real "+
			"absence", name)
		return 0
	}
	return uint32(n)
}

// community validates ?community= at the boundary using query.ParseCommunity
// itself, not a boundary-specific rewrite of it -- p.err is assigned err
// verbatim below, so a bad notation reaches the caller as exactly the
// message ParseCommunity wrote, the same status and the same code a Go
// caller hitting query.RouteFilter.check would get for the same input. What
// parsing here buys is not a friendlier error, it is the parsed
// CommunityMatch: returning it lets the handler report its searched columns
// in meta without parsing the value a second time.
func (p *params) community() (string, query.CommunityMatch) {
	if p.err != nil {
		return "", query.CommunityMatch{}
	}
	raw := p.v.Get("community")
	if raw == "" {
		return "", query.CommunityMatch{}
	}
	m, err := query.ParseCommunity(raw)
	if err != nil {
		p.err = err
		return "", query.CommunityMatch{}
	}
	return raw, m
}

// evpnRouteType parses ?type= on /v1/routes/evpn. The range check is
// query.EVPNRouteFilter.check's, deliberately not duplicated here: this
// only has to produce a uint8, and a value past 255 must be refused HERE
// because the conversion would otherwise wrap into a legal-looking type.
func (p *params) evpnRouteType() uint8 {
	if p.err != nil {
		return 0
	}
	raw := p.v.Get("type")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 8)
	if err != nil {
		p.fail("type= is not an EVPN route type; it takes a number in 1-11, " +
			"IANA's own registry bound")
		return 0
	}
	return uint8(n)
}

// since resolves ?since= on /v1/routes/history: an RFC 3339 timestamp, or a
// duration back from now, defaulting to the contract's 1h.
//
// Both forms, because the two callers want different things and neither is
// served well by the other's. A dashboard refreshing a panel wants "the
// last hour" and would otherwise have to compute a timestamp on every
// refresh; a person correlating an incident has an absolute time from
// somewhere else and would otherwise have to convert it to an age.
//
// now is a parameter rather than a call to time.Now inside, so that the
// duration branch is testable without a clock.
func (p *params) since(now time.Time) time.Time {
	if p.err != nil {
		return time.Time{}
	}
	raw := p.v.Get("since")
	if raw == "" {
		return now.Add(-historyDefaultSince)
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.fail("since= is neither an RFC 3339 timestamp nor a duration " +
			`("1h", "30m", "24h")`)
		return time.Time{}
	}
	if d < 0 {
		// A negative duration would reach FORWARD from now, selecting a
		// window that ends before it starts -- always empty, and
		// indistinguishable from a prefix with no history.
		p.fail("since= is a negative duration; it names how far BACK to look")
		return time.Time{}
	}
	return now.Add(-d)
}

// cursor decodes ?cursor= into the value query.ribPage takes, or nil when
// the caller is asking for page 1.
//
// This is the entry point every /v1/rib/* and /v1/ls/* handler calls, and
// it goes through decodeCursor, which refuses a cursor with no session
// pin. See unpinnedCursor for the one endpoint that must not.
func (p *params) cursor() *query.RIBCursor {
	if p.err != nil {
		return nil
	}
	raw := p.v.Get("cursor")
	if raw == "" {
		return nil
	}
	c, err := decodeCursor(raw)
	if err != nil {
		// decodeCursor's own messages are written not to echo the cursor
		// (see its fail helper), so this one wraps rather than restates:
		// "version 2, but this build accepts 1" is the actionable half and
		// it is already there.
		p.err = err
		return nil
	}
	return &c
}

// unpinnedCursor is cursor, except it goes through decodeCursorUnpinned
// rather than decodeCursor and so accepts a cursor with no session pin --
// the shape query.PeerEventsPage issues, deliberately, because a peer's
// event history is itself the record of sessions starting and ending and a
// pin would stop the walk at the very transitions /v1/events exists to
// show. See api/cursor.go's header for the reasoning and decodeCursor's
// own doc comment for the endpoints that must NOT use this method.
//
// handleEvents is the one caller. Every other paginated handler must keep
// calling cursor instead -- TestHandlerRIBUnicastWalk's "a session-less
// cursor is still refused" subtest is what catches a copy-paste that
// reaches for this one by mistake.
func (p *params) unpinnedCursor() *query.RIBCursor {
	if p.err != nil {
		return nil
	}
	raw := p.v.Get("cursor")
	if raw == "" {
		return nil
	}
	c, err := decodeCursorUnpinned(raw)
	if err != nil {
		p.err = err
		return nil
	}
	return &c
}

// optU32 reads an optional unsigned parameter whose ZERO IS A VALUE. It
// returns nil when the parameter is absent and a pointer to the parsed value
// otherwise, including for "0".
//
// Absence is decided by url.Values.Has and never by the parsed number, which
// is the whole point. area=0 is the OSPF backbone and the most common area
// in the archive; a helper that returned a plain uint32 would make it
// indistinguishable from no filter, and the failure would be silent -- the
// caller gets every area and no error. See query.LSNodeFilter's own comment.
func (p *params) optU32(name string) *uint32 {
	if p.err != nil || !p.v.Has(name) {
		return nil
	}
	raw := p.v.Get(name)
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		p.fail("%s= is not a 32-bit unsigned integer (a decimal 0-4294967295)", name)
		return nil
	}
	v := uint32(n)
	return &v
}

// optU64 is optU32 for the u64 identity parameters -- node=, local_node= and
// remote_node=. They arrive as decimal text for the reason node_key leaves as
// a string: the value exceeds 2^53 and a JSON number would round it.
func (p *params) optU64(name string) *uint64 {
	if p.err != nil || !p.v.Has(name) {
		return nil
	}
	n, err := strconv.ParseUint(p.v.Get(name), 10, 64)
	if err != nil {
		p.fail("%s= is not a node key (a decimal 0-18446744073709551615, as a "+
			"string -- it is a 64-bit hash and does not survive a JSON number)",
			name)
		return nil
	}
	return &n
}

// protocol parses ?protocol=, accepting either notation, through
// query.ParseProtocol for the PARSING -- so "2" and "isis-l2" resolve the
// same way a Go caller's call to it would. The ERROR is this method's own
// rather than that parser's verbatim text: query.ParseProtocol's message
// quotes the value it was given ("protocol %q is neither..."), and this
// file's own header rule is that no 400 body echoes caller-supplied text.
// A Go caller of query.ParseProtocol is not bound by that rule; an HTTP
// caller of this method is, and it wins here. Absent is 0, which the
// filters read as "not asked".
//
// The message still names every accepted registry name, through
// query.KnownProtocolNames -- naming what IS accepted is not the same act as
// quoting what was SENT, and dropping the enumeration entirely (this
// method's first fix, made in response to the
// echo this comment describes above) was a worse defect than the one it
// replaced: api/openapi.yaml's ?protocol= parameter documents one example
// and no list, so a 400 body carrying none either would have left this
// project's lowercase-hyphenated names (isis-l2, not IANA's "IS-IS Level
// 2") undiscoverable anywhere a caller could reach. Calling
// query.KnownProtocolNames rather than copying its table here keeps the
// list a single source of truth: a name added to protocolNames reaches this
// message with no second edit.
func (p *params) protocol() uint8 {
	if p.err != nil {
		return 0
	}
	raw := p.v.Get("protocol")
	if raw == "" {
		return 0
	}
	id, err := query.ParseProtocol(raw)
	if err != nil {
		p.fail("protocol= is not a BGP-LS Protocol-ID: it takes a decimal "+
			"1-255 or one of these registry names (%s); 0 is not assigned",
			query.KnownProtocolNames())
		return 0
	}
	return id
}

// lsState parses ?state=, defaulting to live. An unrecognized value is a 400
// rather than a fall-through to the default: "livee" silently meaning "live"
// is how a typo becomes a wrong answer that looks right.
func (p *params) lsState() query.LSState {
	if p.err != nil {
		return query.LSStateLive
	}
	raw := p.v.Get("state")
	if raw == "" {
		return query.LSStateLive
	}
	s := query.LSState(raw)
	switch s {
	case query.LSStateLive, query.LSStateWithdrawn, query.LSStateAny:
		return s
	default:
		p.fail("state= must be one of live, withdrawn or any")
		return query.LSStateLive
	}
}

// requireNarrowing enforces one route endpoint's own narrowing rule: at
// least one of names must be present, and prefix= and covers= -- wherever
// an endpoint documents both -- stay mutually exclusive with each other.
//
// It used to be exactlyOneTarget: prefix XOR covers, no exceptions. The wide
// filters are the reason it changed. Each of them bounds an answer as well
// as a prefix does, so refusing a query that carries one would be refusing
// the question the slice exists to make askable. What has NOT been relaxed
// is the pairing: prefix= and covers= together still ask for an exact prefix
// that also contains an address, whose empty answer is indistinguishable
// from the prefix being nowhere in the fleet.
//
// names is supplied per call site rather than shared across all four route
// endpoints, because each endpoint reads a different subset of the
// contract's parameters and narrows on a different subset of THAT. One
// list shared by all four used to gate this: it happened to equal what
// VPNRouteFilter.check and EVPNRouteFilter.check already treat as
// narrowing, so /v1/routes/vpn and /v1/routes/evpn were never wrong, but it
// also let rd= satisfy the gate on /v1/routes/unicast, whose RouteFilter
// has no RD field at all -- and RouteFilter.check has no narrowing rule of
// its own to catch what slipped through (a wide filter alone is
// deliberately enough there; see RouteFilter.Prefix's own doc comment), so
// the request reached query as an entirely unfiltered RouteFilter and
// matched the literal empty prefix route_unicast has never held: HTTP 200,
// data: [], indistinguishable from a real absence. See
// TestRDOnUnicastIsRefusedNotSilentlyEmpty, which also covers router= alone
// on the same endpoint -- a real, documented parameter there, but not one
// the contract's narrowing rule has ever included.
//
// The unbounded case is checked here as well as in query/ because the two
// layers refuse it for different reasons -- this one against a documented
// parameter list, that one against a filter struct a Go caller can build
// directly.
func (p *params) requireNarrowing(endpoint string, names ...string) {
	if p.err != nil {
		return
	}
	p.requirePrefixCoversExclusive(endpoint)
	if p.err != nil {
		return
	}
	for _, name := range names {
		if p.v.Get(name) != "" {
			return
		}
	}
	p.fail("%s needs at least one of %s -- with none of them the answer is "+
		"every route in the fleet, which is what the paginated /v1/rib paths "+
		"are for", endpoint, joinWithOr(names))
}

// requirePrefixCoversExclusive is the half of the narrowing rule that every
// endpoint documenting BOTH prefix= and covers= shares, whatever its own
// sufficiency rule is: the two stay mutually exclusive, because together
// they ask for an exact prefix that also contains an address, and that
// question's empty answer is indistinguishable from the prefix being nowhere
// in the fleet.
//
// It is shared where the SUFFICIENCY halves deliberately are not, and the
// line between the two is the message's remedy rather than the shape of the
// check. requireNarrowing's sufficiency message ends "which is what the
// paginated /v1/rib paths are for", which is true of the four route
// endpoints and false of /v1/topology -- no /v1/rib path answers what shape
// a prefix is reached by -- so requireTopologyScope writes its own. This
// half names no remedy at all and is already parameterized by endpoint, so
// one helper is identical by construction where two copies were identical
// only until someone edited one of them. It was two copies, briefly, with a
// comment asserting the equality and nothing enforcing it.
//
// It sets p.err rather than returning a bool for the reason every other
// method here does: the caller checks p.err once, and a second return
// convention in this file would be a second thing to get wrong.
func (p *params) requirePrefixCoversExclusive(endpoint string) {
	if p.err != nil {
		return
	}
	if p.v.Get("prefix") != "" && p.v.Get("covers") != "" {
		p.fail("%s takes exactly one of prefix= or covers=, not both -- together "+
			"they ask for an exact prefix that also contains an address", endpoint)
	}
}

// joinWithOr renders a parameter-name list the way every narrowing 400 here
// spells one: "prefix=, covers= or origin_asn=", not a bare comma list.
func joinWithOr(names []string) string {
	items := make([]string, len(names))
	for i, n := range names {
		items[i] = n + "="
	}
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// writeJSON renders one response body.
//
// The Content-Type is set before WriteHeader because a header written after
// the status line is dropped, and the encode error is dropped because by
// then the status and some bytes are already on the wire: there is no
// second status to send, and the only cause is a client that hung up.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders the contract's ErrorResponse.
func (s *Server) writeError(w http.ResponseWriter, status int, code, msg string) {
	s.writeJSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: msg}})
}

// badParam answers a parameter that failed to parse or failed the
// contract's own rule.
func (s *Server) badParam(w http.ResponseWriter, err error) {
	s.writeError(w, http.StatusBadRequest, ErrInvalidParam, err.Error())
}

// fail maps an error out of query/ or the cursor codec onto a status.
//
// The three 400-and-409 sentinels are matched with errors.Is rather than on
// message text, which is why they are sentinels; see errBadCursor's doc
// comment for why a malformed cursor is its own sentinel rather than
// query.ErrBadFilter, even though both land on 400. Their sentences are
// written for the caller and go out unchanged.
//
// Anything else is a 500, and the error itself never reaches the body. A
// ClickHouse error quotes the statement that failed, SQL and all, and
// redact.Err, which strips credentials from a URL, does nothing about that.
// So the body carries one fixed sentence and a fresh reference, and the
// UNREDACTED error goes to the log under the same reference: an operator
// reading the log is inside the trust boundary and needs the driver's own
// words, and a caller quoting the reference leads them to the right line.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, query.ErrBadFilter), errors.Is(err, errBadCursor):
		s.badParam(w, err)
	case errors.Is(err, query.ErrSessionChanged):
		s.writeError(w, http.StatusConflict, ErrSessionChanged, err.Error())
	default:
		ref := errorRef()
		s.logger.Error("query failed",
			"path", r.URL.Path, "ref", ref, "err", err)
		s.writeError(w, http.StatusInternalServerError, ErrInternal,
			"internal error; the server log records the cause under ref="+ref)
	}
}

// errorRef returns a fresh opaque reference for one 500: 16 hex digits from
// crypto/rand. It exists because nothing upstream of fail assigns a request
// id to reuse, and a generic body with nothing to quote leaves an operator
// no way to find the log line a caller's failure produced. Random rather
// than a counter, so it says nothing about request volume.
func errorRef() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return hex.EncodeToString(b[:])
}

// dumpingWarning is meta.warnings' session_dumping entry, or nil when no
// contributing session was mid-dump.
//
// nil rather than an empty Warnings because Warnings.MarshalJSON already
// renders nil as [], which is the contract's positive claim that nothing
// applied. Building the empty slice here would say the same thing in a way
// a future refactor could drop.
func dumpingWarning(dumping bool) Warnings {
	if !dumping {
		return nil
	}
	return Warnings{{
		Code: WarnSessionDumping,
		Message: "at least one contributing peer is still sending its initial " +
			"RIB dump, so this answer is a partial view of that peer's routes " +
			"rather than its full table",
	}}
}

// StaleWarning is meta.warnings' collector_stale entry, or nil when no row
// came from a stale collector. after is the threshold the answer was read
// with, so the message names the number the operator configured.
func StaleWarning(stale bool, after time.Duration) Warnings {
	if !stale {
		return nil
	}
	return Warnings{{
		Code: WarnCollectorStale,
		Message: fmt.Sprintf("at least one row in this answer comes from a collector "+
			"that has not been heard from in over %s, so it is that collector's last "+
			"view rather than a current one; its peers read \"stale\"", after),
	}}
}

// staleScopeWarning is StaleWarning for an answer that merges collectors and
// cannot name the rows a stale collector contributed: /v1/topology's graphs.
func staleScopeWarning(stale bool, after time.Duration) Warnings {
	if !stale {
		return nil
	}
	return Warnings{{
		Code: WarnCollectorStale,
		Message: fmt.Sprintf("this answer may include routes from a collector that has "+
			"not been heard from in over %s: a graph merges collectors, so which "+
			"routes are that collector's last view rather than a current one cannot "+
			"be told apart here", after),
	}}
}

// staleSet reads the (collector, router) pairs whose current session is
// stale, for an answer with rows to check. An answer with none costs no
// statement and never warns.
//
// It runs after the answer's own statements, not with them: a collector that
// crosses the threshold between the two is misreported in that one answer,
// and the next one is right.
func (s *Server) staleSet(ctx context.Context, rows int) (query.StaleSet, error) {
	if rows == 0 {
		return nil, nil
	}
	return s.q.StalePairs(ctx)
}

// anyPeerStale, anyRouterStale and anyCollectorStale answer query.AnyStale's
// question for the three answers that already carry each row's resolved
// state, and so need no second statement.
func anyPeerStale(peers []query.Peer) bool {
	for _, p := range peers {
		if p.State == query.PeerStateStale {
			return true
		}
	}
	return false
}

func anyRouterStale(routers []query.Router) bool {
	for _, r := range routers {
		if r.PeersStale > 0 {
			return true
		}
	}
	return false
}

func anyCollectorStale(summaries []query.CollectorSummary) bool {
	for _, c := range summaries {
		if c.PeersStale > 0 {
			return true
		}
	}
	return false
}

// truncatedWarning says an answer was capped, and says by how much. The
// numbers are the point: "truncated" alone invites a reader to treat the rows
// returned as the rows that exist, which is the failure meta.total_matched
// exists to prevent.
func truncatedWarning(total uint64, returned int) Warning {
	return Warning{
		Code: WarnTruncated,
		Message: fmt.Sprintf("%d rows matched and %d were returned; narrow the "+
			"query to see the rest -- this answer is a sample, not a total",
			total, returned),
	}
}

// smearWarning is the caveat every /v1/rib page carries, unconditionally.
//
// Unconditional because the property it reports is structural, not
// occasional: a walk is a sequence of statements against a live table, and
// only the session is pinned. Emitting it only when a change was detected
// would be a stronger claim than this API can make -- nothing here watches
// the table between pages -- and a caveat that appears sometimes reads as a
// detection rather than a disclosure.
func smearWarning() Warning {
	return Warning{
		Code: WarnPaginatedSmear,
		Message: "a paginated walk is a smear over a live table, not a snapshot: " +
			"the session is pinned, but rows may be added or withdrawn between " +
			"pages",
	}
}

// anyDumping reports whether any row's own dump state says its family's
// initial dump is still in progress.
//
// It takes an accessor rather than an interface because the three route
// types do not share one: they embed WireRouteCommon on the WIRE, but
// query.Route, query.VPNRoute and query.EVPNRoute are three structs with
// three DumpState fields and no method in common. An interface would mean
// adding one to query/ purely so this file could avoid a closure.
func anyDumping[T any](rows []T, state func(T) string) bool {
	for _, r := range rows {
		if state(r) == dumpStateDumping {
			return true
		}
	}
	return false
}

// anyPeerDumping is anyDumping for the OTHER shape a dump state comes in.
//
// query.Peer carries DumpStates -- a map from family to that family's own
// progress -- rather than the single string every route type carries, and
// the difference is not cosmetic. A peer's dump progress is per family:
// one that has finished its ipv4u dump may still be dumping vpn4, and
// collapsing that to one string is what the map replaced. So this is a
// separate function rather than an accessor into anyDumping, and it has a
// separate test: a check written for the route shape does not fail on this
// one, it simply never fires, and /v1/peers would ship with the warning
// silently missing.
func anyPeerDumping(peers []query.Peer) bool {
	for _, p := range peers {
		for _, state := range p.DumpStates {
			if state == dumpStateDumping {
				return true
			}
		}
	}
	return false
}

// mapRows converts a page of query rows into their wire form, always
// returning a non-nil slice so that an empty answer marshals as [].
func mapRows[T, W any](rows []T, convert func(T) W) []W {
	out := make([]W, len(rows))
	for i, r := range rows {
		out[i] = convert(r)
	}
	return arrayOf(out)
}

// handleAuthConfig tells a browser how to authenticate, before it can.
//
// It is one of two public operations in the contract, and it carries no
// secret by construction: a mode name, and later an issuer and client id,
// all of which are published to every user of an OIDC deployment anyway.
// Nothing from cfg.Tokens is reachable from here, and the test
// TestAuthConfigLeaksNoToken is what keeps that true.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	mode := s.cfg.Auth.Mode
	if mode == "" {
		mode = AuthModeToken
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Mode string `json:"mode"`
	}{Mode: string(mode)}); err != nil {
		s.logger.Error("encoding auth config", "err", err)
	}
}

func (s *Server) handleRouters(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.Routers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// No session_dumping here, and no accident: query.Router carries no
	// dump state at all. It is an inventory of routers and their peer
	// counts, and dump progress is a property of a (peer, family) -- the
	// grain /v1/peers reports it at. Staleness is a property of the router
	// row's own collector, which the row does carry.
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireRouter),
		Meta: Meta{Warnings: StaleWarning(anyRouterStale(rows), s.q.StaleAfter())},
	})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	f := query.PeerFilter{Router: p.addr("router"), RIB: p.rib()}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, err := s.q.Peers(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWirePeer),
		Meta: Meta{Warnings: append(dumpingWarning(anyPeerDumping(rows)),
			StaleWarning(anyPeerStale(rows), s.q.StaleAfter())...)},
	})
}

// handleRoutes is the looking-glass fan-out: one target, all three
// families, three arrays under three required keys.
//
// The three queries run concurrently. That is worth the errgroup here and
// nowhere else in this file: covers= is a full scan by construction
// (measured 31-52ms over 3.2M rows, per the contract), and running three
// sequentially would make the one endpoint a person waits on three times
// the cost of the one they do not.
//
// A failure in ANY family fails the whole request rather than returning the
// two that worked. The contract makes all three keys required precisely so
// a caller never has to distinguish "no results" from "not searched", and
// returning a partial fan-out under those required keys would reintroduce
// exactly that ambiguity in its worst form -- silently, with a 200.
func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	p.requireNarrowing("/v1/routes", "prefix", "covers", "origin_asn", "through_asn", "community")
	prefix, covers := p.str("prefix"), p.covers()
	router, peer, rib := p.addr("router"), p.addr("peer"), p.rib()
	originASN, throughASN := p.asn("origin_asn"), p.asn("through_asn")
	comm, match := p.community()
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	uf := query.RouteFilter{
		Prefix: prefix, Covers: covers, Router: router, Peer: peer, RIB: rib,
		OriginASN: originASN, ThroughASN: throughASN, Community: comm,
		Limit: s.cfg.MaxPage,
	}
	vf := query.VPNRouteFilter{
		Prefix: prefix, Covers: covers, Router: router, Peer: peer, RIB: rib,
		OriginASN: originASN, ThroughASN: throughASN, Community: comm,
		Limit: s.cfg.MaxPage,
	}
	ef := query.EVPNRouteFilter{
		Prefix: prefix, Covers: covers, Router: router, Peer: peer, RIB: rib,
		OriginASN: originASN, ThroughASN: throughASN, Community: comm,
		Limit: s.cfg.MaxPage,
	}

	var (
		unicast                           []query.Route
		vpn                               []query.VPNRoute
		evpn                              []query.EVPNRoute
		unicastTotal, vpnTotal, evpnTotal uint64
	)
	g, ctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) {
		unicast, err = s.q.Routes(ctx, uf)
		return err
	})
	g.Go(func() (err error) {
		vpn, err = s.q.VPNRoutes(ctx, vf)
		return err
	})
	g.Go(func() (err error) {
		evpn, err = s.q.EVPNRoutes(ctx, ef)
		return err
	})
	g.Go(func() (err error) {
		unicastTotal, err = s.q.CountRoutes(ctx, uf)
		return err
	})
	g.Go(func() (err error) {
		vpnTotal, err = s.q.CountVPNRoutes(ctx, vf)
		return err
	})
	g.Go(func() (err error) {
		evpnTotal, err = s.q.CountEVPNRoutes(ctx, ef)
		return err
	})
	if err := g.Wait(); err != nil {
		s.fail(w, r, err)
		return
	}

	dumping := anyDumping(unicast, func(x query.Route) string { return x.DumpState }) ||
		anyDumping(vpn, func(x query.VPNRoute) string { return x.DumpState }) ||
		anyDumping(evpn, func(x query.EVPNRoute) string { return x.DumpState })
	// total_matched here is the sum across all three families, which is the
	// number a caller of a fan-out is asking about: "how many routes exist
	// for this target", not "how many exist in whichever family happened to
	// be first". Each arm now carries a Limit of s.cfg.MaxPage, exactly the
	// cap the three single-family handlers apply, so this sum CAN legitimately
	// exceed what came back -- an uncapped fan-out once answered the slice's
	// headline question ("what does this AS originate") in full on every
	// call, which measured ~1.0s/6.9GiB and 1,050,000 rows matched at 3.2M
	// rows in the table, run three times concurrently. See
	// TestFanoutTruncatesAndCarriesTheHonestTotal.
	total := unicastTotal + vpnTotal + evpnTotal
	set, err := s.staleSet(r.Context(), len(unicast)+len(vpn)+len(evpn))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stale := query.AnyStale(set, unicast, query.Route.StaleKey) || query.AnyStale(set, vpn, query.VPNRoute.StaleKey) || query.AnyStale(set, evpn, query.EVPNRoute.StaleKey)
	meta := Meta{TotalMatched: &total,
		Warnings: append(dumpingWarning(dumping), StaleWarning(stale, s.q.StaleAfter())...)}
	if comm != "" {
		meta.CommunityColumns = match.SearchedColumns()
	}
	if returned := len(unicast) + len(vpn) + len(evpn); total > uint64(returned) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, returned))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: NewWireRouteFanout(
			mapRows(unicast, NewWireUnicastRoute),
			mapRows(vpn, NewWireVPNRoute),
			mapRows(evpn, NewWireEVPNRoute),
		),
		Meta: meta,
	})
}

// rowsAndTotal runs a handler's page query and its matching count
// concurrently and returns both. The two are independent statements over the
// same filter -- neither reads the other's result -- so the serial form this
// replaces spent the SUM of two latencies to produce an answer that was
// available in the MAX of them.
//
// It matters at the table size the cap exists for rather than at today's.
// Measured 2026-08-30, a single wide-filter query took roughly a second over a
// 3.2M-row table, flat across a 100x spread in selectivity, because the
// HAVING runs after the whole GROUP BY regardless of how many rows survive it.
// The count re-runs that same GROUP BY and HAVING without the wide SELECT
// list, and measured 650-800 ms cheaper than the page (roughly 250-375 ms
// against ~1 s), so running the two in sequence added about a third to the
// endpoint's latency rather than a rounding error -- and total_matched is not
// optional, since it is the only thing that makes a capped answer honest.
//
// An errgroup rather than a bare pair of goroutines, for the reason
// handleRoutes already uses one: the first error cancels the sibling instead
// of leaving a second query running against ClickHouse for a result no one
// will read.
func rowsAndTotal[T any](
	ctx context.Context,
	rowsFn func(context.Context) ([]T, error),
	countFn func(context.Context) (uint64, error),
) ([]T, uint64, error) {
	var (
		rows  []T
		total uint64
	)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { rows, err = rowsFn(ctx); return err })
	g.Go(func() (err error) { total, err = countFn(ctx); return err })
	return rows, total, g.Wait()
}

func (s *Server) handleUnicastRoutes(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	p.requireNarrowing("/v1/routes/unicast", "prefix", "covers", "origin_asn", "through_asn", "community")
	comm, match := p.community()
	f := query.RouteFilter{
		Prefix:     p.str("prefix"),
		Covers:     p.covers(),
		Family:     p.str("family"),
		Router:     p.addr("router"),
		Peer:       p.addr("peer"),
		RIB:        p.rib(),
		OriginASN:  p.asn("origin_asn"),
		ThroughASN: p.asn("through_asn"),
		Community:  comm,
		Limit:      s.cfg.MaxPage,
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.Route, error) { return s.q.Routes(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountRoutes(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{
		TotalMatched: &total,
		Warnings: append(dumpingWarning(
			anyDumping(rows, func(x query.Route) string { return x.DumpState })),
			StaleWarning(query.AnyStale(set, rows, query.Route.StaleKey), s.q.StaleAfter())...),
	}
	if comm != "" {
		meta.CommunityColumns = match.SearchedColumns()
	}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireUnicastRoute),
		Meta: meta,
	})
}

// handleVPNRoutes reads no covers=. api/openapi.yaml does not document one
// on this path -- containment is offered by /v1/routes, which fans out to
// this same filter -- and accepting an undocumented parameter that WORKS is
// how a contract quietly grows a second, untested surface.
func (s *Server) handleVPNRoutes(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	p.requireNarrowing("/v1/routes/vpn", "prefix", "rd", "router", "origin_asn", "through_asn", "community")
	comm, match := p.community()
	f := query.VPNRouteFilter{
		Prefix:     p.str("prefix"),
		RD:         p.str("rd"),
		Family:     p.str("family"),
		Router:     p.addr("router"),
		Peer:       p.addr("peer"),
		RIB:        p.rib(),
		OriginASN:  p.asn("origin_asn"),
		ThroughASN: p.asn("through_asn"),
		Community:  comm,
		Limit:      s.cfg.MaxPage,
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.VPNRoute, error) { return s.q.VPNRoutes(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountVPNRoutes(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{
		TotalMatched: &total,
		Warnings: append(dumpingWarning(
			anyDumping(rows, func(x query.VPNRoute) string { return x.DumpState })),
			StaleWarning(query.AnyStale(set, rows, query.VPNRoute.StaleKey), s.q.StaleAfter())...),
	}
	if comm != "" {
		meta.CommunityColumns = match.SearchedColumns()
	}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireVPNRoute),
		Meta: meta,
	})
}

// handleEVPNRoutes reads no covers=, for the reason handleVPNRoutes does.
func (s *Server) handleEVPNRoutes(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	p.requireNarrowing("/v1/routes/evpn", "prefix", "rd", "router", "origin_asn", "through_asn", "community")
	comm, match := p.community()
	f := query.EVPNRouteFilter{
		Prefix:     p.str("prefix"),
		RD:         p.str("rd"),
		RouteType:  p.evpnRouteType(),
		Router:     p.addr("router"),
		Peer:       p.addr("peer"),
		RIB:        p.rib(),
		OriginASN:  p.asn("origin_asn"),
		ThroughASN: p.asn("through_asn"),
		Community:  comm,
		Limit:      s.cfg.MaxPage,
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.EVPNRoute, error) { return s.q.EVPNRoutes(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountEVPNRoutes(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{
		TotalMatched: &total,
		Warnings: append(dumpingWarning(
			anyDumping(rows, func(x query.EVPNRoute) string { return x.DumpState })),
			StaleWarning(query.AnyStale(set, rows, query.EVPNRoute.StaleKey), s.q.StaleAfter())...),
	}
	if comm != "" {
		meta.CommunityColumns = match.SearchedColumns()
	}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireEVPNRoute),
		Meta: meta,
	})
}

// handleHistory carries no session_dumping warning, and that is a positive
// decision rather than an omission. query.HistoryEvent has no dump state:
// history is the one surface that reports raw events rather than current
// state, so "the dump is incomplete" has nothing to qualify -- the events
// that have arrived are exactly the events being reported.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	if p.str("prefix") == "" {
		s.badParam(w, errors.New("prefix= is required on /v1/routes/history -- "+
			"a timeline is scoped to one exact prefix, and an unscoped one would "+
			"be every event in the retention window"))
		return
	}
	f := query.HistoryFilter{
		Prefix: p.str("prefix"),
		Since:  p.since(time.Now()),
		Router: p.addr("router"),
		Peer:   p.addr("peer"),
		RIB:    p.rib(),
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, err := s.q.RouteHistory(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireHistoryEvent),
	})
}

// errEventsCursorNeedsScope refuses ?cursor= on an unscoped /v1/events
// request. It is a package-level value rather than an inline string for
// errLSCursorNeedsScope's own reason: a message that exists once cannot
// drift from itself.
//
// The unscoped mode is a capped list, not a walk -- it hands back no cursor
// at all (see handleEvents) -- so a cursor arriving without a scope was
// either issued for a different walk or invented. Neither can be honored,
// and reinterpreting one against a fleet-wide read would silently skip or
// repeat rows.
var errEventsCursorNeedsScope = errors.New(
	"cursor= needs router= and peer=: an unscoped events query is a capped " +
		"list rather than a walk and issues no cursor, so a cursor here belongs " +
		"to a different question")

// handleEvents serves /v1/events in two modes on one path, plus the 400 that
// is neither.
//
// SCOPED (router AND peer): one peer's history, keyset-ordered on the
// collector clock, cursor and all. See query.PeerEventsPage. Its since= is
// NOT clamped: scoped, a wide window is still a seek, and refusing to
// answer is not the same as explaining why an answer is empty, which
// stands for this mode.
//
// UNSCOPED (neither): the fleet console's question -- what happened anywhere,
// newest first -- answered as a capped list with meta.total_matched, exactly
// the shape /v1/ls/nodes already uses for its own unscoped request, and for
// the same reason: there is no keyset to seek on, so a cursor would offer a
// page 2 whose cost is identical to page 1's. Its since= IS clamped, and
// that reversal is measured rather than stylistic -- the 2026-09-08
// measurement (query.UnscopedEventsMeasurement) puts a wide unscoped window at 237-289ms and 2.4-3.6 GB resident on a two-million-row
// archive, growing with the archive, reachable from one query string.
//
// A HALF SCOPE is a 400, and this is where this handler deliberately parts
// company with handleLSNodes. lsScoped() is `router.IsValid() &&
// peer.IsValid()`, so /v1/ls/nodes?router=X is answered UNSCOPED and filtered
// to that router. Doing the same here would turn an operator's forgotten
// peer= from an error into a fleet-wide answer they did not ask for and
// cannot tell apart from the one they wanted. The "X= is required" PREFIX
// stays requiredAddr's, which is why the parameter below is re-read through
// requiredAddrWithHint rather than a scope helper: the 400's shape -- which
// parameter it names -- must not change because a second mode was added
// beside it. Its trailing clause does change: requiredAddr's own ("an
// unscoped dump of the table is not what this path serves") stopped being
// true here the day the unscoped mode shipped, so this handler supplies one
// naming its own two valid shapes instead. See requiredAddr's doc comment.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	now := time.Now()
	router, peer := p.addr("router"), p.addr("peer")

	switch {
	case router.IsValid() && peer.IsValid():
		s.eventsScoped(w, r, p, now, router, peer)
	case !router.IsValid() && !peer.IsValid():
		s.eventsUnscoped(w, r, p, now)
	default:
		// Exactly one half named. Re-read the missing one so the body names
		// it, with a trailing clause specific to this path -- see this
		// function's own doc comment on why requiredAddr's is no longer
		// accurate here.
		if !router.IsValid() {
			p.requiredAddrWithHint("router", eventsHalfScopeHint)
		} else {
			p.requiredAddrWithHint("peer", eventsHalfScopeHint)
		}
		s.badParam(w, p.err)
	}
}

// eventsHalfScopeHint is the trailing clause handleEvents' default branch
// gives requiredAddrWithHint. A package-level value for the same reason
// errEventsCursorNeedsScope is one: a message that exists once cannot drift
// from itself between the router= and peer= cases above.
const eventsHalfScopeHint = "/v1/events answers a scoped (router, peer) " +
	"walk or an unscoped, capped fleet-wide question; naming only one is neither"

// eventsScoped is /v1/events' original body, moved and otherwise untouched.
// See handleEvents for the mode split and query.PeerEventsPage for why the
// cursor is unpinned.
//
// Like handleHistory and unlike ribPage, this carries no session_dumping or
// paginated_smear warning. query.PeerEvent has no dump state to qualify, and
// the smear ribPage warns about is a RIB row's own STATE changing under a
// walk ordered on identity; this walk is ordered on ts_collector and moves
// backward from a fixed instant, so a row written after the walk began has a
// newer ts_collector than anything already paged and cannot land between two
// pages already fetched.
func (s *Server) eventsScoped(w http.ResponseWriter, r *http.Request, p *params, now time.Time, router, peer netip.Addr) {
	f := query.PeerEventFilter{
		Router: router,
		Peer:   peer,
		RIB:    p.rib(),
		Since:  p.since(now),
		Cursor: p.unpinnedCursor(),
		Limit:  p.limit(s.cfg),
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	rows, next, err := s.q.PeerEventsPage(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var token *string
	if next != nil {
		t := encodeCursor(*next)
		token = &t
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWirePeerEvent),
		Meta: Meta{NextCursor: token, Warnings: Warnings{}},
	})
}

// eventsUnscoped answers the fleet-wide question: capped, no cursor,
// total_matched, and a window bounded at both ends.
func (s *Server) eventsUnscoped(w http.ResponseWriter, r *http.Request, p *params, now time.Time) {
	if p.unpinnedCursor() != nil {
		s.badParam(w, errEventsCursorNeedsScope)
		return
	}
	since := p.since(now)
	f := query.FleetEventFilter{
		RIB:   p.rib(),
		Since: since,
		Limit: p.limit(s.cfg),
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}
	if err := s.checkUnscopedWindow(since, now, unscopedEventsHint); err != nil {
		s.badParam(w, err)
		return
	}

	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.PeerEvent, error) { return s.q.FleetEvents(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountFleetEvents(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}

	meta := Meta{TotalMatched: &total, Warnings: Warnings{}}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWirePeerEvent),
		Meta: meta,
	})
}

// checkUnscopedWindow refuses an unscoped window wider than the operator's
// configured maximum, and the refusal states what an operator needs: it
// names the limit, so an operator learns what to raise, and it names the
// measurement, so a later reader finds it instead of assuming the number
// is arbitrary.
//
// It quotes cfg.MaxUnscopedSince and never the caller's own since=. Both
// halves are true statements, but this file's header forbids echoing
// caller-supplied text into an error body, and the limit is the half that
// tells an operator what to do.
//
// hint IS THE REST OF THE SENTENCE, and it is a parameter rather than fixed
// text because this function guards two endpoint families whose remediations
// are not the same and whose measurements are not the same. The shared half
// is the limit's value and the max_unscoped_since key name -- the half that
// tells an operator what to raise, and the half that must never differ
// between two paths enforcing one configured number. Everything downstream
// of that -- which table the wide read lands on, which measurement covers it,
// and what the caller can do instead -- belongs to the caller, because an
// answer that is true of /v1/events is provably false of /v1/collection/*:
// there is no peer= on those four paths at all, and router= grants no
// exemption from this clamp (see collectionFilter's own doc comment). A
// shared tail told an operator to do something that returns the identical
// 400, which is worse than saying nothing.
//
// The two hints live at their own call sites -- unscopedEventsHint and
// unscopedCollectionHint -- rather than here, so that adding a third guarded
// family cannot silently inherit a remediation that does not apply to it.
//
// A since= in the FUTURE (an RFC 3339 timestamp later than now) yields a
// negative age and passes: it selects an empty window, not an expensive one,
// and this function exists to bound cost.
func (s *Server) checkUnscopedWindow(since, now time.Time, hint string) error {
	if now.Sub(since) <= s.cfg.MaxUnscopedSince {
		return nil
	}
	return fmt.Errorf("since= reaches further back than %v, the widest window "+
		"this daemon answers without a scope (max_unscoped_since). %s",
		s.cfg.MaxUnscopedSince, hint)
}

// unscopedEventsHint is checkUnscopedWindow's tail for /v1/events' unscoped
// mode: the measurement that set the limit, and the remediation that works
// HERE -- naming router= AND peer= turns the read into a keyset seek pinned
// to one session, which is why a scoped walk carries no window bound at all.
// That remediation is true of this path and of no other path this function
// guards; see checkUnscopedWindow's own doc comment.
const unscopedEventsHint = "Unscoped, peer_events is read against the grain of its sort key " +
	"(router_ip, peer_ip, rib, ts_router, stream_seq), so a wide window is a " +
	"full-table read -- 2,040,000 rows, 237-289ms and gigabytes resident when " +
	"measured in " + query.UnscopedEventsMeasurement + ". " +
	"Narrow the window, or name router= and peer=: a scoped walk is a seek and " +
	"carries no such bound"

// unscopedCollectionHint is checkUnscopedWindow's tail for the four
// /v1/collection/* aggregates, and every clause in it has to be true of all
// FOUR -- dumps, sessions, locrib and flags -- because one message serves
// them all.
//
// It deliberately does NOT tell the operator to scope the request, and it
// does not spell the peer parameter's own name either -- so that
// TestCollectionEndpointsClampTheWindow can assert the flat, unfoolable
// thing (no "peer=" anywhere in the body) rather than a phrasing a later
// rewrite could slip past. The parameter does not exist on any of these
// paths (collectionFilter parses router=, since= and limit=, and nothing
// else), so the token has no legitimate place in their errors at all.
// router= does exist here but grants no exemption from this clamp: these
// are aggregates over every row the window covers, so router= narrows the
// WHERE without turning the read into a seek the way
// pinning (router, peer) does on /v1/events. TestCollectionEndpointsClampTheWindow
// asserts that directly, with router= set. An operator who followed
// /v1/events' own advice here would get the identical 400 and conclude the
// daemon was broken, which is the defect this hint exists to remove.
//
// The citation is qualified rather than borrowed. query.DumpCostMeasurement
// measured the DUMP classification -- the three route tables read together
// -- and nothing else: SessionCounts and FlagCounts were measured separately
// on 2026-09-20 (query.SessionsFlagsCostMeasurement). Quoting the dump
// figures without saying which of the signals they cover would pass them off
// as the others' cost.
//
// The peer clause here stays true by staying narrow: /v1/collection/churn
// takes peer= and gets its own hint below, rather than this one growing a
// qualifier about an endpoint the caller did not ask for. Rewriting this
// string to mention churn broke TestCollectionEndpointsClampTheWindow, which
// is the guard doing its job -- a remediation hint is a claim about the API
// and goes stale exactly like any other, which requiredAddr's own 400 body
// did a branch earlier.
const unscopedCollectionHint = "Every /v1/collection/* signal is an aggregate over every row the " +
	"window covers, so a wide window is a wide read. Measured at the 24h operating " +
	"point in " + query.DumpCostMeasurement + ": 802,411 rows / 28 ms / ~50 MiB for the " +
	"dump classification across all three route tables, and 7,571,382 rows / 339 ms / " +
	"~1.46 GiB at the 90-day ceiling. Those figures cover the dump classification " +
	"only -- /v1/collection/sessions and /v1/collection/flags are measured separately " +
	"in " + query.SessionsFlagsCostMeasurement + ". " +
	"Narrow the window, or raise max_unscoped_since. No scope exempts these four: " +
	"this endpoint family takes no peer parameter at all, and router= narrows the " +
	"read without turning it into a seek"

// unscopedChurnHint is unscopedCollectionHint for the one signal in the
// family that DOES take peer=.
//
// A separate string rather than a peer clause added to the shared one: an
// operator refused on /v1/collection/flags must not be advised to add a
// parameter that path does not accept, which is exactly what
// TestCollectionEndpointsClampTheWindow asserts, and it caught this the
// first time round. The clamp is shared; the remediation is per endpoint,
// which is the split collectionFilter already makes.
//
// Neither parameter exempts the window here either. Both narrow the WHERE
// on a read that still scans the window, and the bars guard
// (query.maxChurnBuckets) is a separate refusal about the bucket width.
const unscopedChurnHint = "The churn signal classifies every row the window covers across all " +
	"three route tables, so a wide window is a wide read -- see " + query.DumpCostMeasurement +
	", which measured that same classification at 802,411 rows / 28 ms / ~50 MiB over 24h " +
	"and 7,571,382 rows / 339 ms / ~1.46 GiB at the 90-day ceiling. " +
	"Narrow the window, or raise max_unscoped_since. router= and peer= narrow the read " +
	"without exempting it from the window"

// ribPage serves one page of a keyset walk, for whichever family's page
// function it is given.
//
// It is a generic function rather than three near-copies because the three
// differ in exactly two places -- the query method and the row converter --
// and everything else is the part that has to stay identical: the scope
// parsing that completes the cursor codec's unmapping guarantee, the
// cursor round trip, the 409 on a superseded session, and the smear warning
// that must be on every page of every family. Three copies would be three
// chances for one of those to be dropped from one family, which is the kind
// of difference no per-family test notices because each family's test only
// reads its own.
//
// It is a free function taking *Server as its first argument rather than a
// method with its own type parameters. Go 1.27 allows the method form, but
// editors, linters and analyzers built against older go/types releases do
// not parse it, and this is the one place the tree would need them to.
func ribPage[T, W any](
	s *Server, w http.ResponseWriter, r *http.Request,
	page func(ctx context.Context, router, peer netip.Addr, rib string,
		cur *query.RIBCursor, limit int) ([]T, *query.RIBCursor, error),
	convert func(T) W,
	state func(T) string,
	key func(T) (string, netip.Addr),
) {
	p := newParams(r)
	router, peer := p.requiredAddr("router"), p.requiredAddr("peer")
	rib, cur, limit := p.rib(), p.cursor(), p.limit(s.cfg)
	collector := p.str("collector")
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	// collector= answers ribPin's "monitored by N collectors" refusal, which
	// a caller cannot otherwise answer over HTTP: query has always taken the
	// pin as a cursor carrying (collector, session) with an empty Last, and
	// no query string could express one. Until this existed, the web UI's
	// Routes screen was unusable on every dual-homed router.
	//
	// It is resolved into that same cursor rather than plumbed down beside
	// it, so there is ONE pinning mechanism rather than two that could
	// disagree about which collector a page belongs to.
	//
	// A cursor already carries the pin, so the two together are either
	// redundant or contradictory, and a contradiction must not be resolved
	// silently in favor of either one -- paging a walk under a collector
	// the caller has since changed its mind about would answer a different
	// question from the one the cursor's position came from.
	if collector != "" {
		if cur != nil {
			if cur.Collector != collector {
				// Neither name is quoted: collector= is the caller's text,
				// and so is the cursor's, which is opaque but not signed.
				s.badParam(w, fmt.Errorf("%w: collector= contradicts this cursor, which "+
					"is paging a different collector's walk -- a cursor already carries "+
					"the collector it was issued for, so drop collector= to continue "+
					"this walk or drop the cursor to start the named collector's walk "+
					"from the beginning", query.ErrBadFilter))
				return
			}
		} else {
			start, err := s.q.RIBStart(r.Context(), router, peer, rib, collector)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			cur = start
		}
	}

	rows, next, err := page(r.Context(), router, peer, rib, cur, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// next is nil when the walk is done, and meta.next_cursor is then null
	// rather than absent -- see Meta's own doc comment. Encoding happens
	// here rather than in query because a cursor is a wire concern: query
	// hands back the position, this decides how it travels.
	var token *string
	if next != nil {
		t := encodeCursor(*next)
		token = &t
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, convert),
		Meta: Meta{
			NextCursor: token,
			Warnings: append(append(dumpingWarning(anyDumping(rows, state)),
				StaleWarning(query.AnyStale(set, rows, key), s.q.StaleAfter())...), smearWarning()),
		},
	})
}

func (s *Server) handleRIBUnicast(w http.ResponseWriter, r *http.Request) {
	ribPage(s, w, r, s.q.RIBPageUnicast, NewWireUnicastRoute,
		func(x query.Route) string { return x.DumpState }, query.Route.StaleKey)
}

func (s *Server) handleRIBVPN(w http.ResponseWriter, r *http.Request) {
	ribPage(s, w, r, s.q.RIBPageVPN, NewWireVPNRoute,
		func(x query.VPNRoute) string { return x.DumpState }, query.VPNRoute.StaleKey)
}

func (s *Server) handleRIBEVPN(w http.ResponseWriter, r *http.Request) {
	ribPage(s, w, r, s.q.RIBPageEVPN, NewWireEVPNRoute,
		func(x query.EVPNRoute) string { return x.DumpState }, query.EVPNRoute.StaleKey)
}

// errLSCursorNeedsScope is the 400 every /v1/ls path answers when a cursor
// arrives without both router= and peer= also set. Pagination pins a walk
// to one BMP session -- see LSNodesPage's own doc comment, which
// LSLinksPage and LSPrefixesPage share -- and an unscoped cursor would page
// one router's rows from another's position, skipping everything that
// sorts before it.
//
// It is a package-level value rather than a message built fresh at each of
// the three call sites below because the wording never varies with the
// request: nothing about it depends on which cursor was sent or which of
// the three paths sent it, so three inline literals would be three places
// for the wording to drift apart.
var errLSCursorNeedsScope = errors.New("cursor= requires both router= and " +
	"peer= on a link-state walk -- pagination pins a walk to one BMP " +
	"session, and an unscoped cursor would page one router's rows from " +
	"another's position and skip everything that sorts before it")

// lsScoped reports whether router and peer together scope a link-state
// walk to one (router, peer) pair -- the same test all three /v1/ls
// handlers below make before choosing between a paginated walk and today's
// capped answer.
func lsScoped(router, peer netip.Addr) bool {
	return router.IsValid() && peer.IsValid()
}

// lsScopeCheck is the scope/cursor guard all three /v1/ls handlers share:
// it answers a 400 through w and reports ok=false when cur is non-nil but
// router and peer do not together scope the walk to one BMP session (see
// errLSCursorNeedsScope's own doc comment for why that combination is
// refused). Every other combination -- including an unscoped request that
// carries no cursor at all -- reports ok=true, with scoped carrying which
// case it was.
//
// It is a method on *Server, not three copies of an if statement, because
// the check is entirely family-agnostic: it touches only netip.Addr and
// query.RIBCursor, types every one of the three handlers' own parameter
// lists already shares. That is exactly the risk errLSCursorNeedsScope was
// hoisted to a package-level value to avoid for its own message text --
// three inline copies of the surrounding logic would be three places for
// identical code to drift out of step with each other.
func (s *Server) lsScopeCheck(w http.ResponseWriter, cur *query.RIBCursor, router, peer netip.Addr) (scoped, ok bool) {
	scoped = lsScoped(router, peer)
	if cur != nil && !scoped {
		s.badParam(w, errLSCursorNeedsScope)
		return false, false
	}
	return scoped, true
}

// lsPageMeta builds one paginated /v1/ls page's meta block: the encoded
// next cursor (present and null once the walk is done, per Meta's own doc
// comment) plus the session_dumping and paginated_smear warnings every
// /v1/rib page also carries unconditionally -- see smearWarning's own doc
// comment for why the caveat never varies with what changed between pages.
func lsPageMeta[T any](rows []T, next *query.RIBCursor, state func(T) string, stale Warnings) Meta {
	var token *string
	if next != nil {
		t := encodeCursor(*next)
		token = &t
	}
	return Meta{
		NextCursor: token,
		Warnings:   append(append(dumpingWarning(anyDumping(rows, state)), stale...), smearWarning()),
	}
}

// handleLSNodes serves /v1/ls/nodes.
//
// It calls no requireNarrowing, unlike every route handler above, and that
// is deliberate rather than an omission left over from the stub.
// /v1/routes demands a narrowing parameter because an unfiltered answer
// there is every route in the fleet -- millions of rows -- while the whole
// fleet's link-state topology is a few hundred rows at most: 190 nodes and
// 133 of them live, on a representative deployment. Refusing an unfiltered
// request would refuse the one question this resource exists to answer
// cheaply: "what does the fleet's topology look like right now." The cap
// (f.Limit, from s.cfg.MaxPage) and meta.total_matched bound the answer
// instead, the same pair that already backstops every other capped list in
// this file.
//
// A request that names both router= and peer= is scoped. A scoped request
// with no node-identity narrowing -- protocol=, area=, asn=, node=, or a
// state= other than the default -- is answered by LSNodesPage instead:
// unlike the fleet-wide answer above, a walk pinned to one (router, peer)
// can be pinned to one BMP session too, which is what makes keyset
// pagination -- ?cursor= and the meta.next_cursor it hands back -- safe to
// offer at all. Such a request that names no limit= gets p.lsLimit's
// cfg.MaxPage rather than p.limit's cfg.DefaultPage; see lsLimit's own doc
// comment for why reusing limit here would be a silent page-size cut for
// every caller that scopes its request today.
//
// A scoped request that DOES carry a node-identity narrowing, and no
// cursor= of its own, is answered like an unscoped one instead -- capped,
// with meta.next_cursor always null. LSNodesPage refuses a cursor combined
// with a narrowing (see its own doc comment and LSNodeFilter.Narrowing),
// so paginating page 1 anyway would hand back a next_cursor the SAME
// request's own filter makes illegal to resend on page 2 -- the one thing
// a next_cursor exists to support. A request that already holds a cursor
// from an earlier, unfiltered page is never rerouted this way merely
// because it also carries a narrowing parameter: it still reaches
// LSNodesPage, whose own check is what turns cursor-plus-narrowing into
// the 400 it has always been. Silently dropping the cursor here instead
// would be worse than refusing it.
//
// ?limit= is honored on every branch, not only the paginating one: p.lsLimit
// validates its format and range exactly as p.limit does, and an absent
// limit= still resolves to cfg.MaxPage on every branch, so a request that
// names none keeps today's answer size exactly. A request that does name one
// gets a genuinely smaller page from it now, with meta.total_matched and the
// truncated warning still telling the truth about what was left out.
//
// handleLSLinks and handleLSPrefixes below make the identical decisions,
// for the identical reasons -- see this comment rather than repeating it
// there.
func (s *Server) handleLSNodes(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	router, peer := p.addr("router"), p.addr("peer")
	cur := p.cursor()
	scoped, ok := s.lsScopeCheck(w, cur, router, peer)
	if !ok {
		return
	}
	f := query.LSNodeFilter{
		Router:   router,
		Peer:     peer,
		RIB:      p.rib(),
		Protocol: p.protocol(),
		Area:     p.optU32("area"),
		ASN:      p.optU32("asn"),
		NodeKey:  p.optU64("node"),
		State:    p.lsState(),
	}
	paginate := scoped && (cur != nil || f.Narrowing() == "")
	if paginate {
		f.Cursor = cur
		f.Limit = p.lsLimit(s.cfg)
	} else {
		f.Limit = p.lsLimit(s.cfg)
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	if paginate {
		rows, next, err := s.q.LSNodesPage(r.Context(), f)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		set, err := s.staleSet(r.Context(), len(rows))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.writeJSON(w, http.StatusOK, Envelope{
			Data: mapRows(rows, NewWireLSNode),
			Meta: lsPageMeta(rows, next, func(x query.LSNode) string { return x.DumpState },
				StaleWarning(query.AnyStale(set, rows, query.LSNode.StaleKey), s.q.StaleAfter())),
		})
		return
	}

	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.LSNode, error) { return s.q.LSNodes(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountLSNodes(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{
		TotalMatched: &total,
		Warnings: append(dumpingWarning(
			anyDumping(rows, func(x query.LSNode) string { return x.DumpState })),
			StaleWarning(query.AnyStale(set, rows, query.LSNode.StaleKey), s.q.StaleAfter())...),
	}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireLSNode),
		Meta: meta,
	})
}

// handleLSLinks serves /v1/ls/links. See handleLSNodes for why it calls no
// requireNarrowing and for the scoped/unscoped split ?cursor= and ?limit=
// add below.
//
// LocalNode and RemoteNode read local_node= and remote_node= respectively,
// never folded into one either-end parameter the way Area and ASN are:
// LSLinkFilter's own doc comment explains why a caller naming a node_key is
// naming a direction, and folding the two would make "links out of this
// node" and "links into it" the same question.
func (s *Server) handleLSLinks(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	router, peer := p.addr("router"), p.addr("peer")
	cur := p.cursor()
	scoped, ok := s.lsScopeCheck(w, cur, router, peer)
	if !ok {
		return
	}
	f := query.LSLinkFilter{
		Router:     router,
		Peer:       peer,
		RIB:        p.rib(),
		Protocol:   p.protocol(),
		Area:       p.optU32("area"),
		ASN:        p.optU32("asn"),
		LocalNode:  p.optU64("local_node"),
		RemoteNode: p.optU64("remote_node"),
		State:      p.lsState(),
	}
	paginate := scoped && (cur != nil || f.Narrowing() == "")
	if paginate {
		f.Cursor = cur
		f.Limit = p.lsLimit(s.cfg)
	} else {
		f.Limit = p.lsLimit(s.cfg)
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	if paginate {
		rows, next, err := s.q.LSLinksPage(r.Context(), f)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		set, err := s.staleSet(r.Context(), len(rows))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.writeJSON(w, http.StatusOK, Envelope{
			Data: mapRows(rows, NewWireLSLink),
			Meta: lsPageMeta(rows, next, func(x query.LSLink) string { return x.DumpState },
				StaleWarning(query.AnyStale(set, rows, query.LSLink.StaleKey), s.q.StaleAfter())),
		})
		return
	}

	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.LSLink, error) { return s.q.LSLinks(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountLSLinks(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{
		TotalMatched: &total,
		Warnings: append(dumpingWarning(
			anyDumping(rows, func(x query.LSLink) string { return x.DumpState })),
			StaleWarning(query.AnyStale(set, rows, query.LSLink.StaleKey), s.q.StaleAfter())...),
	}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireLSLink),
		Meta: meta,
	})
}

// handleLSPrefixes serves /v1/ls/prefixes. See handleLSNodes for why it
// calls no requireNarrowing and for the scoped/unscoped split ?cursor= and
// ?limit= add below.
//
// Prefix is read as a plain string. Covers and the prefix/covers pairing
// are checked here as on every other path documenting them, even though
// LSPrefixFilter.predicates checks both again (checkCovers): that check's
// messages are written for a Go caller and name the filter's fields, and a
// 400 body names the request's parameters instead.
func (s *Server) handleLSPrefixes(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	p.requirePrefixCoversExclusive("/v1/ls/prefixes")
	router, peer := p.addr("router"), p.addr("peer")
	cur := p.cursor()
	scoped, ok := s.lsScopeCheck(w, cur, router, peer)
	if !ok {
		return
	}
	f := query.LSPrefixFilter{
		Router:   router,
		Peer:     peer,
		RIB:      p.rib(),
		Protocol: p.protocol(),
		Area:     p.optU32("area"),
		ASN:      p.optU32("asn"),
		NodeKey:  p.optU64("node"),
		Prefix:   p.str("prefix"),
		Covers:   p.covers(),
		State:    p.lsState(),
	}
	paginate := scoped && (cur != nil || f.Narrowing() == "")
	if paginate {
		f.Cursor = cur
		f.Limit = p.lsLimit(s.cfg)
	} else {
		f.Limit = p.lsLimit(s.cfg)
	}
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	if paginate {
		rows, next, err := s.q.LSPrefixesPage(r.Context(), f)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		set, err := s.staleSet(r.Context(), len(rows))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.writeJSON(w, http.StatusOK, Envelope{
			Data: mapRows(rows, NewWireLSPrefix),
			Meta: lsPageMeta(rows, next, func(x query.LSPrefix) string { return x.DumpState },
				StaleWarning(query.AnyStale(set, rows, query.LSPrefix.StaleKey), s.q.StaleAfter())),
		})
		return
	}

	rows, total, err := rowsAndTotal(r.Context(),
		func(ctx context.Context) ([]query.LSPrefix, error) { return s.q.LSPrefixes(ctx, f) },
		func(ctx context.Context) (uint64, error) { return s.q.CountLSPrefixes(ctx, f) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.staleSet(r.Context(), len(rows))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{
		TotalMatched: &total,
		Warnings: append(dumpingWarning(
			anyDumping(rows, func(x query.LSPrefix) string { return x.DumpState })),
			StaleWarning(query.AnyStale(set, rows, query.LSPrefix.StaleKey), s.q.StaleAfter())...),
	}
	if total > uint64(len(rows)) {
		meta.Warnings = append(meta.Warnings, truncatedWarning(total, len(rows)))
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: mapRows(rows, NewWireLSPrefix),
		Meta: meta,
	})
}
