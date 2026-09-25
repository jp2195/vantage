<script setup lang="ts">
import { computed } from 'vue'
import type { AsEdge, Graph } from '@/api/generated'
import { HANDOFF_MAX_PX, NODE, round, settleGraph } from '@/lib/graphLayout'
import { trimGraph } from '@/lib/trimGraph'

/**
 * One family's AS-path graph, laid out before first paint.
 *
 * The graph arrives complete for its scope -- `/v1/topology` returns every
 * node and edge the scope matched -- so `depth` and `minWeight` are trims
 * over what is already in the browser, never a refetch. Everything drawn
 * here is derived from the four fields the server sends per edge (`src`,
 * `dst`, `routes`, `live_routes`, `first_seen`); the server sends no state
 * string on purpose, because the window a state is measured against is a
 * control the operator moves.
 */
const props = defineProps<{
  /** The family's graph, exactly as `/v1/topology` returned it. */
  graph: Graph
  /**
   * How far back "appeared" reaches. Defaults to a day, which is the
   * window an operator looking at a change is usually asking about.
   */
  windowHours?: number
  /**
   * Maximum hops from an origin AS. Undefined draws the whole graph.
   */
  depth?: number
  /**
   * Minimum `routes` an edge must carry to be drawn. Undefined draws every
   * edge, however thin.
   */
  minWeight?: number
  /**
   * Which of the layout's equally valid arrangements to draw.
   *
   * The simulation is deterministic (see `settled` below), which is the
   * property that lets an operator compare two loads of one graph -- and
   * which also means a graph that settles into an unreadable arrangement
   * settles into the SAME unreadable arrangement every time. The seed is
   * the way out: it moves the starting positions, so a different value is a
   * different converged picture of the same graph, and the screen's
   * "Re-layout" control is this prop and nothing else. Defaults to 0, so a
   * caller that never offers the control still gets one fixed layout.
   */
  seed?: number
  /**
   * The instant the window is measured back from, in epoch milliseconds.
   * Injected so a caller -- a test above all -- can ask what this graph
   * looked like at a fixed moment: `first_seen` is absolute and a window is
   * relative, so a comparison against the wall clock has a different answer
   * every hour. Defaults to now, which is what the screen wants.
   */
  now?: number
}>()

const emit = defineEmits<{ select: [asn: number] }>()

const MS_PER_HOUR = 60 * 60 * 1000
const DEFAULT_WINDOW_HOURS = 24

type EdgeState = 'withdrawn' | 'appeared' | 'stable'

/** A settled position. All `curveBetween` needs of a node is where it is. */
type Point = { x: number; y: number }

/**
 * The graph actually drawn: the server's, minus what the two controls trim.
 *
 * The trim itself lives in @/lib/trimGraph, not here, because the screen
 * around this component has to count exactly what this component draws. It
 * did not: TopologyView's summary line and its empty-state gate read the
 * UNTRIMMED graph while this drew the trimmed one, so a Min paths threshold
 * above the heaviest edge left a blank canvas under a line reading "5 ASNs ·
 * 4 edges". One function, asked the same question by both files, is what
 * stops those two numbers from disagreeing again.
 */
const trimmed = computed<Graph>(() =>
  trimGraph(props.graph, { depth: props.depth, minWeight: props.minWeight }),
)

/**
 * Node positions, settled.
 *
 * The layout itself lives in @/lib/graphLayout, not here: the IGP topology
 * screen draws a second graph with the same canvas, the same boxes and the
 * same settle-inside-the-bounds guarantee, and that guarantee has been
 * broken once already by a step added after the simulation ran. One
 * implementation with the comments that record why is what keeps a second
 * screen from reintroducing the defect the first one fixed.
 *
 * Nothing about an AS crosses that boundary. This hands the layout the
 * trimmed nodes and how to read an ASN off one, and gets the same objects
 * back with coordinates, and the canvas they needed.
 *
 * There is no crowding band to work around: the layout separates to a
 * fixpoint and grows its canvas to fit, so nothing overlaps at any size, and
 * `seed` is a preference rather than a remedy.
 */
const settled = computed(() =>
  settleGraph({
    nodes: trimmed.value.nodes,
    id: (n) => n.asn,
    edges: trimmed.value.edges,
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
 * A withdrawn edge is data, not a filter -- and the window -- as one answer
 * per edge.
 *
 * `live_routes === 0` means every route carrying this adjacency has been
 * withdrawn -- the collector knows the edge and the network no longer uses
 * it -- and that outranks the window, which cannot move a withdrawal.
 *
 * The other arm is narrower than its name. `AsEdge.first_seen` is the
 * earliest collector timestamp among the routes that CURRENTLY traverse the
 * edge, and the contract is explicit that a client deriving "newly appeared"
 * from it "is making a claim about the routes in this answer rather than
 * about the adjacency's age, and it can be wrong in both directions" -- a
 * route re-advertised with a new path brings its original timestamp to
 * whatever edge it carries now, and a route that has stopped crossing the
 * edge contributes nothing at all. So `appeared` here names a fact about
 * first_seen and the window, and the legend says first SEEN rather than
 * appeared for that reason.
 */
function edgeState(edge: AsEdge, since: number): EdgeState {
  if (edge.live_routes === 0) return 'withdrawn'
  return Date.parse(edge.first_seen) >= since ? 'appeared' : 'stable'
}

const edges = computed(() => {
  const at = props.now ?? Date.now()
  const since = at - (props.windowHours ?? DEFAULT_WINDOW_HOURS) * MS_PER_HOUR
  const positions = new Map(placed.value.map((p) => [p.node.asn, p]))
  // The whole answer, not the trimmed view. Normalizing against the edges
  // that survive a trim would redraw the SAME edge thicker when the operator
  // raises the Min paths control, with nothing changed in the data underneath;
  // thickness is paths carried in this answer, and an encoding that moves
  // with a control is not that.
  const heaviest = Math.max(1, ...props.graph.edges.map((e) => e.routes))

  return trimmed.value.edges.flatMap((edge) => {
    // An edge whose endpoints are not both drawn has nowhere to land. The
    // trims above never produce one; a response that did would be drawn as
    // the nodes it names rather than as a line into empty space.
    const from = positions.get(edge.src)
    const to = positions.get(edge.dst)
    if (!from || !to) return []
    return [
      {
        key: `${edge.src}-${edge.dst}`,
        state: edgeState(edge, since),
        // Paths carried, on a square-root scale so a ten-times heavier edge
        // does not draw ten times thicker than the rest of the picture.
        width: round(1.4 + 2.6 * Math.sqrt(edge.routes / heaviest)),
        d: curveBetween(from, to),
      },
    ]
  })
})

/**
 * A gentle curve rather than a straight line, offset to one side of the
 * midpoint. The offset is what keeps a reciprocal pair -- routes observed
 * crossing one adjacency in both directions, which happens -- from drawing
 * as a single line with one of the two invisible underneath.
 */
function curveBetween(from: Point, to: Point) {
  const [ax, ay, bx, by] = [from.x, from.y, to.x, to.y]
  const [dx, dy] = [bx - ax, by - ay]
  const length = Math.hypot(dx, dy) || 1
  const bow = Math.min(24, length * 0.12)
  const cx = round((ax + bx) / 2 - (dy / length) * bow)
  const cy = round((ay + by) / 2 + (dx / length) * bow)
  return `M${ax},${ay} Q${cx},${cy} ${bx},${by}`
}

const nodes = computed(() =>
  placed.value.map(({ node, x, y }) => ({
    asn: node.asn,
    x,
    y,
    roles: node.roles.join(' · '),
    // Route identities traversing this AS, which is what the endpoint
    // counts: a path that prepends the same ASN three times traverses it
    // once.
    paths: `${node.routes} ${node.routes === 1 ? 'path' : 'paths'}`,
  })),
)
</script>

<template>
  <figure class="graph">
    <svg
      :viewBox="`0 0 ${canvas.width} ${canvas.height}`"
      :style="{
        maxWidth: `${Math.max(HANDOFF_MAX_PX, canvas.width)}px`,
        aspectRatio: `${canvas.width} / ${canvas.height}`,
      }"
      role="group"
      aria-label="AS path graph"
    >
      <g fill="none">
        <path
          v-for="edge in edges"
          :key="edge.key"
          class="edge"
          :class="edge.state"
          :data-edge="edge.key"
          :data-state="edge.state"
          :d="edge.d"
          :stroke-width="edge.width"
          :stroke-dasharray="edge.state === 'withdrawn' ? '4 3' : undefined"
        />
      </g>
      <g
        v-for="node in nodes"
        :key="node.asn"
        class="node"
        :data-node="node.asn"
        :transform="`translate(${node.x}, ${node.y})`"
        role="button"
        tabindex="0"
        @click="emit('select', node.asn)"
        @keydown.enter.space.prevent="emit('select', node.asn)"
      >
        <rect
          :x="-NODE.width / 2"
          :y="-NODE.height / 2"
          :width="NODE.width"
          :height="NODE.height"
          rx="6"
        />
        <!-- Three baselines, optically centered in the box: the ASN, the
             roles the server sent, and the paths through it. No holder name
             on any of them, by design rather than by absence of the data:
             the full registered-holder line does not fit a node box, and
             the screen around this component puts it in the rail instead
             (TopologyView.vue's own rail heading). -->
        <text class="asn" y="-10">AS{{ node.asn }}</text>
        <text class="role" y="5">{{ node.roles }}</text>
        <text class="role" y="18">{{ node.paths }}</text>
      </g>
    </svg>

    <figcaption class="legend" data-legend>
      <span class="key"><i class="swatch appeared" />first seen in window</span>
      <span class="key"><i class="swatch withdrawn" />withdrawn</span>
      <span class="key"><i class="swatch stable" />stable</span>
      <span class="says">
        Line thickness is paths carried in this answer — how many of the matched routes cross
        that adjacency. It is not how much traffic the link can carry, nor its preference,
        stability or health. First seen dates the routes crossing an adjacency now, not the
        adjacency: a route re-advertised with a new path brings its original timestamp to
        whatever edge it crosses today, so a colored edge is not a claim that the adjacency
        itself is new.
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

.edge { stroke: var(--faint); }
.edge.appeared { stroke: var(--good); }
.edge.withdrawn { stroke: var(--bad); }

.node { cursor: pointer; }
.node rect { fill: var(--surface); stroke: var(--line-2); }
.node:focus-visible rect { stroke: var(--ink); stroke-width: 1.5; }
.node .asn { font: 500 12.5px var(--font-data); fill: var(--ink); text-anchor: middle; }
.node .role { font: 400 10.5px var(--font-ui); fill: var(--muted); text-anchor: middle; }

.legend { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; font: 400 10.5px var(--font-ui); color: var(--muted); }
.key { display: flex; align-items: center; gap: 6px; }
.swatch { width: 14px; height: 3px; display: block; background: var(--faint); }
.swatch.appeared { background: var(--good); }
.swatch.withdrawn { background: var(--bad); }
.says { flex-basis: 100%; color: var(--muted); }
</style>
