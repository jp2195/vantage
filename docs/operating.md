# Operating

Day-to-day operation of a running pipeline: what to look at, what is worth
alerting on, how the one data-loss mode works, and how to upgrade without
surprising the daemons. For what `vantage-collector`, `vantage-writer` and
`vantage-api` each do internally, see `docs/architecture.md`; for standing
the pipeline up in the first place, see `docs/deploying.md`. When something
looks wrong, `docs/troubleshooting.md` is the symptom-first companion to
this page.

## The operational surface

Two things answer "is this pipeline healthy right now," and they answer
different questions.

**`GET /v1/collectors`** (and the "Collectors" screen built on it) is the
fleet view: one card per configured collector, each built from two sources
unioned together — what the archive knows about that `collector_id` and
what the collector says about itself live, over its own `/status`. Fetched
from the dev stack:

```
$ curl -s -H "Authorization: Bearer dev-token-not-a-secret" \
    http://127.0.0.1:9473/v1/collectors | python3 -m json.tool
```

```json
{
    "collector": "dev-c1",
    "reachable": true,
    "error": "",
    "endpoint": "http://collector:9469",
    "status": {
        "started_at": "2026-09-18T16:53:35.896932396Z",
        "observed_at": "2026-09-22T16:46:42.453404052Z",
        "sessions_active": 3,
        "bmp_messages_total": 12534,
        "events_published_total": 12545,
        "publish_errors_total": 0,
        "publish_rejects_total": 0
    }
}
```

(trimmed to one card; the real response also carries that collector's
routers, peer counts, and a per-minute rows-archived sparkline covering the
last 30 minutes, reported as `meta.activity_window`; it is not
configurable.)

`reachable` and `error` are the fields worth watching. Verified by stopping
one collector in the dev stack and asking again:

```json
{"collector": "dev-c2", "reachable": false, "error": "Get \"http://collector2:9569/status\": dial tcp: lookup collector2 on 127.0.0.11:53: server misbehaving", "status": null}
```

`error` names the failure — a DNS lookup here, a timeout or non-200 in
general — rather than leaving a reader to infer "not answering" from an
absence of rows. `collectors_timeout` (default 2s) bounds how long the whole
fan-out waits on a stuck collector.

**Each daemon's own `/status`**, on its metrics listener, is the
single-collector version of the same live facts — this is what
`/v1/collectors` itself polls:

```
$ curl -s 127.0.0.1:9469/status
{"collector_id":"dev-c1","started_at":"2026-09-18T16:53:35.896932396Z","observed_at":"2026-09-22T16:43:58.789261433Z","sessions_active":3,"bmp_messages_total":12355,"events_published_total":12363,"publish_errors_total":0,"publish_rejects_total":0}
```

Go straight to this when you already know which collector you care about,
or when `vantage-api` itself is the thing you are not sure is healthy.

**`/healthz` and `/readyz`**, on each daemon's metrics listener, are what a
load balancer or Kubernetes probe should use. `/healthz` is 200 whenever the
process is serving and never checks a dependency. `/readyz` is 503 with a
body of `not ready: <reason>` while a dependency is unusable: NATS for the
collector, NATS and a ClickHouse ping for the writer. The API's `/readyz`
reports its ClickHouse ping for monitoring only, and the Helm chart
deliberately does not gate the API's readiness on it (see
`deploy/helm/README.md`). ClickHouse pings are cached for 5 seconds.

**Which build is running:** every daemon logs its version at startup, answers
`-version`, and exports `vantage_build_info{component,version,revision}`.
`count by (component, version) (vantage_build_info)` shows a fleet mid-upgrade.

## Metrics worth alerting on

All three daemons publish Prometheus text on their `metrics_listen` port
(default `:9469` collector, `:9472` writer, `:9474` API). The dev stack does
not run a scraper. `deploy/dev/prometheus.yml` wires one up for the writer
and the collector (add `api:9474` for the API's metrics), and Fleet health's
two Prometheus panels in `docs/grafana.md` read the writer's.

**Writer (`:9472/metrics`)** — the more urgent set, because this is where
data loss becomes irreversible:

| Metric | Type | What it means |
| --- | --- | --- |
| `vantage_sink_consumer_lag{stream}` | gauge | Envelopes in the stream this writer has not yet archived: undelivered plus delivered-but-unacked. Climbing means the writer is falling behind the collectors. |
| `vantage_sink_envelopes_lost_total{stream}` | counter | Envelopes discarded by stream retention before this writer acked them — each one is a row permanently missing from ClickHouse. **Any increase is an incident**, not a threshold to tune. |
| `vantage_sink_lag_sample_errors_total{stream}` | counter | Failed attempts to read stream/consumer state; while these accrue, `consumer_lag` is stale rather than low — do not read a flat lag graph as "caught up" if this is also climbing. |
| `vantage_sink_insert_errors_total` | counter | ClickHouse insert attempts that failed; the batch is left unacked and retried. A sustained rate — not an isolated blip — means ClickHouse itself is unhealthy. |
| `vantage_sink_fetch_errors_total` | counter | JetStream `Fetch` calls that failed for a reason other than the batch window's own deadline or shutdown — a NATS-side symptom. |
| `vantage_sink_decode_errors_total` | counter | Envelopes that failed to protobuf-unmarshal and were acked without insertion — a version skew between what published the envelope and what is reading it, and worth paging on since it is otherwise silent. |
| `vantage_sink_invalid_envelopes_total` | counter | Envelopes that decoded but carried a value ClickHouse cannot store (an address field that does not parse, or a BGP ID that is not IPv4), acked without insertion so one bad envelope cannot stall the stream. Like `decode_errors`, it is silent otherwise: any increase means something is publishing envelopes the collector would not have produced. |
| `vantage_sink_rows_inserted_total` | counter | Rows durably inserted across all tables. Useful as a rate — a flatline while collectors report live sessions is itself a symptom. |
| `vantage_sink_current_cleanup_rows_targeted_total{table}` | counter | Rows of superseded sessions deleted from each current table, counted just before each DELETE (ClickHouse reports no deleted-row count). Only the writer holding the cleanup lease moves it; sum across writers. |
| `vantage_sink_current_cleanup_skipped_total` | counter | Cleanup cycles this writer skipped because another writer held the lease. With several writers all but one skip every cycle. If every writer skips, the lease is held by a writer that died without releasing it, and it lapses within two intervals. |
| `vantage_sink_current_cleanup_duration_seconds` | histogram | Duration of each cleanup cycle this writer ran. |
| `vantage_sink_current_cleanup_errors_total` | counter | Cleanup cycles that failed to take the lease or to clean a table. The current tables have no TTL, so while this climbs they grow without bound. |

A single retried insert, on its own, is not an incident — the writer backs
off and redelivers rather than losing the batch. From this stack's own
writer log during normal operation, in the default text log format:

```
time=2026-09-18T22:57:45.000Z level=ERROR msg="insert failed, not acking" stream=STATS durable=vantage-writer-stats rows=1 err="clickhouse: prepare stats_events: query processing: failed to read first block packet from 172.22.0.2:9000 (conn_id=24): read: read tcp 172.22.0.7:57588->172.22.0.2:9000: read: connection reset by peer" backoff=250ms
```

That single event did not move `vantage_sink_envelopes_lost_total` — it
tripped `vantage_sink_insert_errors_total` once and recovered. Alert on the
rate and on `envelopes_lost_total`, not on `insert_errors_total` alone.

**Collector (`:9469/metrics`)**:

| Metric | Type | What it means |
| --- | --- | --- |
| `vantage_collector_sessions_active` | gauge | Open BMP sessions. A sudden drop with no corresponding `bmp session closed` at the log level you'd expect is worth a look. |
| `vantage_collector_publish_errors_total` | counter | Publishes that failed at the call and are not retried: the NATS connection closed for good, an oversized message, or no room to retry: 4096 publishes to the same stream already being retried, after up to 10s of waiting. Each one is an event lost, and closes its BMP session (below). A full reconnect buffer or a full async window at the call is retried instead. |
| `vantage_collector_publish_rejects_total` | counter | Publishes that failed after the call returned: rejected by the JetStream server (a stream at a limit, for example), or retried until the retry bounds ran out (below). Each one is an event lost, and closes its BMP session. |
| `vantage_collector_publish_retries_total` | counter | Re-sends: each time an event was re-published with its original msg-id after a failure that says nothing about the message. The NATS connection dropped before the ack arrived (a NATS server restarting), a stream leader stepping down dropped it unanswered, the stream had no leader to answer, or the client's async window was full. One event can be re-sent more than once, so this counts re-sends, not events. Not a loss; JetStream's duplicate window makes a re-send of a message it already stored a no-op. A burst during a NATS restart is expected. An event that cannot be delivered within the bounds (below) counts as a reject and closes its session. |
| `vantage_collector_sessions_aborted_total` | counter | BMP sessions the collector closed itself because one of their events could not be published. **Any increase means data was lost** and depends on the router re-sending it; see [the second loss mode](#publish-failures-the-second-way-this-pipeline-loses-data). |
| `vantage_collector_beats_published_total` | counter | Heartbeats JetStream acknowledged: one at startup, one every 30 s, and one more each time the NATS connection is re-established. Only that reconnect beat is retried: if it fails, up to twice more, 5 s apart, for a cluster that takes clients back before its leaders are elected. The read side calls a collector `stale` when its newest heartbeat is older than 90 s, so a flat line here for longer than that is every peer of this collector reading `stale`. |
| `vantage_collector_beats_dropped_total` | counter | Heartbeats that failed to publish and were dropped, never retried: the next one replaces them. A few during a NATS restart are expected. A steady climb means NATS is unreachable from this collector. |
| `vantage_collector_owed_view_lost` | gauge | `view_lost` events a session's close-out could not publish, held until NATS is reachable again and then re-sent with their original msg-id. Nonzero only during or just after a NATS outage. An event being re-sent is not counted until that attempt fails, so a stuck event makes this flicker between 0 and 1 rather than hold steady; watch `republish_failed_total` for that. |
| `vantage_collector_owed_view_lost_dropped_total` | counter | Owed `view_lost` events dropped because 50,000 were already held, or because the collector shut down with them still owed (it tries each once first, if NATS is connected). After a shutdown drop the read side still resolves those peers: `view_lost` once this collector restarts, `stale` if it never does. After a drop because 50,000 were held, the collector is still running, so those peers read `up` until their routers open a new session with it or it restarts; a nonzero value here while the collector is up is worth a restart once NATS is back. |
| `vantage_collector_owed_view_lost_resent_total` | counter | Re-send attempts of owed `view_lost` events after NATS returned. One event can be attempted more than once, so this counts attempts, not events. |
| `vantage_collector_owed_view_lost_republish_failed_total` | counter | Re-send attempts that failed; each failed event is owed again and retried about once a second, with no limit. A steady climb with NATS connected means JetStream keeps refusing them (a full `PEER` stream, for example): fix the stream, or restart the collector, whose restart marks those peers `view_lost` anyway. |
| `vantage_collector_bmp_messages_total{type}` | counter | BMP messages read, by type (`initiation`, `termination`, `peer_up`, `peer_down`, `route_monitoring`, `route_mirroring`, `stats_report`, or `unknown` for any other type). A flatline on `route_monitoring` from a fleet you expect to be churning is a "no routes are appearing"-shaped symptom — see `docs/troubleshooting.md`. |
| `vantage_collector_connections_rejected_total{reason}` | counter | BMP connections closed before any BMP was parsed: `source_not_allowed` (outside `allowed_sources`), `proxy_not_trusted` (TCP peer outside `trusted_proxies`), `max_connections` (at the concurrent-connection cap), `nats_disconnected` (NATS was down, so the collector refused the session rather than accept a table dump it could not store). A steady `source_not_allowed` rate from an address you do not recognize means the BMP port is reachable from somewhere it should not be; from one you do, a router missing from the list. |
| `vantage_collector_parse_flags_total{flag}` | counter | Quirk radar — see `docs/quirks.md`. Not an alerting signal by itself (a handful of flags is normal on a diverse fleet), but a sudden step change after a router upgrade is worth investigating. |

`vantage_collector_mirror_active` / `vantage_collector_mirror_errors_total` /
`vantage_collector_mirror_messages_total` exist only when raw-BMP mirroring is
configured; leave them out of alerting on a stack that doesn't use it.

**API (`:9474/metrics`)**:

| Metric | Type | What it means |
|---|---|---|
| `vantage_api_http_requests_total{route,code}` | counter | Requests by route (the API's own route pattern, `ui` for the embedded app, `unmatched` for anything else under `/v1`) and status code. Alert on a sustained `code=~"5.."` rate per route. `route="unmatched"` with 401 is scanner traffic. |
| `vantage_api_http_request_duration_seconds{route}` | histogram | Latency by route: `histogram_quantile(0.99, sum by (route, le) (rate(vantage_api_http_request_duration_seconds_bucket[5m])))`. |

## Lag, the first way this pipeline loses data

Ack-after-durable makes the writer lossless with respect to *its own*
failures, but not with respect to *time*. The streams are `LimitsPolicy` with
`DiscardOld` and a finite `MaxAge`/`MaxBytes` (ROUTES is 48h / 8 GiB), so
retention deletes the oldest messages whether or not anything consumed them.
A writer that is down, wedged, or merely slower than the routers for long
enough therefore loses envelopes permanently — and nothing about the recovery
looks wrong: the stream is healthy, the consumer resumes cleanly from what
survived, and ClickHouse simply never receives those rows.

Each consumer samples its stream and consumer position every 30s and reports:

```
vantage_sink_consumer_lag{stream}              # not yet archived: undelivered + unacked
vantage_sink_envelopes_lost_total{stream}      # discarded by retention before being acked
vantage_sink_lag_sample_errors_total{stream}   # the gauge above is stale, not low
```

`envelopes_lost_total` is a counter rather than a gauge on purpose. The
condition that produces it heals — once the writer catches up, its ack floor
climbs back above the trim point and any "current gap" reading returns to
zero — but the hole in ClickHouse does not. All three series are published at
zero on startup, so a healthy pipeline reads as an explicit `0` rather than as
absent data that an alert rule would never evaluate.

## Publish failures, the second way this pipeline loses data

The collector cannot ask a router for a BMP message again. BMP runs one way,
and once the collector has read a message and failed to hand its events to
JetStream, those events are gone. A failure shows up either at the publish
call (the NATS connection closed for good, or too long waiting for room to
retry) or after it, when JetStream rejects the message, or when a publish
that failed for a transient reason could not be delivered within the retry
bounds (see the re-send bullet below).

Left alone, the session would stay up with a hole in it, and the archive
would stay wrong until the session happened to reset, which can take weeks.
So the collector does three things:

- **It closes a session when any of its events fails to publish.** One
  failure is enough, because the session's view is already wrong. Failures
  that surface after the call are traced back to the session that produced
  the event, so only that session is closed. The router reconnects under a
  new session ID and, on platforms that re-send their tables on reconnect,
  fills the gap. Each close increments
  `vantage_collector_sessions_aborted_total`.
- **It refuses new sessions while NATS is disconnected.** A connection that
  arrives then is accepted and closed at once, counted as
  `vantage_collector_connections_rejected_total{reason="nats_disconnected"}`,
  and the router retries on its own reconnect timer. Accepting it would
  stream the router's full table dump into a reconnect buffer that fills in
  seconds. Sessions that were already open when NATS dropped keep running.
  Their new publishes wait in the NATS client's reconnect buffer and are
  sent whenever NATS returns. One not acknowledged within the 10s async
  timeout is not reported: it is retried like any other timeout (next
  bullet). A session is closed only if one of its publishes fails for good.
- **It re-sends publishes that failed for a transient reason.** Each is
  re-sent unchanged, under the same msg-id; JetStream answers a re-send of a
  message it already stored as a duplicate and stores nothing.
  - *The connection dropped with it in flight.* The NATS client fails every
    publish still waiting for its ack the moment the connection drops,
    whether or not the server stored it. It is re-sent once the connection
    is back.
  - *A stream leader dropped it.* A leader stepping down (as it does when
    its server enters lame-duck mode) discards publishes it had not yet
    committed, without replying. They fail with the 10s ack timeout, or with
    `raft: not leader`, and are re-sent after a backoff of 250ms doubling to
    2s. A re-send can meet `duplicate message id is in process` while the
    leader still holds the original uncommitted; that is retried the same
    way.
  - *The stream did not answer at all* (`no response from stream`: no
    leader, mid election). Re-sent after the same backoff, for at most 20s:
    a subject no stream captures looks the same as a stream with no leader,
    and is reported after those 20s.
  - *The client had no room to send it*: its reconnect buffer or its async
    window (4096 unacknowledged publishes) was full. Re-sent the same way.

  The bounds keep every re-send inside the streams' 2-minute duplicate
  window: no re-send starts more than 60s after the original publish, at
  most 16 attempts per event, and the 20s window above. Each stream has its
  own budget of 4096 events being retried at once. While a stream's budget
  is full, a session publishing to that stream waits up to 10s for one to
  finish, which stops it reading from the router meanwhile; if none does,
  it fails the publish, and later publishes to that stream fail at once
  until one finishes. Shutdown ends that wait at once. Past any bound the
  publish is reported failed and its session closed, as above. Other
  failures are never retried: a JetStream rejection (a stream at a limit, a
  message over the size limit) and a connection closed for good.

  The per-stream budget matters for RAW, which keeps a single copy on one
  NATS server. While that server restarts, RAW publishes get `no response
  from stream` for longer than the 20s window, so sessions that publish to
  RAW -- those with a message the collector could not parse, a Termination
  message, or an armed mirror -- are closed. Sessions that only publish
  routes, peer events, stats and link state are not affected.

  Each re-send increments `vantage_collector_publish_retries_total`. They
  are logged at WARN across the collector, not per session: `publish failed
  in flight; re-sent it with the same msg-id`, with `count` (re-sends
  covered by the line) and the most recent `err`. The first line comes 1s
  after the first re-send, so it counts the burst, and later ones at most
  every 10s. A rolling restart of the NATS cluster should move this counter
  and not `sessions_aborted_total`. Events still being retried at shutdown
  are included in the drain. One that cannot finish before the drain times
  out makes the collector exit non-zero, and the drain error names each
  such event with the failure it was retrying.

  **Known residual: a duplicate history row.** A re-send is only sent while
  the connection is up, but if the connection drops again straight after, it
  waits in the client's reconnect buffer and goes out whenever NATS returns.
  If that is more than 2 minutes after the original was stored, JetStream
  no longer remembers the msg-id and stores the event a second time, under a
  new stream sequence. The history table keeps the duplicate row. The route
  and link-state `*_current` tables collapse it, but `peer_current` keeps it
  too, because the stream sequence is part of its sort key. Current state is
  unaffected, because the two rows are the same event at the same sequence
  number; only counts over events (churn, for example) see it.
- **It logs the failure once per session per 10s**, not once per event:
  `publish failed; closing the BMP session so the router reconnects and
  re-sends`, with `count` (failures covered by this line) and `err` (the
  most recent failure). During an outage every in-flight publish fails, and
  a line per event would bury the rest of the log.

**Recovery depends on the router, and not every platform recovers fully.**

- **NX-OS** re-sends its tables when a BMP session reconnects. It reports
  `initial-refresh delay 30` without it being configured, so after the
  reconnect the collector's view is complete again.
- **IOS-XR** does not re-send its tables after a reconnect of the BMP
  session alone, even with `initial-refresh` configured. After a
  reconnect it sends Initiation and a Peer Up per peer, then only new BGP
  updates (see `docs/quirks.md`, "IOS-XR: no table dump without
  `initial-refresh`"). Routes that were lost and have not changed since
  stay missing until the BGP session itself resets. Bouncing the BGP
  neighbor refills the stream; a soft refresh does not. **For XR the loss
  window is not fully recovered by this mechanism.** Treat any increase of
  `sessions_aborted_total` on an XR router as a reason to reset its BGP
  neighbors or to accept a partial view.

Alert on `vantage_collector_sessions_aborted_total` alongside
`vantage_sink_envelopes_lost_total`: the first counts losses between the
collector and NATS, the second losses between NATS and ClickHouse.

## Current-table cleanup

The `*_current` tables (`route_unicast_current`, `route_vpn_current`,
`route_evpn_current`, `ls_nodes_current`, `ls_links_current`,
`ls_prefixes_current`, `peer_current`, `eor_current`) carry no TTL, so
nothing but this cleanup ever removes a row from them. Every
`vantage-writer` runs it on `current_cleanup_interval` (default `1h`; `0s`
disables it), but only one writer acts on any given cycle: they coordinate
through the `cleanup_lease` table in ClickHouse, and the rest skip. Each
cycle, the holder computes every router's floor — the older of its two
newest known sessions in `peer_current` — and deletes each current table's
rows below their router's floor. See `sink/cleanup.go`'s own header for why
two sessions are kept rather than one, and why the statement names what to
delete rather than what to keep.

The lease assumes a single ClickHouse server; on a replicated or cloud
ClickHouse, run one writer, or accept that cleanup cycles may occasionally
overlap, which only repeats idempotent deletes (and counts their rows in
`rows_targeted_total` twice).

The four metrics are in the writer table above; read together, they answer
"is cleanup keeping the current tables bounded":

- **Healthy, one writer.** `vantage_sink_current_cleanup_duration_seconds`
  reports a cycle roughly every interval, `vantage_sink_current_cleanup_errors_total`
  stays flat at 0, and `vantage_sink_current_cleanup_rows_targeted_total{table}`
  rises only when a router has actually superseded a session (a reconnect, a
  restart) — a long flat stretch on a stable fleet is expected, not a sign
  cleanup stopped running; `duration_seconds` moving each cycle is what shows
  it still is.
- **Healthy, several writers.** Exactly one writer's `rows_targeted_total`
  and `duration_seconds` move each cycle; every other writer's
  `vantage_sink_current_cleanup_skipped_total` climbs by one per cycle
  instead, because it found the lease already held. Summing
  `rows_targeted_total{table}` across writers is still correct — only the
  holder increments it, so the sum is never double-counted.
- **Unhealthy.** A climbing `errors_total` means a cycle failed to take the
  lease or to clean a table; since these tables have no TTL as a backstop,
  they grow without bound for as long as this continues. Every writer's
  `skipped_total` climbing on the same cycle, with none of them reporting
  `rows_targeted_total` or `duration_seconds`, means the lease is held by a
  writer that died without releasing it — expected to self-heal within about
  two intervals (the lease's lifetime), since a dead holder cannot renew it.

**The knob is cadence, not size.** `current_cleanup_interval` controls how
often cleanup runs, not how much it keeps: a cycle always deletes down to
exactly the two-newest-sessions floor, whatever the interval, so a shorter
interval only means superseded rows sit for less time before they go, at the
cost of the count queries a cleanup cycle itself runs. This is a different
knob from history's `retention.days` (see "Retention" in
`docs/deploying.md`): that one bounds the ten history tables by age; the
current tables have no age-based retention at all, live rows stay
indefinitely, and cleanup is the only thing that bounds their size.

## Retiring a router or collector

Current state never expires. Cleanup keeps each router's two newest sessions
and deletes only older ones, so the last session a router had stays in the
current tables after the router is gone, and a `collector_id` that is no
longer used stays listed by `/v1/routers` and `/v1/collectors` until you
remove it. The archive stops vouching for it on its own:
- A retired router's peers read `down` or `view_lost`.
- A retired collector's peers read `view_lost` if it shut down cleanly or
  restarted, or `stale` if it was killed and never came back.

Removing it is an operator's decision, because a collector stale for a week
might be a datacenter outage. `vantage purge` is how:

```
vantage purge -dsn 'clickhouse://<user>:<password>@<host>:9000/vantage' \
  -collector <collector_id> [-router <router address>] [-dry-run] [-force]
```

- With `-router`, it removes that collector's view of that router. Without
  it, it removes everything the collector holds, and its heartbeats too.
- `-dry-run` prints how many rows each table would lose, whether the target
  is live and so whether a real run would refuse, and deletes nothing.
- It refuses while the target is live, meaning both:
  - its collector was heard from within `-stale-after` (90 s by default);
  - the target has a current session that collector's running process
    opened, with at least one peer still `up`.

  Deleting that would remove state a running collector is still
  maintaining. Stop the collector, or disconnect the router from it, first:
  a router whose session closed has its peers read `view_lost` or `down`,
  and purges without `-force`. Or pass `-force`, which purges anyway and
  prints why it would have refused.
- It also refuses, without `-force`, a collector past `-stale-after` but
  heard from less than 15 minutes ago. A NATS or writer stall longer than
  the threshold makes a running collector look exactly like a dead one, and
  once the backlog drains, the rows it had queued would be written after the
  purge and leave a current view missing everything before them.
- Set `-stale-after` to the API's `stale_after` when that is not the 90 s
  default, so the purge judges liveness the way the API reports it.
- It deletes from the eight current tables with `peer_current` last, then
  the heartbeats. A purge interrupted partway leaves the target visible, and
  running it again finishes it. If a `DELETE` fails, ClickHouse can keep
  its mutation queued and retry it; the error says how to find it in
  `system.mutations` and `KILL MUTATION` it.
- It deletes only the sessions it counted. A router that reconnects while
  the purge runs opens a newer session, and that session's rows stay.
- The DSN's user needs `ALTER DELETE` on those tables, the same privilege
  the writer's cleanup uses. The API is read-only by design and has no
  purge.
- The writer's cleanup can run at the same time. When one of its deletes
  fails, it kills only its own mutations, which it finds by the literal
  text `floor_sid` in the mutation's command. ClickHouse records a
  `DELETE`'s command with its values filled in, so the collector id is part
  of that text. `vantage purge` refuses a collector id containing
  `floor_sid`, so a purge never carries it; remove such a collector with
  the SQL below. That SQL carries the text too, so a cleanup failing at the
  same moment can kill one of its statements. Running the same SQL again
  finishes the job, and a `SELECT count()` with the same `WHERE` shows
  whether anything is left.

History is not touched. The router's sessions, events and churn stay in the
ten history tables, and in session history and the fleet events console,
until `retention.days` expires them. If they must go sooner, the same
`WHERE` clause works as a lightweight `DELETE` on each history table.

The same purge by hand, for a ClickHouse the CLI cannot reach:

```
clickhouse-client --host <host> --port 9000 --user <user> --password <password> \
  --param_collector='<collector_id>' --param_router='<router address>' \
  --multiquery <<'SQL'
DELETE FROM vantage.eor_current           WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.route_unicast_current WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.route_vpn_current     WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.route_evpn_current    WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.ls_nodes_current      WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.ls_links_current      WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.ls_prefixes_current   WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
DELETE FROM vantage.peer_current          WHERE collector_id = {collector:String} AND router_ip = toIPv6({router:String});
SQL
```

To retire a whole collector:
1. Drop `AND router_ip = toIPv6({router:String})` from each statement, and
   pass only `--param_collector`.
2. Add, last:

```
DELETE FROM vantage.collector_beats       WHERE collector_id = {collector:String};
```

## Upgrades

**Dev stack.** `docker compose -f docker-compose.dev.yml up --build -d`
rebuilds and replaces images in place. The schema is applied only to an
empty ClickHouse volume (`docker-entrypoint-initdb.d` semantics — see
"Applying the schema" in `docs/deploying.md`), so a schema change needs
`docker compose -f docker-compose.dev.yml down -v` first, or a manual
migration under `deploy/clickhouse/migrations/`.

**Helm.** Five things matter together on every `helm upgrade`:

- Always pass `--wait --wait-for-jobs`, not `--wait` alone. `schemaJob.apply`
  (default `true`) applies `deploy/clickhouse/schema.sql`, its migrations,
  and the AS-holder-name dictionary DDL from a Job that re-runs on every
  upgrade, and `--wait` does not cover Jobs — a failing schema Job can leave
  `helm upgrade` reporting success against a database that never received
  the schema. If your database's owner will not grant this chart DDL
  rights, set `schemaJob.apply=false` and apply the files yourself before
  upgrading, in order: `schema.sql`, then each migration the database
  needs, then the two dictionary files (see "Applying the schema" in
  `docs/deploying.md`).
- NATS mutual TLS is on by default (`nats.tls.enabled: true`), and the
  render fails until you choose: supply certificates
  (`deploy/nats-tls/gen-certs.sh` or `nats.tls.issuerRef`), or opt out
  explicitly with `nats.tls.enabled`,
  `nats.tlsCA.enabled`, `nats.config.nats.tls.enabled` and
  `nats.config.cluster.tls.enabled` all `false`. See "NATS TLS" in
  `deploy/helm/README.md`.
- NATS runs as three clustered servers with three-copy streams by default,
  so upgrading NATS or draining a node restarts one server at a time, and
  publishes to ROUTES, LS, PEER and STATS keep succeeding. RAW keeps one
  copy, so sessions that publish to RAW while the server holding it
  restarts are closed (see the per-stream retry budget under "Publish
  failures" above). With the single-server opt-out
  (`nats.config.cluster.enabled: false`) every NATS restart is still the
  publish failure described under "Publish failures" above. Moving an
  install from the single-server opt-out to three servers is the
  exception: the cluster does not adopt the single server's streams, so
  plan it as described in
  "Moving an existing install to three servers" in
  `deploy/helm/README.md`: the collector is stopped before NATS restarts
  and started again only once the old streams are deleted, so routers are
  disconnected for that window, and the writer is restarted at the end.
- Images are tagged by version or git short SHA, never `latest`. Release
  images carry the release version (`X.Y.Z`, which the chart's empty
  `images.*.tag` resolves to through its `appVersion`) and the commit's
  short SHA; images you build with `make push-images` carry the short SHA.
  Either way an upgrade can be rolled back by pointing `images.*.tag` (or
  the chart version) at the previous one rather than by guessing which
  build is "currently" deployed. See "Images" in `docs/deploying.md`.
- Both `vantage-writer` and `vantage-api` refuse to start against a schema
  version other than the one they were built for, rather than guessing at a
  shape they were not compiled against. Read directly from the writer's own
  check (`sink/clickhouse.go`), this is the exact refusal:

  ```
  clickhouse: schema version %d, this binary expects %d -- refusing to insert into an unexpected shape
  ```

  A stalled or skipped schema Job therefore does not corrupt data — it
  shows up immediately as both daemons crash-looping, which is the intended
  loud failure over a silent one. This message was read from source, not
  triggered against a live cluster; see `docs/deploying.md`'s "It creates;
  it does not migrate" section for the recreate-vs-migrate decision that
  produces it.

Not yet exercised end to end: the Helm rollback flow above, and the exact
log and event shape of the schema-mismatch crash loop under Kubernetes.
Both follow from the code and from `deploy/helm/README.md`.
