<script setup lang="ts">
import type { AuthMode } from '@/api/auth'
import FleetChrome from './FleetChrome.vue'

/**
 * The chrome. Presentational on purpose: the one part of it that reads
 * anything is FleetChrome, a child.
 *
 * This component held a `useRouters()` call for one commit, and the cost
 * showed up immediately -- `useQuery` needs an active Pinia, so every test
 * that mounts the shell, including App's boot tests that reach it through
 * the boot sequence, suddenly needed the Colada plugin installed on paths
 * that render before any query context exists.
 */
defineProps<{ mode: AuthMode }>()

const nav = [
  { to: '/routers', label: 'Routers' },
  { to: '/peers', label: 'Peers' },
  { to: '/looking-glass', label: 'Looking glass' },
  { to: '/routes', label: 'Routes' },
  { to: '/topology', label: 'AS paths' },
  { to: '/link-state', label: 'Link state' },
  { to: '/events', label: 'Events' },
  { to: '/session-history', label: 'Session history' },
  { to: '/monitor', label: 'Monitor' },
  { to: '/collectors', label: 'Collectors' },
  { to: '/states', label: 'States' },
]
</script>

<template>
  <div class="shell">
    <header class="bar">
      <span class="mark">V</span>
      <span class="wordmark">Vantage</span>
      <nav class="nav">
        <RouterLink v-for="item in nav" :key="item.to" :to="item.to" class="nav-item">
          {{ item.label }}
        </RouterLink>
      </nav>
      <!-- The unauthenticated banner is chrome, not a dismissible toast:
           an operator should not be able to lose track of the fact that
           this deployment is open to anyone who can reach it. -->
      <span v-if="mode === 'none'" class="open-warning">unauthenticated</span>

      <FleetChrome />
    </header>
    <main><slot /></main>
  </div>
</template>

<style scoped>
/* Wraps rather than squeezes. On a phone the nav and the fleet cluster do
   not fit beside the wordmark, and without wrapping the nav shrank to 0px
   and the cluster ran past the clipped right edge. Wrapped, the nav takes
   its own row and scrolls sideways inside it. The bar takes one row from
   about 1380px wide; below that the fleet cluster moves to a second row of
   its own, and the nav keeps every item on one line. border-box keeps the
   one-row bar 46px tall: the padding only shows once rows stack. */
.bar {
  box-sizing: border-box;
  min-height: 46px;
  background: var(--chrome);
  padding: 8px 16px;
  display: flex;
  flex-wrap: wrap;
  gap: 6px 16px;
  align-items: center;
  overflow: hidden;
  position: sticky;
  top: 0;
  z-index: 5;
}
.mark {
  width: 18px;
  height: 18px;
  border-radius: 4px;
  background: var(--accent);
  color: var(--chrome);
  font: 700 10px var(--font-data);
  display: grid;
  place-items: center;
}
.wordmark { font: 600 13px var(--font-ui); color: var(--chrome-text); letter-spacing: .02em; }
/* A thin scrollbar in the chrome's own colors: once the bar wraps on a
   narrow screen, the nav scrolls sideways and a default light scrollbar
   would sit as a white stripe across the dark bar. */
.nav {
  display: flex; gap: 2px; flex-wrap: nowrap; overflow-x: auto;
  scrollbar-width: thin; scrollbar-color: var(--chrome-2) transparent;
}

.nav-item {
  font: 500 12px var(--font-ui);
  padding: 5px 11px;
  border-radius: 5px;
  color: var(--faint);
  text-decoration: none;
  white-space: nowrap;
}
.nav-item.router-link-active { background: var(--chrome-2); color: var(--on-dark); }
.open-warning {
  margin-left: auto;
  font: 500 11px var(--font-ui);
  color: var(--chrome);
  background: var(--accent);
  padding: 2px 8px;
  border-radius: 4px;
}
</style>
