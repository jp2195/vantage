import { describe, expect, it } from 'vitest'
import { settleGraph, NODE } from './graphLayout'

// The layout is domain-free on purpose: it is shared by the AS-path graph and
// the IGP graph, whose nodes have nothing in common but a position. These
// fixtures use bare ids so a change that reintroduced a domain field (an asn,
// a router_id) would not compile.
const ids = (n: number) => Array.from({ length: n }, (_, i) => ({ id: `n${i}` }))

describe('settleGraph', () => {
  it('lays the same graph out identically for the same seed', () => {
    const input = {
      nodes: ids(6),
      id: (n: { id: string }) => n.id,
      edges: [
        { src: 'n0', dst: 'n1' },
        { src: 'n1', dst: 'n2' },
        { src: 'n2', dst: 'n3' },
      ],
      seed: 1,
    }
    const a = settleGraph(input)
    const b = settleGraph(input)
    expect(a.nodes.map((p) => [p.x, p.y])).toEqual(b.nodes.map((p) => [p.x, p.y]))
    expect(a.canvas).toEqual(b.canvas)
  })

  it('gives a different arrangement for a different seed', () => {
    const base = { nodes: ids(6), id: (n: { id: string }) => n.id, edges: [] }
    const a = settleGraph({ ...base, seed: 1 })
    const b = settleGraph({ ...base, seed: 2 })
    expect(a.nodes.map((p) => [p.x, p.y])).not.toEqual(b.nodes.map((p) => [p.x, p.y]))
  })

  it('never overlaps two node boxes, across seeds', () => {
    // The defect this guards against is real and shipped once: a fit step
    // scaled node CENTERS while leaving box sizes fixed, so forceCollide's
    // separation guarantee silently stopped holding. Geometry, not attributes.
    for (let seed = 0; seed < 12; seed++) {
      const laid = settleGraph({
        nodes: ids(8),
        id: (n: { id: string }) => n.id,
        edges: [
          { src: 'n0', dst: 'n1' },
          { src: 'n1', dst: 'n2' },
          { src: 'n2', dst: 'n0' },
          { src: 'n3', dst: 'n4' },
        ],
        seed,
      }).nodes
      for (let i = 0; i < laid.length; i++) {
        for (let j = i + 1; j < laid.length; j++) {
          const dx = Math.abs(laid[i].x - laid[j].x)
          const dy = Math.abs(laid[i].y - laid[j].y)
          const overlaps = dx < NODE.width && dy < NODE.height
          expect(overlaps, `seed ${seed}: ${laid[i].node.id} overlaps ${laid[j].node.id}`).toBe(
            false,
          )
        }
      }
    }
  })

  // The 8-node case above is under the canvas's old capacity, so it passed
  // while the real screens overlapped: measured in a browser 2026-09-21, one
  // router's IGP view drew 19 boxes with THIRTEEN overlapping pairs. The
  // canvas was a fixed 580x460 and the bounds clamp beat the collision
  // force above about a dozen nodes.
  //
  // Nothing overlaps now, at any size: the canvas GROWS with the node count
  // so forceCollide's separation always has somewhere to put a node. 24 is
  // well past the live 19 and past every count a real deployment produces.
  it('never overlaps two node boxes at a size the old fixed canvas could not hold', () => {
    for (const count of [14, 19, 24]) {
      for (let seed = 0; seed < 12; seed++) {
        const laid = settleGraph({
          nodes: ids(count),
          id: (n: { id: string }) => n.id,
          // A connected spine plus stragglers: links pull nodes together,
          // which is what makes crowding a layout problem rather than an
          // arithmetic one.
          edges: Array.from({ length: count - 1 }, (_, i) => ({
            src: `n${i}`,
            dst: `n${i + 1}`,
          })),
          seed,
        }).nodes
        for (let i = 0; i < laid.length; i++) {
          for (let j = i + 1; j < laid.length; j++) {
            const dx = Math.abs(laid[i].x - laid[j].x)
            const dy = Math.abs(laid[i].y - laid[j].y)
            expect(
              dx < NODE.width && dy < NODE.height,
              `${count} nodes, seed ${seed}: ${laid[i].node.id} overlaps ${laid[j].node.id}`,
            ).toBe(false)
          }
        }
      }
    }
  })

  it('keeps every node BOX inside the canvas, not merely its center', () => {
    // The box, because the box is what clips. This asserted centers against
    // [0, CANVAS.width] x [0, CANVAS.height] and was strictly weaker than the
    // invariant it was named for: BOUNDS insets by half a box plus the
    // margin, and dropping the half-box inset -- mutating BOUNDS.maxX to
    // CANVAS.width, which IS the clipping defect -- still satisfies
    // `p.x <= 580`, so that version passed the mutation. Half-extents are
    // added here rather than importing BOUNDS, which would assert the
    // implementation against itself and pass the same mutation again.
    //
    // Deliberately NOT the margin: MARGIN is a legibility choice this test
    // has no business pinning, while "the whole box is on the canvas" is the
    // guarantee the doc comment on BOUNDS makes and the one a browser can
    // contradict. PathGraph.test.ts holds the rendered-geometry version of
    // the same invariant one layer up; this is the module's own, and
    // LinkStateGraph has no containment test of its own to lean on.
    const halfW = NODE.width / 2
    const halfH = NODE.height / 2
    // 24 as well as 10: separation runs after the bounds clamp and can push
    // a box past it, so the sizes that actually need growing are the ones
    // where containment could break. A 10-node-only test never reaches that
    // path.
    for (const count of [10, 24])
      for (let seed = 0; seed < 12; seed++) {
        // Against the canvas the layout REPORTS, not a module constant: the
        // canvas is an output now, and a test pinned to 580x460 would fail the
        // moment a graph legitimately grew past it -- which is the behavior
        // this module exists to provide.
        const { nodes: laid, canvas } = settleGraph({
          nodes: ids(count),
          id: (n: { id: string }) => n.id,
          edges: [],
          seed,
        })
        for (const p of laid) {
          expect(
            p.x - halfW,
            `seed ${seed}: ${p.node.id} clipped at the left`,
          ).toBeGreaterThanOrEqual(0)
          expect(
            p.y - halfH,
            `seed ${seed}: ${p.node.id} clipped at the top`,
          ).toBeGreaterThanOrEqual(0)
          expect(
            p.x + halfW,
            `seed ${seed}: ${p.node.id} clipped at the right`,
          ).toBeLessThanOrEqual(canvas.width)
          expect(
            p.y + halfH,
            `seed ${seed}: ${p.node.id} clipped at the bottom`,
          ).toBeLessThanOrEqual(canvas.height)
        }
      }
  })

  // The sizes and densities the module's doc comment CLAIMS were measured.
  //
  // That comment records zero overlap at 7, 9, 13, 19, 25, 40, 60 and 80
  // nodes on sparse and dense graphs, measured with a one-off script
  // that was deleted once it had answered. The tests that stayed
  // covered 24 nodes on a sparse spine, so a regression past that
  // shipped green against a comment promising otherwise. A claim worth
  // writing down is worth a test that fails when it stops being true.
  //
  // Dense here is three edges per node, which is what crowds a layout: what
  // pulls boxes together is the links, not the count.
  it('holds the non-overlap promise at the sizes and densities its doc claims', () => {
    for (const count of [40, 60]) {
      for (let seed = 0; seed < 4; seed++) {
        const laid = settleGraph({
          nodes: ids(count),
          id: (n: { id: string }) => n.id,
          edges: Array.from({ length: count * 3 }, (_, i) => ({
            src: `n${i % count}`,
            dst: `n${(i * 7 + 3) % count}`,
          })).filter((e) => e.src !== e.dst),
          seed,
        }).nodes
        for (let i = 0; i < laid.length; i++) {
          for (let j = i + 1; j < laid.length; j++) {
            const dx = Math.abs(laid[i].x - laid[j].x)
            const dy = Math.abs(laid[i].y - laid[j].y)
            expect(
              dx < NODE.width && dy < NODE.height,
              `${count} dense nodes, seed ${seed}: ${laid[i].node.id} overlaps ${laid[j].node.id}`,
            ).toBe(false)
          }
        }
      }
    }
  })

  it('drops edges whose endpoints are not in the node set', () => {
    // forceLink throws "node not found" on a dangling id. A caller that trims
    // its node set must not have to trim its edges too.
    expect(() =>
      settleGraph({
        nodes: ids(2),
        id: (n: { id: string }) => n.id,
        edges: [{ src: 'n0', dst: 'gone' }],
        seed: 0,
      }),
    ).not.toThrow()
  })
})
