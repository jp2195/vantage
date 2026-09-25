import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import ChurnChart from './ChurnChart.vue'
import type { ChurnBucket } from '@/api/generated'

const buckets: ChurnBucket[] = [
  { ts: '2026-09-17T14:55:00Z', dump: 100, readvertise: 50, withdraw: 10 },
  { ts: '2026-09-17T15:00:00Z', dump: 0, readvertise: 20, withdraw: 5 },
]

// The window the fixture's two buckets sit inside: 15 minutes, which makes
// each 5-minute bar a third of the axis and keeps the arithmetic readable.
const FROM = '2026-09-17T14:50:00Z'
const TO = '2026-09-17T15:05:00Z'

function chart(rows: ChurnBucket[] = buckets) {
  return mount(ChurnChart, {
    props: { buckets: rows, bucketLabel: '5m0s', from: FROM, to: TO },
  })
}

describe('ChurnChart', () => {
  it('draws one bar per bucket, and a segment only for a series with rows', () => {
    const w = chart()
    const bars = w.findAll('[data-bar]')
    expect(bars).toHaveLength(2)
    // The second bucket archived no dumps, so it has no dump segment at all
    // -- a zero-height rect would still be a mark claiming a value.
    expect(bars[0].findAll('rect')).toHaveLength(3)
    expect(bars[1].findAll('rect')).toHaveLength(2)
    expect(bars[1].find('[data-series="dump"]').exists()).toBe(false)
  })

  // The bars are the quantity. A chart whose heights did not track its
  // numbers would be decoration, and this is the assertion that keeps the
  // scaling honest when someone changes the layout maths.
  it('scales segment heights to the values, against the tallest stack', () => {
    const w = chart()
    const h = (i: number, series: string) =>
      Number(w.findAll('[data-bar]')[i].find(`[data-series="${series}"]`).attributes('height'))

    // Bucket 0 totals 160 and is the peak; bucket 1's 20 re-advertisements
    // must therefore be about a fifth of bucket 0's 100 dumps.
    const ratio = h(1, 'readvertise') / h(0, 'dump')
    expect(ratio).toBeGreaterThan(0.15)
    expect(ratio).toBeLessThan(0.25)
  })

  // Identity is never carried by color alone: three series, three names,
  // always present.
  it('names every series in the legend', () => {
    const text = chart().find('.legend').text()
    for (const label of ['Session dumps', 'Re-advertisements', 'Withdrawals']) {
      expect(text).toContain(label)
    }
  })

  // The relief the palette validator's contrast WARN obligates, and the
  // accessible view of the same numbers.
  it('offers a table of the same numbers, exactly', () => {
    const w = chart()
    expect(w.find('[data-table]').exists()).toBe(false)
    return w
      .find('.toggle')
      .trigger('click')
      .then(() => {
        const rows = w.findAll('[data-table] tbody tr')
        expect(rows).toHaveLength(buckets.length)
        expect(rows[0].text()).toContain('100')
        expect(rows[0].text()).toContain('50')
        expect(rows[0].text()).toContain('10')
        expect(w.find('[data-chart]').exists()).toBe(false)
      })
  })

  // The width is the chart's own claim about what a bar MEANS, and it comes
  // from the answer rather than from a default this component assumes.
  it('states the bucket width it was given', () => {
    expect(chart().text()).toContain('5m0s')
  })

  it('says an empty window is an answer rather than drawing an empty frame', () => {
    const w = chart([])
    expect(w.find('[data-chart]').exists()).toBe(false)
    expect(w.find('[data-empty]').text()).toMatch(/nothing was archived/i)
  })
  // The bug the browser caught and no unit test could have: with
  // preserveAspectRatio="none" and only a width, the browser derives the
  // height from the viewBox ratio -- 100x120 inside a 1900px panel is a
  // 2,280px-tall chart, which is what the first render drew. jsdom computes
  // no layout, so what is asserted is the mechanism: the element carries its
  // own height.
  it('fixes its own height in pixels rather than letting the viewBox ratio set it', () => {
    const w = mount(ChurnChart, {
      props: { buckets, bucketLabel: '5m0s', from: FROM, to: TO, height: 90 },
    })
    expect(w.find('[data-chart]').attributes('style')).toContain('height: 90px')
  })
  // The bug the browser found: /v1/collection/churn returns only buckets
  // that HELD rows, so laying bars out by index drew one quiet 30-minute
  // bucket as a block filling 24 hours. Bars sit on a TIME axis, so a gap
  // in the series is a gap on screen -- which is what a bucket with nothing
  // archived in it looks like.
  it('places each bar at its own time, not at its index in the series', () => {
    const w = chart()
    const xs = w.findAll('[data-bar] rect').map((r) => Number(r.attributes('x')))
    // 14:55 is a third of the way through 14:50-15:05; 15:00 is two thirds.
    expect(Math.round(xs[0])).toBe(33)
    expect(Math.round(xs[xs.length - 1])).toBe(67)
  })

  it('gives a bar the width of its own bucket, not a share of the series', () => {
    // A 5-minute bucket in a 15-minute window is a third of the axis,
    // however many buckets came back.
    const w = chart([buckets[0]])
    expect(Math.round(Number(w.find('[data-bar] rect').attributes('width')))).toBe(33)
  })

  // A short bucket inside a long window would otherwise render sub-pixel. A
  // mark too small to see is worse than one slightly wider than its true
  // duration, and the real width is named beside the chart either way.
  it('keeps a narrow bucket visible in a wide window', () => {
    const w = mount(ChurnChart, {
      props: {
        buckets: [buckets[0]],
        bucketLabel: '5m0s',
        from: '2026-09-16T14:55:00Z',
        to: '2026-09-17T14:55:00Z',
      },
    })
    expect(Number(w.find('[data-bar] rect').attributes('width'))).toBeGreaterThanOrEqual(0.35)
  })

  // ------------------------------------------------------------------
  // The x axis. Without it the bars are drawn on empty space: a bar's
  // position encodes a time and nothing on screen says which, so the chart
  // can be read for shape and never for when.
  // ------------------------------------------------------------------

  /** A tick's position on the axis, as the percentage its style carries. */
  function tickAt(el: { attributes(name: string): string | undefined }): number {
    const left = /left:\s*([\d.]+)%/.exec(el.attributes('style') ?? '')
    return left ? Number(left[1]) : NaN
  }

  function axis(props: { from: string; to: string }) {
    const w = mount(ChurnChart, {
      props: { buckets, bucketLabel: '5m0s', ...props },
    })
    return w
      .findAll('[data-tick]')
      .map((t) => ({ label: t.text(), x: tickAt(t), classes: t.classes() }))
  }

  // Wall-clock gridlines, placed at their own instant on the same time axis
  // the bars are placed on -- not spread evenly, and not one per bucket.
  it('labels the x axis with wall-clock times at their own positions', () => {
    const ticks = axis({ from: '2026-09-17T13:20:00Z', to: '2026-09-17T19:20:00Z' })
    // The test clock is America/New_York (vite.config.ts), so 14:00Z is
    // 10:00 on the axis: what a gridline names is the operator's own wall
    // clock, not the instant's UTC rendering.
    expect(ticks.filter((t) => t.label !== 'now').map((t) => t.label)).toEqual([
      '10:00',
      '12:00',
      '14:00',
    ])
    // 14:00Z is 40 minutes into a 360-minute window, 16:00Z is 160, 18:00Z 280.
    expect(ticks.slice(0, 3).map((t) => Math.round(t.x))).toEqual([11, 44, 78])
  })

  // Two branches in one window, and neither is reachable from the test
  // above. The STEP is chosen from the span, so a day-long window gets
  // six-hour gridlines rather than twenty-four hourly ones; and the walk
  // starts from the window's own LOCAL midnight rather than from the epoch,
  // which is only visible from a zone with an offset. This window opens at
  // 20:00 local, so epoch-aligned gridlines would read 20:00/02:00/08:00/
  // 14:00 at 0/25/50/75%, and midnight-aligned ones read the round hours
  // below, shifted four hours in.
  it('coarsens the gridline step as the window widens, and lands on local hours', () => {
    const ticks = axis({ from: '2026-09-17T00:00:00Z', to: '2026-09-18T00:00:00Z' })
    expect(ticks.filter((t) => t.label !== 'now').map((t) => t.label)).toEqual([
      '00:00',
      '06:00',
      '12:00',
      '18:00',
    ])
    expect(ticks.slice(0, 4).map((t) => Math.round(t.x))).toEqual([17, 42, 67, 92])
    // Interior gridlines straddle the instant they name; only the edges are
    // pulled inward, and none of these four is at an edge.
    expect(ticks[0].classes).toContain('middle')
  })

  // The right edge is the browser's own clock -- windowStart/windowEnd are
  // computed from `new Date()`, not from anything the daemon sent, and
  // /v1/collection/churn's meta carries no window bounds to use instead. So
  // the edge is labeled by what it is. Rendering "19:20" there would dress
  // the browser's clock as a collector timestamp, which is the same claim
  // the chrome's stamp already declines to make (FleetChrome.test.ts).
  it('names the right edge "now" rather than stamping the browser clock on it', () => {
    const ticks = axis({ from: '2026-09-17T13:20:00Z', to: '2026-09-17T19:20:00Z' })
    const last = ticks[ticks.length - 1]
    expect(last.label).toBe('now')
    expect(last.x).toBe(100)
    // And pulled back onto the axis rather than centered on 100%, which
    // would hang half the word off the panel's right edge.
    expect(last.classes).toContain('end')
  })

  // A gridline that lands under the right-edge label would print two times
  // on top of each other. 18:00 is 280 of this window's 285 minutes.
  it('drops a gridline that would collide with the right-edge label', () => {
    const ticks = axis({ from: '2026-09-17T13:20:00Z', to: '2026-09-17T18:05:00Z' })
    expect(ticks.map((t) => t.label)).toEqual(['10:00', '12:00', 'now'])
  })

  // Gridlines are walked on the LOCAL CALENDAR, not by adding milliseconds.
  //
  // The walk starts at local midnight to land on wall-clock hours, then used
  // to advance by a fixed number of ms -- which is a different thing the
  // moment a window crosses a DST transition. This window opens at 00:00 EDT
  // on the day the clocks go back and closes at 12:00 EST: 13 absolute hours
  // for 12 on the wall. Adding 6h of milliseconds twice lands on 05:00 and
  // 11:00; the design promises "a wall-clock boundary an operator
  // recognizes -- 14:00, not 14:07", and 05:00 is not one when the step is
  // six hours.
  //
  // The test clock is pinned to America/New_York precisely so a case like
  // this is reachable; vite.config.ts notes the fixtures are September and
  // clear of both transitions, which is why nothing caught it until later
  // manual testing did.
  it('walks gridlines on the local calendar, so a DST change does not shift them', () => {
    const ticks = axis({ from: '2026-11-01T04:00:00Z', to: '2026-11-01T17:00:00Z' })
    expect(ticks.map((t) => t.label)).toEqual(['00:00', '06:00', 'now'])
  })

  // The left edge has the same problem as the right and the same remedy. A
  // window that opens exactly on a six-hour boundary puts its first
  // gridline at 0%, where a label straddling the instant hangs half of
  // itself off the panel.
  it('pulls a gridline sitting on the left edge inward', () => {
    const ticks = axis({ from: '2026-09-17T04:00:00Z', to: '2026-09-18T04:00:00Z' })
    expect(ticks[0].label).toBe('00:00')
    expect(ticks[0].x).toBe(0)
    expect(ticks[0].classes).toContain('start')
  })

  // The floor the heights are read against, and the mark that makes a gap
  // between bars read as a quiet bucket rather than as the edge of the plot.
  it('draws a baseline along the foot of the plot', () => {
    const w = chart()
    const base = w.get('[data-baseline]')
    expect(Number(base.attributes('x1'))).toBe(0)
    expect(Number(base.attributes('x2'))).toBe(100)
    // Both ends at the same height, at the foot of the 120-unit plot.
    expect(base.attributes('y1')).toBe(base.attributes('y2'))
    expect(Number(base.attributes('y1'))).toBeGreaterThan(118)
  })

  // An axis under a sentence saying nothing was archived would be a frame
  // around an empty claim, and one under the table would label a table.
  it('draws no axis when there is no chart', async () => {
    expect(chart([]).find('[data-tick]').exists()).toBe(false)
    const w = chart()
    await w.find('.toggle').trigger('click')
    expect(w.find('[data-tick]').exists()).toBe(false)
    expect(w.find('[data-baseline]').exists()).toBe(false)
  })
})
