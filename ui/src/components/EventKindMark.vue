<script setup lang="ts">
import type { PeerEvent } from '@/api/generated'

/**
 * The one place a peer_event's kind becomes something on screen.
 *
 * Extracted from SessionHistoryView.vue for the Events screen to share, on
 * the reasoning in lib/eventReason.ts. Collapsing two kinds together -- the
 * mutation the view_lost and unspecified tests exist to catch -- now has
 * exactly one file to touch, and both screens' tests are pinned against it.
 */
const props = defineProps<{ kind: PeerEvent['kind'] }>()

const KIND_LABEL: Record<PeerEvent['kind'], string> = {
  unspecified: 'unspecified',
  up: 'up',
  down: 'down',
  view_lost: 'collector lost view',
}

const KIND_TITLE: Record<PeerEvent['kind'], string> = {
  unspecified: 'The pipeline that recorded this event did not state its kind.',
  up: 'The router reported this session established.',
  down: 'The router reported this session ended, with a reason on the wire.',
  view_lost:
    "The collector stopped seeing this peer over BMP; the router said nothing. " +
    'This is a different fact from a session ending, not a quieter version of one.',
}
</script>

<template>
  <span class="kind-mark" :class="props.kind" :title="KIND_TITLE[props.kind]">{{
    KIND_LABEL[props.kind]
  }}</span>
</template>

<style scoped>
.kind-mark { display: inline-block; padding: 2px 8px; border-radius: 20px; font: 500 10.5px var(--font-ui); }
.kind-mark.up { background: var(--good-tint); color: var(--good); }
/* down is amber: the network told us something. view_lost is red: we are
   blind. unspecified is neutral: the pipeline made no claim at all. The
   color difference is the finding, not decoration -- same convention as
   StatePill.vue. */
.kind-mark.down { background: var(--accent-tint); color: var(--accent-ink-2); }
.kind-mark.view_lost { background: var(--bad-tint); color: var(--bad-2); }
.kind-mark.unspecified { background: var(--neutral-chip); color: var(--muted); }
</style>
