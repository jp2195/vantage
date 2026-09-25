// Server: the BMP TCP daemon. Accepts one connection per router, runs one
// Session per connection on that connection's own goroutine (Session is not
// safe for concurrent use -- see session.go's package doc), and forwards
// every Event that Session.Handle produces to an EventPublisher.
package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/proxyproto"
	"github.com/jp2195/vantage/quirk"
)

// defaultIdleReadTimeout bounds how long handleConn will block inside
// bmp.ReadMsg waiting for the next BMP message before giving up and closing
// the connection.
//
// This is deliberately generous, not a tight liveness check: unlike BGP
// KEEPALIVE, nothing in RFC 7854 requires a router to send BMP traffic on any
// schedule when there is nothing new to report, and even Stats Reports (the
// closest thing to a heartbeat BMP has) are commonly configured on the order
// of minutes. A short timeout here would sever perfectly healthy, quiet BMP
// sessions. Its purpose is narrower: bound the lifetime of a goroutine (and
// its socket) for a peer that opens a connection and then never sends
// anything at all -- an accidental listener hit by a health check or port
// scan, or a router wedged before it ever emits its Initiation message -- so
// that case doesn't leak a goroutine for the life of the process.
//
// This bounds only the wait for a connection's FIRST message -- see the read
// loop in handleConn, which clears the deadline once one arrives (and
// defaultMessageReadTimeout, which bounds each message once it has begun). It is
// deliberately not a per-read idle timeout: BMP sessions are hours-long and
// legitimately silent between events, so a per-read deadline resets healthy
// sessions and loses the router's next update. After the first message,
// dead-peer detection is TCP keepalive's job.
//
// Configurable as streams-independent `handshake_read_timeout` in Config.
const defaultIdleReadTimeout = 5 * time.Minute

// defaultMessageReadTimeout bounds how long the rest of a BMP message may take
// to arrive once its first byte has. It is not an idle timeout: the gap
// between messages stays unbounded, because a healthy router can be silent
// for hours. What it closes is a peer that sends a header claiming a large
// body and then trickles it, or stops, holding a goroutine, an fd and a
// partly filled buffer for the life of the process. A real router writes a
// message in one burst; a minute leaves room for a congested WAN path and a
// maximum-size message many times over.
//
// Configurable only in tests, via Server.messageReadTimeout.
const defaultMessageReadTimeout = 60 * time.Second

// tcpKeepAlivePeriod is how often the kernel probes an otherwise-idle BMP
// connection to detect a peer that vanished without a clean FIN (a crashed
// router, a dead link, a stateful middlebox that dropped the flow) well
// before defaultIdleReadTimeout would otherwise notice.
const tcpKeepAlivePeriod = 30 * time.Second

// EventPublisher is the sink handleConn hands every Session-produced Event
// to. It is exactly natsutil.Publisher's method set, kept as a narrow
// interface here so tests can capture published events without a real NATS
// server (see server_test.go's capturePub) and so this package does not
// import natsutil at all.
type EventPublisher interface {
	Publish(Event) error
}

// NotifyingPublisher is an EventPublisher whose publishes can also fail after
// Publish returned -- a JetStream rejection, or an async publish timing out --
// and that can report such a failure back against the publish that caused it.
// natsutil.Publisher implements it. When the Server's publisher does, a late
// failure closes the session that produced the event, exactly as a
// synchronous one does; without it only synchronous failures can.
type NotifyingPublisher interface {
	EventPublisher
	// PublishNotify publishes ev and, if it later fails, calls onFail once
	// with the error, from any goroutine. It does not call onFail for a
	// failure it returns.
	PublishNotify(ev Event, onFail func(error)) error
}

// ConnectedPublisher is a publisher that can say whether its connection to
// the message bus is up. natsutil.Publisher implements it. When the Server's
// publisher does, a connection that arrives while it reports false is closed
// before a session opens (see handleConn).
type ConnectedPublisher interface {
	Connected() bool
}

// Server is the BMP TCP daemon: one accept loop, one goroutine and one
// Session per accepted connection.
//
// Nothing on Server is mutated by handleConn beyond the fields explicitly
// designed for concurrent access (lastSessionID, natsDown, owed, and the
// package-level promauto metrics, all of which are safe for concurrent use on
// their own terms). cfg, pub, now, and idleReadTimeout are set once in NewServer and
// only ever read afterward, so many connection goroutines reading them
// concurrently is safe without a mutex.
type Server struct {
	cfg Config
	pub EventPublisher
	now func() time.Time

	idleReadTimeout    time.Duration
	messageReadTimeout time.Duration
	log                *slog.Logger

	// allowed and trusted are cfg.AllowedSources and cfg.TrustedProxies,
	// parsed once. allowAny and trustAny record that the configured list was
	// empty, and are derived from the configured strings rather than from the
	// parsed slices so that a Config that skipped LoadConfig's validation and
	// carries only unparsable entries fails closed -- an allowlist that
	// parsed down to nothing admits nobody, not everybody.
	allowed  []netip.Prefix
	allowAny bool
	trusted  []netip.Prefix
	trustAny bool
	maxConns int

	// natsDown rate-limits the line logged for each connection refused
	// because the publisher is disconnected: every router retries on its own
	// timer, and a large fleet retrying through an outage would otherwise
	// log a line per router per retry.
	natsDown *rateLimitedLog

	// Mirrors tracks which routers are currently armed for mirror mode (see
	// mirror.go). Safe for concurrent use by design, so every connection
	// goroutine's handleConn can call Take on it without additional locking
	// here.
	Mirrors *MirrorRegistry

	// lastSessionID is the most recently issued session ID (see
	// nextSessionID). It exists so two connections accepted back to back --
	// including two whose now().UnixNano() calls land on the same value,
	// which happens routinely under a coarse or fake clock and is not
	// vanishingly rare even under a real one on fast hardware or a busy
	// reconnect storm -- can never be handed the same sessionID. Two Sessions
	// sharing a sessionID would collide their JetStream dedup keys
	// (router/peer/session_id/seq), silently dropping one session's events
	// as "duplicates" of the other's.
	lastSessionID atomic.Int64

	// owed holds close-out view_lost events whose publish failed, and
	// owedRetryInterval is how often Serve tries them again. See owed.go.
	// owedRetryInterval is set in NewServer and may be changed by a test
	// before Serve starts, never after.
	owed              *owedLedger
	owedRetryInterval time.Duration
}

// NewServer constructs a Server. now supplies collector-side wall-clock time
// (injected for tests) for both envelope timestamps (via the Sessions it
// creates) and session-ID derivation.
//
// cfg is expected to have passed LoadConfig's validation. A Config literal
// that did not is still safe: unparsable allowed_sources or trusted_proxies
// entries are dropped without widening either list, and an unset
// MaxConnections gets the same default LoadConfig would have given it.
func NewServer(cfg Config, pub EventPublisher, now func() time.Time) *Server {
	allowed, _ := parsePrefixes(cfg.AllowedSources)
	trusted, _ := parsePrefixes(cfg.TrustedProxies)
	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = defaultMaxConnections
	}
	return &Server{cfg: cfg, pub: pub, now: now,
		idleReadTimeout: defaultIdleReadTimeout, messageReadTimeout: defaultMessageReadTimeout,
		natsDown: &rateLimitedLog{
			msg:      "closing connection: NATS is disconnected, so nothing this session sends could be stored",
			level:    slog.LevelWarn,
			interval: publishFailureLogInterval,
			now:      now,
		},
		allowed: allowed, allowAny: len(cfg.AllowedSources) == 0,
		trusted: trusted, trustAny: len(cfg.TrustedProxies) == 0,
		maxConns:          maxConns,
		Mirrors:           NewMirrorRegistry(),
		owed:              newOwedLedger(maxOwedViewLost),
		owedRetryInterval: defaultOwedRetryInterval,
	}
}

// nextSessionID returns a collector-side session ID: now().UnixNano() unless
// that value would not be strictly greater than the last one issued, in
// which case it is bumped to last+1. See lastSessionID's doc comment for why
// this matters -- sessionID is part of the JetStream dedup key, so two
// concurrent or rapid-fire connections must never receive the same one.
// Monotonic and collision-free within a process run; not persisted, so a
// restart simply starts issuing fresh IDs from the current wall clock again
// (which is the documented contract: "monotonic across restarts without
// persistence").
func (s *Server) nextSessionID() uint64 {
	for {
		last := s.lastSessionID.Load()
		next := s.now().UnixNano()
		if next <= last {
			next = last + 1
		}
		if s.lastSessionID.CompareAndSwap(last, next) {
			return uint64(next)
		}
	}
}

// Serve accepts BMP connections on ln until ctx is canceled, running each on
// its own goroutine. It returns once the accept loop has stopped AND every
// connection goroutine it started has returned -- not merely once accepting
// has stopped -- so that by the time Serve returns to a caller, every Publish
// call any session ever issued has already happened and is reflected in the
// EventPublisher's own accounting (for natsutil.Publisher, its inflight
// counter). A caller that Drains its Publisher immediately after Serve
// returns is therefore guaranteed to wait for, and learn the fate of, every
// event this server ever handed it -- not a snapshot taken while session
// goroutines might still be running and about to Publish something Drain
// will never see.
//
// ctx cancellation closes ln (unblocking Accept) and, inside each connection
// goroutine, the connection itself (unblocking that goroutine's blocking read
// so it can observe ctx and return promptly rather than waiting out
// defaultIdleReadTimeout).
//
// At most cfg.MaxConnections connections are served at once. One past that is
// closed as soon as it is accepted, before a goroutine is started for it.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer ln.Close()
	// The owed-view_lost republisher lives exactly as long as Serve, and Serve
	// waits for it: its publishes, like every session's, must have happened
	// by the time Serve returns and the daemon Drains. It has its own stop,
	// because Serve also returns on a dead listener with ctx still live. Once
	// it has returned, closeOwed makes one last attempt and closes the ledger.
	stopOwed := make(chan struct{})
	var owedDone sync.WaitGroup
	owedDone.Go(func() { s.republishOwed(ctx, stopOwed) })
	defer func() {
		close(stopOwed)
		owedDone.Wait()
		s.closeOwed()
	}()
	if s.allowAny {
		s.logger().Warn("allowed_sources is empty: accepting BMP from any address")
	}
	maxConns := s.maxConns
	if maxConns <= 0 {
		maxConns = defaultMaxConnections
	}
	slots := make(chan struct{}, maxConns)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-stopWatch:
		}
	}()

	var wg sync.WaitGroup
	// backoff for transient Accept errors, per net/http's Serve.
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			// Only a genuinely dead listener ends the daemon. Returning on
			// every Accept error meant a transient EMFILE/ENFILE/ECONNABORTED
			// -- exactly what a collector with many concurrent BMP sessions
			// and a modest fd limit hits -- killed the process and dropped
			// every other router's session. One bad accept must not do that.
			if errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return err
			}
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else {
				delay *= 2
			}
			if delay > time.Second {
				delay = time.Second
			}
			s.logger().Warn("accept failed, retrying", "err", err, "retry_in", delay)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				wg.Wait()
				return nil
			}
		}
		delay = 0
		select {
		case slots <- struct{}{}:
		default:
			metricConnectionsRejected.WithLabelValues(rejectMaxConnections).Inc()
			s.logger().Debug("closing connection: max_connections reached",
				"peer", conn.RemoteAddr().String(), "max_connections", maxConns)
			conn.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			s.handleConn(ctx, conn)
		})
	}
}

// quirkIDs converts config-file quirk names (plain strings, so config.go
// doesn't need to import the quirk package) to quirk.ID values.
func quirkIDs(ss []string) []quirk.ID {
	out := make([]quirk.ID, len(ss))
	for i, s := range ss {
		out[i] = quirk.ID(s)
	}
	return out
}

// routerIP resolves the address that identifies this connection's router. It
// is the single place that decision is made.
//
// With proxy_protocol off it is the socket peer, exactly as it has always
// been. With proxy_protocol required it is the source the header asserts, and
// there is deliberately no path here that falls back to the socket address
// when a header is missing or unreadable. A permissive mode would let any
// sender that can reach this port claim any router's identity by writing 28
// bytes -- far cheaper than spoofing a TCP source, which needs an on-path
// position or a won sequence-number race. router_ip is the key the query
// layer scopes current state by, so a wrong value does not raise an error, it
// invents a router or writes into a real one's view.
//
// ok is false when the connection must be closed without a session: a header
// that could not be read, one that deliberately asserts no identity -- a
// gateway's LOCAL health probe, or an UNKNOWN/AF_UNSPEC source -- or a peer
// that sent no header bytes at all, which is a health check or a port scan
// rather than a misconfigured proxy and is counted and logged as such.
func (s *Server) routerIP(conn net.Conn) (netip.Addr, bool) {
	if s.cfg.ProxyProtocol != ProxyProtocolRequired {
		return peerAddr(conn), true
	}
	h, err := proxyproto.Read(conn)
	switch {
	case errors.Is(err, proxyproto.ErrNoHeader):
		metricProxyHeader.WithLabelValues(proxyResultAbsent).Inc()
		s.logger().Warn("closing connection: proxy_protocol is required and this connection sent no header",
			"peer", conn.RemoteAddr().String())
		return netip.Addr{}, false
	case errors.Is(err, proxyproto.ErrNoData):
		// proxyproto.ErrNoData establishes, structurally, that not one byte
		// arrived before the peer closed or the read deadline fired -- that
		// is what an L4 health check and a port scan look like from here, not
		// a disagreement about the format. A peer that sent even a partial
		// header does not land here: it falls through to the malformed case
		// below, where it belongs.
		//
		// It matters that this is not counted as malformed. HAProxy's
		// `server ... send-proxy check` does not send a PROXY header on its
		// own health checks unless check-send-proxy is also set, so the most
		// likely front-end -- correctly configured for traffic -- lands here
		// every check interval, forever. Folding that into "malformed" would
		// make the one counter whose job is to distinguish "the proxy is not
		// configured" from "the proxy is configured and we disagree about the
		// format" read as permanent format disagreement on a healthy system,
		// and put a warn line next to it at the health check's frequency.
		//
		// Whether this was a close or a stall is still available from the
		// wrapped cause, so that distinction is not lost -- it just no longer
		// decides malformed-vs-quiet, only which quiet bucket applies.
		if errors.Is(err, os.ErrDeadlineExceeded) {
			metricProxyHeader.WithLabelValues(proxyResultTimeout).Inc()
			s.logger().Debug("closing connection: no PROXY header before the read deadline",
				"peer", conn.RemoteAddr().String(), "timeout", s.idleReadTimeout)
		} else {
			metricProxyHeader.WithLabelValues(proxyResultClosed).Inc()
			s.logger().Debug("peer connected and closed without sending a header",
				"peer", conn.RemoteAddr().String())
		}
		return netip.Addr{}, false
	case err != nil:
		// Bytes arrived that claimed to be a header and could not be parsed
		// as one, including a header truncated mid-read (io.ReadFull reports
		// that as io.ErrUnexpectedEOF, not io.EOF, so it does not reach the
		// arm above) and a header abandoned after the first byte or more of
		// the 12-byte prefix (proxyproto wraps that as ErrNoData only when
		// zero bytes were read). This is the case worth a warn: something
		// committed to a header and we disagree about it.
		metricProxyHeader.WithLabelValues(proxyResultMalformed).Inc()
		s.logger().Warn("closing connection: unreadable PROXY protocol header",
			"peer", conn.RemoteAddr().String(), "err", err)
		return netip.Addr{}, false
	}
	switch h.Kind {
	case proxyproto.KindLocal:
		// A proxy health-checking its backend. Not an error, and not a
		// session; logged at debug so a per-second check does not fill the log.
		metricProxyHeader.WithLabelValues(proxyResultLocal).Inc()
		s.logger().Debug("closing a LOCAL proxy health probe", "peer", conn.RemoteAddr().String())
		return netip.Addr{}, false
	case proxyproto.KindUnspec:
		metricProxyHeader.WithLabelValues(proxyResultUnspec).Inc()
		s.logger().Warn("closing connection: the PROXY header asserts no source address",
			"peer", conn.RemoteAddr().String())
		return netip.Addr{}, false
	}
	// The switch above has no default, so any Kind the parser might grow
	// later reaches this point. Nothing must return an invalid Addr as an
	// accepted identity: netip.Addr{}.String() is "invalid IP", which becomes
	// RouterId.Ip, passes through sink/rows.go's clickHouseIPv6 unchanged,
	// and fails the driver's IPv6 parse on insert -- and sink/consumer.go
	// deliberately does not ack a failed insert, so that batch redelivers
	// forever and archive ingest stalls for every router, not just this one.
	// proxyproto.Read cannot produce Kind-with-no-Source today; this is what
	// keeps that from becoming a fleet-wide outage if it ever can.
	if !h.Source.IsValid() {
		metricProxyHeader.WithLabelValues(proxyResultMalformed).Inc()
		s.logger().Warn("closing connection: the PROXY header parsed but carries no usable source address",
			"peer", conn.RemoteAddr().String(), "kind", int(h.Kind))
		return netip.Addr{}, false
	}
	metricProxyHeader.WithLabelValues(proxyResultAccepted).Inc()
	// Already unmapped by the parser; see proxyproto.Header.
	return h.Source, true
}

// peerAddr is conn's TCP peer address, unmapped and without its IPv6 zone. A
// zone names an interface on this host, not anything about the peer: keeping
// it would give a link-local router one identity per interface it arrives on,
// and a routers: override keyed on the bare address would never match it.
// proxyproto rejects a zoned source outright for the same reason. The zero
// Addr is returned for a peer address that does not parse, which no
// allowlist contains.
func peerAddr(conn net.Conn) netip.Addr {
	ap, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap().WithZone("")
}

// prefixesContain reports whether addr falls in any of ps.
func prefixesContain(ps []netip.Prefix, addr netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// handleConn owns conn and its Session for the connection's entire life: one
// goroutine, one Session, from accept to close, per this package's
// single-goroutine-ownership contract (session.go's package doc). It never
// panics on wire input (bmp.ReadMsg and Session.Handle already guarantee
// that) and never allows one misbehaving router to affect any other
// connection's goroutine, Session, or Publish calls.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(tcpKeepAlivePeriod)
	}

	// The deadline is set before the PROXY header read, not after it. Reading
	// a header touches the socket, so a peer that connects and then says
	// nothing would otherwise hold this goroutine and its fd on that read for
	// the life of the process -- exactly the case defaultIdleReadTimeout was
	// written to bound. The read loop below clears it once the first BMP
	// message arrives.
	if err := conn.SetReadDeadline(time.Now().Add(s.idleReadTimeout)); err != nil {
		s.logger().Warn("set read deadline", "err", err)
	}

	// trusted_proxies is checked before the PROXY header is read, so an
	// untrusted peer's header is never parsed at all. It checks the TCP peer
	// -- the proxy -- which is the one address here that TCP vouches for.
	if s.cfg.ProxyProtocol == ProxyProtocolRequired && !s.trustAny {
		if peer := peerAddr(conn); !prefixesContain(s.trusted, peer) {
			metricConnectionsRejected.WithLabelValues(rejectProxyNotTrusted).Inc()
			s.logger().Warn("closing connection: peer is not in trusted_proxies",
				"peer", conn.RemoteAddr().String())
			return
		}
	}

	routerIP, ok := s.routerIP(conn)
	if !ok {
		return
	}

	// allowed_sources is checked against the router's address, which is the
	// PROXY header's source when one is required -- checking the socket
	// there would check the proxy. It runs after the header and before the
	// first bmp.ReadMsg, so a disallowed sender gets no session and none of
	// its BMP is parsed.
	if !s.allowAny && !prefixesContain(s.allowed, routerIP) {
		metricConnectionsRejected.WithLabelValues(rejectSourceNotAllowed).Inc()
		s.logger().Warn("closing connection: source is not in allowed_sources",
			"router", routerIP.String(), "peer", conn.RemoteAddr().String())
		return
	}

	// A session opened while NATS is down would stream the router's whole
	// initial dump into the client's reconnect buffer, which overflows within
	// seconds, and every event past that point is lost. The connection is
	// accepted and closed at once rather than refused at the listener: the
	// router gets an immediate FIN and retries on its own reconnect timer, and
	// the refusal is counted and logged here. Not calling Accept at all would
	// leave routers connected in the kernel's backlog, sending into a socket
	// nobody reads, with nothing on this side to say so.
	//
	// Sessions already open are not closed when NATS drops. Their new
	// publishes queue in the reconnect buffer and are delivered if NATS
	// returns within the async publish timeout. Publishes already awaiting
	// their ack when it dropped are failed by the NATS client at once
	// (nats.ErrDisconnected, whether or not the server stored them); the
	// publisher re-sends those with the same msg-id once the connection is
	// back, and JetStream's duplicate window makes a re-send of one it did
	// store a no-op (natsutil/retry.go). Only when a publish fails for good
	// -- rejected, timed out, or not re-sent within the retry bounds -- is
	// the session closed (see sessionSink).
	if cp, ok := s.pub.(ConnectedPublisher); ok && !cp.Connected() {
		metricConnectionsRejected.WithLabelValues(rejectNATSDisconnected).Inc()
		if s.natsDown != nil {
			s.natsDown.record(s.logger(), "router", routerIP.String(), "peer", conn.RemoteAddr().String())
		}
		return
	}

	// Counted here rather than on accept: a LOCAL health probe reaches this
	// function, is closed above, and is not an open BMP session.
	metricSessions.Inc()
	defer metricSessions.Dec()

	ov := s.cfg.Routers[routerIP.String()]
	sessionID := s.nextSessionID()
	sess := NewSession(routerIP, s.cfg.CollectorID, sessionID, s.now, Overrides{
		Force: quirkIDs(ov.ForceQuirks), Disable: quirkIDs(ov.DisableQuirks),
		Vendor: ov.Vendor, OS: ov.OS,
	})

	log := s.logger().With("router", routerIP.String(), "session_id", sessionID)
	log.Info("bmp session open")
	sink := s.newSessionSink(conn, log)
	defer sink.done.Store(true)
	// sess.sysName is only known once (if ever) an Initiation message
	// arrives; reading it here, in the deferred close log, reports whatever
	// value it holds by the time this connection ends -- empty if none ever
	// arrived, which is itself useful operational signal.
	defer func() { log.Info("bmp session closed", "sys_name", sess.sysName) }()
	// Publishing the session's close-out runs BEFORE that log line (defers
	// unwind last-in-first-out), so the log reads as "everything for this
	// session is out".
	//
	// This is the only place a peer's view-lost event can come from. The
	// router sends nothing when the transport drops -- BMP is
	// unidirectional and has no close notification -- so every exit from the
	// read loop below is an exit from this defer: a clean EOF, a malformed
	// message, an idle timeout, a transport error, and the ctx-cancel close
	// on collector shutdown alike. See Session.Close for why the event is
	// not a Peer Down.
	defer func() {
		for _, ev := range sess.Close() {
			sink.publishCloseOut(ev)
		}
		sink.failures.flush(log)
	}()

	// Ctx cancellation forces conn closed so a blocked read wakes up promptly
	// on shutdown instead of waiting out idleReadTimeout.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stopWatch:
		}
	}()

	// The read deadline applies only until the first message arrives, then it
	// is cleared for the life of the connection.
	//
	// It exists to stop a peer that connects and never speaks -- a port scan,
	// a health check, a router wedged before Initiation -- from holding a
	// goroutine and an fd forever. That is a pre-Initiation problem, and
	// bounding it is all this deadline was ever for.
	//
	// Applying it per-read instead tears down healthy sessions. BMP feeds are
	// hours-long and legitimately silent between events: a stable peer set
	// with stats reporting off (the default on several platforms) sends
	// nothing for long stretches. Under a per-read deadline the collector
	// closed such a connection every 5 minutes; the router's next update was
	// then written into a closed socket and lost with no error on either side
	// until the RST, and each cycle forced a new sessionID and a full RIB
	// re-dump. BMP sessions are hours-long and must not be idle-killed,
	// whether by the load balancer in front of the collector or by the
	// collector itself.
	//
	// Dead-peer detection after the first message is TCP keepalive's job
	// (enabled above), which probes without requiring the peer to send
	// anything.
	//
	// The deadline itself is set at the top of this function, before the
	// PROXY header read; only the clearing below still happens here.
	//
	// What does apply for the whole session is a per-message deadline (see
	// defaultMessageReadTimeout): mr arms it when a message's first byte
	// arrives, replacing the handshake deadline if that is still set, and it
	// is cleared once the message is complete. The gap between messages is
	// never under a deadline.
	mr := &messageReader{conn: conn, timeout: s.messageReadTimeout}
	for {
		m, err := bmp.ReadMsg(mr)
		if err == nil && mr.armed {
			mr.armed = false
			if derr := conn.SetReadDeadline(time.Time{}); derr != nil {
				log.Warn("clear read deadline", "err", derr)
			}
		}
		if err != nil {
			// A session closed because a publish failed has already said so;
			// the read error that closing it caused is not news.
			if !sink.failed.Load() {
				logReadErr(log, err, ctx, mr.armed)
			}
			// Per bmp.ReadMsg's contract: io.EOF with no bytes consumed is a
			// clean close at a message boundary (the normal end of a
			// session). Any other error -- ErrBadVersion, ErrBadLength,
			// io.ErrUnexpectedEOF, a timeout, or a transport error -- leaves
			// the reader positioned mid-message; the byte stream cannot be
			// resynchronized, so this connection is closed (via the top-level
			// defer) rather than calling ReadMsg again on it. Either way, one
			// connection's read error never touches any other connection's
			// goroutine or Session.
			return
		}
		metricBMPMessages.WithLabelValues(bmpTypeName(m.Type)).Inc()
		// alreadyRaw tracks whether sess.Handle(m) itself already put m on the
		// raw subject: Handle does that for a parse failure (rawEvent(m, err))
		// and for any message type this collector has no typed event for, e.g.
		// Termination or Route Mirroring (rawEvent(m, nil)). In either case m
		// has already reached the raw stream, so mirroring it too would
		// publish the identical bytes a second time under a different msg-id
		// (rawSeq advances on every rawEvent call, mirrored or not, so
		// JetStream's dedup never catches it) -- silently doubling the raw
		// stream's occupancy for exactly the messages mirror mode's byte
		// budget exists to bound. See MirrorEvent's doc comment.
		alreadyRaw := false
		for _, ev := range sess.Handle(m) {
			if ev.Env.GetRaw() != nil {
				alreadyRaw = true
			}
			sink.publish(ev)
		}
		// An event of this session is lost, and only a fresh session -- and
		// the router's re-dump that comes with it -- can put it back. Stop
		// here rather than keep publishing into a view that is already wrong.
		if sink.failed.Load() {
			return
		}

		// The Initiation is where identity is settled, so it is the one
		// place worth saying out loud what this router was resolved to.
		// Logged once per session rather than per envelope, and the conflict
		// case is a warning because it means the operator's config and the
		// router's own banner disagree -- config wins, but one of them is
		// wrong and neither will say so on its own.
		if m.Type == bmp.TypeInitiation {
			ri := sess.Profile().RouterInfo()
			if sess.IdentityConflict() {
				log.Warn("router identity conflict: configured vendor overrides the banner",
					"configured_vendor", ri.GetVendor(), "sys_descr", ri.GetSysDescr())
			} else {
				log.Info("router identified", "vendor", ri.GetVendor(), "os", ri.GetOs(),
					"version", ri.GetVersion(), "sys_descr", ri.GetSysDescr())
			}
		}

		// Mirror mode: republish the original message verbatim so
		// `vantage capture` can build a corpus from traffic that parses
		// fine and therefore never reaches the raw stream on its own. A
		// message that already went out as a RawEvent above (alreadyRaw) is
		// skipped here for exactly that reason -- it reached the raw stream
		// regardless of mirror mode, so a mirror copy would be a duplicate,
		// not a capture of something that would otherwise have been lost.
		//
		// A failed mirror publish is counted and ignored. The real envelope
		// has already gone out; a diagnostic that can degrade production
		// capture would be worse than no diagnostic.
		if !alreadyRaw && s.Mirrors.Take(routerIP, len(m.Payload)+bmp.HeaderLen) {
			mev := sess.MirrorEvent(m)
			if err := s.pub.Publish(mev); err != nil {
				metricMirrorErrors.Inc()
			} else {
				metricMirrorMessages.Inc()
			}
		}
	}
}

// publishFailureLogInterval is how often at most one session logs that its
// publishes are failing, and how often the refusal of new sessions while
// NATS is disconnected is logged.
const publishFailureLogInterval = 10 * time.Second

// sessionSink is one BMP session's path to the publisher. It counts each
// event, publishes it, and closes the session when a publish fails.
//
// A failed publish is not only an observability fact. Nothing upstream can
// retry it: BMP has no way to ask the router for a message again, so the
// event is gone and the archive's view of this session is wrong from that
// point until the session resets -- which, with the session left up, might be
// weeks. Closing the connection makes the router reconnect under a new
// session ID and send its tables again (on platforms that re-dump on
// reconnect -- see docs/operating.md), which is the only repair there is.
//
// Both kinds of failure close the session: an error Publish returns, and one
// that resolves later and is reported through NotifyingPublisher. One failure
// is enough; the view is already wrong after the first lost event.
type sessionSink struct {
	s    *Server
	conn net.Conn
	log  *slog.Logger

	// onFail is fail as a func value, made once per session so publishing an
	// event does not allocate a closure.
	onFail   func(error)
	failures *rateLimitedLog

	// failed is set by the first failure. done is set when handleConn
	// returns; a late failure after that is logged but closes nothing,
	// because the session is already over.
	failed atomic.Bool
	done   atomic.Bool
}

func (s *Server) newSessionSink(conn net.Conn, log *slog.Logger) *sessionSink {
	k := &sessionSink{s: s, conn: conn, log: log,
		failures: &rateLimitedLog{msg: publishFailedMsg, level: slog.LevelError,
			interval: publishFailureLogInterval, now: s.now}}
	k.onFail = k.fail
	return k
}

// publish is publishWith for the read loop: a failure closes the session.
func (k *sessionSink) publish(ev Event) { k.publishWith(ev, k.onFail) }

// publishCloseOut is publish for the view_lost events Session.Close builds.
// A failure closes nothing new -- the session is already ending -- but the
// event is owed rather than lost: Server republishes it once the publisher is
// connected again (see owed.go).
func (k *sessionSink) publishCloseOut(ev Event) {
	k.publishWith(ev, func(err error) {
		k.fail(err)
		k.s.owed.add(ev)
	})
}

// publishWith counts ev and hands it to the publisher; onFail runs once if the
// publish fails, at the call or later.
//
// It is the only way handleConn publishes a session's events, because it has
// two callers that must not drift: the read loop, and handleConn's close-out
// defer. A close-out that skipped metricEvents would leave the events counter
// disagreeing with the stream over exactly the events the counter is least
// likely to be watched for.
func (k *sessionSink) publishWith(ev Event, onFail func(error)) {
	for _, f := range ev.Env.ParseFlags {
		metricParseFlags.WithLabelValues(f.String()).Inc()
	}
	metricEvents.WithLabelValues(payloadKind(ev)).Inc()
	var err error
	if np, ok := k.s.pub.(NotifyingPublisher); ok {
		err = np.PublishNotify(ev, onFail)
	} else {
		err = k.s.pub.Publish(ev)
	}
	if err != nil {
		metricPublishErrors.Inc()
		onFail(err)
	}
}

// fail records one lost event and, the first time, closes the connection.
// Called from the session's own goroutine for a synchronous failure and from
// the publisher's for a late one; closing a net.Conn concurrently with a Read
// on it is safe, and unblocks that Read.
func (k *sessionSink) fail(err error) {
	k.failures.record(k.log, "err", err)
	if k.done.Load() {
		return
	}
	if k.failed.CompareAndSwap(false, true) {
		metricSessionsAborted.Inc()
		k.conn.Close()
	}
}

const publishFailedMsg = "publish failed; closing the BMP session so the router reconnects and re-sends"

// messageReader arms conn's read deadline when the first byte of a BMP message
// arrives, so the rest of that message has timeout to follow it. handleConn
// clears the deadline and resets armed once bmp.ReadMsg returns the message.
// It relies on ReadMsg reading a message's first byte on its own, which it
// does: the version byte is read and checked before the rest of the header.
type messageReader struct {
	conn    net.Conn
	timeout time.Duration
	armed   bool
}

func (r *messageReader) Read(p []byte) (int, error) {
	n, err := r.conn.Read(p)
	if n > 0 && !r.armed && r.timeout > 0 {
		r.armed = true
		// An error here leaves the message unbounded, which is the behavior
		// before this deadline existed; the Read itself still succeeded.
		_ = r.conn.SetReadDeadline(time.Now().Add(r.timeout))
	}
	return n, err
}

// logReadErr logs a bmp.ReadMsg failure at the appropriate level: silent for
// a clean EOF or a shutdown-triggered close (both expected, routine ends of a
// session), a warning for anything else (a malformed/truncated message, an
// idle-timeout, a stalled message, or a genuine transport error). midMessage
// says whether the read failed after a message had begun, which is what
// tells a stalled message apart from a handshake idle timeout.
func logReadErr(log *slog.Logger, err error, ctx context.Context, midMessage bool) {
	if errors.Is(err, io.EOF) {
		return
	}
	if ctx.Err() != nil {
		// Connection was closed by Serve's own shutdown path, not by the
		// peer or a protocol violation; the resulting error (typically "use
		// of closed network connection") isn't informative.
		return
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if midMessage {
			log.Warn("bmp message not completed within the message read timeout; closing connection")
			return
		}
		log.Warn("bmp read idle timeout; closing connection")
		return
	}
	log.Warn("bmp read error; closing connection", "err", err)
}

// payloadKind returns the metricEvents "payload" label for ev: the concrete
// oneof case its Envelope carries. Bounded to the fixed set of payload kinds
// this schema defines.
func payloadKind(ev Event) string {
	switch {
	case ev.Env.GetRoute() != nil:
		return "route"
	case ev.Env.GetLs() != nil:
		return "ls"
	case ev.Env.GetPeerEvent() != nil:
		return "peer"
	case ev.Env.GetStats() != nil:
		return "stats"
	default:
		return "raw"
	}
}

// metricConnectionsRejected counts BMP connections closed before a session was
// opened for them, by why. Its reason label is always one of the reject*
// constants below, never anything a sender chooses.
var metricConnectionsRejected = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "vantage_collector_connections_rejected_total",
	Help: "BMP connections closed before a session opened, by reason."},
	[]string{"reason"})

// The complete vocabulary of metricConnectionsRejected's reason label.
const (
	// rejectSourceNotAllowed: the router's address is outside allowed_sources.
	rejectSourceNotAllowed = "source_not_allowed"
	// rejectProxyNotTrusted: proxy_protocol is required and the TCP peer is
	// outside trusted_proxies.
	rejectProxyNotTrusted = "proxy_not_trusted"
	// rejectMaxConnections: max_connections were already open.
	rejectMaxConnections = "max_connections"
	// rejectNATSDisconnected: the publisher's NATS connection was down.
	rejectNATSDisconnected = "nats_disconnected"
)

// logger returns s.log, falling back to the default logger. NewServer leaves
// s.log unset, so the default is looked up when a line is written rather than
// captured at construction: a daemon that installs its configured logger with
// slog.SetDefault after building the Server still gets every line through it.
// Tests set s.log to capture output.
func (s *Server) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}
