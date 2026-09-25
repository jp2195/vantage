import { describe, expect, it } from 'vitest'
import { formatGoDuration, parseGoDuration } from './goDuration'

describe('parseGoDuration', () => {
  it('parses the shapes the daemon actually sends', () => {
    // Go's Duration.String() always writes the smaller units, which is why
    // meta.churn_bucket reads "30m0s" rather than "30m".
    expect(parseGoDuration('30m0s')).toBe(30 * 60_000)
    expect(parseGoDuration('2m')).toBe(2 * 60_000)
    expect(parseGoDuration('1h0m0s')).toBe(3_600_000)
    expect(parseGoDuration('1h30m0s')).toBe(90 * 60_000)
    expect(parseGoDuration('45s')).toBe(45_000)
  })

  // undefined, never a fallback: a caller that cannot learn the width must
  // say so rather than draw a bar of invented duration.
  it('answers undefined for anything it does not recognize', () => {
    for (const bad of ['', 'five minutes', '30', 'PT30M', '0']) {
      expect(parseGoDuration(bad)).toBeUndefined()
    }
  })
})

describe('formatGoDuration', () => {
  // "30m0s" is a serialization, not a label. A person reads "30 min", and a
  // screen reader says it out loud.
  it('writes the shapes the daemon sends the way a person reads them', () => {
    expect(formatGoDuration('30m0s')).toBe('30 min')
    expect(formatGoDuration('2m')).toBe('2 min')
    expect(formatGoDuration('1h0m0s')).toBe('1 hr')
    expect(formatGoDuration('1h30m0s')).toBe('1 hr 30 min')
    expect(formatGoDuration('45s')).toBe('45 sec')
    expect(formatGoDuration('1m30s')).toBe('1 min 30 sec')
  })

  // Unchanged, never a guess or a dash: what the server actually said is the
  // honest answer when this cannot improve on it.
  it('hands back anything it cannot parse exactly as it arrived', () => {
    for (const bad of ['', 'five minutes', '30', 'PT30M', '0']) {
      expect(formatGoDuration(bad)).toBe(bad)
    }
  })
})
