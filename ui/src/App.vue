<script setup lang="ts">
// The boot sequence: learn the daemon's auth mode before rendering anything
// that could call a /v1 endpoint. `auth` stays undefined until resolveAuth
// settles, so neither TokenPrompt nor AppShell renders on a guess about
// whether a credential is needed -- only once the daemon has actually said.
import { onMounted, ref } from 'vue'
import { resolveAuth, type AuthState } from '@/api/auth'
import { configureClient } from '@/api/client'
import AppShell from '@/components/AppShell.vue'
import BootError from '@/views/BootError.vue'
import TokenPrompt from '@/views/TokenPrompt.vue'

const auth = ref<AuthState>()
// Set when boot() could not reach the daemon. It is a third state, not the
// absence of the other two: "we do not know yet" and "we asked and could
// not find out" have to look different on screen, and before this they did
// not -- both rendered nothing at all. See BootError.vue.
const bootError = ref<string>()

async function boot() {
  bootError.value = undefined
  try {
    const state = await resolveAuth()
    auth.value = state
    if (!state.needsCredential) {
      configureClient(state)
    }
  } catch (err) {
    // The message, not the error object: it is going on screen, and
    // resolveAuth's is already written for an operator ("auth config: HTTP
    // 502"). A network-level failure has no status and arrives here as
    // whatever the fetch layer threw, which is still better on screen than
    // a blank page.
    bootError.value = err instanceof Error ? err.message : String(err)
  }
}

onMounted(boot)

// The operator just typed a token into TokenPrompt, which already stored
// it (storeCredential). This is the only other place a credential reaches
// the client: once it is folded into auth, configureClient runs and every
// later screen's generated call carries it.
function onAuthenticated(token: string) {
  if (!auth.value) return
  const state: AuthState = { ...auth.value, credential: token, needsCredential: false }
  auth.value = state
  configureClient(state)
}
</script>

<template>
  <!-- bootError is tested first: a retry that fails again must show the
       failure rather than whatever the last successful boot left in auth. -->
  <BootError v-if="bootError" :message="bootError" @retry="boot" />
  <TokenPrompt v-else-if="auth?.needsCredential" @authenticated="onAuthenticated" />
  <AppShell v-else-if="auth" :mode="auth.mode">
    <RouterView />
  </AppShell>
</template>
