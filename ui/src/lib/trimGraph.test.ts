import { describe, expect, it } from 'vitest'
import { trimGraph } from './trimGraph'
import type { AsEdge, AsNode, Graph } from '@/api/generated'

// Two clauses of this function, and only two, are tested here. Everything
// else it does is exercised through the components that call it -- PathGraph's
// own depth and minWeight tests run against the captured fixture, and one of
// them pins that what the component DRAWS is exactly what this function
// returns, so a divergence fails there rather than needing a second copy of
// those cases here.
//
// These two are the clauses no consumer's fixture can reach: this
// project's captured graph has no isolated AS and no graph without an
// origin, so both branches would ship on an argument alone. Built
// inline from the generated
// types, which vue-tsc checks against the same shapes a real response decodes
// into.
const seen = '2026-09-11T22:39:28.659855Z'
const node = (asn: number, routes: number, roles: AsNode['roles']): AsNode => ({
  asn,
  routes,
  roles,
  first_seen: seen,
})
const edge = (src: number, dst: number, routes: number): AsEdge => ({
  src,
  dst,
  routes,
  live_routes: routes,
  first_seen: seen,
})

describe('trimGraph', () => {
  it('keeps an AS the server sent with no edge, while dropping one a trim isolated', () => {
    // The distinction is the whole point of the clause. A box left floating
    // because its only adjacency was filtered out is an artifact of the
    // control, and drawing it would invite an operator to read "this AS
    // borders nothing" off their own threshold. A node that arrived with no
    // edge IS that finding -- a one-hop AS path contributes a node and no
    // adjacency at all -- and no trim produced it, so it stays.
    const graph: Graph = {
      nodes: [node(65001, 1, ['transit']), node(65002, 1, ['origin']), node(64999, 1, ['origin'])],
      edges: [edge(65001, 65002, 1)],
    }
    const trimmed = trimGraph(graph, { minWeight: 2 })

    expect(trimmed.edges).toEqual([])
    expect(trimmed.nodes.map((n) => n.asn)).toEqual([64999])
  })

  it('leaves a graph with no origin alone rather than trimming it to nothing', () => {
    // Depth counts hops from an origin, so a graph containing none has
    // nothing to measure from. Returning an empty graph there would report a
    // measurement that was never taken as an answer about the network, and it
    // is reachable for real: a scope whose every matched route is still being
    // walked, or a family whose paths all end outside it.
    const graph: Graph = {
      nodes: [node(65001, 1, ['transit']), node(65002, 1, ['transit'])],
      edges: [edge(65001, 65002, 1)],
    }
    expect(trimGraph(graph, { depth: 0 })).toEqual(graph)
  })
})
