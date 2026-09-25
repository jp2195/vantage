package query

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrRetentionNotInDays is RetentionDays' answer for a history TTL that is
// absent or not a whole number of days, such as one an operator set by hand
// as INTERVAL 3 MONTH. The retention is then unknown to this package, and a
// caller says nothing rather than a number it had to guess.
var ErrRetentionNotInDays = errors.New("query: history TTL is not a whole number of days")

// retentionTTLRe matches the history TTL as system.tables.engine_full spells
// it. ClickHouse normalizes the DDL's INTERVAL 90 DAY to toIntervalDay(90)
// there, so the DDL's own spelling never appears.
var retentionTTLRe = regexp.MustCompile(`\bTTL toDateTime\(ts_collector\) \+ toIntervalDay\((\d+)\)`)

// RetentionDays is the history retention the archive actually applies, in
// days, read from route_unicast's TTL in system.tables rather than from any
// configuration: the TTL is what deletes rows, so it is the only answer that
// cannot disagree with them. The deploy tooling sets the same TTL on all ten
// history tables. The current-state tables have no TTL and never expire.
func (q *Q) RetentionDays(ctx context.Context) (int, error) {
	var engine string
	if err := q.conn.QueryRow(ctx,
		"SELECT engine_full FROM system.tables WHERE database = ? AND name = 'route_unicast'",
		q.db,
	).Scan(&engine); err != nil {
		return 0, fmt.Errorf("query: read route_unicast's TTL: %w", err)
	}
	m := retentionTTLRe.FindStringSubmatch(engine)
	if m == nil {
		return 0, ErrRetentionNotInDays
	}
	days, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("query: parse route_unicast's TTL %q: %w", m[0], err)
	}
	return days, nil
}
