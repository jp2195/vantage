# Troubleshooting

Symptom-first, because that is how someone arrives here. Each section starts
from what you are looking at, not from a component name. For the metrics and
endpoints named below, see `docs/operating.md`; for what each piece does
internally, `docs/architecture.md`.

Every command below was run against a live dev stack
(`docker-compose.dev.yml`) while writing this page, with real output pasted
in, except where a value is shown as a placeholder (`<router-ip>`) rather
than a captured run. A command that needed a production Kubernetes cluster
this environment does not have is marked **untested** rather than presented
as verified.

## A peer will not come up

Start at the transport, then walk up to BGP/BMP framing.

**1. Is the collector's BMP port even reachable?**

```
$ nc -zv 127.0.0.1 11019
Connection to 127.0.0.1 11019 port [tcp/*] succeeded!
```

If this fails, nothing past this point matters yet — it's a network path,
firewall, or (in Kubernetes) Service/ingress problem, not a vantage one. See
`deploy/helm/README.md`'s "BMP ingress and the announcing node" section for
the Kubernetes-specific version of this: a collector pod can be `Running`
with clean logs while its Service VIP answers nothing, if the node
announcing the VIP is not the node the pod landed on.

**2. Does the collector see the TCP connection at all?**

Watch `vantage_collector_sessions_active` on the collector's metrics port while the
router (or your test) connects:

```
$ curl -s 127.0.0.1:9469/metrics | grep vantage_collector_sessions_active
vantage_collector_sessions_active 3
```

and read its logs. A session that opens and is immediately dropped for
protocol reasons looks like this, from opening a raw TCP connection to the
dev collector and writing five bytes that are not a valid BMP header (shown
in the default `log_format: text`; `json` carries the same fields):

```
time=2026-09-22T16:49:35.000Z level=INFO msg="bmp session open" router=172.22.0.1 session_id=1790095775526787708
time=2026-09-22T16:49:35.000Z level=WARN msg="bmp read error; closing connection" router=172.22.0.1 session_id=1790095775526787708 err="bmp: unsupported version: 255"
time=2026-09-22T16:49:35.000Z level=INFO msg="bmp session closed" router=172.22.0.1 session_id=1790095775526787708 sys_name=""
```

A session that never opens at all — no `bmp session open` line for the
router's address — means the connection isn't reaching this collector: wrong
target IP/port on the router, a front-end swallowing it, or (see below) a
`proxy_protocol` mismatch closing it before the collector logs anything
useful.

**3. `proxy_protocol: required` and the front-end have to change together.**
If the collector sits behind a load balancer or proxy configured to send a
PROXY protocol header, the collector's own `proxy_protocol: required` config
setting (see `docs/architecture.md`'s `vantage-collector` section) and that
front-end's header-sending config are one deployment change, not two —
turning on `required` without a front-end actually emitting headers closes
every directly connected router's session immediately, and the reverse
(front-end emits headers, collector still in default mode) has the collector
reading header bytes as malformed BMP. Both failures are loud (a session
that opens and instantly closes, per point 2 above) rather than silent. This
scenario needs a front-end this dev stack does not run, so it is **untested
here**; see `docs/architecture.md`'s `vantage-collector` section and
`deploy/helm/README.md`'s "BMP ingress and the announcing node" for the full
behavior, including the `vantage_collector_proxy_header_total{result}` counter that
distinguishes a real header mismatch from an ordinary health-check probe.

**4. A session that closes after a `publish failed` line is the collector
protecting the archive.** When the collector cannot hand a session's events
to JetStream, it closes that session so the router reconnects and re-sends,
and it logs `publish failed; closing the BMP session so the router
reconnects and re-sends` with the error. `vantage_collector_sessions_aborted_total`
counts these. A connection closed with no session at all while NATS is down
is counted as
`vantage_collector_connections_rejected_total{reason="nats_disconnected"}`.
Either way the fix is on the NATS side: the server is unreachable, or the
stream for that subject is missing or full. Restarting the collector
recreates a missing stream with the partitions, replica count and limits in
its own config. To do it by hand, run `vantage streams init` with those same
values: on the Helm chart's default that is `-replicas 3`, plus `-nats-ca`,
`-nats-cert` and `-nats-key` because its NATS requires mutual TLS (see
"Provisioning streams directly" in `docs/cli.md`). `streams init` leaves an
existing stream's replica count alone unless you pass `-replicas`, and
refuses to lower it without `-allow-lower-replicas`. A session that flaps on every reconnect is
usually a missing stream. A missing stream closes the session about 20s
after its first event for that stream rather than at once, preceded by
`publish failed in flight; re-sent it with the same msg-id` warnings with
`err` naming `no response from stream`: that is also the answer from a
stream electing a leader, which the collector waits out. When a transient
failure is retried until its bounds run out, the `publish failed` line's
`err` says which bound: `backoff retry window passed` (20s of `no response
from stream`, which is almost always a missing stream), `retry deadline
passed` (60s since the event was first sent, typically NATS unreachable for
that long, or a stream that keeps timing out), or `retry attempts
exhausted` (16 attempts; in practice a stream leader that keeps answering
`duplicate message id is in process` for a publish it staged and then
dropped, which nats-server 2.11 can do for up to 2 minutes after a leader
change). A `publish failed` whose `err` says `too many publishes already
retrying` is a different case: 4096 publishes to the same stream were
already being retried, and none finished within 10s (after the first such
failure, later publishes to that stream fail at once until one does). The
stream is unreachable, not the message wrong; see the per-stream retry
budget under "Publish failures" in `docs/operating.md`. See `docs/operating.md`, "Publish failures, the
second way this pipeline loses data", including why IOS-XR does not fully
recover what was lost.

**5. Router identity conflicts log but don't block the session.** If
`routers:` in the collector config names a `vendor` that disagrees with what
the router's own BMP Initiation banner implies, the config wins and the
collector logs `router identity conflict` at warn — the session still comes
up. If a peer looks like it "won't come up" but the collector's logs show it
open under an unexpected router identity, this is why; see
`docs/architecture.md`'s "Why identity is configured rather than sniffed."

## No routes are appearing

**1. Check the peer's actual state**, not just "is it in the table":

```
$ curl -s -H "Authorization: Bearer dev-token-not-a-secret" \
    "http://127.0.0.1:9473/v1/peers?router=<router-ip>" | python3 -m json.tool
```

`state` is one of `up`, `down`, `view_lost`, `stale` (see the next section
for what distinguishes the last three), or `unspecified`, a peer whose newest
event is of a kind the writer did not recognize. Only `up` and `stale` peers
contribute routes to a current-state answer.

**2. A peer still sending its initial dump answers with a warning, not
silence.** A live example from this stack, a peer whose `lu4` family is
still dumping:

```json
"dump_states": {"lu4": "dumping"}
```

```json
"warnings": [{"code": "session_dumping", "message": "at least one contributing peer is still sending its initial RIB dump, so this answer is a partial view of that peer's routes rather than its full table"}]
```

That is expected during a dump, not a bug — the answer is telling you it is
partial. If it stays `dumping` indefinitely, that is a real symptom: check
whether `vantage_collector_bmp_messages_total{type="route_monitoring"}` on the
collector is still climbing for that session.

**3. A RIB walk is pinned to the session that is current right now — an
older session's archived routes are not merged in, even though they're
still sitting in the archive.** This is current behavior, not a
hypothetical. For example, a router `10.0.103.62` whose peer `10.2.0.2`
reports `state: up` with `routes: 0` answers a walk with nothing:

```
$ curl -s -H "Authorization: Bearer dev-token-not-a-secret" \
    "http://127.0.0.1:9473/v1/rib/unicast?router=10.0.103.62&peer=10.2.0.2&limit=2"
{"data":[],"meta":{"next_cursor":null,"warnings":[{"code":"paginated_smear","message":"a paginated walk is a smear over a live table, not a snapshot: the session is pinned, but rows may be added or withdrawn between pages"}],"total_matched":null}}
```

ClickHouse can still hold dozens of rows for that same (router, peer), all
under earlier `session_id` values than the one `/v1/peers` reports as
current. Those rows are real
history and still queryable if you go looking for them directly, but they
are correctly excluded from a *current* RIB answer, because a walk answers
"what does this session show," not "what has ever been seen." If a peer is
`up` with `routes: 0` and no `session_dumping` warning, this — a session
that has archived nothing yet — is the first thing to check, ahead of
assuming something is broken.

**4. Confirm messages are arriving at all**, one level below the archive:

```
$ curl -s 127.0.0.1:9469/metrics | grep 'vantage_collector_bmp_messages_total{type="route_monitoring"}'
vantage_collector_bmp_messages_total{type="route_monitoring"} 886
```

If this counter is flat while you expect churn, the problem is upstream of
vantage entirely (the router isn't sending updates, or its BMP session to
this collector specifically isn't the one carrying that traffic).

**5. A router monitored by more than one collector needs `collector=` on
a RIB walk, and its absence is a 400, not a zero-row answer** — see its own
section below; if you're troubleshooting "no routes" and got an error
instead of an empty table, that's a different, better-signposted problem
than this section.

## Writer lag is climbing

**1. Read the three lag metrics together**, not `consumer_lag` alone — see
`docs/operating.md`'s "Metrics worth alerting on" for the full table:

```
$ curl -s 127.0.0.1:9472/metrics | grep -E 'vantage_sink_(consumer_lag|envelopes_lost_total|lag_sample_errors_total)'
vantage_sink_consumer_lag{stream="LS"} 0
vantage_sink_consumer_lag{stream="PEER"} 0
vantage_sink_consumer_lag{stream="ROUTES"} 0
vantage_sink_consumer_lag{stream="STATS"} 0
vantage_sink_envelopes_lost_total{stream="LS"} 0
vantage_sink_envelopes_lost_total{stream="PEER"} 0
vantage_sink_envelopes_lost_total{stream="ROUTES"} 0
vantage_sink_envelopes_lost_total{stream="STATS"} 0
```

If `lag_sample_errors_total` is climbing, the `consumer_lag` gauge next to
it is stale, not reassuring — do not read a flat lag graph as "caught up" in
that state.

**2. Look for repeated insert failures.** A single failed batch is normal
and recovers (the writer retries with backoff and never acks the loss); a
*sustained* rate is the symptom. This is the real shape of an isolated,
recovered failure from this stack's own writer log:

```
time=2026-09-18T22:57:45.000Z level=ERROR msg="insert failed, not acking" stream=STATS durable=vantage-writer-stats rows=1 err="clickhouse: prepare stats_events: query processing: failed to read first block packet from 172.22.0.2:9000 (conn_id=24): read: read tcp 172.22.0.7:57588->172.22.0.2:9000: read: connection reset by peer" backoff=250ms
```

One line like that, once, is not an incident — check
`vantage_sink_envelopes_lost_total` (from step 1) before treating it as one.
Many lines like it, repeating, is ClickHouse itself being unreachable or
overloaded.

**3. Check ClickHouse is actually answering:**

```
$ docker exec vantage-clickhouse-1 clickhouse-client -u vantage --password vantage -q "SELECT 1"
1
```

(swap the container name / connection details for your deployment; in
Kubernetes this is whatever `clickhouse.externalHost` points at —
`deploy/helm/README.md`.)

**4. If lag is real and sustained, you are on the clock, not just watching
a number.** The streams are bounded (`DiscardOld`, ROUTES at 48h/8GiB —
`docs/operating.md`'s Lag section) — a writer that cannot catch up before
retention trims the oldest messages loses those rows permanently, with
nothing about the recovery looking wrong afterward. Fix the underlying
ClickHouse problem before the lag exceeds the stream's retention window, not
after.

## A screen reads zero

**This is usually a recency problem, not a data problem.** Every windowed
screen — Monitor, Events, Session history, and Peer detail's churn panel —
answers "what happened inside this window," and reads zero the moment
nothing has been archived inside it, even when the archive behind it holds
plenty. Each one says so explicitly rather than rendering a bare zero, but
not all four say it with the same copy.

Monitor and Peer detail's churn panel share `ChurnChart.vue`, which renders
this in place of an empty chart:

> Nothing was archived in this window, so there is nothing to draw. An
> empty chart is an answer — no bar is a bucket with no rows, not a missing
> feed.

Events states it as a note above the table, deliberately kept outside any
loading/error/empty branch so it never disappears along with the rows
(`EventsView.vue`):

> Showing events from the {window}, newest first. This is a window over the
> fleet, not everything the archive holds.

Session history states it via `EventArchiveNote.vue`, shared with Events
for the same reason — present whether the table above is full or empty,
once a router and peer are chosen:

> peer_events keeps history for the configured retention (90 days by
> default) on the collector clock; events past that window are deleted, not
> merely hidden, so an empty or short list here may mean nothing happened in
> the {window} shown, or that it aged out of retention.

That framing matters: a zero on any of these four screens that appears
*with* its screen's own window copy is the screen telling you its window is
genuinely empty. A zero that appears *without* it — a raw `0` with none of
the copy above — means something else is going on and is worth treating as
a real symptom rather than assuming it's this.

**In the dev stack, the fix is re-sending the committed captures:**

```
$ docker compose -f docker-compose.dev.yml restart evpn-replay ls-replay
```

This re-sends the same BMP captures the stack starts with; the writer's
insert-then-ack path means republished envelopes land as new rows —
**nothing already archived is lost or rewritten.** Verified against the dev
stack:

```
-- before --
$ docker exec vantage-clickhouse-1 clickhouse-client -u vantage --password vantage \
    -q "SELECT max(ts_collector), now(), dateDiff('minute', max(ts_collector), now()) FROM vantage.route_unicast"
2026-09-22 15:46:35.390923	2026-09-22 16:44:19	60

$ docker compose -f docker-compose.dev.yml restart evpn-replay ls-replay

-- after --
$ docker exec vantage-clickhouse-1 clickhouse-client -u vantage --password vantage \
    -q "SELECT max(ts_collector), now(), dateDiff('minute', max(ts_collector), now()) FROM vantage.route_unicast"
2026-09-22 16:44:19.954544	2026-09-22 16:44:28	0
```

and the 24-hour row count moved from 68 to 102 — up, not reset, confirming
the archive was appended to rather than replaced.

**In production there is no replay to restart — the equivalent question is
whether the collector is still receiving at all**, and `/v1/collectors`
answers that directly rather than leaving you to infer it from an absence
of rows: each card reports the collector's own live `/status`, so a
collector that has stopped answering is reported `reachable: false` with a
reason, not silently absent. See `docs/operating.md`'s "The operational
surface" section for a worked example (stopping a collector in this stack
and watching the card change).

**Distinguish `collector lost view` from a peer `down` — they are different
facts and no screen merges them.** From the API's own schema for peer
state:

> `state`: "down" is the router reporting the BGP session down, with a
> reason on the wire; `view_lost` is the collector's BMP transport ending,
> where the router said nothing.

If a screen reads zero peers up and you're trying to figure out whether the
*network* changed or the *collector* did, this is the first thing to check
— a page full of `down` means routers tore down sessions (a real network
event); a page full of `view_lost` means one or more collectors stopped
hearing from routers that, as far as anyone else can tell, may still be
fine. `/v1/collectors` and per-collector `/status` (above) tell you which
collector went blind, if any did.

`stale` is the archive saying it cannot vouch for a peer: its last word was
`up`, but its collector has not been heard from for 90 s (`stale_after`). If
every peer reads `stale` at once, look at NATS and `vantage-writer` first:
a stall between the collectors and ClickHouse silences every heartbeat
together. If one collector's peers do, look at that collector. It is down,
wedged, or cannot reach NATS (`vantage_collector_beats_dropped_total`). A
peer that reads `view_lost` with no `view_lost` event in its session history
belongs to a collector that has restarted since the session began.

## A RIB walk returns 400 for a router with more than one collector

This is deliberate, current behavior, and easy to misread as a bug. A RIB
walk (`/v1/rib/unicast`, `/v1/rib/vpn`, `/v1/rib/evpn`) is pinned to one
`(collector, session)` — that pinning is what keeps a keyset page from
silently dropping or duplicating rows if the session it's walking changes
mid-page. Ask for one without naming which collector, on a router two
collectors both monitor, and the answer is a 400 naming both:

```
$ curl -s -H "Authorization: Bearer dev-token-not-a-secret" \
    "http://127.0.0.1:9473/v1/rib/unicast?router=172.22.0.10&peer=10.255.1.1"
{"error":{"code":"invalid_param","message":"query: invalid filter: router 172.22.0.10 is monitored by 2 collectors (dev-c1, dev-c2); a RIB walk is pinned to one (collector, session) and there is nothing here to choose with -- ask Routers for each collector's own session and start the walk with a RIBCursor naming the one you want"}}
```

adding `&collector=<id>` (see the Routers screen, or `/v1/routers`, for
which collectors monitor a given router) resolves it:

```
$ curl -s -H "Authorization: Bearer dev-token-not-a-secret" \
    "http://127.0.0.1:9473/v1/rib/unicast?router=172.22.0.10&peer=10.255.1.1&collector=dev-c1"
{"data":[],"meta":{"next_cursor":null,"warnings":[{"code":"paginated_smear","message":"a paginated walk is a smear over a live table, not a snapshot: the session is pinned, but rows may be added or withdrawn between pages"}],"total_matched":null}}
```

A router monitored by exactly one collector never needs this — `collector=`
is only required once there is a real choice to make.
