import type { VueWrapper } from '@vue/test-utils'
import DataTable from '@/components/DataTable.vue'
import type { GuardedColumn } from './columnGuard'

/**
 * The `columns` a screen handed to its `DataTable`, for the column-guard tests.
 *
 * This exists to hold one cast instead of four. `DataTable` is a generic SFC
 * (`generic="T extends Record<string, unknown>"`), which makes its type a
 * generic function component -- and `findComponent`'s overloads do not resolve
 * that, so it falls through to `DOMWrapper<Node>`, which has no `.props()`.
 * The RUNTIME lookup is unchanged and still finds the component; only the
 * static type is wrong, and only here in the test harness.
 *
 * That trade is the point of the generic: production code gained `id: keyof
 * Row` -- a column naming a field the entity does not have now fails
 * `vue-tsc` -- and the cost is this one contained lie in test plumbing, whose
 * return type is honest.
 */
/** Just the slice of a wrapper this helper reads. */
interface PropsReadable {
  props(name: string): unknown
}

export function columnsOf(wrapper: VueWrapper): GuardedColumn[] {
  const table = wrapper.findComponent(DataTable as never)
  return (table as unknown as PropsReadable).props('columns') as GuardedColumn[]
}

/**
 * Every column a screen declared, with the width it declared.
 *
 * The guard this serves: without declared widths the browser's auto layout
 * spreads a table's short values across the whole viewport -- measured on
 * /routes at 2000px, where eight values sat in 500px gaps and reading one
 * row meant tracking the eye across the screen. This project's own
 * tables specify a declared width for every column, so a screen that
 * drops one is a regression no other assertion here would notice.
 *
 * Returned rather than asserted so each screen's test can name its own
 * columns in the failure message.
 */
export function columnWidths(wrapper: VueWrapper): (string | undefined)[] {
  return columnsOf(wrapper).map((c) => (c as { width?: string }).width)
}

/**
 * The `minWidth` a screen handed to its (first) `DataTable`, in px, or
 * undefined when it set none or set it in another unit.
 *
 * jsdom computes no layout, so what a test can hold about a phone-width
 * table is the floor below which it stops shrinking and scrolls inside its
 * own card instead. Whether the floor is enough is a browser measurement.
 */
export function tableMinWidthPx(wrapper: Pick<VueWrapper, 'findComponent'>): number | undefined {
  const table = wrapper.findComponent(DataTable as never)
  const raw = (table as unknown as PropsReadable).props('minWidth')
  const m = typeof raw === 'string' ? raw.match(/^(\d+(?:\.\d+)?)px$/) : null
  return m ? Number(m[1]) : undefined
}
