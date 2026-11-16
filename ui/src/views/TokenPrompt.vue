<script setup lang="ts">
import { ref } from 'vue'
import { storeCredential } from '@/api/auth'

const token = ref('')
const reveal = ref(false)
const emit = defineEmits<{ (e: 'authenticated', token: string): void }>()

function submit() {
  const value = token.value.trim()
  if (!value) return
  storeCredential(value)
  emit('authenticated', value)
}
</script>

<template>
  <!-- The first screen of a token deployment, so it carries the product's
       mark: a bare form on an empty page reads as something that failed to
       load. -->
  <main class="gate">
    <form class="card" @submit.prevent="submit">
      <div class="brand">
        <span class="mark" aria-hidden="true">V</span>
        <span class="wordmark">Vantage</span>
      </div>
      <h1>Sign in with an API token</h1>
      <p class="lede">This deployment authenticates every request with a bearer token. Get one with:</p>
      <pre class="cmd mono">kubectl -n vantage get secret vantage-api \
  -o jsonpath='{.data.lab}' | base64 -d</pre>

      <label class="field">
        <span class="label">API token</span>
        <span class="input-row">
          <input
            v-model="token"
            class="mono"
            :type="reveal ? 'text' : 'password'"
            autocomplete="off"
            spellcheck="false"
            required
            autofocus
          />
          <button
            type="button"
            class="reveal"
            :aria-pressed="reveal"
            :aria-label="reveal ? 'Hide token' : 'Show token'"
            @click="reveal = !reveal"
          >
            {{ reveal ? 'Hide' : 'Show' }}
          </button>
        </span>
      </label>
      <button type="submit" class="btn primary submit" :disabled="!token.trim()">Continue</button>
      <p class="fine">Kept for this browser tab only, and sent only to this daemon.</p>
    </form>
  </main>
</template>

<style scoped>
.gate {
  min-height: 100vh; display: grid; place-items: center; padding: 24px 16px; box-sizing: border-box;
  /* minmax(0, ...): the card may shrink below its widest line, which then
     scrolls inside it, rather than widening a phone page. */
  grid-template-columns: minmax(0, 480px); justify-content: center;
}
.card {
  width: 100%; max-width: 440px; box-sizing: border-box;
  display: flex; flex-direction: column; gap: 14px;
  padding: 28px; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface);
}
.brand { display: flex; align-items: center; gap: 10px; }
.mark {
  width: 26px; height: 26px; border-radius: var(--radius-md); background: var(--accent); color: var(--chrome);
  font: 700 13px var(--font-data); display: grid; place-items: center;
}
.wordmark { font: 600 15px var(--font-ui); color: var(--ink); }
h1 { margin: 4px 0 0; font: 600 var(--text-page) var(--font-ui); letter-spacing: -.01em; color: var(--ink); }
.lede { margin: 0; color: var(--muted); font: 400 var(--text-body) / 1.5 var(--font-ui); }
.cmd {
  margin: 0; padding: 10px 12px; border-radius: var(--radius-md); background: var(--surface-2);
  border: 1px solid var(--line); color: var(--ink-2); font-size: var(--text-sm); line-height: 1.6;
  white-space: pre; overflow-x: auto;
}
.field { display: flex; flex-direction: column; gap: 6px; }
.label { font: 600 var(--text-label) var(--font-ui); letter-spacing: .06em; text-transform: uppercase; color: var(--muted); }
.input-row { display: flex; gap: 8px; }
.input-row input { flex: 1; min-width: 0; padding: 9px 11px; font-size: var(--text-body); }
.reveal {
  flex: none; padding: 0 12px; border: 1px solid var(--line-2); border-radius: var(--radius-md);
  background: var(--surface); color: var(--ink-2); font: 500 var(--text-sm) var(--font-ui); cursor: pointer;
}
.reveal:hover { border-color: var(--muted-2); color: var(--ink); }
.submit { min-height: 38px; width: 100%; }
.fine { margin: 0; color: var(--muted); font: 400 var(--text-sm) var(--font-ui); }
</style>
