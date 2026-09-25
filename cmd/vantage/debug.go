// debug.go implements `vantage debug`: a NATS subscriber that unmarshals
// every vantage.v1 Envelope it sees and pretty-prints it, decoding the
// subject's hex address tokens back to readable addresses
// (subjects.DecodeIP) rather than dumping raw hex at the operator. With
// -from-start it first replays the ROUTES stream's full history via an
// ordered consumer, then switches to a live core-NATS subscription.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
	"github.com/jp2195/vantage/subjects"
)

// defaultFilter is the subject filter `vantage debug` subscribes on by
// default: every envelope this system's collectors publish. It is matched
// against core-NATS delivery (nc.SubscribeSync below), which sees each
// message on the subject the collector actually published it to --
// partition-free, per subjects' package doc -- not the
// partition-inserted form the ROUTES/LS streams store it under. That is
// what lets a single, unadjusted filter work unmodified for the live path;
// see routesStreamFilter for the one path (-from-start's stream replay)
// where the distinction matters.
var defaultFilter = subjects.Prefix + ".>"

// humanizeSubject renders subject the way an operator can actually read it:
// every hex address token subjects.EncodeIP ever produces (8 hex digits for
// an IPv4 address, 32 for IPv6), wherever it occurs -- the last two tokens
// as the collector published it, or shifted right by one on ROUTES/LS's
// stored, partition-inserted form -- is decoded back to a real address via
// subjects.DecodeIP and substituted in place; a "-r"/"-z"/"-d" suffix (peer
// distinguisher, zone, or RIB direction -- see subjects.PeerToken) is
// preserved verbatim after it, since humanizeSubject splits at the first
// '-' and decodes only what precedes it. Every other token
// -- "vantage", "v1", "route"/"ls"/"peer"/"stats"/"raw", a family name, a
// partition number, or "locrib" -- is not 8 or 32 hex digits (see
// DecodeIP's doc comment) and passes through unchanged.
//
// Hex tokens were chosen over an escaped-text encoding because it
// round-trips. Printing "0a000009" at an operator when
// "10.0.0.9" is one function call away would defeat the purpose of a debug
// tool.
//
// Assumes no non-address token this system's subjects ever produce is
// exactly 8 or 32 bytes of only [0-9a-f] -- true for every literal and
// family name today, and for a partition token unless deployed with an
// implausible partition count (>10,000,000).
func humanizeSubject(subject string) string {
	toks := strings.Split(subject, ".")
	for i, tok := range toks {
		a, err := subjects.DecodeIP(tok)
		if err != nil {
			continue
		}
		base := tok
		if before, _, ok := strings.Cut(tok, "-"); ok {
			base = before
		}
		toks[i] = a.String() + tok[len(base):]
	}
	return strings.Join(toks, ".")
}

// routesStreamFilter adapts a subject filter written in the form the
// collector publishes (partition-free) into the FilterSubjects value an
// ordered consumer reading ROUTES' own stored subjects needs.
// natsutil.EnsureStreams' SubjectTransform inserts a server-computed
// partition token immediately after the literal "route" token, so ROUTES
// stores 7 tokens where the collector published 6
// ("vantage.v1.route.{partition}.{family}.{router}.{peer}" versus
// "vantage.v1.route.{family}.{router}.{peer}"). A filter that names "route"
// as a literal third token -- e.g. "vantage.v1.route.ipv4u.>", a completely
// reasonable thing for an operator to ask for -- would otherwise match
// nothing at all against what the stream actually stores: not an error,
// just silence indistinguishable from "no traffic", which is exactly the
// failure this function exists to prevent. The default filter
// ("vantage.v1.>") and anything else that reaches "route" only via a
// wildcard need no adjustment, since ">"/"*" at that position already spans
// the partition token along with whatever the caller wrote after it.
func routesStreamFilter(filter string) string {
	toks := strings.Split(filter, ".")
	if len(toks) >= 3 && toks[2] == "route" {
		out := make([]string, 0, len(toks)+1)
		out = append(out, toks[:3]...)
		out = append(out, "*")
		out = append(out, toks[3:]...)
		return strings.Join(out, ".")
	}
	return filter
}

// header renders the line printed above each envelope. It shows the
// humanized subject first, then the real one in brackets whenever they
// differ.
//
// Showing only the humanized form was actively misleading: it looks exactly
// like a subject, the flag is literally -filter SUBJ, and pasting it back
// produces a filter that subscribes successfully and then matches nothing
// forever -- silence indistinguishable from "no traffic", the same failure
// routesStreamFilter exists to prevent, reintroduced on the display side.
// The bracketed form is the one to paste.
func header(subject string) string {
	h := humanizeSubject(subject)
	if h == subject {
		return h
	}
	return h + "  [" + subject + "]"
}

func printEnv(subject string, data []byte) {
	env := &vantagev1.Envelope{}
	if err := proto.Unmarshal(data, env); err != nil {
		// Diagnostics go to stderr so a script consuming stdout gets only
		// envelope data.
		fmt.Fprintf(os.Stderr, "── %s (unmarshal error: %v)\n", header(subject), err)
		return
	}
	if len(data) == 0 {
		fmt.Printf("── %s\n(empty payload)\n", header(subject))
		return
	}
	out, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(env)
	fmt.Printf("── %s\n%s\n", header(subject), out)
}

func cmdDebug(args []string) error {
	fs := flag.NewFlagSet("debug", flag.ExitOnError)
	natsURL := natsURLFlag(fs)
	natsTLS := natsTLSFlags(fs)
	filter := fs.String("filter", defaultFilter, "subject filter")
	fromStart := fs.Bool("from-start", false, "replay ROUTES stream history first")
	fs.Parse(args)

	nurl := natsFlag(natsURL)
	nc, err := natsDial(nurl, *natsTLS)
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", nurl, err)
	}
	defer nc.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *fromStart {
		js, err := jetstream.New(nc)
		if err != nil {
			return fmt.Errorf("jetstream: %w", err)
		}
		cons, err := js.OrderedConsumer(ctx, "ROUTES", jetstream.OrderedConsumerConfig{
			DeliverPolicy:  jetstream.DeliverAllPolicy,
			FilterSubjects: []string{routesStreamFilter(*filter)},
		})
		if err != nil {
			return fmt.Errorf("replay ROUTES: %w", err)
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return fmt.Errorf("replay ROUTES: %w", err)
		}
		for n := info.NumPending; n > 0; n-- {
			m, err := cons.Next()
			if err != nil {
				fmt.Fprintf(os.Stderr, "replay ROUTES: stopped early: %v\n", err)
				break
			}
			printEnv(m.Subject(), m.Data())
		}
	}

	sub, err := nc.SubscribeSync(*filter)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", *filter, err)
	}
	fmt.Fprintf(os.Stderr, "subscribed to %s (ctrl-c to exit)\n", *filter)
	for {
		m, err := sub.NextMsgWithContext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // ctrl-c: expected shutdown
			}
			return fmt.Errorf("subscribe %s: %w", *filter, err)
		}
		printEnv(m.Subject, m.Data)
	}
}
