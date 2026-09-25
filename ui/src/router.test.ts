import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { router } from './router'
import AppShell from './components/AppShell.vue'

// The repo has no other route-table test: AppShell.test.ts stubs
// RouterLink outright, on the stated grounds that "which paths exist is
// router.ts's concern, not this test's" -- which leaves nothing checking
// that AppShell's own `nav` array (hand-copied, not derived from routes)
// still agrees with router.ts once either one changes. /events was added
// to both by hand at the same time; the next addition might not be.
//
// Reading hrefs off a mounted, REAL-router AppShell rather than importing
// its `nav` array (not exported) means this test is checking what an
// operator's browser actually resolves, not a second copy of the same
// list -- a typo in `nav` that vue-router still resolves (an extra
// trailing slash, say) would slip past a plain string-equality check on
// the array but still show up here as a route.
describe('nav and router agree', () => {
  it('every AppShell nav entry resolves to a declared route, and no other fixed route is orphaned', () => {
    const w = mount(AppShell, {
      props: { mode: 'none' },
      // FleetChrome reads /v1/routers through Colada, which needs an active
      // Pinia; this test is about the nav's hrefs, so it is stubbed rather
      // than installing a query cache to resolve link targets.
      global: { plugins: [router], stubs: { FleetChrome: true } },
    })
    const navHrefs = w.findAll('a.nav-item').map((a) => a.attributes('href')!)
    // Guards against the whole check passing vacuously because the nav
    // rendered no links at all.
    expect(navHrefs.length).toBeGreaterThan(0)

    for (const href of navHrefs) {
      const resolved = router.resolve(href)
      expect(resolved.matched.length, `nav entry ${href} does not match a declared route`).toBeGreaterThan(0)
    }

    // The other direction: every FIXED-path route (no ":param") is named
    // by some nav entry, so a screen is never reachable only by typing its
    // URL. Two shapes are legitimately exempt rather than nav entries:
    // "/" is a redirect to wherever the nav actually lands (see router.ts's
    // own comment on that entry), and any ":param" route -- today just
    // peer-detail -- is reached by drilling into a row, not from the nav
    // bar, so it is excluded generically rather than by name.
    const navSet = new Set(navHrefs)
    for (const route of router.getRoutes()) {
      if (route.path === '/' || route.path.includes(':')) continue
      expect(navSet.has(route.path), `route ${route.path} has no AppShell nav entry`).toBe(true)
    }
  })
})
