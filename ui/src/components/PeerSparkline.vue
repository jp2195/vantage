<script setup lang="ts">
/**
 * One peer's changes over the window, as a row-height sparkline.
 *
 * # Why the bars are placed by TIME
 *
 * `activity` carries one entry per bucket that HELD a change -- quiet
 * buckets are absent, by the endpoint's own contract, so the array's length
 * is a function of the data and not of the window. Spacing the entries
 * evenly would therefore draw a peer that churned twice an hour apart
 * exactly like one that churned twice in a minute, which is the one
 * distinction this mark exists to show.
 *
 * ChurnChart learned the same thing the hard way on the same data: laying
 * bars out by index drew a single quiet 30-minute bucket as a block filling
 * 24 hours. The gaps are the answer.
 *
 * # Why the window comes from the answer
 *
 * `from` and `to` are `meta.churn_from` / `meta.churn_to`, on the daemon's
 * clock -- the same bounds the bar timestamps were computed against. A local
 * subtraction off `new Date()` would place collector timestamps against the
 * browser's clock, which this project has already fixed once on Monitor.
 *
 * # Scale
 *
 * Each row scales to its OWN peak, and that is deliberate: this sits in a
 * table beside the peer's own rate, so what it shows is that peer's SHAPE
 * over time. A shared scale would flatten every quiet peer to nothing to
 * make room for the busiest, and the rate column is already where magnitudes
 * are compared between peers.
 */
import { computed } from 'vue'
import { formatCount } from '@/lib/formatCount'
import { parseGoDuration } from '@/lib/goDuration'
import type { ChurnActivity } from '@/api/generated'

const props = defineProps<{
  activity: ChurnActivity[]
  /** The window the bars are placed in, from the answer's own meta. */
  from?: string | null
  to?: string | null
  /** meta.churn_bucket: how wide one bar's span really is. */
  bucket?: string | null
}>()

/** A 0..100 x 0..14 viewBox: the mark is a table cell, not a chart. */
const H = 14
/**
 * A FLOOR on bar width, so a short bucket in a long window is still a
 * visible mark rather than a sub-pixel sliver. Wider than its true duration
 * is the lesser evil -- a mark too small to see says nothing at all -- and
 * the same trade ChurnChart's own bars make.
 *
 * It was the width itself until testing caught that: a constant 2.5
 * against a true 4.17 for a 24-bucket window, so every bar was NARROWER
 * than the span it claimed and adjacent buckets drew with a gap the data
 * does not contain. The real width comes from meta.churn_bucket, which this
 * path sends and which was being discarded.
 */
const MIN_W = 2.5

const span = computed(() => {
  const a = Date.parse(props.from ?? '')
  const b = Date.parse(props.to ?? '')
  return Number.isFinite(a) && Number.isFinite(b) && b > a ? { a, ms: b - a } : undefined
})

const peak = computed(() => Math.max(1, ...props.activity.map((e) => e.changes)))

/**
 * One bar's width: its own bucket's duration as a share of the window, the
 * way ChurnChart computes its own. Falls back to the floor when the bucket
 * is absent -- a bar of an unknown span is still better placed than not
 * drawn, and the placement is what this mark is for.
 */
const barW = computed(() => {
  const sp = span.value
  const ms = parseGoDuration(props.bucket ?? '')
  if (!sp || !ms) return MIN_W
  return Math.max((ms / sp.ms) * 100, MIN_W)
})

/**
 * Whether the bars can be placed at all -- which is a DIFFERENT answer from
 * "this peer changed nothing", and used to render as the same dash.
 *
 * The bars sit inside meta.churn_from/churn_to, so without those there is
 * nowhere to put them. A peer with real changes would then have drawn
 * exactly like a quiet one, beside a rate column showing a nonzero number.
 * Same distinction archivedRate draws by returning undefined rather than 0.
 */
const placeable = computed(() => span.value !== undefined)

const bars = computed(() => {
  const sp = span.value
  if (!sp || !props.activity.length) return []
  const w = barW.value
  return props.activity.map((e) => {
    const t = Date.parse(e.bucket)
    const x = Math.min(Math.max(((t - sp.a) / sp.ms) * 100, 0), 100 - w)
    const h = Math.max((e.changes / peak.value) * H, 1)
    return { x, w, y: H - h, h, changes: e.changes, bucket: e.bucket }
  })
})

const label = computed(() =>
  props.activity.length
    ? `Changes over the window, peaking at ${formatCount(peak.value)} in one bucket.`
    : 'No changes in this window.',
)
</script>

<template>
  <!-- Nothing drawn at all when the peer changed nothing: an empty frame
       under a column of real marks reads as a measurement of zero shape,
       where the honest answer is that there is nothing to draw. The rate
       column beside it already says the number. -->
  <svg
    v-if="bars.length"
    class="spark"
    data-spark
    :viewBox="`0 0 100 ${H}`"
    preserveAspectRatio="none"
    role="img"
    :aria-label="label"
  >
    <rect
      v-for="(b, i) in bars"
      :key="i"
      :x="b.x"
      :y="b.y"
      :width="b.w"
      :height="b.h"
      fill="var(--ink)"
    >
      <title>{{ formatCount(b.changes) }} changes</title>
    </rect>
  </svg>
  <!-- Three states, not two. No axis is not "no changes": the first is the
       client unable to place a real series, the second is a measurement. -->
  <span v-else-if="!placeable" class="quiet" data-spark-unplaceable title="This answer carried no window, so these bars cannot be placed in time.">?</span>
  <span v-else class="quiet" data-spark-empty>—</span>
</template>

<style scoped>
/* Height in px, not from the viewBox ratio: with preserveAspectRatio="none"
   and only a width, the browser derives height from the ratio, which at
   100x14 in a wide column is a very tall cell. ChurnChart's own comment
   records that bug; this is the same shape and the same remedy. */
.spark { width: 100%; height: 14px; display: block; }
.quiet { color: var(--muted); }
</style>
