// Router mechanics: the parts of api/server.go that are not contract
// parity. The two parity tests live in api/openapi_test.go, which is where
// anything that reads api/openapi.yaml belongs.
package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// testToken is the credential every test in this file presents. It carries
// config_test.go's sentinel so that a response body or log line that ever
// echoed it would be findable by grepping the suite's output for one
// string, the same way the auth tests use it.
const testToken = "server-" + tokenSentinel

// newTestServer builds a Server whose token list is valid by construction,
// so an error out of NewServer is a bug in the test rather than the case
// under test. The cases that are ABOUT the error call NewServer directly.
//
// It carries a real *query.Q, and did not always: while every data route
// was a 501 stub, q was nil and the routing table could be tested with no
// database at all. That changed once every stub got a real handler. Two
// tests here drive a request all the way through the mux to prove it was
// ROUTED -- reaching a handler is
// the assertion -- and a real handler's first act is to call query, so a
// nil one is a panic rather than a status. The alternative was a nil check
// inside every handler, which would add a branch to production code to
// serve a Server no production path can build; requiring the database is
// the honest cost of testing a server that has handlers.
//
// The consequence is that these tests SKIP without ClickHouse rather than
// running, which is why the source-level guarantee is the one that matters
// most here: TestNoRouteIsRegisteredOutsideTheTable parses this package
// rather than serving from it, and still runs with no database. Any test
// that does not need a handler to ANSWER should use newTestServerNoDB
// below instead, and every auth-boundary test in this package now does.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithToken(t, testToken)
}

// newTestServerNoDB builds the same Server with a nil *query.Q, for tests
// whose subject is the gate rather than an answer.
//
// The difference from newTestServer is not tidiness, it is whether the test
// runs at all. newTestServer goes through chtest.Require, which SKIPS (not
// fails) when ClickHouse is unreachable unless VANTAGE_REQUIRE_CLICKHOUSE
// is set -- so on a developer's machine `go test ./...` printed "ok" for
// this package with every auth-boundary assertion on this branch silently
// dropped, and CI was safe only because ci.yml sets that one variable.
//
// None of the tests built on this helper needs a database, and that is a
// property of what they assert rather than an accident: an unauthenticated
// request is refused by gate() before the mux is ever consulted,
// handleAuthConfig reads only s.cfg.Auth.Mode, serveOpenAPI writes the
// embedded contract, and a non-/v1 path never enters the API at all.
// TestAuthConfigReportsNoneMode in api/handlers_test.go made this argument
// first, for one test; this is that argument extracted so the rest of the
// boundary can share it.
//
// The page sizes match newTestServerWithToken's so that moving a test
// between the two helpers changes only where its rows come from.
func newTestServerNoDB(t *testing.T, token string) *Server {
	t.Helper()
	s, err := NewServer(nil, Config{
		DefaultPage: 1000,
		MaxPage:     10000,
		Tokens:      []Token{{Name: "test", Token: secret.NewAPIToken(token)}},
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// serveWithoutCredential drives one unauthenticated request through srv and
// returns the status, and it exists to turn one specific failure into a
// readable one.
//
// A server from newTestServerNoDB carries a nil *query.Q, so any data
// handler it reaches panics on its first query call. That panic is not
// noise: reaching a handler at all is precisely the thing an
// unauthenticated request must never do, so recovering it and naming it is
// a better report than a process-killing stack trace that takes the rest of
// the package's results with it.
func serveWithoutCredential(t *testing.T, srv *Server, method, path string) int {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s %s reached a handler with no credential -- it panicked on the "+
				"test server's nil database (%v), which means the gate let it through",
				method, path, r)
		}
	}()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

// newTestServerWithToken is newTestServer with the token value supplied by
// the caller rather than fixed, so a test that needs to know the token it
// authenticates with does not have to duplicate the construction.
func newTestServerWithToken(t *testing.T, token string) *Server {
	t.Helper()
	conn := chtest.Require(t, t.Context(), apiTestDB)
	q, err := query.New(conn, apiTestDB)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	s, err := NewServer(q, Config{
		DefaultPage: 1000,
		MaxPage:     10000,
		Tokens:      []Token{{Name: "test", Token: secret.NewAPIToken(token)}},
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// TestNewServerPropagatesTheTokenListRefusal is why NewServer returns an
// error at all. newAuth is the first code to look at token VALUES, and two
// entries sharing one token authenticate identically while differing only
// in which name reaches the log -- decided by config order rather than by
// the operator. If NewServer swallowed that, the daemon would start with an
// audit trail that confidently names the wrong caller.
func TestNewServerPropagatesTheTokenListRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens []Token
		want   string
	}{
		{
			name: "duplicate token value",
			tokens: []Token{
				{Name: "grafana", Token: secret.NewAPIToken("shared-" + tokenSentinel)},
				{Name: "cron", Token: secret.NewAPIToken("shared-" + tokenSentinel)},
			},
			want: "identical",
		},
		{
			// This Config never goes through LoadConfig, so Auth.Mode is
			// the zero value -- which NewServer's switch treats as token
			// mode, not AuthModeNone. A Server built that way with no
			// tokens would 401 every request while looking healthy.
			name:   "no tokens at all",
			tokens: nil,
			want:   "no tokens configured",
		},
		{
			name:   "empty token value",
			tokens: []Token{{Name: "grafana", Token: secret.NewAPIToken("")}},
			want:   "is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(nil, Config{Tokens: tc.tokens}, nil)
			if err == nil {
				t.Fatal("NewServer accepted a token list newAuth refuses")
			}
			if s != nil {
				t.Errorf("NewServer returned a usable Server alongside its error: %#v", s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain the refusal; got %q, want a mention of %q",
					err, tc.want)
			}
			if strings.Contains(err.Error(), tokenSentinel) {
				t.Errorf("the error echoed a configured token: %q", err)
			}
		})
	}
}

// TestOnlyNoneModeOpensTheGate pins NewServer's switch, which is the code
// that DECIDES whether this daemon authenticates anything.
//
// Exactly one mode may hand gate() a pass-through middleware, and the
// switch says so with a single `case AuthModeNone:` over a default that
// builds the token gate. Nothing held it there: widening that case to
// `case AuthModeNone, AuthModeOIDC:` -- making a mode the daemon's own
// error message calls "not implemented" serve every route open -- left the
// whole Go suite green.
//
// LoadConfig rejects oidc and any unknown mode before NewServer sees them,
// so this is unreachable through a config file today. It is not unreachable
// through the API: NewServer is exported, takes a Config by value, and is
// the only place the decision is made. A mode this table does not name
// must land in the default branch and be authenticated, and the assertion
// is written that way round -- over the modes that exist plus one that
// does not -- so a future AuthMode constant added without a thought about
// the gate is the case this test was built for.
//
// The auth/config probe is not decoration. Without it a NewServer that
// refused every request, or a gate that 401d a public route, would satisfy
// the 401 above and look correct.
func TestOnlyNoneModeOpensTheGate(t *testing.T) {
	for _, mode := range []AuthMode{
		AuthModeToken,
		AuthModeOIDC,
		// Neither of these can come out of LoadConfig. Both can come out
		// of a caller: the zero value is what every Config built in code
		// without LoadConfig carries, and "saml" stands for the next
		// mode name someone types.
		AuthMode(""),
		AuthMode("saml"),
	} {
		t.Run(string(mode), func(t *testing.T) {
			srv, err := NewServer(nil, Config{
				Auth:   AuthConfig{Mode: mode},
				Tokens: []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
			}, nil)
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			if code := serveWithoutCredential(t, srv, http.MethodGet, "/v1/routers"); code != http.StatusUnauthorized {
				t.Errorf("auth.mode %q served GET /v1/routers with no credential (status %d); "+
					"only %q may open the gate", mode, code, AuthModeNone)
			}
			// The counterweight: the server is gating, not merely broken.
			if code := serveWithoutCredential(t, srv, http.MethodGet, "/v1/auth/config"); code != http.StatusOK {
				t.Errorf("auth.mode %q returned %d for the public GET /v1/auth/config; "+
					"the gate is refusing routes the contract exempts", mode, code)
			}
		})
	}

	// And none really is open, so the assertions above are about the mode
	// and not about a gate that can never be opened at all.
	srv, err := NewServer(nil, Config{Auth: AuthConfig{Mode: AuthModeNone}}, nil)
	if err != nil {
		t.Fatalf("NewServer(none): %v", err)
	}
	rec := httptest.NewRecorder()
	// A path inside /v1 that the mux does not route: it proves the request
	// got past the gate (401 would mean it did not) without reaching a
	// handler, which on this nil-database Server would panic.
	//
	// The Host is loopback because none mode refuses any other with 421,
	// and a 421 would satisfy "not 401" without the gate being consulted.
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1/nothing-here", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("auth.mode none answered %d for an unrouted /v1 path, want 404 "+
			"from the mux; 401 would mean the mode does nothing", rec.Code)
	}
}

// TestAuthenticatedRequestReachesItsHandler is the pass-through half of the
// auth wiring. TestAuthGateMatchesTheContractsSecurity checks that an
// unauthenticated request is refused on every gated path; a middleware that
// refused EVERY request would satisfy that and serve nothing.
//
// The assertion is only that the request reached a handler, which is why
// neither a 200 nor a 400 is checked for: most of these paths have required
// parameters this probe does not send, so a well-routed request answers 400.
// 401 and 404 are the two codes that mean it never arrived.
func TestAuthenticatedRequestReachesItsHandler(t *testing.T) {
	s := newTestServer(t)
	for _, p := range s.routePatterns() {
		method, path, _ := strings.Cut(p, " ")
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("a valid token was refused on %s", path)
			}
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s is in the routing table but the mux 404s it", path)
			}
		})
	}
}

// TestOpenAPIEndpointServesTheEmbeddedContract checks the bytes, not just
// the status: the point of embedding is that a running daemon describes
// itself with the document its code was built against, so serving anything
// other than openapiYAML byte for byte defeats it.
func TestOpenAPIEndpointServesTheEmbeddedContract(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	// No Authorization header, because api/openapi.yaml gives this
	// operation its own security: []. That claim is checked against the
	// contract in TestAuthGateMatchesTheContractsSecurity; here it is only
	// the way the request is made.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/openapi.yaml returned %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/yaml" {
		t.Errorf("Content-Type is %q, want application/yaml (RFC 9512)", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), openapiYAML) {
		t.Errorf("the served document is not the embedded one (%d bytes served, %d embedded)",
			rec.Body.Len(), len(openapiYAML))
	}
	// And it is a document, not just matching bytes: an embed pointed at
	// the wrong file would pass the comparison above, since both sides read
	// the same variable.
	if _, err := openapi3.NewLoader().LoadFromData(rec.Body.Bytes()); err != nil {
		t.Errorf("the served document does not parse as OpenAPI: %v", err)
	}
}

// TestWrongMethodIsRejectedByTheMux is what the method half of each pattern
// buys. Registered as a bare path, every route would answer a POST with
// whatever the handler does rather than the 405 ServeMux gives for free.
func TestWrongMethodIsRejectedByTheMux(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/routers", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/routers returned %d, want 405", rec.Code)
	}
}

// TestGateAuthenticatesEverythingNotExplicitlyPublic is the property that
// made gate() worth having, and it is the one no parity test can reach: a
// route registered on the mux without going through routes() is invisible
// to routePatterns(), so nothing can assert it is documented -- but it must
// still not be an open door.
//
// The mux here is built by hand precisely because NewServer cannot produce
// this shape. That is the point: the test asks what happens to a
// registration the routing table never saw.
func TestGateAuthenticatesEverythingNotExplicitlyPublic(t *testing.T) {
	auth, err := newAuth([]Token{{Name: "test", Token: secret.NewAPIToken(testToken)}})
	if err != nil {
		t.Fatalf("newAuth: %v", err)
	}
	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stray", func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := gate(map[string]bool{"GET /v1/openapi.yaml": true}, auth, mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/stray", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a route registered outside the table answered %d without a token; "+
			"gate must authenticate everything it does not explicitly exempt", rec.Code)
	}
	if reached {
		t.Error("the stray handler ran for an unauthenticated request")
	}

	// The exemption is exact, and every near miss falls into the
	// authenticated branch rather than out of it. These are the shapes a
	// path-matching gate is usually broken by.
	for _, target := range []string{
		"/v1/openapi.yaml/",
		"/v1/openapi.yaml/../stray",
		"//v1/openapi.yaml",
		"/v1/OPENAPI.yaml",
	} {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%q was treated as the public path (got %d, want 401)", target, rec.Code)
			}
		})
	}
	// And a method the contract does not exempt is not exempt either.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/openapi.yaml", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST to the public path answered %d; the exemption is on the GET "+
			"operation only", rec.Code)
	}
}

// TestUnknownPathIs404 is the floor under the path parity test: that test
// reads a 404 as "this documented path is not routed", which only means
// anything if an unrouted path really does 404.
func TestUnknownPathIs404(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/nothing-here", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("an unrouted path returned %d, want 404", rec.Code)
	}
}

// TestNoRouteIsRegisteredOutsideTheTable is a SOURCE-level test, and it
// exists because no runtime test can do this job.
//
// http.ServeMux exposes no way to enumerate what was registered on it, so
// routePatterns() -- derived from routes() -- is the only view of the mux
// any test has, and by construction it cannot see a registration the table
// never made. Verified by mutation: adding a mux.HandleFunc after the
// registration loop leaves every other test in this package green, so the
// route is served, undocumented, and invisible to contract parity. gate()
// keeps such a route authenticated, which is why this was a defensible gap
// rather than an open door -- but "authenticated and undocumented" is still
// an endpoint nobody agreed to.
//
// So this reads the package's own source instead of its behavior: every
// Handle/HandleFunc call in a non-test file must sit inside the range loop
// in NewServer. That is the rule api/server.go's header states, and a
// comment is a weaker guard than a parser.
//
// If a future change legitimately restructures registration -- extracting a
// registerRoutes helper, say -- this test fails and has to be updated
// deliberately. That is the intended cost: moving where routes are
// registered is exactly the change that deserves a second look.
func TestNoRouteIsRegisteredOutsideTheTable(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no non-test source files parsed -- the walk is wrong, not the package")
	}

	// The one place registration is allowed: the body of a range statement
	// inside NewServer.
	type span struct{ lo, hi token.Pos }
	var allowed []span
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "NewServer" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if rng, ok := n.(*ast.RangeStmt); ok {
					allowed = append(allowed, span{rng.Body.Lbrace, rng.Body.Rbrace})
				}
				return true
			})
		}
	}
	if len(allowed) == 0 {
		t.Fatal("found no range loop in NewServer -- either the routing table stopped " +
			"being registered in a loop, or this test can no longer find it. Both need " +
			"a human")
	}

	found := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			found++
			for _, a := range allowed {
				if call.Pos() > a.lo && call.Pos() < a.hi {
					return true
				}
			}
			t.Errorf("%s: %s is called outside NewServer's loop over routes(). Every "+
				"route must come from the routing table -- a registration made anywhere "+
				"else is served but invisible to routePatterns(), so no contract parity "+
				"test can see that it is undocumented",
				fset.Position(call.Pos()), sel.Sel.Name)
			return true
		})
	}
	if found == 0 {
		t.Fatal("found no Handle/HandleFunc call at all -- this test is not looking at " +
			"the code it thinks it is")
	}
}

// TestUIFallbackDoesNotBypassTheGate is the security-relevant half of the
// UI wiring. The UI is served outside the auth gate, so the wrapper's path
// check is the only thing keeping /v1 authenticated. If it ever matched
// loosely, every route in the contract would be readable without a token.
//
// The bare "/v1" and the doubled-slash "//v1/routers" are here because a
// plain HasPrefix(path, "/v1/") misses both: neither has "/v1/" as a
// literal prefix, so both fell through to the UI handler and answered 200
// or 503 instead of reaching the gate. Every registered route today
// carries the trailing slash, so nothing is exposed by that gap yet -- but
// a future route registered at the bare /v1 would be shadowed by the UI
// permanently, with nothing to say so.
//
// It builds its server with newTestServerNoDB: nothing here is meant to
// reach a handler, so requiring a database would only mean this assertion
// disappears on any machine without one.
func TestUIFallbackDoesNotBypassTheGate(t *testing.T) {
	srv := newTestServerNoDB(t, testToken)
	for _, path := range []string{
		"/v1/routers", "/v1/peers", "/v1/does-not-exist",
		"/v1", "//v1/routers",
	} {
		if code := serveWithoutCredential(t, srv, http.MethodGet, path); code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token: status = %d, want 401", path, code)
		}
	}
}

// TestUIServesWithoutAToken is the other half: the app itself must load
// before anyone can authenticate, so non-/v1 paths are public by design.
func TestUIServesWithoutAToken(t *testing.T) {
	srv := newTestServerNoDB(t, testToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("GET / returned 401; the UI must load before a token exists")
	}
}

// serveHost drives one request with an explicit Host header and returns the
// recorder. httptest.NewRequest's own default Host is "example.com", which
// is exactly the kind of name the none-mode Host check refuses, so every
// test that means to reach a none-mode Server has to say which host it is.
func serveHost(t *testing.T, srv *Server, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestNoneModeRefusesAForeignHost is the DNS-rebinding defense. With no
// token to present, the only thing that tells this daemon's own UI apart
// from an attacker's page that has rebound its name to this listener's
// address is the Host header the browser sends, which carries the
// attacker's name. Reproduced directly: Host: rebind.attacker.example
// answered 200 -- that is the first case below.
//
// Both /v1/auth/config (public, so the refusal is not the gate's 401) and
// the UI are probed: the check sits in front of both.
func TestNoneModeRefusesAForeignHost(t *testing.T) {
	srv, err := NewServer(nil, Config{
		Auth:         AuthConfig{Mode: AuthModeNone},
		AllowedHosts: []string{"vantage.corp.example", "fd00::5"},
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	for _, host := range []string{
		"rebind.attacker.example:9473",
		"rebind.attacker.example",
		"vantage.corp.example.attacker.example",
		"127.0.0.1.attacker.example",
		"localhost.attacker.example:9473",
		"",
	} {
		for _, path := range []string{"/v1/auth/config", "/"} {
			if rec := serveHost(t, srv, host, path); rec.Code != http.StatusMisdirectedRequest {
				t.Errorf("none mode, Host %q, GET %s: status %d, want 421", host, path, rec.Code)
			}
		}
	}
	// The counterweight: the loopback literals and the configured names are
	// still served, however the Host is spelled, so the refusals above are
	// the check and not a server that refuses everything.
	for _, host := range []string{
		"127.0.0.1:9473", "127.0.0.1", "localhost:9473", "LOCALHOST", "localhost.",
		"[::1]:9473", "[::1]",
		"vantage.corp.example", "Vantage.Corp.Example.:443", "[fd00::5]:9473",
	} {
		if rec := serveHost(t, srv, host, "/v1/auth/config"); rec.Code != http.StatusOK {
			t.Errorf("none mode, Host %q, GET /v1/auth/config: status %d, want 200", host, rec.Code)
		}
	}
}

// TestTokenModeDoesNotCheckTheHost: a rebinding page has no token, so in
// token mode the Host check would buy nothing and would break every
// deployment reached by a name nobody listed. Only none mode checks it.
func TestTokenModeDoesNotCheckTheHost(t *testing.T) {
	srv := newTestServerNoDB(t, testToken)
	if rec := serveHost(t, srv, "rebind.attacker.example:9473", "/v1/auth/config"); rec.Code != http.StatusOK {
		t.Errorf("token mode, foreign Host, GET /v1/auth/config: status %d, want 200", rec.Code)
	}
	if rec := serveHost(t, srv, "rebind.attacker.example:9473", "/v1/routers"); rec.Code != http.StatusUnauthorized {
		t.Errorf("token mode, foreign Host, GET /v1/routers with no token: status %d, want 401", rec.Code)
	}
}

// TestSecurityHeadersOnEveryResponse checks the one middleware that sets
// them against each kind of response this daemon writes: the UI, a public
// API route, the gate's 401, a 404 from the mux, and the none-mode Host
// refusal. Any of those written by a path that bypassed the middleware
// would be the one a browser frames or sniffs.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	want := map[string]string{
		"Content-Security-Policy": contentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
	}
	// The policy itself, pinned by value as well as by the constant above:
	// a constant edited to "" would otherwise satisfy the comparison.
	for _, directive := range []string{"default-src 'self'", "object-src 'none'",
		"base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(contentSecurityPolicy, directive) {
			t.Errorf("Content-Security-Policy %q lacks %q", contentSecurityPolicy, directive)
		}
	}
	if strings.Contains(contentSecurityPolicy, "unsafe") {
		t.Errorf("Content-Security-Policy %q allows an unsafe-* source", contentSecurityPolicy)
	}
	tokenSrv := newTestServerNoDB(t, testToken)
	noneSrv, err := NewServer(nil, Config{Auth: AuthConfig{Mode: AuthModeNone}}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	for _, tc := range []struct {
		name       string
		srv        *Server
		host, path string
		status     int
	}{
		{"ui", tokenSrv, "127.0.0.1", "/", 0},
		{"api route", tokenSrv, "127.0.0.1", "/v1/auth/config", http.StatusOK},
		{"401", tokenSrv, "127.0.0.1", "/v1/routers", http.StatusUnauthorized},
		{"421", noneSrv, "rebind.attacker.example", "/v1/auth/config", http.StatusMisdirectedRequest},
	} {
		rec := serveHost(t, tc.srv, tc.host, tc.path)
		if tc.status != 0 && rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.status)
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s (GET %s, %d): %s = %q, want %q", tc.name, tc.path, rec.Code, k, got, v)
			}
		}
	}
}
