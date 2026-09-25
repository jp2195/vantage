import { describe, expect, it } from 'vitest'
import * as generated from './api/generated'

// Every SDK function api/openapi.yaml's operations generate today: all 21 of
// them, in sdk.gen.ts's own order, so `grep -c '^export const' sdk.gen.ts`
// and this array's length are one comparison rather than two lists to
// reconcile. Named explicitly rather than counting Object.keys(generated):
// types.gen.ts is all `export type`, which erases at runtime, so the only
// runtime exports are these 21 functions -- dropping any single one still
// leaves the rest, and a plain Object.keys(generated).length > 0 check keeps
// passing regardless of which one went missing. Naming them means losing one
// from the contract, or pointing openapi-ts.config.ts somewhere else, fails
// here at the point of change instead of only in CI's "generated client is up
// to date" diff.
//
// The list is only worth what its completeness is worth: it shipped naming 16
// of the 21 and claiming to name them all, with five real operations missing
// -- the Events screen runs on findPeerEvents, and the four collection
// operations back Monitor. A regenerate that added a function is what this
// list has to notice, so ADD the new name here rather than letting the count
// drift again.
const expectedOperations = [
  'listRouters',
  'listPeers',
  'findRoutes',
  'findUnicastRoutes',
  'findVpnRoutes',
  'findEvpnRoutes',
  'routeHistory',
  'findTopology',
  'findPeerEvents',
  'ribUnicast',
  'ribVpn',
  'ribEvpn',
  'findLsNodes',
  'findLsLinks',
  'findLsPrefixes',
  'collectionDumps',
  'collectionSessions',
  'collectionLocRib',
  'collectionFlags',
  'getAuthConfig',
  'getOpenApi',
] as const

describe('generated client', () => {
  it('exports every operation the UI depends on', () => {
    const exported = generated as Record<string, unknown>
    for (const name of expectedOperations) {
      expect(typeof exported[name], `"${name}" should be an exported function`).toBe('function')
    }
  })

  // What this test does NOT catch: a field added to or removed from an
  // operation's parameters or response regenerates that operation's
  // Data/Response *type* with a different shape, but under the same
  // runtime function name -- nothing above notices. That is a compile-time
  // fact, not a runtime one, and vue-tsc --noEmit (part of `npm run build`,
  // over every call site) is what enforces it instead.
})
