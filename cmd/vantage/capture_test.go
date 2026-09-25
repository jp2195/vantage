package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil"
	"github.com/jp2195/vantage/natsutil/natstest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
	"google.golang.org/protobuf/proto"
)

// captureTestStreamOpts clamps every replica count to 1, mirroring the same
// pattern used throughout this package's other _test.go files
// (cliTestStreamOpts in bmpgen_test.go): the embedded natstest server is a
// single, non-clustered node, and StreamOpts{}'s own zero-value LSReplicas
// default of 3 is rejected outright by such a node.
// RoutesMaxBytes/RawMaxBytes are bounded because nats-server validates a
// stream's MaxBytes against the FREE SPACE of the JetStream store
// directory's filesystem, not against the account limit -- an unlimited
// account still refuses a stream larger than the disk can hold. The
// production defaults (8 GiB ROUTES, 2 GiB RAW) exceeded what the CI
// runner's temp filesystem had free, so every test that provisioned
// streams failed there with "insufficient storage resources available"
// while passing locally on a bigger disk. Tests publish kilobytes; 16 MiB
// is far above anything they write and fits anywhere.
var captureTestStreamOpts = natsutil.StreamOpts{Replicas: 1, LSReplicas: 1,
	RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20}

// TestCaptureRequiresExactlyOneMode checks the error *text*, not merely its
// presence: cmdCapture also fails whenever it cannot reach NATS (the default
// -nats is a loopback address nothing in this test suite listens on), so an
// assertion of err != nil alone would pass identically whether the
// mode-exclusivity switch ran at all -- it was verified to do exactly that
// under mutation (neutering the switch's two cases left this test green,
// because cmdCapture still failed one step later at nats.Connect). Pinning
// the message is what makes a broken or deleted mode check actually fail
// this test.
func TestCaptureRequiresExactlyOneMode(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"neither", []string{"-router", "10.0.0.1", "-o", "x.bmpcap"}, "one of -window"},
		{"both", []string{"-router", "10.0.0.1", "-o", "x.bmpcap", "-window", "1m", "-since", "1h"}, "mutually exclusive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := cmdCapture(c.args)
			if err == nil {
				t.Fatal("want an error: --window and --since are mutually exclusive and one is required")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("cmdCapture error = %q, want it to contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// TestCaptureWritesFileAndSidecar pins the on-disk shape: raw BMP bytes
// concatenated exactly as received, plus a self-describing sidecar.
func TestCaptureWritesFileAndSidecar(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.bmpcap")
	msgs := [][]byte{
		{3, 0, 0, 0, 6, 4},
		{3, 0, 0, 0, 6, 5},
	}
	meta := captureMeta{Router: "10.0.0.1", SysDescr: "Cisco NX-OS", Vendor: "cisco", OS: "nxos"}
	if err := writeCapture(out, msgs, meta); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, msgs[0]...), msgs[1]...)
	if string(got) != string(want) {
		t.Fatalf("capture = %x, want %x", got, want)
	}
	side, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	for _, k := range []string{"10.0.0.1", "nxos", "message_count"} {
		if !strings.Contains(string(side), k) {
			t.Fatalf("sidecar missing %q: %s", k, side)
		}
	}
}

// TestCaptureOverwritesExistingOutputFile pins the deliberate decision that
// -o names a file the tool trusts the operator to have chosen, not one it
// protects: writeCapture replaces whatever was already at path and path+
// ".json" in full, the same way `tcpdump -w` or a shell `>` redirect would,
// rather than silently appending to (or refusing to touch) stale content
// left over from a previous run.
func TestCaptureOverwritesExistingOutputFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.bmpcap")
	if err := os.WriteFile(out, []byte("STALE-CONTENT-THAT-MUST-NOT-SURVIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out+".json", []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs := [][]byte{{3, 0, 0, 0, 6, 4}}
	if err := writeCapture(out, msgs, captureMeta{Router: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msgs[0]) {
		t.Fatalf("capture file was not fully overwritten: got %x, want %x", got, msgs[0])
	}
	side, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(side, []byte("stale")) {
		t.Fatalf("sidecar retains stale content: %s", side)
	}
}

// TestWriteCaptureRoundTripsThroughBmpReadMsg answers the "does the file
// format let reparse read messages back unambiguously" question empirically
// rather than by assertion: writeCapture uses no delimiter between messages
// at all, which is only safe because every BMP message already declares its
// own total length in its 6-byte common header (bmp.HeaderLen), so
// bmp.ReadMsg run in a loop over the concatenated bytes must recover exactly
// the original message boundaries.
func TestWriteCaptureRoundTripsThroughBmpReadMsg(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "roundtrip.bmpcap")
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	msgs := [][]byte{
		bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.Stats(ph, map[uint32]uint64{0: 1, 7: 2}),
		bmptest.Termination(0),
	}
	if err := writeCapture(out, msgs, captureMeta{Router: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var got [][]byte
	for {
		m, err := bmp.ReadMsg(f)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("bmp.ReadMsg: %v", err)
		}
		got = append(got, append(bmp.AppendHeader(nil, m.Type, len(m.Payload)), m.Payload...))
	}
	if len(got) != len(msgs) {
		t.Fatalf("round-tripped %d messages, want %d", len(got), len(msgs))
	}
	for i := range msgs {
		if !bytes.Equal(got[i], msgs[i]) {
			t.Fatalf("message %d mismatch after round-trip: got %x want %x", i, got[i], msgs[i])
		}
	}
}

// TestCollectRawIncludesUnmirroredRawEvents is the load-bearing regression
// test: collector/server.go's handleConn
// skips the mirror hook for any message Session.Handle already published as
// a RawEvent (a parse failure, a Termination, a Route Mirroring message), so
// those messages reach the raw stream with Mirrored left false even though
// they came from an armed router. A filter on mirrored==true would silently
// drop every one of them -- the single most valuable fixture a corpus can
// contain. collectRaw must return both.
func TestCollectRawIncludesUnmirroredRawEvents(t *testing.T) {
	_, js := natstest.RunJSConn(t)
	ctx := context.Background()
	if err := natsutil.EnsureStreams(ctx, js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}
	pub := natsutil.NewPublisher(js)
	routerTok := subjects.EncodeIP(netip.MustParseAddr("10.0.0.1"))
	subj := subjects.Raw(routerTok)

	parseFailureBytes := []byte{3, 0, 0, 0, 7, 99, 0xAA} // Mirrored=false: a parse failure, unmarked
	mirroredBytes := []byte{3, 0, 0, 0, 6, 4}            // Mirrored=true: a deliberate mirror-mode copy

	start := time.Now().Add(-time.Minute)
	events := []collector.Event{
		{Subject: subj, MsgID: routerTok + "/raw/1/1", Env: &vantagev1.Envelope{
			Payload: &vantagev1.Envelope_Raw{Raw: &vantagev1.RawEvent{
				BmpMsg: parseFailureBytes, ParseError: "bmp: bad length", Mirrored: false,
			}},
		}},
		{Subject: subj, MsgID: routerTok + "/raw/1/2", Env: &vantagev1.Envelope{
			Payload: &vantagev1.Envelope_Raw{Raw: &vantagev1.RawEvent{
				BmpMsg: mirroredBytes, Mirrored: true,
			}},
		}},
	}
	for _, ev := range events {
		if err := pub.Publish(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := pub.Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	msgs, _, err := collectRaw(context.Background(), js, subj, start, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("collectRaw returned %d messages, want 2 (one mirrored, one not): %x", len(msgs), msgs)
	}
	var gotParseFailure, gotMirrored bool
	for _, m := range msgs {
		if bytes.Equal(m, parseFailureBytes) {
			gotParseFailure = true
		}
		if bytes.Equal(m, mirroredBytes) {
			gotMirrored = true
		}
	}
	if !gotParseFailure {
		t.Fatal("collectRaw dropped the unmirrored parse-failure RawEvent -- " +
			"a filter on mirrored==true would silently discard the most valuable fixture a corpus can contain")
	}
	if !gotMirrored {
		t.Fatal("collectRaw dropped the mirrored RawEvent")
	}
}

// TestCollectRawFiltersByRouterSubject confirms the subject filter actually
// scopes the replay to the requested router: publishing another router's
// raw events into the same RAW stream must not leak into this router's
// capture.
func TestCollectRawFiltersByRouterSubject(t *testing.T) {
	_, js := natstest.RunJSConn(t)
	ctx := context.Background()
	if err := natsutil.EnsureStreams(ctx, js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}
	pub := natsutil.NewPublisher(js)

	routerA := netip.MustParseAddr("10.0.0.1")
	routerB := netip.MustParseAddr("10.0.0.2")
	tokA, tokB := subjects.EncodeIP(routerA), subjects.EncodeIP(routerB)
	bytesA := []byte{3, 0, 0, 0, 6, 1}
	bytesB := []byte{3, 0, 0, 0, 6, 2}

	start := time.Now().Add(-time.Minute)
	for _, ev := range []collector.Event{
		{Subject: subjects.Raw(tokA), MsgID: tokA + "/raw/1/1", Env: &vantagev1.Envelope{
			Payload: &vantagev1.Envelope_Raw{Raw: &vantagev1.RawEvent{BmpMsg: bytesA}}}},
		{Subject: subjects.Raw(tokB), MsgID: tokB + "/raw/1/1", Env: &vantagev1.Envelope{
			Payload: &vantagev1.Envelope_Raw{Raw: &vantagev1.RawEvent{BmpMsg: bytesB}}}},
	} {
		if err := pub.Publish(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := pub.Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	msgs, _, err := collectRaw(context.Background(), js, subjects.Raw(tokA), start, 0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !bytes.Equal(msgs[0], bytesA) {
		t.Fatalf("collectRaw(router A filter) = %x, want exactly [%x] (router B must not leak in)", msgs, bytesA)
	}
}

// TestCollectRawStopsOnCanceledContext confirms collectRaw actually threads
// its ctx parameter through to the JetStream calls rather than building its
// own unconditioned context internally -- an already-canceled ctx must fail
// fast rather than silently succeeding against a fresh, unrelated context.
func TestCollectRawStopsOnCanceledContext(t *testing.T) {
	_, js := natstest.RunJSConn(t)
	if err := natsutil.EnsureStreams(context.Background(), js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	filter := subjects.Raw(subjects.EncodeIP(netip.MustParseAddr("10.0.0.1")))
	if _, _, err := collectRaw(ctx, js, filter, time.Now().Add(-time.Minute), 0, time.Minute); err == nil {
		t.Fatal("want an error when collectRaw is given an already-canceled context")
	}
}

// TestCaptureSinceNoMessagesWritesEmptyCaptureFile answers "what happens
// when the stream has no messages for that router": no error, an empty
// capture file, and a sidecar reporting message_count 0 -- silence is not
// itself a failure the operator needs to be told to retry.
func TestCaptureSinceNoMessagesWritesEmptyCaptureFile(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := natsutil.EnsureStreams(context.Background(), js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "empty.bmpcap")
	if err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(), "-router", "10.0.0.1", "-o", out, "-since", "1h",
	}); err != nil {
		t.Fatalf("cmdCapture with no messages in RAW: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("capture file = %d bytes, want 0 (no messages for this router)", len(data))
	}
	side, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var meta captureMeta
	if err := json.Unmarshal(side, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.MessageCount != 0 {
		t.Fatalf("sidecar message_count = %d, want 0", meta.MessageCount)
	}
}

// TestCaptureSinceErrorsWhenRawStreamMissing answers "what happens when the
// consumer can't even attach": a RAW stream that was never provisioned must
// surface as an error, and no output file (capture or sidecar) is written --
// a half-written capture pretending to be complete would be worse than no
// file at all.
func TestCaptureSinceErrorsWhenRawStreamMissing(t *testing.T) {
	nc, _ := natstest.RunJSConn(t) // no EnsureStreams: RAW does not exist
	dir := t.TempDir()
	out := filepath.Join(dir, "x.bmpcap")
	err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(), "-router", "10.0.0.1", "-o", out, "-since", "1h",
	})
	if err == nil {
		t.Fatal("want an error when the RAW stream has never been provisioned")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("capture file must not be written when the replay itself failed")
	}
	if _, statErr := os.Stat(out + ".json"); statErr == nil {
		t.Fatal("sidecar must not be written when the replay itself failed")
	}
}

// TestWaitWindowReturnsEarlyOnInterrupt and TestWaitWindowRunsFullDuration
// pin cmdCapture's answer to "what happens when the consumer is interrupted
// mid-capture" for the forward (-window) path: an operator's ctrl-C during
// the mirror window should stop the wait immediately rather than block for
// the rest of the window, so cmdCapture can go on to disarm the mirror and
// write whatever was captured so far.
func TestWaitWindowReturnsEarlyOnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate the interrupt having already fired
	start := time.Now()
	if !waitWindow(ctx, 3*time.Second) {
		t.Fatal("want interrupted=true when ctx is already done")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitWindow took %v to notice an already-canceled context, want near-instant", elapsed)
	}
}

func TestWaitWindowRunsFullDurationWithoutInterrupt(t *testing.T) {
	start := time.Now()
	if waitWindow(context.Background(), 50*time.Millisecond) {
		t.Fatal("want interrupted=false when the timer elapses first")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("waitWindow returned after %v, want >= 50ms", elapsed)
	}
}

// TestAdminCallsAreBounded proves both admin calls give up on a collector
// that accepts the TCP connection and then never answers. That is not a
// contrived failure: a collector wedged on a full disk, a stale iptables
// DNAT, or an SSH tunnel whose far end is gone all present exactly this
// shape -- the connect succeeds, the response never comes.
//
// Under http.DefaultClient (which is what http.Post uses, and what both
// calls used before) there is no timeout of any kind, so each of these
// blocks forever. armMirror runs before cmdCapture installs its signal
// handler and disarmMirror runs from a defer during shutdown, so in both
// cases an operator's only recourse would be to kill the process -- and
// for disarm that leaves the mirror armed on the collector, still
// republishing into the shared raw stream.
func TestAdminCallsAreBounded(t *testing.T) {
	// A listener that accepts and then holds the connection open, reading
	// nothing and writing nothing. httptest is no good here: its handler
	// runs after the request is read, and we want to stall before that.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn // accepted, never answered
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed by the cleanup below
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		// Order matters: close the listener first and wait for the accept
		// goroutine to observe it, so nothing is still appending to held
		// while this drains it.
		ln.Close()
		<-stopped
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})

	client := &http.Client{Timeout: 250 * time.Millisecond}
	addr := ln.Addr().String()
	router := netip.MustParseAddr("10.0.0.1")

	t.Run("armMirror", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { done <- armMirror(client, addr, router, time.Minute, 1<<20) }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("armMirror: want an error from the stalled collector, got nil")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("armMirror never returned against a collector that accepts " +
				"the connection and never answers: the call is unbounded")
		}
	})

	t.Run("disarmMirror", func(t *testing.T) {
		done := make(chan struct{})
		go func() { disarmMirror(client, addr, router); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("disarmMirror never returned against a stalled collector: " +
				"the call is unbounded, so an interrupted capture would hang " +
				"on exit with the mirror left armed")
		}
	})
}

// TestAdminClientHasTimeout pins the client the two call sites above are
// actually wired to. The behavioral test proves a bounded client works;
// this proves the binary uses one, which is the half a caller could
// silently undo by passing http.DefaultClient.
func TestAdminClientHasTimeout(t *testing.T) {
	if adminClient.Timeout == 0 {
		t.Fatal("adminClient.Timeout is 0: the admin calls are unbounded again")
	}
}

// TestDrainRawReportsAnEarlyStop pins a critical contract: a
// replay that stops before consuming everything it was promised must not be
// reportable as a whole capture. Three messages are promised, the transport
// fails after the first, and what comes back has to say so -- Expected 3,
// Complete false -- rather than looking byte-identical to a legitimate
// one-message capture.
func TestDrainRawReportsAnEarlyStop(t *testing.T) {
	kept := rawEnvelopeBytes(t, []byte{3, 0, 0, 0, 6, 1})
	var calls int
	next := func() ([]byte, error) {
		calls++
		if calls == 1 {
			return kept, nil
		}
		return nil, errors.New("connection reset")
	}

	msgs, meta, complete := drainRaw(3, 0, next)

	if complete {
		t.Error("drainRaw reported a complete capture after the transport failed on message 2 of 3")
	}
	if meta.Expected != 3 {
		t.Errorf("meta.Expected = %d, want 3: the sidecar cannot show a shortfall it never recorded", meta.Expected)
	}
	if meta.Complete {
		t.Error("meta.Complete is true on a truncated replay: the sidecar would claim the short file is whole")
	}
	if len(msgs) != 1 {
		t.Errorf("drainRaw returned %d messages, want the 1 that arrived before the failure", len(msgs))
	}
}

// TestDrainRawReportsACompleteReplay is the other half: consuming every
// promised message must report complete, so the flag discriminates rather
// than always saying "truncated".
func TestDrainRawReportsACompleteReplay(t *testing.T) {
	payloads := [][]byte{{3, 0, 0, 0, 6, 1}, {3, 0, 0, 0, 6, 2}}
	var calls int
	next := func() ([]byte, error) {
		if calls >= len(payloads) {
			return nil, errors.New("next called past the promised count")
		}
		b := rawEnvelopeBytes(t, payloads[calls])
		calls++
		return b, nil
	}

	msgs, meta, complete := drainRaw(uint64(len(payloads)), 0, next)

	if !complete {
		t.Error("drainRaw reported an incomplete capture after consuming every promised message")
	}
	if meta.Expected != 2 {
		t.Errorf("meta.Expected = %d, want 2", meta.Expected)
	}
	if !meta.Complete {
		t.Error("meta.Complete is false after consuming every promised message")
	}
	if len(msgs) != 2 {
		t.Errorf("drainRaw returned %d messages, want 2", len(msgs))
	}
}

// rawEnvelopeBytes marshals a RawEvent envelope the way the raw stream
// carries one, so drainRaw is fed the same shape collectRaw hands it.
func rawEnvelopeBytes(t *testing.T, bmpMsg []byte) []byte {
	t.Helper()
	b, err := proto.Marshal(&vantagev1.Envelope{
		Payload: &vantagev1.Envelope_Raw{Raw: &vantagev1.RawEvent{BmpMsg: bmpMsg}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFinishCaptureWritesTheFileThenErrorsOnAShortReplay pins the required
// ordering: a truncated capture must still land on disk -- a
// partial corpus is useful -- but the command must exit non-zero so it is
// never mistaken for a whole one. Asserting both in one test is the point:
// returning the error *instead of* writing, or writing *without* the error,
// each satisfies half the contract and fails the operator.
func TestFinishCaptureWritesTheFileThenErrorsOnAShortReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.bmpcap")
	msgs := [][]byte{{3, 0, 0, 0, 6, 1}}
	meta := captureMeta{Router: "10.0.0.1", Mode: "since", Expected: 3, Complete: false}

	err := finishCapture(path, msgs, meta)

	if err == nil {
		t.Error("finishCapture returned nil on a truncated capture: the command would exit 0 and the short file would pass as whole")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("finishCapture did not write the capture file: %v -- a partial corpus is still worth keeping", statErr)
	}
	sidecar, readErr := os.ReadFile(path + ".json")
	if readErr != nil {
		t.Fatalf("reading sidecar: %v", readErr)
	}
	var got captureMeta
	if jsonErr := json.Unmarshal(sidecar, &got); jsonErr != nil {
		t.Fatalf("sidecar is not valid JSON: %v", jsonErr)
	}
	if got.Complete {
		t.Error(`sidecar says "complete": true for a truncated capture`)
	}
	if got.Expected != 3 {
		t.Errorf(`sidecar "expected" = %d, want 3`, got.Expected)
	}
}

// TestFinishCaptureSucceedsOnACompleteReplay is the discriminating half: a
// whole capture must return nil, or the error above would fire on every run
// and mean nothing.
func TestFinishCaptureSucceedsOnACompleteReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whole.bmpcap")
	meta := captureMeta{Router: "10.0.0.1", Mode: "since", Expected: 1, Complete: true}

	if err := finishCapture(path, [][]byte{{3, 0, 0, 0, 6, 1}}, meta); err != nil {
		t.Errorf("finishCapture on a complete capture returned %v, want nil", err)
	}
}

// TestCaptureForwardArmsAndDisarmsTheMirror covers forward mode end to end --
// the mode that actually builds a corpus, which previously
// had no coverage at all: changing `if *window != 0` to `if false` left the
// whole package green, so armMirror, disarmMirror and the waitWindow call
// site were exercised by nothing.
//
// It also closes the separate gap that the sidecar's provenance
// is never asserted. TestCaptureWritesFileAndSidecar greps a sidecar built by
// hand from strings the test itself supplied, which is self-consistency, not
// correctness. Router and Mode here come from a real cmdCapture run, so
// deleting either assignment fails this test.
func TestCaptureForwardArmsAndDisarmsTheMirror(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := natsutil.EnsureStreams(context.Background(), js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var arms, disarms int
	admin := collector.NewAdminHandler(collector.NewMirrorRegistry())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/mirror":
			arms++
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/admin/mirror/"):
			disarms++
		}
		mu.Unlock()
		admin.ServeHTTP(w, r)
	}))
	defer ts.Close()

	out := filepath.Join(t.TempDir(), "forward.bmpcap")
	if err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(),
		"-collector", strings.TrimPrefix(ts.URL, "http://"),
		"-router", "10.0.0.1",
		"-o", out,
		"-window", "300ms",
	}); err != nil {
		t.Fatalf("cmdCapture(-window): %v", err)
	}

	mu.Lock()
	gotArms, gotDisarms := arms, disarms
	mu.Unlock()
	if gotArms != 1 {
		t.Errorf("collector saw %d arm requests, want 1: forward mode never armed the mirror", gotArms)
	}
	if gotDisarms != 1 {
		t.Errorf("collector saw %d disarm requests, want 1: an unreleased mirror keeps burning the collector's byte budget", gotDisarms)
	}

	side, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var meta captureMeta
	if err := json.Unmarshal(side, &meta); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v", err)
	}
	if meta.Mode != "window" {
		t.Errorf(`sidecar "mode" = %q, want "window"`, meta.Mode)
	}
	if meta.Router != "10.0.0.1" {
		t.Errorf(`sidecar "router" = %q, want "10.0.0.1"`, meta.Router)
	}
}

// TestCaptureRejectsAnUnwritableOutputBeforeArming pins that -o is validated
// before the mirror is armed, not after the capture is over. The path used
// to be checked only at the final write: `-window 6h -o
// <missing-dir>/x.bmpcap` would arm a mirror, burn a six-hour window and the
// collector's byte budget, and then discard the whole in-memory capture over
// a typo in a directory name.
//
// The assertion is the arm count, not merely that an error came back. An
// error alone proves nothing about ordering -- the unfixed code errors too,
// just six hours later.
func TestCaptureRejectsAnUnwritableOutputBeforeArming(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := natsutil.EnsureStreams(context.Background(), js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var arms int
	admin := collector.NewAdminHandler(collector.NewMirrorRegistry())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/admin/mirror" {
			mu.Lock()
			arms++
			mu.Unlock()
		}
		admin.ServeHTTP(w, r)
	}))
	defer ts.Close()

	missing := filepath.Join(t.TempDir(), "no-such-dir", "x.bmpcap")
	err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(),
		"-collector", strings.TrimPrefix(ts.URL, "http://"),
		"-router", "10.0.0.1",
		"-o", missing,
		"-window", "300ms",
	})

	if err == nil {
		t.Fatal("cmdCapture accepted an output path in a directory that does not exist")
	}
	mu.Lock()
	gotArms := arms
	mu.Unlock()
	if gotArms != 0 {
		t.Errorf("collector saw %d arm requests, want 0: the mirror was armed before -o was known to be writable, "+
			"so a long -window burns the collector's byte budget and is then thrown away", gotArms)
	}
}

// TestCaptureFailureLeavesAnExistingOutputFileIntact pins what the writability
// probe must not do. The probe opens -o to prove the path is usable before a
// long capture starts, and opening for write is one flag away from destroying
// whatever is already there: adding O_TRUNC would empty the operator's
// existing capture at the moment the command starts, then fail, leaving
// nothing behind at all.
//
// A previous capture sitting at that path is real data -- the overwrite this
// command does permit (TestCaptureOverwritesExistingOutputFile) happens on
// success, after new bytes exist to replace it with. A run that fails must
// leave it exactly as it found it.
func TestCaptureFailureLeavesAnExistingOutputFileIntact(t *testing.T) {
	nc, _ := natstest.RunJSConn(t) // no EnsureStreams: RAW does not exist, so the replay fails
	out := filepath.Join(t.TempDir(), "previous.bmpcap")
	previous := []byte("an earlier capture worth keeping")
	if err := os.WriteFile(out, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(), "-router", "10.0.0.1", "-o", out, "-since", "1h",
	}); err == nil {
		t.Fatal("want an error when the RAW stream has never been provisioned")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the pre-existing capture file is gone after a failed run: %v", err)
	}
	if !bytes.Equal(got, previous) {
		t.Errorf("pre-existing capture was modified by a failed run: got %q, want %q", got, previous)
	}
}

// TestWriteCaptureKeepsTheSidecarAndCaptureConsistent pins that
// the pair must never end up describing different captures. writeCapture wrote
// the capture bytes first and the sidecar second, so anything that failed in
// between -- a pre-existing read-only sidecar reproduces it,
// and ENOSPC has the same shape -- left the NEW capture on disk beside the OLD
// router and message count.
//
// That is worse than either failing outright: a corpus fixture whose sidecar
// attributes it to the wrong router is not detectably wrong later, it is just
// wrong. The assertion is therefore consistency, not success -- whichever
// capture is on disk, the sidecar beside it must describe that one.
func TestWriteCaptureKeepsTheSidecarAndCaptureConsistent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "pair.bmpcap")

	// A previous, complete run: one message, sidecar to match.
	old := [][]byte{bmptest.Termination(0)}
	if err := writeCapture(out, old, captureMeta{Router: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	// The sidecar is now read-only, reproducing the exact failure mode.
	if err := os.Chmod(out+".json", 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(out+".json", 0o644) })

	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"),
		AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	fresh := [][]byte{
		bmptest.Init("rr2", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.Stats(ph, map[uint32]uint64{0: 1}),
		bmptest.Termination(0),
	}
	// Its error is not asserted: either outcome is legitimate, so long as
	// what lands on disk is coherent.
	_ = writeCapture(out, fresh, captureMeta{Router: "10.0.0.2"})

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("capture file is gone: %v", err)
	}
	defer f.Close()
	var onDisk int
	for {
		if _, err := bmp.ReadMsg(f); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("bmp.ReadMsg: %v", err)
		}
		onDisk++
	}

	side, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatalf("sidecar is gone: %v", err)
	}
	var meta captureMeta
	if err := json.Unmarshal(side, &meta); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v", err)
	}

	if meta.MessageCount != onDisk {
		t.Errorf("sidecar says %d messages, capture holds %d: the pair describes two different runs",
			meta.MessageCount, onDisk)
	}
	wantRouter := map[int]string{len(old): "10.0.0.1", len(fresh): "10.0.0.2"}[onDisk]
	if meta.Router != wantRouter {
		t.Errorf("capture holds %d messages but the sidecar names router %q (want %q): "+
			"the fixture is attributed to the wrong router", onDisk, meta.Router, wantRouter)
	}
}

// TestCaptureRejectsAZonedRouter pins that a zoned -router
// arms a mirror and then captures nothing.
//
// The mirroring itself works: MirrorRegistry canonicalizes with
// Unmap().WithZone("") on Arm, Disarm and Take alike, so the session's zoned
// address still matches the armed entry. The subject filter is what breaks.
// subjects.EncodeIP appends a "-z<fnv32(zone)>" suffix, and the zone the
// collector sees is its OWN interface name for that link-local peer, not
// anything the operator can name from the router's side. So the filter is
// built from one zone, the collector publishes under another, and the capture
// comes back empty and exits 0 -- the worst outcome available, because an
// empty corpus file is not detectably wrong later.
//
// Rejecting at flag-parse time turns that into an error. The assertion is
// again that nothing was armed: failing after arming would still burn the
// collector's byte budget.
func TestCaptureRejectsAZonedRouter(t *testing.T) {
	var mu sync.Mutex
	var arms int
	admin := collector.NewAdminHandler(collector.NewMirrorRegistry())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/admin/mirror" {
			mu.Lock()
			arms++
			mu.Unlock()
		}
		admin.ServeHTTP(w, r)
	}))
	defer ts.Close()

	err := cmdCapture([]string{
		"-collector", strings.TrimPrefix(ts.URL, "http://"),
		"-router", "fe80::1%eth0",
		"-o", filepath.Join(t.TempDir(), "zoned.bmpcap"),
		"-window", "300ms",
	})

	if err == nil {
		t.Fatal("cmdCapture accepted a zoned -router: it would arm a mirror and write an empty capture, exit 0")
	}
	if !strings.Contains(err.Error(), "zone") {
		t.Errorf("error %q does not mention the zone: the operator needs to know which part of -router was the problem", err)
	}
	mu.Lock()
	gotArms := arms
	mu.Unlock()
	if gotArms != 0 {
		t.Errorf("collector saw %d arm requests, want 0: rejected after arming still burns the byte budget", gotArms)
	}
}

// TestCaptureSidecarRecordsTheCanonicalRouter pins that
// meta.Router stored whatever the operator typed. An IPv4-mapped spelling
// ("::ffff:10.0.0.1") names the same router the collector calls "10.0.0.1" --
// server.go canonicalizes with ap.Addr().Unmap() before building the session
// token, so every envelope inside the capture carries the plain form. A
// sidecar disagreeing with the bytes beside it is the same provenance defect
// as a stale sidecar, just introduced at a different point: nothing later can
// join this fixture to the router it came from by string equality.
func TestCaptureSidecarRecordsTheCanonicalRouter(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := natsutil.EnsureStreams(context.Background(), js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "mapped.bmpcap")
	if err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(), "-router", "::ffff:10.0.0.1", "-o", out, "-since", "1h",
	}); err != nil {
		t.Fatalf("cmdCapture: %v", err)
	}

	side, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var meta captureMeta
	if err := json.Unmarshal(side, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Router != "10.0.0.1" {
		t.Errorf(`sidecar "router" = %q, want "10.0.0.1": the collector names this router by its unmapped form, `+
			`so the sidecar cannot be matched to the envelopes it describes`, meta.Router)
	}
}

// TestDisarmMirrorReportsFailure pins that disarmMirror threw
// every error away. It runs from cmdCapture's defer, so a collector that is
// down, or answers 500, left the mirror armed and still consuming the byte
// budget with nothing printed and exit 0. The operator's next capture then
// competes with a mirror they believe they released.
func TestDisarmMirrorReportsFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	err := disarmMirror(adminClient, strings.TrimPrefix(ts.URL, "http://"), netip.MustParseAddr("10.0.0.1"))
	if err == nil {
		t.Fatal("disarmMirror returned nil when the collector answered 500: the mirror is still armed and nothing says so")
	}
}

// TestDisarmMirrorEscapesTheRouterPath covers the other half of the same
// finding. A zone makes the address contain a '%', which http.NewRequest
// rejects as an invalid URL escape ("%et"), so the request was never even
// built and the discarded error hid that too. cmdCapture now refuses a zoned
// -router upstream, but disarmMirror takes any netip.Addr its callers hand
// it, so it escapes the path itself rather than depending on that.
func TestDisarmMirrorEscapesTheRouterPath(t *testing.T) {
	gotPath := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	zoned := netip.MustParseAddr("fe80::1%eth0")
	if err := disarmMirror(adminClient, strings.TrimPrefix(ts.URL, "http://"), zoned); err != nil {
		t.Fatalf("disarmMirror on a zoned address: %v -- the request was never built", err)
	}
	select {
	case p := <-gotPath:
		if want := "/admin/mirror/" + zoned.String(); p != want {
			t.Errorf("collector received path %q, want %q", p, want)
		}
	default:
		t.Fatal("no request reached the collector: the zoned address was never escaped into a valid URL")
	}
}

// TestDrainRawStopsAtTheByteBudget pins that -max-bytes was
// silently ignored in -since mode, where nothing bounded memory at all: the
// whole replay accumulates in one slice before anything is written.
//
// A budget that stops mid-message would corrupt the capture -- a partial BMP
// message is not a message -- so the budget stops on a whole-message
// boundary, and the shortfall rides the same Complete flag a transport
// failure does. Without that the tool would silently return a subset again,
// the same defect class this file already guards against elsewhere.
func TestDrainRawStopsAtTheByteBudget(t *testing.T) {
	payload := []byte{3, 0, 0, 0, 6, 1} // 6 bytes per kept message
	var calls int
	next := func() ([]byte, error) {
		calls++
		return rawEnvelopeBytes(t, payload), nil
	}

	// Room for two whole messages, not three.
	msgs, meta, complete := drainRaw(3, 14, next)

	if complete {
		t.Error("drainRaw reported complete after the byte budget cut the replay short")
	}
	if len(msgs) != 2 {
		t.Errorf("drainRaw kept %d messages under a 14-byte budget at 6 bytes each, want 2", len(msgs))
	}
	for i, m := range msgs {
		if !bytes.Equal(m, payload) {
			t.Errorf("message %d is %x, want the whole %x: the budget split a message", i, m, payload)
		}
	}
	_ = meta
}

// TestDrainRawUnderTheByteBudgetIsComplete is the discriminating half: a
// replay that fits must still report complete, or the flag means nothing.
func TestDrainRawUnderTheByteBudgetIsComplete(t *testing.T) {
	payload := []byte{3, 0, 0, 0, 6, 1}
	next := func() ([]byte, error) { return rawEnvelopeBytes(t, payload), nil }

	msgs, _, complete := drainRaw(2, 1<<20, next)

	if !complete {
		t.Error("drainRaw reported incomplete for a replay well under its byte budget")
	}
	if len(msgs) != 2 {
		t.Errorf("drainRaw kept %d messages, want 2", len(msgs))
	}
}

// TestCaptureReplayTimeoutIsHonored pins that the replay ceiling comes from
// the flag rather than a hard-coded constant. The 2-minute bound was
// previously absent from the flag set: a replay larger than it fits
// simply stopped, saying nothing about it.
//
// A 1ns budget cannot complete, so the run must fail rather than report a
// whole capture. Asserting failure is the point -- a flag that is parsed and
// then ignored would leave this green.
func TestCaptureReplayTimeoutIsHonored(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := natsutil.EnsureStreams(context.Background(), js, captureTestStreamOpts); err != nil {
		t.Fatal(err)
	}
	err := cmdCapture([]string{
		"-nats", nc.ConnectedUrl(), "-router", "10.0.0.1",
		"-o", filepath.Join(t.TempDir(), "timeout.bmpcap"),
		"-since", "1h", "-replay-timeout", "1ns",
	})
	if err == nil {
		t.Fatal("cmdCapture succeeded with -replay-timeout 1ns: the flag is parsed but not honored")
	}
}
