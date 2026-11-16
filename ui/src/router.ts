import {
  createRouter,
  createWebHistory,
  type RouteLocationNormalizedLoaded,
  type RouteLocationRaw,
  type RouteRecordRaw,
} from 'vue-router'

declare module 'vue-router' {
  interface RouteMeta {
    /**
     * The breadcrumb trail AppShell draws above a screen that has a parent,
     * ending in the screen itself (no `to`). Top-level screens leave it out.
     */
    crumbs?: (route: RouteLocationNormalizedLoaded) => { label: string; to?: RouteLocationRaw }[]
  }
}

const param = (v: string | string[]) => (Array.isArray(v) ? v[0] : v)

const routes: RouteRecordRaw[] = [
  // AppShell has no nav entry for "/" itself, so a bare visit lands
  // somewhere the nav actually highlights rather than a blank main. Monitor,
  // because it answers the first question anyone opening this has: is
  // collection healthy, and is anything down?
  { path: '/', redirect: '/monitor' },
  {
    path: '/routers',
    name: 'routers',
    component: () => import('@/views/RoutersView.vue'),
  },
  { path: '/peers', name: 'peers', component: () => import('@/views/PeersView.vue') },
  {
    path: '/peers/:router/:peer',
    name: 'peer-detail',
    component: () => import('@/views/PeerDetailView.vue'),
    meta: {
      // Back to every peer, then to this router's peers (PeersView reads
      // ?router=), then this one.
      crumbs: (r) => [
        { label: 'Peers', to: '/peers' },
        { label: param(r.params.router), to: { path: '/peers', query: { router: param(r.params.router) } } },
        { label: param(r.params.peer) },
      ],
    },
  },
  {
    path: '/looking-glass',
    name: 'looking-glass',
    component: () => import('@/views/LookingGlassView.vue'),
  },
  { path: '/routes', name: 'routes', component: () => import('@/views/RoutesView.vue') },
  {
    path: '/topology',
    name: 'topology',
    component: () => import('@/views/TopologyView.vue'),
  },
  {
    path: '/link-state',
    name: 'link-state',
    component: () => import('@/views/LinkStateView.vue'),
  },
  { path: '/events', name: 'events', component: () => import('@/views/EventsView.vue') },
  {
    path: '/session-history',
    name: 'session-history',
    component: () => import('@/views/SessionHistoryView.vue'),
  },
  { path: '/monitor', name: 'monitor', component: () => import('@/views/MonitorView.vue') },
  {
    path: '/collectors',
    name: 'collectors',
    component: () => import('@/views/CollectorsView.vue'),
  },
  // This project's own last nav item, and a design reference rather than an
  // operator's screen: it mounts the real DataTable in each state it can
  // land in. It is in the nav because router.test.ts requires every fixed
  // route to be reachable from it -- an unlinked route is one nobody opens
  // until it has rotted.
  { path: '/states', name: 'states', component: () => import('@/views/StatesView.vue') },
]

export const router = createRouter({
  // webui.Handler() falls back to index.html for any path that is not a
  // real file (see webui/webui.go), which is what makes createWebHistory
  // -- real paths, no #-fragment -- work for a deep link like /peers.
  history: createWebHistory(),
  routes,
})
