package query

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

// TestLSNodesReturnsOneRowPerObserver pins a decision, made
// executable. Two peers report one node; the answer carries TWO rows, and a
// statement that grouped by node_key alone returns one.
func TestLSNodesReturnsOneRowPerObserver(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSNodeFixture(t, ctx, q)

	got, err := q.LSNodes(ctx, LSNodeFilter{})
	if err != nil {
		t.Fatalf("LSNodes: %v", err)
	}
	shared := 0
	for _, n := range got {
		if n.Name == "ls-shared" {
			shared++
		}
	}
	// This exact count is load-bearing for every OTHER fixture in the
	// package, not just this one: query/ shares a single ClickHouse
	// database, so anything writing ls_nodes rows -- insertLSLinkFixture
	// does, TWELVE of them: ten in the current session and two in a
	// previous one -- adds to what this test counts. A new row named
	// "ls-shared" breaks this test from a different file. See
	// insertLSLinkFixture's doc comment for the constraints that keeps.
	if shared != 2 {
		t.Errorf("the shared node came back %d times, want 2 -- one row per "+
			"observer is the contract, and 1 means the statement collapsed two "+
			"routers' reports into a winner it picked silently", shared)
	}
}

// TestLSNodesExcludesWithdrawnByDefault, and its other half: state=withdrawn
// returns exactly the row the default hides. Asserting only the first would
// pass against a statement that returned nothing at all.
func TestLSNodesExcludesWithdrawnByDefault(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSNodeFixture(t, ctx, q)

	live, err := q.LSNodes(ctx, LSNodeFilter{})
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	for _, n := range live {
		if n.Name == "ls-gone" {
			t.Errorf("a withdrawn node was returned by the default filter: %+v", n)
		}
	}
	if len(live) == 0 {
		t.Fatal("the live answer is empty, so the assertion above is vacuous")
	}

	gone, err := q.LSNodes(ctx, LSNodeFilter{State: LSStateWithdrawn})
	if err != nil {
		t.Fatalf("withdrawn: %v", err)
	}
	// Exactly one, and that count constrains the shared database rather
	// than just this fixture: every ls_nodes row any other fixture writes
	// must be live, or it lands here. insertLSLinkFixture writes twelve and
	// keeps that rule; see its doc comment.
	if len(gone) != 1 || gone[0].Name != "ls-gone" {
		t.Errorf("state=withdrawn returned %d rows (%+v), want exactly the "+
			"withdrawn node", len(gone), gone)
	}

	any, err := q.LSNodes(ctx, LSNodeFilter{State: LSStateAny})
	if err != nil {
		t.Fatalf("any: %v", err)
	}
	if len(any) != len(live)+len(gone) {
		t.Errorf("state=any returned %d rows, want %d (%d live + %d withdrawn) "+
			"-- the three states must partition the answer",
			len(any), len(live)+len(gone), len(live), len(gone))
	}
}

// TestLSNodesDumpStateIgnoresDuplicateEndOfRibMarkers is lsEorCTE's own
// doc-comment warning made executable. That comment says a count() there
// "would report a collection artifact as a fact about the network," but
// until this test, nothing in the package could tell toUInt8(1) and
// count() apart: every other insertLS* fixture writes zero ls_events rows,
// so LEFT JOIN eor finds no match and coalesce(eor.marked, 0) reads 0
// either way. insertLSDumpStateFixture writes two duplicate end-of-rib
// markers for one session -- the shape the archive itself has (one
// session with 13, per lsEorCTE's comment) -- so `marked` is 1 under the
// real implementation and 2 under the count() mutation, and only the
// former reads dump_state "complete".
func TestLSNodesDumpStateIgnoresDuplicateEndOfRibMarkers(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSDumpStateFixture(t, ctx, q)

	got, err := q.LSNodes(ctx, LSNodeFilter{
		Router: netip.MustParseAddr(lsDumpStateFixtureRouterIP),
	})
	if err != nil {
		t.Fatalf("LSNodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows for this fixture's own router, want exactly 1: %+v",
			len(got), got)
	}
	if got[0].DumpState != "complete" {
		t.Errorf("DumpState = %q, want complete -- two duplicate end_of_rib "+
			"markers for the same session must still read as one converged "+
			"RIB, not as a dump still in progress", got[0].DumpState)
	}
}

// TestLSNodesFiltersOnAreaZero is the zero-value hazard, asserted directly.
// Area 0 is the backbone and the archive's most common area; a filter that
// spelled "not asked" as 0 would return every area for this query and pass
// any test that only checked for a non-empty answer.
func TestLSNodesFiltersOnAreaZero(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSNodeFixture(t, ctx, q)

	zero := uint32(0)
	got, err := q.LSNodes(ctx, LSNodeFilter{Area: &zero})
	if err != nil {
		t.Fatalf("area=0: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("area=0 matched nothing, but the fixture puts three nodes in it")
	}
	for _, n := range got {
		if n.Area != 0 {
			t.Errorf("area=0 returned a node in area %d -- the predicate was "+
				"dropped, and this answer is every area", n.Area)
		}
	}

	one := uint32(1)
	other, err := q.LSNodes(ctx, LSNodeFilter{Area: &one})
	if err != nil {
		t.Fatalf("area=1: %v", err)
	}
	// Exactly one, and this is the count the shared database constrains
	// most sharply: a single area-1 ls_nodes row written by any other
	// fixture fails this test from a different file. It is why
	// insertLSLinkFixture's area-crossing link has an area-1 endpoint with
	// no node row at all -- see lsFixLinkCross.
	if len(other) != 1 {
		t.Errorf("area=1 returned %d rows, want 1 -- area 0 and area 1 must be "+
			"different questions", len(other))
	}
}

// TestLSNodesFiltersByProtocol, both directions.
func TestLSNodesFiltersByProtocol(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSNodeFixture(t, ctx, q)

	isis, err := q.LSNodes(ctx, LSNodeFilter{Protocol: 2})
	if err != nil {
		t.Fatalf("protocol=2: %v", err)
	}
	if len(isis) == 0 {
		t.Fatal("protocol=2 matched nothing, but the fixture carries IS-IS nodes")
	}
	for _, n := range isis {
		if n.Protocol != 2 {
			t.Errorf("protocol=2 returned protocol %d", n.Protocol)
		}
	}
	none, err := q.LSNodes(ctx, LSNodeFilter{Protocol: 7})
	if err != nil {
		t.Fatalf("protocol=7: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("protocol=7 (BGP) matches no fixture row but returned %d nodes "+
			"-- the predicate never reached the statement", len(none))
	}
}

// TestCountLSNodesIgnoresLimit: the count is what total_matched reports, so
// it must describe the whole answer rather than the page.
func TestCountLSNodesIgnoresLimit(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSNodeFixture(t, ctx, q)

	all, err := q.LSNodes(ctx, LSNodeFilter{})
	if err != nil {
		t.Fatalf("LSNodes: %v", err)
	}
	page, err := q.LSNodes(ctx, LSNodeFilter{Limit: 1})
	if err != nil {
		t.Fatalf("LSNodes limited: %v", err)
	}
	total, err := q.CountLSNodes(ctx, LSNodeFilter{Limit: 1})
	if err != nil {
		t.Fatalf("CountLSNodes: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("Limit 1 returned %d rows", len(page))
	}
	if total != uint64(len(all)) {
		t.Errorf("CountLSNodes under Limit 1 reported %d, want %d -- a count "+
			"that honored the limit would report the page size and call a "+
			"truncated answer complete", total, len(all))
	}
}

// TestLSNodeFilterRejectsAnUnknownState. An unrecognized value must not fall
// through to the default: "liv" silently meaning "live" is how a typo
// becomes a wrong answer that looks right.
func TestLSNodeFilterRejectsAnUnknownState(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	_, err := q.LSNodes(ctx, LSNodeFilter{State: LSState("liv")})
	if err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("got %v, which does not wrap ErrBadFilter", err)
	}
}

// TestLSPrefixesRenderCIDR is the storage asymmetry made invisible.
// ls_prefixes holds a bare address and a separate length; the three route
// tables hold one string. A caller must not be able to tell.
func TestLSPrefixesRenderCIDR(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSPrefixFixture(t, ctx, q)

	got, err := q.LSPrefixes(ctx, LSPrefixFilter{})
	if err != nil {
		t.Fatalf("LSPrefixes: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no prefixes, so every assertion below is vacuous")
	}
	for _, p := range got {
		if !strings.Contains(p.Prefix, "/") {
			t.Errorf("prefix %q carries no length -- the wire form is CIDR, and "+
				"a bare address here means the two stored columns leaked apart",
				p.Prefix)
		}
	}
}

// TestLSPrefixesFilterByPrefix, both directions, in the notation a caller
// types: CIDR in, CIDR out.
func TestLSPrefixesFilterByPrefix(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSPrefixFixture(t, ctx, q)

	hit, err := q.LSPrefixes(ctx, LSPrefixFilter{Prefix: lsFixPrefixCIDR})
	if err != nil {
		t.Fatalf("prefix=%s: %v", lsFixPrefixCIDR, err)
	}
	if len(hit) == 0 {
		t.Fatalf("prefix=%s matched nothing, but the fixture carries it",
			lsFixPrefixCIDR)
	}
	for _, p := range hit {
		if p.Prefix != lsFixPrefixCIDR {
			t.Errorf("prefix=%s returned %s", lsFixPrefixCIDR, p.Prefix)
		}
	}
	miss, err := q.LSPrefixes(ctx, LSPrefixFilter{Prefix: "203.0.113.0/24"})
	if err != nil {
		t.Fatalf("non-matching prefix: %v", err)
	}
	if len(miss) != 0 {
		t.Errorf("a prefix no fixture row carries returned %d rows -- the "+
			"predicate never reached the statement", len(miss))
	}
}

// TestLSPrefixesFilterByAreaAndASN exists because area= and asn= on
// /v1/ls/prefixes used to be the SAME FILTER as far as this repo could tell.
// Every ls_prefixes row both fixtures wrote carried one area and one ASN, so
// transposing the two predicates in LSPrefixFilter.predicates -- rendering
// f.Area against p.asn and f.ASN against p.area -- left all eighteen
// packages green. ?area=0 would then answer "prefixes originated by AS 0"
// and ?asn=65001 "prefixes in area 65001": both plausible, both non-empty,
// no error, nothing red. The identical transposition on LSNodeFilter fails
// two tests and on LSLinkFilter one, because those fixtures already vary
// both columns; prefixes alone were blind.
//
// Two things make this test see it. The fixture's third row carries a
// different area AND a different AS from the other two (see
// lsFixPrefixOtherCIDR), so each filter has a strict subset to select. And
// every returned row is checked to CARRY the value that was asked for, not
// merely to exist -- a count alone would pass against a transposition that
// happened to return the same number of rows.
func TestLSPrefixesFilterByAreaAndASN(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSPrefixFixture(t, ctx, q)

	all, err := q.LSPrefixes(ctx, LSPrefixFilter{})
	if err != nil {
		t.Fatalf("unfiltered: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("the unfiltered answer has %d rows; every subset assertion "+
			"below would be vacuous", len(all))
	}

	area := lsFixArea
	otherArea := lsFixPrefixOtherArea
	asn := lsFixASN
	otherASN := lsFixPrefixOtherASN
	// 42 and 424242 are values no fixture prefix carries in either column,
	// so a miss here is a miss under the transposition too -- which is why
	// the match halves, not the miss halves, are what catch it.
	missArea := uint32(42)
	missASN := uint32(424242)

	for _, tc := range []struct {
		name   string
		filter LSPrefixFilter
		want   func(LSPrefix) uint32
		value  uint32
	}{
		{"area-zero", LSPrefixFilter{Area: &area},
			func(p LSPrefix) uint32 { return p.Area }, area},
		{"area-other", LSPrefixFilter{Area: &otherArea},
			func(p LSPrefix) uint32 { return p.Area }, otherArea},
		{"asn", LSPrefixFilter{ASN: &asn},
			func(p LSPrefix) uint32 { return p.ASN }, asn},
		{"asn-other", LSPrefixFilter{ASN: &otherASN},
			func(p LSPrefix) uint32 { return p.ASN }, otherASN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.LSPrefixes(ctx, tc.filter)
			if err != nil {
				t.Fatalf("%+v: %v", tc.filter, err)
			}
			if len(got) == 0 {
				t.Fatalf("%+v matched nothing, but the fixture carries rows "+
					"for it -- the likeliest cause is the predicate being "+
					"rendered against the OTHER column", tc.filter)
			}
			if len(got) >= len(all) {
				t.Errorf("%+v returned %d of %d rows -- it selected no strict "+
					"subset, so the predicate never reached the statement",
					tc.filter, len(got), len(all))
			}
			for _, p := range got {
				if tc.want(p) != tc.value {
					t.Errorf("%+v returned %s, which carries area %d / asn %d "+
						"-- the filter matched on a column it was not asked "+
						"about", tc.filter, p.Prefix, p.Area, p.ASN)
				}
			}
		})
	}

	for _, tc := range []struct {
		name   string
		filter LSPrefixFilter
	}{
		{"area-miss", LSPrefixFilter{Area: &missArea}},
		{"asn-miss", LSPrefixFilter{ASN: &missASN}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.LSPrefixes(ctx, tc.filter)
			if err != nil {
				t.Fatalf("%+v: %v", tc.filter, err)
			}
			if len(got) != 0 {
				t.Errorf("%+v matches no fixture row but returned %d -- the "+
					"filter never reached a predicate", tc.filter, len(got))
			}
		})
	}
}

// TestLSPrefixesCovers: containment over the concatenated CIDR expression.
// The address is inside the fixture's prefix and outside the other one, so a
// covers that silently matched everything fails the second half.
func TestLSPrefixesCovers(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSPrefixFixture(t, ctx, q)

	in, err := q.LSPrefixes(ctx, LSPrefixFilter{Covers: lsFixCoveredAddr})
	if err != nil {
		t.Fatalf("covers=%s: %v", lsFixCoveredAddr, err)
	}
	if len(in) == 0 {
		t.Fatalf("covers=%s matched nothing, but %s contains it",
			lsFixCoveredAddr, lsFixPrefixCIDR)
	}
	out, err := q.LSPrefixes(ctx, LSPrefixFilter{Covers: "203.0.113.9"})
	if err != nil {
		t.Fatalf("covers of an uncovered address: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("an address no fixture prefix contains returned %d rows", len(out))
	}
}

// TestLSPrefixFilterRejectsPrefixAndCoversTogether, the rule checkCovers
// already enforces for the route filters: an exact prefix that also contains
// an address is a question whose empty answer looks exactly like the prefix
// being absent.
func TestLSPrefixFilterRejectsPrefixAndCoversTogether(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	_, err := q.LSPrefixes(ctx, LSPrefixFilter{
		Prefix: lsFixPrefixCIDR, Covers: lsFixCoveredAddr,
	})
	if err == nil {
		t.Fatal("prefix and covers together were accepted")
	}
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("got %v, which does not wrap ErrBadFilter", err)
	}
}

// TestLSLinkLabelsPreferTheReportingObserver is the three-tier label rule
// (reporting observer, then fleet, then identifier), tier by tier. The
// fixture is built so each tier is the ONLY one that can answer for its
// link: without that, a chain that always fell through to the fleet tier
// would pass a test that only checked the label was non-empty.
//
// The fourth case is the one the other three cannot reach. lsFixLinkStale's
// endpoint was named by the reporting peer in a PREVIOUS session, so the
// observer tier -- which lsLinksSQL pins to cur.sid -- must not see it,
// while the fleet tier, keyed by node_key alone and spanning sessions, must.
// Dropping `session_id = cur.sid` from the label join makes that one row
// answer "observer" instead of "fleet", deterministically, which is what
// makes that predicate load-bearing rather than defense in depth: without
// this case the mutation is invisible, and the label a link carries would
// silently become whatever a router called that node during a session that
// has since ended.
func TestLSLinkLabelsPreferTheReportingObserver(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	got, err := q.LSLinks(ctx, LSLinkFilter{})
	if err != nil {
		t.Fatalf("LSLinks: %v", err)
	}
	byRemote := map[string]LSLink{}
	for _, l := range got {
		byRemote[l.Remote.RouterID] = l
	}
	for _, tc := range []struct {
		routerID  string
		wantLabel string
		wantTier  string
	}{
		// The reporting observer named this node itself. The other observer
		// named it too, differently and later, so a fleet-first chain would
		// answer lsFixNearElse here as well as reporting the wrong tier.
		{lsFixLinkNear, lsFixNearLabel, "observer"},
		// Only the other observer named it, so the fleet tier answers.
		{lsFixLinkFar, lsFixFarLabel, "fleet"},
		// Nobody has a name or a router_id_v4 for it.
		{lsFixLinkAnon, lsFixLinkAnon, "identifier"},
		// The reporting observer named it, but in a session that is over.
		{lsFixLinkStale, lsFixStaleLabel, "fleet"},
		// Named by no NAME at all: its label is a router_id_v4, which is the
		// second branch of each tier and the one no other endpoint reaches.
		// The other observer reports a different quad for the same node, so
		// deleting the observer's r4 branch from lsLabelExpr alone -- and
		// leaving lsLabelSourceExpr saying "observer" -- changes the string
		// here rather than falling through to an identical one.
		{lsFixLinkDotted, lsFixDottedV4, "observer"},
		// The same, one tier down: no observer NAMES this node and the
		// reporting one has never seen it, so the fleet tier's own r4 branch
		// is the only thing that can answer. Deleting that branch from
		// lsLabelExpr drops the label to the raw identifier; deleting its
		// clause from lsLabelSourceExpr alone leaves the quad reported as
		// having come from the identifier tier.
		{lsFixLinkFleetDotted, lsFixFleetDottedV4, "fleet"},
	} {
		l, ok := byRemote[tc.routerID]
		if !ok {
			t.Errorf("no link with remote router_id %q; the fixture writes one",
				tc.routerID)
			continue
		}
		if l.Remote.Label != tc.wantLabel {
			t.Errorf("remote %s labeled %q, want %q", tc.routerID,
				l.Remote.Label, tc.wantLabel)
		}
		if l.Remote.LabelSource != tc.wantTier {
			t.Errorf("remote %s labeled from %q, want %q -- the tier a label "+
				"came from is part of the answer, not an implementation detail",
				tc.routerID, l.Remote.LabelSource, tc.wantTier)
		}
	}

	// The same rule at the LOCAL end. The two ends are resolved by two
	// separately-spliced copies of the same expression against two
	// separately-written joins, so a chain correct for one is no evidence
	// about the other -- and the near end is the one every link in the
	// fixture shares, which makes it the easy one to leave untested.
	byLocal := map[string]LSLink{}
	for _, l := range got {
		byLocal[l.Local.RouterID] = l
	}
	for _, tc := range []struct {
		routerID  string
		wantLabel string
		wantTier  string
	}{
		{lsFixLinkLocal, lsFixLocalLabel, "observer"},
		// Named by the reporting observer, but in a session that is over:
		// the local label join pins session_id to cur.sid, so this must
		// reach the fleet tier exactly as its remote-end mirror does.
		{lsFixLinkStaleLocal, lsFixStaleLocalLabel, "fleet"},
	} {
		l, ok := byLocal[tc.routerID]
		if !ok {
			t.Errorf("no link with local router_id %q; the fixture writes one",
				tc.routerID)
			continue
		}
		if l.Local.Label != tc.wantLabel || l.Local.LabelSource != tc.wantTier {
			t.Errorf("local %s labeled %q from %q, want %q from %q",
				tc.routerID, l.Local.Label, l.Local.LabelSource,
				tc.wantLabel, tc.wantTier)
		}
	}
}

// TestLSLinkFleetLabelSurvivesNodeHistoryRetention is the fleet tier's own
// retention test, the counterpart of TestLSLinkLabelsPreferTheReportingObserver's
// fourth case: that case holds that a session ending is not enough to make
// the OBSERVER tier answer for a name it should not; this one holds that
// ls_nodes' own 90-day TTL is not enough to make the FLEET tier stop
// answering for a name it should.
//
// insertLSFleetLabelRetentionFixture writes node N named by one observer and
// a live link to N reported by a second, unrelated observer that names
// nothing -- the ANON shape TestLSLinkLabelsPreferTheReportingObserver
// already covers, except here the fixture's only candidate name for N is
// the namer's one row, which optimizeLSFleetRetentionPartition then ages out
// of ls_nodes with a real OPTIMIZE ... FINAL, exactly as retention
// eventually would in production.
//
// If ls_fleet_labels reads ls_nodes (history), that OPTIMIZE deletes its
// only candidate for node_key and the link's remote label decays to the raw
// router_id -- confirmed RED: this test fails against that source, with the
// remote label reporting the raw router_id from tier "identifier" instead of
// the name from tier "fleet". If it reads ls_nodes_current, the named row
// survives the OPTIMIZE (no TTL on that table) and the label holds -- GREEN.
func TestLSLinkFleetLabelSurvivesNodeHistoryRetention(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSFleetLabelRetentionFixture(t, ctx, q)
	optimizeLSFleetRetentionPartition(t, ctx, q)

	got, err := q.LSLinks(ctx, LSLinkFilter{
		Router: netip.MustParseAddr(lsFleetRetentionRouterIP),
		Peer:   netip.MustParseAddr(lsFleetRetentionPeerIP),
	})
	if err != nil {
		t.Fatalf("LSLinks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d links scoped to the fixture's own router and peer, want 1",
			len(got))
	}
	l := got[0]
	if l.Remote.RouterID != lsFleetRetentionNodeRouterID {
		t.Fatalf("fixture: remote router_id = %q, want %q",
			l.Remote.RouterID, lsFleetRetentionNodeRouterID)
	}
	if l.Remote.Label != lsFleetRetentionName {
		t.Errorf("remote label = %q from tier %q, want %q from the fleet tier -- "+
			"a name whose only history row aged past retention must still resolve "+
			"from the current table", l.Remote.Label, l.Remote.LabelSource, lsFleetRetentionName)
	}
	if l.Remote.LabelSource != "fleet" {
		t.Errorf("remote label source = %q, want %q", l.Remote.LabelSource, "fleet")
	}
}

// TestLSLinkLabelIsNeverEmpty. The chain ends in the raw identifier
// precisely so a caller never has to handle an absent label, and an empty
// string here would put the burden back.
//
// Both ends are checked. The local end resolves through the observer tier
// for every fixture link and the remote ends spread across all three, so a
// chain broken for one end alone -- the two are separate expressions, and
// the aliases they are spliced with are the only thing keeping them apart --
// is caught here rather than left to the tier test's remote-only table.
func TestLSLinkLabelIsNeverEmpty(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	got, err := q.LSLinks(ctx, LSLinkFilter{})
	if err != nil {
		t.Fatalf("LSLinks: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no links, so this assertion is vacuous")
	}
	for _, l := range got {
		if l.Local.Label == "" || l.Remote.Label == "" {
			t.Errorf("a link came back with an empty endpoint label: %+v", l)
		}
		if l.Local.LabelSource == "" || l.Remote.LabelSource == "" {
			t.Errorf("a link came back with a label but no tier to attribute "+
				"it to: %+v", l)
		}
	}
	// Every tier answers for at least one end somewhere in the answer, so
	// the loop above is not passing against a statement whose chain
	// collapsed to a single branch -- an identifier-only chain labels every
	// end non-empty and would satisfy it exactly.
	tiers := map[string]int{}
	for _, l := range got {
		tiers[l.Local.LabelSource]++
		tiers[l.Remote.LabelSource]++
	}
	for _, tier := range []string{"observer", "fleet", "identifier"} {
		if tiers[tier] == 0 {
			t.Errorf("no endpoint in the answer was labeled from the %q tier "+
				"(saw %v) -- the fixture arranges all three, so a missing one "+
				"means the chain collapsed to the branches that remain", tier, tiers)
		}
	}
}

// TestLSLinksAreaMatchesEitherEnd is the either-end area rule.
// ls_links has no plain area column, and a link that CROSSES between areas
// is the case the rule exists for: it must be returned by a query for
// either of its two areas, not neither.
func TestLSLinksAreaMatchesEitherEnd(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	for _, area := range []uint32{0, 1} {
		a := area
		got, err := q.LSLinks(ctx, LSLinkFilter{Area: &a})
		if err != nil {
			t.Fatalf("area=%d: %v", area, err)
		}
		found := false
		for _, l := range got {
			if l.Local.Area != l.Remote.Area {
				found = true
			}
		}
		if !found {
			t.Errorf("area=%d returned no area-crossing link; the fixture writes "+
				"one between area 0 and area 1, and either-end matching means "+
				"both queries must return it", area)
		}
	}

	// The other direction, and it is what keeps the OR from being a
	// tautology: area 1 touches exactly the crossing link, so an either-end
	// predicate that had degenerated into "any area" would return the four
	// area-0 links here too.
	one := uint32(1)
	only, err := q.LSLinks(ctx, LSLinkFilter{Area: &one})
	if err != nil {
		t.Fatalf("area=1: %v", err)
	}
	if len(only) != 1 {
		t.Errorf("area=1 returned %d links, want exactly the one that crosses "+
			"into it", len(only))
	}

	nine := uint32(9)
	none, err := q.LSLinks(ctx, LSLinkFilter{Area: &nine})
	if err != nil {
		t.Fatalf("area=9: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("area=9 matches no fixture link but returned %d -- either-end "+
			"matching must still be matching", len(none))
	}
}

// TestLSLinksAreaLeavesTheOtherFiltersStanding is orEq's parenthesization,
// asserted through the statement rather than through the rendered string.
// where() joins its conditions with AND, so an unparenthesized
// `a = ? OR b = ?` binds as `... AND a = ? OR b = ?` and turns every other
// predicate into a suggestion: this asks for area 0 on a peer that reports
// no links at all, and an answer to it is the OR having swallowed the peer.
func TestLSLinksAreaLeavesTheOtherFiltersStanding(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	zero := uint32(0)
	got, err := q.LSLinks(ctx, LSLinkFilter{
		Area: &zero, Peer: netip.MustParseAddr(lsFixPeerB),
	})
	if err != nil {
		t.Fatalf("area=0 peer=B: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("area=0 on a peer that reports no links returned %d links -- "+
			"the area OR is unparenthesized and every other filter in the "+
			"query has become optional", len(got))
	}
	// And the same question of the peer that does report them, so the case
	// above is not passing because the whole filter matches nothing.
	hit, err := q.LSLinks(ctx, LSLinkFilter{
		Area: &zero, Peer: netip.MustParseAddr(lsFixPeerA),
	})
	if err != nil {
		t.Fatalf("area=0 peer=A: %v", err)
	}
	if len(hit) == 0 {
		t.Error("area=0 on the reporting peer returned nothing, so the case " +
			"above proves only that the fixture is empty")
	}
}

// TestLSLinksExcludeWithdrawnByDefault, and its other half: state=withdrawn
// returns exactly the link the default hides, and state=any returns both
// halves. Asserting only the first would pass against a statement that
// returned nothing at all.
func TestLSLinksExcludeWithdrawnByDefault(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	live, err := q.LSLinks(ctx, LSLinkFilter{})
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	if len(live) == 0 {
		t.Fatal("the live answer is empty, so the assertion below is vacuous")
	}
	for _, l := range live {
		if l.Remote.RouterID == lsFixLinkGone {
			t.Errorf("a withdrawn link was returned by the default filter: %+v", l)
		}
	}

	gone, err := q.LSLinks(ctx, LSLinkFilter{State: LSStateWithdrawn})
	if err != nil {
		t.Fatalf("withdrawn: %v", err)
	}
	if len(gone) != 1 || gone[0].Remote.RouterID != lsFixLinkGone {
		t.Errorf("state=withdrawn returned %d links (%+v), want exactly the "+
			"withdrawn one", len(gone), gone)
	}
	if len(gone) == 1 && !gone[0].IsWithdraw {
		t.Error("the withdrawn link came back with IsWithdraw false -- a " +
			"caller reading state=any must not have to infer an object's " +
			"state from the request it made")
	}

	all, err := q.LSLinks(ctx, LSLinkFilter{State: LSStateAny})
	if err != nil {
		t.Fatalf("any: %v", err)
	}
	if len(all) != len(live)+len(gone) {
		t.Errorf("state=any returned %d links, want %d (%d live + %d withdrawn) "+
			"-- the three states must partition the answer",
			len(all), len(live)+len(gone), len(live), len(gone))
	}
}

// TestLabelCTEsResolveByArgMax closes the one hole the behavioral guard
// cannot. TestLSLinkLabelsPreferTheReportingObserver catches any() in either
// label CTE only when any() happens to return the SUPERSEDED row, which it
// does because stream_seq orders the table that way -- a property of how
// ClickHouse reads these parts today, not one it promises. That guard is
// therefore false-negative-only: it can go quiet, it cannot go wrong. This
// one is structural and can do neither.
//
// Four argMax calls, two per CTE, and no any() anywhere in the CTE text. An
// arbitrary winner would make one request answer differently between calls,
// which lsNodeLabelsCTE's own doc comment argues at length is a worse
// failure than hex: a name that changes under a reader who did not change
// the question is a reason to distrust every column beside it.
//
// It needs no ClickHouse, which is the point -- it holds on a machine where
// the behavioral guard is skipped entirely.
func TestLabelCTEsResolveByArgMax(t *testing.T) {
	if n := strings.Count(lsNodeLabelsCTE, "argMax("); n != 4 {
		t.Errorf("lsNodeLabelsCTE has %d argMax calls, want 4 -- two label "+
			"columns in each of the two CTEs, every one of them resolved by "+
			"(seq, stream_seq) rather than by whichever row was read first", n)
	}
	if strings.Contains(lsNodeLabelsCTE, "any(") {
		t.Error("lsNodeLabelsCTE contains any(): an arbitrary winner among a " +
			"node's names makes one request answer differently between calls, " +
			"and the behavioral guard on this only fires when any() happens " +
			"to pick the superseded row")
	}
}

// TestLSLabelCTEsAreOneRowPerJoinKey asserts the property four doc comments
// used to cite a test for that could not see it: each label CTE emits at
// most one row per the key lsLinksSQL joins it on.
//
// It is what keeps the four LEFT JOINs from fanning one link into several
// rows, and the consequence of losing it is not a wrong row COUNT -- the
// GROUP BY absorbs that -- but a wrong LABEL: with several candidate rows in
// a group, the any() over each label expression picks among them
// arbitrarily, which is the "one request answers differently between calls"
// failure lsNodeLabelsCTE refuses any() for in the first place.
//
// It runs the CTE text itself rather than a copy, so a duplicating edit
// inside either CTE is what the assertion sees. The query is the direct
// question -- group the CTE's own output by the join key and look for a key
// with more than one row -- and it is checked for having any rows at all
// first, because "no duplicate keys" is trivially true of an empty CTE.
func TestLSLabelCTEsAreOneRowPerJoinKey(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	// lsNodeLabelsCTE is written as a continuation of a WITH list, so it
	// leads with a comma; dropping that makes it a WITH list of its own.
	with := fmt.Sprintf("WITH"+strings.TrimPrefix(lsNodeLabelsCTE, ","), testDB)

	for _, tc := range []struct{ cte, key string }{
		// Exactly the six columns lsLinksSQL binds on lloc and lrem.
		{"ls_node_labels", "collector_id, router_ip, peer_ip, rib, session_id, node_key"},
		// And the one it binds on floc and frem.
		{"ls_fleet_labels", "node_key"},
	} {
		t.Run(tc.cte, func(t *testing.T) {
			var rows uint64
			if err := q.conn.QueryRow(ctx,
				with+"\nSELECT count() FROM "+tc.cte).Scan(&rows); err != nil {
				t.Fatalf("count %s: %v", tc.cte, err)
			}
			if rows == 0 {
				t.Fatalf("%s is empty, so a duplicate-key check over it "+
					"asserts nothing", tc.cte)
			}
			var dupes uint64
			if err := q.conn.QueryRow(ctx, with+
				"\nSELECT count() FROM (SELECT count() FROM "+tc.cte+
				" GROUP BY "+tc.key+" HAVING count() > 1)").Scan(&dupes); err != nil {
				t.Fatalf("duplicate keys in %s: %v", tc.cte, err)
			}
			if dupes != 0 {
				t.Errorf("%d keys in %s carry more than one row, over %d rows "+
					"total -- lsLinksSQL LEFT JOINs this CTE twice on exactly "+
					"(%s), so every extra row per key multiplies the rows in a "+
					"link's group and leaves the any() over each label picking "+
					"among them arbitrarily", dupes, tc.cte, rows, tc.key)
			}
		})
	}
}

// TestCountLSLinksMatchesTheAnswerAndIgnoresLimit holds two things, and it
// is worth being exact about which, because this test was originally written
// -- and cited in three doc comments -- as the guard on a third thing it
// cannot see.
//
// It holds that CountLSLinks and LSLinks agree, so the two entry points
// cannot drift apart, and that the count ignores Limit, which is the
// property meta.total_matched needs.
//
// It does NOT hold that the label joins are one row per key. Both sides of
// the comparison read the GROUP BY's output -- that is exactly what makes
// the count correct, see CountLSLinks' own doc comment -- so a label CTE
// emitting two rows per key inflates the joined row count and leaves both
// sides of this comparison untouched. Duplicating every ls_node_labels row,
// with the duplicates carrying different labels, left this test green. The
// property is real and is now asserted directly, by
// TestLSLabelCTEsAreOneRowPerJoinKey.
func TestCountLSLinksMatchesTheAnswerAndIgnoresLimit(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	rows, err := q.LSLinks(ctx, LSLinkFilter{})
	if err != nil {
		t.Fatalf("LSLinks: %v", err)
	}
	total, err := q.CountLSLinks(ctx, LSLinkFilter{})
	if err != nil {
		t.Fatalf("CountLSLinks: %v", err)
	}
	if total != uint64(len(rows)) {
		t.Errorf("CountLSLinks reported %d against %d rows returned -- the "+
			"count and the answer are built from one statement and must not "+
			"disagree", total, len(rows))
	}

	// And the count ignores Limit, the property meta.total_matched needs:
	// a count that honored the page cap would call a truncated answer
	// complete.
	page, err := q.LSLinks(ctx, LSLinkFilter{Limit: 1})
	if err != nil {
		t.Fatalf("LSLinks limited: %v", err)
	}
	capped, err := q.CountLSLinks(ctx, LSLinkFilter{Limit: 1})
	if err != nil {
		t.Fatalf("CountLSLinks limited: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("Limit 1 returned %d links", len(page))
	}
	if capped != uint64(len(rows)) {
		t.Errorf("CountLSLinks under Limit 1 reported %d, want %d", capped, len(rows))
	}
}

// TestLSLinkFilterRejectsAnUnknownState. LSLinkFilter validates in
// predicates() and has no separate check() to skip, which is the whole
// reason that signature returns an error -- see LSState.predicate.
func TestLSLinkFilterRejectsAnUnknownState(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	_, err := q.LSLinks(ctx, LSLinkFilter{State: LSState("liv")})
	if err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if !errors.Is(err, ErrBadFilter) {
		t.Errorf("got %v, which does not wrap ErrBadFilter", err)
	}
	if _, err := q.CountLSLinks(ctx, LSLinkFilter{State: LSState("liv")}); err == nil {
		t.Fatal("CountLSLinks accepted an unknown state; both entry points " +
			"validate, because both build the statement")
	}
}

// TestLSLinksReportBothEndsInOrder is the positional hazard scanLSLinks' own
// doc comment names, asserted on a link whose every column is a different
// value. This statement returns two endpoints of IDENTICAL SHAPE back to
// back -- ASN, BGP-LS id, area, router_id, node_key, interface address,
// interface id, label, label source, twice -- so a column reordered in
// lsLinksSQL and not here does not fail: it binds the remote end's fields to
// the local end's and reports an adjacency running the other way, in an
// answer that still type-checks and still looks like a topology.
//
// The same applies to the metric columns, which are three UInt32 neighbors.
// The fixture gives te_metric 10 and igp_metric 20 rather than one value
// twice for exactly this assertion.
func TestLSLinksReportBothEndsInOrder(t *testing.T) {
	ctx := t.Context()
	q := requireQuery(t, ctx)
	insertLSLinkFixture(t, ctx, q)

	got, err := q.LSLinks(ctx, LSLinkFilter{})
	if err != nil {
		t.Fatalf("LSLinks: %v", err)
	}
	var l LSLink
	found := false
	for _, c := range got {
		if c.Local.RouterID == lsFixLinkLocal && c.Remote.RouterID == lsFixLinkNear {
			l, found = c, true
		}
	}
	if !found {
		t.Fatalf("the fixture's near link is not in the answer: %+v", got)
	}

	for _, tc := range []struct {
		what      string
		got, want any
	}{
		{"router sysname", l.RouterSysName, lsFixSysName},
		{"rib", l.RIB, "in_pre"},
		{"collector", l.Collector, lsFixCollector},
		{"protocol", l.Protocol, lsFixProtocol},
		{"identifier", l.Identifier, uint64(0)},
		{"local asn", l.Local.ASN, lsFixASN},
		{"local area", l.Local.Area, lsFixArea},
		{"local ifaddr", l.Local.IfAddr, "10.1.1.1"},
		{"local interface id", l.Local.InterfaceID, uint32(1)},
		{"remote asn", l.Remote.ASN, lsFixASN},
		{"remote area", l.Remote.Area, lsFixArea},
		{"remote ifaddr", l.Remote.IfAddr, "10.1.1.2"},
		{"remote interface id", l.Remote.InterfaceID, uint32(2)},
		{"te metric", l.TEMetric, uint32(10)},
		{"igp metric", l.IGPMetric, uint32(20)},
		{"admin group", l.AdminGroup, uint32(0)},
		{"max bandwidth", l.MaxBandwidth, float32(1e9)},
		{"is withdraw", l.IsWithdraw, false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v -- Scan is positional, and neighbors of "+
				"one type swap silently", tc.what, tc.got, tc.want)
		}
	}
	if len(l.AdjSIDs) != 1 || l.AdjSIDs[0] != 24001 {
		t.Errorf("adjacency SIDs = %v, want [24001]", l.AdjSIDs)
	}
	// The two node keys are ls_nodes' own node_key over the same six
	// identity columns, which is what makes LSLink.Local.NodeKey a value a
	// caller can hand back through LSLinkFilter.LocalNode. They must differ
	// from each other, or the two ends have been read from one.
	if l.Local.NodeKey == 0 || l.Remote.NodeKey == 0 || l.Local.NodeKey == l.Remote.NodeKey {
		t.Errorf("node keys local=%d remote=%d; a link between two distinct "+
			"nodes has two distinct keys, neither of them zero",
			l.Local.NodeKey, l.Remote.NodeKey)
	}
	// And the key really addresses that end: filtering by it returns the
	// link, which is the round trip a caller paging on an endpoint makes.
	back, err := q.LSLinks(ctx, LSLinkFilter{RemoteNode: &l.Remote.NodeKey})
	if err != nil {
		t.Fatalf("remote_node=%d: %v", l.Remote.NodeKey, err)
	}
	if len(back) == 0 {
		t.Error("filtering on the remote node key returned nothing")
	}
	for _, b := range back {
		if b.Remote.NodeKey != l.Remote.NodeKey {
			t.Errorf("remote_node filter returned a link to %d", b.Remote.NodeKey)
		}
	}
	// LocalNode and RemoteNode are deliberately NOT either-end: asking for
	// this node as a LOCAL end must not return the links that reach it as a
	// remote one.
	none, err := q.LSLinks(ctx, LSLinkFilter{LocalNode: &l.Remote.NodeKey})
	if err != nil {
		t.Fatalf("local_node=%d: %v", l.Remote.NodeKey, err)
	}
	if len(none) != 0 {
		t.Errorf("local_node= on a node that is only ever a remote end "+
			"returned %d links -- the two ends are separate questions", len(none))
	}
}

// TestLinkStateOrderingsAreTotal makes one defect unshippable: an ORDER BY
// that does not cover its statement's own GROUP BY key.
//
// A partial order is not a cosmetic complaint. Rows tied on every ORDER BY
// column come back in whatever order the merge happened to produce, so once
// Limit is set -- and every handler sets it from the operator's page cap --
// a page boundary landing inside a tie silently drops one row and repeats
// another, with nothing in the answer saying so. lsNodesOrder's own doc
// comment records the archive measurement: 11 tied pairs covering 22 of 230
// rows on the presentational half alone, and the rows that moved between two
// runs at different block sizes were exactly those pairs.
//
// It reads the statements rather than the database because the property is
// structural: the ordering is total if and only if every GROUP BY column
// appears in the ORDER BY, and no fixture can demonstrate that -- a tie has
// to exist in the data before a partial order can be caught by querying, and
// the fixture that has one today is not the fixture that will have one after
// the next column is added.
//
// lsLinksSQL is the widest of the three, at twenty columns, and it is the
// one whose presentational half covers least of its key: two parallel links
// between the same pair of named nodes agree on sysname, both addresses and
// both labels, and differ only in their interface addresses and link IDs.
func TestLinkStateOrderingsAreTotal(t *testing.T) {
	// clause returns the text of one clause of a statement, delimited at
	// both ends by markers rather than by indentation. An earlier version
	// took the marker's line and every line after it that began with a
	// space, and that made the test's own coverage depend on formatting: a
	// review dedented lsLinksSQL's GROUP BY continuation lines -- a pure
	// reformat, changing no SQL -- and the parsed key silently shrank from
	// twenty columns to six while the test went on passing. A guard that can
	// quietly shrink what it checks is worse than no guard, because it reads
	// as coverage.
	clause := func(t *testing.T, stmt, from, to string) string {
		t.Helper()
		_, after, ok := strings.Cut(stmt, from)
		if !ok {
			t.Fatalf("no %q in the statement; this test locates the clause by "+
				"that text, so a reformatting has to update it here too", from)
		}
		if to == "" {
			return after
		}
		text, _, ok := strings.Cut(after, to)
		if !ok {
			t.Fatalf("no %q after %q; the clause has no end this test can "+
				"find, and a partial parse would check a fraction of the key "+
				"and call it total", to, from)
		}
		return text
	}
	columns := func(text string) []string {
		var out []string
		for c := range strings.SplitSeq(text, ",") {
			if c = strings.TrimSpace(strings.ReplaceAll(c, "\n", " ")); c != "" {
				out = append(out, c)
			}
		}
		return out
	}

	for _, tc := range []struct {
		name, stmt, order string
		// key is how many columns the statement's GROUP BY has, written out
		// so the parse cannot quietly return fewer. A statement that grows a
		// GROUP BY column fails here until someone updates this number, and
		// updating it means looking at the ORDER BY beside it -- which is
		// the whole point.
		key int
	}{
		{"lsNodes", lsNodesSQL, lsNodesOrder, 11},
		{"lsPrefixes", lsPrefixesSQL, lsPrefixesOrder, 13},
		{"lsLinks", lsLinksSQL, lsLinksOrder, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The GROUP BY runs to the statement's havingClause verb, which
			// is the last thing in all three; the ORDER BY runs to the end
			// of its own const.
			group := columns(clause(t, tc.stmt, "\nGROUP BY ", "\n%[3]s"))
			if len(group) != tc.key {
				t.Fatalf("parsed %d GROUP BY columns (%v), want %d -- either "+
					"the key changed, in which case check the ORDER BY below "+
					"and update this number, or the parse is no longer "+
					"reading the whole clause and this test has stopped "+
					"checking most of it", len(group), group, tc.key)
			}
			ordered := map[string]bool{}
			for _, c := range columns(clause(t, tc.order, "ORDER BY ", "")) {
				ordered[c] = true
			}
			for _, c := range group {
				if !ordered[c] {
					t.Errorf("%s is in the GROUP BY key and not in the ORDER BY: "+
						"two rows differing only in it are tied, and under a "+
						"Limit a page boundary inside that tie drops one row and "+
						"repeats the other", c)
				}
			}
		})
	}
}
