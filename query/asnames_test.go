package query

import (
	"context"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// asNamesSrcTable is the plain table setupASNamesDictionary's own
// CLICKHOUSE-sourced dictionaries read from. It is a single shared name,
// not one per test, because every test in this file that needs a loaded
// dictionary builds it, uses it, and tears it down before the next test
// function runs -- Go does not interleave non-parallel tests, and nothing
// here calls t.Parallel().
const asNamesSrcTable = "asnames_test_src"

// setupASNamesDictionary builds vantage_query_test.asnames as a
// CLICKHOUSE-sourced dictionary over a throwaway table carrying rows, forces
// it to LOAD synchronously, and registers its own teardown before returning.
//
// It is CLICKHOUSE-sourced rather than FILE-sourced on purpose. A Go test
// here runs on the host; the ClickHouse server this
// package's tests talk to reads deploy/dev/asnames/asn.tsv from INSIDE its
// own container's user_files_path, a path a host-side test has no way to
// populate. The code under test -- one dictGetOrDefault call per batch,
// classified by isNoASNamesDataset on failure -- is source-agnostic: a
// CLICKHOUSE-sourced dictionary exercises exactly the same path while
// staying entirely inside this test's own database. The FILE source and
// its container-mount plumbing were verified separately, twice, against
// real containers; this file proves the LOOKUP, not the file delivery.
//
// The CLICKHOUSE source's HOST/PORT ('localhost'/9000) name the server's OWN
// view of itself, not this test process's -- the dictionary source is read
// BY the ClickHouse server, from itself, which always listens on its native
// port 9000 inside its own container regardless of what host port
// docker-compose.dev.yml happens to publish that container's 9000 as (see
// that file's own comment on VANTAGE_CH_NATIVE_PORT). chtest.Addr(), by
// contrast, is this TEST PROCESS's own view through that possibly-remapped
// host port, and using it here would bind the wrong side of the mapping.
// USER/PASSWORD repeat chtest.DSN's own hard-coded vantage/vantage
// credentials -- the same account this whole package already trusts to
// drop and recreate its test database.
//
// buildASNamesDictionary does everything setupASNamesDictionary's own doc
// comment describes -- the source table, its rows, the dictionary itself,
// both teardowns -- but deliberately stops short of forcing a load.
// dictionaries_lazy_load defaults to true (asnames.sql's own comment), so
// the dictionary this leaves behind reads NOT_LOADED, exactly the state
// EVERY dictionary in this project is in immediately after a real
// ClickHouse restart, data and all. setupASNamesDictionary is the wrapper
// every OTHER
// test in this file wants, forcing a deterministic LOADED fixture;
// TestASNamesSelfHealsAfterARestartWithTheDataStillOnDisk calls this one
// directly because NOT_LOADED-with-data-present is precisely the
// precondition it exists to test.
func buildASNamesDictionary(t *testing.T, ctx context.Context, q *Q, rows map[uint32]ASName) {
	t.Helper()
	table := q.db + "." + asNamesSrcTable
	dict := q.db + ".asnames"

	// Defensive, not load-bearing under normal execution: every test below
	// registers its own teardown immediately after creating each object, so
	// these only matter if an earlier run of this same test binary panicked
	// or was killed before its own t.Cleanup ran.
	if err := q.conn.Exec(ctx, "DROP DICTIONARY IF EXISTS "+dict); err != nil {
		t.Fatalf("asnames fixture: drop stray dictionary: %v", err)
	}
	if err := q.conn.Exec(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
		t.Fatalf("asnames fixture: drop stray source table: %v", err)
	}

	if err := q.conn.Exec(ctx, "CREATE TABLE "+table+
		" (asn UInt32, name String, country String) ENGINE = Memory"); err != nil {
		t.Fatalf("asnames fixture: create source table: %v", err)
	}
	// context.WithoutCancel for the reason this package's other
	// t.Cleanup-plus-SYSTEM-statement fixtures already give (see
	// counts_test.go, events_test.go, history_test.go): t's own context is
	// already done by the time Cleanup runs, and a cleanup that silently
	// no-ops on a canceled context would leave this dictionary and table
	// behind for every later test in this run to trip over.
	t.Cleanup(func() {
		if err := q.conn.Exec(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+table); err != nil {
			t.Errorf("asnames fixture cleanup: drop source table: %v", err)
		}
	})

	if len(rows) > 0 {
		b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+table)
		if err != nil {
			t.Fatalf("asnames fixture: prepare insert: %v", err)
		}
		for asn, name := range rows {
			if err := b.Append(asn, name.Name, name.Country); err != nil {
				t.Fatalf("asnames fixture: append row for AS%d: %v", asn, err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("asnames fixture: send batch: %v", err)
		}
	}

	createDict := fmt.Sprintf(`CREATE DICTIONARY %s
(asn UInt32, name String, country String)
PRIMARY KEY asn
SOURCE(CLICKHOUSE(HOST 'localhost' PORT 9000 USER 'vantage' PASSWORD 'vantage' DB '%s' TABLE '%s'))
LAYOUT(HASHED())
LIFETIME(0)`, dict, q.db, asNamesSrcTable)
	if err := q.conn.Exec(ctx, createDict); err != nil {
		t.Fatalf("asnames fixture: create dictionary: %v", err)
	}
	t.Cleanup(func() {
		if err := q.conn.Exec(context.WithoutCancel(ctx), "DROP DICTIONARY IF EXISTS "+dict); err != nil {
			t.Errorf("asnames fixture cleanup: drop dictionary: %v", err)
		}
	})
}

// setupASNamesDictionary builds vantage_query_test.asnames as a
// CLICKHOUSE-sourced dictionary over a throwaway table carrying rows, forces
// it to LOAD synchronously, and registers its own teardown before returning.
//
// It is CLICKHOUSE-sourced rather than FILE-sourced on purpose. A Go test
// here runs on the host; the ClickHouse server this
// package's tests talk to reads deploy/dev/asnames/asn.tsv from INSIDE its
// own container's user_files_path, a path a host-side test has no way to
// populate. The code under test -- one dictGetOrDefault call per batch,
// classified by isNoASNamesDataset on failure -- is source-agnostic: a
// CLICKHOUSE-sourced dictionary exercises exactly the same path while
// staying entirely inside this test's own database. The FILE source and
// its container-mount plumbing were verified separately, twice, against
// real containers; this file proves the LOOKUP, not the file delivery.
//
// The CLICKHOUSE source's HOST/PORT ('localhost'/9000) name the server's OWN
// view of itself, not this test process's -- the dictionary source is read
// BY the ClickHouse server, from itself, which always listens on its native
// port 9000 inside its own container regardless of what host port
// docker-compose.dev.yml happens to publish that container's 9000 as (see
// that file's own comment on VANTAGE_CH_NATIVE_PORT). chtest.Addr(), by
// contrast, is this TEST PROCESS's own view through that possibly-remapped
// host port, and using it here would bind the wrong side of the mapping.
// USER/PASSWORD repeat chtest.DSN's own hard-coded vantage/vantage
// credentials -- the same account this whole package already trusts to
// drop and recreate its test database.
//
// SYSTEM RELOAD DICTIONARY, not a bare dictGet, is what forces the load:
// dictionaries_lazy_load defaults to true (asnames.sql's own comment), so a
// freshly created dictionary reads NOT_LOADED right up until something asks
// it a question. (*Q).ASNames itself would ALSO ask it that question --
// see isNoASNamesDataset's own doc comment on the lazy-load-on-first-use
// fix -- so this step is no longer load-bearing for getting a
// fixture to LOADED at all. It stays because forcing the load here,
// synchronously, before any test calls ASNames, is what keeps a fixture's
// OWN setup from silently depending on the exact self-healing behavior
// TestASNamesSelfHealsAfterARestartWithTheDataStillOnDisk exists to test
// on its own, deliberately, as the thing under test rather than as an
// incidental side effect of some other test's fixture setup.
func setupASNamesDictionary(t *testing.T, ctx context.Context, q *Q, rows map[uint32]ASName) {
	t.Helper()
	buildASNamesDictionary(t, ctx, q, rows)
	dict := q.db + ".asnames"
	if err := q.conn.Exec(ctx, "SYSTEM RELOAD DICTIONARY "+dict); err != nil {
		t.Fatalf("asnames fixture: force load: %v", err)
	}
}

// Addresses for this file's own fixtures are AS numbers, not router/peer
// IPs -- ASNames touches only the asnames dictionary and its own private
// source table (asNamesSrcTable), neither of which any other test in this
// package reads or writes, so none of the router/peer address-range
// bookkeeping the rest of this package's fixtures carry applies here. Every
// value below is drawn from the 32-bit private ASN range RFC 6996 reserves
// (4200000000-4294967294), so none of them can collide with a real,
// RIPE-registered holder if this test ever ran against production data by
// mistake.
const (
	distinguishListedASN     = 4200000001
	distinguishListedName    = "EXAMPLE-HOLDING - Example Holding Company, LLC"
	distinguishListedCountry = "US"
	// distinguishUnlistedASN never appears as a key in the fixture rows
	// setupASNamesDictionary inserts below -- it is state 2, a real ASN the
	// loaded dictionary has simply never heard of.
	distinguishUnlistedASN = 4200000002
	// distinguishNoDatasetASN is asked about before any dictionary exists
	// at all -- its value only has to differ from the other two so the
	// subtest cannot be satisfied by a name some OTHER subtest's fixture
	// happens to have loaded.
	distinguishNoDatasetASN = 4200000003
)

// TestASNamesDistinguishesUnlistedFromNoDataset is the whole point of this
// type's shape. Both subtests below ask for a name and get none back for
// the ASN they asked about -- and they are opposite facts, told apart only
// by the second return value: "no dataset" says vantage_query_test's own
// asnames dictionary object does not exist at all (a fresh clone before
// `make fetch-asnames`, or before the DDL was ever applied), and
// "unlisted" says the dictionary is right there, fully loaded, and this
// particular ASN is simply not one RIPE's list carries -- true of every ASN
// in a private-range test deployment's archive, since every one of them is
// private-range. A caller that read absence from the map alone, without
// also checking loaded, would report the first as the second: "this AS has
// no holder" when the truth is "we have never been told any AS's holder."
func TestASNamesDistinguishesUnlistedFromNoDataset(t *testing.T) {
	ctx := t.Context()

	t.Run("no dataset at all", func(t *testing.T) {
		q := requireQuery(t, ctx)
		// No setup at all: chtest applies only deploy/clickhouse/schema.sql
		// (chtest.go:99-109, by an explicit path, never a glob), so
		// deploy/clickhouse/asnames.sql never reaches vantage_query_test and
		// this database starts every test binary with no dictionary object
		// named asnames whatsoever -- the Code 36 BAD_ARGUMENTS path, and
		// the state a truly fresh clone's own database is already in.
		got, loaded, err := q.ASNames(ctx, []uint32{distinguishNoDatasetASN})
		if err != nil {
			t.Fatalf("ASNames: %v", err)
		}
		if loaded {
			t.Error("loaded = true, want false -- vantage_query_test has no asnames " +
				"dictionary object at all yet")
		}
		if len(got) != 0 {
			t.Errorf("got %d entries, want 0 when there is no dataset: %+v", len(got), got)
		}
	})

	t.Run("loaded, this ASN unlisted", func(t *testing.T) {
		q := requireQuery(t, ctx)
		setupASNamesDictionary(t, ctx, q, map[uint32]ASName{
			distinguishListedASN: {Name: distinguishListedName, Country: distinguishListedCountry},
		})

		got, loaded, err := q.ASNames(ctx, []uint32{distinguishListedASN, distinguishUnlistedASN})
		if err != nil {
			t.Fatalf("ASNames: %v", err)
		}
		if !loaded {
			t.Fatal("loaded = false, want true -- the dictionary was just built and " +
				"forced to LOADED; every assertion below rests on this")
		}
		want := ASName{Name: distinguishListedName, Country: distinguishListedCountry}
		if got, ok := got[distinguishListedASN]; !ok || got != want {
			t.Errorf("got[%d] = %+v, present=%v, want %+v, present=true",
				distinguishListedASN, got, ok, want)
		}
		if got, ok := got[distinguishUnlistedASN]; ok {
			t.Errorf("got[%d] = %+v, present=true, want ABSENT -- this ASN has no "+
				"registered holder, which is a different fact than the previous "+
				"subtest's 'no dataset at all', and the two must not read the same",
				distinguishUnlistedASN, got)
		}
	})
}

// selfHealASN is TestASNamesSelfHealsAfterARestartWithTheDataStillOnDisk's
// own fixture ASN -- distinct from every other constant in this file so a
// stray fixture left behind by a different, failed test cannot make this
// one pass for the wrong reason.
const (
	selfHealASN     = 4200000040
	selfHealName    = "EXAMPLE-RESTART - Example Restart Recovery, LLC"
	selfHealCountry = "US"
)

// TestASNamesSelfHealsAfterARestartWithTheDataStillOnDisk is the one test
// in this file that exists because of a defect, not a contract. It
// reproduces, at the Go-API layer, exactly the deployment-layer shape
// measured against a real ClickHouse restart: a dictionary object that
// exists, whose data is
// genuinely present and correct, but whose status reads NOT_LOADED because
// nothing has called dictGet on it since it was (re)created --
// dictionaries_lazy_load's own default, and the state EVERY dictionary in
// this project is left in by a plain ClickHouse restart, data and all.
//
// buildASNamesDictionary, not setupASNamesDictionary, is what makes this
// reproduction honest: it stops short of the SYSTEM RELOAD DICTIONARY step
// every other test in this file wants, so the dictionary this test calls
// ASNames against starts out genuinely NOT_LOADED, never forced.
//
// Previously, this exact scenario made (*Q).ASNames report
// loaded=false forever: its own system.dictionaries.status = 'LOADED'
// check read NOT_LOADED, correctly, and returned early -- never once
// calling dictGetOrDefault, which is the ONLY thing that could have moved
// status to LOADED. This test asserts the opposite: the very FIRST call,
// with no setup step forcing a load and no second call, must come back
// loaded=true with the real data, because ASNames itself now performs the
// "first use" ClickHouse's own lazy-load mechanism was always waiting for.
func TestASNamesSelfHealsAfterARestartWithTheDataStillOnDisk(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	buildASNamesDictionary(t, ctx, q, map[uint32]ASName{
		selfHealASN: {Name: selfHealName, Country: selfHealCountry},
	})

	// Precondition, asserted rather than assumed: this dictionary really is
	// NOT_LOADED going into the one and only ASNames call below. If this
	// ever read LOADED already, the rest of this test would be proving
	// nothing about the restart scenario it claims to reproduce.
	var status string
	if err := q.conn.QueryRow(ctx,
		"SELECT status FROM system.dictionaries WHERE database = ? AND name = 'asnames'",
		q.db).Scan(&status); err != nil {
		t.Fatalf("read dictionary status: %v", err)
	}
	if status != "NOT_LOADED" {
		t.Fatalf("precondition failed: status = %s, want NOT_LOADED -- buildASNamesDictionary "+
			"was supposed to leave this dictionary unloaded", status)
	}

	got, loaded, err := q.ASNames(ctx, []uint32{selfHealASN})
	if err != nil {
		t.Fatalf("ASNames: %v", err)
	}
	if !loaded {
		t.Fatal("loaded = false, want true -- the dictionary's data is present and correct; " +
			"a single ASNames call must be able to load it itself, the same way a real request " +
			"after a ClickHouse restart has to, with no SYSTEM RELOAD DICTIONARY run in between")
	}
	want := ASName{Name: selfHealName, Country: selfHealCountry}
	if got, ok := got[selfHealASN]; !ok || got != want {
		t.Errorf("got[%d] = %+v, present=%v, want %+v, present=true", selfHealASN, got, ok, want)
	}

	// And the self-heal is not a one-shot fluke: the dictionary this
	// call's own dictGetOrDefault loaded is the SAME object every later
	// request reaches, so it must now read LOADED for good, the same way a
	// real deployment's dictionary stays LOADED after the first request
	// following a restart.
	if err := q.conn.QueryRow(ctx,
		"SELECT status FROM system.dictionaries WHERE database = ? AND name = 'asnames'",
		q.db).Scan(&status); err != nil {
		t.Fatalf("read dictionary status after self-heal: %v", err)
	}
	if status != "LOADED" {
		t.Errorf("status after ASNames call = %s, want LOADED -- the call above was supposed "+
			"to be ClickHouse's own \"first use\" and leave the dictionary loaded", status)
	}
}

// TestASNamesAnswersLoadedForAnEmptyBatch covers the probe this method's
// own doc comment describes: an empty asns argument still has to answer
// `loaded` honestly, which now costs a real dictGetOrDefault call bound
// against a single sentinel (AS 0) rather than skipping the round trip
// entirely -- there is no other way to learn whether the dictionary is
// loaded without calling something that would trigger ClickHouse's own
// lazy-load-on-first-use, and an empty bound array never evaluates
// dictGetOrDefault at all (arrayJoin of nothing produces no rows). Both
// subtests assert the RETURNED MAP stays empty regardless of what the
// probe found about AS 0 itself -- a caller who asked about nothing must
// never see AS 0 appear in its answer.
func TestASNamesAnswersLoadedForAnEmptyBatch(t *testing.T) {
	ctx := t.Context()

	t.Run("no dataset at all", func(t *testing.T) {
		q := requireQuery(t, ctx)
		got, loaded, err := q.ASNames(ctx, nil)
		if err != nil {
			t.Fatalf("ASNames: %v", err)
		}
		if loaded {
			t.Error("loaded = true, want false -- vantage_query_test has no asnames " +
				"dictionary object at all yet")
		}
		if len(got) != 0 {
			t.Errorf("got %d entries, want 0: %+v", len(got), got)
		}
	})

	t.Run("loaded", func(t *testing.T) {
		q := requireQuery(t, ctx)
		setupASNamesDictionary(t, ctx, q, map[uint32]ASName{
			selfHealASN: {Name: selfHealName, Country: selfHealCountry},
		})
		got, loaded, err := q.ASNames(ctx, nil)
		if err != nil {
			t.Fatalf("ASNames: %v", err)
		}
		if !loaded {
			t.Fatal("loaded = false, want true -- the dictionary was just built and forced " +
				"to LOADED")
		}
		if len(got) != 0 {
			t.Errorf("got %d entries, want 0 -- asns was empty, so the map must stay empty "+
				"even though the probe this method issues internally succeeded: %+v", len(got), got)
		}
	})
}

// wholeNameASN and wholeNameText are TestASNamesReturnsTheWholeNameNeverASplit's
// own fixture. wholeNameText deliberately carries TWO " - " separators and a
// comma inside the trailing segment, so a caller that split on the first
// " - ", the last one, or on a comma would each produce a different, equally
// wrong truncation. See (*Q).ASNames' own doc comment:
// 31.8% of RIPE's real lines carry no " - " at all and 1,530 carry more than
// one, so any split here is a guess wearing a parse's clothes.
const (
	wholeNameASN     = 4200000010
	wholeNameText    = "EXAMPLE-HOLDCO - Example Holding Co - Legal Successor, Inc."
	wholeNameCountry = "US"
)

// TestASNamesReturnsTheWholeNameNeverASplit pins the decision: the
// stored name is everything between the ASN and the country code, separator
// and all, with nothing parsed back out of it a second time on the read
// side.
func TestASNamesReturnsTheWholeNameNeverASplit(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	setupASNamesDictionary(t, ctx, q, map[uint32]ASName{
		wholeNameASN: {Name: wholeNameText, Country: wholeNameCountry},
	})

	got, loaded, err := q.ASNames(ctx, []uint32{wholeNameASN})
	if err != nil {
		t.Fatalf("ASNames: %v", err)
	}
	if !loaded {
		t.Fatal("loaded = false, want true")
	}
	name, ok := got[wholeNameASN]
	if !ok {
		t.Fatalf("got[%d] absent, want present", wholeNameASN)
	}
	if name.Name != wholeNameText {
		t.Errorf("Name = %q, want %q UNCHANGED -- a name with more than one "+
			"\" - \" must come back whole, not truncated at either one",
			name.Name, wholeNameText)
	}
	if name.Country != wholeNameCountry {
		t.Errorf("Country = %q, want %q", name.Country, wholeNameCountry)
	}
}

// countingConn wraps a real driver.Conn and counts calls to Query -- never
// QueryRow, Exec or PrepareBatch -- so a test can prove a method issued
// exactly one statement of the kind ASNames' own batch lookup is, without
// depending on ClickHouse's own query_log (which flushes on its own
// schedule and would make such a proof flaky rather than deterministic).
// Embedding driver.Conn rather than hand-implementing every method is what
// that interface's own doc comment recommends for exactly this shape of
// decorator -- see driver.Conn's "Compatibility" note -- and it is the same
// reason stubConn in query_test.go embeds rather than implements.
type countingConn struct {
	driver.Conn
	queries int
}

func (c *countingConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	c.queries++
	return c.Conn.Query(ctx, query, args...)
}

// oncePerBatchListedASN is the one ASN TestASNamesAsksTheDictionaryOncePerBatch's
// fixture lists; every other ASN in its batch is unlisted, which does not
// matter to this test -- it counts STATEMENTS, not names.
const oncePerBatchListedASN = 4200000020

// TestASNamesAsksTheDictionaryOncePerBatch: one dictGetOrDefault per ASN
// inside one statement, not one statement per ASN. A screen showing 40
// ASNs must cost one query, and this asserts exactly that -- the query
// count stays 1 whether the batch asked about is 1 ASN or 25, which is the
// property a loop issuing one statement per ASN would not have.
func TestASNamesAsksTheDictionaryOncePerBatch(t *testing.T) {
	ctx := t.Context()
	realQ := requireQuery(t, ctx)
	setupASNamesDictionary(t, ctx, realQ, map[uint32]ASName{
		oncePerBatchListedASN: {Name: "EXAMPLE-BATCH - Example Batch Holder, LLC", Country: "US"},
	})

	for _, n := range []int{1, 25} {
		asns := make([]uint32, n)
		for i := range asns {
			// oncePerBatchListedASN plus i-1 distinct unlisted neighbors --
			// none of these needs to resolve to a real name; this test
			// reads the query COUNT, not the answer.
			asns[i] = oncePerBatchListedASN + uint32(i)
		}

		counting := &countingConn{Conn: realQ.conn}
		q, err := New(counting, realQ.db)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		if _, loaded, err := q.ASNames(ctx, asns); err != nil {
			t.Fatalf("ASNames(%d ASNs): %v", n, err)
		} else if !loaded {
			t.Fatalf("ASNames(%d ASNs): loaded = false, want true", n)
		}

		if counting.queries != 1 {
			t.Errorf("ASNames(%d ASNs) issued %d Query call(s), want exactly 1 -- "+
				"a screen showing many ASNs must cost one statement, not one per ASN",
				n, counting.queries)
		}
	}
}

// missingFileDictPath is a path INSIDE ClickHouse's own user_files_path
// (confirmed against this stack's config.xml, and named the same way
// asnames.sql's own real FILE source is) that simply does not exist. A
// path outside user_files_path is rejected at CREATE DICTIONARY time on
// this ClickHouse version (Code 481, per asnames.sql's own comment) rather
// than producing the NOT_LOADED state this test needs, so the directory
// component has to be a real, if empty-of-this-file, location under it.
const missingFileDictPath = "/var/lib/clickhouse/user_files/asnames_test_missing_file/does_not_exist.tsv"

// TestASNamesReportsNoDatasetWhenTheDictionaryFileIsMissing is the OTHER
// half of state 3. A dictionary object can exist in system.dictionaries
// with no data behind it for two different reasons: the object itself was
// never created (TestASNamesDistinguishesUnlistedFromNoDataset's
// "no dataset at all" subtest covers that half, for free, since chtest
// never applies asnames.sql to this database at all), or the object exists
// but its source file does not -- the actual "fresh clone, DDL applied,
// nobody has run `make fetch-asnames` yet" shape a real deployment reaches.
// Both must report loaded=false to a caller, and this test is what proves
// the SECOND half does, not just the first: a naive dictGetOrDefault call
// against this dictionary raises Code 107 FILE_DOESNT_EXIST rather than
// returning a default, and this test proves ASNames' own
// isNoASNamesDataset classification (query/asnames.go) turns that raised
// error back into the same loaded=false, no-error answer the "object does
// not exist at all" half already gets, rather than surfacing ClickHouse's
// own error code to this package's caller.
func TestASNamesReportsNoDatasetWhenTheDictionaryFileIsMissing(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	dict := q.db + ".asnames"

	if err := q.conn.Exec(ctx, "DROP DICTIONARY IF EXISTS "+dict); err != nil {
		t.Fatalf("drop stray dictionary: %v", err)
	}
	createDict := fmt.Sprintf(`CREATE DICTIONARY %s
(asn UInt32, name String, country String)
PRIMARY KEY asn
SOURCE(FILE(path '%s' format 'TSV'))
LAYOUT(HASHED())
LIFETIME(0)`, dict, missingFileDictPath)
	if err := q.conn.Exec(ctx, createDict); err != nil {
		t.Fatalf("create FILE-sourced dictionary: %v", err)
	}
	t.Cleanup(func() {
		if err := q.conn.Exec(context.WithoutCancel(ctx), "DROP DICTIONARY IF EXISTS "+dict); err != nil {
			t.Errorf("cleanup: drop dictionary: %v", err)
		}
	})

	// Precondition, asserted rather than assumed: this dictionary's row
	// really is in the middle state -- present, not LOADED -- because its
	// file genuinely does not exist. If this ever read LOADED, the rest of
	// this test would be proving nothing about the case it claims to.
	var status string
	if err := q.conn.QueryRow(ctx,
		"SELECT status FROM system.dictionaries WHERE database = ? AND name = 'asnames'",
		q.db).Scan(&status); err != nil {
		t.Fatalf("read dictionary status: %v", err)
	}
	if status == "LOADED" {
		t.Fatalf("precondition failed: status = LOADED, want NOT_LOADED -- %s was not "+
			"supposed to exist", missingFileDictPath)
	}

	got, loaded, err := q.ASNames(ctx, []uint32{4200000030})
	if err != nil {
		t.Fatalf("ASNames returned an error: %v -- the missing-file half of state 3 must "+
			"report loaded=false, not surface ClickHouse's own Code 107 FILE_DOESNT_EXIST "+
			"to this package's caller", err)
	}
	if loaded {
		t.Error("loaded = true, want false -- this dictionary's row exists but its source " +
			"file does not, which must report the SAME 'no dataset' fact as the object not " +
			"existing at all, not a third state a caller has to handle differently")
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0: %+v", len(got), got)
	}
}
