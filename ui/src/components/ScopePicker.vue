<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { usePeers, useRouters } from '@/api/queries'
import type { Peer, Router } from '@/api/generated'

const emit = defineEmits<{
  (e: 'scope', v: { router: string; peer: string; session: string; collector?: string } | undefined): void
  (e: 'select', v: { router?: string; peer?: string }): void
}>()

// Seeded from the URL by RoutesView so a shared link opens on its scope
// instead of the picker. Only the QUESTION arrives this way -- router and
// peer. The session is resolved from live data below, never accepted from a
// caller, because a session_id from someone else's link is a claim about a
// BMP session that may have ended since.
const props = defineProps<{
  router?: string
  peer?: string
  /**
   * Whether the CONSUMER's own query can address one collector.
   *
   * Required rather than defaulted, because getting it wrong puts a false
   * sentence on screen and there is no safe guess. It is a fact about the
   * endpoint the consumer drives, not a preference:
   *
   *   Routes            /v1/rib/*      takes collector=  -> true
   *   Session history   /v1/events     does not          -> false
   *   Topology          /v1/topology   does not          -> false
   *
   * When true this picker offers the Collector control and says which
   * collector is being walked. When false it offers neither -- a select
   * that changes no request is a dead control, and "walking dev-c1's view"
   * above a table showing dev-c2's rows is the class of claim manual
   * testing on 2026-09-20 found on this very component: a comment here
   * promised a fallback /v1/rib/* never performed. What IS true when a
   * consumer cannot pin depends on what its endpoint does with the second
   * collector's rows, which only the consumer knows -- so it says that
   * itself, through the `ambiguous` slot.
   */
  pinsCollector: boolean
}>()

const routerIp = ref<string | undefined>(props.router)
const peerIp = ref<string | undefined>(props.peer)

// Re-seeded when the props change, not only at setup: RoutesView derives them
// from the URL, and a query-only navigation (the back button) reuses this
// component rather than remounting it. Without this the dropdowns would keep
// showing the previous scope while the address bar named another.
watch(
  () => [props.router, props.peer],
  ([r, p]) => {
    routerIp.value = r
    peerIp.value = p
  },
)
const { data: routers } = useRouters()
const { data: peers } = usePeers(routerIp, ref(undefined))

// One entry per ip, not one per row. api/openapi.yaml documents /v1/routers
// as "one entry per (collector, router)", and Router.collector's own
// description says outright that a router monitored by two collectors
// "yields one entry per collector, not a merged one." Nothing this picker
// reads off a Router row other than ip/sysname is collector-specific (both
// name the one physical router regardless of who is watching it), and
// none of /v1/routers, /v1/peers or /v1/rib/* take a collector= parameter
// -- there is no query this screen could send that would prefer one
// collector's entry over the other's -- so collapsing to one row per ip
// costs nothing a real request could have used.
const routerRows = computed<Router[]>(() => {
  const rows = routers.value?.data ?? []
  const seen = new Map<string, Router>()
  for (const r of rows) if (!seen.has(r.ip)) seen.set(r.ip, r)
  return [...seen.values()]
})

// One entry per peer_ip, not one per row -- but only the rib axis of
// api/openapi.yaml's "one entry per (collector, router, peer, rib)" is
// truly interchangeable. A deployment watching both the pre- and
// post-policy adj-rib for one session has two real rows for the same
// peer_ip that agree on everything else (PeerDetailView.vue hit this exact
// cardinality fact first), and useRibPage never sends rib= -- the walk
// already crosses every rib the chosen peer appears under -- so keeping the
// first row seen loses nothing a rib-scoped walk could have used anyway.
//
// The collector axis is not the same claim. Two collectors run independent
// BMP sessions against the same peer, so session_id and state are real,
// independent facts per collector and CAN disagree -- one session up while
// the other has gone view_lost. /v1/rib/* takes no collector= parameter, so
// the walk cannot address one collector's view over the other's; collapsing
// silently would present an arbitrary collector's session as THE peer's
// session, which is exactly the kind of unstated choice this project audits
// for. `ambiguousPeers` below exists so the template can say so instead of
// asserting a closure the data does not have.
const peerRows = computed<Peer[]>(() => {
  const rows = peers.value?.data ?? []
  const seen = new Map<string, Peer>()
  for (const p of rows) if (!seen.has(p.peer_ip)) seen.set(p.peer_ip, p)
  return [...seen.values()]
})

function groupByPeer(rows: Peer[]): Map<string, Peer[]> {
  const byPeer = new Map<string, Peer[]>()
  for (const p of rows) {
    const group = byPeer.get(p.peer_ip)
    if (group) group.push(p)
    else byPeer.set(p.peer_ip, [p])
  }
  return byPeer
}

/**
 * Peers more than one collector watches -- the peers a walk has to CHOOSE a
 * collector for, because `/v1/rib/*` answers 400 when it cannot tell which
 * view is meant.
 *
 * This is the condition the picker actually acts on, and it is not the same
 * question as whether the collectors agree. Keeping them apart is the whole
 * point: one is a decision the operator must make, the other is a finding.
 */
const multiCollectorPeers = computed<Set<string>>(() => {
  const multi = new Set<string>()
  for (const [peerIpKey, group] of groupByPeer(peers.value?.data ?? [])) {
    if (new Set(group.map((p) => p.collector)).size > 1) multi.add(peerIpKey)
  }
  return multi
})

/**
 * Peers whose collectors genuinely DISAGREE about what they see.
 *
 * `session_id` is deliberately not part of this test, and was the bug it
 * replaces. Each collector mints its own id for the same underlying session
 * -- dev-c1 calls it 1789941661644664702 and dev-c2 calls the same session
 * 1789941661645473726 -- so counting distinct ids detects "a second
 * collector exists" and never "they disagree". The old predicate was
 * `sessions.size > 1 || states.size > 1`, which fired on EVERY dual-homed
 * peer, under a role="alert" reading "Two collectors monitor this peer and
 * disagree on its session or state". Found live at
 * /routes?router=172.22.0.11&peer=10.255.1.1, where both collectors report
 * one in_pre session, both `up`, in perfect agreement.
 *
 * States are compared WITHIN A RIB, not across the peer's rows as a whole.
 * `/v1/peers` is one entry per (collector, router, peer, rib), so a peer
 * mirrored both pre- and post-policy has several rows from ONE collector
 * that can legitimately differ -- different views of the session, not a
 * disagreement about it, and certainly not evidence of a second collector.
 * Comparing across ribs would report a single-collector deployment as
 * having collectors that disagree.
 */
const disagreeingPeers = computed<Set<string>>(() => {
  const disagree = new Set<string>()
  for (const [peerIpKey, group] of groupByPeer(peers.value?.data ?? [])) {
    const statesByRib = new Map<string, Set<string>>()
    for (const p of group) {
      const states = statesByRib.get(p.rib) ?? new Set<string>()
      states.add(p.state)
      statesByRib.set(p.rib, states)
    }
    for (const states of statesByRib.values()) {
      if (states.size > 1) disagree.add(peerIpKey)
    }
  }
  return disagree
})

// collectorsForPeer is every collector watching the CHOSEN peer, which is
// the axis peerRows deliberately collapses. It is a real choice now rather
// than a disclosure: /v1/rib/* takes collector=, so the walk can address one
// collector's view, and on a router two collectors monitor it MUST -- the
// endpoint answers 400 otherwise, which is what made this screen unusable
// on every dual-homed router until 2026-09-20.
const collectorsForPeer = computed<Peer[]>(() => {
  if (!peerIp.value) return []
  return (peers.value?.data ?? []).filter((p) => p.peer_ip === peerIp.value)
})

// Defaults to the first collector rather than to nothing, so the common
// single-collector case needs no interaction and the ambiguous case opens
// on a working walk instead of an empty one. Reset whenever the choice
// stops being available -- a collector kept across a peer change would pin
// the next walk to a collector that peer may not have.
const collectorId = ref<string | undefined>(undefined)
watch(collectorsForPeer, (rows) => {
  if (!rows.some((p) => p.collector === collectorId.value)) {
    collectorId.value = rows[0]?.collector
  }
})

// The session travels with the scope because the cursor is pinned to it. A
// scope without its session would let a walk resume across a session change
// and silently mix two views.
//
// Emits undefined whenever the pair stops being complete and matching, not
// only on the way TO a complete pair. Reconsidering the router (or peer)
// after a full pick is an ordinary interaction, and the prior code only
// ever emitted on success -- so a router change that left the old peerIp
// pointing at nothing in the new peer list emitted nothing at all, and
// RoutesView's `scope` (set once, inside onScope) just sat at the stale
// pair. The consumer needs to be told the picker no longer backs an answer,
// the same way it is told when the picker produces one.
// peerRows is watched, not just the two selections, because a scope seeded
// from the URL is chosen BEFORE /v1/peers has resolved: at first paint the
// seeded peer matches no row, and without this the picker would emit undefined
// once and never revisit it, so every shared link would land on the gate it
// was meant to skip.
//
// Watching the list means a plain 30s refetch retriggers this too, so the emit
// is deduplicated on the resolved triple. Without that, an unchanged scope
// re-emits on every poll and RoutesView reloads the walk -- restarting
// pagination on a timer, which is worse than the problem being solved.
let lastEmitted: string | undefined
watch(
  [routerIp, peerIp, peerRows, collectorId],
  () => {
    // The chosen COLLECTOR's row, not the first row for the peer: the
    // session emitted has to be the one belonging to the collector the walk
    // will name, or the pin is a pair from two different collectors and the
    // endpoint refuses it.
    const byCollector = collectorsForPeer.value.find((x) => x.collector === collectorId.value)
    const p = byCollector ?? peerRows.value.find((x) => x.peer_ip === peerIp.value)
    const resolved =
      routerIp.value && p
        ? {
            router: routerIp.value,
            peer: p.peer_ip,
            session: p.session_id,
            collector: p.collector,
          }
        : undefined
    const key = resolved
      ? `${resolved.router}|${resolved.peer}|${resolved.session}|${resolved.collector}`
      : 'none'
    if (key === lastEmitted) return
    lastEmitted = key
    emit('scope', resolved)
  },
  { immediate: true },
)

// What the two dropdowns currently NAME, as against what they resolve to.
// `scope` above answers "does this pair back an answer"; a consumer that
// can answer for a pair this picker cannot offer -- SessionHistoryView,
// whose endpoint is scoped by router and peer alone -- also needs to know
// whether the operator has moved the selection off the one the URL seeded.
// Those two questions have the same answer (undefined) on `scope` and
// different answers here.
//
// Not immediate: at mount the selections ARE the seeds, and a consumer
// distinguishes "untouched" by never having heard from this event at all.
watch([routerIp, peerIp], ([r, p]) => emit('select', { router: r, peer: p }))

const chosenPeer = computed<Peer | undefined>(() =>
  peerRows.value.find((x) => x.peer_ip === peerIp.value),
)

/**
 * The selected peer when /v1/peers has answered and does not carry it --
 * see the disabled <option> below for why it needs one at all.
 *
 * Gated on an ANSWER (`data` undefined until the first response, the same
 * distinction SessionHistoryView draws), so a seeded peer is not labeled
 * "no current session" during the moment before the list arrives, when it
 * is simply not known yet.
 */
const unlistedPeer = computed<string | undefined>(() => {
  const p = peerIp.value
  if (!p || !peers.value?.data) return undefined
  return peerRows.value.some((x) => x.peer_ip === p) ? undefined : p
})
</script>

<template>
  <div class="picker">
    <label>
      <span>Router</span>
      <select v-model="routerIp">
        <option :value="undefined">—</option>
        <option v-for="r in routerRows" :key="r.ip" :value="r.ip">
          {{ r.sysname }} ({{ r.ip }})
        </option>
      </select>
    </label>
    <label>
      <span>Peer</span>
      <select v-model="peerIp" :disabled="!routerIp">
        <option :value="undefined">—</option>
        <!-- A selection the list cannot contain: a peer seeded from a URL
             whose session has ended, which /v1/peers does not carry. Without
             an option holding that value the control renders BLANK (not
             "—"), because a <select> whose v-model matches no option has
             selectedIndex -1 -- found in a browser, where it reads as a
             broken control beside a populated Router dropdown.

             Disabled, so it names the selection without becoming one: this
             picker is shared with RoutesView, whose walk pins its cursor to
             a session, and an ended session is exactly what it must not be
             able to start against. `scope` still emits undefined for this
             pair, so no consumer's behavior changes -- only what an
             operator can see. -->
        <option v-if="unlistedPeer" :value="unlistedPeer" disabled>
          {{ unlistedPeer }} · no current session
        </option>
        <option v-for="p in peerRows" :key="p.peer_ip" :value="p.peer_ip">
          {{ p.peer_ip }} · AS{{ p.asn }}{{
            disagreeingPeers.has(p.peer_ip)
              ? ' · collectors disagree'
              : multiCollectorPeers.has(p.peer_ip)
                ? ' · 2 collectors'
                : ''
          }}
        </option>
      </select>
    </label>

    <!-- Rendered only when there is something to choose AND the consumer
         can act on the choice. A router with one collector needs no control,
         and offering a one-option select would imply a decision nobody has
         to make; on a consumer whose endpoint takes no collector= the
         select would imply a decision nobody CAN make. -->
    <label v-if="pinsCollector && collectorsForPeer.length > 1">
      <span>Collector</span>
      <select v-model="collectorId" data-collector>
        <option v-for="c in collectorsForPeer" :key="c.collector" :value="c.collector">
          {{ c.collector }} · {{ c.state }}
        </option>
      </select>
    </label>
  </div>

  <!-- States what is true: two collectors disagree, one is selected, and
       the selection is the control beside it. It does not say "this walk
       uses <collector>'s session; the other collector's view is not
       shown": /v1/rib/* performs no such fallback, and a request without
       collector= that needs one answers 400. -->
  <p
    v-if="chosenPeer && multiCollectorPeers.has(chosenPeer.peer_ip)"
    class="ambiguous"
    role="alert"
  >
    <template v-if="disagreeingPeers.has(chosenPeer.peer_ip)">
      Two collectors monitor this peer and disagree on its state.
    </template>
    <!-- States what is true and stops. Adding "each keeps its own session
         id, which is not a disagreement" would be accurate, but it raises a
         doubt the operator did not have in order to answer it, and the word
         belongs in disagreeingPeers' comment where the next reader of the
         predicate will need it. -->
    <template v-else> Two collectors monitor this peer and agree on its state. </template>
    <template v-if="pinsCollector">
      Walking {{ collectorId }}'s view; the other collector's is a separate
      answer, not a continuation of this one — switch with the Collector
      control.
    </template>
    <!-- No fallback content, deliberately. A consumer that cannot pin and
         says nothing leaves a sentence that is TRUE and incomplete; a
         fallback repeating the pinning copy would leave one that is false,
         which is the failure this prop exists to end. -->
    <slot v-else name="ambiguous" />
  </p>
</template>

<style scoped>
/* Wraps, and each control may be narrower than its longest option. A
   <select> is as wide as its longest option by default, and a peer option
   reads "10.0.0.80 · AS4200000002 · 2 collectors": on a 390px phone the
   Peer and Collector controls ran 199px past the right edge and the whole
   page scrolled sideways, on Routes and on Session history. Nothing changes
   where the three fit on one line. */
.picker { display: flex; flex-wrap: wrap; gap: 14px; align-items: flex-end; }
label { display: flex; flex-direction: column; gap: 4px; min-width: 0; max-width: 100%; font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
select { max-width: 100%; font: 400 12px var(--font-ui); padding: 6px 9px; border: 1px solid var(--line-2); border-radius: 6px; background: var(--surface); color: var(--ink); }
.ambiguous { margin: 8px 0 0; padding: 9px 12px; border-radius: 6px; background: var(--accent-tint); color: var(--accent-ink-2); font: 400 11.5px var(--font-ui); }
</style>
