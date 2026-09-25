/**
 * Identity, compared where handing both sides to `toBe` would be a hazard.
 *
 * `expect(a).toBe(b)` builds a failure diff by SERIALIZING both sides, and a
 * Vue ref (or any reactive object) carries its dependency graph with it: the
 * effects subscribed to it, their own dependency lists, and every object those
 * reach. Measured, while mutation-testing LookingGlassView's Topology tab: an
 * `expect(topologyScopeRef).toBe(filtersRef)` over two refs CORRECTLY detected
 * the injected defect and then killed the whole test file with
 *
 *     FATAL ERROR: Ineffective mark-compacts near heap limit
 *     Allocation failed - JavaScript heap out of memory
 *
 * rather than printing a failure -- so the mutation read as "survived" until
 * the run was looked at by hand. A test whose failure mode is an
 * out-of-memory crash reports nothing at the moment it matters most.
 *
 * Comparing here and asserting the BOOLEAN gives the runner nothing to
 * serialize: `expect(sameRef(a, b)).toBe(true)` fails with "expected false to
 * be true", which is terse but survivable. Pair it with a `toEqual` over the
 * VALUES when a failure needs something readable to show -- a `.value` is
 * ordinary data and serializes fine; it is the ref around it that does not.
 */
export function sameRef(a: unknown, b: unknown): boolean {
  return a === b
}
