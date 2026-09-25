package natsutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/bgp"
	"github.com/jp2195/vantage/bmp"
	"github.com/jp2195/vantage/bmp/bmptest"
	"github.com/jp2195/vantage/collector"
	"github.com/jp2195/vantage/natsutil/natstest"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// testEvent builds a minimal publishable event on a real ROUTES subject,
// with msgID as its dedup key so callers can generate distinct events.
func testEvent(msgID string) collector.Event {
	return collector.Event{
		Subject: "vantage.v1.route.ipv4u.0a000001.0a000009",
		MsgID:   msgID,
		Env:     &vantagev1.Envelope{CollectorId: "c1", Seq: 1},
	}
}

func splitTokens(s string) []string      { return strings.Split(s, ".") }
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// testOpts clamps every replica count to 1: the embedded nats-server
// natstest.RunJS starts is a single, non-clustered node, and (as
// TestNonClusteredServerRejectsReplicasAboveOne demonstrates) such a node
// rejects any stream config with num_replicas > 1 outright. LS's production
// default of 3 replicas would make a literal StreamOpts{} fail against
// this test server, so it needs its own override here too.
// RoutesMaxBytes/RawMaxBytes are bounded because nats-server validates a
// stream's MaxBytes against the FREE SPACE of the JetStream store
// directory's filesystem, not against the account limit -- an unlimited
// account still refuses a stream larger than the disk can hold. The
// production defaults (8 GiB ROUTES, 2 GiB RAW) exceeded what the CI
// runner's temp filesystem had free, so every test that provisioned
// streams failed there with "insufficient storage resources available"
// while passing locally on a bigger disk. Tests publish kilobytes; 16 MiB
// is far above anything they write and fits anywhere.
var testOpts = StreamOpts{Replicas: 1, LSReplicas: 1,
	RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20}

// TestEnsureStreamsAndPartitionedPublish exercises EnsureStreams and
// partitioned publish together, using testOpts (above) instead of a
// zero-value StreamOpts{}, and router/peer tokens in
// subjects.EncodeIP's hex-of-address-bytes form ("0a000001" for 10.0.0.1,
// "0a000009" for 10.0.0.9), not dashed-decimal text.
func TestEnsureStreamsAndPartitionedPublish(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatalf("must be idempotent: %v", err)
	}

	// Publish a route event through the session layer and observe the
	// partition token inserted by the stream's subject transform.
	s := collector.NewSession(netip.MustParseAddr("10.0.0.1"), "c1", 1, time.Now, collector.Overrides{})
	ph := bmp.PeerHeader{Type: bmp.PeerTypeGlobal, Addr: netip.MustParseAddr("10.0.0.9"), AS: 65001, BGPID: "10.0.0.9", Timestamp: time.Now()}
	msg := bmptest.RouteMonitoring(ph, bgp.BuildUpdate{
		Announced: []bgp.Prefix{{Prefix: netip.MustParsePrefix("192.0.2.0/24")}},
		NextHop:   netip.MustParseAddr("10.0.0.9"),
	})
	m, err := bmp.ReadMsg(bytesReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	evs := s.Handle(m)
	if len(evs) != 1 {
		t.Fatalf("events=%d", len(evs))
	}

	p := NewPublisher(js)
	if err := p.Publish(evs[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	cons, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := cons.Next()
	if err != nil {
		t.Fatal(err)
	}
	// vantage.v1.route.{p}.{family}.{router}.{peer} = 7 tokens
	want := "vantage.v1.route."
	if len(got.Subject()) <= len(want) || got.Subject()[:len(want)] != want {
		t.Fatal(got.Subject())
	}
	toks := splitTokens(got.Subject())
	if len(toks) != 7 || toks[4] != "ipv4u" || toks[5] != "0a000001" || toks[6] != "0a000009" {
		t.Fatalf("subject=%q", got.Subject())
	}

	// duplicate suppression: same MsgID published twice -> still 1 message
	if err := p.Publish(evs[0]); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	info, err := js.Stream(ctx, "ROUTES")
	if err != nil {
		t.Fatal(err)
	}
	si, err := info.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if si.State.Msgs != 1 {
		t.Fatalf("msgs=%d want 1 (msg-id dedup)", si.State.Msgs)
	}
}

// TestNonClusteredServerRejectsReplicasAboveOne pins down *why* tests must
// clamp Replicas/LSReplicas to 1 (see testOpts above) as a fact about the
// real server, not an assumption: a standalone (non-clustered) nats-server
// rejects any stream config asking for more than one replica.
func TestNonClusteredServerRejectsReplicasAboveOne(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: "R3PROBE", Subjects: []string{"r3probe.>"}, Replicas: 3,
	})
	if err == nil {
		t.Fatal("expected the embedded single-node server to reject Replicas: 3, got nil error")
	}
	t.Logf("server correctly rejected Replicas=3 on a non-clustered node: %v", err)
}

// TestEnsureStreamsServerVerifiedConfig asserts every field this package
// specifies, reading each value back from
// StreamInfo (what the server actually stored), not from the StreamConfig
// this package sent -- a config the server silently coerced or ignored a
// field of would still show up here as a mismatch.
func TestEnsureStreamsServerVerifiedConfig(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Distinguishable per-stream Replicas: ROUTES/PEER/STATS take Replicas
	// (clamped to 1, see testOpts doc comment), LS takes its own LSReplicas
	// field, RAW must come back as 1 regardless of either.
	opts := StreamOpts{Partitions: 16, Replicas: 1, LSReplicas: 1, RoutesMaxBytes: 123 << 20, RawMaxBytes: 45 << 20}
	if err := EnsureStreams(ctx, js, opts); err != nil {
		t.Fatal(err)
	}

	type want struct {
		maxAge       time.Duration
		maxBytes     int64 // -1 means "don't check"
		replicas     int
		hasTransform bool
		tfSource     string
		tfDest       string
	}
	cases := map[string]want{
		"ROUTES": {maxAge: 48 * time.Hour, maxBytes: 123 << 20, replicas: 1, hasTransform: true,
			tfSource: "vantage.v1.route.*.*.*",
			tfDest:   "vantage.v1.route.{{partition(16,2,3)}}.{{wildcard(1)}}.{{wildcard(2)}}.{{wildcard(3)}}"},
		"LS": {maxAge: 14 * 24 * time.Hour, maxBytes: -1, replicas: 1, hasTransform: true,
			tfSource: "vantage.v1.ls.*.*",
			tfDest:   "vantage.v1.ls.{{partition(16,1,2)}}.{{wildcard(1)}}.{{wildcard(2)}}"},
		"PEER":  {maxAge: 90 * 24 * time.Hour, maxBytes: -1, replicas: 1},
		"STATS": {maxAge: 7 * 24 * time.Hour, maxBytes: -1, replicas: 1},
		"RAW":   {maxAge: 24 * time.Hour, maxBytes: 45 << 20, replicas: 1},
	}

	for name, w := range cases {
		t.Run(name, func(t *testing.T) {
			str, err := js.Stream(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			si, err := str.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cfg := si.Config
			if cfg.Retention != jetstream.LimitsPolicy {
				t.Errorf("Retention=%v want LimitsPolicy", cfg.Retention)
			}
			if cfg.Storage != jetstream.FileStorage {
				t.Errorf("Storage=%v want FileStorage", cfg.Storage)
			}
			if cfg.Discard != jetstream.DiscardOld {
				t.Errorf("Discard=%v want DiscardOld", cfg.Discard)
			}
			if cfg.Compression != jetstream.S2Compression {
				t.Errorf("Compression=%v want S2Compression", cfg.Compression)
			}
			if cfg.Duplicates != 2*time.Minute {
				t.Errorf("Duplicates=%v want 2m", cfg.Duplicates)
			}
			if cfg.MaxAge != w.maxAge {
				t.Errorf("MaxAge=%v want %v", cfg.MaxAge, w.maxAge)
			}
			if w.maxBytes >= 0 && cfg.MaxBytes != w.maxBytes {
				t.Errorf("MaxBytes=%d want %d", cfg.MaxBytes, w.maxBytes)
			}
			if cfg.Replicas != w.replicas {
				t.Errorf("Replicas=%d want %d", cfg.Replicas, w.replicas)
			}
			if w.hasTransform {
				if cfg.SubjectTransform == nil {
					t.Fatal("SubjectTransform is nil, want set")
				}
				if cfg.SubjectTransform.Source != w.tfSource {
					t.Errorf("SubjectTransform.Source=%q want %q", cfg.SubjectTransform.Source, w.tfSource)
				}
				if cfg.SubjectTransform.Destination != w.tfDest {
					t.Errorf("SubjectTransform.Destination=%q want %q", cfg.SubjectTransform.Destination, w.tfDest)
				}
			} else if cfg.SubjectTransform != nil {
				t.Errorf("SubjectTransform=%+v want nil", cfg.SubjectTransform)
			}
		})
	}
}

// TestPerStreamReplicas pins the per-stream replica mapping
// against buildStreamConfigs rather than against a live server. A stream with
// Replicas > 1 cannot be created on the single-node embedded server at all
// (it returns err_code=10074, "replicas > 1 not supported in non-clustered
// mode" -- see TestNonClusteredServerRejectsReplicasAboveOne), so a
// round-trip through EnsureStreams can only ever exercise the R1 case and
// could never distinguish "RAW is forced to 1" from "every stream is 1".
// buildStreamConfigs exists precisely so this logic is checkable without a
// multi-node cluster.
func TestPerStreamReplicas(t *testing.T) {
	byName := func(cfgs []jetstream.StreamConfig) map[string]int {
		m := map[string]int{}
		for _, c := range cfgs {
			m[c.Name] = c.Replicas
		}
		return m
	}

	// Caller asks for 3, and separately for a non-default LS value, so
	// "LS follows LSReplicas" is distinguishable from "LS follows Replicas".
	got := byName(buildStreamConfigs(StreamOpts{Replicas: 3, LSReplicas: 5}.withDefaults()))
	want := map[string]int{"ROUTES": 3, "PEER": 3, "STATS": 3, "LS": 5, "RAW": 1}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("Replicas=3, LSReplicas=5: %s Replicas=%d, want %d", name, got[name], w)
		}
	}

	// Defaults: LS is 3 even when the caller never mentions it; RAW is 1 even
	// though the general default is 1 -- so this case alone cannot prove the
	// forcing, which is why the case above uses Replicas=3.
	def := byName(buildStreamConfigs(StreamOpts{}.withDefaults()))
	wantDef := map[string]int{"ROUTES": 1, "PEER": 1, "STATS": 1, "LS": 3, "RAW": 1}
	for name, w := range wantDef {
		if def[name] != w {
			t.Errorf("defaults: %s Replicas=%d, want %d", name, def[name], w)
		}
	}
}

// TestEnsureStreamsReplicasAsStored is the server-backed half: with every
// stream at R1 (all the embedded single-node server accepts), the replica
// count the server actually stored must be 1 -- asserted from StreamInfo,
// not from the config we sent.
func TestEnsureStreamsReplicasAsStored(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	for _, name := range []string{"ROUTES", "LS", "PEER", "STATS", "RAW"} {
		st, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		si, err := st.Info(ctx)
		if err != nil {
			t.Fatalf("%s info: %v", name, err)
		}
		if si.Config.Replicas != 1 {
			t.Errorf("%s Replicas=%d as stored, want 1", name, si.Config.Replicas)
		}
	}
}

// TestEnsureStreamsDefaults confirms StreamOpts{}'s documented zero-value
// defaults (P=32, Replicas=1, LSReplicas=3, RoutesMaxBytes=8GiB,
// RawMaxBytes=2GiB) directly against withDefaults. It does not exercise
// EnsureStreams against a real server: LS's production default of 3
// replicas cannot be created on the single-node embedded test server (see
// TestNonClusteredServerRejectsReplicasAboveOne), so that half of the
// defaults is instead confirmed indirectly by TestPerStreamReplicas and
// TestEnsureStreamsServerVerifiedConfig, both of which pass LSReplicas
// explicitly.
func TestEnsureStreamsDefaults(t *testing.T) {
	o := StreamOpts{}.withDefaults()
	if o.Partitions != 32 {
		t.Errorf("default Partitions=%d want 32", o.Partitions)
	}
	if o.Replicas != 1 {
		t.Errorf("default Replicas=%d want 1", o.Replicas)
	}
	if o.LSReplicas != 3 {
		t.Errorf("default LSReplicas=%d want 3", o.LSReplicas)
	}
	if o.RoutesMaxBytes != 8<<30 {
		t.Errorf("default RoutesMaxBytes=%d want %d", o.RoutesMaxBytes, int64(8<<30))
	}
	if o.RawMaxBytes != 2<<30 {
		t.Errorf("default RawMaxBytes=%d want %d", o.RawMaxBytes, int64(2<<30))
	}
}

// TestIdempotentEnsureStreams runs EnsureStreams twice and asserts every
// stream's StreamInfo.Config is byte-for-byte identical afterwards -- not
// just that the second call returned nil, but that it left the
// server-recorded config completely unchanged (CreateOrUpdateStream
// semantics: safe to run repeatedly against an existing deployment).
func TestIdempotentEnsureStreams(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}
	before := map[string]jetstream.StreamConfig{}
	for _, name := range []string{"ROUTES", "LS", "PEER", "STATS", "RAW"} {
		str, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		si, err := str.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = si.Config
	}

	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatalf("second EnsureStreams: %v", err)
	}
	for _, name := range []string{"ROUTES", "LS", "PEER", "STATS", "RAW"} {
		str, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		si, err := str.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, a := before[name], si.Config
		if b.Retention != a.Retention || b.Storage != a.Storage || b.Discard != a.Discard ||
			b.Compression != a.Compression || b.Duplicates != a.Duplicates || b.MaxAge != a.MaxAge ||
			b.MaxBytes != a.MaxBytes || b.Replicas != a.Replicas ||
			(b.SubjectTransform == nil) != (a.SubjectTransform == nil) {
			t.Fatalf("%s config drifted across a repeated EnsureStreams: before=%+v after=%+v", name, b, a)
		}
		if b.SubjectTransform != nil && (b.SubjectTransform.Source != a.SubjectTransform.Source ||
			b.SubjectTransform.Destination != a.SubjectTransform.Destination) {
			t.Fatalf("%s SubjectTransform drifted: before=%+v after=%+v", name, b.SubjectTransform, a.SubjectTransform)
		}
	}
}

// hexIPv4 renders a as the 8-hex-digit token subjects.EncodeIP would (a
// self-contained copy so this test doesn't need to reach into
// subjects just to build fixture subjects for router/peer pairs
// this test invents itself, not ones a real Event produced).
func hexIPv4(s string) string {
	a := netip.MustParseAddr(s)
	b := a.As4()
	return fmt.Sprintf("%02x%02x%02x%02x", b[0], b[1], b[2], b[3])
}

// TestPartitionVariesByRouterPeer proves not just that a single published
// message lands on *a* partition-transformed subject, but that the partition
// token the server computes actually varies across distinct router/peer
// inputs (and is stable for a repeated one), which is the entire point of
// the transform -- a partition scheme that always emitted the same bucket
// would make EnsureStreams's SubjectTransform pure dead weight. Publishes
// directly (bypassing the session layer, since only the resulting subjects
// matter here) to a range of distinct router/peer address pairs and reads
// every resulting subject back from the real embedded server.
func TestPartitionVariesByRouterPeer(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := EnsureStreams(ctx, js, StreamOpts{Replicas: 1, LSReplicas: 1, Partitions: 32,
		RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20}); err != nil {
		t.Fatal(err)
	}

	routers := []string{"10.0.0.1", "192.168.1.1", "203.0.113.5", "198.51.100.7", "172.16.5.9", "10.99.1.2"}
	peers := []string{"10.0.0.9", "203.0.113.200", "8.8.8.8", "1.1.1.1"}
	for _, r := range routers {
		for _, p := range peers {
			subj := fmt.Sprintf("vantage.v1.route.ipv4u.%s.%s", hexIPv4(r), hexIPv4(p))
			if _, err := js.Publish(ctx, subj, []byte("x"), jetstream.WithMsgID(subj)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Republish one pair a second time (different MsgID, so it is not
	// deduplicated) to confirm the partition assignment for one router/peer
	// pair is stable, not merely varying randomly across calls.
	repeatSubj := fmt.Sprintf("vantage.v1.route.ipv4u.%s.%s", hexIPv4(routers[0]), hexIPv4(peers[0]))
	if _, err := js.Publish(ctx, repeatSubj, []byte("y"), jetstream.WithMsgID(repeatSubj+"-2")); err != nil {
		t.Fatal(err)
	}

	cons, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	total := len(routers)*len(peers) + 1
	partitionOf := map[string]string{} // "router/peer" -> observed partition token
	distinct := map[string]bool{}
	for range total {
		m, err := cons.Next()
		if err != nil {
			t.Fatal(err)
		}
		toks := splitTokens(m.Subject())
		if len(toks) != 7 {
			t.Fatalf("subject=%q: want 7 tokens", m.Subject())
		}
		part, router, peer := toks[3], toks[5], toks[6]
		distinct[part] = true
		key := router + "/" + peer
		if prev, ok := partitionOf[key]; ok && prev != part {
			t.Fatalf("partition for %s changed from %s to %s: transform is not deterministic", key, prev, part)
		}
		partitionOf[key] = part
	}
	if len(distinct) < 2 {
		t.Fatalf("expected the partition token to vary across %d distinct router/peer pairs, saw only %v", len(routers)*len(peers), distinct)
	}
	t.Logf("observed %d distinct partitions across %d router/peer pairs: %v", len(distinct), len(routers)*len(peers), distinct)
}

// TestPublishFailureSurfaces confirms the fix described in publisher.go's
// doc comment: publishing to a subject with no bound stream is rejected by
// the server (no responders once retries are exhausted), and that rejection
// must reach the caller through Drain -- not vanish the way it would
// under a Publish/Drain implementation that discarded the PubAckFuture
// PublishAsync returns and so had no way to learn PublishAsync
// returning nil does not mean the server accepted the message.
func TestPublishFailureSurfaces(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}

	p := NewPublisher(js)
	// A subject no stream captures is retried for backoffWindow before it is
	// reported (see retryClass); shrunk here so the report lands well inside
	// the Drain below. Left at 20s, Drain would time out instead, and a bare
	// err != nil check would pass on the timeout without the rejection ever
	// surfacing -- hence errors.Is.
	p.retry.backoffWindow = 200 * time.Millisecond
	ev := collector.Event{Subject: "vantage.v1.nostream.nobody", MsgID: "no-such-stream/1"}
	// ev.Env is nil, which proto.Marshal accepts as an empty message -- fine,
	// this test only cares about the subject having no bound stream.
	if err := p.Publish(ev); err != nil {
		t.Fatalf("Publish (local/synchronous) unexpectedly failed: %v", err)
	}
	if err := p.Drain(5 * time.Second); !errors.Is(err, jetstream.ErrNoStreamResponse) {
		t.Fatalf("Drain = %v for a publish to a subject with no bound stream; want jetstream.ErrNoStreamResponse", err)
	} else {
		t.Logf("Drain correctly surfaced the server-side rejection: %v", err)
	}
}

// TestPublisherConcurrentPublishAndDrain pins the concurrency contract the
// Publisher's doc comment claims. The original implementation tracked
// outstanding publishes with a sync.WaitGroup, which forbids Add running
// concurrently with Wait; the collector -- publishing from per-session
// goroutines while a flush or shutdown path drains -- would have hit it
// immediately.
//
// This test only discriminates under -race, which is how CI runs it. Without
// the detector the WaitGroup version passes here: the panic path ("WaitGroup
// is reused before previous Wait has returned") needs Add to land in a narrow
// window after the counter reaches zero and before Wait returns, which this
// workload hits only sometimes. Under -race the same version reports a DATA
// RACE on every run, because the race detector flags the concurrent
// Add/Wait itself rather than waiting for the timing to line up.
//
// Each round guarantees the overlap rather than hoping for it: the writers
// keep publishing until a Drain is seen waiting and then publish more, so
// Publish runs while Drain waits in every round. Only then do they stop.
// Drain waits for the publisher to go idle, which it cannot do while
// writers publish without pause (see Drain), so every round's Drain must be
// able to finish once they stop. An earlier version drained 50 times while
// the writers never stopped, and passed only where the server acked faster
// than eight writers could publish. On a slower machine the async window
// stayed full, acks flowing at thousands a second, and a Drain never saw
// the publisher idle.
func TestPublisherConcurrentPublishAndDrain(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := EnsureStreams(ctx, js, testOpts); err != nil {
		t.Fatal(err)
	}
	p := NewPublisher(js)
	draining := func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.idle != nil
	}

	const (
		writers = 8
		rounds  = 20
		// afterDrain is how many publishes each round makes, across all
		// writers, after a Drain was seen waiting.
		afterDrain = 200
	)
	var published atomic.Int64
	for round := range rounds {
		var wg sync.WaitGroup // test-side only; not the Publisher's accounting
		stop := make(chan struct{})
		errCh := make(chan error, writers)
		for g := range writers {
			wg.Go(func() {
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					err := p.Publish(testEvent(fmt.Sprintf("r%d-g%d-%d", round, g, i)))
					if err == nil {
						published.Add(1)
						continue
					}
					// The client stalls once too many publishes are in
					// flight. That is backpressure, not a correctness
					// failure, so back off and keep going rather than
					// failing the test.
					if strings.Contains(err.Error(), "stalled") {
						time.Sleep(time.Millisecond)
						continue
					}
					errCh <- err
					return
				}
			})
		}

		// Start Drains until one is seen waiting. A Drain that finds the
		// publisher idle returns at once, so it may take a few.
		drained := make(chan error, 1)
		for waiting := false; !waiting; {
			go func() { drained <- p.Drain(20 * time.Second) }()
			for !waiting {
				if waiting = draining(); waiting {
					break
				}
				select {
				case err := <-drained:
					if err != nil {
						t.Fatalf("round %d: drain: %v", round, err)
					}
				case <-time.After(time.Millisecond):
					continue
				}
				break
			}
		}
		for start := published.Load(); published.Load() < start+afterDrain; {
			if ctx.Err() != nil {
				t.Fatalf("round %d: the writers stopped publishing", round)
			}
			time.Sleep(time.Millisecond)
		}
		close(stop)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Errorf("round %d: publish: %v", round, err)
		}
		if err := <-drained; err != nil {
			t.Fatalf("round %d: drain: %v", round, err)
		}
	}

	if err := p.Drain(20 * time.Second); err != nil {
		t.Fatalf("final drain: %v", err)
	}
}

// TestEnsureStreamsProvisionsAllStreamsDespiteOneFailure pins that a stream
// failing does not abandon the ones after it. LS is second in the slice and
// its default of 3 replicas is rejected outright by a non-clustered
// server, so returning on first error left a single-node install with only
// ROUTES -- PEER, STATS and RAW silently never created, on every retry.
func TestEnsureStreamsProvisionsAllStreamsDespiteOneFailure(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The shipped zero-value default: LSReplicas becomes 3, which this
	// single-node server cannot satisfy.
	err := EnsureStreams(ctx, js, StreamOpts{RoutesMaxBytes: 16 << 20, RawMaxBytes: 16 << 20})
	if err == nil {
		t.Fatal("want an error for LS on a non-clustered server")
	}
	if !strings.Contains(err.Error(), "LS") {
		t.Fatalf("error should name the failing stream: %v", err)
	}
	for _, name := range []string{"ROUTES", "PEER", "STATS", "RAW"} {
		if _, serr := js.Stream(ctx, name); serr != nil {
			t.Errorf("%s must still be provisioned despite LS failing: %v", name, serr)
		}
	}
}

// TestEnsureStreamsRejectsInvalidOpts pins validation of values that would
// otherwise produce a silently broken deployment rather than an error. A
// negative Partitions is interpolated into the subject transform, accepted
// by the server, and then hashed as "% uint32(-1)", so messages land on a
// partition token far outside 0..P-1 and every partitioned consumer matches
// nothing. The CLI exposes --partitions as a flag.
func TestEnsureStreamsRejectsInvalidOpts(t *testing.T) {
	js := natstest.RunJS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, c := range []struct {
		name string
		opts StreamOpts
	}{
		{"negative partitions", StreamOpts{Partitions: -1, Replicas: 1, LSReplicas: 1}},
		{"negative replicas", StreamOpts{Replicas: -1, LSReplicas: 1}},
		{"negative ls replicas", StreamOpts{Replicas: 1, LSReplicas: -1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := EnsureStreams(ctx, js, c.opts); err == nil {
				t.Fatal("want a validation error before any server call")
			}
		})
	}
}
