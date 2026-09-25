import { describe, expect, it } from 'vitest'
import { distinctAdjacencies, distinctNodes, drawnAdjacencies, drawnLinkRows } from './lsAdjacencies'
import { endpoint, link, node } from '@/test-support/lsBuilders'

// "Rule N" below means the link-state rule of that number, as written out
// in LinkStateGraph.vue's doc comment.
//
// Pinned here as well as through the canvas, because this module has two
// consumers by design: the graph draws its result and the screen counts it.
// A contract enforced only through one consumer's DOM is a contract the other
// consumer can be written against wrongly -- which is the defect
// @/lib/trimGraph was extracted to end one screen over.

describe('distinctAdjacencies', () => {
  it('collapses rows onto one adjacency per directed node pair, keeping direction', () => {
    // Rule 2: rows are not facts. The archive's 229 ls_links rows are 20, 8
    // and 7 distinct adjacencies across the three views that have any, and
    // direction is meaningful here -- a link is advertised by the node at its
    // local end, and nothing pairs A->B with B->A.
    const found = distinctAdjacencies([
      link('1', '2'),
      link('1', '2'),
      link('2', '1'),
      link('2', '3'),
    ])
    expect(found.map((a) => a.key).sort()).toEqual(['1-2', '2-1', '2-3'])
  })

  it('marks an adjacency the data holds in one direction only', () => {
    // Rule 5. Two-sided means the REVERSED KEY is in the data, and it is
    // symmetric by construction, so both halves of a pair answer alike.
    const byKey = new Map(
      distinctAdjacencies([link('1', '2'), link('2', '1'), link('2', '3')]).map((a) => [a.key, a]),
    )
    expect(byKey.get('1-2')?.oneSided).toBe(false)
    expect(byKey.get('2-1')?.oneSided).toBe(false)
    expect(byKey.get('2-3')?.oneSided).toBe(true)
  })

  it('counts parallel links by the identity the query keys on, so a repeated row counts once', () => {
    // query.LSLink keys a row by the observer plus both node keys, both
    // interface addresses and both link IDs, because collapsing parallel
    // links "would report a redundant pair as a single adjacency ... the
    // difference between 'this path has no redundancy' and 'it does'".
    // Counting ROWS would be that defect pointing the other way: a re-read,
    // two concatenated pages or a second observer's copy would each invent
    // redundancy that is not in the data.
    const parallel = link('1', '2', {
      igp_metric: 40,
      max_bandwidth: 1e6,
      local: { ...endpoint('1', '10.255.0.1'), ifaddr: '10.1.2.1', interface_id: 2 },
      remote: { ...endpoint('2', '10.255.0.2'), ifaddr: '10.1.2.2', interface_id: 2 },
    })
    const [bundle] = distinctAdjacencies([link('1', '2', { igp_metric: 10 }), parallel])
    expect(bundle.members).toBe(2)
    expect(bundle.metrics).toEqual([10, 40])
    expect(bundle.bandwidths).toEqual([1e6, 1e9])

    const [repeated] = distinctAdjacencies([link('1', '2'), link('1', '2'), link('1', '2')])
    expect(repeated.members).toBe(1)
    expect(repeated.metrics).toEqual([10])
  })
})

describe('drawnAdjacencies', () => {
  it('drops an adjacency naming a node the answer does not hold', () => {
    // A thin LSDB names nodes whose rows this router never got. forceLink
    // throws "node not found" on an edge like that, and the count has to
    // match the picture, so the filter is here rather than inside the layout.
    const nodes = [node('1', '10.255.0.1'), node('2', '10.255.0.2')]
    const links = [link('1', '2'), link('2', '1'), link('2', '7')]
    expect(drawnAdjacencies(links, nodes).map((a) => a.key).sort()).toEqual(['1-2', '2-1'])
    // Dropping it does not change the answer for anything else: one-sidedness
    // is read over the whole data before any of this.
    expect(drawnAdjacencies(links, nodes).every((a) => !a.oneSided)).toBe(true)
    // And the unfiltered answer still holds all three, so the filter is real.
    expect(distinctAdjacencies(links)).toHaveLength(3)
  })
})

describe('drawnLinkRows', () => {
  it('returns the rows behind the drawn lines, which carry the scope the adjacency drops', () => {
    // An adjacency is a collapse and carries no protocol or area; rule 4 is a
    // question about exactly those, so naming the topology needs the rows.
    const nodes = [node('1', '10.255.0.1'), node('2', '10.255.0.2')]
    const rows = drawnLinkRows([link('1', '2'), link('2', '7')], nodes)
    expect(rows).toHaveLength(1)
    expect(rows[0].protocol_name).toBe('ospfv2')
  })
})

describe('distinctNodes', () => {
  it('collapses one node reported several times into one node', () => {
    // Rule 2 on the node axis. A router monitored through two BMP peers
    // returns every node it holds twice -- LSNodes groups by peer_ip and rib
    // as well as by node_key -- and the archive's 139 ls_nodes rows are 7
    // distinct nodes in the richest view. A caller that laid out one box per
    // ROW would draw the same router twice and count it twice.
    const collapsed = distinctNodes([
      node('1', '10.255.0.1'),
      node('1', '10.255.0.1', { peer_ip: '10.0.0.22' }),
      node('2', '10.255.0.2'),
    ])
    expect(collapsed.map((n) => n.node_key)).toEqual(['1', '2'])
  })

  it('keeps the first row seen, and every field on it', () => {
    // Not a merge of the duplicates: the per-node fields are already
    // resolved server-side within each group, so the row kept is a whole,
    // self-consistent observation rather than fields taken from two.
    const collapsed = distinctNodes([
      node('1', '10.255.0.1', { name: 'nx-p3', srgb_base: 16000 }),
      node('1', '10.255.0.1', { name: 'stale', srgb_base: 99 }),
    ])
    expect(collapsed).toHaveLength(1)
    expect(collapsed[0].name).toBe('nx-p3')
    expect(collapsed[0].srgb_base).toBe(16000)
  })

  it('leaves a set with nothing repeated exactly as it was', () => {
    // So the collapse cannot be credited for an answer it did not change:
    // nx-p3's whole captured view is seven distinct nodes, and every one of
    // its node rows is already distinct.
    const rows = [node('1', '10.255.0.1'), node('2', '10.255.0.2'), node('3', '10.255.0.3')]
    expect(distinctNodes(rows)).toEqual(rows)
  })
})
