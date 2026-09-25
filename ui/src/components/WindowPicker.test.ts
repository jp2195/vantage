import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import WindowPicker from './WindowPicker.vue'

const WINDOWS = [
  { value: '1h', label: 'last hour' },
  { value: '6h', label: 'last 6 hours' },
  { value: '24h', label: 'last 24 hours' },
]

function picker(modelValue = '1h') {
  return mount(WindowPicker, { props: { windows: WINDOWS, modelValue } })
}

describe('WindowPicker', () => {
  it('offers exactly the windows it was given, in order', () => {
    const values = picker().findAll('button').map((b) => b.attributes('value'))
    expect(values).toEqual(['1h', '6h', '24h'])
  })

  // Exactly one, and the right one: a control that marked none would leave
  // an operator unable to tell which window the numbers beside it describe,
  // and one that marked two would be worse.
  it('marks exactly the chosen window', () => {
    const pressed = picker('6h')
      .findAll('button')
      .filter((b) => b.attributes('aria-pressed') === 'true')
      .map((b) => b.attributes('value'))
    expect(pressed).toEqual(['6h'])
  })

  it('emits the window a click chose, rather than keeping it internally', async () => {
    const w = picker()
    await w.findAll('button')[2].trigger('click')
    expect(w.props('modelValue')).toBe('1h')
    expect(w.emitted('update:modelValue')).toEqual([['24h']])
  })
})
