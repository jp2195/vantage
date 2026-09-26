package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// internalMessageRe is the whole of a 500's message: one fixed sentence and
// an opaque reference. Anything else in it came from the error.
var internalMessageRe = regexp.MustCompile(
	`^internal error; the server log records the cause under ref=([0-9a-f]{16})$`)

// leakMarkers are fragments of a ClickHouse driver error, each of which the
// probe below confirms is present in the raw error this file drives. None
// of them belongs in any message this API means to send.
var leakMarkers = []string{"SELECT", " FROM ", "GROUP BY", "code: 60", "Unknown table",
	"system.route_unicast_current", "query routes"}

// requireFailingAPI builds a Server whose every query fails inside
// ClickHouse. "system" exists on every server and holds none of vantage's
// tables, so each statement query/ issues against it fails in the analyzer
// with UNKNOWN_TABLE. That error's message quotes the scope the table was
// named in: the statement's own SQL when that is the outer query, but only
// the name of a WITH entry when the table is named inside one (ClickHouse
// 26.8; 24.8 quoted the whole statement either way). Nothing is written
// anywhere: query/ only reads.
func requireFailingAPI(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	conn := chtest.Require(t, t.Context(), apiTestDB)
	q, err := query.New(conn, "system")
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	var logs bytes.Buffer
	s, err := NewServer(q, Config{
		DefaultPage:      1000,
		MaxPage:          10000,
		MaxUnscopedSince: 24 * time.Hour,
		Tokens:           []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, &logs
}

// logLineWithRef returns the one log line carrying ref, or "" if none does.
func logLineWithRef(t *testing.T, logs, ref string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "ref="+ref) {
			found = append(found, line)
		}
	}
	if len(found) > 1 {
		t.Errorf("ref=%s appears on %d log lines; a reference must name one failure", ref, len(found))
	}
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// TestInternalErrorBodyCarriesNoSQL drives real ClickHouse failures through
// the handler and holds both halves of the 500 contract: the body says
// nothing about the cause, and the log says everything, under a reference
// the body gives the caller to quote.
func TestInternalErrorBodyCarriesNoSQL(t *testing.T) {
	s, logs := requireFailingAPI(t)

	// Probe the probe: the failure driven below must itself carry SQL and
	// every leak marker, or the absence checks pass against a body that
	// never had anything to leak. query.Routes names its first table in the
	// outer query, so its error quotes the SQL; query.Routers names its
	// table inside a WITH, whose error quotes only the WITH entry's name.
	_, raw := s.q.Routes(t.Context(), query.RouteFilter{Prefix: "10.0.0.0/8"})
	if raw == nil {
		t.Fatal("query.Routes against the system database succeeded; this test needs a failing query")
	}
	for _, m := range leakMarkers {
		if !strings.Contains(raw.Error(), m) {
			t.Fatalf("the raw error no longer contains %q, so asserting its absence from the body "+
				"proves nothing -- pick markers the driver error actually carries: %v", m, raw)
		}
	}

	refs := map[string]string{}
	for _, target := range []string{
		"/v1/routers",
		"/v1/peers",
		"/v1/routes/unicast?prefix=10.0.0.0/8",
	} {
		rec := get(t, s, target)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("GET %s = %d, want 500; body %s", target, rec.Code, rec.Body.String())
			continue
		}
		var body ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: the 500 body is not the contract's ErrorResponse: %v; body %s",
				target, err, rec.Body.String())
		}
		if body.Error.Code != ErrInternal {
			t.Errorf("GET %s error code = %q, want %q", target, body.Error.Code, ErrInternal)
		}
		for _, m := range leakMarkers {
			if strings.Contains(rec.Body.String(), m) {
				t.Errorf("GET %s: the body carries %q from the driver error: %s",
					target, m, rec.Body.String())
			}
		}
		match := internalMessageRe.FindStringSubmatch(body.Error.Message)
		if match == nil {
			t.Errorf("GET %s: message %q is not the fixed internal-error sentence with a ref",
				target, body.Error.Message)
			continue
		}
		ref := match[1]
		if prev, dup := refs[ref]; dup {
			t.Errorf("GET %s and GET %s both answered ref=%s; each failure needs its own", prev, target, ref)
		}
		refs[ref] = target

		line := logLineWithRef(t, logs.String(), ref)
		if line == "" {
			t.Errorf("GET %s: no log line carries ref=%s, so the caller's reference leads nowhere", target, ref)
			continue
		}
		if !strings.Contains(line, "Unknown table") || !strings.Contains(line, "path="+strings.SplitN(target, "?", 2)[0]) {
			t.Errorf("GET %s: the log line for ref=%s lacks the driver's error or the path: %s", target, ref, line)
		}
	}
}

// TestInternalErrorKeepsUserFacingFourHundreds is the other side, on the
// SAME failing server: a request that is wrong on its face is refused
// before any query runs, and its sentence is the caller's to read.
func TestInternalErrorKeepsUserFacingFourHundreds(t *testing.T) {
	s, _ := requireFailingAPI(t)
	for _, tc := range []struct{ target, want string }{
		{"/v1/peers?rib=not_a_rib", "rib= is not one of the BMP RIB views"},
		{"/v1/events?router=10.0.0.11", "peer= is required"},
	} {
		msg := requireError(t, s, tc.target, http.StatusBadRequest, ErrInvalidParam)
		if !strings.Contains(msg, tc.want) {
			t.Errorf("GET %s message = %q, want it to keep %q", tc.target, msg, tc.want)
		}
		if internalMessageRe.MatchString(msg) {
			t.Errorf("GET %s: a 400 was answered with the internal-error sentence", tc.target)
		}
	}
}

// TestFailMapsEachSentinelToItsOwnBody pins fail's switch on its own, with
// no database. Three sentinels keep their sentence, and anything else gets
// the generic one. The generic row is written with driver text in it, so
// passing it through unchanged fails here, not only against ClickHouse.
func TestFailMapsEachSentinelToItsOwnBody(t *testing.T) {
	var logs bytes.Buffer
	s, err := NewServer(nil, Config{
		DefaultPage: 1000,
		MaxPage:     10000,
		Tokens:      []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	driverText := "code: 60, message: Unknown table expression identifier " +
		"'vantage.peer_current' in scope SELECT router_ip FROM vantage.peer_current"

	for _, tc := range []struct {
		name     string
		err      error
		status   int
		code     string
		keepsMsg bool
	}{
		{"a bad filter keeps its sentence",
			fmt.Errorf("%w: prefix= must be a CIDR", query.ErrBadFilter),
			http.StatusBadRequest, ErrInvalidParam, true},
		{"a bad cursor keeps its sentence",
			fmt.Errorf("%w: not a cursor this daemon issued", errBadCursor),
			http.StatusBadRequest, ErrInvalidParam, true},
		{"a superseded session keeps its sentence",
			query.ErrSessionChanged,
			http.StatusConflict, ErrSessionChanged, true},
		{"a driver error becomes the generic sentence",
			fmt.Errorf("query routers: %s", driverText),
			http.StatusInternalServerError, ErrInternal, false},
		{"a deadline becomes the generic sentence",
			fmt.Errorf("query peers: %w", context.DeadlineExceeded),
			http.StatusInternalServerError, ErrInternal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.fail(rec, httptest.NewRequest(http.MethodGet, "/v1/routers", nil), tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v; body %s", err, rec.Body.String())
			}
			if body.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.code)
			}
			if tc.keepsMsg {
				if body.Error.Message != tc.err.Error() {
					t.Errorf("message = %q, want the error's own sentence %q", body.Error.Message, tc.err.Error())
				}
				return
			}
			if !internalMessageRe.MatchString(body.Error.Message) {
				t.Errorf("message = %q, want only the fixed sentence and a ref", body.Error.Message)
			}
		})
	}
}
