package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// TestFSAlwaysEmbeds is the guard for the bare-checkout constraint: the
// embed must compile and produce a usable fs.FS even when no Vite build has
// ever run, because `go build ./...` has to work without Node installed.
func TestFSAlwaysEmbeds(t *testing.T) {
	if _, err := fs.Stat(FS, "."); err != nil {
		t.Fatalf("embedded FS is not usable: %v", err)
	}
}

// TestHandlerReportsUnbuiltUI covers the case a Go-only developer actually
// hits. Without this the handler would serve a 404 for "/", which reads as a
// routing bug rather than "you have not run make ui".
func TestHandlerReportsUnbuiltUI(t *testing.T) {
	if indexPresent() {
		t.Skip("dist/index.html is present; this test covers the unbuilt tree")
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if body := rec.Body.String(); !strings.Contains(body, "make ui") {
		t.Errorf("body %q does not tell the reader to run make ui", body)
	}
}

// TestHandlerFallsBackToIndexForDeepLinks is the SPA-routing half of
// Handler(), which TestHandlerReportsUnbuiltUI cannot reach: this
// repository's own dist/ has no index.html (it is not built until the
// Vite build step runs), so the fallback logic that matters once a build
// exists needs a build to exercise it. This test supplies one in memory, by
// swapping the package-level FS for the duration of the test, so the
// fallback is proven correct now rather than deferred to whenever a real
// build happens to be present.
func TestHandlerFallsBackToIndexForDeepLinks(t *testing.T) {
	orig := FS
	t.Cleanup(func() { FS = orig })
	FS = fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>app shell</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('app')")},
	}

	// A deep link with no matching file gets the app shell, not a 404, so
	// the client-side router can resolve it in the browser.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/peers/65000", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /peers/65000: status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "app shell") {
		t.Errorf("GET /peers/65000: body %q does not contain the app shell", body)
	}

	// A real asset is served as itself, not swallowed by the fallback.
	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /assets/app.js: status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "console.log") {
		t.Errorf("GET /assets/app.js: body %q is not the asset's own content", body)
	}
}

// TestCachingHeadersSplitHashedAssetsFromTheDocument covers the rule
// cacheControl states: the hashed bundles may be cached forever, the
// document that names them may not be cached at all.
//
// Both halves are asserted because getting either one wrong is a real
// failure with no error message. immutable on index.html means an operator
// who deploys a fix keeps seeing the old app until a hard reload;
// no-cache on the hashed assets means every page load re-downloads the
// whole bundle over a link that may be a lab VPN.
//
// The /assets/ path that does NOT exist is the case the ordering inside
// Handler exists for: it is rewritten to index.html by the fallback, so it
// must be labeled the way index.html is and not the way its requested
// path looks.
func TestCachingHeadersSplitHashedAssetsFromTheDocument(t *testing.T) {
	orig := FS
	t.Cleanup(func() { FS = orig })
	// Named the way Vite names them, hash included, so the file this test
	// caches forever is the same shape as the real one.
	FS = fstest.MapFS{
		"index.html":              &fstest.MapFile{Data: []byte("<html>app shell</html>")},
		"assets/index-B7cQx1.js":  &fstest.MapFile{Data: []byte("console.log('app')")},
		"assets/index-C3d9f2.css": &fstest.MapFile{Data: []byte(".shell{}")},
	}

	const immutable = "public, max-age=31536000, immutable"
	for _, tc := range []struct{ name, path, want string }{
		{"hashed script", "/assets/index-B7cQx1.js", immutable},
		{"hashed stylesheet", "/assets/index-C3d9f2.css", immutable},
		{"the document itself", "/", "no-cache"},
		{"a deep link, served as the document", "/peers/65000", "no-cache"},
		{"an asset a later deploy removed", "/assets/gone-A0b1c2.js", "no-cache"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200", tc.path, rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("GET %s: Cache-Control = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	// And the reason no-cache is doing the work alone: embed.FS reports a
	// zero mod time, so http.ServeContent sends no Last-Modified, and
	// net/http never invents an ETag. fstest.MapFile has the same zero mod
	// time, so this is the served response's real shape, not a stand-in.
	// If a future change adds a validator, no-cache starts allowing 304s
	// and this assertion is the one to delete deliberately.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if lm := rec.Header().Get("Last-Modified"); lm != "" {
		t.Errorf("Last-Modified = %q; embed.FS has no mod time to report", lm)
	}
	if et := rec.Header().Get("Etag"); et != "" {
		t.Errorf("Etag = %q; net/http does not generate one and this handler sets none", et)
	}
}
