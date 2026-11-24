import { describe, expect, it } from 'vitest'

import { formatClock, formatFreshness } from './formatClock'

/**
 * formatClock renders a row's timestamp. The year is noise on every row of
 * a table when it is the current one, and the fact itself when it is not.
 */
describe('formatClock', () => {
  it('leaves the year out when the instant is in the reader’s current year', () => {
    const text = formatClock('2026-09-21T13:03:07Z', new Date('2026-11-24T18:00:00Z'))
    expect(text).not.toMatch(/2026/)
    expect(text).toMatch(/Sep 21/)
    expect(text).toMatch(/09:03:07/)
  })

  it('keeps the year when the instant is from an earlier year, so an archive gone quiet over New Year says so', () => {
    const text = formatClock('2025-12-28T15:00:00Z', new Date('2026-01-02T18:00:00Z'))
    expect(text).toMatch(/Dec 28, 2025/)
  })

  it('compares years on the operator’s clock, not on UTC’s', () => {
    // The instant is 2025-12-31 19:30 in New York but already 2026 in UTC;
    // "now" is 01:00 on New Year's Day in New York. A UTC comparison calls
    // both 2026 and drops the year the operator needs.
    const text = formatClock('2026-01-01T00:30:00Z', new Date('2026-01-01T06:00:00Z'))
    expect(text).toMatch(/Dec 31, 2025/)
  })
})

/**
 * formatFreshness renders the newest instant the archive holds, for the
 * chrome's freshness fact.
 *
 * The whole point of that fact is to reveal a STALE archive, so the one
 * thing it must never do is render a day-old instant as a bare wall clock:
 * "newest row 09:41" beside "updated 14:03" reads as four hours old when it
 * may be four days. The date appears exactly when it carries information --
 * when the instant is not today -- and stays out of the way when it does
 * not, because the cluster shares one line with an eleven-item nav.
 */
describe('formatFreshness', () => {
  it('renders time of day alone when the instant is today', () => {
    // 09:03 EDT and 14:00 EDT on the same local day.
    const text = formatFreshness('2026-09-21T13:03:07Z', new Date('2026-09-21T18:00:00Z'))
    expect(text).not.toMatch(/2026|Sep/)
    expect(text).toMatch(/9:03:07/)
  })

  it('keeps the date when the instant is not today, so a stale archive cannot read as fresh', () => {
    const text = formatFreshness('2026-09-20T13:03:07Z', new Date('2026-09-21T18:00:00Z'))
    expect(text).toMatch(/Sep 20/)
  })

  /**
   * The falsifier for a UTC-based day comparison.
   *
   * Vitest's TZ is pinned to America/New_York (vite.config.ts) precisely so
   * a test like this can exist: both instants below are 2026-09-21 where
   * the operator is standing, and 2026-09-21 / 2026-09-22 in UTC. An
   * implementation comparing `toISOString().slice(0, 10)` calls them
   * different days and prints a date the reader does not need -- and, worse,
   * would print no date for the reverse pair, which is the failure this
   * function exists to prevent.
   */
  it('compares days on the operator’s clock, not on UTC’s', () => {
    const text = formatFreshness('2026-09-21T23:00:00Z', new Date('2026-09-22T01:00:00Z'))
    expect(text).not.toMatch(/2026|Sep/)
  })
})
