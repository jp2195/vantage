import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import App from './App.vue'
import AppShell from './components/AppShell.vue'
import BootError from './views/BootError.vue'
import TokenPrompt from './views/TokenPrompt.vue'
import { client } from './api/generated/client.gen'
import { listRouters } from './api/generated'

// This is the permanent replacement for the earlier manual check ("open a
// browser, confirm the token prompt appears, a pasted token loads the
// shell"): a one-time eyeball check protects nothing the next time this
// code changes, and nothing here can drive a real browser reliably. This
// pins the same guarantee at the component level, and keeps checking it on
// every run.
//
// It asserts what App RENDERS and what it DID, not only which component
// mounted. The earlier version checked the component alone, and the boot
// sequence's whole job -- handing the resolved credential to the generated
// client -- could be deleted with every case still green.
//
// A router is installed only because AppShell's template holds a
// RouterView and RouterLink; which paths exist is router.ts's concern, not
// this test's, so the table below is a single catch-all rather than a copy
// of the real one.
function testRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/:pathMatch(.*)*', component: { template: '<div />' } }],
  })
}

function mountApp() {
  // FleetChrome is stubbed for the same reason the router table is a
  // catch-all: it reads /v1/routers through Colada and would make every
  // boot-sequence case depend on an active Pinia. What it renders is
  // FleetChrome.test.ts's subject.
  return mount(App, { global: { plugins: [testRouter()], stubs: { FleetChrome: true } } })
}

// The daemon's answer to GET /v1/auth/config, with the Content-Type it
// really sends: the generated client parses by content type, so a stub
// without one is answered as text and never reaches the mode branch.
function stubAuthConfig(mode: string) {
  const fetchStub = vi.fn(async () => new Response(JSON.stringify({ mode }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  }))
  vi.stubGlobal('fetch', fetchStub)
  return fetchStub
}

beforeEach(() => {
  sessionStorage.clear()
  // The generated client is a module singleton, so a credential configured
  // by one case would otherwise be visible to the next.
  client.setConfig({ auth: undefined })
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('App boot sequence', () => {
  it('renders TokenPrompt when the daemon reports it needs a credential', async () => {
    stubAuthConfig('token')
    const wrapper = mountApp()
    await flushPromises()

    expect(wrapper.findComponent(TokenPrompt).exists()).toBe(true)
    expect(wrapper.findComponent(AppShell).exists()).toBe(false)
  })

  it('renders AppShell when the daemon reports no credential is needed', async () => {
    stubAuthConfig('none')
    const wrapper = mountApp()
    await flushPromises()

    expect(wrapper.findComponent(AppShell).exists()).toBe(true)
    expect(wrapper.findComponent(TokenPrompt).exists()).toBe(false)
  })

  // The unauthenticated banner has to survive the trip through App, not
  // just through AppShell in isolation: App is what decides which mode the
  // shell is told about, and telling it the wrong one would silence the
  // warning on exactly the deployment that needs it.
  it('shows the unauthenticated warning end to end in none mode', async () => {
    stubAuthConfig('none')
    const wrapper = mountApp()
    await flushPromises()

    expect(wrapper.text()).toContain('unauthenticated')
  })

  // The boot-time configureClient(state). Deleting it left every case
  // above green -- AppShell still mounted, the app still looked fine, and
  // every gated request the operator went on to make went out with no
  // credential and came back 401. The only assertion that can see it is
  // one that makes a real generated call and reads the header.
  it('hands a stored credential to the generated client at boot', async () => {
    sessionStorage.setItem('vantage.token', 'tok-stored')
    stubAuthConfig('token')
    const wrapper = mountApp()
    await flushPromises()

    // token mode with a token already in storage: no prompt, straight to
    // the shell, and the client is expected to be carrying the credential.
    expect(wrapper.findComponent(AppShell).exists()).toBe(true)

    const seen: Request[] = []
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      seen.push(req)
      return new Response('{"data":[],"meta":{}}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }))
    await listRouters()

    expect(seen[0].headers.get('Authorization')).toBe('Bearer tok-stored')
  })

  // The credential typed into the prompt takes the same road.
  it('hands a typed credential to the generated client', async () => {
    stubAuthConfig('token')
    const wrapper = mountApp()
    await flushPromises()

    await wrapper.find('input').setValue('tok-typed')
    await wrapper.find('form').trigger('submit')
    await flushPromises()

    expect(wrapper.findComponent(AppShell).exists()).toBe(true)

    const seen: Request[] = []
    vi.stubGlobal('fetch', vi.fn(async (req: Request) => {
      seen.push(req)
      return new Response('{"data":[],"meta":{}}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    }))
    await listRouters()

    expect(seen[0].headers.get('Authorization')).toBe('Bearer tok-typed')
  })
})

describe('App boot failure', () => {
  // The failure this screen originally shipped with: resolveAuth rejects, auth stays
  // undefined, both template branches are false, and the operator gets a
  // permanently blank page with the reason in a console they are not
  // looking at.
  it('renders the error instead of nothing when /v1/auth/config is unreachable', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('bad gateway', { status: 502 })))
    const wrapper = mountApp()
    await flushPromises()

    expect(wrapper.findComponent(BootError).exists()).toBe(true)
    expect(wrapper.findComponent(AppShell).exists()).toBe(false)
    expect(wrapper.findComponent(TokenPrompt).exists()).toBe(false)
    // Something readable, and the status the daemon answered with: "it is
    // broken" is not actionable, "HTTP 502" points at the ingress.
    expect(wrapper.text()).toContain('502')
  })

  // A rejected fetch -- DNS, connection refused, a daemon that is not
  // listening yet -- has no status at all and must not fall through the
  // catch into the same blank page.
  it('renders the error when the request never completes', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    const wrapper = mountApp()
    await flushPromises()

    expect(wrapper.findComponent(BootError).exists()).toBe(true)
  })

  // And the retry actually retries: a daemon that was restarting comes
  // back, the operator presses the button, and the app boots without a
  // page reload.
  it('boots on retry once the daemon answers', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('bad gateway', { status: 502 })))
    const wrapper = mountApp()
    await flushPromises()
    expect(wrapper.findComponent(BootError).exists()).toBe(true)

    stubAuthConfig('none')
    await wrapper.findComponent(BootError).find('button').trigger('click')
    await flushPromises()

    expect(wrapper.findComponent(BootError).exists()).toBe(false)
    expect(wrapper.findComponent(AppShell).exists()).toBe(true)
  })
})
