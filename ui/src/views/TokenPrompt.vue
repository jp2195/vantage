<script setup lang="ts">
import { ref } from 'vue'
import { storeCredential } from '@/api/auth'

const token = ref('')
const emit = defineEmits<{ (e: 'authenticated', token: string): void }>()

function submit() {
  const value = token.value.trim()
  if (!value) return
  storeCredential(value)
  emit('authenticated', value)
}
</script>

<template>
  <form class="prompt" @submit.prevent="submit">
    <h1>API token</h1>
    <p>
      This deployment authenticates with a bearer token. Get one with
      <code class="mono">kubectl -n vantage get secret vantage-api -o jsonpath='{.data.lab}' | base64 -d</code>
    </p>
    <input v-model="token" class="mono" type="password" autocomplete="off" placeholder="token" />
    <button type="submit">Continue</button>
  </form>
</template>

<style scoped>
.prompt { max-width: 520px; margin: 64px auto; display: flex; flex-direction: column; gap: 12px; font-family: var(--font-ui); }
h1 { font: 600 var(--text-page) var(--font-ui); color: var(--ink); margin: 0; }
p { font-size: 12px; color: var(--muted); line-height: 1.5; }
code { font-size: 11px; background: var(--surface-2); padding: 2px 5px; border-radius: var(--radius-sm); }
input { border: 1.5px solid var(--focus); border-radius: var(--radius-lg); padding: 9px 11px; font-size: 14px; }
input:focus { outline: none; box-shadow: 0 0 0 3px var(--focus-ring); }
button { background: var(--ink); color: var(--on-ink); border: 0; border-radius: var(--radius-md); padding: 9px; font: 500 var(--text-sm) var(--font-ui); cursor: pointer; }
</style>
