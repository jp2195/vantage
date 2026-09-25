# NATS TLS material

`gen-certs.sh` makes the certificate authority and the three leaf
certificates vantage's NATS transport needs: `server` (the NATS servers,
for client connections and for the cluster routes between them), and
`collector` and `writer` (the two clients). The Helm chart
deliberately does not generate these -- see the comment at the top of the
script for why -- so you run this once, outside Helm, and apply the result.

## Usage

```
deploy/nats-tls/gen-certs.sh --release myrelease --namespace myns
kubectl apply -n myns -f nats-tls/secrets.yaml
```

Requires OpenSSL 1.1.1 or newer (for `-addext`). On macOS the stock
`/usr/bin/openssl` is LibreSSL, which does not support that flag: install
`openssl` via Homebrew and make sure it is the one on `PATH` before running
this. `command -v openssl` only checks that something named `openssl`
exists, not that it is the right one, so the wrong one fails confusingly on
the first call rather than up front.

That writes three `kubernetes.io/tls` Secrets (`<release>-nats-server-tls`,
`<release>-vantage-nats-collector-tls`, `<release>-vantage-nats-writer-tls`)
and prints the values block that wires them into the chart. `--release` and
`--namespace` must match the Helm release you install, because the
certificate's names are built from them. Run `--help` for the full flag
list, including `--extra-san` for debugging through `kubectl port-forward`
and `--cluster-domain` for a cluster whose domain is not `cluster.local`.

## The server certificate also secures the cluster routes

The chart runs NATS as three clustered servers, and the routes between them
are mutual TLS on the server certificate. So that certificate carries:

- **`serverAuth` and `clientAuth`.** The server that dials a route presents
  the certificate as a client certificate, and the other end refuses one
  without `clientAuth`. The collector and writer certificates stay
  `clientAuth` only, so neither can pose as the server.
- **`*.<release>-nats-headless.<namespace>.svc.cluster.local`**, the name
  the routes are dialed by (`<release>-nats-<i>.<release>-nats-headless...`),
  alongside the `<release>-nats` Service names the daemons dial.

A server certificate from before the chart clustered NATS has the name but
not `clientAuth`, and the servers cannot form a cluster with it. Run the
script again and apply its output: it mints a new CA, so it replaces all
three Secrets.

Check a certificate with:

```
openssl x509 -in nats-tls/server.crt -noout -ext subjectAltName,extendedKeyUsage
```

## Keep `ca.key`

It is the only thing that can issue more client or server certificates
against this CA. Losing it means re-issuing everything and rolling every
workload; leaking it means anyone holding it can mint a certificate your
NATS server will trust.

The script writes its output under `nats-tls/` by default, inside this repo.
`.gitignore` excludes the keys, certificates, `.srl` files and
`secrets.yaml` it produces there -- do not remove those entries, and do not
commit this material under a different path either.

## cert-manager users

You do not need this script at all. Set `nats.tls.issuerRef` in your Helm
values and the chart requests the same three certificates from cert-manager
directly, including rotation.
