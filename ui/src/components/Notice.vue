<script setup lang="ts">
/**
 * A sentence about the answer on screen, set apart from it.
 *
 * Three tones, and choosing one is a claim about the sentence:
 *   info  explanation. Nothing is wrong ("dev-c1 and dev-c2 both watch this
 *         route, so it is listed once per collector").
 *   warn  the answer needs a caveat or something needs a look ("two
 *         collectors disagree", "the list restarted mid-walk").
 *   bad   we could not get the answer at all.
 *
 * Every one of these was amber once, which made an explanation and a
 * warning indistinguishable -- and, with enough explanations on a screen,
 * taught the reader to skip amber. The icon repeats the tone in shape, so
 * the distinction survives a grayscale screenshot and color blindness.
 *
 * The root is a <p>, and class, role and data-* attributes fall through to
 * it, so a screen keeps its own hooks: `<Notice class="scope-note"
 * role="status">`.
 */
withDefaults(defineProps<{ tone?: 'info' | 'warn' | 'bad' }>(), { tone: 'info' })
</script>

<template>
  <p class="notice" :class="tone">
    <svg class="icon" width="15" height="15" viewBox="0 0 16 16" aria-hidden="true">
      <template v-if="tone === 'info'">
        <circle cx="8" cy="8" r="6.5" />
        <path d="M8 7v4M8 4.6v.1" />
      </template>
      <template v-else-if="tone === 'warn'">
        <path d="M8 1.8L15 14H1z" stroke-linejoin="round" />
        <path d="M8 6.5v3.5M8 12v.1" />
      </template>
      <template v-else>
        <circle cx="8" cy="8" r="6.5" />
        <path d="M5.5 5.5l5 5M10.5 5.5l-5 5" />
      </template>
    </svg>
    <span class="body"><slot /></span>
  </p>
</template>

<style scoped>
.notice {
  margin: 0;
  display: flex;
  gap: var(--space-2);
  align-items: flex-start;
  padding: 9px var(--space-3);
  border: 1px solid;
  border-radius: var(--radius-md);
  font: 400 var(--text-body) / 1.5 var(--font-ui);
}
.icon { flex: none; margin-top: 2px; fill: none; stroke: currentColor; stroke-width: 1.5; }
.body { flex: 1; min-width: 0; }
.info { background: var(--info-tint); border-color: var(--info-line); color: var(--info-ink); }
.info .icon { color: var(--info); }
.warn { background: var(--warn-tint); border-color: var(--warn-line); color: var(--warn-ink); }
.warn .icon { color: var(--warn); }
.bad { background: var(--bad-tint); border-color: var(--bad-line); color: var(--bad-2); }
.bad .icon { color: var(--bad); }
</style>
