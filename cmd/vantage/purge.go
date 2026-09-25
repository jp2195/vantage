package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"time"

	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
	"github.com/jp2195/vantage/sink"
)

// cmdPurge is `vantage purge`: it removes a retired router's or collector's
// current state. See runPurge.
func cmdPurge(args []string) error { return runPurge(context.Background(), args, os.Stdout) }

// runPurge removes one collector's view of one router, or everything one
// collector holds, from the current tables, and the collector's heartbeats
// with it when the whole collector goes.
//
// It talks to ClickHouse directly and only directly. The API is GET-only by
// design: a purge endpoint would give the API's ClickHouse user ALTER DELETE,
// which its read-only threat model rules out. So -dsn is required, and it
// needs a user with ALTER DELETE on the current tables -- the same privilege
// the writer's cleanup uses.
//
// It refuses while the target is live, meaning its collector was heard from
// inside the stale threshold AND the target has a current session that
// collector's running process opened and that still has a peer up. A router
// disconnected from a running collector is not live: its session closed,
// and its peers read view_lost. Deleting that would delete state a
// running collector is still maintaining, and the next rows it wrote would
// start a current view missing everything before them. -force purges anyway
// and says why it was refused. -dry-run prints the per-table counts and the
// liveness verdict and deletes nothing.
//
// It also refuses, without -force, a target whose collector is past the
// stale threshold but was last heard less than purgeQuietFor ago. A NATS or
// writer stall longer than the stale threshold makes a running collector
// read exactly like a dead one, and once the backlog drains, that
// collector's queued rows would be written after the purge and start a
// current view missing everything before them.
//
// History is not touched: the router's sessions, events and churn stay in the
// history tables until retention.days expires them.
func runPurge(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "ClickHouse DSN (required; the purge deletes, and only the direct path can)")
	collectorID := fs.String("collector", "", "collector_id to purge (required)")
	router := fs.String("router", "", "purge only this router's rows under -collector")
	dryRun := fs.Bool("dry-run", false, "print what would be deleted and delete nothing")
	force := fs.Bool("force", false, "purge even while the target is live, or while its collector is past "+
		"-stale-after but was heard from less than 15 minutes ago")
	staleAfter := fs.Duration("stale-after", query.DefaultStaleAfter,
		"how long a collector may go unheard before it no longer counts as live; match the API's stale_after")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return errors.New("purge: -dsn is required: the API is read-only, so a purge talks " +
			"to ClickHouse directly, as a user with ALTER DELETE on the current tables")
	}
	target := sink.PurgeTarget{Collector: *collectorID}
	if *router != "" {
		a, err := netip.ParseAddr(*router)
		if err != nil {
			return fmt.Errorf("purge: -router %q is not an IP address", *router)
		}
		target.Router = a.Unmap()
	}
	if err := target.Validate(); err != nil {
		return err
	}

	d := secret.NewClickHouseDSN(*dsn)
	ch, err := sink.NewClickHouse(ctx, d)
	if err != nil {
		return fmt.Errorf("connect clickhouse %s: %w", d, err)
	}
	defer ch.Close()
	q, err := query.New(ch.Conn(), ch.DB())
	if err != nil {
		return err
	}
	if q, err = q.WithStaleAfter(*staleAfter); err != nil {
		return fmt.Errorf("-stale-after: %w", err)
	}

	name := "collector " + target.Collector
	if target.Router.IsValid() {
		name += ", router " + target.Router.String()
	}
	live, err := q.TargetLiveness(ctx, target.Collector, target.Router)
	if err != nil {
		return err
	}
	plan, err := ch.PlanPurge(ctx, target)
	if err != nil {
		return err
	}
	rows := plan.Rows()
	if err := tableTo(out, func(w io.Writer) {
		fmt.Fprintln(w, "TABLE\tROWS")
		for _, c := range plan.Counts {
			rowf(w, "%s\t%d\n", c.Table, c.Rows)
		}
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: %s\n", name, describeLiveness(live, *staleAfter))

	// recent is a collector past the stale threshold but heard from too
	// lately to be sure it is gone. A collector inside the threshold is
	// being heard, so its pipeline is not stalled, and Live alone judges
	// the target: a router whose session with it closed purges without
	// -force.
	recent := live.HasBeat && !live.BeatFresh && time.Since(live.LastBeat) < purgeQuietFor
	switch {
	case *dryRun && live.Live() && rows > 0 && !*force:
		fmt.Fprintln(out, "dry run: nothing deleted; a real run would refuse, because the target is live (use -force)")
		return nil
	case *dryRun && recent && rows > 0 && !*force:
		fmt.Fprintf(out, "dry run: nothing deleted; a real run would refuse, because the collector "+
			"was heard from less than %s ago (use -force)\n", purgeQuietFor)
		return nil
	case *dryRun:
		fmt.Fprintln(out, "dry run: nothing deleted")
		return nil
	case rows == 0:
		fmt.Fprintln(out, "nothing to purge")
		return nil
	case live.Live() && !*force:
		return fmt.Errorf("purge: refusing, %s is live: %s. Stop the collector, or "+
			"disconnect the router from it so its session closes, first -- or pass -force",
			name, describeLiveness(live, *staleAfter))
	case live.Live():
		fmt.Fprintf(out, "-force: purging a live target\n")
	case recent && !*force:
		return fmt.Errorf("purge: refusing, %s was heard from less than %s ago: %s. A stalled "+
			"NATS or writer makes a running collector look silent, and its queued rows would "+
			"be written after the purge. Wait until it has been silent for %s, or pass -force",
			name, purgeQuietFor, describeLiveness(live, *staleAfter), purgeQuietFor)
	case recent:
		fmt.Fprintf(out, "-force: purging a target whose collector was heard from less than %s ago\n", purgeQuietFor)
	}
	if err := ch.Purge(ctx, plan); err != nil {
		return fmt.Errorf("%w\nA failed DELETE can stay queued and be retried by ClickHouse: "+
			"look in system.mutations for database %s, is_done = 0, and a command naming "+
			"collector_id %q, and KILL MUTATION any you find. Running the purge again finishes it",
			err, ch.DB(), target.Collector)
	}
	fmt.Fprintf(out, "purged %d rows\n", rows)
	return nil
}

// purgeQuietFor is how long a collector must have been silent before a purge
// of it needs no -force. It is far past any stale threshold, and long enough
// for an operator to have noticed a stalled pipeline.
const purgeQuietFor = 15 * time.Minute

// describeLiveness says in one line why a target is or is not live.
func describeLiveness(l query.TargetLiveness, after time.Duration) string {
	if !l.HasBeat {
		return "its collector has never been heard from"
	}
	heard := time.Since(l.LastBeat).Round(time.Second)
	return fmt.Sprintf("its collector was last heard %s ago (threshold %s), its process started "+
		"%s, and %d current session(s) with a peer up belong to that process",
		heard, after, l.Started.UTC().Format(time.RFC3339), l.LiveSessions)
}
