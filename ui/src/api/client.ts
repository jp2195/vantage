import { client } from './generated/client.gen'
import type { AuthState } from './auth'

/**
 * Configures the generated SDK once, at boot. Every operation the UI calls
 * goes through this client, so this is the only place a credential is set --
 * there is no second path for a screen to get one wrong.
 *
 * It hands the client an `auth` callback rather than setting an
 * Authorization header directly, and the difference is which requests get
 * the token. A raw header goes on EVERY request the client makes, including
 * GET /v1/auth/config and GET /v1/openapi.yaml -- the two operations
 * api/openapi.yaml declares `security: []`, meaning they want no credential
 * and the daemon does not read one from them. `auth` is consulted only for
 * an operation the generator gave security metadata to, which is the
 * contract's own list, so the credential goes exactly where the contract
 * says it belongs and nowhere else.
 *
 * baseUrl is deliberately not set. The contract's `servers: [- url: /]`
 * means the generated calls are same-origin relative paths, which is what
 * the browser needs when one Go binary serves both the app and /v1 -- and
 * leaving it alone is also what lets vitest.setup.ts supply the origin
 * Node's Request insists on under jsdom.
 */
export function configureClient(auth: AuthState): void {
  const credential = auth.credential
  client.setConfig({
    auth: credential ? () => credential : undefined,
  })
}
