<script setup lang="ts">
import { computed } from 'vue'

/**
 * A sentence whose `backticked` spans are commands or names, set as code.
 * The AS-names notice is a plain string from lib/asname.ts so its tests read
 * it whole; printed as-is, the backticks showed on screen around the
 * command an operator is meant to copy.
 */
const props = defineProps<{ text: string }>()
const parts = computed(() => props.text.split('`'))
</script>

<template>
  <template v-for="(part, i) in parts" :key="i"
    ><code v-if="i % 2">{{ part }}</code
    ><template v-else>{{ part }}</template></template
  >
</template>
