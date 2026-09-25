import { describe, expect, it } from 'vitest'

import { formatFreshness } from './formatClock'

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
