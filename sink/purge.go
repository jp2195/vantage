package sink

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// PurgeTarget names what `vantage purge` removes: one collector's view of one
// router, or -- Router invalid -- everything one collector holds.
type PurgeTarget struct {
	Collector string
	Router    netip.Addr
}

// cleanupKillMarker is the text deleteSync's KILL MUTATION matches a
// mutation's command on (cleanupKillFilter). ClickHouse records a
// lightweight DELETE's command with its literal predicate values in it, so a
// purge whose collector_id contained this text would carry the marker and
// could be killed by a cleanup that failed at the same moment.
const cleanupKillMarker = "floor_sid"

// Validate refuses a target the purge cannot remove safely.
func (t PurgeTarget) Validate() error {
	if t.Collector == "" {
		return errors.New("purge: a collector is required")
	}
	if strings.Contains(t.Collector, cleanupKillMarker) {
		return fmt.Errorf("purge: collector id %q contains %q, the text the writer's "+
			"cleanup matches when it kills its own failed mutations, so a purge of it "+
			"could be killed mid-flight; remove it by hand with the SQL in "+
			"docs/operating.md", t.Collector, cleanupKillMarker)
	}
	return nil
}

// Tables is the tables a purge of t deletes from, in the order it deletes:
// the eight current tables in cleanup's own order, peer_current last, then --
// for a whole-collector purge only -- collector_beats.
//
// peer_current is last among the eight because cleanup and every read find a
// router only through it. A purge interrupted before it leaves the router
// visible, and running it again finishes the job. collector_beats comes
// after it for the same reason one level up: a collector whose peer rows are
// gone but whose beats remain is listed nowhere, and a re-run removes the
// beats.
func (t PurgeTarget) Tables() []string {
	out := slices.Clone(cleanupTables)
	if !t.Router.IsValid() {
		out = append(out, "collector_beats")
	}
	return out
}

// where is the predicate selecting t's rows in table, and its arguments.
// collector_beats has no router_ip, and is in Tables only when t names no
// router.
func (t PurgeTarget) where(table string) (string, []any) {
	w, args := "collector_id = ?", []any{t.Collector}
	if t.Router.IsValid() && table != "collector_beats" {
		w += " AND router_ip = toIPv6(?)"
		args = append(args, t.Router.Unmap().String())
	}
	return w, args
}

// deleteWhere is where for a DELETE: where, bounded to sessions no newer
// than maxSession on every table that has a session_id. A router that
// reconnects while the purge runs opens a session with a higher id, and its
// rows are the new session's current state, not the retired one's.
// collector_beats has no session_id and is bounded by nothing.
func (t PurgeTarget) deleteWhere(table string, maxSession uint64) (string, []any) {
	w, args := t.where(table)
	if table != "collector_beats" {
		w += " AND session_id <= ?"
		args = append(args, maxSession)
	}
	return w, args
}

// purgeDeleteSQL is one purge DELETE, waiting for its mutation. It shares
// nothing with cleanup's statements, and must not: see cleanupKillMarker.
func purgeDeleteSQL(db, table, where string) string {
	return "DELETE FROM " + db + "." + table + " WHERE " + where + " SETTINGS mutations_sync = 2"
}

// PurgeCount is how many rows a purge would delete from one table.
type PurgeCount struct {
	Table string
	Rows  uint64
}

// PurgePlan is a purge of Target as PlanPurge read it.
type PurgePlan struct {
	Target PurgeTarget
	// Counts is the rows per table, in the order Purge deletes.
	Counts []PurgeCount
	// MaxSession is the newest session_id among the counted rows. Purge
	// deletes no row of a newer session.
	MaxSession uint64
}

// Rows is the total of Counts.
func (p PurgePlan) Rows() uint64 {
	var n uint64
	for _, c := range p.Counts {
		n += c.Rows
	}
	return n
}

// PlanPurge reads what a purge of t would delete, per table, in the order
// Purge deletes, and the newest session those rows belong to. It deletes
// nothing.
func (c *ClickHouse) PlanPurge(ctx context.Context, t PurgeTarget) (PurgePlan, error) {
	if err := t.Validate(); err != nil {
		return PurgePlan{}, err
	}
	p := PurgePlan{Target: t}
	for _, table := range t.Tables() {
		w, args := t.where(table)
		sel := "count(), max(session_id)"
		if table == "collector_beats" {
			sel = "count(), toUInt64(0)"
		}
		var n, maxSID uint64
		if err := c.conn.QueryRow(ctx, "SELECT "+sel+" FROM "+c.db+"."+table+" WHERE "+w, args...).
			Scan(&n, &maxSID); err != nil {
			return PurgePlan{}, fmt.Errorf("purge: count %s: %w", table, err)
		}
		p.Counts = append(p.Counts, PurgeCount{Table: table, Rows: n})
		p.MaxSession = max(p.MaxSession, maxSID)
	}
	return p, nil
}

// Purge deletes p's rows from every table in Tables, in that order, each
// DELETE waiting for its mutation and bounded to sessions no newer than
// p.MaxSession. It stops at the first failure. The tables after it,
// peer_current among them, are untouched, so the target stays visible, and
// planning and running Purge again finishes it. History is not touched: it
// ages out with retention like everything else there.
func (c *ClickHouse) Purge(ctx context.Context, p PurgePlan) error {
	if err := p.Target.Validate(); err != nil {
		return err
	}
	for _, table := range p.Target.Tables() {
		w, args := p.Target.deleteWhere(table, p.MaxSession)
		if err := c.conn.Exec(ctx, purgeDeleteSQL(c.db, table, w), args...); err != nil {
			return fmt.Errorf("purge: delete from %s: %w", table, err)
		}
	}
	return nil
}
