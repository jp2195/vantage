// Handler tests for GET /v1/asnames. See api/handlers_test.go's own header
// for why these run against a live ClickHouse rather than a mocked driver.
//
// requireAPI(t) alone gives the DEFAULT state this endpoint must degrade
// to: apiTestDB carries no asnames (or asnames_meta) dictionary object at
// all, the same "chtest applies only schema.sql" fact query/asnames_test.go
// relies on for its own "no dataset at all" subtest. The tests that need a
// LOADED dataset build one themselves, via setupASNamesDictionaries below,
// and tear it down before returning -- so a test that runs before or after
// them, in either order, still sees the default state unless it built its
// own.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// The fixture's own coordinates, drawn from RFC 6996's 32-bit private ASN
// range (4200000000-4294967294) for the identical reason
// query/asnames_test.go's own distinguish* constants are: none of them can
// collide with a real, RIPE-registered holder if this ever ran against
// production data by mistake.
const (
	asNamesFixListedASN     = 4200000101
	asNamesFixListedName    = "EXAMPLE-API - Example API Holding Company, LLC"
	asNamesFixListedCountry = "US"
	asNamesFixUnlistedASN   = 4200000102

	// asNamesFixPublished is a real Last-Modified value, in the exact form
	// curl -D captures live, so the parsing half of this file's tests
	// exercises a realistic value rather than a hand-simplified one.
	asNamesFixPublished = "Fri, 18 Sep 2026 10:49:00 GMT"
)

// asNamesFixPublishedTime is asNamesFixPublished, parsed the same way
// api/asnames.go itself parses it (http.ParseTime), so a test comparing
// against it is pinned to the SAME layout the handler uses rather than a
// second, hand-written one that could quietly drift from it.
var asNamesFixPublishedTime = func() time.Time {
	t, err := http.ParseTime(asNamesFixPublished)
	if err != nil {
		panic("asNamesFixPublished does not parse as an HTTP date: " + err.Error())
	}
	return t
}()

// setupASNamesDictionaries builds vantage_api_test.asnames -- CLICKHOUSE-
// sourced over a throwaway table carrying exactly one listed row
// (asNamesFixListedASN) -- and, when withDate is true, the companion
// vantage_api_test.asnames_meta carrying asNamesFixPublished. Both are torn
// down via t.Cleanup before this test function returns.
//
// CLICKHOUSE-sourced rather than FILE-sourced for the reason
// query/asnames_test.go's own setupASNamesDictionary gives: this test
// runs on the host, and the ClickHouse
// server reads a FILE source from inside its own container, a path this
// process cannot place a fixture at. The code under test here -- the HTTP
// handler on top of query.(*Q).ASNames and ASNamesPublished -- never looks
// at how either dictionary is sourced, so a CLICKHOUSE-sourced one
// exercises the identical statements query/'s own tests already prove
// against the FILE-sourced form verified separately against real
// containers.
func setupASNamesDictionaries(t *testing.T, ctx context.Context, withDate bool) {
	t.Helper()
	conn := chtest.Require(t, ctx, apiTestDB)

	const srcTable = apiTestDB + ".asnames_test_src"
	dict := apiTestDB + ".asnames"
	if err := conn.Exec(ctx, "DROP DICTIONARY IF EXISTS "+dict); err != nil {
		t.Fatalf("asnames fixture: drop stray dictionary: %v", err)
	}
	if err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+srcTable); err != nil {
		t.Fatalf("asnames fixture: drop stray source table: %v", err)
	}
	if err := conn.Exec(ctx, "CREATE TABLE "+srcTable+
		" (asn UInt32, name String, country String) ENGINE = Memory"); err != nil {
		t.Fatalf("asnames fixture: create source table: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Exec(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+srcTable); err != nil {
			t.Errorf("asnames fixture cleanup: drop source table: %v", err)
		}
	})
	if err := conn.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s (asn, name, country) VALUES (?, ?, ?)", srcTable),
		uint32(asNamesFixListedASN), asNamesFixListedName, asNamesFixListedCountry); err != nil {
		t.Fatalf("asnames fixture: insert listed row: %v", err)
	}
	createDict := fmt.Sprintf(`CREATE DICTIONARY %s
(asn UInt32, name String, country String)
PRIMARY KEY asn
SOURCE(CLICKHOUSE(HOST 'localhost' PORT 9000 USER 'vantage' PASSWORD 'vantage' DB '%s' TABLE 'asnames_test_src'))
LAYOUT(HASHED())
LIFETIME(0)`, dict, apiTestDB)
	if err := conn.Exec(ctx, createDict); err != nil {
		t.Fatalf("asnames fixture: create dictionary: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Exec(context.WithoutCancel(ctx), "DROP DICTIONARY IF EXISTS "+dict); err != nil {
			t.Errorf("asnames fixture cleanup: drop dictionary: %v", err)
		}
	})
	if err := conn.Exec(ctx, "SYSTEM RELOAD DICTIONARY "+dict); err != nil {
		t.Fatalf("asnames fixture: force load: %v", err)
	}

	if !withDate {
		return
	}

	const metaSrcTable = apiTestDB + ".asnames_meta_test_src"
	metaDict := apiTestDB + ".asnames_meta"
	if err := conn.Exec(ctx, "DROP DICTIONARY IF EXISTS "+metaDict); err != nil {
		t.Fatalf("asnames_meta fixture: drop stray dictionary: %v", err)
	}
	if err := conn.Exec(ctx, "DROP TABLE IF EXISTS "+metaSrcTable); err != nil {
		t.Fatalf("asnames_meta fixture: drop stray source table: %v", err)
	}
	if err := conn.Exec(ctx, "CREATE TABLE "+metaSrcTable+
		" (id UInt8, last_modified String) ENGINE = Memory"); err != nil {
		t.Fatalf("asnames_meta fixture: create source table: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Exec(context.WithoutCancel(ctx), "DROP TABLE IF EXISTS "+metaSrcTable); err != nil {
			t.Errorf("asnames_meta fixture cleanup: drop source table: %v", err)
		}
	})
	if err := conn.Exec(ctx,
		fmt.Sprintf("INSERT INTO %s (id, last_modified) VALUES (0, ?)", metaSrcTable),
		asNamesFixPublished); err != nil {
		t.Fatalf("asnames_meta fixture: insert row: %v", err)
	}
	createMetaDict := fmt.Sprintf(`CREATE DICTIONARY %s
(id UInt8, last_modified String)
PRIMARY KEY id
SOURCE(CLICKHOUSE(HOST 'localhost' PORT 9000 USER 'vantage' PASSWORD 'vantage' DB '%s' TABLE 'asnames_meta_test_src'))
LAYOUT(HASHED())
LIFETIME(0)`, metaDict, apiTestDB)
	if err := conn.Exec(ctx, createMetaDict); err != nil {
		t.Fatalf("asnames_meta fixture: create dictionary: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Exec(context.WithoutCancel(ctx), "DROP DICTIONARY IF EXISTS "+metaDict); err != nil {
			t.Errorf("asnames_meta fixture cleanup: drop dictionary: %v", err)
		}
	})
	if err := conn.Exec(ctx, "SYSTEM RELOAD DICTIONARY "+metaDict); err != nil {
		t.Fatalf("asnames_meta fixture: force load: %v", err)
	}
}

// TestASNamesRequiresAtLeastOneASN is the 400-not-a-dump requirement: a
// request naming no asn= must not answer with every one of the dataset's
// 122,442 real rows (or, here, whatever this test database happens to
// hold).
func TestASNamesRequiresAtLeastOneASN(t *testing.T) {
	s := requireAPI(t)
	msg := requireError(t, s, "/v1/asnames", http.StatusBadRequest, ErrInvalidParam)
	if !strings.Contains(msg, "asn=") {
		t.Errorf("message %q does not name the missing parameter", msg)
	}
}

// TestASNamesRejectsABatchPastTheCap is the other 400: a batch naming more
// than maxASNBatch ASNs is refused rather than silently truncated, and the
// body names the bound so a caller can act on it.
func TestASNamesRejectsABatchPastTheCap(t *testing.T) {
	s := requireAPI(t)
	q := url.Values{}
	for i := 1; i <= maxASNBatch+1; i++ {
		q.Add("asn", strconv.Itoa(i))
	}
	msg := requireError(t, s, "/v1/asnames?"+q.Encode(), http.StatusBadRequest, ErrInvalidParam)
	if !strings.Contains(msg, strconv.Itoa(maxASNBatch)) {
		t.Errorf("message %q does not name the bound %d", msg, maxASNBatch)
	}
}

// TestASNamesRejectsMoreThanTheCapEvenAsRepeatsOfOneValue pins the OTHER
// half of the cap's own doc comment: it is checked against the RAW number
// of asn= occurrences, before dedup, not the number of DISTINCT values --
// a request repeating one ASN 513 times costs the same to receive and
// parse as one naming 513 different ones, and both are refused.
//
// Without this test, (*params).asns' cap check (`len(raw) > max`, BEFORE
// the dedup loop) and TestASNamesRejectsABatchPastTheCap /
// TestASNamesAcceptsExactlyTheCap (which both use exclusively distinct
// values) cannot tell that check apart from one written against `len(seen)`
// AFTER dedup -- moving it there silently reverses this file's own
// documented decision while every other test here stays green (confirmed
// below, and by mutation).
func TestASNamesRejectsMoreThanTheCapEvenAsRepeatsOfOneValue(t *testing.T) {
	s := requireAPI(t)
	q := url.Values{}
	for i := 0; i < maxASNBatch+1; i++ {
		q.Add("asn", strconv.Itoa(asNamesFixListedASN))
	}
	msg := requireError(t, s, "/v1/asnames?"+q.Encode(), http.StatusBadRequest, ErrInvalidParam)
	if !strings.Contains(msg, strconv.Itoa(maxASNBatch)) {
		t.Errorf("message %q does not name the bound %d", msg, maxASNBatch)
	}
}

// TestASNamesAcceptsExactlyTheCap is the boundary TestASNamesRejectsABatchPastTheCap
// alone cannot pin: that test only proves max+1 is refused, which an
// off-by-one (`>=` for `>`) would still satisfy while ALSO refusing a
// request naming exactly max -- a caller doing nothing wrong. Both ends of
// the boundary have to be checked, or a mutation moving it by one passes
// every test in this file untouched (confirmed: `len(raw) >= max` in
// (*params).asns leaves every other test here green, and only this one
// catches it).
func TestASNamesAcceptsExactlyTheCap(t *testing.T) {
	s := requireAPI(t)
	q := url.Values{}
	for i := 1; i <= maxASNBatch; i++ {
		q.Add("asn", strconv.Itoa(i))
	}
	var got []WireASName
	getOK(t, s, "/v1/asnames?"+q.Encode(), &got)
	if len(got) != maxASNBatch {
		t.Fatalf("got %d rows for a batch of exactly %d, want %d", len(got), maxASNBatch, maxASNBatch)
	}
}

// TestASNamesRejectsAMalformedValueWithoutEchoingIt pins two things at
// once: a non-numeric asn= is a 400, and the body does not quote it back --
// api/handlers.go's own header rule, that no 400 echoes caller-supplied
// text, applies here exactly as it does to every parameter this package
// parses.
func TestASNamesRejectsAMalformedValueWithoutEchoingIt(t *testing.T) {
	s := requireAPI(t)
	const bogus = "not-an-as-number"
	msg := requireError(t, s, "/v1/asnames?asn="+bogus, http.StatusBadRequest, ErrInvalidParam)
	if strings.Contains(msg, bogus) {
		t.Errorf("400 body echoed the caller-supplied value: %q", msg)
	}
}

// TestASNamesReportsNoDatasetByDefault is state 3: apiTestDB carries no
// asnames dictionary at all (chtest applies only schema.sql), and the
// answer must say so explicitly -- loaded=false, the publication date
// ABSENT -- while still naming the ASN that was asked about, with an
// explicitly empty name rather than an absent row.
//
// The two meta fields are deliberately different shapes and this test
// pins both. asnames_loaded is always sent, because "is there a dataset"
// always has an answer. asnames_published carries omitempty, so its
// absence IS the no-date answer and it never appears as null -- which is
// why the assertion below reads the RAW WIRE BYTES rather than the
// decoded pointer: a *time.Time decodes to nil from an absent key exactly
// as readily as from a present null, so a nil check alone cannot tell the
// shape this test names from the shape it forbids. Dropping the omitempty
// in api/types.go is the mutation this catches, and it is a real risk:
// the contract's two genuinely-nullable neighbors (next_cursor,
// total_matched) have no omitempty, so the field looks inconsistent until
// you read why.
func TestASNamesReportsNoDatasetByDefault(t *testing.T) {
	s := requireAPI(t)
	rec := get(t, s, fmt.Sprintf("/v1/asnames?asn=%d", asNamesFixListedASN))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/asnames = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	var env struct {
		Data []WireASName `json:"data"`
		Meta Meta         `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body %s", err, body)
	}
	got, meta := env.Data, env.Meta

	if meta.ASNamesLoaded == nil {
		t.Fatal("meta.asnames_loaded is absent, want present (false)")
	}
	if *meta.ASNamesLoaded {
		t.Error("meta.asnames_loaded = true, want false -- this test database has no " +
			"asnames dictionary object at all")
	}
	// asnames_loaded IS always sent here, so its key must be in the bytes:
	// this is the half that would catch it wrongly acquiring an omitempty
	// and vanishing on exactly the false value that matters most.
	if !strings.Contains(body, `"asnames_loaded":false`) {
		t.Errorf("response does not carry asnames_loaded:false; it must always be sent "+
			"on this path, and false is the value a caller most needs: %s", body)
	}
	if meta.ASNamesPublished != nil {
		t.Errorf("meta.asnames_published = %v, want nil when no dataset is loaded",
			*meta.ASNamesPublished)
	}
	if strings.Contains(body, "asnames_published") {
		t.Errorf("response carries an asnames_published key with no dataset loaded; the "+
			"contract says the key is ABSENT here, never present-and-null, and a "+
			"caller reading the key's presence would be misled: %s", body)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want exactly 1 (one per requested ASN): %+v", len(got), got)
	}
	if got[0].ASN != asNamesFixListedASN {
		t.Errorf("data[0].asn = %d, want %d", got[0].ASN, asNamesFixListedASN)
	}
	if got[0].Name != "" || got[0].Country != "" {
		t.Errorf("data[0] = %+v, want an explicitly empty name and country -- there is "+
			"no dataset to have listed this ASN under", got[0])
	}
}

// TestASNamesDistinguishesListedFromUnlistedOnTheWire is state 1 versus
// state 2, both in the SAME response: a loaded dataset answers a real name
// for the ASN it lists and an explicit, equally present empty name for the
// one it does not -- the two must not be told apart by presence in data,
// only by their own name/country fields, with meta.asnames_loaded true for
// both. It also pins meta.asnames_published to the fixture's own date,
// parsed.
func TestASNamesDistinguishesListedFromUnlistedOnTheWire(t *testing.T) {
	s := requireAPI(t)
	setupASNamesDictionaries(t, t.Context(), true)

	var got []WireASName
	meta := getOK(t, s, fmt.Sprintf("/v1/asnames?asn=%d&asn=%d",
		asNamesFixListedASN, asNamesFixUnlistedASN), &got)

	if meta.ASNamesLoaded == nil || !*meta.ASNamesLoaded {
		t.Fatalf("meta.asnames_loaded = %v, want true -- the dictionary was just built "+
			"and forced to LOADED", meta.ASNamesLoaded)
	}
	if meta.ASNamesPublished == nil {
		t.Fatal("meta.asnames_published is nil, want the fixture's own date")
	}
	if !meta.ASNamesPublished.Equal(asNamesFixPublishedTime) {
		t.Errorf("meta.asnames_published = %v, want %v", *meta.ASNamesPublished, asNamesFixPublishedTime)
	}

	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	byASN := map[uint32]WireASName{}
	for _, row := range got {
		byASN[row.ASN] = row
	}
	listed, ok := byASN[asNamesFixListedASN]
	if !ok {
		t.Fatalf("no row for the listed ASN %d: %+v", asNamesFixListedASN, got)
	}
	if listed.Name != asNamesFixListedName || listed.Country != asNamesFixListedCountry {
		t.Errorf("listed row = %+v, want name %q country %q",
			listed, asNamesFixListedName, asNamesFixListedCountry)
	}
	unlisted, ok := byASN[asNamesFixUnlistedASN]
	if !ok {
		t.Fatalf("no row for the unlisted ASN %d: %+v -- it must still appear, "+
			"explicitly empty, not be missing in a way that reads as \"not asked about\"",
			asNamesFixUnlistedASN, got)
	}
	if unlisted.Name != "" || unlisted.Country != "" {
		t.Errorf("unlisted row = %+v, want an explicitly empty name and country", unlisted)
	}
}

// TestASNamesPublishedIsNullWhenTheCompanionDictionaryIsMissing proves the
// two dictionaries are checked independently: the NAMES dataset can be
// loaded while its DATE companion is not (an ordinary, if unusual,
// deployment shape -- see query.(*Q).ASNamesPublished's own doc comment),
// and that must read as loaded=true, published=null rather than either
// dictionary's state leaking into the other's answer.
func TestASNamesPublishedIsNullWhenTheCompanionDictionaryIsMissing(t *testing.T) {
	s := requireAPI(t)
	setupASNamesDictionaries(t, t.Context(), false) // withDate = false: no asnames_meta

	var got []WireASName
	meta := getOK(t, s, fmt.Sprintf("/v1/asnames?asn=%d", asNamesFixListedASN), &got)

	if meta.ASNamesLoaded == nil || !*meta.ASNamesLoaded {
		t.Fatalf("meta.asnames_loaded = %v, want true", meta.ASNamesLoaded)
	}
	if meta.ASNamesPublished != nil {
		t.Errorf("meta.asnames_published = %v, want nil -- no asnames_meta dictionary "+
			"was built for this test", *meta.ASNamesPublished)
	}
	if len(got) != 1 || got[0].Name != asNamesFixListedName {
		t.Errorf("got %+v, want the listed row's real name regardless of the missing date", got)
	}
}

// TestASNamesDedupesRepeatedValues: a caller naming the same ASN twice gets
// one row back, not two -- the response answers each ASN once. The OTHER
// half of this interaction -- that the raw count, repeats included, still
// counts against the cap -- is TestASNamesRejectsMoreThanTheCapEvenAsRepeatsOfOneValue's
// job, not this one's: this test alone cannot tell "cap checked before
// dedup" apart from "cap checked after," since it never sends more than
// the cap either way.
func TestASNamesDedupesRepeatedValues(t *testing.T) {
	s := requireAPI(t)
	var got []WireASName
	getOK(t, s, fmt.Sprintf("/v1/asnames?asn=%d&asn=%d&asn=%d",
		asNamesFixListedASN, asNamesFixListedASN, asNamesFixUnlistedASN), &got)
	if len(got) != 2 {
		t.Fatalf("got %d rows for [X, X, Y], want 2 (deduplicated): %+v", len(got), got)
	}
	if got[0].ASN != asNamesFixListedASN || got[1].ASN != asNamesFixUnlistedASN {
		t.Errorf("got %+v, want first-seen order [%d, %d]",
			got, asNamesFixListedASN, asNamesFixUnlistedASN)
	}
}
