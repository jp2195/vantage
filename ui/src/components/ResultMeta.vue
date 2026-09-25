<script setup lang="ts">
import { computed } from 'vue'
import type { Meta } from '@/api/generated'

// The generated Meta, not a second one declared here. A local re-declaration
// is a copy of the contract that nothing keeps in sync: it compiled happily
// against a `warnings[].code` of plain `string` while the real union is
// three specific values, and it would go on compiling if the contract's
// shape changed underneath it. Importing means openapi-ts regeneration is
// what updates this component's idea of a response.
const props = defineProps<{ meta: Meta; shown?: number }>()

// The codes this component has curated copy for. Typed as a Set<string>
// rather than of the generated union, because its whole job below is to
// answer a question about codes OUTSIDE that union.
const CURATED = new Set<string>(['session_dumping', 'paginated_smear', 'truncated', 'collector_stale'])

const codes = computed(() => new Set<string>(props.meta.warnings.map((w) => w.code)))
const complete = computed(() => props.meta.warnings.length === 0)

/**
 * Warnings this component has no copy for.
 *
 * Every code below was hard-coded with no fallthrough, and `message` -- which
 * api/openapi.yaml requires on every warning and the daemon always sends --
 * was never rendered at all. A fourth code would have compiled cleanly and
 * produced an EMPTY footer: not "complete", because warnings is non-empty,
 * and not the warning either. Going silent about a caveat the server took
 * the trouble to send is the direction of error this component exists to
 * prevent, so an unrecognized code falls back to the daemon's own sentence.
 */
const unrecognized = computed(() => props.meta.warnings.filter((w) => !CURATED.has(w.code)))

// The client's receive time, not a server claim: Meta carries no timestamp,
// so the copy says "as of" a moment this browser observed and must not
// imply the daemon asserted an hour.
//
// Computed rather than a plain const: a ResultMeta stays mounted across a
// refetch -- same screen, new data, no remount, exactly what polling and
// Colada's cache do (queries.ts's pollWhileMounted) -- and `meta` is a new
// object each time (every composable in queries.ts returns a fresh one, never
// a mutated one). Reading `props.meta` here makes that the reactive
// dependency, so the timestamp is recomputed against the response actually
// on screen instead of freezing at whatever first mounted this instance.
const seenAt = computed(() => {
  void props.meta
  return new Date().toLocaleTimeString([], { hour12: false })
})
</script>

<template>
  <footer class="meta">
    <span v-if="complete" class="ok">complete as of {{ seenAt }}</span>

    <span v-if="codes.has('session_dumping')" class="warn">
      a session is still loading its initial view — counts will rise
    </span>

    <span v-if="codes.has('paginated_smear')" class="warn">
      pages were fetched across a changing view — rows may repeat or be missing
    </span>

    <span v-if="codes.has('collector_stale')" class="warn">
      a collector here has gone quiet — its rows are its last view, not a current one
    </span>

    <span v-if="codes.has('truncated')" class="note">
      showing {{ shown ?? '?' }} of {{ meta.total_matched }}
    </span>

    <span v-for="(w, i) in unrecognized" :key="`${w.code}|${i}`" class="warn">
      {{ w.message }}
    </span>
  </footer>
</template>

<style scoped>
.meta { display: flex; gap: 12px; padding: 7px 18px; font: 400 11px var(--font-ui); border-top: 1px solid var(--line-faint); }
.ok { color: var(--muted); }
.warn { color: var(--accent-ink-2); background: var(--accent-tint); padding: 2px 7px; border-radius: 4px; }
.note { color: var(--muted); }
</style>
