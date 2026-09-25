package api

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAPIBinaryDoesNotLinkTheCollectorPackage pins the reason status/ exists.
//
// promauto registers on the DEFAULT registry at package init, so any binary
// that links collector/ exports that package's metrics -- at zero, forever,
// looking exactly like a healthy collector. This is not hypothetical: it is
// already true of writer:9472 (vantage_collector_sessions_active 0) and
// api:9474 (vantage_sink_rows_inserted_total 0), measured on 2026-09-18. If
// vantage-api ever imports collector/ to reach a struct, its own /metrics
// starts claiming it holds BMP sessions, and any discovery that keys on the
// presence of a metric mints a phantom collector.
func TestAPIBinaryDoesNotLinkTheCollectorPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/jp2195/vantage/cmd/vantage-api").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if dep == "github.com/jp2195/vantage/collector" {
			t.Fatal("vantage-api links collector/, which registers that package's " +
				"promauto metrics into the API binary; decode status.Report instead")
		}
	}
}
