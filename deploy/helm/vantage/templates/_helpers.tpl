{{- define "vantage.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vantage.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "vantage.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "vantage.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "vantage.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vantage.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The ClickHouse host the daemons dial. Always external: this chart does not
deploy a database.

It used to branch on clickhouse.enabled, rendering a chart-owned Service
name when a bundled single-node StatefulSet was enabled by default. That
StatefulSet is gone -- standing up a datastore is not vantage's job, and an
operator-managed ClickHouse is the operator's own, so it belongs beside the
chart (deploy/clickhouse/k8s/) rather than inside it. The branch is gone
with it, leaving the message that was already here on the other side.
*/}}
{{- define "vantage.clickhouseHost" -}}
{{- required "clickhouse.externalHost is required: this chart does not deploy ClickHouse. Point it at your own -- see deploy/clickhouse/k8s/ for the operator-managed path." .Values.clickhouse.externalHost -}}
{{- end -}}

{{/*
The ClickHouse database, which is always "vantage". schema.sql, every
migration and every query in the writer and the API name it explicitly
(vantage.route_unicast, ...), so a different database in the DSN or the
schema Job would not move anything anywhere else -- it would only fail at
runtime. The clickhouse.database value that used to pretend otherwise is
gone; a values file that still sets it to anything but "vantage" fails here
instead of being silently ignored.
*/}}
{{- define "vantage.clickhouseDatabase" -}}
{{- with .Values.clickhouse.database -}}
{{- if ne (toString .) "vantage" -}}
{{- fail (printf "clickhouse.database is %q, but vantage's schema and queries name the `vantage` database explicitly, so no other value can work. Remove clickhouse.database from your values; the chart always uses `vantage`." (toString .)) -}}
{{- end -}}
{{- end -}}
vantage
{{- end -}}

{{/*
Where the ClickHouse password is projected into the writer and the API, and
the file the daemons' clickhouse_password_file names. One definition, so the
mount and the config cannot disagree.
*/}}
{{- define "vantage.clickhousePasswordDir" -}}/etc/clickhouse-auth{{- end -}}

{{/*
The Secret holding the ClickHouse credentials: the operator's existingSecret,
or the one secret-clickhouse.yaml renders from clickhouse.auth.
*/}}
{{- define "vantage.clickhouseSecret" -}}
{{- .Values.clickhouse.existingSecret | default (printf "%s-clickhouse" (include "vantage.fullname" .)) -}}
{{- end -}}

{{/*
A daemon's log_level and log_format lines: the component's own logLevel /
logFormat when set, else the top-level ones. Validated here because each
daemon refuses to start on any other value, and a render error names the
value where a CrashLoop does not. Call with (dict "root" $ "c" .Values.<component> "name" "<component>").
*/}}
{{- define "vantage.logConfig" -}}
{{- $level := .c.logLevel | default .root.Values.logLevel | toString -}}
{{- $format := .c.logFormat | default .root.Values.logFormat | toString -}}
{{- if not (has $level (list "debug" "info" "warn" "error")) -}}
{{- fail (printf "%s log level %q is not one of debug, info, warn, error (set by %s.logLevel or the top-level logLevel)" .name $level .name) -}}
{{- end -}}
{{- if not (has $format (list "text" "json")) -}}
{{- fail (printf "%s log format %q is not one of text, json (set by %s.logFormat or the top-level logFormat)" .name $format .name) -}}
{{- end -}}
log_level: {{ $level | quote }}
log_format: {{ $format | quote }}
{{- end -}}

{{/*
The NATS URL. The bundled subchart names its service <release>-nats.
*/}}
{{- define "vantage.natsURL" -}}
{{- if .Values.nats.enabled -}}
{{- printf "nats://%s-nats:4222" .Release.Name -}}
{{- else -}}
{{- required "nats.externalURL is required when nats.enabled is false" .Values.nats.externalURL -}}
{{- end -}}
{{- end -}}

{{/*
Image reference. repository is a full path; tag falls back to appVersion.
*/}}
{{- define "vantage.image" -}}
{{- printf "%s:%s" .repo (default .defaultTag .tag) -}}
{{- end -}}

{{/*
Every value-driven input that changes the schema Job's PodSpec, hashed into
an 8-character suffix for the Job's name. A completed Job's spec.template is
immutable, so this list has to be exhaustive: an upgrade that changes any of
these inputs without changing the name fails with "field is immutable" --
found on a live upgrade that repointed clickhouse.externalHost and
clickhouse.existingSecret at an operator-managed database with the SQL (and
so the old, SQL-only checksum) unchanged.

files/dictionaries/*.sql (the AS holder name dictionaries) is in this hash
for the identical reason files/schema/*.sql is: job-schema.yaml applies both
from the same PodSpec now, so an edit to either -- a migration or a future
change to the dictionary DDL -- has to produce a new Job name or the edit
never actually reapplies; the old, completed Job would just be reused as a
no-op forever.

The two security-context helpers below are in it for the same reason even
though no value drives them: they are template text, and adding them to a
Job that an earlier chart version already completed under the same name
is exactly that immutable-field failure. Hashing them means any future edit
to either renames the Job too.

vantage.retentionScript is in it whole, which covers retention.days: a
changed retention must run a new Job, or the old one is reused and the TTL
never moves.

Deliberately excludes anything that changes on every render (Release.
Revision, timestamps): the Job should be recreated when its spec genuinely
changes, and left alone otherwise.
*/}}
{{- define "vantage.schemaJobChecksum" -}}
{{- $secretName := .Values.clickhouse.existingSecret | default (printf "%s-clickhouse" (include "vantage.fullname" .)) -}}
{{- printf "%s|%s|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%s|%s"
    (include "vantage.schemaJobPodSecurity" .)
    (include "vantage.schemaJobContainerSecurity" .)
    ((.Files.Glob "files/schema/*.sql").AsConfig)
    ((.Files.Glob "files/dictionaries/*.sql").AsConfig)
    .Values.schemaJob.image.repository
    .Values.schemaJob.image.tag
    .Values.schemaJob.image.pullPolicy
    (toYaml .Values.schemaJob.resources)
    (include "vantage.clickhouseDatabase" .)
    (include "vantage.clickhouseHost" .)
    (.Values.clickhouse.externalPort | int)
    $secretName
    (toYaml .Values.imagePullSecrets)
    (include "vantage.retentionScript" .)
    | sha256sum | trunc 8 -}}
{{- end -}}

{{/*
The schema Job's pod- and container-level hardening, the same non-root
context the daemons run under. Helpers rather than inline YAML so that
vantage.schemaJobChecksum can hash them.

clickhouse-client needs no privilege and no writable root: run as uid 65532
with a read-only root, all capabilities dropped and no-new-privileges, this
image's client connected, created a database, applied a file with
--multiquery from stdin and dropped the database again. The render-config
init containers elsewhere in this chart already run the image as 65532.
*/}}
{{- define "vantage.schemaJobPodSecurity" -}}
automountServiceAccountToken: false
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{- define "vantage.schemaJobContainerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
{{- end -}}

{{/*
The schema version this chart's own DDL establishes, read out of
files/schema/000-schema.sql's version INSERT rather than restated as a value
anyone has to remember to bump. job-schema.yaml checks the database against
it after applying everything, which is what turns "the Job completed" into
"the database is actually on the version this chart ships".

`required` rather than a silent empty string: if the INSERT is ever reworded
so this regex stops matching, the render fails loudly here. An empty
expectation would otherwise compare unequal to every real version and fail
every deploy with a confusing message, or -- worse, if the comparison were
written the other way -- pass vacuously.
*/}}
{{- define "vantage.expectedSchemaVersion" -}}
{{- $ddl := .Files.Get "files/schema/000-schema.sql" -}}
{{- $found := regexFind "[0-9]+" (regexFind "SELECT [0-9]+ WHERE" $ddl) -}}
{{- required "files/schema/000-schema.sql carries no `SELECT <version> WHERE` row, so the schema Job cannot know what version it should reach. Did the version INSERT get reworded? Run `make sync-helm-schema` if the chart copy is merely stale." $found -}}
{{- end -}}

{{/*
retention.days, validated: an integer, at least 1. A values file and --set
both reach here as numbers, so a string ("30" through --set-string, or
"90d") is refused rather than coerced, and so is a fraction, which
ClickHouse would truncate to a different retention than the one asked for.
*/}}
{{- define "vantage.retentionDays" -}}
{{- $d := .Values.retention.days -}}
{{- if not (or (kindIs "int64" $d) (kindIs "int" $d) (kindIs "float64" $d)) -}}
{{- fail (printf "retention.days is %v, not a number: set a whole number of days, at least 1" $d) -}}
{{- end -}}
{{- if or (lt (float64 $d) 1.0) (ne (float64 $d) (float64 (int64 $d))) -}}
{{- fail (printf "retention.days is %v: set a whole number of days, at least 1" $d) -}}
{{- end -}}
{{- int64 $d -}}
{{- end -}}

{{/*
The schema Job's retention step: each history table's TTL set to
retention.days. The table list is the ten tables whose TTL is on
ts_collector in files/schema/000-schema.sql; ci.yml's helm job derives the
same list from that file and fails if the two differ. The current-state
tables are not in it: they have no TTL, and current state does not expire.

Unrolled per table, rather than a shell loop over a variable, so each
rendered statement is exactly what runs and a render assertion can read it.
The markers are how sink's TestSchemaJobRetentionStepIsIdempotent cuts
this step out of the rendered Job to run it against a scratch database.
*/}}
{{- define "vantage.retentionScript" -}}
{{- $days := include "vantage.retentionDays" . -}}
# BEGIN history retention
# materialize_ttl_after_modify = 0 leaves existing parts as they are: the
# new TTL applies as ClickHouse merges them, not by rewriting the archive
# now. A table already at this retention is skipped, so a rerun with the
# same value issues no ALTER. system.tables spells the TTL
# toIntervalDay(N), not the DDL's INTERVAL N DAY.
retention_changed=0
set_retention() {
  engine=$(clickhouse-client --host "$CH_HOST" --port "$CH_PORT" --user "$CH_USER" \
    --password "$CH_PASS" \
    --query "SELECT engine_full FROM system.tables WHERE database = '$CH_DB' AND name = '$1'")
  case "$engine" in
    *"TTL toDateTime(ts_collector) + toIntervalDay({{ $days }})"*)
      echo "retention: $CH_DB.$1 already keeps {{ $days }} days"
      return 0
      ;;
  esac
  echo "retention: set $CH_DB.$1 to {{ $days }} days"
  clickhouse-client --host "$CH_HOST" --port "$CH_PORT" --user "$CH_USER" \
    --password "$CH_PASS" --query "$2"
  retention_changed=$((retention_changed + 1))
}
{{- range list "route_unicast" "route_vpn" "route_evpn" "ls_events" "ls_nodes" "ls_links" "ls_prefixes" "peer_events" "eor_events" "stats_events" }}
set_retention {{ . }} "ALTER TABLE $CH_DB.{{ . }} MODIFY TTL toDateTime(ts_collector) + INTERVAL {{ $days }} DAY SETTINGS materialize_ttl_after_modify = 0"
{{- end }}
echo "history retention: {{ $days }} days; $retention_changed of 10 tables changed"
# END history retention
{{- end -}}

{{/*
Fails the render if api.tokens is empty. api/config.go's validate() refuses
to start vantage-api with zero configured tokens -- an unauthenticated read
API over the whole database -- so this chart must guarantee at least one is
rendered rather than deploying a Deployment that only discovers the problem
by crash-looping.
*/}}
{{- define "vantage.requireAPITokens" -}}
{{- if not .Values.api.tokens -}}
{{- fail "api.tokens must list at least one entry: vantage-api refuses to start with zero tokens" -}}
{{- end -}}
{{- end -}}

{{/*
Validates that an API token's name is safe to use as the suffix of a shell
variable name. deployment-api.yaml's init container reads each token's value
out of $TOKEN_<NAME UPPERCASED> inside a /bin/sh -c script, and a POSIX shell
variable name is restricted to [A-Za-z_][A-Za-z0-9_]*. A name with a hyphen
or a dot (e.g. "lab-2", "lab.eu") does not fail loudly there: $TOKEN_LAB-2
expands "$TOKEN_LAB" and leaves a literal "-2" behind, silently writing the
wrong -- and truncated -- token into the rendered config instead of refusing
to render. Call as: {{ include "vantage.validateAPITokenName" .name }}
*/}}
{{- define "vantage.validateAPITokenName" -}}
{{- if not (regexMatch "^[A-Za-z_][A-Za-z0-9_]*$" .) -}}
{{- fail (printf "api.tokens: name %q is not usable as a shell variable suffix (must match ^[A-Za-z_][A-Za-z0-9_]*$); deployment-api.yaml renders it as $TOKEN_%s in the init container's config-assembly script" . (upper .)) -}}
{{- end -}}
{{- end -}}

{{/*
Fails the render if two API token names collide once upper-cased.
deployment-api.yaml derives each token's shell env var name with
`.name | upper` ($TOKEN_<NAME>), and both vantage.validateAPITokenName
above and api/config.go's own duplicate-name check are case-sensitive, so
"lab" and "Lab" pass every existing guard individually and then both
render as $TOKEN_LAB -- one silently overwriting the other in the init
container's environment instead of failing loudly. Call once with the
whole token list: {{ include "vantage.requireUniqueAPITokenNames"
.Values.api.tokens }}
*/}}
{{- define "vantage.requireUniqueAPITokenNames" -}}
{{- $seen := dict -}}
{{- range . -}}
{{- $upper := .name | upper -}}
{{- if hasKey $seen $upper -}}
{{- fail (printf "api.tokens: %q and %q collide once upper-cased to the shared shell variable $TOKEN_%s; deployment-api.yaml would have one silently overwrite the other" (index $seen $upper) .name $upper) -}}
{{- else -}}
{{- $_ := set $seen $upper .name -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Fails the render if collector.proxyProtocol is anything but "off" or
"required". collector/config.go's validate() rejects every other value, so a
chart that rendered one would produce a collector that CrashLoops instead of
a render that fails.

The boolean case is called out on its own because it is a trap, not a typo.
Helm parses values files with YAML 1.1 semantics, where off/no/on/yes are
booleans: an operator's own values file carrying an unquoted
`proxyProtocol: off` arrives here as false and renders `proxy_protocol:
"false"`. That bites hardest on the rollback path, where someone mid-incident
turning the feature back off gets a collector that will not start at all --
worse than the state they were rolling back from. (`--set
collector.proxyProtocol=off` is unaffected; --set values are always strings.)
*/}}
{{- define "vantage.validateProxyProtocol" -}}
{{- $v := .Values.collector.proxyProtocol -}}
{{- if kindIs "bool" $v -}}
{{- fail (printf "collector.proxyProtocol is the boolean %v, not a string: YAML 1.1 reads an unquoted off/no/on/yes as a boolean, so quote it -- proxyProtocol: \"off\" or proxyProtocol: \"required\"" $v) -}}
{{- end -}}
{{- if not (has $v (list "off" "required")) -}}
{{- fail (printf "collector.proxyProtocol: %q is neither \"off\" nor \"required\"; vantage-collector refuses to start on any other value" (toString $v)) -}}
{{- end -}}
{{- if and .Values.collector.trustedProxies (eq $v "off") -}}
{{- fail "collector.trustedProxies is set but collector.proxyProtocol is \"off\": trusted proxies only mean something when a PROXY header is required, and vantage-collector refuses to start on the combination. Set proxyProtocol: \"required\", or clear trustedProxies and use allowedSources." -}}
{{- end -}}
{{- end -}}

{{/*
The BMP Service's loadBalancerSourceRanges: the explicit value if set, else
the addresses that open the TCP connection in the configured mode -- the
routers themselves with proxyProtocol "off" (collector.allowedSources), the
proxies with "required" (collector.trustedProxies). Emits a YAML list, or
nothing when the result is empty.
*/}}
{{- define "vantage.bmpSourceRanges" -}}
{{- $r := .Values.collector.service.loadBalancerSourceRanges -}}
{{- if not $r -}}
{{- if eq .Values.collector.proxyProtocol "required" -}}
{{- $r = .Values.collector.trustedProxies -}}
{{- else -}}
{{- $r = .Values.collector.allowedSources -}}
{{- end -}}
{{- end -}}
{{- with $r -}}
{{- toYaml . -}}
{{- end -}}
{{- end -}}


{{/*
Where the client certificate Secret is projected into the collector and the
writer. One definition, so the mount path and the rendered nats_tls block
cannot disagree -- a config naming a path nothing is mounted at is a daemon
that fails at startup on a missing file.
*/}}
{{- define "vantage.natsTLSDir" -}}/etc/nats-tls{{- end -}}

{{/*
The Secret holding a client's certificate. With an issuerRef the chart issues
it and names it; without one the operator names a Secret they created with
deploy/nats-tls/gen-certs.sh.

Both run vantage.validateNatsTLS first. Helm renders templates in reverse
name order and stops at the first failure, so statefulset-collector.yaml,
which calls these, fails before certificate-nats.yaml ever runs the
validation itself -- and a default install, with TLS on and nothing
configured, would get the bare `required` below naming one Secret instead
of the message that says how to supply certificates or opt out.
*/}}
{{- define "vantage.natsCollectorSecret" -}}
{{- include "vantage.validateNatsTLS" . -}}
{{- if .Values.nats.tls.issuerRef.name -}}
{{- printf "%s-nats-collector-tls" (include "vantage.fullname" .) -}}
{{- else -}}
{{- required "nats.tls.collectorSecret is required when nats.tls.enabled is true and no nats.tls.issuerRef is set. Create the Secrets with deploy/nats-tls/gen-certs.sh, or set nats.tls.issuerRef to have cert-manager issue them." .Values.nats.tls.collectorSecret -}}
{{- end -}}
{{- end -}}

{{- define "vantage.natsWriterSecret" -}}
{{- include "vantage.validateNatsTLS" . -}}
{{- if .Values.nats.tls.issuerRef.name -}}
{{- printf "%s-nats-writer-tls" (include "vantage.fullname" .) -}}
{{- else -}}
{{- required "nats.tls.writerSecret is required when nats.tls.enabled is true and no nats.tls.issuerRef is set. Create the Secrets with deploy/nats-tls/gen-certs.sh, or set nats.tls.issuerRef to have cert-manager issue them." .Values.nats.tls.writerSecret -}}
{{- end -}}
{{- end -}}

{{/*
Refuse to render a half-enabled NATS TLS configuration. Both directions of
disagreement produce a cluster that looks deployed and moves no data, and
both would surface a long way from the value that caused them.

The first check is the default install. TLS is on by default and the chart
creates no keys, so an install that has configured no certificate source
lands here, and the message has to say how to supply one or how to opt out
-- the per-Secret `required`s in the helpers above only name one missing
value, which reads like a typo rather than a decision still to make.
*/}}
{{- define "vantage.validateNatsTLS" -}}
{{- if .Values.nats.tls.enabled -}}
{{- if not (or .Values.nats.tls.issuerRef .Values.nats.tls.collectorSecret .Values.nats.tls.writerSecret) -}}
{{- fail (printf `NATS mutual TLS is on by default (nats.tls.enabled: true) and no certificates are configured. This chart never generates private keys. Choose one:

  1. Create them with deploy/nats-tls/gen-certs.sh:
       deploy/nats-tls/gen-certs.sh --release %[1]s --namespace %[2]s
       kubectl apply -n %[2]s -f nats-tls/secrets.yaml
     then set nats.tls.collectorSecret=%[1]s-vantage-nats-collector-tls and nats.tls.writerSecret=%[1]s-vantage-nats-writer-tls, plus the server-side values the script prints (nats.tlsCA.secretName, nats.config.nats.tls.secretName and nats.config.cluster.tls.secretName, all %[1]s-nats-server-tls).
  2. Have cert-manager issue them: set nats.tls.issuerRef.name (and kind) to an Issuer or ClusterIssuer you run.
  3. Run NATS in plaintext on purpose: set nats.tls.enabled=false, and with the bundled NATS also nats.tlsCA.enabled=false, nats.config.nats.tls.enabled=false and nats.config.cluster.tls.enabled=false.

See deploy/helm/README.md, "NATS TLS".` .Release.Name .Release.Namespace) -}}
{{- end -}}
{{- if and .Values.nats.tls.issuerRef (not .Values.nats.tls.issuerRef.name) -}}
{{- fail "nats.tls.issuerRef is set but nats.tls.issuerRef.name is empty. Without a name there is no issuer to ask, so the chart would quietly fall back to the bring-your-own path and then complain that no issuerRef is set -- which sends you looking at the value you did in fact set. Set nats.tls.issuerRef.name, or remove nats.tls.issuerRef entirely and supply the Secrets with deploy/nats-tls/gen-certs.sh." -}}
{{- end -}}
{{- if and .Values.nats.tls.issuerRef.name (or .Values.nats.tls.collectorSecret .Values.nats.tls.writerSecret) -}}
{{- fail "nats.tls.issuerRef.name and nats.tls.collectorSecret/writerSecret are both set, and they are two different issuance paths. The chart would take the issuerRef one and mount a Secret cert-manager issues, silently ignoring the Secret you named -- so the certificate the pods present would not be the one you created. Set one or the other: an issuerRef to have cert-manager issue the certificates, or the two Secret names to mount Secrets from deploy/nats-tls/gen-certs.sh." -}}
{{- end -}}
{{- end -}}
{{- if and .Values.nats.enabled .Values.nats.tls.enabled -}}
{{- if not .Values.nats.config.nats.tls.enabled -}}
{{- fail "nats.tls.enabled is true but nats.config.nats.tls.enabled is false: the daemons would demand TLS from a NATS server that offers none, and every connection would fail at startup." -}}
{{- end -}}
{{- if not .Values.nats.tlsCA.enabled -}}
{{- fail "nats.tls.enabled is true but nats.tlsCA.enabled is false: the NATS server would mount no CA bundle, so verify:true has nothing to check client certificates against and the server refuses to start." -}}
{{- end -}}
{{- if ne .Values.nats.tlsCA.secretName .Values.nats.config.nats.tls.secretName -}}
{{- fail (printf "nats.tlsCA.secretName (%s) and nats.config.nats.tls.secretName (%s) must name the same Secret -- the one holding the NATS server's own certificate." .Values.nats.tlsCA.secretName .Values.nats.config.nats.tls.secretName) -}}
{{- end -}}
{{- /*
Cluster routes. Checked only with the cluster on: with one server the
subchart renders no cluster block and mounts no route Secret, so these
values are inert and the single-server opt-out stays one value.
*/ -}}
{{- if .Values.nats.config.cluster.enabled -}}
{{- if not .Values.nats.config.cluster.tls.enabled -}}
{{- fail "nats.tls.enabled is true and the bundled NATS is clustered, but nats.config.cluster.tls.enabled is false: client connections would be mutual TLS while the routes between the NATS servers, which carry every message and every stream replica, ran in plaintext and accepted any peer. Set nats.config.cluster.tls.enabled=true (the default), or run one server with nats.config.cluster.enabled=false." -}}
{{- end -}}
{{- if ne (toString .Values.nats.config.cluster.tls.secretName) .Values.nats.config.nats.tls.secretName -}}
{{- fail (printf "nats.config.cluster.tls.secretName (%s) and nats.config.nats.tls.secretName (%s) must name the same Secret. Routes reuse the server certificate: it is the one gen-certs.sh and the cert-manager Certificate issue with the route names and clientAuth, and no other Secret is created for them." (toString .Values.nats.config.cluster.tls.secretName) .Values.nats.config.nats.tls.secretName) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and .Values.nats.enabled (not .Values.nats.tls.enabled) (or .Values.nats.config.nats.tls.enabled .Values.nats.tlsCA.enabled (and .Values.nats.config.cluster.enabled .Values.nats.config.cluster.tls.enabled)) -}}
{{- fail "nats.tls.enabled is false but the bundled NATS server is still configured for TLS (nats.config.nats.tls.enabled, nats.tlsCA.enabled or, with the cluster on, nats.config.cluster.tls.enabled is true): the server would require client certificates the collector and writer do not present, or mount a Secret that does not exist. Running NATS in plaintext takes all four: nats.tls.enabled=false, nats.tlsCA.enabled=false, nats.config.nats.tls.enabled=false and nats.config.cluster.tls.enabled=false." -}}
{{- end -}}
{{- end -}}

{{/*
How many servers the bundled NATS runs: config.cluster.replicas when the
subchart's cluster is on, and 1 when it is off -- the same branch the
subchart's own stateful-set.yaml takes. 0 with external NATS, whose size this
chart cannot see.
*/}}
{{- define "vantage.natsServers" -}}
{{- if not .Values.nats.enabled -}}
0
{{- else if .Values.nats.config.cluster.enabled -}}
{{- .Values.nats.config.cluster.replicas | default 3 | int -}}
{{- else -}}
1
{{- end -}}
{{- end -}}

{{/*
The streams.replicas the collector config renders, or empty for none.

An explicit collector.streams.replicas always wins. Unset (0), it follows the
bundled NATS: a clustered one gets 3 -- vantage.validateNatsCluster holds
the cluster at an odd size of at least 3, and three copies survive one
server's loss at any size, while more would only multiply the disk. A single
bundled server, or external NATS, renders nothing and keeps the collector's
own default of 1 -- external NATS on purpose, since the chart cannot see how
many servers it has and a guess of 3 against one server would stop the
collector at startup.

LS is not derived here. The collector's config loader defaults an unset
ls_replicas from replicas, so rendering replicas alone moves LS with it.
*/}}
{{- define "vantage.streamReplicas" -}}
{{- $explicit := .Values.collector.streams.replicas | default 0 | int -}}
{{- $servers := include "vantage.natsServers" . | int -}}
{{- if gt $explicit 0 -}}
{{- $explicit -}}
{{- else if gt $servers 1 -}}
3
{{- end -}}
{{- end -}}

{{/*
Refuse stream replica counts the bundled NATS cannot hold. JetStream rejects
a stream with more copies than there are servers, and the collector creates
its streams at startup, so the failure would otherwise be a collector that
never starts -- most likely right after someone applied the single-server
opt-out and kept a replicas: 3 from before. LS is checked as the collector
will compute it: lsReplicas when set, else the rendered replicas, else 1.
*/}}
{{- define "vantage.validateStreamReplicas" -}}
{{- $servers := include "vantage.natsServers" . | int -}}
{{- if gt $servers 0 -}}
{{- $r := include "vantage.streamReplicas" . | default "1" | int -}}
{{- $ls := .Values.collector.streams.lsReplicas | default 0 | int -}}
{{- if eq $ls 0 -}}{{- $ls = $r -}}{{- end -}}
{{- if gt $r $servers -}}
{{- fail (printf "collector.streams.replicas is %d, but the bundled NATS runs %d server(s), and JetStream refuses a stream with more copies than servers: the collector would fail to create its streams and never start. Lower collector.streams.replicas, or run more servers (nats.config.cluster.enabled with nats.config.cluster.replicas)." $r $servers) -}}
{{- end -}}
{{- if gt $ls $servers -}}
{{- fail (printf "collector.streams.lsReplicas is %d, but the bundled NATS runs %d server(s), and JetStream refuses a stream with more copies than servers: the collector would fail to create its LS stream and never start." $ls $servers) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Refuse a bundled NATS cluster that cannot survive losing a server, and a
spread rule that no longer covers the cluster's size.

A JetStream cluster keeps working while a majority of its servers are up.
With two, the majority is both: a two-member group needs both members for
quorum, so losing either stops every stream -- and the subchart's
PodDisruptionBudget (maxUnavailable 1) would admit exactly that drain. An
even count above that buys nothing over the odd count below it (four
tolerates one loss, like three). So the size is odd and at least 3.

minDomains on the hostname spread is a literal, because a subchart value
cannot be templated, so it cannot follow cluster.replicas by itself. With
more servers than minDomains, the rule stops guaranteeing each server its
own node once there are fewer nodes than servers. It is checked here, where
both values are readable.
*/}}
{{- define "vantage.validateNatsCluster" -}}
{{- if and .Values.nats.enabled .Values.nats.config.cluster.enabled -}}
{{- $n := .Values.nats.config.cluster.replicas | int -}}
{{- if or (lt $n 3) (eq (mod $n 2) 0) -}}
{{- fail (printf "nats.config.cluster.replicas is %d; a clustered NATS needs an odd number of servers, at least 3. JetStream keeps a stream available only while a majority of its servers are up: a two-member group needs both members for quorum, so losing either one stops ingest -- and the subchart's PodDisruptionBudget (maxUnavailable 1) would allow a drain that does exactly that. An even count tolerates no more failures than the odd count below it. Use 3 (or 5), or run one server with nats.config.cluster.enabled=false." $n) -}}
{{- end -}}
{{- $spread := dig "podTemplate" "topologySpreadConstraints" "kubernetes.io/hostname" dict (.Values.nats | toJson | fromJson) -}}
{{- with $spread.minDomains -}}
{{- if gt $n (int .) -}}
{{- fail (printf "nats.config.cluster.replicas is %d but the hostname spread's minDomains (nats.podTemplate.topologySpreadConstraints.\"kubernetes.io/hostname\".minDomains) is %d. Raise minDomains together with cluster.replicas: it is a literal the chart cannot derive, and below the server count it no longer keeps every server on its own node." $n (int .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
