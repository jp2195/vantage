import { type VueWrapper, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick, reactive, ref } from 'vue'
import routes from '@/api/fixtures/routes.json'
import history from '@/api/fixtures/routes-history.json'
import topology from '@/api/fixtures/topology.json'
import PathGraph from '@/components/PathGraph.vue'
import DataTable from '@/components/DataTable.vue'
import { columnsOf } from '@/test-support/columnsOf'
import DumpStateMark from '@/components/DumpStateMark.vue'
import ResultMeta from '@/components/ResultMeta.vue'
import { inventedColumns } from '@/test-support/columnGuard'
import { sameRef } from '@/test-support/sameRef'
import { disambiguationPair } from '@/test-support/disambiguationRows'
import type { AsEdge, AsNode, Meta, TopologyFanout } from '@/api/generated'
import type { RouteFilters } from '@/api/queries'

// Set inside each test below, never left at these initial values: the
// isolation rule this project enforces requires every test to pass alone via
// `-t`, so each test assigns all four of these itself before mounting
// rather than relying on what a previous test in file order happened to
// leave behind. historyRows in particular carries the isolation risk
// directly: the collector-clock test below is the only one that needs
// to mutate it, but every OTHER test still has to reset it, or running the
// suite in a different order would leak that test's two-event history into
// a test that expects the plain one-event fixture. findRoutesError and
// historyError exist so the error-gating tests can flip one query's error
// on without touching the other's fixture data.
let routesFixture: unknown = routes
let historyRows: unknown[] = history.data
let findRoutesError: unknown = undefined
let historyError: unknown = undefined

// The captured /v1/topology answer, off the wire (fixtures/README.md). Its
// unicast graph is this lab's real one and its vpn and evpn members are the
// empty graphs this lab genuinely produces.
let topologyFixture: { data: TopologyFanout; meta: Meta } | undefined = {
  data: topology.data as TopologyFanout,
  meta: topology.meta as Meta,
}
let topologyError: unknown = undefined

// SYNTHESIZED, for the two cases this lab cannot capture: a VPN or EVPN graph
// with anything in it, and an empty unicast graph. Annotated with the
// generated types, so vue-tsc checks it against the same shapes the real
// response decodes into.
const SYNTH_SEEN = '2026-09-11T22:39:28.659855Z'
const synthNode = (asn: number): AsNode => ({
  asn,
  routes: 1,
  roles: ['origin'],
  first_seen: SYNTH_SEEN,
})
const synthEdge = (src: number, dst: number): AsEdge => ({
  src,
  dst,
  routes: 1,
  live_routes: 1,
  first_seen: SYNTH_SEEN,
})
const synthMeta: Meta = { warnings: [], total_matched: 3, next_cursor: null }

// The three refs the screen hands its composables, captured on the way
// past. Mocking `useFindRoutes` as a function that ignores its argument
// leaves `search()` invoked by nothing: the mode-to-filter mapping is the only
// reason this screen exists, and three separate mutations of it (swapping
// origin_asn for through_asn, ignoring the `covering` checkbox, deleting
// the seq tie-break) all left the suite green. Capturing the refs is what
// lets a test read the request the screen actually composed, rather than
// only the rows a stub handed back.
// A route whose query a test sets BEFORE mounting, mirroring an operator
// opening a shared link.
const route = reactive({ query: {} as Record<string, string> })
const push = vi.fn()
const replace = vi.fn()
vi.mock('vue-router', () => ({
  useRoute: () => route,
  useRouter: () => ({ push, replace }),
}))

let filtersRef: { value: RouteFilters | undefined } | undefined
let historyPrefixRef: { value: string | undefined } | undefined
let sinceRef: { value: string } | undefined
// Captured so a test can prove the Topology tab asks the SAME question the
// Paths tab does, rather than composing a second one of its own.
let topologyScopeRef: { value: RouteFilters | undefined } | undefined

vi.mock('@/api/queries', () => ({
  useFindRoutes: (filters: { value: RouteFilters | undefined }) => {
    filtersRef = filters
    return {
      data: ref(routesFixture),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(findRoutesError),
    }
  },
  useTopology: (scope: { value: RouteFilters | undefined }) => {
    topologyScopeRef = scope
    return {
      data: ref(topologyError ? undefined : topologyFixture),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(topologyError),
    }
  },
  useRouteHistory: (prefix: { value: string | undefined }, since: { value: string }) => {
    historyPrefixRef = prefix
    sinceRef = since
    return {
      data: ref({ data: historyRows, meta: history.meta }),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(historyError),
    }
  },
}))

const LookingGlassView = (await import('./LookingGlassView.vue')).default

// Every test assigns these itself; this resets the captured refs too, so a
// test that never mounts cannot read a previous test's captured ref and
// pass on it.
function freshState() {
  routesFixture = routes
  historyRows = history.data
  findRoutesError = undefined
  historyError = undefined
  filtersRef = undefined
  historyPrefixRef = undefined
  sinceRef = undefined
  topologyFixture = { data: topology.data as TopologyFanout, meta: topology.meta as Meta }
  topologyError = undefined
  topologyScopeRef = undefined
}

// Drives the real form the way an operator does -- click a mode, type a
// term, tick the checkbox that mode offers, submit -- rather than calling
// `search()` directly. The submit handler is the seam under test; reaching
// past the form to the function would stop proving the button is wired to
// it.
const MODE_INDEX = { prefix: 0, asn: 1, community: 2 } as const

async function runSearch(
  w: VueWrapper,
  opts: { mode?: keyof typeof MODE_INDEX; term: string; toggle?: boolean },
) {
  await w.findAll('.modes button')[MODE_INDEX[opts.mode ?? 'prefix']].trigger('click')
  await w.find('.query input.mono').setValue(opts.term)
  if (opts.toggle) await w.find('.query input[type="checkbox"]').setValue(true)
  await w.find('form.query').trigger('submit')
}

// Mounts and asks a real question, for every test whose subject is what
// the screen renders about an ANSWER. The screen deliberately renders
// nothing on the routes tabs until a search has been made -- there is no
// answer to describe -- so a test about rows, groups or a footer has to
// search first, the same way an operator does.
async function mountSearched() {
  const w = mount(LookingGlassView)
  await runSearch(w, { term: routes.data.unicast[0].prefix })
  return w
}

describe('LookingGlassView', () => {
  // The route object and the navigation spies are module-scoped and shared,
  // so they are reset here rather than in each test: a query left behind by
  // one test is exactly the order-dependence this suite's isolation rule
  // exists to prevent, and it is invisible until the tests run in a
  // different order.
  beforeEach(() => {
    route.query = {}
    push.mockClear()
    replace.mockClear()
  })

  // The rail counts distinct ROUTES and the table shows OBSERVATIONS, which
  // is the right split (see distinctRoutes) -- but on a dual-homed router
  // it puts "Paths 1" directly above two rows with nothing on screen
  // reconciling them.
  // Found in a browser 2026-09-21; the word "collector" appeared nowhere in
  // this screen's prose.
  //
  // Conditional, not a standing sentence: on a single-collector deployment
  // there is no apparent contradiction to explain and the note would be
  // noise. That branch is the second half of this test, and without it an
  // unconditional paragraph would pass just as well.
  it('says why one path shows two rows, and only when it actually does', async () => {
    const one = routes.data.unicast[0]
    routesFixture = {
      data: { ...routes.data, unicast: [one, { ...one, collector: 'dev-c2' }] },
      meta: routes.meta,
    }
    historyRows = history.data
    const w = await mountSearched()
    expect(w.findAll('tbody tr')).toHaveLength(2)
    const note = w.find('[data-observers-note]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toMatch(/collector/i)

    // And absent when every row came from one collector.
    routesFixture = routes
    const single = await mountSearched()
    expect(single.find('[data-observers-note]').exists()).toBe(false)
  })

  /**
   * The Grouped tab kept the defect fixed on the paths table on 2026-09-20.
   *
   * That change deduped "Paths" on route identity with the collector
   * removed, because how many ways a prefix is reachable is a question
   * about the NETWORK. The Grouped tab groups raw rows, so its per-group
   * count still counted observers -- and the screen contradicted itself in
   * a browser at /looking-glass?q=10.10.1.0/24&tab=grouped: the rail read
   * PATHS 1 while the group heading beside it read AS 65001 · 2.
   *
   * The two list lines under it were byte-identical --
   * "10.10.1.0/24 in_pre #0 · 172.22.0.8" twice -- with nothing on screen
   * telling them apart, which is the same thing the paths table gained a
   * Collector column for. The list keeps showing both observations; it just
   * has to say whose they are.
   */
  it('counts one grouped path once, and names the collector on each observation', async () => {
    const one = routes.data.unicast[0]
    routesFixture = {
      data: { ...routes.data, unicast: [one, { ...one, collector: 'dev-c2' }] },
      meta: routes.meta,
    }
    historyRows = history.data
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')

    // One route reachable one way, observed twice.
    const heading = w.find('.group h3')
    expect(heading.exists()).toBe(true)
    expect(heading.find('.count').text()).toBe('1')

    // Both observations still listed -- the list shows what was seen -- but
    // now distinguishable. Identical text on both lines is the defect.
    const lines = w.findAll('.group li').map((li) => li.text())
    expect(lines).toHaveLength(2)
    expect(lines[0]).not.toBe(lines[1])
    expect(lines.join(' ')).toContain(one.collector)
    expect(lines.join(' ')).toContain('dev-c2')
  })

  // The sentence has to survive a third collector. It was written as
  // "<list> both monitor this route", which with three reads "dev-c1 and
  // dev-c2 and dev-c3 both monitor" -- ungrammatical and numerically wrong.
  // Nothing about this codebase caps collectors at two: the fan-out on the
  // Peers screen iterates however many there are.
  it('names three collectors without claiming there are two', async () => {
    const one = routes.data.unicast[0]
    routesFixture = {
      data: {
        ...routes.data,
        unicast: [one, { ...one, collector: 'dev-c2' }, { ...one, collector: 'dev-c3' }],
      },
      meta: routes.meta,
    }
    historyRows = history.data
    const w = await mountSearched()
    const note = w.get('[data-observers-note]').text()
    expect(note).not.toMatch(/\bboth\b/)
    expect(note).toContain('dev-c3')
  })

  it('offers exactly the three query modes the API backs', () => {
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = mount(LookingGlassView)
    const text = w.text().toLowerCase()
    expect(text).toContain('prefix')
    expect(text).toContain('asn')
    expect(text).toContain('community')
  })

  it('offers a Topology tab now an endpoint answers it, and still no RPKI or IRR panel', () => {
    // Several placeholder tabs and panels are intentionally not
    // implemented for lack of a data source, and a tab that renders a
    // placeholder implies the data exists and is merely empty. Topology
    // moved off that list here and only here: /v1/topology ships, this
    // screen's own search parameters ARE its scope parameters, and the
    // tab renders a real answer rather than a placeholder.
    //
    // Anchored to the tab bar's own closed set of labels and to element
    // classes, not to whole-page text: once the paths tab renders real route
    // rows (below), scanning w.text() for a bare substring like "roa" would
    // start failing on some unrelated captured community or router name that
    // happens to contain it, for a reason that has nothing to do with
    // whether this screen invented an ROA panel.
    freshState()
    const w = mount(LookingGlassView)
    const tabs = w.findAll('.tabs button').map((b) => b.text().toLowerCase())
    expect(tabs).toEqual(['paths', 'grouped', 'changes', 'topology'])
    for (const invented of ['rpki', 'irr', 'roa']) {
      expect(w.find(`[class*="${invented}"]`).exists()).toBe(false)
    }
  })

  // The risk was never the word "facts"; it is a ROW WITH NOTHING BEHIND
  // IT. RPKI, IRR, first seen and withdrawals-24h are four values this
  // pipeline does not have, so the guard pins the closed set of labels
  // the rail may state, every one of them counted over the rows the
  // answer already carried.
  it('states only facts the answer itself carried, and none the pipeline cannot measure', async () => {
    const w = await mountSearched()
    const labels = w.findAll('[data-facts] dt').map((d) => d.text())
    expect(labels.length).toBeGreaterThan(0)

    const allowed = new Set([
      'Carried in',
      'Paths',
      'Observing peers',
      'Origin AS',
      'Origin ASes',
      'Shortest / longest',
      'Columns searched',
    ])
    for (const l of labels) expect(allowed).toContain(l)

    // The four fields this rail cannot show, named explicitly: an
    // empty row beside "RPKI" reads as "this prefix has no ROA", which is a
    // verdict rather than a gap.
    const railText = w.find('[data-facts]').text().toLowerCase()
    for (const absent of ['rpki', 'irr', 'first seen', 'withdrawals']) {
      expect(railText).not.toContain(absent)
    }
  })

  it('draws the same PathGraph on the Topology tab, for the search already made', async () => {
    // One component, two mount points -- the precedent is the kind marker
    // and reason rendering extracted for Events rather than copied, and the
    // reason is the same: two copies drift. Asserting the COMPONENT rather
    // than some markup it happens to emit is what makes that true here.
    //
    // And the same question: /v1/topology takes this screen's scope
    // parameters exactly, so the tab reuses the filters the search composed.
    // A tab that built its own scope object could answer for a different
    // question than the one on screen beside it.
    freshState()
    const w = await mountSearched()
    await w.get('[data-tab="topology"]').trigger('click')

    const graph = w.findComponent(PathGraph)
    expect(graph.exists()).toBe(true)
    expect(graph.props('graph')).toEqual(topology.data.unicast)

    // The tab forwards the object the search composed rather than building
    // one of its own: a second scope object is a second question that this
    // screen would then have to keep in step with the first, which is the
    // defect this assertion has guarded since before the tab was gated.
    // Compared through sameRef and asserted as a boolean -- see that helper
    // for why handing two refs to toBe can kill the runner outright -- with
    // the readable value comparison beside it.
    expect(sameRef(topologyScopeRef?.value, filtersRef?.value)).toBe(true)
    expect(topologyScopeRef?.value).toEqual(filtersRef?.value)

    // A small graph has to state its size at this mount point too, or an
    // operator cannot tell it from a broken one. Derived from the
    // fixture, not typed in.
    const counts = w.get('[data-topology-counts]').text()
    expect(counts).toContain(`${topology.data.unicast.nodes.length} ASNs`)
    expect(counts).toContain(`${topology.data.unicast.edges.length} edges`)
  })

  it('asks no topology question while the Topology tab is closed', async () => {
    // /v1/topology is a second query over the same scope, and the endpoint's
    // own cost note says the wide scopes (origin_asn=, through_asn=,
    // community=) read the whole table at 237-262ms on a 2M-row rig. Paying
    // that on every search of the most-used screen in the app, for a tab most
    // searches never open, is a cost nobody asked for and nobody measured --
    // and prefetching is a performance claim this project does not get to make
    // without a measurement.
    //
    // Observed as the SCOPE the composable is handed, which is the half this
    // screen controls; that an undefined scope sends no request is
    // useTopology's own contract and is pinned in queries.test.ts ("asks
    // nothing until a scope exists"). Neither test covers it alone.
    freshState()
    const w = await mountSearched()
    expect(filtersRef?.value).toBeDefined()
    expect(topologyScopeRef?.value).toBeUndefined()

    await w.get('[data-tab="topology"]').trigger('click')
    expect(topologyScopeRef?.value).toEqual(filtersRef?.value)
  })

  it('renders no graph on the Topology tab before a search, and no empty-graph claim either', async () => {
    // The same rule the paths and grouped tabs follow. /v1/topology refuses
    // an unscoped request outright, and an empty graph pane rendered before
    // anything was asked would claim this scope reaches no AS at all -- a
    // positive answer to a question nobody put.
    freshState()
    const w = mount(LookingGlassView)
    await w.get('[data-tab="topology"]').trigger('click')
    expect(w.findComponent(PathGraph).exists()).toBe(false)
    expect(w.find('[data-topology-empty]').exists()).toBe(false)
    expect(w.text()).toMatch(/search a prefix, asn or community/i)
  })

  it('says an empty unicast graph is empty rather than drawing a blank canvas', async () => {
    // SYNTHESIZED: this lab's unicast graph is never empty for a scope that
    // matched, so no capture can produce the case. A scope that matches only
    // one-hop paths does produce it for real -- a one-hop AS path contributes
    // a node and no edge, and a scope reaching nothing at all contributes
    // neither -- and a blank 580x460 canvas would read as a screen that
    // broke rather than as an answer.
    freshState()
    topologyFixture = {
      data: {
        unicast: { nodes: [], edges: [] },
        vpn: { nodes: [], edges: [] },
        evpn: { nodes: [], edges: [] },
      },
      meta: synthMeta,
    }
    const w = await mountSearched()
    await w.get('[data-tab="topology"]').trigger('click')
    expect(w.findComponent(PathGraph).exists()).toBe(false)
    const note = w.get('[data-topology-empty]')
    expect(note.attributes('role')).toBeUndefined()
    expect(note.text()).not.toMatch(/error|failed/i)
  })

  it('says when the other two families have a graph this tab does not draw', async () => {
    // The same rule, and the same sentence shape, as this screen's existing
    // fanout note over the routes tabs: this tab renders unicast, so a VPN
    // or EVPN graph the scope reached has to be NAMED rather than silently
    // dropped. It cannot simply be merged in: each family's graph is its
    // own, because an EVPN adjacency and a unicast adjacency are not the
    // same kind of edge.
    freshState()
    topologyFixture = {
      data: {
        unicast: topology.data.unicast as TopologyFanout['unicast'],
        vpn: { nodes: [synthNode(64601), synthNode(64602)], edges: [synthEdge(64601, 64602)] },
        evpn: { nodes: [synthNode(64701)], edges: [] },
      },
      meta: synthMeta,
    }
    const w = await mountSearched()
    await w.get('[data-tab="topology"]').trigger('click')

    const note = w.get('[data-topology-families]').text()
    expect(note).toMatch(/vpn/i)
    expect(note).toMatch(/evpn/i)
    expect(note).toContain('2 ASNs')
    expect(note).toContain('1 edge')
  })

  it('says nothing extra when the other two families reached nothing', async () => {
    // The converse, so the note above cannot be a sentence that is always
    // printed. The captured answer's vpn and evpn arms are both empty, which
    // is the ordinary case in this lab, and a note reading "0 ASNs, 0 edges"
    // beside every graph would be noise that teaches an operator to skip it.
    freshState()
    const w = await mountSearched()
    await w.get('[data-tab="topology"]').trigger('click')
    expect(w.find('[data-topology-families]').exists()).toBe(false)
  })

  it('shows ResultMeta on the Topology tab too, and not when the graph query failed', async () => {
    // Every other tab on this screen prints the response's own completeness
    // claim under its answer, and this one describes a DIFFERENT response --
    // /v1/topology's meta, not /v1/routes'. Dropping it here would say less
    // about that answer than the tab beside it says about its own.
    //
    // Gated on the error for the reason the grouped tab's own footer is:
    // `data` can still hold the last SUCCESSFUL fetch's meta after a later
    // refetch fails, and "complete" printed under that stale meta would
    // contradict the failure on screen beside it.
    freshState()
    const w = await mountSearched()
    await w.get('[data-tab="topology"]').trigger('click')
    expect(w.findComponent(ResultMeta).exists()).toBe(true)

    freshState()
    topologyError = new Error('the daemon refused the scope')
    const failed = await mountSearched()
    await failed.get('[data-tab="topology"]').trigger('click')
    expect(failed.findComponent(ResultMeta).exists()).toBe(false)
    expect(failed.get('.topology [role="alert"]').text()).toContain('refused the scope')
  })

  it('turns a clicked AS into an in-path search rather than doing nothing', async () => {
    // PathGraph emits the ASN of the node clicked, and a tab that dropped
    // that event would ship a graph whose nodes look clickable and are not.
    // through_asn=, not origin_asn=: a node drawn in this graph may be
    // transit-only, and origin_asn= would answer with nothing for exactly
    // the nodes an operator is most likely to be chasing.
    freshState()
    const w = await mountSearched()
    await w.get('[data-tab="topology"]').trigger('click')
    const asn = topology.data.unicast.nodes[0].asn
    await w.get(`[data-node="${asn}"]`).trigger('click')
    expect(filtersRef?.value).toEqual({ through_asn: asn })
  })

  it('opens a shared link on its search, without anyone retyping it', async () => {
    // The whole point of putting the question in the URL: URL sharing
    // carries the full query, which is why server-side saved views are
    // unnecessary here. A link that restores the form but not the answer
    // would leave that reasoning resting on nothing.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    route.query = { q: '10.97.0.0/16', mode: 'prefix' }
    push.mockClear()

    mount(LookingGlassView)
    await nextTick()

    expect(filtersRef?.value).toEqual({ prefix: '10.97.0.0/16' })
    expect(historyPrefixRef?.value).toBe('10.97.0.0/16')
    // Arriving is not asking. Pushing here would put an entry in history for
    // opening the link, so back would return you to the same page.
    expect(push).not.toHaveBeenCalled()
  })

  it('restores the covering flag, not just the term', async () => {
    // covers= and prefix= are different questions against different
    // parameters, and covers= additionally means the Changes tab has no exact
    // prefix to track. A link that dropped the flag would answer a question
    // the sender did not ask and label it as theirs.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    route.query = { q: '10.97.1.5', mode: 'prefix', covers: '1' }

    mount(LookingGlassView)
    await nextTick()

    expect(filtersRef?.value).toEqual({ covers: '10.97.1.5' })
    expect(historyPrefixRef?.value).toBeUndefined()
  })

  it('pushes a submitted question and replaces a mere view change', async () => {
    // push for a search, so back steps between questions an operator actually
    // asked. replace for a tab, because switching tabs is not a new question
    // and a history entry per click would make back useless for the thing it
    // is good at.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    route.query = {}
    push.mockClear()
    replace.mockClear()

    const w = mount(LookingGlassView)
    await runSearch(w, { mode: 'asn', term: '65001' })

    expect(push).toHaveBeenCalledTimes(1)
    expect(push.mock.calls[0][0].query).toMatchObject({ q: '65001', mode: 'asn' })

    push.mockClear()
    await w.find('[data-tab="grouped"]').trigger('click')

    expect(push).not.toHaveBeenCalled()
    expect(replace).toHaveBeenCalled()
    expect(replace.mock.calls.at(-1)![0].query).toMatchObject({ tab: 'grouped' })
  })

  it('follows the URL when the back button changes it', async () => {
    // Vue Router reuses this component across a query-only navigation on the
    // same route -- there is no second setup() to re-read the query in. A
    // restore that only runs at setup leaves the address bar saying one thing
    // while the screen shows another, which is a worse failure than having no
    // URL state at all: the URL becomes a claim the page contradicts.
    // PeerDetailView.vue's routerIp hit this same instance-reuse fact.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    route.query = { q: '10.97.0.0/16', mode: 'prefix' }

    mount(LookingGlassView)
    await nextTick()
    expect(filtersRef?.value).toEqual({ prefix: '10.97.0.0/16' })

    // What the back button does: same route, different query, no remount.
    route.query = { q: '10.255.0.3', mode: 'prefix', tab: 'grouped' }
    await nextTick()
    await nextTick()

    expect(filtersRef?.value).toEqual({ prefix: '10.255.0.3' })
  })

  it('orders history on the collector clock, not the router clock', async () => {
    // The two clocks disagree in real data. sink's
    // TestLookingGlassResolvesLatestOnTheCollectorClock pins this server-side;
    // this is the same rule at the last inch. Sorting on ts_router would make
    // the UI confidently wrong about which observation is latest.
    //
    // SYNTHESIZED INPUT, and it has to be. routes-history.json holds a single
    // event, so no captured fixture can order anything. This builds two events
    // from that real row whose clocks DISAGREE about which is later -- the
    // exact situation the server-side test guards. To capture this instead,
    // a lab deployment would need a prefix flapped across a router/collector
    // clock skew.
    routesFixture = routes
    const real = history.data[0]
    const older = { ...real, seq: '1', ts_collector: '2026-09-06T10:00:00Z', ts_router: '2026-09-06T20:00:00Z' }
    const newer = { ...real, seq: '2', ts_collector: '2026-09-06T11:00:00Z', ts_router: '2026-09-06T09:00:00Z' }
    // Deliberately supplied oldest-first, so a component that does not sort
    // at all would also fail rather than pass by luck of input order.
    historyRows = [older, newer]
    findRoutesError = undefined
    historyError = undefined

    const w = await mountSearched()
    await w.find('[data-tab="diff"]').trigger('click')
    const ts = w.findAll('[data-history-row]').map((r) => r.attributes('data-ts'))

    // Newest-first ON THE COLLECTOR CLOCK. Under ts_router the order inverts,
    // so this assertion fails for a component that sorts on the wrong field.
    expect(ts).toEqual(['2026-09-06T11:00:00Z', '2026-09-06T10:00:00Z'])
  })

  it('says the grouped flap summary is absent rather than showing zero', async () => {
    // Scoped to the grouped tab's own region rather than the whole page --
    // the same anchoring fix as the topology/RPKI test above.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    const groupsText = w.get('.groups').text()
    expect(groupsText).toMatch(/flap/i)
    expect(groupsText).not.toMatch(/0 flaps/i)
  })

  it('renders the real routes a search answers with, not just the empty case', async () => {
    // No earlier test in this suite rendered a route row -- the Looking
    // glass exists to answer a query WITH routes, and a suite that never
    // asserts one appears cannot catch a fanout-vs-array mismatch.
    // routes.json is a real /v1/routes?covers=10.97.1.5 capture: 6 unicast
    // rows across two real prefixes, 10.97.0.0/16 and 10.97.1.0/24.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    const rows = w.findAll('tbody tr')
    expect(rows).toHaveLength(routes.data.unicast.length)
    expect(w.text()).toContain('10.97.0.0/16')
    expect(w.text()).toContain('10.97.1.0/24')
    // The AS path cell joins the real captured path, not a stringified array.
    expect(w.text()).toContain('4200000002 65002 65000')
  })

  // routes.json holds both sides, captured: three rows from 10.0.103.73,
  // which sent no sysName, and rows from named routers. This table has no
  // router address column, so an empty sysname rendered raw is a blank
  // identity cell with nothing beside it to say which router it was.
  it('names a router with no sysname by its address, and a named one by its name', async () => {
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    const routerCol = columnsOf(w).findIndex((c) => c.id === 'router_sysname')
    expect(routerCol).toBeGreaterThanOrEqual(0)
    const cells = w.findAll('tbody tr').map((r) => r.findAll('td')[routerCol].text())
    const unnamed = routes.data.unicast.filter((r) => r.router_sysname === '')
    const named = routes.data.unicast.filter((r) => r.router_sysname !== '')
    expect(unnamed.length).toBeGreaterThan(0) // guard: the capture has both sides
    expect(named.length).toBeGreaterThan(0)
    routes.data.unicast.forEach((r, i) => {
      expect(cells[i]).toBe(r.router_sysname === '' ? r.router_ip : r.router_sysname)
    })
  })

  it('renders no column the API does not measure', async () => {
    // The same closed-set guard RoutersView.test.ts, PeersView.test.ts and
    // RoutesView.test.ts already pin, run against a real row from the
    // fixture this screen actually consumes.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    const columns = columnsOf(w)
    expect(inventedColumns(columns, routes.data.unicast[0])).toEqual([])
  })

  it('renders rib and path_id, so a real add-path pair is not shown as one row repeated', async () => {
    // REAL, not synthesized: routes.json's own first two unicast rows
    // (prefix 10.97.0.0/16, router 10.0.103.73, peer 10.0.0.80, rib in_pre)
    // are identical in every field the table renders except path_id (6 vs
    // 7) -- a genuine captured add-path pair. Isolated to just these two
    // rows so the assertion is unambiguous: if dropping the path_id column
    // ever collapses them, this fails.
    const [a, b] = [routes.data.unicast[0], routes.data.unicast[1]]
    routesFixture = { data: { unicast: [a, b], vpn: [], evpn: [] }, meta: routes.meta }
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    const rowTexts = w.findAll('tbody tr').map((r) => r.text())
    expect(rowTexts).toHaveLength(2)
    expect(new Set(rowTexts).size).toBe(rowTexts.length)
  })

  it('renders rib, so two rows differing only in rib are not shown as one', async () => {
    // SYNTHESIZED: no two rows in routes.json differ by rib alone (the one
    // in_post row also carries a different prefix), so this is built from a
    // real captured row with only `rib` changed, via the same
    // disambiguationPair helper RoutesView.test.ts uses for the same
    // property -- one construction, not two that could drift apart on what
    // "differs only in rib" means.
    const [a, b] = disambiguationPair(routes.data.unicast[0], 'rib', 'loc_rib')
    routesFixture = { data: { unicast: [a, b], vpn: [], evpn: [] }, meta: routes.meta }
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    const rowTexts = w.findAll('tbody tr').map((r) => r.text())
    expect(rowTexts).toHaveLength(2)
    expect(new Set(rowTexts).size).toBe(rowTexts.length)
  })

  it('groups routes by upstream ASN rather than listing them flat', async () => {
    // Real data, no synthesis: routes.json's 6 unicast rows fall into three
    // real upstream groups -- 65002 (four rows sharing as_path
    // [...,65002,65000]), 65001 (10.97.1.0/24's own path), and "direct" (the
    // one row with an empty as_path, an ordinary iBGP/redistributed case).
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    const groups = w.findAll('.group')
    expect(groups).toHaveLength(3)
    const totalRows = groups.reduce((n, g) => n + g.findAll('li').length, 0)
    expect(totalRows).toBe(routes.data.unicast.length)
  })

  it('does not render two different routes as the same line in the grouped tab', async () => {
    // Real regression, not a hypothetical: routes.json's rows[1] and rows[4]
    // both carry (prefix 10.97.0.0/16, path_id 7) -- the SAME (prefix,
    // path_id) legitimately reappears here, once per router, because a
    // (prefix, path_id) pair is unique to (router, peer, rib), not to the
    // prefix alone. A grouped-tab :key of `prefix + path_id` collides on
    // exactly this real pair -- a Vue "duplicate key" warning in a
    // browser, though this jsdom/vitest setup
    // does not surface that warning even for a plain reproduction outside
    // this component, so a spy on it would pass vacuously here regardless
    // of the bug. The line each route renders is the part this test can
    // actually pin: two different real routes must not read as one.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    const lineTexts = w.findAll('.group li').map((li) => li.text())
    expect(lineTexts).toHaveLength(routes.data.unicast.length)
    expect(new Set(lineTexts).size).toBe(lineTexts.length)
  })

  it('says when the fanout matched routes this screen does not render', async () => {
    // This is the defect a hand-written fixture would have hidden: anyone
    // writing a /v1/routes fixture by hand would have written vpn: [] and
    // evpn: [], because that is what everyone would write, and the note
    // below would have shipped untested. routes.json is real: vpn has 1
    // row, evpn has 0, and meta.warnings is empty -- so ResultMeta's own
    // "complete" claim (warnings.length === 0) is satisfied on the paths
    // tab while a real matched route sits in the vpn arm, unrendered and
    // unmentioned. The note must name the real count and must not read as
    // an error or as routes missing from the network.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    const note = w.get('.scope-note').text()
    expect(note).toContain('1')
    expect(note).toMatch(/vpn\/evpn/i)
    // The reassurance matters as much as the count: this must say nothing
    // IS missing from the network, not merely mention the phrase in a way
    // that could equally read as the routes having vanished from the fleet.
    expect(note).toMatch(/nothing is missing from the network/i)
  })

  it('says nothing extra when every matched route is already shown', async () => {
    // The converse of the test above: a fanout whose vpn and evpn arms are
    // both genuinely empty must not print a note about routes it isn't
    // dropping. Built from routes.json's own unicast rows with vpn/evpn
    // zeroed out, the same shape the two disambiguation tests above
    // already construct.
    routesFixture = {
      data: { unicast: routes.data.unicast, vpn: [], evpn: [] },
      meta: routes.meta,
    }
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    expect(w.find('.scope-note').exists()).toBe(false)
  })

  it('shows ResultMeta on the grouped tab too, not only paths', async () => {
    // The grouped tab renders the identical `unicast` rows the paths tab
    // does, from the identical query -- an operator who only ever opens
    // Grouped deserves the same completeness claim Paths already gets from
    // DataTable, not silence.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    expect(w.findComponent(ResultMeta).exists()).toBe(true)
    expect(w.get('.groups').text()).toMatch(/complete as of/i)
  })

  it('does not claim the grouped tab is complete when the query failed', async () => {
    // Mirrors DataTable's own guard: `data` can still hold the last
    // SUCCESSFUL fetch's meta after a later refetch fails, so gating on
    // meta alone would print "complete as of ..." under a tab that has no
    // other way to say the current fetch is broken.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = new Error('boom')
    historyError = undefined
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    expect(w.findComponent(ResultMeta).exists()).toBe(false)
    // And it says so, rather than going silent: this tab does not render
    // through DataTable, so nothing else on it would mention the failure.
    expect(w.get('.groups').text()).toContain('boom')
  })

  it('shows ResultMeta on the changes tab once there is a real event list', async () => {
    // A truncated history reading as complete is the same failure DataTable
    // guards against for routes, in a different costume -- an operator on
    // the Changes tab deserves the same signal.
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = undefined
    const w = await mountSearched()
    await w.find('[data-tab="diff"]').trigger('click')
    expect(w.findComponent(ResultMeta).exists()).toBe(true)
    expect(w.get('.diff').text()).toMatch(/complete as of/i)
  })

  it('does not claim the changes tab is complete when the history query failed', async () => {
    routesFixture = routes
    historyRows = history.data
    findRoutesError = undefined
    historyError = new Error('boom')
    const w = await mountSearched()
    await w.find('[data-tab="diff"]').trigger('click')
    expect(w.findComponent(ResultMeta).exists()).toBe(false)
  })
  // --- The five paths through search(), the screen's whole reason to exist ---
  //
  // These paths need real exercising: a module mock that swallows the
  // filters ref would let swapping origin_asn for through_asn, ignoring
  // the `covering` checkbox, or deleting the seq tie-break each pass
  // unnoticed. The terms below are real values off routes.json rather
  // than invented ones, so a mapping test cannot pass against a prefix or
  // community this capture never carried.

  it('sends a prefix search as prefix=, and tracks that prefix on the changes tab', async () => {
    freshState()
    const w = mount(LookingGlassView)
    const captured = routes.data.unicast[0].prefix
    await runSearch(w, { mode: 'prefix', term: captured })
    expect(filtersRef?.value).toEqual({ prefix: captured })
    expect(historyPrefixRef?.value).toBe(captured)
  })

  it('sends a covering search as covers=, and tracks no prefix for it', async () => {
    // Two claims: covers= takes an ADDRESS and answers with every prefix
    // that contains it;
    // /v1/routes/history takes an exact prefix. Handing the covering address
    // to the history query asks about a prefix nobody searched for, and the
    // Changes tab would then answer confidently about the wrong thing.
    freshState()
    const w = mount(LookingGlassView)
    await runSearch(w, { mode: 'prefix', term: '10.97.1.5', toggle: true })
    expect(filtersRef?.value).toEqual({ covers: '10.97.1.5' })
    expect(historyPrefixRef?.value).toBeUndefined()
  })

  it('sends an ASN search as origin_asn=, not through_asn=', async () => {
    freshState()
    const w = mount(LookingGlassView)
    const asn = routes.data.unicast[0].origin_asn as number
    await runSearch(w, { mode: 'asn', term: String(asn) })
    expect(filtersRef?.value).toEqual({ origin_asn: asn })
    // The two parameters answer different questions -- "originated by" and
    // "transited by" -- so a swap is silently wrong rather than loudly
    // broken, which is why this asserts the absent one too.
    expect(historyPrefixRef?.value).toBeUndefined()
  })

  it('sends an in-path ASN search as through_asn=, not origin_asn=', async () => {
    freshState()
    const w = mount(LookingGlassView)
    // A real transit AS from the captured path [4200000002, 65002, 65000]:
    // 65002 appears in the path without originating the route, which is
    // exactly what through_asn= asks about.
    const transit = routes.data.unicast[0].as_path[1]
    await runSearch(w, { mode: 'asn', term: String(transit), toggle: true })
    expect(filtersRef?.value).toEqual({ through_asn: transit })
  })

  it('sends a community search as community=', async () => {
    freshState()
    const w = mount(LookingGlassView)
    const community = routes.data.unicast[0].communities[0]
    await runSearch(w, { mode: 'community', term: community })
    expect(filtersRef?.value).toEqual({ community })
    expect(historyPrefixRef?.value).toBeUndefined()
  })

  it('asks nothing at all until a term is entered', async () => {
    // An empty term must leave `filters` unset rather than sending a search
    // for the empty string: /v1/routes with no narrowing parameter is a
    // different, far larger question than the operator asked.
    freshState()
    const w = mount(LookingGlassView)
    await w.find('form.query').trigger('submit')
    expect(filtersRef?.value).toBeUndefined()
  })

  it('breaks a collector-clock tie on seq as a number, not as text', async () => {
    // seq is a u64 rendered as a decimal string (api/openapi.yaml calls it
    // "the authoritative order"), so `a.seq < b.seq` on the raw strings
    // compares "10" against "9" character by character and puts the OLDER
    // event first. Two events from one session can share a ts_collector --
    // the collector stamps a whole BMP message, and a single update carries
    // several NLRI -- which is exactly when seq is the only thing left to
    // order by.
    //
    // SYNTHESIZED, and it has to be: routes-history.json holds a single
    // event, so no captured fixture can tie anything. Both events below are
    // that real captured row with only seq and ts_collector changed.
    // Capturing this instead would need a prefix announced and withdrawn
    // inside one collector timestamp.
    freshState()
    const real = history.data[0]
    const tie = '2026-09-06T19:14:29.787271Z'
    // Supplied lowest-seq-first, so a component that does not tie-break at
    // all fails here rather than passing on input order.
    historyRows = [
      { ...real, seq: '9', ts_collector: tie },
      { ...real, seq: '10', ts_collector: tie },
    ]
    const w = mount(LookingGlassView)
    await runSearch(w, { mode: 'prefix', term: real.prefix })
    await w.find('[data-tab="diff"]').trigger('click')
    expect(w.findAll('[data-history-row]').map((r) => r.attributes('data-seq'))).toEqual([
      '10',
      '9',
    ])
  })
  // --- The screen must not answer a question nobody asked ---

  it('renders no answer on the paths tab before a search', async () => {
    // Reproduced live on first paint: useFindRoutes manufactured
    // `{data: {unicast: [], vpn: [], evpn: []}, meta: {warnings: [],
    // total_matched: null}}` for the no-filters case, and the paths tab
    // rendered it -- "no rows matched" over a footer reading "complete as
    // of HH:MM:SS", about a request that was never sent. RoutesView solves
    // the identical problem with `v-if="!scope"`.
    //
    // `routesFixture` is undefined here because that is what the fixed
    // composable now returns before any filters exist. The gate still has
    // to be in the template: an undefined `data` would otherwise reach
    // DataTable as zero rows, which renders "no rows matched" all the same.
    freshState()
    routesFixture = undefined
    const w = mount(LookingGlassView)
    expect(w.findComponent(DataTable).exists()).toBe(false)
    expect(w.findComponent(ResultMeta).exists()).toBe(false)
    expect(w.text()).not.toMatch(/no rows matched/i)
    expect(w.text()).not.toMatch(/complete as of/i)
    expect(w.text()).toMatch(/search a prefix, asn or community/i)
  })

  it('renders no answer on the grouped tab before a search', async () => {
    // The same gate, on the tab that had no equivalent at all: grouped
    // rendered zero groups in silence with a "complete as of" footer
    // underneath.
    freshState()
    routesFixture = undefined
    const w = mount(LookingGlassView)
    await w.find('[data-tab="grouped"]').trigger('click')
    expect(w.findComponent(ResultMeta).exists()).toBe(false)
    expect(w.findAll('.group')).toHaveLength(0)
    expect(w.text()).toMatch(/search a prefix, asn or community/i)
  })

  it('says no rows matched on the grouped tab when a real search finds none', async () => {
    // Post-search, this is a real state: a prefix the archive has
    // never carried answers with three empty arms. Grouped rendered zero groups
    // silently, with ResultMeta's "complete as of" beneath -- an empty
    // answer and an unrendered one looking identical, which is the whole
    // failure this project audits for.
    //
    // SYNTHESIZED: routes.json's own meta with the unicast arm emptied and
    // total_matched dropped to 0. No captured /v1/routes response has
    // every arm empty -- covers=10.97.1.5 was chosen precisely because it
    // matched -- so capturing this would mean querying a lab deployment
    // for a prefix it has never seen.
    freshState()
    routesFixture = {
      data: { unicast: [], vpn: [], evpn: [] },
      meta: { ...routes.meta, total_matched: 0 },
    }
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    expect(w.get('.groups').text()).toMatch(/no rows matched/i)
    // And the footer stays, because the query SUCCEEDED and found nothing:
    // that is a complete answer, and the paths tab prints "complete as of
    // ..." under its own "no rows matched" for the identical response. Two
    // tabs saying different amounts about one answer is the inconsistency
    // this tab already had in another form -- caught by opening both in a
    // browser against the live archive.
    expect(w.findComponent(ResultMeta).exists()).toBe(true)
    expect(w.get('.groups').text()).toMatch(/complete as of/i)
  })

  it('does not read an empty grouped tab as "none" when the query failed', async () => {
    // The companion the branch above needs. A failed first fetch leaves
    // `data` undefined and `unicast` empty, and "no rows matched" over that
    // says "there are none" about an answer nobody has. DataTable draws
    // this distinction for the paths tab; the grouped tab has to draw it
    // itself, because it does not go through DataTable.
    freshState()
    routesFixture = undefined
    findRoutesError = new Error('boom')
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    expect(w.get('.groups').text()).not.toMatch(/no rows matched/i)
    expect(w.get('.groups').text()).toContain('boom')
  })
  // --- The Changes tab's window, which used to be silent ---

  it('sends the history window explicitly rather than inheriting the default', async () => {
    // /v1/routes/history defaults ?since= to 1h (api/openapi.yaml), a
    // number the operator never chose and the screen never mentioned. A UI
    // that displays an answer has to own the bound that produced it.
    freshState()
    const w = mount(LookingGlassView)
    await runSearch(w, { term: history.data[0].prefix })
    expect(sinceRef?.value).toBe('1h')
  })

  it('names the window its changes list covers', async () => {
    // ResultMeta's "complete as of HH:MM:SS" is a claim about a window it
    // has no way to name. Complete over WHAT interval is the half that
    // makes it true, and it belongs beside the list.
    freshState()
    const w = mount(LookingGlassView)
    await runSearch(w, { term: history.data[0].prefix })
    await w.find('[data-tab="diff"]').trigger('click')
    // Read off the caption, not off `.diff` as a whole: the window PICKER
    // sits in the same box and its option labels contain the same words, so
    // a whole-tab text match would pass on the control alone and say
    // nothing about whether the list is captioned. Caught by mutation --
    // deleting the caption left a `.diff` assertion green.
    expect(w.get('[data-history-scope]').text()).toMatch(/last hour/i)
    expect(w.get('[data-history-scope]').text()).toContain(history.data[0].prefix)
  })

  it('sends the window the operator picked, and renames it on screen', async () => {
    freshState()
    const w = mount(LookingGlassView)
    await runSearch(w, { term: history.data[0].prefix })
    await w.find('[data-tab="diff"]').trigger('click')
    await w.find('[data-history-window]').setValue('24h')
    expect(sinceRef?.value).toBe('24h')
    expect(w.get('[data-history-scope]').text()).toMatch(/last 24 hours/i)
  })

  it('offers only windows the daemon can actually parse', async () => {
    // api/handlers.go's since() tries RFC 3339 first and then falls through
    // to Go's time.ParseDuration, which knows ns/us/ms/s/m/h and NO day
    // unit: "7d" is a 400, not a week. A picker offering one would turn a
    // convenience into an error the operator cannot explain.
    freshState()
    const w = mount(LookingGlassView)
    await w.find('[data-tab="diff"]').trigger('click')
    const values = w
      .findAll('[data-history-window] option')
      .map((o) => (o.element as HTMLOptionElement).value)
    expect(values.length).toBeGreaterThan(1)
    for (const v of values) expect(v).toMatch(/^\d+(ns|us|ms|s|m|h)$/)
  })

  it('does not use one sentence for "nothing searched" and "nothing changed"', async () => {
    // Three distinct states shared one string: nothing searched; searched
    // and quiet within the window; searched with no history at all. An
    // operator reading "Changes are per prefix. Search a prefix to see
    // them." after searching a prefix learns nothing about their prefix.
    freshState()
    historyRows = []
    const w = mount(LookingGlassView)
    await w.find('[data-tab="diff"]').trigger('click')
    const beforeSearch = w.get('.diff').text()
    expect(beforeSearch).toMatch(/search a prefix/i)

    await runSearch(w, { term: history.data[0].prefix })
    const afterSearch = w.get('.diff').text()
    expect(afterSearch).not.toBe(beforeSearch)
    expect(afterSearch).toMatch(/no changes/i)
    expect(afterSearch).toContain(history.data[0].prefix)
    // The empty case names the window too, for the same reason the
    // populated one does: "nothing changed" is only meaningful over an
    // interval, and this is the interval it is true of.
    expect(afterSearch).toMatch(/last hour/i)
  })

  it('says a covering or ASN search has no prefix to track, rather than "search a prefix"', async () => {
    // The operator DID search. Repeating "search a prefix" at someone who
    // just searched a covering address reads as a broken screen; the real
    // answer is that history takes one exact prefix and this search names
    // none.
    freshState()
    const w = mount(LookingGlassView)
    await runSearch(w, { mode: 'asn', term: String(routes.data.unicast[0].origin_asn) })
    await w.find('[data-tab="diff"]').trigger('click')
    expect(w.get('.diff').text()).toMatch(/exact prefix/i)
  })

  it('does not render two peers\' events as the same line', async () => {
    // seq is per-(peer, session) -- api/openapi.yaml says so outright --
    // so it is NOT unique across a prefix's history, and the list keyed on
    // it alone while rendering a line that named neither router nor peer.
    // Two real events from different peers therefore rendered as the same
    // line under the same key: a cardinality defect this test guards
    // against.
    //
    // SYNTHESIZED: routes-history.json holds one event, so this is that
    // captured row duplicated under a second (router, peer) keeping its
    // real seq -- which is exactly what a second peer's own sequence would
    // legitimately look like. Capturing it would need a lab deployment to
    // carry two peers announcing one prefix.
    //
    // The LINE is what this pins, not the :key, and that is a limit of the
    // environment rather than a choice -- the same limit the grouped tab's
    // own duplicate-line test above records. Checked directly while writing
    // this: Vue raises no duplicate-key warning here, on mount or on a
    // subsequent patch, and its keyed diff still renders the correct text
    // for a list with colliding keys, so nothing observable in jsdom
    // distinguishes `:key="e.seq"` from `:key="historyKey(e)"`. Two
    // different events reading as one line is the half an operator sees,
    // and it is the half a test can hold.
    freshState()
    const real = history.data[0]
    historyRows = [real, { ...real, router_ip: '10.0.0.81', peer_ip: '172.31.0.91' }]
    const w = mount(LookingGlassView)
    await runSearch(w, { term: real.prefix })
    await w.find('[data-tab="diff"]').trigger('click')
    const lines = w.findAll('[data-history-row]').map((r) => r.text())
    expect(lines).toHaveLength(2)
    expect(new Set(lines).size).toBe(lines.length)
  })
  // --- dump_state, on the screen that rendered every row as settled fact ---

  it('marks the one captured row whose dump state is unknown, and only that one', async () => {
    // REAL, not synthesized. routes.json's six unicast rows carry five
    // `complete` and one `unknown` (unicast[3], router 172.22.0.201) -- the
    // premise that "no capture exercises unknown" is false, and this test
    // asserts the fixture's own composition rather than trusting it.
    //
    // Rows from a still-dumping session need a provisional marker; this
    // screen rendered the identical UnicastRoute shape RoutesView marks,
    // with nothing at all. And a `=== 'dumping'` test
    // would still mark nothing here, because none of these rows is
    // dumping -- the unknown one would read as settled.
    freshState()
    const states = routes.data.unicast.map((r) => r.dump_state)
    expect(states.filter((s) => s === 'unknown')).toHaveLength(1)
    expect(states.filter((s) => s === 'complete')).toHaveLength(5)

    const w = await mountSearched()
    const marks = w.findAllComponents(DumpStateMark).filter((m) => m.text() !== '')
    expect(marks).toHaveLength(1)
    expect(marks[0].text()).toMatch(/unknown/i)
  })

  it('carries the same marker onto the grouped tab', async () => {
    // The grouped tab lists the same rows from the same query. An operator
    // who only ever opens Grouped would otherwise read a row the pipeline
    // cannot vouch for as one it can.
    freshState()
    const w = await mountSearched()
    await w.find('[data-tab="grouped"]').trigger('click')
    const marks = w.findAllComponents(DumpStateMark).filter((m) => m.text() !== '')
    expect(marks).toHaveLength(1)
    expect(marks[0].text()).toMatch(/unknown/i)
  })
})

describe('LookingGlassView, collector axis', () => {
  // /v1/routes is one row per (collector, router, peer, rib, prefix,
  // path_id), so a router two collectors monitor returns each of its routes
  // TWICE. Found in a browser on 2026-09-20 against a lab deployment: the
  // screen read "PATHS 2" for one path, with two rows identical in every
  // rendered column.
  //
  // `Paths` is a NETWORK fact -- how many ways this prefix is reachable --
  // so it must not scale with how many collectors happened to be watching.
  // `Observing peers` was already right, because it counts distinct
  // (router, peer) and carries no collector; this is the same correction
  // applied to the counts beside it.
  function twoCollectorFanout() {
    const base = routes.data.unicast[0]
    return {
      data: {
        ...routes.data,
        unicast: [
          { ...base, collector: 'coll-a' },
          { ...base, collector: 'coll-b' },
        ],
      },
      meta: routes.meta,
    }
  }

  it('counts one path once when two collectors both saw it', async () => {
    routesFixture = twoCollectorFanout()
    const w = await mountSearched()
    const text = w.text()

    // The tab row's count and the rail's fact are the same claim and must
    // agree; asserting only one would let the other drift.
    expect(
      text,
      'two collectors watching one peer is one path, not two -- "Paths 2" ' +
        'here reports observers as reachability',
    ).not.toMatch(/Paths\s*2(?!\d)/)
    expect(text).toMatch(/Paths\s*1(?!\d)/)
  })

  it('still reports one observing peer, which was already correct', async () => {
    routesFixture = twoCollectorFanout()
    const w = await mountSearched()
    expect(w.text()).toMatch(/Observing peers\s*1(?!\d)/)
  })

  it('names the collector on each row, so the pair is not two mystery paths', async () => {
    routesFixture = twoCollectorFanout()
    const w = await mountSearched()
    const headers = w.findAll('th').map((h) => h.text().toLowerCase())
    expect(
      headers.some((h) => h.includes('collector')),
      `the paths table renders ${headers.join(', ')}; with no collector column ` +
        'one route seen twice is two identical rows with nothing to tell them apart',
    ).toBe(true)
  })
})
