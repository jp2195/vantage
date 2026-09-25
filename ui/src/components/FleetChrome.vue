<script setup lang="ts">
/**
 * The chrome's right cluster: how many collectors answered, when this page
 * last received an answer, and the newest row the archive holds.
 *
 * A component of its own, rather than markup inside AppShell, because it is
 * the one piece of the chrome that READS something. AppShell held the query
 * for a commit and every test that mounts the shell -- including App's boot
 * tests, which reach it through the boot sequence -- immediately needed an
 * active Pinia for Colada's useQuery, on paths that render before any query
 * context exists. Keeping the read in a child means the shell stays
 * presentational and stubbable, and this component is mounted only inside
 * the authenticated branch, so it never fires a request before the client
 * has a credential.
 *
 * Absent renders nothing. See useFleetChrome for why a count of 0 and a
 * missing stamp are not the same claim as "0 collectors" and "never".
 *
 * Two stamps, not one. `updated` is when this page last RECEIVED an answer,
 * on the browser's clock, and is deliberately not a server clock: this API
 * returns none, and the browser's is a different clock from the
 * collector's. But receipt time reads as freshness, and on a quiet fleet
 * it is not -- such a fleet archives around 21 rows a day, so "updated"
 * can sit at the current second over an archive whose newest row is
 * hours old. So the archive's own newest instant is named beside it, on
 * the collector's clock, as its own labeled fact: the newest
 * `archive.last_row_at` across `/v1/collectors`, the newest row any
 * collector wrote to any data table. Not a router's `last_seen`, which is
 * only the newest peer event and stands still while route rows keep
 * landing. Each stamp is true about a different thing, and folding them
 * into one number would lose whichever the reader needed.
 */
import { formatFreshness, formatTimeOfDay } from '@/lib/formatClock'
import { useFleetChrome } from '@/api/queries'

const { collectors, updatedAt, newestRow } = useFleetChrome()
</script>

<template>
  <div class="cluster">
    <span v-if="collectors !== undefined" class="meta" data-collectors>
      {{ collectors }} {{ collectors === 1 ? 'collector' : 'collectors' }}
    </span>
    <span v-if="updatedAt" class="meta" data-updated>updated {{ formatTimeOfDay(updatedAt) }}</span>
    <span v-if="newestRow" class="meta" data-newest>newest row {{ formatFreshness(newestRow) }}</span>
  </div>
</template>

<style scoped>
/* Wraps so that on a phone each stamp drops to its own line rather than
   running off the bar. */
.cluster { margin-left: auto; display: flex; flex-wrap: wrap; justify-content: flex-end; align-items: center; gap: 2px 12px; }
.meta { font: 400 11.5px var(--font-data); color: var(--chrome-text-2); white-space: nowrap; }
</style>
