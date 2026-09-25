package query

import (
	"cmp"
	"context"
	"strings"
	"testing"
	"time"
)

// peerEventsCols names the nineteen peer_events columns these fixtures set,
// so their inserts are addressed by NAME rather than by position.
//
// hold_time, hold_time_seen, mp_families, addpath_families and sys_descr
// were added to peer_events, and every positional fixture in this package
// broke at once with "expected 24 arguments, got 19". That is the good
// version of the failure -- loud, immediate, and impossible to miss -- but
// it is also a failure none of these tests had an opinion about: they are
// about events and routers, not about capabilities. Named columns let the
// five take their DEFAULTs, so the next column added to the schema does not
// break a fixture that never mentioned it.
//
// The SINK's own insert stays positional on purpose. There, the argument
// count IS the check that the writer and the schema agree -- see the note
// above insertUnicast.
// peerEventsColsV4 is peerEventsCols plus the five session-fact columns
// (hold_time, hold_time_seen, mp_families, addpath_families, sys_descr),
// for the one fixture helper that sets them.
const peerEventsColsV4 = "(collector_id, router_ip, router_sysname, peer_ip, rib, " +
	"peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, " +
	"parse_flags, stream_seq, kind, local_ip, local_port, remote_port, " +
	"down_reason, cap_four_byte_as, hold_time, hold_time_seen, mp_families, " +
	"addpath_families, sys_descr)"

const peerEventsCols = "(collector_id, router_ip, router_sysname, peer_ip, rib, " +
	"peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, " +
	"parse_flags, stream_seq, kind, local_ip, local_port, remote_port, " +
	"down_reason, cap_four_byte_as)"

// newerSessionID and olderSessionID are the two session_id values
// insertTwoSessionRouter writes for the same router. The numbers
// themselves carry no meaning beyond newer > older; what matters is that
// the test below asserts Routers resolved to the larger one via
// max(session_id), not via insertion order (see insertTwoSessionRouter's
// own comment).
const (
	olderSessionID = 1
	newerSessionID = 2
)

// peerEventFixture is the write-side shape this package's own tests need
// to build one peer_events row. It carries far fewer fields than
// sink.PeerRow: this package never writes production data, only enough of
// a fixture for its own queries to read back, and sink's insertPeer is
// unexported besides.
type peerEventFixture struct {
	RouterIP, RouterSysname, PeerIP, RIB string
	// Collector is the collector_id this row is attributed to. Every
	// fixture written before session identity became (collector, router)
	// leaves it empty and gets defaultFixtureCollector, the one
	// collector_id insertPeerEvent used to hard-code -- so only a fixture
	// whose whole point is that two collectors saw the same router has to
	// name it. See insertTwoCollectorFixture.
	Collector                 string
	PeerASN                   uint32
	PeerBGPID                 string
	SessionID, Seq, StreamSeq uint64
	Kind                      string
	TsRouter, TsCollector     time.Time

	// The session-fact columns. Zero values are meaningful and are what
	// every fixture written before this got: HoldTimeSeen 0 is "no OPEN
	// was observed for this event", which is exactly true of a Peer Down
	// and of every row already in the archive.
	HoldTime                    uint16
	HoldTimeSeen                uint8
	MPFamilies, AddPathFamilies []string
	SysDescr                    string
}

// defaultFixtureCollector is the collector_id every peer_events row in this
// package carried before peerEventFixture could vary it. Fixtures that do
// not care which collector observed a row still write this one rather than
// an empty string: collector_id is part of session identity now (see
// peerStateCTE), so a fixture leaving it blank would be asserting against a
// collector that no deployment has.
const defaultFixtureCollector = "query-test"

// insertPeerEvent writes one peer_events row via q's own connection and
// database, in the same column order sink.insertPeer uses against the same
// table -- see the comment above that function for why the argument count
// and order there matter. The columns Routers never reads (local_ip,
// local_port, remote_port, down_reason, cap_four_byte_as) are pinned to
// their "no value" forms rather than threaded through peerEventFixture,
// since no test in this package needs them to vary yet.
func insertPeerEvent(t *testing.T, ctx context.Context, q *Q, f peerEventFixture) {
	t.Helper()
	// The 24-column list, not peerEventsCols' nineteen: this is the helper
	// fixtures use when the session facts are the point.
	b, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".peer_events "+peerEventsColsV4)
	if err != nil {
		t.Fatalf("prepare peer_events insert: %v", err)
	}
	// Non-nil empty slices, never nil: the columns are
	// Array(LowCardinality(String)) and an empty array is the honest
	// rendering of "negotiated nothing", which is what a fixture that says
	// nothing about families means.
	mp, ap := f.MPFamilies, f.AddPathFamilies
	if mp == nil {
		mp = []string{}
	}
	if ap == nil {
		ap = []string{}
	}
	if err := b.Append(
		cmp.Or(f.Collector, defaultFixtureCollector),
		f.RouterIP, f.RouterSysname, f.PeerIP, f.RIB, f.PeerASN,
		f.PeerBGPID, f.SessionID, f.Seq, f.TsRouter, f.TsCollector,
		[]string{}, f.StreamSeq,
		f.Kind, "::", uint16(0), uint16(0), uint32(0), uint8(0),
		f.HoldTime, f.HoldTimeSeen, mp, ap, f.SysDescr,
	); err != nil {
		t.Fatalf("append peer_events row: %v", err)
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send peer_events batch: %v", err)
	}
}

// insertTwoSessionRouter writes one router's peer through two sessions: the
// newer one first, up, then the older one second, down. The insertion
// order is deliberately the reverse of session recency -- a Routers query
// that resolved "the current session" by taking the last row written
// rather than max(session_id) would pass every other case in this package
// and only fail here, on the newer session's row not being the one
// physically inserted last.
func insertTwoSessionRouter(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const routerIP = "10.0.0.1"
	const sysname = "two-session-router"
	const peerIP = "10.0.0.2"

	now := time.Now().UTC()
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: peerIP,
		SessionID: newerSessionID, Seq: 1, StreamSeq: 0,
		Kind:        "up",
		TsRouter:    now,
		TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: peerIP,
		SessionID: olderSessionID, Seq: 1, StreamSeq: 0,
		Kind:        "down",
		TsRouter:    now.Add(-time.Hour),
		TsCollector: now.Add(-time.Hour),
	})
}

// routerNamed finds the single row in got whose SysName is sysname and
// fails the test otherwise.
//
// It exists because chtest recreates the test database once per test
// binary, not once per test function (see chtest.ensureDatabase): every
// test in this package inserts into the same vantage_query_test and never
// cleans up after itself, so by the time a later test's Routers call runs,
// got also carries whatever rows earlier tests in this run left behind.
// Asserting len(got) == 1 or indexing got[0] treats "nothing else in the
// database" as part of this test's own claim, so it starts failing the
// moment any other test in the package (including ones added later)
// inserts a second, distinct router. Filtering to the fixture's own
// sysname is what keeps this test's assertion scoped to what this test
// actually set up.
func routerNamed(t *testing.T, got []Router, sysname string) Router {
	t.Helper()
	var matches []Router
	for _, r := range got {
		if r.SysName == sysname {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("Routers: got %d rows named %q, want 1: %+v", len(matches), sysname, got)
	}
	return matches[0]
}

func TestRoutersReportsOneRowPerRouterOnItsNewestSession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertTwoSessionRouter(t, ctx, q)

	got, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	// routerNamed itself fails if two-session-router appears more than
	// once -- the router has two sessions, not two identities.
	r := routerNamed(t, got, "two-session-router")
	// insertTwoSessionRouter writes the older session second, so a query
	// that takes "the last row inserted" rather than max(session_id)
	// passes everything else and fails here.
	if r.SessionID != newerSessionID {
		t.Errorf("SessionID = %d, want the newer session %d", r.SessionID, newerSessionID)
	}
}

// insertRecoveredPeer writes one router's one peer through a single
// session in which it went down and then came back up: down at seq 1,
// up at seq 2. The up row -- seq 2, the state the session actually ended
// in -- is inserted first; the down row is inserted second, so the
// physical insertion order is the reverse of seq order.
//
// Both rows share one session_id, which is deliberate but narrower than
// it first looks: it proves argMax(p.kind, (p.seq, p.stream_seq)) orders
// by seq rather than by insertion order or by "was this peer ever down",
// but it does NOT exercise peerStateCTE's "AND p.session_id = cur.sid"
// join predicate -- with one session_id in play, that predicate is
// trivially true for every row this fixture writes, so deleting it
// changes nothing here. See
// TestRoutersScopesArgMaxToTheCurrentSessionNotJustHighestSeq /
// insertPeerWhoseOldSessionOutlivesItInSeq below for the fixture that
// makes the join predicate itself load-bearing.
func insertRecoveredPeer(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const routerIP = "10.0.0.3"
	const sysname = "recovered-peer-router"
	const peerIP = "10.0.0.4"
	const sessionID = 9

	now := time.Now().UTC()
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: peerIP,
		SessionID: sessionID, Seq: 2, StreamSeq: 0,
		Kind:        "up",
		TsRouter:    now,
		TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: peerIP,
		SessionID: sessionID, Seq: 1, StreamSeq: 0,
		Kind:        "down",
		TsRouter:    now.Add(-time.Minute),
		TsCollector: now.Add(-time.Minute),
	})
}

// TestRoutersResolvesADownThenUpPeerAsUpWithinOneSession covers ordering:
// a peer that went down and then came back up within its router's
// current session must count as up, not down -- exactly the pattern
// behind 12 of the live archive's 17 down events. Replacing the
// argMax(..., (p.seq, p.stream_seq)) ordering with anything that isn't
// seq-based fails this test. It does
// NOT cover the current-session join predicate -- see insertRecoveredPeer's
// doc comment for why, and TestRoutersScopesArgMaxToTheCurrentSessionNotJustHighestSeq
// below for the test that does.
func TestRoutersResolvesADownThenUpPeerAsUpWithinOneSession(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertRecoveredPeer(t, ctx, q)

	got, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	r := routerNamed(t, got, "recovered-peer-router")
	if r.PeersUp != 1 || r.PeersDown != 0 {
		t.Errorf("PeersUp=%d PeersDown=%d, want up=1 down=0 -- the peer's "+
			"session ended up (seq 2), even though the down row (seq 1) is "+
			"the one that landed in peer_events last", r.PeersUp, r.PeersDown)
	}
}

// insertPeerWhoseOldSessionOutlivesItInSeq writes one router's one peer
// through two sessions where seq does NOT track session recency: the old
// session (the lower session_id) carries a HIGH seq (500, down), and the
// new session (the higher session_id) carries a LOW seq (1, up). This is
// realistic, not contrived -- seq restarts at zero when a session starts
// (see peerStateCTE's doc comment), so a session that has been up for a
// long time can easily reach a higher seq than one that only just began.
//
// This is what makes peerStateCTE's "AND p.session_id = cur.sid" join
// predicate load-bearing rather than redundant. Without it, argMax(p.kind,
// (p.seq, p.stream_seq)) would run across both sessions and pick the old
// session's down row, seq 500 beating seq 1 -- exactly backwards, since
// the old session is not the one anyone should be asking about. With the
// predicate, only the new session's row survives the join to cur, and the
// peer resolves up. insertRecoveredPeer above cannot exercise this: both
// of its rows share one session_id, so the predicate is trivially true
// for that fixture and removing it changes nothing there.
//
// The up row (in-scope, correct) is inserted first and the down row
// (out-of-scope, higher seq) second, so this also defeats "the last row
// physically inserted wins".
func insertPeerWhoseOldSessionOutlivesItInSeq(t *testing.T, ctx context.Context, q *Q) {
	t.Helper()
	const routerIP = "10.0.0.5"
	const sysname = "seq-outlives-session-router"
	const peerIP = "10.0.0.6"
	const oldSessionID = 10
	const newSessionID = 11

	now := time.Now().UTC()
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: peerIP,
		SessionID: newSessionID, Seq: 1, StreamSeq: 0,
		Kind:        "up",
		TsRouter:    now,
		TsCollector: now,
	})
	insertPeerEvent(t, ctx, q, peerEventFixture{
		RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
		PeerASN: 65000, PeerBGPID: peerIP,
		SessionID: oldSessionID, Seq: 500, StreamSeq: 0,
		Kind:        "down",
		TsRouter:    now.Add(-time.Hour),
		TsCollector: now.Add(-time.Hour),
	})
}

// TestRoutersScopesArgMaxToTheCurrentSessionNotJustHighestSeq is
// regression coverage for the join predicate itself: a router whose
// OLD session reached a higher seq than its NEW session must still
// resolve to the new session's state, because seq is only comparable
// within one session, never across two. Deleting "AND p.session_id =
// cur.sid" from peerStateCTE's join makes this test fail (PeersDown
// becomes 1, PeersUp becomes 0) while every other test in this package
// still passes.
func TestRoutersScopesArgMaxToTheCurrentSessionNotJustHighestSeq(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertPeerWhoseOldSessionOutlivesItInSeq(t, ctx, q)

	got, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	r := routerNamed(t, got, "seq-outlives-session-router")
	if r.PeersUp != 1 || r.PeersDown != 0 {
		t.Errorf("PeersUp=%d PeersDown=%d, want up=1 down=0 -- the old "+
			"session's down row (seq 500) must not outrank the current "+
			"session's up row (seq 1) just because its seq is numerically "+
			"higher", r.PeersUp, r.PeersDown)
	}
}

// TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree guards the invariant
// routers.go's own comment calls out: any(sid) only returns the right
// value because cur has exactly one row -- one sid -- per group routersSQL
// aggregates, and that holds only while cur's GROUP BY and routersSQL's
// are the same pair of columns. Widening cur's (say, to add peer_ip) or
// narrowing routersSQL's (collapsing two collectors into one row is the
// tempting one) would each make any(sid) pick an arbitrary session per
// group -- right on roughly half of any two-session fixture, and therefore
// a bug no result-based test could catch reliably. Checking the SQL text
// is the same defense this repo already uses for argMax versus any
// elsewhere (see sink/dashboard_sql_test.go's
// TestRibBrowserRoutesResolveEveryObservationColumnOnOneClock).
//
// It checks both halves rather than cur alone: with one grouping column
// there is only one way for the pair to disagree, and with two there are
// three.
//
// Checking routersSQL's half means checking the statement with peerStateCTE
// and peerUpCTE cut off the front, and that is not tidiness. routersSQL IS
// "WITH " + peerStateCTE + peerUpCTE + the outer statement, so a
// strings.Contains for the grouping key over the whole const would be
// satisfied by cur's own GROUP BY, sitting inside an embedded CTE, no
// matter what the outer statement does -- an assertion that cannot fail,
// as mutating routersSQL to any(collector_id) + GROUP BY router_ip and
// re-running the suite demonstrates. That mutation is the very "collapse
// two collectors into one row" case routers.go's doc comment names as the
// tempting mistake. CutPrefix, not TrimPrefix, so that routersSQL ceasing
// to be built this way fails here rather than silently restoring the
// vacuous check.
//
// The outer statement now holds TWO aggregations at that grain --
// router_state, which any(sid) is taken over, and router_peers, which counts
// peers off peer_up -- so this counts occurrences rather than asking whether
// one exists. Contains alone would be satisfied by router_peers' GROUP BY
// while router_state had been narrowed to router_ip, which is the vacuity
// the paragraph above describes, one level in.
func TestRoutersUsesAnySidBecauseCurAndThisGroupByAgree(t *testing.T) {
	const key = "GROUP BY collector_id, router_ip\n"
	if !strings.Contains(peerStateCTE, key) {
		t.Fatalf("cur no longer groups by (collector_id, router_ip):\n%s\n"+
			"routers.go's any(sid) is only correct when cur has exactly "+
			"one row (one sid) per group routersSQL aggregates -- changing "+
			"this GROUP BY makes any(sid) pick an arbitrary session.",
			peerStateCTE)
	}
	outer, ok := strings.CutPrefix(routersSQL, "WITH "+peerStateCTE+peerUpCTE)
	if !ok {
		t.Fatalf("routersSQL no longer starts with \"WITH \" + peerStateCTE + "+
			"peerUpCTE, so this test can no longer tell router_state's GROUP BY "+
			"from cur's or peer_up's and would pass on any of them:\n%s", routersSQL)
	}
	if got := strings.Count(outer, key); got != 2 {
		t.Fatalf("routersSQL's outer statement groups by (collector_id, "+
			"router_ip) %d times, want 2 (router_state and router_peers):\n%s\n"+
			"both have to aggregate exactly the pair cur is keyed on; a "+
			"narrower GROUP BY merges two collectors' sessions into one row "+
			"and makes any(sid) arbitrary, a wider one leaves peer counts "+
			"split across rows that share a session, and a mismatch between "+
			"the two turns the join between them into a fan-out.",
			got, outer)
	}
	if !strings.Contains(outer, "any(sid)") {
		t.Fatalf("routersSQL no longer selects any(sid); if it now "+
			"resolves session_id some other way, update this test to guard "+
			"whatever invariant makes that choice correct:\n%s", outer)
	}
	// And the counts come off peer_up, not peer_state. The result-level
	// guard is TestRoutersCountsPeersNotRibViews; this one names the CTE, so
	// that a counting expression moved back onto peer_state fails here even
	// if a future fixture happens to keep every peer under one rib.
	if !strings.Contains(outer, "FROM peer_up") {
		t.Fatalf("routersSQL no longer counts peers off peer_up:\n%s\n"+
			"peer_state's grain is (collector, router, peer, RIB), so "+
			"counting it counts rib views: a peer mirrored pre- and "+
			"post-policy is counted twice, and one whose ribs disagree lands "+
			"in both peers_up and peers_down.", outer)
	}
}

// TestRoutersDoesNotDropASecondCollectorsView pins the semantic that
// session identity is per (collector, router), not per router.
//
// The failure this guards is silent: cur grouping by router_ip alone takes
// max(session_id) across BOTH collectors, and since session_id is
// now().UnixNano() assigned independently by each collector, whichever
// collector's clock is momentarily ahead wins the whole router. Every peer
// the other collector saw then vanishes from every current-state answer,
// with no error and no empty result to notice -- just a smaller fleet,
// reported confidently.
//
// c2's session id is deliberately the LARGER of the two, so an
// implementation that still groups by router alone returns c2's row and
// drops c1's, rather than returning both by luck of ordering. The peers_up
// assertion is what holds peer_state's own join predicate: see
// insertTwoCollectorFixture's doc comment for the third row, dev-c2's
// superseded session, which reappears in dev-c2's answer as a second up
// peer the moment that join stops matching on collector_id.
func TestRoutersDoesNotDropASecondCollectorsView(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertTwoCollectorFixture(t, ctx, q)

	got, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}

	seen := map[string]Router{}
	for _, r := range got {
		if r.IP.String() == tcRouter {
			seen[r.Collector] = r
		}
	}
	if len(seen) != 2 {
		t.Fatalf("router %s reported by %d collectors, want 2 (%v); grouping by "+
			"router alone silently discards one collector's entire view",
			tcRouter, len(seen), seen)
	}
	if got := seen["dev-c1"].SessionID; got != tcSessionC1 {
		t.Errorf("dev-c1 session = %d, want %d", got, tcSessionC1)
	}
	if got := seen["dev-c2"].SessionID; got != tcSessionC2 {
		t.Errorf("dev-c2 session = %d, want %d", got, tcSessionC2)
	}
	if seen["dev-c1"].PeersUp != 1 || seen["dev-c2"].PeersUp != 1 {
		t.Errorf("peers_up = c1:%d c2:%d, want 1 each -- dev-c2's superseded "+
			"session (whose id collides with dev-c1's current one) must not "+
			"contribute a peer to dev-c2's current-state answer",
			seen["dev-c1"].PeersUp, seen["dev-c2"].PeersUp)
	}
}

// TestRoutersCountsPeersNotRibViews is the result-level guard for
// routersSQL's peer counts, and it needed a fixture built for it: every
// insertPeerEvent call written before insertTwoRibPeerFixture hard-codes
// RIB: "in_pre", so a package whose every peer sits under exactly one rib
// cannot tell a per-peer count from a per-(peer, rib) one no matter what it
// asserts.
//
// What it defends is a wrong number, not an error. Counting peer_state rows
// -- the shape this statement carried before route counts moved onto
// peer_up -- reads 3 up and 1 down for a router with two peers, one up and
// one down. Both
// halves of that are wrong in different ways, and insertTwoRibPeerFixture's
// doc comment lays out which row produces which. The sum assertion is the
// one that cannot be satisfied by any rib-grained count at all: both of this
// fixture's peers resolve up or down, so peers_up + peers_down has to be 2,
// while under peer_state it counts (peer, rib) pairs and reads 4. It is a
// claim about this fixture rather than a general identity -- a peer whose
// newest event is 'unspecified' is in neither total, so the sum is a bound
// and not an equality; see routersSQL.
func TestRoutersCountsPeersNotRibViews(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertTwoRibPeerFixture(t, ctx, q)

	got, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	r := routerNamed(t, got, twoRibSysname)

	// The fixture's premise, asserted rather than assumed: this router's
	// peers really are written under two ribs. If a later edit collapses
	// them onto one, every assertion below still passes and stops meaning
	// anything.
	var ribs uint64
	if err := q.conn.QueryRow(ctx,
		"SELECT uniqExact(rib) FROM "+q.db+".peer_events WHERE router_ip = toIPv6(?)",
		twoRibRouter).Scan(&ribs); err != nil {
		t.Fatalf("count ribs: %v", err)
	}
	if ribs != 2 {
		t.Fatalf("insertTwoRibPeerFixture wrote %d rib(s) for %s, want 2 -- "+
			"with one rib per peer this test cannot distinguish a peer count "+
			"from a (peer, rib) count and asserts nothing", ribs, twoRibRouter)
	}

	if r.PeersUp != 1 {
		t.Errorf("PeersUp = %d, want 1 -- %s is ONE peer that the router "+
			"mirrors under two ribs, not two peers", r.PeersUp, twoRibUpPeer)
	}
	if r.PeersDown != 1 {
		t.Errorf("PeersDown = %d, want 1 -- %s ended its session down "+
			"(its newest event across every rib is the in_post down)",
			r.PeersDown, twoRibSplitPeer)
	}
	if sum := r.PeersUp + r.PeersDown; sum != 2 {
		t.Errorf("PeersUp + PeersDown = %d for a router with 2 peers -- a peer "+
			"counted once per rib view inflates the total, and one whose ribs "+
			"disagree is counted in both directions at once", sum)
	}
}

// TestRoutersReportsAViewLostPeerRatherThanLosingIt covers what the new
// third kind does to a shape built around two.
//
// routersSQL counts with countIf(state = 'up') and countIf(state = 'down'),
// so a peer whose newest event is 'view_lost' satisfies neither and would
// simply disappear -- and a router whose collector died is a router where
// EVERY peer is view-lost, so the whole shape reports peers_up = 0,
// peers_down = 0. That reads as "this router has no peers", which is a new
// wrong answer in place of the stale one Session.Close removed. routersSQL's
// own doc comment named this hazard before there was a kind that could
// trigger it: "Do not restore the stronger claim without adding somewhere
// for an unresolved peer to go." PeersViewLost is that somewhere.
func TestRoutersReportsAViewLostPeerRatherThanLosingIt(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertViewLostFixture(t, ctx, q)

	got, err := q.Routers(ctx)
	if err != nil {
		t.Fatalf("Routers: %v", err)
	}
	r := routerNamed(t, got, viewLostSysname)
	if r.PeersUp != 1 || r.PeersDown != 1 || r.PeersViewLost != 1 {
		t.Errorf("PeersUp=%d PeersDown=%d PeersViewLost=%d, want 1/1/1 -- the "+
			"fixture's three peers ended the session up, down and view-lost, "+
			"and each has to land in exactly one bucket",
			r.PeersUp, r.PeersDown, r.PeersViewLost)
	}
	// The counts have to add up to the peers the router actually has, or a
	// reader cannot tell "no peers" from "no view of them".
	if total := r.PeersUp + r.PeersDown + r.PeersViewLost; total != 3 {
		t.Errorf("counts sum to %d over a 3-peer router", total)
	}
}
