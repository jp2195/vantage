package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jp2195/vantage/query"
)

// realSessionID is what a session_id actually is: a collector's
// now().UnixNano(). It is ~1.77e18, past the 2^53 a float64 represents
// exactly -- the nearest float64 is 1774329600123456768, eight less -- and
// every test below that touches a session uses THIS value rather than a
// small one on purpose. A cursor codec that pushes the session through a
// float64 round-trips 1 perfectly and corrupts this, so a test written with
// 1 asserts nothing about the only values that occur.
const realSessionID uint64 = 1774329600123456789

// cursorFor is the well-formed cursor the rejection tests mutate one field
// of. Keeping the "good" shape in one place is what makes those tests
// mean something: each one differs from a cursor that IS accepted (see the
// control at the end of TestCursorRejectsTampering) by exactly the thing
// under test.
func cursorFor(t *testing.T, last []any) query.RIBCursor {
	t.Helper()
	return query.RIBCursor{
		Collector: "dev-c1",
		SessionID: realSessionID,
		Router:    netip.MustParseAddr("10.77.0.33"),
		Peer:      netip.MustParseAddr("2001:db8::1"),
		RIB:       "in_post",
		Last:      last,
	}
}

// assertSameCursor fails unless got is want in every field, comparing Last
// element by element with reflect.DeepEqual so a uint32 that came back as a
// float64 -- equal numerically, wrong to ClickHouse -- is a failure and not
// a pass. DeepEqual on []any compares dynamic TYPES as well as values,
// which is the whole property under test.
func assertSameCursor(t *testing.T, got, want query.RIBCursor) {
	t.Helper()
	if got.Collector != want.Collector {
		t.Errorf("Collector = %q, want %q", got.Collector, want.Collector)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID = %d, want %d -- a session that does not survive the "+
			"round trip exactly never equals the one on record, so every page after "+
			"the first fails with ErrSessionChanged for a router that never reconnected",
			got.SessionID, want.SessionID)
	}
	if got.Router != want.Router {
		t.Errorf("Router = %v, want %v", got.Router, want.Router)
	}
	if got.Peer != want.Peer {
		t.Errorf("Peer = %v, want %v", got.Peer, want.Peer)
	}
	if got.RIB != want.RIB {
		t.Errorf("RIB = %q, want %q", got.RIB, want.RIB)
	}
	if !reflect.DeepEqual(got.Last, want.Last) {
		t.Errorf("Last = %#v, want %#v", got.Last, want.Last)
	}
}

// TestCursorRoundTrips covers all three families' key tuples, because the
// element TYPES differ between them and query.keyset refuses a cursor whose
// types are not the ones its family's key columns declare. The values are
// deliberately awkward: RDs, MACs, ESIs and IPv6 prefixes all contain
// colons, and the encoding tags each element's type with a colon-separated
// prefix, so a decoder that split on every colon instead of the first would
// shred exactly the values EVPN is made of.
func TestCursorRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		last []any
	}{
		{"unicast", []any{"in_post", "10.0.0.0/24", uint32(7)}},
		{"unicast v6 prefix", []any{"loc_rib", "2001:db8:abcd::/48", uint32(0)}},
		{"vpn", []any{"in_pre", "65000:100", "2001:db8::/32", uint32(0)}},
		{"evpn", []any{
			"loc_rib", uint8(2), "65000:1", "10.0.0.5/32", "00:11:22:33:44:55",
			"2001:db8::5", uint32(4094), "00:11:22:33:44:55:66:77:88", uint32(1),
		}},
		{"no key at all, the start-at-the-beginning cursor", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := cursorFor(t, tc.last)
			got, err := decodeCursor(encodeCursor(want))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertSameCursor(t, got, want)
		})
	}
}

// TestCursorSessionIDSurvivesPastFloat64 states the hazard on its own rather
// than leaving it to the round-trip table, because it is the one defect this
// codec exists to avoid and it is invisible to a test written with a small
// number. It asserts the corruption directly: the session, pushed through a
// float64, is NOT the session, so any decode path that touches one is caught.
func TestCursorSessionIDSurvivesPastFloat64(t *testing.T) {
	if uint64(float64(realSessionID)) == realSessionID {
		t.Fatalf("realSessionID %d survives a float64 intact -- this test is "+
			"vacuous and needs a value past 2^53", realSessionID)
	}
	if realSessionID <= 1<<53 {
		t.Fatalf("realSessionID %d is inside 2^53", realSessionID)
	}

	want := cursorFor(t, []any{"in_post", "10.0.0.0/24", uint32(7)})
	got, err := decodeCursor(encodeCursor(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SessionID != realSessionID {
		t.Errorf("SessionID = %d, want %d (a float64 round trip gives %d)",
			got.SessionID, realSessionID, uint64(float64(realSessionID)))
	}
}

// TestCursorEncodesNoJSONNumberButTheVersion is the structural reason the
// test above passes, asserted rather than assumed. Nothing in the document
// is a JSON number except v, so there is no float64 for a value to be
// rounded through -- not the session, not path_id, not ethernet_tag. A
// future edit that "simplifies" one of them back to a number trips here even
// if it happens to pick a small enough value to survive the round trip.
func TestCursorEncodesNoJSONNumberButTheVersion(t *testing.T) {
	raw := decodeCursorPayload(t, encodeCursor(cursorFor(t,
		[]any{"in_post", "10.0.0.0/24", uint32(7)})))

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("payload is not a JSON object: %v (%s)", err, raw)
	}
	for k, v := range obj {
		if k == "v" {
			continue
		}
		if isJSONNumber(t, v) {
			t.Errorf("field %q is the JSON number %s -- every scalar but the "+
				"version is a string so that nothing can be decoded through a "+
				"float64", k, v)
		}
	}
	var key []json.RawMessage
	if err := json.Unmarshal(obj["k"], &key); err != nil {
		t.Fatalf("k is not an array: %v", err)
	}
	for i, e := range key {
		if isJSONNumber(t, e) {
			t.Errorf("key element %d is the JSON number %s -- path_id and "+
				"ethernet_tag are UInt32s that must come back as uint32, not "+
				"as the float64 a bare JSON number decodes to", i, e)
		}
	}
}

// isJSONNumber reports whether one encoded JSON value is a number.
func isJSONNumber(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	_, ok := v.(float64)
	return ok
}

// TestCursorUnmapsAddresses pins both halves of the canonicalization. query
// returns unmapped addresses and query.ribPage compares the cursor's scope
// with netip.Addr !=, which is a comparison of the address's FORM as well as
// its value: ::ffff:10.77.0.33 and 10.77.0.33 select identical rows and are
// not equal.
//
// The claim under test is deliberately narrow -- the issued cursor is
// canonical, and a mapped cursor decodes to the plain form -- and NOT that a
// mapped-form request paginates, which this package cannot deliver alone.
// ribPage compares against the address the handler passed it, so that
// property is the handler's to complete by unmapping what it parses out of
// router= and peer=. See cursor.go's own header.
func TestCursorUnmapsAddresses(t *testing.T) {
	plainRouter := netip.MustParseAddr("10.77.0.33")
	plainPeer := netip.MustParseAddr("10.77.0.34")
	mapped := query.RIBCursor{
		Collector: "dev-c1",
		SessionID: realSessionID,
		Router:    netip.MustParseAddr("::ffff:10.77.0.33"),
		Peer:      netip.MustParseAddr("::ffff:10.77.0.34"),
		RIB:       "in_post",
		Last:      []any{"in_post", "10.0.0.0/24", uint32(7)},
	}
	if mapped.Router == plainRouter {
		t.Fatal("a 4-in-6 Addr already equals its unmapped form -- this test is vacuous")
	}

	// The issued cursor is canonical: the payload itself carries the plain
	// form, so two walks of one router cannot produce two different cursor
	// strings depending on how the request spelled its address. Asserted on
	// the payload rather than only on the round trip, because the decode
	// side unmaps too and would otherwise cover for an encoder that did not.
	payload := string(decodeCursorPayload(t, encodeCursor(mapped)))
	if !strings.Contains(payload, `"rtr":"10.77.0.33"`) ||
		!strings.Contains(payload, `"peer":"10.77.0.34"`) {
		t.Errorf("payload carries a mapped address: %s", payload)
	}

	got, err := decodeCursor(encodeCursor(mapped))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Router != plainRouter {
		t.Errorf("Router = %v (%q), want %v -- query.ribPage compares scope with "+
			"!=, so a mapped form is a different walk and a 400",
			got.Router, got.Router, plainRouter)
	}
	if got.Peer != plainPeer {
		t.Errorf("Peer = %v (%q), want %v", got.Peer, got.Peer, plainPeer)
	}

	// And from the other side: a cursor whose payload carries the mapped
	// spelling, which is what a hand-built or older cursor would look like,
	// still decodes to the plain form.
	handBuilt := makeCursor(t, map[string]any{
		"v": cursorVersion, "c": "dev-c1", "s": strconv.FormatUint(realSessionID, 10),
		"rtr": "::ffff:10.77.0.33", "peer": "::ffff:10.77.0.34", "rib": "in_post",
		"k": []string{"s:in_post", "s:10.0.0.0/24", "u32:7"},
	})
	got, err = decodeCursor(handBuilt)
	if err != nil {
		t.Fatalf("decode hand-built: %v", err)
	}
	if got.Router != plainRouter || got.Peer != plainPeer {
		t.Errorf("hand-built decoded to router %v peer %v, want %v and %v",
			got.Router, got.Peer, plainRouter, plainPeer)
	}
}

// TestCursorCarriesTheWholeScope pins that each of the three scope fields is
// actually IN the payload. Every one of them was added because a conforming
// client may legitimately send page 2 with a different router, peer or rib
// and query.ribPage must be able to refuse -- see query.RIBCursor for what
// each silently answers if it drifts. A codec that dropped one would still
// round-trip its own output through a struct that never held it, so this
// test reads the payload rather than the decoded value.
func TestCursorCarriesTheWholeScope(t *testing.T) {
	raw := decodeCursorPayload(t, encodeCursor(cursorFor(t,
		[]any{"in_post", "10.0.0.0/24", uint32(7)})))
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	for field, want := range map[string]string{
		"rtr":  "10.77.0.33",
		"peer": "2001:db8::1",
		"rib":  "in_post",
		"c":    "dev-c1",
	} {
		got, ok := obj[field]
		if !ok {
			t.Errorf("payload has no %q field: %s", field, raw)
			continue
		}
		if got != want {
			t.Errorf("payload %q = %v, want %q", field, got, want)
		}
	}
}

// TestCursorScopeChangesAreVisibleAfterDecode is the same property from the
// consuming end: two walks that differ only in scope must not produce
// cursors that decode to the same position, or query.ribPage has nothing to
// compare and the mismatch it exists to catch never reaches it.
func TestCursorScopeChangesAreVisibleAfterDecode(t *testing.T) {
	base := cursorFor(t, []any{"in_post", "10.0.0.0/24", uint32(7)})
	for _, tc := range []struct {
		name string
		mut  func(*query.RIBCursor)
	}{
		{"router", func(c *query.RIBCursor) { c.Router = netip.MustParseAddr("10.77.0.99") }},
		{"peer", func(c *query.RIBCursor) { c.Peer = netip.MustParseAddr("2001:db8::9") }},
		{"rib", func(c *query.RIBCursor) { c.RIB = "in_pre" }},
		{"rib dropped", func(c *query.RIBCursor) { c.RIB = "" }},
		{"collector", func(c *query.RIBCursor) { c.Collector = "dev-c2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.mut(&other)

			a, err := decodeCursor(encodeCursor(base))
			if err != nil {
				t.Fatalf("decode base: %v", err)
			}
			b, err := decodeCursor(encodeCursor(other))
			if err != nil {
				t.Fatalf("decode mutated: %v", err)
			}
			if a.Router == b.Router && a.Peer == b.Peer && a.RIB == b.RIB &&
				a.Collector == b.Collector && a.SessionID == b.SessionID {
				t.Errorf("changing %s produced a cursor that decodes to the same "+
					"scope (%+v) -- query.ribPage cannot refuse what it cannot see",
					tc.name, a)
			}
		})
	}
}

// TestCursorPreservesKeyTypesRatherThanCoercingThem covers the boundary
// between this codec and query.keyset. The family check lives THERE (it is
// the only layer that knows which family is being walked), and it works by
// comparing each element's dynamic type against the family's key columns --
// so its whole value depends on this codec handing back the types the cursor
// actually carried instead of the ones it guessed the family wanted. A
// decoder that coerced a stringy path_id into a uint32 would turn a cursor
// query would have refused into one it silently walks with.
func TestCursorPreservesKeyTypesRatherThanCoercingThem(t *testing.T) {
	// The unicast key is (rib string, prefix string, path_id uint32). This
	// cursor has path_id as a string and route_type-shaped junk besides:
	// wrong for every family, and it must come back exactly as wrong.
	wrong := []any{"in_post", "10.0.0.0/24", "7"}
	got, err := decodeCursor(encodeCursor(cursorFor(t, wrong)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got.Last, wrong) {
		t.Fatalf("Last = %#v, want %#v", got.Last, wrong)
	}
	if _, ok := got.Last[2].(string); !ok {
		t.Errorf("Last[2] came back as %T -- coercing it to the type the family "+
			"wants hides a mismatch query.keyset exists to refuse", got.Last[2])
	}

	// And the widths are distinct: uint8 must not come back as uint32.
	// query.evpnRIBKey declares route_type as a UInt8 and every other
	// integer column as a UInt32, and reflect.TypeOf tells them apart.
	widths := []any{"loc_rib", uint8(2), "65000:1", "10.0.0.5/32", "00:11:22:33:44:55",
		"2001:db8::5", uint32(2), "00:11:22:33:44:55:66:77:88", uint32(2)}
	got, err = decodeCursor(encodeCursor(cursorFor(t, widths)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.Last[1].(uint8); !ok {
		t.Errorf("route_type came back as %T, want uint8", got.Last[1])
	}
	if _, ok := got.Last[6].(uint32); !ok {
		t.Errorf("ethernet_tag came back as %T, want uint32", got.Last[6])
	}
}

// TestCursorRejectsTampering covers what a cursor is by construction: an
// opaque string handed to a client and handed back, which is to say the one
// input to this daemon that arrives already parsed by nobody. Every case
// here is a 400 in api/openapi.yaml, and errors.Is against errBadCursor is
// what will let the handler say so without matching on message text.
func TestCursorRejectsTampering(t *testing.T) {
	b64 := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	// The collector is "dev-c01" and not "dev-c1" for one reason: the
	// padding case below needs this payload's length not to be a multiple
	// of 3, or base64 emits no padding and the case is vacuous. The guard
	// under it enforces that, and it has already caught it once.
	good := map[string]any{
		"v": cursorVersion, "c": "dev-c01", "s": strconv.FormatUint(realSessionID, 10),
		"rtr": "10.77.0.33", "peer": "2001:db8::1", "rib": "in_post",
		"k": []string{"s:in_post", "s:10.0.0.0/24", "u32:7"},
	}
	if !strings.Contains(base64.URLEncoding.EncodeToString(mustMarshal(t, good)), "=") {
		t.Fatal("the good payload's length happens to be a multiple of 3, so the " +
			"padding case below encodes to the same string as the raw one and " +
			"tests nothing -- change a value's length")
	}
	// with returns the good payload with one field replaced, so each case
	// below differs from an ACCEPTED cursor by exactly one thing.
	with := func(field string, v any) string {
		m := map[string]any{}
		maps.Copy(m, good)
		m[field] = v
		return makeCursor(t, m)
	}

	for _, tc := range []struct{ name, cursor string }{
		{"empty", ""},
		{"not base64", "!!!!"},
		// A payload that IS accepted, re-encoded with padding. The point is
		// the padding and nothing else, so the fixture has to be otherwise
		// perfect or the case passes for the wrong reason.
		{"base64 with padding, which this codec never emits",
			base64.URLEncoding.EncodeToString(mustMarshal(t, good))},
		{"base64 of garbage", b64("nonsense")},
		{"base64 of a JSON array", b64(`[1,2,3]`)},
		{"base64 of a bare JSON string", b64(`"cursor"`)},
		{"base64 of JSON null", b64(`null`)},
		{"empty JSON object, which carries no version", b64(`{}`)},
		// These three change ONLY the version. The obvious way to write
		// them -- a hand-typed {"v":99,"c":"x","s":"1","k":[]} -- is a
		// cursor that is also missing rtr and peer, so it is refused for
		// those and the version check can be deleted outright with the
		// suite still green. Verified: that mutation survived until these
		// cases were written this way.
		{"a version-1 cursor in every respect but its version", with("v", cursorVersion+1)},
		{"version zero", with("v", 0)},
		{"version negative", with("v", -1)},
		{"wrong version, nothing else well formed", b64(`{"v":99,"c":"x","s":"1","k":[]}`)},
		{"truncated json", b64(`{"v":1,"c":`)},
		{"trailing junk after the object", b64(`{"v":1,"c":"x","s":"1","k":[]} nope`)},

		{"session as a JSON number", with("s", realSessionID)},
		{"session not a number at all", with("s", "not-a-session")},
		{"session absent", with("s", "")},
		{"session zero", with("s", "0")},
		{"session past uint64", with("s", "18446744073709551616")},
		{"session negative", with("s", "-1")},
		{"collector absent", with("c", "")},

		{"router absent", with("rtr", "")},
		{"router not an address", with("rtr", "not-an-address")},
		{"router is a prefix", with("rtr", "10.77.0.0/24")},
		{"peer absent", with("peer", "")},
		{"peer not an address", with("peer", "2001:db8::zz")},

		{"key element with no type tag", with("k", []string{"in_post", "s:10.0.0.0/24", "u32:7"})},
		{"key element with an unknown type tag", with("k", []string{"s:in_post", "s:10.0.0.0/24", "i64:7"})},
		{"key element tagged with a Go type this codec cannot carry",
			with("k", []string{"s:in_post", "s:10.0.0.0/24", "!int:7"})},
		{"u32 that is not a number", with("k", []string{"s:in_post", "s:10.0.0.0/24", "u32:seven"})},
		{"u32 past 2^32", with("k", []string{"s:in_post", "s:10.0.0.0/24", "u32:4294967296"})},
		{"u32 negative", with("k", []string{"s:in_post", "s:10.0.0.0/24", "u32:-1"})},
		{"u8 past 2^8", with("k", []string{"s:loc_rib", "u8:256", "s:65000:1"})},
		{"key element that is a JSON number", with("k", []any{"s:in_post", "s:10.0.0.0/24", 7})},
		{"key element that is a JSON object", with("k", []any{"s:in_post", map[string]any{"t": "s"}})},

		// These two are the only cases that reach the SECOND unmarshal and
		// are still invalid once it is gone, which is what makes them the
		// ones that pin its error branch. encoding/json accumulates type
		// errors and keeps decoding, so a wrong-typed s or k leaves the
		// field zero and ParseUint("") or decodeCursorKey("") fires instead
		// -- every other case here would pass for that reason with the
		// branch deleted. rib and k have no such downstream check, because
		// "" and nil are both LEGAL for them: measured with the branch
		// removed, a numeric rib decodes to RIB:"" (the walk silently widens
		// from one rib to every rib) and an object k decodes to Last:nil
		// (the walk silently restarts at the beginning). Those are two of
		// the exact failures query.RIBCursor's doc comment exists to name.
		{"rib as a JSON number", with("rib", 5)},
		{"k not an array", with("k", map[string]any{"rib": "in_post"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCursor(tc.cursor)
			if err == nil {
				t.Fatalf("decodeCursor(%q) accepted a bad cursor and returned %+v",
					tc.cursor, got)
			}
			if !errors.Is(err, errBadCursor) {
				t.Errorf("err = %v, which does not wrap errBadCursor -- the handler "+
					"has nothing to match on and must answer 500 for a failure "+
					"api/openapi.yaml types as a 400", err)
			}
			if got.Collector != "" || got.SessionID != 0 || got.Router.IsValid() ||
				got.Peer.IsValid() || got.RIB != "" || got.Last != nil {
				t.Errorf("a rejected cursor came back half-populated: %+v -- a caller "+
					"that ignores the error walks from a position nobody chose", got)
			}
		})
	}

	// The control. Without it every case above passes for a decoder that
	// refuses everything, which is a codec that cannot paginate at all.
	if _, err := decodeCursor(makeCursor(t, good)); err != nil {
		t.Fatalf("the payload every case above mutates one field of was itself "+
			"rejected: %v -- the whole table is vacuous", err)
	}
}

// TestCursorRejectsAFutureFormatForItsVersion is why the version is read by
// its own pass over the payload before any other field is looked at. A
// version-2 document is free to change every other field's SHAPE, so forcing
// it into today's struct first reports whichever field happened to change --
// "cannot unmarshal object into Go struct field .c of type string" -- for a
// cursor whose only real problem is that it is from another build. Checking
// err != nil cannot tell the two apart, so this asserts the message.
func TestCursorRejectsAFutureFormatForItsVersion(t *testing.T) {
	future := makeCursor(t, map[string]any{
		"v": cursorVersion + 1,
		// Everything else deliberately the wrong shape for version 1.
		"c": map[string]any{"name": "dev-c1"},
		"s": realSessionID,
		"k": map[string]any{"rib": "in_post"},
	})
	_, err := decodeCursor(future)
	if err == nil {
		t.Fatal("a version-2 cursor was accepted")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(cursorVersion+1)) {
		t.Errorf("err = %v -- it does not name the version, so a future format "+
			"reads as a corrupt one and the operator debugs the wrong thing", err)
	}
}

// TestCursorRejectsEveryTruncationOfItsOwnOutput is the fuzz-shaped version
// of the truncated-JSON case: no prefix of a real cursor may decode to
// anything, because a proxy or a query-string mangler that clips one is a
// silently different position rather than an error.
func TestCursorRejectsEveryTruncationOfItsOwnOutput(t *testing.T) {
	full := encodeCursor(cursorFor(t, []any{"in_post", "10.0.0.0/24", uint32(7)}))
	for i := range len(full) {
		if _, err := decodeCursor(full[:i]); err == nil {
			t.Errorf("decodeCursor accepted the %d-byte prefix %q of a real cursor",
				i, full[:i])
		}
	}
}

// TestCursorIsURLSafeAndUnpadded pins the alphabet, because the cursor's
// only transport is a query string. base64url has no + and no /, and the
// raw encoding has no = to be percent-encoded -- so a cursor survives being
// pasted into a URL, a shell, and a log line unchanged. Standard base64
// would not, and the damage shows up as a 400 on page 2 rather than here.
func TestCursorIsURLSafeAndUnpadded(t *testing.T) {
	for _, last := range [][]any{
		{"in_post", "10.0.0.0/24", uint32(7)},
		{"loc_rib", uint8(2), "65000:1", "10.0.0.5/32", "00:11:22:33:44:55",
			"2001:db8::5", uint32(4094), "00:11:22:33:44:55:66:77:88", uint32(1)},
	} {
		got := encodeCursor(cursorFor(t, last))
		if strings.ContainsAny(got, "+/=") {
			t.Errorf("cursor %q contains a character base64url does not use", got)
		}
		if strings.ContainsAny(got, "&?#% ") {
			t.Errorf("cursor %q contains a character a query string would eat", got)
		}
	}
}

// TestCursorIsNotSigned pins the decision the file comment argues for, so
// that adding an HMAC is a deliberate act that fails a test rather than a
// quiet "hardening" edit. The cursor carries a position, not an
// authorization: every caller has already presented a bearer token, and a
// signature here would buy nothing but a key to rotate and a class of 400s
// on cursors that were perfectly valid before a restart.
func TestCursorIsNotSigned(t *testing.T) {
	c := cursorFor(t, []any{"in_post", "10.0.0.0/24", uint32(7)})
	raw := decodeCursorPayload(t, encodeCursor(c))

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	for _, field := range []string{"sig", "mac", "hmac", "h", "sha", "tag"} {
		if _, ok := obj[field]; ok {
			t.Errorf("payload carries a %q field: %s -- the cursor is deliberately "+
				"unsigned; see the file comment before adding one", field, raw)
		}
	}
	// The observable half: a cursor built by hand, with no secret anywhere,
	// decodes. If a signature is ever added this is the test that says what
	// it breaks.
	byHand := makeCursor(t, map[string]any{
		"v": cursorVersion, "c": "dev-c1", "s": strconv.FormatUint(realSessionID, 10),
		"rtr": "10.77.0.33", "peer": "2001:db8::1", "rib": "in_post",
		"k": []string{"s:in_post", "s:10.0.0.0/24", "u32:7"},
	})
	got, err := decodeCursor(byHand)
	if err != nil {
		t.Fatalf("a hand-built cursor was rejected: %v", err)
	}
	assertSameCursor(t, got, c)
}

// TestCursorEncodesAnUncarryableKeyElementAsSomethingDecodeRefuses covers the
// one case encodeCursor cannot report: its signature returns no error, and
// query's key builders produce only the three types below, so an element of
// any other type is a bug in query rather than a caller's doing. The bug
// must not become a cursor that decodes to a DIFFERENT position -- an int
// silently rendered as a u32 would compare against a UInt32 column and walk
// on. It becomes a cursor nothing will accept, which is a loud 400 on page 2
// with the offending Go type in the message.
func TestCursorEncodesAnUncarryableKeyElementAsSomethingDecodeRefuses(t *testing.T) {
	for _, bad := range []any{
		int(7), int64(7), uint16(7), float64(7), true, nil,
		netip.MustParseAddr("10.0.0.1"), math.NaN(),
	} {
		got, err := decodeCursor(encodeCursor(cursorFor(t, []any{"in_post", bad})))
		if err == nil {
			t.Errorf("a %T key element round-tripped to %#v instead of being refused",
				bad, got.Last)
			continue
		}
		if !errors.Is(err, errBadCursor) {
			t.Errorf("a %T key element gave %v, which does not wrap errBadCursor", bad, err)
		}
	}
}

// TestCursorKeyRoundTripsUint64 covers node_key, which is UInt64. Without a
// u64 tag it falls to encodeCursorKey's default branch and comes back as the
// unusable "!uint64:..." form, so every link-state cursor would be rejected.
func TestCursorKeyRoundTripsUint64(t *testing.T) {
	const want = uint64(18446744073709551615) // 2^64-1, past what a u32 tag holds
	enc := encodeCursorKey(want)
	if enc != "u64:18446744073709551615" {
		t.Fatalf("encodeCursorKey(uint64) = %q, want the u64 tag", enc)
	}
	got, err := decodeCursorKey(enc)
	if err != nil {
		t.Fatalf("decodeCursorKey: %v", err)
	}
	if got != any(want) {
		t.Errorf("round-tripped %#v, want %#v -- a uint64 that returns as another type "+
			"compares wrong in ClickHouse without raising", got, want)
	}
}

// TestCursorKeyRejectsOversizedUint64 mirrors the u32/u8 range strictness: a
// truncated key is a valid-looking position somewhere else in the table.
func TestCursorKeyRejectsOversizedUint64(t *testing.T) {
	if _, err := decodeCursorKey("u64:18446744073709551616"); err == nil {
		t.Fatal("want an error for a value past 2^64-1")
	}
}

// TestCursorErrorsSayNothingAboutTheCursorsContents is a small blast-radius
// rule: the error text a 400 body will carry names the FIELD and the reason,
// never the raw cursor. A cursor is not a credential, but it is caller-
// supplied text that ends up in logs and in a response body, and echoing it
// back verbatim is how a reflected-content bug gets built by accident.
func TestCursorErrorsSayNothingAboutTheCursorsContents(t *testing.T) {
	const marker = "PAYLOAD_MARKER_THAT_MUST_NOT_BE_ECHOED"
	_, err := decodeCursor(base64.RawURLEncoding.EncodeToString(
		[]byte(`{"v":1,"c":"` + marker + `","s":"nope","k":[]}`)))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error echoes the cursor's contents: %v", err)
	}
}

// makeCursor builds a cursor payload from a map, for the tests that need a
// document encodeCursor would never produce. It goes through encoding/json
// rather than string concatenation so a case cannot accidentally test JSON
// syntax when it means to test a field's value.
func makeCursor(t *testing.T, obj map[string]any) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(mustMarshal(t, obj))
}

// decodeCursorPayload returns the JSON inside a cursor, for the tests whose
// subject is the wire shape rather than the round trip.
func decodeCursorPayload(t *testing.T, cursor string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatalf("cursor is not base64url: %v", err)
	}
	return raw
}
