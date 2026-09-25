# Architecture

`vantage-collector`, `vantage-writer` and the `vantage-api` read layer, and
the decisions behind each. For running any of this see `docs/deploying.md`
(Helm and the dev stack), for the CLI see `docs/cli.md`, and for lag/loss
semantics and Grafana see `docs/operating.md` and `docs/grafana.md`.

## `vantage-collector`

Takes one flag, `-config PATH` (a YAML file); the only other command-line
input is `-version`, which prints the build and exits. See `deploy/dev/collector.yaml` for the minimal example
this repo's compose stack runs. Every field is optional and defaulted if
unset:

```yaml
listen: ":11019"              # BMP TCP listener
proxy_protocol: "off"         # "off" (default) or "required" -- see below
allowed_sources: []           # router CIDRs allowed to send BMP; empty = any (warned at startup)
trusted_proxies: []           # CIDRs a PROXY header may come from; only with proxy_protocol "required"
max_connections: 1024         # concurrent BMP connections; excess are closed on accept
nats_url: "nats://127.0.0.1:4222"
collector_id: "vantage-collector-1"   # defaults to os.Hostname() -- see below
metrics_listen: ":9469"        # /metrics, /status, /healthz and /readyz
admin_listen: "127.0.0.1:9470" # mirror arming for `vantage capture`; unauthenticated, so loopback by default
log_level: info               # debug, info, warn or error
log_format: text              # text or json
nats_tls:                     # optional; absent means plaintext NATS
  ca_file: ""
  cert_file: ""
  key_file: ""
streams:
  partitions: 32
  replicas: 1                 # ROUTES/PEER/STATS (RAW is always 1)
  ls_replicas: 0               # 0 = follow `replicas` (see `vantage streams init` in docs/cli.md)
  routes_max_bytes: 0          # 0 = natsutil default (8GiB)
  raw_max_bytes: 0             # 0 = natsutil default (2GiB)
routers:                      # per-router overrides, keyed by IP
  10.0.0.1:
    vendor: cisco             # authoritative identity; outranks the banner
    os: iosxr
    force_quirks: ["QK_TS_ZERO"]
    disable_quirks: []
```

**`proxy_protocol: required` takes each router's identity from a PROXY
protocol header rather than from the socket.** It is for deployments that put
a front-end in front of the collector — HAProxy, NGINX stream, Envoy, or a
load balancer configured to emit one — where `conn.RemoteAddr()` is the proxy
and every router in the fleet would otherwise collapse onto that one identity.
Both header versions are accepted, v1 text and v2 binary.

There is no permissive mode and no fallback, deliberately. With `required`
set, a connection that does not open with a valid header is closed and nothing
is ever filed under the socket address. Forging a header costs 28 bytes, while
spoofing a TCP source needs an on-path position or a won sequence-number race,
so a mode that took whichever arrived would give away a protection TCP
provides for free. The flag is all-or-nothing per listener too: enabling it
without a front-end that actually emits headers closes every directly
connected router's session at once, so the flag and the front-end are one
maintenance action, not two. `deploy/helm/README.md` covers the deployment
side, including what a front-end has to be told to do.

**`collector_id` has to survive a restart.** It is half of session identity —
current state is `max(session_id)` per `(collector_id, router_ip)` — so if a
replacement process comes up under a different name, its sessions land under a
pairing the old ones never used and can never displace them. The previous view
is then stranded in the current tables rather than superseded. Current state
does not expire, so it stays until an operator purges it (see "Retiring a
router or collector" in `docs/operating.md`). After a hard kill, where the
collector never gets to emit its view-lost events, its peers stay recorded `up`
and its routes keep being served as current state.

The `os.Hostname()` default is stable on a VM, a laptop, and a Kubernetes
StatefulSet pod, and is **not** stable under a Kubernetes Deployment, ECS,
Nomad, or anything else that names a replacement differently from what it
replaced. Set it explicitly there — to the shard, not to the process. The
daemon logs a warning at startup whenever it fell back to the hostname.

**Config keys are validated, not merely parsed**: the YAML decoder rejects any
key it doesn't recognize (a typo like `nats_urls:` fails to start rather than
silently keeping the default), and `routers` keys must parse as an IP address
and name only known quirk IDs (`quirk.Registry` — see
`docs/quirks.md`) or the daemon refuses to start. `vendor`/`os` are lowercased
on load, and an `os` with no `vendor` is rejected rather than silently ignored
(both lookups key on the pair, so a bare `os` matches nothing).

**Why identity is configured rather than sniffed.** A BMP Initiation banner
often is not an identity. The built-in patterns are derived from banners
captured off real senders, and even so one of the three cannot be identified by
its banner: IOS-XR (XRd) sends `26.1.1`, a bare version with no vendor token at
all. NX-OS sends a chassis line that never contains "NX-OS" (the `Nexus` model
is what matches), and FRR sends `FRRouting 10.3_git`. An operator knows what
their devices are, so `vendor`/`os` are authoritative and the banner is used for the
**version**, which is deliberately not configurable: a version in a config file
goes stale at the next upgrade and nothing notices, while the router reports
its own on every session. Set nothing and the built-in anchors still apply, so
a zero-config deployment is no worse off. When a banner does name a vendor and
the config names a different one, the config wins and the collector logs a
`router identity conflict` warning.

**Override coverage**: `force_quirks`/`disable_quirks` take effect
for `QK_TS_ZERO`, `QK_CAPS_MISSING` and `QK_VERSION_UNPARSED`.
`QK_ADDPATH_HEURISTIC` is decided inside the BGP UPDATE parser, which has no
dependency on the quirk package, so listing it is accepted but currently has
no effect — see the "Ops escape hatch" section of `docs/quirks.md`.

## `vantage-writer`

Consumes the ROUTES/LS/PEER/STATS streams into ClickHouse, one durable pull
consumer per stream. Same single flag as the collector, `-config PATH`, and
the same strict decoding (an unrecognized key fails startup). See
`deploy/dev/writer.yaml`; every field is optional and defaulted if unset, and
the values below are those defaults:

```yaml
nats_url: "nats://127.0.0.1:4222"
clickhouse_dsn: "clickhouse://vantage:vantage@127.0.0.1:9000/vantage"
clickhouse_password_file: ""     # optional; the file's contents become the DSN's password
metrics_listen: "0.0.0.0:9472"   # /metrics, /healthz and /readyz
log_level: info                  # debug, info, warn or error
log_format: text                 # text or json
batch_rows: 5000                 # rows per insert
batch_wait: "2s"                 # how long a partial batch waits
fetch_batch: 500                 # messages per JetStream fetch
current_cleanup_interval: "1h"   # prune superseded sessions from the current tables; "0s" disables
nats_tls: {}                     # optional; same ca_file/cert_file/key_file as the collector
```

The two URLs hold credentials, so they are typed (`secret`) rather
than plain strings: they cannot be rendered by `fmt`, `slog`, JSON or YAML,
and every log line that names one prints a redacted form.

`clickhouse_password_file` lets a Kubernetes Secret be mounted as a file
instead of rendered into the config: its contents, one trailing newline
trimmed, replace the password in `clickhouse_dsn`. A DSN that carries a
password of its own (in the userinfo or as `password=`) alongside the file
is refused at startup, since two sources for one credential leave nobody
sure which is in effect. `vantage-api` takes the same key with the same
meaning.

**A message is acked only after ClickHouse has accepted the insert**, never
before, so a crash or a rejected batch redelivers rather than loses. The
duplicate that redelivery produces is byte-identical and collapses at merge
time — which is what the sort keys in the schema are for, and why changing
one is a schema-version change.

**Current state is split from history, and reads go to whichever one can
answer the question.** Materialized views feed eight `*_current` tables
(`route_unicast_current`, `route_vpn_current`, `route_evpn_current`,
`ls_nodes_current`, `ls_links_current`, `ls_prefixes_current`,
`peer_current`, `eor_current`) from their history counterparts as each
block lands, and `vantage-writer` separately prunes each current table of
superseded sessions on its own schedule (see "Current-table cleanup" in
`docs/operating.md`). RIB pages, router and peer listings, and topology's
live edges all read the current tables, so a route, session or link-state
object that has gone longer than the history retention without changing is
still reported correctly instead of aging out of the answer. Session
history, the fleet events console, churn, and topology's withdrawn edges and
`first_seen` all stay on history, because each needs more than the newest
row per key — a withdrawal, an intermediate value, or a key's first
appearance survives only there. The current tables carry no TTL, and
cannot: a TTL deletes by age against `ts_collector`, and a withdrawal often
becomes the oldest surviving row for its key well before cleanup or a
background merge reaches it, so expiring it by age would leave the
announcement it superseded as the newest row and report a withdrawn route
as live again. Retention (`docs/deploying.md`'s `retention.days`) therefore
applies only to the ten history tables; a current table's rows leave only
through the writer's own cleanup, or an operator's purge.

A collector that dies without closing its sessions writes no `view_lost`,
so the archive cannot learn of the death from the collector itself. It
learns from the collector's heartbeat instead. Every collector publishes a
`CollectorBeat` at startup and every 30 s, carrying its process start time.
Current-state reads resolve a stored `up` two ways. It reads `view_lost` when
its session is older than the collector's newest process start, because a
restarted collector's old sessions are certainly gone. It reads `stale` when
the collector has not been heard from for `stale_after` (90 s by default,
and configurable), because nobody can vouch for it. A stale peer's routes
are still served, with a `collector_stale` warning, because a NATS or writer
stall makes every collector stale at once, and an empty looking glass
during that incident would be worse than a flagged one.
Nothing is written to make either happen, and a collector that beats again
reads `up` again by itself. A router or collector that is gone for good stays
listed until an operator runs `vantage purge` (see "Retiring a router or
collector" in `docs/operating.md`).

See also: applying the schema and AS holder names (`docs/deploying.md`), lag
and data-loss semantics (`docs/operating.md`), and the Grafana dashboards
built on this data (`docs/grafana.md`).

## Collector health (`vantage-api`)

`GET /v1/collectors`, and the "Collectors" screen built on it, answer
one question per collector: is it there, and is it keeping up? Each card
comes from two sources unioned together — what the archive knows about a
`collector_id` (routers, peers, activity) and what that collector says about
itself right now, live, over `GET /status` on its own metrics listener
(process uptime, BMP messages, sessions active, publish errors and rejects).
A collector present in the archive but absent from config, and one
configured but not answering, are different situations and the card says
which.

`vantage-api` reaches each collector through a configured list:

```yaml
collectors:
  - id: dev-c1
    url: http://collector:9469
collectors_timeout: 2s   # optional, defaults to 2s; bounds the whole fan-out
```

**An empty (or omitted) `collectors:` list is a supported deployment, not a
broken one.** `/v1/collectors` still answers from the archive alone, and
every card says its process facts are unconfigured rather than showing them
as zeros. `deploy/helm/vantage` never leaves this hand-written: it generates
one entry per `collector.replicas` pod, addressed at that pod's own DNS name
rather than the shared headless Service name — see
`deploy/helm/vantage/templates/configmap-api.yaml` for why the shared name
is actively unsafe to paste here, not merely a style preference.

**Two things this screen deliberately does not show, and why the data
cannot honestly say them:**

- **No per-collector lag.** `vantage_sink_consumer_lag{stream}` is the
  writer's, per *stream*, in envelopes — one writer consumes all four
  streams on behalf of every collector, with no per-collector consumer
  anywhere in the pipeline. There is no number to attribute to one
  collector; Grafana's Fleet health dashboard already names this correctly
  ("Writer lag by stream"), and this screen does not restate it under a
  label that would imply otherwise.
- **No messages/s series.** The sparkline plots rows *archived* per minute,
  not messages received — a true messages/s series needs a time-series
  store sampling the collector's own counters, and this tool deliberately
  runs no TSDB. The two numbers are not close substitutes: one Route
  Monitoring message carrying 40 NLRIs becomes 40 archived rows, so a busy
  collector sending few, large messages and one sending many small ones can
  archive the same row count while their real message rates differ by an
  order of magnitude. The axis is labeled for what it plots.

## How the design got here

Vantage started on 2026-07-27 as a ground-up rebuild of the OpenBMP
ecosystem in Go, with no wire compatibility kept: BMP in, protobuf
envelopes on NATS JetStream, an archive in ClickHouse, and Grafana on top.
The first designs had no custom UI at all. The design envelope was set on
day one and has held since: the same architecture scales from a one-node
lab to thousands of BMP sessions by config alone, never by changing
subjects, schema or service code. The decisions that shaped what exists
now, in order:

- **Decode before storing (2026-07-31).** The first collector passed every
  family but IPv4 unicast through as raw bytes. VPNv4, labeled unicast and
  EVPN were given typed decoders before any ClickHouse table was built,
  because tables over undecoded blobs would have forced a schema migration
  and a mass reparse later. Extended communities (route targets) followed
  on 2026-08-10, and captures from a lab of real NX-OS and IOS-XR routers
  joined the hand-built fixtures as the test of every decoder.
- **An append-only event log, not a state table (2026-08-10).** The writer
  stores every advertisement, withdrawal and session event as its own row,
  because a log can answer both "what is true now" and "what did this look
  like at 03:14", and a state table can only answer the first. Current state
  was first derived from the log at read time (`argMax` per route key); it
  later moved into the materialized `*_current` tables described above,
  which are fed from the log, and the log stays the source of history.
- **BGP-LS decoded for a topology (2026-08-13).** The link-state NLRI and
  attribute 29 were decoded so an IGP topology could be assembled from them;
  link-state is always scoped to one router's view, because only some
  routers dump a complete topology and an edge assembled across sessions
  can be one that no longer exists.
- **A question-shaped read API (2026-08-24).** `vantage-api` is Go, written
  against `api/openapi.yaml` first, with static bearer tokens and
  session-pinned keyset cursors. On 2026-08-30 the decision to move Grafana
  onto it was reversed: ten of the thirteen dashboards aggregate, bucket by
  time or read link-state tables, none of which a row-returning API can
  serve, so Grafana stays on ClickHouse and the API serves people, scripts
  and LLM tooling.
- **A page costs a page (2026-09-05).** Measured against a 1,000,000-route
  peer, a RIB page cost the same whatever its size. It was fixed with a
  two-phase reader (keys, then attributes) on the base tables rather than a
  projection, so no schema change and no ingest cost.
- **Kubernetes via Helm (2026-09-05).** Chosen because the chart is the
  public install interface. ClickHouse is the operator's own; NATS ships
  with the chart, and mutual TLS on it followed on 2026-09-21, since
  vantage owns both ends of that transport.
- **A web UI that shows only what was measured (2026-09-05 onward).** The
  UI builds only the screens the archive can fill, and leaves out values the
  pipeline does not measure (updates per second, collector lag, RPKI) rather
  than inventing them. Session history, the fleet Events console, Monitor,
  AS paths and the link-state screen each landed once an endpoint could
  back them.
- **Counts are about the network, not the observers (2026-09-20).** Running
  two collectors against the same routers showed several screens and
  queries counting one route or router once per collector. Those counts now
  dedupe on identity with the collector removed, while tables keep one row
  per collector's observation, because two collectors can disagree.

Where this page and the code disagree, the code is what shipped.
