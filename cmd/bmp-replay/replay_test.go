package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/bmp"
)

// lsFixture is the capture the dev stack replays: of every capture in the
// corpus, it is the only fixture that carries link-state nodes, links,
// prefixes AND VPN routes in a single session, which is what lets one
// connection dual-home every table the link-state dashboards read.
const lsFixture = "../../bgp/testdata/corpus/nxos/n9kv-10.6.2-p3-ospfv3-linkstate.bmpcap"

// TestCaptureMessagesFramesEveryBMPMessage pins that a capture is split on
// BMP message boundaries -- not handed to the socket as an opaque blob -- so
// the sender can report what it sent and refuse a file it cannot frame.
func TestCaptureMessagesFramesEveryBMPMessage(t *testing.T) {
	msgs, err := captureMessages(lsFixture)
	if err != nil {
		t.Fatalf("captureMessages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages framed")
	}
	// Every framed message must read back as a BMP message, and the
	// concatenation must be the file itself -- a framer that dropped or
	// reordered a byte would still produce parseable messages.
	var joined []byte
	for i, raw := range msgs {
		if _, err := bmp.ReadMsg(bytes.NewReader(raw)); err != nil {
			t.Fatalf("message %d does not parse: %v", i, err)
		}
		joined = append(joined, raw...)
	}
	want := readFile(t, lsFixture)
	if !bytes.Equal(joined, want) {
		t.Fatalf("framed bytes (%d) differ from the capture (%d)", len(joined), len(want))
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestCaptureMessagesRefusesATruncatedCapture pins the one place this sender
// deliberately disagrees with `vantage reparse`. reparse keeps the messages it
// recovered before a short record, because an operator debugging a capture
// wants the context. A sender must not: half a capture on the wire is a
// session the collector accepts, records peers for, and then holds as current
// state -- a partial RIB indistinguishable from a complete one.
func TestCaptureMessagesRefusesATruncatedCapture(t *testing.T) {
	full := readFile(t, lsFixture)
	path := filepath.Join(t.TempDir(), "truncated.bmpcap")
	// Cut one byte off the end: every message but the last frames cleanly,
	// so a framer that returned what it had would look successful.
	if err := os.WriteFile(path, full[:len(full)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	msgs, err := captureMessages(path)
	if err == nil {
		t.Fatalf("truncated capture accepted: %d messages", len(msgs))
	}
	if msgs != nil {
		t.Fatalf("got %d messages alongside the error; a partial capture must yield none", len(msgs))
	}
}

// TestReplayWritesNothingWhenOneTargetIsUnreachable is the test this command
// exists for. The failure it guards against is silent and looks like success:
// if the sender dials collector A, writes the whole capture, then fails to
// reach collector B, the archive ends up with exactly the one-collector state
// dual-homing is meant to remove -- and the operator sees an error about B
// while A looks correctly fed. So every target is dialed before any byte is
// written, and a target that cannot be reached means nobody gets anything.
func TestReplayWritesNothingWhenOneTargetIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	msgs, err := captureMessages(lsFixture)
	if err != nil {
		t.Fatal(err)
	}
	conns, err := replay([]string{ln.Addr().String(), deadAddr(t)}, msgs, 2*time.Second)
	if err == nil {
		closeAll(conns)
		t.Fatal("replay succeeded with an unreachable target")
	}
	if len(conns) != 0 {
		closeAll(conns)
		t.Fatalf("replay returned %d connections on failure; it must own its cleanup", len(conns))
	}

	// The reachable target may have been dialed -- that is how we learn it is
	// reachable -- but it must not have been fed.
	select {
	case c := <-accepted:
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		var buf [1]byte
		if n, _ := c.Read(buf[:]); n != 0 {
			t.Fatalf("reachable target received %d byte(s); a failed fan-out must feed nobody", n)
		}
	case <-time.After(time.Second):
		// Never dialed at all is also acceptable: nobody was fed.
	}
}

// deadAddr returns an address nothing is listening on, by binding a port and
// releasing it. Racy in principle, reliable in practice, and far better than
// hardcoding a port number that might be in use on a developer's machine.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func closeAll(conns []net.Conn) {
	for _, c := range conns {
		c.Close()
	}
}

// TestReplayFeedsEveryTargetTheWholeCaptureAndHoldsItOpen pins the two things
// the dev stack depends on: each collector gets the identical byte stream (so
// the same router appears on both), and the sessions stay open afterwards.
//
// Holding matters because of what the collector does when a BMP transport
// ends: Session.Close emits KIND_VIEW_LOST for every peer it could still see.
// A sender that exited after writing would leave every replayed peer recorded
// as lost the moment it finished, so the dashboards would show a router whose
// whole view had just collapsed. A real router holds its session for hours.
func TestReplayFeedsEveryTargetTheWholeCaptureAndHoldsItOpen(t *testing.T) {
	want := readFile(t, lsFixture)
	msgs, err := captureMessages(lsFixture)
	if err != nil {
		t.Fatal(err)
	}

	const targets = 2
	got := make(chan []byte, targets)
	stillOpen := make(chan bool, targets)
	var addrs []string
	for range targets {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		addrs = append(addrs, ln.Addr().String())
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, len(want))
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(c, buf); err != nil {
				got <- nil
				stillOpen <- false
				return
			}
			got <- buf
			// One more read: a sender that closed after writing gives EOF
			// here, one that is holding the session gives a timeout.
			c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			var b [1]byte
			_, err = c.Read(b[:])
			stillOpen <- errors.Is(err, os.ErrDeadlineExceeded)
		}()
	}

	conns, err := replay(addrs, msgs, 2*time.Second)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer closeAll(conns)
	if len(conns) != targets {
		t.Fatalf("got %d connections want %d", len(conns), targets)
	}

	for i := range targets {
		select {
		case b := <-got:
			if !bytes.Equal(b, want) {
				t.Fatalf("target %d received %d bytes, want the capture's %d", i, len(b), len(want))
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("target %d never received the capture", i)
		}
		if open := <-stillOpen; !open {
			t.Fatalf("target %d: session was closed after the write; it must be held open", i)
		}
	}
}

func TestParseTargets(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
		err  bool
	}{
		{"one", "collector:11019", []string{"collector:11019"}, false},
		{"two, spaced", "172.22.0.6:11019, 172.22.0.9:11119",
			[]string{"172.22.0.6:11019", "172.22.0.9:11119"}, false},
		{"empty", "", nil, true},
		// A trailing comma is the shape a hand-edited compose file grows, and
		// silently dropping the empty element would leave the operator one
		// collector short of the fan-out they wrote down.
		{"trailing comma", "a:1,", nil, true},
		{"no port", "collector", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTargets(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("parseTargets(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTargets(%q): %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseTargets(%q) = %v want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPrefixSysNameRenamesTheDeviceButNotTheVendor pins the distinction
// bmpgen learned the hard way and documents in cmd/vantage/bmpgen.go: a
// capture's sysDescr is a VENDOR identity, and rewriting it would invent a
// banner no router sends -- the exact mistake the profile mechanism exists to
// prevent. Its sysName is a DEVICE name, and replaying it verbatim files
// every replayed row under the real lab node the bytes were captured from.
// Observed here before this existed: the dashboards' own $router query
// returned two entries reading "nx-p3", one the original capture's router
// and one the replay.
func TestPrefixSysNameRenamesTheDeviceButNotTheVendor(t *testing.T) {
	msgs, err := captureMessages(lsFixture)
	if err != nil {
		t.Fatal(err)
	}
	before, err := bmp.ParseInit(msgs[0][bmp.HeaderLen:])
	if err != nil {
		t.Fatal(err)
	}
	if before.SysName == "" || before.SysDescr == "" {
		t.Fatalf("fixture carries no identity to rewrite: %+v", before)
	}

	got, err := prefixSysName(msgs, "replay-")
	if err != nil {
		t.Fatalf("prefixSysName: %v", err)
	}

	after, err := bmp.ParseInit(got[0][bmp.HeaderLen:])
	if err != nil {
		t.Fatalf("rewritten Initiation does not parse: %v", err)
	}
	if want := "replay-" + before.SysName; after.SysName != want {
		t.Errorf("sysName = %q want %q", after.SysName, want)
	}
	if after.SysDescr != before.SysDescr {
		t.Errorf("sysDescr was rewritten: %q -> %q; the vendor banner must survive verbatim",
			before.SysDescr, after.SysDescr)
	}
	// The rewritten Initiation must still be a well-formed BMP message whose
	// declared length matches what it now carries.
	if l := binary.BigEndian.Uint32(got[0][1:5]); int(l) != len(got[0]) {
		t.Errorf("Initiation declares %d bytes but is %d", l, len(got[0]))
	}
	if _, err := bmp.ReadMsg(bytes.NewReader(got[0])); err != nil {
		t.Errorf("rewritten Initiation does not read back: %v", err)
	}

	// Nothing else may be touched: this rewrites an identity, not a session.
	if len(got) != len(msgs) {
		t.Fatalf("got %d messages want %d", len(got), len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		if !bytes.Equal(got[i], msgs[i]) {
			t.Fatalf("message %d was modified; only the Initiation may be", i)
		}
	}
}

// TestPrefixSysNamePreservesUnknownTLVs guards the rebuild: the Initiation is
// re-serialized TLV by TLV, and a vendor TLV this project does not decode
// (RFC 7854 type 0, free-form) must come out the other side intact rather
// than being dropped by a rebuild that only knows the two it understands.
func TestPrefixSysNamePreservesUnknownTLVs(t *testing.T) {
	payload := bmp.AppendTLV(nil, 1, []byte("Cisco NX-OS"))
	payload = bmp.AppendTLV(payload, 2, []byte("nx-p3"))
	payload = bmp.AppendTLV(payload, 0, []byte("free-form string"))
	init := append([]byte{bmp.Version, 0, 0, 0, 0, bmp.TypeInitiation}, payload...)
	binary.BigEndian.PutUint32(init[1:5], uint32(len(init)))

	got, err := prefixSysName([][]byte{init}, "replay-")
	if err != nil {
		t.Fatalf("prefixSysName: %v", err)
	}
	tlvs, err := bmp.ParseTLVs(got[0][bmp.HeaderLen:])
	if err != nil {
		t.Fatalf("rewritten payload does not parse: %v", err)
	}
	var sawFreeForm bool
	for _, tl := range tlvs {
		if tl.Type == 0 && string(tl.Value) == "free-form string" {
			sawFreeForm = true
		}
	}
	if !sawFreeForm {
		t.Errorf("the undecoded TLV was dropped by the rebuild; got %d TLVs", len(tlvs))
	}
}

func TestParseTargetDelays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    map[string]time.Duration
		wantErr bool
	}{
		{name: "empty is no delays", in: "", want: map[string]time.Duration{}},
		{name: "one target", in: "127.0.0.1:11119=45s",
			want: map[string]time.Duration{"127.0.0.1:11119": 45 * time.Second}},
		{name: "several, spaces tolerated", in: "a:1=1s, b:2=2m",
			want: map[string]time.Duration{"a:1": time.Second, "b:2": 2 * time.Minute}},
		{name: "zero is allowed and means no delay", in: "a:1=0s",
			want: map[string]time.Duration{"a:1": 0}},
		{name: "a bare host is refused for the reason parseTargets refuses one", in: "collector=5s", wantErr: true},
		{name: "no equals", in: "a:1", wantErr: true},
		{name: "unparseable duration", in: "a:1=soon", wantErr: true},
		// A negative delay would silently mean "no delay" after the clamp
		// every sleep does, which is a typo returning success.
		{name: "negative is refused", in: "a:1=-5s", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTargetDelays(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTargetDelays(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTargetDelays(%q): %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s: got %s, want %s", k, got[k], v)
				}
			}
		})
	}
}

// TestReplayDelaysOneTargetWithoutDelayingTheOther is the whole reason the
// flag exists: reproducing a LAGGING COLLECTOR.
//
// evpn-churn inventing a flap out of a second collector's late copy of an
// advertisement was first proved live on 2026-09-20 by pausing a
// collector container while a real FRR speaker advertised and terminally
// withdrew a route. That proof is a thing done once by hand. This flag
// makes it reproducible from the capture committed that day, with no FRR
// and no docker pause.
//
// The assertion is ORDERING, not elapsed time: the undelayed target must
// receive the whole capture BEFORE the delayed one receives its first byte.
// A test that asserted "took at least 400ms" would be timing-flaky on a
// loaded machine and would still pass if both targets were delayed.
func TestReplayDelaysOneTargetWithoutDelayingTheOther(t *testing.T) {
	msgs, err := captureMessages(lsFixture)
	if err != nil {
		t.Fatal(err)
	}
	want := readFile(t, lsFixture)

	type arrival struct {
		idx  int
		when time.Time
	}
	first := make(chan arrival, 2)
	done := make(chan arrival, 2)
	var addrs []string
	for i := range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		addrs = append(addrs, ln.Addr().String())
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, len(want))
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			var n int
			for n < len(buf) {
				m, err := c.Read(buf[n:])
				if m > 0 && n == 0 {
					first <- arrival{i, time.Now()}
				}
				n += m
				if err != nil {
					return
				}
			}
			done <- arrival{i, time.Now()}
		}()
	}

	// Target 1 lags. 400ms is long enough to order reliably against a
	// loopback write and short enough not to pad the suite.
	const lag = 400 * time.Millisecond
	delays := map[string]time.Duration{addrs[1]: lag}
	conns, err := replayWithDelays(addrs, msgs, 2*time.Second, delays)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer closeAll(conns)

	var zeroDone, oneFirst time.Time
	for range 2 {
		select {
		case a := <-done:
			if a.idx == 0 {
				zeroDone = a.when
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a target never received the whole capture")
		}
		if !zeroDone.IsZero() {
			break
		}
	}
	for range 2 {
		select {
		case a := <-first:
			if a.idx == 1 {
				oneFirst = a.when
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the delayed target never received a byte")
		}
		if !oneFirst.IsZero() {
			break
		}
	}
	if zeroDone.IsZero() || oneFirst.IsZero() {
		t.Fatalf("missing arrivals: undelayed done=%v delayed first=%v", zeroDone, oneFirst)
	}
	if !oneFirst.After(zeroDone) {
		t.Errorf("the delayed target's first byte arrived at %s, not after the "+
			"undelayed target finished at %s -- with a %s delay on one target "+
			"the other must be fed first, which is what makes a lagging "+
			"collector reproducible", oneFirst, zeroDone, lag)
	}
}

// TestRunRefusesADelayForATargetItIsNotFeeding guards the typo case.
//
// A delay keyed on an address that is not in -targets applies nothing, and
// without this check the command would report success while the lagging
// collector it was asked to create was fed at the same instant as the
// other. That is the same failure mode parseTargets rejects a bare host
// for, one layer up: silence about a mistake whose whole symptom is the
// absence of an effect.
func TestRunRefusesADelayForATargetItIsNotFeeding(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	err = run(lsFixture, ln.Addr().String(), time.Second, "", "127.0.0.1:1=5s")
	if err == nil {
		t.Fatal("run accepted a -target-delay naming an address it is not feeding")
	}
	if !strings.Contains(err.Error(), "not in -targets") {
		t.Errorf("error was %q, want it to name the mismatch so the typo is "+
			"findable", err)
	}
}
