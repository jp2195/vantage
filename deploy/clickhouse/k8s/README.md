# ClickHouse under the official operator

The vantage Helm chart does not deploy ClickHouse. It requires
`clickhouse.externalHost` (the render fails without it) and credentials,
from `clickhouse.existingSecret` or `clickhouse.auth.password`. This
directory is one way to provide that database: ClickHouse run by the official
ClickHouse-org operator
([ghcr.io/clickhouse/clickhouse-operator-helm](https://github.com/ClickHouse/clickhouse-operator)),
installed beside the chart rather than inside it.

- `clickhouse-values.yaml`: values for the operator's
  `clickhouse-cluster-helm` chart, with the security hardening described
  below.
- This README: the install, the verification steps, and what was found
  running it.

**Tested with operator v0.0.7**, on Kubernetes 1.36. That is an early
release of a young project, chosen deliberately in favor of the official
ClickHouse project over the more mature third-party (Altinity) operator.
Newer releases may change the CRDs; re-check the field names before using a
different version. The CRDs are `ClickHouseCluster` and `KeeperCluster`, NOT
Altinity's `ClickHouseInstallation` shape. Don't take field names from that
operator or from older blog posts; `kubectl explain clickhousecluster.spec
--recursive` against a real install of the version you run is the reliable
source, and is how `clickhouse-values.yaml` was written.

## Finding: the built-in `default` user has no authentication

The operator's own base config (`users.yaml`, loaded before any
`extraUsersConfig` override) ships the built-in `default` user with
`no_password:` and no `networks` restriction. ClickHouse resolves that to
"reachable from anywhere, no password". Measured on a live install with
operator v0.0.7, before the fix below:

```
$ clickhouse-client --user vantage --password "$CHPASS" --query \
  "SELECT name, auth_type, host_ip FROM system.users ORDER BY name FORMAT TSV"
default   no_password         ['::/0']
operator  sha256_password     ['::/0']
vantage   plaintext_password  ['10.244.0.0/16','10.96.0.0/12']

$ clickhouse-client --user default --query "SELECT currentUser()"
default
```

Anything that could reach the ClickHouse Service had unauthenticated
superuser SQL on the route archive, even though the `vantage` user was
already hardened (password plus CIDR restriction). vantage has hit this
vulnerability class before from a different direction (Grafana's
anonymous-access role resolving to a ClickHouse superuser), so check for it
on every new path into this database, not just this one.

`clickhouse-values.yaml` locks `default` down. `no_password` is explicitly
removed with `{"@remove": "1"}`: a later config file's *same-named* child
key replaces the earlier one, but `no_password` and `password_sha256_hex`
are *different* tags, so without the explicit `@remove` both would coexist
and ClickHouse would refuse to start with "more than one field of password
... is used". Then `default` gets a digest password and is restricted to
loopback (`127.0.0.1`, `::1`). Nothing authenticates as `default` over the
network on this operator version: the operator reconciles as its own
dedicated `operator` user, and its version-probe Job runs `clickhouse local`
(a standalone process with no server connection at all). Both were
confirmed by reading them on a live install before making the change, not
assumed.

**A nuance found while verifying the fix, not a gap in it:** testing the
network restriction by `kubectl exec`-ing into the ClickHouse pod and
connecting to its own headless Service name is NOT a valid test of remote
access. ClickHouse still let `default` in from that path, even though the
peer address (the pod's own IP, confirmed in the server log) is not
`127.0.0.1`/`::1`. The restriction is enforced for everyone else: from a
throwaway pod elsewhere in the namespace, the identical connection was
refused. What remains is "a process already running inside the ClickHouse
pod's own network namespace can reach `default` via its own address". That
requires already being inside that exact pod, a trust boundary that already
grants direct filesystem access to everything the database protects, so it
does not reopen the vulnerability above.

**Always test this restriction from a different pod, never by exec-ing
into the ClickHouse pod and connecting to itself.** The verification steps
below use a throwaway probe pod for exactly that reason.

## Prerequisites

- cert-manager, running in the cluster. The operator's webhook needs it for
  its TLS certificate and will not start without it. If you do not run it
  yet, cert-manager's
  [Helm install](https://cert-manager.io/docs/installation/helm/) is one
  command. Check it:

  ```bash
  kubectl get pods -A | grep cert-manager
  ```

- A namespace for ClickHouse and vantage. The commands below put both in one
  namespace, held in `$NS`:

  ```bash
  NS=<your-namespace>
  kubectl create namespace "$NS"
  ```

  If you put ClickHouse in a different namespace from the vantage release,
  set `clickhouse.externalHost` to the Service's fully qualified name
  (`<service>.<clickhouse-namespace>.svc`) in step 6, and create the Secret
  in step 2 in the vantage release's namespace.

- A copy of `clickhouse-values.yaml`. Without a clone of this repository,
  download it from the tag of the vantage release you install:

  ```bash
  curl -fsSLO https://raw.githubusercontent.com/jp2195/vantage/v0.1.0/deploy/clickhouse/k8s/clickhouse-values.yaml
  ```

  The commands below read it from the current directory; from a clone, use
  `deploy/clickhouse/k8s/clickhouse-values.yaml`.

- Edit `clickhouse-values.yaml` for your cluster before step 3:
  - `storageClassName` (both the ClickHouse and Keeper volumes). Left
    unset, the PVCs use the cluster's default StorageClass. This volume
    holds the route archive, so a class whose reclaim policy is `Retain`
    is worth considering.
  - The `vantage` user's `networks`: your cluster's pod CIDR and Service
    CIDR. The committed values (`10.244.0.0/16`, `10.96.0.0/12`) are common
    defaults and may not match your cluster: k3s, for one, uses
    `10.42.0.0/16` and `10.43.0.0/16`. `kubectl get nodes -o
    jsonpath='{.items[*].spec.podCIDR}'` prints each node's slice of the pod
    CIDR, and the `kubernetes` Service's `ClusterIP` (`kubectl get svc
    kubernetes -n default`) is the first address of the Service CIDR. If they
    are wrong, the writer and API are refused with the same
    `AUTHENTICATION_FAILED` a wrong password gives (see step 5).
  - `containerTemplate.resources` and the volume sizes, for your nodes.

## 1. Install the operator

```bash
helm install clickhouse-operator \
  oci://ghcr.io/clickhouse/clickhouse-operator-helm --version 0.0.7 \
  --create-namespace -n clickhouse-operator-system \
  --wait --wait-for-jobs --timeout 10m
kubectl get crd | grep -i clickhouse
# Expect: clickhouseclusters.clickhouse.com, keeperclusters.clickhouse.com
```

## 2. Create the shared credentials Secret

One password, read by both the database (as a digest, via `--set` at
install time, never committed) and the vantage chart (in plaintext, via
`clickhouse.existingSecret`, keys `username` and `password`).

```bash
CHPASS=$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | head -c 24)
echo "password is: $CHPASS"   # record it; every step below needs it
kubectl -n "$NS" create secret generic vantage-clickhouse-external \
  --from-literal=username=vantage \
  --from-literal=password="$CHPASS" \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 3. Deploy the cluster

`clickhouse-values.yaml` is committed with placeholder digests
(`REPLACE_ME_VIA_HELM_SET_SHA256`) for both the `vantage` user and the
built-in `default` user; neither is ever the real value. Both users use
`password_sha256_hex`, not a plaintext `password` field: the rendered
ClickHouse config is a ConfigMap any cluster reader can see, so it must hold
a digest, not a reversible credential. Compute both digests and supply them
only at install time:

```bash
CHPASS_SHA256=$(printf '%s' "$CHPASS" | sha256sum | cut -d' ' -f1)
DEFAULT_PASS=$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | head -c 24)
DEFAULT_SHA256=$(printf '%s' "$DEFAULT_PASS" | sha256sum | cut -d' ' -f1)
# DEFAULT_PASS only ever needs to be known to whoever already has exec
# access to the ClickHouse pod (`default` is loopback-only). Record it if
# you want it; it is not load-bearing the way CHPASS is.

helm install vantage-ch \
  oci://ghcr.io/clickhouse/clickhouse-cluster-helm --version 0.0.7 \
  -n "$NS" -f clickhouse-values.yaml \
  --set clickhouse.spec.settings.extraUsersConfig.users.vantage.password_sha256_hex="$CHPASS_SHA256" \
  --set clickhouse.spec.settings.extraUsersConfig.users.default.password_sha256_hex="$DEFAULT_SHA256" \
  --wait --wait-for-jobs --timeout 15m
# This chart renders only the two custom resources, and the operator starts
# the pods from them, so --wait cannot tell when the database is serving:
# Helm 3 returns as soon as the resources exist, and Helm 4 reads only what
# their status reports. Wait on the resources themselves before reading pod
# names.
kubectl -n "$NS" wait --for=condition=Ready --timeout=15m \
  clickhousecluster/vantage-ch-clickhouse-cluster \
  keepercluster/vantage-ch-clickhouse-cluster
kubectl -n "$NS" get clickhouseclusters,keeperclusters,pods,svc
```

Pin the cluster chart's `--version` to the same version as the operator
chart. Both charts' values are generated from the CRD schemas of the
operator they ship with, so a version skew between them is a mismatch, not a
supported combination.

This renders a `ClickHouseCluster` and a `KeeperCluster` (Keeper is
required; see the notes at the end). On v0.0.7 the operator creates a
headless client-facing Service named
`<release>-clickhouse-cluster-clickhouse-headless`, e.g.
`vantage-ch-clickhouse-cluster-clickhouse-headless` for the release above.
Read the real name from the `get svc` output rather than assuming it:

```bash
CH_SVC=<the-headless-service-name>
CH_POD=<a-clickhouse-pod-name>   # e.g. vantage-ch-clickhouse-cluster-clickhouse-0-0-0
```

The `...-clickhouse-version-probe-...` pod that also appears is a one-shot
the operator runs to read the server version; it ends `Completed` and is not
a ClickHouse server.

**A missing `--set` fails loudly, on purpose.** `REPLACE_ME_VIA_HELM_SET_SHA256`
is NOT valid hex: it is 30 characters (a real sha256 digest is 64) and 18 of
them fall outside `[0-9a-fA-F]`. If either `--set` is omitted or mistyped so
that the literal string reaches `password_sha256_hex`, ClickHouse's config
loader rejects it and the pod fails to come up: a CrashLoopBackOff with an
explicit config-validation error in its logs, not a quiet fallback. Do not
"fix" the placeholder into something that merely looks like real hex (64
characters of `[0-9a-fA-F]`) to make this failure go away. A well-formed but
fake digest *is* accepted with no complaint, silently locking that user to
whatever password happens to hash to it, which is the dangerous version of
this mistake.

## 4. Verify the `default` user is locked down

Start a throwaway probe pod. It is a different pod from ClickHouse, which is
what makes it a valid test of remote access (see the nuance above). The
probe runs as root with default capabilities, so a cluster that warns at the
`restricted` Pod Security level prints a "would violate PodSecurity" warning
for it; the pod still starts, and step 7 deletes it.

```bash
kubectl -n "$NS" run ch-probe --image=clickhouse/clickhouse-server:26.8-alpine \
  --restart=Never --command -- sleep 3600
kubectl -n "$NS" wait --for=condition=Ready pod/ch-probe --timeout=2m

# From another pod, even with the right password: expect AUTHENTICATION_FAILED.
kubectl -n "$NS" exec ch-probe -- clickhouse-client --host "$CH_SVC" \
  --user default --password "$DEFAULT_PASS" --query "SELECT 1"
```

Loopback is the one path the config still allows, with the password. With
no `--host`, `clickhouse-client` connects to `localhost` as `default`:

```bash
# Inside the ClickHouse pod, over loopback, with the password: expect 1.
kubectl -n "$NS" exec "$CH_POD" -- clickhouse-client \
  --password "$DEFAULT_PASS" --query "SELECT 1"
```

## 5. Verify the vantage user

From the probe pod, which is the same kind of path the vantage writer and
API take:

```bash
kubectl -n "$NS" exec ch-probe -- clickhouse-client --host "$CH_SVC" \
  --user vantage --password "$CHPASS" --query "SELECT version()"
# Expect a 26.8.x version string.
```

`clickhouse-values.yaml` restricts the `vantage` user's `networks` to the
pod CIDR and the Service CIDR, and deliberately does NOT include loopback.
So `clickhouse-client --user vantage` run inside the ClickHouse pod with no
`--host` is refused (`IP_ADDRESS_NOT_ALLOWED`, surfaced to the client as the
same generic `AUTHENTICATION_FAILED` a wrong password gives). If the probe
pod is refused too, check the server's log (`kubectl logs "$CH_POD"`), not
just the client's error text: an address-not-allowed there means the CIDRs
in `clickhouse-values.yaml` do not match your cluster.

## 6. Point the vantage release at it

First create the NATS client certificates, as in the quick start of
`deploy/helm/README.md`. NATS mutual TLS is on by default and the chart
generates no keys, so the render fails without them. The script comes from
the release's tag (from a clone, run `deploy/nats-tls/gen-certs.sh`):

```bash
curl -fsSLO https://raw.githubusercontent.com/jp2195/vantage/v0.1.0/deploy/nats-tls/gen-certs.sh
bash gen-certs.sh --release vantage --namespace "$NS"
kubectl apply -n "$NS" -f nats-tls/secrets.yaml
```

Then install the published chart, which bundles its NATS subchart (use the
newest version on the
[Releases page](https://github.com/jp2195/vantage/releases); from a clone,
see "Installing from source" in `deploy/helm/README.md`):

```bash
helm install vantage oci://ghcr.io/jp2195/charts/vantage --version 0.1.0 \
  --namespace "$NS" \
  --set clickhouse.externalHost="$CH_SVC" \
  --set clickhouse.existingSecret=vantage-clickhouse-external \
  --set nats.tls.collectorSecret=vantage-vantage-nats-collector-tls \
  --set nats.tls.writerSecret=vantage-vantage-nats-writer-tls \
  --wait --wait-for-jobs --timeout 15m
```

Add your other values (API tokens, images, `collector.allowedSources`) as
`deploy/helm/README.md` describes. `clickhouse.externalPort` defaults to 9000 (the native protocol),
which matches this setup. The chart always uses the `vantage` database; there
is no setting for it.

`--wait-for-jobs` is not optional. The chart applies the schema from a Job
(`schemaJob.apply`, default `true`), and `--wait` alone does not wait for a
Job to finish (Helm 3 skips Jobs, Helm 4 counts a started Job as ready), so
a failed schema Job would otherwise leave a release Helm reports as
successful.

## 7. Verify the schema landed

**The schema Job verifies its own result**, so the first check is that it
succeeded. After applying every file it reads `max(version)` back and fails
if the database does not report the version its own `000-schema.sql`
establishes (`vantage.expectedSchemaVersion` in the chart's `_helpers.tpl`).
A completed Job whose log contains `schema version verified: <N>` (followed
by the history retention lines, the dictionary DDL and `schema applied`) is a
statement about the database, not just about the commands having run.

```bash
kubectl -n "$NS" get jobs    # the schema Job is named vantage-schema-<checksum>
kubectl -n "$NS" logs job/<the-schema-job-name> | grep -E 'schema version verified|schema applied'
```

Listing the tables cannot make that check. On a database stuck one version
behind, every table already exists (the versions differ by columns, not by
table names), so `system.tables` looks perfect while the daemons are about
to refuse to start. Read the version, not just the names:

```bash
kubectl -n "$NS" exec ch-probe -- clickhouse-client --host "$CH_SVC" \
  --user vantage --password "$CHPASS" \
  --query "SELECT max(version) FROM vantage.schema_version"
# Must equal the number in the schema Job's "schema version verified: <N>"
# line above: the version deploy/clickhouse/schema.sql establishes, and the
# number sink.ExpectedSchemaVersion carries. The binaries refuse to start
# against anything else.

kubectl -n "$NS" exec ch-probe -- clickhouse-client --host "$CH_SVC" \
  --user vantage --password "$CHPASS" \
  --query "SELECT name, engine FROM system.tables WHERE database='vantage' ORDER BY name FORMAT TSV"
```

**Two of the names that come back are not tables.** The schema Job also
applies the AS holder name dictionary DDL (`job-schema.yaml` runs
`/dictionaries/*.sql` after `/schema/*.sql`, both unconditionally), so the
listing includes `asnames` and `asnames_meta`, each with engine
`Dictionary`. They are deliberately outside the `schema_version` sequence
(see `deploy/clickhouse/asnames.sql`'s header), so they neither bump nor are
gated by it, and their presence says nothing about whether they hold data.

When you are done, remove the probe pod:

```bash
kubectl -n "$NS" delete pod ch-probe
```

## AS holder names: the data is yours to place

The chart ships the dictionary *definitions* and applies them to any
database, this one included. It does not ship or mount the *data*: that
runs to megabytes, over the ConfigMap size limit, and the chart has no
ClickHouse pod to mount it into. Nothing in this directory places it
either.

Until you do, both dictionaries stay at `NOT_LOADED`, `GET /v1/asnames`
reports `meta.asnames_loaded: false`, and every screen renders bare AS numbers
and says so once: a documented degraded state, not a broken install.

To enable it, run `make fetch-asnames` from a checkout of this repository
and put the resulting `deploy/dev/asnames/` directory at
`/var/lib/clickhouse/user_files/asnames` inside the ClickHouse pod, the path
both `asnames.sql` and `asnames_meta.sql` read from. How is up to you: a
volume on the `ClickHouseCluster` CR, an init container, or `kubectl cp` for
a one-off. For a one-off copy, first confirm that
`/var/lib/clickhouse` is on the data volume (`kubectl exec "$CH_POD" -- df
/var/lib/clickhouse`); anything written outside a persistent volume is lost
when the pod restarts. See `deploy/helm/README.md`, "AS holder names
(optional)", for the full account, including when a
`SYSTEM RELOAD DICTIONARY` is needed.

## Findings to keep in mind on any cluster

- **Memory: don't leave the limit unset.** When
  `containerTemplate.resources` is unset, the operator (v0.0.7) defaults to
  a 512Mi request AND limit. That is too small to apply `000-schema.sql`:
  the merge of a compact part for a `ReplacingMergeTree` table with
  `LowCardinality` columns hit `MEMORY_LIMIT_EXCEEDED` against it (it
  needed about 502 MiB against a 460.80 MiB tracked ceiling).
  `clickhouse-values.yaml` sets a 4Gi limit and a 2Gi request. Size it for
  your own nodes, but set it.
- **ClickHouse is pinned to `26.8-alpine`** via the top-level `imageTag`,
  which applies to both the ClickHouse server and Keeper images (both tags
  exist on Docker Hub). The cluster chart's own default (its `Chart.yaml`
  `appVersion` is `"latest"`) would otherwise silently pull whatever is
  newest. The schema and every query in this repository are tested against
  the 26.8 LTS line, the same line `docker-compose.dev.yml`, CI and the
  chart's schema Job use. Do not run this schema against another version
  without re-verifying it: `CREATE TABLE ... AS` alone changed what it
  copies between two LTS lines (see the current-table header in
  `deploy/clickhouse/schema.sql`).
- The operator run described in this README (v0.0.7) ran ClickHouse 24.8;
  the move to 26.8 was verified by rendering this chart, not by a new live
  install. The operator documents no supported range of ClickHouse
  versions. It reads the server version with a probe Job and, from 25.12
  on, requires a `named-collections-key` in `spec.externalSecret`, which
  these values do not use (the operator generates its own).
- **Keeper cannot be disabled.** `ClickHouseCluster.spec.keeperClusterRef`
  is a required field in the v0.0.7 CRD, confirmed with `kubectl explain
  clickhousecluster.spec --recursive` and stated in the operator's own docs
  ("Every ClickHouse cluster must reference a KeeperCluster for
  coordination"). `clickhouse-values.yaml` runs Keeper at `replicas: 1`,
  the smallest value the CRD documents as supported. The schema's enum also
  permits `0`, meaning no Keeper pods at all, but that is untested.
