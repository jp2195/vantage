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
      <p v-if="eyebrow?.length" class="eyebrow mono" data-eyebrow>{{ eyebrow.join(' · ') }}</p>
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
.head { display: flex; align-items: flex-end; justify-content: space-between; gap: 16px; }
.titles { display: flex; flex-direction: column; gap: 3px; }
/* 9.5px 600 uppercase with .07em tracking: this project's label scale, the
   same one DataTable's column headers use, so a scope and a column read as
   the same kind of text. */
.eyebrow {
  margin: 0; color: var(--muted); text-transform: uppercase;
  font-size: 9.5px; font-weight: 600; letter-spacing: .07em;
}
h1 { margin: 0; font: 600 19px var(--font-ui); color: var(--ink); }
.counts { display: flex; gap: 22px; margin: 0; }
.counts dt {
  color: var(--muted); text-transform: uppercase;
  font: 600 9.5px var(--font-ui); letter-spacing: .07em; text-align: right;
}
.counts dd { margin: 2px 0 0; font: 500 19px var(--font-data); color: var(--ink); text-align: right; }
</style>
