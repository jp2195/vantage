import type { RouteLocationRaw } from 'vue-router'

/**
 * Where the sidebar's jump box sends a typed term, or undefined when the
 * term is not one of the shapes it knows.
 *
 * Every answer is a Looking glass search, because that is the screen that
 * answers "who sees this?" for all four shapes. The term is classified by
 * its form alone -- nothing is fetched to decide -- so the jump is instant
 * and works before any screen has loaded data:
 *
 *   10.91.0.0/16, 2001:db8::/32   an exact prefix
 *   10.255.0.2, 2001:db8::1       an address: every prefix covering it
 *   AS65002, as65002, 65002       an origin AS
 *   65002:100, 65002:1:2          a standard or large community
 *
 * A bare number is an ASN, not an IPv4 address: "65002" written by an
 * operator means the AS. The Looking glass parses the term again with
 * Number() for an ASN, so the "AS" prefix is stripped here.
 */
export function jumpTarget(raw: string): RouteLocationRaw | undefined {
  const term = raw.trim()
  if (!term) return undefined

  const asn = term.match(/^as\s*(\d{1,10})$/i) ?? term.match(/^(\d{1,10})$/)
  if (asn) return lookingGlass({ mode: 'asn', q: asn[1] })

  // Two or three all-digit parts. Checked before IPv6, which it never
  // matches: an IPv6 address has a "::" or a hex letter or more parts.
  if (/^\d{1,10}:\d{1,10}(:\d{1,10})?$/.test(term)) {
    return lookingGlass({ mode: 'community', q: term })
  }

  const [addr, len, ...rest] = term.split('/')
  if (rest.length || !isAddress(addr)) return undefined
  if (len === undefined) return lookingGlass({ mode: 'prefix', q: addr, covers: '1' })
  const bits = Number(len)
  const max = addr.includes(':') ? 128 : 32
  if (!/^\d{1,3}$/.test(len) || bits > max) return undefined
  return lookingGlass({ mode: 'prefix', q: term })
}

function lookingGlass(query: Record<string, string>): RouteLocationRaw {
  return { path: '/looking-glass', query }
}

function isAddress(s: string): boolean {
  const v4 = s.split('.')
  if (v4.length === 4) {
    return v4.every((o) => /^\d{1,3}$/.test(o) && Number(o) <= 255)
  }
  // IPv6: hex groups and at most one "::". The daemon validates for real;
  // this only has to tell an address from a word.
  if (!s.includes(':') || !/^[0-9a-f:.]+$/i.test(s)) return false
  const doubles = s.split('::').length - 1
  if (doubles > 1) return false
  const groups = s.split(':').filter(Boolean)
  return groups.length <= 8 && groups.every((g) => g.length <= 4 || g.includes('.'))
}
