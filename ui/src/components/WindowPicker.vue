<script setup lang="ts">
/**
 * A segmented window control (1h / 6h / 24h / 7d on Monitor): the
 * choices visible at once, the chosen one solid, rather than a select an
 * operator has to open to see what a screen is even offering.
 *
 * One component for the three screens that offer a window -- Monitor,
 * Events, Session history -- because they had three copies of the same
 * markup and the lists differ (three windows on the collection endpoints,
 * six on Session history, which is scoped and so escapes the daemon's
 * unscoped clamp). A fourth copy is how one screen would quietly acquire a
 * window the endpoint behind it refuses.
 *
 * `data-since-window` is the hook every one of their tests already uses, and
 * each button carries its own `value` so a test reads the offered set from
 * the DOM rather than from a second copy of the list.
 */
const model = defineModel<string>({ required: true })

defineProps<{
  windows: { value: string; label: string }[]
  /** Accessible name; the screens say "Window" unless they mean something narrower. */
  label?: string
}>()
</script>

<template>
  <div class="segmented" data-since-window role="group" :aria-label="label ?? 'Window'">
    <button
      v-for="w in windows"
      :key="w.value"
      type="button"
      :class="{ on: model === w.value }"
      :aria-pressed="model === w.value"
      :value="w.value"
      @click="model = w.value"
    >
      {{ w.label }}
    </button>
  </div>
</template>

<style scoped>
.segmented { display: inline-flex; border: 1px solid var(--line-2); border-radius: 6px; overflow: hidden; }
.segmented button {
  border: 0; background: var(--surface); color: var(--muted);
  font: 500 11px var(--font-ui); padding: 6px 11px; cursor: pointer; white-space: nowrap;
}
.segmented button + button { border-left: 1px solid var(--line-2); }
.segmented button.on { background: var(--ink); color: var(--on-dark); }
</style>
