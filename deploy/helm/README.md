# vantage Helm chart

`deploy/helm/vantage` deploys vantage's BMP collection pipeline to
Kubernetes: the collector (a StatefulSet behind a LoadBalancer Service), NATS
JetStream (the official NATS chart, as a subchart), the writer, the read API,
and a Job that applies vantage's schema. It does **not** deploy ClickHouse;
you point it at one you run.

The examples below install a release named `vantage` into a namespace named
`vantage`, and keep your settings in a values file called `my-values.yaml`.
Substitute your own names; where a resource name depends on the release name,
the text says so.

## Quick start

    helm repo add nats https://nats-io.github.io/k8s/helm/charts/
    helm dependency build deploy/helm/vantage

    kubectl create namespace vantage
    kubectl -n vantage create secret generic vantage-clickhouse-credentials \
      --from-literal=username=vantage --from-literal=password='<password>'

    # NATS mutual TLS is on by default and the chart generates no keys.
    deploy/nats-tls/gen-certs.sh --release vantage --namespace vantage
    kubectl apply -n vantage -f nats-tls/secrets.yaml

    cat > my-values.yaml <<'EOF'
    clickhouse:
      externalHost: clickhouse.example.internal   # your ClickHouse host or Service
      existingSecret: vantage-clickhouse-credentials  # keys: username, password
    collector:
      allowedSources: ["10.0.0.0/8"]   # your routers' addresses
    nats:
      tls:
        collectorSecret: vantage-vantage-nats-collector-tls   # from gen-certs.sh
        writerSecret: vantage-vantage-nats-writer-tls
    EOF

    helm upgrade --install vantage deploy/helm/vantage \
      --namespace vantage \
      -f my-values.yaml \
      --wait --wait-for-jobs --timeout 10m

This assumes a ClickHouse server you already run, reachable from the cluster,
with a `vantage` user allowed to create the `vantage` database and its tables
(see "ClickHouse" below, including the writer's runtime privileges); at least
three schedulable nodes, because NATS runs as three servers on three
different nodes; and a default StorageClass for NATS JetStream's three 20Gi
volumes, one per server (or set
`nats.config.jetstream.fileStore.pvc.storageClassName`). On a single-node
cluster, add `nats: {config: {cluster: {enabled: false}}}` to
`my-values.yaml`; see "NATS clustering" below.
The release notes printed at the end show how to
find the BMP address to point routers at and how to read the generated API
token. The sections below explain each piece.

On a first install the collector and the writer usually restart a few times
before settling, each exit logging `connect nats ...: nats: no servers
available for connection`. Both need NATS at startup and exit when it is not
yet accepting connections; the kubelet restarts them, and they stay up once
the three NATS servers are ready, typically within a minute. A restart count
that keeps climbing after that is a real fault; read the newest exit with
`kubectl -n vantage logs <pod> --previous`.

Two steps above are security defaults, not ceremony:

- **The certificates.** Without them the render fails with a message listing
  the three ways forward: the script above, cert-manager
  (`nats.tls.issuerRef`), or an explicit plaintext opt-out. The two Secret
  names are for a release called `vantage`; `gen-certs.sh` prints the exact
  values block for any other release name, which then also sets the server
  Secret name. See "NATS TLS" below. Keep `nats-tls/ca.key` somewhere safe
  and out of git.
- **`collector.allowedSources`.** The BMP Service is a `LoadBalancer`, and on
  a cloud provider that means an internet-facing address unless annotated
  otherwise. See "Exposing the BMP port" below before installing anywhere
  but a lab.

Keep every setting in `my-values.yaml` and pass it on every `helm upgrade`.
`helm upgrade` with a `--set` and no `-f` starts from the chart's defaults,
not from what is installed, so a one-off `--set` would drop the required
ClickHouse values and the render would fail.

### GitOps (Argo CD, Flux)

Tools that deploy through `helm template` rather than `helm install` get a new
API token on every sync. The chart generates an empty `api.tokens[].token`
with `randAlphaNum` and keeps it stable across upgrades with `lookup`, which
returns nothing under `helm template`, so every render mints a fresh value
and every client's token stops working. Create the token Secret yourself and
set `api.existingSecret` (one key per token name, e.g. `lab`), or pin
`api.tokens[].token`, which puts the token in your values.

## Fetch the chart dependencies first

The NATS subchart is a dependency, not a vendored copy: `Chart.lock` pins its
version and `.gitignore` excludes the fetched `.tgz` from `charts/`, so a
fresh clone has `Chart.yaml` and `Chart.lock` but no `charts/nats-*.tgz`.
Every `helm install`/`upgrade`/`template`/`lint` command fails until you run:

    helm repo add nats https://nats-io.github.io/k8s/helm/charts/
    helm dependency build deploy/helm/vantage

or, from the repository root, `make chart-deps`, which runs the same commands
and skips them when the fetched chart is already current. The `helm repo add`
is required: `helm dependency build` finds `Chart.lock`'s repository only
among repositories already added to your helm client.

Run `build`, not `update`. `build` fetches exactly what `Chart.lock` pins;
`update` re-resolves against the NATS repository's current index, can pick up
a newer version than the one this chart was tested against, and rewrites the
lock file when it does.

## Images

### Official images

Release images are published to GitHub Container Registry for every tagged
release `vX.Y.Z`:

- `ghcr.io/jp2195/vantage-collector`
- `ghcr.io/jp2195/vantage-writer`
- `ghcr.io/jp2195/vantage-api`

Each is tagged `X.Y.Z` and with the release commit's short SHA. These are the
chart's default `images.*.repository` values, and `images.*.tag` defaults to
empty, which means the chart's `appVersion`. A chart at `appVersion: X.Y.Z`
therefore pulls images `X.Y.Z` with no overrides. Set `images.<name>.tag` only
to run something other than the version the chart was released with.

Nothing here uses `latest`: it cannot be rolled back, and it makes "which
code is running" unanswerable.

### Building your own

To run unreleased code, or a version with no published images, build and push
the three images from the Dockerfiles at the repository root:

    make push-images REGISTRY=registry.example.com/yourorg

`push-images` builds first (`make images` builds without pushing) and tags
each image with the git short SHA of your checkout; pass `TAG=...` to choose
another tag. Then point the chart at them in `my-values.yaml`:

    images:
      collector:
        repository: registry.example.com/yourorg/vantage-collector
        tag: "<tag you pushed>"
      writer:
        repository: registry.example.com/yourorg/vantage-writer
        tag: "<tag you pushed>"
      api:
        repository: registry.example.com/yourorg/vantage-api
        tag: "<tag you pushed>"

`repository` is the full path including the registry host. A bare
`name/image` resolves to Docker Hub.

### Private registries

A private registry needs a pull secret in the release namespace, named in
`imagePullSecrets`:

    kubectl -n vantage create secret docker-registry regcred \
      --docker-server=registry.example.com \
      --docker-username=<user> \
      --docker-password=<password or token>

```yaml
imagePullSecrets:
  - name: regcred
```

`imagePullSecrets` defaults to empty, so without it every pull is anonymous.
Against a private repository that fails as `ImagePullBackOff`, and the event
does not read like a missing credential. Docker Hub, for example, reports:

    pull access denied, repository does not exist or may require
    authorization: server message: insufficient_scope: authorization failed

`imagePullSecrets` applies to the collector, writer, API and schema Job pods.
Two other images are pulled as well, and mirroring into a private registry
means covering both:

- `schemaJob.image` (default `clickhouse/clickhouse-server:24.8-alpine`) runs
  the schema Job and also the config-rendering init containers of the
  collector, writer and API, since the daemons' distroless images have no
  shell.
- The NATS subchart pulls its own images and does not read this chart's
  `imagePullSecrets`. It has its own settings: `nats.global.image.registry`
  and `nats.global.image.pullSecretNames`.

## ClickHouse

The chart never deploys ClickHouse. Standing up a datastore is not vantage's
job: whoever runs ClickHouse already has opinions about replication, backup,
retention and who holds the credentials, and a chart-owned database would
compete with all of them.

`clickhouse.externalHost`, `clickhouse.externalPort` (default `9000`, the
native protocol) and the credentials point the chart at your database. The
database is always `vantage`: the schema and every query the daemons run name
it explicitly, so there is no setting for it, and a values file that still
sets `clickhouse.database` to anything else fails the render. Two things are
required, and the render fails with a sentence naming what to set if either is
missing:

- `clickhouse.externalHost`. There is no default, because dialing a host
  nobody configured is worse than refusing to render.
- `clickhouse.existingSecret` (a Secret with keys `username` and
  `password`), **or** `clickhouse.auth.password` (with
  `clickhouse.auth.username`, default `vantage`). The chart does not generate
  a password: a random credential for a database it does not own is a
  guaranteed authentication failure, which would surface as a crash-looping
  writer instead of a render error.

The password reaches the writer and the API as a file: the chart mounts the
Secret's `password` key at `/etc/clickhouse-auth/password` and points each
daemon's `clickhouse_password_file` at it, so the password is in no rendered
config and no environment variable. The daemons read it at startup; after
rotating it, restart the writer and API pods. The username still comes from
the Secret's `username` key, which each pod's `render-config` init container
percent-encodes into `clickhouse_dsn`.

Pass `--wait-for-jobs` alongside `--wait` on every install and upgrade. The
schema is applied by a Job, and `--wait` covers Pods, PVCs, Services and
workload controllers but not Jobs. Measured with the same chart and the same
failing schema Job, one flag apart: `--wait` alone exited 0 and reported the
upgrade successful while the Job was still on its first attempt;
`--wait --wait-for-jobs` exited 1 with `UPGRADE FAILED`. Without the flag, a
schema failure leaves a release Helm calls successful and a database that is
not on the version the chart ships.

To evaluate vantage without a Kubernetes cluster or a ClickHouse server of
your own, use `docker-compose.dev.yml` at the repository root.

### Running ClickHouse under the operator

One way to provide the database is the official ClickHouse-org Kubernetes
operator, installed separately from this chart. `deploy/clickhouse/k8s/`
holds operator values and a walkthrough: the install, verification, and what
was found running it. Read `deploy/clickhouse/k8s/README.md` before you
install the operator. Among other things, it covers a hardening step every
install needs: the operator's built-in `default` user ships with no password
and no network restriction, and that directory's `clickhouse-values.yaml`
closes it. Then set `clickhouse.externalHost` and the credentials above to
point this chart at the result.

### The schema Job

`schemaJob.apply` (default `true`) controls whether the chart applies
vantage's schema and dictionary DDL to your database. It is on by default
because the schema belongs to vantage: the Job carries the migrations and
re-runs on every `helm upgrade`, as a no-op against an unchanged database.

The Job waits up to five minutes for ClickHouse to answer, creates the
database if needed, applies `files/schema/*.sql` in lexical order, then
checks that the database reports the schema version this chart's DDL
establishes, and fails if it does not. It then sets `retention.days` as the
history tables' TTL, and then applies the AS holder name dictionaries
(`files/dictionaries/*.sql`, below).

`retention.days` (default 90, an integer of at least 1) is how many days of
history the archive keeps in the ten history tables. The Job applies it with
`ALTER TABLE ... MODIFY TTL`, skips tables already at that value, and never
touches the current-state tables, which have no TTL. Its ClickHouse user
needs the `ALTER TTL` privilege on `vantage.*`. A change takes effect as
ClickHouse merges parts, not at once; see "Retention" in
[`docs/deploying.md`](../../docs/deploying.md#retention).

Set `schemaJob.apply=false` if your database is managed by a team that will
not grant a workload chart DDL rights. Then apply the same files yourself, in
the Job's order, before upgrading vantage: `deploy/clickhouse/schema.sql`,
each `deploy/clickhouse/migrations/*.sql` in order, the retention TTL (see
"Retention" in `docs/deploying.md`; `retention.days` does nothing with the
Job off), then `deploy/clickhouse/asnames.sql` and `asnames_meta.sql`. The daemons never
create tables; they refuse to start against a database whose schema is behind
them, which is the intended loud failure.

For a locked-down user: at runtime `vantage-writer`'s ClickHouse user needs,
beyond `SELECT` and `INSERT`, lightweight `DELETE` (the `ALTER DELETE`
privilege) on the `*_current` tables, `KILL MUTATION`, and `INSERT` and `SELECT` on
`cleanup_lease`, because the writer prunes superseded sessions from the
current tables itself. That lease assumes a single ClickHouse server: with
`writer.replicas` above 1 against replicated or cloud ClickHouse, two writers
may both run a cleanup cycle, which only repeats idempotent deletes.

For contributors: everything under `files/` is a copy of
`deploy/clickhouse/`, because `.Files.Glob` cannot read outside the chart
directory. After adding a migration, run `make sync-helm-schema` from the
repository root; `sink`'s `TestHelmChartSchemaFilesMirrorDeployClickhouse`
fails if you forget. Forgetting would break only clusters that already hold
data. A fresh install is correct either way, because `000-schema.sql` creates
every table at the current version, but an existing database would stay on
its old version, since `schema.sql` is all `CREATE TABLE IF NOT EXISTS`. The
version check fails the Job in that case rather than letting it report
success.

## AS holder names (optional)

`GET /v1/asnames` labels each AS number with its RIPE-registered holder name
("Level 3 Parent, LLC" beside AS3356) and the date RIPE published that data.
This is optional. No vantage daemon and nothing in this chart fetches it;
without it the UI shows bare AS numbers and says so on screen, and nothing
fails to start.

**The chart ships the dictionary definitions, never the data.** `make
fetch-asnames` writes about 12 MiB to `deploy/dev/asnames/`, and its largest
file, `asn.txt`, is 2.18 MiB even gzipped. Both are well over the ~1 MiB
ConfigMap ceiling the 37 KB `schema.sql` fits through. The DDL,
`deploy/clickhouse/asnames.sql` and `asnames_meta.sql` (about 10 KB
combined), is small enough to ship, and the schema Job applies both on every
install, whether or not any data backs them. That is safe with no data
present: ClickHouse's `dictionaries_lazy_load` means `CREATE DICTIONARY` never
reads the source path at apply time. Confirmed against the chart's
ClickHouse image with the source directory entirely absent: both
dictionaries land at `system.dictionaries` status `NOT_LOADED`, with no error
and nothing that retries.

**Getting the data onto your ClickHouse host.** Run `make fetch-asnames` from
a checkout of this repository, on a machine that can reach `ftp.ripe.net`;
the cluster never needs to. That produces `deploy/dev/asnames/` with four
files: `asn.txt`, the `asn.tsv` the names dictionary reads, `asn.meta`, and
the `asnames_meta.tsv` the date dictionary reads.

Place that directory at `/var/lib/clickhouse/user_files/asnames` on the
ClickHouse server, the path both DDL files read from and the one
`docker-compose.dev.yml` binds for the dev stack. How it gets there is up to
you: a volume on the ClickHouse pod, an init container, or `kubectl cp` for a
one-off.

**Apply both DDL files together.** `asnames.sql` names the ASNs;
`asnames_meta.sql` carries RIPE's publication date, the one fact that tells a
caller whether those names are stale. The schema Job always applies both. If
you apply them by hand (`clickhouse-client < deploy/clickhouse/asnames.sql`
while debugging, say), apply both, or you get names with a permanently null
publication date.

**Restarts need no action.** A dictionary's data unloads on every ClickHouse
restart and reloads on first use. vantage-api's lookup is that first use: the
first `GET /v1/asnames` after a restart loads the dictionary and answers with
the real name. This was measured on the dev stack (`docker restart` of the
ClickHouse container with the data mounted, then one ordinary API request,
with no manual reload), not by restarting a Kubernetes-managed ClickHouse pod.
It is the same vantage-api code and the same ClickHouse version, so no
different behavior is expected.

**A re-fetch does need a reload.** The dictionaries use `LIFETIME(0)`, so an
already-loaded dictionary never notices a changed file on its own (see
`asnames.sql` for why a timer-based reload was rejected). After running `make
fetch-asnames` again and replacing the files:

```bash
clickhouse-client --host <clickhouse host> --user vantage --password ... \
  --query "SYSTEM RELOAD DICTIONARY vantage.asnames; SYSTEM RELOAD DICTIONARY vantage.asnames_meta;"
```

**Leave `dictionaries_lazy_load` at its default (`true`).** With it set to
`false`, `CREATE DICTIONARY` becomes an eager, synchronous load and fails when
the data file is absent. Measured on the dev stack, where the DDL runs at
container init: ClickHouse failed to start (`Exited (107)`) every time. Under
this chart the same DDL runs in the schema Job under `set -e`, so the Job
would fail and leave the release with no schema at all. That trades a missing
optional feature for an install that does not complete.

## Exposing the BMP port

BMP is unauthenticated: whatever connects to the collector's port and speaks
BMP becomes a router in vantage, and its routes land in the database beside
real ones. The port must be reachable only by your routers. Three layers
enforce that, and a production install should use all three.

**`collector.allowedSources`** is the collector's own check, a list of CIDRs:

```yaml
collector:
  allowedSources: ["10.0.0.0/8", "2001:db8:100::/48"]
```

A connection from outside the list is closed before any BMP byte is parsed
and counted in `vantage_collector_connections_rejected_total{reason="source_not_allowed"}`.
The address checked is the router's: the TCP peer with
`collector.proxyProtocol: "off"`, the PROXY header's source with
`"required"`. Left empty, the collector accepts every source and logs one
warning at startup, and the release notes print a warning too. A malformed
CIDR stops the collector from starting.

With `proxyProtocol: "required"`, also set **`collector.trustedProxies`** to
the proxies' addresses. Otherwise anything that can reach the port can send a
PROXY header claiming to be any router. Connections from outside it are
counted with `reason="proxy_not_trusted"`. Setting it with
`proxyProtocol: "off"` fails the render.

**`collector.maxConnections`** caps concurrent BMP connections (default
1024, from the collector; `0` in values leaves that default). Excess
connections are closed immediately and counted with `reason="max_connections"`.

**`loadBalancerSourceRanges`** drops the same traffic at the load balancer,
before it reaches the cluster. The chart renders it on the BMP Service when
`collector.service.type` is `LoadBalancer`, defaulting to
`collector.allowedSources` (or `collector.trustedProxies` with
`proxyProtocol: "required"`, since the proxy is then what connects). Set
`collector.service.loadBalancerSourceRanges` to override it. Whether it is
enforced depends on the implementation: the cloud providers' load balancers,
kube-proxy and Cilium honor it; check yours.

**An internal load balancer.** On EKS, GKE and AKS a `LoadBalancer` Service
gets a public address unless annotated otherwise. Set your provider's
annotation through `collector.service.annotations`:

```yaml
collector:
  service:
    annotations:
      # EKS (AWS Load Balancer Controller)
      service.beta.kubernetes.io/aws-load-balancer-scheme: internal
      # GKE
      # networking.gke.io/load-balancer-type: "Internal"
      # AKS
      # service.beta.kubernetes.io/azure-load-balancer-internal: "true"
```

On bare metal (MetalLB, Cilium LB IPAM), the address comes from a pool you
defined, so whether it is reachable from outside is a property of that pool
and your network, not of this chart.

## BMP ingress and the announcing node

The BMP Service is a `LoadBalancer` with `externalTrafficPolicy: Local`, and
that setting is load-bearing. (The chart emits it for `LoadBalancer` and
`NodePort` only; the API server rejects it on a `ClusterIP` Service.) A router's source address is what identifies it
(`collector/server.go` derives `router_ip` from `conn.RemoteAddr()`), so
`Cluster` mode's SNAT would rewrite every router in a fleet to a node address
and collapse them onto one identity.

`collector.service.type`, `collector.service.annotations` and
`collector.service.loadBalancerIP` (optional; unset lets your LoadBalancer
implementation assign an address) configure the Service. The port is
`collector.bmpPort` (default `11019`).

The cost of `Local` is that the node answering ARP for the VIP must be the
node running the collector pod. Not every LoadBalancer implementation
guarantees that:

- **MetalLB (L2 mode)** takes the Service's external traffic policy and its
  ready endpoints into account when electing an announcer. Nothing extra is
  needed.
- **Cilium L2 announcements** does not. Its documentation states the feature
  "is incompatible with the `externalTrafficPolicy: Local` on services as it
  may cause service IPs to be announced on nodes without pods causing traffic
  drops", and its leader election is first-come-first-served with no regard
  for where the backend runs.

On Cilium L2 announcements, then, the VIP works only while the announcing
node and the collector pod happen to be the same node. That can hold for a
long time and end at the first reschedule, which looks like a total BMP
outage with a healthy collector: the pod is `Running`, its logs are clean,
and nothing answers on the VIP.

There are two ways to make that colocation a guarantee, and they trade off
differently.

**Pin the collector to the announcing node.** This is the smallest change and
touches nothing outside this release. Find the current announcer:

```sh
kubectl -n kube-system get lease | grep l2announce
```

and add to `my-values.yaml`:

```yaml
collector:
  nodeSelector:
    kubernetes.io/hostname: <announcing-node>
```

then `helm upgrade` as in the quick start.

**This trades one outage mode for a worse one.** Unpinned, a single-replica
StatefulSet reschedules onto a surviving node when its node dies, and BMP
ingestion comes back on its own. Pinned, the collector has nowhere else to
run, and it stays `Pending` until someone edits or clears the `nodeSelector`.
Node failure then takes BMP ingestion down until someone intervenes. Treat the
pin as a stopgap, not something to carry into a deployment where BMP uptime
matters.

**Or constrain the announcer instead of the pod.** Give the BMP Service its
own `CiliumL2AnnouncementPolicy` with a `serviceSelector` and a
`nodeSelector`, and give any cluster-wide policy a `serviceSelector` that
excludes the BMP Service, so the two do not both claim it. Do this deliberately: a cluster-wide
policy usually serves every LoadBalancer in the cluster, and narrowing it
without a `serviceSelector` would drag every other VIP onto the same nodes.

This scopes the constraint to one Service, so no other workload's VIP is
affected. It does **not** remove the colocation requirement:

- Point the policy's `nodeSelector` at a **single node** and the pod still
  has to run there. If Kubernetes reschedules it elsewhere, the announcer is
  left on a node with no local backend, `externalTrafficPolicy: Local` drops
  the traffic, and the VIP goes dark, again with a `Running` pod and clean
  logs.
- Point it at **several nodes** and leader election is first-come-first-served
  among them again, so colocation is back to coincidence.

What removes the colocation requirement is `externalTrafficPolicy: Cluster`
**plus a front-end that speaks PROXY protocol**, so the router's real address
arrives in a header rather than in the socket. Both halves are needed, and the
second is the easy one to forget:

- A Cilium LoadBalancer Service does not write a PROXY header. Under `Cluster`
  its SNAT happens in the eBPF datapath, which has no proxy in it. Cilium's
  own PROXY protocol support runs the other direction: `enableProxyProtocol`
  makes its Envoy *accept* a header sent by an upstream load balancer, and
  the open request is titled "Support PROXY Protocol **from** external
  LoadBalancers" ([cilium#21438](https://github.com/cilium/cilium/issues/21438)).
- So something must emit one: HAProxy (`send-proxy` or `send-proxy-v2`),
  NGINX stream (`proxy_protocol on`), Envoy, a Gateway that can be configured
  for it, or an external load balancer. This chart ships none of them.
- Health checks need their own switch. HAProxy's `send-proxy`/`send-proxy-v2`
  covers proxied traffic only: `server ... send-proxy check` probes the
  collector with a bare TCP connect unless `check-send-proxy` is also set.
  That is not an outage (the collector treats a connection that sends no
  header as a probe, counting `vantage_collector_proxy_header_total{result="closed"}`
  or `{result="timeout"}` and logging at debug), but such a check only proves
  the port is open. Prefer `send-proxy-v2` with `check-send-proxy`: the check
  then arrives as a v2 `LOCAL` command, which the collector closes cleanly and
  logs at debug. A v1 check sends an `UNKNOWN` line instead, closed just as
  cleanly but logged at warn.

The collector accepts both header versions. Set `collector.proxyProtocol:
"required"` **at the same time** as the front-end goes in. There is no mixed
mode by design: enabling it alone closes every directly connected router's
session, and enabling the front-end alone leaves the collector reading header
bytes as BMP. Both failures are loud and immediate, which is the intent; the
alternative to a hard cutover is a collector that quietly mis-attributes
routes. Keep the value quoted in values files (see "What the render refuses").

`collector.nodeSelector`, `collector.affinity` and `collector.tolerations` are
plain passthroughs and default to empty.

## NATS clustering

The bundled NATS runs as **three servers in one JetStream cluster**, one per
node, and the collector's streams keep **three copies**. Any one server can
restart, for a NATS upgrade, a `kubectl drain` or a node failure, while the
other two keep a quorum and keep accepting publishes, so the collector keeps
its BMP sessions and the archive has no gap.

That is not true of a single server. Its restart takes every stream offline,
the collector retries each publish for up to 60 seconds from when it was
first sent, and closes its BMP sessions if the server is not back by then
(see "Publish failures" in `docs/operating.md`). Routers reconnect
and most re-send their tables, but IOS-XR does not, so every NATS restart
cost part of the archive.

What the default needs:

- **Three schedulable nodes.** `nats.podTemplate.topologySpreadConstraints`
  is a hard rule (`kubernetes.io/hostname`, `maxSkew: 1`,
  `whenUnsatisfiable: DoNotSchedule`, `minDomains: 3`). A soft rule
  (`ScheduleAnyway`) lets the scheduler stack two or three servers on one
  node, with no error, and losing that node then loses the quorum and every
  stream. With fewer than three eligible nodes the third server stays
  `Pending` instead, which you can see. The three must be nodes the pods can
  run on: a node they cannot (a tainted control-plane node, say) still
  enters Kubernetes' skew arithmetic under its default
  `nodeTaintsPolicy: Ignore`, but never hosts a server.
- **An odd number of servers, at least three.** `nats.config.cluster.replicas`
  defaults to 3, and the render refuses 1, 2 or any even count. JetStream
  keeps a stream available only while a majority of its servers are up: with
  two, the majority is both, so losing either stops ingest, and the
  PodDisruptionBudget below would allow a drain that does exactly that. An
  even count tolerates no more failures than the odd count below it. To run
  five, also raise
  `nats.podTemplate.topologySpreadConstraints."kubernetes.io/hostname".minDomains`
  to 5: it is a literal (a parent chart cannot template a subchart's
  values), and the render refuses a cluster larger than it.
- **Three volumes.** Each server has its own `nats.config.jetstream.fileStore.pvc`
  (20Gi by default, `<release>-nats-js-<release>-nats-{0,1,2}`), and each
  holds a full copy of the ROUTES, LS, PEER and STATS streams. RAW, the
  24-hour replay buffer, keeps one copy on one server. Plan for three times
  the single-server disk.
- **Three times the NATS CPU and memory**, since every server stores and
  replicates every three-copy stream.

What it gives you:

- **NATS upgrades roll one server at a time.** The StatefulSet replaces one
  pod, waits for it to be ready (its startup probe waits for JetStream to
  catch up), then the next. Each server enters lame-duck mode before it
  stops, which moves its stream leaderships and client connections to the
  others. Moving a connection fails every publish the collector had
  awaiting an ack on it, and a leader stepping down drops the publishes it
  had not committed; the collector re-sends both under the same msg-id,
  counted by `vantage_collector_publish_retries_total` (see "Publish
  failures" in `docs/operating.md`), so the restart does not close BMP
  sessions. One exception: RAW keeps one copy on one server, so while that
  server restarts RAW has no leader at all, and after 20s of no answer a
  session publishing to RAW is closed. Only some sessions publish to RAW:
  those with a message the collector could not parse, a Termination
  message, or an armed mirror.
- **Node drains are safe.** The subchart's PodDisruptionBudget
  (`maxUnavailable: 1`) lets one NATS server be evicted at a time. On a
  cluster of exactly three nodes, the drained node's server stays `Pending`
  until the node is uncordoned, because it cannot share a node with another
  server; the other two keep the quorum, and the budget holds a second drain
  until the first server is back. With a fourth node the pods can run on, it
  reschedules there at once, provided its volume can move with it. A volume bound to one node
  (local-path and similar) cannot, so that server waits for its node either
  way.

`docs/measurements.md`, "Three-copy streams on three servers", measures
this default against one-copy streams on the same cluster: three copies cost
27% of end-to-end throughput at one prefix per UPDATE and 12% at 50, with
no loss in either.

### Stream copies

The collector creates its streams at startup with the replica count in its
config. When `collector.streams.replicas` is `0` (the default), the chart
derives it:

| NATS | `collector.streams.replicas` unset | set |
|---|---|---|
| bundled, clustered (the default) | 3, at any cluster size | your value |
| bundled, one server | not rendered: the collector's default, 1 | your value |
| external (`nats.enabled: false`) | not rendered: the collector's default, 1 | your value |

The LS stream follows `replicas` unless `collector.streams.lsReplicas` is
set, and RAW is always one copy. With external NATS the chart cannot see
how many servers you run, so set `collector.streams.replicas` to match: 3 for
a cluster of three or more, 1 for a single server. The render refuses a
`replicas` or `lsReplicas` larger than the bundled NATS's server count,
since JetStream would refuse to create the stream and the collector would
never start.

Changing the replica count of a running install takes effect when the
collector next starts, which a changed value triggers (it is part of the
collector's config). The collector updates each stream in place, and within
a cluster JetStream copies the existing messages to the new replicas: going
from one copy to three on a three-server cluster kept every message, and
publishes kept succeeding with each server stopped in turn (measured against
nats-server 2.11 in a local three-server cluster).

### Single-node clusters and homelabs

One value goes back to a single server with single-copy streams:

```yaml
nats:
  config:
    cluster:
      enabled: false
```

The stream copies follow it back to one. The spread rule stays in the pod
template but cannot hold one pod `Pending`: a single pod is always within a
skew of 1. Route TLS settings are ignored without a cluster. What you give
up is exactly what the section above describes: every NATS restart is a gap.

### Moving an existing install to three servers

An install on the single-server opt-out (`cluster.enabled: false`) does
not carry its streams across when it moves to three servers. The single
server's streams were created outside any cluster, and the new cluster does
not adopt them: once the server restarts clustered they are reported as not
found, and about 30 seconds after the cluster forms that server deletes them
from disk as orphans (`Detected orphaned stream` in its log). The writer's
durable consumers go the same way. Measured with nats-server 2.11.6, the
version the chart runs: 100 messages in a single-server stream, then the
same store restarted as one of three clustered servers, gave "stream not
found", the orphan line 30 seconds later, and an empty stream once the
collector recreated it.

So the move is a one-time, planned gap, and **routers are disconnected for
all of it**: from stopping the collector in step 1 until it starts again in
step 6, typically a few minutes. That is the price of a clean cutover. The
collector is stopped before NATS restarts, and stays stopped until the old
streams are gone, so that it never runs against a cluster that has no
streams yet or still holds the old single-server ones:

- A collector running through the switch would see every publish fail
  (measured: each one answered `no response from stream`, on every server,
  for the whole window) and close its BMP sessions over and over, each
  router re-sending its table into failures.
- A collector that starts early creates the three-copy streams while the
  orphans are still on disk. The measured outcome of that race was not the
  same across nats-server versions (the old messages were dropped by 2.11.6
  and kept by 2.11.15), so the procedure does not rely on it.
- The writer can only finish reading the old streams once nothing is
  publishing into them, which is also what makes step 2 able to reach zero.

In order (release `vantage` in namespace `vantage`; substitute
`<fullname>-collector`, `<fullname>-writer` and `<release>-nats-0` for other
names):

1. **Stop the collector.** Routers disconnect here.

       kubectl -n vantage scale statefulset/vantage-collector --replicas=0
       kubectl -n vantage wait --for=delete pod/vantage-collector-0 --timeout=2m

2. **Let the writer finish.** Wait for `vantage_sink_consumer_lag` to reach
   0 on every stream. Whatever it has not archived when NATS restarts in
   step 4 is lost.
3. **Re-issue the server certificate.** Routes need the `clientAuth` key
   usage, which earlier server certificates lack. With `gen-certs.sh`, run
   it again and apply the new `secrets.yaml` (it mints a new CA, so it
   replaces all three Secrets), and add the `cluster.tls.secretName` line it
   prints to your values if your release is not named `vantage`. With
   cert-manager, skip this: the upgrade changes the Certificate and
   cert-manager re-issues it.
4. **Upgrade with the collector held at zero:**

       helm upgrade vantage deploy/helm/vantage -n vantage -f my-values.yaml \
         --set collector.replicas=0 --wait --wait-for-jobs

   Without `--set collector.replicas=0` this upgrade would start the
   collector by itself: the collector's config changes (its streams become
   three-copy), and the change to its `checksum/config` annotation rolls
   it. The writer's config does not change, so the upgrade leaves the
   writer running, still attached to the old consumers. (The API also
   restarts, because its list of collectors follows `collector.replicas`,
   and again in step 6.)
5. **Wait for the old streams to be deleted.** Once all three NATS pods are
   ready, the server that held the old volume logs one line per stream:

       kubectl -n vantage logs vantage-nats-0 -c nats | grep 'Detected orphaned stream'

   Continue when the five streams (ROUTES, LS, PEER, STATS, RAW) are listed,
   or, if the log has rotated, at least a minute after all three pods became
   ready.
6. **Start the collector** by upgrading again without the override:

       helm upgrade vantage deploy/helm/vantage -n vantage -f my-values.yaml \
         --wait --wait-for-jobs

   It creates the streams, three-copy, as it starts. Routers reconnect.
7. **Restart the writer**, which creates its consumers only at startup and
   does not notice that the old ones are gone:

       kubectl -n vantage rollout restart deployment/vantage-writer

   Then check that `vantage_sink_fetch_errors_total` stops rising and
   `vantage_sink_consumer_lag` stays near 0.

The same sequence, run against three nats-server 2.11.6 containers,
published nothing during the window and then acknowledged every publish
into the new three-copy stream, including with each server stopped in turn.

Routers that re-send their tables on reconnect (NX-OS) fill the archive back
in after step 6; IOS-XR does not (see "Publish failures" in
`docs/operating.md`), so reset its BGP neighbors or accept the gap. From then
on, NATS restarts no longer cause one.

An install that was already clustered, with single-copy streams, needs none
of this: the upgrade raises its streams to three copies in place.

## NATS TLS

Mutual TLS between the daemons and NATS is **on by default**
(`nats.tls.enabled: true`). The bundled NATS has no authentication of its
own, so without TLS any pod that can reach it can publish forged routes into
the pipeline and read every router's feed. The chart's defaults already
configure the server side (`nats.tlsCA`, `nats.config.nats.tls`, with
`verify: true`) and turn nats-box off (below); what you supply is the
certificates, by one of the two paths below. A default install with neither
fails the render with a message saying so.

The routes between the three NATS servers are mutual TLS too
(`nats.config.cluster.tls`, `verify: true`), on the same server certificate:
one server dials each route and presents the certificate as a client
certificate, and the other presents it as a server certificate, so it
carries both the `serverAuth` and `clientAuth` key usages, and a wildcard
for the route names, `*.<release>-nats-headless.<ns>.svc.cluster.local`.
Routes are dialed by that FQDN (`nats.config.cluster.routeURLs.useFQDN`);
on a cluster whose domain is not `cluster.local`, set
`nats.config.cluster.routeURLs.k8sClusterDomain` and pass the same domain to
`gen-certs.sh --cluster-domain` (the cert-manager path reads it from the
value). `clientAuth` on the server's certificate lets nothing new in: only
the NATS servers hold its key.

### Running without TLS

Plaintext is an explicit opt-out. With the bundled NATS it takes four
values, because the server half lives in the subchart's values, which this
chart cannot template:

```yaml
nats:
  tls:
    enabled: false
  tlsCA:
    enabled: false
  config:
    nats:
      tls:
        enabled: false
    cluster:
      tls:
        enabled: false
```

Setting only `nats.tls.enabled: false` fails the render, naming the others.
The last one, route TLS, is checked only while NATS is clustered; with the
single-server opt-out the first three are enough. With external NATS
(`nats.enabled: false`), `nats.tls.enabled: false` alone is enough.

Turning TLS on for a running plaintext install restarts NATS and both
daemons; the collector's routers reconnect.

### This chart creates no private key

It mounts certificate Secrets or asks cert-manager for them; it never
generates a key itself. That is the same reasoning that keeps ClickHouse out
of the chart: a chart should not mint a credential for something it does not
own.

Helm's `genCA`/`genSignedCert` would have made generation easy, but they
regenerate on every render, so two identical `helm template` runs produce two
different CAs. A chart built on them would mint a fresh PKI on any `helm
upgrade`, including one that changed an unrelated value. The NATS StatefulSet
and the two client workloads roll at different times, so mid-roll a
collector still holding the old CA cannot reach a server already presenting
the new certificate, and for BMP that means dropped router sessions.
`lookup`, the usual way to keep a Secret from an earlier render, returns empty
under `helm template`, which would make rendered output depend on live cluster
state.

So the certificates come from one of two places outside the chart.

### Bring your own (`deploy/nats-tls/gen-certs.sh`)

    deploy/nats-tls/gen-certs.sh --release vantage --namespace vantage
    kubectl apply -n vantage -f nats-tls/secrets.yaml

`--release` and `--namespace` must match the Helm release you install: the
certificate names and SANs are built from them. The script writes to
`./nats-tls` by default (`--out` to change it) and prints the values block
that wires its output in. For release `vantage` that is the block below.
Only the two client Secret names differ from the chart's defaults, which is
why the quick start sets just those; for another release name, the server
`secretName`s differ too, so paste the whole block the script prints.

    nats:
      natsBox:
        enabled: false
      tls:
        enabled: true
        collectorSecret: vantage-vantage-nats-collector-tls
        writerSecret: vantage-vantage-nats-writer-tls
      tlsCA:
        enabled: true
        secretName: vantage-nats-server-tls
      config:
        nats:
          tls:
            enabled: true
            secretName: vantage-nats-server-tls
        cluster:
          tls:
            enabled: true
            secretName: vantage-nats-server-tls

(The script names the client Secrets `<release>-vantage-nats-<daemon>-tls` and
the server Secret `<release>-nats-server-tls`, hence the doubled name for a
release called `vantage`.)

`nats.tls.*` is the client side, for the collector and the writer.
`nats.tlsCA`, `nats.config.nats.tls` and `nats.config.cluster.tls` are
subchart values and configure the NATS server. `nats.config.nats.tls.merge.verify`
is `true` in this chart's defaults, and that is what turns server TLS into
mutual TLS. (nats-server verifies route peers whether or not `verify` is
set; the chart sets it on `cluster.tls` as well so the rendered config says
so.)

Keep `nats-tls/ca.key`: it is the only thing that can issue more certificates
against this CA. See `deploy/nats-tls/README.md`.

### nats-box and `helm test` are off

`nats.natsBox.enabled` defaults to `false`, and that is the one real loss of
the mTLS default. The NATS subchart renders its nats-box context with the CA
and **no client keypair**, and its `helm test` job (`request-reply`) connects
through that same context, so a `verify: true` server refuses both. Left
enabled, `helm test vantage` fails on a healthy install, and `kubectl exec
... nats-box -- nats ...`, the standard way to inspect streams and consumers,
cannot connect either. The parent chart cannot fix this by templating: a
subchart's values cannot see `.Release.Name`, so there is no way to name a
per-release client Secret from here.

To turn it back on, set `nats.natsBox.enabled: true` and give nats-box a
client certificate of its own: point
`nats.natsBox.contexts.default.tls.secretName` at a Secret carrying a
clientAuth certificate signed by the same CA. Either client Secret
above works, though issuing nats-box a third one keeps the daemons'
certificates out of a debugging shell. The subchart's `helm test` then passes
as well, and the NATS NetworkPolicy admits both pods whenever nats-box is
enabled. (With TLS off, nats-box needs no certificate: `nats.natsBox.enabled: true`
alone brings it back.)

The server Secret name appears as a literal, rather than being templated from
the release name, for the same reason. The chart reads it back out of
`nats.config.nats.tls.secretName` wherever it needs it, and the render fails
if `nats.tlsCA.secretName` or, with the cluster on,
`nats.config.cluster.tls.secretName` disagrees.

### cert-manager instead

If you run cert-manager, skip the script and name an issuer:

    nats:
      natsBox:
        enabled: false
      tls:
        enabled: true
        issuerRef: {name: my-ca-issuer, kind: ClusterIssuer}
      tlsCA:
        enabled: true
        secretName: vantage-nats-server-tls
      config:
        nats:
          tls:
            enabled: true
            secretName: vantage-nats-server-tls
        cluster:
          tls:
            enabled: true
            secretName: vantage-nats-server-tls

The block above spells out the defaults in full; `issuerRef` is the only
line the default configuration needs. The chart then issues three
Certificates. The two client Secrets are named
after the release's full name (`vantage-nats-collector-tls` and
`vantage-nats-writer-tls` for release `vantage`; `<release>-vantage-nats-...`
for any release name that does not already contain `vantage`). The server
Secret is whatever `nats.config.nats.tls.secretName` says. All three carry the
same `ca.crt`/`tls.crt`/`tls.key` layout the script produces and the same key
usages (`digital signature`, `key encipherment`, and `client auth`, plus
`server auth` on the server's), so both paths mount identically and present
equivalent certificates.
cert-manager's `usages` field replaces its defaults rather than adding to
them, which is why the chart spells out the two key usage bits.

Set `nats.tls.issuerRef` **or** `nats.tls.collectorSecret` /
`nats.tls.writerSecret`, never both; the render refuses the combination.

The server `secretName` defaults to the literal `vantage-nats-server-tls`,
because a parent chart cannot template a subchart's values. Two vantage
releases in one namespace would therefore both issue a server Certificate
into that one Secret, each overwriting the other's SANs. Give each release its
own name (`<release>-nats-server-tls`, as the script does) whenever more than
one release shares a namespace.

The chart never creates an Issuer: an issuer is your trust root, and a chart
that mints one is making a security decision on your behalf.

Renewal needs nothing from vantage. The daemons re-read their certificate
files on each connection attempt, so a renewed certificate is picked up at the
next reconnect. The certificate Secrets are deliberately left out of the
`checksum/config` annotation that rolls the pods; otherwise every renewal would
restart both daemons for nothing.

### External NATS

`nats.tls.*` is independent of `nats.enabled` on purpose. To point vantage at
your own TLS-enabled NATS, set `nats.enabled=false`, `nats.externalURL`, and
the two client Secrets. `nats.tlsCA` and `nats.config.*` are ignored in that
case, because there is no bundled server to configure.

## Health probes and logging

Each daemon serves two health endpoints on its metrics port (the collector's
`collector.metricsPort`, the writer's `writer.metricsPort`, the API's
`api.metricsPort`), and the chart probes them:

- **Liveness: `/healthz`**, 200 whenever the process is serving. It never
  follows a dependency, because restarting a daemon does not bring NATS or
  ClickHouse back.
- **Readiness: `/readyz`**, 503 with a one-line reason while a dependency is
  unusable: NATS for the collector, NATS and ClickHouse for the writer. A
  NotReady collector drops out of the BMP Service, so routers are not sent to
  a collector that cannot publish. The collector's headless metrics Service
  publishes NotReady pods anyway, so the API's collector cards and a
  Prometheus scrape can still reach a collector that has lost NATS.
- **The API's readiness probe uses `/healthz`.** Its `/readyz` still reports
  ClickHouse health, for monitoring. But gating readiness on it would pull a
  single API replica out of its Service during a ClickHouse outage, and the UI
  would get a bare connection error instead of the API's own explanation.

`logLevel` (`debug`, `info`, `warn`, `error`; default `info`) and `logFormat`
(`text` or `json`; default `text`) set all three daemons' `log_level` and
`log_format`. `collector.logLevel`, `writer.logFormat` and so on override them
per component. Any other value fails the render.

`api.staleAfter` (default `90s`; a Go duration from `60s` to `24h`) is how
long a collector may go without a heartbeat before the API serves its peers
as `stale`, with a `collector_stale` warning. Collectors send one every 30
seconds. After lowering `collector.replicas`, the retired collectors' peers
stay `stale` until you remove their rows with `vantage purge` (see "Retiring
a router or collector" in [`docs/operating.md`](../../docs/operating.md#retiring-a-router-or-collector)).

## Prometheus

Every daemon serves Prometheus metrics at `/metrics` on its metrics port
(collector 9469, writer 9472, API 9474), and each has a Service exposing
that port under the name `metrics`, labeled `app.kubernetes.io/component`
(`collector`, `writer` or `api`). docs/operating.md lists the metrics and
which to alert on.

With the Prometheus Operator (kube-prometheus-stack, for example), let the
chart create the ServiceMonitors:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    labels:
      release: kube-prometheus-stack   # whatever your Prometheus's serviceMonitorSelector matches
    interval: 30s                      # optional
```

It is off by default, because a ServiceMonitor fails to install on a cluster
without the operator's CRDs. `metrics.serviceMonitor.collector`, `.writer`
and `.api` (all `true`) turn off monitoring for one component.

NATS is scraped separately. Its built-in monitoring endpoint serves JSON, not
Prometheus format, so the NATS subchart runs `prometheus-nats-exporter` as a
sidecar and has its own PodMonitor. This chart cannot switch those on from
`metrics.serviceMonitor` (a parent chart cannot derive a subchart's values),
so set them alongside it:

```yaml
nats:
  promExporter:
    enabled: true
    podMonitor:
      enabled: true
      merge:
        metadata:
          labels:
            release: kube-prometheus-stack
networkPolicy:
  nats:
    metricsFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring   # where Prometheus runs
```

The install notes say so if ServiceMonitors are on and NATS's exporter is
not, or if the exporter is on but the NetworkPolicy would block the scrape.
The collector and writer have no NetworkPolicy, and the API's allows its
metrics port from anywhere.

## Per-router overrides

`collector.routers` is passed through to the collector's `routers:` map,
keyed by router IP address:

```yaml
collector:
  routers:
    "192.0.2.11": {vendor: cisco, os: iosxr}
    "2001:db8::1": {vendor: cisco, os: nxos, disable_quirks: [QK_TS_ZERO]}
```

`vendor` and `os` state a router's identity when its BMP Initiation banner
does not (XRd's banner is a bare version number, which no vendor match can
recognize); `force_quirks` and `disable_quirks` apply or suppress quirks by
ID. The collector refuses to start on a key that is not an IP address, an
unknown quirk ID, or an `os` without a `vendor`; the chart does not check them.

## NetworkPolicy and pod security

`networkPolicy.enabled` (default `true`) renders two ingress-only
NetworkPolicies:

- **NATS** (bundled only) accepts client connections on 4222 from the
  collector and writer pods, and from nats-box and the subchart's `helm test`
  pod when `nats.natsBox.enabled` is on. Its cluster-route port (6222) is
  open to the NATS pods themselves, which is how the three servers reach each
  other. Its monitor port (8222) is closed to other pods unless
  `networkPolicy.nats.metricsFrom` lists NetworkPolicy peers (a Prometheus
  namespace, say), which also opens the exporter port when
  `nats.promExporter.enabled` is on. Anything else goes in
  `networkPolicy.nats.extraIngress` as raw ingress rules.
- **The API** accepts its HTTP and metrics ports from anywhere, and no other
  port. It is meant to be reached; the policy closes nothing a client uses.

No egress policy is rendered. **These take effect only on a cluster whose CNI
enforces NetworkPolicy** (Calico, Cilium, Antrea and others). On one that does
not, the API server accepts them and nothing changes. kubelet's health probes
come from the node, which those CNIs admit regardless of policy.

Every pod this chart renders by default, the bundled NATS included, meets
the Kubernetes `restricted` Pod Security Standard, so the release namespace
can enforce it (nats-box, which is off by default, is the exception; see
below):

    kubectl label namespace vantage pod-security.kubernetes.io/enforce=restricted

The collector, writer, API and schema Job run as uid 65532 with
`runAsNonRoot`, the `RuntimeDefault` seccomp profile, no privilege
escalation, all capabilities dropped and no mounted service account token.
The three daemon containers and the schema Job also run with a read-only root
filesystem. The daemons write only to the emptyDir that holds their rendered
config. The images themselves run as the same uid (distroless `:nonroot`), so
they are non-root outside Kubernetes too.

The NATS pods run as uid 1000, with the same seccomp profile, no privilege
escalation and no capabilities. The subchart's images declare no user of
their own, so the chart sets one through the subchart's merge values
(`nats.podTemplate.merge`, `nats.container.merge`, `nats.reloader.merge` and
`nats.promExporter.merge`), and `fsGroup: 1000` makes the JetStream volume
writable by it. A CSI driver that applies group ownership itself at mount
time (the `VOLUME_MOUNT_GROUP` capability) may not honor
`fsGroupChangePolicy` the same way kubelet does; check the storage class's
driver if the JetStream volume is not group-writable after an upgrade.
Upgrading an install whose NATS ran as root restarts the
NATS pods once, and on that first mount kubelet gives every file already in
the JetStream store to group 1000; nothing is copied or lost. nats-box
(`nats.natsBox.enabled`) is not covered: its pods set no security context,
and a namespace enforcing `restricted` refuses them.

## What the render refuses

Each of these fails `helm template`/`install`/`upgrade` with a message naming
the value, rather than producing a release that looks deployed and moves no
data:

- `clickhouse.externalHost` empty.
- Neither `clickhouse.existingSecret` nor `clickhouse.auth.password` set.
- `clickhouse.database` set to anything but `vantage`.
- `logLevel`/`logFormat` (top-level or per component) outside the values the
  daemons accept.
- `nats.enabled=false` with `nats.externalURL` empty.
- `api.tokens` empty. vantage-api refuses to start with zero tokens in token
  mode, and this chart renders only token mode.
- An `api.tokens[].name` that is not a valid shell variable suffix
  (`^[A-Za-z_][A-Za-z0-9_]*$`), or two names that collide once upper-cased
  (`lab` and `Lab`). The API pod's init container reads each token from
  `$TOKEN_<NAME>`, and either case would silently write the wrong token.
- `collector.proxyProtocol` other than `"off"` or `"required"`, including the
  boolean an unquoted `proxyProtocol: off` becomes in a values file (Helm
  reads values files with YAML 1.1 rules, where `off`/`on`/`yes`/`no` are
  booleans). `--set collector.proxyProtocol=off` is unaffected, because `--set`
  values are strings.
- `nats.tls.enabled` on (the default) with no client Secrets and no
  `issuerRef`: the default install, before any certificates are configured.
  The message lists the three ways forward.
- `nats.tls.enabled` on with `nats.config.nats.tls.enabled` off: the daemons
  would demand TLS from a plaintext server.
- `nats.tls.enabled` off with `nats.config.nats.tls.enabled` or
  `nats.tlsCA.enabled` still on: the server would demand client certificates
  the daemons do not present, or mount a CA Secret nobody created. Plaintext
  takes all three switched off.
- `collector.trustedProxies` set with `collector.proxyProtocol: "off"`.
- `nats.tls.enabled` on with `nats.tlsCA.enabled` off: the server would have
  no CA bundle to check client certificates against.
- `nats.tlsCA.secretName` naming a different Secret from
  `nats.config.nats.tls.secretName`.
- With the bundled NATS clustered and `nats.tls.enabled` on:
  `nats.config.cluster.tls.enabled` off (client connections mutual TLS,
  routes plaintext), or `nats.config.cluster.tls.secretName` naming a
  different Secret from `nats.config.nats.tls.secretName`. With
  `nats.tls.enabled` off, route TLS still on.
- A clustered bundled NATS whose `nats.config.cluster.replicas` is even or
  below 3, or larger than the hostname spread's `minDomains`.
- `collector.streams.replicas` or `collector.streams.lsReplicas` larger than
  the number of bundled NATS servers: JetStream would refuse the stream and
  the collector would never start.
- `nats.tls.enabled` on with only one of the two client Secrets: the other
  daemon would mount nothing.
- `nats.tls.issuerRef.name` set **and** a client Secret named: two issuance
  paths at once, where the chart would take the issuer and ignore your Secret.
- `nats.tls.issuerRef` set but with no `name`. The chart would otherwise fall
  back to bring-your-own and then complain that no `issuerRef` is set,
  pointing you at the one value you did set. The fully unset default
  (`issuerRef: {}`) is the bring-your-own path and stays legal.

The checks involving `nats.config.*`, `nats.tlsCA` or the NATS server count
apply only with the bundled NATS (`nats.enabled=true`).

These are render-time checks. Nothing here proves that cert-manager accepts
the issued Certificates or that a live NATS accepts the rendered server
config; the certificates themselves are covered by a test that runs
`gen-certs.sh` and completes a real mutual-TLS handshake with its output.

## Uninstalling

    helm uninstall vantage --namespace vantage

This leaves some things behind on purpose:

- **NATS JetStream's volumes.** PVCs created from a StatefulSet's
  `volumeClaimTemplates` are not deleted by `helm uninstall`. The JetStream
  file store (`nats.config.jetstream.fileStore.pvc`, 20Gi on the cluster's
  default StorageClass unless you set `storageClassName`) is one volume per
  NATS server, bound as `<release>-nats-js-<release>-nats-0` through `-2`
  (only `-0` with the single-server opt-out).
- **Two Secrets** annotated `helm.sh/resource-policy: keep`: the generated API
  tokens (`vantage-api`, i.e. `<fullname>-api`), so clients' tokens survive a
  reinstall, and, if you used `clickhouse.auth.password`, the ClickHouse
  credentials (`vantage-clickhouse`).
- **ClickHouse, untouched.** The chart creates no database and no database
  volume, so uninstalling never touches ClickHouse storage. Clean up there on
  your own terms.

To reclaim the space, delete the NATS volumes by name. For a release named
`vantage` with the default three NATS servers:

    kubectl -n vantage get pvc     # check the names first
    kubectl -n vantage delete pvc vantage-nats-js-vantage-nats-0 \
        vantage-nats-js-vantage-nats-1 vantage-nats-js-vantage-nats-2
    kubectl -n vantage delete secret vantage-api vantage-clickhouse --ignore-not-found
    kubectl get pv | grep Released     # only with a Retain reclaim policy

For another release name the PVCs are `<release>-nats-js-<release>-nats-0`
through `-2`; with the single-server opt-out there is only `-0`. **Never
run `kubectl delete pvc --all` in this namespace.** If ClickHouse runs in the
same namespace, as `deploy/clickhouse/k8s/` sets it up, its data and Keeper
PVCs hold the route archive, and `--all` deletes them too. Do not delete a
ClickHouse PVC as part of uninstalling vantage.

Whether deleting the PVC frees the disk depends on your StorageClass's reclaim
policy. With `Delete`, it does. With `Retain`, the PersistentVolume stays
`Released` until you delete it, and some storage systems keep their own
object behind it even then. Longhorn, for example, keeps a
`volumes.longhorn.io` object named after the PV (`pvc-<uuid>`) in
`longhorn-system` that holds the disk allocated until it is deleted too;
deleting only the PVs reclaimed nothing in a measured cleanup of four of
them:

    kubectl -n longhorn-system get volumes.longhorn.io | grep <pv-name>
    kubectl -n longhorn-system delete volumes.longhorn.io <pv-name>
