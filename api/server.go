// Server: the mux, and the one place in this daemon where a path becomes a
// route.
//
// The routing table is a VALUE -- routes() -- rather than a sequence of
// mux.Handle calls, and everything else is derived from it: NewServer walks
// it to build the mux, and routePatterns() walks it to tell the contract
// parity test what is served. That is the whole design. A parity test that
// read a hand-maintained list of patterns would be a test of the list, and
// would pass while the mux served something else entirely; here there is no
// second list to disagree with.
//
// The consequence, and the rule this file depends on: NOTHING may call
// mux.Handle outside the loop in NewServer. A registration made anywhere
// else is invisible to routePatterns(), which makes it invisible to the
// test that exists to catch an undocumented endpoint -- there is no
// ServeMux API to enumerate what was registered, so no RUNTIME test can see
// one. The rule is enforced instead by
// TestNoRouteIsRegisteredOutsideTheTable, which parses this package's own
// source and fails on any Handle/HandleFunc call outside that loop. And
// gate() sits in front of the whole mux, so even a route that somehow got
// past both is authenticated rather than open.
//
// The handlers themselves live in api/handlers.go. This file landed before
// that one, with every data route a 501 stub, so that each handler was born
// under a test that fails if its path is not in api/openapi.yaml. The stubs
// are gone; the property they were there to establish is not, and is held
// by the parity tests in api/openapi_test.go.
package api

import (
	_ "embed"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/webui"
)

// openapiYAML is the contract, compiled into the binary and served at
// /v1/openapi.yaml.
//
// It is embedded rather than read from disk at startup because the document
// a running daemon describes itself with has to be the one its code was
// built against. A file path would let an operator (or a container image
// rebuild, or a partially-applied deploy) put a newer contract beside an
// older binary, and the daemon would then hand out a description of
// endpoints it does not serve -- with the parity tests in this package all
// still passing, because they read the file in the source tree.
//
//go:embed openapi.yaml
var openapiYAML []byte

// Server is the HTTP read API. It holds no state beyond its dependencies:
// every request is answered from ClickHouse through q.
type Server struct {
	q      *query.Q
	cfg    Config
	logger *slog.Logger

	// handler is the fully-wrapped mux, built once in NewServer. It is
	// built there rather than per Handler() call so that a caller holding
	// the http.Handler is holding the same routing table the parity test
	// checked, and so that the auth middleware is constructed exactly once
	// -- newAuth reveals every configured token, and doing that per call
	// would spread the credential over more copies for no reason.
	handler http.Handler

	// metrics counts and times every request handler answers; see
	// Metrics.
	metrics *httpMetrics
}

// route is one row of the routing table: the ServeMux pattern, whether the
// contract exempts it from authentication, and what answers it.
type route struct {
	// pattern is a Go 1.22+ method+path ServeMux pattern ("GET /v1/routers").
	// The method belongs in it: every endpoint in the contract is a GET, and
	// a bare path pattern would answer a POST with a 501 rather than the 405
	// ServeMux gives for free.
	pattern string

	// public is true only where api/openapi.yaml gives the operation its
	// own `security: []`, which per OpenAPI overrides the document-level
	// `security: [bearerAuth]` and means "no authentication for this
	// operation". Two operations carry it today -- GET /v1/auth/config and
	// GET /v1/openapi.yaml, both because a caller has to be able to reach
	// them before it can hold a credential at all -- and this field is not
	// a convenience -- it is a claim about the contract, checked against
	// the contract by TestAuthGateMatchesTheContractsSecurity. Setting it
	// on any other row fails that test, and so does clearing it on either
	// of these two; if an operation's security is ever meant to change, the
	// contract is what changes and this table follows.
	//
	// It is an opt-OUT and the zero value gates the route, so a row added
	// without thinking about authentication gets it.
	public bool

	handler http.HandlerFunc
}

// routes is the routing table, and the only place a path is written down.
//
// The rows are in contract order. Every handler started out as a 501
// stub; a row's handler was the only part of it that changed as each one
// was implemented, which is what kept the pattern and the contract in
// step through that work.
func (s *Server) routes() []route {
	return []route{
		{pattern: "GET /v1/routers", handler: s.handleRouters},
		{pattern: "GET /v1/collectors", handler: s.handleCollectors},
		{pattern: "GET /v1/peers", handler: s.handlePeers},
		{pattern: "GET /v1/routes", handler: s.handleRoutes},
		{pattern: "GET /v1/routes/unicast", handler: s.handleUnicastRoutes},
		{pattern: "GET /v1/routes/vpn", handler: s.handleVPNRoutes},
		{pattern: "GET /v1/routes/evpn", handler: s.handleEVPNRoutes},
		{pattern: "GET /v1/routes/history", handler: s.handleHistory},
		{pattern: "GET /v1/topology", handler: s.handleTopology},
		{pattern: "GET /v1/events", handler: s.handleEvents},
		{pattern: "GET /v1/rib/unicast", handler: s.handleRIBUnicast},
		{pattern: "GET /v1/rib/vpn", handler: s.handleRIBVPN},
		{pattern: "GET /v1/rib/evpn", handler: s.handleRIBEVPN},
		{pattern: "GET /v1/ls/nodes", handler: s.handleLSNodes},
		{pattern: "GET /v1/ls/links", handler: s.handleLSLinks},
		{pattern: "GET /v1/ls/prefixes", handler: s.handleLSPrefixes},
		{pattern: "GET /v1/collection/dumps", handler: s.handleCollectionDumps},
		{pattern: "GET /v1/collection/sessions", handler: s.handleCollectionSessions},
		{pattern: "GET /v1/collection/locrib", handler: s.handleCollectionLocRIB},
		{pattern: "GET /v1/collection/flags", handler: s.handleCollectionFlags},
		{pattern: "GET /v1/collection/churn", handler: s.handleCollectionChurn},
		{pattern: "GET /v1/collection/churn/peers", handler: s.handleCollectionChurnPeers},
		{pattern: "GET /v1/collection/churn/prefixes", handler: s.handleCollectionChurnPrefixes},
		{pattern: "GET /v1/asnames", handler: s.handleASNames},
		{pattern: "GET /v1/auth/config", public: true, handler: s.handleAuthConfig},
		{pattern: "GET /v1/openapi.yaml", public: true, handler: s.serveOpenAPI},
	}
}

// NewServer builds the read API over q.
//
// It returns an error because newAuth does, and newAuth does because it is
// the first code that looks at the token VALUES: two config entries sharing
// one token load happily and authenticate identically, differing only in
// which name reaches the log, and that is decided by config order rather
// than by the operator. Propagating rather than swallowing is the point of
// the signature -- cmd/vantage-api cannot start past it, so the daemon
// fails closed on a token list LoadConfig had to accept.
//
// A nil logger is replaced with a discarding one so that a caller with
// nothing to log -- a test, mostly -- does not have to construct one, and
// so that no code path here has to nil-check before logging.
//
// q is not checked for nil. Routing never touches it, and the only caller
// that matters gets its *query.Q from query.New, which already refuses a
// nil connection and hands back an error the compiler forces to be handled;
// a second check here would guard a case that cannot arrive while forcing
// every test of the routing table to carry a database handle.
func NewServer(q *query.Q, cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// The gate is chosen once, here, rather than checked per request: the
	// mode cannot change while the process runs, and a per-request branch
	// would be a second place for "is this authenticated" to be decided.
	var auth func(http.Handler) http.Handler
	switch cfg.Auth.Mode {
	case AuthModeNone:
		logger.Warn("serving unauthenticated: every route is open to anyone who can reach this listener",
			"auth_mode", string(AuthModeNone))
		auth = func(next http.Handler) http.Handler { return next }
	default:
		// The zero value reaches here too. That is deliberate: a Config
		// built in code without going through LoadConfig -- which is every
		// test in this package -- gets the token gate, not the open one.
		a, err := newAuth(cfg.Tokens)
		if err != nil {
			return nil, fmt.Errorf("api: %w", err)
		}
		auth = a
	}
	s := &Server{q: q, cfg: cfg, logger: logger}
	mux := http.NewServeMux()
	// public is keyed by the pattern text itself, which gate compares
	// against the request's method and path. That lines up only for a plain
	// "METHOD /path" pattern -- a host prefix or a {wildcard} segment would
	// never match, and so would never be exempted. That is the safe
	// direction, and every exempt route has to be a fixed path.
	public := map[string]bool{}
	for _, rt := range s.routes() {
		mux.Handle(rt.pattern, rt.handler)
		if rt.public {
			public[rt.pattern] = true
		}
	}
	h := uiFallback(gate(public, auth, mux))
	if cfg.Auth.Mode == AuthModeNone {
		h = hostCheck(cfg.AllowedHosts, h)
	}
	s.metrics = newHTTPMetrics(mux, s.routePatterns())
	s.handler = s.metrics.wrap(securityHeaders(h))
	return s, nil
}

// Metrics returns the request counter and latency histogram for this
// Server's handler, for the caller to register: cmd/vantage-api registers
// it on the registry its /metrics serves.
func (s *Server) Metrics() prometheus.Collector { return s.metrics }

// contentSecurityPolicy is the policy every response carries.
//
// The built UI (webui/dist) is one module script and stylesheets, all
// loaded from this origin by URL: index.html has no inline script or style,
// and Vue applies :style bindings through the CSSOM, which style-src does
// not govern. So neither 'unsafe-inline' nor 'unsafe-eval' is needed.
// img-src and font-src add data: because Vite inlines assets under its
// 4 KiB assetsInlineLimit as data: URIs in the built CSS -- the <select>
// chevron and the smallest font subsets -- and neither kind can run code.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"font-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

// securityHeaders sets the browser-facing hardening headers on every
// response, before anything downstream can write one: the UI, every /v1
// answer, the gate's 401 and hostCheck's 421 alike. It is the outermost
// handler so that no path to a response can skip it.
//
// frame-ancestors and X-Frame-Options say the same thing to new and old
// browsers: no page may frame this one, which closes clickjacking against
// the token prompt.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// hostCheck refuses, with 421 Misdirected Request, any request whose Host
// is neither a loopback literal nor in allowed. It is installed only in
// auth.mode: none; see Config.AllowedHosts for why that mode needs it and
// token mode does not.
//
// The body is plain text rather than the contract's ErrorResponse: the
// request was not addressed to this daemon at all, so it never became an
// API call, and the refusal comes before any route -- UI or /v1 -- is
// chosen.
func hostCheck(allowed []string, next http.Handler) http.Handler {
	ok := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range allowed {
		ok[hostOf(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[hostOf(r.Host)] {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusMisdirectedRequest)
			_, _ = w.Write([]byte("vantage-api: this host name is not in allowed_hosts\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostOf reduces a Host header (or an allowed_hosts entry) to the form the
// two are compared in: port stripped, IPv6 brackets removed, case folded,
// trailing dot dropped. "Localhost.:9473" and "localhost" are one host;
// "localhost.attacker.example" is not.
func hostOf(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// gate puts the auth middleware in FRONT of the mux rather than around each
// handler, and lets through only the exact method-and-path pairs the
// contract exempts.
//
// The difference is which way a mistake falls. Wrapping per handler means a
// route registered outside routes() -- a debug endpoint added in a hurry, a
// merge that lands a mux.Handle in the wrong place -- is served with no
// authentication at all, and no runtime test notices, because there is no
// ServeMux API to enumerate registrations. In front, the same stray route
// is authenticated, because reaching the mux at all requires passing the
// gate. This is defense in depth rather than the primary guard:
// TestNoRouteIsRegisteredOutsideTheTable catches the stray registration at
// the source level. gate() is what makes the consequence of that test ever
// being wrong -- or removed -- an undocumented endpoint rather than one
// that dodges whatever this daemon is configured to require of it. Which
// routes are exempt from a token is the contract's decision, read into the
// public set below; whether ANY route needs a token at all is a separate
// decision, auth.mode's, made once in NewServer and never here -- gate()
// enforces both without knowing which mode produced them, which is why
// none mode does not bypass gate() at all: it hands gate() an auth
// middleware that authenticates nothing, rather than skipping it.
//
// The match is exact string equality against the same pattern text the mux
// was registered with, and the public set has two entries today. There is
// no prefix, no wildcard and no normalization, which is what keeps this
// from being the usual path-matching auth bypass: every way of failing to
// match -- a trailing slash, a %2e, a doubled separator, a different
// method -- lands in the authenticated branch. A path that needs cleaning
// never reaches a handler anyway; ServeMux answers it with a redirect.
func gate(public map[string]bool, auth func(http.Handler) http.Handler, mux http.Handler) http.Handler {
	authenticated := auth(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public[r.Method+" "+r.URL.Path] {
			mux.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}

// uiFallback puts the single-page application in front of the whole API,
// and is the ONLY handler in this daemon that answers without consulting
// the gate.
//
// The split is by path and nothing else: anything naming /v1 is the
// contract and goes to the gated mux, and everything else is the app. That
// direction matters. A request for an unknown /v1 path reaches the gate and
// gets a 401 rather than an index.html, so a typo in an endpoint name can
// never look like a successful page load; and no path outside /v1 can reach
// a handler, so serving the app cannot expose one. isV1Path is what keeps
// that claim true for shapes a plain prefix test would miss -- see its
// comment.
//
// The app is public on purpose. It has to load before a token exists --
// that is where the operator types one, or where the OIDC redirect begins.
// It ships no data: every byte it renders comes from a subsequent gated
// /v1 call.
func uiFallback(api http.Handler) http.Handler {
	ui := webui.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isV1Path(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		ui.ServeHTTP(w, r)
	})
}

// isV1Path reports whether path names the /v1 API, for uiFallback's
// classification. A plain strings.HasPrefix(path, "/v1/") misses two shapes
// that must still count as /v1: the bare "/v1" with no trailing segment,
// and any number of doubled leading slashes ("//v1/routers"). Missing
// either sends the request to webui.Handler() instead of the gate, which
// answers 200 or 503 with no token required -- not exploitable today, since
// every row in routes() carries the trailing slash, but a route registered
// at the bare /v1 in the future would be shadowed by the UI permanently,
// reaching neither the gate nor the mux, with nothing to say so.
//
// It collapses only leading slashes, and does not resolve "." or ".."
// segments. Doing that here would let a path like "/v1/../etc" -- which
// already matches the plain prefix and so already reaches the gate today
// -- reclassify as non-API and skip the gate entirely. That is the opposite
// of safe, so the rest of the path is left exactly as the client sent it;
// ServeMux does its own cleaning downstream, behind the gate rather than in
// front of it.
func isV1Path(path string) bool {
	for strings.HasPrefix(path, "//") {
		path = path[1:]
	}
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

// Handler returns the routed, authenticated handler to serve.
func (s *Server) Handler() http.Handler { return s.handler }

// routePatterns returns the patterns the mux was built from, in table
// order. It is what api/openapi_test.go compares against the contract's
// paths, and it is derived from routes() rather than restated so that the
// two cannot drift: there is nothing here to update when a route is added.
func (s *Server) routePatterns() []string {
	rs := s.routes()
	patterns := make([]string, 0, len(rs))
	for _, rt := range rs {
		patterns = append(patterns, rt.pattern)
	}
	return patterns
}

// serveOpenAPI writes the embedded contract.
//
// api/openapi.yaml marks this operation `security: []`, the same override
// GET /v1/auth/config carries, for a related reason: a daemon that would
// not tell an unauthenticated caller what shape its 401 is in has hidden
// nothing (the paths are already public knowledge in this repo) while
// making the API harder to discover, and OpenAPI's per-operation security
// override exists precisely to say so. That is the contract's decision and
// not this file's, and the claim is checked against the document in
// TestAuthGateMatchesTheContractsSecurity, so changing the contract's mind
// about it fails the build rather than silently disagreeing with the code.
func (s *Server) serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	// application/yaml per the contract's own response content type. It is
	// the registered type (RFC 9512) rather than text/yaml or
	// application/x-yaml, both of which predate registration.
	w.Header().Set("Content-Type", "application/yaml")
	// The error is dropped deliberately, as in writeUnauthorized: the
	// status is already written, so there is nothing left to tell the
	// client, and the only cause is a hung-up connection.
	_, _ = w.Write(openapiYAML)
}
