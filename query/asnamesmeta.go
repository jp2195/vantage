package query

import (
	"context"
	"fmt"
)

// asNamesPublishedSQL reads the companion dictionary's single row. There is
// no batch and no arrayJoin here -- unlike asNamesSQL, this dictionary
// holds exactly one row, keyed on a fixed sentinel (id = 0, chosen because
// it is not an AS number and this dictionary is not asnames, so there is no
// AS-0-is-reserved question to raise) -- and the explicit empty-string default is
// what keeps a row that does not exist from raising rather than answering,
// the same reason asNamesSQL binds one for name and country.
//
// dictGetOrDefault here can still raise -- see isNoASNamesDataset's own
// doc comment (query/asnames.go) for the two codes ASNamesPublished
// classifies below, identically to ASNames' own use of the same function:
// a missing dictionary OBJECT (Code 36) and an existing object whose FILE
// source is absent (Code 107), the latter being the ordinary state of
// EVERY dictionary in this project immediately after a ClickHouse
// restart, data and all, until something calls dictGet on it again.
const asNamesPublishedSQL = `SELECT dictGetOrDefault('%[1]s.asnames_meta', 'last_modified', toUInt8(0), '')`

// ASNamesPublished reports the AS holder-name dataset's own publication
// date: RIPE's Last-Modified response header, captured once by `make
// fetch-asnames` (deploy/dev/asnames/asnames_meta.tsv) and carried into
// ClickHouse by the companion dictionary asNamesPublishedSQL reads.
//
// It is returned as the RAW string this package scanned out of ClickHouse
// -- an HTTP-date, unparsed -- rather than a time.Time, because turning it
// into one is a WIRE-FORMAT decision, and every other such decision in this
// codebase (an address's text form, a u64's string form, a timestamp's UTC
// normalization) is made at the api/ boundary that actually renders JSON,
// not inside this package. api/asnames.go is the one caller and the one
// place that parsing happens.
//
// The bool answers the same shape of question ASNames' own second return
// value does, at a coarser grain: true only when THIS dictionary -- not
// asnames itself -- is LOADED with a non-empty value. It does not track
// ASNames' own loaded return and a caller must not assume it does: the two
// dictionaries are independent objects, ordinarily fetched and applied
// together but never guaranteed to be, and a stack where one is present and
// the other is not is a real, if unusual, deployment shape rather than a
// bug to guard against elsewhere.
//
// A stored value that scans as the empty string -- whether because the
// dictionary has no row at all (dictGetOrDefault's own default) or because
// a row exists with last_modified genuinely empty -- reports false, same
// as "not loaded": an empty date is not a date, and this package's rule for
// looked-up data is that it never guesses one.
func (q *Q) ASNamesPublished(ctx context.Context) (string, bool, error) {
	var s string
	if err := q.conn.QueryRow(ctx, fmt.Sprintf(asNamesPublishedSQL, q.db)).Scan(&s); err != nil {
		if isNoASNamesDataset(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("query asnames_meta: %w", err)
	}
	if s == "" {
		return "", false, nil
	}
	return s, true, nil
}
