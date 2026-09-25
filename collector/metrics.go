// Metrics: the Prometheus instrumentation vantage-collector exposes on
// Config.MetricsListen at /metrics. Registered once, at package init, via
// promauto -- not per-Server -- so a process that somehow constructed more
// than one Server (it doesn't today, but nothing stops a future test or tool
// from doing so) still has exactly one counter/gauge family of each, shared
// across every Server instance in that process.
//
// Label cardinality is bounded by construction: every label used below (BMP
// message type, envelope payload kind, parse-flag name) is drawn from a small
// fixed vocabulary this package or the quirk/vantagev1 packages define, never
// from anything a router chooses (a peer IP, a prefix, a router hostname). A
// per-peer or per-prefix label on any of these would be a cardinality
// explosion under a full route dump or many peers -- exactly what this
// design avoids.
package collector

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jp2195/vantage/bmp"
)

var (
	metricBMPMessages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_collector_bmp_messages_total", Help: "BMP messages read, by BMP message type."},
		[]string{"type"})
	metricEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_collector_events_published_total", Help: "Envelopes handed to the publisher, by payload kind."},
		[]string{"payload"})
	metricParseFlags = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_collector_parse_flags_total", Help: "Parse flags attached to envelopes (quirk radar)."},
		[]string{"flag"})
	metricPublishErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_publish_errors_total", Help: "Publisher errors."})
	// metricPublishRejects counts publishes the *server* rejected, which
	// resolve asynchronously long after Publish returned. Kept separate from
	// metricPublishErrors so the two failure modes stay distinguishable:
	// that counter is local backpressure (the client shedding when NATS is
	// unreachable), this one is JetStream refusing to store a message we
	// believed we had sent -- e.g. a subject captured by no stream.
	metricPublishRejects = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_publish_rejects_total", Help: "Publishes rejected by the JetStream server (async)."})
	// metricPublishRetries counts re-sends: each time natsutil re-published
	// an event with its original msg-id after a failure that says nothing
	// about the message (the connection dropped before the ack arrived, a
	// stream leader change dropped it, the stream had no leader). One event
	// can be re-sent more than once, so this counts re-sends, not events. A
	// re-send is not a loss: an event that cannot be delivered within
	// natsutil's bounds is reported as a reject and closes its session like
	// any other. A burst of these during a NATS restart is the retry doing
	// its job; a steady rate means NATS keeps dropping this collector.
	metricPublishRetries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_publish_retries_total", Help: "Re-sends of an event with its original msg-id after it failed in flight."})
	// metricSessionsAborted counts BMP sessions the collector closed itself
	// because an event could not be published. Each one is a router that
	// must reconnect and re-send before its view in the archive is whole
	// again.
	metricSessionsAborted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_sessions_aborted_total", Help: "BMP sessions closed because an event could not be published."})
	metricSessions = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vantage_collector_sessions_active", Help: "Open BMP sessions."})
	metricMirrorActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vantage_collector_mirror_active", Help: "Routers currently being mirrored into the raw stream."})
	metricMirrorMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_mirror_messages_total", Help: "BMP messages republished by mirror mode."})
	metricMirrorErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_mirror_errors_total", Help: "Mirror publishes that failed."})
	// metricProxyHeader counts PROXY protocol header outcomes. "absent" is the
	// one an operator needs during a rollout: separating it from "malformed"
	// distinguishes "the flag is on and the proxy is not configured" from
	// "the proxy is configured and we disagree about the format". Its result
	// label is always one of the proxyResult* constants below -- none comes
	// from anything a sender chooses, per this file's cardinality rule.
	metricProxyHeader = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vantage_collector_proxy_header_total", Help: "PROXY protocol headers read, by outcome."},
		[]string{"result"})
)

// The complete vocabulary of metricProxyHeader's result label. Declared as
// constants rather than written inline at each call site so a typo is a
// compile error instead of a silently new time series that no dashboard or
// alert selects.
const (
	// proxyResultAccepted: a header named a source and the session opened
	// under it.
	proxyResultAccepted = "accepted"
	// proxyResultLocal: a v2 LOCAL command, in practice a proxy
	// health-checking its backend. Not an error and not a session.
	proxyResultLocal = "local"
	// proxyResultUnspec: a v1 UNKNOWN line or a v2 AF_UNSPEC family -- a real
	// connection whose source the sender declines to state.
	proxyResultUnspec = "unspec"
	// proxyResultAbsent: the stream began with neither signature, i.e. raw
	// BMP. The rollout failure: the flag is on and the proxy in front is not
	// configured to send headers.
	proxyResultAbsent = "absent"
	// proxyResultMalformed: bytes that claimed to be a header and could not
	// be parsed as one. The proxy is configured and we disagree about the
	// format.
	proxyResultMalformed = "malformed"
	// proxyResultClosed: the peer connected and hung up having sent nothing.
	// Kept out of "malformed" because it is what an L4 health check and a
	// port scan look like -- notably HAProxy's `send-proxy check` without
	// check-send-proxy, which is a correctly working front-end.
	proxyResultClosed = "closed"
	// proxyResultTimeout: the peer connected, sent nothing, and was closed by
	// handleConn's read deadline. Same senders as "closed", holding the
	// socket open instead of closing it.
	proxyResultTimeout = "timeout"
)

// bmpTypeName maps a bmp.Msg.Type byte to the fixed, human-readable label
// value metricBMPMessages uses. m.Type is attacker-controlled (it comes
// straight off the wire before any parsing), so unrecognized values are
// folded into the single "unknown" bucket rather than interpolated into the
// label (e.g. fmt.Sprintf("type_%d", t)) -- the latter would let a
// misbehaving or hostile peer mint a fresh label value per garbage byte it
// sends, which is the same class of cardinality problem this file's doc
// comment warns about, just attacker- rather than router-count-driven.
func bmpTypeName(t uint8) string {
	switch t {
	case bmp.TypeRouteMonitoring:
		return "route_monitoring"
	case bmp.TypeStatsReport:
		return "stats_report"
	case bmp.TypePeerDown:
		return "peer_down"
	case bmp.TypePeerUp:
		return "peer_up"
	case bmp.TypeInitiation:
		return "initiation"
	case bmp.TypeTermination:
		return "termination"
	case bmp.TypeRouteMirroring:
		return "route_mirroring"
	default:
		return "unknown"
	}
}

// CountPublishReject increments the async server-rejection counter. It exists
// so cmd/vantage-collector can wire natsutil.Publisher.OnError to this
// package's metrics without natsutil importing prometheus or this package
// importing natsutil.
func CountPublishReject(error) { metricPublishRejects.Inc() }

// CountPublishRetry counts and logs one re-send. It is for
// natsutil.Publisher.OnRetry, which calls it once per re-send actually
// published, wired in cmd/vantage-collector for the same reason as
// CountPublishReject.
//
// The log is per process, not per session: a NATS server restart fails the
// in-flight publishes of every session at once. Its first line is written
// publishRetryLogWindow after the first re-send, so "count" covers the
// burst, and later lines at most once per publishFailureLogInterval. Call
// FlushPublishRetryLog once publishing has stopped so the last count is
// written.
func CountPublishRetry(err error) {
	metricPublishRetries.Inc()
	publishRetries.record(slog.Default(), "err", err)
}

// FlushPublishRetryLog writes the re-send count CountPublishRetry has not yet
// logged, if any.
func FlushPublishRetryLog() { publishRetries.flush() }

// publishRetryLogWindow is how long the retry log gathers a burst before its
// first line. Re-sends after a disconnect all go out within milliseconds of
// the reconnect; ack timeouts after a leader change arrive spread over the
// ack timeout's own jitter, well under a second.
const publishRetryLogWindow = time.Second

var publishRetries = &burstLog{msg: publishRetryMsg, level: slog.LevelWarn,
	window: publishRetryLogWindow, interval: publishFailureLogInterval}

const publishRetryMsg = "publish failed in flight; re-sent it with the same msg-id"
