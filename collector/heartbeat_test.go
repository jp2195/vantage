package collector

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/subjects"
)

// beatRecorder is a BeatPublisher that records every attempt and fails the
// first `fail` of them.
type beatRecorder struct {
	mu   sync.Mutex
	evs  []Event
	fail int
}

func (b *beatRecorder) PublishOnce(_ context.Context, ev Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.evs = append(b.evs, ev)
	if b.fail > 0 {
		b.fail--
		return errors.New("nats: no responders available for request")
	}
	return nil
}

func (b *beatRecorder) get() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Event{}, b.evs...)
}

// blockingPublisher is a BeatPublisher standing in for a stalled or
// non-answering NATS: it never resolves on its own and only returns once its
// ctx is done, exactly like a real jetstream client waiting on an ack that
// never arrives.
type blockingPublisher struct{}

func (blockingPublisher) PublishOnce(ctx context.Context, _ Event) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestBeatCarriesTheProcessStartAndItsOwnClock: started_at is the process
// start the caller captured, to the nanosecond, and ts_collector is the
// moment the beat was built. The two differ in this fixture, so swapping them
// fails.
func TestBeatCarriesTheProcessStartAndItsOwnClock(t *testing.T) {
	started := time.Date(2026, 9, 23, 10, 0, 0, 123456789, time.UTC)
	now := started.Add(42 * time.Second)
	rec := &beatRecorder{}
	hb := NewHeartbeat("dev-c1", started, rec, func() time.Time { return now })
	if err := hb.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := rec.get()
	if len(evs) != 1 {
		t.Fatalf("%d publishes, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Subject != subjects.Beat("dev-c1") {
		t.Errorf("subject = %q, want %q", ev.Subject, subjects.Beat("dev-c1"))
	}
	if ev.Env.GetCollectorId() != "dev-c1" {
		t.Errorf("collector_id = %q", ev.Env.GetCollectorId())
	}
	if got := ev.Env.GetBeat().GetStartedAt().AsTime(); !got.Equal(started) {
		t.Errorf("started_at = %v, want %v", got, started)
	}
	if got := ev.Env.GetTsCollector().AsTime(); !got.Equal(now) {
		t.Errorf("ts_collector = %v, want %v", got, now)
	}
}

// TestAFailedBeatIsDroppedNotResent: one attempt per beat, and a failure is
// the next beat's problem, not a retry's. A re-send would carry the failed
// beat's msg-id; the second attempt here must carry a new one.
func TestAFailedBeatIsDroppedNotResent(t *testing.T) {
	rec := &beatRecorder{fail: 1}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	dropped := plainCounterValue(t, "vantage_collector_beats_dropped_total")
	if err := hb.Beat(t.Context()); err == nil {
		t.Fatal("first beat: want the publish error back")
	}
	if err := hb.Beat(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := rec.get()
	if len(evs) != 2 {
		t.Fatalf("%d publish attempts for two beats, want 2 -- a failed beat must not be re-sent", len(evs))
	}
	if evs[0].MsgID == evs[1].MsgID {
		t.Fatalf("both attempts carry msg-id %q: the second is a re-send of the first, not a new beat", evs[0].MsgID)
	}
	if got := plainCounterValue(t, "vantage_collector_beats_dropped_total") - dropped; got != 1 {
		t.Errorf("beats_dropped moved by %v, want 1", got)
	}
}

// TestRunBeatsOnTheIntervalUntilCanceled: Run ticks, and stops when its
// context does -- the collector's shutdown must not leave a beat in flight
// behind it.
func TestRunBeatsOnTheIntervalUntilCanceled(t *testing.T) {
	rec := &beatRecorder{}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hb.Run(ctx, slog.New(slog.DiscardHandler))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.get()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d beats after 2s at a 10ms interval", len(rec.get()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	n := len(rec.get())
	time.Sleep(50 * time.Millisecond)
	if got := len(rec.get()); got != n {
		t.Errorf("%d beats after Run returned", got-n)
	}
}

// TestBeatReturnsWithinItsDeadlineWhenNATSStalls: PublishOnce imposes no
// deadline of its own (see its doc comment), so Beat must pass it one. A
// stalled or non-answering NATS is stood in for by blockingPublisher, which
// never resolves except by its ctx being done. Beat must return at its own
// per-beat deadline -- well under BeatInterval -- rather than hang for the
// life of the caller's ctx, so neither the beat loop nor a collector
// shutdown can be held longer than that deadline.
func TestBeatReturnsWithinItsDeadlineWhenNATSStalls(t *testing.T) {
	hb := NewHeartbeat("dev-c1", time.Now(), blockingPublisher{}, time.Now)
	hb.timeout = 100 * time.Millisecond

	// Beat runs on its own goroutine, so an unbounded Beat fails here in
	// 10 s rather than hanging until go test's own timeout.
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- hb.Beat(context.Background()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Beat has not returned after 10 s against a stalled publisher: it is not bounded by its own deadline")
	}
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when the per-beat deadline expires")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed < hb.timeout {
		t.Errorf("Beat returned after %v, before its own %v deadline elapsed", elapsed, hb.timeout)
	}
	if elapsed > hb.timeout+2*time.Second {
		t.Fatalf("Beat took %v against a stalled publisher with a %v deadline -- "+
			"it is not bounded by its own deadline (context.Background() has none of "+
			"its own to fall back on)", elapsed, hb.timeout)
	}
}

// gatedPublisher is a BeatPublisher whose every attempt waits for release
// before it returns, so a test can hold Run inside a beat.
type gatedPublisher struct {
	beatRecorder
	release chan struct{}
}

func (g *gatedPublisher) PublishOnce(ctx context.Context, ev Event) error {
	_ = g.beatRecorder.PublishOnce(ctx, ev)
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return nil
}

// runHeartbeat starts hb.Run and returns its cancel, which also waits for
// Run to return.
func runHeartbeat(t *testing.T, hb *Heartbeat) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hb.Run(ctx, slog.New(slog.DiscardHandler))
		close(done)
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

// waitBeats waits up to d for at least n publish attempts and returns how
// many there were.
func waitBeats(rec interface{ get() []Event }, n int, d time.Duration) int {
	deadline := time.Now().Add(d)
	for len(rec.get()) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return len(rec.get())
}

// TestAReconnectBeatsWithoutWaitingForTheTick: after NATS comes back, the
// next beat goes out at once, not at the next tick. A beat dropped while the
// client was still reconnecting would otherwise leave a gap of up to two
// intervals plus the outage, which at a 60 s outage is the 90 s stale
// threshold. The interval here is an hour, so a beat inside a second is the
// reconnect's and not the ticker's -- and the half without a reconnect shows
// the ticker gives none.
func TestAReconnectBeatsWithoutWaitingForTheTick(t *testing.T) {
	rec := &beatRecorder{}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = time.Hour
	runHeartbeat(t, hb)

	if got := waitBeats(rec, 1, 200*time.Millisecond); got != 0 {
		t.Fatalf("%d beats with no reconnect and an hour's interval, want 0", got)
	}
	hb.Reconnected()
	if got := waitBeats(rec, 1, time.Second); got != 1 {
		t.Fatalf("%d beats within a second of a reconnect, want 1", got)
	}
	evs := rec.get()
	if evs[0].Subject != subjects.Beat("dev-c1") {
		t.Errorf("reconnect beat subject = %q, want %q", evs[0].Subject, subjects.Beat("dev-c1"))
	}
}

// TestReconnectsCoalesceAndNeverBlock: Reconnected runs on nats.go's callback
// goroutine and must return at once whatever Run is doing, and reconnects
// that land while a beat is already in flight or already asked for become
// one more beat, not one each. Run is held inside the first reconnect beat
// while a hundred more reconnects arrive.
func TestReconnectsCoalesceAndNeverBlock(t *testing.T) {
	g := &gatedPublisher{release: make(chan struct{})}
	hb := NewHeartbeat("dev-c1", time.Now(), g, time.Now)
	hb.interval = time.Hour
	hb.timeout = 10 * time.Second
	runHeartbeat(t, hb)

	hb.Reconnected()
	if got := waitBeats(&g.beatRecorder, 1, time.Second); got != 1 {
		t.Fatalf("%d beats after the first reconnect, want 1", got)
	}
	start := time.Now()
	for range 100 {
		hb.Reconnected()
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("100 Reconnected calls took %v while Run was held in a beat: it blocks", d)
	}
	close(g.release)
	if got := waitBeats(&g.beatRecorder, 2, time.Second); got != 2 {
		t.Fatalf("%d beats after the held beat was released, want 2", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(g.get()); got != 2 {
		t.Fatalf("%d beats for 101 reconnects, want 2: the ones that arrived during "+
			"a beat must coalesce into one", got)
	}
}

// TestNoReconnectBeatAfterShutdown: once Run has returned, a reconnect -- the
// connection's last gasp during shutdown -- neither beats nor blocks.
func TestNoReconnectBeatAfterShutdown(t *testing.T) {
	rec := &beatRecorder{}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = time.Hour
	stop := runHeartbeat(t, hb)
	stop()

	start := time.Now()
	for range 10 {
		hb.Reconnected()
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Reconnected after shutdown took %v: it blocks", d)
	}
	if got := waitBeats(rec, 1, 200*time.Millisecond); got != 0 {
		t.Fatalf("%d beats after Run returned, want 0", got)
	}
}

// stallRecorder records every attempt and then stalls like blockingPublisher.
type stallRecorder struct{ beatRecorder }

func (s *stallRecorder) PublishOnce(ctx context.Context, ev Event) error {
	_ = s.beatRecorder.PublishOnce(ctx, ev)
	<-ctx.Done()
	return ctx.Err()
}

// TestAReconnectBeatIsBoundedByTheBeatDeadline: a reconnect beat goes through
// Beat, deadline and all. Against a NATS that stalls, the first reconnect beat
// must give up at its per-beat deadline, so a second reconnect after that
// deadline gets its own attempt; a beat bounded only by Run's ctx would
// still be holding Run, and there would be one attempt.
func TestAReconnectBeatIsBoundedByTheBeatDeadline(t *testing.T) {
	s := &stallRecorder{}
	hb := NewHeartbeat("dev-c1", time.Now(), s, time.Now)
	hb.interval = time.Hour
	hb.timeout = 100 * time.Millisecond
	runHeartbeat(t, hb)

	hb.Reconnected()
	if got := waitBeats(&s.beatRecorder, 1, time.Second); got != 1 {
		t.Fatalf("%d attempts after a reconnect, want 1", got)
	}
	time.Sleep(3 * hb.timeout)
	hb.Reconnected()
	if got := waitBeats(&s.beatRecorder, 2, time.Second); got != 2 {
		t.Fatalf("%d attempts after two reconnects %v apart against a stalled NATS, "+
			"want 2: the first reconnect beat is not bounded by its %v deadline",
			got, 3*hb.timeout, hb.timeout)
	}
}

// TestAFailedReconnectBeatIsRetried: a cluster that accepts clients before
// its leaders are elected fails the reconnect beat at once. The beat is tried
// again, retryDelay after each failure, until one lands -- here the third --
// and then nothing more goes out: at most three attempts, no burst.
func TestAFailedReconnectBeatIsRetried(t *testing.T) {
	rec := &beatRecorder{fail: 2}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = time.Hour
	hb.retryDelay = 50 * time.Millisecond
	runHeartbeat(t, hb)

	hb.Reconnected()
	if got := waitBeats(rec, 3, 2*time.Second); got != 3 {
		t.Fatalf("%d attempts after a reconnect whose first two beats fail, want 3", got)
	}
	time.Sleep(10 * hb.retryDelay)
	evs := rec.get()
	if len(evs) != 3 {
		t.Fatalf("%d attempts once the third landed, want 3", len(evs))
	}
	if evs[0].MsgID == evs[1].MsgID || evs[1].MsgID == evs[2].MsgID {
		t.Errorf("retries reuse a msg-id (%s, %s, %s); each try is its own beat",
			evs[0].MsgID, evs[1].MsgID, evs[2].MsgID)
	}
}

// TestReconnectRetriesAreBounded: against a NATS that never answers, a
// reconnect beat is tried reconnectRetries+1 times and then left to the
// ticker.
func TestReconnectRetriesAreBounded(t *testing.T) {
	rec := &beatRecorder{fail: 1000}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = time.Hour
	hb.retryDelay = 20 * time.Millisecond
	runHeartbeat(t, hb)

	hb.Reconnected()
	want := reconnectRetries + 1
	if got := waitBeats(rec, want, 2*time.Second); got != want {
		t.Fatalf("%d attempts, want %d", got, want)
	}
	time.Sleep(20 * hb.retryDelay)
	if got := len(rec.get()); got != want {
		t.Fatalf("%d attempts for one reconnect against a dead NATS, want %d", got, want)
	}
}

// TestAFailedScheduledBeatIsNotRetried: only a reconnect beat is retried. The
// first tick fails; the next attempt is the next tick, not a retry
// retryDelay later.
func TestAFailedScheduledBeatIsNotRetried(t *testing.T) {
	rec := &beatRecorder{fail: 1}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = 300 * time.Millisecond
	hb.retryDelay = 20 * time.Millisecond
	start := time.Now()
	runHeartbeat(t, hb)

	if got := waitBeats(rec, 1, time.Second); got != 1 {
		t.Fatalf("%d attempts at the first tick, want 1", got)
	}
	time.Sleep(time.Until(start.Add(450 * time.Millisecond)))
	if got := len(rec.get()); got != 1 {
		t.Fatalf("%d attempts before the second tick, want 1: a failed scheduled beat was retried", got)
	}
	if got := waitBeats(rec, 2, time.Second); got != 2 {
		t.Fatalf("%d attempts by the second tick, want 2", got)
	}
}

// TestAPendingRetryDoesNotOutliveShutdown: a retry scheduled when Run stops
// never fires.
func TestAPendingRetryDoesNotOutliveShutdown(t *testing.T) {
	rec := &beatRecorder{fail: 1000}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = time.Hour
	hb.retryDelay = 100 * time.Millisecond
	stop := runHeartbeat(t, hb)

	hb.Reconnected()
	if got := waitBeats(rec, 1, time.Second); got != 1 {
		t.Fatalf("%d attempts after a reconnect, want 1", got)
	}
	stop()
	time.Sleep(4 * hb.retryDelay)
	if got := len(rec.get()); got != 1 {
		t.Fatalf("%d attempts after shutdown with a retry pending, want 1", got)
	}
}

// TestAReconnectBeatLandsWithinOneRetryDelay: at the shipped delay, a
// reconnect beat whose first try fails lands about 5 s after the reconnect,
// far inside BeatInterval.
func TestAReconnectBeatLandsWithinOneRetryDelay(t *testing.T) {
	if testing.Short() {
		t.Skip("waits one shipped retry delay")
	}
	rec := &beatRecorder{fail: 1}
	hb := NewHeartbeat("dev-c1", time.Now(), rec, time.Now)
	hb.interval = time.Hour
	runHeartbeat(t, hb)

	start := time.Now()
	hb.Reconnected()
	if got := waitBeats(rec, 2, reconnectRetryDelay+2*time.Second); got != 2 {
		t.Fatalf("%d attempts within %v of a reconnect whose first beat fails, want 2",
			got, reconnectRetryDelay+2*time.Second)
	}
	if d := time.Since(start); d < reconnectRetryDelay-time.Second || d >= BeatInterval/2 {
		t.Errorf("the retry landed %v after the reconnect, want about %v", d, reconnectRetryDelay)
	}
}
