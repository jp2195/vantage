import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { type Ref, ref } from 'vue'
import fleetEvents from '@/api/fixtures/fleet-events.json'
import events from '@/api/fixtures/events.json'
import twoCollectors from '@/api/fixtures/events-two-collectors.json'
import { columnWidths, columnsOf } from '@/test-support/columnsOf'
import { inventedColumns } from '@/test-support/columnGuard'
import { capturedMeta } from '@/test-support/capturedMeta'

let fixture: { data: unknown[]; meta: unknown } = fleetEvents
// Drives useFleetEvents' own isPending, independent of isLoading, so a test
// can distinguish the two through the mock rather than by trying to
// reproduce Colada's asyncStatus state machine. Reset in beforeEach.
let isPendingFixture = false
const refetch = vi.fn()

// No ScopePicker and no URL sync to drive: this screen asks a question that
// names no scope, so there is no picker gate to get past and no query
// string to seed or restore. The mock is still here, harness-shaped like
// SessionHistoryView.test.ts's, in case a future change reaches for
// useRoute/useRouter -- EventsView itself calls neither today.
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
}))

vi.mock('@/api/queries', () => ({
  useFleetEvents: (_since: Ref<string>) => ({
    data: ref({ data: fixture.data, meta: fixture.meta }),
    isPending: ref(isPendingFixture),
    isLoading: ref(false),
    error: ref(undefined),
    refetch,
  }),
}))

const EventsView = (await import('./EventsView.vue')).default

describe('EventsView', () => {
  beforeEach(() => {
    fixture = fleetEvents
    isPendingFixture = false
    refetch.mockClear()
  })

  // The screen's own reason to exist, and the one thing Session history
  // cannot do (a scope there is exactly one router). It counts distinct
  // ROUTERS rather than peers: "fleet" names spanning devices more directly
  // than spanning peers on one device would, and data-router exists on the
  // row for this purpose.
  it('shows events from more than one router, which is what a fleet view means', () => {
    const w = mount(EventsView)
    const routers = new Set(w.findAll('tbody tr').map((r) => r.attributes('data-router')))
    expect(routers.size).toBeGreaterThan(1)
  })

  // /v1/events is one row per (collector, router, peer, session), so a
  // router two collectors monitor contributes TWO rows for one event --
  // and until 2026-09-20 they rendered identical in every visible column.
  // Same defect the Peers table carried until its Collector column landed
  // on 2026-09-20; same
  // remedy, for the same reason recorded in PeersView.vue: two collectors'
  // observations are two answers that can disagree, so naming one of them
  // silently is the unstated pick this project audits for.
  //
  // It is worst for view_lost, which is DEFINED as one collector losing its
  // own BMP transport. A view_lost with no collector named is a fact with
  // its subject removed.
  //
  // The fixture is a real capture, and every visible field on its four rows
  // is identical within a kind EXCEPT collector -- so a column rendering
  // some other field that happens to differ cannot pass this by accident.
  it('names the collector that saw each event, so two views of one peer are not identical rows', () => {
    fixture = twoCollectors
    const w = mount(EventsView)
    const rows = w.findAll('tbody tr')
    expect(rows).toHaveLength(4)
    const texts = rows.map((r) => r.text())
    expect(texts.filter((t) => t.includes('dev-c1'))).toHaveLength(2)
    expect(texts.filter((t) => t.includes('dev-c2'))).toHaveLength(2)
    // The defect itself, stated directly: no two rows read the same.
    expect(new Set(texts).size).toBe(4)
  })

  // One of the two cases most easily made vacuous in a test like this.
  // It must fail when the statement is REMOVED and when the window is WRONG,
  // so it asserts the note carries the window actually in use -- not merely
  // that some note exists.
  //
  // Drives the real <select>, not the `since` ref directly: setting a
  // captured ref only proves the note reacts to whatever ref useFleetEvents
  // happens to be handed, not that the DROPDOWN drives that same ref. A
  // v-model dropped, or bound to an unrelated ref, would still pass a
  // ref-only assertion and leave the screen's only control dead.
  it('states the window it covers, and states the right one', async () => {
    const w = mount(EventsView)
    const note = w.find('[data-window-note]')
    expect(note.exists()).toBe(true)
    expect(note.text()).toContain('last hour')

    await w.findAll('[data-since-window] button')[1].trigger('click')
    expect(w.find('[data-window-note]').text()).toContain('last 6 hours')
    expect(w.find('[data-window-note]').text()).not.toContain('last hour')
  })

  // EventArchiveNote.test.ts proves the component renders whatever
  // windowLabel prop it is handed; this proves EventsView hands it the
  // window actually in use, the same gap SessionHistoryView.test.ts's own
  // "names the window actually selected in the archive note" closed for the
  // other consumer of this component (2026-09-09). A hard-coded label there
  // would pass every other test in this file, including the window-note
  // assertion above, which reads a sibling element entirely.
  it('names the window actually selected in the archive note, not a stale or literal one', async () => {
    const w = mount(EventsView)
    expect(w.find('[data-archive-note]').text()).toContain('last hour')

    await w.findAll('[data-since-window] button')[1].trigger('click')
    expect(w.find('[data-archive-note]').text()).toContain('last 6 hours')
    expect(w.find('[data-archive-note]').text()).not.toContain('last hour')
  })

  // The real consequence of isLoading in place of
  // isPending is narrower than "blanks a populated table" -- DataTable's own
  // blocking gate is `loading && rows.length === 0`, so a populated table
  // can never be blanked by either flag. What differs is the EMPTY case:
  // isPending stays false forever after the first response lands, while
  // isLoading flips true again on every poll tick. Driving isPending
  // directly here (with isLoading pinned false, as it always is once a
  // first fetch has resolved) distinguishes "no data yet, block everything"
  // from "a stale empty answer is quietly being re-fetched" through the
  // real seam DataTable reads -- no need to reproduce Colada's asyncStatus
  // machinery to prove the wiring.
  it('shows loading rather than "no rows matched" while the first fetch is still pending', () => {
    isPendingFixture = true
    fixture = { data: [], meta: undefined }
    const w = mount(EventsView)
    expect(w.text()).toContain('loading')
    expect(w.text()).not.toContain('no rows matched')
  })

  // The other case worth its own test. It must distinguish "capped at 5" from
  // "there were 5", which is exactly what a test asserting the rendered row
  // count cannot do. fleet-events.json is a REAL capped answer (see its
  // README section), so both numbers here are the daemon's own.
  //
  // Asserting `footer.text() !== 'showing 5 of 5'` would be vacuous --
  // footer.text() carries the other ResultMeta spans too, so it can never equal
  // that bare string regardless of whether the screen actually distinguishes
  // "capped" from "complete". What actually distinguishes the two is here
  // instead: a guard that the fixture really capped, a toContain that fails the
  // moment the two numbers are made equal, and an explicit assertion that the
  // truncated warning itself is present -- so "capped" is also distinguished
  // from "complete", not only from "wrong count".
  it('says how many matched, not just how many are shown', () => {
    const meta = capturedMeta(fleetEvents.meta)
    const total = meta.total_matched!
    expect(total).toBeGreaterThan(fleetEvents.data.length) // guard: the fixture really capped
    const w = mount(EventsView)
    const footer = w.find('footer.meta')
    expect(footer.text()).toContain(`showing ${fleetEvents.data.length} of ${total}`)
    expect(footer.find('.note').exists()).toBe(true)
  })

  // An empty answer under a window note is the exact shape that must be
  // stated, and the case testing in a browser found on a sibling screen:
  // it must not read as "nothing broke".
  it('states the window even when nothing matched, because empty is not the same as quiet', () => {
    fixture = { data: [], meta: { next_cursor: null, total_matched: 0, warnings: [] } }
    const w = mount(EventsView)
    expect(w.text()).toContain('no rows matched')
    expect(w.find('[data-window-note]').exists()).toBe(true)
    expect(w.find('[data-archive-note]').exists()).toBe(true)
  })

  // Driven from events.json, not fleet-events.json. fleet-events.json's
  // own 5 rows are a real cap from a local dev archive, but
  // every one of them is kind "up" -- that archive has zero view_lost
  // rows (measured: up 2744, down 99, view_lost 0), because view_lost
  // is written only by the collector's session-close path and no
  // session in that archive has ever dropped. That is not fixable by
  // re-capturing fleet-events.json.
  // events.json is this repo's only real captured view_lost (3 of its 6
  // rows), off the same /v1/events endpoint and the same PeerEvent shape --
  // the scoped mode's page rather than the unscoped one's. This proves the
  // screen renders a view_lost correctly; it does NOT prove the unscoped
  // endpoint has ever actually returned one.
  it('renders view_lost as the collector losing its view, not as a down', () => {
    fixture = events
    const w = mount(EventsView)
    const marks = w.findAll('.kind-mark.view_lost')
    expect(marks.length).toBeGreaterThan(0)
    expect(marks[0].text()).toContain('collector lost view')
  })

  // Same fixture substitution and the same honest limit as the test above.
  it('shows no reason for a view_lost, because zero there is absence', () => {
    fixture = events
    const w = mount(EventsView)
    const row = w.find('tr[data-kind="view_lost"]')
    expect(row.find('[data-reason]').text()).toBe('—')
  })

  // Presentation, not invention: the fixture's ts_collector is
  // "2026-08-30T02:36:27.758903Z". An operator reads a clock, not a string
  // with a T and a Z and six digits of sub-second precision in it. Computed
  // the same way the screen computes it, the same reasoning RoutersView's
  // own version of this test gives for not hardcoding one locale's output.
  it('renders ts_collector as a readable clock string, not the raw ISO instant', () => {
    const w = mount(EventsView)
    const expected = new Date(fleetEvents.data[0].ts_collector).toLocaleString([], {
      dateStyle: 'medium',
      timeStyle: 'medium',
    })
    expect(w.text()).toContain(expected)
    expect(w.text()).not.toContain(fleetEvents.data[0].ts_collector)
  })

  it('renders no column the API does not measure', () => {
    const w = mount(EventsView)
    expect(inventedColumns(columnsOf(w), fleetEvents.data[0] as Record<string, unknown>)).toEqual(
      [],
    )
  })

  // A capped fleet list is exactly where a severity ranking would feel
  // natural and would be invented. columnGuard catches it by header text.
  it('has no severity column, invented under any name', () => {
    const w = mount(EventsView)
    const headers = columnsOf(w).map((c) => c.header.toLowerCase())
    expect(headers).not.toContain('severity')
    expect(headers).not.toContain('sev')
  })

  // Offering a window the daemon refuses would produce a 400 an operator
  // could not have predicted from the dropdown they were given.
  it('offers only windows inside the daemon default clamp', () => {
    const w = mount(EventsView)
    const values = w.findAll('[data-since-window] button').map((o) => o.attributes('value'))
    expect(values).toEqual(['1h', '6h', '24h'])
  })

  it('says the notification itself is not retained', () => {
    const w = mount(EventsView)
    expect(w.find('[data-archive-note]').text()).toContain('not retained')
  })
  // The smear guard. Declared widths are what keep a table packed instead of
  // spread across a 2000px viewport; see columnWidths' own note for the
  // measurement that prompted it.
  it('declares a width for every column, so the table cannot smear', () => {
    const w = mount(EventsView)
    const widths = columnWidths(w)
    expect(widths.length).toBeGreaterThan(0)
    expect(widths.filter((x) => !x)).toEqual([])
  })
})

it('names a router that sent no sysName instead of leaving the cell blank', () => {
  // As RoutersView's own case, and sharper here: this table has no address
  // column beside the name, so a blank Router cell leaves the row with
  // nothing identifying the device at all.
  fixture = {
    data: [{ ...(fleetEvents.data[0] as Record<string, unknown>), router_sysname: '' }],
    meta: fleetEvents.meta,
  }
  const w = mount(EventsView)
  expect(w.text()).toContain('(no sysName TLV)')
})
