import { mount } from '@vue/test-utils'
import { h } from 'vue'
import { describe, expect, it } from 'vitest'
import DataTable from './DataTable.vue'
import type { Meta } from '@/api/generated'
import routers from '@/api/fixtures/routers.json'
import peers from '@/api/fixtures/peers.json'

const cols = [
  { id: 'sysname', header: 'Router' },
  { id: 'ip', header: 'Address' },
]

// peers.json rows carry no `sysname`/`ip` -- that is a routers-listing shape,
// not a peer one. Pairing this fixture with `cols` above would render every
// cell empty while still passing the assertion under test. These are two of
// the columns PeersView was built with (peer_ip, router_ip, rib, asn,
// state, routes), so the warning test below exercises the shape a peer
// table actually has.
const peerCols = [
  { id: 'peer_ip', header: 'Peer' },
  { id: 'state', header: 'State' },
]

describe('DataTable', () => {
  it('renders a row per entry and a ResultMeta footer', () => {
    const w = mount(DataTable, {
      props: { columns: cols, rows: routers.data, meta: routers.meta },
    })
    expect(w.findAll('tbody tr')).toHaveLength(routers.data.length)
    // routers.json was captured with an empty warnings array, so the footer
    // must show the quiet positive rather than nothing at all.
    expect(w.text()).toMatch(/complete/i)
  })

  it('surfaces the response warnings in the footer', () => {
    // peers.json carries a real session_dumping warning. Cast rather than
    // left inferred: resolveJsonModule widens `warnings[].code` to `string`,
    // while the generated Meta narrows it to the literal warning-code union
    // -- the same gap queries.ts's own synthesized Meta values cast past.
    const w = mount(DataTable, {
      props: { columns: peerCols, rows: peers.data, meta: peers.meta as Meta },
    })
    expect(w.text()).toMatch(/still loading|counts will rise/i)
  })

  it('shows an error instead of an empty table when the query failed', () => {
    // An empty table reads as "there is nothing here". A failed query means
    // "we do not know", and those must not look the same.
    const w = mount(DataTable, {
      props: { columns: cols, rows: [], meta: undefined, error: new Error('peer is required') },
    })
    expect(w.text()).toContain('peer is required')
    expect(w.find('tbody tr').exists()).toBe(false)
  })

  it('distinguishes an empty result from a result still loading', () => {
    const empty = mount(DataTable, { props: { columns: cols, rows: [], meta: routers.meta } })
    const busy = mount(DataTable, {
      props: { columns: cols, rows: [], meta: undefined, loading: true },
    })
    // A bare "the two don't match" is too weak: deleting the empty-result
    // branch entirely still leaves the two texts unequal, because the rows
    // then fall through to a real (headerful, zero-row) <table> plus its
    // ResultMeta footer -- which is a different string from "loading…" by
    // accident, not because the branch this test is supposed to pin still
    // exists. Pinning both copies closes that hole.
    expect(empty.text()).toMatch(/no rows matched/i)
    expect(busy.text()).toMatch(/loading/i)
    expect(empty.text()).not.toBe(busy.text())
  })

  it('does not let the footer claim completeness when the latest attempt failed', () => {
    // routers.meta is the real empty-warnings "complete" case -- captured
    // for a response that fully succeeded. Pairing it with an error models
    // exactly what real screens do: they pass `:meta="data?.meta"` alongside
    // `:error="error"`, and Pinia Colada keeps the previous successful
    // `data` in place while a rejected refetch sets `error` (queries.ts's
    // pollWhileMounted ticks every REFETCH_MS, so a poll blip hits this on
    // the first bad tick). The footer sits outside the error/loading/empty/
    // rows chain and used to key only on `meta`, so it kept asserting
    // "complete as of HH:MM:SS" underneath the error message -- the last
    // successful answer's completeness claim, presented as if it were
    // still current. An operator reading that sees a confirmed-complete
    // table and a failure banner at the same time, which is exactly the
    // confusion this component exists to prevent.
    const w = mount(DataTable, {
      props: {
        columns: cols,
        rows: [],
        meta: routers.meta,
        error: new Error('deadline exceeded'),
      },
    })
    expect(w.text()).toContain('deadline exceeded')
    expect(w.text()).not.toMatch(/complete/i)
  })

  it('keeps showing already-loaded rows instead of blanking the table when loading flips true again', () => {
    // This is the shape a background poll or a "load more" page fetch
    // actually looks like on the wire: real rows and a real meta already on
    // screen, `loading` true again because the NEXT fetch just started.
    // Both of RoutersView's and useRibPage's own producers set `loading`
    // this way -- Colada's isLoading is true on every refetch, not just the
    // first, and useRibPage.loading is set at the top of every fetchPage
    // call including loadMore(). A caller can still hand this component the
    // wrong signal (RoutersView.vue's comment records finding and fixing
    // exactly that upstream), but this component must not compound a wrong
    // signal into a table that blanks itself every time one arrives.
    const w = mount(DataTable, {
      props: { columns: cols, rows: routers.data, meta: routers.meta, loading: true },
    })
    expect(w.findAll('tbody tr')).toHaveLength(routers.data.length)
    expect(w.text()).not.toMatch(/loading/i)
    // The footer must still say the last fetch completed -- not be
    // suppressed, and not sit under a contradicting "loading…" banner.
    expect(w.text()).toMatch(/complete/i)
  })

  it('renders each column header and lets a named cell slot override the default value', () => {
    // None of the tests above ever pass a slot, so nothing yet proves the
    // #cell-<id> contract screens are written against actually works --
    // only that the fallback (the bare field value) renders. This pins the
    // slot itself: the override replaces the default, and the row it
    // receives is the real row, not an empty scope.
    const w = mount(DataTable, {
      props: { columns: cols, rows: routers.data, meta: routers.meta },
      slots: {
        'cell-sysname': ({ row }: { row: Record<string, unknown> }) =>
          h('strong', `router:${row.sysname}`),
      },
    })
    expect(w.text()).toContain('Router')
    expect(w.text()).toContain('Address')
    expect(w.text()).toContain(`router:${routers.data[0].sysname}`)
    // The un-overridden column still falls back to the bare field, so this
    // is proof the slot replaced one cell rather than the whole row.
    expect(w.text()).toContain(routers.data[0].ip)
  })
  // Widths are the table's own units -- px and %, not CSS grid's
  // `minmax()`, which a <col> cannot use. Translating a CSS grid layout
  // into table widths is each view's job; this component passes through
  // whatever it declares.
  //
  // The smear this fixes, measured in a browser on /routes: eight values --
  // prefix, rib, path id, next hop, as path, origin, localpref, med -- were
  // distributed by the browser's own auto table layout across a 2000px
  // viewport, so a row was read by tracking the eye over 500px of white.
  // This project's own tables specify the opposite: a fixed grid whose
  // columns carry declared widths (the Routes table is
  // `minmax(130px,1.1fr) 88px 130px ...` with a 1150px min-width) and
  // which ends where its data ends.
  //
  // Declared widths reach the table through a <colgroup>, which is the one
  // mechanism that sizes a column rather than a cell: a width on a <td>
  // applies to that row alone and loses to the next row's content.
  it('gives each column the width its spec declares, so a narrow value cannot smear', () => {
    const w = mount(DataTable, {
      props: {
        columns: [
          { id: 'sysname', header: 'Router', width: '30%' },
          { id: 'ip', header: 'Address', width: '88px' },
        ],
        rows: routers.data,
      },
    })
    const widths = w.findAll('colgroup col').map((c) => c.attributes('style'))
    expect(widths).toEqual(['width: 30%;', 'width: 88px;'])
  })

  // A column that declares nothing is what absorbs the slack, so the table
  // fills its container instead of ending in a ragged edge. It must not
  // acquire a width of its own for that to work.
  it('leaves a column that declares no width unconstrained', () => {
    const w = mount(DataTable, {
      props: { columns: cols, rows: routers.data },
    })
    const cols_ = w.findAll('colgroup col')
    expect(cols_).toHaveLength(2)
    expect(cols_.map((c) => c.attributes('style'))).toEqual([undefined, undefined])
  })

  // A wide table scrolls inside its own container rather than widening the
  // page: this project's own tables carry a min-width (1150px on Routes)
  // and the page body must never scroll horizontally because of one of
  // them.
  it('carries the min-width a wide table asks for, on the table and not the page', () => {
    const w = mount(DataTable, {
      props: { columns: cols, rows: routers.data, minWidth: '1150px' },
    })
    expect(w.find('table').attributes('style')).toBe('min-width: 1150px;')
    expect(w.find('.wrap').attributes('style')).toBeUndefined()
  })
})

// A value cut off with an ellipsis is a different value on screen -- an
// address, a collector id, a router name -- so a truncated cell carries its
// full text as a title, set on hover. jsdom computes no layout, so each test
// stubs the two widths the check reads; whether a real cell truncates is a
// browser measurement.
describe('DataTable, titles on truncated cells', () => {
  const titleCols = [{ id: 'ip', header: 'Address' }]
  const titleRows = [{ ip: '2001:7f8:1::a500:6939:1' }]

  function widths(el: Element, scroll: number, client: number) {
    Object.defineProperty(el, 'scrollWidth', { configurable: true, value: scroll })
    Object.defineProperty(el, 'clientWidth', { configurable: true, value: client })
  }

  it('titles a truncated cell with its full text on hover', async () => {
    const w = mount(DataTable, { props: { columns: titleCols, rows: titleRows } })
    const td = w.find('tbody td')
    widths(td.element, 209, 202)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBe('2001:7f8:1::a500:6939:1')
  })

  it('leaves a cell that fits untitled', async () => {
    const w = mount(DataTable, { props: { columns: titleCols, rows: titleRows } })
    const td = w.find('tbody td')
    widths(td.element, 202, 202)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBeUndefined()
  })

  it('keeps a title the cell already had', async () => {
    const w = mount(DataTable, { props: { columns: titleCols, rows: titleRows } })
    const td = w.find('tbody td')
    td.element.setAttribute('title', 'set elsewhere')
    widths(td.element, 209, 202)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBe('set elsewhere')
    // Nor is it cleared once the cell fits: this code did not set it.
    widths(td.element, 202, 202)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBe('set elsewhere')
  })

  it('clears a title it set once the cell fits again', async () => {
    const w = mount(DataTable, { props: { columns: titleCols, rows: titleRows } })
    const td = w.find('tbody td')
    widths(td.element, 209, 202)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBe('2001:7f8:1::a500:6939:1')
    // A wider viewport later: the same cell now holds its value.
    widths(td.element, 209, 240)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBeUndefined()
  })

  it('titles a slotted cell with the text of its content, whitespace collapsed', async () => {
    const w = mount(DataTable, {
      props: { columns: titleCols, rows: titleRows },
      slots: {
        'cell-ip': ({ row }: { row: Record<string, unknown> }) => [
          h('a', { href: '#' }, `\n  ${row.ip}`),
          // No whitespace between the two: on screen a margin separates
          // them, and the title must too.
          h('span', 'history'),
        ],
      },
    })
    const td = w.find('tbody td')
    widths(td.element, 244, 162)
    // The pointer lands on the link, not the cell: the event reaches the
    // cell by bubbling.
    await w.find('tbody td a').trigger('mouseover')
    expect(td.attributes('title')).toBe('2001:7f8:1::a500:6939:1 history')
  })

  it('titles a cell whose own content ellipsizes inside it', async () => {
    const w = mount(DataTable, {
      props: { columns: titleCols, rows: titleRows },
      slots: {
        'cell-ip': ({ row }: { row: Record<string, unknown> }) =>
          h('span', { style: 'display: block; overflow: hidden; text-overflow: ellipsis' }, `${row.ip}`),
      },
    })
    const td = w.find('tbody td')
    widths(td.element, 202, 202)
    widths(td.find('span').element, 173, 166)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBe('2001:7f8:1::a500:6939:1')
  })

  // Only a descendant that ellipsizes counts. One that is merely wider than
  // its own box without cutting text off with an ellipsis is not a
  // shortened value.
  it('ignores a descendant that overflows without ellipsizing', async () => {
    const w = mount(DataTable, {
      props: { columns: titleCols, rows: titleRows },
      slots: {
        'cell-ip': ({ row }: { row: Record<string, unknown> }) =>
          h('span', { style: 'display: block; overflow: auto' }, `${row.ip}`),
      },
    })
    const td = w.find('tbody td')
    widths(td.element, 202, 202)
    widths(td.find('span').element, 173, 166)
    await td.trigger('mouseover')
    expect(td.attributes('title')).toBeUndefined()
  })
})
