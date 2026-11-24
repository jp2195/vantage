<script setup lang="ts">
/**
 * One KPI tile: Monitor and Peer detail both open with a row of them --
 * an uppercase micro-label, a large mono value, and a sub-line naming
 * what the value is made of.
 *
 * `value` is a STRING, formatted by the caller. A tile cannot compute
 * anything: every number it shows has to come from a response that measured
 * it, and a component that did arithmetic here would be the one place in
 * this UI where a displayed figure has no endpoint behind it.
 *
 * There is no `trend`, no `delta` and no color, unlike some dashboard
 * designs, which carry "-2" and "+312" against a previous window;
 * nothing in this API returns a previous window, and computing one
 * client-side would mean two requests whose windows the screen -- not
 * the operator -- chose.
 */
import { computed } from 'vue'

const props = defineProps<{
  label: string
  value: string
  /** What the value is composed of, e.g. "8 416 dumps · 8 006 changes". */
  sub?: string
  /** The claim this number must be read with, shown on hover. */
  note?: string
}>()

/**
 * A ratio the caller formatted as "4 / 4" is split so the denominator can
 * recede: the count is the reading, the total is its context. Presentation
 * only -- the text is unchanged, so a reader (or a test) still gets "4 / 4".
 */
const ratio = computed(() => {
  const m = /^(\S+) \/ (\S+)$/.exec(props.value)
  return m ? { count: m[1], total: m[2] } : null
})
</script>

<template>
  <div class="tile" :title="note">
    <p class="label">{{ label }}</p>
    <!-- The size container is this wrapper, not the tile: containment on
         the tile made it an independent layout, which switches off the
         subgrid a .tile-grid lines tiles up with. -->
    <div class="fit">
      <p v-if="ratio" class="value mono">{{ ratio.count }}<span class="of"> / {{ ratio.total }}</span></p>
      <p v-else class="value mono" :class="{ long: value.length > 12 }">{{ value }}</p>
    </div>
    <p v-if="sub" class="sub">{{ sub }}</p>
  </div>
</template>

<style scoped>
.tile {
  flex: 1 1 0; min-width: 150px; padding: 14px var(--space-4); box-sizing: border-box;
  border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface);
}
.fit { container-type: inline-size; min-width: 0; }
.label {
  margin: 0; color: var(--muted); text-transform: uppercase;
  font: 600 var(--text-label) var(--font-ui); letter-spacing: .06em;
}
/* The KPI value scale, with -.02em tracking. */
.value { margin: 6px 0 0; font: 500 var(--text-kpi) var(--font-data); letter-spacing: -.02em; color: var(--ink); }
/* Scales with the tile, down to the panel-heading size, so a ten-digit ASN
   in a phone-width tile stays inside it rather than running off the edge. */
.value { font-size: clamp(var(--text-title), 13cqi, var(--text-kpi)); overflow-wrap: anywhere; }
/* A timestamp or a duration is not a count and does not need count-sized
   type: "Sep 7, 2026, 21:11:38" at the KPI size wrapped to three lines in a
   tile a sixth of the row wide. */
.value.long { font-size: var(--text-title); letter-spacing: 0; line-height: 1.35; padding-top: 4px; }
.of { color: var(--muted); font-size: .62em; font-weight: 400; letter-spacing: 0; }
.sub { margin: var(--space-1) 0 0; color: var(--muted); font: 400 var(--text-sm) var(--font-ui); }
</style>
