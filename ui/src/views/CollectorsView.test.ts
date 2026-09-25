import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { nextTick, ref } from 'vue'
import type { Collector, CollectorActivity, CollectorRouter } from '@/api/generated'
import { tableMinWidthPx } from '@/test-support/columnsOf'

// Same mechanism SessionHistoryView.test.ts and RoutesView.test.ts document:
// useCollectors hands the component a ref, and this module variable is
// reassigned to that SAME ref every time the mock factory runs, so a test
// can push a LATER poll's answer into it after mount and observe how the
// screen reacts to two samples arriving over time -- which is the entire
// point of the message-rate tests below. A `mockReturnValue` that replaced
// the object wholesale would not do this: the component captures the ref
// once, at setup, and a later poll is Colada mutating THAT ref's `.value`,
// never handing the component a new one.
let collectorsRef = ref<{ data: Collector[]; meta: { activity_window?: string } } | undefined>(
  undefined,
)

vi.mock('@/api/queries', () => ({
  useCollectors: () => {
    return {
      data: collectorsRef,
      isPending: ref(false),
      isLoading: ref(false),
      error: ref(undefined),
    }
  },
}))

const CollectorsView = (await import('./CollectorsView.vue')).default

/**
 * SYNTHESIZED fixtures throughout this file, not captured -- and that is
 * unavoidable rather than a shortcut. Every state this screen exists to
 * distinguish (an id disagreement, a counter that ran backward between two
 * polls, a collector with no configured endpoint) is either a timing
 * behavior that only exists across two requests a real capture cannot hold
 * in one file, or a deployment mistake nobody has reproduced against the
 * usual local dev deployment (see ui/src/api/fixtures/README.md's own
 * guidance for exactly this situation: when a real archive cannot show
 * a shape, say so rather than working around it -- there is no archive
 * that can show an id mismatch on demand). Every field below is a real
 * field of the committed
 * `Collector`/`CollectorRouter`/`CollectorActivity` wire types, though; only
 * the values are invented, never the shape.
 */

const T0 = '2026-09-18T15:00:00.000Z'
const T1 = '2026-09-18T15:01:00.000Z' // 60s after T0
const T2 = '2026-09-18T15:02:00.000Z' // 60s after T1

function zeroFilledActivity(n = 5): CollectorActivity[] {
  return Array.from({ length: n }, (_, i) => ({
    minute: new Date(Date.parse(T0) + i * 60_000).toISOString(),
    rows: 0,
  }))
}

/**
 * A series with REAL rows in it, which every other fixture in this file
 * lacks: makeCollector hands out zeroFilledActivity, so before this existed
 * nothing in the UI suite ever rendered a non-zero bar and barHeight,
 * barWidth, the `.current` class on the last bucket and the per-bucket
 * <title> had no coverage at all -- on a chart whose whole job is drawing
 * them.
 *
 * The values are deliberately uneven, with the PEAK in the middle rather
 * than at either end, so a bar scaled against the wrong bucket (the first,
 * the last, or a hard-coded maximum) lands somewhere this file can see.
 */
const BUSY_ROWS = [0, 12, 40, 5, 20]
function busyActivity(): CollectorActivity[] {
  return BUSY_ROWS.map((rows, i) => ({
    minute: new Date(Date.parse(T0) + i * 60_000).toISOString(),
    rows,
  }))
}

const busyRouter: CollectorRouter = {
  sysname: 'edge1',
  ip: '10.50.1.1',
  peers_up: 3,
  peers_down: 1,
  peers_view_lost: 0,
  peers_stale: 0,
  sys_descr: 'Cisco IOS XR Software, Version 7.11.1',
  last_seen: '2026-09-18T14:59:00Z',
}

const quietRouter: CollectorRouter = {
  sysname: 'edge2',
  ip: '10.50.1.2',
  peers_up: 1,
  peers_down: 0,
  peers_view_lost: 0,
  peers_stale: 0,
  sys_descr: '', // never observed -- see collectors_test.go's identical case
  last_seen: '2026-09-18T14:40:00Z',
}

function makeCollector(over: Partial<Collector> = {}): Collector {
  return {
    collector: 'dev-c1',
    archive: {
      routers: [busyRouter, quietRouter],
      peers_up: 4,
      peers_down: 1,
      peers_view_lost: 0,
      peers_stale: 0,
      last_row_at: T0,
      last_beat_at: T0,
      started_at: '2026-09-18T10:00:00.000Z',
    },
    status: {
      started_at: '2026-09-18T10:00:00.000Z',
      observed_at: T0,
      sessions_active: 4,
      bmp_messages_total: 1000,
      events_published_total: 990,
      publish_errors_total: 2,
      publish_rejects_total: 1,
    },
    reachable: true,
    error: '',
    endpoint: 'http://dev-c1:9469/status',
    id_mismatch: '',
    activity: zeroFilledActivity(),
    ...over,
  }
}

function envelope(data: Collector[]) {
  return { data, meta: { warnings: [], total_matched: null, activity_window: '30m0s' } }
}

function card(w: ReturnType<typeof mount>, id: string) {
  return w.get(`[data-collector="${id}"]`)
}

describe('CollectorsView', () => {
  // --- Last heard is the heartbeat, and never is its own answer ---

  it('shows last heard from the heartbeat, and "never" for a collector never heard from', () => {
    const heard = makeCollector()
    const silent = makeCollector({ collector: 'dev-c9' })
    silent.archive = { ...silent.archive!, last_beat_at: null, started_at: null }
    collectorsRef.value = envelope([heard, silent])
    const w = mount(CollectorsView)
    expect(card(w, 'dev-c9').get('[data-tile="last-heard"]').text()).toMatch(/never/)
    expect(card(w, 'dev-c1').get('[data-tile="last-heard"]').text()).not.toMatch(/never/)
  })

  it("names the API's stale threshold rather than a fixed number", () => {
    // The threshold is the API's stale_after, which an operator can change;
    // 90 s is only its default.
    collectorsRef.value = envelope([makeCollector()])
    const w = mount(CollectorsView)
    const note = card(w, 'dev-c1').get('[data-tile="last-heard"]').attributes('title') ?? ''
    expect(note).toMatch(/after the API's stale threshold \(90 s by default\)/)
  })

  it('counts stale peers in the peers tile without counting them as up', () => {
    const c = makeCollector()
    c.archive = { ...c.archive!, peers_up: 1, peers_down: 0, peers_view_lost: 0, peers_stale: 3 }
    collectorsRef.value = envelope([c])
    const w = mount(CollectorsView)
    const tile = card(w, 'dev-c1').get('[data-tile="peers"]')
    expect(tile.text()).toContain('1 / 4')
    expect(tile.text()).toMatch(/3 stale/)
  })

  // --- The rate is a derivative ---

  it('shows no message rate until it has two samples', () => {
    collectorsRef.value = envelope([makeCollector()])
    const w = mount(CollectorsView)
    const tile = card(w, 'dev-c1').get('[data-tile="msg-rate"]')
    expect(tile.text()).toContain('—')
    expect(tile.text()).not.toMatch(/\b0\b/)
  })

  it("computes msg/s from the delta between two samples and their own observed_at", async () => {
    collectorsRef.value = envelope([
      makeCollector({ status: { ...makeCollector().status!, bmp_messages_total: 1000, observed_at: T0 } }),
    ])
    const w = mount(CollectorsView)

    // Second poll: 300 more messages over the 60s gap BETWEEN THE TWO
    // observed_at VALUES -- never the browser's clock, which this test does
    // not touch at all.
    collectorsRef.value = envelope([
      makeCollector({ status: { ...makeCollector().status!, bmp_messages_total: 1300, observed_at: T1 } }),
    ])
    await nextTick()
    await nextTick()

    const tile = card(w, 'dev-c1').get('[data-tile="msg-rate"]')
    expect(tile.text()).toContain('5.00')
  })

  it('treats a counter going backward as a restart, not as a negative rate', async () => {
    collectorsRef.value = envelope([
      makeCollector({ status: { ...makeCollector().status!, bmp_messages_total: 1000, observed_at: T0 } }),
    ])
    const w = mount(CollectorsView)

    // The process behind this endpoint restarted: its own counter is now
    // LOWER than the sample this screen already holds, even though more
    // real time has passed.
    collectorsRef.value = envelope([
      makeCollector({ status: { ...makeCollector().status!, bmp_messages_total: 40, observed_at: T2 } }),
    ])
    await nextTick()
    await nextTick()

    const tile = card(w, 'dev-c1').get('[data-tile="msg-rate"]')
    expect(tile.text()).toContain('—')
    expect(tile.text()).not.toMatch(/-\d/)
  })

  // --- Zero is not absence ---

  it('says a collector has no status endpoint configured rather than showing zeros', () => {
    collectorsRef.value = envelope([
      makeCollector({ status: null, reachable: null, error: '', endpoint: '' }),
    ])
    const w = mount(CollectorsView)
    const c = card(w, 'dev-c1')

    expect(c.text()).toMatch(/no status endpoint/i)
    // The badge itself, not just the separate `data-no-status` paragraph
    // that also happens to carry this text: a regression that rendered
    // this state's badge as DEGRADED (or dropped its neutral styling)
    // would leave the paragraph assertion above satisfied on its own and
    // ship a fully green suite. `reachable: null` has no health question
    // to answer -- see badgeOf's own doc comment -- so the badge must be
    // neither the healthy nor the degraded color.
    const badge = c.get('[data-badge]')
    expect(badge.text()).toMatch(/no status endpoint/i)
    expect(badge.classes()).toContain('unconfigured')
    expect(badge.classes()).not.toContain('healthy')
    expect(badge.classes()).not.toContain('degraded')
    // None of the status-derived tiles exist at all -- not present-and-zero.
    for (const key of ['msg-rate', 'uptime', 'publish-errors', 'publish-rejects']) {
      expect(c.find(`[data-tile="${key}"]`).exists()).toBe(false)
    }
    // The archive-derived facts are still there: this collector is still in
    // the archive, only unconfigured.
    expect(c.find('[data-tile="peers"]').exists()).toBe(true)
  })

  it('keeps an unreachable collector on screen with its archive facts and the reason', () => {
    collectorsRef.value = envelope([
      makeCollector({
        status: null,
        reachable: false,
        error: 'Get "http://dev-c1:9469/status": dial tcp: connection refused',
      }),
    ])
    const w = mount(CollectorsView)
    const c = card(w, 'dev-c1')

    expect(c.find('[data-badge]').text()).toMatch(/degraded/i)
    expect(c.get('[data-error]').text()).toContain('connection refused')
    // Archive facts survive: the router this collector's archive holds is
    // still on screen, not blanked because the daemon went quiet.
    expect(c.text()).toContain('edge1')
    expect(c.find('[data-tile="peers"]').exists()).toBe(true)
    expect(c.find('[data-tile="msg-rate"]').exists()).toBe(false)
  })

  it('renders the id disagreement when a daemon reports an id its config does not', () => {
    collectorsRef.value = envelope([
      makeCollector({
        collector: 'actually-someone-else',
        id_mismatch: 'configured-name',
        reachable: true,
      }),
      makeCollector({
        collector: 'configured-name',
        archive: null,
        status: null,
        reachable: false,
        error: 'answered as actually-someone-else, not configured-name',
        id_mismatch: '',
        activity: zeroFilledActivity(),
      }),
    ])
    const w = mount(CollectorsView)

    const reported = card(w, 'actually-someone-else')
    expect(reported.get('[data-id-mismatch]').text()).toContain('configured-name')
    expect(reported.get('[data-id-mismatch]').text()).toContain('actually-someone-else')

    const configured = card(w, 'configured-name')
    expect(configured.find('[data-id-mismatch]').exists()).toBe(false)
    expect(configured.find('[data-badge]').text()).toMatch(/degraded/i)
    expect(configured.get('[data-error]').text()).toContain('answered as actually-someone-else')

    // The archive-side paragraph on this same card, whose SENTENCE used to
    // be gated on `!archive` alone: it said "configured, and answering"
    // directly beneath the DEGRADED badge and the alert asserted above,
    // with nothing in this file reading its text. It has to follow
    // `reachable`, not just the archive.
    const note = configured.get('[data-not-archived]').text()
    expect(note).toMatch(/did not answer/i)
    expect(note).not.toMatch(/and answering/i)
  })

  // --- sys_descr is empty for a router never observed carrying it ---

  it('omits the OS line for a router whose sys_descr was never observed', () => {
    collectorsRef.value = envelope([makeCollector()])
    const w = mount(CollectorsView)
    const c = card(w, 'dev-c1')

    const busy = c.get(`[data-router="${busyRouter.ip}"]`)
    expect(busy.get('[data-os-line]').text()).toContain(busyRouter.sys_descr)

    const quiet = c.get(`[data-router="${quietRouter.ip}"]`)
    expect(quiet.find('[data-os-line]').exists()).toBe(false)
  })

  // --- The label is the guard ---

  it('labels the sparkline as rows archived per minute, never as messages/s', () => {
    collectorsRef.value = envelope([makeCollector()])
    const w = mount(CollectorsView)
    const c = card(w, 'dev-c1')

    const axis = c.get('[data-activity-axis]')
    expect(axis.text()).toMatch(/rows archived per minute/i)
    expect(axis.text()).not.toMatch(/messages\/s/i)
    expect(c.get('[data-activity-chart]').attributes('aria-label')).not.toMatch(/messages\/s/i)
  })

  // meta.activity_window is a Go duration string ("30m0s") -- correct on the
  // wire, where it is the value the query ran with, and an implementation
  // detail on a figcaption. The aria-label carries it too, so unformatted it
  // is read out loud as well as shown. The test above pinned the caption's
  // wording without ever reading the window substring inside it.
  it('writes the window as a person reads it, in the caption and in the aria-label', () => {
    collectorsRef.value = envelope([makeCollector()])
    const w = mount(CollectorsView)
    const c = card(w, 'dev-c1')

    const axis = c.get('[data-activity-axis]').text()
    expect(axis).toContain('30 min')
    expect(axis).not.toContain('30m0s')

    const label = c.get('[data-activity-chart]').attributes('aria-label')!
    expect(label).toContain('30 min')
    expect(label).not.toContain('30m0s')
  })

  // --- The chart draws what the series says ---

  // Every other fixture here is all zeros, so this is the only place
  // barHeight, barWidth, the `.current` class and the per-bucket <title>
  // are exercised at all.
  it('scales every bar against the busiest bucket and marks the last one as still accumulating', () => {
    collectorsRef.value = envelope([makeCollector({ activity: busyActivity() })])
    const w = mount(CollectorsView)
    const bars = card(w, 'dev-c1').findAll('[data-activity-chart] rect')
    expect(bars).toHaveLength(BUSY_ROWS.length)

    // Widths and x positions: five equal one-minute slots across the 0..100
    // viewBox, each bar inset by 0.4 from its own slot.
    const slot = 100 / BUSY_ROWS.length
    bars.forEach((bar, i) => {
      expect(Number(bar.attributes('x'))).toBeCloseTo(i * slot, 5)
      expect(Number(bar.attributes('width'))).toBeCloseTo(slot - 0.4, 5)
    })

    // Heights, against the PEAK (40 rows, the middle bucket) and not
    // against the first or last: the tallest bar fills the 0..40 viewBox
    // and every other one is its own share of that.
    const peak = Math.max(...BUSY_ROWS)
    bars.forEach((bar, i) => {
      const want = Math.max((BUSY_ROWS[i] / peak) * 40, BUSY_ROWS[i] > 0 ? 1 : 0.5)
      expect(Number(bar.attributes('height'))).toBeCloseTo(want, 5)
      // The bar hangs from the baseline: its top edge plus its height is
      // the floor of the viewBox, always.
      expect(Number(bar.attributes('y')) + Number(bar.attributes('height'))).toBeCloseTo(40, 5)
    })
    expect(Number(bars[2].attributes('height'))).toBeCloseTo(40, 5)

    // The last bucket is the current, still-accumulating minute and is
    // drawn as such; no earlier one is.
    expect(bars[bars.length - 1].classes()).toContain('current')
    for (const bar of bars.slice(0, -1)) {
      expect(bar.classes()).not.toContain('current')
    }

    // One <title> per bucket, carrying that bucket's own row count, and the
    // last one saying it is not finished.
    const titles = card(w, 'dev-c1').findAll('[data-activity-chart] title')
    expect(titles).toHaveLength(BUSY_ROWS.length)
    // "40 rows", not a bare "40": the title also carries a formatted clock,
    // and a digit pair can turn up in one of those by accident.
    expect(titles[1].text()).toMatch(/\b12 rows\b/)
    expect(titles[2].text()).toMatch(/\b40 rows\b/)
    expect(titles[2].text()).not.toMatch(/still accumulating/i)
    expect(titles[titles.length - 1].text()).toMatch(/still accumulating/i)
  })

  // The API works hardest at exactly this case -- a collector with no key at
  // all in the activity map gets a full-length all-zero series rather than a
  // short one or no card -- and the chart then drew nothing: barHeight 0 put
  // the rect at y in [40, 40.5], below the 0..40 viewBox and clipped away.
  it('draws a visible baseline for a collector that archived nothing all window', () => {
    collectorsRef.value = envelope([makeCollector()])
    const w = mount(CollectorsView)
    const bars = card(w, 'dev-c1').findAll('[data-activity-chart] rect')
    expect(bars.length).toBeGreaterThan(0)

    for (const bar of bars) {
      const y = Number(bar.attributes('y'))
      const height = Number(bar.attributes('height'))
      expect(height).toBeGreaterThan(0)
      // Inside the viewBox, not below it: a rect starting at y=40 is
      // clipped away entirely however tall it claims to be.
      expect(y).toBeLessThan(40)
      expect(y + height).toBeCloseTo(40, 5)
    }
  })

  // --- Additional coverage found on a later read of this file ---

  // The badge rule is separate from the eight named tests above, and
  // deserves its own direct check: a badge whose rule is
  // invisible is a badge nobody can act on.
  it("states each badge's threshold in words, not just its color", () => {
    collectorsRef.value = envelope([makeCollector({ collector: 'healthy-one' })])
    const w = mount(CollectorsView)
    const c = card(w, 'healthy-one')
    expect(c.get('[data-badge]').text()).toMatch(/healthy/i)
    expect(c.get('[data-threshold]').text().length).toBeGreaterThan(0)
  })

  // The fourth union state -- configured only, not yet in the archive --
  // appears in none of the eight named tests above; without this the union
  // table's four rows would be three states tested and one assumed.
  it('answers for a collector the daemon reaches but the archive has never recorded, without inventing routers', () => {
    collectorsRef.value = envelope([makeCollector({ archive: null })])
    const w = mount(CollectorsView)
    const c = card(w, 'dev-c1')

    expect(c.find('[data-tile="peers"]').exists()).toBe(false)
    expect(c.find('[data-tile="liveness"]').exists()).toBe(false)
    // The sentence, not just its presence: this row IS answering, and the
    // wording has to say so -- the same paragraph must read differently on
    // the unreachable row asserted above, or one of the two is lying.
    const note = c.get('[data-not-archived]').text()
    expect(note).toMatch(/and answering/i)
    expect(note).not.toMatch(/did not answer/i)
    // The status side is real and present regardless.
    expect(c.find('[data-tile="uptime"]').exists()).toBe(true)
  })

  // --- Two collector counts sit within a header of each other ---

  // FleetChrome.vue renders "N collectors" in the app chrome from
  // /v1/routers (distinct collectors with at least one router); this screen
  // renders the union /v1/collectors returns. They disagree exactly when a
  // configured collector has no archive record -- the state this screen
  // exists to surface -- so this one has to say which population it counts
  // rather than repeating the bare word both of them would use.
  it('says which population its header count covers', () => {
    collectorsRef.value = envelope([
      makeCollector({ collector: 'in-the-archive' }),
      makeCollector({ collector: 'configured-only', archive: null }),
    ])
    const w = mount(CollectorsView)
    const count = w.get('[data-count-collectors]')

    expect(count.text()).toContain('2')
    expect(count.text()).toMatch(/archived or configured/i)
  })
})

it('names a router that sent no sysName instead of leaving the cell blank', async () => {
  // The case that found this: one router on a local dev deployment is
  // fed by a capture carrying no Initiation, so it reports an empty
  // sysname and this table rendered an empty Router cell beside a
  // populated Address.
  collectorsRef.value = envelope([
    makeCollector({
      archive: {
        routers: [{ ...busyRouter, sysname: '' }],
        peers_up: 1,
        peers_down: 0,
        peers_view_lost: 0,
        peers_stale: 0,
        last_beat_at: T0,
        started_at: '2026-09-18T10:00:00.000Z',
        last_row_at: T0,
      },
    }),
  ])
  const w = mount(CollectorsView)
  await nextTick()
  expect(w.text()).toContain('(no sysName TLV)')
})

// On a 390px phone the card is narrower than its 420px desktop minimum, and
// its router table shrank until every address read "172.2...". The floor
// makes it scroll inside the card instead. A desktop card is at least 420px,
// so its table is at least 386px (420 less 16px padding and a 1px border
// each side), and the floor must not exceed that or it would bind there.
it("floors the card's router table without narrowing it on a desktop", () => {
  collectorsRef.value = envelope([makeCollector()])
  const w = mount(CollectorsView)
  const floor = tableMinWidthPx(card(w, 'dev-c1') as never)
  expect(floor).toBeDefined()
  expect(floor!).toBeLessThanOrEqual(386)
})
