package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// authFor builds the middleware for the tests whose subject is a request
// rather than the construction. All of them configure a token list that is
// valid by construction, so an error out of newAuth is a bug in the test.
func authFor(t *testing.T, tokens ...Token) func(http.Handler) http.Handler {
	t.Helper()
	mw, err := newAuth(tokens)
	if err != nil {
		t.Fatalf("newAuth: %v", err)
	}
	return mw
}

// okHandler reports whether it ran, which is how the 401 cases below tell
// "the middleware answered" from "the handler answered 401 itself".
func okHandler(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthRejectsWrongAndMissingTokens(t *testing.T) {
	ran := false
	mw := authFor(t, Token{Name: "grafana", Token: secret.NewAPIToken("right")})
	h := mw(okHandler(&ran))

	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"valid", "Bearer right", http.StatusOK},
		{"wrong", "Bearer wrong", http.StatusUnauthorized},
		{"absent", "", http.StatusUnauthorized},
		{"no scheme", "right", http.StatusUnauthorized},
		{"wrong scheme", "Basic right", http.StatusUnauthorized},
		// The last two are the reason a length-blind compare or a
		// strings.HasPrefix is not good enough: both of these pass one.
		{"prefix of valid", "Bearer righ", http.StatusUnauthorized},
		{"valid plus suffix", "Bearer rightx", http.StatusUnauthorized},

		// RFC 7235's auth-scheme is case-insensitive, so all three of
		// these are the same request as the first one.
		{"lowercase scheme", "bearer right", http.StatusOK},
		{"uppercase scheme", "BEARER right", http.StatusOK},
		{"mixed-case scheme", "BeArEr right", http.StatusOK},
		// ...and its separator is 1*SP, so more than one space is a legal
		// presentation of the same token.
		{"two spaces", "Bearer  right", http.StatusOK},

		// Everything else about the header's shape fails closed.
		//
		// Two of these do not describe a request anyone can send. Header
		// values are OWS-trimmed at both ends by net/textproto per RFC
		// 7230, so `Authorization: Bearer right ` arrives at a handler as
		// "Bearer right" and is a 200, and `Bearer ` arrives as "Bearer".
		// Header.Set below bypasses that parse, which is the point: what
		// these two cases pin is bearerToken itself, for the day it is
		// handed a value that did not come from net/http -- a header
		// lifted out of a proxy's JSON, say -- and they are why "just
		// trim it" is not the right response to a report about either.
		// The tab case is different and IS reachable over the wire: only
		// the ends are trimmed, so a tab inside the value survives, and a
		// tab is not the SP the grammar names.
		{"scheme only", "Bearer", http.StatusUnauthorized},
		{"scheme and no token", "Bearer ", http.StatusUnauthorized},
		{"tab separator", "Bearer\tright", http.StatusUnauthorized},
		{"trailing space", "Bearer right ", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran = false
			req := httptest.NewRequest(http.MethodGet, "/v1/routers", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if ran != (tc.want == http.StatusOK) {
				t.Errorf("wrapped handler ran = %v, want %v", ran, tc.want == http.StatusOK)
			}
			if tc.want == http.StatusUnauthorized &&
				rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Error("401 must carry WWW-Authenticate: Bearer")
			}
		})
	}
}

// TestAuthErrorBodyDoesNotEchoTheToken: a 401 body or log line that repeats
// what the caller sent turns a mistyped token in someone's shell history
// into a token in the server's logs.
//
// It checks the headers as well as the body because the tempting shape for
// a "helpful" 401 -- echoing the credential back in WWW-Authenticate's
// error_description, which RFC 6750 does have a slot for -- leaks exactly
// as badly and would leave the body assertion green.
func TestAuthErrorBodyDoesNotEchoTheToken(t *testing.T) {
	mw := authFor(t, Token{Name: "grafana", Token: secret.NewAPIToken("right")})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/v1/routers", nil)
	req.Header.Set("Authorization", "Bearer hunter2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("401 body echoed the presented token: %s", rec.Body.String())
	}
	for k, vs := range rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, "hunter2") {
				t.Errorf("401 header %s echoed the presented token: %s", k, v)
			}
		}
	}
	// The configured token has no business in the body either, and the
	// body has to be the contract's ErrorResponse: api/openapi.yaml types
	// every non-2xx as one, with 401's code fixed to "unauthorized".
	if strings.Contains(rec.Body.String(), "right") {
		t.Errorf("401 body contains a configured token: %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("401 Content-Type = %q, want application/json", ct)
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 body is not the contract's ErrorResponse: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Code != ErrUnauthorized {
		t.Errorf("401 code = %q, want %q", body.Error.Code, ErrUnauthorized)
	}
	if body.Error.Message == "" {
		t.Error("401 message is empty")
	}
}

// TestAuthNamesTheCallerInTheRequestContext covers the audit trail, which
// is the whole reason Token carries a name: secret.APIToken has no
// printable form by construction, so the name is the only thing a log line
// can say about who called. It also covers the compare-every-token loop's
// observable half -- with three tokens configured, each one has to
// authenticate as itself.
func TestAuthNamesTheCallerInTheRequestContext(t *testing.T) {
	mw := authFor(t,
		Token{Name: "grafana", Token: secret.NewAPIToken("g-" + tokenSentinel)},
		Token{Name: "cron", Token: secret.NewAPIToken("c-" + tokenSentinel)},
		Token{Name: "oncall", Token: secret.NewAPIToken("o-" + tokenSentinel)},
	)
	got, printed := "", ""
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tokenNameFromContext(r.Context())
		// context.valueCtx has a String method that renders the whole
		// chain, and its stringify prints a string value VERBATIM (a
		// fmt.Stringer value goes through String, which is why a
		// secret.APIToken in a context would render "REDACTED"). So
		// printing the context is a real, if partial, instrument for "the
		// token is not in here": it catches the shape a leak would
		// actually take -- the presented header stashed beside the name
		// as a plain string, one line of "while I am here" -- and it
		// would not catch one stored as a []byte. Nothing can enumerate a
		// context, so this is as close as a test gets.
		printed = fmt.Sprint(r.Context())
	}))
	for _, tc := range []struct{ token, want string }{
		{"g-" + tokenSentinel, "grafana"},
		{"c-" + tokenSentinel, "cron"},
		{"o-" + tokenSentinel, "oncall"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got = ""
			req := httptest.NewRequest(http.MethodGet, "/v1/routers", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got != tc.want {
				t.Errorf("tokenNameFromContext = %q, want %q", got, tc.want)
			}
			if strings.Contains(printed, tokenSentinel) {
				t.Errorf("the request context renders a token: %s", printed)
			}
		})
	}
	// A context that never went through the middleware has no name, and
	// asking for one is not a panic: a handler reached by some other path
	// gets "" and logs an empty caller rather than crashing the daemon.
	if name := tokenNameFromContext(context.Background()); name != "" {
		t.Errorf("tokenNameFromContext(background) = %q, want empty", name)
	}
}

// TestNewAuthRejectsDuplicateTokenValues is the check api/config.go's
// validate() deliberately does not make. Two entries sharing one token
// otherwise load silently, and whichever name this map happens to keep is
// what every later log line attributes those requests to -- an audit trail
// naming the wrong caller, which is worse than none.
func TestNewAuthRejectsDuplicateTokenValues(t *testing.T) {
	_, err := newAuth([]Token{
		{Name: "grafana", Token: secret.NewAPIToken("shared-" + tokenSentinel)},
		{Name: "cron", Token: secret.NewAPIToken("shared-" + tokenSentinel)},
	})
	if err == nil {
		t.Fatal("newAuth accepted two entries sharing one token")
	}
	// Both names, because the operator has to be told which two entries to
	// go look at, and neither token: this error is printed at startup and
	// is a log line like any other.
	for _, want := range []string{"grafana", "cron"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), tokenSentinel) {
		t.Errorf("error %v contains the token", err)
	}
	// The counterweight: distinct tokens are exactly what a multi-caller
	// config looks like, and the check must not reject one.
	if _, err := newAuth([]Token{
		{Name: "grafana", Token: secret.NewAPIToken("g-" + tokenSentinel)},
		{Name: "cron", Token: secret.NewAPIToken("c-" + tokenSentinel)},
	}); err != nil {
		t.Errorf("newAuth rejected two distinct tokens: %v", err)
	}
}

// TestNewAuthRejectsAnUnusableTokenList re-checks at the gate what
// LoadConfig already checks, because the consequence of an empty token
// reaching the compare is specific and silent: subtle.ConstantTimeCompare
// of two empty inputs returns 1, so a configured empty token authenticates
// a request presenting "Bearer " -- the header a client sends when its own
// token variable is unset.
func TestNewAuthRejectsAnUnusableTokenList(t *testing.T) {
	for _, c := range []struct {
		name   string
		tokens []Token
		want   string
	}{
		{"no tokens", nil, "no tokens"},
		{"empty list", []Token{}, "no tokens"},
		{
			"empty token",
			[]Token{{Name: "grafana", Token: secret.NewAPIToken("")}},
			"token is empty",
		},
		{
			"empty token beside a good one",
			[]Token{
				{Name: "grafana", Token: secret.NewAPIToken("g-" + tokenSentinel)},
				{Name: "cron", Token: secret.NewAPIToken("")},
			},
			"token is empty",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			mw, err := newAuth(c.tokens)
			if err == nil {
				t.Fatal("newAuth accepted the list")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if mw != nil {
				t.Error("newAuth returned a middleware alongside an error")
			}
			if strings.Contains(err.Error(), tokenSentinel) {
				t.Errorf("error %v contains a token", err)
			}
		})
	}
}

// TestAuthRejectsTwoAuthorizationHeaders covers an ambiguity that is
// reachable over real HTTP and that nothing in net/http resolves: a request
// may carry two Authorization headers, and r.Header.Get returns the first.
// A fronting proxy that forwards both and reads the last would attribute
// the same call to a different caller than this daemon does -- two audit
// trails disagreeing, with neither one erroring. There is no legitimate
// reason to send two, so the request is not authenticated.
func TestAuthRejectsTwoAuthorizationHeaders(t *testing.T) {
	ran := false
	mw := authFor(t,
		Token{Name: "grafana", Token: secret.NewAPIToken("right")},
		Token{Name: "cron", Token: secret.NewAPIToken("alsoright")},
	)
	h := mw(okHandler(&ran))
	for _, c := range []struct{ name, first, second string }{
		{"both valid and identical", "Bearer right", "Bearer right"},
		{"two different valid tokens", "Bearer right", "Bearer alsoright"},
		{"valid then wrong", "Bearer right", "Bearer wrong"},
		{"wrong then valid", "Bearer wrong", "Bearer right"},
		{"valid then empty", "Bearer right", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ran = false
			req := httptest.NewRequest(http.MethodGet, "/v1/routers", nil)
			req.Header.Add("Authorization", c.first)
			req.Header.Add("Authorization", c.second)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if ran {
				t.Error("wrapped handler ran on an ambiguous request")
			}
			if rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Error("401 must carry WWW-Authenticate: Bearer")
			}
		})
	}
}

// TestTokenNameCannotBeSpoofedThroughTheContext pins the reason the context
// key is an unexported empty struct type rather than a string: a string key
// is a value any other package can write, so a middleware or handler
// upstream of this one could set the caller name that every log line and
// audit record then believes.
//
// The probe is partial in exactly the way the context-rendering check in
// TestAuthNamesTheCallerInTheRequestContext is partial -- it can only try
// key strings someone thought to list, so it catches the plausible
// spellings of the mutation and not an arbitrary one. That is still an
// automated guard on a line that otherwise has none: go vet says nothing
// about a string context key (the check that does, staticcheck SA1029, is
// not in this repo's CI), so without this test the string-key version
// passes everything.
func TestTokenNameCannotBeSpoofedThroughTheContext(t *testing.T) {
	for _, k := range []string{"tokenName", "token_name", "name", "tokenNameKey", "authTokenName", "caller"} {
		ctx := context.WithValue(context.Background(), any(k), "spoofed")
		if got := tokenNameFromContext(ctx); got != "" {
			t.Errorf("string key %q spoofed the caller name: %q", k, got)
		}
	}
	// The same request through the middleware still reports the real name,
	// so what the assertions above pin is the key's type and not a
	// tokenNameFromContext that returns "" no matter what.
	mw := authFor(t, Token{Name: "grafana", Token: secret.NewAPIToken("right")})
	got := ""
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tokenNameFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/routers", nil)
	req.Header.Set("Authorization", "Bearer right")
	// The spoof attempt rides along on the request's own context, which
	// is where a middleware ahead of this one would have put it.
	req = req.WithContext(context.WithValue(req.Context(), any("tokenName"), "spoofed"))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "grafana" {
		t.Errorf("tokenNameFromContext = %q, want %q", got, "grafana")
	}
}

// TestNoneModeServesWithoutAToken is the behavior the mode exists for.
//
// It reaches through /v1/routers -- an ordinarily-gated route, not the
// always-public /v1/openapi.yaml -- and so has to go through handleRouters
// for real, which means q cannot be nil here the way
// TestAuthConfigReportsNoneMode leaves it: NewServer deliberately never
// nil-checks q (see its doc comment), and httptest's direct ServeHTTP call
// has none of a real http.Server's per-request panic recovery, so a nil q
// would crash the whole test binary rather than fail this one test. A
// real connection is
// what every other test in this package already uses for the same reason.
func TestNoneModeServesWithoutAToken(t *testing.T) {
	conn := chtest.Require(t, t.Context(), apiTestDB)
	q, err := query.New(conn, apiTestDB)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	cfg := Config{Auth: AuthConfig{Mode: AuthModeNone}}
	srv, err := NewServer(q, cfg, nil)
	if err != nil {
		t.Fatalf("NewServer in none mode: %v", err)
	}
	rec := httptest.NewRecorder()
	// A loopback Host: none mode refuses any other with 421, which would
	// pass the != 401 below without the request ever reaching a handler.
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/v1/routers", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("none mode returned %d for GET /v1/routers with no token, want 200", rec.Code)
	}
}

// TestNoneModeWarnsOnStartup. A silent open daemon is a real risk (an
// operator may not notice auth.mode: none is set); the log line is the
// difference.
func TestNoneModeWarnsOnStartup(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if _, err := NewServer(nil, Config{Auth: AuthConfig{Mode: AuthModeNone}}, logger); err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if !strings.Contains(buf.String(), "unauthenticated") {
		t.Errorf("startup log %q does not warn that the daemon is unauthenticated", buf.String())
	}
}
