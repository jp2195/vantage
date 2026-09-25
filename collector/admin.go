// admin.go serves the collector's control surface: arming, listing and
// disarming the per-router mirrors that vantage capture drives.
//
// This runs on its own listener rather than joining the Prometheus mux,
// because mirroring another router's BMP traffic is a real capability and
// metrics ports get exposed broadly -- to Prometheus, dashboards, sidecars.
// In Kubernetes, bind this to localhost or gate it with a NetworkPolicy.
// config.go's default for admin_listen is already loopback-only
// (127.0.0.1:9470) for exactly this reason -- an operator who wants it
// reachable off-box sets admin_listen explicitly.
package collector

import (
	"encoding/json"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"time"
)

// maxArmBodyBytes bounds how much of a POST /admin/mirror request body this
// handler will ever read. A real request is under 100 bytes (an IP address,
// a short duration string, and an integer); this is generous headroom above
// that, not a sized-to-fit limit. Without it, json.Decode reading straight
// off r.Body has no bound at all -- a client that sends a body containing one
// enormous JSON string value (or simply never stops sending) would make the
// decoder buffer an unbounded amount of memory, or block the handler
// goroutine forever, before validation ever gets a chance to reject it. This
// is what turns "arm a mirror" from a convenience endpoint into a surface
// that must survive a hostile or merely broken caller.
const maxArmBodyBytes = 4 << 10 // 4 KiB

type armRequest struct {
	Router   string `json:"router"`
	Window   string `json:"window"` // Go duration text, e.g. "10m"
	MaxBytes int64  `json:"max_bytes"`
}

// NewAdminHandler returns the admin mux bound to m. adminListen is the
// configured admin_listen address (host:port); its host, when it is a name
// rather than an IP, is added to the Host headers the handler accepts -- see
// checkHost. Omitting it accepts only IP literals and localhost.
func NewAdminHandler(m *MirrorRegistry, adminListen ...string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/mirror", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			armMirror(w, r, m)
		case http.MethodGet:
			writeJSON(w, http.StatusOK, m.List())
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/mirror/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw := strings.TrimPrefix(r.URL.Path, "/admin/mirror/")
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			http.Error(w, "router must be an IP address", http.StatusBadRequest)
			return
		}
		m.Disarm(addr)
		w.WriteHeader(http.StatusNoContent)
	})
	return checkHost(rejectDirtyPaths(mux), adminListen)
}

// checkHost answers 403 to a request whose Host header is not an IP literal,
// localhost, or the host of a configured admin_listen address.
//
// This is the DNS rebinding defense. The admin API's other browser defenses
// (the Content-Type check in armMirror, and never answering a CORS preflight)
// both rest on a browser treating a page's request to this port as
// cross-origin. A page on attacker.example can undo that by re-pointing its
// own name at 127.0.0.1 after it loads: its requests to attacker.example:9470
// then reach this loopback listener and are same-origin, so the browser lets
// the page set any header and read every response. What the page cannot
// change is the Host header, which still names its own domain.
//
// Any IP literal is accepted, not only loopback: a browser sends one only to
// the address it names, so it cannot be a rebound name, and it is how an
// operator reaches a collector whose admin_listen is 0.0.0.0 from another
// machine. localhost is resolved by the client's own host and cannot be
// rebound either.
func checkHost(next http.Handler, adminListen []string) http.Handler {
	names := map[string]bool{"localhost": true}
	for _, l := range adminListen {
		if h, _, err := net.SplitHostPort(l); err == nil && h != "" {
			if _, err := netip.ParseAddr(h); err != nil {
				names[strings.ToLower(h)] = true
			}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if _, err := netip.ParseAddr(host); err != nil && !names[strings.ToLower(host)] {
			http.Error(w, "unrecognized Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectDirtyPaths returns 400 for a request whose path contains a "//" or
// ".." segment, before ServeMux gets a chance to see it. Left to ServeMux's
// own handling, such a path is 301-redirected to its cleaned form -- safe on
// its own (Disarm is never reached), but Go's http.Client rewrites a
// redirected DELETE to GET, so the caller ends up seeing a confusing 405
// instead of the 400 an invalid router path deserves. Intercepting here,
// ahead of routing, answers with the real status directly instead of
// depending on how a given client happens to follow redirects.
func rejectDirtyPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cleanPath(r.URL.Path) != r.URL.Path {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cleanPath mirrors net/http.ServeMux's own notion of a "dirty" path
// (unexported there): path.Clean, plus preserving a trailing slash, which
// path.Clean otherwise strips and which is meaningful to ServeMux as a
// subtree pattern ("/admin/mirror/"). Comparing against plain path.Clean
// would flag every trailing-slash request as dirty, which is not what
// ServeMux itself considers a path worth redirecting.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

func armMirror(w http.ResponseWriter, r *http.Request, m *MirrorRegistry) {
	// A cross-origin HTML form (enctype="text/plain", a field name that is
	// itself the desired JSON body) is a CORS-simple request: no preflight,
	// so CORS headers never come into play, and the body it produces
	// (the field name followed by "=") is otherwise indistinguishable from a
	// real client's JSON. Requiring Content-Type: application/json closes
	// that hole -- an HTML form cannot set an arbitrary Content-Type value,
	// and a fetch() call that does set one triggers a real preflight, which
	// this mux never answers with CORS headers, so the browser blocks the
	// response before the body is ever sent. This is what admin.go's own
	// package comment above is actually relying on when it recommends
	// binding to localhost: localhost is exactly what a browser reaches, so
	// the origin check has to happen here, not at the network boundary.
	// Content-Type can carry parameters ("application/json; charset=utf-8"),
	// so this parses it with mime.ParseMediaType rather than comparing the
	// raw header string.
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	// Cap the body before decoding it, not after: MaxBytesReader makes every
	// subsequent Read on r.Body fail once the limit is crossed, so the
	// decoder below can never buffer more than maxArmBodyBytes no matter what
	// the caller sends or how long they send it for.
	r.Body = http.MaxBytesReader(w, r.Body, maxArmBodyBytes)

	dec := json.NewDecoder(r.Body)
	// config.go's YAML loader uses KnownFields(true) so a typo'd key is a
	// hard error rather than silently keeping a default in place; the same
	// discipline applies here.
	dec.DisallowUnknownFields()
	var req armRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}
	// json.Decoder.Decode reads exactly one JSON value off the stream and
	// silently ignores anything after it. A body of two concatenated JSON
	// objects, or a well-formed object followed by garbage -- both exactly
	// what the text/plain form attack above produces once its trailing "="
	// is accounted for -- would otherwise decode the first value, arm on it,
	// and never report that the rest of the body was nonsense.
	if dec.More() {
		http.Error(w, "trailing data after JSON value", http.StatusBadRequest)
		return
	}

	addr, err := netip.ParseAddr(req.Router)
	if err != nil {
		http.Error(w, "router must be an IP address", http.StatusBadRequest)
		return
	}
	window, err := time.ParseDuration(req.Window)
	if err != nil {
		http.Error(w, "window must be a Go duration, e.g. \"10m\"", http.StatusBadRequest)
		return
	}
	// MirrorRegistry.Arm is the one place window and max_bytes bounds (and
	// address canonicalization) are enforced; this handler surfaces whatever
	// it rejects as a 400 rather than re-checking those bounds itself, and
	// returns the MirrorStatus Arm itself produced rather than searching
	// List() for it afterward -- List can legitimately have already reaped
	// the same entry (see Arm's doc comment), which would otherwise turn a
	// request processed exactly as designed into a 500.
	status, err := m.Arm(addr, window, req.MaxBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
