# CLI reference

Flags and behavior for `vantage`'s subcommands, `vantage-collector`,
`vantage-writer`, `vantage-api`, and the dev-only `bmp-replay` sender. For what these
processes do once running, see `docs/architecture.md`.

## Provisioning streams directly: `vantage streams init`

`vantage-collector` provisions its own five JetStream streams on every
startup (`natsutil.EnsureStreams`), so the quickstart in `docs/deploying.md`
never needs `streams init`. It exists for managing streams independently of a
running collector — before one has ever started, or against a NATS
deployment you administer separately:

```
vantage streams init [-nats URL] [-nats-ca FILE] [-nats-cert FILE] [-nats-key FILE]
                     [-partitions N] [-replicas N] [-ls-replicas N] [-allow-lower-replicas]
                     [-routes-max-bytes N] [-raw-max-bytes N]
```

On the Helm chart's default NATS, which requires mutual TLS, pass
`-nats-ca`, `-nats-cert` and `-nats-key` (see "NATS TLS" in
`deploy/helm/README.md`).

`streams init` never lowers an existing stream's replica count by
accident. `-replicas` and `-ls-replicas` set the count for streams that do
not exist yet. A stream that already exists keeps its current count unless
you pass one of those flags explicitly, so a plain `vantage streams init`
against the chart's three-copy streams leaves them at three copies. An
explicit count lower than a stream's current one is refused, and nothing is
changed, unless you also pass `-allow-lower-replicas`. RAW is always one
copy.

`-ls-replicas` matters: the stream library (`natsutil`) defaults the LS
stream (BGP-LS/topology data) to **3** replicas regardless of the other
streams' replica count, because topology data is higher-value than route
data. A single, non-clustered NATS server —
the default target of this command on a laptop — **rejects a 3-replica
stream create outright**. `-ls-replicas` defaults to `0` ("follow
`-replicas`", falling back to `1`), so plain `vantage streams init` against a
single-node dev NATS provisions LS at 1 without you having to say so, while
`vantage streams init -replicas 3` against a real 3-node cluster gets LS at 3
too. Set `-ls-replicas` explicitly only to diverge from `-replicas`.
`vantage-collector`'s own config (`streams.ls_replicas` — see
`docs/architecture.md`) applies the identical fallback for the same reason.

## `vantage debug`

```
vantage debug [-nats URL] [-filter SUBJ] [-from-start]
```

Subscribes and pretty-prints every `vantage.v1.Envelope` it sees as
[protojson](https://protobuf.dev/programming-guides/json/). **protojson
output is deliberately not byte-stable** (field order and whitespace are not
part of its contract, and different protobuf runtime versions may render the
same message differently) — treat `debug`'s output as human-readable, not
something to diff byte-for-byte between runs or golden-file it directly.

### Subject tokens are hex, and `debug` prints both forms

Every router/peer address token in a subject is lowercase hex of the address
bytes (8 hex digits for IPv4, 32 for IPv6; `subjects`), not dotted
text — this makes address encoding injective (round-trips exactly, never
collides two distinct addresses) at the cost of not being human-readable on
sight. `debug` prints **both**: a decoded, readable form first, then the
real subject in brackets when it differs, e.g.:

```
── vantage.v1.route.ipv4u.192.168.65.1.10.0.0.9  [vantage.v1.route.ipv4u.c0a84101.0a000009]
```

**The bracketed form is the one to paste** into `-filter`: filtering on the
humanized text (`vantage.v1.route.ipv4u.192.168.65.1.>`) subscribes
successfully and then silently matches nothing forever, because that is not
a real subject the collector ever publishes to.

### Subjects: 6 tokens published, 7 tokens stored

The collector always publishes **partition-free**: a route event's subject
is exactly `vantage.v1.route.{family}.{router}.{peer}` (6 tokens). The
ROUTES and LS streams insert a server-computed partition token themselves,
via a JetStream `SubjectTransform`, immediately after the `route`/`ls`
literal — so **as stored**, a route event's subject has 7 tokens:
`vantage.v1.route.{partition}.{family}.{router}.{peer}`. This only matters
for `-from-start` (which replays the ROUTES stream's own stored subjects);
the live subscription path always sees the 6-token, partition-free form.
`vantage debug`'s `-from-start` path adjusts a `route`-scoped filter for you
(`routesStreamFilter`, `cmd/vantage/debug.go`) — a filter you type as if
subjects were still 6 tokens keeps working — but any other tool reading
directly off the ROUTES/LS streams needs to account for the extra token.

## Reading the archive: `vantage query`

`vantage query` is the operator's read path: `routers`, `peers`, `routes`,
`rib` and `ls nodes|links|prefixes`, printed as a table or (`-o json`) as
JSON. It talks to `vantage-api` by default — `-api`, else the
`VANTAGE_API` environment variable, else `http://127.0.0.1:9473` — with
the bearer token from `-token` or `VANTAGE_API_TOKEN`. Setting the token in
the environment keeps it out of shell history and the process table.
`-dsn` queries ClickHouse directly instead, for an operator on a host that
can reach it; both paths return the same answer.

A peer reads `stale` when its collector has not been heard from for 90 s.
Through the API that threshold is the daemon's own `stale_after`. With
`-dsn` it is `-stale-after`, which defaults to the same 90 s, so the two
paths agree unless one of them is changed. Given without `-dsn`,
`-stale-after` changes nothing, and the command says so on stderr. `query
routers` shows stale peers in their own `STALE` column. When a `routes`,
`rib` or `ls` answer includes rows from a stale collector, both paths print
a `collector_stale` warning to stderr; stdout carries only the answer.

```
export VANTAGE_API_TOKEN=dev-token-not-a-secret
vantage query routes -origin-asn 65002
vantage query rib -router 10.0.0.1 -peer 10.2.0.2 -family vpn
```

`query rib` walks a whole RIB page by page, at 10,000 rows per page unless
`-limit` says otherwise (see [RIB read path](measurements.md#rib-read-path)
for why a larger page is cheaper). A walk reads one collector's view of the
router, so when more than one collector watches it, name one with
`-collector` (for example `-collector dev-c1`); without it the walk is refused
and the error lists the collectors to choose from.

## Capturing and replaying BMP: `vantage capture`, `vantage reparse`

`vantage capture` writes one router's raw BMP messages to a file, plus a
`.json` sidecar recording where they came from. `-window` arms a mirror on
the collector (through its admin listener, `-collector`, default
`127.0.0.1:9470`) and captures forward for that long; `-since` instead
replays what the RAW stream already holds from that far back. Exactly one
of the two is required.

The file is the BMP messages concatenated exactly as received — each
carries its own length — so `vantage reparse FILE` can feed it back through
the same session layer and parsers the collector uses and print what comes
out, without touching NATS or the archive. A truncated final record still
prints every message before it, alongside the error.

## Retiring a router or collector: `vantage purge`

`vantage purge -dsn DSN -collector ID [-router IP]` removes a retired
collector's view of a router, or everything a retired collector holds, from
the current tables. It needs `-dsn` because the API is read-only. It refuses
while the target is still live, or while its collector is past
`-stale-after` but was heard from less than 15 minutes ago, unless given
`-force`, and `-dry-run` shows what it would delete. See "Retiring a router
or collector" in `docs/operating.md`.

## CLI reference

```
vantage streams init [-nats URL] [-partitions N] [-replicas N] [-ls-replicas N]
                     [-allow-lower-replicas] [-routes-max-bytes N] [-raw-max-bytes N]
vantage debug [-nats URL] [-filter SUBJ] [-from-start]
vantage bmpgen [-target HOST:PORT] [-profile iosxr|nxos|frr] [-router NAME] [-vendor DESCR]
               [-peers N] [-updates N] [-churn-prefixes N] [-churn-rounds N]
               [-churn-interval DURATION] [-families LIST]
vantage loadgen [-target HOST:PORT] [-routers N] [-peers N] [-prefixes N] [-nlri-per-update N]
                [-aspath N] [-warmup DURATION] [-hold DURATION]
vantage capture -router IP -o FILE (-window DURATION | -since DURATION)
                [-nats URL] [-collector ADDR] [-max-bytes N] [-replay-timeout DURATION]
vantage reparse [-json] FILE
vantage purge -dsn DSN -collector ID [-router IP] [-dry-run] [-force] [-stale-after DURATION]
vantage query routers|peers|routes|rib|ls ... [-api URL] [-token T] [-dsn DSN] [-stale-after DURATION] [-o table|json]
vantage-collector -config PATH
vantage-writer -config PATH
vantage-api -config PATH

bmp-replay -capture PATH -targets HOST:PORT[,HOST:PORT...] [-sysname-prefix S]
           [-dial-timeout D] [-target-delay HOST:PORT=D[,...]]
```

Every subcommand that talks to NATS (`streams init`, `debug`, `capture`)
also takes `-nats-ca`, `-nats-cert` and `-nats-key` for a TLS-enabled NATS.
Running `vantage` with no arguments prints a usage summary;
`vantage <subcommand> -h` (for example `vantage query rib -h` or
`vantage query ls links -h`) lists every flag that subcommand takes.

`bmpgen`'s `-profile` picks which measured vendor identity it impersonates
(default `iosxr`): the `sysDescr` it sends is one captured from a real
router of that kind, so the collector's quirk matching sees a banner a
router actually sends. `-vendor` overrides that banner, at the cost of an
identity no router has been observed sending. `-router` defaults to
`bmpgen-<profile>` (its `sysName`, sent in the BMP Initiation message) — not
an IP address; the router token other tooling
sees in subjects/logs is always the TCP source address of `bmpgen`'s
connection to the collector, per BMP's own framing (a router's sysName is
metadata carried *inside* the session, not how it's addressed on the wire).
`bmpgen` sends one Initiation, then per simulated peer a Peer-Up (so
capabilities are on record before any route data — see `docs/quirks.md`'s
`QK_CAPS_MISSING`), `-updates` Route Monitoring UPDATEs, and one Stats
Report, then holds the TCP session open (Ctrl-C to close) — mirroring a real
router's long-lived BMP session rather than a one-shot dump.

`loadgen` is the other half of that pair and is **not** bmpgen with bigger
numbers. bmpgen is a correctness fixture: one router, a handful of precisely
shaped messages, a real vendor banner, everything built in memory first. Its
prefixes are `10.{peer}.{update}.0/24` with both indices as bytes, so it tops
out at 65,536 distinct prefixes and then silently repeats them — and repeats
collapse under `ReplacingMergeTree`, so a scale test built on it would
understate its own row count while reporting success.

`loadgen` streams instead of buffering, presents `-routers` simulated routers
from distinct `127.x.y.z` source addresses (router identity is the TCP source
address), and generates up to 26.2M distinct prefixes. **Always check a run's
`uniqExact(prefix)` against its row count** — two independently written
generators for this job have both shipped a prefix-collapsing bug, and both
times it was caught that way rather than by reading the code.

`-nlri-per-update` is the flag that matters most, because it is the variable
that decides both throughput and bytes-per-row: measured 2026-09-04 at 99k
rows/s and 17.6 B/row at one prefix per UPDATE, and 475k rows/s and 5.4 B/row
at 200, for identical row counts. A load-test result that does not state it has
not stated its result. Use `-hold` when the loaded routes need to still be
current state after the run — without it the sessions close, and the collector
correctly marks every peer `view_lost`.

Point it at a throwaway stack, never at an archive worth keeping. See
`docs/measurements.md`'s "Collector and writer load test" for a run and its
isolation setup.

`bmp-replay` (`cmd/bmp-replay`) is the third sender and the only one that does
not invent its traffic: it writes a committed `.bmpcap` to one or more
collectors and holds the sessions open. It is deliberately **not** a `vantage`
subcommand — `vantage` is the operator CLI and ships in `bin/`, while this is
dev-stack scaffolding — but it is still built and vetted by `go build ./...`.

It exists because neither other sender can produce BGP-LS: bmpgen's families
stop at `lu4`, and the routers that produced the link-state corpus are not
something `docker compose up` can bring along. The dev stack runs it
as the `ls-replay` service, which is what gives `ls_nodes`, `ls_links`,
`ls_prefixes` and `route_vpn` a second `collector_id` to render.

Three things about it are load-bearing rather than incidental:

- **All targets are dialed before any byte is written.** The collector records
  the TCP source as `router_ip`, so feeding several collectors from one process
  is what makes the replayed router genuinely dual-homed. A sender that fed
  them in turn would, on failing to reach the second, leave the first holding a
  complete session — the single-collector state this is meant to remove,
  reported as an error about the *other* collector.
- **It holds the sessions open.** `Session.Close` emits `KIND_VIEW_LOST` per
  peer, so exiting after the write would mark every replayed peer lost
  immediately. Same reason `loadgen` has `-hold`.
- **`-sysname-prefix` defaults to `replay-`.** A capture carries a vendor
  `sysDescr` and a device `sysName`, and they need opposite treatment: the
  banner is the identity quirk matching keys off and survives verbatim, while
  the device name would otherwise file every replayed row under the real
  router the bytes came from, alongside that device's own rows. Pass
  `-sysname-prefix ""` to send the name unchanged.
