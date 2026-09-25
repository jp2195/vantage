import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import StatesView from './StatesView.vue'
import routers from '@/api/fixtures/routers.json'

describe('StatesView', () => {
  // The page's whole purpose: the states are distinguishable. If the empty
  // state ever starts rendering like the loading state, or the error state
  // silently keeps the completeness claim, this fails here rather than in
  // front of an operator.
  it('draws each state the real DataTable can land in, and keeps them distinct', () => {
    const w = mount(StatesView)
    const text = (state: string) => w.find(`[data-state="${state}"]`).text()

    expect(text('loading')).toMatch(/loading…/)
    expect(text('loading')).not.toMatch(/no rows matched/)

    expect(text('empty')).toMatch(/no rows matched/)
    expect(text('empty')).not.toMatch(/loading…/)
    // The footer's claim: an empty answer with no warnings says so.
    expect(w.find('[data-state="empty"] footer.meta').text()).toMatch(/complete as of/i)

    // The guard DataTable exists to hold: no completeness claim under a
    // failure, even though this block passes both meta and rows.
    //
    // Asserted on ResultMeta's own footer element, not on the block's text:
    // this page's prose EXPLAINS the suppression, so a text match for
    // "complete as of" finds the explanation and passes whether or not the
    // footer is actually suppressed. It did exactly that on the first run.
    expect(text('error')).toMatch(/did not respond/)
    expect(w.find('[data-state="error"] footer.meta').exists()).toBe(false)
    expect(w.find('[data-state="empty"] footer.meta .ok').exists()).toBe(true)

    expect(text('warned')).toContain(routers.data[0].sysname)
    expect(text('warned')).toMatch(/still loading its initial view|initial RIB dump/i)
  })

  // This page is the DESIGN REFERENCE -- its own lede says every state is
  // "drawn by the real DataTable with the props that produce it" -- so a
  // column rendered here in a way no shipped screen renders it teaches the
  // wrong house style. It declares RoutersView's own `last_seen` column and
  // had no cell slot, so DataTable printed the raw ISO instant, six digits
  // of sub-second precision and all: exactly what lib/formatClock.ts exists
  // to prevent, and what EventsView's own test already forbids on its
  // screen. formatClock's doc comment names the screens that share it so
  // they cannot drift; this was a fourth screen rendering the same column,
  // drifted.
  it('renders the clock the way every real screen does, never the raw instant', () => {
    const w = mount(StatesView)
    const warned = w.find('[data-state="warned"]').text()
    const raw = routers.data[0].last_seen
    expect(warned).not.toContain(raw)
    expect(warned).toContain(
      new Date(raw).toLocaleString([], { dateStyle: 'medium', timeStyle: 'medium' }),
    )
  })

  // Four table blocks, not five, and the page says why rather than
  // quietly dropping one. The missing distinction is real:
  // nothing in an API answer separates "nothing matched" from "your filters
  // excluded everything".
  it('states which state it deliberately does not draw', () => {
    const w = mount(StatesView)
    expect(w.findAll('[data-state]')).toHaveLength(4)
    expect(w.text()).toMatch(/filters excluded/i)
  })
})
