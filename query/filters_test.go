package query

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/subjects"
)

// TestFiltersOmitsAbsentValues is the property the sentinel pattern could
// not have: an absent filter contributes no condition and no value, so a
// statement built from it runs without the predicate rather than with one
// comparing against a "match everything" literal.
func TestFiltersOmitsAbsentValues(t *testing.T) {
	var f filters
	f.eqAddr("router_ip", netip.Addr{}) // absent: contributes nothing
	f.eq("rib", "in_pre")
	if got, want := f.where(), " AND rib = ?"; got != want {
		t.Errorf("where() = %q, want %q", got, want)
	}
	if got := f.values(); len(got) != 1 || got[0] != "in_pre" {
		t.Errorf("values() = %v, want [in_pre]", got)
	}
}

// TestFiltersOrderHavingArgsAfterWhereArgs pins the one thing that cannot be
// checked by reading the SQL: ClickHouse binds ? positionally, and the HAVING
// clause is rendered after the WHERE, so every HAVING argument must follow
// every WHERE argument in values(). Get this backwards and the statement is
// still valid SQL -- a prefix binds to a community and the answer is silently
// empty.
func TestFiltersOrderHavingArgsAfterWhereArgs(t *testing.T) {
	var f filters
	f.eq("r.prefix", "10.0.0.0/8")
	f.havingExpr("has(live_communities, ?)", uint32(4266459236))
	f.eq("r.family", "ipv4u")
	f.havingExpr("live_as_path[-1] = ?", uint32(65000))

	if got, want := f.where(), " AND r.prefix = ? AND r.family = ?"; got != want {
		t.Errorf("where() = %q, want %q", got, want)
	}
	if got, want := f.having(),
		" AND has(live_communities, ?) AND live_as_path[-1] = ?"; got != want {
		t.Errorf("having() = %q, want %q", got, want)
	}
	want := []any{"10.0.0.0/8", "ipv4u", uint32(4266459236), uint32(65000)}
	if !reflect.DeepEqual(f.values(), want) {
		t.Errorf("values() = %v, want %v -- WHERE args must precede HAVING args",
			f.values(), want)
	}
}

// TestFiltersHavingIsEmptyWhenUnused: a filter with no HAVING predicate must
// render nothing, not a dangling " AND ", or every existing statement breaks.
func TestFiltersHavingIsEmptyWhenUnused(t *testing.T) {
	var f filters
	f.eq("r.prefix", "10.0.0.0/8")
	if got := f.having(); got != "" {
		t.Errorf("having() = %q on a filter with no HAVING predicates, want empty", got)
	}
}

// TestFiltersHavingClauseStandsAlone covers the rendering the link-state
// statements need and no route statement does: a HAVING that brings its own
// keyword. Each of the three route statements carries a fixed
// `HAVING live_is_withdraw = 0` for having() to append an AND to; a
// link-state query with state=any has no post-aggregation predicate at all,
// and a bare `HAVING` with nothing after it is a syntax error rather than a
// permissive filter. It is having()'s counterpart exactly as clause() is
// where()'s, so it is pinned the same way clause() is: both renderings, the
// empty one first.
//
// The two-predicate case is covered here before any caller reaches it.
// LSNodeFilter adds at most one HAVING today, so `HAVING a AND b` is a branch
// nothing in this package has ever rendered -- the resources that follow
// LSNodes have wider filters and will be the first to, and a join that lost
// its separator should fail in this file rather than in whichever of them
// happens to add the second predicate.
func TestFiltersHavingClauseStandsAlone(t *testing.T) {
	var f filters
	if got := f.havingClause(); got != "" {
		t.Errorf("havingClause() = %q on a filter with no HAVING predicates, want empty", got)
	}

	f.havingExpr("live_is_withdraw = 1")
	if got, want := f.havingClause(), "HAVING live_is_withdraw = 1"; got != want {
		t.Errorf("havingClause() = %q, want %q", got, want)
	}

	f.havingExpr("has(live_communities, ?)", uint32(4266459236))
	if got, want := f.havingClause(),
		"HAVING live_is_withdraw = 1 AND has(live_communities, ?)"; got != want {
		t.Errorf("havingClause() = %q, want %q -- the join must separate the "+
			"predicates, not concatenate them", got, want)
	}
	if got, want := strings.Count(f.havingClause(), "?"), len(f.values()); got != want {
		t.Errorf("%d placeholders but %d values", got, want)
	}
}

// TestFiltersBindsEveryPlaceholderExactlyOnce is the invariant that the
// sentinel pattern it replaces kept getting wrong: peersSQL put two
// placeholders at each of seven insertion points and bound one value to all
// fourteen. An eighth insertion point took the count to sixteen -- the
// numbers only ever moved in twos, which is its own reason the hand-kept
// tally was easy to get wrong -- and any miscount was a silent step away
// from a runtime error or, worse, a wrong-but-plausible result.
func TestFiltersBindsEveryPlaceholderExactlyOnce(t *testing.T) {
	var f filters
	f.eqAddr("router_ip", netip.MustParseAddr("10.0.1.1"))
	f.eq("rib", "in_pre")
	f.eq("family", "ipv4u")
	if got, want := strings.Count(f.where(), "?"), len(f.values()); got != want {
		t.Errorf("%d placeholders but %d values", got, want)
	}
}

// TestFiltersRendersAnEmptyBuilderAsNothing pins both renderings for the
// no-filter case, which is the one every unfiltered call in this package
// takes. where() must be splice-able after an existing predicate and
// clause() must leave a bare FROM with no WHERE at all -- a builder that
// returned " AND " or "WHERE " for an empty condition list would produce a
// syntax error at exactly the call that asked for everything, which is the
// call least likely to be covered by a fixture that names a router.
func TestFiltersRendersAnEmptyBuilderAsNothing(t *testing.T) {
	var f filters
	if got := f.where(); got != "" {
		t.Errorf("where() = %q, want %q", got, "")
	}
	if got := f.clause(); got != "" {
		t.Errorf("clause() = %q, want %q", got, "")
	}
	if got := f.values(); len(got) != 0 {
		t.Errorf("values() = %v, want empty", got)
	}
}

// TestFiltersClauseStandsAlone covers the rendering peersSQL's dump_families
// branches need: those branches have no predicate of their own, so the
// filter has to bring its own WHERE keyword rather than an AND to attach to
// one.
func TestFiltersClauseStandsAlone(t *testing.T) {
	var f filters
	f.eqAddr("router_ip", netip.MustParseAddr("10.0.1.1"))
	f.eq("rib", "in_pre")
	if got, want := f.clause(), "WHERE router_ip = toIPv6(?) AND rib = ?"; got != want {
		t.Errorf("clause() = %q, want %q", got, want)
	}
	if got, want := strings.Count(f.clause(), "?"), len(f.values()); got != want {
		t.Errorf("%d placeholders but %d values", got, want)
	}
}

// TestFiltersEqAddrLiftsToIPv6 asserts the one thing about eqAddr a
// result-based test cannot distinguish from a passing query: that the bound
// value is the address's plain textual form and the SQL lifts it with
// toIPv6, rather than the reverse. Binding a netip.Addr straight through, or
// dropping the toIPv6 call, both still return rows for an IPv6 fixture --
// it is the IPv4-mapped storage form of an IPv4 router that makes the
// difference, and every router in this repo's archive is IPv4.
func TestFiltersEqAddrLiftsToIPv6(t *testing.T) {
	var f filters
	f.eqAddr("peer_state.router_ip", netip.MustParseAddr("10.0.0.7"))
	if got, want := f.where(), " AND peer_state.router_ip = toIPv6(?)"; got != want {
		t.Errorf("where() = %q, want %q", got, want)
	}
	got := f.values()
	if len(got) != 1 || got[0] != "10.0.0.7" {
		t.Errorf("values() = %v, want [10.0.0.7] -- toIPv6 takes a String, and "+
			"the zero Addr's own String() is the text \"invalid IP\", which is "+
			"why eqAddr formats the address itself rather than handing the "+
			"driver a netip.Addr to format", got)
	}
}

// TestFiltersOrEqParenthesizesItsAlternatives. orEq is the only method here
// that renders a disjunction, and where() joins everything it renders with
// AND: an unparenthesized `a = ? OR b = ?` binds as `... AND a = ? OR b = ?`,
// and AND binding tighter than OR turns every other predicate in the
// statement into a suggestion. A row matching b alone comes back regardless
// of the router, peer and protocol the caller also asked for.
//
// It is asserted beside a second predicate rather than alone, because alone
// is exactly the case where the missing parentheses cannot be seen.
func TestFiltersOrEqParenthesizesItsAlternatives(t *testing.T) {
	var f filters
	f.eq("l.protocol", uint8(2))
	f.orEq("l.local_area", "l.remote_area", uint32(0))
	want := " AND l.protocol = ? AND (l.local_area = ? OR l.remote_area = ?)"
	if got := f.where(); got != want {
		t.Errorf("where() = %q, want %q", got, want)
	}
	// One value per placeholder, the invariant this whole builder exists to
	// keep: orEq emits two `?` for one value and must append it twice.
	got := f.values()
	if len(got) != 3 || got[1] != uint32(0) || got[2] != uint32(0) {
		t.Errorf("values() = %v, want the protocol then the area twice -- "+
			"every placeholder gets its own appended value", got)
	}
}

// TestFiltersEqNonEmptySkipsTheEmptyString is the counterpart to eq's
// unconditional binding: rib's Enum8 has no empty member, so "" can only
// mean "not asked for", while prefix's "" is a real value TestRoutes asks
// about directly.
func TestFiltersEqNonEmptySkipsTheEmptyString(t *testing.T) {
	var f filters
	f.eqNonEmpty("r.rib", "")
	f.eq("r.prefix", "")
	if got, want := f.where(), " AND r.prefix = ?"; got != want {
		t.Errorf("where() = %q, want %q -- an empty rib is absent, an empty "+
			"prefix is a value", got, want)
	}
	if got := f.values(); len(got) != 1 || got[0] != "" {
		t.Errorf("values() = %v, want [\"\"]", got)
	}
}

// wantCoversText is the containment predicate covers must render for
// r.prefix, spelled out here rather than assembled from coversExpr and
// coversRange. Building it from the same consts the code builds it from
// would assert nothing at all: the test would agree with any edit, including
// a deleted guard or a `+ 96` that became a `+ 32`.
//
// Its one %s is the family test's comparison -- "= 0" for an IPv4 target,
// "> 0" for an IPv6 one -- because that conjunct is the only part of the
// rendering that varies with what was asked, and writing the whole 700
// characters out twice would be two transcriptions to keep in agreement
// rather than one.
//
// Several conjuncts in it are load-bearing and invisible to any row count --
// see the "covers alone" case in TestRouteFilterRendersOnlyWhatWasAsked for
// which ones and why. The rest are pinned by results too, in
// TestRoutesCovers, and are here because a predicate this long is one an
// editor can subtly reshape without any of those results moving.
const wantCoversText = `(toIPv6OrNull(?) IS NOT NULL` +
	` AND position(splitByChar('/', r.prefix)[1], ':') %s` +
	` AND toIPv6OrNull(splitByChar('/', r.prefix)[1]) IS NOT NULL` +
	` AND toUInt8OrNull(splitByChar('/', r.prefix)[2]) <= ` +
	`if(position(splitByChar('/', r.prefix)[1], ':') > 0, 128, 32)` +
	` AND toIPv6OrNull(?) >= IPv6CIDRToRange(` +
	`coalesce(toIPv6OrNull(splitByChar('/', r.prefix)[1]), toIPv6('::')), ` +
	`toUInt8(coalesce(toUInt8OrNull(splitByChar('/', r.prefix)[2]), 0) + ` +
	`if(position(splitByChar('/', r.prefix)[1], ':') > 0, 0, 96))).1` +
	` AND toIPv6OrNull(?) <= IPv6CIDRToRange(` +
	`coalesce(toIPv6OrNull(splitByChar('/', r.prefix)[1]), toIPv6('::')), ` +
	`toUInt8(coalesce(toUInt8OrNull(splitByChar('/', r.prefix)[2]), 0) + ` +
	`if(position(splitByChar('/', r.prefix)[1], ':') > 0, 0, 96))).2)`

// coversV4Text and coversV6Text are wantCoversText's two renderings, named
// so the cases below read as the question each one asks.
var (
	coversV4Text = fmt.Sprintf(wantCoversText, "= 0")
	coversV6Text = fmt.Sprintf(wantCoversText, "> 0")
)

// TestRouteFilterRendersOnlyWhatWasAsked walks the shapes Routes can be
// called in (VPNRoutes takes VPNRouteFilter and has a test of its own). It
// asserts the rendered text rather than a result because the predicate's own
// presence is what a later edit can lose: a filter silently dropped from the
// builder still returns rows, just too many of them, and only a fixture with
// a second router in it would notice. TestRoutesFiltersByRouterPeerAndRib is
// that fixture; this test is what says which predicate went missing when it
// fails.
func TestRouteFilterRendersOnlyWhatWasAsked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		f     RouteFilter
		where string
		args  []any
	}{
		{
			name:  "prefix alone",
			f:     RouteFilter{Prefix: "10.1.0.0/24"},
			where: " AND r.prefix = ?",
			args:  []any{"10.1.0.0/24"},
		},
		{
			// The asymmetry eq's own doc comment argues for, pinned on the
			// type a caller hands in: an empty Prefix with no Covers beside
			// it still renders, bound to "". Making it conditional -- the
			// obvious way to keep an exact match against the empty string
			// out of a covers query -- would turn this call into an
			// unfiltered dump of route_unicast instead of the empty-prefix
			// question TestRoutes asks.
			name:  "the empty prefix, with no covers to displace it",
			f:     RouteFilter{},
			where: " AND r.prefix = ?",
			args:  []any{""},
		},
		{
			// And the displacement itself: covers renders INSTEAD of the
			// exact match, never beside it. An exact match against the
			// empty string left in alongside would make every covers query
			// return nothing at all, which is a wrong answer no fixture
			// would flag as one.
			//
			// The expected text is written out by hand, all 600 characters
			// of it, because two of its conjuncts cannot be caught any
			// other way. `toIPv6OrNull(?) IS NOT NULL` changes no result --
			// a NULL address fails the range comparisons on its own (see
			// covers) -- and the two coalesce calls are equally invisible
			// to a row count: without them the statement does not run at
			// all, so a fixture never gets to disagree with it. Asserting
			// the rendered SQL is what pins all three.
			name:  "covers alone",
			f:     RouteFilter{Covers: "10.77.0.33"},
			where: " AND " + coversV4Text,
			args:  []any{"10.77.0.33", "10.77.0.33", "10.77.0.33"},
		},
		{
			name: "covers with the dimensions that narrow it",
			f: RouteFilter{
				Covers: "10.77.0.33",
				Family: "ipv4u",
				Router: netip.MustParseAddr("10.0.0.70"),
			},
			where: " AND " + coversV4Text + " AND r.family = ? AND r.router_ip = toIPv6(?)",
			args:  []any{"10.77.0.33", "10.77.0.33", "10.77.0.33", "ipv4u", "10.0.0.70"},
		},
		{
			// The family test is the one conjunct that varies with the
			// address asked about, and this is where it is pinned in both
			// directions: an IPv6 target must reach the v6 rendering, or
			// every IPv6 question would be answered by IPv4 prefixes and
			// vice versa. TestRoutesCovers proves the two answers differ;
			// this proves WHICH rendering produced them.
			name:  "an IPv6 covers renders the other side of the family test",
			f:     RouteFilter{Covers: "2001:db8:77::1"},
			where: " AND " + coversV6Text,
			args:  []any{"2001:db8:77::1", "2001:db8:77::1", "2001:db8:77::1"},
		},
		{
			// The Unmap, asserted on the rendering rather than only on a
			// result: an IPv4-mapped Covers must render the IPv4 family test
			// AND bind the dotted-quad text, not the mapped spelling.
			name:  "an IPv4-mapped covers is unmapped before either is chosen",
			f:     RouteFilter{Covers: "::ffff:10.77.0.33"},
			where: " AND " + coversV4Text,
			args:  []any{"10.77.0.33", "10.77.0.33", "10.77.0.33"},
		},
		{
			name:  "router",
			f:     RouteFilter{Prefix: "10.1.0.0/24", Router: netip.MustParseAddr("10.0.0.70")},
			where: " AND r.prefix = ? AND r.router_ip = toIPv6(?)",
			args:  []any{"10.1.0.0/24", "10.0.0.70"},
		},
		{
			name:  "peer",
			f:     RouteFilter{Prefix: "10.1.0.0/24", Peer: netip.MustParseAddr("10.0.0.72")},
			where: " AND r.prefix = ? AND r.peer_ip = toIPv6(?)",
			args:  []any{"10.1.0.0/24", "10.0.0.72"},
		},
		{
			name:  "family",
			f:     RouteFilter{Prefix: "10.1.0.0/24", Family: "ipv4u"},
			where: " AND r.prefix = ? AND r.family = ?",
			args:  []any{"10.1.0.0/24", "ipv4u"},
		},
		{
			name: "all four",
			f: RouteFilter{
				Prefix: "10.1.0.0/24",
				Family: "ipv4u",
				Router: netip.MustParseAddr("10.0.0.70"),
				Peer:   netip.MustParseAddr("10.0.0.72"),
				RIB:    "loc_rib",
			},
			where: " AND r.prefix = ? AND r.family = ? AND r.router_ip = toIPv6(?) AND r.peer_ip = toIPv6(?) AND r.rib = ?",
			args:  []any{"10.1.0.0/24", "ipv4u", "10.0.0.70", "10.0.0.72", "loc_rib"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.predicates()
			if got.where() != tc.where {
				t.Errorf("where() = %q, want %q", got.where(), tc.where)
			}
			gotArgs := got.values()
			if len(gotArgs) != len(tc.args) {
				t.Fatalf("values() = %v, want %v", gotArgs, tc.args)
			}
			for i := range tc.args {
				if gotArgs[i] != tc.args[i] {
					t.Errorf("values()[%d] = %v, want %v", i, gotArgs[i], tc.args[i])
				}
			}
			// The invariant again, on the type callers actually hand in:
			// the args slice above is written out by hand, so this is what
			// catches a predicate that emits two placeholders for one
			// value (or none for one) without the expectation being
			// updated to match it.
			if n := strings.Count(got.where(), "?"); n != len(gotArgs) {
				t.Errorf("%d placeholders but %d values", n, len(gotArgs))
			}
		})
	}
}

// TestRouteFilterRendersEveryFieldItCarries is the structural half of the
// test above: that one walks the shapes a caller writes, this one asserts
// that NO field of the struct can be added without a predicate to render it.
//
// The failure it exists for is silent. A field added to RouteFilter and
// forgotten in predicates() compiles, has a doc comment, is set by a caller
// who reasonably expects it to do something, and narrows nothing at all --
// with every existing test still green, because none of them set it. Counting
// conditions against reflect's own field count is what turns that into a
// compile-time-adjacent failure rather than a wrong answer in production.
// The count it asserts is one BELOW the field count, and only because Prefix
// and Covers are mutually exclusive: a RouteFilter with every field set is
// one check rejects, and predicates renders exactly one prefix predicate for
// either shape. Both shapes are walked rather than one, so that a field
// forgotten in predicates() still fails here whichever half of that pair it
// is added beside.
//
// The count checked below is conds PLUS havings, not conds alone, since
// OriginASN, ThroughASN and Community all render into the HAVING channel
// rather than the WHERE one (see wideASN and filters.community) -- a plain
// len(conds) count would read as though this filter had shrunk by three
// fields the moment any of them was added.
//
// The count is TWO below the field count, not one, now that Limit exists:
// Limit is deliberately the second field this method renders no predicate
// for at all, alongside the unused half of the prefix/covers pair (see
// RouteFilter.Limit). It is still set in both literals below, to the same
// standard every other field is held to -- a Limit that predicates() started
// rendering into a WHERE or HAVING clause would be exactly the kind of
// silent narrowing this test exists to catch, and a literal that left it
// unset could not prove predicates() ignores it on purpose rather than by
// omission.
func TestRouteFilterRendersEveryFieldItCarries(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    RouteFilter
	}{
		{"asking by prefix", RouteFilter{
			Prefix:     "10.1.0.0/24",
			Family:     "ipv4u",
			Router:     netip.MustParseAddr("10.0.0.70"),
			Peer:       netip.MustParseAddr("10.0.0.72"),
			RIB:        "loc_rib",
			OriginASN:  65000,
			ThroughASN: 65001,
			Community:  "65000:100",
			Limit:      10,
		}},
		{"asking by covers", RouteFilter{
			Covers:     "10.1.0.33",
			Family:     "ipv4u",
			Router:     netip.MustParseAddr("10.0.0.70"),
			Peer:       netip.MustParseAddr("10.0.0.72"),
			RIB:        "loc_rib",
			OriginASN:  65000,
			ThroughASN: 65001,
			Community:  "65000:100",
			Limit:      10,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.f.predicates()
			got := len(w.conds) + len(w.havings)
			want := reflect.TypeFor[RouteFilter]().NumField() - 2
			if got != want {
				t.Errorf("predicates() rendered %d conditions for a RouteFilter with "+
					"every field but the unused half of the prefix/covers pair set, "+
					"want %d -- a field was added to the struct without a predicate "+
					"in predicates(), or this literal was not updated to set it",
					got, want)
			}
		})
	}
}

// TestVPNRouteFilterRendersOnlyWhatWasAsked is
// TestRouteFilterRendersOnlyWhatWasAsked's counterpart for the VPN builder,
// and its first case is the difference between the two types: an empty
// Prefix here contributes NOTHING, where RouteFilter's contributes
// `r.prefix = ?` bound to "". Those are opposite readings of the same empty
// string (see VPNRouteFilter.Prefix), and this is where the divergence is
// pinned rather than left to a row count that could be explained a dozen
// other ways.
func TestVPNRouteFilterRendersOnlyWhatWasAsked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		f     VPNRouteFilter
		where string
		args  []any
	}{
		{
			name:  "rd alone, no prefix",
			f:     VPNRouteFilter{RD: "65000:1"},
			where: " AND r.rd = ?",
			args:  []any{"65000:1"},
		},
		{
			name:  "prefix alone",
			f:     VPNRouteFilter{Prefix: "192.168.1.0/24"},
			where: " AND r.prefix = ?",
			args:  []any{"192.168.1.0/24"},
		},
		{
			name:  "family alone",
			f:     VPNRouteFilter{Family: "lu4"},
			where: " AND r.family = ?",
			args:  []any{"lu4"},
		},
		{
			name: "everything",
			f: VPNRouteFilter{
				Prefix: "192.168.1.0/24",
				RD:     "65000:1",
				Family: "vpn4",
				Router: netip.MustParseAddr("10.0.0.70"),
				Peer:   netip.MustParseAddr("10.0.0.72"),
				RIB:    "loc_rib",
			},
			where: " AND r.prefix = ? AND r.rd = ? AND r.family = ? AND r.router_ip = toIPv6(?)" +
				" AND r.peer_ip = toIPv6(?) AND r.rib = ?",
			args: []any{"192.168.1.0/24", "65000:1", "vpn4", "10.0.0.70", "10.0.0.72", "loc_rib"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.predicates()
			if got.where() != tc.where {
				t.Errorf("where() = %q, want %q", got.where(), tc.where)
			}
			gotArgs := got.values()
			if len(gotArgs) != len(tc.args) {
				t.Fatalf("values() = %v, want %v", gotArgs, tc.args)
			}
			for i := range tc.args {
				if gotArgs[i] != tc.args[i] {
					t.Errorf("values()[%d] = %v, want %v", i, gotArgs[i], tc.args[i])
				}
			}
			if n := strings.Count(got.where(), "?"); n != len(gotArgs) {
				t.Errorf("%d placeholders but %d values", n, len(gotArgs))
			}
		})
	}

	// The structural guard, for the reason
	// TestRouteFilterRendersEveryFieldItCarries gives, and in its shape: the
	// count is TWO below the field count because Prefix and Covers are
	// mutually exclusive (BOTH halves are walked so that an eighth field
	// forgotten in predicates() fails here whichever of the pair it is added
	// beside), and Limit renders no predicate of its own at all -- see that
	// test's own comment on why Limit is still set in each literal rather
	// than left at its zero value. It is conds PLUS havings for the same
	// reason TestRouteFilterRendersEveryFieldItCarries's own count is:
	// OriginASN and ThroughASN render into HAVING, not WHERE (see wideASN).
	for _, tc := range []struct {
		name string
		f    VPNRouteFilter
	}{
		{"asking by prefix", VPNRouteFilter{
			Prefix:     "192.168.1.0/24",
			RD:         "65000:1",
			Family:     "vpn4",
			Router:     netip.MustParseAddr("10.0.0.70"),
			Peer:       netip.MustParseAddr("10.0.0.72"),
			RIB:        "loc_rib",
			OriginASN:  65000,
			ThroughASN: 65001,
			Community:  "65000:100",
			Limit:      10,
		}},
		{"asking by covers", VPNRouteFilter{
			Covers:     "192.168.1.33",
			RD:         "65000:1",
			Family:     "vpn4",
			Router:     netip.MustParseAddr("10.0.0.70"),
			Peer:       netip.MustParseAddr("10.0.0.72"),
			RIB:        "loc_rib",
			OriginASN:  65000,
			ThroughASN: 65001,
			Community:  "65000:100",
			Limit:      10,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.f.predicates()
			got := len(w.conds) + len(w.havings)
			want := reflect.TypeFor[VPNRouteFilter]().NumField() - 2
			if got != want {
				t.Errorf("predicates() rendered %d conditions for a VPNRouteFilter "+
					"with every field but the unused half of the prefix/covers "+
					"pair set, want %d -- a field was added to the struct without "+
					"a predicate in predicates(), or this literal was not updated "+
					"to set it", got, want)
			}
		})
	}

	// And the empty filter renders to nothing at all -- which is a valid
	// rendering and an invalid QUERY, the gap VPNRouteFilter.check exists to
	// close. Pinned here so that the day someone makes one of these
	// predicates unconditional, the reason check exists does not quietly
	// stop being true.
	if got := (VPNRouteFilter{}).predicates().where(); got != "" {
		t.Errorf("the empty VPNRouteFilter rendered %q, want \"\" -- if it renders "+
			"a predicate on its own then check's prefix-or-rd-or-router rule is "+
			"guarding something other than an unfiltered dump", got)
	}
}

// TestEVPNRouteFilterRendersOnlyWhatWasAsked is the third builder's
// counterpart to the two tests above. Two things about it are specific to
// this filter rather than repetition of theirs.
//
// RouteType renders through eqNonZero, the only numeric optional predicate
// in the package: 0 has to mean "not asked for" (IANA reserves EVPN route
// type 0) the same way "" means it for rib, and a builder that emitted
// `r.route_type = 0` for an unset field would narrow every unfiltered EVPN
// query to nothing at all.
//
// And there is no Family case, because there is no Family field: route_evpn
// has no family column, so ?family= is a parameter api/openapi.yaml does not
// document on /v1/routes/evpn. Its absence is asserted structurally by
// TestFilterStructsAgreeOnTheirSharedDimensions, not by an omitted case here.
func TestEVPNRouteFilterRendersOnlyWhatWasAsked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		f     EVPNRouteFilter
		where string
		args  []any
	}{
		{
			name:  "rd alone, no prefix",
			f:     EVPNRouteFilter{RD: "65090:2"},
			where: " AND r.rd = ?",
			args:  []any{"65090:2"},
		},
		{
			name:  "route type alone",
			f:     EVPNRouteFilter{RouteType: 3},
			where: " AND r.route_type = ?",
			args:  []any{uint8(3)},
		},
		{
			name:  "an unset route type renders nothing at all",
			f:     EVPNRouteFilter{RD: "65090:2", RouteType: 0},
			where: " AND r.rd = ?",
			args:  []any{"65090:2"},
		},
		{
			name: "everything",
			f: EVPNRouteFilter{
				Prefix:    "192.168.95.0/24",
				RD:        "65090:5",
				RouteType: 5,
				Router:    netip.MustParseAddr("10.0.0.90"),
				Peer:      netip.MustParseAddr("10.0.0.91"),
				RIB:       "in_pre",
			},
			where: " AND r.prefix = ? AND r.rd = ? AND r.route_type = ?" +
				" AND r.router_ip = toIPv6(?) AND r.peer_ip = toIPv6(?) AND r.rib = ?",
			args: []any{"192.168.95.0/24", "65090:5", uint8(5), "10.0.0.90", "10.0.0.91", "in_pre"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.predicates()
			if got.where() != tc.where {
				t.Errorf("where() = %q, want %q", got.where(), tc.where)
			}
			gotArgs := got.values()
			if len(gotArgs) != len(tc.args) {
				t.Fatalf("values() = %v, want %v", gotArgs, tc.args)
			}
			for i := range tc.args {
				if gotArgs[i] != tc.args[i] {
					t.Errorf("values()[%d] = %v (%T), want %v (%T)",
						i, gotArgs[i], gotArgs[i], tc.args[i], tc.args[i])
				}
			}
			if n := strings.Count(got.where(), "?"); n != len(gotArgs) {
				t.Errorf("%d placeholders but %d values", n, len(gotArgs))
			}
		})
	}

	// The structural guard, for the reason
	// TestRouteFilterRendersEveryFieldItCarries gives, and in its shape --
	// two below the field count, both halves of the prefix/covers pair
	// walked, Limit rendering nothing at all, conds plus havings for the
	// reason TestVPNRouteFilterRendersOnlyWhatWasAsked's own copy gives. See
	// that test's own copy.
	for _, tc := range []struct {
		name string
		f    EVPNRouteFilter
	}{
		{"asking by prefix", EVPNRouteFilter{
			Prefix:     "192.168.95.0/24",
			RD:         "65090:5",
			RouteType:  5,
			Router:     netip.MustParseAddr("10.0.0.90"),
			Peer:       netip.MustParseAddr("10.0.0.91"),
			RIB:        "in_pre",
			OriginASN:  65000,
			ThroughASN: 65001,
			Community:  "65000:100",
			Limit:      10,
		}},
		{"asking by covers", EVPNRouteFilter{
			Covers:     "192.168.95.33",
			RD:         "65090:5",
			RouteType:  5,
			Router:     netip.MustParseAddr("10.0.0.90"),
			Peer:       netip.MustParseAddr("10.0.0.91"),
			RIB:        "in_pre",
			OriginASN:  65000,
			ThroughASN: 65001,
			Community:  "65000:100",
			Limit:      10,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.f.predicates()
			got := len(w.conds) + len(w.havings)
			want := reflect.TypeFor[EVPNRouteFilter]().NumField() - 2
			if got != want {
				t.Errorf("predicates() rendered %d conditions for an EVPNRouteFilter "+
					"with every field but the unused half of the prefix/covers "+
					"pair set, want %d", got, want)
			}
		})
	}

	// And the empty filter renders to nothing at all -- a valid rendering
	// and an invalid QUERY, the gap EVPNRouteFilter.check exists to close.
	if got := (EVPNRouteFilter{}).predicates().where(); got != "" {
		t.Errorf("the empty EVPNRouteFilter rendered %q, want \"\" -- if it renders "+
			"a predicate on its own then check's prefix-or-rd-or-router rule is "+
			"guarding something other than an unfiltered dump", got)
	}
}

// TestFilterStructsAgreeOnTheirSharedDimensions is the price of writing the
// three filter structs' shared fields out three times instead of embedding
// one in the others (see RouteFilter's doc comment for why the duplication
// was chosen): a dimension added to one and forgotten on the rest is a
// silent gap, where the embedding would have made it impossible. This is
// what makes it loud instead.
//
// It compares field names and types rather than behavior because that is the
// part duplication actually threatens. Whether each field is RENDERED is
// TestRouteFilterRendersEveryFieldItCarries' job and its two counterparts';
// whether it EXISTS everywhere it should is this one's.
//
// The shape is a shared core plus a declared exception per struct, rather
// than a pairwise comparison, because there are three structs now: pairwise
// is three comparisons and a field can be missing from exactly one of them
// while two of the three pairs still agree. Every field that is NOT in the
// core has to be declared below with the column that justifies it, so adding
// a dimension to one struct and forgetting the others fails here rather than
// in a row count nobody scoped to it.
func TestFilterStructsAgreeOnTheirSharedDimensions(t *testing.T) {
	fields := func(v any) map[string]string {
		rt := reflect.TypeOf(v)
		m := make(map[string]string, rt.NumField())
		for field := range rt.Fields() {
			m[field.Name] = field.Type.String()
		}
		return m
	}

	// The dimensions every route filter carries: the four api/openapi.yaml
	// documents on all three route endpoints and that all three route tables
	// have the columns for, plus Covers.
	//
	// Covers is in the core rather than in three `only` maps because the
	// three-way agreement is the property that failed. It lived on
	// RouteFilter alone while api/openapi.yaml documented ?covers= on
	// /v1/routes -- the FAN-OUT, whose answer carries all three
	// families -- so `?covers=X` had no VPN or EVPN arm that could render
	// it, and `?covers=X&router=R` returned every VPN route on that router,
	// unfiltered by the address, beside a correct unicast answer. That is
	// precisely the "one filter gained a dimension and the others did not"
	// failure this test exists for, and it went unnoticed because the entry
	// justifying the exception was itself wrong about the contract.
	//
	// HistoryFilter declares it in `omits` below: /v1/routes/history has no
	// ?covers= and asks a different question entirely.
	core := map[string]string{
		"Prefix": "string",
		"Covers": "string",
		"Router": "netip.Addr",
		"Peer":   "netip.Addr",
		"RIB":    "string",

		// OriginASN and ThroughASN read the live AS path -- see wideASN --
		// and route_unicast, route_vpn and route_evpn all carry as_path, so
		// all three route filters carry both. HistoryFilter declares them
		// in `omits` below, for the reason it already declares Covers there:
		// /v1/routes/history has no ?origin_asn= or ?through_asn= (see
		// api/openapi.yaml), and it answers a different question in any
		// case -- history is raw, non-deduplicated events, each with its own
		// AS path already on it, not a live-path narrowing over a
		// current-state answer.
		"OriginASN":  "uint32",
		"ThroughASN": "uint32",

		// Community searches live_communities, live_large_communities,
		// live_ext_communities and live_route_targets -- see ParseCommunity --
		// and all three route tables carry all four columns, so all three
		// filters carry it. HistoryFilter declares it in `omits` below for the
		// same reason it declares OriginASN and ThroughASN there: history
		// returns raw events, not the live value a community filter has to
		// read (see wideASN's own comment on the same asymmetry).
		"Community": "string",

		// Limit caps the rows Routes, VPNRoutes and EVPNRoutes RETURN, so all
		// three carry it -- see RouteFilter.Limit. It is NOT sourced from a
		// documented ?limit=: none of the four /v1/routes* paths has one
		// (that parameter is api/openapi.yaml's components.parameters.limit,
		// $ref'd by the three /v1/rib/* walks alone); every route handler
		// sets it from the
		// operator's own s.cfg.MaxPage instead. HistoryFilter declares Limit
		// in `omits` below because Limit is scoped to the three route
		// filters alone, not because /v1/routes/history's own missing
		// ?limit= would otherwise justify it.
		"Limit": "int",
	}

	for _, tc := range []struct {
		name string
		f    any
		// only lists this struct's own extra fields against what justifies
		// each: the column the OTHER tables genuinely lack, or the
		// endpoint api/openapi.yaml documents the parameter on and no
		// other. Either way it is a claim about the contract or the
		// schema, never that this filter is special.
		only map[string]string
		// omits is only's mirror: a SHARED dimension this struct does not
		// carry, against what justifies the absence. An entry here is
		// checked in both directions -- the field must really be missing --
		// so an omission left behind after the field arrives fails rather
		// than silently exempting it from the core check.
		omits map[string]string
	}{
		{"RouteFilter", RouteFilter{}, map[string]string{
			"Family": "route_unicast.family (ipv4u, ipv6u)",
		}, nil},
		{"VPNRouteFilter", VPNRouteFilter{}, map[string]string{
			"RD":     "route_vpn.rd",
			"Family": "route_vpn.family (vpn4, vpn6, lu4)",
		}, nil},
		{"EVPNRouteFilter", EVPNRouteFilter{}, map[string]string{
			"RD": "route_evpn.rd",
			// No Family: route_evpn has no family column at all, since an
			// EVPN table holds exactly one family. See evpnFamily.
			"RouteType": "route_evpn.route_type",
		}, nil},
		{"HistoryFilter", HistoryFilter{}, map[string]string{
			// Justified by the CONTRACT, like RouteFilter's Covers, and for
			// a sharper reason: every other surface in this package answers
			// "what is true now", and a time bound on "now" is not a
			// narrowing those endpoints could accept and ignore -- it is a
			// question they cannot be asked. /v1/routes/history is the one
			// endpoint api/openapi.yaml documents ?since= on.
			"Since": "/v1/routes/history's own ?since= (every other surface " +
				"is current-state, where a time bound has no meaning)",
			// No Family: api/openapi.yaml documents no ?family= on
			// /v1/routes/history, and this struct carries the core's other
			// dimensions precisely because the contract documents those
			// here too.
		}, map[string]string{
			"Covers": "/v1/routes/history documents no ?covers=: history is " +
				"an exact-prefix timeline, and a containment query over it " +
				"would be every event of every covering prefix interleaved, " +
				"which is a different question with a different shape",
			"OriginASN": "/v1/routes/history documents no ?origin_asn=: it " +
				"returns raw events, each already carrying its own AS path " +
				"(HistoryEvent.ASPath), not a live-path narrowing over a " +
				"deduplicated current-state answer",
			"ThroughASN": "/v1/routes/history documents no ?through_asn=, " +
				"for the same reason as OriginASN above",
			"Community": "/v1/routes/history documents no ?community=, for " +
				"the same reason as OriginASN above -- and HistoryEvent does " +
				"not even carry a community set for one to narrow",
			"Limit": "Limit is scoped to the three route filters, and " +
				"/v1/routes/history documents no ?limit= of its own",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fields(tc.f)
			for name, typ := range core {
				if why, declared := tc.omits[name]; declared {
					if _, present := got[name]; present {
						t.Errorf("%s.%s exists, but this case declares it "+
							"omitted (%s) -- delete the omits entry; a stale "+
							"one exempts a real field from the type check "+
							"below", tc.name, name, why)
					}
					continue
				}
				switch have, ok := got[name]; {
				case !ok:
					t.Errorf("%s has no %s field -- every route endpoint "+
						"api/openapi.yaml documents carries this narrowing, so "+
						"a field added to one filter belongs on all three unless "+
						"that table genuinely lacks the column or the contract "+
						"does not document the parameter there, in which case "+
						"say so in this case's `omits` map", tc.name, name)
				case have != typ:
					t.Errorf("%s.%s is %s, want %s -- the same dimension must "+
						"have the same type on every filter", tc.name, name, have, typ)
				}
			}
			for name := range got {
				if _, ok := core[name]; ok {
					continue
				}
				if _, ok := tc.only[name]; !ok {
					t.Errorf("%s.%s is neither one of the shared dimensions nor "+
						"listed as this filter's own; if the other route tables "+
						"genuinely lack the column, or api/openapi.yaml documents "+
						"this parameter on this endpoint alone, add it to this "+
						"case's `only` map with what justifies it -- do NOT add "+
						"the field to the other filters, which would be a "+
						"narrowing they accept and ignore", tc.name, name)
				}
			}
		})
	}
}

// TestFamilySetsComeFromTheSubjectsRegistry is the guard
// TestPeersEVPNFamilyLiteralMatchesSubjectsFamilyToken's doc comment
// describes, applied to the fourth copy of the same registry.
//
// sink writes subjects.FamilyToken(f) into every route row's family column
// (see sink/rows.go's familyName, tied back to the same source by sink's own
// TestFamilyNameMatchesSubjectsFamilyToken). unicastFamilies and vpnFamilies
// are what query/ compares a caller's ?family= against, so a token spelled
// here as a string literal would drift the moment the registry was renamed:
// production would start writing the new token while this package kept
// rejecting it, with every fixture in this file -- which spells its families
// by hand -- still agreeing with the old one. The fixtures are not
// downstream of the registry; only this assertion is.
//
// It also pins the split itself. api/openapi.yaml enumerates ?family= as
// [ipv4u, ipv6u] on /v1/routes/unicast and [vpn4, vpn6, lu4] on
// /v1/routes/vpn, and the two sets have to stay disjoint for the
// wrong-endpoint case (TestRoutesRejectsAFamilyRouteUnicastCannotHold) to
// mean anything.
func TestFamilySetsComeFromTheSubjectsRegistry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     map[string]bool
		members []bgp.Family
	}{
		{"route_unicast", unicastFamilies, []bgp.Family{
			{AFI: 1, SAFI: 1}, // ipv4u
			{AFI: 2, SAFI: 1}, // ipv6u
		}},
		{"route_vpn", vpnFamilies, []bgp.Family{
			{AFI: 1, SAFI: 128}, // vpn4
			{AFI: 2, SAFI: 128}, // vpn6
			{AFI: 1, SAFI: 4},   // lu4
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.set) != len(tc.members) {
				t.Fatalf("%s accepts %d families, want %d: %v", tc.name, len(tc.set), len(tc.members), tc.set)
			}
			for _, f := range tc.members {
				want := subjects.FamilyToken(f)
				if !tc.set[want] {
					t.Errorf("%s does not accept %q, the token subjects.FamilyToken "+
						"gives for AFI %d SAFI %d -- sink writes that token into the "+
						"family column, so a set that disagrees with it rejects the "+
						"very rows the archive holds. Derive the set from the "+
						"registry, do not transcribe it: %v",
						tc.name, want, f.AFI, f.SAFI, tc.set)
				}
			}
		})
	}

	// Disjointness, asserted rather than assumed: one merged set would
	// accept ?family=vpn4 against route_unicast and answer it with the
	// empty result the validation exists to prevent.
	for fam := range unicastFamilies {
		if vpnFamilies[fam] {
			t.Errorf("%q is accepted by both route_unicast and route_vpn; the two "+
				"tables hold disjoint families and the wrong-endpoint check "+
				"depends on it", fam)
		}
	}
}

// TestCheckFamilyAcceptsTheEmptyFamily pins the reading eqNonEmpty gives an
// empty string everywhere else in this file: "not asked for", never "a family
// named the empty string". Without it, every unfiltered Routes call in the
// package would be a 400.
func TestCheckFamilyAcceptsTheEmptyFamily(t *testing.T) {
	if err := checkFamily(unicastFamilies, "", "route_unicast"); err != nil {
		t.Errorf("checkFamily(\"\") = %v, want nil", err)
	}
	err := checkFamily(unicastFamilies, "nope", "route_unicast")
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("checkFamily(\"nope\") = %v, want an error wrapping ErrBadFilter", err)
	}
	// The legal values are listed in the message and sorted, so an operator
	// reading a 400 learns what to send instead and a test can assert it.
	for _, want := range []string{"ipv4u", "ipv6u", "route_unicast"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if i, j := strings.Index(err.Error(), "ipv4u"), strings.Index(err.Error(), "ipv6u"); i > j {
		t.Errorf("error %q lists the legal families out of order; a message that "+
			"reshuffles itself between runs is one nobody can assert on", err)
	}
}

// TestWideFiltersSatisfyTheNarrowingRule: each wide filter is a bound on the
// answer, so each one alone is enough. Previously, VPNRoutes and
// EVPNRoutes both refused anything without Prefix, Covers, RD or Router.
//
// Table-driven over BOTH filter types, not VPNRouteFilter alone:
// VPNRouteFilter.check and EVPNRouteFilter.check are word-for-word copies of
// the same rule, maintained independently (see either one's own doc
// comment), so a test that covered only one would miss the other silently
// diverging.
func TestWideFiltersSatisfyTheNarrowingRule(t *testing.T) {
	t.Run("VPNRouteFilter", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    VPNRouteFilter
		}{
			{"origin alone", VPNRouteFilter{OriginASN: 65000}},
			{"transit alone", VPNRouteFilter{ThroughASN: 65000}},
			{"community alone", VPNRouteFilter{Community: "65000:100"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := tc.f.check(); err != nil {
					t.Errorf("check() = %v, want nil: a wide filter bounds the "+
						"answer and is narrowing on its own", err)
				}
			})
		}
		if err := (VPNRouteFilter{}).check(); err == nil {
			t.Error("an entirely unfiltered VPN query was accepted; that is the " +
				"unbounded dump /v1/rib/vpn exists for")
		}
	})

	t.Run("EVPNRouteFilter", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    EVPNRouteFilter
		}{
			{"origin alone", EVPNRouteFilter{OriginASN: 65000}},
			{"transit alone", EVPNRouteFilter{ThroughASN: 65000}},
			{"community alone", EVPNRouteFilter{Community: "65000:100"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if err := tc.f.check(); err != nil {
					t.Errorf("check() = %v, want nil: a wide filter bounds the "+
						"answer and is narrowing on its own", err)
				}
			})
		}
		if err := (EVPNRouteFilter{}).check(); err == nil {
			t.Error("an entirely unfiltered EVPN query was accepted; that is the " +
				"unbounded dump /v1/rib/evpn exists for")
		}
	})
}

// TestFilterRefusalsDoNotQuoteTheirInput: api/ sends an ErrBadFilter's text
// to the caller verbatim (api/handlers.go's fail), so a filter check that
// quotes the value it refuses is a reflected-content bug one layer up. api/
// validates covers= and the prefix/covers pairing itself on every path that
// reads them, and this holds the second line: /v1/ls/prefixes reached
// checkCovers's pairing message directly until that handler gained its own
// check, and any future handler that forgets one lands here.
func TestFilterRefusalsDoNotQuoteTheirInput(t *testing.T) {
	const marker = "zz-reflected-marker-zz"
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"covers not an address", checkCovers("/x", "", marker)},
		{"covers with a zone", checkCovers("/x", "", "fe80::1%"+marker)},
		{"prefix and covers together", checkCovers("/x", marker, "10.0.0.1")},
		{"family not in the table", checkFamily(unicastFamilies, marker, "t")},
	} {
		if !errors.Is(tc.err, ErrBadFilter) {
			t.Errorf("%s: err = %v, want ErrBadFilter", tc.name, tc.err)
			continue
		}
		if strings.Contains(tc.err.Error(), marker) {
			t.Errorf("%s: the refusal quotes its input: %v", tc.name, tc.err)
		}
	}
}
