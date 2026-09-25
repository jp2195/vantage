<script setup lang="ts">
/**
 * What the archive received over time, as stacked bars, one per time
 * bucket, split into the three kinds /v1/collection/churn separates.
 *
 * # Form
 *
 * Stacked bars, not lines and not grouped bars. The three series are
 * mutually exclusive parts of one total -- every archived row is exactly one
 * of them -- so the stack's height is a real quantity (rows archived in that
 * bucket) rather than an artifact of stacking unrelated measures. One axis,
 * one unit, always: rows.
 *
 * # Color
 *
 * Three tokens from this project's own design system, which is deliberately
 * near-monochrome -- ink, one gray, one amber -- and the palette validator
 * was run on them rather than reasoned about:
 *
 *     node scripts/validate_palette.js "#16181d,#666d7d,#e8b53e" --mode light
 *     [PASS] CVD separation      worst adjacent ΔE 28.8 (protan) · 29.8 (tritan)
 *     [PASS] Normal-vision floor worst adjacent ΔE 31.4
 *     [FAIL] Lightness band      #16181d 0.209, #e8b53e 0.799 outside 0.43-0.77
 *     [FAIL] Chroma floor        #16181d, #666d7d read as gray
 *     [WARN] Contrast vs surface #e8b53e 1.84:1 -- relief required
 *
 * The two FAILs are the generic rules meeting a monochrome design system and
 * are accepted deliberately: they measure whether marks read as colorful,
 * while the checks that measure whether marks can be TOLD APART -- the CVD
 * and normal-vision separations, which are the ones a colorblind reader
 * depends on -- pass with a wide margin. Every alternative drawn from these
 * tokens fails the same two (a greener or redder third series was tried and
 * measured worse: #b1451f against #8a6a12 is ΔE 0.6 under deuteranopia).
 *
 * The WARN is not dismissable and is paid for rather than argued with: the
 * amber series is named in the legend,every bar carries its own numbers in a
 * title, and a table view of the same data is one click away.
 *
 * COLOR HERE IS IDENTITY, NOT SEVERITY. Amber marks withdrawals because that
 * is this project's own choice for them, not because a withdrawal is a
 * warning; gray marks session dumps because they are the recessive bulk, not
 * because a dump is unimportant. A DUMP IS NOT A FAILURE and a withdrawal is
 * not an error -- the same rule Monitor's own panels hold.
 */
import { computed, ref } from 'vue'
import { formatCount } from '@/lib/formatCount'
import { formatTimeOfDay } from '@/lib/formatClock'
import { parseGoDuration } from '@/lib/goDuration'
import type { ChurnBucket } from '@/api/generated'

const props = defineProps<{
  buckets: ChurnBucket[]
  /** The width every bar was computed at -- meta.churn_bucket, never assumed. */
  bucketLabel: string
  /**
   * The window the chart covers, as ISO instants. Bars are placed on a TIME
   * axis between them, not spread evenly across however many buckets came
   * back -- /v1/collection/churn returns only buckets that HELD rows, so an
   * index axis drew a single quiet 30-minute bucket as a block filling 24
   * hours. The gaps are the answer.
   */
  from: string
  to: string
  height?: number
}>()

const showTable = ref(false)

// Series in stacking order, bottom first: the bulk at the bottom, the
// smallest and brightest on top where it stays visible at any scale.
const SERIES = [
  { key: 'dump', label: 'Session dumps', color: 'var(--muted)' },
  { key: 'readvertise', label: 'Re-advertisements', color: 'var(--ink)' },
  { key: 'withdraw', label: 'Withdrawals', color: 'var(--accent)' },
] as const

const H = computed(() => props.height ?? 120)
const totals = computed(() => props.buckets.map((b) => b.dump + b.readvertise + b.withdraw))
const peak = computed(() => Math.max(1, ...totals.value))

/**
 * Bars laid out in a 0..100 viewBox on x so the SVG scales to its container
 * without JavaScript measuring anything; y is in real pixels, so the mark
 * specs below (the 2px gap, the 4px corner) keep their stated sizes instead
 * of being stretched by the aspect ratio.
 */
const span = computed(() => {
  const a = Date.parse(props.from)
  const b = Date.parse(props.to)
  return Number.isFinite(a) && Number.isFinite(b) && b > a ? { a, b, ms: b - a } : undefined
})

/**
 * One bar's width in viewBox units: its own bucket duration as a share of
 * the window. A floor of 0.35 keeps a short bucket in a long window from
 * rendering sub-pixel -- a mark too small to see is worse than one slightly
 * wider than its true duration, and the label beside the chart names the
 * real width.
 */
const barWidth = computed(() => {
  const sp = span.value
  const ms = parseGoDuration(props.bucketLabel)
  if (!sp || !ms) return 1.5
  return Math.max((ms / sp.ms) * 100, 0.35)
})

const layout = computed(() => {
  const n = props.buckets.length
  const sp = span.value
  if (!n || !sp) return []
  const w = barWidth.value
  return props.buckets.map((b) => {
    // Placed at its own timestamp, so a gap in the series is visible as a
    // gap -- which is what a bucket with nothing archived in it looks like.
    const t = Date.parse(b.ts)
    const x = Math.min(Math.max(((t - sp.a) / sp.ms) * 100, 0), 100 - w)
    const scale = (v: number) => (v / peak.value) * H.value
    const segs: { key: string; color: string; y: number; h: number; value: number }[] = []
    let acc = 0
    for (const s of SERIES) {
      const v = b[s.key]
      if (v > 0) {
        const h = scale(v)
        // 2px of surface between stacked segments, the spacer the mark
        // specs ask for -- subtracted from the segment rather than added
        // to the stack, so the total height stays the real quantity.
        const drawn = Math.max(h - 2, 0.5)
        segs.push({ key: s.key, color: s.color, y: H.value - acc - h, h: drawn, value: v })
      }
      acc += scale(v)
    }
    return { x, w, segs, bucket: b, total: b.dump + b.readvertise + b.withdraw }
  })
})

/**
 * Gridline steps, coarsest last. Every one divides 24 hours, which is what
 * lets a tick land on a wall-clock boundary an operator recognizes -- 14:00,
 * not 14:07 -- once the walk below starts from midnight.
 */
const TICK_STEPS_MS = [
  60_000, 5 * 60_000, 15 * 60_000, 30 * 60_000,
  3_600_000, 2 * 3_600_000, 3 * 3_600_000, 6 * 3_600_000, 12 * 3_600_000, 24 * 3_600_000,
]

/**
 * A gridline's label: hours and minutes on a 24-hour clock, which is
 * DELIBERATELY not the 12-hour rendering `formatTimeOfDay` gives a bar's
 * title in an en-US locale. A title is one instant read on purpose and has
 * room for "2:55:00 PM"; an axis is four labels read at a glance under bars
 * a few pixels wide, where "02:00 PM" is nearly twice the width of the
 * number it carries. This project draws the same 24-hour scale elsewhere
 * too, and so does every log this archive is read beside.
 */
function tickLabel(t: number): string {
  return new Date(t).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  })
}

/**
 * The x axis: wall-clock gridlines on the same time axis the bars sit on,
 * so a bar's position can be read as a time rather than only as a shape.
 *
 * THE RIGHT EDGE IS NAMED "now", NOT STAMPED WITH A CLOCK -- and the reason
 * has CHANGED, so read this rather than assuming the old one.
 *
 * It used to be that `from` and `to` were computed by MonitorView from
 * `new Date()`, because /v1/collection/churn's meta carried the bucket width
 * and no window bounds. That was worse than an unlabeled edge: the bars were
 * PLACED against the browser's clock while every `ts` here is a
 * ts_collector, so a laptop a few minutes off shifted every bar against its
 * own gridline. The daemon sends `meta.churn_from` / `meta.churn_to` as of
 * 2026-09-21 and the axis is drawn between those.
 *
 * So the bounds are now server-side, and a real time COULD be printed here.
 * It still is not, because the label answers a different question than the
 * gridlines do: a gridline says "this position is 16:00", which the operator
 * reads against the bars, while an edge stamped 19:20 invites them to read
 * it against their own watch -- and the daemon's clock is not theirs. The
 * gridlines are where the precision belongs; the edge only has to say which
 * end is recent.
 */
const ticks = computed(() => {
  const sp = span.value
  if (!sp) return []
  // At most four intervals, so a 24-hour window gets six-hour gridlines
  // rather than twenty-four hourly ones crushed into a strip.
  const step = TICK_STEPS_MS.find((ms) => sp.ms / ms <= 4) ?? TICK_STEPS_MS[TICK_STEPS_MS.length - 1]
  // Walked from the LOCAL midnight the window opens in rather than from the
  // epoch: the epoch is UTC midnight, so an epoch-aligned six-hour tick
  // reads 02:00 in a zone offset by two hours.
  const midnight = new Date(sp.a)
  midnight.setHours(0, 0, 0, 0)
  const origin = midnight.getTime()

  // Walked on the LOCAL CALENDAR, never by adding milliseconds. The origin
  // is local midnight so a tick lands on a wall-clock hour -- and adding a
  // fixed 6h of ms across a DST change lands on 05:00 instead, which is not
  // one. `setHours(getHours() + n)` asks the calendar for "six hours later
  // on the clock", which is the thing this axis actually promises.
  const cursor = new Date(origin)
  const stepHours = step / 3_600_000
  while (cursor.getTime() < sp.a) {
    if (stepHours >= 1) cursor.setHours(cursor.getHours() + stepHours)
    else cursor.setTime(cursor.getTime() + step)
  }

  const out: { key: string; label: string; x: number; align: string }[] = []
  for (; cursor.getTime() <= sp.b; ) {
    const t = cursor.getTime()
    if (stepHours >= 1) cursor.setHours(cursor.getHours() + stepHours)
    else cursor.setTime(cursor.getTime() + step)
    const x = ((t - sp.a) / sp.ms) * 100
    // A gridline this close to the right edge would print underneath the
    // "now" label. 6% is 72px on the 1200px this panel gets on a wide
    // display, against a ~32px label centered on its tick and a ~25px
    // right-aligned one -- so the two clear each other with room left.
    if (x > 94) continue
    out.push({ key: String(t), label: tickLabel(t), x, align: x < 6 ? 'start' : 'middle' })
  }
  out.push({ key: 'now', label: 'now', x: 100, align: 'end' })
  return out
})

/** One bar's own numbers, for its title and for the table view. */
function describe(b: ChurnBucket): string {
  return (
    `${formatTimeOfDay(b.ts)} · ${formatCount(b.dump)} dumps · ` +
    `${formatCount(b.readvertise)} re-advertisements · ${formatCount(b.withdraw)} withdrawals`
  )
}
</script>

<template>
  <figure class="chart">
    <figcaption class="cap">
      <span class="legend">
        <!-- Always present: with three series, identity can never be carried
             by color alone. -->
        <span v-for="s in SERIES" :key="s.key" class="key">
          <span class="swatch" :style="{ background: s.color }" />{{ s.label }}
        </span>
      </span>
      <span class="controls">
        <span class="bucket">{{ bucketLabel }} buckets</span>
        <button type="button" class="toggle" @click="showTable = !showTable">
          {{ showTable ? 'Chart' : 'Table' }}
        </button>
      </span>
    </figcaption>

    <p v-if="!buckets.length" class="quiet" data-empty>
      Nothing was archived in this window, so there is nothing to draw. An
      empty chart is an answer — no bar is a bucket with no rows, not a
      missing feed.
    </p>

    <!-- The height is set HERE, in pixels, not left to the viewBox. With
         `preserveAspectRatio="none"` and only a width, the browser derives
         height from the viewBox ratio: at 100x120 in a 1900px panel that is
         a 2,280px-tall chart, which is what the first render drew. The x
         axis is the only one allowed to stretch. -->
    <svg
      v-else-if="!showTable"
      class="plot"
      :viewBox="`0 0 100 ${H}`"
      :style="{ height: `${H}px` }"
      preserveAspectRatio="none"
      role="img"
      :aria-label="`Rows archived per ${bucketLabel} bucket, split into session dumps, re-advertisements and withdrawals. Peak ${formatCount(peak)} rows.`"
      data-chart
    >
      <!-- Drawn FIRST, so the bars paint over it: the baseline is the floor
           the heights are read against, and what it has to show is the gaps
           between bars -- a bucket that archived nothing sitting on the
           axis rather than falling off the bottom of an empty frame. -->
      <line
        data-baseline
        x1="0"
        x2="100"
        :y1="H - 0.5"
        :y2="H - 0.5"
        stroke="var(--line)"
        stroke-width="1"
        vector-effect="non-scaling-stroke"
        shape-rendering="crispEdges"
      />
      <g v-for="(bar, i) in layout" :key="i" data-bar>
        <title>{{ describe(bar.bucket) }}</title>
        <rect
          v-for="seg in bar.segs"
          :key="seg.key"
          :data-series="seg.key"
          :x="bar.x"
          :y="seg.y"
          :width="bar.w"
          :height="seg.h"
          :fill="seg.color"
          rx="0.6"
        />
      </g>
    </svg>

    <!-- HTML, not <text> inside the plot: the svg is drawn with
         preserveAspectRatio="none" so its x units stretch to the panel,
         which would stretch a glyph with them. Positioned in percent
         against the same 0..100 axis the bars are placed on. -->
    <div v-if="buckets.length && !showTable" class="axis">
      <span
        v-for="t in ticks"
        :key="t.key"
        class="tick"
        :class="t.align"
        :style="{ left: `${t.x}%` }"
        data-tick
        >{{ t.label }}</span
      >
    </div>

    <!-- The relief the palette validator's contrast WARN obligates, and the
         accessible view of the same numbers: every bar, exactly. -->
    <table v-else class="table" data-table>
      <thead>
        <tr>
          <th>Bucket</th>
          <th class="num">Dumps</th>
          <th class="num">Re-advertised</th>
          <th class="num">Withdrawn</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="(b, i) in buckets" :key="i">
          <td class="mono">{{ formatTimeOfDay(b.ts) }}</td>
          <td class="num mono">{{ formatCount(b.dump) }}</td>
          <td class="num mono">{{ formatCount(b.readvertise) }}</td>
          <td class="num mono">{{ formatCount(b.withdraw) }}</td>
        </tr>
      </tbody>
    </table>
  </figure>
</template>

<style scoped>
.chart { margin: 0; display: flex; flex-direction: column; gap: 6px; }
.cap { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.legend { display: flex; gap: 12px; flex-wrap: wrap; }
.key { display: inline-flex; align-items: center; gap: 5px; color: var(--muted); font: 400 11px var(--font-ui); }
.swatch { width: 9px; height: 9px; border-radius: 2px; }
.controls { display: inline-flex; align-items: center; gap: 10px; }
.bucket { color: var(--muted); font: 400 10.5px var(--font-data); }
.toggle {
  border: 1px solid var(--line); border-radius: 5px; background: var(--surface);
  color: var(--ink-2); font: 500 10.5px var(--font-ui); padding: 2px 8px; cursor: pointer;
}
.toggle:hover { border-color: var(--ink); color: var(--ink); }
.plot { width: 100%; display: block; }
/* height comes from the element's own style binding -- see the template. */
.plot [data-bar]:hover rect { opacity: .78; }
.axis { position: relative; height: 13px; }
.tick { position: absolute; top: 0; white-space: nowrap; color: var(--muted); font: 400 10px var(--font-data); }
/* A tick names the instant it sits on, so its label straddles that point --
   except at the two edges, where straddling would hang half a label off the
   panel. */
.tick.middle { transform: translateX(-50%); }
.tick.end { transform: translateX(-100%); }
.quiet { margin: 0; max-width: 72ch; color: var(--muted); font: 400 11px var(--font-ui); }
.table { width: 100%; border-collapse: collapse; font: 400 11px var(--font-ui); }
.table th {
  text-align: left; padding: 4px 8px; color: var(--muted); background: var(--surface-2);
  font: 600 9.5px var(--font-ui); text-transform: uppercase; letter-spacing: .07em;
}
.table td { padding: 4px 8px; border-bottom: 1px solid var(--line-faint); color: var(--ink-2); }
.num { text-align: right; }
</style>
