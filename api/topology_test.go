// Tests for GET /v1/topology, over insertHandlerFixture's rows -- the same
// fixture every other handler test in this package reads. See
// api/handlers_test.go for why these run against a live ClickHouse rather
// than a mocked driver.Conn.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The two ASNs insertHandlerFixture's unicast rows carry, in as_path order.
// Every route_unicast row it writes has the path [65001, 65002], so the
// unicast graph for that fixture is exactly those two nodes and the one
// edge between them.
//
// fixTransitASN is ALSO the fixture's peer_asn, on both peer_events rows and
// every route row. That coincidence is useful rather than incidental: it is
// what gives node 65001 two roles at once, which is the only way a test can
// see that `roles` is a set rather than an enum.
const (
	fixTransitASN = uint32(65001)
	fixOriginASN  = uint32(65002)
)

// TestTopologyScopeRuleAcceptsRouterAndPeer pins the one place this endpoint
// deliberately disagrees with /v1/routes. There, router=+peer= is narrowing
// only and one of five CONTENT filters is required; here the pair is a
// sufficient scope, because it is a prefix of every route table's sort key
// and because this endpoint returns a graph of tens of edges rather than one
// peer's whole RIB.
//
// The 200 case asserts the graph's CONTENT, not merely the status, and that
// is the load-bearing half. query.RouteFilter.predicates renders an equality
// predicate against the EMPTY prefix for a router=+peer= filter carrying no
// prefix -- a query for a prefix route_unicast has never held -- so that
// scope returns zero rows through the normal path. query.topologyPredicates
// renders the exact-prefix predicate only when Prefix is actually set, and
// this endpoint is the first place that divergence is exercised through the
// shipped HTTP path. An empty graph is a legal 200, so a test that checked
// only the status would stay green with the divergence completely reverted.
//
// It also pins the halves that must still be refused, and asserts the 400
// does NOT recommend the /v1/rib paths -- requireNarrowing's message does,
// and it is wrong here: no /v1/rib path answers "what is the shape of this".
func TestTopologyScopeRuleAcceptsRouterAndPeer(t *testing.T) {
	s := requireAPI(t)

	t.Run("router and peer together are a sufficient scope", func(t *testing.T) {
		var got WireTopologyFanout
		meta := getOK(t, s,
			"/v1/topology?router="+fixRouterIP+"&peer="+fixPeerA, &got)

		// Peer A carries four unicast routes, all on the path
		// [65001, 65002] and none of them withdrawn.
		edge := requireEdge(t, got.Unicast, fixTransitASN, fixOriginASN)
		if edge.Routes != 4 || edge.LiveRoutes != 4 {
			t.Errorf("unicast edge %d -> %d carries routes=%d live_routes=%d, want 4 and 4 "+
				"-- peer A holds four unicast route identities, every one of them live "+
				"and every one on that path",
				fixTransitASN, fixOriginASN, edge.Routes, edge.LiveRoutes)
		}

		// roles is a SET: 65001 is the middle of every fixture path AND the
		// AS of the BMP peer, so it must carry both. An implementation that
		// rendered one role per node would drop one of the two here.
		// The role names are spelled as literals rather than through
		// api/types.go's constants: what is being asserted is the text on the
		// WIRE, and a test written against the constant would follow a
		// renamed value out of the contract without noticing.
		transit := requireNode(t, got.Unicast, fixTransitASN)
		for _, role := range []string{"transit", "observed_peer"} {
			if !slices.Contains(transit.Roles, role) {
				t.Errorf("AS%d's roles are %v, missing %q -- it is the middle hop of "+
					"every fixture route and the AS of the BMP peer, so it holds both "+
					"at once and roles is a set, not an enum",
					fixTransitASN, transit.Roles, role)
			}
		}
		origin := requireNode(t, got.Unicast, fixOriginASN)
		if !slices.Contains(origin.Roles, "origin") {
			t.Errorf("AS%d's roles are %v, missing \"origin\" -- it is the last hop of "+
				"every fixture route", fixOriginASN, origin.Roles)
		}

		// The VPN row's path is one hop long, so it contributes a node and
		// no edge at all. That is the case a graph assembled from its own
		// edge list reports as nothing, and it is also what proves the VPN
		// member is queried rather than copied from the unicast one.
		requireNode(t, got.VPN, fixTransitASN)
		if len(got.VPN.Edges) != 0 {
			t.Errorf("the vpn graph carries %d edges, want 0: the fixture's single VPN "+
				"route has the one-hop path [%d], which asserts no adjacency",
				len(got.VPN.Edges), fixTransitASN)
		}

		// No EVPN rows exist under any scope in this fixture.
		if len(got.EVPN.Nodes) != 0 || len(got.EVPN.Edges) != 0 {
			t.Errorf("the evpn graph is %d nodes and %d edges, want an empty graph -- "+
				"the fixture writes no route_evpn rows at all, so anything here came "+
				"from another family's answer", len(got.EVPN.Nodes), len(got.EVPN.Edges))
		}

		// total_matched is the population the three graphs were built from,
		// summed across them: four unicast route identities plus one VPN
		// route plus no EVPN routes. A sum taken over one family only would
		// read 4.
		if meta.TotalMatched == nil {
			t.Fatal("meta.total_matched is null; it carries the route identities the " +
				"scope matched, which is what tells a caller the graph is the whole " +
				"answer rather than what fitted")
		}
		if *meta.TotalMatched != 5 {
			t.Errorf("meta.total_matched = %d, want 5 -- four unicast route identities, "+
				"one VPN, no EVPN", *meta.TotalMatched)
		}
	})

	for _, q := range []string{
		"/v1/topology",
		"/v1/topology?router=" + fixRouterIP,
		"/v1/topology?peer=" + fixPeerA,
	} {
		t.Run("refused: "+q, func(t *testing.T) {
			body := requireError(t, s, q, http.StatusBadRequest, ErrInvalidParam)
			if strings.Contains(body, "/v1/rib") {
				t.Errorf("the 400 for %q recommends the /v1/rib paths: %s\n"+
					"No /v1/rib path answers what shape a prefix is reached by. "+
					"This endpoint needs its own message, not requireNarrowing's.", q, body)
			}
			if !strings.Contains(body, "peer=") {
				t.Errorf("the 400 for %q never names peer=, so it cannot tell a "+
					"caller that router= alone is half a scope: %s", q, body)
			}
		})
	}

	t.Run("prefix and covers together are still refused", func(t *testing.T) {
		requireError(t, s, "/v1/topology?prefix=10.0.0.0/8&covers=10.1.1.1",
			http.StatusBadRequest, ErrInvalidParam)
	})
}

// TestTopologyEmptyFamilyMarshalsAsAnArray reads the RAW body, and has to: a
// decoded []WireASEdge cannot tell [] from null, so every assertion in the
// test above would pass on a response whose empty families were null.
//
// The claim being pinned is the one WireRouteFanout already makes for
// routes. An empty graph here is the positive answer "this scope reaches
// nothing in that family" -- ordinary rather than exceptional, since EVPN's
// real graph is two edges wide and a one-hop path has no edge at all --
// where null reads as "we did not look".
func TestTopologyEmptyFamilyMarshalsAsAnArray(t *testing.T) {
	rec := get(t, requireAPI(t), "/v1/topology?router="+fixRouterIP+"&peer="+fixPeerA)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/topology = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	type rawGraph struct {
		Nodes json.RawMessage `json:"nodes"`
		Edges json.RawMessage `json:"edges"`
	}
	var env struct {
		Data struct {
			VPN  rawGraph `json:"vpn"`
			EVPN rawGraph `json:"evpn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v; body %s", err, rec.Body.String())
	}
	// The fixture makes both shapes reachable at once: evpn is an empty
	// GRAPH, and vpn is a populated graph whose edge list alone is empty.
	for _, tc := range []struct {
		where string
		got   json.RawMessage
	}{
		{"data.evpn.nodes", env.Data.EVPN.Nodes},
		{"data.evpn.edges", env.Data.EVPN.Edges},
		{"data.vpn.edges", env.Data.VPN.Edges},
	} {
		if string(tc.got) != "[]" {
			t.Errorf("%s is %s, want []: the contract types it as an array with no "+
				"null form, and an empty one is the positive claim that this scope "+
				"reaches nothing there", tc.where, tc.got)
		}
	}
}

// TestTopologyCommunityFilterMatches pins the defect where
// GET /v1/topology?community= raised HTTP 500, because topology's
// per-route aggregate never defined the columns
// query.filters' community predicate names in its HAVING clause
// (live_communities, live_route_targets), where origin_asn= and
// through_asn= -- community's own siblings on that HAVING channel -- both
// returned 200. See query/topology.go's topologySQL for the fix.
//
// It reads insertHandlerFixture's own community fixture rather than writing
// a new one: 10.77.2.0/24 is the only route under peer A carrying the route
// target 65000:100 (see that function's own doc comment), and every route
// under peer A shares the AS path [65001, 65002] -- so the edge this query
// draws is the SAME edge TestTopologyScopeRuleAcceptsRouterAndPeer draws
// unfiltered, and the narrowing is visible only in the route count: 1 rather
// than 4.
func TestTopologyCommunityFilterMatches(t *testing.T) {
	s := requireAPI(t)
	var got WireTopologyFanout
	meta := getOK(t, s,
		"/v1/topology?router="+fixRouterIP+"&peer="+fixPeerA+"&community=65000:100", &got)

	edge := requireEdge(t, got.Unicast, fixTransitASN, fixOriginASN)
	if edge.Routes != 1 || edge.LiveRoutes != 1 {
		t.Errorf("unicast edge %d -> %d carries routes=%d live_routes=%d, want 1 and 1 -- "+
			"10.77.2.0/24 is the only route under peer A carrying the route target "+
			"65000:100", fixTransitASN, fixOriginASN, edge.Routes, edge.LiveRoutes)
	}
	if meta.TotalMatched == nil || *meta.TotalMatched != 1 {
		t.Errorf("meta.total_matched = %v, want 1", meta.TotalMatched)
	}
	want := []string{"live_communities", "live_route_targets"}
	if !slices.Equal(meta.CommunityColumns, want) {
		t.Errorf("meta.community_columns = %v, want %v", meta.CommunityColumns, want)
	}
}

// requireEdge returns the src -> dst edge, failing with the whole edge list
// when it is absent -- the absence is the interesting failure here, and
// "index out of range" would not name it.
func requireEdge(t *testing.T, g WireGraph, src, dst uint32) WireASEdge {
	t.Helper()
	for _, e := range g.Edges {
		if e.Src == src && e.Dst == dst {
			return e
		}
	}
	t.Fatalf("the graph has no edge %d -> %d; its edges are %+v", src, dst, g.Edges)
	return WireASEdge{}
}

func requireNode(t *testing.T, g WireGraph, asn uint32) WireASNode {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ASN == asn {
			return n
		}
	}
	t.Fatalf("the graph has no node AS%d; its nodes are %+v", asn, g.Nodes)
	return WireASNode{}
}
