package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// ASName is one AS number's registered holder, exactly as RIPE's ASN list
// carries it. Name is the WHOLE line between the ASN and its trailing
// country code, never split into a handle and an organization -- see
// (*Q).ASNames for why that split cannot be done without guessing, and this
// package's rule for looked-up data is that it never guesses. Country is
// the two-letter code that followed it.
//
// ASName is never the zero value for an ASN this package reports as
// listed: see ASNames' own doc comment for what an absent ASN means
// instead, which is not "an ASName with empty fields."
type ASName struct {
	Name    string
	Country string
}

// The two ClickHouse error codes dictGetOrDefault raises instead of quietly
// returning its own default value, when there is nothing behind a
// dictionary yet -- measured directly against this project's own image
// (24.8.14), and re-confirmed through the exact
// clickhouse-go/v2 driver this package uses (clickhouse.Exception's own doc
// comment: "Use Code for branching", which is why these are plain int32s
// rather than a type pulled in from ClickHouse's own error-code package).
//
//   - chCodeDictionaryNotFound (36, ClickHouse's BAD_ARGUMENTS): the
//     dictionary OBJECT itself was never created -- a stack whose DDL
//     (deploy/clickhouse/asnames.sql or asnames_meta.sql) was never
//     applied.
//   - chCodeSourceFileMissing (107, ClickHouse's FILE_DOESNT_EXIST): the
//     object exists, but its FILE source is absent -- the ordinary "DDL
//     applied, `make fetch-asnames` never run" shape, and -- see
//     isNoASNamesDataset's own doc comment -- also the shape EVERY
//     dictionary is in immediately after a ClickHouse restart, even with
//     its data fully intact on disk.
const (
	chCodeDictionaryNotFound int32 = 36
	chCodeSourceFileMissing  int32 = 107
)

// isNoASNamesDataset reports whether err is one of the two ordinary,
// expected "nothing is behind this dictionary yet" failures a
// dictGetOrDefault call raises -- see the two codes above -- rather than a
// real failure a caller needs to see. Any other error (ClickHouse
// unreachable, a malformed source row, a permissions problem) is left
// alone, for the caller to treat as the genuine failure it is.
//
// THIS FIXES A DEFECT confirmed straight from this image's config.xml
// (dictionaries_lazy_load=true, its documented default): a ClickHouse
// dictionary loads on first USE, not at server startup -- and "use" means
// something actually calling dictGet/dictGetOrDefault on it. Previously,
// ASNames and ASNamesPublished both checked
// system.dictionaries.status = 'LOADED' BEFORE ever calling
// dictGetOrDefault, precisely so a missing dataset would read as "not
// loaded" instead of raising one of the two codes above to an unsuspecting
// caller. That gate was correct on its own and fatal combined with
// ClickHouse's own load-on-first-use contract: a status check that itself
// never calls dictGet can never BE the first use that flips NOT_LOADED to
// LOADED. Once a ClickHouse restart reset every dictionary back to
// NOT_LOADED -- confirmed directly: correctly parsed, correctly mounted
// data on disk, dictionaries LOADED, then a
// plain `docker restart` with no config change and nothing else touched,
// dictionaries NOT_LOADED again -- nothing in the ordinary request path
// could ever load them again. Every screen read "no name dataset is
// loaded" forever, until a human ran SYSTEM RELOAD DICTIONARY by hand.
//
// Calling dictGetOrDefault UNCONDITIONALLY and classifying its own two
// "nothing here yet" codes here, instead of asking first, is what lets
// ClickHouse's own lazy-load-on-first-use mechanism actually run: the
// first real request after a restart becomes that first use. If the data
// is on disk, that request's own dictGetOrDefault call succeeds, returns
// the real name, and leaves the dictionary LOADED for every request after
// it -- verified directly, end to end, against a dictionary a restart had
// just reset to NOT_LOADED with its data still mounted: the very first
// dictGetOrDefault call returned the correct name AND flipped status to
// LOADED, no restart, no reload, no human. No deployment change is needed
// for this to work, because it already works today, unchanged --
// dictionaries_lazy_load stays true (its existing default) in both
// docker-compose.dev.yml and the Helm chart. See the paragraph below for
// the deployment-side fix that was tried FIRST and rejected.
//
// Setting dictionaries_lazy_load=false (loading every dictionary eagerly
// at server startup, so status would be trustworthy immediately, with no
// Go code change at all) was the preferred fix at first, tried and
// rejected before this function was written. Gate result, measured
// directly: with no data file present -- this feature's own default,
// documented state, true of every fresh clone before `make fetch-asnames`
// runs -- CREATE DICTIONARY itself becomes an eager, synchronous load
// attempt and RAISES (the identical Code 107) instead of landing
// NOT_LOADED the way it does today. Both deployment paths apply this
// project's dictionary DDL unconditionally, before the data is known to
// exist (job-schema.yaml on every Helm cluster; docker-entrypoint-initdb.d
// on a fresh dev-stack volume) -- with eager loading, that DDL apply
// itself fails, and the official ClickHouse image's own
// docker-entrypoint-initdb.d loop has no error handling around it (no `||
// true`, no continue-on-error), so under `set -e` THE ENTIRE CONTAINER
// exits. Confirmed directly: a fresh ClickHouse volume, this project's own
// asnames.sql applied through docker-entrypoint-initdb.d exactly as
// docker-compose.dev.yml already does, dictionaries_lazy_load=false, no
// asnames data mounted -- `docker ps` read "Exited (107)" within seconds,
// every time. That trades a dead-but-safe feature for a stack that cannot
// boot AT ALL until a human intervenes: strictly worse than the defect it
// would fix, so it was rejected rather than shipped.
func isNoASNamesDataset(err error) bool {
	var exc *clickhouse.Exception
	if !errors.As(err, &exc) {
		return false
	}
	return exc.Code == chCodeDictionaryNotFound || exc.Code == chCodeSourceFileMissing
}

// asNamesSQL resolves every ASN in one bound array parameter against the
// dictionary in a single statement, via arrayJoin rather than one bound `?`
// per ASN -- the shape a caller reading this file for the first time might
// reach for, and the one this statement exists to avoid. A page rendering
// 40 distinct ASNs binds one array of 40 and this statement runs once; the
// per-ASN alternative would be 40 round trips to ClickHouse for one screen,
// and neither the array's length nor the placeholder count in the SQL text
// would have to agree with anything -- see TestASNamesAsksTheDictionaryOncePerBatch.
//
// toUInt32(asn), on both the outer projection and inside every
// dictGetOrDefault call, is not decoration. arrayJoin(?) infers its
// element's ClickHouse type from the bound array literal's own values, and
// clickhouse-go's row scanner has no narrowing case for a *uint32
// destination against anything narrower (a batch of small ASNs infers as
// UInt16 or smaller) -- the same hazard collectorsSQL's own doc comment
// names for countIf's UInt64. Casting up explicitly, rather than trusting
// whatever width the literal happened to infer to, is what lets this
// statement's asn column always scan into a Go uint32 regardless of which
// ASNs a particular batch happens to name. It also fixes the KEY type
// dictGetOrDefault is called with: vantage.asnames' own PRIMARY KEY is
// UInt32 (see asnames.sql), and a key of a narrower type is exactly the
// kind of implicit-cast question this package would rather not depend on
// ClickHouse resolving the same way across versions.
//
// Both attributes are asked for with their own explicit default -- the
// empty string, for each -- never a bare dictGet that would throw on a key
// the dictionary itself has no row for. dictGetOrDefault can still raise
// -- see isNoASNamesDataset's own doc comment -- but only when the
// DICTIONARY ITSELF has nothing behind it, never merely because one KEY in
// a batch is absent from an otherwise-loaded dictionary.
//
// %[1]s is q.db, filled by (*Q).ASNames the same way every other statement
// in this package fills its own database placeholder.
const asNamesSQL = `
SELECT
    toUInt32(asn)                                                    AS asn,
    dictGetOrDefault('%[1]s.asnames', 'name',    toUInt32(asn), '')   AS name,
    dictGetOrDefault('%[1]s.asnames', 'country', toUInt32(asn), '')   AS country
FROM (SELECT arrayJoin(?) AS asn)`

// ASNames looks up the registered holder name and country for every ASN in
// asns, in exactly one statement no matter how many ASNs are asked for --
// see asNamesSQL's own doc comment for the arrayJoin shape that makes that
// true.
//
// It answers THREE questions that must never collapse into two, and the
// signature is shaped so a caller cannot read the second off the third
// without also reading the bool:
//
//  1. loaded=true, asns[i] present as a key in the map: a real, registered
//     holder. names[asns[i]] is that ASName.
//  2. loaded=true, asns[i] ABSENT as a key in the map: this ASN has no
//     registered holder -- a genuine fact about the ASN, not a gap in the
//     data. Every ASN in a private-range test deployment's archive is in
//     this state, because every one of them is a private-range number
//     RIPE's list was never going to carry.
//  3. loaded=false: there is no dataset AT ALL. Nobody has run `make
//     fetch-asnames` against this deployment, the dictionary's DDL was
//     never applied to it, or -- the case this package used to get wrong,
//     see isNoASNamesDataset's own doc comment -- ClickHouse itself has
//     not yet re-loaded it since its last restart. The returned map is
//     empty in this case (never partially populated), and callers must
//     check loaded BEFORE reading anything out of it -- a caller that
//     reads names[asn] first and treats a missing key as "unlisted"
//     regardless of loaded reports state 3 as state 2, telling an
//     operator "this AS has no holder" when the truth is "we have never
//     been told any AS's holder" at all. That confusion is this type's
//     entire reason for existing; see
//     TestASNamesDistinguishesUnlistedFromNoDataset.
//
// The three do not collide as VALUES: (true, key present), (true, key
// absent) and (false, *) are three distinct combinations of the two
// return values, and the third is reached however asns happens to look --
// including a caller that reused a map from an earlier, loaded call, since
// this method never returns a caller's own map back to them to mutate.
// What Go's type system cannot do is stop a caller from ignoring the bool
// outright (`names, _, err := q.ASNames(...)`); no signature shaped as a
// map can close that gap, since the same problem already exists for `err`
// on every function in this codebase that returns one. This shape was
// chosen over the alternatives that were considered and rejected for it:
//
//   - A sentinel error for "not loaded" (map, error) would fold an ORDINARY,
//     expected deployment state -- DDL applied, file not yet fetched -- into
//     the same channel this package reserves for something having gone
//     wrong. Every other caller-visible "no data yet" fact in this package
//     (an empty DumpStates map, a Router nobody has, a Peer with no session)
//     is a value, not an error; making this one an error would be
//     inconsistent with the rest of the package for no offsetting gain, and
//     it would put the not-loaded case one misplaced `errors.Is` away from
//     being handled identically to a real ClickHouse failure.
//   - A wrapper struct ({Loaded bool; Names map[uint32]ASName}) does not
//     change the actual hazard at all -- a caller can read .Names without
//     ever consulting .Loaded exactly as easily as it can discard a second
//     return value, and now there are two names to remember instead of one
//     bool the compiler forces every caller to at least name.
//
// So this three-value return is the one this package settles on: it says
// what needs saying in the fewest values, and it is symmetric with how
// every other optional fact in this
// package -- an err beside a result -- already asks its callers to look
// before they read.
//
// asns may be empty; ASNames still answers loaded (a caller may want to
// know whether the dataset exists before it has any ASN to look up), and
// the map returned is simply empty rather than nil, matching state 3's own
// "always empty" rule so a caller cannot distinguish "asked for nothing"
// from "the dataset is missing" by checking for a nil map. An empty asns
// still costs one round trip: the underlying statement is bound against a
// single-element probe array (AS 0 -- reserved, RFC 7607, never a real
// caller's own ASN, the identical sentinel asnames_meta.sql's own single
// row uses) purely so ONE real dictGetOrDefault call happens, because that
// call is what isNoASNamesDataset classifies and what can trigger
// ClickHouse's own lazy-load-on-first-use if the dictionary is not loaded
// yet -- an empty bound array makes arrayJoin produce zero rows, and
// dictGetOrDefault would never be evaluated at all, leaving `loaded`
// impossible to answer honestly or usefully. Whatever that probe finds
// about AS 0 itself never reaches the returned map: asns being empty means
// the map is empty regardless.
//
// There is no batch-size limit enforced here: the caller-facing bound
// belongs to the HTTP layer (api/asnames.go) that actually receives an
// unbounded list from a client; this package's own callers are code, not
// a network boundary, and
// bounding a Go function's slice parameter here would duplicate a decision
// that has to be re-checked at the real trust boundary regardless.
func (q *Q) ASNames(ctx context.Context, asns []uint32) (map[uint32]ASName, bool, error) {
	queryAsns := asns
	if len(queryAsns) == 0 {
		queryAsns = []uint32{0} // probe only; see this method's own doc comment
	}

	rows, err := q.conn.Query(ctx, fmt.Sprintf(asNamesSQL, q.db), queryAsns)
	if err != nil {
		if isNoASNamesDataset(err) {
			return map[uint32]ASName{}, false, nil
		}
		return nil, false, fmt.Errorf("query asnames: %w", err)
	}
	defer rows.Close()

	out := make(map[uint32]ASName, len(asns))
	for rows.Next() {
		var asn uint32
		var name, country string
		if err := rows.Scan(&asn, &name, &country); err != nil {
			return nil, false, fmt.Errorf("scan asnames row: %w", err)
		}
		if len(asns) == 0 {
			// The probe row (AS 0) above -- not one the caller asked
			// about; see this method's own doc comment.
			continue
		}
		// name == "" is dictGetOrDefault's OWN default for this ASN, bound
		// explicitly in asNamesSQL -- it is what an absent key returns, and
		// it is what the parse this dictionary's own source data went
		// through guarantees a PRESENT key never returns: every row
		// fetch-asnames writes has a non-empty name (see that command's own
		// anchored-regex parse), so an empty name here is exclusively the
		// "no row for this key" case, never a listed holder whose name
		// happens to be the empty string. Leaving this ASN out of the map
		// entirely -- rather than adding it with a zero-value ASName -- is
		// what makes state 2 (unlisted) legible to a caller that ranges
		// over the ASNs it asked for and checks each one's presence, the
		// same idiom Peer.DumpStates already asks its own callers to use
		// for "no dump to report" (see that map's own doc comment).
		if name == "" {
			continue
		}
		out[asn] = ASName{Name: name, Country: country}
	}
	if err := rows.Err(); err != nil {
		// Classified identically to the error from q.conn.Query itself
		// above, and for a reason specific to THIS statement's shape, not
		// asNamesPublishedSQL's: a plain constant SELECT with no FROM
		// clause raises synchronously, at the initial Query call, but this
		// statement's FROM (SELECT arrayJoin(?) AS asn) streams rows, and
		// ClickHouse evaluates each row's dictGetOrDefault calls as that
		// stream is consumed -- so a dictionary with nothing behind it
		// raises here, on rows.Next()/rows.Err(), not at the call above.
		// Measured directly (query/asnames_test.go's own
		// TestASNamesReportsNoDatasetWhenTheDictionaryFileIsMissing failed
		// here, with this exact error, before this branch was added) --
		// this is not a hypothetical the doc comment above is guessing at.
		// out is discarded, not returned partially populated, matching
		// this method's own "never partially populated" promise -- in
		// practice ClickHouse fails the whole dictionary lookup before any
		// row scans successfully (one dictionary, one file, one load
		// attempt), but returning a fresh empty map here costs nothing and
		// does not depend on that being true forever.
		if isNoASNamesDataset(err) {
			return map[uint32]ASName{}, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}
