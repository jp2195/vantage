<script setup lang="ts">
import { computed, ref } from 'vue'
import DataTable, { type Column } from '@/components/DataTable.vue'
import EventKindMark from '@/components/EventKindMark.vue'
import ChurnChart from '@/components/ChurnChart.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import StatePill from '@/components/StatePill.vue'
import StatTile from '@/components/StatTile.vue'
import WindowPicker from '@/components/WindowPicker.vue'
import { formatClock } from '@/lib/formatClock'
import { formatCount } from '@/lib/formatCount'
import { formatRate } from '@/lib/formatRate'
import { bestVantageRouters } from '@/lib/bestVantage'
import { parseGoDuration } from '@/lib/goDuration'
import { hasDownReason, reasonDisplay } from '@/lib/eventReason'
import {
  useCollectionDumps,
  useCollectionFlags,
  useCollectionLocRib,
  useCollectionSessions,
  useCollectionChurn,
  useFleetEvents,
  useRouters,
  useCollectionChurnPeers,
  usePeers,
} from '@/api/queries'
import type {
  FlagCount,
  PeerEvent,
  PeerLocRib,
  Router,
  RouterDumpCount,
  RouterSessionCount,
  PeerChurn,
  Peer,
} from '@/api/generated'

/**
 * Four sections, four composables, four independent loading and error
 * states -- that independence is the entire reason /v1/collection/* is
 * shipped as four endpoints instead of one merged answer, and it has to be
 * visible in this markup or the API's own shape was pointless. Each
 * <DataTable> below reads only its own composable's `isPending` and
 * `error`; nothing here gates one section's rendering on another's status,
 * so a single endpoint going down cannot blank the other three.
 *
 * THERE IS NO HEALTH SCORE. This screen answers "is collection healthy?"
 * with four measured signals -- dump composition, session churn, Loc-RIB
 * coverage, parse-flag counts -- plus one arithmetic ratio a single row
 * already carries (the dump composition percentage below). A composite
 * across all four would be an invented fifth signal wearing the others'
 * credibility; test-support/columnGuard.ts's term list now blocks a column
 * named for one, and MonitorView.test.ts checks the rendered page directly
 * for the same thing.
 */

// Same three options and the same reason EventsView.vue offers them: none
// of these four endpoints has a scoped mode that exempts a wide window from
// the daemon's max_unscoped_since clamp (24h by default) -- router= narrows
// every one of their WHERE clauses but pins no keyset walk, so it buys no
// exemption the way naming (router, peer) does for /v1/events. Offering
// "last 90 days" here would be a window every one of the four refuses with
// a 400.
const WINDOWS = [
  { value: '1h', label: 'last hour' },
  { value: '6h', label: 'last 6 hours' },
  { value: '24h', label: 'last 24 hours' },
]

const since = ref('1h')
const windowLabel = computed(
  () => WINDOWS.find((w) => w.value === since.value)?.label ?? since.value,
)

const dumps = useCollectionDumps(since)
const sessions = useCollectionSessions(since)
const locrib = useCollectionLocRib(since)
const flags = useCollectionFlags(since)
// Two more answers shown beside the four signals: the fleet's recent
// peer events, and the CURRENT peer state, which is a different fact
// from the window's session count -- one says what is up now, the
// other how many session lifecycles the window contains.
const events = useFleetEvents(since)

/**
 * The update-churn panel. The bucket is chosen from the window so
 * a bar is always a readable slice of it rather than a fixed width that is
 * 12 bars at one setting and 288 at another -- and the CHART renders the
 * width the answer reports, not this request, because the two are only the
 * same until a clamp disagrees.
 */
const churnBucket = computed(() => {
  switch (since.value) {
    case '1h':
      return '2m'
    case '6h':
      return '10m'
    default:
      return '30m'
  }
})
const churn = useCollectionChurn(since, churnBucket)

// The ranked peer table reads TWO answers. Churn knows how much a peer sent
// inside the window; /v1/peers knows what it is holding and what state it is
// in now. Neither endpoint carries the other's half, and joining them here
// rather than server-side keeps each one answerable on its own.
const churnPeers = useCollectionChurnPeers(since)
const joinPeers = usePeers(ref(undefined), ref(undefined))

/**
 * /v1/peers rows grouped by (router, peer) -- BOTH, never peer_ip alone.
 * The same peer address appears under different routers across a fleet
 * (0.0.0.0 is a self-peer every router has), so a join keyed on the peer
 * would attach one router's state to another's churn.
 *
 * A LIST per key, not a row: api/openapi.yaml documents /v1/peers as "one
 * entry per (collector, router, peer, rib)", so a peer mirrored pre- and
 * post-policy has two rows with two real, different route counts. They are
 * not addable -- the same prefixes counted twice -- and picking one is an
 * unstated choice of the kind this project audits for. `peerFacts` below
 * returns nothing rather than resolve it, and the table says so.
 */
const peersByPair = computed(() => {
  const by = new Map<string, Peer[]>()
  for (const p of (joinPeers.data.value?.data ?? []) as Peer[]) {
    const key = `${p.router_ip}|${p.peer_ip}`
    const group = by.get(key)
    if (group) group.push(p)
    else by.set(key, [p])
  }
  return by
})

/**
 * One ranked row: the churn answer, plus the two facts /v1/peers holds and
 * churn does not.
 *
 * Both added fields are REAL fields of a real response -- Peer.routes and
 * Peer.state, straight off /v1/peers -- which is what lets the column guard
 * keep meaning something here. A row type carrying values this screen
 * computed would make that guard vacuous: it checks a column's id against
 * the fields an API actually returned, precisely so a column cannot be
 * backed by an invention.
 *
 * Both are OPTIONAL because the join does not always resolve. `multiRib` is
 * not a column and never renders as a number; it is why the other two are
 * absent.
 */
type RankedPeer = PeerChurn & {
  routes?: Peer['routes']
  state?: Peer['state']
  multiRib: boolean
}

/**
 * The ranking as rendered: the answer's own order, with the join applied
 * per row.
 *
 * The ORDER IS NOT RECOMPUTED. /v1/collection/churn/peers ranks by
 * readvertise + withdraw in SQL, and re-sorting here -- on the total, say --
 * would put a peer whose session restarted once above a genuinely busy one,
 * which is the exact ordering the endpoint went out of its way to avoid.
 */
const churnPeerRows = computed<RankedPeer[]>(() =>
  ((churnPeers.data.value?.data ?? []) as PeerChurn[]).map((row) => {
    const group = peersByPair.value.get(`${row.router_ip}|${row.peer_ip}`)
    const one = group?.length === 1 ? group[0] : undefined
    return {
      ...row,
      routes: one?.routes,
      state: one?.state,
      multiRib: (group?.length ?? 0) > 1,
    }
  }),
)

/** Two decimals under 10/s, none above: a peer doing 0.03/s and one doing
 *  412/s are both readable, and neither is padded with false precision. */



// ASN and the addresses come from the churn answer itself, which carries
// peer_asn -- no join needed for them. There is no AS HOLDER NAME column
// (e.g. "Lumen", "NTT"): the churn answer carries none, and this table does
// not fetch GET /v1/asnames the way the Peers and Routes tables do. A column
// filled from a guess is the invention this screen's column guard exists to
// catch.
// WIDTHS SUM TO 100%, and they are percentages rather than the mix of px and
// % the other tables on this screen use. Found in a browser: seven columns at
// 84/104/104/104/132px plus two percentages over-constrain a panel in this
// grid -- the fixed columns alone are 528px of a ~642px panel -- so
// table-layout:fixed shrank the rest and the Peer column rendered "17..." for
// 172.31.0.90 while Router rendered "4...". Every test passed; jsdom computes
// no layout.
//
// Percentages that total 100 are the fix that holds at BOTH ends: the panel is
// one column of an auto-fit grid, so its width is whatever the viewport leaves
// it, and a declared set that sums over 100% clips at the narrow end while one
// that sums under it smears at the wide end. churnPeerWidthsSumTo100 in the
// test file pins it.
// Floors for the tables, so that on a phone a table scrolls inside its own
// card instead of shrinking until its addresses read "172.2...". Each sits
// below the narrowest width its panel has on a desktop, so neither binds
// there: a grid panel is at least 620px, which leaves its table about
// 590px, and the full-width panel is wider still, 925px at a 1024px
// viewport. 920px for the seven columns below: ASN's 12% has to hold a
// ten-digit 4-byte ASN such as 4200000002, 109px with its padding (measured
// in Chromium), and at the old 760px floor it got 91px and read "42000...".
const PANEL_TABLE_MIN_WIDTH = '560px'
const WIDE_TABLE_MIN_WIDTH = '920px'

const churnPeerColumns: Column<RankedPeer>[] = [
  // 12% for the ASN: 8% of a 641px panel is 51px, which clips even 65010,
  // and a 4-byte AS is ten digits.
  { id: 'peer_asn', header: 'ASN', numeric: true, width: '12%' },
  { id: 'peer_ip', header: 'Peer', width: '20%' },
  { id: 'router_sysname', header: 'Router', width: '18%' },
  { id: 'routes', header: 'Prefixes', numeric: true, width: '12%' },
  { id: 'changes_per_second', header: 'Archived/s', numeric: true, width: '12%' },
  { id: 'withdraw', header: 'Withdraws', numeric: true, width: '12%' },
  { id: 'state', header: 'State', width: '14%' },
]

/**
 * The window the chart draws, as instants -- FROM THE ANSWER.
 *
 * `meta.churn_from` / `meta.churn_to` are the bounds the daemon actually
 * used, on its own clock, and they are what the axis is drawn between. A
 * local subtraction off `new Date()` would draw COLLECTOR timestamps against
 * the BROWSER's clock: every `ts` in the answer is a ts_collector, so a
 * laptop a few minutes off would shift every bar against its own gridline
 * with nothing on screen able to show it. Same rule the bucket
 * width already follows -- render what the answer reported, never what the
 * request asked for.
 *
 * The local fallback stays for the case the daemon sends no bounds (an older
 * build, or a churn error where meta never arrives). It is worse, and it is
 * better than an axis with no span at all: without a window ChurnChart draws
 * nothing, so the alternative to an approximate axis here is a blank panel.
 */
const windowEnd = computed(() => {
  const to = churn.data.value?.meta?.churn_to
  if (to) return to
  return new Date().toISOString()
})
const windowStart = computed(() => {
  const from = churn.data.value?.meta?.churn_from
  if (from) return from
  const ms = parseGoDuration(since.value === '1h' ? '1h0m0s' : since.value === '6h' ? '6h0m0s' : '24h0m0s')
  return new Date(Date.parse(windowEnd.value) - (ms ?? 3_600_000)).toISOString()
})
const routers = useRouters()

const dumpRows = computed<RouterDumpCount[]>(() => dumps.data.value?.data ?? [])
const sessionRows = computed<RouterSessionCount[]>(() => sessions.data.value?.data ?? [])
const locribRows = computed<PeerLocRib[]>(() => locrib.data.value?.data ?? [])
const flagRows = computed<FlagCount[]>(() => flags.data.value?.data ?? [])

// The query layer returns router_sysname verbatim, including empty -- a
// router that sent no sysName TLV in its BMP initiation message. Rendering
// is this screen's job, not the query layer's; "(no sysName TLV)" is the
// existing convention (deploy/grafana/dashboards/fleet-health.json's own
// `if(router_sysname = '', '(no sysName TLV)', router_sysname)`), followed
// here rather than invented fresh so an operator sees the same phrase in
// this screen and in the dashboards behind it.
function sysnameOf(row: { router_sysname: string }): string {
  return row.router_sysname === '' ? '(no sysName TLV)' : row.router_sysname
}

function dumpRowAttrs(row: RouterDumpCount): Record<string, string> {
  return { 'data-router': row.router_ip }
}

function sessionRowAttrs(row: RouterSessionCount): Record<string, string> {
  return { 'data-router': row.router_ip }
}

/**
 * A DUMP IS NOT A FAILURE. archived = dumps + changes on every row this
 * endpoint returns (api/openapi.yaml's own RouterDumpCount contract), so
 * this reads the two parts the response already carries rather than
 * recomputing archived from anything else -- there is exactly one place a
 * mismatch between the two could hide, and this is not it.
 *
 * The result is composition, not a verdict: it says what this router's
 * archived rows are MADE OF (a session reset re-sending its table versus
 * the network actually changing), and it is rendered with one styling
 * rule -- none -- regardless of how high it reads. A router at 100% dumps
 * is a router that reset its BMP session and re-announced everything, which
 * is ordinary; MonitorView.test.ts pins exactly this against xr-rr1's own
 * captured row.
 *
 * `undefined`, not 0, when archived is 0: a router
 * with nothing archived in the window has no composition to state, and
 * "0% of archived" is itself a claim -- that the window's zero rows are
 * measurably 0% dumps -- this screen has no basis for. No captured row
 * exercises this (collection-dumps.json's own minimum archived is 24;
 * MonitorView.test.ts covers it with a small synthesized row, labeled
 * inline there), but the template still has to render SOMETHING for it,
 * and claiming less ("—") is this screen's whole thesis about ratios it
 * cannot support.
 */
function dumpCompositionPct(row: RouterDumpCount): number | undefined {
  return row.archived === 0 ? undefined : Math.round((row.dumps / row.archived) * 100)
}

/** The dumps cell's composition text -- "—" for the undefined case above, never a stated 0%. */
function dumpCompositionLabel(row: RouterDumpCount): string {
  const pct = dumpCompositionPct(row)
  return pct === undefined ? '—' : `(${pct}% of archived)`
}

/**
 * The tile row. Every value is a total the response itself carried --
 * meta.session_totals, meta.dump_totals, meta.flag_totals, and
 * /v1/routers' own per-router peer counts -- so nothing here is a figure
 * this screen derived. StatTile cannot compute; see its own doc comment.
 *
 * A delta against a previous window ("-2", "+312") is absent: nothing in
 * this API returns one, and computing it would mean a second request over
 * a window the screen chose rather than the operator.
 */
const tiles = computed(() => {
  const st = sessions.data.value?.meta?.session_totals
  const dt = dumps.data.value?.meta?.dump_totals
  const ft = flags.data.value?.meta?.flag_totals
  // One entry per ROUTER before summing, never one per (collector, router):
  // /v1/routers returns both, and adding them reported a dual-homed
  // router's peers once per collector watching it. Measured 2026-09-21 --
  // this tile read "31 / 35" against 25 up of 28 distinct (router, peer,
  // rib) identities in the archive. See lib/bestVantage.ts for why the
  // remedy is the best-placed collector's row whole rather than a merge.
  const rows = bestVantageRouters((routers.data.value?.data ?? []) as Router[])
  const up = rows.reduce((n, r) => n + r.peers_up, 0)
  const down = rows.reduce((n, r) => n + r.peers_down, 0)
  const lost = rows.reduce((n, r) => n + r.peers_view_lost, 0)
  const stale = rows.reduce((n, r) => n + r.peers_stale, 0)

  const out: { key: string; label: string; value: string; sub?: string; note?: string }[] = []
  out.push({
    key: 'peers',
    label: 'Peers up now',
    value: rows.length ? `${formatCount(up)} / ${formatCount(up + down + lost + stale)}` : '—',
    sub: rows.length
      ? `${formatCount(down)} down · ${formatCount(lost)} view lost · ${formatCount(stale)} stale`
      : undefined,
    note:
      'Current peer state from /v1/routers, not a count over the window. ' +
      'view_lost is the collector losing its own BMP transport while the ' +
      'router said nothing; it is never summed with down. stale is a peer ' +
      'last seen up whose collector has not been heard from since; it is ' +
      'never counted as up.',
  })
  if (st) {
    out.push({
      key: 'sessions',
      label: `Sessions · ${windowLabel.value}`,
      value: formatCount(st.sessions),
      sub: `${formatCount(st.up)} up · ${formatCount(st.down)} down · ${formatCount(st.view_lost)} view lost`,
      note: 'Distinct BMP sessions the window contains, never the events inside them.',
    })
  }
  if (dt) {
    out.push({
      key: 'archived',
      label: `Archived · ${windowLabel.value}`,
      value: formatCount(dt.archived),
      // The two parts, not a ratio: a fleet-wide percentage would be a
      // second composition figure on a screen whose only legitimate one is
      // the per-row one, and composition is not a verdict.
      sub: `${formatCount(dt.dumps)} dumps · ${formatCount(dt.changes)} changes`,
      note:
        'A dump is a router re-sending a route after its BMP session reset -- ' +
        'ordinary behavior, not a fault. This is what the window\'s archived rows ' +
        'are composed of.',
    })
  }
  if (ft) {
    out.push({
      key: 'flags',
      label: `Flagged envelopes · ${windowLabel.value}`,
      value: formatCount(ft.envelopes),
      sub: `${formatCount(flagRows.value.length)} distinct flags`,
      note:
        'Parse flags are historical: a count records the decoder as it stood ' +
        'when the row was written, never the decoder running now.',
    })
  }
  return out
})

/**
 * The newest events first, capped: the panel is a glance, not the Events
 * screen. `data.value?.data`, because useFleetEvents returns Colada's own
 * query object -- there is no `rows` on it, unlike the cursor-accumulating
 * useEvents next to it in queries.ts. A mock that invented one is how this
 * passed its tests while failing to typecheck.
 */
const recentEvents = computed<PeerEvent[]>(() => (events.data.value?.data ?? []).slice(0, 7))

/**
 * Where a router row goes: that router's peers, not the fleet's. /v1/peers
 * takes `router` and usePeers has always accepted it -- PeersView reads
 * this query parameter and states the narrowing on screen, so a click that
 * names a router arrives somewhere that answers for that router.
 *
 * Object form rather than a template string because a router address can
 * be IPv6 and vue-router encodes an object's query values.
 */
function peersOf(row: { router_ip: string }) {
  return { path: '/peers', query: { router: row.router_ip } }
}

/** Where a peer row's second destination goes -- SessionHistoryView seeds its scope picker off these two. */
function historyOf(row: { router_ip: string; peer_ip: string }) {
  return { path: '/session-history', query: { router: row.router_ip, peer: row.peer_ip } }
}

const dumpColumns: Column<RouterDumpCount>[] = [
  { id: 'router_sysname', header: 'Router' },
  { id: 'archived', header: 'Archived', numeric: true },
  { id: 'dumps', header: 'Dumps', numeric: true },
  { id: 'changes', header: 'Changes', numeric: true },
]

// sessions/up/down/view_lost stay four separate columns, deliberately not
// three or one: view_lost is not down (queries.ts's useCollectionSessions
// doc / RouterSessionCount's own field comments), and folding it in would
// misreport the collector's own blindness as a fact the router stated.
// Rendered as DataTable's own plain numeric cells rather than through a
// custom slot, because these ARE the real, unmodified fields -- nothing is
// derived here the way the dumps section's composition percentage is.
// Declared rather than left to split evenly. Five equal columns gave Router
// 20%, 112px at the panel floor and 122px in a 1366px viewport's panel, and
// a 12-character sysName such as 4a8063e2ed30 needs 123px with its padding
// (measured in Chromium). 28% is 157px at the floor; the widest header
// beside it, VIEW LOST, needs 94px, and 18% is 101px.
const sessionColumns: Column<RouterSessionCount>[] = [
  { id: 'router_sysname', header: 'Router', width: '28%' },
  { id: 'sessions', header: 'Sessions', numeric: true, width: '18%' },
  { id: 'up', header: 'Up', numeric: true, width: '18%' },
  { id: 'down', header: 'Down', numeric: true, width: '18%' },
  { id: 'view_lost', header: 'View lost', numeric: true, width: '18%' },
]

// No router_sysname on PeerLocRib (api/openapi.yaml's own schema carries
// only router_ip, peer_ip, reported, archived, has_stat) -- router_ip is
// the identifying column here, not a stand-in for the sysname convention
// above.
// Declared rather than left to split evenly. The Peer cell is an address
// AND a "history" link, 161px with its padding for 172.31.0.90 (measured in
// Chromium), and an even 25% gave it 140px at the panel floor and 153px in
// a 1366px viewport's panel, so the link was cut off. 38% is 213px at the
// floor. Router is one address, 98px; the numeric headers need 93px.
const locribColumns: Column<PeerLocRib>[] = [
  { id: 'router_ip', header: 'Router', width: '26%' },
  { id: 'peer_ip', header: 'Peer', width: '38%' },
  { id: 'reported', header: 'Reported', numeric: true, width: '18%' },
  { id: 'archived', header: 'Archived', numeric: true, width: '18%' },
]

const flagColumns: Column<FlagCount>[] = [
  { id: 'flag', header: 'Flag' },
  { id: 'envelopes', header: 'Envelopes', numeric: true },
]
</script>

<template>
  <section class="screen">
    <div class="head">
      <ScreenHeader title="Monitor" :eyebrow="['collection signals', windowLabel]" />
      <!-- The segmented control, shared with Events and Session
           history (WindowPicker). Its 7d option is absent for the reason
           WINDOWS documents: every one of these four endpoints refuses a
           window past the daemon's clamp. -->
      <WindowPicker v-model="since" :windows="WINDOWS" />
    </div>

    <p class="window-note" data-window-note>
      Showing collection signals from the {{ windowLabel }}.
    </p>

    <!-- Four measured signals, no verdict: the tiles state totals their own
         answers carried, and the claim each number must be read with is on
         the tile itself (StatTile's `note`). -->
    <div class="tiles">
      <StatTile
        v-for="t in tiles"
        :key="t.key"
        :data-tile="t.key"
        :label="t.label"
        :value="t.value"
        :sub="t.sub"
        :note="t.note"
      />
    </div>

    <!-- The top row, and the reason it is a row: the churn chart
         is the SHAPE of the window and the event feed is what made that
         shape, so the two are read together rather than a scroll apart,
         or the chart reads as a silhouette. -->
    <div class="churn-row" data-row="churn">
    <section class="panel" data-section="churn">
      <h2>Update churn</h2>
      <p class="note">
        What the archive received, by time. A session dump is a router
        re-sending what it already knew; the colors are identity, not
        severity.
      </p>
      <p v-if="churn.error.value" class="error" role="alert">
        {{ churn.error.value.message }}
      </p>
      <!-- 160, not the component's own 120: that default was chosen when
           this panel spanned the page and had no axis under it. Beside the
           feed it is a band, and 160 is the plot height chosen to match
           (a measured bar area of ~158 CSS px). -->
      <ChurnChart
        v-else
        :buckets="churn.data.value?.data ?? []"
        :bucket-label="churn.data.value?.meta?.churn_bucket ?? churnBucket"
        :from="windowStart"
        :to="windowEnd"
        :height="160"
      />
    </section>

    <section class="panel" data-section="events">
      <h2>Recent peer events</h2>
      <!-- The collector is named on the row rather than only here, but the
           note has to say WHAT that id at the end of a row is: a feed has
           no column header to carry the word the Peers and Events tables
           put above the same value. -->
      <p class="note">
        The newest {{ recentEvents.length }} of this window, fleet-wide, each
        with the collector that saw it.
        <RouterLink class="link" to="/events">All events</RouterLink>
      </p>
      <ul class="events">
        <!-- Two lines, not one, because this panel now sits in a rail
             beside the chart rather than across the page: at 1920px a down
             row wanted ~695px of a 680px column and wrapped mid-timestamp.
             The reason gets the second line, and ONLY a down has one,
             because an up or a view_lost has no down_reason to show
             (lib/eventReason.ts: the kind decides). -->
        <li v-for="(e, i) in recentEvents" :key="i" class="event">
          <span class="line">
            <span class="mono when">{{ formatClock(e.ts_collector) }}</span>
            <EventKindMark :kind="e.kind" />
            <span class="who mono">{{ e.router_sysname || e.router_ip }} · {{ e.peer_ip }}</span>
            <!-- WHOSE view this row is. /v1/events is one row per
                 (collector, router, peer, session), so a router two
                 collectors monitor sends two rows for one event -- and a
                 view_lost, which IS one collector losing its own transport,
                 means nothing without it. Right-aligned rather than added to
                 the dot-separated pair beside it, because it answers a
                 different question from "which peer". -->
            <span class="seen mono" data-collector>{{ e.collector }}</span>
          </span>
          <span v-if="hasDownReason(e)" class="why mono" data-why>{{ reasonDisplay(e) }}</span>
        </li>
        <li v-if="!recentEvents.length" class="quiet">
          No peer event in the {{ windowLabel }}. An empty list here is an
          answer -- the window held no session change -- not a missing feed.
        </li>
      </ul>
    </section>
    </div>

    <!-- Full width, outside the panels grid, for a reason measured in a
         browser rather than chosen: this table has SEVEN columns, and the
         grid's own comment records 620px as the narrowest a FIVE-column
         table on this screen reads at without truncating. At 641px -- one
         column of that grid on a wide display -- the router sysname and two
         numeric headers clipped even with the widths summing to 100%. -->
    <section class="panel wide" data-section="churn-peers">
      <h2>Peers by update volume</h2>
      <!-- Said once, in full, because the column header has room for two
           words. Every number in this table counts rows THIS COLLECTOR
           STORED inside the window; a BGP speaker's own update rate is not
           observable from a BMP archive, and reporting one would treat a
           collection artifact as a network fact. -->
      <p class="note" data-churn-peers-note>
        Ranked by changes archived — re-advertisements and withdrawals, not
        session dumps, so a peer whose session restarted once does not outrank
        a busy one. Archived/s is rows this collector stored per second over
        {{ windowLabel }}, which is a collection rate and not the router's
        update rate.
      </p>
      <DataTable
        :columns="churnPeerColumns"
        :rows="churnPeerRows"
        :meta="churnPeers.data.value?.meta"
        :loading="churnPeers.isPending.value"
        :error="churnPeers.error.value ?? undefined"
        :min-width="WIDE_TABLE_MIN_WIDTH"
      >
        <template #cell-peer_ip="{ row }">
          <RouterLink
            class="mono link"
            :to="`/peers/${(row as RankedPeer).router_ip}/${(row as RankedPeer).peer_ip}`"
          >{{ (row as RankedPeer).peer_ip }}</RouterLink>
          <RouterLink class="history" :to="historyOf(row as RankedPeer)">history</RouterLink>
        </template>
        <template #cell-router_sysname="{ row }">
          <RouterLink class="link" :to="peersOf(row as RankedPeer)">{{
            (row as RankedPeer).router_sysname || '(no sysName TLV)'
          }}</RouterLink>
        </template>
        <!-- Prefixes and State are the joined half, and both go quiet
             together when the pair has more than one RIB view: two real
             route counts for one peer are not addable, and choosing between
             them silently is the unstated choice this note replaces. -->
        <template #cell-routes="{ row }">
          <span
            v-if="(row as RankedPeer).multiRib"
            data-multi-rib
            class="quiet"
            title="This peer is mirrored under more than one RIB view, so it has more than one route count. They are views of the same session, not additions to each other."
          >per RIB</span>
          <span v-else-if="(row as RankedPeer).routes !== undefined" class="mono">{{
            formatCount((row as RankedPeer).routes!)
          }}</span>
          <span v-else class="quiet">—</span>
        </template>
        <template #cell-changes_per_second="{ row }">
          <span class="mono">{{ formatRate((row as RankedPeer).changes_per_second) }}</span>
        </template>
        <template #cell-state="{ row }">
          <StatePill v-if="(row as RankedPeer).state" :state="(row as RankedPeer).state!" />
          <span v-else class="quiet">—</span>
        </template>
      </DataTable>
    </section>

    <div class="panels">
    <section class="panel" data-section="dumps">
      <h2>Dumps</h2>
      <p
        class="note"
        title="A dump is a router re-sending a route it already sent, because its BMP session reset -- ordinary BMP behavior, not a fault. This column says what the window's archived rows are composed of; it is not an error state and not a ranking."
      >
        A dump is a session reset re-sending routes: composition, not a fault.
      </p>
      <DataTable
        :columns="dumpColumns"
        :rows="dumpRows"
        :meta="dumps.data.value?.meta"
        :loading="dumps.isPending.value"
        :error="dumps.error.value ?? undefined"
        :min-width="PANEL_TABLE_MIN_WIDTH"
        :row-attrs="dumpRowAttrs"
      >
        <template #cell-router_sysname="{ row }">
          <RouterLink class="link" :to="peersOf(row as RouterDumpCount)">{{
            sysnameOf(row as RouterDumpCount)
          }}</RouterLink>
        </template>
        <template #cell-dumps="{ row }">
          <span class="mono">
            {{ (row as RouterDumpCount).dumps }}
            <span data-dump-ratio class="composition">{{
              dumpCompositionLabel(row as RouterDumpCount)
            }}</span>
          </span>
        </template>
      </DataTable>
    </section>

    <section class="panel" data-section="sessions">
      <h2>Sessions</h2>
      <p
        class="note"
        title="Sessions counts distinct BMP sessions, never events -- one session can carry many peer events. down is the router stating, on the wire, that a session ended. view_lost is the collector losing its own BMP transport while the router said nothing at all. The two are never summed."
      >
        Distinct sessions, never events. <EventKindMark kind="down" /> is the
        router's statement; <EventKindMark kind="view_lost" /> is the
        collector's blindness. Never summed.
      </p>
      <DataTable
        :columns="sessionColumns"
        :rows="sessionRows"
        :meta="sessions.data.value?.meta"
        :loading="sessions.isPending.value"
        :error="sessions.error.value ?? undefined"
        :min-width="PANEL_TABLE_MIN_WIDTH"
        :row-attrs="sessionRowAttrs"
      >
        <template #cell-router_sysname="{ row }">
          <RouterLink class="link" :to="peersOf(row as RouterSessionCount)">{{
            sysnameOf(row as RouterSessionCount)
          }}</RouterLink>
        </template>
      </DataTable>
    </section>

    <section class="panel" data-section="locrib">
      <h2>Loc-RIB</h2>
      <p class="note" data-locrib-note>
        A gap is not proof of loss: most peers send adj-RIB-in, and RFC 9069
        Loc-RIB monitoring is a separate capability most routers do not
        enable. A peer that never sent the stat reads "no stat", never a
        reported gap of 0; the window scopes reported only. Archived counts
        the peer's live Loc-RIB routes in its router's current BMP session,
        never what a superseded session left behind, so widening the window
        never adds an archived route.
      </p>
      <DataTable
        :columns="locribColumns"
        :rows="locribRows"
        :meta="locrib.data.value?.meta"
        :loading="locrib.isPending.value"
        :error="locrib.error.value ?? undefined"
        :min-width="PANEL_TABLE_MIN_WIDTH"
      >
        <template #cell-peer_ip="{ row }">
          <RouterLink
            class="mono link"
            :to="`/peers/${(row as PeerLocRib).router_ip}/${(row as PeerLocRib).peer_ip}`"
          >{{ (row as PeerLocRib).peer_ip }}</RouterLink>
          <RouterLink class="history" :to="historyOf(row as PeerLocRib)">history</RouterLink>
        </template>
        <template #cell-reported="{ row }">
          <span v-if="!(row as PeerLocRib).has_stat" data-no-stat>no stat</span>
          <span v-else class="mono">{{ (row as PeerLocRib).reported }}</span>
        </template>
      </DataTable>
    </section>

    <section class="panel" data-section="flags">
      <h2>Parse flags</h2>
      <p
        class="note"
        data-flags-note
        title="A flag counted in this window may describe a bug already fixed before the window even opened -- never read this count as a current diagnosis."
      >
        Parse flags are historical: the count records the decoder as it stood
        when the row was written, never the current one.
      </p>
      <DataTable
        :columns="flagColumns"
        :rows="flagRows"
        :meta="flags.data.value?.meta"
        :loading="flags.isPending.value"
        :error="flags.error.value ?? undefined"
        :min-width="PANEL_TABLE_MIN_WIDTH"
      />
    </section>
    </div>
  </section>
</template>

<style scoped>
.screen { padding: 18px; display: flex; flex-direction: column; gap: 14px; }
/* Wraps so that on a phone the window picker drops under the title rather
   than running past the screen's right edge. */
.head { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: 10px 16px; }
h2 { margin: 0; font: 600 13px var(--font-ui); color: var(--ink); }
.window-note { margin: 0; color: var(--muted); font: 400 12px var(--font-ui); }
.tiles { display: flex; flex-wrap: wrap; gap: 12px; }
.wide { margin-bottom: 0; }
/* The chart gets the wider share because it is the only mark on this screen
   whose x axis is time: at 1.7:1 on a 1920px display the plot is ~1140px,
   which is ~48 five-minute bars of a 6h window with a pixel between them.
   One column below 1040px, where the feed's own rows start wrapping --
   measured in a browser, not chosen: the feed needs ~330px for a clock, a
   kind, a router/peer pair and a reason, and the grid's own note records
   620px as the narrowest a table on this screen reads at. */
.churn-row { display: grid; grid-template-columns: minmax(0, 1.7fr) minmax(0, 1fr); gap: 14px; }
/* minmax(0, 1fr), not 1fr: a bare 1fr track cannot shrink below its
   content's min-content width, and on a 390px phone the event feed's
   one-line rows held it at 422px, so the whole page scrolled sideways. */
@media (max-width: 1040px) { .churn-row { grid-template-columns: minmax(0, 1fr); } }
/* And an event's line may wrap there, so that the clock, kind, pair and
   collector need not share one line on a phone. Only below the same
   breakpoint: on a desktop the pair already wraps inside its own span, and
   letting the whole line wrap too would move the collector id under the
   clock on the rows with an IPv6 peer. */
@media (max-width: 1040px) { .line { flex-wrap: wrap; } }
/* Two columns where there is room, so the four signals and the event feed
   read as one dashboard rather than a stack four screens tall. */
/* 620px is the narrowest a five-column table on this screen reads at
   without truncating its router names -- measured in a browser at 460px,
   where auto-fit produced five columns and "PARSE_FLAG_TS_COLLECTOR_FAL..."
   was as much of a flag name as fit. Two columns on a laptop, three on a
   wide display, one below 620. */
.panels { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(620px, 100%), 1fr)); gap: 14px; }
.panel {
  display: flex; flex-direction: column; gap: 8px; padding: 12px 14px;
  border: 1px solid var(--line); border-radius: 7px; background: var(--surface);
}
.events { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; }
.event {
  display: flex; flex-direction: column; gap: 3px; padding: 7px 0;
  border-bottom: 1px solid var(--line-faint); font-size: 11px;
}
.line { display: flex; align-items: center; gap: 10px; }
.event:last-child { border-bottom: 0; }
/* The events panel's empty line had no rule of its own and inherited the
   body's 14px inside an 11px panel -- visible in a browser, invisible to
   every test, since jsdom computes no styles. */
.events .quiet { padding: 7px 0; color: var(--muted); font: 400 11px var(--font-ui); }
/* The one span that must never wrap: a broken timestamp reads as two
   events, which is what the rail did before the row became two lines. */
.when { color: var(--faint); white-space: nowrap; }
.who { color: var(--ink-2); }
.seen { margin-left: auto; padding-left: 10px; color: var(--muted); white-space: nowrap; }
/* Indented under the row it explains, so a reason cannot be read as the
   next event's own line. */
.why { color: var(--muted); padding-left: 14px; }
.note { margin: 0; max-width: 84ch; color: var(--muted); font: 400 11px var(--font-ui); }
/* Deliberately no color, no weight, no threshold-based class: a dump
   ratio describes composition, not a fault. See dumpCompositionPct's
   own comment. */
.composition { color: var(--muted); }
/* PeersView.vue's own link convention, not a second one. `history` is a
   secondary destination beside the peer address, so it reads as the
   quieter of the two rather than competing with it. */
.link { color: var(--ink-2); }
.history { margin-left: 7px; color: var(--muted); font: 400 11px var(--font-ui); }
</style>
