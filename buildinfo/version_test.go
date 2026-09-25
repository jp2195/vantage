package buildinfo

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestEveryBinaryPrintsItsVersion builds the three daemons, the CLI and bmp-replay the
// way the Makefile and the Dockerfiles do -- -X on both vars -- and runs
// each with -version. It is the test that notices a binary losing its
// -version flag, or the -X path going stale after a package move: a wrong
// -X target is silently ignored by the linker.
func TestEveryBinaryPrintsItsVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds five binaries")
	}
	dir := t.TempDir()
	ldflags := "-X github.com/jp2195/vantage/buildinfo.Version=v0.0.0-test " +
		"-X github.com/jp2195/vantage/buildinfo.Revision=testrev"
	for _, bin := range []string{"vantage-collector", "vantage-writer", "vantage-api", "vantage", "bmp-replay"} {
		t.Run(bin, func(t *testing.T) {
			out := filepath.Join(dir, bin)
			build := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, "../cmd/"+bin)
			if b, err := build.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, b)
			}
			// A binary that has lost -version must fail fast here, not start:
			// every daemon's defaults point at loopback NATS and ClickHouse,
			// where a developer's dev stack -- and its vantage database --
			// is likely listening. -config names a file that does not exist,
			// so a daemon ignoring -version exits on the config load, and
			// the timeout covers anything that still does not return.
			args := []string{"-version"}
			if strings.HasPrefix(bin, "vantage-") {
				args = append(args, "-config", filepath.Join(dir, "does-not-exist.yaml"))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			b, err := exec.CommandContext(ctx, out, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s -version: %v\n%s", bin, err, b)
			}
			want := bin + " v0.0.0-test (testrev, " + runtime.Version() + ")"
			if got := strings.TrimSpace(string(b)); got != want {
				t.Errorf("%s -version = %q, want %q", bin, got, want)
			}
		})
	}
}
