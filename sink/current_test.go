package sink

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	vantagev1 "github.com/jp2195/vantage/schema/vantage/v1"
)

// currentFixtureRouter is a TEST-NET-1 address no other test in this package
// writes, so a count filtered on it sees only this file's rows even though
// testDB is shared by every live test in the package.
const currentFixtureRouter = "192.0.2.201"

// oneEnvelopeOfEachKind returns one envelope per history table a current
// table is fed from: unicast, VPN and EVPN routes, a BGP End-of-RIB, an LS
// node, link and prefix, an LS End-of-RIB, an LS raw remainder and a Peer
// Up. All of them carry currentFixtureRouter and sessionID, so the
// assertions can find them.
func oneEnvelopeOfEachKind(sessionID uint64) []*vantagev1.Envelope {
	ts := time.Now().UTC()
	env := func(payload any) *vantagev1.Envelope {
		e := testEnv(payload)
		e.CollectorId = "current-test"
		e.Router = &vantagev1.RouterId{Ip: currentFixtureRouter, SysName: "current-rtr"}
		e.SessionId = sessionID
		e.TsRouter = timestamppb.New(ts)
		e.TsCollector = timestamppb.New(ts)
		return e
	}
	lsFamily := &vantagev1.Family{Afi: 16388, Safi: 71}
	local := &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 1}}
	remote := &vantagev1.LsNodeDescriptor{Asn: 65000, RouterId: []byte{10, 255, 0, 2}}
	return []*vantagev1.Envelope{
		env(&vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP}),
		env(&vantagev1.RouteEvent{
			Family:    &vantagev1.Family{Afi: 1, Safi: 1},
			Announced: []*vantagev1.Prefix{{Prefix: "198.51.100.0/24"}},
		}),
		env(&vantagev1.RouteEvent{
			Family:       &vantagev1.Family{Afi: 1, Safi: 128},
			VpnAnnounced: []*vantagev1.VpnPrefix{{Prefix: "10.9.9.0/24", Rd: "65000:1"}},
		}),
		env(&vantagev1.RouteEvent{
			Family: &vantagev1.Family{Afi: 25, Safi: 70},
			EvpnAnnounced: []*vantagev1.EvpnRoute{{
				RouteType: 2, Rd: "10.255.1.2:32777",
				Mac: "00:50:79:66:68:01", Ip: "192.168.10.11",
			}},
		}),
		env(&vantagev1.RouteEvent{Family: &vantagev1.Family{Afi: 1, Safi: 1}, EndOfRib: true}),
		env(&vantagev1.LsEvent{Family: lsFamily, Nodes: []*vantagev1.LsNode{{
			Protocol: 3, Identifier: 100, Name: "r1", Local: local,
		}}}),
		env(&vantagev1.LsEvent{Family: lsFamily, Links: []*vantagev1.LsLink{{
			Protocol: 3, Identifier: 100, Local: local, Remote: remote,
			LocalIfaddr: []byte{10, 1, 0, 1}, RemoteIfaddr: []byte{10, 1, 0, 2},
		}}}),
		env(&vantagev1.LsEvent{Family: lsFamily, Prefixes: []*vantagev1.LsPrefix{{
			Protocol: 3, Identifier: 100, Local: local,
			Prefix: []byte{10, 255, 0, 1}, PrefixLen: 32,
		}}}),
		env(&vantagev1.LsEvent{Family: lsFamily, EndOfRib: true}),
		// An ls_events row that is NOT a marker: an undecoded remainder.
		// eor_current_ls_mv must skip it, and without it in the fixture a
		// view that dropped its end_of_rib filter would pass unnoticed. It
		// comes from another peer so that, filed wrongly, it lands on its own
		// eor_current key rather than merging into the real marker's row.
		func() *vantagev1.Envelope {
			e := env(&vantagev1.LsEvent{Family: lsFamily, RawReach: []byte{1, 2, 3, 4}})
			e.Peer = &vantagev1.PeerId{Ip: "10.255.0.9", Asn: 65000, BgpId: "10.255.0.9"}
			return e
		}(),
	}
}

// Every current table is fed by a materialized view on its history table,
// and nothing else writes to it. This drives one row into each history table
// through the writer's real path (RowsFor, then Insert) and asserts that each
// current table received it. A view that is missing, points at the wrong
// table or filters too much leaves its table empty, and the table names the
// view that did not fire.
func TestCurrentTablesArePopulatedByTheViews(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := requireClickHouse(t, ctx)
	defer c.Close()

	sessionID := uint64(time.Now().UnixNano())
	for i, env := range oneEnvelopeOfEachKind(sessionID) {
		rows, err := RowsFor(env, uint64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Insert(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}

	count := func(t *testing.T, query string) uint64 {
		t.Helper()
		var n uint64
		if err := c.conn.QueryRow(ctx, qualify(c, query), currentFixtureRouter, sessionID).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	for _, table := range []string{
		"peer_current", "eor_current", "route_unicast_current", "route_vpn_current",
		"route_evpn_current", "ls_nodes_current", "ls_links_current", "ls_prefixes_current",
	} {
		q := fmt.Sprintf("SELECT count() FROM vantage.%s WHERE router_ip = toIPv6(?) AND session_id = ?", table)
		if n := count(t, q); n == 0 {
			t.Errorf("%s holds nothing for the fixture router: its view is not firing", table)
		}
	}

	// eor_current takes two feeds. The BGP marker arrives as family 'ipv4u'
	// from eor_events, and the LS marker as family 'ls' from ls_events rows
	// with end_of_rib = 1. The count above passes on either one alone.
	if n := count(t, `SELECT count() FROM vantage.eor_current
		WHERE router_ip = toIPv6(?) AND session_id = ? AND family = 'ls'`); n == 0 {
		t.Error("the LS End-of-RIB did not reach eor_current as family 'ls'")
	}
	if n := count(t, `SELECT count() FROM vantage.eor_current
		WHERE router_ip = toIPv6(?) AND session_id = ? AND family = 'ipv4u'`); n == 0 {
		t.Error("the BGP End-of-RIB did not reach eor_current as family 'ipv4u'")
	}
	// The LS feed must take only markers. ls_events also holds raw_reach
	// remainders, which are not End-of-RIB and must not read as one.
	if n := count(t, `SELECT count() FROM vantage.eor_current
		WHERE router_ip = toIPv6(?) AND session_id = ?`); n != 2 {
		t.Errorf("eor_current holds %d rows for the fixture session, want 2 (one BGP, one LS)", n)
	}

	// A SELECT * view does not carry MATERIALIZED columns: the current table
	// recomputes node_key and the link endpoint keys itself. They must equal
	// the history table's, because node_key is the topology join key and a
	// key computed two ways joins nothing.
	for _, k := range []struct{ table, col string }{
		{"ls_nodes", "node_key"},
		{"ls_prefixes", "node_key"},
		{"ls_links", "local_node_key"},
		{"ls_links", "remote_node_key"},
	} {
		q := fmt.Sprintf(`SELECT count() FROM vantage.%[1]s_current
			WHERE router_ip = toIPv6(?) AND session_id = ? AND %[2]s != 0
			  AND %[2]s IN (SELECT %[2]s FROM vantage.%[1]s WHERE session_id = %[3]d)`,
			k.table, k.col, sessionID)
		if n := count(t, q); n != 1 {
			t.Errorf("%s_current.%s: %d rows carry a nonzero key equal to the history "+
				"table's, want 1", k.table, k.col, n)
		}
	}
}

// Each current table must keep one row per object, and an object is what the
// history table's sort key says it is. Every column of that key is identity
// except three, left out of the current key on purpose so that newest-wins
// collapses an object's versions into one row: ts_router and stream_seq say
// when a version arrived, and is_withdraw says what it did (a withdrawal
// replaces the announcement it withdraws). A current key missing any other
// column merges distinct objects -- two add-path paths, two RDs, two link
// ends -- into one row, and the loser disappears without an error. The
// current key must also carry collector_id and session_id, which the history
// key does not: see TestCurrentStateIgnoresASupersededSessionThatKeepsPublishing.
func TestCurrentTableKeysKeepEveryHistoryIdentityColumn(t *testing.T) {
	notIdentity := map[string]bool{"ts_router": true, "stream_seq": true, "is_withdraw": true}
	for _, table := range []string{
		"route_unicast", "route_vpn", "route_evpn", "ls_nodes", "ls_links", "ls_prefixes",
	} {
		current := map[string]bool{}
		for _, c := range sortKeyFromSchema(t, table+"_current") {
			current[c] = true
		}
		for _, c := range []string{"collector_id", "session_id"} {
			if !current[c] {
				t.Errorf("%s_current's ORDER BY lacks %s", table, c)
			}
		}
		history := sortKeyFromSchema(t, table)
		// The history keys all end in is_withdraw; one that did not parse
		// that far would check a prefix of the key and pass on less.
		if history[len(history)-1] != "is_withdraw" {
			t.Fatalf("%s's ORDER BY parsed as %v, which does not end in is_withdraw", table, history)
		}
		for _, c := range history {
			if !notIdentity[c] && !current[c] {
				t.Errorf("%s_current's ORDER BY lacks %s, which is in %s's: distinct "+
					"objects that differ only in %s collapse into one row", table, c, table, c)
			}
		}
	}
}

// createTableFromSchema returns the CREATE TABLE statement for vantage.<table>
// in schema.sql, from CREATE up to its semicolon, with comments stripped.
func createTableFromSchema(t *testing.T, table string) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("..", "deploy", "clickhouse", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	var b strings.Builder
	for line := range strings.SplitSeq(string(ddl), "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line + "\n")
	}
	re := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS vantage\.` +
		regexp.QuoteMeta(table) + `\b[^;]*;`)
	m := re.FindString(b.String())
	if m == "" {
		t.Fatalf("no CREATE TABLE for vantage.%s in schema.sql", table)
	}
	return m
}

// The object tables' engine is ReplacingMergeTree(seq): newest by seq wins a
// merge. Without the version, the last row INSERTED wins, and the writer
// inserts in any order (see TestCurrentStateIsIndependentOfInsertOrder, which
// exercises only route_unicast_current at runtime). peer_current and
// eor_current are ReplacingMergeTree with no version by design: peer_current
// keeps every event (its key includes ts_router and stream_seq), and
// eor_current records only that a marker exists.
//
// No current table may be partitioned, because ClickHouse deduplicates only
// within a partition and one object's rows can span months. No current table
// may have a TTL either: state that stays stable for longer than retention is
// still live, and a TTL can delete a withdrawal before it merges over the
// announcement it replaced.
func TestCurrentTableEnginesVersionBySeqWithNoPartitionOrTTL(t *testing.T) {
	engine := regexp.MustCompile(`\bENGINE\s*=\s*([A-Za-z]+(?:\([^)]*\))?)`)
	for table, want := range map[string]string{
		"route_unicast_current": "ReplacingMergeTree(seq)",
		"route_vpn_current":     "ReplacingMergeTree(seq)",
		"route_evpn_current":    "ReplacingMergeTree(seq)",
		"ls_nodes_current":      "ReplacingMergeTree(seq)",
		"ls_links_current":      "ReplacingMergeTree(seq)",
		"ls_prefixes_current":   "ReplacingMergeTree(seq)",
		"peer_current":          "ReplacingMergeTree",
		"eor_current":           "ReplacingMergeTree",
	} {
		stmt := createTableFromSchema(t, table)
		m := engine.FindStringSubmatch(stmt)
		if m == nil {
			t.Errorf("%s: no ENGINE clause", table)
		} else if got := strings.Join(strings.Fields(m[1]), ""); got != want {
			t.Errorf("%s: ENGINE = %s, want %s", table, got, want)
		}
		for name, re := range map[string]string{"PARTITION BY": `\bPARTITION\s+BY\b`, "TTL": `\bTTL\b`} {
			if regexp.MustCompile(re).MatchString(stmt) {
				t.Errorf("%s: has a %s clause; current tables must have none", table, name)
			}
		}
	}
}

// currentRoutesSQL reads the current unicast routes of one router, newest
// version per route. It does not rely on merges having run: argMax over
// (seq, stream_seq) picks the newest of whatever versions are still unmerged.
const currentRoutesSQL = `SELECT prefix, path_id,
    argMax(is_withdraw, (seq, stream_seq)), argMax(next_hop, (seq, stream_seq)), argMax(as_path, (seq, stream_seq))
  FROM vantage.route_unicast_current
  WHERE router_ip = toIPv6(?) GROUP BY prefix, path_id ORDER BY prefix, path_id`

// currentRoute is one row of currentRoutesSQL.
type currentRoute struct {
	Prefix     string
	PathID     uint32
	IsWithdraw uint8
	NextHop    string
	ASPath     []uint32
}

func readCurrentRoutes(t *testing.T, ctx context.Context, c *ClickHouse, router string) []currentRoute {
	t.Helper()
	rows, err := c.conn.Query(ctx, qualify(c, currentRoutesSQL), router)
	if err != nil {
		t.Fatalf("%s: %v", c.db, err)
	}
	defer rows.Close()
	var out []currentRoute
	for rows.Next() {
		var r currentRoute
		if err := rows.Scan(&r.Prefix, &r.PathID, &r.IsWithdraw, &r.NextHop, &r.ASPath); err != nil {
			t.Fatal(err)
		}
		if len(r.ASPath) == 0 {
			r.ASPath = nil
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// insertEach inserts every envelope through the writer's real path, one
// insert each, so each lands in a part of its own and a merge sees them in
// insertion order.
func insertEach(t *testing.T, ctx context.Context, c *ClickHouse, envs []seqEnvelope) {
	t.Helper()
	for _, e := range envs {
		rows, err := RowsFor(e.env, e.streamSeq)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Insert(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
}

// optimizeFinal forces every part of a table in c's database into one, which
// is the state ReplacingMergeTree eventually reaches on its own. Until it
// does, a test reading through argMax cannot tell a sort key that keeps the
// right row from one that will throw it away.
func optimizeFinal(t *testing.T, ctx context.Context, c *ClickHouse, table string) {
	t.Helper()
	if err := c.conn.Exec(ctx, qualify(c, "OPTIMIZE TABLE vantage."+table+" FINAL")); err != nil {
		t.Fatal(err)
	}
}

// seqEnvelope is an envelope with the NATS stream sequence the writer would
// have consumed it at.
type seqEnvelope struct {
	env       *vantagev1.Envelope
	streamSeq uint64
}

// routeEnvelope is one unicast UPDATE for one prefix on the fixture peer. A
// nil attrs means a withdrawal.
func routeEnvelope(router string, sessionID, seq uint64, prefix string, attrs *vantagev1.PathAttributes) *vantagev1.Envelope {
	re := &vantagev1.RouteEvent{Family: &vantagev1.Family{Afi: 1, Safi: 1}}
	if attrs == nil {
		re.Withdrawn = []*vantagev1.Prefix{{Prefix: prefix}}
	} else {
		re.Attrs = attrs
		re.Announced = []*vantagev1.Prefix{{Prefix: prefix}}
	}
	e := testEnv(re)
	e.CollectorId = "current-test"
	e.Router = &vantagev1.RouterId{Ip: router, SysName: "current-rtr"}
	e.SessionId = sessionID
	e.Seq = seq
	return e
}

// routePrefix is the one prefix a routeEnvelope carries, announced or withdrawn.
func routePrefix(e *vantagev1.Envelope) string {
	r := e.GetRoute()
	for _, p := range append(r.GetAnnounced(), r.GetWithdrawn()...) {
		return p.GetPrefix()
	}
	return ""
}

func peerUpEnvelope(router string, sessionID uint64) *vantagev1.Envelope {
	e := testEnv(&vantagev1.PeerEvent{Kind: vantagev1.PeerEvent_KIND_UP})
	e.CollectorId = "current-test"
	e.Router = &vantagev1.RouterId{Ip: router, SysName: "current-rtr"}
	e.SessionId = sessionID
	return e
}

func attrsVia(nextHop string, asns ...uint32) *vantagev1.PathAttributes {
	return &vantagev1.PathAttributes{
		NextHop: nextHop,
		AsPath:  []*vantagev1.AsPathSegment{{Type: 2, Asns: asns}},
	}
}

// The current state must not depend on the order the writer inserts events
// in. NATS redelivers, several writers interleave, and a batch that failed is
// retried after later ones succeeded, so a route's versions reach ClickHouse
// in any order and some of them twice. The newest version by seq must win
// regardless, whether or not merges have run (background merges start
// within the test's runtime, so the first read already sees some) and after
// OPTIMIZE FINAL has forced them all.
//
// 200 events over 50 prefixes go into database A in order and into B
// shuffled, every tenth one twice. Each prefix gets four versions, and each
// prefix ends one of three ways: withdrawn, withdrawn then re-announced with
// new attributes, or re-announced three times with attributes changing each
// time. The fixture's expected state is computed here, not read back from A,
// so A and B being wrong the same way fails too.
//
// The shuffle matters most after the merge: a ReplacingMergeTree with no
// version column keeps the LAST row inserted, which in B is often an older
// version. The test checks that the shuffle it used did that to some prefix,
// or it could not catch that engine.
func TestCurrentStateIsIndependentOfInsertOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	a := requireClickHouseDB(t, ctx, "vantage_sink_current_a_test")
	defer a.Close()
	b := requireClickHouseDB(t, ctx, "vantage_sink_current_b_test")
	defer b.Close()

	const router = "192.0.2.202"
	const session = 1
	var inOrder []seqEnvelope
	want := map[string]currentRoute{}
	seq := uint64(0)
	for version := range 4 {
		for i := range 50 {
			prefix := fmt.Sprintf("10.20.%d.0/24", i)
			nh := fmt.Sprintf("192.0.2.%d", 10+version)
			var at *vantagev1.PathAttributes
			switch {
			case i%3 == 0 && version == 3: // ends withdrawn
			case i%3 == 1 && version == 2: // withdrawn, then re-announced
			default:
				at = attrsVia(nh, 65000, uint32(65100+version))
			}
			seq++
			inOrder = append(inOrder, seqEnvelope{routeEnvelope(router, session, seq, prefix, at), seq})
			r := currentRoute{Prefix: prefix, IsWithdraw: 1}
			if at != nil {
				r = currentRoute{Prefix: prefix, NextHop: nh, ASPath: []uint32{65000, uint32(65100 + version)}}
			}
			want[prefix] = r
		}
	}

	shuffled := slices.Clone(inOrder)
	for i := 0; i < len(inOrder); i += 10 {
		shuffled = append(shuffled, inOrder[i])
	}
	rand.New(rand.NewPCG(3, 22)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	// The precondition for catching an engine that keeps the last insert:
	// some prefix's last insert into B is not its newest version.
	lastInserted := map[string]uint64{}
	for _, e := range shuffled {
		lastInserted[routePrefix(e.env)] = e.env.GetSeq()
	}
	stale := 0
	for _, e := range inOrder[150:] { // version 3, the newest of every prefix
		if lastInserted[routePrefix(e.env)] != e.env.GetSeq() {
			stale++
		}
	}
	t.Logf("%d of 50 prefixes have an older version inserted last into B", stale)
	if stale == 0 {
		t.Fatal("the shuffle left every prefix's newest version inserted last; " +
			"this fixture cannot tell highest-seq-wins from last-insert-wins")
	}

	insertEach(t, ctx, a, inOrder)
	insertEach(t, ctx, b, shuffled)

	var wantRows []currentRoute
	for _, p := range slices.Sorted(maps.Keys(want)) {
		wantRows = append(wantRows, want[p])
	}
	withdrawn, reannounced := 0, 0
	for _, r := range wantRows {
		if r.IsWithdraw == 1 {
			withdrawn++
		}
	}
	for i := 1; i < 50; i += 3 {
		if want[fmt.Sprintf("10.20.%d.0/24", i)].IsWithdraw == 0 {
			reannounced++
		}
	}
	if len(wantRows) != 50 || withdrawn == 0 || reannounced == 0 {
		t.Fatalf("fixture: %d routes, %d withdrawn, %d re-announced after a "+
			"withdrawal; want 50 and some of each", len(wantRows), withdrawn, reannounced)
	}

	check := func(stage string) {
		t.Helper()
		for _, c := range []*ClickHouse{a, b} {
			got := readCurrentRoutes(t, ctx, c, router)
			if !reflect.DeepEqual(got, wantRows) {
				diff := 0
				for i := range min(len(got), len(wantRows)) {
					if !reflect.DeepEqual(got[i], wantRows[i]) {
						if diff < 3 {
							t.Errorf("%s, %s: got %+v, want %+v", stage, c.db, got[i], wantRows[i])
						}
						diff++
					}
				}
				t.Errorf("%s, %s: %d rows, want %d; %d differ", stage, c.db, len(got), len(wantRows), diff)
			}
		}
	}
	check("as inserted")
	optimizeFinal(t, ctx, a, "route_unicast_current")
	optimizeFinal(t, ctx, b, "route_unicast_current")
	check("after OPTIMIZE FINAL")
}

// A router that reconnects gets a new session, but the old session's writer
// state can outlive it: a collector draining its buffer, a NATS redelivery, a
// writer retrying a batch. Those stragglers carry the old session id with a
// higher stream_seq, and the old session's seq ran higher than the new one's,
// which restarted. If the current key had no session_id, the old session's
// rows would replace the new session's (measured 2026-09-05: 200,460 of
// 1,000,000 routes lost). With it, they land in the old session's rows, which
// a reader restricted to the newest session never sees.
func TestCurrentStateIgnoresASupersededSessionThatKeepsPublishing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c := requireClickHouseDB(t, ctx, "vantage_sink_current_straggler_test")
	defer c.Close()

	const router = "192.0.2.203"
	const s1, s2 = 100, 101
	const s1NextHop, s2NextHop, stragglerNextHop = "192.0.2.1", "192.0.2.2", "192.0.2.3"
	prefix := func(i int) string { return fmt.Sprintf("10.30.%d.0/24", i) }

	var envs []seqEnvelope
	stream := uint64(0)
	add := func(e *vantagev1.Envelope) {
		stream++
		envs = append(envs, seqEnvelope{e, stream})
	}
	add(peerUpEnvelope(router, s1))
	for i := range 100 {
		add(routeEnvelope(router, s1, uint64(i+1), prefix(i), attrsVia(s1NextHop, 65000)))
	}
	add(peerUpEnvelope(router, s2))
	for i := range 100 {
		add(routeEnvelope(router, s2, uint64(i+1), prefix(i), attrsVia(s2NextHop, 65000)))
	}
	// S1's stragglers: after everything in S2 on the stream, and with seqs
	// above S2's, so neither tiebreaker saves S2.
	for i := range 100 {
		add(routeEnvelope(router, s1, uint64(101+i), prefix(i), attrsVia(stragglerNextHop, 65000)))
	}
	insertEach(t, ctx, c, envs)

	const q = `SELECT prefix, argMax(next_hop, (seq, stream_seq))
		FROM vantage.route_unicast_current
		WHERE collector_id = 'current-test' AND router_ip = toIPv6(?)
		  AND session_id = (SELECT max(session_id) FROM vantage.peer_current
		                    WHERE collector_id = 'current-test' AND router_ip = toIPv6(?))
		GROUP BY prefix ORDER BY prefix`
	check := func(stage string) {
		t.Helper()
		rows, err := c.conn.Query(ctx, qualify(c, q), router, router)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n, wrong := 0, map[string]int{}
		for rows.Next() {
			var p, nh string
			if err := rows.Scan(&p, &nh); err != nil {
				t.Fatal(err)
			}
			n++
			if nh != s2NextHop {
				wrong[nh]++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if n != 100 || len(wrong) != 0 {
			t.Errorf("%s: the newest session has %d routes, want 100; next hops "+
				"other than S2's (%s): %v", stage, n, s2NextHop, wrong)
		}
	}
	check("as inserted")
	optimizeFinal(t, ctx, c, "route_unicast_current")
	check("after OPTIMIZE FINAL")
}
