package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/api"
	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
)

// peerEventsCols names the nineteen peer_events columns these fixtures set,
// so their inserts are addressed by NAME rather than by position. An
// earlier schema added five more and every positional fixture broke at once
// with "expected 24 arguments, got 19" -- loud and correct, but a failure
// these tests have no opinion about. Named columns let the new ones take
// their DEFAULTs. The SINK's insert stays positional on purpose: there the
// argument count IS the check that writer and schema agree.
const peerEventsCols = "(collector_id, router_ip, router_sysname, peer_ip, rib, " +
	"peer_asn, peer_bgp_id, session_id, seq, ts_router, ts_collector, " +
	"parse_flags, stream_seq, kind, local_ip, local_port, remote_port, " +
	"down_reason, cap_four_byte_as)"

// testDB is this command's own database, for the reason every other
// package has one.
const testDB = "vantage_cli_query_test"

const testToken = "cli-test-token"

func testConfig() api.Config {
	return api.Config{
		DefaultPage: query.DefaultRIBPage,
		MaxPage:     query.MaxRIBPage,
		// 24h, matching api.Config's own LoadConfig default: the identical
		// zero-value landmine api/'s own test helpers document at length
		// (requireAPI's doc comment, api/handlers_test.go) -- an unset
		// MaxUnscopedSince refuses every unscoped /v1/events request with a
		// 400 quoting a 0s limit. Latent here today, since no command under
		// cmd/vantage/ calls /v1/events yet, but set correctly so the first
		// one that does is not the one that rediscovers this the hard way.
		MaxUnscopedSince: 24 * time.Hour,
		Tokens:           []api.Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
	}
}

// bothSources stands up a real query.Q over testDB and an httptest server
// in front of it, and returns the two source implementations that must
// agree.
func bothSources(t *testing.T) (direct, viaAPI source) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	q, err := query.New(conn, testDB)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	apiSrv, err := api.NewServer(q, testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)
	return &directSource{q: q}, &apiSource{base: srv.URL, token: testToken}
}

// TestBothSourcesProduceIdenticalResults is the reason the two transports
// are worth having: they must be indistinguishable to everything
// downstream. The API marshals query/'s own values and the client decodes
// them back, so any divergence is a bug in the wire types, not a property
// of the transport.
//
// It runs against whatever testDB holds, which may be nothing. That is not
// a weakness of this particular assertion -- two empty slices are equal for
// the wrong reason, so TestBothSourcesAgreeOnRealRows below inserts rows
// and asserts the comparison is non-vacuous. This one is the shape check;
// that one is the content check.
func TestBothSourcesProduceIdenticalResults(t *testing.T) {
	ctx := t.Context()
	direct, viaAPI := bothSources(t)

	gotDirect, err := direct.Routers(ctx)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	gotAPI, err := viaAPI.Routers(ctx)
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	if !reflect.DeepEqual(gotDirect, gotAPI) {
		t.Errorf("sources disagree:\n\tdirect: %+v\n\tapi:    %+v", gotDirect, gotAPI)
	}
}

// TestBothSourcesAgreeOnRealRows is the same comparison with something in
// the database, and it exists because the test above passes trivially on an
// empty one.
//
// Every field of every type is exercised on purpose: the round trip is
// lossy in exactly the places where the wire form is NOT the Go form --
// session_id and seq are strings, next_hop and origin_asn and label are
// nullable, and timestamps carry a location that JSON does not. A test that
// compared only prefixes would pass through any of those being dropped.
func TestBothSourcesAgreeOnRealRows(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertCLIFixture(t, ctx, conn)
	direct, viaAPI := bothSources(t)

	t.Run("routers", func(t *testing.T) {
		a, err := direct.Routers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := viaAPI.Routers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) == 0 {
			t.Fatal("no routers, so this comparison is vacuous")
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("routers disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
		}
		// The u64 that motivated the string encoding. A float64 round trip
		// would round this, and the value is chosen past 2^53 to make that
		// visible rather than theoretical. Found by address, not taken as
		// a[0]: testDB is shared, and other tests' routers sort ahead of
		// this one's.
		i := slices.IndexFunc(a, func(r query.Router) bool {
			return r.IP == netip.MustParseAddr(cliFixRouterIP)
		})
		if i < 0 {
			t.Fatalf("no router %s among %d", cliFixRouterIP, len(a))
		}
		if a[i].SessionID != cliFixSession {
			t.Errorf("session_id came back %d, want %d -- a large u64 did not "+
				"survive the round trip", a[i].SessionID, cliFixSession)
		}
	})

	t.Run("peers", func(t *testing.T) {
		router := netip.MustParseAddr(cliFixRouterIP)
		a, err := direct.Peers(ctx, router)
		if err != nil {
			t.Fatal(err)
		}
		b, err := viaAPI.Peers(ctx, router)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) == 0 {
			t.Fatal("no peers, so this comparison is vacuous")
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("peers disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
		}
	})

	t.Run("routes", func(t *testing.T) {
		rq := RouteQuery{Prefix: cliFixPrefix}
		a, err := direct.Routes(ctx, rq)
		if err != nil {
			t.Fatal(err)
		}
		b, err := viaAPI.Routes(ctx, rq)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Unicast) == 0 {
			t.Fatal("no unicast routes, so this comparison is vacuous")
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("routes disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
		}
	})

	t.Run("rib", func(t *testing.T) {
		rq := RIBQuery{
			Router: netip.MustParseAddr(cliFixRouterIP),
			Peer:   netip.MustParseAddr(cliFixPeerIP),
			Family: "unicast",
			// One row per page, so the walk crosses at least two page
			// boundaries and the cursor is genuinely exercised. A single
			// page would test the query and not the pagination.
			Limit: 1,
		}
		a, err := direct.RIB(ctx, rq)
		if err != nil {
			t.Fatal(err)
		}
		b, err := viaAPI.RIB(ctx, rq)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Unicast) < 2 {
			t.Fatalf("the walk returned %d rows; with limit 1 that is not enough "+
				"to have followed a cursor", len(a.Unicast))
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("rib disagrees:\n\tdirect: %+v\n\tapi:    %+v", a, b)
		}
	})
}

// A router watched by two collectors. Each collector holds a DIFFERENT set of
// routes for the same (router, peer), so a walk that silently read the wrong
// collector, or both, cannot pass: the answer names which one it came from.
const (
	cliDualRouterIP = "10.0.0.51"
	cliDualPeerIP   = "10.0.0.61"
	cliDualA        = "cli-dual-a"
	cliDualB        = "cli-dual-b"
)

var cliDualRoutes = map[string][]string{
	cliDualA: {"10.98.0.0/24", "10.98.1.0/24"},
	cliDualB: {"10.98.2.0/24"},
}

func insertDualHomedFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	router := netip.MustParseAddr(cliDualRouterIP)
	peer := netip.MustParseAddr(cliDualPeerIP)
	sessions := map[string]uint64{cliDualA: 1788023088001124901, cliDualB: 1788023088001124902}
	// stream_seq is the NATS stream sequence, unique across every collector
	// in production. peer_events deduplicates on (router, peer, rib,
	// ts_router, stream_seq) with no collector_id in the key, so two
	// collectors' rows sharing a stream_seq collapse into one on insert, and
	// the router would read as watched by a single collector.
	seqBase := map[string]uint64{cliDualA: 100, cliDualB: 200}

	pe, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	uni, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast: %v", err)
	}
	for _, c := range []string{cliDualA, cliDualB} {
		if err := pe.Append(
			c, router, "cli-dual-router", peer, "in_pre", uint32(65030),
			router, sessions[c], uint64(1), ts, ts, []string{}, seqBase[c],
			"up", router, uint16(179), uint16(50010), uint32(0), uint8(1),
		); err != nil {
			t.Fatalf("append peer_events: %v", err)
		}
		for i, prefix := range cliDualRoutes[c] {
			if err := uni.Append(
				c, router, "cli-dual-router", peer, "in_pre", uint32(65030),
				router, sessions[c], uint64(i+2), ts, ts, []string{}, seqBase[c]+uint64(i+1),
				"ipv4u", prefix, uint32(0), uint8(0), uint8(0),
				[]uint32{65030}, cliDualPeerIP, nil, nil,
				[]uint32{}, []string{}, []string{}, []string{},
			); err != nil {
				t.Fatalf("append route_unicast: %v", err)
			}
		}
	}
	if err := pe.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
	if err := uni.Send(); err != nil {
		t.Fatalf("send route_unicast: %v", err)
	}
}

// TestBothSourcesWalkADualHomedRouterByCollector: `vantage query rib` must be
// able to walk a router that two collectors watch. The API refuses such a
// walk without collector=, and before -collector existed the CLI had no way
// to send one, so the command failed on every dual-homed router. Both
// transports are checked, since -dsn has no handler in front of it.
func TestBothSourcesWalkADualHomedRouterByCollector(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertDualHomedFixture(t, ctx, conn)
	direct, viaAPI := bothSources(t)

	base := RIBQuery{
		Router: netip.MustParseAddr(cliDualRouterIP),
		Peer:   netip.MustParseAddr(cliDualPeerIP),
		Family: "unicast",
		// One row per page, so collector A's two routes take a cursor, and
		// a pin that only survived the first page would lose the second.
		Limit: 1,
	}

	for name, src := range map[string]source{"direct": direct, "api": viaAPI} {
		t.Run(name, func(t *testing.T) {
			if _, err := src.RIB(ctx, base); err == nil {
				t.Error("no -collector on a dual-homed router: got an answer, want the refusal")
			}

			for _, c := range []string{cliDualA, cliDualB} {
				rq := base
				rq.Collector = c
				out, err := src.RIB(ctx, rq)
				if err != nil {
					t.Fatalf("collector %s: %v", c, err)
				}
				var got []string
				for _, r := range out.Unicast {
					got = append(got, r.Prefix)
				}
				slices.Sort(got)
				if !slices.Equal(got, cliDualRoutes[c]) {
					t.Errorf("collector %s: got %v, want exactly its own routes %v", c, got, cliDualRoutes[c])
				}
			}

			rq := base
			rq.Collector = "no-such-collector"
			// The message, not just an error: without the explicit check the
			// walk still fails, but later, as a confusing "session changed".
			if _, err := src.RIB(ctx, rq); err == nil || !strings.Contains(err.Error(), "has no session for router") {
				t.Errorf("a collector with no session for this router: got %v, want the no-session refusal", err)
			}
		})
	}
}

// TestRIBCollectorHint: the dual-homed refusal gains the CLI's remedy only
// when -collector was not given, and no other error is touched.
func TestRIBCollectorHint(t *testing.T) {
	refusal := errors.New("query: invalid filter: router 10.0.0.51 is monitored by 2 collectors (a, b)")
	if got := ribCollectorHint(refusal, "").Error(); !strings.Contains(got, "pass -collector") {
		t.Errorf("refusal without -collector: %q, want it to name -collector", got)
	}
	if got := ribCollectorHint(refusal, "a"); got != refusal {
		t.Errorf("with -collector already given, the error was rewritten: %q", got)
	}
	other := errors.New("dial tcp: connection refused")
	if got := ribCollectorHint(other, ""); got != other {
		t.Errorf("an unrelated error was rewritten: %q", got)
	}
}

// TestAPISourceReportsAnErrorBodyRatherThanAStatus: a 400 from the API
// carries a message saying which parameter was wrong, and dropping it in
// favor of "unexpected status 400" would make the CLI strictly worse to use
// than curl.
func TestAPISourceReportsAnErrorBodyRatherThanAStatus(t *testing.T) {
	ctx := t.Context()
	_, viaAPI := bothSources(t)

	// Neither prefix nor covers: the fan-out's own 400.
	_, err := viaAPI.Routes(ctx, RouteQuery{})
	if err == nil {
		t.Fatal("an empty route query was accepted")
	}
	if !strings.Contains(err.Error(), "covers") {
		t.Errorf("the error does not carry the API's own message, so the caller "+
			"cannot tell what to fix: %v", err)
	}
}

// TestAPISourceRejectsAMissingToken pins that the CLI surfaces a 401 as a
// 401 rather than as a decode failure on an error body it tried to read as
// data.
func TestAPISourceRejectsAMissingToken(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	q, err := query.New(conn, testDB)
	if err != nil {
		t.Fatal(err)
	}
	apiSrv, err := api.NewServer(q, testConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)

	_, err = (&apiSource{base: srv.URL, token: "wrong"}).Routers(ctx)
	if err == nil {
		t.Fatal("a bad token was accepted")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("a rejected token should say so; got %v", err)
	}
}

// TestQueryFlagsRejectAnUnknownOutputFormat: -o is a closed set, and an
// unrecognized value must not silently fall through to one of them.
func TestQueryFlagsRejectAnUnknownOutputFormat(t *testing.T) {
	err := cmdQuery([]string{"routers", "-o", "yaml", "-dsn", "clickhouse://x/y"})
	if err == nil {
		t.Fatal("-o yaml was accepted")
	}
	if !strings.Contains(err.Error(), "table") || !strings.Contains(err.Error(), "json") {
		t.Errorf("the refusal should name the formats that do work; got %v", err)
	}
}

func TestQueryRejectsAnUnknownSubcommand(t *testing.T) {
	if err := cmdQuery([]string{"prefixes"}); err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
}

// The CLI fixture's coordinates. cliFixSession is deliberately past 2^53:
// it is the value that makes the u64-as-string rule testable, since a
// smaller one round-trips through a JSON number unharmed and would let the
// defect through.
const (
	cliFixCollector = "cli-test-collector"
	cliFixSysName   = "cli-fixture-router"
	cliFixRouterIP  = "10.0.0.31"
	cliFixPeerIP    = "10.0.0.41"
	// A second peer that ends its session view-lost, so the router carries
	// a non-zero peers_view_lost and the round trip for that field is
	// exercised rather than compared as 0 == 0.
	cliFixLostPeerIP = "10.0.0.42"
	cliFixPrefix     = "10.99.0.0/24"
	cliFixSession    = uint64(1788023088001124818)

	// The wide filters' coordinates. The AS path is {65010, 65020}, so
	// the origin is its LAST element and the through-AS is any element --
	// two different questions that a filter reading the wrong end of the
	// path would answer identically, which is why they are separate
	// constants with different values rather than one shared AS.
	cliFixOriginASN    = uint32(65020)
	cliFixThroughASN   = uint32(65010)
	cliFixCommunity    = "65000:100"
	cliFixCommunityRaw = uint32(65000)<<16 | 100
)

// insertCLIFixture writes one router, one peer and three unicast routes.
// Three, not one: the RIB walk in TestBothSourcesAgreeOnRealRows runs at
// limit 1, so it needs enough rows to have followed a cursor at least twice.
func insertCLIFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := peers.Append(
		cliFixCollector, netip.MustParseAddr(cliFixRouterIP), cliFixSysName,
		netip.MustParseAddr(cliFixPeerIP), "in_pre", uint32(65010),
		netip.MustParseAddr(cliFixRouterIP), cliFixSession, uint64(1),
		ts, ts, []string{}, uint64(1),
		"up", netip.MustParseAddr(cliFixRouterIP), uint16(179), uint16(50001),
		uint32(0), uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	// A second peer on the same router whose session ended view-lost. It is
	// here so the round-trip comparison below is non-vacuous for
	// peers_view_lost: with only the 'up' peer above, that count is 0 on
	// both sides and a wire type that dropped the field entirely would
	// compare equal. cliFixLostPeerIP advertises nothing, so it changes no
	// route answer -- only the router's peer counts.
	lost, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events (view-lost): %v", err)
	}
	for i, kind := range []string{"up", "view_lost"} {
		if err := lost.Append(
			cliFixCollector, netip.MustParseAddr(cliFixRouterIP), cliFixSysName,
			netip.MustParseAddr(cliFixLostPeerIP), "in_pre", uint32(65011),
			netip.MustParseAddr(cliFixRouterIP), cliFixSession, uint64(i+1),
			ts, ts, []string{}, uint64(i+10),
			kind, netip.MustParseAddr(cliFixRouterIP), uint16(179), uint16(50002),
			uint32(0), uint8(1),
		); err != nil {
			t.Fatalf("append peer_events (view-lost): %v", err)
		}
	}
	if err := lost.Send(); err != nil {
		t.Fatalf("send peer_events (view-lost): %v", err)
	}

	uni, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast: %v", err)
	}
	for i, prefix := range []string{cliFixPrefix, "10.99.1.0/24", "10.99.2.0/24"} {
		if err := uni.Append(
			cliFixCollector, netip.MustParseAddr(cliFixRouterIP), cliFixSysName,
			netip.MustParseAddr(cliFixPeerIP), "in_pre", uint32(65010),
			netip.MustParseAddr(cliFixRouterIP), cliFixSession, uint64(i+1),
			ts, ts, []string{}, uint64(i+1),
			"ipv4u", prefix, uint32(0), uint8(0), uint8(0),
			// A non-empty AS path, so origin_asn is non-null on the wire
			// and the nullable round trip is exercised rather than skipped.
			[]uint32{65010, 65020}, "10.0.0.41", nil, nil,
			// One standard community, so -community has something real to
			// match. The three community-bearing columns beside it stay
			// empty on purpose: an answer that came back for
			// community=65000:100 could then only have come from this one.
			[]uint32{cliFixCommunityRaw}, []string{}, []string{}, []string{},
		); err != nil {
			t.Fatalf("append route_unicast: %v", err)
		}
	}
	if err := uni.Send(); err != nil {
		t.Fatalf("send route_unicast: %v", err)
	}
}

// TestBothSourcesFilterByWideFilters is the CLI half of the wide-filter
// slice. `vantage query routes -origin-asn X` has to ask the same question
// /v1/routes?origin_asn=X answers, over BOTH transports -- and the -dsn
// transport has no handler in front of it, so it is the one that could
// silently accept a flag and drop it.
//
// Every case asserts in both directions: the matching value returns the
// fixture's rows, and a value nothing carries returns none. The second
// assertion is the load-bearing one. A flag that is parsed, stored and then
// never rendered into a predicate passes the first assertion perfectly --
// it returns rows, they just are not the filtered ones -- and that is not a
// hypothetical failure mode here. It is a defect this code already
// shipped once (see RouteFilter.Prefix's doc comment) and the reason this
// project's rule is to assert on rows rather than on a status code.
func TestBothSourcesFilterByWideFilters(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertCLIFixture(t, ctx, conn)
	direct, viaAPI := bothSources(t)

	for _, tc := range []struct {
		name        string
		match, miss RouteQuery
	}{
		{
			"origin-asn",
			RouteQuery{OriginASN: cliFixOriginASN},
			RouteQuery{OriginASN: 64999},
		},
		{
			// The through-AS is the path's FIRST element, which the origin
			// filter must not match: if -through-asn were wired to
			// as_path[-1] this case returns nothing and the miss below
			// returns nothing too, so both halves are needed to tell the
			// two filters apart.
			"through-asn",
			RouteQuery{ThroughASN: cliFixThroughASN},
			RouteQuery{ThroughASN: 64999},
		},
		{
			"community",
			RouteQuery{Community: cliFixCommunity},
			RouteQuery{Community: "64999:1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, src := range []struct {
				name string
				s    source
			}{{"direct", direct}, {"api", viaAPI}} {
				t.Run(src.name, func(t *testing.T) {
					hit, err := src.s.Routes(ctx, tc.match)
					if err != nil {
						t.Fatalf("matching query: %v", err)
					}
					if len(hit.Unicast) == 0 {
						t.Errorf("%+v returned no unicast routes, but the fixture "+
							"carries three rows that match it -- the filter was "+
							"rendered, and rendered wrong", tc.match)
					}
					none, err := src.s.Routes(ctx, tc.miss)
					if err != nil {
						t.Fatalf("non-matching query: %v", err)
					}
					if n := len(none.Unicast); n != 0 {
						t.Errorf("%+v matches no fixture row but returned %d unicast "+
							"routes -- the flag was accepted and then never reached "+
							"a predicate, so every answer it gives is unfiltered",
							tc.miss, n)
					}
				})
			}
		})
	}
}

// TestQueryRoutesRefusesASZero: AS 0 is reserved by RFC 7607 and originates
// nothing, and query/'s filter structs spell "not asked" as 0 -- so an
// explicit -origin-asn 0 that reached them would quietly become an
// unfiltered dump of every route in the fleet. api/handlers.go's params.asn
// already refuses it with a 400; this is the same refusal on the -dsn path,
// which has no handler in front of it to do the refusing.
//
// The -dsn given is never dialed: the refusal has to happen while parsing
// flags, before any connection, which is what makes this testable without a
// database and is also the right place for it.
func TestQueryRoutesRefusesASZero(t *testing.T) {
	for _, flagName := range []string{"-origin-asn", "-through-asn"} {
		t.Run(flagName, func(t *testing.T) {
			err := cmdQuery([]string{"routes", flagName, "0", "-dsn", "clickhouse://x/y"})
			if err == nil {
				t.Fatalf("%s 0 was accepted", flagName)
			}
			if !strings.Contains(err.Error(), "7607") {
				t.Errorf("the refusal should say why AS 0 is not a question, the way "+
					"the API's own does; got %v", err)
			}
		})
	}
}

// TestQueryRoutesRefusesAnOversizedASN guards the truncation a uint32 field
// makes silent: 4294967296 is 0 in 32 bits, so an unchecked parse turns the
// largest possible typo into the unfiltered dump the test above exists to
// prevent.
func TestQueryRoutesRefusesAnOversizedASN(t *testing.T) {
	err := cmdQuery([]string{"routes", "-origin-asn", "4294967296", "-dsn", "clickhouse://x/y"})
	if err == nil {
		t.Fatal("an AS number past 2^32-1 was accepted")
	}
	if !strings.Contains(err.Error(), "4294967295") {
		t.Errorf("the refusal should name the range that does work; got %v", err)
	}
}

// ---- link-state ----

// The link-state CLI fixture's coordinates, kept apart from cliFixRouterIP
// and friends: insertCLIFixture and insertCLILSFixture can both run inside
// TestBothSourcesAgreeOnRealRows-style tests in one testDB, and
// peerStateCTE resolves exactly one CURRENT session per (collector,
// router) -- so a second fixture reusing the first's (collector, router)
// would not add a second current peer, it would just move which peer_events
// row `cur` calls current. See insertLSFixture's own comment in
// api/handlers_test.go for the same rule stated at the API layer.
const (
	cliLSCollector = "cli-ls-test-collector"
	cliLSSysName   = "cli-ls-fixture-router"
	cliLSRouterIP  = "10.0.0.32"
	cliLSPeerIP    = "10.0.0.42"
	cliLSSession   = uint64(2)

	// isis-l2 by number and by name resolve to the same protocol --
	// ParseProtocol's own property -- so cliLSProtocol backs both "2" and
	// "isis-l2" as CLI input without them meaning two different things.
	cliLSProtocol = uint8(2) // isis-l2
	cliLSArea     = uint32(0)
	cliLSASN      = uint32(65100)

	// cliLSZeroASNRouterID names the one ls_nodes row with an explicit
	// asn=0 -- LSNodeFilter's own doc comment says a node with no AS
	// descriptor stores exactly that, so "asn 0" has to stay a real,
	// askable value rather than collapsing into "asn not asked."
	cliLSZeroASNRouterID = "0a0000f9"

	// cliLSNineAreaRouterID names the one ls_nodes row with area=9 rather
	// than 0. Without it, EVERY ls_nodes row the fixture writes carries
	// area 0, so a filter that quietly treated Area="0" as "not asked" (the
	// exact defect area="0" exists to catch) would return the identical row
	// set as the correctly-filtered answer: nothing in the fixture could
	// tell the two apart. See TestBothSourcesAgreeOnLinkState's own
	// "area-zero" case for the mutation that motivated adding this row --
	// the direct/API DeepEqual comparison is what a zero-collapse bug
	// trips, but only once there is a non-zero-area row for the mutated
	// side to wrongly include.
	cliLSNineAreaRouterID = "0a0000fa"

	// cliLSOffRouterIP, cliLSOffPeerIP and cliLSOffRIB are a router, a peer
	// and a RIB view no fixture row carries -- real, well-formed values
	// (not "any string will do") so that -router=, -peer= and -rib=
	// exercise the actual predicate rather than merely a parse failure.
	cliLSOffRouterIP = "10.0.0.199"
	cliLSOffPeerIP   = "10.0.0.198"
	cliLSOffRIB      = "loc_rib"

	cliLSPrefix     = "10.90.30.0/24"
	cliLSCoversAddr = "10.90.30.5"

	// cliLSRemoteRouterID and cliLSRemoteArea are the far end of the one
	// live link the fixture writes. LocalNode and RemoteNode each have to
	// exclude the OTHER end for the two flags to be more than each other in
	// disguise -- see LSLinkFilter.LocalNode's own doc comment -- and
	// cliLSRemoteArea lets a single link exercise the either-end area
	// predicate without a second link row.
	cliLSRemoteRouterID = "0a0000g1"
	cliLSRemoteArea     = uint32(9)
	cliLSRemoteASN      = uint32(65200)

	// cliLSOtherAreaPrefix is the one ls_prefixes row that carries a
	// different area AND a different AS from every other prefix here. It
	// reuses the link's remote identity (cliLSRemoteRouterID /
	// cliLSRemoteArea / cliLSRemoteASN) rather than inventing a fourth,
	// so the fixture has one fewer identity to keep straight.
	//
	// Without it, every ls_prefixes row shared one area and one ASN, and
	// area= and asn= on /v1/ls/prefixes were the SAME FILTER as far as
	// this repo could tell: transposing them in handleLSPrefixes' filter
	// literal left ./api and ./cmd/vantage green, and transposing them in
	// LSPrefixFilter.predicates left all eighteen packages green. That is
	// cliLSNineAreaRouterID's own argument -- a fixture where every row
	// shares a column cannot tell a filter on that column from no filter
	// at all -- applied to the resource it had not been applied to.
	//
	// The values make a transposed statement match NOTHING rather than
	// something plausible: no fixture prefix carries asn 0 or asn 9, and
	// none carries area 65100 or area 65200, so the swapped predicates
	// return an empty answer and TestBothSourcesAgreeOnLinkStatePrefixes'
	// area and asn MATCH halves fail outright.
	cliLSOtherAreaPrefix = "10.90.32.0/24"
)

// insertCLILSFixture writes the link-state rows TestBothSourcesAgreeOnLinkState
// and its two siblings read: one peer_events row under cliLSRouterIP /
// cliLSPeerIP (distinct from insertCLIFixture's own, per the const block's
// comment above), four ls_nodes rows, one live ls_links adjacency plus its
// withdrawn twin, and one live ls_prefixes row plus its withdrawn twin.
//
// The ls_nodes shape:
//
//	-a     protocol isis-l2, area 0, asn 65100, LIVE. The row every
//	       narrower filter must still include.
//	-c     -a's shape again, WITHDRAWN: announced at a lower seq and
//	       withdrawn at a higher one, same identity both times, so argMax's
//	       (seq, stream_seq) ordering resolves the pair to exactly one row.
//	-zero  -a's shape with asn 0 instead of 65100 -- the row asn="0" has to
//	       match and asn="" (not asked) must not exclude.
//	-nine  -a's shape with area 9 instead of 0, LIVE. Without this row
//	       every ls_nodes row shares area 0, so area="0" and "area not
//	       filtered" answer identically and a zero-collapse bug in the area
//	       predicate has nothing to disturb -- see cliLSNineAreaRouterID's
//	       own comment.
//
// The link reuses -a's identity on its LOCAL end (router_id "0a0000f1", the
// same six columns node_key hashes), so LSLinks' Local.NodeKey and this
// fixture's ls_nodes -a row's own NodeKey are numerically the same value --
// this file never computes that hash itself, TestBothSourcesAgreeOnLinkStateLinks
// reads it back from a live query instead. Its REMOTE end is a second,
// unrelated identity in a different area, so area= has something to match
// on either end and LocalNode/RemoteNode have two distinct real keys to
// tell apart. The prefix rows reuse -a's identity too, for the same reason:
// node= on /v1/ls/prefixes is the identical hash under a different column
// set.
func insertCLILSFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := peers.Append(
		cliLSCollector, netip.MustParseAddr(cliLSRouterIP), cliLSSysName,
		netip.MustParseAddr(cliLSPeerIP), "in_pre", uint32(65001),
		netip.MustParseAddr(cliLSRouterIP), cliLSSession, uint64(1),
		ts, ts, []string{}, uint64(1),
		"up", netip.MustParseAddr(cliLSRouterIP), uint16(179), uint16(50001),
		uint32(0), uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}

	nodes, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".ls_nodes")
	if err != nil {
		t.Fatalf("prepare ls_nodes: %v", err)
	}
	type nodeRow struct {
		seq        uint64
		asn        uint32
		area       uint32
		routerID   string
		name       string
		isWithdraw uint8
	}
	for _, r := range []nodeRow{
		{700, cliLSASN, cliLSArea, "0a0000f1", "cli-ls-node-a", 0},
		{702, cliLSASN, cliLSArea, "0a0000f3", "cli-ls-node-c", 0}, // announce
		{703, cliLSASN, cliLSArea, "0a0000f3", "cli-ls-node-c", 1}, // withdrawal, wins on seq
		{704, 0, cliLSArea, cliLSZeroASNRouterID, "cli-ls-node-zeroasn", 0},
		{705, cliLSASN, cliLSRemoteArea, cliLSNineAreaRouterID, "cli-ls-node-nine", 0},
	} {
		if err := nodes.Append(
			cliLSCollector, netip.MustParseAddr(cliLSRouterIP), cliLSSysName,
			netip.MustParseAddr(cliLSPeerIP), "in_pre", uint32(65001),
			netip.MustParseAddr(cliLSRouterIP), cliLSSession, r.seq,
			ts, ts, []string{}, r.seq,
			cliLSProtocol, uint64(0), r.asn, uint32(0), r.area, r.routerID,
			"", r.isWithdraw, r.name,
			uint32(16000), uint32(8000), uint32(15000), uint32(1000),
			[]uint8{0}, map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_nodes: %v", err)
		}
	}
	if err := nodes.Send(); err != nil {
		t.Fatalf("send ls_nodes: %v", err)
	}

	links, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".ls_links")
	if err != nil {
		t.Fatalf("prepare ls_links: %v", err)
	}
	type linkRow struct {
		seq                       uint64
		localIfaddr, remoteIfaddr string
		localLinkID, remoteLinkID uint32
		isWithdraw                uint8
	}
	for _, r := range []linkRow{
		{720, "10.2.1.1", "10.2.1.2", 1, 2, 0}, // live
		{721, "10.2.2.1", "10.2.2.2", 3, 4, 0}, // announce, second identity
		{722, "10.2.2.1", "10.2.2.2", 3, 4, 1}, // withdrawal of 721's identity
	} {
		if err := links.Append(
			cliLSCollector, netip.MustParseAddr(cliLSRouterIP), cliLSSysName,
			netip.MustParseAddr(cliLSPeerIP), "in_pre", uint32(65001),
			netip.MustParseAddr(cliLSRouterIP), cliLSSession, r.seq,
			ts, ts, []string{}, r.seq,
			cliLSProtocol, uint64(0),
			cliLSASN, uint32(0), cliLSArea, "0a0000f1",
			cliLSRemoteASN, uint32(0), cliLSRemoteArea, cliLSRemoteRouterID,
			r.localIfaddr, r.remoteIfaddr, r.localLinkID, r.remoteLinkID,
			r.isWithdraw,
			[]uint32{24001}, []uint8{0x30}, []uint8{0},
			uint32(10), uint32(20), uint32(0), float32(1e9),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_links: %v", err)
		}
	}
	if err := links.Send(); err != nil {
		t.Fatalf("send ls_links: %v", err)
	}

	pfx, err := conn.PrepareBatch(ctx, "INSERT INTO "+testDB+".ls_prefixes")
	if err != nil {
		t.Fatalf("prepare ls_prefixes: %v", err)
	}
	type pfxRow struct {
		seq        uint64
		addr       string
		asn        uint32
		area       uint32
		routerID   string
		isWithdraw uint8
	}
	for _, r := range []pfxRow{
		{740, "10.90.30.0", cliLSASN, cliLSArea, "0a0000f1", 0}, // live, matches cliLSPrefix/cliLSCoversAddr
		{741, "10.90.31.0", cliLSASN, cliLSArea, "0a0000f1", 0}, // announce, withdrawn below
		{742, "10.90.31.0", cliLSASN, cliLSArea, "0a0000f1", 1}, // withdrawal
		// Live, and the ONLY prefix row that differs from the three above
		// in area and in AS at once. See cliLSOtherAreaPrefix.
		{743, "10.90.32.0", cliLSRemoteASN, cliLSRemoteArea, cliLSRemoteRouterID, 0},
	} {
		if err := pfx.Append(
			cliLSCollector, netip.MustParseAddr(cliLSRouterIP), cliLSSysName,
			netip.MustParseAddr(cliLSPeerIP), "in_pre", uint32(65001),
			netip.MustParseAddr(cliLSRouterIP), cliLSSession, r.seq,
			ts, ts, []string{}, r.seq,
			cliLSProtocol, uint64(0), r.asn, uint32(0), r.area, r.routerID,
			r.addr, uint8(24),
			r.isWithdraw, uint32(16001), uint8(0), uint8(1), uint32(10), uint8(0), uint8(0),
			map[uint16]string{},
		); err != nil {
			t.Fatalf("append ls_prefixes: %v", err)
		}
	}
	if err := pfx.Send(); err != nil {
		t.Fatalf("send ls_prefixes: %v", err)
	}
}

// TestBothSourcesAgreeOnLinkState is the reason the two transports exist.
// The -dsn path has no handler in front of it, so it is the one that could
// accept a flag and silently drop it.
//
// It proves agreement only on a handful of rows, well under any max_page a
// real daemon configures -- see LSQuery's own doc comment for why -dsn is
// uncapped while the API is not, and why that makes this test unable to
// reach the one boundary where the two transports are allowed to disagree.
func TestBothSourcesAgreeOnLinkState(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertCLILSFixture(t, ctx, conn)
	direct, viaAPI := bothSources(t)

	// -a's node_key is read back from a live, unfiltered query rather than
	// computed: node_key is a MATERIALIZED cityHash64, the same reason
	// TestBothSourcesAgreeOnLinkStateLinks reads Local.NodeKey back instead
	// of hard-coding it. Unlike LocalNode/RemoteNode, a plain Node filter
	// has no direction to exploit for its miss case -- a different node's
	// REAL key would still correctly match that other node -- so the miss
	// below is an arbitrary key no fixture row carries, the same choice
	// TestBothSourcesAgreeOnLinkStatePrefixes' own "node" case makes.
	live, err := direct.LSNodes(ctx, LSQuery{})
	if err != nil {
		t.Fatalf("discovering the fixture's node key: %v", err)
	}
	var aKey string
	for _, n := range live {
		if n.RouterID == "0a0000f1" {
			aKey = strconv.FormatUint(n.NodeKey, 10)
			break
		}
	}
	if aKey == "" {
		t.Fatalf("fixture discovery: 0a0000f1 not found among live nodes, got %+v", live)
	}

	for _, tc := range []struct {
		name  string
		match LSQuery
		miss  LSQuery
	}{
		{"protocol", LSQuery{Protocol: "isis-l2"}, LSQuery{Protocol: "bgp"}},
		// The miss is "42", not "9": cliLSNineAreaRouterID's row genuinely
		// carries area 9 now (see its own comment for why that row exists),
		// so "9" would be a match rather than a miss. 42 is a value no
		// ls_nodes row in this fixture carries.
		{"area-zero", LSQuery{Area: "0"}, LSQuery{Area: "42"}},
		// asn=0 is the archive's real "no AS descriptor" value -- see
		// cliLSZeroASNRouterID's own comment -- so this is area-zero's
		// argument applied to the OTHER pointer field LSNodeFilter carries.
		{"asn-zero", LSQuery{ASN: "0"}, LSQuery{ASN: "424242"}},
		{"state", LSQuery{State: "withdrawn"}, LSQuery{State: "withdrawn", Area: "9"}},
		{"router", LSQuery{Router: netip.MustParseAddr(cliLSRouterIP)},
			LSQuery{Router: netip.MustParseAddr(cliLSOffRouterIP)}},
		{"peer", LSQuery{Peer: netip.MustParseAddr(cliLSPeerIP)},
			LSQuery{Peer: netip.MustParseAddr(cliLSOffPeerIP)}},
		{"rib", LSQuery{RIB: "in_pre"}, LSQuery{RIB: cliLSOffRIB}},
		{"node", LSQuery{Node: aKey}, LSQuery{Node: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, src := range []struct {
				name string
				s    source
			}{{"direct", direct}, {"api", viaAPI}} {
				t.Run(src.name, func(t *testing.T) {
					hit, err := src.s.LSNodes(ctx, tc.match)
					if err != nil {
						t.Fatalf("matching: %v", err)
					}
					if len(hit) == 0 {
						t.Errorf("%+v returned no nodes, but the fixture matches it",
							tc.match)
					}
					none, err := src.s.LSNodes(ctx, tc.miss)
					if err != nil {
						t.Fatalf("non-matching: %v", err)
					}
					if len(none) != 0 {
						t.Errorf("%+v matches no fixture row but returned %d nodes "+
							"-- the flag never reached a predicate", tc.miss, len(none))
					}
				})
			}
			a, err := direct.LSNodes(ctx, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			b, err := viaAPI.LSNodes(ctx, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("sources disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
			}
		})
	}
}

// TestBothSourcesAgreeOnLinkStateLinks is TestBothSourcesAgreeOnLinkState's
// counterpart for /v1/ls/links: the either-end area predicate, and
// LocalNode/RemoteNode, which are the one pair links carry that nodes and
// prefixes do not -- see LSLinkFilter.LocalNode's own doc comment for why
// they are deliberately NOT either-end, and this test's local-node/
// remote-node cases for why that distinction needs both a match AND a
// same-link, wrong-end miss to prove: a filter that quietly matched either
// end would still pass a match-only assertion.
func TestBothSourcesAgreeOnLinkStateLinks(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertCLILSFixture(t, ctx, conn)
	direct, viaAPI := bothSources(t)

	// local_node_key and remote_node_key are MATERIALIZED cityHash64
	// values; this test reads them back from the one live link rather than
	// recomputing the hash.
	live, err := direct.LSLinks(ctx, LSQuery{})
	if err != nil {
		t.Fatalf("discovering the fixture's node keys: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("fixture discovery: want exactly 1 live link, got %d", len(live))
	}
	localKey := strconv.FormatUint(live[0].Local.NodeKey, 10)
	remoteKey := strconv.FormatUint(live[0].Remote.NodeKey, 10)

	for _, tc := range []struct {
		name  string
		match LSQuery
		miss  LSQuery
	}{
		{"area-either-end", LSQuery{Area: "9"}, LSQuery{Area: "42"}},
		{"local-node", LSQuery{LocalNode: localKey}, LSQuery{LocalNode: remoteKey}},
		{"remote-node", LSQuery{RemoteNode: remoteKey}, LSQuery{RemoteNode: localKey}},
		{"state", LSQuery{State: "withdrawn"}, LSQuery{State: "withdrawn", Area: "42"}},
		{"router", LSQuery{Router: netip.MustParseAddr(cliLSRouterIP)},
			LSQuery{Router: netip.MustParseAddr(cliLSOffRouterIP)}},
		{"peer", LSQuery{Peer: netip.MustParseAddr(cliLSPeerIP)},
			LSQuery{Peer: netip.MustParseAddr(cliLSOffPeerIP)}},
		{"rib", LSQuery{RIB: "in_pre"}, LSQuery{RIB: cliLSOffRIB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, src := range []struct {
				name string
				s    source
			}{{"direct", direct}, {"api", viaAPI}} {
				t.Run(src.name, func(t *testing.T) {
					hit, err := src.s.LSLinks(ctx, tc.match)
					if err != nil {
						t.Fatalf("matching: %v", err)
					}
					if len(hit) == 0 {
						t.Errorf("%+v returned no links, but the fixture matches it",
							tc.match)
					}
					none, err := src.s.LSLinks(ctx, tc.miss)
					if err != nil {
						t.Fatalf("non-matching: %v", err)
					}
					if len(none) != 0 {
						t.Errorf("%+v matches no fixture row but returned %d links "+
							"-- the flag never reached a predicate", tc.miss, len(none))
					}
				})
			}
			a, err := direct.LSLinks(ctx, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			b, err := viaAPI.LSLinks(ctx, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("sources disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
			}
		})
	}
}

// TestBothSourcesAgreeOnLinkStatePrefixes is TestBothSourcesAgreeOnLinkState's
// counterpart for /v1/ls/prefixes: prefix (exact match), covers
// (containment), node (the identity key nodes and prefixes share, unlike
// links' local/remote pair), and area/asn, which nothing here covered until
// testing found the pair indistinguishable.
//
// The area and asn cases are the ones that need the direct/API DeepEqual
// below as well as their own match/miss halves. A transposition inside
// LSPrefixFilter.predicates makes both transports wrong in the same way, so
// only the match assertion catches it; a transposition in
// handleLSPrefixes' filter literal makes only the API transport wrong, so
// the DeepEqual catches that one. See cliLSOtherAreaPrefix for the fixture
// row without which neither could.
func TestBothSourcesAgreeOnLinkStatePrefixes(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, testDB)
	insertCLILSFixture(t, ctx, conn)
	direct, viaAPI := bothSources(t)

	live, err := direct.LSPrefixes(ctx, LSQuery{})
	if err != nil {
		t.Fatalf("discovering the fixture's node key: %v", err)
	}
	var nodeKey string
	for _, p := range live {
		if p.Prefix == cliLSPrefix {
			nodeKey = strconv.FormatUint(p.NodeKey, 10)
			break
		}
	}
	if nodeKey == "" {
		t.Fatalf("fixture row %s not found among live prefixes: %+v", cliLSPrefix, live)
	}

	for _, tc := range []struct {
		name  string
		match LSQuery
		miss  LSQuery
	}{
		{"prefix", LSQuery{Prefix: cliLSPrefix}, LSQuery{Prefix: "10.90.99.0/24"}},
		{"covers", LSQuery{Covers: cliLSCoversAddr}, LSQuery{Covers: "10.90.99.5"}},
		{"node", LSQuery{Node: nodeKey}, LSQuery{Node: "1"}},
		// area and asn, each in both of its values, because the pair is
		// only distinguishable while at least one row differs in both
		// columns -- see cliLSOtherAreaPrefix for the two transpositions
		// these four cases exist to fail. The misses are values no
		// ls_prefixes row carries in either column.
		{"area-zero", LSQuery{Area: "0"}, LSQuery{Area: "42"}},
		{"area-other", LSQuery{Area: strconv.FormatUint(uint64(cliLSRemoteArea), 10)},
			LSQuery{Area: "42"}},
		{"asn", LSQuery{ASN: strconv.FormatUint(uint64(cliLSASN), 10)},
			LSQuery{ASN: "424242"}},
		{"asn-other", LSQuery{ASN: strconv.FormatUint(uint64(cliLSRemoteASN), 10)},
			LSQuery{ASN: "424242"}},
		{"state", LSQuery{State: "withdrawn"}, LSQuery{State: "withdrawn", Area: "9"}},
		{"router", LSQuery{Router: netip.MustParseAddr(cliLSRouterIP)},
			LSQuery{Router: netip.MustParseAddr(cliLSOffRouterIP)}},
		{"peer", LSQuery{Peer: netip.MustParseAddr(cliLSPeerIP)},
			LSQuery{Peer: netip.MustParseAddr(cliLSOffPeerIP)}},
		{"rib", LSQuery{RIB: "in_pre"}, LSQuery{RIB: cliLSOffRIB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, src := range []struct {
				name string
				s    source
			}{{"direct", direct}, {"api", viaAPI}} {
				t.Run(src.name, func(t *testing.T) {
					hit, err := src.s.LSPrefixes(ctx, tc.match)
					if err != nil {
						t.Fatalf("matching: %v", err)
					}
					if len(hit) == 0 {
						t.Errorf("%+v returned no prefixes, but the fixture matches it",
							tc.match)
					}
					none, err := src.s.LSPrefixes(ctx, tc.miss)
					if err != nil {
						t.Fatalf("non-matching: %v", err)
					}
					if len(none) != 0 {
						t.Errorf("%+v matches no fixture row but returned %d prefixes "+
							"-- the flag never reached a predicate", tc.miss, len(none))
					}
				})
			}
			a, err := direct.LSPrefixes(ctx, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			b, err := viaAPI.LSPrefixes(ctx, tc.match)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("sources disagree:\n\tdirect: %+v\n\tapi:    %+v", a, b)
			}
		})
	}
}

// TestProtocolLabelKeepsTheNumberWhenThereIsNoName pins the rule held on
// the table renderer: an unrecognized BGP-LS Protocol-ID is a value this
// project cannot NAME, not a value it does not have, so the number beside
// the empty name is the whole truth and the table has to print it.
//
// The three ls tables used to render orDash(query.ProtocolName(id)), which
// turned every unassigned or future Protocol-ID into "-" and threw the only
// fact the row carried away -- while -o json, which carries protocol and
// protocol_name side by side, kept it. A production archive was observed
// to hold only IDs 1, 2, 3 and 6, all named, so nothing on real data would
// ever have shown the defect.
func TestProtocolLabelKeepsTheNumberWhenThereIsNoName(t *testing.T) {
	// 2 is isis-l2, a name this project knows; 99 is unassigned in the IANA
	// registry and is what a future Protocol-ID looks like from here.
	if got := protocolLabel(2); got != "isis-l2" {
		t.Errorf("protocolLabel(2) = %q, want isis-l2", got)
	}
	got := protocolLabel(99)
	if got == "-" || got == "" {
		t.Fatalf("protocolLabel(99) = %q -- an ID with no name must still "+
			"print its number, which is the whole truth about the row", got)
	}
	if got != "99" {
		t.Errorf("protocolLabel(99) = %q, want 99 -- ParseProtocol accepts a "+
			"decimal ID, so what this prints has to be what -protocol takes "+
			"back", got)
	}
}

// TestQueryLSRefusesAnUnknownState: the -dsn path has no handler to refuse
// it, so the CLI must, while parsing flags and before dialing.
func TestQueryLSRefusesAnUnknownState(t *testing.T) {
	err := cmdQuery([]string{"ls", "nodes", "-state", "livee", "-dsn", "clickhouse://x/y"})
	if err == nil {
		t.Fatal("-state livee was accepted")
	}
	if !strings.Contains(err.Error(), "withdrawn") {
		t.Errorf("the refusal should name the states that do work; got %v", err)
	}
}

// TestQueryLSRefusesBadStateOnLinksAndPrefixes is
// TestQueryLSRefusesAnUnknownState run against the other two verbs, proving
// -state's pre-dial refusal is a property of validateLSQuery -- shared by
// all three cmdQueryLS* subcommands -- rather than something wired into
// "ls nodes" alone.
func TestQueryLSRefusesBadStateOnLinksAndPrefixes(t *testing.T) {
	for _, verb := range []string{"links", "prefixes"} {
		t.Run(verb, func(t *testing.T) {
			err := cmdQuery([]string{"ls", verb, "-state", "livee", "-dsn", "clickhouse://x/y"})
			if err == nil {
				t.Fatalf("-state livee was accepted on ls %s", verb)
			}
			if !strings.Contains(err.Error(), "withdrawn") {
				t.Errorf("the refusal should name the states that do work; got %v", err)
			}
		})
	}
}

// TestQueryLSRefusesGarbageArea is area-zero's failure mode from the other
// side: -area has to distinguish "0" from "not a number" as well as from
// "absent", and a malformed value must be refused before f.source dials --
// the -dsn path has no handler in front of it to refuse it otherwise.
func TestQueryLSRefusesGarbageArea(t *testing.T) {
	err := cmdQuery([]string{"ls", "nodes", "-area", "not-a-number", "-dsn", "clickhouse://x/y"})
	if err == nil {
		t.Fatal("-area not-a-number was accepted")
	}
	if !strings.Contains(err.Error(), "area") {
		t.Errorf("the refusal should name the flag that failed; got %v", err)
	}
}

// TestQueryLSRejectsAnUnknownVerb: cmdQueryLS dispatches on args[0] the way
// cmdQuery does, and an unknown one must not silently no-op.
func TestQueryLSRejectsAnUnknownVerb(t *testing.T) {
	if err := cmdQuery([]string{"ls", "topology"}); err == nil {
		t.Fatal("an unknown ls subcommand was accepted")
	}
}

// TestRIBWalkDefaultsToTheLargestPage pins the page size a `vantage query rib`
// with no -limit walks at.
//
// It matters more than a default usually would, because of a property of the
// query rather than of this flag: a RIB page costs a whole-peer argMax
// recompute REGARDLESS of limit. Measured 2026-09-04 against a 1,000,000-route
// peer, a page took 487ms at limit=1000 and 495ms at limit=10000 -- the same
// work for ten times the rows (see docs/measurements.md, "Collector and
// writer load test").
//
// A CLI walk follows every page to the end by design (see RIBQuery.Limit), so
// it pays that cost once per page for the whole RIB. At the server's
// interactive default of 1000 that is 1,000 whole-RIB aggregations and about
// eight minutes for one full-table peer; at MaxRIBPage it is 100 of them and
// about fifty seconds, for identical per-page cost. The interactive default
// stays where it is -- an API caller asking one question should not be handed
// ten thousand rows -- and only the walker, which is explicitly asking for
// everything, opts up.
func TestRIBWalkDefaultsToTheLargestPage(t *testing.T) {
	if got := ribWalkLimit(0); got != query.MaxRIBPage {
		t.Errorf("ribWalkLimit(0) = %d, want %d (query.MaxRIBPage): a page costs "+
			"a whole-peer recompute whatever the limit, so a walk that follows "+
			"every page has no reason to ask for a small one", got, query.MaxRIBPage)
	}
	// An operator who names a size still gets it: the flag is documented as a
	// round-trip tuning knob, and overriding it here would make it a lie.
	if got := ribWalkLimit(250); got != 250 {
		t.Errorf("ribWalkLimit(250) = %d, want 250 -- an explicit -limit must win", got)
	}
}
