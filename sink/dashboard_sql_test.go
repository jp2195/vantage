package sink

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/types/known/timestamppb"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// This file runs the SQL that ships. Every query here is read out of the
// committed dashboard JSON under deploy/grafana/dashboards rather than
// copied into the test, because a copy drifts: the dashboard is what an
// operator opens, and a test that agrees with a stale duplicate of it
// proves nothing about what they see.
//
// Grafana renders variables and macros before sending SQL to ClickHouse, so
// the test has to do the same substitution. substituteGrafana below covers
// only the forms these dashboards actually use; anything else must be added
// here rather than worked around in the dashboard.

// panelSQL returns the rawSql of one target in one committed dashboard,
// identified by panel title and refId. It fails rather than skips when the
// panel is missing: a renamed panel that silently stops being tested is the
// failure mode this whole file exists to prevent.
func panelSQL(t *testing.T, dashboard, panelTitle, refID string) string {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				RefID  string `json:"refId"`
				RawSQL string `json:"rawSql"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, p := range d.Panels {
		if p.Title != panelTitle {
			continue
		}
		for _, tg := range p.Targets {
			if tg.RefID == refID {
				if strings.TrimSpace(tg.RawSQL) == "" {
					t.Fatalf("%s panel %q target %q has empty rawSql", path, panelTitle, refID)
				}
				return tg.RawSQL
			}
		}
		t.Fatalf("%s panel %q has no target with refId %q", path, panelTitle, refID)
	}
	t.Fatalf("%s has no panel titled %q", path, panelTitle)
	return ""
}

// dashboardVariableQuery returns the query SQL of one template variable in one
// committed dashboard, identified by name.
//
// It is panelSQL for templating.list[] instead of panels[].targets[], and it
// fails rather than skips for the reason panelSQL's comment gives: a variable
// that is renamed and silently stops being tested is the failure mode this
// file exists to prevent. The four $focus pickers are the only queries in
// these dashboards that resolve a BMP session without being a panel target,
// and they were invisible to the session-scoping debt ratchet until its walk
// was widened to reach them.
//
// The JSON walk itself is dashboardQueryVariables' -- including why a
// variable's query has to be read as any rather than as a string -- so this
// is the lookup and the failure message only.
func dashboardVariableQuery(t *testing.T, dashboard, name string) string {
	t.Helper()
	vars := dashboardQueryVariables(t, dashboard)
	q, ok := vars[name]
	if !ok {
		t.Fatalf("%s declares no $%s query variable; it declares %v",
			dashboard, name, slices.Sorted(maps.Keys(vars)))
	}
	return q
}

var timeFilterRe = regexp.MustCompile(`\$__timeFilter\(([^)]*)\)`)
var timeIntervalRe = regexp.MustCompile(`\$__timeInterval\(([^)]*)\)`)

// sqlStringVarRe matches Grafana's ${name:sqlstring} format option, which the
// looking glass uses so that an operator's own quote characters cannot end
// the SQL literal they sit in.
var sqlStringVarRe = regexp.MustCompile(`\$\{(\w+):sqlstring\}`)

// substituteGrafana renders the macros and variables Grafana would render
// before the ClickHouse plugin sends the query. The time window is
// deliberately wide (the fixture stamps ts_collector at corpusCollectorClock,
// i.e. now), so a panel's own time range never decides whether this test
// passes.
//
// Two variable forms are rendered, because the dashboards use two. A bare
// $name is substituted raw and the SQL supplies its own quotes -- that is
// what toIPv6('$router') expects, and what every dashboard but the looking
// glass does. ${name:sqlstring} is Grafana's own quoting: the value arrives
// already wrapped in single quotes, with any it contains doubled, so the SQL
// must NOT quote it again. Rendering it here is what makes the apostrophe
// case below a test of what Grafana sends rather than of a convenient fiction.
func substituteGrafana(sql string, vars map[string]string) string {
	sql = timeFilterRe.ReplaceAllString(sql, "$1 >= now() - INTERVAL 1 DAY AND $1 <= now() + INTERVAL 1 DAY")
	sql = timeIntervalRe.ReplaceAllString(sql, "toStartOfInterval($1, INTERVAL 1 MINUTE)")
	sql = sqlStringVarRe.ReplaceAllStringFunc(sql, func(m string) string {
		name := sqlStringVarRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			return m
		}
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	})
	// The ${name:sqlstring} pass above runs FIRST, so a value it substitutes is
	// still visible to the bare-$name pass below. A variable value containing
	// the literal text "$rib" would therefore be substituted a second time.
	// Grafana does not work this way -- it renders each reference once against
	// the original template -- so a test that picked such a value would be
	// asserting against SQL Grafana never sends. No current fixture value
	// contains a "$", and this is test-only code, so the two-pass shape stands;
	// a future adversarial value is the thing to watch for.
	//
	// Longest name first, so $router does not partially match $router_x.
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if len(names[j]) > len(names[i]) {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, n := range names {
		sql = strings.ReplaceAll(sql, "$"+n, vars[n])
	}
	return sql
}

const (
	dashRouterIP = "10.0.199.1"
	// dashRouterIPv6 is the same address as ClickHouse stores and renders it.
	// router_ip is IPv6, so an IPv4 router arrives back as a v4-mapped v6
	// address -- which is exactly what the dashboards' $router variable
	// (IPv6NumToString(router_ip)) puts in the picker.
	dashRouterIPv6 = "::ffff:10.0.199.1"
	dashPeerIP     = "10.255.199.1"
	// dashPeerIPB is a SECOND BGP peer under the same router and the same BMP
	// session. $peer is the control that keeps route-
	// reflector disagreement observable rather than averaged away, and the
	// target network has three RRs -- but with one peer per router in the
	// fixture, deleting "AND peer_ip = toIPv6('$peer')" from every panel
	// changed no test at all.
	dashPeerIPB = "10.255.199.5"
	// dashStale is numerically LESS than dashSession, which is the shape a
	// collector actually produces: session_id is now().UnixNano() behind a
	// monotonic guard, so a later session carries a higher id.
	//
	// The dashboards resolve the current session with max(session_id), not
	// argMax(session_id, (ts_collector, stream_seq)). ts_collector is a
	// collector wall clock too, and it LEADS that sort tuple, so stream_seq
	// only breaks exact ties. So argMax, which looks robust against clock
	// trouble, is the more clock-dependent of the two -- argMax loses to a
	// backward step of any size between two sessions, while max only loses
	// to one bigger than the previous session's whole age. Measured against
	// a lab archive: 1,208 session transitions across 15 router views, zero
	// inversions of session_id against first-seen time, zero rows where
	// ts_collector went backwards, and zero (collector, router) pairs where
	// the two expressions disagree. max() and argMax give the same answer
	// on everything the lab archive holds.
	//
	// What this costs: an inverted pair would let THIS fixture tell max from
	// argMax dynamically, but the inverted shape is not one a collector
	// produces. That job belongs to
	// TestDashboardsResolveTheCurrentSessionTheWayQueryDoes, which asserts the
	// expression itself rather than an answer -- the same trade
	// TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree makes on the Go side,
	// and for the same reason.
	dashSession = 9002
	dashStale   = 9001
)

// dashSessionAge is how long before corpusCollectorClock the fixture's
// current BMP session came up. The "Current session age" stat must report
// this, not the age of the session's newest event.
const dashSessionAge = 2 * time.Hour

// topologyVars is what Grafana's template engine would produce for the
// fixture's router and peer. state is the rendered $state list ("0" or
// "0, 1"); focus is a node_key, or "0" for All.
//
// The values are bare, not quoted: Grafana substitutes the raw value and the
// SQL supplies the quotes itself (toIPv6('$router')), so quoting here would
// send the database something Grafana never sends.
func topologyVars(focus, state string) map[string]string {
	return topologyVarsFor(dashPeerIP, focus, state)
}

// topologyVarsFor is topologyVars with the BMP peer chosen, for the tests that
// check a panel returns one peer's view and not another's.
func topologyVarsFor(peer, focus, state string) map[string]string {
	return topologyVarsOn(dashRouterIP, peer, focus, state)
}

// topologyVarsOn is topologyVarsFor with the advertising router chosen too,
// for the fixtures that reserve a router of their own rather than adding rows
// to insertTopologyFixture's counts.
//
// collector defaults to "c1" -- every single-collector fixture in this file
// writes its rows under that id -- so a caller that has not heard of the
// $collector picker still renders a value link-state-nodes' stat can match.
// A dual-homed test overrides the key explicitly (see dualHomedVars' callers)
// rather than relying on this default.
func topologyVarsOn(router, peer, focus, state string) map[string]string {
	return map[string]string{
		"router":    router,
		"peer":      peer,
		"state":     state,
		"focus":     focus,
		"collector": "c1",
	}
}

func lsDesc(routerID []byte) *vantagev1.LsNodeDescriptor {
	return &vantagev1.LsNodeDescriptor{Asn: 65199, BgplsId: 0, Area: 0, RouterId: routerID}
}

// insertTopologyFixture writes a deterministic link-state topology through
// the real RowsFor + Insert path, under a router IP no other test in this
// package uses (testDB is shared and never truncated).
//
// It covers, in one session, every shape the topology queries must handle:
// a node titled by name, one by router_id_v4, one with neither (the observed
// NX-OS case, whose only label is hex), an advertised node with no adjacency
// (a shape real routers do produce), a link to an endpoint never advertised as a
// node, and a withdrawn link. A second, older session carries a link that
// must never appear, which is what makes the session scoping testable.
func insertTopologyFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	var (
		a = []byte{10, 255, 19, 1}
		b = []byte{10, 255, 19, 2}
		c = []byte{10, 255, 19, 3}
		d = []byte{10, 255, 19, 4}
		e = []byte{10, 255, 19, 5}
		// F and G belong to the second peer only.
		f = []byte{10, 255, 19, 11}
		g = []byte{10, 255, 19, 12}
		// P is an OSPFv2 LAN pseudonode: RFC 9552 widens the IGP Router-ID to
		// Router-ID (10.255.19.2) plus the DR's interface address (10.0.19.1)
		// rather than flagging it, so 8 bytes is the only thing that says this
		// vertex is an Ethernet segment and not a router. It carries no name
		// and no router_id_v4, exactly as a real one does,
		// which is what makes it indistinguishable from an unnamed router
		// unless something reads the width.
		pn = []byte{10, 255, 19, 2, 10, 0, 19, 1}
	)
	env := func(peer string, sessionID uint64, ts time.Time, payload *vantagev1.LsEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: dashRouterIP, SysName: "dash-r1"},
			Peer:        &vantagev1.PeerId{Ip: peer},
			SessionId:   sessionID,
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Ls{Ls: payload},
		}
	}
	// The current session's link-state arrives at the start of the session;
	// the STALE session's arrives later still. That ordering is the whole
	// point of this fixture. A BMP transport that drops without a Peer Down
	// -- one router did this 234 times over -- leaves the old session sending
	// while the new one comes up, so an ls_* row from the superseded session
	// can carry the newest ts_collector and the newest stream_seq of anything
	// in the table. Only the peer event tables (peer_events, and peer_current,
	// which the current-state panels read and which holds the same rows) know
	// which session is current, and this arrangement is here to prove that:
	// derive the session from the link-state table being queried and every one
	// of these tests must fail.
	currentLsTS := corpusCollectorClock.Add(-90 * time.Minute)
	staleLsTS := corpusCollectorClock.Add(-30 * time.Minute)
	// The link IDs are derived from the endpoints rather than passed in, so a
	// withdrawal built from the same two addresses lands in the same GROUP BY
	// bucket as its advertisement -- link_local_id and link_remote_id are part
	// of a link's identity tuple, and a withdraw carrying different ones would
	// silently become a second link instead of retiring the first.
	link := func(local, remote []byte, withdraw bool, igp uint32) *vantagev1.LsLink {
		return &vantagev1.LsLink{
			Protocol: 3, Identifier: 100,
			Local: lsDesc(local), Remote: lsDesc(remote),
			LocalIfaddr: local, RemoteIfaddr: remote,
			LinkLocalId: uint32(local[3]) * 100, LinkRemoteId: uint32(remote[3]) * 100,
			IsWithdraw: withdraw, IgpMetric: igp, TeMetric: igp * 2,
		}
	}

	// peerEvent builds one peer_events row. The current session deliberately
	// carries several, spread over dashSessionAge, because "how long since
	// this router last reconnected" is a question about when the session
	// BEGAN: a session whose first and last event are the same row cannot
	// tell min(ts_collector) from max(ts_collector). On a lab router, the
	// newest session spans 5497 seconds across 18 events.
	peerEvent := func(peer string, sessionID uint64, kind vantagev1.PeerEvent_Kind, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: dashRouterIP, SysName: "dash-r1"},
			Peer:        &vantagev1.PeerId{Ip: peer},
			SessionId:   sessionID,
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{Kind: kind}},
		}
	}
	// A Peer Up in the current session: the topology queries take the
	// session from peer_events, not from the link-state tables, because a
	// router can advertise nodes and no links at all.
	peerUp := peerEvent(dashPeerIP, dashSession, vantagev1.PeerEvent_KIND_UP,
		corpusCollectorClock.Add(-dashSessionAge))
	// The BGP peer flaps while the BMP session stays up. This is what makes
	// the session's newest event much younger than the session itself.
	peerFlapDown := peerEvent(dashPeerIP, dashSession, vantagev1.PeerEvent_KIND_DOWN,
		corpusCollectorClock.Add(-time.Hour))
	peerFlapUp := peerEvent(dashPeerIP, dashSession, vantagev1.PeerEvent_KIND_UP,
		corpusCollectorClock)
	// Older than the current session's Peer Up, so peer_events still names
	// dashSession as current.
	stalePeerUp := peerEvent(dashPeerIP, dashStale, vantagev1.PeerEvent_KIND_UP,
		corpusCollectorClock.Add(-dashSessionAge-time.Hour))

	// A second BGP peer under the SAME router and the SAME BMP session,
	// carrying its own node, its own link and its own prefix. Nothing it
	// advertises may appear when $peer names the first peer.
	peerBUp := peerEvent(dashPeerIPB, dashSession, vantagev1.PeerEvent_KIND_UP,
		corpusCollectorClock.Add(-100*time.Minute))

	envs := []*vantagev1.Envelope{
		stalePeerUp,
		peerUp,
		peerBUp,
		peerFlapDown,
		peerFlapUp,
		env(dashPeerIP, dashSession, currentLsTS, &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				// A carries the full segment-routing attribute set the nodes
				// table is specified to show, so SRGB, SRLB, the algorithm
				// list and an unknown TLV are all asserted against a value
				// rather than against "the column exists".
				{Protocol: 3, Identifier: 100, Local: lsDesc(a), Name: "dash-a",
					SrgbBase: 16000, SrgbSize: 8000,
					SrlbBase: 15000, SrlbSize: 1000,
					SrAlgorithms: []uint32{0, 1},
					UnknownTlvs:  map[uint32][]byte{999: {0x01, 0x02}}},
				{Protocol: 3, Identifier: 100, Local: lsDesc(b), RouterIdV4: []byte{10, 255, 29, 2}},
				{Protocol: 3, Identifier: 100, Local: lsDesc(c)},
				{Protocol: 3, Identifier: 100, Local: lsDesc(d), Name: "dash-d"},
				{Protocol: 3, Identifier: 100, Local: lsDesc(pn)},
			},
			Links: []*vantagev1.LsLink{
				func() *vantagev1.LsLink {
					// A->B carries two adjacency SIDs so the links table's
					// arrayStringConcat join is exercised on a populated
					// array, not just proven not to crash on an empty one.
					l := link(a, b, false, 10)
					l.AdjSids = []*vantagev1.LsAdjacencySid{
						{Sid: 24001, Flags: 0x30, Weight: 0},
						{Sid: 24002, Flags: 0x30, Weight: 0},
					}
					return l
				}(),
				link(b, c, false, 20),
				link(a, e, false, 30),
				link(c, a, false, 40),
			},
		}),
		// The withdraw arrives after the advertisement, as it would on the
		// wire: same ts_collector, higher stream_seq, which is the tiebreak
		// every argMax in these queries is ordered on.
		env(dashPeerIP, dashSession, currentLsTS, &vantagev1.LsEvent{
			Links: []*vantagev1.LsLink{link(c, a, true, 40)},
		}),
		// Prefixes touch no node or link, so the counts the topology tests
		// assert stay exactly as they were.
		env(dashPeerIP, dashSession, currentLsTS, &vantagev1.LsEvent{
			Prefixes: []*vantagev1.LsPrefix{
				{Protocol: 3, Identifier: 100, Local: lsDesc(a),
					Prefix: []byte{10, 199, 1, 0}, PrefixLen: 24,
					OspfRouteType: 1, PrefixSid: 16001, HasPrefixSid: true, PrefixMetric: 10},
				{Protocol: 3, Identifier: 100, Local: lsDesc(b),
					Prefix: []byte{10, 199, 2, 0}, PrefixLen: 24,
					OspfRouteType: 1, PrefixMetric: 20},
			},
		}),
		env(dashPeerIPB, dashSession, currentLsTS, &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Local: lsDesc(f), Name: "dash-f"},
			},
			Links: []*vantagev1.LsLink{link(f, g, false, 50)},
			Prefixes: []*vantagev1.LsPrefix{
				{Protocol: 3, Identifier: 100, Local: lsDesc(f),
					Prefix: []byte{10, 199, 11, 0}, PrefixLen: 24,
					OspfRouteType: 1, PrefixMetric: 50},
			},
		}),
		// The stale session's link-state, LAST and NEWEST: staleLsTS is later
		// than every current-session ls_* row, and being last here gives it
		// the highest stream_seq too. So argMax(session_id, (ts_collector,
		// stream_seq)) over ls_links, ls_nodes or ls_prefixes returns
		// dashStale, while peer_events -- the only source that knows -- still
		// returns dashSession. Nothing below may show any of these objects.
		env(dashPeerIP, dashStale, staleLsTS, &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 255, 19, 7}), Name: "dash-stale"},
			},
			Links: []*vantagev1.LsLink{link([]byte{10, 255, 19, 8}, []byte{10, 255, 19, 9}, false, 99)},
			Prefixes: []*vantagev1.LsPrefix{
				{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 255, 19, 7}),
					Prefix: []byte{10, 199, 9, 0}, PrefixLen: 24,
					OspfRouteType: 1, PrefixMetric: 90},
			},
		}),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert fixture envelope %d: %v", i, err)
		}
	}

	// One envelope arrives TWICE, which is what a JetStream redelivery does:
	// the second insert writes a byte-identical row at the same stream_seq
	// (TestRowsForRedeliveryIsByteIdentical), so ReplacingMergeTree holds
	// both until a merge collapses them and FINAL -- or an explicit
	// uniqExact -- is the only thing that makes a query see one.
	//
	// Without this, every FINAL in the four link-state dashboards is a no-op
	// against this fixture and nothing here can tell correct dedup from
	// missing dedup. Measured before it existed: stripping all 19 FINAL
	// occurrences from fleet-health.json failed no behavioral test at all,
	// and stripping all 113 across every dashboard failed only the tests
	// whose fixtures already carried a duplicate (evpn-churn, route-churn,
	// parse-anomalies).
	//
	// It duplicates a genuine ADVERTISEMENT in the CURRENT session, for the
	// reason insertRouteChurnFixture gives for its own: a duplicated session
	// dump is still a session dump and would prove nothing.
	//
	// HOW MUCH THIS BUYS, stated honestly: the duplicate is observable only
	// until a background merge collapses it, which on a table this small is
	// often a second or two. So the sensitivity it gives the tests below is
	// best-effort, exactly as it is for the three fixtures that carried a
	// re-delivery before this one. A test asserting the duplicate is still
	// PRESENT was written and removed: it needed SYSTEM STOP MERGES to be
	// deterministic, and that mutates state shared with every other package
	// `make test` runs concurrently -- too high a price on a database that
	// tests share, where one test's state change is every test's. The
	// deterministic half of this job belongs to
	// TestEveryCountingDashboardTargetDedups, which needs no database at
	// all.
	redelivered := -1
	for i, ev := range envs {
		ls := ev.GetLs()
		if ls == nil || ev.SessionId != dashSession || len(ls.GetNodes()) == 0 {
			continue
		}
		redelivered = i
		break
	}
	if redelivered < 0 {
		t.Fatal("no current-session link-state advertisement in this fixture to " +
			"redeliver; the duplicate has to be a real advertisement or it " +
			"proves nothing about dedup")
	}
	if err := ch.Insert(ctx, mustRowsFor(t, envs[redelivered], uint64(redelivered+1))); err != nil {
		t.Fatalf("insert topology fixture re-delivery: %v", err)
	}
}

const (
	// dualHomedRouterIP is a router that TWO collectors both monitor. It is a
	// block of its own rather than a second peer under dashRouterIP, because
	// every existing link-state assertion counts rows under that router and a
	// second collector's copies would change every one of those counts.
	//
	// 200, not 198: 10.0.198.1 and 10.255.198.1 are already
	// TestEORMarkerBecomesAnEORRowAndNotARoute's router and peer in
	// clickhouse_test.go, and testDB is shared and never truncated.
	dualHomedRouterIP   = "10.0.200.1"
	dualHomedRouterIPv6 = "::ffff:10.0.200.1"
	dualHomedPeerIP     = "10.255.200.1"
	// dualHomedSysname is the one sysname this router reports. It is one
	// router, not two: both collectors see the same name, which is what makes
	// the completeness table's rows for it two answers rather than two
	// routers.
	dualHomedSysname = "dual-r1"

	// dualCollectorA holds the LARGER session id, so max(session_id) with no
	// collector in it resolves to A and discards B. That ordering is the whole
	// point: reverse it and the shipped SQL would pass by accident.
	//
	// The ids are "dual-c1"/"dual-c2" and NOT the "c1"/"c2" every
	// single-collector fixture in this file writes. Two things come of that.
	// The tables, the graph and the $focus picker read no $collector at all,
	// so nothing here depended on topologyVarsOn's "c1" default matching --
	// every test that renders a repeated tile sets vars["collector"] itself.
	// And a future one that FORGETS to now gets a value matching no row under
	// this router and fails loudly, where before it would have quietly
	// rendered collector A's answer and passed for the wrong reason.
	//
	// They keep A before B lexically, which the merged panels rely on:
	// argMin(..., collector_id) is how the graph and the edge metrics choose
	// one collector's value, and every assertion about that choice names A.
	dualCollectorA = "dual-c1"
	dualCollectorB = "dual-c2"
	dualSessionA   = uint64(9_000_000_000_000_000_000)
	dualSessionB   = uint64(1_000_000_000_000_000_000)

	// dualSessionAAge is how long before corpusCollectorClock collector A's
	// session came up, and dualSessionBHeadStart is how much EARLIER than
	// that collector B's came up.
	//
	// The two must differ. "Current session age" reports min(ts_collector)
	// over the session it resolved, so if both collectors' sessions began at
	// the same instant, a tile that resolved the other collector's session
	// would print the right number anyway and
	// TestDualHomedTopologyStatsCountOnlyTheirOwnCollector's age case could
	// not fail. B is the older session and the smaller session id at once,
	// which is the adverse pairing: max(session_id) with no collector in it
	// picks A, so the merged answer is the YOUNGER age for both tiles.
	dualSessionAAge       = 60 * time.Minute
	dualSessionBHeadStart = 30 * time.Minute

	// dualSharedNodeLast is the last octet of the ONE node both collectors
	// advertise, and the two names they advertise it under. Everything else
	// the fixture writes belongs to exactly one collector, which is what makes
	// the per-collector counts unequal; this node is the deliberate exception,
	// because with no shared node_key a collector-keyed join and a merged join
	// return byte-identical rows and neither can be falsified.
	//
	// The names must stay DIFFERENT. Identical names would still share a
	// node_key but would make a merge invisible in the rendered output.
	// They embed the octet by hand -- a const cannot call fmt.Sprintf.
	dualSharedNodeLast  = byte(6)
	dualSharedNodeNameA = "dual-node-6-c1"
	dualSharedNodeNameB = "dual-node-6-c2"
	// dualSharedNodeSrgb* are the SRGBs the two collectors report for that
	// same node, and they DIFFER for the reason the two names do.
	//
	// The topology nodeset resolves srgb_base and srgb_size with
	// argMin(..., collector_id), exactly as it resolves the name. The argument
	// that a merged node's columns need no coverage because they are
	// "determined by node_key" covers router_id and is_pseudonode, which are
	// inputs to the key or derived from one; it does not cover an SRGB, which
	// is a property of what each collector HEARD and which two collectors can
	// legitimately report differently. With one SRGB for both sightings that
	// choice is not decidable, and those two argMins could be rewritten to
	// anything -- argMax over two collectors' clocks included -- with no
	// assertion moving.
	//
	// The SIZES differ as well as the bases because the graph renders the two
	// as one string: with equal sizes, argMin(srgb_size, collector_id) is
	// still undecidable.
	//
	// The third column in that family, argMin(router_id_v4, collector_id),
	// stays uncovered and cannot be covered from here: the nodes target reads
	// router_id_v4 only as a title FALLBACK, and this node has a name, so no
	// per-collector value of it reaches the output. Covering it would need a
	// second shared node with no name at all, which would move every count in
	// this fixture.
	dualSharedNodeSrgbBaseA = uint32(16000)
	dualSharedNodeSrgbSizeA = uint32(8000)
	dualSharedNodeSrgbBaseB = uint32(24000)
	dualSharedNodeSrgbSizeB = uint32(4000)

	// dualBorrowedOriginLast is the node whose prefix BOTH collectors
	// announce while only collector A advertises the node itself. It is the
	// one row in this fixture that can tell a collector-keyed nodeset join
	// from a merged one: collector A renders dualBorrowedOriginName for it,
	// collector B must render dualBorrowedOriginFallback -- the router_id
	// hex decode -- rather than borrowing A's name.
	dualBorrowedOriginLast = byte(1)
	dualBorrowedOriginPfx  = "10.207.0.1/32"
	dualBorrowedOriginName = "dual-node-1"
	// Equal to dualHomedPeerIP by coincidence: node descriptor addresses and
	// the BMP peer address share the 10.255.200.x block (see the node helper),
	// and this decode is of the NODE's router_id, not of the peer.
	dualBorrowedOriginFallback = "10.255.200.1"

	// dualWithdrawnNodeLast is the node BOTH collectors advertise and then
	// only collector A withdraws, A's withdrawal carrying a LATER
	// (ts_collector, stream_seq) than B's advertisement.
	//
	// It is the one row that can tell a PER-COLLECTOR is_withdraw evaluation
	// from a cross-collector one. Evaluated across both, A's withdrawal
	// outranks B's live advertisement and the node vanishes from the merged
	// topology graph -- a live object erased because a DIFFERENT collector's
	// session withdrew it: a loss disguised as a withdrawal, at object
	// granularity instead of session granularity.
	//
	// The comparison it loses to is not merely debatable, it is undefined:
	// deploy/clickhouse/schema.sql calls ts_collector "the collector's own
	// clock" and says seq, stream_seq and the two timestamps are monotonic
	// only WITHIN a session. Across two collectors' sessions the tuple is two
	// unrelated clocks and two independent counters, so a merged argMax over
	// it resolves on clock skew.
	//
	// The two names DIFFER, like the shared node's above, so an assertion can
	// say WHICH collector's view the merged graph rendered: under $state = 0
	// it must be B's, because A's sighting is withdrawn and the state filter
	// runs BEFORE the display merge.
	dualWithdrawnNodeLast  = byte(7)
	dualWithdrawnNodeNameA = "dual-node-7-c1"
	dualWithdrawnNodeNameB = "dual-node-7-c2"

	// The same arrangement one table over, for the two targets that filter
	// EDGES: a link both collectors advertise and only collector A then
	// withdraws. Without it the identical restructuring of the two edge
	// HAVINGs would ship unfalsifiable -- every other link in this fixture
	// belongs to exactly one collector, so a merged withdraw decision and a
	// per-collector one return the same edges.
	//
	// Its FAR END is a node nobody ever advertises, which is what makes it
	// cover the second of those two HAVINGs. The Topology panel's nodes
	// target embeds its own copy of the edges query purely to collect
	// endpoints, and until this link existed every endpoint in the fixture
	// was also an advertised node -- so that embedded copy contributed
	// nothing to the node count and could have been scoped to one collector
	// with no assertion moving. Node 9 reaches the graph only through it.
	//
	// The two metrics DIFFER for the same reason the two names do: under
	// $state = 0 the surviving edge must carry B's metric.
	dualEndpointOnlyLast = byte(9)
	// dualEndpointOnlyTitle is what the graph labels node 9: it has no
	// ls_nodes row to take a name from, so the panel falls back to decoding
	// the router-id the link's descriptor carried.
	dualEndpointOnlyTitle    = "10.255.200.9"
	dualWithdrawnLinkLocal   = dualSharedNodeLast
	dualWithdrawnLinkRemote  = dualEndpointOnlyLast
	dualWithdrawnLinkMetricA = uint32(11)
	dualWithdrawnLinkMetricB = uint32(22)

	// The routes the two looking glasses read. 10.208.x and 10.209.x are
	// blocks none of this package's numbered-prefix fixtures has claimed
	// (asnPfx*, topPfx*, evpnPfx*, churnPfx*, rbPfx*: 201 through 206), and
	// this fixture's own link-state prefixes sit in 10.207.x.
	//
	// dualHomedRoutePrefix and dualHomedVpnPrefix are announced by BOTH
	// collectors; every other route here belongs to exactly one of them,
	// which is what keeps the per-collector counts unequal. The shared pair
	// is not decoration. The route tables' per-row key is (router, peer, rib,
	// prefix, path_id) with no collector in it, so unless some prefix is
	// announced by BOTH collectors, adding collector_id to that GROUP BY
	// changes no row at all and a merged query is indistinguishable from a
	// per-collector one by counting, and asn-view's per-collector scoping
	// could not be told from a merge. Here it is what lets ONE $target
	// return a row per collector, which is what the label assertion reads.
	dualHomedRoutePrefix = "10.208.0.0/24"
	dualHomedVpnPrefix   = "10.209.0.0/24"
	// And one MORE SPECIFIC route under each of those, announced by collector
	// A alone. Without them the two collectors' tiles would report the
	// identical three numbers -- one router advertising, /24, zero superseded
	// -- and the $collector filter that makes a repeated tile count only its
	// own collector could be deleted with every assertion still passing.
	// These make most_specific differ per collector: 25 for A, 24 for B, and
	// 25 for BOTH tiles the moment the filter goes.
	//
	// They cover the same target as the /24s: a typed prefix is reduced to
	// its network address, and 10.208.0.0 is inside 10.208.0.0/25.
	dualHomedRouteMoreSpecific = "10.208.0.0/25"
	dualHomedVpnMoreSpecific   = "10.209.0.0/25"
	// dualHomedVpnOtherMoreSpecific is a /25 collector B alone announces,
	// under a base prefix that does NOT cover dualHomedVpnPrefix's network
	// address. It is therefore invisible to the $target the VPN stat tiles
	// use and changes none of their numbers.
	//
	// It exists so that "ORDER BY collector, ..." on the VPN table is
	// falsifiable. Without it collector B holds exactly one covering row,
	// tying collector A's /24 on every remaining sort key, so deleting the
	// leading collector leaves the rows collector-contiguous or not depending
	// on how ClickHouse breaks that tie -- a clause deletable with the suite
	// green half the time, which is worse than one deletable outright. A B
	// row that outranks an A row on prefix_len makes the two collectors'
	// rows interleave without the clause, whatever the tie-break does. The
	// unicast side already has this shape and needs no equivalent: collector
	// B's 10.208.3.0/24 sorts after two of collector A's /24s.
	//
	// dualHomedVpnUnsharedA below now supplies that property too, from the
	// other direction -- an A row that sorts after B's /24 -- so either row
	// alone would keep the clause falsifiable and the property is no longer
	// this constant's alone to carry. Which is exactly why the check is a
	// derived guard inside
	// TestDualHomedLookingGlassGroupsEachCollectorsRowsTogether rather than a
	// claim written down here: if both ever go, the guard says so.
	dualHomedVpnOtherMoreSpecific = "10.209.8.0/25"
	// dualHomedVpnUnsharedA is a route collector A alone announces, also
	// outside the stat tiles' $target, and it exists for the arithmetic
	// rather than for any single assertion.
	//
	// dualHomedVpnOtherMoreSpecific left route_vpn at two rows per collector,
	// which is the one shape this fixture is built never to have: with equal
	// counts, "kept A" and "kept B" are the same number and a per-collector
	// row-count assertion -- the ONLY check that catches a merged inner
	// aggregation, since the debt ratchet matches strings -- could not tell
	// them apart. Three against two restores that, and makes the merged
	// answer (four distinct keys, because the /24 is shared) a third
	// distinguishable number. The same argument dualHomedASNPrefixRows makes
	// for route_unicast's four against two.
	dualHomedVpnUnsharedA = "10.209.4.0/24"
	// dualHomedRouteRD is the Route Distinguisher both collectors report for
	// dualHomedVpnPrefix. One VRF seen twice, not two VRFs: the collectors
	// are two vantage points on one router, so they have to agree on it.
	dualHomedRouteRD = "64599:209"
	// dualHomedOriginASN is the origin of every route this fixture writes
	// (as_path[-1]) and dualHomedTransitASN the hop before it. Neither
	// appears anywhere else in this repo, so asn-view's $asn picker can
	// select this fixture's routes and nothing else.
	//
	// The path must be NON-EMPTY: asn-view wraps its inner query in
	// WHERE notEmpty(l.as_path), so a route carrying no path is invisible to
	// that dashboard entirely and could not be counted there.
	dualHomedTransitASN = uint32(64598)
	dualHomedOriginASN  = uint32(64599)
)

// insertDualHomedFixture writes one router watched by two collectors at once.
//
// Collector A sees nodes 1, 2, 6 and 7; collector B sees 3, 4, 5, 6 and 7.
// The counts are deliberately unequal, so a panel that keeps one collector
// cannot be mistaken for a panel that merged them: 4 and 5 are
// distinguishable from each other and from the 7 a merge returns, where
// 3-and-3 would make "kept A" and "kept B" indistinguishable.
//
// Node 7 and the link from node 6 to node 9 are the pair collector A then
// WITHDRAWS, so A holds four node keys but renders three rows. Every count
// this fixture feeds therefore comes in two flavors -- see
// dualHomedLinkStateDashboards' rowsA/keysA. Node 9 is never advertised as a
// node by anyone: it exists only at the far end of that link, and reaches the
// topology graph only through the endpoints union.
//
// Node 6 is the ONE overlap, and the two collectors name it differently. Two
// separate things need it, and neither can be falsified without it: the
// nodeset join on link-state-prefixes (collector-keyed or merged returns the
// same rows when no node_key is shared) and link-state-topology's
// merged-node-graph duplicate-id check (adding collector_id to that GROUP BY
// produces no duplicate when every node_key belongs to one collector).
// Collector B also announces the prefix originated by node 1, a node only
// collector A advertises -- the one row whose ORIGIN column differs between
// the two joins.
//
// It also writes BGP routes under the same router: four unicast prefixes from
// collector A and two from collector B, and three labeled (route_vpn)
// prefixes from A against two from B. Unequal in both tables, and in both one
// prefix is announced by both collectors while one is announced by A alone
// and is MORE SPECIFIC than it. Those are what the two looking glasses and
// asn-view read; see dualHomedRoutePrefix and dualHomedRouteMoreSpecific for
// what each of those two shapes is load-bearing for, and
// dualHomedVpnOtherMoreSpecific and dualHomedVpnUnsharedA for the two labeled
// routes that sit outside the stat tiles' $target on purpose.
//
// The lab archive has one collector and can never exercise this. That is the
// reason this fixture exists rather than a query against real data -- the
// same argument evpn-churn's collectorB helper makes for route_evpn.
func insertDualHomedFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	ts := corpusCollectorClock.Add(-dualSessionAAge)
	env := func(collector string, sessionID uint64, payload *vantagev1.LsEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: dualHomedRouterIP, SysName: dualHomedSysname},
			Peer:        &vantagev1.PeerId{Ip: dualHomedPeerIP},
			SessionId:   sessionID,
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Ls{Ls: payload},
		}
	}
	// The Peer Up carries its OWN timestamp rather than the fixture's shared
	// one, because the two collectors' sessions have to have come up at
	// different moments. "Current session age" reads min(ts_collector) of the
	// session it resolves, so with one clock for both sessions a tile that
	// resolved the wrong collector's session would render the right number
	// anyway, and the assertion could not fail.
	peerUp := func(collector string, sessionID uint64, up time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: dualHomedRouterIP, SysName: dualHomedSysname},
			Peer:        &vantagev1.PeerId{Ip: dualHomedPeerIP},
			SessionId:   sessionID,
			TsCollector: timestamppb.New(up),
			Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
				Kind: vantagev1.PeerEvent_KIND_UP,
			}},
		}
	}
	// Node and link descriptor addresses share dualHomedPeerIP's block
	// (10.255.200.x) rather than reserving a second one, the way dashPeerIP
	// and insertTopologyFixture's node addresses do not -- the columns they
	// land in (router_id vs. peer_ip) are different, so nothing here compares
	// them to each other.
	//
	// namedNode spells the display name out instead of deriving it, because
	// the one node BOTH collectors advertise is named differently by each of
	// them -- see dualSharedNodeLast.
	namedNode := func(last byte, name string) *vantagev1.LsEvent {
		return &vantagev1.LsEvent{Nodes: []*vantagev1.LsNode{{
			Protocol: 3, Identifier: 100,
			Local: lsDesc([]byte{10, 255, 200, last}),
			Name:  name,
		}}}
	}
	node := func(last byte) *vantagev1.LsEvent {
		return namedNode(last, fmt.Sprintf("dual-node-%d", last))
	}
	// srgbNode is namedNode carrying a segment-routing global block, for the
	// one node BOTH collectors advertise -- see dualSharedNodeSrgbBaseA.
	srgbNode := func(last byte, name string, base, size uint32) *vantagev1.LsEvent {
		ev := namedNode(last, name)
		ev.Nodes[0].SrgbBase, ev.Nodes[0].SrgbSize = base, size
		return ev
	}
	// withdrawnNode retracts what namedNode advertised: the same descriptors,
	// so the same node_key, with the withdrawal bit set. It has to be sent as
	// a LATER envelope than the advertisement it retracts, never instead of
	// it -- a node that was only ever withdrawn is a different fixture.
	withdrawnNode := func(last byte, name string) *vantagev1.LsEvent {
		ev := namedNode(last, name)
		ev.Nodes[0].IsWithdraw = true
		return ev
	}
	// One link and one prefix helper each, so the links and prefixes panels
	// have real numbers to assert rather than numbers this fixture cannot
	// produce.
	lnk := func(a, b byte) *vantagev1.LsEvent {
		return &vantagev1.LsEvent{Links: []*vantagev1.LsLink{{
			Protocol: 3, Identifier: 100,
			Local:       lsDesc([]byte{10, 255, 200, a}),
			Remote:      lsDesc([]byte{10, 255, 200, b}),
			LocalIfaddr: []byte{10, 255, 200, a}, RemoteIfaddr: []byte{10, 255, 200, b},
			LinkLocalId: uint32(a) * 100, LinkRemoteId: uint32(b) * 100,
			IgpMetric: 10, TeMetric: 20,
		}}}
	}
	// metricLnk is lnk with the IGP metric and the withdrawal bit spelled
	// out, for the one link both collectors advertise: the metric names which
	// collector a merged edge's numbers came from, and the bit is what makes
	// the two edge HAVINGs falsifiable.
	metricLnk := func(a, b byte, igp uint32, withdraw bool) *vantagev1.LsEvent {
		ev := lnk(a, b)
		ev.Links[0].IgpMetric = igp
		ev.Links[0].IsWithdraw = withdraw
		return ev
	}
	// The announced prefix uses 10.207.x, a block none of this package's
	// numbered-prefix fixtures (asnPfx*, topPfx*, evpnPfx*, churnPfx*,
	// rbPfx*: 201 through 206) has claimed.
	pfx := func(last byte) *vantagev1.LsEvent {
		return &vantagev1.LsEvent{Prefixes: []*vantagev1.LsPrefix{{
			Protocol: 3, Identifier: 100,
			Local:     lsDesc([]byte{10, 255, 200, last}),
			Prefix:    []byte{10, 207, 0, last},
			PrefixLen: 32,
		}}}
	}
	var envs []*vantagev1.Envelope
	envs = append(envs,
		peerUp(dualCollectorA, dualSessionA, ts),
		peerUp(dualCollectorB, dualSessionB, ts.Add(-dualSessionBHeadStart)))
	// Counts are UNEQUAL per collector in all three tables -- 3-vs-4 nodes,
	// 1-vs-2 links, 2-vs-4 prefixes -- so that "kept A", "kept B" and "merged"
	// are three distinguishable answers. An equal split could not be.
	for _, last := range []byte{1, 2} {
		envs = append(envs, env(dualCollectorA, dualSessionA, node(last)))
	}
	for _, last := range []byte{3, 4, 5} {
		envs = append(envs, env(dualCollectorB, dualSessionB, node(last)))
	}
	envs = append(envs, env(dualCollectorA, dualSessionA, lnk(1, 2)))
	envs = append(envs, env(dualCollectorB, dualSessionB, lnk(3, 4)), env(dualCollectorB, dualSessionB, lnk(4, 5)))
	for _, last := range []byte{1, 2} {
		envs = append(envs, env(dualCollectorA, dualSessionA, pfx(last)))
	}
	for _, last := range []byte{3, 4, 5} {
		envs = append(envs, env(dualCollectorB, dualSessionB, pfx(last)))
	}
	// The two rows that let a collector-keyed join be told apart from a merged
	// one. Everything above is disjoint per collector, which makes the two
	// joins produce byte-identical output; these do not. They are appended
	// LAST on purpose: testDB is shared and never truncated, so inserting in
	// the middle would renumber every later envelope's stream_seq against rows
	// an earlier run already wrote.
	//
	// One node BOTH collectors advertise, under DIFFERENT names. node_key is
	// cityHash64 over the node's own descriptors and carries no collector, so
	// the two sightings share a key.
	//
	// Each sighting also carries its OWN SRGB, which is what makes the two
	// SRGB argMins beside the name argMin decidable.
	envs = append(envs,
		env(dualCollectorA, dualSessionA, srgbNode(dualSharedNodeLast, dualSharedNodeNameA,
			dualSharedNodeSrgbBaseA, dualSharedNodeSrgbSizeA)),
		env(dualCollectorB, dualSessionB, srgbNode(dualSharedNodeLast, dualSharedNodeNameB,
			dualSharedNodeSrgbBaseB, dualSharedNodeSrgbSizeB)))
	// One prefix announced by a collector that never advertised its
	// originating node: c2 announces the prefix originated by node 1, and only
	// c1 advertises node 1. It goes on c2 rather than c1 deliberately -- on c1
	// it would make prefixes 3-and-3 and destroy the unequal-counts property
	// above.
	envs = append(envs, env(dualCollectorB, dualSessionB, pfx(dualBorrowedOriginLast)))
	// The node and the link that decide whether is_withdraw is evaluated per
	// collector or across both. Both collectors advertise each of them; only
	// collector A withdraws, and A's two withdrawals are the LAST envelopes
	// this fixture writes, so they carry the largest stream_seq here and win
	// a cross-collector argMax outright.
	//
	// Ordering, not timestamps, is what makes A's withdrawal later: every
	// ls_* envelope shares one ts, so (ts_collector, stream_seq) is decided
	// on the second element, which is the index below. That is also why these
	// go at the END -- see the note above about renumbering.
	envs = append(envs,
		env(dualCollectorA, dualSessionA, namedNode(dualWithdrawnNodeLast, dualWithdrawnNodeNameA)),
		env(dualCollectorB, dualSessionB, namedNode(dualWithdrawnNodeLast, dualWithdrawnNodeNameB)),
		env(dualCollectorA, dualSessionA, metricLnk(dualWithdrawnLinkLocal, dualWithdrawnLinkRemote, dualWithdrawnLinkMetricA, false)),
		env(dualCollectorB, dualSessionB, metricLnk(dualWithdrawnLinkLocal, dualWithdrawnLinkRemote, dualWithdrawnLinkMetricB, false)),
		env(dualCollectorA, dualSessionA, withdrawnNode(dualWithdrawnNodeLast, dualWithdrawnNodeNameA)),
		env(dualCollectorA, dualSessionA, metricLnk(dualWithdrawnLinkLocal, dualWithdrawnLinkRemote, dualWithdrawnLinkMetricA, true)),
	)
	// The BGP routes, for the two looking glasses and for asn-view. Appended
	// after the link-state envelopes for the renumbering reason above.
	//
	// Each collector's routes carry that collector's own session id, and that
	// session is the only one peer_events holds for it -- so every one of
	// these routes IS current, and every one of them must render
	// session_current = 1. That is the whole assertion: the joined-comparison
	// shape drops no rows, it mislabels them.
	routeEnv := func(collector string, sessionID uint64, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: dualHomedRouterIP, SysName: dualHomedSysname},
			Peer:        &vantagev1.PeerId{Ip: dualHomedPeerIP},
			SessionId:   sessionID,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	routeAttrs := &vantagev1.PathAttributes{
		Origin: 0, NextHop: "10.208.255.1",
		AsPath: []*vantagev1.AsPathSegment{{
			Type: 2, Asns: []uint32{dualHomedTransitASN, dualHomedOriginASN},
		}},
	}
	unicast := func(collector string, sessionID uint64, prefixes ...string) {
		for _, pfx := range prefixes {
			envs = append(envs, routeEnv(collector, sessionID, &vantagev1.RouteEvent{
				Family:    &vantagev1.Family{Afi: 1, Safi: 1},
				Attrs:     routeAttrs,
				Announced: []*vantagev1.Prefix{{Prefix: pfx}},
			}))
		}
	}
	// FOUR unicast routes for collector A and TWO for collector B, one of
	// them shared -- unequal, for the reason the node counts above are
	// unequal. asn-view counts these.
	unicast(dualCollectorA, dualSessionA, dualHomedRoutePrefix,
		dualHomedRouteMoreSpecific, "10.208.1.0/24", "10.208.2.0/24")
	unicast(dualCollectorB, dualSessionB, dualHomedRoutePrefix, "10.208.3.0/24")
	// The labeled routes, which looking-glass-vpn reads instead of
	// route_unicast: THREE for collector A and TWO for collector B, one of
	// them shared, under one RD. Unequal, for the reason the unicast counts
	// above are unequal. One RD and not two, because the collectors are two
	// vantage points on one router and have to agree about what it exports.
	// Only the shared /24 and collector A's /25 cover the $target the stat
	// tiles use -- see dualHomedVpnOtherMoreSpecific and
	// dualHomedVpnUnsharedA for why the other two are there and why they
	// must stay outside that target.
	vpn := func(collector string, sessionID uint64, prefixes ...string) {
		for _, pfx := range prefixes {
			envs = append(envs, routeEnv(collector, sessionID, &vantagev1.RouteEvent{
				Family: &vantagev1.Family{Afi: 1, Safi: 128},
				Attrs:  routeAttrs,
				VpnAnnounced: []*vantagev1.VpnPrefix{{
					Prefix: pfx, Rd: dualHomedRouteRD, Labels: []uint32{24208},
				}},
			}))
		}
	}
	vpn(dualCollectorA, dualSessionA, dualHomedVpnPrefix, dualHomedVpnMoreSpecific,
		dualHomedVpnUnsharedA)
	vpn(dualCollectorB, dualSessionB, dualHomedVpnPrefix, dualHomedVpnOtherMoreSpecific)
	// stream_seq restarts at 1, which is what every other fixture in this
	// file does. It is safe because fixtures are kept apart by RESERVED
	// ADDRESSES, not by disjoint sequence ranges: every sort key in play
	// leads with router_ip, and each argMax runs inside a GROUP BY already
	// scoped to one router. Do not invent an offset here -- it would be a
	// second convention for the same problem.
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert envelope %d: %v", i, err)
		}
	}
}

// dualHomedVars is the Grafana variable set aimed at the dual-homed router.
func dualHomedVars(focus, state string) map[string]string {
	return topologyVarsOn(dualHomedRouterIP, dualHomedPeerIP, focus, state)
}

// dashRowsAsStrings runs rows to completion and converts every column of
// every row to a string via fmt.Sprint.
//
// The driver's Scan cannot take a bare *any -- it needs a destination typed
// to match each column, which is exactly what a column-count and
// column-type agnostic caller cannot know in advance. ColumnTypes' ScanType
// supplies that type per column, so this works unchanged whether the panel
// underneath is Nodes, Links or Prefixes, each a different shape.
func dashRowsAsStrings(t *testing.T, rows driver.Rows) [][]string {
	t.Helper()
	colTypes := rows.ColumnTypes()
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(colTypes))
		for i, ct := range colTypes {
			vals[i] = reflect.New(ct.ScanType()).Interface()
		}
		if err := rows.Scan(vals...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row := make([]string, len(vals))
		for i, v := range vals {
			row[i] = fmt.Sprint(reflect.ValueOf(v).Elem().Interface())
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// dualHomedLinkStateDashboards is every link-state dashboard whose table and
// stat resolve a session. The table drives both dual-homed tests, so adding a
// sibling dashboard without adding it here leaves it untested -- which is the
// defect the coverage stat on this file already records once.
//
// Every number here is measured by running the panel against
// insertDualHomedFixture, never derived on paper, and every one of them is
// unequal between the two collectors on purpose -- equal counts would make
// "kept A", "kept B" and "merged" indistinguishable.
//
// rowsA/rowsB and keysA/keysB are a coincidence, not an identity: the tables
// render one GROUP BY row per object whose latest state is in $state, while
// the stats count
// countDistinct(node_key) / countDistinct((local_node_key, remote_node_key)) /
// countDistinct((node_key, prefix, prefix_len)) with no withdrawal filter at
// all. The fixture holds a node and a link that collector A withdraws and
// collector B keeps, so A HOLDS four node keys while its Nodes table renders
// three of them, and the two tests need separate numbers.
//
//   - rowsA/rowsB: what the TABLE renders per collector under $state = 0.
//   - keysA/keysB: what the coverage STAT counts per collector, which is also
//     what the completeness table and the topology object stat count, since
//     none of the three filters on is_withdraw.
var dualHomedLinkStateDashboards = []struct {
	dashboard    string
	table        string
	rowsA, rowsB int
	keysA, keysB int
}{
	{"link-state-nodes", "Nodes", 3, 5, 4, 5},
	{"link-state-links", "Links", 1, 3, 2, 3},
	{"link-state-prefixes", "Prefixes", 2, 4, 2, 4},
}

// lsCoverageStatTitle maps a link-state dashboard to its "Carried by the
// current session" stat panel's exact title, which varies per sibling: see
// linkStateNodesCoverageStatTitle's comment for why.
var lsCoverageStatTitle = map[string]string{
	"link-state-nodes":    linkStateNodesCoverageStatTitle,
	"link-state-links":    linkStateLinksCoverageStatTitle,
	"link-state-prefixes": linkStatePrefixesCoverageStatTitle,
}

// TestDualHomedTablesKeepBothCollectors is the behavioral guard the
// substring ratchet could not be, generalized across every link-state table
// in dualHomedLinkStateDashboards.
//
// It asserts the ANSWER, not the SQL text: insertDualHomedFixture's disjoint
// per-collector counts. A panel that resolves the session without
// collector_id returns collector A's rows and drops collector B's entirely,
// which is what this test fails on until the panel is fixed.
func TestDualHomedTablesKeepBothCollectors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	for _, tc := range dualHomedLinkStateDashboards {
		t.Run(tc.dashboard, func(t *testing.T) {
			sql := substituteGrafana(
				panelSQL(t, tc.dashboard, tc.table, "A"),
				dualHomedVars("0", "0"))
			rows, err := ch.conn.Query(ctx, qualify(ch, sql))
			if err != nil {
				t.Fatalf("%s table: %v\nSQL:\n%s", tc.table, err, sql)
			}
			defer rows.Close()

			// Column 0 is the collector once the panel is fixed; before that it
			// is whatever the panel selects first, which is why the assertion
			// below keys on the collector NAMES rather than on a row count.
			byCollector := map[string]int{}
			for _, row := range dashRowsAsStrings(t, rows) {
				byCollector[row[0]]++
			}
			if byCollector[dualCollectorA] != tc.rowsA || byCollector[dualCollectorB] != tc.rowsB {
				t.Errorf("%s rendered %v; want %s=%d and %s=%d. A result of "+
					"%s=%d alone is the bug this test exists for: max(session_id) "+
					"with no collector in it picks the larger of two unrelated "+
					"session ids and discards the other collector's entire view",
					tc.dashboard, byCollector, dualCollectorA, tc.rowsA,
					dualCollectorB, tc.rowsB, dualCollectorA, tc.rowsA)
			}
		})
	}
}

// TestDualHomedStatCountsOnlyItsOwnCollector pins the fix for a
// misreading: the stat rendered in_current_session=2, in_window=5, which an
// operator reads as "three nodes aged out". Those three were a second
// collector's LIVE view. Both halves must now be scoped to one collector, so
// each repeated tile reads n of n. Generalized across every link-state stat
// in dualHomedLinkStateDashboards.
func TestDualHomedStatCountsOnlyItsOwnCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	for _, dash := range dualHomedLinkStateDashboards {
		t.Run(dash.dashboard, func(t *testing.T) {
			title := lsCoverageStatTitle[dash.dashboard]
			for _, tc := range []struct {
				collector string
				want      uint64
			}{
				{dualCollectorA, uint64(dash.keysA)},
				{dualCollectorB, uint64(dash.keysB)},
			} {
				vars := dualHomedVars("0", "0")
				vars["collector"] = tc.collector
				sql := substituteGrafana(
					panelSQL(t, dash.dashboard, title, "A"), vars)
				var inSession, inWindow uint64
				if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&inSession, &inWindow); err != nil {
					t.Fatalf("%s: %v\nSQL:\n%s", tc.collector, err, sql)
				}
				if inSession != tc.want || inWindow != tc.want {
					t.Errorf("%s: in_current_session=%d in_window=%d; want %d and %d. "+
						"An in_window larger than in_current_session here means the "+
						"window half is still counting every collector's rows, which "+
						"renders another collector's live view as nodes that aged out",
						tc.collector, inSession, inWindow, tc.want, tc.want)
				}
			}
		})
	}
}

// dualHomedLookingGlasses is the two looking glasses, with the $target and
// variable rendering that reaches the routes insertDualHomedFixture writes
// under the dual-homed router.
//
// It is a table rather than one case because the two read DIFFERENT tables --
// route_unicast and route_vpn -- and leaving either to the debt ratchet would
// leave it to a TEXT predicate. The joined-comparison shape's defect is
// invisible to a text predicate by construction: the join is present either
// way, and only the contents of the ON clause differ.
//
// $target is rendered as the prefix itself rather than as an address inside
// it. That is a supported input -- the panel reduces a typed prefix to its
// network address, so a prefix finds itself (TestLookingGlassSurvivesEveryInput
// pins that) -- and it keeps both cases the same shape. $rib is "in_pre", the
// stream ribName files a route with neither the adj-RIB-out nor the
// post-policy flag into, so the rib filter is exercised against a real value
// instead of against its allValue.
var dualHomedLookingGlasses = []struct {
	dashboard string
	source    string
	// mostSpecific is the prefix length each collector's repeated tile must
	// report for its OWN routes. The two DIFFER, and that is what makes the
	// tile's "AND collector_id = '$collector'" falsifiable: collector A
	// announces a /25 under this target as well as the /24 both announce, so
	// a tile that aggregated every collector's rows would report 25 in both
	// copies. It doubles as the vacuity guard -- max() over no rows is 0, and
	// a tile that counted nothing would otherwise satisfy the
	// superseded-is-zero assertion below for free.
	mostSpecific map[string]uint64
	vars         map[string]string
}{
	{"looking-glass", "route_unicast",
		map[string]uint64{dualCollectorA: 25, dualCollectorB: 24},
		map[string]string{"target": dualHomedRoutePrefix, "rib": "in_pre"}},
	{"looking-glass-vpn", "route_vpn",
		map[string]uint64{dualCollectorA: 25, dualCollectorB: 24},
		map[string]string{"target": dualHomedVpnPrefix, "rib": "in_pre", "family": "vpn4"}},
}

// dualHomedLookingGlassVars copies one case's variable rendering and, when
// collector is non-empty, adds $collector to the copy -- so a test that
// renders a repeated tile cannot mutate the shared table above.
func dualHomedLookingGlassVars(base map[string]string, collector string) map[string]string {
	out := make(map[string]string, len(base)+1)
	maps.Copy(out, base)
	if collector != "" {
		out["collector"] = collector
	}
	return out
}

// scopeToDualHomedRouter narrows a looking-glass query to the dual-homed
// router by extending the panel's own time filter, the way
// scopeToFixtureRouters and scopeToVpnFixtureRouter do for theirs.
//
// Neither looking glass has a $router control, by design, so every route in
// the shared testDB competes to cover a target. The dual-homed collector ids
// are their own ("dual-c1"/"dual-c2", not the "c1" every single-collector
// fixture writes), so a collision on the id alone is no longer what this
// helper prevents -- but the scope is still load-bearing: without it a
// less-specific route arriving under some other router in a future fixture
// would decide these panels' answers, whatever collector wrote it.
//
// Exactly one occurrence of the anchor, or nothing: a rewrite that moved it
// must fail loudly here rather than quietly stop scoping.
func scopeToDualHomedRouter(t *testing.T, sql string) string {
	t.Helper()
	return scopeToRouter(t, sql, dualHomedRouterIP)
}

// scopeToRouter narrows a panel to one router by hanging a predicate off the
// single time filter every one of these panels opens its innermost scan with.
func scopeToRouter(t *testing.T, sql, routerIP string) string {
	t.Helper()
	const anchor = "WHERE $__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("expected exactly 1 %q to anchor the dual-homed router scope, "+
			"found %d; the panel was rewritten and this helper must be updated "+
			"with it", anchor, n)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip = toIPv6('"+routerIP+"')", 1)
}

// TestDualHomedLookingGlassDoesNotCallLiveRoutesSuperseded pins the failure
// of panels that join the current session and display the comparison rather
// than filtering on it: it is a LABEL and not a missing row.
//
// These panels never filter by session. They LEFT JOIN a current session and
// DISPLAY the comparison as session_current, so no row-count assertion can
// fail on them however wrong the join is. What breaks is what the column
// says: with one joined row per router carrying max(session_id) across both
// collectors, every one of the smaller-id collector's live routes compares
// unequal and renders as superseded -- an operator sees a route the network
// is really carrying, marked stale, and the honest reading of that screen is
// that the route is gone.
//
// Both collectors' routes here are current, so every row must read 1. The
// other side of this column -- a row that must read 0 -- is not this
// fixture's to supply: it needs a second, superseded session under one
// collector, which would break dualHomedRequireTwoCollectorsOneSessionEach.
// TestLookingGlassFlagsObservationsFromSupersededSessions and
// TestVpnLookingGlassHoldsStateStalenessAndBadInput hold that side, on the
// single-collector fixtures built for it.
func TestDualHomedLookingGlassDoesNotCallLiveRoutesSuperseded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	for _, tc := range dualHomedLookingGlasses {
		t.Run(tc.dashboard, func(t *testing.T) {
			sql := substituteGrafana(
				scopeToDualHomedRouter(t, panelSQL(t, tc.dashboard, "Covering routes", "A")),
				dualHomedLookingGlassVars(tc.vars, ""))
			rows, err := ch.conn.Query(ctx, qualify(ch, sql))
			if err != nil {
				t.Fatalf("%s covering routes: %v\nSQL:\n%s", tc.dashboard, err, sql)
			}
			defer rows.Close()
			// The two panels are different widths, so the collector is read as
			// the FIRST column and session_current as the LAST. The names are
			// checked first, so a column added at either end fails here rather
			// than silently moving what the assertion reads.
			cols := rows.Columns()
			if len(cols) < 2 || cols[0] != "collector" || cols[len(cols)-1] != "session_current" {
				t.Fatalf("%s Covering routes returns columns %v; this test reads "+
					"the first as the collector and the last as session_current",
					tc.dashboard, cols)
			}
			states := map[string][]string{}
			for _, row := range dashRowsAsStrings(t, rows) {
				states[row[0]] = append(states[row[0]], row[len(row)-1])
			}
			for _, c := range []string{dualCollectorA, dualCollectorB} {
				got := states[c]
				if len(got) == 0 {
					t.Errorf("%s rendered no row for collector %s; it rendered %v. "+
						"Both collectors advertise %s out of %s, and both rows have "+
						"to be on screen before a label on them can mean anything",
						tc.dashboard, c, states, tc.vars["target"], tc.source)
					continue
				}
				for _, v := range got {
					if v != "1" {
						t.Errorf("%s: collector %s's %s row reads session_current=%q, "+
							"want \"1\". Both collectors' sessions are live. Joining "+
							"the current session on router_ip alone gives every row the "+
							"MAX session id across both collectors, so the collector "+
							"holding the smaller one has its entire live view marked "+
							"stale", tc.dashboard, c, tc.source, v)
						break
					}
				}
			}
		})
	}
}

// TestDualHomedLookingGlassStatsCountNoLiveRouteAsSuperseded is the stat-side
// face of the same defect, and the number an operator actually reads.
//
// "From a superseded session" is countIf(m.session_id != s.sid). Joined on
// router_ip alone, s.sid is the max across both collectors, so every one of
// the smaller-id collector's live rows counts -- the tile reports routes as
// resting on a dead session while the router is carrying them.
//
// The first column is deliberately not asserted: it is routers_advertising on
// one dashboard and vrfs_carrying_it on the other, which are two different
// questions, and on this fixture both read 1 for either collector. Two things
// are: most_specific, which differs between the two collectors and so pins
// that each tile counts its own collector alone, and from_superseded_session,
// which is the column the router-only join corrupts.
func TestDualHomedLookingGlassStatsCountNoLiveRouteAsSuperseded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	for _, tc := range dualHomedLookingGlasses {
		t.Run(tc.dashboard, func(t *testing.T) {
			for _, collector := range []string{dualCollectorA, dualCollectorB} {
				sql := substituteGrafana(
					scopeToDualHomedRouter(t, panelSQL(t, tc.dashboard, lgStatTitle[tc.dashboard], "A")),
					dualHomedLookingGlassVars(tc.vars, collector))
				var headline, mostSpecific, superseded uint64
				if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).
					Scan(&headline, &mostSpecific, &superseded); err != nil {
					t.Fatalf("%s stat for %s: %v\nSQL:\n%s", tc.dashboard, collector, err, sql)
				}
				other := dualCollectorA
				if collector == dualCollectorA {
					other = dualCollectorB
				}
				if want := tc.mostSpecific[collector]; mostSpecific != want {
					// Two OPPOSITE defects land here and they have opposite
					// fixes, so the diagnosis is chosen by what came back
					// rather than fixed in the format string. Merging can only
					// RAISE this figure -- it is a max() over a superset -- so
					// "the tile counted the other collector's rows too" is
					// available only when the number is too HIGH. Too low is
					// the reverse: the tile missed its OWN more specific
					// route, which is the $collector filter excluding rows it
					// should be counting. Printing the merged reading for both
					// told an operator to go looking for the wrong bug on one
					// of the two tiles.
					hint := fmt.Sprintf("that is too LOW, and %s's own answer "+
						"is %d: the tile missed THIS collector's more specific "+
						"route rather than borrowing the other's", other,
						tc.mostSpecific[other])
					if mostSpecific > want {
						hint = fmt.Sprintf("that is too HIGH, and %s's answer "+
							"is %d: the tile counted the OTHER collector's rows "+
							"too, which is the repeat rendering one merged "+
							"answer twice under two different headings", other,
							tc.mostSpecific[other])
					}
					t.Errorf("%s tile for %s reports most_specific=%d, want %d -- "+
						"%s. 0 would mean the tile counted none of that "+
						"collector's routes at all, in which case the superseded "+
						"figure below reads zero for free",
						tc.dashboard, collector, mostSpecific, want, hint)
					continue
				}
				if superseded != 0 {
					t.Errorf("%s tile for %s reports from_superseded_session=%d, "+
						"want 0. Every route that collector holds under %s rests on "+
						"its own current session; a non-zero here is the router-only "+
						"join comparing this collector's rows against the OTHER "+
						"collector's session id", tc.dashboard, collector, superseded,
						dualHomedSysname)
				}
			}
		})
	}
}

// lgTableRow is one row of a looking glass's Covering routes table, reduced
// to the columns its ORDER BY is written in terms of.
type lgTableRow struct {
	collector string
	prefix    string
	// length is the IPv4 prefix length, which is what the panel sorts on:
	// prefix_len is coalesce(toUInt8OrNull(splitByChar('/', prefix)[2]), 0),
	// NOT the v6-mapped len6 the containment maths uses. Parsing the string
	// back out of the rendered prefix column reproduces it exactly.
	length int
}

// collectorRuns counts the CONTIGUOUS blocks of one collector in a row order.
// It equals the number of distinct collectors exactly when every collector's
// rows arrive together, and exceeds it the moment two collectors interleave.
func collectorRuns(rows []lgTableRow) int {
	runs := 0
	for i, r := range rows {
		if i == 0 || rows[i-1].collector != r.collector {
			runs++
		}
	}
	return runs
}

// TestDualHomedLookingGlassGroupsEachCollectorsRowsTogether pins the leading
// "collector" in both looking glasses' ORDER BY, which was deletable with the
// whole suite green.
//
// Nothing else could see it. TestDualHomedLookingGlassDoesNotCallLiveRoutesSuperseded
// accumulates the rows into a map keyed by collector, so it is order-blind by
// construction, and the most-specific-first assertions run on the
// single-collector looking-glass fixtures where the clause is a no-op. What
// the clause buys an operator is that a dual-homed router's two views arrive
// as two blocks rather than shuffled together by prefix length -- with a
// collector column and no grouping, telling one collector's answer from the
// other's is a reading exercise performed row by row.
//
// $target is rendered as the picker's All rather than as one prefix. The
// containment filter narrows to the routes covering ONE address, and on this
// fixture that is a handful of rows whose only inter-collector difference is
// a TIE on the remaining sort key -- and a tie cannot falsify a sort. All
// widens it to every route the dual-homed router carries, where each
// collector holds a row that outranks one of the other's. The guard below
// checks that rather than trusting it.
func TestDualHomedLookingGlassGroupsEachCollectorsRowsTogether(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	for _, tc := range dualHomedLookingGlasses {
		t.Run(tc.dashboard, func(t *testing.T) {
			vars := dualHomedLookingGlassVars(tc.vars, "")
			vars["target"] = ""
			sql := substituteGrafana(
				scopeToDualHomedRouter(t, panelSQL(t, tc.dashboard, "Covering routes", "A")),
				vars)
			rows, err := ch.conn.Query(ctx, qualify(ch, sql))
			if err != nil {
				t.Fatalf("%s covering routes: %v\nSQL:\n%s", tc.dashboard, err, sql)
			}
			defer rows.Close()
			cols := rows.Columns()
			prefixCol := slices.Index(cols, "prefix")
			if len(cols) == 0 || cols[0] != "collector" || prefixCol < 0 {
				t.Fatalf("%s Covering routes returns columns %v; this test reads "+
					"the first as the collector and needs a prefix column",
					tc.dashboard, cols)
			}
			var got []lgTableRow
			for _, row := range dashRowsAsStrings(t, rows) {
				pfx := row[prefixCol]
				n, err := strconv.Atoi(pfx[strings.LastIndex(pfx, "/")+1:])
				if err != nil {
					t.Fatalf("%s rendered prefix %q with no length; the panel "+
						"sorts on one", tc.dashboard, pfx)
				}
				got = append(got, lgTableRow{collector: row[0], prefix: pfx, length: n})
			}

			collectors := map[string]bool{}
			for _, r := range got {
				collectors[r.collector] = true
			}
			if len(collectors) < 2 {
				t.Fatalf("%s returned rows for %d collector(s) (%d rows), so "+
					"there is no grouping to assert", tc.dashboard,
					len(collectors), len(got))
			}

			// The falsifiability guard, and the reason this test can be
			// trusted: re-sort the rows the way the panel would WITHOUT the
			// leading collector -- prefix_len descending, then prefix -- with
			// a STABLE sort, so every tie keeps the collector-grouped order it
			// arrived in. That is the friendliest possible tie-break for the
			// mutation. If even that order is still collector-grouped, then
			// deleting "collector, " from the ORDER BY changes nothing this
			// fixture can show and the assertion below is decoration.
			//
			// router, peer and rd are omitted from the key because the scope
			// is one router, one peer and one RD, so they cannot order
			// anything here.
			shuffled := slices.Clone(got)
			sort.SliceStable(shuffled, func(i, j int) bool {
				if shuffled[i].length != shuffled[j].length {
					return shuffled[i].length > shuffled[j].length
				}
				return shuffled[i].prefix < shuffled[j].prefix
			})
			if collectorRuns(shuffled) == len(collectors) {
				t.Fatalf("%s: sorting this fixture's rows by prefix length "+
					"alone still leaves each collector's rows contiguous, so "+
					"the leading \"collector\" in the panel's ORDER BY could be "+
					"deleted and this test would not notice. The fixture needs "+
					"a row from one collector that outranks a row from the "+
					"other on prefix length -- dualHomedVpnOtherMoreSpecific "+
					"and dualHomedVpnUnsharedA each supply one, from opposite "+
					"directions. Rows: %v", tc.dashboard, shuffled)
			}

			if runs := collectorRuns(got); runs != len(collectors) {
				t.Errorf("%s returned %d collector rows in %d blocks across %d "+
					"collectors, want one block each: %v. The panel orders by "+
					"collector first so a dual-homed router's two views arrive "+
					"as two blocks; without it they interleave by prefix length "+
					"and an operator has to separate them by eye, row by row",
					tc.dashboard, len(got), runs, len(collectors), got)
			}
		})
	}
}

// dualHomedASNPrefixRows is what asn-view's Prefixes table must render per
// collector for the dual-homed router, filtered to dualHomedOriginASN.
//
// Measured against insertDualHomedFixture, not derived: collector A announces
// four prefixes under that origin and collector B announces two, one of which
// (dualHomedRoutePrefix) they BOTH announce. Unequal on purpose -- 4-and-2
// tells "kept A", "kept B" and "merged" apart, where 3-and-3 would not.
//
// A merged answer is five rows, because the shared prefix collapses: with an
// inner GROUP BY key of (router, sysname, peer, rib, prefix, path_id) and no
// collector in it, the two collectors' sightings of dualHomedRoutePrefix are
// one group whose argMax resolves to whichever was written last. The shipped
// SQL has collector_id in that key and does not merge them. That collapse is
// why this panel is countable at all, and it is the only thing separating
// 4+2 from a table that looks plausible.
var dualHomedASNPrefixRows = map[string]int{dualCollectorA: 4, dualCollectorB: 2}

// TestDualHomedASNViewKeepsBothCollectorsPrefixes converts the last panel
// that joins and displays session_current rather than filtering on it, and
// it fails in BOTH of that shape's ways at once.
//
// The label: the panel never filters by session. It LEFT JOINs a current
// session and displays the comparison as session_current, so with the join
// keyed on router_ip alone every row carries max(session_id) across both
// collectors and collector B's whole live view renders as superseded. An
// operator reading that screen concludes the routes are gone.
//
// The row: unlike the looking glasses, this panel's inner GROUP BY also
// decides how many rows there are. Without collector_id in it, the one prefix
// both collectors announce is a single group -- so the table shows five rows
// where the router really has six sightings, and the surviving one is
// attributed to one arbitrary collector.
//
// Every returned row is checked, not one per collector. The panel fans a
// route into one row per ASN in its as_path, so a map keyed on the collector
// keeps only the last row for each and cannot see a mislabeled sibling.
//
// The other side of session_current -- a row that must read 0 -- is not this
// fixture's to supply: it needs a second, superseded session under one
// collector, which dualHomedRequireTwoCollectorsOneSessionEach forbids.
// TestASNViewPrefixTableFlagsASupersededSessionRow holds that side on the ASN
// view's own single-collector fixture.
func TestDualHomedASNViewKeepsBothCollectorsPrefixes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	got := dualHomedASNPrefixes(t, ctx, ch, fmt.Sprint(dualHomedOriginASN))
	byCollector := map[string]int{}
	for _, r := range got {
		byCollector[r.collector]++
	}
	if byCollector[dualCollectorA] != dualHomedASNPrefixRows[dualCollectorA] ||
		byCollector[dualCollectorB] != dualHomedASNPrefixRows[dualCollectorB] {
		t.Errorf("asn-view Prefixes for AS %d on %s rendered %v, want %v. Five "+
			"rows, one collector short by one, is the merge: with no collector "+
			"in the inner GROUP BY, %s -- which both collectors announce -- is "+
			"one group attributed to whichever sighting was written last. "+
			"Doubled counts are the other direction: a session subquery keyed "+
			"on the pair, joined back on router_ip alone, matches both of its "+
			"rows", dualHomedOriginASN, dualHomedSysname, byCollector,
			dualHomedASNPrefixRows, dualHomedRoutePrefix)
	}
	for _, r := range got {
		if r.sessionCurrent != 1 {
			t.Errorf("asn-view Prefixes marks %s from collector %s "+
				"session_current=%d, want 1. Both collectors' sessions are live "+
				"and every one of these routes was heard in its own collector's "+
				"session. Joining the current session on router_ip alone gives "+
				"every row the MAX session id across both collectors, so the "+
				"collector holding the smaller one has its entire live view "+
				"marked stale", r.prefix, r.collector, r.sessionCurrent)
		}
	}
}

// dualHomedTableRows runs one dual-homed link-state table panel and returns
// its rows as strings, so an assertion can read a COLUMN rather than only
// count rows.
func dualHomedTableRows(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, table string) [][]string {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, dashboard, table, "A"),
		dualHomedVars("0", "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("%s table: %v\nSQL:\n%s", table, err, sql)
	}
	defer rows.Close()
	return dashRowsAsStrings(t, rows)
}

// TestDualHomedPrefixOriginComesFromItsOwnCollector pins the collector key on
// the Prefixes panel's nodeset join.
//
// A dual-homed router has one current session PER collector, so the CTE that
// resolves a prefix's origin name now spans both collectors' sessions. Join it
// on node_key alone and a row attributed to c2 is labeled from whichever
// collector's sighting argMax found freshest -- so when c2 has re-dumped its
// prefixes but its node NLRI has not arrived yet, c2's row borrows c1's name
// and reads as a complete view, while the Nodes table on the sibling dashboard
// correctly shows c2 has no such node.
//
// A dual-homed router renders as two rows, one per collector, never
// merged: a row attributed to one collector is built from
// that collector's view and nothing else. The honest answer when the collector
// has not heard the node is the router_id hex decode.
//
// This is the only DUAL-HOMED assertion that reads the origin COLUMN -- every
// other one counts rows, and the row counts are identical under both joins.
// TestPrefixesTableJoinsPrefixToItsOriginatingNode reads the column too, on
// the single-collector fixture, where there is only one collector's name to
// resolve and so no choice for a join to get wrong.
func TestDualHomedPrefixOriginComesFromItsOwnCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	// Columns are collector, prefix, origin, ... -- see prefixesTableFor.
	origins := map[string][]string{}
	for _, row := range dualHomedTableRows(t, ctx, ch, "link-state-prefixes", "Prefixes") {
		if row[1] == dualBorrowedOriginPfx {
			origins[row[0]] = append(origins[row[0]], row[2])
		}
	}
	for _, tc := range []struct {
		collector string
		want      string
		why       string
	}{
		{dualCollectorA, dualBorrowedOriginName,
			"collector A advertised this prefix's originating node, so its row " +
				"must carry the node's name -- a fallback here means the join " +
				"stopped resolving names at all, which would make the c2 case " +
				"below pass for the wrong reason"},
		{dualCollectorB, dualBorrowedOriginFallback,
			"collector B never advertised this prefix's originating node. Seeing " +
				dualBorrowedOriginName + " means the nodeset join lost its " +
				"collector key and B's row borrowed A's name for a node B has " +
				"never heard of"},
	} {
		got := origins[tc.collector]
		if len(got) != 1 {
			t.Errorf("%s rendered %d rows for %s (%v); want exactly 1. More than "+
				"one means the nodeset join matched both collectors' sightings of "+
				"the same node_key and duplicated the prefix row",
				tc.collector, len(got), dualBorrowedOriginPfx, got)
			continue
		}
		if got[0] != tc.want {
			t.Errorf("%s origin for %s = %q; want %q. %s",
				tc.collector, dualBorrowedOriginPfx, got[0], tc.want, tc.why)
		}
	}
}

// TestDualHomedSharedNodeKeepsEachCollectorsName guards the other half of the
// fixture extension above: the one node BOTH collectors advertise, which they
// name differently.
//
// Nothing else asserts those names, and without them the shared node degrades
// into a silent duplicate: a future change to link-state-topology's merged
// node graph depends on this node id staying a duplicate, and a merge that
// quietly kept one collector's label would still satisfy every row count in
// this file.
func TestDualHomedSharedNodeKeepsEachCollectorsName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	// Columns are collector, node, router_id, ... -- see the Nodes panel.
	names := map[string]string{}
	for _, row := range dualHomedTableRows(t, ctx, ch, "link-state-nodes", "Nodes") {
		if row[1] == dualSharedNodeNameA || row[1] == dualSharedNodeNameB {
			names[row[0]] = row[1]
		}
	}
	want := map[string]string{
		dualCollectorA: dualSharedNodeNameA,
		dualCollectorB: dualSharedNodeNameB,
	}
	if !maps.Equal(names, want) {
		t.Errorf("the shared node rendered %v; want %v. Both collectors "+
			"advertise this node under one node_key and two names, so each "+
			"collector's row must carry its own -- one name for both, or one "+
			"row where there should be two, is a merge",
			names, want)
	}
}

// dualHomedObjectCounts is what one collector's current session holds in the
// dual-homed fixture, per unit: nodes and edges.
//
// The numbers are not restated here. They are the keysA/keysB that
// dualHomedLinkStateDashboards already carries, measured through the Nodes
// and Links panels, and the units match exactly rather than by luck: the
// topology stat counts countDistinct(node_key) and
// countDistinct((local_node_key, remote_node_key)), which is what the
// coverage stat on each of those two dashboards counts.
//
// keys, NOT rows. Neither caller filters on is_withdraw -- the completeness
// table's countDistinctIf and the topology object stat both count every key
// the session carries -- so an object one collector withdrew is still one of
// these, while the sibling TABLE no longer renders it.
//
// The units matching is one of TWO conditions, and the second is not visible
// from the unit names: the completeness table is peer-BLIND where the
// coverage stats are scoped to $peer, so the two only agree while the
// dual-homed fixture writes exactly one peer under this router. Give it a
// second peer and the completeness row grows while the stats do not.
func dualHomedObjectCounts(t *testing.T, unit string) (a, b int) {
	t.Helper()
	dashboard, known := map[string]string{
		"nodes": "link-state-nodes",
		"edges": "link-state-links",
	}[unit]
	if !known {
		t.Fatalf("no per-collector count is measured for %q", unit)
	}
	for _, d := range dualHomedLinkStateDashboards {
		if d.dashboard == dashboard {
			return d.keysA, d.keysB
		}
	}
	t.Fatalf("dualHomedLinkStateDashboards no longer carries %s, so the "+
		"per-collector %s counts have nowhere to come from", dashboard, unit)
	return 0, 0
}

// dualHomedGraphNodeCount derives, from the rows insertDualHomedFixture
// actually wrote, how many distinct nodes the merged topology graph has to
// draw: every node_key either collector advertised, plus every link endpoint
// either collector referenced, each counted once.
//
// Derived rather than written down, because the number has already moved once
// -- it was 5 until a node both collectors advertise was added -- and a
// number written down separately cannot notice the fixture changing under it.
//
// It deliberately resolves no session. The fixture writes exactly one session
// per collector (guarded by dualHomedRequireTwoCollectorsOneSessionEach), so
// "every row this router has" and "both collectors' current sessions" are the
// same set, and a derivation that resolved the session the way the panel does
// could share the panel's bug.
func dualHomedGraphNodeCount(t *testing.T, ctx context.Context, ch *ClickHouse) int {
	t.Helper()
	dualHomedRequireTwoCollectorsOneSessionEach(t, ctx, ch)
	dualHomedRequireSomeCollectorStillHolds(t, ctx, ch, "ls_nodes", "node_key")
	// The derivation below unions both ls_links endpoint columns as well, so
	// the guard has to cover that table too. A link withdrawn on EVERY
	// collector whose far end is not an advertised node -- node 9's exact
	// shape, which this fixture introduced -- would add an endpoint to this
	// count while the panel correctly drops it, and the failure would read as
	// a panel bug. That is verbatim what the guard's own comment says it
	// prevents; dualHomedGraphEdgeCount already makes this call.
	dualHomedRequireSomeCollectorStillHolds(t, ctx, ch, "ls_links",
		"(local_node_key, remote_node_key)")
	var nodes uint64
	q := fmt.Sprintf(`SELECT countDistinct(k) FROM (
    SELECT node_key AS k FROM vantage.ls_nodes
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s')
    UNION DISTINCT
    SELECT local_node_key AS k FROM vantage.ls_links
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s')
    UNION DISTINCT
    SELECT remote_node_key AS k FROM vantage.ls_links
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s'))`,
		dualHomedRouterIP, dualHomedPeerIP)
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&nodes); err != nil {
		t.Fatalf("dual-homed node keys: %v", err)
	}
	// Both collectors' contributions have to be in there, or the graph could
	// draw one collector's view and still match.
	aNodes, bNodes := dualHomedObjectCounts(t, "nodes")
	if int(nodes) <= aNodes || int(nodes) <= bNodes {
		t.Fatalf("the fixture's distinct node keys (%d) do not exceed either "+
			"collector's own count (%d and %d), so a graph that kept one "+
			"collector would satisfy this count", nodes, aNodes, bNodes)
	}
	return int(nodes)
}

// dualHomedGraphEdgeCount is dualHomedGraphNodeCount's other half: how many
// distinct edges the merged topology graph has to draw, derived from the rows
// insertDualHomedFixture wrote rather than written down.
//
// An edge is one (local_node_key, remote_node_key) pair. Both collectors'
// pairs, counted once, is the answer -- the same three-way distinction the
// node count makes, since "kept A", "kept B" and "merged" are three different
// numbers here too.
func dualHomedGraphEdgeCount(t *testing.T, ctx context.Context, ch *ClickHouse) int {
	t.Helper()
	dualHomedRequireTwoCollectorsOneSessionEach(t, ctx, ch)
	dualHomedRequireSomeCollectorStillHolds(t, ctx, ch, "ls_links",
		"(local_node_key, remote_node_key)")
	var edges uint64
	q := fmt.Sprintf(`SELECT countDistinct((local_node_key, remote_node_key))
    FROM vantage.ls_links
    WHERE router_ip = toIPv6('%s') AND peer_ip = toIPv6('%s')`,
		dualHomedRouterIP, dualHomedPeerIP)
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&edges); err != nil {
		t.Fatalf("dual-homed link pairs: %v", err)
	}
	aEdges, bEdges := dualHomedObjectCounts(t, "edges")
	if int(edges) <= aEdges || int(edges) <= bEdges {
		t.Fatalf("the fixture's distinct link pairs (%d) do not exceed either "+
			"collector's own count (%d and %d), so a graph that kept one "+
			"collector would satisfy this count", edges, aEdges, bEdges)
	}
	return int(edges)
}

// dualHomedFocusNodeCount derives how many options the $focus picker has to
// offer on the dual-homed router: every node_key at least one collector still
// advertises in its own current session, counted once.
//
// Derived rather than written down, for the reason dualHomedGraphNodeCount
// gives -- it was 5 until the fixture gained more nodes -- and
// derived FROM the graph's count rather than beside it, because the
// difference between the two is itself an assertion. The graph also draws
// link endpoints nobody advertised; the picker reads ls_nodes only, and
// offering an endpoint-only key would hand an operator an option no panel on
// any of these four dashboards can render. So the picker's answer is exactly
// the graph's minus the endpoint-only keys, which is a stronger statement
// than matching either number alone.
func dualHomedFocusNodeCount(t *testing.T, ctx context.Context, ch *ClickHouse) int {
	t.Helper()
	// This call carries both vacuity guards, including the one this
	// derivation leans on hardest: no node_key is withdrawn on EVERY
	// collector, so "every key ls_nodes holds" and "every key some collector
	// still advertises" are the same set, and neither this nor the graph
	// count has to filter on is_withdraw the way the panel does.
	graph := dualHomedGraphNodeCount(t, ctx, ch)
	var endpointOnly uint64
	q := fmt.Sprintf(`SELECT countDistinct(k) FROM (
    SELECT local_node_key AS k FROM vantage.ls_links
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s')
    UNION DISTINCT
    SELECT remote_node_key AS k FROM vantage.ls_links
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s'))
WHERE k NOT IN (
    SELECT node_key FROM vantage.ls_nodes
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s'))`,
		dualHomedRouterIP, dualHomedPeerIP)
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&endpointOnly); err != nil {
		t.Fatalf("dual-homed endpoint-only keys: %v", err)
	}
	if endpointOnly == 0 {
		t.Fatalf("every link endpoint in the dual-homed fixture is also an "+
			"advertised node, so the picker's answer and the graph's %d are "+
			"the same number and subtracting one from the other asserts "+
			"nothing about the picker reading ls_nodes only", graph)
	}
	want := graph - int(endpointOnly)
	// The same three-way distinction dualHomedGraphNodeCount insists on:
	// merged has to be a different number from either collector's own, or a
	// picker that kept one collector would satisfy it.
	//
	// rowsA/rowsB, NOT the keysA/keysB dualHomedObjectCounts returns. The
	// picker filters on $state, so a node one collector withdrew is not one
	// of that collector's options -- which is exactly the distinction the
	// Nodes table draws and the coverage stat does not.
	var rowsA, rowsB int
	for _, d := range dualHomedLinkStateDashboards {
		if d.dashboard == "link-state-nodes" {
			rowsA, rowsB = d.rowsA, d.rowsB
		}
	}
	if rowsA == 0 || rowsB == 0 {
		t.Fatal("dualHomedLinkStateDashboards no longer carries " +
			"link-state-nodes, so the per-collector node counts this " +
			"derivation checks itself against have nowhere to come from")
	}
	if want <= rowsA || want <= rowsB {
		t.Fatalf("the picker's expected option count (%d) does not exceed "+
			"either collector's own advertised node count (%d and %d), so a "+
			"picker that offered one collector's nodes would satisfy it",
			want, rowsA, rowsB)
	}
	return want
}

// dualHomedContestedNodeKeys resolves, from the rows insertDualHomedFixture
// actually wrote, every node_key BOTH collectors advertise under DIFFERENT
// names, with the names each one carries.
//
// Those are the only keys on which a label CHOICE is decidable. Where one
// collector advertises a node there is one name to render, so two panels can
// hold different merge rules and still print the same string; here they
// cannot. Derived rather than named so the check widens on its own as the
// fixture gains contested nodes -- it already holds one more than the single
// node dualSharedNodeLast was added for.
func dualHomedContestedNodeKeys(t *testing.T, ctx context.Context, ch *ClickHouse) map[string][]string {
	t.Helper()
	q := fmt.Sprintf(`SELECT groupUniqArray((toString(node_key), name))
    FROM vantage.ls_nodes
    WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s')
      AND node_key IN (
        SELECT node_key FROM vantage.ls_nodes
        WHERE router_ip = toIPv6('%[1]s') AND peer_ip = toIPv6('%[2]s')
        GROUP BY node_key
        HAVING countDistinct(collector_id) > 1 AND countDistinct(name) > 1)`,
		dualHomedRouterIP, dualHomedPeerIP)
	var pairs [][]string
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&pairs); err != nil {
		t.Fatalf("dual-homed contested node keys: %v", err)
	}
	out := map[string][]string{}
	for _, p := range pairs {
		out[p[0]] = append(out[p[0]], p[1])
	}
	for k := range out {
		sort.Strings(out[k])
	}
	if len(out) == 0 {
		t.Fatal("no node_key in the dual-homed fixture is advertised by both " +
			"collectors under two different names, so no panel has a label " +
			"choice to make and an agreement assertion between two of them " +
			"cannot fail")
	}
	return out
}

// dualHomedRequireTwoCollectorsOneSessionEach is the vacuity guard both
// derivations above depend on: they count every row the router has, which is
// the same set as "both collectors' current sessions" only while there are
// two collectors holding one session apiece.
func dualHomedRequireTwoCollectorsOneSessionEach(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	var sessions, collectors uint64
	q := fmt.Sprintf("SELECT countDistinct(session_id), countDistinct(collector_id) "+
		"FROM vantage.peer_events WHERE router_ip = toIPv6('%s')", dualHomedRouterIP)
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&sessions, &collectors); err != nil {
		t.Fatalf("dual-homed sessions: %v", err)
	}
	if collectors != 2 {
		t.Fatalf("the dual-homed fixture holds %d collectors, want 2 -- with "+
			"one collector every assertion about merging two is vacuous", collectors)
	}
	if sessions != collectors {
		t.Fatalf("the dual-homed fixture holds %d sessions across %d "+
			"collectors; this derivation counts every row the router has and "+
			"is only equal to the current sessions while there is exactly one "+
			"session per collector", sessions, collectors)
	}
}

// dualHomedRequireSomeCollectorStillHolds is the second vacuity guard: the two
// derivations count every key the fixture wrote, withdrawn or not, so they are
// the graph's answer only while no key has been withdrawn by EVERY collector.
//
// That holds today -- collector B keeps both objects collector A withdraws --
// and the day it stops holding, the derivation starts over-counting while the
// panel, correctly, does not, and the failure would read as a panel bug.
//
// It is written out here rather than lifted from the panel. It shares the
// rule that is_withdraw is evaluated inside one collector's own session, but
// none of the panel's text, and it only GUARDS: the expectation itself is a
// plain countDistinct over keys, which no version of the panel's bug can
// reach.
func dualHomedRequireSomeCollectorStillHolds(t *testing.T, ctx context.Context, ch *ClickHouse, table, key string) {
	t.Helper()
	q := fmt.Sprintf(`SELECT count() FROM (
    SELECT k, min(wd) AS best FROM (
        SELECT collector_id, %[1]s AS k,
               argMax(is_withdraw, (ts_collector, stream_seq)) AS wd
        FROM vantage.%[2]s
        WHERE router_ip = toIPv6('%[3]s') AND peer_ip = toIPv6('%[4]s')
        GROUP BY collector_id, k)
    GROUP BY k)
WHERE best != 0`, key, table, dualHomedRouterIP, dualHomedPeerIP)
	var gone uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&gone); err != nil {
		t.Fatalf("dual-homed %s withdrawal check: %v", table, err)
	}
	if gone != 0 {
		t.Fatalf("%d %s keys in the dual-homed fixture are withdrawn on EVERY "+
			"collector, so counting every key the fixture wrote is no longer "+
			"what the graph must draw -- give the derivation the withdrawal "+
			"filter, or put the object back in one collector's view", gone, table)
	}
}

// TestDualHomedTopologyMergesButLosesNothing pins that the graph stays
// single, and both collectors' current sessions feed it.
//
// The node ids must be UNIQUE -- node_key carries no collector_id, so a graph
// that grouped by collector would emit each shared node twice under the same
// id and Grafana would render one of them arbitrarily.
//
// The two title assertions at the end pin what merging TWO views of one node
// is allowed to do to its label:
//
//   - a node BOTH collectors currently advertise renders the lexically-first
//     collector's name. Any choice is arbitrary, but it has to be the same
//     one on every refresh, and the shipped argMax over (ts_collector,
//     stream_seq) was not: across two collectors those columns are two clocks
//     and two independent counters (deploy/clickhouse/schema.sql:44 and :62),
//     so the rendered label moved with the skew between two pods.
//   - a node one collector withdrew and the other still advertises renders
//     the surviving collector's name, which means the state filter has to run
//     BEFORE the label is chosen, not after.
func TestDualHomedTopologyMergesButLosesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	wantNodes := dualHomedGraphNodeCount(t, ctx, ch)
	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", "Topology", "nodes"),
		dualHomedVars("0", "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("topology nodes: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	// Columns are id, title, subtitle, mainstat, secondarystat, and two
	// detail__ fields.
	seen := map[string]int{}
	drawn := map[string]bool{}
	mainstat := map[string]string{}
	for _, row := range dashRowsAsStrings(t, rows) {
		seen[row[0]]++       // the node graph's first column is the node id
		drawn[row[1]] = true // ... and the second is its rendered title
		mainstat[row[1]] = row[3]
	}
	if len(seen) != wantNodes {
		t.Errorf("topology drew %d distinct nodes; want %d -- %s's and %s's "+
			"together, merged, with the shared node counted once. Fewer means "+
			"max(session_id) is still picking one collector's session, or that "+
			"one collector's withdrawal is erasing an object the other "+
			"collector still advertises",
			len(seen), wantNodes, dualCollectorA, dualCollectorB)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("node id %q emitted %d times: node_key carries no "+
				"collector_id, so the graph must NOT be grouped by collector", id, n)
		}
	}
	for _, tc := range []struct {
		title string
		want  bool
		why   string
	}{
		{dualWithdrawnNodeNameB, true,
			"collector " + dualCollectorB + " still advertises this node in its " +
				"current session; only collector " + dualCollectorA + " withdrew " +
				"it. Missing means is_withdraw is being resolved ACROSS both " +
				"collectors, so one collector's withdrawal erased another " +
				"collector's live node -- decided by a (ts_collector, " +
				"stream_seq) comparison that means nothing between two sessions"},
		{dualWithdrawnNodeNameA, false,
			"this is the WITHDRAWN sighting's name. Rendering it means the " +
				"label was merged before the state filter ran, so the graph " +
				"labels a live node with the view that retracted it"},
		{dualSharedNodeNameA, true,
			"both collectors advertise this node and both are in state, so the " +
				"merge picks one label: the lexically-first collector's, which " +
				"is the same on every refresh. " + dualSharedNodeNameB + " here " +
				"means it is being picked by a timestamp comparison across two " +
				"collectors' clocks instead"},
		{dualSharedNodeNameB, false,
			"see " + dualSharedNodeNameA + ": one node draws one label, and the " +
				"choice is by collector id, not by whichever pod's clock ran ahead"},
		{dualEndpointOnlyTitle, true,
			"this node is never advertised by anyone -- it reaches the graph " +
				"only as the far end of the link collector " + dualCollectorA +
				" withdrew and collector " + dualCollectorB + " kept. It is the " +
				"one thing the nodes target's OWN copy of the edges query " +
				"decides on its own, so missing means that embedded copy is " +
				"resolving is_withdraw across both collectors while the nodeset " +
				"beside it does not"},
	} {
		if drawn[tc.title] != tc.want {
			t.Errorf("topology drew title %q: %v, want %v. %s. Titles drawn: %v",
				tc.title, drawn[tc.title], tc.want, tc.why, slices.Sorted(maps.Keys(drawn)))
		}
	}
	// The SRGB is the second half of the same choice, and the only thing in
	// this package that reads the nodeset's argMin(srgb_base, collector_id)
	// and argMin(srgb_size, collector_id) at all. Both collectors advertise
	// this node with a DIFFERENT block, so one of the two strings below is
	// the answer and the other names the bug -- see dualSharedNodeSrgbBaseA.
	srgb := func(base, size uint32) string {
		return fmt.Sprintf("SRGB %d-%d", base, base+size-1)
	}
	wantSRGB, otherSRGB := srgb(dualSharedNodeSrgbBaseA, dualSharedNodeSrgbSizeA),
		srgb(dualSharedNodeSrgbBaseB, dualSharedNodeSrgbSizeB)
	if got := mainstat[dualSharedNodeNameA]; got != wantSRGB {
		why := "that is neither collector's whole block, so the base and the " +
			"size were resolved by different rules and the graph is printing " +
			"an SRGB no collector ever reported"
		switch got {
		case "":
			why = "empty means the node was not drawn at all, or that it was " +
				"drawn from a sighting carrying no SRGB"
		case otherSRGB:
			why = "that is collector " + dualCollectorB + "'s block. The name " +
				"and the SRGB have to be chosen the same way -- by collector " +
				"id -- or the graph renders one collector's label over another " +
				"collector's numbers, which is a node that exists on neither " +
				"collector's view"
		}
		t.Errorf("the shared node's mainstat is %q, want %q (collector %s's). %s",
			got, wantSRGB, dualCollectorA, why)
	}
}

// TestDualHomedTopologyEdgesMergeBothCollectors is the nodes test above for
// the OTHER nodeGraph target. Nothing covered it: every link endpoint in the
// fixture is also an advertised node, so the embedded edges CTE contributes
// nothing to the node count and the edges refId could have been scoped to one
// collector without a single assertion moving.
//
// Merged, kept-A and kept-B are three different numbers here as well, and the
// shared link's presence and metric decide the same two questions the shared
// node's title does above.
//
// It runs under BOTH $state renderings, and the second one is not decoration.
// Under "active" the shared link's only surviving sighting is collector B's,
// so exactly one row per (id, source, target) reaches the outer GROUP BY and
// the duplicate-id check below CANNOT fail: adding collector_id to that
// GROUP BY -- the mutation the check exists to catch, which would ship two
// nodeGraph edges carrying one id -- produces no duplicate at all. The node
// side's twin is falsifiable because node 6 is active on both collectors;
// the edge side has no such link while the withdrawn one is filtered out.
// "withdrawn too" puts both collectors' sightings of it into the merge, which
// arms the duplicate check and makes argMin(mainstat, collector_id) decidable
// at the same time -- so the expected metric FLIPS between the two cases,
// from B's (A's is filtered away) to A's (chosen by collector id).
//
// The edge COUNT is the same under either state, because
// dualHomedRequireSomeCollectorStillHolds pins that no link pair is withdrawn
// on every collector: "every pair some collector still advertises" and "every
// pair the fixture wrote" are the same set.
func TestDualHomedTopologyEdgesMergeBothCollectors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	wantEdges := dualHomedGraphEdgeCount(t, ctx, ch)
	shared := dualHomedLinkEndpointKeys(t, ctx, ch,
		dualWithdrawnLinkLocal, dualWithdrawnLinkRemote)
	for _, tc := range []struct {
		state string
		// want is the shared link's IGP metric the graph must render under
		// this state, and wrong is the other collector's -- the value that
		// names the defect rather than merely differing from the answer.
		want, wrong uint32
		why         string
	}{
		{"0", dualWithdrawnLinkMetricB, dualWithdrawnLinkMetricA,
			"collector " + dualCollectorA + "'s sighting is withdrawn and the " +
				"state filter runs BEFORE the display merge, so only collector " +
				dualCollectorB + "'s metric is left to render. Reading the " +
				"other one means the metrics were merged first"},
		{"0, 1", dualWithdrawnLinkMetricA, dualWithdrawnLinkMetricB,
			"both sightings are in state here, so the merge has a real choice " +
				"and has to make it by collector id -- the lexically first, " +
				dualCollectorA + ". Reading the other one means it is being " +
				"made by an argMax over (ts_collector, stream_seq), which " +
				"across two collectors is two clocks and two independent " +
				"counters and moves with the skew between two pods"},
	} {
		t.Run("state "+tc.state, func(t *testing.T) {
			sql := substituteGrafana(
				panelSQL(t, "link-state-topology", "Topology", "edges"),
				dualHomedVars("0", tc.state))
			rows, err := ch.conn.Query(ctx, qualify(ch, sql))
			if err != nil {
				t.Fatalf("topology edges: %v\nSQL:\n%s", err, sql)
			}
			defer rows.Close()
			// Columns are id, source, target, mainstat, secondarystat.
			ids := map[string]int{}
			metric := map[[2]string]string{}
			for _, row := range dashRowsAsStrings(t, rows) {
				ids[row[0]]++
				metric[[2]string{row[1], row[2]}] = row[3]
			}
			if len(metric) != wantEdges {
				t.Errorf("topology drew %d distinct edges; want %d -- %s's and %s's "+
					"together. Fewer means the edges target resolves one collector's "+
					"session, or that one collector's withdrawal erased a link the "+
					"other collector still advertises",
					len(metric), wantEdges, dualCollectorA, dualCollectorB)
			}
			for id, n := range ids {
				if n > 1 {
					t.Errorf("edge id %q emitted %d times: the id is a hash of the link "+
						"descriptors and carries no collector_id, so two collectors "+
						"seeing one link must draw one edge", id, n)
				}
			}
			got, ok := metric[shared]
			switch {
			case !ok:
				t.Errorf("the link collector %s withdrew and collector %s still "+
					"advertises (%v) is not on the graph. Its withdrawal is the newest "+
					"row of the two sessions, so an is_withdraw resolved across both "+
					"collectors drops it -- which is a live adjacency erased because a "+
					"different collector stopped seeing it. Edges drawn: %v",
					dualCollectorA, dualCollectorB, shared, metric)
			case got != fmt.Sprint(tc.want):
				t.Errorf("the shared link's IGP metric is %s, want %d. %s (%d is "+
					"the other collector's)", got, tc.want, tc.why, tc.wrong)
			}
		})
	}
}

// TestDualHomedTopologySaysWhichCollectorHoldsEachObject pins detail__collectors,
// which exists to close the cost of deciding is_withdraw per collector.
//
// Keeping an object while ANY collector still advertises it is the right call --
// the alternative resolves a live node's existence by comparing two collectors'
// wall clocks, which schema.sql says is meaningless. But it trades disguised
// LOSS for disguised PERSISTENCE: a collector whose session is still nominally
// current, yet has stopped receiving updates, keeps a withdrawn node or
// adjacency drawn indefinitely, and nothing on the graph said WHICH collector
// was holding it up. An operator could not tell a live adjacency from one a
// single stale session was propping up.
//
// The fixture's node 7 is the case this field exists for: both collectors
// advertised it, collector A withdrew it, and it is STILL DRAWN because B
// holds it. detail__collectors must name B alone.
func TestDualHomedTopologySaysWhichCollectorHoldsEachObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	both := dualCollectorA + ", " + dualCollectorB
	want := map[string]string{
		dualHomedNodeKeyFor(t, ctx, ch, 1):                     dualCollectorA,
		dualHomedNodeKeyFor(t, ctx, ch, 3):                     dualCollectorB,
		dualHomedNodeKeyFor(t, ctx, ch, dualSharedNodeLast):    both,
		dualHomedNodeKeyFor(t, ctx, ch, dualWithdrawnNodeLast): dualCollectorB,
	}
	// The endpoint-only node is advertised by nobody, so it holds no
	// collectors -- the same statement detail__advertised already makes, and
	// the row that stops "" from being a bug the other four cannot see.
	endpoints := dualHomedLinkEndpointKeys(t, ctx, ch,
		dualWithdrawnLinkLocal, dualWithdrawnLinkRemote)
	want[endpoints[1]] = ""

	distinct := map[string]bool{}
	for _, v := range want {
		distinct[v] = true
	}
	if len(distinct) < 4 {
		t.Fatalf("the expectations take only %d distinct values (%v), so a panel "+
			"answering with one constant could satisfy them. This assertion is "+
			"only worth running while the fixture keeps a node held by A alone, "+
			"one held by B alone, one held by both, and one held by neither",
			len(distinct), distinct)
	}

	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", "Topology", "nodes"),
		dualHomedVars("0", "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("topology nodes: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	idCol, colsCol := dashColumnIndex(t, rows, "id"), dashColumnIndex(t, rows, "detail__collectors")
	got := map[string]string{}
	for _, row := range dashRowsAsStrings(t, rows) {
		got[row[idCol]] = row[colsCol]
	}
	for key, wantCols := range want {
		if got[key] != wantCols {
			t.Errorf("node %s reports detail__collectors=%q; want %q. A node the "+
				"graph draws while only one collector still advertises it must say "+
				"so: without that, an operator cannot tell a live object from one a "+
				"single stale session is holding up, which is the cost of deciding "+
				"is_withdraw per collector", key, got[key], wantCols)
		}
	}
}

// TestDualHomedTopologySaysWhichCollectorHoldsEachEdge is the edge half of the
// node test above. The shared link is the decidable case: both collectors
// advertised it and A withdrew it, so under the active-only state B holds it
// alone, and under "withdrawn too" both sightings are back.
func TestDualHomedTopologySaysWhichCollectorHoldsEachEdge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	shared := dualHomedLinkEndpointKeys(t, ctx, ch,
		dualWithdrawnLinkLocal, dualWithdrawnLinkRemote)
	for _, tc := range []struct {
		state string
		want  string
		why   string
	}{
		{"0", dualCollectorB, "A withdrew it, so B is the only collector holding it"},
		{"0, 1", dualCollectorA + ", " + dualCollectorB,
			"withdrawn rows are in scope, so both collectors' sightings count"},
	} {
		t.Run("state_"+strings.ReplaceAll(tc.state, ", ", "_"), func(t *testing.T) {
			sql := substituteGrafana(
				panelSQL(t, "link-state-topology", "Topology", "edges"),
				dualHomedVars("0", tc.state))
			rows, err := ch.conn.Query(ctx, qualify(ch, sql))
			if err != nil {
				t.Fatalf("topology edges: %v\nSQL:\n%s", err, sql)
			}
			defer rows.Close()
			src, dst := dashColumnIndex(t, rows, "source"), dashColumnIndex(t, rows, "target")
			colsCol := dashColumnIndex(t, rows, "detail__collectors")
			got := map[[2]string]string{}
			for _, row := range dashRowsAsStrings(t, rows) {
				got[[2]string{row[src], row[dst]}] = row[colsCol]
			}
			if got[shared] != tc.want {
				t.Errorf("the shared link reports detail__collectors=%q; want %q -- %s",
					got[shared], tc.want, tc.why)
			}
		})
	}
}

// dashColumnIndex finds a column by NAME so a positional read cannot silently
// follow a reordered SELECT list. Positional scans have had to be repaired
// more than once after a leading column was prepended; this is the cheaper
// habit.
func dashColumnIndex(t *testing.T, rows driver.Rows, name string) int {
	t.Helper()
	for i, c := range rows.Columns() {
		if c == name {
			return i
		}
	}
	t.Fatalf("the panel has no %q column; it returns %v. A test that scanned "+
		"by position here would have read a different column and asserted "+
		"against it", name, rows.Columns())
	return -1
}

// dualHomedNodeKeyFor resolves the node_key ClickHouse materialized for the
// dual-homed fixture's node at the given last octet, found by the router_id
// its descriptor carries rather than by name -- two collectors name the
// contested nodes differently, so a name is not a key here.
func dualHomedNodeKeyFor(t *testing.T, ctx context.Context, ch *ClickHouse, last byte) string {
	t.Helper()
	q := fmt.Sprintf(`SELECT groupUniqArray(toString(node_key))
    FROM vantage.ls_nodes
    WHERE router_ip = toIPv6('%s') AND peer_ip = toIPv6('%s')
      AND router_id = '%s'`,
		dualHomedRouterIP, dualHomedPeerIP, fmt.Sprintf("0affc8%02x", last))
	var keys []string
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&keys); err != nil {
		t.Fatalf("node key for octet %d: %v", last, err)
	}
	if len(keys) != 1 {
		t.Fatalf("octet %d resolves to %d node keys (%v), want exactly 1 -- "+
			"node_key is a hash of the descriptor tuple, so two collectors "+
			"seeing one node must mint one key", last, len(keys), keys)
	}
	return keys[0]
}

// dualHomedLinkEndpointKeys resolves the (local_node_key, remote_node_key)
// ClickHouse materialized for one of the dual-homed fixture's links, found by
// the interface addresses its descriptors carry.
//
// The node graph's source and target columns are those keys as strings, and an
// assertion about one particular edge needs them by identity rather than by
// position in the result. Keyed on the link rather than on its endpoints'
// names because one endpoint of the link that matters here is a node nobody
// ever advertised, so it has no name to look up.
func dualHomedLinkEndpointKeys(t *testing.T, ctx context.Context, ch *ClickHouse, local, remote byte) [2]string {
	t.Helper()
	q := fmt.Sprintf(`SELECT groupUniqArray((toString(local_node_key), toString(remote_node_key)))
    FROM vantage.ls_links
    WHERE router_ip = toIPv6('%s') AND peer_ip = toIPv6('%s')
      AND local_ifaddr = '10.255.200.%d' AND remote_ifaddr = '10.255.200.%d'`,
		dualHomedRouterIP, dualHomedPeerIP, local, remote)
	var pairs [][]string
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&pairs); err != nil {
		t.Fatalf("node keys for link %d-%d: %v", local, remote, err)
	}
	if len(pairs) != 1 {
		t.Fatalf("the link %d-%d resolves to %d endpoint pairs (%v), want "+
			"exactly 1 -- an assertion about one edge cannot be written "+
			"against an ambiguous pair", local, remote, len(pairs), pairs)
	}
	return [2]string{pairs[0][0], pairs[0][1]}
}

// dualHomedFocusOptions runs one committed dashboard's $focus picker against
// the dual-homed router and returns value -> label, taking column 0 as the
// value and column 1 as the label exactly as the ClickHouse plugin does.
//
// Keyed on the VALUE rather than on the label, which is the opposite of
// focusOptions. Both tests below ask what the picker says about one
// particular node_key, and the label is the answer rather than the question.
//
// The row count comes back beside the map because the two can differ, and the
// difference is an assertion: one node_key offered twice collapses in the map
// and would otherwise be invisible.
func dualHomedFocusOptions(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard string) (byValue map[string]string, rowCount int) {
	t.Helper()
	sql := substituteGrafana(dashboardVariableQuery(t, dashboard, "focus"),
		dualHomedVars("0", "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("%s $focus: %v\nSQL:\n%s", dashboard, err, sql)
	}
	defer rows.Close()
	out := map[string]string{}
	all := dashRowsAsStrings(t, rows)
	for _, row := range all {
		out[row[0]] = row[1]
	}
	return out, len(all)
}

// TestDualHomedFocusVariableListsBothCollectorsNodes covers the seam the debt
// ratchet could not see until its walk was widened to templating.list[]: a
// template VARIABLE, not a panel target.
//
// The $focus picker filters the very panels the dual-homed fixes above cover. A
// picker that resolves one collector's session offers only that collector's
// nodes, and the panels beside it then filter BOTH collectors' rows by a
// node_key only one of them could have offered -- so a correct panel is
// emptied by its own control, with no error and nothing on screen saying the
// node belongs to the other collector's view.
func TestDualHomedFocusVariableListsBothCollectorsNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	wantFocus := dualHomedFocusNodeCount(t, ctx, ch)
	for _, dash := range linkStateDashboards {
		t.Run(dash, func(t *testing.T) {
			offered, rows := dualHomedFocusOptions(t, ctx, ch, dash)
			if len(offered) != wantFocus {
				t.Errorf("%s $focus offered %d nodes; want %d -- every node "+
					"either %s or %s still advertises. Too few means the "+
					"picker still resolves one collector's session, or "+
					"decides is_withdraw across both and lets one collector's "+
					"withdrawal retract the other collector's live node. "+
					"Offered: %v", dash, len(offered), wantFocus,
					dualCollectorA, dualCollectorB, offered)
			}
			// The other direction, which the count alone cannot state:
			// node_key carries no collector_id, so the node both collectors
			// advertise has to arrive as ONE option. Two rows under one value
			// is Grafana's picker listing the same node twice.
			if rows != len(offered) {
				t.Errorf("%s $focus returned %d rows for %d distinct node "+
					"keys: the node both collectors advertise must be offered "+
					"once, not once per collector", dash, rows, len(offered))
			}
		})
	}
}

// TestDualHomedFocusLabelAgreesWithTheTopologyGraph is the assertion neither
// panel's own test can make.
//
// One node_key, two collectors, two names: the merged topology graph and the
// $focus picker each have to choose which to render, and if they choose
// differently an operator reads two names for one node and has no way to see
// it is one node. The graph is pinned to argMin(name, collector_id) --
// stable across two collectors' clocks, which argMax over (ts_collector,
// stream_seq) is not, since schema.sql:44 and :62 make that tuple meaningful
// only WITHIN a session. The picker has to choose the same way.
//
// Both labels are read out of the committed panels and the contested keys out
// of the fixture, so no name is written down here that a dashboard edit would
// have to come back and update.
func TestDualHomedFocusLabelAgreesWithTheTopologyGraph(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	contested := dualHomedContestedNodeKeys(t, ctx, ch)
	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", "Topology", "nodes"),
		dualHomedVars("0", "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("topology nodes: %v\nSQL:\n%s", err, sql)
	}
	drawn := map[string]string{}
	for _, row := range dashRowsAsStrings(t, rows) {
		drawn[row[0]] = row[1] // id, title
	}
	rows.Close()

	for _, dash := range linkStateDashboards {
		t.Run(dash, func(t *testing.T) {
			offered, _ := dualHomedFocusOptions(t, ctx, ch, dash)
			for _, key := range slices.Sorted(maps.Keys(contested)) {
				names := contested[key]
				want, onGraph := drawn[key]
				if !onGraph {
					t.Errorf("the topology graph does not draw node %s, which "+
						"both collectors advertise (%v); with nothing to agree "+
						"WITH, this assertion cannot be made", key, names)
					continue
				}
				// The graph's own label has to be one of the names a
				// collector actually sent. Two panels falling back to the
				// same hex router_id would agree while rendering neither
				// collector's view.
				if !slices.Contains(names, want) {
					t.Errorf("the topology graph labels node %s %q, which "+
						"neither collector advertises (%v)", key, want, names)
					continue
				}
				got, offeredHere := offered[key]
				if !offeredHere {
					t.Errorf("%s $focus offers no option for node %s, which "+
						"the topology graph draws as %q. An operator can see "+
						"the node and cannot focus on it", dash, key, want)
					continue
				}
				if got != want {
					t.Errorf("%s $focus labels node %s %q while the topology "+
						"graph labels it %q. Both collectors advertise it "+
						"(%v), so each panel picks one name -- and picking "+
						"differently renders one node under two names. The "+
						"choice is argMin(name, collector_id), which is the "+
						"same on every refresh; a (ts_collector, stream_seq) "+
						"argMax across two collectors moves with the skew "+
						"between two pods", dash, key, got, want, names)
				}
			}
		})
	}
}

// TestDualHomedCompletenessTableCountsEachCollectorSeparately pins the
// completeness table's half of the requirement, which is the disguised-loss
// case: merged, the dual-homed router renders ONE row reading 3 of 6
// nodes, which an operator reads as three nodes having aged out. They had
// not. They were the other collector's live view.
//
// Per collector the two columns must agree -- each collector has re-dumped
// inside its own current session -- and the two rows must carry that
// collector's own counts, which the fixture makes unequal so that "kept A",
// "kept B" and "merged" are three distinguishable answers.
func TestDualHomedCompletenessTableCountsEachCollectorSeparately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	byCollector := map[string]completenessRow{}
	for _, r := range completenessRows(t, ctx, ch) {
		if r.router != dualHomedSysname {
			continue
		}
		if prev, dup := byCollector[r.collector]; dup {
			t.Fatalf("collector %s got two rows for %s (%v and %v); the panel "+
				"keys on (collector_id, router_sysname, router_ip)",
				r.collector, dualHomedSysname, prev, r)
		}
		// The address is half the key the panel groups on, and nothing else
		// in the package asserts the rendered one: two routers can share a
		// sysname, and this scan would collect the other one's counts.
		if r.routerIP != dualHomedRouterIPv6 {
			t.Errorf("collector %s's %s row reads router_ip %q, want %q",
				r.collector, dualHomedSysname, r.routerIP, dualHomedRouterIPv6)
		}
		byCollector[r.collector] = r
	}
	aNodes, bNodes := dualHomedObjectCounts(t, "nodes")
	aEdges, bEdges := dualHomedObjectCounts(t, "edges")
	for _, tc := range []struct {
		collector    string
		nodes, edges uint64
	}{
		{dualCollectorA, uint64(aNodes), uint64(aEdges)},
		{dualCollectorB, uint64(bNodes), uint64(bEdges)},
	} {
		got, ok := byCollector[tc.collector]
		if !ok {
			t.Errorf("no completeness row for collector %s: %v. One row for a "+
				"router two collectors monitor is the merge this test exists "+
				"for -- its counts belong to whichever collector's session id "+
				"was larger, against a window holding both",
				tc.collector, byCollector)
			continue
		}
		if got.sessions != 1 {
			t.Errorf("%s sessions = %d, want 1 -- each collector holds exactly "+
				"one session for this router, and 2 means the other "+
				"collector's session is being counted as this one's reconnect",
				tc.collector, got.sessions)
		}
		if got.nodesCurrent != tc.nodes || got.nodesWindow != tc.nodes {
			t.Errorf("%s nodes = %d/%d, want %d/%d. A window wider than the "+
				"current session here is the disguised loss: this collector "+
				"HAS re-dumped, and the extra rows are the other collector's "+
				"live view rendered as nodes that aged out",
				tc.collector, got.nodesCurrent, got.nodesWindow, tc.nodes, tc.nodes)
		}
		if got.edgesCurrent != tc.edges || got.edgesWindow != tc.edges {
			t.Errorf("%s edges = %d/%d, want %d/%d",
				tc.collector, got.edgesCurrent, got.edgesWindow, tc.edges, tc.edges)
		}
	}
}

// TestDualHomedTopologyStatsCountOnlyTheirOwnCollector covers the three stat
// panels that now repeat per collector. Each tile must answer for the
// collector in its title and for nothing else.
//
// Every case here is decidable: the fixture gives the two collectors
// different object counts, different session start times, and one session
// each against two merged, so the merged answer differs from BOTH tiles in
// every one of the three panels.
func TestDualHomedTopologyStatsCountOnlyTheirOwnCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	// dualHomedVars leaves $collector at topologyVarsOn's "c1" default, which
	// matches NO row under this router since the rename -- so a case that
	// forgets to override it fails for both collectors with an empty answer,
	// rather than passing for A and failing only for B in a way that reads
	// like a panel bug.
	varsFor := func(collector string) map[string]string {
		vars := dualHomedVars("0", "0")
		vars["collector"] = collector
		return vars
	}
	aNodes, bNodes := dualHomedObjectCounts(t, "nodes")
	aEdges, bEdges := dualHomedObjectCounts(t, "edges")

	t.Run("objects in current session", func(t *testing.T) {
		for _, tc := range []struct {
			collector    string
			nodes, edges uint64
		}{
			{dualCollectorA, uint64(aNodes), uint64(aEdges)},
			{dualCollectorB, uint64(bNodes), uint64(bEdges)},
		} {
			sql := substituteGrafana(
				panelSQL(t, "link-state-topology", lsTopologyObjectsTitle, "A"),
				varsFor(tc.collector))
			var nodes, edges uint64
			if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&nodes, &edges); err != nil {
				t.Fatalf("%s objects: %v\nSQL:\n%s", tc.collector, err, sql)
			}
			if nodes != tc.nodes || edges != tc.edges {
				t.Errorf("%s objects = %d nodes / %d edges, want %d/%d. This "+
					"stat is the denominator for the graph above it, and the "+
					"graph draws both collectors -- so a tile naming one "+
					"collector has to count that collector's session alone",
					tc.collector, nodes, edges, tc.nodes, tc.edges)
			}
		}
	})

	t.Run("reconnects in range", func(t *testing.T) {
		for _, collector := range []string{dualCollectorA, dualCollectorB} {
			sql := substituteGrafana(
				panelSQL(t, "link-state-topology", lsTopologyReconnectsTitle, "A"),
				varsFor(collector))
			var sessions uint64
			if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&sessions); err != nil {
				t.Fatalf("%s reconnects: %v\nSQL:\n%s", collector, err, sql)
			}
			if sessions != 1 {
				t.Errorf("%s reconnects = %d, want 1. Two collectors issue "+
					"disjoint session ids, so counting across both reports a "+
					"router that has never reconnected as having reconnected "+
					"once per collector watching it", collector, sessions)
			}
		}
	})

	t.Run("current session age", func(t *testing.T) {
		for _, tc := range []struct {
			collector string
			want      uint64
		}{
			{dualCollectorA, uint64(dualSessionAAge / time.Second)},
			{dualCollectorB, uint64((dualSessionAAge + dualSessionBHeadStart) / time.Second)},
		} {
			sql := substituteGrafana(
				panelSQL(t, "link-state-topology", lsTopologySessionAgeTitle, "A"),
				varsFor(tc.collector))
			var age uint64
			if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&age); err != nil {
				t.Fatalf("%s session age: %v\nSQL:\n%s", tc.collector, err, sql)
			}
			// The upper bound only allows for the suite's own runtime, the
			// way TestCurrentSessionAgeMeasuresWhenTheSessionBegan's does.
			if age < tc.want || age > tc.want+600 {
				t.Errorf("%s session age = %ds, want about %ds. The two "+
					"collectors' sessions came up %s apart, so a tile reading "+
					"the other one's age is reading the session max(session_id) "+
					"picked rather than the session its title names",
					tc.collector, age, tc.want, dualSessionBHeadStart)
			}
		}
	})
}

// The fixture is only worth what its arrangement is worth. Every
// current-session assertion below is decidable only because the fixture is
// adversarial to the two wrong ways of finding "the current session", so this
// pins that arrangement: an innocent-looking edit that makes dashStale the
// smaller id again, or that stamps every ls_* row with one clock, silently
// turns a suite of invariant tests back into a suite that passes when the
// invariant is deleted.
func TestTopologyFixtureDefeatsBothNaiveSessionDerivations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	newest := func(table string) uint64 {
		var sid uint64
		q := fmt.Sprintf("SELECT argMax(session_id, (ts_collector, stream_seq)) "+
			"FROM vantage.%s FINAL WHERE router_ip = toIPv6('%s')", table, dashRouterIP)
		if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&sid); err != nil {
			t.Fatalf("newest session in %s: %v", table, err)
		}
		return sid
	}
	if got := newest("peer_events"); got != dashSession {
		t.Fatalf("peer_events names session %d as current, want %d -- the "+
			"queries all read the session from here", got, dashSession)
	}
	// Each link-state table must name the WRONG session, or a panel that
	// derives its session from the table it is querying passes by luck.
	for _, table := range []string{"ls_links", "ls_nodes", "ls_prefixes"} {
		if got := newest(table); got != dashStale {
			t.Errorf("%s names session %d as newest, want the stale session %d; "+
				"deriving the session from %s would give the right answer by "+
				"accident and every current-session test here would be vacuous",
				table, got, dashStale, table)
		}
	}
	// max(session_id) -- the resolution the dashboards and query/ both use --
	// must name the current session. Requiring max to disagree with the
	// argMax instead would be wrong: ts_collector is a collector wall clock
	// too and LEADS the argMax's sort tuple, so argMax is the more
	// clock-dependent of the two. See dashSession's own comment for the
	// measurement.
	var maxID uint64
	q := fmt.Sprintf("SELECT max(session_id) FROM vantage.peer_events "+
		"WHERE router_ip = toIPv6('%s')", dashRouterIP)
	if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&maxID); err != nil {
		t.Fatalf("max session: %v", err)
	}
	if maxID != dashSession {
		t.Errorf("max(session_id) = %d, want the current session %d", maxID, dashSession)
	}
	// The fixture must still be adversarial, and after the reversal that job
	// falls entirely on the link-state tables: their newest row belongs to the
	// STALE session, so a panel deriving its session from the table it queries
	// gets the wrong answer rather than the right one by luck. The loop above
	// is what pins that; this is the reminder that it is now the only dynamic
	// guard here, the id ordering having become the ordinary one. The
	// expression itself is pinned statically instead, by
	// TestDashboardsResolveTheCurrentSessionTheWayQueryDoes.
	if dashStale >= dashSession {
		t.Fatalf("dashStale (%d) must be below dashSession (%d): a collector "+
			"issues session ids from a monotonic clock, and a fixture that "+
			"inverts them is testing a shape nothing produces", dashStale, dashSession)
	}
}

// edgeFrameRow is one row of the topology dashboard's edges frame.
type edgeFrameRow struct {
	id            string
	source        string
	target        string
	mainstat      uint32
	secondarystat uint32
	collectors    string
}

// edgesFrame runs the committed edges frame for the fixture's first peer.
func edgesFrame(t *testing.T, ctx context.Context, ch *ClickHouse, focus, state string) []edgeFrameRow {
	t.Helper()
	return edgesFrameFor(t, ctx, ch, dashPeerIP, focus, state)
}

// edgesFrameFor is edgesFrame for one chosen BMP peer.
func edgesFrameFor(t *testing.T, ctx context.Context, ch *ClickHouse, peer, focus, state string) []edgeFrameRow {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", "Topology", "edges"),
		topologyVarsFor(peer, focus, state))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("edges frame: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	var out []edgeFrameRow
	// Mirrors the uniqueness check the nodes frame has had all along.
	// Grafana's Node Graph keys edges by id, so two edges sharing one collapse
	// into a single drawn line with no error anywhere -- and parallel links,
	// the case the identity tuple in the hash exists for, are exactly where
	// that would happen. Replacing the hash with a constant left every test
	// green before this.
	byID := map[string]string{}
	for rows.Next() {
		var r edgeFrameRow
		if err := rows.Scan(&r.id, &r.source, &r.target, &r.mainstat, &r.secondarystat, &r.collectors); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if prev, dup := byID[r.id]; dup {
			t.Fatalf("edges frame returned id %q for both %s and %s->%s; "+
				"Grafana's Node Graph would draw one edge instead of two",
				r.id, prev, r.source, r.target)
		}
		byID[r.id] = r.source + "->" + r.target
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// The edges frame must contain exactly the active links of the router's
// current session -- not the withdrawn one, and not the previous session's.
func TestTopologyEdgesFrameIsCurrentSessionOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	seen := map[string]string{}
	for _, e := range edgesFrame(t, ctx, ch, "0", "0") {
		seen[e.source+"->"+e.target] = e.id
	}
	if len(seen) != 3 {
		t.Fatalf("edges frame returned %d edges, want 3 (A->B, B->C, A->E); "+
			"a 4th means the withdrawn C->A survived, a 5th means the stale "+
			"session leaked in: %v", len(seen), seen)
	}
}

// $state is exposed rather than hard-filtered (OpenBMP's prior art), so
// selecting "all" must bring the withdrawn link back rather than doing
// nothing. This is the assertion that proves the HAVING clause reads the
// variable at all.
func TestTopologyEdgesFrameHonoursStateVariable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	count := func(state string) int {
		sql := substituteGrafana(
			panelSQL(t, "link-state-topology", "Topology", "edges"),
			topologyVars("0", state))
		var n uint64
		q := fmt.Sprintf("SELECT count() FROM (%s)", sql)
		if err := ch.conn.QueryRow(ctx, qualify(ch, q)).Scan(&n); err != nil {
			t.Fatalf("count edges with state=%s: %v", state, err)
		}
		return int(n)
	}
	active, all := count("0"), count("0, 1")
	if active != 3 {
		t.Fatalf("state=active returned %d edges, want 3", active)
	}
	if all != 4 {
		t.Fatalf("state=all returned %d edges, want 4 -- the withdrawn C->A "+
			"link must reappear when the operator asks for it", all)
	}
}

// nodeFrame runs the committed nodes-frame SQL and returns id -> title.
func nodeFrame(t *testing.T, ctx context.Context, ch *ClickHouse, focus, state string) (map[string]string, map[string]string) {
	t.Helper()
	return nodeFrameFor(t, ctx, ch, dashPeerIP, focus, state)
}

// nodeFrameFor is nodeFrame for one chosen BMP peer.
func nodeFrameFor(t *testing.T, ctx context.Context, ch *ClickHouse, peer, focus, state string) (map[string]string, map[string]string) {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", "Topology", "nodes"),
		topologyVarsFor(peer, focus, state))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("nodes frame: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	titles, subtitles := map[string]string{}, map[string]string{}
	for rows.Next() {
		var id, title, subtitle, routerID string
		var mainstat, secondarystat string
		var advertised uint8
		// collectors is scanned but unused here: this reader answers title and
		// subtitle questions. detail__collectors has its own test, and leaving
		// it out of the Scan would fail with an arity error rather than a
		// silent column shift -- which is how adding it was caught.
		var collectors string
		if err := rows.Scan(&id, &title, &subtitle, &mainstat, &secondarystat, &routerID, &advertised, &collectors); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, dup := titles[id]; dup {
			t.Fatalf("nodes frame returned id %q twice; Grafana needs unique node ids", id)
		}
		titles[id], subtitles[id] = title, subtitle
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return titles, subtitles
}

// The invariant the whole two-frame design exists to hold: every edge
// endpoint resolves to a node in the nodes frame. An edge referencing an
// absent node id does not render and Grafana reports no error, so nothing
// but a test catches it.
func TestTopologyEveryEdgeEndpointHasANode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	titles, _ := nodeFrame(t, ctx, ch, "0", "0")
	edges := edgesFrame(t, ctx, ch, "0", "0")
	edgeCount := len(edges)
	for _, e := range edges {
		for _, end := range []string{e.source, e.target} {
			if _, ok := titles[end]; !ok {
				t.Fatalf("edge %s->%s references node id %q, which the nodes "+
					"frame does not contain; Grafana would drop this edge silently",
					e.source, e.target, end)
			}
		}
	}
	if edgeCount == 0 {
		t.Fatal("edges frame returned zero rows; the every-edge-endpoint-has-a-node " +
			"invariant was not exercised by this test")
	}
}

// Five nodes, not four and not three: the four advertised nodes (including
// D, which has no adjacency at all -- a shape real routers do produce) plus
// E, which exists only as a link endpoint.
func TestTopologyNodesFrameUnionCoversIsolatedAndOrphanNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	titles, subtitles := nodeFrame(t, ctx, ch, "0", "0")
	if len(titles) != 6 {
		t.Fatalf("nodes frame returned %d nodes, want 6 (A,B,C,D advertised + "+
			"E endpoint-only + P the LAN pseudonode): %v", len(titles), titles)
	}
	byTitle := map[string]bool{}
	for _, ti := range titles {
		byTitle[ti] = true
	}
	// dash-d is advertised with no links at all. Deriving the node set from
	// ls_links alone -- OpenBMP's approach -- would drop it, and its router
	// would render as a blank panel.
	if !byTitle["dash-d"] {
		t.Errorf("isolated node dash-d is missing; the node set was derived "+
			"from links alone: %v", titles)
	}
	// 10.255.19.5 is never advertised as a node, only as a link endpoint.
	if !byTitle["10.255.19.5"] {
		t.Errorf("endpoint-only node 10.255.19.5 is missing; the node set was "+
			"derived from ls_nodes alone: %v", titles)
	}
	var placeholders int
	for _, st := range subtitles {
		if st == "endpoint only" {
			placeholders++
		}
	}
	if placeholders != 1 {
		t.Errorf("want exactly 1 node marked \"endpoint only\", got %d: %v",
			placeholders, subtitles)
	}
}

// The title fallback chain, on the three cases a lab archive actually
// contains: a name, a router_id_v4 with no name, and neither -- the last
// being a real node whose only available label is hex.
func TestTopologyNodeTitleFallsBackThroughNameV4AndHex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	titles, _ := nodeFrame(t, ctx, ch, "0", "0")
	got := map[string]bool{}
	for _, ti := range titles {
		got[ti] = true
	}
	for _, want := range []string{
		"dash-a",      // name
		"10.255.29.2", // router_id_v4, no name
		"10.255.19.3", // neither: decoded from hex router_id
	} {
		if !got[want] {
			t.Errorf("node title %q missing from %v", want, titles)
		}
	}
	// B's descriptor router-id (10.255.19.2) and its router_id_v4
	// (10.255.29.2) deliberately differ. If the SQL's multiIf skipped the
	// router_id_v4 branch and fell through to hex-decoding the descriptor,
	// the title would read 10.255.19.2 instead -- so this string's absence
	// is what proves the v4 branch actually ran.
	if got["10.255.19.2"] {
		t.Errorf("node title 10.255.19.2 present; this is B's descriptor "+
			"router-id decoded from hex, meaning the router_id_v4 branch was "+
			"skipped: %v", titles)
	}
}

// completenessRow is one row of the topology dashboard's completeness table,
// which several tests below read. Sharing the scan keeps the column list in
// one place: the panel gained a router_ip column precisely because two
// routers can share an address, and a per-test scan would let one of them
// drift out of date silently.
type completenessRow struct {
	collector    string
	router       string
	routerIP     string
	sessions     uint64
	nodesCurrent uint64
	nodesWindow  uint64
	edgesCurrent uint64
	edgesWindow  uint64
}

// completenessRows runs the committed completeness panel and returns every
// row it renders, in the order it renders them.
//
// The leading collector column arrived when the panel stopped resolving one
// session per router and started resolving one per (collector, router): a
// router two collectors both monitor is two rows here, and they are two
// separate answers rather than a duplicate.
func completenessRows(t *testing.T, ctx context.Context, ch *ClickHouse) []completenessRow {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", "View completeness by router", "A"),
		topologyVars("0", "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("completeness panel: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	var out []completenessRow
	for rows.Next() {
		var r completenessRow
		if err := rows.Scan(&r.collector, &r.router, &r.routerIP, &r.sessions,
			&r.nodesCurrent, &r.nodesWindow, &r.edgesCurrent, &r.edgesWindow); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// completenessByRouter runs the committed completeness panel and returns its
// rows keyed by router_sysname.
func completenessByRouter(t *testing.T, ctx context.Context, ch *ClickHouse) map[string]completenessRow {
	t.Helper()
	out := map[string]completenessRow{}
	seen := map[string]bool{}
	for _, r := range completenessRows(t, ctx, ch) {
		// A sysname may legitimately appear more than once: the panel keys
		// routers on (collector_id, router_sysname, router_ip), and both of
		// the other two components genuinely repeat a sysname. The corpus
		// replays the same NX-OS capture under two router_ips, so one
		// router's sysname is two rows; and a dual-homed router is one row
		// per collector under a single address, which is what the collector
		// in this identity is for. Only an exact (collector, sysname, ip)
		// repeat is a real duplicate. Anything else is stored under a
		// compound key so no row is silently dropped, while callers keep
		// looking rows up by the bare sysname (every single-collector
		// fixture sysname here is unique).
		id := r.collector + "/" + r.router + "@" + r.routerIP
		if seen[id] {
			t.Fatalf("completeness panel returned router %q at %s twice for "+
				"collector %q", r.router, r.routerIP, r.collector)
		}
		seen[id] = true
		key := r.router
		if _, dup := out[key]; dup {
			key = id
		}
		out[key] = r
	}
	return out
}

// The completeness table is what keeps a correct empty graph from reading
// as a broken dashboard: it reports, per router, how many objects the
// current session carries against how many have been seen across the whole
// window. On the fixture the current session holds 4 link advertisements
// while 5 distinct pairs exist across both sessions, so the two columns must
// differ -- a panel that reported them equal would be summing the wrong
// thing and would hide exactly the XR shortfall it exists to show. Nodes are
// asserted too: the fixture's current session advertises all four nodes
// (A, B, C, D) while the stale session advertises one node of its own
// (dash-stale, added so the nodes-table query's current-session filter has
// something to exclude), so nodes_current_session stays 4 while
// nodes_in_window rises to 5.
func TestCompletenessPanelSeparatesCurrentSessionFromWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byRouter := completenessByRouter(t, ctx, ch)
	got, found := byRouter["dash-r1"]
	if !found {
		t.Fatalf("completeness panel returned no row for dash-r1: %v", byRouter)
	}
	// The router_ip column exists so an operator can match a row back to the
	// $router they picked, which is an address, not a sysname -- and the
	// dashboard's own variable renders it with IPv6NumToString, so this is
	// the string the picker shows.
	if got.routerIP != dashRouterIPv6 {
		t.Errorf("dash-r1 router_ip = %q, want %q", got.routerIP, dashRouterIPv6)
	}
	// The collector column leads the row. Everything this fixture writes
	// belongs to c1, so what this pins is the column ORDER -- a row whose
	// first column were the sysname would land here as "dash-r1". The value
	// itself is decided by TestDualHomedCompletenessTableCountsEachCollectorSeparately,
	// which is the test with two collectors to tell apart.
	if got.collector != "c1" {
		t.Errorf("dash-r1 collector = %q, want %q -- the collector is the "+
			"first column the panel projects", got.collector, "c1")
	}
	// sessions is read from peer_events, not from ls_links, because a
	// session that reconnected and has not yet re-dumped would otherwise
	// be invisible here -- peer_events has a Peer Up in both the current
	// session and the stale one, so this must be 2 regardless of what
	// ls_links carries.
	if got.sessions != 2 {
		t.Errorf("sessions = %d, want 2", got.sessions)
	}
	// This panel is deliberately peer-blind: it is a per-router table across
	// every router, and $peer only means anything relative to $router. So the
	// second BGP peer's node and link count here even though the Node Graph
	// beside it, scoped to one peer, does not draw them.
	// The LAN pseudonode counts: completeness asks how much of the router's
	// link-state database reached us, and a pseudonode is a vertex in that
	// database, not a collection artifact.
	if got.nodesCurrent != 6 {
		t.Errorf("nodes_current_session = %d, want 6 (A, B, C, D, the LAN "+
			"pseudonode + the second peer's dash-f)", got.nodesCurrent)
	}
	if got.nodesWindow != 7 {
		t.Errorf("nodes_in_window = %d, want 7 (6 current + the stale session's "+
			"dash-stale)", got.nodesWindow)
	}
	// 5, not 4: this panel counts what the session carried, withdrawals
	// included, because "did this router re-dump" is a question about
	// message volume rather than about live topology. The topology frames
	// are where a withdrawn link disappears.
	if got.edgesCurrent != 5 {
		t.Errorf("edges_current_session = %d, want 5 (A->B, B->C, A->E, C->A "+
			"+ the second peer's F->G)", got.edgesCurrent)
	}
	if got.edgesWindow != 6 {
		t.Errorf("edges_in_window = %d, want 6 (5 current + 1 stale)", got.edgesWindow)
	}
}

// insertNodeOnlyRouterFixture inserts a router that reaches BMP (a Peer Up)
// and advertises one ls_nodes row, but never advertises a single ls_links
// row -- a shape real routers do produce. It uses its own reserved
// identifiers, disjoint from insertTopologyFixture's, so that fixture's
// counts stay untouched.
func insertNodeOnlyRouterFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	const (
		routerIP = "10.0.199.2"
		peerIP   = "10.255.199.2"
		sysname  = "dash-r2"
		session  = 9101
	)
	routerID := []byte{10, 255, 39, 1}

	peerUp := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: routerIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(corpusCollectorClock),
		Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
			Kind: vantagev1.PeerEvent_KIND_UP,
		}},
	}
	nodeOnly := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: routerIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(corpusCollectorClock),
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Local: lsDesc(routerID)},
			},
		}},
	}
	for i, ev := range []*vantagev1.Envelope{peerUp, nodeOnly} {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert node-only fixture envelope %d: %v", i, err)
		}
	}
}

// The completeness panel's whole purpose is exposed by a router that never
// advertises a single ls_links row -- a shape real routers do produce. An
// INNER JOIN onto ls_links drops such a router from the
// table entirely, silently reproducing the "empty graph reads as broken
// dashboard" failure this panel exists to prevent, one level up. This
// asserts the row exists at all, with a genuine node count and explicit
// zero edge counts -- not merely that the query doesn't error.
func TestCompletenessPanelIncludesRouterWithNoLinks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertNodeOnlyRouterFixture(t, ctx, ch)

	byRouter := completenessByRouter(t, ctx, ch)
	got, found := byRouter["dash-r2"]
	if !found {
		t.Fatalf("completeness panel returned no row for dash-r2 -- the INNER JOIN onto ls_links dropped a router with no links: %v", byRouter)
	}
	if got.sessions != 1 {
		t.Errorf("dash-r2 sessions = %d, want 1", got.sessions)
	}
	if got.nodesCurrent != 1 || got.nodesWindow != 1 {
		t.Errorf("dash-r2 nodes = %d/%d, want 1/1", got.nodesCurrent, got.nodesWindow)
	}
	if got.edgesCurrent != 0 || got.edgesWindow != 0 {
		t.Errorf("dash-r2 edges = %d/%d, want 0/0 -- it never advertised a link", got.edgesCurrent, got.edgesWindow)
	}
}

// insertSharedIPRouterFixture inserts a SECOND router that reuses
// insertTopologyFixture's router_ip under a DIFFERENT router_sysname. This is
// not a contrived shape: two real routers have been seen sharing
// ::ffff:10.0.103.61, two more sharing ::ffff:10.0.103.66, and two
// container-named routers sharing ::ffff:172.22.0.7 -- a device renamed, or
// a smoke-test collector run against the same address, produces it.
//
// Its Peer Up is deliberately OLDER than insertTopologyFixture's current-session
// Peer Up, AND its session id is correspondingly lower, so the per-router
// queries that scope on router_ip alone still resolve dashRouterIP's current
// session to dashSession and every other test's counts stay exactly as they
// were.
//
// Both halves of that matter, and only the first used to be true. The id was
// 9301 -- above dashSession -- while the timestamp was older, which is an
// inversion of id against time that a collector cannot produce: session ids
// come from now().UnixNano() behind a monotonic guard, and a lab archive
// carries 1,208 session transitions across 15 router views with zero such
// inversions. It went unnoticed because the dashboards resolved the session
// with argMax over ts_collector, which reads the timestamp and ignores the id.
// When that resolution was changed to max(session_id) on 2026-09-04 this
// fixture hijacked dashRouterIP's session and emptied seven topology tests.
//
// So the timestamp is now unambiguously older rather than equal (it was
// corpusCollectorClock.Add(-2h), the same instant as the current session's own
// Peer Up, which left argMax deciding on stream_seq and therefore on insertion
// order), and the id sorts the same way the clock does.
func insertSharedIPRouterFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	const (
		sysname = "dash-r1-twin"
		peerIP  = "10.255.199.4"
		// Below dashStale and dashSession both, matching a Peer Up older than
		// either.
		session = 8301
	)
	twinTS := corpusCollectorClock.Add(-4 * time.Hour)
	peerUp := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: dashRouterIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(twinTS),
		Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
			Kind: vantagev1.PeerEvent_KIND_UP,
		}},
	}
	ls := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: dashRouterIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(twinTS),
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 255, 49, 1}), Name: "twin-a"},
			},
			Links: []*vantagev1.LsLink{{
				Protocol: 3, Identifier: 100,
				Local: lsDesc([]byte{10, 255, 49, 1}), Remote: lsDesc([]byte{10, 255, 49, 2}),
				LocalIfaddr: []byte{10, 255, 49, 1}, RemoteIfaddr: []byte{10, 255, 49, 2},
				IgpMetric: 77, TeMetric: 154,
			}},
		}},
	}
	for i, ev := range []*vantagev1.Envelope{peerUp, ls} {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert shared-IP fixture envelope %d: %v", i, err)
		}
	}
}

// Two sysnames on one router_ip must not be credited with each other's
// objects. The panel keys per_router on (router_sysname, router_ip); if the
// count CTEs join and group on router_ip alone, every ls_* row for that
// address joins once per sysname, countDistinctIf sees BOTH sids inside a
// single group, and the outer join then maps that blended row onto every
// sysname sharing the address. The visible damage is worse than an inflated
// window column: current_session can be inflated to equal in_window, which is
// this panel's "the router re-dumped, trust the graph" signal, for two routers
// neither of which re-dumped.
func TestCompletenessPanelDoesNotBlendRoutersSharingAnIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)
	insertSharedIPRouterFixture(t, ctx, ch)

	byRouter := completenessByRouter(t, ctx, ch)
	orig, ok := byRouter["dash-r1"]
	if !ok {
		t.Fatalf("no row for dash-r1: %v", byRouter)
	}
	twin, ok := byRouter["dash-r1-twin"]
	if !ok {
		t.Fatalf("no row for dash-r1-twin: %v", byRouter)
	}
	if orig.routerIP != dashRouterIPv6 || twin.routerIP != dashRouterIPv6 {
		t.Fatalf("the two rows must be on the same address for this test to "+
			"mean anything: dash-r1 %q, dash-r1-twin %q", orig.routerIP, twin.routerIP)
	}
	// dash-r1's counts are exactly what they are without the twin present.
	// Joining on router_ip alone gives 7/8 here (its own 6 current plus the
	// twin's 1, and 7 distinct pairs plus the twin's 1).
	if orig.nodesCurrent != 6 || orig.nodesWindow != 7 {
		t.Errorf("dash-r1 nodes = %d/%d, want 6/7 -- dash-r1-twin's node was "+
			"counted against dash-r1", orig.nodesCurrent, orig.nodesWindow)
	}
	if orig.edgesCurrent != 5 || orig.edgesWindow != 6 {
		t.Errorf("dash-r1 edges = %d/%d, want 5/6 -- dash-r1-twin's link was "+
			"counted against dash-r1", orig.edgesCurrent, orig.edgesWindow)
	}
	if orig.sessions != 2 {
		t.Errorf("dash-r1 sessions = %d, want 2 -- the twin's session is not "+
			"dash-r1's reconnect", orig.sessions)
	}
	// And the twin's counts are its own one node and one link, not dash-r1's.
	if twin.nodesCurrent != 1 || twin.nodesWindow != 1 {
		t.Errorf("dash-r1-twin nodes = %d/%d, want 1/1 -- it advertised one node",
			twin.nodesCurrent, twin.nodesWindow)
	}
	if twin.edgesCurrent != 1 || twin.edgesWindow != 1 {
		t.Errorf("dash-r1-twin edges = %d/%d, want 1/1 -- it advertised one link",
			twin.edgesCurrent, twin.edgesWindow)
	}
	if twin.sessions != 1 {
		t.Errorf("dash-r1-twin sessions = %d, want 1", twin.sessions)
	}
}

// scalarPanel runs a single-row panel target and returns the one row's
// columns as they scan into dest.
func scalarPanel(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, panel string, dest ...any) {
	t.Helper()
	scalarPanelFor(t, ctx, ch, dashboard, panel, dashPeerIP, dest...)
}

// scalarPanelFor is scalarPanel for one chosen BMP peer.
func scalarPanelFor(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, panel, peer string, dest ...any) {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, dashboard, panel, "A"),
		topologyVarsFor(peer, "0", "0"))
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(dest...); err != nil {
		t.Fatalf("%s panel %q: %v\nSQL:\n%s", dashboard, panel, err, sql)
	}
}

// link-state-topology's three stat panels each repeat per collector, so each
// carries "$collector" in its own title -- see
// linkStateNodesCoverageStatTitle's comment for why a repeated tile has to
// name the collector it is about, and why each panel gets its own constant
// rather than one shared string.
const (
	lsTopologySessionAgeTitle = "Current session age — $collector"
	lsTopologyReconnectsTitle = "Reconnects in range — $collector"
	lsTopologyObjectsTitle    = "Objects in current session — $collector"
)

// "Current session age" answers "how long since this router last
// reconnected", which is a question about when the session BEGAN. Reading it
// off max(ts_collector) reports the age of the session's newest event
// instead, which understates the answer by the whole span of the session:
// on a lab router, the newest session spans 5497 seconds across 18 events,
// so the shipped query reported that router 91 minutes younger than it was.
//
// The fixture's current session came up dashSessionAge ago and has had a
// peer flap since, so max and min are far apart and only one of them is
// right.
func TestCurrentSessionAgeMeasuresWhenTheSessionBegan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	var age uint64
	scalarPanel(t, ctx, ch, "link-state-topology", lsTopologySessionAgeTitle, &age)

	want := uint64(dashSessionAge / time.Second)
	// The lower bound is the assertion that matters: max(ts_collector) would
	// land near zero, since the session's newest peer event is at
	// corpusCollectorClock. The upper bound only allows for the test suite's
	// own runtime.
	if age < want || age > want+600 {
		t.Errorf("current session age = %ds, want about %ds -- reading the age "+
			"off max(ts_collector) reports how long since the session's LAST "+
			"event, not how long since it began", age, want)
	}
}

// A collector clock behind the database's clock makes dateDiff negative, and
// toUInt64 of a negative is ~1.8e19 -- a stat panel reading "584942417355
// years" rather than a small wrong number. greatest(..., 0) is what keeps
// clock skew from rendering as garbage.
func TestCurrentSessionAgeClampsNegativeSkewToZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	// Exercise the clamp on the shipped expression itself rather than on a
	// paraphrase: the same SQL with now() replaced by a moment before the
	// session started is precisely the skewed case.
	sql := substituteGrafana(
		panelSQL(t, "link-state-topology", lsTopologySessionAgeTitle, "A"),
		topologyVars("0", "0"))
	if !strings.Contains(sql, "now()") {
		t.Fatalf("the session-age panel no longer calls now(); this test's "+
			"skew simulation does not apply:\n%s", sql)
	}
	insertTopologyFixture(t, ctx, ch)
	skewed := strings.Replace(sql, "now()", "now() - INTERVAL 1 DAY", 1)
	var age uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, skewed)).Scan(&age); err != nil {
		t.Fatalf("skewed session age: %v\nSQL:\n%s", err, skewed)
	}
	if age != 0 {
		t.Errorf("session age under negative skew = %d, want 0 -- toUInt64 of "+
			"a negative dateDiff wraps to about 1.8e19", age)
	}
}

// insertOldSessionRouterFixture inserts a router whose most recent peer event
// and whose only link-state rows are far older than any dashboard time range
// the tests use. This is the well-behaved router: one long-lived BMP session,
// a complete dump at the start of it, and therefore no recent peer events at
// all. Routers that reconnect constantly always fall inside the window; the
// stable one does not, which makes the selection adverse.
func insertOldSessionRouterFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	const (
		routerIP = "10.0.199.3"
		peerIP   = "10.255.199.3"
		sysname  = "dash-r3"
		session  = 9401
	)
	// Older than substituteGrafana's window by a wide margin, so this does not
	// become a test that passes on a boundary.
	old := corpusCollectorClock.Add(-30 * 24 * time.Hour)
	peerUp := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: routerIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(old),
		Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
			Kind: vantagev1.PeerEvent_KIND_UP,
		}},
	}
	ls := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: routerIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(old),
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 255, 59, 1}), Name: "old-a"},
			},
			Links: []*vantagev1.LsLink{{
				Protocol: 3, Identifier: 100,
				Local: lsDesc([]byte{10, 255, 59, 1}), Remote: lsDesc([]byte{10, 255, 59, 2}),
				LocalIfaddr: []byte{10, 255, 59, 1}, RemoteIfaddr: []byte{10, 255, 59, 2},
				IgpMetric: 88, TeMetric: 176,
			}},
		}},
	}
	for i, ev := range []*vantagev1.Envelope{peerUp, ls} {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert old-session fixture envelope %d: %v", i, err)
		}
	}
}

// A router whose newest peer event predates the dashboard time range must
// still get a row. Time-filtering per_router dropped it entirely -- no row,
// no zeros -- and the router that disappeared was the well-behaved one, since
// a stable session produces no recent peer events. Verified through Grafana's
// own API at the dashboard's default now-6h: the Node Graph drew that
// router's 7 nodes and 20 edges while the completeness table returned 7
// rows, none of them that router.
//
// A router's current session is its current session whatever the picker says,
// so per_router derives it without the filter. The WINDOW columns keep theirs,
// so they read zero here. The CURRENT-SESSION columns do not: they read the
// current-state tables, which answer "what does the session carry now", and
// a stable session carries its node and link whenever they were advertised.
// (They used to keep the filter too, which read this well-behaved router as
// carrying nothing, and disagreed with link-state-nodes' "Carried by the
// current session" stat, which was never windowed. See
// TestLinkStateDashboardsKeepLiveObjectsPastRetention for the same router
// shape past retention.)
func TestCompletenessPanelKeepsRouterWhosePeerEventsPredateTheWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertOldSessionRouterFixture(t, ctx, ch)

	byRouter := completenessByRouter(t, ctx, ch)
	got, ok := byRouter["dash-r3"]
	if !ok {
		t.Fatalf("completeness panel has no row for dash-r3, whose peer events "+
			"are older than the time range; the router that vanishes is the one "+
			"with a stable session, which is the one an operator is least "+
			"likely to suspect: %v", byRouter)
	}
	if got.routerIP != "::ffff:10.0.199.3" {
		t.Errorf("dash-r3 router_ip = %q, want ::ffff:10.0.199.3", got.routerIP)
	}
	// Window zeros: the window CTEs must keep $__timeFilter, or "in window"
	// stops meaning in window.
	if got.sessions != 0 {
		t.Errorf("dash-r3 sessions = %d, want 0 -- it did not reconnect in range", got.sessions)
	}
	if got.nodesCurrent != 1 || got.nodesWindow != 0 {
		t.Errorf("dash-r3 nodes = %d/%d, want 1/0 -- its current session carries "+
			"one node, advertised outside the window", got.nodesCurrent, got.nodesWindow)
	}
	if got.edgesCurrent != 1 || got.edgesWindow != 0 {
		t.Errorf("dash-r3 edges = %d/%d, want 1/0 -- its current session carries "+
			"one link, advertised outside the window", got.edgesCurrent, got.edgesWindow)
	}
}

// linkStateDashboards is every link-state dashboard this repo ships. The all-targets
// test below walks these rather than naming panels, so a panel added later
// is covered without anyone remembering to add it here.
var linkStateDashboards = []string{
	"link-state-topology",
	"link-state-nodes",
	"link-state-links",
	"link-state-prefixes",
}

// allDashboards is every dashboard this repo provisions. The all-targets walk
// covers EVERY file in deploy/grafana/dashboards, not just the four link-state ones:
// fleet-health, route-churn and parse-anomalies ship sixteen panels between
// them and nothing had ever run their SQL, so a renamed column reached an
// operator with CI green.
//
// A written-out count drifts the moment a dashboard is added or removed --
// exposed to it in both directions. It is pinned against the directory
// instead, by TestAllDashboardsMatchesTheProvisionedDirectory, so stating
// the rule rather than a number is what keeps this list from going stale.
var allDashboards = []string{
	"link-state-topology",
	"link-state-nodes",
	"link-state-links",
	"link-state-prefixes",
	"fleet-health",
	"route-churn",
	"parse-anomalies",
	"looking-glass",
	"looking-glass-vpn",
	"asn-view",
	"top-l3vpn-prefixes",
	"evpn-churn",
	"l3vpn-rib-browser",
}

// TestAllDashboardsMatchesTheProvisionedDirectory pins the UNIVERSE that
// every walk in this file runs over.
//
// allDashboards is a hand-written list, and it is what the debt ratchet, the
// collector-repeat guard and four other walks iterate.
// docker-compose.dev.yml mounts the whole of deploy/grafana/dashboards into
// Grafana, so PROVISIONING's universe is the directory while every test's
// universe is this slice. A dashboard added by someone who does not think to
// edit this file is therefore shipped to an operator and walked by nothing:
// no collector-repeat check, no session-scoping ratchet, and not one of its queries
// ever executed against a database.
//
// That gap matters more now, because
// TestCollectorScopedPanelsRepeatAndNameTheirCollector is the only
// enforcement of the collector-repeat rule, which escaped several separate
// hand reviews before it had a guard at all. A guard with a hole in its
// universe is the same defect the session-scoping ratchet also had: a
// guard that cannot see a thing cannot verify it.
func TestAllDashboardsMatchesTheProvisionedDirectory(t *testing.T) {
	dir := filepath.Join("..", "deploy", "grafana", "dashboards")
	// WalkDir, not Glob. Grafana's file provider walks the provisioned path as
	// a TREE, so a dashboard filed one directory down is shipped to operators
	// while a non-recursive glob reports the list as complete -- which is the
	// same hole, one level in, that this test exists to close.
	//
	// The name kept is the path RELATIVE to dir, minus the extension, so a
	// nested file reads "sub/foo" and cannot silently satisfy a flat entry
	// named "foo": every other walk in this file joins dir + name + ".json".
	var onDisk []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		onDisk = append(onDisk, strings.TrimSuffix(filepath.ToSlash(rel), ".json"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(onDisk)
	// Before the comparison, not after: two empty lists are equal, so an
	// unreadable or moved directory would otherwise report agreement while
	// every other walk in this file silently covered nothing.
	if len(onDisk) == 0 {
		t.Fatalf("%s holds no dashboard JSON, so this test compares two empty "+
			"lists and every walk over allDashboards is vacuous", dir)
	}
	listed := slices.Sorted(slices.Values(allDashboards))
	if !slices.Equal(onDisk, listed) {
		t.Errorf("allDashboards has diverged from %s.\n  on disk: %v\n"+
			"  listed:  %v\n"+
			"Every walk in this file iterates the SLICE while docker-compose "+
			"provisions the DIRECTORY, so a file missing from the list is "+
			"shipped to operators and checked by nothing, and a name listed "+
			"with no file behind it fails every walk at once", dir, onDisk, listed)
	}
}

// dashboardTarget names one query inside one committed dashboard.
type dashboardTarget struct {
	dashboard string
	panel     string
	refID     string
	rawSQL    string
}

// dashboardTargets reads every target carrying a rawSql out of one committed
// dashboard, descending into nested panels so a row-collapsed panel is not
// silently skipped.
func dashboardTargets(t *testing.T, dashboard string) []dashboardTarget {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Panels []json.RawMessage `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []dashboardTarget

	// Template variables can resolve a session too, and a $focus picker that
	// resolves one collector's session lists only that collector's nodes --
	// so the walk has to see these queries, not only panels[].targets[].
	//
	// Query is typed any because Grafana writes a variable's query as either
	// a string or an object depending on the variable type; a string field
	// would fail to unmarshal the whole dashboard on the first object-shaped
	// one.
	var tmpl struct {
		Templating struct {
			List []struct {
				Name  string `json:"name"`
				Query any    `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &tmpl); err != nil {
		t.Fatalf("parse %s templating: %v", path, err)
	}
	for _, v := range tmpl.Templating.List {
		q, ok := v.Query.(string)
		if !ok || !strings.Contains(q, "session_id") {
			continue
		}
		out = append(out, dashboardTarget{dashboard: dashboard, panel: "$" + v.Name, refID: "variable", rawSQL: q})
	}
	var walk func(panels []json.RawMessage)
	walk = func(panels []json.RawMessage) {
		for _, pr := range panels {
			var p struct {
				Title   string `json:"title"`
				Panels  []json.RawMessage
				Targets []struct {
					RefID  string `json:"refId"`
					RawSQL string `json:"rawSql"`
				} `json:"targets"`
			}
			if err := json.Unmarshal(pr, &p); err != nil {
				t.Fatalf("parse panel in %s: %v", path, err)
			}
			for _, tg := range p.Targets {
				if strings.TrimSpace(tg.RawSQL) == "" {
					continue
				}
				out = append(out, dashboardTarget{dashboard, p.Title, tg.RefID, tg.RawSQL})
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return out
}

// Every query in every shipped dashboard has to run. Three of the nine
// targets -- "Current session age", "Reconnects in range" and "Objects in
// current session" -- had no test at all, so a schema change could break a
// third of what an operator opens with CI green. The requirement is a
// test asserting that no panel query errors; this is it,
// and it discovers targets by walking the JSON so the guarantee does not
// decay as panels are added.
func TestEveryDashboardTargetRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	// The targets run one at a time, but go test runs this package's tests
	// alongside query's and api's against the same ClickHouse, and by default
	// each statement takes max_threads = every core. Four threads are enough
	// to show a target parses and runs, and keep the walk from exhausting the
	// server's thread pool (code 439) under that load.
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_threads": 4}))
	var threads string
	if err := ch.conn.QueryRow(ctx, "SELECT toString(getSetting('max_threads'))").Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if threads != "4" {
		t.Fatalf("max_threads reads %s inside the walk, want 4: the cap did not reach the server", threads)
	}

	// A real node_key, so the walk renders $focus as an operator's selection
	// and not only as All. Rendering All alone exercises one side of every
	// WHERE it visits, which is how six $focus clauses shipped unexercised.
	focus := focusValue(t, ctx, ch, "link-state-topology", dashPeerIP, "dash-a")

	var total int
	for _, dash := range allDashboards {
		targets := dashboardTargets(t, dash)
		if len(targets) == 0 {
			t.Errorf("%s: the walk found no targets in it, which is itself the "+
				"failure mode a discovery-based check has", dash)
			continue
		}
		total += len(targets)
		for _, vars := range dashboardVarCombos(t, dash, focus) {
			for _, tg := range targets {
				sql := substituteGrafana(tg.rawSQL, vars)
				rows, err := ch.conn.Query(ctx, qualify(ch, sql))
				if err != nil {
					t.Errorf("%s panel %q target %q (%v) errored: %v\nSQL:\n%s",
						tg.dashboard, tg.panel, tg.refID, vars, err, sql)
					continue
				}
				for rows.Next() {
				}
				if err := rows.Err(); err != nil {
					t.Errorf("%s panel %q target %q (%v) failed mid-stream: %v",
						tg.dashboard, tg.panel, tg.refID, vars, err)
				}
				rows.Close()
			}
		}
	}
	// A walk that finds nothing would otherwise be a passing test: the
	// failure mode of a discovery-based check is discovering zero.
	t.Logf("walked %d dashboard targets across %d dashboards", total, len(allDashboards))
	if total < 45 {
		t.Fatalf("found %d dashboard targets across %v, want at least 45 -- "+
			"the walk is not finding what ships", total, allDashboards)
	}
}

// dashboardPromTarget is one Prometheus target of one dashboard. It carries
// the legend as well as the expression because the legend references label
// names too, and a legend naming a label the metric does not carry renders
// every series with the literal text "{{stream}}" in it.
type dashboardPromTarget struct {
	dashboard, panel, refID, expr, legend string
}

// dashboardPromTargets walks one dashboard for targets aimed at Prometheus,
// taking the datasource from the target where it sets one and from the panel
// otherwise -- which is how Grafana itself resolves it, and how a panel comes
// to have a Prometheus datasource on the panel and none on the target.
//
// This walk exists because dashboardTargets, the one TestEveryDashboardTargetRuns
// is built on, skips any target with an empty rawSql -- which every
// Prometheus target has. So "every dashboard target runs" has always meant
// every SQL one, and these two have never been examined by anything.
func dashboardPromTargets(t *testing.T, dashboard string) []dashboardPromTarget {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	type datasource struct {
		Type string `json:"type"`
	}
	var d struct {
		Panels []json.RawMessage `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []dashboardPromTarget
	var walk func(panels []json.RawMessage)
	walk = func(panels []json.RawMessage) {
		for _, pr := range panels {
			var p struct {
				Title      string      `json:"title"`
				Datasource *datasource `json:"datasource"`
				Panels     []json.RawMessage
				Targets    []struct {
					RefID      string      `json:"refId"`
					Expr       string      `json:"expr"`
					Legend     string      `json:"legendFormat"`
					Datasource *datasource `json:"datasource"`
				} `json:"targets"`
			}
			if err := json.Unmarshal(pr, &p); err != nil {
				t.Fatalf("parse panel in %s: %v", path, err)
			}
			for _, tg := range p.Targets {
				ds := tg.Datasource
				if ds == nil {
					ds = p.Datasource
				}
				if ds == nil || ds.Type != "prometheus" {
					continue
				}
				out = append(out, dashboardPromTarget{
					dashboard, p.Title, tg.RefID, tg.Expr, tg.Legend})
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return out
}

// vantageMetricRe matches this project's own metric names inside a PromQL
// expression. Anchoring on the vantage_ prefix rather than parsing PromQL is
// what makes a regex sufficient here: no PromQL function, keyword or label
// name begins with it, so every match is a metric reference and every metric
// reference is a match. The cost is that a panel referencing a metric from
// somewhere else -- Prometheus's own `up`, or a JetStream exporter -- is not
// checked, which is stated rather than hidden. No shipped panel does.
var vantageMetricRe = regexp.MustCompile(`\bvantage_\w+\b`)

// promByClauseRe matches the label list of a `by (...)` or `without (...)`
// grouping; promLegendRe matches a {{label}} reference in a legend format.
var promByClauseRe = regexp.MustCompile(`\b(?:by|without)\s*\(([^)]*)\)`)
var promLegendRe = regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

// promCounterFuncRe matches the PromQL functions that are only meaningful on
// a counter.
var promCounterFuncRe = regexp.MustCompile(`\b(?:increase|rate|irate|resets)\s*\(`)

// vantageOnly narrows a registry dump to this project's own metrics. The
// default registry also carries the Go runtime's forty-odd go_* and process_*
// families, and printing those in a failure buries the one line that answers
// "what should this panel have said instead".
func vantageOnly[V any](m map[string]V) []string {
	var out []string
	for _, k := range sortedKeys(m) {
		if strings.HasPrefix(k, "vantage_") {
			out = append(out, k)
		}
	}
	return out
}

// The two Prometheus panels on fleet-health -- "Writer lag by stream" and
// "Envelopes lost to retention" -- are the only panels in the whole dashboard
// set that do not read ClickHouse, and no test had ever looked at them. They
// had been assumed to be SQL panels with a fixture already in place;
// they have no rawSql at all, so nothing about a ClickHouse fixture could
// reach them.
//
// WHAT THEY CAN GET WRONG IS A NAME. A panel whose expression references a
// metric this binary does not export renders "No data" -- and for "Envelopes
// lost to retention" that is indistinguishable from the healthy answer, which
// is the exact confusion publishLagSeries and TestRunPublishesLagSeriesBeforeAnyLoss
// exist to prevent on the code side. Both of those defend the metric. Neither
// defends the panel's spelling of it, so a rename in lag.go would leave a
// data-loss panel reading "nothing lost" forever, with every Go test green.
//
// So this compares the shipped dashboards against the registry itself rather
// than against a list restated here: the names, the label the panels group
// and legend on, and whether a metric wrapped in increase() is actually a
// counter. Three properties, all of which have a silent failure mode.
func TestEveryDashboardPrometheusTargetNamesAMetricTheSinkExports(t *testing.T) {
	// A Prometheus *Vec exports no family at all until some label set
	// touches it, so the registry has to be asked after that has happened --
	// see publishLagSeries' own comment, which is about the same property
	// biting an operator rather than a test. The probe label is this test's
	// own and matches nothing another test asserts on: seriesValue filters
	// by exact label value, and TestRunPublishesLagSeriesBeforeAnyLoss
	// requires the ABSENCE of "LS" specifically.
	publishLagSeries("dashboard-metric-probe")

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	type metric struct {
		labels  map[string]bool
		counter bool
	}
	exported := map[string]metric{}
	for _, f := range families {
		m := metric{labels: map[string]bool{}, counter: f.GetType() == dto.MetricType_COUNTER}
		for _, s := range f.GetMetric() {
			for _, lp := range s.GetLabel() {
				m.labels[lp.GetName()] = true
			}
		}
		exported[f.GetName()] = m
	}
	if _, ok := exported["vantage_sink_consumer_lag"]; !ok {
		t.Fatal("the registry exports no vantage_sink_consumer_lag even after " +
			"publishLagSeries, so this test is about to find every panel " +
			"wrong for a reason that is its own fault")
	}

	var checked int
	for _, dash := range allDashboards {
		for _, tg := range dashboardPromTargets(t, dash) {
			if strings.TrimSpace(tg.expr) == "" {
				t.Errorf("%s panel %q target %q is aimed at Prometheus and "+
					"carries no expr", tg.dashboard, tg.panel, tg.refID)
				continue
			}
			names := vantageMetricRe.FindAllString(tg.expr, -1)
			if len(names) == 0 {
				t.Errorf("%s panel %q target %q references no vantage_ metric: "+
					"%s", tg.dashboard, tg.panel, tg.refID, tg.expr)
				continue
			}
			checked++

			var found []metric
			for _, n := range names {
				m, ok := exported[n]
				if !ok {
					t.Errorf("%s panel %q target %q queries %q, which this "+
						"binary does not export. The panel renders \"No data\" "+
						"-- which on a loss counter reads exactly like \"nothing "+
						"lost\". This binary's own metrics: %v",
						tg.dashboard, tg.panel, tg.refID, n, vantageOnly(exported))
					continue
				}
				found = append(found, m)
				if promCounterFuncRe.MatchString(tg.expr) && !m.counter {
					t.Errorf("%s panel %q target %q wraps %q in a counter-only "+
						"function, but it is exported as a gauge: %s",
						tg.dashboard, tg.panel, tg.refID, n, tg.expr)
				}
			}

			// Every label the expression groups on, or the legend names, has
			// to be one the metric actually carries. A legend naming a label
			// that is not there does not fail: it renders the literal text
			// "{{stream}}" once per series, which is a legend that has lost
			// the one thing it was for.
			var labels []string
			for _, m := range promByClauseRe.FindAllStringSubmatch(tg.expr, -1) {
				for _, l := range strings.Split(m[1], ",") {
					if l = strings.TrimSpace(l); l != "" {
						labels = append(labels, l)
					}
				}
			}
			for _, m := range promLegendRe.FindAllStringSubmatch(tg.legend, -1) {
				labels = append(labels, m[1])
			}
			for _, l := range labels {
				carried := false
				for _, m := range found {
					if m.labels[l] {
						carried = true
					}
				}
				if !carried {
					t.Errorf("%s panel %q target %q groups or legends on label "+
						"%q, which no metric it queries (%v) carries",
						tg.dashboard, tg.panel, tg.refID, l, names)
				}
			}
		}
	}
	// The failure mode of a discovery-based check is discovering zero -- and
	// this one is a walk over JSON that another walk in this same file
	// already skips silently.
	if checked < 2 {
		t.Fatalf("examined %d Prometheus targets across %v, want at least 2 "+
			"-- the walk is not finding what ships", checked, allDashboards)
	}
}

// dashboardVarCombos returns every combination of variable renderings to try
// against one dashboard: each variable it declares, crossed with the values a
// real selection can produce.
//
// An unknown variable is a hard failure rather than a skip. A dashboard that
// gains a control nobody exercises is exactly how $peer and $focus both came
// to ship broken, and a walk that quietly ignored the new name would report
// full coverage of it.
func dashboardVarCombos(t *testing.T, dashboard, focus string) []map[string]string {
	t.Helper()
	values := map[string][]string{
		"router": {dashRouterIP},
		"peer":   {dashPeerIP},
		"state":  {"0", "0, 1"},
		"focus":  {"0", focus},
		// insertTopologyFixture, the fixture this walk uses, carries one
		// collector, "c1". A single value is the right shape to test here even
		// though the picker is multi-valued: Grafana's repeat hands a repeated
		// panel one concrete collector per instance, never a list.
		"collector": {"c1"},
		// ".*" is $rib's allValue, which is a regex because the panels test it
		// with match(toString(rib), '^($rib)$') rather than equality.
		"rib": {".*", "in_pre", "loc_rib"},
		// Empty is the picker's All; the other two are the branches an
		// operator reaches first, and the second one must not raise.
		"target": {"", "10.77.14.7", "not-an-ip"},
		// ".*" is $family's allValue. The other two are the two shapes
		// route_vpn actually holds -- a family WITH a Route Distinguisher and
		// one without -- because the RD-absent branch is the one that renders
		// a fallback string rather than a blank.
		"family": {".*", "vpn4", "lu4"},
		// ".*" is $asn's allValue. The other two are the two ROLES an ASN
		// can have -- 64512 only ever originates, 65001 only ever transits --
		// because the panels branch on as_path[-1] and rendering one role
		// alone leaves the other side of that branch unexercised.
		"asn": {".*", "64512", "65001"},
		// ".*" is $route_type's allValue. 2 and 5 are the two EVPN shapes
		// whose identity comes from DIFFERENT columns -- a MAC and a prefix
		// -- so rendering only one leaves half the multiIf unexercised.
		"route_type": {".*", "2", "5"},
		// ".*" is $rd's allValue. The third value is the EMPTY Route
		// Distinguisher, which is a real selection rather than an absent one:
		// labeled unicast carries no RD, and a panel whose filter cannot
		// render '' reaches none of those rows.
		"rd": {".*", "65000:51", ""},
	}
	combos := []map[string]string{{}}
	for _, name := range dashboardVariableNames(t, dashboard) {
		vals, ok := values[name]
		if !ok {
			t.Fatalf("%s declares $%s and this walk has no rendering for it. "+
				"Add one here rather than leaving the variable unexercised.",
				dashboard, name)
		}
		var next []map[string]string
		for _, base := range combos {
			for _, v := range vals {
				m := make(map[string]string, len(base)+1)
				maps.Copy(m, base)
				m[name] = v
				next = append(next, m)
			}
		}
		combos = next
	}
	return combos
}

// "Reconnects in range" is one of the three targets nothing ran. It counts
// distinct BMP sessions from the selected router's address inside the time
// range -- the address, not the sysname, because $router is an address and
// two sysnames can share one.
//
// Both fixtures that put peer_events under dashRouterIP are inserted here
// explicitly rather than relied on from an earlier test, so the expected
// count is the same whether this runs alone or in the whole suite.
func TestReconnectsInRangeCountsSessionsForTheSelectedRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)
	insertSharedIPRouterFixture(t, ctx, ch)

	var sessions uint64
	scalarPanel(t, ctx, ch, "link-state-topology", lsTopologyReconnectsTitle, &sessions)
	if sessions != 3 {
		t.Errorf("reconnects in range = %d, want 3 (dashSession, dashStale, and "+
			"the shared-address twin's session -- all inside the window)", sessions)
	}
}

// "Objects in current session" is the stat that sits under the Node Graph and
// says how much of the topology this session actually carries. It counted
// nothing until now.
//
// 4 edges, not the 3 the graph draws: like the completeness table, this
// counts what the session carried, withdrawals included, because "did this
// router re-dump" is a question about message volume. The withdrawn C->A is
// the difference.
func TestObjectsInCurrentSessionCountsThisSessionOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	var nodes, edges uint64
	scalarPanel(t, ctx, ch, "link-state-topology", lsTopologyObjectsTitle, &nodes, &edges)
	// 5, not 4: the panel counts link-state OBJECTS, and the LAN pseudonode
	// is one. It is marked in the graph rather than subtracted here -- a
	// count that quietly dropped it would disagree with the frame beside it.
	if nodes != 5 {
		t.Errorf("objects in current session: nodes = %d, want 5 (A, B, C, D + "+
			"the LAN pseudonode) -- 6 means either the stale session or the "+
			"second peer leaked in", nodes)
	}
	if edges != 4 {
		t.Errorf("objects in current session: edges = %d, want 4 (A->B, B->C, "+
			"A->E, C->A) -- 5 means either the stale session or the second peer "+
			"leaked in", edges)
	}
}

// nodeTableRow is one row of the committed link-state-nodes table. Scanning
// in one place keeps the column list from being restated per test, which is
// how a newly added column ends up asserted nowhere.
type nodeTableRow struct {
	routerID     string
	routerIDv4   string
	protocol     uint8
	area         uint32
	asn          uint32
	srgb         string
	srlb         string
	srAlgorithms string
	unknownTLVs  uint64
	lastSeen     time.Time
}

// nodesTable runs the committed nodes table and returns its rows keyed by the
// rendered node title.
func nodesTable(t *testing.T, ctx context.Context, ch *ClickHouse) map[string]nodeTableRow {
	t.Helper()
	return nodesTableFor(t, ctx, ch, dashPeerIP, "0")
}

// nodesTableFor is nodesTable for one chosen BMP peer and one $focus selection
// ("0" being the picker's All).
func nodesTableFor(t *testing.T, ctx context.Context, ch *ClickHouse, peer, focus string) map[string]nodeTableRow {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, "link-state-nodes", "Nodes", "A"),
		topologyVarsFor(peer, focus, "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("nodes table: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	out := map[string]nodeTableRow{}
	for rows.Next() {
		// collector is the panel's new first column. Every caller here selects
		// a single collector's fixture, so the map stays keyed on node alone --
		// TestDualHomedTablesKeepBothCollectors is what asserts the
		// per-collector value.
		var collector, node string
		var r nodeTableRow
		if err := rows.Scan(&collector, &node, &r.routerID, &r.routerIDv4, &r.protocol, &r.area,
			&r.asn, &r.srgb, &r.srlb, &r.srAlgorithms, &r.unknownTLVs, &r.lastSeen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[node] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// linkTableRow is one row of the committed link-state-links table.
type linkTableRow struct {
	localIfaddr  string
	linkLocalID  uint32
	remoteIfaddr string
	linkRemoteID uint32
	igpMetric    uint32
	teMetric     uint32
	adminGroup   uint32
	maxBandwidth float32
	adjSIDs      string
	lastSeen     time.Time
}

// linksTable runs the committed links table and returns its rows keyed by
// "local -> remote".
func linksTable(t *testing.T, ctx context.Context, ch *ClickHouse) map[string]linkTableRow {
	t.Helper()
	return linksTableFor(t, ctx, ch, dashPeerIP, "0")
}

// linksTableFor is linksTable for one chosen BMP peer and one $focus selection.
func linksTableFor(t *testing.T, ctx context.Context, ch *ClickHouse, peer, focus string) map[string]linkTableRow {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, "link-state-links", "Links", "A"),
		topologyVarsFor(peer, focus, "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("links table: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	out := map[string]linkTableRow{}
	for rows.Next() {
		// collector is the panel's leading column. Every caller here selects a
		// single collector's fixture, so the map stays keyed on the pair alone --
		// TestDualHomedTablesKeepBothCollectors is what asserts the per-collector
		// value.
		var collector, local, remote string
		var r linkTableRow
		if err := rows.Scan(&collector, &local, &r.localIfaddr, &r.linkLocalID,
			&remote, &r.remoteIfaddr, &r.linkRemoteID,
			&r.igpMetric, &r.teMetric, &r.adminGroup, &r.maxBandwidth,
			&r.adjSIDs, &r.lastSeen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[local+" -> "+remote] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// The nodes table must show every advertised node in the current session --
// including the one with no name and no router_id_v4, whose row would
// otherwise be an unreadable hex string. Four nodes, not five: the
// endpoint-only node E is not an advertised node and does not belong in a
// table of what the router said.
func TestNodesTableListsAdvertisedNodesOfCurrentSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byNode := nodesTable(t, ctx, ch)
	if len(byNode) != 5 {
		t.Fatalf("nodes table returned %d rows, want 5 advertised nodes: %v", len(byNode), byNode)
	}
	// The pseudonode is listed by its hex router-id: it is advertised, so the
	// table shows it, and its 8-byte width is what says it is a LAN.
	for _, want := range []string{"dash-a", "dash-d", "10.255.29.2", "10.255.19.3", "0aff13020a001301"} {
		if _, ok := byNode[want]; !ok {
			t.Errorf("node %q missing from the table: %v", want, byNode)
		}
	}
	// dash-stale belongs to the stale session (dashStale), which advertises
	// its own node purely so this filter has something to exclude. Its
	// presence here would mean the session_id = current_session filter was
	// not applied.
	if _, ok := byNode["dash-stale"]; ok {
		t.Errorf("node %q present; the current-session filter did not exclude the stale session's node: %v", "dash-stale", byNode)
	}
}

// SRGB/SRLB ranges, SR algorithms and an unknown-TLV count are
// among what the nodes table owes an operator; only SRGB shipped. These are
// asserted on their values, not merely selected: a column that renders the
// wrong field, or renders an SRLB as an SRGB, would still return a row.
func TestNodesTableRendersSegmentRoutingAttributes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byNode := nodesTable(t, ctx, ch)
	a, ok := byNode["dash-a"]
	if !ok {
		t.Fatalf("no row for dash-a: %v", byNode)
	}
	if a.srgb != "16000-23999" {
		t.Errorf("dash-a srgb = %q, want \"16000-23999\" (base 16000, size 8000)", a.srgb)
	}
	if a.srlb != "15000-15999" {
		t.Errorf("dash-a srlb = %q, want \"15000-15999\" (base 15000, size 1000) -- "+
			"an SRLB rendered from the SRGB columns would read 16000-23999", a.srlb)
	}
	if a.srAlgorithms != "0, 1" {
		t.Errorf("dash-a sr_algorithms = %q, want \"0, 1\"", a.srAlgorithms)
	}
	// A count, not a dump: the value of an unrecognized TLV is bytes, and a
	// table cell is not where an operator decodes it. One is enough to tell
	// them this build did not understand everything the router said.
	if a.unknownTLVs != 1 {
		t.Errorf("dash-a unknown_tlv_count = %d, want 1", a.unknownTLVs)
	}
	// dash-d advertises no SR attributes at all, so an accidentally constant
	// column shows up here.
	d, ok := byNode["dash-d"]
	if !ok {
		t.Fatalf("no row for dash-d: %v", byNode)
	}
	if d.srgb != "" || d.srlb != "" || d.srAlgorithms != "" || d.unknownTLVs != 0 {
		t.Errorf("dash-d SR columns = srgb %q / srlb %q / algorithms %q / unknown %d, "+
			"want all empty -- it advertised none of them",
			d.srgb, d.srlb, d.srAlgorithms, d.unknownTLVs)
	}
}

// ls_links carries no v4 columns, so both endpoints must be decoded from
// hex here or the table is unreadable. The A->E link is the one that
// matters: E is never advertised as a node, so nothing else in the schema
// can supply its address.
func TestLinksTableDecodesBothEndpoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byPair := linksTable(t, ctx, ch)
	if len(byPair) != 3 {
		t.Fatalf("links table returned %d rows, want 3 active links: %v", len(byPair), byPair)
	}
	for _, want := range []string{
		"10.255.19.1 -> 10.255.19.2",
		"10.255.19.2 -> 10.255.19.3",
		"10.255.19.1 -> 10.255.19.5",
	} {
		if _, ok := byPair[want]; !ok {
			t.Errorf("link %q missing or left as hex: %v", want, byPair)
		}
	}
}

// Interface addresses and link IDs are in the panel's GROUP BY -- they are
// part of a link's identity -- but shipped as columns on nothing. They are
// the only fields that tell two parallel links apart, so without them the
// table renders one row twice with no way to see which is which.
func TestLinksTableShowsInterfaceAddressesAndLinkIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byPair := linksTable(t, ctx, ch)
	ab, ok := byPair["10.255.19.1 -> 10.255.19.2"]
	if !ok {
		t.Fatalf("no row for A->B: %v", byPair)
	}
	if ab.localIfaddr != "10.255.19.1" || ab.remoteIfaddr != "10.255.19.2" {
		t.Errorf("A->B ifaddrs = %q / %q, want 10.255.19.1 / 10.255.19.2",
			ab.localIfaddr, ab.remoteIfaddr)
	}
	// The fixture derives link IDs from the endpoints' last octet times 100,
	// so a column reading the wrong one of the pair is visible rather than
	// masked by both being zero.
	if ab.linkLocalID != 100 || ab.linkRemoteID != 200 {
		t.Errorf("A->B link IDs = %d / %d, want 100 / 200", ab.linkLocalID, ab.linkRemoteID)
	}
	bc, ok := byPair["10.255.19.2 -> 10.255.19.3"]
	if !ok {
		t.Fatalf("no row for B->C: %v", byPair)
	}
	if bc.localIfaddr != "10.255.19.2" || bc.remoteIfaddr != "10.255.19.3" {
		t.Errorf("B->C ifaddrs = %q / %q, want 10.255.19.2 / 10.255.19.3",
			bc.localIfaddr, bc.remoteIfaddr)
	}
	if bc.linkLocalID != 200 || bc.linkRemoteID != 300 {
		t.Errorf("B->C link IDs = %d / %d, want 200 / 300", bc.linkLocalID, bc.linkRemoteID)
	}
}

// A->B carries two adjacency SIDs; the other two active links carry none.
// The links table's adj_sids column ships an arrayStringConcat join that
// was previously unexercised on a populated array -- this proves both
// that the join renders "24001, 24002" (not just that it doesn't crash on
// empty) and that the column isn't accidentally populated for every row.
func TestLinksTableRendersAdjSidsForThePopulatedLinkOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byPair := linksTable(t, ctx, ch)
	adjSIDsByPair := map[string]string{}
	for pair, row := range byPair {
		adjSIDsByPair[pair] = row.adjSIDs
	}
	if got := adjSIDsByPair["10.255.19.1 -> 10.255.19.2"]; got != "24001, 24002" {
		t.Errorf("A->B adj_sids = %q, want \"24001, 24002\"", got)
	}
	for _, pair := range []string{
		"10.255.19.2 -> 10.255.19.3",
		"10.255.19.1 -> 10.255.19.5",
	} {
		if got := adjSIDsByPair[pair]; got != "" {
			t.Errorf("%s adj_sids = %q, want empty -- adj_sids must not be "+
				"populated for a link that carries none", pair, got)
		}
	}
}

// prefixTableRow is one row of the committed link-state-prefixes table.
type prefixTableRow struct {
	origin        string
	prefixSID     string
	ospfRouteType uint8
	prefixMetric  uint32
	lastSeen      time.Time
}

// prefixesTable runs the committed prefixes table for the fixture's first peer.
func prefixesTable(t *testing.T, ctx context.Context, ch *ClickHouse) map[string]prefixTableRow {
	t.Helper()
	return prefixesTableFor(t, ctx, ch, dashPeerIP, "0")
}

// prefixesTableFor is prefixesTable for one chosen BMP peer and one $focus
// selection.
func prefixesTableFor(t *testing.T, ctx context.Context, ch *ClickHouse, peer, focus string) map[string]prefixTableRow {
	t.Helper()
	sql := substituteGrafana(
		panelSQL(t, "link-state-prefixes", "Prefixes", "A"),
		topologyVarsFor(peer, focus, "0"))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("prefixes table: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	out := map[string]prefixTableRow{}
	for rows.Next() {
		// collector is the panel's leading column. Every caller here selects a
		// single collector's fixture, so the map stays keyed on the prefix
		// alone -- TestDualHomedTablesKeepBothCollectors is what asserts the
		// per-collector value.
		var collector, prefix string
		var r prefixTableRow
		if err := rows.Scan(&collector, &prefix, &r.origin, &r.prefixSID, &r.ospfRouteType,
			&r.prefixMetric, &r.lastSeen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[prefix] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// A prefix is only useful next to the node that originated it, and
// has_prefix_sid must be distinguishable from a SID of zero -- which is why
// one fixture prefix carries a SID and the other carries none at all.
func TestPrefixesTableJoinsPrefixToItsOriginatingNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	byPrefix := prefixesTable(t, ctx, ch)
	sidByPrefix := map[string]string{}
	originByPrefix := map[string]string{}
	for prefix, row := range byPrefix {
		sidByPrefix[prefix] = row.prefixSID
		originByPrefix[prefix] = row.origin
	}
	if got := originByPrefix["10.199.1.0/24"]; got != "dash-a" {
		t.Errorf("10.199.1.0/24 originated by %q, want dash-a -- the prefix "+
			"did not join to its node on node_key", got)
	}
	if got := sidByPrefix["10.199.1.0/24"]; got != "16001" {
		t.Errorf("10.199.1.0/24 prefix-SID = %q, want 16001", got)
	}
	if got := sidByPrefix["10.199.2.0/24"]; got != "" {
		t.Errorf("10.199.2.0/24 prefix-SID = %q, want empty -- a prefix with "+
			"no SID TLV must not read as a SID of 0", got)
	}
	// 10.199.9.0/24 belongs to the stale session's dash-stale node. Its
	// presence here would mean the AND session_id = (SELECT sid FROM
	// current_session) filter in the ls_prefixes subquery was not applied.
	if _, present := originByPrefix["10.199.9.0/24"]; present {
		t.Errorf("10.199.9.0/24 present; the current-session filter did not "+
			"exclude the stale session's prefix: %v", originByPrefix)
	}
}

// $peer is not a convenience: the target network
// has three route reflectors and averaging their views together would hide
// exactly the disagreement an operator is looking for. Until now every
// fixture router had one peer, so deleting "AND peer_ip = toIPv6('$peer')"
// from every panel changed no test: the control shipped entirely unexercised.
//
// A second BGP peer now advertises its own node, link and prefix inside the
// SAME router and the SAME BMP session, so peer scoping is the only thing
// that can separate the two views.
func TestPanelsShowOnlyTheSelectedPeersView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	t.Run("nodes table", func(t *testing.T) {
		a := nodesTableFor(t, ctx, ch, dashPeerIP, "0")
		if _, leaked := a["dash-f"]; leaked {
			t.Errorf("peer A's nodes table contains dash-f, which only peer B "+
				"advertised: %v", a)
		}
		b := nodesTableFor(t, ctx, ch, dashPeerIPB, "0")
		if len(b) != 1 {
			t.Fatalf("peer B's nodes table returned %d rows, want 1 (dash-f): %v", len(b), b)
		}
		if _, ok := b["dash-f"]; !ok {
			t.Errorf("peer B's nodes table is missing dash-f: %v", b)
		}
	})

	t.Run("links table", func(t *testing.T) {
		const fg = "10.255.19.11 -> 10.255.19.12"
		a := linksTableFor(t, ctx, ch, dashPeerIP, "0")
		if _, leaked := a[fg]; leaked {
			t.Errorf("peer A's links table contains %s, which only peer B "+
				"advertised: %v", fg, a)
		}
		b := linksTableFor(t, ctx, ch, dashPeerIPB, "0")
		if len(b) != 1 {
			t.Fatalf("peer B's links table returned %d rows, want 1 (%s): %v", len(b), fg, b)
		}
		if _, ok := b[fg]; !ok {
			t.Errorf("peer B's links table is missing %s: %v", fg, b)
		}
	})

	t.Run("prefixes table", func(t *testing.T) {
		a := prefixesTableFor(t, ctx, ch, dashPeerIP, "0")
		if _, leaked := a["10.199.11.0/24"]; leaked {
			t.Errorf("peer A's prefixes table contains 10.199.11.0/24, which "+
				"only peer B advertised: %v", a)
		}
		b := prefixesTableFor(t, ctx, ch, dashPeerIPB, "0")
		if len(b) != 1 {
			t.Fatalf("peer B's prefixes table returned %d rows, want 1: %v", len(b), b)
		}
		got, ok := b["10.199.11.0/24"]
		if !ok {
			t.Fatalf("peer B's prefixes table is missing 10.199.11.0/24: %v", b)
		}
		// The origin proves the panel's nodeset CTE is peer-scoped too, not
		// only its ls_prefixes subquery: dash-f is peer B's node.
		if got.origin != "dash-f" {
			t.Errorf("10.199.11.0/24 originated by %q, want dash-f", got.origin)
		}
	})

	t.Run("topology frames", func(t *testing.T) {
		if got := len(edgesFrameFor(t, ctx, ch, dashPeerIP, "0", "0")); got != 3 {
			t.Errorf("peer A's edges frame returned %d edges, want 3", got)
		}
		if got := len(edgesFrameFor(t, ctx, ch, dashPeerIPB, "0", "0")); got != 1 {
			t.Errorf("peer B's edges frame returned %d edges, want 1 (F->G)", got)
		}
		aTitles, _ := nodeFrameFor(t, ctx, ch, dashPeerIP, "0", "0")
		for _, title := range aTitles {
			if title == "dash-f" {
				t.Errorf("peer A's nodes frame contains dash-f: %v", aTitles)
			}
		}
		// Two: dash-f, plus G, which exists only as the far end of F->G.
		bTitles, _ := nodeFrameFor(t, ctx, ch, dashPeerIPB, "0", "0")
		if len(bTitles) != 2 {
			t.Errorf("peer B's nodes frame returned %d nodes, want 2 (dash-f + "+
				"its endpoint-only neighbour): %v", len(bTitles), bTitles)
		}
	})

	t.Run("objects in current session", func(t *testing.T) {
		var nodes, edges uint64
		scalarPanelFor(t, ctx, ch, "link-state-topology", lsTopologyObjectsTitle,
			dashPeerIPB, &nodes, &edges)
		if nodes != 1 || edges != 1 {
			t.Errorf("peer B objects = %d nodes / %d edges, want 1/1 -- this stat "+
				"sits under the Node Graph and has to agree with it", nodes, edges)
		}
	})
}

// Grafana's ClickHouse datasource takes a variable query's FIRST column as the
// value it substitutes into panel SQL, and the second as the label it shows in
// the picker. It does NOT honor the __text/__value aliases that Grafana's own
// documentation describes for other datasources.
//
// That cost a shipped, reviewed, manually-verified dashboard set: every
// $router query selected `router_sysname AS __text` first, so every panel
// interpolated to `toIPv6('dash-r1')` and the whole dashboard failed with
// "Cannot parse IPv6 dash-r1". The Node Graph surfaced it as "id field is
// required for nodes data frame", which names neither the variable nor the
// router and sends you looking at the wrong layer entirely.
//
// Nothing in this file could catch that, because every other test substitutes
// variable values by hand -- interpolation is a browser-side step none of them
// exercise. This test closes that gap: it runs each variable's own query, takes
// the first column exactly as the plugin would, and asserts the result survives
// the conversion the panels apply to it (toIPv6 for $router/$peer, toUInt64 for
// $focus). Wrong column order fails it immediately.
func TestVariableQueriesYieldValuesThePanelsCanConsume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	for _, dash := range []string{
		"link-state-topology", "link-state-nodes",
		"link-state-links", "link-state-prefixes",
	} {
		t.Run(dash, func(t *testing.T) {
			conv := panelVariableConversions(t, dash)
			vars := dashboardQueryVariables(t, dash)
			if len(vars) == 0 {
				t.Fatalf("%s declares no query variables; the walk found nothing "+
					"to check, which is itself the bug this test guards", dash)
			}
			for name, query := range vars {
				fn, used := conv[name]
				if !used {
					continue // not consumed via a typed conversion; nothing to assert
				}
				// The variable queries are chained: $peer and $focus filter on
				// $router. Substitute it the way Grafana would.
				sql := substituteGrafana(query, topologyVars("0", "0"))

				first := firstColumnName(t, ctx, ch, sql)
				checked := fmt.Sprintf(
					"SELECT count() AS total, countIf(%sOrNull(v) IS NULL) AS unusable "+
						"FROM (SELECT toString(%s) AS v FROM (%s))",
					fn, first, sql)
				var total, unusable uint64
				if err := ch.conn.QueryRow(ctx, qualify(ch, checked)).Scan(&total, &unusable); err != nil {
					t.Fatalf("%s $%s: %v\nSQL:\n%s", dash, name, err, checked)
				}
				if total == 0 {
					t.Fatalf("%s $%s returned no options, so this assertion proved "+
						"nothing; the fixture must supply at least one", dash, name)
				}
				if unusable != 0 {
					t.Errorf("%s $%s: %d of %d values in the FIRST column (%s) are not "+
						"%s-parseable, so every panel interpolating '$%s' errors. "+
						"The plugin uses column 1 as the value regardless of __text/__value "+
						"aliases -- put the value column first.",
						dash, name, unusable, total, first, fn, name)
				}
			}
		})
	}
}

// TestCollectorPickerQueriesOfferEveryCollectorWatchingTheRouter runs the one
// control the rest of the collector-id slice hangs on. Nothing else did.
//
// dashboardTargets only admits a template variable whose query contains
// "session_id" -- which is what pulls the four $focus pickers into
// TestEveryDashboardTargetRuns while keeping $router, $peer, $rd and $family
// out of its combinatorial walk -- and none of the six $collector queries
// mentions a session at all. TestVariableQueriesYieldValuesThePanelsCanConsume
// skips them too: it checks only variables a panel consumes through a typed
// conversion (toIPv6, toUInt64), and $collector is interpolated as a bare
// quoted string. So the picker was verified by one browser session and by
// nothing in CI.
//
// Loosening either filter is the wrong fix. The "session_id" test is what
// keeps TestEveryDashboardTargetRuns' variable crossing finite; this is
// twenty lines instead.
//
// What breaks if the picker does is the ORIGINAL defect, silently restored:
// every repeated stat on these six dashboards repeats over $collector, and
// Grafana renders a repeat whose variable has no value ONCE, unsubstituted.
// An operator then sees a single tile for a router two collectors watch, with
// a picker above it implying otherwise.
//
// The assertion is not "the query runs". The dual-homed router is the reason
// the picker exists, so the answer has to name BOTH of its collectors: a
// query that resolved a session, or that filtered to one collector, returns
// one row here and would satisfy a smoke test.
func TestCollectorPickerQueriesOfferEveryCollectorWatchingTheRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	checked := 0
	for _, dash := range allDashboards {
		query, declared := dashboardQueryVariables(t, dash)["collector"]
		if !declared {
			continue
		}
		checked++
		t.Run(dash, func(t *testing.T) {
			// The four link-state pickers filter on $router; the two looking
			// glasses have no $router control and list every collector in
			// peer_events. "Offers both of this router's" is the assertion
			// that holds for either shape.
			sql := substituteGrafana(query, dualHomedVars("0", "0"))
			rows, err := ch.conn.Query(ctx, qualify(ch, sql))
			if err != nil {
				t.Fatalf("%s $collector: %v\nSQL:\n%s", dash, err, sql)
			}
			defer rows.Close()
			cols := rows.Columns()
			if len(cols) == 0 {
				t.Fatalf("%s $collector returned no columns:\n%s", dash, sql)
			}
			// Column 0 is the VALUE Grafana's ClickHouse datasource
			// substitutes, whatever it is aliased to -- the same rule
			// TestVariableQueriesYieldValuesThePanelsCanConsume is built on.
			var got []string
			for _, row := range dashRowsAsStrings(t, rows) {
				got = append(got, row[0])
			}
			if len(got) == 0 {
				t.Fatalf("%s $collector returned no rows, so every panel "+
					"repeating over it renders once, unsubstituted, and filters "+
					"on the literal text $collector\nSQL:\n%s", dash, sql)
			}
			for _, v := range got {
				if v == "" {
					t.Errorf("%s $collector offers an EMPTY value in its first "+
						"column (%s) among %v. A tile repeated on that value "+
						"filters collector_id = '' and counts nothing, under a "+
						"heading naming no collector", dash, cols[0], got)
				}
			}
			for _, want := range []string{dualCollectorA, dualCollectorB} {
				if !slices.Contains(got, want) {
					t.Errorf("%s $collector offers %v, which does not include "+
						"%s. Both collectors watch %s, and a picker that omits "+
						"one of them means the repeat never renders that "+
						"collector's tile -- half the router's view missing "+
						"from the dashboard with nothing on screen saying so",
						dash, got, want, dualHomedRouterIP)
				}
			}
		})
	}
	// The failure mode of a discovery-based check is discovering zero, and
	// this walk's filter is a variable NAME: rename the picker and it checks
	// nothing while every repeated panel still references $collector.
	if checked < 6 {
		t.Fatalf("found a $collector picker on %d dashboards, want at least 6 "+
			"-- the four link-state dashboards and the two looking glasses. "+
			"TestCollectorScopedPanelsRepeatAndNameTheirCollector is what says "+
			"which panels need one", checked)
	}
}

// dashboardQueryVariables returns the name -> query SQL of every query-type
// template variable in a committed dashboard.
func dashboardQueryVariables(t *testing.T, dashboard string) map[string]string {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Templating struct {
			List []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Query any    `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, v := range d.Templating.List {
		if v.Type != "query" {
			continue
		}
		if s, ok := v.Query.(string); ok && strings.TrimSpace(s) != "" {
			out[v.Name] = s
		}
	}
	return out
}

// dashboardVariableMultiplicity returns one template variable's multi-value
// and include-All flags, and whether the dashboard declares the variable at
// all.
//
// Grafana hands a REPEATED panel one concrete value per instance, so a repeat
// only ever produces more than one tile when the variable behind it can hold
// more than one value at once. multi and includeAll are what make that true,
// and no test in this package had read either until
// TestCollectorScopedPanelsRepeatAndNameTheirCollector needed them:
// they are the difference between "two collectors, two tiles" and one tile
// that silently answers for whichever collector the picker happens to hold.
//
// Both are reported rather than asserted here so the caller can name which
// one is missing -- they fail differently. Without multi the picker cannot
// hold two values and the repeat renders once; with multi but without
// includeAll an operator has to select both collectors by hand and the
// dashboard's default view is still one collector's.
func dashboardVariableMultiplicity(t *testing.T, dashboard, name string) (multi, includeAll, found bool) {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Templating struct {
			List []struct {
				Name       string `json:"name"`
				Multi      bool   `json:"multi"`
				IncludeAll bool   `json:"includeAll"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, v := range d.Templating.List {
		if v.Name == name {
			return v.Multi, v.IncludeAll, true
		}
	}
	return false, false, false
}

var typedVarUse = regexp.MustCompile(`(toIPv6|toUInt64)\('\$(\w+)'\)`)

// panelVariableConversions reports, per variable name, the ClickHouse
// conversion the dashboard's panels apply to it -- the contract its picker
// values have to satisfy.
func panelVariableConversions(t *testing.T, dashboard string) map[string]string {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Panels []struct {
			Targets []struct {
				RawSQL string `json:"rawSql"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			for _, m := range typedVarUse.FindAllStringSubmatch(tg.RawSQL, -1) {
				out[m[2]] = m[1]
			}
		}
	}
	return out
}

// firstColumnName returns the name of a query's first result column -- the one
// the ClickHouse datasource turns into the variable's value.
func firstColumnName(t *testing.T, ctx context.Context, ch *ClickHouse, sql string) string {
	t.Helper()
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("variable query: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	cols := rows.Columns()
	if len(cols) == 0 {
		t.Fatalf("variable query returned no columns:\n%s", sql)
	}
	return "`" + cols[0] + "`"
}

// focusOptions runs a committed dashboard's $focus picker query the way
// Grafana would and returns label -> value, taking column 0 as the value and
// column 1 as the label exactly as the ClickHouse plugin does regardless of
// __text/__value aliases.
//
// It reads the picker rather than ls_nodes.node_key directly because the
// value a panel interpolates is whatever the variable query's first column
// produced. That seam is where the $router defect lived, and a test that
// invents its own node_key would step straight over it.
func focusOptions(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, peer string) map[string]string {
	t.Helper()
	return focusOptionsOn(t, ctx, ch, dashboard, dashRouterIP, peer, "0")
}

// focusOptionsOn is focusOptions with the advertising router and the rendered
// $state chosen, for the fixture that reserves a router of its own.
func focusOptionsOn(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, router, peer, state string) map[string]string {
	t.Helper()
	sql := substituteGrafana(dashboardVariableQuery(t, dashboard, "focus"),
		topologyVarsOn(router, peer, "0", state))
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("%s $focus picker: %v\nSQL:\n%s", dashboard, err, sql)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var value, text string
		if err := rows.Scan(&value, &text); err != nil {
			t.Fatalf("%s $focus picker scan: %v", dashboard, err)
		}
		if prev, dup := out[text]; dup && prev != value {
			t.Errorf("%s $focus offers the label %q for two different values "+
				"(%s and %s); an operator picking it cannot tell which node "+
				"they get", dashboard, text, prev, value)
		}
		out[text] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s $focus picker: %v", dashboard, err)
	}
	return out
}

// labels returns the sorted keys of a picker's label -> value map, for
// comparing an offered option set against the one the panels can render.
func labels(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// $focus is a picker of nodes, and nothing has ever asserted what it offers
// -- only that its first column parses as a UInt64
// (TestVariableQueriesYieldValuesThePanelsCanConsume). Every panel it feeds
// is scoped three ways: to $router, to $peer, and to the current BMP
// session. The picker's own query is scoped to $router alone.
//
// So it offers nodes that no panel under the current selection can show, and
// choosing one empties the dashboard silently: the WHERE matches nothing,
// Grafana reports no error, and nothing on screen says the node belongs to
// another peer or to a session that already ended. That is the same shape of
// defect $peer had -- a control that ships looking like it works.
//
// Both fixtures are inserted here rather than relied on from an earlier
// test, because a router_ip-only filter makes the picker's answer depend on
// whatever else the shared testDB happens to hold: dash-r1-twin advertises
// twin-a under the SAME router_ip, so whether it appeared was decided by
// test order.
func TestFocusPickerOffersOnlyTheSelectedPeersCurrentSessionNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)
	insertSharedIPRouterFixture(t, ctx, ch)

	// The four nodes peer A advertised in the current session, labeled the
	// way the picker's own multiIf renders them: by name, by router_id_v4,
	// and -- for the node that has neither -- by decoded hex.
	// The pseudonode is offered: focusing the graph on a LAN segment shows
	// every router attached to it, which is a question worth asking.
	wantA := []string{"0aff13020a001301", "10.255.19.3", "10.255.29.2", "dash-a", "dash-d"}
	// Peer B advertised exactly one node in that same session.
	wantB := []string{"dash-f"}

	for _, dash := range linkStateDashboards {
		t.Run(dash, func(t *testing.T) {
			for _, tc := range []struct {
				peer string
				want []string
			}{{dashPeerIP, wantA}, {dashPeerIPB, wantB}} {
				got := labels(focusOptions(t, ctx, ch, dash, tc.peer))
				if !slices.Equal(got, tc.want) {
					t.Errorf("$focus for peer %s offers %v, want %v.\n"+
						"dash-f belongs to the other BMP peer, dash-stale to a "+
						"superseded session and twin-a to a different router "+
						"sharing this IP; every panel excludes all three, so "+
						"offering them means picking one empties the dashboard "+
						"with no error and no explanation.",
						tc.peer, got, tc.want)
				}
			}
		})
	}
}

// focusValue is what Grafana would interpolate into $focus when an operator
// picks the option labeled title -- read out of the committed picker query,
// not invented, so these tests exercise the picker-to-panel seam rather than
// a node_key the test computed for itself.
func focusValue(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, peer, title string) string {
	t.Helper()
	opts := focusOptions(t, ctx, ch, dashboard, peer)
	v, ok := opts[title]
	if !ok {
		t.Fatalf("%s $focus offers no option labelled %q; it offers %v",
			dashboard, title, labels(opts))
	}
	// "0" is the variable's allValue, i.e. All. A node whose key really were
	// 0 would make every assertion below vacuously true -- the panel would
	// return everything and the test would read it as a correct filter.
	if v == "0" {
		t.Fatalf("%s $focus offers %q with value 0, which is the picker's "+
			"allValue; every focused assertion would silently mean All",
			dashboard, title)
	}
	return v
}

// titlesOf flattens a nodes-frame id -> title map into a sorted title list.
func titlesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// $focus is the control that answers "show me this one router's corner of
// the graph", and deleting all six of its WHERE clauses from the four
// committed dashboards left the entire sink suite green: it shipped
// completely unexercised, exactly as $peer did.
//
// Node B is the one worth focusing on. It sits in the middle of the fixture
// -- A->B and B->C touch it, A->E does not, and only one of the two fixture
// prefixes originates from it -- so a filter that is present but reads the
// wrong column, or that matches the local endpoint only, changes the answer
// here rather than merely reducing the row count.
func TestFocusNarrowsEachTableToTheChosenNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	// B is labeled by its router_id_v4: it carries no name.
	const nodeB = "10.255.29.2"

	t.Run("nodes", func(t *testing.T) {
		focus := focusValue(t, ctx, ch, "link-state-nodes", dashPeerIP, nodeB)
		got := labels(toValueMap(nodesTableFor(t, ctx, ch, dashPeerIP, focus)))
		if want := []string{nodeB}; !slices.Equal(got, want) {
			t.Errorf("nodes table focused on %s returned %v, want %v -- "+
				"the other three advertised nodes are still there, so "+
				"$focus filtered nothing", nodeB, got, want)
		}
	})

	t.Run("links", func(t *testing.T) {
		focus := focusValue(t, ctx, ch, "link-state-links", dashPeerIP, nodeB)
		got := make([]string, 0, 2)
		for pair := range linksTableFor(t, ctx, ch, dashPeerIP, focus) {
			got = append(got, pair)
		}
		sort.Strings(got)
		// B is the REMOTE end of A->B and the LOCAL end of B->C. Both must
		// come back: a clause that tests local_node_key alone loses the
		// first, which is the half of an adjacency an operator is usually
		// looking for.
		want := []string{"10.255.19.1 -> 10.255.19.2", "10.255.19.2 -> 10.255.19.3"}
		if !slices.Equal(got, want) {
			t.Errorf("links table focused on %s returned %v, want %v -- "+
				"A->E does not touch B and must not appear", nodeB, got, want)
		}
	})

	t.Run("prefixes", func(t *testing.T) {
		focus := focusValue(t, ctx, ch, "link-state-prefixes", dashPeerIP, nodeB)
		got := make([]string, 0, 1)
		for prefix := range prefixesTableFor(t, ctx, ch, dashPeerIP, focus) {
			got = append(got, prefix)
		}
		sort.Strings(got)
		// The $focus clause is inside the ls_prefixes subquery, not on the
		// nodeset it joins to: filtering the join side instead would leave
		// A's prefix in the table with an empty origin rather than removing
		// it.
		if want := []string{"10.199.2.0/24"}; !slices.Equal(got, want) {
			t.Errorf("prefixes table focused on %s returned %v, want %v",
				nodeB, got, want)
		}
	})
}

// Focusing must not break the invariant the two-frame design exists to hold:
// every edge the edges frame returns must have both endpoints in the nodes
// frame, or Grafana's Node Graph drops the edge and reports nothing.
//
// That is why the nodes frame carries "OR node_key IN (SELECT k FROM
// endpoints)" on top of its own node_key test. A filter written the obvious
// way -- node_key = $focus, matching the edges frame's shape -- returns the
// focused node alone while the edges frame returns its adjacencies, and the
// panel renders one dot and no lines.
func TestFocusKeepsTheTopologyFramesSelfConsistent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	focus := focusValue(t, ctx, ch, "link-state-topology", dashPeerIP, "dash-a")

	edges := edgesFrame(t, ctx, ch, focus, "0")
	if len(edges) != 2 {
		t.Fatalf("edges frame focused on dash-a returned %d edges, want 2 "+
			"(A->B and A->E; B->C does not touch A and C->A is withdrawn): %v",
			len(edges), edges)
	}
	titles, subtitles := nodeFrame(t, ctx, ch, focus, "0")
	// A, its two active neighbors, and nothing else: C is reachable only
	// over the withdrawn link and D has no adjacency at all.
	want := []string{"10.255.19.5", "10.255.29.2", "dash-a"}
	if got := titlesOf(titles); !slices.Equal(got, want) {
		t.Errorf("nodes frame focused on dash-a returned %v, want %v", got, want)
	}
	for _, e := range edges {
		if _, ok := titles[e.source]; !ok {
			t.Errorf("edge %s->%s has no source node in the focused nodes "+
				"frame; Grafana drops it silently: %v", e.source, e.target, titles)
		}
		if _, ok := titles[e.target]; !ok {
			t.Errorf("edge %s->%s has no target node in the focused nodes "+
				"frame; Grafana drops it silently: %v", e.source, e.target, titles)
		}
	}
	// E reaches the frame only as a link endpoint, and focusing must not
	// promote it to an advertised node.
	var placeholders int
	for _, st := range subtitles {
		if st == "endpoint only" {
			placeholders++
		}
	}
	if placeholders != 1 {
		t.Errorf("want exactly 1 node marked \"endpoint only\" under focus, "+
			"got %d: %v", placeholders, subtitles)
	}

	// $focus and $state are independent controls. The withdrawn C->A link
	// touches the focused node, so widening $state must bring it and C back
	// -- a focus clause that folded the state test into itself would not.
	edgesAll := edgesFrame(t, ctx, ch, focus, "0, 1")
	if len(edgesAll) != 3 {
		t.Errorf("edges frame focused on dash-a with withdrawn links shown "+
			"returned %d edges, want 3: %v", len(edgesAll), edgesAll)
	}
	titlesAll, _ := nodeFrame(t, ctx, ch, focus, "0, 1")
	if got, want := titlesOf(titlesAll),
		[]string{"10.255.19.3", "10.255.19.5", "10.255.29.2", "dash-a"}; !slices.Equal(got, want) {
		t.Errorf("nodes frame focused on dash-a with withdrawn links shown "+
			"returned %v, want %v", got, want)
	}
}

// dash-d is advertised with no adjacency -- a shape real routers do produce. Its
// edges frame is legitimately empty, so the nodes frame's own node_key test
// is the only thing that puts it on screen; without it, focusing the node an
// operator is investigating renders an empty panel.
func TestFocusOnANodeWithNoAdjacencyStillShowsIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	focus := focusValue(t, ctx, ch, "link-state-topology", dashPeerIP, "dash-d")
	if edges := edgesFrame(t, ctx, ch, focus, "0"); len(edges) != 0 {
		t.Errorf("edges frame focused on the isolated dash-d returned %d "+
			"edges, want 0: %v", len(edges), edges)
	}
	titles, _ := nodeFrame(t, ctx, ch, focus, "0")
	if got, want := titlesOf(titles), []string{"dash-d"}; !slices.Equal(got, want) {
		t.Errorf("nodes frame focused on the isolated dash-d returned %v, "+
			"want %v -- an operator who picks it must not get a blank panel",
			got, want)
	}
}

// toValueMap discards a table helper's row values, keeping the keys as a
// label set the picker helpers' comparisons can reuse.
func toValueMap[V any](m map[string]V) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = ""
	}
	return out
}

// dashboardPanel is one committed panel: what it is called, what it says it
// does, the SQL it actually runs, and how Grafana repeats it.
//
// repeat and repeatDirection are here because nothing in this package ever
// read them, and the rule that every panel that answers per collector
// repeats over $collector and names that collector in
// its own heading is a property of exactly those two fields plus the
// title. See TestCollectorScopedPanelsRepeatAndNameTheirCollector.
type dashboardPanel struct {
	dashboard   string
	title       string
	description string
	sql         string
	// repeat is the template variable Grafana repeats this panel over, or ""
	// for a panel rendered once.
	repeat string
	// repeatDirection is "h" for a row of tiles and "v" for a column.
	// Grafana treats an absent value as "h", so a panel relying on the
	// default and a panel stating it render the same -- which is why the
	// guard demands the explicit value rather than accepting either: the
	// point is that the shape was decided, not that it came out right.
	repeatDirection string
}

// dashboardPanels reads every panel that carries a query out of one committed
// dashboard, descending into nested panels the way dashboardTargets does.
func dashboardPanels(t *testing.T, dashboard string) []dashboardPanel {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Panels []json.RawMessage `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []dashboardPanel
	var walk func(panels []json.RawMessage)
	walk = func(panels []json.RawMessage) {
		for _, pr := range panels {
			var p struct {
				Title           string `json:"title"`
				Description     string `json:"description"`
				Repeat          string `json:"repeat"`
				RepeatDirection string `json:"repeatDirection"`
				Panels          []json.RawMessage
				Targets         []struct {
					RawSQL string `json:"rawSql"`
				} `json:"targets"`
			}
			if err := json.Unmarshal(pr, &p); err != nil {
				t.Fatalf("parse panel in %s: %v", path, err)
			}
			var sql strings.Builder
			for _, tg := range p.Targets {
				sql.WriteString(tg.RawSQL)
				sql.WriteString("\n")
			}
			// Any panel with targets, not only the ones carrying SQL. A
			// Prometheus panel cannot filter on a ClickHouse variable at all,
			// which makes it MORE likely to mislead under a picker it ignores,
			// not less -- and gating on rawSql quietly exempted exactly those.
			if len(p.Targets) > 0 {
				out = append(out, dashboardPanel{
					dashboard:       dashboard,
					title:           p.Title,
					description:     p.Description,
					sql:             sql.String(),
					repeat:          p.Repeat,
					repeatDirection: p.RepeatDirection,
				})
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return out
}

// dashboardVariableNames returns every template variable a dashboard exposes,
// query-typed and custom alike -- $state is custom, and a panel ignoring it is
// exactly as misleading as one ignoring $peer.
func dashboardVariableNames(t *testing.T, dashboard string) []string {
	t.Helper()
	path := filepath.Join("..", "deploy", "grafana", "dashboards", dashboard+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var d struct {
		Templating struct {
			List []struct {
				Name string `json:"name"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, v := range d.Templating.List {
		out = append(out, v.Name)
	}
	sort.Strings(out)
	return out
}

// varUse reports whether sql references $name, in either of the forms Grafana
// accepts: bare $name, or ${name} with an optional format option after a
// colon (${target:sqlstring}). Anchored so $router is not found inside a
// longer name.
//
// The brace form has to count. A panel that switched to ${target:sqlstring}
// to escape the operator's input still filters on $target, and reading only
// the bare form would report it as ignoring a control it obeys.
func varUse(sql, name string) bool {
	return regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(name) + `\b`).MatchString(sql)
}

// A dashboard's variables sit in one row above every panel on it, which reads
// as a promise that they all obey them. Several do not: "Reconnects in range"
// counts BMP sessions and cannot be filtered by $peer or $state, and
// "Objects in current session" ignores $focus -- so focusing one node shrinks
// the graph while the counts beside it stay put, and nothing says why. "View
// completeness by router" is the extreme case: it is a fleet view and ignores
// all four, including $router.
//
// None of that is wrong; every one of those panels answers a question the
// controls do not apply to. What was wrong is that no panel said so, and a
// number an operator cannot scope is a number they will misread.
//
// The rule is mechanical on purpose: naming the variable makes the tooltip
// searchable, and makes a panel that quietly stops honoring a control fail
// here rather than mislead in silence.
func TestEveryPanelDescribesTheControlsItIgnores(t *testing.T) {
	for _, dash := range allDashboards {
		t.Run(dash, func(t *testing.T) {
			vars := dashboardVariableNames(t, dash)
			panels := dashboardPanels(t, dash)
			if len(panels) == 0 {
				t.Fatalf("%s: found no panels carrying a query, so this test "+
					"asserts nothing", dash)
			}
			for _, p := range panels {
				if strings.TrimSpace(p.description) == "" {
					t.Errorf("%s panel %q has no description; an operator has "+
						"nothing to read but the title", dash, p.title)
					continue
				}
				for _, v := range vars {
					if varUse(p.sql, v) || varUse(p.description, v) {
						continue
					}
					t.Errorf("%s panel %q does not filter on $%s and does not "+
						"mention it. The variable sits in the row above this "+
						"panel, so it reads as applying to it; say that it "+
						"does not.\ndescription: %s", dash, p.title, v, p.description)
				}
			}
		})
	}
}

// insertWithdrawnNodeRouterFixture inserts a router whose current session
// advertises two nodes and then withdraws one of them. It reserves its own
// router, peer and router-ids so insertTopologyFixture's counts -- which
// three tests assert to the object, with the arithmetic written out in their
// comments -- do not move.
//
// The lab archive holds no withdrawn ls_nodes row at all (0 of 139 as this
// was written), which is precisely why the case needs a fixture: a node
// leaving the IGP is ordinary BGP-LS, and nothing here had ever seen one.
func insertWithdrawnNodeRouterFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	const (
		routerIP = "10.0.199.4"
		peerIP   = "10.255.199.6"
		sysname  = "dash-r4"
		session  = 9401
	)
	live := []byte{10, 255, 59, 1}
	gone := []byte{10, 255, 59, 2}
	env := func(payload *vantagev1.Envelope_Ls) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: routerIP, SysName: sysname},
			Peer:        &vantagev1.PeerId{Ip: peerIP},
			SessionId:   session,
			TsCollector: timestamppb.New(corpusCollectorClock),
			Payload:     payload,
		}
	}
	peerUp := &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: routerIP, SysName: sysname},
		Peer:        &vantagev1.PeerId{Ip: peerIP},
		SessionId:   session,
		TsCollector: timestamppb.New(corpusCollectorClock),
		Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
			Kind: vantagev1.PeerEvent_KIND_UP,
		}},
	}
	advertise := env(&vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
		Nodes: []*vantagev1.LsNode{
			{Protocol: 3, Identifier: 100, Local: lsDesc(live), Name: "dash-live"},
			{Protocol: 3, Identifier: 100, Local: lsDesc(gone), Name: "dash-gone"},
		},
	}})
	// Same ts_collector, higher stream_seq -- the tiebreak every argMax in
	// these queries is ordered on, and how a withdraw arrives on the wire.
	withdraw := env(&vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
		Nodes: []*vantagev1.LsNode{
			{Protocol: 3, Identifier: 100, Local: lsDesc(gone), Name: "dash-gone", IsWithdraw: true},
		},
	}})
	for i, ev := range []*vantagev1.Envelope{peerUp, advertise, withdraw} {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert withdrawn-node fixture envelope %d: %v", i, err)
		}
	}
}

// $state is the third scope the panels apply and the picker did not. Every
// panel ends in HAVING argMax(is_withdraw, ...) IN ($state), so with the
// default "active" a withdrawn node is not on any of them -- but it stayed in
// the picker, and selecting it emptied the dashboard exactly the way a node
// from the wrong peer or a dead session did.
//
// Switching $state to "withdrawn too" must bring it back rather than do
// nothing, which is what makes this an assertion about reading the variable
// and not merely about excluding a row.
func TestFocusPickerTracksTheStateControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertWithdrawnNodeRouterFixture(t, ctx, ch)

	const (
		routerIP = "10.0.199.4"
		peerIP   = "10.255.199.6"
	)
	for _, dash := range linkStateDashboards {
		t.Run(dash, func(t *testing.T) {
			active := labels(focusOptionsOn(t, ctx, ch, dash, routerIP, peerIP, "0"))
			if want := []string{"dash-live"}; !slices.Equal(active, want) {
				t.Errorf("$focus with $state=active offers %v, want %v -- "+
					"dash-gone was withdrawn in this session, so no panel on "+
					"this dashboard can show it and picking it empties the "+
					"dashboard with no error", active, want)
			}
			withdrawnToo := labels(focusOptionsOn(t, ctx, ch, dash, routerIP, peerIP, "0, 1"))
			if want := []string{"dash-gone", "dash-live"}; !slices.Equal(withdrawnToo, want) {
				t.Errorf("$focus with $state=\"withdrawn too\" offers %v, want "+
					"%v -- widening the state control must bring the withdrawn "+
					"node back, or the picker is not reading $state at all",
					withdrawnToo, want)
			}
		})
	}
}

const (
	lgRouterA = "10.0.199.5"
	lgRouterB = "10.0.199.6"
	lgPeer    = "10.255.199.7"
	// lgStale is numerically LESS than lgSession, which is the ordering a
	// collector produces: session ids come from now().UnixNano() behind a
	// monotonic guard, so a superseded session always carries the lower id.
	// The lab archive agrees -- 1,208 session transitions across 15 router
	// views, zero inversions of id against first-seen time.
	//
	// It used to be inverted on purpose, so that a panel reaching for
	// max(session_id) landed on the wrong session and the superseded-session
	// assertions could tell the two resolutions apart. That inverted the one
	// thing production guarantees in order to test a resolution that was
	// itself reversed on 2026-09-04; see dashSession's comment for why
	// max(session_id) won. The expression is pinned statically now, by
	// TestDashboardsResolveTheCurrentSessionTheWayQueryDoes.
	lgSession = 9502
	lgStale   = 9501
)

// lgZeroClock is what a router carrying the QK_TS_ZERO quirk puts in the BMP
// per-peer header: the epoch. It is stamped on ts_router only. ts_collector
// is the collector's own clock, and deploy/clickhouse/schema.sql keys
// PARTITION BY and TTL on it precisely because ts_router looks like this.
// lgPfxNoPath is advertised over iBGP with no AS path at all.
const lgPfxNoPath = "10.77.50.0/24"

// lgWithdrawSentinel is the RFC 3107 / RFC 8277 label stack a router sends in
// place of a real label when withdrawing a labeled route (0x800000 >> 4). The
// proto that carries it says it is "not a real label, and must be ignored".
const lgWithdrawSentinel = 524288

var lgZeroClock = time.Unix(0, 0).UTC()

// The fixture's arrangement is what makes every later assertion decidable, so
// it is pinned here. An innocent edit that gives both routers the same prefix
// length, or that makes lgStale the current session, turns the ordering and
// staleness tests into tests that pass whether or not the clause they name
// exists.
func TestLookingGlassFixtureIsArrangedAdversarially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	// peer_events must name lgSession as current, resolved the way the panels
	// resolve it: max(session_id). lgStale is the SMALLER id (9501 against
	// 9502) and its Peer Up is also the older of the two, so both the old
	// argMax-on-time resolution and the one the dashboards converged on in
	// 2026-09-04 agree here -- but only one of them is what the panels this
	// test guards actually run, and a guard that resolves the session
	// differently from the panel it guards can pass while the panel is wrong.
	// That is the shape the $focus pickers had until 2026-09-19.
	var sid uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT max(session_id) FROM vantage.peer_events FINAL "+
			"WHERE router_ip = toIPv6('"+lgRouterA+"')")).Scan(&sid); err != nil {
		t.Fatalf("current session: %v", err)
	}
	if sid != lgSession {
		t.Fatalf("peer_events names session %d as current, want %d -- "+
			"max(session_id) is how every panel here resolves it, and the "+
			"fixture's id ordering is what makes the superseded-session "+
			"column decidable", sid, lgSession)
	}

	// The two covering prefixes must come from DIFFERENT routers, or
	// "most specific first" could be satisfied by router ordering alone.
	var routers uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT countDistinct(router_ip) FROM vantage.route_unicast FINAL "+
			"WHERE prefix IN ('10.77.14.0/24', '10.77.14.4/30')")).Scan(&routers); err != nil {
		t.Fatalf("covering routers: %v", err)
	}
	if routers != 2 {
		t.Fatalf("the two covering prefixes come from %d router(s), want 2 -- "+
			"most-specific-first must not be satisfiable by router order", routers)
	}

	// The fixture must still emit an end-of-RIB marker, and it must land in
	// eor_events with no empty-prefix row left behind in route_unicast.
	//
	// Before eor_events, the marker was a route_unicast row with prefix =
	// '' and end_of_rib = 1, and this assertion existed to prove the
	// fixture really carried the row that makes a naive containment test
	// raise -- the panels only survive it because they filter it out. The
	// split moves that from a filter the panels have to remember to a
	// property of the table, so both halves are asserted: the marker is
	// still produced (the fixture did not quietly stop exercising the
	// path), and route_unicast no longer holds a prefix-less row for the
	// panels to trip over.
	var markers uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.eor_events "+
			"WHERE router_ip = toIPv6('"+lgRouterA+"')")).Scan(&markers); err != nil {
		t.Fatalf("markers: %v", err)
	}
	if markers == 0 {
		t.Fatal("the fixture carries no end-of-RIB marker; the envelope whose " +
			"filing decision eor_events exists for would go untested")
	}
	var prefixless uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.route_unicast FINAL "+
			"WHERE router_ip = toIPv6('"+lgRouterA+"') AND prefix = ''")).Scan(&prefixless); err != nil {
		t.Fatalf("prefix-less rows: %v", err)
	}
	if prefixless != 0 {
		t.Fatalf("route_unicast holds %d prefix-less row(s) for %s; the "+
			"end-of-RIB marker is filed in eor_events now, and a route table "+
			"that still collects artifacts puts isIPAddressInRange back at "+
			"risk of raising", prefixless, lgRouterA)
	}

	// The QK_TS_ZERO route's two observations must disagree about which is
	// latest, and disagree in opposite directions on the two clocks. Give both
	// clocks the same ordering -- the arrangement every other envelope here
	// has -- and TestLookingGlassResolvesLatestOnTheCollectorClock passes
	// whichever clock the panels resolve on, which is the whole defect it
	// exists to catch.
	var routerNewerWhenWithdrawn, collectorNewerWhenWithdrawn uint8
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT toUInt8(argMax(ts_router, is_withdraw) > argMin(ts_router, is_withdraw)), "+
			"toUInt8(argMax(ts_collector, is_withdraw) > argMin(ts_collector, is_withdraw)) "+
			"FROM vantage.route_unicast FINAL WHERE prefix = '10.77.40.0/24'")).Scan(
		&routerNewerWhenWithdrawn, &collectorNewerWhenWithdrawn); err != nil {
		t.Fatalf("broken-clock route: %v", err)
	}
	if routerNewerWhenWithdrawn != 0 || collectorNewerWhenWithdrawn != 1 {
		t.Fatalf("10.77.40.0/24's withdrawal is newer than its advertisement on "+
			"ts_router=%d and on ts_collector=%d, want 0 and 1 -- the two clocks "+
			"must disagree about which observation is latest, or the panels' "+
			"choice between them is not decidable",
			routerNewerWhenWithdrawn, collectorNewerWhenWithdrawn)
	}
}

// insertLookingGlassFixture writes the routes the looking glass is specified
// against, through the real RowsFor + Insert path, under reserved identifiers
// no other test in this package uses (testDB is shared and never truncated).
//
// In one place it carries: a /24 and a more-specific /30 covering the same
// address from DIFFERENT routers; a prefix whose latest observation is a
// withdrawal; an observation belonging to a session peer_events no longer
// names as current; an end-of-RIB marker whose empty prefix must never reach
// the containment test; an IPv6 prefix, so the +96 length shift is exercised
// rather than assumed; and a route from a router whose ts_router runs
// backwards, so the panels' choice of clock is decidable.
func insertLookingGlassFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	// envAt stamps the two clocks separately, which is the whole point of the
	// QK_TS_ZERO route below: while every envelope carried the same value in
	// both, swapping ts_router for ts_collector in the dashboard changed no
	// test, so the choice of clock was a control nothing exercised.
	envAt := func(router string, session uint64, tsRouter, tsCollector time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		sys := "dash-lg1"
		if router == lgRouterB {
			sys = "dash-lg2"
		}
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: lgPeer, Asn: 65001},
			SessionId:   session,
			TsRouter:    timestamppb.New(tsRouter),
			TsCollector: timestamppb.New(tsCollector),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	env := func(router string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return envAt(router, session, ts, ts, r)
	}
	peerUp := func(router string, session uint64, ts time.Time) *vantagev1.Envelope {
		sys := "dash-lg1"
		if router == lgRouterB {
			sys = "dash-lg2"
		}
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: lgPeer, Asn: 65001},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
				Kind: vantagev1.PeerEvent_KIND_UP,
			}},
		}
	}
	attrs := func(nextHop string, asns []uint32) *vantagev1.PathAttributes {
		return &vantagev1.PathAttributes{
			Origin:  0,
			NextHop: nextHop,
			AsPath:  []*vantagev1.AsPathSegment{{Type: 2, Asns: asns}},
		}
	}
	v4 := &vantagev1.Family{Afi: 1, Safi: 1}

	now := corpusCollectorClock
	// The stale session's Peer Up is OLDER and its id is LOWER (lgStale 9501,
	// lgSession 9502), so both the old argMax-on-time resolution and the
	// max(session_id) the dashboards converged on in 2026-09-04 name
	// lgSession current.
	staleUp := peerUp(lgRouterA, lgStale, now.Add(-3*time.Hour))
	curUpA := peerUp(lgRouterA, lgSession, now.Add(-time.Hour))
	curUpB := peerUp(lgRouterB, lgSession, now.Add(-time.Hour))

	envs := []*vantagev1.Envelope{
		staleUp, curUpA, curUpB,

		// A covers 10.77.14.7 with a /24 ...
		env(lgRouterA, lgSession, now, &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs("10.77.0.1", []uint32{65001, 64500}),
			Announced: []*vantagev1.Prefix{{Prefix: "10.77.14.0/24"}},
		}),
		// ... and B covers it with a more-specific /30, from a different
		// router, so most-specific-first cannot be satisfied by router order.
		env(lgRouterB, lgSession, now, &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs("10.77.0.2", []uint32{65001, 64600}),
			Announced: []*vantagev1.Prefix{{Prefix: "10.77.14.4/30"}},
		}),
		// Advertised, then withdrawn: the latest observation wins and the row
		// must still appear, marked withdrawn.
		env(lgRouterA, lgSession, now, &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs("10.77.0.1", []uint32{65001}),
			Announced: []*vantagev1.Prefix{{Prefix: "10.77.20.0/24"}},
		}),
		env(lgRouterA, lgSession, now, &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs("10.77.0.1", []uint32{65001}),
			Withdrawn: []*vantagev1.Prefix{{Prefix: "10.77.20.0/24"}},
		}),
		// Heard only in the superseded session.
		env(lgRouterA, lgStale, now.Add(-2*time.Hour), &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs("10.77.0.1", []uint32{65001}),
			Announced: []*vantagev1.Prefix{{Prefix: "10.77.30.0/24"}},
		}),
		// Learned over iBGP, so it carries NO AS path. 92 of the lab archive's
		// 476 unicast rows are this shape, and as_path[-1] renders every one
		// of them as the reserved AS 0.
		env(lgRouterA, lgSession, now, &vantagev1.RouteEvent{
			Family: v4,
			Attrs: &vantagev1.PathAttributes{
				Origin: 0, NextHop: "10.77.0.1",
			},
			Announced: []*vantagev1.Prefix{{Prefix: lgPfxNoPath}},
		}),
		// The end-of-RIB marker: no prefix at all.
		env(lgRouterA, lgSession, now, &vantagev1.RouteEvent{
			Family: v4, EndOfRib: true,
		}),
		// IPv6, so the +96 shift is exercised.
		env(lgRouterA, lgSession, now, &vantagev1.RouteEvent{
			Family:    &vantagev1.Family{Afi: 2, Safi: 1},
			Attrs:     attrs("2001:db8:77::1", []uint32{65001}),
			Announced: []*vantagev1.Prefix{{Prefix: "2001:db8:77::/48"}},
		}),
		// A router with a broken clock, advertising and then withdrawing.
		// The advertisement is stamped normally on both clocks; the
		// withdrawal that follows carries an epoch ts_router -- what
		// QK_TS_ZERO describes -- and a LATER ts_collector, because the
		// collector heard it second.
		//
		// The two clocks therefore disagree about which observation is the
		// latest, and they disagree in the direction that matters: resolved on
		// ts_router the advertisement wins and this route reads "advertised"
		// though it is gone, with a last_seen in 1970. Resolved on
		// ts_collector, which is what the panels do, the withdrawal wins.
		//
		// Both ts_collector values stay recent on purpose. That column is what
		// PARTITION BY and the 90-day TTL are keyed on, so an epoch-zero one
		// would drop these rows at the next merge and take the test with them.
		envAt(lgRouterA, lgSession, now.Add(-30*time.Minute), now.Add(-30*time.Minute),
			&vantagev1.RouteEvent{
				Family: v4, Attrs: attrs("10.77.0.1", []uint32{65001}),
				Announced: []*vantagev1.Prefix{{Prefix: "10.77.40.0/24"}},
			}),
		envAt(lgRouterA, lgSession, lgZeroClock, now, &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs("10.77.0.1", []uint32{65001}),
			Withdrawn: []*vantagev1.Prefix{{Prefix: "10.77.40.0/24"}},
		}),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert looking-glass fixture envelope %d: %v", i, err)
		}
	}
}

// lookingGlassStatTitle is looking-glass's "What we know" stat panel's exact
// title, and lookingGlassVpnStatTitle its L3VPN twin's. Both carry the repeat
// variable itself, "$collector", so Grafana substitutes a concrete collector
// into each repeated tile's heading -- without it a dual-homed router draws
// two tiles with the identical heading and two different, unattributed
// numbers beside them, which reads as a contradiction rather than as two
// answers. See linkStateNodesCoverageStatTitle for the same argument on the
// link-state side.
//
// The two read the same text today and still get a constant each, for the
// reason the three link-state siblings do: renaming one dashboard's panel
// must not silently rename the other's expectation. lgStatTitle keys them by
// dashboard so a table-driven test can take the title as data.
const (
	lookingGlassStatTitle    = "What we know — $collector"
	lookingGlassVpnStatTitle = "What we know — $collector"
)

var lgStatTitle = map[string]string{
	"looking-glass":     lookingGlassStatTitle,
	"looking-glass-vpn": lookingGlassVpnStatTitle,
}

// lgRow is one row of the looking glass's covering-routes table.
type lgRow struct {
	// collector leads the row: a dual-homed router renders one row per
	// collector, and without this column the two are indistinguishable.
	collector      string
	prefix         string
	router         string
	peer           string
	rib            string
	state          string
	nextHop        string
	asPath         string
	originAS       string
	localPref      *uint32
	med            *uint32
	communities    string
	lastSeen       time.Time
	sessionCurrent uint8
}

// scopeToFixtureRouters narrows a looking-glass query to the two routers this
// fixture owns, by extending the panel's own time filter.
//
// testDB is shared and never truncated, and the looking glass has no $router
// control by design, so every route in the database competes to cover the
// targets below. The 10.77.0.0/16 reservation stops another fixture claiming a
// MORE specific route; it cannot stop a LESS specific one. A single 0.0.0.0/0
// or ::/0 arriving in some future corpus capture covers every target here at
// once and flips five exact-count assertions to failing -- and they would read
// as a looking-glass bug rather than as a new default route. The counts are
// claims about this fixture's routes, so they are asked of this fixture's
// routers.
//
// The $__timeFilter macro is the anchor because it is a fixed, specified part
// of both panels, and anything but exactly one occurrence is fatal: a rewrite
// that moved it must fail loudly here rather than quietly stop scoping and
// leave these tests exposed again.
func scopeToFixtureRouters(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("the looking-glass query carries %d occurrences of %q, want 1 "+
			"-- these tests scope themselves to the fixture's routers by "+
			"extending that filter, and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip IN (toIPv6('"+lgRouterA+"'), toIPv6('"+lgRouterB+"'))", 1)
}

// lookingGlassRows runs the committed covering-routes table with $target and
// $rib rendered as Grafana would render them, scoped to the fixture's routers
// so that an exact row count stays a claim about this fixture. Reach for
// lookingGlassAllRows where the assertion is deliberately about every route in
// the database.
func lookingGlassRows(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib string) []lgRow {
	t.Helper()
	return lookingGlassRowsScoped(t, ctx, ch, target, rib, true)
}

// lookingGlassAllRows is lookingGlassRows over the whole database, running the
// shipped SQL unmodified. "Empty means everything" and "no end-of-RIB row ever
// reaches the table" are claims about every route there is; scoping them would
// scope away what they assert.
func lookingGlassAllRows(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib string) []lgRow {
	t.Helper()
	return lookingGlassRowsScoped(t, ctx, ch, target, rib, false)
}

func lookingGlassRowsScoped(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib string, scoped bool) []lgRow {
	t.Helper()
	raw := panelSQL(t, "looking-glass", "Covering routes", "A")
	if scoped {
		raw = scopeToFixtureRouters(t, raw)
	}
	sql := substituteGrafana(raw, map[string]string{"target": target, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("covering routes (target=%q): %v\nSQL:\n%s", target, err, sql)
	}
	defer rows.Close()
	var out []lgRow
	for rows.Next() {
		var r lgRow
		if err := rows.Scan(&r.collector, &r.prefix, &r.router, &r.peer, &r.rib,
			&r.state, &r.nextHop, &r.asPath, &r.originAS, &r.localPref, &r.med,
			&r.communities, &r.lastSeen, &r.sessionCurrent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// The question the dashboard exists to answer: given a host address, which
// routes would carry traffic to it, most specific first.
func TestLookingGlassReturnsCoveringRoutesMostSpecificFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	got := lookingGlassRows(t, ctx, ch, "10.77.14.7", ".*")
	var prefixes []string
	for _, r := range got {
		prefixes = append(prefixes, r.prefix)
	}
	want := []string{"10.77.14.4/30", "10.77.14.0/24"}
	if !slices.Equal(prefixes, want) {
		t.Fatalf("covering 10.77.14.7 returned %v, want %v -- the /30 is the "+
			"more specific route and must sort first, and 10.77.20.0/24 and "+
			"10.77.30.0/24 do not cover this address at all", prefixes, want)
	}
	if got[0].router != "dash-lg2" || got[1].router != "dash-lg1" {
		t.Errorf("covering routes came from %q and %q, want dash-lg2 then "+
			"dash-lg1", got[0].router, got[1].router)
	}
	if got[0].originAS != "64600" {
		t.Errorf("origin AS = %q, want 64600 -- the last ASN of the path",
			got[0].originAS)
	}
}

// A route whose latest observation is a withdrawal must still appear, marked
// withdrawn. An empty table is indistinguishable from a typo in the search
// box; "nobody now, dash-lg1 did" is the answer an operator came for.
func TestLookingGlassShowsWithdrawnRoutesAsWithdrawn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	got := lookingGlassRows(t, ctx, ch, "10.77.20.1", ".*")
	if len(got) != 1 {
		t.Fatalf("covering 10.77.20.1 returned %d rows, want 1: %v", len(got), got)
	}
	if got[0].state != "withdrawn" {
		t.Errorf("state = %q, want withdrawn -- the advertisement was followed "+
			"by a withdrawal at a higher stream_seq", got[0].state)
	}
}

// Which clock resolves "the latest observation" is not a detail. ts_router
// comes from the BMP per-peer header and is router-reported: the QK_TS_ZERO
// quirk exists because routers send the epoch, and deploy/clickhouse/schema.sql
// keys PARTITION BY and TTL on ts_collector for exactly that reason.
//
// Inside one key group a zero-clock router loses every argMax race against
// itself. Its newest withdrawal is masked by an older advertisement, so the
// panel reports a route that is gone as advertised -- the worst answer a
// looking glass can give, because it is confident.
//
// last_seen is the same choice made twice. max(ts_router) over this key
// returns the ADVERTISEMENT's stamp (max ignores the epoch rather than being
// dragged to it), so the row is dated half an hour before the withdrawal the
// collector actually heard last; a router whose every message carries the
// epoch is dated 1970 outright.
func TestLookingGlassResolvesLatestOnTheCollectorClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	got := lookingGlassRows(t, ctx, ch, "10.77.40.1", ".*")
	if len(got) != 1 {
		t.Fatalf("covering 10.77.40.1 returned %d rows, want 1: %v", len(got), got)
	}
	if got[0].state != "withdrawn" {
		t.Errorf("state = %q, want withdrawn -- the withdrawal is the latest "+
			"observation on ts_collector, and only on ts_router (which this "+
			"router reports as the epoch) does the earlier advertisement win. "+
			"Reading advertised here means the panels resolve on the router's "+
			"clock, and a broken clock then hides every withdrawal it sends",
			got[0].state)
	}
	if got[0].lastSeen.Before(corpusCollectorClock.Add(-5 * time.Minute)) {
		t.Errorf("last_seen = %s, want the withdrawal's collector stamp near "+
			"%s -- max(ts_router) reports the advertisement instead, half an "+
			"hour earlier, because the withdrawal that followed it carries the "+
			"epoch in the BMP per-peer header", got[0].lastSeen, corpusCollectorClock)
	}
}

// The staleness column is the whole reason this dashboard is not session
// scoped: it must mark an observation whose session peer_events no longer
// names as current, rather than hiding the row or presenting it as fresh.
func TestLookingGlassFlagsObservationsFromSupersededSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	stale := lookingGlassRows(t, ctx, ch, "10.77.30.1", ".*")
	if len(stale) != 1 {
		t.Fatalf("covering 10.77.30.1 returned %d rows, want 1 -- the route is "+
			"only in the superseded session, and must still be shown", len(stale))
	}
	if stale[0].sessionCurrent != 0 {
		t.Errorf("session_current = %d for a route heard only in the "+
			"superseded session, want 0", stale[0].sessionCurrent)
	}
	fresh := lookingGlassRows(t, ctx, ch, "10.77.14.7", ".*")
	for _, r := range fresh {
		if r.sessionCurrent != 1 {
			t.Errorf("session_current = %d for %s, want 1 -- it was heard in "+
				"the current session", r.sessionCurrent, r.prefix)
		}
	}
}

// Four inputs make the obvious implementation raise CANNOT_PARSE_TEXT, and an
// operator reaches three of them on their first visit: an empty box, a typo,
// and a prefix typed where an address was expected. The fourth is not
// reachable by typing at all -- an end-of-RIB row's prefix is empty, so it
// raises on the FUNCTION'S SECOND ARGUMENT, before whatever filter the query
// author believes they wrote, because conjunct order is the optimizer's
// choice.
//
// A fifth never reaches the containment test at all: an apostrophe ends the
// SQL literal the textbox is pasted into, so the panel fails to parse before
// any of the above can matter. That one is about quoting rather than about
// addresses, and it is why the panels embed ${target:sqlstring} rather than
// a '$target' they quote themselves.
//
// lookingGlassRows fails the test on a query error, so each subtest here
// asserts both that nothing raises and that the answer is right.
func TestLookingGlassSurvivesEveryInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	t.Run("a prefix typed instead of an address", func(t *testing.T) {
		got := lookingGlassRows(t, ctx, ch, "10.77.14.0/24", ".*")
		var prefixes []string
		for _, r := range got {
			prefixes = append(prefixes, r.prefix)
		}
		// A typed prefix reduces to its network address, so a prefix finds
		// itself: splitByChar reduces $target to its network address alone
		// (10.77.14.0), and the containment test is single-address-in-range,
		// not range overlap. The /30 (10.77.14.4-7) does not contain
		// 10.77.14.0, so it cannot match; that's structural to this SQL, not
		// specific to this fixture.
		want := []string{"10.77.14.0/24"}
		if !slices.Equal(prefixes, want) {
			t.Errorf("typing a CIDR returned %v, want %v", prefixes, want)
		}
	})

	t.Run("text that is not an address", func(t *testing.T) {
		if got := lookingGlassAllRows(t, ctx, ch, "not-an-ip", ".*"); len(got) != 0 {
			t.Errorf("garbage input returned %d rows, want 0 -- and it must "+
				"return them rather than erroring the panel", len(got))
		}
	})

	// The input that has nothing to do with addresses. While the panels
	// embedded the textbox as a quoted '$target', one typed apostrophe
	// rendered as three in a row: an unterminated literal, a syntax error,
	// and a red panel. The dashboard now hands the quoting to Grafana's
	// ${target:sqlstring}, which doubles the quote rather than letting it
	// close the string, and substituteGrafana renders it the same way -- so
	// this exercises what an operator's keystroke actually sends.
	t.Run("an apostrophe", func(t *testing.T) {
		if got := lookingGlassAllRows(t, ctx, ch, "o'brien", ".*"); len(got) != 0 {
			t.Errorf("a typed apostrophe returned %d rows, want 0 -- it is not "+
				"an address, so it must return nothing WITHOUT erroring the "+
				"panel", len(got))
		}
		if got := lookingGlassAllRows(t, ctx, ch, "'; SELECT 1 --", ".*"); len(got) != 0 {
			t.Errorf("a quote followed by a statement returned %d rows, want 0",
				len(got))
		}
	})

	t.Run("IPv6", func(t *testing.T) {
		got := lookingGlassRows(t, ctx, ch, "2001:db8:77::1", ".*")
		if len(got) != 1 || got[0].prefix != "2001:db8:77::/48" {
			t.Errorf("covering 2001:db8:77::1 returned %v, want one row for "+
				"2001:db8:77::/48 -- the +96 length shift is v4-only and must "+
				"not be applied to a v6 prefix", got)
		}
	})

	t.Run("an empty box shows everything", func(t *testing.T) {
		all := lookingGlassAllRows(t, ctx, ch, "", ".*")
		if len(all) < 5 {
			t.Fatalf("empty $target returned %d rows, want every route in the "+
				"lab archive -- empty means All, the idiom $focus uses", len(all))
		}
		for _, r := range all {
			if r.prefix == "" {
				t.Errorf("an end-of-RIB marker row reached the table; its empty " +
					"prefix is what makes the containment test raise")
			}
		}
	})
}

// lookingGlassStats runs the committed stat row, scoped to the fixture's
// routers for the reason scopeToFixtureRouters gives. The three numbers are
// aggregates, so unlike the table there is nothing to filter afterwards: the
// scoping has to be in the query or not at all.
//
// All three come back UInt64: countDistinctIf and countIf are UInt64 already,
// and most_specific is wrapped in toUInt64 in the SQL because max() over a
// prefix length yields UInt8, which will not scan into a uint64.
func lookingGlassStats(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib string) (routers, mostSpecific, superseded uint64) {
	t.Helper()
	sql := substituteGrafana(
		scopeToFixtureRouters(t, panelSQL(t, "looking-glass", lookingGlassStatTitle, "A")),
		// The stat is repeated per collector and counts only the one named,
		// so it needs a value for $collector. This fixture writes every row
		// under "c1", the id every single-collector fixture in this file uses.
		map[string]string{"target": target, "rib": rib, "collector": "c1"})
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&routers, &mostSpecific, &superseded); err != nil {
		t.Fatalf("stat row (target=%q): %v\nSQL:\n%s", target, err, sql)
	}
	return
}

// The stat row frames the table: how many routers actually advertise this
// address, how specific the best match is, and how much of what follows rests
// on a session that has already ended.
func TestLookingGlassStatsFrameTheTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	routers, mostSpecific, superseded := lookingGlassStats(t, ctx, ch, "10.77.14.7", ".*")
	if routers != 2 {
		t.Errorf("routers advertising 10.77.14.7 = %d, want 2 (dash-lg1's /24 "+
			"and dash-lg2's /30)", routers)
	}
	if mostSpecific != 30 {
		t.Errorf("most specific = %d, want 30", mostSpecific)
	}
	if superseded != 0 {
		t.Errorf("from superseded session = %d, want 0 -- both covering routes "+
			"were heard in the current session", superseded)
	}

	// The withdrawn route must not count as a router advertising it.
	routers, _, _ = lookingGlassStats(t, ctx, ch, "10.77.20.1", ".*")
	if routers != 0 {
		t.Errorf("routers advertising the withdrawn 10.77.20.0/24 = %d, want 0 "+
			"-- the row is shown, but nobody is advertising it", routers)
	}

	// The route heard only in the superseded session must be counted as such.
	_, _, superseded = lookingGlassStats(t, ctx, ch, "10.77.30.1", ".*")
	if superseded != 1 {
		t.Errorf("from superseded session = %d, want 1", superseded)
	}
}

// lookingGlassShared pulls the two blocks the looking glass's panels must keep
// identical out of one panel's SQL: the containment CTE, and the WHERE that
// applies it. Returning them separately rather than as one string means a
// divergence report names which half moved.
func lookingGlassShared(t *testing.T, panel string) (with, where string) {
	t.Helper()
	sql := panelSQL(t, "looking-glass", panel, "A")
	w := regexp.MustCompile(`WITH .*? AS rng `).FindString(sql)
	f := regexp.MustCompile(`WHERE \$__timeFilter.*?(?: GROUP BY)`).FindString(sql)
	if w == "" || f == "" {
		t.Fatalf("looking-glass panel %q: could not find the containment CTE (%d chars) "+
			"or its WHERE (%d chars). This test anchors on their shape, so a rewrite "+
			"that changes it must update this matcher rather than silently stop checking.",
			panel, len(w), len(f))
	}
	return w, f
}

// The stat row and the table answer the same question about the same address,
// and they do it with the same ~550 characters of containment CTE and WHERE
// copied into both. Nothing keeps the copies in step: a fix applied to one and
// not the other makes the two panels disagree on screen, with the numbers above
// the table describing a different row set than the table below it.
//
// The two are no longer expected to describe the same rows OUTSIDE the two
// spans this guard covers, and that is deliberate rather than drift: since
// the 2026-09-19 collector-id slice the stat repeats per collector and counts
// only the collector named in its own heading, while the table lists every
// collector's rows at once behind a leading collector column. So on a
// dual-homed router the stat is narrower than the table BY DESIGN. What must
// still match is which rows COVER the target -- the containment CTE and the
// WHERE that applies it -- because a divergence there means the two panels
// disagree about what a covering route is, which no amount of per-collector
// scoping explains.
//
// Grafana has no include mechanism for panel SQL, so the duplication itself is
// not removable here. What is removable is its silence.
func TestLookingGlassPanelsShareOneContainmentTest(t *testing.T) {
	statWith, statWhere := lookingGlassShared(t, lookingGlassStatTitle)
	tableWith, tableWhere := lookingGlassShared(t, "Covering routes")

	if statWith != tableWith {
		t.Errorf("the two looking-glass panels' containment CTEs have diverged.\n"+
			"stat row: %s\ntable:    %s\n"+
			"They must match, or the numbers above the table describe a different "+
			"row set than the table itself.", statWith, tableWith)
	}
	if statWhere != tableWhere {
		t.Errorf("the two looking-glass panels' WHERE clauses have diverged.\n"+
			"stat row: %s\ntable:    %s\n"+
			"They must match, or the numbers above the table describe a different "+
			"row set than the table itself.", statWhere, tableWhere)
	}
}

const (
	vpnRouter = "10.0.199.7"
	vpnPeer   = "10.255.199.9"
	// vpnStale is numerically LESS than vpnSession, which is the ordering a
	// collector produces: session ids come from now().UnixNano() behind a
	// monotonic guard, so a superseded session always carries the lower id.
	// The lab archive agrees -- 1,208 session transitions across 15 router
	// views, zero inversions of id against first-seen time.
	//
	// It used to be inverted on purpose, so that a panel reaching for
	// max(session_id) landed on the wrong session and the superseded-session
	// assertions could tell the two resolutions apart. That inverted the one
	// thing production guarantees in order to test a resolution that was
	// itself reversed on 2026-09-04; see dashSession's comment for why
	// max(session_id) won. The expression is pinned statically now, by
	// TestDashboardsResolveTheCurrentSessionTheWayQueryDoes.
	vpnSession = 9602
	vpnStale   = 9601
	// The two VRFs exporting the same prefix. This is the case the whole
	// dashboard exists for and the lab archive has none of it: no prefix in
	// vantage.route_vpn appears under more than one RD.
	vpnRDa = "65000:11"
	vpnRDb = "65000:22"
)

// insertVpnLookingGlassFixture writes the labeled routes the L3VPN looking
// glass is specified against, through the real RowsFor + Insert path, under
// reserved identifiers no other test uses (testDB is shared, never truncated).
//
// It carries, in one place: ONE prefix exported by TWO VRFs under different
// RDs from the same router -- the question an L3VPN looking glass exists to
// answer, and the shape the lab archive cannot demonstrate; an lu4 route whose RD
// is empty BY DESIGN (route_vpn is really "labeled routes" discriminated by
// family, per bgp/vpn.go, and lu4 carries no Route Distinguisher); a VPN route
// whose latest observation is a withdrawal; and a route heard only in a
// session peer_events no longer names current.
func insertVpnLookingGlassFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	vpn4 := &vantagev1.Family{Afi: 1, Safi: 128}
	lu4 := &vantagev1.Family{Afi: 1, Safi: 4}
	env := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: vpnRouter, SysName: "dash-vpn1"},
			Peer:        &vantagev1.PeerId{Ip: vpnPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	peerUp := func(session uint64, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: vpnRouter, SysName: "dash-vpn1"},
			Peer:        &vantagev1.PeerId{Ip: vpnPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
				Kind: vantagev1.PeerEvent_KIND_UP,
			}},
		}
	}
	// Route targets are not a property of the NLRI -- they ride in the UPDATE's
	// extended communities, so two VRFs exporting one prefix arrive as two
	// separate messages with different attributes, which is how this fixture
	// builds them. Type 0x00 / sub-type 0x02 is the 2-byte-AS route target
	// (bgp/extcomm.go), the only pair rows.go files into RouteTargets.
	attrs := func(nextHop string, rt string) *vantagev1.PathAttributes {
		a := &vantagev1.PathAttributes{
			Origin: 0, NextHop: nextHop,
			AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000, 64700}}},
		}
		if rt != "" {
			a.ExtendedCommunities = []*vantagev1.ExtCommunity{
				{Type: 0x00, SubType: 0x02, Value: rt},
			}
		}
		return a
	}
	now := corpusCollectorClock
	envs := []*vantagev1.Envelope{
		// The stale session's Peer Up is OLDER and its id is LOWER
		// (vpnStale 9601, vpnSession 9602), so both the old argMax-on-time
		// resolution and the max(session_id) the dashboards converged on in
		// 2026-09-04 name vpnSession current.
		peerUp(vpnStale, now.Add(-3*time.Hour)),
		peerUp(vpnSession, now.Add(-time.Hour)),

		// One prefix, two VRFs, one router -- two UPDATEs, because the route
		// target rides in the attributes. A query that drops rd from its key
		// returns one row here instead of two.
		env(vpnSession, now, &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs("10.88.0.1", vpnRDa),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.88.5.0/24", Rd: vpnRDa, Labels: []uint32{24011}},
			},
		}),
		env(vpnSession, now, &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs("10.88.0.1", vpnRDb),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.88.5.0/24", Rd: vpnRDb, Labels: []uint32{24022}},
			},
		}),
		// lu4: no RD at all, by design.
		env(vpnSession, now, &vantagev1.RouteEvent{
			Family: lu4, Attrs: attrs("10.88.0.1", ""),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.88.9.1/32", Labels: []uint32{24099}},
			},
		}),
		// Advertised then withdrawn, same RD: the latest observation wins and
		// the row must still appear, marked withdrawn.
		env(vpnSession, now, &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs("10.88.0.1", vpnRDa),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				// A real router sends the RFC 3107/8277 sentinel in place of a
				// label when withdrawing, not the label it advertised. The
				// fixture said 24077 and hid what the panel renders.
				{Prefix: "10.88.7.0/24", Rd: vpnRDa, Labels: []uint32{lgWithdrawSentinel}},
			},
		}),
		env(vpnSession, now, &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs("10.88.0.1", vpnRDa),
			VpnWithdrawn: []*vantagev1.VpnPrefix{
				// A real router sends the RFC 3107/8277 sentinel in place of a
				// label when withdrawing, not the label it advertised. The
				// fixture said 24077 and hid what the panel renders.
				{Prefix: "10.88.7.0/24", Rd: vpnRDa, Labels: []uint32{lgWithdrawSentinel}},
			},
		}),
		// Heard only in the superseded session.
		env(vpnStale, now.Add(-2*time.Hour), &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs("10.88.0.1", vpnRDa),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.88.8.0/24", Rd: "65000:33", Labels: []uint32{24088}},
			},
		}),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert vpn looking-glass fixture envelope %d: %v", i, err)
		}
	}
}

// The lab archive holds no prefix under more than one RD, so the assertion the
// L3VPN looking glass is built around is only decidable because this fixture
// supplies it. Pinned here: an edit that gives both rows the same RD, or drops
// one, turns the two-VRF test into a test that passes on one row.
func TestVpnLookingGlassFixtureCarriesOnePrefixInTwoVrfs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertVpnLookingGlassFixture(t, ctx, ch)

	var rds uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT countDistinct(rd) FROM vantage.route_vpn FINAL "+
			"WHERE router_ip = toIPv6('"+vpnRouter+"') AND prefix = '10.88.5.0/24'")).Scan(&rds); err != nil {
		t.Fatalf("distinct rds: %v", err)
	}
	if rds != 2 {
		t.Fatalf("10.88.5.0/24 appears under %d RD(s), want 2 -- the two-VRF "+
			"case is the one thing the lab archive cannot demonstrate, so the "+
			"fixture is the only place it exists", rds)
	}
	// Resolved the way the panels resolve it: max(session_id). The fixture's
	// own comment records that vpnStale is both the older Peer Up and the
	// smaller id, so both resolutions agree -- but this guard has to read the
	// one the panels read, for the reason
	// TestLookingGlassFixtureIsArrangedAdversarially gives.
	var sid uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT max(session_id) FROM vantage.peer_events FINAL "+
			"WHERE router_ip = toIPv6('"+vpnRouter+"')")).Scan(&sid); err != nil {
		t.Fatalf("current session: %v", err)
	}
	if sid != vpnSession {
		t.Fatalf("peer_events names session %d current, want %d", sid, vpnSession)
	}
	var emptyRD uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.route_vpn FINAL "+
			"WHERE router_ip = toIPv6('"+vpnRouter+"') AND family = 'lu4' AND rd = ''")).Scan(&emptyRD); err != nil {
		t.Fatalf("lu4 rows: %v", err)
	}
	if emptyRD == 0 {
		t.Fatal("the fixture carries no lu4 row with an empty RD; the family " +
			"that makes route_vpn \"labeled routes\" rather than \"VPN routes\" " +
			"would go unexercised")
	}
	// The two VRFs must carry DIFFERENT route targets, and carry them at all.
	// Route targets ride in the UPDATE's extended communities rather than on
	// the NLRI, so building this fixture the obvious way -- both prefixes in
	// one envelope -- gives the two rows identical RTs and the column stops
	// distinguishing anything.
	var rts []string
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT arraySort(groupUniqArray(arrayStringConcat(route_targets, ','))) "+
			"FROM vantage.route_vpn FINAL "+
			"WHERE router_ip = toIPv6('"+vpnRouter+"') AND prefix = '10.88.5.0/24'")).Scan(&rts); err != nil {
		t.Fatalf("route targets: %v", err)
	}
	if want := []string{vpnRDa, vpnRDb}; !slices.Equal(rts, want) {
		t.Errorf("10.88.5.0/24 carries route targets %v, want %v -- one per VRF, "+
			"from each UPDATE's own extended communities", rts, want)
	}
}

// vpnRow is one row of the L3VPN looking glass's table.
type vpnLgRow struct {
	// collector leads the row, as it does on the unicast twin.
	collector      string
	prefix         string
	rd             string
	family         string
	router         string
	peer           string
	rib            string
	state          string
	nextHop        string
	routeTargets   string
	labels         string
	asPath         string
	lastSeen       time.Time
	sessionCurrent uint8
}

// vpnLookingGlassStats runs looking-glass-vpn's "What we know" stat, scoped
// to the fixture's own router the way the table beside it is.
func vpnLookingGlassStats(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib, family string) (vrfs, mostSpecific, superseded uint64) {
	t.Helper()
	sql := substituteGrafana(
		scopeToVpnFixtureRouter(t, panelSQL(t, "looking-glass-vpn", lookingGlassVpnStatTitle, "A")),
		// $collector, for the reason lookingGlassStats gives.
		map[string]string{"target": target, "rib": rib, "family": family, "collector": "c1"})
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&vrfs, &mostSpecific, &superseded); err != nil {
		t.Fatalf("vpn stat row (target=%q family=%q): %v\nSQL:\n%s", target, family, err, sql)
	}
	return
}

// looking-glass-vpn's "What we know" was the last panel across all thirteen
// dashboards with nothing asserting a value in it. Its non-VPN twin has had
// TestLookingGlassStatsFrameTheTable since the looking glass shipped; this
// one never got the same treatment.
//
// It is worth more than symmetry, because "VRFs carrying it" is not a row
// count and its description says so in two separate ways that can each be
// wrong on their own:
//
//	"counts distinct non-empty Route Distinguishers whose latest observation
//	 is an advertisement -- so a withdrawn route reads as zero, and an
//	 lu4-only result reads as zero too, because labeled unicast has no VRF"
//
// Both zeros are REAL ANSWERS an operator has to be able to trust: "this
// prefix is in no VRF right now" is the thing they came to find out, and a
// panel that reported 1 for a withdrawn route, or 1 for a labeled-unicast
// route that has no VRF at all, would answer the question wrongly in the
// direction that looks healthy. The fixture supplies one of each.
func TestVpnLookingGlassStatsFrameTheTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertVpnLookingGlassFixture(t, ctx, ch)

	// The headline case: one prefix genuinely exported by two VRFs.
	vrfs, mostSpecific, superseded := vpnLookingGlassStats(t, ctx, ch, "10.88.5.1", ".*", ".*")
	if vrfs != 2 {
		t.Errorf("VRFs carrying 10.88.5.0/24 = %d, want 2 (%s and %s) -- the "+
			"route target rides in the UPDATE's attributes, so these arrive "+
			"as two messages and a query keyed without rd reports one",
			vrfs, vpnRDa, vpnRDb)
	}
	if mostSpecific != 24 {
		t.Errorf("most specific = %d, want 24", mostSpecific)
	}
	if superseded != 0 {
		t.Errorf("from superseded session = %d, want 0 -- both were heard in "+
			"the current session", superseded)
	}

	// A withdrawn route is in NO VRF. The row still appears in the table
	// beside this, marked withdrawn; the count must not include it.
	vrfs, mostSpecific, _ = vpnLookingGlassStats(t, ctx, ch, "10.88.7.1", ".*", ".*")
	if vrfs != 0 {
		t.Errorf("VRFs carrying the withdrawn 10.88.7.0/24 = %d, want 0. The "+
			"route was advertised under %s and then withdrawn, so it is in no "+
			"VRF now -- and 1 here is the answer that looks healthy", vrfs, vpnRDa)
	}
	if mostSpecific != 24 {
		t.Errorf("most specific for the withdrawn route = %d, want 24 -- the "+
			"row is still THERE and still bounds the prefix length; only the "+
			"VRF count excludes it", mostSpecific)
	}

	// Labeled unicast has no Route Distinguisher at all, so its rd is the
	// empty string rather than a missing row. countDistinctIf's `rd != ''`
	// is what keeps that from counting as a VRF.
	vrfs, mostSpecific, _ = vpnLookingGlassStats(t, ctx, ch, "10.88.9.1", ".*", ".*")
	if vrfs != 0 {
		t.Errorf("VRFs carrying the lu4 route = %d, want 0 -- labeled unicast "+
			"carries no RD, and an empty RD is not a VRF", vrfs)
	}
	if mostSpecific != 32 {
		t.Errorf("most specific for the lu4 route = %d, want 32", mostSpecific)
	}

	// And the superseded column, which is the only thing on this panel that
	// reads peer_events at all.
	vrfs, _, superseded = vpnLookingGlassStats(t, ctx, ch, "10.88.8.1", ".*", ".*")
	if superseded != 1 {
		t.Errorf("from superseded session = %d, want 1 -- 10.88.8.0/24 was "+
			"heard only in %d, and %d is the router's current session",
			superseded, vpnStale, vpnSession)
	}
	if vrfs != 1 {
		t.Errorf("VRFs carrying 10.88.8.0/24 = %d, want 1 -- resting on a "+
			"superseded session does not make a route unadvertised, and these "+
			"two columns have to be able to disagree", vrfs)
	}
}

// vpnLookingGlassRows runs the committed L3VPN table, scoped to the fixture's
// own router so an exact count cannot be moved by another test's rows.
func vpnLookingGlassRows(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib, family string) []vpnLgRow {
	t.Helper()
	return vpnRowsFrom(t, ctx, ch, scopeToVpnFixtureRouter(t,
		panelSQL(t, "looking-glass-vpn", "Covering routes", "A")), target, rib, family)
}

// vpnLookingGlassAllRows runs the SAME panel with the SQL exactly as it ships,
// for the assertions that are deliberately global.
func vpnLookingGlassAllRows(t *testing.T, ctx context.Context, ch *ClickHouse, target, rib, family string) []vpnLgRow {
	t.Helper()
	return vpnRowsFrom(t, ctx, ch, panelSQL(t, "looking-glass-vpn", "Covering routes", "A"), target, rib, family)
}

// scopeToVpnFixtureRouter hangs a router predicate off the panel's own time
// filter. It fails rather than guesses if that anchor is not present exactly
// once, so a rewritten panel cannot silently stop being scoped.
func scopeToVpnFixtureRouter(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "WHERE $__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("expected exactly 1 %q to anchor the fixture-router scope, found %d; "+
			"the panel was rewritten and this helper must be updated with it", anchor, n)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip = toIPv6('"+vpnRouter+"')", 1)
}

func vpnRowsFrom(t *testing.T, ctx context.Context, ch *ClickHouse, sql, target, rib, family string) []vpnLgRow {
	t.Helper()
	sql = substituteGrafana(sql, map[string]string{"target": target, "rib": rib, "family": family})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("vpn covering routes (target=%q family=%q): %v\nSQL:\n%s", target, family, err, sql)
	}
	defer rows.Close()
	var out []vpnLgRow
	for rows.Next() {
		var r vpnLgRow
		if err := rows.Scan(&r.collector, &r.prefix, &r.rd, &r.family, &r.router,
			&r.peer, &r.rib, &r.state, &r.nextHop, &r.routeTargets, &r.labels,
			&r.asPath, &r.lastSeen, &r.sessionCurrent); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// The question this dashboard exists for, and the one the lab archive cannot
// ask: one prefix, exported by two VRFs. Both rows must come back, told apart
// by RD and by route target -- a query keyed on prefix alone returns one row
// and silently picks a VRF.
func TestVpnLookingGlassSeparatesTheSamePrefixInTwoVrfs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertVpnLookingGlassFixture(t, ctx, ch)

	got := vpnLookingGlassRows(t, ctx, ch, "10.88.5.7", ".*", ".*")
	if len(got) != 2 {
		t.Fatalf("covering 10.88.5.7 returned %d rows, want 2 -- one per VRF: %v", len(got), got)
	}
	byRD := map[string]vpnLgRow{}
	for _, r := range got {
		byRD[r.rd] = r
	}
	for _, rd := range []string{vpnRDa, vpnRDb} {
		r, ok := byRD[rd]
		if !ok {
			t.Errorf("no row for RD %s: %v", rd, byRD)
			continue
		}
		if r.routeTargets != rd {
			t.Errorf("RD %s carries route targets %q, want %q -- the RT is what "+
				"actually decides VRF import, so a row that loses it cannot "+
				"answer the question", rd, r.routeTargets, rd)
		}
	}
}

// route_vpn is really "labeled routes", discriminated by family: lu4 rows
// carry an empty RD BY DESIGN (bgp/vpn.go), and 45% of the live table is lu4.
// The RD column must say so in words rather than rendering a blank cell that
// reads as missing data, and $family must be able to separate the two.
func TestVpnLookingGlassLabelsTheFamilyWithNoRD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertVpnLookingGlassFixture(t, ctx, ch)

	lu := vpnLookingGlassRows(t, ctx, ch, "10.88.9.1", ".*", ".*")
	if len(lu) != 1 {
		t.Fatalf("covering 10.88.9.1 returned %d rows, want 1: %v", len(lu), lu)
	}
	if lu[0].family != "lu4" {
		t.Errorf("family = %q, want lu4", lu[0].family)
	}
	if lu[0].rd == "" {
		t.Errorf("the lu4 row renders an empty RD cell; it must say the family " +
			"carries no RD, or a blank reads as missing data")
	}
	if lu[0].labels != "24099" {
		t.Errorf("labels = %q, want 24099 -- the label is the whole content of "+
			"an lu4 route", lu[0].labels)
	}

	// $family must discriminate, in both directions.
	if got := vpnLookingGlassRows(t, ctx, ch, "10.88.9.1", ".*", "vpn4"); len(got) != 0 {
		t.Errorf("$family=vpn4 returned %d rows for an lu4 prefix, want 0", len(got))
	}
	if got := vpnLookingGlassRows(t, ctx, ch, "10.88.5.7", ".*", "lu4"); len(got) != 0 {
		t.Errorf("$family=lu4 returned %d rows for a vpn4 prefix, want 0", len(got))
	}
	if got := vpnLookingGlassRows(t, ctx, ch, "10.88.5.7", ".*", "vpn4"); len(got) != 2 {
		t.Errorf("$family=vpn4 returned %d rows for the two-VRF prefix, want 2", len(got))
	}
}

// The same three properties the unicast looking glass holds: a withdrawn route
// is shown as withdrawn rather than hidden, an observation from a superseded
// session is flagged rather than dropped, and no input can raise.
func TestVpnLookingGlassHoldsStateStalenessAndBadInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertVpnLookingGlassFixture(t, ctx, ch)

	wd := vpnLookingGlassRows(t, ctx, ch, "10.88.7.1", ".*", ".*")
	if len(wd) != 1 || wd[0].state != "withdrawn" {
		t.Errorf("covering 10.88.7.1 returned %v, want one row marked withdrawn", wd)
	}
	stale := vpnLookingGlassRows(t, ctx, ch, "10.88.8.1", ".*", ".*")
	if len(stale) != 1 {
		t.Fatalf("covering 10.88.8.1 returned %d rows, want 1 -- heard only in "+
			"the superseded session, and must still be shown", len(stale))
	}
	if stale[0].sessionCurrent != 0 {
		t.Errorf("session_current = %d for a superseded-session route, want 0",
			stale[0].sessionCurrent)
	}
	for _, bad := range []string{"", "not-an-ip", "o'brien", "'; SELECT 1 --"} {
		if got := vpnLookingGlassAllRows(t, ctx, ch, bad, ".*", ".*"); bad != "" && len(got) != 0 {
			t.Errorf("target %q returned %d rows, want 0 -- and it must return "+
				"them rather than erroring the panel", bad, len(got))
		}
	}
}

const (
	asnRouter = "10.0.199.8"
	asnPeer   = "10.255.199.11"
	// asnPeerASN appears in NO as_path this fixture writes, on purpose. The
	// ASN view is a question about as_path, but route_unicast also carries
	// peer_asn, and the two are easy to confuse. A panel that reached for
	// peer_asn would surface an ASN the assertions below never expect.
	asnPeerASN = 65590
	// asnStale is numerically LESS than asnSession, which is the ordering a
	// collector produces: session ids come from now().UnixNano() behind a
	// monotonic guard, so a superseded session always carries the lower id.
	// A lab archive agrees -- 1,208 session transitions across 15 router
	// views, zero inversions of id against first-seen time.
	//
	// It used to be inverted on purpose, so that a panel reaching for
	// max(session_id) landed on the wrong session and the superseded-session
	// assertions could tell the two resolutions apart. That inverted the one
	// thing production guarantees in order to test a resolution that was
	// itself reversed on 2026-09-04; see dashSession's comment for why
	// max(session_id) won. The expression is pinned statically now, by
	// TestDashboardsResolveTheCurrentSessionTheWayQueryDoes.
	asnSession = 9702
	asnStale   = 9701
)

// The ASNs this fixture writes. The lab archive's seven split cleanly into
// four origin-only and three transit-only, carry no prepending, no
// withdrawals and no path longer than two -- so every shape that makes the
// ASN view's arithmetic decidable has to be authored here.
const (
	// asnBoth originates two live prefixes AND transits a third. No ASN in
	// the lab archive does both, so "role" is only falsifiable against this one.
	asnBoth = 65520
	// asnPrepend transits ONE prefix whose path names it twice. A panel
	// counting arrayJoin(as_path) rather than distinct prefixes reports it
	// as two, which is the whole point of prepending it.
	asnPrepend = 65521
	// asnBothOrigin is what asnBoth transits FOR: it is the origin of the
	// prefix that proves asnBoth is not origin-only.
	asnBothOrigin = 65522
	// The deep path. Five hops, where the lab archive's longest is two.
	asnDeep1    = 65523
	asnDeep2    = 65524
	asnDeep3    = 65525
	asnDeepOrig = 65526
	// asnStaleHop and asnStaleOrig appear ONLY on a route heard in the
	// superseded session.
	asnStaleHop  = 65527
	asnStaleOrig = 65528
	// asnSecondUp is asnBoth's second upstream neighbor. Two distinct
	// neighbors is the fan-in shape the lab archive shows for AS 64512, and the
	// adjacency panel is wrong if it collapses them.
	asnSecondUp = 65529
	// asnLocRibHop and asnLocRibOrig appear ONLY on a route in the loc_rib
	// stream. Every other route this fixture writes is in_pre, so without
	// them $rib is a control every assertion renders as All and nothing
	// notices when a panel stops reading it.
	asnLocRibHop  = 65532
	asnLocRibOrig = 65533
	// asnGhost appears ONLY on the end-of-RIB marker's path attributes. An
	// EoR row is not a route, so this ASN must never reach a panel -- and
	// it does reach them the moment a marker is filed into route_unicast
	// again, because a marker carries the envelope's attrs like any other
	// row and every panel reads as_path.
	asnGhost = 65534
)

// asnPrefixes are the routes this fixture writes, in a block no other test
// uses (testDB is shared and never truncated).
const (
	asnPfxPrepend   = "10.201.10.0/24" // [65521 65521 65520] -- prepended
	asnPfxTransit   = "10.201.11.0/24" // [65520 65522] -- asnBoth as transit
	asnPfxDeep      = "10.201.12.0/24" // five hops
	asnPfxWithdrawn = "10.201.13.0/24" // [65521 65520], then withdrawn
	asnPfxStale     = "10.201.14.0/24" // superseded session only
	asnPfxSecondUp  = "10.201.15.0/24" // [65529 65520] -- the fan-in
	asnPfxLocRib    = "10.201.16.0/24" // the only route outside in_pre
)

// insertASNViewFixture writes the routes the ASN view is specified against,
// through the real RowsFor + Insert path, under reserved identifiers.
//
// In one place it carries every shape the lab archive cannot produce: an
// ASN that both originates and transits; a path that prepends one ASN; a
// five-hop path where the lab archive's longest is two; a route whose latest
// observation is a withdrawal; a route resting on a superseded session; an
// origin reached through two different neighbors; and an end-of-RIB marker
// with no prefix at all, which must never be counted as a route.
func insertASNViewFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	// envAt stamps the two clocks separately. While every envelope carried
	// the same value in both, swapping ts_collector for ts_router in a panel
	// changed no test here -- which is exactly how the looking glass shipped
	// a review-caught clock bug, and why the withdrawal below is stamped with
	// an epoch ts_router.
	envAt := func(session uint64, tsRouter, tsCollector time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: asnRouter, SysName: "dash-asn1"},
			Peer:        &vantagev1.PeerId{Ip: asnPeer, Asn: asnPeerASN},
			SessionId:   session,
			TsRouter:    timestamppb.New(tsRouter),
			TsCollector: timestamppb.New(tsCollector),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	env := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return envAt(session, ts, ts, r)
	}
	// locRib stamps an envelope whose peer is an RFC 9069 Loc-RIB peer, which
	// is what rows.go turns into rib = 'loc_rib'.
	locRib := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := envAt(session, ts, ts, r)
		e.Peer = &vantagev1.PeerId{
			Ip: asnPeer, Asn: asnPeerASN,
			Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
		}
		return e
	}
	peerUp := func(session uint64, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: asnRouter, SysName: "dash-asn1"},
			Peer:        &vantagev1.PeerId{Ip: asnPeer, Asn: asnPeerASN},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
				Kind: vantagev1.PeerEvent_KIND_UP,
			}},
		}
	}
	attrs := func(asns ...uint32) *vantagev1.PathAttributes {
		return &vantagev1.PathAttributes{
			Origin:  0,
			NextHop: "10.201.0.1",
			AsPath:  []*vantagev1.AsPathSegment{{Type: 2, Asns: asns}},
		}
	}
	adv := func(prefix string, asns ...uint32) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{
			Family:    &vantagev1.Family{Afi: 1, Safi: 1},
			Attrs:     attrs(asns...),
			Announced: []*vantagev1.Prefix{{Prefix: prefix}},
		}
	}
	now := corpusCollectorClock

	envs := []*vantagev1.Envelope{
		// The stale session's Peer Up is OLDER and its id is LOWER (asnStale
		// 9701, asnSession 9702), so both the old argMax-on-time resolution
		// and the max(session_id) the dashboards converged on in 2026-09-04
		// name asnSession current.
		peerUp(asnStale, now.Add(-3*time.Hour)),
		peerUp(asnSession, now.Add(-time.Hour)),

		// asnPrepend appears twice in one path. Its prefix count must not.
		env(asnSession, now, adv(asnPfxPrepend, asnPrepend, asnPrepend, asnBoth)),
		// asnBoth in transit position, so it is not origin-only.
		env(asnSession, now, adv(asnPfxTransit, asnBoth, asnBothOrigin)),
		// Five hops.
		env(asnSession, now, adv(asnPfxDeep, asnPrepend, asnDeep1, asnDeep2, asnDeep3, asnDeepOrig)),
		// asnBoth's second upstream: the fan-in.
		env(asnSession, now, adv(asnPfxSecondUp, asnSecondUp, asnBoth)),
		// Advertised, then withdrawn. The latest observation wins, and the
		// route must still be listed -- marked -- rather than vanish.
		//
		// The withdrawal carries an EPOCH ts_router -- what QK_TS_ZERO
		// describes -- and a LATER ts_collector, because the collector heard
		// it second. The two clocks therefore disagree about which
		// observation is the newest, and they disagree in the direction that
		// matters: resolved on ts_router the advertisement wins and this
		// route reads "advertised" though it is gone. Resolved on
		// ts_collector, which is what every panel does, the withdrawal wins.
		//
		// ts_collector stays recent on purpose: schema.sql keys PARTITION BY
		// and the 90-day TTL on that column, so an epoch value there would
		// drop the row at the next merge and take the test with it.
		env(asnSession, now, adv(asnPfxWithdrawn, asnPrepend, asnBoth)),
		envAt(asnSession, lgZeroClock, now.Add(time.Minute), &vantagev1.RouteEvent{
			Family:    &vantagev1.Family{Afi: 1, Safi: 1},
			Attrs:     attrs(asnPrepend, asnBoth),
			Withdrawn: []*vantagev1.Prefix{{Prefix: asnPfxWithdrawn}},
		}),
		// Heard only in the superseded session.
		env(asnStale, now.Add(-2*time.Hour), adv(asnPfxStale, asnStaleHop, asnStaleOrig)),
		// The only route outside in_pre, so $rib is a control that changes
		// an answer rather than one every test renders as All.
		locRib(asnSession, now, adv(asnPfxLocRib, asnLocRibHop, asnLocRibOrig)),
		// No prefix at all: an end-of-RIB marker must never be a route.
		//
		// It carries path attributes on purpose. A marker takes the
		// envelope's attrs like any other row, so this one puts asnGhost in
		// an as_path -- and the ONLY thing keeping a ghost ASN out of the
		// picker and every panel is that routeRows files it into eor_events
		// instead of route_unicast. Without the attrs the notEmpty(as_path)
		// filter did that job incidentally, and breaking the filing decision
		// changed nothing.
		env(asnSession, now, &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 1}, EndOfRib: true,
			Attrs: attrs(asnGhost),
		}),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert asn-view fixture envelope %d: %v", i, err)
		}
	}
}

// The fixture's arrangement is what makes every ASN-view assertion below
// decidable, so it is pinned here rather than trusted. An innocent edit that
// drops the prepend, or makes asnBoth origin-only, or lets asnStale become
// the current session, turns those assertions into tests that pass whether
// or not the clause they name exists.
func TestASNViewFixtureIsArrangedAdversarially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	// peer_events must name asnSession current, resolved the way the panels
	// resolve it: max(session_id). asnStale is the SMALLER id (9701 against
	// 9702) and its Peer Up is also the older of the two, so both resolutions
	// agree here -- but this guard has to read the one the panels read, for
	// the reason TestLookingGlassFixtureIsArrangedAdversarially gives.
	var sid uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT max(session_id) FROM vantage.peer_events FINAL "+
			"WHERE router_ip = toIPv6('"+asnRouter+"')")).Scan(&sid); err != nil {
		t.Fatalf("current session: %v", err)
	}
	if sid != asnSession {
		t.Errorf("current session = %d, want %d -- with the stale session "+
			"current, the superseded-route assertions cannot fail", sid, asnSession)
	}

	// asnBoth must originate AND transit. If it only originates, the role
	// column is satisfied by "every ASN is an origin" and proves nothing.
	var originates, transits uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, `
		SELECT countIf(is_origin), countIf(NOT is_origin) FROM (
			SELECT prefix, arrayExists(x -> x = `+fmt.Sprint(asnBoth)+`, as_path) AS present,
			       as_path[-1] = `+fmt.Sprint(asnBoth)+` AS is_origin
			FROM vantage.route_unicast FINAL
			WHERE router_ip = toIPv6('`+asnRouter+`') AND present
		)`)).Scan(&originates, &transits); err != nil {
		t.Fatalf("asnBoth roles: %v", err)
	}
	if originates == 0 || transits == 0 {
		t.Errorf("asnBoth %d originates %d and transits %d; both must be "+
			"non-zero or an origin-only reading of as_path passes",
			asnBoth, originates, transits)
	}

	// Exactly one path must name asnPrepend twice.
	var prepended uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.route_unicast FINAL "+
			"WHERE router_ip = toIPv6('"+asnRouter+"') "+
			"AND length(as_path) != length(arrayDistinct(as_path))")).Scan(&prepended); err != nil {
		t.Fatalf("prepend count: %v", err)
	}
	if prepended != 1 {
		t.Errorf("prepended paths = %d, want 1 -- without one, counting "+
			"arrayJoin(as_path) and counting distinct prefixes agree", prepended)
	}

	// The deep path must be deeper than anything the lab archive holds.
	var longest uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT toUInt64(max(length(as_path))) FROM vantage.route_unicast FINAL "+
			"WHERE router_ip = toIPv6('"+asnRouter+"')")).Scan(&longest); err != nil {
		t.Fatalf("longest path: %v", err)
	}
	if longest != 5 {
		t.Errorf("longest as_path = %d, want 5 -- the lab archive tops out "+
			"at 2, so path-depth is only decidable on this fixture", longest)
	}

	// The withdrawn prefix's LATEST observation must be the withdrawal.
	var withdrawn uint8
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT argMax(is_withdraw, (ts_collector, stream_seq)) FROM vantage.route_unicast FINAL "+
			"WHERE router_ip = toIPv6('"+asnRouter+"') AND prefix = '"+asnPfxWithdrawn+"'")).Scan(&withdrawn); err != nil {
		t.Fatalf("withdrawn state: %v", err)
	}
	if withdrawn != 1 {
		t.Errorf("%s latest is_withdraw = %d, want 1 -- the advertisement "+
			"must not be the newer row", asnPfxWithdrawn, withdrawn)
	}

	// asnBoth must be reached through two DIFFERENT neighbors, or the
	// adjacency panel cannot be caught collapsing them.
	var upstreams uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, `
		SELECT uniqExact(nbr) FROM (
			SELECT as_path[indexOf(as_path, `+fmt.Sprint(asnBoth)+`) - 1] AS nbr
			FROM vantage.route_unicast FINAL
			WHERE router_ip = toIPv6('`+asnRouter+`')
			  AND as_path[-1] = `+fmt.Sprint(asnBoth)+`
			  AND indexOf(as_path, `+fmt.Sprint(asnBoth)+`) > 1
		)`)).Scan(&upstreams); err != nil {
		t.Fatalf("upstream fan-in: %v", err)
	}
	if upstreams != 2 {
		t.Errorf("asnBoth %d reached via %d distinct neighbours, want 2 -- "+
			"the fan-in is what the adjacency panel exists to show",
			asnBoth, upstreams)
	}

	// The end-of-RIB marker must still be written, and it must be written
	// into eor_events rather than into route_unicast.
	//
	// This used to assert a prefix-less route_unicast row, which is what
	// made "end_of_rib = 0" load-bearing in every panel rather than
	// decorative. eor_events replaces that predicate with a property of the
	// table, so the assertion moves with it: the marker must exist (or
	// TestASNViewNeverCountsTheEndOfRibMarkerAsARoute is asserting the
	// absence of something nothing wrote), and route_unicast must hold no
	// prefix-less row for a panel to pick up.
	var eor uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.eor_events "+
			"WHERE router_ip = toIPv6('"+asnRouter+"')")).Scan(&eor); err != nil {
		t.Fatalf("end-of-rib: %v", err)
	}
	if eor == 0 {
		t.Error("the fixture wrote no end-of-RIB marker, so the ghost-ASN " +
			"assertions are asserting the absence of something nothing wrote")
	}
	var prefixless uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.route_unicast FINAL "+
			"WHERE router_ip = toIPv6('"+asnRouter+"') AND prefix = ''")).Scan(&prefixless); err != nil {
		t.Fatalf("prefix-less rows: %v", err)
	}
	if prefixless != 0 {
		t.Errorf("route_unicast holds %d prefix-less row(s) for %s -- the "+
			"marker is filed in eor_events now, and a route table that still "+
			"collects artifacts puts the ghost ASN back in every panel",
			prefixless, asnRouter)
	}
}

// scopeASNViewToFixtureRouter narrows an ASN-view query to the one router
// this fixture owns, by extending the panel's own time filter.
//
// testDB is shared and never truncated, and the ASN view has no $router
// control: every route in the database contributes to its counts. The
// reserved 655xx ASNs keep another fixture out of these rows today, but an
// exact count is a claim about THIS fixture, so it is asked of this
// fixture's router rather than of whatever else the corpus happens to hold.
//
// Anything but exactly one occurrence of the anchor is fatal: a rewrite that
// moved it must fail loudly here rather than quietly stop scoping.
func scopeASNViewToFixtureRouter(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("an asn-view query carries %d occurrences of %q, want 1 -- "+
			"these tests scope to the fixture's router by extending that "+
			"filter, and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip = toIPv6('"+asnRouter+"')", 1)
}

// asnInvRow is one row of the committed "ASNs" inventory table.
type asnInvRow struct {
	asn        uint32
	originated uint64
	transited  uint64
	routers    uint64
	neighbours uint64
}

// asnInventory runs the committed inventory table with $asn and $rib rendered
// as Grafana would render them, scoped to the fixture's router, and returns
// it keyed by ASN.
func asnInventory(t *testing.T, ctx context.Context, ch *ClickHouse, asn, rib string) map[uint32]asnInvRow {
	t.Helper()
	raw := scopeASNViewToFixtureRouter(t, panelSQL(t, "asn-view", "ASNs", "A"))
	sql := substituteGrafana(raw, map[string]string{"asn": asn, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("asn inventory (asn=%q): %v\nSQL:\n%s", asn, err, sql)
	}
	defer rows.Close()
	out := map[uint32]asnInvRow{}
	for rows.Next() {
		var r asnInvRow
		if err := rows.Scan(&r.asn, &r.originated, &r.transited, &r.routers, &r.neighbours); err != nil {
			t.Fatalf("scan inventory row: %v", err)
		}
		out[r.asn] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("inventory rows: %v", err)
	}
	return out
}

// asnPrefixRow is one row of the committed "Prefixes" table.
type asnPrefixRow struct {
	collector      string
	prefix         string
	asn            uint32
	role           string
	asPath         string
	router         string
	peer           string
	rib            string
	state          string
	nextHop        string
	lastSeen       time.Time
	sessionCurrent uint8
}

// asnPrefixColumns is the committed column order of the "Prefixes" table, in
// full, because the scan below is POSITIONAL: every field lands wherever the
// column at its index happens to be.
//
// The leading `collector` column shifts the eleven after it by one -- a
// change a positional scan absorbs in silence, reading a prefix into `role`
// and still passing whatever assertion only reads `asn`. Positional scans
// have had to be repaired more than once for the same reason. Naming the whole
// order rather than only its ends means a swap in the MIDDLE fails here too,
// which is the case first-and-last checks miss.
var asnPrefixColumns = []string{
	"collector", "prefix", "asn", "role", "as_path", "router", "peer",
	"rib", "state", "next_hop", "last_seen", "session_current",
}

func asnPrefixes(t *testing.T, ctx context.Context, ch *ClickHouse, asn, rib string) []asnPrefixRow {
	t.Helper()
	return asnPrefixesFrom(t, ctx, ch,
		scopeASNViewToFixtureRouter(t, panelSQL(t, "asn-view", "Prefixes", "A")),
		asn, rib)
}

// dualHomedASNPrefixes runs the same committed panel against the router two
// collectors both watch, rather than against the ASN view's own single
// collector fixture.
func dualHomedASNPrefixes(t *testing.T, ctx context.Context, ch *ClickHouse, asn string) []asnPrefixRow {
	t.Helper()
	return asnPrefixesFrom(t, ctx, ch,
		scopeToDualHomedRouter(t, panelSQL(t, "asn-view", "Prefixes", "A")),
		asn, ".*")
}

func asnPrefixesFrom(t *testing.T, ctx context.Context, ch *ClickHouse, raw, asn, rib string) []asnPrefixRow {
	t.Helper()
	sql := substituteGrafana(raw, map[string]string{"asn": asn, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("asn prefixes (asn=%q): %v\nSQL:\n%s", asn, err, sql)
	}
	defer rows.Close()
	if got := rows.Columns(); !slices.Equal(got, asnPrefixColumns) {
		t.Fatalf("the Prefixes table returns columns %v, want %v -- the scan "+
			"below is positional, so a reordered or inserted column silently "+
			"moves every field after it", got, asnPrefixColumns)
	}
	var out []asnPrefixRow
	for rows.Next() {
		var r asnPrefixRow
		if err := rows.Scan(&r.collector, &r.prefix, &r.asn, &r.role, &r.asPath,
			&r.router, &r.peer, &r.rib, &r.state, &r.nextHop, &r.lastSeen,
			&r.sessionCurrent); err != nil {
			t.Fatalf("scan prefix row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("prefix rows: %v", err)
	}
	return out
}

// asnAdjRow is one row of the committed "Adjacent ASNs" table.
type asnAdjRow struct {
	asn       uint32
	neighbour uint32
	direction string
	prefixes  uint64
}

func asnAdjacency(t *testing.T, ctx context.Context, ch *ClickHouse, asn, rib string) []asnAdjRow {
	t.Helper()
	raw := scopeASNViewToFixtureRouter(t, panelSQL(t, "asn-view", "Adjacent ASNs", "A"))
	sql := substituteGrafana(raw, map[string]string{"asn": asn, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("asn adjacency (asn=%q): %v\nSQL:\n%s", asn, err, sql)
	}
	defer rows.Close()
	var out []asnAdjRow
	for rows.Next() {
		var r asnAdjRow
		if err := rows.Scan(&r.asn, &r.neighbour, &r.direction, &r.prefixes); err != nil {
			t.Fatalf("scan adjacency row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("adjacency rows: %v", err)
	}
	return out
}

// An AS_PATH is ordered nearest-first: the LAST element is the origin. Every
// panel here turns on that, and reading as_path[1] instead is not a crash --
// it is a dashboard that confidently attributes every prefix to the wrong
// network. The fixture makes the two readings disagree in both directions:
// asnPrepend leads three paths and originates none of them, and asnBoth
// originates two while sitting first in a third.
func TestASNViewOriginIsTheLastASNInThePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	inv := asnInventory(t, ctx, ch, ".*", ".*")

	// asnBoth originates two LIVE prefixes. The third, asnPfxWithdrawn, is
	// withdrawn: the inventory counts what is still advertised, so 3 here
	// means the withdrawal was ignored and 1 means a live route was lost.
	if got := inv[asnBoth].originated; got != 2 {
		t.Errorf("AS %d originates %d prefixes, want 2 (%s and %s; %s is "+
			"withdrawn) -- 1 would mean as_path[1] is being read as the origin",
			asnBoth, got, asnPfxPrepend, asnPfxSecondUp, asnPfxWithdrawn)
	}
	// asnPrepend leads three paths and originates nothing.
	if got := inv[asnPrepend].originated; got != 0 {
		t.Errorf("AS %d originates %d prefixes, want 0 -- it is first in "+
			"three paths and last in none, so anything above zero means the "+
			"panel is reading the near end of as_path", asnPrepend, got)
	}
	// And it must still appear, as a transit-only ASN.
	if _, present := inv[asnPrepend]; !present {
		t.Errorf("AS %d is absent from the inventory; an ASN that only "+
			"transits still has a row, or three of the lab archive's seven "+
			"vanish from the dashboard", asnPrepend)
	}
}

// Prepending is how an operator makes a path look longer; it must not make
// the prefix look like two. asnPrepend leads asnPfxPrepend twice, so a panel
// counting arrayJoin(as_path) reports three transited prefixes where there
// are two.
func TestASNViewPrependedPathCountsOnePrefixOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	inv := asnInventory(t, ctx, ch, ".*", ".*")
	if got := inv[asnPrepend].transited; got != 2 {
		t.Errorf("AS %d transits %d prefixes, want 2 (%s and %s; %s is "+
			"withdrawn) -- 3 is what counting arrayJoin(as_path) returns, "+
			"because %s names it twice",
			asnPrepend, got, asnPfxPrepend, asnPfxDeep, asnPfxWithdrawn, asnPfxPrepend)
	}

	// The counts above are uniqExact, so they survive a duplicated row. The
	// prefix TABLE does not: without the arrayDistinct that collapses a
	// repeated ASN, %s appears in it once per prepend.
	var seen int
	for _, r := range asnPrefixes(t, ctx, ch, fmt.Sprint(asnPrepend), ".*") {
		if r.prefix == asnPfxPrepend {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("%s appears %d times in the prefix table for AS %d, want 1 "+
			"-- its path names that ASN twice, and one route is one row",
			asnPfxPrepend, seen, asnPrepend)
	}

	// Nor may an ASN prepended next to itself count as its own neighbor.
	// asnPrepend touches two other ASNs across its two live prefixes.
	if got := inv[asnPrepend].neighbours; got != 2 {
		t.Errorf("AS %d has %d neighbours, want 2 (AS %d via %s and AS %d via "+
			"%s) -- 3 means the copy of itself that %s prepends was counted",
			asnPrepend, got, asnBoth, asnPfxPrepend, asnDeep1, asnPfxDeep,
			asnPfxPrepend)
	}
}

// The looking glass shows a withdrawn route marked rather than omitting it,
// because "this prefix is gone" is the answer an operator came for. The ASN
// view's prefix table follows the same rule -- and its inventory does NOT,
// counting only what is still advertised. Both halves are asserted here so
// the two cannot quietly converge on one behavior.
func TestASNViewListsWithdrawnRoutesMarkedRatherThanDropping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	byPrefix := map[string]asnPrefixRow{}
	for _, r := range asnPrefixes(t, ctx, ch, fmt.Sprint(asnBoth), ".*") {
		byPrefix[r.prefix] = r
	}
	w, present := byPrefix[asnPfxWithdrawn]
	if !present {
		t.Fatalf("%s is absent from the prefix table for AS %d; a withdrawn "+
			"route is shown marked, not dropped -- the operator asking "+
			"\"where did it go\" gets an empty table otherwise",
			asnPfxWithdrawn, asnBoth)
	}
	if w.state != "withdrawn" {
		t.Errorf("%s state = %q, want \"withdrawn\"", asnPfxWithdrawn, w.state)
	}
	if w.role != "origin" {
		t.Errorf("%s role = %q, want \"origin\" -- AS %d is last in its path",
			asnPfxWithdrawn, w.role, asnBoth)
	}
	// The transited prefix appears under the same ASN, marked differently.
	if got := byPrefix[asnPfxTransit].role; got != "transit" {
		t.Errorf("%s role = %q, want \"transit\" -- AS %d is first in that "+
			"path, not last", asnPfxTransit, got, asnBoth)
	}
}

// The one shape the lab archive does show is fan-in: AS 64512 is reached through
// three different neighbors. A panel that collapsed neighbors -- or that
// let a prepended ASN become its own neighbor -- would turn that into one
// row and hide the multi-homing the dashboard exists to reveal.
func TestASNViewAdjacencyKeepsBothUpstreamsApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	var toward []uint32
	for _, r := range asnAdjacency(t, ctx, ch, fmt.Sprint(asnBoth), ".*") {
		if r.asn == asnBoth && r.direction == "toward collector" {
			toward = append(toward, r.neighbour)
		}
	}
	slices.Sort(toward)
	want := []uint32{asnPrepend, asnSecondUp}
	if !slices.Equal(toward, want) {
		t.Errorf("AS %d neighbours toward the collector = %v, want %v -- "+
			"two distinct upstreams is the multi-homing this panel exists "+
			"to show", asnBoth, toward, want)
	}

	// Prepending must not make an ASN adjacent to itself.
	for _, r := range asnAdjacency(t, ctx, ch, fmt.Sprint(asnPrepend), ".*") {
		if r.asn == r.neighbour {
			t.Errorf("AS %d is listed as its own neighbour; %s prepends it "+
				"and consecutive duplicates are not an adjacency",
				r.asn, asnPfxPrepend)
		}
	}
}

// $focus and $peer both shipped offering values no panel could render. The
// ASN picker is built from as_path so it cannot repeat that -- and this is
// the test that says so, by taking every option the committed variable query
// returns and requiring the prefix table to answer for it.
func TestASNViewPickerOffersOnlyASNsThePanelsRender(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	vars := dashboardQueryVariables(t, "asn-view")
	q, ok := vars["asn"]
	if !ok {
		t.Fatalf("asn-view declares no query variable named asn; it declares %v",
			slices.Sorted(maps.Keys(vars)))
	}
	// Scope the picker to this fixture's router the same way the panels are
	// scoped, so the assertion is about ASNs this test put there.
	scoped := scopeASNViewToFixtureRouter(t, q)
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(scoped, nil)))
	if err != nil {
		t.Fatalf("asn picker query: %v\nSQL:\n%s", err, scoped)
	}
	var offered []uint32
	for rows.Next() {
		var a uint32
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan picker option: %v", err)
		}
		offered = append(offered, a)
	}
	rows.Close()

	if len(offered) != 12 {
		t.Errorf("picker offers %d ASNs for this fixture, want 12 -- it "+
			"writes twelve distinct ASNs across origin and transit positions "+
			"in two RIB streams; got %v", len(offered), offered)
	}
	if slices.Contains(offered, uint32(asnGhost)) {
		t.Errorf("picker offers AS %d, which appears only on an end-of-RIB "+
			"marker; the picker would then offer an ASN with no routes at all",
			asnGhost)
	}
	for _, a := range offered {
		if got := asnPrefixes(t, ctx, ch, fmt.Sprint(a), ".*"); len(got) == 0 {
			t.Errorf("picker offers AS %d and the prefix table renders no "+
				"rows for it -- that is exactly how $focus shipped broken", a)
		}
	}
}

// Every route this fixture writes is in_pre except one, which is the only
// reason $rib is a control that can be caught not being read. Every other
// assertion in this file renders it as All, and All is the branch that looks
// identical whether or not the clause exists.
func TestASNViewRibNarrowsToTheChosenStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	all := asnInventory(t, ctx, ch, ".*", ".*")
	if _, present := all[asnLocRibOrig]; !present {
		t.Fatalf("AS %d is absent with $rib on All; it originates %s in "+
			"loc_rib and All must include it", asnLocRibOrig, asnPfxLocRib)
	}
	inPre := asnInventory(t, ctx, ch, ".*", "in_pre")
	if _, present := inPre[asnLocRibOrig]; present {
		t.Errorf("AS %d is still listed with $rib = in_pre; its only route is "+
			"in loc_rib, so the panel is not reading $rib at all", asnLocRibOrig)
	}
	// The converse, so the test cannot pass by returning nothing.
	if _, present := inPre[asnBoth]; !present {
		t.Errorf("AS %d vanished with $rib = in_pre, where all its routes "+
			"live -- the filter is excluding too much", asnBoth)
	}
}

// An end-of-RIB marker is not a route. It carries no prefix, but it does
// carry the envelope's path attributes, so an ASN named only there is one
// misfiled marker away from appearing in the picker and in every panel as an
// AS that originates nothing, anywhere. eor_events is what keeps it out --
// this is the end-to-end check that the filing decision in rows.go actually
// reaches what an operator sees.
func TestASNViewNeverCountsTheEndOfRibMarkerAsARoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	if row, present := asnInventory(t, ctx, ch, ".*", ".*")[asnGhost]; present {
		t.Errorf("AS %d is in the inventory (%+v); it appears only on an "+
			"end-of-RIB marker's attributes, and a marker is not a route",
			asnGhost, row)
	}
	if got := asnPrefixes(t, ctx, ch, fmt.Sprint(asnGhost), ".*"); len(got) != 0 {
		t.Errorf("the prefix table returns %d rows for AS %d, want 0 -- an "+
			"end-of-RIB row has no prefix, so those rows name the empty "+
			"string: %+v", len(got), asnGhost, got)
	}
}

// A row of the prefix table is a (prefix, ASN) pair, not a prefix: one route
// whose path names four ASNs contributes four rows. With $asn on a single
// ASN that is invisible, and every other test here picks one -- but on All,
// which is how the dashboard opens, the same prefix appears once per ASN in
// its path, and adjacent rows disagree about whether it is `origin` or
// `transit`. Without a column naming the ASN each row is about, that reads
// as a table contradicting itself.
func TestASNViewPrefixTableNamesTheASNEachRowIsAbout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	var got []string
	for _, r := range asnPrefixes(t, ctx, ch, ".*", ".*") {
		if r.prefix == asnPfxPrepend {
			got = append(got, fmt.Sprintf("%d/%s", r.asn, r.role))
		}
	}
	slices.Sort(got)
	want := []string{
		fmt.Sprintf("%d/origin", asnBoth),
		fmt.Sprintf("%d/transit", asnPrepend),
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s on $asn = All renders %v, want %v -- its path names two "+
			"ASNs, so it is two rows, and each has to say which ASN it "+
			"describes", asnPfxPrepend, got, want)
	}
}

// TestASNViewPrefixTableFlagsASupersededSessionRow holds the OTHER side of
// session_current.
//
// This table is deliberately not scoped to the current session -- most
// routers in the lab archive have no prefixes in their newest one -- so the
// column is the entire signal that an observation is stale, and until this
// test existed nothing on this dashboard read it. Every asn-view assertion
// scanned the column and discarded it, and
// TestDualHomedASNViewKeepsBothCollectorsPrefixes asserts only that live rows
// read 1: on that evidence alone the expression could be replaced by the
// literal 1 with the suite still green.
//
// The fixture takes both sides. asnPfxStale was heard ONLY in asnStale, which
// peer_events has since superseded, and every other route here was heard in
// asnSession, which is current.
func TestASNViewPrefixTableFlagsASupersededSessionRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	var stale, live int
	for _, r := range asnPrefixes(t, ctx, ch, ".*", ".*") {
		if r.prefix == asnPfxStale {
			stale++
			if r.sessionCurrent != 0 {
				t.Errorf("%s (AS %d) reads session_current=%d, want 0 -- it was "+
					"heard only in session %d, which %d has superseded, and a 1 "+
					"here tells an operator a dead session's route is live",
					r.prefix, r.asn, r.sessionCurrent, asnStale, asnSession)
			}
			continue
		}
		live++
		if r.sessionCurrent != 1 {
			t.Errorf("%s (AS %d) reads session_current=%d, want 1 -- it was heard "+
				"in the current session %d", r.prefix, r.asn, r.sessionCurrent,
				asnSession)
		}
	}
	// Both counts must be non-zero, or one of the two branches above never
	// ran and the column is asserted in one direction only.
	if stale == 0 || live == 0 {
		t.Errorf("the prefix table rendered %d row(s) for the superseded-session "+
			"route %s and %d row(s) for current-session routes; both must be "+
			"non-zero or this test only ever checks one value of the column",
			stale, asnPfxStale, live)
	}
}

// The stat row's "longest path" is the only panel that reads path DEPTH
// rather than membership, and the lab archive tops out at two hops -- so on
// production data a panel returning 2 and a panel returning
// length(arrayDistinct(as_path)) are indistinguishable. The fixture's
// five-hop path is what separates them.
func TestASNViewStatReportsTheLongestPathThroughTheASN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertASNViewFixture(t, ctx, ch)

	raw := scopeASNViewToFixtureRouter(t, panelSQL(t, "asn-view", "What we know", "A"))
	sql := substituteGrafana(raw, map[string]string{"asn": fmt.Sprint(asnDeepOrig), "rib": ".*"})
	var originated, transited, routers, neighbours, longest uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(
		&originated, &transited, &routers, &neighbours, &longest); err != nil {
		t.Fatalf("asn stat: %v\nSQL:\n%s", err, sql)
	}
	if longest != 5 {
		t.Errorf("longest path through AS %d = %d, want 5 -- %s carries "+
			"five hops and nothing in the lab archive carries more than two",
			asnDeepOrig, longest, asnPfxDeep)
	}
	if originated != 1 {
		t.Errorf("AS %d originates %d, want 1 (%s)", asnDeepOrig, originated, asnPfxDeep)
	}
	if transited != 0 {
		t.Errorf("AS %d transits %d, want 0 -- it is the far end of the deep path",
			asnDeepOrig, transited)
	}
	// One neighbor, not two: it sits at the end of the path, so only
	// asnDeep3 is adjacent to it.
	if neighbours != 1 {
		t.Errorf("AS %d has %d neighbours, want 1 (AS %d) -- an origin at the "+
			"end of a path has exactly one", asnDeepOrig, neighbours, asnDeep3)
	}
	if routers != 1 {
		t.Errorf("AS %d seen by %d routers, want 1", asnDeepOrig, routers)
	}
}

const (
	topRouterA = "10.0.199.9"
	topRouterB = "10.0.199.10"
	// topRouterDual is ONE router watched by TWO collectors. Everything
	// else here carries collector c1 alone, so every existing assertion
	// passes whether these panels resolve collectors or not -- exactly the
	// one-branch fixture problem that left several related defects
	// unfalsifiable when they were first found. route_vpn is dual-collector on
	// the lab archive (178 rows to 14), so this is the shipped shape.
	topRouterDual     = "10.0.199.20"
	topDualCollectorB = "c2"
	topPeer           = "10.255.199.13"
	// Three sessions, because the whole point of this dashboard's arithmetic
	// is that "how many times did we see it" and "how many BMP sessions
	// carried it" are different questions.
	//
	// That was once true of the lab archive too -- 0 of 31 (rd, prefix,
	// router) keys differed, verified 2026-08-23 -- which is the figure this
	// fixture was built against. Re-measured 2026-09-10 it is 855 of 1,354,
	// the lab archive having grown roughly fortyfold. The fixture is what makes
	// the distinction decidable independently of either number, which is why
	// it is built this way and why the drift changes nothing here. See
	// TestTopL3VPNSeparatesObservationsFromSessions for what the drift does
	// change.
	topSessionDual1 = 9804
	topSessionDualB = 9805
	topSessionA1    = 9801
	topSessionA2    = 9802
	topSessionB     = 9803

	topRDa = "65000:44"
	topRDb = "65000:55"
	// topRDLoc exists only in the loc_rib stream. Every other route here is
	// in_pre, so without it every assertion renders $rib as All and a panel
	// that stopped reading it would look identical.
	topRDLoc = "65000:66"
)

const (
	// topPfxRepeat is seen TWICE inside topSessionA1 and once more in
	// topSessionA2: three observations across two sessions. It is the only
	// route in the database for which those two counts disagree.
	topPfxRepeat = "10.202.1.0/24"
	// topPfxTwoRDs is exported by two VRFs from one router. A ranking that
	// drops rd from its key merges them into one row.
	topPfxTwoRDs = "10.202.2.0/24"
	// topPfxWithdrawn is advertised and then withdrawn inside one session.
	topPfxWithdrawn = "10.202.3.0/24"
	// topPfxTwoRouters is carried by both routers, so `routers` is not
	// always 1 the way it is for every other prefix here.
	topPfxTwoRouters = "10.202.4.0/24"
	// topPfxDual is carried by one router that TWO collectors watch, and it
	// is withdrawn once. The chosen collector holds two sessions and the
	// withdrawal; the lagging one holds a single session and its own copy
	// of that same withdrawal.
	//
	// So `sessions` reads 3 merged and 2 chosen, and `withdrawals` reads 2
	// merged and 1 chosen -- one router withdrawing one route once, counted
	// twice for being watched twice. `routers` must stay 1 throughout: it
	// is a uniqExact over router_ip and was always honest.
	topPfxDual = "10.202.6.0/24"
	// topPfxDualTie is the only route here whose two collectors saw the
	// SAME NUMBER of observations. It judges the second element of the
	// (observations, collector_id) ordering: without it the winner
	// comparison matches BOTH collectors' rows and every count on this
	// route doubles. topPfxDual cannot see it -- its collectors are 4 and
	// 2, so the comparison picks a single winner however it is spelled,
	// and the tie-break mutation survived it. Found by running it.
	topPfxDualTie = "10.202.7.0/24"
	// topPfxLu is labeled unicast: no Route Distinguisher, by design.
	topPfxLu = "10.202.5.1/32"
	// topPfxLocRib is the only route outside in_pre.
	topPfxLocRib = "10.202.6.0/24"
)

// insertTopL3VPNFixture writes the labeled routes the L3VPN tops dashboard
// is specified against, through the real RowsFor + Insert path, under
// reserved identifiers (testDB is shared and never truncated).
//
// The shape that matters is topPfxRepeat. Every observation in the lab
// archive sits on its own BMP session -- XRd resets every nine minutes and
// re-dumps its table, and nothing is ever re-advertised inside one session --
// so counting rows and counting sessions return the same number for all 31
// keys there. A "changes" column built on count() would be a reset counter
// wearing a churn label, and no query against production data could tell.
func insertTopL3VPNFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	vpn4 := &vantagev1.Family{Afi: 1, Safi: 128}
	lu4 := &vantagev1.Family{Afi: 1, Safi: 4}
	env := func(router string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		sys := "dash-top1"
		if router == topRouterB {
			sys = "dash-top2"
		}
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: topPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	attrs := func(rt string) *vantagev1.PathAttributes {
		a := &vantagev1.PathAttributes{
			Origin: 0, NextHop: "10.202.0.1",
			AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000, 64700}}},
		}
		if rt != "" {
			a.ExtendedCommunities = []*vantagev1.ExtCommunity{
				{Type: 0x00, SubType: 0x02, Value: rt},
			}
		}
		return a
	}
	// locRib stamps an envelope whose peer is an RFC 9069 Loc-RIB peer, which
	// is what rows.go turns into rib = 'loc_rib'.
	locRib := func(router string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(router, session, ts, r)
		e.Peer = &vantagev1.PeerId{
			Ip: topPeer, Asn: 65000,
			Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
		}
		return e
	}
	adv := func(fam *vantagev1.Family, prefix, rd string, label uint32) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{
			Family: fam, Attrs: attrs(rd),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: prefix, Rd: rd, Labels: []uint32{label}},
			},
		}
	}
	now := corpusCollectorClock

	envs := []*vantagev1.Envelope{
		// Twice inside ONE session, at different times so the two are
		// distinct rows rather than one replaced by the other, then once
		// more in a SECOND session. Three observations, two sessions.
		env(topRouterA, topSessionA1, now.Add(-40*time.Minute), adv(vpn4, topPfxRepeat, topRDa, 24101)),
		env(topRouterA, topSessionA1, now.Add(-35*time.Minute), adv(vpn4, topPfxRepeat, topRDa, 24101)),
		env(topRouterA, topSessionA2, now.Add(-10*time.Minute), adv(vpn4, topPfxRepeat, topRDa, 24101)),

		// One prefix, two VRFs, one router: two rows, not one.
		env(topRouterA, topSessionA1, now, adv(vpn4, topPfxTwoRDs, topRDa, 24102)),
		env(topRouterA, topSessionA1, now, adv(vpn4, topPfxTwoRDs, topRDb, 24103)),

		// Advertised then withdrawn inside one session.
		env(topRouterA, topSessionA1, now.Add(-20*time.Minute), adv(vpn4, topPfxWithdrawn, topRDa, 24104)),
		env(topRouterA, topSessionA1, now.Add(-15*time.Minute), &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs(topRDa),
			VpnWithdrawn: []*vantagev1.VpnPrefix{
				{Prefix: topPfxWithdrawn, Rd: topRDa, Labels: []uint32{24104}},
			},
		}),

		// Two routers carrying the same prefix under the same RD.
		env(topRouterA, topSessionA1, now, adv(vpn4, topPfxTwoRouters, topRDa, 24105)),
		env(topRouterB, topSessionB, now, adv(vpn4, topPfxTwoRouters, topRDa, 24106)),
		// A SECOND observation on router B, in the same session so no
		// session count moves. It breaks the two routers' symmetry, which
		// is what makes "choose the collector per (prefix, router)"
		// distinguishable from "choose it per prefix": with one observation
		// each, a per-prefix choice keeps both routers by accident and the
		// mutation survives. Summing across ROUTERS is addition -- they are
		// disjoint network entities -- and only summing across COLLECTORS
		// is double-counting.
		env(topRouterB, topSessionB, now.Add(-time.Minute), adv(vpn4, topPfxTwoRouters, topRDa, 24106)),

		// Labeled unicast: no RD at all.
		env(topRouterA, topSessionA1, now, adv(lu4, topPfxLu, "", 24199)),

		// The only route outside in_pre.
		locRib(topRouterA, topSessionA1, now, adv(vpn4, topPfxLocRib, topRDLoc, 24106)),
	}

	// The dual-homed router, appended LAST so nothing above is renumbered.
	// The chosen collector holds FOUR observations across two sessions; the
	// lagging one holds TWO across one. Unequal on purpose -- equal counts
	// could not tell "chose one" from "summed both".
	dualEnv := func(collector string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(topRouterDual, session, ts, r)
		e.CollectorId = collector
		e.Router = &vantagev1.RouterId{Ip: topRouterDual, SysName: "dash-top3"}
		return e
	}
	wdr := func() *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs(topRDa),
			VpnWithdrawn: []*vantagev1.VpnPrefix{
				{Prefix: topPfxDual, Rd: topRDa, Labels: []uint32{24107}},
			},
		}
	}
	dual := []*vantagev1.Envelope{
		dualEnv("c1", topSessionDual1, now.Add(-40*time.Minute), adv(vpn4, topPfxDual, topRDa, 24107)),
		dualEnv("c1", topSessionDual1, now.Add(-39*time.Minute), adv(vpn4, topPfxDual, topRDa, 24107)),
		dualEnv("c1", topSessionDual1, now.Add(-38*time.Minute), wdr()),
		dualEnv("c1", 9806, now.Add(-30*time.Minute), adv(vpn4, topPfxDual, topRDa, 24107)),
		// The lagging collector's own copies of the SAME two events.
		dualEnv(topDualCollectorB, topSessionDualB, now.Add(-40*time.Minute), adv(vpn4, topPfxDual, topRDa, 24107)),
		dualEnv(topDualCollectorB, topSessionDualB, now.Add(-38*time.Minute), wdr()),
		// topPfxDualTie: TWO observations each, one session each. "c2"
		// sorts after "c1", so the tie-break names a specific winner.
		// Under topRDb, NOT topRDa. The tie is won by the lagging
		// collector (c2 sorts after c1), so this route legitimately
		// contributes c2's session id -- and under topRDa that would make
		// the RD's distinct-session count 6 whether the winner filter runs
		// or not, quietly destroying what
		// TestTopL3VPNRDSessionsCountOneCollectorsView can see. Keeping the
		// two RDs apart keeps both assertions falsifiable.
		dualEnv("c1", topSessionDual1, now.Add(-20*time.Minute), adv(vpn4, topPfxDualTie, topRDb, 24108)),
		dualEnv("c1", topSessionDual1, now.Add(-19*time.Minute), adv(vpn4, topPfxDualTie, topRDb, 24108)),
		dualEnv(topDualCollectorB, topSessionDualB, now.Add(-20*time.Minute), adv(vpn4, topPfxDualTie, topRDb, 24108)),
		dualEnv(topDualCollectorB, topSessionDualB, now.Add(-19*time.Minute), adv(vpn4, topPfxDualTie, topRDb, 24108)),
	}
	envs = append(envs, dual...)
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert top-l3vpn fixture envelope %d: %v", i, err)
		}
	}
}

// scopeTopL3VPNToFixtureRouters narrows a tops query to the two routers this
// fixture owns, by extending the panel's own time filter. testDB is shared,
// the dashboard has no $router control, and the counts below are claims about
// this fixture's routes rather than about whatever else the corpus holds.
func scopeTopL3VPNToFixtureRouters(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("a top-l3vpn query carries %d occurrences of %q, want 1 -- "+
			"these tests scope to the fixture's routers by extending that "+
			"filter, and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip IN (toIPv6('"+topRouterA+"'), toIPv6('"+topRouterB+
			"'), toIPv6('"+topRouterDual+"'))", 1)
}

// topPrefixRow is one row of the committed "Top prefixes" table.
type topPrefixRow struct {
	family       string
	rd           string
	prefix       string
	routers      uint64
	sessions     uint64
	observations uint64
	withdrawals  uint64
	lastSeen     time.Time
}

// topPrefixes runs the committed ranking table with $family and $rib rendered
// as Grafana would render them, keyed by (rd, prefix) so a merged row is
// visible as a missing key rather than as a count that happens to match.
func topPrefixes(t *testing.T, ctx context.Context, ch *ClickHouse, family, rib string) map[string]topPrefixRow {
	t.Helper()
	raw := scopeTopL3VPNToFixtureRouters(t, panelSQL(t, "top-l3vpn-prefixes", "Top prefixes", "A"))
	sql := substituteGrafana(raw, map[string]string{"family": family, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("top prefixes (family=%q): %v\nSQL:\n%s", family, err, sql)
	}
	defer rows.Close()
	out := map[string]topPrefixRow{}
	for rows.Next() {
		var r topPrefixRow
		if err := rows.Scan(&r.family, &r.rd, &r.prefix, &r.routers, &r.sessions,
			&r.observations, &r.withdrawals, &r.lastSeen); err != nil {
			t.Fatalf("scan top prefix row: %v", err)
		}
		out[r.rd+"|"+r.prefix] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("top prefix rows: %v", err)
	}
	return out
}

// topRDRow is one row of the committed "Top RDs" table.
type topRDRow struct {
	family   string
	rd       string
	prefixes uint64
	routers  uint64
	sessions uint64
}

func topRDs(t *testing.T, ctx context.Context, ch *ClickHouse, family, rib string) map[string]topRDRow {
	t.Helper()
	raw := scopeTopL3VPNToFixtureRouters(t, panelSQL(t, "top-l3vpn-prefixes", "Top RDs", "A"))
	sql := substituteGrafana(raw, map[string]string{"family": family, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("top RDs (family=%q): %v\nSQL:\n%s", family, err, sql)
	}
	defer rows.Close()
	out := map[string]topRDRow{}
	for rows.Next() {
		var r topRDRow
		if err := rows.Scan(&r.family, &r.rd, &r.prefixes, &r.routers, &r.sessions); err != nil {
			t.Fatalf("scan top RD row: %v", err)
		}
		out[r.rd] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("top RD rows: %v", err)
	}
	return out
}

// The fixture's arrangement is what makes the counts below decidable, so it
// is pinned rather than trusted -- particularly the one shape the lab
// archive has none of.
func TestTopL3VPNFixtureIsArrangedAdversarially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	// Exactly one key where observations and sessions disagree. Without it,
	// count() and uniqExact(session_id) return the same number everywhere
	// and a panel cannot be caught using the wrong one.
	var differing uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, `
		SELECT countIf(obs != sess) FROM (
			SELECT rd, prefix, router_ip, count() AS obs,
			       uniqExact(session_id) AS sess
			FROM vantage.route_vpn FINAL
			WHERE router_ip IN (toIPv6('`+topRouterA+`'), toIPv6('`+topRouterB+`'))
			GROUP BY rd, prefix, router_ip
		)`)).Scan(&differing); err != nil {
		t.Fatalf("differing keys: %v", err)
	}
	if differing != 3 {
		t.Errorf("%d keys have observations != sessions, want 3 (%s, seen "+
			"twice in one session and once in another; %s, advertised and "+
			"withdrawn inside one; and %s on router B, advertised twice "+
			"inside one) -- the lab archive has none of these, so this "+
			"fixture is the only place the two counts can be told apart",
			differing, topPfxRepeat, topPfxWithdrawn, topPfxTwoRouters)
	}

	// One prefix under two RDs, or dropping rd from a GROUP BY changes nothing.
	var rdsOfTwoRDPrefix uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT uniqExact(rd) FROM vantage.route_vpn FINAL WHERE prefix = '"+
			topPfxTwoRDs+"' AND router_ip = toIPv6('"+topRouterA+"')")).Scan(&rdsOfTwoRDPrefix); err != nil {
		t.Fatalf("two-RD prefix: %v", err)
	}
	if rdsOfTwoRDPrefix != 2 {
		t.Errorf("%s is exported by %d RDs, want 2", topPfxTwoRDs, rdsOfTwoRDPrefix)
	}

	// An lu4 route whose RD is empty BY DESIGN (bgp/vpn.go), so the fallback
	// rendering is exercised rather than assumed.
	var luRD string
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT any(rd) FROM vantage.route_vpn FINAL WHERE prefix = '"+
			topPfxLu+"'")).Scan(&luRD); err != nil {
		t.Fatalf("lu4 rd: %v", err)
	}
	if luRD != "" {
		t.Errorf("%s has rd %q, want empty -- labelled unicast carries no "+
			"Route Distinguisher and the fallback string depends on it",
			topPfxLu, luRD)
	}
}

// This is the whole reason the dashboard does not copy route-churn's
// "Most-changed prefixes" panel: `observations` and `sessions` answer two
// different questions, and this dashboard presents them as two labeled
// columns rather than folding them into one number it would then have to
// call churn. The two must be separately correct, and only this fixture can
// say so -- topPfxRepeat and topPfxWithdrawn are the only keys that make
// them differ.
//
// route-churn's panel classifies each row against its own route inside its
// own session and sums the result up to the prefix, which is a different
// shape from what this dashboard presents, so its panel cannot simply be
// copied here. The lab archive has grown too: re-measured read-only on
// 2026-09-10, route_vpn holds 6,907 rows over 1,695 route keys and 246
// sessions, and 886 of those keys are observed more often than the
// sessions carrying them -- the per-row rule finds 2,881 real changes
// there.
//
// The "0 of 31 keys differ" this reasoning rests on was verified on
// 2026-08-23 against a lab archive roughly a fortieth of today's, and it no
// longer holds. The conclusion may well survive its premise; whether this
// dashboard should now carry a change column is a question for a pass of
// its own, with its own fixture and its own mutation, and not something to
// infer from here. This test asserts what it always asserted, and it does
// not depend on the lab archive either way.
func TestTopL3VPNSeparatesObservationsFromSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	row, present := topPrefixes(t, ctx, ch, ".*", ".*")[topRDa+"|"+topPfxRepeat]
	if !present {
		t.Fatalf("%s under RD %s is absent from the ranking", topPfxRepeat, topRDa)
	}
	if row.observations != 3 {
		t.Errorf("%s observations = %d, want 3 -- twice in one session and "+
			"once in another; 2 means the panel is counting sessions",
			topPfxRepeat, row.observations)
	}
	if row.sessions != 2 {
		t.Errorf("%s sessions = %d, want 2 -- it was seen in %d and %d; 3 "+
			"means the panel is counting rows and calling them sessions",
			topPfxRepeat, row.sessions, topSessionA1, topSessionA2)
	}
	if row.routers != 1 {
		t.Errorf("%s routers = %d, want 1", topPfxRepeat, row.routers)
	}
	// And a prefix carried by two routers must say so, or `routers` is a
	// column that reads 1 whatever happens.
	if got := topPrefixes(t, ctx, ch, ".*", ".*")[topRDa+"|"+topPfxTwoRouters].routers; got != 2 {
		t.Errorf("%s routers = %d, want 2", topPfxTwoRouters, got)
	}
}

// One prefix exported by two VRFs is two routes. A ranking keyed on prefix
// alone merges them and silently halves the VRF count an operator sees.
func TestTopL3VPNKeepsTwoRDsOfOnePrefixApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	rows := topPrefixes(t, ctx, ch, ".*", ".*")
	a, okA := rows[topRDa+"|"+topPfxTwoRDs]
	b, okB := rows[topRDb+"|"+topPfxTwoRDs]
	if !okA || !okB {
		t.Fatalf("%s appears under RD %s=%v and RD %s=%v; it must be one row "+
			"per RD", topPfxTwoRDs, topRDa, okA, topRDb, okB)
	}
	if a.observations != 1 || b.observations != 1 {
		t.Errorf("%s observations = %d/%d under the two RDs, want 1 each -- "+
			"2 means the rows were merged and then split by something else",
			topPfxTwoRDs, a.observations, b.observations)
	}
}

// A withdrawal is a fact about a route, not a reason to drop it from a
// ranking: an operator scanning for instability wants the withdrawn ones.
func TestTopL3VPNCountsWithdrawalsWithoutHidingTheRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	row, present := topPrefixes(t, ctx, ch, ".*", ".*")[topRDa+"|"+topPfxWithdrawn]
	if !present {
		t.Fatalf("%s is absent; a withdrawn route is still a ranked route",
			topPfxWithdrawn)
	}
	if row.withdrawals != 1 {
		t.Errorf("%s withdrawals = %d, want 1", topPfxWithdrawn, row.withdrawals)
	}
	if row.observations != 2 {
		t.Errorf("%s observations = %d, want 2 (the advertisement and the "+
			"withdrawal) -- 1 means one of them was filtered away",
			topPfxWithdrawn, row.observations)
	}
}

// route_vpn is really "labeled routes" discriminated by family, and lu4
// carries no Route Distinguisher by design (bgp/vpn.go). Presenting that as a
// blank cell next to real RDs is what route-churn already fixed once; this
// dashboard must not reintroduce it.
func TestTopL3VPNRendersTheAbsentRouteDistinguisher(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	const want = "(none: this family carries no RD)"
	var found bool
	for _, r := range topPrefixes(t, ctx, ch, ".*", ".*") {
		if r.prefix != topPfxLu {
			continue
		}
		found = true
		if r.rd != want {
			t.Errorf("%s rd rendered as %q, want %q -- an empty cell reads as "+
				"a missing value rather than as a family that has none",
				topPfxLu, r.rd, want)
		}
	}
	if !found {
		t.Errorf("%s is absent from the ranking entirely", topPfxLu)
	}
}

// $family is the control that separates L3VPN from labeled unicast, and the
// two behave differently enough that a panel ignoring it shows an operator
// RD-less rows in a VRF ranking.
func TestTopL3VPNFamilyNarrowsToTheChosenFamily(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	lu := topPrefixes(t, ctx, ch, "lu4", ".*")
	for _, r := range lu {
		if r.family != "lu4" {
			t.Errorf("$family = lu4 returned a %q row (%s); the panel is not "+
				"reading $family", r.family, r.prefix)
		}
	}
	if len(lu) == 0 {
		t.Error("$family = lu4 returned nothing; the fixture writes one")
	}
	vpn4 := topPrefixes(t, ctx, ch, "vpn4", ".*")
	if _, leaked := vpn4[`|`+topPfxLu]; leaked {
		t.Errorf("%s is an lu4 route and appeared under $family = vpn4", topPfxLu)
	}
	// 7 since topPfxDual joined: the six this fixture always had plus the
	// dual-homed router's one route. It is ONE row, not two, even though
	// two collectors hold it -- the table renders one row per (family, RD,
	// prefix), and that holds however many collectors see the route.
	if len(vpn4) != 8 {
		t.Errorf("$family = vpn4 returned %d rows, want 8 (%s, %s under two "+
			"RDs, %s, %s, %s, %s, %s)", len(vpn4), topPfxRepeat, topPfxTwoRDs,
			topPfxWithdrawn, topPfxTwoRouters, topPfxLocRib, topPfxDual,
			topPfxDualTie)
	}
}

// The RD ranking answers "which VRFs carry the most", which is a different
// question from the prefix ranking above and has to be keyed differently:
// per RD, DISTINCT prefixes rather than rows, or a VRF that re-dumps often
// outranks one that actually carries more.
func TestTopL3VPNRDsRankByDistinctPrefixes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	rds := topRDs(t, ctx, ch, "vpn4", ".*")
	a, ok := rds[topRDa]
	if !ok {
		t.Fatalf("RD %s is absent from the RD ranking", topRDa)
	}
	// 5 since topPfxDual joined. Counting ROWS would now return 10 rather
	// than the old 6, because the dual-homed route contributes six of them
	// -- so this assertion is harder than it was, not merely renumbered.
	if a.prefixes != 5 {
		t.Errorf("RD %s carries %d prefixes, want 5 (%s, %s, %s, %s, %s) -- "+
			"10 is what counting rows returns, because %s was seen three "+
			"times and %s six",
			topRDa, a.prefixes, topPfxRepeat, topPfxTwoRDs, topPfxWithdrawn,
			topPfxTwoRouters, topPfxDual, topPfxRepeat, topPfxDual)
	}
	// 3, and topRouterDual counts ONCE despite two collectors watching it:
	// uniqExact(router_ip) carries no collector_id and was always honest.
	if a.routers != 3 {
		t.Errorf("RD %s spans %d routers, want 3 -- %s is one router however "+
			"many collectors watch it", topRDa, a.routers, topRouterDual)
	}
	// 2 since topPfxDualTie was placed here -- see the fixture for why it
	// is under this RD rather than topRDa.
	if b := rds[topRDb]; b.prefixes != 2 {
		t.Errorf("RD %s carries %d prefixes, want 2 (%s, %s)", topRDb,
			b.prefixes, topPfxTwoRDs, topPfxDualTie)
	}
}

// Every route this fixture writes is in_pre except one, which is the only
// reason $rib can be caught not being read: All is the branch that looks the
// same whether or not the clause exists, and every other test here renders it.
func TestTopL3VPNRibNarrowsToTheChosenStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	if _, present := topPrefixes(t, ctx, ch, ".*", ".*")[topRDLoc+"|"+topPfxLocRib]; !present {
		t.Fatalf("%s is absent with $rib on All; it is a loc_rib route and "+
			"All must include it", topPfxLocRib)
	}
	inPre := topPrefixes(t, ctx, ch, ".*", "in_pre")
	if _, present := inPre[topRDLoc+"|"+topPfxLocRib]; present {
		t.Errorf("%s is still ranked with $rib = in_pre; its only route is in "+
			"loc_rib, so the panel is not reading $rib", topPfxLocRib)
	}
	if _, present := inPre[topRDa+"|"+topPfxRepeat]; !present {
		t.Errorf("%s vanished with $rib = in_pre, where it lives -- the "+
			"filter is excluding too much", topPfxRepeat)
	}
	// The RD ranking reads the same control and must agree.
	if _, present := topRDs(t, ctx, ch, ".*", "in_pre")[topRDLoc]; present {
		t.Errorf("RD %s is still ranked with $rib = in_pre", topRDLoc)
	}
}

// One prefix exported by two VRFs is two routes, and the headline count has
// to say so: collapsing (RD, prefix) to prefix understates the table by
// exactly the multi-VRF exports an L3VPN operator most wants to see.
func TestTopL3VPNStatCountsRoutesNotPrefixes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	raw := scopeTopL3VPNToFixtureRouters(t, panelSQL(t, "top-l3vpn-prefixes", "What we know", "A"))
	sql := substituteGrafana(raw, map[string]string{"family": "vpn4", "rib": ".*"})
	var prefixes, rds, routers, withdrawals uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(
		&prefixes, &rds, &routers, &withdrawals); err != nil {
		t.Fatalf("top-l3vpn stat: %v\nSQL:\n%s", err, sql)
	}
	// 8 since topPfxDual and topPfxDualTie joined.
	if prefixes != 8 {
		t.Errorf("prefixes = %d, want 8 -- distinct (RD, prefix) pairs, and "+
			"%s is exported by two VRFs; 7 is what counting distinct "+
			"prefixes returns", prefixes, topPfxTwoRDs)
	}
	if rds != 3 {
		t.Errorf("rds = %d, want 3 (%s, %s, %s)", rds, topRDa, topRDb, topRDLoc)
	}
	if routers != 3 {
		t.Errorf("routers = %d, want 3 -- %s is one router however many "+
			"collectors watch it", routers, topRouterDual)
	}
	// TWO withdrawals, not three: topPfxWithdrawn's and topPfxDual's. The
	// dual-homed router withdrew its route ONCE and both collectors wrote
	// it down, so 3 is the defect this panel shipped with -- a withdrawal
	// counted per observer rather than per router act.
	if withdrawals != 2 {
		t.Errorf("withdrawals = %d, want 2 (%s and %s). 3 counts %s's single "+
			"withdrawal once per collector that heard it",
			withdrawals, topPfxWithdrawn, topPfxDual, topPfxDual)
	}
}

const (
	evpnRouter = "10.0.199.11"
	// evpnRouterB sees one of the same MACs. A leaf and the route reflector
	// above it disagree about how much a MAC moved, and the table keeps them
	// apart on purpose -- so `router` has to be in the grouping key.
	evpnRouterB = "10.0.199.12"
	evpnPeer    = "10.255.199.15"
	evpnSession = 9901
	// Two route types under ONE route distinguisher. EVPN identity is not
	// (rd, prefix) the way L3VPN's is -- a MAC advertisement and an IP-prefix
	// route can share an RD and are different routes, so route_type has to be
	// part of the key.
	evpnRD = "65000:77"
	// evpnRDOther exists only in the loc_rib stream, so $rib is a control
	// that changes an answer. All 202 EVPN rows in the lab archive are
	// in_pre.
	evpnRDLoc = "65000:88"
	// evpnRDShared carries a type 3 AND a type 1. Those are the two EVPN
	// types that populate none of mac, ip or prefix, so with route_type out
	// of the window's partition key they land in ONE partition and one
	// route's withdrawal gets paired with the other's advertisement.
	evpnRDShared = "65000:99"

	// evpnMACFlapper is withdrawn and re-advertised TWICE. Its flap count and
	// its summed time-away are both wrong in distinguishable ways if the
	// window pairing is off by one.
	evpnMACFlapper = "52:54:00:ff:00:01"
	// evpnMACStable is advertised and never withdrawn, so `flaps` has a
	// reachable zero and a panel cannot pass by calling everything a flap.
	evpnMACStable = "52:54:00:ff:00:02"
	// evpnMACGone ends on a withdrawal and never returns. It is the partition
	// edge: the last row of its window has no following observation, and a
	// pairing without the "next is later than this" guard reads the zero
	// timestamp there and reports a gap of minus fifty-six years.
	evpnMACGone = "52:54:00:ff:00:03"
	// evpnMACTwiceWithdrawn is withdrawn TWICE in a row before it returns --
	// a duplicate withdrawal, or a re-dump that withdraws an already-withdrawn
	// route. It is the only thing here that can falsify "Flap cycles"'
	// `nxt_w = 0`: without that clause the FIRST withdrawal pairs with the
	// SECOND one, which is later and so passes the "next is later than this"
	// guard, and the panel invents a cycle in which the route never came
	// back. Found by a mutation that survived, not by review.
	evpnMACTwiceWithdrawn = "52:54:00:ff:00:09"
	// evpnMACWithIP carries an IP alongside the MAC; the three above do not.
	// Type 2 renders "mac / ip" when both are present and the MAC alone when
	// not, and only one of those branches is exercised without this.
	evpnMACWithIP = "52:54:00:ff:00:04"
	evpnIPOfMAC   = "10.203.0.44"
	// evpnPrefix is a type-5 route sharing evpnRD with the type-2s above.
	evpnPrefix = "10.203.9.0/24"
	// evpnMACShared is advertised by BOTH routers, with different histories.
	evpnMACShared = "52:54:00:ff:00:05"

	// Everything below was added on 2026-09-10 for the TIME SERIES panel,
	// which until then this fixture could not judge at all: every row above
	// sits in ONE BMP session, so "the first observation of this route in
	// this session" and "the first observation of this route" were the same
	// predicate and a re-dump could not be told from a change by
	// construction. A fixture that cannot express two answers cannot defend
	// the one that ships.
	//
	// evpnSession2 and evpnSession3 are the router coming back. Ids rise
	// with time, as a collector's do -- session_id is now().UnixNano()
	// behind a monotonic guard.
	evpnSession2 = 9902
	evpnSession3 = 9903

	// evpnRDBucket carries every row the time-series tests name, so those
	// rows cannot disturb the flap arithmetic the tables above are pinned
	// to. It ends up holding four route types; only evpnRD is asserted to
	// hold exactly two.
	evpnRDBucket = "65000:66"

	// evpnBucketMACFlap and evpnBucketMACRedump are the mixed bucket, and
	// they are what makes this fixture strictly harder than an earlier rig,
	// where the two shapes were in SEPARATE buckets, so a query could
	// have separated them by noticing which bucket a row fell in. Here they
	// are interleaved inside ONE minute:
	//
	//	evpnBucketMACFlap   dumped in minute 99, then genuinely
	//	                    re-advertised TWICE inside minute 100, all in
	//	                    evpnSession -- two real changes
	//	evpnBucketMACRedump seen once in evpnSession2 and once in
	//	                    evpnSession3, both inside minute 100 -- two
	//	                    table dumps and no change at all
	//
	// Minute 100 therefore holds four observations of which exactly two are
	// changes, and the three candidate formulas all disagree about it:
	//
	//	count()                         = 4  -- what the panel shipped
	//	count() - uniqExact(session_id) = 1  -- the bucket-corrected candidate
	//	per-row classification          = 2  -- the truth, and what ships
	evpnBucketMACFlap   = "52:54:00:ff:01:01"
	evpnBucketMACRedump = "52:54:00:ff:01:02"

	// evpnPfxLoneWithdraw is withdrawn in evpnSession2 without ever having
	// been advertised there: the first thing that session says about it is
	// that it is gone.
	//
	// It is the asymmetry argument in one row. A table dump advertises; it
	// never withdraws, so "first observation of this route in this session"
	// identifies a collection artifact for an advertisement and identifies
	// nothing of the sort for a withdrawal, which is why the classification
	// carries an explicit is_withdraw = 0 guard. Dropping that guard would
	// file this row as a dump and delete a real withdrawal from the panel.
	//
	// It is a type 5 rather than a type 2 deliberately: this fixture pins
	// "exactly one type-2 key on the first router ends on a withdrawal" as
	// the partition edge the flap pairing has to survive, and a second one
	// would move that number rather than test this one.
	//
	// The lab archive has no row of this shape in route_evpn today -- 0 of
	// its 246 withdrawals are first-in-session, against 12 of
	// route_unicast's 1,619 (both measured 2026-09-10). The guard is kept
	// because the asymmetry is structural, and this row is what keeps that
	// decision tested rather than merely written down.
	evpnPfxLoneWithdraw = "10.203.8.0/24"

	// evpnPeerB is a SECOND BGP peer of the first router, and evpnCollectorB
	// a second collector watching it. Neither exists anywhere else in this
	// fixture, and without them peer_ip and collector_id are components of
	// the route key that no row here can be wrong about.
	evpnPeerB      = "10.255.199.16"
	evpnCollectorB = "c2"
	// evpnRDBucketAlt is the second route distinguisher, so rd can be
	// isolated the same way.
	evpnRDBucketAlt = "65000:67"

	// The twelve pairs below isolate ONE component each of the EVPN route
	// key, so a classification that dropped a component fails on the bucket
	// named after that component rather than on a single conflated total.
	//
	// query/evpnroutes.go's GROUP BY is the whole NLRI tuple -- collector,
	// router, peer, rib, route_type, rd, prefix, mac, ip, ethernet_tag,
	// esi, path_id -- and its doc comment states that every component of it
	// is load bearing rather than defensive. That is more than a style
	// preference here: an EVPN route is not identified by a prefix, because
	// a type 2 has no prefix at all and a type 3 has neither prefix nor MAC
	// nor IP, so there is no shorter key to fall back on. The lab archive
	// proves the shortening bites -- its type-2 rows under one rd and mac
	// differ only in ip, so a key without ip merges a "MAC only"
	// advertisement with the "MAC plus IP" one.
	//
	// Each pair is TWO routes, each observed exactly ONCE inside
	// evpnSession: two dumps, zero changes. Merge them by dropping the
	// component and the second row stops being its own route's first
	// observation, so the bucket reports one dump and one change instead.
	//
	//	minute 110  ip            evpnKeyIPMac with and without evpnKeyIP
	//	minute 111  ethernet_tag  evpnKeyTagA and evpnKeyTagB
	//	minute 112  esi           evpnKeyESIA and evpnKeyESIB
	//	minute 113  path_id       evpnKeyPathMAC at path 0 and path 7
	//	minute 114  route_type    evpnKeyTypeTag as a type 3 and a type 1
	//	minute 115  peer_ip       evpnKeyPeerMAC from evpnPeer and evpnPeerB
	//	minute 116  rib           evpnKeyRibMAC in in_pre and in_post
	//	minute 117  rd            evpnKeyRDMAC under both bucket RDs
	//	minute 118  prefix        evpnKeyPfxA and evpnKeyPfxB
	//	minute 119  mac           evpnKeyMACA and evpnKeyMACB
	//	minute 120  router_ip     evpnKeyRouterMAC from both routers
	//	minute 121  collector_id  evpnKeyCollectorMAC via both collectors
	evpnKeyIPMac   = "52:54:00:ff:02:01"
	evpnKeyIP      = "10.203.0.55"
	evpnKeyPathMAC = "52:54:00:ff:02:02"
	evpnKeyTagA    = 4001
	evpnKeyTagB    = 4002
	// evpnKeyESITag is one Ethernet tag carrying two Ethernet segments, so
	// esi is the only thing between them.
	evpnKeyESITag = 4003
	evpnKeyESIA   = "00112233445566778801"
	evpnKeyESIB   = "00112233445566778802"
	// evpnKeyTypeTag carries a type 3 and a type 1 with the same RD, the
	// same Ethernet tag and no ESI on either. Those two types populate none
	// of mac, ip or prefix, so route_type is the only column that tells
	// them apart.
	evpnKeyTypeTag      = 4004
	evpnKeyPeerMAC      = "52:54:00:ff:02:03"
	evpnKeyRibMAC       = "52:54:00:ff:02:04"
	evpnKeyRDMAC        = "52:54:00:ff:02:05"
	evpnKeyMACA         = "52:54:00:ff:02:06"
	evpnKeyMACB         = "52:54:00:ff:02:07"
	evpnKeyRouterMAC    = "52:54:00:ff:02:08"
	evpnKeyCollectorMAC = "52:54:00:ff:02:09"
	// evpnKeyLagFlapMAC is advertised once and withdrawn once, and NEVER
	// comes back -- so it has zero flaps, whoever is watching. A second,
	// LAGGING collector's copy of that one advertisement lands after the
	// first collector already recorded the withdrawal.
	//
	// Ordered by ts_collector with no collector in the partition, the three
	// rows read advertise, withdraw, advertise -- and the flap test ("this
	// row is a withdrawal, the next is not, and it is later") fires on a
	// return that never happened. The "return" is one collector's stale copy
	// of the same advertisement the other collector logged first.
	evpnKeyLagFlapMAC = "52:54:00:ff:02:0a"
	evpnKeyPfxA       = "10.203.10.0/24"
	evpnKeyPfxB       = "10.203.11.0/24"

	// evpnZeroClockMAC is one route observed twice inside evpnSession whose
	// SECOND observation carries a router timestamp of zero -- the QK_TS_ZERO
	// quirk, a router reporting 1970 because its clock has not come back
	// after a reload.
	//
	// It exists because ordering the session on ts_router instead of on
	// (seq, stream_seq) is a mutation nothing else here can see. Both
	// observations are advertisements, so the fixture-wide totals are
	// identical either way; only WHICH minute holds the dump moves. Measured
	// read-only on 2026-09-10, the lab archive cannot see it either: zero
	// rows in route_evpn, route_vpn or route_unicast carry a zero ts_router,
	// and ordering by (ts_router, stream_seq) classifies every one of
	// route_evpn's 904 rows exactly as (seq, stream_seq) does. The quirk is
	// real -- the decoder has a flag for it -- and the lab archive simply has
	// not caught a router in that state yet, which is the reason to write
	// the row rather than the reason not to.
	evpnZeroClockMAC = "52:54:00:ff:03:01"
)

// evpnBucketBase is the start of the minute two hours before
// corpusCollectorClock, and it is where every time-series row of this fixture
// is measured from.
//
// The panel buckets with toStartOfInterval, whose boundaries are absolute
// rather than relative to the fixture, so rows stamped at an arbitrary offset
// land in a bucket nobody can name: two observations twenty seconds apart
// share a bucket or straddle one depending on the second corpusCollectorClock
// happened to be read at, and a test asserting "these two are in the same
// bucket" would pass roughly two thirds of the time. Truncating to the minute
// first makes the bucket a fact about the fixture. This is churnBucketBase's
// reasoning and its offset, kept identical on purpose -- the two fixtures
// write to different tables and never share a row.
func evpnBucketBase() time.Time {
	return corpusCollectorClock.Add(-2 * time.Hour).Truncate(time.Minute)
}

// evpnBucketAt stamps a row inside minute bucket `minute` of evpnBucketBase,
// `sec` seconds in. Callers keep sec below 60; the point of the helper is
// that the caller names the bucket rather than computing it.
func evpnBucketAt(minute, sec int) time.Time {
	return evpnBucketBase().Add(
		time.Duration(minute)*time.Minute + time.Duration(sec)*time.Second)
}

// insertEvpnChurnFixture writes the EVPN routes the churn dashboard is
// specified against, through the real RowsFor + Insert path.
//
// route_evpn is the one table in the lab archive with genuine churn -- 26 of 26
// keys are re-observed inside a single BMP session, against 0 of 31 for
// route_vpn (both verified 2026-08-23; the route_vpn half was re-measured on
// 2026-09-10 as 886 of 1,695 and no longer holds, see
// TestTopL3VPNSeparatesObservationsFromSessions) -- so unlike the other
// dashboards this one is not compensating for absent data. What the
// fixture adds is the shapes that make a WRONG
// implementation distinguishable from a right one: a route that never flaps,
// a route that flaps twice, and above all a route whose last word is a
// withdrawal, which is where the window pairing breaks.
//
// It gained a second and third BMP session on 2026-09-10, when the time
// series panel was corrected. Until then every row here sat in ONE session,
// which made a session re-dump and a genuine re-advertisement the same thing
// as far as any query could see -- so the fixture could not have caught the
// defect that panel shipped with, whatever the panel had said. The rows that
// prove the correction live at minutes 99-114 of evpnBucketBase, clear of
// everything above.
func insertEvpnChurnFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	evpn := &vantagev1.Family{Afi: 25, Safi: 70}
	envOn := func(router string, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		sys := "dash-evpn1"
		if router == evpnRouterB {
			sys = "dash-evpn2"
		}
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: evpnPeer, Asn: 65000},
			SessionId:   evpnSession,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	env := func(ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return envOn(evpnRouter, ts, r)
	}
	locRib := func(ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(ts, r)
		e.Peer = &vantagev1.PeerId{
			Ip: evpnPeer, Asn: 65000,
			Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
		}
		return e
	}
	attrs := &vantagev1.PathAttributes{
		Origin: 0, NextHop: "10.203.0.1",
		AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000}}},
	}
	mac2 := func(mac, ip, rd string) *vantagev1.EvpnRoute {
		return &vantagev1.EvpnRoute{RouteType: 2, Rd: rd, Mac: mac, Ip: ip, Labels: []uint32{25001}}
	}
	adv := func(r *vantagev1.EvpnRoute) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{Family: evpn, Attrs: attrs,
			EvpnAnnounced: []*vantagev1.EvpnRoute{r}}
	}
	wdr := func(r *vantagev1.EvpnRoute) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{Family: evpn, Attrs: attrs,
			EvpnWithdrawn: []*vantagev1.EvpnRoute{r}}
	}
	// sess is env with the BMP session named rather than assumed. Every row
	// above this line is in evpnSession; the time-series rows below need the
	// router to have reconnected.
	sess := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(ts, r)
		e.SessionId = session
		return e
	}
	// peerB is the same router's other BGP peer. Only the address changes:
	// rows.go keys peer_ip off PeerId.ip whatever the peer type, so this is
	// a second route for anything both peers carry, and nothing else.
	peerB := func(ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(ts, r)
		e.Peer = &vantagev1.PeerId{Ip: evpnPeerB, Asn: 65001}
		return e
	}
	// inPost is the SAME peer's post-policy adj-RIB-in (RFC 8671's L flag),
	// which rows.go files as rib = in_post: same peer, same session, a
	// different RIB stream, and therefore a different route.
	inPost := func(ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(ts, r)
		e.Peer = &vantagev1.PeerId{Ip: evpnPeer, Asn: 65000, PostPolicy: true}
		return e
	}
	// skewed is env with the ROUTER's clock stated separately from the
	// collector's. Everywhere else in this fixture the two agree.
	skewed := func(ts, routerTS time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(ts, r)
		e.TsRouter = timestamppb.New(routerTS)
		return e
	}
	// collectorB is a second collector watching the same router and peer.
	// The lab archive has one collector, so this pair is the only thing here
	// that can tell whether collector_id is in the route key at all -- and
	// the k8s dual-homing argument is what makes that stop being academic.
	collectorB := func(ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(ts, r)
		e.CollectorId = evpnCollectorB
		return e
	}
	imet := func(rd string, tag uint32, esi string) *vantagev1.EvpnRoute {
		return &vantagev1.EvpnRoute{RouteType: 3, Rd: rd, EthernetTag: tag,
			Esi: esi, Labels: []uint32{25003}}
	}
	ead := func(rd string, tag uint32, esi string) *vantagev1.EvpnRoute {
		return &vantagev1.EvpnRoute{RouteType: 1, Rd: rd, EthernetTag: tag,
			Esi: esi, Labels: []uint32{25001}}
	}
	pfx5 := func(rd, prefix string) *vantagev1.EvpnRoute {
		return &vantagev1.EvpnRoute{RouteType: 5, Rd: rd, Prefix: prefix,
			GatewayIp: "10.203.0.1", Labels: []uint32{25005}}
	}
	macPath := func(mac, rd string, pathID uint32) *vantagev1.EvpnRoute {
		r := mac2(mac, "", rd)
		r.PathId = pathID
		return r
	}
	now := corpusCollectorClock
	at := func(d time.Duration) time.Time { return now.Add(-time.Hour + d) }

	envs := []*vantagev1.Envelope{
		// Flaps twice. Away for 10s, then for 30s: two cycles, 40s total, so
		// a sum and a max and a count are all different numbers.
		env(at(0), adv(mac2(evpnMACFlapper, "", evpnRD))),
		env(at(20*time.Second), wdr(mac2(evpnMACFlapper, "", evpnRD))),
		env(at(30*time.Second), adv(mac2(evpnMACFlapper, "", evpnRD))),
		env(at(60*time.Second), wdr(mac2(evpnMACFlapper, "", evpnRD))),
		env(at(90*time.Second), adv(mac2(evpnMACFlapper, "", evpnRD))),

		// Never withdrawn: flaps must be 0, not "unknown" and not 1.
		env(at(5*time.Second), adv(mac2(evpnMACStable, "", evpnRD))),
		env(at(65*time.Second), adv(mac2(evpnMACStable, "", evpnRD))),

		// Ends withdrawn and never returns. This is the partition edge: the
		// withdrawal is the LAST row for this key, so a pairing that reaches
		// for the next observation finds nothing and must not treat the zero
		// value as one.
		env(at(10*time.Second), adv(mac2(evpnMACGone, "", evpnRD))),
		env(at(70*time.Second), wdr(mac2(evpnMACGone, "", evpnRD))),

		// Withdrawn twice, THEN returns: one completed cycle, not two. The
		// first withdrawal is followed by another withdrawal, so pairing on
		// "the next observation is later" alone reports a return that is
		// itself a withdrawal.
		env(at(5*time.Second), adv(mac2(evpnMACTwiceWithdrawn, "", evpnRD))),
		env(at(45*time.Second), wdr(mac2(evpnMACTwiceWithdrawn, "", evpnRD))),
		env(at(75*time.Second), wdr(mac2(evpnMACTwiceWithdrawn, "", evpnRD))),
		env(at(100*time.Second), adv(mac2(evpnMACTwiceWithdrawn, "", evpnRD))),

		// A type 2 carrying an IP as well as a MAC.
		env(at(15*time.Second), adv(mac2(evpnMACWithIP, evpnIPOfMAC, evpnRD))),

		// A type 5 under the SAME RD as the type 2s, so dropping route_type
		// from the identity merges routes that are not the same route.
		env(at(25*time.Second), adv(&vantagev1.EvpnRoute{
			RouteType: 5, Rd: evpnRD, Prefix: evpnPrefix,
			GatewayIp: "10.203.0.1", Labels: []uint32{25005},
		})),

		// The only route outside in_pre.
		locRib(at(35*time.Second), adv(&vantagev1.EvpnRoute{
			RouteType: 3, Rd: evpnRDLoc, Labels: []uint32{25003},
		})),

		// A type 3 that goes away and stays away, followed shortly after by
		// a type 1 under the SAME RD and Ethernet tag. Neither type
		// populates mac, ip or prefix, so route_type is the only thing
		// keeping them in separate windows -- without it the type 3's
		// withdrawal pairs with the type 1's advertisement and is reported
		// as a flap that never happened.
		env(at(40*time.Second), adv(&vantagev1.EvpnRoute{
			RouteType: 3, Rd: evpnRDShared, Labels: []uint32{25013},
		})),
		env(at(50*time.Second), wdr(&vantagev1.EvpnRoute{
			RouteType: 3, Rd: evpnRDShared, Labels: []uint32{25013},
		})),
		env(at(55*time.Second), adv(&vantagev1.EvpnRoute{
			RouteType: 1, Rd: evpnRDShared, Esi: "0011223344556677889a",
			Labels: []uint32{25011},
		})),

		// One MAC, two routers, different histories: two rows, not one.
		envOn(evpnRouter, at(45*time.Second), adv(mac2(evpnMACShared, "", evpnRD))),
		envOn(evpnRouter, at(75*time.Second), adv(mac2(evpnMACShared, "", evpnRD))),
		envOn(evpnRouterB, at(45*time.Second), adv(mac2(evpnMACShared, "", evpnRD))),
		envOn(evpnRouterB, at(80*time.Second), wdr(mac2(evpnMACShared, "", evpnRD))),

		// Everything below lives at minutes 99-114 of evpnBucketBase, clear
		// of the rows above (which occupy minutes 60-61), so the buckets the
		// time-series tests name hold these rows and nothing else.

		// The mixed bucket, half of it. Dumped in minute 99, then genuinely
		// re-advertised TWICE inside minute 100, all in one session.
		sess(evpnSession, evpnBucketAt(99, 0), adv(mac2(evpnBucketMACFlap, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(100, 5), adv(mac2(evpnBucketMACFlap, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(100, 25), adv(mac2(evpnBucketMACFlap, "", evpnRDBucket))),

		// The other half. Two observations in the SAME minute-100 bucket,
		// one per session, neither of them a change.
		sess(evpnSession2, evpnBucketAt(100, 35), adv(mac2(evpnBucketMACRedump, "", evpnRDBucket))),
		sess(evpnSession3, evpnBucketAt(100, 45), adv(mac2(evpnBucketMACRedump, "", evpnRDBucket))),

		// A withdrawal that is the first word about its route in its
		// session, alone in minute 105.
		sess(evpnSession2, evpnBucketAt(105, 0), wdr(pfx5(evpnRDBucket, evpnPfxLoneWithdraw))),

		// Minutes 110-121: twelve pairs, each two DIFFERENT routes separated
		// by one component of the EVPN key and by nothing else. Every row is
		// the first observation of its own route inside evpnSession, so
		// every bucket here is two dumps and zero changes -- unless the
		// component is missing from the classification, in which case the
		// second row of that pair is reported as a change.
		sess(evpnSession, evpnBucketAt(110, 0), adv(mac2(evpnKeyIPMac, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(110, 10), adv(mac2(evpnKeyIPMac, evpnKeyIP, evpnRDBucket))),

		sess(evpnSession, evpnBucketAt(111, 0), adv(imet(evpnRDBucket, evpnKeyTagA, ""))),
		sess(evpnSession, evpnBucketAt(111, 10), adv(imet(evpnRDBucket, evpnKeyTagB, ""))),

		sess(evpnSession, evpnBucketAt(112, 0), adv(ead(evpnRDBucket, evpnKeyESITag, evpnKeyESIA))),
		sess(evpnSession, evpnBucketAt(112, 10), adv(ead(evpnRDBucket, evpnKeyESITag, evpnKeyESIB))),

		sess(evpnSession, evpnBucketAt(113, 0), adv(macPath(evpnKeyPathMAC, evpnRDBucket, 0))),
		sess(evpnSession, evpnBucketAt(113, 10), adv(macPath(evpnKeyPathMAC, evpnRDBucket, 7))),

		sess(evpnSession, evpnBucketAt(114, 0), adv(imet(evpnRDBucket, evpnKeyTypeTag, ""))),
		sess(evpnSession, evpnBucketAt(114, 10), adv(ead(evpnRDBucket, evpnKeyTypeTag, ""))),

		sess(evpnSession, evpnBucketAt(115, 0), adv(mac2(evpnKeyPeerMAC, "", evpnRDBucket))),
		peerB(evpnBucketAt(115, 10), adv(mac2(evpnKeyPeerMAC, "", evpnRDBucket))),

		sess(evpnSession, evpnBucketAt(116, 0), adv(mac2(evpnKeyRibMAC, "", evpnRDBucket))),
		inPost(evpnBucketAt(116, 10), adv(mac2(evpnKeyRibMAC, "", evpnRDBucket))),

		sess(evpnSession, evpnBucketAt(117, 0), adv(mac2(evpnKeyRDMAC, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(117, 10), adv(mac2(evpnKeyRDMAC, "", evpnRDBucketAlt))),

		sess(evpnSession, evpnBucketAt(118, 0), adv(pfx5(evpnRDBucket, evpnKeyPfxA))),
		sess(evpnSession, evpnBucketAt(118, 10), adv(pfx5(evpnRDBucket, evpnKeyPfxB))),

		sess(evpnSession, evpnBucketAt(119, 0), adv(mac2(evpnKeyMACA, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(119, 10), adv(mac2(evpnKeyMACB, "", evpnRDBucket))),

		envOn(evpnRouter, evpnBucketAt(120, 0), adv(mac2(evpnKeyRouterMAC, "", evpnRDBucket))),
		envOn(evpnRouterB, evpnBucketAt(120, 10), adv(mac2(evpnKeyRouterMAC, "", evpnRDBucket))),

		// The SECOND collector observes FIRST, which is what makes
		// collector_id in the classification's route key observable at all
		// now that this panel reports one collector rather than the sum.
		// Both rows carry evpnSession -- session identity is (collector_id,
		// router_ip, session_id), and this pair is the only thing in the
		// fixture that can be wrong about the collector half. Drop
		// collector_id from the PARTITION BY and the two copies become one
		// route whose earliest observation is c2's, which demotes the
		// WINNING collector's own first observation from a dump to a
		// re-advertisement -- see
		// TestEvpnChurnSeriesReportsOneCollectorsViewNotTheSumOfBoth, which
		// is where that is pinned.
		collectorB(evpnBucketAt(121, 0), adv(mac2(evpnKeyCollectorMAC, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(121, 10), adv(mac2(evpnKeyCollectorMAC, "", evpnRDBucket))),
		// The lagging-collector trio -- see evpnKeyLagFlapMAC.
		// Minute 124, not 122: 122 is evpnZeroClockMAC's and
		// TestEvpnChurnSeriesOrdersTheSessionOnTheCollectorsSequence pins that
		// bucket at one observation. 123 is the skewed router's.
		sess(evpnSession, evpnBucketAt(124, 0), adv(mac2(evpnKeyLagFlapMAC, "", evpnRDBucket))),
		sess(evpnSession, evpnBucketAt(124, 30), wdr(mac2(evpnKeyLagFlapMAC, "", evpnRDBucket))),
		collectorB(evpnBucketAt(124, 40), adv(mac2(evpnKeyLagFlapMAC, "", evpnRDBucket))),

		// Minutes 122 and 123: one route, two observations, and a router
		// clock that goes to zero between them. The dump is the one in
		// minute 122 because it came first in the collector's stream; a
		// classification ordered on ts_router puts it in minute 123 instead,
		// and the fixture-wide totals do not move when it does.
		env(evpnBucketAt(122, 0), adv(mac2(evpnZeroClockMAC, "", evpnRDBucket))),
		skewed(evpnBucketAt(123, 0), time.Unix(0, 0).UTC(),
			adv(mac2(evpnZeroClockMAC, "", evpnRDBucket))),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert evpn churn fixture envelope %d: %v", i, err)
		}
	}

	// One at-least-once re-delivery: the SAME message with the SAME stream
	// sequence, written a second time. The panel reads route_evpn FINAL, and
	// this row is what makes that a requirement these tests can see.
	//
	// A duplicate of a genuine re-advertisement is where it does damage. The
	// classification asks whether a row is its route's minimum (seq,
	// stream_seq) inside its session; two copies of one re-advertisement
	// share that pair, so neither is the minimum and BOTH count as changes.
	// The 2026-09-10 rig measured exactly that on route_unicast: a lab archive
	// whose observations were each delivered twice reported 198 changes
	// where the truth was 99. ReplacingMergeTree collapses the pair on the
	// ORDER BY key, so with FINAL the arithmetic below is unchanged and
	// without it evpnBucketMACFlap gains an observation and a change.
	//
	// It is deliberate and not redundant, though it looks it. chtest drops
	// and recreates testDB once per test BINARY while every test here
	// re-inserts this whole fixture, so the table already holds duplicates
	// of every row -- FINAL is load-bearing today by accident of that
	// lifecycle rather than by design, and a guarantee that depends on how
	// many sibling tests ran first is not a guarantee.
	redelivered := -1
	for i, ev := range envs {
		anns := ev.GetRoute().GetEvpnAnnounced()
		if len(anns) == 1 && anns[0].GetMac() == evpnBucketMACFlap &&
			ev.GetTsCollector().AsTime().Equal(evpnBucketAt(100, 5)) {
			redelivered = i
			break
		}
	}
	if redelivered < 0 {
		t.Fatalf("no envelope re-advertises %s inside minute 100 -- the "+
			"re-delivery has to duplicate a genuine re-advertisement, "+
			"because a duplicated session dump is still a session dump and "+
			"would prove nothing", evpnBucketMACFlap)
	}
	if err := ch.Insert(ctx, mustRowsFor(t, envs[redelivered], uint64(redelivered+1))); err != nil {
		t.Fatalf("insert evpn churn fixture re-delivery: %v", err)
	}
}

// scopeEvpnChurnToFixtureRouter narrows an EVPN churn query to the one router
// this fixture owns, by extending the panel's own time filter. testDB is
// shared and holds 202 real EVPN rows from three NX-OS leaves, all of which
// genuinely flap -- so an unscoped count here would be a claim about the
// corpus rather than about this fixture.
func scopeEvpnChurnToFixtureRouter(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("an evpn-churn query carries %d occurrences of %q, want 1 -- "+
			"these tests scope to the fixture's router by extending that "+
			"filter, and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip IN (toIPv6('"+evpnRouter+"'), toIPv6('"+evpnRouterB+"'))", 1)
}

// evpnChurnRow is one row of the committed "Most unstable routes" table.
type evpnChurnRow struct {
	route          string
	typeName       string
	rd             string
	router         string
	advertisements uint64
	withdrawals    uint64
	flaps          uint64
	secondsAway    float64
	state          string
	lastSeen       time.Time
}

// evpnChurn keys the committed table by route, for the FIRST fixture router
// only. A MAC advertised by two routers is two rows and would otherwise
// collide here; reach for evpnChurnAll where that is the point.
func evpnChurn(t *testing.T, ctx context.Context, ch *ClickHouse, routeType, rib string) map[string]evpnChurnRow {
	t.Helper()
	out := map[string]evpnChurnRow{}
	for _, r := range evpnChurnAll(t, ctx, ch, routeType, rib) {
		if r.router == "dash-evpn1" {
			out[r.route] = r
		}
	}
	return out
}

func evpnChurnAll(t *testing.T, ctx context.Context, ch *ClickHouse, routeType, rib string) []evpnChurnRow {
	t.Helper()
	raw := scopeEvpnChurnToFixtureRouter(t, panelSQL(t, "evpn-churn", "Most unstable routes", "A"))
	sql := substituteGrafana(raw, map[string]string{"route_type": routeType, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("evpn churn (route_type=%q): %v\nSQL:\n%s", routeType, err, sql)
	}
	defer rows.Close()
	var out []evpnChurnRow
	for rows.Next() {
		var r evpnChurnRow
		if err := rows.Scan(&r.route, &r.typeName, &r.rd, &r.router,
			&r.advertisements, &r.withdrawals, &r.flaps, &r.secondsAway,
			&r.state, &r.lastSeen); err != nil {
			t.Fatalf("scan evpn churn row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("evpn churn rows: %v", err)
	}
	return out
}

// evpnWhatWeKnow is evpn-churn's "What we know" stat panel, which frames the
// two tables under it.
type evpnWhatWeKnow struct {
	routes         uint64
	routesFlapping uint64
	flapCycles     uint64
	medianSeconds  float64
}

func evpnStat(t *testing.T, ctx context.Context, ch *ClickHouse, routeType, rib string) evpnWhatWeKnow {
	t.Helper()
	raw := scopeEvpnChurnToFixtureRouter(t, panelSQL(t, "evpn-churn", "What we know", "A"))
	sql := substituteGrafana(raw, map[string]string{"route_type": routeType, "rib": rib})
	var s evpnWhatWeKnow
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(
		&s.routes, &s.routesFlapping, &s.flapCycles, &s.medianSeconds); err != nil {
		t.Fatalf("evpn what-we-know (route_type=%q): %v\nSQL:\n%s", routeType, err, sql)
	}
	return s
}

// evpn-churn's "What we know" is the stat row an operator reads before the
// tables under it, and it was the last panel on that dashboard with nothing
// asserting a value in it.
//
// THE CLAIM ITS DESCRIPTION MAKES is the one worth pinning: "a withdrawal
// that is simply the last thing known about a route is not a flap, and
// counting it as one would make every route that was ever removed look
// unstable". The fixture is now heavily weighted against that mistake -- it
// holds EIGHT withdrawals and only THREE of them complete a cycle:
//
//	evpnMACFlapper        t+20 -> t+30   flap, 10s away
//	evpnMACFlapper        t+60 -> t+90   flap, 30s away
//	evpnMACTwiceWithdrawn t+75 -> t+100  flap, 25s away
//	evpnMACTwiceWithdrawn t+45 -> t+75   NOT: the next word is another withdrawal
//	evpnMACGone           t+70 -> .      NOT: nothing followed it
//	evpnRDShared type 3   t+50 -> .      NOT: what follows is a type 1, a
//	                                     different route under one RD
//	evpnMACShared on B    t+80 -> .      NOT: nothing followed it
//	evpnPfxLoneWithdraw   b105 -> .      NOT: nothing followed it
//
// So a panel that counted withdrawals would read 8 where this reads 3, and
// would report five routes as unstable that simply went away.
//
// The gaps are deliberately three DIFFERENT numbers, so the median is not
// also the mean, the max or the only value -- 25 is a real middle.
func TestEvpnStatCountsCompletedCyclesNotWithdrawals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	got := evpnStat(t, ctx, ch, ".*", ".*")
	if got.flapCycles != 3 {
		t.Errorf("flap_cycles = %d, want 3 -- the fixture holds eight "+
			"withdrawals and exactly three of them are followed by the same "+
			"route coming back. 8 means the panel is counting withdrawals",
			got.flapCycles)
	}
	if got.routesFlapping != 2 {
		t.Errorf("routes_flapping = %d, want 2 -- three cycles, but two of "+
			"them belong to one route. A panel reporting 3 here is counting "+
			"cycles twice and telling an operator more routes are unstable "+
			"than are", got.routesFlapping)
	}
	if got.medianSeconds != 25 {
		t.Errorf("median_seconds_away = %v, want 25 -- the three gaps are 10, "+
			"30 and 25, chosen so the median is not also the mean (21.7), the "+
			"max (30) or the only value", got.medianSeconds)
	}
	// The denominator has to be bigger than the numerator, or "how much of
	// the table is moving" is not a proportion of anything.
	if got.routes <= got.routesFlapping {
		t.Errorf("routes = %d and routes_flapping = %d; the fixture holds many "+
			"routes that never moved, so the total must exceed the flapping "+
			"count or this stat frames nothing", got.routes, got.routesFlapping)
	}
}

// The empty case, which has its own branch in the SQL and its own way of
// going wrong. quantile over an empty set is not 0 -- the panel carries an
// explicit `if(countIf(...) = 0, 0, quantileIf(...))` for that, and without
// it a route type that never flapped renders nan rather than a zero.
//
// route_type 3 is the one to ask with: the fixture has several Inclusive
// Multicast routes and exactly one type-3 withdrawal, the evpnRDShared one
// that never returns. So there is a real population, and none of it flapped.
func TestEvpnStatReadsZeroRatherThanNaNWhenNothingFlapped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	got := evpnStat(t, ctx, ch, "3", ".*")
	if got.routes == 0 {
		t.Fatal("no type-3 routes at all, so the zero below would be the " +
			"zero of an empty table rather than of a population that did " +
			"not flap")
	}
	if got.flapCycles != 0 || got.routesFlapping != 0 {
		t.Errorf("type 3 reports %d cycles over %d routes, want 0 over 0 -- "+
			"its only withdrawal is the evpnRDShared route that never came "+
			"back", got.flapCycles, got.routesFlapping)
	}
	if got.medianSeconds != 0 {
		t.Errorf("median_seconds_away = %v, want 0. quantile over an empty "+
			"set is not zero, which is why the panel guards it -- an operator "+
			"reading nan here learns nothing about a route type that is "+
			"perfectly stable", got.medianSeconds)
	}
}

// evpnFlapCycle is one row of the committed "Flap cycles" table.
type evpnFlapCycle struct {
	// collector: a flap is a per-EVENT row and two collectors watching one
	// router each see the same flap, with no identity shared between their
	// copies to deduplicate on. The listing names which collector saw it
	// rather than silently showing one flap twice -- asn-view's Prefixes
	// table's answer to the same question.
	collector   string
	route       string
	typeName    string
	rd          string
	router      string
	withdrawnAt time.Time
	returnedAt  time.Time
	secondsAway float64
}

func evpnFlapCycles(t *testing.T, ctx context.Context, ch *ClickHouse, routeType, rib string) []evpnFlapCycle {
	t.Helper()
	raw := scopeEvpnChurnToFixtureRouter(t, panelSQL(t, "evpn-churn", "Flap cycles", "A"))
	sql := substituteGrafana(raw, map[string]string{"route_type": routeType, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("flap cycles (route_type=%q): %v\nSQL:\n%s", routeType, err, sql)
	}
	defer rows.Close()
	var out []evpnFlapCycle
	for rows.Next() {
		var c evpnFlapCycle
		if err := rows.Scan(&c.collector, &c.route, &c.typeName, &c.rd, &c.router,
			&c.withdrawnAt, &c.returnedAt, &c.secondsAway); err != nil {
			t.Fatalf("scan flap cycle: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("flap cycle rows: %v", err)
	}
	return out
}

// "Flap cycles" is the evidence table under the counts: every completed
// withdrawal-and-return individually, longest gap first, so a cycle can be
// checked against route_evpn by hand rather than trusted. It had no test.
//
// Its whole difficulty is the PAIRING. It reaches one observation forward
// inside a window partitioned by the full route identity, and three things
// can go wrong, all of which invent cycles that never happened or lose ones
// that did. This fixture was built to punish each:
//
//   - THE PARTITION EDGE. evpnMACGone ends on a withdrawal with nothing
//     after it. The window's next-row lookup returns the zero value there,
//     so without the panel's `nxt > ts_collector` guard that row pairs with
//     an empty timestamp and reports a gap of minus fifty-six years -- which
//     then sorts to the TOP of a table ordered by gap descending, putting a
//     fiction in the first row an operator reads.
//   - THE IDENTITY. evpnRDShared carries a type 3 that is withdrawn at t+50
//     and a type 1 advertised at t+55, same RD, same Ethernet tag. Neither
//     type populates mac, ip or prefix, so route_type is the ONLY column
//     keeping them in separate windows. Drop it and the type 3's withdrawal
//     pairs with the type 1's advertisement: a five-second flap of a route
//     that never came back.
//   - THE RETURN ITSELF. A withdrawal followed by another withdrawal is not
//     a cycle, which is what `nxt_w = 0` is for.
//
// The flapper is away 10s and then 30s, so count, sum and max are three
// different numbers and no single one of them can stand in for the pair.
func TestEvpnFlapCyclesPairsEachWithdrawalWithItsOwnReturn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	var flapper []evpnFlapCycle
	byRoute := map[string]int{}
	for _, c := range evpnFlapCycles(t, ctx, ch, ".*", ".*") {
		if c.router != "dash-evpn1" {
			continue
		}
		byRoute[c.route]++
		if c.route == evpnMACFlapper {
			flapper = append(flapper, c)
		}
	}

	// Two cycles, longest first, and the gaps are the fixture's own.
	if len(flapper) != 2 {
		t.Fatalf("the flapper has %d cycles, want 2 -- it is withdrawn at "+
			"t+20 returning t+30, and withdrawn at t+60 returning t+90; got %+v",
			len(flapper), flapper)
	}
	if flapper[0].secondsAway != 30 || flapper[1].secondsAway != 10 {
		t.Errorf("gaps are %v and %v, want 30 then 10 -- this table's claim is "+
			"\"longest gap first\", and its order IS the ranking an operator "+
			"reads", flapper[0].secondsAway, flapper[1].secondsAway)
	}
	// The two bounding observations must actually bound the gap, which is
	// what lets an operator check a row against route_evpn by hand.
	for _, c := range flapper {
		if !c.returnedAt.After(c.withdrawnAt) {
			t.Errorf("cycle returned_at %s is not after withdrawn_at %s",
				c.returnedAt, c.withdrawnAt)
		}
		if got := c.returnedAt.Sub(c.withdrawnAt).Seconds(); got != c.secondsAway {
			t.Errorf("seconds_away = %v but the two timestamps are %v apart; "+
				"the column has to be derivable from the row beside it",
				c.secondsAway, got)
		}
	}

	// The partition edge: a withdrawal with nothing after it is not a cycle.
	if n := byRoute[evpnMACGone]; n != 0 {
		t.Errorf("the route that was withdrawn and never returned has %d "+
			"cycles, want 0. Its withdrawal is the LAST row for that key, so a "+
			"pairing that takes the window's zero value reports a return that "+
			"never happened -- at a negative gap, which sorts it to the top of "+
			"a table ordered longest-first", n)
	}
	// A route that was never withdrawn cannot have completed a cycle.
	if n := byRoute[evpnMACStable]; n != 0 {
		t.Errorf("the never-withdrawn route has %d cycles, want 0", n)
	}
	// A withdrawal followed by ANOTHER withdrawal is not a return. This is
	// what `nxt_w = 0` is for, and it is the one clause of this panel that a
	// surviving mutation showed nothing could see.
	if n := byRoute[evpnMACTwiceWithdrawn]; n != 1 {
		t.Errorf("the twice-withdrawn route has %d cycles, want 1 -- it is "+
			"withdrawn at t+45, withdrawn AGAIN at t+75 and only returns at "+
			"t+100. 2 means the first withdrawal was paired with the second "+
			"one: later than it, so the \"next is later\" guard lets it "+
			"through, and reported as a route coming back when it did not", n)
	}
}

// The identity half, kept separate because it fails for a different reason
// and names a different column. A type 3 withdrawn at t+50 and a type 1
// advertised at t+55 under ONE RD and Ethernet tag are two routes, not one
// route returning after five seconds.
func TestEvpnFlapCyclesDoesNotPairTwoRouteTypesSharingAnRD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	for _, c := range evpnFlapCycles(t, ctx, ch, ".*", ".*") {
		if c.rd != evpnRDShared {
			continue
		}
		t.Errorf("the panel reports a %v-second cycle on %s (%s, route %q). "+
			"Nothing under that RD ever came back: a type 3 was withdrawn and "+
			"a DIFFERENT route -- a type 1 -- was advertised five seconds "+
			"later. route_type is the only column separating them, because "+
			"neither type populates mac, ip or prefix",
			c.secondsAway, evpnRDShared, c.typeName, c.route)
	}
}

// The fixture's arrangement is what makes the flap arithmetic decidable.
func TestEvpnChurnFixtureIsArrangedAdversarially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	// One key must end on a withdrawal. Without it the window pairing never
	// reaches the partition edge and the missing guard costs nothing.
	var endsWithdrawn uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, `
		SELECT countIf(last_w = 1) FROM (
			SELECT mac, argMax(is_withdraw, (ts_collector, stream_seq)) AS last_w
			FROM vantage.route_evpn FINAL
			WHERE router_ip = toIPv6('`+evpnRouter+`') AND route_type = 2
			GROUP BY mac
		)`)).Scan(&endsWithdrawn); err != nil {
		t.Fatalf("ends-withdrawn: %v", err)
	}
	if endsWithdrawn != 1 {
		t.Errorf("%d type-2 keys end on a withdrawal, want 1 (%s) -- that key "+
			"is the partition edge the flap pairing has to survive",
			endsWithdrawn, evpnMACGone)
	}

	// One key must never be withdrawn, or "flaps" has no reachable zero.
	var neverWithdrawn uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM (SELECT mac, countIf(is_withdraw=1) w FROM "+
			"vantage.route_evpn FINAL WHERE router_ip = toIPv6('"+evpnRouter+
			"') AND route_type = 2 GROUP BY mac HAVING w = 0)")).Scan(&neverWithdrawn); err != nil {
		t.Fatalf("never-withdrawn: %v", err)
	}
	if neverWithdrawn < 1 {
		t.Errorf("every type-2 key was withdrawn at least once; %s must not "+
			"be, or a panel calling everything a flap passes", evpnMACStable)
	}

	// Two route types under one RD, or route_type need not be in the key.
	var typesUnderRD uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT uniqExact(route_type) FROM vantage.route_evpn FINAL WHERE "+
			"router_ip = toIPv6('"+evpnRouter+"') AND rd = '"+evpnRD+"'")).Scan(&typesUnderRD); err != nil {
		t.Fatalf("types under rd: %v", err)
	}
	if typesUnderRD != 2 {
		t.Errorf("RD %s carries %d route types, want 2 -- a MAC advertisement "+
			"and an IP-prefix route sharing an RD are different routes",
			evpnRD, typesUnderRD)
	}
}

// The dashboard exists to answer "what is flapping". A flap is a withdrawal
// followed by the route coming BACK; a withdrawal that is simply the end of
// the story is not one, and conflating them turns every route that was ever
// removed into a flapper.
func TestEvpnChurnCountsReturnsNotWithdrawals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	rows := evpnChurn(t, ctx, ch, ".*", ".*")

	f, ok := rows[evpnMACFlapper]
	if !ok {
		t.Fatalf("%s absent from the churn table; it flaps twice", evpnMACFlapper)
	}
	if f.flaps != 2 {
		t.Errorf("%s flaps = %d, want 2 -- it was withdrawn twice and came "+
			"back both times", evpnMACFlapper, f.flaps)
	}
	if f.withdrawals != 2 || f.advertisements != 3 {
		t.Errorf("%s = %d advertisements / %d withdrawals, want 3/2",
			evpnMACFlapper, f.advertisements, f.withdrawals)
	}

	// Withdrawn once and never seen again: one withdrawal, ZERO flaps.
	g, ok := rows[evpnMACGone]
	if !ok {
		t.Fatalf("%s absent; a route that went away is exactly what an "+
			"operator is looking for here", evpnMACGone)
	}
	if g.withdrawals != 1 {
		t.Errorf("%s withdrawals = %d, want 1", evpnMACGone, g.withdrawals)
	}
	if g.flaps != 0 {
		t.Errorf("%s flaps = %d, want 0 -- it never came back, so its "+
			"withdrawal closes no cycle", evpnMACGone, g.flaps)
	}
	if g.state != "withdrawn" {
		t.Errorf("%s state = %q, want \"withdrawn\"", evpnMACGone, g.state)
	}

	// Never withdrawn at all.
	s, ok := rows[evpnMACStable]
	if !ok {
		t.Fatalf("%s absent; a stable route still belongs in the table", evpnMACStable)
	}
	if s.flaps != 0 || s.withdrawals != 0 {
		t.Errorf("%s = %d flaps / %d withdrawals, want 0/0",
			evpnMACStable, s.flaps, s.withdrawals)
	}
	if s.state != "advertised" {
		t.Errorf("%s state = %q, want \"advertised\"", evpnMACStable, s.state)
	}
}

// TestEvpnChurnDoesNotInventAFlapFromASecondCollectorsCopy pins the severe
// case an aggregate-only scan cannot see: keying on argMax and GROUP BY has
// no notion of window functions, so it walks past every OVER (PARTITION
// BY ...) in the repo.
//
// These panels find flaps with a lead window -- "this row is a withdrawal, the
// NEXT row is not, and it is later" -- partitioned by the route key with NO
// collector_id in it. Two collectors watching one router therefore interleave
// their copies of the same events into one sequence, and the row after a
// withdrawal can be the other collector's copy of an advertisement that
// preceded it.
//
// That is worse than the counting defects fixed elsewhere in this file. A
// doubled count is a number an operator can discount; this INVENTS AN EVENT,
// on the screen whose entire purpose is finding routes that flap, and reports
// a seconds-away figure for time the route was never gone.
//
// It is latent rather than live: route_evpn carries one collector in the
// lab archive today. It arms the moment EVPN is dual-homed, which the dev stack
// is now one replay away from.
func TestEvpnChurnDoesNotInventAFlapFromASecondCollectorsCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	rows := evpnChurn(t, ctx, ch, ".*", ".*")
	r, ok := rows[evpnKeyLagFlapMAC]
	if !ok {
		t.Fatalf("%s absent from the churn table entirely", evpnKeyLagFlapMAC)
	}
	if r.flaps != 0 {
		t.Errorf("%s flaps = %d, want 0. It was advertised once and withdrawn "+
			"once and never came back. The only thing after that withdrawal is "+
			"a SECOND COLLECTOR's copy of the original advertisement, arriving "+
			"late -- and with no collector_id in the window's PARTITION BY, the "+
			"pairing reads it as the route returning", evpnKeyLagFlapMAC, r.flaps)
	}
	if r.state != "withdrawn" {
		t.Errorf("%s state = %q, want \"withdrawn\" -- the last thing the "+
			"router said about it was a withdrawal; a later arrival from "+
			"another collector does not un-say it", evpnKeyLagFlapMAC, r.state)
	}
}

// The last observation of a key has no successor, so a window reaching one
// row forward finds the zero value there.
// Without "the next observation is LATER than this one", that zero is read as
// a timestamp and the gap comes out as minus fifty-six years -- which then
// pollutes every sum, min and median on the panel.
func TestEvpnChurnDoesNotPairTheLastWithdrawalWithNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	rows := evpnChurn(t, ctx, ch, ".*", ".*")

	// The key that ends withdrawn contributes no time-away at all.
	if got := rows[evpnMACGone].secondsAway; got != 0 {
		t.Errorf("%s seconds_away = %v, want 0 -- its withdrawal is the last "+
			"thing known about it, so there is no interval to measure. A "+
			"large negative here is the unguarded window reading the epoch",
			evpnMACGone, got)
	}
	// And no row anywhere may report negative time.
	for name, r := range rows {
		if r.secondsAway < 0 {
			t.Errorf("%s seconds_away = %v, which is negative; time away "+
				"cannot run backwards", name, r.secondsAway)
		}
	}
	// The flapper's two gaps are 10s and 30s: summed, not maxed, not counted.
	if got := rows[evpnMACFlapper].secondsAway; got != 40 {
		t.Errorf("%s seconds_away = %v, want 40 (10s then 30s) -- 30 means "+
			"the panel is reporting the longest gap and 2 means it is "+
			"counting cycles", evpnMACFlapper, got)
	}
}

// EVPN identity is not (rd, prefix). A MAC advertisement and an IP-prefix
// route can share a route distinguisher and are not the same route, and a
// type 2 renders its MAC where a type 5 renders its prefix -- so the table
// needs one column that means "which route" across all of them.
func TestEvpnChurnIdentifiesEachRouteTypeByItsOwnField(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	rows := evpnChurn(t, ctx, ch, ".*", ".*")

	// Type 5 is named by its prefix, and shares evpnRD with the type 2s.
	p, ok := rows[evpnPrefix]
	if !ok {
		t.Fatalf("the type-5 route %s is absent; it shares RD %s with the "+
			"MAC advertisements, so a key without route_type merges them",
			evpnPrefix, evpnRD)
	}
	if p.typeName != "IP prefix" {
		t.Errorf("type 5 rendered as %q, want \"IP prefix\" -- a bare 5 in "+
			"the column tells an operator nothing", p.typeName)
	}
	if p.rd != evpnRD {
		t.Errorf("type-5 rd = %q, want %q", p.rd, evpnRD)
	}

	// A type 2 with an IP shows both; the ones without show the MAC alone.
	if _, ok := rows[evpnMACWithIP+" / "+evpnIPOfMAC]; !ok {
		t.Errorf("the type-2 route carrying an IP is not rendered as %q; the "+
			"MAC-only branch and the MAC+IP branch must differ",
			evpnMACWithIP+" / "+evpnIPOfMAC)
	}
	if r, ok := rows[evpnMACFlapper]; !ok || r.typeName != "MAC/IP advertisement" {
		t.Errorf("type 2 rendered as %q, want \"MAC/IP advertisement\"", r.typeName)
	}
}

// $route_type is the dashboard's main control and the picker is built from
// the types that exist, so it cannot offer one no panel renders.
func TestEvpnChurnRouteTypeNarrowsAndPickerIsScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	only2 := evpnChurn(t, ctx, ch, "2", ".*")
	if _, leaked := only2[evpnPrefix]; leaked {
		t.Errorf("$route_type = 2 returned the type-5 route %s", evpnPrefix)
	}
	if _, missing := only2[evpnMACFlapper]; !missing {
		t.Errorf("$route_type = 2 dropped %s, which is a type 2", evpnMACFlapper)
	}

	// Every option the committed picker offers must render at least one row.
	vars := dashboardQueryVariables(t, "evpn-churn")
	q, ok := vars["route_type"]
	if !ok {
		t.Fatalf("evpn-churn declares no query variable named route_type; it "+
			"declares %v", slices.Sorted(maps.Keys(vars)))
	}
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(
		scopeEvpnChurnToFixtureRouter(t, q), nil)))
	if err != nil {
		t.Fatalf("route_type picker query: %v\nSQL:\n%s", err, q)
	}
	// COLUMN ORDER IS THE CONTRACT, not the column names. Grafana's
	// ClickHouse datasource reads a query variable's first column as the
	// VALUE and its second as the label, and ignores the __value/__text
	// names that MySQL and Postgres honor. Selecting them the other way
	// round returns two perfectly good columns and renders a picker full of
	// bare numbers -- which is what shipped until a browser showed it, since
	// asserting only that the two columns differ passes in both orders.
	numeric := regexp.MustCompile(`^\d+$`)
	var offered []string
	for rows.Next() {
		var value, label string
		if err := rows.Scan(&value, &label); err != nil {
			t.Fatalf("scan picker option (value first, then label): %v", err)
		}
		if !numeric.MatchString(value) {
			t.Errorf("picker's FIRST column is %q; it is the value the panels "+
				"receive and must be the bare route type, so that "+
				"match(toString(route_type), ...) can use it", value)
		}
		if !strings.HasPrefix(label, value+" - ") {
			t.Errorf("picker's SECOND column is %q; it is what an operator "+
				"reads and must name the type, e.g. %q", label, value+" - IP prefix")
		}
		offered = append(offered, value)
	}
	rows.Close()
	if len(offered) != 4 {
		t.Fatalf("picker offers %v, want 4 types (1, 2, 3 and 5) for this "+
			"fixture", offered)
	}
	for _, v := range offered {
		if len(evpnChurn(t, ctx, ch, v, ".*")) == 0 {
			t.Errorf("picker offers route type %s and the table renders no "+
				"rows for it", v)
		}
	}
}

// Every route this fixture writes is in_pre except one, and the whole live
// lab archive is in_pre, so this is the only place $rib can be caught unread.
func TestEvpnChurnRibNarrowsToTheChosenStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	var locInAll, locInPre bool
	for _, r := range evpnChurn(t, ctx, ch, ".*", ".*") {
		if r.rd == evpnRDLoc {
			locInAll = true
		}
	}
	for _, r := range evpnChurn(t, ctx, ch, ".*", "in_pre") {
		if r.rd == evpnRDLoc {
			locInPre = true
		}
	}
	if !locInAll {
		t.Fatalf("RD %s is absent with $rib on All; its route is loc_rib and "+
			"All must include it", evpnRDLoc)
	}
	if locInPre {
		t.Errorf("RD %s is still listed with $rib = in_pre; its only route is "+
			"in loc_rib, so the panel is not reading $rib", evpnRDLoc)
	}
}

// EVPN identity includes the route type. Types 1 and 3 populate none of mac,
// ip or prefix, so under one RD and one Ethernet tag they are distinguished
// by route_type alone -- and if the flap pairing loses it, one route's
// withdrawal is matched with the other's advertisement and reported as a
// flap that never happened.
func TestEvpnChurnDoesNotPairAcrossRouteTypes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	byType := map[string]evpnChurnRow{}
	for _, r := range evpnChurnAll(t, ctx, ch, ".*", ".*") {
		if r.rd == evpnRDShared {
			byType[r.typeName] = r
		}
	}
	imet, okIMET := byType["Inclusive multicast"]
	_, okEAD := byType["Ethernet auto-discovery"]
	if !okIMET || !okEAD {
		t.Fatalf("RD %s must carry both a type 3 and a type 1; it carries %v",
			evpnRDShared, slices.Sorted(maps.Keys(byType)))
	}
	if imet.flaps != 0 {
		t.Errorf("the type-3 route under RD %s reports %d flaps, want 0 -- it "+
			"was withdrawn and never came back. A flap here means its "+
			"withdrawal was paired with the type-1 advertisement that "+
			"follows it, which is a different route", evpnRDShared, imet.flaps)
	}
	if imet.state != "withdrawn" {
		t.Errorf("the type-3 route under RD %s is %q, want withdrawn",
			evpnRDShared, imet.state)
	}
	if imet.secondsAway != 0 {
		t.Errorf("the type-3 route under RD %s reports %v seconds away, want "+
			"0", evpnRDShared, imet.secondsAway)
	}
}

// A leaf and the route reflector above it see the same MAC and disagree about
// its history. Merging them averages away exactly the disagreement an
// operator opens this dashboard to find.
func TestEvpnChurnKeepsEachRoutersViewSeparate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	byRouter := map[string]evpnChurnRow{}
	for _, r := range evpnChurnAll(t, ctx, ch, ".*", ".*") {
		if r.route == evpnMACShared {
			byRouter[r.router] = r
		}
	}
	if len(byRouter) != 2 {
		t.Fatalf("%s appears as %d rows, want 2 (one per router) -- got %v",
			evpnMACShared, len(byRouter), slices.Sorted(maps.Keys(byRouter)))
	}
	if a := byRouter["dash-evpn1"]; a.advertisements != 2 || a.withdrawals != 0 {
		t.Errorf("dash-evpn1 sees %s as %d advertisements / %d withdrawals, "+
			"want 2/0", evpnMACShared, a.advertisements, a.withdrawals)
	}
	if b := byRouter["dash-evpn2"]; b.advertisements != 1 || b.withdrawals != 1 {
		t.Errorf("dash-evpn2 sees %s as %d advertisements / %d withdrawals, "+
			"want 1/1 -- the two routers disagree, which is the point",
			evpnMACShared, b.advertisements, b.withdrawals)
	}
	if b := byRouter["dash-evpn2"]; b.state != "withdrawn" {
		t.Errorf("dash-evpn2 sees %s as %q, want withdrawn", evpnMACShared, b.state)
	}
}

// evpnSeriesTitle is the committed title of the time-series panel, named once
// so a rename is one edit and a typo is a hard failure in panelSQL rather
// than a skipped assertion.
const evpnSeriesTitle = "Re-advertisements, session dumps and withdrawals"

// evpnSeriesSQL renders that panel for one $rib, one $route_type and one
// bucket width, scoped to the routers this fixture owns.
//
// The bucket width is substituted rather than left to substituteGrafana's
// fixed one minute, because the property the fix exists for is that the
// answer does NOT move when the width does -- Grafana derives the width from
// the dashboard's time range, so an operator changes it just by zooming.
func evpnSeriesSQL(t *testing.T, rib, routeType, width string) string {
	t.Helper()
	raw := scopeEvpnChurnToFixtureRouter(t, panelSQL(t, "evpn-churn", evpnSeriesTitle, "A"))

	const intervalAnchor = "$__timeInterval(ts_collector)"
	if n := strings.Count(raw, intervalAnchor); n != 1 {
		t.Fatalf("evpn-churn panel %q carries %d occurrences of %q, want 1 "+
			"-- a time-series panel that stopped bucketing on ts_collector "+
			"is not the panel these tests are asserting about",
			evpnSeriesTitle, n, intervalAnchor)
	}
	raw = strings.Replace(raw, intervalAnchor,
		"toStartOfInterval(ts_collector, INTERVAL "+width+")", 1)

	return substituteGrafana(raw, map[string]string{"rib": rib, "route_type": routeType})
}

// evpnSeries runs that panel, keyed by bucket start in Unix seconds.
//
// It borrows churnBucketRow and churnCountsAsInt64 from the unicast tests
// deliberately: the two panels now compute the same three series by the same
// rule over different tables, and the Int64 widening is needed here for the
// identical reason it is needed there. countIf returns UInt64 while every
// candidate this panel was chosen over -- count() - uniqExact(session_id)
// above all -- is an Int64 subtraction, and clickhouse-go will not scan
// either into the other's pointer. A helper committed to the shipped width
// would kill those mutations on "converting Int64 to *uint64 is unsupported"
// before comparing a single number, which shows only that a mutation is
// differently typed and not that it is wrong.
func evpnSeries(t *testing.T, ctx context.Context, ch *ClickHouse, rib, routeType, width string) map[int64]churnBucketRow {
	t.Helper()
	sql := churnCountsAsInt64(evpnSeriesSQL(t, rib, routeType, width),
		[]string{"t"}, "readvertise", "withdraw", "session_dump")
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("evpn churn series (%s buckets): %v\nSQL:\n%s", width, err, sql)
	}
	defer rows.Close()
	out := map[int64]churnBucketRow{}
	for rows.Next() {
		var ts time.Time
		var r churnBucketRow
		if err := rows.Scan(&ts, &r.readvertise, &r.withdraw, &r.sessionDump); err != nil {
			t.Fatalf("scan evpn churn series row: %v", err)
		}
		out[ts.Unix()] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("evpn churn series rows: %v", err)
	}
	return out
}

// evpnRawBucket counts the fixture's rows in one bucket with no correction at
// all -- the number the panel reported until 2026-09-10 -- alongside the
// sessions visible in it, which is the operand of the candidate that was
// rejected.
func evpnRawBucket(t *testing.T, ctx context.Context, ch *ClickHouse, start time.Time, width string) (observations, sessions int64) {
	t.Helper()
	sql := fmt.Sprintf(`
		SELECT toInt64(count()), toInt64(uniqExact(session_id))
		FROM vantage.route_evpn FINAL
		WHERE router_ip IN (toIPv6('%s'), toIPv6('%s'))
		  AND toStartOfInterval(ts_collector, INTERVAL %s) = toDateTime64(%d, 6)`,
		evpnRouter, evpnRouterB, width, start.Unix())
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&observations, &sessions); err != nil {
		t.Fatalf("raw evpn bucket at %s: %v", start, err)
	}
	return observations, sessions
}

// evpnRawBucketForCollector is evpnRawBucket narrowed to ONE collector's
// rows, which is what the accounting invariant has to compare against now
// that the panel reports the best single vantage point rather than the sum.
//
// The collector is named by the caller rather than derived here on purpose.
// Re-deriving "which collector saw the most" would put a second copy of the
// selection rule in the test that is supposed to judge it; naming it, and
// asserting the fixture's per-collector row counts separately, keeps the
// test about the CLASSIFICATION being total and fails loudly if the fixture
// ever changes which collector wins.
func evpnRawBucketForCollector(t *testing.T, ctx context.Context, ch *ClickHouse, start time.Time, width, collector string) (observations int64) {
	t.Helper()
	sql := fmt.Sprintf(`
		SELECT toInt64(count())
		FROM vantage.route_evpn FINAL
		WHERE router_ip IN (toIPv6('%s'), toIPv6('%s'))
		  AND collector_id = '%s'
		  AND toStartOfInterval(ts_collector, INTERVAL %s) = toDateTime64(%d, 6)`,
		evpnRouter, evpnRouterB, collector, width, start.Unix())
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&observations); err != nil {
		t.Fatalf("raw evpn bucket for %s at %s: %v", collector, start, err)
	}
	return observations
}

// The proven defect, in the one bucket built to prove it.
//
// "Advertisements vs withdrawals" plotted countIf(is_withdraw = 0), which
// never referenced session_id, so a leaf restarting its BMP session and
// re-dumping a MAC table nothing had happened to was indistinguishable from
// the same number of real route changes -- on a dashboard named for churn,
// whose description talked about clock trust and never mentioned re-dumps.
// Measured on the lab archive on 2026-09-10: of the 658 advertisements the
// panel reported, 507 were session table dumps and 151 were genuine
// re-advertisements.
//
// This compresses the 2026-09-10 demonstration into a single bucket, which
// is strictly harder than that earlier rig: there the two
// shapes sat in separate buckets and could in principle be told apart by
// noticing which bucket a row fell in. Here they are INTERLEAVED inside one
// minute, so only a per-row classification can separate them.
//
// Four observations, two of them changes. The three candidates disagree --
// see evpnBucketMACFlap's comment for the arithmetic -- and this test pins
// the panel to the one that is right.
func TestEvpnChurnSeriesSeparatesChangeFromRedumpInsideOneBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	mixed := evpnBucketAt(100, 0)

	// The premise, asserted rather than assumed: four observations from
	// three sessions, all inside one minute.
	obs, sessions := evpnRawBucket(t, ctx, ch, mixed, "1 MINUTE")
	if obs != 4 || sessions != 3 {
		t.Fatalf("the mixed bucket holds %d observations from %d sessions, "+
			"want 4 from 3 -- without that shape this test proves nothing, "+
			"because count() and count()-uniqExact(session_id) would not "+
			"disagree with the truth", obs, sessions)
	}

	got, present := evpnSeries(t, ctx, ch, ".*", ".*", "1 MINUTE")[mixed.Unix()]
	if !present {
		t.Fatalf("the panel reports no bucket at %s at all", mixed)
	}
	if got.readvertise != 2 {
		t.Errorf("readvertise = %d in the mixed bucket, want 2 -- %s was "+
			"re-advertised twice inside session %d there. 4 means the panel "+
			"is back to counting messages; 1 means it is subtracting the "+
			"sessions visible in the bucket, which cancels a re-dump and a "+
			"change alike", got.readvertise, evpnBucketMACFlap, evpnSession)
	}
	if got.sessionDump != 2 {
		t.Errorf("session_dump = %d in the mixed bucket, want 2 -- %s was "+
			"seen once in session %d and once in session %d, and neither was "+
			"a change", got.sessionDump, evpnBucketMACRedump,
			evpnSession2, evpnSession3)
	}
	if got.withdraw != 0 {
		t.Errorf("withdraw = %d in the mixed bucket, want 0; nothing is "+
			"withdrawn there", got.withdraw)
	}
}

// The property the fix exists for: the answer does not move when the bucket
// width does.
//
// Grafana picks the bucket width from the dashboard's time range, so an
// operator changes it just by zooming. The bucket-corrected candidate --
// count() - uniqExact(session_id) -- cannot hold this property even in
// principle: it can only see two observations as belonging to one session if
// they land in the SAME bucket, so narrowing the bucket erodes the signal.
// That erosion was measured on a route genuinely re-advertised 99
// times inside one session: 99 of 99 captured at one-day buckets, 98 at one
// hour, 93 at fifteen minutes, 80 at five, and 0 at one minute. A panel that
// reports a fabric as stable at one zoom level and flapping at the next is
// worse than one that is honestly wrong, because nothing on screen says
// which reading to believe.
func TestEvpnChurnSeriesIsTheSameNumberAtEveryBucketWidth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	// Seventeen genuine changes in the whole fixture: four re-advertisements
	// among the flap arithmetic above (evpnMACFlapper twice, evpnMACStable
	// once, evpnMACShared once on the first router), two re-advertisements
	// of evpnBucketMACFlap, five withdrawals above, the lone withdrawal of
	// evpnPfxLoneWithdraw, the zero-clock route's second observation,
	// evpnMACTwiceWithdrawn's three (two withdrawals and the return), and
	// the lagging-collector trio's withdrawal of evpnKeyLagFlapMAC.
	// Everything else is a session dump -- including the rows of the eleven
	// key-component pairs, which is the point of them, and
	// evpnMACTwiceWithdrawn's own first advertisement.
	const wantReadvertise = 8
	const wantWithdraws = 9
	// 39, not the 41 this held until 2026-09-20, and the two that left are
	// NOT a classification change: the panel now reports the best single
	// vantage point, so the two rows the second collector holds -- its copy
	// of evpnKeyCollectorMAC and its copy of evpnKeyLagFlapMAC, each the
	// first observation of that route in c2's own partition and so each a
	// dump -- are no longer added to c1's. 58 archived rows less those two
	// is 56, less the 17 changes above is 39. Derived that way rather than
	// read off the new output.
	//
	// The changes do not move, which is the check on that arithmetic: both
	// dropped rows are advertisements that are first-in-partition, so
	// neither was ever counted in wantReadvertise or wantWithdraws.
	const wantDumps = 39

	for _, width := range []string{"1 MINUTE", "5 MINUTE", "15 MINUTE", "1 HOUR", "1 DAY"} {
		var readvertise, withdraws, dumps int64
		for _, b := range evpnSeries(t, ctx, ch, ".*", ".*", width) {
			readvertise += b.readvertise
			withdraws += b.withdraw
			dumps += b.sessionDump
		}
		if readvertise != wantReadvertise {
			t.Errorf("at %s buckets the panel reports %d re-advertisements, "+
				"want %d. A number that depends on the bucket width is a "+
				"number that changes meaning when an operator zooms",
				width, readvertise, wantReadvertise)
		}
		if withdraws != wantWithdraws {
			t.Errorf("at %s buckets the panel reports %d withdrawals, want %d",
				width, withdraws, wantWithdraws)
		}
		// The dumps must not move either: a classification that drifted with
		// the width would show up here first if it happened to keep
		// `readvertise` intact.
		if dumps != wantDumps {
			t.Errorf("at %s buckets the panel reports %d session dumps, want "+
				"%d -- the fixture archives 58 EVPN rows for these routers, "+
				"56 of them the chosen collector's, and 17 of those are "+
				"changes", width, dumps, wantDumps)
		}
	}
}

// The asymmetry, tested rather than asserted: the withdrawal series was never
// broken and must not be "fixed".
//
// A BMP table dump advertises. It does not withdraw -- there is nothing to
// withdraw yet, that being the point of a dump -- so "the first observation
// of this route inside this session" identifies a collection artifact for an
// advertisement and identifies nothing at all for a withdrawal. The
// classification carries an is_withdraw = 0 guard for exactly that reason,
// and evpnPfxLoneWithdraw is the row that makes the guard load-bearing: it is
// withdrawn in evpnSession2 having never been advertised there, so a
// symmetric rule would call it a dump and silently delete a real withdrawal
// from the panel.
//
// route_evpn's lab archive holds no row of this shape today -- 0 of its 246
// withdrawals are first-in-session, against 12 of route_unicast's 1,619
// (both measured 2026-09-10). That is why the row is written here rather
// than relied on from the lab archive: the guard is structural, and a lab archive
// that happens not to exercise it is not permission to drop it.
func TestEvpnChurnSeriesLeavesWithdrawalsUncorrected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	lone := evpnBucketAt(105, 0)
	obs, sessions := evpnRawBucket(t, ctx, ch, lone, "1 MINUTE")
	if obs != 1 || sessions != 1 {
		t.Fatalf("the lone-withdrawal bucket holds %d observations from %d "+
			"sessions, want 1 from 1", obs, sessions)
	}

	got, present := evpnSeries(t, ctx, ch, ".*", ".*", "1 MINUTE")[lone.Unix()]
	if !present {
		t.Fatalf("the panel reports no bucket at %s; %s was withdrawn there "+
			"and a withdrawal is a change", lone, evpnPfxLoneWithdraw)
	}
	if got.withdraw != 1 {
		t.Errorf("withdraw = %d in the lone-withdrawal bucket, want 1. 0 "+
			"means the withdrawal series started subtracting first-in-session "+
			"rows, which a dump can never produce", got.withdraw)
	}
	if got.sessionDump != 0 {
		t.Errorf("session_dump = %d in the lone-withdrawal bucket, want 0 -- "+
			"%s is a withdrawal, and a table dump does not withdraw",
			got.sessionDump, evpnPfxLoneWithdraw)
	}
	if got.readvertise != 0 {
		t.Errorf("readvertise = %d in the lone-withdrawal bucket, want 0",
			got.readvertise)
	}
}

// The EVPN key is not the unicast key, and every component of it is load
// bearing.
//
// query/evpnroutes.go groups by the whole NLRI tuple -- collector, router,
// peer, rib, route_type, rd, prefix, mac, ip, ethernet_tag, esi, path_id --
// and says in its own doc comment that there is no shorter key to fall back
// on, because a type 2 has no prefix and a type 3 has neither prefix nor MAC
// nor IP. Copying route_unicast's (family, prefix, path_id) across, or
// reading the key off route_evpn's ORDER BY (which is the ReplacingMergeTree
// dedup key and carries the timestamps), gets a different and wrong tuple.
//
// Each bucket below holds two DIFFERENT routes separated by one component
// and by nothing else, each observed exactly once inside evpnSession: two
// dumps, no change. Drop the component from the classification and the two
// merge, at which point the second row is no longer its own route's first
// observation and is reported as churn that never happened. Naming the
// buckets per component means a failure says WHICH component was lost.
//
// All twelve components are covered, including the two the lab archive
// cannot exercise: it has one collector, and every EVPN row in it is in_pre
// from a single peer per router. Those two are in the fixture precisely
// because the lab archive would let them rot unnoticed.
func TestEvpnChurnSeriesKeepsRoutesApartByEveryNLRIComponent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	series := evpnSeries(t, ctx, ch, ".*", ".*", "1 MINUTE")
	for _, tc := range []struct {
		minute    int
		component string
		why       string
	}{
		{110, "ip", "a MAC-only type 2 and the same MAC advertised with an " +
			"IP are two routes; the lab archive's type-2 rows under one rd and " +
			"mac differ only in ip"},
		{111, "ethernet_tag", "a type 3 has neither prefix nor MAC nor IP, " +
			"so its Ethernet tag is most of what identifies it"},
		{112, "esi", "two rows differing only in esi are two routes for the " +
			"identical reason two differing only in path_id are"},
		{113, "path_id", "add-path makes one MAC two routes"},
		{114, "route_type", "types 1 and 3 populate none of mac, ip or " +
			"prefix, so route_type is the only column between them"},
		{115, "peer_ip", "one router's two BGP peers each carry their own " +
			"view of the same MAC"},
		{116, "rib", "the pre-policy and post-policy adj-RIB-in are two " +
			"streams and two routes"},
		{117, "rd", "one MAC in two MAC-VRFs is two routes"},
		{118, "prefix", "two type-5 IP-prefix routes under one RD"},
		{119, "mac", "two type-2 MAC advertisements under one RD"},
		{120, "router_ip", "a leaf and the route reflector above it are two " +
			"views and this dashboard exists to keep them apart"},
		// collector_id belonged in this table until 2026-09-20 and cannot
		// stay: every case here works by reading TWO dumps out of a bucket
		// holding one row per route, and this panel now reports the best
		// single vantage point, so a bucket holding one row per COLLECTOR
		// reports one dump however the route key is spelled. The property
		// still matters and is pinned in
		// TestEvpnChurnSeriesReportsOneCollectorsViewNotTheSumOfBoth, where
		// the losing collector observes first and dropping collector_id
		// therefore demotes the winner's own first observation.
	} {
		at := evpnBucketAt(tc.minute, 0)
		obs, sessions := evpnRawBucket(t, ctx, ch, at, "1 MINUTE")
		if obs != 2 || sessions != 1 {
			t.Fatalf("the %s bucket holds %d observations from %d sessions, "+
				"want 2 from 1 -- without that shape a lost component is "+
				"invisible here", tc.component, obs, sessions)
		}
		got, present := series[at.Unix()]
		if !present {
			t.Fatalf("the panel reports no bucket at %s (%s)", at, tc.component)
		}
		if got.sessionDump != 2 || got.readvertise != 0 || got.withdraw != 0 {
			t.Errorf("the %s bucket reports %d re-advertisements / %d "+
				"withdrawals / %d session dumps, want 0/0/2 -- both rows are "+
				"the first observation of their own route inside session %d. "+
				"1 dump and 1 re-advertisement means %s is missing from the "+
				"classification's route key, which merges two routes into "+
				"one: %s", tc.component, got.readvertise, got.withdraw,
				got.sessionDump, evpnSession, tc.component, tc.why)
		}
	}
}

// The session is ordered on the collector's sequence, not on the router's
// clock -- and this is the only test here that can tell.
//
// ts_router comes from the BMP per-peer header and is not trustworthy: a
// router with a zero clock reports 1970, which this decoder has a flag for.
// Ordering the classification on it would hand "the first observation of this
// route in this session" to whichever row the router mis-stamped, and the
// fixture-wide totals do not move when that happens -- both rows are
// advertisements either way, so exactly one of them is the dump under both
// rules. Only WHICH minute holds it changes, which is why the counting tests
// above cannot see this and this one asserts per bucket.
//
// The lab archive cannot see it either, measured read-only on 2026-09-10: no
// row of route_evpn, route_vpn or route_unicast carries a zero ts_router, and
// ordering route_evpn's 904 rows by (ts_router, stream_seq) classifies every
// one of them exactly as (seq, stream_seq) does. evpnZeroClockMAC is written
// here for that reason rather than in spite of it.
func TestEvpnChurnSeriesOrdersTheSessionOnTheCollectorsSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	first, second := evpnBucketAt(122, 0), evpnBucketAt(123, 0)
	for _, at := range []time.Time{first, second} {
		if obs, sessions := evpnRawBucket(t, ctx, ch, at, "1 MINUTE"); obs != 1 || sessions != 1 {
			t.Fatalf("the zero-clock bucket at %s holds %d observations from "+
				"%d sessions, want 1 from 1", at, obs, sessions)
		}
	}

	series := evpnSeries(t, ctx, ch, ".*", ".*", "1 MINUTE")
	early, ok := series[first.Unix()]
	if !ok {
		t.Fatalf("the panel reports no bucket at %s", first)
	}
	late, ok := series[second.Unix()]
	if !ok {
		t.Fatalf("the panel reports no bucket at %s", second)
	}
	if early.sessionDump != 1 || early.readvertise != 0 {
		t.Errorf("minute 122 reports %d re-advertisements / %d session dumps, "+
			"want 0/1 -- it is %s's FIRST observation in session %d by the "+
			"collector's own (seq, stream_seq). 1/0 means the classification "+
			"is ordered on ts_router, and the router stamped its second "+
			"observation 1970", early.readvertise, early.sessionDump,
			evpnZeroClockMAC, evpnSession)
	}
	if late.sessionDump != 0 || late.readvertise != 1 {
		t.Errorf("minute 123 reports %d re-advertisements / %d session dumps, "+
			"want 1/0 -- that row carries a zero router clock and is still "+
			"the second thing said about %s in session %d",
			late.readvertise, late.sessionDump, evpnZeroClockMAC, evpnSession)
	}
}

// Every row is classified into exactly one of the three series, so the three
// must add back up to the observations the bucket holds -- WITHIN THE
// COLLECTOR THE PANEL CHOSE.
//
// That qualifier costs the invariant nothing. What this guards is that the
// classification is TOTAL: that no row falls between dump, re-advertisement
// and withdrawal. Totality holds of one collector's view exactly as much as
// of the sum, and this test never asserted that summing observers is the
// intended answer -- that is a separate question, settled by choosing a
// per-collector view over a summed one.
//
// This is the check that catches a classification which DROPS rows rather
// than one which mislabels them. Both failure modes read the same on screen
// -- a line lower than it should be -- and the counts above would only catch
// the second, because they assert about buckets the fixture arranged. This
// one asserts about every bucket there is, including the ones the flap
// arithmetic above happens to own.
func TestEvpnChurnSeriesAccountsForEveryObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	// The premise the invariant is now stated against, asserted rather than
	// assumed: c1 holds 53 rows under the first router and all 3 under the
	// second (evpnMACShared's advertisement and withdrawal, and
	// evpnKeyRouterMAC), c2 holds 2, so c1 is the best vantage point for
	// BOTH routers and the panel reports c1 whole. 53 + 3 + 2 is the
	// fixture's 58.
	//
	// If a later fixture row ever makes c2 the winner under either router,
	// this fails here -- loudly, and naming the number that moved -- rather
	// than letting the invariant below quietly compare the panel against
	// rows it deliberately no longer reports.
	for _, want := range []struct {
		router    string
		collector string
		rows      int64
	}{
		{evpnRouter, "c1", 53},
		{evpnRouter, evpnCollectorB, 2},
		{evpnRouterB, "c1", 3},
	} {
		var got int64
		sql := fmt.Sprintf(`SELECT toInt64(count()) FROM vantage.route_evpn FINAL
			WHERE router_ip = toIPv6('%s') AND collector_id = '%s'`,
			want.router, want.collector)
		if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&got); err != nil {
			t.Fatalf("count %s/%s: %v", want.router, want.collector, err)
		}
		if got != want.rows {
			t.Fatalf("router %s collector %s holds %d rows, want %d -- the "+
				"invariant below is stated against the chosen collector's "+
				"rows, and which collector is chosen depends on this count",
				want.router, want.collector, got, want.rows)
		}
	}

	series := evpnSeries(t, ctx, ch, ".*", ".*", "1 MINUTE")
	if len(series) == 0 {
		t.Fatal("the panel returned no buckets at all")
	}
	var total int64
	for start, b := range series {
		at := time.Unix(start, 0).UTC()
		// WITHIN THE CHOSEN COLLECTOR'S ROWS, every row is classified, and
		// that costs the invariant nothing: this test has only ever guarded
		// that the classification is TOTAL -- that no row falls between
		// dump, re-advertisement and withdrawal -- and totality is a
		// property of one collector's view exactly as much as of the sum.
		// It never asserted that summing observers is the intended answer,
		// which is a separate question, settled by choosing a per-collector
		// view over a summed one.
		obs := evpnRawBucketForCollector(t, ctx, ch, at, "1 MINUTE", "c1")
		if b.readvertise < 0 || b.withdraw < 0 || b.sessionDump < 0 {
			t.Errorf("bucket %s reports %d/%d/%d; a classification counts "+
				"rows and cannot go below zero, while every candidate that "+
				"SUBTRACTS can", at, b.readvertise, b.withdraw, b.sessionDump)
		}
		if sum := b.readvertise + b.withdraw + b.sessionDump; sum != obs {
			t.Errorf("bucket %s: readvertise %d + withdraw %d + session_dump "+
				"%d = %d, but the chosen collector holds %d rows there. "+
				"Every row is either a session dump, a re-advertisement or "+
				"a withdrawal, and nothing may fall between them", at,
				b.readvertise, b.withdraw, b.sessionDump, sum, obs)
		}
		total += obs
	}
	// 56, the chosen collector's rows, out of 58 archived.
	//
	// 55 rather than 56 archived until evpnMACTwiceWithdrawn added four --
	// one advertisement, two withdrawals and the return -- to falsify "Flap
	// cycles"' nxt_w = 0, which a surviving mutation showed nothing could
	// see; and one row is written twice with the same stream sequence, which
	// FINAL collapses. Without FINAL the archive reads 59 and the
	// re-advertisement it duplicates is counted twice.
	//
	// 58 archived since evpnKeyLagFlapMAC added three for the
	// cross-collector flap defect: collector A's advertisement is that key's
	// first observation in its own (collector, session) partition and so a
	// DUMP, its withdrawal is a withdrawal, and collector B's advertisement
	// is the first in B's partition and so a second DUMP.
	//
	// 56 REPORTED since 2026-09-20, and the two that left are the only two
	// rows c2 holds -- its copies of evpnKeyCollectorMAC and
	// evpnKeyLagFlapMAC. c1 is the best vantage point under both routers, so
	// the panel reports c1's 56 and never adds c2's. Derived from the
	// per-collector counts asserted above rather than read off the output.
	if total != 56 {
		t.Errorf("the chosen collector accounts for %d EVPN rows across all "+
			"buckets, want 56 of the fixture's 58", total)
	}
}

const (
	churnRouter = "10.0.199.13"
	churnPeer   = "10.255.199.17"
	// churnPeer2 is the second BGP peer of the SAME router, and it is the
	// reason this fixture can tell a prefix from a route at all. Until
	// 2026-09-10 every row here came from one router, one peer and one rib,
	// so "GROUP BY prefix" and "GROUP BY the route identity" were the same
	// query and the ranking panel's merging defect was undetectable by
	// construction. A fixture that cannot distinguish two answers cannot
	// defend the one that ships.
	churnPeer2    = "10.255.199.18"
	churnSession1 = 9951
	churnSession2 = 9952
	churnSession3 = 9953

	// churnPfxResend and churnPfxRedump are the heart of this fixture: they
	// have the SAME number of observations and completely different
	// stability. One was re-advertised three times inside a single session --
	// something actually changed about it, three times. The other was seen
	// once in each of three sessions, which is XRd resetting on its
	// nine-minute metronome and re-dumping a table that never changed.
	//
	// count() cannot tell them apart. On the lab archive nothing can: 159 of
	// 167 unicast prefixes have count() exactly equal to their session count,
	// so the shipped "Most-changed prefixes" panel ranked the four most
	// STABLE prefixes in the lab archive at the top and called them most-changed.
	// The addresses are chosen so that ALPHABETICAL order contradicts correct
	// order. Both prefixes have three observations and no withdrawals, so a
	// ranking on the observation count ties and falls through to the prefix
	// -- and with the resent one sorting first, the wrong ordering would have
	// produced the right answer by accident and the ranking test passed on a
	// panel that had reverted to counting.
	churnPfxResend = "10.204.2.0/24"
	churnPfxRedump = "10.204.1.0/24"
	// churnPfxWithdrawn is advertised and withdrawn inside one session.
	churnPfxWithdrawn = "10.204.3.0/24"
	// churnPfxOnce is seen exactly once: the floor case, where resends must
	// be 0 rather than negative.
	churnPfxOnce = "10.204.4.0/24"
	// churnPfxLocRib is the only loc_rib route here, which is the only
	// reason $rib can be caught unread.
	churnPfxLocRib = "10.204.5.0/24"

	// churnPfxBucketFlap and churnPfxBucketRedump exist for the TIME SERIES
	// panels, which the two prefixes above cannot judge. Those two are far
	// enough apart in time to land in separate buckets at every width, so a
	// bucketed query can tell them apart by looking only at which bucket a
	// row fell in -- which is exactly the ability the shipped panels did not
	// have and the ability a fix must not be credited with by accident.
	//
	// These two put both shapes in ONE bucket, two observations each:
	// churnPfxBucketFlap is re-advertised twice inside churnSession1 (two
	// real changes), churnPfxBucketRedump is seen once in churnSession2 and
	// once in churnSession3 (two session dumps, no change at all). The
	// bucket therefore holds four observations of which exactly two are
	// changes, and the three candidate formulas all return different
	// numbers for it:
	//
	//	count()                        = 4  -- the shipped panels
	//	count() - uniqExact(session_id) = 1  -- the session-corrected candidate
	//	per-row classification         = 2  -- the truth, and what ships
	//
	// The middle one is the reason the "Most-changed prefixes" formula was
	// not simply copied up into a time series: it subtracts the sessions
	// VISIBLE IN THE BUCKET, so three sessions crossing one bucket cancel
	// three observations whatever those observations were.
	churnPfxBucketFlap   = "10.204.6.0/24"
	churnPfxBucketRedump = "10.204.7.0/24"
	// churnPfxLoneWithdraw is withdrawn without ever having been advertised
	// inside the session that withdraws it -- the first thing said about it
	// in churnSession2 is that it is gone.
	//
	// It is the whole asymmetry argument in one row. A table dump advertises;
	// it never withdraws, so "first observation of this route in this
	// session" means "the dump" for an advertisement and means nothing of
	// the sort for a withdrawal. A classification that dropped the
	// is_withdraw = 0 guard would call this row a collection artifact and
	// delete a real withdrawal from both time series. The lab archive holds
	// 12 rows of this shape among its 1,619 withdrawals, so it is rare
	// enough to be missed by inspection and common enough to matter.
	churnPfxLoneWithdraw = "10.204.8.0/24"

	// The five below exist for the RANKING panel, and they are what a
	// single-peer fixture could not express.
	//
	// "Most-changed prefixes" ranks prefixes, but a prefix is not a route.
	// The identity of a unicast route is the one query/rib.go:781 groups by
	// -- collector, router, peer, rib, family, prefix, path_id -- so one
	// prefix carried by two peers is two routes, and the first observation
	// of EACH inside a session is that session's dump. Grouping the raw rows
	// by prefix and subtracting uniqExact(session_id) sees one session and
	// subtracts one, so it scores 1 change where the truth is 0.
	//
	// Each of the four below isolates ONE component of that key, so a
	// classification that dropped a component fails on the prefix named
	// after it rather than on a single conflated assertion. All four are two
	// observations inside churnSession1 and zero changes:
	//
	//	churnPfxTwoPeers     peer_ip   -- two peers of one router
	//	churnPfxTwoRibs      rib       -- in_pre and in_post of one peer
	//	churnPfxTwoFamilies  family    -- ipv4u and the deferred x1-2
	//	churnPfxTwoPaths     path_id   -- add-path 0 and 7
	//
	// The lab archive is not hypothetical about this: 489 of its 541 prefixes
	// carry more than one route, and 10.99.1.0/24 and 10.99.2.0/24 sit under
	// in_pre, in_post AND loc_rib simultaneously.
	churnPfxTwoPeers    = "10.204.9.0/24"
	churnPfxTwoRibs     = "10.204.10.0/24"
	churnPfxTwoFamilies = "10.204.11.0/24"
	churnPfxTwoPaths    = "10.204.12.0/24"
	// churnPfxBothPeersChange is the other half of the same decision. The
	// four above prove that routes must not be MERGED; this one proves the
	// per-route answers must then be SUMMED back up to the prefix, which is
	// what keeps the panel a ranking of prefixes.
	//
	// Both peers of the router carry it inside churnSession1, and both
	// genuinely re-advertise it: peer 1 three times (one dump, two changes),
	// peer 2 twice (one dump, one change). Five observations, one session,
	// two routes, three changes. The shipped formula reported 5 - 1 = 4, and
	// a fix that classified per route but then reported max() or any()
	// instead of the sum would report 2.
	churnPfxBothPeersChange = "10.204.13.0/24"
)

// churnBucketBase is the start of the minute two hours before
// corpusCollectorClock.
//
// The time-series panels bucket with toStartOfInterval, whose boundaries are
// absolute rather than relative to the fixture, so rows stamped at an
// arbitrary offset land in a bucket nobody can name: two observations twenty
// seconds apart share a bucket or straddle one depending on the second
// corpusCollectorClock happened to be read at, and a test asserting "these
// two are in the same bucket" would pass roughly two thirds of the time.
// Truncating to the minute first makes the bucket a fact about the fixture.
func churnBucketBase() time.Time {
	return corpusCollectorClock.Add(-2 * time.Hour).Truncate(time.Minute)
}

// churnBucketAt stamps a row inside minute bucket `minute` of
// churnBucketBase, `sec` seconds in. Callers keep sec below 60; the point of
// the helper is that the caller names the bucket rather than computing it.
func churnBucketAt(minute, sec int) time.Time {
	return churnBucketBase().Add(
		time.Duration(minute)*time.Minute + time.Duration(sec)*time.Second)
}

// insertRouteChurnFixture writes the unicast routes the churn dashboard's
// arithmetic is specified against. route-churn shipped with no test of its
// own -- only the all-targets walk, which proves its SQL runs and says
// nothing about whether the numbers mean anything.
func insertRouteChurnFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	v4 := &vantagev1.Family{Afi: 1, Safi: 1}
	env := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: churnRouter, SysName: "dash-churn1"},
			Peer:        &vantagev1.PeerId{Ip: churnPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	attrs := &vantagev1.PathAttributes{
		Origin: 0, NextHop: "10.204.0.1",
		AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000}}},
	}
	locRib := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(session, ts, r)
		e.Peer = &vantagev1.PeerId{
			Ip: churnPeer, Asn: 65000,
			Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
		}
		return e
	}
	// peer2 is the same router's other peer. Only the address changes:
	// rows.go keys peer_ip off PeerId.ip whatever the peer type, so this is
	// a second route for any prefix both peers carry, and nothing else.
	peer2 := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(session, ts, r)
		e.Peer = &vantagev1.PeerId{Ip: churnPeer2, Asn: 65001}
		return e
	}
	// inPost is the SAME peer's post-policy adj-RIB-in (RFC 8671's L flag),
	// which rows.go files as rib = in_post. Same peer, same session,
	// different RIB stream, and therefore a different route.
	inPost := func(session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(session, ts, r)
		e.Peer = &vantagev1.PeerId{Ip: churnPeer, Asn: 65000, PostPolicy: true}
		return e
	}
	adv := func(prefix string) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{Family: v4, Attrs: attrs,
			Announced: []*vantagev1.Prefix{{Prefix: prefix}}}
	}
	// advDeferred announces under AFI 1 / SAFI 2, which is not in
	// rows.go's family registry and so lands in route_unicast as the
	// deferred token "x1-2". That is the case family exists in the route key
	// for: two families can carry the same prefix TEXT, and merging their
	// observation streams calls one a re-advertisement of the other.
	advDeferred := func(prefix string) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 2}, Attrs: attrs,
			Announced: []*vantagev1.Prefix{{Prefix: prefix}}}
	}
	// advPath announces one add-path path_id of a prefix. Two path_ids of
	// one prefix from one peer are two routes -- that being the entire point
	// of add-path -- so a key without path_id merges them.
	advPath := func(prefix string, pathID uint32) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{Family: v4, Attrs: attrs,
			Announced: []*vantagev1.Prefix{{Prefix: prefix, PathId: pathID}}}
	}
	now := corpusCollectorClock
	at := func(d time.Duration) time.Time { return now.Add(-2*time.Hour + d) }

	envs := []*vantagev1.Envelope{
		// Three observations, ONE session: genuinely re-advertised twice.
		env(churnSession1, at(0), adv(churnPfxResend)),
		env(churnSession1, at(time.Minute), adv(churnPfxResend)),
		env(churnSession1, at(2*time.Minute), adv(churnPfxResend)),

		// Three observations, THREE sessions: re-dumped, never changed.
		env(churnSession1, at(10*time.Second), adv(churnPfxRedump)),
		env(churnSession2, at(30*time.Minute), adv(churnPfxRedump)),
		env(churnSession3, at(60*time.Minute), adv(churnPfxRedump)),

		// Advertised then withdrawn, one session.
		env(churnSession1, at(20*time.Second), adv(churnPfxWithdrawn)),
		env(churnSession1, at(90*time.Second), &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs,
			Withdrawn: []*vantagev1.Prefix{{Prefix: churnPfxWithdrawn}},
		}),

		// Seen once, ever.
		env(churnSession1, at(30*time.Second), adv(churnPfxOnce)),

		// The only route outside in_pre.
		locRib(churnSession1, at(40*time.Second), adv(churnPfxLocRib)),

		// An end-of-RIB marker carries no prefix. Filed into route_unicast
		// it becomes a row in the ranking whose prefix is the empty string,
		// which reads as a nameless route that changed; filed into
		// eor_events it cannot reach the ranking at all.
		env(churnSession1, at(50*time.Second), &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs, EndOfRib: true,
		}),

		// Everything below lives at minutes 99-105 of churnBucketBase, clear
		// of the rows above (which occupy minutes 0-60), so the buckets these
		// tests name hold these rows and nothing else.

		// The mixed bucket, half of it. Dumped in minute 99, then genuinely
		// re-advertised TWICE inside minute 100, all in one session.
		env(churnSession1, churnBucketAt(99, 0), adv(churnPfxBucketFlap)),
		env(churnSession1, churnBucketAt(100, 5), adv(churnPfxBucketFlap)),
		env(churnSession1, churnBucketAt(100, 25), adv(churnPfxBucketFlap)),

		// The other half. Two observations in the SAME minute-100 bucket,
		// one per session, neither of them a change.
		env(churnSession2, churnBucketAt(100, 35), adv(churnPfxBucketRedump)),
		env(churnSession3, churnBucketAt(100, 45), adv(churnPfxBucketRedump)),

		// A withdrawal that is the first word about its route in its
		// session, alone in minute 105.
		env(churnSession2, churnBucketAt(105, 0), &vantagev1.RouteEvent{
			Family: v4, Attrs: attrs,
			Withdrawn: []*vantagev1.Prefix{{Prefix: churnPfxLoneWithdraw}},
		}),

		// Minutes 110-114: one prefix, more than one ROUTE. Every row below
		// is the first observation of its own route inside churnSession1 --
		// a table dump -- except the three marked as changes. Grouping the
		// raw rows by prefix and subtracting uniqExact(session_id) sees one
		// session per prefix and subtracts exactly one, so it reports a
		// change for each of the four pairs and one change too many for the
		// fifth.
		env(churnSession1, churnBucketAt(110, 0), adv(churnPfxTwoPeers)),
		peer2(churnSession1, churnBucketAt(110, 10), adv(churnPfxTwoPeers)),

		env(churnSession1, churnBucketAt(111, 0), adv(churnPfxTwoRibs)),
		inPost(churnSession1, churnBucketAt(111, 10), adv(churnPfxTwoRibs)),

		env(churnSession1, churnBucketAt(112, 0), adv(churnPfxTwoFamilies)),
		env(churnSession1, churnBucketAt(112, 10), advDeferred(churnPfxTwoFamilies)),

		env(churnSession1, churnBucketAt(113, 0), advPath(churnPfxTwoPaths, 0)),
		env(churnSession1, churnBucketAt(113, 10), advPath(churnPfxTwoPaths, 7)),

		// Two routes that BOTH change, so the per-route answers have to be
		// summed rather than merged or maxed: 1 dump + 2 changes from peer
		// 1, 1 dump + 1 change from peer 2.
		env(churnSession1, churnBucketAt(114, 0), adv(churnPfxBothPeersChange)),
		env(churnSession1, churnBucketAt(114, 10), adv(churnPfxBothPeersChange)),
		env(churnSession1, churnBucketAt(114, 20), adv(churnPfxBothPeersChange)),
		peer2(churnSession1, churnBucketAt(114, 30), adv(churnPfxBothPeersChange)),
		peer2(churnSession1, churnBucketAt(114, 40), adv(churnPfxBothPeersChange)),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert route-churn fixture envelope %d: %v", i, err)
		}
	}

	// One at-least-once re-delivery: the SAME message with the SAME stream
	// sequence, written a second time. Both panels read the table FINAL, and
	// this is what makes that a requirement these tests can see.
	//
	// A duplicate of a genuine re-advertisement is where it does damage. The
	// classification asks whether a row is its route's minimum (seq,
	// stream_seq) inside its session; two copies of one re-advertisement
	// share that pair, so neither is the minimum and BOTH count as changes.
	// The 2026-09-10 rig measured exactly that: a lab archive whose
	// observations were each delivered twice reported 198 changes where the
	// truth was 99. ReplacingMergeTree collapses the pair on the ORDER BY
	// key, so with FINAL the arithmetic below is unchanged and without it
	// churnPfxResend gains an observation and a change.
	//
	// It is deliberate and not redundant, though it looks it. chtest drops
	// and recreates testDB once per test BINARY while every test here
	// re-inserts this whole fixture, so the table already holds duplicates
	// of every row -- FINAL is load-bearing today by accident of that
	// lifecycle rather than by design. Measured: with the panel's FINAL
	// removed, `go test -run TestRouteChurn` fails without this row, and
	// `go test -run TestRouteChurnSeparatesResendsFromSessionRedumps` PASSES
	// without it and fails with it. A guarantee that depends on how many
	// sibling tests ran first is not a guarantee.
	const redelivered = 1
	if got := envs[redelivered].GetRoute().GetAnnounced()[0].GetPrefix(); got != churnPfxResend {
		t.Fatalf("envelope %d announces %s, want %s -- the re-delivery has to "+
			"duplicate a genuine re-advertisement, because a duplicated "+
			"session dump is still a session dump and would prove nothing",
			redelivered, got, churnPfxResend)
	}
	if err := ch.Insert(ctx, mustRowsFor(t, envs[redelivered], redelivered+1)); err != nil {
		t.Fatalf("insert route-churn fixture re-delivery: %v", err)
	}
}

// churnRow is one row of the committed "Most-changed prefixes" table.
//
// Every column is Int64 here, and that is a property of the TEST rather than
// of the shipped expression. The panel now ranks on countIf(NOT
// is_session_dump), a UInt64; every candidate it was chosen over --
// count() - uniqExact(session_id) above all -- is an Int64 subtraction, and
// clickhouse-go will not scan an Int64 into a *uint64 nor a UInt64 into an
// *int64 nor either into an *any. A struct committed to the shipped width
// would therefore kill each of those mutations on "converting Int64 to
// *uint64 is unsupported" before comparing a single number, which shows only
// that the mutation is differently typed and not that it is wrong.
// churnPrefixesInOrder widens every column through churnCountsAsInt64 so the
// mutation reaches the assertion, and so that a subtraction that has gone
// negative stays visible as a negative rather than wrapping to eighteen
// quintillion.
type churnRow struct {
	prefix string
	// changes is the ranking column: observations that were not the first
	// sighting of their route inside their BMP session, plus every
	// withdrawal. It is a count, so unlike the subtraction it replaced it
	// cannot go negative by construction -- which is why the never-negative
	// assertion is kept: it is now a statement about the mutations, not
	// about the panel.
	changes      int64
	observations int64
	// routes is how many distinct routes -- collector, router, peer, rib,
	// family, path_id -- carry this prefix. It is what lets an operator see
	// that a prefix is busy because six peers carry it rather than because
	// anything moved, and it is the column that makes aggregating a
	// per-route classification up to a prefix honest on screen.
	routes    int64
	sessions  int64
	withdraws int64
}

// churnPrefixes keys the ranking by prefix. Reach for churnPrefixesInOrder
// where the assertion is about the ORDER, which is what a panel called
// "Most-changed prefixes" is really promising.
func churnPrefixes(t *testing.T, ctx context.Context, ch *ClickHouse, rib string) map[string]churnRow {
	t.Helper()
	out := map[string]churnRow{}
	for _, r := range churnPrefixesInOrder(t, ctx, ch, rib) {
		out[r.prefix] = r
	}
	return out
}

func churnPrefixesInOrder(t *testing.T, ctx context.Context, ch *ClickHouse, rib string) []churnRow {
	t.Helper()
	raw := panelSQL(t, "route-churn", "Most-changed prefixes", "A")
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(raw, anchor); n != 1 {
		t.Fatalf("the most-changed-prefixes query carries %d occurrences of "+
			"%q, want 1 -- this test scopes to the fixture's router by "+
			"extending that filter", n, anchor)
	}
	raw = strings.Replace(raw, anchor,
		anchor+" AND router_ip = toIPv6('"+churnRouter+"')", 1)
	// The wrapper widens every count to Int64 without touching what ships;
	// see churnRow for why that is load-bearing for the mutations. It is a
	// projection over a subquery that already carries ORDER BY ... LIMIT, so
	// the ranking order survives it -- ClickHouse collapses to a single
	// stream at the LIMIT and the outer expression runs over that stream.
	sql := churnCountsAsInt64(
		substituteGrafana(raw, map[string]string{"rib": rib}),
		[]string{"prefix"},
		"changes", "observations", "routes", "sessions", "withdraws")
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("most-changed prefixes: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	var out []churnRow
	for rows.Next() {
		var r churnRow
		if err := rows.Scan(&r.prefix, &r.changes, &r.observations,
			&r.routes, &r.sessions, &r.withdraws); err != nil {
			t.Fatalf("scan churn row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("churn rows: %v", err)
	}
	return out
}

// The panel is called "Most-changed prefixes" and it ranked the four most
// STABLE prefixes in the lab archive at the top, because every router here
// re-dumps its whole table on each BMP session and count() cannot tell a
// re-dump from a change. Two prefixes with the SAME observation count and
// opposite stability is the shape that makes the distinction decidable.
func TestRouteChurnSeparatesResendsFromSessionRedumps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	rows := churnPrefixes(t, ctx, ch, ".*")

	resend, ok := rows[churnPfxResend]
	if !ok {
		t.Fatalf("%s absent from the ranking", churnPfxResend)
	}
	redump, ok := rows[churnPfxRedump]
	if !ok {
		t.Fatalf("%s absent from the ranking", churnPfxRedump)
	}

	// The premise: identical observation counts.
	if resend.observations != redump.observations {
		t.Fatalf("the two prefixes have %d and %d observations; they must be "+
			"equal or this test proves nothing about what separates them",
			resend.observations, redump.observations)
	}
	if resend.observations != 3 {
		t.Errorf("%s observations = %d, want 3", churnPfxResend, resend.observations)
	}

	// And opposite stability.
	if resend.sessions != 1 || redump.sessions != 3 {
		t.Errorf("sessions: %s = %d (want 1), %s = %d (want 3)",
			churnPfxResend, resend.sessions, churnPfxRedump, redump.sessions)
	}
	if resend.changes != 2 {
		t.Errorf("%s changes = %d, want 2 -- three advertisements inside one "+
			"session is one dump and two re-advertisements", churnPfxResend,
			resend.changes)
	}
	if redump.changes != 0 {
		t.Errorf("%s changes = %d, want 0 -- it was seen once per session, "+
			"which is the router re-dumping its table, not the route "+
			"changing", churnPfxRedump, redump.changes)
	}
	// Both are one route, so neither can be blamed on the merging defect.
	if resend.routes != 1 || redump.routes != 1 {
		t.Errorf("routes: %s = %d, %s = %d, want 1 each -- both live on one "+
			"router, one peer and one rib, so this pair says nothing about "+
			"grouping and everything about classification", churnPfxResend,
			resend.routes, churnPfxRedump, redump.routes)
	}

	// The ranking must put the one that actually changed first.
	if resend.changes <= redump.changes {
		t.Errorf("a re-dumped prefix ranks at or above a genuinely "+
			"re-advertised one (%d vs %d); that is how the shipped panel "+
			"presented the lab archive's four most stable prefixes as its "+
			"most-changed", redump.changes, resend.changes)
	}
}

// The floor cases: a prefix seen once has nothing to report, and a
// withdrawal is a change that must be counted as one.
func TestRouteChurnCountsWithdrawalsAndNeverGoesNegative(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	rows := churnPrefixes(t, ctx, ch, ".*")

	once, ok := rows[churnPfxOnce]
	if !ok {
		t.Fatalf("%s absent; a prefix seen once still has a row", churnPfxOnce)
	}
	if once.observations != 1 || once.sessions != 1 || once.changes != 0 ||
		once.routes != 1 {
		t.Errorf("%s = %d observations / %d sessions / %d changes / %d "+
			"routes, want 1/1/0/1", churnPfxOnce, once.observations,
			once.sessions, once.changes, once.routes)
	}

	w, ok := rows[churnPfxWithdrawn]
	if !ok {
		t.Fatalf("%s absent", churnPfxWithdrawn)
	}
	if w.withdraws != 1 {
		t.Errorf("%s withdraws = %d, want 1", churnPfxWithdrawn, w.withdraws)
	}
	// Advertised and withdrawn in one session: two observations, one session,
	// so the withdrawal itself is the resend.
	if w.changes != 1 {
		t.Errorf("%s changes = %d, want 1 -- the withdrawal is a second "+
			"observation inside the same session, and it is a real change",
			churnPfxWithdrawn, w.changes)
	}

	// The shipped column counts and so cannot go negative; every candidate
	// it replaced SUBTRACTS and can. The check is kept for them.
	for prefix, r := range rows {
		if r.changes < 0 {
			t.Errorf("%s changes = %d; a count cannot be negative, and a "+
				"prefix cannot be observed fewer times than the number of "+
				"sessions carrying it", prefix, r.changes)
		}
		if r.changes > r.observations {
			t.Errorf("%s reports %d changes from %d observations; a change "+
				"is an observation, so it cannot exceed them",
				prefix, r.changes, r.observations)
		}
		if r.sessions > r.observations {
			t.Errorf("%s reports %d sessions from %d observations, which "+
				"cannot happen and would make resends negative",
				prefix, r.sessions, r.observations)
		}
	}
}

// churnRawPrefix counts one prefix's rows on the fixture's router with no
// correction at all: the observations, the BMP sessions carrying them, and
// the number of distinct ROUTES those rows belong to.
//
// It exists so the tests below assert the fixture's premise rather than
// assuming it. "Two routes inside one session" is the only shape that makes
// the grouping question decidable, and a fixture that quietly stopped
// producing it would leave every assertion below passing against nothing --
// which is exactly the state this fixture was in until 2026-09-10, when one
// router, one peer and one rib made "GROUP BY prefix" and "GROUP BY route"
// the same query.
func churnRawPrefix(t *testing.T, ctx context.Context, ch *ClickHouse, prefix string) (observations, sessions, routes int64) {
	t.Helper()
	sql := fmt.Sprintf(`
		SELECT toInt64(count()),
		       toInt64(uniqExact(session_id)),
		       toInt64(uniqExact((collector_id, router_ip, peer_ip, rib, family, path_id)))
		FROM vantage.route_unicast FINAL
		WHERE router_ip = toIPv6('%s') AND prefix = '%s'`, churnRouter, prefix)
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).
		Scan(&observations, &sessions, &routes); err != nil {
		t.Fatalf("raw rows for %s: %v", prefix, err)
	}
	return observations, sessions, routes
}

// The merging defect, isolated one component of the route key at a time.
//
// The panel ranks PREFIXES, but the thing a router can re-advertise is a
// ROUTE, and one prefix is routinely several of them: 489 of the lab
// archive's 541 prefixes carry more than one, and 10.99.1.0/24 sits under
// in_pre, in_post and loc_rib at once. Until 2026-09-10 the panel grouped the
// raw rows by prefix and subtracted uniqExact(session_id), so a prefix dumped
// once by each of two peers inside ONE BMP session produced two dump rows,
// had exactly one session subtracted, and was reported as having changed
// once. Measured against the per-row rule on the lab archive, 486 of 541
// prefixes disagreed and the panel reported 6,206 changes against 4,728 --
// 31% over.
//
// Each of the four prefixes below differs on exactly one component of the
// route key, so a classification that lost a component fails on the prefix
// named for it instead of on one conflated assertion that says only "a
// number is wrong".
func TestRouteChurnKeepsTheRoutesOfOnePrefixApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	rows := churnPrefixes(t, ctx, ch, ".*")

	for _, tc := range []struct{ prefix, key string }{
		{churnPfxTwoPeers, "peer_ip"},
		{churnPfxTwoRibs, "rib"},
		{churnPfxTwoFamilies, "family"},
		{churnPfxTwoPaths, "path_id"},
	} {
		obs, sessions, routes := churnRawPrefix(t, ctx, ch, tc.prefix)
		if obs != 2 || sessions != 1 || routes != 2 {
			t.Fatalf("%s holds %d observations from %d sessions on %d "+
				"routes, want 2 from 1 on 2 -- without that shape this case "+
				"proves nothing, because grouping by prefix and grouping by "+
				"route would not disagree about it", tc.prefix, obs,
				sessions, routes)
		}
		got, present := rows[tc.prefix]
		if !present {
			t.Errorf("%s is absent from the ranking", tc.prefix)
			continue
		}
		if got.routes != 2 {
			t.Errorf("%s routes = %d, want 2 -- the column an operator reads "+
				"to see that a prefix is carried more than once says it is "+
				"carried once, so the two routes were merged before it was "+
				"computed", tc.prefix, got.routes)
		}
		if got.changes != 0 {
			t.Errorf("%s changes = %d, want 0. Its two rows are the first "+
				"sighting of two DIFFERENT routes inside %d, and a session's "+
				"first sighting of a route is that session's table dump, not "+
				"a change. 1 is what count() - uniqExact(session_id) reports "+
				"-- two observations, one session -- and 1 is also what a "+
				"classification whose route key has lost %s reports",
				tc.prefix, got.changes, churnSession1, tc.key)
		}
	}

	// The other half of the same decision. Classifying per route is only
	// half an answer for a panel that ranks prefixes: the per-route answers
	// then have to be ADDED UP, or a prefix that two peers are both flapping
	// reads as calmer than one peer flapping it alone.
	obs, sessions, routes := churnRawPrefix(t, ctx, ch, churnPfxBothPeersChange)
	if obs != 5 || sessions != 1 || routes != 2 {
		t.Fatalf("%s holds %d observations from %d sessions on %d routes, "+
			"want 5 from 1 on 2", churnPfxBothPeersChange, obs, sessions, routes)
	}
	both, present := rows[churnPfxBothPeersChange]
	if !present {
		t.Fatalf("%s is absent from the ranking", churnPfxBothPeersChange)
	}
	if both.observations != 5 || both.routes != 2 || both.sessions != 1 {
		t.Errorf("%s = %d observations / %d routes / %d sessions, want 5/2/1",
			churnPfxBothPeersChange, both.observations, both.routes,
			both.sessions)
	}
	if both.changes != 3 {
		t.Errorf("%s changes = %d, want 3 -- %s re-advertised it twice and "+
			"%s once, and a ranking of prefixes has to add those up. 4 is "+
			"count() - uniqExact(session_id), which subtracts one session "+
			"from five observations; 2 is a fix that classified per route "+
			"and then reported one route's answer rather than the sum",
			churnPfxBothPeersChange, both.changes, churnPeer, churnPeer2)
	}
	// And it must outrank the prefix that changed twice on one route, or
	// summing was not what produced the 3.
	if resend := rows[churnPfxResend]; both.changes <= resend.changes {
		t.Errorf("%s (3 changes across two routes) does not outrank %s (%d "+
			"changes on one): %d vs %d", churnPfxBothPeersChange,
			churnPfxResend, resend.changes, both.changes, resend.changes)
	}
}

// The second defect the ranking had, and the one a subtraction cannot see at
// any grouping: a withdrawal that is the first thing said about its route
// inside its BMP session scored 0.
//
// A BMP table dump advertises. It never withdraws -- there is nothing to
// withdraw yet, that being the point of a dump -- so "first observation of
// this route in this session" identifies a collection artifact for an
// advertisement and identifies nothing at all for a withdrawal. Nothing in
// the collector synthesizes one either: a Peer Down becomes a peer_events
// row. Every withdrawal is therefore a real change, and churnPfxLoneWithdraw
// is the row that makes that decidable: it is withdrawn in churnSession2
// having never been advertised there, so it has exactly one observation from
// exactly one session, and count() - uniqExact(session_id) reports zero
// changes for a route the lab archive was told had gone away. The lab
// archive holds 12 rows of this shape among its 1,619 withdrawals.
func TestRouteChurnCountsAFirstInSessionWithdrawalAsAChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	obs, sessions, routes := churnRawPrefix(t, ctx, ch, churnPfxLoneWithdraw)
	if obs != 1 || sessions != 1 || routes != 1 {
		t.Fatalf("%s holds %d observations from %d sessions on %d routes, "+
			"want 1 from 1 on 1 -- the asymmetry is only visible on a "+
			"withdrawal that is alone in its session", churnPfxLoneWithdraw,
			obs, sessions, routes)
	}

	r, present := churnPrefixes(t, ctx, ch, ".*")[churnPfxLoneWithdraw]
	if !present {
		t.Fatalf("%s is absent from the ranking entirely; a route this "+
			"lab archive was told had gone away is precisely what an operator "+
			"scanning for instability is looking for", churnPfxLoneWithdraw)
	}
	if r.withdraws != 1 || r.observations != 1 {
		t.Errorf("%s = %d observations / %d withdraws, want 1/1",
			churnPfxLoneWithdraw, r.observations, r.withdraws)
	}
	if r.changes != 1 {
		t.Errorf("%s changes = %d, want 1. Its one row is a withdrawal in "+
			"%d that was never advertised there, so it is a real change and "+
			"the only one it has. 0 is what count() - uniqExact(session_id) "+
			"reports -- one observation minus one session -- and 0 is also "+
			"what a classification that dropped the is_withdraw = 0 guard "+
			"reports, and what subtracting the distinct (route, session) "+
			"pairs reports. All three call a withdrawal a table dump, and a "+
			"table dump does not withdraw", churnPfxLoneWithdraw, r.changes,
			churnSession2)
	}
	// The prefix advertised AND withdrawn inside one session pins the other
	// end of the same rule: there the withdrawal is a second observation, so
	// every candidate agrees it is a change and only this one disagrees.
	if w := churnPrefixes(t, ctx, ch, ".*")[churnPfxWithdrawn]; w.changes != 1 {
		t.Errorf("%s changes = %d, want 1", churnPfxWithdrawn, w.changes)
	}
}

// The ranking and the time series above it are two presentations of one
// classification, so over the same rows they must report the same number of
// changes.
//
// Both panels have assertions of their own about the buckets and prefixes the
// fixture arranged. This one asserts about every row there is, and it is what
// catches a ranking that classifies each row correctly and then loses or
// double-counts rows on the way up to the prefix -- a failure that reads on
// screen exactly like a quiet network. Thirteen prefixes cannot be truncated
// by a LIMIT of 50.
func TestRouteChurnTableAndSeriesAgreeOnWhatChanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	rows := churnPrefixesInOrder(t, ctx, ch, ".*")
	if len(rows) != 13 {
		t.Errorf("the ranking returned %d prefixes, want 13; a LIMIT that "+
			"truncated, or an end-of-RIB marker that reached route_unicast, "+
			"would both show up here", len(rows))
	}
	var changes, observations, withdraws int64
	for _, r := range rows {
		changes += r.changes
		observations += r.observations
		withdraws += r.withdraws
		if r.routes < 1 || r.routes > r.observations {
			t.Errorf("%s reports %d routes from %d observations; a prefix in "+
				"the table is carried by at least one route and cannot be "+
				"carried by more routes than it has rows", r.prefix, r.routes,
				r.observations)
		}
		if r.sessions > r.observations {
			t.Errorf("%s reports %d sessions from %d observations, which "+
				"cannot happen", r.prefix, r.sessions, r.observations)
		}
	}
	if observations != 29 {
		t.Errorf("the ranking totals %d observations, want 29 -- that is "+
			"every unicast row this fixture writes for %s", observations,
			churnRouter)
	}
	if withdraws != 2 {
		t.Errorf("the ranking totals %d withdrawals, want 2", withdraws)
	}
	if changes != 9 {
		t.Errorf("the ranking totals %d changes, want 9. 13 is what count() "+
			"- uniqExact(session_id) totals over this fixture, and it is "+
			"what shipped until 2026-09-10", changes)
	}

	var seriesChanges, seriesRows int64
	for _, b := range churnSeries(t, ctx, ch, ".*", "1 MINUTE") {
		seriesChanges += b.readvertise + b.withdraw
		seriesRows += b.readvertise + b.withdraw + b.sessionDump
	}
	if seriesRows != observations {
		t.Errorf("the time series accounts for %d rows and the ranking for "+
			"%d; they read the same table through the same filters",
			seriesRows, observations)
	}
	if seriesChanges != changes {
		t.Errorf("the time series reports %d changes and the ranking %d. "+
			"They are the same classification presented two ways, so a "+
			"disagreement means one of them aggregates it wrongly",
			seriesChanges, changes)
	}
}

// "Most-changed prefixes" is a ranking, so the order IS the claim. Asserting
// only that the numbers differ leaves the panel free to sort on the column
// that caused the original defect.
func TestRouteChurnRanksTheChangedAboveTheMerelyRedumped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	var order []string
	for _, r := range churnPrefixesInOrder(t, ctx, ch, ".*") {
		order = append(order, r.prefix)
	}
	resendAt := slices.Index(order, churnPfxResend)
	redumpAt := slices.Index(order, churnPfxRedump)
	if resendAt < 0 || redumpAt < 0 {
		t.Fatalf("ranking is missing one of the two prefixes: %v", order)
	}
	if resendAt > redumpAt {
		t.Errorf("%s (re-dumped, never changed) ranks above %s (re-advertised "+
			"twice): %v. Ordering on the observation count is what put this "+
			"lab archive's four most stable prefixes at the top of a panel called "+
			"most-changed", churnPfxRedump, churnPfxResend, order)
	}

	// An end-of-RIB marker has no prefix, and a nameless row in a ranking of
	// prefixes is not a route that changed.
	for _, p := range order {
		if p == "" {
			t.Errorf("the ranking contains a row with an empty prefix; that "+
				"is an end-of-RIB marker, not a route: %v", order)
		}
	}
}

// Every route this fixture writes is in_pre except one, which is the only
// reason $rib can be caught unread here.
func TestRouteChurnRibNarrowsToTheChosenStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	if _, present := churnPrefixes(t, ctx, ch, ".*")[churnPfxLocRib]; !present {
		t.Fatalf("%s absent with $rib on All; it is a loc_rib route",
			churnPfxLocRib)
	}
	inPre := churnPrefixes(t, ctx, ch, "in_pre")
	if _, present := inPre[churnPfxLocRib]; present {
		t.Errorf("%s still listed with $rib = in_pre; its only route is in "+
			"loc_rib, so the panel is not reading $rib", churnPfxLocRib)
	}
	if _, present := inPre[churnPfxResend]; !present {
		t.Errorf("%s vanished with $rib = in_pre, where it lives", churnPfxResend)
	}
}

// churnSeriesSQL renders one of route-churn's two time-series panels the way
// Grafana would, at a bucket width the caller chooses, scoped to the
// fixture's router.
//
// The width is a parameter and not a constant because it is the axis the
// whole fix turns on. substituteGrafana renders $__timeInterval as one
// minute, which is one width; Grafana renders it as whatever the dashboard's
// time range implies, which on a seven-day default range is nothing like a
// minute. A panel that is right at one width and wrong at another is a panel
// whose meaning changes when an operator zooms, and only a test that asks for
// more than one width can see that.
func churnSeriesSQL(t *testing.T, panel, rib, width string) string {
	t.Helper()
	raw := panelSQL(t, "route-churn", panel, "A")
	const filterAnchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(raw, filterAnchor); n != 1 {
		t.Fatalf("route-churn panel %q carries %d occurrences of %q, want 1 "+
			"-- these tests scope to the fixture's router by extending that "+
			"filter, and must not do it blind", panel, n, filterAnchor)
	}
	raw = strings.Replace(raw, filterAnchor,
		filterAnchor+" AND router_ip = toIPv6('"+churnRouter+"')", 1)

	const intervalAnchor = "$__timeInterval(ts_collector)"
	if n := strings.Count(raw, intervalAnchor); n != 1 {
		t.Fatalf("route-churn panel %q carries %d occurrences of %q, want 1 "+
			"-- a time-series panel that stopped bucketing on ts_collector "+
			"is not the panel these tests are asserting about", panel, n, intervalAnchor)
	}
	raw = strings.Replace(raw, intervalAnchor,
		"toStartOfInterval(ts_collector, INTERVAL "+width+")", 1)

	return substituteGrafana(raw, map[string]string{"rib": rib})
}

// churnCountsAsInt64 wraps a panel's own SQL so every column after the first
// two named ones comes back as Int64, without changing a character of what
// the dashboard ships.
//
// clickhouse-go will not scan a UInt64 into an *int64 nor an Int64 into a
// *uint64, and it will not scan either into an *any. That turns the width of
// the shipped expression into part of this file's test contract, which is
// backwards: countIf returns UInt64 and count() - uniqExact(session_id)
// returns Int64, so a helper committed to one of them kills the other on a
// driver type error before comparing a single number. A mutation that dies
// with "converting Int64 to *uint64 is unsupported" has not been shown to
// produce a wrong answer -- only a differently typed one -- and the rejected
// session-corrected candidate is exactly that mutation. Normalizing here
// makes every candidate reach the assertion.
//
// The passthrough names are the columns that are not counts -- the bucket,
// the peer where there is one, the prefix in the ranking -- and they are
// selected unchanged. Where the wrapped query is a ranking, its own ORDER BY
// and LIMIT sit inside the subquery and the wrapper adds neither, so the
// order the panel produced is the order that comes back.
func churnCountsAsInt64(sql string, passthrough []string, counts ...string) string {
	cols := strings.Join(passthrough, ", ")
	for _, c := range counts {
		cols += ", toInt64(" + c + ") AS " + c
	}
	return "SELECT " + cols + " FROM (\n" + sql + "\n)"
}

// churnBucketRow is one bucket of the committed "Re-advertisements, session
// dumps and withdrawals" panel.
//
// The three counts are Int64 and not UInt64, which is deliberate and is the
// opposite of the reasoning churnRow.resends records. There the width was a
// property of the shipped expression; here it is a property of the TEST.
// countIf returns UInt64, and every candidate this panel was chosen over --
// count() - uniqExact(session_id) above all -- returns Int64, so a helper
// that scanned UInt64 would kill those mutations on a driver type error
// before ever comparing a number. A test that only ever fails with
// "converting Int64 to *uint64 is unsupported" has not shown that it can
// tell a right answer from a wrong one. Widening here makes the mutation
// fail on what it reports, and leaves the impossible negative visible rather
// than wrapped to eighteen quintillion.
type churnBucketRow struct {
	readvertise int64
	withdraw    int64
	sessionDump int64
}

// churnSeries runs that panel, keyed by bucket start in Unix seconds. Unix
// seconds rather than time.Time because the assertion is "this row landed in
// the bucket the fixture aimed at", and a map keyed on a time.Time compares
// monotonic readings and locations as well as instants.
func churnSeries(t *testing.T, ctx context.Context, ch *ClickHouse, rib, width string) map[int64]churnBucketRow {
	t.Helper()
	sql := churnCountsAsInt64(
		churnSeriesSQL(t, "Re-advertisements, session dumps and withdrawals", rib, width),
		[]string{"t"}, "readvertise", "withdraw", "session_dump")
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("churn series (%s buckets): %v\nSQL:\n%s", width, err, sql)
	}
	defer rows.Close()
	out := map[int64]churnBucketRow{}
	for rows.Next() {
		var ts time.Time
		var r churnBucketRow
		if err := rows.Scan(&ts, &r.readvertise, &r.withdraw, &r.sessionDump); err != nil {
			t.Fatalf("scan churn series row: %v", err)
		}
		out[ts.Unix()] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("churn series rows: %v", err)
	}
	return out
}

// churnByPeer runs "Changes by peer" at a chosen bucket width, summed over
// the router's peers. The fixture owns two of them since 2026-09-10 -- see
// churnPeer2 -- so the peer column is validated and then folded away; the
// assertions this feeds are about the bucket totals, and the ranking panel
// is where the two peers are told apart.
func churnByPeer(t *testing.T, ctx context.Context, ch *ClickHouse, rib, width string) map[int64]int64 {
	t.Helper()
	sql := churnCountsAsInt64(
		churnSeriesSQL(t, "Changes by peer", rib, width),
		[]string{"t", "peer_ip"}, "changes")
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("changes by peer (%s buckets): %v\nSQL:\n%s", width, err, sql)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var ts time.Time
		var peer string
		var changes int64
		if err := rows.Scan(&ts, &peer, &changes); err != nil {
			t.Fatalf("scan changes-by-peer row: %v", err)
		}
		if peer != churnPeer && peer != "::ffff:"+churnPeer &&
			peer != churnPeer2 && peer != "::ffff:"+churnPeer2 {
			t.Errorf("changes-by-peer returned peer %q; this query is scoped "+
				"to %s's rows and that router has two peers, %s and %s",
				peer, churnRouter, churnPeer, churnPeer2)
		}
		out[ts.Unix()] += changes
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("changes-by-peer rows: %v", err)
	}
	return out
}

// churnRawBucket counts the fixture router's rows in one bucket without any
// correction at all -- the number the shipped panels reported.
// distinctCollectors lists the collectors holding unicast rows for one
// router, so a test stating an invariant over "the chosen collector's rows"
// can assert how many there are to choose from.
func distinctCollectors(t *testing.T, ctx context.Context, ch *ClickHouse, router string) []string {
	t.Helper()
	rows, err := ch.conn.Query(ctx, qualify(ch, fmt.Sprintf(
		`SELECT DISTINCT collector_id FROM vantage.route_unicast FINAL
		 WHERE router_ip = toIPv6('%s') ORDER BY collector_id`, router)))
	if err != nil {
		t.Fatalf("list collectors for %s: %v", router, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan collector: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("collectors for %s: %v", router, err)
	}
	return out
}

func churnRawBucket(t *testing.T, ctx context.Context, ch *ClickHouse, start time.Time, width string) (observations, sessions int64) {
	t.Helper()
	sql := fmt.Sprintf(`
		SELECT toInt64(count()), toInt64(uniqExact(session_id))
		FROM vantage.route_unicast FINAL
		WHERE router_ip = toIPv6('%s')
		  AND toStartOfInterval(ts_collector, INTERVAL %s) = toDateTime64(%d, 6)`,
		churnRouter, width, start.Unix())
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&observations, &sessions); err != nil {
		t.Fatalf("raw bucket at %s: %v", start, err)
	}
	return observations, sessions
}

// The proven defect, in the one bucket built to prove it.
//
// Measured on 2026-09-10, on a 2.06M-row rig: the two
// shipped time-series panels reported the identical number for a bucket that
// was entirely session re-dump and a bucket that was entirely genuine churn.
// This is the same demonstration compressed into a single bucket, which is
// strictly harder: the two shapes are not merely equal-looking in separate
// buckets, they are INTERLEAVED in one, so no query can separate them by
// noticing which bucket a row fell in. Only a per-row classification can.
//
// Four observations, two of them changes. The three candidates disagree --
// see churnPfxBucketFlap's comment for the arithmetic -- and this test pins
// the panel to the one that is right.
func TestRouteChurnSeriesSeparatesChangeFromRedumpInsideOneBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	mixed := churnBucketAt(100, 0)

	// The premise, asserted rather than assumed: four observations from
	// three sessions, all inside one minute.
	obs, sessions := churnRawBucket(t, ctx, ch, mixed, "1 MINUTE")
	if obs != 4 || sessions != 3 {
		t.Fatalf("the mixed bucket holds %d observations from %d sessions, "+
			"want 4 from 3 -- without that shape this test proves nothing, "+
			"because count() and count()-uniqExact(session_id) would not "+
			"disagree with the truth", obs, sessions)
	}

	got, present := churnSeries(t, ctx, ch, ".*", "1 MINUTE")[mixed.Unix()]
	if !present {
		t.Fatalf("the panel reports no bucket at %s at all", mixed)
	}
	if got.readvertise != 2 {
		t.Errorf("readvertise = %d in the mixed bucket, want 2 -- %s was "+
			"re-advertised twice inside %d there. 4 means the panel is back "+
			"to counting messages; 1 means it is subtracting the sessions "+
			"visible in the bucket, which cancels a re-dump and a change "+
			"alike", got.readvertise, churnPfxBucketFlap, churnSession1)
	}
	if got.sessionDump != 2 {
		t.Errorf("session_dump = %d in the mixed bucket, want 2 -- %s was "+
			"seen once in %d and once in %d, and neither was a change",
			got.sessionDump, churnPfxBucketRedump, churnSession2, churnSession3)
	}
	if got.withdraw != 0 {
		t.Errorf("withdraw = %d in the mixed bucket, want 0; nothing is "+
			"withdrawn there", got.withdraw)
	}

	// And the same bucket through "Changes by peer", which counts
	// withdrawals as changes too and so must report the same 2 here.
	if byPeer := churnByPeer(t, ctx, ch, ".*", "1 MINUTE")[mixed.Unix()]; byPeer != 2 {
		t.Errorf("changes-by-peer = %d in the mixed bucket, want 2 (the same "+
			"two re-advertisements). 4 is count(); 1 is "+
			"count()-uniqExact(session_id)", byPeer)
	}
}

// The property the fix exists for: the answer does not move when the bucket
// width does.
//
// Grafana picks the bucket width from the dashboard's time range, so an
// operator changes it just by zooming. The session-corrected candidate --
// bucket count() - uniqExact(session_id), the formula "Most-changed
// prefixes" uses per prefix -- cannot hold this property even in principle:
// it can only see two observations as belonging to one session if they land
// in the SAME bucket, so narrowing the bucket erodes the signal. That
// erosion was measured on a prefix genuinely re-advertised 99 times inside
// one session: 99 of 99 captured at one-day buckets, 98 at one hour, 93 at
// fifteen minutes, 80 at five, and 0 at one minute. A panel that reports a
// route as stable at one zoom level and flapping at the next is worse than a
// panel that is honestly wrong, because nothing on screen says which reading
// to believe.
//
// The classification the panels now use is per row and runs before any
// bucketing, so the width cannot reach it.
func TestRouteChurnSeriesIsTheSameNumberAtEveryBucketWidth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	// Nine genuine changes in the whole fixture: two re-advertisements of
	// churnPfxResend, the withdrawal of churnPfxWithdrawn inside the session
	// that advertised it, two re-advertisements of churnPfxBucketFlap, the
	// lone withdrawal of churnPfxLoneWithdraw, and three re-advertisements
	// of churnPfxBothPeersChange across its two routes (two from one peer,
	// one from the other). Everything else this fixture writes is a session
	// dump -- including all eight rows of the four two-route prefixes, which
	// is the whole point of them.
	const wantChanges = 9
	const wantWithdraws = 2

	for _, width := range []string{"1 MINUTE", "5 MINUTE", "15 MINUTE", "1 HOUR", "1 DAY"} {
		var changes, withdraws, dumps int64
		for _, b := range churnSeries(t, ctx, ch, ".*", width) {
			changes += b.readvertise + b.withdraw
			withdraws += b.withdraw
			dumps += b.sessionDump
		}
		if changes != wantChanges {
			t.Errorf("at %s buckets the panel reports %d changes, want %d "+
				"(%s x2, %s, %s x2, %s). A number that depends on the bucket "+
				"width is a number that changes meaning when an operator "+
				"zooms", width, changes, wantChanges, churnPfxResend,
				churnPfxWithdrawn, churnPfxBucketFlap, churnPfxLoneWithdraw)
		}
		if withdraws != wantWithdraws {
			t.Errorf("at %s buckets the panel reports %d withdrawals, want %d",
				width, withdraws, wantWithdraws)
		}
		// The dumps are the fixture's other rows, and they must not move
		// either: a classification that drifted with the width would show up
		// here first if it happened to keep `changes` intact.
		if dumps != 20 {
			t.Errorf("at %s buckets the panel reports %d session dumps, want "+
				"20 -- the fixture writes 29 unicast rows and 9 of them are "+
				"changes", width, dumps)
		}

		// "Changes by peer" counts the same six by the same rule.
		var byPeer int64
		for _, c := range churnByPeer(t, ctx, ch, ".*", width) {
			byPeer += c
		}
		if byPeer != wantChanges {
			t.Errorf("at %s buckets changes-by-peer reports %d, want %d",
				width, byPeer, wantChanges)
		}
	}
}

// The asymmetry, tested rather than asserted: the withdrawal column was
// never broken and must not be "fixed".
//
// A BMP table dump advertises. It does not withdraw -- there is nothing to
// withdraw yet, that being the point of a dump -- so "the first observation
// of this route inside this session" identifies a collection artifact for an
// advertisement and identifies nothing at all for a withdrawal. The
// classification carries an is_withdraw = 0 guard for exactly that reason,
// and %s is the row that makes the guard load-bearing: it is withdrawn in
// churnSession2 having never been advertised there, so a symmetric rule
// would call it a dump and silently delete a real withdrawal from both
// panels. The lab archive holds 12 such rows among its 1,619 withdrawals.
func TestRouteChurnSeriesLeavesWithdrawalsUncorrected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	lone := churnBucketAt(105, 0)
	obs, sessions := churnRawBucket(t, ctx, ch, lone, "1 MINUTE")
	if obs != 1 || sessions != 1 {
		t.Fatalf("the lone-withdrawal bucket holds %d observations from %d "+
			"sessions, want 1 from 1", obs, sessions)
	}

	got, present := churnSeries(t, ctx, ch, ".*", "1 MINUTE")[lone.Unix()]
	if !present {
		t.Fatalf("the panel reports no bucket at %s; %s was withdrawn there "+
			"and a withdrawal is a change", lone, churnPfxLoneWithdraw)
	}
	if got.withdraw != 1 {
		t.Errorf("withdraw = %d in the lone-withdrawal bucket, want 1. 0 "+
			"means the withdrawal column started subtracting first-in-session "+
			"rows, which a dump can never produce", got.withdraw)
	}
	if got.sessionDump != 0 {
		t.Errorf("session_dump = %d in the lone-withdrawal bucket, want 0 -- "+
			"%s is a withdrawal, and a table dump does not withdraw",
			got.sessionDump, churnPfxLoneWithdraw)
	}
	if got.readvertise != 0 {
		t.Errorf("readvertise = %d in the lone-withdrawal bucket, want 0",
			got.readvertise)
	}

	// "Changes by peer" is where dropping the guard actually bites: its one
	// column is countIf(NOT is_session_dump), so a withdrawal misfiled as a
	// dump disappears from the panel entirely.
	if byPeer := churnByPeer(t, ctx, ch, ".*", "1 MINUTE")[lone.Unix()]; byPeer != 1 {
		t.Errorf("changes-by-peer = %d in the lone-withdrawal bucket, want 1. "+
			"0 means the is_withdraw = 0 guard was dropped from the "+
			"classification and %s vanished from the panel",
			byPeer, churnPfxLoneWithdraw)
	}
}

// Every row is classified into exactly one of the three series, so the three
// must add back up to the observations the bucket holds -- WITHIN THE
// COLLECTOR THE PANEL CHOSE.
//
// That qualifier costs the invariant nothing. What this guards is that the
// classification is TOTAL: that no row falls between dump, re-advertisement
// and withdrawal. Totality holds of one collector's view exactly as much as
// of the sum, and this test never asserted that summing observers is the
// intended answer -- that is a separate question, settled by choosing a
// per-collector view over a summed one.
//
// This is the check that catches a classification which drops rows rather
// than one which mislabels them. Both failure modes read the same on screen
// -- a line that is lower than it should be -- and the counts above would
// only catch the second, because they assert about buckets the fixture
// arranged. This one asserts about every bucket there is.
func TestRouteChurnSeriesAccountsForEveryObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRouteChurnFixture(t, ctx, ch)

	// THE PREMISE, asserted rather than assumed: this fixture writes ONE
	// collector, so the chosen collector's rows are all of them and
	// churnRawBucket's unscoped count is the right comparison. That is also
	// exactly why this test passed unchanged through the 2026-09-20
	// best-vantage-point change and proves nothing about it -- the
	// one-branch fixture problem, named here so the next reader does not
	// have to rediscover it.
	//
	// Add a second collector to insertRouteChurnFixture and this fails by
	// name, instead of the invariant quietly comparing the panel against
	// rows it deliberately no longer reports. Two-collector coverage of the
	// same identity lives on insertStaggeredCollectorFixture, which is
	// built for it.
	collectors := distinctCollectors(t, ctx, ch, churnRouter)
	if len(collectors) != 1 {
		t.Fatalf("insertRouteChurnFixture writes %d collectors (%v), want 1. "+
			"This test compares the panel against EVERY row under the "+
			"router, which is only the chosen collector's view while there "+
			"is one; with two it must be narrowed to the winner the way "+
			"TestEvpnChurnSeriesAccountsForEveryObservation is",
			len(collectors), collectors)
	}

	series := churnSeries(t, ctx, ch, ".*", "1 MINUTE")
	if len(series) == 0 {
		t.Fatal("the panel returned no buckets at all")
	}
	var total int64
	for start, b := range series {
		at := time.Unix(start, 0).UTC()
		obs, _ := churnRawBucket(t, ctx, ch, at, "1 MINUTE")
		if b.readvertise < 0 || b.withdraw < 0 || b.sessionDump < 0 {
			t.Errorf("bucket %s reports %d/%d/%d; a classification counts "+
				"rows and cannot go below zero, while every candidate that "+
				"SUBTRACTS can", at, b.readvertise, b.withdraw, b.sessionDump)
		}
		if sum := b.readvertise + b.withdraw + b.sessionDump; sum != obs {
			t.Errorf("bucket %s: readvertise %d + withdraw %d + session_dump "+
				"%d = %d, but the bucket holds %d rows. Every row is either "+
				"a session dump, a re-advertisement or a withdrawal, and "+
				"nothing may fall between them", at, b.readvertise,
				b.withdraw, b.sessionDump, sum, obs)
		}
		total += obs
	}
	if total != 29 {
		t.Errorf("the fixture's router accounts for %d unicast rows across "+
			"all buckets, want 29 -- the end-of-RIB marker goes to "+
			"eor_events and must not appear here", total)
	}
}

const (
	// paNoisy carries ONE flagged prefix and re-dumps it across five
	// sessions. paBroad carries FOUR flagged prefixes in a single session.
	// Ranked on raw message count the noisy router wins 5 to 4; ranked on
	// what the quirk actually affects it loses 1 to 4.
	//
	// This mirrors the lab archive exactly, which is how the defect was
	// found: one router shows 73 VERSION_UNPARSED "events" across 35
	// sessions touching 3 prefixes, while another shows 33 across 2
	// sessions touching 12. The panel called "the new-quirk detector" put
	// the noisier router at the top and the router with four times the
	// affected routes third.
	paNoisyRouter = "10.0.199.14"
	paBroadRouter = "10.0.199.15"
	// paDupRouter emits ONE flagged route written twice under the same
	// stream_seq -- an identical ReplacingMergeTree ORDER BY key, i.e. a true
	// duplicate. FINAL collapses it; without FINAL it is counted twice.
	paDupRouter = "10.0.199.16"
	// paWideRouter sends ONE message carrying paWidePrefixes prefixes, all
	// flagged. It is the only router here whose ROW count and whose MESSAGE
	// count differ, and until it existed no test on this dashboard could tell
	// the two apart -- every other fixture envelope carries exactly one
	// prefix, so count() and uniqExact((stream, stream_seq)) returned the
	// same number and the panels' own descriptions could not be checked
	// against their SQL.
	paWideRouter   = "10.0.199.17"
	paWidePrefixes = 4
	// paSpanRouter sends ONE BGP-LS message that lands in TWO tables:
	// RowsFor emits a typed ls_nodes row AND a raw ls_events row when a
	// decode is mixed, "one envelope can legitimately produce BOTH typed
	// rows AND a raw row" (rows.go). Both carry the same stream_seq in the
	// same LS stream, so they are one message in two places -- which is the
	// only shape that can tell uniqExact((stream, stream_seq)) from
	// uniqExact((src, stream_seq)), and the panel descriptions' own claim
	// that "one flag appearing in several tables is normal".
	paSpanRouter  = "10.0.199.18"
	paPeer        = "10.255.199.19"
	paSessionBase = 9960
	paFlag        = "PARSE_FLAG_VERSION_UNPARSED"
)

// paFixedRouter carries a quirk that stopped: its flagged rows are older than
// every other router's by days, which is what a fixed decoder looks like in an
// append-only archive.
const paFixedRouter = "10.0.199.15"

// paFixedQuirkTS is when that router's quirk was last seen: hours before the
// rest of the fixture, and deliberately still INSIDE the panel's own time
// window.
//
// Inside, because a row the time filter excludes proves nothing -- the panel
// would be silent about it for the ordinary reason and last_seen would not be
// what made the difference. The case worth defending against is the one the
// lab archive is in: the rows are legitimately in range, they are simply
// old, and nothing on the panel says so.
var paFixedQuirkTS = corpusCollectorClock.Add(-20 * time.Hour)

// insertParseAnomalyFixture writes flagged routes from two routers whose
// message volume and quirk spread rank them in opposite orders, plus a third
// whose quirk has been fixed.
func insertParseAnomalyFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	v4 := &vantagev1.Family{Afi: 1, Safi: 1}
	env := func(router, sys string, session uint64, ts time.Time, prefix string) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: paPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			ParseFlags:  []vantagev1.ParseFlag{vantagev1.ParseFlag_PARSE_FLAG_VERSION_UNPARSED},
			Payload: &vantagev1.Envelope_Route{Route: &vantagev1.RouteEvent{
				Family: v4,
				Attrs: &vantagev1.PathAttributes{
					Origin: 0, NextHop: "10.205.0.1",
					AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000}}},
				},
				Announced: []*vantagev1.Prefix{{Prefix: prefix}},
			}},
		}
	}
	// envWide is env with more than one NLRI in the same UPDATE: one BMP
	// message, several routes. This is the shape the whole "messages, not
	// rows" question turns on, and the lab archive has it today --
	// ls_prefixes carries 61 flagged ROWS across 23 flagged MESSAGES.
	envWide := func(router, sys string, session uint64, ts time.Time, prefixes []string) *vantagev1.Envelope {
		e := env(router, sys, session, ts, prefixes[0])
		var nlri []*vantagev1.Prefix
		for _, p := range prefixes {
			nlri = append(nlri, &vantagev1.Prefix{Prefix: p})
		}
		e.GetRoute().Announced = nlri
		return e
	}
	now := corpusCollectorClock
	var envs []*vantagev1.Envelope

	// One prefix, five sessions: five messages, one affected route.
	for i := range 5 {
		envs = append(envs, env(paNoisyRouter, "dash-pa-noisy",
			uint64(paSessionBase+i), now.Add(-time.Duration(50-i*10)*time.Minute),
			"10.205.1.0/24"))
	}
	// Four prefixes, one session: four messages, four affected routes.
	//
	// Spread from nearly a day ago to just now, because a quirk that is STILL
	// happening usually has been happening for a while. That span is also what
	// makes last_seen testable: the first sighting is older than the fixed
	// router's last one, so a panel reporting min(ts_collector) instead of
	// max(ts_collector) would rank this live quirk as the stale one. With the
	// rows bunched into a few minutes, min and max were interchangeable and
	// the assertion could not tell them apart -- verified by mutation.
	broadTS := []time.Duration{-22 * time.Hour, -12 * time.Hour, -2 * time.Hour, -time.Minute}
	for i := range 4 {
		envs = append(envs, env(paBroadRouter, "dash-pa-broad",
			uint64(paSessionBase+90), now.Add(broadTS[i]),
			fmt.Sprintf("10.205.2.%d/24", i)))
	}
	// A quirk that was FIXED: widely spread, so it ranks near the top on
	// affected, and not seen for the better part of a day. parse_flags record the
	// decoder as it was when the row was written, so a flag stops appearing
	// on new rows the moment its cause is fixed and every row that already
	// carries it keeps carrying it forever.
	//
	// The lab archive is doing exactly this today.
	// PARSE_FLAG_VERSION_UNPARSED holds 1,403 events across 263 sessions and
	// stopped on 2026-08-17, when the decoder began deriving vendor identity
	// from the captured banner, in a lab archive that runs to 2026-08-30 --
	// and the panel that calls itself a new-quirk detector ranks it second
	// with nothing saying it is over.
	for i := range 6 {
		envs = append(envs, env(paFixedRouter, "dash-pa-fixed",
			uint64(paSessionBase+120), paFixedQuirkTS.Add(time.Duration(i)*time.Minute),
			fmt.Sprintf("10.205.3.%d/24", i)))
	}
	// ONE message, four flagged routes: the exact inverse of paNoisyRouter,
	// which is one route across five messages. A panel counting rows reports
	// 4 here; a panel counting messages reports 1, and the panels on this
	// dashboard all promise the latter in so many words.
	widePrefixes := make([]string, paWidePrefixes)
	for i := range widePrefixes {
		widePrefixes[i] = fmt.Sprintf("10.205.4.%d/32", i)
	}
	envs = append(envs, envWide(paWideRouter, "dash-pa-wide",
		uint64(paSessionBase+140), now.Add(-15*time.Minute), widePrefixes))

	// One BGP-LS message, mixed decode: a typed node plus raw MP_REACH bytes
	// the build could not type. Two rows, two tables, ONE stream_seq.
	envs = append(envs, &vantagev1.Envelope{
		CollectorId: "c1",
		Router:      &vantagev1.RouterId{Ip: paSpanRouter, SysName: "dash-pa-span"},
		Peer:        &vantagev1.PeerId{Ip: paPeer, Asn: 65000},
		SessionId:   uint64(paSessionBase + 150),
		TsRouter:    timestamppb.New(now.Add(-14 * time.Minute)),
		TsCollector: timestamppb.New(now.Add(-14 * time.Minute)),
		ParseFlags:  []vantagev1.ParseFlag{vantagev1.ParseFlag_PARSE_FLAG_VERSION_UNPARSED},
		Payload: &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{
			Nodes:    []*vantagev1.LsNode{{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 205, 5, 1})}},
			RawReach: []byte{0x40, 0x04, 0x47, 0x00},
		}},
	})

	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert parse-anomaly fixture envelope %d: %v", i, err)
		}
	}

	// The same row twice, at the same stream_seq: identical in every column
	// the ORDER BY names, so ReplacingMergeTree holds both until a merge and
	// FINAL is what collapses them.
	dup := env(paDupRouter, "dash-pa-dup", uint64(paSessionBase+95),
		now.Add(-20*time.Minute), "10.205.3.0/24")
	for i := range 2 {
		if err := ch.Insert(ctx, mustRowsFor(t, dup, 1)); err != nil {
			t.Fatalf("insert parse-anomaly duplicate %d: %v", i, err)
		}
	}
}

// paRow is one row of the committed "Flag by platform" table.
type paRow struct {
	flag     string
	platform string
	router   string
	affected uint64
	sessions uint64
	events   uint64
	inTables uint64
	lastSeen time.Time
}

func parseAnomalyRows(t *testing.T, ctx context.Context, ch *ClickHouse) []paRow {
	t.Helper()
	raw := panelSQL(t, "parse-anomalies", "Flag by platform — the new-quirk detector", "A")
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("flag by platform: %v", err)
	}
	defer rows.Close()
	var out []paRow
	for rows.Next() {
		var r paRow
		if err := rows.Scan(&r.flag, &r.platform, &r.router, &r.affected,
			&r.sessions, &r.events, &r.inTables, &r.lastSeen); err != nil {
			t.Fatalf("scan parse-anomaly row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("parse-anomaly rows: %v", err)
	}
	return out
}

// scopeParseAnomaliesToRouter narrows a parse-anomalies panel to one router.
// Unlike fleet-health's "Parse flags", these panels filter OUTSIDE their
// union -- one WHERE over the whole ten-arm subquery rather than one per arm
// -- so there is a single anchor to extend, and router_ip is a column the
// union already selects.
func scopeParseAnomaliesToRouter(t *testing.T, sql, router string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("a parse-anomalies query carries %d occurrences of %q, want 1 "+
			"-- these panels filter once, outside their UNION, and this helper "+
			"must not scope blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip = toIPv6('"+router+"')", 1)
}

// parseAnomalyFlagMix reads "Flag mix" for one router: flag -> events.
func parseAnomalyFlagMix(t *testing.T, ctx context.Context, ch *ClickHouse, router string) map[string]uint64 {
	t.Helper()
	raw := scopeParseAnomaliesToRouter(t, panelSQL(t, "parse-anomalies", "Flag mix", "A"), router)
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("flag mix: %v", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var flag string
		var events uint64
		if err := rows.Scan(&flag, &events); err != nil {
			t.Fatalf("scan flag mix row: %v", err)
		}
		out[flag] = events
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("flag mix rows: %v", err)
	}
	return out
}

// parseAnomalyFlagsOverTime reads "Flags over time, by flag" for one router
// and sums each flag across every bucket.
func parseAnomalyFlagsOverTime(t *testing.T, ctx context.Context, ch *ClickHouse, router string) (map[string]uint64, int) {
	t.Helper()
	raw := scopeParseAnomaliesToRouter(t, panelSQL(t, "parse-anomalies", "Flags over time, by flag", "A"), router)
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("flags over time: %v", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	var buckets int
	for rows.Next() {
		var bucket time.Time
		var flag string
		var events uint64
		if err := rows.Scan(&bucket, &flag, &events); err != nil {
			t.Fatalf("scan flags-over-time row: %v", err)
		}
		out[flag] += events
		buckets++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("flags-over-time rows: %v", err)
	}
	return out, buckets
}

// parseAnomalyWhereFlagsLand reads "Where the flags land" for one router:
// (flag, table) -> events.
func parseAnomalyWhereFlagsLand(t *testing.T, ctx context.Context, ch *ClickHouse, router string) map[[2]string]uint64 {
	t.Helper()
	raw := scopeParseAnomaliesToRouter(t, panelSQL(t, "parse-anomalies", "Where the flags land", "A"), router)
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("where the flags land: %v", err)
	}
	defer rows.Close()
	out := map[[2]string]uint64{}
	for rows.Next() {
		var flag, table string
		var events uint64
		if err := rows.Scan(&flag, &table, &events); err != nil {
			t.Fatalf("scan where-flags-land row: %v", err)
		}
		out[[2]string{flag, table}] = events
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("where-flags-land rows: %v", err)
	}
	return out
}

// THE THREE PANELS BELOW ALL PROMISE A MESSAGE COUNT IN SO MANY WORDS:
//
//	Flag mix                 "The proportion of flagged MESSAGES each flag
//	                          accounts for ... this panel answers 'what is
//	                          filling the pipe', which is a question about
//	                          messages."
//	Flags over time, by flag "Flagged messages over time."
//	Where the flags land     "a message count is the right measure here"
//
// All three computed count(), which counts ROWS. One BMP UPDATE carrying
// several NLRIs is one message and several rows, so the panels over-reported
// by the width of the update. fleet-health's "Parse flags" panel had already
// ruled on this exact question the other way, with its reasoning written out:
// "one truncated-attribute UPDATE carrying 50 prefixes is one parse event,
// not 50" -- so the two dashboards disagreed about the same number.
//
// NOT THEORETICAL. Measured on the lab archive before writing these:
//
//	flag                          table            rows   messages
//	PARSE_FLAG_VERSION_UNPARSED   route_unicast     230        163
//	PARSE_FLAG_VERSION_UNPARSED   route_evpn        202        149
//	PARSE_FLAG_VERSION_UNPARSED   route_vpn         138        127
//	PARSE_FLAG_VERSION_UNPARSED   ls_prefixes        61         23
//
// "Flag mix" is a PROPORTION, so this does not merely inflate it: ls_prefixes
// is over-counted 2.65x and route_vpn 1.09x, which reorders the pie an
// operator uses to decide which decoder quirk to chase.
//
// The fixture's paWideRouter is one message carrying four flagged routes --
// the inverse of paNoisyRouter's one route across five messages. Every other
// envelope in this fixture carries exactly one prefix, which is why nothing
// here could tell rows from messages until it existed.
func TestParseAnomalyFlagMixCountsMessagesNotRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	got := parseAnomalyFlagMix(t, ctx, ch, paWideRouter)
	if v := got[paFlag]; v != 1 {
		t.Errorf("Flag mix reports %d flagged messages for the router that "+
			"sent ONE message carrying %d flagged routes, want 1. %d is a row "+
			"count, and this panel's own description calls it \"the proportion "+
			"of flagged MESSAGES\" -- as a proportion, over-counting wide "+
			"updates reorders the pie rather than only inflating it",
			v, paWidePrefixes, paWidePrefixes)
	}
}

func TestParseAnomalyFlagsOverTimeCountsMessagesNotRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	got, buckets := parseAnomalyFlagsOverTime(t, ctx, ch, paWideRouter)
	if buckets == 0 {
		t.Fatal("the series is empty for the wide-update router, so the " +
			"count below would be asserting nothing")
	}
	if v := got[paFlag]; v != 1 {
		t.Errorf("Flags over time reports %d flagged messages across %d "+
			"bucket(s) for a router that sent ONE message, want 1 -- the "+
			"panel's description is \"flagged messages over time\", and a step "+
			"in this series is read as a router reconnecting, so a single wide "+
			"update reads as %d separate arrivals",
			v, buckets, paWidePrefixes)
	}
}

func TestParseAnomalyWhereFlagsLandCountsMessagesNotRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	got := parseAnomalyWhereFlagsLand(t, ctx, ch, paWideRouter)
	key := [2]string{paFlag, "route_unicast"}
	v, ok := got[key]
	if !ok {
		t.Fatalf("the panel locates no %s in route_unicast for the wide-update "+
			"router; it holds %v", paFlag, got)
	}
	if v != 1 {
		t.Errorf("Where the flags land reports %d for one message carrying %d "+
			"flagged routes, want 1 -- its description says \"a message count "+
			"is the right measure here\"", v, paWidePrefixes)
	}
}

// One BGP-LS message that lands in TWO tables is the only shape that can tell
// the right dedup key from a plausible wrong one. uniqExact((src, stream_seq))
// -- keyed on the TABLE -- reports it as two messages; uniqExact((stream,
// stream_seq)) reports one. Both are correct for "Where the flags land",
// where the grouping already fixes the table, so the difference is invisible
// there and visible only on the flag-wide panels.
//
// This gap was found by MUTATION, not by review: with a fixture whose
// every envelope produces rows in exactly one table, swapping the key to
// (src, stream_seq) changes no assertion. The
// clause needed that (src, stream_seq)-style disambiguation in the key to
// be falsifiable at all; without it, the test guarding it could not
// actually see the defect.
//
// The panels' descriptions claim the two-table case out loud -- "one flag
// appearing in several tables is normal: a quirk in the per-peer header is
// stamped on every row parsed from that message, whatever stream it belongs
// to" -- and nothing had ever checked it.
func TestParseAnomalyCountsOneMessageOnceAcrossTwoTables(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	// The premise: this really is one message in two tables. Asserted rather
	// than assumed, because if RowsFor ever stopped emitting the raw row
	// beside the typed one, every assertion below would still pass while
	// testing nothing.
	land := parseAnomalyWhereFlagsLand(t, ctx, ch, paSpanRouter)
	if v := land[[2]string{paFlag, "ls_nodes"}]; v != 1 {
		t.Errorf("ls_nodes = %d for the span router, want 1", v)
	}
	if v := land[[2]string{paFlag, "ls_events"}]; v != 1 {
		t.Errorf("ls_events = %d for the span router, want 1 -- a mixed BGP-LS "+
			"decode emits a raw row beside the typed one, and without it this "+
			"fixture holds no envelope spanning two tables at all", v)
	}

	// The claim. Flag mix groups by flag across every table, so it must see
	// the two rows as the ONE message they came from.
	if v := parseAnomalyFlagMix(t, ctx, ch, paSpanRouter)[paFlag]; v != 1 {
		t.Errorf("Flag mix reports %d flagged messages for a router that sent "+
			"ONE, want 1. 2 means the count is keyed on the table rather than "+
			"the stream -- a wrong key that is invisible on \"Where the flags "+
			"land\", whose grouping fixes the table anyway", v)
	}
}

// The detector's own `events` column carries the identical promise -- its
// description says "`events` counts messages" and contrasts it with
// `affected`, which counts distinct things. The wide router is the sharpest
// possible case for that contrast and the exact inverse of paNoisyRouter:
// four affected routes from ONE message, against one affected route from
// five messages. A row count collapses the distinction the column exists to
// draw.
func TestParseAnomalyDetectorCountsAWideUpdateAsOneMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	var wide *paRow
	for _, r := range parseAnomalyRows(t, ctx, ch) {
		if r.platform == "dash-pa-wide" {
			row := r
			wide = &row
		}
	}
	if wide == nil {
		t.Fatal("dash-pa-wide absent from the detector")
	}
	if wide.affected != paWidePrefixes {
		t.Errorf("dash-pa-wide affected = %d, want %d -- one UPDATE carrying "+
			"four NLRIs affects four routes", wide.affected, paWidePrefixes)
	}
	if wide.events != 1 {
		t.Errorf("dash-pa-wide events = %d, want 1. The panel ranks on "+
			"`affected` precisely so that `events` can expose re-dump skew "+
			"beside it; counting rows makes a single wide update look like %d "+
			"re-dumps and hides the skew this column exists to show",
			wide.events, paWidePrefixes)
	}
	if wide.sessions != 1 {
		t.Errorf("dash-pa-wide sessions = %d, want 1", wide.sessions)
	}
}

// The panel's title says it detects new quirks, so what it puts at the top is
// its entire claim. Ranking on the raw message count ranks routers by how
// often they reset: a router that re-dumps one flagged prefix every nine
// minutes outranks one carrying four times as many flagged routes.
func TestParseAnomalyDetectorRanksBySpreadNotMessageVolume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	byPlatform := map[string]paRow{}
	for _, r := range parseAnomalyRows(t, ctx, ch) {
		byPlatform[r.platform] = r
	}
	noisy, okN := byPlatform["dash-pa-noisy"]
	broad, okB := byPlatform["dash-pa-broad"]
	if !okN || !okB {
		t.Fatalf("fixture platforms missing: noisy=%v broad=%v", okN, okB)
	}

	// The premise: message volume ranks them the WRONG way round.
	if noisy.events <= broad.events {
		t.Fatalf("noisy sent %d messages and broad %d; noisy must send MORE "+
			"or this test cannot tell a volume ranking from a spread one",
			noisy.events, broad.events)
	}
	if noisy.affected != 1 {
		t.Errorf("dash-pa-noisy affected = %d, want 1 -- it re-dumped a single "+
			"prefix five times", noisy.affected)
	}
	if broad.affected != 4 {
		t.Errorf("dash-pa-broad affected = %d, want 4", broad.affected)
	}
	if noisy.sessions != 5 || broad.sessions != 1 {
		t.Errorf("sessions: noisy = %d (want 5), broad = %d (want 1)",
			noisy.sessions, broad.sessions)
	}
}

// And the order itself, because a ranking panel's order is the claim.
func TestParseAnomalyDetectorPutsTheWiderQuirkFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	var order []string
	for _, r := range parseAnomalyRows(t, ctx, ch) {
		if r.platform == "dash-pa-noisy" || r.platform == "dash-pa-broad" {
			order = append(order, r.platform)
		}
	}
	if len(order) != 2 {
		t.Fatalf("expected both fixture platforms in the ranking, got %v", order)
	}
	if order[0] != "dash-pa-broad" {
		t.Errorf("ranking puts %q first; dash-pa-broad carries four flagged "+
			"routes to dash-pa-noisy's one, and only outranks it if the panel "+
			"measures spread rather than how often a router re-dumped", order[0])
	}
}

// Every other dashboard reads these tables with FINAL; parse-anomalies read
// all eight without it, so its counts depended on whether a background merge
// had happened to run. The fixture writes one route twice under the same
// ORDER BY key to make that visible.
//
// This assertion is correct either way -- one route is one event -- but it
// only DISCRIMINATES while the duplicate is unmerged. If ClickHouse has
// already merged the part, a query without FINAL returns 1 too and the
// missing clause costs nothing at that moment. That is the nature of the
// bug: it is intermittent, which is worse than being wrong.
func TestParseAnomalyCountsADuplicatedRowOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	var dup *paRow
	for _, r := range parseAnomalyRows(t, ctx, ch) {
		if r.platform == "dash-pa-dup" {
			row := r
			dup = &row
		}
	}
	if dup == nil {
		t.Fatalf("dash-pa-dup absent from the ranking")
	}
	if dup.events != 1 {
		t.Errorf("dash-pa-dup events = %d, want 1 -- one route was written "+
			"twice under one stream_seq, which is a duplicate row and not a "+
			"second occurrence of the quirk", dup.events)
	}
	if dup.affected != 1 {
		t.Errorf("dash-pa-dup affected = %d, want 1", dup.affected)
	}
}

// fleet-health's "Parse flags" panel had no test at all before this -- one of
// 19 panels across the thirteen dashboards that nothing asserts a value for,
// and the one holding the most FINAL keywords (nine, one per UNION arm).
//
// What it actually counts is uniqExact((stream, stream_seq)) over ten tables,
// and that is why those nine FINALs are removable: a redelivered envelope
// carries the SAME stream_seq, so uniqExact collapses it whether or not the
// duplicate has been merged away. FINAL was doing no work the outer aggregate
// was not already doing -- which the eor_events arm says out loud by reaching
// for DISTINCT instead, that table being a plain MergeTree with no FINAL to
// reach for.
//
// insertParseAnomalyFixture writes one envelope TWICE at one stream_seq for
// exactly this purpose, so this asserts the number an operator reads: one
// redelivered envelope is one envelope. It discriminates only while the
// duplicate is unmerged, the same honest limit TestParseAnomalyCountsA
// DuplicatedRowOnce states about itself.
func TestFleetParseFlagsCountsARedeliveredEnvelopeOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	raw := scopeParseFlagsToRouter(t, panelSQL(t, "fleet-health", "Parse flags", "A"), paDupRouter)
	sql := substituteGrafana(raw, map[string]string{"rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("fleet-health parse flags: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()

	got := map[string]uint64{}
	for rows.Next() {
		var flag string
		var envelopes uint64
		if err := rows.Scan(&flag, &envelopes); err != nil {
			t.Fatalf("scan parse-flag row: %v", err)
		}
		got[flag] = envelopes
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("parse flags mid-stream: %v", err)
	}

	// The fixture stamps one flag on every envelope it writes, and this
	// router owns exactly one of them -- written twice.
	if len(got) != 1 {
		t.Fatalf("parse flags returned %d flags for %s, want exactly 1: the "+
			"fixture gives that router a single flagged envelope, so anything "+
			"else means this query is not scoped to it and the count below "+
			"would be a claim about the whole database", len(got), paDupRouter)
	}
	for flag, envelopes := range got {
		if envelopes != 1 {
			t.Errorf("flag %q reports %d envelopes for %s, want 1 -- that "+
				"envelope was written twice under one stream_seq, which is a "+
				"redelivery and not a second occurrence of the quirk. A panel "+
				"that counts it twice tells an operator a quirk is getting "+
				"worse when nothing changed", flag, envelopes, paDupRouter)
		}
	}
}

// scopeParseFlagsToRouter narrows every arm of the "Parse flags" UNION to one
// router. It insists on finding the filter in EVERY arm rather than replacing
// what it happens to find: this query reads ten tables, and scoping nine of
// them would leave the tenth contributing the whole shared corpus to a count
// the test then asserts an exact number for.
func scopeParseFlagsToRouter(t *testing.T, sql, router string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	const wantArms = 10
	if n := strings.Count(sql, anchor); n != wantArms {
		t.Fatalf("the parse-flags query carries %d occurrences of %q, want %d "+
			"(one per table in its UNION). If an arm was added or removed, this "+
			"helper is no longer scoping all of them", n, anchor, wantArms)
	}
	return strings.ReplaceAll(sql, anchor,
		anchor+" AND router_ip = toIPv6('"+router+"')")
}

// orderByLeadRe pulls the first column named by a query's ORDER BY.
var orderByLeadRe = regexp.MustCompile(`(?is)ORDER\s+BY\s+([A-Za-z_][\w.]*)\s*(DESC|ASC)?`)

// A Grafana table's options.sortBy silently OVERRIDES the ORDER BY its query
// carries. A panel can therefore have a perfectly correct ranking query and
// present rows in a different order entirely, with nothing in the SQL to
// suggest it -- which is exactly what happened to the quirk detector: its
// ORDER BY was corrected to rank by spread while a stale sortBy kept the
// table sorted by message volume, so the fix was invisible in the browser and
// every SQL-level test still passed.
//
// This walks every committed dashboard rather than naming panels, so a table
// that gains a sort later is covered without anyone remembering.
func TestTableSortDoesNotContradictItsOwnQuery(t *testing.T) {
	var checked int
	for _, dash := range allDashboards {
		path := filepath.Join("..", "deploy", "grafana", "dashboards", dash+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var d struct {
			Panels []struct {
				Title   string `json:"title"`
				Options struct {
					SortBy []struct {
						DisplayName string `json:"displayName"`
						Desc        bool   `json:"desc"`
					} `json:"sortBy"`
				} `json:"options"`
				Targets []struct {
					RawSQL string `json:"rawSql"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, p := range d.Panels {
			if len(p.Options.SortBy) == 0 || len(p.Targets) == 0 {
				continue
			}
			sql := p.Targets[0].RawSQL
			m := orderByLeadRe.FindStringSubmatch(sql)
			if m == nil {
				t.Errorf("%s panel %q sorts on %q but its query has no ORDER "+
					"BY; the panel's order is then entirely Grafana's and the "+
					"query says nothing about it", dash, p.Title,
					p.Options.SortBy[0].DisplayName)
				continue
			}
			checked++
			wantCol, wantDesc := m[1], strings.EqualFold(m[2], "DESC")
			got := p.Options.SortBy[0]
			if got.DisplayName != wantCol {
				t.Errorf("%s panel %q: the table sorts on %q while the query "+
					"orders by %q. The table sort wins, so the query's "+
					"ranking never reaches the screen.",
					dash, p.Title, got.DisplayName, wantCol)
			}
			if got.Desc != wantDesc {
				t.Errorf("%s panel %q: the table sorts %s while the query "+
					"orders %s on %q -- the ranking is inverted on screen",
					dash, p.Title, descWord(got.Desc), descWord(wantDesc), wantCol)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no panel with a table sort was checked; the failure mode of " +
			"a discovery-based check is discovering zero")
	}
}

func descWord(desc bool) string {
	if desc {
		return "descending"
	}
	return "ascending"
}

// linkStateNodesCoverageStatTitle is link-state-nodes' "Carried by the
// current session" panel's title. It carries the repeat variable itself,
// "$collector", so Grafana substitutes a concrete collector into each
// repeated tile's heading -- without it, a dual-homed router's two tiles
// would read the identical heading with two different, unattributed numbers
// beside it, which is indistinguishable from a contradiction.
//
// linkStateLinksCoverageStatTitle and linkStatePrefixesCoverageStatTitle are
// its siblings, added when those two dashboards' stats got the same
// treatment. All three read the same text today, but each dashboard gets its
// own named constant rather than one shared one, which is why lsCoverage
// takes the title as a parameter rather than assuming one string fits all
// three siblings: a future rename of a single dashboard's panel needs to
// touch only its own constant.
const linkStateNodesCoverageStatTitle = "Carried by the current session — $collector"

// linkStateLinksCoverageStatTitle is link-state-links' equivalent of
// linkStateNodesCoverageStatTitle -- see its comment.
const linkStateLinksCoverageStatTitle = "Carried by the current session — $collector"

// linkStatePrefixesCoverageStatTitle is link-state-prefixes' equivalent of
// linkStateNodesCoverageStatTitle -- see its comment.
const linkStatePrefixesCoverageStatTitle = "Carried by the current session — $collector"

// lsCoverage runs one of the three sibling dashboards' coverage stat, which
// says how much of what the lab archive holds for this router is in its CURRENT
// BMP session. title is the panel's exact title on that dashboard -- see
// linkStateNodesCoverageStatTitle's comment for why this varies by sibling.
func lsCoverage(t *testing.T, ctx context.Context, ch *ClickHouse, dashboard, title string) (current, window uint64) {
	t.Helper()
	raw := panelSQL(t, dashboard, title, "A")
	sql := substituteGrafana(raw, topologyVars("0", "0, 1"))
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&current, &window); err != nil {
		t.Fatalf("%s coverage stat: %v\nSQL:\n%s", dashboard, err, sql)
	}
	return current, window
}

// Three of the four routers carrying link-state data in the lab archive have
// ZERO objects in their current BMP session -- one holds 86 nodes and shows
// none, another holds 19 and shows none. The Nodes, Links and Prefixes tables
// all scope to the current session, so those routers render a blank table
// that is indistinguishable from "this router has no topology".
//
// link-state-topology already diagnoses this with its completeness table.
// These three carried the same scoping and no such signal, which is what this
// stat is for: a blank table underneath a stat reading 0 of 12 is a router
// that has not re-dumped since it reconnected, and says so.
func TestLinkStateCoverageStatSeparatesCurrentSessionFromWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	for _, tc := range []struct {
		dashboard string
		title     string
		current   uint64
	}{
		// The fixture's current session carries four nodes and four edges;
		// its stale session carries objects these must NOT count.
		{"link-state-nodes", linkStateNodesCoverageStatTitle, 5},
		{"link-state-links", linkStateLinksCoverageStatTitle, 4},
		// The third sibling, which this table did not reach until
		// 2026-09-19. Two prefixes under $peer in the current session --
		// 10.199.1.0/24 on node a and 10.199.2.0/24 on node b. The prefix
		// under dashPeerIPB belongs to the OTHER peer and the stale session's
		// 10.199.9.0/24 belongs to the other session, so both are excluded
		// here and only the second one widens the window below.
		{"link-state-prefixes", linkStatePrefixesCoverageStatTitle, 2},
	} {
		t.Run(tc.dashboard, func(t *testing.T) {
			current, window := lsCoverage(t, ctx, ch, tc.dashboard, tc.title)
			if current != tc.current {
				t.Errorf("%s current = %d, want %d -- the stale session's "+
					"objects must not be counted as current",
					tc.dashboard, current, tc.current)
			}
			if window <= current {
				t.Errorf("%s window = %d and current = %d; the window must be "+
					"WIDER, or the stat cannot show a router that stopped "+
					"re-dumping -- which is the only thing it exists for",
					tc.dashboard, window, current)
			}
		})
	}
}

// The stat is the explanation for an empty table, so it has to survive the
// case it explains: a router whose current session carries nothing at all
// must read 0 against a non-zero window, not fail and not read the window
// figure in both halves.
func TestLinkStateCoverageStatReadsZeroForARouterThatNeverRedumped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertQuietReconnectFixture(t, ctx, ch)

	raw := panelSQL(t, "link-state-nodes", linkStateNodesCoverageStatTitle, "A")
	sql := substituteGrafana(raw, topologyVarsOn(quietRouterIP, quietPeerIP, "0", "0, 1"))
	var current, window uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(&current, &window); err != nil {
		t.Fatalf("coverage stat for the stale-only router: %v\nSQL:\n%s", err, sql)
	}
	if current != 0 {
		t.Errorf("current = %d, want 0 -- this router's only nodes belong to "+
			"a superseded session", current)
	}
	if window == 0 {
		t.Errorf("window = 0; the router HAS nodes in the lab archive, and a stat " +
			"reading 0 of 0 says \"nothing to see\" where the truth is " +
			"\"nothing re-dumped\"")
	}
}

const (
	quietRouterIP = "10.0.199.17"
	quietPeerIP   = "10.255.199.21"
	// The OLD session carries the link-state; the NEW one carries only a
	// Peer Up. Both are inside the test window, which is what separates this
	// from insertOldSessionRouterFixture -- that router's whole history sits
	// outside the window, so both halves of the coverage stat read zero and
	// it cannot distinguish "nothing here" from "nothing re-dumped".
	quietOldSession = 9971
	quietNewSession = 9972
)

// insertQuietReconnectFixture writes the shape three of the four link-state
// routers in the lab archive actually have: nodes advertised in one BMP
// session, then a reconnect that has not re-dumped anything. One holds 86
// nodes and shows none; another holds 19 and shows none.
//
// Every panel on the Nodes, Links and Prefixes dashboards scopes to the
// current session, so this router renders three blank tables. The coverage
// stat is what tells an operator that blank means "has not re-dumped" rather
// than "has no topology", and this fixture is the only place that claim can
// be tested.
func insertQuietReconnectFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	env := func(session uint64, ts time.Time, payload *vantagev1.LsEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: quietRouterIP, SysName: "dash-quiet"},
			Peer:        &vantagev1.PeerId{Ip: quietPeerIP},
			SessionId:   session,
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Ls{Ls: payload},
		}
	}
	peerUp := func(session uint64, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: quietRouterIP, SysName: "dash-quiet"},
			Peer:        &vantagev1.PeerId{Ip: quietPeerIP},
			SessionId:   session,
			TsCollector: timestamppb.New(ts),
			Payload: &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{
				Kind: vantagev1.PeerEvent_KIND_UP,
			}},
		}
	}
	now := corpusCollectorClock
	q1 := []byte{10, 255, 21, 1}
	q2 := []byte{10, 255, 21, 2}
	q3 := []byte{10, 255, 21, 3}

	envs := []*vantagev1.Envelope{
		peerUp(quietOldSession, now.Add(-3*time.Hour)),
		env(quietOldSession, now.Add(-3*time.Hour), &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 210, Local: lsDesc(q1), Name: "quiet-1"},
				{Protocol: 3, Identifier: 210, Local: lsDesc(q2), Name: "quiet-2"},
				{Protocol: 3, Identifier: 210, Local: lsDesc(q3), Name: "quiet-3"},
			},
		}),
		// The reconnect. Newer Peer Up, and nothing else at all.
		peerUp(quietNewSession, now.Add(-time.Hour)),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert quiet-reconnect fixture envelope %d: %v", i, err)
		}
	}
}

// The L3VPN RIB browser answers a question no other dashboard asks: what
// label is a router advertising for this prefix in this VRF, and via which
// next hop. Labels sit on all 164 rows of the lab archive and appear in no
// panel today, so nothing about how they behave has ever been pinned.
//
// Two shapes here have no counterpart in that lab archive, and both are what the
// panels are specified against:
//
//   - rbPfxRelabel is re-advertised in a SECOND session under a DIFFERENT
//     label and a DIFFERENT next hop. 11 of the lab archive's 31 keys do carry
//     more than one label across their lifetime, but every one of those
//     changes coincides with a session restart and no key is ever
//     re-observed inside one session, so "the current label" is undecidable
//     there. Here the newest observation is a different row from the oldest
//     and a panel that reaches for any() instead of argMax() is caught.
//   - rbPfxNoRT is a vpn4 route carrying NO route target, which is the shape
//     34 of the lab archive's 90 vpn4 rows have: they were captured before the
//     extended-communities decoder landed on 2026-08-10. A panel that reaches
//     its rows THROUGH route_targets drops every one of them silently.
const (
	rbRouterA = "10.0.196.9"
	rbRouterB = "10.0.196.10"
	// rbRouterDual is ONE router watched by TWO collectors, and is the only
	// router here that can tell a per-collector answer from a merged one.
	// Everything else in this fixture carries collector c1 alone, so every
	// existing assertion passes whether these panels resolve collectors or
	// not -- the one-branch fixture problem that left several related
	// defects unfalsifiable when they were first found.
	//
	// route_vpn is dual-collector on the LAB archive (178 rows to 14), so
	// this is the shipped shape rather than a hypothetical.
	rbRouterDual     = "10.0.196.11"
	rbDualCollectorB = "c2"
	rbPeer           = "10.255.196.13"

	// Two sessions on router A, because the label and next hop this
	// dashboard reports are the newest ones, and one session cannot say
	// which of two observations is newer in a way that survives a re-dump.
	rbSession1 = 9421
	rbSession2 = 9422
	rbSessionB = 9423
	// The dual-homed router's sessions -- one per collector, which is the
	// whole point: session_id is minted per collector, so a merged
	// uniqExact(session_id) reports two BMP sessions where the router had
	// one.
	rbSessionDual1 = 9424
	rbSessionDualB = 9425

	rbRDa = "65000:51"
	rbRDb = "65000:52"
	// rbRDLoc exists only in the loc_rib stream, so a panel that stopped
	// reading $rib renders identically to one that never did.
	rbRDLoc = "65000:53"

	rbRTa = "65000:510"
	rbRTb = "65000:520"
)

const (
	// rbPfxRelabel: same route, two sessions, two different labels and two
	// different next hops. The only key here where "latest" and "any" differ.
	rbPfxRelabel = "10.206.1.0/24"
	// rbPfxTwoRDs is exported by two VRFs from one router: two rows, not one.
	rbPfxTwoRDs = "10.206.2.0/24"
	// rbPfxNoRT is vpn4 with no extended communities at all.
	rbPfxNoRT = "10.206.3.0/24"
	// rbPfxTwoRTs carries TWO route targets, so the route-target panel has
	// to expand the array rather than render it.
	rbPfxTwoRTs = "10.206.4.0/24"
	// rbPfxLu is labeled unicast: no Route Distinguisher, by design.
	rbPfxLu = "10.206.5.1/32"
	// rbPfxWithdrawn is advertised and then withdrawn inside one session.
	rbPfxWithdrawn = "10.206.6.0/24"
	// rbPfxLocRib is the only route outside in_pre.
	rbPfxLocRib = "10.206.7.0/24"
	// rbPfxTwoRouters is carried by both routers under the same RD.
	rbPfxTwoRouters = "10.206.8.0/24"
	// rbPfxNoPath is learned over iBGP, so it carries no AS path and has no
	// origin ASN to report. 142 of the lab archive's 164 labeled rows are this
	// shape, and as_path[-1] renders every one of them as AS 0.
	rbPfxNoPath = "10.206.9.0/24"

	// The two dual-homed routes, split here into their two dangerous halves.
	//
	// rbPfxDualState is THE DANGEROUS ONE: the collector that saw the most
	// holds two advertisements, and the collector that saw the least holds a
	// single WITHDRAWAL stamped later. A panel that resolves `state` with
	// argMax(..., ts_collector) across collectors reads the lagging
	// collector's withdrawal as the route's current state -- an operator
	// sees a route the network is really carrying, marked gone. That is the
	// same failure the looking glass was fixed for, arriving by a different
	// route.
	rbPfxDualState = "10.206.10.0/24"
	// rbPfxDualLabel is the quieter half: both collectors say the route is
	// advertised and they disagree about its LABEL and NEXT HOP, with the
	// lagging collector's observation stamped later. Every one of the four
	// argMax columns comes from whichever collector's wall clock ran ahead,
	// so a row can carry one collector's label beside another's next hop.
	rbPfxDualLabel = "10.206.11.0/24"
	// rbPfxDualTie is the only route here whose two collectors saw the SAME
	// NUMBER of observations. It judges the second element of the
	// (observations, collector_id) ordering: without it the winner
	// comparison matches BOTH collectors' rows and the table renders one
	// route twice.
	//
	// The other two dual routes cannot see that -- their collectors are 2
	// and 1, so the comparison picks a single winner however it is spelled,
	// and the tie-break mutation survived them both. Found by running it.
	rbPfxDualTie = "10.206.12.0/24"
)

const (
	rbLabelOld = 24501
	rbLabelNew = 24502
	// The dual-homed router's two views of one route. Different values, so
	// "came from the chosen collector" and "came from the other one" are
	// distinguishable rather than a coincidence.
	rbLabelDualChosen    = 24520
	rbLabelDualLagging   = 24521
	rbNextHopOld         = "10.206.0.1"
	rbNextHopNew         = "10.206.0.2"
	rbNextHopDualChosen  = "10.206.0.3"
	rbNextHopDualLagging = "10.206.0.4"
	// rbWithdrawSentinel is the RFC 3107/8277 label stack a router sends in
	// place of a real label when withdrawing (0x800000 >> 4). The proto
	// documents it as "not a real label, and must be ignored", and both of
	// the lab archive's withdrawals carry it.
	rbWithdrawSentinel = 524288
	// rbOriginASN is the last ASN in every as_path here, which is what
	// asn-view established as the origin (as_path[-1]).
	rbOriginASN = 64700
)

// insertRibBrowserFixture writes the labeled routes the L3VPN RIB browser is
// specified against, through the real RowsFor + Insert path, under reserved
// identifiers (testDB is shared and never truncated).
func insertRibBrowserFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	vpn4 := &vantagev1.Family{Afi: 1, Safi: 128}
	lu4 := &vantagev1.Family{Afi: 1, Safi: 4}
	env := func(router string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		sys := "dash-rib1"
		if router == rbRouterB {
			sys = "dash-rib2"
		}
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: rbPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	// rts is variadic rather than a single string because rbPfxTwoRTs needs
	// two, and passing none is the pre-decoder shape rather than an omission.
	attrs := func(nextHop string, rts ...string) *vantagev1.PathAttributes {
		a := &vantagev1.PathAttributes{
			Origin: 0, NextHop: nextHop,
			AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000, rbOriginASN}}},
		}
		for _, rt := range rts {
			a.ExtendedCommunities = append(a.ExtendedCommunities,
				&vantagev1.ExtCommunity{Type: 0x00, SubType: 0x02, Value: rt})
		}
		return a
	}
	locRib := func(router string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(router, session, ts, r)
		e.Peer = &vantagev1.PeerId{
			Ip: rbPeer, Asn: 65000,
			Type: vantagev1.PeerType_PEER_TYPE_LOC_RIB,
		}
		return e
	}
	adv := func(fam *vantagev1.Family, prefix, rd string, label uint32, nextHop string, rts ...string) *vantagev1.RouteEvent {
		return &vantagev1.RouteEvent{
			Family: fam, Attrs: attrs(nextHop, rts...),
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: prefix, Rd: rd, Labels: []uint32{label}},
			},
		}
	}
	now := corpusCollectorClock

	envs := []*vantagev1.Envelope{
		// The relabel: older session first, then a newer one carrying a
		// different label AND a different next hop.
		env(rbRouterA, rbSession1, now.Add(-40*time.Minute),
			adv(vpn4, rbPfxRelabel, rbRDa, rbLabelOld, rbNextHopOld, rbRTa)),
		env(rbRouterA, rbSession2, now.Add(-5*time.Minute),
			adv(vpn4, rbPfxRelabel, rbRDa, rbLabelNew, rbNextHopNew, rbRTa)),

		// One prefix, two VRFs, one router.
		env(rbRouterA, rbSession1, now, adv(vpn4, rbPfxTwoRDs, rbRDa, 24503, rbNextHopOld, rbRTa)),
		env(rbRouterA, rbSession1, now, adv(vpn4, rbPfxTwoRDs, rbRDb, 24504, rbNextHopOld, rbRTb)),

		// vpn4 with no extended communities at all: the pre-decoder shape.
		env(rbRouterA, rbSession1, now, adv(vpn4, rbPfxNoRT, rbRDa, 24505, rbNextHopOld)),

		// Two route targets on one route.
		env(rbRouterA, rbSession1, now, adv(vpn4, rbPfxTwoRTs, rbRDb, 24506, rbNextHopOld, rbRTa, rbRTb)),

		// Labeled unicast: no RD.
		env(rbRouterA, rbSession1, now, adv(lu4, rbPfxLu, "", 24507, rbNextHopOld)),

		// Advertised, then withdrawn, inside one session.
		env(rbRouterA, rbSession1, now.Add(-20*time.Minute),
			adv(vpn4, rbPfxWithdrawn, rbRDa, 24508, rbNextHopOld, rbRTa)),
		env(rbRouterA, rbSession1, now.Add(-15*time.Minute), &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs(rbNextHopOld, rbRTa),
			VpnWithdrawn: []*vantagev1.VpnPrefix{
				{Prefix: rbPfxWithdrawn, Rd: rbRDa, Labels: []uint32{rbWithdrawSentinel}},
			},
		}),

		// Learned over iBGP: no AS path, so no origin ASN exists to report.
		env(rbRouterA, rbSession1, now, &vantagev1.RouteEvent{
			Family: vpn4,
			Attrs: &vantagev1.PathAttributes{
				Origin: 0, NextHop: rbNextHopOld,
				ExtendedCommunities: []*vantagev1.ExtCommunity{
					{Type: 0x00, SubType: 0x02, Value: rbRTa},
				},
			},
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: rbPfxNoPath, Rd: rbRDa, Labels: []uint32{24512}},
			},
		}),

		// The only route outside in_pre.
		locRib(rbRouterA, rbSession1, now, adv(vpn4, rbPfxLocRib, rbRDLoc, 24509, rbNextHopOld, rbRTa)),

		// Same prefix and RD from two routers: two rows, not one.
		env(rbRouterA, rbSession1, now, adv(vpn4, rbPfxTwoRouters, rbRDa, 24510, rbNextHopOld, rbRTa)),
		env(rbRouterB, rbSessionB, now, adv(vpn4, rbPfxTwoRouters, rbRDa, 24511, rbNextHopOld, rbRTa)),
	}

	// The dual-homed router, appended LAST so nothing above is renumbered.
	//
	// In BOTH routes the chosen collector holds TWO observations and the
	// lagging one holds a SINGLE observation stamped LATER. That crossing is
	// the whole design: it makes "resolved within the winning collector" and
	// "resolved by whichever clock ran ahead" give different answers, where
	// a lagging collector that was merely quieter would not.
	//
	// Both carry rbRTa and rbRDa deliberately -- reusing the existing target
	// and VRF keeps the route-target and VRF counts unmoved, so the numbers
	// that shift when this router lands are only the ones about ROUTES.
	dualEnv := func(collector string, session uint64, ts time.Time, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := env(rbRouterDual, session, ts, r)
		e.CollectorId = collector
		e.Router = &vantagev1.RouterId{Ip: rbRouterDual, SysName: "dash-rib3"}
		return e
	}
	dual := []*vantagev1.Envelope{
		// rbPfxDualState: advertised twice by the collector that saw the
		// most, withdrawn once -- later -- by the one that saw the least.
		dualEnv("c1", rbSessionDual1, now.Add(-30*time.Minute),
			adv(vpn4, rbPfxDualState, rbRDa, rbLabelDualChosen, rbNextHopDualChosen, rbRTa)),
		dualEnv("c1", rbSessionDual1, now.Add(-29*time.Minute),
			adv(vpn4, rbPfxDualState, rbRDa, rbLabelDualChosen, rbNextHopDualChosen, rbRTa)),
		dualEnv(rbDualCollectorB, rbSessionDualB, now.Add(-1*time.Minute), &vantagev1.RouteEvent{
			Family: vpn4, Attrs: attrs(rbNextHopDualLagging, rbRTa),
			VpnWithdrawn: []*vantagev1.VpnPrefix{
				{Prefix: rbPfxDualState, Rd: rbRDa, Labels: []uint32{rbWithdrawSentinel}},
			},
		}),
		// rbPfxDualLabel: both say advertised, and they disagree about the
		// label and the next hop.
		dualEnv("c1", rbSessionDual1, now.Add(-30*time.Minute),
			adv(vpn4, rbPfxDualLabel, rbRDa, rbLabelDualChosen, rbNextHopDualChosen, rbRTa)),
		dualEnv("c1", rbSessionDual1, now.Add(-29*time.Minute),
			adv(vpn4, rbPfxDualLabel, rbRDa, rbLabelDualChosen, rbNextHopDualChosen, rbRTa)),
		dualEnv(rbDualCollectorB, rbSessionDualB, now.Add(-1*time.Minute),
			adv(vpn4, rbPfxDualLabel, rbRDa, rbLabelDualLagging, rbNextHopDualLagging, rbRTa)),
		// rbPfxDualTie: TWO observations each, disagreeing about the label.
		// "c2" sorts after "c1", so the documented tie-break names a
		// specific winner rather than "either one".
		dualEnv("c1", rbSessionDual1, now.Add(-30*time.Minute),
			adv(vpn4, rbPfxDualTie, rbRDa, rbLabelDualChosen, rbNextHopDualChosen, rbRTa)),
		dualEnv("c1", rbSessionDual1, now.Add(-29*time.Minute),
			adv(vpn4, rbPfxDualTie, rbRDa, rbLabelDualChosen, rbNextHopDualChosen, rbRTa)),
		dualEnv(rbDualCollectorB, rbSessionDualB, now.Add(-28*time.Minute),
			adv(vpn4, rbPfxDualTie, rbRDa, rbLabelDualLagging, rbNextHopDualLagging, rbRTa)),
		dualEnv(rbDualCollectorB, rbSessionDualB, now.Add(-27*time.Minute),
			adv(vpn4, rbPfxDualTie, rbRDa, rbLabelDualLagging, rbNextHopDualLagging, rbRTa)),
	}
	envs = append(envs, dual...)
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert rib-browser fixture envelope %d: %v", i, err)
		}
	}
}

// scopeRibBrowserToFixtureRouters narrows a browser query to the two routers
// this fixture owns, by extending the panel's own time filter. testDB is
// shared, the dashboard has no $router control, and every count below is a
// claim about this fixture rather than about the rest of the corpus.
func scopeRibBrowserToFixtureRouters(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("a rib-browser query carries %d occurrences of %q, want 1 -- "+
			"these tests scope to the fixture's routers by extending that "+
			"filter, and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		anchor+" AND router_ip IN (toIPv6('"+rbRouterA+"'), toIPv6('"+rbRouterB+
			"'), toIPv6('"+rbRouterDual+"'))", 1)
}

// rbRouteRow is one row of the committed "Routes" table.
type rbRouteRow struct {
	family    string
	rd        string
	prefix    string
	label     string
	nextHop   string
	originASN string
	state     string
	router    string
	sessions  uint64
	lastSeen  time.Time
}

// ribRoutes runs the committed Routes table with $rd, $family and $rib
// rendered as Grafana would render them, keyed by (rd, prefix, router) so a
// merged row shows up as a missing key rather than as a count that happens
// to look right.
func ribRoutes(t *testing.T, ctx context.Context, ch *ClickHouse, rd, family, rib string) map[string]rbRouteRow {
	t.Helper()
	raw := scopeRibBrowserToFixtureRouters(t, panelSQL(t, "l3vpn-rib-browser", "Routes", "A"))
	sql := substituteGrafana(raw, map[string]string{"rd": rd, "family": family, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("rib routes (rd=%q family=%q rib=%q): %v\nSQL:\n%s", rd, family, rib, err, sql)
	}
	defer rows.Close()
	out := map[string]rbRouteRow{}
	for rows.Next() {
		var r rbRouteRow
		if err := rows.Scan(&r.family, &r.rd, &r.prefix, &r.label, &r.nextHop,
			&r.originASN, &r.state, &r.router, &r.sessions, &r.lastSeen); err != nil {
			t.Fatalf("scan rib route row: %v", err)
		}
		out[r.rd+"|"+r.prefix+"|"+r.router] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rib route rows: %v", err)
	}
	return out
}

// The fixture's arrangement is what makes the assertions below decidable, so
// it is pinned rather than trusted -- particularly the two shapes the lab
// archive cannot supply.
func TestRibBrowserFixtureIsArrangedAdversarially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	scope := " AND router_ip IN (toIPv6('" + rbRouterA + "'), toIPv6('" + rbRouterB + "'))"

	// Exactly one key whose label changes over its lifetime. Without it,
	// argMax(labels, ts_collector) and any(labels) return the same value
	// everywhere and a panel cannot be caught using the wrong one.
	var relabelled uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, `
		SELECT countIf(lbls > 1) FROM (
			SELECT rd, prefix, router_ip, uniqExact(labels) AS lbls
			FROM vantage.route_vpn FINAL
			WHERE is_withdraw = 0`+scope+`
			GROUP BY rd, prefix, router_ip
		)`)).Scan(&relabelled); err != nil {
		t.Fatalf("relabelled keys: %v", err)
	}
	if relabelled != 1 {
		t.Errorf("%d keys carry more than one ADVERTISED label, want 1 (%s) -- every "+
			"multi-label key in the lab archive changes label only across a "+
			"session restart, so this fixture is the only place 'newest' and "+
			"'any' can be told apart", relabelled, rbPfxRelabel)
	}

	// A vpn4 route with no route target at all: the shape 34 of the lab archive's
	// 90 vpn4 rows have, because they predate the ext-communities decoder.
	var noRT uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.route_vpn FINAL WHERE prefix = '"+rbPfxNoRT+
			"' AND family = 'vpn4' AND empty(route_targets)"+scope)).Scan(&noRT); err != nil {
		t.Fatalf("RT-less vpn4 route: %v", err)
	}
	if noRT != 1 {
		t.Errorf("%s has %d RT-less vpn4 observations, want 1 -- without it "+
			"nothing catches a panel that reaches its rows through "+
			"route_targets", rbPfxNoRT, noRT)
	}

	// A route carrying TWO route targets, so the RT panel must expand the
	// array rather than render it.
	var rts uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT length(any(route_targets)) FROM vantage.route_vpn FINAL WHERE prefix = '"+
			rbPfxTwoRTs+"'"+scope)).Scan(&rts); err != nil {
		t.Fatalf("two-RT route: %v", err)
	}
	if rts != 2 {
		t.Errorf("%s carries %d route targets, want 2", rbPfxTwoRTs, rts)
	}

	// An lu4 route whose RD is empty BY DESIGN (bgp/vpn.go). The $rd filter
	// has to keep it, and only a real empty RD proves that.
	var luRD string
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT any(rd) FROM vantage.route_vpn FINAL WHERE prefix = '"+
			rbPfxLu+"'"+scope)).Scan(&luRD); err != nil {
		t.Fatalf("lu4 rd: %v", err)
	}
	if luRD != "" {
		t.Errorf("%s has rd %q, want empty", rbPfxLu, luRD)
	}
}

// The one question this dashboard exists to answer. A route re-advertised
// under a new label in a later session has TWO labels in the table, and the
// row must report the newest -- any() would return either, and on the lab
// archive both answers look equally plausible because 11 of its 31 keys carry
// more than one label.
func TestRibBrowserRoutesShowTheNewestLabelAndNextHop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	got, ok := rows[rbRDa+"|"+rbPfxRelabel+"|dash-rib1"]
	if !ok {
		t.Fatalf("no row for %s in %s; got %v", rbPfxRelabel, rbRDa, sortedKeys(rows))
	}
	if got.label != fmt.Sprint(rbLabelNew) {
		t.Errorf("label = %q, want %q -- %d is the label from the OLDER "+
			"session, so the panel is not resolving on the collector clock",
			got.label, fmt.Sprint(rbLabelNew), rbLabelOld)
	}
	if got.nextHop != rbNextHopNew {
		t.Errorf("next hop = %q, want %q (the older observation says %q)",
			got.nextHop, rbNextHopNew, rbNextHopOld)
	}
	if got.sessions != 2 {
		t.Errorf("sessions = %d, want 2", got.sessions)
	}
}

// One prefix exported by two VRFs is two routes. A browser that drops rd from
// its key merges them and reports one label for both.
func TestRibBrowserKeepsTwoRDsOfOnePrefixApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	a, okA := rows[rbRDa+"|"+rbPfxTwoRDs+"|dash-rib1"]
	b, okB := rows[rbRDb+"|"+rbPfxTwoRDs+"|dash-rib1"]
	if !okA || !okB {
		t.Fatalf("%s appears under %v, want both %s and %s",
			rbPfxTwoRDs, sortedKeys(rows), rbRDa, rbRDb)
	}
	if a.label == b.label {
		t.Errorf("both VRFs report label %q -- the two rows are being fed "+
			"from one group", a.label)
	}
}

// Two routers advertising the same prefix under the same RD are two rows.
// Collapsing them would hide a label disagreement between them, which is
// exactly what an operator opens this dashboard to find.
func TestRibBrowserKeepsTwoRoutersOfOneRouteApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	a, okA := rows[rbRDa+"|"+rbPfxTwoRouters+"|dash-rib1"]
	b, okB := rows[rbRDa+"|"+rbPfxTwoRouters+"|dash-rib2"]
	if !okA || !okB {
		t.Fatalf("%s appears under %v, want a row from each router",
			rbPfxTwoRouters, sortedKeys(rows))
	}
	if a.label == b.label {
		t.Errorf("both routers report label %q, want the two they actually "+
			"advertised", a.label)
	}
}

// A withdrawn route is not in the RIB, but dropping its row silently is how a
// dashboard tells an operator a route never existed. It is marked instead.
func TestRibBrowserMarksWithdrawnRoutesRatherThanDropping(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	got, ok := rows[rbRDa+"|"+rbPfxWithdrawn+"|dash-rib1"]
	if !ok {
		t.Fatalf("%s was dropped; rows are %v", rbPfxWithdrawn, sortedKeys(rows))
	}
	if got.state != "withdrawn" {
		t.Errorf("state = %q, want %q -- the route was advertised and then "+
			"withdrawn inside one session, and the newest observation is the "+
			"withdrawal", got.state, "withdrawn")
	}
	adv, ok := rows[rbRDa+"|"+rbPfxTwoRouters+"|dash-rib1"]
	if !ok || adv.state != "advertised" {
		t.Errorf("a live route reports state %q, want %q -- if every row says "+
			"the same thing the column carries nothing", adv.state, "advertised")
	}
}

// 34 of the lab archive's 90 vpn4 rows carry no route target, because they were
// captured before the ext-communities decoder landed. A Routes panel that
// reaches its rows through route_targets -- an arrayJoin, or a join against
// the RT panel's query -- drops every one of them and reports a third of the
// L3VPN table as absent.
func TestRibBrowserRoutesReachRowsWithNoRouteTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	if _, ok := rows[rbRDa+"|"+rbPfxNoRT+"|dash-rib1"]; !ok {
		t.Errorf("%s is missing from %v -- it is a vpn4 route with no route "+
			"target, which is the shape a third of the lab archive has",
			rbPfxNoRT, sortedKeys(rows))
	}
}

// sortedKeys renders a row map's keys for a failure message, in a stable
// order so two runs of a failing test read the same.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scopeRibPickerToFixtureRouters narrows the VRF picker's own query to this
// fixture's routers. The picker carries no time filter, so it cannot be
// scoped the way the panels are.
func scopeRibPickerToFixtureRouters(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "WHERE match(toString(rib)"
	if !strings.Contains(sql, anchor) {
		t.Fatalf("the VRF picker does not start its WHERE with %q, so this "+
			"test cannot scope it to the fixture:\n%s", anchor, sql)
	}
	return strings.Replace(sql, anchor,
		"WHERE router_ip IN (toIPv6('"+rbRouterA+"'), toIPv6('"+rbRouterB+
			"')) AND match(toString(rib)", 1)
}

// The VRF picker is the control this dashboard is built around, and it has
// the one property that is easy to get wrong: labeled unicast has no Route
// Distinguisher, so the option that selects it has an EMPTY value. A picker
// written to exclude the empty Route Distinguisher looks tidier and hides 74
// of the lab archive's 164 labeled routes.
//
// The column order is asserted too. Grafana's ClickHouse plugin takes a query
// variable's FIRST column as the value and its SECOND as the label, ignoring
// the __value/__text names entirely -- the same rule pinned for evpn-churn.
func TestRibBrowserVrfPickerOffersTheEmptyRouteDistinguisher(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	vars := dashboardQueryVariables(t, "l3vpn-rib-browser")
	q, ok := vars["rd"]
	if !ok {
		t.Fatalf("l3vpn-rib-browser declares no query variable named rd; it declares %v",
			slices.Sorted(maps.Keys(vars)))
	}
	scoped := scopeRibPickerToFixtureRouters(t, q)
	sql := substituteGrafana(scoped, map[string]string{"family": ".*", "rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("VRF picker query: %v\nSQL:\n%s", err, sql)
	}
	offered := map[string]string{}
	for rows.Next() {
		var value, label string
		if err := rows.Scan(&value, &label); err != nil {
			rows.Close()
			t.Fatalf("scan picker option: %v -- the picker must return the "+
				"value first and the label second", err)
		}
		offered[value] = label
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("picker rows: %v", err)
	}

	label, ok := offered[""]
	if !ok {
		t.Fatalf("the picker offers no option with an empty value, so no "+
			"selection reaches %s and the 74 lu4 routes in the lab archive are "+
			"unreachable; it offers %v", rbPfxLu, slices.Sorted(maps.Keys(offered)))
	}
	if label == "" {
		t.Error("the empty Route Distinguisher is offered with an empty " +
			"label, which renders as a blank row in the picker next to real " +
			"RDs -- it needs the spelled-out fallback")
	}
	for _, rd := range []string{rbRDa, rbRDb, rbRDLoc} {
		if _, ok := offered[rd]; !ok {
			t.Errorf("the picker does not offer %s; it offers %v",
				rd, slices.Sorted(maps.Keys(offered)))
		}
	}
	// Every option must render rows, which is how $focus shipped broken.
	for value := range offered {
		if got := ribRoutes(t, ctx, ch, value, ".*", ".*"); len(got) == 0 {
			t.Errorf("the picker offers %q and the Routes table renders "+
				"nothing for it", value)
		}
	}
}

// Selecting a VRF has to narrow the table to that VRF, including when the VRF
// selected is the one with no Route Distinguisher at all.
func TestRibBrowserVrfNarrowsToTheChosenVrf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	onlyA := ribRoutes(t, ctx, ch, rbRDa, ".*", ".*")
	if len(onlyA) == 0 {
		t.Fatalf("selecting %s renders nothing", rbRDa)
	}
	for key, row := range onlyA {
		if row.rd != rbRDa {
			t.Errorf("selecting %s renders a row from %s (%s)", rbRDa, row.rd, key)
		}
	}
	if _, ok := onlyA[rbRDb+"|"+rbPfxTwoRDs+"|dash-rib1"]; ok {
		t.Errorf("selecting %s still renders the copy of %s exported by %s",
			rbRDa, rbPfxTwoRDs, rbRDb)
	}

	// The empty Route Distinguisher: the selection that reaches lu4.
	onlyLu := ribRoutes(t, ctx, ch, "", ".*", ".*")
	if len(onlyLu) != 1 {
		t.Fatalf("selecting the empty RD renders %d rows, want 1 (%s); got %v",
			len(onlyLu), rbPfxLu, sortedKeys(onlyLu))
	}
	for _, row := range onlyLu {
		if row.prefix != rbPfxLu {
			t.Errorf("selecting the empty RD renders %s, want %s", row.prefix, rbPfxLu)
		}
	}
}

// Every route this fixture writes is in_pre except one, which is the only
// reason $rib is a control that can be caught not being read.
func TestRibBrowserRibNarrowsToTheChosenStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	inPre := ribRoutes(t, ctx, ch, ".*", ".*", "in_pre")
	if _, ok := inPre[rbRDLoc+"|"+rbPfxLocRib+"|dash-rib1"]; ok {
		t.Errorf("in_pre renders %s, which exists only in loc_rib", rbPfxLocRib)
	}
	if len(inPre) == 0 {
		t.Error("in_pre renders nothing at all")
	}

	locRib := ribRoutes(t, ctx, ch, ".*", ".*", "loc_rib")
	if len(locRib) != 1 {
		t.Fatalf("loc_rib renders %d rows, want 1 (%s); got %v",
			len(locRib), rbPfxLocRib, sortedKeys(locRib))
	}
}

// Selecting lu4 must reach the labeled-unicast route and nothing else, which
// is the branch that renders the RD fallback string.
func TestRibBrowserFamilyNarrowsToTheChosenFamily(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	lu := ribRoutes(t, ctx, ch, ".*", "lu4", ".*")
	if len(lu) != 1 {
		t.Fatalf("family lu4 renders %d rows, want 1 (%s); got %v",
			len(lu), rbPfxLu, sortedKeys(lu))
	}
	for _, row := range lu {
		if row.rd == "" {
			t.Error("the lu4 row renders a blank Route Distinguisher, which " +
				"sits in the table looking like a real RD that failed to load")
		}
		if row.family != "lu4" {
			t.Errorf("family lu4 renders a %s row", row.family)
		}
	}
	if vpn := ribRoutes(t, ctx, ch, ".*", "vpn4", ".*"); len(vpn) < 2 {
		t.Errorf("family vpn4 renders %d rows, want the rest", len(vpn))
	}
}

// rbRTRow is one row of the committed "Route targets" table.
type rbRTRow struct {
	rt       string
	vrfs     uint64
	prefixes uint64
	routers  uint64
	lastSeen time.Time
}

func ribRouteTargets(t *testing.T, ctx context.Context, ch *ClickHouse, rd, family, rib string) map[string]rbRTRow {
	t.Helper()
	raw := scopeRibBrowserToFixtureRouters(t, panelSQL(t, "l3vpn-rib-browser", "Route targets", "A"))
	sql := substituteGrafana(raw, map[string]string{"rd": rd, "family": family, "rib": rib})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("rib route targets: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	out := map[string]rbRTRow{}
	for rows.Next() {
		var r rbRTRow
		if err := rows.Scan(&r.rt, &r.vrfs, &r.prefixes, &r.routers, &r.lastSeen); err != nil {
			t.Fatalf("scan route target row: %v", err)
		}
		out[r.rt] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("route target rows: %v", err)
	}
	return out
}

// route_targets is an Array, and a VPN route commonly carries more than one.
// A panel that groups on the array renders "['65000:510','65000:520']" as if
// it were a single route target, and the two RTs can then never be selected
// or counted apart.
func TestRibBrowserRouteTargetsExpandTheArray(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rts := ribRouteTargets(t, ctx, ch, ".*", ".*", ".*")
	for _, want := range []string{rbRTa, rbRTb} {
		if _, ok := rts[want]; !ok {
			t.Fatalf("route target %s is missing; the table offers %v -- a "+
				"route carrying two RTs has to appear under both",
				want, sortedKeys(rts))
		}
	}
	for got := range rts {
		if strings.HasPrefix(got, "[") {
			t.Errorf("route target %q is a rendered array, not a route "+
				"target -- the panel is grouping on the column instead of "+
				"expanding it", got)
		}
	}
	// rbPfxTwoRTs carries both, so each RT's prefix count includes it.
	if rts[rbRTb].prefixes < 2 {
		t.Errorf("%s covers %d prefixes, want at least 2 (%s and %s)",
			rbRTb, rts[rbRTb].prefixes, rbPfxTwoRDs, rbPfxTwoRTs)
	}
}

// The route-target table cannot show a route that carries no route target,
// and on the lab archive that is a third of the vpn4 rows. The panel is allowed
// to omit them; what it is not allowed to do is leave an operator counting
// its prefixes and concluding the VRF is empty.
func TestRibBrowserRouteTargetsCoverFewerRoutesThanTheRoutesTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rts := ribRouteTargets(t, ctx, ch, ".*", ".*", ".*")
	for rt, row := range rts {
		if row.prefixes == 0 || row.routers == 0 || row.vrfs == 0 {
			t.Errorf("route target %s reports vrfs=%d prefixes=%d routers=%d; "+
				"a row with a zero in it is a row that should not be there",
				rt, row.vrfs, row.prefixes, row.routers)
		}
	}
	// The RT-less route is reachable in the Routes table and absent here.
	// Both halves matter: the first is the bug, the second is the honest gap
	// the stat panel is there to report.
	routes := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	if _, ok := routes[rbRDa+"|"+rbPfxNoRT+"|dash-rib1"]; !ok {
		t.Errorf("%s is missing from the Routes table", rbPfxNoRT)
	}
	var covered uint64
	for _, row := range rts {
		covered += row.prefixes
	}
	if covered == 0 {
		t.Fatal("no route target covers any prefix")
	}
}

// rbStatRow is the committed "What we know" stat panel, which is one row.
type rbStatRow struct {
	routes       uint64
	vrfs         uint64
	routeTargets uint64
	routers      uint64
	noRT         uint64
}

func ribStat(t *testing.T, ctx context.Context, ch *ClickHouse, rd, family, rib string) rbStatRow {
	t.Helper()
	raw := scopeRibBrowserToFixtureRouters(t, panelSQL(t, "l3vpn-rib-browser", "What we know", "A"))
	sql := substituteGrafana(raw, map[string]string{"rd": rd, "family": family, "rib": rib})
	var r rbStatRow
	if err := ch.conn.QueryRow(ctx, qualify(ch, sql)).Scan(
		&r.routes, &r.vrfs, &r.routeTargets, &r.routers, &r.noRT); err != nil {
		t.Fatalf("rib stat: %v\nSQL:\n%s", err, sql)
	}
	return r
}

// The stat counts ROUTES, not observations. This fixture writes 12 envelopes
// carrying 10 distinct (family, RD, prefix, router) routes, and on the lab
// archive those two numbers differ for every key -- 164 rows, 31 routes --
// because each BMP session re-dumps the whole table.
func TestRibBrowserStatCountsRoutesNotObservations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	got := ribStat(t, ctx, ch, ".*", ".*", ".*")
	routes := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	if got.routes != uint64(len(routes)) {
		t.Errorf("the stat reports %d routes and the table renders %d rows -- "+
			"the two panels are not counting the same thing", got.routes, len(routes))
	}
	// 14 routes from 23 observations: the eleven this fixture always had
	// (13 observations), plus rbPfxDualState and rbPfxDualLabel at three
	// observations each and rbPfxDualTie at four. Each of those three is
	// ONE route seen by two collectors, so they are also what stops this
	// assertion passing on a stat that counts observations -- now in three
	// ways rather than one.
	if got.routes != 14 {
		t.Errorf("routes = %d, want 14; this fixture writes 23 observations "+
			"of 14 routes, and a stat built on count() would say 23",
			got.routes)
	}
	if got.vrfs != 3 {
		t.Errorf("vrfs = %d, want 3 (%s, %s, %s) -- the empty Route "+
			"Distinguisher is not a VRF and must not be counted as one",
			got.vrfs, rbRDa, rbRDb, rbRDLoc)
	}
	// 3, not 2, since rbRouterDual joined -- and it counts ONCE despite
	// being watched by two collectors, because uniqExact(router_ip) carries
	// no collector_id. That column was already honest and this is what says
	// so.
	if got.routers != 3 {
		t.Errorf("routers = %d, want 3 -- %s is one router however many "+
			"collectors watch it", got.routers, rbRouterDual)
	}
	if got.routeTargets != 2 {
		t.Errorf("route targets = %d, want 2 (%s, %s) -- distinct targets, "+
			"not arrays of them", got.routeTargets, rbRTa, rbRTb)
	}
}

// The field that explains the holes. 34 of the lab archive's 90 vpn4 rows carry
// no route target because they predate the ext-communities decoder, and
// without this number an operator reads the gap between the Routes table and
// the route-target table as a network fact.
func TestRibBrowserStatReportsObservationsMissingARouteTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	got := ribStat(t, ctx, ch, ".*", ".*", ".*")
	if got.noRT != 1 {
		t.Errorf("observations with no route target = %d, want 1 (%s) -- "+
			"the lu4 route carries none either, but labelled unicast has no "+
			"route target BY DEFINITION and counting it here would report a "+
			"decoder gap that does not exist", got.noRT, rbPfxNoRT)
	}
	// Selecting lu4 alone must therefore report zero, not one.
	if lu := ribStat(t, ctx, ch, ".*", "lu4", ".*"); lu.noRT != 0 {
		t.Errorf("family lu4 reports %d observations missing a route target, "+
			"want 0", lu.noRT)
	}
}

// selectedAs returns the whole SELECT-list expression that produces one named
// column. It scans for the depth-0 comma before the "AS <col>", rather than
// taking the line the alias sits on, because an expression that wraps an
// aggregate spans several lines -- and a line-based check would report the
// aggregate as missing exactly when the expression got complicated enough to
// be worth checking.
func selectedAs(sql, col string) (string, bool) {
	alias := " AS " + col
	end := -1
	for _, m := range []string{alias + ",", alias + "\n", alias + " "} {
		if i := strings.Index(sql, m); i >= 0 && (end < 0 || i < end) {
			end = i
		}
	}
	if end < 0 {
		return "", false
	}
	depth, start := 0, 0
	for i, r := range sql[:end] {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				start = i + 1
			}
		}
	}
	return strings.TrimSpace(sql[start:end]), true
}

// Every per-observation column in the Routes table has to be resolved on the
// SAME clock, and this is asserted against the SQL rather than against a
// result because the failure it guards is not deterministic. any(next_hop)
// returns an ARBITRARY row of the group: with two observations it lands on
// the newest about half the time, so a behavioral test passes on the runs
// where the wrong function happens to be right. That is worse than no test --
// it is a green suite over a panel that reports whichever next hop the merge
// order handed it.
//
// The columns listed here are the ones that describe a single observation. A
// new one added without argMax is a new column that silently reports an
// arbitrary row, so the test fails on the addition rather than waiting for
// someone to notice the value moving.
func TestRibBrowserRoutesResolveEveryObservationColumnOnOneClock(t *testing.T) {
	sql := panelSQL(t, "l3vpn-rib-browser", "Routes", "A")

	for _, col := range []string{"label", "next_hop", "origin_asn", "state"} {
		line, ok := selectedAs(sql, col)
		if !ok {
			t.Errorf("the Routes table selects no column named %q", col)
			continue
		}
		if !strings.Contains(line, "argMax(") || !strings.Contains(line, "ts_collector") {
			t.Errorf("the Routes table's %q is not resolved with "+
				"argMax(..., ts_collector), so it reports an arbitrary "+
				"observation of the route rather than the newest one:\n%s",
				col, line)
		}
	}
	for _, banned := range []string{"any(", "anyLast(", "anyHeavy("} {
		if strings.Contains(sql, banned) {
			t.Errorf("the Routes table uses %s, which returns an arbitrary "+
				"row of the group -- on a route observed in two sessions it "+
				"is right roughly half the time, which no result-level test "+
				"can catch reliably", banned)
		}
	}
}

// A withdrawn labeled route carries 0x800000 >> 4 in place of a label. The
// proto that defines the field says so in as many words -- "not a real label,
// and must be ignored" -- and the lab archive's two withdrawals both carry
// it while no advertised route does.
//
// Rendering it puts "524288" in a column headed `label`, next to real labels
// like 24002, and an operator reading down that column has no way to know one
// of them is a protocol sentinel. It is the same defect as counting session
// re-dumps as churn: an artifact of the encoding presented as a fact about
// the network.
//
// The sentinel cannot be recognized by its value alone -- a genuine label of
// 524288 is legal, and reaches this table identically shifted -- so
// is_withdraw is the only thing that can tell them apart.
func TestRibBrowserDoesNotReportTheWithdrawSentinelAsALabel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	got, ok := rows[rbRDa+"|"+rbPfxWithdrawn+"|dash-rib1"]
	if !ok {
		t.Fatalf("%s is missing; rows are %v", rbPfxWithdrawn, sortedKeys(rows))
	}
	if strings.Contains(got.label, fmt.Sprint(rbWithdrawSentinel)) {
		t.Errorf("the withdrawn route reports label %q, which is the RFC "+
			"3107 withdraw sentinel rather than a label any router is "+
			"advertising", got.label)
	}
	// A live route still reports its real label, or the fix has just blanked
	// the column.
	live, ok := rows[rbRDa+"|"+rbPfxTwoRouters+"|dash-rib1"]
	if !ok || live.label == "" {
		t.Errorf("a live route reports label %q, want the label it advertised",
			live.label)
	}
}

// 142 of the lab archive's 164 labeled rows carry no AS path: they are iBGP
// routes, learned from an internal peer. as_path[-1] renders every one of
// them as 0, and AS 0 is reserved -- no router originates it. A column of
// zeroes reads as data rather than as absence.
func TestRibBrowserLeavesTheOriginBlankWhenThereIsNoAsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	rows := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	noPath, ok := rows[rbRDa+"|"+rbPfxNoPath+"|dash-rib1"]
	if !ok {
		t.Fatalf("%s is missing; rows are %v", rbPfxNoPath, sortedKeys(rows))
	}
	if noPath.originASN != "" {
		t.Errorf("a route with no AS path reports origin %q, want blank -- "+
			"%q is what as_path[-1] returns for an empty path, and AS 0 is "+
			"reserved", noPath.originASN, "0")
	}
	withPath, ok := rows[rbRDa+"|"+rbPfxTwoRouters+"|dash-rib1"]
	if !ok || withPath.originASN != fmt.Sprint(rbOriginASN) {
		t.Errorf("a route WITH an AS path reports origin %q, want %d -- the "+
			"column has to still carry the origins that exist",
			withPath.originASN, rbOriginASN)
	}
}

// The same defect the L3VPN RIB browser was built with, in the panel that
// shipped first. A withdrawn labeled route carries 524288 (0x800000 >> 4)
// where a label goes -- the RFC 3107 / RFC 8277 sentinel the proto calls "not
// a real label, and must be ignored" -- and the L3VPN looking glass renders
// the array verbatim, so it prints that constant in a column headed `labels`,
// on the same row as state = withdrawn. Both of the lab archive's
// withdrawals show it.
//
// The sentinel cannot be told from a genuine label of the same value, so
// is_withdraw is the only thing that distinguishes them.
func TestVpnLookingGlassDoesNotReportTheWithdrawSentinelAsALabel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertVpnLookingGlassFixture(t, ctx, ch)

	got := vpnLookingGlassRows(t, ctx, ch, "10.88.7.1", ".*", "vpn4")
	if len(got) != 1 {
		t.Fatalf("covering 10.88.7.1 returned %d rows, want 1: %v", len(got), got)
	}
	if got[0].state != "withdrawn" {
		t.Fatalf("state = %q, want withdrawn -- this test needs the withdrawal "+
			"to be the newest observation", got[0].state)
	}
	if strings.Contains(got[0].labels, fmt.Sprint(lgWithdrawSentinel)) {
		t.Errorf("the withdrawn route reports labels %q, which is the RFC "+
			"3107 withdraw sentinel rather than a label any router is "+
			"advertising", got[0].labels)
	}
	// A live route still shows its real label, or the fix has blanked the
	// column rather than fixing it.
	live := vpnLookingGlassRows(t, ctx, ch, "10.88.5.7", ".*", "vpn4")
	if len(live) == 0 || live[0].labels == "" {
		t.Errorf("a live route reports labels %q, want the label it advertised",
			live[0].labels)
	}
}

// 92 of the lab archive's 476 unicast rows carry no AS path -- they are iBGP
// routes -- and the looking glass reports their origin with as_path[-1],
// which returns 0 for an empty array. AS 0 is reserved: no router originates
// it, so a column of zeroes reads as data where there is none.
func TestLookingGlassLeavesTheOriginBlankWhenThereIsNoAsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertLookingGlassFixture(t, ctx, ch)

	got := lookingGlassRows(t, ctx, ch, "10.77.50.1", ".*")
	if len(got) != 1 {
		t.Fatalf("covering 10.77.50.1 returned %d rows, want 1: %v", len(got), got)
	}
	if got[0].asPath != "" {
		t.Fatalf("as_path = %q, want empty -- this test needs the route with "+
			"no AS path", got[0].asPath)
	}
	if got[0].originAS != "" {
		t.Errorf("a route with no AS path reports origin %q, want blank -- "+
			"that is what as_path[-1] returns for an empty path, and AS 0 is "+
			"reserved", got[0].originAS)
	}
	// A route WITH a path still reports its origin.
	withPath := lookingGlassRows(t, ctx, ch, "10.77.14.7", ".*")
	if len(withPath) == 0 || withPath[0].originAS == "" {
		t.Error("a route with an AS path reports no origin -- the column has " +
			"to still carry the origins that exist")
	}
}

// The distinction fleet-health could not draw. A BMP session ending is the
// COLLECTOR losing its view; a Peer Down is the NETWORK saying a BGP session
// dropped. They are different events with different causes, and the lab archive
// separates them completely: the five XR routers turned over 354 BMP sessions
// between them and sent not one Peer Down, while the NX-OS routers held six
// sessions and sent seventeen.
//
// "Peer up/down over time" plots both as undifferentiated event counts, so
// those 354 lost views render as a wall of healthy-looking `up` spikes. This
// fixture supplies both shapes under its own routers so the two numbers can
// be asserted independently.
const (
	// stRouterLost turns its BMP session over repeatedly and never reports a
	// peer down: the XR shape.
	stRouterLost = "10.0.195.1"
	// stRouterDown holds ONE session and reports several downs: the NX shape.
	stRouterDown = "10.0.195.2"
	// stRouterAnon sends no sysName TLV, so the fallback rendering is
	// exercised rather than assumed.
	stRouterAnon = "10.0.195.3"
	// stRouterTwinA and stRouterTwinB are two DIFFERENT routers reporting the
	// SAME sysName, on different addresses -- two sites that both called a
	// box "core1", or one re-imaged and readdressed. Every sysName in the
	// lab archive maps to exactly one address, so nothing there can catch a
	// panel that groups on the name alone and adds their session counts
	// together.
	stRouterTwinA = "10.0.195.4"
	stRouterTwinB = "10.0.195.5"
	stTwinSysName = "dash-st-twin"
	// stRouterDual is ONE router watched by TWO collectors, and is the only
	// router here that can tell a per-collector answer from a summed one.
	// Every other router in this fixture carries collector c1 alone, so
	// every assertion about them passes whether the panel groups by
	// collector or not.
	//
	// The split is lopsided and crossed, matching the Go fixture this
	// mirrors (seedSessionCountsDualHomed): the collector that heard more
	// FROM THE ROUTER holds no view_lost at all, and every view_lost row
	// belongs to the one that heard least. So best vantage point, sum and
	// first collector are three different answers, and a view_lost that
	// rode along with the chosen collector would read 0.
	stRouterDual     = "10.0.195.6"
	stDualSysName    = "dash-st-dual"
	stDualCollectorB = "c2"
	// stRouterTie is watched by two collectors that heard the SAME NUMBER
	// of router statements and agree about nothing else. It is the only
	// router here that can judge the (router_events, collector_id)
	// tie-break: with a bare argMax on the count, ClickHouse may resolve
	// each column's tie independently and produce a row belonging to
	// neither collector.
	//
	// Not hypothetical -- router 172.22.0.10 on 2026-09-20 was exactly this,
	// 3 ups from each of dev-c1 and dev-c2.
	stRouterTie  = "10.0.195.7"
	stTieSysName = "dash-st-tie"

	stPeer = "10.255.195.1"

	// Four sessions on stRouterLost, so `sessions` and `ups` are not the same
	// number as `downs` by coincidence.
	stSession1 = 9311
	stSession2 = 9312
	stSession3 = 9313
	stSession4 = 9314
	stSessionD = 9315
	stSessionA = 9316
	// Different counts per twin, so a merged row is visible as a wrong
	// number rather than as a plausible one.
	stSessionT1 = 9317
	stSessionT2 = 9318
	stSessionT3 = 9319
	stSessionT4 = 9320
	stSessionT5 = 9321
	// stRouterDual's sessions: two on the collector that hears the router,
	// one on the collector that keeps losing its view.
	stSessionDual1 = 9322
	stSessionDual2 = 9323
	stSessionDualB = 9324
	stSessionTie1  = 9325
	stSessionTie2  = 9326
	stSessionTieB  = 9327

	// stReasonUnknown is not a reason code RFC 7854 defines. A panel that
	// decodes with a chain of equality tests and no final branch renders it
	// as blank or, worse, as whichever reason its last comparison fell
	// through to.
	stReasonUnknown = 99
)

func insertSessionTurnoverFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	now := corpusCollectorClock
	peerEvent := func(router, sys string, session uint64, ts time.Time, pe *vantagev1.PeerEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: sys},
			Peer:        &vantagev1.PeerId{Ip: stPeer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_PeerEvent{PeerEvent: pe},
		}
	}
	up := func() *vantagev1.PeerEvent {
		return &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP}
	}
	down := func(reason uint32) *vantagev1.PeerEvent {
		return &vantagev1.PeerEvent{
			Kind: vantagev1.PeerEvent_KIND_DOWN, DownReason: reason,
		}
	}

	viewLost := func() *vantagev1.PeerEvent {
		return &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_VIEW_LOST}
	}

	envs := []*vantagev1.Envelope{
		// Four BMP sessions, four Peer Ups, no Peer Down anywhere: the view
		// was lost three times and the network never said anything. Each of
		// the three lost sessions is closed out by the view-lost event the
		// collector now emits when a transport drops (collector's
		// Session.Close); the fourth is still open, which is why there are
		// three and not four.
		peerEvent(stRouterLost, "dash-st-lost", stSession1, now.Add(-40*time.Minute), up()),
		peerEvent(stRouterLost, "dash-st-lost", stSession1, now.Add(-39*time.Minute), viewLost()),
		peerEvent(stRouterLost, "dash-st-lost", stSession2, now.Add(-30*time.Minute), up()),
		peerEvent(stRouterLost, "dash-st-lost", stSession2, now.Add(-29*time.Minute), viewLost()),
		peerEvent(stRouterLost, "dash-st-lost", stSession3, now.Add(-20*time.Minute), up()),
		peerEvent(stRouterLost, "dash-st-lost", stSession3, now.Add(-19*time.Minute), viewLost()),
		peerEvent(stRouterLost, "dash-st-lost", stSession4, now.Add(-10*time.Minute), up()),

		// One session, one up, three downs -- and the three carry DIFFERENT
		// reason codes, including one RFC 7854 does not define.
		peerEvent(stRouterDown, "dash-st-down", stSessionD, now.Add(-40*time.Minute), up()),
		peerEvent(stRouterDown, "dash-st-down", stSessionD, now.Add(-30*time.Minute), down(1)),
		peerEvent(stRouterDown, "dash-st-down", stSessionD, now.Add(-20*time.Minute), down(3)),
		peerEvent(stRouterDown, "dash-st-down", stSessionD, now.Add(-10*time.Minute), down(stReasonUnknown)),

		// No sysName TLV, and a reason 5 -- which is not a session reset at
		// all, but "stop telling me about this peer".
		peerEvent(stRouterAnon, "", stSessionA, now.Add(-35*time.Minute), up()),
		peerEvent(stRouterAnon, "", stSessionA, now.Add(-5*time.Minute), down(5)),

		// Two routers, one sysName. Two sessions on the first, three on the
		// second: a panel that groups on the name alone reports one row of
		// five.
		peerEvent(stRouterTwinA, stTwinSysName, stSessionT1, now.Add(-40*time.Minute), up()),
		peerEvent(stRouterTwinA, stTwinSysName, stSessionT2, now.Add(-38*time.Minute), up()),
		peerEvent(stRouterTwinB, stTwinSysName, stSessionT3, now.Add(-36*time.Minute), up()),
		peerEvent(stRouterTwinB, stTwinSysName, stSessionT4, now.Add(-34*time.Minute), up()),
		peerEvent(stRouterTwinB, stTwinSysName, stSessionT5, now.Add(-32*time.Minute), up()),
	}
	// stRouterDual, appended LAST so nothing above is renumbered: one router,
	// two collectors, crossed so that the collector hearing the most FROM
	// THE ROUTER is not the one holding the view_lost rows.
	//
	// c1: two sessions, three ups, one down -- four router statements.
	// c2: one session, one up, two lost views -- one router statement.
	//
	// Summed that reads 3 sessions / 4 up / 1 down / 2 view_lost; the best
	// single vantage point reads 2 / 3 / 1, and view_lost must stay 2
	// because it is the COLLECTOR's own statement and summing it is the
	// honest answer. See sessionCountsSQL, whose doc argues this at length.
	dualEnv := func(collector string, session uint64, ts time.Time, pe *vantagev1.PeerEvent) *vantagev1.Envelope {
		e := peerEvent(stRouterDual, stDualSysName, session, ts, pe)
		e.CollectorId = collector
		return e
	}
	dual := []*vantagev1.Envelope{
		dualEnv("c1", stSessionDual1, now.Add(-50*time.Minute), up()),
		dualEnv("c1", stSessionDual1, now.Add(-49*time.Minute), up()),
		dualEnv("c1", stSessionDual1, now.Add(-48*time.Minute), down(3)),
		dualEnv("c1", stSessionDual2, now.Add(-47*time.Minute), up()),
		dualEnv(stDualCollectorB, stSessionDualB, now.Add(-46*time.Minute), up()),
		dualEnv(stDualCollectorB, stSessionDualB, now.Add(-45*time.Minute), viewLost()),
		dualEnv(stDualCollectorB, stSessionDualB, now.Add(-44*time.Minute), viewLost()),
		dualEnv(stDualCollectorB, stSessionDualB, now.Add(-43*time.Minute), viewLost()),
		dualEnv(stDualCollectorB, stSessionDualB, now.Add(-42*time.Minute), viewLost()),
	}
	// FOUR lost views and not two, which is what makes the RANKING
	// falsifiable as well as the merge. With two, c2 holds 3 rows against
	// c1's 4, so ranking the vantage-point choice on every event instead of
	// on router statements alone picked the same winner and no test moved --
	// a surviving mutation, found by running it. With four it holds 5 to 4
	// and wins that mutation outright, dragging the whole row onto the
	// collector that heard the router least.
	tieEnv := func(collector string, session uint64, ts time.Time, pe *vantagev1.PeerEvent) *vantagev1.Envelope {
		e := peerEvent(stRouterTie, stTieSysName, session, ts, pe)
		e.CollectorId = collector
		return e
	}
	// Four router statements each, agreeing on nothing else: c1 holds 2
	// sessions / 3 up / 1 down, c2 holds 1 / 4 / 0. The tie-break's second
	// element decides, and "c2" sorts after "c1".
	tie := []*vantagev1.Envelope{
		tieEnv("c1", stSessionTie1, now.Add(-38*time.Minute), up()),
		tieEnv("c1", stSessionTie1, now.Add(-37*time.Minute), up()),
		tieEnv("c1", stSessionTie1, now.Add(-36*time.Minute), down(3)),
		tieEnv("c1", stSessionTie2, now.Add(-35*time.Minute), up()),
		tieEnv(stDualCollectorB, stSessionTieB, now.Add(-34*time.Minute), up()),
		tieEnv(stDualCollectorB, stSessionTieB, now.Add(-33*time.Minute), up()),
		tieEnv(stDualCollectorB, stSessionTieB, now.Add(-32*time.Minute), up()),
		tieEnv(stDualCollectorB, stSessionTieB, now.Add(-31*time.Minute), up()),
	}
	envs = append(envs, dual...)
	envs = append(envs, tie...)
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert session-turnover fixture envelope %d: %v", i, err)
		}
	}

	// One envelope arrives TWICE, byte-identical at the same stream_seq --
	// a JetStream redelivery (TestRowsForRedeliveryIsByteIdentical). Until
	// this existed, every panel reading this fixture was being asked only
	// whether it handles distinct rows correctly, never whether it
	// double-counts a duplicate, and the two halves look identical from a
	// green run.
	//
	// It duplicates stRouterLost's FIRST event rather than any later one, so
	// the router whose events this changes is not one of the twins, whose
	// event COUNTS are what the shared-sysname assertion turns on.
	const redelivered = 0
	if got := envs[redelivered].GetPeerEvent().GetKind(); got != vantagev1.PeerEvent_KIND_UP {
		t.Fatalf("envelope %d is kind %v, want an up: the re-delivery has to "+
			"duplicate a real lifecycle event", redelivered, got)
	}
	if err := ch.Insert(ctx, mustRowsFor(t, envs[redelivered], uint64(redelivered+1))); err != nil {
		t.Fatalf("insert session-turnover fixture re-delivery: %v", err)
	}
}

// scopeFleetToTurnoverFixture narrows a fleet-wide panel to this fixture's
// three routers. Fleet health has no $router control -- it is deliberately a
// view of everything -- so these tests scope by extending the panel's own
// time filter, the way the other fixture-scoped tests here do.
// fleet-health's "Routers" table is the operator's first screen and had no
// test asserting a single value in it -- one of 18 panels across the thirteen
// dashboards in that state. What it answers is "which routers are talking to
// this collector, and how much", so the ways it can be wrong are all ways of
// losing or merging a router.
//
// The turnover fixture is built to catch exactly those. stRouterTwinA and
// stRouterTwinB SHARE a sysname and differ only by address, so a GROUP BY
// that dropped router_ip would report one router with five events instead of
// two with two and three -- plausible, and wrong. stRouterAnon sends no
// sysName TLV at all, which is the '(no sysName TLV)' branch of the panel's
// own if(), and an unnamed router is the one most likely to be dropped by a
// join or a filter that assumes a name.
//
// Unlike "Peer status" beside it, this panel's count() counts ROWS rather
// than groups, so its FINAL is load-bearing -- and this test now DEMONSTRATES
// that rather than reasoning about it. insertSessionTurnoverFixture
// redelivers stRouterLost's first event, so removing FINAL from this panel
// makes it report 8 events where the fixture wrote 7. Measured: before that
// re-delivery existed, stripping every FINAL from fleet-health failed no
// behavioral test at all; it now fails this one.
func TestFleetRoutersTableKeepsEveryRouterApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	raw := scopeFleetRoutersToTurnoverFixture(t, panelSQL(t, "fleet-health", "Routers", "A"))
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("fleet-health routers: %v", err)
	}
	defer rows.Close()

	type routerRow struct {
		sysname string
		events  uint64
	}
	got := map[string]routerRow{}
	var order []uint64
	for rows.Next() {
		var ip, sysname string
		var events uint64
		var lastSeen time.Time
		if err := rows.Scan(&ip, &sysname, &events, &lastSeen); err != nil {
			t.Fatalf("scan routers row: %v", err)
		}
		got[ip] = routerRow{sysname: sysname, events: events}
		order = append(order, events)
		if lastSeen.IsZero() {
			t.Errorf("router %s reports a zero last_seen", ip)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("routers mid-stream: %v", err)
	}

	// toString(router_ip) renders the IPv4-mapped form the column stores.
	v6 := func(ip string) string { return "::ffff:" + ip }
	want := map[string]routerRow{
		v6(stRouterLost):  {"dash-st-lost", 7},
		v6(stRouterDown):  {"dash-st-down", 4},
		v6(stRouterAnon):  {"(no sysName TLV)", 2},
		v6(stRouterTwinA): {stTwinSysName, 2},
		v6(stRouterTwinB): {stTwinSysName, 3},
	}
	// Five rows, not four: the twins must not merge into one.
	if len(got) != len(want) {
		t.Fatalf("routers table returned %d rows, want %d: %v -- two of these "+
			"routers share a sysname and differ only by address, so a GROUP BY "+
			"without router_ip collapses them into one plausible row",
			len(got), len(want), got)
	}
	for ip, w := range want {
		g, ok := got[ip]
		if !ok {
			t.Errorf("router %s is missing from the table entirely; got %v", ip, got)
			continue
		}
		if g.sysname != w.sysname {
			t.Errorf("router %s renders sysname %q, want %q", ip, g.sysname, w.sysname)
		}
		if g.events != w.events {
			t.Errorf("router %s reports %d events, want %d -- this counts "+
				"peer_events ROWS, and the fixture writes a known number of them",
				ip, g.events, w.events)
		}
	}

	// ORDER BY events DESC is what makes this a ranking rather than a list.
	for i := 1; i < len(order); i++ {
		if order[i] > order[i-1] {
			t.Errorf("row %d reports %d events after a row reporting %d: the "+
				"table is ordered by events DESC and this is not",
				i, order[i], order[i-1])
		}
	}
}

// fleet-health's "Peer status" is the pie an operator reads first: how many
// peers are up, down, or lost right now. It had no test.
//
// What it answers is a LATEST-STATE question -- argMax(kind) per
// (router_ip, peer_ip), then a count of those verdicts -- and the turnover
// fixture is arranged to punish a naive reading of it. stRouterLost ends on
// an `up` after three view_lost events, so a panel that counted raw kinds
// instead of per-peer verdicts would report three view_losts that are not
// the current state of anything. stRouterDown ends on a `down` after an up,
// and stRouterAnon on a `down` after an up, so `down` must be 2 and not 4.
//
// The FINAL here is NOT load-bearing, unlike the Routers table beside it,
// and this test is what establishes that: countIf counts GROUPS, and a
// redelivered row cannot create a group -- argMax collapses it inside the
// one it already belongs to. The fixture carries such a redelivery, so this
// assertion holds with FINAL and without it, which is why that keyword is
// gone from this target.
func TestFleetPeerStatusCountsTheLatestVerdictPerPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	raw := scopeFleetPeerStatusToTurnoverFixture(t, panelSQL(t, "fleet-health", "Peer status", "A"))
	var up, down, viewLost, unspecified, stale uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, substituteGrafana(raw, nil))).
		Scan(&up, &down, &viewLost, &unspecified, &stale); err != nil {
		t.Fatalf("fleet-health peer status: %v", err)
	}
	// Every collector here is immortal (chtest's test-only beats), so none
	// reads stale; TestFleetPeerStatusAppliesCollectorLiveness owns that.
	if stale != 0 {
		t.Errorf("stale = %d, want 0", stale)
	}

	// Five (router, peer) pairs, each contributing exactly one verdict.
	if got := up + down + viewLost + unspecified; got != 5 {
		t.Errorf("the four counts total %d, want 5 -- this fixture has five "+
			"routers sharing one peer address, so there are five peer "+
			"sessions and every one of them must land in exactly one bucket",
			got)
	}
	if up != 3 {
		t.Errorf("up = %d, want 3 (%s, %s and %s each end on an up)",
			up, stRouterLost, stRouterTwinA, stRouterTwinB)
	}
	if down != 2 {
		t.Errorf("down = %d, want 2 (%s and %s each end on a down)",
			down, stRouterDown, stRouterAnon)
	}
	// The sharp one: stRouterLost emits three view_lost events and then comes
	// back up. Counting kinds rather than per-peer verdicts reports them.
	if viewLost != 0 {
		t.Errorf("view_lost = %d, want 0 -- %s lost its view three times and "+
			"is up again; those are history, not current state, and a pie "+
			"showing them tells an operator to go and look at a healthy router",
			viewLost, stRouterLost)
	}
	if unspecified != 0 {
		t.Errorf("unspecified = %d, want 0", unspecified)
	}
}

// scopeFleetPeerStatusToTurnoverFixture narrows "Peer status" to the turnover
// fixture's routers. This panel carries no $__timeFilter to extend -- it is a
// deliberately unscoped "right now" question over the whole lab archive -- so the
// filter goes in beside the inner GROUP BY instead, and the column is
// table-qualified for the reason scopeFleetRoutersToTurnoverFixture explains.
//
// The anchor is the per-view grouping, psInnerGrouping: one collector's
// current session with one peer in one rib. It used to be the bare
// "GROUP BY router_ip, peer_ip", and when the panel grew an outer merge over
// collectors that string matched twice -- the inner grouping's prefix and the
// outer grouping itself -- so this helper refused rather than scoping the
// wrong one. That refusal is the design working; the anchor now names the
// grouping it actually means.
func scopeFleetPeerStatusToTurnoverFixture(t *testing.T, sql string) string {
	t.Helper()
	const anchor = psInnerGrouping
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("the peer-status query carries %d occurrences of %q, want 1 "+
			"-- this helper scopes to the fixture by filtering beside that "+
			"grouping and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		"WHERE peer_current.router_ip IN (toIPv6('"+stRouterLost+"'), toIPv6('"+stRouterDown+
			"'), toIPv6('"+stRouterAnon+"'), toIPv6('"+stRouterTwinA+
			"'), toIPv6('"+stRouterTwinB+"')) "+anchor, 1)
}

type stEventBucket struct {
	bucket      time.Time
	up          uint64
	down        uint64
	viewLost    uint64
	unspecified uint64
}

// peerEventSeries reads "Peer up/down over time" scoped to the turnover
// fixture. No $rib is rendered because the panel declines to carry one: a
// Peer Up is session lifecycle, not a member of a RIB stream, and the
// panel's own description says so.
func peerEventSeries(t *testing.T, ctx context.Context, ch *ClickHouse) []stEventBucket {
	t.Helper()
	raw := scopeFleetToTurnoverFixture(t, panelSQL(t, "fleet-health", "Peer up/down over time", "A"))
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("peer event series: %v", err)
	}
	defer rows.Close()
	var out []stEventBucket
	for rows.Next() {
		var b stEventBucket
		if err := rows.Scan(&b.bucket, &b.up, &b.down, &b.viewLost, &b.unspecified); err != nil {
			t.Fatalf("scan peer event bucket: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("peer event buckets: %v", err)
	}
	return out
}

// "Peer up/down over time" sits beside the "Peer status" pie and answers the
// OPPOSITE question. The pie reports one verdict per peer; this reports every
// event that arrived. Conflating the two is the easy mistake -- the fixture's
// stRouterLost contributes ONE up to the pie and SEVEN events here -- and
// until now nothing asserted a single value in it.
//
// So the number this pins is "every event the fixture wrote, each counted
// once", which is three failures at the same time:
//
//   - Collapsing to a verdict per peer, the way the pie does. There are five
//     (router, peer) pairs, so that reads up = 5 rather than 11.
//   - Folding view_lost into down. Those are separate columns for the reason
//     they were split on 2026-08-23: on the lab archive the two counts are
//     near perfectly inverted, and a router that silently loses its view
//     looks healthy in a `down` count.
//   - Counting an unmerged duplicate row. THIS PANEL'S FINAL IS LOAD-BEARING
//     and this assertion is what establishes it: countIf here counts ROWS,
//     not groups -- there is no argMax for a duplicate to fall inside of,
//     unlike the pie next to it -- so every duplicate is a real extra `up`
//     until a background merge collapses it. Measured by removing FINAL from
//     this target alone and running the whole package: up = 14, want 11.
//
// That 14 is worth stating exactly, because the number is NOT stable and a
// mutation run that quoted it as a constant would be lying. Two things feed
// it. The fixture redelivers its first envelope on purpose, which is one
// extra `up`; and eight tests in this package each insert the whole fixture
// again into a database chtest.Require recreates once per BINARY, not per
// test, so the same 19 byte-identical rows are written eight times over.
// FINAL is what makes 11 the answer to all of that at once -- which is
// precisely the point. Without it the panel reads somewhere between 11 and
// 96 depending on when ClickHouse last merged, and an operator watching a
// peer-churn graph cannot tell a real reconnect storm from a merge that has
// not run yet.
//
// That is why the panel keeps a keyword 38 of its siblings have shed.
// TestEveryCountingDashboardTargetDedups already refuses to let it go
// statically; this says what the operator would actually read if it did.
func TestFleetPeerEventsSeriesCountsEveryEventOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	buckets := peerEventSeries(t, ctx, ch)
	// A single bucket would make every total below readable off one row,
	// which is not what this panel is: it is a series keyed on ts_collector,
	// and the fixture spreads its events across thirteen distinct minutes.
	if len(buckets) < 2 {
		t.Fatalf("the series has %d buckets, want several -- the fixture's "+
			"events span 35 minutes, so one bucket means the $__timeInterval "+
			"grouping is not grouping and these totals prove nothing about a "+
			"time series", len(buckets))
	}

	var up, down, viewLost, unspecified uint64
	for _, b := range buckets {
		up += b.up
		down += b.down
		viewLost += b.viewLost
		unspecified += b.unspecified
	}

	// 18 since the two dual-collector routers joined this fixture, and the
	// arithmetic is per router with the collector chosen: four ups on %s,
	// one on %s, one on %s, two on %s, three on %s -- eleven, as before --
	// plus three from %s (c1's view, it having heard four router statements
	// to c2's one) and four from %s (c2's view, the two collectors tying on
	// statements and the collector_id tie-break deciding).
	//
	// Derived that way rather than read off the output. Note what is NOT
	// added: c2's single up under %s and c1's three under %s belong to the
	// collectors that lost, and a panel that counted them would be back to
	// reporting how many collectors watch a router as how often it came up.
	if up != 18 {
		t.Errorf("up = %d, want 18 -- eleven from the single-collector "+
			"routers, three from %s and four from %s. 5 means the panel "+
			"collapsed to one verdict per peer the way the pie does; 22 "+
			"means it is summing both collectors of the dual-homed routers; "+
			"anything above that means it is counting unmerged duplicate "+
			"rows and its FINAL is gone",
			up, stRouterDual, stRouterTie)
	}
	// 5: three on the NX-shaped router, one on the anonymous one, and one
	// from %s's chosen collector. %s contributes NONE -- its winning
	// collector saw four ups and no down at all.
	if down != 5 {
		t.Errorf("down = %d, want 5 (three on %s, one on %s, one on %s)",
			down, stRouterDown, stRouterAnon, stRouterDual)
	}
	// The sharp one. stRouterLost reports no peer down at all -- it just
	// stops being heard from -- so these three exist only because the
	// collector synthesizes a view-lost on transport close. A panel that
	// folded them into `down` would report 7 downs and hide the distinction.
	// 7: three from %s and four from %s's blind collector. view_lost is
	// SUMMED across collectors where up and down are not, because it is the
	// collector's own statement rather than the router's -- see
	// sessionCountsSQL, which argues that at length. A best-vantage
	// view_lost would read 3 here, hiding a collector that went blind four
	// times behind the one that stayed healthy.
	if viewLost != 7 {
		t.Errorf("view_lost = %d, want 7 -- three on %s, which loses its "+
			"view and never reports a peer down, and four on %s's blind "+
			"collector. Folding these into `down` is the conflation this "+
			"pair of columns exists to prevent; reporting 3 means view_lost "+
			"was best-vantaged along with its neighbours",
			viewLost, stRouterLost, stRouterDual)
	}
	if unspecified != 0 {
		t.Errorf("unspecified = %d, want 0", unspecified)
	}
	// Every event the fixture wrote landed in exactly one of the four
	// columns: a fifth kind would otherwise vanish silently.
	// 30 of the 35 rows the fixture archives. The five not counted are the
	// losing collectors' router statements -- c2's one up under %s and c1's
	// three ups and one down under %s -- which is the whole point of the
	// vantage-point choice rather than a leak. Every row that IS reported
	// lands in exactly one column, which is what a fifth kind would break.
	if got := up + down + viewLost + unspecified; got != 30 {
		t.Errorf("the four columns total %d, want 30 -- the fixture writes "+
			"35 peer events (plus one redelivery of the first), five of them "+
			"belonging to collectors that lost the vantage-point choice, and "+
			"every remaining one has to land in exactly one column", got)
	}
}

// fleet-health's two stats panels -- "Loc-RIB routes by peer" (RFC 7854 §4.8
// stat type 8) and "Prefixes rejected by inbound policy" (stat type 0) -- are
// the only panels reading stats_events, and no dashboard fixture wrote a
// single row to that table until this one. They were listed as cheap to test
// because a fixture "was already in place"; it was not, and the reason is
// worth the comment: stats_events is the one table whose values are the
// ROUTER'S OWN counts rather than ours, which is exactly why these two panels
// exist and exactly why nothing else in this file had cause to populate it.
//
// THE DEFECT THIS FIXTURE IS SHAPED AROUND is ClickHouse's Map default.
// counters is a Map(UInt32, UInt64), and `counters[8]` on a row with no key 8
// returns 0 -- not null, not an error. So a peer that has never reported a
// Loc-RIB count renders as a flat zero line, pixel-identical to a peer
// reporting a genuinely empty Loc-RIB. Those are opposite operational facts:
// one is "we cannot see this peer's table", the other is "this peer's table
// is empty". The panels' `has(counters, N)` is the only thing separating
// them, and query/collection.go's HasStat column exists for the same reason
// on the API side.
const (
	stsRouter = "10.0.196.1"
	// stsRouterB reports the SAME peer address as stsRouter, so a panel that
	// keyed its series on peer_ip alone would blend two routers' Loc-RIBs
	// into one line.
	stsRouterB = "10.0.196.2"

	// stsPeerBoth reports both counters, and reports them TWICE in one
	// bucket -- see the fixture body.
	stsPeerBoth = "10.255.196.1"
	// stsPeerNoLocRIB sends stats reports that never carry key 8. It must be
	// ABSENT from "Loc-RIB routes by peer", not present at zero.
	stsPeerNoLocRIB = "10.255.196.2"
	// stsPeerZeroRejects reports key 0 with the value 0: a peer whose
	// inbound policy is rejecting nothing. That is a real measurement and it
	// must render, which is the same distinction as stsPeerNoLocRIB's from
	// the other side -- absence and zero have to be told apart in BOTH
	// directions or the `has()` test is only half tested.
	stsPeerZeroRejects = "10.255.196.3"
	// stsPeerNoRejects is stsPeerNoLocRIB's mirror: it reports key 8 and
	// never key 0, so it must be absent from the REJECTED panel. Without it
	// every peer here reported key 0, which made that panel's own
	// `has(counters, 0)` unfalsifiable -- deleting it moved no assertion.
	// Caught by trying the mutation rather than by reading the test.
	stsPeerNoRejects = "10.255.196.4"

	stsSession = 9611
	// Stream sequences well clear of the other fixtures in this file.
	stsSeqBase = 8100
)

func insertStatsFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	now := corpusCollectorClock
	// One bucket. These two panels are tested for what they report per peer,
	// not for their bucketing -- $__timeInterval is already held by
	// TestFleetPeerEventsSeriesCountsEveryEventOnce on the panel beside them
	// -- and a single bucket makes "one row per peer" readable.
	ts := now.Add(-20 * time.Minute)
	stats := func(router, peer string, counters map[uint32]uint64) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: router, SysName: "dash-sts"},
			Peer:        &vantagev1.PeerId{Ip: peer, Asn: 65000},
			SessionId:   stsSession,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Stats{Stats: &vantagev1.StatsEvent{Counters: counters}},
		}
	}

	envs := []*vantagev1.Envelope{
		// TWO samples for one peer at the SAME ts_collector, and the later
		// one is SMALLER. A Loc-RIB that shrank from 900 to 11 is a peer that
		// just lost most of its table -- the single most alarming thing these
		// panels can show -- and a panel reaching for max() instead of argMax
		// would keep drawing 900 and hide it. Identical timestamps also mean
		// the tie is broken by stream_seq alone, which is the half of
		// argMax's (ts_collector, stream_seq) tuple nothing else exercises.
		stats(stsRouter, stsPeerBoth, map[uint32]uint64{0: 4, 8: 900}),
		stats(stsRouter, stsPeerBoth, map[uint32]uint64{0: 6, 8: 11}),

		// Stats reports that carry key 0 and NEVER key 8.
		stats(stsRouter, stsPeerNoLocRIB, map[uint32]uint64{0: 2}),

		// A real zero: this peer's policy rejected nothing.
		stats(stsRouter, stsPeerZeroRejects, map[uint32]uint64{0: 0, 8: 3}),

		// Stats reports that carry key 8 and NEVER key 0.
		stats(stsRouter, stsPeerNoRejects, map[uint32]uint64{8: 5}),

		// Same peer address as the first, different router.
		stats(stsRouterB, stsPeerBoth, map[uint32]uint64{0: 1, 8: 7}),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(stsSeqBase+i))); err != nil {
			t.Fatalf("insert stats fixture envelope %d: %v", i, err)
		}
	}

	// A JetStream redelivery, byte-identical at its original stream_seq.
	// Neither panel carries FINAL -- it was removed from both on 2026-09-19 --
	// so this is what asks whether argMax was really enough on its own, as
	// that change assumed without testing and as query/collection.go's own comment
	// argues. It duplicates the peer whose value is NOT a two-sample argMax,
	// so the duplicate is the only thing that could move that number.
	//
	// WHAT CANNOT BE ASSERTED, stated because the obvious test here is one
	// this repository has already written, found flaky and removed once: that
	// the duplicate is STILL TWO ROWS when a panel reads it. ReplacingMergeTree
	// merges in the background on its own schedule, and it collapsed this pair
	// mid-session while these tests were being written -- a `count() >= 2`
	// guard passed, then failed, on an unchanged fixture. Forcing the window
	// open needs SYSTEM STOP MERGES, which is server-wide state shared with
	// every package running concurrently.
	//
	// So the redelivery is a RATCHET, not a proof: on the runs where the merge
	// has not yet happened, the two panel tests below are being asked whether
	// they double-count, and they would fail if a future edit made either
	// panel duplication-sensitive (argMax swapped for a count, say). On the
	// runs where it has, they are asking the same question of one row. Neither
	// run can tell you which it was, and no assertion here pretends to.
	const redelivered = 3
	if got := envs[redelivered].GetPeer().GetIp(); got != stsPeerZeroRejects {
		t.Fatalf("envelope %d is peer %s, want %s: the re-delivery has to "+
			"duplicate the single-sample peer", redelivered, got, stsPeerZeroRejects)
	}
	if err := ch.Insert(ctx, mustRowsFor(t, envs[redelivered], uint64(stsSeqBase+redelivered))); err != nil {
		t.Fatalf("insert stats fixture re-delivery: %v", err)
	}
}

// scopeFleetStatsToFixture narrows a stats panel to this fixture's two
// routers by extending the panel's own time filter, the way the other
// fixture-scoped fleet-health helpers do.
func scopeFleetStatsToFixture(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("a stats query carries %d occurrences of %q, want 1 -- this "+
			"helper scopes to the fixture's routers by extending that filter "+
			"and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor, anchor+
		" AND router_ip IN (toIPv6('"+stsRouter+"'), toIPv6('"+stsRouterB+"'))", 1)
}

// statsPanelSeries reads one of the two stats panels and returns its series
// keyed by the label an operator actually reads -- "router / peer".
func statsPanelSeries(t *testing.T, ctx context.Context, ch *ClickHouse, panel string) map[string]uint64 {
	t.Helper()
	raw := scopeFleetStatsToFixture(t, panelSQL(t, "fleet-health", panel, "A"))
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, nil)))
	if err != nil {
		t.Fatalf("%s: %v", panel, err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var bucket time.Time
		var peer string
		var value uint64
		if err := rows.Scan(&bucket, &peer, &value); err != nil {
			t.Fatalf("scan %s row: %v", panel, err)
		}
		if _, dup := out[peer]; dup {
			t.Fatalf("%s returned two rows for %q; this fixture writes one "+
				"bucket, so a second row means the grouping is not collapsing "+
				"the samples it is supposed to", panel, peer)
		}
		out[peer] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", panel, err)
	}
	return out
}

// statsLabel builds the "router / peer" label the panels render, from the
// v4-mapped v6 form ClickHouse stores these addresses in.
func statsLabel(router, peer string) string {
	return "::ffff:" + router + " / ::ffff:" + peer
}

// want asserts one series of a stats panel, REPORTING AN ABSENT SERIES AS
// ABSENT rather than as zero. Reading a missing key out of a Go map yields
// the zero value, which is the identical confusion between "no measurement"
// and "measured zero" that both of these panels exist to avoid -- an
// assertion written the obvious way reports a series the panel never drew as
// "= 0, want 11" and sends the next reader looking at argMax.
func want(t *testing.T, got map[string]uint64, series string, v uint64, why string) {
	t.Helper()
	if why != "" {
		why = " -- " + why
	}
	actual, ok := got[series]
	if !ok {
		t.Errorf("the panel drew no series %q at all, want one reading %d%s",
			series, v, why)
		return
	}
	if actual != v {
		t.Errorf("series %q = %d, want %d%s", series, actual, v, why)
	}
}

// "Loc-RIB routes by peer" is the one number on these dashboards that can be
// checked against an outside authority: it is the ROUTER'S count of its own
// Loc-RIB, so comparing it with what we archived says whether we are missing
// routes the router believes it sent. query/collection.go's LocRIBComparison
// is built on exactly that, and this panel is where an operator sees it.
//
// It had no test. The three things it can get wrong are all here:
//
//   - Reporting a peer that has never sent stat type 8 as though its Loc-RIB
//     were empty. counters[8] on a row with no key 8 is 0, so dropping
//     `has(counters, 8)` turns "we cannot see this peer's table" into "this
//     peer's table is empty" with no visible difference.
//   - Reading the largest sample in a bucket instead of the last. This is a
//     gauge; the fixture's peer drops from 900 to 11 inside one bucket, which
//     is a peer losing 98% of its table, and max() draws a flat line through
//     it.
//   - Blending two routers that share a peer address into one series.
func TestFleetLocRIBPanelSeparatesAnEmptyTableFromNoReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertStatsFixture(t, ctx, ch)

	got := statsPanelSeries(t, ctx, ch, "Loc-RIB routes by peer")

	// The absence assertion, and the reason this panel carries has().
	if v, ok := got[statsLabel(stsRouter, stsPeerNoLocRIB)]; ok {
		t.Errorf("%s reports a Loc-RIB of %d for a peer that has never sent "+
			"stat type 8. It must not appear at all: counters[8] defaults to "+
			"0 on a Map with no such key, so this line is indistinguishable "+
			"from a peer whose Loc-RIB really is empty -- and those are "+
			"opposite facts about whether we can see that peer's table",
			stsPeerNoLocRIB, v)
	}
	// The gauge assertion.
	want(t, got, statsLabel(stsRouter, stsPeerBoth), 11,
		"two samples in this bucket, 900 then 11 at the same ts_collector. "+
			"900 means the panel took the largest sample rather than the last, "+
			"which would draw a flat line through a peer losing 98% of its table")
	// The per-router assertion: same peer address, different router.
	want(t, got, statsLabel(stsRouterB, stsPeerBoth), 7,
		"a DIFFERENT router reporting the same peer address as the series "+
			"above, which has to be its own line")
	want(t, got, statsLabel(stsRouter, stsPeerZeroRejects), 3, "")
	want(t, got, statsLabel(stsRouter, stsPeerNoRejects), 5,
		"this peer reports no key 0 at all, which must not keep it out of "+
			"the key 8 panel")
	if len(got) != 4 {
		t.Errorf("the panel drew %d series, want 4 (%v) -- the fixture holds "+
			"five (router, peer) pairs and exactly one of them has never "+
			"reported a Loc-RIB", len(got), sortedKeys(got))
	}
}

// "Prefixes rejected by inbound policy" is the same shape on stat type 0, and
// it is the absence/zero distinction from the OTHER side. Here a value of
// zero is the informative answer -- the peer's inbound policy rejected
// nothing -- and it has to render rather than being swallowed as though the
// peer had said nothing. Testing only the Loc-RIB panel would leave that half
// unexercised, because there the zero and the absence point the same way.
//
// The panel matters for a reason nothing else on either dashboard can cover:
// rejected prefixes are dropped ON THE ROUTER and never advertised over BMP,
// so they reach no table we hold. "The peer is sending nothing" and "our own
// policy is dropping what it sends" are identical everywhere else.
func TestFleetRejectedPrefixesPanelRendersARealZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertStatsFixture(t, ctx, ch)

	got := statsPanelSeries(t, ctx, ch, "Prefixes rejected by inbound policy")

	want(t, got, statsLabel(stsRouter, stsPeerZeroRejects), 0,
		"zero rejections is a MEASUREMENT, not a missing one -- this peer "+
			"reported stat type 0 with the value 0, and dropping it says the "+
			"same thing as drawing it")
	want(t, got, statsLabel(stsRouter, stsPeerBoth), 6,
		"4 then 6 in this bucket, and the later sample is the one a gauge shows")
	// The absence half, mirroring the Loc-RIB panel's. Without a peer that
	// reports key 8 and never key 0, this panel's own has(counters, 0) could
	// be deleted with no test noticing.
	if v, ok := got[statsLabel(stsRouter, stsPeerNoRejects)]; ok {
		t.Errorf("the panel reports %d rejected prefixes for %s, which has "+
			"never sent stat type 0. counters[0] defaults to 0 on a Map with "+
			"no such key, so this draws a peer whose policy we cannot see as "+
			"a peer whose policy is rejecting nothing", v, stsPeerNoRejects)
	}
	if len(got) != 4 {
		t.Errorf("the panel drew %d series, want 4 (%v) -- four of the "+
			"fixture's five (router, peer) pairs report stat type 0",
			len(got), sortedKeys(got))
	}
}

// scopeFleetRoutersToTurnoverFixture narrows the "Routers" table to the
// turnover fixture's five routers, TABLE-QUALIFYING the column it filters on.
//
// That qualification is not decoration. The panel selects
// `toString(router_ip) AS router_ip`, so the alias shadows the column of the
// same name: a plain `WHERE router_ip IN (toIPv6(...))` resolves to the
// String alias rather than the IPv6 column and quietly matches nothing --
// no error, no warning, an empty result. Confirmed directly against
// ClickHouse: identical queries return 7 rows with `peer_events.router_ip`
// and 0 with `router_ip`.
//
// Nothing shipped is broken by this today, because the panel's own WHERE
// filters only ts_collector. It is a trap laid for whoever adds a router
// filter to this panel next, which is why it is written down rather than
// quietly worked around.
func scopeFleetRoutersToTurnoverFixture(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("the routers query carries %d occurrences of %q, want 1 -- "+
			"this helper scopes to the fixture by extending that filter and "+
			"must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor, anchor+
		" AND peer_events.router_ip IN (toIPv6('"+stRouterLost+"'), toIPv6('"+stRouterDown+
		"'), toIPv6('"+stRouterAnon+"'), toIPv6('"+stRouterTwinA+
		"'), toIPv6('"+stRouterTwinB+"'))", 1)
}

func scopeFleetToTurnoverFixture(t *testing.T, sql string) string {
	t.Helper()
	const anchor = "$__timeFilter(ts_collector)"
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("a session-turnover query carries %d occurrences of %q, want "+
			"1 -- these tests scope to the fixture's routers by extending "+
			"that filter, and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor, anchor+
		" AND router_ip IN (toIPv6('"+stRouterLost+"'), toIPv6('"+stRouterDown+
		"'), toIPv6('"+stRouterAnon+"'), toIPv6('"+stRouterTwinA+
		"'), toIPv6('"+stRouterTwinB+"'), toIPv6('"+stRouterDual+
		"'), toIPv6('"+stRouterTie+"'))", 1)
}

type stTurnoverRow struct {
	router    string
	sessions  uint64
	ups       uint64
	downs     uint64
	viewLosts uint64
	lastSeen  time.Time
}

// sessionTurnover keys by router name. Reach for sessionTurnoverRows where
// the assertion is about two routers that SHARE one.
func sessionTurnover(t *testing.T, ctx context.Context, ch *ClickHouse) map[string]stTurnoverRow {
	t.Helper()
	out := map[string]stTurnoverRow{}
	for _, r := range sessionTurnoverRows(t, ctx, ch) {
		out[r.router] = r
	}
	return out
}

func sessionTurnoverRows(t *testing.T, ctx context.Context, ch *ClickHouse) []stTurnoverRow {
	t.Helper()
	raw := scopeFleetToTurnoverFixture(t, panelSQL(t, "fleet-health", "Lost view vs peer down", "A"))
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, map[string]string{"rib": ".*"})))
	if err != nil {
		t.Fatalf("session turnover: %v", err)
	}
	defer rows.Close()
	var out []stTurnoverRow
	for rows.Next() {
		var r stTurnoverRow
		if err := rows.Scan(&r.router, &r.sessions, &r.ups, &r.downs, &r.viewLosts, &r.lastSeen); err != nil {
			t.Fatalf("scan turnover row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("turnover rows: %v", err)
	}
	return out
}

type stReasonRow struct {
	router   string
	peer     string
	reason   string
	downs    uint64
	lastSeen time.Time
}

func peerDownReasons(t *testing.T, ctx context.Context, ch *ClickHouse) []stReasonRow {
	t.Helper()
	raw := scopeFleetToTurnoverFixture(t, panelSQL(t, "fleet-health", "Why peers went down", "A"))
	rows, err := ch.conn.Query(ctx, qualify(ch, substituteGrafana(raw, map[string]string{"rib": ".*"})))
	if err != nil {
		t.Fatalf("peer down reasons: %v", err)
	}
	defer rows.Close()
	var out []stReasonRow
	for rows.Next() {
		var r stReasonRow
		if err := rows.Scan(&r.router, &r.peer, &r.reason, &r.downs, &r.lastSeen); err != nil {
			t.Fatalf("scan reason row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reason rows: %v", err)
	}
	return out
}

// The headline. A router that lost its view four times and a router that
// reported three peer downs must produce different numbers in different
// columns -- on the lab archive these two counts are almost perfectly
// inverted, and nothing in the dashboard set says so.
func TestFleetHealthSeparatesLostViewsFromPeerDowns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	got := sessionTurnover(t, ctx, ch)
	lost, ok := got["dash-st-lost"]
	if !ok {
		t.Fatalf("no row for the router that lost its view; got %v", sortedKeys(got))
	}
	if lost.sessions != 4 {
		t.Errorf("the router that reconnected four times reports %d BMP "+
			"sessions, want 4", lost.sessions)
	}
	if lost.downs != 0 {
		t.Errorf("the router that never sent a Peer Down reports %d downs, "+
			"want 0 -- this is the whole distinction the panel exists to "+
			"draw", lost.downs)
	}
	// The panel used to infer a lost view from session count alone, which is
	// the only signal that existed. It now has a direct one, and reporting
	// both is what lets a reader tell "three sessions ended and we know why"
	// from "three sessions ended and nothing was recorded".
	if lost.viewLosts != 3 {
		t.Errorf("the router that lost its view three times reports %d "+
			"view-lost events, want 3", lost.viewLosts)
	}

	down, ok := got["dash-st-down"]
	if !ok {
		t.Fatalf("no row for the router that reported downs; got %v", sortedKeys(got))
	}
	if down.sessions != 1 {
		t.Errorf("the router that held one session reports %d, want 1", down.sessions)
	}
	if down.downs != 3 {
		t.Errorf("the router that sent three Peer Downs reports %d, want 3", down.downs)
	}
	// The other half of the distinction: a router whose peers the ROUTER
	// reported down has no view-lost events at all. Without this the
	// assertion above would pass against a panel that counted every
	// non-'up' event as a lost view.
	if down.viewLosts != 0 {
		t.Errorf("the router that reported its downs on the wire also "+
			"reports %d view-lost events, want 0", down.viewLosts)
	}
	if lost.sessions == down.sessions || lost.downs == down.downs {
		t.Error("the two routers report the same numbers, so the panel is " +
			"not distinguishing a lost view from a peer down")
	}
}

// A router that sends no sysName TLV still needs a row an operator can read.
func TestFleetHealthTurnoverNamesTheRouterWithNoSysName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	got := sessionTurnover(t, ctx, ch)
	if _, blank := got[""]; blank {
		t.Error("the router with no sysName TLV renders as a blank cell, " +
			"which reads as a name that failed to load rather than as a " +
			"name that was never sent")
	}
	if _, ok := got["(no sysName TLV)"]; !ok {
		t.Errorf("no spelled-out row for the router that sent no sysName "+
			"TLV; rows are %v", sortedKeys(got))
	}
}

// The collector stores the Peer Down reason as the raw byte off the wire and
// never names it -- nothing in the repo turns 3 into English. This panel is
// the first place those codes are read, so an unknown one has to say it is
// unknown rather than fall through to whichever branch came last.
func TestFleetHealthNamesEveryPeerDownReasonItHolds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	rows := peerDownReasons(t, ctx, ch)
	byReason := map[string]stReasonRow{}
	for _, r := range rows {
		byReason[r.reason] = r
	}
	var texts []string
	for reason := range byReason {
		texts = append(texts, reason)
	}
	sort.Strings(texts)

	find := func(substr string) (stReasonRow, bool) {
		for reason, row := range byReason {
			if strings.Contains(strings.ToLower(reason), substr) {
				return row, true
			}
		}
		return stReasonRow{}, false
	}
	if _, ok := find("local system closed"); !ok {
		t.Errorf("reason 1 is not named as a local close; the panel offers %v", texts)
	}
	if _, ok := find("remote system closed"); !ok {
		t.Errorf("reason 3 is not named as a remote close; the panel offers %v", texts)
	}
	// Reason 5 is not a session failure at all -- it means this peer will no
	// longer be reported -- so it must not read like one.
	if _, ok := find("no longer"); !ok {
		t.Errorf("reason 5 is not named as monitoring ceasing; the panel "+
			"offers %v -- read as a failure it would send someone looking "+
			"for an outage that did not happen", texts)
	}
	unknown, ok := find("rfc 7854")
	if !ok {
		t.Fatalf("reason %d is not flagged as undefined; the panel offers %v",
			stReasonUnknown, texts)
	}
	if !strings.Contains(unknown.reason, fmt.Sprint(stReasonUnknown)) {
		t.Errorf("the undefined reason renders as %q without naming the code "+
			"%d, so there is nothing to look up", unknown.reason, stReasonUnknown)
	}
}

// The reason column only means anything for downs. Peer Up events carry
// down_reason = 0, so a panel that forgets to filter on kind renders 788 of
// them in the lab archive as "reason 0" -- a reason code that does not exist
// for an event that did not happen.
func TestFleetHealthPeerDownTableHoldsOnlyDowns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	var total uint64
	for _, r := range peerDownReasons(t, ctx, ch) {
		total += r.downs
		if strings.HasPrefix(r.reason, "0") {
			t.Errorf("the table renders reason %q, which is what a Peer UP "+
				"carries; it is not filtering on kind", r.reason)
		}
	}
	// 5 since stRouterDual joined the fixture: three from one router, one
	// from the router with no sysName, and one from the dual-collector
	// router. That last one is written by ONE of its two collectors, so it
	// counts once here whether the panel merges collectors or not -- this
	// test is about the kind filter, not about the merge, and the number
	// moved because the fixture grew rather than because a panel changed.
	if total != 6 {
		t.Errorf("the table accounts for %d downs, want 6 (three from one "+
			"router, one from the router with no sysName, one from the "+
			"dual-collector router and one from the tie router) -- this "+
			"fixture writes eleven Peer Ups alongside them", total)
	}
}

// A sysName is not an identity. Two routers can report the same one -- two
// sites that both named a box "core1", or one device re-imaged onto a new
// address -- and this panel's whole point is a per-router session count, so
// merging them adds two machines' reconnects into a single wrong number.
//
// Every sysName in the lab archive maps to exactly one address, so this
// fixture is the only place the merge is visible. The sibling case is already
// covered: TestCompletenessPanelDoesNotBlendRoutersSharingAnIP guards two
// sysNames sharing one address.
func TestFleetHealthTurnoverKeepsTwoRoutersSharingASysNameApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	var twins []stTurnoverRow
	for _, r := range sessionTurnoverRows(t, ctx, ch) {
		if r.router == stTwinSysName {
			twins = append(twins, r)
		}
	}
	if len(twins) != 2 {
		var got []uint64
		for _, r := range twins {
			got = append(got, r.sessions)
		}
		t.Fatalf("%d rows report sysName %q, want 2 (session counts %v) -- "+
			"one row means two routers' reconnects were added together",
			len(twins), stTwinSysName, got)
	}
	sessions := []uint64{twins[0].sessions, twins[1].sessions}
	slices.Sort(sessions)
	if sessions[0] != 2 || sessions[1] != 3 {
		t.Errorf("the two routers sharing %q report %v sessions, want [2 3]",
			stTwinSysName, sessions)
	}
}

// TestParseFlagPanelsUnionEveryTableThatCarriesFlags is the guard that would
// have caught eor_events going missing, and the one thing that keeps the
// next table from repeating it.
//
// parse_flags is on the envelope, so it lands on every row of every table
// the writer files -- ten of them. A panel that unions eight is not partly
// right: it reports a fleet-wide total that is confidently short by whatever
// the ninth holds, with nothing in the answer to suggest it. That is the
// exact failure fleet-health's own panel description already records ("a
// condition that had fired 43 times fleet-wide" read as 2 from one table),
// and it happened twice more before this test existed. Splitting End-of-RIB
// markers out of route_unicast moved 56 marker rows -- every one of them
// carrying flags, 60 flag events in total -- into a table these five unions
// did not name. And ls_prefixes, which has carried a parse_flags column
// since it was created and holds 61 flagged rows in the lab archive, had never
// been read by any of them at all: the panel descriptions said "eight
// tables" and there were ten. This test found that one; nobody did.
//
// The set of tables comes from system.columns rather than a list here, so
// this cannot drift: a table added to schema.sql with a parse_flags column
// fails this test until it is either unioned or exempted where a reader can
// see the exemption.
//
// It matches on the table name appearing in the SQL at all rather than
// parsing the UNION structure. A panel that named a table only in a comment
// would pass, which is a hole worth stating -- but the failure being
// defended against is a table nobody remembered, not a table someone
// mentioned and then declined to read.
func TestParseFlagPanelsUnionEveryTableThatCarriesFlags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()

	rows, err := ch.conn.Query(ctx,
		"SELECT table FROM system.columns WHERE database = ? AND name = 'parse_flags' ORDER BY table",
		ch.db)
	if err != nil {
		t.Fatalf("list parse_flags tables: %v", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("parse_flags tables: %v", err)
	}
	// Derived tables are excluded, not unioned. Each *_current table is fed
	// by a materialized view on its history table, so every row in it is a
	// copy of a row the panels already read, and unioning it would count each
	// flag twice and report the duplication as network fact, which is this
	// project's most recurring defect. Each view carries the column for the
	// same reason and is not a table anyone queries. (eor_current and its two
	// views carry no parse_flags column, so they need no entry.)
	//
	// Named rather than pattern-matched: a rule like "skip anything ending in
	// _current" would silently exempt a future base table that happened to be
	// named that way, and the whole point of reading system.columns is that
	// nothing gets excluded without someone writing it down here.
	derived := map[string]string{}
	for _, hist := range []string{
		"peer_events", "route_unicast", "route_vpn", "route_evpn",
		"ls_nodes", "ls_links", "ls_prefixes",
	} {
		cur := strings.TrimSuffix(hist, "_events") + "_current"
		derived[cur] = "fed from " + hist + " by a materialized view; unioning it double-counts every flag"
		derived[cur+"_mv"] = "the materialized view feeding " + cur + ", not a table anyone reads"
	}
	kept := tables[:0]
	for _, name := range tables {
		if _, skip := derived[name]; !skip {
			kept = append(kept, name)
		}
	}
	tables = kept

	// The premise. A query returning nothing would make every assertion
	// below vacuously true, which is the shape this repo keeps finding.
	if len(tables) < 10 {
		t.Fatalf("found %d tables with a parse_flags column (%v), want at least "+
			"10 -- if the schema really shrank, update this floor deliberately; "+
			"an empty or short list makes this test assert nothing", len(tables), tables)
	}

	var checked int
	for _, dash := range allDashboards {
		for _, tg := range dashboardTargets(t, dash) {
			if !strings.Contains(tg.rawSQL, "parse_flags") {
				continue
			}
			checked++
			for _, tbl := range tables {
				if !strings.Contains(tg.rawSQL, "vantage."+tbl) {
					t.Errorf("%s / %q (%s) reads parse_flags but never names "+
						"vantage.%s, which carries the column -- the panel's "+
						"answer is short by whatever that table holds, and "+
						"nothing in it says so. Union the table, or if it is "+
						"deliberately excluded say why in the panel description "+
						"and exempt it here by name.",
						tg.dashboard, tg.panel, tg.refID, tbl)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no dashboard target reads parse_flags; this test walked every " +
			"committed dashboard and found nothing to check, so it is asserting " +
			"nothing at all")
	}
	t.Logf("checked %d parse_flags targets against %d tables: %v", checked, len(tables), tables)
}

// TestTopologyMarksPseudonodesInTheSubtitle pins that a LAN pseudonode is
// distinguishable in the graph from a router.
//
// It cannot be told apart by anything the title shows: a pseudonode carries
// no name and no router_id_v4, so the title falls all the way through to raw
// hex -- which is also what the fixture's unnamed NX-OS-style node renders as.
// Two vertices then look identical while one is a device and the other is an
// Ethernet segment, and the operator has no way to tell which is which.
//
// The subtitle already carries this kind of distinction ("node" vs "endpoint
// only"), so the third value goes there rather than into a new field.
func TestTopologyMarksPseudonodesInTheSubtitle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopologyFixture(t, ctx, ch)

	titles, subtitles := nodeFrame(t, ctx, ch, "0", "0")

	var found bool
	for id, sub := range subtitles {
		if sub == "pseudonode (LAN)" {
			found = true
			if titles[id] == "dash-a" || titles[id] == "dash-d" {
				t.Errorf("a named router was marked as a pseudonode: %q", titles[id])
			}
		}
	}
	if !found {
		t.Errorf("no node subtitled %q; the LAN pseudonode is indistinguishable from a router. titles=%v subtitles=%v",
			"pseudonode (LAN)", titles, subtitles)
	}
	// The discriminating half: real routers must keep their old subtitle, or
	// the marker would be applied to everything and mean nothing.
	var plainNodes int
	for _, sub := range subtitles {
		if sub == "node" {
			plainNodes++
		}
	}
	if plainNodes == 0 {
		t.Error("no node is subtitled \"node\" any more: the pseudonode test is matching every row")
	}
}

// sessionAgg is one aggregate over session_id found in a dashboard target,
// with the vantage table it reads.
type sessionAgg struct {
	fn     string // "argMax" or "max"
	source string // the vantage table, e.g. "peer_events"
}

// sessionAggregates returns, per dashboard target, every aggregate over
// session_id it performs and what each one reads.
//
// The per-OCCURRENCE granularity is the point. A single target routinely does
// both of these, and they are different questions with different right
// answers:
//
//   - over peer_events it RESOLVES "which session is this router's current
//     one", where max(session_id) is the definition query/ uses;
//   - over a route or link-state table it CARRIES that row's own last-observed
//     session through the row's dedup, next to argMax(next_hop, ...), where
//     argMax is exactly right.
//
// Judging a target by whether its text contains "argMax(session_id" anywhere
// conflates the two. That was this walk's second draft, and it reported the
// five looking-glass and asn-view targets as unfixed after they had been
// fixed, because each still carries a legitimate route-table argMax beside the
// session resolution.
func sessionAggregates(t *testing.T) map[string][]sessionAgg {
	t.Helper()
	out := map[string][]sessionAgg{}
	for _, dash := range allDashboards {
		for _, tg := range dashboardTargets(t, dash) {
			flat := strings.Join(strings.Fields(tg.rawSQL), " ")
			where := dash + " / " + tg.panel + " / " + tg.refID
			// The two tokens are disjoint and need no deduping: "argMax" has
			// a capital M, so a search for "max(session_id" cannot match
			// inside "argMax(session_id".
			//
			// That is worth stating because getting it wrong is silent in the
			// worst direction. A walk that scans for "max(session_id" alone
			// and infers argMax from a preceding "arg" never matches, so
			// every target that resolves its session with argMax disappears
			// from the walk entirely and the invariant test passes by having
			// nothing to check. Mutating a dashboard back to argMax and
			// watching the guard stay green is what shows it.
			for _, fn := range []string{"argMax", "max"} {
				for _, seg := range strings.Split(flat, fn+"(session_id")[1:] {
					_, after, ok := strings.Cut(seg, "FROM vantage.")
					if !ok {
						continue
					}
					rest := after
					end := strings.IndexAny(rest, " \t)")
					if end < 0 {
						end = len(rest)
					}
					out[where] = append(out[where], sessionAgg{fn, rest[:end]})
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no dashboard target aggregates session_id at all, so every " +
			"invariant over them is vacuous -- the walk is wrong, not the " +
			"dashboards")
	}
	return out
}

// sessionSources are the tables a dashboard resolves "which session is
// current" from: peer_events, and peer_current, which holds the same rows for
// every retained session and no retention TTL. The panels that answer "what
// is live now" read peer_current, so a session older than history retention
// is still found; both are held to the same rules below.
var sessionSources = map[string]bool{"peer_events": true, "peer_current": true}

// TestDashboardsResolveTheCurrentSessionTheWayQueryDoes is the guard that
// replaced a disagreement.
//
// "Which session is this router's current one" had two implementations that
// did not compute the same function: query/query.go's peerStateCTE used
// max(session_id), and every dashboard that asked used
// argMax(session_id, (ts_collector, stream_seq)) over peer_events. Both
// choices were written down and neither cited the other, so nothing anywhere
// reported that they differed -- each side was tested, mutation-tested even,
// only against itself.
//
// Resolved on 2026-09-04 in favor of max(session_id), on the argument that
// the dashboard expression was the more clock-dependent of the two despite
// having been chosen for the opposite reason: ts_collector is a collector wall
// clock and LEADS that sort tuple, so stream_seq breaks only exact ties.
// argMax loses to a backward clock step of any size between two sessions;
// max loses only to one larger than the previous session's whole age.
//
// Checked against the lab archive before the change, not only reasoned:
// 1,208 session transitions over 15 router views, zero inversions of
// session_id against first-seen time, zero rows where ts_collector went
// backwards, and zero (collector, router) pairs where the two expressions
// disagreed. So the convergence changed no answer the lab archive can give,
// which is the safety question; it does not by itself prove the choice is
// safe in general, and what would falsify it is a collector restart that
// steps its clock backward.
//
// This asserts the expression rather than an answer, which is the same trade
// TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree makes on the Go side and
// for the same reason: the fixture that used to tell the two apart could only
// do so by encoding an id ordering a collector never produces.
func TestDashboardsResolveTheCurrentSessionTheWayQueryDoes(t *testing.T) {
	// Read query/'s definition rather than restating it, so this cannot drift
	// away from the thing it asserts agreement WITH.
	cte, err := os.ReadFile(filepath.Join("..", "query", "query.go"))
	if err != nil {
		t.Fatalf("read query/query.go: %v", err)
	}
	if !strings.Contains(string(cte), "max(session_id) AS sid") {
		t.Fatal("query/'s peerStateCTE no longer resolves the session with " +
			"max(session_id); this test asserts agreement with it and must be " +
			"updated with it, not around it")
	}
	var checked int
	for where, aggs := range sessionAggregates(t) {
		for _, a := range aggs {
			if !sessionSources[a.source] {
				continue
			}
			checked++
			if a.fn == "argMax" {
				t.Errorf("%s resolves the current session with argMax(session_id, ...) "+
					"over %s. That was the old resolution and it was "+
					"reversed deliberately: ts_collector leads its sort tuple and is "+
					"a collector wall clock, so it loses to a backward step of any "+
					"size. Use max(session_id), as query/ does", where, a.source)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no dashboard target resolves a session from peer_events or peer_current, so " +
			"this invariant is vacuous -- the walk is wrong, not the dashboards")
	}
}

// sessionScopedByCollector reports whether sql resolves its session per
// collector rather than picking one arbitrarily.
//
// It demands all THREE parts of the transformation, because any one of them
// alone leaves the bug in place: the session CTE must group by collector_id,
// and the outer query must filter on the (collector_id, session_id) PAIR
// rather than on a scalar session id. Comments are stripped first -- the
// predicate this replaced did not, and accepted a comment as a fix.
func sessionScopedByCollector(sql string) bool {
	var b strings.Builder
	for line := range strings.SplitSeq(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString(" ")
	}
	flat := strings.Join(strings.Fields(b.String()), " ")
	groupsByCollector := strings.Contains(flat, "GROUP BY collector_id")
	// One kind of panel filters on the session; another joins it and displays
	// the comparison. Both are correct, and a predicate that only recognized
	// the filtering shape would mark the five looking-glass and asn-view
	// panels unscoped forever.
	pairFiltered := strings.Contains(flat, "(collector_id, session_id) IN")
	pairJoined := pairJoinRe.MatchString(flat)
	scalarFiltered := strings.Contains(flat, "session_id = (SELECT sid")
	return groupsByCollector && (pairFiltered || pairJoined) && !scalarFiltered
}

// pairJoinRe matches a join whose ON clause equates collector_id on both
// sides. A join on router_ip alone is exactly the defect, so the collector
// equality is the whole test.
var pairJoinRe = regexp.MustCompile(`ON \w+\.collector_id = \w+\.collector_id`)

// TestSessionScopedByCollectorRejectsCosmeticMentions pins what the debt
// ratchet is allowed to accept.
//
// The predicate it replaced was strings.Contains(sql, "collector_id") against
// raw SQL. That accepted a SQL COMMENT mentioning the column, and accepted
// collector_id in a SELECT list while the session subquery still returned one
// scalar for every collector. Sixteen panels could have been marked fixed,
// the debt list emptied and the suite left green with every panel still
// discarding a view.
func TestSessionScopedByCollectorRejectsCosmeticMentions(t *testing.T) {
	scoped := `WITH current_session AS (
		SELECT collector_id, max(session_id) AS sid FROM vantage.peer_events
		WHERE router_ip = toIPv6('$router') GROUP BY collector_id)
		SELECT collector_id AS collector FROM vantage.ls_nodes
		WHERE (collector_id, session_id) IN (SELECT collector_id, sid FROM current_session)`

	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		{"the real shape", scoped, true},
		{"a comment mentioning the column", "-- collector_id is not used here\n" +
			"SELECT max(session_id) FROM vantage.peer_events WHERE router_ip = x", false},
		// A fake pattern hidden in a comment, matching BOTH remaining conjuncts
		// (GROUP BY collector_id and the pair filter), attached to real SQL
		// that has neither. Only a working comment stripper rejects this --
		// without it, the fake pattern reads as the real shape.
		{"a comment hiding a whole fake scope", "-- fake: GROUP BY collector_id ... " +
			"WHERE (collector_id, session_id) IN (SELECT collector_id, sid FROM current_session)\n" +
			"SELECT max(session_id) FROM vantage.peer_events WHERE router_ip = x", false},
		{"selected but not grouped", "SELECT collector_id, max(session_id) AS sid " +
			"FROM vantage.peer_events WHERE router_ip = x", false},
		{"grouped but filtered on the scalar", `WITH current_session AS (
			SELECT collector_id, max(session_id) AS sid FROM vantage.peer_events
			WHERE router_ip = x GROUP BY collector_id)
			SELECT a FROM vantage.ls_nodes WHERE session_id = (SELECT sid FROM current_session)`, false},
		// A real pair filter alongside a leftover scalar filter -- the shape
		// of a panel half-migrated from the old scalar comparison, keeping
		// both rather than replacing one with the other. The scalar leftover
		// still lets the old, uncollector-scoped comparison decide some rows,
		// so this must be rejected even though the pair filter is genuine.
		{"pair-filtered but a scalar leftover still filters too", `WITH current_session AS (
			SELECT collector_id, max(session_id) AS sid FROM vantage.peer_events
			WHERE router_ip = toIPv6('$router') GROUP BY collector_id)
			SELECT collector_id AS collector FROM vantage.ls_nodes
			WHERE (collector_id, session_id) IN (SELECT collector_id, sid FROM current_session)
			OR session_id = (SELECT sid FROM current_session)`, false},
		{"form B, joined on the pair", `SELECT m.a FROM (SELECT collector_id, router_ip FROM vantage.route_unicast
			GROUP BY collector_id, router_ip) AS m LEFT JOIN (SELECT collector_id, router_ip,
			max(session_id) AS sid FROM vantage.peer_events GROUP BY collector_id, router_ip) AS s
			ON s.collector_id = m.collector_id AND s.router_ip = m.router_ip`, true},
		{"form B, joined on the router alone", `SELECT m.a FROM (SELECT collector_id, router_ip FROM vantage.route_unicast
			GROUP BY collector_id, router_ip) AS m LEFT JOIN (SELECT collector_id, router_ip,
			max(session_id) AS sid FROM vantage.peer_events GROUP BY collector_id, router_ip) AS s
			ON s.router_ip = m.router_ip`, false},
	} {
		if got := sessionScopedByCollector(tc.sql); got != tc.want {
			t.Errorf("%s: sessionScopedByCollector = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// dashboardsNotYetCollectorScoped is every dashboard target that resolves a
// session without keying it on collector_id.
//
// Session identity is (collector_id, router_ip) -- query/'s peerStateCTE
// groups on the pair, and TestRoutersDoesNotDropASecondCollectorsView pins
// what picking a winner between two collectors silently costs. A target that
// keys on the router alone resolves a dual-homed router to one collector and
// discards the other's entire view, unreported.
//
// Unlike the argMax-versus-max disagreement above, that was never a
// question with two defensible answers. It was a gap, latent only because
// the lab archive has one collector, and it stopped being latent the
// moment a router could be watched by two collectors at once.
//
// The list is EMPTY as of 2026-09-19, and it was twenty entries long. It
// stayed a list for as long as it did because the fix was not a SQL edit.
// Eleven of the entries were link-state panels scoped by a $router variable
// and returning a scalar session, and making them collector-correct meant
// giving those dashboards a $collector variable and deciding what a
// dual-homed router should render as -- two rows, or one merged view. Five
// more were looking-glass and asn-view panels of the other shape, which
// never filter on the session at all: they join a per-router current session
// and DISPLAY the comparison, so their bug was a wrong label rather than a
// missing row. The last four were the $focus pickers, template variables
// rather than panel targets, invisible to this walk until it was widened to
// templating.list[].
//
// The list must SHRINK, and empty is where it stops. A new target resolving a
// session without collector_id fails this test, and so does leaving a fixed
// one in the list.
//
// Neither clause forbids writing a NEW entry by hand: the first clause skips
// any target the list already names, and the second is satisfied because the
// target really is unscoped. A hand-written entry therefore SILENCES the
// error rather than being rejected by it, and only review catches that. Which
// is the argument for the list being empty rather than merely short -- an
// empty map makes the addition a visible diff on this constant instead of one
// more line in a list of twenty.
//
// Membership is decided by sessionScopedByCollector, not by whether the SQL
// mentions collector_id anywhere. The substring predicate this replaced
// accepted a comment and accepted a SELECT-list mention, so it could not tell
// a fixed panel from a cosmetically-edited one.
var dashboardsNotYetCollectorScoped = map[string]bool{}

func TestDashboardSessionScopingDebtOnlyShrinks(t *testing.T) {
	unscoped := map[string]bool{}
	for where, aggs := range sessionAggregates(t) {
		resolvesSession := false
		for _, a := range aggs {
			if sessionSources[a.source] {
				resolvesSession = true
			}
		}
		if !resolvesSession {
			continue
		}
		sql := ""
		for _, dash := range allDashboards {
			for _, tg := range dashboardTargets(t, dash) {
				if dash+" / "+tg.panel+" / "+tg.refID == where {
					sql = tg.rawSQL
				}
			}
		}
		if !sessionScopedByCollector(sql) {
			unscoped[where] = true
		}
	}
	for where := range unscoped {
		if !dashboardsNotYetCollectorScoped[where] {
			t.Errorf("%s resolves a session without collector_id and is not in "+
				"the known-gap list: session identity is (collector_id, "+
				"router_ip), and grouping on the router alone discards one "+
				"collector's whole view of a dual-homed router. Do not add it "+
				"to the list -- key the resolution on the pair", where)
		}
	}
	for where := range dashboardsNotYetCollectorScoped {
		if !unscoped[where] {
			t.Errorf("%s is listed as not collector-scoped but no longer is; "+
				"remove it from dashboardsNotYetCollectorScoped so the list "+
				"keeps meaning what it says", where)
		}
	}
}

// TestCollectorScopedPanelsRepeatAndNameTheirCollector enforces, in code
// instead of by eye, the rule that a panel answering for ONE collector
// repeats over $collector, lays its tiles out horizontally, and names the
// collector it answers for in its own heading.
//
// Two tiles carrying the identical heading and two different numbers are not
// two answers -- they read as a contradiction, and that shipped once before
// a human reading caught it. It was then missing three more times, caught by
// hand each time. A fourth reading is not a guard, and nothing in this
// package had ever read a panel's repeat property at all: the titles are
// pinned, because panelSQL fatals on a missing panel, but the property that
// makes those titles mean anything was not.
//
// The walk is BIDIRECTIONAL and keyed on the SQL rather than on a list of
// panel names, because both directions are real defects:
//
//   - a panel whose SQL filters on $collector but does NOT repeat renders one
//     tile holding one collector's answer under a heading that claims to
//     describe the router. That is the original defect with a picker bolted
//     on, and it is strictly worse than before, because the picker makes the
//     loss look deliberate.
//   - a panel that repeats over $collector but whose SQL IGNORES it renders N
//     identical tiles: the same number N times under N different headings,
//     asserting that two collectors agree when nothing compared them.
//
// It also closes the includeAll-without-allValue risk: these pickers are
// multi with no allValue, so Grafana renders All as
// 'c1','c2' into a slot the SQL has already quoted -- a syntax error rather
// than a quietly wrong tile, and reachable only by removing the repeat. The
// repeat requirement below is what makes that unreachable rather than merely
// unlikely.
//
// Neither the layout nor the heading is visible to panelSQL, which reads
// rawSql and knows nothing about either.
func TestCollectorScopedPanelsRepeatAndNameTheirCollector(t *testing.T) {
	repeated := 0
	// Panels that sit on a dashboard carrying the $collector picker and
	// deliberately do NOT repeat -- the looking glasses' tables, the
	// link-state object tables, the merged topology graph. They are what
	// makes the bidirectional check more than a tautology: if the walk only
	// ever saw repeated panels, "repeat matches the SQL" would be satisfied
	// by every panel for the same reason.
	deliberatelyNotRepeated := 0
	// allDashboards is the universe here, and it is a hand-written list --
	// TestAllDashboardsMatchesTheProvisionedDirectory is what keeps it equal
	// to what Grafana is actually given.
	for _, dash := range allDashboards {
		multi, includeAll, hasPicker := dashboardVariableMultiplicity(t, dash, "collector")
		// dashboardPanels only returns panels that HAVE targets, so a row or
		// text panel carrying repeat: "collector" escapes this walk. None
		// exists today, and widening the walk would pull in every row header
		// for no gain, so the limitation is recorded rather than closed.
		for _, p := range dashboardPanels(t, dash) {
			where := dash + " / " + p.title
			filters := varUse(p.sql, "collector")
			repeats := p.repeat == "collector"
			switch {
			case filters && !repeats:
				t.Errorf("%s filters its SQL on $collector but does not repeat "+
					"over it (repeat=%q). It renders ONE tile answering for "+
					"whichever collector the picker holds, under a heading that "+
					"reads as the router's whole answer. Set repeat to "+
					"\"collector\"", where, p.repeat)
				continue
			case repeats && !filters:
				t.Errorf("%s repeats over $collector but its SQL never "+
					"references the variable, so every tile runs the identical "+
					"query. A dual-homed router draws N tiles carrying the same "+
					"number under N different collector headings, which asserts "+
					"the two collectors agree when nothing compared them",
					where)
				continue
			case !repeats:
				if hasPicker {
					deliberatelyNotRepeated++
				}
				continue
			}
			repeated++
			if p.repeatDirection != "h" {
				t.Errorf("%s repeats over $collector with repeatDirection=%q, "+
					"want \"h\". Grafana stacks a vertical repeat down the "+
					"dashboard, so the second collector's tile lands below the "+
					"fold and a dual-homed router reads as a single-collector "+
					"one until an operator scrolls", where, p.repeatDirection)
			}
			if !varUse(p.title, "collector") {
				t.Errorf("%s repeats over $collector but its title %q does not "+
					"name the variable, so Grafana renders every tile with the "+
					"identical heading. Two identical headings over two "+
					"different numbers is indistinguishable from a "+
					"contradiction -- put $collector in the title",
					where, p.title)
			}
			if !hasPicker {
				t.Errorf("%s repeats over $collector but %s declares no "+
					"$collector template variable, so Grafana has nothing to "+
					"repeat over and renders the panel once, unsubstituted",
					where, dash)
				continue
			}
			if !multi {
				t.Errorf("%s repeats over $collector but %s declares that "+
					"variable with multi=false, so the picker holds one value "+
					"and the repeat renders exactly one tile however many "+
					"collectors watch the router", where, dash)
			}
			if !includeAll {
				t.Errorf("%s repeats over $collector but %s declares that "+
					"variable with includeAll=false, so the dashboard's default "+
					"view is one collector's and the second only appears if an "+
					"operator knows to select it", where, dash)
			}
		}
	}
	if repeated == 0 {
		t.Fatal("no panel in any dashboard repeats over $collector, so this " +
			"invariant is vacuous -- the walk is wrong, not the dashboards")
	}
	if deliberatelyNotRepeated == 0 {
		t.Fatal("every panel on every $collector-bearing dashboard repeats, " +
			"so the half of this check that rejects an unwanted repeat never " +
			"ran. The looking glasses' tables and the merged topology graph " +
			"are supposed to take the other side")
	}
}

// TestParseAnomalyDetectorSaysWhenAQuirkWasLastSeen covers the difference
// between "this is happening" and "this happened".
//
// parse_flags are a record of the decoder AS IT WAS when a row was written.
// Fix the cause and the flag stops appearing on new rows, while every row that
// already carries it carries it forever -- so a retired quirk keeps its whole
// event count, its whole session count and its whole spread, and outranks a
// live one indefinitely on a panel that ranks by spread.
//
// This is not hypothetical. The lab archive carries 1,403
// PARSE_FLAG_VERSION_UNPARSED events across 263 sessions whose last sighting
// is 2026-08-17, the day the decoder began deriving vendor identity from the
// captured banner, in a lab archive that runs to 2026-08-30. The panel
// titled "the new-quirk detector" ranks it second and says nothing about it
// being over.
//
// A time filter alone does not answer it: the panel is scoped to the
// dashboard's range, so the rows are legitimately in scope. What is missing is
// WHEN, which is one column.
func TestParseAnomalyDetectorSaysWhenAQuirkWasLastSeen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertParseAnomalyFixture(t, ctx, ch)

	var fixed, live *paRow
	for _, r := range parseAnomalyRows(t, ctx, ch) {
		switch r.platform {
		case "dash-pa-fixed":
			fixed = &r
		case "dash-pa-broad":
			live = &r
		}
	}
	if fixed == nil || live == nil {
		t.Fatal("fixture routers missing from the panel")
	}
	if fixed.lastSeen.IsZero() {
		t.Fatal("the panel reports no last_seen, so a quirk that stopped days " +
			"ago is indistinguishable from one happening now")
	}
	if !fixed.lastSeen.Before(live.lastSeen) {
		t.Errorf("the fixed quirk's last_seen (%s) is not before the live "+
			"one's (%s); the column has to order them or it says nothing",
			fixed.lastSeen, live.lastSeen)
	}
	// The live quirk's FIRST sighting is older than the fixed one's last, so
	// reporting min(ts_collector) here would invert the two. That is what
	// makes this an assertion about max rather than merely about age.
	if !fixed.lastSeen.After(paFixedQuirkTS.Add(-time.Minute)) {
		t.Errorf("fixed quirk last_seen = %s, want its NEWEST row (~%s); a "+
			"panel reporting the oldest row would pass the ordering check "+
			"above while answering a different question",
			fixed.lastSeen, paFixedQuirkTS.Add(5*time.Minute))
	}
	// And the reason the column is needed at all: the retired quirk still
	// outranks the live one on the panel's own ranking.
	if fixed.affected <= live.affected {
		t.Errorf("fixture is not adversarial: the fixed quirk affects %d "+
			"objects and the live one %d, so ranking already separates them "+
			"and last_seen is not load-bearing here",
			fixed.affected, live.affected)
	}
}

// The split-view fixture: ONE router, TWO collectors, and three prefixes that
// differ only in which collectors still advertise them.
//
// It is separate from insertDualHomedFixture because that fixture's counts are
// measured and quoted in a dozen assertions; adding a route under its router
// would move every one of them. Kept apart by reserved addresses, which is
// this file's convention: router 10.0.199.19 and the 10.210.0.0/16 block are
// claimed by nothing else here.
const (
	splitRouterIP   = "10.0.199.19"
	splitPeerIP     = "10.210.255.1"
	splitSysname    = "split-r1"
	splitSessionA   = uint64(8_100_000_000_000_000_000)
	splitSessionB   = uint64(2_100_000_000_000_000_000)
	splitTransitASN = uint32(64596)
	splitOriginASN  = uint32(64597)

	// splitPfxLiveOnB is the whole point: both collectors heard it announced,
	// then collector A heard it withdrawn and collector B did not. The router
	// is still advertising it as far as B can tell, and asn-view must still
	// count it.
	splitPfxLiveOnB = "10.210.0.0/24"
	// splitPfxLiveOnBoth is the control: never withdrawn anywhere. Without it
	// a panel that returned nothing at all would be indistinguishable from one
	// that correctly dropped a withdrawn prefix.
	splitPfxLiveOnBoth = "10.210.1.0/24"
	// splitPfxWithdrawnByBoth takes the other side, and is the reason this
	// fixture can falsify a bad fix as well as a bad panel. Adding collector_id
	// to the GROUP BY is not the only way to make splitPfxLiveOnB reappear --
	// deleting the `is_withdraw = 0` filter does it too, and would pass every
	// assertion built on the first two prefixes alone. This one must stay
	// uncounted, so that shortcut fails.
	splitPfxWithdrawnByBoth = "10.210.2.0/24"
)

// insertSplitViewFixture writes the three prefixes above under one router seen
// by two collectors.
//
// Every row carries the same ts_collector, so the argMax inside each panel is
// decided by stream_seq -- which is insertion order here. The withdrawals are
// written LAST and therefore win their group, which is precisely the condition
// the merged panels mishandle: with no collector_id in the GROUP BY, collector
// A's withdrawal of splitPfxLiveOnB is the argMax winner for a group that also
// holds collector B's live announcement, and the prefix disappears from a
// dashboard whose own Prefixes table still lists it as advertised on B.
//
// The withdrawals carry their path attributes, matching what the asn-view
// fixture already does for asnPfxWithdrawn. That is deliberate: it leaves
// is_withdraw as the only thing that can drop these rows. A withdrawal with no
// attributes would be caught by the panels' `notEmpty(as_path)` instead, and
// the test would pass for a reason unrelated to the defect.
func insertSplitViewFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	ts := corpusCollectorClock
	attrs := &vantagev1.PathAttributes{
		Origin: 0, NextHop: "10.210.255.254",
		AsPath: []*vantagev1.AsPathSegment{{
			Type: 2, Asns: []uint32{splitTransitASN, splitOriginASN},
		}},
	}
	ev := func(collector string, sessionID uint64, r *vantagev1.RouteEvent) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: splitRouterIP, SysName: splitSysname},
			Peer:        &vantagev1.PeerId{Ip: splitPeerIP},
			SessionId:   sessionID,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	announce := func(collector string, sessionID uint64, pfx string) *vantagev1.Envelope {
		return ev(collector, sessionID, &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 1}, Attrs: attrs,
			Announced: []*vantagev1.Prefix{{Prefix: pfx}},
		})
	}
	withdraw := func(collector string, sessionID uint64, pfx string) *vantagev1.Envelope {
		return ev(collector, sessionID, &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 1}, Attrs: attrs,
			Withdrawn: []*vantagev1.Prefix{{Prefix: pfx}},
		})
	}
	envs := []*vantagev1.Envelope{
		announce(dualCollectorA, splitSessionA, splitPfxLiveOnB),
		announce(dualCollectorB, splitSessionB, splitPfxLiveOnB),
		announce(dualCollectorA, splitSessionA, splitPfxLiveOnBoth),
		announce(dualCollectorB, splitSessionB, splitPfxLiveOnBoth),
		announce(dualCollectorA, splitSessionA, splitPfxWithdrawnByBoth),
		announce(dualCollectorB, splitSessionB, splitPfxWithdrawnByBoth),
		// Written last, so these are the argMax winners in their groups.
		withdraw(dualCollectorA, splitSessionA, splitPfxLiveOnB),
		withdraw(dualCollectorA, splitSessionA, splitPfxWithdrawnByBoth),
		withdraw(dualCollectorB, splitSessionB, splitPfxWithdrawnByBoth),
	}
	for i, e := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, e, uint64(i+1))); err != nil {
			t.Fatalf("insert split-view envelope %d: %v", i, err)
		}
	}
}

// splitViewPanelRows runs one asn-view panel against the split-view router.
func splitViewPanelRows(t *testing.T, ctx context.Context, ch *ClickHouse, panel, refID string) [][]string {
	t.Helper()
	sql := substituteGrafana(
		scopeToRouter(t, panelSQL(t, "asn-view", panel, refID), splitRouterIP),
		map[string]string{"asn": fmt.Sprint(splitOriginASN), "rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("asn-view %q: %v\nSQL:\n%s", panel, err, sql)
	}
	defer rows.Close()
	return dashRowsAsStrings(t, rows)
}

// TestSplitViewASNCountsKeepAPrefixOneCollectorStillAdvertises is the defect
// these panels shipped with.
//
// Their inner GROUP BY is (router_ip, router_sysname, peer_ip, rib, prefix,
// path_id) with no collector_id in it, so both collectors' sightings of one
// prefix are ONE group and `argMax(is_withdraw, (ts_collector, stream_seq))`
// picks a single verdict for both of them. That comparison has no meaning
// across collectors: ts_collector is each collector's own wall clock, and the
// two are not synchronized. Whichever collector's clock ran ahead decides
// whether a prefix is live -- a collection artifact deciding a fact about the
// network, which is this project's most recurring defect.
//
// Concretely, collector A's withdrawal wins the merged group and
// `WHERE l.is_withdraw = 0` then drops splitPfxLiveOnB entirely, while the
// same dashboard's Prefixes table still lists it as
// advertised on collector B. The two halves of one screen disagree.
//
// The answer is the union of the collectors' views, which is what the topology
// merge already does: resolve state per collector, filter, then merge. A
// prefix is live if any collector still sees it live.
func TestSplitViewASNCountsKeepAPrefixOneCollectorStillAdvertises(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSplitViewFixture(t, ctx, ch)

	// column holding `originated` in each panel's own SELECT list.
	for _, p := range []struct {
		panel string
		col   int
	}{
		{"ASNs", 1},         // asn, originated, transited, routers, neighbours
		{"What we know", 0}, // originated, transited, routers, neighbours, longest_path
	} {
		got := splitViewPanelRows(t, ctx, ch, p.panel, "A")
		if len(got) != 1 {
			t.Fatalf("asn-view %q returned %d rows for AS %d, want 1", p.panel, len(got), splitOriginASN)
		}
		if originated := got[0][p.col]; originated != "2" {
			t.Errorf("asn-view %q reports originated=%s for AS %d, want 2 (%s, still "+
				"advertised by %s, and %s). Collector %s withdrew %s and %s did not, "+
				"but the inner GROUP BY carries no collector_id, so both collectors' "+
				"sightings are one group and the withdrawal -- written last, so the "+
				"argMax winner -- speaks for a collector that never heard it. A "+
				"count of 3 is the opposite failure: the is_withdraw filter is gone "+
				"and %s, withdrawn by BOTH collectors, is being counted as live",
				p.panel, originated, splitOriginASN, splitPfxLiveOnB, dualCollectorB,
				splitPfxLiveOnBoth, dualCollectorA, splitPfxLiveOnB, dualCollectorB,
				splitPfxWithdrawnByBoth)
		}
	}
}

// TestSplitViewAdjacentASNsKeepsThePairOneCollectorStillAdvertises covers the
// third panel sharing that inner subquery. It counts PREFIXES per adjacency,
// so the same merged group that hides splitPfxLiveOnB from the ASN tables
// also undercounts the AS pair that carries it.
func TestSplitViewAdjacentASNsKeepsThePairOneCollectorStillAdvertises(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSplitViewFixture(t, ctx, ch)

	got := splitViewPanelRows(t, ctx, ch, "Adjacent ASNs", "A")
	// asn, neighbour, direction, prefixes -- one row: AS 64597 sees 64596
	// toward the collector on both live prefixes.
	if len(got) != 1 {
		t.Fatalf("Adjacent ASNs returned %d rows for AS %d, want 1: %v", len(got), splitOriginASN, got)
	}
	if prefixes := got[0][3]; prefixes != "2" {
		t.Errorf("Adjacent ASNs reports %s prefixes for AS %d -> %d, want 2. %s is "+
			"still advertised by %s and must count; %s is withdrawn by both "+
			"collectors and must not", prefixes, splitOriginASN, splitTransitASN,
			splitPfxLiveOnB, dualCollectorB, splitPfxWithdrawnByBoth)
	}
}

// The dual-homed peer-status fixture: one router, two collectors, three peers
// whose two collectors DISAGREE in three different ways. 10.0.202.1 and
// 10.255.202.x are claimed by nothing else here.
const (
	psRouter        = "10.0.202.1"
	psSysname       = "dash-ps-dual"
	psPeerLostOnA   = "10.255.202.1"
	psPeerLostOnAll = "10.255.202.2"
	psPeerDownOnA   = "10.255.202.3"
	psPeerDownOnly  = "10.255.202.4"
	psCollectorA    = "ps-c1"
	psCollectorB    = "ps-c2"
)

// insertPeerStatusDualHomedFixture writes three peers whose collectors
// disagree, each disagreement of a different kind.
//
// Collector A's events are written LAST and carry the newest ts_collector
// throughout, so A wins any argMax that is allowed to span the two -- which is
// what makes the defect observable rather than merely wrong in principle.
func insertPeerStatusDualHomedFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	now := corpusCollectorClock
	ev := func(collector, peer string, session uint64, tsRouter, tsCollector time.Time, k vantagev1.PeerEvent_Kind) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: psRouter, SysName: psSysname},
			Peer:        &vantagev1.PeerId{Ip: peer, Asn: 65000},
			SessionId:   session,
			TsRouter:    timestamppb.New(tsRouter),
			TsCollector: timestamppb.New(tsCollector),
			Payload:     &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{Kind: k}},
		}
	}
	up, down, lost := vantagev1.PeerEvent_KIND_UP, vantagev1.PeerEvent_KIND_DOWN, vantagev1.PeerEvent_KIND_VIEW_LOST
	envs := []*vantagev1.Envelope{
		// Peer 1: the router said UP once and both collectors heard it. Then
		// collector A's transport dropped and it emitted view_lost. B still
		// holds the session. The peer is UP -- view_lost is A's statement
		// about A, not the router's about the peer.
		ev(psCollectorB, psPeerLostOnA, 2, now.Add(-30*time.Minute), now.Add(-30*time.Minute), up),
		ev(psCollectorA, psPeerLostOnA, 1, now.Add(-30*time.Minute), now.Add(-30*time.Minute), up),
		ev(psCollectorA, psPeerLostOnA, 1, now.Add(-30*time.Minute), now.Add(-1*time.Minute), lost),

		// Peer 2: BOTH collectors lost their view. Nobody can see it, so
		// view_lost is the honest answer and must survive the fix.
		ev(psCollectorB, psPeerLostOnAll, 2, now.Add(-30*time.Minute), now.Add(-30*time.Minute), up),
		ev(psCollectorA, psPeerLostOnAll, 1, now.Add(-30*time.Minute), now.Add(-30*time.Minute), up),
		ev(psCollectorB, psPeerLostOnAll, 2, now.Add(-30*time.Minute), now.Add(-2*time.Minute), lost),
		ev(psCollectorA, psPeerLostOnAll, 1, now.Add(-30*time.Minute), now.Add(-1*time.Minute), lost),

		// Peer 3: the ROUTER said down, and only collector A heard it -- B's
		// newest router statement is the older up. This is the branch that
		// stops the fix from being "any collector says up wins": the peer is
		// DOWN, and ts_router is the only clock the two collectors share.
		ev(psCollectorB, psPeerDownOnA, 2, now.Add(-30*time.Minute), now.Add(-30*time.Minute), up),
		ev(psCollectorA, psPeerDownOnA, 1, now.Add(-30*time.Minute), now.Add(-30*time.Minute), up),
		ev(psCollectorA, psPeerDownOnA, 1, now.Add(-5*time.Minute), now.Add(-4*time.Minute), down),

		// Peer 4: the lab archive holds a router statement for it, and that
		// statement is a DOWN -- there is no up anywhere. Collector A then
		// lost its view, with a NEWER ts_router than the down.
		//
		// This branch exists to pin the guard's predicate rather than only
		// the selection's. A guard asking "did any collector see an up"
		// instead of "did any collector hear the ROUTER" behaves identically
		// on every other peer here, and on this one it falls through to a
		// merge that lets A's view_lost outrank B's down.
		ev(psCollectorB, psPeerDownOnly, 2, now.Add(-6*time.Minute), now.Add(-6*time.Minute), down),
		ev(psCollectorA, psPeerDownOnly, 1, now.Add(-2*time.Minute), now.Add(-1*time.Minute), lost),
	}
	for i, e := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, e, uint64(7000+i))); err != nil {
			t.Fatalf("insert peer-status envelope %d: %v", i, err)
		}
	}
}

// TestFleetPeerStatusResolvesEachCollectorBeforeCountingPeers pins the fix
// for "Peer status", the pie an operator reads first: it resolved each
// peer's verdict with argMax(kind, (ts_collector, stream_seq)) GROUP BY
// (router_ip, peer_ip) -- no collector in the grouping. ts_collector is each
// collector's own wall clock, so with two collectors watching one router the
// peer's headline state was decided by whichever clock ran ahead.
//
// The concrete harm is specific, not theoretical: KIND_VIEW_LOST is defined in
// collector's Session.Close as the COLLECTOR stating it stopped being able to
// see the peer -- "deliberately NOT a Peer Down", because the router said
// nothing. One collector losing its transport therefore painted a peer as lost
// on a screen whose whole job is telling an operator where to look, while the
// other collector was still watching it perfectly well.
func TestFleetPeerStatusResolvesEachCollectorBeforeCountingPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertPeerStatusDualHomedFixture(t, ctx, ch)

	raw := scopeFleetPeerStatusToRouter(t, panelSQL(t, "fleet-health", "Peer status", "A"), psRouter)
	var up, down, viewLost, unspecified, stale uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, substituteGrafana(raw, nil))).
		Scan(&up, &down, &viewLost, &unspecified, &stale); err != nil {
		t.Fatalf("fleet-health peer status: %v", err)
	}
	// Every collector here is immortal (chtest's test-only beats), so none
	// reads stale; TestFleetPeerStatusAppliesCollectorLiveness owns that.
	if stale != 0 {
		t.Errorf("stale = %d, want 0", stale)
	}

	if got := up + down + viewLost + unspecified; got != 4 {
		t.Fatalf("the four counts total %d, want 4 -- four peers, each landing "+
			"in exactly one bucket. 8 means the collectors are being counted "+
			"as separate peers", got)
	}
	if up != 1 {
		t.Errorf("up = %d, want 1. %s is up: collector %s lost its transport "+
			"and said so, but %s still holds the session and the ROUTER never "+
			"said anything. A view_lost is the collector's statement about "+
			"itself, and letting it outrank another collector's live view "+
			"reports a healthy peer as lost",
			up, psPeerLostOnA, psCollectorA, psCollectorB)
	}
	if down != 2 {
		t.Errorf("down = %d, want 2. %s is down: the router said so and only "+
			"collector %s heard it, so %s's newest router statement is a stale "+
			"up. ts_router is the only clock the two collectors share, and it "+
			"is what has to settle a disagreement between two ROUTER "+
			"statements. %s is the other one, and it is the peer that pins "+
			"what the guard asks: its only router statement is a down, so a "+
			"guard testing for an `up` rather than for any router statement "+
			"falls through and lets a view_lost outrank it",
			down, psPeerDownOnA, psCollectorA, psCollectorB, psPeerDownOnly)
	}
	if viewLost != 1 {
		t.Errorf("view_lost = %d, want 1. %s really is lost -- BOTH collectors "+
			"dropped their transports -- and a fix that simply ignores "+
			"view_lost whenever any collector holds an older router statement "+
			"would report it as up, which is the opposite error",
			viewLost, psPeerLostOnAll)
	}
	if unspecified != 0 {
		t.Errorf("unspecified = %d, want 0", unspecified)
	}
}

// scopeFleetPeerStatusToRouter narrows "Peer status" to one router. It anchors
// on the per-view grouping, psInnerGrouping, which is the inner one: the
// panel resolves each collector's view before merging them, so that is where
// a WHERE belongs.
func scopeFleetPeerStatusToRouter(t *testing.T, sql, router string) string {
	t.Helper()
	const anchor = psInnerGrouping
	if n := strings.Count(sql, anchor); n != 1 {
		t.Fatalf("the peer-status query carries %d occurrences of %q, want 1 "+
			"-- this helper scopes to a router by filtering beside that "+
			"grouping and must not do it blind", n, anchor)
	}
	return strings.Replace(sql, anchor,
		"WHERE peer_current.router_ip = toIPv6('"+router+"') "+anchor, 1)
}

// TestRouteChurnPrefixTableCountsOneCollectorsView pins the defect on the
// panel that showed it most plainly: "Most-changed prefixes" counted
// `routes` with a uniqExact over a
// tuple LEADING with collector_id, so one route two collectors both held
// counted as two ROUTES -- in a column named routes -- and its other counts
// summed the collectors' observations of the same events.
//
// dualHomedRoutePrefix is announced by BOTH collectors and is the only reason
// this is countable: every other prefix under this router belongs to one
// collector, where a merged panel and a per-collector one give the same
// answer. See the note above dualHomedRoutePrefix, which makes exactly this
// argument for the looking glasses.
func TestRouteChurnPrefixTableCountsOneCollectorsView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	sql := substituteGrafana(
		scopeToDualHomedRouter(t, panelSQL(t, "route-churn", "Most-changed prefixes", "A")),
		map[string]string{"rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("most-changed prefixes: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	type row struct{ changes, observations, routes, sessions, withdraws uint64 }
	got := map[string]row{}
	for rows.Next() {
		var p string
		var r row
		if err := rows.Scan(&p, &r.changes, &r.observations, &r.routes, &r.sessions, &r.withdraws); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[p] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	shared := got[dualHomedRoutePrefix]
	if shared.observations != 1 || shared.routes != 1 || shared.sessions != 1 {
		t.Errorf("%s: %d observations / %d routes / %d sessions, want 1/1/1. "+
			"Both collectors heard this prefix announced once, by one router, "+
			"in one session. 2/2/2 is the defect: the observations summed, "+
			"collector_id led the routes tuple so one route counted as two, "+
			"and the two collectors' own session ids counted as two sessions",
			dualHomedRoutePrefix, shared.observations, shared.routes, shared.sessions)
	}
	// The control: a prefix only one collector ever saw must be untouched by
	// any of this, or the fix is just scaling everything down.
	solo := got["10.208.1.0/24"]
	if solo.observations != 1 || solo.routes != 1 {
		t.Errorf("10.208.1.0/24: %d observations / %d routes, want 1/1 -- only "+
			"one collector ever announced it, so merging collectors must not "+
			"change it at all", solo.observations, solo.routes)
	}
}

// TestRouteChurnSeriesReportsOneCollectorsViewNotTheSumOfBoth covers the two
// route-churn panels changed on 2026-09-20 that insertRouteChurnFixture
// CANNOT see. That fixture writes collector "c1" and nothing else, so
// best-vantage-point and summing are the same number there and every existing
// route-churn assertion passes either way -- the one-branch fixture problem,
// walked into while fixing the defect it hides.
//
// insertDualHomedFixture is the one that can tell them apart: four unicast
// prefixes from collector A and two from B under one router, each announced
// once and therefore each its own session dump.
func TestRouteChurnSeriesReportsOneCollectorsViewNotTheSumOfBoth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertDualHomedFixture(t, ctx, ch)

	sql := substituteGrafana(
		scopeToDualHomedRouter(t, panelSQL(t, "route-churn",
			"Re-advertisements, session dumps and withdrawals", "A")),
		map[string]string{"rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("route-churn series: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	var readvertise, withdraw, dumps uint64
	for rows.Next() {
		var t0 time.Time
		var r, w, d uint64
		if err := rows.Scan(&t0, &r, &w, &d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		readvertise += r
		withdraw += w
		dumps += d
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if dumps != 4 || readvertise != 0 || withdraw != 0 {
		t.Errorf("got %d re-advertisements / %d withdrawals / %d session dumps, "+
			"want 0/0/4 -- collector %s announced four prefixes under this "+
			"router and %s announced two, each once, so each is that "+
			"collector's own session dump. 6 dumps is the defect: both "+
			"collectors' observations of one router summed, which counts the "+
			"prefix they BOTH carry twice and makes the series say the router "+
			"churned more because a second collector was watching",
			readvertise, withdraw, dumps, dualCollectorA, dualCollectorB)
	}
}

// The staggered-collector fixture: one router, two collectors, observation
// eras that DO NOT OVERLAP IN TIME.
//
// Every other dual-collector fixture in this file writes both collectors at
// one ts_collector, so every row both of them hold lands in the same bucket
// and a per-bucket collector choice and a per-window one are the same number.
// This one separates them, which is the only way to see the difference.
//
// It is modeled on a shape measured in the lab archive on 2026-09-20 rather
// than invented: router 172.22.0.8 carried 20 rows under dev-c1 (09-17 to
// 09-18) and 8 under dev-c2 (09-20, three minutes), with the SAME TWO
// ts_router values on both sides -- one captured session, replayed to a
// second collector two days later. The router announced those prefixes once.
//
// ts_router is what establishes that here, and it is used for NOTHING ELSE.
// It is the reader's evidence that the late collector's rows are a second
// copy rather than new events; the SQL under test must never key on it, for
// the reason churnByPeerSQL gives at length -- QK_TS_ZERO substitutes each
// COLLECTOR's own now() on a router with a dead clock, so the two collectors
// disagree exactly where the router's clock is broken.
const (
	// 203, following this file's convention of one reserved router per
	// fixture: 10.0.199.x, .200.1 and .202.1 are taken, and testDB is shared
	// and never truncated.
	stagRouterIP  = "10.0.203.1"
	stagPeerIP    = "10.255.203.1"
	stagSysname   = "stag-r1"
	stagCollector = "stag-c1"
	// stagCollectorLate observes the router LATER and sees FEWER rows, so
	// "chose the early collector", "chose the late one" and "summed both" are
	// three distinguishable answers. Equal counts could not be.
	stagCollectorLate = "stag-c2"
	stagSession       = uint64(7_000_000_000_000_000_000)
	stagSessionLate   = uint64(2_000_000_000_000_000_000)
	// The two eras, as ages before corpusCollectorClock. They must be more
	// than a bucket apart: substituteGrafana renders $__timeInterval as one
	// MINUTE, so 30 minutes of separation is 30 buckets, and both must stay
	// inside the one-DAY window it renders $__timeFilter as.
	stagEarlyAge = 40 * time.Minute
	stagLateAge  = 10 * time.Minute
)

// stagPrefixes is what the router announced, once. The late collector holds
// a copy of the first two.
var stagPrefixes = []string{
	"10.211.0.0/24", "10.211.1.0/24", "10.211.2.0/24", "10.211.3.0/24",
}

func insertStaggeredCollectorFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	// One router-side instant for every row, both collectors alike: the
	// router made these announcements once. See the note above.
	tsRouter := corpusCollectorClock.Add(-stagEarlyAge)
	routeEnv := func(collector string, sessionID uint64, at time.Time, pfx string) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: collector,
			Router:      &vantagev1.RouterId{Ip: stagRouterIP, SysName: stagSysname},
			Peer:        &vantagev1.PeerId{Ip: stagPeerIP},
			SessionId:   sessionID,
			TsRouter:    timestamppb.New(tsRouter),
			TsCollector: timestamppb.New(at),
			Payload: &vantagev1.Envelope_Route{Route: &vantagev1.RouteEvent{
				Family:    &vantagev1.Family{Afi: 1, Safi: 1},
				Attrs:     &vantagev1.PathAttributes{Origin: 0, NextHop: "10.211.255.1"},
				Announced: []*vantagev1.Prefix{{Prefix: pfx}},
			}},
		}
	}
	var envs []*vantagev1.Envelope
	early := corpusCollectorClock.Add(-stagEarlyAge)
	for _, pfx := range stagPrefixes {
		envs = append(envs, routeEnv(stagCollector, stagSession, early, pfx))
	}
	late := corpusCollectorClock.Add(-stagLateAge)
	for _, pfx := range stagPrefixes[:2] {
		envs = append(envs, routeEnv(stagCollectorLate, stagSessionLate, late, pfx))
	}
	// One RE-ADVERTISEMENT per era, of the same prefix: the router said this
	// route again, once, and each collector recorded it inside its own era.
	// Without it every row here is a session dump and the panels that count
	// `changes` -- which is countIf(NOT is_session_dump) -- report zero from
	// both collectors, where a summed answer and a best-vantage answer are
	// the same number and nothing is falsifiable.
	//
	// It shares its era's ts_collector, so it lands in that era's bucket and
	// each collector still contributes exactly one point. The classification
	// orders a session on (seq, stream_seq), never on the clock, so being
	// appended after the announcement is what makes it the later of the two.
	envs = append(envs,
		routeEnv(stagCollector, stagSession, early, stagPrefixes[0]),
		routeEnv(stagCollectorLate, stagSessionLate, late, stagPrefixes[0]))
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert envelope %d: %v", i, err)
		}
	}
}

// TestRouteChurnSeriesPicksItsCollectorOnceOverTheWindow pins the fix that
// chooses the best vantage point once per window, which the dual-homed
// fixture cannot see.
//
// The panel picks the best vantage point PER BUCKET. That defends against
// two collectors' copies of one event only when both copies land in the SAME
// bucket -- and the column that separates them into different buckets,
// ts_collector, is precisely the column that differs between collectors. So
// the per-bucket defense fails exactly where duplication is most likely: a
// collector that joins, leaves, restarts or is replayed to at a different
// time from its sibling.
//
// Measured against this fixture, whose router announced four prefixes once:
// a per-bucket choice reports 4 in the early bucket and 2 in the late one,
// SIX, and the router is made to look half again as busy because a second
// collector was handed a copy. Choosing the collector ONCE OVER THE WINDOW
// reports the early collector's four and nothing else.
func TestRouteChurnSeriesPicksItsCollectorOnceOverTheWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertStaggeredCollectorFixture(t, ctx, ch)

	sql := substituteGrafana(
		scopeToRouter(t, panelSQL(t, "route-churn",
			"Re-advertisements, session dumps and withdrawals", "A"), stagRouterIP),
		map[string]string{"rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("route-churn series: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	type point struct {
		at                          time.Time
		readvertise, withdraw, dump uint64
	}
	var pts []point
	var dumps uint64
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.at, &p.readvertise, &p.withdraw, &p.dump); err != nil {
			t.Fatalf("scan: %v", err)
		}
		pts = append(pts, p)
		dumps += p.dump
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(pts) != 1 || dumps != 4 {
		t.Errorf("got %d points totalling %d session dumps, want 1 point of 4: %+v\n"+
			"This router announced %d prefixes once, and %s was handed a copy of "+
			"two of them %s later. 2 points totalling 6 is the per-bucket choice: "+
			"each bucket picks the best collector present IN IT, so a second "+
			"collector's copy that lands in its own bucket is never compared "+
			"against the copy it duplicates and is added instead",
			len(pts), dumps, pts, len(stagPrefixes), stagCollectorLate,
			stagEarlyAge-stagLateAge)
	}
}

// TestChangesByPeerPicksItsCollectorOnceOverTheWindow pins the same rule
// on the second of route-churn's two time series.
//
// It draws a line per peer, so the collector has to be chosen once per PEER
// over the window rather than once for the panel: a collector that watches
// only some of the fleet's routers must not blank the peers it never sees,
// which is what one fleet-wide choice would do. Choosing per peer is also
// the grain ChurnByPeer already uses in the Go layer, and the panel and
// the API must not answer one question two ways.
//
// The fixture's peer was re-advertised one route, once. A per-bucket choice
// draws two points of 1 -- the same re-advertisement, counted again because
// the late collector's copy of it landed in a bucket of its own.
func TestChangesByPeerPicksItsCollectorOnceOverTheWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertStaggeredCollectorFixture(t, ctx, ch)

	sql := substituteGrafana(
		scopeToRouter(t, panelSQL(t, "route-churn", "Changes by peer", "A"), stagRouterIP),
		map[string]string{"rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("changes by peer: %v\nSQL:\n%s", err, sql)
	}
	defer rows.Close()
	type point struct {
		at      time.Time
		peer    string
		changes uint64
	}
	var pts []point
	var changes uint64
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.at, &p.peer, &p.changes); err != nil {
			t.Fatalf("scan: %v", err)
		}
		pts = append(pts, p)
		changes += p.changes
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(pts) != 1 || changes != 1 {
		t.Errorf("got %d points totalling %d changes, want 1 point of 1: %+v\n"+
			"This peer was re-advertised one route, once; %s holds a copy of "+
			"that re-advertisement recorded %s later. 2 points of 1 is the "+
			"per-bucket choice -- the two copies never share a bucket, so the "+
			"best-vantage comparison never sees them together",
			len(pts), changes, pts, stagCollectorLate, stagEarlyAge-stagLateAge)
	}
}

// TestEvpnChurnSeriesReportsOneCollectorsViewNotTheSumOfBoth applies the
// single-vantage-point rule to the panel it was left off.
//
// route-churn's series was moved to the best single vantage point; this one
// got only the PARTITION BY repair, so it still SUMS the collectors'
// observations of one router. The two panels compute the same
// three series over different tables and have disagreed ever since.
//
// The fixture's minute-121 bucket is the smallest case that shows it: one
// MAC, advertised once, recorded by BOTH collectors ten seconds apart inside
// the same minute. The router advertised one route. Two session dumps is the
// sum; one is the router.
//
// Latent rather than live -- route_evpn carries one collector in the lab
// archive today -- and that is exactly why it needs a fixture to hold it.
func TestEvpnChurnSeriesReportsOneCollectorsViewNotTheSumOfBoth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnChurnFixture(t, ctx, ch)

	shared := evpnBucketAt(121, 0)

	// The premise, asserted rather than assumed: two rows in that minute,
	// one per collector. Without both copies present the assertion below
	// would pass on a panel that never changed.
	obs, _ := evpnRawBucket(t, ctx, ch, shared, "1 MINUTE")
	if obs != 2 {
		t.Fatalf("the shared-collector bucket holds %d observations, want 2 "+
			"-- one per collector, of the same advertisement of %s",
			obs, evpnKeyCollectorMAC)
	}

	got, present := evpnSeries(t, ctx, ch, ".*", ".*", "1 MINUTE")[shared.Unix()]
	if !present {
		t.Fatalf("the panel reports no bucket at %s at all", shared)
	}
	if got.sessionDump != 1 || got.readvertise != 0 || got.withdraw != 0 {
		t.Errorf("bucket %s reports %d re-advertisements / %d withdrawals / "+
			"%d session dumps, want 0/0/1 -- %s was advertised ONCE and both "+
			"collectors wrote it down. 2 dumps is the sum of two vantage "+
			"points on one router, which makes the fabric look twice as "+
			"busy for being watched twice",
			shared, got.readvertise, got.withdraw, got.sessionDump,
			evpnKeyCollectorMAC)
	}
}

// TestFleetHealthTurnoverReportsOneCollectorsViewNotTheSumOfBoth holds
// the per-collector session count on the DASHBOARD side: summed across
// two collectors, one router's turnover counted once per observer.
//
// "Lost view vs peer down" is sessionCountsSQL's twin in substance -- the
// same uniqExact(session_id) beside the same three countIf(kind = ...) over
// peer_events, grouped by router with no collector anywhere. Confirmed on a
// lab archive: router 172.22.0.8 reported 7 sessions where dev-c1 saw 5 and
// dev-c2 saw 2.
//
// It has to move WITH the Go answer rather than after it. The deciding
// argument for this whole rule was agreement: leaving the panel
// summing while SessionCounts best-vantages is the asn-view defect rebuilt,
// where one question answers differently depending on whether an operator
// reads the screen or the API.
//
// bmp_sessions, peer_ups and peer_downs are the ROUTER's: RouterSessionCount
// settles that Sessions answers "how often the router's transport actually
// reset", and a Down is the router telling BMP a BGP session ended with a
// reason code.
//
// view_lost is NOT, and is summed. See TestFleetHealthTurnoverKeepsEvery
// CollectorsLostView below, and sessionCountsSQL for the argument.
func TestFleetHealthTurnoverReportsOneCollectorsViewNotTheSumOfBoth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	got, ok := sessionTurnover(t, ctx, ch)[stDualSysName]
	if !ok {
		t.Fatalf("the panel has no row for %s at all", stDualSysName)
	}
	if got.sessions != 2 || got.ups != 3 || got.downs != 1 {
		t.Errorf("got %d sessions / %d up / %d down, want 2/3/1 -- c1's "+
			"whole view, it being the collector that heard the most FROM "+
			"THE ROUTER (four statements to %s's one). 3/4/1 is both "+
			"collectors summed, which reports how many collectors watch "+
			"this router as how often it reconnected",
			got.sessions, got.ups, got.downs, stDualCollectorB)
	}
}

// TestFleetHealthTurnoverKeepsEveryCollectorsLostView is the half that must
// NOT take the treatment its neighbors take, and the panel-side twin of
// TestSessionCountsKeepsEveryCollectorsLostView.
//
// view_lost is the COLLECTOR saying it stopped being able to see the peer,
// with down_reason 0 because the router said nothing. Best-vantage there is
// signal-suppressing and structurally so: a collector that has gone blind
// records fewer router statements by definition, so it always loses the
// choice, and the column that exists to surface its blindness would be read
// off the collector that stayed healthy. Both of this router's lost views
// belong to the collector that loses.
func TestFleetHealthTurnoverKeepsEveryCollectorsLostView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	got, ok := sessionTurnover(t, ctx, ch)[stDualSysName]
	if !ok {
		t.Fatalf("the panel has no row for %s at all", stDualSysName)
	}
	if got.viewLosts != 4 {
		t.Errorf("got %d view_lost, want 4 -- every one of them %s's, the "+
			"collector that lost its view four times and therefore heard the "+
			"LEAST from this router. 0 means view_lost was read off the "+
			"winning collector's row, which hides collector blindness on "+
			"the panel whose whole title is about reporting it",
			got.viewLosts, stDualCollectorB)
	}
}

// TestFleetHealthTurnoverResolvesATieToOneCollectorsWholeRow guards the
// second half of the (router_events, collector_id) ordering tuple, and is
// the panel-side twin of
// TestSessionCountsResolvesATieToOneCollectorsWholeRow.
//
// Every column is an argMax over the SAME expression, which is what makes
// the answer ONE collector's row rather than a per-column maximum. That
// only holds while the expression is a total order: with a bare argMax on
// router_events, two collectors that heard equally many statements tie, and
// ClickHouse may resolve each column's tie independently -- so a row could
// carry one collector's bmp_sessions beside the other's peer_ups and be a
// vantage point that never existed.
func TestFleetHealthTurnoverResolvesATieToOneCollectorsWholeRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	// sessionTurnoverROWS, not the map: the map is keyed by router name, so
	// two rows for one router silently overwrite each other and the most
	// likely way to lose the tie-break -- a winner comparison that matches
	// BOTH collectors' rows -- is invisible through it. That is not
	// hypothetical; it is the mutation this test failed to kill until the
	// assertion moved off the map.
	var mine []stTurnoverRow
	for _, r := range sessionTurnoverRows(t, ctx, ch) {
		if r.router == stTieSysName {
			mine = append(mine, r)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("the panel renders %d rows for %s, want exactly 1: %+v. "+
			"One router is one row however many collectors watch it -- two "+
			"means the winner comparison matched both collectors, which is "+
			"what the collector_id half of the ordering tuple prevents",
			len(mine), stTieSysName, mine)
	}
	got := mine[0]
	type triple struct{ sessions, ups, downs uint64 }
	first := triple{2, 3, 1}  // c1's whole view
	second := triple{1, 4, 0} // c2's whole view
	have := triple{got.sessions, got.ups, got.downs}
	if have != first && have != second {
		t.Fatalf("got %d sessions / %d up / %d down, which is NEITHER "+
			"collector's view (%+v or %+v). A row belonging to no vantage "+
			"point is what the collector_id tie-break exists to prevent: "+
			"both collectors heard four router statements, so a bare argMax "+
			"on the count alone lets every column resolve its tie "+
			"independently", got.sessions, got.ups, got.downs, first, second)
	}
	if have != second {
		t.Errorf("got %+v, want %+v -- %s's view. Both collectors heard four "+
			"router statements, so the tuple's second element decides, and "+
			"%s sorts after c1", have, second, stDualCollectorB,
			stDualCollectorB)
	}
}

// TestFleetPeerEventsSeriesAgreesWithItsOwnSiblingTable pins the
// consistency required on one dashboard.
//
// "Peer up/down over time" and "Lost view vs peer down" plot and tabulate
// THE SAME COLUMNS off the same table. When the table moved to the best
// single vantage point and the chart kept summing, fleet-health began
// answering one question two ways on one screen: the table says this router
// came up 3 times, the chart's bars for it add to 4. That is the asn-view
// defect rebuilt inside a single dashboard, and it is worse here than
// between a panel and an API because both numbers are visible at once.
//
// So the chart takes the same treatment: up, down and unspecified come from
// the collector that heard the most FROM THE ROUTER, chosen once over the
// window and per router, and view_lost is summed because it is the
// collector's own statement. See sessionCountsSQL for the full argument and
// route-churn's series for why the choice is made per window rather than
// per bucket.
func TestFleetPeerEventsSeriesAgreesWithItsOwnSiblingTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertSessionTurnoverFixture(t, ctx, ch)

	// ROWS, not the name-keyed map: stRouterTwinA and stRouterTwinB are two
	// routers sharing one sysName, so the map collapses them and a total
	// summed through it is short by a whole router. Found by this test
	// failing against a chart that was already correct.
	var wantUp, wantDown, wantViewLost uint64
	for _, r := range sessionTurnoverRows(t, ctx, ch) {
		wantUp += r.ups
		wantDown += r.downs
		wantViewLost += r.viewLosts
	}

	var up, down, viewLost uint64
	for _, b := range peerEventSeries(t, ctx, ch) {
		up += b.up
		down += b.down
		viewLost += b.viewLost
	}

	if up != wantUp || down != wantDown || viewLost != wantViewLost {
		t.Errorf("the chart totals %d up / %d down / %d view_lost, the table "+
			"beside it totals %d / %d / %d. Both read peer_events over the "+
			"same routers and name the same three columns, so an operator "+
			"reading one against the other must not see two different "+
			"numbers for one question",
			up, down, viewLost, wantUp, wantDown, wantViewLost)
	}
}

// TestRibBrowserDoesNotCallALiveRouteWithdrawnOnALaggingCollector pins
// the dangerous half of the cross-collector clock defect.
//
// "Routes" groups by (family, rd, prefix, router_ip) with NO collector and
// resolves state, label, next hop and origin with argMax(..., ts_collector).
// ts_collector is each collector's own wall clock, so on a dual-homed router
// every one of those four columns is decided by whichever collector's clock
// ran ahead -- not by which collector actually saw the route best.
//
// The fixture makes that concrete in the direction that hurts: the collector
// holding two advertisements is the one that saw the most, and the collector
// holding a single WITHDRAWAL stamped a half-hour later is the one that saw
// the least. An operator reads a route the network is really carrying as
// gone. This is the looking-glass failure arriving by a different route,
// and the looking glass was already fixed for this class of defect by
// scoping session resolution to one collector.
//
// Latent on the lab archive TODAY -- 0 of 14 shared groups disagree about
// state -- and live the moment they do. route_vpn already carries two
// collectors there (178 rows to 14), so the arming condition is met and only
// the disagreement is missing.
func TestRibBrowserDoesNotCallALiveRouteWithdrawnOnALaggingCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	got, ok := ribRoutes(t, ctx, ch, ".*", ".*", ".*")[rbRDa+"|"+rbPfxDualState+"|dash-rib3"]
	if !ok {
		t.Fatalf("the table has no row for %s at all", rbPfxDualState)
	}
	if got.state != "advertised" {
		t.Errorf("state = %q, want \"advertised\" -- the collector that saw "+
			"this route twice says it is up, and the collector that saw it "+
			"once says it was withdrawn a half-hour later. Reporting "+
			"\"withdrawn\" hands the answer to whichever wall clock ran "+
			"ahead and tells an operator a live route is gone", got.state)
	}
}

// TestRibBrowserResolvesEveryColumnFromOneCollector pins the quieter half
// of the same defect, the one a state assertion cannot reach.
//
// Four columns -- label, next_hop, origin_asn and state -- are four separate
// argMax calls. Even once they are scoped to one collector, they have to be
// scoped to THE SAME one, or a row can carry one collector's label beside
// another's next hop and describe a vantage point that never existed.
//
// rbPfxDualLabel is the case: both collectors call it advertised and they
// disagree about the label and the next hop, so the state column cannot fail
// here and these two can.
func TestRibBrowserResolvesEveryColumnFromOneCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	got, ok := ribRoutes(t, ctx, ch, ".*", ".*", ".*")[rbRDa+"|"+rbPfxDualLabel+"|dash-rib3"]
	if !ok {
		t.Fatalf("the table has no row for %s at all", rbPfxDualLabel)
	}
	wantLabel := strconv.Itoa(rbLabelDualChosen)
	if got.label != wantLabel || got.nextHop != rbNextHopDualChosen {
		t.Errorf("label %q / next hop %q, want %q / %q -- both from the "+
			"collector that saw this route twice. %q / %q is the lagging "+
			"collector's single later observation, picked because its wall "+
			"clock ran ahead; one of each is worse still, a row describing "+
			"a vantage point that never existed",
			got.label, got.nextHop, wantLabel, rbNextHopDualChosen,
			strconv.Itoa(rbLabelDualLagging), rbNextHopDualLagging)
	}
}

// TestRibBrowserCountsOneCollectorsSessions pins the third affected
// column, and the only one of the three that is WRONG ON THE LAB ARCHIVE
// RIGHT NOW rather than latently.
//
// session_id is minted per collector, so uniqExact(session_id) across
// collectors counts how many collectors watch a router as how many times its
// transport reset -- the same defect the peer-status pie has, in a
// different table. Measured on
// the lab archive on 2026-09-20: 28 sessions across the 14 route groups both
// collectors hold, where the best single vantage point saw 14.
func TestRibBrowserCountsOneCollectorsSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	routes := ribRoutes(t, ctx, ch, ".*", ".*", ".*")
	for _, prefix := range []string{rbPfxDualState, rbPfxDualLabel} {
		got, ok := routes[rbRDa+"|"+prefix+"|dash-rib3"]
		if !ok {
			t.Fatalf("the table has no row for %s at all", prefix)
		}
		if got.sessions != 1 {
			t.Errorf("%s: sessions = %d, want 1 -- this router held ONE BMP "+
				"session and two collectors each minted their own id for "+
				"their own view of it. 2 reports how many collectors watch "+
				"this router as how often it reconnected", prefix, got.sessions)
		}
	}
}

// TestRibBrowserRendersOneRowPerRouteWhenCollectorsTie guards the second
// element of the (observations, collector_id) winner comparison.
//
// The comparison keeps the row whose (observations, collector_id) equals the
// group's maximum. Where two collectors saw a route equally often, a
// comparison on the count ALONE is true of both of them, and the table
// renders one route twice -- two rows differing only in a label an operator
// has no way to choose between.
//
// It asserts on ROWS rather than through ribRoutes' map, which is keyed by
// (rd, prefix, router) and would let the duplicate silently overwrite
// itself. That is not hypothetical caution: the same map shape hid exactly
// this mutation on fleet-health's turnover table earlier the same day.
func TestRibBrowserRendersOneRowPerRouteWhenCollectorsTie(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	raw := scopeRibBrowserToFixtureRouters(t, panelSQL(t, "l3vpn-rib-browser", "Routes", "A"))
	sql := substituteGrafana(raw, map[string]string{"rd": ".*", "family": ".*", "rib": ".*"})
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("rib routes: %v", err)
	}
	defer rows.Close()
	var mine []rbRouteRow
	for rows.Next() {
		var r rbRouteRow
		if err := rows.Scan(&r.family, &r.rd, &r.prefix, &r.label, &r.nextHop,
			&r.originASN, &r.state, &r.router, &r.sessions, &r.lastSeen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if r.prefix == rbPfxDualTie {
			mine = append(mine, r)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(mine) != 1 {
		t.Fatalf("the table renders %d rows for %s, want exactly 1: %+v. "+
			"Both collectors saw this route twice, so a winner comparison "+
			"on the observation count alone is true of both and the route "+
			"appears twice", len(mine), rbPfxDualTie, mine)
	}
	if want := strconv.Itoa(rbLabelDualLagging); mine[0].label != want {
		t.Errorf("label = %q, want %q -- the collectors tie on observations, "+
			"so the tuple's second element decides and %q sorts after \"c1\"",
			mine[0].label, want, rbDualCollectorB)
	}
}

// TestRibBrowserLastSeenBelongsToTheChosenCollector settles a column the
// winner comparison could plausibly have been given either way.
//
// last_seen reads as "how fresh is what this row says". Taking
// max(ts_collector) across ALL collectors makes the row internally
// inconsistent the moment they disagree: rbPfxDualState would render
// "advertised ... one minute ago" when the collector that says advertised
// last saw it a half-hour back and the one-minute-old observation is the
// OTHER collector's withdrawal. An operator would read a stale claim as
// fresh evidence, which is a worse error than a stale timestamp.
//
// It is deliberately NOT the reasoning view_lost takes on the
// collection-health panels. That column measures collector blindness, which
// has no meaning inside a single vantage point and must be summed across
// the population. last_seen is a property of the observation this row is
// reporting, and the chosen view has its own answer for it.
func TestRibBrowserLastSeenBelongsToTheChosenCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertRibBrowserFixture(t, ctx, ch)

	got, ok := ribRoutes(t, ctx, ch, ".*", ".*", ".*")[rbRDa+"|"+rbPfxDualState+"|dash-rib3"]
	if !ok {
		t.Fatalf("the table has no row for %s at all", rbPfxDualState)
	}
	// The chosen collector's newest observation is 29 minutes old; the
	// lagging collector's withdrawal is 1 minute old. Anything newer than
	// ~15 minutes means last_seen crossed collectors.
	age := corpusCollectorClock.Sub(got.lastSeen)
	if age < 15*time.Minute {
		t.Errorf("last_seen is %s old on a row that says %q, but the "+
			"collector this row came from last saw the route 29 minutes "+
			"ago -- the 1-minute-old observation is the OTHER collector's "+
			"withdrawal. A row cannot carry one collector's verdict beside "+
			"another's timestamp and still mean anything", age, got.state)
	}
}

// TestTopL3VPNCountsOneCollectorsSessionsAndWithdrawals pins the same
// defect on the "Top prefixes" panel's session and withdrawal counts.
//
// "Top prefixes" groups by (family, rd, prefix) with no collector, so
// `sessions` counts per-collector session ids and `withdrawals` counts one
// router's withdrawal once per collector that heard it. Neither is a
// collection quantity: this dashboard's own description says to read
// `sessions` as "survived this many re-dumps", which is a fact about the
// router's transport, and a withdrawal is something the router did on the
// wire.
//
// `sessions` is also the SECOND RANKING KEY, so the inflation does not just
// misreport a number -- it reorders the table, putting whichever prefix has
// the most observers above whichever is genuinely most persistent. That is
// the asn-view defect's shape.
//
// This is live-wrong on the lab archive rather than latent: route_vpn
// carries two collectors there, and the shared route groups report 28
// sessions where the best single vantage point saw 14.
//
// `observations` comes from the chosen collector too, for the reason
// ChurnByPrefix gives about its own: it is honestly a collection count, but
// reporting it from a DIFFERENT collector than the rest of the row would
// make the row a mix of vantage points rather than one view.
func TestTopL3VPNCountsOneCollectorsSessionsAndWithdrawals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	got, ok := topPrefixes(t, ctx, ch, ".*", ".*")[topRDa+"|"+topPfxDual]
	if !ok {
		t.Fatalf("the table has no row for %s at all", topPfxDual)
	}
	if got.sessions != 2 {
		t.Errorf("sessions = %d, want 2 -- the chosen collector saw this "+
			"route across two BMP sessions and the lagging one minted its "+
			"own id for its own view of one of them. 3 counts observers as "+
			"re-dumps, and `sessions` is this table's second ranking key",
			got.sessions)
	}
	if got.withdrawals != 1 {
		t.Errorf("withdrawals = %d, want 1 -- this router withdrew this "+
			"route ONCE and both collectors wrote it down. 2 makes a route "+
			"look twice as unstable for being watched twice, on the column "+
			"a tops view is read for", got.withdrawals)
	}
	if got.observations != 4 {
		t.Errorf("observations = %d, want 4 -- the chosen collector's own "+
			"count. 6 is both collectors summed, which would leave this row "+
			"describing two vantage points at once", got.observations)
	}
	// The control: routers was ALWAYS honest here, being a uniqExact over
	// router_ip with no collector in it, and must not move.
	if got.routers != 1 {
		t.Errorf("routers = %d, want 1 -- one router, however many "+
			"collectors watch it", got.routers)
	}
}

// TestTopL3VPNRDSessionsCountOneCollectorsView pins the same
// defect on the sibling table, where `sessions` is the THIRD ranking key.
//
// "Top RDs" sums its prefixes' session counts, so a VRF on a dual-homed
// router inherits the inflation once per prefix rather than once.
func TestTopL3VPNRDSessionsCountOneCollectorsView(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	byRD := topRDs(t, ctx, ch, ".*", ".*")
	got, ok := byRD[topRDa]
	if !ok {
		t.Fatalf("the table has no row for RD %s at all", topRDa)
	}
	// DISTINCT session ids under this RD, not a sum across its prefixes --
	// the panel's own description says "summed", and it is wrong: the
	// statement is uniqExact(session_id) over the whole RD group, so a
	// session carrying three of this RD's prefixes counts once. Derived
	// that way after the first derivation, made from the description,
	// disagreed with the shipped panel by three.
	//
	// The five that survive the collector choice: topSessionA1, topSessionA2
	// and topSessionB from the single-collector routers, and topSessionDual1
	// and 9806 from the dual-homed router's chosen collector. Six is
	// topSessionDualB, the lagging collector's own id for a session the
	// router had once.
	if got.sessions != 5 {
		t.Errorf("RD %s reports %d distinct sessions, want 5. 6 counts %s's "+
			"lagging collector's session id as a re-dump the router never "+
			"performed", topRDa, got.sessions, topPfxDual)
	}
}

// TestTopL3VPNCountsEachRouterEvenWhenOneSawLess pins that the collector
// choice is made PER ROUTER, not per prefix.
//
// Summing across routers is addition -- two routers carrying one prefix are
// two real advertisements of it -- while summing across collectors is
// double-counting. A winner comparison partitioned by prefix alone conflates
// the two: it picks the busiest (router, collector) pair and discards every
// other ROUTER's rows, so a prefix carried by a quiet router and a chatty
// one reports one router.
//
// topPfxTwoRouters is deliberately asymmetric -- router B holds two
// observations to router A's one -- because with one each the comparison
// keeps both by accident and the mutation survives.
func TestTopL3VPNCountsEachRouterEvenWhenOneSawLess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	got, ok := topPrefixes(t, ctx, ch, ".*", ".*")[topRDa+"|"+topPfxTwoRouters]
	if !ok {
		t.Fatalf("the table has no row for %s at all", topPfxTwoRouters)
	}
	if got.routers != 2 || got.observations != 3 {
		t.Errorf("%s: %d routers / %d observations, want 2/3 -- router A "+
			"advertised it once and router B twice. 1 router means the "+
			"collector choice was made per prefix and threw away the "+
			"quieter ROUTER, which is addition being mistaken for "+
			"double-counting", topPfxTwoRouters, got.routers, got.observations)
	}
}

// TestTopL3VPNResolvesATieToOneCollector guards the second element of the
// (observations, collector_id) winner comparison.
//
// Where two collectors saw a route equally often, a comparison on the count
// alone is true of BOTH of them, so both collectors' rows survive the filter
// and every count on the route doubles -- the exact defect the filter exists
// to remove, reappearing only on ties.
func TestTopL3VPNResolvesATieToOneCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertTopL3VPNFixture(t, ctx, ch)

	got, ok := topPrefixes(t, ctx, ch, ".*", ".*")[topRDb+"|"+topPfxDualTie]
	if !ok {
		t.Fatalf("the table has no row for %s at all", topPfxDualTie)
	}
	if got.sessions != 1 || got.observations != 2 {
		t.Errorf("%s: %d sessions / %d observations, want 1/2 -- both "+
			"collectors saw this route twice in one session each, so a "+
			"comparison on the observation count alone is true of both and "+
			"keeps every row. 2/4 is that failure",
			topPfxDualTie, got.sessions, got.observations)
	}
}

// The nameless-router fixture: one router that sends no sysName at all.
//
// It is its own fixture and its own router rather than a row added to
// insertEvpnChurnFixture, because that fixture's totals are pinned to the
// row and that a fifth router would renumber them for a reason unrelated to
// what they guard.
//
// 10.0.199.21 and 10.202.13.x are claimed by nothing else in this package.
const (
	evpnNamelessRouter = "10.0.199.21"
	evpnNamelessPeer   = "10.255.199.21"
	evpnNamelessRD     = "65000:77"
	evpnNamelessMAC    = "52:54:00:ff:07:01"
	evpnNamelessSess   = 9931
)

// insertEvpnNamelessRouterFixture writes one EVPN route under a router whose
// sysName is the empty string.
//
// That is not a contrived shape: NONE of the corpus's three EVPN captures
// carries an Initiation message, so the replayed router sends no sysName -- and
// RouterSessionCount's own doc records two routers in the lab archive
// already reporting an empty one. The panels render a blank cell for them
// today.
func insertEvpnNamelessRouterFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	nlri := func() *vantagev1.EvpnRoute {
		return &vantagev1.EvpnRoute{
			RouteType: 2, Rd: evpnNamelessRD, Mac: evpnNamelessMAC,
			Labels: []uint32{25901},
		}
	}
	env := func(at time.Time, withdraw bool) *vantagev1.Envelope {
		r := &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 25, Safi: 70},
			Attrs: &vantagev1.PathAttributes{
				Origin: 0, NextHop: "10.202.13.1",
				AsPath: []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65000}}},
			},
		}
		if withdraw {
			r.EvpnWithdrawn = []*vantagev1.EvpnRoute{nlri()}
		} else {
			r.EvpnAnnounced = []*vantagev1.EvpnRoute{nlri()}
		}
		return &vantagev1.Envelope{
			CollectorId: "c1",
			// SysName deliberately absent -- this is the whole fixture.
			Router:      &vantagev1.RouterId{Ip: evpnNamelessRouter},
			Peer:        &vantagev1.PeerId{Ip: evpnNamelessPeer, Asn: 65000},
			SessionId:   evpnNamelessSess,
			TsRouter:    timestamppb.New(at),
			TsCollector: timestamppb.New(at),
			Payload:     &vantagev1.Envelope_Route{Route: r},
		}
	}
	// Advertise, withdraw, return: ONE flap cycle, which is what makes the
	// "Flap cycles" panel render a row at all -- it lists is_flap = 1 rows
	// and nothing else. It is also the shape the real capture carries seven
	// of, so the fixture matches what the replay will produce.
	base := corpusCollectorClock.Add(-90 * time.Second)
	envs := []*vantagev1.Envelope{
		env(base, false),
		env(base.Add(10*time.Second), true),
		env(base.Add(20*time.Second), false),
	}
	for i, e := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, e, uint64(i+1))); err != nil {
			t.Fatalf("insert nameless-router evpn fixture envelope %d: %v", i, err)
		}
	}
}

// TestEvpnChurnNamesARouterThatSendsNoSysName pins the gap in evpn-churn.
//
// `fleet-health` and `parse-anomalies` both render
// if(router_sysname = ”, '(no sysName TLV)', router_sysname); evpn-churn's
// three panels render router_sysname raw, so a router that sends no
// Initiation appears as an empty cell in a column an operator reads to tell
// one device from another. Two routers in the lab archive already report an
// empty sysName, so this is live rather than hypothetical.
//
// A blank cell is worse than it looks on THIS dashboard in particular: the
// "Flap cycles" table lists one row per flap event, so an unnamed router's
// flaps sit in a list beside named routers' with nothing saying whose they
// are.
func TestEvpnChurnNamesARouterThatSendsNoSysName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireClickHouse(t, ctx)
	defer ch.Close()
	insertEvpnNamelessRouterFixture(t, ctx, ch)

	for _, panel := range []string{"Most unstable routes", "Flap cycles"} {
		raw := panelSQL(t, "evpn-churn", panel, "A")
		const anchor = "$__timeFilter(ts_collector)"
		if n := strings.Count(raw, anchor); n != 1 {
			t.Fatalf("%s carries %d time filters, want 1", panel, n)
		}
		scoped := strings.Replace(raw, anchor,
			anchor+" AND router_ip = toIPv6('"+evpnNamelessRouter+"')", 1)
		sql := substituteGrafana(scoped, map[string]string{"rib": ".*", "route_type": ".*"})
		rows, err := ch.conn.Query(ctx, qualify(ch, sql))
		if err != nil {
			t.Fatalf("%s: %v\nSQL:\n%s", panel, err, sql)
		}
		cols := rows.Columns()
		idx := -1
		for i, c := range cols {
			if c == "router" {
				idx = i
			}
		}
		if idx < 0 {
			rows.Close()
			t.Fatalf("%s renders no `router` column: %v", panel, cols)
		}
		got := dashRowsAsStrings(t, rows)
		rows.Close()
		if len(got) == 0 {
			t.Fatalf("%s renders no rows for the nameless router", panel)
		}
		for _, r := range got {
			if r[idx] != "(no sysName TLV)" {
				t.Errorf("%s: router = %q, want %q. This router sends no "+
					"Initiation, so router_sysname is the empty string, and "+
					"fleet-health and parse-anomalies both render the "+
					"fallback where this panel renders a blank cell",
					panel, r[idx], "(no sysName TLV)")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Current state past retention.
//
// Every history table carries a TTL (90 days by default), and BMP sends only
// changes, so a session that stays up and an object that stays unchanged
// longer than that lose their rows while they are still live. The panels
// below answer "what is live now", so they read the current-state tables
// (peer_current, ls_*_current), which have no TTL. Panels bounded by
// $__timeFilter answer "what did we see in this range" and stay on history.
//
// The fixture is one router whose BMP session came up at retExpiredAt and
// has been quiet since: its Peer Up and its link-state are past retention,
// and OPTIMIZE ... FINAL has applied the TTL, so history holds none of them.
// Its routes were heard recently, so the looking glasses and asn-view (which
// read routes in the window) still see them, and must still call them
// current: the session they rest on is the current one.
//
// It lives in a database of its own. OPTIMIZE ... FINAL also collapses a
// ReplacingMergeTree's duplicates, and several fixtures in testDB carry an
// unmerged duplicate on purpose; and the fleet-wide panels here (Peer status,
// View completeness) are asserted unscoped, which only means something when
// this fixture is the whole database.
// ---------------------------------------------------------------------------

const (
	retentionDashTestDB = "vantage_sink_dashboard_retention_test"

	retRouterIP   = "10.0.213.1"
	retRouterIPv6 = "::ffff:10.0.213.1"
	retPeerIP     = "10.255.213.1"
	retSysname    = "ret-r1"
	retSession    = 7001

	// retReclaimedRouterIP is insertRetentionDashReclaimedRouter's router.
	retReclaimedRouterIP   = "10.0.213.2"
	retReclaimedRouterIPv6 = "::ffff:10.0.213.2"
	retOldSysname          = "ret-r2-old"
	retNewSysname          = "ret-r2"
	retReclaimedOld        = 7101
	retReclaimedNew        = 7102

	// retInvertedRouterIP is insertRetentionDashInvertedNode's router.
	retInvertedRouterIP = "10.0.213.3"
	retInvertedSession  = 7201

	// retExpiredPartition is retExpiredAt's toYYYYMM partition, the only one
	// the fixture's OPTIMIZE touches.
	retExpiredPartition = "202103"
)

// retExpiredAt is when the fixture's BMP session came up: a fixed instant
// far past any retention setting, in a month no other sink fixture writes.
var retExpiredAt = time.Date(2021, 3, 15, 12, 0, 0, 0, time.UTC)

// requireRetentionDashFixture returns a client on retentionDashTestDB with
// the fixture written and its history expired. The database is recreated
// once per test binary, so the fixture is written once and later callers
// find it already there.
func requireRetentionDashFixture(t *testing.T, ctx context.Context) *ClickHouse {
	t.Helper()
	ch := requireClickHouseDB(t, ctx, retentionDashTestDB)
	t.Cleanup(func() { ch.Close() })

	var have uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch,
		"SELECT count() FROM vantage.peer_current WHERE router_ip = toIPv6('"+retRouterIP+"')")).
		Scan(&have); err != nil {
		t.Fatalf("probe the retention fixture: %v", err)
	}
	if have == 0 {
		insertRetentionDashFixture(t, ctx, ch)
	}
	requireRetentionDashFixtureExpired(t, ctx, ch)
	return ch
}

func insertRetentionDashFixture(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	var (
		a = []byte{10, 255, 213, 1}
		b = []byte{10, 255, 213, 2}
	)
	base := func(seq uint64, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: retRouterIP, SysName: retSysname},
			Peer:        &vantagev1.PeerId{Ip: retPeerIP, Asn: 65213},
			SessionId:   retSession,
			Seq:         seq,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
		}
	}
	peer := func(seq uint64, ts time.Time, kind vantagev1.PeerEvent_Kind) *vantagev1.Envelope {
		e := base(seq, ts)
		e.Payload = &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{Kind: kind}}
		return e
	}
	ls := func(seq uint64, ts time.Time, ev *vantagev1.LsEvent) *vantagev1.Envelope {
		e := base(seq, ts)
		e.Payload = &vantagev1.Envelope_Ls{Ls: ev}
		return e
	}
	route := func(seq uint64, ev *vantagev1.RouteEvent) *vantagev1.Envelope {
		e := base(seq, corpusCollectorClock)
		e.Payload = &vantagev1.Envelope_Route{Route: ev}
		return e
	}
	attrs := &vantagev1.PathAttributes{
		NextHop: "10.213.0.1",
		AsPath:  []*vantagev1.AsPathSegment{{Type: 2, Asns: []uint32{65213, 65214}}},
	}
	envs := []*vantagev1.Envelope{
		// The session's first event, then a BGP flap inside it: "session
		// age" is measured from the first, and "Peer status" reads the last.
		peer(1, retExpiredAt, vantagev1.PeerEvent_KIND_UP),
		peer(2, retExpiredAt.Add(30*time.Minute), vantagev1.PeerEvent_KIND_DOWN),
		peer(3, retExpiredAt.Add(time.Hour), vantagev1.PeerEvent_KIND_UP),
		ls(4, retExpiredAt.Add(time.Minute), &vantagev1.LsEvent{
			Nodes: []*vantagev1.LsNode{
				{Protocol: 3, Identifier: 100, Local: lsDesc(a), Name: "ret-a"},
				{Protocol: 3, Identifier: 100, Local: lsDesc(b), Name: "ret-b"},
			},
			Links: []*vantagev1.LsLink{{
				Protocol: 3, Identifier: 100, Local: lsDesc(a), Remote: lsDesc(b),
				LocalIfaddr: a, RemoteIfaddr: b, IgpMetric: 10, TeMetric: 20,
			}},
			Prefixes: []*vantagev1.LsPrefix{{
				Protocol: 3, Identifier: 100, Local: lsDesc(a),
				Prefix: []byte{10, 213, 1, 0}, PrefixLen: 24, PrefixMetric: 10,
			}},
		}),
		route(5, &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 1}, Attrs: attrs,
			Announced: []*vantagev1.Prefix{{Prefix: "10.213.0.0/16"}},
		}),
		route(6, &vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 1, Safi: 128}, Attrs: attrs,
			VpnAnnounced: []*vantagev1.VpnPrefix{
				{Prefix: "10.213.2.0/24", Rd: "65213:1", Labels: []uint32{21301}},
			},
		}),
	}
	for i, ev := range envs {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(i+1))); err != nil {
			t.Fatalf("insert retention fixture envelope %d: %v", i, err)
		}
	}
	insertRetentionDashReclaimedRouter(t, ctx, ch)
	// Retention, the way it runs: a merge that applies the TTL.
	for _, tbl := range []string{"peer_events", "ls_events", "ls_nodes", "ls_links", "ls_prefixes"} {
		if err := ch.conn.Exec(ctx, qualify(ch, "OPTIMIZE TABLE vantage."+tbl+
			" PARTITION ID '"+retExpiredPartition+"' FINAL")); err != nil {
			t.Fatalf("optimize the expired %s partition: %v", tbl, err)
		}
	}
	insertRetentionDashInvertedNode(t, ctx, ch)
}

// insertRetentionDashInvertedNode writes one link-state node twice in one
// session with its receive order inverted against the sender's: the version
// the router numbered LATER (seq 21, "ret-c-new") arrived FIRST, with the
// earlier ts_collector, and the version it numbered earlier (seq 20,
// "ret-c-old") arrived second. That is a message reordered in flight, the
// shape query's insertLSNodeFixture gives its "gone" pair.
//
// ls_nodes_current is ReplacingMergeTree(seq), so its merge keeps seq 21. A
// panel ordering argMax by (ts_collector, stream_seq) reads "ret-c-old"
// until that merge runs and "ret-c-new" after it: its answer flips on a
// background merge. Ordering by (seq, stream_seq), as query/linkstate.go
// does on the same table, agrees with the merge before and after.
// TestLinkStateCurrentReadsAgreeWithTheMerge stops merges on
// ls_nodes_current for its own duration and restores the unmerged pair if a
// merge got there first; see requireInvertedNodeUnmerged.
//
// It has a router of its own, so no other assertion over the fixture's
// routers moves.
func insertRetentionDashInvertedNode(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	env := func(seq uint64, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: retInvertedRouterIP, SysName: "ret-r3"},
			Peer:        &vantagev1.PeerId{Ip: retPeerIP, Asn: 65213},
			SessionId:   retInvertedSession,
			Seq:         seq,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
		}
	}
	up := env(1, corpusCollectorClock.Add(-time.Hour))
	up.Payload = &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP}}
	node := func(seq uint64, ts time.Time, name string) *vantagev1.Envelope {
		e := env(seq, ts)
		e.Payload = &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{Nodes: []*vantagev1.LsNode{
			{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 255, 213, 3}), Name: name},
		}}}
		return e
	}
	base := corpusCollectorClock.Add(-30 * time.Minute)
	for i, ev := range []*vantagev1.Envelope{
		up,
		node(21, base, "ret-c-new"),
		node(20, base.Add(time.Second), "ret-c-old"),
	} {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(200+i))); err != nil {
			t.Fatalf("insert inverted-node envelope %d: %v", i, err)
		}
	}
}

// insertRetentionDashReclaimedRouter writes the other side of the pickers'
// and the completeness table's history halves: a router whose link-state
// history is still inside retention but whose current-table rows cleanup has
// reclaimed. Its first session (sysname retOldSysname) carried link-state;
// the router then reconnected under a new sysname and has not re-dumped.
// Both sessions are recent, and the first session's peer_current and
// ls_nodes_current rows are deleted, as cleanup deletes a superseded
// session's rows.
//
// Reading the current tables alone, the $router picker would stop offering
// this router and the completeness table would lose the row that says it
// holds a node in the window and none in its current session: the exact
// diagnostic "View completeness" and "Carried by the current session" exist
// to give.
func insertRetentionDashReclaimedRouter(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	env := func(session uint64, sysname string, ts time.Time) *vantagev1.Envelope {
		return &vantagev1.Envelope{
			CollectorId: "c1",
			Router:      &vantagev1.RouterId{Ip: retReclaimedRouterIP, SysName: sysname},
			Peer:        &vantagev1.PeerId{Ip: retPeerIP, Asn: 65213},
			SessionId:   session,
			TsRouter:    timestamppb.New(ts),
			TsCollector: timestamppb.New(ts),
		}
	}
	up := func(session uint64, sysname string, ts time.Time) *vantagev1.Envelope {
		e := env(session, sysname, ts)
		e.Payload = &vantagev1.Envelope_PeerEvent{PeerEvent: &vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP}}
		return e
	}
	node := env(retReclaimedOld, retOldSysname, corpusCollectorClock.Add(-50*time.Minute))
	node.Payload = &vantagev1.Envelope_Ls{Ls: &vantagev1.LsEvent{Nodes: []*vantagev1.LsNode{
		{Protocol: 3, Identifier: 100, Local: lsDesc([]byte{10, 255, 213, 9}), Name: "ret-old-node"},
	}}}
	for i, ev := range []*vantagev1.Envelope{
		up(retReclaimedOld, retOldSysname, corpusCollectorClock.Add(-time.Hour)),
		node,
		up(retReclaimedNew, retNewSysname, corpusCollectorClock.Add(-10*time.Minute)),
	} {
		if err := ch.Insert(ctx, mustRowsFor(t, ev, uint64(100+i))); err != nil {
			t.Fatalf("insert reclaimed-router envelope %d: %v", i, err)
		}
	}
	for _, tbl := range []string{"peer_current", "ls_nodes_current"} {
		if err := ch.conn.Exec(ctx, qualify(ch, fmt.Sprintf(
			"DELETE FROM vantage.%s WHERE router_ip = toIPv6('%s') AND session_id = %d",
			tbl, retReclaimedRouterIP, retReclaimedOld))); err != nil {
			t.Fatalf("reclaim the superseded session from %s: %v", tbl, err)
		}
	}
}

// requireRetentionDashFixtureExpired checks the fixture describes what it
// claims before any test leans on it: history holds none of the session's
// peer events or link-state, the current tables hold all of them, and the
// routes are still in history (they are recent).
func requireRetentionDashFixtureExpired(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	for _, c := range []struct {
		table string
		want  uint64
	}{
		{"peer_events", 0}, {"ls_nodes", 0}, {"ls_links", 0}, {"ls_prefixes", 0},
		{"peer_current FINAL", 3}, {"ls_nodes_current FINAL", 2},
		{"ls_links_current FINAL", 1}, {"ls_prefixes_current FINAL", 1},
		{"route_unicast FINAL", 1}, {"route_vpn FINAL", 1},
	} {
		var got uint64
		if err := ch.conn.QueryRow(ctx, qualify(ch, "SELECT count() FROM vantage."+c.table+
			" WHERE router_ip = toIPv6('"+retRouterIP+"')")).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if got != c.want {
			t.Fatalf("the retention fixture has %d rows in %s, want %d: it does not "+
				"describe a live session whose history expired, so nothing built on "+
				"it can say anything about retention", got, c.table, c.want)
		}
	}
	for _, c := range []struct {
		table string
		want  uint64
	}{
		{"peer_events FINAL", 2}, {"ls_nodes FINAL", 1},
		{"peer_current FINAL", 1}, {"ls_nodes_current FINAL", 0},
	} {
		var got uint64
		if err := ch.conn.QueryRow(ctx, qualify(ch, "SELECT count() FROM vantage."+c.table+
			" WHERE router_ip = toIPv6('"+retReclaimedRouterIP+"')")).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if got != c.want {
			t.Fatalf("the reclaimed router has %d rows in %s, want %d: it does not "+
				"describe a session whose current rows cleanup reclaimed while its "+
				"history is still retained", got, c.table, c.want)
		}
	}
}

// retentionRows runs one rendered query and returns its rows keyed by column.
func retentionRows(t *testing.T, ctx context.Context, ch *ClickHouse, what, sql string) []map[string]string {
	t.Helper()
	rows, err := ch.conn.Query(ctx, qualify(ch, sql))
	if err != nil {
		t.Fatalf("%s: %v\nSQL:\n%s", what, err, sql)
	}
	defer rows.Close()
	colTypes := rows.ColumnTypes()
	var out []map[string]string
	for rows.Next() {
		vals := make([]any, len(colTypes))
		for i, ct := range colTypes {
			vals[i] = reflect.New(ct.ScanType()).Interface()
		}
		if err := rows.Scan(vals...); err != nil {
			t.Fatalf("%s: scan: %v", what, err)
		}
		m := map[string]string{}
		for i, ct := range colTypes {
			// A scalar subquery comes back Nullable, which scans into a
			// pointer; render the value it points at.
			v := reflect.ValueOf(vals[i]).Elem()
			for v.Kind() == reflect.Pointer && !v.IsNil() {
				v = v.Elem()
			}
			m[ct.Name()] = fmt.Sprint(v.Interface())
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows: %v", what, err)
	}
	return out
}

func retentionVars() map[string]string {
	return map[string]string{
		"router": retRouterIP, "peer": retPeerIP, "focus": "0", "state": "0",
		"collector": "c1", "target": "10.213.2.9", "rib": ".*", "family": ".*",
		"asn": "65214",
	}
}

func retentionPanel(t *testing.T, ctx context.Context, ch *ClickHouse, dash, title, refID string) []map[string]string {
	t.Helper()
	return retentionRows(t, ctx, ch, dash+" / "+title,
		substituteGrafana(panelSQL(t, dash, title, refID), retentionVars()))
}

func retentionVariable(t *testing.T, ctx context.Context, ch *ClickHouse, dash, name string) []map[string]string {
	t.Helper()
	return retentionRows(t, ctx, ch, dash+" $"+name,
		substituteGrafana(dashboardVariableQuery(t, dash, name), retentionVars()))
}

// wantRetentionRow fails unless rows is exactly one row whose named columns hold
// the wanted values.
func wantRetentionRow(t *testing.T, what string, rows []map[string]string, want map[string]string) {
	t.Helper()
	if len(rows) != 1 {
		t.Errorf("%s returned %d rows, want 1: %v", what, len(rows), rows)
		return
	}
	for k, v := range want {
		if got, ok := rows[0][k]; !ok || got != v {
			t.Errorf("%s: %s = %q, want %q (row %v)", what, k, got, v, rows[0])
		}
	}
}

// columnValues is one column of rows, sorted.
func columnValues(rows []map[string]string, col string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r[col])
	}
	sort.Strings(out)
	return out
}

// The link-state dashboards' pickers and current-session panels keep a live
// session's objects after their history has expired.
func TestLinkStateDashboardsKeepLiveObjectsPastRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ch := requireRetentionDashFixture(t, ctx)

	lsDashboards := []string{"link-state-nodes", "link-state-links", "link-state-prefixes", "link-state-topology"}
	for _, dash := range lsDashboards {
		t.Run(dash+"/pickers", func(t *testing.T) {
			routers := columnValues(retentionVariable(t, ctx, ch, dash, "router"), "__value")
			if !slices.Contains(routers, retRouterIPv6) {
				t.Errorf("$router offers %v, want it to include %s: its session is live "+
					"and its link-state is in the current tables", routers, retRouterIPv6)
			}
			if !slices.Contains(routers, retReclaimedRouterIPv6) {
				t.Errorf("$router offers %v, want it to include %s: its link-state is "+
					"still in history, and \"Carried by the current session\" reading 0 "+
					"against the window is what it is there to show", routers, retReclaimedRouterIPv6)
			}
			peers := retentionVariable(t, ctx, ch, dash, "peer")
			var offered []string
			for _, r := range peers {
				for _, v := range r {
					offered = append(offered, v)
				}
			}
			if !slices.Contains(offered, "::ffff:"+retPeerIP) {
				t.Errorf("$peer offers %v, want it to include %s", offered, "::ffff:"+retPeerIP)
			}
			// The reclaimed router's peer is known to history alone.
			vars := retentionVars()
			vars["router"] = retReclaimedRouterIP
			reclaimed := retentionRows(t, ctx, ch, dash+" $peer",
				substituteGrafana(dashboardVariableQuery(t, dash, "peer"), vars))
			if len(reclaimed) != 1 {
				t.Errorf("$peer on %s offers %v, want its one peer %s", retReclaimedRouterIP, reclaimed, retPeerIP)
			}
			if got := columnValues(retentionVariable(t, ctx, ch, dash, "collector"), "collector_id"); !slices.Equal(got, []string{"c1"}) {
				t.Errorf("$collector offers %v, want [c1]", got)
			}
			if got := columnValues(retentionVariable(t, ctx, ch, dash, "focus"), "__text"); !slices.Equal(got, []string{"ret-a", "ret-b"}) {
				t.Errorf("$focus offers %v, want [ret-a ret-b]", got)
			}
		})
	}

	carried := "Carried by the current session — $collector"
	t.Run("link-state-nodes/Nodes", func(t *testing.T) {
		if got := columnValues(retentionPanel(t, ctx, ch, "link-state-nodes", "Nodes", "A"), "node"); !slices.Equal(got, []string{"ret-a", "ret-b"}) {
			t.Errorf("Nodes lists %v, want [ret-a ret-b]", got)
		}
	})
	t.Run("link-state-nodes/"+carried, func(t *testing.T) {
		wantRetentionRow(t, carried, retentionPanel(t, ctx, ch, "link-state-nodes", carried, "A"),
			map[string]string{"in_current_session": "2", "in_window": "0"})
	})
	t.Run("link-state-links/Links", func(t *testing.T) {
		wantRetentionRow(t, "Links", retentionPanel(t, ctx, ch, "link-state-links", "Links", "A"),
			map[string]string{"local": "10.255.213.1", "remote": "10.255.213.2", "igp_metric": "10"})
	})
	t.Run("link-state-links/"+carried, func(t *testing.T) {
		wantRetentionRow(t, carried, retentionPanel(t, ctx, ch, "link-state-links", carried, "A"),
			map[string]string{"in_current_session": "1", "in_window": "0"})
	})
	t.Run("link-state-prefixes/Prefixes", func(t *testing.T) {
		wantRetentionRow(t, "Prefixes", retentionPanel(t, ctx, ch, "link-state-prefixes", "Prefixes", "A"),
			map[string]string{"prefix": "10.213.1.0/24", "origin": "ret-a"})
	})
	t.Run("link-state-prefixes/"+carried, func(t *testing.T) {
		wantRetentionRow(t, carried, retentionPanel(t, ctx, ch, "link-state-prefixes", carried, "A"),
			map[string]string{"in_current_session": "1", "in_window": "0"})
	})

	const topo = "link-state-topology"
	t.Run(topo+"/Topology nodes", func(t *testing.T) {
		if got := columnValues(retentionPanel(t, ctx, ch, topo, "Topology", "nodes"), "title"); !slices.Equal(got, []string{"ret-a", "ret-b"}) {
			t.Errorf("the node frame draws %v, want [ret-a ret-b]", got)
		}
	})
	t.Run(topo+"/Topology edges", func(t *testing.T) {
		wantRetentionRow(t, "edge frame", retentionPanel(t, ctx, ch, topo, "Topology", "edges"),
			map[string]string{"mainstat": "10"})
	})
	t.Run(topo+"/Current session age", func(t *testing.T) {
		rows := retentionPanel(t, ctx, ch, topo, "Current session age — $collector", "A")
		if len(rows) != 1 {
			t.Fatalf("session age returned %d rows, want 1", len(rows))
		}
		age, err := strconv.ParseInt(rows[0]["age"], 10, 64)
		if err != nil {
			t.Fatalf("age %q: %v", rows[0]["age"], err)
		}
		// Measured from the session's FIRST event, not its flap an hour later.
		want := time.Since(retExpiredAt)
		if d := time.Duration(age)*time.Second - want; d < -10*time.Minute || d > 10*time.Minute {
			t.Errorf("session age = %v, want about %v: the session came up at %s",
				time.Duration(age)*time.Second, want.Round(time.Second), retExpiredAt)
		}
	})
	t.Run(topo+"/Objects in current session", func(t *testing.T) {
		wantRetentionRow(t, "objects", retentionPanel(t, ctx, ch, topo, "Objects in current session — $collector", "A"),
			map[string]string{"nodes": "2", "edges": "1"})
	})
	t.Run(topo+"/View completeness by router", func(t *testing.T) {
		byRouter := map[string][]map[string]string{}
		for _, r := range retentionPanel(t, ctx, ch, topo, "View completeness by router", "A") {
			byRouter[r["router"]] = append(byRouter[r["router"]], r)
		}
		// The live router: everything in its current session, nothing in
		// the window, because its history has expired.
		wantRetentionRow(t, "completeness, "+retSysname, byRouter[retSysname],
			map[string]string{
				"collector": "c1", "router_ip": retRouterIPv6,
				"nodes_current_session": "2", "edges_current_session": "1",
				"nodes_in_window": "0", "edges_in_window": "0", "sessions": "0",
			})
		// The reclaimed router's old sysname: a node in the window, none in
		// the current tables.
		wantRetentionRow(t, "completeness, "+retOldSysname, byRouter[retOldSysname],
			map[string]string{
				"collector": "c1", "router_ip": retReclaimedRouterIPv6,
				"nodes_current_session": "0", "nodes_in_window": "1", "sessions": "1",
			})
		wantRetentionRow(t, "completeness, "+retNewSysname, byRouter[retNewSysname],
			map[string]string{
				"collector": "c1", "router_ip": retReclaimedRouterIPv6,
				"nodes_current_session": "0", "nodes_in_window": "0", "sessions": "1",
			})
	})
}

// "Peer status" counts a peer whose session has been quietly up for longer
// than retention as up, not as absent.
func TestFleetPeerStatusKeepsALivePeerPastRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireRetentionDashFixture(t, ctx)
	// Three routers, each with one peer that is up: the fixture's long-lived
	// session, whose peer events have all expired from history, and the
	// reclaimed and inverted-node routers, whose current sessions are recent.
	wantRetentionRow(t, "Peer status", retentionPanel(t, ctx, ch, "fleet-health", "Peer status", "A"),
		map[string]string{"up": "3", "down": "0", "view_lost": "0", "unspecified": "0"})
}

// The link-state panels that read ls_*_current resolve each object's latest
// version by (seq, stream_seq), the order the table's own merge keeps, so
// their answer does not change when a background merge runs. See
// insertRetentionDashInvertedNode.
func TestLinkStateCurrentReadsAgreeWithTheMerge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireRetentionDashFixture(t, ctx)
	requireInvertedNodeUnmerged(t, ctx, ch)

	var rows uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, "SELECT count() FROM vantage.ls_nodes_current "+
		"WHERE router_ip = toIPv6('"+retInvertedRouterIP+"')")).Scan(&rows); err != nil {
		t.Fatalf("count the inverted node's current rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("the inverted node has %d unmerged current rows, want 2: with one, "+
			"both orderings read the merged row and this test cannot tell them apart", rows)
	}

	vars := retentionVars()
	vars["router"] = retInvertedRouterIP
	for _, c := range []struct{ dash, title, refID, col string }{
		{"link-state-nodes", "Nodes", "A", "node"},
		{"link-state-topology", "Topology", "nodes", "title"},
	} {
		got := columnValues(retentionRows(t, ctx, ch, c.dash+" / "+c.title,
			substituteGrafana(panelSQL(t, c.dash, c.title, c.refID), vars)), c.col)
		if !slices.Equal(got, []string{"ret-c-new"}) {
			t.Errorf("%s %q renders %v, want [ret-c-new]: the router numbered that "+
				"version last (seq 21) and the table's merge keeps it; ordering by "+
				"arrival time reads the older version until the merge runs",
				c.dash, c.title, got)
		}
	}
	for _, dash := range []string{"link-state-nodes", "link-state-links", "link-state-prefixes", "link-state-topology"} {
		got := columnValues(retentionRows(t, ctx, ch, dash+" $focus",
			substituteGrafana(dashboardVariableQuery(t, dash, "focus"), vars)), "__text")
		if !slices.Equal(got, []string{"ret-c-new"}) {
			t.Errorf("%s $focus offers %v, want [ret-c-new]", dash, got)
		}
	}
}

// requireInvertedNodeUnmerged stops merges on the retention database's
// ls_nodes_current until t ends, then puts back the inverted node's older
// version if a merge has already removed it.
//
// The fixture is written once per test binary, by whichever test asks for
// it first, so merges cannot simply be stopped when it is written and
// restarted when that test ends: the merge restarting them allows could run
// before this test. Stopping them here, and restoring the seq 20 row from
// history (where it is never merged away, since its sort key differs from
// seq 21's), makes the two unmerged rows this test's own precondition
// whatever ran before it, and -count=N repeats it.
func requireInvertedNodeUnmerged(t *testing.T, ctx context.Context, ch *ClickHouse) {
	t.Helper()
	if err := ch.conn.Exec(ctx, qualify(ch, "SYSTEM STOP MERGES vantage.ls_nodes_current")); err != nil {
		t.Fatalf("stop merges on ls_nodes_current: %v", err)
	}
	// context.WithoutCancel: ctx is canceled by the time Cleanup runs, and
	// START MERGES must still reach the server.
	t.Cleanup(func() {
		if err := ch.conn.Exec(context.WithoutCancel(ctx),
			qualify(ch, "SYSTEM START MERGES vantage.ls_nodes_current")); err != nil {
			t.Errorf("restart merges on ls_nodes_current: %v", err)
		}
	})
	where := " WHERE router_ip = toIPv6('" + retInvertedRouterIP + "') AND seq = 20"
	var older uint64
	if err := ch.conn.QueryRow(ctx, qualify(ch, "SELECT count() FROM vantage.ls_nodes_current"+where)).
		Scan(&older); err != nil {
		t.Fatalf("count the inverted node's older current row: %v", err)
	}
	if older == 0 {
		if err := ch.conn.Exec(ctx, qualify(ch,
			"INSERT INTO vantage.ls_nodes_current SELECT * FROM vantage.ls_nodes"+where)); err != nil {
			t.Fatalf("restore the inverted node's older current row: %v", err)
		}
	}
}

// The looking glasses and asn-view read routes in the window, which is
// history by design. What they must not do is call a route superseded
// because its session's Peer Up has expired: session_current is decided
// against the current tables.
func TestSessionCurrentFlagsSurviveRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch := requireRetentionDashFixture(t, ctx)

	for _, dash := range []string{"looking-glass", "looking-glass-vpn"} {
		t.Run(dash+"/collector picker", func(t *testing.T) {
			if got := columnValues(retentionVariable(t, ctx, ch, dash, "collector"), "collector_id"); !slices.Equal(got, []string{"c1"}) {
				t.Errorf("$collector offers %v, want [c1]", got)
			}
		})
	}
	t.Run("looking-glass/Covering routes", func(t *testing.T) {
		wantRetentionRow(t, "Covering routes", retentionPanel(t, ctx, ch, "looking-glass", "Covering routes", "A"),
			map[string]string{"prefix": "10.213.0.0/16", "session_current": "1"})
	})
	t.Run("looking-glass/"+lookingGlassStatTitle, func(t *testing.T) {
		wantRetentionRow(t, "What we know", retentionPanel(t, ctx, ch, "looking-glass", lookingGlassStatTitle, "A"),
			map[string]string{"routers_advertising": "1", "from_superseded_session": "0"})
	})
	t.Run("looking-glass-vpn/Covering routes", func(t *testing.T) {
		wantRetentionRow(t, "Covering routes", retentionPanel(t, ctx, ch, "looking-glass-vpn", "Covering routes", "A"),
			map[string]string{"prefix": "10.213.2.0/24", "session_current": "1"})
	})
	t.Run("looking-glass-vpn/"+lookingGlassVpnStatTitle, func(t *testing.T) {
		wantRetentionRow(t, "What we know", retentionPanel(t, ctx, ch, "looking-glass-vpn", lookingGlassVpnStatTitle, "A"),
			map[string]string{"vrfs_carrying_it": "1", "from_superseded_session": "0"})
	})
	t.Run("asn-view/Prefixes", func(t *testing.T) {
		wantRetentionRow(t, "Prefixes", retentionPanel(t, ctx, ch, "asn-view", "Prefixes", "A"),
			map[string]string{"prefix": "10.213.0.0/16", "role": "origin", "session_current": "1"})
	})
}
