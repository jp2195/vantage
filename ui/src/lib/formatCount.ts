/**
 * A count, grouped the way this project writes one: "992 411", not
 * "992,411" (1 284, 214 509 and 6 412 are the same shape). A thin
 * space, not a comma, because a comma is a decimal separator in half the
 * world and these numbers are read by operators everywhere.
 *
 * U+2009 THIN SPACE rather than a plain space: it keeps the group visually
 * one number, and paired with `font-variant-numeric: tabular-nums` (see
 * tokens.css's .mono) a column of them still aligns digit for digit.
 */
export function formatCount(n: number): string {
  return new Intl.NumberFormat('en-US').format(n).replace(/,/g, ' ')
}
