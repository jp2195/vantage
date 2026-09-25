package query

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// DefaultStaleAfter is how long a collector may go unheard before the peers
// of its current sessions read "stale": three missed heartbeats
// (collector.BeatInterval is 30 s). The API's stale_after and the CLI's
// -stale-after both default to it, so the two read paths cannot disagree
// unless an operator makes them.
const DefaultStaleAfter = 90 * time.Second

// MinStaleAfter is the shortest threshold WithStaleAfter accepts: two
// heartbeat intervals. Anything shorter reads a healthy collector stale
// between one beat and the next.
const MinStaleAfter = 60 * time.Second

// MaxStaleAfter is the longest threshold WithStaleAfter accepts: one day.
//
// A ceiling, rather than teaching the stale check to tell a real beat from
// liveCTE's placeholder, because the placeholder is not the only thing a huge
// threshold breaks. A collector never heard from reads stale only because
// its placeholder beat of 1970 is older than the cutoff, now minus the
// threshold; a threshold past about 56 years puts that cutoff before 1970,
// and the silent collector reads up. Past that, the cutoff leaves
// DateTime64's range altogether and the comparison means nothing for a
// collector that did beat either. Bounding the input keeps every cutoff a
// recent instant, so the one comparison beatStaleExpr makes stays right for
// both. A day of silence is already far past any reading of "stale" -- the
// collector beats every 30 s -- so the ceiling refuses nothing an operator
// could mean.
const MaxStaleAfter = 24 * time.Hour

// PeerStateStale is the state a peer resolves to when its stored state is up
// and its collector has not been heard from within the stale threshold.
const PeerStateStale = "stale"

// staleAfterToken is where the stale threshold sits in this package's SQL.
// liveConn replaces it with a literal before every statement reaches
// ClickHouse.
//
// It is spelled as a ClickHouse query parameter on purpose, and deliberately
// NOT bound as one. With server-side parameters on the context, clickhouse-go
// v2.48.0 stops binding positional ? arguments client-side and sends them as
// parameters too, so every statement here that binds a filter breaks
// (measured: `SELECT {p:UInt64}, ?` fails with a syntax error at the ?). The
// parameter spelling is what makes a statement that somehow skipped liveConn
// fail loudly -- ClickHouse answers "Substitution `stale_after_ms` is not
// set" -- instead of silently reading with some other threshold.
const staleAfterToken = "{stale_after_ms:UInt64}"

// The two liveness conditions, each written once and used by every
// statement that asks either question: peer_state and peer_up resolve state
// with them, StalePairs selects with them, and TargetLiveness reuses the
// first. They read cur's epoch_ns and last_beat, which the live CTE fills
// for every collector in peer_current.
const (
	// epochLostExpr: this session is older than its collector's newest
	// process start. session_id is UnixNano on the collector's clock and the
	// process start is taken on the same clock before any session exists, so
	// every session of the newest process is at or above it and every session
	// of an earlier one is below it. Strictly below: a session minted in the
	// same nanosecond the process started is that process's own.
	epochLostExpr = "cur.sid < cur.epoch_ns"

	// beatStaleExpr: the collector's newest heartbeat reached the archive
	// longer ago than the threshold. inserted_at and now64() are both the
	// ClickHouse clock, so the collector's own clock plays no part.
	beatStaleExpr = "cur.last_beat < now64(3) - toIntervalMillisecond(" + staleAfterToken + ")"
)

// liveStateSQL resolves a peer's state against its collector's liveness. It
// assumes the enclosing SELECT aliases the stored argMax(kind) as
// stored_state and joins cur.
//
// Only a stored "up" ever changes. A peer the router reported down, or that a
// closing session already recorded as view_lost, keeps that state: the same
// rule Session.Close follows when it emits nothing for a peer already down.
// The epoch branch runs first, because a session whose collector restarted is
// certainly lost, while silence is only a suspicion.
//
// toString, because peer_events.kind is an Enum8 with no 'stale' value: the
// state this resolves to is a String, for every consumer.
const liveStateSQL = `multiIf(
            stored_state = 'up' AND ` + epochLostExpr + `, 'view_lost',
            stored_state = 'up' AND ` + beatStaleExpr + `, 'stale',
            toString(stored_state))`

// liveCTE is each collector's liveness, from collector_beats: its epoch (the
// newest process start any heartbeat announced) and its last beat (when the
// archive last received one, on the archive's clock).
//
// It carries a row for EVERY collector peer_current knows, beat or no beat:
// the second arm contributes an epoch of 1970 and a last beat of 1970 for
// each, and max() lets a real beat outrank it. That is what lets cur INNER
// JOIN it. A LEFT JOIN would hand an unbeaten collector 1970 anyway under
// this server's join_use_nulls = 0 and NULL under join_use_nulls = 1 -- where
// "NULL < threshold" is false and the peer would read up -- so the union
// makes "never heard from" read stale under either setting, through the one
// comparison beatStaleExpr already makes. beat_seen separates a real beat from
// the placeholder for the one caller that displays it (Collectors).
//
// max() rather than FINAL or argMax: collector_beats collapses on merge, and
// the newest started_at and the newest inserted_at are each a plain max
// whether or not the merge has happened.
const liveCTE = `
live AS (
    SELECT collector_id,
           max(started_at)  AS epoch_at,
           max(inserted_at) AS last_beat,
           max(beat_seen)   AS beat_seen
    FROM (
        SELECT collector_id, started_at, inserted_at, toUInt8(1) AS beat_seen
        FROM %[1]s.collector_beats
        UNION ALL
        SELECT DISTINCT collector_id,
               toDateTime64(0, 9, 'UTC') AS started_at,
               toDateTime64(0, 3, 'UTC') AS inserted_at,
               toUInt8(0)                AS beat_seen
        FROM %[1]s.peer_current
    )
    GROUP BY collector_id
)`

// newQ builds a Q whose statements are read with staleAfter as the stale
// threshold.
func newQ(raw driver.Conn, db string, staleAfter time.Duration) *Q {
	return &Q{
		conn:       liveConn{Conn: raw, literal: staleLiteral(staleAfter)},
		raw:        raw,
		db:         db,
		staleAfter: staleAfter,
	}
}

func staleLiteral(d time.Duration) string {
	return "toUInt64(" + strconv.FormatInt(d.Milliseconds(), 10) + ")"
}

// WithStaleAfter returns a Q over the same connection and database that reads
// a collector as stale after d without a heartbeat. q itself is unchanged.
//
// d below MinStaleAfter is refused: the collector beats every 30 s, and a
// threshold under two beats reads a healthy collector stale between them. d
// above MaxStaleAfter is refused too; see MaxStaleAfter for what it would
// break.
func (q *Q) WithStaleAfter(d time.Duration) (*Q, error) {
	if d < MinStaleAfter {
		return nil, fmt.Errorf("query: stale threshold %v is below %v, two heartbeat "+
			"intervals; a collector would read stale between beats", d, MinStaleAfter)
	}
	if d > MaxStaleAfter {
		return nil, fmt.Errorf("query: stale threshold %v is above %v; a collector "+
			"never heard from could read up", d, MaxStaleAfter)
	}
	return newQ(q.raw, q.db, d), nil
}

// StaleAfter is the threshold q reads with.
func (q *Q) StaleAfter() time.Duration { return q.staleAfter }

// liveConn fills the stale threshold into every statement a Q runs. See
// staleAfterToken for why it is text and not a bound parameter.
//
// It wraps the four calls that carry a statement; PrepareBatch, used only by
// tests to write fixtures, passes through untouched.
type liveConn struct {
	driver.Conn
	literal string
}

func (c liveConn) render(query string) string {
	return strings.ReplaceAll(query, staleAfterToken, c.literal)
}

func (c liveConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	return c.Conn.Query(ctx, c.render(query), args...)
}

func (c liveConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	return c.Conn.QueryRow(ctx, c.render(query), args...)
}

func (c liveConn) Select(ctx context.Context, dest any, query string, args ...any) error {
	return c.Conn.Select(ctx, dest, c.render(query), args...)
}

func (c liveConn) Exec(ctx context.Context, query string, args ...any) error {
	return c.Conn.Exec(ctx, c.render(query), args...)
}

// servedGate is the peer_up predicate every route-bearing statement filters
// on: a route is served while its peer is up, or up as of its collector's
// last heartbeat.
//
// Stale routes are served, not dropped, because staleness is a suspicion and
// a fleet-wide one is the common case: a NATS or writer stall longer than the
// threshold makes every collector stale at once, and dropping stale routes
// would blank every answer for the length of the incident -- absence reported
// as fact, exactly when an operator most needs the last good view. The API
// attaches a collector_stale warning to any answer that carries one (see
// StalePairs). An epoch-lost session is certain, so view_lost, derived or
// stored, stays excluded.
const servedGate = "peer_up.state IN ('up', 'stale')"

// StalePair is one (collector, router) whose current session reads stale
// and has at least one peer that reads stale with it -- a session that
// serves rows under that suspicion. See stalePairsSQL.
type StalePair struct {
	Collector string
	Router    netip.Addr
}

// StaleSet is every StalePair at the moment it was read.
type StaleSet map[StalePair]struct{}

// Has reports whether collector's current session with router reads stale. A
// nil set holds nothing.
func (s StaleSet) Has(collector string, router netip.Addr) bool {
	_, ok := s[StalePair{Collector: collector, Router: router.Unmap()}]
	return ok
}

// AnyRouter reports whether any collector's current session with router reads
// stale, or -- router invalid -- whether any session at all does. It is for
// an answer that merges collectors and cannot say which row came from whom.
func (s StaleSet) AnyRouter(router netip.Addr) bool {
	if !router.IsValid() {
		return len(s) > 0
	}
	router = router.Unmap()
	for p := range s {
		if p.Router == router {
			return true
		}
	}
	return false
}

// stalePairsSQL is every (collector, router) whose current session reads
// stale: not older than its collector's epoch -- an epoch-lost session reads
// view_lost, and its routes are not served at all -- and with the
// collector's newest heartbeat older than the threshold. A peer resolves
// stale exactly when its stored state is up and its pair is in this set:
// these are the same two expressions liveStateSQL applies, not a restatement
// of them.
//
// It is further limited to sessions with at least one peer whose stored
// state is up, which on such a session is exactly a peer that reads stale.
// A session whose every peer is stored down or view_lost serves no rows
// under the suspicion, so no answer can carry it, and left in the set it
// would flag every answer about its router (AnyRouter) for as long as the
// dead collector stays in the archive.
const stalePairsSQL = "WITH " + peerStateCTE + peerUpCTE + `
SELECT collector_id, router_ip
FROM cur
WHERE NOT (` + epochLostExpr + `)
  AND ` + beatStaleExpr + `
  AND (collector_id, router_ip, sid) IN (
      SELECT collector_id, router_ip, sid FROM peer_up WHERE stored_state = 'up')`

// AnyStale reports whether any row came from a stale (collector, router).
//
// It is keyed on each row's OWN collector and router, never on the router
// alone: a router two collectors watch, one of them dead, serves the live
// collector's rows unqualified, and only the dead one's carry the warning.
func AnyStale[T any](set StaleSet, rows []T, key func(T) (string, netip.Addr)) bool {
	for _, r := range rows {
		if set.Has(key(r)) {
			return true
		}
	}
	return false
}

// StaleKey is the row's (collector, router), the key AnyStale looks up.
func (x Route) StaleKey() (string, netip.Addr) { return x.Collector, x.RouterIP }

// StaleKey is the row's (collector, router), the key AnyStale looks up.
func (x VPNRoute) StaleKey() (string, netip.Addr) { return x.Collector, x.RouterIP }

// StaleKey is the row's (collector, router), the key AnyStale looks up.
func (x EVPNRoute) StaleKey() (string, netip.Addr) { return x.Collector, x.RouterIP }

// StaleKey is the row's (collector, router), the key AnyStale looks up.
func (x LSNode) StaleKey() (string, netip.Addr) { return x.Collector, x.RouterIP }

// StaleKey is the row's (collector, router), the key AnyStale looks up.
func (x LSLink) StaleKey() (string, netip.Addr) { return x.Collector, x.RouterIP }

// StaleKey is the row's (collector, router), the key AnyStale looks up.
func (x LSPrefix) StaleKey() (string, netip.Addr) { return x.Collector, x.RouterIP }

// StalePairs reads the StaleSet. The API asks it after it has an answer's
// rows, to decide whether that answer carries a collector_stale warning.
// Every row names its own collector and router, so the warning is keyed on
// both: a router that two collectors watch, one of them dead, serves the
// live collector's rows unqualified.
func (q *Q) StalePairs(ctx context.Context) (StaleSet, error) {
	rows, err := q.conn.Query(ctx, fmt.Sprintf(stalePairsSQL, q.db))
	if err != nil {
		return nil, fmt.Errorf("query stale pairs: %w", err)
	}
	defer rows.Close()
	set := StaleSet{}
	for rows.Next() {
		var p StalePair
		if err := rows.Scan(&p.Collector, &p.Router); err != nil {
			return nil, fmt.Errorf("scan stale pair: %w", err)
		}
		// router_ip is IPv6-typed and clickhouse-go's scan never unmaps an
		// IPv4-mapped address -- see unmapAll's own doc comment.
		unmapAll(&p.Router)
		set[p] = struct{}{}
	}
	return set, rows.Err()
}

// TargetLiveness is what `vantage purge` checks before it deletes anything.
type TargetLiveness struct {
	// HasBeat reports whether any heartbeat from the collector is on record.
	// LastBeat and Started are meaningful only when it is.
	HasBeat bool
	// LastBeat is when the archive last received the collector's heartbeat,
	// on the archive's clock.
	LastBeat time.Time
	// Started is the collector's newest process start, on its own clock.
	Started time.Time
	// BeatFresh reports whether LastBeat is inside the stale threshold.
	BeatFresh bool
	// LiveSessions counts the target's current sessions opened by the
	// collector's newest process -- at or after its epoch -- that still have
	// a peer stored up. A session whose peers are all down or view_lost is
	// one the router closed or the collector closed cleanly: nothing up is
	// left for the collector to maintain, so it does not count.
	LiveSessions uint64
}

// Live reports whether purging the target would delete state a running
// collector is still maintaining: it has been heard from inside the threshold
// AND it holds a current session its running process opened with a peer
// still stored up. A collector that is running but whose sessions for this
// router all predate its epoch -- the router moved to another pod -- is not
// live for that router; nor is one whose session with the router closed,
// every peer down or view_lost -- the router was disconnected, the routine
// way to retire it.
func (l TargetLiveness) Live() bool { return l.BeatFresh && l.LiveSessions > 0 }

// targetLivenessSQL answers TargetLiveness for one collector, and for one of
// its routers when %[2]s narrows it. Arguments: the collector four times,
// then the router if %[2]s names one.
//
// live_sessions keeps only sessions with a peer stored up, the same
// restriction stalePairsSQL applies and for the mirror reason: a session
// with nothing up serves no rows, and holds nothing a collector is
// maintaining.
const targetLivenessSQL = "WITH " + peerStateCTE + peerUpCTE + `
SELECT
    (SELECT max(beat_seen) FROM live WHERE collector_id = ?) AS has_beat,
    (SELECT max(last_beat) FROM live WHERE collector_id = ?) AS newest_beat,
    (SELECT max(epoch_at)  FROM live WHERE collector_id = ?) AS newest_start,
    (SELECT count() FROM cur WHERE collector_id = ? AND NOT (` + epochLostExpr + `)%[2]s
        AND (collector_id, router_ip, sid) IN (
            SELECT collector_id, router_ip, sid FROM peer_up WHERE stored_state = 'up')) AS live_sessions,
    newest_beat >= now64(3) - toIntervalMillisecond(` + staleAfterToken + `) AS beat_fresh`

// TargetLiveness reports how live collector -- or, router valid, its view of
// router -- is right now.
func (q *Q) TargetLiveness(ctx context.Context, collector string, router netip.Addr) (TargetLiveness, error) {
	narrow := ""
	args := []any{collector, collector, collector, collector}
	if router.IsValid() {
		narrow = " AND router_ip = toIPv6(?)"
		args = append(args, router.Unmap().String())
	}
	var (
		l              TargetLiveness
		hasBeat, fresh uint8
	)
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(targetLivenessSQL, q.db, narrow), args...).
		Scan(&hasBeat, &l.LastBeat, &l.Started, &l.LiveSessions, &fresh); err != nil {
		return TargetLiveness{}, fmt.Errorf("query target liveness: %w", err)
	}
	l.HasBeat, l.BeatFresh = hasBeat == 1, fresh == 1
	return l, nil
}
