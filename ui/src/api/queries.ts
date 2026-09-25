// Every server call the UI makes goes through this file. Screens import a
// composable, never an SDK operation and never fetch, so there is exactly one
// place where a query key, a refetch interval or an error shape is decided.
import { useQuery } from '@pinia/colada'
import { computed, getCurrentScope, onScopeDispose, ref, watch, type Ref } from 'vue'
import { ASN_BATCH_CAP } from '@/lib/asname'
import {
  collectionChurn,
  collectionChurnPeers,
  collectionChurnPrefixes,
  collectionDumps,
  collectionFlags,
  collectionLocRib,
  collectionSessions,
  findLsLinks,
  findLsNodes,
  findLsPrefixes,
  findPeerEvents,
  findRoutes,
  findTopology,
  listAsNames,
  listCollectors,
  listPeers,
  listRouters,
  ribEvpn,
  ribUnicast,
  ribVpn,
  routeHistory,
} from './generated'
import type {
  AsName,
  ChurnBucket,
  Collector,
  PeerChurn,
  PrefixChurn,
  FlagCount,
  HistoryEvent,
  LsLink,
  LsNode,
  LsPrefix,
  Meta,
  Peer,
  PeerEvent,
  PeerLocRib,
  Rib,
  Router,
  RouteFanout,
  RouterDumpCount,
  RouterSessionCount,
  TopologyFanout,
  UnicastRoute,
} from './generated'

/** The interval the AppShell's "last updated" reflects. There is no websocket. */
export const REFETCH_MS = 30_000

/**
 * Turns a generated operation's result into data, or throws.
 *
 * The Hey API client resolves with `{ data }` or `{ error }` rather than
 * rejecting, so without this every caller would have to remember to check.
 * The one that forgets renders an empty table, and an empty table reads as
 * "there are no routes" -- which is the confusion this whole project exists
 * to prevent.
 *
 * THE MESSAGE IS `error.error.message`, TWO LEVELS DOWN. `ErrorResponse` is
 * `{ error: { code, message } }` -- see `generated/types.gen.ts` and
 * `api/types.go`'s `ErrorResponse` / `ErrorBody` -- so `res.error` is the
 * envelope and `res.error.error` is the body OBJECT, not a string. Reading
 * one level (`e?.error`) handed `new Error()` an object, which stringifies
 * to `[object Object]`: every API error on every screen in this project
 * rendered that way. The daemon's own sentences are the whole point of
 * throwing here -- the unscoped-window 400 names the limit an operator has
 * to raise, and that sentence is tested at three layers before it reaches
 * this line.
 *
 * The two fallbacks below are not decoration either. A proxy in front of the
 * daemon can return plain text rather than the contract's JSON, which is the
 * string branch; and reading the nested `message` with `?.` rather than a
 * one-level read means an UNWRAPPED body (`{ error: { code, message } }`
 * arriving where the envelope was expected) still yields the daemon's own
 * sentence instead of falling through to the generic one.
 */
export function unwrap<T>(res: { data?: T; error?: unknown }): T {
  if (res.error !== undefined || res.data === undefined) {
    const e = res.error as
      | { error?: { code?: string; message?: string }; message?: string }
      | string
      | undefined
    const message =
      typeof e === 'string'
        ? e
        : (e?.error?.message ?? e?.message ?? 'the API returned no data and no error message')
    throw new Error(message)
  }
  return res.data
}

export type RibFamily = 'unicast' | 'vpn' | 'evpn'

/** The (router, peer) pair the RIB endpoints require, plus the session it is pinned to. */
export interface RibScope {
  router: string
  peer: string
  session: string
  /**
   * Which collector's view to walk.
   *
   * Required in practice on a router more than one collector monitors:
   * /v1/rib/* pins a walk to one (collector, session) and answers 400 when
   * it cannot tell which, so before this existed the Routes screen was
   * unusable on every dual-homed router. Optional on the wire because a
   * router with one collector needs no disambiguation and the endpoint
   * resolves it.
   *
   * It travels WITH `session`, never instead of it: a session_id is minted
   * by one collector's own clock, so the pair is the unit.
   */
  collector?: string
}

/**
 * The query key for one RIB walk.
 *
 * The session is part of the key deliberately. `meta.next_cursor` is only
 * meaningful within the BMP session that produced it, so when a session
 * changes the correct behavior is a new query -- not a cursor carried across
 * a boundary the server never promised to hold.
 */
export function ribKey(family: RibFamily, scope: RibScope) {
  // The collector is part of the key for the same reason the session is:
  // two collectors' walks of one peer are two different answers, and a
  // cached page from one must never be served for the other.
  return ['rib', family, scope.router, scope.peer, scope.session, scope.collector ?? ''] as const
}

/**
 * Refetches a query on a timer for as long as something is using it.
 *
 * The installed Pinia Colada (1.4.5, per node_modules/@pinia/colada's own
 * package.json) has no `refetchInterval` option -- `UseQueryOptions` in its
 * shipped .d.ts still carries only `gcTime`, `enabled`, `refetchOnMount`,
 * `refetchOnReconnect`, `refetchOnWindowFocus`, `staleTime` and
 * `ssrCatchError`, none of which polls a screen nobody is touching. (Polling
 * lives in a separate package, @pinia/colada-plugin-auto-refetch, which
 * this project does not use.)
 * REFETCH_MS still has to mean something, so this drives it directly: a
 * plain timer that calls the query's own `refetch`, torn down when the
 * component that started it unmounts. `getCurrentScope()` guards the
 * teardown registration because a composable invoked with no active effect
 * scope -- a bare function call in a test, never mounted -- has nothing
 * for `onScopeDispose` to attach to, and Vue warns rather than no-ops.
 *
 * The tick itself is skipped while a fetch is already running. Colada's own
 * `fetch` action (node_modules/@pinia/colada/dist/index.mjs, lines 437-472
 * in 1.4.5) unconditionally aborts any pending call before starting a new
 * one -- there is no framework-level de-dupe to lean on. Ticking into a
 * request slower than REFETCH_MS would abort it, restart it, and abort
 * that one too, forever: the request would never complete, and the same
 * action's error handling swallows that abort (a stale call's error only
 * reaches entry state when that call is still `entry.pending`, and
 * replacing it with the new one already made that false) -- so the screen
 * would sit on stale data with nothing on screen saying so. `isLoading` is
 * the alias Colada gives `asyncStatus.value === 'loading'`, and checking it
 * here is the whole fix. It holds across a replaced call because the same
 * action returns asyncStatus to 'idle' only when the call settling is still
 * `entry.pending`, so an aborted call that answers late leaves the query
 * loading. Before 1.4.2 that reset ran for every call, and a late answer
 * from an aborted request reopened the window this guard closes.
 * queries.test.ts holds both behaviors against the real library.
 */
export function pollWhileMounted(isLoading: Ref<boolean>, refetch: () => unknown): void {
  const id = setInterval(() => {
    if (!isLoading.value) refetch()
  }, REFETCH_MS)
  if (getCurrentScope()) onScopeDispose(() => clearInterval(id))
}

export function useRouters() {
  const query = useQuery({
    key: () => ['routers'],
    query: async () => unwrap(await listRouters()) as { data: Router[]; meta: Meta },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * The three fleet facts the chrome shows: how many collectors answered, when
 * this page last received an answer, and how fresh the archive behind that
 * answer is.
 *
 * Reads through `useRouters` and `useCollectors`, which means it shares
 * those keys' cache entries: on a screen that already reads either one,
 * the chrome adds no second request for it. On every other screen the
 * chrome's polls are its own, and each `/v1/collectors` poll makes the
 * daemon probe every configured collector's `/status`. The count and the
 * arrival stamp come from the routers answer; the archive's freshness
 * comes from the collectors answer.
 *
 * `collectors` is `undefined`, never 0, when the inventory is empty or the
 * request failed: "0 collectors" is the claim that nothing is answering, and
 * neither state supports it. `updatedAt` is the ARRIVAL time, which is why it
 * is a watched ref rather than a computed -- it is not a function of the
 * payload. It is deliberately not a server clock: this API returns none, and
 * the browser's own is a different clock from the collector's, which is the
 * distinction ts_router versus ts_collector exists to keep.
 *
 * `newestRow` is the fact `updatedAt` was being mistaken for. Receipt time
 * proves the page is live; it says nothing about whether the data is, and
 * the two were a single stamp for four days. They are rendered as two
 * labeled facts rather than one reconciled number for the same reason
 * `down` and `view_lost` stay two counts: each is true, they answer
 * different questions, and folding them together would lose the one an
 * operator actually needed.
 */
export function useFleetChrome() {
  const { data, error } = useRouters()
  const { data: collectorsData, error: collectorsError } = useCollectors()

  const collectors = computed<number | undefined>(() => {
    if (error.value) return undefined
    const rows = data.value?.data ?? []
    if (!rows.length) return undefined
    return new Set(rows.map((r) => r.collector)).size
  })

  const updatedAt = ref<string | undefined>(undefined)
  watch(
    data,
    (v) => {
      if (v) updatedAt.value = new Date().toISOString()
    },
    { immediate: true },
  )

  /**
   * The newest row any collector has archived -- the max of
   * `archive.last_row_at` over `/v1/collectors`, which is the archive's
   * freshness and NOT what updatedAt says.
   *
   * updatedAt is this browser's receipt time: proof the page is still
   * getting answers, and deliberately the browser's own clock (see
   * FleetChrome's doc comment). It says nothing about how old the data in
   * those answers is, and a reader takes it for exactly that. On a quiet
   * fleet the two diverge badly -- a small lab fleet archives around 21
   * rows a day, so the chrome can stamp the current second over an archive
   * whose newest row is hours old. `last_row_at` is on the collector's
   * clock.
   *
   * Not a router's `last_seen`: that is the newest peer event in the
   * router's current session, and under route churn with stable sessions
   * it stands still for hours while route rows keep landing.
   * `last_row_at` covers every data table the archive has.
   *
   * Compared as instants, never as strings. Go renders RFC3339 with
   * trailing zeros stripped, so "…:02Z" and "…:02.788894Z" both occur and
   * '.' sorts before 'Z' -- a lexicographic max would return the EARLIER of
   * two rows in the same second. queries.test.ts pins that case.
   *
   * undefined, never the epoch, for an empty or failed collectors answer:
   * the same rule the count follows, for the same reason. A collector the
   * archive has no record of (`archive: null`) holds no row and adds
   * nothing.
   */
  const newestRow = computed<string | undefined>(() => {
    if (collectorsError.value) return undefined
    let newest: string | undefined
    let newestMs = -Infinity
    for (const c of collectorsData.value?.data ?? []) {
      const at = c.archive?.last_row_at
      if (!at) continue
      const ms = Date.parse(at)
      if (Number.isNaN(ms) || ms <= newestMs) continue
      newestMs = ms
      newest = at
    }
    return newest
  })

  return { collectors, updatedAt, newestRow }
}

/**
 * The Collector health screen's one request: the union of every collector
 * id the archive has a record for and every id this daemon is configured
 * to poll -- api/openapi.yaml's own words for `GET /v1/collectors`, and see
 * that path's description for why either side alone is a different wrong
 * answer.
 *
 * A plain polling query, like useRouters, not a walk: the endpoint returns
 * one row per collector, capped at however many ids the union holds, never
 * paginated. What the SCREEN does with two successive answers -- deriving a
 * messages/second rate from the delta between their `status.observed_at`
 * values -- is CollectorsView's own job, not this composable's; Colada hands
 * a caller only the latest response, so keeping a memory of the sample
 * before it is presentation state, not a server call.
 */
export function useCollectors() {
  const query = useQuery({
    key: () => ['collectors'],
    query: async () => unwrap(await listCollectors()) as { data: Collector[]; meta: Meta },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

export function usePeers(router: Ref<string | undefined>, rib: Ref<string | undefined>) {
  const query = useQuery({
    key: () => ['peers', router.value ?? '', rib.value ?? ''],
    query: async () =>
      unwrap(
        await listPeers({
          // Screens hand this a bare `ref<string | undefined>()`, so `rib`
          // arrives untyped here; the generated query wants the Rib literal
          // union. A value outside it is a 400 from the daemon regardless,
          // so narrowing the type at the call site doesn't validate any
          // less than the daemon already does.
          query: { router: router.value, rib: rib.value as Rib | undefined },
        }),
      ) as { data: Peer[]; meta: Meta },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * A cursor walk over one peer's RIB, accumulating pages for infinite scroll.
 *
 * `restarted` is the honest half of this: if the walk is resumed against a
 * different session FOR THE SAME (router, peer), the accumulated rows are
 * dropped and the flag is raised so the screen can say so. Appending the
 * new session's rows to the old session's rows would produce a table that
 * never existed on the wire.
 *
 * The (router, peer) qualifier matters because a screen keeps one
 * `useRibPage` instance for its whole lifetime and calls `reload()` again
 * every time a scope picker hands it a different peer -- RoutesView does
 * exactly this. Comparing session alone would flag that ordinary re-scope as
 * a restart too, since a different peer's session_id is, definitionally, a
 * different string: a banner claiming "the walk restarted" about a walk
 * that never started would be its own confusion to audit for. Rows are
 * still dropped either way -- a fresh scope gets a clean table -- only the
 * banner is reserved for the case the comment above describes.
 */
export function useRibPage(family: RibFamily, scope: Ref<RibScope | undefined>) {
  const rows = ref<UnicastRoute[]>([])
  const meta = ref<Meta | undefined>()
  const restarted = ref(false)
  const loading = ref(false)
  const error = ref<Error | undefined>()
  let walked: RibScope | undefined
  // Monotonic, and compared after every await below: this composable is one
  // long-lived instance per screen, and a scope picker can start a second
  // walk while the first is still in flight. Whoever ANSWERED last used to
  // win, so picking a slow peer and then a fast one rendered the fast one
  // and then silently replaced its rows -- and its meta, warnings included
  // -- with the slow one's, while the picker still read the peer the
  // operator chose. A footer describing a peer no longer on screen is the
  // failure fixed on 2026-09-06 for the synchronous case (a scope change
  // clears meta along with rows); this is the same failure arriving late.
  //
  // A counter rather than `scope.value !== s`: ScopePicker emits a fresh
  // object every time, so an identity comparison would also discard a
  // response for a re-pick of the SAME peer -- correct answer, thrown away.
  let requestId = 0

  async function fetchPage(cursor?: string) {
    const s = scope.value
    if (!s) return
    const req = ++requestId
    const prior = walked
    // A collector change is a DIFFERENT walk, not a session change within
    // one: its cursor, its session and its rows all belong to the other
    // collector. Treating it as the same target would carry the previous
    // collector's rows and meta into it.
    const sameTarget =
      prior !== undefined &&
      prior.router === s.router &&
      prior.peer === s.peer &&
      prior.collector === s.collector
    // `meta` is cleared in lockstep with `rows` here, not left to whatever
    // the next fetch resolves to. Found while building RoutesView:
    // ResultMeta reads `meta` straight off this ref, with no way to tell
    // whose scope it describes. Without this, a scope change leaves the
    // PRIOR scope's meta sitting beside the just-cleared, now-empty `rows`
    // for as long as the new fetch takes -- a footer claiming "complete as
    // of ..." about an answer for a peer no longer on screen, which is the
    // exact "partial answer reads as complete" failure this project exists
    // to prevent.
    if (sameTarget && prior.session !== s.session) {
      rows.value = []
      meta.value = undefined
      restarted.value = true
    } else if (!sameTarget) {
      rows.value = []
      meta.value = undefined
    }
    walked = s
    loading.value = true
    error.value = undefined
    try {
      // collector is omitted rather than sent empty when the scope has
      // none: the endpoint treats an absent collector as "resolve it", and
      // an empty string is a collector id that matches nothing.
      const query = {
        router: s.router,
        peer: s.peer,
        cursor,
        limit: 500,
        ...(s.collector ? { collector: s.collector } : {}),
      }
      // Called per family rather than through ribOp[family](...): indexing
      // by a union key made TS infer the response as a union of
      // ribUnicast/ribVpn/ribEvpn's own shapes (VpnRoute and EvpnRoute rows
      // included), which unwrap<T> then can't reconcile with a single T no
      // matter which cast follows it. Branching keeps each call concrete.
      //
      // `rows` stays UnicastRoute[] regardless of family -- the RIB
      // screen only ever walks unicast, and a vpn/evpn walk's family-
      // specific fields (rd, label, route_type, ...) are read through a
      // local cast at that call site, the same pattern the Looking glass
      // screen already uses on a route row. Going through `unknown` for
      // vpn/evpn says that choice is deliberate rather than a mistake TS
      // should flag.
      const res =
        family === 'unicast'
          ? (unwrap(await ribUnicast({ query })) as { data: UnicastRoute[]; meta: Meta })
          : family === 'vpn'
            ? (unwrap(await ribVpn({ query })) as unknown as { data: UnicastRoute[]; meta: Meta })
            : (unwrap(await ribEvpn({ query })) as unknown as { data: UnicastRoute[]; meta: Meta })
      if (req !== requestId) return
      rows.value = cursor ? [...rows.value, ...res.data] : res.data
      meta.value = res.meta
    } catch (e) {
      // Guarded for the same reason, and it matters more: DataTable renders
      // an error INSTEAD of the table, so a superseded walk's failure would
      // blank the current peer's rows and blame them for it.
      if (req !== requestId) return
      error.value = e as Error
    } finally {
      // Only the walk still being awaited gets to say the screen has
      // stopped loading. A superseded request clearing this would drop
      // "loading…" from a table that still has nothing in it.
      if (req === requestId) loading.value = false
    }
  }

  return {
    rows,
    meta,
    restarted,
    loading,
    error,
    hasMore: computed(() => Boolean(meta.value?.next_cursor)),
    reload: () => {
      restarted.value = false
      return fetchPage(undefined)
    },
    loadMore: () => fetchPage(meta.value?.next_cursor ?? undefined),
  }
}

export interface RouteFilters {
  prefix?: string
  covers?: string
  origin_asn?: number
  through_asn?: number
  community?: string
  router?: string
  peer?: string
}

/**
 * The Looking glass query. Note there is no cursor and no limit: /v1/routes
 * caps rather than paginates, and says so with a `truncated` warning and a
 * non-null total_matched. Offering a "next page" here would invent one.
 *
 * `data` is a RouteFanout, not a flat route array. /v1/routes answers across
 * all three families in one keyed object -- api/openapi.yaml's own words are
 * "never a mixed array" -- so a screen that wants unicast paths reads
 * `data.value.data.unicast`, not `data.value.data`.
 *
 * With no filters it resolves to `undefined`, and that is the point rather
 * than a shortcut. This used to manufacture `{data: {unicast: [], vpn: [],
 * evpn: []}, meta: {warnings: [], total_matched: null}}` -- a Meta the
 * server never sent, whose empty `warnings` array is, by this API's own
 * contract, a positive claim that the answer is complete. The Looking glass
 * rendered it on first paint: "no rows matched" over "complete as of
 * HH:MM:SS", about a request that was never sent. `undefined` is the honest
 * value for "nothing was asked", and returning it means no screen CAN
 * render a completeness claim the server never made.
 */
export function useFindRoutes(filters: Ref<RouteFilters | undefined>) {
  return useQuery({
    key: () => ['routes', JSON.stringify(filters.value ?? {})],
    query: async () => {
      if (!filters.value) return undefined
      return unwrap(await findRoutes({ query: filters.value })) as {
        data: RouteFanout
        meta: Meta
      }
    },
  })
}

/**
 * The scope /v1/topology draws a graph for.
 *
 * The same parameter names RouteFilters carries, and deliberately its own
 * type rather than an alias of it, because the two endpoints' SUFFICIENCY
 * rules differ and that difference is the whole reason this path exists.
 * /v1/routes requires one of five CONTENT filters and treats router=/peer=
 * as narrowing only; here `router` AND `peer` together are a sufficient
 * scope, and either half alone is not (api/topology.go's
 * requireTopologyScope). A shared type would quietly invite a caller to
 * assume one endpoint's rule holds on the other, and it does not.
 *
 * `rib` is absent because no screen offers the control: the endpoint accepts
 * it, and a field nothing can set is surface without a caller.
 */
export interface TopologyScope {
  prefix?: string
  covers?: string
  router?: string
  peer?: string
  origin_asn?: number
  through_asn?: number
  community?: string
}

/**
 * The AS-path graph one scope's routes form -- three graphs, one per family.
 *
 * With no scope it resolves to `undefined`, for the reason useFindRoutes
 * does and one more of this endpoint's own. /v1/topology REFUSES an unscoped
 * request (400, naming what would make it answerable), so a composable that
 * fetched on mount would send a request the daemon rejects every time the
 * screen is opened. And the failure mode if it somehow answered is worse
 * here than an empty table: `{nodes: [], edges: []}` is, by this contract's
 * own words, the positive claim "nothing there" -- an empty graph pane would
 * assert that a scope nobody entered reaches no AS at all.
 *
 * No pollWhileMounted, unlike the fleet-wide composables above and like
 * useFindRoutes, whose question this shares. Two reasons, and the second is
 * specific to a graph: a scope here is a question an operator typed rather
 * than a dashboard left open, and PathGraph settles its layout once per
 * response -- so a poll would re-settle the picture underneath someone
 * reading it, every thirty seconds, on data that is usually identical.
 */
export function useTopology(scope: Ref<TopologyScope | undefined>) {
  return useQuery({
    key: () => ['topology', JSON.stringify(scope.value ?? {})],
    query: async () => {
      const s = scope.value
      if (!s) return undefined
      return unwrap(await findTopology({ query: s })) as { data: TopologyFanout; meta: Meta }
    },
  })
}

/**
 * The (router, protocol, area) scope one link-state graph is drawn for --
 * one router's view of one IGP topology, per LinkStateGraph.vue's rule 4
 * (OSPF area 0 and area 1 share no edges, so drawing them together would
 * invent adjacency that never existed on the wire).
 *
 * NOT the server's own `lsScoped(router, peer)` idea (the thing that decides
 * whether a /v1/ls/* request may be paginated). That scope is a pairing of
 * router and peer; this one names a topology, and `peer` is absent here for
 * the same reason `rib` is absent from TopologyScope above -- no screen
 * offers the control, and a field nothing can set is surface without a
 * caller.
 */
export interface LinkStateScope {
  router: string
  protocol?: number
  area?: number
}

/**
 * The query object all three /v1/ls paths take, built from one scope.
 *
 * `protocol` is a number in `LinkStateScope` -- the IANA registry's own
 * integers, which is what a future protocol picker would offer -- but the
 * generated client's `protocol` parameter is typed `string`, because the
 * wire format also accepts a name ("isis-l2"). Stringifying the number here
 * asks the identical question either notation does (the endpoint's own doc
 * comment: "protocol=2 and protocol=isis-l2 select the same rows"), so the
 * conversion changes nothing about what is asked -- it only satisfies the
 * shape the generated client requires.
 */
function lsQuery(s: LinkStateScope) {
  return {
    router: s.router,
    protocol: s.protocol === undefined ? undefined : String(s.protocol),
    area: s.area,
    limit: 10000,
  }
}

/**
 * One router's link-state view: the three /v1/ls paths that together feed
 * one LinkStateGraph, fetched as one answer.
 *
 * Every request carries the WHOLE scope. LinkStateGraph.vue's rule 4 means one
 * graph is one topology; a node query scoped differently from its link query
 * would silently draw adjacencies between two different topologies, and nothing
 * downstream -- not the graph, not a test -- could tell from the rows alone
 * that they came from different questions. So there is exactly one `scope`
 * here, spread into all three requests, rather than three independent filters a
 * caller could accidentally let drift apart.
 *
 * `limit: 10000` and no `cursor`, on all three, deliberately. Confirmed
 * directly against the running daemon on 2026-09-16 that both `truncated` and
 * `session_dumping` warnings are appended in handleLSNodes' NON-paginated
 * branch -- which is the branch a request with no cursor always takes.
 * Sending a cursor would trade that branch for the paginated one, which
 * reports through `lsPageMeta` instead and behaves differently, leaving this
 * composable answering a different question than the one screen it feeds:
 * one router's whole view, not one page of it.
 *
 * That safety net is per LEG, which is why all three metas are returned
 * below. handleLSLinks (api/handlers.go:1700-1711) and handleLSPrefixes
 * (1775-1784) build their own Meta exactly as handleLSNodes does, so a links
 * answer can be truncated or mid-dump while the nodes answer is neither.
 * Returning the nodes meta alone would let a screen mount one ResultMeta on
 * it and print "complete as of" beside a canvas that was silently short of
 * adjacencies.
 *
 * No scope combination is refused server-side, not even an empty one.
 * The daemon does not gate; the client does, and this
 * composable is where that gate lives: each query function below returns
 * `undefined` while `scope.value` is undefined, so none of the three fires
 * and no screen can render a graph for a router nobody chose.
 *
 * One poll, and it refreshes all THREE.
 *
 * Polling the nodes query alone shows in a browser's network panel as nodes
 * fetched six times while links and prefixes are fetched once each, under a
 * footer whose "complete as of" clock advances every 30 seconds. That dates a
 * whole answer with the freshness of one third of it -- an answer that is one
 * topology by LinkStateGraph.vue's rule 4, and whose adjacencies and prefixes
 * are by then a minute older than the nodes beside them. Saving a third of the
 * request volume is not worth a false claim about currency.
 *
 * Still one timer rather than three, and that part is not about load: three
 * independent timers would refresh the three legs at three different
 * instants, so the answer on screen would never be as of any one moment.
 * The tick is gated on any leg being in flight, for the reason
 * `pollWhileMounted` documents -- ticking into a slow request aborts and
 * restarts it forever.
 */
export function useLinkState(scope: Ref<LinkStateScope | undefined>) {
  const nodes = useQuery({
    key: () => ['ls-nodes', JSON.stringify(scope.value ?? {})],
    query: async () => {
      const s = scope.value
      if (!s) return undefined
      return unwrap(await findLsNodes({ query: lsQuery(s) })) as { data: LsNode[]; meta: Meta }
    },
  })
  const links = useQuery({
    key: () => ['ls-links', JSON.stringify(scope.value ?? {})],
    query: async () => {
      const s = scope.value
      if (!s) return undefined
      return unwrap(await findLsLinks({ query: lsQuery(s) })) as { data: LsLink[]; meta: Meta }
    },
  })

  const prefixes = useQuery({
    key: () => ['ls-prefixes', JSON.stringify(scope.value ?? {})],
    query: async () => {
      const s = scope.value
      if (!s) return undefined
      return unwrap(await findLsPrefixes({ query: lsQuery(s) })) as {
        data: LsPrefix[]
        meta: Meta
      }
    },
  })

  // One question in three parts: the legs are asked together, refreshed
  // together and reported together, so nothing on screen can be current
  // while the rest of the same answer is stale.
  const isLoading = computed(
    () => nodes.isLoading.value || links.isLoading.value || prefixes.isLoading.value,
  )
  const refetchAll = () => Promise.all([nodes.refetch(), links.refetch(), prefixes.refetch()])
  pollWhileMounted(isLoading, refetchAll)

  return {
    nodes: computed(() => nodes.data.value?.data),
    links: computed(() => links.data.value?.data),
    prefixes: computed(() => prefixes.data.value?.data),
    // Three metas, one per leg: never merged, and never reduced to one.
    //
    // Not merged, because ResultMeta renders `truncated` as "showing {shown}
    // of {meta.total_matched}" -- so a single Meta carrying the links
    // answer's warning beside the nodes answer's total would state a number
    // that is true of neither answer, and a caller could not say which query
    // any warning described.
    //
    // Not one, because each of the three handlers appends its OWN warnings
    // (see the note above). A caller that reads only `nodesMeta` is claiming
    // nothing about the other two answers, and must not print a completeness
    // claim over them; naming the leg in the field is what makes that
    // visible at the call site rather than in this comment alone.
    nodesMeta: computed(() => nodes.data.value?.meta),
    linksMeta: computed(() => links.data.value?.meta),
    prefixesMeta: computed(() => prefixes.data.value?.meta),
    isLoading,
    // The first error among the three, in fetch order. LinkStateGraph.vue's
    // rule 4 already means a caller cannot use one leg's data without the
    // others agreeing on the same scope, so surfacing whichever failed first is
    // as actionable as surfacing all three -- and the daemon's refusal sentence
    // is the same one regardless of which /v1/ls path said it.
    error: computed(() => nodes.error.value ?? links.error.value ?? prefixes.error.value),
    refetch: refetchAll,
  }
}

/**
 * One prefix's event timeline, over a window the CALLER names.
 *
 * `since` is required rather than optional, and that is the whole point of
 * the parameter. /v1/routes/history defaults ?since= to "1h"
 * (api/openapi.yaml, and api/handlers.go's since() implements it), so a
 * request that omits it still comes back bounded -- by a number the
 * operator never chose and no screen could name. That is a partial answer
 * reading as a complete one, one layer above where Meta.warnings guards.
 * Sending it explicitly means the value on screen and the value in the
 * request are the same value.
 *
 * The screen supplies a Go duration string. api/handlers.go tries RFC 3339
 * first and falls through to time.ParseDuration, which has no day unit --
 * "7d" is a 400 -- so a week is "168h" here.
 */
export function useRouteHistory(prefix: Ref<string | undefined>, since: Ref<string>) {
  return useQuery({
    key: () => ['history', prefix.value ?? '', since.value],
    query: async () => {
      // Aliased rather than read twice: routeHistory's `prefix` is required,
      // and a second `prefix.value` read a few lines down would ask Vue's
      // reactivity system, not the type checker, to prove it is still set.
      const p = prefix.value
      if (!p) return undefined
      return unwrap(await routeHistory({ query: { prefix: p, since: since.value } })) as {
        data: HistoryEvent[]
        meta: Meta
      }
    },
  })
}

/**
 * The (router, peer) pair /v1/events requires, plus the session ScopePicker
 * emits alongside it.
 *
 * `session` is carried rather than dropped, but never sent on the wire: the
 * events endpoint is not pinned to one BMP session the way /v1/rib/* and
 * /v1/ls/* are -- a peer's event history is itself the record of sessions
 * starting and ending, so requiring one would stop the walk at the very
 * transitions it exists to show. Keeping the field lets a screen pass
 * ScopePicker's output straight through without reshaping it.
 */
export interface EventScope {
  router: string
  peer: string
  /**
   * Optional, and never sent. fetchPage below passes router, peer, since,
   * cursor and limit -- api/openapi.yaml states outright that this walk
   * "in either mode -- is not pinned to one BMP session ... The scoped
   * cursor carries no session", because a session pin would stop the walk
   * at the very transitions the endpoint exists to show.
   *
   * It stays in the shape because callers that HAVE a session (PeerDetail
   * reads one off its peer row, ScopePicker resolves one) lose nothing by
   * carrying it. Optional so that a caller which legitimately has no
   * session -- SessionHistoryView answering for a peer whose session has
   * ended -- does not have to invent an empty string to ask a question
   * this endpoint answers on router and peer alone.
   */
  session?: string
}

/**
 * A cursor walk over one peer's event history, accumulating pages for
 * infinite scroll. Modeled on useRibPage above, which shipped two defects
 * this composable inherits the fix for rather than re-discovering:
 *
 * `rows` and `meta` are cleared together the instant the (router, peer)
 * target changes, not left to whatever the next fetch resolves to.
 * ResultMeta reads `meta` with no way to tell whose scope it describes, so a
 * `meta` that outlives the rows it described would leave a footer making a
 * completeness claim about the previous scope's answer.
 *
 * The write-back is guarded by a monotonic request id, compared after every
 * await. useRibPage shipped without this once: an abandoned scope's slow
 * response landed after a fast re-pick's had already rendered, and silently
 * overwrote its rows and meta with the stale scope's. The same guard covers
 * `error` and the `finally` that clears `loading`, for the same reason --
 * a superseded request is not allowed to speak for the scope on screen,
 * whether it succeeds, fails, or merely finishes.
 *
 * Unlike useRibPage, there is no `restarted` flag here. That flag exists to
 * warn when a walk pinned to one session gets yanked to another out from
 * under it -- a live-table smear, not the answer. /v1/events pins to no
 * session, so a session boundary crossed mid-walk is not an anomaly to flag;
 * it is the content the screen asked for.
 */
export function useEvents(scope: Ref<EventScope | undefined>, since: Ref<string>) {
  const rows = ref<PeerEvent[]>([])
  const meta = ref<Meta | undefined>()
  const loading = ref(false)
  const error = ref<Error | undefined>()
  let walked: EventScope | undefined
  // See useRibPage's own `requestId` for the failure this prevents; the
  // mechanism is identical, only the scope shape differs.
  let requestId = 0

  async function fetchPage(cursor?: string) {
    const s = scope.value
    if (!s) return
    const req = ++requestId
    const sameTarget =
      walked !== undefined && walked.router === s.router && walked.peer === s.peer
    if (!sameTarget) {
      rows.value = []
      meta.value = undefined
    }
    walked = s
    loading.value = true
    error.value = undefined
    try {
      const res = unwrap(
        await findPeerEvents({
          query: { router: s.router, peer: s.peer, since: since.value, cursor, limit: 500 },
        }),
      ) as { data: PeerEvent[]; meta: Meta }
      if (req !== requestId) return
      rows.value = cursor ? [...rows.value, ...res.data] : res.data
      meta.value = res.meta
    } catch (e) {
      if (req !== requestId) return
      error.value = e as Error
    } finally {
      if (req === requestId) loading.value = false
    }
  }

  return {
    rows,
    meta,
    loading,
    error,
    hasMore: computed(() => Boolean(meta.value?.next_cursor)),
    reload: () => fetchPage(undefined),
    loadMore: () => fetchPage(meta.value?.next_cursor ?? undefined),
  }
}

/**
 * The newest events anywhere in the fleet, over a window the CALLER names.
 *
 * No cursor and no loadMore, deliberately: /v1/events' unscoped mode caps
 * rather than paginates, and says so with a `truncated` warning and a
 * non-null total_matched. Offering a "next page" here would invent one --
 * the same call useFindRoutes makes for /v1/routes, and for the same reason.
 * It follows that none of useRibPage's accumulate-and-guard machinery
 * belongs here: there is no second request that could land out of order.
 *
 * `since` is required rather than optional, for useRouteHistory's reason
 * exactly. /v1/events defaults an absent ?since= to 1h server-side, so a
 * request that omitted it still comes back bounded -- by a number the
 * operator never chose and no screen could name. Sending it explicitly means
 * the value on screen and the value in the request are the same value, which
 * is what lets the screen state its window honestly.
 *
 * The window is in the query key. It is part of the identity of the answer,
 * not a parameter of it: a cached 1h answer served to a screen displaying
 * "last 6 hours" would be a wrong window stated confidently.
 *
 * Neither router nor peer is sent. Naming both would be the scoped walk;
 * naming exactly one is a 400. This composable exists to ask the question
 * that names neither.
 */
export function useFleetEvents(since: Ref<string>) {
  const query = useQuery({
    key: () => ['fleet-events', since.value],
    query: async () =>
      unwrap(await findPeerEvents({ query: { since: since.value, limit: 500 } })) as {
        data: PeerEvent[]
        meta: Meta
      },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * The four /v1/collection/* signals, one composable each.
 *
 * `useQuery`, not a walk: every one of these four is capped at limit=
 * (default 1000, same clamp every other /v1/collection/* path documents)
 * rather than paginated, so this is useFindRoutes' shape -- a single capped
 * answer -- not useRibPage's or useEvents' cursor-accumulating machinery.
 *
 * Four separate composables, not one that fans out to all four endpoints
 * and merges the results: that would collapse the one property the Monitor
 * screen exists to keep visible -- each signal loading and failing on its
 * own. A caller that awaited a single combined fetch would have no way to
 * show three answers while a fourth was still pending, or to keep three
 * good answers on screen while a fourth's request failed; Promise.all (or
 * Colada's own dependent-query wiring) would turn one endpoint's failure
 * into all four going dark. See MonitorView.vue's own comment on why the
 * API itself is shaped as four endpoints rather than one.
 *
 * `since` is required, for the same reason useRouteHistory's and
 * useFleetEvents' own `since` parameters are: every one of these four
 * defaults an absent ?since= to 1h server-side, so an omitted value would
 * still answer -- bounded by a window the screen never named and the
 * operator never chose. `router` is not exposed here: MonitorView asks a
 * fleet-wide question, the same choice EventsView's unscoped mode makes,
 * and nothing today needs the per-router narrowing these endpoints also
 * accept.
 */
export function useCollectionDumps(since: Ref<string>) {
  const query = useQuery({
    key: () => ['collection-dumps', since.value],
    query: async () =>
      unwrap(await collectionDumps({ query: { since: since.value } })) as {
        data: RouterDumpCount[]
        meta: Meta
      },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

export function useCollectionSessions(since: Ref<string>) {
  const query = useQuery({
    key: () => ['collection-sessions', since.value],
    query: async () =>
      unwrap(await collectionSessions({ query: { since: since.value } })) as {
        data: RouterSessionCount[]
        meta: Meta
      },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

export function useCollectionLocRib(since: Ref<string>) {
  const query = useQuery({
    key: () => ['collection-locrib', since.value],
    query: async () =>
      unwrap(await collectionLocRib({ query: { since: since.value } })) as {
        data: PeerLocRib[]
        meta: Meta
      },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * Churn over time: the fifth collection signal, and the only one with a time
 * axis.
 *
 * `bucket` is sent explicitly rather than left to the daemon's default,
 * because the width is what a bar MEANS and a screen that let the server
 * choose could not label its own chart. The answer states the width it used
 * in meta.churn_bucket regardless, and callers should render THAT rather
 * than the value they asked for -- the two agree today, and a screen that
 * assumed they always would is one clamp away from a chart labeled with a
 * width it was not drawn at.
 *
 * `scope` narrows to one router or one peer. Absent, the answer is
 * fleet-wide, which is what Monitor asks for.
 */
export function useCollectionChurn(
  since: Ref<string>,
  bucket: Ref<string>,
  scope?: Ref<{ router?: string; peer?: string } | undefined>,
) {
  const query = useQuery({
    key: () => [
      'collection-churn',
      since.value,
      bucket.value,
      scope?.value?.router ?? '',
      scope?.value?.peer ?? '',
    ],
    query: async () =>
      unwrap(
        await collectionChurn({
          query: {
            since: since.value,
            bucket: bucket.value,
            router: scope?.value?.router,
            peer: scope?.value?.peer,
          },
        }),
      ) as { data: ChurnBucket[]; meta: Meta },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * The churn signal ranked by peer, busiest first: Monitor's "peers by update
 * volume" table.
 *
 * No `bucket`, because this answer has no time axis -- it is
 * useCollectionChurn's classification summed rather than drawn, so there are
 * no bars to give a width to and no meta.churn_bucket to render.
 *
 * THE COUNTS ARE ARCHIVED ROWS, NOT THE ROUTER'S UPDATE RATE. A screen
 * turning one into a per-second figure is reporting what this collector
 * STORED, and has to say so -- see MonitorView's own column header.
 */
export function useCollectionChurnPeers(since: Ref<string>) {
  const query = useQuery({
    key: () => ['collection-churn-peers', since.value],
    query: async () =>
      unwrap(await collectionChurnPeers({ query: { since: since.value } })) as {
        data: PeerChurn[]
        meta: Meta
      },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * One churn answer PER COLLECTOR, keyed so a screen whose rows are already
 * per collector can join without crossing vantage points.
 *
 * /v1/collection/churn/peers answers at (router, peer) and carries no
 * collector column. Unscoped it is the BEST SINGLE VANTAGE POINT -- the
 * collector that saw the most, reported whole -- and it does not say which
 * collector that was. The Peers table's rows ARE per (collector, router,
 * peer, rib), so putting that unscoped number on every row of a dual-homed
 * peer would label one collector's measurement as another's: the
 * present-an-arbitrary-collector's-answer defect this project has now
 * removed from the dashboards, the Looking glass, the AS-path graph, the
 * Peers table and two fleet counts.
 *
 * So the collector axis is resolved the way the contract says to resolve it:
 * `collector=` narrows to one vantage point, and this issues one request per
 * collector currently on screen. That is a handful of requests -- collectors
 * are few by nature, and the list comes from the rows being rendered -- and
 * the alternative is a number that cannot be attributed to the row it sits
 * on.
 *
 * ONE useQuery over N fetches, not N composables: a composable per collector
 * would be a hook count that changes with the data, which Vue does not allow
 * and which would break the moment a collector appeared or left. Same shape
 * useAsNames uses for its batch, with Promise.all where that has one call.
 *
 * The rib axis is NOT resolved, and cannot be from this endpoint: churn is
 * answered at (router, peer). No peer in a lab archive is mirrored under
 * more than one rib today (measured 2026-09-21: 35 rows, 35 distinct
 * (collector, router, peer)), but the shape allows it, and a peer that was
 * would see its whole-peer rate rendered against each of its rib rows. The
 * screen says so rather than this pretending otherwise.
 */
export function useChurnPeersByCollector(
  since: Ref<string>,
  collectors: Ref<string[]>,
  router?: Ref<string | undefined>,
) {
  const ids = computed(() => Array.from(new Set(collectors.value)).sort())
  const query = useQuery({
    key: () => [
      'collection-churn-peers-by-collector',
      since.value,
      ids.value.join(','),
      router?.value ?? '',
    ],
    query: async () => {
      if (ids.value.length === 0) return undefined
      // allSettled, never all: one collector's failure must not take the
      // others' rates off the screen with it. Promise.all rejects on the
      // first rejection, so a single 500 left every cell reading "—" --
      // including rows whose own collector answered -- with nothing saying
      // why. This file already rules that out by name for the Monitor
      // signals; the fan-out shipped with the wrong primitive anyway.
      const settled = await Promise.allSettled(
        ids.value.map(async (collector) => {
          const answer = unwrap(
            await collectionChurnPeers({
              query: { since: since.value, collector, router: router?.value || undefined },
            }),
          ) as { data: PeerChurn[]; meta: Meta }
          return {
            collector,
            rows: answer.data ?? [],
            from: answer.meta?.churn_from,
            to: answer.meta?.churn_to,
            // The width one bar stands for. Sent by this path for the same
            // reason /v1/collection/churn sends its own, and dropped here
            // until omitting it was found to draw every bar narrower
            // than its true span.
            bucket: answer.meta?.churn_bucket,
          }
        }),
      )
      const by = new Map<string, PeerChurn>()
      const failed: string[] = []
      settled.forEach((r, i) => {
        if (r.status === 'fulfilled') {
          for (const row of r.value.rows) {
            by.set(churnKey(r.value.collector, row.router_ip, row.peer_ip), row)
          }
        } else {
          failed.push(ids.value[i])
        }
      })
      // Surfaced rather than swallowed: a collector whose rates are missing
      // is a fact the screen has to be able to say, or the dashes in its
      // rows read as "archived nothing".
      // The window the answers covered, taken from the first that arrived.
      //
      // NOT identical across the fan-out, and the earlier comment here said
      // it was. since= is usually a DURATION, so each handler resolves it
      // against its own time.Now(): N requests produce N windows, skewed by
      // however long the fan-out takes to issue them. The skew is bounded
      // by that -- milliseconds against a window of hours -- and one axis
      // for the whole column is what makes the rows comparable, so taking
      // one answer's bounds is the right trade rather than an oversight.
      // What it is NOT is a client subtraction off new Date(), which would
      // put collector timestamps against the browser's clock.
      const meta = settled.find((r) => r.status === 'fulfilled')
      const window =
        meta && meta.status === 'fulfilled'
          ? { from: meta.value.from, to: meta.value.to, bucket: meta.value.bucket }
          : undefined
      return { by, failed, window }
    },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * The join key, exported so the screen cannot build a different one. Two
 * spellings of the same key is how a join quietly stops matching and every
 * cell renders the absent case instead.
 */
export function churnKey(collector: string, router: string, peer: string): string {
  return `${collector}|${router}|${peer}`
}

/**
 * The most-changed prefixes in the window, ranked: Peer detail's "most churn
 * from this peer", which is why `scope` is not optional here the way it is on
 * useCollectionChurn. An unscoped ranking is a fleet-wide question no screen
 * currently asks, and passing a partial scope by accident would answer it
 * silently.
 *
 * At most 50 rows, and route_unicast only -- see the endpoint's own contract
 * for why a prefix cannot be summed across the three route tables, and why
 * rows whose NLRI did not decode are excluded rather than ranked first under
 * a blank label.
 */
export function useCollectionChurnPrefixes(
  since: Ref<string>,
  scope: Ref<{ router: string; peer: string } | undefined>,
) {
  const query = useQuery({
    key: () => [
      'collection-churn-prefixes',
      since.value,
      scope.value?.router ?? '',
      scope.value?.peer ?? '',
    ],
    query: async () => {
      const s = scope.value
      if (!s) return { data: [] as PrefixChurn[], meta: {} as Meta }
      return unwrap(
        await collectionChurnPrefixes({
          query: { since: since.value, router: s.router, peer: s.peer },
        }),
      ) as { data: PrefixChurn[]; meta: Meta }
    },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

export function useCollectionFlags(since: Ref<string>) {
  const query = useQuery({
    key: () => ['collection-flags', since.value],
    query: async () =>
      unwrap(await collectionFlags({ query: { since: since.value } })) as {
        data: FlagCount[]
        meta: Meta
      },
  })
  pollWhileMounted(query.isLoading, () => query.refetch())
  return query
}

/**
 * AS holder names for the batch of ASNs a screen is about to render.
 *
 * `asns` is read as whatever a screen currently has on screen -- one entry
 * per row, duplicates included -- because a keyed useQuery's own reactivity
 * is what decides whether to refetch, not the caller pre-deduping. Deduped,
 * sorted (so the key is stable regardless of row order) and capped HERE,
 * once, rather than at each of the three call sites -- "how a screen turns
 * its rows into a batch" has exactly one answer.
 *
 * Resolves to `undefined` with no ASNs to ask about, the same "nothing
 * asked, say nothing" shape useFindRoutes and useTopology use for their own
 * optional fetches: /v1/asnames refuses a request naming none with a 400,
 * and a screen with no rows loaded yet must never send that request.
 *
 * No `pollWhileMounted`. The batch itself already changes the instant the
 * screen's OWN polling composable (usePeers, useRibPage, useTopology) hands
 * it a row with a new ASN, and Colada refetches this query the moment its
 * key -- the distinct, sorted batch -- changes. A second timer here would
 * only repeat a request whose answer is very unlikely to differ: the
 * dataset itself changes only when an operator re-runs `make fetch-asnames`
 * and applies the dictionary, not on a 30-second clock.
 */
export function useAsNames(asns: Ref<number[]>) {
  const distinct = computed(() => Array.from(new Set(asns.value)).sort((a, b) => a - b))
  /**
   * The batch actually sent: the cap GET /v1/asnames itself enforces
   * (`api/asnames.go`'s maxASNBatch, and `api/openapi.yaml`'s words on
   * `asn=`), applied here too, defensively. Without it a screen whose
   * distinct ASNs exceed the cap would send a batch the daemon rejects
   * outright with a 400 -- and on this ENRICHMENT lookup that refusal
   * would blank every name on the page rather than degrade to naming most
   * of them.
   */
  const batch = computed(() => distinct.value.slice(0, ASN_BATCH_CAP))
  /**
   * Whether the cap actually bit -- the half a screen has to be able to SAY.
   *
   * Capping keeps the lookup from becoming a 400 that blanks the page, but
   * it is not free: `slice` keeps the numerically LOWEST ASN_BATCH_CAP
   * ASNs, so past the cutoff every high-numbered ASN arrives with no name
   * and renders exactly like one the dataset genuinely does not list. That
   * is the collapse `@/lib/asname` exists to prevent, and the cap
   * reintroduces it silently unless the screen is told. This is what tells
   * it; `asNamesTruncationNotice` turns it into the sentence.
   */
  const truncated = computed(() => distinct.value.length > ASN_BATCH_CAP)
  return {
    ...useQuery({
      key: () => ['asnames', JSON.stringify(batch.value)],
      query: async () => {
        if (batch.value.length === 0) return undefined
        return unwrap(await listAsNames({ query: { asn: batch.value } })) as {
          data: AsName[]
          meta: Meta
        }
      },
    }),
    truncated,
  }
}
