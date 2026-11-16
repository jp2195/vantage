<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, type RouteLocationRaw } from 'vue-router'
import type { AuthMode } from '@/api/auth'
import FleetChrome from './FleetChrome.vue'
import JumpBox from './JumpBox.vue'

defineProps<{ mode: AuthMode }>()

/**
 * The nav, grouped by the question a screen answers. Eleven flat tabs in one
 * bar mixed health, inventory, routing and history, and on a laptop the bar
 * wrapped. Groups also give each screen a place in the breadcrumb.
 *
 * `icon` is one SVG path, drawn at 16px in currentColor.
 */
const groups: { label: string; items: { to: string; label: string; icon: string }[] }[] = [
  {
    label: 'Overview',
    items: [
      { to: '/monitor', label: 'Monitor', icon: 'M1.5 8h3l2-4.5 3 9 2-4.5h3' },
      { to: '/collectors', label: 'Collectors', icon: 'M2 2.5h12v4.5H2zM2 9h12v4.5H2zM4.5 4.75h.1M4.5 11.25h.1' },
    ],
  },
  {
    label: 'Inventory',
    items: [
      { to: '/routers', label: 'Routers', icon: 'M2.5 8a5.5 5.5 0 1 0 11 0a5.5 5.5 0 1 0-11 0M5 8h6M9 6l2 2-2 2' },
      { to: '/peers', label: 'Peers', icon: 'M1.8 8a2.2 2.2 0 1 0 4.4 0a2.2 2.2 0 1 0-4.4 0M9.8 8a2.2 2.2 0 1 0 4.4 0a2.2 2.2 0 1 0-4.4 0M6.2 8h3.6' },
    ],
  },
  {
    label: 'Routing',
    items: [
      { to: '/looking-glass', label: 'Looking glass', icon: 'M2.5 7a4.5 4.5 0 1 0 9 0a4.5 4.5 0 1 0-9 0M10.5 10.5L14 14' },
      { to: '/routes', label: 'Routes', icon: 'M2 4h12M2 8h12M2 12h8' },
      { to: '/topology', label: 'AS paths', icon: 'M1.7 12.5a1.8 1.8 0 1 0 3.6 0a1.8 1.8 0 1 0-3.6 0M10.7 3.5a1.8 1.8 0 1 0 3.6 0a1.8 1.8 0 1 0-3.6 0M10.7 12.5a1.8 1.8 0 1 0 3.6 0a1.8 1.8 0 1 0-3.6 0M5 11.3l6-6.5M5.3 12.5h5.4' },
      { to: '/link-state', label: 'Link state', icon: 'M1.4 3.5a1.6 1.6 0 1 0 3.2 0a1.6 1.6 0 1 0-3.2 0M11.4 5a1.6 1.6 0 1 0 3.2 0a1.6 1.6 0 1 0-3.2 0M6.4 12.5a1.6 1.6 0 1 0 3.2 0a1.6 1.6 0 1 0-3.2 0M4.5 4l7 .8M3.8 5l3.4 6M12.2 6.4l-3.3 4.8' },
    ],
  },
  {
    label: 'History',
    items: [
      { to: '/events', label: 'Events', icon: 'M3 3h10M3 8h10M3 13h10' },
      { to: '/session-history', label: 'Session history', icon: 'M2 8a6 6 0 1 0 12 0a6 6 0 1 0-12 0M8 4.8V8l2.2 1.6' },
    ],
  },
]

// Optional: AppShell's own tests mount it without a router.
const route = useRoute() as ReturnType<typeof useRoute> | undefined

/** Active for the screen itself and anything under it (/peers/:router/:peer). */
function isActive(to: string): boolean {
  const path = route?.path ?? ''
  return path === to || path.startsWith(`${to}/`)
}

/**
 * The trail above a screen that has a parent, from its route's
 * `meta.crumbs`. A top-level screen has none: the sidebar already says
 * where it is, and a one-item trail would only repeat the title.
 */
const crumbs = computed<{ label: string; to?: RouteLocationRaw }[]>(() => {
  const make = route?.meta.crumbs
  return typeof make === 'function' ? make(route!) : []
})

// The phone layout folds the nav behind a menu button. Following a link
// closes it, or the screen you asked for would open hidden under the menu.
const menuOpen = ref(false)
watch(
  () => route?.fullPath,
  () => (menuOpen.value = false),
)

/**
 * Folded to a rail of icons, for the screens whose tables want every pixel.
 * Remembered per browser: a convenience, so storage that is blocked or
 * cleared only means the sidebar starts open.
 */
const RAIL_KEY = 'vantage.sidebar'
function readRail(): boolean {
  try {
    return localStorage.getItem(RAIL_KEY) === 'rail'
  } catch {
    return false
  }
}
const rail = ref(readRail())
watch(rail, (v) => {
  try {
    if (v) localStorage.setItem(RAIL_KEY, 'rail')
    else localStorage.removeItem(RAIL_KEY)
  } catch {
    /* storage blocked: the choice lasts until reload */
  }
})
</script>

<template>
  <div class="shell" :class="{ rail }">
    <!-- The column carries the chrome's color the full height of the page;
         the aside inside it sticks to the viewport. -->
    <div class="side-col">
    <aside class="side" :class="{ open: menuOpen }">
      <div class="brand-row">
        <RouterLink to="/monitor" class="brand">
          <span class="mark" aria-hidden="true">V</span>
          <span class="wordmark">Vantage</span>
        </RouterLink>
        <!-- The unauthenticated banner is chrome, not a dismissible toast:
             an operator should not be able to lose track of the fact that
             this deployment is open to anyone who can reach it. -->
        <span v-if="mode === 'none'" class="open-warning" title="This deployment requires no credential">
          <span class="badge-text">unauthenticated</span>
          <span class="badge-short" aria-hidden="true">!</span>
        </span>
        <button
          type="button"
          class="menu"
          :aria-expanded="menuOpen"
          aria-controls="primary-nav"
          @click="menuOpen = !menuOpen"
        >
          <svg width="18" height="18" viewBox="0 0 16 16" aria-hidden="true">
            <path v-if="menuOpen" d="M3.5 3.5l9 9M12.5 3.5l-9 9" />
            <path v-else d="M2 4h12M2 8h12M2 12h12" />
          </svg>
          <span class="sr-only">Menu</span>
        </button>
      </div>

      <div id="primary-nav" class="drawer">
        <JumpBox class="jump" @summon="rail = false" />

        <nav class="nav" aria-label="Primary">
          <div v-for="g in groups" :key="g.label" class="group">
            <p class="group-label">{{ g.label }}</p>
            <RouterLink
              v-for="item in g.items"
              :key="item.to"
              :to="item.to"
              class="nav-item"
              :class="{ on: isActive(item.to) }"
              :aria-current="isActive(item.to) ? 'page' : undefined"
              :title="rail ? item.label : undefined"
            >
              <svg class="icon" width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
                <path :d="item.icon" />
              </svg>
              <span class="label">{{ item.label }}</span>
            </RouterLink>
          </div>
        </nav>

        <div class="foot">
          <FleetChrome />
          <!-- The table-states page is a design reference, not an operator
               screen, so it sits down here rather than among the screens. -->
          <RouterLink to="/states" class="nav-item ref" :class="{ on: isActive('/states') }">
            Table states reference
          </RouterLink>
          <button type="button" class="nav-item fold" :aria-pressed="rail" :title="rail ? 'Expand sidebar' : undefined" @click="rail = !rail">
            <svg class="icon" width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
              <path :d="rail ? 'M6 4l4 4-4 4' : 'M10 4L6 8l4 4'" />
            </svg>
            <span class="label">Collapse sidebar</span>
          </button>
        </div>
      </div>
    </aside>
    </div>

    <main class="main">
      <nav v-if="crumbs.length" class="crumbs" aria-label="Breadcrumb">
        <template v-for="(c, i) in crumbs" :key="i">
          <span v-if="i > 0" class="sep" aria-hidden="true">/</span>
          <RouterLink v-if="c.to" :to="c.to">{{ c.label }}</RouterLink>
          <span v-else aria-current="page">{{ c.label }}</span>
        </template>
      </nav>
      <slot />
    </main>
  </div>
</template>

<style scoped>
/* A column of nav beside the screen. The aside is a flex item that sticks
   to the viewport and scrolls on its own, so the nav stays reachable at the
   bottom of a long table. Below 900px it becomes a bar across the top with
   the nav folded behind a menu button. */
.shell { display: flex; min-height: 100vh; }
.side-col { flex: 0 0 228px; background: var(--chrome); }
.side {
  box-sizing: border-box;
  position: sticky; top: 0; height: 100vh; overflow-y: auto;
  background: var(--chrome); color: var(--chrome-text);
  padding: 14px 12px; display: flex; flex-direction: column; gap: 14px;
  scrollbar-width: thin; scrollbar-color: var(--chrome-2) transparent;
}
.main { flex: 1; min-width: 0; }

.brand-row { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
.brand { display: flex; align-items: center; gap: 10px; padding: 2px 6px; text-decoration: none; }
.mark {
  width: 24px; height: 24px; border-radius: var(--radius-md);
  background: var(--accent); color: var(--chrome);
  font: 700 12px var(--font-data); display: grid; place-items: center;
}
.wordmark { font: 600 15px var(--font-ui); color: var(--on-dark); letter-spacing: .01em; }
.open-warning {
  font: 500 var(--text-sm) var(--font-ui); color: var(--chrome);
  background: var(--accent); padding: 2px 8px; border-radius: var(--radius-sm);
}
.menu {
  display: none; margin-left: auto; width: 40px; height: 40px; padding: 0;
  border: 0; border-radius: var(--radius-md); background: transparent;
  color: var(--chrome-text); cursor: pointer; place-items: center;
}
.menu svg { fill: none; stroke: currentColor; stroke-width: 1.6; stroke-linecap: round; }

.drawer { display: flex; flex-direction: column; gap: 14px; flex: 1; }
.nav { display: flex; flex-direction: column; gap: var(--space-3); }
.group { display: flex; flex-direction: column; gap: 1px; }
.group-label {
  margin: 0 0 var(--space-1); padding: 0 10px;
  font: 600 var(--text-label) var(--font-ui); letter-spacing: .08em; text-transform: uppercase;
  color: var(--chrome-text-2);
}
.nav-item {
  display: flex; align-items: center; gap: 10px; min-height: 34px; padding: 0 10px;
  border-radius: var(--radius-md); color: var(--chrome-text-2); text-decoration: none;
  font: 500 var(--text-body) var(--font-ui); white-space: nowrap;
}
.nav-item:hover { background: var(--chrome-2); color: var(--chrome-text); }
.nav-item.on { background: var(--chrome-2); color: var(--on-dark); box-shadow: inset 2px 0 0 var(--focus); }
.icon { flex: none; fill: none; stroke: currentColor; stroke-width: 1.5; stroke-linecap: round; stroke-linejoin: round; }

.foot {
  margin-top: auto; padding-top: var(--space-3); border-top: 1px solid var(--chrome-2);
  display: flex; flex-direction: column; gap: var(--space-2);
}
/* FleetChrome was laid out as a right-aligned row for the old top bar. In a
   column it reads as a short stack of stamps. */
.foot :deep(.cluster) { margin-left: 0; flex-direction: column; align-items: flex-start; justify-content: flex-start; padding: 0 10px; gap: 2px; }
.foot :deep(.meta) { white-space: normal; }
.nav-item.ref { min-height: 28px; font-size: var(--text-sm); }
.fold { border: 0; background: none; cursor: pointer; width: 100%; text-align: left; }
.badge-short { display: none; }

/* The rail: icons only. Labels stay in the DOM, visually hidden, so a
   screen reader still reads "Peers"; a hover title names each icon. */
.rail .side-col { flex-basis: 60px; }
.rail .side { padding-inline: 8px; align-items: center; }
.rail .wordmark, .rail .group-label, .rail .jump, .rail .foot :deep(.cluster), .rail .nav-item.ref { display: none; }
.rail .label { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
.rail .brand-row { justify-content: center; }
.rail .nav-item { justify-content: center; padding: 0; width: 40px; }
.rail .group + .group { padding-top: var(--space-2); border-top: 1px solid var(--chrome-2); }
.rail .foot { align-items: center; }
.rail .badge-text { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
.rail .badge-short { display: inline; }

.crumbs {
  display: flex; flex-wrap: wrap; align-items: center; gap: 6px;
  padding: var(--space-4) 24px 0; font: 400 var(--text-sm) var(--font-ui); color: var(--muted);
}
.crumbs [aria-current] { color: var(--ink); font-family: var(--font-data); }
.crumbs a { font-family: var(--font-data); }
.sep { color: var(--muted-2); }
.sr-only { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }

@media (max-width: 900px) {
  .shell { flex-direction: column; }
  /* The rail is a desktop affordance; a phone has the menu instead. */
  .side-col, .rail .side-col { flex: none; position: sticky; top: 0; z-index: 5; }
  .side { position: static; height: auto; max-height: 100vh; padding: 8px 12px; gap: 0; }
  .rail .side { align-items: stretch; padding-inline: 12px; }
  .rail .wordmark, .rail .group-label, .rail .jump, .rail .foot :deep(.cluster), .rail .nav-item.ref { display: revert; }
  .rail .label, .rail .badge-text { position: static; width: auto; height: auto; overflow: visible; clip: auto; }
  .rail .badge-short, .fold { display: none; }
  .rail .nav-item { justify-content: flex-start; padding: 0 10px; width: auto; }
  .menu { display: grid; }
  .drawer { display: none; padding: 12px 0 8px; }
  .side.open .drawer { display: flex; }
}
</style>
