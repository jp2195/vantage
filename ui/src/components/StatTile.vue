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
defineProps<{
  label: string
  value: string
  /** What the value is composed of, e.g. "8 416 dumps · 8 006 changes". */
  sub?: string
  /** The claim this number must be read with, shown on hover. */
  note?: string
}>()
</script>

<template>
  <div class="tile" :title="note">
    <p class="label">{{ label }}</p>
    <p class="value mono">{{ value }}</p>
    <p v-if="sub" class="sub">{{ sub }}</p>
  </div>
</template>

<style scoped>
.tile {
  flex: 1 1 0; min-width: 150px; padding: 14px var(--space-4);
  border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface);
}
.label {
  margin: 0; color: var(--muted); text-transform: uppercase;
  font: 600 var(--text-label) var(--font-ui); letter-spacing: .06em;
}
/* The KPI value scale, with -.02em tracking. */
.value { margin: 6px 0 0; font: 500 var(--text-kpi) var(--font-data); letter-spacing: -.02em; color: var(--ink); }
.sub { margin: var(--space-1) 0 0; color: var(--muted); font: 400 var(--text-sm) var(--font-ui); }
</style>
