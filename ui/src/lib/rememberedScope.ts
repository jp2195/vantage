import { watch } from 'vue'
import { useRoute, useRouter, type LocationQuery } from 'vue-router'

/**
 * Remembers the last scope a query screen was asked about -- Routes' router
 * and peer, Link state's router, protocol and area -- and puts it back when
 * the screen is opened bare from the nav.
 *
 * Without it every visit to these screens opened on an empty "choose a
 * router" box, even seconds after the operator had chosen one: a screen
 * that forgets its own question reads as unfinished.
 *
 * It restores the QUESTION through the URL, by the same `router.replace` a
 * shared link arrives by, and nothing else. The session, the collector and
 * the answer are re-resolved from live data exactly as they are for a
 * pasted link, so a remembered peer whose session has since ended is
 * handled by the code that already handles that case for links.
 *
 * sessionStorage, like the auth token (api/auth.ts): one tab's working
 * memory, gone when the tab closes, never a preference that outlives it.
 * Every access is guarded, because storage can be unavailable (a locked-down
 * browser, a private window) and a screen must not fail for want of a
 * convenience.
 *
 * Clearing the picker forgets the scope: the screens replace the query
 * with an empty one when a scope is abandoned, and that is read here as
 * "start over", not as a visit to restore into.
 */
export function useRememberedScope(screen: string, keys: readonly string[]): void {
  const route = useRoute()
  const router = useRouter()
  const storageKey = `vantage.scope.${screen}`
  const currentPath = route.path

  const pick = (q: LocationQuery): Record<string, string> => {
    const out: Record<string, string> = {}
    for (const k of keys) {
      const v = q[k]
      if (typeof v === 'string' && v) out[k] = v
    }
    return out
  }

  const read = (): Record<string, string> | undefined => {
    try {
      const raw = sessionStorage.getItem(storageKey)
      if (!raw) return undefined
      const parsed: unknown = JSON.parse(raw)
      if (!parsed || typeof parsed !== 'object') return undefined
      // Only the keys this screen asks with, and only strings: storage is
      // input like any other.
      const restored = pick(parsed as LocationQuery)
      return Object.keys(restored).length ? restored : undefined
    } catch {
      return undefined
    }
  }

  const write = (scope: Record<string, string> | undefined) => {
    try {
      if (scope) sessionStorage.setItem(storageKey, JSON.stringify(scope))
      else sessionStorage.removeItem(storageKey)
    } catch {
      // A convenience, not a requirement.
    }
  }

  // Opened bare: put the last question back. A URL that carries any part of
  // a scope is a question someone asked on purpose, and wins.
  // One that arrives carrying a scope -- a shared link, a link from another
  // screen -- is remembered as it stands: the screens push only when the
  // scope CHANGES, so the watch below would never see it.
  const initial = pick(route.query)
  if (Object.keys(initial).length) {
    write(initial)
  } else {
    const restored = read()
    if (restored) router.replace({ query: { ...route.query, ...restored } })
  }

  watch(
    () => route.query,
    (q) => {
      // Only while this screen is the one showing: leaving it changes the
      // query too, and that is not an answer to this screen's question.
      if (route.path !== currentPath) return
      const scope = pick(q)
      write(Object.keys(scope).length ? scope : undefined)
    },
  )
}
