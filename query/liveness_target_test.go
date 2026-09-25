package query

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// TestTargetLivenessIsTheBeatAndASessionOfTheRunningProcess: a purge target is
// live only while BOTH hold -- the collector was heard from inside the
// threshold, and the target has a current session its running process opened.
// Each case here breaks exactly one half, or neither:
//
//	running  fresh beat; routers moved and kept both open  -> live
//	         .80.2's session closed cleanly, every peer     -> not live for that
//	         view_lost                                         router
//	         .80.3's session has one peer view_lost, one up -> live for that router
//	silent   old beat                                      -> not live
//	moved    fresh beat, but this router's only session    -> not live for that
//	         predates the process (the router went          router; live for the
//	         elsewhere and never came back)                 collector as a whole
//	unbeaten a current session, but no beat ever (a        -> not live, no beat
//	         version-1 collector)
//	unknown  never heard from                              -> not live, no beat
func TestTargetLivenessIsTheBeatAndASessionOfTheRunningProcess(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	run := time.Now().UnixNano()
	id := func(s string) string { return fmt.Sprintf("%starget-%s-%d", chtest.LivenessPrefix, s, run) }
	sid := livenessSID(800)
	started := time.Unix(0, int64(sid)).Add(-time.Second)
	running, silent, moved := id("running"), id("silent"), id("moved")

	livenessPeer(t, ctx, q, running, "10.251.80.1", "10.251.255.1", sid, "up")
	insertBeat(t, ctx, q, running, started, time.Now(), beatFresh)
	livenessPeer(t, ctx, q, running, "10.251.80.2", "10.251.255.1", sid, "view_lost")
	livenessPeer(t, ctx, q, running, "10.251.80.3", "10.251.255.1", sid, "view_lost")
	livenessPeer(t, ctx, q, running, "10.251.80.3", "10.251.255.2", sid, "up")
	livenessPeer(t, ctx, q, silent, "10.251.81.1", "10.251.255.1", sid, "up")
	insertBeat(t, ctx, q, silent, started, time.Now(), beatOld)
	// moved's process started AFTER its 10.251.82.1 session, which is
	// therefore a dead process's; its 10.251.82.2 session is the running
	// process's own.
	livenessPeer(t, ctx, q, moved, "10.251.82.1", "10.251.255.1", sid, "up")
	livenessPeer(t, ctx, q, moved, "10.251.82.2", "10.251.255.1", sid+uint64(2*time.Second), "up")
	insertBeat(t, ctx, q, moved, started.Add(2*time.Second), time.Now(), beatFresh)
	// unbeaten has a session and no beat: live's placeholder arm gives it a
	// row, which must not read as a beat.
	unbeaten := id("unbeaten")
	livenessPeer(t, ctx, q, unbeaten, "10.251.83.1", "10.251.255.1", sid, "up")

	check := func(name, collector, router string, wantLive, wantBeat bool, wantSessions uint64) {
		t.Helper()
		var r netip.Addr
		if router != "" {
			r = netip.MustParseAddr(router)
		}
		l, err := q.TargetLiveness(ctx, collector, r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if l.Live() != wantLive || l.HasBeat != wantBeat || l.LiveSessions != wantSessions {
			t.Errorf("%s: live %v, has beat %v, live sessions %d; want %v, %v, %d (%+v)",
				name, l.Live(), l.HasBeat, l.LiveSessions, wantLive, wantBeat, wantSessions, l)
		}
	}
	check("running collector", running, "", true, true, 2)
	check("running router", running, "10.251.80.1", true, true, 1)
	check("running router closed cleanly", running, "10.251.80.2", false, true, 0)
	check("running router with one peer up", running, "10.251.80.3", true, true, 1)
	check("silent collector", silent, "", false, true, 1)
	check("moved router", moved, "10.251.82.1", false, true, 0)
	check("moved collector", moved, "", true, true, 1)
	check("unbeaten collector", unbeaten, "", false, false, 1)
	check("unknown collector", id("unknown"), "", false, false, 0)

	l, err := q.TargetLiveness(ctx, running, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Started.Equal(started) {
		t.Errorf("Started = %v, want %v", l.Started, started)
	}
}
