<script lang="ts">
import type { Column } from '@/components/DataTable.vue'
import type { LsPrefix } from '@/api/generated'

/**
 * The prefix columns the rail renders.
 *
 * A separate `<script>` block because `<script setup>` cannot carry a named
 * export, and this one is exported so the column guard checks the columns the
 * screen ACTUALLY renders rather than a copy of them kept beside it -- the
 * template below iterates this array for both its headers and its cells, so
 * there is nothing for a guarded copy to drift away from.
 *
 * `Column<LsPrefix>` rather than a local `{ id: string; header: string }`:
 * DataTable's own type is `id: keyof Row`, so a column naming a field
 * `LsPrefix` does not have is a compile error here rather than an empty cell
 * in production. Re-declaring that shape locally would be a second copy of
 * the contract with nothing keeping it in sync.
 *
 * `ospf_route_type` is deliberately absent. It is a real field, but it is a
 * small integer registry this UI has no table for, and a bare number under a
 * header called "Type" is a column an operator cannot read. The three here
 * show the prefixes a node originates, with their metrics and prefix-SIDs.
 */
export const RAIL_PREFIX_COLUMNS: Column<LsPrefix>[] = [
  { id: 'prefix', header: 'Prefix', width: '38%' },
  { id: 'prefix_metric', header: 'Metric', numeric: true, width: '110px' },
  { id: 'prefix_sid', header: 'Prefix SID', numeric: true, width: '120px' },
]
</script>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import LinkStateGraph from '@/components/LinkStateGraph.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import ResultMeta from '@/components/ResultMeta.vue'
import { useLinkState, useRouters, type LinkStateScope } from '@/api/queries'
import { distinctAdjacencies, distinctNodes, drawnAdjacencies } from '@/lib/lsAdjacencies'
import type { LsNode, Meta, Router } from '@/api/generated'

/**
 * One router's view of one IGP topology.
 *
 * The premise, not just a caption: the four routers in a lab archive report
 * 7, 6, 5 and 1 nodes for the same lab network, because no router holds
 * "the" topology -- it holds its own LSDB, and BMP mirrors it. Every graph,
 * every count and the URL on this screen name whose view it is.
 *
 * The other invariant this screen carries end to end: every number here is
 * a count of DISTINCT objects. A lab archive holds 139 `ls_nodes` rows for
 * 7 distinct nodes in the richest view, because a node is re-observed on
 * every refresh and `/v1/ls/nodes` groups by peer and rib as well as by node
 * -- one router with two BMP peers reports every node twice. Reporting a row
 * count as a topology count is this repository's most recurring defect, and
 * link-state data has the widest gap between rows and facts of any table
 * here.
 */

const route = useRoute()
const router = useRouter()

/**
 * The scope, seeded from the URL so a shared link opens on its question.
 *
 * This screen's scope must be shareable via URL: "look at what xr-p2
 * sees versus nx-p3" is the premise of THIS screen, so all three parts of
 * the scope travel in the address.
 *
 * protocol and area are only seeded alongside a router. On their own they
 * name a narrowing of nobody's view, and applying one to whatever router is
 * picked next would answer a question the link never asked.
 */
const seededRouter = queryString('router')
const chosenRouter = ref<string | undefined>(seededRouter)
const protocol = ref<number | undefined>(seededRouter ? queryNumber('protocol') : undefined)
const area = ref<number | undefined>(seededRouter ? queryNumber('area') : undefined)

/** Which of the layout's equally valid arrangements the canvas draws. */
const seed = ref(0)
const selectedKey = ref<string | undefined>(undefined)

function queryString(name: string): string | undefined {
  const raw = route.query[name]
  return typeof raw === 'string' && raw !== '' ? raw : undefined
}

/**
 * A query parameter as an integer, with no falsy-zero trap.
 *
 * NOT `Number(raw) || undefined`. Area 0 is the OSPF backbone and 85% of the
 * archive's `ls_nodes` rows, so that idiom would drop the single most common
 * value in this data and silently widen a shared link's question from one
 * area to every area. A value that is not an integer is dropped instead of
 * sent: `protocol=` reaches the wire as a string either way, but an `area=`
 * of NaN is a request nobody could have meant.
 */
function queryNumber(name: string): number | undefined {
  const raw = queryString(name)
  if (raw === undefined) return undefined
  const n = Number(raw)
  return Number.isInteger(n) ? n : undefined
}

const { data: routersData } = useRouters()

/**
 * One entry per router IP, the same collapse ScopePicker documents: /v1/routers
 * is "one entry per (collector, router)", and a router watched by two
 * collectors is one router to ask about. Nothing this select reads off the row
 * is collector-specific, and `/v1/ls/*` takes no collector= parameter, so
 * there is no request the second entry could have addressed.
 *
 * An empty list while the query is unanswered is not the `?? []` this screen
 * refuses elsewhere: it is the OPTIONS of a control, not an answer about the
 * network, and until a router is chosen the screen is in its "nothing asked"
 * state regardless.
 */
const routerRows = computed<Router[]>(() => {
  const held = new Map<string, Router>()
  for (const r of routersData.value?.data ?? []) if (!held.has(r.ip)) held.set(r.ip, r)
  return [...held.values()]
})

/**
 * How a router is named on this screen: "nx-p3 (10.0.0.11)", or the bare IP.
 *
 * An empty `sysname` is a real answer -- a BMP session whose initiation
 * carried no name -- and the IP is what identifies the router in that case,
 * as it does everywhere else in this UI. LinkStateGraph's own observer
 * caption applies exactly this rule; writing it the shorter way here
 * produced " (10.0.0.11)", a leading space where a name should be, and that
 * string reached the whose-view line, the summary and the empty view.
 */
function labelFor(row: Router): string {
  return row.sysname ? `${row.sysname} (${row.ip})` : row.ip
}

/** The chosen router as named above, or its bare IP until /v1/routers answers. */
const routerLabel = computed(() => {
  const ip = chosenRouter.value
  if (!ip) return ''
  const row = routerRows.value.find((r) => r.ip === ip)
  return row ? labelFor(row) : ip
})

const scope = ref<LinkStateScope | undefined>(undefined)
const { nodes, links, prefixes, nodesMeta, linksMeta, prefixesMeta, error } = useLinkState(scope)

/**
 * `replace`, not `push`.
 *
 * The scope control is this screen's whole question, and an operator narrows
 * it in three steps -- router, then protocol, then area. Pushing one history
 * entry per dropdown would make Back walk the operator through the
 * intermediate questions they were never interested in, three presses deep,
 * before leaving the screen. The address still carries the scope, which is
 * what makes the link shareable; what it does not do is treat every pick as
 * a page.
 */
let hadScope = false
function syncUrl() {
  const q: Record<string, string> = {}
  if (chosenRouter.value) {
    q.router = chosenRouter.value
    if (protocol.value !== undefined) q.protocol = String(protocol.value)
    if (area.value !== undefined) q.area = String(area.value)
  }
  if (!q.router) {
    // "No scope yet" and "the scope was cleared" arrive as the same empty
    // object, and only the second should touch the address. Mounting on a
    // bare /link-state must not navigate.
    if (hadScope && (route.query.router || route.query.protocol || route.query.area)) {
      router.replace({ query: {} })
    }
    return
  }
  hadScope = true
  // Already what the address says: a screen opened on a shared link that
  // rewrote its own URL would replace the entry the operator arrived on.
  if (
    route.query.router === q.router &&
    route.query.protocol === q.protocol &&
    route.query.area === q.area
  ) {
    return
  }
  router.replace({ query: q })
}

// One place the three controls become one question, and the same place the
// address bar is written. Keeping them together is what makes a shared link
// and the request behind it the same scope by construction.
watch(
  [chosenRouter, protocol, area],
  () => {
    scope.value = chosenRouter.value
      ? { router: chosenRouter.value, protocol: protocol.value, area: area.value }
      : undefined
    syncUrl()
  },
  { immediate: true },
)

// The address changing under a mounted screen is the back button: a
// query-only navigation reuses this component rather than remounting it, so
// without this the address would say one scope and the canvas would draw
// another. Copied from RoutesView.vue's own seed, which learned it the same
// way.
watch(
  () => [route.query.router, route.query.protocol, route.query.area].join(' '),
  () => {
    const r = queryString('router')
    chosenRouter.value = r
    protocol.value = r ? queryNumber('protocol') : undefined
    area.value = r ? queryNumber('area') : undefined
  },
)

function onRouterPicked(raw: string) {
  const next = raw || undefined
  if (next === chosenRouter.value) return
  chosenRouter.value = next
  // A protocol and an area are values out of ONE router's database. Carrying
  // either across a router change would narrow the new router's view by a
  // number that came from somebody else's, and in the common case would
  // narrow it to nothing at all.
  protocol.value = undefined
  area.value = undefined
}

function onProtocolPicked(raw: string) {
  protocol.value = raw === '' ? undefined : Number(raw)
}

function onAreaPicked(raw: string) {
  area.value = raw === '' ? undefined : Number(raw)
}

/**
 * The protocols this answer holds, with the name each row gives its own.
 *
 * Derived from the node rows and nothing else -- see `areaOptions` for why
 * the link rows are not consulted for the area, and the same argument keeps
 * both lists reading one source. An unnamed protocol ID renders as its
 * number, which is the contract's own position: "" is a positive answer and
 * the number beside it is the whole truth.
 */
const protocolOptions = computed<[number, string][]>(() => {
  const rows = nodes.value
  if (!rows) return []
  const named = new Map<number, string>()
  for (const r of rows) {
    if (!named.has(r.protocol)) named.set(r.protocol, r.protocol_name || `protocol ${r.protocol}`)
  }
  return [...named].sort((a, b) => a[0] - b[0])
})

/**
 * The areas this answer holds, off the NODE rows alone.
 *
 * `/v1/ls/links` matches area at EITHER end -- the contract's own words: "an
 * area-crossing link is returned by a query for either of its two areas" --
 * so reading endpoint areas would report a legitimate ABR link as a second
 * area in this router's view and grow a control over a topology that is not
 * there. LinkStateGraph reads areas the same way, for the same reason.
 */
const areaOptions = computed<number[]>(() => {
  const rows = nodes.value
  if (!rows) return []
  return [...new Set(rows.map((r) => r.area))].sort((a, b) => a - b)
})

/**
 * A scope control renders where the data holds more than one value -- or
 * where a narrowing is already in effect, so it can be undone.
 *
 * The second half is not decoration. Narrowing to one protocol makes the
 * next answer hold exactly that protocol, so a control derived from the
 * answer alone would DISAPPEAR at the moment it was used, with `protocol=`
 * still in the address: reloading the shared link would restore the narrowed
 * question and the operator would have no way back except editing the URL.
 * The "every protocol" option below is what widens it again.
 *
 * In a lab archive neither control renders: only protocol 3 (OSPFv2)
 * appears anywhere in the capture. An IS-IS L1/L2 router grows the controls
 * rather than silently merging two topologies onto one canvas.
 */
const showProtocol = computed(
  () => protocolOptions.value.length > 1 || protocol.value !== undefined,
)
const showArea = computed(() => areaOptions.value.length > 1 || area.value !== undefined)

/** The narrowing, as words, for the line that names whose view this is. */
const scopeNarrowing = computed(() => {
  const parts: string[] = []
  if (protocol.value !== undefined) {
    const named = protocolOptions.value.find(([n]) => n === protocol.value)
    parts.push(named ? named[1] : `protocol ${protocol.value}`)
  }
  if (area.value !== undefined) parts.push(`area ${area.value}`)
  return parts.length > 0 ? ` · ${parts.join(' · ')}` : ''
})

/**
 * What the canvas draws, and therefore what the summary counts: the ONE
 * object both of them read.
 *
 * `undefined` until both queries have answered, and never `?? []`. An empty
 * array here would render "we looked and there is nothing" about a question
 * that has not been answered yet -- the exact claim the undefined-until-
 * resolved pattern exists to prevent, and the difference between "this
 * router has no IGP topology" and "the daemon has not replied".
 *
 * Both collapses come from @/lib/lsAdjacencies -- `distinctNodes` on the node
 * axis and `drawnAdjacencies` on the link axis -- rather than from a second
 * idea here of what one node or one adjacency is. That module is also what
 * the canvas calls for both, so the set laid out, the set drawn and the set
 * counted are one set by construction rather than by two implementations
 * agreeing, which keeps the summary counts and the canvas from disagreeing
 * about what is drawn.
 */
const drawn = computed(() => {
  const nodeRows = nodes.value
  const linkRows = links.value
  if (!nodeRows || !linkRows) return undefined
  const distinct = distinctNodes(nodeRows)
  return {
    nodes: distinct,
    links: linkRows,
    adjacencies: drawnAdjacencies(linkRows, distinct),
  }
})

/**
 * Distinct prefixes, keyed (node, prefix).
 *
 * Two nodes originating one prefix is two facts about this topology -- an
 * anycast address, or a prefix leaked from another area -- so the pair is
 * the identity rather than the CIDR alone. The same list feeds the rail, so
 * the count above it and the rows in it cannot disagree.
 */
const distinctPrefixes = computed(() => {
  const rows = prefixes.value
  if (!rows) return undefined
  const held = new Map<string, LsPrefix>()
  for (const p of rows) {
    const key = `${p.node_key}|${p.prefix}`
    if (!held.has(key)) held.set(key, p)
  }
  return [...held.values()]
})

/**
 * The selected node, resolved against what is DRAWN rather than held as an
 * object.
 *
 * A selection stops being about the picture the moment the scope narrows to
 * another topology or another router. Resolving it here means the rail can
 * never describe a node the canvas beside it does not draw.
 */
const selected = computed(() => drawn.value?.nodes.find((n) => n.node_key === selectedKey.value))

const selectedPrefixes = computed(() => {
  // Never read while this is undefined: the rail says so instead of
  // rendering an empty table over an unanswered query.
  if (!distinctPrefixes.value) return []
  const key = selected.value?.node_key
  return key === undefined ? [] : distinctPrefixes.value.filter((p) => p.node_key === key)
})

/**
 * Prefixes counted here that no box on the canvas can be selected to show.
 *
 * Counting prefixes over the whole answer is deliberate -- the canvas draws
 * no prefix at all (nx-p3's real view is 7 nodes and 27 prefixes over 16
 * networks, and boxing either number alongside the nodes is past the
 * measured 13+ node threshold at which the shared layout overlaps on every
 * seed), so there is no drawn set for them to agree with. Through
 * `/v1/ls/prefixes` a lab archive answers 27 rows, 27 distinct (node,
 * prefix) pairs and 16 distinct CIDRs. But a thin LSDB can report a prefix
 * whose ORIGINATING node row this router never received, and that prefix is
 * then in the count with no rail row reachable for it. Stated on screen when
 * it happens rather than silently making the two numbers not add up.
 */
const unreachablePrefixes = computed(() => {
  const held = distinctPrefixes.value
  if (!held || !drawn.value) return 0
  const drawnKeys = new Set(drawn.value.nodes.map((n) => n.node_key))
  return held.filter((p) => !drawnKeys.has(p.node_key)).length
})

/**
 * Adjacency directions this answer carries that no curve on the canvas can
 * draw, because one of their ends is a node this router did not report.
 *
 * The link-side counterpart to `unreachablePrefixes` above, and it exists
 * for the same reason: `drawnAdjacencies` filters out an adjacency naming a
 * node outside the node set, and a filter whose OUTPUT is counted on screen
 * while its DISCARDS are silent leaves the screen quietly short of the rows
 * it was handed. xr-p2's captured view is exactly that shape -- two link
 * rows, two distinct directions, both naming nodes its own answer does not
 * carry -- so nothing at all was said about the only adjacency data that
 * router returned.
 *
 * Counted in DIRECTIONS against `distinctAdjacencies` over the same rows,
 * not in rows: reporting "2 link rows dropped" would be a row count standing
 * in for a topology count, pointed at the discards instead of at the total.
 *
 * Reported, not explained: a node absent from this router's answer may be
 * outside its database, outside this scope, or held by a peer whose dump is
 * still arriving, and this data decides none of those.
 */
const undrawnAdjacencies = computed(() => {
  const d = drawn.value
  if (!d) return 0
  return distinctAdjacencies(d.links).length - d.adjacencies.length
})

/**
 * The name the RAIL shows for the selected node, and where it came from.
 *
 * This is the node's OWN row, which is a different claim from the canvas's
 * label: `LsEndpoint.label` is resolved by the API across the fleet, so a node
 * named only by another router draws as `xr-p1` on the canvas and has nothing
 * but a router-id in its own row. The rail keeps the conservative claim -- a
 * name this row carries, or the identifier it does not -- and says which it is
 * showing, because a heading that silently disagreed with the box the operator
 * clicked is the borrowed-name confusion LinkStateGraph.vue's rule 1 (name
 * whose view it is) exists to prevent, one layer in.
 */
const selectedName = computed(() => {
  const n = selected.value
  if (!n) return undefined
  if (n.name) return { text: n.name, from: 'the name in this node\'s own row' }
  const id = n.router_id_v4 || n.router_id
  return {
    text: id,
    from: "this node's row carries no name, so its router-id stands in",
  }
})

/** `1 node`, `7 nodes`. */
function plural(n: number, one: string, many: string) {
  return `${n} ${n === 1 ? one : many}`
}

/**
 * The adjacency count, in the terms the canvas draws it in.
 *
 * DIRECTIONS, not node pairs, and the words have to say so. `lsAdjacencies`
 * keys an adjacency by the ORDERED pair `${local}-${remote}`, the canvas
 * draws one curve for each, so describing that directed number as
 * unordered pairs misleads: "20 adjacencies" over 10 pairs drawn as 20
 * curves, beside a says line claiming a pair was drawn as one line and
 * counted once.
 *
 * The drawing is right and was left alone. IGP metrics are per direction and
 * the two ends genuinely disagree -- one pair in the restored archive is
 * metric 40 one way and 1 the other -- so collapsing the two curves would
 * hide an asymmetric-metric link, which is a real operational finding.
 *
 * The link total is the sum of the canvas's own `×N` markers: `members` is
 * how many distinct links one DIRECTION advertises, so a link both ends
 * report contributes one to each direction. `query.LSLink`'s comment is why
 * it is printed at all -- reporting parallel links as one adjacency is the
 * difference between "this path has no redundancy" and "it does".
 */
const adjacencyPhrase = computed(() => {
  const adjacencies = drawn.value?.adjacencies
  if (!adjacencies) return ''
  const directions = adjacencies.length
  const linkCount = adjacencies.reduce((n, a) => n + a.members, 0)
  const counted = `${directions} adjacency ${directions === 1 ? 'direction' : 'directions'}`
  return directions === linkCount ? counted : `${counted} over ${linkCount} advertised links`
})

/**
 * The prefix count, with the second axis that keeps it from reading as a row
 * count.
 *
 * The headline number is originations -- one per (node, prefix) pair, the
 * identity `distinctPrefixes` keys on and the counts line states. The second
 * is how many distinct NETWORKS those originations name, and it is here
 * because of what the only real view measures: nx-p3's prefixes leg is 27
 * rows, 27 distinct (node, prefix) pairs and 16 distinct CIDRs. Collapsing
 * to distinct objects is a no-op on that answer -- 27 equals 27 -- so a
 * lone "27 prefixes" is a number an operator cannot tell from the row
 * count, and so is a test asserting it. The 16 is a number no row count can
 * produce.
 *
 * It is also the real shape of the data rather than a decoration: eight
 * transit /31s are advertised by the routers at both ends, a ninth by four
 * nodes, and seven loopback /32s by one node each -- 8x2 + 4 + 7 = 27
 * originations over 16 networks. "27 prefixes" alone invites the reading
 * that this view holds 27 networks, which is wrong by eleven.
 *
 * Built like `adjacencyPhrase` above, including the suppression: when the
 * two numbers are equal the second clause says nothing and is left off,
 * rather than printing "N prefixes over N networks" on every thin view.
 */
const prefixPhrase = computed(() => {
  const held = distinctPrefixes.value
  if (!held) return undefined
  const counted = plural(held.length, 'prefix', 'prefixes')
  const networks = new Set(held.map((p) => p.prefix)).size
  return held.length === networks
    ? counted
    : `${counted} over ${plural(networks, 'network', 'networks')}`
})

const summaryCounts = computed(() => {
  if (!drawn.value) return ''
  const parts = [plural(drawn.value.nodes.length, 'node', 'nodes'), adjacencyPhrase.value]
  // Omitted rather than guessed at while /v1/ls/prefixes is unanswered: a
  // "0 prefixes" printed over a pending query is the same false claim an
  // empty canvas would be.
  const phrase = prefixPhrase.value
  if (phrase) parts.push(phrase)
  return parts.join(' · ')
})

/**
 * The three answers' metas, each labeled with the query it describes.
 *
 * Each of the three answers carries its own completeness claim, per
 * ANSWER, not per screen. `handleLSLinks` (api/handlers.go:1700-1711) and
 * `handleLSPrefixes` (1775-1784) append their own `truncated` and
 * `session_dumping` warnings exactly as `handleLSNodes` does, so the links
 * answer can be a sample while the nodes answer is whole -- and then the
 * canvas is short of adjacencies, the adjacency count is understated, and
 * an unlabeled "complete as of" beside them would be a false completeness
 * claim.
 *
 * A leg appears here only once its answer exists, so a pending query is
 * never described as anything.
 */
interface AnswerLeg {
  /** Which query, in the words of the URL it came from. */
  leg: 'nodes' | 'links' | 'prefixes'
  meta: Meta
  /** Rows RETURNED by that leg, which is what ResultMeta counts truncation in. */
  shown: number
}

const legs = computed<AnswerLeg[]>(() => {
  const out: AnswerLeg[] = []
  const add = (leg: AnswerLeg['leg'], meta: Meta | undefined, rows: unknown[] | undefined) => {
    if (meta && rows) out.push({ leg, meta, shown: rows.length })
  }
  add('nodes', nodesMeta.value, nodes.value)
  add('links', linksMeta.value, links.value)
  add('prefixes', prefixesMeta.value, prefixes.value)
  return out
})

/**
 * Whether any of the three answers carries a caveat.
 *
 * Reading `meta.warnings` is not recomputing anything -- it is the same
 * field ResultMeta itself reads, and nothing here derives a warning from row
 * counts. What it decides is only which footer to mount: one unlabeled
 * completeness line when all three answers are in and all three are clean
 * (the sentence is then true of every one of them), or a labeled line per
 * WARNED leg the moment any of them warns -- and no completeness sentence at
 * all, because "complete as of" beside a short canvas would be a false
 * claim however true it may be of some other leg.
 */
const warnedLegs = computed(() => legs.value.filter((l) => l.meta.warnings.length > 0))
const allLegsClean = computed(() => legs.value.length === 3 && warnedLegs.value.length === 0)

/**
 * A prefix-SID, with the one thing that changes what the number MEANS.
 *
 * `has_prefix_sid` separates an absent Prefix-SID TLV from index 0, which is
 * a legal SID, so a missing one is a dash rather than a zero. And the
 * contract says in as many words to read `prefix_sid_flags` before
 * `prefix_sid`: with RFC 9085's V and L flags set the number is an absolute
 * label rather than an index into the SRGB shown above it, and a reader who
 * takes one for the other is wrong by the SRGB base.
 *
 * The flag BITS are not decoded here. Nothing in this repository names their
 * positions -- `bgp/linkstate.go` keeps the byte and says why without
 * masking it, and every SID observed in lab captures so far has flags 0 --
 * so a mask invented on this screen would be a claim about the wire that
 * nothing on this branch can check. A non-zero flag byte is shown instead,
 * which tells a reader the number depends on it without pretending to say
 * how.
 */
function sidText(row: LsPrefix): string {
  if (!row.has_prefix_sid) return '—'
  return row.prefix_sid_flags === 0
    ? String(row.prefix_sid)
    : `${row.prefix_sid} (flags ${row.prefix_sid_flags})`
}

function cellOf(row: LsPrefix, id: Column<LsPrefix>['id']): string {
  switch (id) {
    case 'prefix':
      return row.prefix
    case 'prefix_metric':
      return String(row.prefix_metric)
    case 'prefix_sid':
      return sidText(row)
    // Reached only by a column added to RAIL_PREFIX_COLUMNS without a case
    // here; renders the field rather than an empty cell.
    default:
      return String(row[id])
  }
}

/** The protocol as the row names it, or its number when it carries no name. */
function protocolOf(row: LsNode): string {
  return row.protocol_name || `protocol ${row.protocol}`
}

function select(nodeKey: string) {
  selectedKey.value = nodeKey
}
</script>

<template>
  <section class="screen">
    <!-- The counts stay in their own sentence below rather than moving into
         this header: "20 adjacency directions over 22 advertised links" is
         phrasing that carries the distinction, and three bare numbers in a
         counts row would lose it. -->
    <ScreenHeader
      title="Link state"
      :eyebrow="chosenRouter ? ['one router\'s igp view', routerLabel] : ['one router\'s igp view']"
    />
    <p class="lede">
      One router's view of one IGP topology, as its BMP feed mirrors it. Routers disagree about the
      topology by design — each holds its own link-state database, and this screen shows one of
      them at a time rather than a merge no data supports.
    </p>

    <div class="scope">
      <label>
        <span>Router</span>
        <select
          data-router
          :value="chosenRouter ?? ''"
          @change="onRouterPicked(($event.target as HTMLSelectElement).value)"
        >
          <option value="">—</option>
          <option v-for="r in routerRows" :key="r.ip" :value="r.ip">{{ labelFor(r) }}</option>
        </select>
      </label>

      <!-- One graph is one topology, scoped (router, protocol, area).
           These two render only where the data holds more than one
           value, or where one of them is already narrowing the view. -->
      <label v-if="showProtocol">
        <span>Protocol</span>
        <select
          data-protocol
          :value="protocol === undefined ? '' : String(protocol)"
          @change="onProtocolPicked(($event.target as HTMLSelectElement).value)"
        >
          <option value="">every protocol</option>
          <option v-for="[num, label] in protocolOptions" :key="num" :value="String(num)">
            {{ label }}
          </option>
        </select>
      </label>

      <label v-if="showArea">
        <span>Area</span>
        <select
          data-area
          :value="area === undefined ? '' : String(area)"
          @change="onAreaPicked(($event.target as HTMLSelectElement).value)"
        >
          <option value="">every area</option>
          <option v-for="a in areaOptions" :key="a" :value="String(a)">area {{ a }}</option>
        </select>
      </label>
    </div>

    <!-- Nothing is answered until something is asked. The daemon does not
         refuse an unscoped /v1/ls request -- it answers the whole fleet's
         topology, which is precisely the fleet-wide merge no router holds
         -- so the gate is here and in the composable, not in the HTTP
         layer. -->
    <p v-if="!scope" class="quiet" data-nothing-asked>
      Choose a router to draw its link-state view. Each router reports the topology as its own IGP
      database holds it; there is no fleet-wide view to ask for.
    </p>

    <template v-else>
      <!-- The picture, the counts and the address all name the same
           router, on the screen as well as on the canvas. -->
      <p class="whose" data-whose-view>
        One router's link-state view: <b>{{ routerLabel }}</b
        >{{ scopeNarrowing }}
      </p>

      <p v-if="error" class="error" role="alert" data-error>{{ error.message }}</p>
      <!-- Not gated on isLoading. A scope is set and nothing failed, so the
           only honest thing to say about "no answer yet" is that one is
           coming; gating this on the flag would leave a fifth state --
           silence -- for the combination where the composable is not yet
           calling itself loading. -->
      <p v-else-if="!drawn" class="quiet" data-loading>loading…</p>

      <template v-else>
        <!-- An empty view is this router's own answer, and a different fact
             from "no router chosen" above: `/v1/ls/*` returns `data: []`,
             which this API defines as a positive "we looked and there is
             nothing". A router that speaks no BGP-LS, and one whose LSDB
             this collector has not been sent, both read this way. -->
        <p v-if="drawn.nodes.length === 0" class="empty" data-empty-view>
          {{ routerLabel }} reports no link-state node in this scope. The answer carried an empty
          node set, which this API defines as “nothing there” rather than “we did not look” — a
          router that does not advertise BGP-LS to this collector looks exactly like this.
        </p>

        <template v-else>
          <p class="summary" data-summary>
            <strong>{{ summaryCounts }}</strong> in the link-state view of {{ routerLabel }}
          </p>
          <p class="says" data-counts-say>
            Counts of distinct objects, never of rows: a node re-observed on every refresh is one
            node here, and so is a node two of this router's BMP peers both report. An adjacency
            here is a DIRECTION — one node's advertisement of a link toward another — and the
            canvas draws one curve for each, so a link both ends report is two of them: two curves,
            bowed apart, carrying each end's own IGP metric. They are counted separately because
            the metric is a per-direction cost and the two ends can disagree. Where one direction
            advertises several parallel links its curve is marked “×N” and that direction is still
            counted once; the link total is the sum of those markers, counted as the database
            identifies links — two that advertise neither interface addresses nor link IDs are
            indistinguishable here, as they are in the query. A prefix is counted once for each
            node that originates it, so a transit subnet the routers at both its ends advertise is
            two of them; where that makes the two numbers differ, “over N networks” is how many
            distinct networks those originations name.
            <!-- Said only when it is true of this answer, and with the
                 number, because it is a statement about these rows rather
                 than a caveat about the screen. -->
            <span v-if="unreachablePrefixes > 0" data-unreachable-prefixes>
              {{ unreachablePrefixes }} of them
              {{ unreachablePrefixes === 1 ? 'is' : 'are' }} originated by a node this router did
              not report, so no box on the canvas can be selected to show
              {{ unreachablePrefixes === 1 ? 'it' : 'them' }}.
            </span>
          </p>

          <div class="pane">
            <div class="canvas">
              <LinkStateGraph
                :nodes="drawn.nodes"
                :links="drawn.links"
                :seed="seed"
                @select="select"
              />
              <p class="controls">
                <!-- The layout is deterministic, which is what lets an
                     operator compare two loads of one graph -- and which
                     also means a graph that settles into an unreadable
                     arrangement settles into the same one every time. This
                     re-seeds the starting positions, the only
                     nondeterminism the simulation has. -->
                <button type="button" data-relayout @click="seed++">Re-layout</button>
              </p>
            </div>

            <aside class="rail" data-rail>
              <template v-if="selected">
                <h2 class="mono">{{ selectedName?.text }}</h2>
                <!-- Which name this is. The canvas labels a node with the
                     API's resolved label, which can be a name another
                     router supplied for it (`label_source: fleet`), so a
                     node can draw as xr-p1 and open a rail headed by a
                     router-id. Saying which claim is on screen is what
                     keeps that from reading as a disagreement about which
                     node was clicked. -->
                <p class="says" data-rail-name-source>
                  {{ selectedName?.from }} — the canvas labels it with the name the API resolved
                  across the fleet, which may have come from another router's database.
                </p>
                <dl class="fields">
                  <!-- The dotted form where the API supplies one, the raw
                       identifier where it does not. `router_id` is the RAW
                       field -- hex for an IS-IS system ID, and hex for an
                       OSPF router-id too (0aff0001 for 10.255.0.1) -- while
                       `router_id_v4` is the dotted quad or "". The heading
                       above already prefers the dotted form; showing the hex
                       here made one rail state the same identifier two ways.
                       The attribute names the field actually rendered, so a
                       column guard reading it is not told the wrong one. -->
                  <div class="field">
                    <dt :data-node-field="selected.router_id_v4 ? 'router_id_v4' : 'router_id'">
                      Router-id
                    </dt>
                    <dd class="mono">{{ selected.router_id_v4 || selected.router_id }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="protocol_name">Protocol</dt>
                    <dd>{{ protocolOf(selected) }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="area">Area</dt>
                    <dd>{{ selected.area }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="srgb_base">SRGB base</dt>
                    <dd class="mono">{{ selected.srgb_base }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="srgb_size">SRGB size</dt>
                    <dd class="mono">{{ selected.srgb_size }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="srlb_base">SRLB base</dt>
                    <dd class="mono">{{ selected.srlb_base }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="srlb_size">SRLB size</dt>
                    <dd class="mono">{{ selected.srlb_size }}</dd>
                  </div>
                  <div class="field">
                    <dt data-node-field="sr_algorithms">SR algorithms</dt>
                    <dd class="mono">{{ selected.sr_algorithms.join(' · ') || '—' }}</dd>
                  </div>
                </dl>

                <h3>Prefixes originated</h3>
                <!-- The prefixes query answers on its own clock, and an
                     empty table over an unanswered one would say this node
                     originates nothing. -->
                <p v-if="!distinctPrefixes" class="quiet" data-prefixes-pending>
                  prefixes loading…
                </p>
                <p v-else-if="selectedPrefixes.length === 0" class="quiet">
                  This node originates no prefix in this answer.
                </p>
                <table v-else class="prefixes">
                  <thead>
                    <tr>
                      <th
                        v-for="c in RAIL_PREFIX_COLUMNS"
                        :key="c.id"
                        :data-prefix-col="c.id"
                        :class="{ num: c.numeric }"
                      >
                        {{ c.header }}
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="p in selectedPrefixes" :key="p.prefix" data-prefix-row>
                      <td
                        v-for="c in RAIL_PREFIX_COLUMNS"
                        :key="c.id"
                        :class="{ num: c.numeric, mono: !c.numeric }"
                      >
                        {{ cellOf(p, c.id) }}
                      </td>
                    </tr>
                  </tbody>
                </table>
                <p class="says">
                  Metric is the IGP cost to the prefix as this node advertises it. A prefix-SID is
                  an index into the SRGB above while its flags are 0; one shown with flags is a SID
                  whose meaning depends on them — RFC 9085's V and L make it an absolute label
                  instead — and this screen does not decode them. “—” is an absent Prefix-SID,
                  which is a different thing from index 0.
                </p>
              </template>
              <p v-else class="quiet">
                Select a node in the graph to see its segment-routing ranges and the prefixes it
                originates.
              </p>
            </aside>
          </div>
        </template>

        <!-- Said for the empty view as well as the drawn one, which is why
             it sits outside the branch above rather than beside the
             unreachable-prefix clause it mirrors: the only real capture that
             discards an adjacency is xr-p2, whose answer has no nodes at
             all, so a sentence living inside the non-empty branch would be
             silent on the one view that needs it. -->
        <p v-if="undrawnAdjacencies > 0" class="says" data-undrawn-adjacencies>
          {{ plural(undrawnAdjacencies, 'adjacency direction', 'adjacency directions') }} in this
          answer {{ undrawnAdjacencies === 1 ? 'is' : 'are' }} neither drawn nor counted:
          {{ undrawnAdjacencies === 1 ? 'it names' : 'each names' }} a node this router did not
          report, so the canvas has no box at one of its ends.
        </p>

        <!-- Each answer's own completeness and warning fields are
             rendered per leg, not once per screen. `truncated` and
             `session_dumping` are appended by each of the three handlers
             on exactly the branch this screen's requests take, and
             ResultMeta renders them verbatim. Nothing here recomputes
             truncation.

             All three clean, and all three in: one line, and its "complete
             as of" is true of every one of them. The moment any answer
             carries a caveat, that sentence leaves the screen entirely and
             each warned answer gets a line naming itself -- a links answer
             that truncated leaves the canvas short of adjacencies and the
             count beside it understated, and "complete as of" beside that
             would be false whether or not something else on the page was
             complete. Merging the three into one Meta would
             be the other wrong answer: ResultMeta pairs `truncated` with
             `meta.total_matched`, so the links warning would print the
             nodes answer's total.

             Mounted for the empty view too -- it is nested inside the
             answered branch, not inside the non-empty one -- because an
             empty answer during a dump is the case where the warning
             matters most. -->
        <ResultMeta v-if="allLegsClean" :meta="legs[0].meta" :shown="legs[0].shown" />
        <div v-else-if="warnedLegs.length > 0" class="legs">
          <div v-for="l in warnedLegs" :key="l.leg" class="leg" :data-meta-leg="l.leg">
            <span class="leg-name">{{ l.leg }}</span>
            <ResultMeta :meta="l.meta" :shown="l.shown" />
          </div>
        </div>
      </template>
    </template>
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 12px; }
h1 { margin: 0; font: 600 17px var(--font-ui); color: var(--ink); }
.lede { margin: 0; max-width: 62em; color: var(--muted); font: 400 11.5px var(--font-ui); }

.scope { display: flex; gap: 14px; align-items: flex-end; flex-wrap: wrap; }
.scope label { display: flex; flex-direction: column; gap: 4px; font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
.scope select { font: 400 12px var(--font-ui); padding: 6px 9px; border: 1px solid var(--line-2); border-radius: 6px; background: var(--surface); color: var(--ink); text-transform: none; letter-spacing: normal; }

.whose { margin: 0; font: 400 12px var(--font-ui); color: var(--muted); }
.whose b { font-weight: 600; color: var(--ink-2); }
.summary { margin: 0; font: 400 12px var(--font-ui); color: var(--muted); }
.summary strong { color: var(--ink); font-weight: 600; }
.says { margin: 8px 0 0; color: var(--muted); font: 400 10.5px var(--font-ui); max-width: 62em; }
.quiet { margin: 0; color: var(--muted); font: 400 11.5px var(--font-ui); }
.error { margin: 0; color: var(--bad-2); font: 400 11.5px var(--font-ui); }
.empty { margin: 0; padding: 16px 0; color: var(--muted); font: 400 11.5px var(--font-ui); max-width: 46em; }

/* The rail is a fixed 300px and the canvas takes what is left, with a
   measured floor rather than `min-width: 0` -- TopologyView.vue carries the
   browser measurements behind these exact numbers: below about a 430px
   viewport the graph collapsed to 0 wide and the legend painted over the
   rail's text. The same 500px floor applies here because the canvas is the
   same 580-unit viewBox scaled into the pane. */
/* overflow-x: auto so that on a phone, where the pane is narrower than
   the canvas's 500px floor, the graph scrolls inside the pane -- the same
   thing a wide table does inside its card -- instead of pushing the whole
   page 174px sideways at a 390px viewport. It never scrolls on a desktop,
   where the pane is wider than its content. */
.pane { display: flex; flex-wrap: wrap; gap: 16px; align-items: flex-start; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); padding: 14px 16px; overflow-x: auto; }
.canvas { flex: 1; min-width: 500px; }
.controls { margin: 8px 0 0; }
.controls button { font: 500 11.5px var(--font-ui); padding: 5px 11px; border: 1px solid var(--line-2); background: var(--surface); color: var(--muted); border-radius: 6px; cursor: pointer; }

.rail { width: 300px; flex: none; border-left: 1px solid var(--divider); padding-left: 16px; }
.rail h2 { margin: 0 0 8px; font: 600 13px var(--font-data); color: var(--ink); }
.rail h3 { margin: 14px 0 6px; font: 600 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
.fields { margin: 0; display: flex; flex-direction: column; gap: 5px; }
.field { display: flex; gap: 8px; align-items: baseline; }
.field dt { font: 500 10.5px var(--font-ui); color: var(--muted); min-width: 96px; }
.field dd { margin: 0; font: 400 11.5px var(--font-ui); color: var(--ink-2); }
.field dd.mono { font-family: var(--font-data); font-size: 10.5px; }

.prefixes { width: 100%; border-collapse: collapse; font-size: 11px; }
.prefixes th { text-align: left; font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; padding-bottom: 4px; border-bottom: 1px solid var(--divider); }
.prefixes th.num, .prefixes td.num { text-align: right; }
.prefixes td { padding: 3px 0; color: var(--ink-2); border-bottom: 1px solid var(--line-faint); }
.prefixes td.mono, .prefixes td.num { font: 400 10.5px var(--font-data); }

/* One row per answer, and the label is what makes each row a claim about
   one query rather than about the screen. */
.legs { display: flex; flex-direction: column; }
.leg { display: flex; align-items: baseline; gap: 8px; }
.leg-name { font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; min-width: 62px; padding-left: 18px; }
.leg :deep(.meta) { flex: 1; border-top: 1px solid var(--line-faint); padding-left: 0; }
</style>
