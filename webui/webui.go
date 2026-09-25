// Package webui holds the built single-page application and serves it.
//
// The build output lives inside this package rather than beside the Vite
// source because go:embed cannot reach outside its own directory. It is a
// separate package from api/ so that a build artifact never lands in the
// package that holds the auth gate.
//
// dist/.gitkeep is committed and dist/ is otherwise gitignored. That is
// load-bearing: `go build ./...` has to succeed on a checkout where Node was
// never installed, and //go:embed fails to COMPILE against a directory that
// does not exist. The .gitkeep guarantees the directory is always there, and
// the missing index.html is handled at runtime instead.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// FS is the built application, rooted so that "index.html" is at the top.
var FS fs.FS = mustSub()

func mustSub() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// Unreachable: dist/ is embedded above, so Sub cannot fail. A
		// panic here would mean the embed directive and this path
		// disagree, which is a build-time mistake, not a runtime one.
		panic("webui: dist subtree missing from embedded FS: " + err.Error())
	}
	return sub
}

func indexPresent() bool {
	_, err := fs.Stat(FS, "index.html")
	return err == nil
}

// assetPrefix is where Vite writes its content-hashed bundles (its default
// build.assetsDir, under the outDir configured in ui/vite.config.ts). It is
// the whole basis of cacheControl's split below: a name under here contains
// a hash of the bytes it names, so the bytes at that URL cannot change.
const assetPrefix = "/assets/"

// Handler serves the built assets, falling back to index.html for any path
// that does not name a file. The fallback is what makes client-side routing
// work: a deep link to /peers is not a file, and must return the app rather
// than a 404 so the router can resolve it in the browser.
//
// When no build is present it says so in plain words. A 404 there would read
// as a routing defect; the actual situation is that the reader has not run
// `make ui`, and the response should tell them that.
func Handler() http.Handler {
	files := http.FileServer(http.FS(FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !indexPresent() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("vantage: the web UI is not built into this binary.\nRun `make ui` and rebuild, or use the API at /v1/.\n"))
			return
		}
		if _, err := fs.Stat(FS, cleanPath(r.URL.Path)); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// AFTER the fallback rewrite, deliberately. A request for
		// /assets/gone.js that no longer exists has just been rewritten to
		// "/" and is about to be answered with index.html; labeling that
		// response immutable because of the path the client ASKED for
		// would cache the app shell under an asset URL for a year.
		w.Header().Set("Cache-Control", cacheControl(r.URL.Path))
		files.ServeHTTP(w, r)
	})
}

// cacheControl is the caching rule for a path that has already been
// resolved to a real file, or rewritten to "/" by Handler's fallback.
//
// This deliberately set no Cache-Control at all at first, deferring the
// question until Vite produced content-hashed filenames; it does now.
// Without it a browser re-fetches the whole bundle on every load, because
// embed.FS reports a zero modification time and so http.ServeContent emits no
// Last-Modified validator either -- there is nothing for a conditional
// request to be conditional on.
//
// The split is the standard one and it is not symmetric. Everything under
// assetPrefix carries a content hash in its own name, so those bytes can
// never change under that URL and a year plus immutable says exactly that:
// no request at all, not even a revalidation. index.html is the document
// that NAMES those hashed chunks and its URL never changes, so a cached
// copy that outlives a deploy points at bundles the new binary does not
// have -- a blank page fixed only by a hard reload. It gets no-cache, which
// forbids reuse without revalidating. Here that revalidation is a full
// re-fetch rather than a 304: embed.FS's zero mod time means
// http.ServeContent sends no Last-Modified, and net/http generates no ETag
// of its own (it only honors one a handler already set), so the browser has
// no validator to send back. That is the right trade for a two-kilobyte
// document whose only alternative failure is a broken page.
func cacheControl(path string) string {
	if strings.HasPrefix(path, assetPrefix) {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

// cleanPath turns a request path into the FS path fs.Stat wants: no leading
// slash, and "." for the root. It does not need to defend against traversal
// -- http.FileServer already rejects that -- it only has to answer the
// question "is this a real file", so that anything else can fall back.
func cleanPath(p string) string {
	if len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	if p == "" {
		return "."
	}
	return p
}
