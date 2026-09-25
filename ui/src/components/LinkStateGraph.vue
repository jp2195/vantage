<script setup lang="ts">
import { computed } from 'vue'
import type { LsEndpoint, LsLink, LsNode } from '@/api/generated'
import { HANDOFF_MAX_PX, NODE, round, settleGraph, type Positioned } from '@/lib/graphLayout'
import {
  distinctNodes,
  drawnAdjacencies,
  drawnLinkRows,
  type Adjacency,
} from '@/lib/lsAdjacencies'

/**
 * One router's view of one IGP topology, laid out before first paint.
 *
 * Every object drawn here came out of ONE link-state database, mirrored to
 * the collector by BMP, and the picture says so (rule 1). The routers in
 * this archive disagree -- 7, 6, 5 and 1 nodes for the same router set --
 * because no router holds "the" topology, it holds its own LSDB. A
 * drawing that did not name its observer would be claiming to describe
 * the network, which no single LSDB can do.
 *
 * THE LINK-STATE RULES. Files across the link-state screen cite these by
 * number ("rule 4"); this is the one place they are written out.
 *
 *   1. The picture names whose view it is. Every graph, count and
 *      shareable URL identifies the router whose LSDB produced it, because
 *      no single LSDB describes the network.
 *   2. Rows are never reported as topology. Node, adjacency and prefix
 *      counts are counts of DISTINCT objects (@/lib/lsAdjacencies), and a
 *      summary line states what it counted.
 *   3. Edge width encodes nothing. Every adjacency draws at EDGE_WIDTH; the
 *      IGP metric and the bandwidth are text, never geometry.
 *   4. One graph is one topology, scoped (router, protocol, area). Rows
 *      from more than one scope are said out loud (`mixed`), never merged
 *      and never resolved by picking one.
 *   5. A one-sided adjacency is drawn distinctly and labeled "seen from one
 *      side in this view", with no verdict on why.
 *   6. A truncated answer says so, in the server's words: the screen mounts
 *      ResultMeta for the warnings each /v1/ls answer carries, and never
 *      recomputes truncation or writes "complete as of" over a warned one.
 *
 * It shares @/lib/graphLayout with the AS-path graph and nothing else. The
 * meanings do not transfer: a node there is an ASN and its roles, here a
 * router-id and its area; an edge weight there is routes carried (a count),
 * here an IGP metric (a cost); direction is meaningless there and meaningful
 * here. One component parameterized across those rows would carry props that
 * mean nothing on one screen and make a real claim on the other.
 */
const props = defineProps<{
  /**
   * The nodes in this router's LSDB, as `/v1/ls/nodes` returned them --
   * ROWS, which is not the same thing as nodes. One router monitored through
   * two BMP peers returns every node it holds twice (`LSNodes` groups by
   * peer_ip and rib as well as by node_key), and the archive's 139 rows are 7
   * distinct nodes in the richest view. They are collapsed on `node_key` here
   * rather than by the caller, through the same shared module the adjacency
   * collapse uses, because `settleGraph` lays out one box per node it is
   * given: a component that drew what it was handed would draw one router
   * twice and say nothing about it.
   *
   * Unlike `links` below, this collapse decides nothing -- `node_key` is a
   * hash of the node's own identity, so two rows sharing one ARE one node.
   */
  nodes: LsNode[]
  /**
   * The adjacency rows, exactly as `/v1/ls/links` returned them. One row is
   * one link as one observer reports it, so several rows can share a node
   * pair -- see @/lib/lsAdjacencies for what that means and what is done
   * about it. Rows are drawn as given: `state=live` is the endpoint's
   * default, and deciding here which rows count as current would be a second
   * place that answers a question the query already answered.
   */
  links: LsLink[]
  /**
   * Which of the layout's equally valid arrangements to draw. The screen's
   * re-layout control is this prop and nothing else; see `settled` below.
   */
  seed?: number
}>()

const emit = defineEmits<{ select: [nodeKey: string] }>()

/**
 * Rule 3, as a constant, which is the whole of it.
 *
 * Every adjacency draws at this width. The IGP metric is a COST, so "thicker
 * is higher metric" reads as better-when-worse and "thicker is lower metric"
 * inverts the convention `PathGraph` established one screen over, where
 * thickness is routes carried. `max_bandwidth` is in the data, is more
 * tempting, and is worse: it is the link's configured capacity and says
 * nothing about what the IGP will do with it. Both are text -- the metric on
 * the line, the bandwidth in the line's tooltip -- and neither is geometry.
 */
const EDGE_WIDTH = 1.6

/** A settled position, which is all the curve helper needs of a node. */
type Point = { x: number; y: number }

/**
 * The three naming tiers, ordered by how strong a claim each one is.
 *
 * `observer` means the router reporting this link named the node itself,
 * `fleet` that another router did and this one did not, `identifier` that
 * nobody has a name and the label is the raw router-id.
 */
const TIER_STRENGTH: Record<LsEndpoint['label_source'], number> = {
  identifier: 0,
  fleet: 1,
  observer: 2,
}

/** The tier, for a reader rather than for a stylesheet. */
const TIER_WORDS: Record<LsEndpoint['label_source'], string> = {
  observer: 'named by this router',
  fleet: 'name from another router',
  identifier: 'no name reported',
}

/**
 * Each node's name and the tier that produced it, read off the endpoints of
 * the adjacencies that name it.
 *
 * Both halves move together, always. `query/linkstate.go`'s own comment on
 * `lsLabelSourceExpr` is why: "a label from one tier reported as coming from
 * another is worse than no report at all", because a caller told a name came
 * from the reporting observer will believe the two ends of a link were named
 * by the router that reported it, and act on a name borrowed from a third
 * router. So this picks an ENDPOINT and reads both fields off it, rather
 * than reducing a label and a tier separately.
 *
 * Within one observer's answer there is nothing to pick between: the API
 * resolves the label per node_key (`ls_node_labels` groups by collector,
 * router, peer, rib, session and node_key), so every endpoint naming a node
 * carries the same pair. The reduction matters only for a caller that mixed
 * two views, and there the WEAKEST claim is the one to report -- reporting
 * `observer` for a name only another router held is exactly the blend the
 * comment above forbids. The two rejected rules both fail that: taking the
 * first endpoint in row order answers by the order rows happened to arrive,
 * and taking the strongest upgrades a borrowed name.
 */
const named = computed(() => {
  const byKey = new Map<string, LsEndpoint>()
  for (const l of props.links) {
    for (const end of [l.local, l.remote]) {
      const held = byKey.get(end.node_key)
      if (!held || TIER_STRENGTH[end.label_source] < TIER_STRENGTH[held.label_source]) {
        byKey.set(end.node_key, end)
      }
    }
  }
  return byKey
})

/**
 * The adjacencies drawn: the rows collapsed into distinct directed
 * adjacencies, then narrowed to those with both ends in the node set.
 *
 * Both steps live in @/lib/lsAdjacencies rather than here, because the screen
 * around this component has to COUNT exactly what this draws. One screen over
 * it did not, and @/lib/trimGraph's doc comment records the result: a summary
 * line and an empty-state gate read the untrimmed graph while the canvas drew
 * the trimmed one. Rule 2 makes the link-state version of that gap wider than
 * anywhere else in this repository -- 229 rows in the archive are 20, 8 and 7
 * distinct adjacencies -- so "what is drawn" is one function both files ask,
 * rather than two implementations that agree until one of them is edited.
 */
/**
 * The nodes this canvas holds: one per `node_key`. See the `nodes` prop.
 *
 * Every read below goes through this rather than through `props.nodes`, so
 * the set laid out, the set drawn, the set the caption is derived from and
 * the set a caller counts are one set.
 */
const held = computed(() => distinctNodes(props.nodes))

const visible = computed<Adjacency[]>(() => drawnAdjacencies(props.links, held.value))

/**
 * Node positions, settled.
 *
 * The layout lives in @/lib/graphLayout and is shared with the AS-path
 * graph, because the guarantee it carries has been broken once already by a
 * step added after the simulation ran, and one implementation with the
 * comments that record why is what keeps a second screen from reintroducing
 * the defect the first one fixed. Nothing about a router crosses that
 * boundary: this hands over the nodes and how to read a key off one.
 *
 * This component's own non-overlap test stays, and its reason has changed.
 * It was here because the module's crowding band was stated in node counts
 * and said nothing about DENSITY -- nx-p3's 20 adjacencies over 7 nodes is
 * denser than anything the module was measured against. The module now
 * separates to a fixpoint and is measured on dense graphs too, so this test
 * is no longer covering a gap; it is the screen-level check that the
 * component still hands the layout every node it draws. The band itself is
 * gone: the canvas grows, and nothing overlaps at any size.
 *
 * Which matters more here than anywhere else. Measured in a browser on
 * 2026-09-21, this screen drew THIRTEEN overlapping pairs under a fixed
 * band -- replay-nx-p3's view at "every protocol" is 19 nodes, far past
 * anything a band of 7 or 12 covers.
 */
const settled = computed(() =>
  settleGraph({
    nodes: held.value,
    id: (n) => n.node_key,
    // Mapped down to the two keys rather than handed the adjacency itself.
    // The module is domain-free on purpose -- "no node attribute reaches it"
    // -- and an object carrying metrics and one-sidedness into it would be a
    // second place those could be read from, however carefully it ignores
    // them today.
    edges: visible.value.map((a) => ({ src: a.src, dst: a.dst })),
    seed: props.seed,
  }),
)

/**
 * The settled positions, and the canvas they need.
 *
 * `canvas` is taken from the layout's own answer rather than from a module
 * constant, and that is the whole guarantee: the frame this component draws
 * cannot disagree with the space the boxes were separated into. A viewBox
 * sized independently is how a drawing ends up with boxes outside it.
 */
const placed = computed(() => settled.value.nodes)
const canvas = computed(() => settled.value.canvas)

/**
 * What the picture is OF: whose LSDB, which protocol, which area. Rules 1 and
 * 4, on the canvas itself rather than on the screen around it.
 *
 * Rule 1 binds every graph -- "a picture that omits it is claiming to
 * describe the network, which no single LSDB can do" -- and rule 4 says one
 * graph is one topology, scoped (router, protocol, area), because OSPF area 0
 * and area 1 share no edges and drawing them together invents adjacency.
 * Neither is a caption over the picture; both are what the picture is.
 *
 * Derived from what is DRAWN, not from the props. That distinction is the
 * whole of this function's correctness: with `nodes: []` and a non-empty
 * `links`, every adjacency is dangling, the canvas is blank -- and a version
 * of this that read the raw props still wrote "nx-p3 (10.0.0.11)" over it,
 * vouching for a view that nothing drew. This screen's composable can
 * produce that exact pair by resolving the links query while the nodes query returns
 * empty. So the drawn nodes gate it, and the rows consulted are those nodes
 * plus the link rows behind the lines actually kept.
 *
 * Every value is distinct-and-joined rather than "the first one". A second
 * observer, protocol or area here is a defect above this component, and
 * captioning a merged picture with one of them would hide that behind
 * precisely the claim rule 1 forbids; `mixed` says it out loud instead.
 *
 * Areas come from the NODE rows alone, deliberately. `/v1/ls/links` matches
 * area at EITHER end -- the contract's own words: "an area-crossing link is
 * returned by a query for either of its two areas" -- so reading endpoint
 * areas would report a legitimate ABR link as two topologies on one canvas.
 * A node in the other area is not in a scoped node set anyway, so its
 * adjacency is dangling and never drawn.
 */
const scope = computed(() => {
  const drawnNodes = placed.value.map((p) => p.node)
  if (drawnNodes.length === 0) return null

  const observers = new Set<string>()
  const protocols = new Set<string>()
  const areas = new Set<number>()
  for (const row of [...drawnNodes, ...drawnLinkRows(props.links, drawnNodes)]) {
    // An empty sysname is a real answer here -- a BMP session whose
    // initiation carried no name -- and the IP is what identifies the router
    // in that case, as it does everywhere else in this UI.
    observers.add(row.router_sysname ? `${row.router_sysname} (${row.router_ip})` : row.router_ip)
    // Same contract for an unnamed protocol ID: "" is a positive answer and
    // "the number beside it is the whole truth", so the number is what shows.
    protocols.add(row.protocol_name || `protocol ${row.protocol}`)
  }
  // Area 0 is a REAL value -- the OSPF backbone, and the most common area in
  // this archive -- so it is printed, never suppressed as a falsy default.
  for (const node of drawnNodes) areas.add(node.area)

  return {
    observers: [...observers].join(' + '),
    protocols: [...protocols].join(', '),
    areas: `${areas.size > 1 ? 'areas' : 'area'} ${[...areas].sort((a, b) => a - b).join(', ')}`,
    mixed: observers.size > 1 || protocols.size > 1 || areas.size > 1,
  }
})

const nodes = computed(() =>
  placed.value.map(({ node, x, y }) => {
    const end = named.value.get(node.node_key)
    return {
      key: node.node_key,
      x,
      y,
      // The name, and where it came from. An isolated node -- one no
      // adjacency in this answer names -- has no endpoint and therefore
      // no tier: `label_source` is the API's answer, computed against the
      // fleet's node rows, and this component holds one view's. Leaving it
      // absent is the honest report; deriving one from `name` here would be
      // manufacturing a provenance the API did not give. The name itself
      // still has an answer, down the same chain the API's own label uses.
      name: end?.label ?? (node.name || node.router_id_v4 || node.router_id),
      labelSource: end?.label_source,
      tier: end ? TIER_WORDS[end.label_source] : '',
      // The dotted form where the API supplies one. `router_id` is the RAW
      // identifier -- 0aff0001 where the dotted form is 10.255.0.1, and hex
      // for an IS-IS system ID, where there IS no dotted form and the raw
      // value is the only identity a node has. Showing the hex while the
      // rail beside it showed the quad stated one identifier two ways on one
      // screen; the API's own label resolution prefers the quad for the same
      // reason.
      routerId: node.router_id_v4 || node.router_id,
    }
  }),
)

const edges = computed(() => {
  const at = new Map(placed.value.map((p) => [p.node.node_key, p]))
  return visible.value.flatMap((adjacency) => {
    const from = at.get(adjacency.src)
    const to = at.get(adjacency.dst)
    // `visible` already required both ends, so this is unreachable -- and it
    // is a flatMap rather than a non-null assertion because the alternative
    // to an empty list is a line drawn to NaN, which renders as nothing and
    // reports as an edge.
    if (!from || !to) return []
    const d = curveBetween(from, to)
    const label = labelOf(adjacency)
    return [
      {
        key: adjacency.key,
        oneSided: adjacency.oneSided,
        d,
        at: labelAnchor(from, to, label, placed.value),
        label,
        title: titleOf(adjacency),
      },
    ]
  })
})

/**
 * What one line says out loud: every distinct metric it merged, and a
 * multiplier when it merged more than one link.
 */
function labelOf(adjacency: Adjacency) {
  const metrics = adjacency.metrics.join(' · ')
  return adjacency.members > 1 ? `${metrics} ×${adjacency.members}` : metrics
}

/**
 * The same line, in full, for a pointer and a screen reader.
 *
 * `max_bandwidth` appears here and nowhere else. Rule 3 keeps it out of the
 * geometry, and nothing converts it: BGP-LS carries it in BYTES per second
 * (RFC 5305 sec 3.4), so reading 1000000000 as "1 Gbps" is wrong by a factor
 * of eight, and this screen has not read the write path closely enough to
 * label it any other way.
 *
 * But the unit is PRINTED, because omitting it does not avoid that error --
 * it is the most inviting presentation of it. 1000000000 is exactly the
 * number an operator associates with 1 Gbps, and "B/s" appears nowhere else
 * they can reach: not in api/openapi.yaml, not on query.LSLink.MaxBandwidth,
 * not in this tooltip until now. Four characters convert nothing and make
 * the misreading impossible.
 */
function titleOf(adjacency: Adjacency) {
  const from = named.value.get(adjacency.src)?.label ?? adjacency.src
  const to = named.value.get(adjacency.dst)?.label ?? adjacency.dst
  const parts = [
    `${from} → ${to}`,
    `IGP metric ${adjacency.metrics.join(', ')}`,
    `max_bandwidth ${adjacency.bandwidths.join(', ')} B/s`,
  ]
  if (adjacency.members > 1) parts.push(`${adjacency.members} parallel links`)
  if (adjacency.oneSided) parts.push('seen from one side in this view')
  return parts.join(' · ')
}

/**
 * A gentle curve, bowed to one side of the midpoint.
 *
 * The bow is what keeps the two directions of one adjacency apart: they are
 * two rows, they are drawn as two lines, and the offset flips with the
 * direction, so a reciprocal pair draws as two curves with two metric labels
 * rather than as one line with the other hidden underneath. Direction is
 * meaningful on this screen -- a link is advertised by the node at its local
 * end, and nothing pairs A->B with B->A.
 *
 * Deliberately not shared with `PathGraph`: the layout is shared because its
 * guarantee is expensive and was got wrong once, while this is a drawing
 * convention for a graph whose edges mean something different, and a shared
 * curve would be a third thing the two screens have to agree about.
 */
function curveBetween(from: Point, to: Point) {
  const { x: cx, y: cy } = controlFor(from, to)
  return `M${from.x},${from.y} Q${cx},${cy} ${to.x},${to.y}`
}

function controlFor(from: Point, to: Point) {
  const [dx, dy] = [to.x - from.x, to.y - from.y]
  const length = Math.hypot(dx, dy) || 1
  const bow = Math.min(34, length * 0.16)
  return {
    x: round((from.x + to.x) / 2 - (dy / length) * bow),
    y: round((from.y + to.y) / 2 + (dx / length) * bow),
  }
}

/** A point on the quadratic, at `t`. `t = 0.5` is the curve's midpoint. */
function pointOnCurve(from: Point, control: Point, to: Point, t: number): Point {
  const u = 1 - t
  return {
    x: u * u * from.x + 2 * u * t * control.x + t * t * to.x,
    y: u * u * from.y + 2 * u * t * control.y + t * t * to.y,
  }
}

/**
 * The stops a metric label is offered, in order of preference.
 *
 * The midpoint first, because that is where a label belongs: farthest from
 * both endpoints, and where a reader looks for a line's value. The rest are
 * paired either side of it and move outward, so a label that has to give way
 * moves as little as it can and stays on its own curve.
 *
 * Nothing goes past 0.7: a label near an endpoint reads as belonging to that
 * NODE rather than to the line, which trades one misreading for a worse one.
 */
const LABEL_STOPS = [0.5, 0.42, 0.58, 0.32, 0.68]

/**
 * How wide a label is, estimated from its glyph count.
 *
 * `var(--font-data)` is JetBrains Mono, a monospace face whose advance is
 * 0.6em, so a 9.5px glyph is 5.7px wide. It is an ESTIMATE and is treated as
 * one -- the fallback face if the font has not loaded is not this one, and
 * SVG text metrics do not exist in a test environment at all. The layout has
 * to be deterministic and settled before first paint, so a measured
 * `getComputedTextLength()` was never an option here regardless.
 */
const GLYPH_ADVANCE = 5.7
const LABEL_PAD_Y = 8

/**
 * Where the metric sits: on its curve, and not underneath a node box.
 *
 * Testing in a browser, against a restored archive, proved the problem
 * by pixel diff rather than by eye: 1 to 4 of 20 metric labels were
 * painted over by a node box on 9 of 10 seeds, 3 of them at the default
 * seed -- hiding every label changed nothing inside those boxes. The
 * cause is that a 160x54 box is
 * large against a 580x460 canvas, so a curve between two distant nodes
 * routinely passes its midpoint across a THIRD node's box.
 *
 * Two changes answer it and both are needed. This one moves the label along
 * its own curve to the first stop no box covers, which is what keeps the
 * label off a box in the first place. The other is paint order: the labels
 * are drawn in their own layer AFTER the nodes (see the template), so a
 * label that cannot find a free stop -- a short edge between two adjacent
 * boxes has none -- is still readable over the box, with the halo the
 * stylesheet already gives it, instead of vanishing under it.
 *
 * The rejected third option was drawing the label beside the chord rather
 * than on the curve: it detaches the number from the line it belongs to,
 * which on a canvas that draws both directions of an adjacency as two curves
 * is exactly the ambiguity the bow exists to prevent.
 */
function labelAnchor(from: Point, to: Point, text: string, boxes: Positioned<LsNode>[]): Point {
  const control = controlFor(from, to)
  const halfWidth = (text.length * GLYPH_ADVANCE) / 2
  let best: { at: Point; clearance: number } | undefined
  for (const t of LABEL_STOPS) {
    const at = pointOnCurve(from, control, to, t)
    const clearance = clearanceAt(at, halfWidth, boxes)
    // The first stop that clears every box wins, which keeps a label as
    // close to the midpoint as it can be.
    if (clearance > 0) return { x: round(at.x), y: round(at.y) }
    if (!best || clearance > best.clearance) best = { at, clearance }
  }
  // Every stop is under a box -- a short edge between two adjacent boxes
  // has no free stop at all, since the boxes are 160 wide and the layout
  // holds their centers 176 apart. The least buried one is taken, and the
  // layer above the nodes is what keeps it legible there; moving the label
  // OFF its curve to escape would detach the number from the line it
  // belongs to, on a canvas that draws both directions of an adjacency as
  // two separate curves.
  return { x: round(best!.at.x), y: round(best!.at.y) }
}

/**
 * How far a label anchored at `at` sits outside the nearest node box:
 * positive is clear of every box, negative is buried in the closest one.
 *
 * The label is `text-anchor: middle`, so it spreads `halfWidth` either side
 * of its anchor; the vertical pad is the glyph height plus a little, since
 * the anchor is the text baseline rather than its center.
 */
function clearanceAt(at: Point, halfWidth: number, boxes: Positioned<LsNode>[]) {
  let worst = Infinity
  for (const box of boxes) {
    const x = Math.abs(at.x - box.x) - (NODE.width / 2 + halfWidth)
    const y = Math.abs(at.y - box.y) - (NODE.height / 2 + LABEL_PAD_Y)
    worst = Math.min(worst, Math.max(x, y))
  }
  return worst
}
</script>

<template>
  <figure class="graph">
    <p v-if="scope" class="whose" data-observer>
      One router's link-state view: <b>{{ scope.observers }}</b> · {{ scope.protocols }} ·
      {{ scope.areas }}
    </p>
    <!-- Rule 4, when the rows break it. Said rather than resolved: picking
         one of two areas would draw half a topology under a caption claiming
         it was whole, and merging them is the invented adjacency the rule
         exists to forbid. -->
    <p v-if="scope?.mixed" class="mixed" data-scope-mixed>
      Drawn from more than one (router, protocol, area). One graph is one topology, and this
      canvas holds rows from more than one.
    </p>
    <svg
      :viewBox="`0 0 ${canvas.width} ${canvas.height}`"
      :style="{
        maxWidth: `${Math.max(HANDOFF_MAX_PX, canvas.width)}px`,
        aspectRatio: `${canvas.width} / ${canvas.height}`,
      }"
      role="group"
      :aria-label="
        scope
          ? `IGP topology as seen from ${scope.observers}, ${scope.protocols}, ${scope.areas}`
          : 'IGP topology, nothing to draw'
      "
    >
      <g class="edges">
        <template v-for="edge in edges" :key="edge.key">
          <path
            class="edge"
            :class="{ 'one-sided': edge.oneSided }"
            :data-edge="edge.key"
            :data-one-sided="String(edge.oneSided)"
            :d="edge.d"
            :stroke-width="EDGE_WIDTH"
            :stroke-dasharray="edge.oneSided ? '5 4' : undefined"
          >
            <title>{{ edge.title }}</title>
          </path>
        </template>
      </g>
      <g
        v-for="node in nodes"
        :key="node.key"
        class="node"
        :data-node="node.key"
        :data-label-source="node.labelSource"
        :transform="`translate(${node.x}, ${node.y})`"
        role="button"
        tabindex="0"
        @click="emit('select', node.key)"
        @keydown.enter.space.prevent="emit('select', node.key)"
      >
        <rect
          :x="-NODE.width / 2"
          :y="-NODE.height / 2"
          :width="NODE.width"
          :height="NODE.height"
          rx="6"
        />
        <!-- Three baselines: the name, the router-id it is a name FOR, and
             which tier produced the name. The third is the one that stops
             the first two from reading as one fact -- a borrowed name beside
             a router-id looks exactly like a name this router holds. -->
        <text class="name" data-node-name y="-10">{{ node.name }}</text>
        <text class="rid" data-node-router-id y="5">{{ node.routerId }}</text>
        <text class="tier" y="18">{{ node.tier }}</text>
      </g>
      <!-- The metric labels, in a layer of their own and drawn LAST.
           SVG paints in document order, so while these sat among the edges a
           node box drawn afterwards covered them completely -- measured
           by pixel diff against a restored archive: 1 to 4 of 20 labels
           hidden on 9 of 10 seeds. `labelAnchor` moves a label off a box
           where it can; this is what keeps the remainder readable where
           it cannot. -->
      <g class="labels">
        <text
          v-for="edge in edges"
          :key="edge.key"
          class="metric"
          :data-edge-label="edge.key"
          :x="edge.at.x"
          :y="edge.at.y"
        >
          {{ edge.label }}
        </text>
      </g>
    </svg>

    <figcaption class="legend" data-legend>
      <span class="key"><i class="swatch" />seen from both sides in this view</span>
      <span class="key"><i class="swatch one-sided" />seen from one side in this view</span>
      <span class="says">
        Each curve is one DIRECTION of an adjacency — one node's advertisement of a link toward
        another — so a link both ends report draws as two curves, bowed apart, each carrying that
        end's own IGP metric. They stay apart because the metric is a per-direction cost and the
        two ends can disagree. Every adjacency draws at the same width: line thickness encodes
        nothing here. The number on a line is that metric, which is a cost — a thicker line for a
        higher one would read as better-when-worse, and for a lower one would invert the AS-path
        graph's convention one screen over. Maximum bandwidth is kept off the picture for a
        stronger reason: it is the link's configured capacity and says nothing about what the IGP
        will do with it. Both numbers are in each line's tooltip: the metric as the unitless cost
        it is, and the bandwidth in bytes per second, the unit BGP-LS advertises it in. “×2” marks
        one direction the database carries as two parallel links, drawn
        as one line because they run between the same two boxes; a line without one is one link as
        the database identifies links — two that advertise neither interface addresses nor link IDs
        are indistinguishable here, as they are in the query. “Seen from one side” says that this
        router's database holds one direction of an adjacency and not the other. Why that is, this
        data does not decide, and neither does this screen.
      </span>
    </figcaption>
  </figure>
</template>

<style scoped>
.graph { margin: 0; display: flex; flex-direction: column; gap: 8px; }
/* The ratio and the cap are BOUND, not fixed here: the canvas grows with
   the node count, so a hard-coded 580/460 would letterbox every graph that
   grew and a hard-coded 640px cap would scale a grown one down -- shrinking
   the labels, which is the trade this drawing refuses.

   The 640 stays as a FLOOR rather than being replaced by the canvas width.
   Capping at the canvas alone looked like "never magnify past 1 unit = 1
   px", and testing caught what it actually did: every graph of nine
   nodes or fewer still lays out on the project's 580-unit canvas, so its
   cap fell 640 -> 580 and every small graph -- which is every graph in
   this archive -- rendered 9.4% smaller than before, silently. */
svg { width: 100%; height: auto; display: block; }

.whose { margin: 0; font: 400 11.5px var(--font-ui); color: var(--muted); }
.whose b { font-weight: 600; color: var(--ink-2); }
/* --bad-2 rather than --bad: this is TEXT, and the design system's two reds
   are split by role across the UI -- --bad-2 for words an operator reads
   (every .error paragraph, the withdrawn-route cells, the view_lost pills),
   --bad for strokes, borders and swatches. Both clear AA on this ground, so
   the fix is consistency rather than contrast. */
.mixed { margin: 0; font: 500 11.5px var(--font-ui); color: var(--bad-2); }

.edge { fill: none; stroke: var(--faint); }
/* Distinct, and deliberately not alarming: a dash and the design system's
   accent ink, not --bad. Red would be a verdict, and rule 5 says the screen
   offers none -- in this archive one-sidedness tracks view completeness. */
.edge.one-sided { stroke: var(--accent-ink); }
/* The metric sits ON its own line, so it is painted with a halo of the pane
   color first and the glyphs over it -- `paint-order: stroke` is what makes
   the stroke go under rather than over the fill. Testing in a browser is
   what found this: without the halo the curve runs straight through the
   digits and a 9000 reads as a struck-through number, which no jsdom
   test can see
   because every one of them reads text content. The halo matches the node
   boxes' own fill, which is the background this drawing already assumes. */
.metric {
  font: 400 9.5px var(--font-data);
  fill: var(--muted);
  text-anchor: middle;
  paint-order: stroke;
  stroke: var(--surface);
  stroke-width: 3px;
  stroke-linejoin: round;
}

.node { cursor: pointer; }
.node rect { fill: var(--surface); stroke: var(--line-2); }
.node:focus-visible rect { stroke: var(--ink); stroke-width: 1.5; }
.node .name { font: 500 12.5px var(--font-data); fill: var(--ink); text-anchor: middle; }
.node .rid { font: 400 10.5px var(--font-data); fill: var(--muted); text-anchor: middle; }
.node .tier { font: 400 10px var(--font-ui); fill: var(--muted); text-anchor: middle; }

.legend { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; font: 400 10.5px var(--font-ui); color: var(--muted); }
.key { display: flex; align-items: center; gap: 6px; }
.swatch { width: 14px; height: 3px; display: block; background: var(--faint); }
.swatch.one-sided { background: repeating-linear-gradient(to right, var(--accent-ink) 0 5px, transparent 5px 9px); }
.says { flex-basis: 100%; color: var(--muted); }
</style>
