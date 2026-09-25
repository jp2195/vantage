# Deploying

Two ways to run vantage: `docker-compose.dev.yml` for a laptop, which
bundles its own ClickHouse and Grafana, and the `deploy/helm/vantage` chart
for Kubernetes, which does not — see "Kubernetes (Helm)" below for what the
chart does and does not stand up on its own.

## Quickstart (laptop)

Brings up the whole pipeline in Docker — NATS (JetStream enabled), two
`vantage-collector` instances, ClickHouse with the schema applied,
`vantage-writer`, `vantage-api` with the UI embedded, and Grafana — and
replays committed BMP captures into both collectors, so the stack has real
link-state, VPN and EVPN data on the first `up`, not an empty one waiting
on `bmpgen`:

```
docker compose -f docker-compose.dev.yml up --build -d
```

Open **<http://127.0.0.1:9473>** and paste the development token
`dev-token-not-a-secret` when the UI asks — that is the embedded UI which
`README.md` walks screen by screen. See the dev-stack warning in
`README.md`'s Quickstart section before reaching for Grafana below: its
anonymous access reaches a ClickHouse datasource that can run arbitrary
SQL, which is why every port in this stack stays on loopback except the two
collectors' BMP listeners (11019, 11119).

To push more synthetic traffic at the same stack:

```
go run ./cmd/vantage debug -filter 'vantage.v1.route.>'   &   # subscribe first — see below
go run ./cmd/vantage bmpgen -peers 2 -updates 10
```

`debug`'s default (no `-from-start`) is a **live** core-NATS subscription: it
only sees envelopes published *after* it subscribes, so start it before (or
concurrently with) `bmpgen`, not after. If you missed the live window, replay
the durable history instead:

```
go run ./cmd/vantage debug -from-start -filter 'vantage.v1.route.>'
```

Check the Prometheus counters at each end of the pipeline — the two
collectors on 9469 and 9569, the writer on 9472:

```
curl -s 127.0.0.1:9469/metrics | grep vantage_
curl -s 127.0.0.1:9569/metrics | grep vantage_
curl -s 127.0.0.1:9472/metrics | grep vantage_sink_
```

The same traffic is queryable in ClickHouse and plotted by the thirteen
dashboards at <http://127.0.0.1:3000> (see `docs/grafana.md` for what
anonymous access there does and does not expose):

```
docker compose -f docker-compose.dev.yml exec clickhouse \
  clickhouse-client -u vantage --password vantage \
  -q "SELECT count() FROM vantage.route_unicast"
```

Tear down:

```
docker compose -f docker-compose.dev.yml down
```


## Applying the schema

`deploy/clickhouse/schema.sql` creates the `vantage` database and
twenty-one tables: `schema_version`; the ten history tables the writer fills
(`route_unicast`, `route_vpn`, `route_evpn`, `ls_events`, `ls_nodes`,
`ls_links`, `ls_prefixes`, `peer_events`, `eor_events` and `stats_events`);
eight `*_current` tables holding live state (`route_unicast_current`,
`route_vpn_current`, `route_evpn_current`, `ls_nodes_current`,
`ls_links_current`, `ls_prefixes_current`, `peer_current` and `eor_current`
— `ls_events` and `stats_events` feed no current table of their own, and
`eor_current` takes dump state from both `eor_events` and `ls_events`); and
`cleanup_lease`, which the writer's superseded-session cleanup uses as a
lock; and `collector_beats`, one row per collector heartbeat, which decides
when a collector's peers read `stale` (it has no TTL; `vantage purge`
removes a retired collector's rows). The lease assumes a single ClickHouse
server: against replicated or cloud ClickHouse two writers may both run a
cleanup cycle, which only repeats idempotent deletes. Nine materialized views feed the eight current tables from the
history tables (`eor_current` has two, one from `eor_events` and one from
`ls_events`). It creates no dictionaries; the two optional AS-holder-name
dictionaries are separate files (see
[AS holder names](#as-holder-names-optional) below).

The dev stack applies it automatically: `deploy/clickhouse` is mounted as
ClickHouse's `/docker-entrypoint-initdb.d`, so it runs on the first start
of an empty volume. Against any other ClickHouse, pipe it through
`clickhouse-client` with a user that can create databases and tables:

```
clickhouse-client --host <host> --port 9000 --user <user> --password <password> \
  --multiquery < deploy/clickhouse/schema.sql
```

The tables are always created in the database named `vantage`; the DDL
names it explicitly rather than taking it from the connection.

Beyond creating the schema, `vantage-writer`'s ClickHouse user needs, at
runtime, lightweight `DELETE` (the `ALTER DELETE` privilege) on the
`*_current` tables, `KILL MUTATION`, and `INSERT` and `SELECT` on
`cleanup_lease`: the writer prunes superseded sessions from the current
tables itself.

**It creates; it does not migrate.** Every statement is `IF NOT EXISTS`, and
ClickHouse will not rewrite an existing table's `ORDER BY`, so running it
against a database built by an older version changes nothing about that
database's tables. The version row is inserted only when there isn't one
already, precisely so that such a database keeps its own version and
`vantage-writer` and `vantage-api` refuse to start against it rather than
appending rows whose shape they have guessed wrong. Both check
`max(version)` in `vantage.schema_version` against the version they were
built for (`sink.ExpectedSchemaVersion`, currently **2**) and refuse any
other value, older or newer. Recreating the database is therefore the
default way to move to a new schema version, which for the dev stack means
`docker compose -f docker-compose.dev.yml down -v` — the init script only
runs on an empty volume, so a plain `down` will not re-apply an edited
`schema.sql`.

`deploy/clickhouse/migrations/` holds the exceptions: changes an existing
archive can be moved forward by, for when recreating the database would
throw away data that cannot be regenerated. A database the current
`schema.sql` created already has every migration's effect.

Apply the migrations your database needs, in order, after
stopping `vantage-writer` and `vantage-api` (both refuse to start against a
version they were not built for, and mid-migration the database is
momentarily neither version). Against any ClickHouse:

```
clickhouse-client --host <host> --port 9000 --user <user> --password <password> \
  --multiquery < deploy/clickhouse/migrations/NNN-description.sql
```

or, against the dev stack's own container:

```
docker compose -f docker-compose.dev.yml exec -T clickhouse \
  clickhouse-client -u vantage --password vantage \
  --multiquery < deploy/clickhouse/migrations/NNN-description.sql
```

Each file is idempotent, and its version `INSERT` is conditional on the
database not already being at that version, so running one twice, or
against a database the current `schema.sql` created (which already has
every migration's effect), changes nothing. To see where a database is:

```
clickhouse-client ... -q "SELECT max(version) FROM vantage.schema_version"
```

## Retention

History is kept for 90 days by default: every route, link-state, peer,
End-of-RIB and stats event, in the ten history tables. Each of those tables
has a TTL on `ts_collector`, and ClickHouse deletes rows past it. Current
state (each peer's session, each route, each link-state object as it
stands now) lives in the `*_current` tables, which have no TTL and never
expire: a peer that has been up, or a route that has not changed, for longer
than the retention is still listed.

Under Helm, set `retention.days` (an integer, at least 1). The schema Job
applies it to the ten history tables with `ALTER TABLE ... MODIFY TTL`,
skips any table already at that value, and never touches the current-state
tables. Its ClickHouse user needs the `ALTER TTL` privilege (alias
`ALTER MODIFY TTL`) on `vantage.*`, beside the rights it already needs to
create the schema.
With `schemaJob.apply: false` the value does nothing; apply the TTL
yourself as below.

Without Helm, run the same statements, with your number of days in place
of 30:

```
clickhouse-client --host <host> --port 9000 --user <user> --password <password> --multiquery <<'SQL'
ALTER TABLE vantage.route_unicast MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.route_vpn     MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.route_evpn    MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.ls_events     MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.ls_nodes      MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.ls_links      MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.ls_prefixes   MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.peer_events   MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.eor_events    MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
ALTER TABLE vantage.stats_events  MODIFY TTL toDateTime(ts_collector) + INTERVAL 30 DAY SETTINGS materialize_ttl_after_modify = 0;
SQL
```

Set all ten to the same value. `vantage-api` reports the retention as
`meta.retention_days` on `GET /v1/collectors`, read from `route_unicast`'s
TTL, so that is the table it describes. To check all ten:

```
clickhouse-client ... -q "SELECT name, engine_full FROM system.tables
  WHERE database = 'vantage' AND engine_full LIKE '%TTL toDateTime(ts_collector)%'"
```

`system.tables` spells the TTL `toIntervalDay(30)`, not `INTERVAL 30 DAY`.

**A change applies as parts merge, not at once.**
`materialize_ttl_after_modify = 0` records the new TTL without rewriting
the data already on disk. ClickHouse applies it to each part as it next
merges that part, so rows past a shorter retention are deleted over the
merges that follow (a TTL merge runs on a table at most every four hours by
default, ClickHouse's `merge_with_ttl_timeout`), and rows a longer retention
would have kept are already gone. To apply it now instead, per table:

```
clickhouse-client ... -q "ALTER TABLE vantage.route_unicast MATERIALIZE TTL"
```

**`MATERIALIZE TTL` rewrites every part of the table.** It is a mutation
that reads and writes the whole table, which on a large archive costs as
much disk I/O and temporary disk space as the table itself, and it runs in
the background after the statement returns. Run it one table at a time,
watch `system.mutations`, and prefer waiting for merges unless the disk is
full.

## AS holder names (optional)

`GET /v1/asnames` labels an AS number with its RIPE-registered holder name
("Level 3 Parent, LLC" beside AS3356) and the date RIPE published that data,
wherever the UI shows an ASN. This is entirely optional and off by default:
**no vantage daemon ever fetches anything itself.** Fetching is a person,
once, running:

```
make fetch-asnames
```

That downloads `https://ftp.ripe.net/ripe/asnames/asn.txt` and writes four
files to the gitignored `deploy/dev/asnames/`: the raw file, the parsed TSV
the names dictionary reads, a one-row TSV with the same publication date for
the companion dictionary below, and a human-readable sidecar recording both
that date and when the fetch ran. Nothing else in this repository writes to
that directory, and a fresh clone has nothing there until this target runs.

Two dictionaries carry that data into ClickHouse: `deploy/clickhouse/asnames.sql`
(the names) and `deploy/clickhouse/asnames_meta.sql` (the publication date,
as its own companion object — see that file's header for why). Neither
joins the `schema_version` sequence — an optional lookup must
never gate `vantage-writer`/`vantage-api` startup — so the dev stack applies
both automatically (same `/docker-entrypoint-initdb.d` mount as the schema),
but an **existing** stack needs them applied by hand, same convention as a
migration. Against the dev stack:

```
docker compose -f docker-compose.dev.yml exec -T clickhouse \
  clickhouse-client -u vantage --password vantage --multiquery < deploy/clickhouse/asnames.sql
docker compose -f docker-compose.dev.yml exec -T clickhouse \
  clickhouse-client -u vantage --password vantage --multiquery < deploy/clickhouse/asnames_meta.sql
```

Against any other ClickHouse, the same two files through
`clickhouse-client --host ... --multiquery`, as for the schema above. Both
read their data from `/var/lib/clickhouse/user_files/asnames/` on the
ClickHouse server, so the fetched files have to be placed there too.

**Apply both, always, in the same sitting.** They are two separate files for
a reason (see `asnames_meta.sql`'s header), but they are one operation: the
names dictionary works fine on its own, which is exactly the trap — apply
only `asnames.sql` and the UI shows real holder names beside a permanently
null publication date, the one signal that tells a viewer the data might be
stale. Applying just `asnames.sql`, or applying it and forgetting
`asnames_meta.sql` later, produces that half-working state silently; nothing
refuses to start over it.

Without either file applied, or with no data ever fetched, every ASN in the
UI renders as a bare number and the affected screens say so once — a
degraded feature, not a broken deploy. `GET /v1/asnames`'s own
`meta.asnames_loaded` tells a client which case it is in; see `api/openapi.yaml`.

**A ClickHouse restart needs no manual step.** ClickHouse's
`dictionaries_lazy_load` (left at its image default, `true`) means a
dictionary's data unloads on every server restart and reloads only on first
*use*: an already-`LOADED` dictionary comes back `NOT_LOADED` after a plain
restart. `vantage-api` calls ClickHouse's dictionary lookup directly and
classifies the two ClickHouse error codes that call raises when there is
genuinely nothing behind it ("no dataset" either way, per
`query/asnames.go`), so the first real `GET /v1/asnames` request after a
restart is itself that first use: if the data is on disk, that request
loads it, serves the real answer, and leaves both dictionaries `LOADED`.
Confirmed against a live restart of the dev stack's ClickHouse:
`system.dictionaries.status` read `NOT_LOADED` immediately afterwards, and
the next `GET /v1/asnames` came back with the real holder name and flipped
both dictionaries to `LOADED`, with no `SYSTEM RELOAD DICTIONARY` and no
`vantage-api` restart. (`dictionaries_lazy_load=false`, loading eagerly at
startup, was tried and rejected: with no data file present — this feature's
default state — `CREATE DICTIONARY` itself becomes an eager load that
raises instead of landing `NOT_LOADED`, and since the DDL is applied before
the data is known to exist, the ClickHouse container failed to start at
all.)

**A re-fetch does need a manual reload.** Re-running `make fetch-asnames`
overwrites `asn.tsv` and `asnames_meta.tsv` with newer data, but
`LIFETIME(0)` means an *already-`LOADED`* dictionary never notices a changed
file on its own, on purpose (see `asnames.sql`'s comment on why a
timer-based reload was rejected). Without an explicit reload after a
re-fetch, the UI keeps serving the old names with no error and no signal
that a newer file is sitting on disk:

```
docker compose -f docker-compose.dev.yml exec -T clickhouse \
  clickhouse-client -u vantage --password vantage --query "SYSTEM RELOAD DICTIONARY vantage.asnames"
docker compose -f docker-compose.dev.yml exec -T clickhouse \
  clickhouse-client -u vantage --password vantage --query "SYSTEM RELOAD DICTIONARY vantage.asnames_meta"
```

Run this after every re-fetch against a stack that already has the
dictionaries loaded (against any other ClickHouse, the same two statements
through your own `clickhouse-client`). It is **not** needed after a plain
ClickHouse restart — only after the underlying file changes while
ClickHouse keeps running.

**On Kubernetes** the fetched data does not fit where the schema does
(`asn.txt` alone gzips past the ConfigMap ceiling `schema.sql` comfortably
clears), and the chart has no ClickHouse pod to mount it into, so the files
go on your own ClickHouse host. [`deploy/helm/README.md`](../deploy/helm/README.md)
covers that path, including the same restart and re-fetch behavior.

## API authentication

`vantage-api` authenticates every `/v1` request except `/v1/auth/config` and
`/v1/openapi.yaml` with a bearer token. Tokens are listed in its config file,
each with a name that appears in logs in place of the secret:

```yaml
auth:
  mode: token        # the default
tokens:
  - name: grafana
    token: "at-least-sixteen-characters"
```

The daemon refuses to start with no tokens, with a token shorter than 16
bytes, or with two tokens sharing a name. The Helm chart generates tokens for
you (see "API tokens under GitOps" below).

`auth.mode: none` turns authentication off, for a deployment that already
authenticates every request in front of the API (an authenticating reverse
proxy). It is not safe behind a VPN alone: a web page an operator visits can
use DNS rebinding to make its own requests to the API look same-origin. In
this mode the API therefore answers only requests whose `Host` is a loopback
address or is listed in `allowed_hosts`, and refuses the rest with 421:

```yaml
auth:
  mode: none
allowed_hosts: [vantage.internal.example]   # names only, no port or scheme
```

## Logs, versions and health checks

All three daemons take the same two logging keys, and refuse to start on a
value outside these lists:

```yaml
log_level: info      # debug, info, warn or error
log_format: text     # text or json (one object per line, for a log shipper)
```

`vantage-writer` and `vantage-api` also take `clickhouse_password_file`, a
path whose contents (one trailing newline trimmed) become the password in
`clickhouse_dsn`. It is how a Kubernetes Secret reaches ClickHouse without
being rendered into a ConfigMap. Leave the password out of the DSN when you
use it; a password in both places is a startup error.

```yaml
clickhouse_dsn: "clickhouse://vantage@clickhouse.example:9000/vantage"
clickhouse_password_file: /etc/vantage/clickhouse/password
```

Every binary accepts `-version` and prints `<binary> <version> (<revision>,
<go version>)`; each daemon logs the same at startup and exports it as
`vantage_build_info{component,version,revision,goversion} 1` on its
`/metrics`. `make build` and `make images` stamp the version from `git
describe`. An image built with plain `docker build` reports `dev` unless you
pass `--build-arg VERSION=... --build-arg REVISION=...`, because the build
context carries no `.git` to read it from.

Each daemon's metrics listener (collector `:9469`, writer `:9472`, API
`:9474`) also serves two probes:

| Path | 200 when | Otherwise |
|---|---|---|
| `/healthz` | the process is serving HTTP | (never consults a dependency) |
| `/readyz` (collector) | its NATS connection is CONNECTED | 503, one-line reason |
| `/readyz` (writer) | NATS is CONNECTED and a ClickHouse ping succeeds | 503, one-line reason |
| `/readyz` (API) | a ClickHouse ping succeeds | 503, one-line reason |

The ClickHouse ping is cached for five seconds, so frequent probes do not
turn into a query each. The collector's and writer's `/readyz` are meant for
a readiness probe. The API's is dependency health for monitoring; the Helm
chart deliberately does not use it for readiness, because taking the API out
of its Service while ClickHouse is down would replace the API's own error,
which says what is wrong, with a connection failure in the UI, which says
nothing.

The API also exports `vantage_api_http_requests_total{route,code}` and
`vantage_api_http_request_duration_seconds{route}`. `route` is the matched
pattern (`GET /v1/routes`), `ui` for the web app and its assets, or
`unmatched` for any other `/v1` path, so a scanner cannot grow the series
count.

## Kubernetes (Helm)

`deploy/helm/vantage` deploys the collector, writer and read API to
Kubernetes. [`deploy/helm/README.md`](../deploy/helm/README.md) is the full
reference for the chart — values, BMP ingress, NATS TLS, AS holder names,
uninstalling — and this section only summarizes the points that most often
decide whether an install works.

Fetch the chart's dependency first (`helm dependency build
deploy/helm/vantage`, or `make chart-deps` from the repository root): the
NATS subchart is not vendored.

### Images

The chart's defaults are `ghcr.io/jp2195/vantage-collector`,
`ghcr.io/jp2195/vantage-writer` and `ghcr.io/jp2195/vantage-api`, with an
empty `tag` that resolves to the chart's `appVersion`. Release images are
published when a `vX.Y.Z` tag is pushed and are tagged `X.Y.Z` (and with
the commit's short SHA), so a chart at `appVersion` X.Y.Z pulls matching
images with no overrides.

Releases are attested once the repository is public: each release image,
the chart and each CLI archive carries a signed build provenance
attestation. Verify one before you deploy it:

```
gh attestation verify oci://ghcr.io/jp2195/vantage-api:X.Y.Z --owner jp2195
```

Until a release exists for the version you are installing, build and push
your own:

```
make push-images REGISTRY=<your-registry>
```

That builds all three images and tags them with the current git short SHA
(override with `TAG=...`). Then set `images.collector.repository`,
`images.writer.repository` and `images.api.repository` to
`<your-registry>/vantage-collector` and so on, and each `images.*.tag` to
that tag. A private registry also needs `imagePullSecrets`.

### This chart does not deploy ClickHouse

`clickhouse.externalHost` is required, and the render fails with a message
naming it if left empty — there is no bundled ClickHouse to fall back to.
Point it at a database you already run: set `clickhouse.existingSecret` (a
Secret carrying `username` and `password` keys), or `clickhouse.auth.password`
directly. Standing up a datastore is not this chart's job — an operator
already running ClickHouse has opinions about replication, backup and
retention that a chart-owned database would silently compete with.
`deploy/clickhouse/k8s/` covers running ClickHouse under the official
ClickHouse-org operator, including a finding worth reading before that
install: the operator's default `default` user ships with no password and
no network restriction, which resolves to unauthenticated superuser SQL over
the route archive.

### Exposing the BMP port

BMP carries no authentication, so whatever reaches the collector's port can
feed it routes. Set `collector.allowedSources` to your routers' CIDRs: the
collector closes any other connection before parsing a byte, and the chart
also renders the list as the BMP Service's `loadBalancerSourceRanges`. Left
empty, every source is accepted and the collector and the release notes both
warn. The BMP Service is a `LoadBalancer`, which on EKS, GKE and AKS is
internet-facing unless annotated; `collector.service.annotations` takes each
provider's internal-load-balancer annotation, listed under "Exposing the BMP
port" in [`deploy/helm/README.md`](../deploy/helm/README.md). Behind a PROXY
protocol front-end, also set `collector.trustedProxies`.

### NATS runs as three servers

The bundled NATS is a three-server JetStream cluster by default, one server
per node, and the collector's streams keep three copies. Any one server can
restart, for a NATS upgrade or a `kubectl drain`, while the other two keep
taking publishes to ROUTES, LS, PEER and STATS, so routes, peer events,
stats and link state reach the archive with no gap. RAW keeps one copy, so
sessions that publish to RAW (an unparseable message, a Termination, an
armed mirror) while the server holding it restarts are closed; see
"Publish failures" in `docs/operating.md`. A single server cannot do that: its restart takes the streams
offline. The collector retries each publish for up to 60 seconds from when
it was first sent, and closes its sessions if the server is not back by
then, which a JetStream restart with real data on disk often is not.

The server count must be odd and at least 3 (the render refuses others; to
run five, raise the spread rule's `minDomains` with it). The default
therefore needs **at least three schedulable nodes** (a hard
spread rule places no two servers on one node, and leaves the third
`Pending` rather than stack it) and **three JetStream volumes** of
`nats.config.jetstream.fileStore.pvc.size` (20Gi), one per server, each
holding a full copy of the three-copy streams.

For a single-node cluster or a homelab, one value goes back to one server
with single-copy streams:

```yaml
nats:
  config:
    cluster:
      enabled: false
```

With external NATS (`nats.enabled: false`), set `collector.streams.replicas`
to match your servers yourself: the chart cannot see them and leaves the
collector's default of 1. An install on the single-server opt-out loses the
contents of its streams when it moves to three, once, and its routers are
disconnected while it moves (the collector is stopped before
NATS restarts); see "Moving an existing install to three servers" in
[`deploy/helm/README.md`](../deploy/helm/README.md#nats-clustering) for the
order of steps.

### NATS mutual TLS

`nats.tls.enabled` defaults to `true`: the collector and the writer talk to
NATS over mutual TLS, and the bundled server's side is configured by the
chart's defaults (with nats-box off, since it has no client certificate).
The chart mounts certificate Secrets; it never generates a private key, so a
default install fails the render with a message saying how to supply them.
Two ways:

- **Bring your own**, via `deploy/nats-tls/gen-certs.sh --release <release>
  --namespace <ns>` followed by `kubectl apply -n <ns> -f nats-tls/secrets.yaml`,
  then set `nats.tls.collectorSecret` / `nats.tls.writerSecret` to the
  script's output. For a release not named `vantage`, also set the
  `nats.tlsCA.secretName` and `nats.config.nats.tls.secretName` it prints;
  pasting its whole printed block is always correct.
- **cert-manager**, by setting `nats.tls.issuerRef` (`{name, kind}`) instead
  — the chart then issues all three certificates itself. Set either the two
  client Secret names or `issuerRef`, never both: they are two different
  issuance paths and the render refuses the combination rather than
  silently preferring one.

`nats.config.nats.tls.merge.verify: true` is what turns server-side TLS into
mutual TLS. The routes between the three NATS servers use mutual TLS too, on
the same server certificate, which both issuance paths give the route names
and the `clientAuth` key usage a route needs. Plaintext is an explicit
opt-out, and with the bundled NATS it takes four values, because the server
half is subchart configuration this chart cannot template:
`nats.tls.enabled`, `nats.tlsCA.enabled`, `nats.config.nats.tls.enabled` and
`nats.config.cluster.tls.enabled`, all `false` (the last only while NATS is
clustered). Setting only the first fails the render, naming the others.

### NetworkPolicy

`networkPolicy.enabled` (default `true`) admits NATS client connections from
the collector and writer pods only, and the cluster-route port from the NATS
pods to each other, and limits the API pod to its HTTP and
metrics ports (from anywhere). It is ingress-only, and it does nothing unless
the cluster's CNI enforces NetworkPolicy.

### API tokens under GitOps

An empty `api.tokens[].token` is generated at install and kept across
upgrades through `lookup`, which returns nothing under `helm template`. Argo
CD and other tools that render with `helm template` therefore mint a new token
on every sync. Use `api.existingSecret` with a Secret you manage instead.

### The schema Job is optional

`schemaJob.apply` (default `true`) controls whether the chart applies
`deploy/clickhouse/schema.sql`, its migrations, and the AS-holder-name
dictionary DDL to your database, via a Job that re-runs on every `helm
upgrade`. The chart carries its own copies of those files (a chart cannot
read outside its directory) and applies them in order: the schema, then
each migration, then `retention.days` as the history tables' TTL (see
[Retention](#retention)), then the two dictionaries. Set it to `false` if your
database is managed by a team that will not grant a workload chart DDL
rights, and apply the same files yourself, in that order, before upgrading
(see [Applying the schema](#applying-the-schema)) — the daemons refuse to
start against a schema version other than the one they expect, which is the
intended loud failure rather than a silent one. Always pass
`--wait-for-jobs` alongside `--wait` on install or upgrade: `--wait` does not
cover Jobs, so a failing schema Job can otherwise leave `helm upgrade`
reporting success against a database that never received the schema.
