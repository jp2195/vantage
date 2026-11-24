<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { jumpTarget } from '@/lib/jumpTarget'

/**
 * The sidebar's search: type a prefix, address, AS number or community and
 * land on the Looking glass answering it. ⌘K / Ctrl+K focuses it from
 * anywhere, and so does "/" when focus is not already in a field.
 *
 * Reads nothing: lib/jumpTarget classifies the term by its form, so this
 * stays out of Colada and the shell stays mountable without a query cache
 * (see FleetChrome's comment for why that matters to App's boot tests).
 */
const router = useRouter()
const term = ref('')
const input = ref<HTMLInputElement>()
const declined = ref(false)

function submit() {
  const to = jumpTarget(term.value)
  declined.value = !to && term.value.trim() !== ''
  if (!to) return
  router.push(to)
  term.value = ''
  input.value?.blur()
}

// Asks the shell to unfold the sidebar first: the field is hidden while it
// is folded to a rail, and a hidden field cannot take focus.
const emit = defineEmits<{ summon: [] }>()

function onKey(e: KeyboardEvent) {
  const typing =
    e.target instanceof HTMLElement &&
    (e.target.isContentEditable || ['INPUT', 'SELECT', 'TEXTAREA'].includes(e.target.tagName))
  if ((e.key === 'k' && (e.metaKey || e.ctrlKey)) || (e.key === '/' && !typing)) {
    e.preventDefault()
    emit('summon')
    nextTick(() => input.value?.focus())
  }
}

onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <form class="jump" role="search" @submit.prevent="submit">
    <label class="field">
      <svg class="icon" width="14" height="14" viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="7" cy="7" r="5" />
        <path d="M11 11l3.5 3.5" />
      </svg>
      <span class="sr-only">Jump to a prefix, address, AS or community</span>
      <input
        ref="input"
        v-model="term"
        type="search"
        placeholder="Prefix, AS, community…"
        autocomplete="off"
        spellcheck="false"
        :aria-invalid="declined || undefined"
        aria-describedby="jump-hint"
        @input="declined = false"
      />
      <kbd aria-hidden="true">/</kbd>
    </label>
    <p id="jump-hint" class="hint" :class="{ shown: declined }" aria-live="polite">
      {{ declined ? 'Try a prefix, an address, AS65002 or 65002:100.' : '' }}
    </p>
  </form>
</template>

<style scoped>
.jump { display: flex; flex-direction: column; gap: var(--space-1); }
.field {
  display: flex; align-items: center; gap: var(--space-2);
  min-height: 34px; padding: 0 var(--space-2) 0 10px;
  border: 1px solid var(--chrome-2); border-radius: var(--radius-md);
  background: var(--chrome);
}
.field:focus-within { border-color: var(--focus); box-shadow: 0 0 0 3px var(--focus-ring); }
.icon { flex: none; fill: none; stroke: var(--chrome-text-2); stroke-width: 1.6; }
/* The global input styles draw a light field; this one sits on the chrome. */
input[type='search'] {
  flex: 1; min-width: 0; padding: 6px 0; border: 0; border-radius: 0;
  background: transparent; color: var(--chrome-text);
  font: 400 var(--text-body) var(--font-ui);
}
input[type='search']:focus-visible { box-shadow: none; outline: none; }
input::placeholder { color: var(--chrome-text-2); }
kbd {
  flex: none; font: 500 11px var(--font-data); color: var(--chrome-text-2);
  border: 1px solid var(--chrome-2); border-radius: var(--radius-sm); padding: 0 5px;
}
.hint { margin: 0; min-height: 0; font: 400 var(--text-sm) var(--font-ui); color: var(--accent); }
.hint:not(.shown) { display: none; }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
