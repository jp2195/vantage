import { type VueWrapper, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick, ref } from 'vue'
import topology from '@/api/fixtures/topology.json'
import routers from '@/api/fixtures/routers.json'
import peers from '@/api/fixtures/peers.json'
import PathGraph from '@/components/PathGraph.vue'
import ResultMeta from '@/components/ResultMeta.vue'
import { type GuardedColumn, inventedColumns } from '@/test-support/columnGuard'
import { capturedMeta } from '@/test-support/capturedMeta'
import { formatClock } from '@/lib/formatClock'
import type { AsEdge, AsName, AsNode, Graph, Meta, TopologyFanout } from '@/api/generated'
import type { TopologyScope } from '@/api/queries'
import { declarationsOf } from '@/test-support/styleText'

// The captured answer, off the wire (ui/src/api/fixtures/README.md): GET
// /v1/topology?router=172.22.0.7&peer=172.31.0.90 against a lab
// deployment's own api service. Its unicast graph carries the one
// adjacency in this lab that every route has withdrawn, and its vpn and
// evpn members are the empty graphs this lab genuinely produces -- no
// MPLS module, no VXLAN dataplane, both environment limits of the host,
// not FRR ones (see the fixture's provenance in
// ui/src/api/fixtures/README.md).
//
// The cast is about TypeScript, not about the data: resolveJsonModule types
// every string in an imported .json as `string`, while AsNode.roles is a
// three-value union, so a captured "origin" does not structurally satisfy the
// type it literally came from. Same cast and same reason as
// src/test-support/capturedMeta.ts.
const captured = {
  data: topology.data as TopologyFanout,
  meta: capturedMeta(topology.meta),
}

/** A node the fixture must carry, so a re-capture that drops it fails loudly. */
function requireNode(g: Graph, asn: number): AsNode {
  const node = g.nodes.find((n) => n.asn === asn)
  if (!node) throw new Error(`fixture graph has no node AS${asn}`)
  return node
}

/**
 * An edge the fixture must carry, in the state this suite needs it in.
 *
 * Modeled on PathGraph.test.ts's own requireEdge, and for its reason: find()
 * narrows on the predicate but not on `live_routes`, so a re-capture that
 * re-advertised 65002 -> 64511 would leave the rail's withdrawn-edge test
 * quietly asserting withdrawal against a live edge.
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
 * A family the capture must have answered EMPTY, with both arrays present.
 *
 * The empty-family test below is about the RESPONSE -- `[]` rather than null,
 * and both keys present -- which is why it runs on the capture rather than on
 * an inline prop. This throws if a future capture fills the family in, because
 * at that point the test would be describing a case the fixture no longer
 * holds while still passing.
 */
function requireEmptyFamily(fanout: TopologyFanout, family: 'vpn' | 'evpn'): Graph {
  const g = fanout[family]
  if (!Array.isArray(g?.nodes) || !Array.isArray(g?.edges)) {
    throw new Error(`fixture ${family} is not a graph with both arrays: ${JSON.stringify(g)}`)
  }
  if (g.nodes.length > 0 || g.edges.length > 0) {
    throw new Error(`fixture ${family} is no longer empty (${g.nodes.length} nodes)`)
  }
  return g
}

// SYNTHESIZED, and sanctioned as such: a props object annotated with the
// generated types cannot drift from the contract, because vue-tsc checks it
// against the same types the real response decodes into. It exists because
// this lab cannot produce the two cases below, so no capture can:
//
//   - three families that are all NON-empty at once, which is what makes
//     "the screen shows one family at a time, never the three merged"
//     falsifiable. Against the capture, a screen that merged all three would
//     draw the unicast graph either way, since the other two are empty.
//   - a small-but-real graph. The EVPN baseline is two edges; this
//     lab's EVPN graph is empty for environment reasons, and 0 is not 2.
//
// The three families carry DISJOINT ASNs on purpose. That is the property a
// merge is detectable by: any node from another family appearing while this
// one is selected is a merge, and no overlap can hide it.
const ASNS = {
  unicast: [65001, 65002, 65003],
  vpn: [64601, 64602],
  evpn: [64701, 64702, 64703],
} as const

const SYNTH_SEEN = '2026-09-11T22:39:28.659855Z'

function node(asn: number, routes: number, roles: AsNode['roles']): AsNode {
  return { asn, routes, roles, first_seen: SYNTH_SEEN }
}

function edge(src: number, dst: number, routes: number, live: number): AsEdge {
  return { src, dst, routes, live_routes: live, first_seen: SYNTH_SEEN }
}

const threeFamilies: TopologyFanout = {
  unicast: {
    nodes: [
      node(ASNS.unicast[0], 2, ['transit']),
      node(ASNS.unicast[1], 2, ['transit']),
      node(ASNS.unicast[2], 2, ['origin']),
    ],
    edges: [
      edge(ASNS.unicast[0], ASNS.unicast[1], 2, 2),
      edge(ASNS.unicast[1], ASNS.unicast[2], 2, 2),
    ],
  },
  vpn: {
    nodes: [node(ASNS.vpn[0], 1, ['transit']), node(ASNS.vpn[1], 1, ['origin'])],
    edges: [edge(ASNS.vpn[0], ASNS.vpn[1], 1, 1)],
  },
  // A small graph, deliberately: 3 ASNs, 2 edges.
  evpn: {
    nodes: [
      node(ASNS.evpn[0], 1, ['transit']),
      node(ASNS.evpn[1], 1, ['transit']),
      node(ASNS.evpn[2], 1, ['origin']),
    ],
    edges: [
      edge(ASNS.evpn[0], ASNS.evpn[1], 1, 1),
      edge(ASNS.evpn[1], ASNS.evpn[2], 1, 1),
    ],
  },
}

const synthMeta: Meta = { warnings: [], total_matched: 9, next_cursor: null }

// Set inside every test before mounting, never left at these values: the
// isolation rule this suite holds requires each test to pass alone under `-t`,
// so nothing may depend on what an earlier test left behind.
let answer: { data: TopologyFanout; meta: Meta } | undefined = captured
let topologyError: Error | undefined

// AS holder names, kept apart from the topology answer above: no test
// outside the "AS holder names" describe block below cares about names,
// so its default -- a dataset loaded with no rows for it to say anything
// about -- must render exactly like no names feature existed at all. That
// matches this lab's own realistic case: every ASN in the captured
// fixture's graphs is private-range and genuinely unlisted.
// Whether the composable capped this graph's batch. Depth and Min paths
// bound drawn.nodes well below the cap at ordinary settings, but nothing
// enforces that, and the rail is exactly where an un-looked-up ASN would
// read as an unlisted one.
let asNamesTruncated = false

let asNamesFixture: { data: AsName[]; meta: Meta } | undefined = {
  data: [],
  meta: { warnings: [], total_matched: null, asnames_loaded: true },
}

// The scope ref the screen hands its composable, captured on the way past.
// Mocking useTopology as a function that ignores its argument would leave the
// whole scope control untested: the mode-to-parameter mapping is the only
// thing standing between "around an AS" and a request the daemon refuses, and
// a stub that returns rows regardless would render the same graph however the
// question was composed.
let scopeRef: { value: TopologyScope | undefined } | undefined

// Settable so one test can hand the picker a peer two collectors watch.
// Everything else drives the captured single-collector answer.
let peersFixture: { data: unknown[]; meta: unknown } = peers

vi.mock('@/api/queries', () => ({
  // ScopePicker's own two composables. The screen does not call them; the
  // picker it mounts for the "from a peer" mode does.
  useRouters: () => ({ data: ref(routers), isLoading: ref(false), error: ref(undefined) }),
  usePeers: () => ({ data: ref(peersFixture), isLoading: ref(false), error: ref(undefined) }),
  useTopology: (scope: { value: TopologyScope | undefined }) => {
    scopeRef = scope
    return {
      data: ref(topologyError ? undefined : answer),
      // Returned because the real composable returns it, not because the
      // screen reads it: see the loading test below for why the flag is not
      // part of the condition.
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(topologyError),
    }
  },
  useAsNames: () => ({
    data: ref(asNamesFixture),
    isPending: ref(false),
    isLoading: ref(false),
    error: ref(undefined),
    truncated: ref(asNamesTruncated),
  }),
}))

const TopologyView = (await import('./TopologyView.vue')).default

function freshState() {
  answer = captured
  topologyError = undefined
  scopeRef = undefined
  peersFixture = peers
  asNamesFixture = {
    data: [],
    meta: { warnings: [], total_matched: null, asnames_loaded: true },
  }
  asNamesTruncated = false
}

/**
 * Drives the real scope control the way an operator does, rather than
 * reaching past the form to whatever function it calls. The submit handler is
 * the seam under test.
 */
async function search(
  w: VueWrapper,
  opts: { mode?: 'prefix' | 'asn'; term: string; toggle?: boolean },
) {
  await w.get(`[data-mode="${opts.mode ?? 'prefix'}"]`).trigger('click')
  await w.get('form.scope input.term').setValue(opts.term)
  if (opts.toggle) await w.get('form.scope input[type="checkbox"]').setValue(true)
  await w.get('form.scope').trigger('submit')
}

/** Mounted with a question already asked, for every test about an ANSWER. */
async function mountAnswered() {
  const w = mount(TopologyView)
  await search(w, { term: '10.10.1.0/24' })
  return w
}

/** The ASNs the graph pane is currently drawing. */
function drawn(w: VueWrapper): number[] {
  return w.findAll('[data-node]').map((n) => Number(n.attributes('data-node')))
}

/** Every node's rendered position, so a re-layout can be seen to move them. */
function positions(w: VueWrapper): string[] {
  return w.findAll('[data-node]').map((n) => n.attributes('transform')!)
}

/**
 * The rail's labels, as the column guard reads them: a `data-node-field` or
 * `data-edge-field` element is a LABEL (a dt or a th), its attribute names the
 * response field behind it and its text is the header an operator reads. Built
 * from the DOM rather than from a list the component exports, so a label added
 * to the template without a field behind it is caught at the point it is
 * rendered.
 */
function fieldsOf(w: VueWrapper, attr: 'data-node-field' | 'data-edge-field'): GuardedColumn[] {
  return w.findAll(`[${attr}]`).map((el) => ({ id: el.attributes(attr)!, header: el.text() }))
}

describe('TopologyView', () => {
  beforeEach(freshState)

  it('asks nothing until a scope is chosen, because an unscoped graph is refused', async () => {
    // /v1/topology answers a request that scopes nothing with a 400 whose own
    // sentence says the alternative "would draw every route in the archive as
    // one graph". Fetching on mount would send that refused request on every
    // visit -- and an empty graph rendered underneath it would say this scope
    // reaches no AS at all, which is a positive claim about a question nobody
    // asked.
    const w = mount(TopologyView)
    expect(scopeRef?.value).toBeUndefined()
    expect(w.findComponent(PathGraph).exists()).toBe(false)
    // Nothing that describes an answer, either: a summary line over a graph
    // nobody asked for is the same claim in words.
    expect(w.find('[data-summary]').exists()).toBe(false)
    expect(w.find('[data-empty-family]').exists()).toBe(false)
    // And the three ways in are all on offer up front, matching the
    // endpoint's own three scope modes.
    expect(w.findAll('[data-mode]').map((b) => b.attributes('data-mode'))).toEqual([
      'prefix',
      'asn',
      'peer',
    ])
  })

  it('sends a prefix search as prefix=, and a covering one as covers=', async () => {
    // The two are mutually exclusive on the wire (the endpoint 400s on both),
    // so the checkbox has to REPLACE the parameter rather than add one.
    const w = mount(TopologyView)
    await search(w, { term: '10.10.1.0/24' })
    expect(scopeRef?.value).toEqual({ prefix: '10.10.1.0/24' })

    const covering = mount(TopologyView)
    await search(covering, { term: '10.10.1.1', toggle: true })
    expect(scopeRef?.value).toEqual({ covers: '10.10.1.1' })
  })

  it('sends an ASN search as origin_asn=, and an in-path one as through_asn=', async () => {
    // Two different questions: what an AS ORIGINATES, and what merely crosses
    // it. Collapsing them would answer the question the operator did not ask,
    // and for a transit-only AS origin_asn= answers with nothing at all.
    const w = mount(TopologyView)
    await search(w, { mode: 'asn', term: '65001' })
    expect(scopeRef?.value).toEqual({ origin_asn: 65001 })

    const inPath = mount(TopologyView)
    await search(inPath, { mode: 'asn', term: '65001', toggle: true })
    expect(scopeRef?.value).toEqual({ through_asn: 65001 })
  })

  it('sends a chosen router and peer as the pair, which is this endpoint alone accepts', async () => {
    // /v1/topology's one declared departure from /v1/routes: router= AND
    // peer= together are a sufficient scope here, and either half alone is
    // half a scope. Driving the two selects is what proves the picker is
    // wired to the scope rather than that a scope object can be built.
    const w = mount(TopologyView)
    await w.get('[data-mode="peer"]').trigger('click')
    const selects = w.findAll('select')
    await selects[0].setValue(routers.data[0].ip)
    await selects[1].setValue(peers.data[1].peer_ip)
    expect(scopeRef?.value).toEqual({
      router: routers.data[0].ip,
      peer: peers.data[1].peer_ip,
    })
    // The picker resolves a session too, because the RIB walk it was built
    // for needs one. This endpoint is not pinned to a session -- it joins the
    // current one server-side -- so sending it would be a parameter the
    // contract does not define.
    expect(scopeRef?.value).not.toHaveProperty('session')
  })

  // ScopePicker's advisory belongs to the Routes screen, where /v1/rib/*
  // takes collector= and the walk really is pinned to one. /v1/topology
  // takes /v1/routes' scope parameters and none of them is a collector, so
  // the sentence would claim a narrowing this screen never performs -- and
  // the Collector select beside it would change the request in no way.
  //
  // What is true here is better than a pin: since 2026-09-20 the three counts
  // deduplicate on the route identity with the collector removed, so a
  // dual-homed router's graph is not doubled. The banner says that instead.
  it('says both collectors feed one graph rather than claiming to walk one of them', async () => {
    const base = peers.data[1]
    peersFixture = {
      data: [
        { ...base, collector: 'coll-a', session_id: '111', state: 'up' },
        { ...base, collector: 'coll-b', session_id: '222', state: 'view_lost' },
      ],
      meta: peers.meta,
    }
    const w = mount(TopologyView)
    await w.get('[data-mode="peer"]').trigger('click')
    const selects = w.findAll('select')
    await selects[0].setValue(base.router_ip)
    await selects[1].setValue(base.peer_ip)
    await nextTick()

    const banner = w.find('.ambiguous')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toMatch(/disagree/i)
    expect(banner.text()).not.toMatch(/walking/i)
    expect(banner.text()).toMatch(/route identities|not collector copies/i)
    expect(w.find('select[data-collector]').exists()).toBe(false)
  })

  it('draws one family at a time, never the three merged', async () => {
    // Unicast, VPN and EVPN are drawn one family at a time, never merged:
    // merging them would assert that an EVPN adjacency and a unicast
    // adjacency are the same kind of edge, which no BGP speaker claims. The
    // three synthesized families carry disjoint ASNs, so a merge is visible
    // as a node from another family appearing in this one.
    answer = { data: threeFamilies, meta: synthMeta }
    const w = await mountAnswered()

    expect(drawn(w).sort()).toEqual([...ASNS.unicast].sort())
    await w.get('[data-family="vpn"]').trigger('click')
    expect(drawn(w).sort()).toEqual([...ASNS.vpn].sort())
    await w.get('[data-family="evpn"]').trigger('click')
    expect(drawn(w).sort()).toEqual([...ASNS.evpn].sort())

    // And the count, so a merge cannot hide behind a sort: the union of the
    // three is strictly larger than any one of them.
    const all = [...ASNS.unicast, ...ASNS.vpn, ...ASNS.evpn]
    expect(drawn(w).length).toBeLessThan(all.length)
  })

  it('states a small graph counts rather than letting it read as a failure', async () => {
    // The EVPN graph is two edges wide -- small is the truth
    // there, not a symptom -- and an operator who cannot tell "small" from
    // "broken" has been told the wrong thing. So a real graph states its size
    // and is DRAWN; only a genuinely empty family gets the empty state.
    answer = { data: threeFamilies, meta: synthMeta }
    const w = await mountAnswered()
    await w.get('[data-family="evpn"]').trigger('click')

    const small = threeFamilies.evpn
    expect(w.get('[data-summary]').text()).toContain(`${small.nodes.length} ASNs`)
    expect(w.get('[data-summary]').text()).toContain(`${small.edges.length} edges`)
    expect(w.find('[data-empty-family]').exists()).toBe(false)
    expect(w.findComponent(PathGraph).exists()).toBe(true)
  })

  it('says an empty family is empty, rather than drawing a blank canvas', async () => {
    // The adjacent case to the one above, and they must not contradict each
    // other. This runs on the CAPTURE because its subject is the response:
    // this lab's vpn and evpn arms come back as `{nodes: [], edges: []}`, and
    // the contract defines an empty array as the positive claim "nothing
    // there", never "we did not look". A blank 580x460 canvas says neither --
    // it reads as a screen that failed.
    const empty = requireEmptyFamily(captured.data, 'vpn')
    const w = await mountAnswered()
    await w.get('[data-family="vpn"]').trigger('click')

    const note = w.get('[data-empty-family]')
    expect(w.findComponent(PathGraph).exists()).toBe(false)
    expect(w.get('[data-summary]').text()).toContain(`${empty.nodes.length} ASNs`)
    // Nothing failed, and the screen must not imply it did: an empty family
    // is ordinary. role="alert" is how this repo marks the states that ARE
    // failures (the error line below, ScopePicker's ambiguity warning), so
    // its absence here is the assertion, not decoration.
    expect(note.attributes('role')).toBeUndefined()
    expect(note.text()).not.toMatch(/error|failed/i)
    // And the rail does not ask for something that cannot be done: there is
    // no node to select in an empty graph.
    expect(w.get('[data-rail]').text()).not.toMatch(/select an AS/i)
  })

  it('counts matched routes across all three families, not as this family paths', async () => {
    // meta.total_matched is the route identities the scope matched SUMMED
    // ACROSS the three families (api/topology.go says so where it computes
    // it), while ASNs and edges are one family's own. Printing them in one
    // breath -- "13 paths · 6 ASNs · 8 edges" -- would put three numbers
    // side by side that describe two different populations, which is this
    // repo's most recurring defect. So the count is stated separately and
    // names its population, and switching family must not move it.
    answer = { data: threeFamilies, meta: synthMeta }
    const w = await mountAnswered()
    const matched = w.get('[data-matched]').text()

    expect(matched).toContain(String(synthMeta.total_matched))
    expect(matched).toMatch(/all three families/i)
    await w.get('[data-family="vpn"]').trigger('click')
    expect(w.get('[data-matched]').text()).toBe(matched)
    // ... while the family's own counts DO move, or the test above would
    // pass against a screen that printed one frozen line for everything. The
    // NODE count rather than the edge count, because VPN's single edge reads
    // "1 edge" and asserting the plural form here would be asserting the
    // summary's grammar rather than that it followed the family.
    expect(w.get('[data-summary]').text()).toContain(`${threeFamilies.vpn.nodes.length} ASNs`)
  })

  it('says one matched route in the singular', async () => {
    // A scope matching exactly one route is the ordinary case for an exact
    // prefix, which is the screen's default mode -- so "1 matched routes" is
    // not a rare edge, it is what an operator sees the first time they look
    // up a prefix. The count itself is real; only its grammar was not.
    answer = { data: captured.data, meta: { ...captured.meta, total_matched: 1 } }
    const one = await mountAnswered()
    expect(one.get('[data-matched]').text()).toContain('1 matched route,')
    expect(one.get('[data-matched]').text()).not.toContain('1 matched routes')

    // The plural still reads as a plural, or a fix that simply dropped the
    // "s" everywhere would pass the assertion above.
    freshState()
    answer = { data: captured.data, meta: { ...captured.meta, total_matched: 2 } }
    const two = await mountAnswered()
    expect(two.get('[data-matched]').text()).toContain('2 matched routes')
  })

  it('binds Depth and Min paths to the graph rather than asking the server again', async () => {
    // The server returns the complete graph for the scope, so both controls
    // are views of a graph already in the browser. A refetch would be a
    // second, differently-timed answer to a question that was already
    // answered -- and on this endpoint it would also be a second full scan
    // for the wide scopes.
    const w = await mountAnswered()
    const asked = JSON.stringify(scopeRef?.value)

    await w.get('[data-depth]').setValue('1')
    await w.get('[data-min-weight]').setValue('2')

    const graph = w.findComponent(PathGraph)
    expect(graph.props('depth')).toBe(1)
    expect(graph.props('minWeight')).toBe(2)
    expect(JSON.stringify(scopeRef?.value)).toBe(asked)
  })

  it('says when the trim emptied the canvas, instead of blanking it under a count that disagrees', async () => {
    // Depth and Min paths are views of a graph already in the browser, and
    // they can legitimately leave nothing to draw: a threshold above the
    // heaviest edge strips every edge, and then every node that was adjacent
    // to one. PathGraph draws the TRIMMED graph while the summary and the
    // empty-state gate read the untrimmed one -- so without this the operator
    // gets a blank 580x460 pane, no empty state at all (the gate asked the
    // untrimmed graph and it is not empty), and a line above it still reading
    // "5 ASNs · 4 edges".
    //
    // The two causes must not read alike, which is why this is a separate
    // marker from the empty family: "this family is empty" is the server's
    // answer about the network, and "your trim left nothing" is the
    // operator's own doing and is undone by moving the control back.
    const graph = captured.data.unicast
    const heaviest = Math.max(...graph.edges.map((e) => e.routes))
    const w = await mountAnswered()
    // Derived from the fixture rather than typed in, so a re-capture with
    // heavier edges still exercises the trim.
    await w.get('[data-min-weight]').setValue(String(heaviest + 1))

    expect(w.findComponent(PathGraph).exists()).toBe(false)
    expect(w.find('[data-empty-family]').exists()).toBe(false)
    const note = w.get('[data-empty-trim]')
    expect(note.text()).toMatch(/depth|min paths/i)
    expect(note.attributes('role')).toBeUndefined()

    // And the count beside it states both numbers rather than the untrimmed
    // one alone, which is the half that was actively wrong: a summary that
    // still says "4 edges" over an empty canvas is a screen contradicting
    // itself.
    expect(w.get('[data-summary]').text()).toContain(`${graph.edges.length} edges, 0 drawn`)
  })

  it('states both counts when a trim hides some of the graph, and one when it hides none', async () => {
    // The partial case, which is the ordinary one: an operator narrowing
    // Depth wants to know what is no longer on screen. Depth 0 is the origin
    // ASNs alone -- PathGraph's own test pins that -- so the fixture's five
    // nodes become its three origins.
    const graph = captured.data.unicast
    const origins = graph.nodes.filter((n) => n.roles.includes('origin'))
    expect(origins.length).toBeGreaterThan(0)
    expect(origins.length).toBeLessThan(graph.nodes.length)

    const w = await mountAnswered()
    // Nothing trimmed: one number, not two. Without this the suffix could be
    // printed always, which would make the assertion below meaningless.
    expect(w.get('[data-summary]').text()).not.toMatch(/drawn/)

    await w.get('[data-depth]').setValue('0')
    expect(w.get('[data-summary]').text()).toContain(
      `${graph.nodes.length} ASNs, ${origins.length} drawn`,
    )
    // Still a graph, so still drawn: a trim that hides some of it is not an
    // empty state.
    expect(w.findComponent(PathGraph).exists()).toBe(true)
    expect(w.find('[data-empty-trim]').exists()).toBe(false)
  })

  it('drops a selected AS that a trim has removed from the canvas', async () => {
    // The same rule the family switch follows, for the other way a selection
    // stops being about the picture on screen. A transit-only AS is exactly
    // what depth 0 removes, so the rail would otherwise go on describing an
    // AS the canvas beside it no longer draws.
    const transit = requireNode(captured.data.unicast, 65001)
    expect(transit.roles).not.toContain('origin')

    const w = await mountAnswered()
    await w.get(`[data-node="${transit.asn}"]`).trigger('click')
    expect(w.get('[data-rail]').text()).toContain(`AS${transit.asn}`)

    await w.get('[data-depth]').setValue('0')
    expect(w.get('[data-rail]').text()).not.toContain(`AS${transit.asn}`)
  })

  it('names the trim control by what it filters, never as a weight', async () => {
    // An edge's `routes` is paths carried in THIS answer -- not capacity,
    // not preference, not stability, not health. A control labeled
    // "Weight" would mislead: the rail's column for the identical field
    // says "Paths", and PathGraph's legend denies the capacity reading in
    // as many words. An operator who sets "Weight >= 2" and watches the
    // thin edges vanish would have been handed the capacity reading by the
    // control row, with the denial sitting below the canvas, after the act.
    //
    // The prop and the test attribute stay `minWeight` / data-min-weight:
    // those are code, and nobody operating this screen reads them.
    const w = await mountAnswered()
    expect(w.get('.controls').text()).toContain('Min paths')
    expect(w.get('.controls').text()).not.toMatch(/weight/i)
    expect(w.find('[data-min-weight]').exists()).toBe(true)
  })

  it('names the window the first-seen coloring is measured against', async () => {
    // PathGraph colors an edge by whether first_seen falls inside a window,
    // and its legend says "first seen in window" -- so a screen that never
    // named the window would be showing a colored edge against a bound the
    // operator was never told. The same failure /v1/events' and
    // /v1/routes/history's `since` were made explicit for.
    const w = await mountAnswered()
    const control = w.get<HTMLSelectElement>('[data-window]')
    await control.setValue('168')
    expect(w.findComponent(PathGraph).props('windowHours')).toBe(168)

    // The SELECTED option's own words, not the select's whole text: a
    // <select>'s text() concatenates every option, so scanning it for "7
    // days" would pass however the control was set -- including against a
    // screen whose chosen window and displayed window disagreed.
    const chosen = control
      .findAll('option')
      .find((o) => (o.element as HTMLOptionElement).value === control.element.value)
    expect(chosen?.text()).toMatch(/7 days/i)
  })

  it('moves the drawn graph when Re-layout is pressed', async () => {
    // The layout is deterministic by design, which is what lets an operator
    // compare two loads of one graph -- and which also means a graph that
    // settles into an unreadable arrangement settles into the same one every
    // time. Re-layout is the remedy. Without the seed reaching PathGraph the
    // button redraws the identical picture: a control that does nothing,
    // which is the software telling the operator something false about
    // itself.
    const w = await mountAnswered()
    const before = positions(w)
    expect(before.length).toBeGreaterThan(0)

    await w.get('[data-relayout]').trigger('click')
    const after = positions(w)

    expect(after).toHaveLength(before.length)
    expect(after).not.toEqual(before)
  })

  it('names a selected AS by its number, and does not fabricate a holder name it has none for', async () => {
    // Now that a real (optional) holder-name dataset exists: the
    // heading itself must stay bare -- the name goes on its own line
    // in the rail, never appended to "AS3356" -- and, separately, nothing
    // may fabricate a name for an AS the dataset does not actually name.
    // This test's own AS names fixture stays at its default (a dataset
    // loaded with no row for this ASN), the realistic state in this lab
    // deployment where every ASN is private-range and genuinely unlisted.
    const node = requireNode(captured.data.unicast, 65001)
    const w = await mountAnswered()
    await w.get(`[data-node="${node.asn}"]`).trigger('click')

    // The heading is anchored on its own, because the defect this guards
    // against is not an invented FIELD -- it is a name appended to the
    // number a curious implementer looked up: "AS3356 (Level 3)". Anchored,
    // so that fails the moment it appears.
    expect(w.get('[data-rail] h2').text()).toMatch(/^AS\d+$/)
    expect(w.get('[data-rail]').text()).toContain(`AS${node.asn}`)
    // No dataset row names this AS, so the name line itself must not exist
    // -- not merely be empty text, which an unguarded `{{ '' }}` would also
    // satisfy while still rendering a hollow element.
    expect(w.find('[data-holder-name]').exists()).toBe(false)
    for (const invented of ['owner', 'org', 'whois', 'irr']) {
      expect(w.find(`[class*="${invented}"]`).exists()).toBe(false)
      expect(w.find(`[data-node-field="${invented}"]`).exists()).toBe(false)
    }
  })

  it('dates the routes in this answer, not the AS itself', async () => {
    // AsNode.first_seen is the earliest collection time among the routes in
    // THIS answer that traverse the AS -- bounded by the route tables' 90-day
    // TTL, and moved by a re-advertisement. A rail that let it read as the age
    // of the AS, or of its place in the graph, would make exactly the claim
    // the endpoint's own schema warns against: a client deriving "newly
    // appeared" from first_seen "can be wrong in both directions".
    // PathGraph's legend carries the same sentence for edges; the rail has
    // to carry it for nodes.
    const node = requireNode(captured.data.unicast, 65001)
    const w = await mountAnswered()
    await w.get(`[data-node="${node.asn}"]`).trigger('click')
    const rail = w.get('[data-rail]').text()
    expect(rail).toMatch(/routes in this answer/i)
    expect(rail).not.toMatch(/\bappeared\b/i)
  })

  it('lists the selected AS edges, the withdrawn one included', async () => {
    // A withdrawn edge is listed in the rail, not hidden, because the
    // collector still KNOWS this adjacency even though the network no
    // longer uses it; a rail that listed only live edges would report
    // the collector's knowledge as absence, and this archive's one
    // withdrawn edge is the case that proves it.
    const graph = captured.data.unicast
    const withdrawn = requireEdge(graph, 65002, 64511, 'withdrawn')
    const live = requireEdge(graph, 65001, 65002, 'live')
    const w = await mountAnswered()
    await w.get(`[data-node="${withdrawn.src}"]`).trigger('click')

    const rows = w.findAll('[data-edge-row]').map((r) => r.attributes('data-edge'))
    // Both directions: AS65002 is the far end of one edge and the near end of
    // the other, and an "edges of this AS" list that only followed src would
    // silently drop half of them.
    expect(rows).toContain(`${withdrawn.src}-${withdrawn.dst}`)
    expect(rows).toContain(`${live.src}-${live.dst}`)

    const row = w.get(`[data-edge-row][data-edge="${withdrawn.src}-${withdrawn.dst}"]`)
    expect(row.text()).toContain(String(withdrawn.live_routes))
    expect(row.text()).toContain(String(withdrawn.routes))
  })

  it('renders no rail field the API does not measure', async () => {
    // The guard both ways: every label names a field the captured response
    // actually carries, and no label claims a metric this pipeline does not
    // measure. Testing found the hole the second half closes --
    // `{ id: 'last_seen', header: 'Uptime' }` clears an id-only check while
    // promising telemetry that does not exist.
    const graph = captured.data.unicast
    const w = await mountAnswered()
    await w.get(`[data-node="${requireNode(graph, 65002).asn}"]`).trigger('click')

    const nodeFields = fieldsOf(w, 'data-node-field')
    const edgeFields = fieldsOf(w, 'data-edge-field')
    // Guards against the whole check passing because the rail rendered no
    // labels at all.
    expect(nodeFields.length).toBeGreaterThan(0)
    expect(edgeFields.length).toBeGreaterThan(0)
    expect(inventedColumns(nodeFields, requireNode(graph, 65002))).toEqual([])
    expect(inventedColumns(edgeFields, requireEdge(graph, 65002, 64511, 'withdrawn'))).toEqual([])
  })

  it('drops a selected AS that the newly chosen family does not contain', async () => {
    // The families carry disjoint ASNs, so a selection made in one is a fact
    // about a graph no longer on screen. Keeping it would leave the rail
    // describing an AS the canvas beside it does not draw -- a fact from
    // another family, presented as this one's.
    answer = { data: threeFamilies, meta: synthMeta }
    const w = await mountAnswered()
    await w.get(`[data-node="${ASNS.unicast[0]}"]`).trigger('click')
    expect(w.get('[data-rail]').text()).toContain(`AS${ASNS.unicast[0]}`)

    await w.get('[data-family="vpn"]').trigger('click')
    expect(w.get('[data-rail]').text()).not.toContain(`AS${ASNS.unicast[0]}`)
  })

  it('prints the response own completeness claim, and never beside a failure', async () => {
    // /v1/topology caps nothing, so `warnings` is empty and this footer reads
    // "complete as of ...". That is a true claim about a bounded question and
    // it is the same footer every other screen prints -- but only over an
    // answer. Beside an error it would contradict the sentence next to it.
    const w = await mountAnswered()
    expect(w.findComponent(ResultMeta).exists()).toBe(true)
    expect(w.findComponent(ResultMeta).props('meta')).toEqual(captured.meta)

    freshState()
    topologyError = new Error('the daemon refused the scope')
    const failed = await mountAnswered()
    expect(failed.findComponent(ResultMeta).exists()).toBe(false)
  })

  it('says it is still loading rather than showing an empty graph', async () => {
    // The third of the states that must never be confusable. "Nothing has
    // landed yet" is not "this scope reaches no AS": an empty-family note
    // shown while the first fetch is in flight would be a positive claim made
    // before the answer existed.
    //
    // `isPending` is deliberately NOT part of the screen's condition, and so
    // not part of this test's setup. A scope is set and nothing failed, so
    // "no answer yet" has exactly one honest rendering whatever the flag
    // says -- and gating on the flag left a fifth state, silence, for the
    // combination where the composable has not yet called itself pending.
    answer = undefined
    const w = await mountAnswered()
    expect(w.text()).toMatch(/loading/i)
    expect(w.findComponent(PathGraph).exists()).toBe(false)
    expect(w.find('[data-empty-family]').exists()).toBe(false)
    expect(w.find('[data-empty-trim]').exists()).toBe(false)
    expect(w.find('[data-summary]').exists()).toBe(false)
  })

  it('renders the daemon own sentence when the request fails, not an empty graph', async () => {
    // An empty graph pane and a failed request must never look the same. The
    // first is "this scope reaches nothing", which is an answer; the second
    // is "we do not know", which is not.
    topologyError = new Error('/v1/topology needs something to scope the graph to')
    const w = await mountAnswered()
    expect(w.get('[role="alert"]').text()).toContain('needs something to scope')
    expect(w.findComponent(PathGraph).exists()).toBe(false)
    expect(w.find('[data-empty-family]').exists()).toBe(false)
  })

  // --- AS holder names: rail only, never the graph nodes (@/lib/asname.ts) ---
  describe('AS holder names', () => {
    it('keeps AS-path graph nodes on the bare ASN and puts the name in the rail', async () => {
      const node = requireNode(captured.data.unicast, 65001)
      asNamesFixture = {
        data: [{ asn: node.asn, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = await mountAnswered()

      // The node's own SVG label, both before and after selection: this is
      // PathGraph.vue's own <text class="asn">, which never renders a name
      // regardless of what the rail does beside it.
      const nodeLabel = () => w.get(`[data-node="${node.asn}"] text.asn`)
      expect(nodeLabel().text()).toBe(`AS${node.asn}`)

      await w.get(`[data-node="${node.asn}"]`).trigger('click')

      const railName = w.get('[data-holder-name]')
      expect(railName.text()).toBe('LEVEL3 - Level 3 Parent, LLC')
      // Selecting the node must not retroactively rewrite the canvas.
      expect(nodeLabel().text()).toBe(`AS${node.asn}`)
    })

    it('renders the bare ASN when the dataset is not loaded, and says why once per graph', async () => {
      const node = requireNode(captured.data.unicast, 65001)
      asNamesFixture = {
        data: [{ asn: node.asn, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: false },
      }
      const w = await mountAnswered()
      await w.get(`[data-node="${node.asn}"]`).trigger('click')

      expect(w.get('[data-rail] h2').text()).toBe(`AS${node.asn}`)
      expect(w.find('[data-holder-name]').exists()).toBe(false)

      const notices = w.findAll('[data-asnames-notice]')
      expect(notices).toHaveLength(1)
      expect(notices[0].text()).toMatch(/not loaded/i)
    })

    it('renders the bare ASN for an ASN the dataset does not list, without claiming the dataset is missing', async () => {
      const node = requireNode(captured.data.unicast, 65001)
      asNamesFixture = {
        data: [{ asn: node.asn, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = await mountAnswered()
      await w.get(`[data-node="${node.asn}"]`).trigger('click')

      expect(w.find('[data-holder-name]').exists()).toBe(false)
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
    })

    it('shows the dataset date beside the names', async () => {
      const node = requireNode(captured.data.unicast, 65001)
      const iso = '2026-09-18T10:49:00Z'
      asNamesFixture = {
        data: [{ asn: node.asn, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true, asnames_published: iso },
      }
      const w = await mountAnswered()

      const note = w.find('[data-asnames-date]')
      expect(note.exists()).toBe(true)
      expect(note.text()).toContain(formatClock(iso))
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
      // Nothing was cut short, so nothing says it was.
      expect(w.find('[data-asnames-truncated]').exists()).toBe(false)
    })

    // A graph wider than one lookup. Past the cap the lowest-numbered ASNs
    // are the ones looked up, so a selected high-numbered node shows a bare
    // ASN that is indistinguishable, in the rail, from a genuinely unlisted
    // one -- while the date line says names are loaded as of a date.
    it('says so when the graph outgrew one lookup, rather than showing the rest as unlisted', async () => {
      const node = requireNode(captured.data.unicast, 65001)
      asNamesTruncated = true
      asNamesFixture = {
        data: [{ asn: node.asn, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: {
          warnings: [],
          total_matched: null,
          asnames_loaded: true,
          asnames_published: '2026-09-18T10:49:00Z',
        },
      }
      const w = await mountAnswered()

      const line = w.findAll('[data-asnames-truncated]')
      expect(line).toHaveLength(1)
      expect(line[0].text()).toMatch(/512/)
      expect(line[0].text()).toMatch(/lowest-numbered/i)
      expect(line[0].text()).not.toMatch(/not loaded/i)
      // A SECOND line beside the date, not a replacement for it.
      expect(w.find('[data-asnames-date]').exists()).toBe(true)
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
    })

    // Truncated and no dataset: "not loaded" is the whole story here, and
    // a second sentence would only compete with the one to act on.
    it('says only that the dataset is missing when it is, even if the batch was also cut', async () => {
      const node = requireNode(captured.data.unicast, 65001)
      asNamesTruncated = true
      asNamesFixture = {
        data: [{ asn: node.asn, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: false },
      }
      const w = await mountAnswered()

      expect(w.findAll('[data-asnames-notice]')).toHaveLength(1)
      expect(w.find('[data-asnames-truncated]').exists()).toBe(false)
    })
  })
})

// The canvas has a measured 500px floor (see the .canvas rule's comment), so
// on a 390px phone the pane is narrower than its content. Without its own
// scroller the graph pushed the whole page 174px sideways; with one it
// scrolls inside the pane, as a wide table does inside its card. jsdom
// applies no component stylesheet, so this reads the rule itself.
it('scrolls a graph wider than the screen inside its own pane', () => {
  expect(declarationsOf('views/TopologyView.vue', '.pane')).toContain('overflow-x: auto')
})
