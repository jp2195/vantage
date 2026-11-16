import { describe, expect, it } from 'vitest'
import { jumpTarget } from './jumpTarget'

const lg = (query: Record<string, string>) => ({ path: '/looking-glass', query })

describe('jumpTarget', () => {
  it.each([
    ['10.91.0.0/16', lg({ mode: 'prefix', q: '10.91.0.0/16' })],
    ['2001:db8::/32', lg({ mode: 'prefix', q: '2001:db8::/32' })],
    ['  10.255.0.2/32 ', lg({ mode: 'prefix', q: '10.255.0.2/32' })],
  ])('sends a prefix to an exact prefix search: %s', (term, want) => {
    expect(jumpTarget(term)).toEqual(want)
  })

  it.each([
    ['10.255.0.2', lg({ mode: 'prefix', q: '10.255.0.2', covers: '1' })],
    ['2001:db8:5549:3::1', lg({ mode: 'prefix', q: '2001:db8:5549:3::1', covers: '1' })],
  ])('sends an address to a covering search: %s', (term, want) => {
    expect(jumpTarget(term)).toEqual(want)
  })

  it.each(['AS65002', 'as65002', 'AS 65002', '65002'])('reads %s as AS65002', (term) => {
    expect(jumpTarget(term)).toEqual(lg({ mode: 'asn', q: '65002' }))
  })

  it.each(['65002:100', '65002:1:2'])('reads %s as a community', (term) => {
    expect(jumpTarget(term)).toEqual(lg({ mode: 'community', q: term }))
  })

  it.each(['', '   ', 'xr-pe1', '10.0.0.0/33', '300.1.1.1', '2001:db8::/129', '1::2::3', '10.0.0.0/8/9'])(
    'declines %j rather than guessing',
    (term) => {
      expect(jumpTarget(term)).toBeUndefined()
    },
  )
})
