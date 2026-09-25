// GET /v1/collectors' own tests. Most of this file requires a live
// ClickHouse through chtest.Require and SKIPS (not fails) without one, the
// same as every other handler test in this package -- see
// api/handlers_test.go's own header. TestCollectorsCarriesNoLagField and
// TestCollectorsRetentionReadFailureOmitsTheField are the exceptions, and
// each says why.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/jp2195/vantage/chtest"
	"github.com/jp2195/vantage/query"
	"github.com/jp2195/vantage/secret"
	"github.com/jp2195/vantage/status"
)

// collectorsFix* is a private, single-router fixture under its OWN
// collector id -- deliberately never fixCollector. query.Collectors and
// query.CollectorActivity both aggregate at the COLLECTOR grain, and two
// other files in this package (collection_test.go, events_test.go) add MORE
// peer_events rows under fixCollector -- at different router IPs, correctly
// avoiding a ROUTER-level collision, but at the SAME collector id, with
// fresh (time.Now()) timestamps. That is fine for what those files test and
// fatal to a test here that wants to say "this collector has exactly one
// router" or "this collector is quiet right now": neither stays true once
// this test binary also runs those files' own tests against the same
// shared, never-truncated apiTestDB in the same process. A test in THIS
// file that needs a collector-level invariant to hold uses this fixture's
// own id instead.
const (
	collectorsFixCollector = "collectors-test-collector"
	collectorsFixSysName   = "collectors-test-router"
	collectorsFixRouterIP  = "10.96.0.1"
	collectorsFixPeerA     = "10.96.0.2"
	collectorsFixPeerB     = "10.96.0.3"
	collectorsFixSession   = 777001
)

// collectorsFixBusyCollector is a SECOND private id, distinct from
// collectorsFixCollector, under which insertFreshPeerEvent writes one
// peer_events row with ts_collector = time.Now() -- inside
// collectorActivityWindow, on purpose. It has to be a different id than
// collectorsFixCollector: that fixture's whole point is staying quiet
// inside the window (see TestCollectorsGivesAQuietCollectorAZeroFilled
// ActivitySeries), and a fresh row under the same id would break it.
const (
	collectorsFixBusyCollector = "collectors-test-busy-collector"
	collectorsFixBusyRouterIP  = "10.96.1.1"
	collectorsFixBusyPeerIP    = "10.96.1.2"
)

// collectorsFixBusySeq is a per-process counter so a repeated run of this
// test binary never collides with an earlier run's own (session_id, seq)
// key under collectorsFixBusyCollector -- mirrors collFixSeq in
// collection_test.go for the identical reason: this database is shared and
// never truncated between runs.
var collectorsFixBusySeq atomic.Uint64

// insertFreshPeerEvent writes ONE peer_events row under
// collectorsFixBusyCollector with ts_collector = time.Now(), so
// query.CollectorActivity has a genuinely non-empty series to return for
// this id -- the branch of wireCollectorActivitySeries
// (mapRows(series, NewWireCollectorActivity)) that copies a REAL series
// onto the wire, which nothing else in this file exercises: every other
// activity assertion here is against a collector that is deliberately
// quiet inside the window.
func insertFreshPeerEvent(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	n := collectorsFixBusySeq.Add(1)
	session := uint64(900000) + n
	ts := time.Now().UTC()

	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	if err := batch.Append(
		collectorsFixBusyCollector, netip.MustParseAddr(collectorsFixBusyRouterIP), "busy-router",
		netip.MustParseAddr(collectorsFixBusyPeerIP), "in_pre", uint32(65001),
		netip.MustParseAddr(collectorsFixBusyRouterIP), session, n,
		ts, ts, []string{}, n,
		"up", netip.MustParseAddr(collectorsFixBusyRouterIP), uint16(179), uint16(50000),
		uint32(0), uint8(1),
	); err != nil {
		t.Fatalf("append peer_events: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
}

// insertCollectorsFixture writes one router, two up peers, under
// collectorsFixCollector -- insertHandlerFixture's own shape, at this
// file's own private coordinates, with the same old (2026-08-01) timestamp
// insertHandlerFixture uses, so this collector is guaranteed quiet inside
// collectorActivityWindow's 30-minute lookback no matter when a test here
// actually runs. Idempotent, like insertHandlerFixture: ReplacingMergeTree
// dedups on each row's own key, so re-running this file's tests never
// accumulates a second router under this id.
func insertCollectorsFixture(t *testing.T, ctx context.Context, conn driver.Conn) {
	t.Helper()
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	peers, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".peer_events "+peerEventsCols)
	if err != nil {
		t.Fatalf("prepare peer_events: %v", err)
	}
	var seq uint64
	for _, p := range []string{collectorsFixPeerA, collectorsFixPeerB} {
		seq++
		if err := peers.Append(
			collectorsFixCollector, netip.MustParseAddr(collectorsFixRouterIP), collectorsFixSysName,
			netip.MustParseAddr(p), "in_pre", uint32(65001),
			netip.MustParseAddr(collectorsFixRouterIP), uint64(collectorsFixSession), seq,
			ts, ts, []string{}, seq,
			"up", netip.MustParseAddr(collectorsFixRouterIP), uint16(179), uint16(50000),
			uint32(0), uint8(1),
		); err != nil {
			t.Fatalf("append peer_events: %v", err)
		}
	}
	if err := peers.Send(); err != nil {
		t.Fatalf("send peer_events: %v", err)
	}
}

// requireAPICollectors is requireAPIBuilt's own shape (api/handlers_test.go)
// with two more Config fields a caller actually varies here: Collectors and
// CollectorsTimeout. It loads both the package's shared fixture
// (insertHandlerFixture, under fixCollector) and this file's own private one
// (insertCollectorsFixture, under collectorsFixCollector), so every test in
// this file can pick whichever id its own assertions actually need: the
// shared one when only archive-vs-config PRESENCE matters, this file's own
// when an exact router count or a quiet activity window matters too.
func requireAPICollectors(t *testing.T, collectors []CollectorEndpoint, timeout time.Duration) *Server {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	insertHandlerFixture(t, ctx, conn)
	insertCollectorsFixture(t, ctx, conn)
	q, err := query.New(conn, apiTestDB)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	s, err := NewServer(q, Config{
		DefaultPage:       1000,
		MaxPage:           10000,
		MaxUnscopedSince:  24 * time.Hour,
		Tokens:            []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
		Collectors:        collectors,
		CollectorsTimeout: timeout,
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// fakeCollectorServer starts an httptest server that answers /status with
// rep, closed automatically at test cleanup, for use as a
// CollectorEndpoint.URL.
func fakeCollectorServer(t *testing.T, rep status.Report) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rep)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// deadEndpoint returns a URL nothing is listening on -- connection refused,
// immediately, no timeout wait -- mirroring collectorstatus_test.go's own
// dead listener, for a CollectorEndpoint that must never answer.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()
	return url
}

// findCollector returns the row for id, or fails the test: every test in
// this file asks about a row it knows must be present in the union.
func findCollector(t *testing.T, rows []WireCollector, id string) WireCollector {
	t.Helper()
	for _, r := range rows {
		if r.Collector == id {
			return r
		}
	}
	t.Fatalf("no row for collector %q; got %d rows: %+v", id, len(rows), rows)
	return WireCollector{}
}

// TestCollectorsUnionsTheArchiveAndTheConfig: four cases in one table --
// in both, archive-only, config-only, and config-only-and-unreachable. Each
// renders as a card with a DIFFERENT explanation, and collapsing any two of
// them is the screen lying about what it knows.
//
// Every case shares the same archive fixture (fixCollector, with one router
// and two up peers -- insertHandlerFixture) and varies only Config.Collectors,
// so the archive side of the union never moves across cases; only the
// configured side does, and with it, which of the four states the target row
// lands in.
func TestCollectorsUnionsTheArchiveAndTheConfig(t *testing.T) {
	cases := []struct {
		name          string
		collectors    func(t *testing.T) []CollectorEndpoint
		target        string
		wantArchive   bool
		wantStatus    bool
		wantReachable *bool
		wantErr       bool
	}{
		{
			name: "in the archive and answering",
			collectors: func(t *testing.T) []CollectorEndpoint {
				return []CollectorEndpoint{{ID: fixCollector, URL: fakeCollectorServer(t, status.Report{
					CollectorID: fixCollector, SessionsActive: 3, BMPMessagesTotal: 42,
				})}}
			},
			target:        fixCollector,
			wantArchive:   true,
			wantStatus:    true,
			wantReachable: new(true),
		},
		{
			name: "in the archive, no endpoint configured",
			collectors: func(t *testing.T) []CollectorEndpoint {
				return nil
			},
			target:        fixCollector,
			wantArchive:   true,
			wantStatus:    false,
			wantReachable: nil,
		},
		{
			name: "in the archive, configured but unreachable",
			collectors: func(t *testing.T) []CollectorEndpoint {
				return []CollectorEndpoint{{ID: fixCollector, URL: deadEndpoint(t)}}
			},
			target:        fixCollector,
			wantArchive:   true,
			wantStatus:    false,
			wantReachable: new(false),
			wantErr:       true,
		},
		{
			name: "configured only, not in the archive",
			collectors: func(t *testing.T) []CollectorEndpoint {
				return []CollectorEndpoint{{ID: "ghost-collector", URL: fakeCollectorServer(t, status.Report{
					CollectorID: "ghost-collector",
				})}}
			},
			target:        "ghost-collector",
			wantArchive:   false,
			wantStatus:    true,
			wantReachable: new(true),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := requireAPICollectors(t, tc.collectors(t), time.Second)
			var got []WireCollector
			getOK(t, s, "/v1/collectors", &got)
			row := findCollector(t, got, tc.target)

			if (row.Archive != nil) != tc.wantArchive {
				t.Errorf("archive present = %v, want %v", row.Archive != nil, tc.wantArchive)
			}
			if (row.Status != nil) != tc.wantStatus {
				t.Errorf("status present = %v, want %v", row.Status != nil, tc.wantStatus)
			}
			if (row.Reachable == nil) != (tc.wantReachable == nil) {
				t.Fatalf("reachable = %v, want nil-ness %v", row.Reachable, tc.wantReachable == nil)
			}
			if tc.wantReachable != nil && *row.Reachable != *tc.wantReachable {
				t.Errorf("reachable = %v, want %v", *row.Reachable, *tc.wantReachable)
			}
			if tc.wantErr && row.Error == "" {
				t.Error("wanted a reason for unreachability, got none")
			}
			if !tc.wantErr && row.Error != "" {
				t.Errorf("unexpected error %q", row.Error)
			}
		})
	}
}

// TestCollectorsShowsTheArchivesOwnRouterForTheAnsweringCase pins the shape
// WireCollectorArchive.Routers carries, which the table test above only
// checks for presence: this proves the router the fixture actually wrote
// (collectorsFixSysName, two up peers) survives the union unchanged.
//
// It asks about collectorsFixCollector, not fixCollector: see this file's
// own doc comment on collectorsFixCollector for why an exact router count
// is not a safe thing to assert against the package's SHARED fixture id.
func TestCollectorsShowsTheArchivesOwnRouterForTheAnsweringCase(t *testing.T) {
	s := requireAPICollectors(t, []CollectorEndpoint{
		{ID: collectorsFixCollector, URL: fakeCollectorServer(t, status.Report{CollectorID: collectorsFixCollector})},
	}, time.Second)
	var got []WireCollector
	getOK(t, s, "/v1/collectors", &got)
	row := findCollector(t, got, collectorsFixCollector)

	if row.Archive == nil {
		t.Fatal("archive = nil, want the fixture's router")
	}
	if len(row.Archive.Routers) != 1 {
		t.Fatalf("len(archive.routers) = %d, want 1", len(row.Archive.Routers))
	}
	rtr := row.Archive.Routers[0]
	if rtr.SysName != collectorsFixSysName {
		t.Errorf("routers[0].sysname = %q, want %q", rtr.SysName, collectorsFixSysName)
	}
	if rtr.PeersUp != 2 {
		t.Errorf("routers[0].peers_up = %d, want 2 (both fixture peers are up)", rtr.PeersUp)
	}
	if row.Archive.PeersUp != 2 {
		t.Errorf("archive.peers_up = %d, want 2", row.Archive.PeersUp)
	}
}

// TestCollectorsNeverReportsZerosForAnUnconfiguredCollector: a collector with
// no endpoint has NO uptime and NO message count -- not zero of each. status is
// null, and the card says "no status endpoint configured". A zero here reads as
// a collector that has been up for no time and has read nothing, which is the
// description of a broken one.
func TestCollectorsNeverReportsZerosForAnUnconfiguredCollector(t *testing.T) {
	s := requireAPICollectors(t, nil, time.Second)
	rec := get(t, s, "/v1/collectors")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/collectors = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	var env struct {
		Data []WireCollector `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body %s", err, body)
	}
	row := findCollector(t, env.Data, fixCollector)

	if row.Status != nil {
		t.Fatalf("status = %+v, want nil: no endpoint is configured for %q", row.Status, fixCollector)
	}
	if row.Reachable != nil {
		t.Errorf("reachable = %v, want nil: reachable is not a question when nothing is configured", *row.Reachable)
	}

	// The raw wire bytes, not just the decoded Go zero value: a pointer
	// field decodes to nil from an absent key just as readily as from a
	// present null, so this is the assertion that actually distinguishes
	// "the key is null" from "the key was never emitted at all" -- and it
	// is what would catch WireCollectorStatus ever losing its pointer and
	// starting to marshal as a real, zero-valued object.
	if strings.Contains(body, "sessions_active") {
		t.Errorf("response carries sessions_active with no endpoint configured; "+
			"a zero here would read as a broken collector, not an unconfigured one: %s", body)
	}
}

// TestCollectorsSurfacesAnIDMismatchRatherThanResolvingItSilently: a daemon
// answering under a DIFFERENT collector_id than its own config entry names
// means someone pointed this card's endpoint at another collector's
// process. collectorStatuses keeps both ids -- the configured one as its
// key, the daemon's own in ReportedID (see its own doc comment) -- and this
// proves the wire form carries that same disagreement rather than resolving
// it to one side.
//
// BOTH rows must exist and say DIFFERENT things: the reported id
// ("actually-someone-else") is in no config file and, on this fixture, in
// no archive either, so it exists only because handleCollectors synthesizes
// a row for it; and the configured id ("configured-name") must not quietly
// inherit that daemon's process facts just because its own endpoint is
// where they came from. Asserting the opposite (failing if any row was
// keyed on "configured-name") would let a defect ship where the configured
// id had no row at all: a test that locks in the wrong side of a union can
// pass forever.
func TestCollectorsSurfacesAnIDMismatchRatherThanResolvingItSilently(t *testing.T) {
	srv := fakeCollectorServer(t, status.Report{CollectorID: "actually-someone-else"})
	s := requireAPICollectors(t, []CollectorEndpoint{{ID: "configured-name", URL: srv}}, time.Second)

	var got []WireCollector
	getOK(t, s, "/v1/collectors", &got)

	reported := findCollector(t, got, "actually-someone-else")
	if reported.IDMismatch != "configured-name" {
		t.Errorf("reported row id_mismatch = %q, want the configured id %q", reported.IDMismatch, "configured-name")
	}
	if reported.Status == nil {
		t.Error("reported row status = nil, want the daemon's report -- it DID answer, just under a different id")
	}
	if reported.Reachable == nil || !*reported.Reachable {
		t.Errorf("reported row reachable = %v, want true: the endpoint answered", reported.Reachable)
	}

	// The configured-name row has to read as CONFIGURED BUT NOT ITSELF --
	// reachable: false with a reason -- never as unconfigured (which would
	// be byte-identical to a card with no endpoint at all) and never as a
	// silent duplicate of the reported row's own status.
	configured := findCollector(t, got, "configured-name")
	if configured.Status != nil {
		t.Errorf("configured row status = %+v, want nil: this id's own poll never landed under its own name", configured.Status)
	}
	if configured.Reachable == nil {
		t.Fatal("configured row reachable = nil, want false: an endpoint IS configured for this id, unlike a truly unconfigured one")
	}
	if *configured.Reachable {
		t.Error("configured row reachable = true, want false: this id's own daemon never answered as itself")
	}
	if configured.Error == "" {
		t.Error("configured row error = \"\", want a reason naming the mismatch")
	}
	if configured.Endpoint == "" {
		t.Error("configured row endpoint = \"\", want the configured URL")
	}
	if configured.IDMismatch != "" {
		t.Errorf("configured row id_mismatch = %q, want \"\": the disagreement is already "+
			"stated on the reported row, under the daemon's own authoritative id", configured.IDMismatch)
	}
}

// TestCollectorsKeepsEveryConfiguredIDWhenTwoDaemonsReportTheSameID: every
// configured id gets a row, under EVERY combination of daemon answers --
// including the one where two configured entries' daemons answer with the
// same collector_id.
//
// validate() (api/config.go) rejects a duplicate CONFIGURED id; nothing
// constrains what two daemons report about themselves, and two ways of
// arriving at this are ordinary deployment mistakes rather than exotic
// ones: two entries pointed at one pod, or the bare headless Service name
// pasted twice, which resolves round-robin to whichever pod answers. Keyed
// on the reported id -- which collectorStatuses did until this test existed
// -- the two polls landed on ONE map entry, last write wins, and the losing
// configured entry ended up with no status entry under any name. It then
// fell through newWireCollector's "no endpoint configured" branch and
// serialized BYTE-IDENTICALLY to a collector nobody had set up: a config
// fact stated as an absence, flipping between polls with the race.
//
// Both sub-cases assert the losing row is not that: an endpoint, reachable
// false, and a reason. "Configured but not itself" is a third state, and it
// is the one an operator can act on.
func TestCollectorsKeepsEveryConfiguredIDWhenTwoDaemonsReportTheSameID(t *testing.T) {
	// A distinguishing fact on each poll's own answer, so a row that
	// inherited the WRONG endpoint's report is visible rather than merely
	// plausible.
	reportsAs := func(t *testing.T, id string, sessions int64) string {
		return fakeCollectorServer(t, status.Report{CollectorID: id, SessionsActive: sessions})
	}

	t.Run("both daemons report an id no config entry names", func(t *testing.T) {
		s := requireAPICollectors(t, []CollectorEndpoint{
			{ID: "dup-first", URL: reportsAs(t, "dup-reported", 11)},
			{ID: "dup-second", URL: reportsAs(t, "dup-reported", 22)},
		}, time.Second)
		var got []WireCollector
		getOK(t, s, "/v1/collectors", &got)

		for _, id := range []string{"dup-first", "dup-second"} {
			row := findCollector(t, got, id)
			if row.Status != nil {
				t.Errorf("%s: status = %+v, want nil: its endpoint answered as someone else, "+
					"so those process facts are not this id's", id, row.Status)
			}
			if row.Reachable == nil {
				t.Fatalf("%s: reachable = nil -- byte-identical to a collector with NO endpoint "+
					"configured, while one IS configured for this id", id)
			}
			if *row.Reachable {
				t.Errorf("%s: reachable = true, want false: this id's daemon never answered as itself", id)
			}
			if row.Error == "" {
				t.Errorf("%s: error = \"\", want a reason naming the disagreement", id)
			}
			if row.Endpoint == "" {
				t.Errorf("%s: endpoint = \"\", want the configured URL", id)
			}
		}

		// The reported id gets exactly ONE row, attributed to the FIRST
		// config entry that reported it -- first in the config file, not in
		// Go's map order, so two requests never disagree about whose
		// endpoint this is.
		reported := findCollector(t, got, "dup-reported")
		if reported.IDMismatch != "dup-first" {
			t.Errorf("reported row id_mismatch = %q, want %q -- the first config entry that "+
				"reported this id, deterministically", reported.IDMismatch, "dup-first")
		}
		if reported.Status == nil || reported.Status.SessionsActive != 11 {
			t.Errorf("reported row status = %+v, want the FIRST entry's own answer "+
				"(sessions_active 11)", reported.Status)
		}
		var rows int
		for _, r := range got {
			if r.Collector == "dup-reported" {
				rows++
			}
		}
		if rows != 1 {
			t.Errorf("%d rows for the reported id, want exactly 1", rows)
		}
	})

	t.Run("one daemon reports another configured entry's id", func(t *testing.T) {
		s := requireAPICollectors(t, []CollectorEndpoint{
			{ID: "dup-impostor", URL: reportsAs(t, "dup-real", 33)},
			{ID: "dup-real", URL: reportsAs(t, "dup-real", 44)},
		}, time.Second)
		var got []WireCollector
		getOK(t, s, "/v1/collectors", &got)

		impostor := findCollector(t, got, "dup-impostor")
		if impostor.Reachable == nil {
			t.Fatal("dup-impostor: reachable = nil -- byte-identical to a collector with NO " +
				"endpoint configured, while one IS configured for this id")
		}
		if *impostor.Reachable {
			t.Error("dup-impostor: reachable = true, want false: its daemon answered as dup-real")
		}
		if impostor.Status != nil {
			t.Errorf("dup-impostor: status = %+v, want nil", impostor.Status)
		}
		if impostor.Error == "" {
			t.Error("dup-impostor: error = \"\", want a reason naming dup-real")
		}

		// dup-real's OWN poll answers for dup-real's row -- not the
		// impostor's, which happens to report the same name. A configured
		// id's own endpoint is the authority on its own card.
		real := findCollector(t, got, "dup-real")
		if real.Reachable == nil || !*real.Reachable {
			t.Fatalf("dup-real: reachable = %v, want true: its own endpoint answered", real.Reachable)
		}
		if real.Status == nil || real.Status.SessionsActive != 44 {
			t.Errorf("dup-real: status = %+v, want its OWN endpoint's answer (sessions_active 44), "+
				"not the impostor's", real.Status)
		}
		if real.IDMismatch != "" {
			t.Errorf("dup-real: id_mismatch = %q, want \"\": this id IS what its own daemon "+
				"reports; the disagreement belongs on dup-impostor's row", real.IDMismatch)
		}
	})
}

// TestCollectorsNeverReadsAForeignEndpointAsACollectorCountingZero: a url
// pointed at something that is not a vantage-collector -- another service's
// health endpoint, a different daemon, anything that answers 200 with a JSON
// object -- must not produce a HEALTHY card.
//
// encoding/json leaves an absent field at its zero value, so such a body
// decodes into a zero status.Report with no error at all. Attributed to the
// configured id, that renders as reachable:true with a full set of zero
// process tiles and NO reason: uptime 0, no messages, nothing published --
// the description of a broken collector, produced by a url that was never
// pointing at a collector in the first place. It is the same inversion
// TestCollectorsNeverReportsZerosForAnUnconfiguredCollector guards from the
// other direction, and it is the class of deployment mistake this whole
// endpoint's id checking exists to catch, so a zero-filled healthy card is
// the worst answer available.
//
// The card must read configured-but-not-answering, with a reason that names
// what came back.
func TestCollectorsNeverReadsAForeignEndpointAsACollectorCountingZero(t *testing.T) {
	// A well-formed JSON object with no collector_id -- deliberately not
	// status.Report through fakeCollectorServer, which always emits the
	// field: this is what a foreign endpoint's body actually looks like.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","uptime_seconds":1234}`))
	}))
	t.Cleanup(foreign.Close)

	s := requireAPICollectors(t, []CollectorEndpoint{{ID: "foreign-endpoint", URL: foreign.URL}}, time.Second)
	rec := get(t, s, "/v1/collectors")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/collectors = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []WireCollector `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	row := findCollector(t, env.Data, "foreign-endpoint")

	if row.Status != nil {
		t.Errorf("status = %+v, want nil: this endpoint never said it was a collector, so "+
			"none of these numbers are this collector's", row.Status)
	}
	if row.Reachable == nil {
		t.Fatal("reachable = nil, want false: an endpoint IS configured for this id")
	}
	if *row.Reachable {
		t.Error("reachable = true, want false: the endpoint answered, but not as a collector -- " +
			"true here puts a HEALTHY badge on a url that is not a collector at all")
	}
	if row.Error == "" {
		t.Error("error = \"\", want a reason naming what came back instead of a collector_id")
	}
	if row.IDMismatch != "" {
		t.Errorf("id_mismatch = %q, want \"\": there is no other id to name -- this endpoint "+
			"stated none, which is not the same as stating a different one", row.IDMismatch)
	}

	// The raw bytes, not the decoded Go value: the defect this guards is a
	// zero status object marshaled onto the wire, and a nil *WireCollectorStatus
	// and an all-zero one decode identically into row.Status == nil only
	// because one of them is absent. See
	// TestCollectorsNeverReportsZerosForAnUnconfiguredCollector's identical
	// reasoning.
	if body := rec.Body.String(); strings.Contains(body, "sessions_active") {
		t.Errorf("response carries sessions_active for an endpoint that never identified "+
			"itself as a collector; a zero here reads as a broken collector rather than "+
			"as a misconfigured url: %s", body)
	}

	// And the row still exists at all: the configured id keeps its card,
	// with the reason on it, rather than vanishing -- which is what the
	// whole configured-id union is for.
	if row.Endpoint == "" {
		t.Error("endpoint = \"\", want the configured URL so the card can say which url this was")
	}
}

// lagKey matches a JSON key named "lag" or carrying "lag" as one of its
// underscore-delimited segments -- "consumer_lag", "lag_seconds",
// "sink_consumer_lag" -- and NOT "flags" or "parse_flags", which merely
// contain the three letters. The trailing colon is what makes it a key: a
// string VALUE reading "lag" is not this defect.
var lagKey = regexp.MustCompile(`"(?:[a-z0-9]+_)*lag(?:_[a-z0-9]+)*"\s*:`)

// TestCollectorsCarriesNoLagField: the writer's consumer lag is per stream and
// shared across every collector; there is no per-collector lag to report. This
// test asserts the ABSENCE of the field in the marshaled JSON, because the way
// this defect returns is someone wiring vantage_sink_consumer_lag into a card
// that has a slot waiting for it.
//
// It matches a JSON KEY whose name has "lag" as an underscore-delimited
// segment, not the bare substring "lag" this used to look for: "flags" and
// "parse_flags" both contain it, so the old needle was satisfied by fields
// that have nothing to do with consumer lag and would have failed this test
// the day one of them reached this response. Deliberately wider than the
// exact key "lag": "consumer_lag" and "lag_seconds" are the names this
// defect would actually arrive under, and the point is to catch the field,
// not one spelling of it.
//
// It marshals the wire types directly, with every field populated, rather
// than going through a live Server: this is the one assertion of absence in
// this file, and TestAuthConfigLeaksNoToken's own doc comment (in
// api/handlers_test.go) gives the reason it must not be a test that SKIPS.
// Every other test here requires a live ClickHouse through chtest.Require
// and quietly skips without one; this one runs even where there is no
// database reachable at all.
func TestCollectorsCarriesNoLagField(t *testing.T) {
	reachable := true
	row := WireCollector{
		Collector: "dev-c1",
		Archive: &WireCollectorArchive{
			Routers: []WireCollectorRouter{{
				SysName: "r1", IP: "10.0.0.1", PeersUp: 1, PeersDown: 0, PeersViewLost: 0,
				SysDescr: "Cisco IOS XR", LastSeen: time.Now(),
			}},
			PeersUp: 1, PeersDown: 0, PeersViewLost: 0, LastRowAt: time.Now(),
		},
		Status: &WireCollectorStatus{
			StartedAt: time.Now(), ObservedAt: time.Now(),
			SessionsActive: 3, BMPMessagesTotal: 100,
			EventsPublishedTotal: 99, PublishErrorsTotal: 1, PublishRejectsTotal: 2,
		},
		Reachable: &reachable,
		Endpoint:  "http://collector:9469",
		Activity:  []WireCollectorActivity{{Minute: time.Now(), Rows: 5}},
	}
	window := collectorActivityWindow.String()
	b, err := json.Marshal(Envelope{Data: []WireCollector{row}, Meta: Meta{ActivityWindow: &window}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if m := lagKey.Find(b); m != nil {
		t.Fatalf("response carries the key %s; there is no per-collector lag to report "+
			"(vantage_sink_consumer_lag is the writer's, per stream): %s", m, b)
	}
}

// TestCollectorsGivesAQuietCollectorAZeroFilledActivitySeries: a collector
// that archived zero rows anywhere in query.CollectorActivity's window has
// NO key at all in the map that method returns -- see its own doc comment.
// This endpoint has to pair that map against query.Collectors' own list and
// treat the missing key as an all-zero series, never as a reason to drop the
// card: a quiet collector is exactly the one an operator is looking at this
// screen to find.
//
// insertCollectorsFixture's rows are dated 2026-08-01, long before
// collectorActivityWindow's own 30-minute lookback from whenever this test
// actually runs, so collectorsFixCollector is guaranteed to land in this
// case. fixCollector is NOT used here: see this file's own doc comment on
// collectorsFixCollector for why other files in this package add fresh
// (time.Now()) rows under fixCollector that would make it genuinely busy by
// the time this test's own query.CollectorActivity call runs.
func TestCollectorsGivesAQuietCollectorAZeroFilledActivitySeries(t *testing.T) {
	s := requireAPICollectors(t, nil, time.Second)
	before := time.Now().UTC().Truncate(time.Minute)
	var got []WireCollector
	getOK(t, s, "/v1/collectors", &got)
	after := time.Now().UTC().Truncate(time.Minute)
	row := findCollector(t, got, collectorsFixCollector)

	wantBuckets := int(collectorActivityWindow / time.Minute)
	if len(row.Activity) != wantBuckets {
		t.Fatalf("len(activity) = %d, want %d (one entry per minute of the window)",
			len(row.Activity), wantBuckets)
	}
	for i, a := range row.Activity {
		if a.Rows != 0 {
			t.Errorf("activity[%d].Rows = %d, want 0: this collector archived nothing in the window", i, a.Rows)
		}
		if i > 0 && !a.Minute.Equal(row.Activity[i-1].Minute.Add(time.Minute)) {
			t.Errorf("activity[%d].Minute = %v, want exactly one minute after activity[%d] (%v)",
				i, a.Minute, i-1, row.Activity[i-1].Minute)
		}
	}

	// The newest bucket is the CURRENT, still-accumulating minute, not the
	// last fully-elapsed one -- see (*query.Q).CollectorActivity's own doc
	// comment. now truncated to the minute IS that bucket's own start.
	// before and after bracket the request/response round trip: comparing
	// against both, rather than a single time.Now() call made after getOK
	// returns, is what keeps this assertion from flaking on the rare
	// request that straddles a minute boundary.
	last := row.Activity[len(row.Activity)-1].Minute
	if !last.Equal(before) && !last.Equal(after) {
		t.Errorf("newest bucket = %v, want %v or %v (now truncated to the minute)", last, before, after)
	}
}

// TestCollectorsActivityCarriesRealRowsThroughTheHandler:
// wireCollectorActivitySeries has two branches, and every other test in
// this file that looks at Activity exercises only the zero-fill one (a
// collector query.CollectorActivity's own map has no key for). Mutating the
// real branch away -- always zero-filling, never returning
// mapRows(series, NewWireCollectorActivity) -- would leave the REST of this
// file green while every sparkline on the actual screen reads flat zero for
// every collector: the two nonzero Rows values that appear elsewhere in
// this file (TestCollectorsCarriesNoLagField, the golden-style literals)
// are hand-built and never pass through the handler at all.
//
// This writes one genuinely fresh peer_events row -- ts_collector =
// time.Now(), inside collectorActivityWindow -- under a collector id
// nothing else in this file touches, and proves it reaches the wire as a
// real, non-zero bucket in a full-length series.
func TestCollectorsActivityCarriesRealRowsThroughTheHandler(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	insertFreshPeerEvent(t, ctx, conn)

	s := requireAPICollectors(t, nil, time.Second)
	var got []WireCollector
	getOK(t, s, "/v1/collectors", &got)
	row := findCollector(t, got, collectorsFixBusyCollector)

	wantBuckets := int(collectorActivityWindow / time.Minute)
	if len(row.Activity) != wantBuckets {
		t.Fatalf("len(activity) = %d, want %d: a real series must still cover the whole "+
			"window, not just however many minutes CollectorActivity actually returned",
			len(row.Activity), wantBuckets)
	}

	var total uint64
	for _, a := range row.Activity {
		total += a.Rows
	}
	if total == 0 {
		t.Fatalf("every activity bucket read 0 rows; the fresh peer_events row this test just "+
			"wrote never reached the wire: %+v", row.Activity)
	}
}

// TestCollectorsNamesItsActivityWindowInMeta: "name the window" -- the span
// this endpoint's sparkline covers has to be stated on the wire, not implied
// by a client counting a card's own activity entries or reading its first
// and last minute by hand.
func TestCollectorsNamesItsActivityWindowInMeta(t *testing.T) {
	s := requireAPICollectors(t, nil, time.Second)
	var got []WireCollector
	meta := getOK(t, s, "/v1/collectors", &got)
	if meta.ActivityWindow == nil {
		t.Fatal("meta.activity_window is nil; the window this endpoint covers is not named")
	}
	if *meta.ActivityWindow != collectorActivityWindow.String() {
		t.Errorf("meta.activity_window = %q, want %q", *meta.ActivityWindow, collectorActivityWindow.String())
	}
}

// apiRetentionTestDB is TestCollectorsReportsTheTableRetentionInMeta's own
// database: the test rewrites route_unicast's TTL, which no other test's
// fixture may see.
const apiRetentionTestDB = "vantage_api_retention_test"

// TestCollectorsReportsTheTableRetentionInMeta: meta.retention_days is the
// history retention the archive's own TTL applies, read from the table, so
// a TTL changed in place is reported as changed. A TTL that is not a whole
// number of days is not reported at all rather than guessed at.
func TestCollectorsReportsTheTableRetentionInMeta(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiRetentionTestDB)
	q, err := query.New(conn, apiRetentionTestDB)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	s, err := NewServer(q, Config{
		DefaultPage:       1000,
		MaxPage:           10000,
		MaxUnscopedSince:  24 * time.Hour,
		Tokens:            []Token{{Name: "test", Token: secret.NewAPIToken(testToken)}},
		CollectorsTimeout: time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	modify := func(interval string) {
		t.Helper()
		stmt := "ALTER TABLE " + apiRetentionTestDB + ".route_unicast MODIFY TTL " +
			"toDateTime(ts_collector) + " + interval +
			" SETTINGS materialize_ttl_after_modify = 0"
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if got := getOK(t, s, "/v1/collectors", nil).RetentionDays; got == nil || *got != 90 {
		t.Fatalf("meta.retention_days on the shipped schema = %v, want 90", got)
	}
	modify("INTERVAL 45 DAY")
	if got := getOK(t, s, "/v1/collectors", nil).RetentionDays; got == nil || *got != 45 {
		t.Fatalf("meta.retention_days after MODIFY TTL ... 45 DAY = %v, want 45", got)
	}
	modify("INTERVAL 3 MONTH")
	rec := get(t, s, "/v1/collectors")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/collectors with a TTL in months = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"retention_days"`) {
		t.Fatalf("a TTL in months was reported as a number of days: %s", rec.Body.String())
	}
}

// TestCollectorsRunsTheArchiveQueryAndTheFanOutConcurrently: the archive
// queries are a scan and the fan-out's whole budget is a network timeout;
// running them one after another would make this endpoint wait for their
// SUM. A wedged collector endpoint forces the fan-out to actually spend its
// whole CollectorsTimeout, so a serial implementation costs roughly
// (archive scan time + CollectorsTimeout); a concurrent one costs roughly
// max(archive scan time, CollectorsTimeout).
//
// The bound is measured against a BASELINE request (no collectors
// configured, so the fan-out is instant) rather than a fixed guess: this
// fixture's own archive scan measured 10-70ms across earlier runs, and a
// fixed multiple of the timeout alone (this test's own previous bound,
// 3*timeout = 600ms) could not tell max(archive, 200ms) apart from
// archive+200ms at that scale -- a serial implementation landed at
// 210-270ms, comfortably under a 600ms bound that both hypotheses satisfy.
// Measuring the real archive time on THIS run, on THIS machine, is what
// makes the threshold below actually discriminate the two rather than
// merely being generous enough to pass either way.
func TestCollectorsRunsTheArchiveQueryAndTheFanOutConcurrently(t *testing.T) {
	block := make(chan struct{})
	wedged := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer wedged.Close()
	// Deferred last so it runs FIRST during unwind, before wedged.Close --
	// see collectorstatus_test.go's identical comment on why the order
	// matters (Close blocks on the handler this channel is holding open).
	defer close(block)

	timeout := 200 * time.Millisecond

	// baseline: no collectors configured, so collectorStatuses returns
	// instantly and this measures query.Collectors' and
	// query.CollectorActivity's own combined scan time alone, on this run.
	baseline := requireAPICollectors(t, nil, timeout)
	baselineStart := time.Now()
	get(t, baseline, "/v1/collectors")
	archiveTime := time.Since(baselineStart)

	s := requireAPICollectors(t, []CollectorEndpoint{{ID: "wedged", URL: wedged.URL}}, timeout)

	start := time.Now()
	rec := get(t, s, "/v1/collectors")
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/collectors = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	// want sits strictly between the two hypotheses: a concurrent
	// implementation costs timeout plus a little scheduling overhead
	// (comfortably under timeout + archiveTime/2 as long as that overhead
	// stays below half of archiveTime), while a serial one pays archiveTime
	// IN FULL on top of timeout -- not half of it -- so it exceeds this
	// bound by construction, regardless of how small archiveTime is.
	want := timeout + archiveTime/2
	if elapsed > want {
		t.Errorf("took %v (archive alone took %v, fan-out timeout %v); want under %v -- "+
			"the archive query and the fan-out ran in series, not concurrently",
			elapsed, archiveTime, timeout, want)
	}
}

// TestCollectorsRetentionReadFailureOmitsTheField: a failed retention read
// omits meta.retention_days and is logged, rather than failing the request;
// a TTL that is not a whole number of days is omitted without a log line.
func TestCollectorsRetentionReadFailureOmitsTheField(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))}

	if got := s.retentionForMeta(30, nil); got == nil || *got != 30 {
		t.Fatalf("retentionForMeta(30, nil) = %v, want 30", got)
	}
	if got := s.retentionForMeta(0, query.ErrRetentionNotInDays); got != nil {
		t.Fatalf("retentionForMeta(ErrRetentionNotInDays) = %d, want nil", *got)
	}
	if buf.Len() != 0 {
		t.Fatalf("a TTL in months was logged: %s", buf.String())
	}
	if got := s.retentionForMeta(0, errors.New("system.tables: connection reset")); got != nil {
		t.Fatalf("retentionForMeta(read error) = %d, want nil", *got)
	}
	if !strings.Contains(buf.String(), "connection reset") {
		t.Fatalf("a failed retention read was not logged: %q", buf.String())
	}
}
