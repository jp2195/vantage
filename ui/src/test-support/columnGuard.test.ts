import { describe, expect, it } from 'vitest'
import { inventedColumns } from './columnGuard'

// This file did not exist before `inventedColumns`'s matching was
// refined. It exists now because that change altered how
// `inventedColumns` matches one term ("lag") and added a new one
// ("health") -- a shared guard six screens' own tests
// depend on, with no test of its own proving the matching strategy itself
// behaves as documented in columnGuard.ts's comments. Direct unit tests of
// `inventedColumns`, not routed through any one screen's fixture, so a
// regression here is caught once rather than possibly not at all if it
// happened to miss every screen's own captured headers.
const ROW = { id: 1, value: 2 }

describe('inventedColumns', () => {
  // The regression this file exists to prevent: fixing "lag" must not cost
  // the coverage "sev" was added for. `severity` contains no whole word
  // "sev" bounded by non-letters (`\bsev\b` would not match it), so if that
  // word-boundary treatment had been applied to the whole list
  // instead of to "lag" alone, this would silently stop catching real
  // severity columns.
  it('still catches "severity" as a substring of a longer header', () => {
    expect(inventedColumns([{ id: 'id', header: 'Severity' }], ROW)).not.toEqual([])
    expect(inventedColumns([{ id: 'id', header: 'Event severity' }], ROW)).not.toEqual([])
  })

  it('still catches "sev" as an abbreviation, the reason it is listed beside "severity"', () => {
    expect(inventedColumns([{ id: 'id', header: 'Sev' }], ROW)).not.toEqual([])
  })

  // The false positive later testing found: a real, measured field named
  // "flag" (/v1/collection/flags' own row shape) has a header that
  // contains the letters "lag" purely as English spelling, and used to trip
  // the term meant for invented message-lag telemetry.
  it('does not flag "Flag" on account of the word "lag" hiding inside it', () => {
    expect(inventedColumns([{ id: 'id', header: 'Flag' }], ROW)).toEqual([])
  })

  // The term still has to do its real job: an invented lag metric, as its
  // own word, is still caught.
  it('still catches "lag" as its own word', () => {
    expect(inventedColumns([{ id: 'id', header: 'Update lag' }], ROW)).not.toEqual([])
    expect(inventedColumns([{ id: 'id', header: 'Lag' }], ROW)).not.toEqual([])
  })

  // "health" (bare), a later addition: a column headed exactly
  // "Health" cleared "health score" and "score" (neither is a substring of
  // "Health" alone) before this term existed.
  it('catches a bare "Health" header, not only "health score"', () => {
    expect(inventedColumns([{ id: 'id', header: 'Health' }], ROW)).not.toEqual([])
  })

  it('still passes a column that is real end to end', () => {
    expect(inventedColumns([{ id: 'id', header: 'Router' }], ROW)).toEqual([])
  })
})
