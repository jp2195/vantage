package query

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ErrSessionChanged is returned, with no rows, when the BMP session a walk was
// pinned to is no longer the router's current one: the router reconnected
// part-way through the walk, and the pages already fetched describe a dump
// that no longer exists.
//
// It is a SEPARATE sentinel from ErrBadFilter, not a flavor of it, because the
// two are different failures with different remedies. ErrBadFilter means the
// caller asked a malformed question and asking it again changes nothing -- a
// 400. This means the question was fine and the world moved underneath it: the
// remedy is to restart the walk from page 1, which will pin the new session
// and succeed. api/openapi.yaml gives it its own status for that reason (409,
// code session_changed, on all three /v1/rib paths), and errors.Is is what
// lets cmd/vantage-api tell it from a 400 and from a 500 without matching on
// message text.
//
// The alternative to failing loudly here is the whole reason the pin exists.
// A walk that simply carried on would hand back a page from the NEW dump
// keyed off the OLD dump's last route -- rows that are individually true,
// assembled into a RIB that was never on the router: everything the old dump
// had past the cursor, silently missing, and everything the new dump has
// before it, silently missing too. That answer is indistinguishable from a
// correct one at the caller, which is exactly the confidently-wrong shape this
// package exists to prevent.
var ErrSessionChanged = errors.New("query: the session this walk was pinned to has been superseded")

// DefaultRIBPage and MaxRIBPage are api/openapi.yaml's own limit schema on the
// three /v1/rib paths: {minimum: 1, maximum: 10000, default: 1000}. A limit at
// or below zero means "not asked" and takes the default; one above the maximum
// is CLAMPED, not rejected -- a caller asking for more rows than the surface
// will give is not making a mistake, and answering 10000 of the 50000 it asked
// for, with a cursor for the rest, is the same answer in more calls.
//
// The default is 1000 rather than the 50 or 100 an offset-paginated API would
// pick, and the reason is the cost model spelled out in ribPage's own doc
// comment: a page is not cheaper than the whole peer's RIB, so 20 pages of 50
// cost 20 times what one page of 1000 costs. Anyone lowering this number is
// multiplying the work, not dividing it.
//
// Both are EXPORTED so that api/ can validate an operator's configured
// max_page against the ceiling this package will actually enforce, rather
// than against a copy of the number. api.Config lets an operator set
// max_page freely; without this, setting it above MaxRIBPage produced a
// daemon that accepted a limit the contract types as a 400 and then
// silently returned 10000 rows -- an answer that looks complete and is not.
// A second literal 10000 in api/ would be the fourth copy of a number that
// already lives here, in api/openapi.yaml and in that daemon's config, and
// the copy is what drifts.
const (
	DefaultRIBPage = 1000
	MaxRIBPage     = 10000
)

// RIBCursor is one walk's position: the (collector, session) the walk is
// pinned to, and the key of the last row the previous page returned.
//
// Collector and SessionID together are the pin, and the pair is the unit --
// session_id is assigned by each collector process from its own clock (see
// peerStateCTE), so a session_id means nothing without the collector that
// issued it, and two collectors watching one router can hold the same number
// at the same time with no contradiction. Last is the previous page's final
// route key, family-specific, in the order that family's key columns are
// declared: (rib, prefix, path_id) for unicast, (rib, rd, prefix, path_id) for
// VPN, and rib plus the whole NLRI tuple (route_type, rd, prefix, mac, ip,
// ethernet_tag, esi, path_id) for EVPN. Every key leads with rib -- see
// ribColumn for why it is part of the key and not merely a filter, and for the
// one thing about its Enum8 comparison a reader should not have to
// rediscover. A nil or empty Last with a Collector and SessionID set
// means "start at the beginning of THIS session" -- the only way to name a
// collector when a router has more than one (see ribPin).
//
// Last's elements must carry the Go types the family's key columns have --
// string for a String column, uint32 for a UInt32, uint8 for route_type, and
// string for rib's Enum8, whose cursor value is the member NAME every scan in
// this package already produces -- and a cursor whose shape or types do not
// match is refused with ErrBadFilter rather than bound and left for ClickHouse
// to raise on. That check is what
// makes api/openapi.yaml's "a tampered cursor is a 400" true.
//
// Router, Peer and RIB are the rest of the walk's SCOPE, and they are carried
// so that a page can refuse to answer a question different from the one the
// cursor came from. api/openapi.yaml puts router, peer, rib and cursor on the
// same path with no stated constraint between them, so a perfectly conforming
// client can send page 2 with a different rib, a different peer, or none --
// and every one of those is a silently wrong answer rather than an error,
// because a cursor is a POSITION and a position only means something within
// the walk that produced it:
//
//   - Change rib and the position is a rib boundary away from where the new
//     scope starts. Requesting rib=in_pre with a cursor last seen in in_post
//     returns an empty page and a nil cursor -- a walk that ends early and
//     says it finished.
//   - Drop rib and the walk silently WIDENS from one rib to every rib, from
//     the cursor's position onward: the rows before that position in the ribs
//     that were never being walked are skipped, and nothing reports it.
//   - Change peer and the walk pages another peer's table from this peer's
//     last key, skipping everything that sorts before it. Nothing else
//     catches this: peer is pinned by the statement but appears nowhere in
//     the key, so the position is silently reinterpreted against different
//     rows.
//   - Change router and the same thing happens. It is only ACCIDENTALLY
//     caught today, and not reliably: a different router usually resolves a
//     different max(session_id) and trips ErrSessionChanged, but two routers
//     can hold the same session_id -- nothing makes them differ, they are
//     two clocks with no shared counter (see peerStateCTE) -- and
//     insertRIBWalkFixture writes exactly that pair on purpose.
//
// So all three are required on any cursor and must equal the arguments the
// page was called with; a mismatch is ErrBadFilter. RIB is the walk's rib
// SCOPE, which is not the same thing as Last's leading element: Last[0] is
// the rib of the last ROW, and on a walk with no rib scope at all it is
// whichever rib that row happened to be in. Only a separate field can tell
// "this walk was scoped to in_pre" from "this walk spans every rib and has
// reached in_pre", which is what makes the dropped-rib case detectable.
//
// This type is a Go value, not a wire format. Whatever encodes it for the
// ?cursor= parameter has one hazard to respect and it is not a small one:
// SessionID is a UInt64 whose real values are now().UnixNano(), around 1.7e18,
// far past the 2^53 a JSON number survives intact. Encoded as a JSON number it
// comes back rounded, and a rounded session never equals the one on record --
// every second page would fail with ErrSessionChanged, for a router that never
// reconnected. Render it as a string, the way api/openapi.yaml already renders
// HistoryEvent.seq for the same reason. Last's uint32 and uint8 elements have
// no such problem, but they must come back as those types and not as the
// float64 a naive JSON decode produces.
type RIBCursor struct {
	Collector string
	SessionID uint64
	Router    netip.Addr
	Peer      netip.Addr
	RIB       string
	Last      []any
}

// ribKeyCol is one column of a family's route key: the SQL column, and the Go
// type a cursor value for it must have.
//
// The type is carried here, beside the column, rather than checked at the
// three call sites, because the two have to agree and there is nothing else to
// make them: a cursor element of the wrong type does not fail the way a
// mistyped filter does. ClickHouse compares a String to a UInt32 by raising,
// which is a 500 on a walk whose cursor a caller merely mangled -- and worse,
// some mismatches do NOT raise (a float64 against a UInt32 compares
// numerically, so a cursor round-tripped through JSON would silently walk on
// with values that are no longer the ones the last page ended at).
type ribKeyCol struct {
	col  string
	want reflect.Type
	// domain is the finite set of values the column can hold, or nil when
	// the column's domain is its whole Go type. Only rib has one, and only
	// because rib is the one key column backed by an Enum8: a String that is
	// not one of the enum's members does not compare, it RAISES
	// (UNKNOWN_ELEMENT_OF_ENUM), which turns a mangled cursor into a 500 for
	// a failure api/openapi.yaml types as a 400. Every other key column is a
	// String, a UInt8 or a UInt32 whose whole range is legal, so a type check
	// is already a domain check for them.
	domain map[string]bool
}

// ribColumn leads all three families' keys, and it is the one key column whose
// SQL type is not the Go type its cursor value carries: rib is
// Enum8('in_pre' = 0, 'in_post' = 1, 'out_pre' = 2, 'out_post' = 3,
// 'loc_rib' = 4) and the cursor carries the enum's NAME, the string every scan
// in this package already produces for Route.RIB.
//
// That works only because ClickHouse resolves both halves of the walk the same
// way, and the fact was measured rather than assumed. `ORDER BY r.rib` sorts by
// the enum's NUMERIC value, and a tuple comparison against a bound String
// converts the string to that same numeric value -- so `(rib, ...) > ('in_pre',
// ...)` is true for an in_post row, matching the order the ORDER BY put it in.
// The other reading is not far-fetched and would be silently wrong: compared as
// text, 'in_post' sorts BEFORE 'in_pre', which is the exact pair
// insertRIBWalkFixture exercises, and a walk whose predicate disagreed with its
// ordering by one column skips and repeats rows while every page still looks
// well formed. Verified on 24.8.14.39 against this package's own test database.
//
// It is a shared value rather than three copies for the reason peerStateCTE is:
// the argument above has to hold for all three families and there is nothing to
// keep three copies of it in agreement.
var ribColumn = ribKeyCol{col: "r.rib", want: reflect.TypeFor[string](), domain: ribMembers}

// ribMembers is the rib Enum8's member set, and it is spelled out here rather
// than derived, which is a fourth copy of a list that also lives in
// deploy/clickhouse/schema.sql, api/openapi.yaml and sink's own ribName.
// Nothing in Go can be derived from: sink.ribName is unexported and in a
// package this one must not depend on, and unlike the family tokens (see
// unicastFamilies, derived through subjects.FamilyToken precisely so a rename
// cannot drift) there is no registry behind the rib names -- they are the
// crossing of RFC 8671's O and L flags plus RFC 9069's Loc-RIB, fixed by the
// protocol and spelled independently at each of those four places already.
//
// So it is pinned instead of derived: TestRIBMembersMatchTheSchemaEnum reads
// the enum's own definition back out of ClickHouse, for all three route
// tables, and compares it to this set. That ties the copy to the DDL the tests
// actually apply -- the shipped one, via chtest -- which is a stronger tie
// than any Go-side constant could give it, since the DDL is what the column
// will really accept.
var ribMembers = map[string]bool{
	"in_pre": true, "in_post": true, "out_pre": true, "out_post": true, "loc_rib": true,
}

// ValidRIB reports whether rib names one of the rib column's Enum8 members,
// and RIBNames lists them for an error message.
//
// They exist for api/, and the division of labor they encode is the one
// ribColumn's doc comment argues for: a bad ?rib= FILTER is caught above
// this package, against the contract's own enum, because an unknown Enum8
// member does not compare false in ClickHouse -- it raises
// UNKNOWN_ELEMENT_OF_ENUM, which would be a 500 for a failure the contract
// types as a 400. A bad rib inside a CURSOR is caught here instead, by
// ribColumn's domain check, because a cursor is opaque by construction and
// no layer above can look into one.
//
// Both read ribMembers rather than restating it, so the set an HTTP handler
// validates against is the same set TestRIBMembersMatchTheSchemaEnum ties
// to the shipped DDL. A hand-written enum in api/ would be a fifth copy,
// and the one furthest from the column that decides.
func ValidRIB(rib string) bool { return ribMembers[rib] }

// RIBNames returns the members in sorted order, for the "which values are
// legal" half of a rejection message.
func RIBNames() []string { return slices.Sorted(maps.Keys(ribMembers)) }

// unicastRIBKey, vpnRIBKey and evpnRIBKey are the three families' route keys,
// in keyset order. Each is exactly the part of its statement's own GROUP BY
// that the walk does not already pin: (collector, router, peer) are fixed for
// the whole walk, so what remains -- rib included -- is what distinguishes one
// of that peer's routes from another, and the key is therefore UNIQUE within a
// page set. That uniqueness is the walk's correctness condition, not a nicety:
// a duplicated key is a tie, a tie can straddle a page boundary, and a
// straddled tie is a row dropped from the answer with nothing anywhere to
// notice it.
//
// rib is IN the key rather than merely a filter, and that is the whole reason
// an unset rib is a legitimate walk here instead of a refusal. The same
// (prefix, path_id) really does appear under several ribs for one peer -- the
// live archive carries 10.99.1.0/24 and 10.99.2.0/24 under in_pre, in_post AND
// loc_rib simultaneously -- so a key without rib has a tie for every such row,
// and "show me everything this peer sent me" is a question api/openapi.yaml
// documents (rib is optional on all three /v1/rib paths) rather than one to
// legislate away.
//
// Ordering by rib first also matches the SHAPE of every route table's sort
// key, which leads with (router_ip, peer_ip, rib, ...). That is a statement
// about shape and it is where the statement stops: nothing here claims the
// ordering is therefore cheap. It could not honestly -- the sort key
// continues (ts_router, stream_seq, prefix, path_id), so within one
// (router, peer, rib) the rows on disk are in arrival order and not in key
// order, and this ORDER BY sorts a GROUP BY result in any case. An
// unmeasured cost claim here would not be trustworthy; see ribPage on
// per-page cost for the one place cost is discussed at all, and note that
// it too says outright that nothing was measured.
//
// evpnRIBKey is rib plus the whole NLRI tuple, for the reason EVPNRoute's own
// doc comment gives: an EVPN route has no prefix to be keyed on, and two rows
// differing only in esi or only in ip are two routes.
//
// r.family is the one column routesSQL's and vpnRoutesSQL's GROUP BYs carry
// that these keys deliberately do not, so a reader deriving a key from a
// GROUP BY should not add it: a page key exists to break TIES, and family
// breaks none -- within one peer and one rib it is determined by the rest of
// the key. See routesSQL's own doc comment for the full argument, including
// what would have to change if that determination ever stopped holding.
//
// unicastRIBKey deliberately does NOT carry family, though routesSQL groups by
// it -- so the page key is one column short of that statement's GROUP BY
// remainder, which is the very shape the rib argument above condemns. The
// difference is what backs each claim, and it is worth being exact about
// rather than asserting symmetry.
//
// For rib, the tie is REAL and was found: the live archive carries
// 10.99.1.0/24 and 10.99.2.0/24 under in_pre, in_post and loc_rib at once. For
// family, the tie would need one prefix STRING to appear under both ipv4u and
// ipv6u, and the two text spaces are disjoint -- sink writes prefixes through
// netip.Prefix.String(), so an IPv4 NLRI renders "10.0.0.0/8" and never in
// mapped form, which api/openapi.yaml states outright at its own covers
// parameter. No such row exists in the archive and no conforming collector
// produces one.
//
// That is an argument from the writer's behavior, not a constraint
// route_unicast enforces, and it is weaker than the rib case by exactly that
// much. It is recorded here rather than acted on because there is no tie to
// break: family adds a column to the cursor and changes no answer, so
// widening the key would be paying a cost against a case nobody can produce.
//
// The key is also the contract's -- api/openapi.yaml names (rib, prefix,
// path_id) for /v1/rib/unicast -- but that is a reason to change both
// together, not a reason not to. This contract is hand-written in this repo
// and was amended twice already (rib entered all three page keys;
// covers= entered the VPN and EVPN filters), so "it is in the contract" is
// not an argument on its own and should not be read as one here. If a
// malformed feed ever puts one prefix text under two families, this is the
// paragraph that was wrong, and the fix is one column in each of two
// places.
var (
	unicastRIBKey = []ribKeyCol{
		ribColumn,
		{col: "r.prefix", want: reflect.TypeFor[string]()},
		{col: "r.path_id", want: reflect.TypeFor[uint32]()},
	}
	vpnRIBKey = []ribKeyCol{
		ribColumn,
		// r.rd, the column, never the `vrf` alias vpnRoutesSQL gives it in
		// its SELECT list: this key is rendered into the WHERE and into the
		// ORDER BY, and both must name the same thing. See vpnRoutesOrder.
		{col: "r.rd", want: reflect.TypeFor[string]()},
		{col: "r.prefix", want: reflect.TypeFor[string]()},
		{col: "r.path_id", want: reflect.TypeFor[uint32]()},
	}
	evpnRIBKey = []ribKeyCol{
		ribColumn,
		{col: "r.route_type", want: reflect.TypeFor[uint8]()},
		{col: "r.rd", want: reflect.TypeFor[string]()},
		{col: "r.prefix", want: reflect.TypeFor[string]()},
		{col: "r.mac", want: reflect.TypeFor[string]()},
		{col: "r.ip", want: reflect.TypeFor[string]()},
		{col: "r.ethernet_tag", want: reflect.TypeFor[uint32]()},
		{col: "r.esi", want: reflect.TypeFor[string]()},
		{col: "r.path_id", want: reflect.TypeFor[uint32]()},
	}
)

// keyOrder renders "\nORDER BY col1, col2, ..." for key, with no LIMIT --
// the shared base every page ORDER BY in this package is built from,
// including link-state's (see lsNodesPageOrder).
//
// It is DERIVED from the same []ribKeyCol the keyset predicate is built from,
// rather than written out per family, and that is the single most important
// structural decision in this file. The predicate says "give me rows past
// here" and the ordering says "here is what past means"; if the two ever name
// different columns, or the same columns in a different order, the walk skips
// rows and repeats others and every individual page still looks entirely
// plausible. One slice, read twice, cannot disagree with itself. An ORDER BY
// written out by hand a few lines from its key declaration could, and
// nothing but a full-walk test over a fixture big enough to page would
// notice -- see TestRIBOrderAndKeysetNameTheSameColumns, which pins the
// agreement this function's existence is what makes possible.
//
// The direction is ascending and unwritten, matching the `>` in keysetWhere.
// Both would have to change together to walk backwards; neither is
// parameterized, because a descending walk is not a thing any /v1/rib or
// /v1/ls path offers.
func keyOrder(key []ribKeyCol) string {
	cols := make([]string, len(key))
	for i, k := range key {
		cols[i] = k.col
	}
	return "\nORDER BY " + strings.Join(cols, ", ")
}

// ribOrder renders the tail every RIB page statement carries: ORDER BY the
// key, then a bound LIMIT. It is keyOrder plus that placeholder, split out
// as its own function only because the three RIB statements bind LIMIT as a
// `?` where the link-state statements splice in a literal (see
// lsNodesPageOrder and LSNodesPage's own doc comment for why); the ORDER BY
// half itself, and the correctness argument behind it, is keyOrder's, not
// this function's.
func ribOrder(key []ribKeyCol) string {
	return keyOrder(key) + "\nLIMIT ?"
}

// keyset adds the keyset-pagination predicate for one page: nothing at all on
// the first page of a walk, and `(c1, c2, ...) > (?, ?, ...)` on every page
// after it, one placeholder per key column and one value appended for each --
// the invariant filters.go's own header states, held here the same way covers
// holds it.
//
// It is a method on *filters but lives here rather than in filters.go because
// it is the only predicate in the package that is not a narrowing a caller
// asked for: the other methods render a question, this one renders a position.
//
// A tuple comparison, not the unrolled `a > ? OR (a = ? AND b > ?)` chain: the
// unrolled form needs n(n+1)/2 placeholders for an n-column key, which for
// EVPN's eight-column NLRI is thirty-six, each of which has to be bound in the
// right order by hand. ClickHouse compares tuples lexicographically, which is
// exactly the semantics the unrolled chain is spelling out, so the tuple form
// is the same predicate with eight placeholders and no arithmetic to get
// wrong.
//
// It validates last against key rather than trusting it, and returns
// ErrBadFilter on a mismatch, because a cursor is the one input to this
// package that makes a round trip through an untrusted encoding: it is handed
// to a client as an opaque string and handed back. A wrong LENGTH would
// otherwise build a tuple comparison of mismatched arity, and a wrong TYPE is
// worse than that -- see ribKeyCol for why some type mismatches raise and some
// quietly compare against something else.
func (f *filters) keyset(key []ribKeyCol, last []any) error {
	if len(last) == 0 {
		return nil
	}
	if len(last) != len(key) {
		return fmt.Errorf("%w: cursor carries %d key values, want %d for this family",
			ErrBadFilter, len(last), len(key))
	}
	for i, k := range key {
		if got := reflect.TypeOf(last[i]); got != k.want {
			return fmt.Errorf("%w: cursor value %d for %s has type %v, want %v",
				ErrBadFilter, i, k.col, got, k.want)
		}
		// The domain check, for the one column that has one. A type check is
		// not a domain check when the column is an Enum8: an out-of-domain
		// String does not compare false, it makes ClickHouse raise
		// UNKNOWN_ELEMENT_OF_ENUM, and a mangled cursor is then a 500 for a
		// failure the contract types as a 400. Letting the server raise on a
		// bad ?rib= FILTER is a different bargain and still the right one --
		// an HTTP layer can validate that against the contract's own enum
		// before it ever arrives -- but a cursor is opaque by construction,
		// so no layer above this one can check it and this one must.
		if k.domain != nil && !k.domain[last[i].(string)] {
			return fmt.Errorf("%w: cursor value %d for %s is not one of "+
				"that column's values (%s)",
				ErrBadFilter, i, k.col, strings.Join(slices.Sorted(maps.Keys(k.domain)), ", "))
		}
	}
	f.conds = append(f.conds, tupleCompare(key, ">"))
	f.args = append(f.args, last...)
	return nil
}

// atMost is keyset's upper half: `(c1, c2, ...) <= (?, ?, ...)`, closing a
// page's key range at the top the way keyset opens it at the bottom. Together
// they render exactly the span one page covers.
//
// It takes no error return and validates nothing, and that asymmetry with
// keyset is the point. keyset's bound is a CURSOR -- it has made a round trip
// through an opaque encoding in a client's hands, so its arity and its types
// are inputs to be checked. This bound is the last key phase one just read out
// of the database through this same key declaration, so its arity and types
// are correct by construction. Validating it would be checking this package
// against itself.
//
// It DOES check the bound's arity, and returns an error rather than rendering
// nothing, because the two failures are not comparable. A cursor with the
// wrong arity is a bad request. A BOUND with the wrong arity is this package
// failing to bound its own second statement, and rendering nothing for it
// would leave phase two reading the peer's whole RIB from the cursor onward
// -- an answer ribPage still truncates to the right rows, so no test fails,
// no caller notices, and the entire speedup this design exists for is gone
// silently. An invisible failure is the one worth paying an error return for.
func (f *filters) atMost(key []ribKeyCol, bound []any) error {
	if len(bound) != len(key) {
		return fmt.Errorf("%w: a page's upper bound has %d values for a %d-column key",
			ErrBadFilter, len(bound), len(key))
	}
	f.conds = append(f.conds, tupleCompare(key, "<="))
	f.args = append(f.args, bound...)
	return nil
}

// tupleCompare renders `(c1, c2, ...) OP (?, ?, ...)` for a page key.
//
// One rendering, called by both bounds, for the reason keyOrder gives about
// itself: the predicate that opens a page's span and the one that closes it
// have to agree on the column list, the order and the placeholder count, and
// two copies of that rendering are two chances for them to stop agreeing.
func tupleCompare(key []ribKeyCol, op string) string {
	cols := make([]string, len(key))
	marks := make([]string, len(key))
	for i, k := range key {
		cols[i] = k.col
		marks[i] = "?"
	}
	return "(" + strings.Join(cols, ", ") + ") " + op + " (" + strings.Join(marks, ", ") + ")"
}

// clampRIBLimit resolves a caller's page size against api/openapi.yaml's own
// bounds: at or below zero takes the default, above the maximum is clamped to
// it, anything between is honored exactly. See DefaultRIBPage for why the
// default is as large as it is.
//
// Clamped rather than rejected, deliberately. A limit of 50000 is not a
// malformed request; it is a caller who wants the table and does not know this
// surface's ceiling. Returning 10000 rows and a cursor answers that question
// completely, in more calls than they hoped for -- an error would answer none
// of it and tell them to guess again.
func clampRIBLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultRIBPage
	case limit > MaxRIBPage:
		return MaxRIBPage
	default:
		return limit
	}
}

// ribScopeText renders a walk's rib scope for an error message, so that the
// difference between "scoped to in_pre" and "across every rib" reads as the
// difference it is rather than as an empty pair of quotes.
func ribScopeText(rib string) string {
	if rib == "" {
		return "across every rib"
	}
	return "scoped to rib " + rib
}

// ribPinSQL resolves which (collector, session) a walk over one router would
// be pinned to: one row per collector that has ever recorded a peer event for
// it, carrying that collector's own newest session.
//
// It MIRRORS peerStateCTE's cur exactly -- the same table, the same
// max(session_id), the same (collector, router) grain -- and it has to. cur is
// what the page statement's own INNER JOIN scopes route rows by, so a pin
// resolved any other way would pin the walk to a session the page query itself
// does not consider current, and every page would come back empty. The one
// difference is shape rather than meaning: this narrows to a single router in
// its WHERE and therefore groups by collector alone, where cur has no WHERE
// and groups by both. TestRIBPagePinMatchesTheCurrentSession holds the two
// together against Routers' own reading, without either naming the other's SQL
// text.
//
// max(session_id) is not a count of anything, so uniqExact does not apply
// here. The alias is `sid`, not `session_id`, for the alias-shadowing reason
// peerStateCTE's doc comment gives at length -- cur spells it the same way.
//
// It reads peer_current, not peer_events, for the reason peerStateCTE's own
// doc comment gives: peer_current has no TTL, so a walk can still pin to a
// long-lived session after peer_events' own retention would have expired
// the rows that first established it.
const ribPinSQL = `
SELECT collector_id, max(session_id) AS sid
FROM %[1]s.peer_current
WHERE router_ip = toIPv6(?)
GROUP BY collector_id
ORDER BY collector_id`

// ribSessionSQL is one pinned collector's current session for one router: the
// same question ribPinSQL asks, narrowed to the collector already pinned, and
// it is what turns a mid-walk reconnect into ErrSessionChanged.
//
// A router this collector has no peer_events row for at all yields 0, not an
// empty result: max over no rows is the type's zero for a non-nullable UInt64,
// and one row comes back regardless. That is the right answer here rather than
// a case to special-case -- a collector that has no record of the router
// certainly does not have the walk's session current -- and it is why this
// returns a plain uint64 with no "found" flag beside it. No real session_id is
// 0: they are now().UnixNano() values (see peerStateCTE).
//
// It reads peer_current, not peer_events, for the same reason ribPinSQL does:
// peer_current has no TTL, so a mid-walk reconnect check still sees the
// session peer_events' own retention would already have expired.
const ribSessionSQL = `
SELECT max(session_id) AS sid
FROM %[1]s.peer_current
WHERE collector_id = ? AND router_ip = toIPv6(?)`

// ribPin resolves the (collector, session) a fresh walk over router pins
// itself to. An empty collector with a nil error means the router has no
// session on record at all.
//
// A router two collectors are both watching is refused rather than resolved,
// and that is the point of returning an error here instead of picking one.
// Both plausible ways to pick are wrong in the same direction: taking
// max(session_id) across collectors resolves the router to whichever
// collector's clock is momentarily ahead and silently discards the other's
// entire view (the defect peerStateCTE's own doc comment describes at length),
// and taking the first collector alphabetically discards it just as silently
// with less to say for itself. A walk is pinned to ONE session by
// construction, so there is a real choice to be made here and no information
// with which to make it -- the caller has it, this function does not.
//
// The remedy is in the error text and it is a real one, not an apology:
// Routers returns one row per (collector, router), each with its own
// SessionID, so a caller facing this can name the collector it means by
// starting the walk with a RIBCursor carrying that pair and an empty Last.
//
// The error wraps ErrBadFilter, not ErrSessionChanged and not nothing at all.
// That is the right sentinel for what a caller can do about it: ErrBadFilter is
// this package's mark for "the CALLER got this wrong", which cmd/vantage-api
// renders as a 400, and the defining property of a 400 is that the caller can
// supply something better. Here they can -- a cursor naming the collector -- so
// the request really is under-specified rather than unserviceable. An
// unwrapped error would leave the daemon nothing to match on but message text
// and no choice but a 500, which api/openapi.yaml has no case for on these
// paths and which tells the caller the fault was ours.
func (q *Q) ribPin(ctx context.Context, router netip.Addr) (string, uint64, error) {
	rows, err := q.conn.Query(ctx, fmt.Sprintf(ribPinSQL, q.db), router.String())
	if err != nil {
		return "", 0, fmt.Errorf("resolve rib walk session: %w", err)
	}
	defer rows.Close()

	var collectors []string
	var sids []uint64
	for rows.Next() {
		var c string
		var sid uint64
		if err := rows.Scan(&c, &sid); err != nil {
			return "", 0, fmt.Errorf("scan rib walk session: %w", err)
		}
		collectors = append(collectors, c)
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("resolve rib walk session: %w", err)
	}
	switch len(collectors) {
	case 0:
		return "", 0, nil
	case 1:
		return collectors[0], sids[0], nil
	default:
		return "", 0, fmt.Errorf("%w: router %s is monitored by %d collectors (%s); "+
			"a RIB walk is pinned to one (collector, session) and there is nothing here "+
			"to choose with -- ask Routers for each collector's own session and start the "+
			"walk with a RIBCursor naming the one you want",
			ErrBadFilter, router, len(collectors), strings.Join(collectors, ", "))
	}
}

// RIBSessionFor reports the session one NAMED collector currently holds for
// router, so a caller facing ribPin's "monitored by N collectors" refusal can
// answer it.
//
// It is the exported half of the remedy RIBCursor's doc comment describes: a
// walk pinned to a collector needs that collector's own session_id, session
// ids are minted per collector and mean nothing across them, and until this
// existed the only way to obtain one was to read every router in the fleet
// through Routers and filter. api/ calls it to turn a collector= parameter
// into the cursor a pinned walk starts from.
//
// A zero session with a nil error means this collector has no record of this
// router -- see ribSessionSQL, where max over no rows is the type's zero and
// no real session_id is 0. That is a CALLER error rather than an empty
// answer, and the caller is the one placed to say so: it named a collector
// that is not watching this router.
func (q *Q) RIBSessionFor(ctx context.Context, router netip.Addr, collector string) (uint64, error) {
	return q.ribCurrentSession(ctx, collector, router)
}

// RIBStart returns the cursor a RIB walk pinned to one named collector starts
// from: that collector's current session for router, with nothing read yet.
// It is how a caller answers ribPin's "monitored by N collectors" refusal, and
// it is the single place a collector name becomes a pin, shared by api/'s
// collector= parameter and the CLI's -collector flag so the two cannot
// disagree about what a pinned walk means.
//
// An empty collector returns a nil cursor, which leaves the choice to ribPin:
// a router watched by one collector needs no name. A collector that has no
// session for router is ErrBadFilter, since the caller named a collector that
// is not watching this router.
func (q *Q) RIBStart(ctx context.Context, router, peer netip.Addr, rib, collector string) (*RIBCursor, error) {
	if collector == "" {
		return nil, nil
	}
	sid, err := q.RIBSessionFor(ctx, router, collector)
	if err != nil {
		return nil, err
	}
	if sid == 0 {
		return nil, fmt.Errorf("%w: the named collector has no session for router %s -- "+
			"ask Routers which collectors monitor it", ErrBadFilter, router)
	}
	return &RIBCursor{
		Collector: collector, SessionID: sid,
		Router: router, Peer: peer, RIB: rib,
	}, nil
}

// ribCurrentSession reports what collector currently considers router's
// session to be, so ribPage can tell a superseded pin from a live one.
func (q *Q) ribCurrentSession(ctx context.Context, collector string, router netip.Addr) (uint64, error) {
	var sid uint64
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(ribSessionSQL, q.db),
		collector, router.String()).Scan(&sid); err != nil {
		return 0, fmt.Errorf("check rib walk session: %w", err)
	}
	return sid, nil
}

// ribCursorScope resolves the (collector, session, last-key) one page of a
// scoped walk should run against, either restored and validated from cur or
// freshly pinned via ribPin when cur is nil. It is the validation every
// scoped walk in this package shares -- ribPage today, LSNodesPage, and the
// link-state walks after it -- factored out so that a check added here is
// added for all of them at once, rather than re-typed at each call site with
// its own chance to drift. It was extracted directly from ribPage's own
// first form, unchanged.
//
// An empty collector with a nil error means router has no session on record
// at all: the caller's walk is over before it starts, with no rows and no
// cursor, not an error -- Routes says the same thing about a router it has
// never heard of.
//
// The scope check against a non-nil cur runs before ribPin would ever be
// consulted, because a cursor is a POSITION, and a position only means
// something inside the walk that produced it: pinning to a DIFFERENT
// (router, peer, rib) than the one recorded on the cursor would silently
// resume a walk from a position that has nothing to do with the new scope,
// skipping every row that sorts before it with nothing to report the loss.
func ribCursorScope(ctx context.Context, q *Q, cur *RIBCursor, router, peer netip.Addr, rib string) (
	collector string, sid uint64, last []any, err error) {
	if cur != nil {
		collector, sid, last = cur.Collector, cur.SessionID, cur.Last
		if collector == "" || sid == 0 {
			return "", 0, nil, fmt.Errorf("%w: a cursor must carry both a collector and a "+
				"session -- a session_id is issued by one collector's own clock and means "+
				"nothing without it (see peerStateCTE)", ErrBadFilter)
		}
		if cur.Router != router || cur.Peer != peer {
			return "", 0, nil, fmt.Errorf("%w: this cursor was issued for a walk of "+
				"router %s peer %s and cannot be used on a walk of router %s peer %s -- "+
				"its position is the last key of a DIFFERENT table, and paging one "+
				"peer's rows from another peer's position skips every row that sorts "+
				"before it without reporting anything",
				ErrBadFilter, cur.Router, cur.Peer, router, peer)
		}
		if cur.RIB != rib {
			return "", 0, nil, fmt.Errorf("%w: this cursor was issued for a walk with "+
				"a different rib scope and cannot be used on a walk %s -- changing the "+
				"rib mid-walk leaves the "+
				"position a rib boundary away from where the new scope starts (an empty "+
				"page and a nil cursor, which reads as a finished walk), and dropping it "+
				"silently widens the walk to every rib from this position onward",
				ErrBadFilter, ribScopeText(rib))
		}
		return collector, sid, last, nil
	}
	collector, sid, err = q.ribPin(ctx, router)
	return collector, sid, nil, err
}

// sessionStillCurrent reports whether the (collector, sid) a page's query
// already ran against is still router's current session, and returns
// ErrSessionChanged if it is not.
//
// Every scoped walk in this package calls this AFTER its page query runs,
// never before -- see ribPage's own doc comment for why that order is the
// difference between a loud failure and a silent truncation. The argument
// applies with extra force to a link-state walk: lsNodesSQL's own INNER JOIN
// against cur means a superseded session does not merely risk answering from
// a stale dump, it makes the page query itself return ZERO rows, which is
// indistinguishable from "the walk finished" if this check were skipped or
// run first. Checked after, that same reconnect is caught instead of read as
// success.
func (q *Q) sessionStillCurrent(ctx context.Context, collector string, sid uint64, router netip.Addr) error {
	now, err := q.ribCurrentSession(ctx, collector, router)
	if err != nil {
		return err
	}
	if now != sid {
		return fmt.Errorf("%w: the walk is pinned to its collector's session %d, "+
			"which is no longer that collector's current session for %s (%d) -- the "+
			"router reconnected and "+
			"re-dumped, so the pages already fetched describe a dump that no longer "+
			"exists; restart the walk without a cursor",
			ErrSessionChanged, sid, router, now)
	}
	return nil
}

// unicastRIBKeysSQL is phase one of a unicast RIB page: the same question
// routesSQL answers, asked only about WHICH ROUTES are on the page rather than
// what they carry.
//
// It is routesSQL with the eleven argMax attribute columns removed, and with
// them the two LEFT JOINs that exist only to feed dump_state -- eor and
// peer_state. The cur and peer_up joins, the WHERE, the GROUP BY and the
// withdrawal HAVING are unchanged. Dropping a LEFT JOIN cannot change which
// rows survive and the GROUP BY collapses any fan-out it produced, so the two
// statements still resolve the same route keys; this enumeration is exact
// because it is the only place a reader is told what the two differ by. That is the entire optimization, and the
// 2026-09-05 measurements are what say so: against a peer holding 1,000,000
// routes, the four CTE joins cost almost nothing (0.104s bare, 0.136s with all
// four) while the eleven argMax columns cost 0.449s. The cost of a page was
// never the rows read -- read_rows is the same 1.5M either way -- it is eleven
// argMax state machines run across the peer's whole RIB. This statement runs
// ONE, for the withdrawal filter, which is not optional: a withdrawn route is
// not on the page, and deciding that requires resolving its newest observation.
//
// The session scoping, the peer_up gate and the GROUP BY are shared with
// routesSQL by being written the same way rather than by being factored out,
// which is the one place this design pays for itself twice. They are the two
// things that are invisible when wrong (see peerStateCTE), so
// TestRIBPageUnicastTwoPhaseMatchesOneStatement compares this statement's page
// against routesSQL's own, row for row, over a fixture built so that a reader
// which resolved current state differently would produce different values --
// not merely a different row count.
//
// Which of those three narrowings a test can actually catch is not symmetric,
// and a mutation pass on 2026-09-05 settled it by measurement rather than by
// reading:
//
//   - The GROUP BY is caught structurally. Dropping r.family or r.peer_ip from
//     it fails TestRIBPageKeysAreTheirStatementsUnpinnedGroupBy, which holds
//     this statement's granularity against unicastRIBKey exactly as it holds
//     routesSQL's.
//   - The session scope is deliberately enforced TWICE, so deleting
//     `AND r.session_id = cur.sid` from the cur join below changes no answer:
//     ribPageFilters binds r.session_id = ? on every phase of every page.
//     Dropping both halves fails eight tests across the walk, Routes and
//     RouteHistory; dropping either half alone fails nothing that reads an
//     answer. The join condition is the legible half of one pin, not a second
//     mechanism, and this is the measurement behind ribPageFilters' own
//     "belt to the cur join's braces".
//   - The peer_up gate cannot be caught through a page at all. It is caught by
//     comparing this statement's keys to routesSQL's rows directly, which is
//     what TestRIBUnicastKeysPhaseMatchesTheAttributesPhase does; its doc
//     comment carries the argument for why no page can see the gate go.
//
// It selects the page key and groups by the route key, family included, for
// the reason routesSQL's own GROUP BY carries family: the group must hold one
// family or nothing downstream may assume it does. family is not in the SELECT
// list because nothing here reads it -- phase two re-derives every attribute
// from the base table, and this statement's only output is a position.
//
// peer_state comes along inside peerStateCTE unused. It is one aggregate over
// peer_events, which is peer-grained and does not grow with the route tables
// (2,843 rows against 8,611 in the archive, and the ratio only widens), and
// taking peerUpCTE without it is not possible -- peer_up is defined in terms
// of cur, and cur is defined in peerStateCTE. Sharing the definition is worth
// more than dropping the aggregate.
const unicastRIBKeysSQL = "WITH " + peerStateCTE + peerUpCTE + `
SELECT r.rib, r.prefix, r.path_id
FROM %[1]s.route_unicast_current r
INNER JOIN cur
    ON r.collector_id = cur.collector_id
   AND r.router_ip    = cur.router_ip
   AND r.session_id   = cur.sid
INNER JOIN peer_up
    ON r.collector_id = peer_up.collector_id
   AND r.router_ip    = peer_up.router_ip
   AND r.peer_ip      = peer_up.peer_ip
   AND cur.sid        = peer_up.sid
WHERE ` + servedGate + `%[2]s
GROUP BY r.collector_id, r.router_ip, r.peer_ip, r.rib, r.family, r.prefix, r.path_id
HAVING argMax(r.is_withdraw, (r.seq, r.stream_seq)) = 0%[3]s`

// ribSpec is everything that differs between the three families' walks. The
// paging itself -- the pin, the clamp, the keyset predicate, the probe row,
// the supersession check -- is identical for all three and lives in ribPage,
// which is generic over the row type so it can be written once rather than
// three times with three chances to get the cursor arithmetic wrong.
type ribSpec[T any] struct {
	// what names the family in error text ("unicast", "vpn", "evpn").
	what string
	// body is the family's current-state statement without an ORDER BY:
	// routesSQL, vpnRoutesSQL or evpnRoutesSQL. ribOrder supplies the tail.
	body string
	// key is the family's route key, in keyset order.
	key []ribKeyCol
	// lead is the values bound BEFORE the filter's own, for a statement whose
	// placeholders do not all come from the filter. Only EVPN has any (the
	// eor join's family token, which sits above the outer WHERE); the driver
	// binds strictly left to right and silently discards a surplus argument,
	// so the order here is not cosmetic. See evpnRoutesStatement.
	lead []any
	// scan and keyOf are the family's own scan loop and the key it extracts
	// from a row for the next page's cursor. keyOf must produce values in
	// key's order and with key's types; TestRIBCursorKeysMatchTheirColumns
	// holds both.
	scan  func(driver.Rows) ([]T, error)
	keyOf func(T) []any
	// keysBody is the family's PAGE-KEY statement, and setting it is what
	// makes the walk fetch a page in two phases instead of one. It carries
	// the same session scoping, the same peer_up gate and the same withdrawal
	// filter body does, and selects nothing but the route key -- so it runs
	// ONE argMax where body runs eleven, which is the whole difference the
	// 2026-09-05 measurements found (652-677ms a page against 87-117ms,
	// measured twice against the same rig). Empty means
	// the family runs body as a single statement, which is what VPN and EVPN
	// still do; see ribFetch.
	keysBody string
}

var (
	unicastRIBSpec = ribSpec[Route]{
		what: "unicast", body: routesSQL, key: unicastRIBKey, scan: scanRoutes,
		keysBody: unicastRIBKeysSQL,
		keyOf:    func(r Route) []any { return []any{r.RIB, r.Prefix, r.PathID} },
	}
	vpnRIBSpec = ribSpec[VPNRoute]{
		what: "vpn", body: vpnRoutesSQL, key: vpnRIBKey, scan: scanVPNRoutes,
		keyOf: func(r VPNRoute) []any { return []any{r.RIB, r.RD, r.Prefix, r.PathID} },
	}
	evpnRIBSpec = ribSpec[EVPNRoute]{
		what: "evpn", body: evpnRoutesSQL, key: evpnRIBKey, scan: scanEVPNRoutes,
		// evpnFamily leads because the eor join's placeholder sits above the
		// outer WHERE in the finished statement -- the same ordering
		// evpnRoutesStatement documents for EVPNRoutes.
		lead: []any{evpnFamily},
		keyOf: func(r EVPNRoute) []any {
			return []any{
				r.RIB, r.RouteType, r.RD, r.Prefix, r.MAC, r.IP, r.EthernetTag, r.ESI, r.PathID,
			}
		},
	}
)

// ribStatement renders one page of a walk and returns it with the values its
// placeholders bind, in the order the driver binds them.
//
// It is split out of ribPage for the reason evpnRoutesStatement is split out
// of EVPNRoutes: so a test can hold it to this package's binding invariant --
// one `?` emitted, one value appended -- and to the clamp, without a live
// database. Both matter more here than on a statement whose placeholders all
// come from one filter rendering, because this one interleaves three sources:
// the EVPN family token that binds ahead of the WHERE, the filter's own
// values, and the LIMIT that binds after everything.
//
// The limit bound is the clamped page size PLUS ONE, and that extra row is a
// probe rather than an off-by-one. api/openapi.yaml says next_cursor is
// non-null "only when another page exists", so the statement has to actually
// find out: asking for limit rows and handing back a cursor whenever exactly
// limit came back would promise a page that is very often not there, and every
// walk whose length is a multiple of the page size would end on an empty one.
// ribPage drops the probe row before returning.
func ribStatement[T any](db string, spec ribSpec[T], router, peer netip.Addr,
	rib, collector string, sid uint64, last []any, limit int) (string, []any, error) {
	f, err := ribPageFilters(spec, router, peer, rib, collector, sid, last)
	if err != nil {
		return "", nil, err
	}
	args := append([]any{}, spec.lead...)
	args = append(args, f.values()...)
	args = append(args, clampRIBLimit(limit)+1)
	return fmt.Sprintf(spec.body+ribOrder(spec.key), db, f.where(), f.having()), args, nil
}

// ribPageFilters renders the scope every statement of a page shares: the
// walk's four narrowings, its pin, and its position.
//
// It is one function rather than a block copied into each phase's builder for
// the reason peerStateCTE is one const: session scoping is invisible when it
// is wrong, and a second copy is a second chance to get it wrong with only one
// copy's tests watching. A two-phase page runs this twice and gets the same
// predicates both times by construction, which is what makes the phases'
// answers comparable at all.
//
// The order these are added in is the order their placeholders bind: every
// statement here splices the whole rendering in at one point, so nothing has
// to be ordered by hand the way peersStatement's seven insertion points do.
//
// router and peer are not optional for a walk and an omitted predicate is
// not a wider answer here, it is a fleet-wide dump -- ribPage refuses an
// invalid one before reaching this function, which is what makes eqAddr's
// own "drop an invalid address silently" behavior safe to use.
//
// rib IS optional, and this is the one place that distinction lives.
// eqNonEmpty omits the predicate entirely for an empty rib, so the walk
// spans every rib the peer has -- which is a well-defined answer rather
// than a broken walk precisely because rib is part of the page key (see
// ribColumn). A non-empty rib narrows exactly as it did before, and one
// that is not an Enum8 member makes ClickHouse itself raise, which is the
// loud failure this package wants for a value the HTTP layer was supposed
// to have validated against the same contract.
//
// The pin. r.collector_id is what keeps a second collector's view of the
// same router out of the page: without it, every route that collector
// also saw is a second row under the SAME key, which is a tie the keyset
// walk can straddle and lose. r.session_id pins the dump; it is belt to
// the cur join's braces (cur already scopes r to the current session, so
// no fixture can tell the two apart), and
// it is kept because the walk's pin should be legible in the statement
// that does the walking rather than implied by a CTE defined in another
// file, and because it is what makes a page that races a reconnect come
// back empty rather than full of the new dump's rows.
func ribPageFilters[T any](spec ribSpec[T], router, peer netip.Addr,
	rib, collector string, sid uint64, last []any) (filters, error) {
	var f filters
	f.eqAddr("r.router_ip", router)
	f.eqAddr("r.peer_ip", peer)
	f.eqNonEmpty("r.rib", rib)
	f.eq("r.collector_id", collector)
	f.eq("r.session_id", sid)
	if err := f.keyset(spec.key, last); err != nil {
		return f, err
	}
	return f, nil
}

// ribKeysStatement renders phase one: which routes are on this page, as their
// page keys and nothing else. It is ribStatement over spec.keysBody instead of
// spec.body, and binds the same limit+1 probe for the same reason.
//
// spec.lead is deliberately NOT bound here. It exists for evpnRoutesSQL's
// family token, which sits above that statement's outer WHERE, and a keysBody
// is written without such a placeholder -- TestRIBSpecsWithKeysBodyBindNoLead
// holds that, so a family adopting two-phase paging cannot quietly acquire a
// misbound argument.
func ribKeysStatement[T any](db string, spec ribSpec[T], router, peer netip.Addr,
	rib, collector string, sid uint64, last []any, limit int) (string, []any, error) {
	f, err := ribPageFilters(spec, router, peer, rib, collector, sid, last)
	if err != nil {
		return "", nil, err
	}
	args := append([]any{}, f.values()...)
	args = append(args, clampRIBLimit(limit)+1)
	return fmt.Sprintf(spec.keysBody+ribOrder(spec.key), db, f.where(), f.having()), args, nil
}

// ribAttrsStatement renders phase two: everything those routes carry.
//
// The statement is spec.body -- routesSQL itself, the same one the
// single-statement walk runs and the same one Routes runs -- with one extra
// predicate bounding it to the page. That is the property this whole design
// rests on: there is no second definition of current state anywhere in it. The
// session scoping, the peer_up gate, the argMax dedup, the withdrawal filter
// and the per-family DumpState are not reimplemented cheaply for the fast
// path; they are the original, asked a narrower question.
//
// The bound is a RANGE rather than a list of the keys phase one returned, and
// the difference is not cosmetic. A page is contiguous in key order by
// construction -- that is what keyset pagination means -- so `key <= last` is
// the same set as an IN of a thousand tuples, with two placeholders instead of
// three thousand. Measured on 2026-09-05 against a 1,000,000-route peer the
// two forms cost 10ms and 12ms, and 20ms and 27ms in a second run against a
// freshly recreated container; the range form wins both times and is chosen
// for the arity, not the millisecond.
//
// Rows withdrawn between the phases are excluded by spec.body's own HAVING,
// which is why the range form needs no separate withdrawal handling: the
// range is over keys, and a key whose newest observation is now a withdrawal
// simply does not survive the statement. That makes a page RACING A WITHDRAWAL
// come back one row short rather than one row wrong, which is the same smear
// api/openapi.yaml's paginated_smear warning already documents for the
// single-statement walk.
//
// No LIMIT. The range already bounds the answer, and a LIMIT here would cap a
// phase-two result that disagreed with phase one instead of letting the parity
// test see it.
//
// The bound is not observable in a page's CONTENT, which is worth stating
// here rather than leaving to be rediscovered. Delete the f.atMost call and
// this statement returns the rest of the peer's RIB from the cursor onwards
// in key order; ribPage truncates that to the page size and the answer is the
// one it returns today, row for row. What changes is the cost -- 10ms against
// a second whole-RIB read, which is the entire reason the two-phase reader
// beats the one it replaced. A mutation pass on 2026-09-05 found no
// behavioral test that fails when the bound goes, so what holds it is an
// arity assertion: TestRIBStatementBindsEveryPlaceholder's "unicast
// attributes bound the page at both ends and carry no limit".
func ribAttrsStatement[T any](db string, spec ribSpec[T], router, peer netip.Addr,
	rib, collector string, sid uint64, last []any, upto []any) (string, []any, error) {
	f, err := ribPageFilters(spec, router, peer, rib, collector, sid, last)
	if err != nil {
		return "", nil, err
	}
	if err := f.atMost(spec.key, upto); err != nil {
		return "", nil, err
	}
	args := append([]any{}, spec.lead...)
	args = append(args, f.values()...)
	return fmt.Sprintf(spec.body+keyOrder(spec.key), db, f.where(), f.having()), args, nil
}

// ribFetch returns up to limit+1 rows of one page, in key order -- the probe
// row included, because ribPage's cursor logic is what reads it and that logic
// is identical for both fetch shapes.
//
// Which shape runs is spec.keysBody, and the asymmetry is deliberate rather
// than a migration half-done: unicast is the family a full table lands on, and
// the work was scoped to it first. VPN and EVPN adopt this by filling in
// their own keysBody, and nothing else about them changes.
func ribFetch[T any](ctx context.Context, q *Q, spec ribSpec[T],
	router, peer netip.Addr, rib, collector string, sid uint64,
	last []any, limit int) ([]T, []any, error) {
	if spec.keysBody != "" {
		return ribFetchTwoPhase(ctx, q, spec, router, peer, rib, collector, sid, last, limit)
	}
	stmt, args, err := ribStatement(q.db, spec, router, peer, rib, collector, sid, last, limit)
	if err != nil {
		return nil, nil, err
	}
	out, err := ribQuery(ctx, q, spec, stmt, args)
	if err != nil {
		return nil, nil, err
	}
	// The probe row, for the family that fetches its page in one statement:
	// the database applied the LIMIT to the live answer, so a row beyond the
	// page is proof another page exists, and the page's last key comes off the
	// last row kept.
	size := clampRIBLimit(limit)
	if len(out) <= size {
		return out, nil, nil
	}
	out = out[:size]
	return out, spec.keyOf(out[len(out)-1]), nil
}

// ribQuery runs one rendered statement and scans it through the family's own
// scan loop.
func ribQuery[T any](ctx context.Context, q *Q, spec ribSpec[T],
	stmt string, args []any) ([]T, error) {
	rows, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query %s rib page: %w", spec.what, err)
	}
	defer rows.Close()
	return spec.scan(rows)
}

// ribFetchTwoPhase reads one page as two statements: which routes are on it,
// then what those routes carry.
//
// The 2026-09-05 measurements are the whole argument for it. A page of a
// 1,000,000-route peer cost 652ms as one statement and 87ms as these two --
// 77ms to resolve the keys and 10ms to fetch their attributes -- because the
// expense was never the rows read but the ELEVEN argMax state machines run
// across the peer's whole RIB to produce them. Phase one runs one argMax over
// the same rows; phase two runs eleven over a thousand.
//
// Measured a second time after this shipped, on a freshly recreated container:
// 677ms against 117ms (97 + 20). The baseline reproduced within 4%, so the
// honest speedup is a band, 5.8x to 7.5x, rather than the single figure the
// deciding run produced. Both runs read the same 1.5M rows in every shape --
// phase two is cheaper because of what it computes per row, never because it
// reads fewer. See docs/measurements.md, "RIB read path", which also records
// what was measured and rejected: a materialized current-RIB
// projection, which reached 50ms but cost 23% of ingest throughput, 27% more
// storage, and silently lost 200,460 of 1,000,000 routes whenever a superseded
// session published late.
//
// Per-page cost is still proportional to the peer's whole RIB, not to limit,
// and this does not change that -- see ribPage, whose warning stands. It
// changes the constant by roughly 6x. No correct reader measured on
// 2026-09-05 was proportional to the page; the only shape that terminated
// early returned 778 distinct routes in a page of 1000.
func ribFetchTwoPhase[T any](ctx context.Context, q *Q, spec ribSpec[T],
	router, peer netip.Addr, rib, collector string, sid uint64,
	last []any, limit int) ([]T, []any, error) {
	keysStmt, keysArgs, err := ribKeysStatement(q.db, spec, router, peer, rib, collector, sid, last, limit)
	if err != nil {
		return nil, nil, err
	}
	keys, err := ribScanKeys(ctx, q, spec, keysStmt, keysArgs)
	if err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 {
		// An empty page. Phase two would render `key <= ()` over nothing;
		// returning here is what makes atMost's empty-bound guard a backstop
		// rather than the path.
		return nil, nil, nil
	}

	// THE PROBE IS PHASE ONE'S BUSINESS ALONE, and this is the whole of it.
	//
	// Phase one asked for size+1 keys, so a (size+1)th key is proof another
	// page exists -- proof taken at the moment the page's membership was
	// decided, which is the only moment it is true of. Phase two is then asked
	// about the page's OWN keys and never about the probe, so the number of
	// rows it returns is not evidence of anything and is never read as such.
	//
	// Reading it as such is the trap. Bound phase two at keys[len(keys)-1]
	// -- the probe -- and let ribPage count the result, and one route
	// withdrawn between the two statements comes back as size rows instead
	// of size+1, ribPage reads that short page as the end of the walk, and
	// every route after it is silently gone while every row returned is
	// real. That is the exact failure class the projection was rejected for,
	// arriving through the remedy, and it is not "the same smear as the
	// single-statement walk": the single-statement reader's LIMIT is applied
	// by the database to the live answer, so a withdrawal there lets the
	// next live route take the vacated slot and the page still proves
	// another exists. See TestRIBPageUnicastSurvivesAWithdrawalBetweenThePhases.
	//
	// What a raced withdrawal costs now is the row it withdrew and nothing
	// else -- the smear api/openapi.yaml's paginated_smear warning documents.
	size := clampRIBLimit(limit)
	page := keys
	var next []any
	if len(keys) > size {
		page = keys[:size]
		next = keys[size-1]
	}

	stmt, args, err := ribAttrsStatement(q.db, spec, router, peer, rib, collector,
		sid, last, page[len(page)-1])
	if err != nil {
		return nil, nil, err
	}
	out, err := ribQuery(ctx, q, spec, stmt, args)
	if err != nil {
		return nil, nil, err
	}
	// Phase two returning FEWER rows than the page has keys is expected and
	// must not be an error: it is exactly the raced withdrawal above, now
	// costing one row instead of a walk. A runtime `len(out) != len(page)`
	// check would turn every such race into a failed request, and it could not
	// tell a race from a genuine phase-one-is-looser bug anyway -- both look
	// like a short answer at this point in the code. What separates them is
	// that a bug is reproducible, which is a test's job:
	// TestRIBUnicastKeysPhaseMatchesTheAttributesPhase compares the statements
	// directly, and TestRIBPageUnicastSurvivesAWithdrawalBetweenThePhases
	// covers the race.
	//
	// MORE rows than keys is the only impossible direction, and trimming
	// rather than trusting keeps the contract with ribPage exact if it ever
	// happens.
	if len(out) > len(page) {
		out = out[:len(page)]
	}
	return out, next, nil
}

// ribScanKeys reads phase one's rows into key tuples, building each row's scan
// destinations from spec.key's own declared types.
//
// Derived from the []ribKeyCol rather than written out per family, for the
// reason keyOrder is: the predicate, the ordering and now the scan all read
// one slice, and one slice read three times cannot disagree with itself. A
// per-family scanKeys function would be a fourth place to state the key, and
// the failure it would produce -- a page ordered by one tuple and cursored by
// another -- is invisible in any single page.
func ribScanKeys[T any](ctx context.Context, q *Q, spec ribSpec[T],
	stmt string, args []any) ([][]any, error) {
	rows, err := q.conn.Query(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query %s rib page keys: %w", spec.what, err)
	}
	defer rows.Close()

	// Built once for the page rather than once per row: Scan overwrites what
	// the pointers address, and the loop copies every value out below, so
	// nothing downstream aliases dest. At limit=10000 the per-row form cost
	// 10,001 slices and 30,003 reflect.New calls on the path this whole design
	// exists to make fast.
	dest := make([]any, len(spec.key))
	for i, k := range spec.key {
		dest[i] = reflect.New(k.want).Interface()
	}

	var out [][]any
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s rib page key: %w", spec.what, err)
		}
		key := make([]any, len(dest))
		for i := range dest {
			key[i] = reflect.ValueOf(dest[i]).Elem().Interface()
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s rib page keys: %w", spec.what, err)
	}
	return out, nil
}

// ribPage is the body of all three RIBPage functions: one page of one peer's
// current RIB, in keyset order, pinned to one BMP session.
//
// # What keyset ordering buys, and what it does not
//
// It buys CORRECTNESS UNDER A SHIFTING TABLE, and nothing else. Every page
// asks "the next `limit` routes whose key sorts after this one", so a route
// inserted or withdrawn between two pages cannot shift a row across a page
// boundary the way OFFSET would -- with OFFSET, one withdrawal before the
// cursor moves every later row one place earlier and the next page skips one
// outright, and one advertisement returns a row the caller already has. No
// skips and no duplicates is the entire benefit.
//
// It buys NOTHING IN COST, and a comment claiming otherwise would be wrong in
// a way this package has already had to retract once. Keyset pagination is
// cheap on a B-tree because the database seeks straight to the cursor;
// ClickHouse has no such index on these keys (route_unicast's sort key leads
// with router_ip, peer_ip, rib, ts_router, stream_seq -- prefix is the sixth
// component, and route_vpn's and route_evpn's are shaped the same way), and
// the argMax dedup above it is a GROUP BY over every one of that peer's rows
// that has to be recomputed from scratch on every single page regardless of
// what the cursor says. PER-PAGE COST IS PROPORTIONAL TO THE WHOLE PEER'S RIB,
// NOT TO limit.
//
// That was argued here from the shape of the query for as long as this comment
// has existed, with the standing admission that no measurement had been taken.
// One has now: against a peer holding 1,000,000 routes, a page took **487ms at
// limit=1000 and 495ms at limit=10000** -- the same work for ten times the rows
// -- and was flat in page depth as well, page six costing what page one did.
// Measured 2026-09-04; see docs/measurements.md, "Collector and writer load
// test".
//
// That is why the default page is 1000 and not 50. Halving the page size does
// not halve the work, it doubles the number of full recomputations -- a walk
// of a 50000-route peer costs 50 whole-RIB aggregations at the default and 1000
// of them at limit=50. A future reader lowering this number to be kind to the
// database would be doing the opposite, which is the only reason this
// paragraph exists.
//
// The same arithmetic run upward is why `vantage query rib` does not use this
// default at all: a walk that follows every page to the end has no reason to
// ask for a small one, so it opts up to MaxRIBPage and turns an eight-minute
// full-table walk into a fifty-second one. See cmd/vantage's ribWalkLimit.
// DefaultRIBPage stays where it is because it answers a different question --
// what ONE interactive request gets when it names no size -- and ten thousand
// rows is not a courteous answer to that.
//
// # The pin, and what it is for
//
// The cursor carries the (collector, session) the walk started in, every page
// is scoped to that pair, and every page checks -- AFTER reading its rows --
// that the pair is still the router's current session. A mismatch is
// ErrSessionChanged with no rows.
//
// This is not bookkeeping. BMP re-dumps a peer's entire table from scratch on
// a new session, so a reconnect mid-walk means the pages already fetched
// describe a dump that no longer exists. Carrying on would assemble one answer
// out of two dumps, and the seam is invisible: every row in it is real, the
// page sizes are right, the keys ascend, and the result is a RIB that was
// never on the router. Failing loudly is the only honest option, and it is
// what api/openapi.yaml's 409 exists for.
//
// The check runs after the page query rather than before it, and the order is
// load-bearing. Checked first, a reconnect landing in the gap between check
// and query leaves the page query pinned to a session cur no longer considers
// current: it matches nothing, and the walk ends early with an empty page and
// a nil cursor -- a silent truncation, which is the failure mode this whole
// design is against. Checked after, that same reconnect is caught, at the
// price of discarding a page that was in fact fine. A reconnect landing after
// the check is the NEXT page's problem, and the next page checks too.
//
// A cursor-less first call is checked the same way, which can return
// ErrSessionChanged to a caller that has no pages to discard. That is
// deliberate: it means the router reconnected during the call, the pin taken
// microseconds earlier is stale, and calling again -- which is all a restart
// is, with no cursor to drop -- pins the new session.
//
// # What the pin does NOT buy
//
// A page set is a SMEAR, not a snapshot. Within one session, routes really do
// change while a walk is in progress: a prefix withdrawn after its page was
// read is reported as live, one advertised behind the cursor is missed
// entirely, and one advertised ahead of it appears. Nothing here freezes the
// table and nothing could -- ClickHouse offers no snapshot read this could
// take, and holding one across a caller's page requests would mean holding
// state per walk. api/openapi.yaml documents this on every page, as
// meta.warnings' paginated_smear. It is a property, not a defect; the pin's
// promise is that every row came from ONE dump, not that the dump stood still.
//
// # Why an unset rib is a walk and not a refusal
//
// rib is optional, matching api/openapi.yaml, and an unset one walks every rib
// the peer has. That is only safe because rib is part of the PAGE KEY rather
// than merely a filter (see ribColumn): the same (prefix, path_id) legitimately
// appears under in_pre, in_post and loc_rib for one peer -- the live archive
// carries 10.99.1.0/24 and 10.99.2.0/24 exactly that way -- so a key without
// rib would have a tie for every one of them, and a tie that straddles a page
// boundary is a route silently dropped.
//
// The alternative considered and rejected was requiring the parameter. It
// would have closed the same defect by making the question unaskable, which is
// a smaller API to work around an incomplete key; "show me everything this peer
// sent me" is a legitimate question and the contract documents it. Putting rib
// in the key also matches the shape of every route table's sort key, which
// leads with (router_ip, peer_ip, rib, ...) -- a statement about shape and
// nothing more. It is NOT a claim that the ordering is free: the sort key
// continues (ts_router, stream_seq, prefix, path_id), so rows under one
// (router, peer, rib) sit on disk in arrival order rather than key order, and
// this ORDER BY sorts a GROUP BY result regardless. Nothing here has been
// measured, and the reason rib is in the key is correctness (above), not
// cost.
func ribPage[T any](ctx context.Context, q *Q, spec ribSpec[T],
	router, peer netip.Addr, rib string, cur *RIBCursor, limit int) ([]T, *RIBCursor, error) {
	if !router.IsValid() {
		return nil, nil, fmt.Errorf("%w: a %s RIB walk needs a router -- "+
			"api/openapi.yaml documents router= as required on /v1/rib/%s, and an "+
			"unscoped walk is a fleet-wide dump, not a wider answer",
			ErrBadFilter, spec.what, spec.what)
	}
	if !peer.IsValid() {
		return nil, nil, fmt.Errorf("%w: a %s RIB walk needs a peer -- "+
			"api/openapi.yaml documents peer= as required on /v1/rib/%s",
			ErrBadFilter, spec.what, spec.what)
	}
	collector, sid, last, err := ribCursorScope(ctx, q, cur, router, peer, rib)
	if err != nil {
		return nil, nil, err
	}
	if collector == "" {
		// No peer_events row for this router at all: it has no current
		// session, so its current RIB is empty. That is an answer, not an
		// error -- Routes says the same thing about a router it has never
		// heard of -- and a nil cursor ends the walk in one page.
		return nil, nil, nil
	}

	out, next, err := ribFetch(ctx, q, spec, router, peer, rib, collector, sid, last, limit)
	if err != nil {
		return nil, nil, err
	}

	// After the rows, never before -- see this function's own doc comment on
	// why the order is the difference between a loud failure and a silent
	// truncation.
	if err := q.sessionStillCurrent(ctx, collector, sid, router); err != nil {
		return nil, nil, err
	}

	// next is the page's last key when another page exists and nil when it
	// does not. ribFetch resolves it, because WHERE the proof comes from
	// differs between the two fetch shapes -- a one-statement walk reads it
	// off the probe row the database returned, a two-phase walk off the probe
	// KEY phase one resolved -- and reading it off the returned rows is wrong
	// for the second (see ribFetchTwoPhase).
	if next == nil {
		return out, nil, nil
	}
	return out, &RIBCursor{
		Collector: collector,
		SessionID: sid,
		Router:    router,
		Peer:      peer,
		RIB:       rib,
		Last:      next,
	}, nil
}

// RIBPageUnicast reports one page of one (router, peer, rib)'s current unicast
// RIB, keyset-ordered by (rib, prefix, path_id) and pinned to one BMP session
// -- api/openapi.yaml's /v1/rib/unicast, whose own description states the same
// key and why rib is part of it.
//
// It is Routes' counterpart for the question Routes refuses to answer: Routes
// requires a prefix (or a covers address) precisely so that an unfiltered dump
// of route_unicast is unreachable through it, and this is the surface that
// serves such a dump, one bounded page at a time, for one peer.
//
// Pass a nil cur for the first page and the *RIBCursor from the previous page
// for every page after it. A nil returned cursor means the walk is over. See
// ribPage for the whole design -- what the pin is for, why the check runs
// after the page rather than before it, why a page set is a smear rather than
// a snapshot, why an unset rib walks every rib instead of being refused, and
// why a page costs the whole peer's RIB no matter how small limit is.
//
// limit is clamped to [1, 10000] and defaults to 1000 when it is at or below
// zero; a limit above the maximum is clamped rather than refused (see
// clampRIBLimit). Every row is a Route, exactly as Routes returns it, with the
// same session scoping, the same down-peer exclusion, the same withdrawal
// exclusion and the same per-family DumpState -- both functions run the same
// statement under a different ORDER BY.
func (q *Q) RIBPageUnicast(ctx context.Context, router, peer netip.Addr, rib string,
	cur *RIBCursor, limit int) ([]Route, *RIBCursor, error) {
	return ribPage(ctx, q, unicastRIBSpec, router, peer, rib, cur, limit)
}

// RIBPageVPN reports one page of one (router, peer, rib)'s current VPN and
// labeled RIB, keyset-ordered by (rib, rd, prefix, path_id) and pinned to one
// BMP session -- api/openapi.yaml's /v1/rib/vpn.
//
// The key leads with rib, for the reason ribColumn gives. PAST rib it leads
// with rd, because a VPN route's identity does: two VRFs can carry the very
// same prefix as two entirely unrelated routes (see vpnRoutesSQL), so a walk
// keyed on prefix alone would have a tie for every such pair and lose one of
// them whenever the tie straddled a page boundary.
//
// It is VPNRoutes' counterpart for the dump VPNRoutes refuses: that function
// requires a Prefix, an RD or a Router precisely so an unpaginated dump of
// route_vpn is unreachable through it, and this is where such a dump belongs.
// See ribPage for the design and RIBPageUnicast for the paging protocol; every
// row is a VPNRoute exactly as VPNRoutes returns it.
func (q *Q) RIBPageVPN(ctx context.Context, router, peer netip.Addr, rib string,
	cur *RIBCursor, limit int) ([]VPNRoute, *RIBCursor, error) {
	return ribPage(ctx, q, vpnRIBSpec, router, peer, rib, cur, limit)
}

// RIBPageEVPN reports one page of one (router, peer, rib)'s current EVPN RIB,
// keyset-ordered by rib plus the whole NLRI tuple (route_type, rd, prefix,
// mac, ip, ethernet_tag, esi, path_id) and pinned to one BMP session --
// api/openapi.yaml's /v1/rib/evpn.
//
// Past rib (see ribColumn), the key is the entire NLRI tuple because an EVPN
// route has no shorter identity:
// a type-2 MAC/IP route has no prefix at all and a type-3 IMET route has
// neither prefix nor MAC nor IP, so any shortened key would tie rows that are
// genuinely different routes -- and a tie in a keyset walk is a route silently
// dropped whenever it straddles a page boundary. See EVPNRoute's own doc
// comment for the archive evidence that the key cannot be shortened.
//
// It is EVPNRoutes' counterpart for the dump EVPNRoutes refuses. See ribPage
// for the design and RIBPageUnicast for the paging protocol; every row is an
// EVPNRoute exactly as EVPNRoutes returns it.
func (q *Q) RIBPageEVPN(ctx context.Context, router, peer netip.Addr, rib string,
	cur *RIBCursor, limit int) ([]EVPNRoute, *RIBCursor, error) {
	return ribPage(ctx, q, evpnRIBSpec, router, peer, rib, cur, limit)
}
