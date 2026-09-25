// Package chtest is the live-ClickHouse test harness shared by every
// package whose tests need a database built from the shipped DDL --
// currently sink and, from here on, query. It used to live entirely inside
// sink/clickhouse_test.go; it moved here because a _test.go file cannot be
// imported by another package, and query's tests need exactly what sink's
// already built rather than a second, drifting copy of it.
package chtest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// addrEnv lets a developer point every package's live-ClickHouse tests at a
// server other than the default -- see Addr's doc comment for why that has
// to be possible at all.
const addrEnv = "VANTAGE_TEST_CLICKHOUSE_ADDR"

// Addr is the host:port every package's live-ClickHouse tests dial: the
// VANTAGE_TEST_CLICKHOUSE_ADDR override if set, otherwise
// docker-compose.dev.yml's published native port.
//
// It is a function rather than a package-level constant because 9000 is a
// popular port: any other compose stack on the machine that publishes it
// makes docker-compose.dev.yml's clickhouse unstartable and every package
// depending on this one unrunnable, with no way to say "the server I mean
// is over there". addrEnv is that escape hatch, and docker-compose.dev.yml
// takes the matching VANTAGE_CH_NATIVE_PORT override so the two can be
// pointed at each other. Only the address is overridable this way: the
// credentials in DSN below are this repo's, and a DSN-shaped override
// would let a run silently aim its DROP DATABASE at a server that is not
// the dev stack.
func Addr() string {
	return cmp.Or(os.Getenv(addrEnv), "127.0.0.1:9000")
}

// DSN builds a ClickHouse DSN for db against Addr(), using the vantage
// user's credentials from docker-compose.dev.yml (README Quickstart) and
// ci.yml's service container -- both start the same image on the same
// port, so this DSN reaches a real ClickHouse in either place.
func DSN(db string) string {
	return "clickhouse://vantage:vantage@" + Addr() + "/" + db
}

// SchemaStatements turns schema.sql's text into executable statements aimed
// at db: comments stripped, every bare "vantage" identifier rewritten to
// db, then split on the statement separator. clickhouse-go's Exec takes one
// statement at a time, which is why this splits rather than handing over
// the file whole the way docker-compose.dev.yml's initdb.d mount and
// ci.yml's `clickhouse-client --multiquery` both do.
//
// db is a parameter rather than a package constant because the whole point
// of this package is that sink and query each get their own test database
// -- a shared constant here would put both packages' fixtures right back
// in the same tables, which is the exact problem testDB was invented to
// avoid in the first place (see sink/clickhouse_test.go's testDB doc
// comment). The rewrite is a plain text substitution and assumes the DDL
// never spells "vantage" inside a string literal (it does not: the only
// literals are the rib and kind enum values).
func SchemaStatements(ddl, db string) []string {
	var b strings.Builder
	for line := range strings.SplitSeq(ddl, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	renamed := regexp.MustCompile(`\bvantage\b`).ReplaceAllString(b.String(), db)
	var out []string
	for stmt := range strings.SplitSeq(renamed, ";") {
		if strings.TrimSpace(stmt) != "" {
			out = append(out, stmt)
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// schemaPath resolves deploy/clickhouse/schema.sql from this file's own
// location rather than the caller's working directory. go test runs each
// package in its own directory, so a relative "../deploy/..." resolves
// differently for sink/ than for query/ -- and silently, into a "file not
// found" that reads like a broken checkout rather than a broken helper.
func schemaPath() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("chtest: cannot resolve own source path")
	}
	return filepath.Join(filepath.Dir(self), "..", "deploy", "clickhouse", "schema.sql"), nil
}

// dbSetup is one database's setup state: run at most once, by whichever
// caller reaches Do first, with the result cached for every caller after.
type dbSetup struct {
	once sync.Once
	err  error
}

// setups is keyed by database name rather than being a single package-level
// sync.Once, because that is exactly what DSN's db parameter above exists
// to allow: sink and query each drive their own database through this same
// package, in the same process should a test binary ever import both (it
// does not today, but nothing here should assume it never will). A single
// unkeyed sync.Once would silently skip setting up the second database
// ever requested in such a binary and hand back success for a database
// nothing had touched.
var (
	setupsMu sync.Mutex
	setups   = map[string]*dbSetup{}
)

// testDBSuffix is the suffix every database this package is allowed to
// touch must carry, and checkTestDB is what makes it a rule rather than a
// convention.
//
// This package DROPS the database it is handed, and it reaches ClickHouse
// through the same DSN, on the same server, as the shared development
// archive -- so the only thing standing between a mistyped constant and
// the loss of every route in `vantage` is the argument at the call site.
// On 2026-09-11
// that turned out to be nothing at all: a scratch test called
// Require(t, ctx, "vantage") to read the live archive, and this package
// dropped and recreated it from schema.sql. ClickHouse's own eight-minute
// UNDROP window was the only reason the loss was even visible before the
// data was gone.
//
// The rule is a suffix rather than a deny list naming "vantage", because a
// deny list protects the one name someone thought of and a suffix protects
// every name nobody did -- including the production archive of a deployment
// this repo has never seen. Every one of this repo's test databases
// carries it -- vantage_test and the three vantage_sink_current_*_test
// databases (sink), vantage_query_test (query), vantage_api_test (api),
// vantage_api_cmd_test (cmd/vantage-api) and vantage_cli_query_test
// (cmd/vantage) -- so it costs nothing and refuses exactly the call that
// was made.
const testDBSuffix = "_test"

// LivenessPrefix marks a test collector_id as one whose liveness the test
// controls. See immortalBeatsSQL.
const LivenessPrefix = "liveness-"

// immortalBeatsSQL makes every collector a test database hears of alive,
// forever, unless its id starts with LivenessPrefix. %[1]s is the database.
//
// Current-state queries read a collector with no fresh heartbeat as stale,
// and a session older than its collector's newest process start as lost.
// Almost no test is about either: they write peer events under collector ids
// like "c1" or "parity-c1" and assert "up". Without this, every one of them
// would read "stale" the moment it ran. So every peer_current row -- whether
// it arrived through peer_events' own view or was written directly -- also
// writes a beat for its collector that started at the Unix epoch (below every
// session_id) and was received in 2200 (after every now()): never stale, and
// never an epoch newer than a session.
//
// A test that is ABOUT liveness names its collectors with LivenessPrefix and
// writes its own collector_beats rows; this view skips them. It exists only
// in test databases -- setupDatabase, behind checkTestDB, is the one place
// that creates it -- and never in schema.sql.
const immortalBeatsSQL = `CREATE MATERIALIZED VIEW IF NOT EXISTS %[1]s.test_immortal_beats_mv
TO %[1]s.collector_beats
AS SELECT
    collector_id,
    toDateTime64(0, 9, 'UTC')                     AS started_at,
    toDateTime64(ts_collector, 3, 'UTC')          AS beat_at,
    toDateTime64('2200-01-01 00:00:00', 3, 'UTC') AS inserted_at
FROM %[1]s.peer_current
WHERE NOT startsWith(collector_id, '` + LivenessPrefix + `')`

// checkTestDB refuses a database this package must not drop.
//
// It returns an error rather than panicking so Require reports it through
// Skip's usual path -- and it is deliberately NOT skippable by an
// environment variable: an escape hatch here would be an escape hatch from
// the only thing preventing a DROP DATABASE on a live archive, and the
// correct way to read one is a plain clickhouse-go connection, which needs
// nothing from this package.
func checkTestDB(db string) error {
	if !strings.HasSuffix(db, testDBSuffix) {
		return fmt.Errorf("chtest: refusing to use database %q: this package "+
			"DROPS and recreates the database it is given, so it will only "+
			"touch one whose name ends in %q. %q looks like a real archive -- "+
			"to READ one, open your own clickhouse-go connection instead",
			db, testDBSuffix, db)
	}
	return nil
}

// ensureDatabase drops and recreates db from deploy/clickhouse/schema.sql,
// exactly once per db name per test binary. Concurrent `go test` runs
// against the same server would fight over the same database; that is the
// same constraint the dev stack already has (one ClickHouse, one dev
// database) and is left as is rather than papered over with a per-run
// random name, which would leave a trail of abandoned databases behind
// instead.
//
// It refuses outright any database whose name is not a test database's --
// see checkTestDB, and the incident in its doc comment.
func ensureDatabase(ctx context.Context, db string) error {
	if err := checkTestDB(db); err != nil {
		return err
	}
	setupsMu.Lock()
	s, ok := setups[db]
	if !ok {
		s = &dbSetup{}
		setups[db] = s
	}
	setupsMu.Unlock()

	// sync.Once.Do's own synchronization -- not setupsMu, which only
	// protects the map above -- is what makes s.err below safe to read
	// without a lock: Do does not return in any caller until the one call
	// to the function has, so every caller observes the write it made.
	s.once.Do(func() { s.err = setupDatabase(ctx, db) })
	return s.err
}

// setupDatabase drops and recreates db, then applies the shipped DDL to it.
//
// It applies deploy/clickhouse/schema.sql itself rather than a copy, so a
// caller's tests run against the real deployed shape -- a sort key or
// column type that exists only in a test fixture would prove nothing about
// what a writer inserts into in production.
func setupDatabase(ctx context.Context, db string) error {
	// Checked again here, not only in ensureDatabase: this is the function
	// that issues the DROP, and a future caller reaching it by another path
	// must not be able to skip the one guard that matters.
	if err := checkTestDB(db); err != nil {
		return err
	}
	opts, err := clickhouse.ParseDSN(DSN("default"))
	if err != nil {
		return fmt.Errorf("chtest: parse bootstrap dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return fmt.Errorf("chtest: open bootstrap conn: %w", err)
	}
	defer conn.Close()
	if err := conn.Ping(ctx); err != nil {
		return fmt.Errorf("chtest: ping: %w", err)
	}
	// Dropped, not just created-if-missing: a run must start from an empty
	// database, or a fixture row from a previous run under a previous
	// schema silently becomes input to this one.
	if err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+db); err != nil {
		return fmt.Errorf("chtest: drop %s: %w", db, err)
	}
	path, err := schemaPath()
	if err != nil {
		return err
	}
	ddl, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("chtest: read schema.sql: %w", err)
	}
	for _, stmt := range SchemaStatements(string(ddl), db) {
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("chtest: apply %q: %w", firstLine(stmt), err)
		}
	}
	if err := conn.Exec(ctx, fmt.Sprintf(immortalBeatsSQL, db)); err != nil {
		return fmt.Errorf("chtest: create %s.test_immortal_beats_mv: %w", db, err)
	}
	return nil
}

// requireEnv gates what happens when a live-ClickHouse test can't get the
// connection it needs. Unset (the common case for a developer who has not
// started the dev stack), Skip ends the test with a message telling them
// how to start it -- `go test ./...` must stay usable without ClickHouse
// running locally. Set to a non-empty value, the same condition is a hard
// failure instead. ci.yml sets it, because a passing CI run must never be
// a CI run that silently skipped the live ClickHouse suites -- sink's
// Append column-drift guardrail chief among them.
const requireEnv = "VANTAGE_REQUIRE_CLICKHOUSE"

// Skip ends the calling test via t.Skip when err is non-nil and requireEnv
// is unset, or via t.Fatal when requireEnv is set (see its doc comment
// above); it does nothing when err is nil.
//
// It is exported, not folded into Require below, because Require's own
// connection is not always the only one a caller needs gated the same way.
// sink's requireClickHouse, for one, calls Require to prove testDB is
// ready and then dials a second, independent connection of its own type
// via NewClickHouse -- production code's own constructor, exercising its
// DSN validation, ping and checkSchema, not just reachability. That second
// dial can fail on its own (a transient reconnect race between the two
// otherwise-unrelated connections, or a genuine schema-version mismatch),
// and a developer with no dev stack running must see that failure skip
// too, not just Require's. Passing it through Skip is what makes that
// true without sink re-implementing (and risking drifting from) this
// env-var gate itself.
func Skip(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := fmt.Sprintf("chtest: %v (start the dev database with "+
		"`docker compose -f docker-compose.dev.yml up -d clickhouse`, "+
		"or check deploy/clickhouse/schema.sql is applied)", err)
	if os.Getenv(requireEnv) != "" {
		t.Fatalf("%s -- %s is set, so an unreachable ClickHouse is a "+
			"hard failure here, not a skip", msg, requireEnv)
	}
	t.Skip(msg)
}

// Require ensures db exists with the shipped schema applied (see
// ensureDatabase for the once-per-db-per-binary guarantee), dials it, and
// returns a live connection with its Close registered via t.Cleanup. When
// ClickHouse cannot be reached, or the schema cannot be applied, it ends
// the test via Skip above.
func Require(t *testing.T, ctx context.Context, db string) driver.Conn {
	t.Helper()
	conn, err := requireConn(ctx, db)
	Skip(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func requireConn(ctx context.Context, db string) (driver.Conn, error) {
	if err := ensureDatabase(ctx, db); err != nil {
		return nil, err
	}
	opts, err := clickhouse.ParseDSN(DSN(db))
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open conn: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		// clickhouse.Open already started a background pool-drain goroutine
		// (holding a time.Ticker) even though the connection never proved
		// live: only Close stops it. Every transient Ping failure without
		// this leaks one goroutine and one ticker permanently -- see
		// sink's TestNewClickHouseClosesConnOnPingFailure, which this
		// mirrors.
		conn.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return conn, nil
}
