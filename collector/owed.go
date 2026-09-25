package collector

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// maxOwedViewLost caps the view_lost events a collector holds for
// republishing. A view is one RIB stream of one peer, and an event is a few
// hundred bytes, so this is room for every peer of a very large fleet --
// maxPeersPerSession is 10,000 per session -- in tens of megabytes. Past it,
// an event is dropped and counted. While the process keeps running, the peers
// of a dropped event read up until their router opens a new session with this
// collector. If the process then dies, the read side's epoch rule covers those
// sessions at restart; if it never restarts, the stale rule covers them.
const maxOwedViewLost = 50000

// defaultOwedRetryInterval is how often Serve tries to republish owed events.
// A try while the publisher reports disconnected costs one method call.
const defaultOwedRetryInterval = time.Second

var (
	metricOwedViewLost = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vantage_collector_owed_view_lost",
		Help: "view_lost events whose publish failed, held until NATS is reachable again. " +
			"An event handed back to the publisher is not counted while that republish is in flight."})
	metricOwedViewLostDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_owed_view_lost_dropped_total",
		Help: "view_lost events dropped because the owed ledger was full or the collector was shutting down."})
	metricOwedViewLostResent = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_owed_view_lost_resent_total",
		Help: "Republish attempts of owed view_lost events after NATS returned; one event can be attempted more than once."})
	metricOwedViewLostRepublishFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vantage_collector_owed_view_lost_republish_failed_total",
		Help: "Republish attempts of owed view_lost events that failed, each of which is owed again."})
)

// owedLedger holds the view_lost events a session's close-out could not
// publish.
//
// Session.Close is the only source of view_lost, and it runs when a session
// ends -- including the session closed because a publish failed, which is
// exactly when NATS is least able to take the close-out too. Without this, a
// router that never reconnects leaves its peers reading up forever while the
// collector that knows better is alive and holding the answer.
//
// Each event is kept exactly as Close built it: its seq, session and msg-id
// are what make it resolve as a close that had succeeded, and what let
// JetStream drop it as a duplicate if the first attempt did land after all --
// within the streams' 2-minute duplicate window. A republish later than that,
// of an event the server had stored while every ack was lost, is stored a
// second time under a new stream_seq, and both peer_events and peer_current
// keep both rows: stream_seq is in both tables' sort keys. State still
// resolves the same, since both rows are the same view_lost at the same seq,
// but anything counting peer events counts it twice.
//
// At shutdown Serve makes one last attempt and then closes the ledger (see
// closeOwed): whatever it still holds, and whatever fails after, is dropped
// and counted.
type owedLedger struct {
	mu     sync.Mutex
	events []Event
	max    int
	closed bool
}

func newOwedLedger(max int) *owedLedger { return &owedLedger{max: max} }

// add holds ev, or drops and counts it when the ledger is full. Safe from any
// goroutine: a late publish failure arrives on the publisher's.
func (l *owedLedger) add(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || len(l.events) >= l.max {
		metricOwedViewLostDropped.Inc()
		return
	}
	l.events = append(l.events, ev)
	metricOwedViewLost.Set(float64(len(l.events)))
}

// take empties the ledger and returns what it held, oldest first.
func (l *owedLedger) take() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.events
	l.events = nil
	metricOwedViewLost.Set(0)
	return out
}

// close drops and counts every event the ledger holds, returning how many,
// and makes every later add drop and count its event too.
func (l *owedLedger) close() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.events)
	metricOwedViewLostDropped.Add(float64(n))
	l.events, l.closed = nil, true
	metricOwedViewLost.Set(0)
	return n
}

func (l *owedLedger) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// republishOwed tries the ledger every owedRetryInterval until ctx is done or
// stop is closed. Serve runs it and waits for it, so every publish it makes
// has happened by the time Serve returns and the daemon Drains.
func (s *Server) republishOwed(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(s.owedRetryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			s.flushOwed()
		}
	}
}

// flushOwed hands every owed event back to the publisher, if it is connected.
// Each goes through the ordinary publish path, in-flight retries included;
// one that fails again, synchronously or later, is owed again.
func (s *Server) flushOwed() {
	if s.owed.len() == 0 {
		return
	}
	if cp, ok := s.pub.(ConnectedPublisher); ok && !cp.Connected() {
		return
	}
	for _, ev := range s.owed.take() {
		reowe := func(error) {
			metricOwedViewLostRepublishFailed.Inc()
			s.owed.add(ev)
		}
		var err error
		if np, ok := s.pub.(NotifyingPublisher); ok {
			err = np.PublishNotify(ev, reowe)
		} else {
			err = s.pub.Publish(ev)
		}
		if err != nil {
			reowe(err)
			continue
		}
		metricOwedViewLostResent.Inc()
	}
}

// closeOwed is the ledger's end, run by Serve after every session and the
// republisher have returned, so before the daemon Drains.
//
// It makes one last attempt, if the publisher is connected: the close-outs of
// the sessions shutdown itself ended are among what it may hold. Those
// publishes are ordinary ones, so Drain waits for them within its own bound,
// and once StopWaiting has been called none of them waits for room in a full
// retry budget. Then the ledger closes. What it still holds -- NATS was down,
// or a publish failed at the call -- and every failure that arrives later, is
// dropped and counted: nothing is left to republish it. The read side still
// resolves those peers, view_lost once this collector restarts and stale if
// it never does.
func (s *Server) closeOwed() {
	s.flushOwed()
	if n := s.owed.close(); n > 0 {
		s.logger().Warn("dropping view_lost events owed at shutdown; the read side marks those peers "+
			"view_lost when this collector restarts, stale if it does not", "count", n)
	}
}
