<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { routerLabel } from '@/lib/routerLabel'
import { useRoute, useRouter } from 'vue-router'
import DataTable, { type Column } from '@/components/DataTable.vue'
import WindowPicker from '@/components/WindowPicker.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import { formatCount } from '@/lib/formatCount'
import ScopePicker from '@/components/ScopePicker.vue'
import EventKindMark from '@/components/EventKindMark.vue'
import EventArchiveNote from '@/components/EventArchiveNote.vue'
import { reasonDisplay } from '@/lib/eventReason'
import { formatClock } from '@/lib/formatClock'
import { useEvents, usePeers, type EventScope } from '@/api/queries'
import type { PeerEvent } from '@/api/generated'

/** Events on screen for the chosen scope. Nothing is claimed before one
 *  exists: /v1/events refuses an unscoped request outright. */
const counts = computed(() =>
  scope.value
    ? [{ key: 'events', label: 'Events', value: formatCount(walk.rows.value.length) }]
    : [],
)

const route = useRoute()
const router = useRouter()

// The question, taken from the URL so a shared /session-history?router=..&
// peer=.. link opens on its scope. Computed, not read once: a query-only
// navigation reuses this component instead of remounting it, so the back
// button would otherwise change the address bar and leave the picker and
// table on the old scope. Copied from RoutesView.vue's own seed.
const seededRouter = computed(() =>
  typeof route.query.router === 'string' ? route.query.router : undefined,
)
const seededPeer = computed(() =>
  typeof route.query.peer === 'string' ? route.query.peer : undefined,
)

/**
 * The windows since= accepts, and the labels this screen names them by.
 *
 * api/handlers.go's since() tries RFC 3339 first and falls through to Go's
 * time.ParseDuration, which knows ns/us/ms/s/m/h and no day unit at all --
 * "7d" is a 400 an operator could not predict or explain. Modeled on
 * LookingGlassView.vue's own HISTORY_WINDOWS for the identical reason; the
 * last entry, 2160h, is this screen's own addition, matching peer_events'
 * default 90-day retention exactly so an operator can ask for everything the
 * archive still holds in one click. An operator who has raised or lowered
 * retention.days does not get this list adjusted for them, and nothing on
 * this screen states the configured value: EventArchiveNote.vue says only
 * "the configured retention (90 days by default)".
 */
const WINDOWS = [
  { value: '1h', label: 'last hour' },
  { value: '6h', label: 'last 6 hours' },
  { value: '24h', label: 'last 24 hours' },
  { value: '168h', label: 'last 7 days' },
  { value: '720h', label: 'last 30 days' },
  { value: '2160h', label: 'last 90 days' },
]

// A session's history is read after the fact, not watched live -- this
// screen starts wider than /v1/events' own 1h default (and the Looking
// glass's Changes tab) because "what happened to this session" is rarely
// answered by the last hour alone.
const since = ref('24h')
const windowLabel = computed(
  () => WINDOWS.find((w) => w.value === since.value)?.label ?? since.value,
)

/** What ScopePicker resolved: a pair with a CURRENT session behind it. */
const pickerScope = ref<EventScope | undefined>(undefined)

/**
 * What the pickers currently NAME, which is a different question -- see
 * ScopePicker's own `select` emit. `undefined` means untouched, so the URL
 * still asks the question.
 */
const picked = ref<{ router?: string; peer?: string } | undefined>(undefined)
const seedGoverns = computed(
  () =>
    picked.value === undefined ||
    (picked.value.router === seededRouter.value && picked.value.peer === seededPeer.value),
)

/**
 * Whether the seeded pair is one the picker cannot offer.
 *
 * ScopePicker lists /v1/peers, which is current-session state ("Per-(router,
 * peer, rib) state as of the current session"). This screen's question is
 * the opposite one -- what happened to a session, which most often means one
 * that has ENDED -- and /v1/events can answer it: api/openapi.yaml says this
 * walk "in either mode -- is not pinned to one BMP session ... The scoped
 * cursor carries no session", and useEvents sends router, peer and since and
 * nothing else. So the picker's inability to represent a pair was never a
 * reason this screen could not answer for it; a seeded link to an ended
 * session landed on "choose a router and peer" with the scope silently
 * discarded, while /v1/events held its history the whole time. Measured on
 * a lab archive: 10.0.103.61/10.9.9.2 has three events and appears in no
 * /v1/peers row for that router.
 *
 * Gated on an ANSWER from /v1/peers, not merely on the peer being missing
 * from it: Pinia Colada leaves `data` undefined until the first response,
 * and a response holding no rows is a different fact from no response yet.
 * Claiming "this peer has no current session" before the list arrives would
 * be asserting it of every peer, briefly, including live ones.
 *
 * usePeers here costs no second request -- its key is
 * ['peers', router, rib], the same one ScopePicker's own call resolves.
 */
const { data: livePeers } = usePeers(seededRouter, ref(undefined))
const seedIsArchived = computed(() => {
  if (pickerScope.value || !seedGoverns.value) return false
  const r = seededRouter.value
  const p = seededPeer.value
  if (!r || !p) return false
  const answered = livePeers.value?.data
  if (!answered) return false
  return !answered.some((row) => row.peer_ip === p)
})

// No `session`: there is none to carry for a peer with no current session,
// and the endpoint is not scoped by one (see EventScope's own field doc).
const scope = computed<EventScope | undefined>(() => {
  if (pickerScope.value) return pickerScope.value
  if (!seedIsArchived.value) return undefined
  return { router: seededRouter.value!, peer: seededPeer.value! }
})

const walk = useEvents(scope, since)

/**
 * useEvents is a cursor WALK: setting the scope ref asks for nothing until
 * reload() fires (the defect PeerDetailView.vue hit first and documents).
 * Keyed on the (router, peer) pair rather than on the scope object, because
 * `scope` is a computed whose identity changes on every /v1/peers poll --
 * watching the object would restart pagination on a timer.
 */
watch(
  () => (scope.value ? `${scope.value.router}|${scope.value.peer}` : undefined),
  (key) => {
    if (key) walk.reload()
  },
  { immediate: true },
)

// Same shape as RoutesView.vue's onScope: undefined arrives whenever
// ScopePicker stops representing a complete, matching pair, and clearing
// `scope` is what hides the DataTable (the v-if="!scope" guard below)
// rather than leaving a stale answer on screen under dropdowns that no
// longer agree with it.
function onScope(s: EventScope | undefined) {
  pickerScope.value = s
  syncUrl(s)
}

/**
 * The pickers taking the question over from the URL. Until this fires the
 * seeded pair governs; once the operator moves either dropdown off it, the
 * picker does -- otherwise a seeded answer would sit on screen under
 * dropdowns naming something else, which is the stale answer `onScope`'s own
 * undefined branch exists to prevent, arriving by the other door.
 */
function onSelect(v: { router?: string; peer?: string }) {
  picked.value = v
}

// since= is deliberately not carried in the URL, for the same reason
// RoutesView.vue keeps session out of it: it is how the question is asked
// today, not part of the question a shared link should pin -- a link ought
// to reopen on the same session, not necessarily the same window over it.
//
// useEvents never watches its own `since` argument (see its doc comment in
// queries.ts); reloading here is the one piece of behavior a window change
// needs that a fresh scope pick already gets from onScope above.
watch(since, () => {
  if (scope.value) walk.reload()
})

// "No scope yet" and "the scope was cleared" arrive as the same undefined,
// and only the second one should touch the URL -- see RoutesView.vue's own
// `hadScope` for the full reasoning. Copied verbatim: at mount the picker
// cannot resolve a seeded peer until /v1/peers answers, so treating that
// first undefined as an abandonment would erase the query being restored,
// destroying a shared link the moment it is opened.
let hadScope = false

function syncUrl(s: EventScope | undefined) {
  if (!s) {
    if (hadScope && (route.query.router || route.query.peer)) router.replace({ query: {} })
    return
  }
  hadScope = true
  if (route.query.router === s.router && route.query.peer === s.peer) return
  router.push({ query: { router: s.router, peer: s.peer } })
}

/**
 * The one place "which of the four kinds does this row render as" is
 * decided. The row's own `data-kind` attribute, the marker's CSS class, its
 * label and its title all read this rather than `row.kind` directly, so
 * collapsing two kinds together -- the exact mutation the view_lost and
 * unspecified tests exist to catch -- has exactly one line to touch, and
 * touching it here is what those tests are pinned against.
 */
function kindOf(row: PeerEvent): PeerEvent['kind'] {
  return row.kind
}

function rowAttrs(row: PeerEvent): Record<string, string> {
  return { 'data-kind': kindOf(row) }
}

// Six real PeerEvent fields, newest first (the walk's own order). ts_router
// is left off: it is router-reported and untrusted -- a router with a dead
// clock reports 1970 -- and this walk is keyset-ordered on ts_collector, so
// a second, uncorrelated clock beside it would invite a reader to sort by
// eye on the wrong one. There is no severity column: four kinds and a
// six-value reason registry are measured; a ranking over them would be
// editorial, inventing a shape like `{ time, sev, peer, msg, collector }`
// with no field behind either sev or msg.
// Collector is a column even though the scope is ONE (router, peer) pair,
// and that is precisely why: a scoped walk still spans collectors. Verified
// on the wire -- GET /v1/events?router=&peer= returns dev-c1's and dev-c2's
// rows interleaved, with different session_ids -- so without it this screen
// reads TWO session lifecycles as one, which is the single thing it exists
// to show. Same column, same argument, as EventsView.vue and PeersView.vue.
const columns: Column<PeerEvent>[] = [
  { id: 'ts_collector', header: 'Time', width: '180px' },
  { id: 'kind', header: 'Kind', width: '112px' },
  { id: 'collector', header: 'Collector', width: '11%' },
  { id: 'rib', header: 'RIB', width: '92px' },
  { id: 'down_reason', header: 'Reason', width: '20%' },
  // 110px holds a ten-digit 4-byte ASN such as 4200000002, 109px with its
  // padding (measured in Chromium); 84px cut it to "42000...".
  { id: 'peer_asn', header: 'ASN', numeric: true, width: '110px' },
  { id: 'router_sysname', header: 'Router', width: '16%' },
]

// Four of the seven columns are fixed px, 494px between them, and
// table-layout:fixed hands the three percentage columns only what is left:
// on a 390px phone that was nothing, and Collector, Reason and Router
// rendered 0px wide with their headers cut off. The floor holds the fixed
// columns plus the percentages taken of the floor itself (494 + 47% of
// 933 = 933), the pattern RoutersView uses. At the floor Collector's 11%
// is 102px, which holds its 99px header, and Router's 16% holds a
// 12-character sysName (123px). Reason went from 26% to 20% so that the
// floor stays under the table's 958px at a 1024px viewport; above the
// floor the browser spreads the slack over every column as before.
const SESSION_HISTORY_MIN_WIDTH = '933px'
</script>

<template>
  <section class="screen">
    <ScreenHeader
      title="Session history"
      :eyebrow="scope ? [scope.router, scope.peer] : ['choose a router and peer']"
      :counts="counts"
    />
    <!-- pins-collector is false: /v1/events takes router, peer, rib, since,
         cursor and limit -- no collector= -- and EventScope carries no
         collector field, so a scoped walk here spans collectors however
         narrow the pair looks. Saying otherwise put "walking dev-c1's view"
         directly above dev-c2's own rows. -->
    <ScopePicker
      :router="seededRouter"
      :peer="seededPeer"
      :pins-collector="false"
      @scope="onScope"
      @select="onSelect"
    >
      <template #ambiguous>
        Both collectors' events are shown here, newest first, and the
        Collector column says which saw each one — this walk is not pinned to
        either.
      </template>
    </ScopePicker>

    <!-- Why a pair is on screen that the pickers above do not list. Without
         this the table would answer for a peer the dropdowns cannot even
         show as selected, which reads as a bug rather than as the archive
         doing its job. -->
    <p v-if="seedIsArchived" class="note" data-archived-scope>
      {{ seededPeer }} has no current session on {{ seededRouter }}, so the pickers above
      cannot offer it. Its history is read from the archive all the same:
      <code>/v1/events</code> is scoped by router and peer, not pinned to a session.
    </p>

    <!-- /v1/events requires router and peer; an unscoped request is a 400.
         Rendering an empty table before a scope exists would say "this
         session has no history" about a question nobody asked. -->
    <p v-if="!scope" class="quiet">Choose a router and peer to see its session history.</p>

    <template v-else>
      <WindowPicker v-model="since" :windows="WINDOWS" />

      <DataTable
        :columns="columns"
        :rows="walk.rows.value"
        :meta="walk.meta.value"
        :loading="walk.loading.value"
        :error="walk.error.value"
        :row-attrs="rowAttrs"
        :min-width="SESSION_HISTORY_MIN_WIDTH"
      >
        <template #cell-router_sysname="{ row }">{{ routerLabel(row.router_sysname) }}</template>
        <template #cell-ts_collector="{ row }">
          <span class="mono">{{ formatClock((row as PeerEvent).ts_collector) }}</span>
        </template>
        <template #cell-collector="{ row }">
          <span class="mono">{{ (row as PeerEvent).collector }}</span>
        </template>
        <template #cell-kind="{ row }">
          <EventKindMark :kind="kindOf(row as PeerEvent)" />
        </template>
        <template #cell-down_reason="{ row }">
          <span data-reason class="mono">{{ reasonDisplay(row as PeerEvent) }}</span>
        </template>
      </DataTable>

      <button v-if="walk.hasMore.value" class="more" @click="walk.loadMore()">
        Load more
      </button>

      <!-- What this archive can and cannot say -- see EventArchiveNote.vue. -->
      <EventArchiveNote :window-label="windowLabel" />
    </template>
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 10px; }
h1 { margin: 0; font: 600 17px var(--font-ui); color: var(--ink); }
.quiet { margin: 0; color: var(--muted); font: 400 12px var(--font-ui); }
.note {
  margin: 0; padding: 9px 12px; border-radius: 6px; background: var(--accent-tint);
  color: var(--accent-ink-2); font: 400 11.5px var(--font-ui);
}
.note code { font: 400 11px var(--font-data); }
.window {
  display: flex; flex-direction: column; gap: 4px; align-self: flex-start;
  font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em;
}
.window select {
  font: 400 12px var(--font-ui); padding: 6px 9px; border: 1px solid var(--line-2);
  border-radius: 6px; background: var(--surface); color: var(--ink);
  text-transform: none; letter-spacing: normal;
}
.more { align-self: flex-start; background: var(--ink); color: var(--on-dark); border: 0; border-radius: 6px; padding: 7px 13px; font: 500 11.5px var(--font-ui); cursor: pointer; }
</style>
