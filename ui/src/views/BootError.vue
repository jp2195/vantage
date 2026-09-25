<script setup lang="ts">
// What the operator sees when the app cannot reach GET /v1/auth/config,
// which is the first thing it asks for and the one call it cannot boot
// without: a restarting daemon, an ingress answering 502, a cold start.
//
// Before this existed the failure rendered NOTHING. resolveAuth's rejection
// left auth undefined, both branches in App.vue were false, and Vue routed
// the rejection to console.error -- no unhandled rejection, no on-screen
// signal, a blank page forever. For a console whose whole purpose is to be
// the thing you open when something is already wrong, that is the worst
// available failure mode: it looks identical to a broken build.
//
// So it says what failed, quotes the status verbatim rather than
// summarizing it, and offers the one action that can help. Retry re-runs
// the whole boot rather than reloading the page, because a daemon coming
// back up is the common case and a reload would lose nothing but cost the
// bundle again.
defineProps<{ message: string }>()
defineEmits<{ (e: 'retry'): void }>()
</script>

<template>
  <div class="boot-error" role="alert">
    <h1>Cannot reach the API</h1>
    <p>
      Vantage asked this daemon how to authenticate and got no usable answer.
      It is reachable at
      <code class="mono">/v1/auth/config</code>
      when the daemon is up.
    </p>
    <p class="detail mono">{{ message }}</p>
    <button type="button" @click="$emit('retry')">Try again</button>
  </div>
</template>

<style scoped>
.boot-error { max-width: 520px; margin: 64px auto; display: flex; flex-direction: column; gap: 12px; font-family: var(--font-ui); }
h1 { font: 600 19px var(--font-ui); color: var(--ink); margin: 0; }
p { font-size: 12px; color: var(--muted); line-height: 1.5; margin: 0; }
code { font-size: 11px; background: var(--surface-2); padding: 2px 5px; border-radius: 4px; }
.detail {
  font-size: 12px;
  color: var(--bad-2);
  background: var(--bad-tint);
  border: 1px solid var(--bad);
  border-radius: 6px;
  padding: 9px 11px;
}
button { background: var(--ink); color: var(--on-dark); border: 0; border-radius: 6px; padding: 9px; font: 500 12px var(--font-ui); cursor: pointer; align-self: flex-start; padding-inline: 16px; }
</style>
