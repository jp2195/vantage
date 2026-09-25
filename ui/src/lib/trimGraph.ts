import type { Graph } from '@/api/generated'

/**
 * The two client-side trims the AS-paths screen offers, as one pure function.
 *
 * `/v1/topology` returns the COMPLETE graph for a scope, so Depth and Min
 * paths are views of a graph already in the browser rather than parameters --
 * neither is a refetch. That makes the trim a piece of shared knowledge
 * rather than a detail of one component: `PathGraph` draws its result, and
 * `TopologyView` counts it, and those two numbers have to be the same
 * numbers. They were not: the screen's summary line and its empty-state gate
 * read the UNTRIMMED graph while the canvas drew the trimmed one, so a Min
 * paths above the heaviest edge left a blank 580x460 pane with "5 ASNs · 4
 * edges" written above it and no empty state anywhere. Extracting the trim is
 * what makes "what is drawn" a thing both files can ask the same question
 * about.
 */
export interface Trim {
  /** Maximum hops from an origin AS. Undefined keeps the whole graph. */
  depth?: number
  /** Minimum `routes` an edge must carry. Undefined keeps every edge. */
  minWeight?: number
}

/**
 * Hops from an origin AS, following edges backwards.
 *
 * An AS path is ordered peer-first and origin-last, so an edge's `dst` is the
 * hop nearer the origin and walking `dst -> src` counts hops the way the
 * Depth control reads them. An AS no such walk reaches has no finite depth
 * and is absent from the map; the caller trims it at any depth.
 */
function hopsFromOrigin(graph: Graph): Map<number, number> {
  const upstream = new Map<number, number[]>()
  for (const edge of graph.edges) {
    upstream.set(edge.dst, [...(upstream.get(edge.dst) ?? []), edge.src])
  }

  const hops = new Map<number, number>()
  let frontier = graph.nodes.filter((n) => n.roles.includes('origin')).map((n) => n.asn)
  for (const asn of frontier) hops.set(asn, 0)

  for (let hop = 1; frontier.length > 0; hop++) {
    const next: number[] = []
    for (const asn of frontier) {
      for (const src of upstream.get(asn) ?? []) {
        if (hops.has(src)) continue
        hops.set(src, hop)
        next.push(src)
      }
    }
    frontier = next
  }
  return hops
}

/**
 * The graph actually drawn: the server's, minus what the two controls trim.
 *
 * The controls compose, and in this order, because Min paths decides which
 * adjacencies count and Depth then counts hops over the ones that do.
 */
export function trimGraph(graph: Graph, trim: Trim): Graph {
  let nodes = graph.nodes
  let edges = graph.edges

  // Min paths filters EDGES, so a node it strips of every adjacency goes with
  // them: a box left floating because its only edge was filtered out is an
  // artifact of the control rather than a finding. A node the server sent
  // with no edge at all stays -- an AS in the answer that borders nothing in
  // it is data, and no trim produced that.
  if (trim.minWeight !== undefined) {
    const min = trim.minWeight
    edges = edges.filter((e) => e.routes >= min)
    const adjacentBefore = new Set(graph.edges.flatMap((e) => [e.src, e.dst]))
    const adjacentNow = new Set(edges.flatMap((e) => [e.src, e.dst]))
    nodes = nodes.filter((n) => adjacentNow.has(n.asn) || !adjacentBefore.has(n.asn))
  }

  // Depth filters NODES, and its answer is the node set itself: an origin
  // within the depth stays even where the trim left it with no edge drawn,
  // because "the origins" is what depth 0 was asked for.
  if (trim.depth !== undefined) {
    const max = trim.depth
    const hops = hopsFromOrigin({ nodes, edges })
    // No origin anywhere means nothing to measure hops from. Trimming the
    // graph to nothing would be a worse answer than not trimming it.
    if (hops.size > 0) {
      const within = new Set(
        nodes.filter((n) => (hops.get(n.asn) ?? Infinity) <= max).map((n) => n.asn),
      )
      nodes = nodes.filter((n) => within.has(n.asn))
      edges = edges.filter((e) => within.has(e.src) && within.has(e.dst))
    }
  }

  return { nodes, edges }
}
