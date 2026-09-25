package main

import (
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/natsutil/natstest"
)

// TestCmdStreamsInitFlagsAfterSubcommand drives cmdStreams with the exact
// argument order this command's own usage text documents: "init" first,
// every flag after it. Go's flag.FlagSet.Parse stops parsing at the first
// non-flag argument, so a naive implementation that ran a single FlagSet
// over the whole argument list and only afterward checked for "init" as a
// positional argument would silently fail this exact, documented
// invocation -- every flag written after "init" lands in fs.Args() instead
// of being parsed, and the command returns a bare usage error without ever
// reaching NATS. This test would fail if that regressed.
func TestCmdStreamsInitFlagsAfterSubcommand(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	url := nc.ConnectedUrl()
	if url == "" {
		t.Fatal("embedded test server: ConnectedUrl() is empty")
	}

	if err := cmdStreams([]string{
		"init", "-nats", url, "-partitions", "4", "-replicas", "1", "-ls-replicas", "1",
		// Small on purpose: the production defaults exceed what a modest
		// filesystem can back, and nats-server checks MaxBytes against free
		// disk. See TestCmdStreamsInitHonorsMaxBytesFlags.
		"-routes-max-bytes", "16777216", "-raw-max-bytes", "16777216",
	}); err != nil {
		t.Fatalf("cmdStreams with flags after \"init\" (the documented order): %v", err)
	}

	ctx := t.Context()
	for _, name := range []string{"ROUTES", "LS", "PEER", "STATS", "RAW"} {
		if _, err := js.Stream(ctx, name); err != nil {
			t.Errorf("stream %s not provisioned: %v", name, err)
		}
	}
	si, err := mustStreamInfo(t, js, "LS")
	if err != nil {
		t.Fatal(err)
	}
	if si.Config.Replicas != 1 {
		t.Errorf("LS replicas = %d, want 1 (from -ls-replicas)", si.Config.Replicas)
	}
}

// TestCmdStreamsInitRejectsBadArgs pins the argument-error paths: no verb,
// a wrong verb, and a stray positional argument after the flags all return
// the usage error rather than attempting to connect to NATS.
func TestCmdStreamsInitRejectsBadArgs(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"init", "extra-positional-arg"},
	} {
		if err := cmdStreams(args); err == nil {
			t.Errorf("cmdStreams(%v): want usage error, got nil", args)
		}
	}
}

func mustStreamInfo(t *testing.T, js jetstream.JetStream, name string) (*jetstream.StreamInfo, error) {
	t.Helper()
	s, err := js.Stream(t.Context(), name)
	if err != nil {
		return nil, err
	}
	return s.Info(t.Context())
}

// TestCmdStreamsInitHonorsMaxBytesFlags pins that the stream size limits are
// reachable from the command line.
//
// Without them `vantage streams init` can only ever ask for the production
// defaults -- 8 GiB for ROUTES, 2 GiB for RAW -- and nats-server validates a
// stream's MaxBytes against the free space of the JetStream store directory's
// filesystem, refusing anything larger than the disk can hold even when the
// account limit is unlimited. So on a small disk the command failed outright
// with "insufficient storage resources available" and the operator had no way
// to ask for less. CI hit exactly this.
func TestCmdStreamsInitHonorsMaxBytesFlags(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := cmdStreams([]string{
		"init", "-nats", nc.ConnectedUrl(), "-partitions", "4", "-replicas", "1", "-ls-replicas", "1",
		"-routes-max-bytes", "12582912", // 12 MiB
		"-raw-max-bytes", "6291456", // 6 MiB
	}); err != nil {
		t.Fatalf("cmdStreams with size flags: %v", err)
	}
	for _, tc := range []struct {
		stream string
		want   int64
	}{{"ROUTES", 12 << 20}, {"RAW", 6 << 20}} {
		si, err := mustStreamInfo(t, js, tc.stream)
		if err != nil {
			t.Fatalf("%s: %v", tc.stream, err)
		}
		if si.Config.MaxBytes != tc.want {
			t.Errorf("%s MaxBytes = %d, want %d: the flag was parsed and ignored",
				tc.stream, si.Config.MaxBytes, tc.want)
		}
	}
}

// TestCmdStreamsInitNeverLowersReplicas pins that `vantage streams init`
// cannot quietly take copies away from streams that already exist. The
// command provisions with CreateOrUpdateStream, and -replicas defaults to 1,
// so without this guard a plain `vantage streams init` against the Helm
// default (three servers, three-copy streams) updates every stream down to
// one copy.
//
// Every step runs against the same three-server cluster, in order: new
// streams get the count asked for; existing R3 streams stay R3 when no
// count is given; an explicit lower count is refused and changes nothing;
// the same count with -allow-lower-replicas is applied.
func TestCmdStreamsInitNeverLowersReplicas(t *testing.T) {
	c := natstest.RunCluster(t)
	url := c.Servers[0].ClientURL()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// Small on purpose; see TestCmdStreamsInitHonorsMaxBytesFlags.
	base := []string{"init", "-nats", url, "-partitions", "4",
		"-routes-max-bytes", "16777216", "-raw-max-bytes", "16777216"}
	run := func(extra ...string) error {
		return cmdStreams(append(append([]string{}, base...), extra...))
	}
	want := func(step string, n int) {
		t.Helper()
		for _, name := range []string{"ROUTES", "LS", "PEER", "STATS"} {
			si, err := mustStreamInfo(t, js, name)
			if err != nil {
				t.Fatalf("%s: %s: %v", step, name, err)
			}
			if si.Config.Replicas != n {
				t.Errorf("%s: %s replicas = %d, want %d", step, name, si.Config.Replicas, n)
			}
		}
		si, err := mustStreamInfo(t, js, "RAW")
		if err != nil {
			t.Fatalf("%s: RAW: %v", step, err)
		}
		if si.Config.Replicas != 1 {
			t.Errorf("%s: RAW replicas = %d, want 1 (always)", step, si.Config.Replicas)
		}
	}

	if err := run("-replicas", "3"); err != nil {
		t.Fatalf("new streams with -replicas 3: %v", err)
	}
	want("new streams, -replicas 3", 3)

	if err := run(); err != nil {
		t.Fatalf("re-run with no -replicas: %v", err)
	}
	want("existing R3, no -replicas", 3)

	err = run("-replicas", "1")
	if err == nil {
		t.Fatal("explicit -replicas 1 against R3 streams: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "-allow-lower-replicas") {
		t.Errorf("refusal does not name the override flag: %v", err)
	}
	want("refused -replicas 1", 3)

	// -ls-replicas alone, so the refusal cannot come from -replicas.
	if err := run("-ls-replicas", "1"); err == nil {
		t.Fatal("explicit -ls-replicas 1 against an R3 LS: want a refusal, got nil")
	}
	want("refused -ls-replicas 1", 3)

	// Asking for the count a stream already has is not lowering it.
	if err := run("-replicas", "3"); err != nil {
		t.Fatalf("explicit -replicas 3 against R3 streams: %v", err)
	}
	want("explicit -replicas 3 against R3", 3)

	if err := run("-replicas", "1", "-allow-lower-replicas"); err != nil {
		t.Fatalf("-replicas 1 -allow-lower-replicas: %v", err)
	}
	want("-replicas 1 with the override", 1)
}

// TestCmdStreamsInitNewStreamsDefaultToOne pins the other side of the
// guard: with nothing provisioned, no -replicas still means one copy, which
// is what a single-server NATS accepts.
func TestCmdStreamsInitNewStreamsDefaultToOne(t *testing.T) {
	nc, js := natstest.RunJSConn(t)
	if err := cmdStreams([]string{"init", "-nats", nc.ConnectedUrl(), "-partitions", "4",
		"-routes-max-bytes", "16777216", "-raw-max-bytes", "16777216"}); err != nil {
		t.Fatalf("cmdStreams with no -replicas on an empty server: %v", err)
	}
	for _, name := range []string{"ROUTES", "LS", "PEER", "STATS", "RAW"} {
		si, err := mustStreamInfo(t, js, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if si.Config.Replicas != 1 {
			t.Errorf("%s replicas = %d, want 1", name, si.Config.Replicas)
		}
	}
}
