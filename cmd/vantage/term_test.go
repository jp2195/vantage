package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/api"
	"github.com/jp2195/vantage/query"
)

// hostilePayload reproduces a real hostile payload: a BMP Initiation
// sysName that retitles the terminal (OSC 0 ... BEL), clears it (CSI 2J)
// and recolors what follows (CSI 31m). bmp.ParseInit keeps it byte for
// byte, the archive stores it, and the API returns it, so the CLI is the
// last place it can be stopped before a terminal acts on it.
const hostilePayload = "\x1b]0;pwned\a\x1b[2J\x1b[31mcore-1"

// captureStdio runs fn with os.Stdout and os.Stderr pointed at pipes and
// returns what each received.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	read := func(f **os.File) (func() string, func()) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := *f
		*f = w
		done := make(chan string)
		go func() {
			b, _ := io.ReadAll(r)
			done <- string(b)
		}()
		return func() string { return <-done }, func() { w.Close(); *f = orig }
	}
	outText, outRestore := read(&os.Stdout)
	errText, errRestore := read(&os.Stderr)
	fn()
	outRestore()
	errRestore()
	return outText(), errText()
}

// controlBytes returns every byte of s a terminal would act on: C0 other
// than the newline that ends a line, DEL, and the two-byte UTF-8 encoding
// of a C1 control (0xC2 0x80-0x9F). The tabwriter pads with spaces, so a
// tab in table output is itself a leak.
func controlBytes(s string) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n':
		case c < 0x20, c == 0x7f:
			out = append(out, c)
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			out = append(out, c, s[i+1])
		}
	}
	return out
}

// fakeRoutersAPI answers GET /v1/routers with one router named sysName and
// one meta warning whose message is warning.
func fakeRoutersAPI(t *testing.T, sysName, warning string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"data": []api.WireRouter{api.NewWireRouter(query.Router{
				SysName: sysName, IP: netip.MustParseAddr("10.0.0.1"),
				Collector: "c1", SessionID: 1, LastSeen: time.Unix(0, 0),
			})},
			"meta": api.Meta{Warnings: api.Warnings{{Code: api.WarnSessionDumping, Message: warning}}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRoutersTableEscapesRouterControlBytes drives `vantage query routers`
// end to end against an API that returns the hostile sysName above, plus a
// sysName carrying a tab, a newline and a C1 CSI, which would otherwise
// shift the columns, forge a second row, or reach a terminal that honors
// 8-bit controls.
func TestRoutersTableEscapesRouterControlBytes(t *testing.T) {
	for _, name := range []string{hostilePayload, "a\tb\nFAKE-ROW\u009b31m"} {
		srv := fakeRoutersAPI(t, name, hostilePayload)
		var err error
		stdout, stderr := captureStdio(t, func() {
			err = cmdQueryRouters([]string{"-api", srv.URL, "-token", "t"})
		})
		if err != nil {
			t.Fatalf("query routers: %v", err)
		}
		if bad := controlBytes(stdout); len(bad) > 0 {
			t.Errorf("table output for sysName %q carries control bytes %q:\n%s", name, bad, stdout)
		}
		if n := strings.Count(stdout, "\n"); n != 2 {
			t.Errorf("table output has %d lines, want 2 (header and one row):\n%s", n, stdout)
		}
		if bad := controlBytes(stderr); len(bad) > 0 {
			t.Errorf("printed warning carries control bytes %q: %q", bad, stderr)
		}
		// Escaped, not deleted: the operator still sees that the router
		// sent something strange, and what.
		if !strings.Contains(stdout, `\x1b`) && !strings.Contains(stdout, `\x09`) {
			t.Errorf("control bytes were dropped rather than shown escaped:\n%s", stdout)
		}
	}
}

// TestJSONOutputKeepsTheRawValue: -o json is for programs, and
// encoding/json already escapes every control byte, so the value must
// arrive unaltered rather than escaped twice.
func TestJSONOutputKeepsTheRawValue(t *testing.T) {
	srv := fakeRoutersAPI(t, hostilePayload, "")
	var err error
	stdout, _ := captureStdio(t, func() {
		err = cmdQueryRouters([]string{"-api", srv.URL, "-token", "t", "-o", "json"})
	})
	if err != nil {
		t.Fatalf("query routers -o json: %v", err)
	}
	var got []api.WireRouter
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if len(got) != 1 || got[0].SysName != hostilePayload {
		t.Errorf("sys_name = %+v, want the raw payload %q", got, hostilePayload)
	}
}

// TestErrorsFromTheAPIAreEscaped covers the other two ways server text
// reaches the terminal: the ErrorResponse message an error carries, and
// the line main prints it on.
func TestErrorsFromTheAPIAreEscaped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.ErrorBody{
			Code: api.ErrInvalidParam, Message: hostilePayload + "\nvantage: forged line",
		}})
	}))
	t.Cleanup(srv.Close)
	_, err := (&apiSource{base: srv.URL, token: "t"}).Routers(t.Context())
	if err == nil {
		t.Fatal("a 400 was accepted")
	}
	var buf bytes.Buffer
	reportError(&buf, err)
	if bad := controlBytes(buf.String()); len(bad) > 0 {
		t.Errorf("printed error carries control bytes %q: %q", bad, buf.String())
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("printed error spans %d lines, want 1 -- the server's newline forged a line: %q", n, buf.String())
	}
	// The CLI's own multi-line errors keep their layout.
	buf.Reset()
	reportError(&buf, errors.New("first\n\nsecond\x1b[2J"))
	if got, want := buf.String(), "vantage: first\n\nsecond\\x1b[2J\n"; got != want {
		t.Errorf("reportError = %q, want %q", got, want)
	}
}

// TestAPIClientTimesOutPerRequest: the CLI used http.DefaultClient, which
// has no timeout, so a daemon that accepted the connection and never
// answered hung the command forever. The bound is per request -- a RIB walk
// is many requests and may legitimately take longer than any one of them.
func TestAPIClientTimesOutPerRequest(t *testing.T) {
	if defaultAPIClient.Timeout != apiRequestTimeout || apiRequestTimeout != 2*time.Minute {
		t.Errorf("default client timeout = %v (apiRequestTimeout %v), want 2m",
			defaultAPIClient.Timeout, apiRequestTimeout)
	}
	// Behaviorally: the nil-client apiSource must go through
	// defaultAPIClient. It is swapped for one with a short bound so the test
	// does not wait two minutes; a fallback to http.DefaultClient would hang
	// until the context's own 5s deadline instead.
	orig := defaultAPIClient
	defaultAPIClient = newAPIClient(100 * time.Millisecond)
	t.Cleanup(func() { defaultAPIClient = orig })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := (&apiSource{base: srv.URL, token: "t"}).Routers(ctx)
	if err == nil {
		t.Fatal("a server that never answered produced no error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("request gave up after %v, want about 100ms -- the client has no timeout of its own", elapsed)
	}
}

// TestTableEscapesWhatBypassesRowf pins the second layer: a table row
// written with plain fmt.Fprintf rather than rowf still cannot put a
// control byte on the terminal, though its tabs and newlines -- which a
// format string needs -- pass through.
func TestTableEscapesWhatBypassesRowf(t *testing.T) {
	var buf bytes.Buffer
	if err := tableTo(&buf, func(w io.Writer) {
		fmt.Fprintf(w, "A\tB\n%s\t%s\n", hostilePayload, "x\u009b\x9b")
	}); err != nil {
		t.Fatal(err)
	}
	if bad := controlBytes(buf.String()); len(bad) > 0 {
		t.Errorf("table carries control bytes %q: %q", bad, buf.String())
	}
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Errorf("table has %d lines, want 2: %q", n, buf.String())
	}
}

func TestEscapeControl(t *testing.T) {
	for _, tc := range []struct {
		in         string
		keepLayout bool
		want       string
	}{
		{"core-1", false, "core-1"},
		{"rtr-é-1", false, "rtr-é-1"},
		{hostilePayload, false, `\x1b]0;pwned\x07\x1b[2J\x1b[31mcore-1`},
		{"a\tb\nc\r", false, `a\x09b\x0ac\x0d`},
		{"a\tb\nc\r", true, "a\tb\nc\\x0d"},
		{"del\x7f", false, `del\x7f`},
		{"c1\u009b31m", false, `c1\u009b31m`},
		{"raw\x9b31m", false, `raw\x9b31m`},
	} {
		if got := escapeControl(tc.in, tc.keepLayout); got != tc.want {
			t.Errorf("escapeControl(%q, %v) = %q, want %q", tc.in, tc.keepLayout, got, tc.want)
		}
	}
}
