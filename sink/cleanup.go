package sink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The current tables have no TTL: a route that stays stable for longer than
// retention is still live. What removes rows from them is this cleanup,
// which deletes the rows of sessions a router has superseded.
//
// A router's floor is the older of its two newest sessions known to
// peer_current, and cleanup deletes the rows whose session_id is below it.
// Two sessions are kept, not one, so a reconnect that lands while cleanup
// runs never removes the session that was live a moment before. Session ids
// are wall-clock UnixNano per collector, bumped when the clock repeats, so a
// lower id is an older session.
//
// The statements name what to remove, never what to keep. Rows of a new
// session can reach ClickHouse before its Peer Up reaches peer_current (they
// arrive on different streams); a "session_id NOT IN the kept sessions"
// predicate deletes them, while "below the floor" cannot, since a new
// session's id is higher. Rows of a session whose peer rows are already gone,
// such as a straggler insert after an earlier cleanup, are below the floor
// and go.

// cleanupTables are the current tables, in the order cleanup runs, with
// peer_current last so every statement reads its floor from an unpruned
// peer_current. The order does not change what is deleted: removing a
// router's rows below its floor cannot change its two newest sessions.
var cleanupTables = []string{
	"eor_current",
	"route_unicast_current",
	"route_vpn_current",
	"route_evpn_current",
	"ls_nodes_current",
	"ls_links_current",
	"ls_prefixes_current",
	"peer_current",
}

// cleanupFloorSQL is the floor per (collector_id, router_ip); %[1]s is the
// database. It is written inline in each statement, not as a WITH: ClickHouse
// 24.8 resolves a WITH inside a DELETE's IN as a table when the mutation
// runs, fails with UNKNOWN_TABLE on any table that has parts, and succeeds on
// an empty one.
const cleanupFloorSQL = `SELECT collector_id, router_ip, min(session_id) AS floor_sid
    FROM (
        SELECT collector_id, router_ip, session_id,
               row_number() OVER (PARTITION BY collector_id, router_ip ORDER BY session_id DESC) AS rn
        FROM (SELECT DISTINCT collector_id, router_ip, session_id FROM %[1]s.peer_current)
    ) WHERE rn <= 2
    GROUP BY collector_id, router_ip`

// cleanupTargetSQL is the rows below their router's floor in one table;
// %[1]s is the database and %[2]s the table.
const cleanupTargetSQL = `FROM %[1]s.%[2]s AS t
    INNER JOIN (` + cleanupFloorSQL + `) AS floor USING (collector_id, router_ip)
    WHERE t.session_id < floor.floor_sid`

const (
	cleanupCountSQL  = `SELECT count() ` + cleanupTargetSQL
	cleanupDeleteSQL = `DELETE FROM %[1]s.%[2]s WHERE (collector_id, router_ip, session_id) IN (
    SELECT DISTINCT t.collector_id, t.router_ip, t.session_id ` + cleanupTargetSQL + `)`
)

var (
	metricCleanupRowsTargeted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_sink_current_cleanup_rows_targeted_total",
		Help: "Rows of superseded sessions the current-table cleanup deleted, " +
			"counted by a count() with the DELETE's predicate just before it runs.",
	}, []string{"table"})
	metricCleanupDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "vantage_sink_current_cleanup_duration_seconds",
		Help:    "Duration of each current-table cleanup cycle this writer ran.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
	})
	metricCleanupSkipped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_current_cleanup_skipped_total",
		Help: "Current-table cleanup cycles this writer skipped because another " +
			"writer held the lease.",
	})
	metricCleanupErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_sink_current_cleanup_errors_total",
		Help: "Current-table cleanup cycles that failed to take the lease or to " +
			"clean at least one table.",
	})
)

// CleanupSuperseded deletes, from every current table, the rows of sessions
// below their router's floor, and returns the rows targeted per table. A
// table that fails does not stop the others; the errors are joined.
//
// It is idempotent. A second run, or two runs at once, delete what one run
// would: the floor only rises, and the live session is never below it.
// Runs that overlap each count the same rows as targeted. If one run's
// DELETE fails, deleteSync kills every cleanup mutation on that table, the
// other run's too; that run counts an error, and the next cycle removes
// what was left.
func (c *ClickHouse) CleanupSuperseded(ctx context.Context) (map[string]uint64, error) {
	targeted := map[string]uint64{}
	var errs []error
	for _, table := range cleanupTables {
		var n uint64
		if err := c.conn.QueryRow(ctx, fmt.Sprintf(cleanupCountSQL, c.db, table)).Scan(&n); err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s: count: %w", table, err))
			continue
		}
		if n == 0 {
			continue
		}
		if err := c.deleteSync(ctx, table, fmt.Sprintf(cleanupDeleteSQL, c.db, table)); err != nil {
			errs = append(errs, err)
			continue
		}
		targeted[table] = n
	}
	return targeted, errors.Join(errs...)
}

// cleanupKillFilter selects cleanup's own mutations in system.mutations: its
// DELETEs, and only its, name floor_sid, the floor subquery's column.
//
// The underscore is escaped because LIKE reads a bare _ as any one
// character, and a bare pattern would also match "floor-sid" or "floorXsid"
// anywhere in another statement's command -- a lightweight DELETE's command
// carries its literal predicate values, so a collector id like
// "edge-floor-sid-1" in a purge's DELETE would match and the purge could be
// killed. In the SQL text, \\_ is a string literal holding \_, which LIKE
// reads as a literal underscore.
const cleanupKillFilter = `command LIKE '%floor\\_sid%'`

// deleteSync runs a cleanup DELETE and waits for its mutation. A mutation
// that fails is otherwise left queued and retried forever, so on failure the
// table's unfinished cleanup mutations are killed.
//
// A client-side timeout also counts as a failure here, although the
// mutation may still be running and valid. Killing it is harmless: a
// lightweight DELETE killed partway has removed only superseded rows, and
// the next cycle removes the rest.
func (c *ClickHouse) deleteSync(ctx context.Context, table, stmt string) error {
	err := c.conn.Exec(ctx, stmt+" SETTINGS mutations_sync = 2")
	if err == nil {
		return nil
	}
	err = fmt.Errorf("cleanup %s: delete: %w", table, err)
	// Shutdown cancels a statement that is valid and will finish on its own.
	if ctx.Err() != nil {
		return err
	}
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if kerr := c.conn.Exec(killCtx, `KILL MUTATION WHERE database = ? AND table = ?
        AND is_done = 0 AND `+cleanupKillFilter+` SYNC`, c.db, table); kerr != nil {
		err = errors.Join(err, fmt.Errorf("cleanup %s: kill mutation: %w", table, kerr))
	}
	return err
}

// cleanupLeaseHolder is the holder of the newest unexpired lease row, or ""
// when none is unexpired or the newest is a release (holder ""). The holder
// breaks a tie in expires_at so every writer reads the same answer.
func (c *ClickHouse) cleanupLeaseHolder(ctx context.Context) (string, error) {
	var h string
	err := c.conn.QueryRow(ctx, fmt.Sprintf(`SELECT argMax(holder, (expires_at, holder))
        FROM %s.cleanup_lease WHERE expires_at > now64(3)`+cleanupLeaseReadSettings, c.db)).Scan(&h)
	return h, err
}

// cleanupLeaseClaims counts the live claims since the newest release:
// holder's, and every other writer's with the newest of those writers.
// mineUntil is when holder's newest live row expires, in Unix
// milliseconds. A release row outlasts every row written before it, so the
// claims that count are the unexpired ones that expire after it.
//
// That holds while every writer uses the same lease length. A writer
// with a shorter one, such as a new writer during a rollout that shortens
// the interval, writes claims that expire before an old writer's release
// row. They never count, and its cycles fail with "claim ... not found"
// until the release expires, up to the old lease length. No writer runs a
// cycle meanwhile, so this delays cleanup and does not overlap it.
func (c *ClickHouse) cleanupLeaseClaims(ctx context.Context, holder string) (mine, rivals uint64, rival string, mineUntil int64, err error) {
	err = c.conn.QueryRow(ctx, fmt.Sprintf(`SELECT
            countIf(holder = ?),
            countIf(holder != ?),
            argMaxIf(holder, (expires_at, holder), holder != ?),
            toUnixTimestamp64Milli(maxIf(expires_at, holder = ?))
        FROM %[1]s.cleanup_lease
        WHERE holder != ''
          AND expires_at > greatest(now64(3),
              (SELECT max(expires_at) FROM %[1]s.cleanup_lease WHERE holder = ''))`+cleanupLeaseReadSettings, c.db),
		holder, holder, holder, holder).Scan(&mine, &rivals, &rival, &mineUntil)
	return mine, rivals, rival, mineUntil, err
}

// The lease assumes a single ClickHouse server. decideCleanupLease relies
// on an INSERT being visible to every read that starts after it returns.
// One server with synchronous inserts gives that. It does not hold across
// the replicas of a ReplicatedMergeTree or SharedMergeTree table (ClickHouse
// Cloud), across the hosts of a multi-host DSN, or for an asynchronous
// insert that returns before it is written. The lease INSERT sets
// async_insert = 0 so a server or user default cannot make it asynchronous.
// The reads set select_sequential_consistency = 1; on a plain MergeTree it
// changes nothing, and on a replicated table it only helps inserts written
// with insert_quorum, which these are not. On a replicated or cloud
// ClickHouse two writers can both run a cycle. The DELETEs are idempotent,
// so an overlap deletes what one cycle would; see CleanupSuperseded.
const cleanupLeaseReadSettings = ` SETTINGS select_sequential_consistency = 1`

func (c *ClickHouse) writeCleanupLease(ctx context.Context, holder string, d time.Duration) error {
	return c.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.cleanup_lease (holder, expires_at)
        SETTINGS async_insert = 0
        SELECT ?, now64(3) + toIntervalMillisecond(?)`, c.db), holder, d.Milliseconds())
}

// releaseCleanupLease frees the lease if holder's row is the newest, by
// writing a release row (holder "") that outlasts holder's own rows, so the
// next writer's cycle claims it instead of skipping until it expires. The
// caller releases only while it holds the lease: a release outlasts every
// claim before it, so one written by a writer that lost, or whose lease
// lapsed, would free the lease under the writer that holds it.
func (c *ClickHouse) releaseCleanupLease(ctx context.Context, holder string, d time.Duration) error {
	cur, err := c.cleanupLeaseHolder(ctx)
	if err != nil || cur != holder {
		return err
	}
	return c.writeCleanupLease(ctx, "", d)
}

// takeCleanupLease reports whether holder holds the lease for this cycle,
// and otherwise which writer does. held is whether holder held it without
// a break through the end of its previous cycle. until is when holder's
// claim expires, in Unix milliseconds, for renewCleanupLease.
//
// A writer that does not hold the lease claims it only when no other writer
// has a live claim, so a writer that lost does not keep claiming while the
// winner holds it.
func (c *ClickHouse) takeCleanupLease(ctx context.Context, holder string, held bool, d time.Duration) (won bool, cur string, until int64, err error) {
	if !held {
		_, rivals, rival, _, err := c.cleanupLeaseClaims(ctx, holder)
		if err != nil {
			return false, "", 0, fmt.Errorf("cleanup lease: read: %w", err)
		}
		if rivals > 0 {
			return false, rival, 0, nil
		}
	}
	return c.claimCleanupLease(ctx, holder, held, d)
}

// claimCleanupLease writes holder's claim and decides whether it won.
func (c *ClickHouse) claimCleanupLease(ctx context.Context, holder string, held bool, d time.Duration) (won bool, cur string, until int64, err error) {
	if err := c.writeCleanupLease(ctx, holder, d); err != nil {
		return false, "", 0, fmt.Errorf("cleanup lease: claim: %w", err)
	}
	return c.decideCleanupLease(ctx, holder, held)
}

// decideCleanupLease reads the lease back after holder's claim.
//
// A writer taking a free lease wins only if no other writer has a live
// claim. Two writers that both found the lease free both get here. Each
// writes its claim before it reads, and an INSERT is visible to every read
// that starts after it returns on a single ClickHouse server (see
// cleanupLeaseReadSettings), so whichever of the two reads second sees
// the other's claim and loses. A writer finds no claim but its own only if
// the other's was not yet written, and then the other sees it. When each
// sees the other, both lose, and neither claims again until the other's
// claim has expired and its own next cycle comes around: cleanup is delayed
// by up to d plus 1.1 intervals, about 3.1 hours at the default interval.
//
// A writer that held the lease through its previous cycle keeps it if a
// row of its own from before this claim is still live: its rows have then
// been live without a break since it took the lease, so any other writer's
// claim in that time saw one and lost. Without this, a loser's live claim
// would cost the holder its next cycle.
//
// Taking the newest claim as the winner instead lets both writers run
// whenever one's claim lands after the other has read its own back.
func (c *ClickHouse) decideCleanupLease(ctx context.Context, holder string, held bool) (won bool, cur string, until int64, err error) {
	mine, rivals, rival, until, err := c.cleanupLeaseClaims(ctx, holder)
	if err != nil {
		return false, "", 0, fmt.Errorf("cleanup lease: read back: %w", err)
	}
	if mine == 0 {
		return false, "", 0, fmt.Errorf("cleanup lease: read back: claim by %s not found", holder)
	}
	if held && mine >= 2 {
		return true, holder, until, nil
	}
	if rivals > 0 {
		return false, rival, 0, nil
	}
	return true, holder, until, nil
}

// renewCleanupLease writes holder's renewal after a cycle and reports
// whether holder still holds the lease: whether its claim, which expires at
// until (Unix milliseconds), was still live when the renewal landed.
func (c *ClickHouse) renewCleanupLease(ctx context.Context, holder string, d time.Duration, until int64) (bool, error) {
	if err := c.writeCleanupLease(ctx, holder, d); err != nil {
		return false, fmt.Errorf("cleanup lease: renew: %w", err)
	}
	// Checked on the server's clock, which the rows' expiries are on. The
	// writer's own clock can disagree with it, and a monotonic clock does not
	// advance while the host is suspended. The check runs after the renewal
	// returned, so if the claim is live now it was live when the renewal
	// landed.
	var live bool
	if err := c.conn.QueryRow(ctx, "SELECT now64(3) < fromUnixTimestamp64Milli(?)"+cleanupLeaseReadSettings, until).Scan(&live); err != nil {
		return false, fmt.Errorf("cleanup lease: renew: check claim: %w", err)
	}
	return live, nil
}

// RunCleanup runs CleanupSuperseded every interval, plus up to 10% jitter,
// until ctx is done, in whichever writer holds the cleanup lease. holder
// names this writer in the lease; it must differ between writers.
//
// The lease lasts two intervals from the end of the holder's last cycle, so
// a holder's next cycle, at most 1.1 intervals later, renews it before it
// expires. A holder that returns between cycles because ctx is done
// releases the lease, so a rollout does not leave the new writers skipping
// for two intervals. One stopped mid-cycle cannot renew, so it does not
// hold the lease and does not release it; like one that dies without
// returning, it is replaced within about two intervals.
func RunCleanup(ctx context.Context, c *ClickHouse, interval time.Duration, holder string, log *slog.Logger) {
	if interval <= 0 {
		return
	}
	runCleanup(ctx, c, interval, 2*interval, holder, log)
}

// runCleanup is RunCleanup with the lease length given. A cycle that runs
// longer than lease can outlast its claim, and another writer can then take
// the lease while it runs.
func runCleanup(ctx context.Context, c *ClickHouse, interval, lease time.Duration, holder string, log *slog.Logger) {
	var held bool
	defer func() {
		if !held {
			return
		}
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := c.releaseCleanupLease(relCtx, holder, lease); err != nil {
			log.Warn("current cleanup: release lease", "err", err)
		}
	}()
	for _, table := range cleanupTables {
		metricCleanupRowsTargeted.WithLabelValues(table)
	}
	for {
		jitter := time.Duration(rand.Int64N(int64(interval)/10 + 1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval + jitter):
		}
		held = c.cleanupCycle(ctx, holder, held, lease, c.CleanupSuperseded, log)
	}
}

// cleanupCycle runs one cycle of work if holder takes the lease, and
// reports whether holder still holds it after the cycle: whether it won,
// renewed, and its claim was still live when the renewal landed. work is
// CleanupSuperseded.
func (c *ClickHouse) cleanupCycle(ctx context.Context, holder string, held bool, lease time.Duration,
	work func(context.Context) (map[string]uint64, error), log *slog.Logger) bool {
	won, cur, until, err := c.takeCleanupLease(ctx, holder, held, lease)
	if err != nil {
		if ctx.Err() == nil {
			metricCleanupErrors.Inc()
			log.Warn("current cleanup: lease", "err", err)
		}
		return false
	}
	if !won {
		metricCleanupSkipped.Inc()
		log.Info("current cleanup skipped: another writer holds the lease", "holder", cur)
		return false
	}
	start := time.Now()
	targeted, err := work(ctx)
	elapsed := time.Since(start)
	metricCleanupDuration.Observe(elapsed.Seconds())
	attrs := []any{"holder", holder, "duration", elapsed.Round(time.Millisecond)}
	for _, table := range cleanupTables {
		metricCleanupRowsTargeted.WithLabelValues(table).Add(float64(targeted[table]))
		attrs = append(attrs, table, targeted[table])
	}
	if err != nil && ctx.Err() == nil {
		metricCleanupErrors.Inc()
		attrs = append(attrs, "err", err)
	}
	// Renewed after the cycle so the lease outlasts the next sleep however
	// long the cycle took.
	stillHeld, rerr := c.renewCleanupLease(ctx, holder, lease, until)
	if rerr != nil && ctx.Err() == nil {
		log.Warn("current cleanup: renew lease", "err", rerr)
	}
	if rerr == nil && !stillHeld {
		log.Warn("current cleanup: the cycle outlasted the lease; another writer may have taken it")
	}
	log.Info("current cleanup", attrs...)
	return stillHeld
}
