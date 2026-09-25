import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import StatePill from './StatePill.vue'
import peersDown from '@/api/fixtures/peers-down.json'
import peersViewLost from '@/api/fixtures/peers-view-lost.json'
import peersStale from '@/api/fixtures/peers-stale.json'

// All four states came off the wire, not out of a fixture author's head
// (see ui/src/api/fixtures/README.md, "The peer-state fixtures"):
// peers-down.json was produced by shutting down the BGP neighbor on the
// live FRR sender while BMP stayed connected, peers-view-lost.json by
// killing the BMP transport itself while the router said nothing, and
// peers-stale.json by killing a collector outright and waiting out the
// stale threshold. A test that mounted `{ state: 'view_lost' }` by hand
// would only prove the component agrees with itself about what that string
// means.

// find() narrows on the predicate but not on the field, so the cast below
// says explicitly what the predicate already guarantees: the fixture
// really does contain this state. If a re-capture ever dropped the state
// this looks for, the fixture would return undefined and this throws
// immediately with a clear cause, rather than the test silently mounting
// `undefined` and passing or failing for the wrong reason.
function requireState(peers: Array<{ state: string }>, want: 'up' | 'down' | 'view_lost' | 'stale') {
  const peer = peers.find((p) => p.state === want)
  if (!peer) throw new Error(`fixture has no peer in state ${want}`)
  return peer.state as 'up' | 'down' | 'view_lost' | 'stale'
}

describe('StatePill', () => {
  it('distinguishes view_lost from down', () => {
    // These mean different things and must not render alike. down is the
    // router reporting its BGP session ended; view_lost is the collector's
    // transport dying while the router said nothing, so nobody is watching
    // and those routes are absent from every current-state answer.
    // Showing view_lost as "idle" would misreport blindness as quiet.
    // peers-down.json holds a down peer and an up peer in one response --
    // the case a table actually renders -- and peers-view-lost.json holds
    // the third state.
    const down = mount(StatePill, { props: { state: requireState(peersDown.data, 'down') } })
    const lost = mount(StatePill, {
      props: { state: requireState(peersViewLost.data, 'view_lost') },
    })
    expect(down.text()).not.toBe(lost.text())
    expect(lost.text()).toMatch(/view lost/i)
  })

  it('explains what view_lost means rather than only labeling it', () => {
    const lost = mount(StatePill, {
      props: { state: requireState(peersViewLost.data, 'view_lost') },
    })
    expect(lost.attributes('title')).toMatch(/collector|watching|transport/i)
  })

  it('renders stale as its own state, neither up nor lost', () => {
    // peers-stale.json was captured with collector dev-c2 killed hard and
    // silent past the 90 s threshold (fixtures/README.md). stale is not a
    // softer up and not a milder view_lost: nobody is confirming the session
    // and nobody is contradicting it, and its routes are still being shown.
    const stale = mount(StatePill, { props: { state: requireState(peersStale.data, 'stale') } })
    const up = mount(StatePill, { props: { state: requireState(peersDown.data, 'up') } })
    const lost = mount(StatePill, {
      props: { state: requireState(peersViewLost.data, 'view_lost') },
    })
    expect(stale.text()).toMatch(/^stale$/)
    expect(stale.classes()).toContain('stale')
    expect(stale.classes()).not.toContain('up')
    expect(stale.text()).not.toBe(up.text())
    expect(stale.text()).not.toBe(lost.text())
    expect(stale.attributes('title')).toMatch(/heard from/i)
    expect(stale.attributes('title')).toMatch(/still shown/i)
  })

  it('labels unspecified rather than rendering an empty pill', () => {
    // Mounted by hand: no capture holds this state, because the writer
    // stores it only for a peer event of a kind it does not recognize. The
    // contract allows it, so the pill must still say something.
    const pill = mount(StatePill, { props: { state: 'unspecified' } })
    expect(pill.text()).toMatch(/^unspecified$/)
    expect(pill.attributes('title')).toMatch(/did not recognize/i)
  })

  it('renders up as the good state', () => {
    const up = mount(StatePill, { props: { state: requireState(peersDown.data, 'up') } })
    expect(up.classes()).toContain('up')
  })
})
