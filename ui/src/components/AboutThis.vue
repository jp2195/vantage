<script setup lang="ts">
/**
 * The long explanation of a number, folded under one line.
 *
 * This project's screens say what each figure does and does not measure,
 * and they are right to: a collection rate read as a router's update rate
 * is a wrong conclusion drawn from a true number. But a paragraph of it
 * above every table pushed the table below the fold, and a reader who has
 * read it once does not need it in the way every time.
 *
 * A <details>, not a tooltip: the text stays in the page, selectable,
 * findable with Ctrl+F, and read by a screen reader in order. It is in the
 * DOM whether open or not, so a test that asserts the caveat exists still
 * finds it. The summary names what is explained, so the reader knows there
 * is a caveat before choosing to read it.
 */
withDefaults(defineProps<{ summary?: string }>(), { summary: 'How to read this' })
</script>

<template>
  <details class="about">
    <summary>
      <svg class="icon" width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="6.5" />
        <path d="M8 7v4M8 4.6v.1" />
      </svg>
      {{ summary }}
      <svg class="chev" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
        <path d="M3 2l3 3-3 3" />
      </svg>
    </summary>
    <div class="body"><slot /></div>
  </details>
</template>

<style scoped>
.about { font: 400 var(--text-sm) / 1.5 var(--font-ui); color: var(--muted); }
summary {
  display: inline-flex; align-items: center; gap: 6px; cursor: pointer; list-style: none;
  color: var(--link); font-weight: 500; border-radius: var(--radius-sm);
}
summary::-webkit-details-marker { display: none; }
summary:hover { color: var(--link-hover); }
.icon, .chev { flex: none; fill: none; stroke: currentColor; stroke-width: 1.5; stroke-linecap: round; }
.chev { transition: transform .15s; }
.about[open] .chev { transform: rotate(90deg); }
.body {
  margin-top: var(--space-2); padding: 0 0 0 20px; max-width: 84ch;
  display: flex; flex-direction: column; gap: var(--space-2);
}
/* Paragraphs moved in keep their own classes; their margins and widths
   were set for standing alone on the page. */
.body :slotted(p), .body :slotted(span) { margin: 0; max-width: none; }
@media (prefers-reduced-motion: reduce) { .chev { transition: none; } }
</style>
