<script setup lang="ts" generic="T extends Record<string, unknown>">
import { computed, ref } from 'vue'
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
  /**
   * The header becomes a button that sorts the rows on screen by this
   * column. Opt-in, per column: a screen offers it only where the order of
   * its rows is not itself the answer.
   */
  sortable?: boolean
  /**
   * What to sort by, when the field's raw value is not it -- a state that
   * should order by severity, a cell the slot renders from two fields.
   * Null and undefined sort last in both directions.
   */
  sortValue?: (row: Row) => string | number | null | undefined
  /**
   * Set the column in the data face: addresses, prefixes, AS paths. Numeric
   * columns get it already. Without it a table showed a peer address in
   * one face and the router address beside it in another.
   */
  mono?: boolean
}

const props = defineProps<{
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
  /**
   * How many rows the caller's own filter is hiding. With no rows left, the
   * empty state says the filter emptied the table rather than "no rows
   * matched", which would read as a claim about the network.
   */
  filteredOut?: number
  /**
   * A second line under the empty state, for what the screen knows about
   * why nothing matched -- "No events in the last hour" says more than the
   * bare fact, and points at the control that would widen it.
   */
  emptyHint?: string
}>()

/**
 * The column the rows are sorted by, and which way. Null is the order the
 * caller passed, which is the API's order and often meaningful (newest
 * first, ranked by churn), so a third click goes back to it.
 */
const sort = ref<{ id: string; dir: 1 | -1 } | null>(null)

/**
 * Sorting is withheld from an answer that is not the whole answer. A
 * truncated result sorted by a column reads as "the top N by that column",
 * which it is not; a paginated walk across a changing view may hold a row
 * twice. Both are already said in the footer, and a sort would quietly
 * contradict it.
 */
const sortBlocked = computed(() =>
  (props.meta?.warnings ?? []).some((w) => w.code === 'truncated' || w.code === 'paginated_smear'),
)

const canSort = (c: Column<T>) => !!c.sortable && !sortBlocked.value

function toggleSort(c: Column<T>) {
  const cur = sort.value
  if (!cur || cur.id !== c.id) sort.value = { id: c.id, dir: 1 }
  else if (cur.dir === 1) sort.value = { id: c.id, dir: -1 }
  else sort.value = null
}

function ariaSort(c: Column<T>): 'ascending' | 'descending' | 'none' | undefined {
  if (!canSort(c)) return undefined
  if (sort.value?.id !== c.id) return 'none'
  return sort.value.dir === 1 ? 'ascending' : 'descending'
}

/** Up for ascending, down for descending, both for a column not yet sorted. */
function arrowPath(c: Column<T>): string {
  if (sort.value?.id !== c.id) return 'M1 4l3-3 3 3M1 6l3 3 3-3'
  return sort.value.dir === 1 ? 'M1 6l3-3 3 3' : 'M1 4l3 3 3-3'
}

// Digit runs compare as numbers, so 10.0.0.9 sorts before 10.0.0.10 and
// xr-p2 before xr-p10.
const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

const shownRows = computed<T[]>(() => {
  const s = sort.value
  if (!s || sortBlocked.value) return props.rows
  const col = props.columns.find((c) => c.id === s.id)
  if (!col) return props.rows
  const valueOf = (r: T) => (col.sortValue ? col.sortValue(r) : (r[col.id] as unknown))
  const blank = (v: unknown) => v === null || v === undefined || v === ''
  // slice: never reorder the caller's array in place.
  return props.rows.slice().sort((a, b) => {
    const va = valueOf(a)
    const vb = valueOf(b)
    if (blank(va) || blank(vb)) return blank(va) === blank(vb) ? 0 : blank(va) ? 1 : -1
    const cmp =
      typeof va === 'number' && typeof vb === 'number' ? va - vb : collator.compare(String(va), String(vb))
    return cmp * s.dir
  })
})

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
    <!-- The caller's filter controls, inside the card and above every state
         below, so a filter that emptied the table can be cleared from where
         its effect is shown. -->
    <div v-if="$slots.toolbar" class="toolbar"><slot name="toolbar" /></div>

    <p v-if="error" class="error" role="alert">{{ error.message }}</p>
    <p v-else-if="loading && rows.length === 0" class="quiet loading" role="status">loading…</p>
    <p v-else-if="rows.length === 0 && filteredOut" class="empty" data-filtered-empty>
      <strong>Nothing here matches the filter.</strong>
      <span>It is hiding {{ filteredOut }} {{ filteredOut === 1 ? 'row' : 'rows' }} of a complete answer.</span>
    </p>
    <p v-else-if="rows.length === 0" class="empty" data-empty>
      <strong>No rows matched.</strong>
      <span v-if="emptyHint">{{ emptyHint }}</span>
    </p>

    <div v-else class="scroll">
    <table :style="minWidth ? { minWidth } : undefined">
      <!-- The declared widths. `table-layout: fixed` below is what makes
           them binding: without it a long cell value still stretches its
           column past the width its spec asked for. -->
      <colgroup>
        <col v-for="c in columns" :key="c.id" :style="c.width ? { width: c.width } : undefined" />
        <col v-if="$slots.actions" :style="actionsWidth ? { width: actionsWidth } : undefined" />
      </colgroup>
      <thead>
        <tr>
          <th v-for="c in columns" :key="c.id" :class="{ num: c.numeric }" :aria-sort="ariaSort(c)">
            <button v-if="canSort(c)" type="button" class="sort" @click="toggleSort(c)">
              {{ c.header }}<svg class="arrow" :class="{ idle: sort?.id !== c.id }" width="8" height="10" viewBox="0 0 8 10" aria-hidden="true"><path :d="arrowPath(c)" /></svg>
            </button>
            <template v-else>{{ c.header }}</template>
          </th>
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
        <tr v-for="(row, i) in shownRows" :key="i" v-bind="rowAttrs ? rowAttrs(row) : {}">
          <td v-for="c in columns" :key="c.id" :class="{ num: c.numeric, mono: c.numeric || c.mono }">
            <slot :name="`cell-${c.id}`" :row="row">{{ row[c.id] }}</slot>
          </td>
          <td v-if="$slots.actions" class="actions"><slot name="actions" :row="row" /></td>
        </tr>
      </tbody>
    </table>
    </div>

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
/* The card. Its corners clip what is inside; the table scrolls in .scroll. */
.wrap {
  border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface);
  overflow: hidden;
}
/* overflow-x, not hidden: a table wider than its container scrolls HERE,
   inside its own card, so the page body never scrolls sideways. */
.scroll { overflow-x: auto; overflow-y: hidden; }
/* Scroll shadows: a shade at whichever edge cuts content off, and none when
   the table fits. The two "local" covers scroll with the content and hide
   the two "scroll" shades once an edge is reached, so the shade shows only
   where there is more to see. Without it, a table cut at the card's edge
   looked like a rendering fault rather than something to scroll. */
.scroll {
  background:
    linear-gradient(to right, var(--surface) 30%, transparent) left / 40px 100% no-repeat local,
    linear-gradient(to left, var(--surface) 30%, transparent) right / 40px 100% no-repeat local,
    radial-gradient(farthest-side at 0 50%, var(--scroll-shade), transparent) left / 14px 100% no-repeat scroll,
    radial-gradient(farthest-side at 100% 50%, var(--scroll-shade), transparent) right / 14px 100% no-repeat scroll;
}
.toolbar {
  display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2) var(--space-3);
  padding: 10px 18px; border-bottom: 1px solid var(--line);
}
/* A sortable header is a button that looks like the label it replaces. */
.sort {
  all: unset; box-sizing: border-box; cursor: pointer;
  display: inline-flex; align-items: center; gap: 5px;
  font: inherit; letter-spacing: inherit; text-transform: inherit; color: inherit;
}
.sort:hover { color: var(--ink); }
.sort:focus-visible { outline: 2px solid var(--focus); outline-offset: 2px; border-radius: 2px; }
.arrow { flex: none; fill: none; stroke: currentColor; stroke-width: 1.4; stroke-linecap: round; stroke-linejoin: round; }
/* An unsorted column's arrows wait for the pointer: on every header at
   once they were a row of noise beside the labels. */
.arrow.idle { opacity: 0; transition: opacity .12s; }
.sort:hover .arrow.idle, .sort:focus-visible .arrow.idle { opacity: .6; }
@media (hover: none) { .arrow.idle { opacity: .35; } }
th[aria-sort='ascending'], th[aria-sort='descending'] { color: var(--ink); }
/* fixed, so <colgroup>'s widths bind and a long value wraps or ellipses
   inside its column instead of widening it. */
table { width: 100%; table-layout: fixed; border-collapse: collapse; font: 400 var(--text-data) var(--font-ui); }
/* This project's column label: --text-label, 600 uppercase with .06em
   tracking on --surface-2, the scale used for column and field headers alike.
   Not simply "smaller" than the previous sentence-case 11px -- these
   labels are meant to recede so the data reads first. */
th {
  text-align: left; padding: 7px 18px; color: var(--muted);
  font: 600 var(--text-label) var(--font-ui); text-transform: uppercase; letter-spacing: .06em;
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
.error { margin: 0; padding: 14px 16px; color: var(--bad-2); background: var(--bad-tint); font: 400 var(--text-body) var(--font-ui); }
.quiet { margin: 0; padding: 14px 16px; color: var(--muted); font: 400 var(--text-body) var(--font-ui); }
/* Empty: centered in the card with room around it, so "nothing" reads as
   the answer rather than a table that failed to draw its rows. */
.empty {
  margin: 0; padding: 36px 24px; text-align: center;
  display: flex; flex-direction: column; gap: 4px; align-items: center;
  color: var(--muted); font: 400 var(--text-sm) / 1.5 var(--font-ui);
}
.empty strong { color: var(--ink-2); font-weight: 500; font-size: var(--text-body); }
.loading { display: flex; align-items: center; gap: 10px; }
.loading::before {
  content: ''; width: 14px; height: 14px; flex: none; border-radius: 50%;
  border: 2px solid var(--line-2); border-top-color: var(--muted);
  animation: spin .8s linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
@media (prefers-reduced-motion: reduce) { .loading::before { animation: none; } }
</style>
