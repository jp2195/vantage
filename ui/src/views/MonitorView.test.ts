import { RouterLinkStub, type VueWrapper, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { type Ref, ref } from 'vue'
import type { RouteLocationRaw } from 'vue-router'
import DataTable from '@/components/DataTable.vue'
import collectionDumps from '@/api/fixtures/collection-dumps.json'
import collectionSessions from '@/api/fixtures/collection-sessions.json'
import collectionLocrib from '@/api/fixtures/collection-locrib.json'
import collectionLocribNoStat from '@/api/fixtures/collection-locrib-2026-09-11.json'
import collectionFlags from '@/api/fixtures/collection-flags.json'
import fleetEvents from '@/api/fixtures/fleet-events.json'
import twoCollectors from '@/api/fixtures/events-two-collectors.json'
import { twoCollectorRouters } from '@/test-support/twoCollectorRouters'
import routersStale from '@/api/fixtures/routers-stale.json'
import { bestVantageRouters } from '@/lib/bestVantage'
import type { Router } from '@/api/generated'
import { type GuardedColumn, inventedColumns } from '@/test-support/columnGuard'
import { unresolvable } from '@/test-support/routeResolution'
import { tableMinWidthPx } from '@/test-support/columnsOf'
import { formatCount } from '@/lib/formatCount'

// One fixture, one pending flag and one error per section -- the mock's own
// shape has to let a test drive all four sections independently, the same
// property the screen itself exists to prove. Reset in beforeEach so the
// isolation rule (every test passes alone via `-t`) holds: a value left
// behind by one test must never leak into the next.
let dumpsFixture: { data: unknown[]; meta: unknown } = collectionDumps
let dumpsPending = false
let dumpsErr: Error | undefined

let sessionsFixture: { data: unknown[]; meta: unknown } = collectionSessions
let sessionsPending = false
let sessionsErr: Error | undefined

let locribFixture: { data: unknown[]; meta: unknown } = collectionLocrib
let locribPending = false
let locribErr: Error | undefined

/**
 * CAPTURED, not written: one real `down` row taken off a lab archive with
 *
 *     curl -H "Authorization: Bearer $TOKEN" \
 *       "http://127.0.0.1:9473/v1/events?since=6h&limit=20"
 *
 * on 2026-09-20. It is here rather than in fleet-events.json because that
 * file is a REAL capped answer whose `total_matched` three other tests
 * assert against (EventsView, ResultMeta), and every one of its five rows is
 * an `up` -- so the committed cap cannot drive the branch below. A reason
 * line renders for a `down` and for nothing else, and a fixture that only
 * ever takes one side of that could not tell the two apart.
 */
const downEvent = {
  ts_collector: '2026-09-20T22:01:01.645657Z',
  ts_router: '2026-08-10T17:37:12.975241Z',
  stream_seq: '3289',
  seq: '2',
  session_id: '1789941661645473726',
  router_ip: '172.22.0.11',
  router_sysname: '',
  peer_ip: '10.255.1.1',
  peer_asn: 65100,
  collector: 'dev-c2',
  rib: 'in_pre',
  kind: 'down',
  down_reason: 1,
  reason_name: 'local system closed, notification follows',
  local_ip: '::',
  local_port: 0,
  remote_port: 0,
}

let eventsFixture: { data: unknown[]; meta: unknown } = fleetEvents

let flagsFixture: { data: unknown[]; meta: unknown } = collectionFlags
// routers-stale.json, not routers.json: the peer tile sums peers_stale,
// which routers.json was captured before, and a missing field sums to NaN.
let routersFixture: { data: unknown[]; meta: unknown } = routersStale
let flagsPending = false
let flagsErr: Error | undefined

// SYNTHESIZED, and labeled: a lab archive's churn inside any window this
// screen offers is four rows from one peer -- most lab data is older than
// the 24h clamp -- which cannot show a RANKING at all. Built to the wire
// shape of
// /v1/collection/churn/peers with three peers whose order is unambiguous and
// whose series differ from each other, so a column reading the wrong one
// fails rather than coincidentally matching.
//
// changes_per_second is written as the arithmetic rather than as a decimal
// -- (readvertise + withdraw) over 3600, this screen's default 1h window --
// so a reader can see that the fixture agrees with itself. The daemon
// computes this field for the reason its contract gives: only the daemon has
// resolved what since= meant.
//
// peerChurnFixture[0] is the busiest by changes (readvertise + withdraw) but
// NOT by dumps, and [2] has the most dumps of the three. A ranking that
// sorted on dumps, or on the total of all three, would put a different row
// first -- which is the mistake a fixture ranked consistently on every
// column could not catch.
const peerChurnFixture = {
  data: [
    {
      router_ip: '10.0.103.61', router_sysname: 'xr-rr1', peer_ip: '10.255.0.2',
      peer_asn: 65000, readvertise: 400, withdraw: 50, dump: 10,
      changes_per_second: 450 / 3600,
    },
    {
      router_ip: '10.0.103.61', router_sysname: 'xr-rr1', peer_ip: '10.255.0.3',
      peer_asn: 65000, readvertise: 100, withdraw: 5, dump: 20,
      changes_per_second: 105 / 3600,
    },
    {
      router_ip: '10.0.103.62', router_sysname: 'xr-pe1', peer_ip: '10.2.0.2',
      peer_asn: 65001, readvertise: 1, withdraw: 0, dump: 900,
      changes_per_second: 1 / 3600,
    },
  ],
  meta: { next_cursor: null, warnings: [], total_matched: null },
}
let churnPeersFixture: { data: unknown[]; meta: unknown } = peerChurnFixture
let churnPeersPending = false
let churnPeersErr: Error | undefined

// The /v1/peers rows the ranking is joined against. One row per (router,
// peer) here; joinPeersAmbiguous below is the same peer under TWO ribs,
// which api/openapi.yaml's own "one entry per (collector, router, peer,
// rib)" allows and this lab happens not to contain today. Absence from one
// short archive is not proof a shape cannot occur, and PeerDetailView and
// ScopePicker both already carry handling for exactly this cardinality.
const joinPeers = {
  data: [
    { router_ip: '10.0.103.61', peer_ip: '10.255.0.2', rib: 'in_pre', state: 'up', routes: 812, asn: 65000, dump_states: {} },
    { router_ip: '10.0.103.61', peer_ip: '10.255.0.3', rib: 'in_pre', state: 'down', routes: 0, asn: 65000, dump_states: {} },
    { router_ip: '10.0.103.62', peer_ip: '10.2.0.2', rib: 'in_pre', state: 'up', routes: 97, asn: 65001, dump_states: {} },
  ],
  meta: { next_cursor: null, warnings: [], total_matched: null },
}
const joinPeersAmbiguous = {
  data: [
    ...joinPeers.data,
    { router_ip: '10.0.103.61', peer_ip: '10.255.0.2', rib: 'in_post', state: 'up', routes: 44, asn: 65000, dump_states: {} },
  ],
  meta: joinPeers.meta,
}
let joinPeersFixture: { data: unknown[]; meta: unknown } = joinPeers

// SYNTHESIZED, not captured. collection-sessions.json's own archive holds
// zero view_lost rows fleet-wide -- meta.session_totals.view_lost: 0, and
// every one of its 15 rows reads view_lost: 0, not only the total (see
// ui/src/api/fixtures/README.md's own "collection-sessions.json" section,
// which calls this "a real archive gap, not a capture mistake":
// vantage.peer_events holds 2,744 up rows and 99 down rows and exactly
// zero view_lost rows). No session in that archive has ever lost
// its BMP transport while the router stayed silent, so there is nothing to
// re-capture -- the README names exactly this choice as the fallback: build
// a synthesized fixture and label it, the way SessionHistoryView.test.ts
// already does for the identical situation on a different endpoint. Built
// from the real fixture's own busiest row (router_ip 10.0.103.65, xr-p2)
// with ONLY view_lost changed, from 0 to 7 -- router_ip, router_sysname,
// sessions, up and down all stay the real captured values, including down
// staying at its real 2, so a test can tell the two counts were not folded
// into each other.
const sessionsWithViewLost = {
  data: [{ ...collectionSessions.data[0], view_lost: 7 }, ...collectionSessions.data.slice(1)],
  meta: collectionSessions.meta,
}

function mockSection(fixture: { data: unknown[]; meta: unknown }, pending: boolean, err: Error | undefined) {
  return {
    data: ref(err ? undefined : { data: fixture.data, meta: fixture.meta }),
    isPending: ref(pending),
    isLoading: ref(false),
    error: ref(err),
  }
}

// The shared-since defect: MonitorView.vue passes ONE `since` ref to all
// four composables, but nothing before this proved that -- the mock took no
// `since` parameter at all, so `useCollectionDumps(ref('1h'))` (a fresh,
// dead ref that never moves) or a stale ref on any ONE of the four would
// have passed the entire suite while `data-window-note` kept announcing
// "last 6 hours". This repo already treats that gap as a first-class
// defect on a single-composable screen -- SessionHistoryView.test.ts
// captures `since` off the mocked useEvents call and asserts its value
// (added 2026-09-07, after removing `since` from the request left every UI
// test green) -- and four call sites sharing one ref quadruple the
// surface for exactly one of them to be wrong. Captured per section here,
// reset in beforeEach for the same isolation reason every other mutable
// module variable above is.
let capturedSince: {
  dumps?: Ref<string>
  sessions?: Ref<string>
  locrib?: Ref<string>
  flags?: Ref<string>
  events?: Ref<string>
  churn?: Ref<string>
  churnPeers?: Ref<string>
} = {}
let capturedChurnBucket: Ref<string> | undefined
// The window the ANSWER reports, which is what the chart's axis is drawn
// between. Defaults to absent so every other test in this file exercises the
// no-bounds path the daemon can still produce.
let churnWindow: { churn_from?: string; churn_to?: string } = {}
// Two buckets with all three series non-zero: a fixture where one series is
// always zero would let a chart that dropped it pass.
const churnFixture = [
  { ts: '2026-09-17T14:55:00Z', dump: 120, readvertise: 40, withdraw: 6 },
  { ts: '2026-09-17T14:57:00Z', dump: 0, readvertise: 18, withdraw: 2 },
]

vi.mock('@/api/queries', () => ({
  useCollectionDumps: (since: Ref<string>) => {
    capturedSince.dumps = since
    return mockSection(dumpsFixture, dumpsPending, dumpsErr)
  },
  useCollectionSessions: (since: Ref<string>) => {
    capturedSince.sessions = since
    return mockSection(sessionsFixture, sessionsPending, sessionsErr)
  },
  useCollectionLocRib: (since: Ref<string>) => {
    capturedSince.locrib = since
    return mockSection(locribFixture, locribPending, locribErr)
  },
  useCollectionFlags: (since: Ref<string>) => {
    capturedSince.flags = since
    return mockSection(flagsFixture, flagsPending, flagsErr)
  },
  // An events panel beside the four signals, and a peer tile above them.
  // Both read endpoints this screen did not previously call, and both
  // already exist as composables.
  // The REAL shape: useFleetEvents returns Colada's query object -- `data`
  // holding { data, meta } -- not the { rows, meta, hasMore } a cursor walk
  // returns. A mock that invents `rows` instead would pass every test here
  // while the screen failed to typecheck: a mock is a claim about the
  // contract, and an invented one is a self-confirming test (see
  // queries.ts's unwrap).
  useFleetEvents: (since: Ref<string>) => {
    capturedSince.events = since
    return {
      data: ref({ data: eventsFixture.data, meta: eventsFixture.meta }),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(undefined),
    }
  },
  // The churn panel's own answer. Its bucket comes back in meta, not from
  // the request, because the screen renders the width the ANSWER reports --
  // the two agree until a clamp disagrees.
  useCollectionChurn: (since: Ref<string>, bucket: Ref<string>) => {
    capturedSince.churn = since
    capturedChurnBucket = bucket
    return {
      data: ref({ data: churnFixture, meta: { warnings: [], churn_bucket: '2m0s', ...churnWindow } }),
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(undefined),
    }
  },
  // The ranked peer table's own answer, and the /v1/peers rows it is joined
  // against for state and route count -- two endpoints, because churn knows
  // how much a peer sent and nothing about what it is holding now.
  useCollectionChurnPeers: (since: Ref<string>) => {
    capturedSince.churnPeers = since
    return mockSection(churnPeersFixture, churnPeersPending, churnPeersErr)
  },
  usePeers: () => mockSection(joinPeersFixture, false, undefined),
  useRouters: () => ({
    data: ref(routersFixture),
    isPending: ref(false),
    isLoading: ref(false),
    error: ref(undefined),
  }),
}))

const MonitorView = (await import('./MonitorView.vue')).default

// RouterLinkStub rather than `RouterLink: true`: these tests read each
// link's own destination off its `to` prop, which the boolean stub does not
// carry. No router is installed -- which paths exist is router.ts's
// concern, the same split PeersView.test.ts and AppShell.test.ts already
// make. Every mount in this file goes through here so a RouterLink
// anywhere in the screen cannot log "Failed to resolve component" into the
// pristine-console bar this project holds itself to.
function mountMonitor(opts: Record<string, unknown> = {}) {
  return mount(MonitorView, { global: { stubs: { RouterLink: RouterLinkStub } }, ...opts })
}

/** Just the slice of a wrapper this helper reads -- copied from columnsOf.ts's own. */
interface PropsReadable {
  props(name: string): unknown
}


/**
 * The columns of the table inside ONE NAMED section.
 *
 * Named rather than positional: a positional lookup ties every assertion's
 * index to this file's panel order, so moving one panel would shift every
 * later index and break assertions that have nothing to do with the
 * change -- for example, reading the DUMPS table's headers while looking
 * for a column only the sessions table has, and reporting it as missing.
 * A test should fail for the reason it names.
 *
 * The `unknown` cast is the one columnsOf.ts itself takes and explains:
 * DataTable is a generic SFC and findComponent's overloads do not resolve
 * that, so the static type is wrong in test plumbing only -- the runtime
 * lookup is unaffected.
 */
function columnsOfNamed(w: VueWrapper, section: string): GuardedColumn[] {
  const table = w.find(`[data-section="${section}"]`).findComponent(DataTable as never)
  return (table as unknown as PropsReadable).props('columns') as GuardedColumn[]
}

describe('MonitorView', () => {
  beforeEach(() => {
    dumpsFixture = collectionDumps
    dumpsPending = false
    dumpsErr = undefined
    churnPeersFixture = peerChurnFixture
    churnPeersPending = false
    churnPeersErr = undefined
    joinPeersFixture = joinPeers
    sessionsFixture = collectionSessions
    sessionsPending = false
    sessionsErr = undefined
    locribFixture = collectionLocrib
    locribPending = false
    locribErr = undefined
    flagsFixture = collectionFlags
    flagsPending = false
    flagsErr = undefined
    eventsFixture = fleetEvents
    routersFixture = routersStale
    capturedSince = {}
    churnWindow = {}
  })

  // The shared-since rule: the window control has to reach and move
  // all FOUR composables through the SAME ref, not just one of them or a
  // fresh snapshot handed to each. Captures the actual ref instance each
  // mocked composable received, the same mechanism
  // SessionHistoryView.test.ts uses for its own single `since`, applied to
  // all four call sites here. A stale ref on any one
  // of the four -- or a screen that quietly forked one composable onto its
  // own since -- would leave that section's value frozen at '1h' below
  // while the others (and the window note) moved to '6h'.
  it('drives all four composables off the same window control, not a stale or partial ref', async () => {
    const w = mountMonitor()
    expect(capturedSince.dumps?.value).toBe('1h')
    expect(capturedSince.sessions?.value).toBe('1h')
    expect(capturedSince.locrib?.value).toBe('1h')
    expect(capturedSince.flags?.value).toBe('1h')

    await w.findAll('[data-since-window] button')[1].trigger('click')

    expect(capturedSince.dumps?.value).toBe('6h')
    expect(capturedSince.sessions?.value).toBe('6h')
    expect(capturedSince.locrib?.value).toBe('6h')
    expect(capturedSince.flags?.value).toBe('6h')
  })

  // A dump ratio is composition, not a failure state.
  it('renders the dump ratio as composition, never as an error state or a ranking', () => {
    const w = mountMonitor()
    const marks = w.findAll('[data-dump-ratio]')
    expect(marks.length).toBe(collectionDumps.data.length)

    // xr-rr1 (10.0.103.61): dumps 29 of archived 29 -- 100%, the highest
    // ratio this fixture carries and exactly the row an error-above-a-
    // threshold mutation would color.
    const highest = marks.find((m) => m.text().includes('100%'))
    expect(highest).toBeDefined()
    expect(highest!.classes()).not.toContain('error')
    expect(highest!.classes()).not.toContain('bad')
    expect(highest!.classes()).not.toContain('warn')

    // Class location matters too: the check above only reads the
    // `data-dump-ratio` span's OWN classes -- a position, not the shape of
    // every place a threshold class could actually live. The outer
    // `span.mono` wrapping it and the `<td>` itself (the very element the
    // first attempt at this mutation landed on and slipped past
    // undetected, before the hook was moved) are both
    // still places a mutation could attach `:class="{ error: ... }"` while
    // coloring the identical visible text. Scoped to the dumps section so
    // an error/warn state genuinely raised by, say, a fetch failure
    // elsewhere on the page cannot make this pass or fail for the wrong
    // reason.
    const dumpsSectionEl = w.get('[data-section="dumps"]')
    expect(dumpsSectionEl.findAll('.error, .warn, .bad').length).toBe(0)

    // Not ranked either: the table keeps /v1/collection/dumps' own
    // archived-DESC order verbatim. bmpgen-iosxr leads (66% ratio) even
    // though xr-rr1 (100%) and xr-pe1 (98%) both read higher -- a ratio-
    // sorted table would put one of those first instead.
    const order = w
      .get('[data-section="dumps"]')
      .findAll('tbody tr')
      .map((tr) => tr.attributes('data-router'))
    expect(order).toEqual(collectionDumps.data.map((r) => r.router_ip))
  })

  // A ratio over zero archived rows is undefined, not zero. SYNTHESIZED --
  // collection-dumps.json's own captured minimum is archived: 24, so no real
  // row exercises this. Built as a one-row fixture that still honors
  // RouterDumpCount's own contract (archived = dumps + changes: 0 = 0 + 0),
  // rather than reused from any captured row.
  const dumpsWithNothingArchived = {
    data: [{ router_ip: '10.0.0.9', router_sysname: 'synth-empty', archived: 0, dumps: 0, changes: 0 }],
    meta: collectionDumps.meta,
  }

  it('renders "—" for a router with nothing archived, never a stated "0% of archived"', () => {
    dumpsFixture = dumpsWithNothingArchived
    const w = mountMonitor()
    const ratio = w.get('[data-dump-ratio]')
    expect(ratio.text()).toBe('—')
    expect(ratio.text()).not.toContain('%')
  })

  // view_lost and down are never folded together.
  it('keeps view_lost and down as separate columns, neither folded into the other', () => {
    sessionsFixture = sessionsWithViewLost
    const w = mountMonitor()

    const headers = columnsOfNamed(w, 'sessions').map((c) => c.header.toLowerCase())
    expect(headers).toContain('down')
    expect(headers).toContain('view lost')

    const row = w.get('[data-section="sessions"]').get('tr[data-router="10.0.103.65"]')
    const cells = row.findAll('td')
    // Column order: router, sessions, up, down, view_lost.
    expect(cells[3].text()).toBe('2') // the real captured down count
    expect(cells[4].text()).toBe('7') // the synthesized view_lost count

    // The vocabulary that says what the two columns mean, reused rather
    // than reinvented -- EventKindMark.vue's own label for view_lost.
    expect(w.get('[data-section="sessions"]').text()).toContain('collector lost view')
  })

  // A Loc-RIB gap does not by itself indicate lost data.
  //
  // collection-locrib-2026-09-11.json, not collection-locrib.json: the
  // current capture holds no has_stat: false row, because no router in
  // the archive it was captured from has Loc-RIB routes in its current
  // session any more.
  // The older capture is kept whole for this shape; see fixtures/README.md.
  it('renders a has_stat: false Loc-RIB row as "no stat," never a gap of 0', () => {
    locribFixture = collectionLocribNoStat
    const noStatRows = collectionLocribNoStat.data.filter((r) => !r.has_stat)
    expect(noStatRows.length).toBe(2) // guard: the captured fixture's own two rows

    const w = mountMonitor()
    const marks = w.findAll('[data-no-stat]')
    expect(marks.length).toBe(2)
    for (const m of marks) expect(m.text()).toBe('no stat')

    // A has_stat: true row with a real reported count still renders its
    // number -- "no stat" is not applied wholesale to the column.
    const realRow = collectionLocribNoStat.data.find((r) => r.has_stat && r.reported > 0)!
    expect(w.get('[data-section="locrib"]').text()).toContain(String(realRow.reported))
  })

  // The same invariant, stated in prose. There is no gap COLUMN to assert
  // structurally -- reported and archived are two plain numbers side by
  // side and the reading between them lives entirely in this paragraph --
  // so the paragraph IS the on-screen enforcement of "a Loc-RIB gap is not
  // proof of loss." Nothing else in the locrib section asserts it: the
  // other assertions there read [data-no-stat] and a reported figure, and
  // the paragraph carries no digits, so this test is what holds the claim.
  // Asserted the way the flags note two sections down already is:
  // existence, plus a regex per claim that has to survive an edit.
  it('states that a Loc-RIB gap is not proof of loss, and why', () => {
    const w = mountMonitor()
    const note = w.find('[data-locrib-note]')
    expect(note.exists()).toBe(true)
    // The rule itself, as the note states it.
    expect(note.text()).toMatch(/not proof of loss/i)
    // The reason a gap is ordinary: Loc-RIB monitoring is a separate
    // router capability, not something collection failed to store.
    expect(note.text()).toMatch(/RFC 9069/i)
    expect(note.text()).toMatch(/separate capability|adj-RIB-in/i)
    // And the one reading the data itself cannot carry: "no stat" is not a
    // reported gap of 0. This is the sentence that keeps the [data-no-stat]
    // mark above from being read as a zero.
    expect(note.text()).toMatch(/no stat/i)
    expect(note.text()).toMatch(/never a reported gap of 0/i)
    // The window scopes reported only -- archived is what collection holds
    // in the router's current session, whole, so widening the window moves one column and not the other.
    // Without this, an operator who widens the window and sees archived
    // stand still reads it as the screen being stuck.
    expect(note.text()).toMatch(/window/i)
    expect(note.text()).toMatch(/reported only/i)
    // What archived counts has to be in the visible text, not a title
    // attribute: a <p> never takes focus and touch has no hover, so a
    // title-only sentence is unreadable by keyboard and touch.
    expect(note.text()).toMatch(/current BMP session/i)
    expect(note.text()).toMatch(/superseded session/i)
    expect(note.attributes('title')).toBeUndefined()
  })

  // Two columns at the top: the churn chart on the left, the event feed
  // in a rail beside it. A panel far down the page, under
  // Dumps/Sessions/Loc-RIB, would put the two halves of "what happened in
  // this window" -- the shape and the sessions that made it -- a scroll
  // apart. jsdom computes no layout, so what a test can hold is the
  // containment that the CSS grid acts on. The look is checked in a browser.
  it('puts the event feed beside the churn chart rather than in the grid below', () => {
    const w = mountMonitor()
    const row = w.get('[data-row="churn"]')
    expect(row.find('[data-section="churn"]').exists()).toBe(true)
    expect(row.find('[data-section="events"]').exists()).toBe(true)
    // And not ALSO in the grid: a second copy would render the feed twice.
    expect(w.get('.panels').find('[data-section="events"]').exists()).toBe(false)
    expect(w.findAll('[data-section="events"]')).toHaveLength(1)
  })

  // Parse flags describe the decoder as it stood when the row was
  // written, not its current state.
  it('states that parse flags describe the decoder as it stood when the row was written, not its current state', () => {
    const w = mountMonitor()
    const note = w.find('[data-flags-note]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toMatch(/historical/i)
    expect(note.text()).toMatch(/decoder/i)
    expect(note.text()).toMatch(/written|stood/i)
    expect(note.text()).toMatch(/current/i)
  })

  // This screen shows measured signals, never a composite score.
  // Exactly one "NN%" figure per dumps row (the composition ratio above)
  // and nothing else -- a composite across the four signals would add a
  // percentage this count does not expect.
  it('renders no composite score across the four signals', () => {
    const w = mountMonitor()
    const percentages = w.text().match(/\d+%/g) ?? []
    expect(percentages.length).toBe(collectionDumps.data.length)
  })

  it('renders no column claiming a health score or any other invented metric, under any section', () => {
    const w = mountMonitor()
    // columnsOfSection indexes DATATABLES, not sections: this screen has
    // seven panels and five tables (churn draws an SVG, events a list).
    // Peers-by-volume is FIRST because it sits outside the panels grid, at
    // full width -- seven columns do not fit one grid column, measured in a
    // browser. Adding it shifted every later index, which is how this guard
    // reported it: against the wrong fixture, with a readable message. The
    // table-bearing order is pinned here so the next insertion or move fails
    // the same way instead of silently checking one section's columns
    // against another section's shape.
    expect(
      w
        .findAll('section.panel')
        .filter((sec) => sec.find('table, .error, .quiet').exists())
        .map((sec) => sec.attributes('data-section'))
        .filter((name) => name !== 'churn' && name !== 'events'),
    ).toEqual(['churn-peers', 'dumps', 'sessions', 'locrib', 'flags'])

    expect(inventedColumns(columnsOfNamed(w, 'dumps'), collectionDumps.data[0])).toEqual([])
    expect(inventedColumns(columnsOfNamed(w, 'sessions'), collectionSessions.data[0])).toEqual([])
    // The ranked peer table's row is a JOIN of two responses, so its guard
    // row is the union of two captured shapes -- never the computed row the
    // screen hands DataTable. Checking a column id against a shape this
    // screen built would make the guard vacuous: it exists precisely to
    // confirm every column is backed by a field some API actually returned.
    expect(
      inventedColumns(columnsOfNamed(w, 'churn-peers'), {
        ...peerChurnFixture.data[0],
        ...joinPeers.data[0],
      }),
    ).toEqual([])
    expect(inventedColumns(columnsOfNamed(w, 'locrib'), collectionLocrib.data[0])).toEqual([])
    expect(inventedColumns(columnsOfNamed(w, 'flags'), collectionFlags.data[0])).toEqual([])
  })

  // Each section renders its own error without blanking its siblings --
  // the whole reason /v1/collection/* is four endpoints rather than one.
  //
  // Parameterized over all four sections rather than
  // proven through `dumpsErr` alone. A gate accidentally keyed on a
  // DIFFERENT section's error (or on none of them) would have passed the
  // single-section version of this test; running it once per section with
  // only that one section's error set closes that. Sibling checks read
  // `tbody tr` existence generically (not fixture-specific text) so the
  // same assertion applies to whichever three sections are NOT the one
  // under error in a given case.
  const SECTIONS = ['dumps', 'sessions', 'churn-peers', 'locrib', 'flags'] as const
  const setSectionError: Record<(typeof SECTIONS)[number], (e: Error) => void> = {
    dumps: (e) => {
      dumpsErr = e
    },
    sessions: (e) => {
      sessionsErr = e
    },
    'churn-peers': (e) => {
      churnPeersErr = e
    },
    locrib: (e) => {
      locribErr = e
    },
    flags: (e) => {
      flagsErr = e
    },
  }

  it.each(SECTIONS)("renders the %s section's own error without blanking its siblings", (section) => {
    setSectionError[section](new Error(`collection ${section} unavailable`))
    const w = mountMonitor()

    const failed = w.get(`[data-section="${section}"]`)
    expect(failed.text()).toContain(`collection ${section} unavailable`)
    expect(failed.find('tbody').exists()).toBe(false)

    for (const sibling of SECTIONS.filter((s) => s !== section)) {
      expect(w.get(`[data-section="${sibling}"]`).find('tbody tr').exists()).toBe(true)
    }
  })

  // isPending, not isLoading -- see MonitorView.vue's and queries.ts's own
  // comments, and RoutersView.vue's and DataTable.vue's for the shared
  // reasoning. Mirrors EventsView.test.ts's own version of this test,
  // applied once per section since each is its own independent query.
  it('shows loading rather than "no rows matched" in the dumps section while its first fetch is pending', () => {
    dumpsPending = true
    dumpsFixture = { data: [], meta: undefined }
    const w = mountMonitor()
    const section = w.get('[data-section="dumps"]')
    expect(section.text()).toContain('loading')
    expect(section.text()).not.toContain('no rows matched')
  })

  it('shows loading rather than "no rows matched" in the sessions section while its first fetch is pending', () => {
    sessionsPending = true
    sessionsFixture = { data: [], meta: undefined }
    const w = mountMonitor()
    const section = w.get('[data-section="sessions"]')
    expect(section.text()).toContain('loading')
    expect(section.text()).not.toContain('no rows matched')
  })

  it('shows loading rather than "no rows matched" in the locrib section while its first fetch is pending', () => {
    locribPending = true
    locribFixture = { data: [], meta: undefined }
    const w = mountMonitor()
    const section = w.get('[data-section="locrib"]')
    expect(section.text()).toContain('loading')
    expect(section.text()).not.toContain('no rows matched')
  })

  it('shows loading rather than "no rows matched" in the flags section while its first fetch is pending', () => {
    flagsPending = true
    flagsFixture = { data: [], meta: undefined }
    const w = mountMonitor()
    const section = w.get('[data-section="flags"]')
    expect(section.text()).toContain('loading')
    expect(section.text()).not.toContain('no rows matched')
  })

  // The query layer returns router_sysname verbatim, including empty --
  // rendering the fallback is this screen's job. Both collection-dumps.json
  // and collection-sessions.json carry the same two blank-sysname routers
  // (10.0.103.73 and 10.0.103.69), so both sections contribute occurrences
  // here.
  it('renders an empty router_sysname as "(no sysName TLV)", never a blank cell', () => {
    const dumpsBlank = collectionDumps.data.filter((r) => r.router_sysname === '').length
    const sessionsBlank = collectionSessions.data.filter((r) => r.router_sysname === '').length
    expect(dumpsBlank).toBeGreaterThan(0)
    expect(sessionsBlank).toBeGreaterThan(0)

    const w = mountMonitor()
    const occurrences = w.findAll('td').filter((td) => td.text() === '(no sysName TLV)')
    expect(occurrences.length).toBe(dumpsBlank + sessionsBlank)
  })

  it('offers only windows inside the daemon default clamp', () => {
    const w = mountMonitor()
    const values = w.findAll('[data-since-window] button').map((o) => o.attributes('value'))
    expect(values).toEqual(['1h', '6h', '24h'])
  })

  it('marks exactly one window as the chosen one', async () => {
    const w = mountMonitor()
    const pressed = () =>
      w
        .findAll('[data-since-window] button')
        .filter((b) => b.attributes('aria-pressed') === 'true')
        .map((b) => b.attributes('value'))
    expect(pressed()).toEqual(['1h'])
    await w.findAll('[data-since-window] button')[2].trigger('click')
    expect(pressed()).toEqual(['24h'])
  })

  it('states the window it covers, and states the right one', async () => {
    const w = mountMonitor()
    expect(w.find('[data-window-note]').text()).toContain('last hour')
    await w.findAll('[data-since-window] button')[1].trigger('click')
    expect(w.find('[data-window-note]').text()).toContain('last 6 hours')
    expect(w.find('[data-window-note]').text()).not.toContain('last hour')
  })
  // The navigation this screen initially shipped without, added later: the
  // reason for calling out that Grafana's panels are terminal, while this
  // navigation is the thing the UI can do that a dashboard structurally
  // cannot.
  //
  // The form is PeersView.vue:60's -- a #cell-<id> slot rendering a
  // RouterLink built from the row -- not a navigable row: DataTable.vue:87
  // spreads rowAttrs onto the <tr> only, so a row cannot carry a
  // destination of its own without changing the component all ten screens
  // share.
  function linksIn(w: VueWrapper, section: string): PropsReadable[] {
    // findAllComponents on a DOMWrapper is typed `any` by @vue/test-utils
    // itself (dist/src/domWrapper.d.ts), so the cast restores the one
    // thing these tests read -- props('to') -- rather than widening it.
    const scope = w.find(`[data-section="${section}"]`)
    return scope.findAllComponents(RouterLinkStub) as unknown as PropsReadable[]
  }

  // Scoped, not the bare list: /v1/peers takes `router` and usePeers has
  // always accepted it, so a click that names a router arrives somewhere
  // that answers FOR that router. Object query form rather than a template
  // string because a router address can be IPv6 and vue-router encodes an
  // object's query values.
  it("links every dumps router row into that router's peers", () => {
    const w = mountMonitor()
    expect(linksIn(w, 'dumps').map((l) => l.props('to'))).toEqual(
      collectionDumps.data.map((r) => ({ path: '/peers', query: { router: r.router_ip } })),
    )
  })

  it("links every sessions router row into that router's peers", () => {
    const w = mountMonitor()
    expect(linksIn(w, 'sessions').map((l) => l.props('to'))).toEqual(
      collectionSessions.data.map((r) => ({ path: '/peers', query: { router: r.router_ip } })),
    )
  })

  // Two destinations per peer row, in a fixed order. A Loc-RIB peer at
  // 0.0.0.0 (collection-locrib-2026-09-11.json opens with two) is not a dead
  // link either: it appears in /v1/peers under that exact address with
  // rib=loc_rib (peers.json holds one), so Peer detail has something to show
  // for it.
  it('links every Loc-RIB peer row into its peer detail and its session history', () => {
    const w = mountMonitor()
    expect(linksIn(w, 'locrib').map((l) => l.props('to'))).toEqual(
      collectionLocrib.data.flatMap((r) => [
        `/peers/${r.router_ip}/${r.peer_ip}`,
        { path: '/session-history', query: { router: r.router_ip, peer: r.peer_ip } },
      ]),
    )
  })

  // The churn-by-peer section is the fifth table, added after dumps,
  // sessions and Loc-RIB already linked out. Its rows
  // are PEER rows, so the same linking rule applies to them exactly as it does
  // to Loc-RIB's: peer detail AND session history. It shipped with the first and
  // not the second, which left one screen answering the same question two
  // ways depending on which table an operator happened to be reading.
  //
  // Three destinations per row, in column order (peer_asn, peer_ip,
  // router_sysname): the peer's detail, its history, then its router's peers.
  it('links every churn peer row into its peer detail, its session history and its router', () => {
    const w = mountMonitor()
    expect(linksIn(w, 'churn-peers').map((l) => l.props('to'))).toEqual(
      peerChurnFixture.data.flatMap((r) => [
        `/peers/${r.router_ip}/${r.peer_ip}`,
        { path: '/session-history', query: { router: r.router_ip, peer: r.peer_ip } },
        { path: '/peers', query: { router: r.router_ip } },
      ]),
    )
  })

  // The three tests above pin each destination against a literal this file
  // composes the same way the view does -- which proves the view agrees with
  // THIS FILE, and nothing about whether either agrees with router.ts. The
  // route table has no catch-all, so a destination that matches no route
  // renders a blank main area: no error, no 404, an operator staring at
  // chrome with an empty middle.
  //
  // Renaming router.ts's '/peers/:router/:peer' to '/peer/:router/:peer'
  // passed all 425 tests, MonitorView's 30 included, while making Peer
  // detail unreachable from every link in the app. router.test.ts would
  // ordinarily catch a rename like that, but it skips ':param' routes by
  // design; this is the guard for the destinations it leaves out.
  //
  // Every link in the view, not the three named above: a link added later
  // is covered without anyone remembering to extend this.
  it('points every link at a route the router actually declares', () => {
    const w = mountMonitor()
    const destinations = (
      w.findAllComponents(RouterLinkStub) as unknown as PropsReadable[]
    ).map((l) => l.props('to') as RouteLocationRaw)
    // Without this the check passes loudest when the view renders no links
    // at all -- the failure it is least able to notice on its own.
    expect(destinations.length).toBeGreaterThan(0)
    expect(unresolvable(destinations)).toEqual([])
  })

  // A parse-flag row names no router and no peer (FlagCount is {flag,
  // envelopes}), so there is no scope to carry anywhere. Linking it to
  // something fleet-wide would be a destination invented to make the
  // section match its three siblings.
  it('adds no link to a parse-flag row, which names no router and no peer', () => {
    const w = mountMonitor()
    expect(linksIn(w, 'flags').length).toBe(0)
  })
  // --- The tile row ---
  //
  // Every figure here is a total the response already carried --
  // meta.session_totals, meta.dump_totals, meta.flag_totals, and
  // /v1/routers' own per-router peer counts. StatTile computes nothing, so
  // a number on this row that no endpoint measured cannot appear.
  it('opens with the totals the four answers carried, not a figure it derived', () => {
    const w = mountMonitor()
    const tiles = w.findAll('[data-tile]')
    const byKey = Object.fromEntries(
      tiles.map((t) => [t.attributes('data-tile'), t.text()]),
    )
    // The fixtures' own totals, through the formatter that renders them --
    // hardcoding "1 208" here would pin formatCount's separator in a second
    // place, and it has its own test for that (lib/formatCount.test.ts).
    const st = collectionSessions.meta.session_totals
    const dt = collectionDumps.meta.dump_totals
    const ft = collectionFlags.meta.flag_totals
    expect(byKey.sessions).toContain(formatCount(st.sessions))
    expect(byKey.sessions).toContain(formatCount(st.up))
    expect(byKey.archived).toContain(formatCount(dt.archived))
    expect(byKey.archived).toContain(formatCount(dt.dumps))
    expect(byKey.flags).toContain(formatCount(ft.envelopes))
    // Peers are CURRENT state, from /v1/routers, which is a different fact
    // from the window's session count above.
    const up = bestVantageRouters(routersStale.data as Router[]).reduce((n, r) => n + r.peers_up, 0)
    expect(byKey.peers).toContain(formatCount(up))
    expect(byKey.peers).not.toMatch(/NaN/)
  })

  // At tile scale too: the fleet dump share is a composition, and the
  // tile states its two parts rather than a ratio. This is also what
  // keeps the "no composite score" percentage count below exact -- a
  // fleet-wide "51%" would be a second ratio on a screen whose only
  // legitimate one is per-row.
  it('states the fleet dump composition as its parts, never as a second ratio', () => {
    const w = mountMonitor()
    const tile = w.find('[data-tile="archived"]')
    expect(tile.text()).toMatch(/dumps/i)
    expect(tile.text()).toMatch(/changes/i)
    expect(tile.text()).not.toMatch(/%/)
  })

  it('shows recent fleet events beside the signals, on the same window', () => {
    const w = mountMonitor()
    const panel = w.find('[data-section="events"]')
    expect(panel.exists()).toBe(true)
    // The same ref the four signals move on: an events panel frozen at 1h
    // while the window says 6 hours is the defect the shared-since fix
    // closed for the other four.
    expect(capturedSince.events).toBe(capturedSince.dumps)
  })
  // PeersView fixed the same defect on 2026-09-21: two `formatRate`s
  // existed, one per screen, and only one of them had been corrected. A
  // lab archive's real rates are 0.000162 and 0.0000347, and toFixed(2)
  // draws both as "0.00" -- which on this very table means a peer that
  // archived nothing.
  //
  // One formatter in lib/ now, because the duplicate is what let the two
  // drift: a rule worth stating twice is worth stating once.
  it('never renders a nonzero archived rate as the zero that means "archived nothing"', () => {
    churnPeersFixture = {
      data: [{ ...peerChurnFixture.data[0], changes_per_second: 0.000162 }],
      meta: peerChurnFixture.meta,
    }
    const w = mountMonitor()
    const cell = w.get('[data-section="churn-peers"] tbody tr td:nth-child(5)')
    expect(cell.text()).not.toBe('0.00')
    expect(cell.text()).toBe('<0.01')
  })

  // The headline tile, and it was summing observers. /v1/routers is one
  // entry per (collector, router), so this screen added a dual-homed
  // router's peer counts together once per collector watching it. Measured
  // on a lab deployment 2026-09-21: the tile read "31 / 35" where the
  // archive holds 25 up of 28 distinct (router, peer, rib) identities --
  // six peers that do not exist, on the first number an operator reads.
  //
  // BEST SINGLE VANTAGE POINT is this screen's own remedy (measured
  // 2026-09-20): resolve each collector's view and report whole the
  // one that saw the most, argMax on (total, collector) so every number
  // comes from ONE collector's row. It can never inflate, it is exact when
  // collectors agree, and on a single-collector deployment it returns
  // exactly what summing returned.
  //
  // twoCollectorRouters' numbers separate all three candidate answers --
  // summing gives 4 of 8, a per-column max gives 3 of 7, and the correct
  // answer is coll-b's row whole.
  it('reports one router from its best-placed collector rather than summing its observers', () => {
    routersFixture = twoCollectorRouters()
    const w = mountMonitor()
    const tile = w.get('[data-tile="peers"]')
    expect(tile.text()).toContain('1 / 5')
    expect(tile.text()).not.toContain('4 / 8')
    expect(tile.text()).not.toContain('3 / 7')

    // The same answer whichever order the entries arrive in. /v1/routers
    // promises no row order, so "keep the last one seen" would be a coin
    // flip that happens to land right -- it agreed with the rule above
    // until this pair was reversed, which is how the mutation that keeps
    // the last row survived its first run.
    const flipped = twoCollectorRouters()
    routersFixture = { ...flipped, data: [...flipped.data].reverse() }
    expect(mountMonitor().get('[data-tile="peers"]').text()).toContain('1 / 5')
  })

  it('counts stale peers toward the total and never as up', () => {
    const base = twoCollectorRouters().data[0]
    routersFixture = {
      data: [{ ...base, collector: 'coll-a', peers_up: 1, peers_down: 0, peers_view_lost: 0, peers_stale: 2 }],
      meta: twoCollectorRouters().meta,
    }
    const tile = mountMonitor().get('[data-tile="peers"]')
    expect(tile.text()).toContain('1 / 3')
    expect(tile.text()).toMatch(/2 stale/)
  })

  // A duplicate-looking pair can render as two rows reading
  // "6:22:26 PM · collector lost view · 7a35fd594265 · 0.0.0.0", one above
  // the other, identical in every visible column. They are not a duplicate:
  // off the wire they are dev-c1 and dev-c2 with DIFFERENT session_ids --
  // two collectors that each lost their own transport. Same defect the
  // Peers table carried until 2026-09-20, and view_lost is the kind it
  // hurts most, being defined as one collector losing its own view.
  //
  // Real capture, four rows, identical in every visible field except the
  // collector.
  it('names the collector that saw each event, so two views of one peer are not identical rows', () => {
    eventsFixture = twoCollectors
    const w = mountMonitor()
    const texts = w.findAll('[data-section="events"] .event').map((e) => e.text())
    expect(texts).toHaveLength(4)
    expect(texts.filter((t) => t.includes('dev-c1'))).toHaveLength(2)
    expect(texts.filter((t) => t.includes('dev-c2'))).toHaveLength(2)
    expect(new Set(texts).size).toBe(4)
    // A feed has no column header, so the note is the only thing on screen
    // that says what that id at the end of a row IS. The Peers and Events
    // tables put the word above the same value; here it has to be said.
    expect(w.get('[data-section="events"] .note').text()).toMatch(/collector/i)
  })

  // Moving this panel into the rail beside the chart halved the width its
  // rows had: measured in a browser at 1920px, a `down` row needed ~695px
  // of a 680px column and wrapped mid-timestamp, breaking "Sep 20, 2026,
  // 6:01:01 PM" across two lines. So the reason moves to a line of its
  // own: a title line and a mono detail line under it.
  //
  // A reason line ONLY for a `down`. lib/eventReason.ts's rule is that the
  // KIND decides -- an `up` or a `view_lost` has no down_reason, it has an
  // absence -- so seven rows of "—" stacked under seven events would be an
  // absence rendered seven times as a fact.
  it('gives a down event its reason on a line of its own, and gives no other event one', () => {
    eventsFixture = { data: [downEvent, ...fleetEvents.data], meta: fleetEvents.meta }
    const w = mountMonitor()
    const rows = w.findAll('[data-section="events"] .event')
    expect(rows[0].find('[data-why]').text()).toContain('local system closed')
    expect(rows[0].find('[data-why]').text()).toContain('1')
    // Every other row in this window is an up, and carries no reason node
    // at all -- not an empty one, and not a dash.
    for (const row of rows.slice(1)) {
      expect(row.find('[data-why]').exists()).toBe(false)
    }
    expect(w.find('[data-section="events"]').text()).not.toContain('—')
  })

  // The churn panel is the first chart in this UI, and the rule it has to
  // hold is the one every windowed number here holds: the label comes from
  // the ANSWER, never from the request. They agree until a clamp disagrees.
  it('labels the chart with the bucket the answer reported, not the one it asked for', () => {
    const w = mountMonitor()
    const panel = w.find('[data-section="churn"]')
    expect(panel.exists()).toBe(true)
    expect(panel.text()).toContain('2m0s')
    // And the request itself moved with the window control rather than
    // being fixed: a 1h window gets finer bars than a 24h one.
    expect(capturedChurnBucket?.value).toBe('2m')
    expect(capturedSince.churn).toBe(capturedSince.dumps)
  })

  // The axis is drawn between the bounds the ANSWER reports, never between
  // the browser's own subtraction.
  //
  // Every ts in the data is a COLLECTOR timestamp. Until /v1/collection/churn
  // carried its window, this screen computed one from `new Date()` and drew
  // collector time against browser time -- a laptop a few minutes off shifted
  // every bar against its own gridline, and nothing on screen could show it.
  // The daemon now sends both bounds, and the same rule already applied to
  // the bucket width applies to them: render what the answer reported, not
  // what the request asked for.
  it('draws the axis between the window the answer reported, not the browser clock', () => {
    churnWindow = {
      churn_from: '2020-01-01T00:00:00Z',
      churn_to: '2020-01-01T06:00:00Z',
    }
    const w = mountMonitor()
    const chart = w.findComponent({ name: 'ChurnChart' })
    expect(chart.props('from')).toBe('2020-01-01T00:00:00Z')
    expect(chart.props('to')).toBe('2020-01-01T06:00:00Z')
  })

  it('draws a bar per bucket the churn answer carried', () => {
    const w = mountMonitor()
    expect(w.findAll('[data-section="churn"] [data-bar]')).toHaveLength(churnFixture.length)
  })
})

// --- Peers by update volume ---
//
// A ranked table, not the time series route-churn.json draws. They
// share a classification and not a shape.
describe('MonitorView peers by update volume', () => {
  beforeEach(() => {
    dumpsFixture = collectionDumps
    dumpsPending = false
    dumpsErr = undefined
    sessionsFixture = collectionSessions
    sessionsPending = false
    sessionsErr = undefined
    locribFixture = collectionLocrib
    locribPending = false
    locribErr = undefined
    flagsFixture = collectionFlags
    flagsPending = false
    flagsErr = undefined
    routersFixture = routersStale
    eventsFixture = fleetEvents
    churnPeersFixture = peerChurnFixture
    churnPeersPending = false
    churnPeersErr = undefined
    joinPeersFixture = joinPeers
  })

  // The order is the ANSWER's, rendered as it arrives. The endpoint ranks by
  // readvertise + withdraw -- what the network did -- and deliberately not by
  // every row archived, because a peer whose session restarted once outranks
  // a genuinely busy one on total rows. peerChurnFixture's last row has 900
  // dumps and 1 change for exactly this reason: a table that re-sorted on the
  // total, or that fell back to fixture order, puts it first.
  it('renders the ranking the answer carried, busiest by change first', () => {
    const w = mountMonitor()
    const rows = w.findAll('[data-section="churn-peers"] tbody tr')
    expect(rows.length).toBe(peerChurnFixture.data.length)
    expect(rows[0].text()).toContain('10.255.0.2')
    expect(rows[rows.length - 1].text()).toContain('10.2.0.2')
  })

  // A count of rows this collector STORED must not be rendered under a
  // label claiming a property of the network. The endpoint's own contract
  // says so outright, and the column header is where an operator actually
  // reads it.
  it('names its rate a collection rate, never the router update rate', () => {
    const w = mountMonitor()
    // The COLUMN HEADER, not the section text. The panel's note says the
    // words "not the router's update rate" on purpose, and an assertion over
    // the whole section forbids the honest disclaimer along with the lie --
    // which is what this test did on its first writing.
    const headers = w
      .findAll('[data-section="churn-peers"] thead th')
      .map((th) => th.text())
    expect(headers).toContain('Archived/s')
    expect(headers.join(' ')).not.toMatch(/updates/i)
    // And the disclaimer is present, in the panel rather than in a tooltip.
    expect(w.find('[data-churn-peers-note]').text()).toMatch(/not the router/i)
  })

  // State and route count come from /v1/peers, joined on (router, peer) --
  // churn knows what a peer SENT and nothing about what it holds now.
  it('joins state and prefixes from /v1/peers on the router and peer together', () => {
    const w = mountMonitor()
    const first = w.findAll('[data-section="churn-peers"] tbody tr')[0]
    expect(first.text()).toContain('812')
    const second = w.findAll('[data-section="churn-peers"] tbody tr')[1]
    // 10.255.0.3 is down with 0 routes: a join keyed on peer_ip alone, or one
    // that took the first peers row regardless, would show 812 here too.
    expect(second.text()).toContain('down')
  })

  // api/openapi.yaml documents /v1/peers as "one entry per (collector,
  // router, peer, rib)". A peer mirrored pre- AND post-policy therefore has
  // two real route counts, and they are not addable -- the same prefixes seen
  // twice. This lab holds no such peer today, which is why the fixture states
  // the shape rather than waiting for one.
  it('declines to state one route count for a peer that has two', () => {
    joinPeersFixture = joinPeersAmbiguous
    const w = mountMonitor()
    const first = w.findAll('[data-section="churn-peers"] tbody tr')[0]
    expect(first.text()).not.toContain('812')
    expect(first.text()).not.toContain('856') // 812 + 44, the sum that is wrong
    expect(first.find('[data-multi-rib]').exists()).toBe(true)
  })
})

// The width invariant a browser found and jsdom can still hold.
//
// Seven columns shipped as 84/104/104/104/132px plus 18%/16%: the fixed
// columns alone were 528px of a ~642px panel in this screen's auto-fit grid,
// so table-layout:fixed shrank the rest and the Peer cell rendered "17..."
// for 172.31.0.90. Every one of the 440 tests passed, because jsdom computes
// no layout.
//
// What jsdom CAN check is the declared set, and percentages summing to
// exactly 100 are what makes the table correct at every panel width this grid
// produces: over 100 clips at the narrow end, under 100 smears at the wide
// end -- /routes shipped the second of those once already.
describe('MonitorView peers-by-volume column widths', () => {
  it('declares percentage widths that sum to exactly 100', () => {
    const w = mountMonitor()
    const widths = (columnsOfNamed(w, 'churn-peers') as { width?: string }[]).map((c) => c.width)
    expect(widths.every((x) => typeof x === 'string' && x.endsWith('%'))).toBe(true)
    const total = widths.reduce((n, x) => n + parseFloat(x!), 0)
    expect(total).toBe(100)
  })

  // On a 390px phone the grid's 620px panels stood 297px past their column,
  // and once they fit it, their tables shrank until addresses read
  // "172.2...". Each table now has a floor, below which it scrolls inside
  // its own card. The floor must never bind on a desktop, where a grid panel
  // is at least 620px wide and its table therefore 590px (620 less 14px
  // padding and a 1px border each side). Whether the floor is wide enough
  // for a phone is a browser measurement; jsdom computes no layout.
  it('floors every table without narrowing any on a desktop', () => {
    const w = mountMonitor()
    for (const section of ['churn-peers', 'dumps', 'sessions', 'locrib', 'flags']) {
      const floor = tableMinWidthPx(w.get(`[data-section="${section}"]`) as never)
      expect(floor, section).toBeDefined()
      if (section !== 'churn-peers') expect(floor!, section).toBeLessThanOrEqual(590)
    }
  })
})
