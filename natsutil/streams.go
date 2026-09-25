// Package natsutil provisions the five vantage JetStream streams and
// publishes collector.Events onto them.
//
// Collectors publish partition-free (see subjects' package doc):
// every Event's Subject is exactly the router/peer/family-token form built by
// that package, with no partition token anywhere in it. The ROUTES and LS
// streams insert that token themselves, server-side, via a StreamConfig
// SubjectTransform -- a NATS subject-mapping function evaluated once per
// message as it's ingested, not something this package or the collector ever
// computes. That keeps the partition count (P) in exactly one place (the
// stream config passed to EnsureStreams) rather than duplicated between
// publisher and consumer-side assumptions.
//
// The transform indexes wildcard *occurrences* in its Source pattern by
// position (1st '*', 2nd '*', ...), not by the literal text that ends up
// there, so it is unaffected by what those tokens actually look like --
// verified here against the real nats-server rather than assumed (see
// EnsureStreams' doc comment for the exact semantics confirmed against
// server/subject_transform.go).
package natsutil

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// StreamOpts configures EnsureStreams. Zero values take the defaults in
// withDefaults: 32 partitions, replicas 1 (ROUTES/PEER/STATS) or 3 (LS; RAW
// is always 1 regardless of any field here -- see withDefaults and
// EnsureStreams), 8GiB of ROUTES storage, 2GiB of RAW storage.
//
// Replicas is deliberately not a single knob for every stream: ROUTES,
// PEER, and STATS take the caller's Replicas, but LS -- lower volume,
// higher operational value (topology data) -- defaults to 3 whether
// or not the caller sets Replicas, via its own LSReplicas field, and RAW --
// a lossless safety net whose whole point is to be cheap -- is always forced
// to 1 no matter what either field says. A single Replicas field applied
// uniformly to all five streams would silently under-replicate LS or
// over-replicate RAW; the collector's config and the CLI both need to be
// able to express the LS value independently of the general one, which is
// why it is its own field rather than, say, a special sentinel value of
// Replicas.
//
// StreamReplicas, when it names a stream, sets that stream's count in place
// of Replicas or LSReplicas. `vantage streams init` uses it to keep an
// existing stream's count when the operator did not ask for one. RAW stays
// at 1 even if StreamReplicas names it.
type StreamOpts struct {
	Partitions     int
	Replicas       int
	LSReplicas     int
	StreamReplicas map[string]int
	RoutesMaxBytes int64
	RawMaxBytes    int64
}

func (o StreamOpts) withDefaults() StreamOpts {
	if o.Partitions == 0 {
		o.Partitions = 32
	}
	if o.Replicas == 0 {
		o.Replicas = 1
	}
	if o.LSReplicas == 0 {
		o.LSReplicas = 3
	}
	if o.RoutesMaxBytes == 0 {
		o.RoutesMaxBytes = 8 << 30
	}
	if o.RawMaxBytes == 0 {
		o.RawMaxBytes = 2 << 30
	}
	return o
}

// EnsureStreams creates or updates the five vantage streams
// (CreateOrUpdateStream, so safe to call repeatedly against an already
// -provisioned deployment). LimitsPolicy, file storage, S2 compression,
// DiscardOld, and a 2-minute duplicate window apply to every stream;
// only retention age, byte caps, and replica counts vary per stream.
//
// ROUTES and LS carry a SubjectTransform that inserts the server-computed
// partition token immediately after the stream's own subject-type token
// ("route"/"ls"), ahead of the family/router/peer tokens the collector
// published without it:
//
//	ROUTES source "vantage.v1.route.*.*.*" (wildcards 1,2,3 = family,
//	router, peer) -> dest "vantage.v1.route.{{partition(P,2,3)}}.
//	{{wildcard(1)}}.{{wildcard(2)}}.{{wildcard(3)}}", partitioning on
//	router+peer (wildcards 2 and 3) so one router/peer's events always land
//	in the same partition and so preserve per-peer ordering.
//	LS source "vantage.v1.ls.*.*" (wildcards 1,2 = router, peer) -> dest
//	"vantage.v1.ls.{{partition(P,1,2)}}.{{wildcard(1)}}.{{wildcard(2)}}",
//	partitioning on the same two tokens.
//
// This is confirmed against nats-server's actual subject-transform
// implementation (server/subject_transform.go in nats-io/nats-server/v2),
// not merely against what the config accepts:
//   - a {{wildcard(n)}}/{{partition(N,n,...)}} argument n is the 1-based
//     index of the n-th '*' *occurrence* in Source, not an absolute token
//     position in the full subject and not a literal string match -- so the
//     hex-vs-text encoding of the router/peer tokens (subjects)
//     cannot affect which tokens get hashed or substituted, only the bytes
//     that are fed into the partition hash and copied into the wildcard
//     slots.
//   - partition() hashes (FNV-1a, mod P) the concatenation of the raw bytes
//     of every token index it's given, and emits the bare decimal result
//     (0..P-1) -- no "p" prefix, matching this package's contract.
//   - the transform is applied once, at ingestion, to the subject the
//     message is actually stored and later delivered under (mset.itr.Match
//     in the server's message-processing path); a message whose subject
//     doesn't match Source is stored under its original subject unchanged
//     rather than erroring, which cannot happen here because every subject
//     Route/Ls ever build matches these Source patterns exactly (each
//     router/peer/family token is single, dot-free, by subjects'
//     construction).
//
// See streams_test.go's TestEnsureStreamsAndPartitionedPublish, which
// publishes through the real collector/subjects/bmp stack and asserts the
// partition-transformed subject a consumer actually receives, and
// TestPartitionVariesByRouterPeer, which asserts the partition token differs
// across distinct router/peer inputs -- both against StreamInfo/consumed
// messages from a real embedded nats-server, not just that EnsureStreams
// returned nil.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, o StreamOpts) error {
	if err := o.validate(); err != nil {
		return err
	}
	// Every stream is attempted even after one fails, and the failures are
	// joined. Returning on the first error left the deployment partially
	// provisioned in a way that is both silent and order-dependent: LS is
	// second in the slice, and its default of 3 replicas is
	// rejected outright by a non-clustered server, so a single-node install
	// running with defaults created ROUTES and then abandoned PEER, STATS
	// and RAW on every attempt -- and every peer/stats/raw publish failed
	// thereafter with nothing pointing at the cause.
	var errs []error
	for _, c := range buildStreamConfigs(o) {
		if _, err := js.CreateOrUpdateStream(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("natsutil: stream %s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// validate rejects option values that would otherwise produce a silently
// broken deployment rather than an error. A non-positive Partitions is the
// dangerous one: it is interpolated straight into the subject transform,
// the server accepts it, and getHashPartition then computes
// "hash % uint32(numBuckets)" -- for -1 that is "% 4294967295", so messages
// are stored with a partition token far outside 0..P-1 and every
// partition-filtered consumer silently matches nothing. The CLI exposes
// --partitions as a flag, so this is reachable from user input.
func (o StreamOpts) validate() error {
	var errs []error
	if o.Partitions < 0 {
		errs = append(errs, fmt.Errorf("natsutil: Partitions must be positive, got %d", o.Partitions))
	}
	if o.Replicas < 0 {
		errs = append(errs, fmt.Errorf("natsutil: Replicas must be positive, got %d", o.Replicas))
	}
	if o.LSReplicas < 0 {
		errs = append(errs, fmt.Errorf("natsutil: LSReplicas must be positive, got %d", o.LSReplicas))
	}
	return errors.Join(errs...)
}

// streamDuplicateWindow is the Duplicates window set on every stream: how
// long JetStream remembers a Nats-Msg-Id and answers a re-publish of it as a
// duplicate instead of storing it again. Publisher's in-flight retry depends
// on it (see publishRetryDeadline), so shortening it is a change to the
// publish path too.
const streamDuplicateWindow = 2 * time.Minute

// buildStreamConfigs computes the five stream configs EnsureStreams sends,
// with no side effects -- split out from EnsureStreams so the per-stream
// replica logic (in particular, that RAW's Replicas is always 1
// no matter what o.Replicas is) can be checked directly, without needing a
// multi-node cluster to create a stream with Replicas > 1 on. See
// streams_test.go's TestPerStreamReplicas.
func buildStreamConfigs(o StreamOpts) []jetstream.StreamConfig {
	o = o.withDefaults()
	part := func(src, dst string) *jetstream.SubjectTransformConfig {
		return &jetstream.SubjectTransformConfig{Source: src, Destination: dst}
	}
	streams := []jetstream.StreamConfig{
		{
			Name: "ROUTES", Subjects: []string{"vantage.v1.route.>"},
			SubjectTransform: part("vantage.v1.route.*.*.*",
				fmt.Sprintf("vantage.v1.route.{{partition(%d,2,3)}}.{{wildcard(1)}}.{{wildcard(2)}}.{{wildcard(3)}}", o.Partitions)),
			MaxAge: 48 * time.Hour, MaxBytes: o.RoutesMaxBytes,
			Replicas: o.Replicas,
		},
		{
			Name: "LS", Subjects: []string{"vantage.v1.ls.>"},
			SubjectTransform: part("vantage.v1.ls.*.*",
				fmt.Sprintf("vantage.v1.ls.{{partition(%d,1,2)}}.{{wildcard(1)}}.{{wildcard(2)}}", o.Partitions)),
			MaxAge:   14 * 24 * time.Hour,
			Replicas: o.LSReplicas,
		},
		{Name: "PEER", Subjects: []string{"vantage.v1.peer.>"}, MaxAge: 90 * 24 * time.Hour, Replicas: o.Replicas},
		{Name: "STATS", Subjects: []string{"vantage.v1.stats.>"}, MaxAge: 7 * 24 * time.Hour, Replicas: o.Replicas},
		// RAW's replica count is not o.Replicas: it is forced to 1
		// unconditionally below, regardless of what the caller passed, so
		// setting it here is only a documentation of intent -- the loop's
		// override is what actually takes effect. See StreamOpts' doc
		// comment for why.
		{Name: "RAW", Subjects: []string{"vantage.v1.raw.>"}, MaxAge: 24 * time.Hour, MaxBytes: o.RawMaxBytes, Replicas: o.Replicas},
	}
	for i := range streams {
		c := &streams[i]
		c.Retention = jetstream.LimitsPolicy
		c.Storage = jetstream.FileStorage
		c.Discard = jetstream.DiscardOld
		c.Compression = jetstream.S2Compression
		c.Duplicates = streamDuplicateWindow
		if n, ok := o.StreamReplicas[c.Name]; ok && n > 0 {
			c.Replicas = n
		}
		if c.Name == "RAW" {
			// RAW is always R1, never the caller's Replicas --
			// it is a lossless safety net, not a stream anything downstream
			// depends on surviving a node loss.
			c.Replicas = 1
		}
	}
	return streams
}
