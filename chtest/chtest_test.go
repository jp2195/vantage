package chtest

import (
	"strings"
	"testing"
	"time"
)

func TestAddrDefaultsToDevStackPort(t *testing.T) {
	t.Setenv(addrEnv, "")
	if got := Addr(); got != "127.0.0.1:9000" {
		t.Errorf("Addr() = %q, want the dev stack's default 127.0.0.1:9000", got)
	}
}

func TestAddrHonorsOverride(t *testing.T) {
	t.Setenv(addrEnv, "10.0.0.5:19000")
	if got := Addr(); got != "10.0.0.5:19000" {
		t.Errorf("Addr() = %q, want the %s override 10.0.0.5:19000", got, addrEnv)
	}
}

func TestDSNNamesTheVantageUserAndGivenDatabase(t *testing.T) {
	t.Setenv(addrEnv, "10.0.0.5:19000")
	const want = "clickhouse://vantage:vantage@10.0.0.5:19000/vantage_query_test"
	if got := DSN("vantage_query_test"); got != want {
		t.Errorf("DSN(%q) = %q, want %q", "vantage_query_test", got, want)
	}
}

func TestSchemaStatementsRewritesDatabaseAndDropsComments(t *testing.T) {
	const ddl = `-- a comment
CREATE DATABASE IF NOT EXISTS vantage;
CREATE TABLE vantage.route_unicast (prefix String) ENGINE = Memory;`

	got := SchemaStatements(ddl, "vantage_query_test")
	if len(got) != 2 {
		t.Fatalf("SchemaStatements returned %d statements, want 2: %q", len(got), got)
	}
	for _, stmt := range got {
		if strings.Contains(stmt, "vantage.") || strings.Contains(stmt, "EXISTS vantage;") {
			t.Errorf("statement still names the production database: %q", stmt)
		}
		if strings.Contains(stmt, "--") {
			t.Errorf("comment survived into statement: %q", stmt)
		}
	}
}

// TestRefusesADatabaseThatIsNotATestDatabase is the guard that was missing on
// 2026-09-11, when Require(t, ctx, "vantage") dropped and recreated the
// shared development archive from schema.sql -- 8,611 unicast routes,
// 6,907 VPN routes, 904 EVPN routes and every peer event behind them --
// because this
// package's contract is "drop and recreate" and nothing checked what it had
// been pointed at.
func TestRefusesADatabaseThatIsNotATestDatabase(t *testing.T) {
	for _, db := range []string{"vantage", "default", "system", "vantage_test_archive", ""} {
		t.Run(db, func(t *testing.T) {
			err := checkTestDB(db)
			if err == nil {
				t.Fatalf("checkTestDB(%q) = nil; this package drops what it is "+
					"given, and %q is not a test database", db, db)
			}
			if !strings.Contains(err.Error(), db) {
				t.Errorf("the refusal does not name the database: %v", err)
			}
		})
	}

	// Every database this repo's own tests really use must still pass, or the
	// guard is a build break rather than a safety net. The list is explicit
	// rather than derived because deriving it from the callers is what let it
	// be written as four when it is five.
	for _, db := range []string{
		"vantage_test",           // sink
		"vantage_query_test",     // query
		"vantage_api_test",       // api
		"vantage_api_cmd_test",   // cmd/vantage-api
		"vantage_cli_query_test", // cmd/vantage
	} {
		if err := checkTestDB(db); err != nil {
			t.Errorf("checkTestDB(%q) = %v, want nil -- this is a real caller's "+
				"database", db, err)
		}
	}
}

// TestEnsureDatabaseRefusesBeforeItConnects proves the refusal happens in Go,
// before any DSN is parsed or any connection is opened, so it holds on a
// machine with no ClickHouse running as well as on one with a live archive to
// lose. A guard that only fired after a successful connection would pass CI
// and fail in the one place it matters.
//
// It asks twice because ensureDatabase caches per database name in a
// sync.Once and the second call takes a different path through it than the
// first. Both refusals are the same one; what the repeat rules out is a guard
// that fires once and then hands back a cached success.
func TestEnsureDatabaseRefusesBeforeItConnects(t *testing.T) {
	// NOT "vantage", even though that is the name the incident used. If this
	// guard ever regresses, this test is the thing that runs next -- and a
	// test that drops the live archive to prove the archive is protected is
	// the incident with a green tick on it. The probe name lacks the suffix,
	// which is all the guard looks at, and names no database that exists.
	const probe = "chtest_guard_probe_not_a_database"
	err := ensureDatabase(t.Context(), probe)
	if err == nil {
		t.Fatalf("ensureDatabase(%q) = nil, want a refusal", probe)
	}
	if !strings.Contains(err.Error(), "refusing to use database") {
		t.Errorf("ensureDatabase returned %v, want the checkTestDB refusal", err)
	}
	if err := ensureDatabase(t.Context(), probe); err == nil {
		t.Errorf("the second ensureDatabase(%q) = nil; the refusal must not be "+
			"cached away", probe)
	}
}

// TestEveryCollectorATestDatabaseHearsOfBeatsForever is the rule that keeps
// the whole suite from reading "stale": a peer event under an ordinary
// collector id makes that collector immortal -- started at the epoch, heard
// from in 2200 -- while one under LivenessPrefix gets nothing, because a test
// using that prefix is about liveness and writes its own beats.
//
// Both insert paths are covered: a peer_events row (which reaches
// peer_current through the schema's own view) and a direct peer_current row.
// The view hangs off peer_current so that neither can be missed.
func TestEveryCollectorATestDatabaseHearsOfBeatsForever(t *testing.T) {
	ctx := t.Context()
	const db = "vantage_chtest_liveness_test"
	conn := Require(t, ctx, db)
	now := time.Now().UTC()
	insert := func(table, collector string) {
		t.Helper()
		if err := conn.Exec(ctx, "INSERT INTO "+db+"."+table+
			" (collector_id, router_ip, peer_ip, rib, session_id, seq, ts_router, ts_collector, stream_seq, kind)"+
			" VALUES (?, toIPv6('10.0.0.1'), toIPv6('10.0.0.2'), 'in_pre', 1, 1, ?, ?, 1, 'up')",
			collector, now, now); err != nil {
			t.Fatalf("insert %s row for %s: %v", table, collector, err)
		}
	}
	insert("peer_events", "chtest-ordinary")
	insert("peer_current", "chtest-direct")
	insert("peer_events", LivenessPrefix+"chtest")

	beat := func(collector string) (n uint64, started, inserted time.Time) {
		t.Helper()
		if err := conn.QueryRow(ctx, "SELECT count(), min(started_at), max(inserted_at) FROM "+db+
			".collector_beats WHERE collector_id = ?", collector).Scan(&n, &started, &inserted); err != nil {
			t.Fatalf("read beats for %s: %v", collector, err)
		}
		return n, started, inserted
	}
	for _, c := range []string{"chtest-ordinary", "chtest-direct"} {
		n, started, inserted := beat(c)
		if n == 0 {
			t.Errorf("%s has no beat: every existing test's collector would read stale", c)
			continue
		}
		if !started.Equal(time.Unix(0, 0)) {
			t.Errorf("%s started_at = %v, want the epoch (below every session id)", c, started)
		}
		if inserted.Year() != 2200 {
			t.Errorf("%s inserted_at = %v, want 2200 (never older than any threshold)", c, inserted)
		}
	}
	if n, _, _ := beat(LivenessPrefix + "chtest"); n != 0 {
		t.Errorf("%schtest got %d immortal beats; a liveness test's collector must get none",
			LivenessPrefix, n)
	}
}
