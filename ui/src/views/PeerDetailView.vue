<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import EventKindMark from '@/components/EventKindMark.vue'
import ChurnChart from '@/components/ChurnChart.vue'
import DataTable, { type Column } from '@/components/DataTable.vue'
import ResultMeta from '@/components/ResultMeta.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import StatePill from '@/components/StatePill.vue'
import StatTile from '@/components/StatTile.vue'
import { bestVantageCount } from '@/lib/bestVantage'
import { reasonDisplay } from '@/lib/eventReason'
import { formatClock } from '@/lib/formatClock'
import { formatCount } from '@/lib/formatCount'
import { useEvents, usePeers, type EventScope,
  useCollectionChurn,
  useCollectionChurnPrefixes,
} from '@/api/queries'
import type { Peer, PeerEvent,
  PrefixChurn,
} from '@/api/generated'

const route = useRoute()
// A computed, not a one-time ref copy of route.params.router: Vue Router
// reuses this component instance rather than remounting it when only the
// path params change on the same matched route (/peers/:router/:peer to
// another /peers/:router/:peer), so a plain `ref(String(route.params.router))`
// read once at setup would go stale on the second navigation and query the
// first peer's router forever after.
const routerIp = computed(() => String(route.params.router))
// isPending, not isLoading -- see RoutersView.vue's comment on the same
// seam. usePeers polls every 30s; isLoading flips true on every tick, not
// only the first fetch. Wiring isLoading here would hide an already-found
// peer's stats behind "loading..." on every poll tick.
const { data, isPending, error } = usePeers(routerIp, ref(undefined))

// Filter, not find. api/openapi.yaml documents /v1/peers as "one entry per
// (collector, router, peer, rib)" -- rib is a five-valued enum (in_pre,
// in_post, out_pre, out_post, loc_rib), and one (router, peer) pair can
// carry more than one row: a deployment watching both the pre- and
// post-policy adj-rib for the same session has two real rows for the same
// peer, each with its own state/asn/routes/dump_states. The PEER THIS
// SCREEN'S FIXTURE COVERS only ever appears under in_pre -- which is a
// claim about that peer, not about the deployment it came from, whose
// capture also holds a real loc_rib row for the 0.0.0.0 self-peer. What no fixture
// shows is two ribs for one (router, peer), and .find() would have
// silently rendered whichever row came first and presented it as the
// whole peer, with nothing on screen saying another view existed. That is
// the exact defect class this project audits for, so every matching row
// gets rendered, not just one.
const peerRows = computed<Peer[]>(
  () =>
    data.value?.data.filter(
      (p: Peer) => p.peer_ip === route.params.peer && p.router_ip === route.params.router,
    ) ?? [],
)

/**
 * One section per (collector, rib), which is what a row actually is.
 *
 * api/openapi.yaml's "one entry per (collector, router, peer, rib)" has
 * four axes; this keyed on `rib` alone, so two collectors watching the same
 * (router, peer, rib) collided on the key and rendered two identical-looking
 * sections. Two collectors run independent BMP sessions: their state,
 * session_id, route counts and dump progress are independent facts that can
 * disagree, so which collector a number came from is part of the number.
 * Router.collector's own description in the contract says as much -- one
 * entry per collector, "not a merged one".
 */
function sectionKey(row: Peer): string {
  return `${row.collector}|${row.rib}`
}

/**
 * The peer's own event history, which is where every session fact below
 * comes from. The Session panel and its uptime/flaps tiles are all
 * derived from peer_events; what this screen adds is nothing but a
 * scope -- /v1/events already answers for one (router, peer).
 *
 * 24 hours, fixed rather than a control: this is a peer's detail page, not
 * the Session history screen, which exists for the same data over a window
 * the operator picks and is one link away below. The window is stated on
 * every figure derived from it, because "3 session changes" and "3 session
 * changes in the last 24 hours" are different claims and only the second is
 * one this answer supports.
 */
const WINDOW = '24h'
const WINDOW_LABEL = 'last 24 hours'

// `session` is part of EventScope's shape but not part of the question this
// screen asks: it wants the peer's history across whatever sessions the
// window holds, which is precisely how the tiles below count them.
const eventScope = computed<EventScope | undefined>(() => {
  const rows = peerRows.value
  if (!rows.length) return undefined
  return { router: rows[0].router_ip, peer: rows[0].peer_ip, session: rows[0].session_id }
})
const history = useEvents(eventScope, ref(WINDOW))

/**
 * This peer's own churn, and its own most-changed prefixes.
 *
 * SCOPED, and the scope is the whole point: both endpoints answer
 * fleet-wide when given no router and peer, and a fleet answer rendered
 * under one peer's heading is somebody else's activity wearing this peer's
 * name -- which looks entirely plausible on screen. The scope is derived
 * from the peer's own row rather than from the URL params so it cannot name
 * a pair /v1/peers does not have.
 *
 * 30m buckets across a 24h window: 48 bars, which is a readable slice of
 * the window rather than a fixed width that means something different at
 * every window. The CHART renders the width the ANSWER reports, never this
 * request's -- the two agree until a clamp disagrees.
 */
/**
 * The negotiated session facts, from the peer row.
 *
 * `facts` is peerRows[0] because every row for this (router, peer) shares a
 * session and therefore shares its OPEN: the rows differ by RIB view, which
 * is a direction of monitoring rather than a separate negotiation.
 */
const sessionFacts = computed(() => peerRows.value[0])

/**
 * How the hold timer reads, in words rather than a bare number.
 *
 * THREE CASES, and collapsing any two of them is the defect this exists to
 * avoid:
 *
 *   null   no OPEN was observed for this session -- true of every peer whose
 *          only event is a Down, and of every row written before the column
 *          existed. NOT a hold time of zero.
 *   0      a real negotiated value. RFC 4271 §4.2: the timer never expires,
 *          which means keepalives are off for this session. Rendering a bare
 *          "0" here reads as a missing value, which is the opposite of what
 *          it means.
 *   n      the negotiated seconds.
 */
const holdTimeText = computed(() => {
  const h = sessionFacts.value?.hold_time
  if (h === null || h === undefined) return 'not observed — no OPEN seen this session'
  if (h === 0) return '0 — the hold timer never expires, so keepalives are off'
  return `${h}s`
})

// There is deliberately no keepalive here. BGP's OPEN carries a hold time
// and nothing else; the keepalive interval is a LOCAL timer, conventionally
// hold_time/3, that a speaker never announces, so showing a number here
// would dress up a convention as an observation.

const CHURN_BUCKET = '30m'
const churnScope = computed(() => {
  const rows = peerRows.value
  if (!rows.length) return undefined
  return { router: rows[0].router_ip, peer: rows[0].peer_ip }
})
const churn = useCollectionChurn(ref(WINDOW), ref(CHURN_BUCKET), churnScope)
const prefixChurn = useCollectionChurnPrefixes(ref(WINDOW), churnScope)

/**
 * The window the chart's time axis spans, as instants. Recomputed when an
 * answer lands rather than fixed at mount, so the newest bars do not slide
 * off the right edge as the poll ticks -- MonitorView.vue's own reasoning.
 */
const churnWindowEnd = computed(() => {
  void churn.data.value
  return new Date().toISOString()
})
const churnWindowStart = computed(() =>
  new Date(Date.parse(churnWindowEnd.value) - 24 * 3_600_000).toISOString(),
)

// The panel's own columns. `observations` sits beside the two that rank it
// precisely because it does NOT rank it: a prefix with 90 observations of
// which 87 are session dumps is a session that restarted, not a prefix that
// churned, and showing the number that ranks nothing beside the ones that
// do is what makes that legible rather than surprising.
const prefixChurnColumns: Column<PrefixChurn>[] = [
  { id: 'prefix', header: 'Prefix', width: '34%' },
  { id: 'readvertise', header: 'Re-advertised', numeric: true, width: '17%' },
  { id: 'withdraw', header: 'Withdrawn', numeric: true, width: '15%' },
  { id: 'observations', header: 'Observations', numeric: true, width: '17%' },
  { id: 'sessions', header: 'Sessions', numeric: true, width: '17%' },
]

/**
 * useEvents is a cursor WALK, and a walk fetches on reload() -- setting the
 * scope ref alone asks for nothing. Without this the screen rendered "no
 * peer event in the last 24 hours" for a peer whose events were on the
 * Monitor screen at the same moment, and the mocked tests could not see it
 * because a mock hands back rows either way.
 *
 * Keyed on the (router, peer) pair rather than on the scope object: the
 * object is a computed and gets a new identity whenever /v1/peers polls,
 * which would re-walk the same peer every 30 seconds. `immediate` because
 * the peer is usually already in the first answer.
 */
watch(
  () => {
    const s = eventScope.value
    return s ? `${s.router}|${s.peer}` : undefined
  },
  (key) => {
    if (key) history.reload()
  },
  { immediate: true },
)

const eventRows = computed<PeerEvent[]>(() => history.rows.value ?? [])

/**
 * Distinct BMP sessions the window holds -- never the events inside them,
 * and never once per collector watching.
 *
 * `/v1/events` is one row per OBSERVATION, so a router two collectors
 * monitor reports every session twice, and each collector mints its OWN
 * session_id for the one underlying session. Counting distinct ids across
 * all rows therefore counts OBSERVERS, which is what this tile did: it read
 * 2 in a browser for a peer with one session. See lib/bestVantage.ts for
 * why the answer is the best single vantage point rather than a dedupe --
 * there is no key two collectors agree on to dedupe by.
 */
const sessionCount = computed(() =>
  bestVantageCount(
    eventRows.value,
    (e) => e.collector,
    (rows) => new Set(rows.map((e) => e.session_id)).size,
  ),
)

/**
 * Flaps, and a COMPLETED cycle each time -- the session went down and
 * came back.
 *
 * `down` and `view_lost` are never summed into one number: the two are
 * kept as separate tiles, so `view_lost` is counted next door instead of
 * folded in here.
 *
 * A session that went down and STAYED down is not a flap, it is an outage,
 * and the State pill and `Up since` are what say so. Counting it here would
 * make "4 flaps" and "down since Tuesday" the same number.
 *
 * Consecutive downs collapse: a session cannot go down twice without coming
 * up in between, so a second `down` is a repeat of the state and not a new
 * cycle. Events arrive newest-first, so they are read in ascending order
 * here -- a state machine over a descending list counts up-then-down pairs,
 * which is the same number only by coincidence and not on the edges above.
 *
 * `view_lost` is ignored rather than treated as a down: it is the collector
 * losing its transport while the ROUTER said nothing, so folding it in
 * would attribute the collector's own outage to the peer.
 */
function countFlaps(rows: PeerEvent[]): number {
  const ascending = [...rows].sort((a, b) => a.ts_collector.localeCompare(b.ts_collector))
  let flaps = 0
  let sawDown = false
  for (const e of ascending) {
    if (e.kind === 'down') sawDown = true
    else if (e.kind === 'up' && sawDown) {
      flaps += 1
      sawDown = false
    }
  }
  return flaps
}

const flapCount = computed(() =>
  bestVantageCount(eventRows.value, (e) => e.collector, countFlaps),
)

/**
 * The collector losing its own BMP transport while the router said nothing
 * -- its own tile, never added to the flaps beside it.
 *
 * This is the one event on this screen that is genuinely per-collector: one
 * collector can go blind while the other keeps watching. Best vantage is
 * still right for it, and for the usual reason -- the collector that saw
 * the most interruptions is reported whole, rather than two collectors'
 * different views being added into a number neither of them observed.
 */
const viewLostCount = computed(() =>
  bestVantageCount(
    eventRows.value,
    (e) => e.collector,
    (rows) => rows.filter((e) => e.kind === 'view_lost').length,
  ),
)

/**
 * When the newest view began, from the newest `up` in the window.
 *
 * `undefined` when the window holds no `up`, and the tile then does not
 * render at all: an established duration like "UPTIME 61d" is not a claim
 * this answer can support for a session whose up event is older than the
 * window. A dash there would read as zero.
 */
const upSince = computed<string | undefined>(() => {
  const ups = eventRows.value.filter((e) => e.kind === 'up')
  if (!ups.length) return undefined
  return ups.reduce((a, b) => (a.ts_collector > b.ts_collector ? a : b)).ts_collector
})

const tiles = computed(() => {
  const rows = peerRows.value
  const out: { key: string; label: string; value: string; sub?: string; note?: string }[] = []
  if (rows.length) {
    out.push({
      key: 'routes',
      label: 'Routes',
      value: formatCount(rows.reduce((n, r) => n + r.routes, 0)),
      sub: rows.length > 1 ? `across ${rows.length} rib views` : rows[0].rib,
      note: 'Routes this peer currently has in the archive, per the /v1/peers answer.',
    })
    out.push({ key: 'asn', label: 'ASN', value: String(rows[0].asn) })
  }
  out.push({
    key: 'sessions',
    label: `Sessions · ${WINDOW_LABEL}`,
    value: formatCount(sessionCount.value),
    note: 'Distinct BMP sessions in this window, never the events inside them.',
  })
  out.push({
    key: 'flaps',
    label: `Flaps · ${WINDOW_LABEL}`,
    value: formatCount(flapCount.value),
    note:
      'Times this session went down and came back inside the window. A ' +
      'session that went down and stayed down is an outage, not a flap — ' +
      'the state pill and Up since are what say so. Counted once however ' +
      'many collectors watched.',
  })
  out.push({
    key: 'view-lost',
    label: `View lost · ${WINDOW_LABEL}`,
    value: formatCount(viewLostCount.value),
    note:
      'Times a collector lost its own BMP transport while the router said ' +
      'nothing. This is the collector going blind, not the peer going down, ' +
      'and it is never added to the flaps beside it.',
  })
  if (upSince.value) {
    out.push({
      key: 'up-since',
      label: 'Up since',
      value: formatClock(upSince.value),
      note:
        "The newest up event in this window, on the collector's clock. Absent " +
        'when the window holds none -- an established duration is not something ' +
        'this answer can support for a session older than the window.',
    })
  }
  return out
})

/** Newest first, capped: the full list is one link away. */
const recentEvents = computed(() => eventRows.value.slice(0, 6))

/**
 * The session's endpoints, as the events carry them. The only place in this
 * UI that shows them, and real: local_ip, local_port and remote_port are
 * fields on the peer_events shape. Rendered from the newest event rather
 * than the oldest -- a session that moved ports is describing its current
 * self.
 */
const endpoints = computed(() => {
  const e = eventRows.value[0]
  if (!e) return undefined
  return { local: `${e.local_ip}:${e.local_port}`, remote: `${e.peer_ip}:${e.remote_port}` }
})
</script>

<template>
  <section class="screen">
    <p v-if="error" class="error" role="alert">{{ error.message }}</p>
    <!-- data-blocking, because this is THE state that hides everything
         else. Tests used to look for the word "loading" anywhere on the
         page, which the footer's own session_dumping copy ("a session is
         still loading its initial view") now also satisfies -- an
         assertion that would pass or fail on unrelated wording. -->
    <p v-else-if="isPending" class="quiet" data-blocking>loading…</p>
    <template v-else>
      <p v-if="peerRows.length === 0" class="quiet">no such peer on this router</p>

      <template v-else>
        <div class="head">
          <ScreenHeader
            :title="peerRows[0].peer_ip"
            :eyebrow="[`as${peerRows[0].asn}`, peerRows[0].collector]"
          />
          <!-- The state belongs beside the subject: one pill per rib
               view, because a peer can be up pre-policy and something
               else post-policy. -->
          <div class="states">
            <span v-for="row in peerRows" :key="sectionKey(row)" class="state-of">
              <span class="mono rib-tag">{{ row.rib }}</span>
              <StatePill :state="row.state" />
            </span>
          </div>
        </div>

        <!-- Only the tiles with an answer behind them are shown, and
             each says which window it covers. -->
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

        <div class="panels">
          <!-- What the session is, as its own events report it. The hold
               timer, the negotiated families and the init message's
               sysDescr ARE drawn below. What is still absent is the
               local ASN, because nothing stores it, and the keepalive
               timer, which the protocol does not carry -- see
               holdTimeText. -->
          <section class="panel" data-section="session">
            <h2>Session</h2>
            <dl class="facts">
              <template v-if="endpoints">
                <dt>Local</dt>
                <dd class="mono">{{ endpoints.local }}</dd>
                <dt>Remote</dt>
                <dd class="mono">{{ endpoints.remote }}</dd>
              </template>
              <dt>Session id</dt>
              <dd class="mono">{{ peerRows[0].session_id }}</dd>
              <dt>RIB views</dt>
              <dd class="mono">{{ peerRows.map((r) => r.rib).join(' · ') }}</dd>

              <dt>Hold timer</dt>
              <dd class="mono" data-hold-time>{{ holdTimeText }}</dd>

              <!-- NEGOTIATED families, which is a different and stronger
                   claim than the families this peer holds routes in: one
                   negotiated and carrying nothing shows up here and in no
                   route count. -->
              <dt>Families negotiated</dt>
              <dd class="mono" data-mp-families>
                {{ sessionFacts?.mp_families?.length
                  ? sessionFacts.mp_families.join(' · ')
                  : 'none observed' }}
              </dd>
              <template v-if="sessionFacts?.addpath_families?.length">
                <dt>ADD-PATH</dt>
                <dd class="mono" data-addpath-families>
                  {{ sessionFacts.addpath_families.join(' · ') }}
                </dd>
              </template>

              <!-- Verbatim, never parsed into vendor/version on screen: the
                   raw string is what the router actually said. -->
              <template v-if="sessionFacts?.sys_descr">
                <dt>Router says</dt>
                <dd class="mono descr" data-sys-descr>{{ sessionFacts.sys_descr }}</dd>
              </template>
            </dl>
            <p v-if="!endpoints" class="note">
              No event for this peer inside the {{ WINDOW_LABEL }}, so its endpoints
              are unknown here — they are carried by peer events, not by the peer
              listing.
            </p>
          </section>

          <section class="panel" data-section="history">
            <h2>Session history · {{ WINDOW_LABEL }}</h2>
            <ul class="events">
              <li v-for="(e, i) in recentEvents" :key="i" class="event">
                <span class="mono when">{{ formatClock(e.ts_collector) }}</span>
                <EventKindMark :kind="e.kind" />
                <span class="mono why">{{ reasonDisplay(e) }}</span>
              </li>
              <li v-if="!recentEvents.length" class="quiet">
                No peer event in the {{ WINDOW_LABEL }}. That is an answer — the
                window held no session change — not a missing feed.
              </li>
            </ul>
            <p class="note">
              <RouterLink
                class="link"
                :to="{
                  path: '/session-history',
                  query: { router: peerRows[0].router_ip, peer: peerRows[0].peer_ip },
                }"
                >Full history, over a window you choose</RouterLink
              >
            </p>
          </section>

          <!-- What this peer sent over the window, by time. The same
               classification Monitor's fleet chart draws, scoped to one
               peer: session dumps are a session re-sending what it already
               knew, not a fault, and the colors are identity rather than
               severity. -->
          <section class="panel" data-section="churn">
            <h2>
              Update churn · {{ WINDOW_LABEL }}
              <span class="bucket">{{
                churn.data.value?.meta?.churn_bucket ?? CHURN_BUCKET
              }}</span>
            </h2>
            <p v-if="churn.error.value" class="error" role="alert">
              {{ churn.error.value.message }}
            </p>
            <ChurnChart
              v-else
              :buckets="churn.data.value?.data ?? []"
              :bucket-label="churn.data.value?.meta?.churn_bucket ?? CHURN_BUCKET"
              :from="churnWindowStart"
              :to="churnWindowEnd"
            />
          </section>

          <section class="panel" data-section="churn-prefixes">
            <h2>Most churn from this peer · {{ WINDOW_LABEL }}</h2>
            <!-- Said here because the ranking is not the obvious one, and a
                 reader who assumes it is will misread every row. -->
            <p class="note">
              Ranked by changes — re-advertisements and withdrawals — so a prefix
              re-sent by a restarting session does not outrank one the network
              actually moved. Observations counts every archived row, including
              those dumps, which is why it does not match the order. Unicast
              only: a prefix means one thing on route_unicast and something
              else under a route distinguisher.
            </p>
            <DataTable
              :columns="prefixChurnColumns"
              :rows="prefixChurn.data.value?.data ?? []"
              :meta="prefixChurn.data.value?.meta"
              :loading="prefixChurn.isPending.value"
              :error="prefixChurn.error.value ?? undefined"
            >
              <template #cell-prefix="{ row }">
                <span class="mono">{{ (row as PrefixChurn).prefix }}</span>
              </template>
            </DataTable>
          </section>
        </div>

      <!-- One section per rib row. The dump-state list is a per-rib fact (a
           peer can be dumping pre-policy and complete post-policy), so it
           belongs once per row. -->
        <section v-for="row in peerRows" :key="sectionKey(row)" class="rib-view">
          <h2 class="rib-label mono">{{ row.collector }} · {{ row.rib }}</h2>

          <h3>Initial dump</h3>
          <!-- Per family, never summarized: "ipv4u dumping, evpn complete" is a
               different fact from "dumping", and the difference tells an operator
               which table to trust. -->
          <ul class="dumps">
            <li v-for="(state, family) in row.dump_states" :key="family">
              <span class="mono">{{ family }}</span>
              <span :class="['dump', state]">{{ state }}</span>
            </li>
            <li v-if="Object.keys(row.dump_states).length === 0" class="quiet">
              no family has carried a route or an end-of-RIB marker on this session
            </li>
          </ul>
        </section>
      </template>

      <!-- Every table's footer states completeness; this screen had been
           missing it. /v1/peers answers with the same Meta every other
           endpoint does -- a paginated_smear or a
           truncated here would have gone unsaid, and peers.json's own
           captured meta carries session_dumping, so the silence was live.
           It renders on the empty branch too: "no such peer on this router"
           and "a session is still loading its initial view" are the
           difference between "it is not there" and "we may not have looked
           at all of it". No `!error` guard is needed because the error
           branch above already took the whole screen. -->
      <ResultMeta v-if="data?.meta" :meta="data.meta" :shown="peerRows.length" />
    </template>
  </section>
</template>

<style scoped>
.screen { padding: 18px; display: flex; flex-direction: column; gap: 14px; }
.head { display: flex; align-items: flex-end; justify-content: space-between; gap: 16px; }
.states { display: flex; gap: 12px; }
.state-of { display: inline-flex; align-items: center; gap: 6px; }
.rib-tag { color: var(--muted); font-size: 10.5px; }
.tiles { display: flex; flex-wrap: wrap; gap: 12px; }
.panels { display: grid; grid-template-columns: repeat(auto-fit, minmax(340px, 1fr)); gap: 14px; }
.panel {
  display: flex; flex-direction: column; gap: 8px; padding: 12px 14px;
  border: 1px solid var(--line); border-radius: 7px; background: var(--surface);
}
h2 { margin: 0; font: 600 13px var(--font-ui); color: var(--ink); }
.facts { display: grid; grid-template-columns: auto 1fr; gap: 5px 12px; margin: 0; }
.facts dt {
  color: var(--muted); text-transform: uppercase;
  font: 600 9.5px var(--font-ui); letter-spacing: .07em; align-self: center;
}
.facts dd { margin: 0; color: var(--ink-2); font-size: 11.5px; text-align: right; }
.events { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; }
.event {
  display: flex; align-items: center; gap: 10px; padding: 6px 0;
  border-bottom: 1px solid var(--line-faint); font-size: 11px;
}
.event:last-child { border-bottom: 0; }
.when { color: var(--faint); }
.why { color: var(--muted); margin-left: auto; }
.note { margin: 0; color: var(--muted); font: 400 11px var(--font-ui); }
.link { color: var(--ink-2); }
.rib-view { display: flex; flex-direction: column; gap: 8px; }
/* A visible seam between two rib views of the same peer, not just
   whitespace -- when peerRows has one row (the common case today) this
   never renders, since there is only one .rib-view and no sibling above it
   to draw the rule against. */
.rib-view + .rib-view { padding-top: 12px; border-top: 1px solid var(--divider); }
.rib-label {
  margin: 0; padding: 1px 7px; align-self: flex-start; border-radius: 4px;
  background: var(--neutral-chip); color: var(--muted); font: 500 10px var(--font-ui);
  text-transform: uppercase; letter-spacing: .04em;
}
h3 { margin: 2px 0 0; font: 600 12px var(--font-ui); color: var(--ink-2); }
.strip { display: flex; gap: 26px; padding: 12px 16px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); }
.stat { display: flex; flex-direction: column; gap: 5px; }
.k { font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
.v { font: 500 15px var(--font-data); color: var(--ink); }
.dumps { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 5px; font: 400 12px var(--font-ui); }
.dumps li { display: flex; gap: 10px; align-items: center; }
.dump { padding: 1px 7px; border-radius: 4px; font: 500 10px var(--font-ui); }
.complete { background: var(--good-tint); color: var(--good); }
.dumping { background: var(--accent-tint); color: var(--accent-ink-2); }
.error { margin: 0; color: var(--bad-2); font: 400 12px var(--font-ui); }
.quiet { margin: 0; color: var(--muted); font: 400 12px var(--font-ui); }
.descr { white-space: normal; line-height: 1.45; }
.bucket {
  margin-left: 7px; padding: 1px 6px; border-radius: 4px; background: var(--surface-2);
  color: var(--muted); font: 400 10px var(--font-data); letter-spacing: .02em;
}
</style>
