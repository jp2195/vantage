package collector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAdminMirrorLifecycle(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	body := strings.NewReader(`{"router":"10.0.0.1","window":"10m","max_bytes":1048576}`)
	resp, err := http.Post(srv.URL+"/admin/mirror", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("arm: status %d", resp.StatusCode)
	}
	var armed MirrorStatus
	if err := json.NewDecoder(resp.Body).Decode(&armed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if armed.Router != "10.0.0.1" || armed.BytesRemaining != 1048576 {
		t.Fatalf("armed = %+v", armed)
	}

	resp, err = http.Get(srv.URL + "/admin/mirror")
	if err != nil {
		t.Fatal(err)
	}
	var list []MirrorStatus
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list) != 1 || list[0].Router != "10.0.0.1" {
		t.Fatalf("list = %+v", list)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/mirror/10.0.0.1", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disarm: status %d", resp.StatusCode)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("still armed after delete: %+v", got)
	}
}

// TestAdminMirrorRejectsBadInput pins that the API fails cleanly rather than
// arming something half-formed.
func TestAdminMirrorRejectsBadInput(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	for _, c := range []struct{ name, body string }{
		{"bad router", `{"router":"not-an-ip","window":"1m","max_bytes":100}`},
		// A window that is not a Go duration at all, as opposed to "zero
		// window"/"window too long" below which parse fine and are rejected
		// by MirrorRegistry.Arm's bound check instead. NOTE: this does not
		// actually pin armMirror's own time.ParseDuration error check as a
		// distinct code path -- verified by mutation: time.ParseDuration
		// always returns a zero Duration on error, and Arm's own "window
		// must be > 0" check rejects zero just as surely as it rejects an
		// explicit "0s", so deleting the handler's ParseDuration error
		// branch entirely leaves this case (and every other case in this
		// table) still returning 400 via Arm. The same is true of the
		// router-parse error check on this POST path: netip.ParseAddr
		// fails to netip.Addr{}, which Arm's own IsValid check also rejects.
		// Both handler-level checks are genuinely redundant with Arm's own
		// validation for the arm path (they only improve the error message
		// and skip a wasted Arm call) -- unlike the DELETE path's address
		// check, which has no Arm call backing it up and is load-bearing
		// (see TestAdminMirrorDisarmRejectsBadRouter). This case is kept
		// for behavioral coverage -- an invalid duration string must still
		// be rejected with 400 -- not as a mutation-proof pin.
		{"unparseable window", `{"router":"10.0.0.1","window":"not-a-duration","max_bytes":100}`},
		{"zero window", `{"router":"10.0.0.1","window":"0s","max_bytes":100}`},
		{"window too long", `{"router":"10.0.0.1","window":"48h","max_bytes":100}`},
		{"zero bytes", `{"router":"10.0.0.1","window":"1m","max_bytes":0}`},
		{"malformed json", `{`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Post(srv.URL+"/admin/mirror", "application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
			if got := reg.List(); len(got) != 0 {
				t.Fatalf("a rejected request must not arm anything: %+v", got)
			}
		})
	}
}

// TestAdminMirrorRejectsMalformedBodies covers the request-body shapes a
// hostile or merely broken client can send that TestAdminMirrorRejectsBadInput
// does not: no body at all, an empty body, a body that isn't JSON, and JSON
// that decodes but is the wrong shape (a top-level array, and field values of
// the wrong type). None of these may arm anything or return anything but 400.
func TestAdminMirrorRejectsMalformedBodies(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	for _, c := range []struct {
		name string
		body io.Reader
	}{
		{"nil body", nil},
		{"empty body", strings.NewReader("")},
		{"not json", strings.NewReader("this is not json")},
		{"json array instead of object", strings.NewReader(`[1,2,3]`)},
		{"json string instead of object", strings.NewReader(`"10.0.0.1"`)},
		{"wrong field types", strings.NewReader(`{"router":123,"window":456,"max_bytes":"nope"}`)},
		// Truncated mid-stream (no closing brace). Verified empirically
		// (encoding/json's Decoder buffers a raw JSON value via its scanner
		// before ever unmarshaling into the struct, and a syntax error --
		// which an unterminated object is -- aborts before that unmarshal
		// step runs at all): Decode on this exact body leaves req entirely
		// zero-valued, not populated with router=10.0.0.1/window=1m/
		// max_bytes=100 as a prior version of this comment claimed. That
		// means this case alone is caught downstream by the empty-router
		// check even if the Decode error were ignored, so it is kept here
		// only as a "no body at all is well-formed JSON" case, not as proof
		// the Decode error check matters -- see the next case for that.
		{"truncated after all fields valid", strings.NewReader(`{"router":"10.0.0.1","window":"1m","max_bytes":100`)},
		// A duplicate "router" key: encoding/json processes object keys in
		// stream order and assigns into the destination field as it goes, so
		// by the time the second "router" occurrence is reached, req already
		// holds genuinely valid values (router=10.0.0.1, window=1m,
		// max_bytes=100) from the first occurrence and the two other fields.
		// Only then does the wrong-typed second "router" value fail to
		// unmarshal (confirmed empirically: req == {10.0.0.1, 1m, 100} at the
		// point Decode returns its error for this exact body). This is the
		// case that actually exercises the Decode error check: drop that
		// check and this request would sail through every subsequent
		// field-level validation and genuinely arm a mirror on 10.0.0.1 --
		// unlike "truncated after all fields valid" above, which cannot
		// (its req never leaves the zero value).
		{"duplicate key overwrites valid fields with a bad type", strings.NewReader(
			`{"router":"10.0.0.1","window":"1m","max_bytes":100,"router":123}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/mirror", c.body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
			if got := reg.List(); len(got) != 0 {
				t.Fatalf("a rejected request must not arm anything: %+v", got)
			}
		})
	}
}

// neverEndingJSONString is an io.Reader that produces an opening quote
// followed by an endless stream of filler bytes: syntactically, the start of
// a JSON string that never closes. A JSON decoder reading it without any
// bound on how much it will buffer would never terminate, because there is no
// EOF, no closing quote, and no read error -- exactly the shape of a hostile
// "enormous body" attack (a single huge string field) rather than a body that
// is merely long. It also records how many bytes were actually read from it,
// so a test can assert the handler stopped well short of "forever".
type neverEndingJSONString struct {
	n int64
}

func (r *neverEndingJSONString) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.n == 0 {
		p[0] = '"'
		for i := 1; i < len(p); i++ {
			p[i] = 'a'
		}
	} else {
		for i := range p {
			p[i] = 'a'
		}
	}
	r.n += int64(len(p))
	return len(p), nil
}

// TestAdminMirrorArmBoundsBodySize pins that arming cannot be made to read an
// unbounded request body into memory. The body here never ends -- see
// neverEndingJSONString -- so if armMirror ever went back to decoding
// directly off r.Body with no cap, this test would hang rather than fail
// cleanly; the goroutine+timeout below turns that hang into a reported
// failure instead of wedging the suite. On a correct handler, decoding fails
// once the bound is hit, the handler returns quickly with 400, and the number
// of bytes actually read from the body stays small and fixed regardless of
// how long the attacker is willing to keep sending data.
func TestAdminMirrorArmBoundsBodySize(t *testing.T) {
	reg := NewMirrorRegistry()
	h := NewAdminHandler(reg)

	body := &neverEndingJSONString{}
	req := httptest.NewRequest(http.MethodPost, "/admin/mirror", body)
	req.Host = "127.0.0.1:9470" // httptest's default, example.com, is not an admin host
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return within 5s: it is reading an unbounded request body")
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("a rejected request must not arm anything: %+v", got)
	}
	// No byte-count assertion here: with a reader that never terminates on
	// its own, there is no outcome between "capped correctly, so this point
	// is reached with a small body.n" and "not capped, so the goroutine
	// above never finishes and the timeout above already failed the test" --
	// any threshold on body.n at this point can only ever be checked once
	// the first of those is already true, so it can never itself catch a
	// regression. See TestAdminMirrorArmBoundsBodySizeExactly for a
	// byte-count assertion that actually can fail on its own, using a large
	// but finite body.
}

// TestAdminMirrorArmBoundsBodySizeExactly complements
// TestAdminMirrorArmBoundsBodySize's hang detection with a byte-count
// assertion that can actually fail by itself. Its predecessor's body never
// terminates on its own, so by the time its "did we read too much" check
// would run, either the cap already stopped it (small, unremarkable count)
// or the goroutine is still blocked forever and the test already failed on
// the timeout -- there is no path through that test where a broken, unbounded
// read shows up as a byte-count failure. Here the body is large (1 MiB, far
// past maxArmBodyBytes) but finite, so an uncapped handler would still
// finish reading it and return -- just after consuming far more than it
// should have -- and this test can catch exactly that.
func TestAdminMirrorArmBoundsBodySizeExactly(t *testing.T) {
	reg := NewMirrorRegistry()
	h := NewAdminHandler(reg)

	huge := `{"router":"` + strings.Repeat("a", 1<<20) + `"}`
	body := &countingReader{r: strings.NewReader(huge)}
	req := httptest.NewRequest(http.MethodPost, "/admin/mirror", body)
	req.Host = "127.0.0.1:9470" // httptest's default, example.com, is not an admin host
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	// Generous slack over the exact cap for buffering, but nowhere near the
	// full 1 MiB+ body -- a handler that reads the whole thing before
	// rejecting it fails this, even though (unlike
	// TestAdminMirrorArmBoundsBodySize's never-ending reader) it still
	// returns promptly either way.
	const slack = 4 << 10
	if body.n > maxArmBodyBytes+slack {
		t.Fatalf("handler read %d bytes from a %d-byte body; it must stop within "+
			"%d bytes of the %d-byte cap, not read the whole thing", body.n, len(huge), slack, maxArmBodyBytes)
	}
}

// countingReader wraps an io.Reader and records how many bytes have been
// read through it, so a test can assert on exactly how much of a body a
// handler consumed rather than only whether it returned.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestAdminMirrorMethodNotAllowed pins that an unsupported method on either
// route is rejected with 405, not treated as a match for some other verb's
// handler (which, for the collection route, would otherwise risk silently
// arming or listing on the wrong verb).
func TestAdminMirrorMethodNotAllowed(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/admin/mirror"},
		{http.MethodDelete, "/admin/mirror"},
		{http.MethodPatch, "/admin/mirror/"},
		{http.MethodPost, "/admin/mirror/10.0.0.1"},
		{http.MethodGet, "/admin/mirror/10.0.0.1"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req, err := http.NewRequest(c.method, srv.URL+c.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("status %d, want 405", resp.StatusCode)
			}
		})
	}
}

// TestAdminMirrorDisarmRejectsBadRouter pins that DELETE never panics or
// silently no-ops on a path segment that isn't an IP address -- it must
// report 400, the same contract as the arm path's router validation.
func TestAdminMirrorDisarmRejectsBadRouter(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	for _, path := range []string{
		"/admin/mirror/not-an-ip",
		"/admin/mirror/",
		"/admin/mirror/10.0.0.1/extra",
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestAdminMirrorDisarmRejectsDirtyPaths pins that a "//" or ".." path
// segment gets a direct 400, not net/http.ServeMux's own 301-to-cleaned-path
// redirect. The redirect itself is harmless -- Disarm is never reached on
// either request -- but Go's http.Client rewrites a redirected DELETE to
// GET, so the caller would otherwise see a confusing 405 for what is really
// a malformed router path.
func TestAdminMirrorDisarmRejectsDirtyPaths(t *testing.T) {
	reg := NewMirrorRegistry()
	if _, err := reg.Arm(netip.MustParseAddr("10.0.0.1"), time.Minute, 1000); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	for _, path := range []string{
		"/admin/mirror//10.0.0.1",
		"/admin/mirror/../mirror/10.0.0.1",
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
		})
	}
	// Neither dirty-path request may have reached Disarm.
	if got := reg.List(); len(got) != 1 {
		t.Fatalf("a dirty path must never reach Disarm: %+v", got)
	}
}

// TestAdminMirrorRequiresJSONContentType pins that a cross-origin HTML
// form with enctype="text/plain" is a CORS-simple request -- no preflight, so
// CORS headers never come into play -- and a single field whose *name* is the
// desired JSON body serializes to exactly this shape (the field name
// followed by a trailing "=" for its empty value). Nothing about that body is
// distinguishable from a legitimate client's JSON except the Content-Type an
// HTML form is capable of sending, so that is what must be checked.
func TestAdminMirrorRequiresJSONContentType(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	body := `{"router":"10.0.0.1","window":"24h","max_bytes":9999999}=`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/mirror", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415 (a text/plain body, exactly what a cross-origin HTML form can send, must be rejected)", resp.StatusCode)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("a text/plain form body must never arm anything: %+v", got)
	}
}

// TestAdminMirrorAcceptsContentTypeWithParams pins that a Content-Type
// header carrying parameters (as a real JSON client, e.g. one that always
// sets a charset, commonly sends) is still accepted -- the content-type
// check must parse the media type, not compare the raw header string.
func TestAdminMirrorAcceptsContentTypeWithParams(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/mirror",
		strings.NewReader(`{"router":"10.0.0.1","window":"10m","max_bytes":100}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (Content-Type parameters must not cause rejection)", resp.StatusCode)
	}
}

// TestAdminMirrorRejectsTrailingData pins the check alongside content-type
// validation: json.Decoder.Decode reads exactly one JSON value and
// silently ignores anything after it. Both cases below are a
// correctly-Content-Typed request whose body is nonetheless not "one JSON
// object and nothing else" -- the second is exactly what the text/plain
// form body from TestAdminMirrorRequiresJSONContentType would decode to if
// a caller ever forged the right Content-Type around it (a JSON object
// followed by "=" is no more valid than the two-objects case here), which
// is why this check has to be independent of the Content-Type gate above,
// not a substitute for it.
func TestAdminMirrorRejectsTrailingData(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	for _, c := range []struct{ name, body string }{
		{"garbage after value", `{"router":"10.0.0.1","window":"24h","max_bytes":9999999}garbage`},
		{"trailing equals from a forged form body", `{"router":"10.0.0.1","window":"24h","max_bytes":9999999}=`},
		{"two concatenated objects", `{"router":"10.0.0.1","window":"1m","max_bytes":100}{"router":"10.0.0.2","window":"1m","max_bytes":100}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/mirror", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", resp.StatusCode)
			}
			if got := reg.List(); len(got) != 0 {
				t.Fatalf("trailing data after the JSON value must not arm anything: %+v", got)
			}
		})
	}
}

// TestAdminMirrorRejectsUnknownFields pins the JSON path to the same
// discipline config.go's YAML loader applies via KnownFields(true): an
// unrecognized field is a hard error, not silently ignored.
func TestAdminMirrorRejectsUnknownFields(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/mirror",
		strings.NewReader(`{"router":"10.0.0.1","window":"10m","max_bytes":100,"nonsense":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("a rejected request must not arm anything: %+v", got)
	}
}

// TestAdminMirrorArmDoesNotReadBack pins that arming with a window
// short enough to have already expired by the time the handler responds
// must still return 200 with the status Arm produced, not a 500 from a
// List() search that can no longer find the (legitimately, correctly)
// already-reaped entry.
func TestAdminMirrorArmDoesNotReadBack(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/admin/mirror",
		strings.NewReader(`{"router":"10.0.0.1","window":"1ns","max_bytes":100}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("arm with an already-expired window: status %d, want 200", resp.StatusCode)
	}
	var got MirrorStatus
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Router != "10.0.0.1" || got.BytesRemaining != 100 {
		t.Fatalf("armed = %+v, want Router=10.0.0.1 BytesRemaining=100", got)
	}
}

// TestAdminMirrorDisarmCanonicalizesAddress pins, at the HTTP layer, the
// exact operator-facing symptom: arming 10.0.0.1 and then
// issuing DELETE against its IPv4-mapped form must actually disarm it, not
// return 204 while the mirror stays armed and keeps consuming the shared raw
// stream.
func TestAdminMirrorDisarmCanonicalizesAddress(t *testing.T) {
	reg := NewMirrorRegistry()
	srv := httptest.NewServer(NewAdminHandler(reg))
	defer srv.Close()

	body := strings.NewReader(`{"router":"10.0.0.1","window":"10m","max_bytes":1048576}`)
	resp, err := http.Post(srv.URL+"/admin/mirror", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("arm: status %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/admin/mirror/::ffff:10.0.0.1", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disarm via IPv4-mapped form: status %d, want 204", resp.StatusCode)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("still armed after disarming via the IPv4-mapped form: %+v", got)
	}
}

// TestAdminRejectsForeignHost is the DNS rebinding defense. A page on
// attacker.example can re-point its own name at 127.0.0.1 after load, and its
// requests to attacker.example:9470 then reach this loopback listener as
// same-origin -- which is exactly what the Content-Type check in armMirror
// relies on a browser NOT being. The one thing such a request cannot change
// is its Host header, which still names the attacker's domain.
func TestAdminRejectsForeignHost(t *testing.T) {
	for _, tc := range []struct {
		host   string
		listen []string
		want   bool
	}{
		{"127.0.0.1:9470", nil, true},
		{"[::1]:9470", nil, true},
		{"localhost:9470", nil, true},
		{"LOCALHOST", nil, true},
		// Any IP literal: a browser only sends one to the address it names,
		// so it cannot be a rebound name. This is how an operator reaches a
		// collector bound to 0.0.0.0 from another host.
		{"10.0.0.80:9470", []string{"0.0.0.0:9470"}, true},
		{"[2001:db8::1]:9470", nil, true},
		{"collector.lab:9470", []string{"collector.lab:9470"}, true},
		{"Collector.Lab", []string{"collector.lab:9470"}, true},

		{"attacker.example:9470", nil, false},
		{"attacker.example", []string{"0.0.0.0:9470"}, false},
		{"collector.lab:9470", nil, false},
		{"127.0.0.1.nip.io:9470", nil, false},
		{"localhost.attacker.example", nil, false},
		{"", nil, false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			reg := NewMirrorRegistry()
			h := NewAdminHandler(reg, tc.listen...)
			req := httptest.NewRequest(http.MethodPost, "/admin/mirror",
				strings.NewReader(`{"router":"10.0.0.1","window":"1m","max_bytes":1024}`))
			req.Host = tc.host
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.want {
				if rec.Code != http.StatusOK || len(reg.List()) != 1 {
					t.Fatalf("Host %q: status %d, armed %d; want 200 and one mirror", tc.host, rec.Code, len(reg.List()))
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Host %q: status %d, want 403", tc.host, rec.Code)
			}
			if got := reg.List(); len(got) != 0 {
				t.Fatalf("Host %q armed a mirror: %+v", tc.host, got)
			}
			// GET too: a rebound page could otherwise read the mirror list.
			req = httptest.NewRequest(http.MethodGet, "/admin/mirror", nil)
			req.Host = tc.host
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("GET with Host %q: status %d, want 403", tc.host, rec.Code)
			}
		})
	}
}
