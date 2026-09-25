package query

import (
	"context"
	"fmt"
	"testing"
)

// buildASNamesMetaDictionary does everything setupASNamesMetaDictionary's
// own doc comment describes -- the source table, its one row, the
// dictionary itself, both teardowns -- but deliberately stops short of
// forcing a load, the same split buildASNamesDictionary/
// setupASNamesDictionary makes in asnames_test.go and for the identical
// reason: dictionaries_lazy_load's own default (true) leaves a freshly
// created dictionary NOT_LOADED, which is exactly the state EVERY
// dictionary in this project is in immediately after a real ClickHouse
// restart, data and all.
// TestASNamesPublishedSelfHealsAfterARestartWithTheDataStillOnDisk is the
// one test that wants this state on purpose; every other test in this file
// wants a guaranteed-LOADED fixture and calls setupASNamesMetaDictionary
// instead.
func buildASNamesMetaDictionary(t *testing.T, ctx context.Context, q *Q, lastModified string) {
	t.Helper()
	const srcTable = "asnames_meta_test_src"
	table := q.db + "." + srcTable
	dict := q.db + ".asnames_meta"

	if err := q.conn.Exec(ctx, "DROP DICTIONARY IF EXISTS "+dict); err != nil {
		t.Fatalf("asnames_meta fixture: drop stray dictionary: %v", err)
	}
	if err := q.conn.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
		t.Fatalf("asnames_meta fixture: drop stray source table: %v", err)
	}

	if err := q.conn.Exec(ctx, "CREATE TABLE "+table+
		" (id UInt8, last_modified String) ENGINE = Memory"); err != nil {
		t.Fatalf("asnames_meta fixture: create source table: %v", err)
	}
	t.Cleanup(func() {
		if err := q.conn.Exec(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+table); err != nil {
			t.Errorf("asnames_meta fixture cleanup: drop source table: %v", err)
		}
	})

	if err := q.conn.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s (id, last_modified) VALUES (0, ?)", table),
		lastModified); err != nil {
		t.Fatalf("asnames_meta fixture: insert row: %v", err)
	}

	createDict := fmt.Sprintf(`CREATE DICTIONARY %s
(id UInt8, last_modified String)
PRIMARY KEY id
SOURCE(CLICKHOUSE(HOST 'localhost' PORT 9000 USER 'vantage' PASSWORD 'vantage' DB '%s' TABLE '%s'))
LAYOUT(HASHED())
LIFETIME(0)`, dict, q.db, srcTable)
	if err := q.conn.Exec(ctx, createDict); err != nil {
		t.Fatalf("asnames_meta fixture: create dictionary: %v", err)
	}
	t.Cleanup(func() {
		if err := q.conn.Exec(context.WithoutCancel(ctx), "DROP DICTIONARY IF EXISTS "+dict); err != nil {
			t.Errorf("asnames_meta fixture cleanup: drop dictionary: %v", err)
		}
	})
}

// setupASNamesMetaDictionary builds vantage_query_test.asnames_meta as a
// CLICKHOUSE-sourced dictionary over a throwaway table carrying exactly one
// row (id = 0, last_modified = lastModified), forces it to LOAD
// synchronously, and registers its own teardown -- the same recipe
// setupASNamesDictionary uses for asnames itself, and for the same reason:
// a host-side test cannot put a file where a FILE-sourced dictionary would
// find it, but the code under test
// here -- one dictGetOrDefault call, classified by isNoASNamesDataset on
// failure -- is source-agnostic, so a CLICKHOUSE-sourced dictionary
// exercises the identical path while staying entirely inside this test's
// own database.
func setupASNamesMetaDictionary(t *testing.T, ctx context.Context, q *Q, lastModified string) {
	t.Helper()
	buildASNamesMetaDictionary(t, ctx, q, lastModified)
	dict := q.db + ".asnames_meta"
	if err := q.conn.Exec(ctx, "SYSTEM RELOAD DICTIONARY "+dict); err != nil {
		t.Fatalf("asnames_meta fixture: force load: %v", err)
	}
}

// TestASNamesPublishedSelfHealsAfterARestartWithTheDataStillOnDisk is
// ASNamesPublished's own half of the regression guard -- see
// TestASNamesSelfHealsAfterARestartWithTheDataStillOnDisk in
// asnames_test.go for the full reasoning, identical here except that this
// dictionary's own lookup (asNamesPublishedSQL) is a single QueryRow with
// no arrayJoin, a different code path through isNoASNamesDataset than the
// batch statement asnames_test.go's own version proves.
func TestASNamesPublishedSelfHealsAfterARestartWithTheDataStillOnDisk(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	buildASNamesMetaDictionary(t, ctx, q, publishedDateFixture)

	var status string
	if err := q.conn.QueryRow(ctx,
		"SELECT status FROM system.dictionaries WHERE database = ? AND name = 'asnames_meta'",
		q.db).Scan(&status); err != nil {
		t.Fatalf("read dictionary status: %v", err)
	}
	if status != "NOT_LOADED" {
		t.Fatalf("precondition failed: status = %s, want NOT_LOADED -- "+
			"buildASNamesMetaDictionary was supposed to leave this dictionary unloaded", status)
	}

	got, loaded, err := q.ASNamesPublished(ctx)
	if err != nil {
		t.Fatalf("ASNamesPublished: %v", err)
	}
	if !loaded {
		t.Fatal("loaded = false, want true -- the dictionary's data is present and correct; " +
			"a single ASNamesPublished call must be able to load it itself, the same way a " +
			"real request after a ClickHouse restart has to")
	}
	if got != publishedDateFixture {
		t.Errorf("got %q, want %q", got, publishedDateFixture)
	}
}

// TestASNamesPublishedReportsNoDatasetByDefault covers the same "chtest
// never applies this DDL" default state TestASNamesDistinguishesUnlistedFromNoDataset's
// first subtest does, for the companion dictionary: a truly fresh
// vantage_query_test carries no asnames_meta object at all, and that must
// answer loaded=false with no error, not a Code 36 BAD_ARGUMENTS surfaced
// to this package's caller.
func TestASNamesPublishedReportsNoDatasetByDefault(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)

	got, loaded, err := q.ASNamesPublished(ctx)
	if err != nil {
		t.Fatalf("ASNamesPublished: %v", err)
	}
	if loaded {
		t.Errorf("loaded = true, want false -- vantage_query_test has no asnames_meta "+
			"dictionary object at all yet; got %q", got)
	}
	if got != "" {
		t.Errorf("got %q, want \"\" when there is no dataset", got)
	}
}

// publishedDateFixture is the Last-Modified text this file's own positive
// test stores and expects back UNCHANGED -- a real HTTP-date, in the exact
// form curl -D captures live, so a caller parsing it (api/asnames.go, not
// this package) has a realistic value to parse.
const publishedDateFixture = "Fri, 18 Sep 2026 10:49:00 GMT"

// TestASNamesPublishedReturnsTheStoredDateVerbatim is the positive case:
// loaded, with a value, returned exactly as stored -- no reformatting, no
// parsing, matching this method's own doc comment on why that is api/'s job
// and not this package's.
func TestASNamesPublishedReturnsTheStoredDateVerbatim(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	setupASNamesMetaDictionary(t, ctx, q, publishedDateFixture)

	got, loaded, err := q.ASNamesPublished(ctx)
	if err != nil {
		t.Fatalf("ASNamesPublished: %v", err)
	}
	if !loaded {
		t.Fatal("loaded = false, want true -- the dictionary was just built and forced " +
			"to LOADED; every assertion below rests on this")
	}
	if got != publishedDateFixture {
		t.Errorf("got %q, want %q UNCHANGED", got, publishedDateFixture)
	}
}

// TestASNamesPublishedTreatsAnEmptyStoredValueAsNoDate is the one case where
// a present, LOADED row must still answer loaded=false: last_modified
// itself scanned as "". That can arrive two ways -- no row for the bound
// key at all (dictGetOrDefault's own default) or a row whose own value
// genuinely is "" -- and both must read identically to a caller, since an
// empty string is not a date either way and this package never guesses one.
func TestASNamesPublishedTreatsAnEmptyStoredValueAsNoDate(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	setupASNamesMetaDictionary(t, ctx, q, "")

	got, loaded, err := q.ASNamesPublished(ctx)
	if err != nil {
		t.Fatalf("ASNamesPublished: %v", err)
	}
	if loaded {
		t.Errorf("loaded = true, want false -- the stored value is the empty string, "+
			"which is not a date; got %q", got)
	}
	if got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}

// missingFileMetaDictPath mirrors query/asnames_test.go's own
// missingFileDictPath: a path inside ClickHouse's own user_files_path that
// simply does not exist, so CREATE DICTIONARY succeeds (the object is
// valid) but the dictionary can never LOAD (Code 107 FILE_DOESNT_EXIST on
// any dictGet against it).
const missingFileMetaDictPath = "/var/lib/clickhouse/user_files/asnames_test_missing_file/asnames_meta_does_not_exist.tsv"

// TestASNamesPublishedReportsNoDatasetWhenTheDictionaryFileIsMissing is the
// companion dictionary's own half of state 3's middle case, applied here
// rather than to asnames itself: a dictionary object can exist
// in system.dictionaries, status NOT_LOADED, with no data behind it because
// its source file is gone (the ordinary "DDL applied, `make fetch-asnames`
// never run" shape, and -- see isNoASNamesDataset's own doc comment in
// query/asnames.go -- also the shape this exact dictionary is in
// immediately after a ClickHouse restart). isNoASNamesDataset's Code 107
// classification is what keeps that from surfacing to this package's
// caller as an error, now that ASNamesPublished calls dictGetOrDefault
// directly rather than gating on a separate status check first.
func TestASNamesPublishedReportsNoDatasetWhenTheDictionaryFileIsMissing(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	dict := q.db + ".asnames_meta"

	if err := q.conn.Exec(ctx, "DROP DICTIONARY IF EXISTS "+dict); err != nil {
		t.Fatalf("drop stray dictionary: %v", err)
	}
	createDict := fmt.Sprintf(`CREATE DICTIONARY %s
(id UInt8, last_modified String)
PRIMARY KEY id
SOURCE(FILE(path '%s' format 'TSV'))
LAYOUT(HASHED())
LIFETIME(0)`, dict, missingFileMetaDictPath)
	if err := q.conn.Exec(ctx, createDict); err != nil {
		t.Fatalf("create FILE-sourced dictionary: %v", err)
	}
	t.Cleanup(func() {
		if err := q.conn.Exec(context.WithoutCancel(ctx), "DROP DICTIONARY IF EXISTS "+dict); err != nil {
			t.Errorf("cleanup: drop dictionary: %v", err)
		}
	})

	// Precondition, asserted rather than assumed -- see
	// TestASNamesReportsNoDatasetWhenTheDictionaryFileIsMissing's own
	// identical check on asnames itself for why.
	var status string
	if err := q.conn.QueryRow(ctx,
		"SELECT status FROM system.dictionaries WHERE database = ? AND name = 'asnames_meta'",
		q.db).Scan(&status); err != nil {
		t.Fatalf("read dictionary status: %v", err)
	}
	if status == "LOADED" {
		t.Fatalf("precondition failed: status = LOADED, want NOT_LOADED -- %s was not "+
			"supposed to exist", missingFileMetaDictPath)
	}

	got, loaded, err := q.ASNamesPublished(ctx)
	if err != nil {
		t.Fatalf("ASNamesPublished returned an error: %v -- the missing-file half of "+
			"state 3 must report loaded=false, not surface ClickHouse's own Code 107 "+
			"FILE_DOESNT_EXIST to this package's caller", err)
	}
	if loaded {
		t.Error("loaded = true, want false -- this dictionary's row exists but its " +
			"source file does not")
	}
	if got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}
