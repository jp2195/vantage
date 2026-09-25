import { forceCenter, forceCollide, forceLink, forceManyBody, forceSimulation } from 'd3-force'
import type { SimulationLinkDatum, SimulationNodeDatum } from 'd3-force'

/**
 * One settled force layout, shared by every graph screen and owned by none.
 *
 * Two screens draw a graph -- the AS-path graph and one router's IGP
 * topology -- and their nodes have nothing in common but a position. So this
 * module knows about keys and coordinates and nothing else: no node
 * attribute reaches it, and a caller hands in its own objects and gets them
 * back with an x and a y. That is not tidiness. The layout is the expensive
 * part and it has been wrong once (see BOUNDS below), and the comments that
 * record why it is shaped this way are worth more traveling with the code
 * than they are re-derived by the second screen to reimplement it.
 */

/**
 * This project's 580x460 canvas, and the size every graph of nine nodes
 * or fewer is still drawn at. Larger graphs GROW -- see `canvasFor`.
 */
export const CANVAS = { width: 580, height: 460 }

/**
 * The node count this canvas holds without crowding, measured rather
 * than chosen: at 20 seeds per size, nine nodes overlapped at no
 * seed and thirteen overlapped at every one.
 */
const FITS = 9

/**
 * The widest a graph was ever drawn before the canvas could grow: the
 * project's 580-unit canvas rendered into 640 CSS px.
 *
 * It survives as a FLOOR on the rendered size, not as a cap. Small graphs
 * still lay out on 580 units, so capping at the canvas width would render
 * them at 580px -- 9.4% smaller than they have always been, for a change
 * that was supposed to be about LARGE graphs only.
 */
export const HANDOFF_MAX_PX = 640
export const NODE = { width: 160, height: 54 }
/** Kept clear of the canvas edge so a node's box never touches the frame. */
export const MARGIN = 8

/**
 * Ticks run before first paint. d3's own default alpha schedule reaches
 * alphaMin at 300 ticks, so this is "to convergence" rather than a guess,
 * and it is run synchronously here rather than animated: an operator
 * comparing two loads of the same graph must see the same picture.
 */
export const TICKS = 300
/**
 * Force constants. `COLLIDE_RADIUS` is the one that is not a taste: it holds
 * node CENTERS 176 apart, and two 160x54 boxes overlap only when their
 * centers are within 160 horizontally AND 54 vertically, which is at most
 * sqrt(160^2 + 54^2) = 169 apart. So 176 of circular separation is a
 * guarantee of non-overlap rather than a hope, for as long as the canvas has
 * room to hold every node that far from its neighbors.
 *
 * THERE USED TO BE A CROWDING BAND HERE, and it was wrong to accept it.
 * This comment recorded, measured at 20 seeds per size, that 9 nodes fit,
 * 10-12 was shape-dependent and 13-and-up overlapped at every seed -- then
 * called that "an accepted and stated limit rather than one to chase",
 * naming a bigger canvas as one of the ways out and not taking it. Measured
 * in a browser on 2026-09-21, that cost is plain: one router's IGP view
 * drew 19 boxes with THIRTEEN overlapping pairs, boxes sitting on top of
 * each other's text. An overlapped label is not a legible graph, and
 * re-seeding cannot fix a drawing with nowhere to put its nodes.
 *
 * Three things changed, and the measurements behind each are worth keeping
 * because two of them were dead ends:
 *
 *   1. **The canvas grows with the node count** (`canvasFor`), by area. On
 *      its own this was NOT enough: 19 nodes still overlapped at 6 seeds in
 *      20, and buying more area did not converge -- area for 8 nodes fixed
 *      19 and broke 25, for 7 fixed 25 and broke 22, for 6 fixed 22 and
 *      broke 40. Overlap past that point is not a space problem.
 *   2. **The collision force is fully resolved** -- `strength(1)` and four
 *      iterations, where d3's defaults are 0.7 and one. That took 19 nodes
 *      from 6 seeds in 20 to 2, and every other size to zero. Still not a
 *      guarantee: `forceCollide` is a relaxation, not a constraint.
 *   3. **A separation pass runs to a fixpoint afterwards** (`separate`),
 *      which is what makes it a promise instead of a tendency, and the
 *      canvas is then taken from where the boxes actually ended up.
 *
 * Measured after all three, at 30 seeds per size, on sparse graphs (a
 * connected spine) and dense ones (three edges per node): **7, 9, 13, 19,
 * 25, 40, 60 and 80 nodes all overlap at zero seeds.** Containment holds at
 * every one. One layout costs 10 ms at 19 nodes and 62 ms at 80, run once
 * per render before first paint.
 *
 * `seed` (the screens' Re-layout control) is still the operator's way to a
 * different arrangement -- it is now a preference rather than a remedy.
 */
export const LINK_DISTANCE = 150
export const CHARGE = -900
export const COLLIDE_RADIUS = 88

export interface Canvas {
  width: number
  height: number
}

/**
 * The canvas a graph of `count` nodes is laid out on: the project's
 * 580x460 default, grown to hold that many boxes at the separation
 * `COLLIDE_RADIUS` guarantees.
 *
 * THE DRAWING GROWS; IT DOES NOT CROWD. Until 2026-09-21 the canvas was a
 * fixed 580x460 and the bounds clamp beat the collision force above about a
 * dozen nodes -- measured in a browser on one router's IGP view: 19 boxes,
 * THIRTEEN overlapping pairs. That was recorded here as an accepted limit,
 * with "a bigger canvas" named as one of the ways out and not taken. It is
 * taken now, because an overlapped box hides another box's text and no
 * amount of re-seeding fixes a graph that has nowhere to put its nodes.
 *
 * Area per node, not width per node: nodes spread in two dimensions, so the
 * canvas scales by the SQUARE ROOT of the count past `FITS`. Nine nodes fit
 * 580x460; thirty-six would need twice each dimension, not four times.
 *
 * The aspect ratio is held at 580:460 so the shape an operator reads is
 * the same at every size, and so the components' own `aspect-ratio`
 * stays a single ratio rather than a per-graph one.
 *
 * Small graphs are untouched -- `Math.max(1, ...)` -- because the default
 * canvas is a design decision for them, not a fallback. A three-node graph
 * on a canvas sized for three would be three boxes filling the frame.
 */
export function canvasFor(count: number): Canvas {
  const scale = Math.max(1, Math.sqrt(count / FITS))
  return {
    width: Math.round(CANVAS.width * scale),
    height: Math.round(CANVAS.height * scale),
  }
}

/**
 * Separation, run to a fixpoint after the forces have settled: any two boxes
 * still overlapping are pushed apart along the axis they overlap LEAST, half
 * the distance each.
 *
 * This exists because a force simulation cannot promise what this drawing
 * needs. `forceCollide` is a relaxation -- it reduces overlap, it does not
 * forbid it -- and the measurements say so plainly: with the canvas grown by
 * area and collision fully resolved, 19 nodes still overlapped at 2 seeds in
 * 20, and buying more area did not converge either. Area 8x nodes fixed 19
 * and broke 25; 7x fixed 25 and broke 22; 6x fixed 22 and broke 40. Overlap
 * at that point is not a space problem and no amount of space solves it.
 *
 * IT IS THE OPPOSITE OF THE `fitToCanvas` THIS MODULE REMOVED, and the
 * distinction is the whole reason it is safe. That step SCALED settled
 * centers toward each other while the boxes kept their fixed size, so it
 * silently undid the separation the collision force had produced. This step
 * only ever increases the distance between two boxes, so it cannot undo a
 * separation -- it can only complete one. Nothing runs after it that could.
 *
 * The push is along the smaller overlap because that is the shorter way out:
 * two boxes sharing a long horizontal edge are one nudge apart vertically
 * and a whole box-width apart horizontally, and taking the long way would
 * scatter a graph that is merely touching.
 *
 * Deterministic: the pair order is the node order, the push is arithmetic,
 * and no random source is consulted. Two runs of one graph at one seed still
 * draw the same picture.
 */
function separate(nodes: LayoutNode[]): void {
  // A ceiling, not a schedule: the loop breaks the moment a pass moves
  // nothing, which is the normal exit and the one every measured size takes.
  //
  // IF IT IS EVER REACHED, THE GUARANTEE DEGRADES SILENTLY -- boxes come
  // back overlapping with no signal, which is the defect this function
  // exists to end. It is not made a throw because that would blank a screen
  // over a drawing that is merely crowded. What stands behind it instead is
  // the test above it, which holds 40 and 60 nodes at three edges each; a
  // graph needing more than 200 passes would have to be denser than
  // anything measured, and the test is what would say so.
  const MAX_PASSES = 200
  for (let pass = 0; pass < MAX_PASSES; pass++) {
    let moved = false
    for (let i = 0; i < nodes.length; i++) {
      for (let j = i + 1; j < nodes.length; j++) {
        const a = nodes[i]
        const b = nodes[j]
        const dx = (b.x ?? 0) - (a.x ?? 0)
        const dy = (b.y ?? 0) - (a.y ?? 0)
        const overlapX = NODE.width - Math.abs(dx)
        const overlapY = NODE.height - Math.abs(dy)
        if (overlapX <= 0 || overlapY <= 0) continue

        // A hair past touching, so a pair that lands exactly on the
        // threshold is not re-detected on the next pass and pushed forever.
        const push = 0.5
        if (overlapX < overlapY) {
          const half = (overlapX + push) / 2
          const dir = dx === 0 ? 1 : Math.sign(dx)
          a.x = (a.x ?? 0) - half * dir
          b.x = (b.x ?? 0) + half * dir
        } else {
          const half = (overlapY + push) / 2
          const dir = dy === 0 ? 1 : Math.sign(dy)
          a.y = (a.y ?? 0) - half * dir
          b.y = (b.y ?? 0) + half * dir
        }
        moved = true
      }
    }
    if (!moved) return
  }
}

/**
 * The canvas the settled nodes actually need: their bounding box plus a
 * margin, never smaller than the one they were laid out on.
 *
 * Taken AFTER separation, which is the point -- `separate` may push a box
 * past the bounds it settled inside, and the answer to that is a bigger
 * canvas rather than a clamp that would put the overlap back. The drawing
 * grows; it does not crowd.
 */
function canvasAround(nodes: LayoutNode[], atLeast: Canvas): Canvas {
  let maxX = 0
  let maxY = 0
  for (const n of nodes) {
    maxX = Math.max(maxX, (n.x ?? 0) + NODE.width / 2 + MARGIN)
    maxY = Math.max(maxY, (n.y ?? 0) + NODE.height / 2 + MARGIN)
  }
  return {
    width: Math.max(atLeast.width, Math.ceil(maxX)),
    height: Math.max(atLeast.height, Math.ceil(maxY)),
  }
}

/**
 * Slides every node so no box sits left of or above the margin. `separate`
 * pushes both ways, so a graph can come out of it with a node at a negative
 * coordinate; a translation fixes that without changing any distance, which
 * is what keeps the separation it just earned.
 */
function shiftIntoFrame(nodes: LayoutNode[]): void {
  let minX = Infinity
  let minY = Infinity
  for (const n of nodes) {
    minX = Math.min(minX, (n.x ?? 0) - NODE.width / 2 - MARGIN)
    minY = Math.min(minY, (n.y ?? 0) - NODE.height / 2 - MARGIN)
  }
  if (!Number.isFinite(minX)) return
  const dx = minX < 0 ? -minX : 0
  const dy = minY < 0 ? -minY : 0
  if (dx === 0 && dy === 0) return
  for (const n of nodes) {
    n.x = (n.x ?? 0) + dx
    n.y = (n.y ?? 0) + dy
  }
}

/**
 * Where a node CENTER may sit: the canvas, less half a box on each side, less
 * the margin. The simulation is clamped into this every tick.
 *
 * This replaced a post-hoc `fitToCanvas` that scaled the settled centers into
 * the canvas while the boxes kept their fixed 160x54 -- which meant a scale
 * of 0.6 turned COLLIDE_RADIUS's guaranteed 176 of separation into about 105
 * and the boxes overlapped. Measured on a running screen: zero seeds out
 * of 20 overlapped before that fit ran and 6 of 20 did after it, and
 * testing in a browser found one node's text sitting hidden under
 * another node's box. The force that guarantees separation was applied
 * and a later step quietly undid it.
 *
 * Of the three ways to stop that, this is the one that keeps the drawing
 * legible. Scaling the whole rendering -- boxes and text inside one
 * transform -- would preserve separation but shrink the labels along with
 * everything else, and the text is already the tightest thing on this canvas.
 * Feeding the eventual scale back into the force constants is circular: the
 * scale is a function of the layout the forces produce. Settling inside the
 * final bounds needs no second pass at all, so there is no later step left to
 * undo the guarantee -- containment becomes true by construction, and the
 * collision force keeps the separation it was given.
 */
export function boundsFor(canvas: Canvas) {
  return {
    minX: NODE.width / 2 + MARGIN,
    maxX: canvas.width - NODE.width / 2 - MARGIN,
    minY: NODE.height / 2 + MARGIN,
    maxY: canvas.height - NODE.height / 2 - MARGIN,
  }
}


/** An adjacency, naming its two endpoints by whatever `LayoutInput.id` returns. */
export interface LayoutEdge {
  src: string | number
  dst: string | number
}

export interface LayoutInput<T> {
  nodes: ReadonlyArray<T>
  /** How to read a node's identity. Edges name nodes by this value. */
  id: (node: T) => string | number
  edges: ReadonlyArray<LayoutEdge>
  /** Re-seeding produces a different arrangement of the same graph. */
  seed?: number
}

export interface Positioned<T> {
  node: T
  x: number
  y: number
}

/**
 * What the simulation is actually run over: a key, a slot back into the
 * caller's array, and the coordinates d3 writes.
 *
 * No attribute of the caller's node reaches this. `at` rather than the
 * caller's object itself is what keeps the module domain-free -- a field
 * named `asn` or `router_id` in here would be a second place the two screens
 * could disagree about what a node is.
 */
interface LayoutNode extends SimulationNodeDatum {
  key: string | number
  /** Index into `LayoutInput.nodes`, so the caller gets its own object back. */
  at: number
}

type LayoutLink = SimulationLinkDatum<LayoutNode>

/**
 * Node positions, settled.
 *
 * The nodes handed to d3 are copies: the simulation writes `x`, `y`, `vx`,
 * `vy` and `index` onto whatever objects it is given, and the objects here
 * belong to the caller's response (and, in tests, to a module-level fixture
 * shared by every mount in the file).
 *
 * Starting positions are derived from the node's index and `seed`, never
 * from a random source, which is what makes two runs of one graph at one
 * seed identical. d3-force's own random source is already a seeded LCG
 * created per simulation (d3-force/src/lcg.js), so where the nodes START is
 * the only nondeterminism a caller could otherwise introduce -- and it is
 * therefore also the only place a deliberate re-layout can enter.
 */
export interface SettledGraph<T> {
  /**
   * The canvas these positions need, which is an OUTPUT rather than a
   * setting: a caller draws its viewBox from this, so the frame can never
   * disagree with what was laid out inside it.
   */
  canvas: Canvas
  nodes: Positioned<T>[]
}

export function settleGraph<T>(input: LayoutInput<T>): SettledGraph<T> {
  const count = input.nodes.length
  // The canvas this graph is laid out on, and everything below derives from
  // it: the starting ring, the centering force and the bounds clamp. A
  // module constant anywhere in here would be the fixed canvas back again.
  const canvas = canvasFor(count)
  const bounds = boundsFor(canvas)
  const radius = Math.min(canvas.width, canvas.height) / 3

  // One draw per node, taken in index order, so the whole starting ring is a
  // function of (seed, node count) alone. The offset shifts a node off its
  // evenly spaced slot by up to one slot, which is enough to hand the
  // simulation a different neighborhood to converge from -- a plain rotation
  // of the whole ring would not be, since a rotated start can converge to a
  // rotated copy of the same arrangement and redraw the picture the operator
  // asked to be rid of.
  const random = seededRandom(input.seed ?? 0)
  const nodes: LayoutNode[] = input.nodes.map((n, i) => {
    const angle = (2 * Math.PI * (i + random())) / Math.max(count, 1)
    return {
      key: input.id(n),
      at: i,
      x: canvas.width / 2 + radius * Math.cos(angle),
      y: canvas.height / 2 + radius * Math.sin(angle),
    }
  })

  // forceLink throws "node not found" on an edge naming a key that is not in
  // the node set, so a caller that trims its nodes would otherwise have to
  // trim its edges in the same breath or crash the screen. Dropping the
  // dangling edge here makes the node set the single answer to "what is
  // drawn": an edge with nowhere to land is not drawable in any case.
  const drawn = new Set(nodes.map((n) => n.key))
  const links: LayoutLink[] = input.edges
    .filter((e) => drawn.has(e.src) && drawn.has(e.dst))
    .map((e) => ({ source: e.src, target: e.dst }))

  const simulation = forceSimulation<LayoutNode, LayoutLink>(nodes)
    // stop() before the first tick: forceSimulation starts a timer of its
    // own on construction, and this layout is run to convergence here
    // rather than animated into place.
    .stop()
    .force(
      'link',
      forceLink<LayoutNode, LayoutLink>(links)
        .id((n) => n.key)
        .distance(LINK_DISTANCE),
    )
    .force('charge', forceManyBody<LayoutNode>().strength(CHARGE))
    .force('collide', forceCollide<LayoutNode>(COLLIDE_RADIUS).strength(1).iterations(4))
    .force('center', forceCenter<LayoutNode>(canvas.width / 2, canvas.height / 2))

  // Bounded WHILE it settles, not scaled to fit afterwards. See BOUNDS
  // above: the collision force is what keeps two boxes apart, and a post-hoc
  // rescale of the centers silently undoes it.
  for (let tick = 0; tick < TICKS; tick++) {
    simulation.tick()
    for (const node of nodes) {
      node.x = clamp(node.x ?? 0, bounds.minX, bounds.maxX)
      node.y = clamp(node.y ?? 0, bounds.minY, bounds.maxY)
    }
  }

  // The forces get the graph close; these three make the promise. Order is
  // load-bearing: separate first (it may push a box outside), then slide
  // everything back into frame, then take the canvas from where the boxes
  // actually ended up. Each step only translates or increases distance, so
  // none of them can undo the one before -- which is exactly what the
  // removed `fitToCanvas` did.
  separate(nodes)
  shiftIntoFrame(nodes)

  return {
    canvas: canvasAround(nodes, canvas),
    nodes: nodes.map((n) => ({
      node: input.nodes[n.at],
      x: round(n.x ?? 0),
      y: round(n.y ?? 0),
    })),
  }
}

/**
 * mulberry32, a seeded generator, standing in for Math.random.
 *
 * Integer arithmetic throughout -- Math.imul and `>>> 0` are exact 32-bit
 * operations -- so one seed yields one sequence in every engine this can run
 * in. That matters more than the quality of the randomness: the layout is
 * supposed to be reproducible, and a generator whose output drifted between
 * Node and a browser would give a test one picture and an operator another.
 */
export function seededRandom(seed: number): () => number {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

function clamp(value: number, low: number, high: number) {
  return Math.min(high, Math.max(low, value))
}

/** Two decimals is finer than a pixel here and keeps the markup readable. */
export function round(value: number) {
  return Number(value.toFixed(2))
}
