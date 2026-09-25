package sink

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// peerStatusLivenessCase is one router of insertPeerStatusLivenessFixture:
// one peer, watched by one collector, and the state the panel must count it
// under.
type peerStatusLivenessCase struct {
	router    string
	stored    vantagev1.PeerEvent_Kind
	restarted bool          // the collector's process started after the session
	beatAge   time.Duration // 0: never heard from
	want      string
}

var peerStatusLiveness = []peerStatusLivenessCase{
	{"10.0.230.1", vantagev1.PeerEvent_KIND_UP, false, time.Second, "up"},
	{"10.0.230.2", vantagev1.PeerEvent_KIND_UP, false, 2 * time.Hour, "stale"},
	{"10.0.230.3", vantagev1.PeerEvent_KIND_UP, false, 0, "stale"},
	{"10.0.230.4", vantagev1.PeerEvent_KIND_UP, true, time.Second, "view_lost"},
	{"10.0.230.5", vantagev1.PeerEvent_KIND_DOWN, true, time.Second, "down"},
}

// psView is one collector's view of the one peer of a peerStatusMergeCase.
type psView struct {
	stored    vantagev1.PeerEvent_Kind
	restarted bool
	beatAge   time.Duration
	tsRouter  time.Duration // after the fixture's base instant
}

// peerStatusMergeCase is one router two collectors watch, one peer, and the
// state the cross-collector merge must count it under.
type peerStatusMergeCase struct {
	router string
	views  []psView
	want   string
	why    string
}

const (
	// psDualRouter: a live collector says up, a dead one's up reads stale.
	psDualRouter = "10.0.230.6"
	// psLostVsStaleRouter: a restarted collector's old session reads
	// view_lost, and the only other collector watching went quiet.
	psLostVsStaleRouter = "10.0.230.7"
	// psDownVsStaleRouter: a live collector heard the router say down after
	// the other collector went quiet on an up.
	psDownVsStaleRouter = "10.0.230.8"
	// psSupersededRouter: one collector, two sessions; the newer one no
	// longer carries one of the older one's peers.
	psSupersededRouter = "10.0.230.9"
	// psLostVsUnspecifiedRouter: a restarted collector's view_lost, and a
	// live collector's unspecified.
	psLostVsUnspecifiedRouter = "10.0.230.10"
	// psClockStepRouter: one collector whose clock stepped back between a
	// peer's up and its down, so ts_collector orders the two the wrong way
	// round and seq, the collector's own order within the session, does not.
	psClockStepRouter = "10.0.230.11"
	// psUpVsDownRouter: two live collectors disagree, and the down is the
	// newer router statement.
	psUpVsDownRouter = "10.0.230.12"
	// psDeadDownVsStaleRouter: two dead collectors, one holding a down and
	// the other a newer up that reads stale.
	psDeadDownVsStaleRouter = "10.0.230.13"

	psPeer     = "10.255.230.1"
	psPeerGone = "10.255.230.2"
)

var peerStatusMerge = []peerStatusMergeCase{
	{psDualRouter, []psView{
		{vantagev1.PeerEvent_KIND_UP, false, time.Second, 0},
		{vantagev1.PeerEvent_KIND_UP, false, 2 * time.Hour, time.Minute},
	}, "up", "the dead collector's view carries the newer ts_router, so a merge on " +
		"ts_router picks stale; a live collector's up outranks a stale one"},
	{psLostVsStaleRouter, []psView{
		{vantagev1.PeerEvent_KIND_UP, true, time.Second, 2 * time.Minute},
		{vantagev1.PeerEvent_KIND_UP, false, 2 * time.Hour, 0},
	}, "stale", "the restarted collector's view_lost carries the newer ts_router, so a " +
		"merge on ts_router picks view_lost and hides the one collector that was " +
		"still watching the peer when it went quiet"},
	{psDownVsStaleRouter, []psView{
		{vantagev1.PeerEvent_KIND_DOWN, false, time.Second, 2 * time.Minute},
		{vantagev1.PeerEvent_KIND_UP, false, 2 * time.Hour, 0},
	}, "down", "a live collector heard the router say down after the other collector " +
		"went quiet; the quiet collector's up is older news"},
	{psLostVsUnspecifiedRouter, []psView{
		{vantagev1.PeerEvent_KIND_UNSPECIFIED, false, time.Second, 2 * time.Minute},
		{vantagev1.PeerEvent_KIND_UP, true, time.Second, 0},
	}, "view_lost", "the unspecified view carries the newer ts_router; view_lost at least " +
		"says the peer was up when its collector restarted, unspecified says nothing"},
	{psUpVsDownRouter, []psView{
		{vantagev1.PeerEvent_KIND_DOWN, false, time.Second, 2 * time.Minute},
		{vantagev1.PeerEvent_KIND_UP, false, time.Second, 0},
	}, "down", "both collectors are live and the down is the newer router statement; " +
		"the other collector missed it, and \"any up wins\" reports a down peer as up"},
	{psDeadDownVsStaleRouter, []psView{
		{vantagev1.PeerEvent_KIND_DOWN, false, 2 * time.Hour, 0},
		{vantagev1.PeerEvent_KIND_UP, false, 2 * time.Hour, 2 * time.Minute},
	}, "down", "a resolved down is a router statement whatever its collector's liveness, " +
		"and outranks another collector's stale even when the stale up carries the newer ts_router"},
}

// psViewKey names one collector's view of one peer.
type psViewKey struct{ collector, router, peer string }

// psFixture is what insertPeerStatusLivenessFixture wrote: its run id, and
// the ts_router of each view's current session, for the one merge rule that
// reads it.
type psFixture struct {
	run      int64
	tsRouter map[psViewKey]time.Time
}

func psLivenessCollector(run int64, i int) string {
	return fmt.Sprintf("%sps-%d-%d", chtest.LivenessPrefix, run, i)
}

// psAllRouters is every router the fixture writes.
func psAllRouters() []string {
	var out []string
	for _, c := range peerStatusLiveness {
		out = append(out, c.router)
	}
	for _, c := range peerStatusMerge {
		out = append(out, c.router)
	}
	return append(out, psSupersededRouter, psClockStepRouter)
}

// insertPeerStatusLivenessFixture writes the tables above, and the
// superseded-session router, through the real insert path, with per-run
// collector ids so a second run in one binary starts clean.
func insertPeerStatusLivenessFixture(t *testing.T, ctx context.Context, ch *ClickHouse) psFixture {
	t.Helper()
	f := psFixture{run: time.Now().UnixNano(), tsRouter: map[psViewKey]time.Time{}}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	beat := func(collector string, sid uint64, restarted bool, age time.Duration) {
		t.Helper()
		if age == 0 {
			return
		}
		started := int64(sid) - int64(time.Second)
		if restarted {
			started = int64(sid) + int64(time.Second)
		}
		if err := ch.conn.Exec(ctx, qualify(ch, "INSERT INTO vantage.collector_beats (collector_id, started_at, beat_at, inserted_at) "+
			"SELECT ?, fromUnixTimestamp64Nano(toInt64(?), 'UTC'), now64(3), now64(3) - toIntervalMillisecond(?)"),
			collector, started, age.Milliseconds()); err != nil {
			t.Fatalf("beat for %s: %v", collector, err)
		}
	}
	streamSeq := uint64(8000)
	eventAt := func(collector, router, peer string, sid uint64, kind vantagev1.PeerEvent_Kind,
		tsRouter time.Time, seq uint64, tsCollector time.Time) {
		t.Helper()
		env := &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: router, SysName: "dash-ps-live"},
			Peer:        &vantagev1.PeerId{Ip: peer, Asn: 65230},
			SessionId:   sid, Seq: seq,
			TsRouter:    timestamppb.New(tsRouter),
			TsCollector: timestamppb.New(tsCollector),
			Payload:     &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{Kind: kind}},
		}
		streamSeq++
		if err := ch.Insert(ctx, mustRowsFor(t, env, streamSeq)); err != nil {
			t.Fatalf("insert %s: %v", router, err)
		}
		f.tsRouter[psViewKey{collector, router, peer}] = tsRouter
	}
	event := func(collector, router, peer string, sid uint64, kind vantagev1.PeerEvent_Kind, tsRouter time.Time) {
		t.Helper()
		eventAt(collector, router, peer, sid, kind, tsRouter, 1, base)
	}
	for i, c := range peerStatusLiveness {
		collector := psLivenessCollector(f.run, i)
		sid := uint64(base.UnixNano()) + uint64(i)
		event(collector, c.router, psPeer, sid, c.stored, base)
		beat(collector, sid, c.restarted, c.beatAge)
	}
	for i, c := range peerStatusMerge {
		sid := uint64(base.UnixNano()) + uint64(100+10*i)
		for j, v := range c.views {
			collector := psLivenessCollector(f.run, 100+10*i+j)
			event(collector, c.router, psPeer, sid, v.stored, base.Add(v.tsRouter))
			beat(collector, sid, v.restarted, v.beatAge)
		}
	}
	// The superseded session: psPeerGone was up in the older session and the
	// router's current session does not carry it. Both sessions belong to the
	// collector's current process, so nothing here reads view_lost by epoch;
	// the older session is simply not current.
	collector := psLivenessCollector(f.run, 200)
	older := uint64(base.UnixNano()) + 200
	newer := older + uint64(time.Minute)
	event(collector, psSupersededRouter, psPeer, older, vantagev1.PeerEvent_KIND_UP, base)
	event(collector, psSupersededRouter, psPeerGone, older, vantagev1.PeerEvent_KIND_UP, base)
	event(collector, psSupersededRouter, psPeer, newer, vantagev1.PeerEvent_KIND_UP, base.Add(time.Minute))
	beat(collector, older, false, time.Second)

	// The clock step: the down is seq 2 and the up seq 1, but the down's
	// ts_collector is a minute earlier. The API orders on seq and reads down.
	collector = psLivenessCollector(f.run, 300)
	sid := uint64(base.UnixNano()) + 300
	eventAt(collector, psClockStepRouter, psPeer, sid, vantagev1.PeerEvent_KIND_UP, base, 1, base.Add(time.Minute))
	eventAt(collector, psClockStepRouter, psPeer, sid, vantagev1.PeerEvent_KIND_DOWN, base.Add(time.Minute), 2, base)
	beat(collector, sid, false, time.Second)
	return f
}

type peerStatusCounts struct{ up, down, viewLost, unspecified, stale uint64 }

func (c peerStatusCounts) of(state string) uint64 {
	return map[string]uint64{"up": c.up, "down": c.down, "view_lost": c.viewLost,
		"unspecified": c.unspecified, "stale": c.stale}[state]
}

func (c peerStatusCounts) total() uint64 {
	return c.up + c.down + c.viewLost + c.unspecified + c.stale
}

func (c *peerStatusCounts) add(state string) {
	switch state {
	case "up":
		c.up++
	case "down":
		c.down++
	case "view_lost":
		c.viewLost++
	case "unspecified":
		c.unspecified++
	case "stale":
		c.stale++
	}
}

// psInnerGrouping is the panel's per-view grouping, where a WHERE scoping it
// to a fixture belongs: the panel resolves each view before merging them.
const psInnerGrouping = "GROUP BY collector_id, router_ip, peer_ip, rib, sid"

// peerStatusFor runs "Peer status" scoped to routers and to this run's
// collectors, so no other fixture's peers are counted.
func peerStatusFor(t *testing.T, ctx context.Context, ch *ClickHouse, run int64, routers ...string) peerStatusCounts {
	t.Helper()
	sql := panelSQL(t, "fleet-health", "Peer status", "A")
	if n := strings.Count(sql, psInnerGrouping); n != 1 {
		t.Fatalf("the peer-status query carries %d occurrences of %q, want 1", n, psInnerGrouping)
	}
	in := make([]string, len(routers))
	for i, r := range routers {
		in[i] = "toIPv6('" + r + "')"
	}
	sql = strings.Replace(sql, psInnerGrouping, fmt.Sprintf(
		"WHERE peer_current.router_ip IN (%s) AND startsWith(peer_current.collector_id, '%sps-%d-') %s",
		strings.Join(in, ", "), chtest.LivenessPrefix, run, psInnerGrouping), 1)
	var c peerStatusCounts
	if err := ch.conn.QueryRow(ctx, qualify(ch, substituteGrafana(sql, nil))).
		Scan(&c.up, &c.down, &c.viewLost, &c.unspecified, &c.stale); err != nil {
		t.Fatalf("fleet-health peer status: %v", err)
	}
	return c
}

// TestFleetPeerStatusAppliesCollectorLiveness: the pie counts each peer in the
// state the API would give it. Each branch has a row that takes it and a row
// that does not:
//
//   - a fresh collector's up peer stays up, and a silent one's reads stale;
//   - a collector never heard from reads stale too;
//   - a restarted collector's old session reads view_lost -- but a peer that
//     session reported DOWN stays down;
//   - across two collectors, a live up outranks a stale, a stale outranks a
//     view_lost, a live down outranks a stale, and a view_lost outranks an
//     unspecified, whatever ts_router says between them; a dead collector's
//     down outranks another dead collector's newer stale up; between an up
//     and a down, the newer ts_router wins;
//   - a peer only a superseded session carries is not counted, and the peer
//     the current session carries is;
//   - within one view, the later event by seq wins, even when the
//     collector's clock stepped back between the two.
func TestFleetPeerStatusAppliesCollectorLiveness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	f := insertPeerStatusLivenessFixture(t, ctx, ch)

	for _, c := range peerStatusLiveness {
		got := peerStatusFor(t, ctx, ch, f.run, c.router)
		if got.of(c.want) != 1 || got.total() != 1 {
			t.Errorf("%s: counts %+v, want exactly one %s", c.router, got, c.want)
		}
	}
	for _, c := range peerStatusMerge {
		got := peerStatusFor(t, ctx, ch, f.run, c.router)
		if got.of(c.want) != 1 || got.total() != 1 {
			t.Errorf("%s: counts %+v, want exactly one %s -- %s", c.router, got, c.want, c.why)
		}
	}
	if got := peerStatusFor(t, ctx, ch, f.run, psSupersededRouter); got.up != 1 || got.total() != 1 {
		t.Errorf("%s: counts %+v, want exactly one up -- %s is carried only by a "+
			"superseded session and the API does not serve it; %s is carried by the "+
			"current one", psSupersededRouter, got, psPeerGone, psPeer)
	}
	if got := peerStatusFor(t, ctx, ch, f.run, psClockStepRouter); got.down != 1 || got.total() != 1 {
		t.Errorf("%s: counts %+v, want exactly one down -- the down is the later event "+
			"by seq, and the collector's clock stepped back between the two", psClockStepRouter, got)
	}
}

// psMergeAPIViews is the cross-collector merge, applied to the API's rows
// for one (router, peer): the rule the panel implements in SQL, written a
// second time in Go so the two can be compared.
//
//   - A router statement -- up or down from a live view -- outranks every
//     collector verdict; between two, the newer ts_router wins, the one clock
//     the collectors watching one router share.
//   - Otherwise stale, then view_lost, then unspecified: stale means the peer
//     was up when its collector was last heard from, and view_lost says
//     nothing about the peer at all.
func psMergeAPIViews(t *testing.T, f psFixture, router, peer string, views []query.Peer) string {
	t.Helper()
	best, bestTS := "", time.Time{}
	for _, v := range views {
		if v.State != "up" && v.State != "down" {
			continue
		}
		ts, ok := f.tsRouter[psViewKey{v.Collector, router, peer}]
		if !ok {
			t.Fatalf("%s/%s: the API serves a view from %s the fixture did not write", router, peer, v.Collector)
		}
		if best == "" || ts.After(bestTS) {
			best, bestTS = v.State, ts
		}
	}
	if best != "" {
		return best
	}
	for _, s := range []string{"stale", "view_lost", "unspecified"} {
		for _, v := range views {
			if v.State == s {
				return s
			}
		}
	}
	t.Fatalf("%s/%s: no view resolves to a state the panel counts: %+v", router, peer, views)
	return ""
}

// TestFleetPeerStatusAgreesWithTheAPI: over the same rows, the panel and
// query.Peers -- what /v1/peers and the CLI serve -- merged one state per
// (router, peer), give the same counts. The panel is a separate
// implementation of the rule, in a dashboard, and this is the test that
// keeps the two from drifting: on the current-session scope (the API serves
// no peer of a superseded session) and on the merge across collectors.
//
// Both sides are pinned: the API's merged counts must first equal the
// fixture's intent, so a fixture the API reads differently fails here rather
// than agreeing with a wrong panel.
func TestFleetPeerStatusAgreesWithTheAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	f := insertPeerStatusLivenessFixture(t, ctx, ch)
	q, err := query.New(ch.Conn(), ch.DB())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]peerStatusCounts{psSupersededRouter: {up: 1}, psClockStepRouter: {down: 1}}
	for _, c := range peerStatusLiveness {
		var w peerStatusCounts
		w.add(c.want)
		want[c.router] = w
	}
	for _, c := range peerStatusMerge {
		var w peerStatusCounts
		w.add(c.want)
		want[c.router] = w
	}
	prefix := fmt.Sprintf("%sps-%d-", chtest.LivenessPrefix, f.run)

	var apiAll peerStatusCounts
	for _, router := range psAllRouters() {
		peers, err := q.Peers(ctx, query.PeerFilter{Router: netip.MustParseAddr(router)})
		if err != nil {
			t.Fatal(err)
		}
		byPeer := map[string][]query.Peer{}
		for _, p := range peers {
			if strings.HasPrefix(p.Collector, prefix) {
				byPeer[p.PeerIP.String()] = append(byPeer[p.PeerIP.String()], p)
			}
		}
		var api peerStatusCounts
		for peer, views := range byPeer {
			api.add(psMergeAPIViews(t, f, router, peer, views))
		}
		if api != want[router] {
			t.Fatalf("%s: the API merges to %+v, want %+v -- fix the fixture before comparing",
				router, api, want[router])
		}
		if got := peerStatusFor(t, ctx, ch, f.run, router); got != api {
			t.Errorf("%s: the API merges to %+v, the panel counts %+v", router, api, got)
		}
		apiAll.up += api.up
		apiAll.down += api.down
		apiAll.viewLost += api.viewLost
		apiAll.unspecified += api.unspecified
		apiAll.stale += api.stale
	}
	if got := peerStatusFor(t, ctx, ch, f.run, psAllRouters()...); got != apiAll {
		t.Errorf("every fixture router at once: the API merges to %+v, the panel counts %+v", apiAll, got)
	}
}

// TestFleetPeerStatusThresholdIsTheQueryDefault: a dashboard cannot read the
// API's stale_after, so the panel spells the threshold out. It must spell the
// default, or the panel and an unconfigured API disagree about every
// collector between the two numbers.
func TestFleetPeerStatusThresholdIsTheQueryDefault(t *testing.T) {
	sql := panelSQL(t, "fleet-health", "Peer status", "A")
	want := fmt.Sprintf("toIntervalSecond(%d)", int(query.DefaultStaleAfter/time.Second))
	if !strings.Contains(sql, want) {
		t.Errorf("Peer status does not use %s, query.DefaultStaleAfter", want)
	}
}
