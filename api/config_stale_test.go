package api

import (
	"strings"
	"testing"
	"time"

	"github.com/jp2195/vantage/query"
)

// TestStaleAfterDefaultsToTheQueryDefault: an api.yaml that says nothing reads
// with the same threshold `vantage query -dsn` does, so the two read paths
// agree unless an operator changes one of them.
func TestStaleAfterDefaultsToTheQueryDefault(t *testing.T) {
	cfg := writeAndLoad(t, "tokens:\n  - {name: t, token: 0123456789abcdef}\n")
	if cfg.StaleAfter != query.DefaultStaleAfter {
		t.Fatalf("StaleAfter = %v, want %v", cfg.StaleAfter, query.DefaultStaleAfter)
	}
	cfg = writeAndLoad(t, "stale_after: 5m\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	if cfg.StaleAfter != 5*time.Minute {
		t.Fatalf("StaleAfter = %v, want 5m", cfg.StaleAfter)
	}
}

// TestStaleAfterBelowTwoBeatsIsRefused at startup, where the operator who
// wrote it will see it, rather than as every peer reading stale between
// heartbeats. The floor itself, two beats exactly, loads.
func TestStaleAfterBelowTwoBeatsIsRefused(t *testing.T) {
	if cfg := writeAndLoad(t, "stale_after: 60s\ntokens:\n  - {name: t, token: 0123456789abcdef}\n"); cfg.StaleAfter != query.MinStaleAfter {
		t.Fatalf("StaleAfter = %v, want %v", cfg.StaleAfter, query.MinStaleAfter)
	}
	path := writeTempYAML(t, "stale_after: 59s\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "stale_after") {
		t.Fatalf("LoadConfig with stale_after 59s = %v, want an error naming stale_after", err)
	}
}

// TestStaleAfterAboveADayIsRefused: past about 56 years, a threshold would
// read a collector never heard from as up (see query.MaxStaleAfter). The
// ceiling itself loads; one past it, and an absurd one, do not.
func TestStaleAfterAboveADayIsRefused(t *testing.T) {
	cfg := writeAndLoad(t, "stale_after: 24h\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
	if cfg.StaleAfter != query.MaxStaleAfter {
		t.Fatalf("StaleAfter = %v, want %v", cfg.StaleAfter, query.MaxStaleAfter)
	}
	for _, v := range []string{"24h0m1s", "876000h"} {
		_, err := loadFrom(t, "stale_after: "+v+"\ntokens:\n  - {name: t, token: 0123456789abcdef}\n")
		if err == nil || !strings.Contains(err.Error(), "stale_after") {
			t.Errorf("LoadConfig with stale_after %s = %v, want an error naming stale_after", v, err)
		}
	}
}
