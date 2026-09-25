import { mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { computed, nextTick, reactive, ref } from 'vue'
import { endpoint, link, node, prefix } from '@/test-support/lsBuilders'
import { inventedColumns } from '@/test-support/columnGuard'
import linkState from '@/api/fixtures/link-state.json'
import type { LsCommon, LsLink, LsNode, LsPrefix, Meta, Router } from '@/api/generated'
import type { LinkStateScope } from '@/api/queries'

// "Rule N" below means the link-state rule of that number, as written out
// in LinkStateGraph.vue's doc comment.
//
// Most rows below are still SYNTHESIZED, not the captured fixture -- and for
// a reason recorded in ui/src/api/fixtures/README.md rather than a blocked
// wait: this suite needs the router the screen ASKS for and the observer on
// the link-state rows to be the same router, and the mocked `useRouters`
// list below is deliberately independent of the router-scoped mock ls rows
// (see NX_P3/XR_RR1/UNNAMED below), a pairing the real capture cannot supply
// for every test without also rewriting how this suite mocks the router
// picker. The builders in @/test-support/lsBuilders are typed by the
// generated types, so `vue-tsc` checks every object here against the same
// shape a real response decodes into -- but every claim about the RESPONSE
// (key presence, `[]` versus `null`, the envelope, `meta`'s shape) belongs to
// the captured fixture, imported below as `linkState`, and is made from it
// rather than from a builder wherever a test is genuinely about the wire.
//
// The synthesized shapes still stand in for measured
// ones: nx-p3 sees 7 nodes and 20 distinct adjacencies. xr-rr1 does NOT see
// "1 node and no adjacency" as an earlier measurement here claimed --
// corrected 2026-09-17, through the shipped path rather than the table: it
// returns NOTHING at all through any of the three /v1/ls endpoints, the same
// as xr-p1, because the nodes/links/prefixes statements INNER JOIN the
// current session and `peer_up.state = 'up'`, and neither router qualifies.
// XR_RR1 below stands in for that shape -- a router with no link-state rows
// at all -- which is a hypothetical no capture can supply since there is
// nothing to capture.

/**
 * The routers the picker offers, synthesized rather than taken from
 * `api/fixtures/routers.json`.
 *
 * This suite needs the router the screen ASKS for and the observer on the
 * link-state rows to be the same router, which no pairing of captures can
 * currently give without also rewriting how this suite mocks the router
 * picker: routers.json is the dev stack's own router (10.0.0.80), and the
 * link-state builders carry the archive's nx-p3 as a synthesized router_ip
 * (10.0.0.11) chosen to match them, not the real captured one (10.0.103.74).
 * Every claim this file makes is about the screen, not about /v1/routers,
 * whose captured response other suites already test.
 *
 * Two entries, because the empty view needs a router with no link-state
 * rows behind it -- xr-rr1 stands in for that shape. Measured on 2026-09-17
 * through the shipped path rather than the table, xr-rr1 returns NOTHING
 * at all through any of the three /v1/ls endpoints
 * (see the file banner above), so "no rows in this answer at all" is not a
 * stand-in for a thinner view, it is exactly what this router's real answer
 * looks like. A router that does not advertise BGP-LS at all reads the
 * same way from here.
 */
const NX_P3 = '10.0.0.11'
const XR_RR1 = '10.0.0.14'
// A BMP session whose initiation carried no sysname. Real, and the shape
// LinkStateGraph's own observer caption already handles.
const UNNAMED = '10.0.0.15'

const routers: { data: Router[]; meta: Meta } = {
  data: [
    {
      sysname: 'nx-p3',
      ip: NX_P3,
      collector: 'dev-c1',
      session_id: '1788722065877779361',
      peers_up: 2,
      peers_down: 0,
      peers_view_lost: 0,
      peers_stale: 0,
      last_seen: '2026-09-16T10:14:28.788894Z',
    },
    {
      sysname: 'xr-rr1',
      ip: XR_RR1,
      collector: 'dev-c1',
      session_id: '1788722065877779362',
      peers_up: 1,
      peers_down: 0,
      peers_view_lost: 0,
      peers_stale: 0,
      last_seen: '2026-09-16T10:14:28.788894Z',
    },
    {
      sysname: '',
      ip: UNNAMED,
      collector: 'dev-c1',
      session_id: '1788722065877779363',
      peers_up: 1,
      peers_down: 0,
      peers_view_lost: 0,
      peers_stale: 0,
      last_seen: '2026-09-16T10:14:28.788894Z',
    },
  ],
  meta: { warnings: [], total_matched: null, next_cursor: null },
}

/** Two nodes and the reciprocal pair between them: the smallest real view. */
const DEFAULT_NODES = [node('1', '10.255.0.1'), node('2', '10.255.0.2')]
const DEFAULT_LINKS = [link('1', '2'), link('2', '1')]
const DEFAULT_PREFIXES = [prefix('10.255.0.1/32', '1', '10.255.0.1')]
const OK_META: Meta = { warnings: [], total_matched: 2, next_cursor: null }

/**
 * xr-p2's captured view, exactly as `/v1/ls/*` returned it -- see
 * ui/src/api/fixtures/README.md's link-state.json entry. Used below for the
 * one test in this suite that mounts a real wire shape rather than a
 * synthesized stand-in: zero nodes, non-empty links and prefixes, and
 * `session_dumping` on exactly those two legs and not the (clean, empty)
 * nodes leg. Its router_ip is read off the capture itself rather than
 * hardcoded, so a re-capture against a different router cannot silently
 * desync the constant from the data pulled through it.
 */
const XR_P2 = linkState['xr-p2']
const XR_P2_ROUTER = XR_P2.links.data[0].router_ip

/**
 * nx-p3's captured view -- this archive's only complete one, and until
 * 2026-09-17 no test in this suite mounted it at all.
 *
 * Every count test above drives the screen with two or three synthesized
 * rows, which pins a collapse but says nothing about what this screen
 * actually prints over the one real view it has. Its router_ip is read off
 * the capture, the same way `XR_P2_ROUTER` is, so a re-capture against a
 * different router cannot silently desync the constant from the data.
 */
const NX_P3_VIEW = linkState['nx-p3']
const NX_P3_VIEW_ROUTER = NX_P3_VIEW.nodes.data[0].router_ip

/**
 * A Meta that must carry a warning of the given code, in the style of
 * `PathGraph.test.ts`'s `requireEdge`.
 *
 * xr-p2's links and prefixes legs are this project's only real capture of
 * `session_dumping` outside a synthesized meta object. If a re-capture ever
 * caught that peer's dump finishing, this throws rather than leaving the
 * test below quietly asserting nothing about a warning that is no longer
 * there.
 */
function requireWarning(meta: Meta, code: string): void {
  if (!meta.warnings.some((w) => w.code === code)) {
    throw new Error(`fixture meta carries no "${code}" warning: ${JSON.stringify(meta.warnings)}`)
  }
}

// `null` is the state before an answer exists -- what the composable's data
// is until its query resolves -- and it is a different thing from `[]`, which
// is the server's positive "we looked and there is nothing". Keeping the two
// apart here is what makes the loading and empty tests below falsifiable.
let rows: {
  nodes: LsNode[] | null
  links: LsLink[] | null
  prefixes: LsPrefix[] | null
}
// One meta per leg, because the daemon builds one per leg: handleLSLinks
// and handleLSPrefixes append their own `truncated` and `session_dumping`
// warnings exactly as handleLSNodes does.
let metas: { nodes: Meta; links: Meta; prefixes: Meta }
let lsError: Error | undefined

// The scope ref the screen hands its composable, captured on the way past. A
// stub that ignored its argument would leave the whole scope control
// untested: the three dropdowns and the URL are the only things standing
// between "one router's view" and a request for somebody else's.
let lsScope: { value: LinkStateScope | undefined } | undefined

const route = reactive({ query: {} as Record<string, string> })
const push = vi.fn()
const replace = vi.fn()
vi.mock('vue-router', () => ({
  useRoute: () => route,
  useRouter: () => ({ push, replace }),
}))

/**
 * What the three `/v1/ls/*` filters do to a set of rows, so this suite
 * exercises the screen against a narrowing backend rather than a constant
 * one. Without it, choosing a protocol could not be seen to change anything
 * and the control's whole reason for existing would be untested.
 *
 * `router=` is matched against the ROW's observer (`router_ip`), which is
 * what `query.LSNodeFilter` does with it (`fl.eqAddr("n.router_ip",
 * f.Router)`). It is not a claim about the response body -- the rows here
 * are synthesized -- only about which of them a scoped request would have
 * come back with.
 */
function narrowRows<T extends LsCommon>(
  held: T[] | null,
  s: LinkStateScope | undefined,
): T[] | undefined {
  if (!s || held === null) return undefined
  return held.filter(
    (r) =>
      r.router_ip === s.router &&
      (s.protocol === undefined || r.protocol === s.protocol) &&
      (s.area === undefined || r.area === s.area),
  )
}

/**
 * The same three filters over link rows, which carry their scope
 * differently: `protocol` is on the row, and the area is on each endpoint.
 * `area=` matches at EITHER end, in the contract's own words -- "an
 * area-crossing link is returned by a query for either of its two areas".
 */
function narrowLinks(held: LsLink[] | null, s: LinkStateScope | undefined): LsLink[] | undefined {
  if (!s || held === null) return undefined
  return held.filter(
    (l) =>
      l.router_ip === s.router &&
      (s.protocol === undefined || l.protocol === s.protocol) &&
      (s.area === undefined || l.local.area === s.area || l.remote.area === s.area),
  )
}

vi.mock('@/api/queries', () => ({
  useRouters: () => ({ data: ref(routers), isLoading: ref(false), error: ref(undefined) }),
  useLinkState: (scope: { value: LinkStateScope | undefined }) => {
    lsScope = scope
    return {
      // Undefined until the query resolves, and undefined while no scope is
      // set -- the real composable's own contract (queries.ts: each query
      // returns undefined while `scope.value` is undefined, so none of the
      // three fires).
      nodes: computed(() => narrowRows(rows.nodes, scope.value)),
      links: computed(() => narrowLinks(rows.links, scope.value)),
      prefixes: computed(() => narrowRows(rows.prefixes, scope.value)),
      nodesMeta: computed(() => (scope.value && rows.nodes !== null ? metas.nodes : undefined)),
      linksMeta: computed(() => (scope.value && rows.links !== null ? metas.links : undefined)),
      prefixesMeta: computed(() =>
        scope.value && rows.prefixes !== null ? metas.prefixes : undefined,
      ),
      isLoading: computed(() => Boolean(scope.value) && rows.nodes === null),
      error: computed(() => (scope.value ? lsError : undefined)),
      refetch: async () => {},
    }
  },
}))

const LinkStateView = (await import('./LinkStateView.vue')).default
const { RAIL_PREFIX_COLUMNS } = await import('./LinkStateView.vue')

interface Options {
  /** Chosen in the router dropdown after mount, the way an operator does. */
  router?: string
  protocol?: number
  area?: number
  /** `null` means "this query has not resolved yet". */
  nodes?: LsNode[] | null
  links?: LsLink[] | null
  prefixes?: LsPrefix[] | null
  meta?: Meta
  linksMeta?: Meta
  prefixesMeta?: Meta
  error?: Error
  /** The URL the screen opens on, as a shared link would carry it. */
  query?: Record<string, string>
}

function freshState() {
  rows = { nodes: DEFAULT_NODES, links: DEFAULT_LINKS, prefixes: DEFAULT_PREFIXES }
  metas = { nodes: OK_META, links: OK_META, prefixes: OK_META }
  lsError = undefined
  lsScope = undefined
  for (const k of Object.keys(route.query)) delete route.query[k]
  push.mockClear()
  replace.mockClear()
}

/** Drives one of the screen's real dropdowns, rather than reaching past it. */
async function choose(w: VueWrapper, control: string, value: string) {
  await w.get(control).setValue(value)
  await nextTick()
}

async function mountView(opts: Options = {}): Promise<VueWrapper> {
  if (opts.nodes !== undefined) rows.nodes = opts.nodes
  if (opts.links !== undefined) rows.links = opts.links
  if (opts.prefixes !== undefined) rows.prefixes = opts.prefixes
  if (opts.meta) metas.nodes = opts.meta
  if (opts.linksMeta) metas.links = opts.linksMeta
  if (opts.prefixesMeta) metas.prefixes = opts.prefixesMeta
  if (opts.error) lsError = opts.error
  if (opts.query) Object.assign(route.query, opts.query)

  const w = mount(LinkStateView)
  await nextTick()
  if (opts.router) await choose(w, '[data-router]', opts.router)
  if (opts.protocol !== undefined) await choose(w, '[data-protocol]', String(opts.protocol))
  if (opts.area !== undefined) await choose(w, '[data-area]', String(opts.area))
  return w
}

/** One node per protocol, so the protocol control has something to offer. */
function nodesAcrossProtocols(protocols: number[]): LsNode[] {
  return protocols.map((p, i) =>
    node(`p${p}`, `10.255.0.${i + 1}`, { protocol: p, protocol_name: `protocol-${p}` }),
  )
}

/** One node per area, the same way, for rule 4's other axis. */
function nodesAcrossAreas(areas: number[]): LsNode[] {
  return areas.map((a, i) => node(`a${a}`, `10.255.0.${i + 1}`, { area: a }))
}

describe('LinkStateView', () => {
  beforeEach(freshState)

  it('names the router whose view this is', async () => {
    // Rule 1. The premise of the screen. A graph that does not say whose
    // LSDB produced it is claiming to describe the network, which no LSDB
    // can do -- the four routers in this test archive report 7, 6, 5 and
    // 1 nodes for the same router set.
    const w = await mountView({ router: NX_P3 })
    expect(w.get('[data-whose-view]').text()).toMatch(/nx-p3/)
    expect(w.get('[data-whose-view]').text()).toMatch(/10\.0\.0\.11/)
    // And the counts carry it too, for the same reason: a number with no
    // observer beside it is a claim about the network.
    expect(w.get('[data-summary]').text()).toMatch(/nx-p3/)
  })

  it('counts distinct objects, not rows', async () => {
    // Rule 2. The archive holds 139 ls_nodes rows for 7 distinct nodes,
    // because a node is re-observed on every refresh and `/v1/ls/nodes`
    // groups by peer and rib as well as by node -- one router with two BMP
    // peers reports every node twice. A screen that printed the row count
    // would be reporting a collection artifact as topology, this
    // repository's most recurring defect.
    //
    // Three rows, two distinct node_keys. The summary must say 2.
    const w = await mountView({
      router: NX_P3,
      nodes: [node('1', '10.255.0.1'), node('1', '10.255.0.1'), node('2', '10.255.0.2')],
    })
    expect(w.get('[data-summary]').text()).toMatch(/\b2 nodes\b/)
    expect(w.get('[data-summary]').text()).not.toMatch(/\b3 nodes\b/)
    // The canvas has to agree, which is the half a count on its own cannot
    // prove: a re-observed node drawn twice is two boxes on top of a claim
    // that there are two nodes.
    expect(w.findAll('[data-node]')).toHaveLength(2)
  })

  it('counts the directions the canvas draws, not the link rows behind them', async () => {
    // This test pins two rules together. `@/lib/lsAdjacencies` is the
    // definition of a distinct adjacency and the canvas draws what it
    // returns, so the
    // summary asks the same function rather than recomputing identity --
    // one screen over, TopologyView's summary read the untrimmed graph while
    // the canvas drew the trimmed one, and printed "5 ASNs · 4 edges" over a
    // blank pane.
    //
    // Keying by node pair collapses parallel links, and the wording has to
    // match: query.LSLink's own comment warns that collapsing reports "this
    // path has no redundancy" when the truth is "it does". Two links between
    // one pair draw as ONE line marked ×2, so the count of pairs says how
    // many links it stands for.
    const parallel = link('1', '2', {
      igp_metric: 40,
      local: { ...endpoint('1', '10.255.0.1'), ifaddr: '10.1.2.1', interface_id: 2 },
      remote: { ...endpoint('2', '10.255.0.2'), ifaddr: '10.1.2.2', interface_id: 2 },
    })
    const w = await mountView({
      router: NX_P3,
      links: [link('1', '2'), parallel, link('2', '1')],
    })
    // Two directions (1->2 and 2->1) over three distinct advertised links.
    expect(w.get('[data-summary]').text()).toMatch(/\b2 adjacency directions\b/)
    expect(w.get('[data-summary]').text()).toMatch(/\b3 advertised links\b/)
    // With the qualification @/lib/lsAdjacencies asks callers for in as many
    // words: "one link as the database identifies links" rather than "one
    // link". Two links advertising neither interface addresses nor link IDs
    // are indistinguishable here, as they are in the query. The canvas
    // legend carries this; the count has to as well.
    expect(w.get('[data-counts-say]').text()).toMatch(/as the database identifies links/)
    // And that is exactly what the canvas drew: two lines, one of them
    // carrying the ×2 that the count of pairs would otherwise contradict.
    expect(w.findAll('[data-edge]')).toHaveLength(2)
    expect(w.get('[data-edge-label="1-2"]').text()).toMatch(/×2/)
  })

  it('counts adjacency directions, which is not the number of node pairs', async () => {
    // Testing in a browser, against a restored archive, found the screen
    // printing the DIRECTED count -- "20 adjacencies" -- under a sentence
    // describing unordered pairs, over a canvas drawing 20 curves for 10
    // pairs. The drawing is right: IGP metrics are per direction and the
    // restored archive holds a pair that is metric 40 one way and 1 the
    // other, so collapsing the two curves would hide an asymmetric-metric
    // link. The words were wrong, and this pins the two numbers apart so
    // they cannot be silently swapped.
    //
    // Three unordered pairs: 1-2 and 2-3 reciprocal, 1-3 seen from one side
    // only. Five directions, five curves.
    const w = await mountView({
      router: NX_P3,
      nodes: [node('1', '10.255.0.1'), node('2', '10.255.0.2'), node('3', '10.255.0.3')],
      links: [link('1', '2'), link('2', '1'), link('2', '3'), link('3', '2'), link('1', '3')],
    })
    expect(w.get('[data-summary]').text()).toMatch(/\b5 adjacency directions\b/)
    expect(w.get('[data-summary]').text()).not.toMatch(/\b3 adjacenc/)
    expect(w.findAll('[data-edge]')).toHaveLength(5)

    // And the pair count really is 3 -- the number the old wording claimed
    // while this one was on screen.
    const pairs = new Set(
      w.findAll('[data-edge]').map((e) => e.attributes('data-edge')!.split('-').sort().join('-')),
    )
    expect(pairs.size).toBe(3)

    // The sentence beneath says what the number counts.
    expect(w.get('[data-counts-say]').text()).toMatch(/a link both ends report is two of them/)
    expect(w.get('[data-counts-say]').text()).not.toMatch(/pair of nodes drawn as one line/)
  })

  it('renders the server warnings rather than calling a warned answer complete', async () => {
    // Rule 6. The server already emits `truncated` and `session_dumping`
    // (api/handlers.go:1627-1632) and ResultMeta.vue already renders them;
    // this screen's whole job is to mount it and not swallow them. It must
    // not recompute truncation, and must not print "complete as of" over an
    // answer the daemon warned about.
    const w = await mountView({
      router: NX_P3,
      meta: {
        warnings: [
          { code: 'truncated', message: '24000 rows matched and 2 were returned' },
          { code: 'session_dumping', message: 'a peer is still sending its initial dump' },
        ],
        total_matched: 24000,
        next_cursor: null,
      },
    })
    // ResultMeta's own copy for `truncated`, over the rows this answer
    // returned and the total the server matched.
    expect(w.text()).toMatch(/showing 2 of 24000/)
    expect(w.text()).toMatch(/still loading its initial view/)
    expect(w.text()).not.toMatch(/complete as of/i)
  })

  it('surfaces a warned LINKS answer, and stops calling the screen complete', async () => {
    // Rule 6 is per ANSWER. handleLSLinks (api/handlers.go:1700-1711) and
    // handleLSPrefixes (1775-1784) append their own truncated and
    // session_dumping warnings exactly as handleLSNodes does -- so the links
    // answer can be a sample while the nodes answer is whole, and then the
    // canvas is short of adjacencies, "N adjacencies" is understated, and an
    // unlabeled "complete as of" beside them is the one sentence this rule
    // forbids. Reading only the nodes meta is what let that happen.
    const w = await mountView({
      router: NX_P3,
      linksMeta: {
        warnings: [{ code: 'truncated', message: '24000 rows matched and 2 were returned' }],
        total_matched: 24000,
        next_cursor: null,
      },
    })
    // The warning is on screen, against the LINK rows it describes -- not
    // against the nodes answer's totals, which merging the three metas
    // would have paired it with.
    expect(w.get('[data-meta-leg="links"]').text()).toMatch(/showing 2 of 24000/)
    // And no completeness claim anywhere, labeled or not: two of the three
    // answers are clean, and saying so beside a short canvas is the claim
    // this rule is about.
    expect(w.text()).not.toMatch(/complete as of/i)
    expect(w.findAll('[data-meta-leg]').map((l) => l.attributes('data-meta-leg'))).toEqual([
      'links',
    ])
  })

  it('claims completeness only when all three answers are in and clean', async () => {
    // The other half, so the test above cannot pass by never printing the
    // sentence at all. One unlabeled line, because it is then true of every
    // one of the three.
    const w = await mountView({ router: NX_P3 })
    expect(w.text()).toMatch(/complete as of/i)
    expect(w.find('[data-meta-leg]').exists()).toBe(false)

    // A prefixes answer that is still pending is not a complete one either.
    freshState()
    const pending = await mountView({ router: NX_P3, prefixes: null })
    expect(pending.text()).not.toMatch(/complete as of/i)
  })

  it('surfaces a warning over the empty view, where it matters most', async () => {
    // The empty view is nested inside the answered branch precisely so the
    // footer reaches it: an empty answer while a peer is still sending its
    // initial dump is "nothing YET", and without the warning it reads as
    // "this router has no IGP topology".
    const w = await mountView({
      router: NX_P3,
      nodes: [],
      links: [],
      prefixes: [],
      meta: {
        warnings: [{ code: 'session_dumping', message: 'a peer is still sending its dump' }],
        total_matched: 0,
        next_cursor: null,
      },
    })
    expect(w.find('[data-empty-view]').exists()).toBe(true)
    expect(w.get('[data-meta-leg="nodes"]').text()).toMatch(/still loading its initial view/)
    expect(w.text()).not.toMatch(/complete as of/i)
  })

  it('renders xr-p2\'s real capture: no nodes, and a warned leg beside a clean one', async () => {
    // Every other test in this file hands the screen a synthesized stand-in
    // for this shape (the test above included: `nodes: []` there is typed,
    // not measured). This one hands it xr-p2's actual captured answer
    // (ui/src/api/fixtures/link-state.json), unmodified, and it is worth
    // having on its own terms: xr-p2 is simultaneously the links-without-
    // nodes case (a real `data: []` on the nodes leg, not an invented one),
    // the warned-leg-beside-a-clean-leg case (nodes carries no warning of its
    // own; links and prefixes both do), and -- one level down, in
    // LinkStateGraph.test.ts -- a one-sided adjacency. requireWarning guards the
    // second of those so a re-capture that resolved xr-p2's dump cannot leave
    // this test quietly passing over a warning that is no longer there.
    //
    // The router is seeded through the URL rather than the picker
    // (`choose(w, '[data-router]', ...)`): xr-p2's real router_ip is not one
    // of this suite's synthesized `routers` entries (see that constant's own
    // comment on why), and the "restores a scope from the URL" test below
    // already establishes that the screen reads a scope straight off the
    // address without validating it against the picker's list.
    requireWarning(XR_P2.links.meta as Meta, 'session_dumping')
    requireWarning(XR_P2.prefixes.meta as Meta, 'session_dumping')
    expect((XR_P2.nodes.meta as Meta).warnings).toEqual([])

    const w = await mountView({
      query: { router: XR_P2_ROUTER },
      nodes: XR_P2.nodes.data as LsNode[],
      links: XR_P2.links.data as LsLink[],
      prefixes: XR_P2.prefixes.data as LsPrefix[],
      meta: XR_P2.nodes.meta as Meta,
      linksMeta: XR_P2.links.meta as Meta,
      prefixesMeta: XR_P2.prefixes.meta as Meta,
    })

    // Real `data: []`, not a pending query and not a synthesized empty array.
    expect(w.find('[data-empty-view]').exists()).toBe(true)
    expect(w.findAll('[data-node]')).toHaveLength(0)

    // The clean leg (nodes) stays silent; both warned legs name themselves;
    // no completeness claim anywhere, because two of the three answers are
    // not clean.
    expect(w.findAll('[data-meta-leg]').map((l) => l.attributes('data-meta-leg')).sort()).toEqual([
      'links',
      'prefixes',
    ])
    expect(w.text()).toMatch(/still loading its initial view/)
    expect(w.text()).not.toMatch(/complete as of/i)

    // And the two adjacency rows it DID return are accounted for. Both name
    // nodes this answer does not carry, so `drawnAdjacencies` discards both
    // and the canvas is empty -- but the rows are in hand, and until this
    // sentence existed the screen said nothing at all about the only
    // adjacency data xr-p2 returns. `unreachablePrefixes` has done the
    // equivalent for the prefix leg since the screen shipped; this is the
    // link leg's half of the same accounting.
    expect(w.get('[data-undrawn-adjacencies]').text()).toMatch(
      /2 adjacency directions in this answer are neither drawn nor counted/,
    )
    expect(w.get('[data-undrawn-adjacencies]').text()).toMatch(
      /names a node this router did not report/,
    )
  })

  it("renders nx-p3's real capture, and the summary counts match the wire", async () => {
    // The rich capture, mounted for the first time. Measured on the
    // committed fixture and confirmed against a live `/v1/ls/*` curl on
    // 2026-09-17 -- the two agree exactly:
    //
    //   nodes      7 rows,  7 distinct node_keys
    //   links     22 rows, 20 distinct DIRECTED adjacencies over 10
    //             unordered pairs, 0 one-sided
    //   prefixes  27 rows, 27 distinct (node_key, prefix) pairs,
    //             16 distinct CIDRs -- eight transit /31s advertised by the
    //             routers at both ends, a ninth by four nodes, and seven
    //             loopback /32s by one node each (8x2 + 4 + 7 = 27)
    //
    // The prefix axis is why this is a capture test rather than a fourth
    // synthesized one. On this view 27 originations collapse from 27 rows:
    // rule 2's collapse is a NO-OP here, so a lone "27 prefixes" is a number
    // no reader and no assertion can tell from a row count. The 16 beside it
    // is the number a row count cannot produce, which is why the summary
    // prints it.
    const nodes = NX_P3_VIEW.nodes.data as LsNode[]
    const links = NX_P3_VIEW.links.data as LsLink[]
    const prefixes = NX_P3_VIEW.prefixes.data as LsPrefix[]

    // Derived from the capture rather than typed in, so a re-capture moves
    // the expectation with the data instead of failing on a constant nobody
    // can trace back to a row.
    const distinctNodes = new Set(nodes.map((n) => n.node_key)).size
    const directions = new Set(links.map((l) => `${l.local.node_key}-${l.remote.node_key}`)).size
    const originations = new Set(prefixes.map((p) => `${p.node_key}|${p.prefix}`)).size
    const networks = new Set(prefixes.map((p) => p.prefix)).size

    // The premise, asserted rather than assumed, in the style of
    // `requireWarning` above: if a re-capture ever made the distinct count
    // differ from the row count, or made every prefix its own network, this
    // test's whole reason for naming both numbers would be gone and it
    // should fail loudly rather than keep passing.
    expect(originations).toBe(prefixes.length)
    expect(networks).toBeLessThan(originations)

    const w = await mountView({
      // Seeded through the URL for the same reason the xr-p2 test above is:
      // the real router_ip is not one of this suite's synthesized `routers`
      // entries.
      query: { router: NX_P3_VIEW_ROUTER },
      nodes,
      links,
      prefixes,
      meta: NX_P3_VIEW.nodes.meta as Meta,
      linksMeta: NX_P3_VIEW.links.meta as Meta,
      prefixesMeta: NX_P3_VIEW.prefixes.meta as Meta,
    })

    const summary = w.get('[data-summary]').text()
    expect(summary).toContain(`${distinctNodes} nodes`)
    expect(summary).toContain(
      `${directions} adjacency directions over ${links.length} advertised links`,
    )
    expect(summary).toContain(`${originations} prefixes over ${networks} networks`)
    // The whole sentence as it stands today, so a refactor that rebuilt each
    // phrase out of different numbers cannot pass by moving all three
    // together with the derivations above.
    expect(summary).toContain(
      '7 nodes · 20 adjacency directions over 22 advertised links · 27 prefixes over 16 networks',
    )

    // The canvas agrees with every count, which is the half a summary can
    // never prove about itself -- and is the seam that printed
    // "5 ASNs · 4 edges" over a blank pane one screen over.
    expect(w.findAll('[data-node]')).toHaveLength(distinctNodes)
    expect(w.findAll('[data-edge]')).toHaveLength(directions)

    // Nothing was discarded on either axis on this view, so neither
    // accounting sentence appears: both are statements about an answer, not
    // caveats the screen always carries.
    expect(w.find('[data-unreachable-prefixes]').exists()).toBe(false)
    expect(w.find('[data-undrawn-adjacencies]').exists()).toBe(false)

    // All three legs clean, so rule 6's single completeness line is the one
    // that shows -- the real captured metas, not a synthesized OK_META.
    expect(w.findAll('[data-meta-leg]')).toHaveLength(0)
  })

  it('puts the scope in the URL', async () => {
    // /topology shipped without this, and later testing named it
    // the one answer screen whose question cannot be shared. "Look at what
    // xr-p2 sees versus nx-p3" is the premise of THIS screen, so all three
    // parts of the scope go in the address.
    const w = await mountView({
      router: NX_P3,
      // Two protocols, and two areas WITHIN the chosen one, so all three
      // controls are on offer at the moment each is used -- which is also
      // the only shape rule 4's scope has three parts for.
      nodes: [
        node('1', '10.255.0.1', { protocol: 3, protocol_name: 'ospfv2', area: 0 }),
        node('2', '10.255.0.2', { protocol: 3, protocol_name: 'ospfv2', area: 1 }),
        node('3', '10.255.0.3', { protocol: 2, protocol_name: 'isis-l2', area: 0 }),
      ],
      protocol: 3,
      area: 0,
    })
    // `find`, never `get`: get() throws when the element is absent, so the
    // negative half of an existence assertion written with it can never fail.
    expect(w.find('[data-protocol]').exists()).toBe(true)
    expect(w.find('[data-area]').exists()).toBe(true)
    expect(replace).toHaveBeenCalledWith(
      expect.objectContaining({ query: { router: NX_P3, protocol: '3', area: '0' } }),
    )
  })

  it('restores a scope from the URL, area 0 included', async () => {
    // Area 0 is the OSPF backbone and 85% of the archive's ls_nodes rows, so
    // the falsy-zero trap here is not a hypothetical: a restore written as
    // `Number(q) || undefined` would drop the most common area in the data
    // and silently widen a shared link's question to every area.
    await mountView({
      query: { router: NX_P3, protocol: '3', area: '0' },
      nodes: nodesAcrossProtocols([2, 3]),
    })
    expect(lsScope?.value).toEqual({ router: NX_P3, protocol: 3, area: 0 })
    // And no navigation, because the address already says this: a screen
    // that rewrote the URL it was opened on would push a history entry for
    // every shared link.
    expect(replace).not.toHaveBeenCalled()
  })

  it('offers a protocol control only when the scope holds more than one', async () => {
    // Rule 4. In this archive that is a single router dropdown -- only
    // protocol 3 (OSPFv2) appears anywhere in the surviving capture -- and
    // an IS-IS L1/L2 router grows the control rather than silently merging
    // two topologies onto one canvas.
    const one = await mountView({ router: NX_P3, nodes: nodesAcrossProtocols([3]) })
    expect(one.find('[data-protocol]').exists()).toBe(false)

    freshState()
    const three = await mountView({ router: NX_P3, nodes: nodesAcrossProtocols([1, 2, 3]) })
    expect(three.find('[data-protocol]').exists()).toBe(true)
  })

  it('offers an area control on the same rule', async () => {
    // The other half of rule 4's scope, and the one this archive could
    // actually produce: OSPF area 0 and area 1 share no edges, so drawing
    // them on one canvas invents adjacency.
    const one = await mountView({ router: NX_P3, nodes: nodesAcrossAreas([0]) })
    expect(one.find('[data-area]').exists()).toBe(false)

    freshState()
    const two = await mountView({ router: NX_P3, nodes: nodesAcrossAreas([0, 1]) })
    expect(two.find('[data-area]').exists()).toBe(true)
  })

  it('can be widened again after narrowing to one protocol', async () => {
    // The trap a control derived from the current answer alone falls into:
    // choose L2, the next answer holds only L2, and the control that offered
    // the choice disappears with `protocol=2` still in the address -- so
    // reloading the shared link restores the narrowed question and the
    // operator has no way back except editing the URL.
    const w = await mountView({
      router: NX_P3,
      nodes: nodesAcrossProtocols([1, 2]),
      protocol: 2,
    })
    expect(lsScope?.value).toEqual({ router: NX_P3, protocol: 2, area: undefined })
    expect(w.find('[data-protocol]').exists()).toBe(true)

    // The way back, and it really widens the question rather than only the
    // control: the other protocol's node is on the canvas again.
    await choose(w, '[data-protocol]', '')
    expect(lsScope?.value).toEqual({ router: NX_P3, protocol: undefined, area: undefined })
    expect(w.findAll('[data-node]')).toHaveLength(2)
    expect(w.findAll('[data-protocol] option')).toHaveLength(3)
  })

  it('drops one router\'s narrowing when another router is chosen', async () => {
    // A protocol is a value out of ONE router's database. Carried across a
    // router change it narrows the new router's view by a number that came
    // from somebody else's -- and in the common case narrows it to nothing,
    // which would read as "this router reports no topology".
    const w = await mountView({
      router: NX_P3,
      nodes: [
        ...nodesAcrossProtocols([1, 2]),
        // A node reported by the OTHER router, carrying protocol 3 --
        // which neither of nx-p3's protocols above would match. (XR_RR1's
        // real answer is empty; see the file banner. Here it stands in for
        // any second router whose view is a different topology.)
        node('r1', '10.255.0.9', { router_ip: XR_RR1, router_sysname: 'xr-rr1' }),
      ],
      protocol: 2,
    })
    expect(lsScope?.value).toEqual({ router: NX_P3, protocol: 2, area: undefined })

    await choose(w, '[data-router]', XR_RR1)
    expect(lsScope?.value).toEqual({ router: XR_RR1, protocol: undefined, area: undefined })
    expect(w.findAll('[data-node]').map((n) => n.attributes('data-node'))).toEqual(['r1'])
    // And out of the address too, or the shared link would carry a question
    // about a router that was never asked it.
    expect(replace).toHaveBeenCalledWith(expect.objectContaining({ query: { router: XR_RR1 } }))
  })

  it('draws one topology once a protocol is chosen, not two', async () => {
    // Rule 4 end to end: the control, the scope, the request and the canvas.
    // Before the choice both protocols' nodes are on one canvas and the
    // component says so itself; after it, one topology is drawn.
    const w = await mountView({ router: NX_P3, nodes: nodesAcrossProtocols([2, 3]) })
    expect(w.findAll('[data-node]')).toHaveLength(2)
    expect(w.find('[data-scope-mixed]').exists()).toBe(true)

    await choose(w, '[data-protocol]', '3')
    expect(w.findAll('[data-node]').map((n) => n.attributes('data-node'))).toEqual(['p3'])
    expect(w.find('[data-scope-mixed]').exists()).toBe(false)
  })

  it('tells an empty view apart from no router chosen', async () => {
    // Two different facts and they must not render alike. "No router
    // chosen" is a question nobody has asked; an empty view is this
    // router's own answer -- `data: []` on all three paths, which this API
    // returns as a positive "we looked and there is nothing".
    const nothing = await mountView()
    expect(nothing.find('[data-nothing-asked]').exists()).toBe(true)
    expect(nothing.find('[data-empty-view]').exists()).toBe(false)
    expect(nothing.find('[data-summary]').exists()).toBe(false)
    expect(lsScope?.value).toBeUndefined()

    freshState()
    // xr-rr1 has no rows in this answer at all, which is what a router that
    // advertises no BGP-LS looks like from here.
    const empty = await mountView({ router: XR_RR1 })
    expect(empty.find('[data-empty-view]').exists()).toBe(true)
    expect(empty.find('[data-nothing-asked]').exists()).toBe(false)
    // Named, because an empty answer is still one router's answer.
    expect(empty.get('[data-empty-view]').text()).toMatch(/xr-rr1/)
  })

  it('tells a failed answer and a pending one apart from both', async () => {
    // The other two of the four states. An error is "we do not know"; a
    // pending query is "we have not been told yet"; neither is "there is
    // nothing here", and a screen that rendered any two of them alike would
    // tell an operator the topology is empty when the daemon is down.
    const failed = await mountView({ router: NX_P3, error: new Error('ls nodes: 503') })
    expect(failed.get('[data-error]').text()).toMatch(/503/)
    expect(failed.find('[data-summary]').exists()).toBe(false)
    expect(failed.find('[data-empty-view]').exists()).toBe(false)

    freshState()
    // This pins a rule, as a test: `nodes.value ?? []` at the
    // prop-binding site would render "we looked and there is nothing"
    // about a question that has not been answered yet. The gate is on the
    // data being there.
    const pending = await mountView({ router: NX_P3, nodes: null, links: null, prefixes: null })
    expect(pending.find('[data-loading]').exists()).toBe(true)
    expect(pending.find('[data-empty-view]').exists()).toBe(false)
    expect(pending.find('[data-summary]').exists()).toBe(false)
    expect(pending.find('[data-node]').exists()).toBe(false)
  })

  it('shows the selected node in the rail with its prefixes and SR ranges', async () => {
    const w = await mountView({ router: NX_P3 })
    await w.get('[data-node="1"]').trigger('click')
    const rail = w.get('[data-rail]').text()
    expect(rail).toMatch(/10\.255\.0\.1/) // router-id
    expect(rail).toMatch(/16000/) // srgb_base
    expect(rail).toMatch(/15000/) // srlb_base
    expect(rail).toMatch(/10\.255\.0\.1\/32/) // the prefix it originates
  })

  it('shows only the selected node\'s prefixes, once each', async () => {
    // Rule 2 in the rail: a prefix re-observed on every refresh is one
    // prefix, and another node's prefix is not this node's at all.
    const w = await mountView({
      router: NX_P3,
      prefixes: [
        prefix('10.255.0.1/32', '1', '10.255.0.1'),
        prefix('10.255.0.1/32', '1', '10.255.0.1'),
        prefix('10.255.0.2/32', '2', '10.255.0.2'),
      ],
    })
    await w.get('[data-node="1"]').trigger('click')
    expect(w.findAll('[data-prefix-row]')).toHaveLength(1)
    expect(w.get('[data-rail]').text()).not.toMatch(/10\.255\.0\.2\/32/)
    // And the summary counts both nodes' prefixes once each, not three rows.
    expect(w.get('[data-summary]').text()).toMatch(/\b2 prefixes\b/)
  })

  it('accounts for a prefix no box on the canvas can show', async () => {
    // Counting prefixes over the whole answer is right -- the canvas draws
    // no prefix at all, so there is no drawn set for them to agree with --
    // but a thin LSDB can report a prefix whose originating node row this
    // router never received, and that prefix is then in the count with no
    // rail row reachable for it. Said on screen, with the number, rather
    // than leaving two numbers that do not add up.
    const w = await mountView({
      router: NX_P3,
      prefixes: [
        prefix('10.255.0.1/32', '1', '10.255.0.1'),
        prefix('10.9.9.0/24', '9', '10.255.0.9'),
      ],
    })
    expect(w.get('[data-summary]').text()).toMatch(/\b2 prefixes\b/)
    expect(w.get('[data-unreachable-prefixes]').text()).toMatch(
      /1 of them is originated by a node this router did not report/,
    )
    // Not reachable, which is the thing being accounted for: node 1 is the
    // only node in the view and its rail holds only its own prefix.
    await w.get('[data-node="1"]').trigger('click')
    expect(w.get('[data-rail]').text()).not.toMatch(/10\.9\.9\.0/)
  })

  it('accounts for an adjacency no curve on the canvas can draw', async () => {
    // The link-side half of the same accounting. `drawnAdjacencies` drops an
    // adjacency naming a node outside the node set, and until this sentence
    // existed the discards were silent -- the screen counted what survived
    // the filter and said nothing about what did not.
    //
    // Synthesized, deliberately, and it is the case the real capture cannot
    // reach: xr-p2 is this archive's only view with a discarded adjacency and
    // it reports ZERO nodes, so the drawn-canvas version of this state (nodes
    // on screen, and one adjacency missing from them) has no capture behind
    // it. The xr-p2 test above covers the empty-canvas version on real rows;
    // this covers the singular wording and the non-empty branch.
    const w = await mountView({
      router: NX_P3,
      nodes: [node('1', '10.255.0.1'), node('2', '10.255.0.2')],
      // Two drawn directions between 1 and 2, plus one naming a node this
      // answer does not carry.
      links: [link('1', '2'), link('2', '1'), link('1', '9')],
    })
    expect(w.get('[data-summary]').text()).toMatch(/\b2 adjacency directions\b/)
    expect(w.findAll('[data-edge]')).toHaveLength(2)
    // Singular, and grammatical: "1 adjacency direction ... is ... it names".
    expect(w.get('[data-undrawn-adjacencies]').text()).toMatch(
      /^1 adjacency direction in this answer is neither drawn nor counted: it names a node this router did not report/,
    )
  })

  it('says nothing about undrawn adjacencies when every endpoint has its node', async () => {
    // So the sentence above is a statement about an answer rather than a
    // caveat the screen always carries, the same way the prefix one is.
    const w = await mountView({ router: NX_P3 })
    expect(w.find('[data-undrawn-adjacencies]').exists()).toBe(false)
  })

  it('says nothing about unreachable prefixes when every prefix has its node', async () => {
    // So the sentence above is a statement about an answer rather than a
    // caveat the screen always carries.
    const w = await mountView({ router: NX_P3 })
    expect(w.find('[data-unreachable-prefixes]').exists()).toBe(false)
  })

  it('names a router with no sysname by its IP alone', async () => {
    // An empty sysname is a real answer -- a BMP session whose initiation
    // carried no name -- and the IP identifies the router in that case, as
    // it does everywhere else in this UI. Written the shorter way this
    // rendered " (10.0.0.15)": a leading space where a name should be, in
    // the whose-view line, the summary and the empty view.
    const w = await mountView({ router: UNNAMED })
    expect(w.get('[data-whose-view]').text()).toContain(UNNAMED)
    expect(w.get('[data-whose-view]').text()).not.toMatch(/\s\(10\.0\.0\.15\)/)
    expect(w.find('[data-router] option[value="10.0.0.15"]').text()).toBe(UNNAMED)
  })

  it('says which name the rail is showing, because the canvas may show another', async () => {
    // LsEndpoint.label is resolved by the API across the fleet, so a node
    // named only by ANOTHER router (label_source: fleet) draws as xr-p1 and
    // has nothing but a router-id in its own row. The rail keeps the
    // conservative claim and says which one it is, rather than opening a
    // heading that silently disagrees with the box that was clicked.
    const w = await mountView({
      router: NX_P3,
      nodes: [node('1', '10.255.0.1', { name: '', router_id_v4: '' }), node('2', '10.255.0.2')],
      links: [
        link('1', '2', {
          local: endpoint('1', '10.255.0.1', 'xr-p1', 'fleet'),
          remote: endpoint('2', '10.255.0.2'),
        }),
        link('2', '1'),
      ],
    })
    expect(w.get('[data-node="1"] [data-node-name]').text()).toBe('xr-p1')
    await w.get('[data-node="1"]').trigger('click')
    expect(w.get('[data-rail] h2').text()).toBe('10.255.0.1')
    expect(w.get('[data-rail-name-source]').text()).toMatch(/carries no name/)

    // And the other tier: a node whose own row names it says so.
    freshState()
    const named = await mountView({
      router: NX_P3,
      nodes: [node('1', '10.255.0.1', { name: 'nx-p3-lo0' }), node('2', '10.255.0.2')],
    })
    await named.get('[data-node="1"]').trigger('click')
    expect(named.get('[data-rail] h2').text()).toBe('nx-p3-lo0')
    expect(named.get('[data-rail-name-source]').text()).toMatch(/in this node's own row/)
  })

  it('shows the dotted router-id in the rail where the API supplies one', async () => {
    // `router_id` is the RAW identifier -- 0aff0001 where the dotted form is
    // 10.255.0.1 -- and `router_id_v4` is the dotted quad or "". The rail's
    // heading already preferred the dotted form, so the field beneath it
    // showing hex made one rail state one identifier two ways.
    const w = await mountView({
      router: NX_P3,
      nodes: [
        node('1', '0aff0001', { router_id_v4: '10.255.0.1' }),
        node('2', '10.255.0.2'),
      ],
    })
    await w.get('[data-node="1"]').trigger('click')
    expect(w.get('[data-rail]').text()).toContain('10.255.0.1')
    expect(w.get('[data-rail]').text()).not.toContain('0aff0001')
    expect(w.find('[data-rail] [data-node-field="router_id_v4"]').exists()).toBe(true)

    // Where the API supplies no dotted form, the raw identifier is all there
    // is and is what shows -- that path is correct and stays.
    freshState()
    const raw = await mountView({
      router: NX_P3,
      nodes: [node('1', '0aff0001', { router_id_v4: '' }), node('2', '10.255.0.2')],
    })
    await raw.get('[data-node="1"]').trigger('click')
    expect(raw.get('[data-rail]').text()).toContain('0aff0001')
    expect(raw.find('[data-rail] [data-node-field="router_id"]').exists()).toBe(true)
  })

  it('invents no metric header in the rail', async () => {
    // columnGuard: a column has to clear two checks, not one -- its id must
    // be a real field on the row, AND its header must not name a metric this
    // pipeline does not measure. A real field wearing a lying label is the
    // hole this closes. Signature is (columns, fixtureRow), see
    // ui/src/test-support/columnGuard.ts:103.
    //
    // A captured row now, not the
    // synthesized one this test used before the fixture existed: nx-p3's
    // first captured prefix row, exactly as `/v1/ls/prefixes` returned it.
    // The synthesized `prefix()` builder was weaker in exactly one way -- a
    // field the contract declares but the server never populates would have
    // passed against it and failed against the wire.
    expect(inventedColumns(RAIL_PREFIX_COLUMNS, linkState['nx-p3'].prefixes.data[0])).toEqual([])

    // And the exported array is what the rail actually renders, rather than
    // a second copy of it that the guard could pass while the screen showed
    // something else.
    const w = await mountView({ router: NX_P3 })
    await w.get('[data-node="1"]').trigger('click')
    expect(w.findAll('[data-prefix-col]').map((th) => th.text())).toEqual(
      RAIL_PREFIX_COLUMNS.map((c) => c.header),
    )
  })
})
