// How the browser learns to authenticate, and the one place a credential is
// read. Screens never touch a header; they call generated operations.

import { getAuthConfig } from './generated'

export type AuthMode = 'token' | 'oidc' | 'none'

export interface AuthState {
  mode: AuthMode
  /** Present only when the daemon actually wants one. */
  credential?: string
  /** True when the app must ask the operator for a token before it can load data. */
  needsCredential: boolean
}

const STORAGE_KEY = 'vantage.token'

/**
 * Asks the daemon which mode it is in, then decides what the app needs.
 *
 * The daemon is the authority, not storage. A token left in sessionStorage
 * from a previous configuration must not cause the app to send an
 * Authorization header at a daemon running in none mode -- and must not make
 * the app believe it is authenticated at a daemon that has since been
 * switched back to token mode with a different token.
 *
 * It goes through the GENERATED getAuthConfig rather than a hand-written
 * fetch('/v1/auth/config'), and that is the point rather than a style
 * preference: this is the one call the app cannot boot without, and a
 * literal path here would live outside src/api/generated/ where CI's drift
 * check cannot see it. Renaming the operation's path in api/openapi.yaml and
 * api/server.go would then leave CI green while the app 404s at boot. From
 * the generated SDK the path comes from the contract, so the same rename
 * regenerates this call site with it.
 *
 * No credential is involved and none is sent: the contract gives this
 * operation its own `security: []`, so the generated call carries no
 * security metadata and client.ts's `auth` callback is never consulted for
 * it. That is what makes calling it before configureClient legitimate --
 * there is no chicken-and-egg, only an operation that needs nothing.
 */
export async function resolveAuth(): Promise<AuthState> {
  const { data, response } = await getAuthConfig()
  // response is optional in the generated client as of @hey-api/openapi-ts
  // 0.97: a request that never reached the server resolves with no response
  // at all rather than rejecting. That case has to say so, because "HTTP
  // undefined" in the boot error is the least useful thing this can report --
  // this is the one call the app cannot start without.
  if (!response) {
    throw new Error('auth config: no response from the server')
  }
  if (!response.ok || !data) {
    throw new Error(`auth config: HTTP ${response.status}`)
  }
  const { mode } = data

  if (mode === 'none') {
    return { mode, needsCredential: false }
  }
  const stored = readStored()
  return { mode, credential: stored, needsCredential: !stored }
}

export function storeCredential(token: string): void {
  try {
    sessionStorage.setItem(STORAGE_KEY, token)
  } catch {
    // Private windows and blocked site data both throw. The app still
    // works for this tab; the token is simply not remembered.
  }
}

function readStored(): string | undefined {
  try {
    return sessionStorage.getItem(STORAGE_KEY) ?? undefined
  } catch {
    return undefined
  }
}
