package sink

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// TestPurgeStatementsNeverCarryTheCleanupKillMarker: cleanup kills its own
// failed mutations by matching `floor_sid` in their command text. A purge's
// DELETE must never match, or a cleanup failing at the same moment would kill
// the purge -- and a collector id is part of that text, so an id carrying the
// marker is refused outright.
func TestPurgeStatementsNeverCarryTheCleanupKillMarker(t *testing.T) {
	for _, target := range []PurgeTarget{
		{Collector: "dev-c1"},
		{Collector: "dev-c1", Router: netip.MustParseAddr("10.0.0.1")},
	} {
		for _, table := range target.Tables() {
			w, _ := target.deleteWhere(table, 1)
			if stmt := purgeDeleteSQL("vantage", table, w); strings.Contains(stmt, cleanupKillMarker) {
				t.Errorf("purge statement carries the kill marker: %s", stmt)
			}
		}
	}
	if err := (PurgeTarget{Collector: "edge-floor_sid-1"}).Validate(); err == nil {
		t.Error("a collector id containing the kill marker was accepted")
	}
	if err := (PurgeTarget{}).Validate(); err == nil {
		t.Error("an empty collector was accepted")
	}
}

// TestPurgeOrderEndsAtPeerCurrentThenBeats: peer_current is the last current
// table, so an interrupted purge leaves the router visible and a re-run
// finishes it; and the collector's beats go only when the whole collector
// does.
func TestPurgeOrderEndsAtPeerCurrentThenBeats(t *testing.T) {
	router := PurgeTarget{Collector: "c", Router: netip.MustParseAddr("10.0.0.1")}.Tables()
	whole := PurgeTarget{Collector: "c"}.Tables()
	if len(router) != 8 || router[7] != "peer_current" {
		t.Errorf("router purge tables = %v, want the eight current tables ending at peer_current", router)
	}
	if slices.Contains(router, "collector_beats") {
		t.Error("a router purge deletes the collector's beats; the collector is still watching other routers")
	}
	if len(whole) != 9 || whole[7] != "peer_current" || whole[8] != "collector_beats" {
		t.Errorf("collector purge tables = %v, want the eight then collector_beats", whole)
	}
}

// writePurgeFixture writes one peer and one row in each of three other
// current tables for collector's session sid with router: one row in each of
// four current tables.
func writePurgeFixture(t *testing.T, ctx context.Context, c *ClickHouse, collector string, router netip.Addr, sid uint64, ts time.Time) {
	t.Helper()
	env := envelope{
		CollectorID: collector, RouterIP: router.String(), PeerIP: "10.200.2.1", RIB: "in_pre",
		PeerBGPID: "10.200.2.1", SessionID: sid, Seq: 1, TsRouter: ts, TsCollector: ts, StreamSeq: 1,
	}
	if err := c.Insert(ctx, Rows{
		Peer:    []PeerRow{{envelope: env, Kind: "up", LocalIP: "::", MPFamilies: []string{}, AddPathFamilies: []string{}}},
		Unicast: []UnicastRow{{envelope: env, Family: "ipv4u", Prefix: "10.200.3.0/24", NextHop: "10.200.2.1"}},
		Eor:     []EorRow{{envelope: env, Family: "ipv4u"}},
		LsNodes: []LsNodeRow{{envelope: env, Protocol: 2, RouterID: "0a0000c8"}},
	}); err != nil {
		t.Fatalf("insert %s/%s session %d: %v", collector, router, sid, err)
	}
}

func plan(t *testing.T, ctx context.Context, c *ClickHouse, target PurgeTarget) PurgePlan {
	t.Helper()
	p, err := c.PlanPurge(ctx, target)
	if err != nil {
		t.Fatalf("PlanPurge(%+v): %v", target, err)
	}
	return p
}

// tableRows is how many rows collector holds in table.
func tableRows(t *testing.T, ctx context.Context, c *ClickHouse, table, collector string) uint64 {
	t.Helper()
	var n uint64
	if err := c.conn.QueryRow(ctx, "SELECT count() FROM "+c.db+"."+table+" WHERE collector_id = ?",
		collector).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPurgeRemovesTheTargetAndNothingElse writes one collector watching two
// routers and a second collector watching the first of them, with rows in
// four current tables each and beats for both. A router purge must take
// exactly the (collector, router) pair; a collector purge everything that
// collector holds, beats included; and neither may touch its neighbors.
func TestPurgeRemovesTheTargetAndNothingElse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()
	run := time.Now().UnixNano()
	a := fmt.Sprintf("%spurge-a-%d", chtest.LivenessPrefix, run)
	b := fmt.Sprintf("%spurge-b-%d", chtest.LivenessPrefix, run)
	r1, r2 := netip.MustParseAddr("10.200.1.1"), netip.MustParseAddr("10.200.1.2")
	ts := time.Now().UTC().Truncate(time.Microsecond)
	write := func(collector string, router netip.Addr) {
		t.Helper()
		writePurgeFixture(t, ctx, c, collector, router, uint64(run), ts)
	}
	write(a, r1)
	write(a, r2)
	write(b, r1)
	if err := c.Insert(ctx, Rows{Beats: []BeatRow{
		{CollectorID: a, StartedAt: ts, BeatAt: ts},
		{CollectorID: b, StartedAt: ts, BeatAt: ts},
	}}); err != nil {
		t.Fatal(err)
	}
	total := func(target PurgeTarget) map[string]uint64 {
		t.Helper()
		out := map[string]uint64{}
		for _, pc := range plan(t, ctx, c, target).Counts {
			out[pc.Table] = pc.Rows
		}
		return out
	}
	want := map[string]uint64{
		"eor_current": 1, "route_unicast_current": 1, "route_vpn_current": 0, "route_evpn_current": 0,
		"ls_nodes_current": 1, "ls_links_current": 0, "ls_prefixes_current": 0, "peer_current": 1,
	}
	pairA1 := PurgeTarget{Collector: a, Router: r1}
	if got := total(pairA1); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dry-run counts for (%s, %s) = %v, want %v", a, r1, got, want)
	}

	if err := c.Purge(ctx, plan(t, ctx, c, pairA1)); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	for table, n := range total(pairA1) {
		if n != 0 {
			t.Errorf("after the router purge, %s still holds %d rows of (%s, %s)", table, n, a, r1)
		}
	}
	if got := total(PurgeTarget{Collector: a, Router: r2}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the same collector's other router lost rows: %v", got)
	}
	if got := total(PurgeTarget{Collector: b, Router: r1}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the other collector's view of the same router lost rows: %v", got)
	}
	if got := total(PurgeTarget{Collector: a})["collector_beats"]; got != 1 {
		t.Errorf("a router purge touched the collector's beats: %d left, want 1", got)
	}

	if err := c.Purge(ctx, plan(t, ctx, c, PurgeTarget{Collector: a})); err != nil {
		t.Fatalf("Purge collector: %v", err)
	}
	for table, n := range total(PurgeTarget{Collector: a}) {
		if n != 0 {
			t.Errorf("after the collector purge, %s still holds %d rows of %s", table, n, a)
		}
	}
	gotB := total(PurgeTarget{Collector: b})
	if gotB["peer_current"] != 1 || gotB["collector_beats"] != 1 {
		t.Errorf("the other collector lost rows to a collector purge: %v", gotB)
	}
}

// TestPurgeKeepsASessionThatArrivesAfterThePlan: a router that reconnects
// between the plan and the DELETEs opens a newer session, and its rows are
// current state the purge never counted. They must survive it.
func TestPurgeKeepsASessionThatArrivesAfterThePlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()
	run := time.Now().UnixNano()
	a := fmt.Sprintf("%spurge-reconnect-%d", chtest.LivenessPrefix, run)
	r := netip.MustParseAddr("10.200.4.1")
	ts := time.Now().UTC().Truncate(time.Microsecond)
	target := PurgeTarget{Collector: a, Router: r}
	writePurgeFixture(t, ctx, c, a, r, uint64(run), ts)
	p := plan(t, ctx, c, target)
	if p.MaxSession != uint64(run) {
		t.Fatalf("plan MaxSession = %d, want %d", p.MaxSession, run)
	}
	writePurgeFixture(t, ctx, c, a, r, uint64(run)+1, ts)
	if err := c.Purge(ctx, p); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	for _, table := range []string{"eor_current", "route_unicast_current", "ls_nodes_current", "peer_current"} {
		var old, fresh uint64
		if err := c.conn.QueryRow(ctx, "SELECT countIf(session_id = ?), countIf(session_id = ?) FROM "+
			c.db+"."+table+" WHERE collector_id = ?", uint64(run), uint64(run)+1, a).Scan(&old, &fresh); err != nil {
			t.Fatal(err)
		}
		if old != 0 || fresh != 1 {
			t.Errorf("%s after the purge: %d rows of the planned session, %d of the newer; want 0 and 1",
				table, old, fresh)
		}
	}
}

// TestPurgeDeletesInOrderAndStopsAtAFailure makes ls_links_current, the
// seventh table, fail by renaming it away after the plan. Purge must have
// deleted the six before it and nothing after it: peer_current and the
// beats stay, so the target is still visible. With the table back, running
// the plan again finishes the job.
//
// It owns its database, because it renames a table out from under anything
// else that would use one.
func TestPurgeDeletesInOrderAndStopsAtAFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_purge_order_test")
	defer c.Close()
	run := time.Now().UnixNano()
	a := fmt.Sprintf("%spurge-order-%d", chtest.LivenessPrefix, run)
	r := netip.MustParseAddr("10.200.5.1")
	ts := time.Now().UTC().Truncate(time.Microsecond)
	writePurgeFixture(t, ctx, c, a, r, uint64(run), ts)
	if err := c.Insert(ctx, Rows{Beats: []BeatRow{{CollectorID: a, StartedAt: ts, BeatAt: ts}}}); err != nil {
		t.Fatal(err)
	}
	p := plan(t, ctx, c, PurgeTarget{Collector: a})

	links, away := c.db+".ls_links_current", c.db+".ls_links_current_away"
	if err := c.conn.Exec(ctx, "RENAME TABLE "+links+" TO "+away); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if err := c.conn.Exec(context.WithoutCancel(ctx), "RENAME TABLE "+away+" TO "+links); err != nil {
				t.Errorf("restore %s: %v", links, err)
			}
		}
	}
	defer restore()

	err := c.Purge(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "ls_links_current") {
		t.Fatalf("Purge with ls_links_current gone = %v, want its failure", err)
	}
	for table, want := range map[string]uint64{
		"eor_current": 0, "route_unicast_current": 0, "ls_nodes_current": 0,
		"peer_current": 1, "collector_beats": 1,
	} {
		if got := tableRows(t, ctx, c, table, a); got != want {
			t.Errorf("after a purge that failed at ls_links_current, %s holds %d rows, want %d", table, got, want)
		}
	}

	restore()
	if err := c.Purge(ctx, p); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	for _, table := range p.Target.Tables() {
		if got := tableRows(t, ctx, c, table, a); got != 0 {
			t.Errorf("after the re-run, %s holds %d rows, want 0", table, got)
		}
	}
}

// TestPurgeMutationsEscapeTheCleanupKillFilter: cleanup kills failed
// mutations by matching cleanupKillFilter against system.mutations. Its
// underscore is escaped, so a collector id that differs from the marker
// only where LIKE's _ wildcard would stand in -- edge-floor-sid -- must not
// match, while cleanup's own DELETEs, which name floor_sid, still must. Both
// are asked of ClickHouse, through the filter's own text.
//
// It owns its database, so every mutation in it is this test's.
func TestPurgeMutationsEscapeTheCleanupKillFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_purge_kill_test")
	defer c.Close()
	run := time.Now().UnixNano()
	lookalike := fmt.Sprintf("%sedge-floor-sid-%d", chtest.LivenessPrefix, run)
	if err := (PurgeTarget{Collector: lookalike}).Validate(); err != nil {
		t.Fatalf("Validate(%q): %v", lookalike, err)
	}
	ts := time.Now().UTC().Truncate(time.Microsecond)
	r := netip.MustParseAddr("10.200.6.1")
	writePurgeFixture(t, ctx, c, lookalike, r, uint64(run), ts)

	// Three sessions of one router, so cleanup has one below its floor to
	// delete, and so writes DELETEs naming floor_sid.
	cleaned := fmt.Sprintf("kill-c-%d", run)
	for i := range uint64(3) {
		writePurgeFixture(t, ctx, c, cleaned, r, uint64(run)+i, ts)
	}
	if _, err := c.CleanupSuperseded(ctx); err != nil {
		t.Fatalf("CleanupSuperseded: %v", err)
	}
	if err := c.Purge(ctx, plan(t, ctx, c, PurgeTarget{Collector: lookalike})); err != nil {
		t.Fatalf("Purge: %v", err)
	}

	var purges, purgesMatched, cleanupsMatched uint64
	if err := c.conn.QueryRow(ctx, `SELECT
            countIf(position(command, ?) > 0),
            countIf(position(command, ?) > 0 AND `+cleanupKillFilter+`),
            countIf(position(command, ?) = 0 AND `+cleanupKillFilter+`)
        FROM system.mutations WHERE database = ?`,
		lookalike, lookalike, lookalike, c.db).Scan(&purges, &purgesMatched, &cleanupsMatched); err != nil {
		t.Fatal(err)
	}
	if purges != 9 {
		t.Fatalf("system.mutations holds %d purge mutations for %s, want 9", purges, lookalike)
	}
	if purgesMatched != 0 {
		t.Errorf("%d of the purge's mutations match cleanup's kill filter %s", purgesMatched, cleanupKillFilter)
	}
	if cleanupsMatched == 0 {
		t.Errorf("no cleanup mutation matches cleanup's kill filter %s", cleanupKillFilter)
	}
}
