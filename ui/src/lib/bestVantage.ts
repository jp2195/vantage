import type { Router } from '@/api/generated'

/**
 * One entry per ROUTER, from the collector best placed to answer for it.
 *
 * `/v1/routers` is one entry per (collector, router) -- its own contract
 * says a router two collectors monitor "yields one entry per collector, not
 * a merged one" -- so anything counting or summing those entries counts
 * OBSERVERS. That defect class has turned up in the query layer, the
 * Peers table, the Looking glass, the AS-path graph and here.
 *
 * THE RULE THAT SEPARATES A DEFECT FROM AN HONEST NUMBER, because both look
 * identical in code: a count describing the NETWORK -- how many routers
 * there are, how many of their peers are up -- must not grow because a
 * second collector watched. A count describing COLLECTION -- rows archived,
 * observations recorded -- legitimately does. This function is for the
 * first kind only; a table showing what each collector observed must keep
 * rendering every entry, which is why this returns rows rather than
 * mutating anything.
 *
 * BEST SINGLE VANTAGE POINT, not deduplication: resolve
 * each collector's view of a router and report whole the one that saw the
 * most among the views carrying the router's own statements -- then, only
 * when none does, the one that saw the most before its collector went
 * quiet, and last a view that is nothing but view_lost. It can never inflate,
 * it is exact when collectors agree, and on a single-collector deployment it
 * returns exactly what the rows already were.
 *
 * argMax over the TUPLE (tier, total, collector), never a per-column max. A
 * per-column max would take `peers_up` from one collector's row and
 * `peers_view_lost` from another's and present the mixture as one router's
 * state -- two vantage points wearing one row. Ties break on the collector
 * id so the pick is deterministic: an operator reloading the same screen
 * must see the same number, and a tie means the two collectors agree on the
 * total anyway. See rank for what the tier is read from.
 *
 * Deduplicating across collectors is NOT the alternative and is not
 * available: two collectors observing one peer agree on almost nothing a
 * key could be built from, and `QK_TS_ZERO` substitutes each collector's
 * own clock when a router's reads zero -- so an identity keyed on time
 * fails precisely on the routers with broken clocks.
 */
export function bestVantageRouters(rows: readonly Router[]): Router[] {
  const best = new Map<string, Router>()
  for (const row of rows) {
    const seen = best.get(row.ip)
    if (!seen || rank(row) > rank(seen)) best.set(row.ip, row)
  }
  return [...best.values()]
}

/**
 * The comparison key: `(tier, total, collector)`.
 *
 * - tier first, the same order the Grafana Peer status panel merges in:
 *   - 2: no stale peer and at least one up or down. The collector is still
 *     being heard from, or at least its last word here is the router's own
 *     statement about a BGP session.
 *   - 1: at least one stale peer. The collector has gone quiet, and every up
 *     peer it holds reads stale: its row is its last view, and a larger last
 *     view is not a better answer than a smaller current one.
 *   - 0: nothing but view_lost (or unspecified). view_lost is a collector's
 *     statement about itself -- it lost its BMP transport, or restarted --
 *     and says nothing about any peer. When a restarted collector's lost
 *     session sits beside the collector the router reconnected to, the lost
 *     view must not stand in for the live one, whatever its size. A stale
 *     view outranks it: stale peers were up when their collector was last
 *     heard from.
 * - then the total, stale and view_lost included: sessions this collector
 *   observed, in any state. Within a tier the one that saw the most wins.
 * - then the collector id, so a reload shows the same row.
 */
function rank(r: Router): string {
  const tier = r.peers_stale > 0 ? 1 : r.peers_up + r.peers_down > 0 ? 2 : 0
  const total = r.peers_up + r.peers_down + r.peers_view_lost + r.peers_stale
  return `${tier}|${String(total).padStart(12, '0')}|${r.collector}`
}

/**
 * The best-vantage doctrine applied to a COUNT rather than to rows.
 *
 * `bestVantageRouters` above answers the question for `/v1/routers`, where
 * each row IS a router and the identity to collapse on is on the row. This
 * answers it for rows that describe events -- `/v1/events` is one row per
 * OBSERVATION, so a router two collectors watch reports every session and
 * every state change twice, and a fold over all rows returns two for one
 * thing that happened once.
 *
 * Session identity cannot rescue such a fold: each collector mints its own
 * `session_id` for the same underlying session (1789941661644664702 and
 * 1789941661645473726 are dev-c1's and dev-c2's names for one session), so
 * counting distinct session ids counts observers just as surely as counting
 * rows does.
 *
 * Same rule as the row form, and the same reason it is a rule rather than a
 * judgment call: a count describing the NETWORK must not grow because a
 * second collector watched, while a count describing COLLECTION legitimately
 * does. `measure` is applied to each collector's rows ALONE and the largest
 * answer wins -- never the sum, which inflates, and never the smallest,
 * which would hide events a collector genuinely recorded. Ties break on the
 * collector id so a reload shows the same number, and a tie means the two
 * agreed anyway.
 *
 * `measure` receives one collector's rows and returns its count, so the
 * caller keeps the definition of what is being counted: distinct sessions,
 * down events, transitions. The grouping is the part that must not be
 * gotten wrong twice.
 */
export function bestVantageCount<T>(
  rows: readonly T[],
  collectorOf: (row: T) => string,
  measure: (rows: T[]) => number,
): number {
  const byCollector = new Map<string, T[]>()
  for (const row of rows) {
    const id = collectorOf(row)
    const group = byCollector.get(id)
    if (group) group.push(row)
    else byCollector.set(id, [row])
  }

  let best = 0
  let bestCollector: string | undefined
  for (const [id, group] of byCollector) {
    const n = measure(group)
    if (bestCollector === undefined || n > best || (n === best && id < bestCollector)) {
      best = n
      bestCollector = id
    }
  }
  return best
}
