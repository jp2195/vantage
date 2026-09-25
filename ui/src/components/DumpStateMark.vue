<script setup lang="ts">
/**
 * What a route row wears when its own family's initial dump is not known to
 * have finished.
 *
 * One component for both route screens. RoutesView grew this marker first
 * and the Looking glass rendered the identical row shape without it, so the
 * same route was provisional on one screen and settled fact on the other.
 *
 * Keyed on `!== 'complete'`, never on `=== 'dumping'`. api/openapi.yaml
 * documents three values and says outright why the field exists at all --
 * "because an empty result and a partial result are otherwise
 * indistinguishable" -- and a `=== 'dumping'` test renders the third value,
 * `unknown`, exactly like `complete`. That is the failure the field was
 * added to prevent, reintroduced by the code reading it. A fourth value
 * added to the enum later lands in the same branch for the same reason.
 *
 * `unknown` gets its own copy because it is a different fact, not a quieter
 * version of the same one. query/query.go's dumpStateExpr resolves it when
 * the contributing peer is not up, or when the collector holds no session
 * state for that RIB view at all: "still arriving" and "nobody can say" are
 * different things to tell an operator mid-incident.
 */
defineProps<{ state: string }>()
</script>

<template>
  <span
    v-if="state === 'dumping'"
    class="mark dumping"
    title="This row came from a session still sending its initial dump, so the peer's table is not fully represented yet."
    >provisional</span
  >
  <span
    v-else-if="state !== 'complete'"
    class="mark unknown"
    title="The peer behind this row is not up, or the collector holds no session state for its RIB view, so whether this family's table was fully delivered cannot be determined. This is not a claim that it was."
    >dump unknown</span
  >
</template>

<style scoped>
.mark {
  margin-left: 7px; padding: 1px 6px; border-radius: 4px;
  font: 500 10px var(--font-ui);
}
.dumping { background: var(--neutral-chip); color: var(--muted); }
/* Amber, not neutral: "we cannot say" is a caveat about the answer, the
   same class of thing ResultMeta paints amber, while "still arriving" is
   an ordinary transient. */
.unknown { background: var(--accent-tint); color: var(--accent-ink-2); }
</style>
