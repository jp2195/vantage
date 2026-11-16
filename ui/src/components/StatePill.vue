<script setup lang="ts">
import { computed } from 'vue'
import type { Peer } from '@/api/generated'

// The contract's own union, not a copy of it: a state the API adds is a type
// error here until this pill can say what the state means.
type State = Peer['state']
const props = defineProps<{ state: State }>()

const label = computed(() => ({
  unspecified: 'unspecified',
  up: 'up',
  down: 'down',
  view_lost: 'view lost',
  stale: 'stale',
}[props.state]))

const title = computed(() => ({
  unspecified:
    'The newest event for this session is of a kind the writer did not recognize, ' +
    'so the archive cannot say whether it is up. Its routes are excluded from every ' +
    'current-state answer.',
  up: 'The BGP session is established and its routes are current.',
  down: 'The router reported this BGP session down, with a reason on the wire.',
  view_lost:
    'The collector lost its BMP transport and the router said nothing, or the ' +
    'collector has restarted since this session began. Nobody is watching this ' +
    'peer, and its routes are excluded from every current-state answer.',
  stale:
    'This session was last up, but its collector has not been heard from within ' +
    'the stale threshold, so nobody can vouch for it right now. Its routes are ' +
    'still shown, as that collector last saw them.',
}[props.state]))
</script>

<template>
  <span class="pill" :class="state" :title="title">{{ label }}</span>
</template>

<style scoped>
.pill { display: inline-block; padding: 2px 9px 2px 7px; border-radius: var(--radius-full); font: 500 var(--text-sm) var(--font-ui); white-space: nowrap; }
.up { background: var(--good-tint); color: var(--good); }
/* down is amber: the network told us something. view_lost is red: we are
   blind. The color difference is the finding, not decoration. */
.down { background: var(--warn-tint); color: var(--warn-ink); }
/* A shape beside the color, so the state still reads in a grayscale
   screenshot or to a color-blind operator: a dot is up, a bar is down, a
   cross is blind, a ring is unknown. Drawn in currentColor, as a pseudo
   element, so the text a screen reader or a test reads is unchanged. */
.pill::before {
  content: ''; display: inline-block; vertical-align: 1px; margin-right: 5px;
  width: 6px; height: 6px; border-radius: 50%; background: currentColor;
}
.down::before { width: 7px; height: 2px; border-radius: 1px; vertical-align: 2px; }
.view_lost::before {
  width: 7px; height: 7px; border-radius: 0; vertical-align: 0;
  background:
    linear-gradient(45deg, transparent 43%, currentColor 43% 57%, transparent 57%),
    linear-gradient(-45deg, transparent 43%, currentColor 43% 57%, transparent 57%);
}

.view_lost { background: var(--bad-tint); color: var(--bad-2); }
/* stale is neither: nobody is contradicting "up", nobody is confirming it.
   Neutral, and dashed, because it is a gap in what we know rather than a
   state of the network. --ink-2 text and a --muted-2 dash, not --muted text
   and a --line-2 dash. --muted on --neutral-chip measures 4.63:1, which
   passes AA but is the dumping chip's own pairing, and the pill must not
   read as the dumping chip it often sits beside. --ink-2 on the same chip
   is 9.25:1, and the --line-2 dash barely showed. */
/* unspecified: the archive has no state to report, so it takes the neutral
   chip with no dash, apart from stale's "last known up". */
.unspecified { background: var(--neutral-chip); color: var(--ink-2); }
.stale { background: var(--neutral-chip); color: var(--ink-2); outline: 1px dashed var(--muted-2); outline-offset: -1px; }
.stale::before { background: none; box-shadow: inset 0 0 0 1.5px currentColor; }
.unspecified::before { display: none; }
</style>
