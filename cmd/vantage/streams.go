// streams.go implements `vantage streams init`: idempotent provisioning of
// the five vantage JetStream streams (natsutil.EnsureStreams) against a
// running NATS server. Safe to re-run against an already-provisioned
// deployment (EnsureStreams uses CreateOrUpdateStream throughout): an
// existing stream keeps its replica count unless -replicas or -ls-replicas
// asks for another, and a lower one is refused without
// -allow-lower-replicas (see planReplicas).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/jp2195/vantage/natsutil"
)

// usageStreams is returned on any argument error and is the exact
// invocation this command's own help text documents: the "init" verb
// first, flags after it.
const usageStreams = "usage: vantage streams init [-nats URL] [-nats-ca FILE] [-nats-cert FILE] [-nats-key FILE] " +
	"[-partitions N] [-replicas N] [-ls-replicas N] [-allow-lower-replicas] [-routes-max-bytes N] [-raw-max-bytes N]"

func cmdStreams(args []string) error {
	// "init" must be checked and consumed *before* the FlagSet parses
	// anything: flag.FlagSet.Parse stops parsing at the first non-flag
	// argument, so a naive fs.Parse(args) followed by checking
	// fs.Arg(0) == "init" would only ever work if every flag were written
	// *before* "init" -- the opposite of the order this command's own usage
	// text (above) documents ("vantage streams init [-nats URL] ..."). An
	// operator typing the command exactly as documented would have every
	// flag after "init" silently swept into fs.Args() as extra positional
	// arguments instead of being parsed, and get a bare usage error instead
	// of ever reaching NATS.
	if len(args) < 1 || args[0] != "init" {
		return fmt.Errorf(usageStreams)
	}
	fs := flag.NewFlagSet("streams init", flag.ExitOnError)
	natsURL := natsURLFlag(fs)
	natsTLS := natsTLSFlags(fs)
	partitions := fs.Int("partitions", 32, "partition count P")
	// Exposed because nats-server validates a stream's MaxBytes against the
	// free space of the JetStream store directory's filesystem, not against
	// the account limit -- an unlimited account still refuses a stream bigger
	// than the disk can hold. With these fixed at the production defaults,
	// `streams init` simply failed on any host with less than ~10 GiB free
	// and the operator had no way to ask for less. Defaults match
	// natsutil.StreamOpts' own, so an unset flag provisions exactly what it
	// always did.
	routesMaxBytes := fs.Int64("routes-max-bytes", 8<<30, "ROUTES stream byte limit")
	rawMaxBytes := fs.Int64("raw-max-bytes", 2<<30, "RAW stream byte limit")
	replicas := fs.Int("replicas", 1, "stream replicas (ROUTES/PEER/STATS; RAW is always 1 regardless). "+
		"Applies to new streams; an existing stream keeps its count unless this is set")
	lsReplicas := fs.Int("ls-replicas", 0,
		"LS stream replicas (0 = follow -replicas; natsutil's own zero-value default of 3 "+
			"is rejected outright by a non-clustered NATS server)")
	allowLower := fs.Bool("allow-lower-replicas", false,
		"apply a -replicas or -ls-replicas lower than an existing stream's count (refused otherwise)")
	fs.Parse(args[1:])
	if fs.NArg() != 0 {
		return fmt.Errorf(usageStreams)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// The flag is a second ingress for a credential, alongside the config
	// file -- see natsFlag.
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// natsutil.StreamOpts's own zero-value default for LSReplicas is 3,
	// which a single, non-clustered NATS server -- the common target for
	// this command against a laptop/dev deployment -- rejects outright, so
	// LS would never provision and every peer/route/stats publish
	// downstream would look healthy while LS silently stayed missing. The
	// collector's own config loading (LoadConfig) solves the identical
	// problem by defaulting an unset ls_replicas from replicas;
	// -ls-replicas defaults to 0 (meaning "unset") here for the same
	// reason, so a dev install (replicas=1, the flag default) provisions
	// LS at 1 and a cluster started with -replicas 3 gets LS at 3 without
	// the operator having to say so twice.
	ls := *lsReplicas
	if ls == 0 {
		ls = *replicas
		if ls <= 0 {
			ls = 1
		}
	}

	perStream, err := planReplicas(ctx, js, []replicaAsk{
		{"ROUTES", *replicas, set["replicas"]},
		{"LS", ls, set["replicas"] || set["ls-replicas"]},
		{"PEER", *replicas, set["replicas"]},
		{"STATS", *replicas, set["replicas"]},
	}, *allowLower)
	if err != nil {
		return err
	}

	if err := natsutil.EnsureStreams(ctx, js, natsutil.StreamOpts{
		Partitions:     *partitions,
		Replicas:       *replicas,
		LSReplicas:     ls,
		StreamReplicas: perStream,
		RoutesMaxBytes: *routesMaxBytes,
		RawMaxBytes:    *rawMaxBytes,
	}); err != nil {
		return fmt.Errorf("ensure streams: %w", err)
	}
	fmt.Println("streams ready: ROUTES LS PEER STATS RAW")
	return nil
}

// replicaAsk is one replicated stream's requested copy count, and whether
// the operator set it on the command line (explicit) or it is only the
// flag default.
type replicaAsk struct {
	stream   string
	want     int
	explicit bool
}

// planReplicas decides each existing stream's replica count before anything
// is changed. CreateOrUpdateStream applies whatever count it is given, and
// -replicas defaults to 1, so without this a plain `vantage streams init`
// against a three-copy deployment took every stream down to one copy.
//
// A stream that does not exist yet gets its asked-for count. An existing
// stream keeps its current count when the count was not set explicitly. An
// explicit count lower than the current one is refused, for every stream
// at once and before any stream is touched, unless allowLower is set.
func planReplicas(ctx context.Context, js jetstream.JetStream, asks []replicaAsk, allowLower bool) (map[string]int, error) {
	out := map[string]int{}
	var lower []string
	for _, a := range asks {
		s, err := js.Stream(ctx, a.stream)
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			out[a.stream] = a.want
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stream %s: %w", a.stream, err)
		}
		cur := s.CachedInfo().Config.Replicas
		switch {
		case !a.explicit:
			out[a.stream] = cur
		case a.want < cur && !allowLower:
			lower = append(lower, fmt.Sprintf("%s %d -> %d", a.stream, cur, a.want))
		default:
			out[a.stream] = a.want
		}
	}
	if len(lower) > 0 {
		return nil, fmt.Errorf("refusing to lower stream replicas (%s): this removes copies of data "+
			"the deployment relies on; pass -allow-lower-replicas to apply it", strings.Join(lower, ", "))
	}
	return out, nil
}
