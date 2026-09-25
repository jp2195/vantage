package query

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
)

// filters builds the optional half of a WHERE clause: the predicates a
// caller may or may not have asked for, in a form a statement can splice in
// without knowing which of them are present.
//
// COLUMNS ARE INTERPOLATED, VALUES ARE BOUND. That split is this file's
// entire security argument, and it holds only because of where each half
// comes from. A column name reaching any method here is a literal written
// in this package -- "r.router_ip", "peer_state.router_ip" -- never a
// string that arrived from an HTTP request, a dashboard variable or a
// config file; ClickHouse has no placeholder for an identifier in any case,
// which is the same bargain New already strikes for the database name (see
// dbNameRe). Every VALUE takes the opposite path: it is appended to args and
// reaches the statement only as a `?` the driver binds, so a prefix, a
// router IP or a rib named by a caller is never text this package
// concatenates into SQL. A future method that wanted to interpolate a value
// -- to build an IN list, say -- would be dissolving that split, not
// extending it.
//
// The invariant that follows, and the reason this type exists at all: every
// `?` emitted gets its own value appended, every time. Two methods emit more
// than one. covers emits three, for the same address, which it appends three
// times rather than leaving a reader to keep the count; keyset -- which lives
// in rib.go, next to its only caller, because it renders a walk's POSITION
// rather than a narrowing a caller asked for -- emits one per route-key
// column and appends the cursor values in the same order.
//
// The sentinel pattern this builder replaced -- `(? = toIPv6('::') OR
// router_ip = ?)`, repeated at seven insertion points in peersSQL -- put TWO
// placeholders at each of them and bound ONE value to all fourteen, with the
// count maintained by hand at the call site. A second optional filter, or an eighth insertion point (which
// would have taken the count to sixteen), was one miscount away from either
// a driver error or, worse, a plausible-looking wrong answer. See
// TestFiltersBindsEveryPlaceholderExactlyOnce, which pins the invariant
// directly.
//
// An absent filter contributes NOTHING -- no condition, no value -- rather
// than a predicate that compares against a sentinel meaning "match
// everything". What that buys is worth stating precisely, because the
// obvious larger claim is not true: on the no-router path every filter here
// renders to the empty string, so a statement that had a predicate to skip
// now has no predicate at all, and any work its shape implied -- peersSQL's
// route_counts aggregation and the hash side of its join, say -- is done in
// full either way. That cost is structural to an unfiltered query, not
// something the sentinel added. What actually goes away is one per-row
// comparison, and what is actually gained is a placeholder-counting hazard
// closed. Nothing here is a claim about the optimizer; none has been
// measured.
type filters struct {
	conds []string
	args  []any

	// havings and havingArgs are the second channel, for predicates that
	// must run AFTER the GROUP BY.
	//
	// Every filter this package had before the wide-filter slice was on a
	// GROUP BY key -- prefix, family, rib, router_ip, peer_ip, rd -- and a
	// GROUP BY key is immutable within its group, so filtering before
	// aggregation is safe for all of them. origin_asn, through_asn and
	// community are the first filters on MUTABLE attributes, and for those a
	// WHERE is not an optimization of a HAVING, it is a different and wrong
	// query: it removes rows from the group, and argMax then picks the newest
	// of what REMAINS. A route whose newest observation dropped a community
	// comes back reported as still carrying it, with the stale next hop and
	// AS path of the observation that did.
	//
	// A row-level WHERE pre-filter in THIS statement is unsafe even as a
	// superset, for exactly that reason, and there is deliberately no
	// helper that adds one.
	havings    []string
	havingArgs []any

	// candidates are the havings' raw-row twins: the same predicate, the same
	// placeholders and the same values, written against the raw column instead
	// of the live_ aggregate. They are NOT a pre-filter for this statement --
	// see above, that is still forbidden -- they are the pruning half of the
	// candidate-key semi-join, and they are sound there for a reason worth
	// spelling out, because it is one clause away from the thing §4.1 bans.
	//
	// The semi-join restricts this statement to route keys returned by a
	// subquery, and the subquery is where the candidates run. Inside it they
	// DO remove rows before an argMax, which is exactly §4.1's hazard -- and
	// it costs nothing there, because:
	//
	//   - No route can be lost. A key qualifies only if its LIVE row makes the
	//     having true, and that row is by construction the newest observation
	//     of the key. A candidate is the same predicate over that row's own
	//     raw columns, so the live row always passes it; and the newest row
	//     overall is still the newest within any subset that contains it, so
	//     the subquery's own argMax picks the same row. No false negatives.
	//   - Extra keys cost nothing. A key whose OLD observation matches while
	//     its live one does not is admitted by the subquery, because argMax
	//     over the surviving rows picks that old one. It is then rejected by
	//     THIS statement's having, which aggregates over every row of the key
	//     with no candidate applied. False positives, filtered by the original
	//     predicate.
	//
	// So the answer is the statement's own, unchanged, and the subquery only
	// decides which keys are worth asking about.
	// TestRoutesOriginASNReadsTheLIVEPath and
	// TestRoutesCommunityFilterReadsTheLIVEValue are the fixtures that would
	// catch this being wrong: each holds one key whose old observation matches
	// and whose live one does not.
	candidates    []string
	candidateArgs []any

	// liveAliases are the live_ aggregates the havings name, in first-seen
	// order. The semi-join's inner statement declares exactly these and no
	// others: declaring all of them would run the eleven argMax the semi-join
	// exists to avoid running twice.
	liveAliases []string

	// noSemiJoin records that some predicate could not be expressed as a
	// candidate, which disables the optimization for the whole statement. It
	// is one flag rather than a per-predicate skip because a candidate set
	// missing one predicate's contribution is narrower than the answer, and a
	// narrower candidate set loses routes. Correctness is not the optional
	// half.
	noSemiJoin bool
}

// wide records one wide-filter predicate in the two forms the semi-join needs:
// the having that decides the answer, and its raw-row twin that decides which
// keys are worth aggregating. The args are shared because the two forms differ
// only in the column they name.
//
// An empty candidate disables the semi-join for the whole statement rather
// than being skipped. See noSemiJoin.
func (f *filters) wide(having, candidate string, aliases []string, args ...any) {
	f.havingExpr(having, args...)
	if candidate == "" {
		f.noSemiJoin = true
		return
	}
	f.candidates = append(f.candidates, candidate)
	f.candidateArgs = append(f.candidateArgs, args...)
	for _, a := range aliases {
		if !slices.Contains(f.liveAliases, a) {
			f.liveAliases = append(f.liveAliases, a)
		}
	}
}

// semiJoinable reports whether this statement can carry a candidate-key
// semi-join: it needs at least one candidate, and every predicate that
// produced a having must have produced one.
func (f *filters) semiJoinable() bool {
	return !f.noSemiJoin && len(f.candidates) > 0 && len(f.candidates) == len(f.havings)
}

// eq adds `col = ?` unconditionally: v is a value the caller means to filter
// on whatever it is, the zero value included.
//
// Routes' own prefix is the case that needs this. Routes(ctx,
// RouteFilter{Prefix: ""}) asks for routes to the empty prefix -- a question
// TestRoutes' "an end-of-rib marker is not a route" subtest really does ask
// -- so an eq that quietly skipped the empty string would turn that call
// into an unfiltered dump of every route in the table, which is both a
// different question and a far more expensive one. eqNonEmpty is the variant
// for a filter whose empty value can only ever mean "not asked for".
func (f *filters) eq(col string, v any) {
	f.conds = append(f.conds, col+" = ?")
	f.args = append(f.args, v)
}

// orEq adds `(colA = ? OR colB = ?)` for one value tested against two
// columns. It is the either-end predicate LSLinkFilter needs and nothing
// else uses: ls_links has no plain area or asn column at all, only
// local_/remote_ pairs, because a link describes two nodes and can cross
// between them.
//
// THE PARENTHESES ARE LOAD-BEARING. where() joins its conditions with AND
// and SQL binds AND tighter than OR, so an unparenthesized
// `colA = ? OR colB = ?` would render as `... AND colA = ? OR colB = ?` and
// turn every other filter in the query into a suggestion -- a link matching
// colB alone would come back regardless of the router, peer, protocol and
// state the caller also asked for. That failure is silent and it grows the
// answer rather than shrinking it, which is the direction nobody checks.
// TestFiltersOrEqParenthesizesItsAlternatives pins the rendering and
// TestLSLinksAreaLeavesTheOtherFiltersStanding pins the behavior through the
// statement.
//
// The value is appended TWICE, once per placeholder, which is this file's
// own invariant rather than a convenience -- see covers, which appends the
// same address three times for the same reason.
func (f *filters) orEq(colA, colB string, v any) {
	f.conds = append(f.conds, "("+colA+" = ? OR "+colB+" = ?)")
	f.args = append(f.args, v, v)
}

// eqNonEmpty adds `col = ?` only when v is non-empty. An empty v contributes
// nothing, so the statement runs without the predicate rather than with one
// that matches nothing.
//
// rib is the case this was written for, and it is exactly the right shape:
// rib is an Enum8 whose five members are all non-empty (see schema.sql), so
// "" is not a value any row can hold and can only mean "the caller did not
// ask". A rib that is non-empty but not one of the five is left for
// ClickHouse to reject -- an unknown enum element raises rather than
// silently matching nothing, which is the loud failure this package wants
// for a value the HTTP layer was supposed to have validated against the
// same contract (api/openapi.yaml's own rib parameter, whose enum lists
// those five).
//
// family, rd and VPNRouteFilter's own prefix take the same rendering and
// only one of them inherits that last property. rd is a plain String whose
// "" is a REAL value lu4 rows carry, so filtering on it can never select
// the RD-less family, only leave it unfiltered -- see VPNRouteFilter.RD.
// family is LowCardinality(String), where an unknown value neither raises
// nor matches: it returns the empty result nobody can tell from a correct
// one, which is why family is the one filter this package validates in Go
// before it renders (see unicastFamilies and checkFamily).
func (f *filters) eqNonEmpty(col, v string) {
	if v == "" {
		return
	}
	f.eq(col, v)
}

// eqAddr adds `col = toIPv6(?)` for a valid address, and nothing at all for
// the zero netip.Addr.
//
// Omission is the point. The zero Addr means "every router" (or every peer)
// at every caller in this package, and the shape that meant before this
// builder existed was `(? = toIPv6('::') OR col = ?)` with "::" bound to
// both halves. That worked. What it cost was one comparison per row and two
// placeholders per insertion point, both of which an absent predicate simply
// does not have -- see this file's own header for why the larger claim
// ("this removes peersSQL's full-table hash build") is false, and for what
// is deliberately not being claimed about the optimizer.
//
// toIPv6(?) rather than a bare `col = ?`: every address column in this
// schema is IPv6 (router_ip and peer_ip on all three route tables,
// peer_events and eor_events -- see schema.sql), and an IPv4 address is
// stored in its IPv4-mapped form, so the value is lifted into the same
// representation the column holds rather than left to an implicit cast at
// comparison time. The bound value is a.String(), the plain textual form,
// because toIPv6 takes a String -- and the zero Addr's own String() is the
// text "invalid IP", which is precisely why the invalid case has to be
// dropped here rather than formatted and sent. A statement that bound it
// would not merely fail to filter; it would raise, or match nothing, on a
// call that asked for everything.
func (f *filters) eqAddr(col string, a netip.Addr) {
	if !a.IsValid() {
		return
	}
	f.conds = append(f.conds, col+" = toIPv6(?)")
	f.args = append(f.args, a.String())
}

// eqNonZero adds `col = ?` only when v is non-zero, the numeric counterpart
// to eqNonEmpty. A zero v contributes nothing, so the statement runs without
// the predicate rather than with one that matches nothing.
//
// EVPNRouteFilter.RouteType is the only caller, and it is the right shape
// there for the reason eqNonEmpty's is right for rib: IANA reserves EVPN
// route type 0, so no legally-formed NLRI carries it and "0" can only mean
// "the caller did not ask". That is a property of the EVPN registry rather
// than of this function, which is why it is argued at the field rather than
// here -- a second caller filtering on, say, an ethernet tag would be wrong
// to use this, because ethernet tag 0 is the value 202 of the 202 archive
// rows actually carry.
func (f *filters) eqNonZero(col string, v uint8) {
	if v == 0 {
		return
	}
	f.eq(col, v)
}

// since adds `col >= ?` for a non-zero t, and nothing at all for the zero
// time. It is the one predicate in this file that is not an equality of some
// kind, covers aside, and the only one whose column is a timestamp.
//
// HistoryFilter.Since is the only caller, and both halves of the rendering
// are argued at that field rather than here: what a zero Since means (an
// unbounded timeline, bounded only by the required prefix and by
// route_unicast's own 90-day TTL) and why the boundary is INCLUSIVE. The
// short version of the second: `>=` is what makes since=<the timestamp of an
// event a caller just read> return that same event again rather than silently
// drop it, which is the behavior a caller polling for "anything since the
// last thing I saw" will assume either way -- and the assumption is only safe
// if the query actually holds it.
//
// The bound value is the instant's MICROSECOND COUNT, an int64, lifted back
// into a DateTime64(6) by fromUnixTimestamp64Micro -- not the time.Time
// itself. Binding the time.Time is the obvious shape and it is wrong twice
// over; both were found by running it, not by reading:
//
//   - It loses precision. clickhouse-go renders a bound time.Time as
//     toDateTime('2026-08-24 23:34:52', 'UTC') -- SECOND granularity, with
//     the fraction dropped -- against a DateTime64(6) column. A since bound
//     of ...:52.622780 therefore behaved as ...:52.000000 and returned up to
//     a second of events older than the caller asked for. It only ever
//     WIDENS the window, which is why no count-based test on second-aligned
//     fixtures could see it; TestRouteHistorySince's "one microsecond after
//     it" case is what caught it.
//   - It sends the caller's timezone NAME to the server, which then has to
//     resolve it. A time.Time in any zone Go built locally (time.FixedZone,
//     and any zone the server's own tzdata lacks) fails outright: "Cannot
//     load time zone test+5", code 36 -- a 500 for a query whose bound was
//     perfectly well formed.
//
// A microsecond count has neither problem: it is the column's own resolution
// exactly, and an absolute instant carries no zone to resolve.
// TestRouteHistorySinceIsAnAbsoluteInstant pins both halves with a non-UTC
// location, because every fixture in this package writes UTC and a
// UTC-only test cannot tell a correct binding from either failure above.
//
// The COLUMN side stays bare -- `col >= f(?)`, never `f(col) >= ?` -- so the
// function is applied to the constant and not per row. That is the same rule
// HistoryFilter.Prefix states for idx_prefix, and it matters here for
// route_unicast's PARTITION BY toYYYYMM(ts_collector) rather than for a skip
// index.
func (f *filters) since(col string, t time.Time) {
	if t.IsZero() {
		return
	}
	f.conds = append(f.conds, col+" >= fromUnixTimestamp64Micro(?)")
	f.args = append(f.args, t.UnixMicro())
}

// before bounds a read at the top, exclusively, the way since bounds it at
// the bottom inclusively.
//
// It exists so that two statements answering about ONE window really do
// answer about one window. Without it each read is bounded at the bottom
// and open at the top, so a row archived between them lands in one answer
// and not the other -- which is how /v1/collection/churn/peers' ranking and
// its per-peer series could disagree about a peer that churned while the
// handler was mid-request. A zero time applies no bound, so a caller that
// does not care pays nothing.
func (f *filters) before(col string, t time.Time) {
	if t.IsZero() {
		return
	}
	f.conds = append(f.conds, col+" < fromUnixTimestamp64Micro(?)")
	f.args = append(f.args, t.UnixMicro())
}

// coversRange is the IPv6 address range one stored prefix spans, as a
// (start, end) tuple. It is a fragment rather than a method of its own
// because covers needs it twice -- once per bound of the containment test --
// and ClickHouse has nowhere to put an intermediate alias inside a spliced
// WHERE predicate. %[1]s is the prefix column, interpolated by covers.
//
// Two things in it are load-bearing and neither is obvious.
//
// The `+ 96` converts an IPv4 prefix length into its IPv4-mapped IPv6
// equivalent. Every address in this schema is IPv6 (see eqAddr) and an IPv4
// prefix is stored textually as "10.0.0.0/8", so its /8 has to become the
// /104 of ::ffff:10.0.0.0 before IPv6CIDRToRange is asked for a range.
// Getting the constant wrong does not raise: it returns a range that is
// plausibly sized and wrong, and the answer looks like an ordinary routing
// table. The v4/v6 discriminator is a colon in the ADDRESS half, not in the
// whole string -- splitByChar already removed the length, so no other colon
// can appear.
//
// coalesce, on both arguments, is what keeps the expression total for a row
// whose prefix does not parse, and it is NOT made redundant by the guards
// covers places above it. ClickHouse does not guarantee predicate evaluation
// order, so a guard in an earlier conjunct cannot be relied on to keep a
// later one from evaluating -- and this particular failure is worse than a
// per-row error in any case: IPv6CIDRToRange refuses a Nullable argument at
// ANALYSIS time, "Nested type Tuple(IPv6, IPv6) cannot be inside Nullable
// type" (verified on 24.8.14.39), so a version of this without coalesce does
// not fail on the malformed row -- it fails on every row, before the query
// runs at all. Deleting either coalesce is caught by every covers test in
// the package for that reason.
const coversRange = `IPv6CIDRToRange(` +
	`coalesce(toIPv6OrNull(splitByChar('/', %[1]s)[1]), toIPv6('::')), ` +
	`toUInt8(coalesce(toUInt8OrNull(splitByChar('/', %[1]s)[2]), 0) + ` +
	`if(position(splitByChar('/', %[1]s)[1], ':') > 0, 0, 96)))`

// coversStoredV4 and coversStoredV6 complete coversExpr's family test, which
// is what keeps an address from being compared against a prefix of the OTHER
// family: %[2]s is one of these two, chosen by covers from the address it was
// given.
//
// The test is on the stored TEXT -- a colon in the address half or no colon
// -- rather than on route_unicast's own family column, and that is
// deliberate. It is the identical discriminator coversRange uses to decide
// whether to add 96, so one row cannot be treated as IPv4 by the family test
// and as IPv6 by the arithmetic; a family column consulted here and text
// consulted there would be two answers to one question, and route_evpn has
// no family column at all if this predicate is ever wanted over its prefixes.
// It also costs nothing a caller's own ?family= would have covered: family=
// narrows to one of route_unicast's families, while this excludes a
// cross-family comparison the caller never asked for and cannot see.
const (
	coversStoredV4 = "= 0"
	coversStoredV6 = "> 0"
)

// coversExpr is the whole containment predicate: the guards that make a
// row-supplied prefix safe, the one that makes an operator-supplied address
// safe, and the range test itself. %[1]s is the prefix column and %[2]s the
// family test's comparison; see covers for the argument.
const coversExpr = `(toIPv6OrNull(?) IS NOT NULL` +
	` AND position(splitByChar('/', %[1]s)[1], ':') %[2]s` +
	` AND toIPv6OrNull(splitByChar('/', %[1]s)[1]) IS NOT NULL` +
	` AND toUInt8OrNull(splitByChar('/', %[1]s)[2]) <= ` +
	`if(position(splitByChar('/', %[1]s)[1], ':') > 0, 128, 32)` +
	` AND toIPv6OrNull(?) >= ` + coversRange + `.1` +
	` AND toIPv6OrNull(?) <= ` + coversRange + `.2)`

// covers adds the longest-match containment predicate: the rows whose own
// prefix column contains a. It is the only predicate in this file that is
// not an equality, and the only one whose cost is a full scan by
// construction -- api/openapi.yaml documents that at the parameter itself
// ("Full scan by construction (no index can serve containment); measured
// 31-52ms at 3.2M rows"), and it is a property of the question rather than
// of this rendering: a bloom filter answers exact membership, and
// containment is a range.
//
// It is TOTAL. Neither the address an operator asked about nor any string a
// row happens to be holding in its prefix column can make it raise, and that
// is the whole design rather than a nicety. This predicate serves a
// looking-glass surface, where an exception is a 500 for a question whose
// honest answer is "nothing covers that", and where a plausible-looking
// wrong answer is worse than either.
//
// The argument side is guarded by toIPv6OrNull, never toIPv6:
// toIPv6('not-an-address') raises, toIPv6OrNull returns NULL, and NULL in
// either range comparison yields NULL, which WHERE reads as false. The
// explicit `toIPv6OrNull(?) IS NOT NULL` conjunct is therefore redundant
// with three-valued logic TODAY, and that was checked rather than assumed:
// neutralizing it (`... OR 1 = 1`, parenthesized so precedence does not
// change the rest) leaves every result-based test in this package green.
// Deleting it OUTRIGHT is caught, but only incidentally -- one fewer
// placeholder than the three values covers appends is an arity error the
// driver raises before ClickHouse sees the statement -- so
// TestRouteFilterRendersOnlyWhatWasAsked is what actually pins it, by
// asserting the rendered text. It is here because the range test is one
// rewrite away from needing it: any future shape that coalesces or
// assumeNotNull's the argument to avoid a Nullable comparison makes the
// missing guard a matched-everything bug rather than a matched-nothing one.
//
// The row side is guarded because route_unicast.prefix is a plain String
// column with no constraint of any kind (see schema.sql). The sink writes
// netip.Prefix.String() into it and bgp/prefix.go rejects a v4 length above
// 32 before that, so today's pipeline cannot produce a malformed one -- but
// the column is what the query has to answer for, not the writer, and a row
// that predates a schema change, arrives from a future sender or is inserted
// by hand is not a reason to raise. The guards exclude such a row rather
// than letting the coalesce fallbacks above answer FOR it: '::' with a short
// length, or a length coalesced to 0 and lifted by 96, are both ranges that
// contain a great deal, so an unguarded malformed row does not merely
// survive -- it is reported as covering nearly everything asked about. See
// insertCoversFixture, which stores one row per shape on purpose.
//
// The length guard is a RANGE test, `<= if(colon, 128, 32)`, not the
// `IS NOT NULL` it reads like it should be. NULL is covered either way (NULL
// <= 32 is NULL, which WHERE reads as false), and the range is what closes
// the case IS NOT NULL leaves open: toUInt8OrNull('200') is a perfectly good
// 200, and 200 + 96 is 296, which toUInt8 WRAPS to 40 -- a /40 whose range
// contains every address this query could be asked about. Any stored v4
// length from 160 to 255 wraps that way. The two tests are not both kept,
// deliberately: with the range test present the null check can never be the
// reason a row is excluded, and a conjunct that cannot change an answer is
// the vacuity this package keeps writing tests to find. The bounds are the
// families' real ones rather than a single 128, so a stored "10.0.0.0/40" is
// excluded outright rather than reported as covering exactly 10.0.0.0 (which
// is what a length of 136 clamps to).
//
// The family test is what keeps an IPv4 question from being answered by an
// IPv6 route. IPv6CIDRToRange is arithmetic on 128 bits and does not know
// what a family is: '::/0' contains ::ffff:10.77.0.33 the way it contains
// everything, so without this conjunct an IPv6 default route -- a real route
// a default-originate session really does carry -- is reported as covering
// every IPv4 address in the fleet. Nothing about that answer looks wrong to
// a reader. The comparison is on the stored text's own form, so a prefix
// written in IPv6 form is an IPv6 prefix even when it is IPv4-MAPPED:
// "::ffff:10.0.0.0/104" does not answer a query for 10.0.0.33. That is the
// predictable reading rather than the clever one -- the sink writes
// netip.Prefix.String(), which renders a v4 NLRI in dotted quad and never in
// mapped form, so a mapped prefix in this column did not come from a v4
// advertisement and treating it as one would be inferring a family from a
// textual coincidence. Such a row is still reachable by an exact
// RouteFilter.Prefix.
//
// a is Unmap'd first, which is the same decision from the argument's side
// and the opposite outcome: Covers "::ffff:10.77.0.33" is the mapped
// spelling of an address a person typed, not a stored prefix a router
// advertised, so it asks the IPv4 question. Unmapping also fixes the bound
// text at one spelling, and toIPv6 lifts the two spellings to the same value
// in any case, so nothing but the family test's choice depends on it.
//
// An invalid a is bound rather than dropped, which is the opposite of what
// eqAddr does with one, and deliberately. eqAddr's zero Addr means "every
// router", so omitting the predicate is exactly right there; a zero Addr
// here would mean "every route in the table", so omitting it would turn a
// bad address into a fleet-wide dump. Binding it instead sends the text
// "invalid IP" (netip.Addr.String()'s own rendering of the zero value)
// through toIPv6OrNull, which answers NULL, which matches nothing. The zero
// Addr takes the IPv6 form of the family test, which is arbitrary and cannot
// matter: no row survives a NULL argument either way.
// RouteFilter.check rejects it long before that, and this is what happens if
// a future caller forgets to call check.
//
// It emits THREE placeholders and appends THREE values -- the same address
// three times -- rather than binding one value to three placeholders. That
// is this file's own invariant (see the header): every placeholder gets its
// own appended value, which is precisely what the sentinel pattern this
// builder replaced kept getting wrong. A reader who wants the value bound
// once should note that ClickHouse has no way to name a bound parameter
// twice in a positional statement, and that a WITH alias would couple this
// builder to the statement it is spliced into.
func (f *filters) covers(col string, a netip.Addr) {
	a = a.Unmap()
	form := coversStoredV6
	if a.Is4() {
		form = coversStoredV4
	}
	f.conds = append(f.conds, fmt.Sprintf(coversExpr, col, form))
	s := a.String()
	f.args = append(f.args, s, s, s)
}

// where renders the predicates as a suffix to a WHERE clause that already
// carries at least one predicate of its own: "" when nothing was added, and
// " AND a AND b" otherwise. The leading space is part of the contract, so a
// caller writes `WHERE ` + servedGate + `%[2]s` and gets valid SQL whether
// or not any filter is present.
func (f *filters) where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return " AND " + strings.Join(f.conds, " AND ")
}

// havingExpr adds one post-aggregation predicate. expr is rendered verbatim
// and must contain one ? per arg; it names live_* aliases, not raw columns.
func (f *filters) havingExpr(expr string, args ...any) {
	f.havings = append(f.havings, expr)
	f.havingArgs = append(f.havingArgs, args...)
}

// having renders the tail appended to each statement's own
// `HAVING live_is_withdraw = 0`, so it leads with AND and is empty when
// unused.
func (f *filters) having() string {
	if len(f.havings) == 0 {
		return ""
	}
	return " AND " + strings.Join(f.havings, " AND ")
}

// havingClause renders the accumulated post-aggregation predicates as a
// HAVING clause in their own right: "" when nothing was added, "HAVING a AND
// b" otherwise. It is having()'s counterpart for a statement with no
// predicate of its own to hang an AND on, exactly as clause() is where()'s.
//
// The three route statements do not need it -- each carries a fixed `HAVING
// live_is_withdraw = 0` that having() appends to. The link-state statements
// do, because ?state=any renders NO post-aggregation predicate at all, and a
// bare `HAVING` with nothing after it is a syntax error rather than a
// permissive filter.
func (f *filters) havingClause() string {
	if len(f.havings) == 0 {
		return ""
	}
	return "HAVING " + strings.Join(f.havings, " AND ")
}

// wideASN adds the origin and transit predicates, both over the LIVE AS path.
//
// HAVING, not WHERE, and the difference is correctness rather than speed --
// see the havings field's own comment.
//
// `length(live_as_path) > 0` is not redundant with the equality beside it.
// ClickHouse returns 0 for an out-of-range array subscript, so
// `live_as_path[-1] = 0` is TRUE for every empty path, and without the guard
// origin_asn=0 would match exactly the rows whose origin is unknown -- the
// one answer this filter must never give. api/ refuses 0 at the boundary and
// this refuses it again here, because the two layers are refusing it for
// different reasons and neither should depend on the other's diligence.
func (f *filters) wideASN(origin, through uint32) {
	if origin != 0 {
		// The candidate keeps the length guard for the reason the having has
		// it: r.as_path[-1] is 0 for an empty array too, and a candidate that
		// admitted every path-less row would prune nothing on exactly the
		// rows most likely to be numerous.
		f.wide("(length(live_as_path) > 0 AND live_as_path[-1] = ?)",
			"(length(r.as_path) > 0 AND r.as_path[-1] = ?)",
			[]string{colASPath}, origin)
	}
	if through != 0 {
		f.wide("has(live_as_path, ?)", "has(r.as_path, ?)", []string{colASPath}, through)
	}
}

// clause renders the same predicates as a WHERE clause in their own right:
// "" when nothing was added, "WHERE a AND b" otherwise. It is where's
// counterpart for an insertion point that has no predicate of its own to
// hang an AND on.
//
// peersSQL's dump_families branches are the case. Each selects from one
// table with nothing else to say about it, so an absent filter has to leave
// the branch with no WHERE at all -- not with a `WHERE 1 = 1` written only
// to give the AND something to attach to, which would put a placeholder-free
// tautology into the statement for no reason other than the builder's
// convenience.
func (f *filters) clause() string {
	if len(f.conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(f.conds, " AND ")
}

// values returns the bound arguments in the order the statement renders its
// placeholders: every WHERE argument, then every HAVING argument. The order
// is the contract between this type and every caller that splices where()
// and having() into one statement, and it is not checkable by reading the
// SQL -- see TestFiltersOrderHavingArgsAfterWhereArgs.
//
// A caller splicing several filters into one statement must concatenate
// their values in the order the placeholders appear in the FINISHED
// statement, which is not necessarily the order the filters were built in:
// fmt.Sprintf's indexed verbs let one filter's text land at several
// positions, and the driver binds strictly left to right. Peers is the
// caller that has to think about this, across seven insertion points; see
// its own comment on the ordered list it walks.
func (f *filters) values() []any {
	if len(f.havingArgs) == 0 {
		return f.args
	}
	out := make([]any, 0, len(f.args)+len(f.havingArgs))
	out = append(out, f.args...)
	out = append(out, f.havingArgs...)
	return out
}

// ErrBadFilter is the sentinel every filter-validation error in this package
// wraps: an unknown address family, or a VPN query with nothing to narrow it.
// It marks the errors the CALLER got wrong, as opposed to a database that was
// unreachable or a statement ClickHouse refused -- a 400 and a 500
// respectively at the HTTP layer, and errors.Is is what lets cmd/vantage-api
// tell them apart without matching on message text.
var ErrBadFilter = errors.New("query: invalid filter")

// unicastFamilies and vpnFamilies are the family tokens route_unicast's and
// route_vpn's own `family` columns can hold. They are DERIVED from the
// family-token registry via subjects.FamilyToken rather than spelled out
// here, and that is the whole point of the shape.
//
// It is the same coupling TestPeersEVPNFamilyLiteralMatchesSubjectsFamilyToken
// pins for peersSQL's one EVPN literal -- see that test for the full account
// of what a drifted copy of this registry costs, and sink/rows_test.go's
// TestFamilyNameMatchesSubjectsFamilyToken for the occurrence that already
// bit this repo once. sink writes subjects.FamilyToken(f) into the family
// column of every route row it inserts, so a token written here as a string
// would be a fourth untethered copy: rename familyNames[{AFI: 1, SAFI: 128}]
// in subjects and production starts writing the new token into route_vpn
// while this package keeps rejecting it, with the suite green throughout.
// Naming the bgp.Family instead -- an AFI/SAFI pair, which is what the
// protocol fixes and what no rename can move -- makes the token looked up
// rather than remembered, so the two ends cannot drift at all.
//
// Validating in Go rather than letting the statement carry an unknown value
// down to ClickHouse is a choice rib did not have to make. rib is an Enum8
// and an unknown member RAISES (see eqNonEmpty, and
// TestRoutesRejectsARibThatIsNotAnEnumMember for why a loud failure is the
// right one). family is LowCardinality(String): an unknown family matches
// NOTHING and returns an empty result indistinguishable from "this prefix is
// not carried in that family anywhere", which is exactly the quiet wrong
// answer that test argues against. Nothing in the database will raise on it,
// so this is the only place it can.
//
// The two sets are separate because the two tables are, and because
// api/openapi.yaml says so: ?family= on /v1/routes/unicast enumerates
// [ipv4u, ipv6u], on /v1/routes/vpn [vpn4, vpn6, lu4]. One merged set would
// accept ?family=vpn4 against route_unicast and answer it with the empty
// result this validation exists to prevent.
var (
	unicastFamilies = familyTokens(
		bgp.FamilyIPv4U,             // ipv4u
		bgp.Family{AFI: 2, SAFI: 1}, // ipv6u
	)
	vpnFamilies = familyTokens(
		bgp.FamilyVPNv4,               // vpn4
		bgp.Family{AFI: 2, SAFI: 128}, // vpn6
		bgp.FamilyLU4,                 // lu4
	)
)

// familyTokens renders each family through the registry and collects the
// results as a set. It takes bgp.Family values, never strings, so that a
// caller cannot quietly reintroduce the hard-coded token this indirection
// exists to remove.
func familyTokens(fs ...bgp.Family) map[string]bool {
	m := make(map[string]bool, len(fs))
	for _, f := range fs {
		m[subjects.FamilyToken(f)] = true
	}
	return m
}

// checkFamily accepts the empty family -- "not asked for", the same reading
// eqNonEmpty gives it -- and any token in allowed, and rejects everything
// else naming the table it was asked of. The legal values are sorted so the
// message is the same on every run: a map's iteration order is not, and an
// error string that reshuffles itself is one no test can assert and no
// operator can grep for.
func checkFamily(allowed map[string]bool, v, table string) error {
	if v == "" || allowed[v] {
		return nil
	}
	return fmt.Errorf("%w: family is not one of %s's families (%s)",
		ErrBadFilter, table, strings.Join(slices.Sorted(maps.Keys(allowed)), ", "))
}

// checkCovers validates the prefix/covers pair every route filter carries:
// that the two were not asked for together, and that Covers is an address
// this package can turn into one. endpoint names, in the message, where
// api/openapi.yaml documents the pair for the filter being checked.
//
// It is one function rather than three copies because all three route
// filters now carry Covers -- api/openapi.yaml documents ?covers= on
// /v1/routes, the fan-out, whose answer is all three families at once, so a
// covers= the VPN and EVPN filters could not render would be an endpoint
// with no way to be served. See VPNRouteFilter.Covers.
//
// Covers is refused rather than passed through for the same reason family
// is. The predicate covers renders is TOTAL -- a malformed address reaches
// ClickHouse as a comparison against NULL and comes back as zero rows, never
// as an exception (see covers) -- so nothing downstream will ever complain
// about "10.0.0.0/99" or a hostname typo. The caller would get the empty
// result that is indistinguishable from "no route in the fleet covers that
// address", which is the quiet wrong answer this package keeps being written
// against. A prefix is called out by name in the message because it is the
// likeliest of these mistakes: ?covers= sits directly beside ?prefix= in the
// contract and takes the other kind of argument.
//
// The two together are refused rather than one preferred, because ANDing
// them would answer "this exact prefix, which also contains that address" --
// a question nobody asked, whose empty result reads like "that prefix is
// nowhere".
func checkCovers(endpoint, prefix, covers string) error {
	if covers == "" {
		return nil
	}
	if prefix != "" {
		return fmt.Errorf("%w: Prefix and Covers are mutually exclusive -- "+
			"api/openapi.yaml documents exactly one of prefix= or covers= on "+
			"%s, and the two together ask for an exact prefix that also "+
			"contains an address, whose empty answer looks exactly like the "+
			"prefix being nowhere in the fleet",
			ErrBadFilter, endpoint)
	}
	a, err := netip.ParseAddr(covers)
	if err != nil {
		return fmt.Errorf("%w: Covers is not an IP address -- covers= takes "+
			"the address a route would have to contain, never a prefix; a "+
			"prefix belongs in Prefix, which matches it exactly", ErrBadFilter)
	}
	if a.Zone() != "" {
		return fmt.Errorf("%w: Covers carries an IPv6 zone -- a zone is a "+
			"property of the asking host's own interfaces, not of anything a "+
			"router advertises, and ClickHouse's toIPv6 does not parse one, so "+
			"this would match no route at all rather than fail", ErrBadFilter)
	}
	return nil
}

// coversIfAsked adds the one prefix-space predicate a filter asks for:
// Covers' containment when it was given, and otherwise nothing -- the caller
// renders its own exact match. It returns whether it rendered anything, so a
// caller whose Prefix is unconditional (RouteFilter's) can tell the two
// apart.
//
// The parse error is dropped rather than reported because there is nothing
// useful to do with it here and nothing unsafe about it: check has already
// refused an unparseable Covers, and covers binds the zero Addr's own text
// ("invalid IP") to a predicate that matches nothing if one ever reaches it
// anyway. See covers for why binding is the safe failure and omitting would
// not be.
func (f *filters) coversIfAsked(col, covers string) bool {
	if covers == "" {
		return false
	}
	a, _ := netip.ParseAddr(covers)
	f.covers(col, a)
	return true
}

// RouteFilter is the narrowing Routes accepts: one of the two ways
// api/openapi.yaml lets a caller name what it is asking about on
// /v1/routes/unicast (?prefix= exactly, or ?covers= by containment) plus the
// four optional dimensions it documents beside them (?family=, ?router=,
// ?peer=, ?rib=).
//
// It is a struct rather than six positional parameters because the list has
// in fact grown once already -- Covers was the field the contract documented
// before this package could render it -- and a struct is what let it arrive
// without touching a call site that does not set it.
//
// VPNRoutes takes VPNRouteFilter, not this type, and the two are separate
// structs rather than one embedding the other for a reason worth stating:
// Prefix means different things to them. Here "" is the empty prefix and the
// predicate is rendered anyway; there it is optional and "" means "every
// prefix". One field name carrying two meanings across an embedding is the
// kind of thing a reader has to already know to get right, so the fields are
// written out twice instead. What the duplication costs is a dimension added
// to one struct and forgotten on the other;
// TestFilterStructsAgreeOnTheirSharedDimensions is what fails when that
// happens -- and Covers is the dimension it caught: it lived on RouteFilter
// alone, while api/openapi.yaml documented ?covers= on the /v1/routes
// fan-out, whose answer is all three families. See VPNRouteFilter.Covers for
// what an unrenderable covers= did to that endpoint.
type RouteFilter struct {
	// Prefix is an exact match on the route's own prefix column. It is not
	// optional the way the four dimensions below it are: "" means the empty
	// prefix, not "every prefix" (see eq's own doc comment for why that
	// distinction is load-bearing rather than pedantic), and it is rendered
	// whenever Covers is absent -- with one exception, added after a
	// critical defect.
	//
	// The exception is a wide filter (OriginASN, ThroughASN or Community --
	// see wideAsked) set with Prefix left at its zero value. That is not a
	// caller asking about the empty prefix AND that AS; it is a caller who
	// never set Prefix at all, asking the wide question alone, and
	// predicates renders no prefix predicate for it. Rendering
	// `r.prefix = ''` regardless would look right and be wrong:
	// RouteFilter{OriginASN: 64512}, measured against a lab archive where
	// AS 64512 genuinely originates 3,250 rows across 501 prefixes, returned
	// zero every time rendered that way, because the query asked for the one
	// prefix route_unicast has never held. That is the headline feature --
	// /v1/routes/unicast?origin_asn=X -- silently non-functional, and a test
	// that sets Prefix alongside its wide filter (see
	// TestRoutesFiltersByOriginASN's own comment), which guards a different
	// hazard (an unset Prefix misread as "every prefix"), cannot see it. See
	// predicates' own doc comment for the rendering and
	// TestRouteFilterWideAloneMatchesWithoutPrefix in routes_test.go for the
	// regression coverage.
	//
	// A Prefix that IS set still renders exactly as before, wide filter or
	// not: {Prefix: "10.0.0.0/8", OriginASN: X} is a legitimate narrowing --
	// this exact prefix, further narrowed to that AS's live announcement of
	// it -- not the case this exception changes.
	//
	// Short of that one exception, this is what makes "an unfiltered dump"
	// unreachable through this type without a rule like VPNRouteFilter.check's:
	// a RouteFilter with no wide filter set renders exactly one prefix
	// predicate, either this exact match or Covers' containment, and the
	// empty RouteFilter -- no wide filter, no Covers -- asks about the empty
	// prefix, which is a real question with a real (empty) answer rather than
	// a request for every route in route_unicast -- TestRoutes' "an
	// end-of-rib marker is not a route" subtest asks it directly. A
	// RouteFilter that DOES set a wide filter is narrowed by that filter's
	// own HAVING predicate instead, which is exactly as bounded an answer as
	// the exact match would have given.
	//
	// Mutually exclusive with Covers, which api/openapi.yaml says outright
	// ("Exact prefix match [...] Mutually exclusive with covers"). check
	// refuses both together rather than preferring one, because ANDing them
	// would answer "this exact prefix, which also contains that address" --
	// a question nobody asked, whose empty result reads like "that prefix is
	// nowhere".
	Prefix string

	// Covers is api/openapi.yaml's ?covers= on /v1/routes/unicast and on
	// /v1/routes, the fan-out: an ADDRESS, and the routes reported are the
	// ones whose prefix contains it, at every prefix length.
	//
	// VPNRouteFilter and EVPNRouteFilter carry the identical field, with
	// the identical meaning, so that the fan-out has an answer from every
	// family rather than from one -- see VPNRouteFilter.Covers. It is the looking-glass question -- "what
	// covers this address" -- and it is the one narrowing here that is not
	// an equality, with a documented full-scan cost to match (see covers).
	//
	// It is a string rather than a netip.Addr, which is the one place this
	// struct disagrees with Router and Peer, and the reason is the failure
	// mode a netip.Addr would have. netip.ParseAddr("not-an-address") hands
	// back the ZERO Addr, and a zero-Addr Covers is indistinguishable from
	// "the caller did not ask" -- so a malformed ?covers= would silently
	// become an exact-prefix query for whatever Prefix held (usually ""),
	// and answer it with an empty result nobody could tell from "nothing in
	// the fleet covers that address". Keeping the raw text means check can
	// tell the two apart and reject the first, which is exactly the argument
	// unicastFamilies makes for validating family in Go: this package's job
	// is to make a caller's mistake loud, because nothing downstream will.
	//
	// An IPv4 Covers is answered only by prefixes stored in IPv4 form and
	// an IPv6 one only by prefixes stored in IPv6 form, so an IPv6 default
	// route is not reported as covering an IPv4 address -- see covers for
	// why that comparison is on the stored text and what it means for an
	// IPv4-mapped prefix. An IPv4-mapped Covers ("::ffff:10.77.0.33") is
	// unmapped and asks the IPv4 question, which is the same rule read from
	// the other side: what a person typed, not what a router advertised.
	//
	// "" means "not asked for" -- the only reading available, since the
	// empty string is not an address -- and leaves Prefix rendering the
	// exact match above.
	Covers string

	// Family narrows to one of route_unicast's own families (ipv4u,
	// ipv6u). It is validated against the registry rather than passed
	// through, because an unknown family matches nothing instead of
	// raising -- see unicastFamilies.
	//
	// Against Prefix this filter can only ever narrow an answer to zero
	// rows: no prefix string is legal NLRI in both families, so a prefix
	// already picks one out. Against Covers it genuinely selects -- an
	// address picks no family, and a containment query spans both at once
	// -- which is the job it was documented for before covers= existed to
	// give it one.
	Family string

	// Router, Peer and RIB are each omitted from the statement entirely
	// when unset -- the zero netip.Addr and the empty string respectively.
	// "Unset" is not a value they compare against; see eqAddr and
	// eqNonEmpty.
	Router netip.Addr
	Peer   netip.Addr
	RIB    string

	// OriginASN and ThroughASN filter on the LIVE AS path, and zero means
	// "not asked for" in both.
	//
	// Zero cannot mean AS 0 here, and that is safe rather than sloppy: AS 0
	// is reserved by RFC 7607 and originates nothing, and api/ rejects an
	// explicit origin_asn=0 with a 400 before it reaches this struct. The
	// split matches the one ribColumn's doc comment describes -- a value the
	// HTTP layer can refuse against a documented rule is refused there, so
	// this layer's zero value stays free to mean "unset".
	//
	// A route with an EMPTY AS path never matches either filter. query's own
	// originASN returns 0 for an empty path and the wire renders
	// origin_asn: null, because "no path" is not "originated in AS 0"; a
	// filter that matched them would contradict the field it filters on.
	// 1,839 of the archive's 8,611 unicast rows take that branch.
	OriginASN  uint32
	ThroughASN uint32

	// Community is one community in any of the four notations this archive
	// stores, "" meaning not asked for. See ParseCommunity for the dispatch
	// and for why a value can search more than one column.
	Community string

	// Limit caps the rows Routes RETURNS, not the rows it matches, and 0
	// means no cap -- so every caller that predates this field, and every
	// endpoint that has not wired it through yet, is unaffected.
	//
	// It renders no predicate of its own, unlike every field above it:
	// predicates() never mentions it, because a row cap is not a narrowing
	// of WHICH rows match, only of how many of the matching rows come back.
	// Routes itself splices it into the finished statement as a bare
	// `LIMIT %d` after routesOrder (see Routes' own comment on why %d and
	// not a bound `?`), which is also why the reflection guards in
	// filters_test.go count it as a field that contributes nothing to
	// predicates()'s conds/havings total -- see
	// TestRouteFilterRendersEveryFieldItCarries's own comment.
	//
	// It is deliberately not paired with an offset. CountRoutes answers "how
	// much did you not show me", and paging through a capped fleet-wide
	// answer is a different feature: the existing cursor machinery
	// (RIBCursor) does not extend to one -- current
	// state is defined per (collector, router) session, so a fleet-wide walk
	// spans many sessions that can each be superseded independently, with no
	// single position to pin and no coherent 409 to report when one is.
	Limit int
}

// wideAsked reports whether rf carries at least one of the WIDE filters --
// OriginASN, ThroughASN or Community, the predicates wideASN and
// filters.community render into the HAVING channel over the live AS path and
// the live community set, never into WHERE (see the havings field's own doc
// comment for why).
//
// predicates uses it to decide what an unset Prefix means. A RouteFilter
// with nothing at all set still asks about the empty prefix (see
// RouteFilter.Prefix), but a RouteFilter with, say, OriginASN set and Prefix
// left at "" asked a different question -- "who originates this AS's
// routes", with no prefix named -- and treating the zero Prefix as though it
// meant the first question anyway is exactly the defect
// RouteFilter.Prefix's own doc comment recounts: it rendered `r.prefix = ?`
// bound to "" against a query that never asked about the empty prefix at
// all, and matched nothing on an archive where the wide filter alone
// genuinely matched thousands of rows.
func (rf RouteFilter) wideAsked() bool {
	return rf.OriginASN != 0 || rf.ThroughASN != 0 || rf.Community != ""
}

// predicates renders rf against the route table alias routesSQL uses, `r`.
// The alias is hard-coded rather than passed in because it is a property of
// that statement, not of the caller: a route query that aliased its FROM
// differently would be silently unfiltered, so the coupling belongs
// somewhere a reader of the statement can see it rather than in an argument
// each call site restates. VPNRouteFilter.predicates hard-codes the same
// alias against vpnRoutesSQL, which spells it the same way on purpose.
//
// The order the predicates are added in is the order their placeholders
// bind, so it must match the order they appear in the finished statement.
// routesSQL splices the whole rendering in at a single point, which is what
// makes that trivially true here and is worth keeping.
//
// At most one prefix-space predicate is rendered: Covers' containment when
// it was asked for, Prefix's exact match otherwise -- and, when neither
// Prefix nor Covers was set but a wide filter was, NEITHER. The first branch
// is what makes eq's unconditional binding survive covers=: an exact match
// against the empty string, left in alongside a containment test, would
// answer "the empty prefix, which also contains 10.77.0.33" and return
// nothing at all on every covers query -- while keeping "" the empty prefix
// for a caller who did not ask about an address. It is not the shape a
// second, eqNonEmpty-style prefix would have given: that one drops the
// predicate entirely for an empty Prefix, which is an unfiltered dump on
// every call that sets neither field. See RouteFilter.Prefix.
//
// The second branch, wideAsked's, is the fix for the defect
// RouteFilter.Prefix's own doc comment recounts at length: an unset Prefix
// beside a wide filter used to render `r.prefix = ?` bound to "" regardless,
// and route_unicast has never held a matching row, so RouteFilter{OriginASN: X}
// alone was silently unanswerable. Omitting the prefix predicate entirely
// here is safe for the same reason omitting it for Covers is -- the wide
// filter's own HAVING predicate is doing the narrowing instead, so this is
// not the "renders nothing at all" case check exists to catch on
// VPNRouteFilter and EVPNRouteFilter; RouteFilter carries no such rule
// because a wide filter alone is exactly as bounded an answer as an exact
// prefix match. See TestRouteFilterWideAloneMatchesWithoutPrefix in
// routes_test.go.
//
// The parse error is dropped rather than reported because there is nothing
// useful to do with it here and nothing unsafe about it: check has already
// refused an unparseable Covers, and covers binds the zero Addr's own text
// ("invalid IP") to a predicate that matches nothing if one ever reaches it
// anyway. See covers for why binding is the safe failure and omitting would
// not be.
func (rf RouteFilter) predicates() *filters {
	var f filters
	if !f.coversIfAsked("r.prefix", rf.Covers) {
		if rf.Prefix != "" || !rf.wideAsked() {
			f.eq("r.prefix", rf.Prefix)
		}
	}
	f.eqNonEmpty("r.family", rf.Family)
	f.eqAddr("r.router_ip", rf.Router)
	f.eqAddr("r.peer_ip", rf.Peer)
	f.eqNonEmpty("r.rib", rf.RIB)
	f.wideASN(rf.OriginASN, rf.ThroughASN)
	if rf.Community != "" {
		// The error is dropped here because check() has already refused an
		// unparseable value, exactly as coversIfAsked drops covers' parse
		// error for the same reason.
		m, _ := ParseCommunity(rf.Community)
		f.community(m)
	}
	return &f
}

// check validates what the statement cannot: Family, for the reason
// unicastFamilies gives, and Covers, which is text this package turns into
// an address -- see checkCovers, which all three route filters share. rib
// raises on an unknown enum member of its own accord, the two address fields
// are already netip.Addr, and prefix is a String column that legitimately
// matches nothing.
func (rf RouteFilter) check() error {
	if err := checkCovers("/v1/routes/unicast", rf.Prefix, rf.Covers); err != nil {
		return err
	}
	if rf.Community != "" {
		if _, err := ParseCommunity(rf.Community); err != nil {
			return err
		}
	}
	return checkFamily(unicastFamilies, rf.Family, "route_unicast")
}

// VPNRouteFilter is the narrowing VPNRoutes accepts: /v1/routes/vpn's own
// six documented parameters (?prefix=, ?rd=, ?family=, ?router=, ?peer=,
// ?rib=), plus Covers, which /v1/routes documents and fans out to this type
// (see the field). RD and the wider Family vocabulary are why it is not
// RouteFilter:
// route_unicast has no rd column at all, so an rd on the shared struct would
// be a field Routes accepted and ignored, and vpn4/vpn6/lu4 are not values
// route_unicast's family column can hold.
//
// Prefix is OPTIONAL here, which is the one place this type deliberately
// disagrees with RouteFilter, and the reason the two are separate structs
// rather than one embedded in the other. See Prefix's own comment.
type VPNRouteFilter struct {
	// Prefix is an exact match, and unlike RouteFilter.Prefix it is
	// OPTIONAL: "" means "every prefix", not "the empty prefix". The two
	// readings are opposites, so this field is written out here rather than
	// inherited from a struct that documents the other one.
	//
	// Nothing is lost by it. RouteFilter.Prefix has to keep the literal
	// reading because Routes(ctx, RouteFilter{Prefix: ""}) is a question
	// TestRoutes really asks -- an end-of-RIB marker used to answer it --
	// but route_vpn has never carried an empty prefix in any row, and
	// api/openapi.yaml documents ?prefix= on /v1/routes/vpn as optional
	// outright. What replaces it as the guard against an accidental
	// full-table dump is check's prefix-or-rd-or-router rule, which is the
	// contract's own.
	Prefix string

	// Covers is the containment question, the same ADDRESS-valued narrowing
	// RouteFilter.Covers documents and with the same family rule -- see it,
	// and covers itself, for both.
	//
	// It is here because api/openapi.yaml documents ?covers= on /v1/routes,
	// the looking-glass FAN-OUT, whose answer is a keyed object carrying all
	// three families at once. That endpoint cannot be served unless every
	// arm of the fan-out can render the predicate: with Covers on
	// RouteFilter alone, `?covers=X` had no VPN arm to answer at all, and
	// `?covers=X&router=R` was worse -- the VPN arm passed its check on
	// Router and then returned EVERY VPN route that router has, unfiltered
	// by the address, in the same JSON object as a correct unicast answer.
	// A wrong answer with a correct one beside it is the shape this package
	// is written against.
	//
	// route_vpn.prefix is a plain String column exactly as route_unicast's
	// is, and covers is generic over the column it is given, so nothing
	// about the predicate changes here. lu4 and vpn4/vpn6 rows are all
	// reachable by it; the RD is orthogonal.
	//
	// /v1/routes/vpn itself documents no ?covers= parameter, so an HTTP
	// caller reaches this field only through the fan-out. That is a
	// contract decision rather than a limit of this type, and widening it
	// costs a parameter entry and nothing here.
	Covers string

	// RD is an exact match on route_vpn's route distinguisher, "" meaning
	// "not asked for". "" is also a real value that lu4 rows genuinely
	// carry -- BGP-LU has no route distinguisher -- so an RD-less family
	// cannot be selected FOR by this field, only left unfiltered. Family
	// is how a caller asks for lu4, which is exactly the distinction
	// api/openapi.yaml draws ("rd is \"\" and route_targets empty by
	// nature, not by absence of data. family= distinguishes them without
	// inferring from an empty rd").
	RD string

	// Family narrows to one of route_vpn's own families (vpn4, vpn6, lu4),
	// validated against the registry -- see vpnFamilies.
	Family string

	// Router, Peer and RIB mean exactly what RouteFilter's do.
	Router netip.Addr
	Peer   netip.Addr
	RIB    string

	// OriginASN and ThroughASN filter on the LIVE AS path, and zero means
	// "not asked for" in both.
	//
	// Zero cannot mean AS 0 here, and that is safe rather than sloppy: AS 0
	// is reserved by RFC 7607 and originates nothing, and api/ rejects an
	// explicit origin_asn=0 with a 400 before it reaches this struct. The
	// split matches the one ribColumn's doc comment describes -- a value the
	// HTTP layer can refuse against a documented rule is refused there, so
	// this layer's zero value stays free to mean "unset".
	//
	// A route with an EMPTY AS path never matches either filter. query's own
	// originASN returns 0 for an empty path and the wire renders
	// origin_asn: null, because "no path" is not "originated in AS 0"; a
	// filter that matched them would contradict the field it filters on.
	// 1,839 of the archive's 8,611 unicast rows take that branch.
	OriginASN  uint32
	ThroughASN uint32

	// Community is one community in any of the four notations this archive
	// stores, "" meaning not asked for. See ParseCommunity for the dispatch
	// and for why a value can search more than one column.
	Community string

	// Limit caps the rows VPNRoutes RETURNS, not the rows it matches, and 0
	// means no cap. See RouteFilter.Limit for the full argument -- it applies
	// here unchanged: no predicate of its own, spliced in as a bare
	// `LIMIT %d` after vpnRoutesOrder, and deliberately not paired with an
	// offset (CountVPNRoutes is the "how much did you not show me" half; a
	// fleet-wide walk is RIBPageVPN's job, not this one's).
	Limit int
}

// predicates renders vf against vpnRoutesSQL's own `r` alias. Every
// predicate is optional here, prefix included, so the empty filter renders
// to nothing at all -- which is why check below has to run first.
//
// At most ONE prefix-space predicate is rendered: Covers' containment when
// it was asked for, Prefix's exact match otherwise, neither when neither
// was given. That is RouteFilter.predicates' branch minus its unconditional
// arm, and the difference is the one this type exists for -- "" is "every
// prefix" here, not the empty prefix. check's prefix-or-covers-or-rd-or-
// router rule is what stops "neither" from being a fleet-wide dump.
func (vf VPNRouteFilter) predicates() *filters {
	var f filters
	if !f.coversIfAsked("r.prefix", vf.Covers) {
		f.eqNonEmpty("r.prefix", vf.Prefix)
	}
	f.eqNonEmpty("r.rd", vf.RD)
	f.eqNonEmpty("r.family", vf.Family)
	f.eqAddr("r.router_ip", vf.Router)
	f.eqAddr("r.peer_ip", vf.Peer)
	f.eqNonEmpty("r.rib", vf.RIB)
	f.wideASN(vf.OriginASN, vf.ThroughASN)
	if vf.Community != "" {
		// The error is dropped here because check() has already refused an
		// unparseable value, exactly as coversIfAsked drops covers' parse
		// error for the same reason.
		m, _ := ParseCommunity(vf.Community)
		f.community(m)
	}
	return &f
}

// check enforces api/openapi.yaml's own rule for /v1/routes/vpn: "At least
// one of prefix=, rd=, or router= is required -- the same rule as
// /v1/routes/evpn (an unfiltered dump belongs to /v1/rib/vpn)."
//
// Covers satisfies that rule as a fourth member, and it belongs there on the
// rule's own terms rather than as an exception to it: what the three named
// members have in common is that each bounds the answer to something a
// single unpaginated response can carry, and a containment query is a
// prefix-space narrowing that does exactly that -- the routes covering one
// address, at every prefix length. It is the fan-out's only narrowing when a
// caller asks GET /v1/routes?covers=X, so a rule that refused it would make
// that documented request unanswerable.
//
// OriginASN, ThroughASN and Community join the list on the same terms, not
// as a later exception to it: each is a bound on the live AS path or the
// live community set rather than a filter that merely happens to narrow, and
// ThroughASN alone can pin down the one row an operator is chasing (every
// VPN route a given transit AS appears on) as surely as a prefix can.
// Refusing a query that carries one of them and nothing else would refuse
// exactly the question these filters exist to make askable.
//
// Peer, RIB and Family are deliberately NOT on that list, and the asymmetry
// is the contract's rather than an oversight here. /v1/rib/vpn exists to
// walk a whole table, and it does it with a cursor, a page limit and a
// pinned session precisely because that answer is unbounded. This surface
// has none of those, so `?rib=in_pre` alone -- which narrows a fleet-wide
// dump by roughly nothing -- must not be the thing that lets a caller ask
// for one here. Prefix, Covers, RD, Router, OriginASN, ThroughASN and
// Community are the ones that bound the answer to something a single
// unpaginated response can carry.
//
// The rule is enforced here rather than left to the HTTP layer because
// "return everything" is a real query this package would otherwise happily
// run; a validation that lives only in the caller is one a `vantage query`
// invocation or a test helper walks straight past.
func (vf VPNRouteFilter) check() error {
	if err := checkCovers("/v1/routes", vf.Prefix, vf.Covers); err != nil {
		return err
	}
	if vf.Prefix == "" && vf.Covers == "" && vf.RD == "" && !vf.Router.IsValid() &&
		vf.OriginASN == 0 && vf.ThroughASN == 0 && vf.Community == "" {
		return fmt.Errorf("%w: VPNRoutes needs at least one of Prefix, Covers, RD, "+
			"Router, OriginASN, ThroughASN or Community -- an unfiltered VPN dump "+
			"is what the paginated /v1/rib/vpn walk is for, and neither Peer, RIB "+
			"nor Family bounds the answer enough to stand in for one of them",
			ErrBadFilter)
	}
	if vf.Community != "" {
		if _, err := ParseCommunity(vf.Community); err != nil {
			return err
		}
	}
	return checkFamily(vpnFamilies, vf.Family, "route_vpn")
}

// evpnMaxRouteType is the largest EVPN route type EVPNRouteFilter will
// filter on: IANA's own bound. The EVPN Route Types registry runs to 11, and
// api/openapi.yaml types the ?type= parameter as
// `integer, minimum 1, maximum 11` to match.
//
// It was 5 at first -- the five types RFC 7432 and RFC 9136 define, and
// the bound the contract carried first -- and that was backwards. bgp/evpn.go
// sets RouteType unconditionally and carries an undecoded type's bytes in
// Raw with its REAL route type tagged, so sink can and does write a
// route_type above 5, and EVPNRoutes can and does RETURN one. A bound
// enforced on input and unenforceable on output is a bound that makes the
// server violate its own schema while refusing the query that would have
// found the row. The registry's 11 is enforceable in both directions.
//
// Refusing above it is still the deliberate half of the trade, for the
// reason the old bound gave: route_type is a bare UInt8 with no enum behind
// it, so an out-of-range value neither raises nor matches, and returning the
// empty result nobody can tell from a correct one is the failure this
// package keeps being written against. What changed is where the line sits,
// not that there is one.
//
// Only types 2, 3 and 5 occur in the archive, so the name mappings for the
// rest are untested -- api/openapi.yaml says so at the parameter.
const evpnMaxRouteType = 11

// EVPNRouteFilter is the narrowing EVPNRoutes accepts: /v1/routes/evpn's own
// six documented parameters (?prefix=, ?rd=, ?type=, ?router=, ?peer=,
// ?rib=), plus Covers, which /v1/routes documents and fans out to this type
// (see the field).
//
// It is a third struct rather than a reuse of VPNRouteFilter for the reason
// RouteFilter and VPNRouteFilter are two: the dimensions genuinely differ.
// route_evpn has a route_type column the other two tables have nothing to
// put in it, and it has no family column at all -- an EVPN table holds
// exactly one family, so ?family= is a parameter api/openapi.yaml does not
// document on this endpoint and a field EVPNRoutes would have to accept and
// ignore. Prefix keeps VPNRouteFilter's optional reading here, not
// RouteFilter's required one: "" means "every NLRI", which it has to,
// because 136 of the archive's 202 route_evpn rows carry no prefix at all.
type EVPNRouteFilter struct {
	// Prefix is an exact match on route_evpn's own prefix column, "" meaning
	// "not asked for" -- the same reading VPNRouteFilter.Prefix has and the
	// opposite of RouteFilter.Prefix's.
	//
	// It cannot select FOR the prefix-less route types, only leave them
	// unfiltered, and that is a stronger statement here than the same
	// caveat on VPNRouteFilter.RD: "" is the real value every type-2 and
	// type-3 row carries, so a caller looking for IMET routes reaches them
	// with type= or rd=, never by asking for the empty prefix.
	Prefix string

	// Covers is the containment question, here for the reason
	// VPNRouteFilter.Covers gives at length: api/openapi.yaml documents
	// ?covers= on the /v1/routes fan-out, whose answer carries all three
	// families, so an EVPN arm that could not render it would either be
	// missing from that answer or -- worse -- be present and unfiltered
	// beside a correct one.
	//
	// It reaches only the route types that carry an IP prefix. A type-2 or
	// type-3 row's prefix is "", which the predicate's own row-side guard
	// excludes (toIPv6OrNull("") is NULL), so those rows are absent from a
	// covers= answer rather than matched by it -- the right result, since
	// the empty prefix contains no address, and the same shape as
	// Prefix's own "cannot select FOR the prefix-less types" caveat above.
	Covers string

	// RD is an exact match on route_evpn's route distinguisher, "" meaning
	// "not asked for". Unlike route_vpn's, an EVPN RD is not optional in
	// practice -- every route type in RFC 7432 carries one, and all 202
	// archive rows have a non-empty rd -- so "" here has no second reading
	// to collide with.
	RD string

	// RouteType narrows to one EVPN route type, 0 meaning "not asked for".
	// See eqNonZero for why 0 can carry that meaning (IANA reserves route
	// type 0) and evpnMaxRouteType for why a value above the registry's own
	// 11 is refused rather than passed through to a column that would match
	// nothing quietly.
	RouteType uint8

	// Router, Peer and RIB mean exactly what RouteFilter's and
	// VPNRouteFilter's do.
	Router netip.Addr
	Peer   netip.Addr
	RIB    string

	// OriginASN and ThroughASN filter on the LIVE AS path, and zero means
	// "not asked for" in both.
	//
	// Zero cannot mean AS 0 here, and that is safe rather than sloppy: AS 0
	// is reserved by RFC 7607 and originates nothing, and api/ rejects an
	// explicit origin_asn=0 with a 400 before it reaches this struct. The
	// split matches the one ribColumn's doc comment describes -- a value the
	// HTTP layer can refuse against a documented rule is refused there, so
	// this layer's zero value stays free to mean "unset".
	//
	// A route with an EMPTY AS path never matches either filter. query's own
	// originASN returns 0 for an empty path and the wire renders
	// origin_asn: null, because "no path" is not "originated in AS 0"; a
	// filter that matched them would contradict the field it filters on.
	// 1,839 of the archive's 8,611 unicast rows take that branch.
	OriginASN  uint32
	ThroughASN uint32

	// Community is one community in any of the four notations this archive
	// stores, "" meaning not asked for. See ParseCommunity for the dispatch
	// and for why a value can search more than one column.
	Community string

	// Limit caps the rows EVPNRoutes RETURNS, not the rows it matches, and 0
	// means no cap. See RouteFilter.Limit for the full argument -- it applies
	// here unchanged: no predicate of its own, spliced in as a bare
	// `LIMIT %d` after evpnRoutesOrder, and deliberately not paired with an
	// offset (CountEVPNRoutes is the "how much did you not show me" half; a
	// fleet-wide walk is RIBPageEVPN's job, not this one's).
	Limit int
}

// predicates renders ef against evpnRoutesSQL's own `r` alias, the same
// alias RouteFilter.predicates and VPNRouteFilter.predicates hard-code
// against their own statements and for the same reason (see
// RouteFilter.predicates).
//
// The order the predicates are added in is the order their placeholders
// bind. evpnRoutesSQL splices this whole rendering in at one point, but it
// is NOT the statement's only placeholder -- the eor join binds the EVPN
// family token ahead of it -- so EVPNRoutes prepends that value rather than
// passing w.values() alone. See EVPNRoutes' own comment on the argument
// order.
func (ef EVPNRouteFilter) predicates() *filters {
	var f filters
	if !f.coversIfAsked("r.prefix", ef.Covers) {
		f.eqNonEmpty("r.prefix", ef.Prefix)
	}
	f.eqNonEmpty("r.rd", ef.RD)
	f.eqNonZero("r.route_type", ef.RouteType)
	f.eqAddr("r.router_ip", ef.Router)
	f.eqAddr("r.peer_ip", ef.Peer)
	f.eqNonEmpty("r.rib", ef.RIB)
	f.wideASN(ef.OriginASN, ef.ThroughASN)
	if ef.Community != "" {
		// The error is dropped here because check() has already refused an
		// unparseable value, exactly as coversIfAsked drops covers' parse
		// error for the same reason.
		m, _ := ParseCommunity(ef.Community)
		f.community(m)
	}
	return &f
}

// check enforces api/openapi.yaml's own rule for /v1/routes/evpn -- "At
// least one of prefix=, rd=, or router= is required (an unfiltered EVPN dump
// belongs to /v1/rib/evpn)" -- and the ?type= range that endpoint documents.
//
// The first rule is VPNRouteFilter.check's, word for word, Covers, OriginASN,
// ThroughASN and Community included and for the same reasons (see it), and
// it is the contract that makes them identical rather than a copy: peer=,
// rib= and type= are all real filters that really do narrow, and not one of
// them bounds a fleet-wide answer to something a single unpaginated response
// can carry. /v1/rib/evpn has the cursor, the page limit and the pinned
// session that make an unfiltered walk answerable.
//
// It is enforced here rather than in cmd/vantage-api for the reason
// VPNRouteFilter.check gives: "return everything" is a query this package
// would otherwise run happily, and a validation living only in the HTTP
// layer is one `vantage query` and every test helper walks straight past.
func (ef EVPNRouteFilter) check() error {
	if err := checkCovers("/v1/routes", ef.Prefix, ef.Covers); err != nil {
		return err
	}
	if ef.Prefix == "" && ef.Covers == "" && ef.RD == "" && !ef.Router.IsValid() &&
		ef.OriginASN == 0 && ef.ThroughASN == 0 && ef.Community == "" {
		return fmt.Errorf("%w: EVPNRoutes needs at least one of Prefix, Covers, RD, "+
			"Router, OriginASN, ThroughASN or Community -- an unfiltered EVPN dump "+
			"is what the paginated /v1/rib/evpn walk is for, and neither Peer, RIB "+
			"nor RouteType bounds the answer enough to stand in for one of them",
			ErrBadFilter)
	}
	if ef.RouteType > evpnMaxRouteType {
		return fmt.Errorf("%w: EVPN route type %d is outside the 1-%d range IANA "+
			"registers and api/openapi.yaml documents for /v1/routes/evpn -- "+
			"route_type is a bare UInt8, so an out-of-range value would match "+
			"nothing rather than raise, and return an empty result "+
			"indistinguishable from a correct one",
			ErrBadFilter, ef.RouteType, evpnMaxRouteType)
	}
	if ef.Community != "" {
		if _, err := ParseCommunity(ef.Community); err != nil {
			return err
		}
	}
	return nil
}

// PeerFilter is the narrowing Peers accepts: the two optional dimensions
// api/openapi.yaml documents on /v1/peers (?router=, ?rib=).
//
// It is a struct rather than two positional parameters for the reason
// RouteFilter is one, and because two bare arguments of type netip.Addr and
// string are exactly the pair a call site can transpose without the
// compiler noticing once a third arrives.
//
// It renders no predicates of its own the way RouteFilter does: peersSQL
// splices its filter in at seven insertion points, qualified three
// different ways, so the rendering lives in peersStatement where that list
// is maintained. See peersStatement, and TestPeersStatementBindsEveryPlaceholder
// for what holds the two in agreement.
type PeerFilter struct {
	// Router and RIB are each omitted from the statement entirely when
	// unset. A zero Router reports every router; an empty RIB reports every
	// rib, which is not the same as reporting one row per peer -- Peers'
	// grain is (collector, router, peer, rib) either way (see Peer).
	Router netip.Addr
	RIB    string
}
