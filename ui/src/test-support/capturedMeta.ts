import type { Meta } from '@/api/generated'

/**
 * A captured fixture's `meta`, typed as the contract's own Meta.
 *
 * The cast is about TypeScript, not about the data. `resolveJsonModule`
 * types every string in an imported .json as `string`, while the generated
 * Meta narrows `warnings[].code` to the three-value union the contract
 * defines -- so a real captured `"session_dumping"` does not structurally
 * satisfy the type it literally came from. Asserting it here, once and with
 * a name that says what is being asserted, keeps the cast out of every test
 * and keeps ResultMeta's props on the generated type rather than on a
 * loosened local copy of it.
 *
 * It takes `unknown` deliberately: nothing here validates the shape, and a
 * parameter type that implied otherwise would be the more misleading of the
 * two options. What makes these safe is that fixtures/README.md's rule
 * holds -- they came off the wire from a real vantage-api.
 */
export function capturedMeta(meta: unknown): Meta {
  return meta as Meta
}
