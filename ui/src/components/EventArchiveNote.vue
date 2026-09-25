<script setup lang="ts">
/**
 * The two things this archive cannot say, beside the results rather than
 * buried in a tooltip.
 *
 * Extracted from SessionHistoryView.vue so the Events screen states them
 * identically. Both are present whether the table above is full or empty,
 * because an empty list does not mean "nothing happened": it can equally
 * mean the events aged past the history retention, or simply fell outside the
 * window in use.
 *
 * `windowLabel` is a prop rather than a constant because the two screens
 * offer different windows -- Session history reaches out to the default
 * 90-day retention, the fleet view is clamped by the daemon's max_unscoped_since --
 * and a note naming the wrong one would be worse than no note.
 *
 * This does NOT state the cap. A cap is a property of one answer, not of the
 * archive, and ResultMeta's truncated path already says it with the real
 * numbers.
 */
defineProps<{ windowLabel: string }>()
</script>

<template>
  <p class="notes" data-archive-note>
    peer_events keeps history for the configured retention (90 days by
    default) on the collector clock; events past that window are deleted, not merely hidden, so an empty or short list here
    may mean nothing happened in the {{ windowLabel }} shown, or that it aged
    out of retention. A "down" whose reason says notification follows only
    records that a NOTIFICATION PDU was sent -- its contents are not retained
    in this archive.
  </p>
</template>

<style scoped>
.notes { margin: 0; padding: 9px 12px; border-radius: 6px; background: var(--surface-2); border: 1px solid var(--line); color: var(--muted); font: 400 11.5px var(--font-ui); }
</style>
