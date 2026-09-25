package natsutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/collector"
)

// maxRetainedErrs bounds how many individual publish errors a Publisher
// holds between Drains. During a NATS outage a collector republishing a full
// route dump can reject tens of thousands of messages per second, and
// retaining one formatted error per message would consume hundreds of
// megabytes exactly when the system is already degraded. Past this many, the
// count is kept but the errors are not, and Drain reports the overflow.
const maxRetainedErrs = 64

// Publisher publishes collector.Events onto JetStream, deduplicated by
// MsgID via the streams' 2-minute duplicate window (see EnsureStreams).
//
// A publish that fails in flight for a reason that says nothing about the
// message -- the connection dropped before its ack arrived, or the stream
// had no leader -- is re-sent with the same MsgID rather than reported
// failed, within bounds that keep it inside that duplicate window. See
// retry.go.
//
// All methods are safe for concurrent use, including Publish concurrently
// with Drain. Note that this rules out sync.WaitGroup for the outstanding-
// publish accounting: WaitGroup forbids Add running concurrently with Wait
// and panics ("WaitGroup is reused before previous Wait has returned") when
// it does, which a daemon publishing from per-session goroutines while a
// shutdown or periodic-flush path drains would hit almost immediately.
// Outstanding publishes are tracked with an explicit counter and a
// close-on-idle channel instead.
type Publisher struct {
	js jetstream.JetStream

	// OnError, if set, is called once per publish the *server* rejected, at
	// the moment that rejection resolves. Set it before the first Publish;
	// it is read without synchronization.
	//
	// Drain alone is not enough for a long-running daemon. Everything the
	// server rejects -- a subject captured by no stream, a clustered
	// consistency error, an async publish timeout -- resolves here rather
	// than at the Publish call, so without this hook a collector that has
	// been failing to store an entire stream for weeks exposes no signal
	// until it is restarted and Drain finally runs. Called from the await
	// goroutine, so it must be cheap and safe for concurrent use (a
	// Prometheus counter is the intended consumer).
	OnError func(error)

	// OnRetry, if set, is called each time a publish that failed in flight
	// is about to be re-sent, with the failure that caused it. It has the
	// same constraints as OnError, and like it must be set before the first
	// Publish. A retry that later succeeds calls nothing else; one that is
	// given up on reaches OnError and the publish's onFail as before.
	OnRetry func(error)

	retry retryPolicy

	mu       sync.Mutex
	inflight int
	// retrying holds each publish between its first retryable failure and
	// its resolution, with its latest failure, for maxRetrying and for
	// Drain's timeout report.
	retrying map[*outgoing]error
	// budgets is each stream's count of retrying publishes (see
	// maxRetrying).
	budgets map[string]*retryBudget
	// stopped is closed, and stopWaiting set, by StopWaiting.
	stopped     chan struct{}
	stopWaiting bool
	// idle is non-nil only while at least one Drain is waiting. The last
	// await to bring inflight to zero closes it and clears it, so every
	// waiter wakes and the next Drain starts from a fresh channel.
	idle    chan struct{}
	errs    []error
	dropped int
}

// The collector finds these by type assertion on the EventPublisher it is
// given, so a signature drift here would not fail to compile anywhere else:
// the collector would silently stop closing sessions on async failures and
// stop refusing sessions while NATS is down. These make it fail here instead.
var (
	_ collector.NotifyingPublisher = (*Publisher)(nil)
	_ collector.ConnectedPublisher = (*Publisher)(nil)
	_ collector.BeatPublisher      = (*Publisher)(nil)
)

// NewPublisher wraps js for publishing. js should generally already have
// streams provisioned via EnsureStreams.
//
// Callers should construct js with jetstream.WithPublishAsyncTimeout: it
// defaults to zero (no timeout), so a future that is neither acked nor
// resolved by the client's reconnect path never resolves, keeping this
// Publisher's inflight count above zero and making every subsequent Drain
// time out for the life of the process.
func NewPublisher(js jetstream.JetStream) *Publisher {
	return &Publisher{js: js, retry: defaultRetryPolicy, stopped: make(chan struct{})}
}

// Publish marshals ev.Env and publishes it to ev.Subject asynchronously
// (PublishAsync), tagged with ev.MsgID as the JetStream dedup key.
//
// PublishAsync's own return error only reports failures detectable
// synchronously and locally (a bad publish option, for instance) -- it does
// NOT mean the server accepted the message, because the actual accept/reject
// decision is made by the server and reported back later, asynchronously, on
// the *PubAckFuture PublishAsync also returns. A naive Publish
// discarded that future entirely ("_, err = p.js.PublishAsync(...)"), which
// would silently drop any publish the server itself rejected -- e.g. a
// message routed to a subject with no bound stream, or (in a clustered
// deployment) a consistency error -- while Publish still reported success
// and Drain still returned nil, because PublishAsyncComplete's channel closes
// once every outstanding publish has *resolved*, successfully or not, and
// says nothing about which. That is exactly the "publish failure surfaces to
// the caller" requirement this type exists to satisfy, so every future this
// method issues is tracked and its eventual result is what Drain reports.
func (p *Publisher) Publish(ev collector.Event) error { return p.PublishNotify(ev, nil) }

// PublishNotify is Publish, plus onFail: if the server later rejects this
// particular publish, or it times out unresolved, or it fails in flight and
// cannot be re-sent within the retry bounds, onFail is called once with that
// error from the goroutine that observed it -- after Publish has long since
// returned. OnError still runs as well; onFail does not replace it. A publish
// re-sent successfully calls neither.
//
// It exists so a failure that resolves asynchronously can be attributed to
// the BMP session that produced the event. The collector closes that session
// so the router reconnects and re-sends what was lost (see
// collector.NotifyingPublisher). onFail must be cheap and safe for concurrent
// use, like OnError. A nil onFail is Publish.
//
// A non-nil return is a synchronous failure, and onFail is not called for it:
// the caller already has the error. A synchronous failure retryClass
// retries -- the async window full, or the reconnect buffer full -- is not
// returned: the publish is retried like one that failed in flight, and its
// fate reported through onFail like theirs. While maxRetrying publishes to
// ev's stream are retrying, it first waits for one to resolve, for up to
// retryCapacityWait, and returns errRetryCapacity if none does (at once if a
// previous wait already timed out with no room since, or after StopWaiting).
func (p *Publisher) PublishNotify(ev collector.Event, onFail func(error)) error {
	data, err := proto.Marshal(ev.Env)
	if err != nil {
		return fmt.Errorf("natsutil: marshal %s: %w", ev.Subject, err)
	}
	stream := streamOf(ev.Subject)
	if err := p.waitRetryRoom(stream); err != nil {
		return fmt.Errorf("natsutil: publish %s (msgid %s): %w", ev.Subject, ev.MsgID, err)
	}
	m := &outgoing{subject: ev.Subject, msgID: ev.MsgID, stream: stream, data: data, start: time.Now()}
	future, err := p.js.PublishAsync(m.subject, m.data, jetstream.WithMsgID(m.msgID))
	if err != nil && retryClass(err) == retryNever {
		return fmt.Errorf("natsutil: publish %s (msgid %s): %w", ev.Subject, ev.MsgID, err)
	}
	p.mu.Lock()
	p.inflight++
	p.mu.Unlock()
	go p.await(m, future, err, onFail)
	return nil
}

// ErrNotConnected is what PublishOnce reports while the NATS connection is
// down.
var ErrNotConnected = errors.New("natsutil: not connected to NATS")

// PublishOnce publishes ev and waits for JetStream's ack, with no retry of any
// kind. It is for a message that is worth nothing late: a collector heartbeat,
// whose next copy is 30 s away and supersedes this one.
//
// The caller's ctx must carry a short deadline: PublishOnce imposes none of
// its own, so without one a stalled or non-answering NATS server blocks this
// call -- and everything waiting on it -- indefinitely.
//
// Everything Publish does to keep a message from being lost is wrong for such
// a message:
//
//   - A beat waiting for room in the stream's retry budget would hold the
//     heartbeat behind route publishes.
//   - A beat still retrying at shutdown would hold Drain for up to a minute
//     over a message nobody needs.
//
// So this bypasses all of it, and the two retries beneath it too.
//
//   - While the connection is down it returns ErrNotConnected at once instead
//     of handing the message to nats.go's reconnect buffer, which would deliver
//     it whenever the connection came back.
//   - It turns off jetstream's own no-responders retry (WithRetryAttempts(0)).
//   - It is synchronous and never counted in inflight, so Drain neither waits
//     for it nor reports it, and OnError and OnRetry never see it.
//
// A failure is the caller's to count and drop.
func (p *Publisher) PublishOnce(ctx context.Context, ev collector.Event) error {
	if !p.Connected() {
		return fmt.Errorf("natsutil: publish %s: %w", ev.Subject, ErrNotConnected)
	}
	data, err := proto.Marshal(ev.Env)
	if err != nil {
		return fmt.Errorf("natsutil: marshal %s: %w", ev.Subject, err)
	}
	opts := []jetstream.PublishOpt{jetstream.WithRetryAttempts(0)}
	if ev.MsgID != "" {
		opts = append(opts, jetstream.WithMsgID(ev.MsgID))
	}
	if _, err := p.js.Publish(ctx, ev.Subject, data, opts...); err != nil {
		return fmt.Errorf("natsutil: publish %s (msgid %s): %w", ev.Subject, ev.MsgID, err)
	}
	return nil
}

// outgoing is one message a Publisher is responsible for until it resolves:
// everything a re-send needs, and when it was first sent.
type outgoing struct {
	subject, msgID string
	stream         string // streamOf(subject): the retry budget it counts against
	data           []byte
	start          time.Time
}

// await blocks until m is resolved -- accepted, or failed with nothing left
// to retry -- and, if it failed, records the error for the next Drain to
// report. m stays counted in inflight throughout, retries included, which is
// what makes Drain wait for a retry rather than report around it.
func (p *Publisher) await(m *outgoing, future jetstream.PubAckFuture, sendErr error, onFail func(error)) {
	perr := p.resolve(m, future, sendErr)

	if perr != nil && p.OnError != nil {
		// Outside the lock: OnError is caller code and must not be able to
		// deadlock the publisher by touching it.
		p.OnError(perr)
	}
	if perr != nil && onFail != nil {
		onFail(perr)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if perr != nil {
		if len(p.errs) < maxRetainedErrs {
			p.errs = append(p.errs, perr)
		} else {
			p.dropped++
		}
	}
	p.inflight--
	if p.inflight == 0 && p.idle != nil {
		close(p.idle)
		p.idle = nil
	}
}

// takeErrsLocked drains the accumulated errors. p.mu must be held.
func (p *Publisher) takeErrsLocked() []error {
	errs := p.errs
	if p.dropped > 0 {
		errs = append(errs, fmt.Errorf("natsutil: %d further publish errors not retained (cap %d)", p.dropped, maxRetainedErrs))
	}
	p.errs, p.dropped = nil, 0
	return errs
}

// Connected reports whether the underlying NATS connection is currently
// connected. While it is not, new publishes queue in the client's reconnect
// buffer and are lost if that fills or the connection does not come back
// within the async publish timeout, and publishes that were awaiting their
// ack when it dropped wait to be re-sent (see retry.go); the collector
// refuses new BMP sessions in that state rather than accept a full RIB dump
// it has nowhere to put.
func (p *Publisher) Connected() bool {
	nc := p.js.Conn()
	return nc != nil && nc.IsConnected()
}

// Drain waits up to timeout until no publish is outstanding, then returns an
// aggregate (errors.Join) of every publish failure since the previous Drain
// -- nil only if every publish that resolved in that time was accepted or
// deduplicated by the server, not merely that the wait finished. It waits
// for the publisher to go idle, not for a snapshot of the publishes issued
// before it was called: a caller still publishing without pause can keep it
// from ever going idle, and Drain then times out even though every earlier
// publish resolved. Shutdown stops publishing first for that reason. A non-nil error here is the caller's only
// signal that a publish silently accepted by PublishAsync was in fact
// rejected by JetStream; do not treat a nil Publish return alone as "this
// event is durably stored."
//
// A publish being retried is still unresolved: Drain waits for the retry's
// outcome, not for its first failure.
//
// On timeout the accumulated publish errors are reported alongside the
// timeout rather than discarded, so a caller that gives up waiting still
// learns which publishes had already failed. The timeout error says how many
// were still unresolved and how many of those were retrying, and each
// retrying one (up to maxRetainedErrs) is reported with the failure it is
// retrying, so errors.Is finds that failure. Each unresolved publish still
// resolves later -- once the connection is closed, a retry waiting for it
// fails with nats.ErrConnectionClosed -- and reaches OnError, its onFail, and
// the next Drain.
//
// Calling Publish concurrently with Drain is safe.
func (p *Publisher) Drain(timeout time.Duration) error {
	p.mu.Lock()
	if p.inflight == 0 {
		errs := p.takeErrsLocked()
		p.mu.Unlock()
		return errors.Join(errs...)
	}
	if p.idle == nil {
		p.idle = make(chan struct{})
	}
	idle := p.idle
	p.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var timedOut error
	select {
	case <-idle:
	case <-timer.C:
		p.mu.Lock()
		timedOut = fmt.Errorf("natsutil: publish drain timed out after %v with %d publishes unresolved, %d of them retrying",
			timeout, p.inflight, len(p.retrying))
		stillRetrying := p.retryingErrsLocked()
		p.mu.Unlock()
		timedOut = errors.Join(append([]error{timedOut}, stillRetrying...)...)
	}

	p.mu.Lock()
	errs := p.takeErrsLocked()
	p.mu.Unlock()
	if timedOut != nil {
		errs = append(errs, timedOut)
	}
	return errors.Join(errs...)
}

// retryingErrsLocked describes up to maxRetainedErrs publishes still
// retrying, each with the failure it is retrying. p.mu must be held.
func (p *Publisher) retryingErrsLocked() []error {
	var errs []error
	for m, err := range p.retrying {
		if len(errs) == maxRetainedErrs {
			break
		}
		errs = append(errs, fmt.Errorf("natsutil: publish %s (msgid %s): still retrying after: %w", m.subject, m.msgID, err))
	}
	return errs
}
