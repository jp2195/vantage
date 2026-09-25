package collector

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
)

func TestMirrorArmAndTake(t *testing.T) {
	r := NewMirrorRegistry()
	router := netip.MustParseAddr("10.0.0.1")

	if r.Take(router, 100) {
		t.Fatal("an unarmed router must not be mirrored")
	}
	status, err := r.Arm(router, time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Arm must return the status it created directly, not force the caller
	// to re-derive it via List() -- List can legitimately have already
	// reaped the very entry Arm just created (a sub-window that expires
	// before List's own check, or a first message that exhausts max_bytes
	// before List ever runs), which would otherwise turn a request processed
	// exactly as designed into a spurious error.
	if status.Router != "10.0.0.1" || status.BytesRemaining != 1000 {
		t.Fatalf("Arm returned %+v, want Router=10.0.0.1 BytesRemaining=1000", status)
	}
	if !r.Take(router, 100) {
		t.Fatal("an armed router must be mirrored")
	}
	if got := r.List(); len(got) != 1 || got[0].Router != "10.0.0.1" {
		t.Fatalf("List = %+v", got)
	}
	r.Disarm(router)
	if r.Take(router, 100) {
		t.Fatal("a disarmed router must not be mirrored")
	}
}

// TestMirrorCanonicalizesIPv4MappedAddress pins that Arm/Disarm/Take treat
// 10.0.0.1 and ::ffff:10.0.0.1 as the same router. server.go derives the
// router address via ap.Addr().Unmap(), so it is always the plain IPv4 form
// -- but the admin API takes an operator-supplied string, and nothing stops
// an operator (or a client library that normalizes addresses differently)
// from writing the IPv4-mapped form. Without canonicalizing here, arming via
// one form and disarming via the other silently fails to disarm, and arming
// via the mapped form arms an entry Take (called with the plain form by
// server.go) can never match -- an inert mirror that still holds a registry
// slot and inflates vantage_collector_mirror_active.
func TestMirrorCanonicalizesIPv4MappedAddress(t *testing.T) {
	plain := netip.MustParseAddr("10.0.0.1")
	mapped := netip.MustParseAddr("::ffff:10.0.0.1")

	t.Run("disarm via mapped form disarms the plain-armed entry", func(t *testing.T) {
		r := NewMirrorRegistry()
		if _, err := r.Arm(plain, time.Minute, 1000); err != nil {
			t.Fatal(err)
		}
		r.Disarm(mapped)
		if r.Take(plain, 10) {
			t.Fatal("disarming via the IPv4-mapped form must disarm the canonical entry")
		}
		if got := r.List(); len(got) != 0 {
			t.Fatalf("List = %+v, want empty after disarm", got)
		}
	})

	t.Run("disarm via plain form disarms the mapped-armed entry", func(t *testing.T) {
		r := NewMirrorRegistry()
		if _, err := r.Arm(mapped, time.Minute, 1000); err != nil {
			t.Fatal(err)
		}
		r.Disarm(plain)
		if r.Take(plain, 10) {
			t.Fatal("disarming via the plain form must disarm an entry armed via the mapped form")
		}
	})

	t.Run("arming the mapped form fires for the plain form", func(t *testing.T) {
		r := NewMirrorRegistry()
		status, err := r.Arm(mapped, time.Minute, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if status.Router != plain.String() {
			t.Fatalf("Arm(%s) returned Router=%q, want canonical %q", mapped, status.Router, plain.String())
		}
		if !r.Take(plain, 10) {
			t.Fatal("arming the IPv4-mapped form must arm the same entry Take(plain) fires for")
		}
		if got := r.List(); len(got) != 1 || got[0].Router != plain.String() {
			t.Fatalf("List = %+v, want canonical %s", got, plain)
		}
	})
}

// TestMirrorCanonicalizesZonedAddress pins that a zoned link-local address
// (e.g. "fe80::1%eth0", as a client library might produce when reporting its
// own interface-scoped address) arms the same entry the unzoned form's Take
// fires for.
func TestMirrorCanonicalizesZonedAddress(t *testing.T) {
	zoned := netip.MustParseAddr("fe80::1%eth0")
	plain := zoned.WithZone("")

	r := NewMirrorRegistry()
	status, err := r.Arm(zoned, time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if status.Router != plain.String() {
		t.Fatalf("Arm(%s) returned Router=%q, want zone-stripped %q", zoned, status.Router, plain.String())
	}
	if !r.Take(plain, 10) {
		t.Fatal("arming a zoned address must arm the entry Take(unzoned) fires for")
	}
	r.Disarm(zoned)
	if r.Take(plain, 10) {
		t.Fatal("disarming via the zoned form must disarm the canonical entry")
	}
}

// TestMirrorByteBudgetAutoDisarms pins the bound that protects the raw stream.
func TestMirrorByteBudgetAutoDisarms(t *testing.T) {
	r := NewMirrorRegistry()
	router := netip.MustParseAddr("10.0.0.1")
	if _, err := r.Arm(router, time.Minute, 250); err != nil {
		t.Fatal(err)
	}
	if !r.Take(router, 100) || !r.Take(router, 100) {
		t.Fatal("first two messages are within budget")
	}
	if r.Take(router, 100) {
		t.Fatal("third message exceeds the 250-byte budget and must not mirror")
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("an exhausted mirror must auto-disarm, still listed: %+v", got)
	}
}

func TestMirrorWindowExpires(t *testing.T) {
	r := NewMirrorRegistry()
	r.now = func() time.Time { return time.Unix(1000, 0) }
	router := netip.MustParseAddr("10.0.0.1")
	if _, err := r.Arm(router, 10*time.Second, 1<<20); err != nil {
		t.Fatal(err)
	}
	if !r.Take(router, 10) {
		t.Fatal("within the window")
	}
	r.now = func() time.Time { return time.Unix(1011, 0) }
	if r.Take(router, 10) {
		t.Fatal("past the window the mirror must be inactive")
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("an expired mirror must auto-disarm: %+v", got)
	}
}

func TestMirrorRejectsBadBounds(t *testing.T) {
	r := NewMirrorRegistry()
	router := netip.MustParseAddr("10.0.0.1")
	for _, c := range []struct {
		name   string
		window time.Duration
		bytes  int64
	}{
		{"zero window", 0, 100},
		{"negative window", -time.Second, 100},
		{"zero bytes", time.Minute, 0},
		{"window too long", 25 * time.Hour, 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := r.Arm(router, c.window, c.bytes); err == nil {
				t.Fatalf("Arm(%v, %d) must be rejected", c.window, c.bytes)
			}
		})
	}
}

func TestMirrorConcurrentTakeIsRaceFree(t *testing.T) {
	r := NewMirrorRegistry()
	router := netip.MustParseAddr("10.0.0.1")
	if _, err := r.Arm(router, time.Minute, 1<<30); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 200 {
				r.Take(router, 1)
				r.List()
			}
		})
	}
	wg.Wait()

	// Without this the test asserts nothing at all: it starts goroutines,
	// waits, and returns, so it means something only under -race. The budget
	// is the observable the mutex actually protects -- a lost update or a
	// double decrement across 3200 concurrent Takes shows up here and nowhere
	// else, and a data race is not guaranteed to be detected on any given run.
	got := r.List()
	if len(got) != 1 {
		t.Fatalf("List = %+v, want the one armed router", got)
	}
	if want := int64(1<<30) - 3200; got[0].BytesRemaining != want {
		t.Fatalf("BytesRemaining = %d after 16x200 one-byte Takes, want %d",
			got[0].BytesRemaining, want)
	}
}

// TestMirrorProducesMarkedRawEvents drives real BMP at a Server with a mirror
// armed and asserts the mirrored copies arrive marked, alongside the normal
// typed envelopes.
func TestMirrorProducesMarkedRawEvents(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	if _, err := srv.Mirrors.Arm(netip.MustParseAddr("127.0.0.1"), time.Minute, 1<<20); err != nil {
		t.Fatal(err)
	}

	conn := dial(t, ln)
	defer conn.Close()
	if _, err := conn.Write(bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(bmptest.Stats(peerHdr(), map[uint32]uint64{1: 1})); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mirrored := 0
		for _, ev := range pub.get() {
			if r := ev.Env.GetRaw(); r != nil && r.Mirrored {
				mirrored++
			}
		}
		if mirrored >= 2 { // the Init and the Stats
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected mirrored raw events, got %d events total", len(pub.get()))
}

// TestMirrorDoesNotDoublePublishAlreadyRawMessages pins a case unconditional
// mirror-hook placement gets wrong: putting the mirror hook unconditionally
// after the per-message publish loop double-publishes, because Session.Handle
// already publishes a RawEvent for any message it has no typed event for -- a
// Termination, a Route Mirroring message, or any parse failure. Mirroring
// those too puts the identical bytes on the raw subject twice under different
// msg-ids (rawSeq advances on every rawEvent call, so JetStream's dedup never
// catches it), silently doubling raw-stream occupancy for exactly the traffic
// mirror mode's byte budget exists to bound.
//
// Removing the guard leaves the rest of the suite green, so without this test
// the deviation is unfalsifiable and a later reader would be free to "restore"
// the unconditional hook and reintroduce the bug.
func TestMirrorDoesNotDoublePublishAlreadyRawMessages(t *testing.T) {
	pub := &capturePub{}
	srv := NewServer(Config{CollectorID: "c1"}, pub, time.Now)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go srv.Serve(ctx, ln)

	if _, err := srv.Mirrors.Arm(netip.MustParseAddr("127.0.0.1"), time.Minute, 1<<20); err != nil {
		t.Fatal(err)
	}

	conn := dial(t, ln)
	defer conn.Close()
	if _, err := conn.Write(bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2")); err != nil {
		t.Fatal(err)
	}
	// A Termination has no typed event, so Handle publishes it raw on its own.
	if _, err := conn.Write(bmptest.Termination(0)); err != nil {
		t.Fatal(err)
	}

	// Wait for the Init's mirrored copy, which orders behind nothing and tells
	// us the Termination that follows it on the same connection has been
	// processed too.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		term := 0
		for _, ev := range pub.get() {
			if r := ev.Env.GetRaw(); r != nil && isBMPType(r.BmpMsg, bmp.TypeTermination) {
				term++
			}
		}
		if term > 0 {
			// Give any erroneous duplicate a chance to land before asserting.
			time.Sleep(200 * time.Millisecond)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	term, mirroredTerm := 0, 0
	for _, ev := range pub.get() {
		r := ev.Env.GetRaw()
		if r == nil || !isBMPType(r.BmpMsg, bmp.TypeTermination) {
			continue
		}
		term++
		if r.Mirrored {
			mirroredTerm++
		}
	}
	if term != 1 {
		t.Fatalf("the Termination reached the raw stream %d times, want exactly 1 "+
			"(mirror mode must not re-publish what Handle already sent raw)", term)
	}
	if mirroredTerm != 0 {
		t.Fatalf("the single Termination raw event is marked mirrored; it should be " +
			"the one Handle published, not a mirror copy")
	}
}

// isBMPType reports whether raw is a BMP message of type typ. RawEvent carries
// the original message rather than a decoded type, and the common header is
// version(1) length(4) type(1), so the type is byte 5 (RFC 7854 §4.1).
func isBMPType(raw []byte, typ uint8) bool {
	return len(raw) > 5 && raw[5] == typ
}
