# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Report it privately through GitHub:
[**Report a vulnerability**](https://github.com/jp2195/vantage/security/advisories/new)
(the *Security* tab, then *Report a vulnerability*). Only the maintainer
can see the report, and the fix can be worked on in a private fork before
anything is disclosed.

Please include:

- which component is affected: collector, writer, API, UI, Helm chart or
  the dev stack
- the version or commit you tested against
- steps to reproduce, and what an attacker gains
- a BMP capture or request transcript if the issue is in parsing or the
  API. Strip anything from your own network you do not want to share.

You should get an acknowledgment within a week. vantage is maintained by
one person, so a fix may take longer than that, but you will be told what
is happening and when disclosure is planned. Credit goes in the advisory
unless you would rather not be named.

## Supported versions

vantage has not had a release yet. Fixes land on `main` only.

## What is in scope

Anything that lets a party beyond the ones a deployment trusts do more
than they should. The collector parses BMP from routers, and a malformed
or hostile BMP stream that crashes it, exhausts its memory or corrupts
stored data is in scope, as is anything that gets past the API's bearer
token or NATS's mutual TLS.

BMP itself has no authentication, so the collector trusts whatever reaches
its port. A deployment must keep that port reachable only by its routers:
the collector's `allowed_sources` (the Helm chart's
`collector.allowedSources`), a load balancer that is internal or restricted
by source range, or both. A way past `allowed_sources` or `trusted_proxies`
is in scope; a collector left open to the internet with an empty
`allowed_sources`, which it warns about at startup, is a configuration
choice. The same goes for running NATS in plaintext, which the chart
supports only as an explicit opt-out.

## What is not

**The dev stack is not a deployment.** `docker-compose.dev.yml` runs
Grafana with anonymous access against a ClickHouse datasource, so anyone
who can reach it can run arbitrary SQL. That is why Grafana, ClickHouse,
NATS, the writer and the API all bind to `127.0.0.1`. Only the two
collectors' BMP and metrics ports listen on every interface, because
routers have to reach them. Exposing the loopback-bound ports is a
configuration choice, not a vulnerability. The same goes for the published dev token
(`dev-token-not-a-secret`).
