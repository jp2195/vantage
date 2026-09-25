import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import type { Meta } from '@/api/generated'
import { PiniaColada } from '@pinia/colada'
import { effectScope } from 'vue'
import collectionDumpsFixture from './fixtures/collection-dumps.json'
import collectionFlagsFixture from './fixtures/collection-flags.json'
import collectionLocribFixture from './fixtures/collection-locrib.json'
import collectionSessionsFixture from './fixtures/collection-sessions.json'
import collectorsStale from './fixtures/collectors-stale.json'
import rib from './fixtures/rib-unicast.json'
import routers from './fixtures/routers.json'
import routersStale from './fixtures/routers-stale.json'
import topology from './fixtures/topology.json'

// Fake timers are a global mode switch, not test-local state, but this
// still keeps to the isolation rule: it guarantees real timers are back
// before the NEXT test runs regardless of how this one finished, rather
// than leaving that to be remembered inside each test that uses them.
afterEach(() => {
  vi.useRealTimers()
})

// vi.mock is hoisted above these imports by Vitest's transform, so the
// factory below cannot close over an ordinary top-level `const` declared
// after it -- it has to be one vi.hoisted() gives it, or the reference
// would run before the variable exists.
const {
  ribUnicastMock,
  findRoutesMock,
  routeHistoryMock,
  findPeerEventsMock,
  findTopologyMock,
  findLsNodesMock,
  findLsLinksMock,
  findLsPrefixesMock,
  collectionDumpsMock,
  collectionSessionsMock,
  collectionLocRibMock,
  collectionFlagsMock,
  listRoutersMock,
  listCollectorsMock,
  listAsNamesMock,
} = vi.hoisted(() => ({
  ribUnicastMock: vi.fn(),
  findRoutesMock: vi.fn(),
  routeHistoryMock: vi.fn(),
  findPeerEventsMock: vi.fn(),
  findTopologyMock: vi.fn(),
  findLsNodesMock: vi.fn(),
  findLsLinksMock: vi.fn(),
  findLsPrefixesMock: vi.fn(),
  collectionDumpsMock: vi.fn(),
  collectionSessionsMock: vi.fn(),
  collectionLocRibMock: vi.fn(),
  collectionFlagsMock: vi.fn(),
  listRoutersMock: vi.fn(),
  listCollectorsMock: vi.fn(),
  listAsNamesMock: vi.fn(),
}))

// Stubs the whole generated client so this test needs no server. Only
// ribUnicast's behavior matters below; the rest are unused by this file's
// tests but still have to exist, because queries.ts imports all of them at
// module scope and a missing export would be `undefined` at the call site.
vi.mock('./generated', () => ({
  listRouters: listRoutersMock,
  listPeers: vi.fn(),
  findRoutes: findRoutesMock,
  ribUnicast: ribUnicastMock,
  ribVpn: vi.fn(),
  ribEvpn: vi.fn(),
  routeHistory: routeHistoryMock,
  findPeerEvents: findPeerEventsMock,
  findTopology: findTopologyMock,
  findLsNodes: findLsNodesMock,
  findLsLinks: findLsLinksMock,
  findLsPrefixes: findLsPrefixesMock,
  // Absent until now, and silently: this factory replaces the whole
  // generated module, so an operation missing from it is `undefined` at
  // queries.ts's own import -- exactly the trap the comment above names.
  // Nothing in this file called the four collection operations, so nothing
  // noticed.
  collectionDumps: collectionDumpsMock,
  collectionSessions: collectionSessionsMock,
  collectionLocRib: collectionLocRibMock,
  collectionFlags: collectionFlagsMock,
  // Also absent until now, for the identical reason: queries.ts's own
  // useAsNames imports this at module scope, so a factory that omitted it
  // would hand every OTHER composable in this file `listAsNames: undefined`
  // rather than merely leaving useAsNames untested.
  listAsNames: listAsNamesMock,
  listCollectors: listCollectorsMock,
}))

import {
  pollWhileMounted,
  REFETCH_MS,
  ribKey,
  unwrap,
  useAsNames,
  useCollectionDumps,
  useCollectors,
  useFleetChrome,
  useCollectionFlags,
  useCollectionLocRib,
  useCollectionSessions,
  useEvents,
  useFindRoutes,
  useFleetEvents,
  useLinkState,
  useRibPage,
  useRouteHistory,
  useTopology,
  type EventScope,
  type LinkStateScope,
  type RibScope,
  type RouteFilters,
  type TopologyScope,
} from './queries'

// useQuery needs the query cache Colada installs, which is an app-level
// plugin rather than something a bare function call can reach. This mounts
// the smallest component that can hold one and hands back what the
// composable returned -- the alternative, exercising useFindRoutes only
// through a screen, would test the screen's gating rather than the
// composable's own answer.
function inColadaApp<T>(setup: () => T): T {
  let captured!: T
  mount(
    defineComponent({
      setup() {
        captured = setup()
        return () => h('div')
      },
    }),
    { global: { plugins: [createPinia(), PiniaColada] } },
  )
  return captured
}

describe('unwrap', () => {
  it('returns the payload when the call succeeded', () => {
    expect(unwrap({ data: { data: [], meta: { warnings: [], total_matched: null } } })).toEqual({
      data: [],
      meta: { warnings: [], total_matched: null },
    })
  })

  it('throws with the API error message rather than a bare status', () => {
    // The generated client returns { error } instead of rejecting. Screens
    // must not have to remember that, and an operator staring at a failed
    // table deserves the daemon's own sentence, not "Error 400".
    //
    // The input is ErrorResponse as the CONTRACT defines it --
    // { error: { code, message } }, see generated/types.gen.ts and
    // api/types.go -- not { error: { error: string } }, which this test
    // asserted for six screens and which the contract has never defined.
    // A test that builds its input from the code's assumption instead of
    // the server's shape cannot fail, and this one demonstrably did not:
    // unwrap read one level down, handed new Error() the body OBJECT, and
    // every screen rendered "[object Object]".
    expect(() =>
      unwrap({ error: { error: { code: 'invalid_param', message: 'peer is required' } } }),
    ).toThrow(/peer is required/)
  })

  it('never renders an error body as "[object Object]"', () => {
    // The regression assertion for the line above. A message read one level
    // too shallow stringifies the ErrorBody object, and /peer is required/
    // alone would not catch a reinstated shallow read if the thrown text
    // happened to contain the phrase some other way. Nothing the daemon
    // sends contains "[object", so its presence means an object reached
    // new Error().
    let thrown = ''
    try {
      unwrap({ error: { error: { code: 'invalid_param', message: 'peer is required' } } })
    } catch (e) {
      thrown = String((e as Error).message)
    }
    expect(thrown).not.toContain('[object')
    expect(thrown).toBe('peer is required')
  })

  it('throws even when the error body is not the documented shape', () => {
    // A proxy in front of the daemon can return HTML. Falling through to
    // "undefined" data would render an empty table, which reads as "no
    // routes" -- the exact confusion this project audits for.
    expect(() => unwrap({ error: 'gateway timeout' } as never)).toThrow()
  })
})

describe('ribKey', () => {
  it('includes the session so a session change is a different query', () => {
    // meta.next_cursor is pinned to a BMP session. Reusing a key across a
    // session change would splice rows from two different views into one
    // table and call the result a page.
    const a = ribKey('unicast', { router: '10.0.0.80', peer: '172.31.0.90', session: 's1' })
    const b = ribKey('unicast', { router: '10.0.0.80', peer: '172.31.0.90', session: 's2' })
    expect(a).not.toEqual(b)
  })
})

describe('useRibPage session guard', () => {
  it('drops accumulated rows when the session changes under a walk', async () => {
    // SYNTHESIZED, not captured: rib-unicast.json's next_cursor is null
    // throughout (see fixtures/README.md -- the lab is too small to
    // paginate), so no captured response can drive a second page. This
    // stubs the two pages a real cursor walk would see instead.
    //
    // Set at the start of this test, not at module scope: this file's mock
    // state must not leak between tests, so every test that needs one
    // assigns it fresh here rather than trusting what an earlier test left.
    ribUnicastMock.mockReset()
    ribUnicastMock
      .mockResolvedValueOnce({
        data: {
          data: [{ prefix: '10.10.1.0/24' }],
          meta: { warnings: [], total_matched: null, next_cursor: 'c1' },
        },
      })
      .mockResolvedValueOnce({
        data: {
          data: [{ prefix: '10.10.9.0/24' }],
          meta: { warnings: [], total_matched: null, next_cursor: null },
        },
      })

    const scope = ref<RibScope | undefined>({ router: 'r', peer: 'p', session: 's1' })
    const walk = useRibPage('unicast', scope)

    await walk.reload()
    expect(walk.rows.value).toEqual([{ prefix: '10.10.1.0/24' }])
    expect(walk.restarted.value).toBe(false)

    // The router reconnected mid-walk: same scope, new session. A cursor
    // from s1 is meaningless once the daemon has moved on to s2.
    scope.value = { router: 'r', peer: 'p', session: 's2' }
    await walk.loadMore()

    expect(walk.restarted.value).toBe(true)
    // Only s2's page should remain -- s1's row must not still be there.
    expect(walk.rows.value).toEqual([{ prefix: '10.10.9.0/24' }])
  })

  it('does not call it a restart when reload() targets a different peer', async () => {
    // A screen keeps one useRibPage instance for its whole lifetime and
    // calls reload() again every time a scope picker hands it a new peer --
    // RoutesView does exactly this. Comparing session alone would flag
    // that ordinary re-scope as a restart too, since a different peer's
    // session_id is, definitionally, a different string. A banner claiming
    // the walk restarted, about a walk that never started, would be its
    // own confusion to audit for.
    ribUnicastMock.mockReset()
    ribUnicastMock
      .mockResolvedValueOnce({
        data: {
          data: [{ prefix: '10.10.1.0/24' }],
          meta: { warnings: [], total_matched: null, next_cursor: null },
        },
      })
      .mockResolvedValueOnce({
        data: {
          data: [{ prefix: '10.20.1.0/24' }],
          meta: { warnings: [], total_matched: null, next_cursor: null },
        },
      })

    const scope = ref<RibScope | undefined>({ router: 'r1', peer: 'p1', session: 's1' })
    const walk = useRibPage('unicast', scope)
    await walk.reload()
    expect(walk.rows.value).toEqual([{ prefix: '10.10.1.0/24' }])

    // A different router AND a different peer, the way picking a fresh row
    // in ScopePicker would -- not a session churning under the same peer.
    scope.value = { router: 'r2', peer: 'p2', session: 's9' }
    await walk.reload()

    expect(walk.restarted.value).toBe(false)
    expect(walk.rows.value).toEqual([{ prefix: '10.20.1.0/24' }])
  })

  it("clears meta along with rows on a scope change, so a stale footer cannot describe a peer no longer on screen", async () => {
    // Found while building RoutesView: fetchPage clears `rows` on
    // a scope change but left `meta` holding the PRIOR scope's object until
    // the new fetch resolved. ResultMeta reads `meta` directly and has no
    // way to know it describes a different (router, peer) than the rows
    // sitting beside it -- a footer saying "complete as of ..." about
    // scope A, rendered under scope B's now-empty table, is exactly the
    // "partial answer reads as complete" failure this project exists to
    // prevent.
    ribUnicastMock.mockReset()
    ribUnicastMock
      .mockResolvedValueOnce({
        data: {
          data: [{ prefix: '10.10.1.0/24' }],
          meta: {
            warnings: [{ code: 'session_dumping', message: 'x' }],
            total_matched: null,
            next_cursor: null,
          },
        },
      })
      .mockResolvedValueOnce({
        data: {
          data: [{ prefix: '10.20.1.0/24' }],
          meta: { warnings: [], total_matched: null, next_cursor: null },
        },
      })

    const scope = ref<RibScope | undefined>({ router: 'r1', peer: 'p1', session: 's1' })
    const walk = useRibPage('unicast', scope)
    await walk.reload()
    expect(walk.meta.value?.warnings).toHaveLength(1)

    // A different peer, the way ScopePicker handing RoutesView a fresh
    // choice does -- checked in the synchronous window before the new
    // fetch resolves, which is exactly where the stale meta used to live.
    scope.value = { router: 'r2', peer: 'p2', session: 's2' }
    const pending = walk.reload()
    expect(walk.rows.value).toEqual([])
    expect(walk.meta.value).toBeUndefined()
    await pending
    expect(walk.meta.value?.warnings).toHaveLength(0)
  })
})

describe('useRibPage request ordering', () => {
  it('lets the last REQUEST win, not the last response', async () => {
    // Reproduced live: pick peer A, whose walk is slow, then
    // peer B, whose walk is fast. B renders; then A's response lands and
    // overwrites B's rows AND B's meta -- including A's session_dumping
    // warning -- while the scope picker still reads B. A footer describing
    // one peer over another peer's rows is the failure fixed on 2026-09-06
    // for the synchronous case (a scope change now clears meta along with
    // rows); this is the same failure arriving late.
    //
    // SYNTHESIZED ORDERING over captured payloads: both responses below are
    // real fixture rows and real fixture metas (rib-unicast.json carries
    // session_dumping; routers.json is this directory's captured
    // empty-warnings meta). Only the interleaving is constructed, because a
    // captured response cannot arrive out of order by itself -- capturing
    // this would mean a lab where one peer's RIB walk is reliably slower
    // than another's.
    ribUnicastMock.mockReset()
    let landA!: (v: unknown) => void
    ribUnicastMock
      .mockReturnValueOnce(
        new Promise((resolve) => {
          landA = resolve
        }),
      )
      .mockResolvedValueOnce({
        data: { data: [rib.data[1]], meta: { ...routers.meta, next_cursor: null } },
      })

    const scope = ref<RibScope | undefined>({ router: 'rA', peer: 'pA', session: 'sA' })
    const walk = useRibPage('unicast', scope)
    const slow = walk.reload()

    scope.value = { router: 'rB', peer: 'pB', session: 'sB' }
    await walk.reload()
    expect(walk.rows.value).toEqual([rib.data[1]])

    landA({ data: { data: [rib.data[0]], meta: { ...rib.meta, next_cursor: null } } })
    await slow

    // B's answer, still. A's rows must not reappear, and A's warnings must
    // not describe B's table.
    expect(walk.rows.value).toEqual([rib.data[1]])
    expect(walk.meta.value?.warnings).toHaveLength(0)
    expect(walk.loading.value).toBe(false)
  })

  it('does not clear loading when a superseded request finishes first', async () => {
    // The same guard on the other ref. Without it, A's `finally` sets
    // loading false while B is still in flight, so DataTable stops saying
    // "loading…" over a table that has nothing in it yet.
    ribUnicastMock.mockReset()
    let landA!: (v: unknown) => void
    ribUnicastMock
      .mockReturnValueOnce(
        new Promise((resolve) => {
          landA = resolve
        }),
      )
      .mockReturnValueOnce(new Promise(() => {}))

    const scope = ref<RibScope | undefined>({ router: 'rA', peer: 'pA', session: 'sA' })
    const walk = useRibPage('unicast', scope)
    const slow = walk.reload()
    scope.value = { router: 'rB', peer: 'pB', session: 'sB' }
    void walk.reload()
    expect(walk.loading.value).toBe(true)

    landA({ data: { data: [rib.data[0]], meta: { ...rib.meta, next_cursor: null } } })
    await slow

    // B has not answered yet, so the screen is still waiting.
    expect(walk.loading.value).toBe(true)
  })

  it('does not report a superseded request\'s failure against the current scope', async () => {
    // An error from the walk nobody is watching would render as this
    // peer's error, which is the same misattribution the rows and meta
    // guards prevent -- and worse, because DataTable renders an error
    // INSTEAD of the table.
    ribUnicastMock.mockReset()
    let failA!: (e: unknown) => void
    ribUnicastMock
      .mockReturnValueOnce(
        new Promise((_, reject) => {
          failA = reject
        }),
      )
      .mockResolvedValueOnce({
        data: { data: [rib.data[1]], meta: { ...routers.meta, next_cursor: null } },
      })

    const scope = ref<RibScope | undefined>({ router: 'rA', peer: 'pA', session: 'sA' })
    const walk = useRibPage('unicast', scope)
    const slow = walk.reload()
    scope.value = { router: 'rB', peer: 'pB', session: 'sB' }
    await walk.reload()

    failA(new Error('peer A timed out'))
    await slow

    expect(walk.error.value).toBeUndefined()
    expect(walk.rows.value).toEqual([rib.data[1]])
  })
})

describe('pollWhileMounted', () => {
  it('does not poll into a fetch that is still in flight', () => {
    // Colada's own `fetch` action (node_modules/@pinia/colada/dist/index.mjs,
    // around lines 389-424) unconditionally aborts any pending call and
    // starts a fresh one -- there is no framework-level de-dupe underneath
    // this. Ticking into a request slower than REFETCH_MS would abort it,
    // restart it, and abort that one too, forever: the request would never
    // complete, and the abort is swallowed rather than surfaced, so the
    // screen would sit on stale data with no sign anything is wrong.
    vi.useFakeTimers()

    const isLoading = ref(false)
    // The fake fetch that never resolves: calling refetch() flips loading
    // on and leaves it there, the same as a real request still in flight
    // when the next tick arrives.
    const refetch = vi.fn(() => {
      isLoading.value = true
    })

    pollWhileMounted(isLoading, refetch)

    vi.advanceTimersByTime(REFETCH_MS)
    expect(refetch).toHaveBeenCalledTimes(1)

    // The first call never "resolved" (isLoading is still true), so this
    // second tick must be skipped rather than aborting it and trying again.
    vi.advanceTimersByTime(REFETCH_MS)
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('stops when the scope that started it goes away', async () => {
    // The onScopeDispose teardown was pinned by nothing: deleting it left
    // the suite green while every navigation away from a polling screen
    // leaked a 30s timer that keeps refetching, forever, for a component
    // nobody is looking at. An effect scope is what a mounted component
    // gives a composable, so stopping one is what unmounting does here.
    vi.useFakeTimers()
    const isLoading = ref(false)
    const refetch = vi.fn()
    const scope = effectScope()
    scope.run(() => pollWhileMounted(isLoading, refetch))

    vi.advanceTimersByTime(REFETCH_MS)
    expect(refetch).toHaveBeenCalledTimes(1)

    scope.stop()
    vi.advanceTimersByTime(REFETCH_MS * 5)
    expect(refetch).toHaveBeenCalledTimes(1)
  })
})

describe('useFindRoutes', () => {
  it('answers undefined before any filters exist, rather than manufacturing a Meta', async () => {
    // This used to return `{data: {unicast: [], vpn: [], evpn: []}, meta:
    // {warnings: [], total_matched: null}}` -- a response no server sent,
    // whose empty warnings array is by this API's own contract a positive
    // claim that the answer is COMPLETE. The Looking glass rendered it on
    // first paint. A screen cannot render a completeness claim that was
    // never made if the composable never invents one.
    findRoutesMock.mockReset()
    const filters = ref<RouteFilters | undefined>(undefined)
    const query = inColadaApp(() => useFindRoutes(filters))
    await flushPromises()
    expect(query.data.value).toBeUndefined()
    // And no request went out: /v1/routes with no narrowing parameter is a
    // far larger question than the one nobody asked.
    expect(findRoutesMock).not.toHaveBeenCalled()
  })

  it('sends the filters and returns the payload once a search exists', async () => {
    // The converse, so "always undefined" cannot pass the test above.
    findRoutesMock.mockReset()
    findRoutesMock.mockResolvedValue({
      data: { data: { unicast: [], vpn: [], evpn: [] }, meta: { warnings: [], total_matched: 0 } },
    })
    const filters = ref<RouteFilters | undefined>({ prefix: '10.97.0.0/16' })
    const query = inColadaApp(() => useFindRoutes(filters))
    await flushPromises()
    expect(findRoutesMock).toHaveBeenCalledWith({ query: { prefix: '10.97.0.0/16' } })
    expect(query.data.value?.meta.warnings).toEqual([])
  })
})

describe('useRouteHistory', () => {
  it('sends the window the caller named rather than letting the daemon pick one', async () => {
    // /v1/routes/history bounds every answer at ?since=, defaulting to 1h
    // (api/openapi.yaml; api/handlers.go's since() implements it). A
    // request that omits it still comes back bounded -- by a number no
    // screen can name, which is a partial answer reading as a complete one
    // a layer above where Meta.warnings guards. Sending it means the value
    // on screen and the value in the request are the same value.
    routeHistoryMock.mockReset()
    routeHistoryMock.mockResolvedValue({
      data: { data: [], meta: { warnings: [], total_matched: null } },
    })
    const query = inColadaApp(() =>
      useRouteHistory(ref<string | undefined>('10.10.1.0/24'), ref('24h')),
    )
    await flushPromises()
    expect(routeHistoryMock).toHaveBeenCalledWith({
      query: { prefix: '10.10.1.0/24', since: '24h' },
    })
    expect(query.data.value?.data).toEqual([])
  })

  it('asks nothing, and answers nothing, until a prefix exists', async () => {
    // Same rule useFindRoutes follows: an empty event list next to a
    // manufactured "complete" meta is a claim about a request never sent.
    routeHistoryMock.mockReset()
    const query = inColadaApp(() => useRouteHistory(ref<string | undefined>(undefined), ref('1h')))
    await flushPromises()
    expect(routeHistoryMock).not.toHaveBeenCalled()
    expect(query.data.value).toBeUndefined()
  })
})

describe('useEvents', () => {
  it('does not call findPeerEvents until a scope exists', async () => {
    // /v1/events requires router and peer: a call without them is a 400,
    // not an empty list, so the composable must not fetch before a scope
    // exists. Pinned against the mocked client operation itself -- an
    // assertion on a `vi.fn()` wired to nothing would pass no matter what
    // the code did.
    findPeerEventsMock.mockReset()
    const scope = ref<EventScope | undefined>(undefined)
    const walk = useEvents(scope, ref('24h'))
    await walk.reload()
    expect(findPeerEventsMock).not.toHaveBeenCalled()
    expect(walk.rows.value).toEqual([])
  })

  it('sends the window the caller named rather than letting the daemon pick one', async () => {
    // The same rule useRouteHistory's own version of this test states, and
    // the same 1h default behind it (api/openapi.yaml; api/handlers.go's
    // since()): a request that omits since= still comes back bounded, by a
    // number no screen can name. On this screen that is worse than an
    // unbounded answer would be -- SessionHistoryView prints the chosen
    // window and peer_events' 90-day retention in a note beside the table,
    // so an operator who picks "last 90 days" and is served one hour reads a
    // paragraph explaining a bound the rows do not have. That is this
    // project's governing rule violated directly: a partial answer reading as a
    // complete one.
    //
    // SessionHistoryView.test.ts's own "reloads the walk when the window
    // changes" cannot see this. It asserts on capturedSince, the ref handed
    // to a MOCKED useEvents, which stays correct however the request is
    // built; only the outgoing query says whether the value ever left.
    findPeerEventsMock.mockReset()
    findPeerEventsMock.mockResolvedValue({
      data: { data: [], meta: { warnings: [], total_matched: null, next_cursor: null } },
    })
    const since = ref('2160h')
    const scope = ref<EventScope | undefined>({ router: 'r', peer: 'p', session: 's' })
    const walk = useEvents(scope, since)
    await walk.reload()
    expect(findPeerEventsMock).toHaveBeenCalledWith({
      query: { router: 'r', peer: 'p', since: '2160h', cursor: undefined, limit: 500 },
    })

    // Read per request, not captured once: the screen changes this ref and
    // then calls reload() (useEvents watches neither argument, by design),
    // so a since read at construction time would leave every later request
    // asking the first window over again.
    since.value = '1h'
    await walk.reload()
    expect(findPeerEventsMock).toHaveBeenLastCalledWith({
      query: { router: 'r', peer: 'p', since: '1h', cursor: undefined, limit: 500 },
    })
  })

  it('clears rows and meta together when the scope changes', async () => {
    // The defect useRibPage shipped and had to fix: meta outliving the rows
    // it described leaves a footer describing the previous scope's answer.
    //
    // The first reload() below is load-bearing, not decoration: without it,
    // `rows` and `meta` still hold their untouched initial values ([] and
    // undefined) at the second reload(), and the assertions below would
    // pass even if the scope-change clear were deleted entirely. Populating
    // them for real first is what lets this test fail for the right reason
    // -- confirmed in the mutation check.
    findPeerEventsMock.mockReset()
    findPeerEventsMock.mockResolvedValueOnce({
      data: {
        data: [{ session_id: 'b1' }],
        meta: { warnings: [{ code: 'x', message: 'x' }], total_matched: null, next_cursor: null },
      },
    })
    const scope = ref<EventScope | undefined>({ router: 'a', peer: 'b', session: '1' })
    const walk = useEvents(scope, ref('24h'))
    await walk.reload()
    expect(walk.rows.value).toHaveLength(1)
    expect(walk.meta.value?.warnings).toHaveLength(1)

    scope.value = { router: 'a', peer: 'c', session: '2' }
    const pending = walk.reload()
    expect(walk.rows.value).toEqual([])
    expect(walk.meta.value).toBeUndefined()
    await pending
  })
})

describe('useEvents request ordering', () => {
  it('lets the last REQUEST win, not the last response', async () => {
    // Ported from useRibPage's own "request ordering" suite above: the
    // same defect (an abandoned scope's slow response landing after a
    // fast re-pick's has already rendered, and silently overwriting its
    // rows and meta) is possible here too -- useEvents's own doc comment
    // calls the guard mechanism "identical, only the scope shape differs".
    //
    // Peer A's response is a promise resolved BY HAND (`landA`), not
    // mockResolvedValueOnce -- an immediately-resolved mock cannot model
    // "the old scope's answer arrives after the new scope already
    // rendered", which is the entire situation under test. Peer B's mock
    // resolves right away, which is what lets it render BEFORE A's
    // deferred promise is settled below.
    findPeerEventsMock.mockReset()
    let landA!: (v: unknown) => void
    findPeerEventsMock
      .mockReturnValueOnce(
        new Promise((resolve) => {
          landA = resolve
        }),
      )
      .mockResolvedValueOnce({
        data: {
          data: [{ session_id: 'b1' }],
          meta: { warnings: [], total_matched: null, next_cursor: null },
        },
      })

    const scope = ref<EventScope | undefined>({ router: 'rA', peer: 'pA', session: 'sA' })
    const walk = useEvents(scope, ref('24h'))
    const slow = walk.reload()

    scope.value = { router: 'rB', peer: 'pB', session: 'sB' }
    await walk.reload()
    expect(walk.rows.value).toEqual([{ session_id: 'b1' }])

    landA({
      data: {
        data: [{ session_id: 'a1' }],
        meta: { warnings: [{ code: 'x', message: 'x' }], total_matched: null, next_cursor: null },
      },
    })
    await slow

    // B's answer, still. A's rows must not reappear, and A's warnings must
    // not describe B's table.
    expect(walk.rows.value).toEqual([{ session_id: 'b1' }])
    expect(walk.meta.value?.warnings).toHaveLength(0)
    expect(walk.loading.value).toBe(false)
  })

  it('does not clear loading when a superseded request finishes first', async () => {
    // The same guard on the other ref. Without it, A's `finally` sets
    // loading false while B is still in flight, so DataTable stops saying
    // "loading…" over a table that has nothing in it yet.
    findPeerEventsMock.mockReset()
    let landA!: (v: unknown) => void
    findPeerEventsMock
      .mockReturnValueOnce(
        new Promise((resolve) => {
          landA = resolve
        }),
      )
      .mockReturnValueOnce(new Promise(() => {}))

    const scope = ref<EventScope | undefined>({ router: 'rA', peer: 'pA', session: 'sA' })
    const walk = useEvents(scope, ref('24h'))
    const slow = walk.reload()
    scope.value = { router: 'rB', peer: 'pB', session: 'sB' }
    void walk.reload()
    expect(walk.loading.value).toBe(true)

    landA({
      data: {
        data: [{ session_id: 'a1' }],
        meta: { warnings: [], total_matched: null, next_cursor: null },
      },
    })
    await slow

    // B has not answered yet, so the screen is still waiting.
    expect(walk.loading.value).toBe(true)
  })

  it("does not report a superseded request's failure against the current scope", async () => {
    // An error from the walk nobody is watching would render as this
    // peer's error, which is the same misattribution the rows and meta
    // guards prevent -- and worse, because DataTable renders an error
    // INSTEAD of the table.
    findPeerEventsMock.mockReset()
    let failA!: (e: unknown) => void
    findPeerEventsMock
      .mockReturnValueOnce(
        new Promise((_, reject) => {
          failA = reject
        }),
      )
      .mockResolvedValueOnce({
        data: {
          data: [{ session_id: 'b1' }],
          meta: { warnings: [], total_matched: null, next_cursor: null },
        },
      })

    const scope = ref<EventScope | undefined>({ router: 'rA', peer: 'pA', session: 'sA' })
    const walk = useEvents(scope, ref('24h'))
    const slow = walk.reload()
    scope.value = { router: 'rB', peer: 'pB', session: 'sB' }
    await walk.reload()

    failA(new Error('peer A timed out'))
    await slow

    expect(walk.error.value).toBeUndefined()
    expect(walk.rows.value).toEqual([{ session_id: 'b1' }])
  })
})

describe('useFleetEvents', () => {
  // The window is what bounds an unscoped read, so it must be SENT, not
  // assumed. /v1/events defaults an absent since= to 1h server-side; a
  // request that omitted it would still come back bounded, by a number the
  // operator never chose and no screen could name -- the same "partial
  // answer reads as complete" failure useRouteHistory's own doc comment
  // describes.
  it('sends the window the screen names, never relying on the server default', async () => {
    findPeerEventsMock.mockReset()
    findPeerEventsMock.mockResolvedValue({
      data: { data: [], meta: { warnings: [], total_matched: null } },
    })
    const since = ref('6h')
    const query = inColadaApp(() => useFleetEvents(since))
    await flushPromises()
    expect(findPeerEventsMock).toHaveBeenCalledWith(
      expect.objectContaining({ query: expect.objectContaining({ since: '6h' }) }),
    )
    expect(query.data.value?.data).toEqual([])
  })

  // Naming either half of a scope would turn the fleet question into a
  // different one -- or, for exactly one half, into a 400.
  it('names neither router nor peer, because that is what makes it a fleet question', async () => {
    findPeerEventsMock.mockReset()
    findPeerEventsMock.mockResolvedValue({
      data: { data: [], meta: { warnings: [], total_matched: null } },
    })
    inColadaApp(() => useFleetEvents(ref('1h')))
    await flushPromises()
    const q = findPeerEventsMock.mock.calls[0][0].query
    expect(q).not.toHaveProperty('router')
    expect(q).not.toHaveProperty('peer')
  })

  // A cursor on an unscoped request is a 400 (errEventsCursorNeedsScope), and
  // the mode issues none anyway.
  it('never sends a cursor', async () => {
    findPeerEventsMock.mockReset()
    findPeerEventsMock.mockResolvedValue({
      data: { data: [], meta: { warnings: [], total_matched: null } },
    })
    inColadaApp(() => useFleetEvents(ref('1h')))
    await flushPromises()
    expect(findPeerEventsMock.mock.calls[0][0].query).not.toHaveProperty('cursor')
  })

  // The window is part of the identity of the answer. A key that ignored it
  // would serve a cached 1h answer to a screen displaying "last 6 hours".
  it('keys the query on the window, so a wider one is a different answer', async () => {
    findPeerEventsMock.mockReset()
    findPeerEventsMock.mockResolvedValue({
      data: { data: [], meta: { warnings: [], total_matched: null } },
    })
    const since = ref('1h')
    inColadaApp(() => useFleetEvents(since))
    await flushPromises()
    const before = findPeerEventsMock.mock.calls.length
    since.value = '6h'
    await flushPromises()
    expect(findPeerEventsMock.mock.calls.length).toBeGreaterThan(before)
  })
})

describe('useTopology', () => {
  // /v1/topology refuses a request that scopes nothing: its own 400 says the
  // alternative "would draw every route in the archive as one graph"
  // (api/topology.go's requireTopologyScope). A composable that fired on
  // mount would send that refused request on every visit to the screen, and
  // -- worse -- an empty TopologyFanout rendered underneath it would read as
  // "this scope reaches no AS anywhere", which is the confusion this whole
  // project exists to prevent. useFindRoutes answers undefined for the same
  // reason on the same shape of question.
  it('asks nothing until a scope exists, rather than sending a request the API refuses', async () => {
    findTopologyMock.mockReset()
    const scope = ref<TopologyScope | undefined>(undefined)
    const query = inColadaApp(() => useTopology(scope))
    await flushPromises()
    expect(query.data.value).toBeUndefined()
    expect(findTopologyMock).not.toHaveBeenCalled()
  })

  // The scope has to reach the wire as query parameters, and router= with
  // peer= is the mode worth pinning: it is this endpoint's one declared
  // departure from /v1/routes, where the same pair alone is a 400. A
  // composable that dropped either half would turn a sufficient scope into
  // half a scope, and the daemon would refuse it.
  //
  // The payload is the CAPTURED fixture rather than a hand-written one,
  // because the assertion below is about the RESPONSE: all three family keys
  // present, and the two families this lab cannot produce arriving as
  // { nodes: [], edges: [] } rather than null. That is the contract's own
  // "an empty one is the positive claim 'nothing there', never 'we did not
  // look'", and only a real capture demonstrates it.
  it('sends router and peer together, the scope /v1/routes alone would refuse', async () => {
    findTopologyMock.mockReset()
    findTopologyMock.mockResolvedValue({ data: topology })
    const scope = ref<TopologyScope | undefined>({
      router: '172.22.0.7',
      peer: '172.31.0.90',
    })
    const query = inColadaApp(() => useTopology(scope))
    await flushPromises()
    expect(findTopologyMock).toHaveBeenCalledWith({
      query: { router: '172.22.0.7', peer: '172.31.0.90' },
    })
    expect(query.data.value?.data.unicast.edges).toHaveLength(topology.data.unicast.edges.length)
    expect(query.data.value?.data.vpn).toEqual({ nodes: [], edges: [] })
    expect(query.data.value?.data.evpn).toEqual({ nodes: [], edges: [] })
    expect(query.data.value?.meta.total_matched).toBe(topology.meta.total_matched)
  })

  // A cached answer served under a different question is a wrong graph shown
  // confidently. useFleetEvents keys on its window for the same reason.
  it('keys the answer on the scope, so a new question is a new answer', async () => {
    findTopologyMock.mockReset()
    findTopologyMock.mockResolvedValue({ data: topology })
    const scope = ref<TopologyScope | undefined>({ prefix: '10.10.1.0/24' })
    inColadaApp(() => useTopology(scope))
    await flushPromises()
    const before = findTopologyMock.mock.calls.length
    scope.value = { prefix: '10.10.3.0/24' }
    await flushPromises()
    expect(findTopologyMock.mock.calls.length).toBeGreaterThan(before)
    expect(findTopologyMock.mock.calls.at(-1)?.[0]).toEqual({
      query: { prefix: '10.10.3.0/24' },
    })
  })

  // The REAL ErrorResponse shape, { error: { code, message } } -- see
  // generated/types.gen.ts and api/types.go, and unwrap's own doc comment on
  // the six screens that rendered "[object Object]" because a test built its
  // input from the code's assumption instead of the server's shape. This
  // endpoint's refusal sentence is the longest and most useful in the API:
  // it names what would make the request answerable. Losing it to a
  // stringified object would leave an operator with a blank graph pane and
  // nothing to act on.
  it('surfaces the daemon own refusal sentence, never a stringified object', async () => {
    findTopologyMock.mockReset()
    const message =
      '/v1/topology needs something to scope the graph to: one of prefix, covers, ' +
      'origin_asn, through_asn or community names the routes, and router= together ' +
      'with peer= names the vantage point'
    findTopologyMock.mockResolvedValue({
      error: { error: { code: 'invalid_param', message } },
    })
    const scope = ref<TopologyScope | undefined>({ router: '172.22.0.7' })
    const query = inColadaApp(() => useTopology(scope))
    await flushPromises()
    expect(query.error.value?.message).toBe(message)
    expect(query.error.value?.message).not.toContain('[object')
    expect(query.data.value).toBeUndefined()
  })
})

describe('useAsNames', () => {
  // /v1/asnames refuses a request naming no `asn=` at all with a 400 whose
  // own sentence says the alternative would be a dump of the whole 122,442-
  // row dataset. A composable that fired with an empty batch would send
  // that refused request every time a screen mounts before its own rows
  // have landed.
  it('asks nothing until there is at least one ASN to name', async () => {
    listAsNamesMock.mockReset()
    const asns = ref<number[]>([])
    const query = inColadaApp(() => useAsNames(asns))
    await flushPromises()
    expect(query.data.value).toBeUndefined()
    expect(listAsNamesMock).not.toHaveBeenCalled()
  })

  // The batch this composable sends is the DISTINCT, sorted set, not
  // whatever order or repeats the caller's own rows happen to carry --
  // repeats cost nothing on the wire, but they do cost against
  // /v1/asnames' own 512 cap (api/asnames.go counts raw occurrences, not
  // distinct values), so paying that cost here rather than in each of the
  // three screens is the whole point of this composable.
  it('sends the distinct ASNs, deduped and sorted', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [{ asn: 3356, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    const asns = ref<number[]>([15169, 3356, 15169, 3356])
    const query = inColadaApp(() => useAsNames(asns))
    await flushPromises()
    expect(listAsNamesMock).toHaveBeenCalledWith({ query: { asn: [3356, 15169] } })
    expect(query.data.value?.data[0].name).toBe('LEVEL3 - Level 3 Parent, LLC')
    expect(query.data.value?.meta.asnames_loaded).toBe(true)
  })

  // The defensive half of the cap: this composable must not let a wide
  // screen's own batch grow past what the daemon accepts and turn an
  // enrichment lookup into a 400 that blanks every name on the page.
  it('caps the batch at 512 rather than sending one the daemon rejects outright', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    const many = Array.from({ length: 600 }, (_, i) => i + 1)
    const asns = ref<number[]>(many)
    inColadaApp(() => useAsNames(asns))
    await flushPromises()
    const sent = listAsNamesMock.mock.calls.at(-1)?.[0].query.asn as number[]
    expect(sent).toHaveLength(512)
  })

  // WHICH 512, not just how many. The count alone passes for any cutoff
  // rule, and the rule is the whole reason the cap is dangerous rather
  // than merely lossy: the batch is sorted ascending and sliced, so the
  // survivors are the numerically LOWEST -- which makes the ASNs that go
  // unnamed a stable, predictable set (4-byte ASNs, the modern ones)
  // rather than a random sample. A screen full of unnamed high ASNs looks
  // like a screen full of unlisted ones, every time, reproducibly.
  //
  // Pinned here so that changing the rule is a deliberate edit to a test
  // that states it, not a silent change in which operators get names.
  it('keeps the numerically lowest ASNs when it caps, not an arbitrary 512', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    // Shuffled input, so a composable that merely preserved caller order
    // could not pass this by accident.
    const many = Array.from({ length: 600 }, (_, i) => i + 1)
    const shuffled = [...many].reverse()
    const asns = ref<number[]>(shuffled)
    inColadaApp(() => useAsNames(asns))
    await flushPromises()
    const sent = listAsNamesMock.mock.calls.at(-1)?.[0].query.asn as number[]
    expect(sent).toEqual(Array.from({ length: 512 }, (_, i) => i + 1))
    expect(sent.at(-1)).toBe(512)
    expect(sent).not.toContain(513)
    expect(sent).not.toContain(600)
  })

  // The cap's honest half. Capping keeps the lookup from becoming a 400
  // that blanks the page, but it silently reintroduces the exact collapse
  // @/lib/asname exists to prevent: past the cutoff a high ASN renders
  // bare, identical to one the dataset genuinely does not list. A screen
  // cannot say so unless the composable tells it, and this is the signal.
  it('reports that it capped, so a screen can say the rest were never looked up', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    const asns = ref<number[]>(Array.from({ length: 513 }, (_, i) => i + 1))
    const query = inColadaApp(() => useAsNames(asns))
    await flushPromises()
    expect(query.truncated.value).toBe(true)
  })

  // Exactly at the cap nothing was dropped, so nothing must be claimed --
  // an off-by-one here would put a "some ASNs were not looked up" line on
  // a screen where every one of them was.
  it('reports no truncation at exactly the cap, or one under it', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    const asns = ref<number[]>(Array.from({ length: 512 }, (_, i) => i + 1))
    const query = inColadaApp(() => useAsNames(asns))
    await flushPromises()
    expect(query.truncated.value).toBe(false)
    asns.value = Array.from({ length: 511 }, (_, i) => i + 1)
    await flushPromises()
    expect(query.truncated.value).toBe(false)
  })

  // DISTINCT ASNs, not raw rows. RoutesView hands this one entry per row
  // and a walk repeats origins heavily, so counting rows would put the
  // truncation line on a screen whose distinct set fits comfortably.
  it('counts distinct ASNs when deciding it capped, not the caller\'s row count', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    // 2,000 rows, 10 distinct origins -- the ordinary RIB walk.
    const asns = ref<number[]>(Array.from({ length: 2000 }, (_, i) => (i % 10) + 1))
    const query = inColadaApp(() => useAsNames(asns))
    await flushPromises()
    expect(query.truncated.value).toBe(false)
  })

  // And it must become true as a walk accumulates, because that is exactly
  // how RoutesView reaches the cap: useRibPage appends each page's rows to
  // the same array, so the batch grows with every "more" click rather than
  // being re-derived from one page.
  it('starts reporting truncation once an accumulating walk grows past the cap', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    const asns = ref<number[]>(Array.from({ length: 500 }, (_, i) => i + 1))
    const query = inColadaApp(() => useAsNames(asns))
    await flushPromises()
    expect(query.truncated.value).toBe(false)
    // One more page of 500 distinct origins -- the second click.
    asns.value = [...asns.value, ...Array.from({ length: 500 }, (_, i) => i + 501)]
    await flushPromises()
    expect(query.truncated.value).toBe(true)
  })

  // A cached answer served under a different batch is a wrong set of names
  // shown confidently, the identical reasoning useTopology's own "keys the
  // answer on the scope" test states for its question.
  it('keys the answer on the batch, so a new set of ASNs is a new answer', async () => {
    listAsNamesMock.mockReset()
    listAsNamesMock.mockResolvedValue({
      data: {
        data: [],
        meta: { warnings: [], total_matched: null, asnames_loaded: true } satisfies Meta,
      },
    })
    const asns = ref<number[]>([3356])
    inColadaApp(() => useAsNames(asns))
    await flushPromises()
    const before = listAsNamesMock.mock.calls.length
    asns.value = [15169]
    await flushPromises()
    expect(listAsNamesMock.mock.calls.length).toBeGreaterThan(before)
    expect(listAsNamesMock.mock.calls.at(-1)?.[0]).toEqual({ query: { asn: [15169] } })
  })
})

describe('useLinkState', () => {
  const emptyLs = { data: { data: [], meta: { warnings: [], total_matched: 0 } } }

  // No scope combination is refused server-side, not even an empty one
  // -- the client is the only gate, and
  // it has to hold BEFORE the first request rather than after a 400 comes
  // back. useTopology's and useFindRoutes' own "nothing until a scope
  // exists" tests are the precedent this follows.
  it('asks nothing until a scope exists', async () => {
    findLsNodesMock.mockReset()
    findLsLinksMock.mockReset()
    findLsPrefixesMock.mockReset()
    const scope = ref<LinkStateScope | undefined>(undefined)
    const q = inColadaApp(() => useLinkState(scope))
    await flushPromises()
    expect(q.nodes.value).toBeUndefined()
    expect(findLsNodesMock).not.toHaveBeenCalled()
    expect(findLsLinksMock).not.toHaveBeenCalled()
    expect(findLsPrefixesMock).not.toHaveBeenCalled()
  })

  it('sends the whole scope on every one of the three requests', async () => {
    // LinkStateGraph.vue's rule 4: a graph is ONE topology. A node query scoped
    // differently from its link query would silently draw adjacencies between
    // two of them, and nothing downstream could tell.
    findLsNodesMock.mockReset()
    findLsLinksMock.mockReset()
    findLsPrefixesMock.mockReset()
    findLsNodesMock.mockResolvedValue(emptyLs)
    findLsLinksMock.mockResolvedValue(emptyLs)
    findLsPrefixesMock.mockResolvedValue(emptyLs)
    const scope = ref<LinkStateScope | undefined>({ router: '10.0.0.11', protocol: 3, area: 0 })
    inColadaApp(() => useLinkState(scope))
    await flushPromises()
    // `protocol` arrives as a string: the generated client's own `protocol`
    // parameter is typed `string` because the wire format also accepts a
    // name ("isis-l2"), and a decimal string asks the identical question --
    // the endpoint's own doc comment says so.
    const expected = { query: { router: '10.0.0.11', protocol: '3', area: 0, limit: 10000 } }
    expect(findLsNodesMock).toHaveBeenCalledWith(expected)
    expect(findLsLinksMock).toHaveBeenCalledWith(expected)
    expect(findLsPrefixesMock).toHaveBeenCalledWith(expected)
  })

  it('refreshes all three legs on one timer, so no leg is fresher than the answer', async () => {
    // A poll on the nodes query alone shows in a browser's network panel as
    // nodes fetched six times against one fetch each for links and prefixes
    // -- under a footer whose "complete as of" clock advances every 30
    // seconds for an answer two thirds of which is a minute old. The
    // screen's answer is ONE topology (LinkStateGraph.vue's rule 4);
    // refreshing a third of it and dating the whole is the defect.
    vi.useFakeTimers()
    findLsNodesMock.mockReset()
    findLsLinksMock.mockReset()
    findLsPrefixesMock.mockReset()
    findLsNodesMock.mockResolvedValue(emptyLs)
    findLsLinksMock.mockResolvedValue(emptyLs)
    findLsPrefixesMock.mockResolvedValue(emptyLs)
    const scope = ref<LinkStateScope | undefined>({ router: '10.0.0.11' })
    inColadaApp(() => useLinkState(scope))
    // Awaited through the fake clock rather than flushPromises, which is a
    // setTimeout(0) and would never resolve while the timers are faked.
    await vi.advanceTimersByTimeAsync(0)
    expect(findLsNodesMock).toHaveBeenCalledTimes(1)
    expect(findLsLinksMock).toHaveBeenCalledTimes(1)
    expect(findLsPrefixesMock).toHaveBeenCalledTimes(1)

    // One tick, three refetches -- not one refetch and two stale answers.
    await vi.advanceTimersByTimeAsync(REFETCH_MS)
    expect(findLsNodesMock).toHaveBeenCalledTimes(2)
    expect(findLsLinksMock).toHaveBeenCalledTimes(2)
    expect(findLsPrefixesMock).toHaveBeenCalledTimes(2)
    vi.useRealTimers()
  })

  it("returns each leg's own meta, because each handler appends its own warnings", async () => {
    // LinkStateGraph.vue's rule 6, per answer. handleLSLinks and
    // handleLSPrefixes build their own Meta exactly as handleLSNodes does, so a
    // links answer can be truncated while the nodes answer is complete. A
    // composable that returned the nodes meta alone let a screen print
    // ResultMeta's "complete as of" beside a canvas that was silently short of
    // adjacencies -- and merging the three would pair the links answer's
    // warning with the nodes answer's total_matched, a number true of neither.
    findLsNodesMock.mockReset()
    findLsLinksMock.mockReset()
    findLsPrefixesMock.mockReset()
    findLsNodesMock.mockResolvedValue(emptyLs)
    findLsLinksMock.mockResolvedValue({
      data: {
        data: [],
        meta: {
          warnings: [{ code: 'truncated', message: '24000 rows matched and 10000 were returned' }],
          total_matched: 24000,
        },
      },
    })
    findLsPrefixesMock.mockResolvedValue({
      data: {
        data: [],
        meta: {
          warnings: [{ code: 'session_dumping', message: 'a peer is still sending its dump' }],
          total_matched: 0,
        },
      },
    })
    const scope = ref<LinkStateScope | undefined>({ router: '10.0.0.11' })
    const q = inColadaApp(() => useLinkState(scope))
    await flushPromises()
    expect(q.nodesMeta.value?.warnings).toEqual([])
    expect(q.linksMeta.value?.warnings.map((w) => w.code)).toEqual(['truncated'])
    expect(q.linksMeta.value?.total_matched).toBe(24000)
    expect(q.prefixesMeta.value?.warnings.map((w) => w.code)).toEqual(['session_dumping'])
  })

  // The real body is { error: { code, message } } -- see
  // generated/types.gen.ts and api/types.go. Asserting a bare string would
  // pass against a shape the server never sends. useTopology's own refusal
  // test above is the existing precedent for this assertion; the failure is
  // driven the same way, by stubbing the generated SDK module rather than
  // fetch -- this file mocks './generated' wholesale rather than the
  // network layer, so findLsNodesMock (etc.) is what stands in for the
  // outbound request here.
  it("surfaces the daemon's own sentence, from the contract's error shape", async () => {
    findLsNodesMock.mockReset()
    findLsLinksMock.mockReset()
    findLsPrefixesMock.mockReset()
    findLsNodesMock.mockResolvedValue({
      error: { error: { code: 'invalid_param', message: 'router= is required' } },
    })
    findLsLinksMock.mockResolvedValue(emptyLs)
    findLsPrefixesMock.mockResolvedValue(emptyLs)
    const scope = ref<LinkStateScope | undefined>({ router: '10.0.0.11' })
    const q = inColadaApp(() => useLinkState(scope))
    await flushPromises()
    expect(q.error.value?.message).toBe('router= is required')
    expect(q.error.value?.message).not.toContain('[object')
  })

  // The union is `nodes.error ?? links.error ?? prefixes.error`, and until
  // this ran, two thirds of it could not fail a test: the refusal test above
  // drives the nodes leg alone, and LinkStateView.test.ts replaces this
  // composable wholesale. Drop `links.error` from that chain and nothing goes
  // red -- `links.value` stays undefined forever, so the screen's `drawn`
  // stays undefined and it renders `loading…` permanently over a dead daemon.
  // A failed daemon reading as a slow one is the worst of the four states
  // this screen has, because it is the one an operator waits through.
  //
  // Driven per leg rather than all three at once so each arm of the union is
  // exercised on its own: a test that failed every leg would pass with two of
  // the three arms deleted.
  for (const leg of ['links', 'prefixes'] as const) {
    it(`surfaces a refusal from the ${leg} leg, not only from nodes`, async () => {
      findLsNodesMock.mockReset()
      findLsLinksMock.mockReset()
      findLsPrefixesMock.mockReset()
      findLsNodesMock.mockResolvedValue(emptyLs)
      findLsLinksMock.mockResolvedValue(emptyLs)
      findLsPrefixesMock.mockResolvedValue(emptyLs)
      const failing = leg === 'links' ? findLsLinksMock : findLsPrefixesMock
      const message = `area= must be an integer`
      failing.mockResolvedValue({ error: { error: { code: 'invalid_param', message } } })

      const scope = ref<LinkStateScope | undefined>({ router: '10.0.0.11' })
      const q = inColadaApp(() => useLinkState(scope))
      await flushPromises()

      expect(q.error.value?.message).toBe(message)
      expect(q.error.value?.message).not.toContain('[object')
      // And the leg that failed carries no data, which is the half that makes
      // the error the ONLY thing a screen can say about this answer: a caller
      // that read `nodes` alone would draw a canvas over a failed links query.
      expect(leg === 'links' ? q.links.value : q.prefixes.value).toBeUndefined()
    })
  }
})

// The four /v1/collection/* composables are copy-paste siblings: one window
// in, the same { data, meta } out, four different endpoints. Until now the
// only thing exercising them was MonitorView.test.ts, which mocks all four
// away -- so the screen proved the window REF reached each composable (the
// gap that, until 2026-09-07, let a request drop `since` with every test
// still green -- found only by later manual testing), and nothing proved
// a composable then sends that window, reads its OWN endpoint, or
// unwraps what comes back. A sibling's
// operation pasted into the wrong composable typechecks, because all four
// take the same argument and return the same shape; the screen would then
// render one signal's answer under another's heading.
//
// The expectations are exact objects rather than objectContaining: `router`
// is deliberately not exposed by these four (useCollectionDumps' own doc
// comment says why), and an exact match is what holds that, plus the
// absence of a cursor these endpoints do not take.
describe('the four /v1/collection/* composables', () => {
  const CASES = [
    {
      label: 'dumps',
      use: useCollectionDumps,
      op: collectionDumpsMock,
      payload: collectionDumpsFixture,
    },
    {
      label: 'sessions',
      use: useCollectionSessions,
      op: collectionSessionsMock,
      payload: collectionSessionsFixture,
    },
    {
      label: 'locrib',
      use: useCollectionLocRib,
      op: collectionLocRibMock,
      payload: collectionLocribFixture,
    },
    {
      label: 'flags',
      use: useCollectionFlags,
      op: collectionFlagsMock,
      payload: collectionFlagsFixture,
    },
  ]

  // Captured fixtures, not hand-written payloads -- the same rule the rest
  // of this suite follows, so an unwrap that mangled a real response could
  // not pass here by matching a shape invented for the test.
  beforeEach(() => {
    for (const c of CASES) {
      c.op.mockReset()
      c.op.mockResolvedValue({ data: { data: c.payload.data, meta: c.payload.meta } })
    }
  })

  for (const c of CASES) {
    it(`${c.label} sends the window the screen named, never relying on the server default`, async () => {
      const query = inColadaApp(() => c.use(ref('6h')))
      await flushPromises()
      expect(c.op).toHaveBeenCalledWith({ query: { since: '6h' } })
      expect(query.data.value?.data).toEqual(c.payload.data)
      expect(query.data.value?.meta).toEqual(c.payload.meta)
    })

    it(`${c.label} reads its own endpoint and no sibling's`, async () => {
      inColadaApp(() => c.use(ref('1h')))
      await flushPromises()
      expect(c.op).toHaveBeenCalledTimes(1)
      for (const other of CASES) {
        if (other.op !== c.op) expect(other.op).not.toHaveBeenCalled()
      }
    })

    it(`${c.label} keys on the window, so a wider one is a different answer`, async () => {
      const since = ref('1h')
      inColadaApp(() => c.use(since))
      await flushPromises()
      const before = c.op.mock.calls.length
      since.value = '24h'
      await flushPromises()
      expect(c.op.mock.calls.length).toBeGreaterThan(before)
      expect(c.op).toHaveBeenLastCalledWith({ query: { since: '24h' } })
    })
  }
})

// The chrome's two fleet facts. Derived here rather than in the component
// so the rules -- count distinct collectors, and NEVER render a zero -- are
// exercised against Colada's real query object instead of a hand-written
// stand-in for one.
describe('useFleetChrome', () => {
  beforeEach(() => {
    listRoutersMock.mockReset()
    listCollectorsMock.mockReset()
    // The count tests below read routers only; a collectors request that
    // never answers keeps them to that.
    listCollectorsMock.mockReturnValue(new Promise(() => {}))
  })

  it('counts distinct collectors, not routers', async () => {
    // Three routers, two collectors: a count of routers would say 3, which
    // is the wrong fact in the one place on screen that is always visible.
    listRoutersMock.mockResolvedValue({
      data: {
        data: [
          { ...routers.data[0], collector: 'c1' },
          { ...routers.data[0], collector: 'c1' },
          { ...routers.data[0], collector: 'c2' },
        ],
        meta: { warnings: [] },
      },
    })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.collectors.value).toBe(2)
    expect(chrome.updatedAt.value).toBeTruthy()
  })

  // Absent, never 0. An empty inventory and a failed request are both "we
  // cannot say", and "0 collectors" is the claim that nothing is answering.
  it('answers undefined for an empty inventory rather than zero', async () => {
    listRoutersMock.mockResolvedValue({ data: { data: [], meta: { warnings: [] } } })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.collectors.value).toBeUndefined()
  })

  it('answers undefined when the inventory request failed', async () => {
    listRoutersMock.mockResolvedValue({ error: { error: { code: 'x', message: 'nope' } } })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.collectors.value).toBeUndefined()
  })

  it('has no arrival stamp before the first answer', () => {
    listRoutersMock.mockReturnValue(new Promise(() => {}))
    const chrome = inColadaApp(() => useFleetChrome())
    expect(chrome.updatedAt.value).toBeUndefined()
  })

  /**
   * The archive's freshness, which is a different fact from updatedAt.
   *
   * updatedAt is when this BROWSER received an answer; newestRow is the
   * newest row any collector actually wrote, on the collector's own clock.
   * They are hours apart on a quiet fleet -- this lab archives about 21
   * rows a day -- and the chrome showed only the first for four days,
   * where a reader takes it for the second.
   *
   * routers-stale.json and collectors-stale.json were captured together,
   * within one second. dev-c2 had written a row at 13:34:03, a row newer
   * than every router's last_seen, so its archive.last_row_at is four and a
   * half minutes newer than the newest of them. A header built from last_seen reads
   * that much stale while rows keep landing.
   */
  it('reports the newest archived row across collectors, not the newest peer event', async () => {
    listRoutersMock.mockResolvedValue({ data: routersStale })
    listCollectorsMock.mockResolvedValue({ data: collectorsStale })
    const newestLastRow = '2026-09-24T13:34:03.99019Z'
    // Guards on the captures themselves: the winner is not the first
    // collector's row, and it is newer than every router's last_seen, so
    // the old source and a first-row read both give a different answer.
    expect(collectorsStale.data[0].archive?.last_row_at).not.toBe(newestLastRow)
    expect(collectorsStale.data.map((c) => c.archive?.last_row_at)).toContain(newestLastRow)
    for (const r of routersStale.data) {
      expect(Date.parse(r.last_seen!)).toBeLessThan(Date.parse(newestLastRow))
    }
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.newestRow.value).toBe(newestLastRow)
  })

  /**
   * The falsifier for a lexicographic max.
   *
   * Go renders timestamps as RFC3339 with trailing zeros stripped, so the
   * same daemon emits both "…:02Z" and "…:02.788894Z" and the archive
   * carries both shapes -- collectors-stale.json's own captured rows have
   * five and six sub-second digits. Compared as strings, '.' (0x2E) sorts
   * before 'Z' (0x5A), so the instant 788 ms LATER sorts EARLIER and the
   * chrome would report the older of the two as its freshest. Both rows
   * below are the same second, which is the only case where the two
   * implementations disagree and exactly the case a tidy fixture never
   * contains.
   */
  it('picks the later instant when two rows share a second at different precisions', async () => {
    listRoutersMock.mockResolvedValue({ data: routersStale })
    const [c1, c2] = collectorsStale.data
    listCollectorsMock.mockResolvedValue({
      data: {
        ...collectorsStale,
        data: [
          { ...c1, archive: { ...c1.archive!, last_row_at: '2026-09-24T13:58:02.788894Z' } },
          { ...c2, archive: { ...c2.archive!, last_row_at: '2026-09-24T13:58:02Z' } },
        ],
      },
    })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.newestRow.value).toBe('2026-09-24T13:58:02.788894Z')
  })

  // A configured collector the archive has no record of has archive: null.
  // It holds no row, so it contributes nothing rather than failing the read.
  it('skips a collector with no archive record', async () => {
    listRoutersMock.mockResolvedValue({ data: routersStale })
    const [c1] = collectorsStale.data
    listCollectorsMock.mockResolvedValue({
      data: { ...collectorsStale, data: [{ ...c1, archive: null }, c1] },
    })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.newestRow.value).toBe(c1.archive!.last_row_at)
  })

  // The routers answer is non-empty in each case below, so a newestRow
  // that fell back to router.last_seen would come out defined here.
  it('has no freshness fact for an empty collectors answer, rather than the epoch', async () => {
    listRoutersMock.mockResolvedValue({ data: routersStale })
    listCollectorsMock.mockResolvedValue({ data: { ...collectorsStale, data: [] } })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.collectors.value).toBe(2) // guard: the routers answer landed
    expect(chrome.newestRow.value).toBeUndefined()
  })

  it('has no freshness fact when the collectors request failed', async () => {
    listRoutersMock.mockResolvedValue({ data: routersStale })
    listCollectorsMock.mockResolvedValue({ error: { error: { code: 'x', message: 'nope' } } })
    const chrome = inColadaApp(() => useFleetChrome())
    await flushPromises()
    expect(chrome.collectors.value).toBe(2) // guard: the routers answer landed
    expect(chrome.newestRow.value).toBeUndefined()
  })

  // A failure AFTER a good answer. Colada keeps the last good data beside
  // the new error, so this is the case where only the error check stands
  // between the header and a stamp from an answer that no longer holds.
  it('drops the freshness fact when a later collectors request fails', async () => {
    listRoutersMock.mockResolvedValue({ data: routersStale })
    listCollectorsMock.mockResolvedValueOnce({ data: collectorsStale })
    const { chrome, query } = inColadaApp(() => ({
      chrome: useFleetChrome(),
      query: useCollectors(),
    }))
    await flushPromises()
    expect(chrome.newestRow.value).toBeDefined() // guard: the first answer landed
    listCollectorsMock.mockResolvedValue({ error: { error: { code: 'x', message: 'nope' } } })
    await query.refetch()
    await flushPromises()
    expect(query.data.value).toBeDefined() // guard: the stale data is still held
    expect(chrome.newestRow.value).toBeUndefined()
  })
})
