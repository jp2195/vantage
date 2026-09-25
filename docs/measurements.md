# Measurements

Several limits in vantage are numbers rather than preferences: the 24-hour
`max_unscoped_since` default, the requirement that an unscoped `/v1/events`
request carry a `since=` bound, the page size `vantage query rib` walks at,
the result cap on `/v1/routes`, the column codecs in the schema. This page
records the measurements behind them. The code cites these sections by
name, and some API 400 responses quote a link to one, so an operator
refused by the daemon can see what the refusal rests on and judge whether
their own hardware changes the answer.

Unless a section says otherwise, each measurement was taken on a single host
running the dev stack's ClickHouse (24.8.14), in a scratch database built
from the exact DDL in `deploy/clickhouse/schema.sql` and dropped afterwards,
and timings are ClickHouse's own `query_duration_ms` from `system.query_log`
rather than wall clock, with `read_rows` recorded alongside because
it is what separates "the `LIMIT` stopped the scan" from "the scan ran and
the `LIMIT` trimmed it". Each section ends with what the measurement did not
cover. Re-measure before carrying a number to a different ClickHouse version
or a very different fleet.

## Unscoped events

**Measured 2026-09-08.** The question: can `GET /v1/events` answer "what just
broke anywhere" — no `router=`, no `peer=` — without reading the whole
`peer_events` table, and which endpoint shape does that support?

**Method.** 2,040,000 `peer_events` rows: 5,000 (router, peer) identities
across 200 routers, Pareto-skewed churn (50 hot peers at ~3,077 events each,
the rest at ~373), 89 days of `ts_collector` spanning four monthly
partitions, plus a 40,000-row (2%) batch of duplicate keys modeling a read
that lands before a background merge. The scoped path was checked first,
through the shipped `query.PeerEventsPage`: 6–10 ms, reading 94,054 of
2,040,000 rows (4.6%) for one peer — a seek, as designed. That 94,054 is a
floor of roughly one granule per active part (12 parts here), not a row
count; after `OPTIMIZE ... FINAL` consolidated the table to 4 parts, the same
read fell to 32,730.

**An unscoped newest-first read never stops early.**

| `LIMIT` | `read_rows` (12 parts) | ms | `read_rows` (4 parts) | ms |
|---:|---:|---:|---:|---:|
| 100 | 2,040,000 | 17 | 2,000,000 | 17 |
| 1,000 | 2,040,000 | 16 | 2,000,000 | 22 |
| 10,000 | 2,040,000 | 26 | 2,000,000 | 32 |

`ts_collector` is not a prefix of `peer_events`' sort key (`router_ip,
peer_ip, rib, ts_router, stream_seq`), so ClickHouse must read every row the
query may see, sort in memory, then trim. With the shipped `argMax` dedup
(see [Peer event deduplication](#peer-event-deduplication)) added, the same
full-table read costs 237–289 ms and 2.4–3.6 GB of memory at every `LIMIT`.

**`since=` prunes whole monthly partitions, and nothing finer on the base
table.**

| bound | `read_rows` | ms |
|---|---:|---:|
| none | 2,040,000 | 14–20 |
| last 24 hours | 174,262 (the current partition, exactly) | 8–9 |
| last 45 days | 1,595,386 (three partitions, exactly) | 19–21 |

The 24-hour window matched only 22,862 rows but still read the whole
174,262-row partition. With the dedup layered on, the 24-hour read stayed at
174,262 rows, 14–20 ms and 24–30 MB. A 1-hour dedup read cost 4–5 ms and
~100–210 KB. (An earlier 1-hour figure of 4,787 rows read was taken with a
projection present and silently selected; it is superseded by the
projection-free 4–5 ms figure.)

**A `(ts_collector, stream_seq)` projection was rejected.** Adding one to a
`ReplacingMergeTree` first needs the non-default
`deduplicate_merge_projection_mode = 'rebuild'` and a materialization pass.
ClickHouse 24.8 never selects it for a bare "newest N" query with no `WHERE`
(forcing it fails with `Code: 117`), on `ReplacingMergeTree` and plain
`MergeTree` alike. Under a `since=` bound it can be selected, but its
`read_rows` stayed flat across `LIMIT` in every case tested: 704,600 at
`LIMIT` 100, 1,000 and 10,000 for a 30-day bound. It narrows what `since=`
already narrows and never makes the read proportional to the page.

**What this decides.** The unscoped mode of `/v1/events` is a capped list
with `meta.total_matched`, no cursor, and a required `since=`:
`query.FleetEventFilter` refuses a zero `Since`, and the API applies
`historyDefaultSince` (1 hour) when `since=` is absent. `max_unscoped_since`
defaults to 24 hours because that is the widest window measured as cheap;
past it the request is a 400 that cites this section. A scoped request
(`router=` and `peer=` together) is a seek and carries no window bound.

**Not covered.** A two-tier churn model rather than a continuous
distribution; one `rib` per peer; the duplicate batch landed unevenly across
peers (the 2% aggregate rate is right, its per-peer spread is not); one host,
one seeding. The part-count floor above depends on how well background merges
keep up, which this rig does not model.

## Collection dumps cost

**Measured 2026-09-11.** The question: can `/v1/collection/dumps` afford to
classify re-dumps against genuine changes across all three route tables —
`route_unicast`, `route_vpn` and `route_evpn` — or does it need narrowing?

**Method.** Each table seeded to its own route identity: a bulk population of
2,000,000 rows (100 router/peer pairs × 200 identity combinations × 100
sessions, one observation per combination per session, so pure re-dump) and
a flap population of 20,000 rows (100 pairs × 2 combinations × 100
observations inside one session, 60 s apart, so genuine churn), spread over
89 days. `route_unicast` also carried a 2% duplicate-key batch. The query
counted dumps, changes and archived rows per router with the dashboards'
window-function classification. Measured at 24 hours, the widest window the
shipped `max_unscoped_since` allows, and at the 90-day TTL, the widest any
window could ever return. Best of three.

| window | table | `read_rows` | ms | memory |
|---|---|---:|---:|---:|
| 24h | `route_unicast` | 260,011 | 7 | 14.64 MiB |
| 24h | `route_vpn` | 271,200 | 10 | 16.91 MiB |
| 24h | `route_evpn` | 271,200 | 11 | 18.73 MiB |
| 24h | **summed** | **802,411** | **28** | **~50.3 MiB** |
| 90d | `route_unicast` | 2,511,496 | 104 | 420.4 MiB |
| 90d | `route_vpn` | 2,529,943 | 110 | 477.9 MiB |
| 90d | `route_evpn` | 2,529,943 | 125 | 600.2 MiB |
| 90d | **summed** | **7,571,382** | **339** | **~1,498 MiB (~1.46 GiB)** |

**Each table's own identity is load-bearing.** On a 20,000-row population per
table where two routes share every identity column but one, dropping `rd`
from the VPN identity or `ip` from the EVPN identity turned 10,100 dumps /
9,900 changes into 10,000 / 10,000: exactly 100 real dumps per table
miscounted as changes.

**The shipped statement, measured on a rebuilt rig.** `query.DumpCounts`
groups by `router_ip` (resolving the name with `argMax`), issues one
`UNION ALL` statement over the three tables rather than three, and reads
without `FINAL`. That shape was measured separately on a rebuilt rig with
fewer rows in the 24-hour window, so its absolute numbers should not be read
against the table above:

| statement, all three tables | `read_rows` | ms | memory |
|---|---:|---:|---:|
| 24h, union, `FINAL`, `GROUP BY router_sysname` | 720,000 | 12 | 32.94 MiB |
| 24h, union, `FINAL`, `GROUP BY router_ip` + `argMax` | 720,000 | 13 | 39.14 MiB |
| **24h, union, no `FINAL`, `GROUP BY router_ip` + `argMax` (shipped)** | **720,000** | **13** | **24.87 MiB** |
| **1h, union, no `FINAL`, `GROUP BY router_ip` + `argMax` (shipped)** | **720,000** | **11** | **12.18 MiB** |

The grouping key made no measurable difference. One union statement beat
three sequential ones (13 ms against a summed 19–21 ms on the same rig), so
the summed figures above are a conservative bound. Dropping `FINAL` cost the
same time and 36% less memory. Narrowing from 24 hours to 1 hour inside one
monthly partition read the same rows; only the classification work shrank.

**What this decides.** `DumpCounts` reads all three route tables with no
narrowing, classifies each table under its own identity and sums only the
counts, and sits behind the same 24-hour `max_unscoped_since` clamp as the
rest of `/v1/collection/*`. The 400 for an over-wide window quotes the 24h and
90d totals above.

**Not covered.** The VPN and EVPN populations were sized to match
`route_unicast`, far beyond the real VPN and EVPN volume at the time, so
their numbers are pessimistic. All 100 router/peer
pairs share one synchronized session schedule. No withdrawal population was
seeded at volume. Both shapes produced 100 groups; group cardinality in the
thousands was not tested.

## Collection sessions and flags cost

**Measured 2026-09-20.** The question: what do the other two collection
signals cost — `SessionCounts`, a `FINAL` read of `peer_events`, and
`FlagCounts`, a union over ten tables' `parse_flags`?

**Method.** Both statements were rendered from the shipped Go constants, not
transcribed. 4.92 M rows per variant across ten tables, spread over 90 days:

| table | rows | table | rows |
|---|---:|---|---:|
| `route_unicast` | 1,500,000 | `ls_links` | 300,000 |
| `peer_events` | 1,020,000 | `ls_prefixes` | 300,000 |
| `route_vpn` | 750,000 | `route_evpn` | 250,000 |
| `stats_events` | 500,000 | `ls_nodes` | 150,000 |
| `eor_events` | 100,000 | `ls_events` | 50,000 |

`peer_events` is deliberately within 1.5x of the largest route table — a
fleet flapping far harder than a real one — and 20,000 of its rows are
redelivered duplicates for `FINAL` to collapse. Flag density was built at two
rates: 1% (production-like) and 50% (a lab archive full of malformed
captures). "As ingested" includes the unmerged duplicate parts a live archive
always has; "steady state" is after `OPTIMIZE ... FINAL`. Best of three.

`sessions`, unaffected by flag density:

| window | state | ms | `read_rows` | read bytes |
|---|---|---:|---:|---:|
| 24h | as ingested | 8 | 326,419 | 20.55 MiB |
| 24h | steady state | 3 | 219,923 | 7.13 MiB |
| 90d | as ingested | 25 | 1,510,559 | 95.08 MiB |
| 90d | steady state | 23 | 1,490,248 | 93.80 MiB |

`flags`, the ten-table union, no `FINAL`:

| window | flag rate | ms | `read_rows` | read bytes |
|---|---|---:|---:|---:|
| 24h | 1% | 7 | 1,077,625 | 25.91 MiB |
| 24h | 50% | 8 | 1,077,654 | 26.42 MiB |
| 90d | 1% | 28 | 4,919,992 | 81.51 MiB |
| 90d | 50% | 81 | 4,919,996 | 83.82 MiB |

Ten narrow reads cost less than one wide one: each union branch selects only
`stream_seq` and `parse_flags`, so `flags` reads 81 MiB at 90 days while the
single-table `sessions`, where `FINAL` reads every column, reads 94 MiB.
Flag density, not row count, is what moves `flags`: 50 times the density cost
2.9x the time (28 ms to 81 ms) on almost the same bytes read, because the
work is `arrayJoin` and `uniqExact` over the flagged rows.

**What this decides.** Neither signal needs narrowing; both sit behind the
same 24-hour `max_unscoped_since` clamp as `dumps`. `SessionCounts` keeps its
`FINAL`. `flags` gets slower as a fleet's flag rate climbs — which is exactly
when an operator opens it — so a fleet flagging heavily with a raised
`max_unscoped_since` is the combination to re-measure.

## Collector and writer load test

**Measured 2026-09-04.** The question: how much can one collector and one
writer carry, what saturates first, and what signals that a second one is
needed?

**Method.** A load generator (now `vantage loadgen`) speaking real BMP to a
real collector, into a separate NATS server and a separate ClickHouse
database, with the collector and writer as local binaries. Everything —
generator, collector, writer, NATS and ClickHouse — on one 32-core host.
10 M `route_unicast` rows per packing run; the longest single run was about 190
seconds.

**NLRIs per UPDATE decides throughput and storage.** Same row count, only
the packing varied:

| NLRI per UPDATE | rows/s end to end | compressed B/row |
|---:|---:|---:|
| 1 | 99,261 | 17.6 |
| 2 | 169,615 | 15.8 |
| 4 | 234,612 | 11.2 |
| 8 | 325,337 | 8.1 |
| 50 | 462,812 | 5.8 |
| 200 | 475,432 | 5.4 |

Packing is set by the router, not by vantage: an initial table dump packs
heavily and behaves like the bottom of this table, steady-state churn is
closer to one prefix per UPDATE and behaves like the top. A capacity figure
in routes per second means nothing without it.

**Component ceilings, measured separately:**

| | rate | CPU |
|---|---:|---:|
| Collector alone (writer stopped) | 233,009 msg/s | 3.6 cores |
| Collector with the writer running | ~180,000 msg/s sustained | 3.6 cores |
| Writer alone (NATS pre-filled) | 170,425 rows/s at 1 NLRI | 0.5 core |
| Both together | ~99,000 rows/s at 1 NLRI | — |

These figures are for single-copy streams on one NATS server, on one host.
"Three-copy streams on three servers" below measures the chart's default
configuration on a cluster.

Neither daemon is CPU-bound. They contend on the single NATS server, which
writes every published envelope to disk while serving the consumer from the
same disk, and that roughly halves the writer. NATS is the first thing to
scale, not either daemon. Raising the writer's batching (`batch_rows` 5000 to
50000, `fetch_batch` 500 to 5000, `batch_wait` 2s to 1s) gave 95,222 rows/s
against 99,261 untuned: no improvement.

**Zero loss.** Across 115,048,000 published route envelopes, every publish,
loss, insert and decode error counter stayed at 0, and every run's
`uniqExact(prefix)` equaled its row count (20,000,000 of 20,000,000 on the
largest).

**Storage is dominated by per-observation columns.** At 10 M rows and 1 NLRI
per UPDATE, `prefix` cost 4.437 B/row, `ts_collector` 4.437, `stream_seq`
4.142 and `seq` 3.907. The identity columns a normalized design would remove
— `router_ip`, `peer_ip`, `session_id`, `next_hop` — summed to 0.217 B/row
(`router_ip` alone 0.069), because they lead the sort key. Storing each
prefix once with a list of routers would save nothing.

**Column codecs.** `DoubleDelta, ZSTD(1)` on `seq`, `stream_seq`,
`ts_collector` and `ts_router`, and `ZSTD(3)` on `prefix`, A/B through the
real write path:

| | rows/s end to end | B/row | stored |
|---|---:|---:|---:|
| default `LZ4` | 98,884 | 17.71 | 168.9 MiB |
| with codecs | 98,395 | 4.11 | 39.2 MiB |

4.3x less storage for a 0.5% throughput difference, inside run-to-run noise;
the writer's half-core at saturation absorbs `ZSTD`'s cost.

**RIB page cost.** Through `query.RIBPageUnicast` against one peer holding
1,000,000 routes, a page took 487 ms at `limit=1000` and 495 ms at
`limit=10000`, flat in page depth too: the whole peer's RIB is aggregated on
every page, whatever the limit. A full walk took 8m 07s at 1,000 per page and
50s at 10,000.

**What this decides.** `deploy/clickhouse/schema.sql` declares the codecs
above on every table (they arrived before the schema was squashed to its
version-1 baseline). The schema keeps one row per observation rather than
normalizing routers out. `vantage query rib` walks at `MaxRIBPage`
(10,000) unless told otherwise, since a smaller page only adds whole-RIB
aggregations. `vantage loadgen`'s `-nlri-per-update` flag exists because
packing is the variable that decides a load test's result.

**Not covered.** Uniform synthetic data (one `/24` per route, one AS path per
peer), so the compression figures are optimistic for `as_path`, `next_hop`
and the community columns. One host for every component, so the NATS
contention is an upper bound. `route_unicast` only. Bursts, not a soak:
nothing about hours of steady state, merge pressure, the 90-day TTL,
withdrawals, session resets or reconnect storms.

### Three-copy streams on three servers

**Measured 2026-09-24.** The question: what the chart's default costs.
Three NATS servers, each publish acknowledged only once a second server has
stored it, against one-copy streams on the same cluster.

**Method.** The Helm chart as `docs/deploying.md` installs it, on a
four-node Kubernetes 1.36 cluster (Talos v1.13.7): three NATS 2.11.6
servers, one per node, each on a 20Gi Longhorn volume; one collector; one
writer; ClickHouse 24.8 under the operator, on a Longhorn volume on one of
the same three nodes. `vantage loadgen` ran in the collector's pod (an
ephemeral container, so its 127.x source addresses reach the collector over
loopback): 20 routers x 8 peers x 25,000 prefixes = 4,000,000
`route_unicast` rows per run. End to end is from the dump's start to the
writer's last inserted row, read from `vantage_sink_rows_inserted_total`
once a second. One-copy was the same install upgraded with
`collector.streams.replicas: 1`, then upgraded back.

| streams | NLRI per UPDATE | rows/s end to end | loadgen send rate (msg/s) |
|---|---:|---:|---:|
| three-copy | 1 | 38,171 | 44,309 |
| three-copy | 50 | 162,178 | 1,333 |
| one-copy (same cluster) | 1 | 51,958 | 46,815 |
| one-copy (same cluster) | 50 | 184,717 | 1,333 |

At 50 NLRI per UPDATE the send rate is loadgen's paced 60-second hold, not a
limit; the pipeline finished each 4,000,000-row dump in 22 to 25 seconds.

**Zero loss.** Every run landed exactly 4,000,000 rows and 4,000,000 distinct
prefixes in ClickHouse, and no publish, reject, abort, loss, decode or
insert-error counter moved.

**What this decides.** Three copies cost 27% of end-to-end throughput at one
prefix per UPDATE (38,171 against 51,958 rows/s) and 12% at 50 (162,178
against 184,717): the price is per message, so it shrinks as routers pack
more prefixes into each UPDATE. The default stands: even unpacked, three
copies carried about 38,000 route changes a second with no loss. A
deployment that expects sustained churn near that rate should measure its
own.

**Not covered.** One run per cell. Longhorn replicates each volume itself,
so every JetStream copy may be stored more than once underneath; the
figures include that cost and do not separate it. The nodes also run the
cluster's other workloads. A dump, not churn: nothing about hours of steady
state or a NATS server restarting mid-load.

## RIB read path

**Measured 2026-09-05, re-measured after shipping.** The question: the RIB
page's cost is proportional to the peer's whole RIB (see [Collector and
writer load test](#collector-and-writer-load-test)). What fixes that — a
materialized current-state projection, or a different reader on the base
table?

**Method.** 1,000,000 routes on one peer in a scratch database, seeded
directly so routes could span a monthly partition boundary. The rig
reproduced the earlier RIB page figure through `query.RIBPageUnicast` (506 ms
at `limit=1000`, 532 ms at `limit=10000`) before being trusted.

**No correct reader is proportional to the page.** A projection read without
`FINAL` did stop early — 4 ms, 40,960 rows read — but against unmerged
duplicates its first page of 1,000 held 778 distinct routes, 222 carrying a
stale next hop. Every reader that answered correctly read the whole peer:

| reader, 1,000-row page | `read_rows` |
|---|---:|
| base table `argMax` | 1,500,644 |
| projection, `FINAL` | 1,298,519 |
| projection, `LIMIT 1 BY` | 630,784 |

**The projection was rejected.** Keyed without `session_id` and versioned on
`stream_seq`, it silently dropped routes whenever a superseded session kept
publishing (BMP transport lost without a Peer Down): with 200,460 of
1,000,000 routes carrying such a straggler, the base table reported
1,000,000 current routes and the projection 799,540. It also cost 23% of
ingest throughput and 27% more storage (measured 2026-09-04). Separately,
`do_not_merge_across_partitions_select_final=1` returned a route living in
two partitions twice (778 distinct routes in a page of 1,000) and must never
be set on this schema; plain `FINAL` across two partitions cost nothing extra
(56 ms against 59 ms).

**Two-phase reader on the base table.** Phase one resolves the page's route
keys with one `argMax`; phase two fetches the eleven attribute columns for
just those keys. The deciding run measured 652 ms for the single statement
against 87 ms two-phase (7.5x). Re-measured after shipping, through the
shipped code on a freshly recreated container:

| statement, limit 1000 | deciding run | after shipping |
|---|---:|---:|
| one statement (before) | 652 | 677 |
| phase one, page keys | 77 | 97 |
| phase two, range bound | 10 | 20 |
| **two-phase total** | **87** | **117** |
| **speedup** | **7.5x** | **5.8x** |

The baseline reproduced within 4%, so the speedup is a band, **5.8x–7.5x**,
not the single 7.5x the deciding run produced. Both phases read the same
~1.5 M rows; phase two is cheaper because it runs eleven `argMax` state
machines over a thousand keys instead of a million. Parity with the old
statement was 0 disagreeing rows on 1,000 keys, and the parity check was
itself shown to fail under three deliberate mutations (299, 208 and 218
disagreeing rows).

**Wide route filters share the defect.** Through `query.Routes` on the same
peer, an exact-prefix lookup took 11 ms (the bloom filter on `prefix` already
works), while `origin_asn=` took 615 ms and `community=` 605 ms, for the same
whole-RIB reason. [Wide route filters](#wide-route-filters) records
how that was fixed.

**What this decides.** `query/rib.go` reads a RIB page in two phases over the
base table, with no projection and no schema change. Whether another page
exists is decided by phase one alone (it resolves `limit+1` keys), so a
withdrawal racing between the phases costs only the withdrawn row rather than
ending the walk early. Per-page cost is still proportional to the peer's
whole RIB; the two-phase reader changes the constant by roughly 6x.

**Not covered.** Synthetic, uniform data (one router, one peer, one `rib`,
one family, uniform `/24`s). The superseded-session interleaving was
constructed, not observed in a real archive. Two partitions, not the four a
90-day TTL admits. Unicast only.

## Wide route filters

**Measured 2026-08-30; the scaling numbers superseded 2026-09-05.** The
question: `origin_asn=`, `through_asn=` and `community=` on `/v1/routes`
(and the three single-family `/v1/routes/*` endpoints) must be evaluated as
a `HAVING` over the current-state `argMax` aggregates, never as a `WHERE`,
because a `WHERE` on a mutable attribute changes which row `argMax` picks
and reports stale attributes as live. A skip index cannot serve a
`HAVING`. What does that cost, does the answer need a cap, and does
anything make it cheaper?

**Method, 2026-08-30.** Every figure is `system.query_log` for the exact
statement a running `vantage-api` issued, after `SYSTEM FLUSH LOGS`. Two
archives: the dev stack's live archive (8,611 `route_unicast` rows, still
ingesting from a running collector), and an isolated pipeline — its own
NATS, ClickHouse, collector and writer — loaded by one `vantage bmpgen`
session to 3,211,674 raw rows over 2,130,000 distinct route keys (100
peers, `ipv4u` only, 14.68 MiB on disk).

At the dev stack archive's size every filter was imperceptible: 12–16 ms for the
page statement and 9–11 ms for its `total_matched` count, 0.47–4.7 MiB of
memory. At 3.2M rows:

| filter | `total_matched` | selectivity | statement | ms | `read_rows` | memory |
|---|---:|---:|---|---:|---:|---:|
| `origin_asn=64512` | 1,050,000 | 32.7% | page | 1,026 | 3,211,674 | 6.93 GiB |
| | | | count | 363 | 3,211,674 | 2.47 GiB |
| `through_asn=65050` | 10,500 | 0.33% | page | 1,160 | 3,211,674 | 6.83 GiB |
| | | | count | 357 | 3,211,674 | 2.34 GiB |
| `community=65000:100` | 0 | 0% | page | 960 | 3,211,674 | 6.83 GiB |
| | | | count | 359 | 3,211,674 | 3.09 GiB |

**The cost is flat across a 100x spread in selectivity**, because the
`HAVING` runs after the whole `GROUP BY` however few rows survive it.
Memory tracks the number of distinct route keys, not rows or matches:
single-threaded, the same `through_asn` statement still needed 5.69 GiB
(7,334 ms), and 16 explicit threads took it to 10.83 GiB (1,994 ms). The
page statement costs 2–3x its count regardless of how many rows it returns,
because the extra cost is the width of its `SELECT` list, computed for every
group before the `HAVING` discards most of them.

**Method, 2026-09-05.** A candidate-key semi-join: the same statement,
restricted to route keys that a much cheaper subquery (the same scoping and
the same `HAVING`, computing two `argMax` instead of eleven) says can match,
with a raw-row `has()` pre-filter (`has(as_path, X)`) narrowing that
subquery. The pre-filter is a superset of the answer, so it cannot lose a
route; the outer statement still aggregates every row of every surviving
key. Measured on one peer holding 1,500,644 rows over 1,000,000 route
keys, `origin_asn=65413` matching 2,000 of them (0.2%), best of three,
interleaved. Parity was checked as a row count plus a checksum over every
returned attribute, and the checksum was shown to change under a deliberate
mutation before its agreement was trusted.

| statement | ms | `read_rows` | peak memory |
|---|---:|---:|---:|
| `HAVING` baseline | 683 | 1,500,650 | 4.43 GiB |
| semi-join | 227 | 3,001,297 | 1.28 GiB |
| semi-join + `has()` pre-filter | **53** | 3,001,297 | **71 MiB** |

Read rows double, because the table is scanned once for candidates and once
for the answer; the cost was never the rows read but the `argMax` state for
every key. A `bloom_filter` skip index on `as_path` changed nothing (53 ms,
same rows, same memory): it pruned 0 of 185 granules, because an origin
AS's routes are spread across the whole time-ordered dump (184 of the 185
granules here), and that follows from the sort key, not from this data.

Re-measured after shipping, through `query.Routes` rather than hand-derived
SQL:

| `/v1/routes` shape, 1.5M-row peer | before | after |
|---|---:|---:|
| `origin_asn=`, 0.2% selectivity | 708 ms | **56 ms** |
| `through_asn=`, **100%** selectivity | 622 ms | **973 ms** |
| exact `prefix=` (no semi-join rendered) | 11 ms | 10 ms |

**The worst case is a regression.** A filter that matches every route
prunes nothing, so the subquery is pure overhead: 56% slower and 4.59 GiB
against 4.43. The penalty is bounded by what the candidate subquery costs,
roughly a third of the full statement.

**What superseded what.** The 2026-08-30 figures stand as the cost of the
`HAVING` baseline and as the evidence for the cap below. They no longer
describe what a selective wide filter costs today: the semi-join shipped on
2026-09-05, and the 56 ms figure is the current one for that case. The two
rigs differ (2.13 M keys over 100 peers against 1 M keys on one peer), so
the 1,026 ms and 56 ms figures are not a before-and-after pair; 708 ms and
56 ms are.

**What this decides.** `/v1/routes` refuses a request with none of
`prefix=`, `covers=`, `origin_asn=`, `through_asn=` or `community=`: with
none, the answer is every route in the fleet, which is what the paginated
`/v1/rib` paths are for. Each family's statement is capped at the API's
`max_page` (default 10,000 rows). `meta.total_matched` carries the full
match count (summed across the three families on `/v1/routes`) and a
`truncated` warning states both numbers whenever the cap cut the answer
short, because the count is the only thing that makes a capped answer
honest. The page and its count run concurrently, since at the 3.2M-row
scale the count alone cost about a third of the page. The wide filters are
answered through the semi-join, and no skip index was added: no schema
change, no migration.

**Not covered.** The crossover selectivity at which the semi-join stops
paying is not measured; the 2026-09-05 rig held only 0.2% and 100% shapes.
The semi-join variants were measured for `origin_asn=` only; `through_asn=`
and `community=` share the structure and the superset argument, but only
their baselines were measured. The 3.2M-row archive carried no community
data at all, so `community=` there measured a scan that matched nothing,
and its `origin_asn=` match rate is a generator artifact (every synthetic
path ends in AS 64512). All scaling data was synthetic and uniform: one
router, `route_unicast` only, bursts rather than a soak. The 2026-09-05
answer (2,000 rows) fit under the cap, so how the rewrite interacts with a
capped answer is untested, and its count statement was not measured
separately.

## Topology cost

**Measured 2026-09-11.** The question: `/v1/topology` builds an AS-path graph
from current routes. Which of its scopes are seeks and which are scans, and
does any need the `max_unscoped_since` clamp `/v1/events` and
`/v1/collection/*` use?

**Method.** 2,000,000 distinct `route_unicast` routes loaded through `vantage
loadgen` (10 routers × 10 peers × 20,000 prefixes, 100 NLRIs per UPDATE, AS
paths of length 6) into a real collector and writer, with sessions held open
so every route was current state. The rig was validated first: an exact
prefix lookup through the shipped `query.Routes` read under 3% of the table
in 3–14 ms. The edges statement was then rendered through the real
`RouteFilter` and semi-join code and run once per scope, three runs each:

| scope | ms | `read_rows` | % of table | memory | edges returned |
|---|---:|---:|---:|---:|---:|
| `prefix=` | 11–17 | 57,660 | 2.88% | 424K–481K | 5 |
| `router=` + `peer=` | 29–32 | 24,892 | 1.24% | 22.0–22.5M | 5 |
| `origin_asn=` (10% selectivity) | 237–262 | 2,439,119 | 122.0% | 283–288M | 5 (200,000 routes) |
| `community=` (matched nothing) | 19–25 | 2,000,622 | 100.0% | 750K–868K | 0 |
| no scope (context only) | 388–413 | 2,000,316 | 100.0% | 2.3–2.5G | 14 (2,000,000 routes) |

`prefix=` and `router=` + `peer=` are seeks. `origin_asn=` is not: the
semi-join reads candidate keys once and the outer aggregate a second time,
and every surviving route is exploded into its AS-path edges. `community=`
read the whole table but was cheap only because no route matched; it says
what the scan costs, not what a matching community filter costs.

**What this decides.** `/v1/topology` has no `since=` and no
`max_unscoped_since` clamp: the clamp bounds a time window, and this endpoint
builds its graph from current state on every scope. The case the clamp
exists to police — a request with no scope at all — is refused outright
before any SQL runs. The wide scopes carry the same unbounded cost
`/v1/routes/unicast` already ships for the same filters. Building this rig
also showed that the shared `RouteFilter` predicates render `router=` +
`peer=` with no prefix as a query for the empty prefix, which returned 0 rows
against 20,000 real routes; `/v1/topology` renders its own predicates for
that reason.

**Not covered.** The shipped endpoint runs three statements per family
(edges, nodes and a count), not the one measured here, so a wide scope's real
cost is about three times the figure above — inferred, not re-measured
through the shipped path. One archive size and one selectivity per wide
scope; `through_asn=` was not measured directly and is more likely than
`origin_asn=` to match a large share of an archive. `peer_events` was not
scaled (see [Unscoped events](#unscoped-events) for that table's cost). The
generator's AS numbering is what made `origin_asn=` 10% selective, not a real
fleet's distribution.

## Peer event deduplication

**Measured 2026-09-07.** The question: `peer_events` is a
`ReplacingMergeTree`, so a read that lands between an insert and the
background merge can see duplicate rows for one `(router_ip, peer_ip, rib,
ts_router, stream_seq)`. How should `GET /v1/events` dedup them?

**Method.** Three candidates on a lab archive's busiest peer (235 of 2,843
rows), `LIMIT 100`: (A) no dedup, (B) `FINAL`, (C) `argMax(field,
ts_collector)` grouped by the full key.

| candidate | ms | `read_rows`, `LIMIT` 100 | `read_rows`, `LIMIT` 1000 |
|---|---:|---:|---:|
| A, no dedup | 2 | 2,843 | 2,843 |
| B, `FINAL` | 1, 2, 2 | 2,843 | 2,843 |
| C, `argMax` | 3, 3, 3 | 2,843 | 2,843 |

The numbers cannot separate the candidates: the whole table was one part
smaller than one index granule (8,192 rows), so every query read all of it
regardless of predicate, `LIMIT` or dedup strategy. At the time it held zero
duplicates.

**What this decides.** `query/events.go` dedups with `argMax` over the full
key (candidate C), chosen for correctness, not speed. No dedup relies on a
merge having happened, which nothing guarantees. `FINAL` depends on
ClickHouse reconciling parts at read time; `argMax` resolves each key's
freshest row from whatever rows the read sees, merged or not. C cost ~1–2 ms
more than A and B here. None of the three paginates in proportion to
`LIMIT`, because all sort on `ts_collector`, which is not a prefix of the
table's sort key; the cost of that at volume is measured in [Unscoped
events](#unscoped-events).

**Not covered.** The archive could not demonstrate a duplicate, and at 2,843
rows it could not separate the candidates on cost. One host, one snapshot.

## Current-state tables

**Measured 2026-09-22.** The question: eight `*_current` tables, fed by
materialized views from the history tables, hold live routes, peers and
link-state objects with no TTL, pruned by a writer-side cleanup that deletes
each router's superseded sessions (`sink/cleanup.go`; see "Current-table
cleanup" in `docs/operating.md`). What do the views cost to ingest behind,
what do the current tables cost to read, and what does cleanup cost to run?

**Method.** Run on the same 32-core host (AMD Ryzen 9 9950X3D, load average
about 0.9 before the runs) as the published
[Collector and writer load test](#collector-and-writer-load-test), against
locally built `vantage-collector`, `vantage-writer` and `vantage` binaries
from a build made before the current-table DDL reached its final form. A
throwaway NATS container fed two scratch ClickHouse databases
on the dev ClickHouse (24.8.14.39): `vantage_spike_base` (that point's
`schema.sql`, history tables only) and `vantage_spike_views` (the same plus
the current-table DDL — 8 tables and 9 materialized views, none of the
eight carrying a TTL or a `PARTITION BY`). The dev stack's own `vantage`
database was read only (`route_unicast` count 685, unchanged throughout)
and never written to.

**Ingest cost.** `vantage loadgen -target 127.0.0.1:21019 -routers 20 -peers
8 -prefixes 62500 -nlri-per-update 1`: 10,000,000 `route_unicast` rows,
runs alternating base/views/base/views, both runs reported with no
best-of selection:

| run | arm | s to 10 M | rows/s end to end | rows/s from first row |
|---|---|---:|---:|---:|
| 1 | base | 99.42 | 100,584 | 100,912 |
| 1 | views | 106.07 | 94,276 | 94,581 |
| 2 | base | 98.00 | 102,042 | 102,397 |
| 2 | views | 107.02 | 93,437 | 93,716 |

The base runs reproduced the published 99,261 rows/s within +1.3% and
+2.8%. The views/base ratio (the ingest gate) averaged 93,857 / 101,313 =
**0.926** (paired: run 1 0.937, run 2 0.916; worst cross-pair 93,437 /
102,042 = 0.916), against a 0.85 threshold. Every run: `uniqExact(prefix)` =
`count()` = 10,000,000, every publish/reject/insert/decode-error and
envelope-lost counter stayed at 0, and loadgen sent 10,000,180 messages at
200,000-206,000 msg/s.

Per-table insert timings (`system.query_log`, `QueryFinish`):

| run | tables | inserts | written rows | total ms | avg ms | p99 ms |
|---|---|---:|---:|---:|---:|---:|
| base-1 | route_unicast | 2,000 | 10,000,000 | 17,590 | 8.8 | 13 |
| base-2 | route_unicast | 2,000 | 10,000,000 | 17,545 | 8.8 | 14 |
| views-1 | route_unicast + route_unicast_current | 2,000 | 20,000,000 | 27,483 | 13.7 | 20 |
| views-2 | route_unicast + route_unicast_current | 2,000 | 20,000,000 | 27,340 | 13.7 | 20 |

`system.query_views_log`: `route_unicast_current_mv` took 6,377 ms and 6,327
ms over 2,000 blocks and 10,000,000 rows; `peer_current_mv` took 0 ms for
320 rows. Per batch, the view makes an insert 56% slower (8.8 to 13.7 ms),
but end to end the cost is only 7%, because the writer is not bound by its
insert time: about 18% of wall time inserting without the views, 26% with
them, against NATS contention as the limit either way.

Storage after the views-1 run (all parts active, 10,000,000 unique routes):
`route_unicast` 35.15 MiB (3.69 B/row), `route_unicast_current` 49.08 MiB
(5.15 B/row) — the current table is 1.4x the size of history for a pure
dump. Its sort key orders by `(…, session_id, peer_ip, rib, family, prefix,
path_id)`, not by `ts_router, stream_seq`, so the `DoubleDelta` codecs on
`seq`, `stream_seq` and the timestamps see unsorted sequences — inferred
from the key and the codecs, not measured per column. Only 50 MB at this
scale.

**Read cost.** A fresh `vantage_spike_views`: `vantage loadgen -routers 1
-peers 1 -prefixes 1000000 -nlri-per-update 1 -hold 3h` gave 1,000,000
rows, a real Peer Up and a live session; a direct re-advertisement of every
even-`seq` route (500,000 rows, new `next_hop 10.0.0.2`, 2,000 of them
retagged origin AS 65413) brought history to 1,500,000 rows over 1,000,000
keys, matching the published rig's shape (1,500,644). The view populated
`route_unicast_current` with 1,500,000 rows in 4 parts; `FINAL` gave
1,000,000. Each statement's exact SQL was read from `system.query_log`,
rewritten with `route_unicast` swapped for `route_unicast_current` and
`peer_events` for `peer_current` (the swap topology and RIB reads make),
and replayed 3 times per arm, best of three shown. Parity: the sorted
output of every statement was identical between history and current (md5),
both unmerged and merged.

`RIBPageUnicast`, `limit=1000`:

| source | total ms | phase 1 ms / read_rows | phase 2 ms / read_rows |
|---|---:|---:|---:|
| history (called directly) | 175 / 154 / 155 | 151, 132, 133 / 1,500,003 | 20 / 1,500,005 |
| history (replayed) | 151 | 130 / 1,500,003 | 19 / 1,500,005 |
| current, unmerged (4 parts) | 143 | 122 / 1,500,003 | 19 / 1,500,005 |
| history (replayed, second pass) | 157 | 135 / 1,500,003 | 20 / 1,500,005 |
| current, after `OPTIMIZE ... FINAL` (1 part) | **107** | **89** / 1,000,003 | 16 / 1,000,005 |

`query.Routes`, `origin_asn=65413` (0.2% selectivity, 2,000 rows returned):

| source | ms (3 runs) | best | read_rows |
|---|---|---:|---:|
| history (called directly) | 61, 59, 64 | 59 | 3,000,008 |
| history (replayed, unmerged) | 60, 64, 64 | 60 | 3,000,008 |
| current, unmerged | 59, 64, 62 | 59 | 3,000,008 |
| history (replayed, second pass) | 64, 63, 65 | 63 | 3,000,008 |
| current, merged | 43, 45, 43 | **43** | 2,000,008 |

The current table does not make either read proportional to the page: both
statements still aggregate the whole peer. The gain is only the superseded
rows a merge has removed — nothing before a merge, and 1,500,000 to
1,000,000 rows read (about 1.3-1.5x) after one, exactly this rig's
re-advertisement ratio. Phase 1's memory was 675-893 MiB on both sources.

**Cleanup cost.** With only two sessions, the amended floor rule (the older
of a router's two newest sessions) targets nothing, so cleanup was measured
with a third and, for a repeat run, a fourth session added (each 1,000,000
rows, 1 hour and 2 hours behind the live session).

The natural `DELETE` statement — a `WITH floor AS (…)` referenced from
the `IN (...)` — fails on ClickHouse 24.8 on any table holding parts
(`Code: 341 … Code: 60. DB::Exception: Unknown table expression identifier
'<db>.floor' … (UNKNOWN_TABLE)`, because the mutation rewrites the CTE
reference as a table). It "succeeds" silently on an empty table, since no
part runs the mutation, and a failed mutation stays queued (`is_done = 0`,
retried in the background) until `KILL MUTATION`. The shipped statement
inlines the floor as a subquery instead. Measured with it, seven tables
first and `peer_current` last:

| run | table | rows before | targeted (count) | count ms / read_rows | DELETE ms (query_log) | DELETE wall s |
|---|---|---:|---:|---:|---:|---:|
| 2 sessions | route_unicast_current | 2,000,000 | 0 | — | — | 0.027 |
| 3 sessions, 1 | route_unicast_current | 3,000,000 | 1,000,000 | 15 / 3,000,003 | 205 | 0.211 |
| 3 sessions, 1 | peer_current | 3 | 1 | 2 / 6 | 10 | 0.013 |
| 3 sessions, 2 | route_unicast_current | 3,000,000 (+1 M masked) | 1,000,000 | 16 / 4,000,004 | 219 | 0.224 |
| 3 sessions, 2 | peer_current | 3 | 1 | 2 / 8 | 9 | 0.012 |

The six empty tables took 4-8 ms each. Every mutation reached `is_done = 1`.
Run 2's `count()` read 4,000,004 rows: lightweight-deleted rows stay in the
part, masked, until a merge rewrites it. `query_log` reports `read_rows = 0`
for the DELETE itself (the mutation's work is not attributed to it), so the
ms column is the synchronous wait (`lightweight_deletes_sync = 2`).

**Tombstone growth.** `vantage loadgen` only announces, so the 10,000,000-row
ingest runs above carried no withdrawals; `route_unicast_current FINAL`
after views-1 held 10,000,000 rows, all live. A separate rig, `vantage
bmpgen -peers 2 -updates 10 -churn-prefixes 200 -churn-rounds 50` (each
round announces 200 new prefixes and withdraws 101), measured the withdrawn
shape:

| | rows | withdrawn | announced |
|---|---:|---:|---:|
| history `route_unicast` | 30,120 | 10,100 | 20,020 |
| `route_unicast_current` (`FINAL`) | 20,000 | 10,100 | 9,900 |
| history `argMax` per key (today's reader) | 20,000 keys | 10,100 | 9,900 live |

The current table held 20,000 rows for 9,900 live routes: **10,100
tombstones, 51% of it.** A tombstone is a distinct key whose newest event in
the session is a withdrawal; the count grows with distinct prefixes
withdrawn over a session's life, not with the number of events, and a
tombstone leaves only when cleanup removes that session.

**What this decides.** The materialized views cost about 7% of ingest
throughput end to end (0.926 against the 0.85 gate) and about 1.4x the
storage of history for a pure dump, at this rig's scale. Neither
`RIBPageUnicast` nor a wide `query.Routes` filter reads in proportion to the
page against a current table; the current tables' value is correctness past
retention, not read speed. The shipped cleanup statement inlines its floor
subquery rather than using a `WITH`, which fails on ClickHouse 24.8 once a
table holds parts, and removed 1,000,000 rows in about 210 ms in this rig.
A session that sees many distinct prefixes come and go can leave most of
its rows in a current table as tombstones until cleanup reaches it.

**Not covered.** `route_unicast` only at ingest volume; the VPN, EVPN and
link-state views received no load, so their per-insert cost and the ingest
gate are unmeasured for them. One host, a roughly 100-second burst, not a
soak: nothing about merge pressure on the current tables over hours. The
current-table read figures come from replayed statements with the source
table swapped, not from the shipped Go code path, and cover only a fully
merged and a freshly written state. The writer-alone ceiling (170,425
rows/s published) was not re-measured with the views running. The insert
share of wall time grows from 18% to 26% with the views, so without NATS
contention the views' share of end-to-end cost would be larger than the 7%
measured here. How fast tombstones accumulate against a real router's churn
rate was not measured.
