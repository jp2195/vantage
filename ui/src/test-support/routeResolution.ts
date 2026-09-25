import type { RouteLocationRaw } from 'vue-router'
import { router } from '@/router'

/**
 * The destinations among these that vue-router matches to no declared route.
 *
 * Why this exists rather than string equality against the path a view
 * composes: a test that asserts `to` equals '/peers/1.2.3.4/5.6.7.8' pins
 * the view against its own literal, not against router.ts. Both copies stay
 * agreeable while the ROUTE moves out from under them -- and router.ts has
 * no catch-all, so an unmatched path renders a blank main area rather than
 * anything an operator could read as an error.
 *
 * router.test.ts already closes this for the nav bar, but it skips every
 * ":param" route by design (those are reached by drilling into a row, not
 * from the nav), which leaves /peers/:router/:peer -- the app's only
 * drill-in destination -- guarded by nothing. Renaming it to /peer/:router/
 * :peer passed all 425 tests while making Peer detail unreachable from
 * every link in the app; that mutation is what this helper was written
 * against.
 *
 * Returns the unresolvable destinations rather than asserting, so a caller's
 * `toEqual([])` names the offender in its own diff.
 */
export function unresolvable(destinations: RouteLocationRaw[]): string[] {
  const bad: string[] = []
  for (const d of destinations) {
    if (router.resolve(d).matched.length === 0) {
      bad.push(typeof d === 'string' ? d : JSON.stringify(d))
    }
  }
  return bad
}
