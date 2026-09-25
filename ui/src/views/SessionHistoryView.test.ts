import { type VueWrapper, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { type Ref, nextTick, reactive, ref } from 'vue'
import events from '@/api/fixtures/events.json'
import twoCollectors from '@/api/fixtures/events-two-collectors.json'
import routers from '@/api/fixtures/routers.json'
import peers from '@/api/fixtures/peers.json'
import { columnWidths, columnsOf } from '@/test-support/columnsOf'
import { inventedColumns } from '@/test-support/columnGuard'
import type { EventScope } from '@/api/queries'

// Set inside each test below, never left at these initial values: the
// isolation rule requires every test to pass alone via `-t`, so each test
// assigns every one of these itself before mounting rather than relying on
// what a previous test in file order happened to leave behind.
let scopeChosen = false
let eventsFixture: { data: unknown[]; meta: unknown } = events
let peersFixture: unknown = peers
let routersFixture: unknown = routers
const loadMore = vi.fn()
const reload = vi.fn()

// useEvents hands the component a ref (the `since` window) and this repo's
// own useRibPage-derived contract: nothing inside useEvents watches either
// argument, so a test asserting "the window change reloaded the walk" needs
// the SAME ref instance the component holds, captured off the mocked call.
let capturedSince: Ref<string> | undefined

// The scope ref the screen handed useEvents. Read rather than inferred from
// what renders: the mocked walk returns rows regardless of scope, so only
// this says WHICH pair was actually asked about.
let capturedScope: Ref<EventScope | undefined> | undefined

// Same mechanism RoutesView.test.ts documents: usePeers hands the component
// a ref, and reassigning this module variable to the SAME object the
// component holds lets a test push a LATER value into it after mount, which
// is how /v1/peers resolving a moment after the screen renders actually
// behaves.
let peersRef = ref<unknown>(peers)

// A route whose query a test sets BEFORE mounting, mirroring an operator
// opening a shared link. Vue Router keeps one reactive route object for the
// app's lifetime (RoutesView.test.ts and PeerDetailView.test.ts document the
// same shape).
const route = reactive({ query: {} as Record<string, string> })
const push = vi.fn()
const replace = vi.fn()
vi.mock('vue-router', () => ({
  useRoute: () => route,
  useRouter: () => ({ push, replace }),
}))

vi.mock('@/api/queries', () => ({
  useRouters: () => ({ data: ref(routersFixture), isLoading: ref(false), error: ref(undefined) }),
  usePeers: () => {
    peersRef = ref(peersFixture)
    return { data: peersRef, isLoading: ref(false), error: ref(undefined) }
  },
  useEvents: (scope: Ref<EventScope | undefined>, since: Ref<string>) => {
    capturedScope = scope
    capturedSince = since
    return {
      rows: ref(scopeChosen ? eventsFixture.data : []),
      meta: ref(scopeChosen ? eventsFixture.meta : undefined),
      loading: ref(false),
      error: ref(undefined),
      hasMore: ref(false),
      reload,
      loadMore,
    }
  },
}))

const SessionHistoryView = (await import('./SessionHistoryView.vue')).default

// `scopeChosen` only controls what the MOCKED useEvents hands back -- it
// says nothing about SessionHistoryView's own `scope` ref, which gates the
// whole v-else block and is set only by ScopePicker's real, unmocked
// `scope` event. That event fires only once ScopePicker's internal watch
// sees both a router and a peer selected, so a test that wants past the
// "choose a router and peer" gate has to drive the two <select> elements
// themselves, exactly the way an operator's click would -- the same helper
// RoutesView.test.ts uses.
async function chooseScope(w: VueWrapper) {
  const selects = w.findAll('select')
  await selects[0].setValue(routers.data[0].ip)
  await selects[1].setValue(peers.data[1].peer_ip)
}

// events.json's own 6 rows, real captured peer_events (see
// ui/src/api/fixtures/README.md's "events.json" section): 3 view_lost, 3
// up, newest first, spanning three real BMP sessions closed by a SIGINT to
// the sender. Referenced directly rather than copied, so the view_lost
// tests below drive from captured data, not a labeled synthesis.
const viewLostFixture = events

// SYNTHESIZED, not captured: no capture in this repo carries kind
// "unspecified" -- events.json's lab deployment produced only
// up/view_lost, and a standing lab archive (see the same README section)
// has only up/down.
// Built from events.json's own "up" row with only `kind` changed, so every
// other field -- router, peer, ASN, ports -- stays a real captured value.
// down_reason/reason_name are left at the real row's own 0/"" rather than
// asserted afresh, because api/types.go's reasonName already returns "" for
// any kind that is not "down".
const unspecifiedFixture = {
  data: [{ ...events.data[1], kind: 'unspecified' }],
  meta: events.meta,
}

// SYNTHESIZED, not captured: events.json has no "down" row at all (see its
// README section -- the lab deployment that captured it stopped a session
// with SIGINT, which the collector marks view_lost, not down). Built
// from events.json's own "up" row with kind and down_reason changed to a
// real registry entry, api/types.go's own peerDownReasons[3]: "remote system closed,
// notification follows" -- exactly the case the screen has to say it
// cannot go past, because down_data (the NOTIFICATION PDU itself) has no
// column in peer_events.
const notificationFixture = {
  data: [
    {
      ...events.data[1],
      kind: 'down',
      down_reason: 3,
      reason_name: 'remote system closed, notification follows',
    },
  ],
  meta: events.meta,
}

// SYNTHESIZED, not captured, for notificationFixture's own reason: no
// capture in this repo carries a "down" at all, let alone one whose
// down_reason falls outside api/types.go's six-entry peerDownReasons. Built
// from events.json's own "up" row with kind and down_reason changed, and
// reason_name left at the real row's own "" -- which is exactly what the API
// returns for an unregistered code (TestEventsUnknownReasonCodeIsUnnamedNotGuessed),
// so this is the wire shape, not a UI-side invention. Capturing it instead
// would need a router that sends a Peer Down with a reason outside RFC 7854's
// four plus this project's two.
const unregisteredReasonFixture = {
  data: [{ ...events.data[1], kind: 'down', down_reason: 99, reason_name: '' }],
  meta: events.meta,
}

describe('SessionHistoryView', () => {
  // The route object and the navigation/reload spies are module-scoped and
  // shared, so they are reset here rather than in each test: a query or a
  // call count left behind by one test is exactly the order-dependence this
  // suite's isolation rule exists to prevent, and it is invisible until the
  // tests run in a different order.
  beforeEach(() => {
    route.query = {}
    push.mockClear()
    replace.mockClear()
    reload.mockClear()
    loadMore.mockClear()
    capturedScope = undefined
  })

  it('asks for a scope before fetching, because router and peer are required', () => {
    // /v1/events without router and peer is a 400. Rendering an empty table
    // on load would tell an operator this session has no history, when in
    // fact nothing was ever asked.
    scopeChosen = false
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    expect(w.text()).toMatch(/choose a router and peer/i)
    expect(w.find('tbody tr').exists()).toBe(false)
  })

  it('names a router that sent no sysName instead of leaving the cell blank', async () => {
    // As the three sibling screens. This table also has no address column
    // beside the name, so a blank Router cell identifies nothing.
    scopeChosen = true
    eventsFixture = {
      data: [{ ...(events.data[0] as Record<string, unknown>), router_sysname: '' }],
      meta: events.meta,
    }
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.text()).toContain('(no sysName TLV)')
  })

  it('renders view_lost as the collector losing its view, not as a down', async () => {
    // These are separate facts and the pipeline went to deliberate trouble
    // to keep them separate: a Peer Down is the router stating a session
    // dropped; view_lost is the collector saying it stopped being able to
    // see the peer.
    scopeChosen = true
    eventsFixture = viewLostFixture
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const row = w.find('[data-kind="view_lost"]')
    expect(row.exists()).toBe(true)
    expect(row.text()).toMatch(/collector/i)
    expect(row.text()).not.toMatch(/\bdown\b/i)
  })

  // The scope here is ONE (router, peer) pair, which is exactly why this
  // screen needs the collector most: a scoped walk still spans collectors
  // -- verified on the wire, GET /v1/events?router=&peer= returns dev-c1's
  // and dev-c2's rows interleaved -- so what looks like one peer's session
  // lifecycle is TWO lifecycles, with different session_ids, read as one.
  //
  // Real capture, four rows: two collectors, an up and a view_lost each,
  // identical in every other visible field. A column rendering something
  // else that happened to differ could not pass this.
  it('names the collector on a scoped walk, which spans collectors even though the pair does not', async () => {
    scopeChosen = true
    eventsFixture = twoCollectors
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const texts = w.findAll('tbody tr').map((r) => r.text())
    expect(texts).toHaveLength(4)
    expect(texts.filter((t) => t.includes('dev-c1'))).toHaveLength(2)
    expect(texts.filter((t) => t.includes('dev-c2'))).toHaveLength(2)
    expect(new Set(texts).size).toBe(4)
  })

  // ScopePicker's advisory was written for the Routes screen, where
  // /v1/rib/* really is pinned to one collector (it took collector= on
  // 2026-09-20). It is FALSE here: /v1/events takes no collector= at all, and
  // the table below it shows both collectors' rows -- which only became
  // visible once each row named its collector. The Collector select is dead
  // here for the same reason: EventScope carries no collector and
  // useEvents' fetchPage sends router, peer, since, cursor and limit.
  //
  // Same synthesis RoutesView.test.ts and PeersView.test.ts use: one
  // captured peer row duplicated under a second collector with its own
  // session, which is what /v1/peers returns for a dual-homed router.
  it('says both collectors are shown rather than claiming to walk one, because the walk is not pinned', async () => {
    scopeChosen = true
    eventsFixture = twoCollectors
    const base = peers.data[1]
    peersFixture = {
      data: [
        { ...base, collector: 'coll-a', session_id: '111', state: 'up' },
        { ...base, collector: 'coll-b', session_id: '222', state: 'view_lost' },
      ],
      meta: peers.meta,
    }
    routersFixture = routers
    const w = mount(SessionHistoryView)
    const selects = w.findAll('select')
    await selects[0].setValue(base.router_ip)
    await selects[1].setValue(base.peer_ip)
    await nextTick()

    const banner = w.find('.ambiguous')
    expect(banner.exists()).toBe(true)
    expect(banner.text()).toMatch(/disagree/i)
    expect(
      banner.text(),
      'the Routes copy claims a narrowing this screen never performs',
    ).not.toMatch(/walking/i)
    expect(banner.text()).toMatch(/both collectors/i)
    // And no control offering a pin nothing applies.
    expect(w.find('select[data-collector]').exists()).toBe(false)
  })

  it('shows no reason for a view_lost, because zero there is absence', async () => {
    scopeChosen = true
    eventsFixture = viewLostFixture
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const row = w.find('[data-kind="view_lost"]')
    // Not "0", and not a reason name: the router made no statement.
    expect(row.find('[data-reason]').text().trim()).toBe('—')
  })

  it('renders unspecified as itself, never as a settled kind', async () => {
    scopeChosen = true
    eventsFixture = unspecifiedFixture // synthesized from a real row, labeled inline above
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.find('[data-kind="unspecified"]').exists()).toBe(true)
  })

  it('renders no column the API does not measure', async () => {
    // SessionHistoryView is a scoped screen like RoutesView, not an
    // always-fetching one like RoutersView/PeersView, so DataTable is
    // not in the tree until a scope resolves (see the "asks
    // for a scope" test above) and there is nothing for columnsOf to find
    // before that. RoutesView.test.ts's own version of this test resolves
    // the scope first for the identical reason.
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const columns = columnsOf(w)
    expect(inventedColumns(columns, events.data[0])).toEqual([])
  })

  // Presentation, not invention: the fixture's ts_collector is
  // "2026-09-08T01:11:42.295926Z". An operator reads a clock, not a string
  // with a T and a Z and six digits of sub-second precision in it. Computed
  // the same way the screen computes it, the same reasoning RoutersView's
  // own version of this test gives for not hardcoding one locale's output.
  it('renders ts_collector as a readable clock string, not the raw ISO instant', async () => {
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const expected = new Date(events.data[0].ts_collector).toLocaleString([], {
      dateStyle: 'medium',
      timeStyle: 'medium',
    })
    expect(w.text()).toContain(expected)
    expect(w.text()).not.toContain(events.data[0].ts_collector)
  })

  it('says the notification itself is not retained', async () => {
    // "notification follows" is exactly as far as this archive can report:
    // down_data has no column in peer_events.
    scopeChosen = true
    eventsFixture = notificationFixture
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.text()).toMatch(/not retained|not stored/i)
  })

  it('keeps the reason code beside its name, so the decoding can be checked', async () => {
    // /v1/events' own contract, which this column is the last inch of: the
    // registry name AND the number, "so a caller is never forced to trust
    // the decoding" (api/types.go's reasonName). The name alone is the API
    // honoring that and the screen dropping it.
    scopeChosen = true
    eventsFixture = notificationFixture // synthesized from a real row, labeled above
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const reason = w.find('[data-kind="down"] [data-reason]').text()
    expect(reason).toContain('3')
    expect(reason).toContain('remote system closed, notification follows')
  })

  it('says an unregistered code has no name, rather than showing a bare number', async () => {
    // The half a bare number cannot say. Without this, "2" and "local system
    // closed, FSM event follows" sit in one column meaning different kinds of
    // thing, and an operator has no way to tell that 2 is a code this build
    // has no name for rather than the reason itself.
    scopeChosen = true
    eventsFixture = unregisteredReasonFixture // synthesized, labeled above
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const reason = w.find('[data-kind="down"] [data-reason]').text()
    expect(reason).toContain('99')
    expect(reason).toMatch(/no name/i)
    // And not a guess: an unregistered code gets no registry name invented
    // for it here any more than it does in api/types.go's reasonName.
    expect(reason.trim()).not.toBe('99')
  })

  it('states the 90-day retention bound beside the results', async () => {
    // peer_events carries a TTL of 90 days on ts_collector. An operator
    // asking about a session from before that gets an empty list for a
    // reason this screen has to say out loud, or the emptiness reads as
    // "nothing happened" -- this project's defect class, arriving through
    // retention instead of through a query.
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.text()).toMatch(/90 day/i)
  })

  it('states the retention bound even when the result is empty, not only when rows exist', async () => {
    // The claim above is worthless if it only shows up beside a populated
    // table -- the empty case is the exact one an operator needs it for.
    scopeChosen = true
    eventsFixture = { data: [], meta: { next_cursor: null, warnings: [], total_matched: null } }
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.find('tbody tr').exists()).toBe(false)
    expect(w.text()).toMatch(/90 day/i)
  })

  it('offers only since= windows the daemon can actually parse', async () => {
    // api/handlers.go's since() falls through to Go's time.ParseDuration,
    // which knows ns/us/ms/s/m/h and no day unit at all: "7d" is a 400 an
    // operator could neither predict nor explain. Same guard
    // LookingGlassView.test.ts already pins on its own window picker.
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const values = w
      .findAll('[data-since-window] button')
      .map((o) => (o.element as HTMLOptionElement).value)
    expect(values.length).toBeGreaterThan(1)
    for (const v of values) expect(v).toMatch(/^\d+(ns|us|ms|s|m|h)$/)
  })

  it('reloads the walk when the window changes, since useEvents never watches it on its own', async () => {
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const before = reload.mock.calls.length
    await w
      .findAll('[data-since-window] button')
      .find((b) => b.attributes('value') === '168h')!
      .trigger('click')
    expect(reload.mock.calls.length).toBeGreaterThan(before)
    expect(capturedSince?.value).toBe('168h')
  })

  it('names the window actually selected in the archive note, not a stale or literal one', async () => {
    // EventArchiveNote.test.ts proves the component renders whatever
    // windowLabel prop it is handed; this proves SessionHistoryView hands it
    // the window actually in use. A hard-coded label or a ref captured once
    // at mount would pass every other test here and still tell an operator
    // the wrong window aged an empty result out of retention.
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.find('[data-archive-note]').text()).toMatch(/last 24 hours/i)
    await w
      .findAll('[data-since-window] button')
      .find((b) => b.attributes('value') === '168h')!
      .trigger('click')
    expect(w.find('[data-archive-note]').text()).toMatch(/last 7 days/i)
    expect(w.find('[data-archive-note]').text()).not.toMatch(/last 24 hours/i)
  })

  it('puts the question in the URL and never the session', async () => {
    // session_id is a runtime identity discovered at fetch time; a link
    // carrying one would let a walk resume against a session that may have
    // ended. Same guard, same reasoning as RoutesView.test.ts.
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = {}
    push.mockClear()

    const w = mount(SessionHistoryView)
    await chooseScope(w)
    await nextTick()

    expect(push).toHaveBeenCalled()
    const q = push.mock.calls.at(-1)![0].query as Record<string, string>
    expect(q.router).toBe(routers.data[0].ip)
    expect(q.peer).toBe(peers.data[1].peer_ip)
    expect(Object.keys(q)).not.toContain('session')
    expect(JSON.stringify(q)).not.toContain(peers.data[1].session_id)
  })

  it('restores a scope from the URL, even when the peer list arrives after the screen', async () => {
    // The case a shared link actually hits: /session-history?router=..&peer=..
    // renders before /v1/peers resolves, so at first paint the seeded peer
    // matches no row. Same defect, same fix as RoutesView.test.ts.
    //
    // `undefined`, not `{ data: [], meta }`, for the not-yet-answered state:
    // Pinia Colada leaves `data` undefined until the first response, and an
    // answer holding no rows is a different fact -- it says this router has
    // no current sessions, which makes a seeded peer an ARCHIVED one this
    // screen now answers for (the test directly below). Both readings
    // rendered the same gate before that existed, so the imprecision here
    // was invisible; it is not any more, and the fixture has to mean one.
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = undefined
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    const w = mount(SessionHistoryView)
    expect(w.text()).toMatch(/choose a router and peer/i)
    expect(replace).not.toHaveBeenCalled()

    peersRef.value = peers
    await nextTick()
    await nextTick()

    expect(w.text()).not.toMatch(/choose a router and peer/i)
    expect(w.find('tbody tr').exists()).toBe(true)
  })

  // The other reading of the same shape, pinned so the two cannot be
  // confused again: /v1/peers ANSWERED and holds no row for this router, so
  // the seeded peer has no current session -- which is a fact, not a
  // pending state, and the archive is asked.
  it('treats an answered but empty peer list as evidence, not as a pending one', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = { data: [], meta: peers.meta }
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    const w = mount(SessionHistoryView)
    await nextTick()
    await nextTick()

    expect(w.find('[data-archived-scope]').exists()).toBe(true)
    expect(capturedScope?.value).toMatchObject({ peer: peers.data[1].peer_ip })
  })

  it('follows the URL when the back button changes the scope', async () => {
    // A query-only navigation does not remount, so a scope read once at
    // setup would leave the picker and the table showing the previous
    // scope while the URL names another.
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    const w = mount(SessionHistoryView)
    await nextTick()
    expect((w.findAll('select')[1].element as HTMLSelectElement).value).toBe(peers.data[1].peer_ip)

    route.query = { router: routers.data[0].ip, peer: peers.data[0].peer_ip }
    await nextTick()
    await nextTick()

    expect((w.findAll('select')[1].element as HTMLSelectElement).value).toBe(peers.data[0].peer_ip)
  })

  it('clears the scope when a complete pick becomes incomplete, rather than leaving a stale answer on screen', async () => {
    scopeChosen = true
    eventsFixture = events
    peersFixture = peers
    routersFixture = routers
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    expect(w.find('tbody tr').exists()).toBe(true)

    await w.findAll('select')[1].setValue('')
    expect(w.text()).toMatch(/choose a router and peer/i)
    expect(w.find('tbody tr').exists()).toBe(false)
  })

  it('does not re-walk when the peer list refetches without changing the scope', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: peers.data[1].peer_ip }

    mount(SessionHistoryView)
    await nextTick()
    const afterRestore = reload.mock.calls.length

    peersRef.value = { data: [...peers.data], meta: peers.meta }
    await nextTick()
    await nextTick()

    expect(reload.mock.calls.length).toBe(afterRestore)
  })
  // --- A pair the picker cannot offer ---
  //
  // ScopePicker resolves a peer against /v1/peers, which is CURRENT-session
  // state ("Per-(router, peer, rib) state as of the current session", its
  // own OpenAPI summary). This screen's question is the opposite one: what
  // happened to a session, which most often means one that has ended.
  //
  // api/openapi.yaml settles that /v1/events can answer it -- this walk
  // "in either mode -- is not pinned to one BMP session ... The scoped
  // cursor carries no session" -- and useEvents sends router, peer and
  // since and nothing else, so EventScope's `session` is shape, not
  // question. Confirmed against a live lab archive: 10.0.103.61/10.9.9.2
  // holds three events while appearing in no /v1/peers row for that router.
  //
  // 10.9.9.2 below is that same shape against peers.json, whose only live
  // peers on this router are 0.0.0.0 and 172.31.0.90.
  it('answers for a pair whose session has ended, which the picker cannot offer', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: '10.9.9.2' }

    const w = mount(SessionHistoryView)
    await nextTick()
    await nextTick()

    expect(capturedScope?.value).toMatchObject({ router: routers.data[0].ip, peer: '10.9.9.2' })
    expect(reload).toHaveBeenCalled()
    expect(w.find('tbody tr').exists()).toBe(true)
  })

  it('says the pair has no current session, rather than asking for one again', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: '10.9.9.2' }

    const w = mount(SessionHistoryView)
    await nextTick()
    await nextTick()

    const note = w.find('[data-archived-scope]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('10.9.9.2')
    expect(note.text()).toContain(routers.data[0].ip)
    expect(w.text()).not.toMatch(/choose a router and peer/i)
  })

  // The claim needs evidence. Before /v1/peers has answered there is no
  // basis for saying a peer has no current session -- Colada leaves `data`
  // undefined until the first response, which is a different state from a
  // response holding no rows.
  it('claims nothing about a seeded pair before /v1/peers has answered', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = undefined
    route.query = { router: routers.data[0].ip, peer: '10.9.9.2' }

    const w = mount(SessionHistoryView)
    await nextTick()

    expect(w.find('[data-archived-scope]').exists()).toBe(false)
    expect(capturedScope?.value).toBeUndefined()
    expect(w.text()).toMatch(/choose a router and peer/i)
  })

  // Found in a browser, invisible to every assertion above: a <select>
  // whose v-model holds a value no <option> carries renders BLANK, not "—".
  // Beside a populated Router dropdown that reads as a broken control, on
  // the one screen that deliberately puts a peer on display which the list
  // cannot contain. Disabled rather than selectable, because the pair is
  // not a scope a caller can act on -- RoutesView shares this picker and
  // must not be able to start a RIB walk against a session that has ended.
  it('names a seeded peer the list cannot offer, instead of rendering a blank control', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: '10.9.9.2' }

    const w = mount(SessionHistoryView)
    await nextTick()
    await nextTick()

    const peerSelect = w.findAll('select')[1].element as HTMLSelectElement
    expect(peerSelect.selectedIndex).toBeGreaterThanOrEqual(0)
    const chosen = peerSelect.options[peerSelect.selectedIndex]
    expect(chosen.text).toContain('10.9.9.2')
    expect(chosen.disabled).toBe(true)
  })

  // The URL's pair governs only until the operator touches a dropdown.
  // Without this the seeded answer would sit under pickers that name
  // something else -- the same stale-answer failure the "complete pick
  // becomes incomplete" test above exists to prevent, arriving by the
  // other door.
  it('hands the question back to the pickers once one is touched', async () => {
    scopeChosen = true
    eventsFixture = events
    routersFixture = routers
    peersFixture = peers
    route.query = { router: routers.data[0].ip, peer: '10.9.9.2' }

    const w = mount(SessionHistoryView)
    await nextTick()
    await nextTick()
    expect(w.find('[data-archived-scope]').exists()).toBe(true)

    await w.findAll('select')[1].setValue(peers.data[1].peer_ip)
    await nextTick()

    expect(w.find('[data-archived-scope]').exists()).toBe(false)
    expect(capturedScope?.value).toMatchObject({ peer: peers.data[1].peer_ip })
  })

  // The smear guard. Declared widths are what keep a table packed instead of
  // spread across a 2000px viewport; see columnWidths' own note for the
  // measurement that prompted it.
  it('declares a width for every column, so the table cannot smear', async () => {
    // The table exists only once a scope does: /v1/events refuses an
    // unscoped request, so this screen renders a prompt until both selects
    // are driven -- the same reason every other test here calls
    // chooseScope.
    scopeChosen = true
    const w = mount(SessionHistoryView)
    await chooseScope(w)
    const widths = columnWidths(w)
    expect(widths.length).toBeGreaterThan(0)
    expect(widths.filter((x) => !x)).toEqual([])
  })
})
