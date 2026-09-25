package sink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jp2195/vantage/chtest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/secret"
)

// The writer must refuse to start against a schema it does not expect, rather
// than inserting into the wrong columns quietly. If this constant changes,
// deploy/clickhouse/schema.sql must have changed with it.
func TestExpectedSchemaVersion(t *testing.T) {
	// Version 1 is the squashed public baseline; version 2 adds
	// collector_beats (migrations/001). The next shape change is version 3
	// and brings its own migration;
	// TestSchemaAndItsHighestMigrationDeclareTheSameVersion enforces that.
	if ExpectedSchemaVersion != 2 {
		t.Fatalf("ExpectedSchemaVersion = %d; if the schema changed, update "+
			"schema.sql's own version insert to match and add a migration",
			ExpectedSchemaVersion)
	}

	// schema.sql's header states the version in prose for whoever reads the
	// file, and prose is the part nobody re-reads when bumping the INSERT: it
	// once said 3 while the INSERT said 4. Pin the sentence to the row.
	ddl := filepath.Join("..", "deploy", "clickhouse", "schema.sql")
	body, err := os.ReadFile(ddl)
	if err != nil {
		t.Fatalf("read %s: %v", ddl, err)
	}
	stated := regexp.MustCompile(`(?m)^-- The version is (\d+)\.`).FindAllStringSubmatch(string(body), -1)
	if len(stated) != 1 {
		t.Fatalf("%s: want exactly one header line `-- The version is <n>.`, found %d; "+
			"the header states the version so a reader need not find the INSERT, "+
			"and this test keeps the two equal", ddl, len(stated))
	}
	inserted := regexp.MustCompile(`SELECT (\d+) WHERE`).FindStringSubmatch(string(body))
	if inserted == nil {
		t.Fatalf("%s carries no `SELECT <version> WHERE` row", ddl)
	}
	if stated[0][1] != inserted[1] {
		t.Errorf("%s: the header says the version is %s but the INSERT establishes %s; "+
			"update the header when the version moves", ddl, stated[0][1], inserted[1])
	}
	if stated[0][1] != strconv.Itoa(ExpectedSchemaVersion) {
		t.Errorf("%s: the header says the version is %s but ExpectedSchemaVersion is %d",
			ddl, stated[0][1], ExpectedSchemaVersion)
	}
}

// schema.sql and the highest-numbered migration must declare the SAME
// version, and it must be ExpectedSchemaVersion. The three are three
// independent statements of one number, and nothing tied them before this:
// TestExpectedSchemaVersion pins the constant and schema.sql's header,
// TestNewClickHouseSchemaCheck proves the shipped schema.sql reports the
// constant to a live server, and neither looks at what a MIGRATION's own
// version row says. At the version-1 baseline there is no migration, and
// the test then requires the constant to be 1.
//
// The gap is not academic; it is what the Helm chart's schema Job now
// verifies at deploy time. That Job derives the version it expects to reach
// from its copy of schema.sql (regexFind over the INSERT, see
// job-schema.yaml) and fails if the database does not report it after every
// file is applied. A migration that bumped the database to 3 while
// schema.sql still declared 2 would make every operator-managed upgrade fail
// that check -- correctly, but at deploy time on a real cluster, which is a
// much more expensive place to find it than here.
func TestSchemaAndItsHighestMigrationDeclareTheSameVersion(t *testing.T) {
	declared := func(path string) int {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		m := regexp.MustCompile(`SELECT (\d+) WHERE`).FindStringSubmatch(string(body))
		if m == nil {
			t.Fatalf("%s carries no `SELECT <version> WHERE` row: every file that "+
				"establishes or advances the schema states the version it leaves "+
				"behind, and this test cannot check a file that does not", path)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s declares a non-numeric version %q", path, m[1])
		}
		return n
	}

	ddl := filepath.Join("..", "deploy", "clickhouse", "schema.sql")
	migDir := filepath.Join("..", "deploy", "clickhouse", "migrations")
	// The directory itself must exist: a glob over a moved or deleted
	// directory matches nothing, which would read as "no migrations" and
	// pass the baseline branch below without looking at anything.
	if fi, err := os.Stat(migDir); err != nil || !fi.IsDir() {
		t.Fatalf("%s is not a directory (%v): the migration mechanism outlives "+
			"the squash, and this test cannot check a directory that is gone", migDir, err)
	}
	migs, err := filepath.Glob(filepath.Join(migDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}

	schemaVer := declared(ddl)
	if schemaVer != ExpectedSchemaVersion {
		t.Errorf("schema.sql establishes version %d but ExpectedSchemaVersion is %d: "+
			"a fresh install would be refused by the binary that created it",
			schemaVer, ExpectedSchemaVersion)
	}

	// No migrations is the baseline, and only the baseline: a version beyond
	// 1 with nothing in migrations/ is a shape change an existing database
	// has no way to reach.
	if len(migs) == 0 {
		if ExpectedSchemaVersion != 1 {
			t.Errorf("ExpectedSchemaVersion is %d but %s holds no migrations: an "+
				"existing database at version 1 has no way to reach it",
				ExpectedSchemaVersion, migDir)
		}
		return
	}
	sort.Strings(migs)
	highest := migs[len(migs)-1]

	migVer := declared(highest)
	if migVer != ExpectedSchemaVersion {
		t.Errorf("%s advances an existing database to version %d but "+
			"ExpectedSchemaVersion is %d: a MIGRATED database and a FRESH one would "+
			"report different versions, which is exactly what schema_version exists "+
			"to prevent", highest, migVer, ExpectedSchemaVersion)
	}
	if schemaVer != migVer {
		t.Errorf("schema.sql declares %d and %s declares %d. The Helm schema Job "+
			"derives the version it expects from schema.sql and checks the database "+
			"against it after applying every migration, so this disagreement would "+
			"fail every operator-managed upgrade at deploy time",
			schemaVer, highest, migVer)
	}
}

// The kind enum in schema.sql must carry every kind sink/rows.go can produce.
// ClickHouse rejects an insert of an Enum8 element the column does not
// declare, and the PEER consumer retries an insert forever, so a kind peerRow
// can emit but the column cannot hold wedges that consumer permanently --
// which is exactly how the first real Peer Down wedged it (see
// TestRowsForPeerDownNormalizesEmptyLocalIP). This asserts the mapping from
// the schema side; peerRow's switch is the other half.
func TestSchemaPeerKindEnumCoversEveryKindRowsForEmits(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	for _, kind := range []vantagev1.PeerEvent_Kind{
		vantagev1.PeerEvent_KIND_UNSPECIFIED,
		vantagev1.PeerEvent_KIND_UP,
		vantagev1.PeerEvent_KIND_DOWN,
		vantagev1.PeerEvent_KIND_VIEW_LOST,
	} {
		rows := mustRowsFor(t, testEnv(&vantagev1.PeerEvent{Kind: kind}), 1)
		if len(rows.Peer) != 1 {
			t.Fatalf("kind %v produced %d peer rows", kind, len(rows.Peer))
		}
		if want := "'" + rows.Peer[0].Kind + "'"; !strings.Contains(string(ddl), want) {
			t.Errorf("peer kind %v maps to %q, which schema.sql's kind Enum8 "+
				"does not declare: ClickHouse would reject the insert and the "+
				"PEER consumer would retry it forever", kind, rows.Peer[0].Kind)
		}
	}
}

// The version row in deploy/clickhouse/schema.sql must be inserted only into
// a database that has none. schema.sql is all CREATE TABLE IF NOT EXISTS and
// ClickHouse will not rewrite an existing table's ORDER BY, so applying it to
// a database built by an older version leaves that version's tables in place.
// An unconditional INSERT would then raise max(version) to the current one
// over the old sort keys, and checkSchema above -- the only thing standing
// between a shape change and a silently mis-shaped archive -- would report
// success. This test exists because the guard is one line of SQL that reads
// like boilerplate and would be an easy "simplification".
func TestSchemaVersionInsertIsConditional(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	re := regexp.MustCompile(`(?is)INSERT INTO vantage\.schema_version[^;]*;`)
	m := re.FindAllString(string(ddl), -1)
	if len(m) != 1 {
		t.Fatalf("schema.sql: want exactly 1 INSERT INTO vantage.schema_version, got %d: %q", len(m), m)
	}
	stmt := m[0]
	if !strings.Contains(strings.ToUpper(stmt), "WHERE") {
		t.Fatalf("schema.sql: the schema_version INSERT is unconditional:\n\t%s\n"+
			"It must not insert into a database that already carries a version "+
			"row, or applying this file to an older database raises the version "+
			"without migrating the tables and checkSchema passes over the wrong "+
			"shape.", stmt)
	}
	if !strings.Contains(stmt, fmt.Sprintf("%d", ExpectedSchemaVersion)) {
		t.Fatalf("schema.sql: the schema_version INSERT does not mention %d, "+
			"which is this binary's ExpectedSchemaVersion:\n\t%s",
			ExpectedSchemaVersion, stmt)
	}
}

// fakeSchemaVersionConn is a driver.Conn test fake that answers checkSchema's
// "SELECT max(version) FROM vantage.schema_version" query with a canned
// value, touching no database. It embeds driver.Conn rather than
// implementing it -- per that interface's own doc comment -- so it stays a
// valid driver.Conn if the driver adds methods in a future minor release;
// every method but QueryRow panics via the nil embedded value if called,
// which is fine because checkSchema calls only QueryRow.
type fakeSchemaVersionConn struct {
	driver.Conn
	row fakeSchemaVersionRow
}

func (f fakeSchemaVersionConn) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	return f.row
}

// fakeSchemaVersionRow is a canned driver.Row. version is what Scan writes
// into checkSchema's `var got uint32`; err, if set, is what Scan returns
// instead (modeling a query failure, e.g. schema_version does not exist).
type fakeSchemaVersionRow struct {
	version uint32
	err     error
}

func (r fakeSchemaVersionRow) Err() error { return r.err }

func (r fakeSchemaVersionRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	got, ok := dest[0].(*uint32)
	if !ok {
		return fmt.Errorf("fakeSchemaVersionRow: unsupported dest %T", dest[0])
	}
	*got = r.version
	return nil
}

func (r fakeSchemaVersionRow) ScanStruct(dest any) error {
	return errors.New("fakeSchemaVersionRow: ScanStruct not implemented")
}

// TestCheckSchemaRejectsMismatch is the one test the fail-fast-on-
// schema-drift design actually needs: it proves checkSchema refuses a
// wrong version rather than merely proving NewClickHouse can dial a
// database that already happens to be at the right one. Exercising the
// mismatch through the real dev ClickHouse would mean either standing up a
// second instance or mutating the shared dev database's schema_version out
// from under every other test and the collector using it -- both worse than
// a small in-package fake of the two-method slice of driver.Conn checkSchema
// actually touches.
func TestCheckSchemaRejectsMismatch(t *testing.T) {
	c := &ClickHouse{conn: fakeSchemaVersionConn{
		row: fakeSchemaVersionRow{version: ExpectedSchemaVersion + 1},
	}}
	err := c.checkSchema(context.Background())
	if err == nil {
		t.Fatal("checkSchema: want error for schema_version mismatch, got nil")
	}
}

// TestCheckSchemaAcceptsMatch is TestCheckSchemaRejectsMismatch's control:
// without it, a checkSchema that always errors would also make the mismatch
// test above pass.
func TestCheckSchemaAcceptsMatch(t *testing.T) {
	c := &ClickHouse{conn: fakeSchemaVersionConn{
		row: fakeSchemaVersionRow{version: ExpectedSchemaVersion},
	}}
	if err := c.checkSchema(context.Background()); err != nil {
		t.Fatalf("checkSchema: unexpected error for matching version: %v", err)
	}
}

// TestCheckSchemaPropagatesQueryError covers checkSchema's other error
// path: schema_version unreadable at all (e.g. deploy/clickhouse/schema.sql
// was never applied), not just readable-but-wrong.
func TestCheckSchemaPropagatesQueryError(t *testing.T) {
	c := &ClickHouse{conn: fakeSchemaVersionConn{
		row: fakeSchemaVersionRow{err: errors.New("no such table: vantage.schema_version")},
	}}
	if err := c.checkSchema(context.Background()); err == nil {
		t.Fatal("checkSchema: want error when schema_version is unreadable, got nil")
	}
}

// TestNewClickHouseClosesConnOnPingFailure guards against the leak fixed in
// NewClickHouse: clickhouse.Open starts a background pool-drain goroutine
// (holding a time.Ticker) before any I/O happens, released only by Close.
// A failed Ping used to return without calling it. This needs no real
// ClickHouse -- a bare TCP listener that accepts connections but never
// speaks the native protocol handshake is enough to make Open succeed (it
// dials nothing) and Ping fail (its handshake read blocks until ctx's
// deadline, then errors).
func TestNewClickHouseClosesConnOnPingFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without ever writing the
			// handshake, so the client's Ping blocks until ctx expires
			// rather than failing instantly (which could pass this test
			// without ever exercising the leak).
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			c.Close()
		}
	}()

	before := runtime.NumGoroutine()

	// dial_timeout is set explicitly and short: the driver's handshake sets
	// its own connection deadline from Options.DialTimeout (default 30s),
	// not from ctx, so leaving it at the default would make this test wait
	// out a real 30-second timeout instead of ctx's -- confirmed by running
	// this test without the parameter, which failed on testing's own -timeout
	// rather than on a driver error.
	dsn := fmt.Sprintf("clickhouse://%s/default?dial_timeout=300ms", ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewClickHouse(ctx, secret.NewClickHouseDSN(dsn)); err == nil {
		t.Fatal("NewClickHouse: want an error dialing a listener that never " +
			"speaks the ClickHouse protocol, got nil")
	}

	// runtime.NumGoroutine() is inherently a little noisy (background
	// runtime goroutines come and go), so retry briefly rather than
	// asserting on the first sample immediately after NewClickHouse
	// returns -- the drain goroutine's teardown is not synchronous with
	// Close() returning.
	deadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		if after := runtime.NumGoroutine(); after <= before {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("goroutine count grew from %d to %d after a failed Ping -- "+
				"NewClickHouse must Close() the connection clickhouse.Open already "+
				"started a pool-drain goroutine for", before, after)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// testDB is this package's own ClickHouse database, and it is deliberately
// NOT the dev stack's "vantage".
//
// Every live test in this package inserts rows: hand-built fixtures here,
// and the whole vendor corpus in corpus_integration_test.go. Pointed at
// "vantage", those rows landed in the same tables the Grafana dashboards
// read and could not be told apart from a router's -- the route-churn
// dashboard's entire withdrawal series, and its top "most-changed prefix",
// were fixtures. Worse, replayCorpus replays router 10.0.103.62, which the
// dev collector also monitors, with a stream_seq counter starting at 1 in
// the same space as the real ROUTES stream: a test row and a production row
// coinciding on the sort key destroy each other under ReplacingMergeTree.
//
// requireClickHouse below, via chtest.Require, drops and recreates testDB
// from deploy/clickhouse/schema.sql once per test binary, so these tests
// own their data outright, start from a known-empty state, and cannot touch
// the archive. chtest also carries query's identical database of its own --
// see chtest/chtest.go's package comment -- which is why testDB stays a
// name chosen by this package rather than something chtest hands back.
const testDB = "vantage_test"

// qualify rewrites the "vantage." table qualifier the queries in this
// package's tests are written with into the database the client under test
// is actually pointed at. The queries stay readable next to
// deploy/clickhouse/schema.sql, which is where the names come from, while
// the rows they read live in testDB.
func qualify(c *ClickHouse, query string) string {
	return strings.ReplaceAll(query, "vantage.", c.db+".")
}

// requireClickHouse ensures testDB exists with the shipped schema applied
// and returns a live client -- or ends the test via chtest.Skip when
// ClickHouse is unreachable (see its doc comment for the
// VANTAGE_REQUIRE_CLICKHOUSE gate: skip for a developer without the dev
// stack running, hard-fail under CI).
//
// It builds on chtest.Require rather than dialing testDB directly: Require
// is what actually owns "is this database set up and reachable", since
// query's tests need to ask the identical question of their own database.
// The driver.Conn Require hands back exists only to prove that -- it is
// closed via t.Cleanup without this function ever touching it. What this
// returns is a second, independent connection of sink's own type, dialed
// the same way production code dials one, via NewClickHouse -- and routed
// through chtest.Skip in turn, rather than a bare t.Fatalf, because that
// second dial can itself fail the same ways the first one can (a transient
// reconnect race between the two connections, or testDB's schema having
// drifted from ExpectedSchemaVersion): a developer with no dev stack
// running must see this failure skip too, not just chtest.Require's.
func requireClickHouse(t *testing.T, ctx context.Context) *ClickHouse {
	t.Helper()
	return requireClickHouseDB(t, ctx, testDB)
}

// requireClickHouseDB is requireClickHouse for a database other than testDB,
// for a test that must own every row in its tables: one that forces merges
// with OPTIMIZE FINAL, or compares two databases fed the same events.
func requireClickHouseDB(t *testing.T, ctx context.Context, db string) *ClickHouse {
	t.Helper()
	chtest.Require(t, ctx, db)
	c, err := NewClickHouse(ctx, secret.NewClickHouseDSN(chtest.DSN(db)))
	if err != nil {
		err = fmt.Errorf("NewClickHouse: %w", err)
	}
	chtest.Skip(t, err)
	return c
}

//go:fix inline
func u32(v uint32) *uint32 { return new(v) }

// A DSN with no database in it is rejected at construction, and the
// rejection says so without echoing the DSN.
//
// NewClickHouse qualifies every statement with the database the DSN names,
// so there has to be one. clickhouse-go leaves Auth.Database empty for a
// path-less DSN and lets the server fall back to "default", which is never
// where deploy/clickhouse/schema.sql has been applied -- so defaulting here
// would turn a startup-time configuration mistake into a missing-table
// error much later, on whichever table happened to receive the first row.
//
// No server is needed: the check runs before clickhouse.Open, which is also
// why it cannot leak a connection.
func TestNewClickHouseRejectsADSNWithNoDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const password = "hunter2"
	_, err := NewClickHouse(ctx, secret.NewClickHouseDSN(
		"clickhouse://vantage:"+password+"@127.0.0.1:9000"))
	if err == nil {
		t.Fatal("NewClickHouse with no database in the DSN returned no error")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("error %q does not mention the database, so it does not tell "+
			"an operator what to fix", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error %q contains the DSN password", err)
	}
}

// TestNewClickHouseSchemaCheck connects to a real ClickHouse (see
// docker-compose.dev.yml) carrying deploy/clickhouse/schema.sql as applied
// by chtest.Require, and asserts NewClickHouse accepts it: the shipped
// DDL really does report ExpectedSchemaVersion, not merely that our own
// checkSchema logic is internally consistent. It is also what catches the
// schema.sql INSERT and ExpectedSchemaVersion being bumped apart from each
// other, which is the whole reason the fail-fast check is trustworthy.
func TestNewClickHouseSchemaCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()
}

// TestInsertRoundTrip is the test the positional-argument constraint
// exists for: it inserts one row into every table against
// the real dev ClickHouse and reads each one back by its distinguishing
// columns. A mock could not catch an Append argument landing in the wrong
// column -- the driver would happily accept it -- so this asserts on values
// ClickHouse itself stored and returned, which is the only place a
// column-order mistake would actually show up (e.g. a family string landing
// in the prefix column, or MED landing in local_pref).
func TestInsertRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	// sessionID is the marker every SELECT below filters on: unique per test
	// run, so this test is safe to run repeatedly against a shared dev
	// database without colliding with rows from a previous run.
	sessionID := uint64(time.Now().UnixNano())
	ts := time.Now().UTC().Truncate(time.Microsecond)
	env := func() envelope {
		return envelope{
			CollectorID: "chtest", RouterIP: "10.0.0.1", RouterSysname: "sysA",
			PeerIP: "10.0.0.2", RIB: "in_pre", PeerASN: 65001,
			PeerBGPID: "10.0.0.3", SessionID: sessionID, Seq: 42,
			TsRouter: ts, TsCollector: ts,
			ParseFlags: []string{"trunc_attr"}, StreamSeq: 7,
		}
	}

	rows := Rows{
		Unicast: []UnicastRow{{
			envelope: env(),
			// Origin 2 (INCOMPLETE), not 0: origin is now the column
			// immediately after is_withdraw -- end_of_rib used to sit
			// between them -- so these are the adjacent same-typed
			// (UInt8) pair, and they carry different values on purpose.
			// A swap between them would insert cleanly, both being valid
			// UInt8s, and only a read-back assertion on both can catch it.
			Origin: 2, ASPath: []uint32{65001, 65002}, NextHop: "10.0.0.9",
			MED: new(uint32(100)), LocalPref: new(uint32(200)),
			Communities: []uint32{4260495361}, ExtCommunities: []string{"rt:65001:100"},
			RouteTargets: []string{"65001:100"}, LargeCommunities: []string{"65001:1:2"},
			Family: "ipv4u", Prefix: "203.0.113.0/24", PathID: 1,
			IsWithdraw: 1,
		}},
		// eor_events is the table the End-of-RIB marker moved into. A
		// distinct family from the unicast row above ("evpn", not "ipv4u")
		// is what proves the marker carries its own family rather than
		// inheriting the route table's -- the property that makes dump
		// progress answerable for a peer that only ever carried EVPN.
		Eor: []EorRow{{envelope: env(), Family: "evpn"}},
		Vpn: []VpnRow{{
			envelope: env(),
			Origin:   0, NextHop: "10.0.0.10",
			Communities: []uint32{1}, ExtCommunities: []string{"rt:65001:200"},
			RouteTargets: []string{"65001:200"},
			Family:       "vpn4", Prefix: "198.51.100.0/24", PathID: 2,
			RD: "65001:1", Labels: []uint32{100},
		}},
		Evpn: []EvpnRow{{
			envelope: env(),
			Origin:   0, NextHop: "10.0.0.11",
			RouteType: 2, RD: "65001:2", Prefix: "2001:db8:1::/64",
			MAC: "00:11:22:33:44:55",
			IP:  "192.0.2.5", GatewayIP: "192.0.2.1", EthernetTag: 100,
			ESI: "00:00:00:00:00:00:00:00:00", Labels: []uint32{200}, PathID: 3,
		}},
		Ls: []LsRow{{
			envelope: env(), Family: "ls",
			RawReach: []byte{1, 2, 3, 4}, RawUnreach: []byte{},
			// EndOfRIB set to 1 here purely to catch a column
			// shift, the same way UnicastRow's IsWithdraw/EndOfRIB pair
			// above does -- this row is not a real End-of-RIB (it also
			// carries RawReach), but the column round-trip is what this
			// test checks, not real-world co-occurrence.
			EndOfRIB: 1,
		}},
		LsNodes: []LsNodeRow{{
			envelope: env(), Protocol: 3, Identifier: 100,
			ASN: 65001, BgplsID: 1, Area: 0,
			RouterID: "0aff0001",
			// RouterIDv4 is adjacent to RouterID in both the
			// DDL and the Append call -- a distinct, unmistakable value here
			// (not just "a valid-looking IP") is what would catch an
			// off-by-one landing it in is_withdraw or leaving it in
			// router_id instead.
			RouterIDv4: "10.255.0.1",
			IsWithdraw: 0, Name: "xr-rr1",
			SrgbBase: 16000, SrgbSize: 8000, SrlbBase: 15000, SrlbSize: 1000,
			SrAlgorithms: []uint8{0, 1},
			UnknownTLVs:  map[uint16]string{999: "x"},
		}},
		LsLinks: []LsLinkRow{{
			envelope: env(), Protocol: 3, Identifier: 100,
			LocalASN: 65001, LocalBgplsID: 1, LocalArea: 0, LocalRouterID: "0aff0002",
			RemoteASN: 65001, RemoteBgplsID: 1, RemoteArea: 0, RemoteRouterID: "0aff0005",
			LocalIfAddr: "10.1.0.2", RemoteIfAddr: "10.1.0.3",
			LinkLocalID: 4, LinkRemoteID: 3,
			IsWithdraw:    0,
			AdjSIDs:       []uint32{24001},
			AdjSIDFlags:   []uint8{0x60},
			AdjSIDWeights: []uint8{0},
			TEMetric:      1, IGPMetric: 1, AdminGroup: 0, MaxBandwidth: 1.25e8,
			UnknownTLVs: map[uint16]string{999: "y"},
		}},
		Peer: []PeerRow{{
			envelope: env(), Kind: "up", LocalIP: "10.0.0.20",
			LocalPort: 179, RemotePort: 54321, DownReason: 3, CapFourByteAS: 1,
		}},
		Stats: []StatsRow{{
			envelope: env(), Counters: map[uint32]uint64{1: 100, 2: 200},
		}},
	}

	if err := c.Insert(ctx, rows); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	t.Run("route_unicast", func(t *testing.T) {
		var family, prefix, nextHop string
		var med, localPref *uint32
		var routeTargets []string
		var isWithdraw, origin uint8
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT family, prefix, next_hop, med, local_pref, route_targets,
			        is_withdraw, origin
			 FROM vantage.route_unicast WHERE session_id = ?`), sessionID)
		if err := row.Scan(&family, &prefix, &nextHop, &med, &localPref, &routeTargets,
			&isWithdraw, &origin); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if family != "ipv4u" || prefix != "203.0.113.0/24" || nextHop != "10.0.0.9" {
			t.Errorf("family=%q prefix=%q next_hop=%q", family, prefix, nextHop)
		}
		if med == nil || *med != 100 || localPref == nil || *localPref != 200 {
			t.Errorf("med=%v local_pref=%v, want 100/200 (an off-by-one Append "+
				"would swap these)", med, localPref)
		}
		if len(routeTargets) != 1 || routeTargets[0] != "65001:100" {
			t.Errorf("route_targets=%v", routeTargets)
		}
		// is_withdraw and origin are adjacent UInt8 columns set to different
		// values (1, 2) specifically so a swap between them shows up here
		// rather than passing silently as two valid-looking small integers.
		if isWithdraw != 1 || origin != 2 {
			t.Errorf("is_withdraw=%d origin=%d, want 1/2", isWithdraw, origin)
		}
	})

	// eor_events carries the envelope columns plus family and nothing else,
	// so family is the only column an off-by-one Append could land wrong --
	// and it lands wrong in a way that reads as a plausible value, since
	// every envelope column before it is also a string or a number. Reading
	// it back next to router_sysname, the last envelope column before it, is
	// what makes a shift visible.
	t.Run("eor_events", func(t *testing.T) {
		var family, sysname string
		var seq uint64
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT family, router_sysname, seq FROM vantage.eor_events
			 WHERE session_id = ?`), sessionID)
		if err := row.Scan(&family, &sysname, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if family != "evpn" || sysname != "sysA" || seq != 42 {
			t.Errorf("family=%q router_sysname=%q seq=%d, want evpn/sysA/42",
				family, sysname, seq)
		}
	})

	t.Run("route_vpn", func(t *testing.T) {
		var rd, prefix string
		var labels []uint32
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT rd, prefix, labels FROM vantage.route_vpn WHERE session_id = ?`),
			sessionID)
		if err := row.Scan(&rd, &prefix, &labels); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if rd != "65001:1" || prefix != "198.51.100.0/24" || len(labels) != 1 || labels[0] != 100 {
			t.Errorf("rd=%q prefix=%q labels=%v", rd, prefix, labels)
		}
	})

	t.Run("route_evpn", func(t *testing.T) {
		var rd, prefix, mac, ip, gatewayIP string
		var routeType uint8
		var ethernetTag uint32
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT route_type, rd, prefix, mac, ip, gateway_ip, ethernet_tag
			 FROM vantage.route_evpn WHERE session_id = ?`), sessionID)
		if err := row.Scan(&routeType, &rd, &prefix, &mac, &ip, &gatewayIP, &ethernetTag); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// route_type, rd, and prefix are the DDL's first three
		// evpn-specific columns, each a different type (UInt8/String/
		// String) but rd and prefix are both plain strings and adjacent --
		// asserting all three together catches a shift between them, not
		// just a type-level accident.
		if routeType != 2 || rd != "65001:2" || prefix != "2001:db8:1::/64" {
			t.Errorf("route_type=%d rd=%q prefix=%q, want 2/\"65001:2\"/\"2001:db8:1::/64\"",
				routeType, rd, prefix)
		}
		if mac != "00:11:22:33:44:55" || ip != "192.0.2.5" || gatewayIP != "192.0.2.1" || ethernetTag != 100 {
			t.Errorf("mac=%q ip=%q gateway_ip=%q ethernet_tag=%d", mac, ip, gatewayIP, ethernetTag)
		}
	})

	t.Run("ls_events", func(t *testing.T) {
		var family string
		var rawReach, rawUnreach []byte
		var endOfRib uint8
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT family, raw_reach, raw_unreach, end_of_rib FROM vantage.ls_events
			 WHERE session_id = ?`), sessionID)
		if err := row.Scan(&family, &rawReach, &rawUnreach, &endOfRib); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if family != "ls" || string(rawReach) != "\x01\x02\x03\x04" || len(rawUnreach) != 0 {
			t.Errorf("family=%q raw_reach=%v raw_unreach=%v", family, rawReach, rawUnreach)
		}
		// end_of_rib: the newest column, appended last in
		// insertLs's Append call -- exactly the position an argument-count
		// mismatch is easiest to introduce and easiest to miss.
		if endOfRib != 1 {
			t.Errorf("end_of_rib=%d, want 1", endOfRib)
		}
	})

	t.Run("ls_nodes", func(t *testing.T) {
		var routerID, routerIDv4, name string
		var srgbBase, srgbSize uint32
		var unknownTLVs map[uint16]string
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT router_id, router_id_v4, name, srgb_base, srgb_size, unknown_tlvs
			 FROM vantage.ls_nodes WHERE session_id = ?`), sessionID)
		if err := row.Scan(&routerID, &routerIDv4, &name, &srgbBase, &srgbSize, &unknownTLVs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// router_id and router_id_v4 are adjacent String
		// columns in the DDL -- distinct values on each is what would catch
		// a shift between them.
		if routerID != "0aff0001" || routerIDv4 != "10.255.0.1" {
			t.Errorf("router_id=%q router_id_v4=%q, want 0aff0001/10.255.0.1", routerID, routerIDv4)
		}
		if name != "xr-rr1" || srgbBase != 16000 || srgbSize != 8000 {
			t.Errorf("name=%q srgb_base=%d srgb_size=%d", name, srgbBase, srgbSize)
		}
		if unknownTLVs[999] != "x" {
			t.Errorf("unknown_tlvs=%v, want {999: x}", unknownTLVs)
		}
	})

	t.Run("ls_links", func(t *testing.T) {
		var localRouterID, remoteRouterID, localIfaddr, remoteIfaddr string
		var linkLocalID, linkRemoteID uint32
		var adjSIDs []uint32
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT local_router_id, remote_router_id, local_ifaddr, remote_ifaddr,
			        link_local_id, link_remote_id, adj_sids
			 FROM vantage.ls_links WHERE session_id = ?`), sessionID)
		if err := row.Scan(&localRouterID, &remoteRouterID, &localIfaddr, &remoteIfaddr,
			&linkLocalID, &linkRemoteID, &adjSIDs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if localRouterID != "0aff0002" || remoteRouterID != "0aff0005" {
			t.Errorf("local_router_id=%q remote_router_id=%q", localRouterID, remoteRouterID)
		}
		if localIfaddr != "10.1.0.2" || remoteIfaddr != "10.1.0.3" {
			t.Errorf("local_ifaddr=%q remote_ifaddr=%q", localIfaddr, remoteIfaddr)
		}
		// link_local_id/link_remote_id set to distinct values (4, 3),
		// matching real XRd corpus data, where the two values differ.
		if linkLocalID != 4 || linkRemoteID != 3 {
			t.Errorf("link_local_id=%d link_remote_id=%d, want 4/3", linkLocalID, linkRemoteID)
		}
		if len(adjSIDs) != 1 || adjSIDs[0] != 24001 {
			t.Errorf("adj_sids=%v, want [24001]", adjSIDs)
		}
	})

	t.Run("peer_events", func(t *testing.T) {
		var kind, localIP string
		var localPort, remotePort uint16
		var downReason uint32
		var capFourByteAS uint8
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT kind, local_ip, local_port, remote_port, down_reason, cap_four_byte_as
			 FROM vantage.peer_events WHERE session_id = ?`), sessionID)
		if err := row.Scan(&kind, &localIP, &localPort, &remotePort, &downReason, &capFourByteAS); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if kind != "up" || localIP != "10.0.0.20" || localPort != 179 || remotePort != 54321 || capFourByteAS != 1 {
			t.Errorf("kind=%q local_ip=%q local_port=%d remote_port=%d cap_four_byte_as=%d",
				kind, localIP, localPort, remotePort, capFourByteAS)
		}
		// down_reason (UInt32) sits between local/remote_port (UInt16) and
		// cap_four_byte_as (UInt8) in the DDL -- a distinct nonzero value
		// here catches it landing in the wrong column.
		if downReason != 3 {
			t.Errorf("down_reason=%d, want 3", downReason)
		}
	})

	t.Run("stats_events", func(t *testing.T) {
		var counters map[uint32]uint64
		row := c.conn.QueryRow(ctx,
			qualify(c, `SELECT counters FROM vantage.stats_events WHERE session_id = ?`), sessionID)
		if err := row.Scan(&counters); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if counters[1] != 100 || counters[2] != 200 {
			t.Errorf("counters=%v", counters)
		}
	})
}

// TestInsertEmptyRowsIsNoop asserts Insert does not send an empty batch to
// ClickHouse (which the server would reject) when an envelope legitimately
// produced no rows for a table -- e.g. a Ls or PeerEvent envelope has empty
// Unicast/Vpn/Evpn slices, and every one of the six insert* methods must
// treat that as a no-op rather than an error.
func TestInsertEmptyRowsIsNoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	if err := c.Insert(ctx, Rows{}); err != nil {
		t.Fatalf("Insert(Rows{}): %v", err)
	}
}

// TestClickHousePeerDownRoundTrips is the regression test for the bug the
// live dev stack found: collector/session.go's handlePeerDown never
// sets PeerEvent.LocalIp (a Peer Down genuinely has no local address), so it
// decodes to the empty string, and peer_events.local_ip is a bare (non-
// nullable) IPv6 column that rejects an empty string outright. Every real
// Peer Down hit this, and because ack-after-durable never acks a failed
// insert, JetStream redelivered the same batch forever -- a permanent
// livelock on the PEER consumer from the very first session flap, not a rare
// edge case.
//
// A pure sink/rows_test.go test (TestRowsForPeerDownNormalizesEmptyLocalIP)
// covers that RowsFor produces "::" rather than "". It would NOT have caught
// the original bug: the failure only happened at Insert, against ClickHouse's
// actual IPv6 type, which a pure test never touches. This test goes through
// both RowsFor and a real Insert, then reads the stored value back, so it
// covers the whole path the bug was in.
func TestClickHousePeerDownRoundTrips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	sessionID := uint64(time.Now().UnixNano())
	env := testEnv(&vantagev1.PeerEvent{
		Kind: vantagev1.PeerEvent_KIND_DOWN, DownReason: 6,
	})
	env.SessionId = sessionID
	// testEnv stamps ts_collector as time.Unix(1700000001, 0) -- 2023-11-14 --
	// which is fine for the pure rows_test.go callers and is NOT fine here,
	// because this test inserts and then reads back. peer_events is
	// PARTITION BY toYYYYMM(ts_collector) with TTL ts_collector + 90 DAY, so
	// that row lands already years past its delete-TTL, alone in partition
	// 202311, and surviving to the SELECT below is a race against the
	// background TTL merge. It lost that race intermittently.
	// corpus_integration_test.go's corpusCollectorClock documents the same
	// failure for the same reason; this is that fix, applied here.
	env.TsCollector = timestamppb.New(time.Now().UTC().Truncate(time.Second))

	rows := mustRowsFor(t, env, 9)
	if len(rows.Peer) != 1 {
		t.Fatalf("want 1 peer row, got %d", len(rows.Peer))
	}
	if rows.Peer[0].LocalIP == "" {
		t.Fatal("RowsFor produced an empty LocalIP -- the normalization this " +
			"test exists to pin is not happening before Insert is even reached")
	}

	if err := c.Insert(ctx, rows); err != nil {
		t.Fatalf("Insert: %v -- a Peer Down must insert cleanly, not wedge the "+
			"consumer the way the empty-string local_ip did", err)
	}

	var kind, localIP string
	var downReason uint32
	row := c.conn.QueryRow(ctx,
		qualify(c, `SELECT kind, local_ip, down_reason FROM vantage.peer_events WHERE session_id = ?`),
		sessionID)
	if err := row.Scan(&kind, &localIP, &downReason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kind != "down" || downReason != 6 {
		t.Errorf("kind=%q down_reason=%d, want down/6", kind, downReason)
	}
	// ClickHouse renders the unspecified IPv6 address as "::".
	if localIP != "::" {
		t.Errorf("local_ip=%q, want \"::\" (the unspecified address ClickHouse "+
			"round-trips an empty local address to)", localIP)
	}
}

// TestNewClickHouseRedactsAQueryParameterPassword drives the
// query-parameter-credential leak through the real dial path rather than
// through redact's own tests: clickhouse-go accepts the password as a
// query parameter, so this DSN is well-formed -- it passes every
// validation, reaches clickhouse-go, and fails only on the dial -- and
// its error is the one that reaches vantage-writer's slog.Error("fatal",
// ...).
//
// NewClickHouse takes a secret.ClickHouseDSN, so the caller has nothing
// printable to leak, and the error it returns has already been through
// redact.Err. Both halves are asserted here, on a port nothing is listening
// on so the test needs no infrastructure.
func TestNewClickHouseRedactsAQueryParameterPassword(t *testing.T) {
	const sentinel = "SECRET"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, raw := range []string{
		"clickhouse://" + addr + "/vantage?password=" + sentinel + "&dial_timeout=300ms",
		"clickhouse://" + addr + "/vantage?password=p@ss" + sentinel + "&dial_timeout=300ms",
		"clickhouse://vantage:pw" + sentinel + "@" + addr + "/vantage?dial_timeout=300ms",
	} {
		dsn := secret.NewClickHouseDSN(raw)
		c, err := NewClickHouse(ctx, dsn)
		if err == nil {
			c.Close()
			t.Fatalf("NewClickHouse(%q) succeeded against a closed port, want an error", raw)
		}
		// The error itself, and the message a caller builds out of it the
		// obvious way -- which is what cmd/vantage-writer does.
		wrapped := fmt.Errorf("connect clickhouse %s: %w", dsn, err)
		for _, got := range []string{err.Error(), wrapped.Error()} {
			if strings.Contains(got, sentinel) {
				t.Errorf("NewClickHouse(%q) error %q contains the password", raw, got)
			}
		}
		if !strings.Contains(wrapped.Error(), "127.0.0.1") {
			t.Errorf("NewClickHouse(%q) error %q does not name the host", raw, wrapped.Error())
		}
	}
}

// TestNewClickHouseRejectsAMalformedHTTPProxy drives the http_proxy
// credential-leak defect through the real driver: a DSN whose
// "http_proxy" parameter is itself a malformed URL carrying its own
// credential.
//
// It is the one leak redact.Err structurally cannot reach.
// clickhouse-go's fromDSN (v2.48.0) does
//
//	return fmt.Errorf("clickhouse [dsn parse]: http_proxy: %s", err)
//
// -- "%s", not "%w" -- which flattens the *url.Error to text and wraps
// nothing, so errors.As finds no *url.Error and Err hands the message
// back untouched. Before the fix this test's first case produced, on
// stderr, at startup:
//
//	connect clickhouse clickhouse://127.0.0.1:REDACTED/REDACTED?dial_timeout=100ms&http_proxy=REDACTED:
//	  clickhouse: parse dsn: clickhouse [dsn parse]: http_proxy:
//	  parse "http://u:SUPERLONGPASSWORDSECRET@proxy:8o80": invalid port ":8o80" after host
//
// -- a complete credential, in the same line whose other half was already
// correctly redacted. NewClickHouse now pre-validates the parameter, so the
// driver is never handed it; see redact.CheckClickHouseProxy.
//
// The DSN's own host is 127.0.0.1:9 (discard), so every case fails without
// infrastructure and without waiting: the malformed ones fail before any
// dial at all, and the valid-proxy control fails on the dial.
func TestNewClickHouseRejectsAMalformedHTTPProxy(t *testing.T) {
	const sentinel = "SUPERLONGPASSWORDSECRET"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, c := range []struct {
		name      string
		raw       string
		wantProxy bool // the error must be our http_proxy rejection
	}{
		{"invalid port, percent-encoded",
			"clickhouse://127.0.0.1:9/db?dial_timeout=100ms&http_proxy=http%3A%2F%2Fu%3A" +
				sentinel + "%40proxy%3A8o80", true},
		{"invalid port, literal",
			"clickhouse://127.0.0.1:9/db?dial_timeout=100ms&http_proxy=http://u:" +
				sentinel + "@proxy:8o80", true},
		{"space in proxy host",
			"clickhouse://127.0.0.1:9/db?dial_timeout=100ms&http_proxy=http://u:" +
				sentinel + "@pro%20xy:8080", true},
		// The control. A well-formed http_proxy must still be accepted and
		// handed to the driver unmodified -- a pre-validation that rejected
		// working configurations would be a worse bug than the leak. This
		// one gets past the check and fails on the DSN's own dial, which is
		// what wantProxy: false asserts.
		{"valid proxy is not rejected",
			"clickhouse://127.0.0.1:9/db?dial_timeout=100ms&http_proxy=http://u:" +
				sentinel + "@proxy:8080", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn := secret.NewClickHouseDSN(c.raw)
			ch, err := NewClickHouse(ctx, dsn)
			if err == nil {
				ch.Close()
				t.Fatalf("NewClickHouse(%q) succeeded, want an error", c.raw)
			}
			// The error itself and the message vantage-writer builds out
			// of it -- both operands, which is the whole contract.
			wrapped := fmt.Errorf("connect clickhouse %s: %w", dsn, err)
			for _, got := range []string{err.Error(), wrapped.Error()} {
				if strings.Contains(got, sentinel) {
					t.Errorf("NewClickHouse(%q) error %q contains the proxy password",
						c.raw, got)
				}
			}
			named := strings.Contains(err.Error(), "http_proxy")
			if named != c.wantProxy {
				t.Errorf("NewClickHouse(%q) error %q: names http_proxy = %v, want %v",
					c.raw, err, named, c.wantProxy)
			}
			if !strings.Contains(wrapped.Error(), "127.0.0.1") {
				t.Errorf("NewClickHouse(%q) error %q does not name the host",
					c.raw, wrapped.Error())
			}
		})
	}

	// And the same values through Validate, which is what vantage-writer
	// calls first: the fast failure and the driver-adjacent guard must
	// agree, or an operator gets a confusing two-stage rejection.
	bad := "clickhouse://127.0.0.1:9/db?http_proxy=http://u:" + sentinel + "@proxy:8o80"
	if err := secret.NewClickHouseDSN(bad).Validate(); err == nil {
		t.Error("ClickHouseDSN.Validate accepted a malformed http_proxy")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Errorf("ClickHouseDSN.Validate error %q contains the proxy password", err)
	}
}

// TestClickHouseExplodedUpdateSurvivesMerge is the live half of the sort-key
// contract, and it is the assertion that would have caught the defect
// rows_test.go's TestRowsForDistinctSortTuplesPerRow describes: the pure test
// proves the rows differ in the columns schema.sql *says* it sorts on; this
// one proves ClickHouse itself keeps them, by forcing the merge that does the
// collapsing rather than waiting for a background one.
//
// It checks both directions at once, which is the point:
//
//   - One envelope's several routes must all survive OPTIMIZE ... FINAL. A
//     sort key that under-specifies route identity leaves one row per
//     envelope here, exactly as it did in a lab archive.
//   - The same envelope inserted twice -- a JetStream redelivery, which the
//     writer's ack-after-durable loop deliberately allows -- must collapse
//     back to the same count, not double it. Widening a sort key is only
//     safe while that holds.
func TestClickHouseExplodedUpdateSurvivesMerge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	// stream_seq is in every sort key, so a fresh one per run keeps this
	// test's rows from sharing a tuple with anything else in the database.
	streamSeq := uint64(time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	env := &vantagev1.Envelope{
		CollectorId: "chtest",
		Router:      &vantagev1.RouterId{Ip: "10.0.0.1", SysName: "sysA"},
		Peer:        &vantagev1.PeerId{Ip: "10.0.0.2", Asn: 65001, BgpId: "10.0.0.3"},
		SessionId:   7,
		Seq:         42,
		// A recent ts_collector, because that is what schema.sql's TTL is
		// keyed on: an epoch timestamp here would make every row below
		// already expired, and the OPTIMIZE ... FINAL these assertions rely
		// on would delete them all rather than merely merging them.
		TsRouter:    timestamppb.New(now),
		TsCollector: timestamppb.New(now),
		Payload: &vantagev1.Envelope_Route{Route: &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 25, Safi: 70},
			// Four EVPN routes with no prefix between them: two type-2
			// MAC/IPs for one VLAN (sharing RD, tag and label, differing
			// only in MAC and IP) and two type-3 IMETs under different RDs.
			// This is the shape a leaf sends on its initial RIB dump.
			EvpnAnnounced: []*vantagev1.EvpnRoute{
				{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:01",
					Ip: "192.168.10.11", Labels: []uint32{10010}},
				{RouteType: 2, Rd: "10.255.1.2:32777", Mac: "00:50:79:66:68:02",
					Ip: "192.168.10.12", Labels: []uint32{10010}},
				{RouteType: 3, Rd: "10.255.1.2:32777", Ip: "10.255.1.2"},
				{RouteType: 3, Rd: "10.255.1.3:32777", Ip: "10.255.1.3"},
			},
			// The same prefix exported by two VRFs under different RDs.
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.9.9.0/24", Rd: "65000:1", Labels: []uint32{24001}},
				{Prefix: "10.9.9.0/24", Rd: "65000:2", Labels: []uint32{24002}},
			},
			// The same prefix under two add-path path-ids.
			Announced: []*vantagev1.Prefix{
				{Prefix: "10.8.8.0/24", PathId: 1},
				{Prefix: "10.8.8.0/24", PathId: 2},
			},
		}},
	}
	rows := mustRowsFor(t, env, streamSeq)

	// Twice: the second Insert is the redelivery.
	for i := range 2 {
		if err := c.Insert(ctx, rows); err != nil {
			t.Fatalf("Insert %d: %v", i+1, err)
		}
	}

	for _, tc := range []struct {
		table string
		want  uint64
	}{
		{"route_evpn", uint64(len(rows.Evpn))},
		{"route_vpn", uint64(len(rows.Vpn))},
		{"route_unicast", uint64(len(rows.Unicast))},
	} {
		t.Run(tc.table, func(t *testing.T) {
			// OPTIMIZE ... FINAL forces the merge ReplacingMergeTree would
			// otherwise do in the background at an unpredictable time. Without
			// it this test would pass against a broken sort key simply by
			// reading the parts before they were merged -- which is exactly
			// how the defect stayed invisible in a lab archive.
			if err := c.conn.Exec(ctx, "OPTIMIZE TABLE "+c.db+"."+tc.table+" FINAL"); err != nil {
				t.Fatalf("OPTIMIZE %s FINAL: %v", tc.table, err)
			}
			var got uint64
			if err := c.conn.QueryRow(ctx, "SELECT count() FROM "+c.db+"."+tc.table+
				" WHERE stream_seq = ?", streamSeq).Scan(&got); err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != tc.want {
				t.Errorf("%s: %d rows survived the merge, want %d -- one envelope's "+
					"routes are sharing a sort tuple, so ReplacingMergeTree kept one "+
					"and discarded the rest (see schema.sql's ORDER BY and "+
					"TestRowsForDistinctSortTuplesPerRow)", tc.table, got, tc.want)
			}
		})
	}
}

// TestClickHouseZeroRouterTimestampSurvives pins the other half of the
// schema's timestamp decision: PARTITION BY and TTL are keyed on
// ts_collector, the collector's own clock, and never on ts_router.
//
// ts_router is whatever the router put in the BMP per-peer header, and this
// project already knows it cannot be trusted -- that is what
// collector/session.go's QK_TS_ZERO quirk is for. The quirk is
// defeatable by design (`disable_quirks: ["QK_TS_ZERO"]` means "trust the
// router's zero"), so a zero ts_router does reach the writer, and a router
// with a merely wrong clock is not covered by the quirk at all. Keyed on
// ts_router, such rows land in partition 197001 with an already-expired
// delete-TTL and the next merge removes them silently; a clock set far in
// the future sprays toYYYYMM partitions across a 136-year range instead,
// which pushes the table toward "too many parts" and fails inserts for
// every router at once.
//
// So: a row whose ts_router is the epoch must survive a merge and land in
// the current month's partition. This test fails outright against the
// ts_router-keyed version of the DDL.
func TestClickHouseZeroRouterTimestampSurvives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	streamSeq := uint64(time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := Rows{Stats: []StatsRow{{
		CollectorID: "chtest", RouterIP: "10.0.0.1", RouterSysname: "sysA",
		PeerIP: "10.0.0.2", RIB: "in_pre", PeerASN: 65001,
		PeerBGPID: "10.0.0.3", SessionID: 7, Seq: 1,
		// The wire value a router with a zero clock sends, passed
		// through rather than corrected -- which is what happens when
		// QK_TS_ZERO is disabled.
		TsRouter: time.Unix(0, 0).UTC(), TsCollector: now,
		StreamSeq: streamSeq,
		Counters:  map[uint32]uint64{1: 1},
	}}}
	if err := c.Insert(ctx, rows); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := c.conn.Exec(ctx, "OPTIMIZE TABLE "+c.db+".stats_events FINAL"); err != nil {
		t.Fatalf("OPTIMIZE FINAL: %v", err)
	}
	var got uint64
	if err := c.conn.QueryRow(ctx, "SELECT count() FROM "+c.db+
		".stats_events WHERE stream_seq = ?", streamSeq).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Fatalf("a row with ts_router = 1970-01-01 did not survive a merge "+
			"(count = %d, want 1) -- PARTITION BY/TTL must be keyed on "+
			"ts_collector, not on the router-supplied timestamp", got)
	}
	// _partition_id is the MergeTree virtual column naming the partition the
	// row actually landed in -- asked of the row itself rather than of
	// system.parts, so this cannot accidentally read some other test's part.
	var partition string
	if err := c.conn.QueryRow(ctx, "SELECT any(_partition_id) FROM "+c.db+
		".stats_events WHERE stream_seq = ?", streamSeq).Scan(&partition); err != nil {
		t.Fatalf("read partition: %v", err)
	}
	if want := now.Format("200601"); partition != want {
		t.Errorf("row landed in partition %q, want %q -- PARTITION BY is "+
			"keyed on the wrong timestamp column", partition, want)
	}
}

// ribEnumDecls returns, for each table in ddl that has a rib column, the
// table name and the full text of its Enum8 declaration. Tables are found by
// scanning forward from each CREATE TABLE to that table's rib line, so a new
// table with a rib column is picked up without editing this helper -- which
// is the point: a rib column added later must not quietly skip the checks
// below.
func ribEnumDecls(t *testing.T, ddl string) map[string]string {
	t.Helper()
	out := map[string]string{}
	create := regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?vantage\.(\w+)`)
	ribCol := regexp.MustCompile(`(?im)^\s*rib\s+(Enum8\([^)]*\))`)
	locs := create.FindAllStringSubmatchIndex(ddl, -1)
	for i, loc := range locs {
		end := len(ddl)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		name := ddl[loc[2]:loc[3]]
		if m := ribCol.FindStringSubmatch(ddl[loc[1]:end]); m != nil {
			out[name] = m[1]
		}
	}
	if len(out) == 0 {
		t.Fatal("found no rib columns in schema.sql; the helper's regexps have drifted from the DDL")
	}
	return out
}

// Every rib column must be able to name a Loc-RIB route. RFC 9069 Loc-RIB is
// not one of the four adj-RIB directions -- it is the router's own table
// after best-path selection -- and without an enum value for it, FRR's
// Loc-RIB routes would be stored as in_pre from a peer at 0.0.0.0 and
// inflate every in_pre count. The four original values must keep their
// numbers: rows already in the archive were written with them, and an Enum8
// is stored as its number.
func TestSchemaRibEnumCanExpressLocRib(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	for table, decl := range ribEnumDecls(t, string(ddl)) {
		for _, want := range []string{
			"'in_pre' = 0", "'in_post' = 1", "'out_pre' = 2", "'out_post' = 3", "'loc_rib' = 4",
		} {
			if !strings.Contains(decl, want) {
				t.Errorf("vantage.%s: rib enum is missing %s:\n\t%s", table, want, decl)
			}
		}
	}
}

// A Loc-RIB row must survive the round trip to a real ClickHouse, not just
// through ribName. The consequence of the narrow enum is worse than a wrong
// label: ClickHouse rejects an unknown enum element outright
// (UNKNOWN_ELEMENT_OF_ENUM, verified against the pinned 24.8 image), and the
// writer batches several tables per flush -- so the first Loc-RIB session
// against an unmigrated database would fail whole batches, not mislabel rows.
// Only a live insert proves the deployed shape actually accepts the value;
// ribName's unit tests would pass against a database that predates this
// column.
func TestInsertRoundTripStoresLocRib(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	sessionID := uint64(time.Now().UnixNano())
	ts := time.Now().UTC().Truncate(time.Microsecond)
	// peer_ip 0.0.0.0 is not incidental: RFC 9069 §4.2 says the Loc-RIB peer
	// carries no peer address, and FRR sends exactly this. It is also the
	// value that made the old behavior ambiguous, so storing it alongside
	// rib = 'loc_rib' is what the fix has to make readable again.
	env := envelope{
		CollectorID: "chtest", RouterIP: "10.99.0.1", RouterSysname: "frr1",
		PeerIP: "0.0.0.0", RIB: "loc_rib", PeerASN: 65001,
		PeerBGPID: "10.99.0.1", SessionID: sessionID, Seq: 1,
		TsRouter: ts, TsCollector: ts, StreamSeq: 1,
	}
	rows := Rows{Unicast: []UnicastRow{{
		envelope: env, Origin: 0, NextHop: "0.0.0.0",
		Family: "ipv4u", Prefix: "10.99.0.1/32",
	}}}
	if err := c.Insert(ctx, rows); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	var rib, peerIP, prefix string
	row := c.conn.QueryRow(ctx,
		qualify(c, `SELECT rib, toString(peer_ip), prefix FROM vantage.route_unicast
		 WHERE session_id = ?`), sessionID)
	if err := row.Scan(&rib, &peerIP, &prefix); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rib != "loc_rib" {
		t.Errorf("rib = %q, want %q", rib, "loc_rib")
	}
	if prefix != "10.99.0.1/32" {
		t.Errorf("prefix = %q, want 10.99.0.1/32", prefix)
	}
	// The four adj-RIB names must still read back as themselves. An Enum8 is
	// stored as its number, so renumbering the existing values instead of
	// appending would silently relabel every row already in the archive.
	var counts uint64
	if err := c.conn.QueryRow(ctx, qualify(c,
		`SELECT count() FROM vantage.route_unicast WHERE rib = 'in_pre'`)).Scan(&counts); err != nil {
		t.Fatalf("in_pre still selectable: %v", err)
	}
}

// createTableStatement returns the full text of ddl's CREATE TABLE statement
// for one qualified table name -- from "CREATE TABLE" through the semicolon
// that ends it, so the engine, PARTITION BY, ORDER BY and TTL clauses are all
// inside what it hands back, not just the column list.
//
// It t.Fatals when there is no such statement rather than returning "". Every
// caller below asserts that some substring is or is not present, and against
// an empty string a "must not contain" assertion passes vacuously -- so a
// table that was renamed, or never added at all, would read as a clean pass.
func createTableStatement(t *testing.T, ddl, qualified string) string {
	t.Helper()
	re := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?` +
		regexp.QuoteMeta(qualified) + `\s*\(.*?;`)
	stmt := re.FindString(ddl)
	if stmt == "" {
		t.Fatalf("schema.sql has no CREATE TABLE for %s", qualified)
	}
	return stmt
}

// TestEORTableIsNotAReplacingMergeTree pins the one design decision in
// eor_events that is easy to get wrong by pattern-matching the route
// tables. Every route table is ReplacingMergeTree because a redelivered
// envelope must collapse to one row; eor_events is a plain MergeTree
// because its rows are already unique by (router, peer, rib, session,
// family) and, more importantly, because a ReplacingMergeTree here would
// invite the same "count() is wrong until the next merge" hazard that
// route_counts needs uniqExact to work around -- for a table whose entire
// purpose is to be counted and joined against.
func TestEORTableIsNotAReplacingMergeTree(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	stmt := createTableStatement(t, string(b), "vantage.eor_events")
	if strings.Contains(stmt, "ReplacingMergeTree") {
		t.Errorf("eor_events must be a plain MergeTree, not ReplacingMergeTree:\n\t%s", stmt)
	}
	for _, col := range []string{"family", "session_id", "seq", "stream_seq", "rib"} {
		if !strings.Contains(stmt, col) {
			t.Errorf("eor_events is missing column %q:\n\t%s", col, stmt)
		}
	}
}

// TestNoRouteTableCarriesEndOfRib is the structural half of the EOR move:
// the whole point is that a route table cannot contain a collection
// artifact, so no route table may keep the column that made that possible.
// This is the test that would catch a well-meaning revert.
func TestNoRouteTableCarriesEndOfRib(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	for _, table := range []string{"route_unicast", "route_vpn", "route_evpn"} {
		stmt := createTableStatement(t, string(b), "vantage."+table)
		if strings.Contains(stmt, "end_of_rib") {
			t.Errorf("vantage.%s still carries end_of_rib; EOR lives in eor_events:\n\t%s", table, stmt)
		}
	}
}

// TestEORMarkerBecomesAnEORRowAndNotARoute is the behavioral half of the
// EOR move. It asserts both directions, because only asserting the first
// would pass for an implementation that writes the marker to BOTH tables --
// which is the shape that leaves the original defect fully intact while
// looking like the fix.
//
// The marker's evpn family is not decoration either. Before the split a
// marker for a family with no unicast table still had to land in
// route_unicast, so "has this peer's EVPN dump finished" was answerable only
// by reading non-unicast rows back out of the unicast table -- which nothing
// did, so dump progress read 'unknown' forever for a VPN- or EVPN-only peer.
// eor_events carries the family itself, which is what makes the question
// askable at all.
func TestEORMarkerBecomesAnEORRowAndNotARoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	// Unique per run, so this test owns its rows in a database it shares
	// with every other test in this file.
	sessionID := uint64(time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	env := func(seq uint64, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "chtest",
			Router:      &vantagev1.RouterId{Ip: "10.0.198.1", SysName: "sysEOR"},
			Peer: &vantagev1.PeerId{
				Ip: "10.255.198.1", Asn: 65001, BgpId: "10.255.198.1",
			},
			SessionId:   sessionID,
			Seq:         seq,
			TsRouter:    timestamppb.New(now),
			TsCollector: timestamppb.New(now),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}

	// Both envelopes go through RowsFor, not through a hand-built Rows: the
	// filing decision this test is about lives there, and a fixture that
	// placed the rows itself would assert nothing about it.
	var rows Rows
	// An ordinary ipv4-unicast announcement...
	rows.Add(mustRowsFor(t, env(1, &vantagev1.RouteEvent{
		Family:    &vantagev1.Family{Afi: 1, Safi: 1},
		Announced: []*vantagev1.Prefix{{Prefix: "10.198.0.0/24"}},
	}), 1))
	// ...and the End-of-RIB marker that terminates the EVPN dump.
	rows.Add(mustRowsFor(t, env(2, &vantagev1.RouteEvent{
		Family:   &vantagev1.Family{Afi: 25, Safi: 70},
		EndOfRib: true,
	}), 2))

	if err := c.Insert(ctx, rows); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var routeRows uint64
	if err := c.conn.QueryRow(ctx, qualify(c,
		`SELECT count() FROM vantage.route_unicast WHERE session_id = ?`),
		sessionID).Scan(&routeRows); err != nil {
		t.Fatalf("count routes: %v", err)
	}
	if routeRows != 1 {
		t.Errorf("route_unicast rows = %d, want 1 (the announcement only; the "+
			"EOR marker must not be filed as a route)", routeRows)
	}

	var eorRows uint64
	var fam string
	if err := c.conn.QueryRow(ctx, qualify(c,
		`SELECT count(), any(family) FROM vantage.eor_events WHERE session_id = ?`),
		sessionID).Scan(&eorRows, &fam); err != nil {
		t.Fatalf("count eor: %v", err)
	}
	if eorRows != 1 || fam != "evpn" {
		t.Errorf("eor_events = %d rows family %q, want 1 row family \"evpn\"", eorRows, fam)
	}
}

// TestCheckAddressesMatchesWhatClickHouseRefuses pins checkAddresses against
// the real driver and server from both sides. Each value it refuses is
// built into a row directly -- past RowsFor, the way it was built before
// the check existed -- and must fail the insert (or panic in the driver,
// which is what an IPv6 peer_bgp_id does); each value it accepts must
// insert. A check looser than the driver leaves the wedge open; a stricter
// one drops envelopes ClickHouse would have stored.
func TestCheckAddressesMatchesWhatClickHouseRefuses(t *testing.T) {
	ctx := t.Context()
	ch := requireClickHouse(t, ctx)
	insert := func(rows Rows) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("driver panicked: %v", r)
			}
		}()
		return ch.Insert(ctx, rows)
	}
	build := func(env *vantagev1.Envelope) Rows {
		e := commonColumns(env, 1)
		return Rows{Peer: []PeerRow{peerRow(e, "", env.GetPeerEvent())}}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*vantagev1.Envelope)
		refused bool
	}{
		{"router ip x", func(e *vantagev1.Envelope) { e.Router.Ip = "x" }, true},
		{"peer ip out of range", func(e *vantagev1.Envelope) { e.Peer.Ip = "10.0.0.256" }, true},
		{"bgp id not an address", func(e *vantagev1.Envelope) { e.Peer.BgpId = "not-an-id" }, true},
		{"bgp id IPv6", func(e *vantagev1.Envelope) { e.Peer.BgpId = "2001:db8::1" }, true},
		{"local ip x", func(e *vantagev1.Envelope) { e.GetPeerEvent().LocalIp = "x" }, true},
		{"as built", func(*vantagev1.Envelope) {}, false},
		{"IPv6 router and peer", func(e *vantagev1.Envelope) { e.Router.Ip = "2001:db8::1"; e.Peer.Ip = "fe80::1" }, false},
		{"IPv4-mapped bgp id", func(e *vantagev1.Envelope) { e.Peer.BgpId = "::ffff:10.0.0.1" }, false},
	} {
		env := testEnv(&vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP, LocalIp: "10.0.0.2"})
		tc.mutate(env)
		checkErr := checkAddresses(env)
		insertErr := insert(build(env))
		if tc.refused {
			if checkErr == nil {
				t.Errorf("%s: checkAddresses accepted it", tc.name)
			}
			if insertErr == nil {
				t.Errorf("%s: ClickHouse stored it, so refusing it drops a storable envelope", tc.name)
			}
		} else {
			if checkErr != nil {
				t.Errorf("%s: checkAddresses refused it: %v", tc.name, checkErr)
			}
			if insertErr != nil {
				t.Errorf("%s: insert failed: %v", tc.name, insertErr)
			}
		}
	}
}

// TestInsertRoundTripStoresABeat proves three things about the one insert that
// names its columns.
//
//   - started_at keeps its nanoseconds.
//   - beat_at lands where it belongs.
//   - inserted_at is filled by ClickHouse at insert. Every liveness read
//     compares it to the server's now64(), so a zero or a writer-supplied
//     value here would make every collector stale, or none.
func TestInsertRoundTripStoresABeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	collectorID := fmt.Sprintf("liveness-sink-roundtrip-%d", time.Now().UnixNano())
	started := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	built := time.Date(2026, 9, 23, 10, 0, 42, 5_000_000, time.UTC)
	before := time.Now()
	if err := c.Insert(ctx, Rows{Beats: []BeatRow{{
		CollectorID: collectorID, StartedAt: started, BeatAt: built,
	}}}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	after := time.Now()

	var startedNS int64
	var beatAt, insertedAt time.Time
	if err := c.conn.QueryRow(ctx, qualify(c, `SELECT toUnixTimestamp64Nano(started_at), beat_at, inserted_at
		FROM vantage.collector_beats WHERE collector_id = ?`), collectorID).
		Scan(&startedNS, &beatAt, &insertedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if startedNS != started.UnixNano() {
		t.Errorf("started_at = %d ns, want %d", startedNS, started.UnixNano())
	}
	if !beatAt.Equal(built) {
		t.Errorf("beat_at = %v, want %v", beatAt, built)
	}
	// The dev ClickHouse and this process share a host clock; five seconds of
	// slack on each side covers the insert and nothing else.
	if insertedAt.Before(before.Add(-5*time.Second)) || insertedAt.After(after.Add(5*time.Second)) {
		t.Errorf("inserted_at = %v, want the server's clock between %v and %v", insertedAt, before, after)
	}
}
