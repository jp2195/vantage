// Captures one screenshot of every operator-facing screen in the REAL,
// embedded UI bundle -- the one webui.Handler() serves out of the API
// daemon, not a vite dev server. A screen changing is then "re-run this
// script," not "remember which of eleven pictures are now lying."
//
// Requires the dev stack up (`docker compose -f docker-compose.dev.yml up
// -d`) with the replayers recently restarted -- route_unicast and
// peer_events both go stale fast, and a stale archive means the windowed
// screens (Monitor, Events, Session history) render real rows from an hour
// that is no longer "recent." `docker compose -f docker-compose.dev.yml
// restart evpn-replay ls-replay` re-sends the same captures without losing
// anything already archived -- see docs/troubleshooting.md's "A screen
// reads zero" section.
//
// Run with: npm --prefix ui run capture
//
// Every wait below is for a SELECTOR that only exists once real data has
// rendered -- never a fixed sleep. If a screen's wait times out, or the
// screen would otherwise render its empty state, this exits non-zero and
// names the screen: a capture run that cannot fail will happily photograph
// an empty dashboard, and every image after that is a lie nobody catches.

import { chromium, type Page } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const BASE_URL = 'http://127.0.0.1:9473'
// Matches ui/src/api/auth.ts's STORAGE_KEY and the dev daemon's token= in
// deploy/dev -- not a secret, a fixture value for a loopback-only daemon.
const TOKEN = 'dev-token-not-a-secret'
// The one place a rendered picture is allowed to still be a spinner or an
// empty state: this many milliseconds of polling for the real-data selector
// before a screen is declared a failure. Generous for a local dev stack
// over loopback; short enough that a genuinely broken screen fails the run
// in well under a minute rather than hanging it.
const DATA_TIMEOUT_MS = 20_000

const __dirname = dirname(fileURLToPath(import.meta.url))
const OUT_DIR = resolve(__dirname, '../../docs/images')

// ---- the daemon's own answers, read directly (not through the generated
// client -- this script runs under Node, outside the browser the client's
// relative URLs assume) ----

async function api<T>(path: string): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, {
    headers: { Authorization: `Bearer ${TOKEN}` },
  })
  if (!res.ok) {
    const body = await res.text().catch(() => '')
    throw new Error(`GET ${path} -> HTTP ${res.status}${body ? `: ${body}` : ''}`)
  }
  return (await res.json()) as T
}

interface PeerRow {
  router_ip: string
  peer_ip: string
  collector: string
  routes: number
}
interface RouterRow {
  ip: string
}
interface PeerEventRow {
  router_ip: string
  peer_ip: string
}
interface UnicastRouteRow {
  prefix: string
  family: string
  next_hop: string | null
  as_path: number[]
}

interface Scope {
  router: string
  peer: string
}

/** (router, then peer), ascending -- the one stable order that keeps
 *  results deterministic, so a re-run picks the same pair off an archive
 *  that has not changed shape, and a DIFFERENT pair the moment it has. */
function byRouterThenPeer<T extends { router_ip: string; peer_ip: string }>(rows: T[]): T[] {
  return [...rows].sort((a, b) =>
    a.router_ip === b.router_ip
      ? a.peer_ip.localeCompare(b.peer_ip)
      : a.router_ip.localeCompare(b.router_ip),
  )
}

// Each of the pick* functions below is memoized: called once, the first
// time a screen that needs it runs, and every later caller (including a
// second screen scoped the same way) reuses that one snapshot rather than
// re-querying a table that may have moved between screens. That is what
// "at the start of the run" buys here -- one consistent pick per kind of
// scope, not a fresh and possibly different one per screen.

let peersPromise: Promise<PeerRow[]> | undefined
function allPeers(): Promise<PeerRow[]> {
  if (!peersPromise) peersPromise = api<{ data: PeerRow[] }>('/v1/peers').then((r) => r.data)
  return peersPromise
}

/**
 * /peers/:router/:peer's scope: the peer with the MOST routes, ties broken
 * by the stable (router, peer) sort -- still fully deterministic, and no
 * longer able to land on an empty one.
 *
 * The original picker took the literal first (router, peer) pair by the
 * stable sort with no filter. Measured against a local dev deployment's
 * archive, the naive picker landed on a peer with routes: 0, no session
 * activity in the last 24 hours -- exactly the "photographs as zeros"
 * failure this fix exists to prevent, on the one screen whose whole
 * subject is a single peer. 13 of 35 peers carry routes > 0 on this
 * archive; the richest carries 50.
 *
 * Array.prototype.sort is stable (ECMA-262), so sorting the already
 * router-then-peer-sorted array by routes descending keeps every tie in
 * that original order -- ties are not rare here: three peers tie at 50
 * routes in that same local archive (all under one router).
 */
let peerDetailPromise: Promise<Scope> | undefined
function peerDetailScope(): Promise<Scope> {
  if (!peerDetailPromise) {
    peerDetailPromise = allPeers().then((peers) => {
      if (peers.length === 0) throw new Error('/v1/peers returned zero rows')
      const [best] = byRouterThenPeer(peers).sort((a, b) => b.routes - a.routes)
      return { router: best.router_ip, peer: best.peer_ip }
    })
  }
  return peerDetailPromise
}

interface ArchivedRoute {
  router: string
  peer: string
  prefix: string
  next_hop: string | null
  as_path: number[]
}

/**
 * Every real (family ipv4u/ipv6u, non-empty prefix) unicast row this
 * script's own API access can currently see, across every (router, peer,
 * collector) /v1/peers names -- deliberately NOT filtered to routes > 0
 * first. /v1/peers' `routes` count disagrees with a real walk in both
 * directions on a local dev archive: sometimes reading too HIGH (a
 * `routes: 1` peer with zero real ipv4u/ipv6u rows); router 172.22.0.7's
 * peer 172.31.0.90 is the opposite -- `routes: 0`, `state: view_lost`,
 * yet ClickHouse holds 43 real archived rows for it that neither this
 * walk nor /v1/routes (what the
 * Looking glass itself calls) will ever return, because a lost view has no
 * live session left to pin a walk to. Filtering on routes > 0 would still
 * miss that peer (routes reads 0), so the filter is dropped entirely here
 * and every candidate is asked for real: emptiness is discovered from the
 * walk's own answer, not from trusting the count first. The one exclusion
 * is loc_rib's sentinel peer_ip "0.0.0.0" rows -- bookkeeping, not a BGP
 * peer a prefix can be "observed" through.
 *
 * This is also, deliberately, ranking over what the API actually returns
 * rather than what ClickHouse holds underneath it:
 * a view_lost peer's archived rows are real, but the product will never
 * serve them, so they must not count toward any picker's ranking either.
 *
 * One limit=1000 request per (router, peer, collector) -- 1000 is
 * /v1/rib/unicast's documented maximum, and the richest peer in a local
 * dev archive (at 50 routes) is well under it, so one page is the whole
 * walk, no cursor needed. Memoized and shared across every picker below
 * that needs real route data (Routes, Looking glass, AS paths), so the
 * archive is walked once, not once per screen.
 */
let unicastArchivePromise: Promise<ArchivedRoute[]> | undefined
function unicastArchive(): Promise<ArchivedRoute[]> {
  if (!unicastArchivePromise) {
    unicastArchivePromise = (async () => {
      const peers = await allPeers()
      const candidates = byRouterThenPeer(peers.filter((p) => p.peer_ip !== '0.0.0.0'))
      const rows: ArchivedRoute[] = []
      for (const c of candidates) {
        const { data } = await api<{ data: UnicastRouteRow[] }>(
          `/v1/rib/unicast?router=${encodeURIComponent(c.router_ip)}&peer=${encodeURIComponent(c.peer_ip)}` +
            `&collector=${encodeURIComponent(c.collector)}&limit=1000`,
        )
        for (const r of data) {
          if ((r.family === 'ipv4u' || r.family === 'ipv6u') && r.prefix !== '') {
            rows.push({ router: c.router_ip, peer: c.peer_ip, prefix: r.prefix, next_hop: r.next_hop, as_path: r.as_path })
          }
        }
      }
      return rows
    })()
  }
  return unicastArchivePromise
}

/**
 * Routes' (router, peer): ranked by attribute VARIETY, not mere row count.
 *
 * The previous picker (first (router, peer) with routes > 0, verified
 * real) landed on a synthetic bmpgen-generated peer -- 50 rows identical
 * but for the prefix (same in_pre, path_id 0, next_hop 10.0.0.10,
 * as_path "65002 64512", origin 64512, no localpref/MED/communities).
 * That proves the table renders, not that vantage shows routing data.
 *
 * Ranked by distinct AS paths first, then distinct next hops, then row
 * count, over unicastArchive()'s own rows -- the same shared, live-API-only
 * walk lookingGlassPrefix() and asPathsScope() already use, so a peer whose
 * view is lost is correctly invisible here too (see unicastArchive()'s own
 * comment), not just to the other two. Ties keep unicastArchive()'s own
 * discovery order (router, then peer, ascending) -- the same stable-sort
 * rule the other two rankings use.
 */
let routesScopePromise: Promise<Scope> | undefined
function routesScope(): Promise<Scope> {
  if (!routesScopePromise) {
    routesScopePromise = unicastArchive().then((rows) => {
      const byPair = new Map<
        string,
        { router: string; peer: string; paths: Set<string>; nextHops: Set<string | null>; rows: number }
      >()
      for (const r of rows) {
        const key = `${r.router}|${r.peer}`
        let agg = byPair.get(key)
        if (!agg) {
          agg = { router: r.router, peer: r.peer, paths: new Set(), nextHops: new Set(), rows: 0 }
          byPair.set(key, agg)
        }
        agg.paths.add(r.as_path.join(','))
        agg.nextHops.add(r.next_hop)
        agg.rows += 1
      }
      if (byPair.size === 0) {
        throw new Error('no (router, peer) in this archive carries a real ipv4u/ipv6u row -- nothing to scope Routes to')
      }
      const [best] = [...byPair.values()].sort(
        (a, b) => b.paths.size - a.paths.size || b.nextHops.size - a.nextHops.size || b.rows - a.rows,
      )
      return { router: best.router, peer: best.peer }
    })
  }
  return routesScopePromise
}

/**
 * The Looking glass's search target: the prefix ranked best by observing
 * peers first (uniqExact (router, peer) over the archive's own rows), then
 * distinct AS paths, then row count -- "who sees this prefix, and by what
 * path" is the whole point of this screen, and a
 * one-path/one-peer prefix answers neither question. Ties keep the order
 * they were first discovered in while walking unicastArchive()'s candidates
 * (router, then peer, ascending; each candidate's own rows already
 * keyset-ordered by prefix) -- Array.prototype.sort is stable (ECMA-262),
 * so this is the same "stable sort settles ties" rule peerDetailScope()
 * already uses, generalized to three ranking keys instead of one.
 */
function lookingGlassPrefix(): Promise<string> {
  return unicastArchive().then((rows) => {
    const byPrefix = new Map<string, { peers: Set<string>; paths: Set<string>; rows: number }>()
    for (const r of rows) {
      let agg = byPrefix.get(r.prefix)
      if (!agg) {
        agg = { peers: new Set(), paths: new Set(), rows: 0 }
        byPrefix.set(r.prefix, agg)
      }
      agg.peers.add(`${r.router}|${r.peer}`)
      agg.paths.add(r.as_path.join(','))
      agg.rows += 1
    }
    if (byPrefix.size === 0) {
      throw new Error(
        'no real ipv4u/ipv6u prefix is visible across any (router, peer) this archive currently answers for ' +
          '-- nothing to scope the Looking glass to',
      )
    }
    const [best] = [...byPrefix.entries()].sort(
      (a, b) => b[1].peers.size - a[1].peers.size || b[1].paths.size - a[1].paths.size || b[1].rows - a[1].rows,
    )
    return best[0]
  })
}

/**
 * AS paths' (router, peer): the pair ranked best by distinct ASNs first,
 * then distinct AS paths, then the same stable-discovery-order tie-break
 * lookingGlassPrefix() uses -- "the most visually distinctive thing vantage
 * does" wants the busiest graph, not merely a non-empty one. Rows with an
 * empty as_path (a directly-originated or iBGP route with nothing to draw)
 * are excluded, matching the measurement query
 * (`WHERE length(as_path) > 0`).
 */
let asPathsScopePromise: Promise<Scope> | undefined
function asPathsScope(): Promise<Scope> {
  if (!asPathsScopePromise) {
    asPathsScopePromise = unicastArchive().then((rows) => {
      const byPair = new Map<string, { router: string; peer: string; asns: Set<number>; paths: Set<string> }>()
      for (const r of rows) {
        if (r.as_path.length === 0) continue
        const key = `${r.router}|${r.peer}`
        let agg = byPair.get(key)
        if (!agg) {
          agg = { router: r.router, peer: r.peer, asns: new Set(), paths: new Set() }
          byPair.set(key, agg)
        }
        agg.paths.add(r.as_path.join(','))
        for (const asn of r.as_path) agg.asns.add(asn)
      }
      if (byPair.size === 0) {
        throw new Error('no (router, peer) in this archive carries a real as_path -- nothing to scope AS paths to')
      }
      const [best] = [...byPair.values()].sort(
        (a, b) => b.asns.size - a.asns.size || b.paths.size - a.paths.size,
      )
      return { router: best.router, peer: best.peer }
    })
  }
  return asPathsScopePromise
}

/**
 * Session history reads the EVENTS archive, not /v1/peers -- its whole
 * point is answering for a session that may have since ended. So its scope
 * is picked from a real fleet-wide window of /v1/events rather than from
 * the peer list the other screens use.
 */
let sessionHistoryScopePromise: Promise<Scope> | undefined
function sessionHistoryScope(): Promise<Scope> {
  if (!sessionHistoryScopePromise) {
    sessionHistoryScopePromise = api<{ data: PeerEventRow[] }>('/v1/events?since=24h&limit=200').then(
      ({ data }) => {
        if (data.length === 0) {
          throw new Error('/v1/events?since=24h returned zero rows -- nothing to scope Session history to')
        }
        const [first] = byRouterThenPeer(data)
        return { router: first.router_ip, peer: first.peer_ip }
      },
    )
  }
  return sessionHistoryScopePromise
}

/**
 * Link state's router: the first router IP, ascending, whose /v1/ls/nodes
 * answer actually carries a node. Most routers in this lab speak no BGP-LS
 * at all (see LinkStateView.vue's own comments), so picking the literal
 * first router by IP would draw the screen's own "no link-state node in
 * this scope" empty view -- real, but not what this script is for.
 */
let linkStateRouterPromise: Promise<string> | undefined
function linkStateRouter(): Promise<string> {
  if (!linkStateRouterPromise) {
    linkStateRouterPromise = (async () => {
      const { data } = await api<{ data: RouterRow[] }>('/v1/routers')
      const ips = [...new Set(data.map((r) => r.ip))].sort()
      for (const ip of ips) {
        const nodes = await api<{ data: unknown[] }>(`/v1/ls/nodes?router=${encodeURIComponent(ip)}`)
        if (nodes.data.length > 0) return ip
      }
      throw new Error(
        `none of ${ips.length} routers in /v1/routers answers /v1/ls/nodes with any node -- no link-state view to draw`,
      )
    })()
  }
  return linkStateRouterPromise
}

// ---- capture plumbing ----

function screenshotPath(name: string): string {
  return resolve(OUT_DIR, `${name}.png`)
}

/** Waits for a selector that only exists once real data rendered, then
 *  screenshots the full page. Any timeout is rethrown naming the screen and
 *  the selector, so a failure reads as "peers: ..." rather than a bare
 *  Playwright TimeoutError several stack frames from the screen it was
 *  about to photograph. */
async function waitAndShoot(page: Page, screen: string, selector: string): Promise<void> {
  try {
    await page.waitForSelector(selector, { timeout: DATA_TIMEOUT_MS, state: 'attached' })
  } catch {
    throw new Error(
      `${screen}: no real data rendered within ${DATA_TIMEOUT_MS}ms (waited for "${selector}") -- ` +
        'the screen is likely still loading, showing an error, or showing its empty state',
    )
  }
  // fullPage rather than a per-screen fixed height: every screen here is
  // either a literal table (which can only be trusted whole, at whatever
  // length its rows run to) or a graph/dashboard whose own layout already
  // bounds its height (a fixed-width rail beside a fixed-viewBox canvas, or
  // a tile row above a couple of panels) -- so fullPage produces the same
  // framing a hand-picked height would, off one code path instead of two.
  await page.screenshot({ path: screenshotPath(screen), fullPage: true })
}

async function captureMonitor(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/monitor`)
  // The peers-by-update-volume table: the one panel on this screen that
  // joins two endpoints (churn + /v1/peers) into a single row, so its
  // presence proves more than any one endpoint answering on its own.
  await waitAndShoot(page, 'monitor', '[data-section="churn-peers"] tbody tr')
}

async function captureLookingGlass(page: Page): Promise<void> {
  const prefix = await lookingGlassPrefix()
  await page.goto(`${BASE_URL}/looking-glass?mode=prefix&q=${encodeURIComponent(prefix)}`)
  // Seeded via the URL exactly as a shared link would be (LookingGlassView's
  // own applyQuery()), so no click is needed -- the Paths tab's table is the
  // default view and only renders once the search actually matched rows.
  await waitAndShoot(page, 'looking-glass', '.pane table tbody tr')
}

async function captureAsPaths(page: Page): Promise<void> {
  const scope = await asPathsScope()
  await page.goto(`${BASE_URL}/topology`)
  // TopologyView reads no URL query at all (unlike Looking glass), so the
  // "from a peer" scope has to be driven through the form.
  await page.click('[data-mode="peer"]')
  const routerSelect = page.getByLabel('Router')
  // state: 'attached', not the default 'visible' -- an <option> inside a
  // closed <select> is never "visible" by Playwright's own actionability
  // rules, in a headless browser or otherwise; waiting for the default
  // state here would time out even once the option is really there.
  await routerSelect
    .locator(`option[value="${scope.router}"]`)
    .waitFor({ timeout: DATA_TIMEOUT_MS, state: 'attached' })
  await routerSelect.selectOption(scope.router)
  const peerSelect = page.getByLabel('Peer')
  await peerSelect
    .locator(`option[value="${scope.peer}"]`)
    .waitFor({ timeout: DATA_TIMEOUT_MS, state: 'attached' })
  await peerSelect.selectOption(scope.peer)
  // PathGraph draws one element per node with data-node="<asn>" -- present
  // only once the graph actually has a node to draw, unlike the summary
  // line above it, which also renders (as "0 ASNs · 0 edges") for an empty
  // graph.
  await waitAndShoot(page, 'as-paths', '.canvas [data-node]')
}

async function capturePeers(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/peers`)
  await waitAndShoot(page, 'peers', 'table tbody tr')
}

async function capturePeerDetail(page: Page): Promise<void> {
  const scope = await peerDetailScope()
  await page.goto(`${BASE_URL}/peers/${encodeURIComponent(scope.router)}/${encodeURIComponent(scope.peer)}`)
  // The ASN tile renders only once peerRows resolves to at least one row --
  // i.e. once the peer named in the URL was actually found. A ScreenHeader
  // title or a container div would be on screen during the loading state
  // too; this would not.
  await waitAndShoot(page, 'peer-detail', '[data-tile="asn"]')
}

async function captureLinkState(page: Page): Promise<void> {
  const router = await linkStateRouter()
  await page.goto(`${BASE_URL}/link-state`)
  const routerSelect = page.locator('select[data-router]')
  await routerSelect
    .locator(`option[value="${router}"]`)
    .waitFor({ timeout: DATA_TIMEOUT_MS, state: 'attached' })
  await routerSelect.selectOption(router)
  // Same rule as AS paths: LinkStateGraph draws one element per node with
  // data-node="<key>", present only once this router's view actually has a
  // node in it -- not merely once the summary sentence above it renders.
  await waitAndShoot(page, 'link-state', '.canvas [data-node]')
}

async function captureCollectors(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/collectors`)
  // Rendered only for a collector with an archive record (v-if="c.archive"),
  // which is the one tile a collector with no status endpoint at all never
  // gets -- present means at least one card is describing real archived
  // data rather than "no archive record for this id yet."
  await waitAndShoot(page, 'collectors', '[data-tile="peers"]')
}

async function captureRouters(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/routers`)
  await waitAndShoot(page, 'routers', 'table tbody tr')
}

async function captureRoutes(page: Page): Promise<void> {
  const scope = await routesScope()
  await page.goto(
    `${BASE_URL}/routes?router=${encodeURIComponent(scope.router)}&peer=${encodeURIComponent(scope.peer)}`,
  )
  await waitAndShoot(page, 'routes', 'table tbody tr')
}

async function captureEvents(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/events`)
  await waitAndShoot(page, 'events', 'table tbody tr')
}

async function captureSessionHistory(page: Page): Promise<void> {
  const scope = await sessionHistoryScope()
  await page.goto(
    `${BASE_URL}/session-history?router=${encodeURIComponent(scope.router)}&peer=${encodeURIComponent(scope.peer)}`,
  )
  await waitAndShoot(page, 'session-history', 'table tbody tr')
}

const SCREENS: { name: string; run: (page: Page) => Promise<void> }[] = [
  { name: 'monitor', run: captureMonitor },
  { name: 'looking-glass', run: captureLookingGlass },
  { name: 'as-paths', run: captureAsPaths },
  { name: 'peers', run: capturePeers },
  { name: 'peer-detail', run: capturePeerDetail },
  { name: 'link-state', run: captureLinkState },
  { name: 'collectors', run: captureCollectors },
  { name: 'routers', run: captureRouters },
  { name: 'routes', run: captureRoutes },
  { name: 'events', run: captureEvents },
  { name: 'session-history', run: captureSessionHistory },
]

async function main(): Promise<void> {
  mkdirSync(OUT_DIR, { recursive: true })

  const browser = await chromium.launch()
  const context = await browser.newContext({
    viewport: { width: 1920, height: 1080 },
    deviceScaleFactor: 2,
  })
  // Seeded BEFORE the app boots. ui/src/api/auth.ts's resolveAuth() reads
  // sessionStorage synchronously the first time the app renders; setting the
  // token after navigation races that read and the app asks for a token no
  // operator is here to type.
  await context.addInitScript((token: string) => {
    window.sessionStorage.setItem('vantage.token', token)
  }, TOKEN)
  const page = await context.newPage()

  const failures: { name: string; error: string }[] = []
  for (const screen of SCREENS) {
    try {
      await screen.run(page)
      console.log(`OK   ${screen.name}`)
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err)
      console.error(`FAIL ${screen.name}: ${message}`)
      failures.push({ name: screen.name, error: message })
    }
  }

  await browser.close()

  if (failures.length > 0) {
    console.error(`\n${failures.length} of ${SCREENS.length} screen(s) failed to capture real data:`)
    for (const f of failures) console.error(`  - ${f.name}: ${f.error}`)
    process.exitCode = 1
    return
  }

  console.log(`\nCaptured all ${SCREENS.length} screens into ${OUT_DIR}`)
}

main().catch((err) => {
  console.error(err instanceof Error ? err.stack ?? err.message : err)
  process.exitCode = 1
})
