import { afterEach, describe, expect, it } from 'vitest'
import { defineComponent, h, nextTick } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import { useRememberedScope } from './rememberedScope'

const Screen = defineComponent({
  setup() {
    useRememberedScope('routes', ['router', 'peer'])
    return () => h('div')
  },
})
const Elsewhere = defineComponent({ render: () => h('div') })

async function app(at: string) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/routes', component: Screen },
      { path: '/peers', component: Elsewhere },
    ],
  })
  await router.push(at)
  await router.isReady()
  mount(defineComponent({ render: () => h(RouterView) }), { global: { plugins: [router] } })
  await flushPromises()
  return router
}

afterEach(() => sessionStorage.clear())

describe('useRememberedScope', () => {
  it('opens a bare visit on the last scope asked in this tab', async () => {
    await app('/routes?router=172.26.0.9&peer=10.0.0.80')
    const router = await app('/routes')
    expect(router.currentRoute.value.query).toEqual({ router: '172.26.0.9', peer: '10.0.0.80' })
  })

  it('lets a URL that names a scope win over the remembered one', async () => {
    await app('/routes?router=172.26.0.9&peer=10.0.0.80')
    const router = await app('/routes?router=172.26.0.4')
    expect(router.currentRoute.value.query).toEqual({ router: '172.26.0.4' })
  })

  it('forgets the scope when the picker is cleared', async () => {
    const router = await app('/routes?router=172.26.0.9&peer=10.0.0.80')
    await router.replace({ query: {} })
    await flushPromises()
    const again = await app('/routes')
    expect(again.currentRoute.value.query).toEqual({})
  })

  it('does not record another screen’s query as its own', async () => {
    const router = await app('/routes?router=172.26.0.9&peer=10.0.0.80')
    await router.push('/peers?router=172.26.0.4')
    await nextTick()
    const again = await app('/routes')
    expect(again.currentRoute.value.query).toEqual({ router: '172.26.0.9', peer: '10.0.0.80' })
  })

  it('takes only its own keys, as strings, from storage', async () => {
    sessionStorage.setItem('vantage.scope.routes', JSON.stringify({ router: '172.26.0.9', peer: 7, evil: 'x' }))
    const router = await app('/routes')
    expect(router.currentRoute.value.query).toEqual({ router: '172.26.0.9' })
  })

  it('ignores storage it cannot parse', async () => {
    sessionStorage.setItem('vantage.scope.routes', '{not json')
    const router = await app('/routes')
    expect(router.currentRoute.value.query).toEqual({})
  })
})
