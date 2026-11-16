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
  <main class="gate">
    <div class="card" role="alert">
      <div class="brand">
        <span class="mark" aria-hidden="true">V</span>
        <span class="wordmark">Vantage</span>
      </div>
      <h1>Cannot reach the API</h1>
      <p class="lede">
        Vantage asked this daemon how to authenticate and got no usable answer.
        It is reachable at
        <code class="mono">/v1/auth/config</code>
        when the daemon is up.
      </p>
      <p class="detail mono">{{ message }}</p>
      <button type="button" class="btn primary retry" @click="$emit('retry')">Try again</button>
    </div>
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
  width: 100%; max-width: 480px; box-sizing: border-box;
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
code { font-size: var(--text-sm); background: var(--surface-2); padding: 2px 5px; border-radius: var(--radius-sm); }
.detail {
  margin: 0; font-size: var(--text-sm); color: var(--bad-2); background: var(--bad-tint);
  border: 1px solid var(--bad-line); border-radius: var(--radius-md); padding: 10px 12px; overflow-wrap: anywhere;
}
.retry { align-self: flex-start; min-height: 36px; }
</style>
