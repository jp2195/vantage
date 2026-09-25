import { describe, expect, it } from 'vitest'
import type { Peer, Router } from '@/api/generated'
import type { Column } from './DataTable.vue'

// Compile-time assertions, pinned the only way a type can be pinned: with
// `@ts-expect-error`. An UNUSED @ts-expect-error is itself a TypeScript error,
// so if `Column.id` ever loosens back to `string`, these directives stop
// suppressing anything and `vue-tsc` fails. The pin is the directive; the
// runtime assertions below only keep the values referenced.
//
// This file exists because the property it guards is exactly the one
// committing the generated client is FOR -- a field moving should surface
// as a compile error in a table's column definitions, not as a
// blank cell in production -- and that property was stated in prose and
// enforced by nothing until this test was added.

const routerColumns: Column<Router>[] = [
  { id: 'sysname', header: 'Router' },
  { id: 'peers_up', header: 'Up', numeric: true },
]

const peerColumns: Column<Peer>[] = [{ id: 'peer_ip', header: 'Peer' }]

const invented: Column<Router>[] = [
  // @ts-expect-error 'uptime' is a metric this pipeline does not measure, and
  // not a field on Router. The closed-set columnGuard test catches this at run
  // time against a captured fixture; this catches it at compile time.
  { id: 'uptime', header: 'Uptime' },
]

const wrongEntity: Column<Router>[] = [
  // @ts-expect-error 'peer_ip' is real, but it belongs to Peer, not Router.
  // A real field from the WRONG entity is the likelier mistake of the two.
  { id: 'peer_ip', header: 'Peer' },
]

describe('Column<Row>', () => {
  it('accepts ids that are fields of the row entity', () => {
    expect(routerColumns.map((c) => c.id)).toEqual(['sysname', 'peers_up'])
    expect(peerColumns[0].id).toBe('peer_ip')
  })

  it('rejects invented ids and ids from another entity (see @ts-expect-error above)', () => {
    // These compile only because the directives above suppress the errors.
    // If the type loosens, the directives go unused and the typecheck fails
    // before this assertion ever runs.
    expect(invented).toHaveLength(1)
    expect(wrongEntity).toHaveLength(1)
  })
})
