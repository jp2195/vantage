import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { computed, ref } from 'vue'

// The composable is mocked here and exercised for real in queries.test.ts:
// this file is about what the chrome RENDERS for each of its three states,
// which is a different question from whether the count is derived correctly.
let collectors: number | undefined
let updatedAt: string | undefined
let newestRow: string | undefined
vi.mock('@/api/queries', () => ({
  useFleetChrome: () => ({
    collectors: computed(() => collectors),
    updatedAt: ref(updatedAt),
    newestRow: computed(() => newestRow),
  }),
}))

const FleetChrome = (await import('./FleetChrome.vue')).default

describe('FleetChrome', () => {
  beforeEach(() => {
    collectors = undefined
    updatedAt = undefined
    newestRow = undefined
  })

  it('names the collector count it was given', () => {
    collectors = 3
    const w = mount(FleetChrome)
    expect(w.find('[data-collectors]').text()).toBe('3 collectors')
  })

  it('says "collector" for one, because a fleet of one is the common lab case', () => {
    collectors = 1
    expect(mount(FleetChrome).find('[data-collectors]').text()).toBe('1 collector')
  })

  // Absent is not zero. "0 collectors" is the claim that nothing is
  // answering, which an empty or failed inventory cannot support -- and the
  // chrome is the worst place in the app to state it, since it would be on
  // screen over every other screen's real data.
  it('renders no count at all when the count is unknown', () => {
    const w = mount(FleetChrome)
    expect(w.find('[data-collectors]').exists()).toBe(false)
    expect(w.text()).not.toContain('0')
  })

  // A server clock ("19:24:07 UTC") is not what this renders. This API
  // returns no server time and the browser's own is a different clock
  // from the collector's -- the ts_router versus ts_collector
  // distinction this project audits everywhere. So the stamp is labeled
  // by what it is.
  it('labels the stamp as when the page last updated, never as a clock', () => {
    updatedAt = '2026-09-17T23:24:07.000Z'
    const stamp = mount(FleetChrome).find('[data-updated]')
    expect(stamp.text()).toMatch(/^updated /)
    // Time of day only: the date is always today and the cluster shares one
    // line with a nine-item nav. A month name here was the first render.
    expect(stamp.text()).not.toMatch(/2026|Sep/)
  })

  it('renders no stamp before any answer has arrived', () => {
    expect(mount(FleetChrome).find('[data-updated]').exists()).toBe(false)
  })

  /**
   * Receipt time and archive freshness are two facts, and the chrome showed
   * only the first for four days.
   *
   * "updated 14:03:07" is when this browser got an answer; it reads as how
   * current the data is, which is a different thing entirely. At a quiet
   * fleet's ~21 archived rows a day the two are hours apart, so the
   * stamp could sit at the current second over an archive nobody had
   * written to since morning. Both are labeled, and neither is folded
   * into the other.
   */
  it('names the archive’s newest row beside the receipt stamp', () => {
    updatedAt = '2026-09-21T18:03:07.000Z'
    newestRow = '2026-09-21T13:41:12.000Z'
    const w = mount(FleetChrome)
    expect(w.find('[data-updated]').text()).toMatch(/^updated /)
    expect(w.find('[data-newest]').text()).toMatch(/^newest row /)
    // The two must not render the same instant: that would be the single
    // stamp again, wearing two labels.
    expect(w.find('[data-newest]').text()).not.toContain(
      w.find('[data-updated]').text().replace('updated ', ''),
    )
  })

  /**
   * The case the freshness fact exists for. A row from a previous day must
   * carry its date -- "newest row 09:41" beside "updated 14:03" reads as
   * four hours old when it is really a day and four hours.
   */
  it('keeps the date on a newest row that is not from today', () => {
    updatedAt = '2026-09-21T18:03:07.000Z'
    newestRow = '2026-09-18T13:41:12.000Z'
    expect(mount(FleetChrome).find('[data-newest]').text()).toMatch(/Sep 18/)
  })

  // Absent is not "now", for the same reason absent is not 0 above: an empty
  // or failed inventory cannot support a freshness claim at all.
  it('renders no freshness fact when the archive has no newest row to name', () => {
    updatedAt = '2026-09-21T18:03:07.000Z'
    const w = mount(FleetChrome)
    expect(w.find('[data-updated]').exists()).toBe(true)
    expect(w.find('[data-newest]').exists()).toBe(false)
    expect(w.text()).not.toContain('newest row')
  })
})
