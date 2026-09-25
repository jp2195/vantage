package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/chtest"
)

// TestRunReadsWithTheConfiguredStaleAfter: api.yaml's stale_after reaches the
// query layer, not only the Config. The collector here never beat, so its
// peer reads stale at any threshold, and the collector_stale warning names
// the threshold the answer was read with: 5m0s as configured, never the
// 1m30s default a run that dropped the setting would read with.
func TestRunReadsWithTheConfiguredStaleAfter(t *testing.T) {
	ctx := t.Context()
	conn := chtest.Require(t, ctx, apiCmdTestDB)
	if err := conn.Exec(ctx, "INSERT INTO "+apiCmdTestDB+".peer_events (collector_id, router_ip, peer_ip, rib, "+
		"session_id, seq, ts_router, ts_collector, stream_seq, kind) VALUES (?, toIPv6('10.253.9.1'), "+
		"toIPv6('10.253.9.2'), 'in_pre', 1, 1, now64(9), now64(9), 1, 'up')",
		chtest.LivenessPrefix+"api-cmd-silent"); err != nil {
		t.Fatal(err)
	}
	apiAddr := freeAddr(t)
	path := writeTempYAML(t, "listen: "+apiAddr+"\nmetrics_listen: "+freeAddr(t)+"\n"+
		"clickhouse_dsn: \""+devDSN()+"\"\nstale_after: 5m\n"+
		"tokens:\n  - name: test\n    token: not-a-real-secret\n")

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(runCtx, path, slog.New(slog.DiscardHandler)) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("run did not return after cancel")
		}
	}()

	var body string
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+apiAddr+"/v1/peers?router=10.253.9.1", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer not-a-real-secret")
		if resp, err := client.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				body = string(b)
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the API never answered /v1/peers")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, `"collector_stale"`) || !strings.Contains(body, "over 5m0s") {
		t.Errorf("/v1/peers over a silent collector with stale_after 5m: %s, want a collector_stale warning naming 5m0s", body)
	}
	if strings.Contains(body, "1m30s") {
		t.Errorf("/v1/peers read with the default threshold, not stale_after: %s", body)
	}
}
