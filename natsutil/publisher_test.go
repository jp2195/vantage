package natsutil

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/natsutil/natstest"
)

// failRecorder collects the errors a PublishNotify callback receives.
type failRecorder struct {
	mu   sync.Mutex
	errs []error
}

func (f *failRecorder) record(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, err)
}

func (f *failRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.errs)
}

// TestPublishNotifyAttributesAsyncRejections is what lets the collector close
// the one BMP session whose event the server rejected: the rejection must
// reach the callback handed in with that publish, and only that one. The
// accepted publish on the same Publisher is the other side -- a callback that
// fired for every publish, or for none, fails here.
func TestPublishNotifyAttributesAsyncRejections(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(js)
	var global failRecorder
	p.OnError = global.record

	// A stream full under DiscardNew is a rejection the server answers with,
	// and one Publisher never retries. A subject no stream captures would
	// be the obvious choice, but that is retried for backoffRetryWindow
	// first (see retryClass).
	createFullStream(t, js)
	if err := p.Publish(fullEvent("fill/1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(5 * time.Second); err != nil {
		t.Fatalf("filling FULL: %v", err)
	}

	var rejected, accepted failRecorder
	bad := fullEvent("rejected/1")
	if err := p.PublishNotify(bad, rejected.record); err != nil {
		t.Fatalf("PublishNotify (synchronous) unexpectedly failed: %v", err)
	}
	if err := p.PublishNotify(testEvent("notify-ok/1"), accepted.record); err != nil {
		t.Fatalf("PublishNotify (synchronous) unexpectedly failed: %v", err)
	}
	if err := p.Drain(5 * time.Second); err == nil {
		t.Fatal("Drain returned nil; the rejection must still reach Drain as well")
	}
	if got := rejected.count(); got != 1 {
		t.Fatalf("the rejected publish's callback ran %d times, want 1", got)
	}
	if got := accepted.count(); got != 0 {
		t.Fatalf("the accepted publish's callback ran %d times, want 0", got)
	}
	if got := global.count(); got != 1 {
		t.Fatalf("OnError ran %d times, want 1 (per-publish callbacks do not replace it)", got)
	}
}

// TestPublisherConnected reports the underlying connection's state, which is
// what the collector gates new BMP sessions on.
func TestPublisherConnected(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	p := NewPublisher(js)
	if !p.Connected() {
		t.Fatal("Connected() = false on a live connection")
	}
	nc.Close()
	if p.Connected() {
		t.Fatal("Connected() = true after the connection closed")
	}
}
