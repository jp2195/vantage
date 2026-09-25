import { describe, expect, it } from 'vitest'
import { formatCount } from './formatCount'

describe('formatCount', () => {
  // U+2009 THIN SPACE, not a comma and not an ASCII space. A comma is a
  // decimal separator in half the world, and these numbers are read by
  // operators everywhere; the thin space is also what this project's own
  // reference values use (1 284, 214 509, 992 411).
  it('groups thousands with a thin space, never a comma', () => {
    expect(formatCount(1208)).toBe('1 208')
    expect(formatCount(992411)).toBe('992 411')
    expect(formatCount(0)).toBe('0')
    expect(formatCount(999)).toBe('999')
  })

  it('groups every three digits, not only the first thousand', () => {
    expect(formatCount(1500644)).toBe('1 500 644')
  })
})
