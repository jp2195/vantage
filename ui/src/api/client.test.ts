import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { client } from './generated/client.gen'
import { getAuthConfig, listRouters } from './generated'
import { configureClient } from './client'
import type { AuthState } from './auth'

// client.ts had no test file at all, and it is the single line that
// authenticates every request this UI will ever make: the whole suite
// stayed green with the credential wiring replaced by nothing. So these
// cases assert the REQUEST, not the configuration object -- a config the
// client happens to hold proves nothing about what goes on the wire, and
// what goes on the wire is the only thing the daemon reads.

// captureRequests stubs fetch and returns the array the generated client's
// outgoing Requests land in. Every case states its own preconditions: the
// stub is removed and the shared client reset before each one, so a case
// would still pass run alone or in a different order.
function captureRequests(): Request[] {
  const seen: Request[] = []
  vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
    seen.push(req)
    return new Response(JSON.stringify({ data: [], meta: {} }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  }))
  return seen
}

function tokenState(credential?: string): AuthState {
  return { mode: credential ? 'token' : 'none', credential, needsCredential: false }
}

beforeEach(() => {
  // baseUrl comes from vitest.setup.ts and must survive; auth is what each
  // case sets for itself. Passing undefined clears whatever a previous case
  // configured, because setConfig merges rather than replaces.
  client.setConfig({ auth: undefined })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('configureClient', () => {
  it('sends the credential as a bearer token on a gated operation', async () => {
    const seen = captureRequests()
    configureClient(tokenState('tok-abc123'))

    await listRouters()

    expect(seen).toHaveLength(1)
    expect(seen[0].headers.get('Authorization')).toBe('Bearer tok-abc123')
  })

  it('sends no credential before it has run', async () => {
    const seen = captureRequests()

    await listRouters()

    expect(seen[0].headers.get('Authorization')).toBeNull()
  })

  // The reason the credential is driven by the client's `auth` callback and
  // not by a raw Authorization header: a raw header goes on every request,
  // including the two operations api/openapi.yaml declares `security: []`.
  // Those are public precisely because a caller reaches them before it holds
  // a credential; sending one anyway hands the token to routes that never
  // asked for it and that the daemon does not read it from.
  it('does not send the credential to the operations the contract declares public', async () => {
    const seen = captureRequests()
    configureClient(tokenState('tok-abc123'))

    await getAuthConfig()

    expect(seen[0].headers.get('Authorization')).toBeNull()
  })

  // none mode: the daemon wants no credential, so nothing may be sent even
  // if a stale one is lying around in storage. resolveAuth already refuses
  // to put it in the state; this is the second half of that promise.
  it('clears a credential a previous call configured', async () => {
    configureClient(tokenState('tok-abc123'))
    configureClient(tokenState(undefined))
    const seen = captureRequests()

    await listRouters()

    expect(seen[0].headers.get('Authorization')).toBeNull()
  })

  // The path the request carries has to be the contract's, unprefixed by
  // anything this file sets. vitest.setup.ts supplies the origin jsdom's
  // Request insists on (see its comment); everything after it comes from
  // the generated operation.
  it('leaves the generated path alone', async () => {
    const seen = captureRequests()
    configureClient(tokenState('tok-abc123'))

    await listRouters()

    expect(new URL(seen[0].url).pathname).toBe('/v1/routers')
  })
})
