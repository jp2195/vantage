<script setup lang="ts">
/**
 * "Is collection healthy" at the collector grain: one card per id
 * `GET /v1/collectors` returns, which is the UNION of every id the archive
 * has a record for and every id this daemon is configured to poll --
 * api/openapi.yaml's own words for why either side alone is a different
 * wrong answer.
 *
 * Two independent nullable facets per card -- `archive` and `status` -- not
 * one status enum, mirroring the wire shape exactly (api/collectors.go's
 * `WireCollector`). `reachable` is three-valued for the same reason: `null`
 * means no endpoint is configured for this id at all, and that is not the
 * same claim as `false`, which means one IS configured and did not answer.
 * Rendering the two alike would tell an operator "nobody set this up" and
 * "this is down" with the same pixels.
 *
 * ZERO IS NOT ABSENCE. A collector with no configured endpoint has no
 * uptime and no message count to show -- not zero of each, which would
 * read as a collector that has been up for no time and processed nothing,
 * the description of a broken one rather than an unconfigured one. Every
 * status-derived tile below is gated on `collector.status`, not rendered
 * with a placeholder zero when it is absent -- the same rule FleetChrome.vue
 * already follows with `undefined` rather than `0` collectors.
 */
import { computed, ref, watch } from 'vue'
import { routerLabel } from '@/lib/routerLabel'
import DataTable, { type Column } from '@/components/DataTable.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import StatTile from '@/components/StatTile.vue'
import { formatClock } from '@/lib/formatClock'
import { formatCount } from '@/lib/formatCount'
import { formatGoDuration } from '@/lib/goDuration'
import { useCollectors } from '@/api/queries'
import type { Collector, CollectorRouter } from '@/api/generated'

const { data, isPending, error } = useCollectors()

const collectors = computed<Collector[]>(() => data.value?.data ?? [])

/**
 * `meta.activity_window` arrives as a Go duration string ("30m0s") -- the
 * value the query actually ran with, which is the honest thing for the wire
 * to carry and the wrong thing to print. This screen puts it in a figcaption
 * AND in the chart's aria-label, so the unformatted form is read out loud as
 * well as shown. Formatted here rather than on the wire: the contract, and
 * `meta.churn_bucket` beside it, both commit to the Go form.
 */
const activityWindow = computed(() => {
  const w = data.value?.meta?.activity_window
  return w ? formatGoDuration(w) : 'the polled window'
})

/**
 * The count is labeled by the POPULATION it counts, not just "Collectors":
 * this is the union of every id the archive has a record for and every id
 * this daemon is configured to poll, and the app chrome renders its own "N
 * collectors" a header away from it, counting something else entirely
 * (FleetChrome.vue, distinct collectors with at least one router, from
 * /v1/routers). The two disagree exactly when a configured collector has no
 * archive record -- which is the state this screen exists to surface -- so
 * an unlabeled number here reads as one of them contradicting the other.
 */
const counts = computed(() => [
  { key: 'collectors', label: 'Archived or configured', value: formatCount(collectors.value.length) },
])

/**
 * The messages/second rate, which is a DERIVATIVE this response never
 * carries -- `status.observed_at`'s own doc comment: "a single answer is
 * one sample, and a rate needs two." Colada only ever hands this screen the
 * LATEST poll, so the memory of the sample before it has to live here,
 * across ticks of the same mounted component -- not in a computed, which
 * has nothing before `data` changed to compare against.
 *
 * Keyed by collector id and never reset by anything other than a fresh
 * mount, so a screen left open across many 30s polls keeps computing a real
 * rate rather than losing its baseline every time one collector's row
 * happens not to change shape.
 */
interface StatusSample {
  observedAt: string
  bmpMessagesTotal: number
}
const previousSamples = new Map<string, StatusSample>()
const rates = ref(new Map<string, number | undefined>())

/**
 * Two failure modes this must not produce:
 *
 * - No previous sample: this is the first observation, and "0 msg/s" would
 *   describe a dead collector rather than what is actually true (one
 *   sample, no rate derivable from it).
 * - The counter went BACKWARD: `bmp_messages_total` is monotonic within one
 *   process's lifetime, so a lower reading than the one already held means
 *   the process restarted between polls -- a different process, not a
 *   slower one -- and dividing the negative delta would report a negative
 *   rate that describes nothing real.
 *
 * The gap is computed from the two samples' OWN `observed_at` values, never
 * from when this screen happened to receive either response -- the same
 * distinction `ts_router` vs `ts_collector` exists to keep elsewhere in
 * this codebase, applied to the browser's clock instead of a second
 * collector's.
 */
function messageRate(previous: StatusSample | undefined, current: StatusSample): number | undefined {
  if (!previous) return undefined
  const deltaMessages = current.bmpMessagesTotal - previous.bmpMessagesTotal
  if (deltaMessages < 0) return undefined
  const deltaSeconds = (Date.parse(current.observedAt) - Date.parse(previous.observedAt)) / 1000
  if (!(deltaSeconds > 0)) return undefined
  return deltaMessages / deltaSeconds
}

watch(
  data,
  (v) => {
    if (!v) return
    const next = new Map<string, number | undefined>()
    for (const c of v.data) {
      if (!c.status) continue
      const current: StatusSample = {
        observedAt: c.status.observed_at,
        bmpMessagesTotal: c.status.bmp_messages_total,
      }
      next.set(c.collector, messageRate(previousSamples.get(c.collector), current))
      previousSamples.set(c.collector, current)
    }
    rates.value = next
  },
  { immediate: true },
)

/** Two decimals under 10/s, none above -- MonitorView's own Archived/s convention. */
function formatRate(n: number | undefined): string {
  if (n === undefined) return '—'
  return n >= 10 ? formatCount(Math.round(n)) : n.toFixed(2)
}

/**
 * Uptime is `observed_at - started_at`, both on the DAEMON'S OWN clock from
 * the same gather -- unlike the rate above, there is no second sample and
 * no cross-clock question here at all.
 */
function formatUptime(startedAt: string, observedAt: string): string {
  const ms = Date.parse(observedAt) - Date.parse(startedAt)
  if (!(ms >= 0)) return '—'
  const minutes = Math.floor(ms / 60_000)
  const days = Math.floor(minutes / 1440)
  const hours = Math.floor((minutes % 1440) / 60)
  const mins = minutes % 60
  const parts: string[] = []
  if (days) parts.push(`${days}d`)
  if (days || hours) parts.push(`${hours}h`)
  parts.push(`${mins}m`)
  return parts.join(' ')
}

/**
 * "Liveness", never "lag": this project's own decision
 * (2026-09-17) is that `now() - archive.last_row_at` is the one honest end-to-end signal
 * this pipeline can show on a card, and that it CONFLATES an idle collector
 * with a stuck one -- which is exactly why it is labeled by what it
 * measures rather than by the sharper claim ("lag") this pipeline cannot
 * back. There is no per-collector lag field anywhere on this wire; see this
 * screen's own sparkline note and api/openapi.yaml's own words on the path.
 */
function formatLiveness(lastRowAt: string): string {
  const ms = Date.now() - Date.parse(lastRowAt)
  if (!(ms >= 0)) return '—'
  const minutes = Math.floor(ms / 60_000)
  if (minutes < 1) return '<1m ago'
  const hours = Math.floor(minutes / 60)
  const mins = minutes % 60
  return hours ? `${hours}h ${mins}m ago` : `${mins}m ago`
}

interface Badge {
  level: 'healthy' | 'degraded' | 'unconfigured'
  label: string
  threshold: string
}

/**
 * Mirrors `reachable`'s own three values exactly, rather than forcing it
 * into two. `null` is not folded into either HEALTHY or DEGRADED: a
 * collector nobody told this daemon to poll has no health question to
 * answer, and calling that DEGRADED would be the same false claim the rule
 * that zero is not absence exists to prevent, just spoken through a badge
 * instead of a number.
 *
 * `id_mismatch` is deliberately NOT folded in here -- it renders as its own
 * statement below, because it is a fact about IDENTITY, not about whether
 * this endpoint answered. A card can be reachable (this badge says HEALTHY)
 * while also carrying a disagreement worth flagging on its own.
 */
function badgeOf(c: Collector): Badge {
  if (c.reachable === true) {
    return { level: 'healthy', label: 'HEALTHY', threshold: 'Threshold: answered its /status endpoint at the last poll.' }
  }
  if (c.reachable === false) {
    return {
      level: 'degraded',
      label: 'DEGRADED',
      threshold: 'Threshold: an endpoint is configured, but it did not answer the last poll.',
    }
  }
  return {
    level: 'unconfigured',
    label: 'NO STATUS ENDPOINT',
    threshold: 'Threshold: no /status endpoint is configured for this id, so there is no health question to answer.',
  }
}

/**
 * The "no archive record" line, whose WORDING has to follow `reachable` and
 * not just `!archive`. It was a single sentence gated on the archive side
 * alone, and on a redirect counterpart row (no archive record, endpoint
 * configured, did not answer as itself) it read "configured, and answering"
 * directly beneath a DEGRADED badge and an alert saying the endpoint did not
 * answer -- the card contradicting itself in two adjacent paragraphs.
 *
 * All three of `reachable`'s values get their own sentence, including the
 * null one: a row with neither an archive record nor a configured endpoint
 * cannot occur today (the union has no third source to put it there), and
 * wording it anyway costs one line and removes the chance that a future
 * source lands it silently in the wrong sentence.
 */
function notArchivedNote(c: Collector): string {
  if (c.reachable === false) {
    return (
      'No archive record for this id yet, and its endpoint did not answer the last poll -- ' +
      'neither side of the union has anything to show for it.'
    )
  }
  if (c.reachable === null) {
    return (
      'No archive record for this id yet, and no status endpoint is configured for it either.'
    )
  }
  return (
    'No archive record for this id yet -- configured, and answering, but nothing this ' +
    'collector has sent has been written yet.'
  )
}

/** Fleet totals this collector's archive rolled up, e.g. "4 / 5". */
function peersLabel(archive: NonNullable<Collector['archive']>): string {
  const total = archive.peers_up + archive.peers_down + archive.peers_view_lost + archive.peers_stale
  return `${formatCount(archive.peers_up)} / ${formatCount(total)}`
}
function peersSub(archive: NonNullable<Collector['archive']>): string {
  return (
    `${formatCount(archive.peers_down)} down · ${formatCount(archive.peers_view_lost)} view lost` +
    ` · ${formatCount(archive.peers_stale)} stale`
  )
}

/**
 * When the archive last heard this collector's heartbeat. Unlike Liveness
 * beside it, this does not conflate an idle collector with a stuck one: a
 * collector with nothing to report still beats every 30 s. null is "never
 * heard from", which is not the same statement as a long time ago.
 */
function formatLastHeard(lastBeatAt: string | null): string {
  return lastBeatAt === null ? 'never' : formatLiveness(lastBeatAt)
}

/**
 * The activity chart's own bar geometry, in a 0..100 x 0..40 viewBox --
 * ChurnChart.vue's own trick, so the SVG scales to its card without
 * JavaScript measuring anything. Simpler than ChurnChart's: every bucket is
 * exactly one minute wide (this path's own contract), so bars sit at equal
 * intervals rather than at real timestamps positioned across a variable
 * window.
 */
function activityPeak(activity: Collector['activity']): number {
  return Math.max(1, ...activity.map((a) => a.rows))
}
function barWidth(n: number): number {
  return n > 0 ? 100 / n : 100
}
function barHeight(rows: number, peak: number): number {
  return Math.max((rows / peak) * 40, rows > 0 ? 1 : 0)
}

/**
 * The DRAWN height and top edge of one bar, which are not barHeight and
 * `40 - barHeight`: a bucket that archived nothing has barHeight 0, and a
 * zero-height rect draws nothing at all, so a completely quiet collector's
 * sparkline rendered as an empty box under a caption promising a series.
 * That is the exact case the API works hardest to produce -- it zero-fills
 * a missing key into a full-length series rather than shortening it or
 * dropping the card -- and the screen then showed nothing.
 *
 * The 0.5 floor was already here, on the height alone, and drew nothing
 * anyway: with y left at 40 the rect sat at y in [40, 40.5], entirely below
 * the 0..40 viewBox and clipped away. Both edges come from one function
 * each so that `barTop + drawnBarHeight === 40` holds by construction
 * rather than by two template expressions agreeing.
 */
const minDrawnBar = 0.5
function drawnBarHeight(rows: number, peak: number): number {
  return Math.max(barHeight(rows, peak), minDrawnBar)
}
function barTop(rows: number, peak: number): number {
  return 40 - drawnBarHeight(rows, peak)
}

// Address is sized for a dotted quad such as 10.0.103.67, which does not
// wrap and needs 98px with its padding (measured in Chromium). At 25% it
// got 97px in the narrowest desktop card, 388px at a 1366px viewport, and
// every such address read "10.0.103...". 28% gives it 106px at the 380px
// floor and more everywhere above it. Router and Peers both wrap, so the
// three points came from them.
const routerColumns: Column<CollectorRouter>[] = [
  { id: 'sysname', header: 'Router', width: '38%' },
  { id: 'ip', header: 'Address', width: '28%' },
  { id: 'peers_up', header: 'Peers', width: '34%' },
]

// A floor for the card's router table, so that on a phone, where the card
// is narrower than its 420px desktop minimum, the table scrolls inside the
// card instead of cutting every address to "172.2...". A desktop card is at
// least 420px, which leaves its table about 386px, so this never binds
// there.
const ROUTER_TABLE_MIN_WIDTH = '380px'

function routerRowAttrs(row: CollectorRouter): Record<string, string> {
  return { 'data-router': row.ip }
}
</script>

<template>
  <section class="screen">
    <ScreenHeader title="Collectors" :eyebrow="['collector health']" :counts="counts" />

    <p v-if="error" class="error" role="alert">{{ error.message }}</p>
    <p v-else-if="isPending && collectors.length === 0" class="quiet">loading…</p>
    <p v-else-if="collectors.length === 0" class="quiet">no collectors matched</p>

    <div v-else class="grid">
      <article
        v-for="c in collectors"
        :key="c.collector"
        class="card"
        :data-collector="c.collector"
      >
        <header class="card-head">
          <h2 class="mono">{{ c.collector }}</h2>
          <span class="badge" :class="badgeOf(c).level" data-badge>{{ badgeOf(c).label }}</span>
        </header>
        <p class="threshold" data-threshold>{{ badgeOf(c).threshold }}</p>

        <p v-if="c.id_mismatch" class="mismatch" data-id-mismatch>
          Configured as <strong>{{ c.id_mismatch }}</strong>, but this endpoint answered as
          <strong>{{ c.collector }}</strong> -- someone pointed
          <strong>{{ c.id_mismatch }}</strong>'s endpoint at a different collector's process.
        </p>

        <p v-if="c.reachable === false" class="error" data-error role="alert">{{ c.error }}</p>
        <p v-else-if="c.reachable === null" class="quiet" data-no-status>
          No status endpoint is configured for this collector, so there is no uptime, message
          rate or publish count to show -- not zero of each.
        </p>

        <div class="tiles">
          <StatTile
            v-if="c.archive"
            data-tile="peers"
            label="Peers up"
            :value="peersLabel(c.archive)"
            :sub="peersSub(c.archive)"
            note="Fleet totals this collector's own archive record rolls up, across every router it currently monitors."
          />
          <StatTile
            v-if="c.archive"
            data-tile="last-heard"
            label="Last heard"
            :value="formatLastHeard(c.archive.last_beat_at)"
            note="Time since the archive received this collector's heartbeat, on the archive's own clock. A collector beats every 30 s whether or not it has anything to report; its peers read stale after the API's stale threshold (90 s by default) without one."
          />
          <StatTile
            v-if="c.archive"
            data-tile="liveness"
            label="Liveness"
            :value="formatLiveness(c.archive.last_row_at)"
            note="Time since the newest row this collector wrote to ANY archive table -- end-to-end pipeline liveness, which conflates an idle collector with a stuck one. Not a per-collector lag; this pipeline has none to report."
          />
          <StatTile
            v-if="c.status"
            data-tile="msg-rate"
            label="Msg/s"
            :value="formatRate(rates.get(c.collector))"
            note="The delta between this poll's and the previous poll's bmp_messages_total, divided by the gap between their own observed_at timestamps. One sample has no rate; a counter that went backward means the process restarted, not that it slowed down."
          />
          <StatTile
            v-if="c.status"
            data-tile="uptime"
            label="Uptime"
            :value="formatUptime(c.status.started_at, c.status.observed_at)"
            note="observed_at minus started_at, both on the daemon's own clock."
          />
          <StatTile
            v-if="c.status"
            data-tile="publish-errors"
            label="Publish errors"
            :value="formatCount(c.status.publish_errors_total)"
            note="Local backpressure: this collector shedding a publish because NATS was unreachable. Never added to publish rejects -- a different failure with a different fix."
          />
          <StatTile
            v-if="c.status"
            data-tile="publish-rejects"
            label="Publish rejects"
            :value="formatCount(c.status.publish_rejects_total)"
            note="The JetStream server refusing a publish this collector believed it had sent. Never added to publish errors -- a different failure with a different fix."
          />
        </div>

        <figure class="activity">
          <figcaption class="axis" data-activity-axis>
            Rows archived per minute, over the last {{ activityWindow }}
          </figcaption>
          <svg
            v-if="c.activity.length"
            class="plot"
            viewBox="0 0 100 40"
            preserveAspectRatio="none"
            role="img"
            :aria-label="`Rows archived per minute over the last ${activityWindow}. The final bar is the current, still-accumulating minute.`"
            data-activity-chart
          >
            <g v-for="(bucket, i) in c.activity" :key="i">
              <title>
                {{ formatClock(bucket.minute) }} · {{ formatCount(bucket.rows) }} rows{{
                  i === c.activity.length - 1 ? ' (still accumulating)' : ''
                }}
              </title>
              <rect
                :x="i * barWidth(c.activity.length)"
                :y="barTop(bucket.rows, activityPeak(c.activity))"
                :width="Math.max(barWidth(c.activity.length) - 0.4, 0.6)"
                :height="drawnBarHeight(bucket.rows, activityPeak(c.activity))"
                :class="{ current: i === c.activity.length - 1 }"
              />
            </g>
          </svg>
        </figure>

        <DataTable
          v-if="c.archive"
          class="routers"
          :columns="routerColumns"
          :rows="c.archive.routers"
          :row-attrs="routerRowAttrs"
          :min-width="ROUTER_TABLE_MIN_WIDTH"
        >
          <template #cell-sysname="{ row }">
            {{ routerLabel((row as CollectorRouter).sysname) }}
            <div v-if="(row as CollectorRouter).sys_descr" class="os-line" data-os-line>
              {{ (row as CollectorRouter).sys_descr }}
            </div>
          </template>
          <template #cell-peers_up="{ row }">
            {{ (row as CollectorRouter).peers_up }} up ·
            {{ (row as CollectorRouter).peers_down }} down ·
            {{ (row as CollectorRouter).peers_view_lost }} view lost ·
            {{ (row as CollectorRouter).peers_stale }} stale
          </template>
        </DataTable>
        <p v-else class="quiet" data-not-archived>{{ notArchivedNote(c) }}</p>
      </article>
    </div>
  </section>
</template>

<style scoped>
.screen { padding: 18px; display: flex; flex-direction: column; gap: 14px; }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(min(420px, 100%), 1fr)); gap: 14px; }
.card {
  display: flex; flex-direction: column; gap: 10px; padding: 14px 16px;
  border: 1px solid var(--line); border-radius: 8px; background: var(--surface);
}
.card-head { display: flex; align-items: center; gap: 10px; }
.card-head h2 { margin: 0; font: 600 13px var(--font-data); color: var(--ink); flex: 1 1 auto; overflow-wrap: anywhere; }
.badge {
  display: inline-block; padding: 2px 8px; border-radius: 20px;
  font: 600 10px var(--font-ui); text-transform: uppercase; letter-spacing: .04em; white-space: nowrap;
}
.badge.healthy { background: var(--good-tint); color: var(--good); }
.badge.degraded { background: var(--bad-tint); color: var(--bad-2); }
.badge.unconfigured { background: var(--surface-2); color: var(--muted); }
.threshold { margin: 0; color: var(--muted); font: 400 10.5px var(--font-ui); }
.mismatch {
  margin: 0; padding: 8px 10px; border-radius: 6px; background: var(--accent-tint);
  color: var(--accent-ink-2); font: 400 11px var(--font-ui);
}
.error {
  margin: 0; padding: 8px 10px; border-radius: 6px; background: var(--bad-tint);
  color: var(--bad-2); font: 400 11px var(--font-ui);
}
.quiet { margin: 0; color: var(--muted); font: 400 11px var(--font-ui); }
.tiles { display: flex; flex-wrap: wrap; gap: 8px; }
.tiles :deep(.tile) { min-width: 110px; padding: 8px 10px; }
.tiles :deep(.value) { font-size: 17px; }
.activity { margin: 0; display: flex; flex-direction: column; gap: 4px; }
.axis { margin: 0; color: var(--muted); font: 400 10.5px var(--font-data); }
.plot { width: 100%; height: 44px; display: block; }
.plot rect { fill: var(--ink-2); }
/* The final bar is the current, still-accumulating minute -- never drawn
   as though it were a completed one. */
.plot rect.current { fill: var(--muted); }
.routers { font-size: 11px; }
.os-line { color: var(--muted); font: 400 10.5px var(--font-ui); }
</style>
