// Cursor: the opaque ?cursor= token every paginated /v1 path hands out in
// meta.next_cursor and takes back on the next request.
//
// It is base64url of a versioned JSON object, and it carries a
// query.RIBCursor whole -- the (collector, session), the walk's
// (router, peer, rib) SCOPE, and the last page's family-specific key tuple.
// query.RIBCursor's own doc comment is the account of what each silently
// answers if it is allowed to drift, and this file exists to get all of
// them across a query string and back without changing any of them.
//
// # The session pin is per-endpoint, not universal
//
// Collector and SessionID are "the pinned (collector, session)" for
// /v1/rib/* and /v1/ls/*: those walks answer what is true NOW, so a cursor
// that could resume across a session change would silently mix two views,
// and decodeCursor refuses a cursor missing either field. /v1/events'
// cursor is a genuine exception, not a bug -- query.PeerEventsPage issues
// one with both fields at their zero values, deliberately, because a
// peer's event history is itself the record of sessions starting and
// ending, and pinning one would stop the walk at the very transitions that
// endpoint exists to show.
//
// The requirement therefore lives on the CALLER, not in one shared
// decode function: decodeCursorFields parses the wire bytes both shapes
// share, decodeCursor adds the pin check for /v1/rib/* and /v1/ls/*, and
// decodeCursorUnpinned skips it for /v1/events. params.cursor and
// params.unpinnedCursor are the two entry points a handler actually calls,
// named so the choice is visible at the call site rather than buried here.
//
// # The cursor is NOT signed, and must not become one
//
// There is no HMAC here and its absence is deliberate, not an oversight.
// A cursor carries a POSITION, not an authorization: every caller has
// already presented a bearer token (see auth.go), and the position it names
// is inside data that caller is already entitled to read. Signing it would
// protect nothing -- forging one buys a caller a page of rows they can
// fetch by asking for page 1 and paging forward, which is the supported way
// to get them -- while adding a key to distribute and rotate and a new
// failure mode with no good answer: every cursor a client is holding turns
// into a 400 the moment the key changes or a second replica starts with a
// different one. What a tampered cursor actually costs is a wrong ANSWER,
// and that is defended against by validating it rather than by signing it:
// this file refuses anything that is not the shape it issues, and
// query.ribPage refuses a cursor whose scope disagrees with the request or
// whose key tuple does not match the family being walked. Those checks run
// on a forged cursor and on a corrupted one alike; a signature would only
// tell the two apart, which is a distinction nothing here needs to make.
//
// There is no length cap on the input either, for the same reason: a cursor
// arrives in a query string, so net/http's own header limit bounds it long
// before this code sees it, and base64 plus one json.Unmarshal of a bounded
// document is not work worth a second limit.
//
// # Version first
//
// v is the first field emitted and the first thing checked, read by its own
// pass over the payload before any other field is looked at. That is what
// makes a future format change a clean rejection -- "cursor version 2, this
// build accepts 1" -- instead of whatever field-level type error a v2
// document happens to trip when forced into today's struct. It costs one
// extra json.Unmarshal of a ~200 byte document per page, which is not a
// cost worth trading a confusing error for.
//
// # Nothing in the document is a JSON number except the version
//
// The session_id is a collector's now().UnixNano(), around 1.77e18, well
// past the 2^53 a float64 holds exactly. Encoded as a JSON number it comes
// back rounded, and a rounded session never equals the one on record: every
// page after the first would fail with ErrSessionChanged for a router that
// never reconnected. So it is a string, the way api/openapi.yaml already
// renders session_id and seq for the same reason.
//
// The key tuple has a second, quieter form of the same problem. Its
// elements are typed -- string for a String column, uint8 for route_type,
// uint32 for path_id and ethernet_tag -- and the types are what query.keyset
// binds and checks; a float64 where a uint32 belongs does not raise in
// ClickHouse, it compares numerically and walks on from a position that is
// no longer the one the last page ended at. encoding/json has no way to put
// a uint32 into an `any` and get a uint32 back, so the elements are not
// numbers either: each is a string with its type as a prefix, "u32:7", cut
// at the FIRST colon so that the RDs, MACs, ESIs and IPv6 prefixes EVPN
// keys are made of survive intact.
//
// The result is a document whose only JSON number is v, which is small and
// bounded. There is no float64 anywhere on the decode path for a value to
// be rounded through -- a structural property, not a remembered one, and
// TestCursorEncodesNoJSONNumberButTheVersion holds it that way.
//
// # Addresses are unmapped
//
// query returns unmapped addresses and query.ribPage compares the cursor's
// scope to the request's with netip.Addr !=, which compares an address's
// FORM as well as its value: ::ffff:10.77.0.33 and 10.77.0.33 select
// identical rows and are not equal.
//
// What unmapping here buys is exactly two things, and it is worth being
// precise because the obvious larger claim is false. Encoding unmapped makes
// the cursor this daemon ISSUES canonical, so one walk has one cursor string
// however the request spelled its address; decoding unmapped makes a
// hand-built or older cursor carrying the mapped form name the same walk
// rather than being refused.
//
// It does NOT on its own guarantee that a mapped-form request paginates.
// ribPage compares the decoded cursor against the address the HANDLER passed
// it, and builds the next cursor from that argument verbatim -- so if the
// handler does not Unmap the parsed router= and peer=, a mapped-form request
// succeeds on page 1 (filters unmap at bind time) and then fails on page 2
// against this codec's canonicalized cursor, a mismatch that would not exist
// had this codec left the form alone. The page-2 guarantee is the handler's
// to complete by unmapping what it parses; this half is necessary and not
// sufficient.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/jp2195/vantage/query"
)

// cursorVersion is the format version this build issues, and the only one
// it accepts. A change to the payload's shape bumps it, and every cursor a
// client is holding becomes a 400 that says so -- which is the right
// outcome, since the alternative is decoding an old document as a new one.
const cursorVersion = 1

// errBadCursor is the sentinel every decodeCursor failure wraps, so that
// cmd/vantage-api can answer api/openapi.yaml's "a tampered cursor is a
// 400" with errors.Is rather than by matching on message text.
//
// It is api-local rather than query.ErrBadFilter, which is the other
// sentinel that means 400 on these paths, and the split is deliberate.
// query.ErrBadFilter marks a malformed QUESTION -- an address family that
// does not exist, a VPN query with nothing to narrow it -- and its remedy is
// "fix the parameters and ask again". This marks a cursor STRING that is not
// one we issued, which query never sees and has no vocabulary for: base64
// that does not decode, a version from another build, a key element with no
// type. Its remedy is different and worth saying separately in the response
// body: drop ?cursor= and restart the walk from page 1. query's own
// ErrSessionChanged doc comment makes this argument for the same reason --
// two failures with the same status but different remedies are two
// sentinels, not one. Both map to 400 in the handler, in one switch, and
// that handler's own test is what keeps this branch from being forgotten.
var errBadCursor = errors.New("api: malformed cursor")

// wireCursor is the JSON shape. Field names are short because this rides in
// a query string, and spelled out where a human decoding a cursor by hand
// would otherwise have to guess.
//
// No field carries omitempty. rib is legitimately empty -- it is the walk's
// rib SCOPE, and "no scope" is a real walk that query.ribPage must be able
// to tell from a cursor issued before the field existed -- and the same
// present-and-explicit rule the wire types follow (see types.go) applies
// just as well to a document nobody is meant to read.
type wireCursor struct {
	V    int      `json:"v"`
	C    string   `json:"c"`
	S    string   `json:"s"`
	Rtr  string   `json:"rtr"`
	Peer string   `json:"peer"`
	RIB  string   `json:"rib"`
	K    []string `json:"k"`
}

// encodeCursor renders a cursor for meta.next_cursor.
//
// It cannot fail, and its signature says so: the only input that could make
// it fail is a Last element of a type this codec cannot carry, which is a
// bug in query's key builders rather than anything a caller did. That case
// is handled by making the OUTPUT undecodable rather than by returning an
// error nobody upstream could act on -- see encodeCursorKey.
func encodeCursor(c query.RIBCursor) string {
	w := wireCursor{
		V:    cursorVersion,
		C:    c.Collector,
		S:    strconv.FormatUint(c.SessionID, 10),
		Rtr:  c.Router.Unmap().String(),
		Peer: c.Peer.Unmap().String(),
		RIB:  c.RIB,
		// Non-nil so an empty key renders as [] rather than null. Both
		// decode to the same thing here, but a cursor a human is squinting
		// at should not have two spellings for "this walk has not moved".
		K: make([]string, len(c.Last)),
	}
	for i, v := range c.Last {
		w.K[i] = encodeCursorKey(v)
	}
	// Cannot fail: wireCursor is strings, a []string and an int, none of
	// which json.Marshal has a way to reject. There is no channel, func,
	// cycle or NaN reachable from it.
	b, _ := json.Marshal(w)
	// RawURLEncoding, not StdEncoding: base64url's alphabet has no + or /
	// to be percent-encoded on the way into a query string, and dropping
	// the padding drops the = that would be. A cursor survives a URL, a
	// shell and a log line unchanged.
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursorFields parses a cursor's wire bytes into a query.RIBCursor,
// checking everything the format itself constrains -- version, base64,
// JSON shape, the scope addresses, the key tuple's types -- but NOT
// whether a session pin is present. That is this function's whole reason
// to exist separately from decodeCursor: the pin requirement is the one
// thing that differs by ENDPOINT (see this file's header), and every other
// check here is identical whichever of decodeCursor or
// decodeCursorUnpinned is asking.
//
// Every failure wraps errBadCursor and every failure returns the zero
// cursor, never a partly filled one: a caller that checked the error
// carelessly would otherwise walk from a position nobody chose.
func decodeCursorFields(s string) (query.RIBCursor, error) {
	fail := func(format string, args ...any) (query.RIBCursor, error) {
		return query.RIBCursor{}, fmt.Errorf("%w: "+format,
			append([]any{errBadCursor}, args...)...)
	}
	// A message, not a gate: "" decodes to zero bytes and would be refused
	// by the unmarshal below anyway, as "not a cursor object". Deleting
	// this leaves every caller-visible behavior unchanged and no test
	// fails, which is measured rather than assumed -- it is here so an
	// operator reading a 400 sees "empty" instead of a JSON complaint about
	// a document that was never sent.
	if s == "" {
		return fail("empty")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// The cursor itself is not in the message, here or anywhere below.
		// It is caller-supplied text headed for a 400 body and a log line,
		// and echoing it back is how a reflected-content bug gets built by
		// accident. base64's own error names an offset, not the input.
		return fail("not base64url: %w", err)
	}

	// Version first, in its own pass, so that a document from a future
	// format is rejected for its VERSION rather than for whichever field
	// happens to have changed shape underneath it.
	var ver struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(raw, &ver); err != nil {
		return fail("not a cursor object")
	}
	if ver.V != cursorVersion {
		return fail("version %d, but this build issues and accepts version %d "+
			"only -- restart the walk without a cursor", ver.V, cursorVersion)
	}

	var w wireCursor
	if err := json.Unmarshal(raw, &w); err != nil {
		return fail("version %d, but its fields are not the shape version %d has",
			ver.V, cursorVersion)
	}

	sid, err := strconv.ParseUint(w.S, 10, 64)
	if err != nil {
		return fail("session is not a base-10 uint64")
	}

	// The scope. All three are required, and query.ribPage compares every
	// one of them against the request: a conforming client may legitimately
	// send page 2 with a different rib, a different peer, or none, and each
	// of those is a silently truncated or silently widened result rather
	// than an error unless the cursor remembers what walk it came from.
	router, err := netip.ParseAddr(w.Rtr)
	if err != nil {
		return fail("router is not an IP address")
	}
	peer, err := netip.ParseAddr(w.Peer)
	if err != nil {
		return fail("peer is not an IP address")
	}

	var last []any
	if len(w.K) > 0 {
		last = make([]any, len(w.K))
		for i, e := range w.K {
			v, err := decodeCursorKey(e)
			if err != nil {
				return fail("key element %d: %w", i, err)
			}
			last[i] = v
		}
	}

	return query.RIBCursor{
		Collector: w.C,
		SessionID: sid,
		// Unmapped on the way in as well as on the way out, so that a
		// hand-built or older cursor carrying ::ffff:10.0.0.1 still names
		// the same walk as 10.0.0.1 rather than failing query's scope
		// comparison, which is a != on the Addr and so on its form.
		Router: router.Unmap(),
		Peer:   peer.Unmap(),
		RIB:    w.RIB,
		Last:   last,
	}, nil
}

// decodeCursor is decodeCursorFields plus the session-pin requirement
// /v1/rib/* and /v1/ls/* hold, unconditionally: an empty collector or a
// zero session_id is refused, because pagination there is pinned to one
// BMP session (query.RIBCursor's own doc comment; query.LSNodesPage's) and
// a cursor missing either half of the pin cannot have come from a walk
// that had one.
//
// This is the ONLY entry point params.cursor calls, which is what every
// /v1/rib/* and /v1/ls/* handler goes through. See decodeCursorUnpinned
// for the one endpoint the pin requirement does not hold for, and this
// file's header for why the split is expressed this way.
func decodeCursor(s string) (query.RIBCursor, error) {
	c, err := decodeCursorFields(s)
	if err != nil {
		return query.RIBCursor{}, err
	}
	// Both halves of the pin, checked here as well as in query.ribPage.
	// This is not a duplicated policy: query refuses them because a walk
	// cannot be pinned without them, and this refuses them because no
	// cursor /v1/rib/* or /v1/ls/* ISSUES can lack them, so one that does
	// did not come from a walk this endpoint can resume. Rejecting at the
	// boundary keeps the "did we write this?" question in one place, and a
	// session_id is a UnixNano -- zero is not a value any clock produces.
	if c.Collector == "" {
		return query.RIBCursor{}, fmt.Errorf("%w: no collector -- a session_id is "+
			"issued by one collector's own clock and means nothing without it",
			errBadCursor)
	}
	if c.SessionID == 0 {
		return query.RIBCursor{}, fmt.Errorf(
			"%w: session is zero, which no collector's clock issues", errBadCursor)
	}
	return c, nil
}

// decodeCursorUnpinned is decodeCursorFields with no session-pin check:
// /v1/events' own cursor, which query.PeerEventsPage issues with Collector
// and SessionID deliberately at their zero values (see its own doc
// comment) because a peer's event history IS the record of sessions
// starting and ending, and a pin would stop the walk at the very
// transitions that endpoint exists to show.
//
// params.unpinnedCursor is the one caller. Every other paginated endpoint
// must keep going through params.cursor / decodeCursor instead --
// TestHandlerRIBUnicastWalk's "a session-less cursor is still refused"
// subtest (api/handlers_test.go) is what catches a handler that borrows
// this one by mistake.
func decodeCursorUnpinned(s string) (query.RIBCursor, error) {
	return decodeCursorFields(s)
}

// encodeCursorKey renders one element of the family's key tuple as
// "<type>:<value>". The type tag is what lets decodeCursorKey hand back a
// uint32 rather than the float64 a JSON number would become, which is the
// difference between query.keyset binding the value the last page really
// ended at and binding something that merely compares equal to it.
//
// The three cases are the three Go types query's key columns declare (see
// query's unicastRIBKey, vpnRIBKey and evpnRIBKey). Anything else is a bug
// in those tables rather than a caller's doing, and it must not become a
// cursor that decodes to a DIFFERENT position -- an int quietly written as
// a u32 would compare against a UInt32 column and walk on. So it is written
// with a tag that begins with '!', which no case below accepts and no legal
// tag can collide with, making the bug a loud 400 on page 2 whose message
// names the offending Go type.
//
// One wart in that, worth knowing before it is debugged from the wrong end:
// the fault is in query's key builders, but the only symptom is a 400 at a
// client. Encoding is where the bug is first visible and nothing is logged
// there, because this package has no logger yet. The handler that calls
// encodeCursor is the place to log it once it has one.
func encodeCursorKey(v any) string {
	switch t := v.(type) {
	case string:
		return "s:" + t
	case uint32:
		return "u32:" + strconv.FormatUint(uint64(t), 10)
	case uint8:
		return "u8:" + strconv.FormatUint(uint64(t), 10)
	case uint64:
		return "u64:" + strconv.FormatUint(t, 10)
	default:
		return fmt.Sprintf("!%T:%v", v, v)
	}
}

// decodeCursorKey is encodeCursorKey's inverse, and it is strict on both the
// tag and the value's range: a u32 past 2^32 or a u8 past 2^8 is refused
// rather than truncated into the column's width, because a truncated key is
// a valid-looking position somewhere else in the table.
func decodeCursorKey(s string) (any, error) {
	tag, val, ok := strings.Cut(s, ":")
	if !ok {
		return nil, errors.New("has no type tag")
	}
	switch tag {
	case "s":
		return val, nil
	case "u32":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return nil, errors.New("is tagged u32 but is not a uint32")
		}
		return uint32(n), nil
	case "u8":
		n, err := strconv.ParseUint(val, 10, 8)
		if err != nil {
			return nil, errors.New("is tagged u8 but is not a uint8")
		}
		return uint8(n), nil
	case "u64":
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return nil, errors.New("is tagged u64 but is not a uint64")
		}
		return n, nil
	default:
		// Neither the tag nor the value is named. A tag that reached this
		// branch is by definition not one of the fixed tokens above: it is
		// whatever a forged or corrupted cursor put before its first colon,
		// and that is caller-supplied text.
		return nil, errors.New("has a type tag this codec does not carry")
	}
}
