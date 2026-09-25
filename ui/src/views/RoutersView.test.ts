import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { ref } from 'vue'
import { columnWidths, columnsOf, tableMinWidthPx } from '@/test-support/columnsOf'
import routers from '@/api/fixtures/routers.json'
import routersStale from '@/api/fixtures/routers-stale.json'
import { inventedColumns } from '@/test-support/columnGuard'
import { twoCollectorRouters } from '@/test-support/twoCollectorRouters'

// Set inside each test below via vi.mocked(...).mockReturnValue, never at
// module scope: the isolation rule requires every test to pass alone via
// `-t`, and a shared mock return value here would let one test's fixture
// leak into the next.
vi.mock('@/api/queries', () => ({ useRouters: vi.fn() }))

const { useRouters } = await import('@/api/queries')
const RoutersView = (await import('./RoutersView.vue')).default

describe('RoutersView', () => {
  // The title names only what this test asserts. That the peer counts stay
  // separate columns (view_lost the collector blind, down the router
  // reporting, stale nobody hearing from the collector) is pinned by the
  // view_lost and Stale column tests below.
  it("renders the captured router's name, address and up count", async () => {
    vi.mocked(useRouters).mockReturnValue({
      data: ref(routers),
      isPending: ref(false),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    const r = routers.data[0]
    expect(w.text()).toContain(r.sysname)
    expect(w.text()).toContain(r.ip)
    expect(w.text()).toContain(String(r.peers_up))
  })

  // /v1/routers is one entry per (collector, router), so a router two
  // collectors monitor arrives as TWO entries -- and the header counted
  // them. Measured on a lab deployment 2026-09-21: 19 entries for 15 routers,
  // rendered as "ROUTERS 19". "Routers" is a fact about the FLEET and must
  // not grow because a second collector watched, which is the rule applied
  // on 2026-09-20 to the Looking glass's "paths" and to the AS-path graph's
  // three counts.
  //
  // The two halves are asserted together on purpose, because the fix is
  // only right if BOTH hold: the table keeps showing what was observed --
  // two rows, two real observations that can disagree -- while the count
  // answers the question it is labeled with.
  it('counts routers, not the entries two collectors produce for one of them', () => {
    vi.mocked(useRouters).mockReturnValue({
      data: ref(twoCollectorRouters()),
      isPending: ref(false),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    expect(w.findAll('tbody tr')).toHaveLength(2)
    const header = w.find('header, .head')?.text() ?? w.text()
    expect(header).toMatch(/Routers\s*1\b/)
    expect(header).not.toMatch(/Routers\s*2\b/)
  })

  it('labels the view_lost count as its own column, not merged into down', () => {
    vi.mocked(useRouters).mockReturnValue({
      data: ref(routers),
      isPending: ref(false),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    const headers = w.findAll('th').map((h) => h.text().toLowerCase())
    expect(headers.some((h) => /view lost/.test(h))).toBe(true)
    expect(headers.some((h) => /^down$/.test(h))).toBe(true)
  })

  it('counts stale peers in their own column, under the router whose collector went quiet', () => {
    vi.mocked(useRouters).mockReturnValue({
      data: ref(routersStale),
      isPending: ref(false),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    const headers = w.findAll('th').map((h) => h.text())
    const col = headers.indexOf('Stale')
    expect(col).toBeGreaterThan(-1)
    const stale = routersStale.data.find((r) => r.peers_stale > 0)
    if (!stale) throw new Error('routers-stale.json has no stale router')
    const row = w.findAll('tbody tr').find((tr) => tr.text().includes(stale.ip) && tr.text().includes(stale.collector))
    if (!row) throw new Error(`no row for ${stale.ip} under ${stale.collector}`)
    expect(row.findAll('td')[col].text()).toBe(String(stale.peers_stale))
  })

  it('renders no column the API does not measure, under a header that does not either', () => {
    // Two independent claims, both required (see test-support/columnGuard.ts):
    //
    // Not a blocklist of banned names ("no messages-per-second, no lag, no
    // drop count, no uptime") -- that is a list a future column addition
    // would have to remember to extend, and the whole point of this screen
    // is that a reader should never have to remember which telemetry it
    // does not measure. The Router shape itself is the closed set for
    // column ids: the fixture's row is real server output (see
    // fixtures/README.md), so its own JSON keys ARE that shape,
    // authoritatively, with no hand-typed list to drift out of sync with
    // api/openapi.yaml.
    //
    // But an id alone is not the whole claim. `{ id: 'last_seen', header:
    // 'Uptime' }` has a perfectly real id and still lies about what the
    // column shows -- a legitimate field wearing a label for a metric this
    // pipeline does not measure. inventedColumns' second half catches
    // exactly that, and is what backporting this guard from PeersView
    // closes here too.
    //
    // Shallow-mounted so the columns array reaching DataTable can be read
    // directly off the stub's props, rather than reverse-engineered from
    // rendered header text -- which would require a name-mapping table
    // that is itself something to remember to extend.
    vi.mocked(useRouters).mockReturnValue({
      data: ref(routers),
      isPending: ref(false),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView, { shallow: true })
    const columns = columnsOf(w)
    // routers-stale.json, not routers.json: the closed set of fields is the
    // CURRENT contract's, and routers.json was captured before peers_stale
    // existed. routers-stale.json was captured from this contract.
    expect(inventedColumns(columns, routersStale.data[0])).toEqual([])
  })

  it('renders last_seen as a readable clock string, not the raw ISO instant', () => {
    // Presentation, not invention: the fixture's last_seen is
    // "2026-09-06T19:14:28.788894Z". An operator reads a clock, not a
    // string with a T and a Z and six digits of sub-second precision in it.
    //
    // Asserting only that the raw ISO string is absent would also pass for
    // an empty-string formatter -- that proves nothing rendered, not that
    // something readable did. Computing the expected value the same way
    // the component computes it (rather than hardcoding one locale's output
    // as a magic string) pins the actual rendered value without coupling
    // the test to a specific format choice beyond the component's own.
    vi.mocked(useRouters).mockReturnValue({
      data: ref(routers),
      isPending: ref(false),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    const expected = new Date(routers.data[0].last_seen).toLocaleString([], {
      dateStyle: 'medium',
      timeStyle: 'medium',
    })
    expect(w.text()).toContain(expected)
    expect(w.text()).not.toContain(routers.data[0].last_seen)
  })

  // --- The loading/refetch defect owned by this screen ---
  //
  // useRouters() polls every 30s and Colada preserves `data` across a
  // refetch while flipping `asyncStatus` to "loading" -- `isLoading` in
  // Colada's own naming is an alias for asyncStatus === 'loading', true on
  // the FIRST fetch and on every later poll tick alike. `isPending` is the
  // other signal Colada exposes (state.status === 'pending'): true only
  // until the first response ever lands, false for the rest of the
  // component's life regardless of how many refetches follow. DataTable's
  // `loading` prop means "there is nothing to show yet, block everything" --
  // that is isPending's meaning, not isLoading's.
  //
  // `data.data` is deliberately `[]` here, not the fixture's one row. This
  // component's own DataTable.vue was separately hardened to keep ANY
  // non-empty table from blanking while `loading` is true
  // (`loading && rows.length === 0`), which is real, independent
  // protection -- but it also means a mock with real rows would pass this
  // assertion even if RoutersView.vue were reverted straight back to
  // `:loading="isLoading"`, because DataTable's own guard would mask the
  // wrong wiring at the call site. An empty `data` array (a real, completed
  // "no routers matched" result -- the fetch already succeeded, so
  // isPending is false) removes that mask: DataTable's row-count guard
  // cannot tell "no data yet" from "an empty result already arrived" by
  // row count alone, so isPending being wired correctly at THIS call site
  // is the only thing left that can keep the loading banner off screen.
  // This test fails against `:loading="isLoading"` and passes against
  // `:loading="isPending"` -- checked by reverting RoutersView.vue alone,
  // leaving DataTable.vue's hardening in place.
  it('does not blank the table or contradict the footer while a background poll is in flight', () => {
    vi.mocked(useRouters).mockReturnValue({
      data: ref({ data: [], meta: routers.meta }),
      isPending: ref(false),
      isLoading: ref(true),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    expect(w.text()).not.toMatch(/loading/i)
    expect(w.text()).toMatch(/no rows matched/i)
    // routers.meta carries an empty warnings array -- the footer must still
    // say so, and must not say it beneath a "loading…" banner.
    expect(w.text()).toMatch(/complete/i)
  })

  it('blocks on the loading state before any response has ever landed', () => {
    vi.mocked(useRouters).mockReturnValue({
      data: ref(undefined),
      isPending: ref(true),
      isLoading: ref(true),
      error: ref(undefined),
    } as never)
    const w = mount(RoutersView)
    expect(w.text()).toMatch(/loading/i)
    expect(w.text()).not.toContain(routers.data[0].sysname)
  })
  // The smear guard. Declared widths are what keep a table packed instead of
  // spread across a 2000px viewport; see columnWidths' own note for the
  // measurement that prompted it.
  it('declares a width for every column, so the table cannot smear', () => {
    const w = mount(RoutersView)
    const widths = columnWidths(w)
    expect(widths.length).toBeGreaterThan(0)
    expect(widths.filter((x) => !x)).toEqual([])
  })
})

it('names a router that sent no sysName instead of leaving the cell blank', async () => {
  // A router that never sends a BMP Initiation leaves router_sysname as
  // the empty string -- a real value the collector accepts by design, and
  // one two routers in the live archive already report. Rendered raw it is
  // a blank cell in the Router column, which an operator cannot tell from
  // a rendering fault. Found in a browser on 2026-09-20.
  vi.mocked(useRouters).mockReturnValue({
    data: ref({ ...routers, data: [{ ...routers.data[0], sysname: '' }] }),
    isPending: ref(false),
    error: ref(undefined),
  } as never)
  const w = mount(RoutersView)
  expect(w.text()).toContain('(no sysName TLV)')
})

// Six of the eight columns are fixed px widths totaling 612px, and
// table-layout:fixed hands the two percentage columns only what is left.
// On a 390px phone that was nothing: Router and Collector rendered 0px
// wide and their headers overprinted their neighbors. The table's floor
// has to hold the fixed columns AND the percentages taken of the floor
// itself, or the percentages collapse again below it.
it('floors the table wide enough for its fixed columns plus its percentages', () => {
  vi.mocked(useRouters).mockReturnValue({
    data: ref(routersStale),
    isPending: ref(false),
    error: ref(undefined),
  } as never)
  const w = mount(RoutersView, { shallow: true })
  const floor = tableMinWidthPx(w)
  expect(floor).toBeDefined()
  const needed = columnWidths(w).reduce((sum, width) => {
    if (!width) throw new Error('a column has no declared width')
    return sum + (width.endsWith('%') ? (floor! * parseFloat(width)) / 100 : parseFloat(width))
  }, 0)
  expect(needed).toBeLessThanOrEqual(floor!)
})
