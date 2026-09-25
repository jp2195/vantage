package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/chtest"
)

// insertPurgeFixture writes one router and one up peer under collector, in
// session sid, with a heartbeat age old whose process started one second
// before the session.
func insertPurgeFixture(t *testing.T, ctx context.Context, conn driver.Conn, collector, router string, sid int64, age time.Duration) {
	t.Helper()
	insertPurgePeer(t, ctx, conn, collector, router, sid, "up")
	if err := conn.Exec(ctx, "INSERT INTO "+testDB+".collector_beats (collector_id, started_at, beat_at, inserted_at) "+
		"SELECT ?, fromUnixTimestamp64Nano(toInt64(?), 'UTC'), now64(3), now64(3) - toIntervalMillisecond(?)",
		collector, sid-int64(time.Second), age.Milliseconds()); err != nil {
		t.Fatal(err)
	}
}

// insertPurgePeer writes one peer of router under collector's session sid,
// stored as kind.
func insertPurgePeer(t *testing.T, ctx context.Context, conn driver.Conn, collector, router string, sid int64, kind string) {
	t.Helper()
	if err := conn.Exec(ctx, "INSERT INTO "+testDB+".peer_events (collector_id, router_ip, peer_ip, rib, "+
		"session_id, seq, ts_router, ts_collector, stream_seq, kind) "+
		"VALUES (?, toIPv6(?), toIPv6('10.201.9.9'), 'in_pre', ?, 1, now64(6), now64(6), 1, ?)",
		collector, router, uint64(sid), kind); err != nil {
		t.Fatal(err)
	}
}

func peerRows(t *testing.T, ctx context.Context, conn driver.Conn, collector string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+testDB+".peer_current WHERE collector_id = ?",
		collector).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// routerRows is peerRows narrowed to one router.
func routerRows(t *testing.T, ctx context.Context, conn driver.Conn, collector, router string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM "+testDB+".peer_current "+
		"WHERE collector_id = ? AND router_ip = toIPv6(?)", collector, router).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func purgeIDs() (live, dead, neighbor string) {
	run := time.Now().UnixNano()
	return fmt.Sprintf("%scli-purge-live-%d", chtest.LivenessPrefix, run),
		fmt.Sprintf("%scli-purge-dead-%d", chtest.LivenessPrefix, run),
		fmt.Sprintf("%scli-purge-neighbor-%d", chtest.LivenessPrefix, run)
}

// TestPurgeRefusesALiveTarget, and -force overrides it: a collector heard from
// a minute ago, holding a session its running process opened, is not
// retired, and deleting its state would corrupt what it is still maintaining.
//
// The id is held by two running processes at once, a copied config or a
// split StatefulSet: the older opened 10.201.1.1 and the newer 10.201.1.2,
// and the older's beat is the one the archive received last. The older's
// session reads view_lost, the newer's up, and the id is live. A second
// collector watching 10.201.1.1 must survive the forced purge.
func TestPurgeRefusesALiveTarget(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	live, _, neighbor := purgeIDs()
	sid := time.Now().UnixNano()
	older, newer := sid, sid+int64(2*time.Second)
	insertPurgeFixture(t, ctx, conn, live, "10.201.1.1", older, 30*time.Second)
	insertPurgeFixture(t, ctx, conn, live, "10.201.1.2", newer, time.Minute)
	insertPurgeFixture(t, ctx, conn, neighbor, "10.201.1.1", sid, time.Minute)
	var out bytes.Buffer
	err := runPurge(ctx, []string{"-dsn", chtest.DSN(testDB), "-collector", live, "-stale-after", "1h"}, &out)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("purge of a live collector = %v, want a refusal", err)
	}
	// One, not two: the older process's session predates the newer's start.
	if !strings.Contains(err.Error(), "1 current session(s) with a peer up belong to that process") {
		t.Errorf("refusal does not count the newer process's one session: %v", err)
	}
	if n := peerRows(t, ctx, conn, live); n != 2 {
		t.Fatalf("a refused purge left %d peer rows, want 2", n)
	}
	out.Reset()
	if err := runPurge(ctx, []string{"-dsn", chtest.DSN(testDB), "-collector", live, "-stale-after", "1h", "-force"}, &out); err != nil {
		t.Fatalf("-force: %v", err)
	}
	if !strings.Contains(out.String(), "-force: purging a live target") {
		t.Errorf("-force output does not say it overrode the refusal:\n%s", out.String())
	}
	if n := peerRows(t, ctx, conn, live); n != 0 {
		t.Errorf("-force left %d peer rows, want 0", n)
	}
	if n := peerRows(t, ctx, conn, neighbor); n != 1 {
		t.Errorf("the other collector watching 10.201.1.1 has %d peer rows after the purge, want 1", n)
	}
}

// TestPurgeDryRunDeletesNothing, and then a real purge of a collector silent
// for two hours goes through without -force. The same collector's other
// router, and another collector's view of the purged router, must survive
// both.
func TestPurgeDryRunDeletesNothing(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	_, dead, neighbor := purgeIDs()
	sid := time.Now().UnixNano()
	insertPurgeFixture(t, ctx, conn, dead, "10.201.2.1", sid, 2*time.Hour)
	insertPurgeFixture(t, ctx, conn, dead, "10.201.2.2", sid, 2*time.Hour)
	insertPurgeFixture(t, ctx, conn, neighbor, "10.201.2.1", sid, time.Minute)
	neighbors := func(when string) {
		t.Helper()
		if n := routerRows(t, ctx, conn, dead, "10.201.2.2"); n != 1 {
			t.Errorf("%s: the same collector's other router has %d peer rows, want 1", when, n)
		}
		if n := routerRows(t, ctx, conn, neighbor, "10.201.2.1"); n != 1 {
			t.Errorf("%s: the other collector's view of the router has %d peer rows, want 1", when, n)
		}
	}
	var out bytes.Buffer
	if err := runPurge(ctx, []string{"-dsn", chtest.DSN(testDB), "-collector", dead,
		"-router", "10.201.2.1", "-stale-after", "1h", "-dry-run"}, &out); err != nil {
		t.Fatalf("-dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "peer_current") || !strings.Contains(out.String(), "dry run: nothing deleted") {
		t.Errorf("-dry-run output:\n%s", out.String())
	}
	if n := routerRows(t, ctx, conn, dead, "10.201.2.1"); n != 1 {
		t.Fatalf("-dry-run left %d peer rows, want 1 -- it must delete nothing", n)
	}
	neighbors("after -dry-run")
	out.Reset()
	if err := runPurge(ctx, []string{"-dsn", chtest.DSN(testDB), "-collector", dead,
		"-router", "10.201.2.1", "-stale-after", "1h"}, &out); err != nil {
		t.Fatalf("purge of a silent collector's router: %v", err)
	}
	if n := routerRows(t, ctx, conn, dead, "10.201.2.1"); n != 0 {
		t.Errorf("purge left %d peer rows, want 0", n)
	}
	neighbors("after the purge")
}

func TestPurgeRequiresADSNAndACollector(t *testing.T) {
	ctx := t.Context()
	var out bytes.Buffer
	if err := runPurge(ctx, []string{"-collector", "c1"}, &out); err == nil || !strings.Contains(err.Error(), "-dsn") {
		t.Errorf("no -dsn: %v", err)
	}
	if err := runPurge(ctx, []string{"-dsn", "clickhouse://x/y"}, &out); err == nil || !strings.Contains(err.Error(), "collector") {
		t.Errorf("no -collector: %v", err)
	}
}

// TestPurgeOfARouterIsJudgedOnThatRouter: on a running, beating collector,
// a router whose session closed cleanly -- its peer view_lost -- is retired
// and purges without -force, while a router with a peer up is refused. The
// collector as a whole is live either way, so a purge that judged the
// collector instead of the router would refuse both.
func TestPurgeOfARouterIsJudgedOnThatRouter(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	live, _, neighbor := purgeIDs()
	sid := time.Now().UnixNano()
	insertPurgeFixture(t, ctx, conn, live, "10.201.3.1", sid, time.Minute)
	insertPurgePeer(t, ctx, conn, live, "10.201.3.2", sid, "view_lost")
	insertPurgeFixture(t, ctx, conn, neighbor, "10.201.3.2", sid, time.Minute)
	dsn := chtest.DSN(testDB)

	var out bytes.Buffer
	if err := runPurge(ctx, []string{"-dsn", dsn, "-collector", live, "-router", "10.201.3.1",
		"-stale-after", "1h", "-dry-run"}, &out); err != nil {
		t.Fatalf("-dry-run of the up router: %v", err)
	}
	if !strings.Contains(out.String(), "a real run would refuse") {
		t.Errorf("-dry-run of a live router does not say a real run would refuse:\n%s", out.String())
	}
	err := runPurge(ctx, []string{"-dsn", dsn, "-collector", live, "-router", "10.201.3.1",
		"-stale-after", "1h"}, &out)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("purge of the router with a peer up = %v, want a refusal", err)
	}

	out.Reset()
	if err := runPurge(ctx, []string{"-dsn", dsn, "-collector", live, "-router", "10.201.3.2",
		"-stale-after", "1h"}, &out); err != nil {
		t.Fatalf("purge of the router whose session closed: %v", err)
	}
	if strings.Contains(out.String(), "-force") {
		t.Errorf("the closed router's purge was treated as live:\n%s", out.String())
	}
	if n := routerRows(t, ctx, conn, live, "10.201.3.2"); n != 0 {
		t.Errorf("the closed router has %d peer rows after its purge, want 0", n)
	}
	if n := routerRows(t, ctx, conn, live, "10.201.3.1"); n != 1 {
		t.Errorf("the same collector's up router has %d peer rows, want 1", n)
	}
	if n := routerRows(t, ctx, conn, neighbor, "10.201.3.2"); n != 1 {
		t.Errorf("the other collector's view of the closed router has %d peer rows, want 1", n)
	}

	// The collector as a whole still has its up session, so it is live.
	if err := runPurge(ctx, []string{"-dsn", dsn, "-collector", live, "-stale-after", "1h"}, &out); err == nil ||
		!strings.Contains(err.Error(), "refusing") {
		t.Errorf("purge of the whole live collector = %v, want a refusal", err)
	}
}

// TestPurgeOfARecentlyHeardCollectorNeedsForce: a collector last heard five
// minutes ago is past the stale threshold, so it is not live, but a stalled
// NATS or writer makes a running collector look exactly like that, and its
// queued rows would land after the purge. The purge refuses without -force
// and goes through with it. The other side, first: a collector last heard
// twenty minutes ago purges without -force.
func TestPurgeOfARecentlyHeardCollectorNeedsForce(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	_, recent, _ := purgeIDs()
	_, gone, _ := purgeIDs()
	sid := time.Now().UnixNano()
	insertPurgeFixture(t, ctx, conn, recent, "10.201.4.1", sid, 5*time.Minute)
	insertPurgeFixture(t, ctx, conn, gone, "10.201.4.2", sid, 20*time.Minute)
	dsn := chtest.DSN(testDB)

	var out bytes.Buffer
	if err := runPurge(ctx, []string{"-dsn", dsn, "-collector", gone}, &out); err != nil {
		t.Fatalf("purge of a collector heard 20m ago: %v", err)
	}
	if strings.Contains(out.String(), "-force") {
		t.Errorf("the purge of a collector heard 20m ago was treated as needing -force:\n%s", out.String())
	}
	if n := peerRows(t, ctx, conn, gone); n != 0 {
		t.Errorf("purge left %d peer rows, want 0", n)
	}

	out.Reset()
	if err := runPurge(ctx, []string{"-dsn", dsn, "-collector", recent, "-dry-run"}, &out); err != nil {
		t.Fatalf("-dry-run: %v", err)
	}
	if !strings.Contains(out.String(), "a real run would refuse") {
		t.Errorf("-dry-run of a collector heard 5m ago does not say a real run would refuse:\n%s", out.String())
	}
	err := runPurge(ctx, []string{"-dsn", dsn, "-collector", recent}, &out)
	if err == nil || !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), "15m") {
		t.Fatalf("purge of a collector heard 5m ago = %v, want a refusal naming the 15m bound", err)
	}
	if n := peerRows(t, ctx, conn, recent); n != 1 {
		t.Fatalf("a refused purge left %d peer rows, want 1", n)
	}
	out.Reset()
	if err := runPurge(ctx, []string{"-dsn", dsn, "-collector", recent, "-force"}, &out); err != nil {
		t.Fatalf("-force: %v", err)
	}
	if !strings.Contains(out.String(), "-force:") {
		t.Errorf("-force output does not say it overrode the refusal:\n%s", out.String())
	}
	if n := peerRows(t, ctx, conn, recent); n != 0 {
		t.Errorf("-force left %d peer rows, want 0", n)
	}

}
