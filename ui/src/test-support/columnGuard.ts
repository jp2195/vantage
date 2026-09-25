// Shared by every screen's column-guard test -- RoutersView.test.ts and
// PeersView.test.ts both call this rather than each hand-rolling its own
// closed-set check, so the two screens cannot drift apart on what "invented
// telemetry" means.
//
// This closes a hole: a guard that only
// checks a column's `id` against the fixture row's real JSON keys lets
// `{ id: 'last_seen', header: 'Uptime' }` through silently -- a legitimate
// field wearing a label that promises a metric the API never returns. A
// column has to clear two independent checks, not one:
//
// 1. Its `id` is a real field on the fixture row. The fixture is captured
//    server output (see api/fixtures/README.md), so its own JSON keys ARE
//    the shape, authoritatively -- no hand-typed field list to fall out of
//    sync with api/openapi.yaml.
//
// 2. Its `header` text does not name a metric this pipeline does not
//    measure, regardless of how real the id backing it is. This is what
//    catches the realistic version of the hole above: a column with a
//    perfectly real id (`session_id`) and a header that lies about what the
//    number means ("Session uptime"). The list below is exactly the
//    product's absent metrics -- present in early mockups, absent from
//    every captured response body.
//
// The last three guard the Events screen's own most explicitly named
// must-not, "There is no severity column": four kinds and a six-value
// reason registry are measured, a ranking over them is editorial, and
// this project's Events row (`{ time, sev, peer, msg, collector }`)
// names two constructs called out verbatim as having "no field behind
// them". They belong on THIS list rather than in a screen-local check
// because the hole they close is the one this file exists for:
// `inventedColumns` catches an invented
// field, so relabeling a real `kind` column "Severity" cleared it. `sev`
// already catches `severity` as a substring; both are listed because a list
// that doubles as documentation should name the word that is forbidden and
// not only its abbreviation.
// "health score" and "score" are later additions, for the Monitor
// screen's own most explicitly named must-not: "there is no health score."
// Four measured signals and one arithmetic ratio (the dump composition
// percentage a single row already carries) are what /v1/collection/* gives
// this screen -- a composite across all four is not a fifth signal, it is
// an invented one, and "score" alone is listed beside "health score" for
// the same reason "sev" sits beside "severity" above: a list that doubles
// as documentation should name the word a future column would reach for
// and not only the two-word phrase.
//
// "health" (bare) is a later addition: `{ id: 'archived',
// header: 'Health' }` rendering a non-percentage composite ("7/10",
// "degraded") clears "health score" and "score" -- neither is a substring
// of "Health" alone -- and clears MonitorView's own percent-count check,
// since it carries no "%". "health" closes that specific hole. Checked
// against every header this repo's screens actually render before adding
// it: none contains "health" today, so this costs no existing coverage.
const INVENTED_METRIC_TERMS = [
  'uptime',
  'updates',
  'per second',
  'pfx/s',
  'lag',
  'drop',
  'sparkline',
  'severity',
  'sev',
  'msg',
  'health',
  'health score',
  'score',
] as const

// `lag` is matched as a whole word only, never as a substring -- unlike
// every other term above. Building MonitorView.vue's own flags section
// found the reason: `/v1/collection/flags` rows carry a real, measured
// `flag` field (bgp/'s own PARSE_FLAG_* name), and "Flag" as a header
// contains the letters "lag" by nothing more than English spelling,
// tripping a term meant to catch invented MESSAGE-LAG telemetry ("update
// lag", "pfx/s lag") on a column that measures neither.
//
// This is a narrow, single-term exemption, not a policy change to the
// whole list. `sev` stays a plain substring match on purpose -- it exists
// SPECIFICALLY to catch "severity" as a substring (see the comment above),
// and switching every term to whole-word matching would silently stop
// `sev` from catching `severity` (since "severity" does not contain the
// word "sev" bounded by non-letters at both ends the way `\bsev\b` would
// require) -- undoing exactly the coverage that term was added for. `lag`
// is the one term this project has actually seen collide with a real field
// name, so it is the one exempted; a future collision gets the same
// treatment on its own term, not a rewrite of this function's matching
// strategy. columnGuard.test.ts pins both directions: "severity" is still
// caught, and "Flag" is not.
const WORD_BOUNDARY_TERMS = new Set<string>(['lag'])

export interface GuardedColumn {
  id: string
  header: string
}

/**
 * Returns every violation found in `columns`, or an empty array when the
 * whole set is clean. A list of strings rather than a boolean/throw so a
 * failing test's diff names exactly which column and which half of the
 * guard caught it, instead of forcing whoever reads the failure to go
 * re-derive that from a thrown message or a bare `false`.
 */
export function inventedColumns(
  columns: GuardedColumn[],
  fixtureRow: Record<string, unknown>,
): string[] {
  const realFields = new Set(Object.keys(fixtureRow))
  const violations: string[] = []
  for (const c of columns) {
    if (!realFields.has(c.id)) {
      violations.push(`column "${c.id}": not a field the captured fixture row carries`)
      continue
    }
    const header = c.header.toLowerCase()
    for (const term of INVENTED_METRIC_TERMS) {
      const matched = WORD_BOUNDARY_TERMS.has(term)
        ? new RegExp(`\\b${term}\\b`).test(header)
        : header.includes(term)
      if (matched) {
        violations.push(`column "${c.id}": header "${c.header}" claims "${term}"`)
      }
    }
  }
  return violations
}
