import { beforeEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TokenPrompt from './TokenPrompt.vue'

// Submitting the prompt has to do TWO things, and deleting either one left
// the suite green: it must tell App a credential now exists (so the shell
// renders and configureClient runs), and it must write the token to
// sessionStorage (so a reload inside the same tab does not ask again).
// Emitting without storing looks correct until the first refresh.
beforeEach(() => {
  sessionStorage.clear()
})

async function submit(value: string) {
  const wrapper = mount(TokenPrompt)
  await wrapper.find('input').setValue(value)
  await wrapper.find('form').trigger('submit')
  return wrapper
}

describe('TokenPrompt', () => {
  it('stores the token and announces it', async () => {
    const wrapper = await submit('tok-abc123')

    expect(sessionStorage.getItem('vantage.token')).toBe('tok-abc123')
    expect(wrapper.emitted('authenticated')).toEqual([['tok-abc123']])
  })

  // A token pasted from a terminal or a kubectl one-liner arrives with
  // surrounding whitespace more often than not, and a bearer token with a
  // trailing newline is a 401 the operator cannot see the cause of. The
  // stored value and the emitted value must be the same trimmed string --
  // storing one form and sending another would authenticate this session
  // and fail the next.
  it('trims what it was given, in both places', async () => {
    const wrapper = await submit('  tok-abc123\n')

    expect(sessionStorage.getItem('vantage.token')).toBe('tok-abc123')
    expect(wrapper.emitted('authenticated')).toEqual([['tok-abc123']])
  })

  it.each(['', '   '])('does nothing for %j', async (value) => {
    const wrapper = await submit(value)

    expect(sessionStorage.getItem('vantage.token')).toBeNull()
    expect(wrapper.emitted('authenticated')).toBeUndefined()
  })
})
