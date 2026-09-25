// defineConfig comes from vitest/config rather than plain vite: Vite's own
// UserConfig has no `test` field, so vue-tsc --noEmit would fail on the
// `test: { environment: 'jsdom' }` block below if this imported from
// 'vite'. vitest/config re-exports Vite's config merged with vitest's own
// test-config type, which is exactly the shape this file needs, and it is
// vitest's documented way to type a combined vite+vitest config -- see
// https://vitest.dev/config/ ("Configuring Vitest" / "vitest/config").
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    // Straight into the Go package that embeds it. go:embed cannot reach
    // outside its own directory, so the build output has to land there
    // rather than be copied in by a step someone can forget.
    outDir: fileURLToPath(new URL('../webui/dist', import.meta.url)),
    emptyOutDir: true,
  },
  server: {
    // Same-origin in development too. Without this the dev server would be
    // cross-origin to the API and the project would need CORS -- which it
    // otherwise never needs, because production serves both from one binary.
    proxy: { '/v1': 'http://127.0.0.1:9473' },
  },
  test: {
    environment: 'jsdom',
    // Every screen in this app renders timestamps through
    // toLocaleTimeString, so a test that asserts a rendered clock asserts
    // the MACHINE's timezone unless one is pinned -- "14:00" has to mean the
    // same thing on a laptop and in CI.
    //
    // DELIBERATELY NOT UTC, which is the obvious pin and the useless one:
    // under it, local wall-clock time and the instant's own UTC time are the
    // same string, so code that conflates the two passes. ChurnChart's axis
    // walks its gridlines from the LOCAL midnight the window opens in, and
    // that choice is only observable from a zone with an offset -- align it
    // to the epoch instead and a six-hour gridline reads 20:00 here and
    // 00:00 under a UTC pin, with no test able to tell.
    //
    // A whole-hour offset, so a two-hour gridline still lands on an even
    // hour; the archive's own fixtures are September, clear of both DST
    // transitions.
    env: { TZ: 'America/New_York' },
    // See vitest.setup.ts: jsdom ships no fetch, so the generated client's
    // relative urls have to be given an origin before Node's Request will
    // accept them. Nothing else belongs in there.
    setupFiles: ['./vitest.setup.ts'],
  },
})
