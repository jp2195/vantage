package natsutil

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil/natstest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
)

func beatEvent(msgID string) collector.Event {
	return collector.Event{
		Subject: subjects.Beat("c1"),
		MsgID:   msgID,
		Env:     &vantagev1.Envelope{CollectorId: "c1"},
	}
}

// TestPublishOnceStoresTheMessage is the happy path: the beat lands in STATS
// and the call reports success only after JetStream acked it.
func TestPublishOnceStoresTheMessage(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(js)
	if err := p.PublishOnce(ctx, beatEvent("beat/1")); err != nil {
		t.Fatalf("PublishOnce: %v", err)
	}
	s, err := js.Stream(ctx, "STATS")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("STATS holds %d messages, want 1", info.State.Msgs)
	}
}

// TestPublishOnceMakesOneAttempt: to a subject no stream captures, Publish
// would retry for 20 s (backoffRetryWindow) and report through Drain, while
// jetstream's own Publish would retry no-responders twice more. PublishOnce
// must make exactly ONE attempt, report the failure itself, and leave
// nothing for Drain, OnError or OnRetry. The server's own inbound-message
// counter is the witness: it sees every attempt, whatever the client does
// with the replies. Against nats-server v2.15.0 and nats.go v1.54.0 it moves
// by 3 for a default publish and by 1 with retries off.
func TestPublishOnceMakesOneAttempt(t *testing.T) {
	srv := natstest.RunServer(t)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(js)
	var onError, onRetry atomic.Int32
	p.OnError = func(error) { onError.Add(1) }
	p.OnRetry = func(error) { onRetry.Add(1) }

	inMsgs := func() int64 {
		v, err := srv.Varz(nil)
		if err != nil {
			t.Fatal(err)
		}
		return v.InMsgs
	}
	before := inMsgs()
	start := time.Now()
	ev := beatEvent("beat/1")
	ev.Subject = "vantage.v1.nostream.x"
	err = p.PublishOnce(context.Background(), ev)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("PublishOnce to a subject no stream captures returned nil")
	}
	if got := inMsgs() - before; got != 1 {
		t.Errorf("the server saw %d publishes, want exactly 1: a beat is never re-sent", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("PublishOnce took %v; a publish that is not retried answers at once", elapsed)
	}
	if err := p.Drain(10 * time.Millisecond); err != nil {
		t.Errorf("Drain = %v; PublishOnce must leave nothing for Drain to wait on or report", err)
	}
	if onError.Load() != 0 || onRetry.Load() != 0 {
		t.Errorf("OnError ran %d times and OnRetry %d; PublishOnce reports only to its caller",
			onError.Load(), onRetry.Load())
	}
}

// TestPublishOnceRefusesWhileDisconnected: with the connection down, nats.go
// would buffer the message and deliver it whenever the connection returned --
// a late beat, which is exactly what PublishOnce exists not to send.
func TestPublishOnceRefusesWhileDisconnected(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	p := NewPublisher(js)
	nc.Close()
	err := p.PublishOnce(context.Background(), beatEvent("beat/1"))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("PublishOnce while disconnected = %v, want ErrNotConnected", err)
	}
}
