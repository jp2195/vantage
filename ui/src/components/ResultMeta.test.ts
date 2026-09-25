import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import ResultMeta from './ResultMeta.vue'
import type { Meta } from '@/api/generated'
import { capturedMeta } from '@/test-support/capturedMeta'
import routers from '@/api/fixtures/routers.json'
import peers from '@/api/fixtures/peers.json'
import ribUnicast from '@/api/fixtures/rib-unicast.json'
import fleetEvents from '@/api/fixtures/fleet-events.json'
import routesStale from '@/api/fixtures/routes-stale.json'

// Fake timers are a global mode switch, not test-local state (see the same
// pattern in src/api/queries.test.ts): this guarantees real timers are back
// before the next test regardless of how this one finished.
afterEach(() => {
  vi.useRealTimers()
})

// Every meta below except the truncated one is lifted verbatim from a
// response a running deployment actually returned (see
// ui/src/api/fixtures/README.md). Hand-writing `{ warnings: [] }` would only
// prove the component agrees with what someone imagined an empty answer
// looks like; the fixture proves it agrees with what the server sent.

describe('ResultMeta', () => {
  it('affirms completeness when there are no warnings', () => {
    // routers.json is the real "complete" case: captured the same session
    // as the other fixtures, but the router-listing endpoint has no
    // mid-dump peer to report, so meta.warnings came back empty. The
    // contract says an empty warnings array is a claim, not an omission.
    // Rendering nothing would make a complete answer and an unexamined one
    // look identical, which throws away the API's most careful property at
    // the last inch.
    const w = mount(ResultMeta, { props: { meta: capturedMeta(routers.meta) } })
    expect(w.text()).toMatch(/complete/i)
  })

  it('names a dumping session rather than showing a bare count', () => {
    // peers.json was captured while peer 172.31.0.90 was still sending its
    // initial ipv4u dump, so meta.warnings carries exactly session_dumping
    // and nothing else.
    const w = mount(ResultMeta, { props: { meta: capturedMeta(peers.meta) } })
    expect(w.text()).toMatch(/still loading|will rise/i)
    // A response with a warning is not the complete case; if the "complete"
    // span rendered unconditionally this would catch it.
    expect(w.text()).not.toMatch(/complete/i)
  })

  it('warns that pages may repeat or be missing on a smear, without dropping the dumping-session warning it arrived with', () => {
    // rib-unicast.json is the only fixture with two warnings at once: the
    // same still-dumping session plus a paginated walk over it. No captured
    // response hands you paginated_smear in isolation, so this also proves
    // the component renders more than one warning at a time rather than
    // showing only the first it recognizes.
    const w = mount(ResultMeta, { props: { meta: capturedMeta(ribUnicast.meta) } })
    expect(w.text()).toMatch(/repeat|missing/i)
    expect(w.text()).toMatch(/still loading|will rise/i)
  })

  it('says the rows of a quiet collector are its last view, in its own words', () => {
    // routes-stale.json is a real /v1/routes/unicast answer over a route of
    // a collector silent past the stale threshold, so its warnings carry a
    // genuine collector_stale.
    const w = mount(ResultMeta, { props: { meta: capturedMeta(routesStale.meta) } })
    expect(w.text()).toMatch(/gone quiet/i)
    expect(w.text()).not.toMatch(/complete/i)
    // Curated, so the daemon's own sentence is not ALSO printed beside it.
    const warning = routesStale.meta.warnings.find((x) => x.code === 'collector_stale')
    if (!warning) throw new Error('routes-stale.json carries no collector_stale')
    expect(w.text()).not.toContain(warning.message)
  })

  it('reports how many of the matched rows are shown when truncated', () => {
    // fleet-events.json is a real captured meta: GET /v1/events?since=2160h
    // &limit=5 against a local vantage-api reading a local dev ClickHouse
    // container's archive (2,843 peer_events rows) really did match 2,843
    // rows and return 5, so meta.warnings carries a genuine `truncated` and
    // total_matched is a genuine 2843 -- neither field is grafted on (see
    // ui/src/api/fixtures/README.md's `## fleet-events.json` entry). A
    // non-null next_cursor remains the only uncaptured case in this
    // directory (README's "Still not captured").
    const w = mount(ResultMeta, {
      props: { meta: capturedMeta(fleetEvents.meta), shown: fleetEvents.data.length },
    })
    // The joined form, not the two numbers checked separately: two
    // standalone toContains would still pass if the component printed "5"
    // and "2843" in unrelated spans, or swapped, neither of which says
    // "showing 5 of 2843". EventsView.test.ts's own cap test already
    // asserts this exact joined string against the same fixture.
    expect(w.text()).toContain(`showing ${fleetEvents.data.length} of ${fleetEvents.meta.total_matched}`)
  })

  it('reflects the response being rendered, not the moment this instance first mounted', async () => {
    // A screen keeps its ResultMeta mounted across a refetch -- same
    // component instance, new data, no remount -- because that is exactly
    // what polling and Colada's cache do (see queries.ts's pollWhileMounted).
    // "complete as of HH:MM:SS" has to mean the response now on screen; a
    // seenAt computed once at setup would freeze at the first response
    // forever while the copy keeps implying "now".
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-06T10:00:00Z'))
    const w = mount(ResultMeta, { props: { meta: capturedMeta(routers.meta) } })
    const first = w.text()

    vi.setSystemTime(new Date('2026-09-06T10:05:00Z'))
    // A new object, not a mutated one: queries.ts hands every refetch a
    // freshly returned meta rather than patching the one already rendered.
    await w.setProps({ meta: capturedMeta({ ...routers.meta }) })

    expect(w.text()).not.toBe(first)
  })
  it('renders the daemon\'s own sentence for a warning code it does not recognize', () => {
    // The three codes were hard-coded here with no fallthrough, and
    // `warning.message` -- which the server sends on every warning and
    // which api/openapi.yaml requires -- was never rendered at all. A
    // fourth code added to the contract would compile cleanly, satisfy
    // nothing, and print an EMPTY footer: not "complete" (warnings is
    // non-empty) and not the warning either. Silence about a caveat the
    // server took the trouble to send is the exact direction of error this
    // component exists to prevent.
    //
    // SYNTHESIZED, and it has to be: by definition no captured response
    // carries a code this UI has not been taught. Built from routers.json's
    // real captured meta with one warning grafted on, in the shape
    // api/openapi.yaml requires of every warning -- a code and a message.
    // The cast is the point: the generated union does not contain this
    // code, which is precisely the case under test.
    const meta = {
      ...routers.meta,
      warnings: [{ code: 'shard_unreachable', message: 'one shard did not answer' }],
    } as unknown as Meta
    const w = mount(ResultMeta, { props: { meta } })
    expect(w.text()).toContain('one shard did not answer')
    // And it is still not the complete case.
    expect(w.text()).not.toMatch(/complete/i)
  })

  it('does not print a recognized code twice by also falling through to its message', () => {
    // The fallthrough must be for the UNRECOGNIZED remainder only. Rendering
    // both the curated copy and the raw message for session_dumping would
    // say the same thing twice in a footer that is one line tall.
    const w = mount(ResultMeta, { props: { meta: capturedMeta(peers.meta) } })
    expect(w.text()).toMatch(/still loading|will rise/i)
    expect(w.text()).not.toContain(peers.meta.warnings[0].message)
  })
})
