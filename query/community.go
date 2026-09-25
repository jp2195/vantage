// community.go: turning one ?community= value into the predicates that find
// it, across the four columns this archive stores communities in.
//
// There are four notations because there are four kinds of community and
// three of them are stored as text:
//
//	communities        Array(UInt32)  raw 32-bit; rendered "high:low" on output
//	large_communities  Array(String)  RFC 8092 text, "65101:1:100"
//	ext_communities    Array(String)  "rt:65101:1" where a name is registered,
//	                                  else "type:subtype:value" -- and value
//	                                  may itself contain colons, which is why
//	                                  "128:0:0:256" has four parts
//	route_targets      Array(String)  the rt: subset of ext_communities, stored
//	                                  again with the prefix stripped
//
// A caller should not have to know which of those a value came from, so the
// shape decides, and where a shape can occupy more than one column every one
// of them is searched. That union can only ADD true matches -- every
// predicate is exact membership -- but it can leave a caller unsure whether
// an empty answer means "absent" or "looked in the wrong place", so
// SearchedColumns is reported in meta.
package query

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Column aliases, matching the SELECT lists in routes.go, vpnroutes.go and
// evpnroutes.go. They are the live_ aggregates, not the raw columns, because
// these predicates run in HAVING -- see filters.havings.
const (
	colCommunities      = "live_communities"
	colLargeCommunities = "live_large_communities"
	colExtCommunities   = "live_ext_communities"
	colRouteTargets     = "live_route_targets"
	colASPath           = "live_as_path"
)

// rawColumn maps a live_ aggregate alias to the raw column it aggregates.
//
// It exists for the candidate-key semi-join and for nothing else: a predicate
// written against the live aggregate has a twin written against the raw rows,
// and that twin is a sound SUPERSET of it -- see filters.candidates for the
// argument, which is the whole reason the semi-join is allowed to exist beside
// §4.1's rule.
//
// A miss returns false rather than a guess, and every caller treats a miss as
// "this query gets no semi-join". Rendering a candidate that named a live_
// alias inside the subquery's WHERE would raise UNKNOWN_IDENTIFIER; rendering
// one that named the wrong column would silently narrow the answer. Falling
// back to the statement that was always correct is the only safe third option.
var rawColumn = map[string]string{
	colCommunities:      "r.communities",
	colLargeCommunities: "r.large_communities",
	colExtCommunities:   "r.ext_communities",
	colRouteTargets:     "r.route_targets",
	colASPath:           "r.as_path",
}

// liveAggregate is the argMax that DEFINES each live_ alias, so the semi-join's
// inner statement can declare exactly the aliases its HAVING names and no
// others -- declaring all of them would run the eleven argMax the semi-join
// exists to avoid.
//
// These are second copies of expressions that also live inside routesSQL,
// vpnRoutesSQL and evpnRoutesSQL, which is the one duplication this design
// could not avoid: the statements are consts with the expressions inline.
// TestLiveAggregatesMatchTheirStatements holds the two together.
var liveAggregate = map[string]string{
	colASPath:           "argMax(r.as_path, (r.seq, r.stream_seq))",
	colCommunities:      "argMax(r.communities, (r.seq, r.stream_seq))",
	colLargeCommunities: "argMax(r.large_communities, (r.seq, r.stream_seq))",
	colExtCommunities:   "argMax(r.ext_communities, (r.seq, r.stream_seq))",
	colRouteTargets:     "argMax(r.route_targets, (r.seq, r.stream_seq))",
}

// CommunityMatch is one parsed ?community= value: the columns it will be
// looked for in, and the predicates that do it.
type CommunityMatch struct {
	cols       []string
	predicates []string
	// candidates are predicates' raw-row twins, one for one, for the
	// candidate-key semi-join. Built here rather than derived later because
	// this is where the column each predicate names is known.
	candidates []string
	args       []any
}

// SearchedColumns returns the columns this match will search, sorted, for
// meta.community_columns.
func (m CommunityMatch) SearchedColumns() []string {
	out := slices.Clone(m.cols)
	slices.Sort(out)
	return out
}

func (m *CommunityMatch) add(col, expr string, arg any) {
	m.cols = append(m.cols, col)
	m.predicates = append(m.predicates, expr)
	m.args = append(m.args, arg)
	// The raw-row twin. Every predicate built here is has(<col>, ?) with the
	// column named exactly once, so swapping that one occurrence is the whole
	// rewrite. A column with no raw twin records an empty candidate, which
	// disables the semi-join for the whole match rather than dropping one
	// branch of an OR -- dropping a branch would narrow the candidate set and
	// lose routes.
	raw, ok := rawColumn[col]
	if !ok || !strings.Contains(expr, col) {
		m.candidates = append(m.candidates, "")
		return
	}
	m.candidates = append(m.candidates, strings.Replace(expr, col, raw, 1))
}

// ParseCommunity classifies s by shape and builds the predicates for it.
func ParseCommunity(s string) (CommunityMatch, error) {
	bad := func(why string) (CommunityMatch, error) {
		// s is not echoed: it is caller-supplied text headed for a 400 body
		// and a log line. The message names the notations instead.
		return CommunityMatch{}, fmt.Errorf("%w: community= %s -- it takes a "+
			"standard community (65000:100), a large community (65101:1:100), "+
			"an extended community (rt:65101:1, soo:65000:777, or the "+
			"type:subtype:value form) or a route target (65100:777, "+
			"10.255.0.3:900)", ErrBadFilter, why)
	}
	if s == "" {
		return bad("is empty")
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return bad("has no ':' separator")
	}
	if slices.Contains(parts, "") {
		return bad("has an empty component")
	}

	var m CommunityMatch
	allNumeric := true
	for _, p := range parts {
		if _, err := strconv.ParseUint(p, 10, 64); err != nil {
			allNumeric = false
			break
		}
	}

	switch {
	case len(parts) == 2 && allNumeric:
		hi, errHi := strconv.ParseUint(parts[0], 10, 16)
		lo, errLo := strconv.ParseUint(parts[1], 10, 16)
		if errHi != nil || errLo != nil {
			// Two numeric parts that do not fit 16 bits each cannot be a
			// standard community. It is still a legal route target, so the
			// value is searched there rather than refused.
			m.add(colRouteTargets, "has("+colRouteTargets+", ?)", s)
			return m, nil
		}
		m.add(colCommunities, "has("+colCommunities+", ?)", uint32(hi<<16|lo))
		m.add(colRouteTargets, "has("+colRouteTargets+", ?)", s)
		return m, nil

	case len(parts) == 2:
		// An IPv4 administrator field: "10.255.0.3:900". Anything else with
		// two parts and a non-numeric half is not a shape this archive stores.
		if _, err := netip.ParseAddr(parts[0]); err != nil {
			return bad("is not a notation this archive stores")
		}
		m.add(colRouteTargets, "has("+colRouteTargets+", ?)", s)
		m.add(colExtCommunities, "has("+colExtCommunities+", ?)", "rt:"+s)
		return m, nil

	case allNumeric && len(parts) == 3:
		// Genuinely ambiguous: a large community and an unnamed extended
		// community of the form type:subtype:value are the same text.
		m.add(colLargeCommunities, "has("+colLargeCommunities+", ?)", s)
		m.add(colExtCommunities, "has("+colExtCommunities+", ?)", s)
		return m, nil

	case allNumeric:
		m.add(colExtCommunities, "has("+colExtCommunities+", ?)", s)
		return m, nil

	default:
		// A leading type token: rt, soo, and the rest of bgp.ExtCommName's
		// vocabulary.
		m.add(colExtCommunities, "has("+colExtCommunities+", ?)", s)
		if parts[0] == "rt" {
			m.add(colRouteTargets, "has("+colRouteTargets+", ?)",
				strings.TrimPrefix(s, "rt:"))
		}
		return m, nil
	}
}

// community adds the match to f as a single OR-ed HAVING predicate. OR, not
// AND: the columns are alternative places one value can live, so a match in
// any of them is a match.
func (f *filters) community(m CommunityMatch) {
	if len(m.predicates) == 0 {
		return
	}
	// The candidate is the same OR over the raw columns. It is built only if
	// EVERY branch has a raw twin: an OR missing a branch matches fewer rows,
	// which would narrow the candidate set and lose routes -- so one
	// untranslatable branch disables the semi-join rather than shrinking it.
	var candidate string
	if !slices.Contains(m.candidates, "") && len(m.candidates) == len(m.predicates) {
		candidate = "(" + strings.Join(m.candidates, " OR ") + ")"
	}
	aliases := slices.Clone(m.cols)
	slices.Sort(aliases)
	f.wide("("+strings.Join(m.predicates, " OR ")+")", candidate,
		slices.Compact(aliases), m.args...)
}
