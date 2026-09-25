import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AppShell from './AppShell.vue'

// The unauthenticated banner is the only thing on screen that tells an
// operator this deployment is open to anyone who can reach it, and deleting
// its <span> left the whole suite green. AppShell's own comment calls it
// chrome rather than a dismissible toast; nothing held it there.
//
// RouterLink is stubbed rather than installing a router: which paths exist
// is router.ts's concern, and this test is about one span. FleetChrome is
// stubbed because it reads /v1/routers through Colada, which would make
// this test -- and App's boot tests, which mount the shell through the boot
// sequence -- need an active Pinia to assert one span. Its own rules live
// in FleetChrome.test.ts.
function shell(mode: 'token' | 'oidc' | 'none') {
  return mount(AppShell, {
    props: { mode },
    global: { stubs: { RouterLink: true, FleetChrome: true } },
  })
}

describe('AppShell', () => {
  it('warns on screen when the daemon is serving unauthenticated', () => {
    expect(shell('none').text()).toContain('unauthenticated')
  })

  // The condition is `mode === 'none'`, not "anything that is not token".
  // Both other modes want a credential and neither deployment is open, so
  // a banner on either would be a false alarm -- and an alarm that cries
  // wolf in oidc mode is an alarm nobody reads in none mode.
  it.each(['token', 'oidc'] as const)('shows no such warning in %s mode', (mode) => {
    expect(shell(mode).text()).not.toContain('unauthenticated')
  })

  it('always renders the wordmark, so the absence above is not an empty render', () => {
    for (const mode of ['token', 'oidc', 'none'] as const) {
      expect(shell(mode).text()).toContain('Vantage')
    }
  })
})
