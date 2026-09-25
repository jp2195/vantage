// capture.go implements `vantage capture`: pull a router's BMP messages out
// of the raw stream and write them to a file that is directly replayable by
// `vantage reparse` and directly committable as a golden fixture.
//
// The file format is deliberately dumb -- raw BMP messages concatenated
// exactly as received, with no delimiter of any kind, plus a sidecar .json
// for provenance. No delimiter is needed: every BMP message already
// declares its own total length in its 6-byte common header
// (bmp.HeaderLen), so a reader that calls bmp.ReadMsg in a loop
// over the concatenated bytes recovers exactly the original message
// boundaries -- see TestWriteCaptureRoundTripsThroughBmpReadMsg. The file IS
// what the parsers eat, so there is no format to keep in sync as the schema
// moves.
//
// Two modes, because the two reasons to capture are opposites in time.
// Forward (-window) arms a mirror and collects traffic that has not
// happened yet -- corpus building. Backward (-since) replays what is
// already in the raw stream -- anomaly capture. They are mutually
// exclusive, and supplying neither is an error rather than a silent
// default.
//
// collectRaw takes every RawEvent for the router regardless of its Mirrored
// flag. Mirrored distinguishes a deliberate mirror-mode republish from a
// parse failure, Termination, or Route Mirroring message that
// Session.Handle already put on the raw subject on its own (see
// collector/server.go's handleConn and session.go's
// rawEvent/MirrorEvent) -- both land on the identical subject, and both are
// exactly what a corpus wants. A filter on mirrored==true would silently
// drop every parse failure an armed router produced -- the single most
// valuable fixture this command could collect -- so collectRaw never
// inspects the flag to decide inclusion; see
// TestCollectRawIncludesUnmirroredRawEvents.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
)

// captureMeta is the sidecar written alongside every capture, so a corpus
// sample is self-describing years after the session that produced it.
type captureMeta struct {
	Router       string    `json:"router"`
	SysDescr     string    `json:"sys_descr"`
	Vendor       string    `json:"vendor"`
	OS           string    `json:"os"`
	Version      string    `json:"version"`
	CapturedAt   time.Time `json:"captured_at"`
	MessageCount int       `json:"message_count"`
	Mode         string    `json:"mode"` // "window" or "since"

	// Expected is how many messages the replay was promised (the consumer's
	// NumPending at the moment it started); Complete reports whether it
	// consumed all of them. Without these a capture cut short by the replay
	// deadline, a second ctrl-C or any transport error is byte-identical to
	// a legitimate short one, and a golden fixture silently missing its tail
	// is exactly the failure a regression corpus cannot detect later.
	// Both modes report these: a forward capture ends by replaying the
	// window it just mirrored, through the same consumer, so it is truncated
	// by a transport failure in exactly the same way a -since replay is.
	Expected uint64 `json:"expected"`
	Complete bool   `json:"complete"`
}

// writeCapture writes the concatenated BMP messages and the sidecar,
// replacing whatever was already at path and path+".json" in full.
//
// This is a deliberate choice, not an oversight: the operator named this
// exact path with -o, and -- like `tcpdump -w` or a shell `>` redirect --
// the tool trusts that choice rather than second-guessing it with a clobber
// guard nothing else in this codebase's CLI conventions has either. See
// TestCaptureOverwritesExistingOutputFile.
// The capture and its sidecar are staged as temporary files and renamed into
// place, rather than written directly. The pair must never describe two
// different runs: writing the capture first and the sidecar second meant
// anything failing in between -- a pre-existing read-only sidecar, ENOSPC --
// left the new bytes on disk beside the previous run's router and message
// count. A fixture attributed to the wrong router is not detectably wrong
// later; it is just wrong. Staging both before either is published means a
// failure leaves the previous pair untouched and coherent.
//
// Renaming also fixes the read-only case outright rather than merely making
// it consistent: rename needs write permission on the directory, not on the
// file it replaces.
func writeCapture(path string, msgs [][]byte, meta captureMeta) error {
	var buf bytes.Buffer
	for _, m := range msgs {
		buf.Write(m)
	}
	meta.MessageCount = len(msgs)
	if meta.CapturedAt.IsZero() {
		meta.CapturedAt = time.Now().UTC()
	}
	// Encoded before anything touches the filesystem, so a marshal failure
	// cannot be the thing that splits the pair.
	side, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sidecar: %w", err)
	}

	capTmp, sideTmp := path+".tmp", path+".json.tmp"
	// Both removals are no-ops once the rename below has consumed the file;
	// they matter on the paths that return early.
	defer os.Remove(capTmp)
	defer os.Remove(sideTmp)
	if err := os.WriteFile(capTmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write capture: %w", err)
	}
	if err := os.WriteFile(sideTmp, append(side, '\n'), 0o644); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	if err := os.Rename(capTmp, path); err != nil {
		return fmt.Errorf("write capture: %w", err)
	}
	if err := os.Rename(sideTmp, path+".json"); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	return nil
}

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	natsURL := natsURLFlag(fs)
	natsTLS := natsTLSFlags(fs)
	collectorAddr := fs.String("collector", "127.0.0.1:9470", "collector admin address")
	router := fs.String("router", "", "router IP to capture (required)")
	out := fs.String("o", "", "output file (required)")
	window := fs.Duration("window", 0, "arm a mirror and capture forward for this long")
	since := fs.Duration("since", 0, "replay what is already in the raw stream from this long ago")
	maxBytes := fs.Int64("max-bytes", 64<<20, "byte budget for the capture, in both modes")
	replayTimeout := fs.Duration("replay-timeout", 2*time.Minute, "ceiling on the replay step")
	fs.Parse(args)

	if *router == "" || *out == "" {
		return fmt.Errorf("capture: -router and -o are required")
	}
	addr, err := netip.ParseAddr(*router)
	if err != nil {
		return fmt.Errorf("capture: -router %q is not an IP address: %w", *router, err)
	}
	// A zone makes the capture silently empty, so refuse it rather than
	// produce one. Mirroring would work -- MirrorRegistry canonicalizes with
	// Unmap().WithZone("") on Arm, Disarm and Take alike -- but the subject
	// filter would not: subjects.EncodeIP appends a "-z<fnv32(zone)>" suffix,
	// and the zone the collector sees is its own interface name for that
	// link-local peer, which nothing on the router's side can tell the
	// operator. The filter would be built from one zone and the collector
	// would publish under another, leaving an empty capture that exits 0.
	if addr.Zone() != "" {
		return fmt.Errorf("capture: -router %q carries a zone (%q); the collector names this router by its own zone for the link, "+
			"which cannot be known from here, so the capture would match nothing -- pass %q instead",
			*router, addr.Zone(), addr.WithZone("").String())
	}
	// Exactly one mode. Defaulting silently would do the wrong thing half the
	// time: corpus building needs traffic that has not happened yet, anomaly
	// capture needs bytes that are already in the stream.
	switch {
	case *window == 0 && *since == 0:
		return fmt.Errorf("capture: one of -window (capture forward) or -since (replay past) is required")
	case *window != 0 && *since != 0:
		return fmt.Errorf("capture: -window and -since are mutually exclusive")
	}

	// Prove -o is writable before anything expensive happens. Validated only
	// at the final write, a typo in a directory name costs the entire
	// capture: `-window 6h` would arm a mirror, burn six hours and the
	// collector's byte budget, and then discard everything gathered because
	// the destination never existed.
	//
	// The probe opens the real path rather than stat-ing its directory, so
	// it exercises the same permission and path resolution the final write
	// will hit -- a -o naming an existing read-only file, or a directory,
	// fails here rather than at the end. O_TRUNC is deliberately absent: the
	// operator's existing file must survive a capture that later fails, and
	// it is writeCapture's job, not this check's, to replace it.
	//
	// Whatever this creates, it removes. Leaving an empty file behind would
	// break the guarantee TestCaptureSinceErrorsWhenRawStreamMissing pins --
	// a failed capture writes no capture file -- and an empty .bmpcap that
	// looks like a real one is its own trap for a corpus.
	_, statErr := os.Stat(*out)
	preexisting := statErr == nil
	probe, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("capture: -o %s is not writable: %w", *out, err)
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("capture: -o %s: %w", *out, err)
	}
	if !preexisting {
		os.Remove(*out)
	}

	nurl := natsFlag(natsURL)
	nc, err := natsDial(nurl, *natsTLS)
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", nurl, err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	mode := "since"
	start := time.Now().Add(-*since)
	if *window != 0 {
		mode = "window"
		if err := armMirror(adminClient, *collectorAddr, addr, *window, *maxBytes); err != nil {
			return err
		}
		// Reported, not swallowed: a mirror left armed keeps consuming the
		// collector's byte budget, and the operator's next capture would
		// compete with one they believe they released.
		defer func() {
			if err := disarmMirror(adminClient, *collectorAddr, addr); err != nil {
				fmt.Fprintf(os.Stderr, "capture: %v -- the mirror may still be armed\n", err)
			}
		}()
		start = time.Now()
		fmt.Fprintf(os.Stderr, "mirror armed for %s on %s; capturing…\n", *window, addr)

		// Ctrl-C stops the wait early rather than killing the process (the
		// default disposition for os.Interrupt, which signal.NotifyContext
		// suppresses for as long as this context is live) -- matching
		// `vantage debug` and `vantage bmpgen`'s own signal handling. The
		// mirror is already armed and collecting on the collector side, so
		// an operator who interrupts a long -window capture should still get
		// everything mirrored up to that point, not nothing. See
		// waitWindow's tests.
		waitCtx, stopWait := signal.NotifyContext(context.Background(), os.Interrupt)
		if waitWindow(waitCtx, *window) {
			fmt.Fprintln(os.Stderr, "capture: interrupted; disarming mirror and writing what was captured so far")
		}
		stopWait()
	}

	// A second, independent interrupt scope for the replay step below. It
	// must be independent of the wait's scope above: once one os.Interrupt
	// has fired on a context, that context stays done forever, so reusing it
	// here would make an interrupted forward capture fail the replay outright
	// instead of writing the partial capture ctrl-C was supposed to preserve.
	// A second ctrl-C during replay still gets caught, and collectRaw stops
	// early with whatever it has already gathered (see its "stopped early"
	// path) rather than this process needing to be killed.
	replayCtx, stopReplay := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopReplay()

	filter := subjects.Raw(subjects.EncodeIP(addr))
	msgs, meta, err := collectRaw(replayCtx, js, filter, start, *maxBytes, *replayTimeout)
	if err != nil {
		return err
	}
	// Unmap, not the operator's spelling: server.go canonicalizes with
	// ap.Addr().Unmap() before building the session token, so every envelope
	// in this capture names the router in its plain form. A sidecar recording
	// "::ffff:10.0.0.1" beside envelopes saying "10.0.0.1" cannot be joined
	// to them by string equality.
	meta.Router = addr.Unmap().String()
	meta.Mode = mode
	return finishCapture(*out, msgs, meta)
}

// finishCapture writes the capture and its sidecar, then reports a truncated
// replay as an error.
//
// The order is the contract. A short capture is still written -- a partial
// corpus is useful, and the operator asked for this path with -o -- but the
// command must not exit 0 on one, because a golden fixture silently missing
// its tail is indistinguishable from a legitimate short one and no later
// regression run can detect the difference. Writing without erroring, or
// erroring without writing, each satisfies half of that and fails the
// operator in a different direction.
func finishCapture(path string, msgs [][]byte, meta captureMeta) error {
	if err := writeCapture(path, msgs, meta); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d messages to %s (+ .json)\n", len(msgs), path)
	if !meta.Complete {
		return fmt.Errorf("capture is incomplete: replay stopped after %d of %d promised messages; %s holds what was gathered",
			len(msgs), meta.Expected, path)
	}
	return nil
}

// waitWindow blocks until d elapses or ctx is done, whichever comes first,
// reporting whether ctx ended it early. Factored out of cmdCapture so a
// forward capture's ctrl-C handling is directly testable without sending a
// real OS signal.
func waitWindow(ctx context.Context, d time.Duration) (interrupted bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-ctx.Done():
		return true
	}
}

// collectRaw replays the raw stream for filter from start, returning the
// original BMP bytes of every RawEvent it finds -- mirrored or not (see this
// file's package doc comment for why filtering on Mirrored would be wrong).
// ctx bounds the whole replay; canceling it (including via cmdCapture's
// ctrl-C handling) makes an in-progress fetch fail, which is treated the
// same as any other stopped-early replay: whatever was already gathered is
// returned with a nil error, not discarded.
func collectRaw(ctx context.Context, js jetstream.JetStream, filter string, start time.Time, maxBytes int64, timeout time.Duration) ([][]byte, captureMeta, error) {
	var meta captureMeta
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cons, err := js.OrderedConsumer(ctx, "RAW", jetstream.OrderedConsumerConfig{
		DeliverPolicy:  jetstream.DeliverByStartTimePolicy,
		OptStartTime:   &start,
		FilterSubjects: []string{filter},
	})
	if err != nil {
		return nil, meta, fmt.Errorf("replay RAW: %w", err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return nil, meta, fmt.Errorf("replay RAW: %w", err)
	}
	out, meta, _ := drainRaw(info.NumPending, maxBytes, func() ([]byte, error) {
		m, err := cons.Next(jetstream.FetchContext(ctx))
		if err != nil {
			return nil, err
		}
		return m.Data(), nil
	})
	return out, meta, nil
}

// drainRaw consumes the expected messages next yields, returning the BMP
// payloads it kept, the provenance it learned from the first envelope
// carrying RouterInfo, and whether it consumed every message it was promised.
//
// next abstracts the fetch rather than taking a jetstream.Consumer directly
// so that a mid-replay transport failure -- the exact condition that makes a
// truncated capture look whole -- is reachable from a test without mutating
// this file. That failure has no other seam: it cannot be provoked through a
// real consumer deterministically, which is why demonstrating it requires
// editing the loop by hand.
//
// Messages that fail to unmarshal, and those carrying no BMP bytes, are
// skipped but still counted as consumed: they were delivered, so they are not
// a shortfall. Completeness tracks delivery, not what survived filtering --
// conflating the two would report every capture containing a non-Raw envelope
// as truncated.
func drainRaw(expected uint64, maxBytes int64, next func() ([]byte, error)) ([][]byte, captureMeta, bool) {
	meta := captureMeta{Expected: expected}
	var out [][]byte
	var consumed uint64
	var kept int64
	var truncated bool
	for n := expected; n > 0; n-- {
		data, err := next()
		if err != nil {
			fmt.Fprintf(os.Stderr, "replay RAW: stopped early: %v\n", err)
			break
		}
		consumed++
		env := &vantagev1.Envelope{}
		if err := proto.Unmarshal(data, env); err != nil {
			continue
		}
		if meta.SysDescr == "" && env.RouterInfo != nil {
			meta.SysDescr = env.RouterInfo.SysDescr
			meta.Vendor = env.RouterInfo.Vendor
			meta.OS = env.RouterInfo.Os
			meta.Version = env.RouterInfo.Version
		}
		// Mirrored is deliberately not consulted here -- see the package
		// doc comment. Every RawEvent for this router is captured,
		// regardless of why it reached the raw stream.
		if r := env.GetRaw(); r != nil && len(r.BmpMsg) > 0 {
			// The budget stops on a whole-message boundary. A partial BMP
			// message is not a message, and one written into a corpus file
			// would fail every later reader at a byte offset that says
			// nothing about why. The first message is always kept, so an
			// oversized one yields a capture that is honest about being
			// short rather than an empty file.
			if maxBytes > 0 && len(out) > 0 && kept+int64(len(r.BmpMsg)) > maxBytes {
				truncated = true
				break
			}
			kept += int64(len(r.BmpMsg))
			out = append(out, r.BmpMsg)
		}
	}
	// truncated is tracked separately from the consumed count: a budget that
	// rejects the very last message still consumed every one it was promised,
	// so the count alone would call that capture whole.
	meta.Complete = consumed == expected && !truncated
	return out, meta, meta.Complete
}

// adminRequestTimeout bounds each of the two admin-API calls below. Both
// send well under 100 bytes and read a response the collector produces
// without touching disk or network, so ten seconds is generous headroom
// rather than a fitted budget -- the point is that it is finite.
//
// A var, not a const, so the tests can shrink it. Production never assigns.
var adminRequestTimeout = 10 * time.Second

// adminClient is what both calls below use. http.DefaultClient (and the
// http.Post helper, which is DefaultClient) has NO timeout of any kind, so
// a collector that accepts the TCP connection and then never answers hangs
// this CLI forever with nothing to interrupt it: armMirror runs before the
// signal handler is installed, and disarmMirror runs from a defer during
// shutdown. Both are exactly where an unbounded wait is worst.
var adminClient = &http.Client{Timeout: adminRequestTimeout}

func armMirror(client *http.Client, collectorAddr string, router netip.Addr, window time.Duration, maxBytes int64) error {
	body := fmt.Sprintf(`{"router":%q,"window":%q,"max_bytes":%d}`, router.String(), window.String(), maxBytes)
	ctx, cancel := context.WithTimeout(context.Background(), adminRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+collectorAddr+"/admin/mirror", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("arm mirror on %s: %w", collectorAddr, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("arm mirror on %s: %w", collectorAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("arm mirror on %s: status %d", collectorAddr, resp.StatusCode)
	}
	return nil
}

// disarmMirror deliberately builds its context from context.Background()
// rather than accepting the caller's. It runs from cmdCapture's defer, and
// by then the capture context is routinely already canceled -- an operator
// who ended a -window capture with Ctrl-C is the ordinary case, not the
// exceptional one. Inheriting that context would make every interrupted
// capture fail its disarm instantly and leave the mirror armed on the
// collector, which is the one outcome this call exists to prevent.
func disarmMirror(client *http.Client, collectorAddr string, router netip.Addr) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminRequestTimeout)
	defer cancel()
	// PathEscape, because a zone puts a '%' in the address and
	// http.NewRequest rejects that as an invalid URL escape ("%et") before
	// the request is ever built. cmdCapture refuses a zoned -router upstream,
	// but this takes any netip.Addr a caller hands it.
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://"+collectorAddr+"/admin/mirror/"+url.PathEscape(router.String()), nil)
	if err != nil {
		return fmt.Errorf("disarm mirror on %s: %w", collectorAddr, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("disarm mirror on %s: %w", collectorAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("disarm mirror on %s: status %d", collectorAddr, resp.StatusCode)
	}
	return nil
}
