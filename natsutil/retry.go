package natsutil

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/subjects"
)

// Retrying a publish that failed in flight.
//
// A publish can fail for reasons that say nothing about the message: the
// connection dropped while its PubAck was outstanding (a NATS server
// restarting in lame-duck mode closes its clients), the stream had no leader
// to answer it, a leader stepping down dropped it unanswered, or the client's
// async window was full. Such a publish is re-sent, byte for byte and with
// the same Nats-Msg-Id, instead of being reported failed. See retryClass for
// exactly which errors qualify and why.
//
// Re-sending is safe only while JetStream still remembers the msg-id. A
// retry of a message the server did store (only its ack was lost) is then
// answered as a duplicate and stored nothing; past the stream's duplicate
// window it would be stored a second time. Every stream EnsureStreams
// creates sets that window to streamDuplicateWindow, and every bound below
// keeps the last re-send well inside it.
//
// A re-sent message is stored after messages published later, so the stream
// no longer holds one session's events in the order the router sent them.
// Nothing downstream reads that order. Every current-state answer ranks a
// session's rows by seq, the collector's per-peer-view counter assigned
// before the first publish and carried unchanged by the re-send
// (argMax(..., (seq, stream_seq)) in the queries, ReplacingMergeTree(seq) in
// the current tables); stream_seq only breaks a tie between rows of the SAME
// message, which a re-send cannot create because JetStream stores it once.
// The sink is stateless per message.
const (
	// publishRetryDeadline is how long after the ORIGINAL publish the last
	// re-send may start. It is measured from the first PublishAsync, not
	// from the failure, because the duplicate window runs from when the
	// server stored the message, which cannot be earlier than that call.
	// Half of streamDuplicateWindow leaves the other half as margin for a
	// re-send sent over a live connection to reach the server.
	//
	// That margin does not cover every path. A re-send is only issued once
	// the connection is CONNECTED, but if it drops again straight after, the
	// re-send sits in nats.go's reconnect buffer and is flushed whenever the
	// connection returns -- possibly after the window, when a message the
	// server had already stored is stored a second time, under a new
	// stream_seq. The history table keeps the duplicate row. The route and
	// link-state *_current tables collapse it, being keyed on what the row
	// describes; peer_current keeps it, because stream_seq is in its sort key.
	// Current state is unaffected, since the two rows are the same event at
	// the same seq, but a count over events sees it twice. See "Publish
	// failures" in docs/operating.md.
	//
	// TestRetryBoundsFitTheDuplicateWindow and
	// TestNewPublisherInstallsBoundsInsideTheDuplicateWindow hold the
	// relation.
	publishRetryDeadline = 60 * time.Second

	// publishRetryMaxAttempts bounds re-send attempts per message. Each
	// disconnect the message is caught by costs one; a rolling restart of a
	// three-server cluster disconnects a client at most three times, an ack
	// timeout costs one per timeout (at most five in the deadline with the
	// collector's 10 s timeout), and a leaderless stream one per backoff
	// step. None of those reaches 16 before publishRetryDeadline or
	// backoffRetryWindow. A failure that is answered at once, every time,
	// does: the 409 "in process" answer (see retryClass) exhausts 16
	// attempts in about 28 s of backoff. That is the backstop working as
	// intended, stopping a condition that would otherwise re-send until the
	// deadline.
	publishRetryMaxAttempts = 16

	// backoffRetryWindow bounds how long a message keeps retrying a
	// no-responders failure, measured from the first one. It exists because
	// the same signal also means "no stream captures this subject", which
	// is permanent. It covers a Raft election in nats-server v2.11: a
	// follower waits 4-9 s (minElectionTimeoutDefault..
	// maxElectionTimeoutDefault in server/raft.go) for a lost leader before
	// it campaigns, so 20 s allows one full timeout, a second election
	// round, and the time to answer.
	backoffRetryWindow = 20 * time.Second

	// backoffRetryMin and backoffRetryMax bound the doubling wait before a
	// re-send that is not waiting on a reconnect. nats.go has already
	// retried a no-responders failure twice, 250 ms apart
	// (jetstream.DefaultPubRetryAttempts/Wait), before it reports
	// ErrNoStreamResponse, so the first wait here does not need to be
	// shorter than that.
	backoffRetryMin = 250 * time.Millisecond
	backoffRetryMax = 2 * time.Second

	// maxRetrying bounds the retrying publishes, those between their first
	// retryable failure and their resolution, per stream: while this many
	// publishes to one stream are retrying, a Publish to that stream waits
	// for one to resolve before it sends, for up to retryCapacityWait.
	//
	// Per stream, because one stream's outage must not close sessions that
	// never publish to it. RAW is a single copy on one server, so while that
	// server restarts RAW publishes retry for backoffRetryWindow; one session
	// mirroring a router through a table dump fills a budget in a second or
	// two, and with one shared budget every ROUTES-only session then failed
	// its next publish. With a budget per stream, the RAW outage closes only
	// sessions that publish to RAW. A retrying publish keeps its
	// marshaled bytes, which nats.go dropped when it failed the future, and
	// it no longer counts toward the async window
	// (WithPublishAsyncMaxPending), so without this gate a stream electing
	// a leader under load would retain every event the routers sent until
	// it had one. Measured: an unthrottled publisher against an embedded
	// cluster whose ROUTES leader was not yet reachable passed 4096
	// retrying publishes within a second.
	//
	// Publishes already in flight still enter the retry when they fail, so
	// the bound on retained messages is maxRetrying for each of the five
	// streams, plus the async window (4096 in the collector), plus one first
	// send per publishing goroutine that passed the gate together.
	maxRetrying = 4096

	// retryCapacityWait is how long Publish waits at a full stream's gate
	// before it fails with errRetryCapacity. Waiting is backpressure: the
	// session stops reading and the router's TCP send buffer absorbs the
	// pause. 10 s covers a stream leader election (4-9 s, see
	// backoffRetryWindow).
	//
	// It is paid once per outage, not once per publish: when a wait times
	// out with the stream's budget still full, every later publish to that
	// stream fails at once until a retry resolves and makes room. Without
	// that, a failing session's remaining events -- and its close-out, a
	// view-lost event per peer -- each waited the full 10 s in turn, and a
	// shutdown during an outage outlasted the pod's grace period. Shutdown
	// ends every wait at once (StopWaiting).
	retryCapacityWait = 10 * time.Second

	// connectedRecheck is how often a re-send waiting for the connection
	// reads its status directly, in case the status event is lost (see
	// waitConnected).
	connectedRecheck = 250 * time.Millisecond
)

// retryPolicy is the bounds above, as a value so a test can shrink them.
type retryPolicy struct {
	deadline      time.Duration
	maxAttempts   int
	backoffWindow time.Duration
	backoffMin    time.Duration
	backoffMax    time.Duration
	maxRetrying   int
	capacityWait  time.Duration
}

var defaultRetryPolicy = retryPolicy{
	deadline:      publishRetryDeadline,
	maxAttempts:   publishRetryMaxAttempts,
	backoffWindow: backoffRetryWindow,
	backoffMin:    backoffRetryMin,
	backoffMax:    backoffRetryMax,
	maxRetrying:   maxRetrying,
	capacityWait:  retryCapacityWait,
}

// retryKind is how a failed publish may be retried.
type retryKind int

const (
	// retryNever: report the failure.
	retryNever retryKind = iota
	// retryOnReconnect: re-send as soon as the connection is CONNECTED.
	retryOnReconnect
	// retryAfterBackoff: re-send after a backoff, once CONNECTED.
	retryAfterBackoff
	// retryNoResponders: retryAfterBackoff, and also bounded by
	// backoffWindow.
	retryNoResponders
)

// retryClass says whether err, the failure of one publish, is retried. The
// same classification applies to the first send's synchronous failure and to
// every later one, synchronous or not.
//
// Re-sent as soon as the connection is back:
//
//   - nats.ErrDisconnected. When the connection enters RECONNECTING,
//     nats.go (v1.49.0, jetstream/publish.go resetPendingAcksOnReconnect)
//     fails every publish still awaiting its PubAck with this error, at
//     once. Whether the server stored the message is unknown; either way
//     the same msg-id makes a re-send correct. This is the lame-duck
//     restart case.
//   - nats.ErrReconnectBufExceeded, synchronous: the connection is down and
//     nats.go's reconnect buffer is full.
//
// Re-sent after a backoff, within publishRetryDeadline:
//
//   - jetstream.ErrAsyncPublishTimeout. Nothing answered within the ack
//     timeout. A stream leader that steps down -- as it does when its
//     server enters lame-duck mode -- drops the proposals it had in flight
//     without a reply; measured against an embedded three-server cluster,
//     three stepdowns of the ROUTES leader under load left 30 to 244
//     publishes timing out per run, on a connection that never dropped.
//     NATS's guidance for this is to re-send with the same msg-id. A
//     subject captured by nothing but a subscriber that never replies
//     times out the same way, forever; publishRetryDeadline bounds that.
//   - A 503 "raft: not leader" API error (see isNotLeader): the leader
//     stepped down between accepting the message and proposing it. Seen in
//     the same runs, about once per stepdown.
//   - A 409 "duplicate message id is in process" API error (JetStream error
//     code 10158; see isDuplicateInProcess): a re-send reached a leader that
//     still holds the original's msg-id as staged but not yet stored. Seen
//     in the same runs as a first re-send's answer. It means "not yet", not
//     "no": once the original is stored a re-send is answered as a
//     duplicate, which is success. It is not always transient. nats-server
//     v2.11 stages the msg-id before proposing and does not remove it when
//     the proposal fails (processClusteredInboundMsg); a leader change
//     clears only the newest run of staged entries (processStreamLeaderChange).
//     A leaked entry is purged with the rest of the duplicate window, 2
//     minutes after it was staged -- after publishRetryDeadline, so a
//     publish that meets one exhausts its attempts and is reported failed,
//     which closes its session: the pre-retry outcome, never a silent loss.
//     Seen once, in about twenty runs of the lame-duck test on a heavily
//     loaded machine. nats-server v2.15 records the msg-id only after a
//     successful proposal, so the leak does not arise there.
//   - jetstream.ErrTooManyStalledMsgs, synchronous: the async window
//     (WithPublishAsyncMaxPending) stayed full for 200 ms. That happens
//     when acks stop arriving, which is exactly during a leader change or a
//     reconnect burst.
//
// Re-sent after a backoff, within backoffRetryWindow as well:
//
//   - jetstream.ErrNoStreamResponse. nats.go reports it for a publish
//     answered with the no-responders status after its own two retries. In
//     an R3 cluster that is what a stream with no leader looks like: only
//     the leader subscribes to the stream's subjects (server/stream.go
//     setLeader), so between losing one leader and electing the next
//     nothing answers. Measured against an embedded three-server cluster:
//     a hard shutdown of the stream leader under load failed 66 in-flight
//     publishes with exactly this error. The same signal also means that no
//     stream captures the subject, which is permanent; backoffRetryWindow
//     bounds that case, so it is still reported, 20 s later than before.
//     nats.ErrNoResponders is not listed: the async path never returns it,
//     having converted it to ErrNoStreamResponse.
//
// Never retried, because the answer would be the same:
//
//   - Every other JetStream API error in the PubAck, 409s other than 10158
//     included: a stream full under
//     DiscardNew, a message over the size limit, a wrong expected
//     sequence, insufficient resources, and a 503 whose description is
//     anything but "raft: not leader" (the same 503 carries a storage
//     write error).
//   - nats.ErrConnectionClosed: the connection will not come back.
//   - Any other synchronous error: a bad subject, a payload over the
//     server's max_payload, a marshal failure.
func retryClass(err error) retryKind {
	switch {
	case errors.Is(err, nats.ErrDisconnected), errors.Is(err, nats.ErrReconnectBufExceeded):
		return retryOnReconnect
	case errors.Is(err, jetstream.ErrNoStreamResponse):
		return retryNoResponders
	case errors.Is(err, jetstream.ErrAsyncPublishTimeout), errors.Is(err, jetstream.ErrTooManyStalledMsgs),
		isNotLeader(err), isDuplicateInProcess(err):
		return retryAfterBackoff
	}
	return retryNever
}

// isNotLeader reports whether err is the PubAck a clustered stream sends when
// its Raft node refuses a proposal because it is no longer the leader:
// ApiError{Code: 503, Description: err.Error()} for raft's errNotLeader, with
// no JetStream error code (nats-server v2.11, server/jetstream_cluster.go
// processClusteredInboundMsg and server/raft.go). The description is matched
// exactly because the same 503 also carries a storage write error.
func isNotLeader(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.Code == 503 && apiErr.ErrorCode == 0 &&
		apiErr.Description == "raft: not leader"
}

// jsErrDuplicateInProcess is JetStream's JSStreamDuplicateMessageConflict
// (nats-server v2.11, server/jetstream_errors_generated.go), which nats.go
// has no constant for.
const jsErrDuplicateInProcess jetstream.ErrorCode = 10158

// isDuplicateInProcess reports whether err is the PubAck a clustered stream
// sends for a msg-id it has staged for an uncommitted proposal: "duplicate
// message id is in process".
func isDuplicateInProcess(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jsErrDuplicateInProcess
}

// Reasons a retryable failure was reported anyway. Each is joined with the
// last failure, so errors.Is still finds nats.ErrDisconnected and the like.
var (
	errRetryDeadline = errors.New("retry deadline passed")
	errRetryAttempts = errors.New("retry attempts exhausted")
	errRetryWindow   = errors.New("backoff retry window passed")
	errRetryCapacity = errors.New("too many publishes already retrying")
)

// resolve returns nil once m is accepted (or deduplicated -- PubAck.Duplicate
// is not an error: the whole point of MsgID is that a re-send of the same
// event is expected and harmless), else the error to report. err is the
// first send's synchronous failure, in which case future is nil; otherwise
// resolve waits for future. While the failure is retryable and the bounds
// allow, it re-sends m and waits again.
func (p *Publisher) resolve(m *outgoing, future jetstream.PubAckFuture, err error) error {
	if err == nil {
		err = wait(future)
	}
	if err == nil {
		return nil
	}
	defer p.leaveRetry(m)
	attempts, resends := 0, 0
	var noRespondersSince time.Time
	for {
		kind := retryClass(err)
		if kind == retryNever {
			if attempts == 0 {
				return fmt.Errorf("natsutil: publish %s (msgid %s): %w", m.subject, m.msgID, err)
			}
			return m.gaveUp(resends, err, nil)
		}
		deadline, past := m.start.Add(p.retry.deadline), errRetryDeadline
		if kind == retryNoResponders {
			if noRespondersSince.IsZero() {
				noRespondersSince = time.Now()
			}
			if w := noRespondersSince.Add(p.retry.backoffWindow); w.Before(deadline) {
				deadline, past = w, errRetryWindow
			}
		}
		switch {
		case attempts >= p.retry.maxAttempts:
			return m.gaveUp(resends, err, errRetryAttempts)
		case !time.Now().Before(deadline):
			return m.gaveUp(resends, err, past)
		}
		p.enterRetry(m, err)
		attempts++
		var backoff time.Duration
		if kind != retryOnReconnect {
			backoff = p.retry.backoff(attempts)
		}
		next, rerr := p.resend(m, backoff, deadline, past)
		switch {
		case rerr == nil:
			resends++
			if p.OnRetry != nil {
				p.OnRetry(fmt.Errorf("natsutil: publish %s (msgid %s): re-sent (re-send %d) after: %w", m.subject, m.msgID, resends, err))
			}
			if err = wait(next); err == nil {
				return nil
			}
		case errors.Is(rerr, errRetryDeadline), errors.Is(rerr, errRetryWindow), errors.Is(rerr, nats.ErrConnectionClosed):
			// Never re-sent: err is still the failure being retried.
			return m.gaveUp(resends, err, rerr)
		default:
			// The re-send failed synchronously. That is classified like
			// any other failure on the next pass.
			err = rerr
		}
	}
}

// retryBudget is one stream's share of maxRetrying.
type retryBudget struct {
	n int // publishes to this stream now retrying
	// room is non-nil only while a publish waits for room; leaveRetry
	// closes it when there is some.
	room chan struct{}
	// saturated is set when a wait timed out with the budget still full,
	// and cleared when room appears. While it is set, publishes fail at once
	// instead of each waiting capacityWait in turn.
	saturated bool
}

// streamOf names the budget a subject's retries count against: the subject
// type token that selects one of the five streams EnsureStreams creates, or
// otherStreams for any other subject. Unknown subjects share one budget so
// the set of budgets stays bounded whatever subjects a caller publishes to.
func streamOf(subject string) string {
	if rest, ok := strings.CutPrefix(subject, subjects.Prefix+"."); ok {
		tok, _, _ := strings.Cut(rest, ".")
		switch tok {
		case "route", "ls", "peer", "stats", "raw":
			return tok
		}
	}
	return otherStreams
}

// otherStreams is the one budget every subject outside the five streams
// shares.
const otherStreams = ""

// budgetLocked returns the budget for stream, creating it. p.mu must be
// held.
func (p *Publisher) budgetLocked(stream string) *retryBudget {
	b := p.budgets[stream]
	if b == nil {
		if p.budgets == nil {
			p.budgets = map[string]*retryBudget{}
		}
		b = &retryBudget{}
		p.budgets[stream] = b
	}
	return b
}

// enterRetry records m as retrying after err. The first time, it counts m
// against its stream's budget.
func (p *Publisher) enterRetry(m *outgoing, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retrying == nil {
		p.retrying = map[*outgoing]error{}
	}
	if _, ok := p.retrying[m]; !ok {
		p.budgetLocked(m.stream).n++
	}
	p.retrying[m] = err
}

// leaveRetry forgets m once it is resolved, and wakes publishes waiting for
// room in its stream's budget.
func (p *Publisher) leaveRetry(m *outgoing) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.retrying[m]; !ok {
		return
	}
	delete(p.retrying, m)
	b := p.budgetLocked(m.stream)
	b.n--
	if b.n < p.retry.maxRetrying {
		b.saturated = false
		if b.room != nil {
			close(b.room)
			b.room = nil
		}
	}
}

// waitRetryRoom returns nil once fewer than maxRetrying publishes to stream
// are retrying. It returns errRetryCapacity after capacityWait, at once if
// an earlier wait already timed out and there has been no room since, and at
// once when StopWaiting has been called.
func (p *Publisher) waitRetryRoom(stream string) error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		p.mu.Lock()
		b := p.budgetLocked(stream)
		if b.n < p.retry.maxRetrying {
			p.mu.Unlock()
			return nil
		}
		if b.saturated || p.stopWaiting {
			p.mu.Unlock()
			return errRetryCapacity
		}
		if b.room == nil {
			b.room = make(chan struct{})
		}
		room, stop := b.room, p.stopped
		p.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(p.retry.capacityWait)
		}
		select {
		case <-room:
		case <-stop:
			return errRetryCapacity
		case <-timer.C:
			p.mu.Lock()
			if b.n >= p.retry.maxRetrying {
				b.saturated = true
			}
			p.mu.Unlock()
			return errRetryCapacity
		}
	}
}

// StopWaiting ends every wait for retry room and makes every later one fail
// at once. It is for shutdown: a publish waiting for room holds its session's
// goroutine, and the collector's Serve waits for every session goroutine to
// return before Drain can report anything. It does not stop retries already
// under way or publishes that find room; Drain still accounts for those.
func (p *Publisher) StopWaiting() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopWaiting {
		p.stopWaiting = true
		close(p.stopped)
	}
}

// backoff is the wait before the n-th re-send attempt (n from 1) of a
// failure that is not waiting on a reconnect: backoffMin, doubling, capped at
// backoffMax.
func (r retryPolicy) backoff(n int) time.Duration {
	d := r.backoffMin
	for i := 1; i < n && d < r.backoffMax; i++ {
		d *= 2
	}
	return min(d, r.backoffMax)
}

// resend waits backoff, then until the connection is CONNECTED, then
// re-publishes m with its original msg-id. It gives up with past if deadline
// comes first, and with nats.ErrConnectionClosed if the connection closes: a
// closed connection never reconnects, so there is nothing to wait for.
func (p *Publisher) resend(m *outgoing, backoff time.Duration, deadline time.Time, past error) (jetstream.PubAckFuture, error) {
	if backoff > 0 {
		if !time.Now().Add(backoff).Before(deadline) {
			return nil, past
		}
		time.Sleep(backoff)
	}
	if err := waitConnected(p.js.Conn(), deadline); err != nil {
		if errors.Is(err, errRetryDeadline) {
			return nil, past
		}
		return nil, err
	}
	return p.js.PublishAsync(m.subject, m.data, jetstream.WithMsgID(m.msgID))
}

// waitConnected returns nil once nc is CONNECTED, nats.ErrConnectionClosed
// if it closes, or errRetryDeadline at deadline.
//
// It wakes on the status listener and also reads the status every
// connectedRecheck, because the listener is not reliable: nats.go v1.49.0's
// sendStatusEvent, finding a listener's previous event still unread, reads
// it off the channel and unregisters the listener as if it were closed. Two
// status changes in quick succession -- a reconnect that drops again at once
// -- can therefore leave nothing to wake this function, and without the
// recheck it would wait until the deadline with the connection long back.
func waitConnected(nc *nats.Conn, deadline time.Time) error {
	ch := statusListener(nc)
	defer nc.RemoveStatusListener(ch)
	recheck := time.NewTicker(connectedRecheck)
	defer recheck.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		switch {
		case nc.IsConnected():
			return nil
		case nc.IsClosed():
			return nats.ErrConnectionClosed
		}
		select {
		case <-ch:
		case <-recheck.C:
		case <-timer.C:
			return errRetryDeadline
		}
	}
}

// statusListener registers the listener waitConnected wakes on. A variable
// so a test can substitute one that never fires, which is what a listener
// nats.go has dropped looks like.
var statusListener = func(nc *nats.Conn) chan nats.Status {
	return nc.StatusChanged(nats.CONNECTED, nats.CLOSED)
}

// gaveUp is the error for m after resends re-sends: the last failure, and
// the reason no further re-send was made when that is not the failure
// itself.
func (m *outgoing) gaveUp(resends int, last, reason error) error {
	if reason == nil {
		return fmt.Errorf("natsutil: publish %s (msgid %s): failed after %d re-sends: %w", m.subject, m.msgID, resends, last)
	}
	return fmt.Errorf("natsutil: publish %s (msgid %s): failed after %d re-sends, %w: %w", m.subject, m.msgID, resends, reason, last)
}

// wait blocks until future resolves and returns its error, nil if accepted.
func wait(future jetstream.PubAckFuture) error {
	select {
	case err := <-future.Err():
		return err
	case <-future.Ok():
		return nil
	}
}
