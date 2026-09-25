import { describe, expect, it } from 'vitest'

import { bestVantageCount, bestVantageRouters } from './bestVantage'
import type { Router } from '@/api/generated'
import routersStale from '@/api/fixtures/routers-stale.json'

/**
 * The counting half of the best-vantage doctrine.
 *
 * `bestVantageRouters` answers it for `/v1/routers` rows, where the identity
 * is the router. This answers it for any rows carrying a collector, where
 * the question is a COUNT of something the network did -- sessions a peer
 * had, times it went down -- and the hazard is identical: two collectors
 * watching one router report every event twice, so a fold over all rows
 * returns two for one thing that happened once.
 */

type Row = { collector: string; session: string; kind: string }

const rows = (...r: [string, string, string][]): Row[] =>
  r.map(([collector, session, kind]) => ({ collector, session, kind }))

const downs = (rs: Row[]) => rs.filter((r) => r.kind === 'down').length
const sessions = (rs: Row[]) => new Set(rs.map((r) => r.session)).size

describe('bestVantageCount', () => {
  /**
   * The defect this exists for, in its exact shipped shape: one BGP session
   * going down once, reported by two collectors that each minted their own
   * session_id for it. Peer detail rendered 2 and 2 in a browser.
   */
  it('does not grow because a second collector watched the same event', () => {
    const observed = rows(
      ['dev-c1', '1789941661644664702', 'down'],
      ['dev-c1', '1789941661644664702', 'up'],
      ['dev-c2', '1789941661645473726', 'down'],
      ['dev-c2', '1789941661645473726', 'up'],
    )
    expect(bestVantageCount(observed, (r) => r.collector, downs)).toBe(1)
    expect(bestVantageCount(observed, (r) => r.collector, sessions)).toBe(1)
  })

  /**
   * Best vantage, not deduplication. When collectors disagree the answer is
   * the one that saw the most -- never their sum, and never the lesser view,
   * which would hide events one collector genuinely recorded.
   */
  it('reports the collector that saw the most, never the sum', () => {
    const observed = rows(
      ['dev-c1', 's1', 'down'],
      ['dev-c1', 's1', 'down'],
      ['dev-c2', 's2', 'down'],
    )
    expect(bestVantageCount(observed, (r) => r.collector, downs)).toBe(2)
  })

  // On a single-collector deployment the doctrine must be a no-op: it has to
  // return exactly what a plain fold returned, or it is a behavior change
  // dressed as a fix.
  it('returns the plain count when only one collector answered', () => {
    const observed = rows(['dev-c1', 's1', 'down'], ['dev-c1', 's1', 'up'], ['dev-c1', 's2', 'down'])
    expect(bestVantageCount(observed, (r) => r.collector, downs)).toBe(2)
    expect(bestVantageCount(observed, (r) => r.collector, sessions)).toBe(2)
  })

  it('answers zero for no rows at all', () => {
    expect(bestVantageCount([], (r: Row) => r.collector, downs)).toBe(0)
  })

  /**
   * A collector that reported rows but none matching the measure still
   * counts as having answered zero -- it must not be skipped in a way that
   * lets another collector's higher number stand in for its view. Here c2
   * saw the session but no down, and the answer is c1's 1, not 0.
   */
  it('takes the maximum when one collector saw the event and another did not', () => {
    const observed = rows(['dev-c1', 's1', 'down'], ['dev-c2', 's2', 'up'])
    expect(bestVantageCount(observed, (r) => r.collector, downs)).toBe(1)
  })
})

/**
 * The row form with a collector gone quiet. Built from a captured row with
 * only the named fields changed.
 */
describe('bestVantageRouters with a stale collector', () => {
  const base: Router = routersStale.data[0]
  const live: Router = { ...base, collector: 'coll-a', peers_up: 3, peers_down: 0, peers_view_lost: 0, peers_stale: 0 }
  // The quiet collector's id sorts LAST, so a rank of (total, collector)
  // alone would take it on a tie.
  const dead: Router = { ...base, collector: 'coll-z', peers_up: 0, peers_down: 0, peers_view_lost: 0, peers_stale: 3 }

  it('on a tie, takes the collector still being heard from, in either order', () => {
    for (const rows of [[live, dead], [dead, live]]) {
      const got = bestVantageRouters(rows)
      expect(got).toHaveLength(1)
      expect(got[0].collector).toBe('coll-a')
    }
  })

  it('takes a live view over a LARGER quiet one', () => {
    // A quiet collector's row is its last view. Seeing more peers before it
    // went quiet does not make it a better answer than a live collector's
    // current one.
    const smallLive: Router = { ...live, peers_up: 1 }
    const bigDead: Router = { ...dead, peers_stale: 7 }
    for (const rows of [[smallLive, bigDead], [bigDead, smallLive]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })

  it('between two live views, still takes the one that saw the most', () => {
    // coll-0 sorts FIRST, so the collector id alone would take coll-a.
    const bigLive: Router = { ...live, collector: 'coll-0', peers_up: 1, peers_down: 4 }
    for (const rows of [[live, bigLive], [bigLive, live]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-0')
    }
  })

  it('between two quiet views, counts stale peers in the total', () => {
    // coll-a saw three peers before it went quiet, coll-z two. Leaving
    // stale out of the total would score them 0 and 1 and take coll-z.
    const quietA: Router = { ...dead, collector: 'coll-a', peers_stale: 3 }
    const quietZ: Router = { ...dead, collector: 'coll-z', peers_down: 1, peers_stale: 1 }
    expect(bestVantageRouters([quietA, quietZ])[0].collector).toBe('coll-a')
  })
})

/**
 * The row form with a restarted collector. A collector that restarts turns
 * every peer of the session it lost into view_lost -- its own statement that
 * it stopped watching, not the router's statement about any peer. Meanwhile
 * the router has reconnected to another collector, which reports those
 * peers up.
 */
describe('bestVantageRouters with a view_lost-only view', () => {
  const base: Router = routersStale.data[0]
  const live: Router = { ...base, collector: 'coll-a', peers_up: 40, peers_down: 0, peers_view_lost: 0, peers_stale: 0 }
  // The restarted collector's id sorts LAST, so a rank of (total, collector)
  // alone would take it on a tie.
  const lost: Router = { ...base, collector: 'coll-z', peers_up: 0, peers_down: 0, peers_view_lost: 40, peers_stale: 0 }

  it('on a tie, takes the view with router statements, in either order', () => {
    for (const rows of [[live, lost], [lost, live]]) {
      const got = bestVantageRouters(rows)
      expect(got).toHaveLength(1)
      expect(got[0].collector).toBe('coll-a')
    }
  })

  it('takes a live view over a LARGER view_lost-only one', () => {
    const bigLost: Router = { ...lost, peers_view_lost: 41 }
    for (const rows of [[live, bigLost], [bigLost, live]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })

  it('takes a quiet collector\'s stale view over a larger view_lost-only one', () => {
    // Stale means the peer was up when its collector was last heard from;
    // view_lost says nothing about the peer at all.
    const stale: Router = { ...lost, collector: 'coll-a', peers_view_lost: 0, peers_stale: 3 }
    const bigLost: Router = { ...lost, collector: 'coll-z', peers_view_lost: 9 }
    for (const rows of [[stale, bigLost], [bigLost, stale]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })

  it('still takes a live view over a larger stale one', () => {
    const smallLive: Router = { ...live, peers_up: 1 }
    const bigStale: Router = { ...lost, peers_view_lost: 0, peers_stale: 50 }
    for (const rows of [[smallLive, bigStale], [bigStale, smallLive]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })

  it('takes a view of down peers only over a larger view_lost-only one', () => {
    // A down is the router's own statement about a session, so a view made
    // only of downs is a live answer, not a view_lost-only one.
    const downs: Router = { ...lost, collector: 'coll-a', peers_view_lost: 0, peers_down: 2 }
    const bigLost: Router = { ...lost, collector: 'coll-z', peers_view_lost: 9 }
    for (const rows of [[downs, bigLost], [bigLost, downs]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })

  it('takes a view of down peers only over a larger stale one', () => {
    const downs: Router = { ...lost, collector: 'coll-a', peers_view_lost: 0, peers_down: 2 }
    const bigStale: Router = { ...lost, collector: 'coll-z', peers_view_lost: 0, peers_stale: 9 }
    for (const rows of [[downs, bigStale], [bigStale, downs]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })

  it('between two view_lost-only views, still takes the one that saw the most', () => {
    const lostA: Router = { ...lost, collector: 'coll-a', peers_view_lost: 5 }
    const lostZ: Router = { ...lost, collector: 'coll-z', peers_view_lost: 4 }
    for (const rows of [[lostA, lostZ], [lostZ, lostA]]) {
      expect(bestVantageRouters(rows)[0].collector).toBe('coll-a')
    }
  })
})
