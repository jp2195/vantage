// Shared by RoutesView.test.ts and LookingGlassView.test.ts, so the
// rib/path_id disambiguation check both screens need cannot drift into two
// different -- and possibly differently wrong -- constructions.
//
// Rows that vary rib and path_id together leave a hole: removing ONLY the
// path_id column leaves the test passing, because the rows already differ
// on rib alone. A construction that actually pins BOTH columns
// independently needs two
// pairs, not one -- a pair that holds rib constant and varies only path_id,
// and a pair that holds path_id constant and varies only rib. Using this
// helper for both pairs (rather than hand-rolling one of them) is what
// keeps a future editor from reintroducing the same hole by changing one
// field alongside the other again.
//
// Built from one real captured row in every caller, so every OTHER field on
// it -- prefix, next_hop, as_path, communities, all of it -- stays a real,
// captured value; only the field named by `field` is a synthesized second
// value.
export function disambiguationPair<
  T extends Record<string, unknown> & { rib: unknown; path_id: unknown },
>(base: T, field: 'rib' | 'path_id', secondValue: T[typeof field]): [T, T] {
  return [{ ...base }, { ...base, [field]: secondValue }]
}
