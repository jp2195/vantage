<script setup lang="ts">
/**
 * Every state a result table in this UI can land in, on one page, so the
 * four are verifiably distinct rather than distinct in someone's memory.
 *
 * It is a DESIGN REFERENCE and it is honest about being one: each block
 * mounts the REAL DataTable in the real state, driven by props, with rows
 * from a captured fixture. Nothing here is a picture of a table -- if the
 * component's empty state ever starts looking like its loading state, this
 * page shows it.
 *
 * Four blocks, not five: `DataTable` does not distinguish "the query
 * returned nothing" from "your filters excluded everything", because
 * nothing in the API's answers does -- an empty `data` with no
 * `truncated` warning is the same shape either way. Saying so here is
 * the point of the page; drawing a fifth block whose distinction the
 * component cannot make would be the page lying about the thing it
 * exists to document.
 *
 * A "not configured -- no peers yet" block is likewise absent: it is a
 * deployment state, not a table state. Collector health
 * (CollectorsView.vue) now owns it, and owns it more precisely than one
 * block could -- `reachable: null` (no endpoint configured) and
 * `reachable: false` (configured but silent, with a reason) are two
 * different facts that screen renders differently, neither of which is a
 * DataTable state this page's four blocks could stand in for.
 */
import DataTable, { type Column } from '@/components/DataTable.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import routers from '@/api/fixtures/routers.json'
import type { Meta, Router as RouterEntity } from '@/api/generated'
import { formatClock } from '@/lib/formatClock'

const columns: Column<RouterEntity>[] = [
  { id: 'sysname', header: 'Router', width: '22%' },
  { id: 'ip', header: 'Address', width: '160px' },
  { id: 'peers_up', header: 'Up', numeric: true, width: '70px' },
  // Rendered through formatClock below, like RoutersView's own column. A
  // reference page that printed the raw instant would be teaching a format
  // no screen in this app uses.
  { id: 'last_seen', header: 'Last seen', width: '180px' },
]

const rows = routers.data.slice(0, 3) as RouterEntity[]
const complete = { warnings: [], total_matched: null } as unknown as Meta
/** A real warning body, copied from what the daemon sends rather than invented. */
const dumping = {
  warnings: [
    {
      code: 'session_dumping',
      message:
        "at least one contributing peer is still sending its initial RIB dump, so this answer is a partial view of that peer's routes rather than its full table",
    },
  ],
  total_matched: null,
} as unknown as Meta
</script>

<template>
  <section class="screen">
    <ScreenHeader title="Table states" :eyebrow="['design reference']" />
    <p class="lede">
      Every state a result table in this UI can land in, drawn by the real
      <code>DataTable</code> with the props that produce it. A state that stops being
      distinguishable from another one is visible here before it ships.
    </p>

    <section class="block" data-state="loading">
      <h2>Loading — nothing has arrived yet</h2>
      <p class="note">
        Only before the FIRST answer. A refetch of rows already on screen keeps the rows:
        <code>loading &amp;&amp; rows.length === 0</code> is the gate, so a poll tick cannot blank a
        populated table.
      </p>
      <DataTable :columns="columns" :rows="[]" :loading="true" />
    </section>

    <section class="block" data-state="empty">
      <h2>Empty — the answer carried no rows</h2>
      <p class="note">
        "No rows matched" plus the footer's completeness claim: an empty answer with no warnings is
        a claim that there is nothing there, which is a different statement from "we do not know".
        This is the one state with two distinct causes and one block — nothing in an
        API answer distinguishes an empty result from one your filters excluded.
      </p>
      <DataTable :columns="columns" :rows="[]" :meta="complete" />
    </section>

    <section class="block" data-state="error">
      <h2>Error — the query failed</h2>
      <p class="note">
        The error replaces the table, and the footer's "complete as of" is suppressed even though
        the last good answer is still in props. A completeness claim under a failure message is a
        contradiction on screen, not a nuance.
      </p>
      <DataTable
        :columns="columns"
        :rows="rows"
        :meta="complete"
        :error="new Error('rr02.fra did not respond in 30 s')"
      >
        <template #cell-last_seen="{ row }">{{
          formatClock((row as RouterEntity).last_seen)
        }}</template>
      </DataTable>
    </section>

    <section class="block" data-state="warned">
      <h2>Rows, with what the answer says about itself</h2>
      <p class="note">
        Warnings are rendered whenever a response arrived, including when there are none — an empty
        warnings array is a claim, and dropping it makes a complete answer and an unexamined one
        look identical.
      </p>
      <DataTable :columns="columns" :rows="rows" :meta="dumping">
        <template #cell-last_seen="{ row }">{{
          formatClock((row as RouterEntity).last_seen)
        }}</template>
      </DataTable>
    </section>
  </section>
</template>

<style scoped>
.screen { padding: 18px; display: flex; flex-direction: column; gap: 16px; }
.lede { margin: 0; max-width: 84ch; color: var(--muted); font: 400 12px var(--font-ui); }
.block { display: flex; flex-direction: column; gap: 8px; }
h2 { margin: 0; font: 600 13px var(--font-ui); color: var(--ink); }
.note { margin: 0; max-width: 84ch; color: var(--muted); font: 400 11px var(--font-ui); }
code { font: 400 11px var(--font-data); color: var(--ink-2); }
</style>
