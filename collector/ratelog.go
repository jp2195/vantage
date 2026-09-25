package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// rateLimitedLog writes one log line for the first occurrence of a repeating
// failure and then at most one per interval, each carrying in "count" how
// many occurrences it accounts for -- itself plus every one suppressed since
// the previous line. flush writes whatever is still pending. Every occurrence
// is accounted for in exactly one line, as long as flush is eventually
// called after the last one.
//
// It exists for failures that arrive in floods: during a NATS outage every
// in-flight publish of a session fails, thousands a second, and one line per
// failure would bury everything else in the log. The attributes logged are
// the most recent occurrence's.
//
// Safe for concurrent use: publish failures are reported both from a session's
// own goroutine and from the publisher's.
type rateLimitedLog struct {
	msg      string
	level    slog.Level
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	last    time.Time // when the last line was written; zero before the first
	pending int
	args    []any
}

// record notes one occurrence, described by args (slog key/value pairs), and
// writes a line if none has been written within the interval.
func (r *rateLimitedLog) record(log *slog.Logger, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending++
	r.args = args
	now := r.clock()
	if !r.last.IsZero() && now.Sub(r.last) < r.interval {
		return
	}
	r.emitLocked(log, now)
}

// flush writes a line for any occurrences not yet logged.
func (r *rateLimitedLog) flush(log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending > 0 {
		r.emitLocked(log, r.clock())
	}
}

func (r *rateLimitedLog) emitLocked(log *slog.Logger, now time.Time) {
	log.Log(context.Background(), r.level, r.msg, append([]any{"count", r.pending}, r.args...)...)
	r.pending, r.args, r.last = 0, nil, now
}

func (r *rateLimitedLog) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// burstLog is rateLimitedLog for failures that arrive in bursts from many
// goroutines at once: it never writes on the first occurrence, but window
// after it, so the line reports the burst rather than its first member. It
// then writes at most one line per interval, each carrying in "count" every
// occurrence since the previous line, and flush writes whatever is pending.
// Like rateLimitedLog, every occurrence is accounted for in exactly one line
// as long as flush is eventually called after the last one.
//
// It exists for publish retries: a NATS server restart fails the in-flight
// publishes of every session within milliseconds, and rateLimitedLog's
// immediate first line reported that as count=1, the one line a reader sees
// for the next 10 s.
type burstLog struct {
	msg      string
	level    slog.Level
	window   time.Duration
	interval time.Duration

	mu      sync.Mutex
	log     *slog.Logger
	last    time.Time // when the last line was written; zero before the first
	pending int
	args    []any
	timer   *time.Timer
}

// record notes one occurrence, described by args (slog key/value pairs), and
// schedules a line if none is scheduled.
func (b *burstLog) record(log *slog.Logger, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending++
	b.args = args
	b.log = log
	if b.timer != nil {
		return
	}
	wait := b.window
	if !b.last.IsZero() {
		wait = max(wait, b.interval-time.Since(b.last))
	}
	b.timer = time.AfterFunc(wait, b.fire)
}

func (b *burstLog) fire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timer = nil
	b.emitLocked()
}

// flush writes a line for any occurrences not yet logged.
func (b *burstLog) flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.emitLocked()
}

func (b *burstLog) emitLocked() {
	if b.pending == 0 {
		return
	}
	b.log.Log(context.Background(), b.level, b.msg, append([]any{"count", b.pending}, b.args...)...)
	b.pending, b.args, b.last = 0, nil, time.Now()
}
