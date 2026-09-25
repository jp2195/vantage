import { describe, expect, it } from 'vitest'
import { routerLabel } from './routerLabel'

describe('routerLabel', () => {
  it('passes a real name through untouched', () => {
    expect(routerLabel('nx-spine1')).toBe('nx-spine1')
  })

  it('names the absence rather than rendering an empty identity cell', () => {
    // The empty string is what a router that sends no Initiation leaves in
    // router_sysname -- a real value, not a gap. A blank cell in a Router
    // column is indistinguishable from a rendering fault.
    expect(routerLabel('')).toBe('(no sysName TLV)')
  })

  it('treats a missing field the same as an empty one', () => {
    // The generated client types several of these as optional, and a
    // response that omitted the key would otherwise render "undefined".
    expect(routerLabel(undefined)).toBe('(no sysName TLV)')
    expect(routerLabel(null)).toBe('(no sysName TLV)')
  })
})
