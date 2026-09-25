<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import DataTable, { type Column } from '@/components/DataTable.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import { formatCount } from '@/lib/formatCount'
import { formatRate } from '@/lib/formatRate'
import { formatClock } from '@/lib/formatClock'
import StatePill from '@/components/StatePill.vue'
import WindowPicker from '@/components/WindowPicker.vue'
import PeerSparkline from '@/components/PeerSparkline.vue'
import { churnKey, useAsNames, useChurnPeersByCollector, usePeers } from '@/api/queries'
import {
  asNamesNotice,
  asNamesPublishedLabel,
  asNamesTruncationNotice,
  indexAsNames,
  resolveAsName,
} from '@/lib/asname'
import type { ChurnActivity, Peer } from '@/api/generated'

// usePeers takes router and rib filters. `router` is now driven, by the
// query string rather than by a control on this screen: Monitor's dumps
// and sessions rows link here as /peers?router=<ip>, so a click that named
// a router arrives at a list that answers for that router. It stays a
// computed off the route rather than a value read once at setup, because
// usePeers keys its query on router.value (queries.ts) -- a frozen ref
// would leave the table showing the router the URL named a navigation ago.
//
// `rib` is still sent as an unassigned ref, and deliberately: nothing
// links here with one, and a second half-wired filter is what the two refs
// this replaced were removed for.
const route = useRoute()
const scopedRouter = computed(() =>
  typeof route.query.router === 'string' ? route.query.router : undefined,
)

// isPending, not isLoading -- see RoutersView.vue's comment on the same
// seam. usePeers polls through the same pollWhileMounted timer useRouters
// does (queries.ts), so isLoading flips true again on every 30s refetch,
// not only the first fetch. isPending is the one that stays false once the
// first response has ever landed, which is what DataTable's `loading` prop
// actually means: "block everything, there is nothing to show yet."
const { data, isPending, error } = usePeers(scopedRouter, ref(undefined))

// Eight columns, because eight is what the API measures. Updates-per-
// second and a sparkline are still absent from this response body; a
// column for them could only be filled by inventing numbers, and they
// belong to the churn port rather than to /v1/peers.
//
// Its "uptime" is the one that moved, and not by being copied. /v1/peers
// gained `up_since` on 2026-09-21 after a measurement found an
// established duration unsupportable on a lab archive -- see the
// column's own comment below.
//
// Collector is one of the seven because /v1/peers is one entry per
// (collector, router, peer, rib): a router two collectors monitor
// contributes TWO rows for one peer, and until 2026-09-20 they rendered as
// duplicates identical in every visible column. Found in a browser against
// a lab deployment, where seven pairs did exactly that.
//
// A column rather than a merge, deliberately. The two rows are two real
// observations that can disagree about session and state -- one collector
// up while the other has gone view_lost -- so collapsing them would present
// an arbitrary collector's answer as THE peer's, which is the unstated
// pick this project audits for. ScopePicker annotates the same condition
// two screens away and this is the same claim in table form.
const columns: Column<PeerRow>[] = [
  // Percentages summing to 100, not a mix of px and %: a mix over-
  // constrains the table below ~1400px, and the browser absorbs it by
  // shrinking columns rather than truncating, so nothing would break on
  // a lab archive's short addresses today -- but MonitorView already
  // recorded what it costs once content is longer ("the Peer column
  // rendered '17...' for 172.31.0.90"). Normalized here rather than
  // waiting for an AS holder name to find it.
  //
  // Peer is the widest column because an address cannot wrap: at 12% it
  // cut 2001:db8:5549:3::1 off at every width. 19% holds a 23-character
  // address (202px), the length of an IXP peering-LAN address such as
  // 2001:7f8:1::a500:6939:1, at the floor below, and more above it. The
  // seven points came from Collector, ASN, State, Up since and Changes,
  // each still at least as wide as its header and its widest value at the
  // floor.
  { id: 'peer_ip', header: 'Peer', width: '19%' },
  { id: 'router_ip', header: 'Router', width: '11%' },
  { id: 'collector', header: 'Collector', width: '9%' },
  { id: 'rib', header: 'RIB', width: '7%' },
  // Not `numeric`: once a holder name is beside it, the cell holds more
  // than a number, and right-aligned tabular figures are the wrong shape
  // for a name. Widened accordingly -- 92px fit a bare ASN, not "LEVEL3 -
  // Level 3 Parent, LLC" beside one. The name wraps; the ten digits of the
  // longest ASN do not, and need 109px.
  { id: 'asn', header: 'ASN', width: '11%' },
  // The widest pill, "view lost", needs 95px; the dumping chip wraps below it.
  { id: 'state', header: 'State', width: '9%' },
  { id: 'routes', header: 'Routes', numeric: true, width: '7%' },
  // "Up since", not "Uptime", and the difference is measured rather than
  // stylistic. A measurement on 2026-09-21 ran `now - up_since` over a
  // lab archive: for 25 of 35 peers it equaled how long the ROUTER had
  // been SILENT, to the decimal. A lab torn down rather than shut down
  // sends no Peer Down, so the last state stays `up` forever and the
  // subtraction reports archive silence as a live session -- the
  // collection-artifact-as-network-fact class, in a column an operator would
  // trust. The instant is a fact the archive holds; the duration is an
  // inference about the present it cannot support.
  { id: 'up_since', header: 'Up since', width: '11%' },
  // Named for the field it carries -- changes_per_second, off
  // /v1/collection/churn/peers -- not for its header. The column guard
  // checks ids against the shapes real endpoints return, so an id invented
  // to read nicely would make this the one column nothing backs.
  //
  // Archived/s, never Updates/s: this column is a collection rate, not
  // the router's own update rate -- what it holds is rows THIS collector
  // stored. Reporting it as the network's is the artifact-as-fact
  // defect, and the note beside the table says which of the two this is.
  { id: 'changes_per_second', header: 'Archived/s', numeric: true, width: '8%' },
  // The shape the rate beside it averages away: two peers with one rate may
  // have churned steadily and all at once. Named for the field it carries,
  // like its neighbor.
  { id: 'activity', header: 'Changes', width: '8%' },
]

// Percentages alone shrink every column with the screen: on a 390px phone
// the columns came out 22-40px wide, nine of ten headers were cut off and
// every state pill was clipped. The column that needs the widest table is
// Peer, 202px for a 23-character address at 19%, which needs a 1064px
// table, so the floor is exactly that. State's 95px pill at 9% is next at
// 1056px, then Archived/s, 84px of header label and padding at 8%, at
// 1050px. The card is 66px narrower than the viewport, so the table fits
// without scrolling from a 1130px viewport, or 1145px with a 15px classic
// scrollbar showing. A narrower screen scrolls the table inside its own
// card.
const PEERS_MIN_WIDTH = '1064px'

// Same three the other windowed screens offer, and the same reason: every
// /v1/collection/* endpoint refuses a window past the daemon's
// max_unscoped_since (24h by default), and scoping by router buys no
// exemption -- it narrows a WHERE clause without pinning a keyset walk.
// Probed against the running daemon rather than assumed: 168h answers
// "since= reaches further back than 24h0m0s".
const WINDOWS = [
  { value: '1h', label: 'last hour' },
  { value: '6h', label: 'last 6 hours' },
  { value: '24h', label: 'last 24 hours' },
]
const since = ref('1h')
const windowLabel = computed(() => WINDOWS.find((w) => w.value === since.value)?.label ?? since.value)

/**
 * The collectors whose rows are on screen, which is what the churn join is
 * fetched for -- one request each. Taken from the rendered rows rather than
 * from /v1/collectors so the join asks about exactly the vantage points it
 * is about to render, and no others.
 */
const collectorsOnScreen = computed(() =>
  Array.from(new Set((data.value?.data ?? []).map((p) => p.collector))),
)
// Scoped by the same router the table is, so a router-scoped screen asks a
// router-scoped question. Unscoped it was a whole-archive aggregation per
// collector, re-issued every 30s to fill a handful of cells -- and the
// endpoint's own answer is allowed to truncate, which on a narrow screen
// would drop the very peers being rendered and leave `?? 0` reporting them
// as a measured zero.
const churn = useChurnPeersByCollector(since, collectorsOnScreen, scopedRouter)

/**
 * This row's archived rate, or undefined when there is no measurement to
 * report yet.
 *
 * The three states are deliberately distinct. A peer the answer does not
 * mention archived nothing in the window -- the endpoint ranks by change and
 * omits the quiet -- so that is a real 0. Before an answer arrives, or after
 * a failed one, there is no measurement at all, and rendering 0 then would
 * manufacture one.
 */
/**
 * This row's churn answer, or undefined when there is none to read.
 *
 * ONE lookup for both joined columns. They had two: the rate checked
 * `failed` and returned undefined for a collector whose request died, while
 * the sparkline read the map directly and rendered the "changed nothing"
 * dash for the same row. Two rules for one join is how they drift -- the
 * moment either changes, one column starts presenting another collector's
 * answer as this row's.
 */
function churnFor(p: Peer) {
  const answer = churn.data.value
  if (!answer) return undefined
  // A collector whose own request failed has no measurement for its rows --
  // not a zero. Only the collectors that actually answered get the "absent
  // means archived nothing" reading.
  if (answer.failed.includes(p.collector)) return undefined
  return answer.by.get(churnKey(p.collector, p.router_ip, p.peer_ip)) ?? null
}

function archivedRate(p: Peer): number | undefined {
  const row = churnFor(p)
  if (row === undefined) return undefined
  return row?.changes_per_second ?? 0
}

/** The window the churn answers covered, which is where the bars are placed. */
const churnWindow = computed(() => churn.data.value?.window)

/** Collectors whose rate request failed, for the one sentence that says so. */
const churnFailed = computed(() => churn.data.value?.failed ?? [])


/**
 * A peer row joined with the churn answer for ITS OWN collector.
 *
 * The rate lives on the row rather than being looked up in the cell slot, so
 * the join happens once per render in one place -- and so the column can be
 * named for the field it carries, which is what keeps the column guard
 * meaningful. Same shape MonitorView's ranked table uses for the same join.
 *
 * Optional, because undefined and 0 are different answers here: see
 * archivedRate.
 */
type PeerRow = Peer & { changes_per_second?: number; activity?: ChurnActivity[] }

const rows = computed<PeerRow[]>(() =>
  (data.value?.data ?? []).map((p) => ({
    ...p,
    changes_per_second: archivedRate(p),
    activity: churnFor(p)?.activity,
  })),
)

/**
 * AS holder names for every ASN this table is about to render.
 *
 * The batch is the answer's own rows, not a fixed page size -- usePeers
 * already caps what lands here, so "every ASN currently on screen" is never
 * unbounded. See @/api/queries.ts's useAsNames for why this needs no timer
 * of its own: the batch changes the moment usePeers' own 30s poll hands
 * this a new row, and that change is what refetches it.
 */
const asnsOnScreen = computed(() => (data.value?.data ?? []).map((p) => p.asn))
const asNamesQuery = useAsNames(asnsOnScreen)
const asNameIndex = computed(() => indexAsNames(asNamesQuery.data.value?.data))

/** The once-per-screen fact, never once per row -- see @/lib/asname.ts. */
const asNamesLoadNotice = computed(() => asNamesNotice(asNamesQuery.data.value?.meta))
const asNamesDate = computed(() => asNamesPublishedLabel(asNamesQuery.data.value?.meta))
/**
 * The second once-per-screen fact. This screen is the LEAST likely of the
 * three to reach the cap -- usePeers answers one row per BGP session, so
 * the batch is bounded by fleet size -- but "unlikely here" is exactly the
 * reasoning that left RoutesView unguarded, and the rule is one function
 * in @/lib/asname.ts precisely so no screen has to re-derive it.
 */
const asNamesTruncNotice = computed(() =>
  asNamesTruncationNotice(asNamesQuery.data.value?.meta, asNamesQuery.truncated.value),
)

function holderName(asn: number): string | undefined {
  return resolveAsName(asNameIndex.value, asn)
}

/** Peers in this answer -- the whole fleet's, or one router's when the
 *  query narrowed it, which the note beside this says out loud. */
const counts = computed(() => [
  { key: 'peers', label: 'Peers', value: formatCount(data.value?.data?.length ?? 0) },
])

/**
 * Whether the Up since cell shows the instant. up, and stale too: a stale
 * peer is the same last-known session, whose routes are still served, so its
 * up_since is the start of what is on screen. down and view_lost are not:
 * the session has ended, or nobody can see it, so there is no ongoing state
 * for the instant to belong to.
 */
function showsUpSince(state: Peer['state']): boolean {
  return state === 'up' || state === 'stale'
}

function dumping(p: Peer): boolean {
  return Object.values(p.dump_states).includes('dumping')
}
</script>

<template>
  <section class="screen">
    <ScreenHeader
      title="Peers"
      :eyebrow="scopedRouter ? ['bgp peers', scopedRouter] : ['bgp peers', 'all routers']"
      :counts="counts"
    />
    <!-- The AS holder-name dataset's own state, said ONCE for the whole
         screen -- never once per row, which is why this lives beside the
         table rather than inside the asn cell slot below. The two branches
         are mutually exclusive facts: either no dataset is loaded at all,
         or one is and this is when its source published it. Neither prints
         when the batch has not answered yet, and no answer is not "not
         loaded" -- see @/lib/asname.ts. -->
    <p v-if="asNamesLoadNotice" class="note" data-asnames-notice>{{ asNamesLoadNotice }}</p>
    <p v-else-if="asNamesDate" class="note" data-asnames-date>
      AS holder names as of {{ asNamesDate }}.
    </p>
    <!-- A SECOND line, not an alternative to the two above: a loaded
         dataset and a batch wider than one lookup are both true at once,
         and only the first of them is otherwise said out loud. -->
    <p v-if="asNamesTruncNotice" class="note" data-asnames-truncated>{{ asNamesTruncNotice }}</p>
    <!-- The column's own limit, said once for the screen rather than once
         per row, the same way the AS-name facts above are. It is not
         decoration: the difference between "came up then" and "has been up
         since then" is the whole reason this column is an instant. -->
    <WindowPicker v-model="since" :windows="WINDOWS" />
    <!-- The window named beside the rate it scopes, and the rate named for
         what it measures. Both halves are required of any rate shown
         here: a rate whose bounds are not on screen is an unlabeled
         window, and a collection rate presented as the network's own
         misstates what was measured. -->
    <!-- Said, not swallowed. Without this the failed collector's rows show
         the same dash as a peer with no measurement, and a dash that means
         two different things is the collapse this project audits for. -->
    <p v-if="churnFailed.length || churn.error.value" class="note error" data-churn-error role="alert">
      No archived rate for
      {{ churnFailed.length ? churnFailed.join(', ') : 'this window' }} — that
      request failed, so those rows show no rate rather than a zero.
    </p>
    <p class="note" data-archived-note>
      Archived/s is rows this collector stored per second over the
      {{ windowLabel }} — a collection rate, not the router's update rate. It
      is measured per peer, across whatever RIB views that peer is mirrored
      under.
    </p>
    <p class="note" data-up-since-note>
      Up since is when this collector last saw the session come up, and it is
      shown only while that session is up or stale: a stale peer is the same
      last-known session, whose routes are still shown, while a peer now down
      or view_lost has an up to report and no ongoing state to report it as.
      It is still not a liveness claim — a session the collector has stopped
      hearing from shows the same value forever.
    </p>
    <!-- A list narrowed to one router looks exactly like a complete one.
         Saying which router it holds is the difference between a scoped
         answer and a quietly partial one; the way back to the fleet is the
         nav's own Peers link, which carries no query.

         Gated on `!error` as well as on the scope, because the note is a
         claim about rows. A failed request has none -- DataTable renders
         its error where they would be -- so the scope alone would print
         "Showing the peers of foo only" above "no such router": a narrowing
         claimed over an answer that does not exist. -->
    <p v-if="scopedRouter && !error" class="note" data-scope-note>
      Showing the peers of {{ scopedRouter }} only, not the whole fleet.
    </p>
    <DataTable
      :columns="columns"
      :rows="rows"
      :meta="data?.meta"
      :loading="isPending"
      :error="error ?? undefined"
      :min-width="PEERS_MIN_WIDTH"
    >
      <!-- The same formatClock every other screen's timestamps go through:
           one clock rendering in this app, not a second one that drifts.
           Null is the wire's "this session never came up" and gets the dash,
           never a date -- rendering it would print 1970 as an observation. -->
      <template #cell-activity="{ row }">
        <PeerSparkline
          :activity="(row as PeerRow).activity ?? []"
          :from="churnWindow?.from"
          :to="churnWindow?.to"
          :bucket="churnWindow?.bucket"
        />
      </template>
      <!-- formatRate, not toFixed: see its own comment. Two decimals alone
           drew this lab's real rates as "0.00", the value that means the
           peer archived nothing. -->
      <template #cell-changes_per_second="{ row }">
        <span
          v-if="(row as PeerRow).changes_per_second !== undefined"
          data-archived
          class="mono"
          >{{ formatRate((row as PeerRow).changes_per_second) }}</span
        >
        <span v-else data-archived class="quiet">—</span>
      </template>
      <template #cell-up_since="{ row }">
        <span
          v-if="(row as Peer).up_since && showsUpSince((row as Peer).state)"
          data-up-since
          class="mono"
          >{{
          formatClock((row as Peer).up_since!)
        }}</span
        >
        <span v-else data-up-since class="quiet">—</span>
      </template>
      <template #cell-state="{ row }">
        <StatePill :state="(row as Peer).state" />
        <!-- A peer mid-dump has a real route count that is still climbing.
             Marking the row is what stops the number reading as final. -->
        <span
          v-if="dumping(row as Peer)"
          class="provisional"
          title="This peer is still sending its initial RIB dump, so its route count is provisional."
        >dumping</span>
      </template>
      <template #cell-peer_ip="{ row }">
        <RouterLink
          class="mono link"
          :to="`/peers/${(row as Peer).router_ip}/${(row as Peer).peer_ip}`"
          :title="(row as Peer).peer_ip"
        >{{ (row as Peer).peer_ip }}</RouterLink>
      </template>
      <!-- Titled like the peer beside them, because both can outgrow their
           column: a default Helm install's vantage-collector-0 ellipsizes
           in Collector at every width, and an IPv6 router address does in
           Router. -->
      <template #cell-router_ip="{ row }">
        <span data-router :title="(row as Peer).router_ip">{{ (row as Peer).router_ip }}</span>
      </template>
      <template #cell-collector="{ row }">
        <span data-collector :title="(row as Peer).collector">{{ (row as Peer).collector }}</span>
      </template>
      <template #cell-asn="{ row }">
        <span class="mono">{{ (row as Peer).asn }}</span>
        <!-- The WHOLE registered-holder line, never split on " - ": some of
             RIPE's own lines carry more than one such separator, and any
             split guesses at a boundary the dataset does not mark. Absent
             for two different reasons a row does not need to tell apart --
             this ASN is genuinely unlisted, or no dataset is loaded -- the
             screen-wide note above is where that distinction is made. -->
        <span
          v-if="holderName((row as Peer).asn)"
          class="holder"
          data-holder-name
        >{{ holderName((row as Peer).asn) }}</span>
      </template>
    </DataTable>
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 10px; }
h1 { margin: 0; font: 600 17px var(--font-ui); color: var(--ink); }
.note { margin: 0; max-width: 72ch; color: var(--muted); font: 400 12px var(--font-ui); }
.link { color: var(--ink-2); }
.holder { margin-left: 6px; color: var(--muted); font: 400 11px var(--font-ui); }
.provisional {
  margin-left: 7px; padding: 1px 6px; border-radius: 4px;
  background: var(--neutral-chip); color: var(--muted);
  font: 500 10px var(--font-ui);
}
</style>
