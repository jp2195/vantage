// Auth: the bearer-token gate every route sits behind, and the only place
// in this daemon that looks at a token's value.
//
// Two properties run through it, and neither is enforced by a test:
//
//   - The comparison is constant-time, and every configured token is
//     compared even after one has matched. A test cannot observe either;
//     see match.
//   - Nothing the caller presented is ever written back out -- not into
//     the 401 body, not into a header, not into an error. The 401 body is
//     a constant, so there is no format string for a token to be
//     interpolated into by a later "helpful" edit.
//
// What a log line gets instead is the token's NAME, from the request
// context (see tokenNameFromContext). secret.APIToken has no printable
// form by construction, so the name is the whole audit trail.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// authToken is one configured caller with its token revealed. The reveal
// happens once here, at construction, rather than per request: this file's
// single RevealSecret call is easier to keep honest than one buried in a
// request path, and the same loop that reveals is the one that can see two
// entries sharing a value.
type authToken struct {
	name string
	// raw is []byte because subtle.ConstantTimeCompare takes []byte, and
	// converting per request would allocate a copy of the credential on
	// every call.
	raw []byte
}

// newAuth builds the middleware that gates a handler on the Authorization
// header, returning 401 with the contract's ErrorResponse body when the
// header is missing, malformed, or presents a token no configured entry
// matches. On success it passes the request through with the matching
// entry's name in its context.
//
// It returns an error, unlike the middleware constructors it will sit
// beside, because it is the first code to look at the token VALUES and so
// the first that can reject a list LoadConfig had to accept. The error is
// the point of the signature: a caller cannot ignore it without the
// compiler noticing, so a duplicate or empty token stays a startup failure
// rather than becoming a runtime surprise.
func newAuth(tokens []Token) (func(http.Handler) http.Handler, error) {
	if len(tokens) == 0 {
		// LoadConfig rejects this too. It is re-checked at the gate
		// because the failure mode of not checking is a daemon that
		// starts, looks healthy, and 401s every request -- a much longer
		// afternoon than a startup error.
		return nil, errors.New("no tokens configured: newAuth has nothing to " +
			"authenticate against, and this path only runs in token mode -- " +
			"auth.mode: none skips newAuth entirely")
	}
	configured := make([]authToken, 0, len(tokens))
	seen := make(map[string]int, len(tokens))
	for i, t := range tokens {
		// An empty token is not merely useless here, it is dangerous:
		// subtle.ConstantTimeCompare returns 1 for two empty inputs, so an
		// empty configured token authenticates a request presenting
		// "Bearer " -- which is exactly what a client sends when its own
		// token variable is unset. LoadConfig rejects this as a
		// half-finished edit; match's correctness depends on it, so the
		// dependency is checked where it is relied on.
		if t.Token.Empty() {
			return nil, fmt.Errorf("tokens[%d] (%q): token is empty", i, t.Name)
		}
		raw := t.Token.RevealSecret()
		// Duplicate token values, the check api/config.go's validate()
		// defers to here. Two entries sharing one token load happily and
		// authenticate identically; the only difference they make is which
		// name gets logged, and that is decided by iteration order rather
		// than by the operator. An audit trail that confidently names the
		// wrong caller is worse than one that names none, so this is a
		// startup error for the same reason a duplicate name is.
		//
		// This is also why the check is here and not in validate(): it
		// needs the revealed value, and this loop is the one place that
		// has to reveal anyway. Doing it in validate() would have added a
		// second RevealSecret call site for no new information.
		if prev, ok := seen[raw]; ok {
			return nil, fmt.Errorf("tokens[%d] (%q): token is identical to "+
				"tokens[%d] (%q); two callers sharing one token cannot be told "+
				"apart in a log line, and which name is logged would be decided "+
				"by config order", i, t.Name, prev, tokens[prev].Name)
		}
		seen[raw] = i
		configured = append(configured, authToken{name: t.Name, raw: []byte(raw)})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Exactly one Authorization header, because r.Header.Get
			// returns the FIRST of several and nothing in net/http rejects
			// a request carrying two. A fronting proxy that forwards both
			// and reads the last would then disagree with this daemon
			// about which token authenticated the request -- the two would
			// log different callers for the same call, which is the audit
			// trail failing silently rather than loudly. There is no
			// legitimate reason to send two, so a request that does is not
			// authenticated. Zero headers lands here too, and is the same
			// 401 by a shorter path.
			presented := r.Header.Values("Authorization")
			if len(presented) != 1 {
				writeUnauthorized(w)
				return
			}
			name, ok := match(configured, presented[0])
			if !ok {
				writeUnauthorized(w)
				return
			}
			ctx := context.WithValue(r.Context(), tokenNameKey{}, name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}, nil
}

// match returns the name of the configured entry whose token the header
// presents.
//
// Every configured token is compared, with no early return once one has
// matched. Returning early would make the time this function takes depend
// on WHICH token matched, which is a slow read of the config's ordering
// for anyone holding one valid token. The cost of not returning early is a
// handful of byte compares per request.
func match(configured []authToken, header string) (string, bool) {
	presented, ok := bearerToken(header)
	if !ok {
		return "", false
	}
	name, found := "", false
	for _, t := range configured {
		// NO TEST CAN CATCH A REGRESSION ON THIS LINE. Replace
		// subtle.ConstantTimeCompare with ==, or with strings.HasPrefix
		// plus a length check, and every test in api/auth_test.go still
		// passes -- verified. A byte-at-a-time comparison returns sooner
		// the earlier it finds a difference, which is enough to recover a
		// token one byte at a time over enough requests, and no assertion
		// available to a Go test can see the difference. This comment is
		// the guard; review is what enforces it.
		//
		// The assignment below is not an early exit: it runs at most once
		// per request, so the total time does not depend on which entry
		// matched, only on how many are configured.
		if subtle.ConstantTimeCompare(presented, t.raw) == 1 {
			name, found = t.name, true
		}
	}
	return name, found
}

// bearerToken pulls the token68 out of an Authorization header, reporting
// whether the header was a Bearer credential at all.
//
// Per RFC 7235 the scheme is case-insensitive ("bearer" and "BEARER" are
// the same scheme) and its separator is 1*SP, so extra spaces before the
// token are legal. Nothing else is trimmed: a tab separator and a trailing
// space are both outside the grammar, and accepting them would mean this
// daemon's idea of a token quietly differs from the operator's file by
// whitespace.
func bearerToken(header string) ([]byte, bool) {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	return []byte(strings.TrimLeft(rest, " ")), true
}

// writeUnauthorized writes the contract's 401: WWW-Authenticate per RFC
// 7235, and an ErrorResponse body whose message is a CONSTANT.
//
// The constant is the design. A formatted message is one edit away from
// naming the token that failed -- the "helpful" version of this response
// that puts a mistyped credential from someone's shell history into the
// server's logs, and from any proxy log between here and there.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// The error is dropped deliberately: the status and headers are
	// already written, so there is nothing left to say to the client, and
	// the only causes are a hung-up connection.
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: ErrorBody{
		Code:    ErrUnauthorized,
		Message: "missing or unrecognized bearer token",
	}})
}

// tokenNameKey is the context key the authenticated caller's name is stored
// under. It is an unexported empty struct type, the standard shape: no
// other package can construct the key, so nothing outside this one can set
// or shadow the value a handler reads.
//
// Only the name is ever put in a context. The token is not, in any form:
// a context is copied into every derived request and is reachable from
// anything holding one, which is the opposite of what a credential wants.
type tokenNameKey struct{}

// tokenNameFromContext returns the name of the token that authenticated the
// request, or "" if the request did not come through newAuth's middleware.
// It is what a log line or an audit record names the caller by.
func tokenNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(tokenNameKey{}).(string)
	return name
}
