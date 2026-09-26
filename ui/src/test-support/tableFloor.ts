import type { GuardedColumn } from './columnGuard'

/**
 * Arithmetic for a table's floor: the `minWidth` below which it stops
 * shrinking and scrolls inside its own card.
 *
 * jsdom computes no layout, so a test cannot measure a column. What it can
 * hold is the declared set: under table-layout:fixed a px column is its px
 * and a percentage column is that share of the table, so at the floor every
 * column's width is known. Whether a header or a value fits in that width
 * is a browser measurement, which each test states as a constant.
 */

/** A declared column width, in px, at the given table width. */
export function columnPxAt(width: string | undefined, table: number): number {
  if (!width) throw new Error('a column has no declared width')
  return width.endsWith('%') ? (table * parseFloat(width)) / 100 : parseFloat(width)
}

/**
 * The px columns plus the percentage columns taken of `table`. When this is
 * over `table`, the percentages cannot all be honored: table-layout:fixed
 * pays the px columns first and the percentages get what is left, which on a
 * phone was nothing -- 0px columns with their headers cut off.
 */
export function declaredTotalAt(columns: GuardedColumn[], table: number): number {
  return columns.reduce((sum, c) => sum + columnPxAt((c as { width?: string }).width, table), 0)
}

/**
 * The ids of columns narrower at `table` than the px `need` states for them.
 * Columns `need` does not name are not checked.
 */
export function crampedAt(
  columns: GuardedColumn[],
  table: number,
  need: Record<string, number>,
): [string, number][] {
  return columns
    .map((c) => [c.id, columnPxAt((c as { width?: string }).width, table)] as [string, number])
    .filter(([id, px]) => px < (need[id] ?? -Infinity))
}
