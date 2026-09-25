package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jp2195/vantage/bmp/bmptest"
)

// TestReparseReplaysACapture writes a capture the same way vantage capture
// does, then replays it and asserts the parsed envelopes come back.
func TestReparseReplaysACapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.bmpcap")
	msgs := [][]byte{
		bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.Termination(1),
	}
	if err := writeCapture(path, msgs, captureMeta{Router: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	evs, err := replayCapture(path)
	if err != nil {
		t.Fatal(err)
	}
	// Initiation produces no envelope; Termination produces a RawEvent.
	if len(evs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(evs))
	}
	if evs[0].Env.GetRaw() == nil {
		t.Fatalf("termination should replay as a RawEvent: %+v", evs[0].Env)
	}
	if evs[0].Env.Router.SysName != "rr1" {
		t.Fatalf("router identity from the replayed Initiation not applied: %+v", evs[0].Env.Router)
	}
}

func TestReparseRejectsMissingFile(t *testing.T) {
	if _, err := replayCapture(filepath.Join(t.TempDir(), "nope.bmpcap")); err == nil {
		t.Fatal("want an error for a missing capture")
	}
}

func TestReparseRejectsTruncatedCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.bmpcap")
	// A BMP header claiming more bytes than the file holds.
	if err := os.WriteFile(path, []byte{3, 0, 0, 0, 99, 4}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := replayCapture(path); err == nil {
		t.Fatal("want an error for a truncated capture")
	}
}

// TestReparseKeepsWhatItRecoveredFromATruncatedCapture is the other half of
// the test above, and answers a known warning carried forward from an
// earlier fix: a truncated final record yields io.ErrUnexpectedEOF with
// every earlier message intact, so treating it as a flat rejection lets one
// short byte discard an entire corpus.
//
// The contract is the same one `vantage capture` settled on: report the
// failure, but hand back everything that was recovered. An operator
// diagnosing a bad capture needs the messages that did parse -- they are the
// context for the one that did not.
func TestReparseKeepsWhatItRecoveredFromATruncatedCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.bmpcap")
	var b []byte
	b = append(b, bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2")...)
	b = append(b, bmptest.Termination(1)...)
	// A fourth header claiming 99 bytes with only two of them present.
	b = append(b, 3, 0, 0, 0, 99, 4)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	evs, err := replayCapture(path)

	if err == nil {
		t.Fatal("want an error for a capture whose last record is truncated")
	}
	if len(evs) != 1 {
		t.Fatalf("recovered %d envelopes, want the 1 that parsed before the truncation: "+
			"one short byte must not discard the whole capture", len(evs))
	}
	if evs[0].Env.Router.SysName != "rr1" {
		t.Errorf("recovered envelope lost its router identity: %+v", evs[0].Env.Router)
	}
}

// TestReparseTakesTheRouterFromTheSidecar pins that the sidecar's provenance
// is actually used. Without it every replayed envelope is attributed to the
// 127.0.0.1 placeholder, which silently makes replayed output un-joinable to
// the router the capture came from -- and 55 of the 76 committed corpus
// captures carry a sidecar, so this is the normal path, not an edge case.
//
// It is its own test because the whole block is deletable otherwise: removing
// the sidecar lookup left the rest of this file green.
func TestReparseTakesTheRouterFromTheSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.bmpcap")
	msgs := [][]byte{
		bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2"),
		bmptest.Termination(1),
	}
	if err := writeCapture(path, msgs, captureMeta{Router: "10.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	evs, err := replayCapture(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(evs))
	}
	if got := evs[0].Env.Router.Ip; got != "10.9.9.9" {
		t.Errorf("replayed envelope names router %q, want %q from the sidecar: "+
			"replayed output cannot be joined to the capture's own provenance", got, "10.9.9.9")
	}
}

// TestReparseWithoutASidecarStillReplays is the discriminating half: the
// sidecar is provenance, not input the parsers need, so its absence must not
// fail the replay. 21 of the committed corpus captures have none.
func TestReparseWithoutASidecarStillReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bare.bmpcap")
	var b []byte
	b = append(b, bmptest.Init("rr1", "Cisco IOS XR Software, Version 7.9.2")...)
	b = append(b, bmptest.Termination(1)...)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := replayCapture(path)
	if err != nil {
		t.Fatalf("a capture with no sidecar must still replay: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(evs))
	}
}
