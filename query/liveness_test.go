package query

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/collector"
)

// livenessBase is the instant every session in these fixtures opened near.
// Session ids are UnixNano on the collector's clock, so these fixtures build
// them from a real instant rather than the small integers other fixtures in
// this package use: an epoch comparison against 1, 2 or 100 would test
// nothing.
var livenessBase = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

// A fresh beat is one minute old and an old one two hours, against a
// one-hour threshold (livenessQ), so neither answer depends on how long the
// test takes to run: at the default 90 s a fresh beat would go stale under a
// slow runner.
const (
	beatFresh = time.Minute
	beatOld   = 2 * time.Hour
)

// livenessQ is requireQuery reading with a one-hour stale threshold.
func livenessQ(t *testing.T, ctx context.Context) *Q {
	t.Helper()
	q, err := requireQuery(t, ctx).WithStaleAfter(time.Hour)
	if err != nil {
		t.Fatalf("WithStaleAfter: %v", err)
	}
	return q
}

// livenessSID is the session id fixture case i opens: distinct per case and
// on a real clock.
func livenessSID(i int) uint64 { return uint64(livenessBase.UnixNano()) + uint64(i)*1000 }

// insertBeat writes one collector_beats row. inserted_at is age before the
// ClickHouse server's own now64() -- the clock liveness is read against --
// and beat_at, the collector's clock, is given separately so a test can make
// the two disagree.
func insertBeat(t *testing.T, ctx context.Context, q *Q, collectorID string, startedAt, beatAt time.Time, age time.Duration) {
	t.Helper()
	if err := q.conn.Exec(ctx, "INSERT INTO "+q.db+".collector_beats (collector_id, started_at, beat_at, inserted_at) "+
		"SELECT ?, fromUnixTimestamp64Nano(toInt64(?), 'UTC'), fromUnixTimestamp64Milli(toInt64(?), 'UTC'), "+
		"now64(3) - toIntervalMillisecond(?)",
		collectorID, startedAt.UnixNano(), beatAt.UnixMilli(), age.Milliseconds()); err != nil {
		t.Fatalf("insert beat for %s: %v", collectorID, err)
	}
}

// livenessPeer writes one peer_events row for collectorID's session sid with
// router, carrying the stored kind.
func livenessPeer(t *testing.T, ctx context.Context, q *Q, collectorID, router, peer string, sid uint64, kind string) {
	t.Helper()
	insertPeerEvent(t, ctx, q, peerEventFixture{
		Collector: collectorID, RouterIP: router, RouterSysname: "r-" + router,
		PeerIP: peer, RIB: "in_pre", PeerASN: 65251, PeerBGPID: peer,
		SessionID: sid, Seq: 1, StreamSeq: 1, Kind: kind,
		TsRouter: livenessBase, TsCollector: livenessBase,
	})
}

// peerStates is Peers' answer for every collector whose id starts with prefix,
// as collector -> state. Each such collector in these fixtures has one peer.
func peerStates(t *testing.T, ctx context.Context, q *Q, prefix string) map[string]string {
	t.Helper()
	peers, err := q.Peers(ctx, PeerFilter{})
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	out := map[string]string{}
	for _, p := range peers {
		if strings.HasPrefix(p.Collector, prefix) {
			if prev, ok := out[p.Collector]; ok {
				t.Fatalf("%s has two peer rows (%s, %s); each fixture collector has one", p.Collector, prev, p.State)
			}
			out[p.Collector] = p.State
		}
	}
	return out
}

// TestPeerStateResolvesCollectorLiveness is the read rule's whole truth
// table: each stored state, with the session before, at and after its
// collector's epoch, and a beat that is fresh, old or missing. Every row is
// its own collector, router and session, so no case can lend another its
// answer.
//
// The rows that pin each clause:
//
//   - up/after/*: the epoch branch. With it deleted they read up or stale.
//   - up/before/old and up/before/missing: the stale branch.
//   - up/after/old: branch ORDER. With the two swapped it reads stale.
//   - up/at/fresh: `<`, not `<=`. A session minted in the nanosecond its
//     process started is that process's own.
//   - every down and view_lost row: only a stored up ever changes.
//   - up/before/missing: the placeholder arm of live. Without it the
//     collector has no live row, the INNER JOIN drops it, and the peer
//     vanishes rather than reading stale.
//
// Routers is asserted beside Peers, because it counts off peer_up while Peers
// reads peer_state: the rule is written into both CTEs, and either copy could
// drift.
func TestPeerStateResolvesCollectorLiveness(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	const prefix = chtest.LivenessPrefix + "rule-"
	type row struct{ stored, epoch, beat, want string }
	var cases []row
	for _, stored := range []string{"up", "down", "view_lost"} {
		for _, c := range []struct{ epoch, beat string }{
			{"before", "fresh"}, {"before", "old"}, {"before", "missing"},
			{"at", "fresh"}, {"after", "fresh"}, {"after", "old"},
		} {
			want := stored
			if stored == "up" {
				switch {
				case c.epoch == "after":
					want = "view_lost"
				case c.beat != "fresh":
					want = "stale"
				}
			}
			cases = append(cases, row{stored, c.epoch, c.beat, want})
		}
	}
	name := func(c row) string { return fmt.Sprintf("%s%s-%s-%s", prefix, c.stored, c.epoch, c.beat) }
	for i, c := range cases {
		sid := livenessSID(i)
		livenessPeer(t, ctx, q, name(c), fmt.Sprintf("10.251.%d.1", i), "10.251.255.1", sid, c.stored)
		started := time.Unix(0, int64(sid))
		switch c.epoch {
		case "before":
			started = started.Add(-time.Second)
		case "after":
			started = started.Add(time.Second)
		}
		switch c.beat {
		case "fresh":
			insertBeat(t, ctx, q, name(c), started, started, beatFresh)
		case "old":
			insertBeat(t, ctx, q, name(c), started, started, beatOld)
		}
	}

	states := peerStates(t, ctx, q, prefix)
	routers, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	counts := map[string]map[string]int{}
	for _, r := range routers {
		if strings.HasPrefix(r.Collector, prefix) {
			counts[r.Collector] = map[string]int{
				"up": r.PeersUp, "down": r.PeersDown, "view_lost": r.PeersViewLost, "stale": r.PeersStale,
			}
		}
	}
	for _, c := range cases {
		n := name(c)
		got, ok := states[n]
		if !ok {
			t.Errorf("%s: Peers returned no row -- a collector the live CTE cannot join is dropped, not read stale", n)
		} else if got != c.want {
			t.Errorf("%s: Peer.State = %q, want %q", n, got, c.want)
		}
		cnt := counts[n]
		total := cnt["up"] + cnt["down"] + cnt["view_lost"] + cnt["stale"]
		if cnt[c.want] != 1 || total != 1 {
			t.Errorf("%s: Routers counts %v, want exactly one %s", n, cnt, c.want)
		}
	}
}

// TestStalenessIsMeasuredOnTheArchiveClock: a collector is stale by when the
// ARCHIVE last received its beat, never by the time on the beat. Two cases,
// each the other's opposite. A collector whose clock runs three hours slow --
// or whose beat sat in a backlog -- was received a minute ago and is fresh. A
// collector whose clock runs three hours fast was last received two hours ago
// and is stale. Reading beat_at instead of inserted_at flips both.
func TestStalenessIsMeasuredOnTheArchiveClock(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	const prefix = chtest.LivenessPrefix + "clock-"
	now := time.Now()
	for i, c := range []struct {
		id     string
		beatAt time.Time
		age    time.Duration
	}{
		{prefix + "slow", now.Add(-3 * time.Hour), beatFresh},
		{prefix + "fast", now.Add(3 * time.Hour), beatOld},
	} {
		sid := livenessSID(100 + i)
		livenessPeer(t, ctx, q, c.id, fmt.Sprintf("10.251.100.%d", i+1), "10.251.255.1", sid, "up")
		insertBeat(t, ctx, q, c.id, time.Unix(0, int64(sid)).Add(-time.Second), c.beatAt, c.age)
	}
	states := peerStates(t, ctx, q, prefix)
	if got := states[prefix+"slow"]; got != "up" {
		t.Errorf("slow collector clock, beat received a minute ago: state %q, want up", got)
	}
	if got := states[prefix+"fast"]; got != "stale" {
		t.Errorf("fast collector clock, beat received two hours ago: state %q, want stale", got)
	}
}

// TestTheThresholdIsTheOneTheQReadsWith: one beat, two minutes old, read by
// two Qs. The default 90 s reads it stale and a one-hour Q reads it up. A
// threshold hard-coded anywhere in the SQL passes one half and fails the
// other.
func TestTheThresholdIsTheOneTheQReadsWith(t *testing.T) {
	ctx := t.Context()
	base := requireQuery(t, ctx)
	hour := livenessQ(t, ctx)
	id := chtest.LivenessPrefix + "threshold"
	sid := livenessSID(200)
	livenessPeer(t, ctx, base, id, "10.251.200.1", "10.251.255.1", sid, "up")
	insertBeat(t, ctx, base, id, time.Unix(0, int64(sid)).Add(-time.Second),
		time.Now(), 2*time.Minute)
	if got := peerStates(t, ctx, base, id)[id]; got != "stale" {
		t.Errorf("default threshold (%v), beat 2m old: %q, want stale", base.StaleAfter(), got)
	}
	if got := peerStates(t, ctx, hour, id)[id]; got != "up" {
		t.Errorf("one-hour threshold, beat 2m old: %q, want up", got)
	}
}

// TestStaleRecoversToUpWithNoWrite: nothing is written to make a peer stale,
// and nothing to bring it back. An old beat reads stale; a new beat arriving
// reads up again; the peer tables are not touched in between.
func TestStaleRecoversToUpWithNoWrite(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	// Unique per run: this test's second beat is fresh, and a fresh beat
	// left by an earlier run in the same database would keep the first
	// assertion from ever seeing stale.
	id := fmt.Sprintf("%srecover-%d", chtest.LivenessPrefix, time.Now().UnixNano())
	sid := livenessSID(300)
	started := time.Unix(0, int64(sid)).Add(-time.Second)
	livenessPeer(t, ctx, q, id, "10.251.30.1", "10.251.255.1", sid, "up")
	insertBeat(t, ctx, q, id, started, started, beatOld)
	if got := peerStates(t, ctx, q, id)[id]; got != "stale" {
		t.Fatalf("after a two-hour silence: %q, want stale", got)
	}
	var before, after uint64
	count := func(dst *uint64) {
		if err := q.conn.QueryRow(ctx, "SELECT count() FROM "+q.db+".peer_current WHERE collector_id = ?", id).
			Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	count(&before)
	insertBeat(t, ctx, q, id, started, time.Now(), 0)
	count(&after)
	if got := peerStates(t, ctx, q, id)[id]; got != "up" {
		t.Errorf("after the collector beat again: %q, want up", got)
	}
	if before != after {
		t.Errorf("peer_current went from %d to %d rows: returning to up must write nothing", before, after)
	}
}

// TestLivenessIsKeyedOnTheCollector: two collectors watch one router, one
// heard from a minute ago and one never. Each collector's view of the same
// peer resolves on its OWN liveness: up for the first, stale for the second.
// A live CTE that pooled collectors -- the newest beat of any collector
// vouching for all of them -- reads both up.
func TestLivenessIsKeyedOnTheCollector(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	const prefix = chtest.LivenessPrefix + "dual-"
	live, dead := prefix+"live", prefix+"dead"
	sid := livenessSID(400)
	for _, id := range []string{live, dead} {
		livenessPeer(t, ctx, q, id, "10.251.40.1", "10.251.255.1", sid, "up")
	}
	insertBeat(t, ctx, q, live, time.Unix(0, int64(sid)).Add(-time.Second), time.Now(), beatFresh)
	states := peerStates(t, ctx, q, prefix)
	if states[live] != "up" || states[dead] != "stale" {
		t.Errorf("states = %v, want %s up and %s stale", states, live, dead)
	}
}

// TestTwoCollectorsCountOneRouterOnTheirOwnRows: two collectors watch one
// router, one heard from a minute ago and one never. Routers and Collectors
// each report the router once per collector, with that collector's own
// counts, and never the two views added together. Adding them is the defect
// this pins: counts grouped on the router alone report the live collector's
// row with stale peers it does not have, and the silent one's with up peers
// nobody can vouch for.
//
// The two views differ in size so no sum can pass for either: the live
// collector sees two up peers and one down, the silent one sees the same two
// plus a third, all stale, and the same down peer.
func TestTwoCollectorsCountOneRouterOnTheirOwnRows(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	const prefix = chtest.LivenessPrefix + "pair-"
	live, dead := prefix+"live", prefix+"dead"
	const router = "10.251.70.1"
	sid := livenessSID(800)
	for _, p := range []string{"10.251.70.2", "10.251.70.3"} {
		livenessPeer(t, ctx, q, live, router, p, sid, "up")
		livenessPeer(t, ctx, q, dead, router, p, sid, "up")
	}
	livenessPeer(t, ctx, q, dead, router, "10.251.70.4", sid, "up")
	livenessPeer(t, ctx, q, live, router, "10.251.70.5", sid, "down")
	livenessPeer(t, ctx, q, dead, router, "10.251.70.5", sid, "down")
	insertBeat(t, ctx, q, live, time.Unix(0, int64(sid)).Add(-time.Second), time.Now(), beatFresh)

	type counts struct{ up, down, viewLost, stale int }
	want := map[string]counts{live: {2, 1, 0, 0}, dead: {0, 1, 0, 3}}

	routers, err := q.Routers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotRouters := map[string]counts{}
	for _, r := range routers {
		if r.IP.String() != router {
			continue
		}
		if _, dup := gotRouters[r.Collector]; dup {
			t.Errorf("Routers: two rows for (%s, %s)", r.Collector, router)
		}
		gotRouters[r.Collector] = counts{r.PeersUp, r.PeersDown, r.PeersViewLost, r.PeersStale}
	}
	if fmt.Sprint(gotRouters) != fmt.Sprint(want) {
		t.Errorf("Routers counts by collector = %v, want %v", gotRouters, want)
	}

	all, err := q.Collectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotRouter, gotTotal := map[string]counts{}, map[string]counts{}
	for _, c := range all {
		if _, ok := want[c.Collector]; !ok {
			continue
		}
		gotTotal[c.Collector] = counts{c.PeersUp, c.PeersDown, c.PeersViewLost, c.PeersStale}
		for _, r := range c.Routers {
			if r.IP.String() == router {
				gotRouter[c.Collector] = counts{r.PeersUp, r.PeersDown, r.PeersViewLost, r.PeersStale}
			}
		}
	}
	if fmt.Sprint(gotRouter) != fmt.Sprint(want) {
		t.Errorf("Collectors per-router counts = %v, want %v", gotRouter, want)
	}
	if fmt.Sprint(gotTotal) != fmt.Sprint(want) {
		t.Errorf("Collectors totals = %v, want %v", gotTotal, want)
	}
}

// TestTwoLiveProcessesUnderOneCollectorID is the documented outcome of a
// misconfiguration the epoch rule cannot tell from a restart: two running
// processes both claiming one collector_id, both beating. The newer process
// start is the epoch, so the older process's sessions read view_lost -- wrong
// for a process that is in fact alive, but visible, where before this design
// the same misconfiguration was silent.
func TestTwoLiveProcessesUnderOneCollectorID(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	id := chtest.LivenessPrefix + "twin"
	older, newer := livenessBase.Add(-time.Hour), livenessBase
	sidOld, sidNew := uint64(older.Add(time.Minute).UnixNano()), uint64(newer.Add(time.Minute).UnixNano())
	insertPeerEvent(t, ctx, q, peerEventFixture{Collector: id, RouterIP: "10.251.50.1", PeerIP: "10.251.255.1",
		RIB: "in_pre", PeerASN: 65251, PeerBGPID: "10.251.255.1", SessionID: sidOld, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: livenessBase, TsCollector: livenessBase})
	insertPeerEvent(t, ctx, q, peerEventFixture{Collector: id, RouterIP: "10.251.50.2", PeerIP: "10.251.255.1",
		RIB: "in_pre", PeerASN: 65251, PeerBGPID: "10.251.255.1", SessionID: sidNew, Seq: 1, StreamSeq: 1,
		Kind: "up", TsRouter: livenessBase, TsCollector: livenessBase})
	insertBeat(t, ctx, q, id, older, time.Now(), beatFresh)
	insertBeat(t, ctx, q, id, newer, time.Now(), beatFresh)
	peers, err := q.Peers(ctx, PeerFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range peers {
		if p.Collector == id {
			got[p.RouterIP.String()] = p.State
		}
	}
	if got["10.251.50.1"] != "view_lost" || got["10.251.50.2"] != "up" {
		t.Errorf("states by router = %v, want the older process's session view_lost and the newer's up", got)
	}
}

// TestCollectorsReportsLastBeatAndStart: the Collectors card's last-heard and
// started-at come from the beats, and a collector never heard from has
// neither -- nil, not 1970. peers_stale is summed across the collector's
// routers like the three counts beside it.
func TestCollectorsReportsLastBeatAndStart(t *testing.T) {
	ctx := t.Context()
	q := livenessQ(t, ctx)
	const prefix = chtest.LivenessPrefix + "card-"
	heard, silent := prefix+"heard", prefix+"silent"
	sid := livenessSID(600)
	started := time.Unix(0, int64(sid)).Add(-time.Second).UTC()
	livenessPeer(t, ctx, q, heard, "10.251.60.1", "10.251.255.1", sid, "up")
	livenessPeer(t, ctx, q, silent, "10.251.60.2", "10.251.255.1", sid, "up")
	livenessPeer(t, ctx, q, silent, "10.251.60.3", "10.251.255.1", sid, "up")
	insertBeat(t, ctx, q, heard, started, time.Now(), beatFresh)
	all, err := q.Collectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]CollectorSummary{}
	for _, c := range all {
		by[c.Collector] = c
	}
	h, s := by[heard], by[silent]
	if h.LastBeatAt == nil || h.StartedAt == nil {
		t.Fatalf("%s: LastBeatAt %v, StartedAt %v; want both set", heard, h.LastBeatAt, h.StartedAt)
	}
	if !h.StartedAt.Equal(started) {
		t.Errorf("%s: StartedAt %v, want %v", heard, *h.StartedAt, started)
	}
	if age := time.Since(*h.LastBeatAt); age < 30*time.Second || age > 10*time.Minute {
		t.Errorf("%s: LastBeatAt is %v old, want about a minute", heard, age)
	}
	if s.LastBeatAt != nil || s.StartedAt != nil {
		t.Errorf("%s: LastBeatAt %v, StartedAt %v; want nil for a collector never heard from", silent, s.LastBeatAt, s.StartedAt)
	}
	if h.PeersUp != 1 || h.PeersStale != 0 || s.PeersUp != 0 || s.PeersStale != 2 {
		t.Errorf("counts: %s up %d stale %d, %s up %d stale %d; want 1/0 and 0/2",
			heard, h.PeersUp, h.PeersStale, silent, s.PeersUp, s.PeersStale)
	}
	// Each router carries its own count, not only the collector's total: the
	// silent collector's two routers read one stale peer each, and the heard
	// collector's one router reads its peer up.
	for _, c := range []struct {
		cs        CollectorSummary
		routers   int
		up, stale int
	}{{h, 1, 1, 0}, {s, 2, 0, 1}} {
		if len(c.cs.Routers) != c.routers {
			t.Errorf("%s: %d routers, want %d", c.cs.Collector, len(c.cs.Routers), c.routers)
			continue
		}
		for _, r := range c.cs.Routers {
			if r.PeersUp != c.up || r.PeersStale != c.stale {
				t.Errorf("%s router %s: up %d stale %d, want %d/%d",
					c.cs.Collector, r.IP, r.PeersUp, r.PeersStale, c.up, c.stale)
			}
		}
	}
}

// recordingConn records the text of every statement handed to it and fails
// each one, so the threshold substitution can be checked with no database.
type recordingConn struct {
	driver.Conn
	mu    sync.Mutex
	texts []string
}

type errRow struct{ err error }

func (r errRow) Err() error           { return r.err }
func (r errRow) Scan(...any) error    { return r.err }
func (r errRow) ScanStruct(any) error { return r.err }
func (c *recordingConn) record(q string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, q)
	return errors.New("recorded")
}
func (c *recordingConn) Query(_ context.Context, q string, _ ...any) (driver.Rows, error) {
	return nil, c.record(q)
}
func (c *recordingConn) QueryRow(_ context.Context, q string, _ ...any) driver.Row {
	return errRow{c.record(q)}
}
func (c *recordingConn) Exec(_ context.Context, q string, _ ...any) error { return c.record(q) }
func (c *recordingConn) Select(_ context.Context, _ any, q string, _ ...any) error {
	return c.record(q)
}
func (c *recordingConn) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.texts[len(c.texts)-1]
}

// TestEveryStatementCarriesTheQsOwnThreshold: the token never reaches
// ClickHouse, and each Q fills in its own number -- WithStaleAfter returns a
// new Q and leaves the one it was called on alone.
func TestEveryStatementCarriesTheQsOwnThreshold(t *testing.T) {
	ctx := context.Background()
	rc := &recordingConn{}
	q, err := New(rc, "vantage_query_test")
	if err != nil {
		t.Fatal(err)
	}
	q2, err := q.WithStaleAfter(2 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q    *Q
		want string
	}{{q2, "toUInt64(7200000)"}, {q, "toUInt64(90000)"}} {
		_, _ = c.q.Routers(ctx)
		s := rc.last()
		if strings.Contains(s, staleAfterToken) || !strings.Contains(s, c.want) {
			t.Errorf("a %v Q's statement: token left in %v, %s present %v",
				c.q.StaleAfter(), strings.Contains(s, staleAfterToken), c.want, strings.Contains(s, c.want))
		}
	}
}

// TestLiveConnRendersEveryStatementMethod: each of the four calls liveConn
// wraps fills the threshold in. Only Query and QueryRow carry a statement
// with the token today; Select and Exec are wrapped so the next caller that
// uses them cannot skip the substitution, and this test is what holds them
// to it.
func TestLiveConnRendersEveryStatementMethod(t *testing.T) {
	ctx := context.Background()
	rc := &recordingConn{}
	c := liveConn{Conn: rc, literal: staleLiteral(2 * time.Hour)}
	stmt := "SELECT " + staleAfterToken
	for _, m := range []struct {
		name string
		call func()
	}{
		{"Query", func() { _, _ = c.Query(ctx, stmt) }},
		{"QueryRow", func() { _ = c.QueryRow(ctx, stmt) }},
		{"Select", func() { _ = c.Select(ctx, nil, stmt) }},
		{"Exec", func() { _ = c.Exec(ctx, stmt) }},
	} {
		m.call()
		if got := rc.last(); got != "SELECT toUInt64(7200000)" {
			t.Errorf("%s sent %q, want the threshold filled in", m.name, got)
		}
	}
}

func TestWithStaleAfterRefusesLessThanTwoBeats(t *testing.T) {
	q, err := New(&recordingConn{}, "vantage_query_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.WithStaleAfter(MinStaleAfter - time.Second); err == nil {
		t.Error("WithStaleAfter accepted a threshold under two beats")
	}
	if _, err := q.WithStaleAfter(MinStaleAfter); err != nil {
		t.Errorf("WithStaleAfter(MinStaleAfter): %v", err)
	}
}

// TestWithStaleAfterRefusesAnAbsurdThreshold: past about 56 years the cutoff
// falls before liveCTE's 1970 placeholder, and a collector never heard from
// would read up. The ceiling is accepted; anything past it, up to the
// largest Duration there is, is not.
func TestWithStaleAfterRefusesAnAbsurdThreshold(t *testing.T) {
	q, err := New(&recordingConn{}, "vantage_query_test")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{MaxStaleAfter + time.Nanosecond, 100 * 365 * 24 * time.Hour, math.MaxInt64} {
		if _, err := q.WithStaleAfter(d); err == nil {
			t.Errorf("WithStaleAfter(%v) accepted a threshold above the ceiling", d)
		}
	}
	if _, err := q.WithStaleAfter(MaxStaleAfter); err != nil {
		t.Errorf("WithStaleAfter(MaxStaleAfter): %v", err)
	}
}

// TestTheThresholdsAreCountedInBeats holds the read side's two numbers to the
// collector's beat interval, so neither can move without the other.
func TestTheThresholdsAreCountedInBeats(t *testing.T) {
	if DefaultStaleAfter != 3*collector.BeatInterval {
		t.Errorf("DefaultStaleAfter = %v, want three beats (%v)", DefaultStaleAfter, 3*collector.BeatInterval)
	}
	if MinStaleAfter != 2*collector.BeatInterval {
		t.Errorf("MinStaleAfter = %v, want two beats (%v)", MinStaleAfter, 2*collector.BeatInterval)
	}
}
