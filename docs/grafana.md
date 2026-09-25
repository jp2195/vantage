# Grafana

vantage provisions thirteen Grafana dashboards from `deploy/grafana`. The
dev stack (`docker compose -f docker-compose.dev.yml up`) brings up a
Grafana with them and a ClickHouse datasource already wired in, described
below; see `docs/architecture.md` for what the pipeline behind these panels
does.

## Dashboards

Grafana comes up on <http://127.0.0.1:3000> (anonymous viewer; admin login
`admin`/`vantage`; set `VANTAGE_GRAFANA_PORT` to move it) with the ClickHouse
datasource and thirteen dashboards provisioned from `deploy/grafana`. That
anonymous access reaches a ClickHouse datasource directly, so anyone who
can reach the port can run arbitrary SQL against the archive — see
`README.md`'s Quickstart section for why every port in this stack stays on
loopback except the two collectors' BMP listeners, and do not move this one:

- **Fleet health** — routers, peer status, peer up/down over time, parse flags,
  plus writer lag and envelopes lost. Two panels separate losing the *view*
  from the *network* changing: a BMP session ending is the collector losing
  its connection, while a Peer Down is the router reporting a BGP session
  dropped, decoded from RFC 7854 §4.9.
- **Route churn** — advertisements vs withdrawals, changes by peer, most-changed
  prefixes. Unicast only: on `route_vpn` every observation arrives on a fresh BMP
  session, so a count taken there would measure session restarts, not churn.
- **EVPN churn** — MAC mobility and flap: which routes are withdrawn and
  re-advertised, and how long they stay away.
- **Parse anomalies** — which quirk fired, on which platform, in which table, and
  when it started.
- **Looking glass** and **L3VPN looking glass** — the covering route for one
  prefix, scoped to a live session rather than to the whole archive.
- **ASN view** and **Top L3VPN prefixes** — who originates what, and which VRFs
  carry the most.
- **L3VPN RIB browser** — what each router advertises inside each VRF: the MPLS
  label, the next hop, and which route target reaches it.
- **Link-state nodes**, **links**, **prefixes** and **topology** — the BGP-LS
  view, with the topology drawn as a node graph.

The route dashboards (the two looking glasses, Route churn, EVPN churn, ASN
view, Top L3VPN prefixes and the L3VPN RIB browser) are scoped by a `rib`
template variable — adj-RIB-in and adj-RIB-out are different questions, and
no route panel mixes them. Fleet health carries the variable too, but its
session, peer-event and stats-report panels do not read it. The link-state
dashboards and Parse anomalies have no `rib` variable and read every RIB
view.

Fleet health's two lag panels read from Prometheus rather than ClickHouse,
because a question about data that never arrived cannot be answered by
querying the store it never arrived in. The dev stack runs no Prometheus, so
those two panels show "No data" until you start one — see
`deploy/dev/prometheus.yml` for the one-liner.
