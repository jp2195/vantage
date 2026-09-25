<script setup lang="ts" generic="T extends Record<string, unknown>">
import type { Meta } from '@/api/generated'
import ResultMeta from './ResultMeta.vue'

/**
 * A column addresses a field that exists ON THE ROW TYPE. `id` is
 * `keyof Row`, not `string`, so a column naming a field the entity does not
 * have is a compile error at the call site rather than an empty cell in
 * production.
 *
 * That is the property committing the generated client is meant to buy: a
 * field moving should surface as a compile error in a table's column
 * definitions, not as a blank cell in production. With `id: string`
 * it surfaced as a blank cell -- `columnGuard` caught it eventually, but only
 * on the next fixture recapture, and only for screens that call the guard.
 * Renaming a field in api/openapi.yaml, regenerating, and typechecking now
 * names every table that referenced it.
 *
 * `Row` defaults to the old permissive shape so an untyped `Column[]` still
 * compiles; every screen in this repo passes a real entity.
 */
export interface Column<Row extends Record<string, unknown> = Record<string, unknown>> {
  id: Extract<keyof Row, string>
  header: string
  /** Right-align and use tabular figures. Numbers only. */
  numeric?: boolean
  /**
   * This column's width, in the table's own units -- `'88px'`, `'12%'` --
   * rendered into a `<colgroup>` and therefore sizing the COLUMN rather
   * than one row's cell. Optional: a column that declares nothing absorbs
   * the slack the declared ones leave, which is how a table fills its
   * container without a ragged edge.
   *
   * Why this exists at all: with the browser's auto layout, /routes
   * distributed eight short values across a 2000px viewport and a row had
   * to be read by tracking 500px of white. This project's tables specify
   * declared widths on every table (Routes, for one, is
   * `minmax(130px,1.1fr) 88px 130px ...` with a 1150px min-width), in grid
   * `minmax()` units; the
   * translation into px and % is the view's, because `minmax()` is not
   * something a `<col>` can carry.
   */
  width?: string
}

defineProps<{
  columns: Column<T>[]
  rows: T[]
  meta?: Meta
  loading?: boolean
  error?: Error
  /**
   * Extra attributes merged onto each row's `<tr>`. Optional and additive --
   * every other caller in this repo omits it and gets the old markup
   * unchanged.
   *
   * This exists so a screen can address one semantic ROW rather than one
   * cell. A `#cell-<id>` slot only ever reaches its own `<td>`; there is no
   * way for it to mark up a SIBLING cell, so a test (or a screen) that needs
   * to find "the down_reason cell that belongs to this view_lost row" has
   * nothing to anchor on without an attribute the two cells actually share
   * an ancestor of. SessionHistoryView.vue is the first caller: its rows
   * carry `data-kind`, and its down_reason cell carries `data-reason`
   * beneath it, so a test can assert the reason cell WITHIN one addressed
   * kind rather than trusting DOM order to keep the two aligned.
   */
  rowAttrs?: (row: T) => Record<string, string>
  /**
   * The narrowest this table reads correctly at, e.g. `'1150px'`. Applied
   * to the TABLE, never to the page: the wrapper scrolls horizontally, so a
   * wide table is scrollable inside its own card and the page body never
   * scrolls sideways because of it.
   */
  minWidth?: string
  /**
   * Width for the trailing actions column, which exists only when a caller
   * passes an `#actions` slot.
   */
  actionsWidth?: string
}>()

/**
 * Cells whose `title` this component set, as opposed to one a caller's slot
 * or markup put there. Only these are rewritten or cleared.
 */
const titledHere = new WeakSet<HTMLTableCellElement>()

/**
 * Whether a cell shows less than its content: the cell itself overflows, or
 * something inside it that ellipsizes on its own does. Measured only when
 * the pointer arrives, never per render.
 */
function truncated(td: HTMLTableCellElement): boolean {
  if (td.scrollWidth > td.clientWidth) return true
  for (const el of td.querySelectorAll<HTMLElement>('*')) {
    if (el.scrollWidth > el.clientWidth && getComputedStyle(el).textOverflow === 'ellipsis') return true
  }
  return false
}

/**
 * A cell's text, one space between each piece, whitespace collapsed.
 *
 * Its text nodes joined with a space, not innerText: sibling elements a
 * slot places side by side are separated on screen by margin, not by a
 * character, and innerText runs them together. Monitor's Peer cell -- an
 * address link, then a "history" link -- came out "172.31.0.90history".
 */
function cellText(td: HTMLTableCellElement): string {
  const parts: string[] = []
  const walk = document.createTreeWalker(td, NodeFilter.SHOW_TEXT)
  for (let n = walk.nextNode(); n; n = walk.nextNode()) parts.push(n.textContent ?? '')
  return parts.join(' ').replace(/\s+/g, ' ').trim()
}

/**
 * A body cell cut off with an ellipsis shows a different value -- an
 * address, a collector id -- so on hover it gets its full text as a title.
 * One delegated handler on the tbody. A title the cell already carries is
 * left alone; one set here is cleared when the cell fits again, after a
 * resize.
 */
function titleIfTruncated(e: MouseEvent) {
  const td = (e.target as Element | null)?.closest?.('td')
  if (!td || !(e.currentTarget as Element).contains(td)) return
  // Moving between elements inside one cell is not arriving at it.
  if (e.relatedTarget instanceof Node && td.contains(e.relatedTarget)) return
  const ours = titledHere.has(td)
  if (td.hasAttribute('title') && !ours) return
  if (truncated(td)) {
    const text = cellText(td)
    if (text) {
      td.setAttribute('title', text)
      titledHere.add(td)
    }
  } else if (ours) {
    td.removeAttribute('title')
    titledHere.delete(td)
  }
}
</script>

<template>
  <div class="wrap">
    <!-- Four states, and they must not be confusable. A failed query is not
         an empty result: one means "we do not know", the other means "there
         is nothing here", and showing an empty table for both is how a UI
         tells an operator the network is fine when the API is down.

         `loading` alone is not the right gate for the blocking state, even
         though it looks like it should be. A polling query (Colada's
         useRouters, refetching every 30s) and a paginated walk (useRibPage's
         loadMore, refetching on every "load more" click) both flip `loading`
         true again long after real rows are already on screen -- a caller
         that wires either one straight into this prop would blank a
         populated table on every tick or click, then print "loading…"
         directly above the ResultMeta footer's still-valid "complete as of"
         claim underneath it. `rows.length === 0` is what actually
         distinguishes "nothing to show yet" from "a refetch of something we
         already have is in flight": a caller can still get the SIGNAL
         wrong (RoutersView.vue found and fixed exactly that -- Colada's
         isPending, not isLoading, is the one that means "no data yet"), but
         it can no longer blank a table that already has rows just by being
         slow to say so. -->
    <p v-if="error" class="error" role="alert">{{ error.message }}</p>
    <p v-else-if="loading && rows.length === 0" class="quiet">loading…</p>
    <p v-else-if="rows.length === 0" class="quiet">no rows matched</p>

    <table v-else :style="minWidth ? { minWidth } : undefined">
      <!-- The declared widths. `table-layout: fixed` below is what makes
           them binding: without it a long cell value still stretches its
           column past the width its spec asked for. -->
      <colgroup>
        <col v-for="c in columns" :key="c.id" :style="c.width ? { width: c.width } : undefined" />
        <col v-if="$slots.actions" :style="actionsWidth ? { width: actionsWidth } : undefined" />
      </colgroup>
      <thead>
        <tr>
          <th v-for="c in columns" :key="c.id" :class="{ num: c.numeric }">{{ c.header }}</th>
          <!-- The trailing affordance column carries no label: this project's
               own Detail column has none, and a header over it would have to
               name a field that does not exist on the row. It lives OUTSIDE
               `columns` for that reason -- Column.id is `keyof Row`, which is
               the property that turns a renamed field into a compile error,
               and columnGuard reads the same array to catch a column claiming
               a metric the API never measured. An action is neither. -->
          <th v-if="$slots.actions" aria-label="row actions"></th>
        </tr>
      </thead>
      <tbody @mouseover="titleIfTruncated">
        <tr v-for="(row, i) in rows" :key="i" v-bind="rowAttrs ? rowAttrs(row) : {}">
          <td v-for="c in columns" :key="c.id" :class="{ num: c.numeric, mono: c.numeric }">
            <slot :name="`cell-${c.id}`" :row="row">{{ row[c.id] }}</slot>
          </td>
          <td v-if="$slots.actions" class="actions"><slot name="actions" :row="row" /></td>
        </tr>
      </tbody>
    </table>

    <!-- Rendered whenever a response arrived, including one with no warnings:
         an empty warnings array is a claim the answer is complete, and
         dropping it makes a complete answer and an unexamined one identical.

         Gated on `!error` too: `meta` is whatever the last SUCCESSFUL fetch
         returned, and a screen keeps it in props even after a later refetch
         fails (Colada leaves `data` in place and only sets `error`). Without
         this guard the footer would keep asserting "complete as of" -- a
         claim about the last good answer -- directly beneath the message
         saying the current attempt failed. The error already means "we do
         not know"; a completeness claim printed under it is a contradiction
         on screen, not a nuance. -->
    <ResultMeta v-if="meta && !error" :meta="meta" :shown="rows.length" />
  </div>
</template>

<style scoped>
/* overflow-x, not hidden: a table wider than its container scrolls HERE,
   inside its own card, so the page body never scrolls sideways. */
.wrap {
  border: 1px solid var(--line); border-radius: 8px; background: var(--surface);
  overflow-x: auto; overflow-y: hidden;
}
/* fixed, so <colgroup>'s widths bind and a long value wraps or ellipses
   inside its column instead of widening it. */
table { width: 100%; table-layout: fixed; border-collapse: collapse; font: 400 12px var(--font-ui); }
/* This project's column label: 9.5px 600 uppercase with .07em tracking
   on --surface-2, the scale used for column and field headers alike.
   Not simply "smaller" than the previous sentence-case 11px -- these
   labels are meant to recede so the data reads first. */
th {
  text-align: left; padding: 7px 18px; color: var(--muted);
  font: 600 9.5px var(--font-ui); text-transform: uppercase; letter-spacing: .07em;
  border-bottom: 1px solid var(--line); background: var(--surface-2);
  white-space: nowrap;
}
/* 9px 18px is this project's `regular` density. A `compact` 5px mode, a
   persisted user preference, is not built here. */
td {
  padding: 9px 18px; border-bottom: 1px solid var(--line-faint); color: var(--ink);
  overflow: hidden; text-overflow: ellipsis;
}
tbody tr:hover td { background: var(--surface-2); }
.actions { text-align: right; overflow: visible; }
tbody tr:last-child td { border-bottom: 0; }
.num { text-align: right; font-variant-numeric: tabular-nums; }
.error { margin: 0; padding: 14px 16px; color: var(--bad-2); background: var(--bad-tint); font: 400 12px var(--font-ui); }
.quiet { margin: 0; padding: 14px 16px; color: var(--muted); font: 400 12px var(--font-ui); }
</style>
