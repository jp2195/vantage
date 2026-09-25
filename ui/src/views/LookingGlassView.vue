<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import DataTable, { type Column } from '@/components/DataTable.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import { formatCount } from '@/lib/formatCount'
import DumpStateMark from '@/components/DumpStateMark.vue'
import PathGraph from '@/components/PathGraph.vue'
import ResultMeta from '@/components/ResultMeta.vue'
import { useFindRoutes, useRouteHistory, useTopology, type RouteFilters } from '@/api/queries'
import type { Graph, HistoryEvent, UnicastRoute } from '@/api/generated'

type Mode = 'prefix' | 'asn' | 'community'
type Tab = 'paths' | 'grouped' | 'diff' | 'topology'

const mode = ref<Mode>('prefix')
const tab = ref<Tab>('paths')
const term = ref('')
const covers = ref(false)
const inPath = ref(false)

const filters = ref<RouteFilters | undefined>(undefined)
// isPending, not isLoading -- see RoutersView.vue's comment on the same
// seam, and DataTable's own. isLoading flips true again on every refetch,
// long after rows are on screen; isPending is the one that means "nothing
// has landed yet", which is what DataTable's `loading` prop asks for. This
// was the one screen that had not applied that lesson.
const { data, isPending, error } = useFindRoutes(filters)

const historyPrefix = ref<string | undefined>(undefined)

/**
 * The windows the Changes tab offers, and the labels it names them by.
 *
 * Every value is a duration Go's time.ParseDuration accepts, because that
 * is what api/handlers.go's since() falls through to once the string is not
 * RFC 3339. It knows ns/us/ms/s/m/h and no day unit at all, so a week is
 * "168h" and "7d" would be a 400 -- an error the operator could neither
 * predict nor explain. The label is where "7 days" is allowed to be said.
 */
const HISTORY_WINDOWS = [
  { value: '1h', label: 'last hour' },
  { value: '6h', label: 'last 6 hours' },
  { value: '24h', label: 'last 24 hours' },
  { value: '168h', label: 'last 7 days' },
]

// Defaulted to the same 1h the daemon would have applied on its own, so
// this changes nothing about what comes back -- only about whether the
// screen can say what it asked for. The endpoint's default was invisible:
// an operator reading "complete as of 14:02:11" under an empty list had no
// way to know they were being told about one hour.
const since = ref('1h')
const windowLabel = computed(
  () => HISTORY_WINDOWS.find((w) => w.value === since.value)?.label ?? since.value,
)

const { data: history, error: historyError } = useRouteHistory(historyPrefix, since)

/**
 * The Topology tab, asking the SAME question the Paths tab asks -- and asking
 * it only while the tab is open.
 *
 * /v1/topology takes /v1/routes' scope parameters exactly, so this forwards
 * the very filters object the search composed rather than building a second
 * scope. A tab with its own scope could answer for a different question than
 * the one on screen beside it, and nothing would say so.
 *
 * The tab gate is a cost decision, not a caching one. This is a SECOND query
 * over the scope, and the endpoint's own measurement says the wide filters
 * (origin_asn=, through_asn=, community=) read the whole table -- 237-262ms,
 * measured on a 2,000,000-row rig. Firing that on every search of the
 * most-used screen in the app, for a tab most searches never open, is a cost
 * nobody asked for; calling it a prefetch would be a performance claim with
 * no measurement behind it, and no such claim is made here.
 */
const topologyScope = computed(() => (tab.value === 'topology' ? filters.value : undefined))
const { data: graphs, error: graphError } = useTopology(topologyScope)

/**
 * The unicast graph, which is the one this screen draws.
 *
 * The same choice the Paths and Grouped tabs already make and say out loud:
 * this screen renders unicast, and names what it is not showing rather than
 * dropping it silently. The other two are named rather than merged in
 * because an EVPN adjacency and a unicast adjacency are not the same kind
 * of edge, and one graph made of both would assert that they are.
 */
const unicastGraph = computed<Graph | undefined>(() => graphs.value?.data.unicast)

const hasGraph = (g: Graph | undefined) => Boolean(g && (g.nodes.length > 0 || g.edges.length > 0))

function graphSize(g: Graph): string {
  const n = g.nodes.length
  const e = g.edges.length
  return `${n} ${n === 1 ? 'AS' : 'ASNs'} and ${e} ${e === 1 ? 'edge' : 'edges'}`
}

/**
 * What the other two families hold for this scope, when they hold anything.
 *
 * Undefined when both are empty, exactly as scopeNote above is: a line
 * reading "0 ASNs and 0 edges" printed beside every graph is noise that
 * teaches an operator to skip the place a real one would appear.
 */
const otherGraphs = computed(() => {
  const vpn = graphs.value?.data.vpn
  const evpn = graphs.value?.data.evpn
  if (!vpn || !evpn || (!hasGraph(vpn) && !hasGraph(evpn))) return undefined
  return (
    `The VPN graph for this scope has ${graphSize(vpn)}, and the EVPN graph has ` +
    `${graphSize(evpn)}. This tab draws unicast only; the AS paths screen draws all three. ` +
    `They are not merged into one picture because an EVPN adjacency and a unicast adjacency ` +
    `are not the same kind of edge.`
  )
})

/**
 * A clicked node, turned back into a search.
 *
 * through_asn=, not origin_asn=: a node drawn in this graph may be transit
 * only, and origin_asn= would answer with nothing for exactly the nodes an
 * operator chasing a path is most likely to click. Dropping the event
 * instead would ship a graph whose nodes look clickable and are not.
 */
function onGraphSelect(asn: number) {
  mode.value = 'asn'
  inPath.value = true
  term.value = String(asn)
  search()
}

const route = useRoute()
const router = useRouter()

/** The question and the view, as a URL carries them. Defaults are omitted so a
 *  shared link stays short and says only what was actually chosen. */
function currentQuery(): Record<string, string> {
  const q: Record<string, string> = {}
  if (term.value.trim()) q.q = term.value.trim()
  if (mode.value !== 'prefix') q.mode = mode.value
  else q.mode = 'prefix'
  if (mode.value === 'prefix' && covers.value) q.covers = '1'
  if (mode.value === 'asn' && inPath.value) q.inPath = '1'
  if (tab.value !== 'paths') q.tab = tab.value
  if (since.value !== '1h') q.since = since.value
  return q
}

const asString = (v: unknown) => (typeof v === 'string' ? v : undefined)

/** True when the address bar already says exactly what this screen is showing. */
function urlMatchesState(): boolean {
  const want = currentQuery()
  const have = route.query
  const keys = new Set([...Object.keys(want), ...Object.keys(have)])
  for (const k of keys) if (want[k] !== have[k]) return false
  return true
}

// Applied on setup AND whenever the query changes underneath us. The second
// half is not optional: Vue Router reuses this component across a query-only
// navigation on the same route, so the back button changes the address bar
// with no second setup() to re-read it. Restoring only at setup leaves the URL
// asserting one search while the screen shows another -- a URL that lies is
// worse than a URL that carries nothing, and PeerDetailView.vue's routerIp hit
// this same instance-reuse fact from the params side.
function applyQuery() {
  const q = route.query
  const seededMode = asString(q.mode)
  mode.value =
    seededMode === 'asn' || seededMode === 'community' || seededMode === 'prefix'
      ? seededMode
      : 'prefix'
  const seededTab = asString(q.tab)
  tab.value =
    seededTab === 'grouped' || seededTab === 'diff' || seededTab === 'topology' || seededTab === 'paths'
      ? seededTab
      : 'paths'
  since.value = asString(q.since) ?? '1h'
  covers.value = q.covers === '1'
  inPath.value = q.inPath === '1'
  const seededTerm = asString(q.q)
  term.value = seededTerm ?? ''
  if (seededTerm) {
    search({ fromUrl: true })
  } else {
    filters.value = undefined
    historyPrefix.value = undefined
  }
}

applyQuery()

// Both watchers carry the same guard, in opposite directions: neither acts
// when the URL and the screen already agree. That is what keeps them from
// chasing each other -- our own push/replace lands in route.query and the
// reader sees nothing to do.
watch(
  () => route.query,
  () => {
    if (urlMatchesState()) return
    applyQuery()
  },
)

// The tab and the window change what you are looking AT, not what was asked.
// replace keeps them in a shareable link without spending a history entry per
// click, which would make back useless for stepping between searches.
watch([tab, since], () => {
  if (urlMatchesState()) return
  router.replace({ query: currentQuery() })
})

function search(opts: { fromUrl?: boolean } = {}) {
  const v = term.value.trim()
  if (!v) return
  if (mode.value === 'prefix') {
    filters.value = covers.value ? { covers: v } : { prefix: v }
    // Only an EXACT prefix search gives the Changes tab something to track.
    // covers= takes an address and answers with every prefix containing it;
    // /v1/routes/history takes an exact prefix and nothing else, so handing
    // it the covering address would return the history of a prefix nobody
    // searched for and label it as this search's changes.
    historyPrefix.value = covers.value ? undefined : v
  } else if (mode.value === 'asn') {
    const asn = Number(v)
    filters.value = inPath.value ? { through_asn: asn } : { origin_asn: asn }
    historyPrefix.value = undefined
  } else {
    filters.value = { community: v }
    historyPrefix.value = undefined
  }
  // A submitted search is a new question, so it gets a history entry: back
  // steps between questions an operator actually asked. Restoring from a link
  // is not a new question -- pushing there would add an entry for arriving.
  if (!opts.fromUrl) router.push({ query: currentQuery() })
}

// Seven columns, all real UnicastRoute fields. rib and path_id are part
// of this shape's identity, not incidental: api/openapi.yaml's
// /v1/rib/unicast description says the same (prefix, path_id) legitimately
// appears under in_pre, in_post and loc_rib for one peer -- verified
// against a live archive -- and this screen's own captured fixture proves
// it again from the /v1/routes side: routes.json carries a real add-path
// pair (same router, peer, rib and prefix, path_id 6 vs 7) that a table
// without both columns would render as one row repeated twice. Uptime,
// updates-per-second and a sparkline have no field on this shape; a
// column for any of them could only be filled by inventing a number.
const columns: Column<UnicastRoute>[] = [
  { id: 'prefix', header: 'Prefix', width: '14%' },
  { id: 'rib', header: 'RIB', width: '88px' },
  { id: 'path_id', header: 'Path ID', numeric: true, width: '74px' },
  { id: 'router_sysname', header: 'Router', width: '12%' },
  { id: 'peer_ip', header: 'Peer', width: '12%' },
  // /v1/routes is one row per (collector, router, peer, rib, prefix,
  // path_id), so a router two collectors monitor renders each route twice.
  // Without this the pair is two rows identical in every rendered column --
  // found in a browser on 2026-09-20, where a real dual-homed router showed
  // exactly that.
  //
  // A column rather than a merge, for the reason PeersView states: the two
  // rows are two real observations that can disagree, and collapsing them
  // would present one collector's attributes as THE route's. The counts
  // beside the table dedupe (see distinctRoutes) because "how many paths"
  // is a question about the network; the table shows what was observed.
  { id: 'collector', header: 'Collector', width: '10%' },
  { id: 'next_hop', header: 'Next hop', width: '12%' },
  { id: 'as_path', header: 'AS path', width: '20%' },
]

// findRoutes answers with a RouteFanout -- { unicast, vpn, evpn } -- because
// one prefix can be carried in several families at once. v1 renders the
// unicast arm; the other two are real and deliberately not shown yet, which
// is a scope decision rather than an oversight. The paths tab below reads
// THIS computed, not `data.value.data` directly: RouteFanout is a keyed
// object, never a flat array, so handing DataTable `data.value?.data` would
// pass it a plain object where an array of rows belongs.
const unicast = computed<UnicastRoute[]>(() => data.value?.data?.unicast ?? [])

/**
 * The collectors the rows on screen came from, when there is more than one.
 *
 * The rail counts distinct routes and the table shows observations, which is
 * the right split -- and on a dual-homed router it also puts "Paths 1"
 * directly above two rows, with nothing saying why. The reasoning was in a
 * commit message and nowhere an operator could read it.
 *
 * Conditional on the rows themselves rather than standing: where one
 * collector answered there is no apparent contradiction to explain, and a
 * permanent sentence about collectors would be noise on the single-collector
 * deployment this project expects to be the common case.
 */
/**
 * The collectors, as a list a sentence can hold. "both" used to be a literal
 * in that sentence, which read "dev-c1 and dev-c2 and dev-c3 both monitor"
 * the moment a third appeared -- and nothing here caps them at two.
 */
const observerList = computed(() => {
  const seen = observingCollectors.value ?? []
  if (seen.length <= 2) return seen.join(' and ')
  return `${seen.slice(0, -1).join(', ')} and ${seen[seen.length - 1]}`
})

const observingCollectors = computed(() => {
  const seen = [...new Set(unicast.value.map((r) => r.collector))].sort()
  return seen.length > 1 ? seen : undefined
})

/**
 * How many distinct ROUTES a set of rows holds, which is not how many rows
 * it holds.
 *
 * /v1/routes answers one row per (collector, router, peer, rib, prefix,
 * path_id), so a router two collectors monitor returns each of its routes
 * twice -- and a naive count over these rows would read "Paths 2" for one
 * path.
 *
 * The key is the route's own identity with the collector REMOVED, which is
 * the same key query/rib.go uses: a network fact must not scale with how
 * many observers watched. `Observing peers` beside this was already right
 * for exactly this reason -- it keys on (router, peer) and never carried a
 * collector.
 *
 * Two collectors CAN disagree, and where they do this still counts one:
 * they are two observations of one route, and which of their attribute
 * sets is current is a question the rows themselves answer, not the count.
 */
type Routeish = Record<string, unknown>

/**
 * The route identity for each family, collector REMOVED. These mirror
 * query/rib.go's own keys: unicast is (router, peer, rib, prefix,
 * path_id); VPN adds the RD, because one prefix under two RDs is two
 * unrelated routes; EVPN is the whole NLRI tuple, because a type-2 MAC
 * route has no prefix at all and several types share the columns they do
 * populate.
 */
const routeKeyFields: Record<'unicast' | 'vpn' | 'evpn', string[]> = {
  unicast: ['router_ip', 'peer_ip', 'rib', 'prefix', 'path_id'],
  vpn: ['router_ip', 'peer_ip', 'rib', 'rd', 'prefix', 'path_id'],
  evpn: [
    'router_ip', 'peer_ip', 'rib', 'route_type', 'rd',
    'prefix', 'mac', 'ip', 'ethernet_tag', 'esi', 'path_id',
  ],
}

function distinctRoutes(rows: Routeish[] | undefined, family: 'unicast' | 'vpn' | 'evpn') {
  const fields = routeKeyFields[family]
  return new Set((rows ?? []).map((r) => fields.map((f) => String(r[f] ?? '')).join('/'))).size
}

/**
 * The rail's facts list: origin AS, paths/peers and shortest/longest path.
 * Every entry is counted over the rows this answer already carried -- no
 * second request, and nothing that needs a dataset this project does not
 * hold.
 *
 * RPKI validity and IRR match are absent because no feed is integrated;
 * first seen is absent because this answer is current state rather than a
 * retention-bounded history; withdrawals 24h is absent because this
 * endpoint takes no such window. They are absent rather than shown empty,
 * because a facts row reading "-" next to "RPKI" invites the reading that
 * the prefix has no ROA.
 *
 * `Carried in` names the families the fanout actually returned. That matters
 * more here than anywhere else in the UI: this screen renders the unicast
 * arm, and a prefix carried in VPN or EVPN too would otherwise look like a
 * unicast-only prefix on the one screen built to answer "who sees this".
 */
/** What the pane's tab row states on the right: a distinct-route count,
 *  e.g. "13 paths". Unicast rows, because the unicast arm is what these
 *  tabs render; the other families are named in the rail's "Carried in"
 *  fact instead. */
const counts = computed(() => [
  { key: 'paths', label: 'Paths', value: formatCount(distinctRoutes(unicast.value, 'unicast')) },
])

const facts = computed<{ label: string; value: string }[]>(() => {
  const fan = data.value?.data
  if (!fan) return []
  const u = fan.unicast ?? []
  const out: { label: string; value: string }[] = []

  const carried: string[] = []
  if (u.length) carried.push(`unicast ${formatCount(distinctRoutes(u, 'unicast'))}`)
  if (fan.vpn?.length) carried.push(`vpn ${formatCount(distinctRoutes(fan.vpn, 'vpn'))}`)
  if (fan.evpn?.length) carried.push(`evpn ${formatCount(distinctRoutes(fan.evpn, 'evpn'))}`)
  out.push({ label: 'Carried in', value: carried.length ? carried.join(' · ') : 'nothing matched' })

  if (!u.length) return out

  out.push({ label: 'Paths', value: formatCount(distinctRoutes(u, 'unicast')) })
  out.push({
    label: 'Observing peers',
    value: formatCount(new Set(u.map((r) => `${r.router_ip}/${r.peer_ip}`)).size),
  })

  // The ASNs the paths END at, which is what "origin" means on this shape;
  // a null origin_asn is a real value (an empty AS_PATH, iBGP), so it is
  // counted as unknown rather than dropped.
  const origins = [...new Set(u.map((r) => r.origin_asn).filter((a) => a !== null))]
  out.push({
    label: origins.length === 1 ? 'Origin AS' : 'Origin ASes',
    value: origins.length ? origins.join(', ') : 'none advertised',
  })

  // Hop counts over the paths in hand. Two identical numbers are printed
  // once: "1 / 1" invites reading a range where there is none.
  const lens = u.map((r) => r.as_path.length)
  const lo = Math.min(...lens)
  const hi = Math.max(...lens)
  out.push({
    label: 'Shortest / longest',
    value: lo === hi ? `${lo} hops` : `${lo} / ${hi} hops`,
  })

  // Which community columns the daemon searched -- the answer's own
  // meta.community_columns. An empty community answer means something very
  // different depending on where it looked.
  const cols = data.value?.meta?.community_columns
  if (cols?.length) out.push({ label: 'Columns searched', value: cols.join(', ') })

  return out
})

/**
 * ResultMeta's "complete" claim is `meta.warnings.length === 0` -- true of
 * every response this screen's own captured fixture demonstrates, INCLUDING
 * one whose vpn arm carries a real matched route this screen never renders
 * (routes.json: vpn has 1 row, evpn has 0, meta.warnings is empty). Without
 * this, the paths tab would print "complete as of ..." directly under a
 * table that had silently dropped a real match -- this project's core
 * defect class, on the screen built to be its showcase. Counted here,
 * across both arms, so the notice below can say a real number rather than
 * "some routes are hidden".
 */
const otherFamiliesCount = computed(
  () => (data.value?.data?.vpn?.length ?? 0) + (data.value?.data?.evpn?.length ?? 0),
)

/**
 * Worded to do three things at once: name a real count rather than vanish
 * into "some routes are hidden"; read as scope, not as an error (nothing
 * failed -- the query succeeded and found them); and say outright that
 * nothing is missing from the network, only from this screen. That last
 * clause matters as much as the count: a notice that let an operator infer
 * routes had vanished from the fleet would be a worse failure than the
 * silence it replaces.
 */
const scopeNote = computed(() => {
  const n = otherFamiliesCount.value
  if (n === 0) return undefined
  const noun = n === 1 ? 'route' : 'routes'
  const verb = n === 1 ? 'is' : 'are'
  return `${n} matched ${noun} ${verb} in VPN/EVPN and not shown on this screen -- this view renders unicast only. Nothing is missing from the network, only from this view.`
})

/** One key per route, not just per prefix: the same (prefix, path_id) is
 * only unique within a (router, peer, rib) -- routes.json's own rows[1] and
 * rows[4] share (prefix, path_id) and differ only by router. A key built
 * from prefix and path_id alone collides on that real pair, which Vue
 * would warn about (a stale DOM node reused across an update) even though
 * a static first render happens to look fine either way -- the same reason
 * the grouped list's own text below also shows router_ip, not just prefix
 * and path_id: two different routes rendering the same LINE is the defect
 * this project audits for regardless of whether the key underneath also
 * collided. */
function rowKey(r: UnicastRoute): string {
  return [r.router_ip, r.peer_ip, r.rib, r.prefix, r.path_id].join('|')
}

/**
 * One key per history event, and a line that shows who reported it.
 *
 * `seq` alone is not unique across a prefix's history: api/openapi.yaml
 * calls it a "collector-assigned per-(peer, session) sequence", so two
 * peers announcing the same prefix each carry their own seq 1, 2, 3 -- and
 * so does the same peer after a session reset. Keyed on seq alone, two real
 * events collided; rendered without a router or peer, they also read as the
 * same line, which is the half that matters even where Vue's duplicate-key
 * warning does not surface (this jsdom setup does not raise it). The line
 * below therefore names the peer as well.
 */
function historyKey(e: HistoryEvent): string {
  return [e.router_ip, e.peer_ip, e.session_id, e.seq].join('|')
}

/**
 * Grouped collapses by the last ASN before the origin -- the upstream.
 *
 * Each group carries BOTH numbers, because they answer different questions
 * and this tab printed only the wrong one. `paths` is distinct routes with
 * the collector removed from the identity -- how many ways this prefix is
 * reachable through that upstream, a fact about the NETWORK -- and is the
 * number the heading shows. `rows` is the observations, which is what the
 * list renders, and stays one entry per (collector, router, peer, rib).
 *
 * The split was drawn on 2026-09-20, and is applied here where it was
 * missed: that change deduped
 * the rail's "Paths" and left this tab grouping raw rows, so the screen
 * contradicted itself -- PATHS 1 in the rail, AS 65001 · 2 in the heading
 * beside it, on the same answer.
 */
const grouped = computed(() => {
  const by = new Map<string, UnicastRoute[]>()
  for (const r of unicast.value) {
    const path = r.as_path
    const upstream = path.length >= 2 ? String(path[path.length - 2]) : 'direct'
    by.set(upstream, [...(by.get(upstream) ?? []), r])
  }
  return [...by.entries()].map(
    ([upstream, rows]) => [upstream, rows, distinctRoutes(rows, 'unicast')] as const,
  )
})

/**
 * History ordered newest-first on the COLLECTOR clock, with seq breaking ties.
 *
 * ts_router is the router's own opinion and the two disagree in real data --
 * sink pins this with TestLookingGlassResolvesLatestOnTheCollectorClock.
 * Sorting on ts_router here would make the UI confidently wrong about which
 * observation is latest, which is worse than showing nothing.
 */
const events = computed<HistoryEvent[]>(() =>
  [...((history.value?.data ?? []) as HistoryEvent[])].sort((a, b) => {
    if (a.ts_collector !== b.ts_collector) return a.ts_collector < b.ts_collector ? 1 : -1
    return -compareSeq(a.seq, b.seq)
  }),
)

/**
 * Orders two `seq` values as the u64 numbers they are.
 *
 * seq arrives as a decimal STRING (api/openapi.yaml: "u64 as string"),
 * because a u64 does not survive a JSON number. A plain `a.seq < b.seq` on
 * those strings compares them character by character, so "10" sorts before
 * "9" and a ts_collector tie -- which is ordinary, since the collector
 * stamps a whole BMP message and one update carries several NLRI -- comes
 * out backwards on the one field api/openapi.yaml calls "the authoritative
 * order".
 *
 * Length first, then plain string order, rather than BigInt: it is exactly
 * equivalent for the decimal, non-negative, leading-zero-free rendering
 * strconv.FormatUint produces, and it cannot throw. BigInt('') and
 * BigInt('n/a') raise, and a throw inside a computed read during render
 * takes the whole screen down -- a worse answer to a malformed seq than a
 * mis-ordered list.
 */
function compareSeq(a: string, b: string): number {
  if (a.length !== b.length) return a.length - b.length
  return a < b ? -1 : a > b ? 1 : 0
}
</script>

<template>
  <section class="screen">
    <ScreenHeader
      title="Looking glass"
      :eyebrow="['who sees this', mode]"
      :counts="filters ? counts : undefined"
    />

    <!-- Two-column shape: a 290px query rail beside the result pane. The
         rail holds the question and what the answer says ABOUT its
         subject; the pane holds the rows. -->
    <div class="split">
      <aside class="rail">
        <form class="query" @submit.prevent="search()">
      <div class="modes">
        <button type="button" :class="{ on: mode === 'prefix' }" @click="mode = 'prefix'">Prefix</button>
        <button type="button" :class="{ on: mode === 'asn' }" @click="mode = 'asn'">ASN</button>
        <button type="button" :class="{ on: mode === 'community' }" @click="mode = 'community'">Community</button>
      </div>
      <input v-model="term" class="mono" :placeholder="mode === 'prefix' ? '10.0.0.0/24' : mode === 'asn' ? '65010' : '65010:100'" />
      <label v-if="mode === 'prefix'"><input v-model="covers" type="checkbox" /> covering</label>
      <label v-if="mode === 'asn'"><input v-model="inPath" type="checkbox" /> in path</label>
          <button type="submit">Search</button>
        </form>

        <!-- Facts about the subject, counted over the rows in hand. RPKI,
             IRR, first-seen and withdrawals-24h rows are absent rather
             than empty, because no feed or endpoint supplies them: "-"
             beside RPKI reads as "no ROA". -->
        <dl v-if="facts.length" class="facts" data-facts>
          <template v-for="f in facts" :key="f.label">
            <dt>{{ f.label }}</dt>
            <dd class="mono">{{ f.value }}</dd>
          </template>
        </dl>
      </aside>

      <div class="pane">
        <div class="tabs">
      <button data-tab="paths" :class="{ on: tab === 'paths' }" @click="tab = 'paths'">Paths</button>
      <button data-tab="grouped" :class="{ on: tab === 'grouped' }" @click="tab = 'grouped'">Grouped</button>
      <button data-tab="diff" :class="{ on: tab === 'diff' }" @click="tab = 'diff'">Changes</button>
      <button data-tab="topology" :class="{ on: tab === 'topology' }" @click="tab = 'topology'">Topology</button>
    </div>

    <!-- Visible regardless of the active tab: the fanout matched these
         routes whichever tab is open, and a note that only showed up on
         the Paths tab would vanish the moment an operator switched to
         Grouped and stayed gone if they forgot to switch back. -->
    <p v-if="scopeNote" class="scope-note" role="status">{{ scopeNote }}</p>

    <!-- Beside the counts it reconciles, and for the same reason the counts
         dedupe at all: "paths" is a question about the network, a row is
         one collector's observation of it. Without this the rail reads
         "Paths 1" over two identical-looking rows and the operator is left
         to guess which number is wrong. -->
    <p
      v-if="observingCollectors"
      class="scope-note"
      data-observers-note
      role="status"
    >
      {{ observerList }} monitor this route, so it is listed once per
      collector below. The counts beside the results are per
      route, not per observation — two collectors watching does not make a
      second path.
    </p>

    <!-- Nothing is answered until something is asked. Rendering the paths
         or grouped tab before a search would print "no rows matched" -- and,
         under the Meta this screen used to manufacture, "complete as of
         HH:MM:SS" beneath it -- about a request never sent. RoutesView
         solves the identical problem with its own `v-if="!scope"`. The
         Changes tab is excluded because it has a narrower thing to say:
         history is per exact prefix, so an ASN or community search leaves
         it with nothing to track even after a search HAS been made. -->
    <p v-if="!filters && tab !== 'diff'" class="quiet">
      Search a prefix, ASN or community to see routes.
    </p>

    <DataTable
      v-else-if="tab === 'paths'"
      :columns="columns"
      :rows="unicast"
      :meta="data?.meta"
      :loading="isPending"
      :error="error ?? undefined"
    >
      <template #cell-as_path="{ row }">
        <span class="mono">{{ (row as UnicastRoute).as_path.join(' ') }}</span>
      </template>
      <!-- A router that sent no sysName has an empty router_sysname. This
           table has no router address column, so the cell falls back to the
           address, as Monitor's event feed does, rather than to routerLabel's
           "(no sysName TLV)": that notice is for tables whose address column
           already identifies the router (see lib/routerLabel.ts). -->
      <template #cell-router_sysname="{ row }">
        {{ (row as UnicastRoute).router_sysname || (row as UnicastRoute).router_ip }}
      </template>
      <!-- The rule that "rows from a still-dumping session get a provisional
           marker" applies to this screen too: it renders the identical
           UnicastRoute shape RoutesView does, and rendered it unmarked, so
           the same route was provisional on one screen and settled fact on
           the other. routes.json proves the case is live rather than
           theoretical -- one of its six captured unicast rows is `unknown`. -->
      <template #cell-prefix="{ row }">
        <span class="mono">{{ (row as UnicastRoute).prefix }}</span>
        <DumpStateMark :state="(row as UnicastRoute).dump_state" />
      </template>
    </DataTable>

    <div v-else-if="tab === 'grouped'" class="groups">
      <!-- Per-group flap counts need history aggregated per group, which
           no endpoint does. Absent rather than zero: a zero would read
           as "this group is stable". -->
      <p class="quiet">Grouped by the upstream ASN in each path. Per-group flap counts are not available — no endpoint aggregates history per group.</p>

      <!-- The same four states DataTable draws for the paths tab, drawn
           again here because this tab does not go through DataTable. They
           must not be confusable: a failed query means "we do not know",
           an empty one means "there is nothing here", and this tab used to
           render both as zero groups in silence with a completeness claim
           underneath. -->
      <p v-if="error" class="error" role="alert">{{ error.message }}</p>
      <p v-else-if="isPending && unicast.length === 0" class="quiet">loading…</p>
      <p v-else-if="unicast.length === 0" class="quiet">no rows matched</p>
      <template v-else>
        <div v-for="[upstream, rows, paths] in grouped" :key="upstream" class="group">
          <!-- Distinct paths, not observations: see `grouped`. The list
               below stays one line per observation, so on a dual-homed
               router this heading is deliberately smaller than the number
               of lines under it -- which is why each line names its
               collector rather than repeating itself verbatim. -->
          <h3 class="mono">AS {{ upstream }} <span class="count">{{ paths }}</span></h3>
          <ul>
            <li v-for="r in rows" :key="rowKey(r)" class="mono">
              {{ r.prefix }}
              <span class="detail">
                {{ r.rib }} #{{ r.path_id }} · {{ r.router_ip }} · {{ r.collector }}
              </span>
              <!-- The same rows the paths tab shows, so the same caveat.
                   An operator who only ever opens Grouped would otherwise
                   read a partial table as a settled one. -->
              <DumpStateMark :state="r.dump_state" />
            </li>
          </ul>
        </div>
      </template>
      <!-- Sibling of the states above, exactly as it is in DataTable, not
           nested inside the has-rows branch. An empty answer that SUCCEEDED
           still carries a completeness claim, and the paths tab prints
           "no rows matched" with "complete as of ..." beneath it for the
           very same query -- a grouped tab that dropped the footer there
           would say less about the identical response, which is the
           inconsistency this tab already had in a different form. Found by
           opening both tabs in a browser against the live archive.

           Gated on `!error` for the reason DataTable's own comment gives:
           `data` can still hold the last SUCCESSFUL fetch's meta after a
           later refetch fails, and "complete" printed under that stale meta
           would contradict the failure on screen beside it. -->
      <ResultMeta v-if="data?.meta && !error" :meta="data.meta" :shown="unicast.length" />
    </div>

    <div v-else-if="tab === 'topology'" class="topology">
      <!-- Four states, and they must not be confusable. A failed request
           means "we do not know"; an empty graph means "there is nothing
           here"; a drawn graph is the answer. The blank canvas a missing
           empty state would leave says none of the three. -->
      <p v-if="graphError" class="error" role="alert">{{ graphError.message }}</p>
      <template v-else-if="unicastGraph">
        <!-- The size, stated rather than left to be inferred from the
             picture: a two-edge graph is small because the answer is small,
             and an operator who cannot tell that from a broken screen has
             been told the wrong thing. -->
        <p class="quiet" data-topology-counts>
          <strong>{{ graphSize(unicastGraph) }}</strong> in unicast — one node per AS, one edge per
          adjacency their paths cross. Click a node to search the routes crossing that AS.
        </p>
        <p v-if="otherGraphs" class="scope-note" data-topology-families>{{ otherGraphs }}</p>
        <PathGraph v-if="hasGraph(unicastGraph)" :graph="unicastGraph" @select="onGraphSelect" />
        <!-- Not an alert, and worded so it cannot read as one: `nodes` and
             `edges` are arrays with no null form, so an empty one is this
             API's positive claim "nothing there" rather than "we did not
             look". A one-hop AS path contributes a node and no edge, so a
             scope can legitimately reach routes and no adjacency. -->
        <p v-else class="quiet" data-topology-empty>
          No unicast adjacency in this scope. The answer carried an empty graph, which this API
          defines as a positive “nothing there” — a route reached in one hop contributes an AS and
          no adjacency at all.
        </p>
        <!-- /v1/topology's own meta, not /v1/routes' -- a different response,
             and this tab is the only thing on screen describing it. It sits
             INSIDE the v-else-if, which is what keeps it off the error state:
             `graphs` can still hold the last SUCCESSFUL fetch's meta after a
             later refetch fails, and "complete" printed under that would
             contradict the failure rendered in its place. -->
        <ResultMeta v-if="graphs?.meta" :meta="graphs.meta" />
      </template>
      <p v-else class="quiet">loading…</p>
    </div>

    <div v-else class="diff">
      <label class="window">
        Window
        <select v-model="since" data-history-window>
          <option v-for="w in HISTORY_WINDOWS" :key="w.value" :value="w.value">{{ w.label }}</option>
        </select>
      </label>

      <!-- Five states, and the first two used to be one sentence. "Changes
           are per prefix. Search a prefix to see them." was printed at an
           operator who had just searched a prefix and whose prefix simply
           had not changed in the hour nobody told them about. -->
      <p v-if="!filters" class="quiet">Changes are per prefix. Search a prefix to see them.</p>
      <p v-else-if="!historyPrefix" class="quiet">
        This search names no exact prefix. Changes come from
        /v1/routes/history, which answers for one exact prefix — a covering,
        ASN or community search has none to ask about.
      </p>
      <p v-else-if="historyError" class="error" role="alert">{{ historyError.message }}</p>
      <p v-else-if="events.length === 0" class="quiet">
        No changes to {{ historyPrefix }} in the {{ windowLabel }}.
      </p>
      <template v-else>
        <!-- The interval, printed where the list is read. ResultMeta's
             "complete as of HH:MM:SS" underneath is a true claim about a
             bounded question, and this is the bound: without it the footer
             reads as complete over all time, which is a partial answer
             wearing a complete one's clothes. -->
        <p class="quiet" data-history-scope>
          Changes to {{ historyPrefix }} in the {{ windowLabel }}.
        </p>
        <ul class="events">
          <li
            v-for="e in events"
            :key="historyKey(e)"
            data-history-row
            :data-ts="e.ts_collector"
            :data-seq="e.seq"
          >
            <span :class="['action', e.action]">{{ e.action }}</span>
            <span class="mono">{{ e.prefix }}</span>
            <!-- Who reported it. Two peers carry independent seq sequences
                 for the same prefix, so a line without them can be two
                 different events rendered identically. -->
            <span class="mono via">{{ e.router_ip }} · {{ e.peer_ip }}</span>
            <span class="mono path">{{ e.as_path.join(' ') }}</span>
            <!-- The collector clock is what ordered this list; the router's
                 own timestamp is shown because it is real, not because it
                 sorts. -->
            <time class="mono when" :title="`router clock: ${e.ts_router}`">{{ e.ts_collector }}</time>
          </li>
        </ul>
        <!-- Only in this branch, never beside a placeholder: before a
             prefix is chosen there is no answer, and "complete as of ..."
             next to "search a prefix to see them" would claim completeness
             about a request never sent. Once there IS a real event list, a
             truncated history reading as complete is the same failure
             DataTable's guard prevents for routes, in a different costume. -->
        <ResultMeta v-if="history?.meta" :meta="history.meta" :shown="events.length" />
      </template>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 12px; }
/* 290px is the rail's fixed width. minmax(0, 1fr) on the pane, not
   1fr: without the 0 minimum a wide table inside it pushes the grid wider
   than the viewport instead of scrolling in its own card. */
.split { display: grid; grid-template-columns: 290px minmax(0, 1fr); gap: 16px; align-items: start; }
.rail {
  display: flex; flex-direction: column; gap: 12px; padding: 12px;
  border: 1px solid var(--line); border-radius: 7px; background: var(--surface-2);
}
.pane { display: flex; flex-direction: column; gap: 10px; min-width: 0; }
.facts { display: grid; grid-template-columns: auto 1fr; gap: 5px 10px; margin: 0; }
.facts dt {
  color: var(--muted); text-transform: uppercase;
  font: 600 9.5px var(--font-ui); letter-spacing: .07em; align-self: center;
}
.facts dd { margin: 0; color: var(--ink-2); font-size: 11.5px; text-align: right; }
@media (max-width: 900px) {
  .split { grid-template-columns: minmax(0, 1fr); }
}
.query { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.modes { display: flex; gap: 2px; }
.modes button, .tabs button { font: 500 11.5px var(--font-ui); padding: 5px 11px; border: 1px solid var(--line-2); background: var(--surface); color: var(--muted); border-radius: 6px; cursor: pointer; }
.modes button.on, .tabs button.on { background: var(--ink); color: var(--on-dark); border-color: var(--ink); }
.query input[type="text"], .query input:not([type]) { border: 1px solid var(--line-2); border-radius: 6px; padding: 6px 10px; font-size: 12px; min-width: 220px; }
.query label { font: 400 11.5px var(--font-ui); color: var(--muted); display: flex; gap: 5px; align-items: center; }
.query button[type="submit"] { background: var(--ink); color: var(--on-dark); border: 0; border-radius: 6px; padding: 7px 13px; font: 500 11.5px var(--font-ui); cursor: pointer; }
.tabs { display: flex; gap: 2px; }
.groups, .diff, .topology { border: 1px solid var(--line); border-radius: 8px; background: var(--surface); padding: 14px 16px; }
.topology { display: flex; flex-direction: column; gap: 10px; }
.group h3 { margin: 10px 0 4px; font: 600 12px var(--font-data); color: var(--ink-2); }
.count { color: var(--muted); font-weight: 400; }
.group ul, .events { list-style: none; margin: 0; padding: 0; font-size: 11.5px; }
.group li { padding: 2px 0; }
.detail { color: var(--muted); font-size: 10.5px; }
.events li { display: flex; gap: 12px; align-items: baseline; padding: 4px 0; border-bottom: 1px solid var(--divider); }
.via { color: var(--muted); font-size: 10.5px; }
.window { display: flex; gap: 7px; align-items: center; margin-bottom: 10px; font: 400 11.5px var(--font-ui); color: var(--muted); }
.window select { border: 1px solid var(--line-2); border-radius: 6px; padding: 4px 8px; font: 400 11.5px var(--font-ui); background: var(--surface); color: var(--ink); }
.action { padding: 1px 7px; border-radius: 4px; font: 500 10px var(--font-ui); }
.announce { background: var(--good-tint); color: var(--good); }
.withdraw { background: var(--bad-tint); color: var(--bad-2); }
.path { color: var(--muted); }
.when { margin-left: auto; color: var(--muted); }
.quiet { margin: 0 0 8px; color: var(--muted); font: 400 11.5px var(--font-ui); }
.error { margin: 0; color: var(--bad-2); font: 400 11.5px var(--font-ui); }
.scope-note { margin: 0; padding: 9px 12px; border-radius: 6px; background: var(--accent-tint); color: var(--accent-ink-2); font: 400 11.5px var(--font-ui); }
</style>
