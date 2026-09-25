import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import EventArchiveNote from './EventArchiveNote.vue'

// SessionHistoryView.test.ts's own "states the 90-day retention bound beside
// the results" test only greps for /90 day/i, which is silent on the window
// itself -- mutation testing caught this: hard-coding "the last hour"
// in place of {{ windowLabel }} leaves that test passing. Rather than edit
// the protected suite, the window claim gets
// its own pinned assertion here, so a caller passing a mismatched window --
// the exact defect the doc comment warns is worse than no note -- fails a
// test instead of shipping silently. This is the two screens' shared
// coverage: whichever window Session history or the Events screen passes in,
// this is what proves it actually reached the sentence.
describe('EventArchiveNote', () => {
  it('names the caller-supplied window, not a fixed one', () => {
    const w = mount(EventArchiveNote, { props: { windowLabel: 'last 7 days' } })
    expect(w.text()).toContain('last 7 days')
  })

  it('states the 90-day retention bound', () => {
    const w = mount(EventArchiveNote, { props: { windowLabel: 'last hour' } })
    expect(w.text()).toMatch(/90 day/i)
  })

  it('says the notification itself is not retained', () => {
    const w = mount(EventArchiveNote, { props: { windowLabel: 'last hour' } })
    expect(w.text()).toMatch(/not retained|not stored/i)
  })

  // A cap is a property of one answer (ResultMeta's truncated path), not of
  // the archive -- this note must never claim one.
  it('does not state a cap', () => {
    const w = mount(EventArchiveNote, { props: { windowLabel: 'last hour' } })
    expect(w.text()).not.toMatch(/\bcap\b/i)
  })

  it('marks the paragraph with data-archive-note', () => {
    const w = mount(EventArchiveNote, { props: { windowLabel: 'last hour' } })
    expect(w.find('[data-archive-note]').exists()).toBe(true)
  })
})
