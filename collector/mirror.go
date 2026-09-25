package collector

import (
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// maxMirrorWindow bounds how long a single mirror may stay armed. The raw
// stream is 24h/R1 and sized for anomalies, not for full sessions, so a mirror
// that outlives its usefulness would evict the very anomalies the stream exists
// to keep.
const maxMirrorWindow = 24 * time.Hour

// MirrorStatus is one armed mirror, as reported by the admin API.
type MirrorStatus struct {
	Router         string    `json:"router"`
	ExpiresAt      time.Time `json:"expires_at"`
	BytesRemaining int64     `json:"bytes_remaining"`
}

type mirrorEntry struct {
	expiresAt      time.Time
	bytesRemaining int64
}

// MirrorRegistry tracks which routers are currently being mirrored into the
// raw stream, and enforces the time and byte bounds that keep that stream
// usable. Safe for concurrent use: Take is called from every connection
// goroutine while the admin API arms and disarms.
type MirrorRegistry struct {
	mu  sync.Mutex
	m   map[netip.Addr]*mirrorEntry
	now func() time.Time
}

func NewMirrorRegistry() *MirrorRegistry {
	return &MirrorRegistry{m: map[netip.Addr]*mirrorEntry{}, now: time.Now}
}

// canonicalRouterAddr normalizes router to the single form MirrorRegistry
// keys its map by. Arm, Disarm and Take each call this before touching r.m,
// so a router can never be armed under one spelling of its address and
// disarmed (or matched by Take) under another -- config.go's validate() doc
// comment names exactly this failure mode ("::ffff:10.0.0.1") for router
// overrides, matched by exact string equality; the admin API takes an
// operator-supplied string and must not reintroduce it. Unmap collapses an
// IPv4-mapped IPv6 address ("::ffff:10.0.0.1") to plain IPv4, matching what
// server.go's ap.Addr().Unmap() always produces for a real connection.
// WithZone("") strips a zone ID ("fe80::1%eth0"), which is meaningful for
// routing a packet but not for naming which router is being mirrored.
func canonicalRouterAddr(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}

// Arm starts mirroring router for at most window or maxBytes, whichever is
// exhausted first, and returns the status it just created. Both bounds are
// required: an unbounded mirror is the one way this feature can damage
// production capture. Callers must use the returned MirrorStatus directly
// rather than re-deriving it via List(): List can legitimately have already
// reaped this exact entry (a sub-window that expires before List's own
// check runs, or -- via a concurrent Take -- a max_bytes budget exhausted by
// the router's very first message) before the caller gets a chance to look
// it up.
func (r *MirrorRegistry) Arm(router netip.Addr, window time.Duration, maxBytes int64) (MirrorStatus, error) {
	router = canonicalRouterAddr(router)
	if !router.IsValid() {
		return MirrorStatus{}, fmt.Errorf("mirror: router address is invalid")
	}
	if window <= 0 || window > maxMirrorWindow {
		return MirrorStatus{}, fmt.Errorf("mirror: window must be > 0 and <= %v, got %v", maxMirrorWindow, window)
	}
	if maxBytes <= 0 {
		return MirrorStatus{}, fmt.Errorf("mirror: max_bytes must be > 0, got %d", maxBytes)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	expiresAt := r.now().Add(window)
	r.m[router] = &mirrorEntry{expiresAt: expiresAt, bytesRemaining: maxBytes}
	metricMirrorActive.Set(float64(len(r.m)))
	return MirrorStatus{Router: router.String(), ExpiresAt: expiresAt, BytesRemaining: maxBytes}, nil
}

// Disarm stops mirroring router. Disarming an unarmed router is not an error.
func (r *MirrorRegistry) Disarm(router netip.Addr) {
	router = canonicalRouterAddr(router)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, router)
	metricMirrorActive.Set(float64(len(r.m)))
}

// Take reports whether a message of n bytes from router should be mirrored,
// charging it against the budget. A mirror that has run out of time or bytes
// is removed here rather than by a background sweeper, so the bound is enforced
// at the only place it matters.
func (r *MirrorRegistry) Take(router netip.Addr, n int) bool {
	router = canonicalRouterAddr(router)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[router]
	if !ok {
		return false
	}
	if !r.now().Before(e.expiresAt) || e.bytesRemaining < int64(n) {
		delete(r.m, router)
		metricMirrorActive.Set(float64(len(r.m)))
		return false
	}
	e.bytesRemaining -= int64(n)
	return true
}

// List reports the currently armed mirrors, dropping any that have expired.
func (r *MirrorRegistry) List() []MirrorStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]MirrorStatus, 0, len(r.m))
	for addr, e := range r.m {
		if !now.Before(e.expiresAt) {
			delete(r.m, addr)
			continue
		}
		out = append(out, MirrorStatus{
			Router: addr.String(), ExpiresAt: e.expiresAt, BytesRemaining: e.bytesRemaining,
		})
	}
	metricMirrorActive.Set(float64(len(r.m)))
	return out
}
