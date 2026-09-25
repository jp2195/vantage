package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// churnBody is the shape /v1/collection/churn answers with, read back the
// way a client reads it rather than through the Go types -- the contract is
// the JSON.
type churnBody struct {
	Data []struct {
		TS          string `json:"ts"`
		Readvertise uint64 `json:"readvertise"`
		Withdraw    uint64 `json:"withdraw"`
		Dump        uint64 `json:"dump"`
	} `json:"data"`
	Meta struct {
		Warnings []struct {
			Code string `json:"code"`
		} `json:"warnings"`
		ChurnBucket string `json:"churn_bucket"`
		ChurnFrom   string `json:"churn_from"`
		ChurnTo     string `json:"churn_to"`
	} `json:"meta"`
}

// seedChurnPeersJoin writes two peers whose sessions each carry the SAME
// prefix twice, so each has a real change rather than only a dump.
//
// insertCollRoute opens a fresh session per row on purpose, which makes
// every row its session's first observation and therefore always a dump --
// a population with no changes at all, and the sparkline draws changes. Two
// rows under ONE session is the smallest fixture that produces one.
//
// TWO peers, because the join this test is about is a map lookup: with one
// row there is nothing to attach to the wrong thing.
func seedChurnPeersJoin(t *testing.T) {
	t.Helper()
	churnJoinOnce.Do(func() {
		for i, peer := range []string{churnJoinPeerA, churnJoinPeerB} {
			session := uint64(77_000 + i)
			for seq := uint64(1); seq <= 2+uint64(i); seq++ {
				insertChurnJoinRoute(t, peer, session, seq)
			}
		}
	})
}

var churnJoinOnce sync.Once

const (
	churnJoinRouter = "10.96.40.1"
	churnJoinPeerA  = "10.96.40.2"
	churnJoinPeerB  = "10.96.40.3"
)

// One route_unicast row under a NAMED session, so several rows can share
// one and the classification has a redump to find. Peer B gets one more
// observation than peer A, so the two rows carry different change counts
// and a series attached to the wrong row fails the sum rather than
// coincidentally matching.
func insertChurnJoinRoute(t *testing.T, peer string, session, seq uint64) {
	t.Helper()
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiTestDB)
	ts := time.Now().UTC().Add(-time.Duration(30-seq) * time.Minute)
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+apiTestDB+".route_unicast")
	if err != nil {
		t.Fatalf("prepare route_unicast: %v", err)
	}
	if err := batch.Append(
		fixCollector, netip.MustParseAddr(churnJoinRouter), "churn-join-r1",
		netip.MustParseAddr(peer), "in_pre", uint32(65001),
		// Column order is session_id, seq, ts_router, ts_collector,
		// parse_flags, stream_seq -- the session leads. Writing it into
		// stream_seq instead left every row opening its own session, which
		// made all of them dumps and gave this test no bars to check.
		netip.MustParseAddr(churnJoinRouter), session, seq,
		ts, ts, []string{}, seq,
		"ipv4u", "10.96.41.0/24", uint32(0), uint8(0), uint8(0),
		[]uint32{65001}, "", nil, nil,
		[]uint32{}, []string{}, []string{}, []string{},
	); err != nil {
		t.Fatalf("append route_unicast: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send route_unicast: %v", err)
	}
}

// The peers path's own shape, decoded rather than shared with churnBody:
// this answer ranks rather than draws, and each row carries its own series.
type churnPeersBody struct {
	Data []struct {
		RouterIP    string `json:"router_ip"`
		PeerIP      string `json:"peer_ip"`
		Readvertise uint64 `json:"readvertise"`
		Withdraw    uint64 `json:"withdraw"`
		Activity    []struct {
			Bucket  string `json:"bucket"`
			Changes uint64 `json:"changes"`
		} `json:"activity"`
	} `json:"data"`
	Meta struct {
		ChurnBucket string `json:"churn_bucket"`
		ChurnFrom   string `json:"churn_from"`
		ChurnTo     string `json:"churn_to"`
	} `json:"meta"`
}

func getChurnPeers(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, churnPeersBody) {
	t.Helper()
	rec := get(t, s, "/v1/collection/churn/peers"+query)
	var body churnPeersBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v -- body %s", err, rec.Body.String())
		}
	}
	return rec, body
}

func getChurn(t *testing.T, s *Server, query string) (*httptest.ResponseRecorder, churnBody) {
	t.Helper()
	rec := get(t, s, "/v1/collection/churn"+query)
	var body churnBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v -- body %s", err, rec.Body.String())
		}
	}
	return rec, body
}

// The bucket an answer was computed at is part of the answer. A chart
// labeled "5 minutes" drawn from 1-minute bars is the same class of defect
// as an unlabeled window: the picture is right and the claim beside it is
// not.
func TestChurnStatesTheBucketItUsed(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	rec, body := getChurn(t, s, "?since=1h&bucket=5m")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if body.Meta.ChurnBucket != "5m0s" {
		t.Errorf("meta.churn_bucket = %q, want the bucket the answer used", body.Meta.ChurnBucket)
	}
}

// The window this answer covers, on the DAEMON's clock, stated the same way
// the bucket width already is.
//
// Without it a client has only its own clock to place the bars against, and
// that is exactly what MonitorView did: it derived the chart's axis from
// `new Date()` while every bar carried a ts_collector. A browser minutes off
// shifted every bar against its own gridline, and nothing on screen could
// have shown it. The bucket width was already sent for the same reason --
// a picture is only as good as the claim beside it.
func TestChurnStatesTheWindowItCovered(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	before := time.Now().UTC()
	rec, body := getChurn(t, s, "?since=1h&bucket=5m")
	after := time.Now().UTC()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	from, err := time.Parse(time.RFC3339Nano, body.Meta.ChurnFrom)
	if err != nil {
		t.Fatalf("meta.churn_from = %q: %v", body.Meta.ChurnFrom, err)
	}
	to, err := time.Parse(time.RFC3339Nano, body.Meta.ChurnTo)
	if err != nil {
		t.Fatalf("meta.churn_to = %q: %v", body.Meta.ChurnTo, err)
	}
	// The end is when the daemon answered, inside the span this test brackets.
	if to.Before(before.Add(-time.Second)) || to.After(after.Add(time.Second)) {
		t.Errorf("meta.churn_to = %v, want the daemon's own now (between %v and %v)", to, before, after)
	}
	// And the start is one hour before it, because since=1h was asked for.
	// Not merely "before the end" -- that would pass for any window at all,
	// which is the whole thing this field exists to pin down.
	span := to.Sub(from)
	if span < 59*time.Minute || span > 61*time.Minute {
		t.Errorf("churn_to - churn_from = %v, want ~1h: the window is what since= asked for", span)
	}
}

func TestChurnDefaultsToABucketRatherThanRefusing(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	rec, body := getChurn(t, s, "?since=1h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if body.Meta.ChurnBucket == "" {
		t.Error("an omitted bucket= produced no churn_bucket; the default has to be stated, " +
			"or a caller cannot know what the bars mean")
	}
}

// The peers path states its window and its bucket, the same way the chart
// path does -- the contract now promises all three on both, and only one of
// them had a test.
func TestChurnPeersStatesItsWindowAndBucket(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	rec, body := getChurnPeers(t, s, "?since=1h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if body.Meta.ChurnBucket == "" {
		t.Error("no churn_bucket: a sparkline labeled with a width it was not drawn at " +
			"is the defect that field exists to prevent")
	}
	from, err := time.Parse(time.RFC3339Nano, body.Meta.ChurnFrom)
	if err != nil {
		t.Fatalf("meta.churn_from = %q: %v", body.Meta.ChurnFrom, err)
	}
	to, err := time.Parse(time.RFC3339Nano, body.Meta.ChurnTo)
	if err != nil {
		t.Fatalf("meta.churn_to = %q: %v", body.Meta.ChurnTo, err)
	}
	if span := to.Sub(from); span < 59*time.Minute || span > 61*time.Minute {
		t.Errorf("churn_to - churn_from = %v, want ~1h: the bars are placed in this window", span)
	}
}

// Each row's bars belong to THAT row.
//
// The handler joins the series onto the ranking through a map keyed on
// (router, peer) strings, and a key built from the wrong field -- or an
// address unmapped on one side and not the other -- would silently hang one
// peer's sparkline on another's row while every other test still passed.
// openapi_test.go only calls NewWirePeerChurn directly with hand-built
// activity, so nothing exercised the join itself.
func TestChurnPeersAttachesEachSeriesToItsOwnRow(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	seedChurnPeersJoin(t)
	rec, body := getChurnPeers(t, s, "?since=24h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if len(body.Data) == 0 {
		t.Fatal("no churn in the window, so this join has nothing to get wrong -- " +
			"the seeder above is what makes this test non-vacuous")
	}
	var withBars int
	for _, row := range body.Data {
		if row.Activity == nil {
			t.Errorf("peer %s has null activity; empty is the answer for a quiet peer", row.PeerIP)
		}
		var sum uint64
		for _, a := range row.Activity {
			sum += a.Changes
		}
		if len(row.Activity) > 0 {
			withBars++
		}
		// The identity the query layer pins, across the join this time: a
		// row's own bars sum to its own changes. A misattached series breaks
		// it unless two peers happen to have churned identically.
		if want := row.Readvertise + row.Withdraw; sum != want {
			t.Errorf("peer %s: bars sum to %d, its own changes are %d -- the series "+
				"attached to this row is not this row's", row.PeerIP, sum, want)
		}
	}
	if withBars == 0 {
		t.Error("no row carried a bar, so the sums above proved nothing -- " +
			"seedChurnPeersJoin is what makes this test non-vacuous")
	}
}

// The guard the query layer owns, surfaced as a 400 rather than a timeout.
func TestChurnRefusesMoreBarsThanItWillDraw(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	// Inside the window clamp and still undrawable: 24h of one-second bars is
	// 86,400 of them. A wider window would be refused by the clamp first,
	// which is a different refusal about a different parameter -- and was
	// what this test actually exercised until the message named since=.
	rec, _ := getChurn(t, s, "?since=24h&bucket=1s")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d for 24h of 1s buckets, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bucket") {
		t.Errorf("the refusal does not name the parameter to change: %s", rec.Body.String())
	}
}

func TestChurnRefusesABucketItCannotParse(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	// Escaped, because one of these has a space in it and httptest.NewRequest
	// panics on a target it cannot parse -- which is a test-rig failure that
	// looks nothing like the 400 under test.
	for _, bad := range []string{"five minutes", "-5m", "0"} {
		rec, _ := getChurn(t, s, "?since=1h&bucket="+url.QueryEscape(bad))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bucket=%q: status %d, want 400", bad, rec.Code)
		}
	}
}

// The same clamp every other /v1/collection/* path uses, for the same
// reason: unscoped, this reads three route tables across the window.
func TestChurnClampsTheUnscopedWindow(t *testing.T) {
	s := requireAPIWithMaxUnscopedSince(t, 24*time.Hour)
	rec, _ := getChurn(t, s, "?since=2160h&bucket=1h")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d for a 90-day unscoped window, want 400; body %s", rec.Code, rec.Body.String())
	}
}
