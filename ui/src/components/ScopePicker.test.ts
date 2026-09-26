import { describe, expect, it } from 'vitest'
import { declarationsOf } from '@/test-support/styleText'

// A <select> is as wide as its longest option, and a peer option reads
// "10.0.0.80 · AS4200000002 · 2 collectors". Laid out on one line that
// never wrapped, the Peer and Collector controls ran 199px past the right
// edge of a 390px phone and the whole page scrolled sideways, on Routes
// and on Session history. Measured in a browser; jsdom applies no
// component stylesheet, so these read the rules themselves.
describe('ScopePicker layout', () => {
  const of = (selector: string) => declarationsOf('components/ScopePicker.vue', selector)

  it('wraps its controls onto a second line rather than past the screen edge', () => {
    expect(of('.picker')).toContain('flex-wrap: wrap')
  })

  it('lets a control be narrower than its longest option', () => {
    expect(of('label')).toContain('max-width: 100%')
    expect(of('select')).toContain('max-width: 100%')
  })
})
