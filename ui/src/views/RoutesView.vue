<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import DataTable, { type Column } from '@/components/DataTable.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import DumpStateMark from '@/components/DumpStateMark.vue'
import ScopePicker from '@/components/ScopePicker.vue'
import { useAsNames, useRibPage, type RibScope } from '@/api/queries'
import {
  asNamesNotice,
  asNamesPublishedLabel,
  asNamesTruncationNotice,
  indexAsNames,
  resolveAsName,
} from '@/lib/asname'
import type { UnicastRoute } from '@/api/generated'

const route = useRoute()
const router = useRouter()

// The question, taken from the URL so a shared /routes?router=..&peer=.. link
// opens on its scope. Computed, not read once: a query-only navigation reuses
// this component instead of remounting it, so the back button would otherwise
// change the address bar and leave the picker and table on the old scope.
const seededRouter = computed(() =>
  typeof route.query.router === 'string' ? route.query.router : undefined,
)
const seededPeer = computed(() =>
  typeof route.query.peer === 'string' ? route.query.peer : undefined,
)

const scope = ref<RibScope | undefined>(undefined)
const walk = useRibPage('unicast', scope)

// undefined arrives whenever ScopePicker stops representing a complete,
// matching pair -- including a router or peer change made AFTER a scope
// was already chosen, not only the initial "nothing picked yet" state.
// Clearing `scope` here is what hides the DataTable (the v-if="!scope"
// guard below) rather than leaving the previous scope's rows and
// ResultMeta on screen under dropdowns that no longer agree with them.
function onScope(s: RibScope | undefined) {
  scope.value = s
  if (s) walk.reload()
  syncUrl(s)
}

// Only router and peer reach the URL. `session` is deliberately absent: it is
// a runtime identity discovered at fetch time and used to pin the walk's
// cursor, so a link carrying one would invite a walk to resume against a
// session that may have ended -- the exact condition useRibPage's restart
// guard exists to catch. A shared link carries the question; the session is
// re-resolved from live data by whoever opens it.
//
// push for a chosen scope, so back steps between scopes an operator actually
// looked at; replace when the scope is abandoned, because "I cleared the
// picker" is not a destination worth a history entry.
// "No scope yet" and "the scope was cleared" arrive as the same undefined, and
// only the second one should touch the URL. At mount the picker cannot resolve
// a seeded peer until /v1/peers answers, so treating that first undefined as an
// abandonment would erase the query being restored -- a shared link that
// destroys itself the moment it is opened. Nothing is cleared until a scope has
// actually resolved once.
let hadScope = false

function syncUrl(s: RibScope | undefined) {
  if (!s) {
    if (hadScope && (route.query.router || route.query.peer)) router.replace({ query: {} })
    return
  }
  hadScope = true
  if (route.query.router === s.router && route.query.peer === s.peer) return
  router.push({ query: { router: s.router, peer: s.peer } })
}

// Eight columns, all real UnicastRoute fields. rib and path_id are part of
// this shape's identity, not incidental: api/openapi.yaml's /v1/rib/unicast
// description says the same (prefix, path_id) legitimately appears under
// in_pre, in_post and loc_rib for one peer -- verified against a live
// archive -- so a row missing both would render three real, distinct routes
// as one prefix repeated three times with nothing to tell them apart.
// Uptime, updates-per-second and a sparkline have no field on this shape;
// a column for any of them could only be filled by inventing a number.
// Column widths, translated to what a <col> can carry from the grid
// tracks: `minmax(130px,1.1fr) 88px 130px minmax(150px,1.3fr) 70px 56px
// 74px minmax(140px,1fr) 62px`, min-width 1150px. A <col> cannot carry
// `minmax()`, so its fr units become percentages and its fixed tracks stay
// px -- same intent, expressed in what a table accepts: numerics get just
// enough room for their digits, and the three text columns absorb the rest.
//
// Declaring them is not cosmetic. Without widths the browser's auto
// layout spreads these eight short values across a 2000px viewport, so
// reading one row means tracking the eye over 500px of white.
const columns: Column<UnicastRoute>[] = [
  { id: 'prefix', header: 'Prefix', width: '15%' },
  { id: 'rib', header: 'RIB', width: '82px' },
  { id: 'path_id', header: 'Path ID', numeric: true, width: '72px' },
  { id: 'next_hop', header: 'Next hop', width: '12%' },
  { id: 'as_path', header: 'AS path', width: '17%' },
  // Not `numeric`: once a holder name is beside it, the cell holds more
  // than a number, and right-aligned tabular figures are the wrong shape
  // for a name. Widened accordingly -- 78px fit a bare ASN, not "LEVEL3 -
  // Level 3 Parent, LLC" beside one.
  { id: 'origin_asn', header: 'Origin', width: '13%' },
  { id: 'local_pref', header: 'LocalPref', numeric: true, width: '84px' },
  { id: 'med', header: 'MED', numeric: true, width: '62px' },
  // Standard communities only, and the header says so. large_communities,
  // ext_communities and route_targets are three different BGP attributes on
  // this same row, not longer spellings of this one, and folding them into
  // one column here would report a large community as a standard one. The
  // other three have no column on this screen today.
  { id: 'communities', header: 'Communities', width: '18%' },
]

/**
 * AS holder names for the origin ASNs this walk's accumulated rows carry.
 *
 * `null` filtered out: origin_asn is null exactly when a route's AS path is
 * empty (iBGP, reflected routes -- UnicastRoute's own doc comment), which
 * is a fact about the route, not an ASN to look up. That `.filter` is
 * load-bearing rather than tidy: a null reaching the batch goes out as an
 * `asn=` value the daemon refuses with a 400, and on this ENRICHMENT
 * lookup one refusal blanks every name on the page.
 *
 * The batch is the whole accumulated walk, not one page of it, because
 * `useAsNames` dedupes and caps on its own -- see @/api/queries.ts. This
 * screen is the one most likely to REACH that cap, not the one furthest
 * from it: `useRibPage` pages at 500 and ACCUMULATES, so every "more"
 * click adds rows to the same batch, and on a transit peer's real RIB the
 * distinct origin count passes 512 within a click or two. Past the cap the
 * lookup keeps the lowest-numbered 512 and every other ASN renders bare --
 * per row, indistinguishable from one the dataset genuinely does not list.
 * `asNamesTruncNotice` below is what keeps that from being a silent lie.
 */
const originAsnsOnScreen = computed(() =>
  walk.rows.value
    .map((r) => r.origin_asn)
    .filter((asn): asn is number => asn !== null),
)
const asNamesQuery = useAsNames(originAsnsOnScreen)
const asNameIndex = computed(() => indexAsNames(asNamesQuery.data.value?.data))

/** The once-per-screen fact, never once per row -- see @/lib/asname.ts. */
const asNamesLoadNotice = computed(() => asNamesNotice(asNamesQuery.data.value?.meta))
const asNamesDate = computed(() => asNamesPublishedLabel(asNamesQuery.data.value?.meta))
/** The second once-per-screen fact: this walk outgrew one lookup's batch. */
const asNamesTruncNotice = computed(() =>
  asNamesTruncationNotice(asNamesQuery.data.value?.meta, asNamesQuery.truncated.value),
)

function holderName(asn: number | null): string | undefined {
  return asn === null ? undefined : resolveAsName(asNameIndex.value, asn)
}

/** The scope, as the eyebrow states it: the answer's own sysname when it
 *  carried one, the address the operator picked when it has not answered
 *  yet. Never both, and never a name for a router the rows do not describe. */
const eyebrow = computed<string[]>(() => {
  const s = scope.value
  if (!s) return []
  const first = walk.rows.value[0]
  return [first?.router_sysname || s.router, s.peer]
})

/** Counts the answer actually carried -- see ScreenHeader's own note on why
 *  an absent total is omitted rather than rendered as 0. total_matched is
 *  null on every RIB walk: counting a keyset walk's matches would mean a
 *  second read of the whole table. */
const counts = computed(() => {
  const out = [
    { key: 'loaded', label: 'Rows loaded', value: String(walk.rows.value.length) },
  ]
  const total = walk.meta.value?.total_matched
  if (total !== null && total !== undefined) {
    out.push({ key: 'total', label: 'Matching', value: String(total) })
  }
  return out
})
</script>

<template>
  <section class="screen">
    <ScreenHeader title="Route table" :eyebrow="eyebrow" :counts="scope ? counts : undefined" />

    <!-- The filter row is a band, not a bare row of controls: its own
         --surface-2 with a hairline under it, so the question is visibly
         separate from the answer below it, full-bleed through the
         screen's own padding. -->
    <div class="filters">
      <!-- pins-collector: /v1/rib/* takes collector= (since 2026-09-20), so this
           screen can and MUST address one collector's view -- two
           collectors' RIBs are two answers a walk cannot interleave. -->
      <ScopePicker
        :router="seededRouter"
        :peer="seededPeer"
        :pins-collector="true"
        @scope="onScope"
      />
    </div>

    <!-- /v1/rib/* require router and peer. Fetching without them would 400,
         and rendering an empty table before a scope exists would say "this
         peer has no routes" about a question nobody asked. -->
    <p v-if="!scope" class="quiet">Choose a router and peer to walk its RIB.</p>

    <template v-else>
      <p v-if="walk.restarted.value" class="restart" role="alert">
        The BMP session changed while this list was being walked, so the cursor
        no longer refers to the view it started in. The list restarted rather
        than joining rows from two different views.
      </p>

      <!-- The AS holder-name dataset's own state, said ONCE for the whole
           screen -- never once per row. The two branches are mutually
           exclusive facts, and neither prints before the batch has
           answered: no answer yet is not "not loaded" -- see
           @/lib/asname.ts. -->
      <p v-if="asNamesLoadNotice" class="quiet" data-asnames-notice>{{ asNamesLoadNotice }}</p>
      <p v-else-if="asNamesDate" class="quiet" data-asnames-date>
        AS holder names as of {{ asNamesDate }}.
      </p>
      <!-- A SECOND line, not an alternative to the two above: with a
           dataset loaded and a walk wider than one batch, "names are
           loaded as of X" is true and still incomplete. Separate rather
           than folded into the date line because a loaded dataset with no
           publication date prints no date line at all, and this fact does
           not stop being true there. -->
      <p v-if="asNamesTruncNotice" class="quiet" data-asnames-truncated>
        {{ asNamesTruncNotice }}
      </p>

      <DataTable
        :columns="columns"
        :rows="walk.rows.value"
        :meta="walk.meta.value"
        :loading="walk.loading.value"
        :error="walk.error.value"
        min-width="1150px"
        actions-width="84px"
      >
        <template #cell-as_path="{ row }">
          <span class="mono">{{ (row as UnicastRoute).as_path.join(' ') }}</span>
        </template>
        <template #cell-prefix="{ row }">
          <span class="mono">{{ (row as UnicastRoute).prefix }}</span>
          <!-- Shared with the Looking glass, which renders this same row
               shape. DumpStateMark renders all three of dump_state's
               documented values -- complete, dumping and unknown --
               rather than a literal `=== 'dumping'` test that would fold
               unknown into complete. -->
          <DumpStateMark :state="(row as UnicastRoute).dump_state" />
        </template>
        <template #cell-communities="{ row }">
          <span class="mono communities" data-communities>{{
            (row as UnicastRoute).communities.join(' ') || '—'
          }}</span>
        </template>
        <template #cell-origin_asn="{ row }">
          <span v-if="(row as UnicastRoute).origin_asn !== null" class="mono">{{
            (row as UnicastRoute).origin_asn
          }}</span>
          <!-- The WHOLE registered-holder line, never split on " - ". Absent
               for two different reasons a row does not need to tell apart --
               unlisted, or no dataset loaded -- the screen-wide note above
               is where that distinction is made. -->
          <span
            v-if="holderName((row as UnicastRoute).origin_asn)"
            class="holder"
            data-holder-name
          >{{ holderName((row as UnicastRoute).origin_asn) }}</span>
        </template>
        <!-- This button keeps the prefix text itself deliberately
             unlinked so the row stays readable. This app has no prefix
             page; the Looking glass's Paths and Changes tabs are what
             one would have held, so that is where this goes, with
             mode=prefix explicit because that screen seeds its mode
             from the query. -->
        <template #actions="{ row }">
          <RouterLink
            class="detail"
            :to="{
              path: '/looking-glass',
              query: { mode: 'prefix', q: (row as UnicastRoute).prefix },
            }"
            >Detail</RouterLink
          >
        </template>
      </DataTable>

      <!-- This footer omits a previous-page control, a virtualization
           note and a request-duration readout: this walk is
           forward-only (a keyset cursor has no previous page to ask for),
           nothing here is virtualized, and no request duration is measured,
           so the control is what ships and the readout is not invented. -->
      <footer v-if="walk.hasMore.value" class="foot">
        <button class="more" @click="walk.loadMore()">Load more</button>
      </footer>
    </template>
  </section>
</template>

<style scoped>
.screen { padding: 18px 0 20px; display: flex; flex-direction: column; gap: 12px; }
.screen > :not(.filters) { margin-inline: 18px; }
.filters {
  padding: 10px 18px; background: var(--surface-2);
  border-top: 1px solid var(--line-faint); border-bottom: 1px solid var(--line);
}
.quiet { margin: 0; color: var(--muted); font: 400 12px var(--font-ui); }
.holder { margin-left: 6px; color: var(--muted); font: 400 11px var(--font-ui); }
.communities { color: var(--ink-2); font-size: 11px; }
.foot { display: flex; justify-content: flex-end; }
.detail {
  display: inline-block; padding: 2px 8px; border: 1px solid var(--line);
  border-radius: 5px; color: var(--ink-2); text-decoration: none;
  font: 500 10.5px var(--font-ui);
}
.detail:hover { border-color: var(--ink); color: var(--ink); }
.restart { margin: 0; padding: 9px 12px; border-radius: 6px; background: var(--accent-tint); color: var(--accent-ink-2); font: 400 11.5px var(--font-ui); }
.more { align-self: flex-start; background: var(--ink); color: var(--on-dark); border: 0; border-radius: 6px; padding: 7px 13px; font: 500 11.5px var(--font-ui); cursor: pointer; }
</style>
