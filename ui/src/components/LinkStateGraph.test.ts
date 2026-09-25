import { mount, type VueWrapper } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import LinkStateGraph from './LinkStateGraph.vue'
import { endpoint, link, node } from '@/test-support/lsBuilders'
import { NODE } from '@/lib/graphLayout'
import linkState from '@/api/fixtures/link-state.json'
import type { LsLink, LsNode } from '@/api/generated'

// "Rule N" below means the link-state rule of that number, as written out
// in LinkStateGraph.vue's doc comment.
//
// Most props below are still synthesized, and that is a deliberate choice
// now rather than a wait for a blocked capture: see
// ui/src/api/fixtures/README.md's link-state.json entry for the full
// account. Two real captured views exist -- nx-p3 (7 nodes, 20 distinct
// DIRECTED adjacencies over 10 unordered pairs, none one-sided) and xr-p2
// (0 nodes, 2 directed adjacencies, both one-sided, both legs
// `session_dumping`) -- and the tests below that can be
// driven by real rows without losing what they test now are. What stays
// synthesized either tests a case this archive cannot produce (a second
// observer or protocol or area on one canvas, a borrowed `fleet` name
// disagreeing with an `observer` one, a deliberately round bandwidth number
// chosen to demonstrate the Gbps misreading) or, in one case, cannot be
// produced AT ALL: rule 5's DRAWN one-sided edge. xr-p2 is this archive's
// only one-sided data, and xr-p2 reports zero nodes, so `drawnAdjacencies`
// drops both its one-sided rows before this component ever sees them. See
// the "marks an adjacency seen from only one side" test below.
const nxp3 = linkState['nx-p3']
const xrp2 = linkState['xr-p2']
/**
 * nx-p3's whole captured view: 7 nodes, 22 link rows, 20 distinct DIRECTED
 * adjacencies over 10 unordered pairs. Directions, not pairs: the two ends of
 * one link carry their own IGP metrics and one pair in this capture is 40 one
 * way and 1 the other, so the canvas draws 20 curves, not 10.
 */
const NXP3_NODES = nxp3.nodes.data as LsNode[]
const NXP3_LINKS = nxp3.links.data as LsLink[]
/** xr-p2's two captured link rows -- both one-sided, and both dangling: xr-p2 reports zero nodes. */
const XRP2_LINKS = xrp2.links.data as LsLink[]

/**
 * A directed pair in `links` whose reverse the data never reports, in the
 * style of `PathGraph.test.ts`'s `requireEdge`.
 *
 * xr-p2's whole captured view is exactly two adjacencies and both are
 * one-sided -- the richest one-sided sample this archive has, and the only
 * one, because the router with a complete view (nx-p3) has none. If a
 * re-capture ever resolved xr-p2's mid-dump peer and its reverse rows
 * arrived, this throws rather than leaving the tests that lean on this
 * shape silently checking nothing.
 */
function requireOneSided(links: LsLink[]): LsLink {
  const key = (a: string, b: string) => `${a}>${b}`
  const present = new Set(links.map((l) => key(l.local.node_key, l.remote.node_key)))
  const found = links.find((l) => !present.has(key(l.remote.node_key, l.local.node_key)))
  if (!found) throw new Error('fixture has no adjacency whose reverse is absent')
  return found
}

const NODES = [node('1', '10.255.0.1'), node('2', '10.255.0.2'), node('3', '10.255.0.3')]

// Seven nodes and ten adjacencies, both directions of each: the shape of the
// richest view in the archive (nx-p3 sees 7 nodes and 20 directed
// adjacencies), and dense enough that curve midpoints land on third nodes'
// boxes. Used only by the label-placement measurement below.
const RING_KEYS = ['1', '2', '3', '4', '5', '6', '7']
const RING_NODES = RING_KEYS.map((k) => node(k, `10.255.0.${k}`))
const RING_PAIRS: [string, string][] = [
  ['1', '2'],
  ['2', '3'],
  ['3', '4'],
  ['4', '5'],
  ['5', '6'],
  ['6', '7'],
  ['7', '1'],
  ['1', '4'],
  ['2', '5'],
  ['3', '6'],
]
const RING_LINKS = RING_PAIRS.flatMap(([a, b]) => [link(a, b), link(b, a)])

/** Every node's settled position, as rendered, keyed by node_key. */
function positionsOf(w: VueWrapper) {
  return w.findAll('[data-node]').map((n) => [n.attributes('data-node'), n.attributes('transform')])
}

/** Each node's box center, parsed out of the transform the component wrote. */
function boxesOf(w: VueWrapper) {
  return w.findAll('[data-node]').map((n) => {
    const t = n.attributes('transform') ?? ''
    const m = /translate\(([-\d.]+),\s*([-\d.]+)\)/.exec(t)
    return { key: n.attributes('data-node'), x: Number(m?.[1]), y: Number(m?.[2]) }
  })
}

describe('LinkStateGraph', () => {
  it('draws every adjacency at the same width, whatever the metric', () => {
    // Rule 3. IGP metric is a COST: thicker-is-higher reads as
    // better-when-worse, and thicker-is-lower inverts the convention the AS
    // graph set one screen over. max_bandwidth is more tempting and worse --
    // it is configured capacity and says nothing about what the IGP will do
    // with it -- so both are varied here, by a factor of 9000 and of 1000,
    // and neither may move a stroke.
    const w = mount(LinkStateGraph, {
      props: {
        nodes: NODES,
        links: [
          link('1', '2', { igp_metric: 1, max_bandwidth: 1e9 }),
          link('2', '1', { igp_metric: 1, max_bandwidth: 1e9 }),
          link('2', '3', { igp_metric: 9000, max_bandwidth: 1e6 }),
          link('3', '2', { igp_metric: 9000, max_bandwidth: 1e6 }),
        ],
      },
    })
    const widths = w.findAll('[data-edge]').map((e) => e.attributes('stroke-width'))
    // Four adjacencies, not zero: a component that drew no edge at all would
    // satisfy a bare "one distinct width" with an empty set.
    expect(widths).toHaveLength(4)
    expect(new Set(widths).size).toBe(1)
  })

  it('marks an adjacency seen from only one side, and says so', () => {
    // Rule 5. It is drawn, distinctly, and the screen offers no verdict on
    // why -- the data cannot tell, and in this archive one-sidedness tracks
    // view completeness rather than network faults: the router with the
    // complete view has none and the thinnest view has most of them.
    //
    // Synthesized props, and they must stay that way: xr-p2's captured view
    // (ui/src/api/fixtures/link-state.json) really does hold a one-sided
    // pair -- requireOneSided proves it below, on every run -- but xr-p2
    // reports ZERO nodes, so `drawnAdjacencies` drops both its one-sided rows
    // before this component ever sees them. Rule 5's DRAWN case has no
    // capture in this archive and cannot get one: the only one-sided data
    // belongs to the one view with nothing to draw it against. This test
    // stays the only coverage of the drawn, marked case.
    requireOneSided(XRP2_LINKS)
    const w = mount(LinkStateGraph, {
      props: { nodes: NODES, links: [link('1', '2'), link('2', '1'), link('2', '3')] },
    })
    expect(w.get('[data-edge="2-3"]').attributes('data-one-sided')).toBe('true')
    expect(w.get('[data-edge="1-2"]').attributes('data-one-sided')).toBe('false')
    expect(w.get('[data-edge="2-1"]').attributes('data-one-sided')).toBe('false')

    // The LEGEND has to carry the sentence, not merely the component's text.
    // Each line also names its own state in a <title>, and <title> text is
    // part of textContent -- so asserting against the whole component would
    // pass on a graph with no legend at all, which is the state rule 5 is
    // written against.
    expect(w.get('[data-legend]').text()).toMatch(/seen from one side/i)

    // And nowhere in anything rendered, legend or tooltip, is there a
    // verdict on why.
    expect(w.text()).not.toMatch(/one-way adjacency|misconfigur|broken|error/i)
  })

  it('shows where a node name came from, and keeps the name with its tier', () => {
    // label_source distinguishes a name this router holds from one borrowed
    // off another. Blending them would be rule 1 one level down, and
    // query/linkstate.go's own comment on lsLabelSourceExpr calls a label
    // from one tier reported as coming from another "worse than no report at
    // all".
    //
    // Node 2 is named twice here and the two rows disagree -- `fleet` on the
    // adjacency out of node 1, `observer` on the one back out of node 2.
    // Within one observer's answer that cannot happen (the API resolves a
    // label per node_key, not per row), so what this pins is the reduction's
    // direction: the WEAKER claim wins, and the label travels with the tier
    // that produced it rather than being picked separately.
    const w = mount(LinkStateGraph, {
      props: {
        nodes: NODES,
        links: [
          link('1', '2', { remote: endpoint('2', '10.255.0.2', 'p2-borrowed', 'fleet') }),
          link('2', '1'),
        ],
      },
    })
    const two = w.get('[data-node="2"]')
    expect(two.attributes('data-label-source')).toBe('fleet')
    expect(two.get('[data-node-name]').text()).toBe('p2-borrowed')

    // Node 1 is named `observer` by both rows, so this is not a component
    // that simply always answers "fleet".
    const one = w.get('[data-node="1"]')
    expect(one.attributes('data-label-source')).toBe('observer')
    expect(one.get('[data-node-name]').text()).toBe('10.255.0.1')

    // Surfaced to a reader, not only to a selector: a data attribute no eye
    // can see is not "the screen surfaces that provenance".
    expect(two.text()).toMatch(/another router/i)
  })

  it('never draws two node boxes on top of each other', () => {
    // nx-p3's whole captured view (ui/src/api/fixtures/link-state.json): 7
    // nodes and 22 link rows collapsing to 20 distinct adjacencies -- denser
    // than anything the shared layout was measured against, and a real
    // density rather than a hand-built stand-in for one. Density is what the
    // collision force trades off.
    //
    // NODE comes from the layout module rather than being typed in as 160
    // and 54. A hardcoded copy keeps passing while the real box grows, which
    // is exactly the test-that-cannot-fail this project keeps finding.
    for (let seed = 0; seed < 8; seed++) {
      const boxes = boxesOf(
        mount(LinkStateGraph, { props: { nodes: NXP3_NODES, links: NXP3_LINKS, seed } }),
      )
      expect(boxes).toHaveLength(NXP3_NODES.length)
      for (let i = 0; i < boxes.length; i++) {
        for (let j = i + 1; j < boxes.length; j++) {
          const overlaps =
            Math.abs(boxes[i].x - boxes[j].x) < NODE.width &&
            Math.abs(boxes[i].y - boxes[j].y) < NODE.height
          expect(overlaps, `seed ${seed}: boxes ${i} and ${j} overlap`).toBe(false)
        }
      }
    }
  })

  it('redraws the same graph elsewhere for a different seed, and identically for the same one', () => {
    // The screen's re-layout control is this prop and nothing else, and the
    // layout is deterministic BY DESIGN -- so a seed that does not reach the
    // starting positions leaves that control redrawing the identical
    // picture: software telling an operator something false about itself.
    //
    // The non-overlap test above cannot catch that, and it was measured
    // rather than assumed: 7 nodes at 28 directed adjacencies overlap at
    // none of 20 seeds, so a component that pinned the seed to a constant
    // passes all 8 of its iterations. This is the test that fails instead.
    const shared = { nodes: NODES, links: [link('1', '2'), link('2', '1'), link('2', '3')] }
    const first = mount(LinkStateGraph, { props: { ...shared, seed: 0 } })
    const second = mount(LinkStateGraph, { props: { ...shared, seed: 1 } })
    const again = mount(LinkStateGraph, { props: { ...shared, seed: 1 } })

    const keysOf = (w: VueWrapper) => positionsOf(w).map((p) => p[0])
    // Same nodes, drawn elsewhere. Without this a seed that dropped a node
    // would satisfy "not equal" while being a different defect entirely.
    expect(keysOf(second)).toEqual(keysOf(first))
    expect(positionsOf(second)).not.toEqual(positionsOf(first))
    expect(positionsOf(again)).toEqual(positionsOf(second))

    // Every node, not merely one: a re-layout that moved a single box would
    // pass a bare not.toEqual while giving an operator the same unreadable
    // picture they asked to be rid of.
    const before = new Map(positionsOf(first).map(([key, at]) => [key, at] as const))
    const moved = positionsOf(second).filter(([key, at]) => before.get(key) !== at)
    expect(moved).toHaveLength(NODES.length)
  })

  it('collapses parallel links onto one line and reports every metric it collapsed', () => {
    // The node-key pair is the drawing's key, because two parallel links run
    // between the same two boxes and would draw as one line with the other
    // hidden underneath. But query.LSLink keys a ROW by the observer plus
    // both node keys, both interface addresses and both link IDs, and its
    // doc comment says why: "collapsing them would report a redundant pair
    // as a single adjacency ... which is the difference between 'this path
    // has no redundancy' and 'it does'". So the line has to say what it
    // collapsed -- every distinct metric, and how many distinct links.
    //
    // The two rows below differ in both interface addresses and both link
    // IDs, which is what makes them two links rather than one row seen
    // twice; a duplicate row must NOT read as redundancy that is not there.
    const parallel = link('1', '2', {
      igp_metric: 40,
      local: { ...endpoint('1', '10.255.0.1'), ifaddr: '10.1.2.1', interface_id: 2 },
      remote: { ...endpoint('2', '10.255.0.2'), ifaddr: '10.1.2.2', interface_id: 2 },
    })
    const w = mount(LinkStateGraph, {
      props: { nodes: NODES, links: [link('1', '2', { igp_metric: 10 }), parallel, link('2', '1')] },
    })
    expect(w.findAll('[data-edge="1-2"]')).toHaveLength(1)
    expect(w.get('[data-edge-label="1-2"]').text()).toBe('10 · 40 ×2')

    // A repeated row is one link, not two. Same node pair, same addresses,
    // same link IDs, same metric.
    const twice = mount(LinkStateGraph, {
      props: { nodes: NODES, links: [link('1', '2'), link('1', '2'), link('2', '1')] },
    })
    expect(twice.get('[data-edge-label="1-2"]').text()).toBe('10')
  })

  it('draws neither a node nor an adjacency the node set does not hold', () => {
    // A thin LSDB reaches this component as an adjacency naming a node whose
    // row it never got, and this is the first caller that can hand the
    // shared layout a dangling edge -- forceLink throws "node not found" on
    // one. The module drops such an edge itself, and this does not lean on
    // that: the filter is here, so the set laid out and the set drawn are
    // one list rather than two that can disagree.
    //
    // The endpoint is NOT promoted into a node. Doing that would put a box
    // on the canvas for a router this LSDB does not hold, which is the
    // screen inventing the topology rule 1 says it cannot describe.
    const w = mount(LinkStateGraph, {
      props: {
        nodes: [node('1', '10.255.0.1'), node('2', '10.255.0.2')],
        links: [link('1', '2'), link('2', '1'), link('2', '7')],
      },
    })
    expect(w.findAll('[data-node]').map((n) => n.attributes('data-node')).sort()).toEqual(['1', '2'])
    expect(w.findAll('[data-edge]').map((e) => e.attributes('data-edge')).sort()).toEqual([
      '1-2',
      '2-1',
    ])
  })

  it('shows the dotted router-id on a node box where the API supplies one', () => {
    // `router_id` is the RAW identifier: 0aff0001 where the dotted form is
    // 10.255.0.1. Testing in a browser found the rail beside this canvas
    // showing the quad while the box showed the hex -- one screen stating
    // one identifier two ways.
    const w = mount(LinkStateGraph, {
      props: {
        nodes: [
          node('1', '0aff0001', { router_id_v4: '10.255.0.1' }),
          node('2', '10.255.0.2'),
        ],
        links: [link('1', '2'), link('2', '1')],
      },
    })
    expect(w.get('[data-node="1"] [data-node-router-id]').text()).toBe('10.255.0.1')

    // An IS-IS system ID has no dotted form at all, and then the raw value
    // is the only identity the node has -- that path is unchanged.
    const isis = mount(LinkStateGraph, {
      props: {
        nodes: [node('1', '0aff0001', { router_id_v4: '' }), node('2', '10.255.0.2')],
        links: [link('1', '2'), link('2', '1')],
      },
    })
    expect(isis.get('[data-node="1"] [data-node-router-id]').text()).toBe('0aff0001')
  })

  it('claims no naming tier for a node no adjacency names', () => {
    // A hypothetical, and necessarily so: the router this scenario is named
    // for, xr-rr1, was measured (2026-09-17, through the shipped path rather
    // than the table) to return NOTHING at all through any of the three
    // /v1/ls endpoints, not even a single node -- so there is no capture of
    // "one isolated node, no adjacency" for this component to be handed.
    // label_source is the API's answer, computed across the fleet's node
    // rows, and it arrives on an ADJACENCY endpoint -- so for a node no
    // adjacency names, this component has no tier and must not manufacture
    // one. It still has the node's own row, which is where the name comes
    // from.
    const w = mount(LinkStateGraph, {
      props: { nodes: [node('9', '10.255.0.9', { name: 'xr-rr1' })], links: [] },
    })
    const only = w.get('[data-node="9"]')
    expect(only.attributes('data-label-source')).toBeUndefined()
    expect(only.get('[data-node-name]').text()).toBe('xr-rr1')
    expect(only.get('[data-node-router-id]').text()).toBe('10.255.0.9')
    expect(w.findAll('[data-edge]')).toHaveLength(0)
  })

  it('names the router whose LSDB it drew', () => {
    // Rule 1, and it binds the picture and not only the screen around it: "a
    // picture that omits it is claiming to describe the network, which no
    // single LSDB can do". Taken from the rows drawn rather than from a prop
    // so the name cannot disagree with them. Real captured rows, not a
    // synthesized stand-in: nx-p3's whole view.
    const w = mount(LinkStateGraph, { props: { nodes: NXP3_NODES, links: NXP3_LINKS } })
    expect(w.get('[data-observer]').text()).toContain('nx-p3')
    expect(w.get('svg').attributes('aria-label')).toContain('nx-p3')

    // Handed two views' rows, it names both rather than picking one. The
    // scope control makes this unreachable and that is exactly why it is
    // asserted: silently captioning a merged picture with one router's name
    // is the failure rule 1 exists to prevent.
    const mixed = mount(LinkStateGraph, {
      props: {
        nodes: [
          node('1', '10.255.0.1'),
          node('2', '10.255.0.2', { router_sysname: 'xr-p2', router_ip: '10.0.0.12' }),
        ],
        links: [],
      },
    })
    expect(mixed.get('[data-observer]').text()).toContain('nx-p3')
    expect(mixed.get('[data-observer]').text()).toContain('xr-p2')
    expect(mixed.find('[data-scope-mixed]').exists()).toBe(true)
  })

  it('names the topology it drew, not only the router', () => {
    // Rule 4: the scope is (router, protocol, area), and a canvas that names
    // only its observer has said which LSDB it read without saying which of
    // that LSDB's topologies it drew. OSPF area 0 and area 1 share no edges.
    // Real captured rows: nx-p3's whole view is one topology, ospfv2 area 0.
    const w = mount(LinkStateGraph, { props: { nodes: NXP3_NODES, links: NXP3_LINKS } })
    const caption = w.get('[data-observer]').text()
    expect(caption).toContain('nx-p3')
    expect(caption).toContain('ospfv2')
    // Area 0 is the OSPF backbone and the most common area in this archive --
    // a real value, printed, never suppressed as a falsy default.
    expect(caption).toContain('area 0')
    expect(w.find('[data-scope-mixed]').exists()).toBe(false)

    // An ID this project has no name for reports the NUMBER. The contract is
    // explicit that "" is a positive answer and "the number beside it is the
    // whole truth", never a guess.
    const unnamed = mount(LinkStateGraph, {
      props: { nodes: [node('1', '10.255.0.1', { protocol: 9, protocol_name: '' })], links: [] },
    })
    expect(unnamed.get('[data-observer]').text()).toContain('protocol 9')
  })

  it('says when its rows are more than one topology rather than picking one', () => {
    // Rule 4 again, from the other side. Picking one of two areas would draw
    // half a topology under a caption claiming it was whole, and merging them
    // invents adjacency. The scope control makes this unreachable, which is
    // why the canvas says it rather than relying on the control to prevent it.
    const twoAreas = mount(LinkStateGraph, {
      props: { nodes: [node('1', '10.255.0.1'), node('2', '10.255.0.2', { area: 1 })], links: [] },
    })
    expect(twoAreas.get('[data-observer]').text()).toContain('areas 0, 1')
    expect(twoAreas.get('[data-scope-mixed]').text()).toMatch(/more than one/i)

    const twoProtocols = mount(LinkStateGraph, {
      props: {
        nodes: [
          node('1', '10.255.0.1'),
          node('2', '10.255.0.2', { protocol: 2, protocol_name: 'isis-l2' }),
        ],
        links: [],
      },
    })
    const spanning = twoProtocols.get('[data-observer]').text()
    expect(spanning).toContain('ospfv2')
    expect(spanning).toContain('isis-l2')
    expect(twoProtocols.find('[data-scope-mixed]').exists()).toBe(true)
  })

  it('reports max_bandwidth with its unit, and converts nothing', () => {
    // Rule 3 keeps it off the geometry; this keeps it reaching a reader at
    // all, which nothing pinned -- the field could have been deleted from the
    // tooltip in silence.
    //
    // The unit is the point. BGP-LS carries maximum bandwidth in BYTES per
    // second (RFC 5305 sec 3.4), and 1000000000 is exactly the number an
    // operator reads as "1 Gbps" -- which is wrong by a factor of eight. The
    // bare number is not neutral about that error, it is the most inviting
    // presentation of it, and the unit appears nowhere else a reader can
    // reach: not in api/openapi.yaml, not on query.LSLink.MaxBandwidth.
    const w = mount(LinkStateGraph, {
      props: {
        nodes: NODES,
        links: [link('1', '2', { max_bandwidth: 1e9 }), link('2', '1', { max_bandwidth: 1e9 })],
      },
    })
    expect(w.get('[data-edge="1-2"]').text()).toContain('max_bandwidth 1000000000 B/s')
    // Reported, not converted. A screen that cannot read the write path must
    // not invent the unit it wishes the number had.
    expect(w.text()).not.toMatch(/gbps|mbps|gbit|mbit/i)
  })

  it('names nobody over a canvas it drew nothing on, rows or no rows', () => {
    // An empty answer must not caption a blank canvas with a router's name:
    // the caption exists to say whose view this is, and over an empty canvas
    // it vouches for a view nothing drew, which is rule 1 inverted.
    const nothing = mount(LinkStateGraph, { props: { nodes: [], links: [] } })
    expect(nothing.findAll('[data-node]')).toHaveLength(0)
    expect(nothing.findAll('[data-edge]')).toHaveLength(0)
    expect(nothing.find('[data-observer]').exists()).toBe(false)

    // The case that actually matters, and the one the mount above passes for
    // the wrong reason: rows ARE present, and still nothing is drawn -- no
    // node holds either end, so the adjacency is dangling and the canvas is
    // blank. A caption derived from the props rather than from what was drawn
    // writes a real router's name over an empty picture.
    //
    // This is not a synthesized stand-in for that shape: it is xr-p2's actual
    // captured answer (ui/src/api/fixtures/link-state.json) -- `/v1/ls/nodes`
    // returns zero rows for xr-p2 while `/v1/ls/links` returns two, because
    // the nodes statement's INNER JOIN on the current session and
    // `peer_up.state = 'up'` does not qualify xr-p2's peer the way the links
    // and prefixes statements do. It is exactly the reachable case the
    // comment above describes, measured rather than imagined: the screen's
    // composable really can resolve the links query while the nodes query
    // comes back empty for the very same router. requireOneSided also pins
    // that both rows stay one-sided, which matters for the test above this
    // one -- see its own comment on why that property cannot be rendered.
    requireOneSided(XRP2_LINKS)
    const undrawable = mount(LinkStateGraph, { props: { nodes: [], links: XRP2_LINKS } })
    expect(undrawable.findAll('[data-node]')).toHaveLength(0)
    expect(undrawable.findAll('[data-edge]')).toHaveLength(0)
    expect(undrawable.find('[data-observer]').exists()).toBe(false)
    expect(undrawable.text()).not.toMatch(/xr-p1|xr-p2|xr-pe1|xr-pe2/)
    expect(undrawable.get('svg').attributes('aria-label')).not.toContain('xr-p2')
  })

  it('draws one box for a node the answer reports more than once', () => {
    // Rule 2, where this component can break it on its own: `settleGraph`
    // lays out one box per node it is given, and `/v1/ls/nodes` returns one
    // ROW per (peer, rib) as well as per node -- so a router monitored
    // through two BMP peers reports every node twice, and a canvas that drew
    // what it was handed would draw one router twice. The archive's 139 rows
    // are 7 distinct nodes in the richest view.
    //
    // Still synthesized: nx-p3's own captured view (link-state.json) is
    // scoped to a single BMP peer (10.255.0.1) and carries no duplicate
    // node_key, so this exact shape has no capture in this archive to stand
    // on.
    const w = mount(LinkStateGraph, {
      props: {
        nodes: [
          node('1', '10.255.0.1'),
          node('1', '10.255.0.1', { peer_ip: '10.0.0.22' }),
          node('2', '10.255.0.2'),
        ],
        links: [link('1', '2'), link('2', '1')],
      },
    })
    expect(w.findAll('[data-node]').map((n) => n.attributes('data-node'))).toEqual(['1', '2'])
    // And the observer caption still names one router rather than reporting
    // the duplicate as a second view.
    expect(w.get('[data-observer]').text()).toContain('nx-p3')
    expect(w.find('[data-scope-mixed]').exists()).toBe(false)
  })

  it('keeps metric labels out of the node boxes, and on their own curves', () => {
    // Testing in a browser, against a restored archive, proved this by
    // pixel diff rather than by eye: 1 to 4 of 20 metric labels were
    // painted over by a node box on 9 of 10 seeds -- hiding every label
    // changed nothing inside
    // those boxes. A 160x54 box is large against a 580x460 canvas, so a
    // curve between two distant nodes routinely carries its midpoint across
    // a THIRD node's box.
    //
    // What this can check is the anchor geometry the fix computes. What it
    // CANNOT check is the rendered result: jsdom has no text metrics and no
    // painting, so the glyph extents are estimated here exactly as the
    // component estimates them, and "is the label legible over a box it
    // could not escape" is a pixel question no test in this suite can ask.
    const boxesOf = (w: VueWrapper) =>
      w.findAll('[data-node]').map((n) => {
        const m = /translate\(([-\d.]+),\s*([-\d.]+)\)/.exec(n.attributes('transform') ?? '')
        return { x: Number(m?.[1]), y: Number(m?.[2]) }
      })
    // The component's own estimate: JetBrains Mono's 0.6em advance at 9.5px,
    // and the vertical pad it uses for a baseline-anchored label.
    const covers = (
      box: { x: number; y: number },
      at: { x: number; y: number },
      half: number,
    ) =>
      Math.abs(at.x - box.x) <= NODE.width / 2 + half &&
      Math.abs(at.y - box.y) <= NODE.height / 2 + 8

    let covered = 0
    let coveredAtMidpoint = 0
    let offCurve = 0
    for (let seed = 0; seed < 12; seed++) {
      const w = mount(LinkStateGraph, { props: { nodes: RING_NODES, links: RING_LINKS, seed } })
      const boxes = boxesOf(w)
      for (const label of w.findAll('[data-edge-label]')) {
        const half = (label.text().length * 5.7) / 2
        const at = { x: Number(label.attributes('x')), y: Number(label.attributes('y')) }
        if (boxes.some((b) => covers(b, at, half))) covered++

        // The curve this label belongs to, read back off the path.
        const d = w.get(`[data-edge="${label.attributes('data-edge-label')}"]`).attributes('d')!
        const m = /M([-\d.]+),([-\d.]+) Q([-\d.]+),([-\d.]+) ([-\d.]+),([-\d.]+)/.exec(d)!
        const [x0, y0, cx, cy, x1, y1] = m.slice(1).map(Number)
        const on = (t: number) => ({
          x: (1 - t) ** 2 * x0 + 2 * (1 - t) * t * cx + t * t * x1,
          y: (1 - t) ** 2 * y0 + 2 * (1 - t) * t * cy + t * t * y1,
        })
        if (boxes.some((b) => covers(b, on(0.5), half))) coveredAtMidpoint++
        // On its own curve, not beside it: a label offset away from the line
        // it belongs to is a worse answer on a canvas that draws both
        // directions of an adjacency as two separate curves.
        let nearest = Infinity
        for (let t = 0; t <= 1; t += 0.005) {
          const p = on(t)
          nearest = Math.min(nearest, Math.hypot(p.x - at.x, p.y - at.y))
        }
        if (nearest > 0.5) offCurve++
      }
    }

    // Non-vacuous: on this topology the midpoint really is buried, over and
    // over, which is the case the search exists for.
    expect(coveredAtMidpoint).toBeGreaterThan(0)
    // And the rendered anchors escape it far more often than not.
    expect(covered).toBeLessThan(coveredAtMidpoint)
    expect(offCurve).toBe(0)
  })

  it('draws the metric labels after the nodes, so a box cannot hide one', () => {
    // SVG paints in document order and the labels sat among the edges, so a
    // node box drawn afterwards covered them completely -- that is WHY the
    // measurement above found hidden labels rather than merely crowded ones.
    // Order is the whole mechanism, so order is what this pins; whether the
    // result is legible is a pixel question this suite cannot ask.
    const w = mount(LinkStateGraph, { props: { nodes: NODES, links: [link('1', '2')] } })
    const children = [...w.get('svg').element.children].map((el) => el.getAttribute('class'))
    expect(children).toContain('labels')
    expect(children.indexOf('labels')).toBe(children.length - 1)
    expect(children.indexOf('labels')).toBeGreaterThan(children.indexOf('edges'))
    // And the node groups really are between them, so the assertion above is
    // about paint order rather than about an empty svg.
    expect(children.filter((c) => c === 'node').length).toBeGreaterThan(0)
  })

  it('emits the node key when a node is selected', () => {
    const w = mount(LinkStateGraph, { props: { nodes: NODES, links: [link('1', '2')] } })
    w.get('[data-node="2"]').trigger('click')
    expect(w.emitted('select')?.[0]).toEqual(['2'])
  })
})
