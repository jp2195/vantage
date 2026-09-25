# Captured API responses

Every file here came off the wire from a running `vantage-api`. None was hand-written, and
none may be. A hand-authored fixture encodes what someone *thought* the server returns, which
is the failure this directory exists to prevent: a component suite that passes green against a
shape the server has never produced.

Re-capture with `make ui-fixtures` (see `scripts/capture-ui-fixtures.sh`). Most files are
captured this way, against a local dev deployment's ClickHouse archive. A handful need a
different path, because the script's derivation cannot reach that endpoint or the archive
lacked the shape a test needs at capture time; each exception is noted below, with any caveat a
test depends on.

## `routers.json`, `peers.json`, `rib-unicast.json`, `routes-history.json`

Captured while one peer was still mid-RIB-dump, deliberately: waiting for the dump to finish
would have produced an empty `meta.warnings` on every response. `routers.json` carries the
genuine empty case; `peers.json` and `rib-unicast.json` carry real `session_dumping` (and, for
`rib-unicast`, `paginated_smear`) warnings. A recapture needs that same mid-dump window, or
these fixtures lose the shape they exist for.

## `auth-config.json`

Captured from a locally built `vantage-api`, not a cluster deployment: the endpoint did not
exist yet in the deployed image. The capture script skips this file with a loud message instead
of a placeholder; re-capture it from a running deployment once it carries this repository's
current code.

## The peer-state fixtures

`StatePill` renders four states, each produced by a real router or collector action, not
written by hand:

| File | States | How it was produced |
| --- | --- | --- |
| `peers.json` | `up` | the healthy steady state |
| `peers-down.json` | `down`, `up` | the BGP neighbor shut down, BMP still connected |
| `peers-view-lost.json` | `view_lost` | the BMP transport killed, router silent |
| `peers-stale.json` | `stale`, `view_lost` | the collector itself killed and silent past the stale threshold |

`peers-down.json` holds a `down` peer and an `up` peer in one response, so a table rendering
both cannot show them alike.

## The liveness fixtures: `routers-stale.json`, `peers-stale.json`, `routes-stale.json`, `collectors-stale.json`

Captured on 2026-09-24, between 13:36:34 and 13:36:35 UTC, from the dev stack's own `api`
service (`127.0.0.1:9473`), running the code that introduced collector heartbeats. The dev
stack's second collector, `collector2` (collector id `dev-c2`), had been killed with SIGKILL at
13:34:18. Its last heartbeat reached the archive at 13:33:57, about 157 s before the capture
and well past the 90 s default stale threshold. So `dev-c2`'s peers read `stale`, its routers
carry `peers_stale`, and every answer holding its rows carries a `collector_stale` warning.
Nothing in them was edited.

- `routers-stale.json` is `GET /v1/routers`. It is also the fixture the Routers column guard
  reads, because `routers.json` predates `peers_stale` and is kept for the tests that pair its
  router with `peers.json`.
- `peers-stale.json` is `GET /v1/peers?router=172.22.0.8`, the FRR sender, chosen because
  `dev-c2` was its only live collector. It holds `dev-c2`'s two peers as `stale` and the first
  collector's view of the same two peers as `view_lost`, from an older session. Every row
  carries an `up_since`, so a screen that hides one is making its own decision.
- `routes-stale.json` is `GET /v1/routes/unicast?prefix=10.10.1.0/24`: one `dev-c2` row.
- `collectors-stale.json` is `GET /v1/collectors`. `dev-c1`'s `last_beat_at` is a second or two
  older than the capture, and `dev-c2`'s about 157 s older.

The capture script has no phase for these. A recapture needs a collector that is actually
silent past the threshold while the API answers, which a healthy dev stack never has.

## `peers-up-since.json`

A later, separate capture, taken after `/v1/peers` grew `up_since`. It does **not** replace
`peers.json` or `peers-down.json`: those predate four rounds of field additions (`hold_time`,
`mp_families`, `addpath_families`, `sys_descr`, `up_since`) and back tests that assert their
specific rows. Recapturing them would churn those tests for no benefit; this file covers only
`up_since`, and the column guard takes the union of both shapes.

## `events.json`

Captured against a disposable, purpose-built session rather than the usual local archive,
because that archive holds no `view_lost` rows -- a session has to actually drop its BMP
transport to produce one, and none had. The router and peer identifiers here are synthetic and
visibly so. Recapturing this against the usual archive is not possible until a real session
there drops; the capture script has no phase for `/v1/events` regardless.

## `fleet-events.json`

A real, unscoped capture with a genuine `truncated` warning and a genuine `total_matched`
(2843): the archive holds far more matching rows than the requested `limit=5`. Reaching an
archive whose newest row was over a week old required raising the daemon's `max_unscoped_since`
past its default -- a config-file-only setting, with no command-line flag.

## `events-flapping-peer.json`

No script phase; captured directly from `/v1/events`, scoped to one router (`nx-leaf1`,
`10.0.103.67`) and one peer (`10.255.1.1`) over an 1100-hour window. It is the archive's only
peer that flapped for real: nine rows in one session, four clean `down`/`up` cycles ending in
`up`, from one collector. A scoped query is exempt from `max_unscoped_since`, which is what
made an old enough window reachable. `PeerDetailView.test.ts` asserts the four downs and the
single session, so a recapture that flattens either fails loudly.

## `events-two-collectors.json`

`fleet-events.json` predates a second collector and cannot exercise multi-collector
disambiguation. This file captures one router/peer/session observed by two collectors: two rows
identical in every visible field, differing only in `collector` and the underlying
`session_id` -- the shape `test-support/disambiguationRows.ts` exists to catch a screen relying
on any other field to tell them apart.

## `routes.json`

No script phase (`/v1/routes` takes `prefix=`/`covers=`/`origin_asn=`/`community=`, none of
which the capture script's first-phase derivation produces); captured directly instead. The `evpn` arm is genuinely
empty for this query, not filled in to avoid an empty array, and `total_matched` here is a real
per-family count rather than the `null` every paginated RIB/history endpoint returns until a
walk truncates.

## `collection-{churn-peers,dumps,sessions,locrib,flags}.json`

Fleet-wide captures against an archive over a week stale, so most also needed a raised
`max_unscoped_since` (see `fleet-events.json` above).

- `collection-churn-peers.json` was recaptured after the endpoint grew a per-row `activity`
  series; the column guard caught the earlier capture predating that field. The archive is
  quiet -- two rows, busiest peer at 21 rows/day -- which is itself load-bearing: that low a
  rate is what exposed a formatter rendering every real rate as `"0.00"`.
- `collection-dumps.json` and `collection-sessions.json` share two routers with
  `router_sysname: ""` -- real, documented behavior for a router whose sysName TLV carried
  nothing -- so both sections must render the fallback text, never a blank cell.
- `collection-sessions.json` carries `view_lost: 0` on every row: a real archive gap, not a
  capture mistake. No test can get a `view_lost` row from this file; one that needs one must
  synthesize it and say so.
- `collection-locrib.json` is scoped to each router's current BMP session and so has no
  `has_stat: false` row any more. The pre-scoping capture is kept, unchanged, as
  `collection-locrib-2026-09-11.json`, for its two `has_stat: false` rows -- the only captured
  instance of that shape.

## `topology.json`

No script phase: the script's derivation would pick a `(router, peer)` pair with no guarantee
of a withdrawn edge, and a phase built that way could silently overwrite this fixture with a
graph missing the property it exists to carry. Captured by hand instead, against the pair known
to hold one.

The unequal edge weights (`routes: [3, 1, 1, 1]`, not four edges tied at 1) are load-bearing: a
mutation of `PathGraph.vue`'s stroke-width expression survived against an earlier, all-tied
capture, since a tie makes every stroke render at the same width regardless of what the
expression computes. The withdrawn edge is tied to the specific BMP session live at capture
time; resetting it (a router reload, a bounced collector) would replace the edge with a live
one, so a naive re-capture can silently lose the fixture's reason for existing.

## `link-state.json`

Restored from the one surviving backup of a multi-vendor test lab that no longer exists and is
not part of this repository (`make restore-archive-backup`, which refuses to run a second
time). The dev stack's `ls-replay` service does feed BGP-LS live, but from a single NX-OS
OSPFv3 capture, and nothing can reproduce these two views live; so this file cannot be
re-captured against a running deployment -- only by restoring that same backup again.

Two views, chosen for what they prove together: the richest view in the archive (most nodes,
links and prefixes, nothing one-sided), and a second that is simultaneously three of this
contract's hardest cases at once -- `/v1/ls/nodes` returning zero rows for a router while
`/v1/ls/links` and `/v1/ls/prefixes` return real ones for it, a warned leg beside a clean one,
and a one-sided adjacency. No view here has both a one-sided adjacency and the nodes to draw it
between, so `LinkStateGraph.test.ts`'s "marks an adjacency seen from only one side" test stays
synthesized permanently -- restoring the backup again would not change that. The archive also
holds only one IGP protocol and one area, so a second protocol, a second area, or an IS-IS
view's own field shapes stay synthesized too.

## Still not captured

- A `view_lost` row on `/v1/collection/sessions` (see above).
- A non-null `next_cursor`: every unscoped capture here is a capped list, not a walk, and every
  scoped one holds too few rows to fill a page.
- The two `link-state.json` gaps described above (a drawn one-sided adjacency; a second
  protocol, area or IS-IS view) -- both permanent.
