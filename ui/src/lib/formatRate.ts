import { formatCount } from './formatCount'

/**
 * An archived-rows-per-second rate, at a precision that never collides with
 * the one value on these screens that already means something else.
 *
 * ZERO IS A MEASUREMENT HERE, not an absence. /v1/collection/churn/peers
 * ranks by change and omits the quiet, so a peer it never mentions archived
 * nothing in the window. That makes "0.00" a claim, and rounding a real rate
 * into it is the same class of mistake as reporting an absence as a value --
 * which is the defect this project audits for everywhere else.
 *
 * Found in a browser on the Peers screen's first live render against a
 * local dev archive: its busiest peer archives 21 rows in 24 hours, or
 * 0.000162/s, and every rate on the screen drew as "0.00". A quiet
 * archive is exactly where this bites, because the honest answer and
 * the wrong one round together.
 *
 * ONE FORMATTER, not one per screen. Monitor and Peers each had their own,
 * and testing found the fix applied to Peers alone while Monitor's
 * ranked table still drew the same archive's rates as zero. Two copies of a
 * rule is how one of them stays wrong; the duplication IS the defect, not
 * the tidiness problem beside it.
 *
 * Whole numbers above ten, because a rate that large is read for magnitude
 * rather than precision and "1 284.00" is harder to scan than "1 284".
 */
export function formatRate(n: number | undefined): string {
  if (n === undefined) return '—'
  if (n >= 10) return formatCount(Math.round(n))
  // Below the two decimals shown, but not zero: say so rather than round
  // into the value that means nothing was archived at all.
  if (n > 0 && n < 0.005) return '<0.01'
  return n.toFixed(2)
}
