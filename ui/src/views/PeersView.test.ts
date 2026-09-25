import { RouterLinkStub, mount } from '@vue/test-utils'
import type { AsName, ChurnActivity, Meta } from '@/api/generated'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { type Ref, nextTick, reactive, ref } from 'vue'
import peers from '@/api/fixtures/peers.json'
import peersDown from '@/api/fixtures/peers-down.json'
import peersLost from '@/api/fixtures/peers-view-lost.json'
import peersUpSince from '@/api/fixtures/peers-up-since.json'
import peersStale from '@/api/fixtures/peers-stale.json'
import churnPeers from '@/api/fixtures/collection-churn-peers.json'
import { columnWidths, columnsOf, tableMinWidthPx } from '@/test-support/columnsOf'
import { inventedColumns } from '@/test-support/columnGuard'
import { unresolvable } from '@/test-support/routeResolution'
import { formatClock } from '@/lib/formatClock'

// Set inside each test below, never left at these initial values: the
// isolation rule requires every test to pass alone via `-t`, so each test
// assigns fixture/pending/loading itself before mounting rather than relying
// on what a previous test in file order happened to leave behind.
let fixture: unknown = peersDown
let pending = false
let loading = false
let err: Error | undefined

// AS holder names, kept apart from the peers fixture above: no test outside
// the describe block below cares about names, so its default -- a dataset
// loaded with no rows for it to say anything about -- must render exactly
// like no names feature existed at all, or every other test in this file
// would have to know about this one too.
// Whether the composable capped this screen's batch. usePeers answers one
// row per BGP session, so a real fleet stays well under the cap -- but the
// screen renders the rule rather than assuming its own bound, and this is
// how a test reaches the state that exercises it.
let asNamesTruncated = false

// The churn join's own state, reset per test like every other fixture here.
// The REAL shape the composable resolves with -- a keyed map PLUS the
// collectors whose own request failed. A mock that returned only the map
// would be a claim about the contract that the contract does not make, and
// the screen reads `failed` to decide between "archived nothing" and "no
// measurement".
let churnFixture:
  | {
      by: Map<string, { changes_per_second: number; activity?: ChurnActivity[] }>
      failed: string[]
      window?: { from?: string; to?: string; bucket?: string }
    }
  | undefined
let churnPending = false
let churnErr: Error | undefined
let capturedSince: Ref<string> | undefined
let capturedCollectors: Ref<string[]> | undefined

let asNamesFixture: { data: AsName[]; meta: Meta } | undefined = {
  data: [],
  meta: { warnings: [], total_matched: null, asnames_loaded: true },
}

// This screen reads its scope off the query string -- Monitor's router rows
// link here with ?router=<ip> -- so useRoute has to exist for a bare mount.
// Reactive rather than a plain object because one test below moves the
// query AFTER mount, which is the difference between a live scope and one
// frozen at mount time. Same shape SessionHistoryView.test.ts uses for the
// same reason.
const route = reactive({ query: {} as Record<string, string> })
// Spread over the real module rather than replaced outright: the screen only
// needs useRoute stubbed, but a wholesale factory also deletes createRouter,
// and '@/test-support/routeResolution' imports the real router.ts to check
// that this screen's links resolve against the real route table. The stubs
// below come after the spread, so they still win.
vi.mock('vue-router', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-router')>()),
  useRoute: () => route,
}))

// The ref instance the screen handed usePeers, not a copy of its value: a
// screen that read the query once and passed a dead ref would satisfy an
// equality check on the value alone.
let capturedRouter: Ref<string | undefined> | undefined
vi.mock('@/api/queries', () => ({
  usePeers: (router: Ref<string | undefined>) => {
    capturedRouter = router
    return {
      data: ref(fixture),
      isPending: ref(pending),
      isLoading: ref(loading),
      error: ref(err),
    }
  },
  useAsNames: () => ({
    data: ref(asNamesFixture),
    isPending: ref(false),
    isLoading: ref(false),
    error: ref(undefined),
    truncated: ref(asNamesTruncated),
  }),
  // The REAL shape: one Map keyed by churnKey, or undefined before the first
  // answer. Captured so a test can assert which collectors the screen asked
  // about -- the join is per collector and a screen that asked for one would
  // render the other's rows as if they had archived nothing.
  useChurnPeersByCollector: (since: Ref<string>, collectors: Ref<string[]>) => {
    capturedSince = since
    capturedCollectors = collectors
    return {
      data: ref(churnFixture),
      isPending: ref(churnPending),
      isLoading: ref(false),
      error: ref(churnErr),
    }
  },
  churnKey: (c: string, r: string, p: string) => `${c}|${r}|${p}`,
}))

const PeersView = (await import('./PeersView.vue')).default

// RouterLink needs no router installed to test what this screen renders --
// which paths exist is router.ts's concern, same reasoning AppShell.test.ts
// already applies to its own nav links. Left unstubbed, Vue logs "Failed to
// resolve component: RouterLink" on every mount here, which fails the
// pristine-console bar this project holds itself to.
function mountPeers(opts: Record<string, unknown> = {}) {
  return mount(PeersView, { global: { stubs: { RouterLink: true } }, ...opts })
}

describe('PeersView', () => {
  // The isolation rule this file already states for fixture/pending/loading
  // extends to the query: a scope left behind by one test would otherwise
  // narrow the next one's mount, and a test that only passes in file order
  // is not a test.
  beforeEach(() => {
    route.query = {}
    capturedRouter = undefined
    err = undefined
    asNamesFixture = {
      data: [],
      meta: { warnings: [], total_matched: null, asnames_loaded: true },
    }
    asNamesTruncated = false
    // The churn join's state resets here too, not only in the archived-rate
    // block below. These are module-scope lets and this file's own header
    // states the rule: every test must pass alone via `-t`. Left unreset,
    // the three other describes inherited whatever the last block happened
    // to leave -- harmless in file order today, a different mount state when
    // run alone.
    churnFixture = undefined
    churnPending = false
    churnErr = undefined
  })

  it('renders a distinct pill per state in one table', () => {
    // peers-down.json holds a down peer and an up peer in one response, so a
    // table that rendered them alike would be caught here rather than in
    // production.
    fixture = peersDown
    pending = false
    loading = false
    const w = mountPeers()
    const pills = w.findAll('.pill').map((p) => p.text())
    expect(new Set(pills).size).toBeGreaterThan(1)
  })

  it('does not render view_lost as down or as idle', () => {
    fixture = peersLost
    pending = false
    loading = false
    const w = mountPeers()
    expect(w.text()).toMatch(/view lost/i)
    expect(w.text()).not.toMatch(/idle/i)
  })

  it('renders no column the API does not measure', () => {
    // Column ids come from the real Peer shape, and no header claims a
    // metric that shape cannot support -- both halves, not just the id
    // check shipped first. Shallow-mounted so the columns array
    // reaching DataTable can be read directly off the stub's props, the
    // same technique RoutersView.test.ts uses for the same reason: no
    // hand-typed header-to-field mapping table to keep in sync.
    fixture = peersDown
    pending = false
    loading = false
    const w = mountPeers({ shallow: true })
    const columns = columnsOf(w)
    // The union of two CAPTURED shapes, never a row this test types out.
    // peers-down.json was captured 2026-09-06 and predates `up_since`, which
    // /v1/peers gained on 2026-09-21; peers-up-since.json is a capture from
    // after it. Widening the guard with a second real response keeps it
    // asking its own question -- is every column backed by a field some API
    // actually returned -- where a hand-written field would have made it
    // vacuous. Same construction MonitorView.test.ts uses for its joined row.
    // Three captured shapes now, because the table is a JOIN of three
    // answers: the two /v1/peers captures above, and one from
    // /v1/collection/churn/peers, which is where changes_per_second comes
    // from. Same construction MonitorView.test.ts uses for its own joined
    // row -- a column backed by a real field on SOME endpoint, never a field
    // a test typed out to make its own guard pass.
    const captured = {
      ...peersDown.data[0],
      ...peersUpSince.data[0],
      ...churnPeers.data[0],
    }
    expect(inventedColumns(columns, captured)).toEqual([])
  })

  it("marks a mid-dump peer's route count as provisional", () => {
    // peers.json's in_pre peer is captured mid-dump (ipv4u: dumping). Its
    // routes count is real but still climbing, and the marker is what stops
    // that number from reading as final.
    fixture = peers
    pending = false
    loading = false
    const w = mountPeers()
    expect(w.findAll('.provisional')).toHaveLength(1)
  })

  // --- The isPending/isLoading regression RoutersView.vue already paid for ---
  //
  // usePeers polls every 30s through the same pollWhileMounted seam
  // useRouters uses (queries.ts), which flips isLoading true on the FIRST
  // fetch and on every later poll tick alike. isPending is the signal that
  // means "no data has ever landed" and stays false for the rest of this
  // screen's life once the first response arrives. Wiring isLoading into
  // DataTable's `loading` prop here would repeat RoutersView's own shipped
  // bug: blanking an already-populated peers table on every poll tick.
  it('does not blank the table or contradict the footer while a background poll is in flight', () => {
    fixture = { data: [], meta: peersDown.meta }
    pending = false
    loading = true
    const w = mountPeers()
    expect(w.text()).not.toMatch(/loading/i)
    expect(w.text()).toMatch(/no rows matched/i)
  })

  it('blocks on the loading state before any response has ever landed', () => {
    fixture = undefined
    pending = true
    loading = true
    const w = mountPeers()
    expect(w.text()).toMatch(/loading/i)
    expect(w.text()).not.toContain(peersDown.data[0].peer_ip)
  })
  it('links each peer row to that peer\'s own detail screen', () => {
    // Pinned by nothing until now: deleting the RouterLink left the suite
    // green while the only route into /peers/:router/:peer disappeared --
    // PeerDetailView is reachable from this cell and nowhere else in the
    // app. Both params matter and both come off the ROW: a link built from
    // peer_ip alone would open some other router's peer of the same
    // address, which is a real collision here (api/openapi.yaml keys peers
    // by (collector, router, peer, rib), and 0.0.0.0 is a self-peer every
    // router has).
    //
    // RouterLinkStub rather than `stubs: { RouterLink: true }`, because the
    // point is the `to` this screen composed, and a boolean stub renders
    // the component away without keeping its props readable.
    fixture = peersDown
    pending = false
    loading = false
    const w = mount(PeersView, { global: { stubs: { RouterLink: RouterLinkStub } } })
    const links = w.findAllComponents(RouterLinkStub)
    expect(links.map((l) => l.props('to'))).toEqual(
      peersDown.data.map((p) => `/peers/${p.router_ip}/${p.peer_ip}`),
    )
    // And the link is the peer address itself, not a bare "open" affordance
    // sitting beside it.
    expect(links.map((l) => l.text())).toEqual(peersDown.data.map((p) => p.peer_ip))
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
    fixture = peersDown
    pending = false
    loading = false
    const w = mount(PeersView, { global: { stubs: { RouterLink: RouterLinkStub } } })
    const destinations = w.findAllComponents(RouterLinkStub).map((l) => l.props('to'))
    expect(destinations.length).toBeGreaterThan(0)
    expect(unresolvable(destinations)).toEqual([])
  })

  // --- The scope Monitor's router rows arrive with ---
  //
  // /v1/peers takes `router` and usePeers has always accepted it; until now
  // nothing drove it, and the two unassigned refs this screen used to hold
  // were removed for exactly that reason. The link is the driver, so the
  // filter is wired to the query string rather than to a control this
  // screen does not have.
  it('narrows the list to the router named in the query rather than answering fleet-wide', () => {
    route.query = { router: '10.0.103.74' }
    fixture = peersDown
    pending = false
    loading = false
    mountPeers()
    expect(capturedRouter?.value).toBe('10.0.103.74')
  })

  it('asks for no router when the query names none', () => {
    fixture = peersDown
    pending = false
    loading = false
    mountPeers()
    expect(capturedRouter?.value).toBeUndefined()
  })

  // The one that makes the two above non-vacuous: a screen that read
  // route.query once at setup passes both of them and fails here, and
  // usePeers keys its query on router.value (queries.ts), so a frozen ref
  // means a table that never refetches for the router the URL now names.
  it('follows a query change rather than freezing the scope at mount', async () => {
    fixture = peersDown
    pending = false
    loading = false
    mountPeers()
    expect(capturedRouter?.value).toBeUndefined()
    route.query = { router: '10.0.103.74' }
    await nextTick()
    expect(capturedRouter?.value).toBe('10.0.103.74')
  })

  // A narrowed list that does not say so is this project's own cardinal
  // error in miniature: the rows are complete for one router and look
  // complete for the fleet. The way back out is the nav's own Peers link,
  // which carries no query -- no second affordance invented here.
  it('says which router the list is narrowed to', () => {
    route.query = { router: '10.0.103.74' }
    fixture = peersDown
    pending = false
    loading = false
    const w = mountPeers()
    const note = w.find('[data-scope-note]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('10.0.103.74')
  })

  it('makes no narrowed claim when the list is the whole fleet', () => {
    fixture = peersDown
    pending = false
    loading = false
    const w = mountPeers()
    expect(w.find('[data-scope-note]').exists()).toBe(false)
  })

  // The note is a claim about rows that are on screen. When the request
  // failed there are none, and DataTable renders its error in their place
  // -- so a note gated on the query parameter alone prints "Showing the
  // peers of foo only" directly above "no such router", which is the exact
  // contradiction DataTable.vue's own error/empty branch exists to prevent.
  // The scope must be reported as narrowing an ANSWER, never a failure.
  it('makes no narrowed claim when the scoped request failed', () => {
    route.query = { router: 'no-such-router' }
    fixture = { data: [], meta: {} }
    pending = false
    loading = false
    err = new Error('no such router')
    const w = mountPeers()
    expect(w.text()).toContain('no such router')
    expect(w.find('[data-scope-note]').exists()).toBe(false)
  })

  // --- AS holder names on the asn column (@/lib/asname.ts) ---
  describe('AS holder names', () => {
    it('renders the bare ASN when the dataset is not loaded, and says why once per screen', () => {
      // peers-down.json's two rows share one ASN (65010), so a notice
      // rendered per row rather than once for the screen would show twice.
      fixture = peersDown
      pending = false
      loading = false
      asNamesFixture = {
        data: [{ asn: 65010, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: false },
      }
      const w = mountPeers()

      const rows = w.findAll('tbody tr')
      expect(rows).toHaveLength(peersDown.data.length)
      for (const r of rows) expect(r.text()).toContain('65010')
      expect(w.find('[data-holder-name]').exists()).toBe(false)

      const notices = w.findAll('[data-asnames-notice]')
      expect(notices).toHaveLength(1)
      expect(notices[0].text()).toMatch(/not loaded/i)
    })

    it('renders the bare ASN for an ASN the dataset does not list, without claiming the dataset is missing', () => {
      fixture = peersDown
      pending = false
      loading = false
      asNamesFixture = {
        data: [{ asn: 65010, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = mountPeers()

      const rows = w.findAll('tbody tr')
      for (const r of rows) expect(r.text()).toContain('65010')
      expect(w.find('[data-holder-name]').exists()).toBe(false)
      // The dataset IS loaded -- it answered "" for this ASN, a genuine
      // fact about it -- so nothing here may say the dataset is missing.
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
    })

    it('renders the whole name, not a fragment before a separator', () => {
      fixture = peersDown
      pending = false
      loading = false
      const wholeName = 'ATT-INTERNET4 - AT&T Services, Inc. - Legal Successor'
      asNamesFixture = {
        data: [{ asn: 65010, name: wholeName, country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true },
      }
      const w = mountPeers()

      const names = w.findAll('[data-holder-name]').map((n) => n.text())
      expect(names.length).toBeGreaterThan(0)
      for (const n of names) expect(n).toBe(wholeName)
    })

    it('shows the dataset date beside the names', () => {
      fixture = peersDown
      pending = false
      loading = false
      const iso = '2026-09-18T10:49:00Z'
      asNamesFixture = {
        data: [{ asn: 65010, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true, asnames_published: iso },
      }
      const w = mountPeers()

      const note = w.find('[data-asnames-date]')
      expect(note.exists()).toBe(true)
      expect(note.text()).toContain(formatClock(iso))
      // The two notices are mutually exclusive: a loaded dataset must never
      // also claim to be missing.
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
      // And nothing was cut short, so nothing says it was.
      expect(w.find('[data-asnames-truncated]').exists()).toBe(false)
    })

    // Past the cap the lookup keeps the lowest-numbered ASNs, so every one
    // above the cutoff renders bare -- identical to one the dataset does
    // not list -- while the date line still says names are loaded. The
    // screen has to say which of those two it is.
    it('says so when the batch was cut short, rather than showing the rest as unlisted', () => {
      fixture = peersDown
      pending = false
      loading = false
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
      const w = mountPeers()

      const line = w.findAll('[data-asnames-truncated]')
      expect(line).toHaveLength(1)
      expect(line[0].text()).toMatch(/512/)
      expect(line[0].text()).toMatch(/lowest-numbered/i)
      expect(line[0].text()).not.toMatch(/not loaded/i)
      // A SECOND line beside the date, not a replacement for it.
      expect(w.find('[data-asnames-date]').exists()).toBe(true)
      expect(w.find('[data-asnames-notice]').exists()).toBe(false)
    })

    // Truncated and no dataset at all: "not loaded" is the whole story,
    // and a second sentence about a lookup that would have found nothing
    // only competes with the one an operator has to act on.
    it('says only that the dataset is missing when it is, even if the batch was also cut', () => {
      fixture = peersDown
      pending = false
      loading = false
      asNamesTruncated = true
      asNamesFixture = {
        data: [{ asn: 65010, name: '', country: '' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: false },
      }
      const w = mountPeers()

      expect(w.findAll('[data-asnames-notice]')).toHaveLength(1)
      expect(w.find('[data-asnames-truncated]').exists()).toBe(false)
    })
  })
})

describe('PeersView, archived rate', () => {
  beforeEach(() => {
    churnFixture = undefined
    churnPending = false
    churnErr = undefined
    capturedSince = undefined
    capturedCollectors = undefined
  })

  // The join is per COLLECTOR, and this is the case that proves it: one peer
  // two collectors watch, each having archived a different amount. A join on
  // (router, peer) alone would put one number on both rows and label one
  // collector's measurement as the other's.
  it("gives each collector's row that collector's own archived rate", async () => {
    const base = peersDown.data[0]
    fixture = {
      data: [
        { ...base, collector: 'dev-c1' },
        { ...base, collector: 'dev-c2' },
      ],
      meta: peersDown.meta,
    }
    churnFixture = { failed: [], by: new Map([
      [`dev-c1|${base.router_ip}|${base.peer_ip}`, { changes_per_second: 0.5 }],
      [`dev-c2|${base.router_ip}|${base.peer_ip}`, { changes_per_second: 2 }],
    ]) }
    const w = mountPeers()
    await nextTick()
    const cells = w.findAll('tbody tr [data-archived]').map((c) => c.text())
    expect(cells).toEqual(['0.50', '2.00'])
    // And it asked about both, rather than one collector's answer reused.
    expect([...(capturedCollectors?.value ?? [])].sort()).toEqual(['dev-c1', 'dev-c2'])
  })

  // One collector's request failing must not blank the others' rates.
  //
  // The fan-out is N requests, and Promise.all rejects the moment any one
  // does -- so a single 500 left every cell on the screen reading "—",
  // including rows whose own collector answered fine, with no error anywhere
  // to say why. queries.ts already rules this out by name for the Monitor
  // signals: "Promise.all would turn one endpoint's failure into all four
  // going dark." The same rule, on a fan-out that shipped with the wrong
  // primitive.
  it("keeps a working collector's rates when another collector's request fails", async () => {
    const base = peersDown.data[0]
    fixture = {
      data: [
        { ...base, collector: 'dev-c1' },
        { ...base, collector: 'dev-c2' },
      ],
      meta: peersDown.meta,
    }
    // dev-c2 answered; dev-c1's request failed, so it is simply absent from
    // the merged map -- which is what allSettled leaves behind.
    churnFixture = {
      failed: ['dev-c1'],
      by: new Map([[`dev-c2|${base.router_ip}|${base.peer_ip}`, { changes_per_second: 2 }]]),
    }
    const w = mountPeers()
    await nextTick()
    const cells = w.findAll('tbody tr [data-archived]').map((c) => c.text())
    expect(cells[1]).toBe('2.00')
    // And the failure is SAID, rather than leaving a silent dash.
    expect(w.get('[data-churn-error]').text()).toMatch(/dev-c1|rate/i)
  })

  // A peer the churn answer does not mention archived NOTHING in the window
  // -- the endpoint ranks by change and omits the quiet. That is a
  // measurement, so it reads 0.00 rather than a dash: a dash here would say
  // "we do not know", which is a different and weaker claim.
  it('reads zero for a peer that archived nothing, not a dash', async () => {
    fixture = { data: [{ ...peersDown.data[0], collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = { failed: [], by: new Map() }
    const w = mountPeers()
    await nextTick()
    expect(w.get('tbody tr [data-archived]').text()).toBe('0.00')
  })

  // The sparkline places each bar at its OWN time across the window, never
  // at its index in the array.
  //
  // activity omits buckets that held no change, so even spacing would draw a
  // peer that churned twice an hour apart exactly like one that churned
  // twice in a minute -- the same defect ChurnChart already fixed once, when
  // /v1/collection/churn's answer turned out to skip its empty buckets too.
  // The window comes from the answer's own meta, not a local subtraction.
  it('places sparkline bars at their own time in the window, not at their index', async () => {
    const base = peersDown.data[0]
    fixture = { data: [{ ...base, collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = {
      failed: [],
      // A six-hour window with two bars: one at the start, one three
      // quarters through. Evenly spaced they would sit at 0% and 50%.
      window: { from: '2026-09-20T00:00:00Z', to: '2026-09-20T06:00:00Z' },
      by: new Map([
        [
          `dev-c1|${base.router_ip}|${base.peer_ip}`,
          {
            changes_per_second: 1,
            activity: [
              { bucket: '2026-09-20T00:00:00Z', changes: 2 },
              { bucket: '2026-09-20T04:30:00Z', changes: 4 },
            ],
          },
        ],
      ]),
    }
    const w = mountPeers()
    await nextTick()
    const bars = w.findAll('tbody tr [data-spark] rect')
    expect(bars).toHaveLength(2)
    const xs = bars.map((b) => Math.round(Number(b.attributes('x'))))
    expect(xs[0]).toBe(0)
    // 04:30 of a 06:00 window is three quarters through, not half.
    expect(xs[1]).toBe(75)
  })

  // A missing axis is not a measured zero.
  //
  // The bars are placed inside meta.churn_from/churn_to, so without those
  // there is nowhere to put them -- but the em dash beside this column is
  // documented as "this peer changed nothing in the window". An older
  // daemon, a stripped meta, or a fan-out whose first answer carried none
  // would have drawn every row as quiet while the rate column beside it
  // showed a real nonzero number. That is the same conflation archivedRate
  // goes out of its way to avoid by returning undefined rather than 0.
  it('says the axis is missing rather than drawing every peer as quiet', async () => {
    const base = peersDown.data[0]
    fixture = { data: [{ ...base, collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = {
      failed: [],
      window: undefined,
      by: new Map([
        [
          `dev-c1|${base.router_ip}|${base.peer_ip}`,
          { changes_per_second: 2, activity: [{ bucket: '2026-09-20T01:00:00Z', changes: 9 }] },
        ],
      ]),
    }
    const w = mountPeers()
    await nextTick()
    expect(w.find('tbody tr [data-spark]').exists()).toBe(false)
    // NOT the "changed nothing" dash: this peer changed nine times.
    expect(w.find('tbody tr [data-spark-empty]').exists()).toBe(false)
    expect(w.get('tbody tr [data-spark-unplaceable]').text()).toBe('?')
  })

  // A bar is as wide as the bucket it stands for. The width was a constant
  // 2.5% while a 24-bucket window makes each bucket 4.17% -- so every bar
  // was narrower than the span it claimed, and two bars in adjacent buckets
  // sat with a gap that the data does not contain. ChurnChart derives its
  // own width from meta.churn_bucket; this path sends that field and the
  // component was not given it.
  it('draws a bar as wide as the bucket it stands for', async () => {
    const base = peersDown.data[0]
    fixture = { data: [{ ...base, collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = {
      failed: [],
      // Six hours, one-hour buckets: each bar is a sixth of the axis.
      window: { from: '2026-09-20T00:00:00Z', to: '2026-09-20T06:00:00Z', bucket: '1h0m0s' },
      by: new Map([
        [
          `dev-c1|${base.router_ip}|${base.peer_ip}`,
          { changes_per_second: 1, activity: [{ bucket: '2026-09-20T00:00:00Z', changes: 2 }] },
        ],
      ]),
    }
    const w = mountPeers()
    await nextTick()
    const bar = w.get('tbody tr [data-spark] rect')
    expect(Math.round(Number(bar.attributes('width')))).toBe(17)
  })

  // A peer that changed nothing draws NOTHING, not an empty frame. An
  // untouched box in a column of real marks reads as a measured flat line,
  // where the truth is that there is no series to draw -- and the rate
  // column beside it already carries the number.
  it('draws no sparkline at all for a peer with no changes', async () => {
    const base = peersDown.data[0]
    fixture = { data: [{ ...base, collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = {
      failed: [],
      window: { from: '2026-09-20T00:00:00Z', to: '2026-09-20T06:00:00Z' },
      by: new Map([
        [`dev-c1|${base.router_ip}|${base.peer_ip}`, { changes_per_second: 0, activity: [] }],
      ]),
    }
    const w = mountPeers()
    await nextTick()
    expect(w.find('tbody tr [data-spark]').exists()).toBe(false)
    expect(w.get('tbody tr [data-spark-empty]').text()).toBe('—')
  })

  // Found in a browser: a lab archive's busiest peer archives 21 rows in 24
  // hours, which is 0.000162/s, and toFixed(2) draws it as "0.00" --
  // identical to a peer that archived NOTHING. Two decimals are not enough
  // for rates like that, and the zero above is the claim they collide
  // with.
  //
  // A nonzero rate below the displayed precision says so rather than
  // rounding into the value that means something else.
  it('never renders a nonzero rate as the zero that means "archived nothing"', async () => {
    const base = peersDown.data[0]
    fixture = { data: [{ ...base, collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = { failed: [], by: new Map([
      [`dev-c1|${base.router_ip}|${base.peer_ip}`, { changes_per_second: 0.000162 }],
    ]) }
    const w = mountPeers()
    await nextTick()
    const cell = w.get('tbody tr [data-archived]')
    expect(cell.text()).not.toBe('0.00')
    expect(cell.text()).toBe('<0.01')
  })

  // Before an answer, and after a failed one, there is no measurement to
  // report -- and 0.00 would be one. This is the branch that keeps the zero
  // above meaning what it says.
  it('says nothing rather than zero when the churn answer has not arrived', async () => {
    fixture = { data: [{ ...peersDown.data[0], collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = undefined
    churnPending = true
    const w = mountPeers()
    await nextTick()
    expect(w.get('tbody tr [data-archived]').text()).toBe('—')
  })

  // The window is the operator's, and it is named on screen beside the
  // number it scopes: whichever rate this table shows has to name both
  // bounds on screen, or it becomes an unlabeled-window defect.
  it('drives the rate off a window control and names that window', async () => {
    fixture = { data: [{ ...peersDown.data[0], collector: 'dev-c1' }], meta: peersDown.meta }
    churnFixture = { failed: [], by: new Map() }
    const w = mountPeers()
    await nextTick()
    expect(capturedSince?.value).toBe('1h')
    const note = w.get('[data-archived-note]')
    expect(note.text()).toMatch(/last hour/i)
    expect(note.text()).toMatch(/archived|collection rate/i)
    // And it is not the router's update rate, said out loud.
    expect(note.text()).toMatch(/not the router/i)
  })
})

// Column widths must sum to exactly 100, and now this is tested for it:
// an invariant a comment alone does not enforce can drift as columns are
// added or resized.
//
// MonitorView has had this test for its own table since its widths
// over-constrained a panel; this screen did not, and jsdom computes no
// layout, so a drifted total would leave the whole suite green.
describe('PeersView column widths', () => {
  it('declares percentage widths that sum to exactly 100', () => {
    fixture = peersDown
    const widths = columnWidths(mountPeers({ shallow: true }))
    expect(widths.every((x) => typeof x === 'string' && x.endsWith('%'))).toBe(true)
    const total = widths.reduce((n, x) => n + parseFloat(x!), 0)
    expect(total).toBe(100)
  })
})

describe('PeersView, up since', () => {
  // A measurement on 2026-09-21 showed what a DURATION
  // here would claim: across a lab archive `now - up_since` equaled how
  // long the ROUTER had been silent for 25 of 35 peers, to the decimal. A
  // lab torn down rather than shut down sends no Peer Down, so the last
  // state stays `up` forever and the subtraction renders archive silence as
  // a live session.
  //
  // So the column is the INSTANT, rendered through the same formatClock
  // every other screen's timestamps go through, and never an elapsed time.
  it('renders when the session came up, never how long it has been up', async () => {
    const base = peersDown.data[0]
    fixture = {
      data: [{ ...base, state: 'up', up_since: '2026-08-18T22:13:18.766453Z' }],
      meta: peersDown.meta,
    }
    const w = mountPeers()
    await nextTick()
    const cell = w.get('tbody tr [data-up-since]')
    expect(cell.text()).toBe(formatClock('2026-08-18T22:13:18.766453Z'))
    // Must-not-say: no elapsed time, in any of the shapes one would take.
    expect(cell.text()).not.toMatch(/\bago\b|\d+\s*(d|days|h|hours|m|minutes)\b/i)
  })

  // "Up since" asserts an ONGOING state, so it renders only for a peer that
  // is up or stale (stale is the next test). A peer whose State column reads
  // `down` two cells to the left, with
  // "Up since Aug 18" beside it, describes a session that has demonstrably
  // ended as though it were live -- and the query deliberately keeps the last
  // up of the session regardless of what followed it, which is what makes
  // this the screen's decision rather than the API's.
  //
  // view_lost is the same refusal for a different reason: the collector
  // stopped being able to see the peer, so whether the session is still up
  // is precisely what it cannot say.
  it('shows no up-since for a peer that is down or view_lost', async () => {
    const base = peersDown.data[0]
    fixture = {
      data: [
        { ...base, state: 'up', up_since: '2026-08-18T22:13:18Z' },
        { ...base, state: 'down', up_since: '2026-08-18T22:13:18Z' },
        { ...base, state: 'view_lost', up_since: '2026-08-18T22:13:18Z' },
        { ...base, state: 'stale', up_since: '2026-08-18T22:13:18Z' },
      ],
      meta: peersDown.meta,
    }
    const w = mountPeers()
    await nextTick()
    const cells = w.findAll('tbody tr [data-up-since]').map((c) => c.text())
    expect(cells[0]).toBe(formatClock('2026-08-18T22:13:18Z'))
    expect(cells[1]).toBe('—')
    expect(cells[2]).toBe('—')
    expect(cells[3]).toBe(formatClock('2026-08-18T22:13:18Z'))
  })

  // stale is the other side of that rule. Its collector has gone quiet, but
  // nothing has ended the session: it is the same last-known session whose
  // routes are still served, so the instant it came up is the start of what
  // is on screen. peers-stale.json is a real capture holding both sides for
  // one router: dev-c2's peers stale, and dev-c1's view of the same peers
  // view_lost. Every row carries an up_since on the wire, so a dash here is
  // the screen's decision, not a missing field.
  it('shows up-since for a stale peer, as it was captured, and not for its view_lost twin', async () => {
    fixture = peersStale
    const w = mountPeers()
    await nextTick()
    for (const p of peersStale.data) {
      if (!p.up_since) throw new Error('peers-stale.json has a row with no up_since')
    }
    // Read each row's state off its own pill, so the pairing of state and
    // cell is the rendered one, not an assumption about row order.
    const rendered = w.findAll('tbody tr').map((r) => ({
      state: r.get('.pill').classes().find((c) => c !== 'pill'),
      upSince: r.get('[data-up-since]').text(),
    }))
    const stale = rendered.filter((r) => r.state === 'stale')
    const lost = rendered.filter((r) => r.state === 'view_lost')
    expect(stale).toHaveLength(peersStale.data.filter((p) => p.state === 'stale').length)
    expect(lost).toHaveLength(peersStale.data.filter((p) => p.state === 'view_lost').length)
    expect(stale.length).toBeGreaterThan(0)
    expect(lost.length).toBeGreaterThan(0)
    const staleInstants = peersStale.data
      .filter((p) => p.state === 'stale')
      .map((p) => formatClock(p.up_since!))
      .sort()
    expect(stale.map((r) => r.upSince).sort()).toEqual(staleInstants)
    for (const r of lost) expect(r.upSince).toBe('—')
  })

  // Null is the wire's "this session never came up" -- the same encoding
  // hold_time uses, and for the same reason. Rendering it as a date would
  // print 1970 (or an Invalid Date) as though it were an observation.
  it('says nothing rather than a date when the session never came up', async () => {
    const base = peersDown.data[0]
    fixture = { data: [{ ...base, up_since: null }], meta: peersDown.meta }
    const w = mountPeers()
    await nextTick()
    const cell = w.get('tbody tr [data-up-since]')
    expect(cell.text()).toBe('—')
    expect(cell.text()).not.toMatch(/1970|Invalid/)
  })

  // The limit said once, on screen, in this project's usual voice: the
  // column is a record of an event, not a claim that the session is still up.
  it('states that the column is not a liveness claim', async () => {
    fixture = peersDown
    const w = mountPeers()
    await nextTick()
    const note = w.get('[data-up-since-note]')
    expect(note.text()).toMatch(/liveness|still up|stopped hearing/i)
  })
})

describe('PeersView, collector axis', () => {
  // /v1/peers is one entry per (collector, router, peer, rib), so a router
  // two collectors monitor contributes two rows for one peer. Until
  // 2026-09-20 this table rendered them as duplicates, identical in every
  // visible column, with nothing on screen to say why -- found in a browser
  // against a lab deployment, where 7 pairs did exactly that.
  //
  // A column, not a merge: the two rows are two real observations that can
  // disagree about session and state, and collapsing them would be the
  // silent pick this project audits for. ScopePicker already annotates the
  // same condition two screens away.
  it('names the collector, so one peer seen twice is not two mystery rows', async () => {
    const base = peers.data[0]
    fixture = {
      data: [
        { ...base, collector: 'coll-a', session_id: '111' },
        { ...base, collector: 'coll-b', session_id: '222' },
      ],
      meta: peers.meta,
    }
    const w = mountPeers()
    await nextTick()

    const headers = w.findAll('th').map((h) => h.text().toLowerCase())
    expect(
      headers.some((h) => h.includes('collector')),
      `the table renders ${headers.join(', ')} with no collector column, so a ` +
        'peer two collectors watch appears twice with nothing to tell them apart',
    ).toBe(true)

    const body = w.text()
    expect(body).toContain('coll-a')
    expect(body).toContain('coll-b')
  })

  // Every column here is a percentage, so every column shrinks with the
  // screen: at 390px they were 22-40px wide, nine of ten headers were cut
  // off and every state pill was clipped. The floor is where the table
  // stops shrinking and scrolls inside its own card instead, and it has to
  // leave each column room for its own header. The widths below are each
  // header's label plus its 36px of padding, measured in Chromium; jsdom
  // cannot measure them.
  it('floors the table where every header still fits its column', () => {
    fixture = peersDown
    pending = false
    loading = false
    const HEADER_PX: Record<string, number> = {
      peer_ip: 45, router_ip: 60, collector: 81, rib: 36, asn: 40, state: 50,
      routes: 60, up_since: 68, changes_per_second: 84, activity: 70,
    }
    const w = mountPeers({ shallow: true })
    const floor = tableMinWidthPx(w)
    expect(floor).toBeDefined()
    const cramped = columnsOf(w)
      .map((c) => {
        const width = (c as { width?: string }).width ?? ''
        expect(width, `${c.id} is not a percentage`).toMatch(/%$/)
        return [c.id, (floor! * parseFloat(width)) / 100] as const
      })
      .filter(([id, px]) => px < (HEADER_PX[id] ?? Infinity))
    expect(cramped).toEqual([])
  })

  // Headers are not the only content with a fixed minimum. A peer address
  // does not wrap, so a Peer column narrower than its address cuts it off
  // with an ellipsis -- which it did at every width while the column was
  // 12%: 2001:db8:5549:3::1 needs 166px and got 126px at the floor. The
  // widths below are each cell's content plus its 36px of padding, measured
  // in Chromium; jsdom cannot measure them.
  //
  // peer_ip is sized for 23 characters, 202px: an IXP peering-LAN address
  // such as 2001:7f8:1::a500:6939:1, the long compressed form real peers
  // carry. state is the widest pill, "view lost", at 95px. No captured
  // fixture holds an IPv6 peer, so these are measured constants rather
  // than fixture values.
  it('floors the table where a long IPv6 peer and every state pill still fit', () => {
    fixture = peersDown
    pending = false
    loading = false
    const CONTENT_PX: Record<string, number> = { peer_ip: 202, state: 95 }
    const w = mountPeers({ shallow: true })
    const floor = tableMinWidthPx(w)
    expect(floor).toBeDefined()
    const cramped = columnsOf(w)
      .map((c) => [c.id, (floor! * parseFloat((c as { width?: string }).width ?? '')) / 100] as const)
      .filter(([id, px]) => px < (CONTENT_PX[id] ?? -Infinity))
    expect(cramped).toEqual([])
  })
})

describe('PeersView, peer address', () => {
  // A cell narrower than its address ellipsizes it, and an ellipsized
  // address is a different address. The title keeps the full value one
  // hover away at any width the table is drawn at.
  it('carries the full peer address as the link title', () => {
    fixture = peersDown
    pending = false
    loading = false
    const w = mount(PeersView, { global: { stubs: { RouterLink: RouterLinkStub } } })
    const links = w.findAllComponents(RouterLinkStub)
    expect(links.length).toBe(peersDown.data.length)
    expect(links.map((l) => l.attributes('title'))).toEqual(peersDown.data.map((p) => p.peer_ip))
  })

  // The same for the two other identifiers beside it. A default Helm install
  // names its collector vantage-collector-0, which ellipsizes in Collector at
  // every width, and an IPv6 router address ellipsizes in Router.
  it('carries the full router address and collector id as cell titles', () => {
    fixture = peersDown
    pending = false
    loading = false
    const w = mountPeers()
    const routers = w.findAll('[data-router]')
    const collectors = w.findAll('[data-collector]')
    expect(routers.length).toBe(peersDown.data.length)
    expect(collectors.length).toBe(peersDown.data.length)
    expect(routers.map((r) => r.attributes('title'))).toEqual(peersDown.data.map((p) => p.router_ip))
    expect(routers.map((r) => r.text())).toEqual(peersDown.data.map((p) => p.router_ip))
    expect(collectors.map((c) => c.attributes('title'))).toEqual(peersDown.data.map((p) => p.collector))
    expect(collectors.map((c) => c.text())).toEqual(peersDown.data.map((p) => p.collector))
  })
})
