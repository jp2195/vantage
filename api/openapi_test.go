// Contract parity: what makes api/openapi.yaml the source of truth rather
// than a document describing what the code used to do.
//
// The two that carry the weight are two because either one alone is
// comfortable and wrong:
//
//   - The PATH test asks whether every route is documented and every
//     documented path is routed. It passes happily while every response
//     body is misspelled -- it never looks at a body.
//   - The SCHEMA test marshals a golden instance of each response type and
//     validates it against the contract's own schema. It is what catches a
//     renamed JSON field, and it says nothing about whether anything is
//     reachable.
//
// Two more sit under them, both found by mutating the things the pair
// depends on. TestAuthGateMatchesTheContractsSecurity reads which paths the
// contract exempts from auth instead of the router deciding;
// TestEveryDocumentedPropertyIsRequired is the direction all of the above
// are blind to, the contract itself getting weaker.
//
// Both read the EMBEDDED contract (openapiYAML), not the file on disk. That
// is deliberate: the document a running daemon hands out at
// /v1/openapi.yaml is the one compiled into it, so testing the embedded
// bytes tests what ships. Reading openapi.yaml from the source tree would
// pass even if the embed directive pointed somewhere else.
//
// No test HERE can see a route registered outside api/server.go's table --
// there is no ServeMux API to enumerate registrations, so routePatterns()
// is the only view of the mux available, and it is derived from the same
// slice the mux was built from. That is a limit of runtime testing, not of
// testing: TestNoRouteIsRegisteredOutsideTheTable in api/server_test.go
// parses the package's source and fails on a Handle call outside the
// registration loop, which is what actually closes it. What these tests
// enforce is that the table and the contract agree, and that every
// documented path is a path the mux actually matches.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/status"
	"github.com/jp2195/vantage/subjects"
)

// loadContractDoc parses the embedded contract with a real OpenAPI loader.
//
// kin-openapi is a test-only dependency and must stay one. The daemon does
// not read this document at runtime -- it serves the bytes -- so a parser
// in the binary would be a dependency carried for nothing. It is here
// because validating a JSON value against a schema is the whole point of
// the second test, and a hand-rolled "does this map have these keys" check
// is not schema validation: it would not see a null in a non-nullable
// field, an integer where a string is documented, or a value outside an
// enum, which are three of the four ways a wire type drifts.
func loadContractDoc(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openapiYAML)
	if err != nil {
		t.Fatalf("load embedded contract: %v", err)
	}
	if doc.Paths == nil || doc.Paths.Len() == 0 {
		t.Fatal("the embedded contract parsed to zero paths -- the parse is wrong, not the file")
	}
	return doc
}

// documentedOperations flattens the contract into (path, method) pairs, so
// both directions of the path test compare the same kind of thing.
func documentedOperations(t *testing.T, doc *openapi3.T) map[string]map[string]*openapi3.Operation {
	t.Helper()
	out := map[string]map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		ops := item.Operations()
		if len(ops) == 0 {
			t.Errorf("api/openapi.yaml documents %s with no operation on it", path)
		}
		out[path] = ops
	}
	return out
}

// routedOperations reads the mux's own routing table back as (path, method)
// pairs. The method is split off rather than trimmed: a pattern whose
// method changed -- or one that grew a host or a wildcard segment -- has to
// fail this comparison rather than slip through a TrimPrefix that leaves
// the whole pattern in place when the prefix does not match.
func routedOperations(t *testing.T, s *Server) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range s.routePatterns() {
		method, path, ok := strings.Cut(p, " ")
		if !ok {
			t.Fatalf("route pattern %q has no method; every pattern in this "+
				"package is method-qualified so that a wrong method is a 405 "+
				"rather than a 501", p)
		}
		if prev, dup := out[path]; dup {
			t.Errorf("path %s is registered twice (%s and %s)", path, prev, method)
		}
		out[path] = method
	}
	if len(out) == 0 {
		t.Fatal("routePatterns() returned nothing -- the routing table is empty")
	}
	return out
}

// TestEveryRouteIsDocumentedAndViceVersa is the path half of contract
// parity. It runs in both directions on purpose: an undocumented endpoint
// is a promise nobody can find, and a documented endpoint that does not
// exist is a promise that 404s.
//
// The last phase is not redundant with the first two. Those compare a slice
// of strings against a document; this one drives the constructed handler
// and checks the path is actually matched, which is what catches a pattern
// ServeMux accepted but never routes the way its text reads.
func TestEveryRouteIsDocumentedAndViceVersa(t *testing.T) {
	doc := loadContractDoc(t)
	documented := documentedOperations(t, doc)
	s := newTestServer(t)
	routed := routedOperations(t, s)

	for path, method := range routed {
		ops, ok := documented[path]
		if !ok {
			t.Errorf("route %s %s is served but absent from api/openapi.yaml", method, path)
			continue
		}
		if _, ok := ops[method]; !ok {
			t.Errorf("route %s %s is served but api/openapi.yaml documents only %v on that path",
				method, path, methodsOf(ops))
		}
	}
	for path, ops := range documented {
		method, ok := routed[path]
		if !ok {
			t.Errorf("api/openapi.yaml documents %s but nothing serves it", path)
			continue
		}
		for documentedMethod := range ops {
			if documentedMethod != method {
				t.Errorf("api/openapi.yaml documents %s %s but the mux serves %s on that path",
					documentedMethod, path, method)
			}
		}
	}

	// A documented path must be reachable, not merely present in a slice.
	// The probe presents a token, and has to: gate() authenticates before
	// the mux is consulted, so an unauthenticated request answers 401 for a
	// registered and an unregistered path alike. Past the gate, 404 means
	// exactly one thing -- ServeMux matched no pattern.
	for path := range documented {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Errorf("api/openapi.yaml documents %s but the mux 404s it", path)
			}
		})
	}
}

func methodsOf(ops map[string]*openapi3.Operation) []string {
	out := make([]string, 0, len(ops))
	for m := range ops {
		out = append(out, m)
	}
	return out
}

// TestAuthGateMatchesTheContractsSecurity derives from the contract which
// paths are authenticated, instead of restating the answer in the test.
//
// The contract applies `security: [bearerAuth]` at the document level and
// overrides it on two operations -- /v1/openapi.yaml and /v1/auth/config
// each carry their own `security: []`, which per OpenAPI means "no
// authentication for this operation". Whether the spec document and the
// auth-mode probe should be readable without a token is a contract
// question, so the contract answers it and this test makes the router
// obey: adding `security: []` to a data path, or removing it from either of
// those two, fails here until api/server.go's table agrees.
//
// An unauthenticated request is the whole probe. A 401 means the request
// never reached a handler, which is exactly the claim being checked; a
// non-401 on a public path means the middleware was not in the way.
//
// It says nothing about whether the path is REGISTERED -- gate() 401s an
// unknown path too, deliberately. Registration is the path test's job, and
// the two together are what pin "documented, routed, and gated the way the
// contract says".
func TestAuthGateMatchesTheContractsSecurity(t *testing.T) {
	doc := loadContractDoc(t)
	if len(doc.Security) == 0 {
		t.Fatal("the contract has no document-level security, so every path below " +
			"would be 'public' and this test would assert nothing")
	}
	// WHICH scheme, not merely that there is one. api/auth.go implements
	// HTTP Bearer and nothing else -- one Authorization header, the token68
	// after a case-insensitive "Bearer" -- so a contract that named
	// basicAuth here would describe a daemon that does not exist, and the
	// probe below would keep passing because a Basic-auth client gets the
	// same 401 from a Bearer gate.
	const scheme = "bearerAuth"
	def := doc.Components.SecuritySchemes[scheme]
	if def == nil || def.Value == nil {
		t.Fatalf("the contract has no %q security scheme", scheme)
	}
	if def.Value.Type != "http" || def.Value.Scheme != "bearer" {
		t.Errorf("%s is documented as type %q scheme %q; api/auth.go implements HTTP "+
			"Bearer and only that", scheme, def.Value.Type, def.Value.Scheme)
	}
	for _, requirement := range doc.Security {
		for name := range requirement {
			if name != scheme {
				t.Errorf("the document-level security names %q; the router requires %s "+
					"on every gated path and nothing else", name, scheme)
			}
		}
	}

	documented := documentedOperations(t, doc)
	// newTestServerNoDB, not newTestServer: every probe below is
	// unauthenticated, so a gated path is refused before the mux is
	// consulted and the two public ones answer from cfg and from the
	// embedded contract. Requiring a database would only mean this whole
	// contract-to-gate comparison vanishes from a run without one.
	s := newTestServerNoDB(t, testToken)

	gated := 0
	for path, ops := range documented {
		for method, op := range ops {
			// A non-nil Security is an override even when it is empty; nil
			// means "inherit the document's". That distinction IS the
			// exemption, so it must be read from the pointer rather than
			// from the length alone.
			public := op.Security != nil && len(*op.Security) == 0
			if op.Security != nil {
				for _, requirement := range *op.Security {
					for name := range requirement {
						if name != scheme {
							t.Errorf("%s %s names security scheme %q; the router requires "+
								"%s and nothing else", method, path, name, scheme)
						}
					}
				}
			}
			t.Run(method+" "+path, func(t *testing.T) {
				code := serveWithoutCredential(t, s, method, path)
				if public {
					if code == http.StatusUnauthorized {
						t.Errorf("api/openapi.yaml gives %s its own security: [] so it "+
							"must answer without a token, but the router 401s it", path)
					}
					// An operation with no security cannot produce a 401,
					// so documenting one would describe a response nothing
					// can send.
					if r := op.Responses.Status(http.StatusUnauthorized); r != nil {
						t.Errorf("%s is exempt from authentication but documents a 401", path)
					}
					return
				}
				if code != http.StatusUnauthorized {
					t.Errorf("%s inherits the contract's security: [bearerAuth] but an "+
						"unauthenticated request got %d -- it is registered outside the "+
						"auth middleware", path, code)
				}
				// And the 401 the router just sent has to be a documented
				// answer. Deleting the "401" from a gated path leaves the
				// contract describing a response set the daemon does not
				// have, which no other test here can see: the probe above
				// asserts the STATUS, not that it is written down.
				if r := op.Responses.Status(http.StatusUnauthorized); r == nil {
					t.Errorf("%s is gated and the router 401s it, but the contract "+
						"documents no 401 response for it", path)
				}
			})
			if !public {
				gated++
			}
		}
	}
	if gated == 0 {
		t.Fatal("no path was checked as authenticated -- the security read is wrong")
	}
}

// schemaSource says where in the contract a schema lives, so the parity
// table can name an inline response schema as readily as a component.
//
// The envelope is why this type exists. {data, meta} -- the outermost shape
// of all ten JSON responses, and the only shape every single one of them
// shares -- is declared inline under paths:, not as a component. A table
// that could only reach components.schemas therefore left it with no parity
// at all. Verified by mutation: deleting `required: [data, meta]` from
// /v1/routers' 200 schema, and renaming the envelope's `data` to `items` in
// the contract, both used to leave this package green.
type schemaSource struct {
	// component is a name under components.schemas.
	component string
	// responseOf is a path whose GET 200 application/json schema is the
	// subject. Mutually exclusive with component.
	responseOf string
}

func (s schemaSource) String() string {
	if s.component != "" {
		return s.component
	}
	return "envelope" + s.responseOf
}

func (s schemaSource) resolve(t *testing.T, doc *openapi3.T) *openapi3.Schema {
	t.Helper()
	if s.component != "" {
		ref := doc.Components.Schemas[s.component]
		if ref == nil || ref.Value == nil {
			t.Fatalf("contract has no component schema %q", s.component)
		}
		return ref.Value
	}
	schema := jsonResponseSchema(doc, s.responseOf)
	if schema == nil {
		t.Fatalf("contract has no GET %s 200 application/json schema", s.responseOf)
	}
	return schema
}

// jsonResponseSchema returns a path's GET 200 application/json schema, or
// nil where it has none -- /v1/openapi.yaml serves application/yaml, and is
// the one path with no envelope.
func jsonResponseSchema(doc *openapi3.T, path string) *openapi3.Schema {
	item := doc.Paths.Find(path)
	if item == nil || item.Get == nil || item.Get.Responses == nil {
		return nil
	}
	resp := item.Get.Responses.Status(200)
	if resp == nil || resp.Value == nil {
		return nil
	}
	mt := resp.Value.Content.Get("application/json")
	if mt == nil || mt.Schema == nil {
		return nil
	}
	return mt.Schema.Value
}

// envelopeGolden is one path's data field in both variants: what a handler
// would put there for a full page, and for an empty one.
type envelopeGolden struct{ populated, sparse any }

// envelopeGoldens pairs every path that answers with a JSON envelope to the
// data it carries. The test asserts this map and the contract cover exactly
// the same paths, so a path added to api/openapi.yaml has to be given an
// envelope golden rather than quietly going unchecked.
//
// The sparse side is an EMPTY SLICE, never nil, and that is the assertion
// rather than an implementation detail: a nil slice marshals to null, and
// every data field in the contract is typed array (or the fanout object)
// with no null form. An empty page is a positive claim -- "nothing matched"
// -- where null reads as "we did not look".
func envelopeGoldens() map[string]envelopeGolden {
	routers := []WireRouter{NewWireRouter(goldenRouter()), NewWireRouter(sparseRouter())}
	peers := []WirePeer{NewWirePeer(goldenPeer()), NewWirePeer(sparsePeer())}
	unicast := []WireUnicastRoute{
		NewWireUnicastRoute(goldenUnicastRoute()), NewWireUnicastRoute(sparseUnicastRoute())}
	vpn := []WireVPNRoute{NewWireVPNRoute(goldenVPNRoute()), NewWireVPNRoute(sparseVPNRoute())}
	evpn := []WireEVPNRoute{NewWireEVPNRoute(goldenEVPNRoute()), NewWireEVPNRoute(sparseEVPNRoute())}
	history := []WireHistoryEvent{
		NewWireHistoryEvent(goldenHistoryEvent()), NewWireHistoryEvent(sparseHistoryEvent())}
	events := []WirePeerEvent{
		NewWirePeerEvent(goldenPeerEvent()), NewWirePeerEvent(sparsePeerEvent())}
	lsNodes := []WireLSNode{NewWireLSNode(goldenLSNode()), NewWireLSNode(sparseLSNode())}
	lsLinks := []WireLSLink{NewWireLSLink(goldenLSLink()), NewWireLSLink(sparseLSLink())}
	lsPrefixes := []WireLSPrefix{NewWireLSPrefix(goldenLSPrefix()), NewWireLSPrefix(sparseLSPrefix())}
	dumpCounts := []WireRouterDumpCount{
		NewWireRouterDumpCount(goldenRouterDumpCount()), NewWireRouterDumpCount(sparseRouterDumpCount())}
	sessionCounts := []WireRouterSessionCount{
		NewWireRouterSessionCount(goldenRouterSessionCount()), NewWireRouterSessionCount(sparseRouterSessionCount())}
	locRIBs := []WirePeerLocRIB{
		NewWirePeerLocRIB(goldenPeerLocRIB()), NewWirePeerLocRIB(sparsePeerLocRIB())}
	flagCounts := []WireFlagCount{
		NewWireFlagCount(goldenFlagCount()), NewWireFlagCount(sparseFlagCount())}
	churn := []WireChurnBucket{
		NewWireChurnBucket(goldenChurnBucket()), NewWireChurnBucket(sparseChurnBucket())}
	// 3600 -- an hour's window, the Monitor screen's own default -- so the
	// golden's rate is a number a reader can check against its counts rather
	// than an arbitrary float.
	// The golden carries a two-bar activity series and the sparse carries
	// none, so this pair covers both shapes the contract allows: a peer that
	// changed over time, and one whose array is honestly empty.
	peerChurn := []WirePeerChurn{
		NewWirePeerChurn(goldenPeerChurn(), 3600, []query.ChurnPeerActivity{
			{Bucket: goldenTime(), Changes: 3},
			{Bucket: goldenTime().Add(time.Minute), Changes: 1},
		}),
		NewWirePeerChurn(sparsePeerChurn(), 0, nil)}
	prefixChurn := []WirePrefixChurn{
		NewWirePrefixChurn(goldenPrefixChurn()), NewWirePrefixChurn(sparsePrefixChurn())}
	// The four rows named below are the four states GET /v1/collectors' own
	// 200 description commits to -- see goldenCollector's own doc comment --
	// rather than a golden/sparse pair: there is no single query type this
	// endpoint's row converts from (see newWireCollector's own doc comment),
	// so "sparse" here would only mean the same degenerate, never-actually-
	// produced case sparseRouter's zero value already stands in for
	// elsewhere; these four cover real, distinguishable shapes instead.
	collectors := []WireCollector{
		goldenCollector(), archiveOnlyCollector(), unreachableCollector(), configOnlyCollector(),
	}
	// asNames carries BOTH of the two per-row states this endpoint's own
	// contract description commits to, in the SAME populated array rather
	// than a golden/sparse pair -- ASName has no nullable or optional
	// field for "sparse" to exercise (asn, name and country are all
	// required and non-null), so the shape worth pinning here is the
	// listed-vs-unlisted contrast itself: goldenASNamesListedASN's row
	// carries a real name and country, goldenASNamesUnlistedASN's carries
	// both "", the exact wire shape a caller sees for an ASN this dataset
	// simply has never heard of.
	asNames := []WireASName{
		NewWireASName(goldenASNamesListedASN,
			query.ASName{Name: goldenASNamesListedName, Country: goldenASNamesListedCountry}),
		NewWireASName(goldenASNamesUnlistedASN, query.ASName{}),
	}
	return map[string]envelopeGolden{
		"/v1/routers":             {routers, []WireRouter{}},
		"/v1/peers":               {peers, []WirePeer{}},
		"/v1/routes":              {goldenRouteFanout(), NewWireRouteFanout(nil, nil, nil)},
		"/v1/routes/unicast":      {unicast, []WireUnicastRoute{}},
		"/v1/routes/vpn":          {vpn, []WireVPNRoute{}},
		"/v1/routes/evpn":         {evpn, []WireEVPNRoute{}},
		"/v1/routes/history":      {history, []WireHistoryEvent{}},
		"/v1/topology":            {goldenTopologyFanout(), emptyTopologyFanout()},
		"/v1/events":              {events, []WirePeerEvent{}},
		"/v1/rib/unicast":         {unicast, []WireUnicastRoute{}},
		"/v1/rib/vpn":             {vpn, []WireVPNRoute{}},
		"/v1/rib/evpn":            {evpn, []WireEVPNRoute{}},
		"/v1/ls/nodes":            {lsNodes, []WireLSNode{}},
		"/v1/ls/links":            {lsLinks, []WireLSLink{}},
		"/v1/ls/prefixes":         {lsPrefixes, []WireLSPrefix{}},
		"/v1/collection/dumps":    {dumpCounts, []WireRouterDumpCount{}},
		"/v1/collection/sessions": {sessionCounts, []WireRouterSessionCount{}},
		"/v1/collection/locrib":   {locRIBs, []WirePeerLocRIB{}},
		"/v1/collection/flags":    {flagCounts, []WireFlagCount{}},
		"/v1/collection/churn":    {churn, []WireChurnBucket{}},

		"/v1/collection/churn/peers":    {peerChurn, []WirePeerChurn{}},
		"/v1/collection/churn/prefixes": {prefixChurn, []WirePrefixChurn{}},

		"/v1/collectors": {collectors, []WireCollector{}},

		"/v1/asnames": {asNames, []WireASName{}},
	}
}

// goldenASNamesListedASN/Name/Country and goldenASNamesUnlistedASN are
// envelopeGoldens' own fixture for /v1/asnames -- drawn from RFC 6996's
// 32-bit private ASN range (4200000000-4294967294) for the identical
// reason query/asnames_test.go's own distinguish* constants are: neither
// can collide with a real, RIPE-registered holder if this contract test
// ever ran against production data by mistake.
const (
	goldenASNamesListedASN     = 4200000201
	goldenASNamesListedName    = "EXAMPLE-CONTRACT - Example Contract Holding, LLC"
	goldenASNamesListedCountry = "US"
	goldenASNamesUnlistedASN   = 4200000202
)

// goldenCollector is GET /v1/collectors' first state: known to the archive,
// and its process answered. Built through newWireCollector rather than a
// single NewWireX(queryType{}) call -- unlike every other golden in this
// file, one row here is assembled from THREE independent sources (the
// archive summary, the fan-out status, and the activity series), which is
// exactly what newWireCollector itself exists to do, so this reuses it
// rather than duplicating its branching by hand.
func goldenCollector() WireCollector {
	return newWireCollector(
		"dev-c1",
		query.CollectorSummary{
			Collector: "dev-c1",
			Routers: []query.CollectorRouter{{
				SysName: "core1", IP: netip.MustParseAddr("10.0.103.61"),
				PeersUp: 3, PeersDown: 1, PeersViewLost: 0,
				SysDescr: "Cisco IOS XR Software, Version 7.9.1",
				LastSeen: goldenTime(),
			}},
			PeersUp: 3, PeersDown: 1, PeersViewLost: 0, PeersStale: 0, LastRowAt: goldenTime(),
			// Set, so the contract validates the non-null form of both. The
			// archive-only golden below leaves them nil for the null form.
			LastBeatAt: new(goldenTime()), StartedAt: new(goldenTime()),
		},
		true,
		CollectorStatus{
			Endpoint: "http://vantage-collector-0.vantage-collector:9469",
			Report: &status.Report{
				CollectorID: "dev-c1", StartedAt: goldenTime(), ObservedAt: goldenTime(),
				SessionsActive: 4, BMPMessagesTotal: 1000, EventsPublishedTotal: 990,
				PublishErrorsTotal: 2, PublishRejectsTotal: 1,
			},
		},
		true,
		[]WireCollectorActivity{{Minute: goldenTime(), Rows: 12}},
	)
}

// archiveOnlyCollector is the second state: known to the archive, no
// endpoint configured at all. status and reachable are both null on the
// wire -- see WireCollector's own doc comment for why reachable stays null
// here rather than false.
func archiveOnlyCollector() WireCollector {
	return newWireCollector(
		"legacy-c9",
		query.CollectorSummary{
			Collector: "legacy-c9",
			Routers: []query.CollectorRouter{{
				SysName: "edge3", IP: netip.MustParseAddr("10.0.103.62"),
				PeersUp: 1, LastSeen: goldenTime(),
			}},
			PeersUp: 1, LastRowAt: goldenTime(),
		},
		true,
		CollectorStatus{}, false,
		[]WireCollectorActivity{{Minute: goldenTime(), Rows: 0}},
	)
}

// unreachableCollector is the third state: known to the archive, an
// endpoint IS configured, and it did not answer.
func unreachableCollector() WireCollector {
	return newWireCollector(
		"flaky-c4",
		query.CollectorSummary{
			Collector: "flaky-c4",
			Routers: []query.CollectorRouter{{
				SysName: "edge1", IP: netip.MustParseAddr("10.0.103.63"),
				PeersUp: 2, LastSeen: goldenTime(),
			}},
			PeersUp: 2, LastRowAt: goldenTime(),
		},
		true,
		CollectorStatus{
			Endpoint: "http://vantage-collector-3.vantage-collector:9469",
			Err:      "dial tcp 10.0.103.63:9469: connect: connection refused",
		},
		true,
		[]WireCollectorActivity{{Minute: goldenTime(), Rows: 0}},
	)
}

// configOnlyCollector is the fourth state: configured, answering, and not
// in the archive at all -- a collector that has never written a row, or
// one Config.Collectors names under an id the archive has never used.
func configOnlyCollector() WireCollector {
	return newWireCollector(
		"brand-new-c11",
		query.CollectorSummary{}, false,
		CollectorStatus{
			Endpoint: "http://vantage-collector-11.vantage-collector:9469",
			Report: &status.Report{
				CollectorID: "brand-new-c11", StartedAt: goldenTime(), ObservedAt: goldenTime(),
			},
		},
		true,
		[]WireCollectorActivity{{Minute: goldenTime(), Rows: 0}},
	)
}

func goldenRouteFanout() WireRouteFanout {
	return NewWireRouteFanout(
		[]WireUnicastRoute{NewWireUnicastRoute(goldenUnicastRoute())},
		[]WireVPNRoute{NewWireVPNRoute(goldenVPNRoute())},
		[]WireEVPNRoute{NewWireEVPNRoute(goldenEVPNRoute())},
	)
}

// goldenTopologyFanout is one graph per family. Graph, ASNode and ASEdge get
// no case of their own in the test above for LSCommon's reason: $ref reaches
// all three through this type, and a separate case would validate the same
// bytes twice under two names.
func goldenTopologyFanout() WireTopologyFanout {
	return NewWireTopologyFanout(goldenGraph(), goldenGraph(), goldenGraph())
}

// emptyTopologyFanout is the sparse variant, and the empty graph really is
// this type's sparse case rather than a contrived one: a scope that reaches
// nothing in a family is an ordinary answer, EVPN's real graph is two edges
// wide, and nodes and edges are both arrays with no null form. There is no
// zero-VALUE ASNode or ASEdge variant because neither type has a nullable or
// optional field to exercise -- every member is required and non-null, so a
// zero one would assert against a shape query cannot produce.
func emptyTopologyFanout() WireTopologyFanout {
	return NewWireTopologyFanout(query.Graph{}, query.Graph{}, query.Graph{})
}

// goldenGraph's single node carries all three roles at once, which is the
// shape a client rendering `roles` as one value gets wrong -- and not a
// contrived one: query.ASNode's own doc comment calls an AS that originates
// some prefixes and carries others the ordinary shape of a mid-size network
// rather than an edge case.
//
// The three counts are coherent rather than merely non-zero: 420 route
// identities in the population, all of them traversing the node, 380 of them
// carrying the edge and 363 of those still live. A golden whose edge claimed
// more routes than its graph held would be a shape query cannot produce.
//
// Routes is set and appears nowhere in the wire form: it is the population
// the graph was built from, and /v1/topology sums the three families' into
// meta.total_matched rather than repeating each beside its graph.
func goldenGraph() query.Graph {
	return query.Graph{
		Nodes: []query.ASNode{{
			ASN:       65001,
			Routes:    420,
			Origin:    true,
			Transit:   true,
			Peer:      true,
			FirstSeen: goldenTime(),
		}},
		Edges: []query.ASEdge{{
			Src:        65001,
			Dst:        65002,
			Routes:     380,
			LiveRoutes: 363,
			FirstSeen:  goldenTime(),
		}},
		Routes: 420,
	}
}

// TestResponsesValidateAgainstTheContract is the half that actually catches
// a renamed JSON field. The path test above passes happily while every
// response body is wrong.
//
// Two variants per shape, and the second one is the interesting one:
//
//   - populated: every optional field set to a non-zero value, so a field
//     the contract does not document -- or one whose json tag no longer
//     matches the name it is required under -- is caught.
//   - sparse: every field the contract permits to be empty or null left at
//     its zero value, so null handling is exercised. That is where a schema
//     and an implementation most often disagree, and this contract is
//     strict about it: origin_asn, label, next_hop, med and local_pref are
//     nullable-but-required, meaning the key is always present and the
//     value may be null.
//
// It runs over the component schemas AND over every path's inline envelope,
// because the envelope is where the shared shape lives and it is not a
// component. api/types_test.go's TestEnvelopeShape pins the same object as
// a Go-side golden string; that is a different claim -- it says the bytes
// have not changed, not that they satisfy the contract -- and it would keep
// passing while the contract renamed the field underneath it.
//
// The sparse fixtures still carry a legal value in every enum-constrained
// or bounded field (rib, state, family, dump_state, action, route_type).
// That is not the test being bent to pass: "" is not a value the contract
// permits for any of them and not one the query layer can produce -- each
// comes from a column with a closed set of values -- so a fixture carrying
// one would assert against a shape that cannot occur, and the failure it
// produced would hide the null handling the variant exists to check.
//
// What this test does not catch is a key the contract does not document at
// all: none of these schemas sets additionalProperties: false, so an extra
// key validates. TestEmittedKeysAreDocumented in api/types_test.go is that
// direction, and a renamed tag fails both -- here because the old name is
// required and now missing, there because the new name is undocumented.
func TestResponsesValidateAgainstTheContract(t *testing.T) {
	doc := loadContractDoc(t)
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("contract is not a valid OpenAPI document: %v", err)
	}
	cases := []struct {
		source  schemaSource
		variant string
		golden  any
	}{
		{schemaSource{component: "Router"}, "populated", NewWireRouter(goldenRouter())},
		{schemaSource{component: "Router"}, "sparse", NewWireRouter(sparseRouter())},
		{schemaSource{component: "Peer"}, "populated", NewWirePeer(goldenPeer())},
		{schemaSource{component: "Peer"}, "sparse", NewWirePeer(sparsePeer())},
		{schemaSource{component: "UnicastRoute"}, "populated", NewWireUnicastRoute(goldenUnicastRoute())},
		{schemaSource{component: "UnicastRoute"}, "sparse", NewWireUnicastRoute(sparseUnicastRoute())},
		{schemaSource{component: "VPNRoute"}, "populated", NewWireVPNRoute(goldenVPNRoute())},
		{schemaSource{component: "VPNRoute"}, "sparse", NewWireVPNRoute(sparseVPNRoute())},
		{schemaSource{component: "EVPNRoute"}, "populated", NewWireEVPNRoute(goldenEVPNRoute())},
		{schemaSource{component: "EVPNRoute"}, "sparse", NewWireEVPNRoute(sparseEVPNRoute())},
		{schemaSource{component: "HistoryEvent"}, "populated", NewWireHistoryEvent(goldenHistoryEvent())},
		{schemaSource{component: "HistoryEvent"}, "sparse", NewWireHistoryEvent(sparseHistoryEvent())},
		{schemaSource{component: "PeerEvent"}, "populated", NewWirePeerEvent(goldenPeerEvent())},
		{schemaSource{component: "PeerEvent"}, "sparse", NewWirePeerEvent(sparsePeerEvent())},
		// LSCommon and LSEndpoint have no case of their own: allOf (LSNode,
		// LSPrefix) and $ref (LSLink.local/remote) reach them through their
		// parents, and TestLSEndpointLabelSurvivesTheZeroValue in
		// api/types_test.go is what pins WireLSEndpoint's own shape.
		{schemaSource{component: "LSNode"}, "populated", NewWireLSNode(goldenLSNode())},
		{schemaSource{component: "LSNode"}, "sparse", NewWireLSNode(sparseLSNode())},
		{schemaSource{component: "LSLink"}, "populated", NewWireLSLink(goldenLSLink())},
		{schemaSource{component: "LSLink"}, "sparse", NewWireLSLink(sparseLSLink())},
		{schemaSource{component: "LSPrefix"}, "populated", NewWireLSPrefix(goldenLSPrefix())},
		{schemaSource{component: "LSPrefix"}, "sparse", NewWireLSPrefix(sparseLSPrefix())},
		{schemaSource{component: "RouteFanout"}, "populated", goldenRouteFanout()},
		{schemaSource{component: "RouteFanout"}, "sparse", NewWireRouteFanout(nil, nil, nil)},
		{schemaSource{component: "TopologyFanout"}, "populated", goldenTopologyFanout()},
		{schemaSource{component: "TopologyFanout"}, "sparse", emptyTopologyFanout()},
		{schemaSource{component: "RouterDumpCount"}, "populated", NewWireRouterDumpCount(goldenRouterDumpCount())},
		{schemaSource{component: "RouterDumpCount"}, "sparse", NewWireRouterDumpCount(sparseRouterDumpCount())},
		{schemaSource{component: "RouterSessionCount"}, "populated", NewWireRouterSessionCount(goldenRouterSessionCount())},
		{schemaSource{component: "RouterSessionCount"}, "sparse", NewWireRouterSessionCount(sparseRouterSessionCount())},
		{schemaSource{component: "PeerLocRIB"}, "populated", NewWirePeerLocRIB(goldenPeerLocRIB())},
		{schemaSource{component: "PeerLocRIB"}, "sparse", NewWirePeerLocRIB(sparsePeerLocRIB())},
		{schemaSource{component: "FlagCount"}, "populated", NewWireFlagCount(goldenFlagCount())},
		{schemaSource{component: "FlagCount"}, "sparse", NewWireFlagCount(sparseFlagCount())},
		{schemaSource{component: "Meta"}, "populated", Meta{
			NextCursor:       new("opaque"),
			Warnings:         Warnings{{Code: WarnSessionDumping, Message: "peer 10.0.103.61 is mid-dump"}},
			TotalMatched:     new(uint64(3250)),
			CommunityColumns: []string{"live_communities", "live_route_targets"},
			DumpTotals:       &WireDumpTotals{Archived: 100, Dumps: 40, Changes: 60},
			SessionTotals:    &WireSessionTotals{Sessions: 9, Up: 4, Down: 3, ViewLost: 2},
			LocRIBTotals:     &WireLocRIBTotals{Reported: 140, Archived: 610},
			FlagTotals:       &WireFlagTotals{Envelopes: 58},
			ChurnBucket:      new("5m0s"),
			ActivityWindow:   new("30m0s"),
			RetentionDays:    new(90),
			ASNamesLoaded:    new(true),
			ASNamesPublished: new(goldenTime()),
		}},
		{schemaSource{component: "Meta"}, "sparse", Meta{}},
		{schemaSource{component: "ErrorResponse"}, "populated", ErrorResponse{Error: ErrorBody{
			Code: ErrInvalidParam, Message: "prefix and covers are mutually exclusive"}}},
		// /v1/auth/config is the one JSON path that is not the {data, meta}
		// envelope every other path shares -- it predates having a token to
		// authenticate with, so it has nothing to page -- and is excluded
		// from the envelope loop below for exactly that reason. Each mode
		// the enum permits gets its own golden here instead.
		{schemaSource{responseOf: "/v1/auth/config"}, "token", struct {
			Mode string `json:"mode"`
		}{Mode: string(AuthModeToken)}},
		{schemaSource{responseOf: "/v1/auth/config"}, "none", struct {
			Mode string `json:"mode"`
		}{Mode: string(AuthModeNone)}},
		{schemaSource{responseOf: "/v1/auth/config"}, "oidc", struct {
			Mode string `json:"mode"`
		}{Mode: string(AuthModeOIDC)}},
	}

	// Every path that answers with a JSON envelope, in both variants, and
	// the two directions of coverage between the contract and the goldens.
	goldens := envelopeGoldens()
	for path := range doc.Paths.Map() {
		if jsonResponseSchema(doc, path) == nil {
			continue // /v1/openapi.yaml: application/yaml, no envelope.
		}
		if path == "/v1/auth/config" {
			continue // {mode}, not an envelope; covered directly above.
		}
		g, ok := goldens[path]
		if !ok {
			t.Errorf("api/openapi.yaml documents a JSON envelope for %s but no golden "+
				"covers it, so its data field is unchecked", path)
			continue
		}
		src := schemaSource{responseOf: path}
		cases = append(cases,
			struct {
				source  schemaSource
				variant string
				golden  any
			}{src, "populated", Envelope{Data: g.populated, Meta: Meta{
				NextCursor: new("opaque"),
				Warnings:   Warnings{{Code: WarnPaginatedSmear, Message: "page set is a smear"}},
			}}},
			struct {
				source  schemaSource
				variant string
				golden  any
			}{src, "sparse", Envelope{Data: g.sparse, Meta: Meta{}}},
		)
	}
	for path := range goldens {
		if jsonResponseSchema(doc, path) == nil {
			t.Errorf("envelopeGoldens covers %s, which the contract does not document "+
				"as a JSON response", path)
		}
	}

	for _, tc := range cases {
		t.Run(tc.source.String()+"/"+tc.variant, func(t *testing.T) {
			schema := tc.source.resolve(t, doc)
			b, err := json.Marshal(tc.golden)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := schema.VisitJSON(v); err != nil {
				t.Errorf("%s does not satisfy its own contract schema: %v\n\t%s",
					tc.source, err, b)
			}
		})
	}
}

// optionalByDesign is the contract's documented exceptions to the rule
// TestEveryDocumentedPropertyIsRequired enforces, keyed by "Schema.property".
// Every entry is a real exception rather than an oversight, and each is
// listed here so that a new one cannot be added without saying why.
//
// Meta.next_cursor is optional in the contract, and api/types.go emits it
// anyway -- as null on a last page -- because "present and null" is the
// shape every other nullable field commits to and a client should not have
// to read the last page differently from every other page. The contract is
// the looser of the two on purpose (a future writer of Meta may omit it).
//
// Meta.community_columns is the opposite shape: the contract is looser
// because api/types.go really does OMIT the key, with omitempty, whenever
// the request carried no community=. Unlike every other field here, an
// empty array would be ambiguous -- it would read as "searched, found
// nothing" rather than "not asked" -- so absence is the one honest encoding
// and this is the one field required cannot cover.
//
// Meta.dump_totals, Meta.session_totals, Meta.locrib_totals and
// Meta.flag_totals are community_columns' own shape, four times over: each
// is the fleet total for exactly ONE of the four /v1/collection/* signals
// (see api/collection.go's dumpTotals/sessionTotals/locRIBTotals/
// flagTotals), and api/types.go's Meta OMITS the other three whenever it
// answers any one of them -- and omits all four on every endpoint outside
// this family. required cannot express "present on exactly one sibling
// response and absent on every other," so these stay optional the same way
// community_columns already does, for the same reason: a key that is truly
// never sent on most responses is honestly absent, not present and null.
var optionalByDesign = map[string]bool{
	"Meta.next_cursor":       true,
	"Meta.community_columns": true,
	"Meta.dump_totals":       true,
	"Meta.session_totals":    true,
	"Meta.locrib_totals":     true,
	"Meta.flag_totals":       true,
	// churn_bucket joins them for the same reason and with one difference
	// worth naming: unlike the four totals it is not a sum, it is the width
	// every bar in a /v1/collection/churn answer was computed at. On that
	// path it is ALWAYS sent, empty data included -- a chart labeled with a
	// width it was not drawn at is the defect this field exists to prevent
	// -- and on every other path it is honestly absent rather than null.
	"Meta.churn_bucket": true,

	// Meta.churn_from and Meta.churn_to travel WITH churn_bucket and share
	// its shape exactly: always sent on /v1/collection/churn, empty data
	// included, and honestly absent on every other path. They are the other
	// half of what churn_bucket exists for. The width says what a bar MEANS
	// and these say where it SITS -- and without them a client bounds the
	// axis with its own clock, drawing collector timestamps against browser
	// time, which is what Monitor did until 2026-09-21.
	"Meta.churn_from": true,
	"Meta.churn_to":   true,

	// Meta.activity_window is churn_bucket's own shape, for the identical
	// reason: it names the span every Collector.activity array in a
	// /v1/collectors response was computed over, not a sum, so it joins
	// churn_bucket rather than the four totals above. Always sent on that
	// path, data empty included; honestly absent on every other one.
	"Meta.activity_window": true,

	// Meta.retention_days is activity_window's shape, on the same path: sent
	// on /v1/collectors whenever the history TTL is a whole number of days,
	// and absent on every other path, where the archive's reach is not the
	// question.
	"Meta.retention_days": true,

	// Meta.asnames_loaded and Meta.asnames_published join churn_bucket and
	// activity_window's own shape, for GET /v1/asnames, and both are
	// honestly absent -- never present-and-null -- on every other
	// endpoint's Meta, where the question of whether AN UNRELATED ASN
	// dataset is loaded does not apply. They differ from each other in
	// how far that goes: asnames_loaded is ALWAYS sent on /v1/asnames,
	// true or false, because "is there a dataset" always has an answer;
	// asnames_published is sent there only when there IS a date, and is
	// omitted rather than nulled when there is not (see Meta's own doc
	// comment in types.go for why emitting null instead would be worse).
	"Meta.asnames_loaded":    true,
	"Meta.asnames_published": true,
}

// TestEveryDocumentedPropertyIsRequired is the direction every other test in
// this package is blind to: the contract getting WEAKER.
//
// Verified by mutation. Deleting origin_asn from RouteCommon.required and
// running the whole api package still printed ok -- every other test here
// asks whether the code conforms to the contract, and a contract that has
// gone quiet about a field is easier to conform to, not harder. That is not
// a harmless edit: required is what api/types.go's header means by "the key
// is ALWAYS PRESENT, not that its value is non-null", and the moment a
// field drops off a required list, a handler may start omitting it and no
// test in this repo will fail. Then a caller has to tell "the server
// omitted this" from "this route has no MED", which is exactly the
// distinction the contract exists to remove.
//
// So the rule is: every property of every response schema is required, with
// the single exception listed in optionalByDesign. Nested inline objects
// (Meta.warnings.items, ErrorResponse.error) are held to it too. Schemas
// reached by $ref are skipped where they are referenced and checked where
// they are defined, so a failure names the component that owns the field.
func TestEveryDocumentedPropertyIsRequired(t *testing.T) {
	doc := loadContractDoc(t)
	if len(doc.Components.Schemas) == 0 {
		t.Fatal("the contract parsed to zero component schemas -- the parse is wrong")
	}
	checked := 0
	var walk func(path string, s *openapi3.Schema)
	walk = func(path string, s *openapi3.Schema) {
		if s == nil {
			return
		}
		required := map[string]bool{}
		for _, r := range s.Required {
			required[r] = true
		}
		for name, prop := range s.Properties {
			checked++
			if !required[name] && !optionalByDesign[path+"."+name] {
				t.Errorf("%s.%s is documented but not in that schema's required list. "+
					"required here means the key is always present, not that its value "+
					"is non-null; a property that is not required may be omitted, and "+
					"nothing else in this package would notice", path, name)
			}
			// Only inline subschemas: a $ref is checked at its own
			// definition, and following it here would report the same
			// field twice under two names.
			if prop.Ref == "" {
				walk(path+"."+name, prop.Value)
			}
		}
		if s.Items != nil && s.Items.Ref == "" {
			walk(path+".items", s.Items.Value)
		}
		for i, member := range s.AllOf {
			if member.Ref == "" {
				walk(fmt.Sprintf("%s.allOf[%d]", path, i), member.Value)
			}
		}
	}
	for name, ref := range doc.Components.Schemas {
		walk(name, ref.Value)
	}

	// And the inline schemas under paths:, which is where the ENVELOPE
	// lives. Walking only components.schemas left the one shape every JSON
	// response shares -- {data, meta} -- outside this rule entirely, and
	// deleting its `required: [data, meta]` was a change nothing in the
	// package noticed. Anything reached by $ref from here is skipped; it is
	// a component and was walked above.
	responses := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Responses == nil {
				continue
			}
			for status, resp := range op.Responses.Map() {
				if resp.Ref != "" || resp.Value == nil {
					continue
				}
				for mime, mt := range resp.Value.Content {
					if mt.Schema == nil || mt.Schema.Ref != "" {
						continue
					}
					responses++
					walk(fmt.Sprintf("%s %s %s %s", method, path, status, mime), mt.Schema.Value)
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no property was examined -- the schema walk is wrong, not the contract")
	}
	if responses == 0 {
		t.Fatal("no inline response schema was examined, so the envelope went unchecked " +
			"-- the paths walk is wrong, not the contract")
	}
}

// jsonOf marshals v and decodes it into a map, so a negative fixture can be
// built by editing one field of a value that is otherwise known-good.
func jsonOf(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func nested(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	sub, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object in the golden: %T", key, m[key])
	}
	return sub
}

func firstElem(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	arr, ok := m[key].([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("%q is not a non-empty array in the golden: %T", key, m[key])
	}
	elem, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("%q[0] is not an object: %T", key, arr[0])
	}
	return elem
}

// TestTheContractRejectsWhatItForbids is the negative half of schema
// parity, and it is what stops the contract from quietly RELAXING.
//
// TestResponsesValidateAgainstTheContract only ever asks whether a legal
// value is accepted, so every constraint in the document could be deleted
// and it would stay green -- a schema that forbids nothing accepts
// everything. Measured, before this test existed: deleting the enum on
// Peer.rib, on dump_state and on action; deleting the minimum/maximum on
// route_type; widening sysname to [string, "null"]; erasing peers_up's type
// to {}; and dropping the additionalProperties enum on dump_states all left
// the whole package green.
//
// One known-illegal value per constrained field kills that entire class at
// once, which is why this is a table rather than a list of tests per
// construct: an enum that was deleted, a bound that was removed, a type
// that was erased and a union that was widened all stop rejecting the same
// fixture. Each case names the construct it is standing guard over, so a
// failure says which line of the contract went missing rather than just
// "expected an error".
//
// It doubles as the guard against the positive test being vacuous: if
// VisitJSON returned nil unconditionally -- a schema that resolved to
// empty, a loader that dropped the required lists -- every case here fails
// at once.
func TestTheContractRejectsWhatItForbids(t *testing.T) {
	doc := loadContractDoc(t)
	routers := Envelope{Data: []WireRouter{NewWireRouter(goldenRouter())}, Meta: Meta{}}
	for _, tc := range []struct {
		name   string
		source schemaSource
		base   any
		mutate func(*testing.T, map[string]any)
		guards string
	}{
		// Required lists. A renamed json tag reaches the contract as
		// exactly this shape.
		{"Router/no ip", schemaSource{component: "Router"}, NewWireRouter(goldenRouter()),
			func(_ *testing.T, m map[string]any) { delete(m, "ip") }, "Router.required"},
		{"UnicastRoute/no next_hop", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { delete(m, "next_hop") }, "RouteCommon.required"},
		{"ErrorResponse/no error", schemaSource{component: "ErrorResponse"},
			ErrorResponse{Error: ErrorBody{Code: ErrInternal, Message: "x"}},
			func(_ *testing.T, m map[string]any) { delete(m, "error") }, "ErrorResponse.required"},
		{"ErrorResponse/error with no code", schemaSource{component: "ErrorResponse"},
			ErrorResponse{Error: ErrorBody{Code: ErrInternal, Message: "x"}},
			func(t *testing.T, m map[string]any) { delete(nested(t, m, "error"), "code") },
			"the nested required list inside ErrorResponse.error"},
		{"RouteFanout/no unicast", schemaSource{component: "RouteFanout"}, goldenRouteFanout(),
			func(_ *testing.T, m map[string]any) { delete(m, "unicast") }, "RouteFanout.required"},
		{"envelope/no data", schemaSource{responseOf: "/v1/routers"}, routers,
			func(_ *testing.T, m map[string]any) { delete(m, "data") },
			"the envelope's inline required: [data, meta]"},
		{"envelope/no meta", schemaSource{responseOf: "/v1/routers"}, routers,
			func(_ *testing.T, m map[string]any) { delete(m, "meta") },
			"the envelope's inline required: [data, meta]"},

		// Enums. Deleting one stops every one of these from being caught.
		{"Peer/rib", schemaSource{component: "Peer"}, NewWirePeer(goldenPeer()),
			func(_ *testing.T, m map[string]any) { m["rib"] = "not_a_rib" }, "Peer.rib.enum"},
		{"Peer/state", schemaSource{component: "Peer"}, NewWirePeer(goldenPeer()),
			func(_ *testing.T, m map[string]any) { m["state"] = "flapping" }, "Peer.state.enum"},
		{"Peer/dump_states value", schemaSource{component: "Peer"}, NewWirePeer(goldenPeer()),
			func(_ *testing.T, m map[string]any) { m["dump_states"] = map[string]any{"ipv4u": "maybe"} },
			"the additionalProperties enum on Peer.dump_states"},
		{"UnicastRoute/rib", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["rib"] = "not_a_rib" }, "RouteCommon.rib.enum"},
		{"UnicastRoute/family", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["family"] = "ipv4mcast" }, "UnicastRoute.family.enum"},
		{"UnicastRoute/dump_state", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["dump_state"] = "partial" },
			"RouteCommon.dump_state.enum"},
		{"VPNRoute/family", schemaSource{component: "VPNRoute"}, NewWireVPNRoute(goldenVPNRoute()),
			func(_ *testing.T, m map[string]any) { m["family"] = "vpn8" }, "VPNRoute.family.enum"},
		{"HistoryEvent/action", schemaSource{component: "HistoryEvent"},
			NewWireHistoryEvent(goldenHistoryEvent()),
			func(_ *testing.T, m map[string]any) { m["action"] = "flap" }, "HistoryEvent.action.enum"},
		{"Meta/warning code", schemaSource{component: "Meta"},
			Meta{Warnings: Warnings{{Code: WarnSessionDumping, Message: "x"}}},
			func(t *testing.T, m map[string]any) { firstElem(t, m, "warnings")["code"] = "boom" },
			"the enum on Meta.warnings.items.code"},
		{"ErrorResponse/code", schemaSource{component: "ErrorResponse"},
			ErrorResponse{Error: ErrorBody{Code: ErrInternal, Message: "x"}},
			func(t *testing.T, m map[string]any) { nested(t, m, "error")["code"] = "boom" },
			"the enum on ErrorResponse.error.code"},

		// Bounds. route_type is the only bounded field in the contract, and
		// its bound is IANA's EVPN route type registry.
		{"EVPNRoute/route_type below minimum", schemaSource{component: "EVPNRoute"},
			NewWireEVPNRoute(goldenEVPNRoute()),
			func(_ *testing.T, m map[string]any) { m["route_type"] = 0 }, "EVPNRoute.route_type.minimum"},
		{"EVPNRoute/route_type above maximum", schemaSource{component: "EVPNRoute"},
			NewWireEVPNRoute(goldenEVPNRoute()),
			func(_ *testing.T, m map[string]any) { m["route_type"] = 12 }, "EVPNRoute.route_type.maximum"},

		// Types. Erasing one to {} makes every value legal, including the
		// wrong one.
		{"Router/session_id as a number", schemaSource{component: "Router"},
			NewWireRouter(goldenRouter()),
			func(_ *testing.T, m map[string]any) { m["session_id"] = 1774329600123456789 },
			"session_id being typed string (the u64-as-string rule)"},
		{"Router/peers_up as a string", schemaSource{component: "Router"},
			NewWireRouter(goldenRouter()),
			func(_ *testing.T, m map[string]any) { m["peers_up"] = "three" }, "Router.peers_up.type"},
		{"Router/last_seen as a number", schemaSource{component: "Router"},
			NewWireRouter(goldenRouter()),
			func(_ *testing.T, m map[string]any) { m["last_seen"] = 5 }, "Router.last_seen.type"},
		{"Peer/asn as a string", schemaSource{component: "Peer"}, NewWirePeer(goldenPeer()),
			func(_ *testing.T, m map[string]any) { m["asn"] = "65001" }, "Peer.asn.type"},
		{"UnicastRoute/path_id as a string", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["path_id"] = "7" }, "RouteCommon.path_id.type"},
		{"UnicastRoute/as_path not an array", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["as_path"] = "65001" }, "RouteCommon.as_path.type"},
		{"UnicastRoute/as_path of strings", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["as_path"] = []any{"65001"} },
			"the integer items of RouteCommon.as_path"},
		{"UnicastRoute/communities of integers", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["communities"] = []any{1} },
			"the string items of RouteCommon.communities"},
		{"EVPNRoute/labels of strings", schemaSource{component: "EVPNRoute"},
			NewWireEVPNRoute(goldenEVPNRoute()),
			func(_ *testing.T, m map[string]any) { m["labels"] = []any{"24001"} },
			"the integer items of EVPNRoute.labels"},
		{"HistoryEvent/seq as a number", schemaSource{component: "HistoryEvent"},
			NewWireHistoryEvent(goldenHistoryEvent()),
			func(_ *testing.T, m map[string]any) { m["seq"] = 12345 },
			"seq being typed string (the u64-as-string rule)"},
		{"RouteFanout/unicast not an array", schemaSource{component: "RouteFanout"}, goldenRouteFanout(),
			func(_ *testing.T, m map[string]any) { m["unicast"] = map[string]any{} },
			"RouteFanout.unicast.type"},
		{"envelope/data not an array", schemaSource{responseOf: "/v1/routers"}, routers,
			func(_ *testing.T, m map[string]any) { m["data"] = map[string]any{} },
			"the envelope's data being typed array"},
		{"envelope/data element missing a field", schemaSource{responseOf: "/v1/routers"}, routers,
			func(t *testing.T, m map[string]any) { delete(firstElem(t, m, "data"), "sysname") },
			"the envelope's items ref reaching the Router schema"},

		// Nullability, in the direction the positive test cannot reach: a
		// field the contract does NOT allow to be null.
		{"Router/sysname null", schemaSource{component: "Router"}, NewWireRouter(goldenRouter()),
			func(_ *testing.T, m map[string]any) { m["sysname"] = nil },
			"Router.sysname being type string rather than [string, \"null\"]"},
		{"Peer/dump_states null", schemaSource{component: "Peer"}, NewWirePeer(goldenPeer()),
			func(_ *testing.T, m map[string]any) { m["dump_states"] = nil },
			"Peer.dump_states being non-nullable"},
		{"UnicastRoute/prefix null", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["prefix"] = nil }, "UnicastRoute.prefix being non-nullable"},
		{"UnicastRoute/communities null", schemaSource{component: "UnicastRoute"},
			NewWireUnicastRoute(goldenUnicastRoute()),
			func(_ *testing.T, m map[string]any) { m["communities"] = nil },
			"RouteCommon.communities being typed array with no null form"},
		{"Meta/warnings null", schemaSource{component: "Meta"}, Meta{},
			func(_ *testing.T, m map[string]any) { m["warnings"] = nil },
			"Meta.warnings being typed array with no null form"},
		{"Meta/next_cursor as a number", schemaSource{component: "Meta"}, Meta{},
			func(_ *testing.T, m map[string]any) { m["next_cursor"] = 5 },
			"Meta.next_cursor being typed [string, \"null\"]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := tc.source.resolve(t, doc)
			v := jsonOf(t, tc.base)
			// The fixture has to start legal, or "rejected" would prove
			// nothing about the edit under test.
			if err := schema.VisitJSON(v); err != nil {
				t.Fatalf("the unmutated fixture already fails its schema: %v", err)
			}
			tc.mutate(t, v)
			if err := schema.VisitJSON(v); err == nil {
				b, _ := json.Marshal(v)
				t.Errorf("%s accepted an illegal value, so %s is no longer in the "+
					"contract (or is no longer being enforced):\n\t%s",
					tc.source, tc.guards, b)
			}
		})
	}
}

// TestTheContractRejectsAnUnscopedLSCursor is TestTheContractRejectsWhatItForbids'
// same guarantee for a rule that table cannot express. Every case there
// mutates a synthetic golden into an illegal one and checks the SCHEMA
// notices; "a cursor without router= and peer=" is not a schema violation
// on any response BODY at all -- it is a business rule the query layer
// enforces (see query.LSNodesPage's own doc comment) that the handler
// surfaces as a 400, so proving the contract rejects it means driving a
// real request through the real router rather than mutating a fixture. It
// lives beside that test rather than inside its table for that reason: the
// regression this guards against is a handler that starts silently
// accepting the cursor and answering 200, which no golden mutation can
// express.
func TestTheContractRejectsAnUnscopedLSCursor(t *testing.T) {
	doc := loadContractDoc(t)
	s := newTestServer(t)

	// The cursor only has to decode -- it never reaches query, because the
	// handler's own scope check runs first. Its scope names no fixture row
	// at all, and does not need to: the check that rejects it never gets
	// far enough to ask the database anything.
	cur := encodeCursor(query.RIBCursor{
		Collector: "x",
		SessionID: 1,
		Router:    netip.MustParseAddr("10.0.0.1"),
		Peer:      netip.MustParseAddr("10.0.0.2"),
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/ls/nodes?cursor="+cur, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /v1/ls/nodes?cursor=... (no router=, no peer=) = %d, want %d; "+
			"body %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode body: %v; body %s", err, rec.Body.String())
	}
	schema := (schemaSource{component: "ErrorResponse"}).resolve(t, doc)
	if err := schema.VisitJSON(v); err != nil {
		t.Errorf("the real 400 body does not satisfy the contract's own "+
			"ErrorResponse schema: %v\n\t%s", err, rec.Body.Bytes())
	}

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode ErrorResponse: %v", err)
	}
	if body.Error.Code != ErrInvalidParam {
		t.Errorf("error code = %q, want %q", body.Error.Code, ErrInvalidParam)
	}
	lower := strings.ToLower(body.Error.Message)
	for _, want := range []string{"router", "peer"} {
		if !strings.Contains(lower, want) {
			t.Errorf("error message %q does not mention %q", body.Error.Message, want)
		}
	}
}

// collectEnums walks the WHOLE document and returns every closed value set
// in it, keyed by where it lives.
//
// Enumerating rather than looking up named locations is the point, and it
// is the same correction the envelope goldens needed: a table that names
// what to check freezes exactly what someone remembered to name, and a set
// added later is unfrozen with nothing failing. Measured -- the two inline
// `family` query parameters on /v1/routes/unicast and /v1/routes/vpn were
// enums 13 and 14 in a document whose freeze table named 12, and widening
// either one survived.
//
// A schema reached by $ref is skipped: it is a component, frozen where it
// is defined, and following it would report one set under several names.
// const is collected alongside enum because it is the same kind of claim --
// a closed set of one -- and the WWW-Authenticate header is one.
func collectEnums(t *testing.T, doc *openapi3.T) (map[string][]string, map[string]string) {
	t.Helper()
	out := map[string][]string{}
	bounds := map[string]string{}
	var walk func(where string, ref *openapi3.SchemaRef)
	walk = func(where string, ref *openapi3.SchemaRef) {
		if ref == nil || ref.Ref != "" || ref.Value == nil {
			return
		}
		s := ref.Value
		if len(s.Enum) > 0 {
			out[where] = enumStrings(t, where, s.Enum)
		}
		if s.Const != nil {
			out[where+".const"] = enumStrings(t, where+".const", []any{s.Const})
		}
		if b := boundOf(s); b != "" {
			bounds[where] = b
		}
		for name, prop := range s.Properties {
			walk(where+"."+name, prop)
		}
		walk(where+".items", s.Items)
		if s.AdditionalProperties.Schema != nil {
			walk(where+".additionalProperties", s.AdditionalProperties.Schema)
		}
		for i, member := range s.AllOf {
			walk(fmt.Sprintf("%s.allOf[%d]", where, i), member)
		}
	}
	for name, ref := range doc.Components.Schemas {
		walk(name, ref)
	}
	for name, param := range doc.Components.Parameters {
		if param.Ref == "" && param.Value != nil {
			walk("parameters."+name, param.Value.Schema)
		}
	}
	for name, resp := range doc.Components.Responses {
		if resp.Ref != "" || resp.Value == nil {
			continue
		}
		for header, h := range resp.Value.Headers {
			if h.Ref == "" && h.Value != nil {
				walk("responses."+name+".headers."+header, h.Value.Schema)
			}
		}
		for mime, mt := range resp.Value.Content {
			walk("responses."+name+" "+mime, mt.Schema)
		}
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			for _, param := range op.Parameters {
				if param.Ref != "" || param.Value == nil {
					continue
				}
				walk(fmt.Sprintf("%s %s.parameters.%s", method, path, param.Value.Name),
					param.Value.Schema)
			}
			if op.Responses == nil {
				continue
			}
			for status, resp := range op.Responses.Map() {
				if resp.Ref != "" || resp.Value == nil {
					continue
				}
				for mime, mt := range resp.Value.Content {
					walk(fmt.Sprintf("%s %s %s %s", method, path, status, mime), mt.Schema)
				}
			}
		}
	}
	return out, bounds
}

// boundOf renders a schema's numeric range and default as one comparable
// string, or "" where it constrains neither. They travel together because
// they are the same kind of promise about a value the caller supplies, and
// because limit carries all three.
func boundOf(s *openapi3.Schema) string {
	if s.Min == nil && s.Max == nil && s.Default == nil &&
		s.MinItems == 0 && s.MaxItems == nil {
		return ""
	}
	part := func(name string, v any) string {
		if v == nil {
			return ""
		}
		if f, ok := v.(*float64); ok {
			if f == nil {
				return ""
			}
			return fmt.Sprintf(" %s=%v", name, *f)
		}
		return fmt.Sprintf(" %s=%v", name, v)
	}
	// An array's LENGTH bounds are a different axis from its items' value
	// range, and asnBatch is the contract's only place carrying both: its
	// items are 0..4294967295 while the array itself is 1..512. They are
	// named distinctly so one can never be read as the other -- the item
	// range lands at "parameters.asnBatch.items", these at
	// "parameters.asnBatch".
	out := part("min", s.Min) + part("max", s.Max) + part("default", s.Default)
	if s.MinItems != 0 {
		out += fmt.Sprintf(" minItems=%d", s.MinItems)
	}
	if s.MaxItems != nil {
		out += fmt.Sprintf(" maxItems=%d", *s.MaxItems)
	}
	return strings.TrimSpace(out)
}

func enumStrings(t *testing.T, where string, values []any) []string {
	t.Helper()
	out := make([]string, 0, len(values))
	for _, v := range values {
		str, ok := v.(string)
		if !ok {
			t.Fatalf("%s has a non-string member %v (%T)", where, v, v)
		}
		out = append(out, str)
	}
	sort.Strings(out)
	return out
}

func sorted(v ...string) []string {
	out := slices.Clone(v)
	sort.Strings(out)
	return out
}

// wwwAuthenticateTheRouterSends returns the header value api/auth.go
// actually puts on a 401, so the contract's const can be checked against the
// daemon rather than against a second copy of the word "Bearer".
func wwwAuthenticateTheRouterSends(t *testing.T) string {
	t.Helper()
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/routers", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected a 401 to read WWW-Authenticate from, got %d", rec.Code)
	}
	return rec.Header().Get("WWW-Authenticate")
}

// TestContractEnumsDoNotQuietlyWiden is the one relaxation a negative
// fixture cannot catch, and it now runs over every closed set in the
// document rather than a remembered list of them.
//
// TestTheContractRejectsWhatItForbids kills an enum that was DELETED -- its
// illegal value starts being accepted. It does not kill an enum that gained
// a member, because the fixture's illegal value is still illegal. And a
// widened enum is a real change: every set below mirrors a closed set
// somewhere else in this stack, so a new member is either a change the
// decoder and query layer have to make too, or a value no code can produce
// and no client can act on.
//
// Where the set exists in Go it is DERIVED, never copied:
//
//   - The family tokens come through subjects.FamilyToken, named by
//     bgp.Family (an AFI/SAFI pair, which no rename can move), exactly as
//     query/filters.go's unicastFamilies and vpnFamilies do. Its comment
//     spends fifteen lines on why, and names the occurrence that already
//     bit this repo: sink writes subjects.FamilyToken(f) into every route
//     row, query derives its filters from the same registry, so a token
//     renamed in subjects moves BOTH ends silently -- and a hard-coded copy
//     here would keep the contract, this list and every test green while
//     responses carried a family the document does not describe.
//
//   - WWW-Authenticate is read off an actual 401 from the router.
//
//   - rib is pinned rather than derived, and that is not an oversight: as
//     query/rib.go's ribMembers explains, there is no registry behind the
//     rib names -- they are RFC 8671's O and L flags crossed, plus RFC
//     9069's Loc-RIB -- and the Go-side set that exists is unexported in a
//     package this one must not import. query pins it to the shipped DDL
//     instead; this pins it to the same five names.
//
// The two code enums name api/types.go's constants, which buys one specific
// thing and not the thing it looks like: a renamed VALUE fails on both
// sides at once, because the contract and this list would disagree. It does
// NOT catch a constant that was added and never documented -- which
// constants exist is still written out here by hand, and Go has no way to
// ask a package for its exported constants at runtime.
//
// A failure here is not necessarily a bug. It is a contract change that has
// to be deliberate, and the entry is updated in the same commit.
func TestContractEnumsDoNotQuietlyWiden(t *testing.T) {
	doc := loadContractDoc(t)
	ribValues := sorted("in_pre", "in_post", "out_pre", "out_post", "loc_rib")
	unicastFamilies := sorted(
		subjects.FamilyToken(bgp.FamilyIPv4U),
		subjects.FamilyToken(bgp.Family{AFI: 2, SAFI: 1}), // ipv6u
	)
	vpnFamilies := sorted(
		subjects.FamilyToken(bgp.FamilyVPNv4),
		subjects.FamilyToken(bgp.Family{AFI: 2, SAFI: 128}), // vpn6
		subjects.FamilyToken(bgp.FamilyLU4),
	)
	frozen := map[string][]string{
		"Peer.rib":                                 ribValues,
		"RouteCommon.rib":                          ribValues,
		"HistoryEvent.rib":                         ribValues,
		"parameters.rib":                           ribValues,
		"Peer.state":                               sorted("unspecified", "up", "down", "view_lost", query.PeerStateStale),
		"Peer.dump_states.additionalProperties":    sorted("complete", "dumping"),
		"RouteCommon.dump_state":                   sorted("complete", "dumping", "unknown"),
		"HistoryEvent.action":                      sorted("announce", "withdraw"),
		"PeerEvent.rib":                            ribValues,
		"PeerEvent.kind":                           sorted("unspecified", "up", "down", "view_lost"),
		"UnicastRoute.allOf[1].family":             unicastFamilies,
		"VPNRoute.allOf[1].family":                 vpnFamilies,
		"GET /v1/routes/unicast.parameters.family": unicastFamilies,
		"GET /v1/routes/vpn.parameters.family":     vpnFamilies,
		// The link-state schemas' own copies of rib and dump_state -- LSCommon
		// and LSLink each declare the field directly rather than through
		// RouteCommon, so the walk sees a new "where" for the same closed set
		// RouteCommon.rib and RouteCommon.dump_state already freeze above.
		"LSCommon.rib":        ribValues,
		"LSLink.rib":          ribValues,
		"LSCommon.dump_state": sorted("complete", "dumping", "unknown"),
		"LSLink.dump_state":   sorted("complete", "dumping", "unknown"),
		// The two additional closed sets below. label_source names api/openapi.yaml's
		// own three-tier naming scheme (observer, fleet, identifier); those
		// three strings exist only as SQL literals inside query/'s
		// lsLabelSourceExpr, with no Go declaration, so this is pinned as a
		// literal rather than derived. state= is the opposite case: it is
		// query.LSState's own closed set (query/lsfilters.go), so it is
		// derived from query.LSStateLive, query.LSStateWithdrawn and
		// query.LSStateAny rather than copied -- a rename of any one of them
		// moves the handler and this frozen list together, instead of
		// leaving the contract silently accepting a token the handler no
		// longer does.
		"LSEndpoint.label_source": sorted("observer", "fleet", "identifier"),
		"parameters.lsState": sorted(
			string(query.LSStateLive), string(query.LSStateWithdrawn), string(query.LSStateAny)),
		// Derived from the role names api/types.go declares, so renaming one
		// moves NewWireASNode and this list together rather than leaving the
		// contract advertising a role nothing emits.
		"ASNode.roles.items": sorted(roleOrigin, roleTransit, roleObservedPeer),
		"Meta.warnings.items.code": sorted(
			WarnSessionDumping, WarnPaginatedSmear, WarnTruncated, WarnCollectorStale),
		"ErrorResponse.error.code": sorted(
			ErrInvalidParam, ErrUnauthorized, ErrSessionChanged, ErrInternal),
		"responses.Unauthorized.headers.WWW-Authenticate.const": sorted(
			wwwAuthenticateTheRouterSends(t)),
		// Derived from the AuthMode constants api/config.go declares, so a
		// renamed or added mode moves this list and the code together
		// instead of the contract silently advertising one LoadConfig does
		// not accept, or vice versa.
		"GET /v1/auth/config 200 application/json.mode": sorted(
			string(AuthModeToken), string(AuthModeOIDC), string(AuthModeNone)),
	}

	found, _ := collectEnums(t, doc)
	if len(found) == 0 {
		t.Fatal("no closed value set was found in the contract -- the walk is wrong, " +
			"not the document")
	}
	for where, got := range found {
		want, ok := frozen[where]
		if !ok {
			// The completeness half, and the reason this walks rather than
			// looks up. A set nothing freezes can be widened, narrowed or
			// deleted with the whole package green.
			t.Errorf("%s is a closed value set (%v) that nothing in this test freezes. "+
				"Add it -- derive the members if anything in Go declares them, and say "+
				"why not if nothing does", where, got)
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s is %v, expected %v. Changing a closed set is a contract change: "+
				"the members mirror a closed set elsewhere in this stack, so a new one is "+
				"either a change the decoder and query layer have to make too, or a value "+
				"nothing can produce. If it is deliberate, update this entry in the same "+
				"commit", where, got, want)
		}
	}
	for where := range frozen {
		if _, ok := found[where]; !ok {
			t.Errorf("%s is frozen here but the contract no longer has a closed set "+
				"there -- either it was deleted (which is a relaxation) or it moved",
				where)
		}
	}
}

// TestContractBoundsDoNotQuietlyRelax is the enum test's other half.
//
// A deleted minimum or maximum is the same relaxation as a deleted enum,
// and TestTheContractRejectsWhatItForbids only kills it where a negative
// fixture exists -- which means only on route_type, the one bounded field
// that appears in a RESPONSE. The two bounded query parameters and every
// documented default were unreachable: nothing read path-level parameters
// at all until collectEnums started walking them. Measured: deleting
// limit's minimum and maximum, and the 1..11 on ?type=, both survived.
//
// Derived where anything declares these values:
//
//   - limit's maximum and default are LoadConfig's built-in max_page and
//     default_page. A daemon started with no overrides is the deployment
//     the contract describes, so changing either built-in without changing
//     the document makes the documented default wrong for every
//     out-of-the-box install. (An operator who sets max_page explicitly is
//     a separate question, and belongs with parameter validation elsewhere.)
//   - ?type='s bound is asserted equal to EVPNRoute.route_type's rather
//     than to a literal. The contract's own comment on that parameter is
//     that a narrower bound here would refuse the query that finds a route
//     the response schema still has to be able to carry, so the two moving
//     together is the actual invariant.
//
// 1..11 itself is frozen, not derived: it is IANA's EVPN Route Types
// registry, which nothing in this repo declares.
func TestContractBoundsDoNotQuietlyRelax(t *testing.T) {
	doc := loadContractDoc(t)
	cfg, err := LoadConfig(writeTempYAML(t, "tokens:\n  - name: t\n    token: 0123456789abcdef\n"))
	if err != nil {
		t.Fatalf("LoadConfig with built-in defaults: %v", err)
	}
	_, found := collectEnums(t, doc)
	if len(found) == 0 {
		t.Fatal("the contract carries no bounds or defaults at all -- the walk is wrong")
	}
	evpnType := found["EVPNRoute.allOf[1].route_type"]
	if evpnType == "" {
		t.Fatal("EVPNRoute.route_type has no bound; it is the contract's only bounded " +
			"response field and the ?type= parameter is checked against it")
	}
	// originASN and throughASN's bound (1..4294967295) is RFC 7607's -- AS 0
	// is reserved and is refused at the parameter, not clamped like limit's
	// ceiling -- and it is frozen as a literal for the same reason evpnType's
	// bound is derived rather than pinned by hand: nothing in Go declares the
	// AS number range the way EVPNRoute.route_type declares its own.
	const asnBound = "min=1 max=4.294967295e+09"
	// lsASNBound is area's and lsASN's shared range on the link-state paths.
	// It is NOT asnBound: origin_asn and through_asn refuse 0 because RFC
	// 7607 reserves it for route origination, but an IGP area or a
	// link-state AS descriptor genuinely stores 0 (the OSPF backbone; a node
	// with no AS descriptor) -- see their own descriptions in
	// api/openapi.yaml. Frozen as a literal for the same reason asnBound is:
	// nothing in Go declares this range independently.
	const lsASNBound = "min=0 max=4.294967295e+09"
	// protocolIDBound covers every field mirroring a raw wire byte with no
	// registry narrower than a uint8's own range: LSCommon.protocol and
	// LSLink.protocol (BGP-LS Protocol-ID, RFC 7752), LSPrefix's
	// ospf_route_type and prefix_sid_flags, and LSNode's sr_algorithms
	// items (the IGP Algorithm Types registry).
	const protocolIDBound = "min=0 max=255"
	frozen := map[string]string{
		"EVPNRoute.allOf[1].route_type":       "min=1 max=11",
		"GET /v1/routes/evpn.parameters.type": evpnType,
		"parameters.limit": fmt.Sprintf("min=1 max=%d default=%d",
			cfg.MaxPage, cfg.DefaultPage),
		"GET /v1/routes/history.parameters.since": "default=1h",
		"GET /v1/events.parameters.since":         "default=1h",
		// collectionSince is shared by all four /v1/collection/* paths (see
		// its own doc comment in api/openapi.yaml for why one component
		// rather than four near-identical inlined copies), so it is keyed
		// once here rather than once per path the way the two inlined
		// since= parameters above are.
		"parameters.collectionSince": "default=1h",
		// The churn bucket's default is the width every bar is computed at
		// when a caller names none, so it is a claim the contract makes about
		// what a chart MEANS -- frozen for the same reason the window
		// defaults above are.
		"GET /v1/collection/churn.parameters.bucket": "default=5m",
		"parameters.originASN":                       asnBound,
		"parameters.throughASN":                      asnBound,
		"parameters.area":                            lsASNBound,
		"parameters.lsASN":                           lsASNBound,
		// asnBatch's own items share area's and lsASN's range rather than
		// originASN's/throughASN's, and for a related but distinct reason:
		// this parameter has no "unset" value of its own for 0 to collide
		// with (absence is naming asn= zero times, not the value 0), so
		// there is no RFC 7607 ambiguity for refusing 0 to resolve. See
		// this parameter's own description in api/openapi.yaml.
		"parameters.asnBatch.items": lsASNBound,
		// The ARRAY's own length bounds, as opposed to its items' range
		// above. maxItems is DERIVED from the daemon's own constant rather
		// than retyped, the way parameters.limit derives from cfg.MaxPage:
		// api/asnames.go refuses a batch past maxASNBatch with a 400, and
		// until this entry existed the contract could be widened to 1024
		// while the daemon went on refusing at 512 with this whole package
		// green -- verified by mutating the contract and watching nothing
		// fail. minItems stays a literal because nothing in Go declares it:
		// "at least one asn= is required" is enforced by asns() returning an
		// error for an empty batch, not by a named constant.
		"parameters.asnBatch":                 fmt.Sprintf("minItems=1 maxItems=%d", maxASNBatch),
		"parameters.lsState":                  "default=live",
		"LSCommon.protocol":                   protocolIDBound,
		"LSLink.protocol":                     protocolIDBound,
		"LSPrefix.allOf[1].ospf_route_type":   protocolIDBound,
		"LSPrefix.allOf[1].prefix_sid_flags":  protocolIDBound,
		"LSNode.allOf[1].sr_algorithms.items": protocolIDBound,
	}
	for where, got := range found {
		want, ok := frozen[where]
		if !ok {
			t.Errorf("%s constrains a value (%s) and nothing in this test freezes it. "+
				"A bound or default that nothing pins can be widened or deleted with the "+
				"whole package green", where, got)
			continue
		}
		if got != want {
			t.Errorf("%s is %q, expected %q. Relaxing a bound is a contract change: it "+
				"widens what a caller may send without anything in the daemon agreeing "+
				"to accept it", where, got, want)
		}
	}
	for where := range frozen {
		if _, ok := found[where]; !ok {
			t.Errorf("%s is frozen here but the contract no longer constrains it -- "+
				"either the bound was deleted, which is the relaxation this test exists "+
				"for, or it moved", where)
		}
	}
}

// The cap appears a THIRD time, as prose: asnBatch's description says "more
// than 512 is a 400 naming that bound", and the description is what a client
// developer actually reads before writing a batching loop. The frozen bound
// above pins the machine-readable half and says nothing about this one, so a
// cap that moved could leave the prose quietly promising the old number --
// the same class of drift as a stale comment, except this one ships to
// callers as the contract.
func TestTheASNBatchDescriptionNamesTheRealCap(t *testing.T) {
	doc := loadContractDoc(t)
	param, ok := doc.Components.Parameters["asnBatch"]
	if !ok || param.Value == nil {
		t.Fatal("the contract has no asnBatch parameter component; GET /v1/asnames " +
			"is where the batch cap is documented and this test cannot see it")
	}
	desc := param.Value.Description
	// Without this an empty description would satisfy nothing and fail for a
	// reason that reads like drift, and a description that lost its prose
	// entirely would be reported as "does not name the cap" rather than "has
	// no prose at all".
	if strings.TrimSpace(desc) == "" {
		t.Fatal("asnBatch carries no description at all, so there is no prose for this " +
			"test to check and a client developer has nothing to read")
	}
	if !strings.Contains(desc, strconv.Itoa(maxASNBatch)) {
		t.Errorf("asnBatch's description does not name the cap the daemon actually "+
			"enforces (%d). api/asnames.go refuses a longer batch with a 400 naming "+
			"that number, so prose naming a different one sends callers to build "+
			"batches the daemon will reject:\n\t%s", maxASNBatch, desc)
	}
}

// The golden fixtures. Each populated variant sets every field to a
// non-zero, non-empty value, including the ones a real row often leaves
// empty: that is what makes a field the contract does not document, or one
// whose tag no longer matches, visible.
//
// The values are shaped like a real deployment's -- 10.0.103.x, a named
// collector, AS 65000-range -- so that a failure prints something
// recognizable rather than a wall of "a"/1.

// theGoldenSessionID is past 2^53, which is what makes the u64-as-string
// rule observable at all. It is the same magnitude a collector actually
// assigns (now().UnixNano()).
const theGoldenSessionID = uint64(1774329600123456789)

func goldenTime() time.Time { return time.Date(2026, 8, 24, 15, 4, 5, 0, time.UTC) }

func goldenRouter() query.Router {
	return query.Router{
		SysName:   "ce1.lab",
		IP:        netip.MustParseAddr("10.0.103.61"),
		Collector: "cml-1",
		SessionID: theGoldenSessionID,
		PeersUp:   3,
		PeersDown: 1,
		LastSeen:  goldenTime(),
	}
}

// sparseRouter is genuinely all-zero: Router has no enum-constrained or
// bounded field, so nothing has to be filled in to keep it legal.
func sparseRouter() query.Router { return query.Router{} }

func goldenPeer() query.Peer {
	return query.Peer{
		RouterIP:   netip.MustParseAddr("10.0.103.61"),
		PeerIP:     netip.MustParseAddr("10.0.103.62"),
		Collector:  "cml-1",
		RIB:        "in_post",
		ASN:        65001,
		State:      "up",
		DumpStates: map[string]string{"ipv4u": "complete", "vpn4": "dumping"},
		SessionID:  theGoldenSessionID,
		Routes:     420,
		// The session facts, POPULATED -- which is the whole job of the
		// populated variant. Left at their zero values this golden would
		// validate a null hold_time and two empty arrays and prove nothing
		// about the encoding of a real one, while looking like coverage.
		// sparsePeer below is the other side: a down peer, no OPEN, null.
		HoldTime:        180,
		HoldTimeSeen:    true,
		MPFamilies:      []string{"ipv4u", "vpn4"},
		AddPathFamilies: []string{"ipv4u"},
		SysDescr:        "Cisco IOS XR Software, Version 24.1.1",
	}
}

// sparsePeer is a DOWN peer, which is the real shape this variant exists
// for: a down peer's DumpStates is empty by design (its dump progress is
// not a meaningful question), and the contract documents {} there as a
// positive claim where null would read as "unknown".
func sparsePeer() query.Peer {
	return query.Peer{RIB: "in_pre", State: "down"}
}

func goldenUnicastRoute() query.Route {
	med, localPref := uint32(50), uint32(100)
	return query.Route{
		RouterSysName:    "ce1.lab",
		RouterIP:         netip.MustParseAddr("10.0.103.61"),
		PeerIP:           netip.MustParseAddr("10.0.103.62"),
		NextHop:          netip.MustParseAddr("10.0.103.62"),
		Collector:        "cml-1",
		RIB:              "in_post",
		Family:           "ipv4u",
		Prefix:           "10.77.0.0/24",
		PathID:           7,
		ASPath:           []uint32{65001, 65002},
		OriginASN:        65002,
		MED:              &med,
		LocalPref:        &localPref,
		Communities:      []string{"65000:100"},
		LargeCommunities: []string{"65000:1:2"},
		DumpState:        "complete",
	}
}

// sparseUnicastRoute is the iBGP-shaped row: no AS path, so origin_asn is
// null, and no MED, local pref, communities or next hop. dump_state is
// "unknown", which is a real value of that enum rather than a stand-in.
func sparseUnicastRoute() query.Route {
	return query.Route{RIB: "in_pre", Family: "ipv4u", DumpState: "unknown"}
}

func goldenVPNRoute() query.VPNRoute {
	med, localPref := uint32(50), uint32(100)
	return query.VPNRoute{
		RouterSysName:    "ce1.lab",
		RouterIP:         netip.MustParseAddr("10.0.103.61"),
		PeerIP:           netip.MustParseAddr("10.0.103.62"),
		NextHop:          netip.MustParseAddr("10.0.103.62"),
		Collector:        "cml-1",
		RIB:              "in_post",
		Family:           "vpn4",
		RD:               "65000:1",
		Prefix:           "10.77.0.0/24",
		PathID:           7,
		Label:            24001,
		HasLabel:         true,
		RouteTargets:     []string{"65000:1"},
		ASPath:           []uint32{65001, 65002},
		OriginASN:        65002,
		MED:              &med,
		LocalPref:        &localPref,
		Communities:      []string{"65000:100"},
		LargeCommunities: []string{"65000:1:2"},
		DumpState:        "complete",
	}
}

// sparseVPNRoute is lu4, the family for which an empty RD and empty route
// targets are the nature of the thing rather than missing data -- and with
// HasLabel false, so label is null rather than a defaulted 0. Label 0 is
// the real implicit-null label, which is why the two must differ on the
// wire; TestLabelZeroSurvivesMarshaling pins the other side of that.
func sparseVPNRoute() query.VPNRoute {
	return query.VPNRoute{RIB: "in_pre", Family: "lu4", DumpState: "unknown"}
}

func goldenEVPNRoute() query.EVPNRoute {
	med, localPref := uint32(50), uint32(100)
	return query.EVPNRoute{
		RouterSysName:    "ce1.lab",
		RouterIP:         netip.MustParseAddr("10.0.103.61"),
		PeerIP:           netip.MustParseAddr("10.0.103.62"),
		NextHop:          netip.MustParseAddr("10.0.103.62"),
		Collector:        "cml-1",
		RIB:              "in_post",
		RouteType:        2,
		RD:               "65000:1",
		Prefix:           "10.77.0.0/24",
		MAC:              "00:1b:21:aa:bb:cc",
		IP:               "10.77.0.33",
		GatewayIP:        "10.77.0.1",
		EthernetTag:      100,
		ESI:              "00:11:22:33:44:55:66:77:88:99",
		PathID:           7,
		Labels:           []uint32{24001, 24002},
		RouteTargets:     []string{"65000:1"},
		ASPath:           []uint32{65001, 65002},
		OriginASN:        65002,
		MED:              &med,
		LocalPref:        &localPref,
		Communities:      []string{"65000:100"},
		LargeCommunities: []string{"65000:1:2"},
		DumpState:        "complete",
	}
}

// sparseEVPNRoute is a type-3 (IMET) route, where "" for prefix, mac and
// gateway_ip is what the route carries rather than what was lost. route_type
// is 3 and not 0 because the contract bounds it to IANA's registry (1-11)
// and the decoder sets it from the wire on every route, so 0 is a value
// neither end can produce.
func sparseEVPNRoute() query.EVPNRoute {
	return query.EVPNRoute{RIB: "in_pre", RouteType: 3, DumpState: "unknown"}
}

func goldenHistoryEvent() query.HistoryEvent {
	return query.HistoryEvent{
		TsCollector: goldenTime(),
		TsRouter:    goldenTime().Add(-time.Second),
		Seq:         18446744073709551615,
		SessionID:   theGoldenSessionID,
		RouterIP:    netip.MustParseAddr("10.0.103.61"),
		PeerIP:      netip.MustParseAddr("10.0.103.62"),
		Collector:   "cml-1",
		RIB:         "in_post",
		Family:      "ipv4u",
		Prefix:      "10.77.0.0/24",
		PathID:      7,
		Action:      "announce",
		NextHop:     netip.MustParseAddr("10.0.103.62"),
		ASPath:      []uint32{65001, 65002},
	}
}

// sparseHistoryEvent is a withdrawal, which carries no path attributes at
// all: next_hop is null and as_path is empty, and on this surface that is
// the ORDINARY event rather than the exceptional one.
func sparseHistoryEvent() query.HistoryEvent {
	return query.HistoryEvent{RIB: "in_pre", Action: "withdraw"}
}

// goldenPeerEvent is a "down" whose reason IS in peerDownReasons, so the
// populated variant also exercises reasonName's positive case -- reason_name
// carries a real registry name beside the number, the same way
// theGoldenLSProtocol exercises query.ProtocolName's.
func goldenPeerEvent() query.PeerEvent {
	return query.PeerEvent{
		TsCollector:   goldenTime(),
		TsRouter:      goldenTime().Add(-time.Second),
		StreamSeq:     18446744073709551615,
		Seq:           18446744073709551614,
		SessionID:     theGoldenSessionID,
		RouterIP:      netip.MustParseAddr("10.0.103.61"),
		RouterSysname: "ce1.lab",
		PeerIP:        netip.MustParseAddr("10.0.103.62"),
		PeerASN:       65001,
		Collector:     "cml-1",
		RIB:           "in_post",
		Kind:          "down",
		DownReason:    3, // remote system closed, notification follows
		LocalIP:       netip.MustParseAddr("10.0.103.61"),
		LocalPort:     179,
		RemotePort:    50000,
	}
}

// sparsePeerEvent is a view_lost event, the shape whose down_reason is
// genuinely 0 because the router said nothing -- not because a value was
// omitted -- so reason_name is "" for the same reason. rib and kind still
// carry a legal enum member, exactly as every other sparse fixture here
// does for its own bounded fields.
func sparsePeerEvent() query.PeerEvent {
	return query.PeerEvent{RIB: "in_pre", Kind: "view_lost"}
}

// theGoldenLSProtocol is a real BGP-LS Protocol-ID (RFC 9552 section 5.1
// Table 2) rather than an arbitrary byte, so the golden also exercises
// query.ProtocolName's positive case -- protocol_name carries "isis-l2"
// beside the number -- the same way TestProtocolNameTravelsWithItsNumber
// does, but folded into the shape every other field is checked alongside.
const theGoldenLSProtocol = uint8(2)

func goldenLSNode() query.LSNode {
	return query.LSNode{
		RouterSysName: "p1.lab",
		RouterIP:      netip.MustParseAddr("10.0.103.61"),
		PeerIP:        netip.MustParseAddr("10.0.103.63"),
		Collector:     "cml-1",
		RIB:           "in_post",
		Protocol:      theGoldenLSProtocol,
		Identifier:    100,
		ASN:           65001,
		BGPLSID:       1,
		Area:          490001,
		RouterID:      "0a0000f1",
		NodeKey:       8792346123987552,
		RouterIDv4:    "10.0.0.1",
		Name:          "p1.lab",
		SRGBBase:      16000,
		SRGBSize:      8000,
		SRLBBase:      15000,
		SRLBSize:      1000,
		SRAlgorithms:  []uint8{0, 1},
		DumpState:     "complete",
	}
}

// sparseLSNode is the zero-value shape query/ never actually produces (see
// LSEndpoint.Label's own doc comment on why a real row's label is never
// empty) -- but that is exactly the point: the sparse variant is what
// notices a field this package marshals as null or omits where the
// contract types it as a plain, always-present value, and the zero value
// exercises that more thoroughly than a hand-picked "mostly empty" fixture
// would. Only rib and dump_state are set, to a legal member of each enum;
// "" is not a value either can take, so leaving them zero would test a
// shape the query layer can never hand this package.
func sparseLSNode() query.LSNode {
	return query.LSNode{RIB: "in_pre", DumpState: "unknown"}
}

func goldenLSLink() query.LSLink {
	return query.LSLink{
		RouterSysName: "p1.lab",
		RouterIP:      netip.MustParseAddr("10.0.103.61"),
		PeerIP:        netip.MustParseAddr("10.0.103.63"),
		Collector:     "cml-1",
		RIB:           "in_post",
		Protocol:      theGoldenLSProtocol,
		Identifier:    100,
		Local: query.LSEndpoint{
			ASN:         65001,
			BGPLSID:     1,
			Area:        490001,
			RouterID:    "0a0000f1",
			NodeKey:     8792346123987552,
			IfAddr:      "10.0.12.1",
			InterfaceID: 5,
			Label:       "p1.lab",
			LabelSource: "observer",
		},
		Remote: query.LSEndpoint{
			ASN:         65002,
			BGPLSID:     2,
			Area:        490002,
			RouterID:    "0a0000f2",
			NodeKey:     8792346123987553,
			IfAddr:      "10.0.12.2",
			InterfaceID: 6,
			Label:       "p2.lab",
			LabelSource: "fleet",
		},
		AdjSIDs:      []uint32{24001, 24002},
		TEMetric:     100,
		IGPMetric:    10,
		AdminGroup:   1,
		MaxBandwidth: 1000000000,
		DumpState:    "complete",
	}
}

// sparseLSLink is LSLink's zero value plus a legal member of every
// enum-constrained field: rib, dump_state, and -- unlike LSNode and
// LSPrefix -- label_source on BOTH endpoints, since LSEndpoint.label_source
// is itself an enum with no "" member. "identifier" is the tier that means
// "nobody named this node," which is the honest reading of an endpoint
// whose RouterID is also "".
func sparseLSLink() query.LSLink {
	return query.LSLink{
		RIB:       "in_pre",
		DumpState: "unknown",
		Local:     query.LSEndpoint{LabelSource: "identifier"},
		Remote:    query.LSEndpoint{LabelSource: "identifier"},
	}
}

func goldenLSPrefix() query.LSPrefix {
	return query.LSPrefix{
		RouterSysName:  "p1.lab",
		RouterIP:       netip.MustParseAddr("10.0.103.61"),
		PeerIP:         netip.MustParseAddr("10.0.103.63"),
		Collector:      "cml-1",
		RIB:            "in_post",
		Protocol:       theGoldenLSProtocol,
		Identifier:     100,
		ASN:            65001,
		BGPLSID:        1,
		Area:           490001,
		RouterID:       "0a0000f1",
		NodeKey:        8792346123987552,
		Prefix:         "10.77.7.0/24",
		PrefixSID:      16001,
		PrefixSIDFlags: 0x60,
		HasPrefixSID:   true,
		PrefixMetric:   10,
		OSPFRouteType:  3,
		DumpState:      "complete",
	}
}

// sparseLSPrefix is LSNode's own reasoning applied to LSPrefix: only rib
// and dump_state are set. HasPrefixSID stays false, which is the ordinary
// case -- most prefixes carry no Prefix-SID TLV -- and is why
// WireLSPrefix.HasPrefixSID exists at all rather than folding into
// prefix_sid the way VPNRoute.Label does with HasLabel; prefix_sid_flags
// disambiguates the SID's meaning, but only once has_prefix_sid says one is
// present.
func sparseLSPrefix() query.LSPrefix {
	return query.LSPrefix{RIB: "in_pre", DumpState: "unknown"}
}

// limitedPathsExemptFromTotalMatched is TestEveryLimitedPathDocumentsTotalMatched's
// short, explicitly-justified exemption list, keyed by path. Every entry
// here is a path that declares limit= yet genuinely has no total_matched to
// report, not an oversight papered over.
//
// /v1/rib/{unicast,vpn,evpn} are the only three: each is ribPage's own
// PAGINATED cursor walk, pinned to one (router, peer) BMP session, and
// limit= there is a page size, not a cap on an unpaginated answer -- see
// ribPage's own doc comment (api/handlers.go). Completeness is signaled by
// meta.next_cursor being null (the walk is exhausted), never by a count,
// and ribPage sets no TotalMatched at all -- there is no "N matched, K
// returned" question a keyset walk answers, only "is there a next page".
// That is structurally different from /v1/events and the three /v1/ls
// paths, which LOOK similar (limit=, a cursor=) but carry a SECOND,
// unpaginated mode for an unscoped request -- and it is exactly that second
// mode's own 200 description that mentions total_matched for those three,
// which is why they need no exemption here.
var limitedPathsExemptFromTotalMatched = map[string]string{
	"/v1/rib/unicast": "ribPage cursor walk, no unpaginated mode -- see this var's own doc comment",
	"/v1/rib/vpn":     "ribPage cursor walk, no unpaginated mode -- see this var's own doc comment",
	"/v1/rib/evpn":    "ribPage cursor walk, no unpaginated mode -- see this var's own doc comment",
}

// TestEveryLimitedPathDocumentsTotalMatched closes a gap found by hand on
// 2026-09-11: /v1/collection/{dumps,sessions,locrib,flags} each
// declared limit=, applied a real LIMIT in query/collection.go, and left
// meta.total_matched permanently null -- the exact "a partial answer reads
// as complete" failure this project's contract exists hardest to prevent,
// and it sat one layer below where TestResponsesValidateAgainstTheContract
// or TestEveryDocumentedPropertyIsRequired could see it: null is a LEGAL
// value of total_matched's own `[integer, "null"]` type (Meta's own
// component schema), so nothing TYPE-level distinguishes "this endpoint
// cannot count what LIMIT truncated" from "it could, and simply does not."
// Hand-checking every limit=-taking path's handler found the four missing
// cases; this test does that check itself, against
// the CONTRACT rather than the Go source, so the same gap cannot reappear
// on a ninth limit=-taking path unnoticed.
//
// The check is textual, against each path's own GET 200 description,
// because that is the only place this document can assert a RUNTIME
// behavior (does the handler actually fill total_matched) rather than a
// SHAPE (does the field's type merely permit null) -- the same reasoning
// TestTheContractDocumentsBothEventsModes already applies to /v1/events' own
// two-mode behavior, immediately below. A path that declares limit= and
// never says "total_matched" anywhere in its description is either an
// oversight -- what this test's own mutation proves it catches -- or
// belongs on limitedPathsExemptFromTotalMatched with a reason; there is no
// third, silent option.
func TestEveryLimitedPathDocumentsTotalMatched(t *testing.T) {
	doc := loadContractDoc(t)
	checked := 0
	for path, item := range doc.Paths.Map() {
		op := item.Get
		if op == nil || op.Parameters.GetByInAndName("query", "limit") == nil {
			continue
		}
		checked++
		if reason, exempt := limitedPathsExemptFromTotalMatched[path]; exempt {
			t.Logf("%s exempt: %s", path, reason)
			continue
		}
		resp := op.Responses.Status(200)
		if resp == nil || resp.Value == nil || resp.Value.Description == nil {
			t.Errorf("%s declares limit= but has no GET 200 description to check", path)
			continue
		}
		if !strings.Contains(*resp.Value.Description, "total_matched") {
			t.Errorf("%s declares limit= (a real LIMIT-backed cap, per its own contract) "+
				"but its 200 description never mentions total_matched. Either the handler "+
				"cannot count what LIMIT truncates and this path belongs on "+
				"limitedPathsExemptFromTotalMatched with a reason, or meta.total_matched "+
				"is silently left null the way /v1/collection/* four all did before this "+
				"test existed -- the exact defect this test is here to catch", path)
		}
	}
	if checked == 0 {
		t.Fatal("no path declaring limit= was found -- the walk is wrong, not the contract")
	}
}

// TestTheContractDocumentsBothEventsModes. The contract is what a client
// generates from, so a second mode that exists only in Go is a mode no
// generated client can call: hey-api emits router/peer as REQUIRED
// properties on findPeerEvents' query type from routerRequired/peerRequired,
// and TypeScript then refuses the unscoped call the daemon now serves.
func TestTheContractDocumentsBothEventsModes(t *testing.T) {
	doc := loadContractDoc(t)
	item := doc.Paths.Find("/v1/events")
	if item == nil || item.Get == nil {
		t.Fatal("contract has no GET /v1/events")
	}
	params := item.Get.Parameters
	for _, name := range []string{"router", "peer"} {
		p := params.GetByInAndName("query", name)
		if p == nil {
			t.Fatalf("/v1/events has no %q query parameter", name)
		}
		if p.Required {
			t.Errorf("%s is still required on /v1/events; the unscoped mode cannot "+
				"be expressed by a client generated from this contract", name)
		}
	}
	resp := item.Get.Responses.Status(200)
	if resp == nil || resp.Value == nil || resp.Value.Description == nil {
		t.Fatal("/v1/events has no GET 200 description")
	}
	desc := *resp.Value.Description
	for _, must := range []string{"max_unscoped_since", "total_matched", "half"} {
		if !strings.Contains(desc, must) {
			t.Errorf("the 200 description does not mention %q", must)
		}
	}
	// These are negative assertions: each substring below was true of this
	// path before the unscoped mode existed and is false of it now, so its
	// return is a regression the positive assertions above cannot catch --
	// they only check that the current truths are PRESENT, and a future
	// edit could paste an old falsehood back in beside them without
	// disturbing any of the three. Pin absence, not just presence, so that
	// edit fails here instead of shipping a contract that once again
	// contradicts the handler.
	mustNot := []string{
		// router/peer required, and an unscoped request being a 400 rather
		// than an empty list, both stopped being true when router/peer
		// became optional on this path (Step 3 above).
		"are both required",
		"an unscoped request is a 400 rather",
		// The unqualified claim that this endpoint never clamps since=.
		// The clamp now applies to the unscoped mode; "does not clamp"
		// does not otherwise appear in the scoped-only phrasing this
		// description legitimately kept ("Its since= is not clamped" /
		// "this same window is still not clamped").
		"does not clamp",
	}
	for _, forbidden := range mustNot {
		if strings.Contains(desc, forbidden) {
			t.Errorf("the 200 description still contains %q, a claim that was true "+
				"before the unscoped mode existed and is false now", forbidden)
		}
	}
}

// --- /v1/collection/* goldens ---
//
// None of the four row types carries an enum-constrained or bounded field
// (router_ip and peer_ip are plain strings on the wire, not a format the
// contract restricts; has_stat is a bare boolean), so unlike sparseRouter's
// siblings above that must keep a legal rib/state/family/action, every
// sparse builder here is genuinely the zero value.

func goldenRouterDumpCount() query.RouterDumpCount {
	return query.RouterDumpCount{
		RouterIP:      netip.MustParseAddr("10.0.103.61"),
		RouterSysname: "ce1.lab",
		Archived:      100,
		Dumps:         40,
		Changes:       60,
	}
}

func sparseRouterDumpCount() query.RouterDumpCount { return query.RouterDumpCount{} }

func goldenRouterSessionCount() query.RouterSessionCount {
	return query.RouterSessionCount{
		RouterIP:      netip.MustParseAddr("10.0.103.61"),
		RouterSysname: "ce1.lab",
		Sessions:      9,
		Up:            4,
		Down:          3,
		ViewLost:      2,
	}
}

func sparseRouterSessionCount() query.RouterSessionCount { return query.RouterSessionCount{} }

func goldenPeerLocRIB() query.PeerLocRIB {
	return query.PeerLocRIB{
		RouterIP: netip.MustParseAddr("10.0.103.61"),
		PeerIP:   netip.MustParseAddr("10.0.103.62"),
		Reported: 140,
		Archived: 610,
		HasStat:  true,
	}
}

// sparsePeerLocRIB is the shape HasStat exists for: HasStat false alongside
// Reported's own zero value, which is exactly the pair a reader cannot tell
// apart from a genuine "the router reported zero" without the flag -- see
// PeerLocRIB's own doc comment.
func sparsePeerLocRIB() query.PeerLocRIB {
	return query.PeerLocRIB{
		RouterIP: netip.MustParseAddr("10.0.103.61"),
		PeerIP:   netip.MustParseAddr("10.0.103.63"),
		Archived: 3,
	}
}

func goldenFlagCount() query.FlagCount {
	return query.FlagCount{Flag: "PARSE_FLAG_VERSION_UNPARSED", Envelopes: 58}
}

func sparseFlagCount() query.FlagCount { return query.FlagCount{} }

// goldenChurnBucket is one bar with all three series non-zero, which is what
// makes it a golden rather than a sample: a bucket carrying only dumps
// validates against a contract that had dropped either of the other two
// fields.
func goldenChurnBucket() query.ChurnBucket {
	return query.ChurnBucket{
		TS:          time.Date(2026, 9, 17, 14, 55, 0, 0, time.UTC),
		Readvertise: 412,
		Withdraw:    54,
		Dump:        1204,
	}
}

// sparseChurnBucket is a bucket that held nothing. It is a real answer --
// this endpoint returns only buckets the window had rows in, so an all-zero
// bar can still be produced by a caller filtering the series -- and the
// zero TS exercises the timestamp encoding on the empty side.
func sparseChurnBucket() query.ChurnBucket { return query.ChurnBucket{} }

func goldenPeerChurn() query.PeerChurn {
	return query.PeerChurn{
		RouterIP:      netip.MustParseAddr("10.0.103.62"),
		RouterSysname: "xr-pe1",
		PeerIP:        netip.MustParseAddr("10.2.0.2"),
		PeerASN:       65001,
		Readvertise:   412,
		Withdraw:      54,
		Dump:          1204,
	}
}

// sparsePeerChurn is the row a real fleet produces most of: a peer that sat
// there. Its zero sysname is the "router sent no sysName TLV" case the
// collection paths all carry, and its zero ASN exercises the encoding on the
// side a populated row cannot.
func sparsePeerChurn() query.PeerChurn { return query.PeerChurn{} }

func goldenPrefixChurn() query.PrefixChurn {
	return query.PrefixChurn{
		Prefix:       "10.10.1.0/24",
		Observations: 1670,
		Readvertise:  412,
		Withdraw:     54,
		Dump:         1204,
		Routes:       13,
		Sessions:     4,
	}
}

// sparsePrefixChurn deliberately keeps a NON-EMPTY prefix while zeroing
// everything else. An empty prefix is not a sparse row on this path, it is
// an excluded one -- undecoded NLRI never reaches the answer at all -- so a
// golden carrying "" here would assert the opposite of what the contract
// says.
func sparsePrefixChurn() query.PrefixChurn { return query.PrefixChurn{Prefix: "0.0.0.0/0"} }
