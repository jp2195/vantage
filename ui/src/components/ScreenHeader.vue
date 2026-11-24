<script setup lang="ts">
/**
 * This project's page header, and the hierarchy every screen in it opens
 * with: a muted eyebrow naming the scope the answer is about, the title,
 * and the answer's own counts right-aligned opposite them.
 *
 * Why it is a component rather than markup in each screen: the eyebrow is
 * the one place a screen says WHAT IT IS SHOWING as opposed to what it can
 * show, and ten screens writing that themselves is ten chances to phrase a
 * scope differently from the answer underneath it.
 *
 * `counts` carries only what an answer measured. A count whose value the
 * response did not include must be OMITTED by the caller rather than passed
 * as 0 or "-": a rendered zero is a measurement, and this project's own
 * "Entries 992 411" has nothing behind it on a keyset walk, whose
 * meta.total_matched is null by design.
 */
defineProps<{
  title: string
  /** Scope segments, joined with a middot. */
  eyebrow?: string[]
  /** `key` becomes a `data-count-<key>` hook so a test can address one count. */
  counts?: { key: string; label: string; value: string }[]
}>()
</script>

<template>
  <header class="head">
    <div class="titles">
      <p v-if="eyebrow?.length" class="eyebrow" data-eyebrow>{{ eyebrow.join(' · ') }}</p>
      <h1>{{ title }}</h1>
    </div>
    <dl v-if="counts?.length" class="counts">
      <div v-for="c in counts" :key="c.key" v-bind="{ ['data-count-' + c.key]: '' }">
        <dt>{{ c.label }}</dt>
        <dd class="mono">{{ c.value }}</dd>
      </div>
    </dl>
  </header>
</template>

<style scoped>
.head { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: var(--space-4); }
.titles { display: flex; flex-direction: column; gap: var(--space-1); }
/* The label scale, the same one DataTable's column headers and StatTile's
   labels use, so a scope and a column read as the same kind of text. In the
   UI face: set in the data face it was the one label on the page that
   looked different from the rest. */
.eyebrow {
  margin: 0; color: var(--muted); text-transform: uppercase;
  font: 600 var(--text-label) var(--font-ui); letter-spacing: .06em;
}
h1 { margin: 0; font: 600 var(--text-page) var(--font-ui); letter-spacing: -.01em; color: var(--ink); }
.counts { display: flex; gap: var(--space-6); margin: 0; }
.counts dt {
  color: var(--muted); text-transform: uppercase;
  font: 600 var(--text-label) var(--font-ui); letter-spacing: .06em; text-align: right;
}
.counts dd { margin: 2px 0 0; font: 500 var(--text-page) var(--font-data); color: var(--ink); text-align: right; }
</style>
