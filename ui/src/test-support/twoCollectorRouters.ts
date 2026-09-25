import routers from '@/api/fixtures/routers.json'

/**
 * The two entries `/v1/routers` really returns for one router that two
 * collectors monitor, built from the captured single-collector row so every
 * field other than the four named here stays a real captured value.
 *
 * Synthesized rather than captured for `routers.json`'s own reason: that
 * fixture was taken before this stack ran a second collector, it holds ONE
 * entry, and eleven test files read it -- recapturing it to reach this case
 * would churn all of them. The shape it stands in for is real and current
 * though: on the dev stack `/v1/routers` returns 19 entries for 15 routers,
 * four of them dual-homed.
 *
 * Shared by RoutersView and MonitorView so the two cannot drift into two
 * different -- and possibly differently wrong -- constructions of the same
 * condition, the same reason `disambiguationRows.ts` exists.
 *
 * THE NUMBERS ARE CHOSEN TO SEPARATE THREE ANSWERS, not just two. For the
 * single router below:
 *
 *   summing every entry         -> 4 up of 8   (wrong: scales with observers)
 *   per-COLUMN max              -> 3 up of 7   (wrong: mixes vantage points)
 *   argMax on (live, total, id) -> 1 up of 5   (the correct answer, coll-b)
 *
 * A fixture whose two entries merely agreed would pass all three, which is
 * the hole this project has already been caught by once.
 */
export function twoCollectorRouters() {
  const base = routers.data[0]
  return {
    data: [
      // peers_stale is named because routers.json predates the field; 0 is
      // what a collector being heard from reports.
      { ...base, collector: 'coll-a', peers_up: 3, peers_down: 0, peers_view_lost: 0, peers_stale: 0 },
      { ...base, collector: 'coll-b', peers_up: 1, peers_down: 2, peers_view_lost: 2, peers_stale: 0 },
    ],
    meta: routers.meta,
  }
}
