#!/usr/bin/env bash
# Generate the certificate material vantage's NATS transport needs.
#
# This exists because the Helm chart deliberately creates no private keys.
# Helm's own genCA/genSignedCert regenerate on every render -- two identical
# `helm template` runs produce different CAs -- so a chart that used them
# would mint a fresh PKI on any `helm upgrade`, including one that changed an
# unrelated value. The NATS StatefulSet and the two client workloads roll at
# different times, so mid-roll a collector holding the old CA cannot reach a
# server presenting the new certificate: dropped BMP sessions, from a value
# change that had nothing to do with TLS. The usual workaround, `lookup` to
# retain the existing Secret, returns empty under `helm template` and would
# make rendered output depend on live cluster state.
#
# So: you run this once, apply the Secrets, and the chart mounts them. It is
# also why `helm upgrade` can never rotate your PKI by accident.
#
# If you already run cert-manager, you do not need this at all -- set
# nats.tls.issuerRef and the chart issues the same three Secrets for you.
set -euo pipefail
# secrets.yaml embeds three private keys in the clear. Set the umask before
# anything is written, not just before the belt-and-suspenders chmod below,
# so nothing is ever briefly group- or world-readable on a permissive
# system umask.
umask 077

release=vantage
namespace=vantage
cluster_domain=cluster.local
days=825
out=./nats-tls
extra_sans=()

usage() {
	cat <<'USAGE'
Usage: gen-certs.sh [options]

  --release NAME      Helm release name (default: vantage). Must match the
                      release you install, because the NATS Service is named
                      <release>-nats and that name goes in the certificate.
  --namespace NAME    Namespace (default: vantage). Same reason.
  --cluster-domain D  The Kubernetes cluster domain (default: cluster.local).
                      Set it, and nats.config.cluster.routeURLs.k8sClusterDomain
                      to match, on a cluster whose domain differs: the NATS
                      cluster routes are dialed by FQDN.
  --days N            Certificate lifetime (default: 825, the maximum many
                      TLS stacks accept for a server certificate).
  --out DIR           Output directory (default: ./nats-tls).
  --extra-san SAN     Additional SAN on the SERVER certificate, in openssl
                      form ("DNS:host" or "IP:1.2.3.4"). Repeatable. Use it
                      for "IP:127.0.0.1" if you debug through
                      `kubectl port-forward`.

Writes ca.{crt,key}, server.{crt,key}, collector.{crt,key}, writer.{crt,key}
and secrets.yaml into DIR. Apply the Secrets with:

  kubectl apply -n NAMESPACE -f DIR/secrets.yaml

Keep ca.key. Losing it means re-issuing everything; leaking it means anyone
can mint a certificate your NATS will trust.
USAGE
}

while [ $# -gt 0 ]; do
	case "$1" in
	--release) release=$2; shift 2 ;;
	--namespace) namespace=$2; shift 2 ;;
	--cluster-domain) cluster_domain=$2; shift 2 ;;
	--days) days=$2; shift 2 ;;
	--out) out=$2; shift 2 ;;
	--extra-san) extra_sans+=("$2"); shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "gen-certs.sh: unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
done

command -v openssl >/dev/null || { echo "gen-certs.sh: openssl is required" >&2; exit 1; }
command -v base64 >/dev/null || { echo "gen-certs.sh: base64 is required" >&2; exit 1; }
command -v tr >/dev/null || { echo "gen-certs.sh: tr is required" >&2; exit 1; }

# Everything below is generated into a private scratch directory and moved
# into --out only once every file exists (the final `mv`, near the end).
# Without this, an interrupted re-run into an already-populated --out --
# Ctrl-C, a full disk, a dropped SSH session -- can rewrite ca.{crt,key} and
# die before the leaves are reissued, leaving collector.crt/writer.crt
# signed by a CA that no longer exists. That directory looks complete: only
# `openssl verify -CAfile ca.crt collector.crt` says otherwise, and nothing
# prompts anyone to run it. Generating into $work first means the previous
# $out is untouched by anything short of the final move, and mktemp -d's
# own 0700 keeps it private independent of the umask above.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# P-256 rather than RSA: faster handshakes, smaller certificates, and
# universally supported by anything that speaks TLS 1.2 or later. The NATS
# server and nats.go both accept it -- exercised by natstls/gencerts_test.go,
# which runs this script and connects a real client to a real server.
newkey() { openssl ecparam -name prime256v1 -genkey -noout -out "$1"; }

newkey "$work/ca.key"
openssl req -x509 -new -key "$work/ca.key" -sha256 -days "$days" \
	-subj "/CN=$release-nats-ca" -out "$work/ca.crt" \
	-addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
	-addext "keyUsage=critical,keyCertSign,cRLSign"

# issue <basename> <common-name> <extensions>
issue() {
	local base=$1 cn=$2 exts=$3
	newkey "$work/$base.key"
	openssl req -new -key "$work/$base.key" -subj "/CN=$cn" -out "$work/$base.csr"
	openssl x509 -req -in "$work/$base.csr" -CA "$work/ca.crt" -CAkey "$work/ca.key" \
		-CAcreateserial -days "$days" -sha256 \
		-extfile <(printf '%s\n' "$exts") -out "$work/$base.crt"
	rm -f "$work/$base.csr"
}

# <release>-nats is the Service the NATS subchart creates and the name the
# daemons dial. *.<release>-nats-headless.<ns>.svc.<domain> is the cluster
# routes: with nats.config.cluster.routeURLs.useFQDN (the chart's default)
# the subchart writes each route as
# <release>-nats-<i>.<release>-nats-headless.<ns>.svc.<domain>, and the
# server dialing a route verifies the other's certificate against that name.
# A wildcard, not per-pod names, because every server presents this one
# certificate, and a route learned by gossip is dialed by IP and verified
# against pod 0's route name whichever server answers. Without it a
# three-server NATS never forms a cluster while a single server looks fine.
server_san="DNS:$release-nats,DNS:$release-nats.$namespace.svc,DNS:$release-nats.$namespace.svc.$cluster_domain"
server_san="$server_san,DNS:*.$release-nats-headless.$namespace.svc.$cluster_domain"
for san in ${extra_sans+"${extra_sans[@]}"}; do
	server_san="$server_san,$san"
done

# serverAuth for clients and for the route a peer dials in; clientAuth for
# the route this server dials out, where it presents this same certificate
# to a peer that requires one (cluster.tls verify: true). Go's TLS stack
# refuses a client certificate without clientAuth, so without it every route
# handshake fails. It grants nothing new: only the NATS server holds this key.
issue server "$release-nats" "$(printf '%s\n' \
	"subjectAltName=$server_san" \
	"keyUsage=critical,digitalSignature,keyEncipherment" \
	"extendedKeyUsage=serverAuth,clientAuth")"

# Client certificates carry clientAuth ONLY. A client certificate that also
# carried serverAuth could be used to impersonate the broker to the other
# client, which is most of what mTLS was bought to prevent.
for svc in collector writer; do
	issue "$svc" "$release-vantage-$svc" "$(printf '%s\n' \
		"subjectAltName=DNS:$release-vantage-$svc" \
		"keyUsage=critical,digitalSignature,keyEncipherment" \
		"extendedKeyUsage=clientAuth")"
done

b64() { base64 < "$1" | tr -d '\n'; }

{
	for pair in "server:$release-nats-server-tls" \
		"collector:$release-vantage-nats-collector-tls" \
		"writer:$release-vantage-nats-writer-tls"; do
		base=${pair%%:*}
		name=${pair#*:}
		# Substitutions assigned here, not inlined in the heredoc below:
		# `set -e` does not fire on a failing command substitution nested
		# inside a heredoc, so a broken `b64` there would still exit 0 and
		# write a Secret with a blank or truncated field. In a plain
		# assignment it does fire.
		ca_b64=$(b64 "$work/ca.crt")
		crt_b64=$(b64 "$work/$base.crt")
		key_b64=$(b64 "$work/$base.key")
		cat <<YAML
---
apiVersion: v1
kind: Secret
metadata:
  name: $name
  namespace: $namespace
type: kubernetes.io/tls
data:
  ca.crt: $ca_b64
  tls.crt: $crt_b64
  tls.key: $key_b64
YAML
	done
} > "$work/secrets.yaml"

chmod 600 "$work"/*.key "$work/secrets.yaml"

# Every file exists in $work now. Move the finished set into place as the
# last step -- a rename on the same filesystem, and even where $work and
# $out differ (mv falls back to copy-then-remove), a stray complete $work
# next to an untouched $out is a strictly better failure mode than a
# half-rewritten $out.
mkdir -p "$out"
mv "$work"/* "$out/"

cat >&2 <<EOF
Wrote $out/{ca,server,collector,writer}.{crt,key} and $out/secrets.yaml

  kubectl apply -n $namespace -f $out/secrets.yaml

Then set, in your values:

  nats:
    # nats-box ships a context with the CA and no client keypair, and the
    # subchart's \`helm test\` job runs through it -- so a verify:true server
    # refuses both. Turn it off, or point
    # nats.natsBox.contexts.default.tls.secretName at a client Secret.
    natsBox:
      enabled: false
    tls:
      enabled: true
      collectorSecret: $release-vantage-nats-collector-tls
      writerSecret: $release-vantage-nats-writer-tls
    tlsCA:
      enabled: true
      secretName: $release-nats-server-tls
    config:
      nats:
        tls:
          enabled: true
          secretName: $release-nats-server-tls
      cluster:
        tls:
          enabled: true
          secretName: $release-nats-server-tls

Keep $out/ca.key. It is the only thing that can issue more of these.
EOF
