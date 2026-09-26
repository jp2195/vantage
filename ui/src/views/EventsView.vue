<script setup lang="ts">
import { computed, ref } from 'vue'
import { routerLabel } from '@/lib/routerLabel'
import DataTable, { type Column } from '@/components/DataTable.vue'
import WindowPicker from '@/components/WindowPicker.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import { formatCount } from '@/lib/formatCount'
import EventKindMark from '@/components/EventKindMark.vue'
import EventArchiveNote from '@/components/EventArchiveNote.vue'
import { reasonDisplay } from '@/lib/eventReason'
import { formatClock } from '@/lib/formatClock'
import { useFleetEvents } from '@/api/queries'
import type { PeerEvent } from '@/api/generated'

/**
 * The windows this screen offers, and why they stop at 24 hours.
 *
 * /v1/events clamps an UNSCOPED since= at the daemon's max_unscoped_since --
 * 24h by default (api/config.go) -- because unscoped, peer_events is read
 * against the grain of its sort key: a wide window is a full-table read,
 * measured at 2,040,000 rows and gigabytes resident (docs/measurements.md,
 * "Unscoped events"). Offering
 * "last 90 days" here, the way Session history does, would offer a window
 * this daemon answers with a 400.
 *
 * The list is fixed at the shipped default rather than read from the server:
 * there is no endpoint that reports max_unscoped_since, so an operator who
 * raises it gets no new option here. That is a real limit, recorded rather
 * than designed around -- the wider question is available by scoping it,
 * which is what Session history is.
 *
 * An operator may just as legally LOWER max_unscoped_since, to 1h say, and
 * that direction degrades more visibly: the "last 6 hours" and "last 24
 * hours" options above are then two entries in a dropdown this screen itself
 * offers that 400 on selection. It degrades gracefully rather than
 * silently, though -- the 400 names the configured limit (see
 * checkUnscopedWindow in api/handlers.go), so the operator who lowered it is
 * told what to raise back, not left guessing why a window this screen
 * offered was refused.
 */
const WINDOWS = [
  { value: '1h', label: 'last hour' },
  { value: '6h', label: 'last 6 hours' },
  { value: '24h', label: 'last 24 hours' },
]

const since = ref('1h')
const windowLabel = computed(
  () => WINDOWS.find((w) => w.value === since.value)?.label ?? since.value,
)

const walk = useFleetEvents(since)
const rows = computed<PeerEvent[]>(() => walk.data.value?.data ?? [])

/** Events loaded, plus the matched total when the answer carried one --
 *  omitted, never zeroed, when it did not. */
const counts = computed(() => {
  const out = [{ key: 'events', label: 'Events', value: formatCount(rows.value.length) }]
  const total = walk.data.value?.meta?.total_matched
  if (total !== null && total !== undefined) {
    out.push({ key: 'total', label: 'Matching', value: formatCount(total) })
  }
  return out
})
const meta = computed(() => walk.data.value?.meta)

function rowAttrs(row: PeerEvent): Record<string, string> {
  return { 'data-kind': row.kind, 'data-router': row.router_ip }
}

// Router and peer ARE columns here and are not on Session history, which is
// the whole difference between the two screens: there, the scope is the
// question and repeating it in every row would be noise; here, WHICH peer an
// event belongs to is the answer.
//
// ts_router is still left off, and there is still no severity column, for
// SessionHistoryView.vue's own reasons -- a second, uncorrelated clock
// invites sorting by eye on the untrusted one, and a ranking over four kinds
// and a six-value registry is editorial. This screen is where a severity
// column would feel most natural and would be most invented, which is why
// test-support/columnGuard names "severity", "sev" and "msg".
//
// Collector is here for the reason PeersView.vue's own column is: /v1/events
// is one row per (collector, router, peer, session), so a router two
// collectors monitor contributes TWO rows for one event, and until
// 2026-09-20 they rendered identical in every visible column -- the same
// duplicate-row defect the Peers table's own Collector column fixed that
// day, one screen away. It sits beside the
// (router, peer) pair it qualifies, because what it says is WHOSE view of
// that pair this row is.
//
// A column rather than a merge, and the argument is stronger here than it
// was for the Peers table: view_lost is DEFINED as one collector losing its
// own BMP transport, so merging two view_lost rows would claim one event
// where two collectors each lost their own session, and the sessions are
// genuinely different (different session_id on the wire).
const columns: Column<PeerEvent>[] = [
  { id: 'ts_collector', header: 'Time', width: '180px' },
  { id: 'kind', header: 'Kind', width: '112px' },
  { id: 'router_sysname', header: 'Router', width: '13%' },
  { id: 'peer_ip', header: 'Peer', width: '13%' },
  { id: 'collector', header: 'Collector', width: '11%' },
  // 110px holds a ten-digit 4-byte ASN such as 4200000002, 109px with its
  // padding (measured in Chromium). At 84px it read "42000..." at every
  // width, 1440 included.
  { id: 'peer_asn', header: 'ASN', numeric: true, width: '110px' },
  { id: 'rib', header: 'RIB', width: '92px' },
  { id: 'down_reason', header: 'Reason', width: '18%' },
]

// Four of the eight columns are fixed px, 494px between them, and
// table-layout:fixed hands the four percentage columns only what is left:
// on a 390px phone that was nothing, and Router, Peer, Collector and Reason
// rendered 0px wide with their headers cut off. The floor holds the fixed
// columns plus the percentages taken of the floor itself (494 + 55% of
// 1098 = 1098), the pattern RoutersView uses. At the floor Peer's 13% is
// 143px, which holds 2001:db8:5549:3::1 (142px), and Router's holds a
// 12-character sysName (123px). Reason went from 22% to 18% so that the
// floor stays under the table's 1199px at a 1280px viewport; above the
// floor the browser spreads the slack over every column as before.
const EVENTS_MIN_WIDTH = '1098px'
</script>

<template>
  <section class="screen">
    <ScreenHeader
      title="Events"
      :eyebrow="['all routers', windowLabel]"
      :counts="counts"
    />
    <p class="lede">Recent events across the whole fleet, newest first.</p>

    <WindowPicker v-model="since" :windows="WINDOWS" />

    <!-- The window, stated beside the control that sets it and BEFORE the
         results rather than only after them. A console listing recent
         events looks authoritative in a way a scoped timeline does not:
         "nothing broke" and "nothing broke in the last hour" are different
         claims, and only the second is one this data supports. A reader who
         stops at the lede above should not walk away with the unbounded
         claim -- this is what pins the window to the results before the
         table, not after it. The cap is the other bound, and ResultMeta
         says that one with the real numbers, below the table.

         Deliberately OUTSIDE any v-if/v-else on the table below: an empty
         answer is exactly the case where an operator most needs to know
         what window produced it, not the case where this note should
         disappear along with the rows. -->
    <p class="window-note" data-window-note>
      Showing events from the {{ windowLabel }}, newest first. This is a
      window over the fleet, not everything the archive holds.
    </p>

    <!-- isPending, not isLoading -- see RoutersView.vue's comment on the same
         seam. useFleetEvents polls. DataTable's own blocking gate is
         `loading && rows.length === 0` (DataTable.vue), so this choice
         cannot blank an ALREADY-POPULATED table on a poll tick either way --
         that failure mode is what the gate itself already prevents. What
         isLoading would get wrong here is narrower, and still real: on an
         EMPTY answer (zero events matched the window), a poll tick flips
         isLoading true again while rows stays [], so the screen would
         flicker from "no rows matched" to "loading…" every 30 seconds -- a
         quiet, correct result restating itself as a stall. isPending stays
         false after the first response for the rest of this component's
         life, so it does not. See the test asserting this directly. -->
    <DataTable
      :columns="columns"
      :rows="rows"
      :meta="meta"
      :loading="walk.isPending.value"
      :error="walk.error.value ?? undefined"
      :row-attrs="rowAttrs"
      :min-width="EVENTS_MIN_WIDTH"
    >
      <template #cell-router_sysname="{ row }">{{ routerLabel(row.router_sysname) }}</template>
      <template #cell-collector="{ row }">
        <span class="mono">{{ (row as PeerEvent).collector }}</span>
      </template>
      <template #cell-ts_collector="{ row }">
        <span class="mono">{{ formatClock((row as PeerEvent).ts_collector) }}</span>
      </template>
      <template #cell-kind="{ row }">
        <EventKindMark :kind="(row as PeerEvent).kind" />
      </template>
      <template #cell-down_reason="{ row }">
        <span data-reason class="mono">{{ reasonDisplay(row as PeerEvent) }}</span>
      </template>
    </DataTable>

    <!-- What this archive can and cannot say -- see EventArchiveNote.vue.
         Unconditional for the same reason as the window note above. -->
    <EventArchiveNote :window-label="windowLabel" />
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 10px; }
h1 { margin: 0; font: 600 17px var(--font-ui); color: var(--ink); }
.lede { margin: 0; color: var(--muted); font: 400 12px var(--font-ui); }
.window {
  display: flex; flex-direction: column; gap: 4px; align-self: flex-start;
  font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em;
}
.window select {
  font: 400 12px var(--font-ui); padding: 6px 9px; border: 1px solid var(--line-2);
  border-radius: 6px; background: var(--surface); color: var(--ink);
  text-transform: none; letter-spacing: normal;
}
.window-note { margin: 0; color: var(--muted); font: 400 12px var(--font-ui); }
</style>
