package sink

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jp2195/vantage/chtest"
)

// migrationTestDB is this test's own database. The test drops a table and
// rewinds schema_version, which no other test may see.
const migrationTestDB = "vantage_sink_migration_test"

// TestMigration001MatchesAFreshSchema moves a version 1 database to version 2
// with 001-collector-beats.sql and requires the result to be what schema.sql
// creates fresh: the same collector_beats columns, types and defaults, the
// same engine and sort key, and schema_version 2. It then applies the file a
// second time and requires nothing to change, because the Helm schema Job
// applies every migration on every upgrade.
//
// A fresh database and a migrated one reporting the same version while
// holding different shapes is the failure schema_version exists to prevent,
// and neither TestExpectedSchemaVersion nor the Helm mirror test looks at the
// migration's DDL at all.
func TestMigration001MatchesAFreshSchema(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, migrationTestDB)

	type shape struct{ columns, engine string }
	read := func() shape {
		t.Helper()
		var s shape
		if err := conn.QueryRow(ctx, `
			SELECT arrayStringConcat(groupArray(concat(name, ' ', type, ' ', default_kind, ' ', default_expression)), ', ')
			FROM (SELECT name, type, default_kind, default_expression FROM system.columns
			      WHERE database = ? AND table = 'collector_beats' ORDER BY position)`,
			migrationTestDB).Scan(&s.columns); err != nil {
			t.Fatalf("read columns: %v", err)
		}
		if err := conn.QueryRow(ctx,
			"SELECT engine_full FROM system.tables WHERE database = ? AND name = 'collector_beats'",
			migrationTestDB).Scan(&s.engine); err != nil {
			t.Fatalf("read engine: %v", err)
		}
		return s
	}
	version := func() (max uint32, rows uint64) {
		t.Helper()
		if err := conn.QueryRow(ctx, "SELECT max(version), count() FROM "+migrationTestDB+".schema_version").
			Scan(&max, &rows); err != nil {
			t.Fatalf("read schema_version: %v", err)
		}
		return max, rows
	}

	fresh := read()
	if fresh.columns == "" {
		t.Fatal("schema.sql created no collector_beats; this test compares nothing")
	}

	// Back to version 1: the table gone and the version rewound -- exactly what
	// a database the version 1 schema.sql created holds.
	for _, stmt := range []string{
		"DROP TABLE " + migrationTestDB + ".collector_beats",
		"TRUNCATE TABLE " + migrationTestDB + ".schema_version",
		"INSERT INTO " + migrationTestDB + ".schema_version (version) VALUES (1)",
	} {
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	body, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "migrations", "001-collector-beats.sql"))
	if err != nil {
		t.Fatal(err)
	}
	apply := func() {
		t.Helper()
		for _, stmt := range chtest.SchemaStatements(string(body), migrationTestDB) {
			if err := conn.Exec(ctx, stmt); err != nil {
				t.Fatalf("apply migration: %v", err)
			}
		}
	}

	apply()
	if got := read(); got != fresh {
		t.Errorf("migrated collector_beats differs from a fresh one:\nmigrated: %+v\nfresh:    %+v", got, fresh)
	}
	if v, n := version(); v != 2 || n != 2 {
		t.Errorf("after one application: max(version) = %d over %d rows, want 2 over 2 (1, then 2)", v, n)
	}

	apply()
	if v, n := version(); v != 2 || n != 2 {
		t.Errorf("after a second application: max(version) = %d over %d rows, want 2 over 2 -- "+
			"the version INSERT must be conditional", v, n)
	}
}
