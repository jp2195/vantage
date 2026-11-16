<script setup lang="ts">
import { computed } from 'vue'
import { formatClock, formatFreshness } from '@/lib/formatClock'

/**
 * A timestamp in a table cell or a list row.
 *
 * The text is formatClock's (or, with `today`, formatFreshness's, which
 * drops the date for an instant on the reader's own today). What this adds
 * is where the text may break: only between the date and the time. In a
 * narrow column "Nov 16, 2026, 08:31:05" broke wherever the width ran out
 * -- three lines, with "2026," alone on one -- and a timestamp in pieces
 * reads as several values. Now it is one line, or the date over the time.
 *
 * The rendered text is exactly the formatter's, spaces included, so a test
 * comparing a cell to formatClock(iso) still holds.
 */
const props = defineProps<{ iso: string; today?: boolean }>()

const text = computed(() => (props.today ? formatFreshness(props.iso) : formatClock(props.iso)))

// Split at the last ", ": en-US renders "Nov 16, 2026, 08:31:05". A locale
// that renders no such separator keeps the whole string as one piece.
const parts = computed(() => {
  const t = text.value
  const at = t.lastIndexOf(', ')
  return at < 0 ? { date: '', time: t } : { date: t.slice(0, at + 1), time: t.slice(at + 2) }
})
</script>

<template>
  <time class="clock mono" :datetime="iso" :title="formatClock(iso)"
    ><span v-if="parts.date" class="part">{{ parts.date }}</span>{{ parts.date ? ' ' : '' }}<span class="part">{{ parts.time }}</span></time
  >
</template>

<style scoped>
.part { white-space: nowrap; }
</style>
