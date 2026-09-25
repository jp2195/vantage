import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { resolveAuth } from './auth'

// Each case states its own preconditions rather than relying on file order:
// sessionStorage is cleared and the fetch stub is removed before every test,
// so a case would still pass run alone or in a different order. Sharing
// sessionStorage across cases, or leaving fetch stubbed, would make the
// cases order-dependent.
beforeEach(() => {
  sessionStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

// The daemon's real answer shape, Content-Type included. handleAuthConfig
// sets application/json and the generated client parses by content type, so
// a stub without one would be read as text and never reach a mode branch --
// the test would then be asserting against a shape the daemon never sends.
function stubAuthConfig(mode: string) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ mode }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })))
}

describe('resolveAuth', () => {
  it('reports none mode as authenticated with no credential', async () => {
    stubAuthConfig('none')
    const state = await resolveAuth()
    expect(state.mode).toBe('none')
    expect(state.needsCredential).toBe(false)
  })

  it('reports token mode as needing a credential when none is stored', async () => {
    stubAuthConfig('token')
    const state = await resolveAuth()
    expect(state.mode).toBe('token')
    expect(state.needsCredential).toBe(true)
  })

  it('does not treat a stored token as proof the daemon wants one', async () => {
    // The stored token is stale if the operator switched the daemon to
    // none mode. Trusting storage over the daemon would send an
    // Authorization header the daemon no longer expects.
    sessionStorage.setItem('vantage.token', 'stale')
    stubAuthConfig('none')
    const state = await resolveAuth()
    expect(state.credential).toBeUndefined()
  })

  // It asks the contract's path, and it asks it with no credential. Both
  // come from the generated operation rather than from a literal here --
  // see resolveAuth's comment on why that matters -- so this is the
  // assertion that the generated call is really the one being made.
  it('asks the contract path, unauthenticated', async () => {
    const seen: Request[] = []
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      seen.push(req)
      return new Response(JSON.stringify({ mode: 'none' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }))

    await resolveAuth()

    expect(new URL(seen[0].url).pathname).toBe('/v1/auth/config')
    expect(seen[0].headers.get('Authorization')).toBeNull()
  })

  // A daemon that is up but answering badly must not resolve to a state
  // the app then acts on. App.vue turns this rejection into an on-screen
  // error (see BootError.vue); resolving it into some default would boot
  // the app into a mode nobody chose.
  it('rejects, with the status, when the daemon does not answer 200', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('bad gateway', { status: 502 })))

    await expect(resolveAuth()).rejects.toThrow('502')
  })
})
