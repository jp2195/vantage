import { RouterLinkStub, type VueWrapper, mount } from '@vue/test-utils'
import type { AsName, Meta } from '@/api/generated'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick, reactive, ref, type Ref } from 'vue'
import rib from '@/api/fixtures/rib-unicast.json'
import routers from '@/api/fixtures/routers.json'
import peers from '@/api/fixtures/peers.json'
import { columnsOf } from '@/test-support/columnsOf'
import { unresolvable } from '@/test-support/routeResolution'
import DumpStateMark from '@/components/DumpStateMark.vue'
import { inventedColumns } from '@/test-support/columnGuard'
import { disambiguationPair } from '@/test-support/disambiguationRows'
import { formatClock } from '@/lib/formatClock'

// Set inside each test below, never left at these initial values: the
// isolation rule requires every test to pass alone via `-t`, so each test
// assigns every one of these itself before mounting rather than relying on
// what a previous test in file order happened to leave behind.
let scopeChosen = false
let restarted = false
let peersFixture: unknown = peers
let routersFixture: unknown = routers
let ribRows: unknown = rib.data
const loadMore = vi.fn()
const reload = vi.fn()

// AS holder names, kept apart from the rest of this file's fixtures: no
// test outside the describe block below cares about names, so its default
// -- a dataset loaded with no rows for it to say anything about -- must
// render exactly like no names feature existed at all.
let asNamesFixture: { data: AsName[]; meta: Meta } | undefined = {
  data: [],
  meta: { warnings: [], total_matched: null, asnames_loaded: true },
}

// Whether the composable capped this screen's batch. The real one derives
// it (queries.ts), so mocking it is how a view test reaches the state that
// needs an operator told something -- see the truncation tests below.
let asNamesTruncated = false

// The ref the screen handed useAsNames -- the instance, not a copy of its
// value, the same reason PeersView.test.ts captures the ref it gives
// usePeers. A screen that built the batch wrong is invisible to any
// assertion made on the rendered table alone, because a wrong batch still
// renders a table.
let capturedAsns: Ref<number[]> | undefined

// usePeers hands the component a ref; reassigning this module variable to the
// same object the component holds lets a test push a LATER value into it after
// mount -- which is how the real composable behaves when /v1/peers resolves a
// moment after the screen renders, and the only way to exercise a scope seeded
// from the URL before its peer list exists.
let peersRef = ref<unknown>(peers)

// A route whose query a test sets BEFORE mounting, mirroring an operator
// opening a shared link. Vue Router keeps one reactive route object for the
// app's lifetime (PeerDetailView.test.ts documents the same shape).
const route = reactive({ query: {} as Record<string, string> })
const push = vi.fn()
const replace = vi.fn()
// Spread over the real module rather than replaced outright: the screen only
// needs useRoute stubbed, but a wholesale factory also deletes createRouter,
// and '@/test-support/routeResolution' imports the real router.ts to check
// that this screen's links resolve against the real route table. The stubs
// below come after the spread, so they still win.
vi.mock('vue-router', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-router')>()),
  useRoute: () => route,
  useRouter: () => ({ push, replace }),
}))

vi.mock('@/api/queries', () => ({
  useRouters: () => ({ data: ref(routersFixture), isLoading: ref(false), error: ref(undefined) }),
  usePeers: () => {
    peersRef = ref(peersFixture)
    return { data: peersRef, isLoading: ref(false), error: ref(undefined) }
  },
  useRibPage: (_family: string, scope: Ref<{ collector?: string } | undefined>) => {
    capturedRibScope = scope
    return {
    rows: ref(scopeChosen ? ribRows : []),
    meta: ref(scopeChosen ? rib.meta : undefined),
    restarted: ref(restarted),
    loading: ref(false),
    error: ref(undefined),
    hasMore: ref(false),
    reload,
    loadMore,
    }
  },
  useAsNames: (asns: Ref<number[]>) => {
    capturedAsns = asns
    return {
      data: ref(asNamesFixture),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(undefined),
      truncated: ref(asNamesTruncated),
    }
  },
}))

let capturedRibScope: Ref<{ collector?: string; session?: string } | undefined> | undefined

const RoutesView = (await import('./RoutesView.vue')).default

// RouterLinkStub rather than `RouterLink: true`: the per-row Detail link's
// whole content is the `to` it composed, which the boolean stub discards.
// Every mount goes through here so a RouterLink anywhere in the screen
// cannot log "Failed to resolve component" into the pristine-console bar.
function mountRoutes() {
  return mount(RoutesView, { global: { stubs: { RouterLink: RouterLinkStub } } })
}

// `scopeChosen` only controls what the MOCKED useRibPage hands back -- it
// says nothing about RoutesView's own `scope` ref, which gates the whole
// v-else block and is set only by ScopePicker's real, unmocked `scope`
// event. That event fires only once ScopePicker's internal watch sees both
// a router and a peer selected, so a test that wants past the "choose a
// router and peer" gate has to drive the two <select> elements themselves,
// exactly the way an operator's click would.
async function chooseScope(w: VueWrapper) {
  const selects = w.findAll('select')
  await selects[0].setValue(routers.data[0].ip)
  await selects[1].setValue(peers.data[1].peer_ip)
}

describe('RoutesView', () => {
  // The route object and the navigation spies are module-scoped and shared,
  // so they are reset here rather than in each test: a query left behind by
  // one test is exactly the order-dependence this suite's isolation rule
  // exists to prevent, and it is invisible until the tests run in a
  // different order.
  beforeEach(() => {
    route.query = {}
    push.mockClear()
    replace.mockClear()
    asNamesFixture = {
      data: [],
      meta: { warnings: [], total_matched: null, asnames_loaded: true },
    }
    asNamesTruncated = false
    capturedAsns = undefined
  })

  it('asks for a scope before fetching, because router and peer are required', () => {
    // A RIB request without router and peer is a 400. Rendering an empty
    // table on load would tell an operator this peer has no routes, when in
    // fact nothing was ever asked.
    scopeChosen = false
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    expect(w.text()).toMatch(/choose a router and peer/i)
    expect(w.find('tbody tr').exists()).toBe(false)
  })

  it('renders both captured warnings once a scope is chosen', async () => {
    // rib-unicast.json carries session_dumping AND paginated_smear. Rendering
    // only the first would drop the one that matters most under infinite
    // scroll, where page boundaries are invisible.
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    expect(w.text()).toMatch(/still loading|counts will rise/i)
    expect(w.text()).toMatch(/repeat|missing/i)
  })

  it('marks rows from a still-dumping session as provisional', async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    // The premise is asserted, not assumed. This read `if (dumping.length >
    // 0) expect(...)`, which would have gone quietly vacuous the first time
    // rib-unicast.json was re-captured against a settled lab -- a test that
    // passes by not running is worse than no test, because the suite still
    // reports it green.
    const dumping = rib.data.filter((r) => r.dump_state === 'dumping')
    expect(dumping).toHaveLength(rib.data.length)
    expect(w.findAllComponents(DumpStateMark)).toHaveLength(rib.data.length)
    expect(w.text()).toMatch(/provisional/i)
  })

  it("does not render dump_state 'unknown' as settled fact", async () => {
    // dump_state has three documented values and this screen tested for one
    // of them (`=== 'dumping'`), so `unknown` rendered exactly like
    // `complete` -- the precise confusion api/openapi.yaml says the field
    // exists to prevent. The Looking glass's own routes.json carries a real
    // `unknown` row, so the value is live in this archive rather than
    // theoretical.
    //
    // SYNTHESIZED for THIS screen: rib-unicast.json's rows are all
    // `dumping`, so this is its own first captured row with only dump_state
    // changed to the value routes.json demonstrates. Capturing it here
    // would mean walking a RIB for a peer that has gone down mid-session,
    // which is what query/query.go's dumpStateExpr resolves to `unknown`.
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = [{ ...rib.data[0], dump_state: 'unknown' }]
    const w = mountRoutes()
    await chooseScope(w)
    expect(w.findAllComponents(DumpStateMark)).toHaveLength(1)
    expect(w.text()).toMatch(/unknown/i)
  })

  it('says when the walk restarted under a session change', async () => {
    // useRibPage raises `restarted` when a cursor walk is resumed against a
    // different session for the same (router, peer): the accumulated rows
    // were dropped because they no longer describe one continuous view.
    // Silence here would let an operator read a freshly-restarted, partial
    // list as the walk continuing normally.
    scopeChosen = true
    restarted = true
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    expect(w.text()).toMatch(/restart/i)
  })

  it('renders no column the API does not measure', async () => {
    // Column ids come from the real UnicastRoute shape, and no header
    // claims a metric that shape cannot support -- the same closed-set
    // guard RoutersView.test.ts and PeersView.test.ts already pin.
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    const columns = columnsOf(w)
    expect(inventedColumns(columns, rib.data[0])).toEqual([])
  })

  it('renders rib and path_id, so the same prefix repeating under different ribs is not shown as one row three times', async () => {
    // SYNTHESIZED, not captured: rib-unicast.json's two real rows already
    // have distinct prefixes, so no capture demonstrates the case
    // api/openapi.yaml's /v1/rib/unicast response describes and query/rib.go
    // verifies against the live archive: the SAME (prefix, path_id)
    // legitimately appears under in_pre, in_post and loc_rib for one peer.
    // Built from rib-unicast.json's own first row with only rib and path_id
    // changed, so every other field -- including the shared prefix -- stays
    // a real captured value.
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    const base = rib.data[0]
    ribRows = [
      { ...base, rib: 'in_pre', path_id: 0 },
      { ...base, rib: 'in_post', path_id: 0 },
      { ...base, rib: 'loc_rib', path_id: 1 },
    ]
    const w = mountRoutes()
    await chooseScope(w)
    const rowTexts = w.findAll('tbody tr').map((r) => r.text())
    expect(rowTexts).toHaveLength(3)
    // Without rib/path_id on screen these three rows -- same prefix, same
    // next_hop, same AS path, same origin/localpref/med -- would render as
    // three visually identical lines, which is exactly what was
    // reproduced live against 10.255.0.3/32.
    expect(new Set(rowTexts).size).toBe(rowTexts.length)
  })

  it('renders path_id, so two rows that differ only in path_id are not shown as one', async () => {
    // Backport of LookingGlassView.test.ts's own version of this check,
    // built from the same shared disambiguationPair helper so the two
    // screens cannot drift on what "differs only in path_id" means. It
    // closes a real hole in the test above: that test's three rows vary rib
    // AND path_id together, so path_id changes always come with a rib
    // change too -- mutation testing proved that dropping
    // ONLY the path_id column still left that test passing, because rib
    // alone kept every row distinct even with path_id hidden. Holding rib
    // constant here and varying only path_id is what makes dropping EITHER
    // column independently break a test in this file.
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    const [a, b] = disambiguationPair(rib.data[0], 'path_id', rib.data[0].path_id + 1)
    ribRows = [a, b]
    const w = mountRoutes()
    await chooseScope(w)
    const rowTexts = w.findAll('tbody tr').map((r) => r.text())
    expect(rowTexts).toHaveLength(2)
    expect(new Set(rowTexts).size).toBe(rowTexts.length)
  })

  it('lists each peer once even when it appears under more than one rib', async () => {
    // SYNTHESIZED, not captured: peers.json has no peer that appears under
    // two ribs, but api/openapi.yaml documents /v1/peers as "one entry per
    // (collector, router, peer, rib)" -- a deployment watching both the
    // pre- and post-policy adj-rib for one session has two real rows for
    // the same peer_ip, each otherwise identical (PeerDetailView.vue hit
    // this exact cardinality fact first, on 2026-09-06). Built from
    // peers.json's own in_pre row with only `rib` changed, so every other
    // field stays a real captured value.
    //
    // useRibPage never sends rib= -- the walk is deliberately over every
    // rib a peer appears under -- so a second row for a peer already
    // listed would only add a same-valued, unselectable duplicate <option>
    // to the picker below.
    scopeChosen = false
    restarted = false
    routersFixture = routers
    ribRows = rib.data
    const capturedRow = peers.data[1]
    peersFixture = { data: [...peers.data, { ...capturedRow, rib: 'in_post' }], meta: peers.meta }
    const w = mountRoutes()
    const selects = w.findAll('select')
    await selects[0].setValue(capturedRow.router_ip)
    const peerValues = selects[1].findAll('option').map((o) => o.element.value)
    expect(new Set(peerValues).size).toBe(peerValues.length)
  })

  it('restores a scope from the URL, even when the peer list arrives after the screen', async () => {
    // The case a shared link actually hits: /routes?router=..&peer=.. renders
    // before /v1/peers resolves, so at first paint the seeded peer matches no
    // row. The picker must finish the job when the list lands rather than
    // emitting undefined once and giving up -- otherwise every shared link
    // opens on the "choose a router and peer" gate it was meant to skip.
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    routersFixture = routers
    peersFixture = { data: [], meta: peers.meta }
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    const w = mountRoutes()
    expect(w.text()).toMatch(/choose a router and peer/i)
    // The link must survive being opened. At mount the picker cannot resolve
    // the seeded peer yet -- /v1/peers has not answered -- and treating that
    // "not yet" as "the user cleared the picker" would erase the query the
    // screen is in the middle of restoring, so a shared link would destroy
    // itself on open.
    expect(replace).not.toHaveBeenCalled()

    peersRef.value = peers
    await nextTick()
    await nextTick()

    expect(w.text()).not.toMatch(/choose a router and peer/i)
    expect(w.find('tbody tr').exists()).toBe(true)
  })

  it('does not re-walk the RIB when the peer list refetches without changing the scope', async () => {
    // The picker now watches peerRows so a late list can complete a seeded
    // pair -- which means a plain 30s refetch also retriggers that watch. If
    // the emit is not deduplicated, every poll re-emits an identical scope and
    // RoutesView reloads the walk, restarting pagination on a timer.
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    mountRoutes()
    await nextTick()
    const afterRestore = reload.mock.calls.length

    peersRef.value = { data: [...peers.data], meta: peers.meta }
    await nextTick()
    await nextTick()

    expect(reload.mock.calls.length).toBe(afterRestore)
  })

  it('puts the question in the URL and never the session', async () => {
    // session_id is a runtime identity discovered at fetch time and used to pin
    // a pagination cursor -- not part of the question. A link carrying one
    // would let a walk resume against a session that no longer exists, which is
    // the exact condition the restart guard exists to catch.
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    routersFixture = routers
    peersFixture = peers
    route.query = {}
    push.mockClear()

    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()

    expect(push).toHaveBeenCalled()
    const q = push.mock.calls.at(-1)![0].query as Record<string, string>
    expect(q.router).toBe(routers.data[0].ip)
    expect(q.peer).toBe(peers.data[1].peer_ip)
    expect(Object.keys(q)).not.toContain('session')
    expect(JSON.stringify(q)).not.toContain(peers.data[1].session_id)
  })

  it('follows the URL when the back button changes the scope', async () => {
    // Same instance-reuse fact as the Looking glass: a query-only navigation
    // does not remount, so a scope read once at setup would leave the picker
    // and the table showing the previous scope while the URL names another.
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    const w = mountRoutes()
    await nextTick()
    expect((w.findAll('select')[1].element as HTMLSelectElement).value).toBe(peers.data[1].peer_ip)

    route.query = { router: routers.data[0].ip, peer: peers.data[0].peer_ip }
    await nextTick()
    await nextTick()

    expect((w.findAll('select')[1].element as HTMLSelectElement).value).toBe(peers.data[0].peer_ip)
  })

  it('lists each router once even when two collectors monitor it', async () => {
    // SYNTHESIZED, not captured: routers.json holds one router. api/openapi.yaml
    // documents /v1/routers as "one entry per (collector, router)", and
    // Router.collector's own description says outright that a router
    // monitored by two collectors "yields one entry per collector, not a
    // merged one." Built from routers.json's own row, duplicated under a
    // second collector -- same ip and sysname, since it is the same
    // physical router; only collector (and, plausibly, session_id) differ.
    scopeChosen = false
    restarted = false
    peersFixture = peers
    ribRows = rib.data
    const capturedRouter = routers.data[0]
    routersFixture = {
      data: [capturedRouter, { ...capturedRouter, collector: 'vantage-collector-1' }],
      meta: routers.meta,
    }
    const w = mountRoutes()
    const routerValues = w.findAll('select')[0].findAll('option').map((o) => o.element.value)
    expect(new Set(routerValues).size).toBe(routerValues.length)
  })

  it('says when two collectors disagree about the same peer instead of picking one silently', async () => {
    // SYNTHESIZED, not captured: this lab runs one collector, so no capture
    // shows two collectors watching the same peer. api/openapi.yaml
    // documents /v1/peers as one entry per (collector, router, peer, rib);
    // two collectors run independent BMP sessions against the same peer, so
    // session_id and state are real, independent facts per collector and
    // can disagree -- one session up while the other has gone view_lost.
    // /v1/rib/* takes no collector= parameter, so the walk cannot address
    // one collector's view over the other's. Built from peers.json's own
    // in_pre row, duplicated under a second collector with a different
    // session_id and state -- everything else (peer_ip, asn) is identical,
    // because it is the same real peer.
    scopeChosen = false
    restarted = false
    ribRows = rib.data
    const capturedRow = peers.data[1]
    const secondCollectorRow = {
      ...capturedRow,
      collector: 'vantage-collector-1',
      session_id: '1788722065877779999',
      state: 'view_lost',
    }
    peersFixture = { data: [...peers.data, secondCollectorRow], meta: peers.meta }
    routersFixture = routers
    const w = mountRoutes()
    const selects = w.findAll('select')
    await selects[0].setValue(capturedRow.router_ip)
    // Still one <option> for the peer -- the walk can only address peer_ip,
    // not (peer_ip, collector) -- but it must not stay silent about the
    // disagreement it just papered over.
    const peerOptions = selects[1].findAll('option')
    const matching = peerOptions.filter((o) => o.element.value === capturedRow.peer_ip)
    expect(matching).toHaveLength(1)
    expect(matching[0].text()).toMatch(/disagree/i)

    // A dedicated banner, not just the option label re-surfacing: an
    // operator who has already opened the peer select and closed it again
    // never re-reads its option text, so the warning has to live somewhere
    // else once the ambiguous peer is actually chosen. `.ambiguous` is that
    // banner; asserting against it (rather than `w.text()`, which would
    // still contain the option's own label regardless of whether any
    // banner rendered) is what makes this assertion fail if the banner is
    // ever removed while the option label is left standing.
    await selects[1].setValue(capturedRow.peer_ip)
    expect(w.find('.ambiguous').exists()).toBe(true)
    expect(w.find('.ambiguous').text()).toMatch(/disagree/i)
  })

  /**
   * Two collectors that AGREE must not be reported as disagreeing.
   *
   * The test above synthesizes a second collector differing in BOTH
   * session_id and state, so it passes whether the predicate behind the
   * banner tests one, the other, or their disjunction -- it cannot tell a
   * correct implementation from a broken one. This is the row that takes
   * the other side.
   *
   * `session_id` is minted by each collector independently, so two
   * collectors watching one peer ALWAYS report two of them even when they
   * agree completely. Counting distinct session_ids therefore detects
   * "a second collector exists", never "they disagree" -- and the banner
   * asserted the second. Live on a lab deployment at
   * /routes?router=172.22.0.11&peer=10.255.1.1, where both collectors say
   * `up` about one in_pre session, it rendered "Two collectors monitor this
   * peer and disagree on its session or state" in a role="alert".
   *
   * Needing to CHOOSE a collector and the collectors DISAGREEING are
   * different facts, and only the first is true here.
   */
  it('does not claim two agreeing collectors disagree', async () => {
    scopeChosen = false
    restarted = false
    ribRows = rib.data
    const capturedRow = peers.data[1]
    // Everything a collector can legitimately differ on is identical; only
    // the collector-minted id differs, exactly as the live archive shows.
    const agreeingSecondCollector = {
      ...capturedRow,
      collector: 'vantage-collector-1',
      session_id: '1788722065877779999',
    }
    peersFixture = { data: [...peers.data, agreeingSecondCollector], meta: peers.meta }
    routersFixture = routers
    const w = mountRoutes()
    const selects = w.findAll('select')
    await selects[0].setValue(capturedRow.router_ip)

    const peerOptions = selects[1].findAll('option')
    const matching = peerOptions.filter((o) => o.element.value === capturedRow.peer_ip)
    expect(matching).toHaveLength(1)
    expect(matching[0].text()).not.toMatch(/disagree/i)

    await selects[1].setValue(capturedRow.peer_ip)
    // The banner may still appear -- a collector must be chosen, and saying
    // so is useful -- but it must not assert a disagreement that did not
    // happen.
    const banner = w.find('.ambiguous')
    if (banner.exists()) expect(banner.text()).not.toMatch(/disagree/i)
  })

  it('clears the scope when a complete pick becomes incomplete, rather than leaving a stale answer on screen', async () => {
    // Reproduced live: reconsidering the router (or peer) after
    // a complete pair was chosen left the old peer's rows and ResultMeta
    // footer rendered verbatim while the dropdowns no longer agreed with
    // them. ScopePicker's watch emitted a scope only on a complete, matching
    // pair, and never told RoutesView the pair had stopped being complete --
    // so RoutesView's own `scope` ref, set once inside onScope, just sat at
    // whatever it was: meta outliving a scope change, an answer for a
    // question no longer being asked. Minimal repro: resetting the peer back to "--" is the same
    // "pair no longer complete" transition as changing the router away from
    // the one the peer was chosen under -- ScopePicker's watch treats both
    // uniformly.
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    expect(w.text()).toMatch(/still loading|counts will rise/i)

    await w.findAll('select')[1].setValue('')
    expect(w.text()).toMatch(/choose a router and peer/i)
    expect(w.find('tbody tr').exists()).toBe(false)
  })
  // --- Layout: an eyebrow, title, table, per-row action and footer ---
  //
  // This screen has an eyebrow naming the scope, an H1, right-aligned
  // counts, a fixed-width table, a per-row Detail affordance and a
  // footer readout. What is NOT here is equally deliberate: no RPKI
  // column (nothing in the pipeline validates), no best-path star (no
  // such flag on the shape), no as-path regex field (/v1/rib/* takes no
  // regex), no "Entries" total (the walk's meta carries total_matched:
  // null).

  it('names the scope in an eyebrow above the title, taken from the answer itself', async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()
    const eyebrow = w.find('[data-eyebrow]')
    expect(eyebrow.exists()).toBe(true)
    // The sysname, not the router address: rib-unicast.json's rows carry
    // router_sysname, and a screen that had only the IP would be throwing
    // away a name the answer already gave it.
    expect(eyebrow.text()).toContain(rib.data[0].router_sysname)
    expect(eyebrow.text()).toContain(rib.data[0].peer_ip)
  })

  it('counts the rows it has loaded and claims no total the answer never carried', async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()
    expect(w.find('[data-count-loaded]').text()).toContain(String(rib.data.length))
    // total_matched is null on this fixture, as it is on every RIB walk:
    // counting a keyset walk's matches would mean a second whole-table
    // read. An "Entries" total has nothing behind it here, and a slot
    // rendering 0 or "—" would read as a measurement.
    expect(rib.meta.total_matched).toBeNull()
    expect(w.find('[data-count-total]').exists()).toBe(false)
  })

  it("shows the standard communities a row carries, and not its other three community types", async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()
    const cell = w.find('[data-communities]')
    expect(cell.exists()).toBe(true)
    for (const c of rib.data[0].communities) expect(cell.text()).toContain(c)
    // large_communities, ext_communities and route_targets are different
    // BGP attributes, not longer spellings of this one. Folding all four
    // into one column would report a large community as a standard one.
    for (const c of rib.data[0].large_communities) expect(cell.text()).not.toContain(c)
  })

  it("opens the Looking glass on a row's own prefix from its Detail link", async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()
    const links = w.findAllComponents(RouterLinkStub)
    // This app has no prefix page; the Detail link goes to the Looking
    // glass's Paths and Changes tabs instead, since those are what a
    // prefix page would have contained. mode=prefix is explicit because
    // the Looking glass seeds its own mode from the query.
    expect(links.map((l) => l.props('to'))).toEqual(
      rib.data.map((r) => ({
        path: '/looking-glass',
        query: { mode: 'prefix', q: r.prefix },
      })),
    )
  })

  // The property that keeps the smear from coming back. Every column
  // declares a width, so the browser's auto layout never distributes
  // eight short values across a 2000px viewport.
  it('declares a width for every column, so the table cannot smear', async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()
    const widths = columnsOf(w).map((c) => (c as { width?: string }).width)
    expect(widths.every((x) => typeof x === 'string' && x.length > 0)).toBe(true)
  })

  // router.ts has no catch-all, so a destination matching no route renders a
  // blank main area -- no error, no 404. The per-destination tests above pin
  // this screen against literals THIS FILE composes, which proves the two
  // copies agree and nothing about whether either agrees with the route
  // table. Renaming router.ts's '/peers/:router/:peer' to '/peer/:router/
  // :peer' passed all 425 tests while making Peer detail unreachable from
  // every link in the app; router.test.ts skips ':param' routes by design,
  // so this is the guard for the ones it leaves out.
  it('points every link at a route the router actually declares', async () => {
    scopeChosen = true
    restarted = false
    peersFixture = peers
    routersFixture = routers
    ribRows = rib.data
    const w = mountRoutes()
    await chooseScope(w)
    await nextTick()
    const destinations = w.findAllComponents(RouterLinkStub).map((l) => l.props('to'))
    expect(destinations.length).toBeGreaterThan(0)
    expect(unresolvable(destinations)).toEqual([])
  })

  // --- AS holder names on the origin_asn column (@/lib/asname.ts) ---
  describe('AS holder names', () => {
    it('renders the bare ASN when the dataset is not loaded, and says why once per screen', async () => {
      // Two rows sharing one origin ASN, so a notice rendered per row
      // rather than once for the screen would show twice.
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      ribRows = [
        { ...rib.data[0], origin_asn: 65010 },
        { ...rib.data[0], origin_asn: 65010, path_id: 1 },
      ]
      asNamesFixture = {
        data: [{ asn: 65010, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: false },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      const rows = w.findAll('tbody tr')
      expect(rows).toHaveLength(2)
      for (const r of rows) expect(r.text()).toContain('65010')
      expect(w.find('[data-holder-name]').exists()).toBe(false)

      const notices = w.findAll('[data-asnames-notice]')
      expect(notices).toHaveLength(1)
      expect(notices[0].text()).toMatch(/not loaded/i)
    })

    it('renders the bare ASN for an ASN the dataset does not list, without claiming the dataset is missing', async () => {
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      ribRows = [{ ...rib.data[0], origin_asn: 65010 }]
      asNamesFixture = {
        data: [{ asn: 65010, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      expect(w.get('tbody tr').text()).toContain('65010')
      expect(w.find('[data-holder-name]').exists()).toBe(false)
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
    })

    it('renders the whole name, not a fragment before a separator', async () => {
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      const wholeName = 'ATT-INTERNET4 - AT&T Services, Inc. - Legal Successor'
      ribRows = [{ ...rib.data[0], origin_asn: 65010 }]
      asNamesFixture = {
        data: [{ asn: 65010, name: wholeName, country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      const names = w.findAll('[data-holder-name]').map((n) => n.text())
      expect(names.length).toBeGreaterThan(0)
      for (const n of names) expect(n).toBe(wholeName)
    })

    it('shows the dataset date beside the names', async () => {
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      const iso = '2026-09-18T10:49:00Z'
      ribRows = [{ ...rib.data[0], origin_asn: 65010 }]
      asNamesFixture = {
        data: [{ asn: 65010, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true, asnames_published: iso },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      const note = w.find('[data-asnames-date]')
      expect(note.exists()).toBe(true)
      expect(note.text()).toContain(formatClock(iso))
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
    })

    it('never sends origin_asn: null to the batch, and shows nothing for it', async () => {
      // rib-unicast.json's own captured rows both have origin_asn: null
      // (iBGP, an empty AS path) -- a fact about the route, not an ASN to
      // look up. One row with a REAL origin is added alongside them, and
      // both halves of this test's name are asserted directly:
      //
      //   1. the BATCH the screen handed useAsNames, read off the captured
      //      ref rather than inferred from the table. A null reaching that
      //      array goes out as an `asn=` value /v1/asnames refuses with a
      //      400, and one refusal blanks every name on the page -- damage
      //      no assertion about this screen's own DOM can see, because the
      //      names are missing either way.
      //   2. that the null row renders no holder name while the real one
      //      does, which is what makes the first assertion a filter rather
      //      than an empty batch.
      //
      // Verified by mutation: deleting `.filter(...)` from RoutesView.vue
      // fails assertion 1 with [null, null, 65010].
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      expect(rib.data[0].origin_asn).toBeNull()
      ribRows = [...rib.data, { ...rib.data[0], origin_asn: 65010, path_id: 7 }]
      asNamesFixture = {
        data: [{ asn: 65010, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      expect(capturedAsns).toBeDefined()
      expect(capturedAsns?.value).toEqual([65010])

      const named = w.findAll('[data-holder-name]')
      expect(named).toHaveLength(1)
      expect(named[0].text()).toBe('LEVEL3 - Level 3 Parent, LLC')
    })

    // The cap's honest half. RoutesView is the screen most likely to reach
    // it: useRibPage pages at 500 and ACCUMULATES, so a transit peer's RIB
    // passes 512 distinct origins within a click or two of "more". Past the
    // cutoff useAsNames keeps the lowest-numbered 512, so every ASN above
    // it renders bare -- identical, per row, to one the dataset genuinely
    // does not list -- while the date line goes on saying names are loaded.
    it('says so when the walk outgrew one lookup, rather than showing the rest as unlisted', async () => {
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      ribRows = [{ ...rib.data[0], origin_asn: 65010 }]
      asNamesTruncated = true
      asNamesFixture = {
        data: [{ asn: 65010, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: {
          warnings: [],
          total_matched: null,
          asnames_loaded: true,
          asnames_published: '2026-09-18T10:49:00Z',
        },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      const line = w.findAll('[data-asnames-truncated]')
      expect(line).toHaveLength(1)
      expect(line[0].text()).toMatch(/512/)
      expect(line[0].text()).toMatch(/lowest-numbered/i)
      // It must not claim the dataset is missing: it is loaded, and a
      // truncated name is not an absent one.
      expect(line[0].text()).not.toMatch(/not loaded/i)
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
      // A SECOND line, not a replacement: the date is still true and still
      // shown, and this is what keeps it from being the whole story.
      expect(w.find('[data-asnames-date]').exists()).toBe(true)
    })

    // Truncated AND no dataset at all. The two siblings that carry this
    // screen's AS-name block (PeersView, TopologyView) each guard this
    // combination and RoutesView did not, which matters here more than
    // either of them: RoutesView is the screen that actually reaches the
    // cap, so it is the most likely place for both conditions to be true
    // at once. "Not loaded" is the whole story -- a second sentence about a
    // lookup that would have found nothing anyway only competes with the
    // one an operator can act on.
    //
    // asnames_published is OMITTED rather than null: the daemon carries
    // omitempty on that field, so nil means the key is ABSENT from the JSON,
    // and the contract says to read its presence, not its value.
    it('says only that the dataset is missing when it is, even if the walk was also cut', async () => {
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      ribRows = [{ ...rib.data[0], origin_asn: 65010 }]
      asNamesTruncated = true
      asNamesFixture = {
        data: [{ asn: 65010, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: false },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      expect(w.findAll('[data-asnames-notice]')).toHaveLength(1)
      expect(w.find('[data-asnames-truncated]').exists()).toBe(false)
      // And no date line either: there is no date, and the notice is not a
      // second line beside one.
      expect(w.find('[data-asnames-date]').exists()).toBe(false)
    })

    it('says nothing about truncation when the batch fit, so the line means something', async () => {
      scopeChosen = true
      restarted = false
      peersFixture = peers
      routersFixture = routers
      ribRows = [{ ...rib.data[0], origin_asn: 65010 }]
      asNamesTruncated = false
      asNamesFixture = {
        data: [{ asn: 65010, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: {
          warnings: [],
          total_matched: null,
          asnames_loaded: true,
          asnames_published: '2026-09-18T10:49:00Z',
        },
      }
      const w = mountRoutes()
      await chooseScope(w)
      await nextTick()

      expect(w.find('[data-asnames-truncated]').exists()).toBe(false)
      expect(w.find('[data-asnames-date]').exists()).toBe(true)
    })
  })
})

describe('RoutesView, collector axis', () => {
  // A false session-fallback advisory: on a router two collectors
  // monitor, this screen rendered an advisory promising "this walk uses
  // <collector>'s session" directly above the API's refusal to walk at
  // all, and ROWS LOADED: 0. The advisory described a fallback the code
  // never performed.
  //
  // /v1/rib/* now takes collector=, so the choice is real and the screen
  // has to offer it rather than narrate an arbitrary resolution.
  //
  // Nothing here is captured: the lab deployment that produced
  // peers.json ran one collector. Two collectors watching one peer is
  // documented by api/openapi.yaml (/v1/peers is one entry per
  // (collector, router, peer, rib)) and is now a real deployment shape.
  function twoCollectorPeers() {
    const base = peers.data[1]
    return {
      data: [
        { ...base, collector: 'coll-a', session_id: '111', state: 'up' },
        { ...base, collector: 'coll-b', session_id: '222', state: 'up' },
      ],
      meta: peers.meta,
    }
  }

  it('offers the collector as a choice when two of them watch the peer', async () => {
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    const base = peers.data[1]
    peersFixture = twoCollectorPeers()
    routersFixture = routers

    const w = mountRoutes()
    const selects = w.findAll('select')
    await selects[0].setValue(base.router_ip)
    await selects[1].setValue(base.peer_ip)
    await nextTick()

    const collectorSelect = w.find('select[data-collector]')
    expect(
      collectorSelect.exists(),
      'a peer two collectors watch must offer which one to walk; without it ' +
        'the screen can only narrate the ambiguity it cannot resolve',
    ).toBe(true)
    const values = collectorSelect.findAll('option').map((o) => o.element.value)
    expect(values).toContain('coll-a')
    expect(values).toContain('coll-b')
  })

  it('walks the collector that was chosen, with that collector own session', async () => {
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    const base = peers.data[1]
    peersFixture = twoCollectorPeers()
    routersFixture = routers

    const w = mountRoutes()
    const selects = w.findAll('select')
    await selects[0].setValue(base.router_ip)
    await selects[1].setValue(base.peer_ip)
    await nextTick()
    await w.find('select[data-collector]').setValue('coll-b')
    await nextTick()

    expect(capturedRibScope?.value?.collector).toBe('coll-b')
    expect(
      capturedRibScope?.value?.session,
      "the session must be the CHOSEN collector's -- a session_id is minted " +
        'by one collector and means nothing under another',
    ).toBe('222')
  })

  it('does not promise a fallback it cannot perform', async () => {
    scopeChosen = true
    restarted = false
    ribRows = rib.data
    const base = peers.data[1]
    peersFixture = twoCollectorPeers()
    routersFixture = routers

    const w = mountRoutes()
    const selects = w.findAll('select')
    await selects[0].setValue(base.router_ip)
    await selects[1].setValue(base.peer_ip)
    await nextTick()

    const banner = w.find('.ambiguous')
    expect(banner.exists()).toBe(true)
    expect(
      banner.text(),
      'the banner must describe the choice on offer, not a fallback like ' +
        '"this walk uses X\'s session"',
    ).not.toMatch(/this walk uses/i)

    // Characterization, pinned here before ScopePicker learned that its
    // consumers differ: this screen IS collector-pinned -- /v1/rib/* took
    // collector= on 2026-09-20 -- so naming the one being walked is a true
    // statement here, and the control that switches it has to stay. Session
    // history and Topology drive endpoints with no collector= at all, and
    // this sentence would be false on either.
    expect(banner.text()).toMatch(/walking coll-a/i)
    expect(w.find('select[data-collector]').exists()).toBe(true)
  })
})
