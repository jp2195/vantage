// GET /v1/topology: the AS-path graph the routes in one scope form.
//
// It is a new path rather than a mode on /v1/routes because it answers a
// different question over the same scope -- that endpoint returns routes,
// this returns the graph those routes form -- and it takes that endpoint's
// scope parameters exactly, so a person who has narrowed a looking-glass
// query already knows how to ask this one.
//
// Everything this file does NOT do is worth naming, because each omission is
// a decision rather than an oversight:
//
//   - No limit=. All three query functions ignore f.Limit deliberately; a
//     cap here would mean choosing which adjacencies to hide, which is a
//     decision no caller asked for and none could detect from the answer.
//     That is also why TestEveryLimitedPathDocumentsTotalMatched does not
//     reach this path, while meta.total_matched is reported anyway.
//
//   - No since=, and so no max_unscoped_since clamp. That clamp bounds a
//     TIME WINDOW; this endpoint has none on any scope, and the unscoped
//     case the clamp exists to police is refused outright by
//     requireTopologyScope. Measured before it was left out rather than
//     assumed -- but read the measurement for exactly what it covers.
//     The 2026-09-11 measurement (docs/measurements.md, "Topology cost")
//     ran ONE statement, the edges SELECT, as first drafted, on a
//     2,000,000-row rig built before query/topology.go existed: origin_asn=
//     at 10% selectivity read 122% of the table at 237-262ms and 283-288MB,
//     while prefix= and router=+peer= were sort-key seeks at 11-32ms.
//     The SHIPPED path is three statements per family, not one -- q.graph()
//     runs edges, nodes and count() sequentially over the same CTE text, and
//     each one re-executes the whole aggregate. So a wide scope's real cost
//     is about three times the figure above, call it 0.7-0.8s of query time
//     for one wide family. That multiplication is INFERRED, not re-measured
//     through the shipped path, and it is stated here because this repo's
//     standing rule is to measure through that path rather than through one
//     component of it; nobody has yet. Peak memory does not triple with it:
//     the three statements are sequential, so one family's peak stays near
//     one statement's 283-288MB, while the three FAMILIES run concurrently
//     below and their peaks can coincide.
//
//     That makes "the same unbounded trade /v1/routes/unicast already ships"
//     an understatement, though not for the reason it first looks: that
//     endpoint is not one statement either. handleUnicastRoutes runs Routes
//     and CountRoutes through rowsAndTotal, which is two reads of the same
//     wide scope -- but CONCURRENTLY, in an errgroup. So the work is three
//     against two while the WALL TIME is about 3x against about 1x, and the
//     wall-time gap is the one an operator feels.
//
//   - No session_dumping warning. query.Graph carries no dump state at all,
//     the way query.Router does not -- dump progress is a property of a
//     (peer, family), which is the grain /v1/peers reports it at, and a
//     warning synthesized here would be this layer asserting a fact rather
//     than reporting one.
package api

import (
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/jp2195/vantage/query"
)

// topologyScopes is the parameter list requireTopologyScope names in its
// refusal, and it is the CONTENT half of the rule: each of these names the
// routes the graph is built from. router= and peer= are the other half and
// are deliberately absent from it -- they name a vantage point rather than a
// set of routes, they are sufficient only TOGETHER, and the message spells
// that pairing out in words rather than hiding it in a list of five.
var topologyScopes = []string{"prefix", "covers", "origin_asn", "through_asn", "community"}

// requireTopologyScope enforces this endpoint's own scope rule: prefix= and
// covers= stay mutually exclusive, and the request must carry either one of
// topologyScopes or router= AND peer= together.
//
// It is requireNarrowing's sibling rather than a call to it, and the reason
// is the MESSAGE. requireNarrowing's ends "which is what the paginated
// /v1/rib paths are for", which is true of /v1/routes and false here: no
// /v1/rib path answers what shape a prefix is reached by, so a caller
// following that advice would be sent to an endpoint that cannot help. A
// shared helper whose message names another endpoint's remedy is a defect,
// not reuse.
//
// That argument covers the SUFFICIENCY half only. The prefix/covers half
// names no remedy, is already parameterized by the endpoint, and is
// therefore genuinely shared: requirePrefixCoversExclusive, called from here
// and from requireNarrowing, so the two are identical by construction, an
// arrangement that stays true only until someone edits one of them.
//
// The rule itself differs from /v1/routes', and that is the endpoint's
// declared departure rather than an inconsistency: there, router=+peer= is
// narrowing only and one of five content filters is required, because one
// peer's RIB is hundreds of thousands of ROUTES. Here the same peer's
// AS-path graph is tens of edges, and router_ip/peer_ip are a prefix of
// every route table's sort key, so that mode is a seek where origin_asn= is
// a scan. Refusing it would delete the screen's "from a peer" mode outright.
//
// query.RouteFilter.checkTopology refuses the same unscoped case underneath
// this, and the duplication is the one requireNarrowing's own doc comment
// describes: the two layers refuse for different reasons -- this one against
// a documented parameter list, that one against a filter struct a Go caller
// can build directly.
func (p *params) requireTopologyScope() {
	if p.err != nil {
		return
	}
	p.requirePrefixCoversExclusive("/v1/topology")
	if p.err != nil {
		return
	}
	for _, name := range topologyScopes {
		if p.v.Get(name) != "" {
			return
		}
	}
	if p.v.Get("router") != "" && p.v.Get("peer") != "" {
		return
	}
	p.fail("/v1/topology needs something to scope the graph to: one of %s names the "+
		"routes, and router= together with peer= names the vantage point -- either "+
		"half of that pair alone is half a scope. rib= narrows an answer but does "+
		"not make one, so without one of these this would draw every route in the "+
		"archive as one graph", joinWithOr(topologyScopes))
}

// handleTopology is the graph fan-out: one scope, all three families, three
// graphs under three required keys.
//
// The three queries run concurrently for handleRoutes' reason -- covers= is
// a full scan by construction and the wide filters are HAVING-filtered scans
// -- and a failure in ANY family fails the whole request rather than
// returning the two that worked. The contract makes all three keys required
// precisely so a caller never has to distinguish "this scope reaches nothing
// there" from "we did not look, because that query failed", and returning a
// partial fan-out under those required keys would reintroduce exactly that
// ambiguity in its worst form: silently, with a 200.
//
// Three filter TYPES rather than one, because query's three topology
// functions take three: VPNRouteFilter carries an rd and a vpn4/vpn6/lu4
// family vocabulary route_unicast's column cannot hold, and EVPNRouteFilter
// carries an EVPN route type. This handler leaves every one of those unset
// -- it fills in only what the contract documents on this path, which is
// /v1/routes' parameter set exactly.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	p := newParams(r)
	p.requireTopologyScope()
	prefix, covers := p.str("prefix"), p.covers()
	router, peer, rib := p.addr("router"), p.addr("peer"), p.rib()
	originASN, throughASN := p.asn("origin_asn"), p.asn("through_asn")
	comm, match := p.community()
	if p.err != nil {
		s.badParam(w, p.err)
		return
	}

	// No Limit on any of the three. See this file's header: all three query
	// functions ignore it, and setting it anyway would put a number in the
	// filter that nothing reads.
	uf := query.RouteFilter{
		Prefix: prefix, Covers: covers, Router: router, Peer: peer, RIB: rib,
		OriginASN: originASN, ThroughASN: throughASN, Community: comm,
	}
	vf := query.VPNRouteFilter{
		Prefix: prefix, Covers: covers, Router: router, Peer: peer, RIB: rib,
		OriginASN: originASN, ThroughASN: throughASN, Community: comm,
	}
	ef := query.EVPNRouteFilter{
		Prefix: prefix, Covers: covers, Router: router, Peer: peer, RIB: rib,
		OriginASN: originASN, ThroughASN: throughASN, Community: comm,
	}

	var unicast, vpn, evpn query.Graph
	g, ctx := errgroup.WithContext(r.Context())
	g.Go(func() (err error) {
		unicast, err = s.q.TopologyUnicast(ctx, uf)
		return err
	})
	g.Go(func() (err error) {
		vpn, err = s.q.TopologyVPN(ctx, vf)
		return err
	})
	g.Go(func() (err error) {
		evpn, err = s.q.TopologyEVPN(ctx, ef)
		return err
	})
	if err := g.Wait(); err != nil {
		s.fail(w, r, err)
		return
	}

	// total_matched is the route identities the scope matched, summed across
	// the three families: the population the graphs were built from. It is
	// NOT a sum over nodes or edges, both of which count each route once per
	// AS or adjacency it touches, and it is what lets a caller tell "six
	// ASNs is the whole answer" from "six ASNs is what fitted". Nothing here
	// caps an answer, so no truncation warning can accompany it.
	total := unicast.Routes + vpn.Routes + evpn.Routes
	// A graph merges every collector's routes, so no row names the collector
	// it came from. The warning is therefore scoped rather than exact: it is
	// raised when any stale session could have contributed -- one with
	// router= when the request names it, any at all when it does not -- and
	// its message says "may". An answer with no routes reads no set.
	set, err := s.staleSet(r.Context(), int(total))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	meta := Meta{TotalMatched: &total, Warnings: staleScopeWarning(set.AnyRouter(router), s.q.StaleAfter())}
	if comm != "" {
		meta.CommunityColumns = match.SearchedColumns()
	}
	s.writeJSON(w, http.StatusOK, Envelope{
		Data: NewWireTopologyFanout(unicast, vpn, evpn),
		Meta: meta,
	})
}
