import { RouterLinkStub, mount } from '@vue/test-utils'
import { unresolvable } from '@/test-support/routeResolution'
import { describe, expect, it, vi } from 'vitest'
import { type Ref, nextTick, reactive, ref } from 'vue'
import peers from '@/api/fixtures/peers.json'
import events from '@/api/fixtures/events.json'
import twoCollectors from '@/api/fixtures/events-two-collectors.json'
import flapping from '@/api/fixtures/events-flapping-peer.json'
import ResultMeta from '@/components/ResultMeta.vue'

// Set inside each test below, never left at these initial values: every
// test must pass alone via `-t`, so each assigns fixture/pending/loading
// itself before mounting.
let fixture: unknown = peers
let pending = false
let loading = false
// Captured on every usePeers() call so a test can assert what routerIp
// *actually was* at the moment usePeers ran, independent of what the mocked
// data happens to contain -- see "does not go stale" below.
let capturedRouterIp: { value: string } | undefined
// The peer's own event history, which is where every session fact on this
// screen comes from: /v1/events scoped to (router, peer). Rows are the
// captured fixture unless a test narrows them.
let eventRows: unknown[] = events.data
let capturedSince: { value: string } | undefined
// useEvents is a cursor WALK: it fetches nothing until reload() is called
// (SessionHistoryView calls it the moment a scope resolves). A screen that
// never calls it renders "no peer event in the last 24 hours" for a peer
// with events -- and a mock that hands back rows regardless lets every test
// pass. Counting the calls is what makes the mock describe the contract
// instead of hiding it.
let reloadCalls = 0
vi.mock('@/api/queries', () => ({
  usePeers: (routerIp: { value: string }) => {
    capturedRouterIp = routerIp
    return {
      data: ref(fixture),
      isPending: ref(pending),
      isLoading: ref(loading),
      error: ref(undefined),
    }
  },
  useEvents: (_scope: unknown, since: { value: string }) => {
    capturedSince = since
    return {
      rows: ref(eventRows),
      meta: ref(events.meta),
      loading: ref(false),
      error: ref(undefined),
      hasMore: ref(false),
      loadMore: () => {},
      reload: () => {
        reloadCalls += 1
      },
    }
  },
  // The two churn panels this screen draws, both SCOPED to this peer. The
  // scope is captured rather than assumed: an unscoped call returns the
  // fleet's churn, which would render another peer's activity under this
  // peer's name and look entirely plausible.
  useCollectionChurn: (_since: unknown, _bucket: unknown, scope?: Ref<unknown>) => {
    capturedChurnScope = scope
    return {
      data: ref({ data: churnBuckets, meta: { warnings: [], churn_bucket: '30m0s' } }),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(undefined),
    }
  },
  useCollectionChurnPrefixes: (_since: unknown, scope: Ref<unknown>) => {
    capturedPrefixScope = scope
    return {
      data: ref({ data: prefixChurn, meta: { warnings: [] } }),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(undefined),
    }
  },
}))

// A real Vue Router keeps one reactive route object for the app's whole
// life and mutates its `params` in place on navigation -- it does not hand
// each navigation a fresh object, which is exactly why a component matched
// to the SAME route record (/peers/:router/:peer to a different
// /peers/:router/:peer) is reused rather than remounted. `reactive(...)`
// here reproduces that mechanism; a fixed object literal would not let the
// "does not go stale" test below mutate params on an already-mounted
// instance the way a real navigation does. Every test sets `route.params`
// itself before mounting, same isolation rule as fixture/pending/loading.
const route = reactive({ params: { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip } })
// Spread over the real module rather than replaced outright: the screen only
// needs useRoute stubbed, but a wholesale factory also deletes createRouter,
// and '@/test-support/routeResolution' imports the real router.ts to check
// that this screen's links resolve against the real route table. The stubs
// below come after the spread, so they still win.
vi.mock('vue-router', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-router')>()),
  useRoute: () => route,
  RouterLink: { template: '<a><slot /></a>' },
}))

// The scope refs each churn composable was handed, read rather than
// inferred: both answers render plausibly whether or not they were scoped.
let capturedChurnScope: Ref<unknown> | undefined
let capturedPrefixScope: Ref<unknown> | undefined

// SYNTHESIZED, labeled: this lab's archive inside a 24h window is four rows
// from one peer, which draws neither a chart nor a ranking.
const churnBuckets = [
  { ts: '2026-09-17T12:00:00Z', readvertise: 40, withdraw: 6, dump: 120 },
  { ts: '2026-09-17T12:30:00Z', readvertise: 12, withdraw: 0, dump: 0 },
]

// Ordered as the endpoint ranks -- by changes, not by observations -- so a
// panel that re-sorted on the bigger number would visibly disagree.
const prefixChurn = [
  { prefix: '10.10.1.0/24', observations: 30, readvertise: 20, withdraw: 4, dump: 6, routes: 2, sessions: 1 },
  { prefix: '10.10.2.0/24', observations: 90, readvertise: 2, withdraw: 1, dump: 87, routes: 1, sessions: 9 },
]

const PeerDetailView = (await import('./PeerDetailView.vue')).default

// RouterLink stubbed for the reason every other view test in this repo
// stubs it: which paths exist is router.ts's concern, and an unresolved
// component warns into the pristine-console bar on every mount.
function mountDetail() {
  // RouterLinkStub rather than `true`: the link into Session history has to
  // carry this peer's own scope, and the boolean stub discards the `to`.
  return mount(PeerDetailView, { global: { stubs: { RouterLink: RouterLinkStub } } })
}

describe('PeerDetailView', () => {
  it('shows only tiles an endpoint answers, never invented telemetry', () => {
    // This screen draws six tiles. Prefixes/s and updates/s have no field
    // in any response, and "UPTIME 61d established" is a duration a lab
    // archive cannot support for a session older than the window -- so the
    // row carries what is answerable and nothing else. Rendering
    // placeholders for the rest would put invented telemetry on an incident
    // screen.
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()
    const keys = w.findAll('[data-tile]').map((t) => t.attributes('data-tile'))
    expect(keys).toEqual(['routes', 'asn', 'sessions', 'flaps', 'view-lost', 'up-since'])
    // `flaps` left this list on 2026-09-21: peer_events now answers it,
    // so it stopped being invented telemetry and became a derived one.
    // The other two have no field in any response and stay here.
    for (const invented of ['updates', 'prefixes/s']) {
      expect(w.text().toLowerCase()).not.toContain(invented)
    }
  })

  it("names each family's dump state rather than summarizing it", () => {
    // dump_states is per-family: one family complete and another dumping is
    // a normal, meaningful situation, and a single "dumping: yes" would
    // erase it.
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    const w = mountDetail()
    for (const family of Object.keys(peers.data[1].dump_states)) {
      expect(w.text()).toContain(family)
    }
  })

  // --- The isPending/isLoading regression RoutersView.vue already paid for ---
  it('does not blank an already-found peer while a background poll is in flight', () => {
    // isPending is the "nothing has ever arrived" signal; isLoading flips
    // true on every 30s tick. Wiring the wrong one here hid a found peer's
    // whole detail behind "loading..." once per tick.
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = true
    eventRows = events.data
    const w = mountDetail()
    expect(w.find('[data-blocking]').exists()).toBe(false)
    expect(w.findAll('[data-tile]').length).toBeGreaterThan(0)
  })

  it('blocks on the loading state before any response has ever landed', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = { data: [], meta: peers.meta }
    pending = true
    loading = true
    const w = mountDetail()
    expect(w.find('[data-blocking]').exists()).toBe(true)
  })

  // --- The .find() defect: it silently dropped every row but the first ---
  it('shows every rib a peer appears under, not just the first', () => {
    // api/openapi.yaml documents /v1/peers as "one entry per (collector,
    // router, peer, rib)" -- rib is a five-valued enum (in_pre, in_post,
    // out_pre, out_post, loc_rib), and a deployment watching both the
    // pre- and post-policy adj-rib for one session has two real rows for
    // the same (router, peer) pair. The PEER UNDER TEST appears only under
    // in_pre; the lab deployment is not in_pre-only, and saying so would
    // overstate what was captured in the opposite direction -- peers.json's
    // other row is a real loc_rib entry for the 0.0.0.0 self-peer. What no
    // capture holds is two ribs for ONE (router, peer), and capturing that
    // would mean a lab peer carrying an out_pre or loc_rib session
    // alongside its in_pre one.
    //
    // Synthesized here instead: peers.json's real captured in_pre row,
    // duplicated with only `rib` changed. Every other field -- asn, state,
    // dump_states, routes -- is exactly as captured; nothing about the
    // Peer shape itself is invented, only the fact that this deployment's
    // lab happens not to carry a second rib for it.
    const capturedRow = peers.data[1]
    const secondRibRow = { ...capturedRow, rib: 'out_pre' }
    route.params = { router: capturedRow.router_ip, peer: capturedRow.peer_ip }
    fixture = { data: [capturedRow, secondRibRow], meta: peers.meta }
    pending = false
    loading = false
    const w = mountDetail()
    expect(w.findAll('.rib-view')).toHaveLength(2)
    expect(w.text()).toContain(capturedRow.rib)
    expect(w.text()).toContain('out_pre')
  })

  // --- The routerIp defect: it went stale across a reused instance ---
  it('does not go stale when Vue Router reuses this instance for a different peer', () => {
    // Pins the computed-vs-ref fix directly. PeerDetailView.vue derives
    // routerIp with `computed(() => String(route.params.router))`; a
    // one-time `ref(String(route.params.router))` would copy
    // route.params.router once at setup and never notice a later
    // navigation change it, because Vue Router never remounts a component
    // matched to the same route record across a params-only navigation --
    // there is no fresh setup() call to tell it to re-read.
    //
    // The two router values below are arbitrary path segments, not peer
    // data -- this test is about route-param propagation into usePeers,
    // not about any claim on the Peer shape, so it does not need to be
    // fixture-sourced the way peer-content assertions do.
    route.params = { router: '10.0.0.80', peer: '172.31.0.90' }
    fixture = peers
    pending = false
    loading = false
    mountDetail()
    expect(capturedRouterIp?.value).toBe('10.0.0.80')

    // Same mounted instance, same route object -- mutated in place, the
    // way a real navigation to a different /peers/:router/:peer would be,
    // rather than a fresh mount() call.
    route.params = { router: '10.0.0.90', peer: '172.31.0.91' }
    expect(capturedRouterIp?.value).toBe('10.0.0.90')
  })
  // --- The collector axis, and the footer this screen never had ---

  it('names the collector each view came from, so two collectors do not read as one', async () => {
    // api/openapi.yaml documents /v1/peers as one entry per (collector,
    // router, peer, rib) -- FOUR axes. This screen keyed its sections on
    // `rib` and labeled them with `rib`, so two collectors watching the
    // same (router, peer, rib) rendered two visually identical sections
    // under a duplicate key, with the `collector` field sitting unused on
    // the row. Two collectors run independent BMP sessions and their
    // state, session_id and route counts are independent facts that can
    // disagree, so which one a number came from is not decoration.
    //
    // SYNTHESIZED: this lab runs one collector, so no capture holds two.
    // Built from peers.json's own captured row with only `collector`
    // changed -- deliberately identical in every other field, because that
    // is the case the old code rendered as one section twice. Capturing it
    // would mean running a second collector against the same router.
    const captured = peers.data[1]
    route.params = { router: captured.router_ip, peer: captured.peer_ip }
    fixture = {
      data: [captured, { ...captured, collector: 'vantage-collector-1' }],
      meta: peers.meta,
    }
    pending = false
    loading = false
    const w = mountDetail()
    expect(w.findAll('.rib-view')).toHaveLength(2)
    const labels = w.findAll('.rib-label').map((l) => l.text())
    expect(new Set(labels).size).toBe(labels.length)
    expect(w.text()).toContain(captured.collector)
    expect(w.text()).toContain('vantage-collector-1')
  })

  it('shows the completeness footer every other screen shows', () => {
    // Every screen's footer states completeness; this screen had been
    // missing it. /v1/peers can answer with paginated_smear or truncated,
    // and this screen would have said nothing about either -- and
    // peers.json's own captured meta carries session_dumping, so the
    // silence was live rather than hypothetical.
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    const w = mountDetail()
    expect(w.findComponent(ResultMeta).exists()).toBe(true)
    expect(w.text()).toMatch(/still loading|counts will rise/i)
  })

  it('keeps the footer when no such peer was found', () => {
    // "no such peer on this router" and "a session is still loading its
    // initial view" are the difference between "it is not there" and "we
    // may not have looked at all of it". Dropping the footer on the empty
    // branch would answer the first when only the second is known.
    //
    // SYNTHESIZED PAIRING: peers.json's real captured meta beside an empty
    // data array. A lab deployment cannot produce an empty /v1/peers that
    // still reports a dumping session, because the dumping session IS one
    // of the rows it would return.
    route.params = { router: '10.0.0.99', peer: '172.31.0.99' }
    fixture = { data: [], meta: peers.meta }
    pending = false
    loading = false
    const w = mountDetail()
    expect(w.text()).toMatch(/no such peer/i)
    expect(w.findComponent(ResultMeta).exists()).toBe(true)
  })
  // --- Session facts, derived from events ---
  //
  // Every one of these is derived from the peer's OWN events -- the
  // /v1/events answer scoped to this (router, peer) -- so the screen gains
  // its tile row without a new endpoint. What it must not do is present
  // a windowed count as an all-time one: "flaps (24h)" is a 24h figure,
  // and this screen says which window it covers because the window is a
  // control, not a constant.
  it("derives the peer's session facts from its own events, and names the window", () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()

    const tile = (key: string) => w.find(`[data-tile="${key}"]`).text()

    // Three distinct session_ids in the captured history.
    const sessions = new Set(events.data.map((e) => e.session_id)).size
    expect(tile('sessions')).toContain(String(sessions))

    // Session changes are the events that ENDED a view: down (the router
    // said so) and view_lost (the collector went blind). up is not a
    // change, it is the state the others interrupt.
    // Split into its two honest halves on 2026-09-21. This capture is all
    // `up` and `view_lost` with no `down` at all, so the flap count is 0 and
    // every interruption in it is a collector going blind -- which is
    // exactly the distinction the single "Session changes" tile could not
    // draw.
    const viewLost = events.data.filter((e) => e.kind === 'view_lost').length
    expect(tile('view-lost')).toContain(String(viewLost))
    expect(w.find('[data-tile="flaps"] .value').text()).toBe('0')

    // The window is on screen, on the tiles themselves: a bare "3" invites
    // reading a lifetime count.
    expect(w.text()).toMatch(/last 24 hours|24h/i)
    expect(capturedSince?.value).toBe('24h')
  })

  /**
   * FLAPS 24h, counted from the peer's own events.
   *
   * A flap is a COMPLETED cycle: the session went down and came back. A
   * session that went down and stayed down is an outage, not a flap, and
   * the State pill and Up since are what say so -- counting it here would
   * make "4 flaps" and "down since Tuesday" the same number.
   *
   * The fixture is captured, not synthesized: `/v1/events` scoped to
   * (10.0.103.67, 10.255.1.1) over a 1100h window, which is the only real
   * flapping peer in the archive it was captured from -- nx-leaf1, four clean down/up
   * cycles inside ONE session on 2026-08-10. A scoped window earns the
   * exemption from max_unscoped_since, which is why this capture is
   * possible at all and the withdrawals one is not.
   */
  it('counts a completed down-and-back cycle as a flap, from a real flapping peer', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = flapping.data
    const w = mountDetail()

    // The capture really does hold four cycles in one session -- if a
    // recapture flattens it, this fails loudly rather than asserting 0 == 0.
    expect(flapping.data.filter((e) => e.kind === 'down')).toHaveLength(4)
    expect(new Set(flapping.data.map((e) => e.session_id)).size).toBe(1)

    expect(w.find('[data-tile="flaps"] .value').text()).toBe('4')
  })

  /**
   * The two edges that separate "flap" from "down".
   *
   * Without these the implementation could count `down` events and pass the
   * capture above, which has an `up` after every one of them.
   */
  it('does not count a session that went down and stayed down', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    const [up, down] = [flapping.data[8], flapping.data[7]]
    // Ascending: the session comes up, then goes down and never returns.
    eventRows = [{ ...down, ts_collector: '2026-08-10T18:00:00Z' }, up]
    const w = mountDetail()
    expect(w.find('[data-tile="flaps"] .value').text()).toBe('0')
  })

  it('counts two downs with no up between them as one flap, not two', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    const base = flapping.data[0]
    // A session cannot go down twice without coming up in between; a second
    // `down` is a repeat of the state, not a new cycle.
    eventRows = [
      { ...base, kind: 'up', ts_collector: '2026-08-10T16:00:00Z' },
      { ...base, kind: 'down', ts_collector: '2026-08-10T16:01:00Z' },
      { ...base, kind: 'down', ts_collector: '2026-08-10T16:02:00Z' },
      { ...base, kind: 'up', ts_collector: '2026-08-10T16:03:00Z' },
    ]
    const w = mountDetail()
    expect(w.find('[data-tile="flaps"] .value').text()).toBe('1')
  })

  /**
   * view_lost is the collector losing its own transport while the router
   * said nothing, so it is not the router flapping and must not be counted
   * as one. It gets its own tile instead: down and view_lost are never
   * summed together.
   */
  it('keeps the collector losing view out of the flap count, and names it separately', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = twoCollectors.data
    const w = mountDetail()
    expect(w.find('[data-tile="flaps"] .value').text()).toBe('0')
    // One view lost, reported by both collectors -- best vantage, not two.
    expect(w.find('[data-tile="view-lost"] .value').text()).toBe('1')
  })

  /**
   * Neither tile may grow because a second collector watched.
   *
   * The test above uses a single-collector capture, so it cannot see this:
   * it computes its own expectation with the same `new Set(...).size` and
   * `filter(...).length` the screen used, and both were wrong in the same
   * way. `events-two-collectors.json` is one (router, peer) whose session
   * dev-c1 and dev-c2 BOTH observed -- each with its own `up` and
   * `view_lost`, and each having minted its OWN session_id for the one
   * underlying session.
   *
   * The truth is one session and one view lost. Shipped, this screen
   * rendered 2 and 2 in a browser at /peers/172.22.0.11/10.255.1.1:
   * counting distinct session_ids double-counts a session two collectors
   * both observed. Session identity cannot fix it: the two ids differ
   * precisely because each collector minted one.
   */
  it('counts sessions once however many collectors watched', () => {
    // The peers fixture supplies the peer row the screen renders around;
    // the events mock hands back `eventRows` regardless of scope, so the
    // two-collector history is what the tiles count. Same split the sibling
    // test above relies on.
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = twoCollectors.data
    const w = mountDetail()

    // The VALUE, not the tile's whole text: the label reads "· last 24
    // hours", so a `not.toContain('2')` over the tile would fail on the 24
    // and pass for the wrong reason in the other direction.
    const value = (key: string) => w.find(`[data-tile="${key}"] .value`).text()

    // The fixture is genuinely doubled -- if this stops holding, the fixture
    // was recaptured against a single-collector archive and the assertions
    // below would pass without testing anything.
    expect(new Set(twoCollectors.data.map((e) => e.collector)).size).toBe(2)
    expect(new Set(twoCollectors.data.map((e) => e.session_id)).size).toBe(2)

    // Sessions only: the view-lost half of this fixture is asserted by
    // "keeps the collector losing view out of the flap count" above, and
    // repeating it here would be two tests failing for one cause.
    expect(value('sessions')).toBe('1')
  })

  // An established duration like "UPTIME 61d" is not shown: this screen
  // can say when the current view began only if an `up` for it is inside
  // the window -- and if it is not, the honest answer is nothing, not 0
  // and not a dash that reads as "zero seconds".
  it('says when the peer came up only when an up event is in the window', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    expect(mountDetail().find('[data-tile="up-since"]').exists()).toBe(true)

    eventRows = events.data.filter((e) => e.kind !== 'up')
    const w = mountDetail()
    expect(w.find('[data-tile="up-since"]').exists()).toBe(false)
    // And it does not quietly become a zero somewhere else on the strip.
    expect(w.text()).not.toMatch(/up since\s*(0|—)/i)
  })

  it("shows the peer's recent events, with a way to the full history", () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()
    const panel = w.find('[data-section="history"]')
    expect(panel.exists()).toBe(true)
    expect(panel.findAll('li').length).toBeGreaterThan(0)
    // The link carries THIS peer's scope, not a bare path: Session history
    // seeds its pickers from router= and peer=, and a link without them
    // would land an operator on "choose a router and peer" after clicking
    // through from one.
    const link = panel.findComponent(RouterLinkStub)
    expect(link.props('to')).toEqual({
      path: '/session-history',
      query: { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip },
    })
  })

  // Real fields on the event shape, and the only place in the UI that shows
  // them: which local address and ports this BMP-reported session runs
  // between. This panel shows the peer's side of the session only.
  it('states the session endpoints the events carry, not an invented pair', () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()
    const facts = w.find('[data-section="session"]').text()
    expect(facts).toContain(events.data[0].local_ip)
  })
  // The defect the browser found and every test above missed: useEvents
  // fetches on reload(), not on a scope change, so a screen that only sets
  // the scope shows an empty history forever. The mock cannot express that
  // -- it hands back rows either way -- so the contract is asserted
  // directly: the walk is told to fetch once the peer is known.
  it('starts the event walk once the peer is known, rather than waiting for a fetch nothing triggers', async () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    reloadCalls = 0
    mountDetail()
    await nextTick()
    expect(reloadCalls).toBeGreaterThan(0)
  })

  it('does not ask for a peer that is not in the answer', async () => {
    // No matching row means no scope, and a walk reloaded against an
    // undefined scope would fire /v1/events with no router or peer, which
    // that endpoint refuses with a 400.
    route.params = { router: '10.99.99.99', peer: '10.99.99.98' }
    fixture = peers
    pending = false
    loading = false
    eventRows = []
    reloadCalls = 0
    mountDetail()
    await nextTick()
    expect(reloadCalls).toBe(0)
  })

  // router.ts has no catch-all, so a destination matching no route renders a
  // blank main area -- no error, no 404. The per-destination tests above pin
  // this screen against literals THIS FILE composes, which proves the two
  // copies agree and nothing about whether either agrees with the route
  // table. Renaming router.ts's '/peers/:router/:peer' to '/peer/:router/
  // :peer' passed all 425 tests while making Peer detail unreachable from
  // every link in the app; router.test.ts skips ':param' routes by design,
  // so this is the guard for the ones it leaves out.
  it('points every link at a route the router actually declares', () => {
    // Same setup as the history-link test above: the one link this screen
    // renders lives in the history panel, which needs both a matching peer
    // row and events to draw.
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()
    const destinations = w.findAllComponents(RouterLinkStub).map((l) => l.props('to'))
    expect(destinations.length).toBeGreaterThan(0)
    expect(unresolvable(destinations)).toEqual([])
  })
})

// --- The two churn panels ---
describe('PeerDetailView churn panels', () => {
  // Scoped to THIS peer, on both panels. An unscoped churn answer is the
  // fleet's, and rendered under one peer's heading it would look entirely
  // plausible -- a chart of somebody else's activity with this peer's name
  // on it. Reading the scope ref the composable was handed is the only way
  // to tell, since the mocked answer is identical either way.
  it('asks both churn answers for this peer, never the fleet', async () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    mountDetail()
    await nextTick()

    expect(capturedChurnScope?.value).toEqual({
      router: peers.data[1].router_ip,
      peer: peers.data[1].peer_ip,
    })
    expect(capturedPrefixScope?.value).toEqual({
      router: peers.data[1].router_ip,
      peer: peers.data[1].peer_ip,
    })
  })

  // The ranking is the ANSWER's order. 10.10.2.0/24 has three times the
  // observations of the row above it and belongs second, because 87 of its
  // 90 rows are session dumps -- a session that restarted, not a prefix that
  // churned. A panel that sorted on `observations` would swap them.
  it('keeps the ranking the answer carried, not the bigger number', async () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()
    await nextTick()

    const rows = w.findAll('[data-section="churn-prefixes"] tbody tr')
    expect(rows.length).toBe(prefixChurn.length)
    expect(rows[0].text()).toContain('10.10.1.0/24')
    expect(rows[1].text()).toContain('10.10.2.0/24')
  })

  // The chart is drawn at the width the ANSWER reports, never the one the
  // request asked for: the two agree until a clamp disagrees, and a chart
  // labeled with a width it was not drawn at is the same defect as one
  // labeled with a window it does not cover.
  it('labels the chart with the width the answer reported', async () => {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = peers
    pending = false
    loading = false
    eventRows = events.data
    const w = mountDetail()
    await nextTick()
    expect(w.find('[data-section="churn"]').text()).toContain('30m0s')
  })
})

// --- The negotiated session facts ---
describe('PeerDetailView session facts', () => {
  function mountWithPeer(extra: Record<string, unknown>) {
    route.params = { router: peers.data[1].router_ip, peer: peers.data[1].peer_ip }
    fixture = { ...peers, data: [{ ...peers.data[1], ...extra }] }
    pending = false
    loading = false
    eventRows = events.data
    return mountDetail()
  }

  // The families the two speakers NEGOTIATED, which is a stronger claim than
  // the families this peer happens to hold routes in -- a family negotiated
  // and carrying nothing appears here and in no route count.
  it('states the negotiated families, not the families with routes', async () => {
    const w = mountWithPeer({
      mp_families: ['ipv4u', 'vpn4', 'evpn'],
      addpath_families: ['ipv4u'],
      hold_time: 180,
      sys_descr: 'FRRouting 10.3_git',
    })
    await nextTick()
    const facts = w.find('[data-section="session"]').text()
    expect(facts).toContain('ipv4u')
    expect(facts).toContain('vpn4')
    expect(facts).toContain('evpn')
  })

  // A hold time of 0 is REAL -- RFC 4271: the timer never expires,
  // keepalives off for this session. It must not render as "unknown", and it
  // must not render as a bare 0 either, which reads as a missing value.
  it('renders a negotiated zero hold time as a fact, never as unknown', async () => {
    const w = mountWithPeer({ hold_time: 0, mp_families: ['ipv4u'], addpath_families: [] })
    await nextTick()
    const hold = w.find('[data-hold-time]')
    expect(hold.exists()).toBe(true)
    expect(hold.text().toLowerCase()).not.toContain('unknown')
    expect(hold.text().toLowerCase()).toMatch(/keepalive|never expires|disabled/)
  })

  // null is the other case and must say so rather than showing 0. A row
  // with no OPEN observed is this, and so is every peer whose only event
  // is a Down.
  it('says no OPEN was observed rather than showing a zero hold time', async () => {
    const w = mountWithPeer({ hold_time: null, mp_families: [], addpath_families: [] })
    await nextTick()
    const hold = w.find('[data-hold-time]')
    expect(hold.exists()).toBe(true)
    expect(hold.text()).not.toMatch(/\b0\b/)
    expect(hold.text().toLowerCase()).toMatch(/not observed|no open/)
  })

  // There is no keepalive beside the hold timer. BGP does not carry
  // one -- the interval is a local timer a speaker never announces -- so a
  // number here would be hold_time/3 wearing an observation's clothes. This
  // is the guard that keeps it from being "helpfully" added later.
  it('shows no keepalive anywhere, because the protocol carries none', async () => {
    const w = mountWithPeer({ hold_time: 180, mp_families: ['ipv4u'], addpath_families: [] })
    await nextTick()
    expect(w.text().toLowerCase()).not.toContain('keepalive interval')
    expect(w.find('[data-keepalive]').exists()).toBe(false)
  })

  // What the router says it IS, verbatim. Never parsed into vendor/version
  // on screen: the raw string is what was observed.
  it("shows the router's own sysDescr, unparsed", async () => {
    const w = mountWithPeer({
      sys_descr: 'Cisco IOS XR Software, Version 24.1.1',
      mp_families: ['ipv4u'],
      addpath_families: [],
      hold_time: 180,
    })
    await nextTick()
    expect(w.find('[data-sys-descr]').text()).toBe('Cisco IOS XR Software, Version 24.1.1')
  })
})
