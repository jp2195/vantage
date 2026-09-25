// Test-environment setup, run before every test file (vite.config.ts,
// test.setupFiles). It exists for exactly one reason, and that reason is a
// gap in the environment rather than anything about this application.
//
// jsdom implements no fetch, so vitest leaves Node's own Request and fetch
// in place. Node's Request requires an ABSOLUTE url and throws on a
// relative one -- and a relative url is exactly what the generated client
// emits, because api/openapi.yaml declares `servers: [- url: /]`, so
// client.gen.ts sets no baseUrl and @hey-api/client-fetch's buildUrl falls
// back to `(baseUrl ?? '') + path`. In a browser that is correct and is the
// whole point: the SPA and the API are the same origin, served by the same
// Go binary. Here it throws before any stubbed fetch is ever reached.
//
// Giving the client the jsdom document's own origin reproduces what the
// browser does with that relative url rather than changing it: same origin,
// same path, one absolute url instead of a resolved one. Nothing in
// src/api/client.ts sets baseUrl, so this survives configureClient.
import { client } from './src/api/generated/client.gen'

client.setConfig({ baseUrl: window.location.origin })
