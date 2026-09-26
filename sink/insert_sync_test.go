package sink

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/secret"
)

// syncInsertTestDB is this file's own database: the beats it inserts would
// otherwise count as a collector in testDB's liveness and dashboard tests.
const syncInsertTestDB = "vantage_sink_sync_insert_test"

// TestInsertIsSynchronousUnlessTheDSNSaysOtherwise reads back from
// system.query_log whether ClickHouse ran Insert's statement as an
// asynchronous insert. ClickHouse 26.2 and later do so by default, so a
// connection that does not set async_insert itself buffers every INSERT on
// the server. The second case is the other side: a DSN that asks for
// asynchronous inserts gets them, so the default does not override an
// operator's explicit choice.
func TestInsertIsSynchronousUnlessTheDSNSaysOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dsnSuffix string
		wantAsync uint64
	}{
		{"default DSN", "", 0},
		{"DSN with async_insert=1", "?async_insert=1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn := chtest.Require(t, ctx, syncInsertTestDB)
			c, err := NewClickHouse(ctx, secret.NewClickHouseDSN(chtest.DSN(syncInsertTestDB)+tc.dsnSuffix))
			if err != nil {
				t.Fatalf("NewClickHouse: %v", err)
			}
			defer c.Close()

			queryID := fmt.Sprintf("sink-insert-sync-%d", time.Now().UnixNano())
			now := time.Now().UTC()
			err = c.Insert(clickhouse.Context(ctx, clickhouse.WithQueryID(queryID)), Rows{
				Beats: []BeatRow{{CollectorID: "sync-insert-test", StartedAt: now, BeatAt: now}},
			})
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
				t.Fatalf("SYSTEM FLUSH LOGS: %v", err)
			}
			var n, async uint64
			if err := conn.QueryRow(ctx, `SELECT count(), max(ProfileEvents['AsyncInsertQuery'])
				FROM system.query_log WHERE query_id = ? AND type = 'QueryFinish'`, queryID).
				Scan(&n, &async); err != nil {
				t.Fatalf("read system.query_log: %v", err)
			}
			if n != 1 {
				t.Fatalf("system.query_log has %d finished queries with id %s, want the one insert", n, queryID)
			}
			if async != tc.wantAsync {
				t.Errorf("AsyncInsertQuery = %d, want %d", async, tc.wantAsync)
			}
		})
	}
}
