// Command bmp-replay is a BMP router made of a committed capture: it dials one
// or more collectors from a single source address, writes a `vantage capture`
// file to each, and holds the sessions open.
//
// It exists because the dev stack has no source of BGP-LS. bmpgen synthesizes
// BMP, but its families stop at lu4, and the private lab that produced the
// link-state corpus is not something `docker compose up` can bring with it. So
// the link-state half of the archive could only ever be populated by one
// collector, and every dashboard that resolves a node or link per collector
// was unverifiable outside fixtures.
//
// Deliberately not a `vantage` subcommand. vantage is the operator CLI and
// ships in bin/; this is dev-stack scaffolding that only ever points at a
// collector you control. It is still built and vetted by `go build ./...`, so
// CI covers it -- it simply is not handed to operators.
//
// The identity the collector records is the TCP source address, not anything
// in the capture (collector.Server.routerIP), so all targets are fed from this
// one process: that is what makes the replayed router genuinely dual-homed
// rather than two routers that happen to carry the same routes.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jp2195/vantage/buildinfo"
)

// parseTargets splits the -targets flag into collector addresses.
//
// Every element must carry a port. A bare host would be dialed and fail with
// "missing port in address" only after the other targets were already open,
// which is a confusing way to learn about a typo; rejecting it here keeps the
// all-or-nothing contract in replay honest about what it is enforcing.
func parseTargets(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("no targets: pass -targets host:port[,host:port...]")
	}
	var out []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			return nil, fmt.Errorf("empty target in %q", s)
		}
		if _, _, err := net.SplitHostPort(t); err != nil {
			return nil, fmt.Errorf("target %q: %w", t, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func main() {
	capture := flag.String("capture", "", "path to a .bmpcap file to replay")
	targets := flag.String("targets", "", "comma-separated collector addresses (host:port)")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "per-target dial timeout")
	// Defaults to renaming, because the unsafe option is the one that looks
	// like it is working: a verbatim sysName files every replayed row under
	// the real device the capture was taken from, indistinguishable from
	// that device's own rows. Pass -sysname-prefix "" to send the
	// capture's name unchanged.
	// See parseTargetDelays for what a delay models and why it is honest.
	targetDelay := flag.String("target-delay", "",
		"per-target wait before that target's first byte, host:port=duration[,...] -- reproduces a LAGGING collector")
	sysNamePrefix := flag.String("sysname-prefix", "replay-",
		`prepended to the capture's sysName so replayed rows are distinguishable from the device they were captured from ("" sends it unchanged)`)
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Line("bmp-replay"))
		return
	}

	if err := run(*capture, *targets, *dialTimeout, *sysNamePrefix, *targetDelay); err != nil {
		fmt.Fprintf(os.Stderr, "bmp-replay: %v\n", err)
		os.Exit(1)
	}
}

func run(capture, targets string, dialTimeout time.Duration, sysNamePrefix, targetDelay string) error {
	if capture == "" {
		return fmt.Errorf("no capture: pass -capture path/to/file.bmpcap")
	}
	addrs, err := parseTargets(targets)
	if err != nil {
		return err
	}
	delays, err := parseTargetDelays(targetDelay)
	if err != nil {
		return err
	}
	// A delay naming a target that is not being fed is a typo that would
	// otherwise apply nothing and report success -- the same reasoning that
	// makes parseTargets reject a bare host.
	for t := range delays {
		if !slices.Contains(addrs, t) {
			return fmt.Errorf("-target-delay names %q, which is not in -targets (%s)",
				t, strings.Join(addrs, ", "))
		}
	}
	msgs, err := captureMessages(capture)
	if err != nil {
		return err
	}
	msgs, err = prefixSysName(msgs, sysNamePrefix)
	if err != nil {
		return err
	}
	conns, err := replayWithDelays(addrs, msgs, dialTimeout, delays)
	if err != nil {
		return err
	}
	defer closeAllConns(conns)

	lag := ""
	if len(delays) > 0 {
		var parts []string
		for _, t := range addrs {
			if d := delays[t]; d > 0 {
				parts = append(parts, fmt.Sprintf("%s lagged %s", t, d))
			}
		}
		if len(parts) > 0 {
			lag = " (" + strings.Join(parts, ", ") + ")"
		}
	}
	fmt.Fprintf(os.Stderr, "bmp-replay: sent %d messages from %s to %s%s; holding sessions open\n",
		len(msgs), capture, strings.Join(addrs, ", "), lag)

	// SIGTERM as well as SIGINT: this runs as a compose service, and `docker
	// compose down` sends TERM. Without it the container is killed after the
	// stop timeout, which works but makes every shutdown take ten seconds.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}
