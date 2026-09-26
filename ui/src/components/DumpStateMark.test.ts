import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import DumpStateMark from './DumpStateMark.vue'
import routes from '@/api/fixtures/routes.json'
import { declarationsOf } from '@/test-support/styleText'
import rib from '@/api/fixtures/rib-unicast.json'

// All three values come off captured responses rather than the enum in
// api/openapi.yaml: rib-unicast.json's rows are `dumping`, routes.json's
// unicast[3] is a real `unknown` (a row whose peer is not up -- see
// query/query.go's dumpStateExpr), and its other five are `complete`. The
// third value was claimed to have no capture anywhere in the fixtures; it
// does.
describe('DumpStateMark', () => {
  it('marks nothing when the dump is complete', () => {
    const complete = routes.data.unicast[0].dump_state
    expect(complete).toBe('complete')
    const w = mount(DumpStateMark, { props: { state: complete } })
    expect(w.text()).toBe('')
  })

  it('marks a still-dumping row provisional', () => {
    const dumping = rib.data[0].dump_state
    expect(dumping).toBe('dumping')
    const w = mount(DumpStateMark, { props: { state: dumping } })
    expect(w.text()).toMatch(/provisional/i)
  })

  it('marks an unknown dump state distinctly, not as settled and not as dumping', () => {
    // api/openapi.yaml: dump_state exists "because an empty result and a
    // partial result are otherwise indistinguishable". `unknown` is the
    // third value and it is NOT a quieter `complete`: query/query.go
    // resolves it when the contributing peer is not up, or when the
    // collector holds no session state for that RIB view at all -- so
    // whether the family's table was fully delivered is a question with no
    // answer, which is exactly what must not render as "yes".
    const unknown = routes.data.unicast[3].dump_state
    expect(unknown).toBe('unknown')
    const w = mount(DumpStateMark, { props: { state: unknown } })
    expect(w.text()).not.toBe('')
    expect(w.text()).not.toMatch(/provisional/i)
    expect(w.text()).toMatch(/unknown/i)
  })

  it('marks a value it has never seen rather than treating it as complete', () => {
    // Keyed on `!== 'complete'`, not on `=== 'dumping'`. A fourth value
    // added to the enum later must not silently render as settled fact.
    const w = mount(DumpStateMark, { props: { state: 'reconciling' } })
    expect(w.text()).not.toBe('')
  })
})

// The mark follows the prefix with no space between them -- Vue drops the
// newline in the template -- so as a plain inline span it gave the line no
// place to break, and "10.255.0.2/32provisional" overflowed the Prefix
// column on Routes below 1440, and on the Looking glass at 1440 too. As an
// inline-block it is an atomic inline, which a line may break before.
// jsdom applies no component stylesheet, so this reads the rule itself.
it('is an atomic inline, so the line may break before it', () => {
  expect(declarationsOf('components/DumpStateMark.vue', '.mark')).toContain('display: inline-block')
})
