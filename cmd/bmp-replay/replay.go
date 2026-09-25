package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jp2195/vantage/bmp"
)

// captureMessages reads a `vantage capture` file and splits it on BMP message
// boundaries, returning the original bytes of each message.
//
// It slices the file rather than re-serializing what bmp.ReadMsg parsed. A
// replayer's whole value is that the collector sees the bytes the router
// actually sent, so the framing here validates and cuts but never rewrites --
// reconstructing a header from a parsed Msg would quietly normalize anything
// the capture recorded and make this tool a worse witness than the file. The
// single deliberate exception is prefixSysName below, which edits one TLV and
// says why.
func captureMessages(path string) ([][]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read capture: %w", err)
	}
	var msgs [][]byte
	for off := 0; off < len(b); {
		if len(b)-off < bmp.HeaderLen {
			return nil, fmt.Errorf("%s: %d trailing bytes at offset %d are shorter than a BMP header",
				path, len(b)-off, off)
		}
		if b[off] != bmp.Version {
			return nil, fmt.Errorf("%s: offset %d: %w: %d", path, off, bmp.ErrBadVersion, b[off])
		}
		l := int(binary.BigEndian.Uint32(b[off+1 : off+5]))
		if l < bmp.HeaderLen || l > bmp.MaxMsgLen {
			return nil, fmt.Errorf("%s: offset %d: %w: %d", path, off, bmp.ErrBadLength, l)
		}
		if off+l > len(b) {
			return nil, fmt.Errorf("%s: offset %d: message claims %d bytes but only %d remain",
				path, off, l, len(b)-off)
		}
		msgs = append(msgs, b[off:off+l])
		off += l
	}
	return msgs, nil
}

// parseTargetDelays splits the -target-delay flag into a per-target wait.
//
// WHAT IT IS FOR. A collector that has stalled records events LATE, and the
// archive cannot tell that apart from a sender that wrote late: ts_collector
// is each collector's own clock and is the only timestamp either one leaves.
// That equivalence is what makes this flag an honest reproduction rather
// than a simulation.
//
// It exists because evpn-churn inventing a flap from a second collector's
// late copy of an advertisement can only be reproduced when two collectors
// disagree about WHEN they saw one event. Proving it the first time took a
// live FRR speaker and a paused container. With this flag and the capture
// committed (frr-10.3-evpn-terminal-withdraw.bmpcap) it is one command.
//
// The key must carry a port, matching parseTargets, and for the same reason:
// a key that does not match a target exactly is a typo that would otherwise
// apply no delay and report success. A zero delay is allowed and means what
// it says; a NEGATIVE one is refused, because every sleep clamps it to zero
// and the flag would then quietly succeed at doing nothing.
func parseTargetDelays(s string) (map[string]time.Duration, error) {
	out := map[string]time.Duration{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty entry in %q", s)
		}
		target, dur, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("target delay %q: want host:port=duration", part)
		}
		target = strings.TrimSpace(target)
		if _, _, err := net.SplitHostPort(target); err != nil {
			return nil, fmt.Errorf("target delay %q: %w", part, err)
		}
		d, err := time.ParseDuration(strings.TrimSpace(dur))
		if err != nil {
			return nil, fmt.Errorf("target delay %q: %w", part, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("target delay %q: negative", part)
		}
		out[target] = d
	}
	return out, nil
}

// replay dials every target, then writes the capture to all of them, and
// returns the open connections for the caller to hold.
//
// The two phases are the whole point, and the order is not a style choice.
// This command's only job is to make one router visible to several collectors
// at once; a sender that dialed and fed each target in turn would, on failing
// to reach the second collector, leave the first holding a complete session
// and the archive in exactly the single-collector state this is meant to fix.
// The operator would see an error naming the collector that failed and no
// sign that the other one now carries a router nothing else does. So a target
// that cannot be dialed means nobody is fed, and replay closes what it opened
// rather than handing back connections the caller did not ask for.
//
// A write that fails partway is not recoverable the same way -- those bytes
// are gone -- so it is reported with every connection closed, leaving a
// truncated session the collector will end rather than a silent half-feed.
func replay(targets []string, msgs [][]byte, timeout time.Duration) ([]net.Conn, error) {
	return replayWithDelays(targets, msgs, timeout, nil)
}

// replayWithDelays is replay with a per-target wait before that target's
// first byte -- see parseTargetDelays for what the wait is modeling.
//
// The targets are fed CONCURRENTLY, which the undelayed path did not need
// and this one does: feeding them in turn would make every target wait out
// the delays of the targets before it, so "one collector lags by a minute"
// would become "every later collector lags by a minute", and the ordering
// the flag exists to create would depend on the order of -targets.
//
// The all-or-nothing dial contract is UNCHANGED and is still enforced before
// any byte is written, for the reason spelled out above: a target that
// cannot be dialed must mean nobody is fed. Only the writing is concurrent.
//
// A write that fails is still reported with every connection closed. The
// first error wins; the rest are dropped, because they are overwhelmingly
// the same broken-pipe cascade and naming one target the operator must fix
// beats naming all of them.
func replayWithDelays(targets []string, msgs [][]byte, timeout time.Duration, delays map[string]time.Duration) ([]net.Conn, error) {
	conns := make([]net.Conn, 0, len(targets))
	for _, t := range targets {
		c, err := net.DialTimeout("tcp", t, timeout)
		if err != nil {
			closeAllConns(conns)
			return nil, fmt.Errorf("dial %s: %w", t, err)
		}
		conns = append(conns, c)
	}

	errs := make(chan error, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d := delays[targets[i]]; d > 0 {
				time.Sleep(d)
			}
			for _, m := range msgs {
				if _, err := c.Write(m); err != nil {
					errs <- fmt.Errorf("write to %s: %w", targets[i], err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		closeAllConns(conns)
		return nil, err
	}
	return conns, nil
}

func closeAllConns(conns []net.Conn) {
	for _, c := range conns {
		c.Close()
	}
}

// prefixSysName rewrites the sysName in the capture's Initiation message,
// leaving every other byte of every message alone.
//
// This is the one place the replayer edits what it sends, and the reason is
// the same one cmd/vantage/bmpgen.go spells out: a capture carries two
// identities and they need opposite treatment. sysDescr is the VENDOR banner
// the collector's quirk matching keys off, and inventing one puts traffic on
// the wire under an identity no implementation has been observed sending --
// so it survives verbatim. sysName names a DEVICE, and replaying it as-is
// files every row under the real lab router the bytes came from. bmpgen
// once did exactly that: 480 rows filed under a real lab device's own
// hostname, indistinguishable from that device's own rows; here it showed
// up as the dashboards' $router list offering two entries both reading
// "nx-p3".
//
// The prefix rather than a replacement keeps the provenance legible:
// "replay-nx-p3" still says which capture is on the wire.
//
// The payload is rebuilt TLV by TLV rather than patched in place, because the
// new name changes the message's length; TLVs this project does not decode
// are copied through in order, since a rebuild that emitted only the two it
// understands would silently strip whatever else the router sent.
func prefixSysName(msgs [][]byte, prefix string) ([][]byte, error) {
	if prefix == "" {
		return msgs, nil
	}
	out := make([][]byte, len(msgs))
	copy(out, msgs)
	for i, m := range msgs {
		if len(m) < bmp.HeaderLen || m[5] != bmp.TypeInitiation {
			continue
		}
		tlvs, err := bmp.ParseTLVs(m[bmp.HeaderLen:])
		if err != nil {
			return nil, fmt.Errorf("initiation at message %d: %w", i, err)
		}
		var payload []byte
		for _, t := range tlvs {
			v := t.Value
			if t.Type == tlvSysName {
				v = append([]byte(prefix), v...)
			}
			payload = bmp.AppendTLV(payload, t.Type, v)
		}
		msg := append([]byte{bmp.Version, 0, 0, 0, 0, bmp.TypeInitiation}, payload...)
		binary.BigEndian.PutUint32(msg[1:5], uint32(len(msg)))
		out[i] = msg
	}
	return out, nil
}

// tlvSysName is RFC 7854 §4.3's sysName TLV. bmp keeps its own copy
// unexported -- it exposes ParseInit for reading and the generic TLV
// primitives for everything else -- and this is the one caller that needs to
// rewrite the field rather than read it.
const tlvSysName = 2
