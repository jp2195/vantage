import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import StatesView from './StatesView.vue'
import routers from '@/api/fixtures/routers.json'
import DataTable from '@/components/DataTable.vue'
import type { GuardedColumn } from '@/test-support/columnGuard'
import { tableMinWidthPx } from '@/test-support/columnsOf'
import { crampedAt, declaredTotalAt } from '@/test-support/tableFloor'

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

// Three of the four columns are fixed px, and table-layout:fixed pays those
// first: with no floor, a 390px phone left Router 0px wide and cut its
// header off. The floor has to hold the fixed columns plus Router's share
// of the floor itself, and that share has to hold a 12-character sysName.
// 123px is 4a8063e2ed30 with the cell's 36px of padding, measured in
// Chromium; jsdom computes no layout.
describe('StatesView table floor', () => {
  it('floors the table wide enough for its fixed columns and a full sysName', () => {
    const w = mount(StatesView)
    const tables = w.findAllComponents(DataTable as never)
    expect(tables.length).toBe(4)
    for (const t of tables) {
      const floor = tableMinWidthPx(t as never)
      expect(floor).toBeDefined()
      const columns = (t as unknown as { props(n: string): GuardedColumn[] }).props('columns')
      expect(declaredTotalAt(columns, floor!)).toBeLessThanOrEqual(floor!)
      expect(crampedAt(columns, floor!, { sysname: 123 })).toEqual([])
    }
  })
})
