// reparse.go implements `vantage reparse`: replay a capture file through the
// same session layer and parser library the collector uses, and print what
// comes out.
//
// Read-only by design. Re-emitting corrected data into NATS raises backfill
// semantics -- msg-ids dedup inside the 2m duplicate window and duplicate
// outside it -- that belong with the writer that persists corrected data,
// not with a debugging verb.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/collector"
)

// replayCapture feeds every BMP message in the capture at path through one
// Session and returns the events produced.
//
// A malformed or truncated record returns the events recovered before it
// alongside the error, rather than only the error. This was carried
// forward as a warning: a capture whose final record is short yields
// io.ErrUnexpectedEOF with every earlier message intact, so a flat rejection
// lets one bad byte discard an entire corpus. The messages that did parse are
// the context an operator needs for the one that did not. This mirrors the
// contract `vantage capture` settled on -- keep the partial result, still
// report the failure.
func replayCapture(path string) ([]collector.Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read capture: %w", err)
	}
	// Router identity is cosmetic here; the capture's sidecar carries the real
	// one, and the parsers do not depend on it.
	router := netip.MustParseAddr("127.0.0.1")
	if meta, err := readCaptureMeta(path); err == nil && meta.Router != "" {
		if a, err := netip.ParseAddr(meta.Router); err == nil {
			router = a
		}
	}
	s := collector.NewSession(router, "reparse", uint64(time.Now().UnixNano()), time.Now, collector.Overrides{})

	var out []collector.Event
	r := bytes.NewReader(b)
	for {
		m, err := bmp.ReadMsg(r)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("capture is not a valid BMP byte stream at offset %d: %w", len(b)-r.Len(), err)
		}
		out = append(out, s.Handle(m)...)
	}
}

// readCaptureMeta reads the sidecar written beside a capture. Its absence is
// not fatal to a replay -- the parsers do not depend on router identity -- so
// callers treat the error as "no provenance available" rather than a failure.
func readCaptureMeta(path string) (captureMeta, error) {
	var m captureMeta
	b, err := os.ReadFile(path + ".json")
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func cmdReparse(args []string) error {
	fs := flag.NewFlagSet("reparse", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit one JSON envelope per line instead of pretty-printing")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("reparse: exactly one capture file is required")
	}
	// Printed before the error is returned, for the same reason capture writes
	// its short file: what was recovered is the useful half of a failed replay.
	evs, replayErr := replayCapture(fs.Arg(0))
	for _, ev := range evs {
		if *asJSON {
			b, err := protojson.Marshal(ev.Env)
			if err != nil {
				return fmt.Errorf("encode envelope: %w", err)
			}
			fmt.Println(string(b))
			continue
		}
		printEnv(ev.Subject, mustMarshal(ev))
	}
	fmt.Fprintf(os.Stderr, "replayed %d envelopes\n", len(evs))
	return replayErr
}

// mustMarshal re-encodes an envelope so printEnv, which takes wire bytes, can
// render a replayed event exactly as it renders a live one.
func mustMarshal(ev collector.Event) []byte {
	b, err := proto.Marshal(ev.Env)
	if err != nil {
		return nil
	}
	return b
}
