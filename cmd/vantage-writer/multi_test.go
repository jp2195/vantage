package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/sink"
)

// The two archives this file's test compares: one written by a single
// writer, one by two writers sharing the same durable consumers.
const (
	oneWriterDB  = "vantage_writer_multi_one_test"
	twoWritersDB = "vantage_writer_multi_two_test"
)

// multiEnvelopes is how many archive envelopes (ROUTES, LS, PEER and STATS
// subjects) the replay publishes.
const multiEnvelopes = 20000

// archiveTables is every history and current table in schema.sql. The test
// also asserts the database holds no other data table, so a table added to
// the schema later fails here until it is listed.
var archiveTables = []string{
	// History.
	"route_unicast", "route_vpn", "route_evpn",
	"ls_events", "ls_nodes", "ls_links", "ls_prefixes",
	"peer_events", "eor_events", "stats_events",
	// Current.
	"peer_current", "eor_current",
	"route_unicast_current", "route_vpn_current", "route_evpn_current",
	"ls_nodes_current", "ls_links_current", "ls_prefixes_current",
}

// writerDurables are the durable consumer names consumerConfigs gives each
// stream.
func writerDurables() map[string]string {
	out := map[string]string{}
	for _, cc := range consumerConfigs(sink.Config{}) {
		out[cc.Stream] = cc.Durable
	}
	return out
}

// TestTwoWritersArchiveTheSameAsOne publishes one replay of the committed
// BMP corpus to JetStream and archives it twice: once with one writer, once
// with two writers pulling from the same durable consumers, so JetStream
// splits every stream's messages between them. Both archives must hold
// exactly the same rows in every history and current table.
//
// Both runs read the same stream. The first writer's consumers are deleted
// once it has drained, and the two writers recreate them under the same
// durable names, so they start again from the first message. stream_seq is
// part of every row and of most sort keys; reading one stream twice is what
// makes it identical in both archives, where two NATS servers would need
// their publish order to match message for message. ts_collector is stamped
// by the collector's clock when the envelope is built, not at publish or at
// insert, and the envelopes are published once, so it is identical too.
// Nothing is excluded from the comparison.
//
// The superseded-session cleanup is off in both runs
// (current_cleanup_interval: 0s): it deletes from the current tables on a
// timer, so whether a cycle ran before the comparison would differ by run
// and have nothing to do with how many writers there were.
func TestTwoWritersArchiveTheSameAsOne(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, oneWriterDB)
	chtest.Require(t, ctx, twoWritersDB)

	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncTimeout(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := natsutil.EnsureStreams(ctx, js, natsutil.StreamOpts{
		Replicas: 1, LSReplicas: 1, RoutesMaxBytes: 256 << 20, RawMaxBytes: 16 << 20,
	}); err != nil {
		t.Fatal(err)
	}

	publishCorpus(t, js, multiEnvelopes)

	runWriters(t, ns, js, oneWriterDB, 1)
	for stream, durable := range writerDurables() {
		if err := js.DeleteConsumer(ctx, stream, durable); err != nil {
			t.Fatalf("delete consumer %s/%s: %v", stream, durable, err)
		}
	}
	runWriters(t, ns, js, twoWritersDB, 2)

	compareArchives(t, ctx, conn, oneWriterDB, twoWritersDB)
}

// publishCorpus replays every committed .bmpcap through collector.Session
// and publishes the events through natsutil.Publisher, the collector's own
// publish path, until want archive envelopes have been published. The
// corpus alone is smaller than want, so it is replayed repeatedly: pass r
// is session r+1 of the same router per capture file, the way a router that
// reconnects opens a new session. Each capture has its own router address,
// so two captures' sessions never share a router, and every event's MsgID
// (router, peer, session, seq) is distinct, so JetStream's duplicate window
// drops none of them. RAW events are not published: the writer never reads
// the RAW stream.
func publishCorpus(t *testing.T, js jetstream.JetStream, want int) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "bgp", "testdata", "corpus", "*", "*.bmpcap"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no corpus captures: %v", err)
	}
	if len(paths) > 250 {
		t.Fatalf("%d captures: the router address scheme below has room for 250", len(paths))
	}
	captures := make([][]byte, len(paths))
	for i, p := range paths {
		if captures[i], err = os.ReadFile(p); err != nil {
			t.Fatal(err)
		}
	}

	// One clock for every envelope: ts_collector is what the tables
	// partition and expire on, so it has to be recent, and it has to be
	// the same for any two envelopes that would otherwise be identical.
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }

	pub := natsutil.NewPublisher(js)
	kinds := map[string]int{}
	published := 0
	for pass := 0; published < want; pass++ {
		before := published
		for i, raw := range captures {
			router := netip.AddrFrom4([4]byte{10, 201, byte(i + 1), 1})
			s := collector.NewSession(router, "multi-writer", uint64(pass+1), clock, collector.Overrides{})
			r := bytes.NewReader(raw)
			for published < want {
				m, err := bmp.ReadMsg(r)
				if err != nil {
					break // end of the capture
				}
				for _, ev := range s.Handle(m) {
					kind := strings.Split(ev.Subject, ".")[2]
					if kind == "raw" || published == want {
						continue
					}
					if ev.Env.GetRoute().GetEndOfRib() || ev.Env.GetLs().GetEndOfRib() {
						kinds["eor"]++
					}
					kinds[kind]++
					if err := pub.Publish(ev); err != nil {
						t.Fatal(err)
					}
					published++
				}
			}
		}
		if published == before {
			t.Fatal("a pass over the corpus published nothing")
		}
	}
	if err := pub.Drain(60 * time.Second); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for _, k := range []string{"route", "ls", "peer", "stats", "eor"} {
		if kinds[k] == 0 {
			t.Fatalf("the replay published no %s envelopes: %v", k, kinds)
		}
	}
	t.Logf("published %d envelopes: %v", published, kinds)
}

// runWriters runs n writers against db until every stream's durable
// consumer has delivered and had acked every message in its stream, then
// stops them. The small batches make each writer insert many times, so the
// two writers' batches interleave rather than one taking a whole stream in a
// single fetch.
func runWriters(t *testing.T, ns *server.Server, js jetstream.JetStream, db string, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, n)
	for i := range n {
		path := writeWriterConfig(t, ns.ClientURL(), chtest.DSN(db)+"?log_comment="+writerTag(db, i))
		appendConfig(t, path, fmt.Sprintf("metrics_listen: %q\nlog_level: warn\n"+
			"batch_rows: 500\nbatch_wait: 100ms\nfetch_batch: 100\n"+
			"current_cleanup_interval: 0s\n", freeAddr(t)))
		go func() { done <- run(ctx, path) }()
	}

	drained := waitDrained(t, js, 90*time.Second)
	cancel()
	for range n {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("writer against %s: %v", db, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("a writer against %s did not stop", db)
		}
	}
	if !drained {
		t.Fatalf("writers against %s did not drain the streams", db)
	}
}

// writerTag names writer i against db in ClickHouse's query log. The
// process start time keeps an earlier run's log entries from counting.
func writerTag(db string, i int) string { return fmt.Sprintf("%s_%d_w%d", db, runNonce, i) }

var runNonce = time.Now().UnixNano()

// waitDrained reports whether every archive stream's durable consumer has
// delivered its whole stream and has nothing awaiting an ack. The writer
// acks only after the insert returns, so at that point every message is in
// ClickHouse.
func waitDrained(t *testing.T, js jetstream.JetStream, timeout time.Duration) bool {
	t.Helper()
	ctx := t.Context()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		all := true
		for stream, durable := range writerDurables() {
			s, err := js.Stream(ctx, stream)
			if err != nil {
				t.Fatal(err)
			}
			si, err := s.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c, err := js.Consumer(ctx, stream, durable)
			if err != nil {
				all = false // not created yet
				break
			}
			ci, err := c.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if ci.NumPending != 0 || ci.NumAckPending != 0 ||
				ci.Delivered.Stream != si.State.LastSeq || ci.AckFloor.Stream != si.State.LastSeq {
				all = false
				break
			}
		}
		if all {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// compareArchives asserts db1 and db2 hold the same rows in every table of
// archiveTables, and that none is empty. Each table is read FINAL, as every
// query of these tables reads it, ordered by its own sort key and then by
// the whole row, including MATERIALIZED columns. eor_events is a plain MergeTree, which has no FINAL: it is
// compared as it is stored, duplicates included, and its sort key does not
// cover every column, so the whole-row tiebreak is what makes its order
// stable. Before comparing, it asserts that both writers of the two-writer
// run inserted rows, so the comparison is not of one writer against itself.
func compareArchives(t *testing.T, ctx context.Context, conn driver.Conn, db1, db2 string) {
	t.Helper()

	// collector_beats is left out on purpose. The corpus replay publishes no
	// heartbeat, so every row it holds here was written by chtest's test-only
	// view, one per peer_current insert: it records nothing either writer
	// archived.
	var tables []string
	rows, err := conn.Query(ctx, `SELECT name FROM system.tables
		WHERE database = ? AND engine != 'MaterializedView'
		  AND name NOT IN ('schema_version', 'cleanup_lease', 'collector_beats') ORDER BY name`, db1)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if want := slices.Sorted(slices.Values(archiveTables)); !slices.Equal(tables, want) {
		t.Fatalf("%s's data tables are %v, the test compares %v", db1, tables, want)
	}

	assertBothWritersInserted(t, ctx, conn, db2)

	for _, table := range archiveTables {
		var engine, key string
		if err := conn.QueryRow(ctx, `SELECT engine, sorting_key FROM system.tables
			WHERE database = ? AND name = ?`, db1, table).Scan(&engine, &key); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		final := ""
		if strings.HasPrefix(engine, "Replacing") {
			final = " FINAL"
		}
		cols := rowColumns(t, ctx, conn, db1, table)
		read := func(db string) []string {
			q := fmt.Sprintf("SELECT formatRow('TSV', %s) AS r FROM %s.%s%s ORDER BY %s, r",
				cols, db, table, final, key)
			rows, err := conn.Query(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var r string
				if err := rows.Scan(&r); err != nil {
					t.Fatal(err)
				}
				out = append(out, r)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return out
		}
		one, two := read(db1), read(db2)
		if len(one) == 0 {
			t.Errorf("%s: empty after the replay", table)
			continue
		}
		if len(one) != len(two) {
			t.Errorf("%s: one writer archived %d rows, two writers %d", table, len(one), len(two))
		}
		for i := range min(len(one), len(two)) {
			if one[i] != two[i] {
				t.Errorf("%s: row %d differs\n one writer:  %s two writers: %s", table, i, one[i], two[i])
				break
			}
		}
		t.Logf("%s: %d rows", table, len(one))
	}
}

// rowColumns is the column list compareArchives formats each row from:
// every stored column, MATERIALIZED ones included. A Map column is read
// through mapSort. The writer builds each Map from a Go map, whose iteration
// order is random, so the same map lands with its entries in a different
// order on every insert -- two runs of one writer differ that way too. Sorted,
// the entries are compared exactly.
func rowColumns(t *testing.T, ctx context.Context, conn driver.Conn, db, table string) string {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT name, type FROM system.columns
		WHERE database = ? AND table = ? AND default_kind != 'ALIAS' ORDER BY position`, db, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		col := "`" + name + "`"
		if strings.HasPrefix(typ, "Map(") {
			col = "mapSort(" + col + ")"
		}
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		t.Fatalf("%s.%s has no columns", db, table)
	}
	return strings.Join(cols, ", ")
}

// assertBothWritersInserted reads ClickHouse's query log for the inserts
// each two-writer-run writer made, told apart by the log_comment its DSN
// set.
func assertBothWritersInserted(t *testing.T, ctx context.Context, conn driver.Conn, db string) {
	t.Helper()
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		var inserts, written uint64
		if err := conn.QueryRow(ctx, `SELECT count(), sum(written_rows) FROM system.query_log
			WHERE type = 'QueryFinish' AND query_kind = 'Insert'
			  AND log_comment = ?`,
			writerTag(db, i)).Scan(&inserts, &written); err != nil {
			t.Fatal(err)
		}
		if inserts == 0 {
			t.Fatalf("writer %d of the two-writer run made no inserts: JetStream gave it nothing, "+
				"so the run did not test two writers", i)
		}
		t.Logf("two-writer run, writer %d: %d inserts, %d rows written", i, inserts, written)
	}
}
