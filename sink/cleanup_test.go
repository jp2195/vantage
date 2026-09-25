package sink

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// Every cleanup test owns its database. CleanupSuperseded deletes superseded
// rows of every router in the database it is pointed at, so run in testDB it
// would delete other tests' fixtures while they read them.

// sessionEnvelopes is one row in every current table for (router, session),
// Peer Up included, plus routes unicast routes. withPeerUp false drops the
// Peer Up: rows of a session peer_current does not know.
func sessionEnvelopes(router string, session uint64, routes int, withPeerUp bool) []*vantagev1.Envelope {
	var out []*vantagev1.Envelope
	for _, e := range oneEnvelopeOfEachKind(session) {
		if e.GetPeerEvent() != nil && !withPeerUp {
			continue
		}
		e.Router.Ip = router
		out = append(out, e)
	}
	for i := range routes {
		out = append(out, routeEnvelope(router, session, uint64(1000+i), sessionPrefix(session, i), attrsVia("192.0.2.1", 65000)))
	}
	return out
}

func sessionPrefix(session uint64, i int) string {
	return fmt.Sprintf("10.%d.%d.0/24", session/1000%200, i)
}

// insertEnvelopes inserts through the writer's real path, one insert each,
// with stream_seq continuing from *stream.
func insertEnvelopes(t *testing.T, ctx context.Context, c *ClickHouse, stream *uint64, envs []*vantagev1.Envelope) {
	t.Helper()
	var seq []seqEnvelope
	for _, e := range envs {
		*stream++
		seq = append(seq, seqEnvelope{e, *stream})
	}
	insertEach(t, ctx, c, seq)
}

// sessionRowCount is how many rows table holds for (router, session) of the
// collector the fixtures default to.
func sessionRowCount(t *testing.T, ctx context.Context, c *ClickHouse, table, router string, session uint64) uint64 {
	t.Helper()
	return collectorSessionRowCount(t, ctx, c, table, "current-test", router, session)
}

func collectorSessionRowCount(t *testing.T, ctx context.Context, c *ClickHouse, table, collector, router string, session uint64) uint64 {
	t.Helper()
	var n uint64
	q := "SELECT count() FROM vantage." + table +
		" WHERE collector_id = ? AND router_ip = toIPv6(?) AND session_id = ?"
	if err := c.conn.QueryRow(ctx, qualify(c, q), collector, router, session).Scan(&n); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n
}

// truncateTables empties tables in c's database, which must be a scratch
// database ending in _test.
func truncateTables(t *testing.T, ctx context.Context, c *ClickHouse, tables []string) {
	t.Helper()
	if !strings.HasSuffix(c.db, "_test") {
		t.Fatalf("refusing to truncate tables in %q: not a _test database", c.db)
	}
	for _, table := range tables {
		if err := c.conn.Exec(ctx, fmt.Sprintf("TRUNCATE TABLE %s.%s", c.db, table)); err != nil {
			t.Fatalf("truncate %s.%s: %v", c.db, table, err)
		}
	}
}

// requireNoUnfinishedMutations fails if a DELETE left a mutation behind. A
// mutation that fails is retried forever until killed.
func requireNoUnfinishedMutations(t *testing.T, ctx context.Context, c *ClickHouse) {
	t.Helper()
	var n uint64
	if err := c.conn.QueryRow(ctx,
		"SELECT count() FROM system.mutations WHERE database = ? AND is_done = 0", c.db).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%s has %d unfinished mutations", c.db, n)
	}
}

// requireRowsInEveryTable fails unless every current table holds rows of
// (router, session), and returns the counts. The cleanup DELETE form that
// 24.8 rejects succeeds on an empty table, so a table with no rows proves
// nothing.
func requireRowsInEveryTable(t *testing.T, ctx context.Context, c *ClickHouse, router string, session uint64) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, table := range cleanupTables {
		n := sessionRowCount(t, ctx, c, table, router, session)
		if n == 0 {
			t.Fatalf("fixture: %s holds no rows of session %d", table, session)
		}
		out[table] = n
	}
	return out
}

func TestCleanupKeepsCurrentAndPreviousSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_keeps_test")
	defer c.Close()

	const router = "192.0.2.210"
	const s1, s2, s3 = 1_000, 2_000, 3_000
	var stream uint64
	for _, s := range []uint64{s1, s2, s3} {
		insertEnvelopes(t, ctx, c, &stream, sessionEnvelopes(router, s, 10, true))
	}
	want := requireRowsInEveryTable(t, ctx, c, router, s1)

	got, err := c.CleanupSuperseded(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("rows targeted = %v, want S1's rows %v", got, want)
	}
	for _, table := range cleanupTables {
		if n := sessionRowCount(t, ctx, c, table, router, s1); n != 0 {
			t.Errorf("%s still holds %d rows of S1, the superseded session", table, n)
		}
	}
	requireRowsInEveryTable(t, ctx, c, router, s2)
	requireRowsInEveryTable(t, ctx, c, router, s3)
	requireNoUnfinishedMutations(t, ctx, c)
}

// Route rows of a new session can reach ClickHouse before its Peer Up
// reaches peer_current: they travel on different streams, consumed
// independently. Cleanup must not read "not known to peer_current" as
// "superseded".
func TestCleanupNeverDeletesASessionNotYetInPeerCurrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_unknown_test")
	defer c.Close()

	const router = "192.0.2.211"
	const s1, s2, s3, s4 = 1_000, 2_000, 3_000, 4_000
	var stream uint64
	for _, s := range []uint64{s1, s2, s3} {
		insertEnvelopes(t, ctx, c, &stream, sessionEnvelopes(router, s, 10, true))
	}
	var s4Routes []*vantagev1.Envelope
	for i := range 5 {
		s4Routes = append(s4Routes, routeEnvelope(router, s4, uint64(i+1), sessionPrefix(s4, i), attrsVia("192.0.2.4", 65000)))
	}
	insertEnvelopes(t, ctx, c, &stream, s4Routes)
	requireRowsInEveryTable(t, ctx, c, router, s1)
	if n := sessionRowCount(t, ctx, c, "peer_current", router, s4); n != 0 {
		t.Fatalf("fixture: peer_current knows S4 (%d rows)", n)
	}

	if _, err := c.CleanupSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	if n := sessionRowCount(t, ctx, c, "route_unicast_current", router, s4); n != 5 {
		t.Errorf("route_unicast_current holds %d of S4's 5 routes after cleanup", n)
	}
	// The cleanup did run: S1 is gone.
	if n := sessionRowCount(t, ctx, c, "route_unicast_current", router, s1); n != 0 {
		t.Errorf("route_unicast_current still holds %d rows of S1", n)
	}
	requireNoUnfinishedMutations(t, ctx, c)
}

// A straggler insert of S1 can land after an earlier cleanup removed S1's
// Peer Up from peer_current. Its rows are below the floor and must go, even
// in a table where the session between S1 and the live one has no rows
// (S2 went down before it sent any): the floor comes from peer_current,
// not from the sessions a table happens to hold.
func TestCleanupRemovesOrphanedOldStragglers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_straggler_test")
	defer c.Close()

	const router = "192.0.2.212"
	const s1, s2, s3 = 1_000, 2_000, 3_000
	var stream uint64
	insertEnvelopes(t, ctx, c, &stream, []*vantagev1.Envelope{peerUpEnvelope(router, s2)})
	insertEnvelopes(t, ctx, c, &stream, sessionEnvelopes(router, s3, 10, true))
	insertEnvelopes(t, ctx, c, &stream, sessionEnvelopes(router, s1, 10, false))
	if n := sessionRowCount(t, ctx, c, "peer_current", router, s1); n != 0 {
		t.Fatalf("fixture: peer_current knows S1 (%d rows)", n)
	}
	var before uint64
	for _, table := range cleanupTables {
		before += sessionRowCount(t, ctx, c, table, router, s1)
	}
	if before == 0 {
		t.Fatal("fixture: no S1 rows")
	}

	if _, err := c.CleanupSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range cleanupTables {
		if n := sessionRowCount(t, ctx, c, table, router, s1); n != 0 {
			t.Errorf("%s still holds %d straggler rows of S1", table, n)
		}
	}
	if n := sessionRowCount(t, ctx, c, "peer_current", router, s2); n == 0 {
		t.Error("peer_current lost S2's Peer Up")
	}
	requireRowsInEveryTable(t, ctx, c, router, s3)
	requireNoUnfinishedMutations(t, ctx, c)
}

// syncBuffer is a bytes.Buffer a logger and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Two writers run the cleanup loop against one database. writer-a takes the
// lease on its first cycle; writer-b, started after that, must skip every
// cycle while writer-a keeps the lease, and must not claim it.
//
// writer-b starts after writer-a's first cycle, and the lease is a minute,
// so neither the order in which the two first claim nor how long a cycle
// takes decides the outcome. Two writers that claim a free lease together
// are covered by TestCleanupLeaseLateClaimDoesNotAlsoRun and
// TestCleanupLeaseClaimsThatSeeEachOtherBothLose.
func TestCleanupLeaseLetsOneWriterRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_lease_test")
	defer c.Close()
	// chtest drops the database once per process, so under -count a run
	// would start with the fixtures and the lease rows of the runs before
	// it, live for the minute the lease lasts. The lease-row counts below
	// are per holder over the whole table.
	truncateTables(t, ctx, c, append([]string{"cleanup_lease"}, cleanupTables...))

	const router = "192.0.2.213"
	const s1, s2, s3 = 1_000, 2_000, 3_000
	var stream uint64
	for _, s := range []uint64{s1, s2, s3} {
		insertEnvelopes(t, ctx, c, &stream, sessionEnvelopes(router, s, 10, true))
	}
	want := requireRowsInEveryTable(t, ctx, c, router, s1)
	before := map[string]float64{}
	for _, table := range cleanupTables {
		before[table] = testutil.ToFloat64(metricCleanupRowsTargeted.WithLabelValues(table))
	}

	holders := []string{"writer-a", "writer-b"}
	logs := map[string]*syncBuffer{}
	count := func(h, msg string) int {
		return strings.Count(logs[h].String(), `msg="`+msg+`"`)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	start := func(h string) {
		logs[h] = &syncBuffer{}
		log := slog.New(slog.NewTextHandler(logs[h], &slog.HandlerOptions{Level: slog.LevelInfo}))
		wg.Go(func() { runCleanup(runCtx, c, 100*time.Millisecond, time.Minute, h, log) })
	}
	start("writer-a")
	waitUntil(t, ctx, "writer-a's first cycle", func() bool {
		return count("writer-a", "current cleanup") >= 1
	})
	start("writer-b")
	// Stops early if writer-b runs a cycle, which fails below.
	waitUntil(t, ctx, "three more writer-a cycles and three writer-b skips", func() bool {
		return count("writer-b", "current cleanup") > 0 ||
			count("writer-a", "current cleanup") >= 4 &&
				count("writer-b", "current cleanup skipped: another writer holds the lease") >= 3
	})
	// writer-c's claim lands and loses to writer-a. It stays live for the
	// rest of the test, and writer-a, holding the lease from cycle to cycle,
	// keeps running. Stops early if writer-a skips a cycle, which fails
	// below.
	if won, _, _, err := c.claimCleanupLease(ctx, "writer-c", false, time.Minute); err != nil || won {
		t.Fatalf("writer-c's claim: won %v, err %v; want it to lose to writer-a", won, err)
	}
	ranBefore := count("writer-a", "current cleanup")
	waitUntil(t, ctx, "two writer-a cycles after writer-c's claim", func() bool {
		return count("writer-a", "current cleanup skipped: another writer holds the lease") > 0 ||
			count("writer-a", "current cleanup") >= ranBefore+2
	})
	stop()
	wg.Wait()
	if n := count("writer-a", "current cleanup skipped: another writer holds the lease"); n > 0 {
		t.Errorf("writer-a skipped %d cycles while it held the lease: %q", n, logs["writer-a"].String())
	}

	cycles := map[string]int{}
	for _, h := range holders {
		cycles[h] = count(h, "current cleanup")
	}
	t.Logf("cleanup cycles per holder: %v", cycles)
	if cycles["writer-b"] != 0 {
		t.Errorf("writer-b ran %d cycles while writer-a held the lease", cycles["writer-b"])
		for _, h := range holders {
			t.Logf("%s log:\n%s", h, logs[h].String())
		}
	}
	if !strings.Contains(logs["writer-b"].String(), "holder=writer-a") {
		t.Errorf("writer-b's skips do not name writer-a: %q", logs["writer-b"].String())
	}

	rows, err := c.conn.Query(ctx, qualify(c,
		"SELECT holder, count() FROM vantage.cleanup_lease GROUP BY holder"))
	if err != nil {
		t.Fatal(err)
	}
	leaseRows := map[string]uint64{}
	for rows.Next() {
		var h string
		var n uint64
		if err := rows.Scan(&h, &n); err != nil {
			t.Fatal(err)
		}
		leaseRows[h] = n
	}
	rows.Close()
	t.Logf("lease rows per holder: %v", leaseRows)
	if leaseRows["writer-b"] != 0 {
		t.Errorf("writer-b wrote %d lease rows; a writer that finds the lease held must not claim it",
			leaseRows["writer-b"])
	}
	// writer-a writes a claim before every cycle.
	if leaseRows["writer-a"] < uint64(cycles["writer-a"]) {
		t.Errorf("writer-a ran %d cycles but wrote %d lease rows", cycles["writer-a"], leaseRows["writer-a"])
	}

	// Only the first cycle finds S1's rows, so the metric moves by exactly
	// one cleanup's worth however many cycles ran.
	for _, table := range cleanupTables {
		got := testutil.ToFloat64(metricCleanupRowsTargeted.WithLabelValues(table)) - before[table]
		if got != float64(want[table]) {
			t.Errorf("rows targeted in %s moved by %v, want %d", table, got, want[table])
		}
	}
	requireNoUnfinishedMutations(t, ctx, c)
}

// Two writers find the lease free, and writer-b's claim lands after
// writer-a has read its own claim back. writer-a saw no rival, so it runs;
// writer-b must not run as well. Taking the newest claim as the winner let
// both run: writer-a found only its own claim, writer-b found its own newer
// one. After that, writer-a keeps the lease and writer-b does not claim
// again while writer-a holds it.
func TestCleanupLeaseLateClaimDoesNotAlsoRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_late_claim_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const d = time.Minute
	wonA, curA, untilA, err := c.takeCleanupLease(ctx, "writer-a", false, d)
	if err != nil {
		t.Fatal(err)
	}
	if !wonA {
		t.Fatalf("writer-a found the lease free but did not take it; holder is %q", curA)
	}
	// writer-b read the lease free before writer-a's claim landed, so it
	// goes straight to claiming.
	wonB, curB, _, err := c.claimCleanupLease(ctx, "writer-b", false, d)
	if err != nil {
		t.Fatal(err)
	}
	if wonB {
		t.Errorf("writer-b took the lease while writer-a holds it; both run a cycle")
	}
	if curB != "writer-a" {
		t.Errorf("writer-b lost the lease to %q, want writer-a", curB)
	}

	// writer-a renews after its cycle and claims its next one. writer-b's
	// losing claim is still live and must not cost writer-a the lease.
	heldA, err := c.renewCleanupLease(ctx, "writer-a", d, untilA)
	if err != nil {
		t.Fatal(err)
	}
	if !heldA {
		t.Fatalf("writer-a renewed within its lease but does not hold it")
	}
	wonA, curA, _, err = c.takeCleanupLease(ctx, "writer-a", heldA, d)
	if err != nil {
		t.Fatal(err)
	}
	if !wonA {
		t.Errorf("writer-a lost the lease it holds to %q, whose claim lost to it", curA)
	}

	// writer-b's next cycle finds writer-a holding the lease and writes no
	// claim.
	before := leaseRowsOf(t, ctx, c, "writer-b")
	wonB, curB, _, err = c.takeCleanupLease(ctx, "writer-b", false, d)
	if err != nil {
		t.Fatal(err)
	}
	if wonB || curB != "writer-a" {
		t.Errorf("writer-b's next cycle: won %v, holder %q; want writer-a to hold it", wonB, curB)
	}
	if after := leaseRowsOf(t, ctx, c, "writer-b"); after != before {
		t.Errorf("writer-b wrote %d claims while writer-a holds the lease", after-before)
	}
}

// Two writers find the lease free and each reads back after the other's
// claim landed. Neither may run: each saw a rival, and neither can tell the
// other also did.
func TestCleanupLeaseClaimsThatSeeEachOtherBothLose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_both_see_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const d = time.Minute
	for _, h := range []string{"writer-a", "writer-b"} {
		if err := c.writeCleanupLease(ctx, h, d); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"writer-a", "writer-b"} {
		won, cur, _, err := c.decideCleanupLease(ctx, h, false)
		if err != nil {
			t.Fatal(err)
		}
		if won {
			t.Errorf("%s won the lease; each writer saw the other's claim, want neither to win", h)
		}
		if cur == h || cur == "" {
			t.Errorf("%s lost the lease to %q, want the other writer", h, cur)
		}
	}
}

// A writer whose cycle outlasts its claim no longer holds the lease, even
// though its renewal lands: another writer can have taken the lease in
// between. Here writer-b does. writer-a must not count itself the holder,
// must lose its next cycle to writer-b, and must not release writer-b's
// lease when it shuts down.
func TestCleanupLeaseCycleThatOutlastsItsClaimLosesTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_outlasts_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const short, long = 300 * time.Millisecond, time.Minute
	wonA, curA, untilA, err := c.takeCleanupLease(ctx, "writer-a", false, short)
	if err != nil || !wonA {
		t.Fatalf("writer-a on a free lease: won %v, holder %q, err %v", wonA, curA, err)
	}
	// writer-a's cycle runs past its claim, and writer-b takes the lease.
	waitForLeaseExpiry(t, ctx, c, untilA)
	if wonB, curB, _, err := c.takeCleanupLease(ctx, "writer-b", false, long); err != nil || !wonB {
		t.Fatalf("writer-b after writer-a's claim expired: won %v, holder %q, err %v", wonB, curB, err)
	}

	heldA, err := c.renewCleanupLease(ctx, "writer-a", long, untilA)
	if err != nil {
		t.Fatal(err)
	}
	if heldA {
		t.Errorf("writer-a renewed after its claim expired and counts itself the holder")
	}
	if cur, err := c.cleanupLeaseHolder(ctx); err != nil || cur != "writer-a" {
		t.Fatalf("fixture: newest lease row is %q (err %v), want writer-a's renewal", cur, err)
	}

	wonA, curA, _, err = c.takeCleanupLease(ctx, "writer-a", heldA, long)
	if err != nil {
		t.Fatal(err)
	}
	if wonA || curA != "writer-b" {
		t.Errorf("writer-a's next cycle: won %v, holder %q; want writer-b to hold the lease", wonA, curA)
	}

	// writer-a shuts down, not holding the lease.
	done, stopA := context.WithCancel(ctx)
	stopA()
	runCleanup(done, c, time.Hour, long, "writer-a", slog.New(slog.NewTextHandler(&syncBuffer{}, nil)))
	if wonC, curC, _, err := c.takeCleanupLease(ctx, "writer-c", false, long); err != nil || wonC {
		t.Errorf("writer-c after writer-a shut down: won %v, holder %q, err %v; want writer-b to keep the lease",
			wonC, curC, err)
	}
}

// The same through a whole cycle: writer-a's cycle runs past its claim and
// writer-b takes the lease meanwhile. The cycle must report that writer-a
// no longer holds the lease, so writer-a skips its next cycle.
func TestCleanupCycleThatOutlastsItsClaimDoesNotKeepTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_cycle_outlasts_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const short, long = 300 * time.Millisecond, time.Minute
	slow := func(ctx context.Context) (map[string]uint64, error) {
		_, _, _, untilA, err := c.cleanupLeaseClaims(ctx, "writer-a")
		if err != nil {
			return nil, err
		}
		waitForLeaseExpiry(t, ctx, c, untilA)
		if wonB, curB, _, err := c.takeCleanupLease(ctx, "writer-b", false, long); err != nil || !wonB {
			t.Errorf("writer-b during writer-a's cycle: won %v, holder %q, err %v", wonB, curB, err)
		}
		return map[string]uint64{}, nil
	}
	logA := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logA, nil))
	held := c.cleanupCycle(ctx, "writer-a", false, short, slow, log)
	if held {
		t.Errorf("writer-a's cycle outlasted its claim, and writer-b took the lease, but writer-a still counts itself the holder")
	}
	if !strings.Contains(logA.String(), `msg="current cleanup"`) {
		t.Fatalf("fixture: writer-a ran no cycle: %q", logA.String())
	}
	if c.cleanupCycle(ctx, "writer-a", held, long, c.CleanupSuperseded, log) {
		t.Errorf("writer-a took the lease back from writer-b")
	}
	if n := strings.Count(logA.String(), `msg="current cleanup"`); n != 1 {
		t.Errorf("writer-a ran %d cycles, want 1: its second must skip while writer-b holds the lease", n)
	}
}

// A holder keeps running cycles while a rival's losing claim is live: the
// held state from one cycle carries into the next.
func TestCleanupCycleHolderKeepsTheLeaseThroughALosingClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_cycle_keeps_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const d = time.Minute
	logA := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logA, nil))
	held := c.cleanupCycle(ctx, "writer-a", false, d, c.CleanupSuperseded, log)
	if !held {
		t.Fatalf("writer-a does not hold the lease after a cycle on a free lease: %q", logA.String())
	}
	if won, _, _, err := c.claimCleanupLease(ctx, "writer-b", false, d); err != nil || won {
		t.Fatalf("writer-b's late claim: won %v, err %v; want it to lose", won, err)
	}
	if !c.cleanupCycle(ctx, "writer-a", held, d, c.CleanupSuperseded, log) {
		t.Errorf("writer-a lost the lease to writer-b's losing claim")
	}
	if n := strings.Count(logA.String(), `msg="current cleanup"`); n != 2 {
		t.Errorf("writer-a ran %d cycles, want 2: %q", n, logA.String())
	}
}

// A holder paused past its lease -- a stalled process or a suspended host --
// comes back believing it holds the lease, but its rows have expired and
// another writer has taken it. The old holder's next claim must lose.
func TestCleanupLeaseHolderPausedPastItsLeaseLoses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_paused_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const short, long = 300 * time.Millisecond, time.Minute
	wonA, curA, untilA, err := c.takeCleanupLease(ctx, "writer-a", false, short)
	if err != nil || !wonA {
		t.Fatalf("writer-a on a free lease: won %v, holder %q, err %v", wonA, curA, err)
	}
	heldA, err := c.renewCleanupLease(ctx, "writer-a", short, untilA)
	if err != nil || !heldA {
		t.Fatalf("writer-a renewing within its lease: held %v, err %v", heldA, err)
	}
	// writer-a pauses; its rows expire and writer-b takes the lease.
	_, _, _, renewedUntil, err := c.cleanupLeaseClaims(ctx, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	waitForLeaseExpiry(t, ctx, c, renewedUntil)
	if wonB, curB, _, err := c.takeCleanupLease(ctx, "writer-b", false, long); err != nil || !wonB {
		t.Fatalf("writer-b after writer-a's rows expired: won %v, holder %q, err %v", wonB, curB, err)
	}

	wonA, curA, _, err = c.takeCleanupLease(ctx, "writer-a", heldA, long)
	if err != nil {
		t.Fatal(err)
	}
	if wonA {
		t.Errorf("writer-a took the lease back from writer-b after its own rows expired; both run a cycle")
	}
	if curA != "writer-b" {
		t.Errorf("writer-a lost the lease to %q, want writer-b", curA)
	}
}

// A writer that lost the lease can own the newest lease row, when its claim
// landed after the winner's. When it shuts down it must not release the
// lease: a release outlasts every claim before it, the winner's too, so a
// third writer would take the lease while the winner holds it.
func TestCleanupLeaseLoserDoesNotRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_loser_release_test")
	defer c.Close()
	truncateTables(t, ctx, c, []string{"cleanup_lease"})

	const d = time.Minute
	if won, cur, _, err := c.takeCleanupLease(ctx, "writer-a", false, d); err != nil || !won {
		t.Fatalf("writer-a on a free lease: won %v, holder %q, err %v", won, cur, err)
	}
	if won, _, _, err := c.claimCleanupLease(ctx, "writer-b", false, d); err != nil || won {
		t.Fatalf("writer-b's late claim: won %v, err %v; want it to lose", won, err)
	}
	if cur, err := c.cleanupLeaseHolder(ctx); err != nil || cur != "writer-b" {
		t.Fatalf("fixture: newest lease row is %q (err %v), want writer-b's losing claim", cur, err)
	}

	// writer-b shuts down before it runs a cycle.
	done, stopB := context.WithCancel(ctx)
	stopB()
	runCleanup(done, c, time.Hour, d, "writer-b", slog.New(slog.NewTextHandler(&syncBuffer{}, nil)))

	won, cur, _, err := c.takeCleanupLease(ctx, "writer-c", false, d)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Errorf("writer-c took the lease writer-a holds, after writer-b released it")
	}
	if cur != "writer-a" && cur != "writer-b" {
		t.Errorf("writer-c lost the lease to %q, want writer-a or writer-b's live claim", cur)
	}
}

// waitForLeaseExpiry waits until the server's clock passes until, a lease
// expiry in Unix milliseconds.
func waitForLeaseExpiry(t *testing.T, ctx context.Context, c *ClickHouse, until int64) {
	t.Helper()
	waitUntil(t, ctx, "the lease to expire", func() bool {
		var passed bool
		if err := c.conn.QueryRow(ctx, "SELECT now64(3) >= fromUnixTimestamp64Milli(?)", until).Scan(&passed); err != nil {
			t.Fatal(err)
		}
		return passed
	})
}

// waitUntil polls cond until it holds, failing the test if ctx ends first.
func waitUntil(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", what, ctx.Err())
		case <-tick.C:
		}
	}
}

// leaseRowsOf is how many lease rows holder has written.
func leaseRowsOf(t *testing.T, ctx context.Context, c *ClickHouse, holder string) uint64 {
	t.Helper()
	var n uint64
	if err := c.conn.QueryRow(ctx, qualify(c,
		"SELECT count() FROM vantage.cleanup_lease WHERE holder = ?"), holder).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A DELETE that fails at mutation time must not stay queued. This is the
// form 24.8 rejects (a WITH inside the IN, named as a table at mutation
// time), run against a non-empty table.
func TestCleanupDeleteKillsAFailedMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_kill_test")
	defer c.Close()

	const router = "192.0.2.214"
	var stream uint64
	insertEnvelopes(t, ctx, c, &stream, sessionEnvelopes(router, 1_000, 10, true))

	bad := "DELETE FROM " + c.db + ".route_unicast_current WHERE (collector_id, router_ip, session_id) IN " +
		"(WITH floor AS (SELECT collector_id, router_ip, max(session_id) AS floor_sid FROM " + c.db + ".peer_current " +
		"GROUP BY collector_id, router_ip) SELECT DISTINCT t.collector_id, t.router_ip, t.session_id FROM " +
		c.db + ".route_unicast_current AS t INNER JOIN floor USING (collector_id, router_ip) " +
		"WHERE t.session_id < floor.floor_sid)"
	if err := c.deleteSync(ctx, "route_unicast_current", bad); err == nil {
		t.Fatal("the WITH-in-IN DELETE succeeded; this test no longer exercises a failed mutation")
	}
	requireNoUnfinishedMutations(t, ctx, c)
}

// Every current table has no TTL, so one the cleanup does not cover grows
// without bound. The list is read from schema.sql.
func TestCleanupCoversEveryCurrentTable(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var inSchema []string
	for _, m := range regexp.MustCompile(`CREATE TABLE IF NOT EXISTS vantage\.(\w+_current)\b`).
		FindAllStringSubmatch(string(ddl), -1) {
		inSchema = append(inSchema, m[1])
	}
	if len(inSchema) < 8 {
		t.Fatalf("found %d current tables in schema.sql, want at least 8: %v", len(inSchema), inSchema)
	}
	if got, want := slices.Sorted(slices.Values(cleanupTables)), slices.Sorted(slices.Values(inSchema)); !slices.Equal(got, want) {
		t.Errorf("cleanup covers %v, schema.sql's current tables are %v", got, want)
	}
}

// Session ids are minted per collector, so two collectors watching the same
// router have unrelated ids, and each (collector, router) has its own floor.
// Collector A's only session, 100, is live even though collector B's
// sessions of the same router are all higher.
func TestCleanupFloorIsPerCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_collectors_test")
	defer c.Close()

	const router = "192.0.2.215"
	const collectorA, collectorB = "collector-a", "collector-b"
	sessionsOf := map[string][]uint64{collectorA: {100}, collectorB: {150, 200, 300}}
	var stream uint64
	for _, collector := range []string{collectorA, collectorB} {
		for _, s := range sessionsOf[collector] {
			envs := sessionEnvelopes(router, s, 10, true)
			for _, e := range envs {
				e.CollectorId = collector
			}
			insertEnvelopes(t, ctx, c, &stream, envs)
		}
	}
	for _, collector := range []string{collectorA, collectorB} {
		for _, s := range sessionsOf[collector] {
			for _, table := range cleanupTables {
				if collectorSessionRowCount(t, ctx, c, table, collector, router, s) == 0 {
					t.Fatalf("fixture: %s holds no rows of %s session %d", table, collector, s)
				}
			}
		}
	}

	if _, err := c.CleanupSuperseded(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range cleanupTables {
		for _, kept := range []struct {
			collector string
			session   uint64
		}{{collectorA, 100}, {collectorB, 200}, {collectorB, 300}} {
			if collectorSessionRowCount(t, ctx, c, table, kept.collector, router, kept.session) == 0 {
				t.Errorf("%s lost %s session %d", table, kept.collector, kept.session)
			}
		}
		// B's own floor is 200, so the cleanup did run.
		if n := collectorSessionRowCount(t, ctx, c, table, collectorB, router, 150); n != 0 {
			t.Errorf("%s still holds %d rows of %s session 150", table, n, collectorB)
		}
	}
	requireNoUnfinishedMutations(t, ctx, c)
}

// The writer is a Deployment, so a rollout replaces the lease holder. A
// holder that shuts down releases the lease, so the next writer cleans on
// its next cycle rather than skipping for up to two intervals. A writer that
// finds the lease held says so at Info and in a counter.
func TestCleanupShutdownReleasesTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_cleanup_release_test")
	defer c.Close()

	const interval = time.Second
	logA := &syncBuffer{}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunCleanup(runCtx, c, interval, "writer-a", slog.New(slog.NewTextHandler(logA, nil)))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logA.String(), `msg="current cleanup"`) {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("writer-a ran no cleanup cycle in 10 s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// While writer-a holds the lease, writer-b's cycle is a visible skip.
	skippedBefore := testutil.ToFloat64(metricCleanupSkipped)
	logB := &syncBuffer{}
	c.cleanupCycle(ctx, "writer-b", false, 2*interval, c.CleanupSuperseded, slog.New(slog.NewTextHandler(logB, nil)))
	if got := testutil.ToFloat64(metricCleanupSkipped) - skippedBefore; got != 1 {
		t.Errorf("skipped counter moved by %v, want 1", got)
	}
	if !strings.Contains(logB.String(), "level=INFO") || !strings.Contains(logB.String(), "holder=writer-a") {
		t.Errorf("writer-b's skip was not logged at Info naming writer-a: %q", logB.String())
	}

	stop()
	<-done
	// writer-a's lease had up to two intervals left; released, it is free now.
	held, cur, _, err := c.takeCleanupLease(ctx, "writer-b", false, 2*interval)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Errorf("writer-b could not take the lease after writer-a shut down; holder is %q", cur)
	}
}
