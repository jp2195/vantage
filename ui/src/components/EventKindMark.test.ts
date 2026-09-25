import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import EventKindMark from './EventKindMark.vue'

describe('EventKindMark', () => {
  // view_lost is not a down and must never read as one. The label says what
  // happened -- the collector stopped seeing the peer -- rather than
  // borrowing "down"'s word for a different fact.
  it('renders view_lost as the collector losing its view, not as a down', () => {
    const w = mount(EventKindMark, { props: { kind: 'view_lost' } })
    expect(w.text()).toContain('collector lost view')
    expect(w.text()).not.toContain('down')
    expect(w.classes()).toContain('view_lost')
  })

  it('renders unspecified as itself, never as a settled kind', () => {
    const w = mount(EventKindMark, { props: { kind: 'unspecified' } })
    expect(w.text()).toContain('unspecified')
    expect(w.classes()).toContain('unspecified')
  })

  // up and down are the two kinds a swapped KIND_LABEL entry would confuse
  // silently: :class binds props.kind directly, so the class-uniqueness test
  // below cannot see this, and neither can SessionHistoryView.test.ts, which
  // locates a "down" row by the data-kind attribute rowAttrs sets from
  // kindOf, independent of what this component renders as text.
  it('renders up and down as their own labels, not swapped with each other', () => {
    const up = mount(EventKindMark, { props: { kind: 'up' } })
    const down = mount(EventKindMark, { props: { kind: 'down' } })
    expect(up.text()).toBe('up')
    expect(down.text()).toBe('down')
  })

  // Four kinds, four distinct classes. This catches two kinds sharing one
  // CSS class -- it cannot see a KIND_LABEL collapse, since :class binds
  // props.kind directly regardless of what KIND_LABEL says; the up/down test
  // above is what covers a label swap.
  it('gives each kind its own class, so none can be collapsed into another', () => {
    const kinds = ['unspecified', 'up', 'down', 'view_lost'] as const
    const classes = kinds.map((k) => {
      const w = mount(EventKindMark, { props: { kind: k } })
      return w.classes().filter((c) => c !== 'kind-mark').join(',')
    })
    expect(new Set(classes).size).toBe(kinds.length)
  })

  it('carries a title explaining what the kind means', () => {
    const w = mount(EventKindMark, { props: { kind: 'view_lost' } })
    expect(w.attributes('title')).toContain('router said nothing')
  })
})
