<script setup lang="ts">
import { computed } from 'vue'
import { routerLabel } from '@/lib/routerLabel'
import DataTable, { type Column } from '@/components/DataTable.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import { formatCount } from '@/lib/formatCount'
import { bestVantageRouters } from '@/lib/bestVantage'
import type { Router } from '@/api/generated'
import { useRouters } from '@/api/queries'
import { formatClock } from '@/lib/formatClock'

// isPending, not isLoading. Colada's isLoading aliases asyncStatus ===
// 'loading', which is true for the first fetch AND for every later
// 30-second poll refetch alike (queries.ts's pollWhileMounted comment).
// isPending aliases state.status === 'pending' -- true only until the
// first response ever lands, false for the rest of this screen's life no
// matter how many refetches follow (checked against
// ui/node_modules/@pinia/colada/dist/index.mjs, the useQuery return object).
// DataTable's `loading` prop means "there is nothing to show yet, block
// everything"; that is isPending's meaning. Wiring isLoading here instead
// would blank an already-populated table on every poll tick and print
// "loading…" directly above the still-valid "complete as of" footer from
// the last successful fetch -- the same completeness-claim-under-
// uncertainty already fixed once for the error case, just reached
// through the loading branch instead.
const { data, isPending, error } = useRouters()

const rows = computed(() => data.value?.data ?? [])

/** What the answer holds. /v1/routers returns total_matched: null -- there is
 *  no second number to show, and a rendered 0 would be a measurement. */
// bestVantageRouters, not rows.length: /v1/routers is one entry per
// (collector, router), so counting rows counts OBSERVERS -- 19 entries for
// 15 routers on a lab deployment. "Routers" is a fact about the fleet and must
// not grow because a second collector watched. The TABLE below still
// renders every entry, because two collectors' views are two real
// observations that can disagree; only the count answers a question about
// the network. Same division drawn on the Looking glass on 2026-09-20.
const counts = computed(() => [
  {
    key: 'routers',
    label: 'Routers',
    value: formatCount(bestVantageRouters(rows.value).length),
  },
])

// Peer counts stay four columns, never one. "peers: 7" would hide whether
// this router is healthy, whether the collector has gone blind to it, or
// whether nobody has heard from the collector at all -- three different
// operational situations, and only the first is fine.
const columns: Column<Router>[] = [
  { id: 'sysname', header: 'Router', width: '15%' },
  { id: 'ip', header: 'Address', width: '140px' },
  { id: 'collector', header: 'Collector', width: '14%' },
  { id: 'peers_up', header: 'Up', numeric: true, width: '70px' },
  { id: 'peers_down', header: 'Down', numeric: true, width: '76px' },
  { id: 'peers_view_lost', header: 'View lost', numeric: true, width: '86px' },
  { id: 'peers_stale', header: 'Stale', numeric: true, width: '70px' },
  { id: 'last_seen', header: 'Last seen', width: '170px' },
]

// The fixed columns alone are 612px, so below that width table-layout:fixed
// leaves the two percentage columns, Router and Collector, nothing: on a
// 390px phone both rendered 0px wide and their headers overprinted their
// neighbors. With this floor the percentages always have room, and a
// narrower screen scrolls the table inside its own card instead. The least
// floor that holds is 862px (612 / 0.71); this adds 18px. The card is 66px
// narrower than the viewport, so the table fits without scrolling from a
// 946px viewport, or 961px with a 15px classic scrollbar showing.
const ROUTERS_MIN_WIDTH = '880px'

</script>

<template>
  <section class="screen">
    <!-- Not "Collectors". /v1/routers is an inventory of the routers being
         monitored; nothing in this response measures collector health, and a
         title implying otherwise would be the screen lying about its
         source. The eyebrow says the same thing in fewer words. -->
    <ScreenHeader title="Routers" :eyebrow="['bmp routers']" :counts="counts" />
    <DataTable
      :columns="columns"
      :rows="rows"
      :meta="data?.meta"
      :loading="isPending"
      :error="error ?? undefined"
      :min-width="ROUTERS_MIN_WIDTH"
    >
      <template #cell-sysname="{ row }">{{ routerLabel(row.sysname) }}</template>
      <template #cell-last_seen="{ row }">{{ formatClock(row.last_seen) }}</template>
    </DataTable>
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 10px; }
h1 { margin: 0; font: 600 17px var(--font-ui); color: var(--ink); }
.lede { margin: 0 0 4px; font: 400 12px var(--font-ui); color: var(--muted); }
</style>
