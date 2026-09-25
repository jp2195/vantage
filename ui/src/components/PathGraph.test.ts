import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import PathGraph from './PathGraph.vue'
import topology from '@/api/fixtures/topology.json'
import { trimGraph } from '@/lib/trimGraph'
import type { AsEdge, AsNode, Graph } from '@/api/generated'

// The graph below came off the wire, not out of a test author's head (see
// ui/src/api/fixtures/README.md's `topology.json` entry): GET
// /v1/topology?router=172.22.0.7&peer=172.31.0.90, against a local dev
// deployment's api service, chosen because it is the only scope in the
// archive whose graph contains an edge every route has withdrawn. That
// edge is the whole reason the withdrawn test below can assert anything.
//
// The cast is about TypeScript, not about the data: resolveJsonModule types
// every string in an imported .json as `string`, while AsNode.roles is a
// three-value union, so a captured "origin" does not structurally satisfy
// the type it literally came from. Same cast and same reason as
// src/test-support/capturedMeta.ts.
const graph = topology.data.unicast as Graph

const HOUR_MS = 60 * 60 * 1000

/**
 * An edge the fixture must contain, in the state this test needs it in.
 *
 * find() narrows on the predicate but not on the field, and neither narrows
 * on `live_routes` at all -- so `want` is checked here rather than assumed.
 * If a re-capture ever dropped the withdrawn edge, or re-advertised it, the
 * tests below must fail with that sentence rather than silently passing
 * against a graph that no longer has the case they were written for.
 */
function requireEdge(g: Graph, src: number, dst: number, want: 'live' | 'withdrawn'): AsEdge {
  const edge = g.edges.find((e) => e.src === src && e.dst === dst)
  if (!edge) throw new Error(`fixture has no edge ${src} -> ${dst}`)
  const got = edge.live_routes === 0 ? 'withdrawn' : 'live'
  if (got !== want) {
    throw new Error(
      `fixture edge ${src} -> ${dst} is ${got}, not ${want} (live_routes ${edge.live_routes})`,
    )
  }
  return edge
}

/**
 * The fixture's lightest and heaviest edge, which must not be the same weight.
 *
 * Stroke width encodes paths carried, and a graph whose every edge carries
 * the same number of routes draws every stroke identically -- so the encoding
 * is not merely untested there, it is invisible. The first capture of this
 * fixture was exactly that graph, and the width tests below passed against it
 * while proving nothing. This throws instead.
 */
function requireVariedWeights(g: Graph): { lightest: AsEdge; heaviest: AsEdge } {
  const byWeight = [...g.edges].sort((a, b) => a.routes - b.routes)
  const lightest = byWeight[0]
  const heaviest = byWeight[byWeight.length - 1]
  if (!lightest || !heaviest || lightest.routes === heaviest.routes) {
    throw new Error(
      `fixture edges all carry ${lightest?.routes ?? 0} route(s); stroke width encodes nothing`,
    )
  }
  return { lightest, heaviest }
}

/** The fixture's nodes, with the same loud failure if a re-capture empties them. */
function requireNodes(g: Graph): AsNode[] {
  if (g.nodes.length === 0) throw new Error('fixture graph has no nodes')
  return g.nodes
}

/** The selector the component publishes for an edge, derived rather than typed in. */
const edgeSelector = (e: AsEdge) => `[data-edge="${e.src}-${e.dst}"]`

/** Each node's rendered box: its center from the transform, its size from the rect. */
function boxesOf(w: VueWrapper) {
  return w.findAll('[data-node]').map((node) => {
    const rect = node.get('rect')
    const [x, y] = node.attributes('transform')!.match(/-?\d+(\.\d+)?/g)!.map(Number)
    return {
      asn: node.attributes('data-node'),
      x,
      y,
      width: Number(rect.attributes('width')),
      height: Number(rect.attributes('height')),
    }
  })
}

/** Every node's settled position, keyed by ASN, as rendered. */
function positionsOf(w: VueWrapper) {
  return w.findAll('[data-node]').map((n) => [n.attributes('data-node'), n.attributes('transform')])
}

describe('PathGraph', () => {
  it('lays the same graph out identically on every mount', () => {
    // Pre-settled, not animated: the simulation runs to convergence before
    // first paint. An operator comparing two loads must see the same
    // picture, and an animated force layout gives a different one every
    // time. The only nondeterminism a caller of d3-force can introduce is
    // in where the nodes START -- the library's own random source is a
    // seeded LCG created per simulation (d3-force/src/lcg.js) -- so this
    // asserts the component seeds its own positions rather than rolling
    // them.
    const a = mount(PathGraph, { props: { graph } })
    const b = mount(PathGraph, { props: { graph } })
    expect(positionsOf(a)).toEqual(positionsOf(b))
    expect(positionsOf(a)).toHaveLength(requireNodes(graph).length)
    // Identical is not enough on its own: a layout that collapsed every node
    // onto the canvas center would be perfectly reproducible, and would also
    // sit inside the bounds the next test checks. Distinct positions are what
    // makes "the same picture" a picture.
    expect(new Set(positionsOf(a).map((p) => p[1])).size).toBe(graph.nodes.length)
  })

  it('lays a graph out differently under a different seed, and identically under the same one', () => {
    // The screen's "Re-layout" control is this prop, and nothing else. A
    // force layout can settle into an arrangement that is correct and
    // unreadable -- two boxes overlapping, an edge running under a node --
    // and offering another arrangement is the remedy for that. But the
    // layout above is deterministic BY DESIGN, so without a seed reaching
    // the starting positions that button redraws the identical picture: a
    // control that does nothing, which is the software telling an operator
    // something false about itself.
    //
    // Both halves are asserted here because they are in tension. Moving the
    // picture is the point; moving it for a reason other than the seed
    // would break the guarantee the test above pins, which is that an
    // operator comparing two loads of one graph sees one picture.
    const first = mount(PathGraph, { props: { graph, seed: 0 } })
    const second = mount(PathGraph, { props: { graph, seed: 1 } })
    const again = mount(PathGraph, { props: { graph, seed: 1 } })

    const nodesOf = (w: VueWrapper) => positionsOf(w).map((p) => p[0])
    // Same nodes, drawn elsewhere. Without this a seed that dropped a node
    // would satisfy "not equal" while being a different defect entirely.
    expect(nodesOf(second)).toEqual(nodesOf(first))
    expect(positionsOf(second)).not.toEqual(positionsOf(first))
    expect(positionsOf(again)).toEqual(positionsOf(second))

    // Every node, not merely one: a re-layout that moved a single box and
    // left the rest where they were would pass a bare not.toEqual while
    // giving an operator the same unreadable picture they asked to be rid
    // of. Derived from the fixture's own node count rather than typed in.
    // `as const` so the pairs type as tuples: positionsOf hands back
    // string[] entries, and Map's constructor wants [k, v].
    const before = new Map(positionsOf(first).map(([asn, at]) => [asn, at] as const))
    const moved = positionsOf(second).filter(([asn, at]) => before.get(asn) !== at)
    expect(moved).toHaveLength(requireNodes(graph).length)
  })

  it('settles every node inside the canvas', () => {
    // jsdom implements no SVG layout APIs, so nothing here can measure the
    // rendered picture -- and a force layout that converges off-canvas
    // renders a blank pane that every attribute-reading test still passes.
    // The positions are computed in JS and written as attributes precisely
    // so this is checkable at all; the bounds come from the rendered
    // viewBox and the rendered box size, not from constants copied out of
    // the component.
    const w = mount(PathGraph, { props: { graph } })
    const [, , width, height] = w.get('svg').attributes('viewBox')!.split(/[\s,]+/).map(Number)

    for (const node of w.findAll('[data-node]')) {
      const box = node.get('rect')
      const halfW = Number(box.attributes('width')) / 2
      const halfH = Number(box.attributes('height')) / 2
      const [x, y] = node.attributes('transform')!.match(/-?\d+(\.\d+)?/g)!.map(Number)
      expect(x - halfW).toBeGreaterThanOrEqual(0)
      expect(y - halfH).toBeGreaterThanOrEqual(0)
      expect(x + halfW).toBeLessThanOrEqual(width)
      expect(y + halfH).toBeLessThanOrEqual(height)
    }
  })

  it('never draws two node boxes on top of each other, at any seed', () => {
    // The collision force holds node CENTERS 176 apart, and two 160x54 boxes
    // can only overlap within 169 of each other, so separation is a
    // guarantee -- right up until a later step moves the centers and leaves
    // the boxes their fixed size. A fit that scaled the settled centers into
    // the canvas did exactly that at a scale of about 0.6, turning 176 of
    // separation into 105; testing in a browser found AS65002's role
    // text sitting under AS64511, and not one test here saw it, because
    // every one of them reads attributes and none of them read geometry.
    //
    // Twelve seeds rather than one. That defect showed on 6 of 20 seeds of
    // this graph, so a single-seed version of this test had five chances in
    // six of passing against it -- which is the same reason the layout is
    // checked here at all rather than trusted to the force constants.
    const collisions: string[] = []
    for (let seed = 0; seed < 12; seed++) {
      const boxes = boxesOf(mount(PathGraph, { props: { graph, seed } }))
      expect(boxes).toHaveLength(requireNodes(graph).length)
      for (const [i, a] of boxes.entries()) {
        for (const b of boxes.slice(i + 1)) {
          const overlapX = (a.width + b.width) / 2 - Math.abs(a.x - b.x)
          const overlapY = (a.height + b.height) / 2 - Math.abs(a.y - b.y)
          if (overlapX > 0 && overlapY > 0) {
            collisions.push(
              `seed ${seed}: AS${a.asn} and AS${b.asn} overlap by ` +
                `${overlapX.toFixed(1)} x ${overlapY.toFixed(1)}px`,
            )
          }
        }
      }
    }
    expect(collisions, collisions.join('\n  ')).toEqual([])
  })

  it('renders a withdrawn edge dashed rather than omitting it', () => {
    // A withdrawn edge is data, not a filter. live_routes === 0 is the whole
    // signal: the collector knows this adjacency and the network no longer uses
    // it. Dropping the edge would report the collector's knowledge as absence.
    const withdrawn = requireEdge(graph, 65002, 64511, 'withdrawn')
    const w = mount(PathGraph, { props: { graph } })
    const edge = w.get(edgeSelector(withdrawn))
    expect(edge.attributes('stroke-dasharray')).toBeTruthy()
    expect(edge.attributes('data-state')).toBe('withdrawn')
  })

  it('says what stroke width means', () => {
    // A thin edge is not a weak link. Without this sentence a thick line reads
    // as bandwidth, which is a claim this data cannot support: the weight is
    // paths carried in THIS answer and says nothing about the link.
    const w = mount(PathGraph, { props: { graph } })
    expect(w.get('[data-legend]').text()).toMatch(/paths carried/i)
    expect(w.get('[data-legend]').text()).not.toMatch(/bandwidth|capacity/i)
  })

  it('says first seen rather than appeared, which the contract disclaims', () => {
    // AsEdge.first_seen is the earliest collector timestamp among the routes
    // that CURRENTLY traverse the edge, and the contract says in as many
    // words that a client deriving "newly appeared" from it is claiming
    // something about those routes rather than about the adjacency's age,
    // "and it can be wrong in both directions". The API had that same false
    // promise deleted from it earlier; a legend key
    // reading "appeared in window" would put it back one layer up.
    //
    // The forbidden word here is not an arbitrary one: it is the exact word
    // the contract's own disclaimer names. data-state stays "appeared" --
    // that is a style hook, and the window test above pins it.
    const legend = mount(PathGraph, { props: { graph } }).get('[data-legend]').text()
    expect(legend).toMatch(/first seen in window/i)
    expect(legend).not.toMatch(/\bappeared\b/i)
  })

  it('colors an edge as appeared only when first_seen is inside the window', () => {
    // The server deliberately does NOT send a state string: it sends
    // first_seen and live_routes, and the WINDOW is a thing the operator
    // moves. A baked-in state would make the control a lie.
    //
    // A LIVE edge, not the withdrawn one: an edge with live_routes 0 is
    // withdrawn whatever the window says, so running this on 65002 -> 64511
    // would test window handling against a value the window cannot move.
    //
    // `now` is injected rather than read from the clock. The fixture's
    // first_seen values are absolute and a window is relative to now, so a
    // test against the real clock would pass or fail by the calendar: with
    // the capture minutes old both windows say "appeared", an hour later
    // they differ, and past the 90-day retention bound they agree again.
    // Deriving the instant from the fixture's own newest edge keeps the
    // separation fixed forever: at two hours past it, every edge is older
    // than the one-hour window and every edge is inside the ninety-day one.
    const recent = requireEdge(graph, 65001, 65002, 'live')
    const newest = Math.max(...graph.edges.map((e) => Date.parse(e.first_seen)))
    const now = newest + 2 * HOUR_MS

    const narrow = mount(PathGraph, { props: { graph, now, windowHours: 1 } })
      .get(edgeSelector(recent))
      .attributes('data-state')
    const wide = mount(PathGraph, { props: { graph, now, windowHours: 24 * 90 } })
      .get(edgeSelector(recent))
      .attributes('data-state')

    expect(narrow).not.toEqual(wide)
    expect(wide).toBe('appeared')

    // And the same window on the OTHER side of the edge, which is what pins
    // the injected instant itself rather than just the comparison. Half an
    // hour past the newest edge, a one-hour window INCLUDES this one. A
    // component that ignored `now` and read the wall clock would answer both
    // mounts from the same instant and could not produce "stable" above and
    // "appeared" here from one window width -- which is exactly the mutation
    // that passed every assertion before this line existed.
    const inside = mount(PathGraph, {
      props: { graph, now: newest + HOUR_MS / 2, windowHours: 1 },
    })
      .get(edgeSelector(recent))
      .attributes('data-state')
    expect(inside).toBe('appeared')
  })

  it('drops edges under minWeight without refetching anything', () => {
    // The server returns the complete graph for the scope; Min paths is a view
    // of a graph already in the browser. The threshold is derived from the
    // fixture rather than typed in -- one above the heaviest edge drops every
    // edge whatever the capture's weights are -- so a re-capture that moves
    // them still exercises the trim. (The fixture's weights are 3, 1, 1, 1,
    // not all 1, which is what makes the stroke-width tests below able to
    // fail at all.)
    const heaviest = Math.max(...graph.edges.map((e) => e.routes))
    const all = mount(PathGraph, { props: { graph } })
    const trimmed = mount(PathGraph, { props: { graph, minWeight: heaviest + 1 } })
    expect(all.findAll('[data-edge]')).toHaveLength(graph.edges.length)
    expect(trimmed.findAll('[data-edge]')).toHaveLength(0)
  })

  it('draws a heavier edge as a thicker line', () => {
    // The width encoding itself, which needs unequal weights to pin: against
    // a graph where every edge carries one route, a component that draws
    // every stroke the same width passes every other test here.
    const { lightest, heaviest } = requireVariedWeights(graph)
    const w = mount(PathGraph, { props: { graph } })
    const widthOf = (e: AsEdge) => Number(w.get(edgeSelector(e)).attributes('stroke-width'))
    expect(widthOf(heaviest)).toBeGreaterThan(widthOf(lightest))
  })

  it('scales stroke width against the whole answer, not the surviving edges', () => {
    // Thickness is paths carried IN THIS ANSWER. Normalize against the edges
    // a trim left behind instead and the SAME edge redraws thicker when the
    // operator raises Min paths, with nothing changed in the data underneath
    // -- an encoding that moves when a control moves is not that one.
    //
    // The trim is `depth`, not `minWeight`, and that is not a limit of this
    // fixture -- it is a property of the control. minWeight keeps the edges
    // at or above a threshold, so the heaviest edge survives every threshold
    // that leaves anything at all: the maximum over the survivors is always
    // the maximum over the whole answer, and no minWeight value can tell the
    // two normalizations apart. Measured, not argued -- with the trimmed-set
    // normalization in place, `minWeight: 2` leaves the heaviest edge drawing
    // the same 4 it draws untrimmed. `depth: 0` is the trim that CAN remove
    // the heaviest edge, and it leaves this light one behind to be measured.
    //
    // The varied-weights guard belongs here as much as on its two siblings:
    // against a uniform re-capture, max(trimmed) and max(whole) are equal by
    // arithmetic, both mounts draw the same width, and this test passes under
    // the very normalization defect it exists to catch -- while the siblings
    // throw in the same run, which reads as one flaky fixture guard rather
    // than as a test that has stopped testing anything.
    requireVariedWeights(graph)
    const kept = requireEdge(graph, 65002, 64511, 'withdrawn')
    const whole = mount(PathGraph, { props: { graph } })
    const trimmed = mount(PathGraph, { props: { graph, depth: 0 } })

    expect(trimmed.findAll('[data-edge]').length).toBeLessThan(graph.edges.length)
    expect(trimmed.get(edgeSelector(kept)).attributes('stroke-width')).toBe(
      whole.get(edgeSelector(kept)).attributes('stroke-width'),
    )
  })

  it('trims hops from the origin for depth', () => {
    // Depth is the other client-side trim. An AS path is ordered
    // peer-first and origin-last, so depth 0 is the origin ASNs alone: the
    // fixture's three origin nodes survive, and the two transit-only ASNs
    // the paths reach them through do not.
    const origins = requireNodes(graph).filter((n) => n.roles.includes('origin'))
    const w = mount(PathGraph, { props: { graph, depth: 0 } })
    const drawn = w.findAll('[data-node]').map((n) => Number(n.attributes('data-node')))
    const byNumber = (a: number, b: number) => a - b
    expect(drawn.sort(byNumber)).toEqual(origins.map((n) => n.asn).sort(byNumber))
    expect(drawn.length).toBeLessThan(graph.nodes.length)
  })

  it('draws exactly what trimGraph says is drawn, so the screen can count it', () => {
    // The screen around this component states how much of the graph is on
    // screen ("4 edges, 1 drawn") and decides whether to show an empty state
    // instead of a blank canvas. Both answers come from @/lib/trimGraph, and
    // they are only true while THIS component draws that same set -- a
    // second, drifting copy of the trim in here would put a count beside a
    // picture it no longer describes, which is the defect that made the
    // function shared in the first place.
    //
    // Both controls at once, and at values the fixture makes meaningful: the
    // lightest edge's weight is the threshold that separates anything at all,
    // and depth 1 keeps more than the origins without keeping everything.
    const { lightest } = requireVariedWeights(graph)
    const trim = { depth: 1, minWeight: lightest.routes + 1 }
    const expected = trimGraph(graph, trim)
    const w = mount(PathGraph, { props: { graph, ...trim } })

    const byNumber = (a: number, b: number) => a - b
    expect(w.findAll('[data-node]').map((n) => Number(n.attributes('data-node'))).sort(byNumber))
      .toEqual(expected.nodes.map((n) => n.asn).sort(byNumber))
    expect(w.findAll('[data-edge]').map((e) => e.attributes('data-edge')).sort()).toEqual(
      expected.edges.map((e) => `${e.src}-${e.dst}`).sort(),
    )
    // Not vacuous: this trim really does remove something, so a component
    // that ignored both props would fail rather than match an untrimmed
    // expectation.
    expect(expected.nodes.length).toBeLessThan(graph.nodes.length)
  })

  it('emits the ASN of the node clicked', async () => {
    // The rail, the looking glass tab and the "Query AS" button all hang
    // off this: selecting a node is how the graph hands a scope onward.
    const node = requireNodes(graph)[0]
    const w = mount(PathGraph, { props: { graph } })
    await w.get(`[data-node="${node.asn}"]`).trigger('click')
    expect(w.emitted('select')).toEqual([[node.asn]])
  })
})
