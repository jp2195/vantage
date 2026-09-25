import { describe, expect, it } from 'vitest'
import { reasonDisplay } from './eventReason'

// The two notations treated as one contract: a known code renders as
// "3 · remote system closed, notification follows" and an unregistered one as
// "99 (no name for this code)". Both carry the NUMBER, which is the point --
// showing the name alone drops the half a reader needs to check the decoding,
// and a bare number for an unregistered code puts two different notations in
// one column.
describe('reasonDisplay', () => {
  it('names a known code beside its number', () => {
    expect(
      reasonDisplay({ kind: 'down', down_reason: 3, reason_name: 'remote system closed, notification follows' }),
    ).toBe('3 · remote system closed, notification follows')
  })

  it('says an unregistered code has no name rather than showing a bare number', () => {
    expect(reasonDisplay({ kind: 'down', down_reason: 99, reason_name: '' })).toBe(
      '99 (no name for this code)',
    )
  })

  // A view_lost's down_reason is 0 because the router made no statement at
  // all, never because the reason was "0". Rendering that 0 would report an
  // absence as a code.
  it.each(['view_lost', 'up', 'unspecified'] as const)('shows no reason for a %s', (kind) => {
    expect(reasonDisplay({ kind, down_reason: 0, reason_name: '' })).toBe('—')
  })

  // The kind decides, not the presence of a reason. A non-down row that
  // somehow carried a populated reason must still render none: this is the
  // guard against a future writer defaulting the column.
  it('shows no reason for a non-down even when one is present', () => {
    expect(reasonDisplay({ kind: 'view_lost', down_reason: 3, reason_name: 'remote system closed' })).toBe('—')
  })
})
