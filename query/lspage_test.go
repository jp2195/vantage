package query

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLSNodesPageWalksEveryRowExactlyOnce is the load-bearing test for the
// whole feature: the union of the pages must equal the unpaginated answer for
// the same scope. A keyset that skips or repeats is the failure mode, and it
// is invisible in any single page.
func TestLSNodesPageWalksEveryRowExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSPageFixture(t, ctx, q) // 5 nodes, one scope, one session

	// State is left at its default (live) on both sides of the comparison,
	// not set to LSStateAny: LSNodesPage refuses to combine a cursor with
	// any node-identity narrowing, State included (see
	// LSNodeFilter.narrowing), so a walk cannot ask for state=any past page
	// one. The fixture carries no withdrawn rows, so live-only and
	// state=any agree on this scope regardless.
	whole, err := q.LSNodes(ctx, LSNodeFilter{Router: router, Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 5 {
		t.Fatalf("fixture: unpaginated answer has %d nodes, want 5", len(whole))
	}

	var walked []LSNode
	f := LSNodeFilter{Router: router, Peer: peer, Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("walk did not terminate in 10 pages: the cursor is not advancing")
		}
		page, next, err := q.LSNodesPage(ctx, f)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		f.Cursor = next
	}

	if len(walked) != len(whole) {
		t.Fatalf("walked %d nodes, unpaginated answer has %d: the keyset skipped or repeated",
			len(walked), len(whole))
	}
	seen := map[uint64]int{}
	for _, n := range walked {
		seen[n.NodeKey]++
	}
	for _, n := range whole {
		if seen[n.NodeKey] != 1 {
			t.Errorf("node_key %d appeared %d times across the walk, want exactly 1",
				n.NodeKey, seen[n.NodeKey])
		}
	}
}

// TestLSNodesPageIsStableWhenANameChangesMidWalk is the regression for the
// ordering decision recorded in lsNodesKey's own doc comment: live_name is
// argMax(name, (seq, stream_seq)) and keeps advancing within a pinned
// session, so a walk ordered on it could move a row between pages. Fetch
// page 1, rename a node that has not been reached yet -- a higher-seq
// ls_nodes row for the SAME identity, so live_name's argMax picks up the new
// name mid-walk without touching node_key -- then finish the walk: every
// node must still appear exactly once and the total must match the
// unpaginated answer.
//
// What prepending live_name to lsNodesKey actually does to THIS test: it
// makes page 2's query die with ClickHouse code 184 ("aggregate function...
// is found in WHERE"), because (*filters).keyset always renders the keyset
// predicate into f.conds -- the pre-GROUP-BY WHERE -- and live_name has no
// value until after the GROUP BY runs. That is a t.Fatal, not one of the
// t.Errorf calls below: the walk never reaches the uniqueness check, so
// THIS test's own duplicate/skip-detection code has never itself been
// observed to fire. What the mutation demonstrates is "an aggregate cannot
// sit in this key at all", not "a moved row would be caught by the
// assertions below".
//
// The assertion idiom itself -- len(walked) != len(whole), seen[key] != 1
// -- is still trustworthy: it is the identical pattern
// TestLSNodesPageWalksEveryRowExactlyOnce uses, and insertLSPageFixture's
// own doc comment records that dropping n.node_key from lsNodesKey (a
// physical, non-aggregate column with no WHERE-placement problem) makes
// that sibling test fail with a real tied-row duplicate. So the detection
// path has been watched to fire -- just not by this test's own mutation.
//
// The guarantee this test stands on also has a scope worth stating
// explicitly: it holds because keyset() always emits into f.conds (WHERE),
// never into f.havingExpr (HAVING) -- see filters.go's havings field, which
// exists precisely for predicates on mutable, argMax-derived attributes and
// which lsNodesSQL already wires a live HAVING slot for. Nothing in keyset()
// stops a future change from routing a key column through havingExpr
// instead of conds; if that happened, live_name (or any argMax'd column)
// would no longer trip ClickHouse's WHERE-clause rejection, and the
// original silent-skip-or-repeat hazard this test is named for would
// return -- through exactly the assertion path this comment says has never
// been exercised.
func TestLSNodesPageIsStableWhenANameChangesMidWalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSPageFixture(t, ctx, q) // 5 nodes, one scope, one session

	whole, err := q.LSNodes(ctx, LSNodeFilter{Router: router, Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 5 {
		t.Fatalf("fixture: unpaginated answer has %d nodes, want 5", len(whole))
	}

	f := LSNodeFilter{Router: router, Peer: peer, Limit: 2}
	page1, next, err := q.LSNodesPage(ctx, f)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if next == nil {
		t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
	}

	// The rename target: a node from `whole` that page 1 did NOT return, so
	// it is still ahead of the walk when its name changes.
	onPage1 := map[uint64]bool{}
	for _, n := range page1 {
		onPage1[n.NodeKey] = true
	}
	var target LSNode
	haveTarget := false
	for _, n := range whole {
		if !onPage1[n.NodeKey] {
			target = n
			haveTarget = true
			break
		}
	}
	if !haveTarget {
		t.Fatal("fixture: every node already appeared on page 1; need one held back for a later page")
	}

	// The rename itself: same identity columns (protocol, identifier, asn,
	// bgpls_id, area, router_id) as target, so node_key -- and so
	// lsNodesKey -- is unchanged, but a higher (seq, stream_seq) than every
	// row already written for this node, so live_name's argMax resolves to
	// THIS row's name once it lands.
	const renamedTo = "ls-page-renamed-midwalk"
	nodes, err := q.conn.PrepareBatch(ctx, "INSERT INTO "+q.db+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes rename: %v", err)
	}
	if err := nodes.Append(
		lsFixCollector, router, target.RouterSysName,
		peer, target.RIB, lsFixASN,
		router, lsPageFixtureSession, uint64(100),
		time.Now().UTC(), time.Now().UTC(), []string{}, uint64(100),
		target.Protocol, target.Identifier, target.ASN, target.BGPLSID, target.Area, target.RouterID,
		target.RouterIDv4,
		uint8(0), renamedTo,
		target.SRGBBase, target.SRGBSize, target.SRLBBase, target.SRLBSize,
		target.SRAlgorithms, map[uint16]string{},
	); err != nil {
		t.Fatalf("append renamed ls_nodes row: %v", err)
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send renamed ls_nodes row: %v", err)
	}

	walked := append([]LSNode(nil), page1...)
	f.Cursor = next
	for pages := 2; ; pages++ { // page 1 was already fetched above, before the rename
		if pages > 10 {
			t.Fatal("walk did not terminate in 10 pages: the cursor is not advancing")
		}
		page, next, err := q.LSNodesPage(ctx, f)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		f.Cursor = next
	}

	if len(walked) != len(whole) {
		t.Fatalf("walked %d nodes, unpaginated answer has %d: the mid-walk rename skipped or "+
			"repeated a row", len(walked), len(whole))
	}
	seen := map[uint64]int{}
	var gotName string
	for _, n := range walked {
		seen[n.NodeKey]++
		if n.NodeKey == target.NodeKey {
			gotName = n.Name
		}
	}
	for _, n := range whole {
		if seen[n.NodeKey] != 1 {
			t.Errorf("node_key %d appeared %d times across the walk, want exactly 1",
				n.NodeKey, seen[n.NodeKey])
		}
	}
	// Proof the rename actually took: without this, a broken insert above
	// would leave nothing for a name-ordered walk to trip over, and this
	// test would pass for the wrong reason no matter what lsNodesKey orders
	// on.
	if gotName != renamedTo {
		t.Errorf("renamed node's live_name in the walk = %q, want %q: the rename did not take, "+
			"so this test cannot exercise the hazard it is named for", gotName, renamedTo)
	}
}

// TestLSNodesPageRefusesAnUnscopedWalk pins the central paging rule: a
// fleet-wide walk spans many sessions, any of which can be superseded
// mid-walk, so it cannot be paged.
func TestLSNodesPageRefusesAnUnscopedWalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	_, _, err := q.LSNodesPage(ctx, LSNodeFilter{State: LSStateAny, Limit: 2})
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("unscoped LSNodesPage: err = %v, want ErrBadFilter", err)
	}
}

// TestLSNodesPageRefusesACursorWithANarrowingSet pins the fix for the defect
// ribPage never had a chance to hit: ribPage takes scalar router/peer/rib
// arguments, so there was nowhere for a node-identity narrowing to sneak in,
// but LSNodesPage takes the whole LSNodeFilter. RIBCursor has no field to
// record which narrowing (if any) a walk started under, so a page continued
// with a narrowing set -- whether the same one as page one, a different one,
// or one added where page one had none -- would silently return rows past
// the cursor from a different scope than the rows before it, with no error
// anywhere to catch the seam. See LSNodeFilter.narrowing.
func TestLSNodesPageRefusesACursorWithANarrowingSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSPageFixture(t, ctx, q)

	// A genuine cursor from an unfiltered page one, exactly what a caller
	// continuing a real walk would hold.
	_, next, err := q.LSNodesPage(ctx, LSNodeFilter{Router: router, Peer: peer, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if next == nil {
		t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
	}

	area, asn, nodeKey := lsFixArea, lsFixASN, uint64(1)
	for _, tc := range []struct {
		name      string
		narrowing LSNodeFilter
	}{
		{"protocol", LSNodeFilter{Protocol: lsFixProtocol}},
		{"area", LSNodeFilter{Area: &area}},
		{"asn", LSNodeFilter{ASN: &asn}},
		{"node_key", LSNodeFilter{NodeKey: &nodeKey}},
		{"state_withdrawn", LSNodeFilter{State: LSStateWithdrawn}},
		{"state_any", LSNodeFilter{State: LSStateAny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.narrowing
			f.Router, f.Peer, f.Limit, f.Cursor = router, peer, 2, next
			if _, _, err := q.LSNodesPage(ctx, f); !errors.Is(err, ErrBadFilter) {
				t.Fatalf("cursor + %s: err = %v, want ErrBadFilter", tc.name, err)
			}
		})
	}
}

// TestLSPrefixesPageWalksEveryRowExactlyOnce is
// TestLSNodesPageWalksEveryRowExactlyOnce's counterpart for LSPrefixesPage.
//
// The uniqueness check is keyed on (NodeKey, Prefix) rather than NodeKey
// alone, unlike the nodes test: a node row's whole identity is its NodeKey,
// but a prefix row's is (node, prefix) together, since one node originates
// several prefixes -- insertLSPrefixPageFixture's own doc comment gives the
// fixture's exact shape, including the cidr tie across two nodes that makes
// this test capable of failing against a key missing p.node_key.
func TestLSPrefixesPageWalksEveryRowExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSPrefixPageFixture(t, ctx, q) // 5 prefixes, 2 nodes, one scope, one session

	// State is left at its default (live) on both sides of the comparison,
	// not set to LSStateAny: LSPrefixesPage refuses to combine a cursor with
	// any narrowing, State included (see LSPrefixFilter.narrowing), so a
	// walk cannot ask for state=any past page one. The fixture carries no
	// withdrawn rows, so live-only and state=any agree on this scope
	// regardless.
	whole, err := q.LSPrefixes(ctx, LSPrefixFilter{Router: router, Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 5 {
		t.Fatalf("fixture: unpaginated answer has %d prefixes, want 5", len(whole))
	}

	var walked []LSPrefix
	f := LSPrefixFilter{Router: router, Peer: peer, Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("walk did not terminate in 10 pages: the cursor is not advancing")
		}
		page, next, err := q.LSPrefixesPage(ctx, f)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		f.Cursor = next
	}

	if len(walked) != len(whole) {
		t.Fatalf("walked %d prefixes, unpaginated answer has %d: the keyset skipped or repeated",
			len(walked), len(whole))
	}
	type rowID struct {
		nodeKey uint64
		prefix  string
	}
	seen := map[rowID]int{}
	for _, r := range walked {
		seen[rowID{r.NodeKey, r.Prefix}]++
	}
	for _, r := range whole {
		id := rowID{r.NodeKey, r.Prefix}
		if seen[id] != 1 {
			t.Errorf("(node_key %d, prefix %s) appeared %d times across the walk, want exactly 1",
				r.NodeKey, r.Prefix, seen[id])
		}
	}
}

// TestLSPrefixesPageRefusesAnUnscopedWalk is
// TestLSNodesPageRefusesAnUnscopedWalk's counterpart for LSPrefixesPage.
func TestLSPrefixesPageRefusesAnUnscopedWalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	_, _, err := q.LSPrefixesPage(ctx, LSPrefixFilter{State: LSStateAny, Limit: 2})
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("unscoped LSPrefixesPage: err = %v, want ErrBadFilter", err)
	}
}

// TestLSPrefixesPageRefusesACursorWithANarrowingSet is
// TestLSNodesPageRefusesACursorWithANarrowingSet's counterpart for
// LSPrefixesPage, extended to the two prefix-space narrowings LSPrefixFilter
// carries that LSNodeFilter does not: Prefix and Covers. See
// LSPrefixFilter.narrowing for why both belong on this list alongside the
// node-identity fields and State.
func TestLSPrefixesPageRefusesACursorWithANarrowingSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSPrefixPageFixture(t, ctx, q)

	// A genuine cursor from an unfiltered page one, exactly what a caller
	// continuing a real walk would hold.
	_, next, err := q.LSPrefixesPage(ctx, LSPrefixFilter{Router: router, Peer: peer, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if next == nil {
		t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
	}

	area, asn, nodeKey := lsFixArea, lsFixASN, uint64(1)
	for _, tc := range []struct {
		name      string
		narrowing LSPrefixFilter
	}{
		{"protocol", LSPrefixFilter{Protocol: lsFixProtocol}},
		{"area", LSPrefixFilter{Area: &area}},
		{"asn", LSPrefixFilter{ASN: &asn}},
		{"node_key", LSPrefixFilter{NodeKey: &nodeKey}},
		{"prefix", LSPrefixFilter{Prefix: "10.90.20.0/24"}},
		{"covers", LSPrefixFilter{Covers: "10.90.20.5"}},
		{"state_withdrawn", LSPrefixFilter{State: LSStateWithdrawn}},
		{"state_any", LSPrefixFilter{State: LSStateAny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.narrowing
			f.Router, f.Peer, f.Limit, f.Cursor = router, peer, 2, next
			if _, _, err := q.LSPrefixesPage(ctx, f); !errors.Is(err, ErrBadFilter) {
				t.Fatalf("cursor + %s: err = %v, want ErrBadFilter", tc.name, err)
			}
		})
	}
}

// TestLSLinksPageWalksParallelLinksExactlyOnce covers the measured case: in
// the archive, up to 2 distinct (local_ifaddr, remote_ifaddr) pairs share one
// (local_node_key, remote_node_key, link_local_id, link_remote_id). A key
// without the ifaddrs is not unique, so a page boundary landing inside such a
// pair drops one and repeats the other.
//
// The walk runs at Limit: 1, not 2 like the nodes and prefixes walks: at
// Limit 1 every single row is its own page boundary, so the boundary is
// GUARANTEED to land between the two parallel links regardless of where in
// the fixture's five rows they happen to sort -- unlike the nodes and
// prefixes fixtures, which had to place their own tied pair at a specific
// index to land it on a Limit: 2 boundary. See insertLSLinkPageFixture for
// the fixture's exact shape.
func TestLSLinksPageWalksParallelLinksExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSLinkPageFixture(t, ctx, q) // 5 links, one parallel pair, one scope, one session

	// State is left at its default (live) on both sides of the comparison,
	// not set to LSStateAny: LSLinksPage refuses to combine a cursor with
	// any narrowing, State included (see LSLinkFilter.narrowing), so a walk
	// cannot ask for state=any past page one. The fixture carries no
	// withdrawn rows, so live-only and state=any agree on this scope
	// regardless.
	whole, err := q.LSLinks(ctx, LSLinkFilter{Router: router, Peer: peer})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 5 {
		t.Fatalf("fixture: unpaginated answer has %d links, want 5", len(whole))
	}

	var walked []LSLink
	f := LSLinkFilter{Router: router, Peer: peer, Limit: 1}
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("walk did not terminate in 10 pages: the cursor is not advancing")
		}
		page, next, err := q.LSLinksPage(ctx, f)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		f.Cursor = next
	}

	if len(walked) != len(whole) {
		t.Fatalf("walked %d links, unpaginated answer has %d: the keyset skipped or repeated",
			len(walked), len(whole))
	}
	type rowID struct {
		localNode, remoteNode       uint64
		localIfaceID, remoteIfaceID uint32
		localIfAddr, remoteIfAddr   string
	}
	seen := map[rowID]int{}
	for _, l := range walked {
		seen[rowID{
			l.Local.NodeKey, l.Remote.NodeKey,
			l.Local.InterfaceID, l.Remote.InterfaceID,
			l.Local.IfAddr, l.Remote.IfAddr,
		}]++
	}
	for _, l := range whole {
		id := rowID{
			l.Local.NodeKey, l.Remote.NodeKey,
			l.Local.InterfaceID, l.Remote.InterfaceID,
			l.Local.IfAddr, l.Remote.IfAddr,
		}
		if seen[id] != 1 {
			t.Errorf("link (local %d/%d %s, remote %d/%d %s) appeared %d times across "+
				"the walk, want exactly 1",
				l.Local.NodeKey, l.Local.InterfaceID, l.Local.IfAddr,
				l.Remote.NodeKey, l.Remote.InterfaceID, l.Remote.IfAddr, seen[id])
		}
	}
}

// TestLSLinksPageRefusesAnUnscopedWalk is
// TestLSNodesPageRefusesAnUnscopedWalk's counterpart for LSLinksPage.
func TestLSLinksPageRefusesAnUnscopedWalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	_, _, err := q.LSLinksPage(ctx, LSLinkFilter{State: LSStateAny, Limit: 2})
	if !errors.Is(err, ErrBadFilter) {
		t.Fatalf("unscoped LSLinksPage: err = %v, want ErrBadFilter", err)
	}
}

// TestLSLinksPageRefusesACursorWithANarrowingSet is
// TestLSNodesPageRefusesACursorWithANarrowingSet's counterpart for
// LSLinksPage, covering the narrowings LSLinkFilter actually carries:
// Protocol, Area, ASN, LocalNode, RemoteNode and State. See
// LSLinkFilter.narrowing.
func TestLSLinksPageRefusesACursorWithANarrowingSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	q := requireQuery(t, ctx)
	router, peer := insertLSLinkPageFixture(t, ctx, q)

	// A genuine cursor from an unfiltered page one, exactly what a caller
	// continuing a real walk would hold.
	_, next, err := q.LSLinksPage(ctx, LSLinkFilter{Router: router, Peer: peer, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if next == nil {
		t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
	}

	area, asn, nodeKey := lsFixArea, lsFixASN, uint64(1)
	for _, tc := range []struct {
		name      string
		narrowing LSLinkFilter
	}{
		{"protocol", LSLinkFilter{Protocol: lsFixProtocol}},
		{"area", LSLinkFilter{Area: &area}},
		{"asn", LSLinkFilter{ASN: &asn}},
		{"local_node", LSLinkFilter{LocalNode: &nodeKey}},
		{"remote_node", LSLinkFilter{RemoteNode: &nodeKey}},
		{"state_withdrawn", LSLinkFilter{State: LSStateWithdrawn}},
		{"state_any", LSLinkFilter{State: LSStateAny}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.narrowing
			f.Router, f.Peer, f.Limit, f.Cursor = router, peer, 2, next
			if _, _, err := q.LSLinksPage(ctx, f); !errors.Is(err, ErrBadFilter) {
				t.Fatalf("cursor + %s: err = %v, want ErrBadFilter", tc.name, err)
			}
		})
	}
}

// TestLSPageRefusesASupersededSession pins a gap: nothing proved the
// three *Page functions actually call sessionStillCurrent.
// TestRIBPageUnicastRefusesASupersededSession covers ribPage's own copy of
// this check; nothing in query/ or api/ covered LSNodesPage's, LSLinksPage's
// or LSPrefixesPage's, and deleting the call from any one of them left this
// package's whole suite green.
//
// It matters more here than it does for ribPage: lsNodesSQL's (and
// lsLinksSQL's and lsPrefixesSQL's) own INNER JOIN against cur pins every
// page's query to one session, so a superseded session does not merely risk
// answering from a stale dump -- the page query itself returns ZERO rows,
// which is indistinguishable from "the walk finished" unless
// sessionStillCurrent turns it into an error instead. See
// sessionStillCurrent's own doc comment.
//
// Each subtest walks page 1 of its own *Page fixture at a Limit that leaves a
// second page, writes a peer_events row for the SAME (collector, router) with
// a higher session_id than the fixture's own -- the shape a router that
// reconnected and re-dumped leaves behind, the same hazard
// insertRIBResetFixture models for ribPage -- and requests page 2 with page
// 1's cursor. errors.Is(err, ErrSessionChanged), zero rows and a nil cursor
// together are what makes a superseded walk distinguishable from a finished
// one at the call site; a client that reads a nil cursor as "done" must never
// see one from a walk whose session no longer exists.
func TestLSPageRefusesASupersededSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// reconnect writes the peer_events row that supersedes session for
	// (lsFixCollector, routerIP): a fresh "up" at a higher session_id than
	// whatever the fixture under test already wrote. ribCurrentSession reads
	// max(session_id) across ALL peer_events rows for (collector, router)
	// regardless of peer (see ribSessionSQL), so peerIP need not be the same
	// peer the walk is scoped to for this to supersede it.
	reconnect := func(t *testing.T, q *Q, routerIP, peerIP, sysname string, session uint64) {
		t.Helper()
		now := time.Now().UTC()
		insertPeerEvent(t, ctx, q, peerEventFixture{
			RouterIP: routerIP, RouterSysname: sysname, PeerIP: peerIP, RIB: "in_pre",
			Collector: lsFixCollector,
			PeerASN:   lsFixASN, PeerBGPID: peerIP,
			SessionID: session, Seq: 1, StreamSeq: 1,
			Kind: "up", TsRouter: now, TsCollector: now,
		})
	}

	t.Run("nodes", func(t *testing.T) {
		q := requireQuery(t, ctx)
		router, peer := insertLSPageFixture(t, ctx, q) // 5 nodes, one scope, one session

		f := LSNodeFilter{Router: router, Peer: peer, Limit: 2}
		_, next, err := q.LSNodesPage(ctx, f)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if next == nil {
			t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
		}

		reconnect(t, q, lsPageFixtureRouterIP, lsPageFixturePeerIP, "ls-page-router",
			lsPageFixtureSession+1)

		f.Cursor = next
		rows, cur, err := q.LSNodesPage(ctx, f)
		if !errors.Is(err, ErrSessionChanged) {
			t.Fatalf("page 2 after reconnect: err = %v, want ErrSessionChanged", err)
		}
		if len(rows) != 0 || cur != nil {
			t.Errorf("got %d rows and cursor %+v alongside the error; a superseded "+
				"walk returns nothing, never a plausible page from another dump",
				len(rows), cur)
		}
	})

	t.Run("links", func(t *testing.T) {
		q := requireQuery(t, ctx)
		router, peer := insertLSLinkPageFixture(t, ctx, q) // 5 links, one scope, one session

		f := LSLinkFilter{Router: router, Peer: peer, Limit: 2}
		_, next, err := q.LSLinksPage(ctx, f)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if next == nil {
			t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
		}

		reconnect(t, q, lsLinkPageFixtureRouterIP, lsLinkPageFixturePeerIP, "ls-link-page-router",
			lsLinkPageFixtureSession+1)

		f.Cursor = next
		rows, cur, err := q.LSLinksPage(ctx, f)
		if !errors.Is(err, ErrSessionChanged) {
			t.Fatalf("page 2 after reconnect: err = %v, want ErrSessionChanged", err)
		}
		if len(rows) != 0 || cur != nil {
			t.Errorf("got %d rows and cursor %+v alongside the error; a superseded "+
				"walk returns nothing, never a plausible page from another dump",
				len(rows), cur)
		}
	})

	t.Run("prefixes", func(t *testing.T) {
		q := requireQuery(t, ctx)
		router, peer := insertLSPrefixPageFixture(t, ctx, q) // 5 prefixes, one scope, one session

		f := LSPrefixFilter{Router: router, Peer: peer, Limit: 2}
		_, next, err := q.LSPrefixesPage(ctx, f)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if next == nil {
			t.Fatal("fixture: page 1 of a 5-row fixture at Limit 2 returned no cursor")
		}

		reconnect(t, q, lsPrefixPageFixtureRouterIP, lsPrefixPageFixturePeerIP, "ls-prefix-page-router",
			lsPrefixPageFixtureSession+1)

		f.Cursor = next
		rows, cur, err := q.LSPrefixesPage(ctx, f)
		if !errors.Is(err, ErrSessionChanged) {
			t.Fatalf("page 2 after reconnect: err = %v, want ErrSessionChanged", err)
		}
		if len(rows) != 0 || cur != nil {
			t.Errorf("got %d rows and cursor %+v alongside the error; a superseded "+
				"walk returns nothing, never a plausible page from another dump",
				len(rows), cur)
		}
	})
}
